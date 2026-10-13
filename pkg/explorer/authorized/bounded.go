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
	"strconv"
	"strings"
	"time"

	"github.com/phrocker/shoal-oss/pkg/explorer"
	"github.com/phrocker/shoal-oss/pkg/explorer/auth"
	"github.com/phrocker/shoal-oss/pkg/graph"
	"github.com/phrocker/shoal-oss/pkg/inference"
	"github.com/phrocker/shoal-oss/pkg/ontology"
	"github.com/phrocker/shoal-oss/pkg/retrieval"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

const vectorCapabilityProbeText = "shoal vector capability probe"
const graphAssertionEdgeIDMetadata = "shoal.graph.edge_id"
const extractionRelationshipIDProperty = "ontology_relationship_id"

func (c *Client) Snapshot(ctx context.Context) (explorer.Snapshot, error) {
	bounded, err := c.boundedBase()
	if err != nil {
		return explorer.Snapshot{}, err
	}
	_, guard, _, err := c.begin(ctx, auth.OperationRetrieve)
	if err != nil {
		return explorer.Snapshot{}, err
	}
	snapshot, err := bounded.Snapshot(ctx)
	if err != nil {
		return explorer.Snapshot{}, directBaseError(err)
	}
	if err := guard.Check(ctx); err != nil {
		return explorer.Snapshot{}, err
	}
	return snapshot, nil
}

func (c *Client) BoundedAvailable() bool {
	_, ok := c.base.(explorer.BoundedClient)
	return ok
}

func (c *Client) VectorAvailable(ctx context.Context) (bool, error) {
	if !c.authorizedVectorScoringAvailable() {
		return false, nil
	}
	decision, guard, now, err := c.begin(ctx, auth.OperationRetrieve)
	if err != nil {
		return false, err
	}
	summaries, err := c.base.Documents(ctx)
	if err != nil {
		return false, directBaseError(err)
	}
	visibleOrder := make([]shoal.ID, 0, len(summaries))
	visible := make(map[shoal.ID]RevisionRegistration, len(summaries))
	for _, summary := range summaries {
		if err := validateSummary(summary); err != nil {
			return false, inconsistentBase()
		}
	}
	documentIDs := make([]shoal.ID, 0, len(summaries))
	for _, summary := range summaries {
		documentIDs = append(documentIDs, summary.Document.ID)
	}
	currentRevisions, err := c.resolveCurrentRevisions(ctx, documentIDs)
	if err != nil {
		return false, err
	}
	for _, summary := range summaries {
		registration, ok := currentRevisions[summary.Document.ID]
		if !ok || registration.RevisionID != summary.Revision.ID {
			continue
		}
		allowed, err := ruleAllows(
			registration.Rule, decision, auth.OperationRetrieve, now)
		if err != nil {
			return false, err
		}
		if !allowed {
			continue
		}
		if _, duplicate := visible[summary.Document.ID]; duplicate {
			return false, inconsistentBase()
		}
		visible[summary.Document.ID] = registration
		visibleOrder = append(visibleOrder, summary.Document.ID)
	}
	cacheKey := authorizedVectorAvailabilityKey(decision, visibleOrder, visible)
	if available, ok := c.cachedAuthorizedVectorAvailability(cacheKey, now); ok {
		if err := guard.Check(ctx); err != nil {
			return false, err
		}
		return available, nil
	}
	if len(visibleOrder) > retrieval.MaxScopeIDs {
		if err := guard.Check(ctx); err != nil {
			return false, err
		}
		c.cacheAuthorizedVectorAvailability(cacheKey, false, now)
		return false, nil
	}
	corpus, err := c.hydrateRetrievalCorpus(ctx, visibleOrder, visible, decision, now)
	if err != nil {
		return false, err
	}
	available := true
	if len(visibleOrder) == 0 {
		available = false
	} else {
		probe := retrieval.Request{
			Text:  vectorCapabilityProbeText,
			Modes: []retrieval.Mode{retrieval.ModeVector},
			Scope: retrieval.Scope{
				DocumentIDs: append([]shoal.ID(nil), visibleOrder...),
			},
		}
		probe, err = probe.Normalize()
		if err != nil {
			return false, err
		}
		response, err := c.base.Retrieve(ctx, probe)
		if err != nil {
			err = directBaseError(err)
			switch {
			case shoal.IsErrorCode(err, shoal.ErrorCanceled),
				shoal.IsErrorCode(err, shoal.ErrorDeadline):
				return false, err
			case shoal.IsErrorCode(err, shoal.ErrorUnavailable),
				shoal.IsErrorCode(err, shoal.ErrorConflict),
				shoal.IsErrorCode(err, shoal.ErrorInvalidArgument):
				available = false
			default:
				return false, err
			}
		} else if err := response.ValidateFor(probe); err != nil {
			available = false
		} else if err := c.validateRetrievedResponse(
			ctx, response, probe, corpus, decision, now,
		); err != nil {
			if shoal.IsErrorCode(err, shoal.ErrorCanceled) ||
				shoal.IsErrorCode(err, shoal.ErrorDeadline) {
				return false, err
			}
			available = false
		}
	}
	if err := guard.Check(ctx); err != nil {
		return false, err
	}
	c.cacheAuthorizedVectorAvailability(cacheKey, available, now)
	return available, nil
}

func (c *Client) cachedAuthorizedVectorAvailability(
	key string,
	now time.Time,
) (bool, bool) {
	c.vectorMu.Lock()
	defer c.vectorMu.Unlock()
	cached := c.vectorAvailability
	age := now.Sub(cached.checkedAt)
	if cached.key == key && !cached.checkedAt.IsZero() &&
		age >= 0 && age < time.Minute {
		return cached.available, true
	}
	return false, false
}

func (c *Client) cacheAuthorizedVectorAvailability(
	key string,
	available bool,
	checkedAt time.Time,
) {
	c.vectorMu.Lock()
	defer c.vectorMu.Unlock()
	c.vectorAvailability = authorizedVectorAvailabilityCache{
		key: key, checkedAt: checkedAt, available: available,
	}
}

func (c *Client) invalidateAuthorizedVectorAvailability() {
	c.vectorMu.Lock()
	defer c.vectorMu.Unlock()
	c.vectorAvailability = authorizedVectorAvailabilityCache{}
}

func authorizedVectorAvailabilityKey(
	decision auth.Decision,
	visibleOrder []shoal.ID,
	visible map[shoal.ID]RevisionRegistration,
) string {
	ids := append([]shoal.ID(nil), visibleOrder...)
	sort.Slice(ids, func(left, right int) bool {
		return shoal.CompareID(ids[left], ids[right]) < 0
	})
	var builder strings.Builder
	builder.WriteString(strconv.FormatInt(decision.PolicyGeneration(), 10))
	for _, documentID := range ids {
		registration := visible[documentID]
		builder.WriteByte('|')
		writeLengthPrefixedID(&builder, documentID)
		writeLengthPrefixedID(&builder, registration.RevisionID)
	}
	return builder.String()
}

