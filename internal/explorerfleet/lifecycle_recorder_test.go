// Licensed to the Apache Software Foundation (ASF) under one or more
// contributor license agreements. See the NOTICE file distributed with this
// work for additional information regarding copyright ownership.
package explorerfleet

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/phrocker/shoal-oss/pkg/explorer"
	"github.com/phrocker/shoal-oss/pkg/explorer/auth"
	"github.com/phrocker/shoal-oss/pkg/explorer/fleet"
	"github.com/phrocker/shoal-oss/pkg/interaction"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

func TestLifecycleRecorderUsesExactPinsAndTrustedResult(t *testing.T) {
	lifecycle := testLifecycle()
	sink := &trustedLifecycleRecorder{lifecycle: lifecycle}
	recorder, err := NewLifecycleRecorder(sink)
	if err != nil {
		t.Fatal(err)
	}
	if err := recorder.RecordLifecycle(context.Background(), lifecycle); err != nil {
		t.Fatal(err)
	}
	requested := sink.requests[0]
	if requested.Operation != interaction.OperationToolCall ||
		!interaction.IsSessionID(requested.ID) ||
		!requested.RecordedAt.IsZero() ||
		requested.AuthorizationOperation != string(lifecycle.Operation) ||
		requested.AuthorizationFingerprint !=
			shoal.ID(lifecycle.AuthorizationFingerprint.String()) ||
		!requested.AuthorizationExpiresAt.Equal(
			lifecycle.AuthorizationExpiresAt) ||
		requested.SnapshotID != lifecycle.SnapshotID ||
		!requested.SnapshotAsOf.Equal(lifecycle.SnapshotAsOf) ||
		!reflect.DeepEqual(requested.Actor, interaction.ActorContext{}) ||
		requested.Reason != (interaction.Reason{}) ||
		len(requested.SeedNodeIDs) != 0 ||
		len(requested.CitedNodeIDs) != 0 ||
		len(requested.Turns) != 1 ||
		requested.Turns[0].Decision !=
			"admitted:"+string(lifecycle.Operation) ||
		requested.Turns[0].ToolCall == nil ||
		requested.Turns[0].ToolCall.Kind !=
			"fleet.registry."+string(lifecycle.Operation) ||
		requested.ResultID != lifecycle.AgentID {
		t.Fatalf("requested lifecycle session = %#v", requested)
	}
}

