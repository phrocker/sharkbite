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

// Relabel narrowing (declassification) audit, issue #630.
//
// A principal holding every label on a document may re-ingest it under fewer
// labels (sourceClaimAllowsMutation requires the old rule and
// selectIngestRule the new one). That is allowed by design, but it must not
// be indistinguishable from an ordinary ingest, so every such relabel leaves a
// durable DeclassificationRecord in the policy catalog.
//
// # What counts as narrowing
//
// An AccessRule is a conjunction of structured policies, and a reader is
// allowed iff it is granted every component (AccessRule.Authorize). The test
// used here (ruleNarrows) reports a narrowing iff some logical component of
// the replaced rule P (domain, source, grant policy ID, service role; epochs
// never participate) is not a component of the new rule N.
//
// The test is conservative: it never misses a narrowing, because a rule that
// keeps every component of P requires at least what P requires. It is exact
// for LabelRule-shaped rules (one source policy plus one policy per label),
// where every component is an independently granted policy, so any subset of
// them can be held. For other shapes it can over-report. For example, a rule
// whose single component already stands for {S, X} replaced by the rule {S}
// AND {X} is reported although it admits nobody new. Such a false positive is
// harmless to the audit, but with RequireDeclassificationReason on it refuses
// that ingest unless a reason is given.
//
// A removed label, a swapped label ("x" for "secret"), a changed source policy
// and a dropped service role all count. Adding components (widening the label
// set, which narrows the readers) never does. Components are compared by
// their canonical logical keys, which are exact bytes: labels are never
// folded or trimmed (LabelPolicyID), so "Secret" and "secret" are different
// components and swapping one for the other is a removal.
//
// # When the audit is written
//
// The base write and the catalog registration are separate stores, so the
// audit cannot share one atomic write with the narrowing. It follows the
// approval-audit pattern instead (internal/explorerfleet ApprovalRecorder): an
// audit records an ATTEMPT, written durably before the step it describes, and
// the catalog's revision registrations record the outcome. Ingest writes:
//
//   - DeclassificationIntent, at source-claim time, after the principal has
//     been authorized under every rule it would replace and under the new
//     rule, and before the claim is swapped or the base is written. It
//     compares the new rule with the claim's rule, a pending claim's
//     PreviousRule, a legacy unclaimed document's registration, and the
//     current registration of the document the URI names (#665 authorizes
//     the principal under that registration at the same point).
//   - DeclassificationPreCommit, immediately before the PutRevision that
//     makes the narrowed revision current, which is the write that actually
//     lets the new readers in. It is written BEFORE that registration, so it
//     records that the narrowing was about to be committed, not that it was.
//     It compares the new rule with the document's current registration
//     again and carries the document and revision identities.
//
// If either audit write fails, the ingest fails before the step it guards, so
// no narrowing becomes visible without at least one durable record of it. A
// crash after an audit and before its step leaves an attempt record with no
// matching outcome, never the reverse. The outcome is the catalog's revision
// registration: a narrowing took effect iff the record's RevisionID is (or
// was) the document's current registration.
//
// Since #665 a source claim commits only once its revision is registered, so
// there is no state in which one principal's half-finished narrowing can be
// completed by another who lacks the replaced rule. A retry of an
// interrupted narrowing is an ordinary narrowing: it is authorized under the
// replaced rule (the pending claim's PreviousRule, or the registration), its
// own intent is recorded, and the reason policy applies to it.
//
// # Who can read it
//
// Records are reachable only through the PolicyStore (Declassifications); no
// Client method, HTTP route or MCP tool returns them. The one shipped reader
// is the offline operator listing, shoal-explore-web
// -list-declassifications (#664). The policy catalog already persists every
// rule in the clear, including label policy IDs and so the label names, so a
// record holding the replaced and new rules discloses nothing the catalog
// does not already hold to the same operator. Label names are therefore kept
// in the clear rather than digested, because an auditor needs them. A future
// read surface must restrict them to readers who could read the replaced
// rule.
//
// # Reason codes
//
// Config.RequireDeclassificationReason makes a narrowing that carries no
// reason (WithDeclassificationReason) fail with InvalidArgument at the intent
// check, before anything is written and only after the principal has been
// authorized under every rule it replaces and the new rule, so it tells them
// nothing new. The pre-commit check applies the same rule statelessly to any
// narrowing of the registration the claim-time check did not see: the
// principal must be authorized under that registration's rule (NotFound
// otherwise), and then the reason policy applies. shoal-explore-web sets
// the policy with -require-declassification-reason, and its upload surfaces
// (the browser route, the MCP ingest tool, the remote web API) carry a reason
// as declassification_reason (#664).
//
// # Copy-down through declared inputs (#663)
//
// Copy-down is the other half of #630: a principal who can read labelled
// content writes it into material stored under a weaker rule. Where an
// operation names its inputs, the comparison is precise. Each such operation
// in this package binds its output to those inputs on every read:
//
//   - Connect stores an application edge under the edge policy, which can
//     never carry a label (selectEdgeRule). So an edge to a labelled
//     document is stored under a rule weaker than that endpoint's. Every
//     read of an edge also requires both endpoints' current rules
//     (edgeAllowsResolved, edgeEndpointsAllow), and the rule of the revision
//     each endpoint belonged to when it was connected (#695,
//     revision_binding.go). A neighborhood drops an edge whose endpoint it
//     hides.
//   - An interaction's stored output restriction (RequiredVisibility) may
//     carry none of its evidence's labels. An MCP tool call, for example,
//     records only the workspace output policy. Every read also requires the
//     current rule of every touched node and edge
//     (authorizeInteractionRecords, interactionEvidenceAllows), and
//     revisionAllows for every revision the session was derived from
//     (#695). A fold requires every member.
//   - ExtractDocument registers what it extracts under the source document's
//     own rule. An extracted registration is conjoined with the document's
//     current rule once that document is relabelled (derived_rule.go).
//
// None of them serves, at the write, the output's content to a reader its
// inputs would refuse. The effective rule of each output is its stored rule
// AND its inputs' current rules AND the rules of the revisions it was derived
// from, which never narrows the inputs, not even after a relabel. This is a
// claim about content. Existence and count signals are separate gaps (below). So they write no
// DeclassificationRecord, and RequireDeclassificationReason does not apply to
// them. A record would describe a widening that does not happen, at one record
// per MCP read of labelled content, which breaks the retention premise below.
// Refusing them would refuse ordinary reads. The copy-down tests
// (copy_down_test.go) pin this binding for each operation.
//
// Known gaps, tracked separately:
//
//   - A fleet action's Output names its evidence but is served to readers
//     whose evidence refs are withheld (#696). That is a precise copy-down
//     that is not bound to its inputs.
//   - Existence and count oracles: a bounded neighborhood shows hidden
//     adjacency on a visible node only as one uniform bit, whether its
//     total adjacency exceeds MaxBoundedNodeAdjacency, and as scan time
//     that grows with hidden density (#698); a Connect ID colliding with a
//     hidden edge reveals it, and through intrinsic edge IDs a hidden
//     document by URI (#699); a shared extracted entity first owned by a
//     later-labelled document reveals its entity key (#700).
//
// Copy-down that names no input stays undetected: labelled text ingested
// under a new source URI, or placed in a MaterializeGraph node. Only session
// correlation or content similarity can link such a write to what the writer
// read. Those are operator signals, never gates (#694).
//
// # Retention
//
// Records are append-only and loaded into memory whenever the catalog opens;
// nothing compacts them. That is acceptable only because declassification is
// expected to be rare. The retention policy is an open decision on #664 (see
// docs/shoal-explore-web-deploy.md for the volume estimate).

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"time"

	"github.com/phrocker/shoal-oss/pkg/explorer/auth"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

