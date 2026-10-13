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

package authorized_test

import (
	"context"
	"fmt"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/phrocker/shoal-oss/pkg/explorer"
	"github.com/phrocker/shoal-oss/pkg/explorer/authorized"
	"github.com/phrocker/shoal-oss/pkg/graph"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

// policyStoreTrips records every policy-store read a page issued, broken down
// by method. Batched and unbatched reads are counted separately so a regression
// from one batch back to a lookup per item is visible as a count, not merely as
// additional latency. Every read on the PolicyStore interface is represented:
// a counter that ignores some round trips manufactures false confidence, so
// TestCountingPolicyStoreCountsEveryPolicyStoreRead fails if a read method is
// added to the interface without being classified here.
type policyStoreTrips struct {
	node             int
	nodes            int
	edge             int
	edges            int
	revision         int
	currentRevision  int
	currentRevisions int
	sourceClaim      int
	// documentRevisionRules and interactionRevisions are the revision
	// binding's batched reads (#695).
	documentRevisionRules int
	revisionRules         int
	interactionRevisions  int

	batchedNodeIDs       int
	batchedEdgeIDs       int
	batchedDocumentIDs   int
	largestNodeBatch     int
	largestEdgeBatch     int
	largestDocumentBatch int
}

// roundTripCounts is the comparable subset of policyStoreTrips holding only
// call counts. The batch-size telemetry is deliberately excluded because it
// legitimately grows with page size; only the number of calls must stay flat.
type roundTripCounts struct {
	node             int
	nodes            int
	edge             int
	edges            int
	revision         int
	currentRevision  int
	currentRevisions int
	sourceClaim      int

	documentRevisionRules int
	revisionRules         int
	interactionRevisions  int
}

// roundTrips is every counted policy-store call, without the batch-size
// telemetry, so two pages can be compared for round-trip equality.
func (t policyStoreTrips) roundTrips() roundTripCounts {
	return roundTripCounts{
		node: t.node, nodes: t.nodes, edge: t.edge, edges: t.edges,
		revision: t.revision, currentRevision: t.currentRevision,
		currentRevisions:      t.currentRevisions,
		sourceClaim:           t.sourceClaim,
		documentRevisionRules: t.documentRevisionRules,
		revisionRules:         t.revisionRules,
		interactionRevisions:  t.interactionRevisions,
	}
}

// total is every policy-store round trip, batched or not.
func (t policyStoreTrips) total() int {
	return t.node + t.nodes + t.edge + t.edges +
		t.revision + t.currentRevision + t.currentRevisions + t.sourceClaim +
		t.documentRevisionRules + t.revisionRules + t.interactionRevisions
}

// perItem is the round trips that still cost one call per item, and is the
// residual N+1 a page pays.
func (t policyStoreTrips) perItem() int {
	return t.node + t.edge + t.revision + t.currentRevision + t.sourceClaim
}

// byMethod exposes every counter keyed by the PolicyStore method it tracks, so
// coverage of the interface can be asserted rather than assumed.
func (t policyStoreTrips) byMethod() map[string]int {
	return map[string]int{
		"Node": t.node, "Nodes": t.nodes, "Edge": t.edge, "Edges": t.edges,
		"Revision": t.revision, "CurrentRevision": t.currentRevision,
		"CurrentRevisions":      t.currentRevisions,
		"SourceClaim":           t.sourceClaim,
		"DocumentRevisionRules": t.documentRevisionRules,
		"RevisionRules":         t.revisionRules,
		"InteractionRevisions":  t.interactionRevisions,
	}
}

// countingPolicyStore is a test-only PolicyStore that counts every policy-store
// read, optionally injects per-round-trip latency, and can withhold identifiers
// from batch results so a partial batch can be exercised without mutating the
// catalog. Withholding deliberately does not affect point lookups: it models a
// batch that returned fewer registrations than were asked for.
type countingPolicyStore struct {
	authorized.PolicyStore
	mu              sync.Mutex
	trips           policyStoreTrips
	latency         time.Duration
	duplicateBatch  bool
	hiddenNodes     map[shoal.ID]struct{}
	hiddenEdges     map[shoal.ID]struct{}
	hiddenRevisions map[shoal.ID]struct{}
}

func newCountingPolicyStore(inner authorized.PolicyStore) *countingPolicyStore {
	return &countingPolicyStore{
		PolicyStore:     inner,
		hiddenNodes:     make(map[shoal.ID]struct{}),
		hiddenEdges:     make(map[shoal.ID]struct{}),
		hiddenRevisions: make(map[shoal.ID]struct{}),
	}
}

func (s *countingPolicyStore) reset() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.trips = policyStoreTrips{}
	s.duplicateBatch = false
}

func (s *countingPolicyStore) snapshot() policyStoreTrips {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.trips
}

func (s *countingPolicyStore) sawDuplicateBatch() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.duplicateBatch
}