func writeLengthPrefixedID(builder *strings.Builder, id shoal.ID) {
	value := string(id)
	builder.WriteString(strconv.Itoa(len(value)))
	builder.WriteByte(':')
	builder.WriteString(value)
	builder.WriteByte(';')
}

func (c *Client) ValidateAuthorization(ctx context.Context, pin inference.AuthPin) error {
	decision, guard, _, err := c.begin(ctx, auth.OperationValidate)
	if err != nil {
		return err
	}
	fingerprint, err := auth.AuthorizationFingerprint(decision)
	if err != nil {
		return err
	}
	if shoal.ID(fingerprint.String()) != pin.Fingerprint() ||
		decision.AuthenticationExpires().Before(pin.ExpiresAt()) {
		return shoal.NewError(shoal.ErrorUnauthorized, "authorization pin does not match current authorization")
	}
	return guard.Check(ctx)
}

func (c *Client) BoundedNeighborhood(
	ctx context.Context,
	request explorer.BoundedNeighborhoodRequest,
) (explorer.BoundedNeighborhood, error) {
	bounded, err := c.boundedBase()
	if err != nil {
		return explorer.BoundedNeighborhood{}, err
	}
	if request.Depth == 0 || request.Fanout == 0 || request.MaxNodes == 0 {
		return explorer.BoundedNeighborhood{}, shoal.NewError(
			shoal.ErrorInvalidArgument, "bounded graph limits must be nonzero")
	}
	normalized, err := (explorer.NeighborhoodRequest{
		NodeIDs: request.NodeIDs, Depth: request.Depth, EdgeTypes: request.EdgeTypes,
	}).Normalize()
	if err != nil {
		return explorer.BoundedNeighborhood{}, err
	}
	decision, guard, now, err := c.begin(ctx, auth.OperationNeighborhood)
	if err != nil {
		return explorer.BoundedNeighborhood{}, err
	}
	for _, nodeID := range normalized.NodeIDs {
		if possibleProvenanceNodeID(nodeID) {
			continue
		}
		if _, err := c.authorizedNode(
			ctx, nodeID, decision, auth.OperationNeighborhood, now); err != nil {
			return explorer.BoundedNeighborhood{}, err
		}
	}
	direction := request.Direction
	if direction == "" {
		direction = explorer.GraphDirectionBoth
	}
	switch direction {
	case explorer.GraphDirectionBoth,
		explorer.GraphDirectionOutgoing,
		explorer.GraphDirectionIncoming:
	default:
		return explorer.BoundedNeighborhood{}, shoal.NewError(
			shoal.ErrorInvalidArgument, "unknown graph direction")
	}
	if uint32(len(normalized.NodeIDs)) > request.MaxNodes {
		return explorer.BoundedNeighborhood{}, shoal.NewError(
			shoal.ErrorInvalidArgument, "graph seeds exceed max_nodes")
	}
	if authorizedCursorEligible(normalized) {
		return c.boundedAuthorizedNeighborhoodPage(
			ctx, bounded, request, normalized, decision, guard, now, direction,
			auth.OperationNeighborhood, true, true)
	}
	return c.boundedAuthorizedTraversal(
		ctx, bounded, request, normalized, decision, guard, now, direction)
}

const (
	derivedAssertionPropertyAssertionID     = "ontology.assertion.id"
	derivedAssertionPropertyDerivationID    = "ontology.assertion.derivation.id"
	derivedAssertionPropertyDerivationScore = "ontology.assertion.derivation.score"
)

type neighborhoodAuthorizationCacheKey struct{}

type neighborhoodAuthorizationCache struct {
	resolved           registeredNodes
	resolvedIDs        map[shoal.ID]struct{}
	canonicalDocuments map[shoal.ID]*canonicalRetrievalDocument
}

func withNeighborhoodAuthorizationCache(ctx context.Context) context.Context {
	if _, ok := ctx.Value(
		neighborhoodAuthorizationCacheKey{}).(*neighborhoodAuthorizationCache); ok {
		return ctx
	}
	return context.WithValue(ctx, neighborhoodAuthorizationCacheKey{},
		&neighborhoodAuthorizationCache{
			resolved:           make(registeredNodes),
			resolvedIDs:        make(map[shoal.ID]struct{}),
			canonicalDocuments: make(map[shoal.ID]*canonicalRetrievalDocument),
		})
}

func neighborhoodCache(ctx context.Context) *neighborhoodAuthorizationCache {
	if cached, ok := ctx.Value(
		neighborhoodAuthorizationCacheKey{}).(*neighborhoodAuthorizationCache); ok {
		return cached
	}
	return &neighborhoodAuthorizationCache{
		resolved:           make(registeredNodes),
		resolvedIDs:        make(map[shoal.ID]struct{}),
		canonicalDocuments: make(map[shoal.ID]*canonicalRetrievalDocument),
	}
}

func (c *Client) resolveNeighborhoodNodes(
	ctx context.Context,
	nodeIDs []shoal.ID,
	cache *neighborhoodAuthorizationCache,
) (registeredNodes, error) {
	missing := make([]shoal.ID, 0, len(nodeIDs))
	for _, nodeID := range nodeIDs {
		if _, seen := cache.resolvedIDs[nodeID]; seen {
			continue
		}
		cache.resolvedIDs[nodeID] = struct{}{}
		missing = append(missing, nodeID)
	}
	if len(missing) > 0 {
		resolved, err := c.resolveNodes(ctx, missing)
		if err != nil {
			for _, nodeID := range missing {
				delete(cache.resolvedIDs, nodeID)
			}
			return nil, err
		}
		for nodeID, registration := range resolved {
			cache.resolved[nodeID] = registration
		}
	}
	return cache.resolved, nil
}

func authorizedCursorEligible(normalized explorer.NeighborhoodRequest) bool {
	return len(normalized.NodeIDs) == 1 && normalized.Depth == 1
}

// DefaultMaxBoundedNodeAdjacency is the per-node total adjacency cap the
// authorized bounded graph reads apply when Config.MaxBoundedNodeAdjacency is
// zero. It is high on purpose: it is the one point at which hidden adjacency
// shows, as a single uniform bit, so moving it takes about a million writes.
const DefaultMaxBoundedNodeAdjacency uint32 = 1 << 20

// boundedAdjacencyMaxChunk is the largest base page a bounded read streams
// at once. Chunks start at the visible edges a page needs plus
// boundedAdjacencyChunkSlack and double up to it, so a mostly visible node
// costs about its fanout per page.
const boundedAdjacencyMaxChunk uint32 = 1024