func TestLifecycleRecorderRetryIsByteStable(t *testing.T) {
	lifecycle := testLifecycle()
	sink := &trustedLifecycleRecorder{lifecycle: lifecycle}
	recorder, err := NewLifecycleRecorder(sink)
	if err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := recorder.RecordLifecycle(
			context.Background(), lifecycle,
		); err != nil {
			t.Fatal(err)
		}
	}
	if len(sink.requests) != 2 ||
		!reflect.DeepEqual(sink.requests[0], sink.requests[1]) {
		t.Fatalf("retry sessions differ: %#v", sink.requests)
	}
	refreshed := lifecycle
	refreshed.CorrelationID = "new-correlation"
	refreshed.Actor = "new-actor"
	refreshed.OnBehalfOf = []shoal.ID{"new-delegator"}
	refreshed.AuthorizationFingerprint = auth.Fingerprint{9, 8, 7}
	refreshed.AuthorizationExpiresAt = lifecycle.AuthorizationExpiresAt.Add(time.Hour)
	refreshed.SnapshotID = "new-snapshot"
	refreshed.SnapshotAsOf = lifecycle.SnapshotAsOf.Add(time.Minute)
	refreshed.AuditPurpose = "renewed purpose"
	refreshed.Deadline += int64(time.Minute)
	if lifecycleSessionID(refreshed) != lifecycleSessionID(lifecycle) {
		t.Fatal("renewed authority minted a duplicate lifecycle receipt ID")
	}
	store := &reconcilingLifecycleStore{trustedLifecycleRecorder: trustedLifecycleRecorder{
		lifecycle: lifecycle,
	}}
	reconciling, err := NewLifecycleRecorderWithReader(store, store)
	if err != nil {
		t.Fatal(err)
	}
	if err := reconciling.RecordLifecycle(
		context.Background(), lifecycle,
	); err != nil {
		t.Fatal(err)
	}
	store.lifecycle = refreshed
	if err := reconciling.RecordLifecycle(
		context.Background(), refreshed,
	); err != nil {
		t.Fatalf("refreshed retry = %v", err)
	}
	if len(store.requests) != 2 {
		t.Fatalf("record attempts = %d", len(store.requests))
	}
	expectedID := interaction.DerivedID(
		"session", "fleet.lifecycle.v4", string(lifecycle.Operation),
		string(lifecycle.RequestID), string(lifecycle.AgentID),
	)
	// The earlier identities are still derived exactly, because retries
	// reconcile against receipts written under them.
	if v3LifecycleSessionID(lifecycle) != interaction.DerivedID(
		"session", "fleet.lifecycle.v3", string(lifecycle.Operation),
		string(lifecycle.RequestID), string(lifecycle.AgentID),
	) {
		t.Fatal("v3 lifecycle receipt identity drifted")
	}
	if v2LifecycleSessionID(lifecycle) != interaction.DerivedID(
		"session", "fleet.lifecycle.v2", string(lifecycle.Operation),
		string(lifecycle.RequestID), string(lifecycle.AgentID),
	) {
		t.Fatal("v2 lifecycle receipt identity drifted")
	}
	if lifecycleSessionID(lifecycle) != expectedID {
		t.Fatalf(
			"lifecycle session ID = %q, want %q",
			lifecycleSessionID(lifecycle), expectedID,
		)
	}
}

func TestLifecycleRecorderReconcilesLegacyReceiptBeforeWritingV2(t *testing.T) {
	lifecycle := testLifecycle()
	accepted := lifecycleSession(lifecycle, interaction.CallerAssertedReason{})
	accepted.ID = v1LifecycleSessionID(lifecycle)
	accepted.RecordedAt = lifecycle.SnapshotAsOf.Add(time.Second)
	accepted.Actor = interaction.ActorContext{
		SubjectID: lifecycle.Subject, ActorID: lifecycle.Actor,
		ClientID:   lifecycle.ClientID,
		OnBehalfOf: append([]shoal.ID(nil), lifecycle.OnBehalfOf...),
	}
	accepted.Reason, _ = interaction.NewReason(
		"audit_purpose", lifecycle.AuditPurpose,
	)
	store := &reconcilingLifecycleStore{
		trustedLifecycleRecorder: trustedLifecycleRecorder{
			lifecycle: lifecycle,
		},
		stored: accepted,
	}
	recorder, err := NewLifecycleRecorderWithReader(store, store)
	if err != nil {
		t.Fatal(err)
	}
	if err := recorder.RecordLifecycle(
		context.Background(), lifecycle,
	); err != nil {
		t.Fatalf("legacy receipt reconciliation = %v", err)
	}
	if store.stored.ID != accepted.ID {
		t.Fatal("reconciliation rewrote the legacy receipt")
	}
	requireV4Receipt(t, store, lifecycle)
}

// requireV4Receipt checks that a retry reconciled with a legacy receipt also
// wrote exactly one v4 receipt, carrying the current registry digest, beside
// it (#521): the v1 digest the legacy receipt holds can be shared by two
// different mutations, so the one actually applied is recorded under v2.
func requireV4Receipt(
	t *testing.T, store *reconcilingLifecycleStore, lifecycle fleet.Lifecycle,
) {
	t.Helper()
	id := LifecycleReceiptID(
		lifecycle.Operation, lifecycle.RequestID, lifecycle.AgentID)
	written, ok := store.written[id]
	if !ok || len(store.requests) != 1 || store.requests[0].ID != id {
		t.Fatalf("legacy reconciliation did not write the v4 receipt: %d "+
			"requests", len(store.requests))
	}
	asserted, err := fleet.CallerAssertedRegistryReason(
		lifecycle.ReasonCode, lifecycle.ReasonDetail)
	if err != nil {
		t.Fatal(err)
	}
	if written.QueryDigest != lifecycleQueryDigest(lifecycle, asserted) {
		t.Fatal("v4 receipt does not carry the current registry digest")
	}
}

