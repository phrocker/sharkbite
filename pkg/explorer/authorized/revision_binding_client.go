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

	"github.com/phrocker/shoal-oss/pkg/explorer"
	"github.com/phrocker/shoal-oss/pkg/explorer/auth"
	"github.com/phrocker/shoal-oss/pkg/interaction"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

// The client half of the revision binding (#695); see revision_binding.go.

// documentBound reports whether a registration of kind belongs to a document
// revision: an intrinsic node or edge of it, or extraction output it asserted.
func documentBound(kind RegistrationKind) bool {
	return kind == RegistrationDocument || kind == RegistrationExtracted
}

func nodeRevision(registration NodeRegistration) (RevisionReference, bool) {
	if !documentBound(registration.Kind) ||
		registration.DocumentID == "" || registration.RevisionID == "" {
		return RevisionReference{}, false
	}
	return RevisionReference{
		DocumentID: registration.DocumentID,
		RevisionID: registration.RevisionID,
	}, true
}

func edgeRevision(registration EdgeRegistration) (RevisionReference, bool) {
	if !documentBound(registration.Kind) ||
		registration.DocumentID == "" || registration.RevisionID == "" {
		return RevisionReference{}, false
	}
	return RevisionReference{
		DocumentID: registration.DocumentID,
		RevisionID: registration.RevisionID,
	}, true
}

// connectSourceRevisions is the binding Connect records for an edge between
// two authorized endpoint registrations.
func connectSourceRevisions(
	from, to NodeRegistration,
) ([]RevisionReference, error) {
	var references []RevisionReference
	for _, endpoint := range []NodeRegistration{from, to} {
		if reference, ok := nodeRevision(endpoint); ok {
			references = append(references, reference)
		}
	}
	return canonicalRevisionReferences(references)
}

// revisionRuleCache resolves revision rules at most once per read, in
// bounded batches: a referenced revision with its document's current one
// (loadReferences), or a document's whole history (loadDocuments).
type revisionRuleCache struct {
	client     *Client
	rules      map[shoal.ID][]RevisionRule
	full       map[shoal.ID]struct{}
	references map[RevisionReference]struct{}
	// published is the corpus's publication of each revision of a fully
	// loaded document, from the trusted PublicationReader; empty when the
	// client has none.
	published map[shoal.ID]map[shoal.ID]explorer.RevisionPublication
}

func (c *Client) newRevisionRuleCache() *revisionRuleCache {
	return &revisionRuleCache{
		client:     c,
		rules:      make(map[shoal.ID][]RevisionRule),
		full:       make(map[shoal.ID]struct{}),
		references: make(map[RevisionReference]struct{}),
		published:  make(map[shoal.ID]map[shoal.ID]explorer.RevisionPublication),
	}
}

// loadDocuments loads every revision of each document, and when the client
// has a PublicationReader, when each was published.
func (r *revisionRuleCache) loadDocuments(
	ctx context.Context, documentIDs []shoal.ID,
) error {
	missing := make([]shoal.ID, 0, len(documentIDs))
	queued := make(map[shoal.ID]struct{}, len(documentIDs))
	for _, documentID := range documentIDs {
		if _, done := r.full[documentID]; done {
			continue
		}
		if _, done := queued[documentID]; done {
			continue
		}
		queued[documentID] = struct{}{}
		missing = append(missing, documentID)
	}
	for start := 0; start < len(missing); {
		end := chunkEnd(start, len(missing), maxInteractionAuthorizationIDs)
		chunk := missing[start:end]
		start = end
		batch, err := r.client.policyStore.DocumentRevisionRules(ctx, chunk)
		if err != nil {
			return policyCatalogReadError(ctx, err)
		}
		var published map[shoal.ID]map[shoal.ID]explorer.RevisionPublication
		if reader := r.client.publications; !isNilDependency(reader) {
			published, err = reader.RevisionPublications(ctx, chunk)
			if err != nil {
				return directBaseError(err)
			}
		}
		for _, documentID := range chunk {
			r.rules[documentID] = batch[documentID]
			r.full[documentID] = struct{}{}
			r.published[documentID] = published[documentID]
		}
	}
	return nil
}

