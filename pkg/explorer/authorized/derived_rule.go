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
	"time"

	"github.com/phrocker/shoal-oss/pkg/explorer/auth"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

// Extracted registrations (RegistrationExtracted) are bound to the document
// revision that asserted them. Their effective rule is their own rule AND the
// asserting document's current rule, exactly as revisionAllows governs an
// older revision: when a document is relabelled, the entities and relations
// extracted from it close with it (#570). An extracted registration whose
// document has no current registration, or whose conjoined rule cannot be
// built, is dropped, which every caller treats as unregistered and denied.
//
// Every other kind keeps its registered rule: intrinsic (document) nodes and
// edges always name the current revision, and materialized and application
// registrations are not asserted by any document. An application edge is,
// however, bound to the revisions its endpoints belonged to when it was
// connected (#695, revision_binding.go). The decision is made on
// the stored RegistrationKind, never on which fields happen to be empty.

// registrationReader is the reader a shared extracted registration is
// projected for (#700): the owner whose effective rule allows this decision
// for this operation. A nil reader is a trusted, reader-free use, which gets
// the first owner that still contributes.
type registrationReader struct {
	decision  auth.Decision
	operation auth.Operation
	now       time.Time
}

func readerFor(
	decision auth.Decision, operation auth.Operation, now time.Time,
) *registrationReader {
	return &registrationReader{decision: decision, operation: operation, now: now}
}

// ownedFields is what effectiveRegistrations reads of a registration.
type ownedFields struct {
	kind       RegistrationKind
	documentID shoal.ID
	revisionID shoal.ID
	rule       AccessRule
	coOwners   []ExtractionOwner
}

// effectiveNodeRegistrations applies the extracted-registration rule to node
// registrations, projected for reader. It runs where node registrations are
// resolved (resolveNodes, authorizedNode, the lexicon batch).
func (c *Client) effectiveNodeRegistrations(
	ctx context.Context,
	registrations map[shoal.ID]NodeRegistration,
	reader *registrationReader,
) (map[shoal.ID]NodeRegistration, error) {
	return effectiveRegistrations(c, ctx, registrations, reader,
		nodeOwnedFields, nodeWithOwner)
}

func nodeOwnedFields(registration NodeRegistration) ownedFields {
	return ownedFields{
		kind: registration.Kind, documentID: registration.DocumentID,
		revisionID: registration.RevisionID, rule: registration.Rule,
		coOwners: registration.CoOwners,
	}
}

func nodeWithOwner(registration NodeRegistration, owner ExtractionOwner) NodeRegistration {
	registration.DocumentID = owner.DocumentID
	registration.RevisionID = owner.RevisionID
	registration.Rule = owner.Rule
	registration.CoOwners = nil
	return registration
}

func edgeOwnedFields(registration EdgeRegistration) ownedFields {
	return ownedFields{
		kind: registration.Kind, documentID: registration.DocumentID,
		revisionID: registration.RevisionID, rule: registration.Rule,
		coOwners: registration.CoOwners,
	}
}

func edgeWithOwner(registration EdgeRegistration, owner ExtractionOwner) EdgeRegistration {
	registration.DocumentID = owner.DocumentID
	registration.RevisionID = owner.RevisionID
	registration.Rule = owner.Rule
	registration.CoOwners = nil
	return registration
}

// effectiveEdgeRegistrations applies the same rule to edge registrations. It
// runs where edge registrations are resolved (resolveEdges, edgeAllows), so a
// relation asserted only by a relabelled document closes even when both of
// its endpoint entities are shared with, and owned by, public documents.
func (c *Client) effectiveEdgeRegistrations(
	ctx context.Context,
	registrations map[shoal.ID]EdgeRegistration,
	reader *registrationReader,
) (map[shoal.ID]EdgeRegistration, error) {
	effective, err := effectiveRegistrations(c, ctx, registrations, reader,
		edgeOwnedFields, edgeWithOwner)
	if err != nil {
		return nil, err
	}
	// Application edges are bound to their endpoints' revisions (#695).
	return c.bindEdgeSourceRevisions(ctx, effective)
}

