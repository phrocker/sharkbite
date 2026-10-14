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
	"slices"
	"sort"

	"github.com/phrocker/shoal-oss/pkg/shoal"
)

// Shared extracted registrations (#700).
//
// An extracted entity's node ID depends only on its extraction namespace,
// type and key, and a relation's edge ID only on its type and endpoints, so
// several documents can extract the very same node or relation. The catalog
// used to keep only the FIRST extractor as the registration's owner. A reader
// who could read a later extractor, but not the first, then lost the node as
// soon as the first document was relabelled, which told them that a document
// they cannot read contains the entity (an entity-key oracle).
//
// Now every extracting document revision owns the registration. The first
// keeps the registration's DocumentID, RevisionID and Rule, as before, so a
// registration recorded before this change is simply a registration with one
// owner; every later document is recorded as a co-owner, one entry per
// document naming the revision that extracted it last. A reader sees the
// registration when ANY owner's effective rule allows it, and each owner's
// effective rule is bound to the revision that extracted it (#570, #695):
//
//   - the first owner: its own rule, AND its document's current rule when its
//     revision is no longer current (effectiveRegistrations, unchanged);
//   - a co-owner: its own rule, AND the rule its revision is registered under
//     now, AND its document's current rule. The middle term makes a co-owner
//     self-protecting: a TightenRule that its stored rule missed (a catalog
//     rolled back to a binary that does not know co-owners, then forward
//     again) is still enforced.
//
// An owner whose document has no current revision, or whose revision is not
// registered, contributes nothing. Read paths project a shared registration
// onto ONE owner the reader can read (projectOwner), so everything downstream
// (labels, revision bindings, domains) sees only a rule of a document that
// reader may read. A reader who can read no owner gets the first surviving
// owner, which denies.
//
// Owners never change the node's content, and no owner's presentation is
// part of it: a shared node or relation carries only what its identity
// determines (explorer.CanonicalExtractedNode, CanonicalExtractedRelation),
// whichever extractor produced it, so every extraction of it agrees and
// there is nothing per-owner to show or to conflict over.

// ExtractionOwner is one document revision, beyond a registration's first
// owner, that extracted a shared node or relation, with the rule it was
// extracted under.
type ExtractionOwner struct {
	DocumentID shoal.ID
	RevisionID shoal.ID
	Rule       AccessRule
}

func (o ExtractionOwner) reference() RevisionReference {
	return RevisionReference{DocumentID: o.DocumentID, RevisionID: o.RevisionID}
}

func compareOwners(left, right ExtractionOwner) int {
	return shoal.CompareID(left.DocumentID, right.DocumentID)
}

// cloneOwners copies the owner list. Rules are immutable values once stored
// (every change builds a new one), so they are shared, not rebuilt: a read
// of a popular entity copies its owners, never their rules.
func cloneOwners(owners []ExtractionOwner) ([]ExtractionOwner, error) {
	if len(owners) == 0 {
		return nil, nil
	}
	return append([]ExtractionOwner(nil), owners...), nil
}

// ownerMerge is what recording one more extraction of a shared registration
// does.
type ownerMerge uint8

const (
	ownerMergeConflict ownerMerge = iota
	// ownerMergeKeep: the extracting revision already owns it, under the
	// same rule.
	ownerMergeKeep
	// ownerMergeChanged: the extracting document became an owner, or its
	// owner entry moved to the extracting revision.
	ownerMergeChanged
)

// ownerChange is the outcome of recordOwner.
type ownerChange struct {
	merge ownerMerge
	// primary: the first owner's entry moved to the extracting revision.
	primary bool
	// owner: the co-owner entry added or moved.
	owner *ExtractionOwner
}

// recordOwner records an extraction by incoming of a registration whose
// content already matched. Owners are kept per DOCUMENT: a document's entry
// names the revision that extracted it most recently, so re-ingesting a
// popular entity's documents does not grow the list. Moving an entry only
// ever drops one disjunct of who may read the registration (the older
// revision's), so it can only narrow, never widen; outputs derived earlier
// keep the revisions they were bound to (#695), which are recorded on them,
// not here. A revision that already owns it under a different rule
// conflicts: that is the extracting document's own state, never another
// document's.
//
// coOwners is updated in place (binary search, one insertion) and returned;
// the caller stores it. Readers only ever hold copies (cloneOwners).
func recordOwner(
	primaryDocument, primaryRevision shoal.ID,
	primaryRule AccessRule,
	coOwners []ExtractionOwner,
	incoming ExtractionOwner,
) ([]ExtractionOwner, ownerChange) {
	if incoming.DocumentID == primaryDocument {
		if incoming.RevisionID != primaryRevision {
			return coOwners, ownerChange{merge: ownerMergeChanged, primary: true}
		}
		if primaryRule.equal(incoming.Rule) {
			return coOwners, ownerChange{merge: ownerMergeKeep}
		}
		return coOwners, ownerChange{merge: ownerMergeConflict}
	}
	index := sort.Search(len(coOwners), func(i int) bool {
		return compareOwners(coOwners[i], incoming) >= 0
	})
	recorded := incoming
	if index < len(coOwners) && coOwners[index].DocumentID == incoming.DocumentID {
		existing := coOwners[index]
		if existing.RevisionID == incoming.RevisionID {
			if existing.Rule.equal(incoming.Rule) {
				return coOwners, ownerChange{merge: ownerMergeKeep}
			}
			return coOwners, ownerChange{merge: ownerMergeConflict}
		}
		coOwners[index] = incoming
		return coOwners, ownerChange{merge: ownerMergeChanged, owner: &recorded}
	}
	coOwners = slices.Insert(coOwners, index, incoming)
	return coOwners, ownerChange{merge: ownerMergeChanged, owner: &recorded}
}

// canonicalOwners reports whether owners is strictly ascending by document,
// holds valid identities and rules, and never names the first owner's
// document. It is what the durable store requires of a reconstructed
// registration.
func canonicalOwners(
	primaryDocument shoal.ID,
	owners []ExtractionOwner,
) bool {
	for index, owner := range owners {
		if shoal.ValidateRequiredID("owner document ID", owner.DocumentID) != nil ||
			shoal.ValidateRequiredID("owner revision ID", owner.RevisionID) != nil {
			return false
		}
		if len(owner.Rule.components()) == 0 {
			return false
		}
		if owner.DocumentID == primaryDocument {
			return false
		}
		if index > 0 && compareOwners(owners[index-1], owner) >= 0 {
			return false
		}
	}
	return true
}