func (s *countingPolicyStore) setLatency(latency time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.latency = latency
}

func (s *countingPolicyStore) withholdNodes(ids ...shoal.ID) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, id := range ids {
		s.hiddenNodes[id] = struct{}{}
	}
}

func (s *countingPolicyStore) withholdEdges(ids ...shoal.ID) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, id := range ids {
		s.hiddenEdges[id] = struct{}{}
	}
}

func (s *countingPolicyStore) withholdRevisions(ids ...shoal.ID) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, id := range ids {
		s.hiddenRevisions[id] = struct{}{}
	}
}

// enterRead records one round trip against the supplied counter and applies any
// injected latency outside the lock.
func (s *countingPolicyStore) enterRead(counter func(*policyStoreTrips)) {
	s.mu.Lock()
	counter(&s.trips)
	latency := s.latency
	s.mu.Unlock()
	if latency > 0 {
		time.Sleep(latency)
	}
}

func (s *countingPolicyStore) recordBatch(ids []shoal.ID, edges bool) {
	seen := make(map[shoal.ID]struct{}, len(ids))
	duplicate := false
	for _, id := range ids {
		if _, repeated := seen[id]; repeated {
			duplicate = true
		}
		seen[id] = struct{}{}
	}
	s.mu.Lock()
	s.duplicateBatch = s.duplicateBatch || duplicate
	if edges {
		s.trips.batchedEdgeIDs += len(ids)
		if len(ids) > s.trips.largestEdgeBatch {
			s.trips.largestEdgeBatch = len(ids)
		}
	} else {
		s.trips.batchedNodeIDs += len(ids)
		if len(ids) > s.trips.largestNodeBatch {
			s.trips.largestNodeBatch = len(ids)
		}
	}
	s.mu.Unlock()
}

func (s *countingPolicyStore) Node(
	ctx context.Context,
	nodeID shoal.ID,
) (authorized.NodeRegistration, bool, error) {
	s.enterRead(func(trips *policyStoreTrips) { trips.node++ })
	return s.PolicyStore.Node(ctx, nodeID)
}

func (s *countingPolicyStore) Nodes(
	ctx context.Context,
	nodeIDs []shoal.ID,
) (map[shoal.ID]authorized.NodeRegistration, error) {
	s.recordBatch(nodeIDs, false)
	s.enterRead(func(trips *policyStoreTrips) { trips.nodes++ })
	resolved, err := s.PolicyStore.Nodes(ctx, nodeIDs)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for nodeID := range s.hiddenNodes {
		delete(resolved, nodeID)
	}
	return resolved, nil
}

func (s *countingPolicyStore) Edge(
	ctx context.Context,
	edgeID shoal.ID,
) (authorized.EdgeRegistration, bool, error) {
	s.enterRead(func(trips *policyStoreTrips) { trips.edge++ })
	return s.PolicyStore.Edge(ctx, edgeID)
}

func (s *countingPolicyStore) Edges(
	ctx context.Context,
	edgeIDs []shoal.ID,
) (map[shoal.ID]authorized.EdgeRegistration, error) {
	s.recordBatch(edgeIDs, true)
	s.enterRead(func(trips *policyStoreTrips) { trips.edges++ })
	resolved, err := s.PolicyStore.Edges(ctx, edgeIDs)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for edgeID := range s.hiddenEdges {
		delete(resolved, edgeID)
	}
	return resolved, nil
}

func (s *countingPolicyStore) Revision(
	ctx context.Context,
	documentID, revisionID shoal.ID,
) (authorized.RevisionRegistration, bool, error) {
	s.enterRead(func(trips *policyStoreTrips) { trips.revision++ })
	return s.PolicyStore.Revision(ctx, documentID, revisionID)
}

func (s *countingPolicyStore) CurrentRevision(
	ctx context.Context,
	documentID shoal.ID,
) (authorized.RevisionRegistration, bool, error) {
	s.enterRead(func(trips *policyStoreTrips) { trips.currentRevision++ })
	return s.PolicyStore.CurrentRevision(ctx, documentID)
}

func (s *countingPolicyStore) CurrentRevisions(
	ctx context.Context,
	documentIDs []shoal.ID,
) (map[shoal.ID]authorized.RevisionRegistration, error) {
	seen := make(map[shoal.ID]struct{}, len(documentIDs))
	duplicate := false
	for _, documentID := range documentIDs {
		if _, repeated := seen[documentID]; repeated {
			duplicate = true
		}
		seen[documentID] = struct{}{}
	}
	s.mu.Lock()
	s.duplicateBatch = s.duplicateBatch || duplicate
	s.trips.batchedDocumentIDs += len(documentIDs)
	if len(documentIDs) > s.trips.largestDocumentBatch {
		s.trips.largestDocumentBatch = len(documentIDs)
	}
	s.mu.Unlock()
	s.enterRead(func(trips *policyStoreTrips) { trips.currentRevisions++ })
	resolved, err := s.PolicyStore.CurrentRevisions(ctx, documentIDs)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for documentID := range s.hiddenRevisions {
		delete(resolved, documentID)
	}
	return resolved, nil
}

