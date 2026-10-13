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

// A reference model of the bounded read semantics over the visible graph
// alone (#698). The before/after differential cannot catch a mutation that
// changes both runs alike, so the bounded results are also compared against
// pages and traversals computed here from the reader's own unbounded
// Neighborhood, an independent read path.

import (
	"context"
	"fmt"
	"sort"
	"testing"

	"github.com/phrocker/shoal-oss/pkg/explorer"
	"github.com/phrocker/shoal-oss/pkg/explorer/authorized"
	"github.com/phrocker/shoal-oss/pkg/graph"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

// visibleReference holds the reader's visible one-hop adjacency per node.
type visibleReference struct {
	t        testing.TB
	ctx      context.Context
	client   *authorized.Client
	incident map[shoal.ID][]graph.Edge
}

func newVisibleReference(
	t testing.TB, ctx context.Context, client *authorized.Client,
) *visibleReference {
	return &visibleReference{
		t: t, ctx: ctx, client: client, incident: map[shoal.ID][]graph.Edge{},
	}
}

func (r *visibleReference) adjacency(
	nodeID shoal.ID, direction explorer.GraphDirection, types []string,
) []graph.Edge {
	r.t.Helper()
	incident, ok := r.incident[nodeID]
	if !ok {
		around, err := r.client.Neighborhood(r.ctx, explorer.NeighborhoodRequest{
			NodeIDs: []shoal.ID{nodeID}, Depth: 1,
		})
		if err != nil {
			r.t.Fatalf("reference neighborhood of %s: %v", nodeID, err)
		}
		for _, edge := range around.Edges {
			if edge.From == nodeID || edge.To == nodeID {
				incident = append(incident, edge)
			}
		}
		sort.Slice(incident, func(i, j int) bool {
			return shoal.CompareID(incident[i].ID, incident[j].ID) < 0
		})
		r.incident[nodeID] = incident
	}
	allowed := map[string]bool{}
	for _, edgeType := range types {
		allowed[edgeType] = true
	}
	var result []graph.Edge
	for _, edge := range incident {
		if len(allowed) > 0 && !allowed[edge.Type] {
			continue
		}
		switch direction {
		case explorer.GraphDirectionOutgoing:
			if edge.From != nodeID {
				continue
			}
		case explorer.GraphDirectionIncoming:
			if edge.To != nodeID {
				continue
			}
		}
		result = append(result, edge)
	}
	return result
}

type referenceOutcome struct {
	nodes        []shoal.ID
	edges        []shoal.ID
	truncated    bool
	next         shoal.ID
	continuation bool
}

func (o referenceOutcome) String() string {
	return fmt.Sprintf("nodes=%v edges=%v truncated=%t next=%q continuation=%t",
		o.nodes, o.edges, o.truncated, o.next, o.continuation)
}

func sortedIDs(set map[shoal.ID]struct{}) []shoal.ID {
	ids := make([]shoal.ID, 0, len(set))
	for id := range set {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return shoal.CompareID(ids[i], ids[j]) < 0 })
	return ids
}

// page is one cursor page of a single seed at depth one: the first visible
// edges after the cursor that fit Fanout and MaxNodes.
func (r *visibleReference) page(
	seed, after shoal.ID, fanout, maxNodes uint32,
	direction explorer.GraphDirection, types []string,
) referenceOutcome {
	nodes := map[shoal.ID]struct{}{seed: {}}
	var edges []shoal.ID
	out := referenceOutcome{}
	for _, edge := range r.adjacency(seed, direction, types) {
		if after != "" && shoal.CompareID(edge.ID, after) <= 0 {
			continue
		}
		adds := 0
		if _, ok := nodes[edge.From]; !ok {
			adds++
		}
		if _, ok := nodes[edge.To]; !ok && edge.To != edge.From {
			adds++
		}
		if uint32(len(nodes)+adds) > maxNodes {
			out.truncated = true
			out.continuation = len(edges) > 0
			break
		}
		if uint32(len(edges)) == fanout {
			out.truncated, out.continuation = true, true
			break
		}
		edges = append(edges, edge.ID)
		nodes[edge.From], nodes[edge.To] = struct{}{}, struct{}{}
		out.next = edge.ID
	}
	if len(edges) == 0 {
		out.continuation = false
	}
	if !out.continuation {
		out.next = ""
	}
	out.nodes, out.edges = sortedIDs(nodes), edges
	if out.edges == nil {
		out.edges = []shoal.ID{}
	}
	return out
}

// traverse is a breadth-first bounded expansion: each expanded node takes
// its first Fanout visible edges, and MaxNodes stops expansion.
func (r *visibleReference) traverse(
	seeds []shoal.ID, depth, fanout, maxNodes uint32,
	direction explorer.GraphDirection, types []string,
) referenceOutcome {
	seen := map[shoal.ID]struct{}{}
	for _, seed := range seeds {
		seen[seed] = struct{}{}
	}
	edges := map[shoal.ID]struct{}{}
	frontier := append([]shoal.ID(nil), seeds...)
	out := referenceOutcome{}
	stop := false
	for level := uint32(0); level < depth && len(frontier) > 0 && !stop; level++ {
		var next []shoal.ID
		for _, seed := range frontier {
			adjacency := r.adjacency(seed, direction, types)
			if uint32(len(adjacency)) > fanout {
				adjacency = adjacency[:fanout]
				out.truncated = true
			}
			for _, edge := range adjacency {
				other := edge.To
				if other == seed {
					other = edge.From
				}
				if _, ok := seen[other]; !ok {
					if uint32(len(seen)) >= maxNodes {
						out.truncated, stop = true, true
						break
					}
					seen[other] = struct{}{}
					next = append(next, other)
				}
				edges[edge.ID] = struct{}{}
			}
			if stop {
				break
			}
		}
		frontier = next
	}
	out.nodes, out.edges = sortedIDs(seen), sortedIDs(edges)
	return out
}