// loadReferences loads each referenced revision and its document's current
// revision.
func (r *revisionRuleCache) loadReferences(
	ctx context.Context, references []RevisionReference,
) error {
	missing := make([]RevisionReference, 0, len(references))
	for _, reference := range references {
		if _, done := r.full[reference.DocumentID]; done {
			continue
		}
		if _, done := r.references[reference]; done {
			continue
		}
		r.references[reference] = struct{}{}
		missing = append(missing, reference)
	}
	for start := 0; start < len(missing); {
		end := chunkEnd(start, len(missing), maxInteractionAuthorizationIDs)
		chunk := missing[start:end]
		start = end
		batch, err := r.client.policyStore.RevisionRules(ctx, chunk)
		if err != nil {
			for _, reference := range missing {
				delete(r.references, reference)
			}
			return policyCatalogReadError(ctx, err)
		}
		requested := make(map[shoal.ID]struct{}, len(chunk))
		for _, reference := range chunk {
			requested[reference.DocumentID] = struct{}{}
		}
		for documentID, rules := range batch {
			if _, ok := requested[documentID]; ok {
				r.merge(documentID, rules)
			}
		}
	}
	return nil
}

// merge adds rules a partial load returned. One read sees one catalog per
// lookup; should two lookups disagree on which revision is current, both are
// kept marked current, and referenceRules then requires both.
func (r *revisionRuleCache) merge(documentID shoal.ID, rules []RevisionRule) {
	known := make(map[shoal.ID]int, len(r.rules[documentID]))
	for index, rule := range r.rules[documentID] {
		known[rule.RevisionID] = index
	}
	for _, rule := range rules {
		if index, ok := known[rule.RevisionID]; ok {
			existing := &r.rules[documentID][index]
			existing.Current = existing.Current || rule.Current
			continue
		}
		known[rule.RevisionID] = len(r.rules[documentID])
		r.rules[documentID] = append(r.rules[documentID], rule)
	}
}

// referenceRules is what revisionAllows requires of a reader of reference:
// its own rule and, when it is no longer current, its document's current
// rule. It is false for an unregistered revision and for a document with no
// current revision, which every caller treats as denied.
func (r *revisionRuleCache) referenceRules(
	reference RevisionReference,
) ([]AccessRule, bool) {
	var own *RevisionRule
	var current []AccessRule
	rules := r.rules[reference.DocumentID]
	for index := range rules {
		if rules[index].RevisionID == reference.RevisionID {
			own = &rules[index]
		}
		if rules[index].Current {
			current = append(current, rules[index].Rule)
		}
	}
	if own == nil || len(current) == 0 {
		return nil, false
	}
	if own.Current && len(current) == 1 {
		return []AccessRule{own.Rule}, true
	}
	return append([]AccessRule{own.Rule}, current...), true
}

// sequenceBound is the publication sequence a session made at asOf is held
// to for a document: the highest sequence among the revisions the corpus
// published at or before asOf. The wall clock only picks that revision; the
// sequence, which the corpus assigns monotonically, then decides which
// revisions existed by then, so a clock step between two publications cannot
// drop one. Zero means no bound: every revision counts (a zero asOf, no
// publication reader, or no revision with both a time and a sequence at or
// before asOf).
func (r *revisionRuleCache) sequenceBound(
	documentID shoal.ID, asOf time.Time,
) uint64 {
	if asOf.IsZero() {
		return 0
	}
	var bound uint64
	for _, publication := range r.published[documentID] {
		if publication.Sequence == 0 || publication.PublishedAt.IsZero() ||
			publication.PublishedAt.After(asOf) {
			continue
		}
		if publication.Sequence > bound {
			bound = publication.Sequence
		}
	}
	return bound
}