func TestLifecycleRecorderRejectsTypedNilAndMismatchedResult(t *testing.T) {
	var typedNil *trustedLifecycleRecorder
	if _, err := NewLifecycleRecorder(typedNil); !shoal.IsErrorCode(
		err, shoal.ErrorInvalidArgument,
	) {
		t.Fatalf("typed-nil recorder = %v", err)
	}
	lifecycle := testLifecycle()
	sink := &trustedLifecycleRecorder{
		lifecycle: lifecycle,
		mutate: func(session *interaction.Session) {
			session.AuthorizationOperation = string(auth.OperationRetrieve)
		},
	}
	recorder, err := NewLifecycleRecorder(sink)
	if err != nil {
		t.Fatal(err)
	}
	if err := recorder.RecordLifecycle(
		context.Background(), lifecycle,
	); !shoal.IsErrorCode(err, shoal.ErrorInternal) ||
		!explorer.IsCommittedInteraction(err) {
		t.Fatalf("mismatched trusted result = %v", err)
	}
}

func TestLifecycleRecorderRejectsForgedTrustedEnrichment(t *testing.T) {
	tests := map[string]func(*interaction.Session){
		"actor": func(session *interaction.Session) {
			session.Actor.ActorID = "forged-actor"
		},
		"reason": func(session *interaction.Session) {
			session.Reason, _ = interaction.NewReason(
				"audit_purpose", "forged purpose",
			)
		},
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			lifecycle := testLifecycle()
			recorder, err := NewLifecycleRecorder(&trustedLifecycleRecorder{
				lifecycle: lifecycle,
				mutate:    mutate,
			})
			if err != nil {
				t.Fatal(err)
			}
			err = recorder.RecordLifecycle(context.Background(), lifecycle)
			if !shoal.IsErrorCode(err, shoal.ErrorInternal) ||
				!explorer.IsCommittedInteraction(err) {
				t.Fatalf("forged trusted enrichment error = %v", err)
			}
		})
	}
}

func TestLifecycleRecorderPreservesCommittedAmbiguity(t *testing.T) {
	cause := context.DeadlineExceeded
	sink := &trustedLifecycleRecorder{
		lifecycle:           testLifecycle(),
		err:                 explorer.MarkCommittedInteraction(cause),
		returnResultOnError: true,
	}
	recorder, err := NewLifecycleRecorder(sink)
	if err != nil {
		t.Fatal(err)
	}
	err = recorder.RecordLifecycle(context.Background(), testLifecycle())
	if !errors.Is(err, cause) || !explorer.IsCommittedInteraction(err) {
		t.Fatalf("committed ambiguity = %v", err)
	}
}

func TestLifecycleRecorderRecoversDroppedCommittedResult(t *testing.T) {
	cause := context.DeadlineExceeded
	lifecycle := testLifecycle()
	accepted := lifecycleSession(lifecycle, interaction.CallerAssertedReason{})
	accepted.RecordedAt = lifecycle.SnapshotAsOf.Add(time.Second)
	accepted.Actor = interaction.ActorContext{
		SubjectID: lifecycle.Subject, ActorID: lifecycle.Actor,
		ClientID:   lifecycle.ClientID,
		OnBehalfOf: append([]shoal.ID(nil), lifecycle.OnBehalfOf...),
	}
	accepted.Reason, _ = interaction.NewReason(
		"audit_purpose", lifecycle.AuditPurpose,
	)
	store := &reconcilingLifecycleStore{
		trustedLifecycleRecorder: trustedLifecycleRecorder{
			lifecycle: lifecycle,
		},
		stored:    accepted,
		recordErr: explorer.MarkCommittedInteraction(cause),
	}
	recorder, err := NewLifecycleRecorderWithReader(store, store)
	if err != nil {
		t.Fatal(err)
	}
	err = recorder.RecordLifecycle(context.Background(), lifecycle)
	if !errors.Is(err, cause) || !explorer.IsCommittedInteraction(err) ||
		shoal.IsErrorCode(err, shoal.ErrorInternal) {
		t.Fatalf("dropped committed result = %v", err)
	}
}

