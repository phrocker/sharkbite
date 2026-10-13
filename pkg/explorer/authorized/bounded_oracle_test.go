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

// The bounded neighborhood oracle (#698). The authorized scan limit used to
// be the caller's fanout, so the outcome of a bounded read (503 versus
// success, the page, the cursor, Truncated, the scan count) depended on how
// much adjacency the reader could not see: sweeping fanout 1..N on a visible
// node counted its hidden Connect edges and interactions exactly. The tests
// below sweep every bounded read shape before and after hidden adjacency is
// added and require byte-identical outcomes, and pin the one documented
// uniform failure that remains: a node whose total raw adjacency exceeds the
// server's MaxBoundedNodeAdjacency.

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/phrocker/shoal-oss/pkg/explorer"
	"github.com/phrocker/shoal-oss/pkg/explorer/auth"
	"github.com/phrocker/shoal-oss/pkg/explorer/authorized"
	"github.com/phrocker/shoal-oss/pkg/graph"
	"github.com/phrocker/shoal-oss/pkg/interaction"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

const oracleEdgeType = "link"

// scanRecordingBase records every bounded scan the authorized client asks
// of the real Explorer, so a test can assert the server work stays bounded.
type scanRecordingBase struct {
	*explorer.Explorer
	mu       sync.Mutex
	requests []explorer.BoundedNeighborhoodRequest
	scanned  []uint32
	// beforeScan, when set, runs before the scan with the number of scans
	// recorded so far, outside the lock, so it may write to the corpus.
	beforeScan func(recorded int)
}

func (b *scanRecordingBase) BoundedNeighborhood(
	ctx context.Context, request explorer.BoundedNeighborhoodRequest,
) (explorer.BoundedNeighborhood, error) {
	b.mu.Lock()
	hook, recorded := b.beforeScan, len(b.requests)
	b.mu.Unlock()
	if hook != nil {
		hook(recorded)
	}
	result, err := b.Explorer.BoundedNeighborhood(ctx, request)
	b.mu.Lock()
	defer b.mu.Unlock()
	b.requests = append(b.requests, request)
	b.scanned = append(b.scanned, result.ScannedEdges)
	return result, err
}

func (b *scanRecordingBase) reset() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.requests, b.scanned = nil, nil
}

type oracleWorld struct {
	t      *testing.T
	f      *fixture
	base   *scanRecordingBase
	client *authorized.Client
	limit  uint32
	docs   map[string]shoal.ID
	views  map[string]explorer.DocumentView
	hidden int
}

func newOracleWorld(t *testing.T, store authorized.PolicyStore, limit uint32) *oracleWorld {
	t.Helper()
	f := newFixture(t)
	base := &scanRecordingBase{Explorer: f.base}
	selector := f.staticSelector(t, f.sourceA, f.policyA)
	proposals, _ := explorer.Client(base).(explorer.OntologyProposalStore)
	client, err := authorized.NewClient(authorized.Config{
		Base:                    base,
		VectorScorer:            trustedVectorScorer(base),
		InteractionWriter:       trustedInteractionWriter(base),
		InteractionReader:       trustedInteractionReader(base),
		SnapshotValidator:       trustedSnapshotValidator(base),
		DerivedAssertionReader:  trustedDerivedAssertionReader(base),
		FoldStore:               trustedFoldStore(base),
		OntologyProposalStore:   proposals,
		Resolver:                f.authority.Resolver(),
		PolicySelector:          selector,
		EdgePolicySelector:      selector,
		PolicyStore:             store,
		GenerationReader:        f.reader,
		Clock:                   f.clock.Now,
		MaxBoundedNodeAdjacency: limit,
	})
	if err != nil {
		t.Fatal(err)
	}
	w := &oracleWorld{
		t: t, f: f, base: base, client: client, limit: limit,
		docs: map[string]shoal.ID{}, views: map[string]explorer.DocumentView{},
	}
	// The visible graph: P is the public node under test, with outgoing,
	// incoming and depth-two adjacency. Edge IDs leave gaps so hidden edges
	// can interleave with them in ID order.
	for _, name := range []string{"P", "V1", "V2", "V3", "W"} {
		w.ingest(name, "")
	}
	for _, edge := range []struct{ id, from, to string }{
		{"e-b", "P", "V1"}, {"e-d", "P", "V2"}, {"e-f", "V3", "P"},
		{"e-h", "V1", "W"}, {"e-j", "P", "V3"}, {"e-l", "W", "V2"},
	} {
		w.connect(edge.id, edge.from, edge.to)
	}
	return w
}

