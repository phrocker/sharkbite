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
	"sort"

	"github.com/phrocker/shoal-oss/pkg/shoal"
)

// Derived outputs are bound to the revisions they were derived from (#695).
//
// An older document revision is readable only under its own rule AND the
// document's current rule (revisionAllows). Document and root-section node
// IDs do not depend on the revision, so a new revision takes over those node
// registrations; anything authorized only through the touched nodes' current
// registrations therefore followed the current rule alone. After an
// authorized relabel that removed a label, the labelled revision stayed
// closed while every interaction recorded over it, every fold of those
// interactions, and every application edge connected to it while it was
// labelled, opened.
//
// Two derived outputs are now bound:
//
//   - An interaction session records, at write, the revision of every
//     document its touched nodes and edges belonged to, and every revision its
//     document evidence cited (PutInteractionRevisions). A read additionally
//     requires revisionAllows for each recorded revision. Folds inherit this
//     through foldVisible, which reads every member through InteractionRecord.
//   - An application edge records, at Connect, the revision of each endpoint's
//     document (EdgeRegistration.SourceRevisions). Its effective rule conjoins
//     each recorded revision's rule and, when that revision is no longer
//     current, the document's current rule, as RegistrationExtracted does. A
//     recorded revision that is unregistered, or whose document has no current
//     revision, drops the edge (denied).
//
// Outputs recorded before this binding existed carry no recorded revisions.
// They are held to the rule of the revisions of each document their inputs
// now belong to that they could have been derived from, and never less than
// before the binding existed:
//
//   - A legacy session is held to every revision the trusted corpus
//     (PublicationReader) published up to the one that was current at its
//     snapshot, plus the current revision. The session's SnapshotAsOf was
//     validated against a snapshot the corpus took when the session was
//     written, and that AsOf is no earlier than the publication time of
//     every revision then current. The wall clock only picks the newest
//     revision published at or before AsOf; its publication sequence, which
//     the corpus assigns monotonically, bounds the rest (sequenceBound), so a
//     clock step between two publications cannot drop the revision the
//     session saw or any before it. A revision with no sequence counts, as
//     does every revision when nothing bounds them.
//   - A legacy application edge is held to every registered revision of each
//     endpoint's document. Its Connect time and a revision's publication time
//     are unrelated wall-clock readings, so no time bound is safe. An unbound
//     application edge the corpus holds no Connect record for was never
//     connected: it is an extracted or materialized edge persisted before
//     RegistrationKind existed (legacyEdgeKind), and is left as registered.
//   - Without a PublicationReader both count every revision, and every
//     unbound application edge counts as connected.
//
// The legacy rule reads a document's whole revision history (once per
// document per read), so its cost grows with that history; only outputs
// recorded before this change pay it.
//
// Bindings are never removed: a session's binding row outlives its
// tombstone, which keeps no source content and is not checked against it.
//
// Denials are indistinguishable from an object that was never written: every
// read surface maps them to the same ObjectNotFound (#398).

// RevisionReference names one exact document revision.
type RevisionReference struct {
	DocumentID shoal.ID
	RevisionID shoal.ID
}

// RevisionRule is the rule of one registered revision of a document.
type RevisionRule struct {
	RevisionID shoal.ID
	Rule       AccessRule
	Current    bool
}

func compareRevisionReferences(left, right RevisionReference) int {
	if compared := shoal.CompareID(left.DocumentID, right.DocumentID); compared != 0 {
		return compared
	}
	return shoal.CompareID(left.RevisionID, right.RevisionID)
}

// canonicalRevisionReferences validates, sorts and deduplicates references.
// The result is nil for an empty input.
func canonicalRevisionReferences(
	references []RevisionReference,
) ([]RevisionReference, error) {
	if len(references) == 0 {
		return nil, nil
	}
	canonical := make([]RevisionReference, 0, len(references))
	for _, reference := range references {
		if err := shoal.ValidateRequiredID(
			"source document ID", reference.DocumentID); err != nil {
			return nil, err
		}
		if err := shoal.ValidateRequiredID(
			"source revision ID", reference.RevisionID); err != nil {
			return nil, err
		}
		canonical = append(canonical, reference)
	}
	sort.Slice(canonical, func(left, right int) bool {
		return compareRevisionReferences(canonical[left], canonical[right]) < 0
	})
	deduplicated := canonical[:1]
	for _, reference := range canonical[1:] {
		if reference != deduplicated[len(deduplicated)-1] {
			deduplicated = append(deduplicated, reference)
		}
	}
	return deduplicated, nil
}

func revisionReferencesEqual(left, right []RevisionReference) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}

// unionRevisionReferences is the canonical union of two canonical sets.
func unionRevisionReferences(
	left, right []RevisionReference,
) ([]RevisionReference, error) {
	return canonicalRevisionReferences(
		append(append([]RevisionReference(nil), left...), right...))
}

