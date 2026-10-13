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

package authorized

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"reflect"
	"strings"
	"time"

	"github.com/phrocker/shoal-oss/internal/explorerfleetcap"
	"github.com/phrocker/shoal-oss/pkg/explorer"
	"github.com/phrocker/shoal-oss/pkg/explorer/auth"
	"github.com/phrocker/shoal-oss/pkg/interaction"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

// EnsureInteractionSink verifies the caller's credential and the configured
// durable write path. Setup is not pinned to one operation: authorization for
// the recorded work is operation-specific and is enforced by
// RecordInteractionResult once the Session declares AuthorizationOperation,
// and requiring Retrieve here would incorrectly reject evidence-empty
// privileged action recorders. It still requires a live credential that grants
// something, because the base sink probe is a durable write.
func (c *Client) EnsureInteractionSink(ctx context.Context) error {
	guard, err := c.beginAny(ctx)
	if err != nil {
		return err
	}
	writer, err := c.interactionWriter()
	if err != nil {
		return err
	}
	if _, ok := writer.(interaction.ResultSink); !ok {
		return shoal.NewError(
			shoal.ErrorUnavailable,
			"trusted interaction result sink is unavailable",
		)
	}
	if err := writer.EnsureInteractionSink(ctx); err != nil {
		return directBaseError(err)
	}
	return guard.Check(ctx)
}

type operationInteractionSink struct {
	client            *Client
	operation         auth.Operation
	evidenceOperation auth.Operation
}

// AnalyticsInteractionSink returns the durable interaction sink bound to the
// analytics authorization operation.
func (c *Client) AnalyticsInteractionSink() interaction.ResultSink {
	if c == nil {
		return nil
	}
	writer, err := c.interactionWriter()
	if err != nil {
		return nil
	}
	if _, ok := writer.(interaction.ResultSink); !ok {
		return nil
	}
	if isNilDependency(c.snapshotValidator) {
		return nil
	}
	if _, ok := c.snapshotValidator.(EvidenceSnapshotValidator); !ok {
		return nil
	}
	return operationInteractionSink{
		client: c, operation: auth.OperationAnalyticsRead,
		evidenceOperation: auth.OperationRetrieve,
	}
}

// FleetActionInteractionSink returns a durable interaction sink whose session
// and exact evidence are both authorized with the supplied fleet action
// operation. Dispatch lifecycle evidence has already been admitted under that
// operation and must not require the unrelated retrieve permission.
//
// OperationExecute is one of those operations (#437, #480). Omitting it did
// not merely skip an audit: main.go falls back to the bare client when this
// returns nil, the bare client has no reconciliation sink, and the auditor
// answers "trusted interaction reconciliation sink is unavailable" — which
// classifyAuditError turns into ErrAuditOutcomeUnknown and the dispatch
// service joins with ErrActionCommitted. Every claim taken under execute
// committed and then came back 503 "fleet action outcome requires
// reconciliation", so the capability did not work end to end.
// fleetActionLifecycleOperation is the set of operations a dispatch lifecycle
// audit can be admitted under. Shared by the sink and its reconciliation
// method because they were two separate literal comparisons of the same set,
// and execute was missing from both — a surface that admits an operation it
// cannot then reconcile is the 503 above with an extra step.
//
// Approve is the fourth (#653): an approver's decision is published as an
// approval lifecycle event under that approver's own decision, and its
// publication audit needs the same reconciling sink. Without it the
// approver's decision committed and its event then failed "trusted
// interaction reconciliation sink is unavailable", the shape above.
//
// Approval sweep is the fifth (#655): the host's system observer publishes
// the expiries and strandings it records under its own decision, and its
// publication audit needs the same reconciling sink for the same reason.
func fleetActionLifecycleOperation(operation auth.Operation) bool {
	switch operation {
	case auth.OperationDispatch, auth.OperationInvoke, auth.OperationExecute,
		auth.OperationActionApprove, auth.OperationApprovalSweep:
		return true
	default:
		return false
	}
}

func (c *Client) FleetActionInteractionSink(
	operation auth.Operation,
) interaction.ResultSink {
	if c == nil || !fleetActionLifecycleOperation(operation) {
		return nil
	}
	writer, err := c.interactionWriter()
	if err != nil {
		return nil
	}
	if _, ok := writer.(interaction.ResultSink); !ok {
		return nil
	}
	if isNilDependency(c.snapshotValidator) {
		return nil
	}
	if _, ok := c.snapshotValidator.(EvidenceSnapshotValidator); !ok {
		return nil
	}
	return operationInteractionSink{
		client: c, operation: operation, evidenceOperation: operation,
	}
}

func (s operationInteractionSink) EnsureInteractionSink(
	ctx context.Context,
) error {
	if s.client == nil {
		return shoal.NewError(
			shoal.ErrorUnavailable, "authorized interaction sink is unavailable")
	}
	writer, err := s.client.interactionWriter()
	if err != nil {
		return err
	}
	if _, ok := writer.(interaction.ResultSink); !ok {
		return shoal.NewError(
			shoal.ErrorUnavailable,
			"trusted interaction result sink is unavailable",
		)
	}
	if err := writer.EnsureInteractionSink(ctx); err != nil {
		return directBaseError(err)
	}
	return nil
}

