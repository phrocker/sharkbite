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

package interaction_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/phrocker/shoal-oss/pkg/interaction"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

type recorderSink struct {
	ensureErr error
	recordErr error
	ensured   int
	recorded  []interaction.Session
	result    interaction.Session
}

func (s *recorderSink) EnsureInteractionSink(context.Context) error {
	s.ensured++
	return s.ensureErr
}

func (s *recorderSink) RecordInteraction(
	_ context.Context, session interaction.Session,
) error {
	s.recorded = append(s.recorded, session)
	return s.recordErr
}

func (s *recorderSink) RecordInteractionResult(
	ctx context.Context, session interaction.Session,
) (interaction.Session, error) {
	if err := s.RecordInteraction(ctx, session); err != nil {
		return interaction.Session{}, err
	}
	if s.result.ID != "" {
		return s.result, nil
	}
	return session, nil
}

func TestProductRecorderIsFailClosedAndCanonical(t *testing.T) {
	ctx := context.Background()
	fixed := time.Date(2026, time.September, 5, 22, 0, 0, 123, time.UTC)
	sink := &recorderSink{}
	recorder, err := interaction.NewRecorder(ctx, sink)
	if err != nil {
		t.Fatal(err)
	}
	if sink.ensured != 1 {
		t.Fatalf("sink checks = %d", sink.ensured)
	}
	if err := recorder.SetClock(func() time.Time { return fixed }); err != nil {
		t.Fatal(err)
	}
	id, err := interaction.OperationSessionID(
		interaction.OperationRetrieval, "request-1", fixed)
	if err != nil {
		t.Fatal(err)
	}
	recorded, err := recorder.Record(ctx, interaction.Session{
		ID:         id,
		RecordedAt: fixed.Add(24 * time.Hour),
		Operation:  interaction.OperationRetrieval,
		SeedNodeIDs: []shoal.ID{
			"span-b", "span-a", "span-b",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !recorded.RecordedAt.Equal(fixed) ||
		len(recorded.SeedNodeIDs) != 2 ||
		recorded.SeedNodeIDs[0] != "span-a" {
		t.Fatalf("canonical recorded session = %+v", recorded)
	}

	sink.result = recorded
	enrichedID := interaction.DerivedID("session", "enriched")
	sink.result.ID = enrichedID
	sink.result.RecordedAt = fixed
	sink.result.Operation = interaction.OperationToolCall
	sink.result.SeedNodeIDs = nil
	sink.result.Actor = interaction.ActorContext{
		SubjectID: "trusted-subject",
		ActorID:   "trusted-actor",
	}
	returned, err := recorder.Record(ctx, interaction.Session{
		ID:         enrichedID,
		RecordedAt: fixed.Add(time.Second),
		Operation:  interaction.OperationToolCall,
		Actor: interaction.ActorContext{
			SubjectID: "forged-subject",
			ActorID:   "forged-actor",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if returned.Actor.SubjectID != "trusted-subject" ||
		returned.Actor.ActorID != "trusted-actor" {
		t.Fatalf("recorder returned caller metadata: %+v", returned.Actor)
	}
	sink.result = interaction.Session{}

	sink.recordErr = errors.New("durable sink unavailable")
	if _, err := recorder.Record(ctx, interaction.Session{
		ID:        interaction.DerivedID("session", "failing"),
		Operation: interaction.OperationToolCall,
	}); !errors.Is(err, sink.recordErr) {
		t.Fatalf("record error = %v", err)
	}

	unavailable := &recorderSink{ensureErr: errors.New("read-only")}
	if _, err := interaction.NewRecorder(ctx, unavailable); !errors.Is(
		err, unavailable.ensureErr,
	) {
		t.Fatalf("setup error = %v", err)
	}

	var typedNil *recorderSink
	if _, err := interaction.NewRecorder(
		ctx, typedNil,
	); !shoal.IsErrorCode(err, shoal.ErrorInvalidArgument) {
		t.Fatalf("typed-nil sink error = %v", err)
	}
}

func TestProductRecorderRejectsChangedWorkAsCommitted(t *testing.T) {
	now := time.Date(2026, time.September, 6, 10, 0, 0, 0, time.UTC)
	for name, mutate := range map[string]func(*interaction.Session){
		"result":    func(s *interaction.Session) { s.ResultID = "different-result" },
		"operation": func(s *interaction.Session) { s.AuthorizationOperation = "dispatch" },
		"evidence":  func(s *interaction.Session) { s.SeedNodeIDs = []shoal.ID{"different-node"} },
		"expiry":    func(s *interaction.Session) { s.AuthorizationExpiresAt = now.Add(2 * time.Hour) },
	} {
		t.Run(name, func(t *testing.T) {
			request := interaction.Session{
				ID: interaction.DerivedID("session", name), RecordedAt: now,
				Operation: interaction.OperationToolCall, AuthorizationOperation: "invoke",
				SnapshotID: "snapshot", SnapshotAsOf: now.Add(-time.Minute),
				AuthorizationFingerprint: "auth-sha256:test",
				AuthorizationExpiresAt:   now.Add(time.Hour),
			}
			sink := &recorderSink{result: request}
			mutate(&sink.result)
			recorder, err := interaction.NewRecorder(context.Background(), sink)
			if err != nil {
				t.Fatal(err)
			}
			if err := recorder.SetClock(func() time.Time { return now }); err != nil {
				t.Fatal(err)
			}
			if _, err := recorder.Record(context.Background(), request); !interaction.IsCommittedRecord(err) {
				t.Fatalf("changed %s must fail as committed: %v", name, err)
			}
		})
	}
}

func TestProductRecorderRejectsExpiredTrustedClockBeforeSink(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, time.September, 6, 10, 0, 0, 0, time.UTC)
	sink := &recorderSink{}
	recorder, err := interaction.NewRecorder(ctx, sink)
	if err != nil {
		t.Fatal(err)
	}
	if err := recorder.SetClock(func() time.Time { return now }); err != nil {
		t.Fatal(err)
	}
	_, err = recorder.Record(ctx, interaction.Session{
		ID:                       interaction.DerivedID("session", "expired-before-sink"),
		RecordedAt:               now.Add(-time.Hour),
		Operation:                interaction.OperationRetrieval,
		SnapshotID:               "snapshot-expired",
		SnapshotAsOf:             now.Add(-2 * time.Hour),
		AuthorizationFingerprint: "auth-sha256:expired",
		AuthorizationExpiresAt:   now,
	})
	if !shoal.IsErrorCode(err, shoal.ErrorUnauthorized) {
		t.Fatalf("expired trusted-clock error = %v", err)
	}
	if sink.recorded != nil {
		t.Fatalf("expired recording invoked sink: %+v", sink.recorded)
	}
}

func TestProductRecorderMarksPostSinkExpiryCommitted(t *testing.T) {
	ctx := context.Background()
	started := time.Date(2026, time.September, 6, 10, 0, 0, 0, time.UTC)
	expires := started.Add(time.Second)
	calls := 0
	sink := &recorderSink{}
	recorder, err := interaction.NewRecorder(ctx, sink)
	if err != nil {
		t.Fatal(err)
	}
	if err := recorder.SetClock(func() time.Time {
		calls++
		if calls == 1 {
			return started
		}
		return expires
	}); err != nil {
		t.Fatal(err)
	}
	recorded, err := recorder.Record(ctx, interaction.Session{
		ID:                       interaction.DerivedID("session", "expires-after-sink"),
		Operation:                interaction.OperationRetrieval,
		SnapshotID:               "snapshot-live",
		SnapshotAsOf:             started.Add(-time.Second),
		AuthorizationFingerprint: "auth-sha256:live",
		AuthorizationExpiresAt:   expires,
	})
	if !interaction.IsCommittedRecord(err) ||
		!shoal.IsErrorCode(err, shoal.ErrorUnauthorized) {
		t.Fatalf("post-sink expiry error = %v", err)
	}
	if recorded.ID == "" || len(sink.recorded) != 1 {
		t.Fatalf("committed result = %+v, sink writes = %d",
			recorded, len(sink.recorded))
	}
	if !recorded.RecordedAt.Equal(started) {
		t.Fatalf("accepted timestamp = %v", recorded.RecordedAt)
	}
}

func TestGenericRetrievalHasNoInferenceNode(t *testing.T) {
	session := interaction.Session{
		ID:         interaction.DerivedID("session", "retrieval"),
		RecordedAt: time.Unix(1700000000, 0).UTC(),
		Operation:  interaction.OperationRetrieval,
		Actor: interaction.ActorContext{
			SubjectID:  "subject",
			ActorID:    "agent",
			ClientID:   "client",
			OnBehalfOf: []shoal.ID{"delegate-a", "delegate-b"},
		},
		Reason:      mustReason(t, "retrieve_context", "answer user request"),
		SeedNodeIDs: []shoal.ID{"span-a"},
	}
	subgraph, err := session.Subgraph(func(shoal.ID) ([]string, error) {
		return []string{"ops"}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	kinds := make(map[string]int)
	var sessionProperties map[string]string
	for _, node := range subgraph.Nodes {
		kinds[node.Kind]++
		if node.Kind == interaction.KindSession {
			sessionProperties = node.Properties
		}
	}
	if kinds[interaction.KindSession] != 1 ||
		kinds[interaction.KindInference] != 0 {
		t.Fatalf("node kinds = %v", kinds)
	}
	if sessionProperties[interaction.PropertyOperation] !=
		string(interaction.OperationRetrieval) ||
		sessionProperties[interaction.PropertySubjectID] != "subject" ||
		sessionProperties[interaction.PropertyActorID] != "agent" ||
		sessionProperties[interaction.PropertyClientID] != "client" ||
		sessionProperties[interaction.PropertyDelegationCount] != "2" ||
		sessionProperties[interaction.PropertyDelegationID] == "" ||
		sessionProperties[interaction.PropertyReasonCode] != "retrieve_context" ||
		sessionProperties[interaction.PropertyReasonDigest] == "" {
		t.Fatalf("session properties = %+v", sessionProperties)
	}
	for _, edge := range subgraph.Edges {
		if edge.Type == interaction.EdgeHasInference {
			t.Fatal("generic retrieval materialized an inference edge")
		}
	}
}

func mustReason(t *testing.T, code, detail string) interaction.Reason {
	t.Helper()
	reason, err := interaction.NewReason(code, detail)
	if err != nil {
		t.Fatal(err)
	}
	return reason
}