func (s *countingPolicyStore) SourceClaim(
	ctx context.Context,
	sourceURI string,
) (authorized.SourcePolicyClaim, bool, error) {
	s.enterRead(func(trips *policyStoreTrips) { trips.sourceClaim++ })
	return s.PolicyStore.SourceClaim(ctx, sourceURI)
}

func (s *countingPolicyStore) DocumentRevisionRules(
	ctx context.Context,
	documentIDs []shoal.ID,
) (map[shoal.ID][]authorized.RevisionRule, error) {
	s.enterRead(func(trips *policyStoreTrips) { trips.documentRevisionRules++ })
	return s.PolicyStore.DocumentRevisionRules(ctx, documentIDs)
}

func (s *countingPolicyStore) RevisionRules(
	ctx context.Context,
	references []authorized.RevisionReference,
) (map[shoal.ID][]authorized.RevisionRule, error) {
	s.enterRead(func(trips *policyStoreTrips) { trips.revisionRules++ })
	return s.PolicyStore.RevisionRules(ctx, references)
}

func (s *countingPolicyStore) InteractionRevisions(
	ctx context.Context,
	sessionIDs []shoal.ID,
) (map[shoal.ID][]authorized.RevisionReference, error) {
	s.enterRead(func(trips *policyStoreTrips) { trips.interactionRevisions++ })
	return s.PolicyStore.InteractionRevisions(ctx, sessionIDs)
}

// batchFixture ingests documents and returns their node identifiers along with
// a client whose policy store counts round trips.
func batchFixture(
	t *testing.T,
	documents int,
) (*fixture, *authorized.Client, *countingPolicyStore, []shoal.ID) {
	t.Helper()
	f := newFixture(t)
	admin := f.admin(t)
	nodeIDs := make([]shoal.ID, 0, documents)
	for index := 0; index < documents; index++ {
		result, err := f.clientA.Ingest(admin, explorer.Source{
			URI:       fmt.Sprintf("file:///batch-%d.txt", index),
			MediaType: explorer.MediaTypeText,
			Content:   fmt.Sprintf("batch document %d", index),
		})
		if err != nil {
			t.Fatal(err)
		}
		nodeIDs = append(nodeIDs, result.Document.ID)
	}
	counting := newCountingPolicyStore(f.store)
	client := f.newClient(t, f.base, counting, f.sourceA, f.policyA, nil)
	return f, client, counting, nodeIDs
}

func TestDocumentsBatchesCurrentRevisionReads(t *testing.T) {
	f, client, counting, documents := batchFixture(t, 4)
	counting.reset()
	summaries, err := client.Documents(f.alice(t))
	if err != nil {
		t.Fatal(err)
	}
	if len(summaries) != len(documents) {
		t.Fatalf("documents = %d, want %d", len(summaries), len(documents))
	}
	trips := counting.snapshot()
	if trips.currentRevision != 0 || trips.currentRevisions != 1 {
		t.Fatalf("current revision reads = %d point, %d batch, want 0 and 1",
			trips.currentRevision, trips.currentRevisions)
	}
	if trips.batchedDocumentIDs != len(documents) ||
		trips.largestDocumentBatch != len(documents) {
		t.Fatalf("batched document identifiers = %d (largest %d), want %d",
			trips.batchedDocumentIDs, trips.largestDocumentBatch, len(documents))
	}
	if counting.sawDuplicateBatch() {
		t.Fatal("document batch repeated an identifier")
	}
}

// TestDocumentsDeniesRevisionMissingFromBatchResult is the fail-closed
// mutation guard for revision batching: a document omitted by the batch must
// remain hidden even though its point lookup still succeeds.
func TestDocumentsDeniesRevisionMissingFromBatchResult(t *testing.T) {
	f, client, counting, documents := batchFixture(t, 3)
	counting.withholdRevisions(documents[1])
	summaries, err := client.Documents(f.alice(t))
	if err != nil {
		t.Fatal(err)
	}
	if len(summaries) != len(documents)-1 {
		t.Fatalf("documents = %d, want %d after one omitted registration",
			len(summaries), len(documents)-1)
	}
	for _, summary := range summaries {
		if summary.Document.ID == documents[1] {
			t.Fatal("document omitted from the revision batch was authorized")
		}
	}
}

func connectBatchEdge(
	t *testing.T,
	f *fixture,
	admin context.Context,
	edgeID shoal.ID,
	from, to shoal.ID,
) {
	t.Helper()
	if err := f.clientA.Connect(admin, graph.Edge{
		ID: edgeID, From: from, To: to, Type: "link", Weight: 1,
	}); err != nil {
		t.Fatal(err)
	}
}