// DeclassificationPhase names which guarded step a record precedes.
type DeclassificationPhase string

const (
	// DeclassificationIntent precedes the source-claim swap and the base
	// write.
	DeclassificationIntent DeclassificationPhase = "intent"
	// DeclassificationPreCommit precedes the registration that makes the
	// narrowed revision current.
	DeclassificationPreCommit DeclassificationPhase = "pre_commit"
)

const (
	// MaxDeclassificationReasonBytes bounds a reason code.
	MaxDeclassificationReasonBytes = 128
	// maxDeclassificationPriorRules bounds the replaced rules one record
	// may carry: a claim rule, a pending claim's previous rule and the
	// current registration's rule.
	maxDeclassificationPriorRules = 3
)

// Validate refuses an unknown phase.
func (p DeclassificationPhase) Validate() error {
	switch p {
	case DeclassificationIntent, DeclassificationPreCommit:
		return nil
	default:
		return shoal.NewError(
			shoal.ErrorInvalidArgument, "unknown declassification phase")
	}
}

// DeclassificationRecord is the durable audit of one relabel narrowing
// attempt (#630). AttemptID groups the intent and pre-commit records of one
// Ingest call. A record is written before the step it guards and so records
// an attempt; the catalog's revision registration is the outcome.
type DeclassificationRecord struct {
	AttemptID  shoal.ID
	Phase      DeclassificationPhase
	OccurredAt time.Time
	// SourceURI is the document's source identity, the key its source claim
	// is held under.
	SourceURI string
	// DocumentID and PriorRevisionID name the replaced current revision when
	// it is known (always on a pre-commit record). RevisionID is the narrowed
	// revision, set only on a pre-commit record.
	DocumentID      shoal.ID
	PriorRevisionID shoal.ID
	RevisionID      shoal.ID
	// PriorRules are the rules being replaced; Rule is the new one. At least
	// one prior rule has a component Rule lacks.
	PriorRules []AccessRule
	Rule       AccessRule
	// The acting principal and its request, from the trusted decision.
	Subject                  shoal.ID
	Actor                    shoal.ID
	ClientID                 shoal.ID
	OnBehalfOf               []shoal.ID
	RequestID                shoal.ID
	CorrelationID            shoal.ID
	AuthorizationFingerprint auth.Fingerprint
	// ReasonCode is the caller's declared reason, if any.
	ReasonCode string
}

