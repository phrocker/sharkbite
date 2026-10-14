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
	"encoding/binary"
	"encoding/hex"
	"errors"
	"reflect"
	"sort"
	"time"

	"github.com/phrocker/shoal-oss/internal/explorerfleetcap"
	"github.com/phrocker/shoal-oss/pkg/explorer"
	"github.com/phrocker/shoal-oss/pkg/explorer/auth"
	"github.com/phrocker/shoal-oss/pkg/interaction"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

// InteractionAuditor records privileged fleet actions through the existing
// durable interaction recorder without persisting event payloads or raw
// opaque object identities.
type InteractionAuditor struct {
	recorder  *interaction.Recorder
	result    interaction.ResultSink
	resultFor func(auth.Operation) interaction.ResultSink
	reader    InteractionReceiptReader
	snapshots InteractionSnapshotProvider
}

type InteractionReceiptReader interface {
	InteractionRecord(context.Context, shoal.ID) (explorer.InteractionRecord, error)
}

type InteractionSnapshotProvider interface {
	InteractionSnapshot(context.Context) (explorer.Snapshot, error)
}

type reconciliationResultSink interface {
	RecordReconciledInteractionResult(
		context.Context, explorerfleetcap.Capability, interaction.Session,
	) (interaction.Session, error)
}

func NewInteractionAuditor(
	recorder *interaction.Recorder, snapshots InteractionSnapshotProvider,
) (*InteractionAuditor, error) {
	if recorder == nil || snapshots == nil {
		return nil, shoal.NewError(
			shoal.ErrorInvalidArgument,
			"interaction recorder and snapshot provider are required")
	}
	return &InteractionAuditor{recorder: recorder, snapshots: snapshots}, nil
}

func NewInteractionAuditorWithReader(
	result interaction.ResultSink,
	reader InteractionReceiptReader,
	snapshots InteractionSnapshotProvider,
) (*InteractionAuditor, error) {
	if result == nil || reader == nil || snapshots == nil {
		return nil, shoal.NewError(
			shoal.ErrorInvalidArgument,
			"interaction result sink, reader, and snapshot provider are required")
	}
	return &InteractionAuditor{
		result: result, reader: reader, snapshots: snapshots,
	}, nil
}

func NewOperationInteractionAuditorWithReader(
	resultFor func(auth.Operation) interaction.ResultSink,
	reader InteractionReceiptReader,
	snapshots InteractionSnapshotProvider,
) (*InteractionAuditor, error) {
	if resultFor == nil || reader == nil || snapshots == nil {
		return nil, shoal.NewError(
			shoal.ErrorInvalidArgument,
			"operation interaction sink, reader, and snapshot provider are required")
	}
	return &InteractionAuditor{
		resultFor: resultFor, reader: reader, snapshots: snapshots,
	}, nil
}

func (a *InteractionAuditor) RecordFleetAction(ctx context.Context, record AuditRecord) error {
	session, err := a.fleetActionSession(ctx, record)
	if err != nil {
		return err
	}
	var persisted interaction.Session
	result := a.result
	if a.resultFor != nil {
		result = a.resultFor(record.Operation)
		if result == nil {
			return shoal.NewError(
				shoal.ErrorUnavailable,
				"operation-bound interaction sink is unavailable")
		}
	}
	if result != nil {
		persisted, err = result.RecordInteractionResult(ctx, session)
	} else {
		persisted, err = a.recorder.Record(ctx, session)
	}
	return validateFleetReceipt(session, persisted, err)
}

func (a *InteractionAuditor) RecordFleetActionRetry(
	ctx context.Context, record AuditRecord,
) error {
	sessionID, err := fleetActionSessionID(record)
	if err != nil {
		return err
	}
	if a.reader != nil {
		stored, readErr := a.reader.InteractionRecord(
			context.WithoutCancel(ctx), sessionID)
		if readErr != nil {
			return readErr
		}
		if stored.Summary.Deleted {
			return explorer.MarkCommittedInteraction(shoal.NewError(
				shoal.ErrorNotFound, "fleet interaction receipt was deleted"))
		}
		requested, buildErr := a.fleetActionSession(ctx, record)
		if buildErr != nil {
			return buildErr
		}
		if sameFleetReceipt(requested, stored.Session) {
			return nil
		}
		return explorer.MarkCommittedInteraction(shoal.NewError(
			shoal.ErrorInternal,
			"persisted fleet interaction receipt does not match request"))
	}
	return a.RecordFleetAction(ctx, record)
}