func (w *oracleWorld) owner() context.Context {
	return w.f.labelIngester(w.t, "owner", "secret")
}

func (w *oracleWorld) reader() context.Context {
	return w.f.labelIngester(w.t, "reader")
}

func (w *oracleWorld) ingest(name, visibility string) shoal.ID {
	w.t.Helper()
	result, err := w.client.Ingest(w.owner(), labelledSource(
		"file:///oracle/"+name+".txt", "oracle document "+name, visibility))
	if err != nil {
		w.t.Fatalf("ingest %s: %v", name, err)
	}
	view, err := w.client.Document(w.owner(), result.Document.ID, result.Revision.ID)
	if err != nil {
		w.t.Fatal(err)
	}
	w.docs[name] = result.Document.ID
	w.views[name] = view
	return result.Document.ID
}

func (w *oracleWorld) connect(id, from, to string) {
	w.t.Helper()
	if err := w.client.Connect(w.owner(), graph.Edge{
		ID: shoal.ID(id), From: w.docs[from], To: w.docs[to],
		Type: oracleEdgeType, Weight: 1,
	}); err != nil {
		w.t.Fatalf("connect %s: %v", id, err)
	}
}

// hide adds a secret document and connects it to (or from) a visible node
// with the given edge ID, as a label holder can.
func (w *oracleWorld) hide(edgeID, visible string, outgoing bool) {
	w.t.Helper()
	w.hidden++
	name := fmt.Sprintf("S%03d", w.hidden)
	w.ingest(name, "secret")
	if outgoing {
		w.connect(edgeID, visible, name)
	} else {
		w.connect(edgeID, name, visible)
	}
}

// record records an interaction seeded on the given documents, by the label
// holder, so it touches both the public node and a secret one.
func (w *oracleWorld) record(id string, names ...string) {
	w.t.Helper()
	snapshot, err := w.f.base.Snapshot(context.Background())
	if err != nil {
		w.t.Fatal(err)
	}
	if now := w.f.clock.Now(); snapshot.AsOf.Add(time.Second).After(now) {
		w.f.clock.Set(snapshot.AsOf.Add(time.Second))
	}
	decision := w.f.decision(w.t, "owner",
		[][]byte{w.f.sourceA, w.f.sourceB},
		[][]byte{w.f.policyA, w.f.policyB, labelPolicy(w.t, w.f.sourceA, "secret")},
		labelReaderOperations)
	fingerprint, err := auth.AuthorizationFingerprint(decision)
	if err != nil {
		w.t.Fatal(err)
	}
	seeds := make([]shoal.ID, 0, 2*len(names))
	for _, name := range names {
		seeds = append(seeds, w.docs[name], firstSpanID(w.t, w.views[name]))
	}
	if err := w.client.RecordInteraction(w.f.context(w.t, decision), interaction.Session{
		ID:                       interaction.DerivedID("session", id),
		RecordedAt:               w.f.clock.Now(),
		SnapshotID:               shoal.ID(snapshot.ID),
		SnapshotAsOf:             snapshot.AsOf,
		AuthorizationFingerprint: shoal.ID(fingerprint.String()),
		AuthorizationExpiresAt:   decision.AuthenticationExpires(),
		SeedNodeIDs:              seeds,
	}); err != nil {
		w.t.Fatalf("record %s: %v", id, err)
	}
}