// RemovedPolicies returns the components of the prior rules that Rule does
// not carry, deduplicated, in prior-rule order. These are exactly what the
// narrowing stopped requiring.
func (r DeclassificationRecord) RemovedPolicies() []auth.Policy {
	var removed []auth.Policy
	var seen [][]byte
	for _, prior := range r.PriorRules {
		for index, key := range prior.keys {
			if ruleHasKey(r.Rule, key) || containsKey(seen, key) {
				continue
			}
			seen = append(seen, key)
			if cloned, _, err := clonePolicy(prior.policies[index]); err == nil {
				removed = append(removed, cloned)
			}
		}
	}
	return removed
}

// RemovedLabels returns the free-form labels among RemovedPolicies, decoded
// from their label policy IDs. A removed component that is not a label
// policy (a changed source policy, for example) is not listed here.
func (r DeclassificationRecord) RemovedLabels() []string {
	var labels []string
	for _, policy := range r.RemovedPolicies() {
		if _, label, err := ParseLabelPolicyID(
			policy.GrantPolicyID()); err == nil {
			labels = append(labels, label)
		}
	}
	return labels
}

func containsKey(keys [][]byte, key []byte) bool {
	for _, candidate := range keys {
		if string(candidate) == string(key) {
			return true
		}
	}
	return false
}

// validate checks the record's invariants: a known phase, required
// identities, bounded fields, and that it really describes a narrowing.
func (r DeclassificationRecord) validate() error {
	if err := r.Phase.Validate(); err != nil {
		return err
	}
	if err := shoal.ValidateRequiredID(
		"declassification attempt ID", r.AttemptID); err != nil {
		return err
	}
	if r.OccurredAt.IsZero() {
		return shoal.NewError(shoal.ErrorInvalidArgument,
			"declassification occurrence time is required")
	}
	if err := validateSourceURI(r.SourceURI); err != nil {
		return err
	}
	for name, id := range map[string]shoal.ID{
		"declassification document ID":       r.DocumentID,
		"declassification prior revision ID": r.PriorRevisionID,
		"declassification revision ID":       r.RevisionID,
		"declassification subject":           r.Subject,
		"declassification actor":             r.Actor,
		"declassification client ID":         r.ClientID,
		"declassification request ID":        r.RequestID,
		"declassification correlation ID":    r.CorrelationID,
	} {
		if err := shoal.ValidateOptionalID(name, id); err != nil {
			return err
		}
	}
	if r.Phase == DeclassificationPreCommit {
		if err := shoal.ValidateRequiredID(
			"declassification document ID", r.DocumentID); err != nil {
			return err
		}
		if err := shoal.ValidateRequiredID(
			"declassification revision ID", r.RevisionID); err != nil {
			return err
		}
	}
	if len(r.OnBehalfOf) > auth.MaxOnBehalfOfEntries {
		return shoal.NewError(shoal.ErrorInvalidArgument,
			"declassification delegation chain exceeds its bound")
	}
	for _, id := range r.OnBehalfOf {
		if err := shoal.ValidateRequiredID(
			"declassification delegator", id); err != nil {
			return err
		}
	}
	if err := validateDeclassificationReason(r.ReasonCode); err != nil {
		return err
	}
	if len(r.Rule.keys) == 0 {
		return shoal.NewError(shoal.ErrorInvalidArgument,
			"declassification new rule is required")
	}
	if len(r.PriorRules) == 0 ||
		len(r.PriorRules) > maxDeclassificationPriorRules {
		return shoal.NewError(shoal.ErrorInvalidArgument,
			"declassification prior rules are out of bounds")
	}
	narrowed := false
	for _, prior := range r.PriorRules {
		if len(prior.keys) == 0 {
			return shoal.NewError(shoal.ErrorInvalidArgument,
				"declassification prior rule is empty")
		}
		if ruleNarrows(prior, r.Rule) {
			narrowed = true
		}
	}
	if !narrowed {
		return shoal.NewError(shoal.ErrorInvalidArgument,
			"declassification record describes no narrowing")
	}
	return nil
}