func TestLifecycleRecorderRejectsDroppedDivergentCommittedResult(t *testing.T) {
	cause := context.DeadlineExceeded
	lifecycle := testLifecycle()
	accepted := lifecycleSession(lifecycle, interaction.CallerAssertedReason{})
	accepted.RecordedAt = lifecycle.SnapshotAsOf.Add(time.Second)
	accepted.Actor = interaction.ActorContext{
		SubjectID: lifecycle.Subject, ActorID: lifecycle.Actor,
		ClientID:   lifecycle.ClientID,
		OnBehalfOf: append([]shoal.ID(nil), lifecycle.OnBehalfOf...),
	}
	accepted.Reason, _ = interaction.NewReason(
		"audit_purpose", lifecycle.AuditPurpose,
	)
	accepted.ResultID = "different-agent"
	store := &reconcilingLifecycleStore{
		trustedLifecycleRecorder: trustedLifecycleRecorder{
			lifecycle: lifecycle,
		},
		stored:    accepted,
		recordErr: explorer.MarkCommittedInteraction(cause),
	}
	recorder, err := NewLifecycleRecorderWithReader(store, store)
	if err != nil {
		t.Fatal(err)
	}
	err = recorder.RecordLifecycle(context.Background(), lifecycle)
	if !errors.Is(err, cause) || !explorer.IsCommittedInteraction(err) ||
		!shoal.IsErrorCode(err, shoal.ErrorConflict) {
		t.Fatalf("dropped divergent committed result = %v", err)
	}
}

func TestLifecycleRecorderRejectsCommittedDivergentResult(t *testing.T) {
	cause := context.DeadlineExceeded
	sink := &trustedLifecycleRecorder{
		lifecycle:           testLifecycle(),
		err:                 explorer.MarkCommittedInteraction(cause),
		returnResultOnError: true,
		mutate: func(session *interaction.Session) {
			session.ResultID = "different-agent"
		},
	}
	recorder, err := NewLifecycleRecorder(sink)
	if err != nil {
		t.Fatal(err)
	}
	err = recorder.RecordLifecycle(context.Background(), testLifecycle())
	if !errors.Is(err, cause) || !explorer.IsCommittedInteraction(err) ||
		!shoal.IsErrorCode(err, shoal.ErrorInternal) {
		t.Fatalf("committed divergent result = %v", err)
	}
}

func TestLifecycleRecorderRejectsTrustedChronology(t *testing.T) {
	for name, recordedAt := range map[string]time.Time{
		"before snapshot": testLifecycle().SnapshotAsOf.Add(-time.Nanosecond),
		"at expiry":       testLifecycle().AuthorizationExpiresAt,
	} {
		t.Run(name, func(t *testing.T) {
			sink := &trustedLifecycleRecorder{
				lifecycle: testLifecycle(), recordedAt: recordedAt,
			}
			recorder, err := NewLifecycleRecorder(sink)
			if err != nil {
				t.Fatal(err)
			}
			err = recorder.RecordLifecycle(context.Background(), testLifecycle())
			if !shoal.IsErrorCode(err, shoal.ErrorInternal) ||
				!explorer.IsCommittedInteraction(err) {
				t.Fatalf("invalid chronology = %v", err)
			}
		})
	}
}

func testLifecycle() fleet.Lifecycle {
	now := time.Date(2026, 9, 6, 6, 0, 0, 0, time.UTC)
	return fleet.Lifecycle{
		Operation:     auth.OperationAgentRegister,
		RequestID:     "request",
		CorrelationID: "correlation",
		Subject:       "subject",
		Actor:         "actor",
		ClientID:      "client",
		OnBehalfOf:    []shoal.ID{"delegator"},
		AgentID:       "agent",
		Deadline:      now.Add(time.Minute).UnixNano(),
		AuthorizationFingerprint: auth.Fingerprint{
			1, 2, 3,
		},
		AuthorizationExpiresAt: now.Add(time.Hour),
		AuditPurpose:           "operate the registered agent",
		SnapshotID:             "snapshot",
		SnapshotAsOf:           now,
	}
}