// TestNeighborhoodBatchesLookupsPerPage pins the total policy-store round-trip
// count for an authorized neighborhood page, counting every read the store
// serves rather than only node lookups. Doubling the edge count over the same
// node set must not change any counter: both the page's distinct nodes and its
// candidate edges must cost one batch each, with no lookup per edge or per edge
// endpoint.
func TestNeighborhoodBatchesLookupsPerPage(t *testing.T) {
	f, client, counting, documents := batchFixture(t, 4)
	admin := f.admin(t)
	for index := 1; index < len(documents); index++ {
		connectBatchEdge(
			t, f, admin, shoal.ID(fmt.Sprintf("star-%d", index)),
			documents[0], documents[index])
	}
	request := explorer.NeighborhoodRequest{
		NodeIDs: []shoal.ID{documents[0]}, Depth: 2, EdgeTypes: []string{"link"},
	}

	counting.reset()
	sparse, err := client.Neighborhood(f.alice(t), request)
	if err != nil {
		t.Fatal(err)
	}
	sparseTrips := counting.snapshot()
	if len(sparse.Edges) != 3 || len(sparse.Nodes) != 4 {
		t.Fatalf("sparse neighborhood = %d nodes %d edges",
			len(sparse.Nodes), len(sparse.Edges))
	}

	connectBatchEdge(t, f, admin, "mesh-1-2", documents[1], documents[2])
	connectBatchEdge(t, f, admin, "mesh-1-3", documents[1], documents[3])
	connectBatchEdge(t, f, admin, "mesh-2-3", documents[2], documents[3])

	counting.reset()
	dense, err := client.Neighborhood(f.alice(t), request)
	if err != nil {
		t.Fatal(err)
	}
	denseTrips := counting.snapshot()
	if len(dense.Edges) != 6 || len(dense.Nodes) != 4 {
		t.Fatalf("dense neighborhood = %d nodes %d edges",
			len(dense.Nodes), len(dense.Edges))
	}

	if counting.sawDuplicateBatch() {
		t.Fatal("batch lookup repeated an identifier instead of deduplicating")
	}
	t.Logf("policy store reads: 3 edges %+v (total %d, per-item %d), "+
		"6 edges %+v (total %d, per-item %d)",
		sparseTrips, sparseTrips.total(), sparseTrips.perItem(),
		denseTrips, denseTrips.total(), denseTrips.perItem())
	// Doubling the edge count must not move any counter. This is the assertion
	// that fails if either the per-endpoint node lookup or the per-edge Edge
	// lookup is reintroduced.
	if sparseTrips.roundTrips() != denseTrips.roundTrips() {
		t.Fatalf(
			"policy store reads scale with edge count: 3 edges = %+v, 6 edges = %+v",
			sparseTrips, denseTrips)
	}
	if denseTrips.nodes != 1 {
		t.Fatalf("node batch lookups = %d, want exactly one per page",
			denseTrips.nodes)
	}
	if denseTrips.edges != 1 {
		t.Fatalf("edge batch lookups = %d, want exactly one per page",
			denseTrips.edges)
	}
	if denseTrips.edge != 0 {
		t.Fatalf("per-edge point lookups = %d, want none", denseTrips.edge)
	}
	if denseTrips.node != len(request.NodeIDs) {
		t.Fatalf("node point lookups = %d, want %d (seed authorization only)",
			denseTrips.node, len(request.NodeIDs))
	}
	if denseTrips.largestNodeBatch != len(dense.Nodes) {
		t.Fatalf("batched node identifiers = %d, want %d distinct page nodes",
			denseTrips.largestNodeBatch, len(dense.Nodes))
	}
	if denseTrips.largestEdgeBatch != len(dense.Edges) {
		t.Fatalf("batched edge identifiers = %d, want %d candidate page edges",
			denseTrips.largestEdgeBatch, len(dense.Edges))
	}
	if denseTrips.currentRevision != 0 || denseTrips.currentRevisions != 2 {
		t.Fatalf("current revision reads = %d point, %d batch, want 0 and 2",
			denseTrips.currentRevision, denseTrips.currentRevisions)
	}
	// Resolving each item individually costs one seed lookup, one lookup per
	// page node, two per admitted edge and one Edge lookup per candidate edge.
	// Recording it keeps the regression this test guards explicit.
	perItemBaseline := len(request.NodeIDs) + len(dense.Nodes) +
		3*len(dense.Edges) + len(dense.Nodes)
	if denseTrips.total() >= perItemBaseline {
		t.Fatalf("policy store reads = %d, want fewer than the %d per-item lookups",
			denseTrips.total(), perItemBaseline)
	}
}