func (s operationInteractionSink) RecordInteraction(
	ctx context.Context,
	session interaction.Session,
) error {
	_, err := s.RecordInteractionResult(ctx, session)
	return err
}

func (s operationInteractionSink) RecordInteractionResult(
	ctx context.Context,
	session interaction.Session,
) (interaction.Session, error) {
	if s.client == nil {
		return interaction.Session{}, shoal.NewError(
			shoal.ErrorUnavailable, "authorized interaction sink is unavailable")
	}
	session.AuthorizationOperation = string(s.operation)
	return s.client.recordInteractionWithEvidenceOperation(
		ctx, session, true, s.evidenceOperation, false)
}

// RecordReconciledInteractionResult records a durable fleet transition after
// fresh operation and evidence authorization without replacing its original
// authorization receipt.
func (s operationInteractionSink) RecordReconciledInteractionResult(
	ctx context.Context,
	capability explorerfleetcap.Capability,
	session interaction.Session,
) (interaction.Session, error) {
	if s.client == nil || !capability.Valid() ||
		!fleetActionLifecycleOperation(s.operation) {
		return interaction.Session{}, shoal.NewError(
			shoal.ErrorUnavailable,
			"authorized fleet reconciliation sink is unavailable")
	}
	session.AuthorizationOperation = string(s.operation)
	return s.client.recordInteractionWithEvidenceOperation(
		ctx, session, true, s.evidenceOperation, true)
}

// RecordInteraction appends one redacted interaction after verifying that its
// pinned authorization is the exact current decision and that every source
// node it retrieved or cited is still authorized. A revoked or missing source
// fails the inference instead of creating an under-authorized record.
func (c *Client) RecordInteraction(
	ctx context.Context, session interaction.Session,
) error {
	_, err := c.recordInteraction(ctx, session, false)
	return err
}

// RecordInteractionResult records one interaction and returns the exact
// trusted session accepted for persistence, including actor, delegation, and
// derived reason metadata supplied by the bound authorization Decision.
func (c *Client) RecordInteractionResult(
	ctx context.Context, session interaction.Session,
) (interaction.Session, error) {
	return c.recordInteraction(ctx, session, true)
}

func (c *Client) recordInteraction(
	ctx context.Context, session interaction.Session, requireResult bool,
) (interaction.Session, error) {
	return c.recordInteractionWithEvidenceOperation(
		ctx, session, requireResult, auth.OperationRetrieve, false)
}

