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

package auth

import (
	"bytes"
	"strconv"
	"strings"
	"time"
)

// SplitVisibilityConjunction converts the &-joined expression form of a stored
// label set into the term-slice form VisibilityPermittedForUser and
// VisibilityPermittedForService accept.
//
// Stored label sets appear in both forms. interaction.Session.RequiredVisibility,
// fleet.EvidenceRef.Visibility and the fold member visibilities hold a slice
// of terms; the persisted interaction and fold records hold the same set
// rendered by interaction.Expression, which joins the sorted terms with '&'.
// Neither form has disjunction: interaction.Conjoin is the only constructor.
//
// The split is on '&' alone and never trims or interprets anything else, so a
// '|', a parenthesis or whitespace stays inside a term, and the evaluator
// refuses that term as malformed. An empty piece (from "a&&b", a leading or a
// trailing '&') is kept as an empty term, which is also malformed. Only the
// empty expression is the unlabelled set, returned as nil.
func SplitVisibilityConjunction(expression string) []string {
	if expression == "" {
		return nil
	}
	return strings.Split(expression, "&")
}

// VisibilityPermittedForUser decides whether a user decision may see data
// stored with the conjunction labels, without a policy catalog and without a
// scan. It answers for stored (not scanned) labelled fields the question the
// tablet answers for cells (#564). A decision carrying a service role is an
// error here: it must be asked through VisibilityPermittedForService, so that
// neither call site can be read as the other.
//
// labels is a conjunction: every term must pass, and an empty set is
// unlabelled, which every valid decision may see. Each term must be a
// structured grant label from the policy grammar:
//
//   - d:<domain> passes when domain is the decision's authorization domain;
//   - s:<source> passes when source is a permitted source;
//   - g:<policy>:e:<epoch> passes when policy is a permitted policy, at any
//     epoch. A decision carries logical grants only, and AccessRule.Authorize
//     already treats physical epochs as not participating in object
//     authorization, so a record stamped at an older epoch stays visible to a
//     reader still granted its logical policy;
//   - svc:<role> never passes for a user.
//
// A free-form term (for example an ingest label such as "secret") is held by
// no decision and is false, as is a malformed term: one corrupt stored record
// must not fail a whole list, so it is withheld rather than reported.
//
// Visibility is deliberately coarser than authorization. It reads only the
// operation-invariant fields of the decision (authorization domain, permitted
// source and policy IDs, service role) and never Authorize, preflight or the
// allowed operations: the consumer has already authorized the operation on
// the record, and this answers only whether the record's stored labels are
// within the reader's grants.
//
// The only errors are an invalid decision, one expired at now, and a decision
// in the wrong mode. The answer depends only on fields AuthorizationFingerprint
// already hashes and the stored labels.
func VisibilityPermittedForUser(
	decision Decision,
	labels []string,
	now time.Time,
) (bool, error) {
	cloned, err := visibilityDecision(decision, now)
	if err != nil {
		return false, err
	}
	if cloned.serviceRole != "" || cloned.serviceCeilingIdentity != "" {
		return false, unauthorized()
	}
	return evaluateVisibility(cloned, labels, nil), nil
}

// VisibilityPermittedForService decides whether a trusted service decision
// may see data stored with the conjunction labels. A decision that is not a
// trusted service is an error here.
//
// Terms are evaluated as for VisibilityPermittedForUser, except that svc:<role>
// passes when role is the decision's service role, and the decision is
// additionally bounded by its ceiling, exactly as DeriveScannerAuthorizations
// bounds it: the ceiling must be configured and be the one the decision names
// (identity and role), and every term must be within it. Ceiling membership is
// exact for every term, g:<policy>:e:<N> included, matching the tablet and
// DeriveScannerAuthorizations. The older-epoch rule applies only to the
// decision's logical grants, never to the ceiling: an operator who rotates a
// policy and removes the old epoch from an account has revoked it, and the
// tablet then refuses cells at that epoch, so stored records at that epoch
// must be refused too. An unconfigured (zero) ceiling is false.
//
// A non-empty label set seen by a role whose scans require service
// visibility (every role but data_read and data_write) must also carry that
// role's own svc:<role> term. An empty, unlabelled set is visible to every
// role, as unlabelled cells are at the tablet.
//
// As for users, visibility is deliberately coarser than authorization and
// never consults the operation: DeriveScannerAuthorizations refuses a
// decision or role that does not hold its operation, and this does not.
func VisibilityPermittedForService(
	decision Decision,
	labels []string,
	ceiling ServiceCeiling,
	now time.Time,
) (bool, error) {
	cloned, err := visibilityDecision(decision, now)
	if err != nil {
		return false, err
	}
	if !cloned.TrustedService() {
		return false, unauthorized()
	}
	if !ceiling.set || ceiling.authorizations == nil ||
		ceiling.role != cloned.serviceRole ||
		ceiling.identity != cloned.serviceCeilingIdentity {
		return false, nil
	}
	return evaluateVisibility(cloned, labels, &ceiling), nil
}

