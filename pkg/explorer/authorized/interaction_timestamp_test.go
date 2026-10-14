/*
 * Licensed to the Apache Software Foundation (ASF) under one
 * or more contributor license agreements. See the NOTICE file
 * distributed with this work for additional information
 * regarding copyright ownership. The ASF licenses this file
 * to you under the Apache License, Version 2.0 (the
 * "License"); you may not use this file except in compliance
 * with the License. You may obtain a copy of the License at
 *
 *     https://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing,
 * software distributed under the License is distributed on an
 * "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY
 * KIND, either express or implied. See the License for the
 * specific language governing permissions and limitations
 * under the License.
 */

package authorized_test

import (
	"context"
	"reflect"
	"testing"
	"time"

	"github.com/phrocker/shoal-oss/pkg/explorer"
	"github.com/phrocker/shoal-oss/pkg/explorer/auth"
	"github.com/phrocker/shoal-oss/pkg/interaction"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

type timestampResultBase struct {
	*explorer.Explorer
	changeTime  bool
	hideFirst   bool
	deleted     bool
	divergeRead bool
	readFailure error
	reads       int
}

func (b *timestampResultBase) RecordInteractionResult(
	ctx context.Context, session interaction.Session,
) (interaction.Session, error) {
	recorded, err := b.Explorer.RecordInteractionResult(ctx, session)
	if err == nil && b.changeTime {
		recorded.RecordedAt = recorded.RecordedAt.Add(time.Second)
	}
	return recorded, err
}

func (b *timestampResultBase) InteractionRecord(
	ctx context.Context, id shoal.ID,
) (explorer.InteractionRecord, error) {
	b.reads++
	if b.hideFirst && b.reads == 1 {
		return explorer.InteractionRecord{}, shoal.NewError(
			shoal.ErrorNotFound, "simulated concurrent winner")
	}
	if b.reads > 1 && b.readFailure != nil {
		return explorer.InteractionRecord{}, b.readFailure
	}
	record, err := b.Explorer.InteractionRecord(ctx, id)
	if err == nil && b.reads > 1 {
		record.Summary.Deleted = b.deleted
		if b.divergeRead {
			record.Session.RecordedAt = record.Session.RecordedAt.Add(time.Second)
			record.Session.Actor.SubjectID = "different-durable-actor"
		}
	}
	return record, err
}

func TestAuthorizedResultSinkVerifiesChangedTimestamp(t *testing.T) {
	for _, mode := range []string{
		"unchanged", "forged", "read-failure", "deleted", "different-record",
	} {
		t.Run(mode, func(t *testing.T) {
			f := newFixture(t)
			snapshot, err := f.base.Snapshot(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			f.clock.Set(snapshot.AsOf.Add(time.Second))
			decision := f.decision(t, "timestamp-proof",
				[][]byte{f.sourceA}, [][]byte{f.policyA},
				[]auth.Operation{auth.OperationRetrieve})
			fingerprint, err := auth.AuthorizationFingerprint(decision)
			if err != nil {
				t.Fatal(err)
			}
			base := &timestampResultBase{
				Explorer: f.base, changeTime: mode != "unchanged",
				deleted: mode == "deleted", divergeRead: mode == "different-record",
			}
			if mode == "read-failure" {
				base.readFailure = shoal.NewError(shoal.ErrorUnavailable, "read failed")
			}
			client := f.newClient(t, base, f.store, f.sourceA, f.policyA, nil)
			session := interaction.Session{
				ID:                       interaction.DerivedID("session", mode),
				Operation:                interaction.OperationRetrieval,
				SnapshotID:               shoal.ID(snapshot.ID),
				SnapshotAsOf:             snapshot.AsOf,
				AuthorizationFingerprint: shoal.ID(fingerprint.String()),
				AuthorizationExpiresAt:   decision.AuthenticationExpires(),
			}
			_, err = client.RecordInteractionResult(f.context(t, decision), session)
			if mode == "unchanged" {
				if err != nil || base.reads != 1 {
					t.Fatalf("unchanged receipt: reads=%d, err=%v", base.reads, err)
				}
			} else if !explorer.IsCommittedInteraction(err) || base.reads != 2 {
				t.Fatalf("unverified receipt: reads=%d, err=%v", base.reads, err)
			}
			if mode == "read-failure" && (!explorer.IsIndeterminateCommit(err) ||
				!shoal.IsErrorCode(err, shoal.ErrorUnavailable)) {
				t.Fatalf("lost indeterminate read failure: %v", err)
			}
			stored, err := f.base.InteractionRecord(context.Background(), session.ID)
			if err != nil {
				t.Fatal(err)
			}
			if !stored.Session.RecordedAt.Equal(f.clock.Now()) {
				t.Fatalf("durable timestamp changed: %v", stored.Session.RecordedAt)
			}
		})
	}
}

func TestAuthorizedResultSinkVerifiesNarrowRoleRetryWinner(t *testing.T) {
	f := newFixture(t)
	snapshot, err := f.base.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	firstTime := snapshot.AsOf.Add(time.Second)
	f.clock.Set(firstTime)
	decision := f.decision(t, "invoke-timestamp-proof",
		[][]byte{f.sourceA}, [][]byte{f.policyA},
		[]auth.Operation{auth.OperationInvoke})
	fingerprint, err := auth.AuthorizationFingerprint(decision)
	if err != nil {
		t.Fatal(err)
	}
	session := interaction.Session{
		ID:                       interaction.DerivedID("session", "invoke-timestamp"),
		Operation:                interaction.OperationToolCall,
		AuthorizationOperation:   string(auth.OperationInvoke),
		SnapshotID:               shoal.ID(snapshot.ID),
		SnapshotAsOf:             snapshot.AsOf,
		AuthorizationFingerprint: shoal.ID(fingerprint.String()),
		AuthorizationExpiresAt:   decision.AuthenticationExpires(),
	}
	ctx := f.context(t, decision)
	first, err := f.clientA.RecordInteractionResult(ctx, session)
	if err != nil {
		t.Fatal(err)
	}
	f.clock.Set(firstTime.Add(time.Minute))
	base := &timestampResultBase{Explorer: f.base, hideFirst: true}
	client := f.newClient(t, base, f.store, f.sourceA, f.policyA, nil)
	retried, err := client.RecordInteractionResult(ctx, session)
	if err != nil || !reflect.DeepEqual(first, retried) || base.reads != 2 {
		t.Fatalf("narrow-role retry: reads=%d, err=%v, result=%+v",
			base.reads, err, retried)
	}
}