// documentRules is what an output recorded before the binding existed is
// held to: the rule of every registered revision of the document it could
// have been derived from. With a nonzero sequence bound (a session's, see
// sequenceBound), a revision the corpus published with a higher sequence is
// left out: it was published after the one current at the session's
// snapshot. The current revision, and every revision with no known sequence,
// is always kept. A zero bound keeps every revision (every legacy edge). It
// is false for a document with no current revision.
func (r *revisionRuleCache) documentRules(
	documentID shoal.ID, bound uint64,
) ([]AccessRule, bool) {
	if _, loaded := r.full[documentID]; !loaded {
		return nil, false
	}
	rules := r.rules[documentID]
	published := r.published[documentID]
	required := make([]AccessRule, 0, len(rules))
	hasCurrent := false
	for _, rule := range rules {
		hasCurrent = hasCurrent || rule.Current
		// A revision with no sequence (zero) is never above the bound, so
		// it always counts.
		if publication, known := published[rule.RevisionID]; !rule.Current &&
			known && bound != 0 && publication.Sequence > bound {
			continue
		}
		required = append(required, rule.Rule)
	}
	return required, hasCurrent
}

// bindEdgeSourceRevisions conjoins each application edge's revision binding
// into its effective rule: a bound edge's recorded revisions, and for an edge
// registered before the binding existed every revision of each endpoint's
// document. An edge whose binding cannot be satisfied or conjoined is dropped,
// which every caller treats as unregistered and denied.
func (c *Client) bindEdgeSourceRevisions(
	ctx context.Context,
	registrations map[shoal.ID]EdgeRegistration,
) (map[shoal.ID]EdgeRegistration, error) {
	var references []RevisionReference
	var documentIDs, legacyEndpoints []shoal.ID
	for _, registration := range registrations {
		switch {
		case registration.SourceRevisionsBound:
			references = append(references, registration.SourceRevisions...)
		case registration.Kind == RegistrationApplication:
			legacyEndpoints = append(legacyEndpoints,
				registration.Edge.From, registration.Edge.To)
		}
	}
	if len(references) == 0 && len(legacyEndpoints) == 0 {
		return registrations, nil
	}
	endpointDocuments := make(map[shoal.ID]shoal.ID)
	if len(legacyEndpoints) > 0 {
		for start := 0; start < len(legacyEndpoints); {
			end := chunkEnd(start, len(legacyEndpoints), maxInteractionAuthorizationIDs)
			endpoints, err := c.policyStore.Nodes(ctx, legacyEndpoints[start:end])
			start = end
			if err != nil {
				return nil, policyCatalogReadError(ctx, err)
			}
			for nodeID, endpoint := range endpoints {
				if reference, ok := nodeRevision(endpoint); ok {
					endpointDocuments[nodeID] = reference.DocumentID
					documentIDs = append(documentIDs, reference.DocumentID)
				}
			}
		}
	}
	connected, err := c.connectedLegacyEdges(ctx, registrations)
	if err != nil {
		return nil, err
	}
	cache := c.newRevisionRuleCache()
	if err := cache.loadDocuments(ctx, documentIDs); err != nil {
		return nil, err
	}
	if err := cache.loadReferences(ctx, references); err != nil {
		return nil, err
	}
	effective := make(map[shoal.ID]EdgeRegistration, len(registrations))
	for id, registration := range registrations {
		var required []AccessRule
		satisfiable := true
		switch {
		case registration.SourceRevisionsBound:
			for _, reference := range registration.SourceRevisions {
				rules, ok := cache.referenceRules(reference)
				if !ok {
					satisfiable = false
					break
				}
				required = append(required, rules...)
			}
		case registration.Kind == RegistrationApplication:
			if !connected.lookup(id) {
				// Not a connected edge: the corpus holds no Connect record
				// for it, so it is an extracted or materialized edge
				// persisted before RegistrationKind existed
				// (legacyEdgeKind). Nothing was connected to a revision.
				effective[id] = registration
				continue
			}
			// No time bound: an edge's Connect time and a revision's
			// publication time are unrelated wall-clock readings, and a
			// clock step between them could leave out the very revision the
			// edge was connected over. Every revision counts.
			for _, endpoint := range []shoal.ID{
				registration.Edge.From, registration.Edge.To,
			} {
				documentID, ok := endpointDocuments[endpoint]
				if !ok {
					// Not a document's node: the endpoint check decides it.
					continue
				}
				rules, ok := cache.documentRules(documentID, 0)
				if !ok {
					satisfiable = false
					break
				}
				required = append(required, rules...)
			}
		default:
			effective[id] = registration
			continue
		}
		if !satisfiable {
			continue
		}
		rule := registration.Rule
		conjoined := true
		for _, component := range required {
			next, err := conjoinRules(rule, component)
			if err != nil {
				conjoined = false
				break
			}
			rule = next
		}
		if !conjoined {
			continue
		}
		registration.Rule = rule
		effective[id] = registration
	}
	return effective, nil
}