func effectiveRegistrations[R any](
	c *Client,
	ctx context.Context,
	registrations map[shoal.ID]R,
	reader *registrationReader,
	fields func(R) ownedFields,
	withOwner func(R, ExtractionOwner) R,
) (map[shoal.ID]R, error) {
	documentIDs := make([]shoal.ID, 0, len(registrations))
	seen := make(map[shoal.ID]struct{}, len(registrations))
	for _, registration := range registrations {
		owned := fields(registration)
		if owned.kind != RegistrationExtracted {
			continue
		}
		if _, duplicate := seen[owned.documentID]; duplicate {
			continue
		}
		seen[owned.documentID] = struct{}{}
		documentIDs = append(documentIDs, owned.documentID)
	}
	if len(documentIDs) == 0 {
		return registrations, nil
	}
	currents, err := c.policyStore.CurrentRevisions(ctx, documentIDs)
	if err != nil {
		return nil, policyCatalogReadError(ctx, err)
	}
	effective := make(map[shoal.ID]R, len(registrations))
	// A shared registration the first owner already decides (it contributes
	// and, for a reader, allows it) needs no co-owner lookup; the rest wait
	// for one batch of co-owner revision rules.
	pending := make(map[shoal.ID]*ExtractionOwner)
	var coOwned []RevisionReference
	for id, registration := range registrations {
		owned := fields(registration)
		if owned.kind != RegistrationExtracted {
			effective[id] = registration
			continue
		}
		var first *ExtractionOwner
		if rule, ok := firstOwnerRule(owned, currents); ok {
			first = &ExtractionOwner{
				DocumentID: owned.documentID, RevisionID: owned.revisionID,
				Rule: rule,
			}
		}
		if len(owned.coOwners) == 0 {
			if first != nil {
				effective[id] = withOwner(registration, *first)
			}
			continue
		}
		if first != nil && (reader == nil || readerAllows(first.Rule, reader)) {
			effective[id] = withOwner(registration, *first)
			continue
		}
		pending[id] = first
		for _, owner := range owned.coOwners {
			coOwned = append(coOwned, owner.reference())
		}
	}
	if len(pending) == 0 {
		return effective, nil
	}
	coOwnerRules, err := c.coOwnerRevisionRules(ctx, coOwned)
	if err != nil {
		return nil, err
	}
	for id, first := range pending {
		registration := registrations[id]
		owned := fields(registration)
		candidates := make([]ExtractionOwner, 0, 1+len(owned.coOwners))
		if first != nil {
			candidates = append(candidates, *first)
		}
		for _, owner := range owned.coOwners {
			if rule, ok := coOwnerRule(owner, coOwnerRules); ok {
				owner.Rule = rule
				candidates = append(candidates, owner)
			}
		}
		chosen, ok := projectOwner(candidates, reader)
		if !ok {
			continue
		}
		effective[id] = withOwner(registration, chosen)
	}
	return effective, nil
}

// readerAllows is ruleAllows for a projection: a rule that fails to evaluate
// counts as a denial (see projectOwner).
func readerAllows(rule AccessRule, reader *registrationReader) bool {
	allowed, err := ruleAllows(rule, reader.decision, reader.operation, reader.now)
	return err == nil && allowed
}

// firstOwnerRule is the effective rule of a registration's first owner: its
// own rule, AND its document's current rule when its revision is no longer
// current (#570). It is false when the document has no current registration
// or the rules cannot be conjoined.
func firstOwnerRule(
	owned ownedFields, currents map[shoal.ID]RevisionRegistration,
) (AccessRule, bool) {
	current, ok := currents[owned.documentID]
	if !ok || current.DocumentID != owned.documentID {
		return AccessRule{}, false
	}
	if current.RevisionID == owned.revisionID {
		return owned.rule, true
	}
	conjoined, err := conjoinRules(owned.rule, current.Rule)
	if err != nil {
		return AccessRule{}, false
	}
	return conjoined, true
}