// boundedAdjacencyChunkSlack is the extra entries the first chunk reads
// beyond the visible edges a page needs. Up to this many hidden entries in
// that window cost no extra base call, so a sparse hidden edge cannot be
// located by the call count (or its latency) as a cursor sweeps past it;
// only hidden density beyond it shows as time.
const boundedAdjacencyChunkSlack uint64 = 64

func (c *Client) maxBoundedNodeAdjacency() uint64 {
	if c.maxNodeAdjacency == 0 {
		return uint64(DefaultMaxBoundedNodeAdjacency)
	}
	return uint64(c.maxNodeAdjacency)
}

// errNodeAdjacencyLimit is the single failure a bounded read gives when a
// node it must expand has more total adjacency than the uniform cap. It
// depends only on that node's total adjacency in the requested direction,
// which the base reports in O(1) whatever the cursor, never on the caller's
// fanout, cursor, edge types or MaxScannedEdges, nor on where hidden edges
// lie.
var errNodeAdjacencyLimit = shoal.NewError(
	shoal.ErrorUnavailable,
	"authorized bounded graph node adjacency exceeds the server limit")

// seedAdjacencyError maps the uniform cap failure for a provenance seed to
// not-found: whether a provenance seed is visible is only learnt by
// filtering its adjacency, which the cap forbids, so it is answered as for an
// invisible one, uniformly for every reader.
func seedAdjacencyError(seed shoal.ID, err error) error {
	if err == errNodeAdjacencyLimit && possibleProvenanceNodeID(seed) {
		return auth.ObjectNotFound()
	}
	return err
}

// authorizedAdjacency is a prefix of one node's reader-visible adjacency
// after a cursor: every visible edge up to the point the stream stopped, in
// edge ID order, and their nodes and assertions.
type authorizedAdjacency struct {
	nodes      map[shoal.ID]graph.Node
	edges      []graph.Edge
	assertions map[shoal.ID]ontology.Assertion
}

// authorizedNodeAdjacency streams one node's adjacency after the cursor in
// growing chunks, filters each to what the reader may see, and stops once it
// holds need visible edges or the adjacency is exhausted.
//
// The outcome of a bounded authorized read must be a function only of the
// graph the reader can see (#698). A page needs only the first fanout+1
// visible edges after its cursor, and those are the same whatever hidden
// adjacency lies between them, so streaming until they are found gives
// exactly what the complete visible adjacency would. The stream never stops
// on a work budget: a budget that hidden edges can exhaust mid-scan depends on
// where they lie relative to the cursor and could be binary-searched. Hidden
// adjacency therefore shows only as latency, and as the one uniform failure
// when the node's total adjacency, read in O(1) from the base, exceeds
// MaxBoundedNodeAdjacency. The base leaves interaction adjacency of a source
// node out of both, so interactions count toward neither.
func (c *Client) authorizedNodeAdjacency(
	ctx context.Context,
	bounded explorer.BoundedClient,
	seed shoal.ID,
	after shoal.ID,
	need uint64,
	edgeTypes []string,
	direction explorer.GraphDirection,
	decision auth.Decision,
	guard auth.GenerationGuard,
	now time.Time,
	operation auth.Operation,
) (authorizedAdjacency, error) {
	result := authorizedAdjacency{
		nodes:      make(map[shoal.ID]graph.Node),
		assertions: make(map[shoal.ID]ontology.Assertion),
	}
	chunk := boundedAdjacencyMaxChunk
	if first := need + boundedAdjacencyChunkSlack; first < uint64(chunk) {
		chunk = uint32(first)
	}
	cursor := after
	total := uint64(0)
	seenEdges := make(map[shoal.ID]struct{})
	for call := uint64(0); ; call++ {
		if err := ctx.Err(); err != nil {
			return authorizedAdjacency{}, err
		}
		if err := guard.Check(ctx); err != nil {
			return authorizedAdjacency{}, err
		}
		raw, err := bounded.BoundedNeighborhood(ctx, explorer.BoundedNeighborhoodRequest{
			NodeIDs: []shoal.ID{seed}, Depth: 1, Fanout: chunk,
			// One more than the edges the chunk can hold, so the node bound
			// can never be what stops it.
			MaxNodes:    chunk + 1,
			Direction:   direction,
			AfterEdgeID: cursor,
		})
		if err != nil {
			return authorizedAdjacency{}, directBaseError(err)
		}
		if !raw.NodeAdjacencyKnown {
			return authorizedAdjacency{}, shoal.NewError(
				shoal.ErrorUnavailable,
				"bounded Explorer base does not report node adjacency")
		}
		if call == 0 {
			total = raw.NodeAdjacency
			if total > c.maxBoundedNodeAdjacency() {
				return authorizedAdjacency{}, errNodeAdjacencyLimit
			}
		}
		// The node's adjacency changing mid-stream is a concurrent write, as
		// the snapshot check reports for writes anywhere else: a conflict,
		// whether or not the reader can see the written edge.
		if raw.NodeAdjacency != total {
			return authorizedAdjacency{}, shoal.NewError(
				shoal.ErrorConflict, "corpus changed while scanning bounded graph")
		}
		// An honest base advances at least one entry per continuation, so
		// it cannot need more calls than the node has entries.
		if call > total ||
			uint32(len(raw.Neighborhood.Edges)) > chunk ||
			(raw.Truncated && !raw.Continuation) ||
			(!raw.Continuation && raw.NextAfterEdgeID != "") ||
			(raw.Continuation && (raw.NextAfterEdgeID == "" ||
				shoal.CompareID(raw.NextAfterEdgeID, cursor) <= 0)) ||
			(raw.ScannedEdgesKnown &&
				(raw.ScannedEdges < uint32(len(raw.Neighborhood.Edges)) ||
					raw.ScannedEdges > chunk ||
					(raw.ScannedEdges == 0 && raw.Continuation))) {
			return authorizedAdjacency{}, inconsistentBase()
		}
		for _, edge := range raw.Neighborhood.Edges {
			if cursor != "" && shoal.CompareID(edge.ID, cursor) <= 0 {
				return authorizedAdjacency{}, inconsistentBase()
			}
		}
		filtered, err := c.filterNeighborhood(
			ctx, raw.Neighborhood,
			explorer.NeighborhoodRequest{
				NodeIDs: []shoal.ID{seed}, Depth: 1,
				EdgeTypes: append([]string(nil), edgeTypes...),
			},
			direction, decision, now, true, operation)
		if err != nil {
			return authorizedAdjacency{}, err
		}
		for _, node := range filtered.Nodes {
			if _, ok := result.nodes[node.ID]; !ok {
				result.nodes[node.ID] = cloneGraphNode(node)
			}
		}
		for _, edge := range filtered.Edges {
			if _, ok := seenEdges[edge.ID]; ok {
				continue
			}
			seenEdges[edge.ID] = struct{}{}
			result.edges = append(result.edges, cloneGraphEdge(edge))
		}
		for _, assertion := range filtered.Assertions {
			key := assertion.ID()
			if edgeID := assertion.Metadata()[graphAssertionEdgeIDMetadata]; edgeID != "" {
				key = shoal.ID(edgeID)
			}
			if _, ok := result.assertions[key]; !ok {
				result.assertions[key] = assertion
			}
		}
		if uint64(len(result.edges)) >= need || !raw.Continuation {
			break
		}
		cursor = raw.NextAfterEdgeID
		if chunk < boundedAdjacencyMaxChunk {
			chunk *= 2
			if chunk > boundedAdjacencyMaxChunk {
				chunk = boundedAdjacencyMaxChunk
			}
		}
	}
	sort.Slice(result.edges, func(left, right int) bool {
		return shoal.CompareID(result.edges[left].ID, result.edges[right].ID) < 0
	})
	return result, nil
}

