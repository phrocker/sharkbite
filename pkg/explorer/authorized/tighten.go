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
	"bytes"
	"context"
	"sort"

	"github.com/phrocker/shoal-oss/pkg/explorer/auth"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

// tightenChanges lists what one TightenRule call rewrote, so the durable
// store can persist exactly those records in one batch. Document-kind node
// and intrinsic-edge projections are not listed: they are rebuilt from the
// current revision on load.
type tightenChanges struct {
	revisions []revisionKey
	nodes     []shoal.ID
	edges     []shoal.ID
	// nodeOwners and edgeOwners are the co-owners (#700) whose rules were
	// rewritten; their registrations' first owners are listed in nodes and
	// edges only when those changed too.
	nodeOwners  []ownerKey
	edgeOwners  []ownerKey
	sourceClaim string
}

// ownerKey names one co-owner of one shared registration.
type ownerKey struct {
	objectID   shoal.ID
	documentID shoal.ID
	revisionID shoal.ID
}

func (c tightenChanges) changed() bool {
	return len(c.revisions) > 0 || len(c.nodes) > 0 || len(c.edges) > 0 ||
		len(c.nodeOwners) > 0 || len(c.edgeOwners) > 0 ||
		c.sourceClaim != ""
}

// RuleTightening is one TightenRule request.
type RuleTightening struct {
	DocumentID shoal.ID
	RevisionID shoal.ID
	// SourceURI names the document's source claim. It is required when
	// RevisionID is the current revision, because the catalog keeps no
	// document-to-URI index.
	SourceURI string
	From, To  AccessRule
	// AssertedEdgeIDs are relation edges the base's own extraction records
	// attribute to this document (to RevisionID when it is historical). They
	// name legacy relations the catalog cannot tie to the document: an edge
	// persisted before RegistrationKind existed decodes as
	// RegistrationApplication with no document. Each one registered as
	// RegistrationApplication takes the delta; one registered as
	// RegistrationExtracted takes it only on the owners of it (first owner
	// or co-owner, #700) the tightening covers, like every extracted edge.
	AssertedEdgeIDs []shoal.ID
}

// TighteningIndex maps each document to its registrations, so TightenRule
// visits only one document's records instead of scanning the catalog. It is
// built once, by PolicyStore.TighteningIndex, for a run that holds the
// mutation lease throughout: TightenRule only rewrites rules, never which
// registrations exist, so the index stays exact for the whole run. Every
// entry is re-checked against the stored registration (kind, document,
// revision, endpoints) before it is touched, so a stale entry can never widen
// scope.
type TighteningIndex struct {
	revisions map[shoal.ID][]shoal.ID
	// extractedNodes and extractedEdges group each extracted registration
	// under every document that owns it, first owner or co-owner (#700).
	extractedNodes map[shoal.ID][]shoal.ID
	extractedEdges map[shoal.ID][]shoal.ID
	// applicationEdges groups each application edge under the document that
	// solely owns an extracted endpoint of it (both, when they differ).
	applicationEdges map[shoal.ID][]shoal.ID
}

// Revisions returns the revision IDs the catalog registers for a document,
// ordered by ID.
func (i *TighteningIndex) Revisions(documentID shoal.ID) []shoal.ID {
	if i == nil {
		return nil
	}
	return append([]shoal.ID(nil), i.revisions[documentID]...)
}

