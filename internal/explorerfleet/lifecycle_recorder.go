// Licensed to the Apache Software Foundation (ASF) under one or more
// contributor license agreements. See the NOTICE file distributed with this
// work for additional information regarding copyright ownership.
package explorerfleet

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"hash"
	"reflect"
	"strconv"

	"github.com/phrocker/shoal-oss/pkg/explorer"
	"github.com/phrocker/shoal-oss/pkg/explorer/auth"
	"github.com/phrocker/shoal-oss/pkg/explorer/fleet"
	"github.com/phrocker/shoal-oss/pkg/interaction"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

// LifecycleInteractionReader reads authoritative committed lifecycle receipts
// from the same corpus used by the result sink.
type LifecycleInteractionReader interface {
	InteractionRecord(
		context.Context, shoal.ID,
	) (explorer.InteractionRecord, error)
}

// LifecycleRecorder converts fleet lifecycle admissions into durable,
// authorization-pinned interaction receipts.
type LifecycleRecorder struct {
	record func(
		context.Context, interaction.Session,
	) (interaction.Session, error)
	read func(
		context.Context, shoal.ID,
	) (explorer.InteractionRecord, error)
}

// NewLifecycleRecorder constructs the production lifecycle receipt adapter.
func NewLifecycleRecorder(
	recorder interaction.ResultSink,
) (*LifecycleRecorder, error) {
	if isNilLifecycleInteractionRecorder(recorder) {
		return nil, shoal.NewError(
			shoal.ErrorInvalidArgument,
			"fleet lifecycle interaction recorder is required",
		)
	}
	return &LifecycleRecorder{
		record: recorder.RecordInteractionResult,
	}, nil
}

// NewLifecycleRecorderWithReader constructs the production adapter with an
// authoritative same-corpus reader for refreshed retry reconciliation.
func NewLifecycleRecorderWithReader(
	recorder interaction.ResultSink,
	reader LifecycleInteractionReader,
) (*LifecycleRecorder, error) {
	if isNilLifecycleInteractionRecorder(recorder) ||
		isNilLifecycleInteractionRecorder(reader) {
		return nil, shoal.NewError(
			shoal.ErrorInvalidArgument,
			"fleet lifecycle interaction recorder and reader are required",
		)
	}
	return &LifecycleRecorder{
		record: recorder.RecordInteractionResult,
		read:   reader.InteractionRecord,
	}, nil
}