// TestBoundedNeighborhoodBatchesEndpointLookupsPerPage pins the same round-trip
// bound for the bounded traversal path, which filters each scanned page.
func TestBoundedNeighborhoodBatchesEndpointLookupsPerPage(t *testing.T) {
	f, client, counting, documents := batchFixture(t, 4)
	admin := f.admin(t)
	for index := 1; index < len(documents); index++ {
		connectBatchEdge(
			t, f, admin, shoal.ID(fmt.Sprintf("bounded-%d", index)),
			documents[0], documents[index])
	}
	request := explorer.BoundedNeighborhoodRequest{
		NodeIDs: []shoal.ID{documents[0]}, Depth: 1, Fanout: 16, MaxNodes: 16,
		MaxScannedEdges: 16, EdgeTypes: []string{"link"},
		Direction: explorer.GraphDirectionOutgoing,
	}
	counting.reset()
	page, err := client.BoundedNeighborhood(f.alice(t), request)
	if err != nil {
		t.Fatal(err)
	}
	trips := counting.snapshot()
	if len(page.Neighborhood.Edges) != 3 {
		t.Fatalf("bounded page edges = %d, want 3", len(page.Neighborhood.Edges))
	}
	if counting.sawDuplicateBatch() {
		t.Fatal("bounded batch lookup repeated an identifier")
	}
	t.Logf("bounded policy store reads %+v (total %d, per-item %d)",
		trips, trips.total(), trips.perItem())
	if trips.edge != 0 {
		t.Fatalf("bounded per-edge point lookups = %d, want none", trips.edge)
	}
	if trips.nodes != 1 || trips.edges != 1 {
		t.Fatalf("bounded batch lookups = %d node, %d edge, want one of each",
			trips.nodes, trips.edges)
	}
	if trips.currentRevision != 0 || trips.currentRevisions != 2 {
		t.Fatalf("bounded current revision reads = %d point, %d batch, want 0 and 2",
			trips.currentRevision, trips.currentRevisions)
	}
	perItemBaseline := len(request.NodeIDs) + len(page.Neighborhood.Nodes) +
		3*len(page.Neighborhood.Edges) + len(page.Neighborhood.Nodes)
	if trips.total() >= perItemBaseline {
		t.Fatalf("bounded reads = %+v, want fewer than %d per-item lookups",
			trips, perItemBaseline)
	}
	if trips.node != len(request.NodeIDs) {
		t.Fatalf("bounded node point lookups = %d, want %d (seed authorization only)",
			trips.node, len(request.NodeIDs))
	}
}

// TestNeighborhoodRoundTripLatencyTracksBatchCount confirms the fake's injected
// latency is charged once per round trip, so the recorded counts are the real
// cost a distributed policy store would pay. Only the lower bound is measured
// on the clock, which is deterministic because Sleep guarantees a minimum; the
// upper bound is asserted on the counters.
func TestNeighborhoodRoundTripLatencyTracksBatchCount(t *testing.T) {
	f, client, counting, documents := batchFixture(t, 4)
	admin := f.admin(t)
	for index := 1; index < len(documents); index++ {
		connectBatchEdge(
			t, f, admin, shoal.ID(fmt.Sprintf("latency-%d", index)),
			documents[0], documents[index])
	}
	const latency = 20 * time.Millisecond
	counting.reset()
	counting.setLatency(latency)
	started := time.Now()
	neighborhood, err := client.Neighborhood(f.alice(t), explorer.NeighborhoodRequest{
		NodeIDs: []shoal.ID{documents[0]}, Depth: 2, EdgeTypes: []string{"link"},
	})
	elapsed := time.Since(started)
	counting.setLatency(0)
	if err != nil {
		t.Fatal(err)
	}
	trips := counting.snapshot()
	if len(neighborhood.Edges) != 3 {
		t.Fatalf("neighborhood edges = %d, want 3", len(neighborhood.Edges))
	}
	if elapsed < time.Duration(trips.total())*latency {
		t.Fatalf("elapsed %s is below the %d injected round trips",
			elapsed, trips.total())
	}
	perItemBaseline := 1 + len(neighborhood.Nodes) +
		3*len(neighborhood.Edges) + len(neighborhood.Nodes)
	// The upper bound is asserted on the counters, not the clock. A wall-clock
	// ceiling is nondeterministic — Sleep guarantees only a minimum, so a
	// loaded host can push a correctly batched call past any budget — and it is
	// simultaneously too loose to catch a real regression, because a handful of
	// reintroduced per-edge lookups stays comfortably under it. Round-trip
	// count is the property this test exists to bound, and the counters measure
	// it directly rather than through elapsed time.
	if trips.total() >= perItemBaseline {
		t.Fatalf("policy store reads = %d, want fewer than the %d per-item lookups",
			trips.total(), perItemBaseline)
	}
	// The baseline comparison alone is loose enough that a few reintroduced
	// point lookups would still pass it, so the per-item counters this change
	// drove to zero are pinned directly as well.
	if trips.edge != 0 {
		t.Fatalf("per-edge point lookups = %d, want none", trips.edge)
	}
	if trips.nodes != 1 || trips.edges != 1 {
		t.Fatalf("batch lookups = %d node and %d edge, want one of each",
			trips.nodes, trips.edges)
	}
	if trips.currentRevision != 0 || trips.currentRevisions != 2 {
		t.Fatalf("current revision reads = %d point, %d batch, want 0 and 2",
			trips.currentRevision, trips.currentRevisions)
	}
	t.Logf("policy store reads %+v (total %d) cost %s at %s per round trip",
		trips, trips.total(), elapsed, latency)
}

