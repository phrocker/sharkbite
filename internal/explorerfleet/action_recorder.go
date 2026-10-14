// Licensed to the Apache Software Foundation (ASF) under one or more
// contributor license agreements. See the NOTICE file distributed with this
// work for additional information regarding copyright ownership.
package explorerfleet

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"hash"
	"reflect"
	"strconv"

	"github.com/phrocker/shoal-oss/pkg/explorer"
	"github.com/phrocker/shoal-oss/pkg/explorer/fleet"
	"github.com/phrocker/shoal-oss/pkg/interaction"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

type ActionInteractionRecorder interface {
	Record(context.Context, interaction.Session) (interaction.Session, error)
}

// ActionRecorder records exact action evidence through the result-returning
// interaction recorder and rejects any sink result that changes the effect.
type ActionRecorder struct {
	recorder  ActionInteractionRecorder
	snapshots fleet.InteractionSnapshotProvider
}

func NewActionRecorder(
	recorder ActionInteractionRecorder,
) (*ActionRecorder, error) {
	if isNilActionInteractionRecorder(recorder) {
		return nil, shoal.NewError(
			shoal.ErrorInvalidArgument,
			"fleet action interaction recorder is required")
	}
	return &ActionRecorder{recorder: recorder}, nil
}

// NewActionRecorderWithSnapshots additionally pins action audits without
// executor evidence to the host's current durable corpus snapshot.
func NewActionRecorderWithSnapshots(
	recorder ActionInteractionRecorder,
	snapshots fleet.InteractionSnapshotProvider,
) (*ActionRecorder, error) {
	if isNilActionInteractionRecorder(recorder) ||
		isNilActionSnapshotProvider(snapshots) {
		return nil, shoal.NewError(
			shoal.ErrorInvalidArgument,
			"fleet action recorder dependencies are required")
	}
	return &ActionRecorder{recorder: recorder, snapshots: snapshots}, nil
}

func (r *ActionRecorder) RecordAction(
	ctx context.Context,
	audit fleet.ActionAudit,
) error {
	if audit.Phase == "" {
		return shoal.NewError(
			shoal.ErrorInvalidArgument, "fleet action audit phase is required")
	}
	if err := audit.Operation.Validate(); err != nil {
		return err
	}
	if err := audit.Record.Validate(); err != nil {
		return err
	}
	evidence := make([]interaction.EvidenceReference, len(audit.Record.Evidence))
	retrievedNodeIDs := make([]shoal.ID, 0, len(audit.Record.Evidence))
	for index, item := range audit.Record.Evidence {
		evidence[index] = interaction.EvidenceReference{
			AnchorID: item.AnchorID, Kind: item.Kind, Citation: item.Citation,
			NodeIDs: append([]shoal.ID(nil), item.NodeIDs...),
			EdgeIDs: append([]shoal.ID(nil), item.EdgeIDs...),
			Assertions: append(
				[]interaction.AssertionReference(nil), item.Assertions...),
		}
		retrievedNodeIDs = append(retrievedNodeIDs, item.NodeIDs...)
	}
	requested := interaction.Session{
		ID: actionSessionID(audit), Operation: interaction.OperationToolCall,
		AuthorizationOperation: string(audit.Operation),
		QueryDigest: interaction.Digest(
			string(audit.Operation) + ":" + audit.Phase),
		RequestID:  audit.Record.RequestID,
		ResultID:   shoal.ID(hex.EncodeToString(audit.Record.ID)),
		StopReason: audit.Phase,
		Turns: []interaction.Turn{{
			Index:    0,
			Decision: actionIdentifier(audit),
			Failed: audit.EffectError != nil ||
				audit.Record.State == fleet.DispatchFailed,
			ToolCall: &interaction.ToolCall{
				Kind:              actionIdentifier(audit),
				RetrievedNodeIDs:  retrievedNodeIDs,
				RetrievedEvidence: evidence,
			},
		}},
	}
	if len(evidence) > 0 {
		requested.SnapshotID = audit.Record.EvidenceSnapshotID
		requested.SnapshotAsOf = audit.Record.EvidenceSnapshotAsOf
		requested.AuthorizationFingerprint = shoal.ID(
			audit.Record.ExecutionFingerprint.String())
		requested.AuthorizationExpiresAt = audit.Record.ExecutionExpiresAt
	} else if r.snapshots != nil {
		snapshot, err := r.snapshots.InteractionSnapshot(ctx)
		if err != nil {
			return err
		}
		provenance := audit.Record.EventProvenance()
		requested.SnapshotID = shoal.ID(snapshot.ID)
		requested.SnapshotAsOf = snapshot.AsOf
		requested.AuthorizationFingerprint = shoal.ID(
			provenance.AuthorizationFingerprint.String())
		requested.AuthorizationExpiresAt =
			provenance.AuthorizationExpiresAt
	}
	persisted, err := r.recorder.Record(ctx, requested)
	if err != nil {
		return err
	}
	expected := requested
	expected.RecordedAt = persisted.RecordedAt
	// The actor is stamped by the trusted sink from the resolved decision —
	// the *caller's* chain. This used to assert the record's own
	// Subject/Actor/ClientID/OnBehalfOf instead, which is the *enqueuer's*,
	// and the two differ for any claimant that is not the enqueuer. Since
	// #437 that is the ordinary case, and the mismatch raised "fleet action
	// recorder returned a mismatched trusted session" before
	// store.ApplyAction — so a worker granted execute could never claim at
	// all, and the queue grew with no record of why (#480 item 2).
	//
	// Not simply accepted from persisted, either. The recorder has no
	// business asserting which identity performed a phase; it does have
	// business refusing a sink that stamped someone who has nothing to do
	// with this action. So the stamped actor must be one of the record's
	// known identities: its own principal, or the claimant chain the claim
	// durably recorded. A phase-to-identity table was considered and
	// rejected — it re-derives what the decision already knows, and goes
	// wrong the moment a phase is added.
	if err := refuseUnknownAuditActor(audit.Record, persisted.Actor); err != nil {
		return explorer.MarkCommittedInteraction(err)
	}
	expected.Actor = persisted.Actor
	expected.Reason = persisted.Reason
	// The trusted sink stamps the recording decision's correlation (#532).
	// It is accepted, not compared with the record's transition correlation:
	// those can differ, as the record's actor and the caller can (#480 item
	// 2), and correlation is metadata that must never fail an audit.
	expected.CorrelationID = persisted.CorrelationID
	expected, err = expected.Canonical()
	if err != nil {
		return explorer.MarkCommittedInteraction(err)
	}
	persisted, err = persisted.Canonical()
	if err != nil || !reflect.DeepEqual(persisted, expected) {
		return explorer.MarkCommittedInteraction(shoal.NewError(
			shoal.ErrorInternal,
			"fleet action recorder returned a mismatched trusted session"))
	}
	return nil
}