// RecordLifecycle records one stable pre-admission receipt. Actor, delegation,
// and the trusted Reason (the decision's audit purpose) are deliberately
// omitted from the request and accepted only from the trusted recorder result.
//
// The caller's reason code and detail are recorded separately, as the
// session's CallerAssertedReason: what the authenticated actor asserted, never
// verified, never used for authorization, and never in place of the audit
// purpose. It is bound into the receipt's query digest, so a retry asserting a
// different reason conflicts instead of reconciling. A malformed assertion,
// such as an "atpl-apply" detail that is not a policy digest, is refused
// before anything is read or written.
func (r *LifecycleRecorder) RecordLifecycle(
	ctx context.Context,
	lifecycle fleet.Lifecycle,
) error {
	if r == nil || r.record == nil {
		return shoal.NewError(
			shoal.ErrorInvalidArgument,
			"fleet lifecycle recorder is required",
		)
	}
	if err := lifecycle.Operation.Validate(); err != nil {
		return err
	}
	asserted, err := fleet.CallerAssertedRegistryReason(
		lifecycle.ReasonCode, lifecycle.ReasonDetail)
	if err != nil {
		return err
	}
	requested := lifecycleSession(lifecycle, asserted)
	if r.read != nil {
		// Receipts written before this version live under the v1, v2 and v3
		// identities. Reconcile a retry against one of those before writing a
		// v4 receipt.
		//
		// Every one of them carries the v1 registry mutation digest, so the
		// retry is compared as that version: the version is read from the
		// receipt's identity, never guessed from the stored digest. v1 and v2
		// receipts also never carry a caller-asserted reason; v3 receipts do.
		//
		// A match is not the end of it. The v1 digest is not injective (#521):
		// two different descriptors can share one, so a retry that matches a
		// v1-era receipt may be a different mutation from the one that receipt
		// admitted. If that receipt's own store write never landed, the retry
		// is what gets applied. So a reconciled retry also writes the v4
		// receipt, with the v2 digest of the mutation actually being applied,
		// and falls through to the ordinary path below: the applied mutation
		// is then recorded unambiguously, a repeat of the same retry
		// reconciles with that v4 receipt, and any other mutation under the
		// same request conflicts with it.
		legacy := lifecycleWithV1RegistryDigest(lifecycle)
		for _, prior := range []struct {
			id                shoal.ID
			withoutAssertions bool
		}{
			{v1LifecycleSessionID(legacy), true},
			{v2LifecycleSessionID(legacy), true},
			{v3LifecycleSessionID(legacy), false},
		} {
			priorRequested := lifecycleSession(legacy, asserted)
			priorRequested.ID = prior.id
			record, readErr := r.read(
				context.WithoutCancel(ctx), priorRequested.ID,
			)
			if readErr == nil {
				if err := validateLifecycleReplay(
					record.Session, priorRequested, legacy,
					prior.withoutAssertions,
				); err != nil {
					return err
				}
				break
			}
		}
	}
	persisted, recordErr := r.record(ctx, requested)
	if recordErr != nil {
		if r.read != nil {
			record, readErr := r.read(
				context.WithoutCancel(ctx), requested.ID,
			)
			if readErr == nil {
				reconcileErr := validateLifecycleReplay(
					record.Session, requested, lifecycle, false,
				)
				if reconcileErr == nil {
					if interaction.IsCommittedRecord(recordErr) {
						return recordErr
					}
					return nil
				}
				if interaction.IsCommittedRecord(recordErr) {
					return committedLifecycleError(
						recordErr, reconcileErr,
					)
				}
			}
		}
		if !interaction.IsCommittedRecord(recordErr) || persisted.ID == "" {
			return recordErr
		}
	}
	expected := requested
	expected.RecordedAt = persisted.RecordedAt
	expected.Actor = interaction.ActorContext{
		SubjectID: lifecycle.Subject,
		ActorID:   lifecycle.Actor,
		ClientID:  lifecycle.ClientID,
		OnBehalfOf: append(
			[]shoal.ID(nil), lifecycle.OnBehalfOf...),
	}
	// Correlation is the trusted sink's, stamped from the recording decision
	// (#532). It is metadata: accepted, never compared.
	expected.CorrelationID = persisted.CorrelationID

	if lifecycle.AuditPurpose != "" {
		var err error
		expected.Reason, err = interaction.NewReason(
			"audit_purpose", lifecycle.AuditPurpose)
		if err != nil {
			return committedLifecycleError(recordErr, err)
		}
	}
	if persisted.RecordedAt.Before(lifecycle.SnapshotAsOf) ||
		!persisted.RecordedAt.Before(lifecycle.AuthorizationExpiresAt) {
		return committedLifecycleError(recordErr, shoal.NewError(
			shoal.ErrorInternal,
			"fleet lifecycle recorder returned an invalid trusted chronology",
		))
	}
	expected, err = expected.Canonical()
	if err != nil {
		return committedLifecycleError(recordErr, err)
	}
	persisted, err = persisted.Canonical()
	if err != nil || !reflect.DeepEqual(persisted, expected) {
		return committedLifecycleError(recordErr, shoal.NewError(
			shoal.ErrorInternal,
			"fleet lifecycle recorder returned a mismatched trusted session",
		))
	}
	return recordErr
}