// outcome renders everything a bounded read reports, so two outcomes are
// equal exactly when a caller could not tell them apart.
func boundedOutcome(result explorer.BoundedNeighborhood, err error) string {
	if err != nil {
		code := "other"
		for _, candidate := range []shoal.ErrorCode{
			shoal.ErrorUnavailable, shoal.ErrorInternal, shoal.ErrorNotFound,
			shoal.ErrorInvalidArgument, shoal.ErrorConflict,
			shoal.ErrorUnauthorized,
		} {
			if shoal.IsErrorCode(err, candidate) {
				code = string(candidate)
			}
		}
		return "error " + code + ": " + err.Error()
	}
	var builder strings.Builder
	neighborhood := result.Neighborhood
	builder.WriteString("nodes:")
	for _, node := range neighborhood.Nodes {
		fmt.Fprintf(&builder, " %s/%s/%v", node.ID, node.Kind, node.Properties)
	}
	builder.WriteString(" edges:")
	for _, edge := range neighborhood.Edges {
		fmt.Fprintf(&builder, " %s(%s>%s %s)", edge.ID, edge.From, edge.To, edge.Type)
	}
	builder.WriteString(" assertions:")
	for _, assertion := range neighborhood.Assertions {
		fmt.Fprintf(&builder, " %s", assertion.ID())
	}
	fmt.Fprintf(&builder,
		" interpretations:%d truncated:%t next:%q continuation:%t scanned:%d known:%t",
		len(neighborhood.Interpretations), result.Truncated,
		result.NextAfterEdgeID, result.Continuation,
		result.ScannedEdges, result.ScannedEdgesKnown)
	return builder.String()
}

// sweep runs every bounded read shape against the visible graph and returns
// each outcome by name. Single-seed depth-one reads are paged to completion.
func (w *oracleWorld) sweep(ctx context.Context, maxFanout uint32) map[string]string {
	w.t.Helper()
	outcomes := make(map[string]string)
	// Every page and traversal must also equal the reference computed from
	// the reader's visible graph alone.
	reference := newVisibleReference(w.t, ctx, w.client)
	p, v1, w1 := w.docs["P"], w.docs["V1"], w.docs["W"]
	seedSets := map[string][]shoal.ID{
		"P": {p}, "P+V1": {p, v1}, "V1+W": {v1, w1},
	}
	directions := []explorer.GraphDirection{
		explorer.GraphDirectionBoth, explorer.GraphDirectionOutgoing,
		explorer.GraphDirectionIncoming,
	}
	for _, direction := range directions {
		for _, types := range [][]string{nil, {oracleEdgeType}} {
			for fanout := uint32(1); fanout <= maxFanout; fanout++ {
				for _, maxNodes := range []uint32{2, 3, 64} {
					key := fmt.Sprintf("%s/%v/f%d/n%d", direction, types, fanout, maxNodes)
					// Paged single seed, depth one, followed to completion.
					after := shoal.ID("")
					for page := 0; ; page++ {
						result, err := w.client.BoundedNeighborhood(ctx,
							explorer.BoundedNeighborhoodRequest{
								NodeIDs: []shoal.ID{p}, Depth: 1, Fanout: fanout,
								MaxNodes: maxNodes, EdgeTypes: types,
								Direction: direction, AfterEdgeID: after,
								MaxScannedEdges: fanout,
							})
						pageKey := fmt.Sprintf("page/%s/%d", key, page)
						outcomes[pageKey] = boundedOutcome(result, err)
						requireReference(w.t, pageKey, result, err, reference.page(
							p, after, fanout, maxNodes, direction, types))
						if err != nil || !result.Continuation {
							break
						}
						if page > 64 {
							w.t.Fatalf("%s: paging did not terminate", key)
						}
						after = result.NextAfterEdgeID
					}
					for name, seeds := range seedSets {
						for _, depth := range []uint32{1, 2, 3} {
							if name == "P" && depth == 1 {
								continue
							}
							result, err := w.client.BoundedNeighborhood(ctx,
								explorer.BoundedNeighborhoodRequest{
									NodeIDs: seeds, Depth: depth, Fanout: fanout,
									MaxNodes: maxNodes, EdgeTypes: types,
									Direction: direction,
								})
							traverseKey := fmt.Sprintf("traverse/%s/%s/d%d", key, name, depth)
							outcomes[traverseKey] = boundedOutcome(result, err)
							requireReference(w.t, traverseKey, result, err, reference.traverse(
								seeds, depth, fanout, maxNodes, direction, types))
						}
					}
					for _, depth := range []uint32{1, 2} {
						materialized, err := w.client.MaterializeAnalytics(ctx,
							explorer.BoundedNeighborhoodRequest{
								NodeIDs: []shoal.ID{p}, Depth: depth, Fanout: fanout,
								MaxNodes: maxNodes, EdgeTypes: types,
								Direction: direction, MaxScannedEdges: fanout,
							}, 64, 1<<20)
						outcomes[fmt.Sprintf("analytics/%s/d%d", key, depth)] =
							boundedOutcome(explorer.BoundedNeighborhood{
								Neighborhood: materialized.Neighborhood,
								Truncated:    !materialized.Complete,
							}, err)
					}
				}
			}
		}
	}
	return outcomes
}

