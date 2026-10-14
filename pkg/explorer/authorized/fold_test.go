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
	"github.com/phrocker/shoal-oss/pkg/explorer/authorized"
	"github.com/phrocker/shoal-oss/pkg/interaction"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

type postFoldHookStore struct {
	*explorer.Explorer
	hook          func()
	rehydrateHook func()
	result        explorer.FoldResult
}

func (s *postFoldHookStore) FoldInteractions(
	ctx context.Context,
	request explorer.FoldRequest,
) (explorer.FoldResult, error) {
	result, err := s.Explorer.FoldInteractions(ctx, request)
	if err == nil {
		s.result = result
		if s.hook != nil {
			s.hook()
		}
	}
	return result, err
}

func (s *postFoldHookStore) RehydrateFold(
	ctx context.Context, foldID shoal.ID,
) (interaction.Fold, error) {
	fold, err := s.Explorer.RehydrateFold(ctx, foldID)
	if err == nil && s.rehydrateHook != nil {
		s.rehydrateHook()
	}
	return fold, err
}

func TestAuthorizedFoldWithholdsCommittedResultAfterMemberReclassification(
	t *testing.T,
) {
	for _, adopted := range []bool{false, true} {
		name := "created"
		if adopted {
			name = "adopted"
		}
		t.Run(name, func(t *testing.T) {
			f := newFixture(t)
			const uri = "file:///fold-race.txt"
			receipt, err := f.clientA.Ingest(f.admin(t), explorer.Source{
				URI: uri, MediaType: explorer.MediaTypeText,
				Content: "fold authorization must remain current",
			})
			if err != nil {
				t.Fatal(err)
			}
			view, err := f.clientA.Document(
				f.admin(t), receipt.Document.ID, receipt.Revision.ID)
			if err != nil {
				t.Fatal(err)
			}
			snapshot, err := f.base.Snapshot(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			f.clock.Set(snapshot.AsOf.Add(time.Second))
			decision := f.decision(
				t, "fold-reader",
				[][]byte{f.sourceA}, [][]byte{f.policyA}, allOperations,
			)
			ctx := f.context(t, decision)
			fingerprint, err := auth.AuthorizationFingerprint(decision)
			if err != nil {
				t.Fatal(err)
			}
			session := interaction.Session{
				ID: interaction.DerivedID(
					"session", "fold-race"),
				RecordedAt:               f.clock.Now(),
				SnapshotID:               shoal.ID(snapshot.ID),
				SnapshotAsOf:             snapshot.AsOf,
				AuthorizationFingerprint: shoal.ID(fingerprint.String()),
				AuthorizationExpiresAt:   decision.AuthenticationExpires(),
				SeedNodeIDs:              []shoal.ID{firstSpanID(t, view)},
			}
			if err := f.clientA.RecordInteraction(ctx, session); err != nil {
				t.Fatal(err)
			}
			request := explorer.FoldRequest{
				SessionIDs: []shoal.ID{session.ID},
			}
			var accepted explorer.FoldResult
			if adopted {
				accepted, err = f.clientA.FoldInteractions(ctx, request)
				if err != nil {
					t.Fatal(err)
				}
			}

			var hookErr error
			store := &postFoldHookStore{Explorer: f.base}
			store.hook = func() {
				_, hookErr = f.clientB.Ingest(f.admin(t), explorer.Source{
					URI: uri, MediaType: explorer.MediaTypeText,
					Content: "reclassified after durable fold commit",
				})
			}
			client := f.newClient(
				t, store, f.store, f.sourceA, f.policyA, nil)
			result, err := client.FoldInteractions(ctx, request)
			if hookErr != nil {
				t.Fatalf("reclassify fold member: %v", hookErr)
			}
			if !explorer.IsCommittedInteraction(err) {
				t.Fatalf("post-commit authorization error = %v", err)
			}
			if !reflect.DeepEqual(result, explorer.FoldResult{}) {
				t.Fatalf("post-commit failure leaked fold result: %+v", result)
			}
			if store.result.FoldID == "" ||
				store.result.Created == adopted {
				t.Fatalf("durable fold result = %+v, adopted=%v",
					store.result, adopted)
			}
			if adopted &&
				(store.result.FoldID != accepted.FoldID ||
					!store.result.FoldedAt.Equal(accepted.FoldedAt) ||
					store.result.Visibility != accepted.Visibility ||
					store.result.MemberCount != accepted.MemberCount ||
					store.result.RetrievedCount != accepted.RetrievedCount ||
					store.result.CitedCount != accepted.CitedCount) {
				t.Fatalf("retry changed durable fold receipt: first=%+v retry=%+v",
					accepted, store.result)
			}
			folds, err := client.Folds(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if len(folds) != 0 {
				t.Fatalf("reclassified fold remained visible: %+v", folds)
			}
			if _, err := client.RehydrateFold(
				ctx, store.result.FoldID,
			); !shoal.IsErrorCode(err, shoal.ErrorNotFound) {
				t.Fatalf("reclassified fold rehydration = %v, want not found", err)
			}
		})
	}
}

func TestAuthorizedFoldRequiresMutationOperation(t *testing.T) {
	f := newFixture(t)
	receipt, err := f.clientA.Ingest(f.admin(t), explorer.Source{
		URI:       "file:///fold-mutation-operation.txt",
		MediaType: explorer.MediaTypeText,
		Content:   "read access does not grant fold mutation",
	})
	if err != nil {
		t.Fatal(err)
	}
	view, err := f.clientA.Document(
		f.admin(t), receipt.Document.ID, receipt.Revision.ID)
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := f.base.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	f.clock.Set(snapshot.AsOf.Add(time.Second))
	writerDecision := f.decision(
		t, "fold-writer",
		[][]byte{f.sourceA}, [][]byte{f.policyA}, allOperations)
	writerCtx := f.context(t, writerDecision)
	fingerprint, err := auth.AuthorizationFingerprint(writerDecision)
	if err != nil {
		t.Fatal(err)
	}
	session := interaction.Session{
		ID:                       interaction.DerivedID("session", "fold-mutation-operation"),
		RecordedAt:               f.clock.Now(),
		SnapshotID:               shoal.ID(snapshot.ID),
		SnapshotAsOf:             snapshot.AsOf,
		AuthorizationFingerprint: shoal.ID(fingerprint.String()),
		AuthorizationExpiresAt:   writerDecision.AuthenticationExpires(),
		SeedNodeIDs:              []shoal.ID{firstSpanID(t, view)},
	}
	if err := f.clientA.RecordInteraction(writerCtx, session); err != nil {
		t.Fatal(err)
	}
	readerDecision := f.decision(
		t, "fold-reader",
		[][]byte{f.sourceA}, [][]byte{f.policyA},
		[]auth.Operation{auth.OperationRead})
	if _, err := f.clientA.FoldInteractions(
		f.context(t, readerDecision),
		explorer.FoldRequest{SessionIDs: []shoal.ID{session.ID}},
	); !shoal.IsErrorCode(err, shoal.ErrorUnauthorized) {
		t.Fatalf("read-only fold mutation error = %v", err)
	}
	folds, err := f.base.Folds(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(folds) != 0 {
		t.Fatalf("read-only principal created folds: %+v", folds)
	}
}

func TestAuthorizedFoldMemberReadFailureIsNotConcealed(t *testing.T) {
	f := newFixture(t)
	snapshot, err := f.base.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	f.clock.Set(snapshot.AsOf.Add(time.Second))
	decision := f.decision(
		t, "fold-read-failure",
		nil, nil, allOperations,
	)
	ctx := f.context(t, decision)
	fingerprint, err := auth.AuthorizationFingerprint(decision)
	if err != nil {
		t.Fatal(err)
	}
	session := interaction.Session{
		ID:                       interaction.DerivedID("session", "fold-read-failure"),
		RecordedAt:               f.clock.Now(),
		SnapshotID:               shoal.ID(snapshot.ID),
		SnapshotAsOf:             snapshot.AsOf,
		AuthorizationFingerprint: shoal.ID(fingerprint.String()),
		AuthorizationExpiresAt:   decision.AuthenticationExpires(),
	}
	if err := f.clientA.RecordInteraction(ctx, session); err != nil {
		t.Fatal(err)
	}
	created, err := f.clientA.FoldInteractions(ctx, explorer.FoldRequest{
		SessionIDs: []shoal.ID{session.ID},
	})
	if err != nil {
		t.Fatal(err)
	}

	for _, test := range []struct {
		name string
		call func(context.Context, *authorized.Client) error
	}{
		{
			name: "list",
			call: func(ctx context.Context, client *authorized.Client) error {
				_, err := client.Folds(ctx)
				return err
			},
		},
		{
			name: "rehydrate",
			call: func(ctx context.Context, client *authorized.Client) error {
				_, err := client.RehydrateFold(ctx, created.FoldID)
				return err
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			cancelCtx, cancel := context.WithCancel(ctx)
			store := &postFoldHookStore{Explorer: f.base}
			store.rehydrateHook = cancel
			client := f.newClient(
				t, store, f.store, f.sourceA, f.policyA, nil)
			err := test.call(cancelCtx, client)
			if !shoal.IsErrorCode(err, shoal.ErrorCanceled) {
				t.Fatalf("member read failure = %v, want canceled", err)
			}
		})
	}
}