// refuseUnknownAuditActor refuses a trusted session whose actor is neither
// the action's own principal nor the principal that holds its claim.
//
// Both identities are on the record, and which one performed a given phase is
// the decision's business rather than this function's: an enqueue and a cancel
// are the principal, a claim and a completion are the claimant, and an
// admission report is whichever of them the admission surface authorized. What
// is checked here is the property that holds for every phase — that the sink
// did not attribute this action's audit to a stranger.
//
// A record with no claimant chain predates the field or has never been
// claimed; there the claimant was by construction the enqueuer.
func refuseUnknownAuditActor(
	record fleet.ActionRecord, actor interaction.ActorContext,
) error {
	if sameAuditActor(actor, record.Subject, record.Actor,
		record.ClientID, record.OnBehalfOf) {
		return nil
	}
	if record.ClaimantSubject != "" && sameAuditActor(
		actor, record.ClaimantSubject, record.ClaimantActor,
		record.ClaimantClientID, record.ClaimantOnBehalfOf) {
		return nil
	}
	// Names neither chain: the message is read by an operator reconciling a
	// committed action, and the identities are this record's own.
	return shoal.NewError(
		shoal.ErrorInternal,
		"fleet action audit was attributed to a principal that is neither "+
			"the action's own nor its claimant")
}

func sameAuditActor(
	actor interaction.ActorContext,
	subject, actorID, clientID shoal.ID,
	onBehalfOf []shoal.ID,
) bool {
	if actor.SubjectID != subject || actor.ActorID != actorID ||
		actor.ClientID != clientID ||
		len(actor.OnBehalfOf) != len(onBehalfOf) {
		return false
	}
	for index := range onBehalfOf {
		if actor.OnBehalfOf[index] != onBehalfOf[index] {
			return false
		}
	}
	return true
}

func actionSessionID(audit fleet.ActionAudit) shoal.ID {
	digest := sha256.New()
	writeActionField(digest, []byte("shoal.fleet.action-audit.v2"))
	writeActionField(digest, []byte(audit.Phase))
	writeActionField(digest, audit.Record.ID)
	writeActionField(
		digest, []byte(strconv.FormatUint(audit.Record.Version, 10)))
	// A refusal writes no version, so (phase, ID, version) would collapse
	// every refusal at one version into one session and name only the
	// first refused caller. The refusing request distinguishes them. Only
	// this phase, so every other session ID is unchanged.
	if audit.Phase == fleet.ClaimRefusedAttestationPhase {
		writeActionField(digest, []byte(audit.Record.TransitionRequestID))
	}
	return interaction.DerivedID(
		"session", hex.EncodeToString(digest.Sum(nil)))
}

func actionIdentifier(audit fleet.ActionAudit) string {
	return "fleet." + string(audit.Operation) + "." + audit.Phase
}

func writeActionField(digest hash.Hash, value []byte) {
	var length [8]byte
	binary.BigEndian.PutUint64(length[:], uint64(len(value)))
	_, _ = digest.Write(length[:])
	_, _ = digest.Write(value)
}

func isNilActionInteractionRecorder(recorder ActionInteractionRecorder) bool {
	if recorder == nil {
		return true
	}
	value := reflect.ValueOf(recorder)
	switch value.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map,
		reflect.Pointer, reflect.Slice:
		return value.IsNil()
	default:
		return false
	}
}

func isNilActionSnapshotProvider(
	provider fleet.InteractionSnapshotProvider,
) bool {
	if provider == nil {
		return true
	}
	value := reflect.ValueOf(provider)
	switch value.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map,
		reflect.Pointer, reflect.Slice:
		return value.IsNil()
	default:
		return false
	}
}

var _ fleet.ActionRecorder = (*ActionRecorder)(nil)