func (a *InteractionAuditor) RecordFleetActionReconciliation(
	ctx context.Context, capability explorerfleetcap.Capability,
	record AuditRecord,
) error {
	if !capability.Valid() {
		return shoal.NewError(
			shoal.ErrorUnauthorized,
			"fleet reconciliation capability is invalid")
	}
	session, err := a.fleetActionSession(ctx, record)
	if err != nil {
		return err
	}
	result := a.result
	if a.resultFor != nil {
		result = a.resultFor(record.Operation)
		if result == nil {
			return shoal.NewError(
				shoal.ErrorUnavailable,
				"operation-bound interaction sink is unavailable")
		}
	}
	reconciler, ok := result.(reconciliationResultSink)
	if !ok {
		return shoal.NewError(
			shoal.ErrorUnavailable,
			"trusted interaction reconciliation sink is unavailable")
	}
	persisted, err := reconciler.RecordReconciledInteractionResult(
		ctx, capability, session)
	return validateFleetReceipt(session, persisted, err)
}

func (a *InteractionAuditor) fleetActionSession(
	ctx context.Context, record AuditRecord,
) (interaction.Session, error) {
	sessionID, err := fleetActionSessionID(record)
	if err != nil {
		return interaction.Session{}, err
	}
	snapshot, err := a.snapshots.InteractionSnapshot(ctx)
	if err != nil {
		return interaction.Session{}, err
	}
	return interaction.Session{
		ID: sessionID, RecordedAt: record.OccurredAt, Operation: interaction.OperationToolCall,
		AuthorizationFingerprint: shoal.ID(
			record.AuthorizationFingerprint.String()),
		AuthorizationExpiresAt: record.AuthorizationExpiresAt,
		AuthorizationOperation: string(record.Operation),
		SnapshotID:             shoal.ID(snapshot.ID), SnapshotAsOf: snapshot.AsOf,
		RequestID:     record.RequestID,
		QueryDigest:   interaction.Digest(hex.EncodeToString(auditDigest(record))),
		CitedNodeIDs:  evidenceNodeIDs(record.CitedEvidence),
		CitedEvidence: cloneEvidenceReferences(record.CitedEvidence),
		Turns: []interaction.Turn{{
			Index: 0, Decision: string(record.Operation),
			ToolCall: &interaction.ToolCall{
				Kind:              "fleet." + string(record.Operation),
				RetrievedNodeIDs:  evidenceNodeIDs(record.ConsumedEvidence),
				RetrievedEvidence: cloneEvidenceReferences(record.ConsumedEvidence),
			},
		}},
	}, nil
}

func fleetActionSessionID(record AuditRecord) (shoal.ID, error) {
	correlationID := shoal.ID(hex.EncodeToString(deriveID(
		"fleet-action-session-v1", []byte(record.Operation), record.ActionID,
		record.ObjectID,
	)))
	return interaction.OperationSessionID(
		interaction.OperationToolCall, correlationID, record.OccurredAt)
}

func validateFleetReceipt(
	session, persisted interaction.Session, err error,
) error {
	if err != nil && !interaction.IsCommittedRecord(err) {
		return err
	}
	if !sameFleetReceipt(session, persisted) {
		mismatch := explorer.MarkCommittedInteraction(shoal.NewError(
			shoal.ErrorInternal,
			"persisted fleet interaction receipt does not match request"))
		if err != nil {
			return errors.Join(err, mismatch)
		}
		return mismatch
	}
	if err != nil {
		return err
	}
	return nil
}