func (c *Client) recordInteractionWithEvidenceOperation(
	ctx context.Context, session interaction.Session, requireResult bool,
	evidenceOperation auth.Operation, durablePin bool,
) (interaction.Session, error) {
	writer, err := c.interactionWriter()
	if err != nil {
		return interaction.Session{}, err
	}
	resultWriter, hasResultWriter := writer.(interaction.ResultSink)
	if requireResult && !hasResultWriter {
		return interaction.Session{}, shoal.NewError(
			shoal.ErrorUnavailable,
			"trusted interaction result sink is unavailable",
		)
	}
	authorizationOperation := auth.OperationRetrieve
	if session.AuthorizationOperation != "" {
		authorizationOperation = auth.Operation(session.AuthorizationOperation)
	}
	decision, guard, now, err := c.begin(ctx, authorizationOperation)
	if err != nil {
		return interaction.Session{}, err
	}
	recordedAt := now.UTC()
	if durablePin {
		if !durableInteractionPinValid(session) {
			return interaction.Session{}, shoal.NewError(
				shoal.ErrorUnauthorized,
				"durable interaction authorization receipt is invalid")
		}
		recordedAt = session.RecordedAt
	}
	session.RecordedAt = recordedAt
	// Correlation is stamped from the decision below, as Actor is. A
	// producer's value is never trusted, and is cleared before it can fail
	// validation: correlation must never be the reason an audit fails.
	session.CorrelationID = ""
	if !durablePin && !interactionPinMatchesDecision(session, decision, now) {
		return interaction.Session{}, authorizationDenied()
	}
	canonical, err := session.Canonical()
	if err != nil {
		return interaction.Session{}, err
	}
	canonical.Actor = interaction.ActorContext{
		SubjectID:  decision.Subject(),
		ActorID:    decision.Actor(),
		ClientID:   decision.ClientID(),
		OnBehalfOf: decision.OnBehalfOf(),
	}
	// The decision's correlation (#532): metadata only, never identity. A
	// value the session boundary would refuse is dropped, not refused, so
	// the audited operation is never failed by its grouping label.
	canonical.CorrelationID = interaction.RecordableCorrelationID(
		decision.CorrelationID())
	canonical.Reason = interaction.Reason{}
	if decision.AuditPurpose() != "" {
		canonical.Reason, err = interaction.NewReason(
			"audit_purpose", decision.AuditPurpose())
		if err != nil {
			return interaction.Session{}, authorizationDenied()
		}
	}
	canonical, err = canonical.Canonical()
	if err != nil {
		return interaction.Session{}, err
	}
	// The revisions the session was derived from (#695), read before its
	// sources are authorized and again once its snapshot is validated, and
	// recorded as the union: whichever revision the catalog named at either
	// point is bound. (The corpus also refuses the write if a source changed
	// after validation; TestRelabelRacingARecordingNeverOpensTheSession.)
	evidence, err := canonical.EvidenceReferences()
	if err != nil {
		return interaction.Session{}, err
	}
	sourceRevisions, err := c.interactionSourceRevisions(
		ctx, canonical.TouchedNodeIDs(), canonical.TouchedEdgeIDs(), evidence)
	if err != nil {
		return interaction.Session{}, err
	}
	if err := c.authorizeInteractionEvidence(
		ctx,
		canonical.TouchedNodeIDs(),
		canonical.TouchedEdgeIDs(),
		decision,
		evidenceOperation,
		now,
	); err != nil {
		return interaction.Session{}, err
	}
	// The stored output restriction is what reads evaluate (#564, #567), so
	// it must hold terms a reader can hold. A producer derives it from the
	// shoal.visibility of the nodes it touched, which is free-form ("secret")
	// and held by nobody; each such term is replaced by the grant labels of
	// the label policy enforcing it on those nodes (#570). Translated before
	// the retry comparison, so an exact retry compares like with like.
	canonical.RequiredVisibility, err = c.structuredLabels(
		ctx, canonical.RequiredVisibility,
		canonical.TouchedNodeIDs(), canonical.TouchedEdgeIDs())
	if err != nil {
		return interaction.Session{}, err
	}
	canonical, err = canonical.Canonical()
	if err != nil {
		return interaction.Session{}, err
	}
	reader, err := c.interactionReader()
	if err != nil {
		return interaction.Session{}, err
	}
	existing, readErr := reader.InteractionRecord(ctx, canonical.ID)
	switch {
	case readErr == nil:
		if existing.Summary.Deleted || existing.Session.ID == "" {
			return interaction.Session{}, shoal.NewError(
				shoal.ErrorConflict,
				"interaction session ID is not available for an exact retry",
			)
		}
		existingCanonical, canonicalErr := existing.Session.Canonical()
		matches := canonicalErr == nil
		if durablePin {
			matches = matches &&
				durableInteractionRetryMatches(canonical, existingCanonical)
		} else {
			retryCanonical := canonical
			retryCanonical.RecordedAt = existingCanonical.RecordedAt
			// A retry under another correlation is the same session; the
			// first recorded correlation stands (#532).
			retryCanonical.CorrelationID = existingCanonical.CorrelationID
			matches = matches &&
				reflect.DeepEqual(existingCanonical, retryCanonical)
		}
		if !matches {
			return interaction.Session{}, shoal.NewError(
				shoal.ErrorConflict,
				"interaction session ID already exists with different content",
			)
		}
		storedVisibility, visibilityErr := interaction.ParseVisibility(
			existing.Summary.Visibility)
		requiredVisibility := interaction.RequiredVisibility(ctx)
		combinedVisibility, combinedErr := interaction.Conjoin(
			storedVisibility, requiredVisibility)
		if visibilityErr != nil || combinedErr != nil ||
			interaction.Expression(combinedVisibility) !=
				interaction.Expression(storedVisibility) {
			return interaction.Session{}, shoal.NewError(
				shoal.ErrorConflict,
				"interaction retry requires stricter output visibility",
			)
		}
		if err := guard.Check(ctx); err != nil {
			return interaction.Session{}, err
		}
		deliveredAt := c.clock().UTC()
		if deliveredAt.IsZero() ||
			(!durablePin && !interactionPinMatchesDecision(
				existingCanonical, decision, deliveredAt)) ||
			(durablePin && !durableInteractionPinValid(existingCanonical)) {
			return interaction.Session{}, authorizationDenied()
		}
		return existingCanonical, nil
	case !shoal.IsErrorCode(readErr, shoal.ErrorNotFound):
		return interaction.Session{}, directBaseError(readErr)
	}
	if isNilDependency(c.snapshotValidator) {
		return interaction.Session{}, shoal.NewError(
			shoal.ErrorUnavailable,
			"trusted interaction snapshot validator is unavailable",
		)
	}
	edgeIDs := canonical.TouchedEdgeIDs()
	references, err := canonical.EvidenceReferences()
	if err != nil {
		return interaction.Session{}, err
	}
	if len(references) > 0 || len(edgeIDs) > 0 {
		validator, ok := c.snapshotValidator.(EvidenceSnapshotValidator)
		if !ok {
			return interaction.Session{}, shoal.NewError(
				shoal.ErrorUnavailable,
				"trusted interaction evidence validator is unavailable",
			)
		}
		err = validator.ValidateEvidenceSnapshot(
			ctx, canonical.SnapshotID, canonical.SnapshotAsOf,
			canonical.TouchedNodeIDs(), edgeIDs,
			references,
		)
	} else {
		err = c.snapshotValidator.ValidateSnapshot(
			ctx, canonical.SnapshotID, canonical.SnapshotAsOf,
			canonical.TouchedNodeIDs(),
		)
	}
	if err != nil {
		return interaction.Session{}, directBaseError(err)
	}
	if err := guard.Check(ctx); err != nil {
		return interaction.Session{}, err
	}
	validatedRevisions, err := c.interactionSourceRevisions(
		ctx, canonical.TouchedNodeIDs(), edgeIDs, references)
	if err != nil {
		return interaction.Session{}, err
	}
	sourceRevisions, err = unionRevisionReferences(
		sourceRevisions, validatedRevisions)
	if err != nil {
		return interaction.Session{}, inconsistentBase()
	}
	if err := c.recordInteractionSourceRevisions(
		ctx, canonical.ID, sourceRevisions); err != nil {
		return interaction.Session{}, err
	}
	admittedAt := c.clock().UTC()
	if admittedAt.IsZero() ||
		(!durablePin &&
			!interactionPinMatchesDecision(canonical, decision, admittedAt)) ||
		(durablePin && !durableInteractionPinValid(canonical)) {
		return interaction.Session{}, shoal.NewError(
			shoal.ErrorUnauthorized,
			"interaction authorization receipt is no longer valid")
	}
	if !durablePin {
		canonical.RecordedAt = admittedAt
	}
	canonical, err = canonical.Canonical()
	if err != nil {
		return interaction.Session{}, err
	}

	persisted := canonical
	if hasResultWriter {
		persisted, err = resultWriter.RecordInteractionResult(ctx, canonical)
	} else {
		err = writer.RecordInteraction(ctx, canonical)
	}
	if err != nil {
		return persisted, directBaseError(err)
	}
	if hasResultWriter {
		returned, canonicalErr := persisted.Canonical()
		expected := canonical
		expected.RecordedAt = returned.RecordedAt
		// A concurrent writer of the same session may have won with its own
		// correlation; that is still this session.
		expected.CorrelationID = returned.CorrelationID
		if canonicalErr != nil || !reflect.DeepEqual(returned, expected) {
			return persisted, explorer.MarkCommittedInteraction(
				shoal.NewError(
					shoal.ErrorInternal,
					"durable interaction sink returned a different record",
				),
			)
		}
		if !returned.RecordedAt.Equal(canonical.RecordedAt) {
			// Only a durable retry winner may replace the admitted timestamp.
			stored, readErr := reader.InteractionRecord(ctx, canonical.ID)
			if readErr != nil {
				return persisted, explorer.MarkCommittedInteraction(
					explorer.MarkIndeterminateCommit(directBaseError(readErr)),
				)
			}
			durable, durableErr := stored.Session.Canonical()
			if stored.Summary.Deleted || durableErr != nil ||
				!reflect.DeepEqual(durable, returned) {
				return persisted, explorer.MarkCommittedInteraction(
					shoal.NewError(
						shoal.ErrorInternal,
						"durable interaction sink returned an unverified timestamp",
					),
				)
			}
		}
		persisted = returned
	}
	if err := guard.Check(ctx); err != nil {
		if authorizationOperation == auth.OperationAnalyticsRead {
			return persisted, explorer.MarkIndeterminateCommit(err)
		}
		return persisted, explorer.MarkCommittedInteraction(err)
	}
	deliveredAt := c.clock().UTC()
	if deliveredAt.IsZero() ||
		(!durablePin &&
			!interactionPinMatchesDecision(persisted, decision, deliveredAt)) ||
		(durablePin && !durableInteractionPinValid(persisted)) {
		if authorizationOperation == auth.OperationAnalyticsRead {
			return persisted, explorer.MarkIndeterminateCommit(
				authorizationDenied())
		}
		return persisted, explorer.MarkCommittedInteraction(
			authorizationDenied())
	}
	return persisted, nil
}