// visibilityDecision is the only failing check: the decision must be valid and
// unexpired at now, the same unauthorized failure DeriveScannerAuthorizations
// returns for them. It deliberately does not go through preflight, which
// would consult the operation.
func visibilityDecision(decision Decision, now time.Time) (Decision, error) {
	cloned, err := decision.cloneValidated()
	if err != nil || now.IsZero() || !now.Before(cloned.expiresAt) {
		return Decision{}, unauthorized()
	}
	return cloned, nil
}

// evaluateVisibility is the term evaluator both modes share. ceiling is nil
// exactly in user mode; the exported entry points establish that. An empty set
// is unlabelled and visible to every decision and every role, before the
// own-svc:<role> rule, which binds only a labelled set.
func evaluateVisibility(decision Decision, labels []string, ceiling *ServiceCeiling) bool {
	if len(labels) == 0 {
		return true
	}
	carriesOwnRole := false
	for _, label := range labels {
		term, ok := parseVisibilityTerm(label)
		if !ok || !decision.grantsTerm(term) {
			return false
		}
		if ceiling != nil && !ceiling.containsTerm(label) {
			return false
		}
		if term.kind == termService && term.role == decision.serviceRole {
			carriesOwnRole = true
		}
	}
	if ceiling != nil && roleRequiresServiceVisibility(decision.serviceRole) &&
		!carriesOwnRole {
		return false
	}
	return true
}

type visibilityTermKind uint8

const (
	termDomain visibilityTermKind = iota + 1
	termSource
	termGrant
	termService
)

type visibilityTerm struct {
	kind      visibilityTermKind
	component []byte
	epoch     int64
	role      ServiceRole
}

// parseVisibilityTerm accepts exactly the canonical structured grammar that
// validateStructuredLabel accepts, and reports anything else as not ok.
func parseVisibilityTerm(label string) (visibilityTerm, bool) {
	switch {
	case strings.HasPrefix(label, "d:"):
		component, err := DecodeComponent(strings.TrimPrefix(label, "d:"))
		return visibilityTerm{kind: termDomain, component: component}, err == nil
	case strings.HasPrefix(label, "s:"):
		component, err := DecodeComponent(strings.TrimPrefix(label, "s:"))
		return visibilityTerm{kind: termSource, component: component}, err == nil
	case strings.HasPrefix(label, "g:"):
		encodedPolicy, encodedEpoch, found := strings.Cut(
			strings.TrimPrefix(label, "g:"), ":e:")
		if !found {
			return visibilityTerm{}, false
		}
		component, err := DecodeComponent(encodedPolicy)
		if err != nil {
			return visibilityTerm{}, false
		}
		epoch, err := strconv.ParseInt(encodedEpoch, 10, 64)
		if err != nil || epoch <= 0 || strconv.FormatInt(epoch, 10) != encodedEpoch {
			return visibilityTerm{}, false
		}
		return visibilityTerm{kind: termGrant, component: component, epoch: epoch}, true
	case strings.HasPrefix(label, "svc:"):
		role := ServiceRole(strings.TrimPrefix(label, "svc:"))
		return visibilityTerm{kind: termService, role: role}, role.Validate() == nil
	default:
		return visibilityTerm{}, false
	}
}

// grantsTerm is the decision half: the same domain, source and policy checks
// authorizeResource makes, with epochs not participating.
func (d Decision) grantsTerm(term visibilityTerm) bool {
	switch term.kind {
	case termDomain:
		return bytes.Equal(d.domain, term.component)
	case termSource:
		return containsBytes(d.sources, term.component)
	case termGrant:
		return containsBytes(d.policies, term.component)
	case termService:
		return d.TrustedService() && d.serviceRole == term.role
	default:
		return false
	}
}

// containsTerm is the ceiling half: exact membership for every term. A grant
// at another epoch than the ceiling holds is outside it, as at the tablet.
func (c *ServiceCeiling) containsTerm(label string) bool {
	return c.authorizations.Contains([]byte(label))
}

// IsStructuredVisibilityTerm reports whether term is a canonical structured
// grant label (d:, s:, g:<policy>:e:<epoch> or svc:<role>), the only kind a
// decision can hold. Anything else, a free-form ingest label such as "secret"
// included, is not, and the evaluators above refuse it.
func IsStructuredVisibilityTerm(term string) bool {
	_, ok := parseVisibilityTerm(term)
	return ok
}

// VisibilityTerms returns the structured grant labels this policy stamps on
// what it governs, as the term slice VisibilityPermittedForUser and
// VisibilityPermittedForService accept: d:<domain>, s:<source>,
// g:<policy>:e:<epoch> and, for a service policy, svc:<role>. They are the
// same labels ConjoinPolicies flattens and DeriveScannerAuthorizations
// derives for the policy.
func (p Policy) VisibilityTerms() ([]string, error) {
	if err := p.validate(); err != nil {
		return nil, err
	}
	labels, err := p.labels()
	if err != nil {
		return nil, err
	}
	terms := make([]string, len(labels))
	for index, label := range labels {
		terms[index] = string(label)
	}
	return terms, nil
}