func sameFleetReceipt(expected, persisted interaction.Session) bool {
	if persisted.ID != expected.ID ||
		persisted.Operation != interaction.OperationToolCall ||
		persisted.RecordedAt.IsZero() ||
		persisted.RecordedAt.Location() != time.UTC ||
		persisted.Actor.SubjectID == "" ||
		persisted.Actor.ActorID == "" ||
		persisted.Actor.Validate() != nil ||
		persisted.Reason.Validate() != nil {
		return false
	}
	expected.RecordedAt = persisted.RecordedAt
	expected.Actor = persisted.Actor
	expected.Reason = persisted.Reason
	// The trusted sink stamps the recording decision's correlation (#532).
	// It is metadata, so a receipt replayed or reconciled under another
	// correlation is still this receipt.
	expected.CorrelationID = persisted.CorrelationID
	expected.SnapshotID = persisted.SnapshotID
	expected.SnapshotAsOf = persisted.SnapshotAsOf
	canonicalExpected, err := expected.Canonical()
	if err != nil {
		return false
	}
	canonicalPersisted, err := persisted.Canonical()
	if err != nil {
		return false
	}
	return reflect.DeepEqual(canonicalExpected, canonicalPersisted)
}

func auditDigest(record AuditRecord) []byte {
	parts := [][]byte{
		[]byte(record.Operation), record.ActionID, []byte(record.RequestID),
		record.CorrelationID, record.ObjectID, encodedUint64(uint64(len(record.Evidence))),
	}
	for _, evidence := range record.Evidence {
		parts = append(parts,
			evidence.SourceID, evidence.PolicyID, []byte(evidence.ObjectID),
		)
	}
	parts = append(parts, encodedUint64(uint64(len(record.ConsumedEvidence))))
	parts = appendEvidenceDigestParts(parts, record.ConsumedEvidence)
	parts = append(parts, encodedUint64(uint64(len(record.CitedEvidence))))
	parts = appendEvidenceDigestParts(parts, record.CitedEvidence)
	return deriveID("fleet-action-audit-v3", parts...)
}

func evidenceNodeIDs(references []interaction.EvidenceReference) []shoal.ID {
	seen := make(map[shoal.ID]struct{})
	for _, reference := range references {
		for _, id := range reference.NodeIDs {
			seen[id] = struct{}{}
		}
	}
	result := make([]shoal.ID, 0, len(seen))
	for id := range seen {
		result = append(result, id)
	}
	sort.Slice(result, func(i, j int) bool {
		return shoal.CompareID(result[i], result[j]) < 0
	})
	return result
}

func appendEvidenceDigestParts(
	parts [][]byte, references []interaction.EvidenceReference,
) [][]byte {
	for _, reference := range references {
		parts = append(parts,
			[]byte(reference.AnchorID), []byte(reference.Kind),
			[]byte(reference.Citation.DocumentID),
			[]byte(reference.Citation.RevisionID),
			[]byte(reference.Citation.SectionID),
			[]byte(reference.Citation.SpanID),
			encodedUint64(uint64(reference.Citation.Range.Start.Offset)),
			encodedUint64(uint64(reference.Citation.Range.Start.Page)),
			encodedUint64(uint64(reference.Citation.Range.End.Offset)),
			encodedUint64(uint64(reference.Citation.Range.End.Page)),
			encodedUint64(uint64(len(reference.NodeIDs))),
		)
		for _, id := range reference.NodeIDs {
			parts = append(parts, []byte(id))
		}
		parts = append(parts, encodedUint64(uint64(len(reference.EdgeIDs))))
		for _, id := range reference.EdgeIDs {
			parts = append(parts, []byte(id))
		}
		parts = append(parts, encodedUint64(uint64(len(reference.Assertions))))
		for _, assertion := range reference.Assertions {
			parts = append(parts, []byte(assertion.AssertionID),
				[]byte(assertion.EdgeID), []byte(assertion.Origin))
		}
	}
	return parts
}

func encodedUint64(value uint64) []byte {
	encoded := make([]byte, 8)
	binary.BigEndian.PutUint64(encoded, value)
	return encoded
}