func durableInteractionPinValid(session interaction.Session) bool {
	const prefix = "auth-sha256:"
	fingerprint := string(session.AuthorizationFingerprint)
	if !strings.HasPrefix(fingerprint, prefix) {
		return false
	}
	encoded := strings.TrimPrefix(fingerprint, prefix)
	decoded, err := hex.DecodeString(encoded)
	if err != nil || len(decoded) != sha256.Size {
		return false
	}
	nonzero := false
	for _, value := range decoded {
		nonzero = nonzero || value != 0
	}
	return nonzero &&
		session.RequestID != "" &&
		!session.RecordedAt.IsZero() &&
		session.RecordedAt.Location() == time.UTC &&
		!session.AuthorizationExpiresAt.IsZero() &&
		session.AuthorizationExpiresAt.Location() == time.UTC &&
		session.RecordedAt.Before(session.AuthorizationExpiresAt)
}

func durableInteractionRetryMatches(
	requested, persisted interaction.Session,
) bool {
	if !durableInteractionPinValid(persisted) {
		return false
	}
	requested.RecordedAt = persisted.RecordedAt
	requested.Actor = persisted.Actor
	requested.Reason = persisted.Reason
	requested.CorrelationID = persisted.CorrelationID
	requested.SnapshotID = persisted.SnapshotID
	requested.SnapshotAsOf = persisted.SnapshotAsOf
	canonicalRequested, err := requested.Canonical()
	if err != nil {
		return false
	}
	canonicalPersisted, err := persisted.Canonical()
	return err == nil && reflect.DeepEqual(canonicalRequested, canonicalPersisted)
}