// boundedEdgesNeeded is the visible edges a page or expansion of fanout
// needs: its fanout, and one more to know whether more remain.
func boundedEdgesNeeded(fanout uint32) uint64 {
	return uint64(fanout) + 1
}

func (c *Client) boundedAuthorizedNeighborhoodPage(
	ctx context.Context,
	bounded explorer.BoundedClient,
	request explorer.BoundedNeighborhoodRequest,
	normalized explorer.NeighborhoodRequest,
	decision auth.Decision,
	guard auth.GenerationGuard,
	now time.Time,
	direction explorer.GraphDirection,
	operation auth.Operation,
	applyLens bool,
	validateSnapshot bool,
) (explorer.BoundedNeighborhood, error) {
	var before explorer.Snapshot
	var err error
	if validateSnapshot {
		before, err = bounded.Snapshot(ctx)
		if err != nil {
			return explorer.BoundedNeighborhood{}, directBaseError(err)
		}
	}
	ctx = withNeighborhoodAuthorizationCache(ctx)
	ctx, err = c.withCanonicalDocumentIndex(ctx)
	if err != nil {
		return explorer.BoundedNeighborhood{}, err
	}
	seed := normalized.NodeIDs[0]
	adjacency, err := c.authorizedNodeAdjacency(
		ctx, bounded, seed, request.AfterEdgeID,
		boundedEdgesNeeded(request.Fanout), normalized.EdgeTypes, direction,
		decision, guard, now, operation)
	if err != nil {
		return explorer.BoundedNeighborhood{}, seedAdjacencyError(seed, err)
	}
	if _, ok := adjacency.nodes[seed]; !ok && possibleProvenanceNodeID(seed) {
		return explorer.BoundedNeighborhood{}, auth.ObjectNotFound()
	}
	// The page is cut from the visible edges after the cursor (the stream
	// refuses any at or before it), so a page and its continuation are the
	// same whatever hidden adjacency lies between visible edges.
	edges := make(map[shoal.ID]graph.Edge, len(adjacency.edges))
	for _, edge := range adjacency.edges {
		edges[edge.ID] = edge
	}
	if err := guard.Check(ctx); err != nil {
		return explorer.BoundedNeighborhood{}, err
	}
	if validateSnapshot {
		afterSnapshot, snapshotErr := bounded.Snapshot(ctx)
		if snapshotErr != nil {
			return explorer.BoundedNeighborhood{}, directBaseError(snapshotErr)
		}
		if before != afterSnapshot {
			return explorer.BoundedNeighborhood{}, shoal.NewError(
				shoal.ErrorConflict, "corpus changed while scanning bounded graph")
		}
	}
	result := authorizedBoundedPage(
		adjacency.nodes, edges, adjacency.assertions, request, len(normalized.NodeIDs))
	// Scan accounting is not reported: any count of what the base examined
	// includes adjacency the reader cannot see.
	result.ScannedEdges = 0
	result.ScannedEdgesKnown = false
	if applyLens {
		interpreted, interpretErr := c.applyOntologyLens(
			ctx, result.Neighborhood, decision)
		if interpretErr != nil {
			return explorer.BoundedNeighborhood{}, interpretErr
		}
		result.Neighborhood = interpreted
	}
	if err := guard.Check(ctx); err != nil {
		return explorer.BoundedNeighborhood{}, err
	}
	return result, nil
}