// TestNeighborhoodDeniesNodeMissingFromBatchResult verifies that batching did
// not turn a missing node into an allow. A node the batch result omits must be
// excluded exactly as a point lookup reporting !ok excludes it, and every edge
// incident to it must be denied.
func TestNeighborhoodDeniesNodeMissingFromBatchResult(t *testing.T) {
	f, client, counting, documents := batchFixture(t, 4)
	admin := f.admin(t)
	for index := 1; index < len(documents); index++ {
		connectBatchEdge(
			t, f, admin, shoal.ID(fmt.Sprintf("missing-%d", index)),
			documents[0], documents[index])
	}
	request := explorer.NeighborhoodRequest{
		NodeIDs: []shoal.ID{documents[0]}, Depth: 2, EdgeTypes: []string{"link"},
	}
	before, err := client.Neighborhood(f.alice(t), request)
	if err != nil {
		t.Fatal(err)
	}
	if len(before.Edges) != 3 || !hasNode(before, documents[1]) {
		t.Fatalf("baseline neighborhood = %#v", before)
	}

	counting.withholdNodes(documents[1])
	after, err := client.Neighborhood(f.alice(t), request)
	if err != nil {
		t.Fatal(err)
	}
	if hasNode(after, documents[1]) {
		t.Fatal("node absent from the batch result was authorized")
	}
	for _, edge := range after.Edges {
		if edge.From == documents[1] || edge.To == documents[1] {
			t.Fatalf("edge %q kept an endpoint absent from the batch result", edge.ID)
		}
	}
	if len(after.Edges) != 2 ||
		!hasNode(after, documents[2]) || !hasNode(after, documents[3]) {
		t.Fatalf("partial batch result denied more than the missing node: %#v", after)
	}
}

// TestBoundedNeighborhoodDeniesNodeMissingFromBatchResult repeats the
// fail-closed assertion on the bounded traversal path.
func TestBoundedNeighborhoodDeniesNodeMissingFromBatchResult(t *testing.T) {
	f, client, counting, documents := batchFixture(t, 4)
	admin := f.admin(t)
	for index := 1; index < len(documents); index++ {
		connectBatchEdge(
			t, f, admin, shoal.ID(fmt.Sprintf("bounded-missing-%d", index)),
			documents[0], documents[index])
	}
	request := explorer.BoundedNeighborhoodRequest{
		NodeIDs: []shoal.ID{documents[0]}, Depth: 1, Fanout: 16, MaxNodes: 16,
		MaxScannedEdges: 16, EdgeTypes: []string{"link"},
		Direction: explorer.GraphDirectionOutgoing,
	}
	counting.withholdNodes(documents[2])
	page, err := client.BoundedNeighborhood(f.alice(t), request)
	if err != nil {
		t.Fatal(err)
	}
	if hasNode(page.Neighborhood, documents[2]) {
		t.Fatal("bounded page authorized a node absent from the batch result")
	}
	for _, edge := range page.Neighborhood.Edges {
		if edge.From == documents[2] || edge.To == documents[2] {
			t.Fatalf("bounded page kept edge %q with a missing endpoint", edge.ID)
		}
	}
	if len(page.Neighborhood.Edges) != 2 {
		t.Fatalf("bounded page edges = %d, want 2", len(page.Neighborhood.Edges))
	}
}

// TestNeighborhoodDeniesEveryEdgeWhenBatchResultIsEmpty verifies a wholesale
// empty batch result authorizes nothing. Because the seed itself is then
// unresolvable, this surfaces as an inconsistent-base error rather than an
// empty page, which is still a denial and never a degradation to an allow.
func TestNeighborhoodDeniesEveryEdgeWhenBatchResultIsEmpty(t *testing.T) {
	f, client, counting, documents := batchFixture(t, 3)
	admin := f.admin(t)
	for index := 1; index < len(documents); index++ {
		connectBatchEdge(
			t, f, admin, shoal.ID(fmt.Sprintf("empty-%d", index)),
			documents[0], documents[index])
	}
	counting.withholdNodes(documents...)
	_, err := client.Neighborhood(f.alice(t), explorer.NeighborhoodRequest{
		NodeIDs: []shoal.ID{documents[0]}, Depth: 2, EdgeTypes: []string{"link"},
	})
	if !shoal.IsErrorCode(err, shoal.ErrorInternal) {
		t.Fatalf("empty batch result error = %v, want an inconsistent-base denial", err)
	}
}