func postCommitInteractionError(
	operation auth.Operation, err error,
) error {
	if operation == auth.OperationAnalyticsRead {
		return explorer.MarkIndeterminateCommit(err)
	}
	return explorer.MarkCommittedInteraction(err)
}

// Interactions lists only derived records whose complete current source set
// the caller may read. Tombstones have intentionally discarded their source
// edges, so they are visible only to the exact authorization projection that
// created the original record.
func (c *Client) Interactions(
	ctx context.Context,
) ([]explorer.InteractionSummary, error) {
	records, err := c.InteractionRecords(ctx)
	if err != nil {
		return nil, err
	}
	summaries := make([]explorer.InteractionSummary, len(records))
	for index, record := range records {
		summaries[index] = record.Summary
	}
	return summaries, nil
}

// InteractionRecords returns authorized interaction summaries and provenance
// in one base read and bounded batched policy lookups.
func (c *Client) InteractionRecords(
	ctx context.Context,
) ([]explorer.InteractionRecord, error) {
	reader, err := c.interactionReader()
	if err != nil {
		return nil, err
	}
	decision, guard, now, err := c.begin(ctx, auth.OperationRead)
	if err != nil {
		return nil, err
	}
	records, err := reader.InteractionRecords(ctx)
	if err != nil {
		return nil, directBaseError(err)
	}
	allowed, err := c.authorizeInteractionRecords(ctx, records, decision, now)
	if err != nil {
		return nil, err
	}
	if err := c.authorizeInteractionRevisions(
		ctx, c.newRevisionGate(decision, auth.OperationRead, now),
		records, allowed,
	); err != nil {
		return nil, err
	}
	visible := make([]explorer.InteractionRecord, 0, len(records))
	for index, record := range records {
		if !allowed[index] {
			continue
		}
		readable, ok, err := c.readableInteraction(ctx, decision, now, record)
		if err != nil {
			return nil, err
		}
		if ok {
			visible = append(visible, readable)
		}
	}
	if err := guard.Check(ctx); err != nil {
		return nil, err
	}
	return visible, nil
}

func (c *Client) InteractionRecordsPage(
	ctx context.Context, after shoal.ID, limit uint32,
) (explorer.InteractionRecordPage, error) {
	if err := shoal.ValidateOptionalID("interaction page cursor", after); err != nil {
		return explorer.InteractionRecordPage{}, err
	}
	if limit == 0 || limit > explorer.MaxInteractionRecordPageSize {
		return explorer.InteractionRecordPage{}, shoal.NewError(
			shoal.ErrorInvalidArgument, "interaction page limit is outside its bound")
	}
	records, err := c.InteractionRecords(ctx)
	if err != nil {
		return explorer.InteractionRecordPage{}, err
	}
	page := explorer.InteractionRecordPage{
		Records: make([]explorer.InteractionRecord, 0, limit),
	}
	for _, record := range records {
		if shoal.CompareID(record.Summary.SessionID, after) <= 0 {
			continue
		}
		if len(page.Records) == int(limit) {
			page.NextAfter = page.Records[len(page.Records)-1].Summary.SessionID
			break
		}
		page.Records = append(page.Records, record)
	}
	return page, nil
}

// maxInteractionAuthorizationIDs bounds how many provenance identifiers one
// policy-store lookup may carry while authorizing a list of interaction
// records. Interaction provenance is intentionally uncapped per record, so
// without this bound a single list call would submit the union of the whole
// durable corpus in one request. Records are grouped into batches under this
// bound instead, and one record whose own provenance exceeds it is authorized
// in bounded chunks of its own.
const maxInteractionAuthorizationIDs = 1024

// authorizeInteractionRecords decides visibility for every record in list
// order without ever holding more than one bounded batch of registrations.
func (c *Client) authorizeInteractionRecords(
	ctx context.Context,
	records []explorer.InteractionRecord,
	decision auth.Decision,
	now time.Time,
) ([]bool, error) {
	allowed := make([]bool, len(records))
	batch := make([]int, 0, len(records))
	batchIDs := 0
	flush := func() error {
		if len(batch) == 0 {
			return nil
		}
		if err := c.authorizeInteractionBatch(
			ctx, records, batch, allowed, decision, now,
		); err != nil {
			return err
		}
		batch = batch[:0]
		batchIDs = 0
		return nil
	}
	for index, record := range records {
		if record.Summary.Deleted || len(record.TouchedNodeIDs) == 0 {
			// Tombstones discarded their source edges, and a zero-hit record
			// never had any, so both are visible only to the exact
			// authorization projection that created them.
			allowed[index] = summaryFingerprintMatchesDecision(
				record.Summary, decision)
			continue
		}
		count := interactionAuthorizationCost(
			len(record.TouchedNodeIDs), len(record.TouchedEdgeIDs))
		if batchIDs > 0 && batchIDs+count > maxInteractionAuthorizationIDs {
			if err := flush(); err != nil {
				return nil, err
			}
		}
		batch = append(batch, index)
		batchIDs += count
		if batchIDs >= maxInteractionAuthorizationIDs {
			if err := flush(); err != nil {
				return nil, err
			}
		}
	}
	if err := flush(); err != nil {
		return nil, err
	}
	return allowed, nil
}