func requireSameOutcomes(t *testing.T, before, after map[string]string) {
	t.Helper()
	keys := make([]string, 0, len(before))
	for key := range before {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	failures := 0
	for _, key := range keys {
		if after[key] != before[key] {
			failures++
			if failures <= 10 {
				t.Errorf("%s differs once hidden adjacency exists:\n before: %s\n after:  %s",
					key, before[key], after[key])
			}
		}
	}
	for key := range after {
		if _, ok := before[key]; !ok {
			failures++
			if failures <= 10 {
				t.Errorf("%s only exists once hidden adjacency exists: %s", key, after[key])
			}
		}
	}
	if failures > 0 {
		t.Fatalf("%d of %d bounded outcomes depend on hidden adjacency", failures, len(before))
	}
}

// requireBoundedScans asserts every streamed chunk stayed within the chunk
// cap and examined no more entries than it asked for.
func (w *oracleWorld) requireBoundedScans() {
	w.t.Helper()
	w.base.mu.Lock()
	defer w.base.mu.Unlock()
	if len(w.base.requests) == 0 {
		w.t.Fatal("no bounded scans were recorded")
	}
	for index, request := range w.base.requests {
		if request.Fanout == 0 || request.Fanout > 1024 ||
			request.MaxNodes != request.Fanout+1 || request.Depth != 1 ||
			len(request.NodeIDs) != 1 || len(request.EdgeTypes) != 0 {
			w.t.Fatalf("scan %d = %#v, want one seed in a chunk of at most 1024",
				index, request)
		}
		if w.base.scanned[index] > request.Fanout {
			w.t.Fatalf("scan %d examined %d adjacency entries, above its chunk %d",
				index, w.base.scanned[index], request.Fanout)
		}
	}
}

// TestBoundedNeighborhoodOutcomeIgnoresHiddenAdjacency is the issue's
// repro as a differential test: every bounded read shape (fanout 1..N, each
// direction, with and without an edge-type filter, several max_nodes,
// depth one paged to completion, depth two and three, several seeds, and
// analytics materialization) must give exactly the same outcome whether or
// not a label holder has added hidden Connect edges and hidden interactions
// around the visible nodes.
func TestBoundedNeighborhoodOutcomeIgnoresHiddenAdjacency(t *testing.T) {
	withPolicyStores(t, func(t *testing.T, store authorized.PolicyStore) {
		w := newOracleWorld(t, store, 64)
		const maxFanout = 8
		before := w.sweep(w.reader(), maxFanout)
		for key, outcome := range before {
			// Analytics fails closed on its visible bounds by design.
			if !strings.HasPrefix(key, "analytics/") && strings.HasPrefix(outcome, "error") {
				t.Fatalf("visible-only read %s failed: %s", key, outcome)
			}
		}
		// The issue's repro: hidden Connect edges on the public node, in
		// every ID gap and both directions, and on a depth-two neighbor.
		for _, hidden := range []struct {
			id, visible string
			outgoing    bool
		}{
			{"e-a", "P", true}, {"e-c1", "P", true}, {"e-c2", "P", false},
			{"e-c3", "P", true}, {"e-e", "P", false}, {"e-g", "P", true},
			{"e-i", "V1", true}, {"e-k", "P", false}, {"e-z", "P", true},
			{"e-h0", "V1", false}, {"e-m", "W", true},
		} {
			w.hide(hidden.id, hidden.visible, hidden.outgoing)
		}
		// Hidden interactions touching the public node and a secret one.
		w.record("oracle-one", "P", "S001")
		w.record("oracle-two", "P", "V1", "S002")
		w.base.reset()
		after := w.sweep(w.reader(), maxFanout)
		requireSameOutcomes(t, before, after)
		w.requireBoundedScans()
	})
}

// TestBoundedNeighborhoodHiddenAdjacencyOnlyFailsAtTheUniformLimit pins the
// one remaining dependence on hidden adjacency and that it is uniform: a
// label holder can add Connect edges up to the server's node adjacency cap without
// changing any reader's outcome, interactions never count toward it, and
// once a node's raw adjacency exceeds it every read that expands the node
// fails with the same error for every reader, the label holder included,
// whatever their fanout, cursor or MaxScannedEdges.
func TestBoundedNeighborhoodHiddenAdjacencyOnlyFailsAtTheUniformLimit(t *testing.T) {
	const limit = 12
	w := newOracleWorld(t, authorized.NewMemoryPolicyStore(), limit)
	const maxFanout = 4
	before := w.sweep(w.reader(), maxFanout)
	// P has 4 visible edges and 2 intrinsic ones in "both"; fill its raw
	// adjacency to exactly the limit.
	p := w.docs["P"]
	rawBoth := func() uint64 {
		raw, err := w.f.base.BoundedNeighborhood(context.Background(),
			explorer.BoundedNeighborhoodRequest{
				NodeIDs: []shoal.ID{p}, Depth: 1, Fanout: 1, MaxNodes: 2,
			})
		if err != nil || !raw.NodeAdjacencyKnown {
			t.Fatalf("raw adjacency probe: %v", err)
		}
		return raw.NodeAdjacency
	}
	for index := 0; rawBoth() < limit; index++ {
		w.hide(fmt.Sprintf("e-fill-%02d", index), "P", index%2 == 0)
	}
	// Interactions are not scanned, so they cannot push P over the limit.
	for index := 0; index < limit; index++ {
		w.record(fmt.Sprintf("oracle-fill-%02d", index), "P", "S001")
	}
	if raw := rawBoth(); raw != limit {
		t.Fatalf("raw adjacency = %d after interactions, want %d", raw, limit)
	}
	w.base.reset()
	requireSameOutcomes(t, before, w.sweep(w.reader(), maxFanout))
	w.requireBoundedScans()

	// One more hidden edge crosses the limit: now every read that expands P
	// fails, with one error, for the reader and the label holder alike.
	w.hide("e-over", "P", true)
	var failure string
	for name, ctx := range map[string]context.Context{
		"reader": w.reader(), "label holder": w.owner(),
	} {
		for fanout := uint32(1); fanout <= 16; fanout++ {
			for _, request := range []explorer.BoundedNeighborhoodRequest{
				{NodeIDs: []shoal.ID{p}, Depth: 1},
				{NodeIDs: []shoal.ID{p}, Depth: 1, AfterEdgeID: "e-j"},
				{NodeIDs: []shoal.ID{p}, Depth: 1, MaxScannedEdges: 1 << 20},
				{NodeIDs: []shoal.ID{p}, Depth: 1, EdgeTypes: []string{oracleEdgeType}},
				{NodeIDs: []shoal.ID{p}, Depth: 2},
				{NodeIDs: []shoal.ID{w.docs["V3"]}, Depth: 2},
			} {
				request.Fanout, request.MaxNodes = fanout, 64
				_, err := w.client.BoundedNeighborhood(ctx, request)
				outcome := boundedOutcome(explorer.BoundedNeighborhood{}, err)
				if failure == "" {
					failure = outcome
				}
				if !shoal.IsErrorCode(err, shoal.ErrorUnavailable) || outcome != failure {
					t.Fatalf("%s %#v = %s, want the uniform %s", name, request, outcome, failure)
				}
			}
		}
	}
	// A visible node that does not expand P is unaffected.
	if _, err := w.client.BoundedNeighborhood(w.reader(),
		explorer.BoundedNeighborhoodRequest{
			NodeIDs: []shoal.ID{w.docs["W"]}, Depth: 1, Fanout: 1, MaxNodes: 64,
		}); err != nil {
		t.Fatalf("unrelated node failed: %v", err)
	}
}