// connectedLegacyEdges is what the trusted corpus says about the unbound
// application edges among registrations: which of them it holds a Connect
// record for. Without a PublicationReader every such edge counts as
// connected.
type connectedLegacyEdges struct {
	known     bool
	connected map[shoal.ID]bool
}

func (l connectedLegacyEdges) lookup(edgeID shoal.ID) bool {
	return !l.known || l.connected[edgeID]
}

func (c *Client) connectedLegacyEdges(
	ctx context.Context, registrations map[shoal.ID]EdgeRegistration,
) (connectedLegacyEdges, error) {
	reader := c.publications
	if isNilDependency(reader) {
		return connectedLegacyEdges{}, nil
	}
	var edgeIDs []shoal.ID
	for id, registration := range registrations {
		if !registration.SourceRevisionsBound &&
			registration.Kind == RegistrationApplication {
			edgeIDs = append(edgeIDs, id)
		}
	}
	result := connectedLegacyEdges{
		known: true, connected: make(map[shoal.ID]bool),
	}
	for start := 0; start < len(edgeIDs); {
		end := chunkEnd(start, len(edgeIDs), maxInteractionAuthorizationIDs)
		connected, err := reader.ConnectedEdges(ctx, edgeIDs[start:end])
		start = end
		if err != nil {
			return connectedLegacyEdges{}, directBaseError(err)
		}
		for id, ok := range connected {
			result.connected[id] = ok
		}
	}
	return result, nil
}

// interactionSourceRevisions is the binding a session records: the revision
// of every document a touched node or edge (or a touched edge's endpoint)
// belongs to, each touched application edge's own binding, and every
// revision its document evidence cites. It reads registrations directly, in
// bounded batches. A shared extracted node or relation (#700) contributes the
// revision of the owner the recorder (reader) sees it through, never another
// owner's: binding a session to a revision its recorder cannot read would
// hide the recorder's own session whenever that revision is closed.
func (c *Client) interactionSourceRevisions(
	ctx context.Context,
	nodeIDs, edgeIDs []shoal.ID,
	references []interaction.EvidenceReference,
	reader *registrationReader,
) ([]RevisionReference, error) {
	var revisions []RevisionReference
	nodeIDs = append([]shoal.ID(nil), nodeIDs...)
	edgeIDs = append([]shoal.ID(nil), edgeIDs...)
	for _, reference := range references {
		nodeIDs = append(nodeIDs, reference.NodeIDs...)
		edgeIDs = append(edgeIDs, reference.EdgeIDs...)
		if reference.Kind == interaction.EvidenceDocument {
			revisions = append(revisions, RevisionReference{
				DocumentID: reference.Citation.DocumentID,
				RevisionID: reference.Citation.RevisionID,
			})
		}
	}
	nodes, edges, err := c.rawRegistrations(ctx, nodeIDs, edgeIDs)
	if err != nil {
		return nil, err
	}
	nodes, edges, err = c.projectSharedRegistrations(ctx, nodes, edges, reader)
	if err != nil {
		return nil, err
	}
	for _, registration := range nodes {
		if reference, ok := nodeRevision(registration); ok {
			revisions = append(revisions, reference)
		}
	}
	for _, registration := range edges {
		if reference, ok := edgeRevision(registration); ok {
			revisions = append(revisions, reference)
		}
		revisions = append(revisions, registration.SourceRevisions...)
	}
	return canonicalRevisionReferences(revisions)
}