// authorizeInteractionBatch resolves one bounded batch of records in a single
// edge and node lookup pair. A record whose own provenance is larger than the
// batch bound is the only member of its batch and is resolved in chunks.
func (c *Client) authorizeInteractionBatch(
	ctx context.Context,
	records []explorer.InteractionRecord,
	batch []int,
	allowed []bool,
	decision auth.Decision,
	now time.Time,
) error {
	batchNodeIDs := make([]shoal.ID, 0, maxInteractionAuthorizationIDs)
	batchEdgeIDs := make([]shoal.ID, 0, maxInteractionAuthorizationIDs)
	for _, index := range batch {
		batchNodeIDs = append(batchNodeIDs, records[index].TouchedNodeIDs...)
		batchEdgeIDs = append(batchEdgeIDs, records[index].TouchedEdgeIDs...)
	}
	if interactionAuthorizationCost(
		len(batchNodeIDs), len(batchEdgeIDs),
	) > maxInteractionAuthorizationIDs {
		for _, index := range batch {
			decided, err := c.interactionEvidenceAllowsBounded(
				ctx,
				records[index].TouchedNodeIDs,
				records[index].TouchedEdgeIDs,
				decision,
				auth.OperationRead,
				now,
			)
			if err != nil {
				return err
			}
			allowed[index] = decided
		}
		return nil
	}
	edges, err := c.resolveEdges(ctx, batchEdgeIDs)
	if err != nil {
		return err
	}
	for _, registration := range edges {
		batchNodeIDs = append(
			batchNodeIDs, registration.Edge.From, registration.Edge.To)
	}
	registrations, err := c.resolveNodes(ctx, batchNodeIDs)
	if err != nil {
		return err
	}
	for _, index := range batch {
		decided, err := interactionEvidenceAllows(
			registrations, edges,
			records[index].TouchedNodeIDs, records[index].TouchedEdgeIDs,
			decision, auth.OperationRead, now,
		)
		if err != nil {
			return err
		}
		allowed[index] = decided
	}
	return nil
}

// InteractionRecord returns one authorized point record without scanning the
// complete interaction history.
func (c *Client) InteractionRecord(
	ctx context.Context, sessionID shoal.ID,
) (explorer.InteractionRecord, error) {
	reader, err := c.interactionReader()
	if err != nil {
		return explorer.InteractionRecord{}, err
	}
	decision, guard, now, err := c.begin(ctx, auth.OperationRead)
	if err != nil {
		return explorer.InteractionRecord{}, err
	}
	record, err := reader.InteractionRecord(ctx, sessionID)
	if err != nil {
		if shoal.IsErrorCode(err, shoal.ErrorUnavailable) ||
			shoal.IsErrorCode(err, shoal.ErrorConflict) {
			return explorer.InteractionRecord{}, auth.ObjectNotFound()
		}
		return explorer.InteractionRecord{}, directBaseError(err)
	}
	if record.Summary.Deleted {
		if !summaryFingerprintMatchesDecision(record.Summary, decision) {
			return explorer.InteractionRecord{}, auth.ObjectNotFound()
		}
	} else if len(record.TouchedNodeIDs) == 0 {
		if !summaryFingerprintMatchesDecision(record.Summary, decision) {
			return explorer.InteractionRecord{}, auth.ObjectNotFound()
		}
	} else if err := c.authorizeInteractionEvidence(
		ctx,
		record.TouchedNodeIDs,
		record.TouchedEdgeIDs,
		decision,
		auth.OperationRead,
		now,
	); err != nil {
		if shoal.IsErrorCode(err, shoal.ErrorUnauthorized) ||
			shoal.IsErrorCode(err, shoal.ErrorNotFound) {
			return explorer.InteractionRecord{}, auth.ObjectNotFound()
		}
		return explorer.InteractionRecord{}, err
	}
	// The revisions it was derived from (#695), decided like
	// revisionAllows; a denial is exactly a record that was never written.
	if !record.Summary.Deleted {
		bound, err := c.interactionRevisionsAllow(ctx, decision, now, record)
		if err != nil {
			return explorer.InteractionRecord{}, err
		}
		if !bound {
			return explorer.InteractionRecord{}, auth.ObjectNotFound()
		}
	}
	readable, ok, err := c.readableInteraction(ctx, decision, now, record)
	if err != nil {
		return explorer.InteractionRecord{}, err
	}
	if !ok {
		return explorer.InteractionRecord{}, auth.ObjectNotFound()
	}
	if err := guard.Check(ctx); err != nil {
		return explorer.InteractionRecord{}, err
	}
	return readable, nil
}

