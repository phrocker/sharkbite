// Licensed to the Apache Software Foundation (ASF) under one or more
// contributor license agreements. See the NOTICE file distributed with this
// work for additional information regarding copyright ownership.
package explorerfleet

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"reflect"
	"strconv"

	"github.com/phrocker/shoal-oss/pkg/explorer"
	"github.com/phrocker/shoal-oss/pkg/explorer/auth"
	"github.com/phrocker/shoal-oss/pkg/explorer/fleet"
	"github.com/phrocker/shoal-oss/pkg/interaction"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

// AttestationRecorder records executor attestation presentations as trusted
// interaction sessions, through the same result-returning recorder and
// snapshot provider the action audits use.
//
// The session is attributed to the presenting decision — which is the
// principal the attestation is for, since a caller can only attest itself —
// so, unlike the claim audits of #480 item 2, the expected and stamped actors
// agree.
type AttestationRecorder struct {
	recorder  ActionInteractionRecorder
	snapshots fleet.InteractionSnapshotProvider
}

func NewAttestationRecorder(
	recorder ActionInteractionRecorder,
	snapshots fleet.InteractionSnapshotProvider,
) (*AttestationRecorder, error) {
	if isNilActionInteractionRecorder(recorder) ||
		isNilActionSnapshotProvider(snapshots) {
		return nil, shoal.NewError(shoal.ErrorInvalidArgument,
			"fleet attestation recorder dependencies are required")
	}
	return &AttestationRecorder{recorder: recorder, snapshots: snapshots}, nil
}

func (r *AttestationRecorder) RecordAttestation(
	ctx context.Context, audit fleet.AttestationAudit,
) error {
	if audit.Phase == "" || audit.ExecutorRef == "" {
		return shoal.NewError(shoal.ErrorInvalidArgument,
			"fleet attestation audit is incomplete")
	}
	snapshot, err := r.snapshots.InteractionSnapshot(ctx)
	if err != nil {
		return err
	}
	identifier := "fleet." + string(auth.OperationExecute) + "." + audit.Phase
	requested := interaction.Session{
		ID:                     attestationSessionID(audit),
		Operation:              interaction.OperationToolCall,
		AuthorizationOperation: string(auth.OperationExecute),
		QueryDigest: interaction.Digest(
			string(auth.OperationExecute) + ":" + audit.Phase),
		RequestID: audit.RequestID,
		// The result names the outcome, never the reason: the reason is the
		// stop reason's suffix and stays in the operator's audit.
		ResultID:                 shoal.ID(hex.EncodeToString(attestationResult(audit))),
		StopReason:               stopReason(audit),
		SnapshotID:               shoal.ID(snapshot.ID),
		SnapshotAsOf:             snapshot.AsOf,
		AuthorizationFingerprint: shoal.ID(audit.AuthorizationFingerprint.String()),
		AuthorizationExpiresAt:   audit.AuthorizationExpiresAt,
		Turns: []interaction.Turn{{
			Index: 0, Decision: identifier,
			Failed:   audit.Phase != "attestation_presented",
			ToolCall: &interaction.ToolCall{Kind: identifier},
		}},
	}
	persisted, err := r.recorder.Record(ctx, requested)
	if err != nil {
		return err
	}
	expected := requested
	expected.RecordedAt = persisted.RecordedAt
	expected.Actor = interaction.ActorContext{
		SubjectID: audit.Principal.Subject, ActorID: audit.Actor,
		ClientID: audit.Principal.ClientID,
	}
	expected.Reason = persisted.Reason
	// The presenting decision's correlation, stamped by the trusted sink
	// (#532).
	expected.CorrelationID = persisted.CorrelationID
	expected, err = expected.Canonical()
	if err != nil {
		return explorer.MarkCommittedInteraction(err)
	}
	persisted, err = persisted.Canonical()
	if err != nil || !reflect.DeepEqual(persisted, expected) {
		return explorer.MarkCommittedInteraction(shoal.NewError(
			shoal.ErrorInternal,
			"fleet attestation recorder returned a mismatched trusted session"))
	}
	return nil
}

func stopReason(audit fleet.AttestationAudit) string {
	if audit.Reason == "" {
		return audit.Phase
	}
	return audit.Phase + ":" + audit.Reason
}

// attestationResult identifies the outcome: the attestation for an accepted
// presentation, the presentation itself for a refusal.
func attestationResult(audit fleet.AttestationAudit) []byte {
	digest := sha256.New()
	writeActionField(digest, []byte("shoal.fleet.attestation-result.v1"))
	writeActionField(digest, []byte(audit.AttestationID))
	writeActionField(digest, []byte(audit.ExecutorRef))
	writeActionField(digest, audit.KeyDigest[:])
	return digest.Sum(nil)
}

func attestationSessionID(audit fleet.AttestationAudit) shoal.ID {
	digest := sha256.New()
	writeActionField(digest, []byte("shoal.fleet.attestation-audit.v1"))
	writeActionField(digest, []byte(audit.Phase))
	writeActionField(digest, []byte(audit.Reason))
	writeActionField(digest, audit.Principal.Domain)
	writeActionField(digest, []byte(audit.Principal.Subject))
	writeActionField(digest, []byte(audit.Principal.ClientID))
	writeActionField(digest, []byte(audit.ExecutorRef))
	writeActionField(digest, audit.KeyDigest[:])
	writeActionField(digest, []byte(audit.AttestationID))
	writeActionField(digest, []byte(audit.RequestID))
	writeActionField(digest, []byte(strconv.FormatInt(audit.ExpiresAt.UnixNano(), 10)))
	return interaction.DerivedID("session", hex.EncodeToString(digest.Sum(nil)))
}

var _ fleet.AttestationRecorder = (*AttestationRecorder)(nil)