// TestNeighborhoodDeniesEdgeMissingFromBatchResult is the fail-closed mutation
// for the batch edge path: an edge the batch result omits must be denied
// exactly as a point Edge lookup reporting !ok denies it, and must not leak
// into the page merely because its endpoints were authorized.
func TestNeighborhoodDeniesEdgeMissingFromBatchResult(t *testing.T) {
	f, client, counting, documents := batchFixture(t, 4)
	admin := f.admin(t)
	edgeIDs := make([]shoal.ID, 0, len(documents)-1)
	for index := 1; index < len(documents); index++ {
		edgeID := shoal.ID(fmt.Sprintf("missing-edge-%d", index))
		connectBatchEdge(t, f, admin, edgeID, documents[0], documents[index])
		edgeIDs = append(edgeIDs, edgeID)
	}
	request := explorer.NeighborhoodRequest{
		NodeIDs: []shoal.ID{documents[0]}, Depth: 2, EdgeTypes: []string{"link"},
	}
	before, err := client.Neighborhood(f.alice(t), request)
	if err != nil {
		t.Fatal(err)
	}
	if len(before.Edges) != 3 {
		t.Fatalf("baseline edges = %d, want 3", len(before.Edges))
	}

	counting.withholdEdges(edgeIDs[0])
	after, err := client.Neighborhood(f.alice(t), request)
	if err != nil {
		t.Fatal(err)
	}
	for _, edge := range after.Edges {
		if edge.ID == edgeIDs[0] {
			t.Fatal("edge absent from the batch result was authorized")
		}
	}
	if len(after.Edges) != 2 {
		t.Fatalf("partial edge batch denied more than the missing edge: %#v", after.Edges)
	}
	if hasNode(after, documents[1]) {
		t.Fatal("a node reachable only through the missing edge stayed in the page")
	}

	counting.withholdEdges(edgeIDs...)
	empty, err := client.Neighborhood(f.alice(t), request)
	if err != nil {
		t.Fatal(err)
	}
	if len(empty.Edges) != 0 {
		t.Fatalf("empty edge batch authorized %d edges", len(empty.Edges))
	}
}

// TestBoundedNeighborhoodDeniesEdgeMissingFromBatchResult repeats the missing
// edge mutation on the bounded traversal path.
func TestBoundedNeighborhoodDeniesEdgeMissingFromBatchResult(t *testing.T) {
	f, client, counting, documents := batchFixture(t, 4)
	admin := f.admin(t)
	for index := 1; index < len(documents); index++ {
		connectBatchEdge(
			t, f, admin, shoal.ID(fmt.Sprintf("bounded-missing-edge-%d", index)),
			documents[0], documents[index])
	}
	request := explorer.BoundedNeighborhoodRequest{
		NodeIDs: []shoal.ID{documents[0]}, Depth: 1, Fanout: 16, MaxNodes: 16,
		MaxScannedEdges: 16, EdgeTypes: []string{"link"},
		Direction: explorer.GraphDirectionOutgoing,
	}
	counting.withholdEdges("bounded-missing-edge-2")
	page, err := client.BoundedNeighborhood(f.alice(t), request)
	if err != nil {
		t.Fatal(err)
	}
	for _, edge := range page.Neighborhood.Edges {
		if edge.ID == "bounded-missing-edge-2" {
			t.Fatal("bounded page authorized an edge absent from the batch result")
		}
	}
	if len(page.Neighborhood.Edges) != 2 {
		t.Fatalf("bounded page edges = %d, want 2", len(page.Neighborhood.Edges))
	}
}

// countedPolicyStoreReads are the PolicyStore reads countingPolicyStore
// overrides and counts. uncountedPolicyStoreMethods are the leases and writes it
// deliberately does not count.
var (
	countedPolicyStoreReads = map[string]struct{}{
		"Node": {}, "Nodes": {}, "Edge": {}, "Edges": {},
		"Revision": {}, "CurrentRevision": {}, "CurrentRevisions": {},
		"SourceClaim": {}, "DocumentRevisionRules": {}, "RevisionRules": {},
		"InteractionRevisions": {},
	}
	uncountedPolicyStoreMethods = map[string]struct{}{
		"AcquireMutation": {}, "CompareAndSwapSourceClaim": {},
		"CommitSourceClaim": {}, "PendSourceClaim": {}, "RollbackSourceClaim": {},
		"PutRevision": {}, "ReserveEdge": {}, "RollbackEdgeReservation": {},
		"PutEdge": {}, "PutNode": {},
		// The startup label migration (#570) is the only caller of these;
		// no request read path reaches them.
		"TightenRule": {}, "TighteningIndex": {},
		"LabelMigration": {}, "PutLabelMigration": {},
		// The declassification audit (#630): an append written by Ingest
		// and an operator read no request read path reaches.
		"RecordDeclassification": {}, "Declassifications": {},
		// The revision binding's write (#695), made while recording.
		"PutInteractionRevisions": {},
	}
)