// Interaction returns one authorized typed interaction. It is an explicit
// derived view and therefore cannot affect the source-only retrieval surface.
func (c *Client) Interaction(
	ctx context.Context, sessionID shoal.ID,
) (interaction.Session, error) {
	record, err := c.InteractionRecord(ctx, sessionID)
	if err != nil {
		return interaction.Session{}, err
	}
	if record.Summary.Deleted || record.Session.ID == "" {
		return interaction.Session{}, auth.ObjectNotFound()
	}
	return record.Session, nil
}

// InteractionSubgraph returns an authorized explicit graph view. Every
// touched source is re-authorized before any derived node or edge is returned.
func (c *Client) InteractionSubgraph(
	ctx context.Context, sessionID shoal.ID,
) (explorer.Neighborhood, error) {
	reader, err := c.interactionReader()
	if err != nil {
		return explorer.Neighborhood{}, err
	}
	decision, guard, now, err := c.begin(ctx, auth.OperationRead)
	if err != nil {
		return explorer.Neighborhood{}, err
	}
	record, err := reader.InteractionRecord(ctx, sessionID)
	if err != nil {
		if shoal.IsErrorCode(err, shoal.ErrorUnavailable) ||
			shoal.IsErrorCode(err, shoal.ErrorConflict) {
			return explorer.Neighborhood{}, auth.ObjectNotFound()
		}
		return explorer.Neighborhood{}, directBaseError(err)
	}
	if record.Summary.Deleted {
		if !summaryFingerprintMatchesDecision(record.Summary, decision) {
			return explorer.Neighborhood{}, auth.ObjectNotFound()
		}
		subgraph, readErr := reader.InteractionSubgraph(ctx, sessionID)
		if readErr != nil {
			return explorer.Neighborhood{}, directBaseError(readErr)
		}
		permitted, err := c.subgraphLabelsPermitted(ctx, decision, now, record)
		if err != nil {
			return explorer.Neighborhood{}, err
		}
		if err := guard.Check(ctx); err != nil {
			return explorer.Neighborhood{}, err
		}
		return withholdSubgraphLabels(subgraph, permitted), nil
	}
	if len(record.TouchedNodeIDs) == 0 &&
		!summaryFingerprintMatchesDecision(record.Summary, decision) {
		return explorer.Neighborhood{}, auth.ObjectNotFound()
	}
	if err := c.authorizeInteractionEvidence(
		ctx,
		record.TouchedNodeIDs,
		record.TouchedEdgeIDs,
		decision,
		auth.OperationRead,
		now,
	); err != nil {
		if shoal.IsErrorCode(err, shoal.ErrorUnauthorized) {
			return explorer.Neighborhood{}, auth.ObjectNotFound()
		}
		return explorer.Neighborhood{}, err
	}
	bound, err := c.interactionRevisionsAllow(ctx, decision, now, record)
	if err != nil {
		return explorer.Neighborhood{}, err
	}
	if !bound {
		return explorer.Neighborhood{}, auth.ObjectNotFound()
	}
	subgraph, err := reader.InteractionSubgraph(ctx, sessionID)
	if err != nil {
		return explorer.Neighborhood{}, directBaseError(err)
	}
	if interactionSubgraphIsTombstone(subgraph) &&
		!summaryFingerprintMatchesDecision(record.Summary, decision) {
		return explorer.Neighborhood{}, auth.ObjectNotFound()
	}
	permitted, err := c.subgraphLabelsPermitted(ctx, decision, now, record)
	if err != nil {
		return explorer.Neighborhood{}, err
	}
	if err := guard.Check(ctx); err != nil {
		return explorer.Neighborhood{}, err
	}
	return withholdSubgraphLabels(subgraph, permitted), nil
}

// subgraphLabelsPermitted applies the record read's label rule to the derived
// graph: a record the reader may not see is ObjectNotFound, and its stamped
// expression is shown only when the reader may see every term of it.
func (c *Client) subgraphLabelsPermitted(
	ctx context.Context,
	decision auth.Decision,
	now time.Time,
	record explorer.InteractionRecord,
) (bool, error) {
	readable, ok, err := c.readableInteraction(ctx, decision, now, record)
	if err != nil {
		return false, err
	}
	if !ok {
		return false, auth.ObjectNotFound()
	}
	return readable.Summary.Visibility == record.Summary.Visibility, nil
}

func interactionSubgraphIsTombstone(subgraph explorer.Neighborhood) bool {
	return len(subgraph.Nodes) == 1 &&
		subgraph.Nodes[0].Kind == interaction.KindTombstone
}

func (c *Client) authorizeInteractionEvidence(
	ctx context.Context,
	nodeIDs []shoal.ID,
	edgeIDs []shoal.ID,
	decision auth.Decision,
	operation auth.Operation,
	now time.Time,
) error {
	allowed, err := c.interactionEvidenceAllowsBounded(
		ctx, nodeIDs, edgeIDs, decision, operation, now)
	if err != nil {
		return err
	}
	if !allowed {
		return auth.ObjectNotFound()
	}
	return nil
}