func validateLifecycleReplay(
	persisted interaction.Session,
	requested interaction.Session,
	lifecycle fleet.Lifecycle,
	legacy bool,
) error {
	canonical, err := persisted.Canonical()
	if err != nil {
		return err
	}
	if canonical.Actor.SubjectID != lifecycle.Subject {
		return shoal.NewError(
			shoal.ErrorUnauthorized,
			"fleet lifecycle receipt belongs to another subject",
		)
	}
	if canonical.RecordedAt.Before(canonical.SnapshotAsOf) ||
		!canonical.RecordedAt.Before(canonical.AuthorizationExpiresAt) {
		return shoal.NewError(
			shoal.ErrorInternal,
			"stored fleet lifecycle receipt has invalid chronology",
		)
	}
	expected := requested
	if legacy {
		// v1 and v2 receipts were written before caller-asserted reasons were
		// recorded, so they carry neither the field nor its query-digest
		// binding. That shape still reconciles: the retry gains no authority
		// from it, and the assertion simply was not kept. Tolerance is keyed on
		// the receipt's identity version, not on the field being empty, so a
		// v3 or v4 receipt stripped of its asserted reason is a conflict, and a
		// legacy-ID receipt that does carry one is too.
		expected.CallerAssertedReason = interaction.CallerAssertedReason{}
		expected.QueryDigest = lifecycleQueryDigest(
			lifecycle, interaction.CallerAssertedReason{})
	}
	expected.RecordedAt = canonical.RecordedAt
	expected.Actor = canonical.Actor
	expected.Reason = canonical.Reason
	// A receipt recorded before #532, or under another request's
	// correlation, is still this receipt.
	expected.CorrelationID = canonical.CorrelationID
	expected.SnapshotID = canonical.SnapshotID
	expected.SnapshotAsOf = canonical.SnapshotAsOf
	expected.AuthorizationFingerprint = canonical.AuthorizationFingerprint
	expected.AuthorizationExpiresAt = canonical.AuthorizationExpiresAt
	expected, err = expected.Canonical()
	if err != nil {
		return err
	}
	if !reflect.DeepEqual(canonical, expected) {
		return shoal.NewError(
			shoal.ErrorConflict,
			"stored fleet lifecycle receipt has different mutation identity",
		)
	}
	return nil
}

func lifecycleSession(
	lifecycle fleet.Lifecycle,
	asserted interaction.CallerAssertedReason,
) interaction.Session {
	operation := string(lifecycle.Operation)
	return interaction.Session{
		ID:                       lifecycleSessionID(lifecycle),
		Operation:                interaction.OperationToolCall,
		CallerAssertedReason:     asserted,
		SnapshotID:               lifecycle.SnapshotID,
		SnapshotAsOf:             lifecycle.SnapshotAsOf.UTC(),
		AuthorizationFingerprint: shoal.ID(lifecycle.AuthorizationFingerprint.String()),
		AuthorizationExpiresAt:   lifecycle.AuthorizationExpiresAt.UTC(),
		AuthorizationOperation:   operation,
		QueryDigest:              lifecycleQueryDigest(lifecycle, asserted),
		RequestID:                lifecycle.RequestID,
		ResultID:                 lifecycle.AgentID,
		StopReason:               "pre_admission",
		Turns: []interaction.Turn{{
			Index:    0,
			Decision: "admitted:" + operation,
			ToolCall: &interaction.ToolCall{
				Kind: "fleet.registry." + operation,
			},
		}},
	}
}

// lifecycleQueryDigest binds the admitted mutation and the caller-asserted
// reason. It is an unkeyed SHA-256: it makes a retry that asserts something
// different detectable as a conflict, not storage tampering by a writer.
//
// Without an asserted reason it is exactly the v1/v2 receipt digest, so those
// receipts are reproduced bit for bit. With one, a separately namespaced
// encoding length-prefixes every part, so moving a boundary between parts
// (Code, DetailDigest, Source) always changes the digest.
func lifecycleQueryDigest(
	lifecycle fleet.Lifecycle,
	asserted interaction.CallerAssertedReason,
) string {
	if asserted.IsZero() {
		return interaction.Digest(
			string(lifecycle.Operation) + "\x00" + string(lifecycle.AgentID) +
				"\x00" + hex.EncodeToString(lifecycle.MutationDigest[:]),
		)
	}
	digest := sha256.New()
	for _, part := range [][]byte{
		[]byte("shoal.fleet.lifecycle.query.v3"),
		[]byte(lifecycle.Operation),
		[]byte(lifecycle.AgentID),
		lifecycle.MutationDigest[:],
		[]byte(asserted.Code),
		[]byte(asserted.DetailDigest),
		[]byte(asserted.Source),
	} {
		writeLifecycleField(digest, part)
	}
	return hex.EncodeToString(digest.Sum(nil))
}

func lifecycleSessionID(lifecycle fleet.Lifecycle) shoal.ID {
	return LifecycleReceiptID(
		lifecycle.Operation, lifecycle.RequestID, lifecycle.AgentID)
}

// v2LifecycleSessionID is the identity receipts had before they recorded a
// caller-asserted reason. It is read, never written.
func v2LifecycleSessionID(lifecycle fleet.Lifecycle) shoal.ID {
	return interaction.DerivedID(
		"session",
		"fleet.lifecycle.v2",
		string(lifecycle.Operation),
		string(lifecycle.RequestID),
		string(lifecycle.AgentID),
	)
}