// TestCountingPolicyStoreCountsEveryPolicyStoreRead makes the round-trip
// counter honest by construction. countingPolicyStore embeds PolicyStore, so a
// read it does not override is silently promoted and silently uncounted, which
// is exactly how a per-edge Edge lookup previously hid behind a counter that
// only tracked node lookups. Every interface method must therefore be
// classified as counted or deliberately uncounted, and adding a method to
// PolicyStore fails this test until it is.
func TestCountingPolicyStoreCountsEveryPolicyStoreRead(t *testing.T) {
	policyStore := reflect.TypeOf((*authorized.PolicyStore)(nil)).Elem()
	declared := make(map[string]struct{}, policyStore.NumMethod())
	for index := 0; index < policyStore.NumMethod(); index++ {
		name := policyStore.Method(index).Name
		declared[name] = struct{}{}
		_, counted := countedPolicyStoreReads[name]
		_, uncounted := uncountedPolicyStoreMethods[name]
		if counted == uncounted {
			t.Fatalf(
				"PolicyStore.%s is unclassified: add it to countedPolicyStoreReads "+
					"with a counting override if it is a read, or to "+
					"uncountedPolicyStoreMethods if it is not", name)
		}
	}
	for name := range countedPolicyStoreReads {
		if _, ok := declared[name]; !ok {
			t.Fatalf("countedPolicyStoreReads names %q, which PolicyStore does not declare", name)
		}
	}
	for name := range uncountedPolicyStoreMethods {
		if _, ok := declared[name]; !ok {
			t.Fatalf("uncountedPolicyStoreMethods names %q, which PolicyStore does not declare", name)
		}
	}
	// Every counted read must actually be overridden on the concrete fake
	// rather than promoted from the embedded store. Invoking each one and
	// checking its counter moved catches a name that is listed as counted but
	// silently promoted, which is the exact failure mode being guarded.
	ctx := context.Background()
	invocations := map[string]func(*countingPolicyStore){
		"Node": func(s *countingPolicyStore) {
			_, _, _ = s.Node(ctx, "node-a")
		},
		"Nodes": func(s *countingPolicyStore) {
			_, _ = s.Nodes(ctx, []shoal.ID{"node-a"})
		},
		"Edge": func(s *countingPolicyStore) {
			_, _, _ = s.Edge(ctx, "edge-a")
		},
		"Edges": func(s *countingPolicyStore) {
			_, _ = s.Edges(ctx, []shoal.ID{"edge-a"})
		},
		"Revision": func(s *countingPolicyStore) {
			_, _, _ = s.Revision(ctx, "document-a", "revision-a")
		},
		"CurrentRevision": func(s *countingPolicyStore) {
			_, _, _ = s.CurrentRevision(ctx, "document-a")
		},
		"CurrentRevisions": func(s *countingPolicyStore) {
			_, _ = s.CurrentRevisions(ctx, []shoal.ID{"document-a"})
		},
		"SourceClaim": func(s *countingPolicyStore) {
			_, _, _ = s.SourceClaim(ctx, "file:///source-a.txt")
		},
		"DocumentRevisionRules": func(s *countingPolicyStore) {
			_, _ = s.DocumentRevisionRules(ctx, []shoal.ID{"document-a"})
		},
		"RevisionRules": func(s *countingPolicyStore) {
			_, _ = s.RevisionRules(ctx, []authorized.RevisionReference{{
				DocumentID: "document-a", RevisionID: "revision-a",
			}})
		},
		"InteractionRevisions": func(s *countingPolicyStore) {
			_, _ = s.InteractionRevisions(ctx, []shoal.ID{"session-a"})
		},
	}
	if len(invocations) != len(countedPolicyStoreReads) {
		t.Fatalf("counted reads = %d but invocations = %d",
			len(countedPolicyStoreReads), len(invocations))
	}
	for name, invoke := range invocations {
		if _, ok := countedPolicyStoreReads[name]; !ok {
			t.Fatalf("invocation %q is not a counted read", name)
		}
		counting := newCountingPolicyStore(authorized.NewMemoryPolicyStore())
		invoke(counting)
		trips := counting.snapshot()
		if got := trips.byMethod()[name]; got != 1 {
			t.Fatalf("%s counter = %d after one call, want 1", name, got)
		}
		if trips.total() != 1 {
			t.Fatalf("%s recorded %d total reads, want 1", name, trips.total())
		}
	}
}