// rawRegistrations resolves stored node and edge registrations, plus every
// resolved edge's endpoints, in lookups bounded by
// maxInteractionAuthorizationIDs. Unregistered identifiers are omitted.
func (c *Client) rawRegistrations(
	ctx context.Context,
	nodeIDs, edgeIDs []shoal.ID,
) (map[shoal.ID]NodeRegistration, map[shoal.ID]EdgeRegistration, error) {
	edges := make(map[shoal.ID]EdgeRegistration)
	for start := 0; start < len(edgeIDs); {
		end := chunkEnd(start, len(edgeIDs), maxInteractionAuthorizationIDs)
		batch, err := c.policyStore.Edges(ctx, edgeIDs[start:end])
		start = end
		if err != nil {
			return nil, nil, policyCatalogReadError(ctx, err)
		}
		for id, registration := range batch {
			edges[id] = registration
		}
	}
	all := append([]shoal.ID(nil), nodeIDs...)
	for _, registration := range edges {
		all = append(all, registration.Edge.From, registration.Edge.To)
	}
	nodes := make(map[shoal.ID]NodeRegistration)
	for start := 0; start < len(all); {
		end := chunkEnd(start, len(all), maxInteractionAuthorizationIDs)
		batch, err := c.policyStore.Nodes(ctx, all[start:end])
		start = end
		if err != nil {
			return nil, nil, policyCatalogReadError(ctx, err)
		}
		for id, registration := range batch {
			nodes[id] = registration
		}
	}
	return nodes, edges, nil
}

// projectSharedRegistrations replaces each raw registration that has
// co-owners (#700) with its first owner or the co-owner reader sees it
// through, keeping every other field. A registration none of whose owners
// still contributes is left as stored: authorizing it denies anyway.
func (c *Client) projectSharedRegistrations(
	ctx context.Context,
	nodes map[shoal.ID]NodeRegistration,
	edges map[shoal.ID]EdgeRegistration,
	reader *registrationReader,
) (map[shoal.ID]NodeRegistration, map[shoal.ID]EdgeRegistration, error) {
	sharedNodes := make(map[shoal.ID]NodeRegistration)
	for id, registration := range nodes {
		if registration.Kind == RegistrationExtracted && len(registration.CoOwners) > 0 {
			sharedNodes[id] = registration
		}
	}
	sharedEdges := make(map[shoal.ID]EdgeRegistration)
	for id, registration := range edges {
		if registration.Kind == RegistrationExtracted && len(registration.CoOwners) > 0 {
			sharedEdges[id] = registration
		}
	}
	if len(sharedNodes) > 0 {
		projected, err := c.effectiveNodeRegistrations(ctx, sharedNodes, reader)
		if err != nil {
			return nil, nil, err
		}
		for id, registration := range sharedNodes {
			if owner, ok := projected[id]; ok {
				registration.DocumentID = owner.DocumentID
				registration.RevisionID = owner.RevisionID
			}
			registration.CoOwners = nil
			nodes[id] = registration
		}
	}
	if len(sharedEdges) > 0 {
		projected, err := effectiveRegistrations(c, ctx, sharedEdges, reader,
			edgeOwnedFields, edgeWithOwner)
		if err != nil {
			return nil, nil, err
		}
		for id, registration := range sharedEdges {
			if owner, ok := projected[id]; ok {
				registration.DocumentID = owner.DocumentID
				registration.RevisionID = owner.RevisionID
			}
			registration.CoOwners = nil
			edges[id] = registration
		}
	}
	return nodes, edges, nil
}