// coOwnerRevisionRules loads, in bounded batches, the registered rule of each
// co-owner's revision and of its document's current revision.
func (c *Client) coOwnerRevisionRules(
	ctx context.Context, references []RevisionReference,
) (map[shoal.ID][]RevisionRule, error) {
	if len(references) == 0 {
		return nil, nil
	}
	canonical, err := canonicalRevisionReferences(references)
	if err != nil {
		return nil, inconsistentBase()
	}
	rules := make(map[shoal.ID][]RevisionRule)
	for start := 0; start < len(canonical); {
		end := chunkEnd(start, len(canonical), maxInteractionAuthorizationIDs)
		batch, err := c.policyStore.RevisionRules(ctx, canonical[start:end])
		start = end
		if err != nil {
			return nil, policyCatalogReadError(ctx, err)
		}
		for documentID, revisions := range batch {
			rules[documentID] = mergeRevisionRules(rules[documentID], revisions)
		}
	}
	return rules, nil
}

// mergeRevisionRules adds revisions a later batch returned. Should two
// batches disagree on which revision is current, both stay current, and
// coOwnerRule then requires both.
func mergeRevisionRules(known, more []RevisionRule) []RevisionRule {
	for _, rule := range more {
		found := false
		for index := range known {
			if known[index].RevisionID == rule.RevisionID {
				known[index].Current = known[index].Current || rule.Current
				found = true
				break
			}
		}
		if !found {
			known = append(known, rule)
		}
	}
	return known
}

// coOwnerRule is a co-owner's effective rule: its own rule, AND the rule its
// revision is registered under now, AND its document's current rule (#700).
// It is false when the revision is not registered, the document has no
// current revision, or the rules cannot be conjoined.
func coOwnerRule(
	owner ExtractionOwner, rules map[shoal.ID][]RevisionRule,
) (AccessRule, bool) {
	var own *RevisionRule
	var current []AccessRule
	revisions := rules[owner.DocumentID]
	for index := range revisions {
		if revisions[index].RevisionID == owner.RevisionID {
			own = &revisions[index]
		}
		if revisions[index].Current {
			current = append(current, revisions[index].Rule)
		}
	}
	if own == nil || len(current) == 0 {
		return AccessRule{}, false
	}
	rule := owner.Rule
	// Load-bearing: TestSharedOwnerHeldToItsRevisionsRegisteredRule pins
	// that a co-owner whose stored rule missed a tightening still enforces
	// it.
	required := []AccessRule{own.Rule}
	if !own.Current || len(current) > 1 {
		required = append(required, current...)
	}
	for _, component := range required {
		if ruleIncludes(rule, component) {
			continue
		}
		next, err := conjoinRules(rule, component)
		if err != nil {
			return AccessRule{}, false
		}
		rule = next
	}
	return rule, true
}

// projectOwner picks the owner a reader sees a shared registration through:
// the first, in owner order, whose effective rule allows the reader. A
// reader no owner allows, and a nil reader, get the first owner that still
// contributes; for such a reader every downstream check then denies. A rule
// that fails to evaluate counts as a denial here, so one owner's malformed
// rule cannot fail a read another owner allows; the downstream check of the
// owner returned reports it. It is false when no owner contributes at all.
func projectOwner(
	candidates []ExtractionOwner, reader *registrationReader,
) (ExtractionOwner, bool) {
	if len(candidates) == 0 {
		return ExtractionOwner{}, false
	}
	if reader == nil || len(candidates) == 1 {
		return candidates[0], true
	}
	for _, candidate := range candidates {
		if readerAllows(candidate.Rule, reader) {
			return candidate, true
		}
	}
	return candidates[0], true
}

// conjoinRules is the AND of two rules, built only through NewAccessRule.
func conjoinRules(left, right AccessRule) (AccessRule, error) {
	leftPolicies, rightPolicies := left.components(), right.components()
	if len(leftPolicies) == 0 || len(rightPolicies) == 0 {
		return AccessRule{}, inconsistentBase()
	}
	return NewAccessRule(append(leftPolicies, rightPolicies...)...)
}
