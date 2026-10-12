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
//     been authorized under both the replaced and the new rule and before
//     the claim is swapped or the base is written. It compares the new rule
//     with the claim's rule (and a pending claim's PreviousRule), or with a
//     legacy unclaimed document's current registration.
//   - DeclassificationPreCommit, immediately before the PutRevision that
//     makes the narrowed revision current, which is the write that actually
//     lets the new readers in. It is written BEFORE that registration, so
//     it records that the narrowing was about to be committed, not that it
//     was. It compares the new rule with the document's current
//     registration as well, so a narrowing is caught at the visibility
//     commit even if the claim and the registration ever disagreed (for
//     example a retry after an earlier attempt registered nothing). It
//     carries the document and revision identities.
//
// If either audit write fails, the ingest fails before the step it guards, so
// no narrowing becomes visible without at least one durable record of it. A
// crash after an audit and before its step leaves an attempt record with no
// matching outcome, never the reverse. The outcome is the catalog's revision
// registration: a narrowing took effect iff the record's RevisionID is (or
// was) the document's current registration.
//
// # Who can read it
//
// Records are reachable only through the PolicyStore (Declassifications); no
// Client method, HTTP route or MCP tool returns them. The policy catalog
// already persists every rule in the clear, including label policy IDs and so
// the label names, so a record holding the replaced and new rules discloses
// nothing the catalog does not already hold to the same operator. Label names
// are therefore kept in the clear rather than digested, because an auditor
// needs them. A future read surface must restrict them to readers who could
// read the replaced rule, and is out of scope here.
//
// # Reason codes
//
// Config.RequireDeclassificationReason makes a narrowing that carries no
// reason (WithDeclassificationReason) fail with InvalidArgument at its first
// check. The refusal always comes after the principal has been authorized
// under the replaced and the new rule, so it tells them nothing new.
//
//   - Normally the first check is the intent check, before anything is
//     written. It compares against the claim, a pending claim's previous
//     rule, or an unclaimed URI's registration found through the base.
//   - A narrowing first seen at the pre-commit check, where the claim this
//     ingest holds does not descend from an intent covering it, is also a
//     first check. The claim-time check had nothing to compare with because
//     the catalog and the base diverged: for example, a base restored from
//     backup no longer holds a document the catalog still registers, or
//     such a state let an attempt create a claim without passing this
//     check. The principal is first authorized under the registration it
//     replaces (NotFound otherwise), and then the reason policy applies,
//     after the base write and before the registration. This holds even
//     when the ingest recorded its own intent against the claim: that
//     intent authorized the principal under the claim, not under a
//     registration it never compared with.
//   - A narrowing first seen at the pre-commit check, where the claim DOES
//     descend from a covering intent, is a reconciliation. Claim lineage
//     works like this: every claim version created by a CAS that followed an
//     intent is bound to that intent (BindDeclassificationClaim, written
//     before the base). A later ingest that records no intent and keeps
//     every component of the replaced claim's rule and of the bound
//     intent's rule inherits that binding while the lineage is open: its
//     own version is bound to the same intent, also before the base write,
//     so the lineage survives its failures, pends and restarts. When an
//     ingest on a bound version registers its revision as current, the
//     narrowing no longer lags, and the lineage is closed
//     (CloseDeclassificationClaim). Ordinary later ingests therefore
//     inherit nothing, write nothing, and keep the pre-#630 claim commit on
//     apply. "Covering" means the same URI, the selected rule keeping
//     every component of the intent's rule (a retry may be stricter), and
//     the current registration's rule being exactly one of the intent's
//     replaced rules. Here the claim already holds the narrow rule while the
//     registration lags (#665), and that intent already passed the reason
//     policy. The narrowing is recorded but not refused. The claim is the
//     mutation authority, so the claim holder completing it need not be
//     authorized under the lagging registration, and a refusal would
//     disclose that the registration carries labels.
//
// An intent on file that the current claim does not descend from never
// counts: one left by a CAS conflict, a failed base write, or an old
// completed narrowing. Neither does a claim created without an intent.
//
// The invariant: a narrowing never becomes current unless the reason policy
// was applied at its first check. That check is either an earlier attempt's
// recorded intent or a check this ingest makes itself.