// TighteningIndex builds the per-run index in one pass over the catalog.
func (s *MemoryPolicyStore) TighteningIndex(ctx context.Context) (*TighteningIndex, error) {
	if err := contextFailure(ctx); err != nil {
		return nil, err
	}
	if s == nil {
		return nil, catalogUnavailable()
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.buildTighteningIndexLocked(""), nil
}

// buildTighteningIndexLocked indexes every document, or only the document
// named by only when it is not empty (a TightenRule call without an index).
func (s *MemoryPolicyStore) buildTighteningIndexLocked(only shoal.ID) *TighteningIndex {
	index := &TighteningIndex{
		revisions:        make(map[shoal.ID][]shoal.ID),
		extractedNodes:   make(map[shoal.ID][]shoal.ID),
		extractedEdges:   make(map[shoal.ID][]shoal.ID),
		applicationEdges: make(map[shoal.ID][]shoal.ID),
	}
	wanted := func(documentID shoal.ID) bool {
		return only == "" || documentID == only
	}
	for key := range s.revisions {
		if wanted(key.documentID) {
			index.revisions[key.documentID] = append(
				index.revisions[key.documentID], key.revisionID)
		}
	}
	for _, revisions := range index.revisions {
		sortIDs(revisions)
	}
	// ownerDocuments is every distinct document owning a registration.
	ownerDocuments := func(primary shoal.ID, coOwners []ExtractionOwner) []shoal.ID {
		documents := []shoal.ID{primary}
		if len(coOwners) == 0 {
			return documents
		}
		known := map[shoal.ID]struct{}{primary: {}}
		for _, owner := range coOwners {
			if _, duplicate := known[owner.DocumentID]; !duplicate {
				known[owner.DocumentID] = struct{}{}
				documents = append(documents, owner.DocumentID)
			}
		}
		return documents
	}
	for nodeID, registration := range s.nodes {
		if registration.Kind != RegistrationExtracted {
			continue
		}
		for _, documentID := range ownerDocuments(
			registration.DocumentID, registration.CoOwners) {
			if wanted(documentID) {
				index.extractedNodes[documentID] = append(
					index.extractedNodes[documentID], nodeID)
			}
		}
	}
	// A shared endpoint (#700) is not a sole owner's: other owners' readers
	// read an edge touching it, so one owner's tightening must not close it.
	extractedOwner := func(nodeID shoal.ID) (shoal.ID, bool) {
		registration, ok := s.nodes[nodeID]
		if !ok || registration.Kind != RegistrationExtracted ||
			len(registration.CoOwners) > 0 ||
			!wanted(registration.DocumentID) {
			return "", false
		}
		return registration.DocumentID, true
	}
	for edgeID, registration := range s.edges {
		switch registration.Kind {
		case RegistrationExtracted:
			for _, documentID := range ownerDocuments(
				registration.DocumentID, registration.CoOwners) {
				if wanted(documentID) {
					index.extractedEdges[documentID] = append(
						index.extractedEdges[documentID], edgeID)
				}
			}
		case RegistrationApplication:
			fromOwner, fromOK := extractedOwner(registration.Edge.From)
			toOwner, toOK := extractedOwner(registration.Edge.To)
			if fromOK {
				index.applicationEdges[fromOwner] = append(
					index.applicationEdges[fromOwner], edgeID)
			}
			if toOK && (!fromOK || toOwner != fromOwner) {
				index.applicationEdges[toOwner] = append(
					index.applicationEdges[toOwner], edgeID)
			}
		}
	}
	return index
}

// TightenRule narrows the catalog rule of one document (#570). It is how the
// startup label migration closes documents that were labelled before labels
// were enforced: their registrations carry the bare source rule, and To adds
// the label policies.
//
// It refuses (InvalidArgument) unless To's components are a strict superset
// of From's, so it can never widen. The added components (the delta, To
// minus From) are conjoined onto every registration in scope through
// NewAccessRule, which only ever adds conjuncts; a registration that already
// carries the delta is left alone, which makes a repeated call a no-op.
//
// The revision registration (DocumentID, RevisionID) must exist (NotFound)
// and its rule must be From or already include To (Conflict otherwise), so a
// caller acting on a stale read cannot rewrite a document that changed
// underneath it. What is in scope depends on which revision that is:
//
//   - RevisionID is the document's current revision: every revision
//     registration of the document, historical ones included; the current
//     node and intrinsic-edge projections (rebuilt from the tightened current
//     revision); every RegistrationExtracted node and edge bound to the
//     document, whatever revision asserted it, where the document may be its
//     first owner or a co-owner (#700) and only that owner's rule takes the
//     delta, never another owner's; the application edges described
//     below; and the committed or pending source claim for SourceURI, whose
//     Rule and PreviousRule both take the delta and whose version advances. A
//     URI with no claim is not an error; a claim held by an in-flight
//     mutation conflicts.
//   - RevisionID is a historical revision: that revision registration, and
//     the extracted nodes and edges bound to exactly that revision (as first
//     owner or co-owner), plus the
//     application edges described below. SourceURI is ignored. This lets a
//     caller translate a historical revision's own labels, which may differ
//     from the current revision's.
//
// Registrations are selected by their stored RegistrationKind, never by an ID
// prefix. RegistrationMaterialized nodes and edges belong to no document and
// are never touched, nor are pending edge reservations (a reservation still
// holding the old rule can only conflict when it is committed).
//
// Application edges. An edge persisted before RegistrationKind existed
// decodes as RegistrationApplication even when ExtractDocument wrote it, and
// carries no document, so the catalog alone cannot tell a legacy extracted
// relation from a Connect edge. Two sets of them take the delta:
//
//   - every application edge with an endpoint among the in-scope extracted
//     nodes that the document owns alone. Reading such an edge already
//     requires reading that endpoint, so this changes no read; it keeps the
//     stored rule self-describing. An endpoint with co-owners (#700) is read
//     through other documents too, so tightening its edges would change their
//     readers' reads; the edge's revision binding (#695) already holds it to
//     the revision its connector read the endpoint through.
//   - every application edge named in AssertedEdgeIDs: the relations the
//     base's extraction records say this document asserted. This is what
//     closes a relation that only this document states between two entities
//     first extracted by OTHER documents: its endpoints stay visible, so
//     nothing else would hide it. If another, unlabelled document asserts the
//     same relation, the relation closes anyway (fail closed: such a legacy
//     registration records no other asserter).
//
// Either can only narrow. The cost is that retrying an identical Connect
// afterwards conflicts, because the stored rule no longer equals the one the
// selector picks.
//
// A nil index makes the call build one itself, a full pass over the catalog;
// a migration run passes the index it built once. The call is atomic: every
// rewritten registration is derived before any is stored, under one lock, so
// a failure changes nothing.
func (s *MemoryPolicyStore) TightenRule(
	ctx context.Context,
	index *TighteningIndex,
	tightening RuleTightening,
) (bool, error) {
	changes, err := s.tightenRule(ctx, index, tightening)
	if err != nil {
		return false, err
	}
	return changes.changed(), nil
}

func (s *MemoryPolicyStore) tightenRule(
	ctx context.Context,
	index *TighteningIndex,
	tightening RuleTightening,
) (tightenChanges, error) {
	documentID, revisionID := tightening.DocumentID, tightening.RevisionID
	sourceURI := tightening.SourceURI
	if err := contextFailure(ctx); err != nil {
		return tightenChanges{}, err
	}
	if err := shoal.ValidateRequiredID("document ID", documentID); err != nil {
		return tightenChanges{}, err
	}
	if err := shoal.ValidateRequiredID("revision ID", revisionID); err != nil {
		return tightenChanges{}, err
	}
	for _, edgeID := range tightening.AssertedEdgeIDs {
		if err := shoal.ValidateRequiredID("asserted edge ID", edgeID); err != nil {
			return tightenChanges{}, err
		}
	}
	fromRule, err := tightening.From.clone()
	if err != nil {
		return tightenChanges{}, err
	}
	toRule, err := tightening.To.clone()
	if err != nil {
		return tightenChanges{}, err
	}
	delta, err := strictRuleExtension(fromRule, toRule)
	if err != nil {
		return tightenChanges{}, err
	}
	if s == nil {
		return tightenChanges{}, catalogUnavailable()
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	s.initialize()
	key := revisionKey{documentID: documentID, revisionID: revisionID}
	target, ok := s.revisions[key]
	if !ok {
		return tightenChanges{}, shoal.NewError(
			shoal.ErrorNotFound, "revision registration is not in the catalog")
	}
	if !target.Rule.equal(fromRule) && !ruleIncludes(target.Rule, toRule) {
		return tightenChanges{}, catalogConflict()
	}
	currentKey, hasCurrent := s.current[documentID]
	whole := hasCurrent && currentKey == key
	if whole {
		if err := validateSourceURI(sourceURI); err != nil {
			return tightenChanges{}, err
		}
	}
	if index == nil {
		index = s.buildTighteningIndexLocked(documentID)
	}
	inScope := func(document, revision shoal.ID) bool {
		return document == documentID && (whole || revision == revisionID)
	}

	// Derive everything first; nothing is stored until all of it is built.
	var changes tightenChanges
	revisions := make(map[revisionKey]RevisionRegistration)
	for _, candidateRevision := range index.revisions[documentID] {
		candidate := revisionKey{documentID: documentID, revisionID: candidateRevision}
		registration, ok := s.revisions[candidate]
		if !ok || !inScope(candidate.documentID, candidate.revisionID) {
			continue
		}
		rule, changed, err := conjoinDelta(registration.Rule, delta)
		if err != nil {
			return tightenChanges{}, err
		}
		if !changed {
			continue
		}
		tightened := cloneRevisionRegistration(registration)
		tightened.Rule = rule
		revisions[candidate] = tightened
		changes.revisions = append(changes.revisions, candidate)
	}
	nodes := make(map[shoal.ID]NodeRegistration)
	scoped := make(map[shoal.ID]struct{})
	for _, nodeID := range index.extractedNodes[documentID] {
		if _, done := nodes[nodeID]; done {
			continue
		}
		registration, ok := s.nodes[nodeID]
		if !ok || registration.Kind != RegistrationExtracted {
			continue
		}
		primaryInScope := inScope(registration.DocumentID, registration.RevisionID)
		if primaryInScope && len(registration.CoOwners) == 0 {
			scoped[nodeID] = struct{}{}
		}
		tightened, primaryChanged, owners, err := tightenOwners(
			nodeID, registration.Rule, registration.CoOwners,
			primaryInScope, inScope, delta)
		if err != nil {
			return tightenChanges{}, err
		}
		if !primaryChanged && len(owners) == 0 {
			continue
		}
		next, err := cloneNodeRegistration(registration)
		if err != nil {
			return tightenChanges{}, catalogUnavailable()
		}
		next.Rule = tightened.rule
		next.CoOwners = tightened.coOwners
		nodes[nodeID] = next
		if primaryChanged {
			changes.nodes = append(changes.nodes, nodeID)
		}
		changes.nodeOwners = append(changes.nodeOwners, owners...)
	}
	// Candidate edges: extracted edges bound to the document, application
	// edges touching an in-scope extracted node, and the asserted edges.
	type edgeCandidate struct {
		id       shoal.ID
		asserted bool
	}
	var candidates []edgeCandidate
	for _, edgeID := range index.extractedEdges[documentID] {
		candidates = append(candidates, edgeCandidate{id: edgeID})
	}
	// The application edges' endpoints are re-checked against the scoped
	// nodes below, so an edge indexed under this document but touching only
	// another revision's nodes is left alone.
	for _, edgeID := range index.applicationEdges[documentID] {
		candidates = append(candidates, edgeCandidate{id: edgeID})
	}
	for _, edgeID := range tightening.AssertedEdgeIDs {
		candidates = append(candidates, edgeCandidate{id: edgeID, asserted: true})
	}
	edges := make(map[shoal.ID]EdgeRegistration)
	for _, candidate := range candidates {
		if _, done := edges[candidate.id]; done {
			continue
		}
		registration, ok := s.edges[candidate.id]
		if !ok {
			continue
		}
		switch registration.Kind {
		case RegistrationExtracted:
			// Every owner of it this tightening covers takes the delta
			// (#700); others keep their rules.
			tightened, primaryChanged, owners, err := tightenOwners(
				candidate.id, registration.Rule, registration.CoOwners,
				inScope(registration.DocumentID, registration.RevisionID),
				inScope, delta)
			if err != nil {
				return tightenChanges{}, err
			}
			if !primaryChanged && len(owners) == 0 {
				continue
			}
			next, err := cloneEdgeRegistrationChecked(registration)
			if err != nil {
				return tightenChanges{}, catalogUnavailable()
			}
			next.Rule = tightened.rule
			next.CoOwners = tightened.coOwners
			edges[candidate.id] = next
			if primaryChanged {
				changes.edges = append(changes.edges, candidate.id)
			}
			changes.edgeOwners = append(changes.edgeOwners, owners...)
			continue
		case RegistrationApplication:
			_, fromScoped := scoped[registration.Edge.From]
			_, toScoped := scoped[registration.Edge.To]
			if !candidate.asserted && !fromScoped && !toScoped {
				continue
			}
		default:
			continue
		}
		rule, changed, err := conjoinDelta(registration.Rule, delta)
		if err != nil {
			return tightenChanges{}, err
		}
		if !changed {
			continue
		}
		tightened, err := cloneEdgeRegistrationChecked(registration)
		if err != nil {
			return tightenChanges{}, catalogUnavailable()
		}
		tightened.Rule = rule
		edges[candidate.id] = tightened
		changes.edges = append(changes.edges, candidate.id)
	}
	var claim *sourceClaimState
	if whole {
		if state, exists := s.sourceClaims[sourceURI]; exists {
			if state.held {
				return tightenChanges{}, catalogConflict()
			}
			tightened, changed, err := tightenSourceClaim(state.claim, delta)
			if err != nil {
				return tightenChanges{}, err
			}
			if changed {
				if s.sourceVersion == ^uint64(0) {
					return tightenChanges{}, catalogUnavailable()
				}
				claim = &sourceClaimState{claim: tightened}
				changes.sourceClaim = sourceURI
			}
		}
	}

	// Store.
	for candidate, registration := range revisions {
		s.revisions[candidate] = registration
	}
	if whole && len(revisions) > 0 {
		if current, ok := revisions[key]; ok {
			s.replaceCurrent(key, current)
		}
	}
	for nodeID, registration := range nodes {
		s.nodes[nodeID] = registration
	}
	for edgeID, registration := range edges {
		s.edges[edgeID] = registration
	}
	if claim != nil {
		s.sourceVersion++
		claim.claim.Version = s.sourceVersion
		s.sourceClaims[sourceURI] = *claim
	}
	sort.Slice(changes.revisions, func(left, right int) bool {
		return shoal.CompareID(changes.revisions[left].revisionID,
			changes.revisions[right].revisionID) < 0
	})
	sortIDs(changes.nodes)
	sortIDs(changes.edges)
	sortOwnerKeys(changes.nodeOwners)
	sortOwnerKeys(changes.edgeOwners)
	return changes, nil
}

// tightenedOwners is a shared registration's rules after a tightening.
type tightenedOwners struct {
	rule     AccessRule
	coOwners []ExtractionOwner
}

// tightenOwners conjoins delta onto the first owner's rule when it is in
// scope, and onto every co-owner in scope (#700). It reports whether the
// first owner's rule changed and which co-owners' did.
func tightenOwners(
	objectID shoal.ID,
	rule AccessRule,
	coOwners []ExtractionOwner,
	primaryInScope bool,
	inScope func(document, revision shoal.ID) bool,
	delta []auth.Policy,
) (tightenedOwners, bool, []ownerKey, error) {
	result := tightenedOwners{rule: rule}
	primaryChanged := false
	if primaryInScope {
		next, changed, err := conjoinDelta(rule, delta)
		if err != nil {
			return tightenedOwners{}, false, nil, err
		}
		result.rule, primaryChanged = next, changed
	}
	owners, err := cloneOwners(coOwners)
	if err != nil {
		return tightenedOwners{}, false, nil, catalogUnavailable()
	}
	var changedOwners []ownerKey
	for index, owner := range owners {
		if !inScope(owner.DocumentID, owner.RevisionID) {
			continue
		}
		next, changed, err := conjoinDelta(owner.Rule, delta)
		if err != nil {
			return tightenedOwners{}, false, nil, err
		}
		if !changed {
			continue
		}
		owners[index].Rule = next
		changedOwners = append(changedOwners, ownerKey{
			objectID: objectID, documentID: owner.DocumentID,
			revisionID: owner.RevisionID,
		})
	}
	result.coOwners = owners
	return result, primaryChanged, changedOwners, nil
}

func sortOwnerKeys(keys []ownerKey) {
	sort.Slice(keys, func(left, right int) bool {
		if compared := shoal.CompareID(keys[left].objectID, keys[right].objectID); compared != 0 {
			return compared < 0
		}
		return compareRevisionReferences(
			RevisionReference{DocumentID: keys[left].documentID, RevisionID: keys[left].revisionID},
			RevisionReference{DocumentID: keys[right].documentID, RevisionID: keys[right].revisionID},
		) < 0
	})
}

// LabelMigration returns the recorded label-migration marker.
func (s *MemoryPolicyStore) LabelMigration(
	ctx context.Context,
) (LabelMigrationRecord, bool, error) {
	if err := contextFailure(ctx); err != nil {
		return LabelMigrationRecord{}, false, err
	}
	if s == nil {
		return LabelMigrationRecord{}, false, catalogUnavailable()
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.labelMigration == nil {
		return LabelMigrationRecord{}, false, nil
	}
	return s.labelMigration.clone(), true, nil
}

// PutLabelMigration records the label-migration marker, replacing any
// earlier one.
func (s *MemoryPolicyStore) PutLabelMigration(
	ctx context.Context,
	record LabelMigrationRecord,
) error {
	if err := contextFailure(ctx); err != nil {
		return err
	}
	if record.Version == 0 {
		return shoal.NewError(
			shoal.ErrorInvalidArgument, "label migration version is required")
	}
	if s == nil {
		return catalogUnavailable()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	cloned := record.clone()
	s.labelMigration = &cloned
	return nil
}

// strictRuleExtension returns the components of `to` that `from` lacks,
// refusing unless every component of `from` is in `to` and `to` has at least
// one more. Components compare by logical identity, as AccessRule equality
// does.
func strictRuleExtension(from, to AccessRule) ([]auth.Policy, error) {
	if !ruleIncludes(to, from) {
		return nil, shoal.NewError(shoal.ErrorInvalidArgument,
			"refusing to change an access rule: the new rule does not keep "+
				"every component of the old one, so it would widen access")
	}
	if len(to.keys) <= len(from.keys) {
		return nil, shoal.NewError(shoal.ErrorInvalidArgument,
			"refusing to change an access rule: the new rule adds no component")
	}
	delta := make([]auth.Policy, 0, len(to.keys)-len(from.keys))
	for index, key := range to.keys {
		if !ruleHasKey(from, key) {
			delta = append(delta, to.policies[index])
		}
	}
	return delta, nil
}

// ruleIncludes reports whether every component of inner is a component of
// outer, so that outer is at least as narrow as inner.
func ruleIncludes(outer, inner AccessRule) bool {
	if len(inner.keys) == 0 || len(outer.keys) == 0 {
		return false
	}
	for _, key := range inner.keys {
		if !ruleHasKey(outer, key) {
			return false
		}
	}
	return true
}

func ruleHasKey(rule AccessRule, key []byte) bool {
	for _, candidate := range rule.keys {
		if bytes.Equal(candidate, key) {
			return true
		}
	}
	return false
}

// tightenSourceClaim narrows a source claim. A committed claim's Rule takes
// the delta. A pending claim is an interrupted mutation whose retry must
// select exactly its Rule (sourceClaimAllowsMutation), and that Rule was
// chosen by the ingest itself, under its own labels. So the delta goes onto
// its PreviousRule, which the retry must ALSO satisfy, and Rule is left
// alone: the retry stays possible for a principal holding the old and the
// new labels (the relabel rule), and impossible for a source-only one. A
// pending claim with no PreviousRule gains one, the narrowed Rule.
func tightenSourceClaim(
	claim SourcePolicyClaim,
	delta []auth.Policy,
) (SourcePolicyClaim, bool, error) {
	tightened, err := cloneSourcePolicyClaim(claim)
	if err != nil {
		return SourcePolicyClaim{}, false, catalogUnavailable()
	}
	if !claim.Pending {
		rule, changed, err := conjoinDelta(claim.Rule, delta)
		if err != nil || !changed {
			return claim, false, err
		}
		tightened.Rule = rule
		return tightened, true, nil
	}
	base := claim.Rule
	if claim.PreviousRule != nil {
		base = *claim.PreviousRule
	}
	previous, changed, err := conjoinDelta(base, delta)
	if err != nil {
		return SourcePolicyClaim{}, false, err
	}
	if !changed && claim.PreviousRule != nil {
		return claim, false, nil
	}
	if !changed && ruleIncludes(claim.Rule, previous) {
		// A pending claim with no previous rule whose Rule already carries
		// the delta needs nothing.
		return claim, false, nil
	}
	tightened.PreviousRule = &previous
	return tightened, true, nil
}

// conjoinDelta adds the delta's components to rule through NewAccessRule and
// reports whether that changed it. It can only add conjuncts.
func conjoinDelta(rule AccessRule, delta []auth.Policy) (AccessRule, bool, error) {
	// Already carrying every delta component (the common case on every start
	// after the first): compare logical keys and skip rebuilding the rule.
	carried := len(rule.keys) > 0
	for _, policy := range delta {
		if !ruleHasKey(rule, logicalPolicyKey(policy)) {
			carried = false
			break
		}
	}
	if carried {
		return rule, false, nil
	}
	components := rule.components()
	if len(components) == 0 {
		return AccessRule{}, false, catalogUnavailable()
	}
	narrowed, err := NewAccessRule(append(components, delta...)...)
	if err != nil {
		return AccessRule{}, false, err
	}
	if narrowed.equal(rule) {
		return rule, false, nil
	}
	return narrowed, true, nil
}

func sortIDs(ids []shoal.ID) {
	sort.Slice(ids, func(left, right int) bool {
		return shoal.CompareID(ids[left], ids[right]) < 0
	})
}