// boundedAuthorizedTraversal serves a bounded read that cannot page (depth
// two or more, or several seeds). It expands breadth first over the reader's
// visible graph: each expanded node contributes its first Fanout visible
// edges in edge ID order, and MaxNodes stops expansion, exactly as the base
// traversal would over a graph holding only the visible edges. Hidden edges
// therefore never consume a fanout slot, and Truncated is derived from
// visible data alone.
func (c *Client) boundedAuthorizedTraversal(
	ctx context.Context,
	bounded explorer.BoundedClient,
	request explorer.BoundedNeighborhoodRequest,
	normalized explorer.NeighborhoodRequest,
	decision auth.Decision,
	guard auth.GenerationGuard,
	now time.Time,
	direction explorer.GraphDirection,
) (explorer.BoundedNeighborhood, error) {
	before, err := bounded.Snapshot(ctx)
	if err != nil {
		return explorer.BoundedNeighborhood{}, directBaseError(err)
	}
	ctx = withNeighborhoodAuthorizationCache(ctx)
	ctx, err = c.withCanonicalDocumentIndex(ctx)
	if err != nil {
		return explorer.BoundedNeighborhood{}, err
	}
	nodes := make(map[shoal.ID]graph.Node)
	edges := make(map[shoal.ID]graph.Edge)
	assertions := make(map[shoal.ID]ontology.Assertion)
	seen := make(map[shoal.ID]struct{}, len(normalized.NodeIDs))
	frontier := append([]shoal.ID(nil), normalized.NodeIDs...)
	for _, nodeID := range frontier {
		seen[nodeID] = struct{}{}
	}
	truncated := false
	stopExpansion := false
	for level := uint32(0); level < normalized.Depth && len(frontier) > 0 &&
		!stopExpansion; level++ {
		next := make([]shoal.ID, 0)
		for _, seed := range frontier {
			adjacency, err := c.authorizedNodeAdjacency(
				ctx, bounded, seed, "", boundedEdgesNeeded(request.Fanout),
				normalized.EdgeTypes, direction,
				decision, guard, now, auth.OperationNeighborhood)
			if err != nil {
				if level == 0 {
					err = seedAdjacencyError(seed, err)
				}
				return explorer.BoundedNeighborhood{}, err
			}
			seedNode, ok := adjacency.nodes[seed]
			if !ok {
				if level == 0 {
					if possibleProvenanceNodeID(seed) {
						return explorer.BoundedNeighborhood{}, auth.ObjectNotFound()
					}
					return explorer.BoundedNeighborhood{}, inconsistentBase()
				}
				// A node reached only through an admitted provenance edge
				// is not expanded unless it is visible as a seed itself.
				// That depends only on visible data, so skipping it keeps
				// the outcome a function of the reader's graph.
				continue
			}
			if _, ok := nodes[seed]; !ok {
				nodes[seed] = seedNode
			}
			selected := adjacency.edges
			if uint32(len(selected)) > request.Fanout {
				selected = selected[:request.Fanout]
				truncated = true
			}
			for _, edge := range selected {
				other := edge.To
				if other == seed {
					other = edge.From
				}
				otherNode, ok := adjacency.nodes[other]
				if !ok {
					return explorer.BoundedNeighborhood{}, inconsistentBase()
				}
				if _, exists := seen[other]; !exists {
					if uint32(len(seen)) >= request.MaxNodes {
						truncated = true
						stopExpansion = true
						break
					}
					seen[other] = struct{}{}
					next = append(next, other)
				}
				if _, exists := nodes[other]; !exists {
					nodes[other] = otherNode
				}
				edges[edge.ID] = edge
				if assertion, ok := adjacency.assertions[edge.ID]; ok {
					assertions[edge.ID] = assertion
				}
				if edge.Type == graph.EdgeTypeProduced {
					assertionID := shoal.ID(
						edge.Properties[derivedAssertionPropertyAssertionID])
					if assertion, ok := adjacency.assertions[assertionID]; ok {
						assertions[assertionID] = assertion
					}
				}
			}
			if stopExpansion {
				break
			}
		}
		frontier = next
	}
	// Every seed is part of the result, as in the base traversal, even one
	// MaxNodes stopped expansion before reaching. Its node is read with a
	// single small chunk (streamed further only for a provenance seed, whose
	// visibility rests on its visible edges).
	for _, seed := range normalized.NodeIDs {
		if _, ok := nodes[seed]; ok {
			continue
		}
		adjacency, err := c.authorizedNodeAdjacency(
			ctx, bounded, seed, "", 1, normalized.EdgeTypes, direction,
			decision, guard, now, auth.OperationNeighborhood)
		if err != nil {
			return explorer.BoundedNeighborhood{}, seedAdjacencyError(seed, err)
		}
		seedNode, ok := adjacency.nodes[seed]
		if !ok {
			if possibleProvenanceNodeID(seed) {
				return explorer.BoundedNeighborhood{}, auth.ObjectNotFound()
			}
			return explorer.BoundedNeighborhood{}, inconsistentBase()
		}
		nodes[seed] = seedNode
	}
	if err := guard.Check(ctx); err != nil {
		return explorer.BoundedNeighborhood{}, err
	}
	afterSnapshot, err := bounded.Snapshot(ctx)
	if err != nil {
		return explorer.BoundedNeighborhood{}, directBaseError(err)
	}
	if before != afterSnapshot {
		return explorer.BoundedNeighborhood{}, shoal.NewError(
			shoal.ErrorConflict, "corpus changed while scanning bounded graph")
	}
	result := explorer.BoundedNeighborhood{
		Neighborhood: explorer.Neighborhood{
			Nodes:      make([]graph.Node, 0, len(nodes)),
			Edges:      make([]graph.Edge, 0, len(edges)),
			Assertions: make([]ontology.Assertion, 0, len(assertions)),
		},
		Truncated: truncated,
	}
	for _, node := range nodes {
		result.Neighborhood.Nodes = append(result.Neighborhood.Nodes, cloneGraphNode(node))
	}
	for _, edge := range edges {
		result.Neighborhood.Edges = append(result.Neighborhood.Edges, cloneGraphEdge(edge))
	}
	result.Neighborhood.Assertions = selectBoundedAssertions(
		result.Neighborhood.Edges, assertions)
	sort.Slice(result.Neighborhood.Nodes, func(left, right int) bool {
		return shoal.CompareID(
			result.Neighborhood.Nodes[left].ID, result.Neighborhood.Nodes[right].ID) < 0
	})
	sort.Slice(result.Neighborhood.Edges, func(left, right int) bool {
		return shoal.CompareID(
			result.Neighborhood.Edges[left].ID, result.Neighborhood.Edges[right].ID) < 0
	})
	result.Neighborhood, err = c.applyOntologyLens(ctx, result.Neighborhood, decision)
	if err != nil {
		return explorer.BoundedNeighborhood{}, err
	}
	if err := guard.Check(ctx); err != nil {
		return explorer.BoundedNeighborhood{}, err
	}
	return result, nil
}

func authorizedBoundedPage(
	nodes map[shoal.ID]graph.Node,
	edges map[shoal.ID]graph.Edge,
	assertions map[shoal.ID]ontology.Assertion,
	request explorer.BoundedNeighborhoodRequest,
	seedCount int,
) explorer.BoundedNeighborhood {
	edgeIDs := make([]shoal.ID, 0, len(edges))
	for edgeID := range edges {
		edgeIDs = append(edgeIDs, edgeID)
	}
	sort.Slice(edgeIDs, func(left, right int) bool {
		return shoal.CompareID(edgeIDs[left], edgeIDs[right]) < 0
	})
	nodeIDs := make(map[shoal.ID]struct{}, len(request.NodeIDs)+len(edgeIDs)+1)
	for _, nodeID := range request.NodeIDs {
		nodeIDs[nodeID] = struct{}{}
	}
	selectedEdges := make([]graph.Edge, 0, len(edgeIDs))
	truncated := false
	continuation := false
	nextAfter := shoal.ID("")
	for _, edgeID := range edgeIDs {
		edge := edges[edgeID]
		adds := 0
		if _, ok := nodeIDs[edge.From]; !ok {
			adds++
		}
		if _, ok := nodeIDs[edge.To]; !ok && edge.To != edge.From {
			adds++
		}
		if uint32(len(nodeIDs)+adds) > request.MaxNodes {
			truncated = true
			continuation = len(selectedEdges) > 0
			break
		}
		if len(selectedEdges) == int(request.Fanout) {
			continuation = true
			truncated = true
			break
		}
		selectedEdges = append(selectedEdges, cloneGraphEdge(edge))
		nodeIDs[edge.From] = struct{}{}
		nodeIDs[edge.To] = struct{}{}
		nextAfter = edge.ID
	}
	if len(selectedEdges) == 0 {
		continuation = false
		nextAfter = ""
	}
	resultNodes := make([]graph.Node, 0, len(nodeIDs))
	for nodeID := range nodeIDs {
		if node, ok := nodes[nodeID]; ok {
			resultNodes = append(resultNodes, cloneGraphNode(node))
		}
	}
	sort.Slice(resultNodes, func(left, right int) bool {
		return shoal.CompareID(resultNodes[left].ID, resultNodes[right].ID) < 0
	})
	if len(resultNodes) < seedCount {
		truncated = true
	}
	if !continuation {
		nextAfter = ""
	}
	selectedAssertions := selectBoundedAssertions(selectedEdges, assertions)
	return explorer.BoundedNeighborhood{
		Neighborhood: explorer.Neighborhood{
			Nodes: resultNodes, Edges: selectedEdges, Assertions: selectedAssertions,
		},
		Truncated: truncated, NextAfterEdgeID: nextAfter, Continuation: continuation,
	}
}