func cloneDeclassificationRecord(
	record DeclassificationRecord,
) (DeclassificationRecord, error) {
	cloned := record
	cloned.OccurredAt = record.OccurredAt.UTC()
	rule, err := record.Rule.clone()
	if err != nil {
		return DeclassificationRecord{}, err
	}
	cloned.Rule = rule
	cloned.PriorRules = make([]AccessRule, 0, len(record.PriorRules))
	for _, prior := range record.PriorRules {
		clonedPrior, err := prior.clone()
		if err != nil {
			return DeclassificationRecord{}, err
		}
		cloned.PriorRules = append(cloned.PriorRules, clonedPrior)
	}
	cloned.OnBehalfOf = append([]shoal.ID(nil), record.OnBehalfOf...)
	return cloned, nil
}

// ruleNarrows reports whether next admits a reader prior does not: whether
// some logical component of prior is missing from next. See the file comment
// for why this is conservative, and exact for LabelRule-shaped rules.
func ruleNarrows(prior, next AccessRule) bool {
	for _, key := range prior.keys {
		if !ruleHasKey(next, key) {
			return true
		}
	}
	return false
}

type declassificationReasonKey struct{}

// WithDeclassificationReason attaches a reason code to an ingest. It is
// recorded on any declassification audit the ingest writes and satisfies
// Config.RequireDeclassificationReason. A code is 1 to
// MaxDeclassificationReasonBytes bytes of ASCII letters, digits, '.', '_',
// '-', ':' or '/'.
func WithDeclassificationReason(
	ctx context.Context,
	reason string,
) (context.Context, error) {
	if err := ValidateDeclassificationReason(reason); err != nil {
		return nil, err
	}
	return context.WithValue(ctx, declassificationReasonKey{}, reason), nil
}

// ValidateDeclassificationReason reports whether reason is a code
// WithDeclassificationReason accepts, with the same InvalidArgument error. A
// transport that carries a reason (#664) checks it with this before any
// effect, so the rule lives in one place.
func ValidateDeclassificationReason(reason string) error {
	if reason == "" {
		return shoal.NewError(shoal.ErrorInvalidArgument,
			"declassification reason code is required")
	}
	return validateDeclassificationReason(reason)
}

// errDeclassificationReasonRequired is the cause every refusal under
// Config.RequireDeclassificationReason carries, so a transport can recognise
// it (IsDeclassificationReasonRequired) without matching message text.
var errDeclassificationReasonRequired = errors.New(
	"declassification reason code required")

func declassificationReasonRequired() error {
	return shoal.WrapError(shoal.ErrorInvalidArgument,
		"this relabel removes visibility labels and the operator "+
			"requires a declassification reason code",
		errDeclassificationReasonRequired)
}

// IsDeclassificationReasonRequired reports whether err is, or wraps, the
// refusal of a narrowing that carried no reason under
// Config.RequireDeclassificationReason. The refusal is returned only after
// the principal has been authorized under every rule the narrowing replaces
// and under the new rule, so a transport may tell the caller about it.
func IsDeclassificationReasonRequired(err error) bool {
	return errors.Is(err, errDeclassificationReasonRequired)
}

