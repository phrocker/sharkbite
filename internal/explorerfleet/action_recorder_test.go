// Licensed to the Apache Software Foundation (ASF) under one or more
// contributor license agreements. See the NOTICE file distributed with this
// work for additional information regarding copyright ownership.
package explorerfleet

import (
	"context"
	"crypto/sha256"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/phrocker/shoal-oss/pkg/document"
	"github.com/phrocker/shoal-oss/pkg/explorer"
	"github.com/phrocker/shoal-oss/pkg/explorer/auth"
	"github.com/phrocker/shoal-oss/pkg/explorer/fleet"
	"github.com/phrocker/shoal-oss/pkg/interaction"
	"github.com/phrocker/shoal-oss/pkg/ontology"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

func TestActionRecorderPreservesExactEvidenceAndTrustedResult(t *testing.T) {
	record := testActionRecord()
	record.EvidenceSnapshotID = "snapshot"
	record.EvidenceSnapshotAsOf = time.Date(2026, 9, 6, 8, 0, 0, 0, time.UTC)
	record.ExecutionFingerprint = auth.Fingerprint(sha256.Sum256([]byte("authorization")))
	record.ExecutionExpiresAt = record.Deadline
	sink := &capturingActionRecorder{record: record}
	recorder, err := NewActionRecorder(sink)
	if err != nil {
		t.Fatal(err)
	}

	record.Evidence = []fleet.EvidenceRef{
		{
			AnchorID: "anchor", Kind: interaction.EvidenceDocument,
			Citation: document.Citation{
				DocumentID: "document", RevisionID: "revision", SpanID: "span",
				Range: document.SourceRange{
					Start: document.SourcePosition{Offset: 4},
					End:   document.SourcePosition{Offset: 9},
				},
			},
			NodeIDs:    []shoal.ID{"document", "span"},
			Visibility: []string{"restricted"},
		},
		{
			AnchorID: "graph-anchor", Kind: interaction.EvidenceGraph,
			NodeIDs: []shoal.ID{"left", "right"}, EdgeIDs: []shoal.ID{"edge"},
			Assertions: []interaction.AssertionReference{{
				AssertionID: "assertion", EdgeID: "edge",
				Origin: ontology.AssertionExplicit,
			}},
			Visibility: []string{"restricted"},
		},
	}
	audit := fleet.ActionAudit{
		Phase: "effect_outcome", Operation: auth.OperationInvoke,
		Record: record,
	}
	if err := recorder.RecordAction(context.Background(), audit); err != nil {
		t.Fatal(err)
	}
	if len(sink.sessions) != 1 ||
		len(sink.sessions[0].Turns[0].ToolCall.RetrievedEvidence) != 2 {
		t.Fatalf("captured sessions = %#v", sink.sessions)
	}
	session := sink.sessions[0]
	if session.AuthorizationOperation != string(auth.OperationInvoke) {
		t.Fatalf("authorization operation = %q", session.AuthorizationOperation)
	}
	captured := session.Turns[0].ToolCall.RetrievedEvidence[0]
	if captured.AnchorID != record.Evidence[0].AnchorID ||
		captured.Kind != record.Evidence[0].Kind ||
		!reflect.DeepEqual(captured.Citation, record.Evidence[0].Citation) ||
		!reflect.DeepEqual(captured.NodeIDs, record.Evidence[0].NodeIDs) ||
		session.SnapshotID != record.EvidenceSnapshotID ||
		!session.SnapshotAsOf.Equal(record.EvidenceSnapshotAsOf) ||
		!reflect.DeepEqual(
			session.Turns[0].ToolCall.RetrievedNodeIDs,
			[]shoal.ID{"document", "left", "right", "span"}) {
		t.Fatalf("captured evidence = %#v", captured)
	}
	graph := session.Turns[0].ToolCall.RetrievedEvidence[1]
	if graph.Kind != interaction.EvidenceGraph ||
		!reflect.DeepEqual(graph.Assertions, record.Evidence[1].Assertions) ||
		!reflect.DeepEqual(graph.EdgeIDs, record.Evidence[1].EdgeIDs) {
		t.Fatalf("captured graph evidence = %#v", graph)
	}
}

func TestActionRecorderRejectsAbsentAndDivergentResult(t *testing.T) {
	var typedNil *capturingActionRecorder
	if _, err := NewActionRecorder(typedNil); !shoal.IsErrorCode(
		err, shoal.ErrorInvalidArgument,
	) {
		t.Fatalf("typed nil recorder error = %v", err)
	}
	record := testActionRecord()
	sink := &capturingActionRecorder{record: record, diverge: true}
	recorder, err := NewActionRecorder(sink)
	if err != nil {
		t.Fatal(err)
	}
	if err := recorder.RecordAction(context.Background(), fleet.ActionAudit{
		Phase: "enqueue_admission", Operation: auth.OperationDispatch,
		Record: record,
	}); !shoal.IsErrorCode(err, shoal.ErrorInternal) ||
		!explorer.IsCommittedInteraction(err) {
		t.Fatalf("divergent result error = %v", err)
	}
	if got := sink.sessions[0].AuthorizationOperation; got != string(auth.OperationDispatch) {
		t.Fatalf("authorization operation = %q", got)
	}
}

func TestActionRecorderReturnsSinkErrorsUnchanged(t *testing.T) {
	record := testActionRecord()
	sinkErr := errors.New("ambiguous durable sink")
	recorder, err := NewActionRecorder(&capturingActionRecorder{
		record: record, err: sinkErr,
	})
	if err != nil {
		t.Fatal(err)
	}

	err = recorder.RecordAction(context.Background(), fleet.ActionAudit{
		Phase: "effect_outcome", Operation: auth.OperationInvoke, Record: record,
	})
	if !errors.Is(err, sinkErr) {
		t.Fatalf("sink error = %v", err)
	}
}

func TestActionRecorderMarksErrorCodeOutcomeFailed(t *testing.T) {
	record := testActionRecord()
	record.Version = 2
	record.State = fleet.DispatchFailed
	record.ClaimID = []byte("claim")
	record.ClaimFence = 1
	record.ClaimLease = time.Minute
	record.ClaimLeaseUntil = record.Deadline
	record.ExecutionPolicyGeneration = 1
	record.ExecutionExpiresAt = record.Deadline
	record.EffectPossible = true
	record.ErrorCode = "executor_failure"
	sink := &capturingActionRecorder{record: record}
	recorder, err := NewActionRecorder(sink)
	if err != nil {
		t.Fatal(err)
	}
	if err := recorder.RecordAction(context.Background(), fleet.ActionAudit{
		Phase: "effect_outcome", Operation: auth.OperationInvoke, Record: record,
	}); err != nil {
		t.Fatal(err)
	}
	if !sink.sessions[0].Turns[0].Failed {
		t.Fatal("error-code outcome was not recorded as failed")
	}
}

func TestActionRecorderPinsNoEvidenceToTrustedSnapshot(t *testing.T) {
	record := testActionRecord()
	record.AuthorizationFingerprint = auth.Fingerprint(
		sha256.Sum256([]byte("authorization")))
	snapshot := explorer.Snapshot{
		ID:   "snapshot",
		AsOf: time.Date(2026, 9, 6, 8, 30, 0, 0, time.UTC),
	}
	sink := &capturingActionRecorder{record: record}
	recorder, err := NewActionRecorderWithSnapshots(
		sink, fixedActionSnapshotProvider{snapshot: snapshot})
	if err != nil {
		t.Fatal(err)
	}
	if err := recorder.RecordAction(context.Background(), fleet.ActionAudit{
		Phase: "enqueue_admission", Operation: auth.OperationDispatch,
		Record: record,
	}); err != nil {
		t.Fatal(err)
	}
	if len(sink.sessions) != 1 {
		t.Fatalf("captured sessions = %d", len(sink.sessions))
	}
	session := sink.sessions[0]
	if session.SnapshotID != shoal.ID(snapshot.ID) ||
		!session.SnapshotAsOf.Equal(snapshot.AsOf) ||
		session.AuthorizationFingerprint !=
			shoal.ID(record.AuthorizationFingerprint.String()) ||
		!session.AuthorizationExpiresAt.Equal(
			record.AuthorizationExpiresAt) {
		t.Fatalf("action audit pins = %#v", session)
	}

	var typedNil *fixedActionSnapshotProvider
	if _, err := NewActionRecorderWithSnapshots(
		sink, typedNil,
	); !shoal.IsErrorCode(err, shoal.ErrorInvalidArgument) {
		t.Fatalf("typed nil snapshot provider error = %v", err)
	}
}

type capturingActionRecorder struct {
	sessions []interaction.Session
	record   fleet.ActionRecord
	err      error
	diverge  bool
}

type fixedActionSnapshotProvider struct {
	snapshot explorer.Snapshot
}

func (p fixedActionSnapshotProvider) InteractionSnapshot(
	context.Context,
) (explorer.Snapshot, error) {
	return p.snapshot, nil
}

func (r *capturingActionRecorder) Record(
	_ context.Context,
	session interaction.Session,
) (interaction.Session, error) {
	if r.err != nil {
		return interaction.Session{}, r.err
	}
	session.RecordedAt = time.Date(2026, 9, 6, 9, 0, 0, 0, time.UTC)
	session.Actor = interaction.ActorContext{
		SubjectID: r.record.Subject, ActorID: r.record.Actor,
		ClientID:   r.record.ClientID,
		OnBehalfOf: append([]shoal.ID(nil), r.record.OnBehalfOf...),
	}
	canonical, err := session.Canonical()
	if err != nil {
		return interaction.Session{}, err
	}
	r.sessions = append(r.sessions, canonical)
	if r.diverge {
		canonical.ResultID = "divergent"
	}
	return canonical, nil
}

// callerStampingRecorder stamps the session actor the way the real trusted
// sink does — from the *caller's* resolved decision, not from the record.
//
// This is the whole reason #480 item 2 survived five review rounds:
// capturingActionRecorder stamps it from r.record.Subject/Actor, hard-coding
// the same assumption the real sink breaks, so the package's own tests could
// not see the defect. A test double that performs no attribution proves
// nothing about attribution.
type callerStampingRecorder struct {
	caller   interaction.ActorContext
	sessions []interaction.Session
}

func (r *callerStampingRecorder) Record(
	_ context.Context, session interaction.Session,
) (interaction.Session, error) {
	session.RecordedAt = time.Date(2026, 9, 6, 9, 0, 0, 0, time.UTC)
	session.Actor = r.caller
	canonical, err := session.Canonical()
	if err != nil {
		return interaction.Session{}, err
	}
	r.sessions = append(r.sessions, canonical)
	return canonical, nil
}

// TestAForeignClaimantsAuditIsNotAMismatch is #480 item 2.
//
// The recorder compared the sink's stamped actor against the record's own
// Subject/Actor/ClientID/OnBehalfOf — the enqueuer's chain. The real sink
// stamps the caller's. Those differ for any claimant that is not the
// enqueuer, which since #437 is the ordinary case, and the mismatch raised
// "fleet action recorder returned a mismatched trusted session" *before*
// store.ApplyAction. So a worker granted execute could never claim at all:
// not a mislabelled audit, a capability that did not work, with a growing
// queue and no record of why.
func TestAForeignClaimantsAuditIsNotAMismatch(t *testing.T) {
	record := testActionRecord()
	record.State = fleet.DispatchClaimed
	record.ClaimID = []byte("worker-claim")
	record.ClaimFence = 1
	record.ClaimLease = time.Minute
	record.ClaimLeaseUntil = record.UpdatedAt.Add(time.Minute).UTC()
	record.ExecutionPolicyGeneration = 1
	record.ExecutionExpiresAt = record.Deadline
	record.ClaimantSubject = "worker-subject"
	record.ClaimantActor = "worker-actor"
	record.ClaimantClientID = "worker-client"
	record.ClaimantOnBehalfOf = []shoal.ID{"worker-delegator"}
	if record.Subject == record.ClaimantSubject {
		t.Fatal("the claimant is the enqueuer, so this fixture cannot " +
			"distinguish the two chains")
	}

	for _, probe := range []struct {
		name   string
		caller interaction.ActorContext
		// refused says whether the recorder must reject the attribution.
		refused bool
	}{
		{
			// The case that was broken. A worker holding execute claims work
			// it did not enqueue; the sink stamps the worker.
			name: "the claimant",
			caller: interaction.ActorContext{
				SubjectID: record.ClaimantSubject,
				ActorID:   record.ClaimantActor,
				ClientID:  record.ClaimantClientID,
				OnBehalfOf: append(
					[]shoal.ID(nil), record.ClaimantOnBehalfOf...),
			},
		},
		{
			// Still accepted: the enqueuer reaching its own action, which is
			// every phase before a foreign claim and Cancel after one.
			name: "the action's own principal",
			caller: interaction.ActorContext{
				SubjectID: record.Subject, ActorID: record.Actor,
				ClientID: record.ClientID,
				OnBehalfOf: append(
					[]shoal.ID(nil), record.OnBehalfOf...),
			},
		},
		{
			// And the check still has teeth. Accepting the stamped actor
			// unchecked would let a sink attribute this action's audit to
			// anyone, which is worse than the false assertion it replaced.
			name: "a stranger",
			caller: interaction.ActorContext{
				SubjectID: "stranger", ActorID: "stranger-actor",
			},
			refused: true,
		},
		{
			// One component off the claimant is a different principal, not a
			// near-match. The chain is compared element for element because
			// that is what identity means here.
			name: "the claimant with a different delegator",
			caller: interaction.ActorContext{
				SubjectID:  record.ClaimantSubject,
				ActorID:    record.ClaimantActor,
				ClientID:   record.ClaimantClientID,
				OnBehalfOf: []shoal.ID{"other-delegator"},
			},
			refused: true,
		},
		{
			name: "the claimant with an emptied chain",
			caller: interaction.ActorContext{
				SubjectID: record.ClaimantSubject,
				ActorID:   record.ClaimantActor,
				ClientID:  record.ClaimantClientID,
			},
			refused: true,
		},
	} {
		t.Run(probe.name, func(t *testing.T) {
			sink := &callerStampingRecorder{caller: probe.caller}
			recorder, err := NewActionRecorder(sink)
			if err != nil {
				t.Fatal(err)
			}
			// claim_admission is the phase that fires before the write, so it
			// is the one whose refusal stops the claim landing at all.
			err = recorder.RecordAction(context.Background(), fleet.ActionAudit{
				Phase: "claim_admission", Operation: auth.OperationExecute,
				Record: record,
			})
			if probe.refused {
				if err == nil {
					t.Fatal("the recorder accepted an audit attributed to a " +
						"principal with no standing on this action")
				}
				return
			}
			if err != nil {
				t.Fatalf("a legitimate caller's audit was rejected as a "+
					"mismatched trusted session, which refuses before the "+
					"durable write: %v", err)
			}
			if len(sink.sessions) != 1 {
				t.Fatalf("captured %d sessions, want 1", len(sink.sessions))
			}
			if sink.sessions[0].Actor.SubjectID != probe.caller.SubjectID {
				t.Fatalf("recorded actor = %q, want the caller %q",
					sink.sessions[0].Actor.SubjectID, probe.caller.SubjectID)
			}
		})
	}
}