// recordInteractionSourceRevisions durably records a session's binding
// before the session is written, so a session is never readable without it.
func (c *Client) recordInteractionSourceRevisions(
	ctx context.Context,
	sessionID shoal.ID,
	revisions []RevisionReference,
) error {
	if len(revisions) == 0 {
		return nil
	}
	if err := c.policyStore.PutInteractionRevisions(
		ctx, sessionID, revisions); err != nil {
		return policyCatalogWriteError(ctx, err)
	}
	return nil
}

// revisionGate decides revision requirements for one reader, caching each
// revision's and each document's outcome for the whole read.
type revisionGate struct {
	cache      *revisionRuleCache
	decision   auth.Decision
	operation  auth.Operation
	now        time.Time
	references map[RevisionReference]bool
	documents  map[boundDocument]bool
}

// boundDocument is a legacy requirement: a document's revisions up to a
// publication sequence (zero: all of them).
type boundDocument struct {
	documentID shoal.ID
	bound      uint64
}

func (c *Client) newRevisionGate(
	decision auth.Decision, operation auth.Operation, now time.Time,
) *revisionGate {
	return &revisionGate{
		cache: c.newRevisionRuleCache(), decision: decision,
		operation: operation, now: now,
		references: make(map[RevisionReference]bool),
		documents:  make(map[boundDocument]bool),
	}
}

func (g *revisionGate) rulesAllow(rules []AccessRule) (bool, error) {
	for _, rule := range rules {
		allowed, err := ruleAllows(rule, g.decision, g.operation, g.now)
		if err != nil || !allowed {
			return false, err
		}
	}
	return true, nil
}

func (g *revisionGate) referenceAllows(
	reference RevisionReference,
) (bool, error) {
	if allowed, done := g.references[reference]; done {
		return allowed, nil
	}
	rules, ok := g.cache.referenceRules(reference)
	allowed := false
	if ok {
		var err error
		allowed, err = g.rulesAllow(rules)
		if err != nil {
			return false, err
		}
	}
	g.references[reference] = allowed
	return allowed, nil
}

func (g *revisionGate) documentAllows(
	documentID shoal.ID, asOf time.Time,
) (bool, error) {
	key := boundDocument{
		documentID: documentID,
		bound:      g.cache.sequenceBound(documentID, asOf),
	}
	if allowed, done := g.documents[key]; done {
		return allowed, nil
	}
	rules, ok := g.cache.documentRules(documentID, key.bound)
	allowed := false
	if ok {
		var err error
		allowed, err = g.rulesAllow(rules)
		if err != nil {
			return false, err
		}
	}
	g.documents[key] = allowed
	return allowed, nil
}

// legacyInteractionDocuments is every document a session recorded before the
// binding existed now reaches through its touched registrations.
func legacyInteractionDocuments(
	nodes map[shoal.ID]NodeRegistration,
	edges map[shoal.ID]EdgeRegistration,
	record explorer.InteractionRecord,
) []shoal.ID {
	var documentIDs []shoal.ID
	addNode := func(nodeID shoal.ID) {
		if reference, ok := nodeRevision(nodes[nodeID]); ok {
			documentIDs = append(documentIDs, reference.DocumentID)
		}
	}
	for _, nodeID := range record.TouchedNodeIDs {
		addNode(nodeID)
	}
	for _, edgeID := range record.TouchedEdgeIDs {
		registration, ok := edges[edgeID]
		if !ok {
			continue
		}
		if reference, ok := edgeRevision(registration); ok {
			documentIDs = append(documentIDs, reference.DocumentID)
		}
		addNode(registration.Edge.From)
		addNode(registration.Edge.To)
	}
	return documentIDs
}