func declassificationReason(ctx context.Context) string {
	reason, _ := ctx.Value(declassificationReasonKey{}).(string)
	if validateDeclassificationReason(reason) != nil {
		return ""
	}
	return reason
}

func validateDeclassificationReason(reason string) error {
	if len(reason) > MaxDeclassificationReasonBytes {
		return shoal.NewError(shoal.ErrorInvalidArgument,
			"declassification reason code exceeds its bound")
	}
	for index := 0; index < len(reason); index++ {
		character := reason[index]
		switch {
		case character >= 'a' && character <= 'z',
			character >= 'A' && character <= 'Z',
			character >= '0' && character <= '9',
			character == '.', character == '_', character == '-',
			character == ':', character == '/':
		default:
			return shoal.NewError(shoal.ErrorInvalidArgument,
				"declassification reason code has an invalid character")
		}
	}
	return nil
}

// declassificationAttempt carries one Ingest call's audit state between its
// source claim and its registration.
type declassificationAttempt struct {
	id         shoal.ID
	priorRules []AccessRule
	reason     string
	documentID shoal.ID
	priorRev   shoal.ID
}

func newDeclassificationAttemptID() (shoal.ID, error) {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", catalogUnavailable()
	}
	return shoal.ID(hex.EncodeToString(raw[:])), nil
}

// declassificationIntent decides, at source-claim time, whether selected
// narrows any of the replaced rules. When it does it enforces the reason
// policy and durably records the intent before returning, so the caller may
// only proceed to the claim swap after this returns nil. It must be called
// only after the principal has been authorized under every prior rule and
// under selected.
func (c *Client) declassificationIntent(
	ctx context.Context,
	decision auth.Decision,
	now time.Time,
	sourceURI string,
	selected AccessRule,
	priorRules []AccessRule,
	documentID, priorRevisionID shoal.ID,
) (*declassificationAttempt, error) {
	narrowing := make([]AccessRule, 0, len(priorRules))
	for _, prior := range priorRules {
		if ruleNarrows(prior, selected) {
			narrowing = appendDistinctRule(narrowing, prior)
		}
	}
	if len(narrowing) == 0 {
		return nil, nil
	}
	attempt := &declassificationAttempt{
		priorRules: narrowing,
		reason:     declassificationReason(ctx),
		documentID: documentID,
		priorRev:   priorRevisionID,
	}
	if err := c.recordDeclassification(
		ctx, decision, now, DeclassificationIntent, sourceURI, selected,
		attempt, ""); err != nil {
		return nil, err
	}
	return attempt, nil
}

// declassificationCommit runs immediately before the registration that makes
// revisionID current. It audits a narrowing against the document's current
// registration, and always audits an attempt whose intent was recorded.
//
// A narrowing of the registration that the claim-time check did not compare
// with (its rule is not among the attempt's replaced rules) is checked here
// statelessly: the principal must be authorized under that registration's
// rule (NotFound otherwise), and the reason policy applies. #665's pre-write
// check normally makes this unreachable; it stays as the guard at the step
// that actually lets new readers in.
func (c *Client) declassificationCommit(
	ctx context.Context,
	decision auth.Decision,
	now time.Time,
	sourceURI string,
	documentID, revisionID shoal.ID,
	selected AccessRule,
	attempt *declassificationAttempt,
) error {
	current, ok, err := c.policyStore.CurrentRevision(ctx, documentID)
	if err != nil {
		return policyCatalogReadError(ctx, err)
	}
	var priorRules []AccessRule
	if attempt != nil {
		priorRules = append(priorRules, attempt.priorRules...)
	}
	priorRevision := shoal.ID("")
	if ok && current.RevisionID != revisionID {
		priorRevision = current.RevisionID
		if ruleNarrows(current.Rule, selected) {
			if attempt == nil || !containsRule(attempt.priorRules, current.Rule) {
				allowed, err := ruleAllows(
					current.Rule, decision, auth.OperationIngest, now)
				if err != nil {
					return err
				}
				if !allowed {
					return auth.ObjectNotFound()
				}
			}
			priorRules = appendDistinctRule(priorRules, current.Rule)
		}
	}
	if len(priorRules) == 0 {
		return nil
	}
	if attempt == nil {
		attempt = &declassificationAttempt{reason: declassificationReason(ctx)}
	}
	attempt.priorRules = priorRules
	attempt.documentID = documentID
	if priorRevision != "" {
		attempt.priorRev = priorRevision
	}
	return c.recordDeclassification(
		ctx, decision, now, DeclassificationPreCommit, sourceURI, selected,
		attempt, revisionID)
}