// selectBoundedAssertions returns, sorted, the assertions of the selected
// edges: an edge's own assertion, or the derived assertion a produced edge
// names.
func selectBoundedAssertions(
	selectedEdges []graph.Edge,
	assertions map[shoal.ID]ontology.Assertion,
) []ontology.Assertion {
	selectedAssertions := make([]ontology.Assertion, 0, len(selectedEdges))
	seenAssertions := make(map[shoal.ID]struct{}, len(selectedEdges))
	for _, edge := range selectedEdges {
		if assertion, ok := assertions[edge.ID]; ok {
			seenAssertions[assertion.ID()] = struct{}{}
			selectedAssertions = append(selectedAssertions, assertion)
			continue
		}
		if edge.Type != graph.EdgeTypeProduced {
			continue
		}
		assertionID, ok := edge.Properties[derivedAssertionPropertyAssertionID]
		if !ok {
			continue
		}
		assertion, ok := assertions[shoal.ID(assertionID)]
		if !ok {
			continue
		}
		if _, duplicate := seenAssertions[assertion.ID()]; duplicate {
			continue
		}
		seenAssertions[assertion.ID()] = struct{}{}
		selectedAssertions = append(selectedAssertions, assertion)
	}
	sort.Slice(selectedAssertions, func(left, right int) bool {
		return shoal.CompareID(selectedAssertions[left].ID(), selectedAssertions[right].ID()) < 0
	})
	return selectedAssertions
}

func (c *Client) boundedBase() (explorer.BoundedClient, error) {
	bounded, ok := c.base.(explorer.BoundedClient)
	if !ok {
		return nil, shoal.NewError(shoal.ErrorUnavailable, "bounded Explorer base unavailable")
	}
	return bounded, nil
}