// authorizeInteractionRevisions applies the revision binding to every
// record whose allowed entry is set, clearing it for each record a reader
// may not see. A tombstone keeps no source content and is left alone.
func (c *Client) authorizeInteractionRevisions(
	ctx context.Context,
	gate *revisionGate,
	records []explorer.InteractionRecord,
	allowed []bool,
) error {
	pending := make([]int, 0, len(records))
	for index, record := range records {
		if allowed[index] && !record.Summary.Deleted {
			pending = append(pending, index)
		}
	}
	for start := 0; start < len(pending); {
		end := start
		cost := 0
		for end < len(pending) {
			record := records[pending[end]]
			next := 1 + interactionAuthorizationCost(
				len(record.TouchedNodeIDs), len(record.TouchedEdgeIDs))
			if end > start && cost+next > maxInteractionAuthorizationIDs {
				break
			}
			cost += next
			end++
		}
		if err := c.authorizeInteractionRevisionBatch(
			ctx, gate, records, pending[start:end], allowed); err != nil {
			return err
		}
		start = end
	}
	return nil
}

func (c *Client) authorizeInteractionRevisionBatch(
	ctx context.Context,
	gate *revisionGate,
	records []explorer.InteractionRecord,
	batch []int,
	allowed []bool,
) error {
	sessionIDs := make([]shoal.ID, 0, len(batch))
	for _, index := range batch {
		sessionIDs = append(sessionIDs, records[index].Summary.SessionID)
	}
	recorded, err := c.policyStore.InteractionRevisions(ctx, sessionIDs)
	if err != nil {
		return policyCatalogReadError(ctx, err)
	}
	var references []RevisionReference
	var documentIDs, legacyNodes, legacyEdges []shoal.ID
	for _, index := range batch {
		bound, ok := recorded[records[index].Summary.SessionID]
		if ok {
			references = append(references, bound...)
			continue
		}
		legacyNodes = append(legacyNodes, records[index].TouchedNodeIDs...)
		legacyEdges = append(legacyEdges, records[index].TouchedEdgeIDs...)
	}
	var nodes map[shoal.ID]NodeRegistration
	var edges map[shoal.ID]EdgeRegistration
	legacy := make(map[int][]shoal.ID)
	if len(legacyNodes) > 0 || len(legacyEdges) > 0 {
		nodes, edges, err = c.rawRegistrations(ctx, legacyNodes, legacyEdges)
		if err != nil {
			return err
		}
		for _, index := range batch {
			if _, bound := recorded[records[index].Summary.SessionID]; bound {
				continue
			}
			legacy[index] = legacyInteractionDocuments(nodes, edges, records[index])
			documentIDs = append(documentIDs, legacy[index]...)
		}
	}
	if err := gate.cache.loadDocuments(ctx, documentIDs); err != nil {
		return err
	}
	if err := gate.cache.loadReferences(ctx, references); err != nil {
		return err
	}
	for _, index := range batch {
		decided := true
		if references, bound := recorded[records[index].Summary.SessionID]; bound {
			for _, reference := range references {
				ok, err := gate.referenceAllows(reference)
				if err != nil {
					return err
				}
				if !ok {
					decided = false
					break
				}
			}
		} else {
			// Bounded by the snapshot the session was validated against
			// when it was written (sequenceBound): a revision published
			// after the one then current cannot be what the session saw.
			asOf := records[index].Session.SnapshotAsOf
			for _, documentID := range legacy[index] {
				ok, err := gate.documentAllows(documentID, asOf)
				if err != nil {
					return err
				}
				if !ok {
					decided = false
					break
				}
			}
		}
		allowed[index] = decided
	}
	return nil
}

// interactionRevisionsAllow is authorizeInteractionRevisions for one record.
func (c *Client) interactionRevisionsAllow(
	ctx context.Context,
	decision auth.Decision,
	now time.Time,
	record explorer.InteractionRecord,
) (bool, error) {
	allowed := []bool{true}
	if err := c.authorizeInteractionRevisions(
		ctx, c.newRevisionGate(decision, auth.OperationRead, now),
		[]explorer.InteractionRecord{record}, allowed,
	); err != nil {
		return false, err
	}
	return allowed[0], nil
}