func containsRule(rules []AccessRule, rule AccessRule) bool {
	for _, candidate := range rules {
		if candidate.equal(rule) {
			return true
		}
	}
	return false
}

func appendDistinctRule(rules []AccessRule, rule AccessRule) []AccessRule {
	for _, existing := range rules {
		if existing.equal(rule) {
			return rules
		}
	}
	return append(rules, rule)
}

func (c *Client) recordDeclassification(
	ctx context.Context,
	decision auth.Decision,
	now time.Time,
	phase DeclassificationPhase,
	sourceURI string,
	selected AccessRule,
	attempt *declassificationAttempt,
	revisionID shoal.ID,
) error {
	if c.requireDeclassificationReason && attempt.reason == "" {
		return declassificationReasonRequired()
	}
	if attempt.id == "" {
		id, err := newDeclassificationAttemptID()
		if err != nil {
			return err
		}
		attempt.id = id
	}
	fingerprint, err := auth.AuthorizationFingerprint(decision)
	if err != nil {
		return authorizationDenied()
	}
	record := DeclassificationRecord{
		AttemptID:                attempt.id,
		Phase:                    phase,
		OccurredAt:               now,
		SourceURI:                sourceURI,
		DocumentID:               attempt.documentID,
		PriorRevisionID:          attempt.priorRev,
		RevisionID:               revisionID,
		PriorRules:               attempt.priorRules,
		Rule:                     selected,
		Subject:                  decision.Subject(),
		Actor:                    decision.Actor(),
		ClientID:                 decision.ClientID(),
		OnBehalfOf:               decision.OnBehalfOf(),
		RequestID:                decision.RequestID(),
		CorrelationID:            decision.CorrelationID(),
		AuthorizationFingerprint: fingerprint,
		ReasonCode:               attempt.reason,
	}
	if err := c.policyStore.RecordDeclassification(ctx, record); err != nil {
		return policyCatalogWriteError(ctx, err)
	}
	return nil
}

// RecordDeclassification appends one declassification audit record. The
// record is validated, including that it really describes a narrowing.
func (s *MemoryPolicyStore) RecordDeclassification(
	ctx context.Context,
	record DeclassificationRecord,
) error {
	if err := contextFailure(ctx); err != nil {
		return err
	}
	if s == nil {
		return catalogUnavailable()
	}
	if err := record.validate(); err != nil {
		return err
	}
	cloned, err := cloneDeclassificationRecord(record)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.hasDeclassificationLocked(record.AttemptID, record.Phase) {
		return duplicateDeclassification()
	}
	s.declassifications = append(s.declassifications, cloned)
	return nil
}

// hasDeclassification reports whether (attemptID, phase) is recorded. Records
// are append-only, so a second record for the same pair is refused rather
// than allowed to shadow the first on disk.
func (s *MemoryPolicyStore) hasDeclassification(
	attemptID shoal.ID,
	phase DeclassificationPhase,
) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.hasDeclassificationLocked(attemptID, phase)
}

func (s *MemoryPolicyStore) hasDeclassificationLocked(
	attemptID shoal.ID,
	phase DeclassificationPhase,
) bool {
	for _, existing := range s.declassifications {
		if existing.AttemptID == attemptID && existing.Phase == phase {
			return true
		}
	}
	return false
}

func duplicateDeclassification() error {
	return shoal.NewError(shoal.ErrorConflict,
		"declassification audit record already exists")
}

// Declassifications returns every declassification audit record in the order
// it was recorded. It is an operator read: no Client method exposes it.
func (s *MemoryPolicyStore) Declassifications(
	ctx context.Context,
) ([]DeclassificationRecord, error) {
	if err := contextFailure(ctx); err != nil {
		return nil, err
	}
	if s == nil {
		return nil, catalogUnavailable()
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	records := make([]DeclassificationRecord, 0, len(s.declassifications))
	for _, record := range s.declassifications {
		cloned, err := cloneDeclassificationRecord(record)
		if err != nil {
			return nil, catalogUnavailable()
		}
		records = append(records, cloned)
	}
	return records, nil
}