import (
	"context"
	"crypto/rand"
	"encoding/hex"
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
	// it is known (always on a pre-commit record). RevisionID is the
	// narrowed revision, set only on a pre-commit record.
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
	if reason == "" {
		return nil, shoal.NewError(shoal.ErrorInvalidArgument,
			"declassification reason code is required")
	}
	if err := validateDeclassificationReason(reason); err != nil {
		return nil, err
	}
	return context.WithValue(ctx, declassificationReasonKey{}, reason), nil
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
	// reconciliation marks a narrowing first seen at the pre-commit check,
	// which completes an earlier, already-audited attempt.
	reconciliation bool
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
func (c *Client) declassificationCommit(
	ctx context.Context,
	decision auth.Decision,
	now time.Time,
	sourceURI string,
	documentID, revisionID shoal.ID,
	selected AccessRule,
	attempt *declassificationAttempt,
	claimVersion uint64,
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
	// uncovered is a narrowing of the current registration that the
	// claim-time check neither compared with nor authorized the principal
	// under. That check authorized every rule in attempt.priorRules.
	uncovered := false
	if ok && current.RevisionID != revisionID {
		priorRevision = current.RevisionID
		if ruleNarrows(current.Rule, selected) {
			if attempt == nil || !containsRule(attempt.priorRules, current.Rule) {
				uncovered = true
			}
			priorRules = appendDistinctRule(priorRules, current.Rule)
		}
	}
	if len(priorRules) == 0 {
		return nil
	}
	if uncovered {
		covering, err := c.claimIntentCovers(
			ctx, sourceURI, claimVersion, selected, current.Rule)
		reconciled := covering != ""
		if err != nil {
			return err
		}
		switch {
		case reconciled && attempt == nil:
			// The claim this ingest holds descends from an earlier
			// attempt's intent (its claim lineage, not merely some intent
			// on file), which narrowed exactly this registration and passed
			// the reason policy. The claim already holds the narrow rule
			// while the registration lags (an earlier pre-commit audit or
			// PutRevision failed after the base write, #665). The claim is
			// the mutation authority, so any claim holder may complete it,
			// exactly as before #630. The reason policy is not re-applied:
			// refusing would tell a claim holder outside the
			// registration's rule that the registration carries labels it
			// lacks. The record names the principal whose ingest makes the
			// narrowing current.
			attempt = &declassificationAttempt{
				reason:         declassificationReason(ctx),
				reconciliation: true,
			}
		case reconciled:
			// This ingest's own intent (reason-checked at claim time)
			// covers the registration through its lineage.
		default:
			// The claim does not descend from an intent covering this
			// narrowing of the registration, so this is its first check
			// against it: the claim-time check had nothing to compare
			// with, because the catalog and the base diverged (a current
			// registration for a URI whose base document is gone, for
			// example a base restored from backup), or the claim was
			// created by an attempt that never passed this check. That
			// holds whether or not this ingest recorded an intent against
			// the claim: such an intent authorized the principal under the
			// claim, not under this registration. An intent on file that
			// this claim does not descend from (a CAS conflict, a failed
			// base write, an old completed narrowing) never counts. The
			// principal must be authorized under the registration it
			// replaces (NotFound otherwise, so nothing about its labels is
			// disclosed), and the reason policy applies.
			allowed, err := ruleAllows(
				current.Rule, decision, auth.OperationIngest, now)
			if err != nil {
				return err
			}
			if !allowed {
				return auth.ObjectNotFound()
			}
			if attempt == nil {
				attempt = &declassificationAttempt{
					reason: declassificationReason(ctx),
				}
			}
		}
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

// claimIntentCovers returns the attempt ID of the intent the held claim
// version (sourceURI, version) is bound to, while that lineage is open, when
// it covers narrowing prior to selected, or "". Covering means the same URI,
// a rule whose every component selected keeps (a retry may choose a stricter
// rule), and prior exactly among its replaced rules. Only such an intent
// passed the reason policy for this narrowing on this claim's lineage.
func (c *Client) claimIntentCovers(
	ctx context.Context,
	sourceURI string,
	version uint64,
	selected, prior AccessRule,
) (shoal.ID, error) {
	if version == 0 {
		return "", nil
	}
	intent, ok, err := c.policyStore.DeclassificationClaimIntent(
		ctx, sourceURI, version)
	if err != nil {
		return "", policyCatalogReadError(ctx, err)
	}
	if !ok || intent.Phase != DeclassificationIntent ||
		intent.SourceURI != sourceURI || ruleNarrows(intent.Rule, selected) ||
		!containsRule(intent.PriorRules, prior) {
		return "", nil
	}
	return intent.AttemptID, nil
}

// declassificationLineage returns the intent the new claim version must be
// bound to before the base is written, or "". That is this ingest's own
// intent, if it recorded one. Otherwise, it is the intent the replaced claim
// version is bound to, while that lineage is open (its narrowing has not yet
// been registered) and the selected rule keeps every component of both the
// replaced claim's rule and the intent's rule. So a retry completing a lagging
// narrowing (#665) stays on its lineage across failures, pends and restarts,
// and a claim created without an intent never acquires one. Once a narrowing
// is registered its lineage is closed, so ordinary later ingests inherit
// nothing and write nothing. A selected rule that narrows the replaced claim
// always records its own intent, so that rule test is defensive.
func (c *Client) declassificationLineage(
	ctx context.Context,
	sourceURI string,
	existing *SourcePolicyClaim,
	selected AccessRule,
	attempt *declassificationAttempt,
) (shoal.ID, error) {
	if attempt != nil {
		return attempt.id, nil
	}
	if existing == nil || ruleNarrows(existing.Rule, selected) {
		return "", nil
	}
	intent, ok, err := c.policyStore.DeclassificationClaimIntent(
		ctx, sourceURI, existing.Version)
	if err != nil {
		return "", policyCatalogReadError(ctx, err)
	}
	if !ok || ruleNarrows(intent.Rule, selected) {
		return "", nil
	}
	return intent.AttemptID, nil
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
	if c.requireDeclassificationReason && attempt.reason == "" &&
		!attempt.reconciliation {
		return shoal.NewError(shoal.ErrorInvalidArgument,
			"this relabel removes visibility labels and the operator "+
				"requires a declassification reason code")
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
	s.appendDeclassificationLocked(cloned)
	return nil
}

func (s *MemoryPolicyStore) appendDeclassificationLocked(
	record DeclassificationRecord,
) {
	if record.Phase == DeclassificationIntent {
		if s.declassificationIntents == nil {
			s.declassificationIntents = make(map[shoal.ID]int)
		}
		s.declassificationIntents[record.AttemptID] = len(s.declassifications)
	}
	s.declassifications = append(s.declassifications, record)
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

// declassificationClaimKey names one source claim version. Versions come from
// one monotonic counter, are kept by commit and pend, and are restored by
// rollback, so a version identifies one claim state for its whole life.
type declassificationClaimKey struct {
	sourceURI string
	version   uint64
}

// BindDeclassificationClaim records that source claim version version of
// sourceURI descends from the intent attemptID. The intent must already be
// recorded for the same URI. Rebinding a version to the same intent is a
// no-op; to a different one, a conflict.
func (s *MemoryPolicyStore) BindDeclassificationClaim(
	ctx context.Context,
	sourceURI string,
	version uint64,
	attemptID shoal.ID,
) error {
	if err := contextFailure(ctx); err != nil {
		return err
	}
	if s == nil {
		return catalogUnavailable()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.bindDeclassificationClaimLocked(sourceURI, version, attemptID)
}

func (s *MemoryPolicyStore) checkDeclassificationClaimLocked(
	sourceURI string,
	version uint64,
	attemptID shoal.ID,
) (bool, error) {
	if err := validateSourceURI(sourceURI); err != nil {
		return false, err
	}
	if version == 0 {
		return false, shoal.NewError(shoal.ErrorInvalidArgument,
			"declassification claim version is required")
	}
	if err := shoal.ValidateRequiredID(
		"declassification attempt ID", attemptID); err != nil {
		return false, err
	}
	intent, ok := s.declassificationIntentLocked(attemptID)
	if !ok || intent.SourceURI != sourceURI {
		return false, shoal.NewError(shoal.ErrorInvalidArgument,
			"declassification claim binding names no intent for its source")
	}
	key := declassificationClaimKey{sourceURI: sourceURI, version: version}
	if existing, bound := s.declassificationClaims[key]; bound {
		if existing != attemptID {
			return false, duplicateDeclassification()
		}
		return true, nil
	}
	return false, nil
}

func (s *MemoryPolicyStore) bindDeclassificationClaimLocked(
	sourceURI string,
	version uint64,
	attemptID shoal.ID,
) error {
	bound, err := s.checkDeclassificationClaimLocked(
		sourceURI, version, attemptID)
	if err != nil || bound {
		return err
	}
	if s.declassificationClaims == nil {
		s.declassificationClaims = make(map[declassificationClaimKey]shoal.ID)
	}
	s.declassificationClaims[declassificationClaimKey{
		sourceURI: sourceURI, version: version,
	}] = attemptID
	return nil
}

func (s *MemoryPolicyStore) declassificationIntentLocked(
	attemptID shoal.ID,
) (DeclassificationRecord, bool) {
	index, ok := s.declassificationIntents[attemptID]
	if !ok || index >= len(s.declassifications) {
		return DeclassificationRecord{}, false
	}
	return s.declassifications[index], true
}

// CloseDeclassificationClaim ends the lineage of a bound source claim
// version: its narrowing has been registered, so nothing lags behind it.
// Closing an unbound version is refused; closing twice is a no-op.
func (s *MemoryPolicyStore) CloseDeclassificationClaim(
	ctx context.Context,
	sourceURI string,
	version uint64,
) error {
	if err := contextFailure(ctx); err != nil {
		return err
	}
	if s == nil {
		return catalogUnavailable()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.closeDeclassificationClaimLocked(sourceURI, version)
}

func (s *MemoryPolicyStore) checkCloseDeclassificationClaimLocked(
	sourceURI string,
	version uint64,
) (bool, error) {
	key := declassificationClaimKey{sourceURI: sourceURI, version: version}
	if _, bound := s.declassificationClaims[key]; !bound {
		return false, shoal.NewError(shoal.ErrorInvalidArgument,
			"declassification claim to close is not bound")
	}
	_, closed := s.closedDeclassificationClaims[key]
	return closed, nil
}

func (s *MemoryPolicyStore) closeDeclassificationClaimLocked(
	sourceURI string,
	version uint64,
) error {
	closed, err := s.checkCloseDeclassificationClaimLocked(sourceURI, version)
	if err != nil || closed {
		return err
	}
	if s.closedDeclassificationClaims == nil {
		s.closedDeclassificationClaims = make(map[declassificationClaimKey]struct{})
	}
	s.closedDeclassificationClaims[declassificationClaimKey{
		sourceURI: sourceURI, version: version,
	}] = struct{}{}
	return nil
}

// DeclassificationClaimIntent returns the intent record source claim version
// version of sourceURI is bound to, if any and only while that lineage is
// open (not closed). It is an exact indexed read.
func (s *MemoryPolicyStore) DeclassificationClaimIntent(
	ctx context.Context,
	sourceURI string,
	version uint64,
) (DeclassificationRecord, bool, error) {
	if err := contextFailure(ctx); err != nil {
		return DeclassificationRecord{}, false, err
	}
	if s == nil {
		return DeclassificationRecord{}, false, catalogUnavailable()
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	key := declassificationClaimKey{sourceURI: sourceURI, version: version}
	attemptID, ok := s.declassificationClaims[key]
	if !ok {
		return DeclassificationRecord{}, false, nil
	}
	if _, closed := s.closedDeclassificationClaims[key]; closed {
		return DeclassificationRecord{}, false, nil
	}
	intent, ok := s.declassificationIntentLocked(attemptID)
	if !ok {
		return DeclassificationRecord{}, false, catalogUnavailable()
	}
	cloned, err := cloneDeclassificationRecord(intent)
	if err != nil {
		return DeclassificationRecord{}, false, catalogUnavailable()
	}
	return cloned, true, nil
}
