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

package fleetevents

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/phrocker/shoal-oss/internal/explorerfleetcap"
	"github.com/phrocker/shoal-oss/pkg/document"
	"github.com/phrocker/shoal-oss/pkg/explorer"
	"github.com/phrocker/shoal-oss/pkg/explorer/auth"
	"github.com/phrocker/shoal-oss/pkg/interaction"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

type testAuditSnapshots struct{}

func (testAuditSnapshots) InteractionSnapshot(
	context.Context,
) (explorer.Snapshot, error) {
	return explorer.Snapshot{
		ID:   "audit-snapshot",
		AsOf: time.Date(2026, 9, 5, 19, 0, 0, 0, time.UTC),
	}, nil
}

func TestInteractionAuditorRecordsRedactedAction(t *testing.T) {
	now := time.Date(2026, 9, 5, 20, 0, 0, 0, time.UTC)
	sink := &auditSink{}
	recorder, err := interaction.NewRecorder(context.Background(), sink)
	if err != nil {
		t.Fatal(err)
	}
	if err := recorder.SetClock(func() time.Time { return now }); err != nil {
		t.Fatal(err)
	}
	auditor, err := NewInteractionAuditor(recorder, testAuditSnapshots{})
	if err != nil {
		t.Fatal(err)
	}
	err = auditor.RecordFleetAction(context.Background(), AuditRecord{
		Operation: auth.OperationEventPublish, ActionID: []byte{0, 0xff},
		RequestID: "request",
		ObjectID:  []byte("event"),
		Evidence: []Evidence{
			{
				SourceID: []byte("source-a"), PolicyID: []byte("policy-a"),
				ObjectID: "object-a",
			},
			{SourceID: []byte("source-b"), PolicyID: []byte("policy-b"), ObjectID: "object-b"},
		},
		ConsumedEvidence: []interaction.EvidenceReference{{
			AnchorID: "anchor-a", Kind: interaction.EvidenceGraph,
			NodeIDs: []shoal.ID{"node-a"},
		}},
		CitedEvidence: []interaction.EvidenceReference{{
			AnchorID: "citation-a", Kind: interaction.EvidenceDocument,
			Citation: document.Citation{
				DocumentID: "document-a", RevisionID: "revision-a",
				SectionID: "section-a",
				Range: document.SourceRange{
					Start: document.SourcePosition{Offset: 1},
					End:   document.SourcePosition{Offset: 2},
				},
			},
			NodeIDs: []shoal.ID{"document-a", "section-a"},
		}},
		AuthorizationFingerprint: auth.Fingerprint{1},
		AuthorizationExpiresAt:   now.Add(time.Hour), OccurredAt: now,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(sink.sessions) != 1 || sink.sessions[0].Operation != interaction.OperationToolCall ||
		sink.sessions[0].AuthorizationOperation != string(auth.OperationEventPublish) ||
		sink.sessions[0].Turns[0].ToolCall.Kind != "fleet.event_publish" ||
		sink.sessions[0].RequestID != "request" ||
		sink.sessions[0].AuthorizationFingerprint != "auth-sha256:0100000000000000000000000000000000000000000000000000000000000000" ||
		sink.sessions[0].Actor.SubjectID != "" ||
		sink.sessions[0].Actor.ActorID != "" ||
		sink.sessions[0].Actor.ClientID != "" ||
		len(sink.sessions[0].Actor.OnBehalfOf) != 0 ||
		sink.sessions[0].Reason != (interaction.Reason{}) ||
		len(sink.sessions[0].SeedEvidence) != 0 ||
		len(sink.sessions[0].Turns[0].ToolCall.RetrievedEvidence) != 1 ||
		len(sink.sessions[0].CitedEvidence) != 1 ||
		len(sink.sessions[0].TouchedNodeIDs()) != 3 {
		t.Fatalf("session = %#v", sink.sessions)
	}
	if got := sink.sessions[0].Turns[0].ToolCall.RetrievedEvidence; got[0].AnchorID != "anchor-a" {
		t.Fatalf("retrieved evidence = %#v", got)
	}
	if got := sink.sessions[0].CitedEvidence; got[0].AnchorID != "citation-a" {
		t.Fatalf("cited evidence = %#v", got)
	}
	if got := sink.sessions[0].TouchedNodeIDs(); !reflect.DeepEqual(
		got, []shoal.ID{"document-a", "node-a", "section-a"}) {
		t.Fatalf("evidence = %#v", got)
	}
}

func TestInteractionAuditorRejectsMismatchedPersistedReceipt(t *testing.T) {
	sink := &auditSink{mutateResult: func(session interaction.Session) interaction.Session {
		session.AuthorizationOperation = string(auth.OperationSubscriptionCreate)
		return session
	}}
	recorder, err := interaction.NewRecorder(context.Background(), sink)
	if err != nil {
		t.Fatal(err)
	}
	auditor, err := NewInteractionAuditor(recorder, testAuditSnapshots{})
	if err != nil {
		t.Fatal(err)
	}
	err = auditor.RecordFleetAction(context.Background(), AuditRecord{
		Operation: auth.OperationEventPublish, ActionID: []byte("action"),
		ObjectID: []byte("event"), AuthorizationFingerprint: auth.Fingerprint{1},
		AuthorizationExpiresAt: time.Now().Add(time.Hour), OccurredAt: time.Now(),
	})
	if !interaction.IsCommittedRecord(err) {
		t.Fatalf("mismatched accepted receipt must fail as committed: %v", err)
	}
}

func TestInteractionAuditorRejectsNilRecorder(t *testing.T) {
	if _, err := NewInteractionAuditor(nil, testAuditSnapshots{}); err == nil {
		t.Fatal("nil recorder succeeded")
	}
}

func TestInteractionAuditorAcceptsOriginalRetryTimestamp(t *testing.T) {
	original := time.Date(2026, 9, 6, 10, 0, 0, 0, time.UTC)
	retry := original.Add(time.Minute)
	sink := &auditSink{mutateResult: func(session interaction.Session) interaction.Session {
		session.RecordedAt = original
		return session
	}}
	recorder, err := interaction.NewRecorder(context.Background(), sink)
	if err != nil {
		t.Fatal(err)
	}
	if err := recorder.SetClock(func() time.Time { return retry }); err != nil {
		t.Fatal(err)
	}
	auditor, err := NewInteractionAuditor(recorder, testAuditSnapshots{})
	if err != nil {
		t.Fatal(err)
	}
	if err := auditor.RecordFleetAction(context.Background(), AuditRecord{
		Operation: auth.OperationEventPublish, ActionID: []byte("retry"),
		ObjectID: []byte("event"), AuthorizationFingerprint: auth.Fingerprint{1},
		AuthorizationExpiresAt: retry.Add(time.Hour), OccurredAt: original,
	}); err != nil {
		t.Fatalf("original timestamp retry = %v", err)
	}
}

func TestInteractionAuditorRejectsPreSinkExpiryWithoutWrite(t *testing.T) {
	now := time.Date(2026, 9, 6, 10, 0, 0, 0, time.UTC)
	sink := &auditSink{}
	recorder, err := interaction.NewRecorder(context.Background(), sink)
	if err != nil {
		t.Fatal(err)
	}
	if err := recorder.SetClock(func() time.Time { return now }); err != nil {
		t.Fatal(err)
	}
	auditor, err := NewInteractionAuditor(recorder, testAuditSnapshots{})
	if err != nil {
		t.Fatal(err)
	}
	err = auditor.RecordFleetAction(context.Background(), AuditRecord{
		Operation: auth.OperationEventPublish, ActionID: []byte("expired"),
		ObjectID: []byte("event"), AuthorizationFingerprint: auth.Fingerprint{1},
		AuthorizationExpiresAt: now, OccurredAt: now.Add(-time.Second),
	})
	if !shoal.IsErrorCode(err, shoal.ErrorUnauthorized) ||
		interaction.IsCommittedRecord(err) || len(sink.sessions) != 0 {
		t.Fatalf("pre-sink expiry = %v, writes = %d", err, len(sink.sessions))
	}
}

func TestInteractionAuditorPreservesCommittedPostSinkFailures(t *testing.T) {
	started := time.Date(2026, 9, 6, 10, 0, 0, 0, time.UTC)
	t.Run("expiry", func(t *testing.T) {
		calls := 0
		sink := &auditSink{}
		recorder, err := interaction.NewRecorder(context.Background(), sink)
		if err != nil {
			t.Fatal(err)
		}
		if err := recorder.SetClock(func() time.Time {
			calls++
			if calls == 1 {
				return started
			}
			return started.Add(time.Second)
		}); err != nil {
			t.Fatal(err)
		}
		auditor, err := NewInteractionAuditor(recorder, testAuditSnapshots{})
		if err != nil {
			t.Fatal(err)
		}
		err = auditor.RecordFleetAction(context.Background(), AuditRecord{
			Operation: auth.OperationEventPublish, ActionID: []byte("expiry"),
			ObjectID: []byte("event"), AuthorizationFingerprint: auth.Fingerprint{1},
			AuthorizationExpiresAt: started.Add(time.Second),
			OccurredAt:             started.Add(-time.Second),
		})
		if !interaction.IsCommittedRecord(err) ||
			!shoal.IsErrorCode(err, shoal.ErrorUnauthorized) ||
			len(sink.sessions) != 1 {
			t.Fatalf("post-sink expiry = %v, writes = %d", err, len(sink.sessions))
		}
	})

	t.Run("cancellation", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		sink := &auditSink{afterRecord: cancel}
		recorder, err := interaction.NewRecorder(context.Background(), sink)
		if err != nil {
			t.Fatal(err)
		}
		if err := recorder.SetClock(func() time.Time { return started }); err != nil {
			t.Fatal(err)
		}
		auditor, err := NewInteractionAuditor(recorder, testAuditSnapshots{})
		if err != nil {
			t.Fatal(err)
		}
		err = auditor.RecordFleetAction(ctx, AuditRecord{
			Operation: auth.OperationEventPublish, ActionID: []byte("canceled"),
			ObjectID: []byte("event"), AuthorizationFingerprint: auth.Fingerprint{1},
			AuthorizationExpiresAt: started.Add(time.Hour),
			OccurredAt:             started.Add(-time.Second),
		})
		if !interaction.IsCommittedRecord(err) ||
			!errors.Is(err, context.Canceled) || len(sink.sessions) != 1 {
			t.Fatalf("post-sink cancellation = %v, writes = %d", err, len(sink.sessions))
		}
	})
}

func TestInteractionAuditorRetryUsesAuthoritativeReceipt(t *testing.T) {
	original := time.Date(2026, 9, 6, 10, 0, 0, 0, time.UTC)
	snapshots := &changingAuditSnapshots{snapshot: explorer.Snapshot{
		ID: "original-snapshot", AsOf: original.Add(-time.Minute),
	}}
	sink := &auditSink{}
	auditor, err := NewInteractionAuditorWithReader(sink, sink, snapshots)
	if err != nil {
		t.Fatal(err)
	}
	record := AuditRecord{
		Operation: auth.OperationEventPublish, ActionID: []byte("retry"),
		ObjectID: []byte("event"), AuthorizationFingerprint: auth.Fingerprint{1},
		AuthorizationExpiresAt: original.Add(time.Second), OccurredAt: original,
	}
	if err := auditor.RecordFleetAction(context.Background(), record); err != nil {
		t.Fatal(err)
	}
	snapshots.snapshot = explorer.Snapshot{
		ID: "changed-snapshot", AsOf: original.Add(time.Hour),
	}
	if err := auditor.RecordFleetActionRetry(context.Background(), record); err != nil {
		t.Fatalf("authoritative retry after snapshot and expiry changes = %v", err)
	}
	if len(sink.sessions) != 1 {
		t.Fatalf("retry wrote another interaction: %d", len(sink.sessions))
	}
}

func TestInteractionAuditorRetryRejectsMismatchedReceipt(t *testing.T) {
	now := time.Date(2026, 9, 6, 10, 0, 0, 0, time.UTC)
	sink := &auditSink{}
	auditor, err := NewInteractionAuditorWithReader(sink, sink, testAuditSnapshots{})
	if err != nil {
		t.Fatal(err)
	}
	record := AuditRecord{
		Operation: auth.OperationEventPublish, ActionID: []byte("retry"),
		ObjectID: []byte("event"), AuthorizationFingerprint: auth.Fingerprint{1},
		AuthorizationExpiresAt: now.Add(time.Hour), OccurredAt: now,
	}
	if err := auditor.RecordFleetAction(context.Background(), record); err != nil {
		t.Fatal(err)
	}
	sink.receipt.AuthorizationOperation = string(auth.OperationSubscriptionDelete)
	err = auditor.RecordFleetActionRetry(context.Background(), record)
	if !interaction.IsCommittedRecord(err) {
		t.Fatalf("mismatched authoritative receipt = %v", err)
	}
}

func TestInteractionAuditorRetryRejectsChangedAuthorizationPins(t *testing.T) {
	now := time.Date(2026, 9, 7, 10, 0, 0, 0, time.UTC)
	sink := &auditSink{}
	auditor, err := NewInteractionAuditorWithReader(sink, sink, testAuditSnapshots{})
	if err != nil {
		t.Fatal(err)
	}
	record := AuditRecord{
		Operation: auth.OperationEventPublish, ActionID: []byte("retry"),
		RequestID: "original", CorrelationID: []byte("correlation"),
		ObjectID: []byte("event"), AuthorizationFingerprint: auth.Fingerprint{1},
		AuthorizationExpiresAt: now.Add(time.Hour), OccurredAt: now,
	}
	if err := auditor.RecordFleetAction(context.Background(), record); err != nil {
		t.Fatal(err)
	}
	record.RequestID = "changed"
	err = auditor.RecordFleetActionRetry(context.Background(), record)
	if !interaction.IsCommittedRecord(err) {
		t.Fatalf("changed authorization pins = %v", err)
	}
}

func TestInteractionAuditorRetryRejectsDeletedReceipt(t *testing.T) {
	now := time.Date(2026, 9, 7, 10, 0, 0, 0, time.UTC)
	sink := &auditSink{}
	auditor, err := NewInteractionAuditorWithReader(sink, sink, testAuditSnapshots{})
	if err != nil {
		t.Fatal(err)
	}
	record := AuditRecord{
		Operation: auth.OperationEventPublish, ActionID: []byte("retry"),
		ObjectID: []byte("event"), AuthorizationFingerprint: auth.Fingerprint{1},
		AuthorizationExpiresAt: now.Add(time.Hour), OccurredAt: now,
	}
	if err := auditor.RecordFleetAction(context.Background(), record); err != nil {
		t.Fatal(err)
	}
	sink.deleted = true
	err = auditor.RecordFleetActionRetry(context.Background(), record)
	if !interaction.IsCommittedRecord(err) ||
		!shoal.IsErrorCode(err, shoal.ErrorNotFound) {
		t.Fatalf("deleted authoritative receipt = %v", err)
	}
}

func TestOperationInteractionAuditorSelectsBoundSink(t *testing.T) {
	now := time.Date(2026, 9, 7, 10, 0, 0, 0, time.UTC)
	sink := &auditSink{}
	var selected auth.Operation
	auditor, err := NewOperationInteractionAuditorWithReader(
		func(operation auth.Operation) interaction.ResultSink {
			selected = operation
			return sink
		},
		sink, testAuditSnapshots{},
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := auditor.RecordFleetAction(context.Background(), AuditRecord{
		Operation: auth.OperationInvoke, ActionID: []byte("action"),
		ObjectID: []byte("event"), AuthorizationFingerprint: auth.Fingerprint{1},
		AuthorizationExpiresAt: now.Add(time.Hour), OccurredAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	if selected != auth.OperationInvoke {
		t.Fatalf("selected operation = %q", selected)
	}
}

func TestOperationInteractionAuditorUsesTrustedReconciliationSink(t *testing.T) {
	now := time.Date(2026, 9, 7, 10, 0, 0, 0, time.UTC)
	sink := &auditSink{}
	auditor, err := NewOperationInteractionAuditorWithReader(
		func(auth.Operation) interaction.ResultSink { return sink },
		sink, testAuditSnapshots{},
	)
	if err != nil {
		t.Fatal(err)
	}
	record := AuditRecord{
		Operation: auth.OperationDispatch, ActionID: []byte("action"),
		RequestID: "original-request", ObjectID: []byte("event"),
		AuthorizationFingerprint: auth.Fingerprint{1},
		AuthorizationExpiresAt:   now.Add(time.Hour), OccurredAt: now,
	}
	if err := auditor.RecordFleetActionReconciliation(
		context.Background(), explorerfleetcap.New(), record,
	); err != nil {
		t.Fatal(err)
	}
	if sink.reconciliations != 1 || len(sink.sessions) != 1 ||
		sink.sessions[0].RequestID != record.RequestID ||
		!sink.sessions[0].RecordedAt.Equal(record.OccurredAt) {
		t.Fatalf(
			"reconciliation calls = %d, sessions = %#v",
			sink.reconciliations, sink.sessions,
		)
	}
}

func TestInteractionAuditorRetryDoesNotCreateMissingReceipt(t *testing.T) {
	now := time.Date(2026, 9, 6, 10, 0, 0, 0, time.UTC)
	sink := &auditSink{readErr: shoal.NewError(shoal.ErrorNotFound, "missing")}
	auditor, err := NewInteractionAuditorWithReader(sink, sink, testAuditSnapshots{})
	if err != nil {
		t.Fatal(err)
	}
	err = auditor.RecordFleetActionRetry(context.Background(), AuditRecord{
		Operation: auth.OperationEventPublish, ActionID: []byte("retry"),
		ObjectID: []byte("event"), AuthorizationFingerprint: auth.Fingerprint{1},
		AuthorizationExpiresAt: now.Add(time.Hour), OccurredAt: now,
	})
	if !shoal.IsErrorCode(err, shoal.ErrorNotFound) ||
		interaction.IsCommittedRecord(err) || len(sink.sessions) != 0 {
		t.Fatalf("missing retry receipt = %v, writes = %d", err, len(sink.sessions))
	}
}

type changingAuditSnapshots struct {
	snapshot explorer.Snapshot
}

func (s *changingAuditSnapshots) InteractionSnapshot(
	context.Context,
) (explorer.Snapshot, error) {
	return s.snapshot, nil
}

type auditSink struct {
	sessions        []interaction.Session
	reconciliations int
	mutateResult    func(interaction.Session) interaction.Session
	afterRecord     func()
	receipt         interaction.Session
	readErr         error
	deleted         bool
}

func (*auditSink) EnsureInteractionSink(context.Context) error { return nil }

func (s *auditSink) RecordInteraction(_ context.Context, session interaction.Session) error {
	s.sessions = append(s.sessions, session)
	return nil
}

func (s *auditSink) RecordInteractionResult(
	ctx context.Context, session interaction.Session,
) (interaction.Session, error) {
	if err := s.RecordInteraction(ctx, session); err != nil {
		return interaction.Session{}, err
	}
	result := session
	result.Actor = interaction.ActorContext{
		SubjectID: "trusted-subject", ActorID: "trusted-actor",
	}
	result.Reason = interaction.Reason{Code: "audit_purpose"}
	if s.mutateResult != nil {
		result = s.mutateResult(result)
	}
	if s.afterRecord != nil {
		s.afterRecord()
	}
	s.receipt = result
	return result, nil
}

func (s *auditSink) RecordReconciledInteractionResult(
	ctx context.Context, _ explorerfleetcap.Capability,
	session interaction.Session,
) (interaction.Session, error) {
	s.reconciliations++
	return s.RecordInteractionResult(ctx, session)
}

func (s *auditSink) InteractionRecord(
	context.Context, shoal.ID,
) (explorer.InteractionRecord, error) {
	if s.readErr != nil {
		return explorer.InteractionRecord{}, s.readErr
	}
	return explorer.InteractionRecord{
		Summary: explorer.InteractionSummary{Deleted: s.deleted},
		Session: s.receipt,
	}, nil
}
