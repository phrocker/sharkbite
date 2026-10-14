// Licensed to the Apache Software Foundation (ASF) under one or more
// contributor license agreements. See the NOTICE file distributed with this
// work for additional information regarding copyright ownership.
package explorerfleet

import (
	"context"
	"testing"
	"time"

	"github.com/phrocker/shoal-oss/pkg/explorer"
	"github.com/phrocker/shoal-oss/pkg/explorer/auth"
	"github.com/phrocker/shoal-oss/pkg/explorer/fleet"
	"github.com/phrocker/shoal-oss/pkg/interaction"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

// stampingSink is the trusted sink's contract as these recorders see it: it
// returns the requested session with the recording decision's actor, reason
// and correlation stamped on, as authorized.Client does (#532).
type stampingSink struct {
	actor       interaction.ActorContext
	reason      interaction.Reason
	correlation shoal.ID
	recordedAt  time.Time
	sessions    []interaction.Session
}

func (s *stampingSink) Record(
	ctx context.Context, session interaction.Session,
) (interaction.Session, error) {
	return s.RecordInteractionResult(ctx, session)
}

func (*stampingSink) EnsureInteractionSink(context.Context) error { return nil }

func (s *stampingSink) RecordInteraction(
	ctx context.Context, session interaction.Session,
) error {
	_, err := s.RecordInteractionResult(ctx, session)
	return err
}

func (s *stampingSink) RecordInteractionResult(
	_ context.Context, session interaction.Session,
) (interaction.Session, error) {
	session.RecordedAt = s.recordedAt
	session.Actor = s.actor
	session.Reason = s.reason
	session.CorrelationID = s.correlation
	canonical, err := session.Canonical()
	if err != nil {
		return interaction.Session{}, err
	}
	s.sessions = append(s.sessions, canonical)
	return canonical, nil
}

func (s *stampingSink) only(t *testing.T) interaction.Session {
	t.Helper()
	if len(s.sessions) != 1 {
		t.Fatalf("recorded %d sessions, want 1", len(s.sessions))
	}
	return s.sessions[0]
}

var correlationSnapshot = fixedActionSnapshotProvider{snapshot: explorer.Snapshot{
	ID: "snapshot", AsOf: time.Date(2026, 9, 6, 8, 0, 0, 0, time.UTC),
}}

// Each fleet recorder accepts the correlation the trusted sink stamped. A
// recorder that did not would refuse every audit the hosted sink returns
// once that sink stamps one — so each of these fails if its recorder drops
// the field (#532).

func TestActionRecorderCarriesTheSinkCorrelation(t *testing.T) {
	record := testActionRecord()
	sink := &stampingSink{
		actor: interaction.ActorContext{
			SubjectID: record.Subject, ActorID: record.Actor,
			ClientID: record.ClientID, OnBehalfOf: record.OnBehalfOf,
		},
		correlation: "upstream-action-532",
		recordedAt:  time.Date(2026, 9, 6, 12, 30, 0, 0, time.UTC),
	}
	recorder, err := NewActionRecorderWithSnapshots(sink, correlationSnapshot)
	if err != nil {
		t.Fatal(err)
	}
	if err := recorder.RecordAction(context.Background(), fleet.ActionAudit{
		Phase: "enqueue_admission", Operation: auth.OperationDispatch,
		Record: record,
	}); err != nil {
		t.Fatal(err)
	}
	if got := sink.only(t).CorrelationID; got != "upstream-action-532" {
		t.Fatalf("action session correlation = %q", got)
	}
}

func TestApprovalRecorderCarriesTheSinkCorrelation(t *testing.T) {
	record := decidedApprovalRecord()
	audit := fleet.ApprovalAudit{
		Phase: "approval_decision", Operation: auth.OperationActionApprove,
		Record: record, Subject: record.ApproverSubject,
		Actor: record.ApproverActor, ClientID: record.ApproverClientID,
		RequestID:                "decision",
		AuthorizationFingerprint: auth.Fingerprint{9},
		AuthorizationExpiresAt:   time.Date(2026, 9, 7, 0, 0, 0, 0, time.UTC),
	}
	sink := &stampingSink{
		actor: interaction.ActorContext{
			SubjectID: audit.Subject, ActorID: audit.Actor,
			ClientID: audit.ClientID,
		},
		correlation: "upstream-approval-532",
		recordedAt:  time.Date(2026, 9, 6, 12, 30, 0, 0, time.UTC),
	}
	recorder, err := NewApprovalRecorder(sink, correlationSnapshot)
	if err != nil {
		t.Fatal(err)
	}
	if err := recorder.RecordApproval(context.Background(), audit); err != nil {
		t.Fatal(err)
	}
	if got := sink.only(t).CorrelationID; got != "upstream-approval-532" {
		t.Fatalf("approval session correlation = %q", got)
	}
}

func TestAttestationRecorderCarriesTheSinkCorrelation(t *testing.T) {
	audit := fleet.AttestationAudit{
		Phase: "attestation_refused", Reason: "no-client",
		Principal: fleet.AttestationPrincipal{
			Domain: []byte("domain"), Subject: "executor",
		},
		Actor: "executor-actor", RequestID: "present", ExecutorRef: "local",
		AuthorizationFingerprint: auth.Fingerprint{3},
		AuthorizationExpiresAt:   time.Date(2026, 9, 7, 0, 0, 0, 0, time.UTC),
	}
	sink := &stampingSink{
		actor: interaction.ActorContext{
			SubjectID: audit.Principal.Subject, ActorID: audit.Actor,
		},
		correlation: "upstream-attestation-532",
		recordedAt:  time.Date(2026, 9, 6, 12, 30, 0, 0, time.UTC),
	}
	recorder, err := NewAttestationRecorder(sink, correlationSnapshot)
	if err != nil {
		t.Fatal(err)
	}
	if err := recorder.RecordAttestation(context.Background(), audit); err != nil {
		t.Fatal(err)
	}
	if got := sink.only(t).CorrelationID; got != "upstream-attestation-532" {
		t.Fatalf("attestation session correlation = %q", got)
	}
}

func TestLifecycleRecorderCarriesTheSinkCorrelation(t *testing.T) {
	lifecycle := testLifecycle()
	reason, err := interaction.NewReason("audit_purpose", lifecycle.AuditPurpose)
	if err != nil {
		t.Fatal(err)
	}
	sink := &stampingSink{
		actor: interaction.ActorContext{
			SubjectID: lifecycle.Subject, ActorID: lifecycle.Actor,
			ClientID: lifecycle.ClientID, OnBehalfOf: lifecycle.OnBehalfOf,
		},
		reason:      reason,
		correlation: "upstream-lifecycle-532",
		recordedAt:  lifecycle.SnapshotAsOf.Add(time.Second),
	}
	recorder, err := NewLifecycleRecorder(sink)
	if err != nil {
		t.Fatal(err)
	}
	if err := recorder.RecordLifecycle(context.Background(), lifecycle); err != nil {
		t.Fatal(err)
	}
	if got := sink.only(t).CorrelationID; got != "upstream-lifecycle-532" {
		t.Fatalf("lifecycle session correlation = %q", got)
	}
}

// TestLifecycleReplayAcceptsTheStoredCorrelation: a receipt already durable
// carries the correlation it was first recorded under. Reconciling a retry
// against it accepts that one — a retry is the same receipt whatever its own
// correlation, and a pre-#532 receipt has none.
func TestLifecycleReplayAcceptsTheStoredCorrelation(t *testing.T) {
	// The sink's answer to a write that found the receipt already there.
	cause := shoal.NewError(shoal.ErrorConflict,
		"interaction session ID already exists with different content")
	lifecycle := testLifecycle()
	for _, stored := range []shoal.ID{"first-attempt-trace", ""} {
		accepted := lifecycleSession(lifecycle, interaction.CallerAssertedReason{})
		accepted.RecordedAt = lifecycle.SnapshotAsOf.Add(time.Second)
		accepted.Actor = interaction.ActorContext{
			SubjectID: lifecycle.Subject, ActorID: lifecycle.Actor,
			ClientID:   lifecycle.ClientID,
			OnBehalfOf: append([]shoal.ID(nil), lifecycle.OnBehalfOf...),
		}
		accepted.Reason, _ = interaction.NewReason(
			"audit_purpose", lifecycle.AuditPurpose)
		accepted.CorrelationID = stored
		store := &reconcilingLifecycleStore{
			trustedLifecycleRecorder: trustedLifecycleRecorder{lifecycle: lifecycle},
			stored:                   accepted,
			recordErr:                cause,
		}
		recorder, err := NewLifecycleRecorderWithReader(store, store)
		if err != nil {
			t.Fatal(err)
		}
		// Reconciled against the durable receipt, the retry succeeds.
		if err := recorder.RecordLifecycle(
			context.Background(), lifecycle); err != nil {
			t.Fatalf("stored correlation %q: replay = %v", stored, err)
		}
	}
}

// TestFleetSessionIdentityIgnoresCorrelation: every fleet session ID is
// derived from its audit, and an audit carries no correlation of its own. Two
// transitions whose records differ only in correlation therefore have one
// session identity — correlation cannot split them — and two different
// transitions under one correlation keep theirs — it cannot merge them.
func TestFleetSessionIdentityIgnoresCorrelation(t *testing.T) {
	one, two := testActionRecord(), testActionRecord()
	one.CorrelationID, two.CorrelationID = "trace-one", "trace-two"
	one.TransitionRequestID, two.TransitionRequestID = "request", "request"
	one.TransitionCorrelationID, two.TransitionCorrelationID = "trace-one", "trace-two"
	for _, phase := range []string{
		"enqueue_admission", fleet.ClaimRefusedAttestationPhase,
	} {
		left := fleet.ActionAudit{Phase: phase, Operation: auth.OperationDispatch, Record: one}
		right := fleet.ActionAudit{Phase: phase, Operation: auth.OperationDispatch, Record: two}
		if actionSessionID(left) != actionSessionID(right) {
			t.Fatalf("%s: correlation split the session", phase)
		}
	}
	merged := testActionRecord()
	merged.Version++
	if actionSessionID(fleet.ActionAudit{Phase: "enqueue_admission", Record: one}) ==
		actionSessionID(fleet.ActionAudit{Phase: "enqueue_admission", Record: merged}) {
		t.Fatal("two transitions share a session")
	}
	approval := decidedApprovalRecord()
	approvalOne, approvalTwo := approval, approval
	approvalOne.Request.CorrelationID = "trace-one"
	approvalTwo.Request.CorrelationID = "trace-two"
	approvalOne.DecisionCorrelationID = "trace-one"
	approvalTwo.DecisionCorrelationID = "trace-two"
	if approvalSessionID(fleet.ApprovalAudit{
		Phase: "approval_decision", Record: approvalOne, RequestID: "decision",
	}) != approvalSessionID(fleet.ApprovalAudit{
		Phase: "approval_decision", Record: approvalTwo, RequestID: "decision",
	}) {
		t.Fatal("correlation split an approval session")
	}
	lifecycleOne, lifecycleTwo := testLifecycle(), testLifecycle()
	lifecycleTwo.CorrelationID = "trace-two"
	if lifecycleSessionID(lifecycleOne) != lifecycleSessionID(lifecycleTwo) {
		t.Fatal("correlation split a lifecycle session")
	}
}