func (c *Client) filterNeighborhood(
	ctx context.Context,
	raw explorer.Neighborhood,
	normalized explorer.NeighborhoodRequest,
	direction explorer.GraphDirection,
	decision auth.Decision,
	now time.Time,
	allowMissingProvenanceSeeds bool,
	operation auth.Operation,
) (explorer.Neighborhood, error) {
	cache := neighborhoodCache(ctx)
	candidates := make(map[shoal.ID]graph.Node, len(raw.Nodes))
	registrations := make(map[shoal.ID]NodeRegistration, len(raw.Nodes))
	rawNodes := make(map[shoal.ID]graph.Node, len(raw.Nodes))
	rawNodeIDs := make([]shoal.ID, 0, len(raw.Nodes))
	for _, node := range raw.Nodes {
		if err := node.Validate(); err != nil {
			return explorer.Neighborhood{}, inconsistentBase()
		}
		if _, duplicate := rawNodes[node.ID]; duplicate {
			return explorer.Neighborhood{}, inconsistentBase()
		}
		rawNodes[node.ID] = cloneGraphNode(node)
		rawNodeIDs = append(rawNodeIDs, node.ID)
	}
	derivedAssertions, err := derivedAssertionsByEdge(raw.Assertions)
	if err != nil {
		return explorer.Neighborhood{}, err
	}
	typeFilter := make(map[string]struct{}, len(normalized.EdgeTypes))
	for _, edgeType := range normalized.EdgeTypes {
		typeFilter[edgeType] = struct{}{}
	}
	requiredDerivedAssertions := make(
		map[shoal.ID]ontology.Assertion, len(derivedAssertions))
	for _, edge := range raw.Edges {
		if len(typeFilter) > 0 {
			if _, ok := typeFilter[edge.Type]; !ok {
				continue
			}
		}
		if assertion, ok := derivedAssertions[edge.ID]; ok &&
			assertion.Origin() == ontology.AssertionDerived {
			requiredDerivedAssertions[assertion.ID()] = assertion
		}
		if edge.Type == graph.EdgeTypeProduced {
			if assertion, ok := derivedAssertions[edge.To]; ok {
				requiredDerivedAssertions[assertion.ID()] = assertion
			}
		}
	}
	if len(requiredDerivedAssertions) > 0 {
		if isNilDependency(c.derivedAssertions) {
			return explorer.Neighborhood{}, shoal.NewError(
				shoal.ErrorUnavailable,
				"trusted derived assertion reader is unavailable",
			)
		}
		ids := make([]shoal.ID, 0, len(requiredDerivedAssertions))
		for id := range requiredDerivedAssertions {
			ids = append(ids, id)
		}
		trusted, err := c.derivedAssertions.DerivedAssertions(ctx, ids)
		if err != nil {
			return explorer.Neighborhood{}, directBaseError(err)
		}
		if err := validateTrustedDerivedAssertions(
			requiredDerivedAssertions, trusted); err != nil {
			return explorer.Neighborhood{}, err
		}
		for id := range requiredDerivedAssertions {
			requiredDerivedAssertions[id] = trusted[id]
		}
		for edgeID, assertion := range derivedAssertions {
			if canonical, ok := trusted[assertion.ID()]; ok {
				derivedAssertions[edgeID] = canonical
			}
		}
	}
	for _, assertion := range requiredDerivedAssertions {
		target, ok := assertion.Object().ReferenceValue()
		if !ok {
			continue
		}
		rawNodeIDs = append(rawNodeIDs, assertion.Subject(), target)
	}
	resolved, err := c.resolveNeighborhoodNodes(ctx, rawNodeIDs, cache)
	if err != nil {
		return explorer.Neighborhood{}, err
	}
	for _, node := range raw.Nodes {
		registration, ok := resolved[node.ID]
		if !ok {
			continue
		}
		allowed, err := ruleAllows(
			registration.Rule, decision, operation, now)
		if err != nil {
			return explorer.Neighborhood{}, err
		}
		if !allowed {
			continue
		}
		if _, duplicate := candidates[node.ID]; duplicate {
			return explorer.Neighborhood{}, inconsistentBase()
		}
		candidates[node.ID] = cloneGraphNode(node)
		registrations[node.ID] = registration
	}
	derivedAllowed := make(map[shoal.ID]bool, len(requiredDerivedAssertions))
	for _, assertion := range requiredDerivedAssertions {
		target, ok := assertion.Object().ReferenceValue()
		if !ok {
			continue
		}
		allowed, err := edgeEndpointsAllow(
			resolved,
			EdgeRegistration{Edge: graph.Edge{
				ID: assertion.ID(), From: assertion.Subject(), To: target,
				Type: string(assertion.Predicate()), Weight: assertion.Confidence(),
			}},
			decision, operation, now,
		)
		if err != nil {
			return explorer.Neighborhood{}, err
		}
		derivedAllowed[assertion.ID()] = allowed
		if allowed {
			registrations[assertion.Subject()] = resolved[assertion.Subject()]
			registrations[target] = resolved[target]
		}
	}
	canonicalNodes, err := c.canonicalRegisteredNodesCached(
		ctx, registrations, cache.canonicalDocuments)
	if err != nil {
		return explorer.Neighborhood{}, err
	}
	visibleNodes := make(map[shoal.ID]graph.Node, len(candidates))
	for nodeID, node := range candidates {
		canonical, ok := canonicalNodes[nodeID]
		if !ok || !graphNodesEqual(canonical, node) {
			return explorer.Neighborhood{}, inconsistentBase()
		}
		visibleNodes[nodeID] = node
	}

	admittedEdges := make(map[shoal.ID]graph.Edge, len(raw.Edges))
	admittedAssertions := make(map[shoal.ID]ontology.Assertion, len(raw.Assertions))
	candidateEdges := make([]graph.Edge, 0, len(raw.Edges))
	candidateEdgeIDs := make([]shoal.ID, 0, len(raw.Edges))
	for _, edge := range raw.Edges {
		if err := edge.Validate(); err != nil {
			return explorer.Neighborhood{}, inconsistentBase()
		}
		if len(typeFilter) > 0 {
			if _, ok := typeFilter[edge.Type]; !ok {
				continue
			}
		}
		assertion, hasAssertion := derivedAssertions[edge.ID]
		if hasAssertion && assertion.Origin() == ontology.AssertionDerived {
			allowed, err := derivedAssertionEndpointsAllow(
				rawNodes, visibleNodes, canonicalNodes, assertion,
				derivedAllowed[assertion.ID()])
			if err != nil {
				return explorer.Neighborhood{}, err
			}
			if !allowed || !derivedAssertionMatchesEdge(assertion, edge) {
				continue
			}
			admittedEdges[edge.ID] = cloneGraphEdge(edge)
			admittedAssertions[edge.ID] = assertion
			continue
		}
		if edge.Type == graph.EdgeTypeProduced {
			assertion, ok := derivedAssertions[edge.To]
			if !ok {
				return explorer.Neighborhood{}, inconsistentBase()
			}
			allowed := derivedAllowed[assertion.ID()]
			if !allowed || !producerDerivationEdgeMatches(edge, rawNodes, assertion) {
				continue
			}
			// Load-bearing: TestAuthorizedProducerProvenanceDoesNotAggregateHiddenAssertions
			// pins that producer and assertion nodes are admitted only through
			// an already authorized produced assertion.
			visibleNodes[edge.From] = cloneGraphNode(rawNodes[edge.From])
			visibleNodes[edge.To] = cloneGraphNode(rawNodes[edge.To])
			admittedEdges[edge.ID] = cloneGraphEdge(edge)
			admittedAssertions[edge.ID] = assertion
			continue
		}
		if _, ok := visibleNodes[edge.From]; !ok {
			continue
		}
		if _, ok := visibleNodes[edge.To]; !ok {
			continue
		}
		candidateEdges = append(candidateEdges, edge)
		candidateEdgeIDs = append(candidateEdgeIDs, edge.ID)
	}
	resolvedEdges, err := c.resolveEdges(ctx, candidateEdgeIDs)
	if err != nil {
		return explorer.Neighborhood{}, err
	}
	for _, edge := range candidateEdges {
		registration, ok := resolvedEdges[edge.ID]
		if !ok || !graphEdgesEqual(registration.Edge, edge) {
			continue
		}
		allowed, err := edgeAllowsResolved(
			resolved, registration, decision, operation, now)
		if err != nil {
			return explorer.Neighborhood{}, err
		}
		if !allowed {
			continue
		}
		if _, duplicate := admittedEdges[edge.ID]; duplicate {
			return explorer.Neighborhood{}, inconsistentBase()
		}
		admittedEdges[edge.ID] = cloneGraphEdge(edge)
		assertion, hasAssertion := derivedAssertions[edge.ID]
		if operation == auth.OperationAnalyticsRead &&
			edge.Properties[extractionRelationshipIDProperty] != "" &&
			(!hasAssertion || !extractionAssertionMatchesEdge(assertion, edge)) {
			return explorer.Neighborhood{}, inconsistentBase()
		}
		if hasAssertion {
			admittedAssertions[edge.ID] = assertion
		}
	}
	missingProvenanceSeed := false
	for _, seed := range normalized.NodeIDs {
		if _, ok := visibleNodes[seed]; ok {
			continue
		}
		if node, ok := rawNodes[seed]; ok && graph.IsProvenanceKind(node.Kind) {
			if allowMissingProvenanceSeeds {
				missingProvenanceSeed = true
				continue
			}
			// Load-bearing:
			// TestAuthorizedUnboundedNeighborhoodProducerSeedRequiresAuthorization
			// pins that this unbounded Neighborhood-only seed guard returns
			// not-found rather than empty. Empty success is an existence oracle,
			// and Neighborhood has no post-filter provenance seed check.
			return explorer.Neighborhood{}, auth.ObjectNotFound()
		}
		return explorer.Neighborhood{}, inconsistentBase()
	}
	if missingProvenanceSeed {
		return explorer.Neighborhood{}, nil
	}

	reachable := make(map[shoal.ID]struct{}, len(normalized.NodeIDs))
	frontier := make(map[shoal.ID]struct{}, len(normalized.NodeIDs))
	for _, seed := range normalized.NodeIDs {
		reachable[seed] = struct{}{}
		frontier[seed] = struct{}{}
	}
	selectedEdges := make(map[shoal.ID]graph.Edge)
	for depth := uint32(0); depth < normalized.Depth && len(frontier) > 0; depth++ {
		next := make(map[shoal.ID]struct{})
		for edgeID, edge := range admittedEdges {
			reached := reachableThrough(edge, frontier, direction)
			if len(reached) == 0 {
				continue
			}
			selectedEdges[edgeID] = edge
			for _, nodeID := range reached {
				if _, seen := reachable[nodeID]; seen {
					continue
				}
				reachable[nodeID] = struct{}{}
				next[nodeID] = struct{}{}
			}
		}
		frontier = next
	}

	result := explorer.Neighborhood{
		Nodes:      make([]graph.Node, 0, len(reachable)),
		Edges:      make([]graph.Edge, 0, len(selectedEdges)),
		Assertions: make([]ontology.Assertion, 0, len(selectedEdges)),
	}
	for nodeID := range reachable {
		result.Nodes = append(result.Nodes, cloneGraphNode(visibleNodes[nodeID]))
	}
	for _, edge := range selectedEdges {
		result.Edges = append(result.Edges, cloneGraphEdge(edge))
	}
	for edgeID := range selectedEdges {
		if assertion, ok := admittedAssertions[edgeID]; ok {
			result.Assertions = append(result.Assertions, assertion)
		}
	}
	sort.Slice(result.Nodes, func(left, right int) bool {
		return shoal.CompareID(result.Nodes[left].ID, result.Nodes[right].ID) < 0
	})
	sort.Slice(result.Edges, func(left, right int) bool {
		return shoal.CompareID(result.Edges[left].ID, result.Edges[right].ID) < 0
	})
	sort.Slice(result.Assertions, func(left, right int) bool {
		return shoal.CompareID(result.Assertions[left].ID(), result.Assertions[right].ID()) < 0
	})
	return result, nil
}