type trustedLifecycleRecorder struct {
	lifecycle           fleet.Lifecycle
	requests            []interaction.Session
	mutate              func(*interaction.Session)
	err                 error
	recordedAt          time.Time
	returnResultOnError bool
}

func (*trustedLifecycleRecorder) EnsureInteractionSink(context.Context) error {
	return nil
}

func (r *trustedLifecycleRecorder) RecordInteraction(
	ctx context.Context,
	request interaction.Session,
) error {
	_, err := r.RecordInteractionResult(ctx, request)
	return err
}

func (r *trustedLifecycleRecorder) RecordInteractionResult(
	_ context.Context,
	request interaction.Session,
) (interaction.Session, error) {
	r.requests = append(r.requests, request)
	result := request
	result.RecordedAt = r.recordedAt
	if result.RecordedAt.IsZero() {
		result.RecordedAt = r.lifecycle.SnapshotAsOf.Add(time.Second)
	}
	result.Actor = interaction.ActorContext{
		SubjectID: r.lifecycle.Subject,
		ActorID:   r.lifecycle.Actor,
		ClientID:  r.lifecycle.ClientID,
		OnBehalfOf: append(
			[]shoal.ID(nil), r.lifecycle.OnBehalfOf...),
	}
	if r.lifecycle.AuditPurpose != "" {
		result.Reason, _ = interaction.NewReason(
			"audit_purpose", r.lifecycle.AuditPurpose)
	}
	if r.mutate != nil {
		r.mutate(&result)
	}
	if r.err != nil && !r.returnResultOnError {
		return interaction.Session{}, r.err
	}
	if r.err != nil {
		return result, r.err
	}
	return result, nil
}

// reconcilingLifecycleStore is a session store keyed by ID. stored is the
// first receipt (seeded by a test or written first); later receipts under
// other IDs are kept in written. A write to an ID that already holds a
// receipt is refused, as the corpus refuses it.
type reconcilingLifecycleStore struct {
	trustedLifecycleRecorder
	stored    interaction.Session
	written   map[shoal.ID]interaction.Session
	recordErr error
}

func (r *reconcilingLifecycleStore) lookup(id shoal.ID) (interaction.Session, bool) {
	if r.stored.ID != "" && r.stored.ID == id {
		return r.stored, true
	}
	session, ok := r.written[id]
	return session, ok
}

func (r *reconcilingLifecycleStore) RecordInteractionResult(
	ctx context.Context,
	request interaction.Session,
) (interaction.Session, error) {
	if r.recordErr != nil {
		r.requests = append(r.requests, request)
		return interaction.Session{}, r.recordErr
	}
	if _, exists := r.lookup(request.ID); exists {
		r.requests = append(r.requests, request)
		return interaction.Session{}, shoal.NewError(
			shoal.ErrorConflict,
			"interaction session ID already exists with different content",
		)
	}
	result, err := r.trustedLifecycleRecorder.RecordInteractionResult(
		ctx, request,
	)
	if err == nil {
		if r.stored.ID == "" {
			r.stored = result
		} else {
			if r.written == nil {
				r.written = make(map[shoal.ID]interaction.Session)
			}
			r.written[result.ID] = result
		}
	}
	return result, err
}

func (r *reconcilingLifecycleStore) InteractionRecord(
	_ context.Context,
	id shoal.ID,
) (explorer.InteractionRecord, error) {
	session, ok := r.lookup(id)
	if !ok {
		return explorer.InteractionRecord{}, shoal.NewError(
			shoal.ErrorNotFound, "interaction receipt not found",
		)
	}
	return explorer.InteractionRecord{Session: session}, nil
}