// v3LifecycleSessionID is the identity receipts had before the registry
// mutation digest was versioned (#521). A receipt under it carries a v1
// registry mutation digest. It is read, never written.
func v3LifecycleSessionID(lifecycle fleet.Lifecycle) shoal.ID {
	return interaction.DerivedID(
		"session",
		"fleet.lifecycle.v3",
		string(lifecycle.Operation),
		string(lifecycle.RequestID),
		string(lifecycle.AgentID),
	)
}

// lifecycleWithV1RegistryDigest returns the lifecycle as a build before #521
// would have recorded it: with the v1 registry mutation digest. Only receipts
// under a v1, v2 or v3 identity are compared against it.
func lifecycleWithV1RegistryDigest(lifecycle fleet.Lifecycle) fleet.Lifecycle {
	lifecycle.MutationDigest = lifecycle.LegacyMutationDigest
	return lifecycle
}

// LifecycleReceiptID is the durable interaction session ID of the lifecycle
// receipt for one registry operation, by request and agent. Reading it with
// the corpus's InteractionRecord returns the receipt, including its
// CallerAssertedReason (for an "atpl-apply" registration, the policy digest as
// Source) and the trusted Actor who asserted it.
//
// Receipts under this identity, fleet.lifecycle.v4, carry the v2 registry
// mutation digest. Receipts written before it have v1, v2 or v3 identities
// and carry the v1 digest; v1 and v2 receipts also predate caller-asserted
// reasons. The identity, not the stored digest, says which version a receipt
// holds.
func LifecycleReceiptID(
	operation auth.Operation, requestID, agentID shoal.ID,
) shoal.ID {
	return interaction.DerivedID(
		"session",
		"fleet.lifecycle.v4",
		string(operation),
		string(requestID),
		string(agentID),
	)
}

func v1LifecycleSessionID(lifecycle fleet.Lifecycle) shoal.ID {
	digest := sha256.New()
	writeLifecycleField(digest, []byte("shoal.fleet.lifecycle.v1"))
	writeLifecycleField(digest, []byte(lifecycle.Operation))
	writeLifecycleField(digest, []byte(lifecycle.RequestID))
	writeLifecycleField(digest, []byte(lifecycle.CorrelationID))
	writeLifecycleField(digest, []byte(lifecycle.Subject))
	writeLifecycleField(digest, []byte(lifecycle.Actor))
	writeLifecycleField(digest, []byte(lifecycle.ClientID))
	for _, id := range lifecycle.OnBehalfOf {
		writeLifecycleField(digest, []byte(id))
	}
	writeLifecycleField(digest, []byte(lifecycle.AgentID))
	writeLifecycleField(digest, lifecycle.MutationDigest[:])
	writeLifecycleField(
		digest, []byte(strconv.FormatInt(lifecycle.Deadline, 10)))
	writeLifecycleField(
		digest, []byte(lifecycle.AuthorizationFingerprint.String()))
	writeLifecycleField(
		digest,
		[]byte(lifecycle.AuthorizationExpiresAt.UTC().Format(
			"2006-01-02T15:04:05.999999999Z07:00")),
	)
	writeLifecycleField(digest, []byte(lifecycle.AuditPurpose))
	writeLifecycleField(digest, []byte(lifecycle.SnapshotID))
	writeLifecycleField(
		digest,
		[]byte(lifecycle.SnapshotAsOf.UTC().Format(
			"2006-01-02T15:04:05.999999999Z07:00")),
	)
	return interaction.DerivedID(
		"session", hex.EncodeToString(digest.Sum(nil)))
}

func writeLifecycleField(digest hash.Hash, value []byte) {
	var length [8]byte
	binary.BigEndian.PutUint64(length[:], uint64(len(value)))
	_, _ = digest.Write(length[:])
	_, _ = digest.Write(value)
}

func committedLifecycleError(recordErr, validationErr error) error {
	if recordErr != nil {
		return explorer.MarkCommittedInteraction(errors.Join(
			recordErr, validationErr))
	}
	return explorer.MarkCommittedInteraction(validationErr)
}

func isNilLifecycleInteractionRecorder(
	recorder any,
) bool {
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

var _ fleet.LifecycleRecorder = (*LifecycleRecorder)(nil)