func reachableThrough(
	edge graph.Edge,
	frontier map[shoal.ID]struct{},
	direction explorer.GraphDirection,
) []shoal.ID {
	switch direction {
	case explorer.GraphDirectionOutgoing:
		if _, ok := frontier[edge.From]; ok {
			return []shoal.ID{edge.To}
		}
	case explorer.GraphDirectionIncoming:
		if _, ok := frontier[edge.To]; ok {
			return []shoal.ID{edge.From}
		}
	default:
		_, from := frontier[edge.From]
		_, to := frontier[edge.To]
		switch {
		case from && to:
			return []shoal.ID{edge.From, edge.To}
		case from:
			return []shoal.ID{edge.To}
		case to:
			return []shoal.ID{edge.From}
		}
	}
	return nil
}

func validateTrustedDerivedAssertions(
	claimed, trusted map[shoal.ID]ontology.Assertion,
) error {
	for id, untrusted := range claimed {
		canonical, ok := trusted[id]
		if !ok || canonical.ID() != id ||
			canonical.Origin() != ontology.AssertionDerived ||
			!assertionsSemanticallyEqual(canonical, untrusted) {
			return inconsistentBase()
		}
	}
	return nil
}

func assertionsSemanticallyEqual(
	left, right ontology.Assertion,
) bool {
	if left.ID() != right.ID() ||
		!metadataSemanticallyEqual(left.Metadata(), right.Metadata()) {
		return false
	}
	leftEvidence := left.Evidence()
	rightEvidence := right.Evidence()
	if len(leftEvidence) != len(rightEvidence) {
		return false
	}
	for index := range leftEvidence {
		if leftEvidence[index].ID() != rightEvidence[index].ID() ||
			!metadataSemanticallyEqual(
				leftEvidence[index].Metadata(),
				rightEvidence[index].Metadata(),
			) {
			return false
		}
	}
	return true
}

func metadataSemanticallyEqual(left, right shoal.Metadata) bool {
	if len(left) != len(right) {
		return false
	}
	for key, leftValue := range left {
		rightValue, ok := right[key]
		if !ok || rightValue != leftValue {
			return false
		}
	}
	return true
}

func derivedAssertionsByEdge(
	assertions []ontology.Assertion,
) (map[shoal.ID]ontology.Assertion, error) {
	byEdge := make(map[shoal.ID]ontology.Assertion, len(assertions))
	for _, assertion := range assertions {
		if err := assertion.Validate(); err != nil {
			return nil, inconsistentBase()
		}
		if assertion.Origin() != ontology.AssertionDerived {
			edgeID := shoal.ID(assertion.Metadata()[graphAssertionEdgeIDMetadata])
			if edgeID == "" {
				// This skip is load-bearing; TestExtractDocumentAuthorizationControlsDerivedGraph pins that cited inferred extraction assertions do not make authorized graph reads fail closed.
				continue
			}
			if err := shoal.ValidateRequiredID("assertion graph edge ID", edgeID); err != nil {
				return nil, inconsistentBase()
			}
			if _, duplicate := byEdge[edgeID]; duplicate {
				return nil, inconsistentBase()
			}
			byEdge[edgeID] = assertion
			continue
		}
		if _, ok := assertion.Object().ReferenceValue(); !ok {
			return nil, inconsistentBase()
		}
		if _, duplicate := byEdge[assertion.ID()]; duplicate {
			return nil, inconsistentBase()
		}
		byEdge[assertion.ID()] = assertion
	}
	return byEdge, nil
}

func derivedAssertionMatchesEdge(assertion ontology.Assertion, edge graph.Edge) bool {
	target, ok := assertion.Object().ReferenceValue()
	if !ok {
		return false
	}
	return edge.ID == assertion.ID() &&
		edge.From == assertion.Subject() &&
		edge.To == target &&
		edge.Type == string(assertion.Predicate()) &&
		scoresEqual(edge.Weight, assertion.Confidence())
}

func extractionAssertionMatchesEdge(
	assertion ontology.Assertion,
	edge graph.Edge,
) bool {
	target, ok := assertion.Object().ReferenceValue()
	if !ok {
		return false
	}
	return edge.From == assertion.Subject() &&
		edge.To == target &&
		edge.Properties[extractionRelationshipIDProperty] ==
			string(assertion.Predicate()) &&
		scoresEqual(edge.Weight, assertion.Confidence())
}

func producerDerivationEdgeMatches(
	edge graph.Edge,
	rawNodes map[shoal.ID]graph.Node,
	assertion ontology.Assertion,
) bool {
	producer, assertionNode, producedEdge, ok, err :=
		explorer.ProducerGraphElementsForAssertion(assertion)
	if err != nil || !ok || !graphEdgesEqual(producedEdge, edge) {
		return false
	}
	rawProducer, ok := rawNodes[producer.ID]
	if !ok || !graphNodesEqual(rawProducer, producer) {
		return false
	}
	rawAssertion, ok := rawNodes[assertionNode.ID]
	if !ok || !graphNodesEqual(rawAssertion, assertionNode) {
		return false
	}
	return true
}

func derivedAssertionEndpointsAllow(
	rawNodes map[shoal.ID]graph.Node,
	visibleNodes map[shoal.ID]graph.Node,
	canonical map[shoal.ID]graph.Node,
	assertion ontology.Assertion,
	allowed bool,
) (bool, error) {
	target, ok := assertion.Object().ReferenceValue()
	if !ok || !allowed {
		return false, nil
	}
	for _, nodeID := range []shoal.ID{assertion.Subject(), target} {
		node, ok := rawNodes[nodeID]
		if !ok || !graphNodesEqual(canonical[nodeID], node) {
			return false, inconsistentBase()
		}
		visibleNodes[nodeID] = cloneGraphNode(node)
	}
	return true, nil
}