func boundedReferenceOutcome(result explorer.BoundedNeighborhood) referenceOutcome {
	out := referenceOutcome{
		nodes: []shoal.ID{}, edges: []shoal.ID{},
		truncated: result.Truncated, next: result.NextAfterEdgeID,
		continuation: result.Continuation,
	}
	for _, node := range result.Neighborhood.Nodes {
		out.nodes = append(out.nodes, node.ID)
	}
	for _, edge := range result.Neighborhood.Edges {
		out.edges = append(out.edges, edge.ID)
	}
	return out
}

func requireReference(
	t testing.TB, key string, result explorer.BoundedNeighborhood, err error,
	want referenceOutcome,
) {
	t.Helper()
	if err != nil {
		t.Fatalf("%s: %v, want %s", key, err, want)
	}
	if got := boundedReferenceOutcome(result); got.String() != want.String() {
		t.Fatalf("%s differs from the visible-graph reference:\n got:  %s\n want: %s",
			key, got, want)
	}
	if uint32(len(result.Neighborhood.Nodes)) == 0 {
		t.Fatalf("%s returned no nodes", key)
	}
}

// TestBoundedNeighborhoodHubAboveOneChunk is the regression for a visible
// hub with more adjacency than one streamed chunk (1030 visible edges):
// depth-one paging to completion and depth-two traversal work and equal the
// visible-graph reference, and a fanout-one page costs about its fanout.
func TestBoundedNeighborhoodHubAboveOneChunk(t *testing.T) {
	w := newOracleWorld(t, authorized.NewMemoryPolicyStore(), 0)
	targets := []string{"V1", "V2", "V3"}
	for index := 0; index < 1030; index++ {
		w.connect(fmt.Sprintf("hub-%04d", index), "W", targets[index%len(targets)])
	}
	for index := 0; index < 40; index++ {
		w.hide(fmt.Sprintf("hub-%04d-hidden", index*25), "W", index%2 == 0)
	}
	hub := w.docs["W"]
	reader := w.reader()
	reference := newVisibleReference(t, reader, w.client)
	if got := len(reference.adjacency(hub, explorer.GraphDirectionBoth, nil)); got < 1030 {
		t.Fatalf("reference hub adjacency = %d", got)
	}
	for _, fanout := range []uint32{1, 50, 1100} {
		after := shoal.ID("")
		seen := 0
		for page := 0; ; page++ {
			w.base.reset()
			result, err := w.client.BoundedNeighborhood(reader,
				explorer.BoundedNeighborhoodRequest{
					NodeIDs: []shoal.ID{hub}, Depth: 1, Fanout: fanout,
					MaxNodes: 64, AfterEdgeID: after,
				})
			requireReference(t, fmt.Sprintf("hub page f%d/%d", fanout, page), result, err,
				reference.page(hub, after, fanout, 64, explorer.GraphDirectionBoth, nil))
			seen += len(result.Neighborhood.Edges)
			if fanout == 1 {
				scanned := uint32(0)
				for _, count := range w.base.scanned {
					scanned += count
				}
				// Two visible edges answer a fanout-one page, and the first
				// chunk's slack absorbs the sparse hidden edges around them:
				// one call of fanout + 1 + 64 entries.
				if len(w.base.requests) != 1 || scanned > 66 {
					t.Fatalf("fanout-one hub page took %d calls, %d entries",
						len(w.base.requests), scanned)
				}
			}
			w.requireBoundedScans()
			if !result.Continuation {
				break
			}
			if fanout == 1 && page >= 40 {
				break
			}
			after = result.NextAfterEdgeID
		}
		if fanout != 1 && seen != len(reference.adjacency(hub, explorer.GraphDirectionBoth, nil)) {
			t.Fatalf("fanout %d paged %d hub edges", fanout, seen)
		}
	}
	// An edge type nothing has streams the whole hub: growing chunks of a
	// non-power-of-two start must stay clamped to the chunk cap.
	w.base.reset()
	empty, err := w.client.BoundedNeighborhood(reader, explorer.BoundedNeighborhoodRequest{
		NodeIDs: []shoal.ID{hub}, Depth: 1, Fanout: 1, MaxNodes: 64,
		EdgeTypes: []string{"no-such-type"},
	})
	if err != nil || len(empty.Neighborhood.Edges) != 0 || empty.Truncated {
		t.Fatalf("hub stream with no matching edge = %#v, %v", empty, err)
	}
	if len(w.base.requests) < 5 {
		t.Fatalf("hub stream took %d chunks, want at least 5", len(w.base.requests))
	}
	w.requireBoundedScans()
	for _, request := range []explorer.BoundedNeighborhoodRequest{
		{NodeIDs: []shoal.ID{w.docs["P"]}, Depth: 2, Fanout: 1100, MaxNodes: 64},
		{NodeIDs: []shoal.ID{w.docs["V1"]}, Depth: 2, Fanout: 1100, MaxNodes: 64},
		{NodeIDs: []shoal.ID{hub}, Depth: 2, Fanout: 7, MaxNodes: 5},
		{NodeIDs: []shoal.ID{hub, w.docs["P"]}, Depth: 3, Fanout: 2000, MaxNodes: 250},
	} {
		result, err := w.client.BoundedNeighborhood(reader, request)
		requireReference(t, fmt.Sprintf("hub traversal %#v", request), result, err,
			reference.traverse(request.NodeIDs, request.Depth, request.Fanout,
				request.MaxNodes, explorer.GraphDirectionBoth, nil))
	}
}