// interactionEvidenceAllowsBounded authorizes one record's uncapped provenance
// without ever submitting more than maxInteractionAuthorizationIDs identifiers
// in a single policy-store lookup. Authorization is a conjunction, so chunking
// preserves the fail-closed result exactly.
func (c *Client) interactionEvidenceAllowsBounded(
	ctx context.Context,
	nodeIDs []shoal.ID,
	edgeIDs []shoal.ID,
	decision auth.Decision,
	operation auth.Operation,
	now time.Time,
) (bool, error) {
	for start := 0; start < len(edgeIDs); {
		end := chunkEnd(start, len(edgeIDs), maxInteractionAuthorizationIDs/2)
		chunk := edgeIDs[start:end]
		start = end
		edges, err := c.resolveEdges(ctx, chunk)
		if err != nil {
			return false, err
		}
		endpoints := make([]shoal.ID, 0, 2*len(edges))
		for _, registration := range edges {
			endpoints = append(
				endpoints, registration.Edge.From, registration.Edge.To)
		}
		registrations, err := c.resolveNodes(ctx, endpoints)
		if err != nil {
			return false, err
		}
		allowed, err := interactionEvidenceAllows(
			registrations, edges, endpoints, chunk, decision, operation, now)
		if err != nil || !allowed {
			return false, err
		}
	}
	for start := 0; start < len(nodeIDs); {
		end := chunkEnd(start, len(nodeIDs), maxInteractionAuthorizationIDs)
		chunk := nodeIDs[start:end]
		start = end
		registrations, err := c.resolveNodes(ctx, chunk)
		if err != nil {
			return false, err
		}
		allowed, err := interactionEvidenceAllows(
			registrations, nil, chunk, nil, decision, operation, now)
		if err != nil || !allowed {
			return false, err
		}
	}
	return true, nil
}

// chunkEnd is the exclusive end of the chunk of at most size elements that
// starts at start.
func chunkEnd(start, length, size int) int {
	end := start + size
	if end > length {
		return length
	}
	return end
}

// interactionAuthorizationCost is the number of identifiers a batch submits to
// the policy store. Every edge costs its own identifier plus the two endpoint
// node identifiers its registration expands into, so the bound holds for the
// node lookup as well as the edge lookup.
func interactionAuthorizationCost(nodes, edges int) int {
	return nodes + 3*edges
}

func interactionEvidenceAllows(
	registrations registeredNodes,
	edges registeredEdges,
	nodeIDs []shoal.ID,
	edgeIDs []shoal.ID,
	decision auth.Decision,
	operation auth.Operation,
	now time.Time,
) (bool, error) {
	for _, nodeID := range nodeIDs {
		registration, ok := registrations[nodeID]
		if !ok {
			return false, nil
		}
		allowed, err := ruleAllows(
			registration.Rule, decision, operation, now)
		if err != nil {
			return false, err
		}
		if !allowed {
			return false, nil
		}
	}
	for _, edgeID := range edgeIDs {
		registration, ok := edges[edgeID]
		if !ok || registration.Edge.ID != edgeID {
			return false, nil
		}
		allowed, err := edgeAllowsResolved(
			registrations, registration, decision, operation, now)
		if err != nil {
			return false, err
		}
		if !allowed {
			return false, nil
		}
	}
	return true, nil
}

func interactionPinMatchesDecision(
	session interaction.Session, decision auth.Decision, now time.Time,
) bool {
	fingerprint, err := auth.AuthorizationFingerprint(decision)
	if err != nil {
		return false
	}
	// A runtime pin may deliberately be shorter than its enclosing credential,
	// but it must still be live now and may never outlive that credential.
	return session.AuthorizationFingerprint == shoal.ID(fingerprint.String()) &&
		now.Before(session.AuthorizationExpiresAt) &&
		!decision.AuthenticationExpires().Before(session.AuthorizationExpiresAt)
}

func summaryFingerprintMatchesDecision(
	summary explorer.InteractionSummary, decision auth.Decision,
) bool {
	fingerprint, err := auth.AuthorizationFingerprint(decision)
	if err != nil {
		return false
	}
	return summary.AuthorizationFingerprint == shoal.ID(fingerprint.String())
}

func (c *Client) interactionWriter() (explorer.InteractionWriter, error) {
	if isNilDependency(c.interactionSink) {
		return nil, shoal.NewError(
			shoal.ErrorUnavailable,
			"trusted interaction writer is unavailable",
		)
	}
	return c.interactionSink, nil
}

func (c *Client) interactionReader() (explorer.InteractionReader, error) {
	if isNilDependency(c.interactionSource) {
		return nil, shoal.NewError(
			shoal.ErrorUnavailable,
			"trusted interaction reader is unavailable",
		)
	}
	return c.interactionSource, nil
}

var (
	_ explorer.InteractionWriter       = (*Client)(nil)
	_ explorer.InteractionResultWriter = (*Client)(nil)
	_ explorer.InteractionReader       = (*Client)(nil)
)