// DocumentRevisionRules returns every registered revision's rule for each
// requested document, ordered by revision ID, with the current one marked.
// Each identifier is validated in request order; repeated identifiers are
// resolved once and documents with no revision are omitted.
func (s *MemoryPolicyStore) DocumentRevisionRules(
	ctx context.Context,
	documentIDs []shoal.ID,
) (map[shoal.ID][]RevisionRule, error) {
	if err := contextFailure(ctx); err != nil {
		return nil, err
	}
	for _, documentID := range documentIDs {
		if err := shoal.ValidateRequiredID("document ID", documentID); err != nil {
			return nil, err
		}
	}
	if s == nil {
		return nil, catalogUnavailable()
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	resolved := make(map[shoal.ID][]RevisionRule, len(documentIDs))
	for _, documentID := range documentIDs {
		if _, done := resolved[documentID]; done {
			continue
		}
		revisionIDs := s.documentRevisions[documentID]
		if len(revisionIDs) == 0 {
			continue
		}
		current, hasCurrent := s.current[documentID]
		rules := make([]RevisionRule, 0, len(revisionIDs))
		for _, revisionID := range revisionIDs {
			key := revisionKey{documentID: documentID, revisionID: revisionID}
			registration, ok := s.revisions[key]
			if !ok {
				return nil, catalogUnavailable()
			}
			rule, err := registration.Rule.clone()
			if err != nil {
				return nil, catalogUnavailable()
			}
			rules = append(rules, RevisionRule{
				RevisionID: revisionID,
				Rule:       rule,
				Current:    hasCurrent && current == key,
			})
		}
		sort.Slice(rules, func(left, right int) bool {
			return shoal.CompareID(rules[left].RevisionID, rules[right].RevisionID) < 0
		})
		resolved[documentID] = rules
	}
	return resolved, nil
}

// RevisionRules returns, for each document a reference names, the rules of
// its referenced revisions that are registered and of its current revision,
// ordered by revision ID: what revisionAllows needs for each reference,
// without the rest of a long revision history. Each reference is validated
// in request order; a document with neither is omitted.
func (s *MemoryPolicyStore) RevisionRules(
	ctx context.Context,
	references []RevisionReference,
) (map[shoal.ID][]RevisionRule, error) {
	if err := contextFailure(ctx); err != nil {
		return nil, err
	}
	for _, reference := range references {
		if _, err := canonicalRevisionReferences(
			[]RevisionReference{reference}); err != nil {
			return nil, err
		}
	}
	if s == nil {
		return nil, catalogUnavailable()
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	wanted := make(map[shoal.ID]map[shoal.ID]struct{}, len(references))
	for _, reference := range references {
		revisions, ok := wanted[reference.DocumentID]
		if !ok {
			revisions = make(map[shoal.ID]struct{}, 2)
			wanted[reference.DocumentID] = revisions
			if current, ok := s.current[reference.DocumentID]; ok {
				revisions[current.revisionID] = struct{}{}
			}
		}
		revisions[reference.RevisionID] = struct{}{}
	}
	resolved := make(map[shoal.ID][]RevisionRule, len(wanted))
	for documentID, revisions := range wanted {
		current, hasCurrent := s.current[documentID]
		rules := make([]RevisionRule, 0, len(revisions))
		for revisionID := range revisions {
			key := revisionKey{documentID: documentID, revisionID: revisionID}
			registration, ok := s.revisions[key]
			if !ok {
				continue
			}
			rule, err := registration.Rule.clone()
			if err != nil {
				return nil, catalogUnavailable()
			}
			rules = append(rules, RevisionRule{
				RevisionID: revisionID,
				Rule:       rule,
				Current:    hasCurrent && current == key,
			})
		}
		if len(rules) == 0 {
			continue
		}
		sort.Slice(rules, func(left, right int) bool {
			return shoal.CompareID(rules[left].RevisionID, rules[right].RevisionID) < 0
		})
		resolved[documentID] = rules
	}
	return resolved, nil
}

// PutInteractionRevisions unions references into what is recorded for the
// session. An empty set records nothing.
func (s *MemoryPolicyStore) PutInteractionRevisions(
	ctx context.Context,
	sessionID shoal.ID,
	references []RevisionReference,
) error {
	_, err := s.mergeInteractionRevisions(ctx, sessionID, references, true)
	return err
}

// mergeInteractionRevisions computes, and when apply is set stores, the union
// of what is recorded for the session with references. It reports the union,
// or nil when there is nothing to record.
func (s *MemoryPolicyStore) mergeInteractionRevisions(
	ctx context.Context,
	sessionID shoal.ID,
	references []RevisionReference,
	apply bool,
) ([]RevisionReference, error) {
	if err := contextFailure(ctx); err != nil {
		return nil, err
	}
	if err := shoal.ValidateRequiredID("interaction session ID", sessionID); err != nil {
		return nil, err
	}
	canonical, err := canonicalRevisionReferences(references)
	if err != nil {
		return nil, err
	}
	if s == nil {
		return nil, catalogUnavailable()
	}
	if len(canonical) == 0 {
		return nil, nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.initialize()
	merged, err := unionRevisionReferences(
		s.interactionRevisions[sessionID], canonical)
	if err != nil {
		return nil, catalogUnavailable()
	}
	if apply {
		s.interactionRevisions[sessionID] = merged
	}
	return append([]RevisionReference(nil), merged...), nil
}

// InteractionRevisions returns the recorded revisions of each requested
// session; sessions with no recording are omitted.
func (s *MemoryPolicyStore) InteractionRevisions(
	ctx context.Context,
	sessionIDs []shoal.ID,
) (map[shoal.ID][]RevisionReference, error) {
	if err := contextFailure(ctx); err != nil {
		return nil, err
	}
	for _, sessionID := range sessionIDs {
		if err := shoal.ValidateRequiredID(
			"interaction session ID", sessionID); err != nil {
			return nil, err
		}
	}
	if s == nil {
		return nil, catalogUnavailable()
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	resolved := make(map[shoal.ID][]RevisionReference, len(sessionIDs))
	for _, sessionID := range sessionIDs {
		recorded, ok := s.interactionRevisions[sessionID]
		if !ok {
			continue
		}
		resolved[sessionID] = append([]RevisionReference(nil), recorded...)
	}
	return resolved, nil
}

func (s *MemoryPolicyStore) snapshotInteractionRevisions(
	sessionID shoal.ID,
) ([]RevisionReference, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	recorded, ok := s.interactionRevisions[sessionID]
	return append([]RevisionReference(nil), recorded...), ok
}
