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

// Streaming edge cases of the authorized bounded read (#698): a write that
// lands while a node's adjacency is being streamed, a provenance seed whose
// visibility rests on edges past its first hidden ones, and the per-direction
// cap counting incoming adjacency.

import (
	"fmt"
	"sort"
	"strings"
	"testing"

	"github.com/phrocker/shoal-oss/pkg/explorer"
	"github.com/phrocker/shoal-oss/pkg/explorer/authorized"
	"github.com/phrocker/shoal-oss/pkg/graph"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

// TestBoundedNeighborhoodWriteDuringStreamIsConflict pins that a hidden
// write landing between two streamed chunks is reported as the corpus
// changing, the same Conflict whether it touches the streamed node itself
// (its reported adjacency changes) or another node (the snapshot changes),
// and never as an internal error that would single out the streamed node.
func TestBoundedNeighborhoodWriteDuringStreamIsConflict(t *testing.T) {
	for _, target := range []string{"W", "P"} {
		t.Run("hidden write on "+target, func(t *testing.T) {
			w := newOracleWorld(t, authorized.NewMemoryPolicyStore(), 0)
			for index := 0; index < 80; index++ {
				w.connect(fmt.Sprintf("race-%03d", index), "W", "V1")
			}
			wrote := false
			w.base.mu.Lock()
			w.base.beforeScan = func(recorded int) {
				if recorded == 1 && !wrote {
					wrote = true
					w.hide("race-hidden", target, true)
				}
			}
			w.base.mu.Unlock()
			_, err := w.client.BoundedNeighborhood(w.reader(),
				explorer.BoundedNeighborhoodRequest{
					NodeIDs: []shoal.ID{w.docs["W"]}, Depth: 1, Fanout: 1,
					MaxNodes: 64, EdgeTypes: []string{"no-such-type"},
				})
			if !wrote {
				t.Fatal("the stream finished in one chunk; the write never landed")
			}
			if !shoal.IsErrorCode(err, shoal.ErrorConflict) ||
				!strings.Contains(err.Error(), "corpus changed while scanning bounded graph") {
				t.Fatalf("write during stream = %v, want the corpus-changed conflict", err)
			}
		})
	}
}

// TestBoundedNeighborhoodCapCountsIncomingAdjacency pins that the cap
// applies to the requested direction's total: hidden adjacency that is
// incoming only puts a node over the cap for "both" and "incoming" reads
// while its outgoing reads still succeed.
func TestBoundedNeighborhoodCapCountsIncomingAdjacency(t *testing.T) {
	const limit = 4
	w := newOracleWorld(t, authorized.NewMemoryPolicyStore(), limit)
	for index := 0; index < limit; index++ {
		w.hide(fmt.Sprintf("e-in-%02d", index), "V2", false)
	}
	for _, direction := range []explorer.GraphDirection{
		explorer.GraphDirectionBoth, explorer.GraphDirectionIncoming,
		explorer.GraphDirectionOutgoing,
	} {
		_, err := w.client.BoundedNeighborhood(w.reader(),
			explorer.BoundedNeighborhoodRequest{
				NodeIDs: []shoal.ID{w.docs["V2"]}, Depth: 1, Fanout: 1,
				MaxNodes: 64, Direction: direction,
			})
		if direction == explorer.GraphDirectionOutgoing {
			if err != nil {
				t.Fatalf("outgoing read under the cap = %v", err)
			}
			continue
		}
		if !shoal.IsErrorCode(err, shoal.ErrorUnavailable) {
			t.Fatalf("%s read over the cap = %v, want the uniform failure", direction, err)
		}
	}
}

// TestBoundedNeighborhoodUnexpandedProvenanceSeedStreamsPastHiddenEdges
// pins how a seed MaxNodes stopped expansion before reaching is still put in
// the result: a provenance seed is visible only through a visible produced
// edge, so its lookup streams past the hidden produced edges in front of
// that edge rather than reading one chunk and calling it invisible.
func TestBoundedNeighborhoodUnexpandedProvenanceSeedStreamsPastHiddenEdges(t *testing.T) {
	f := newFixture(t)
	admin := f.admin(t)
	source, err := f.clientA.Ingest(admin, explorer.Source{
		URI: "file:///stream-producer-source.txt", MediaType: explorer.MediaTypeText,
		Content: "visible streamed producer source",
	})
	if err != nil {
		t.Fatal(err)
	}
	target, err := f.clientA.Ingest(admin, explorer.Source{
		URI: "file:///stream-producer-target-0.txt", MediaType: explorer.MediaTypeText,
		Content: "visible streamed producer target",
	})
	if err != nil {
		t.Fatal(err)
	}
	cells := []explorer.LatentLinkCell{
		authorizedLatentCell(source.Document.ID, target.Document.ID, 1),
	}
	bobSource, err := f.clientB.Ingest(admin, explorer.Source{
		URI: "file:///stream-producer-bob-source.txt", MediaType: explorer.MediaTypeText,
		Content: "hidden streamed producer source",
	})
	if err != nil {
		t.Fatal(err)
	}
	bobTarget, err := f.clientB.Ingest(admin, explorer.Source{
		URI: "file:///stream-producer-bob-target.txt", MediaType: explorer.MediaTypeText,
		Content: strings.Repeat("hidden streamed producer target\n\n", 300),
	})
	if err != nil {
		t.Fatal(err)
	}
	view, err := f.base.Document(admin, bobTarget.Document.ID, bobTarget.Revision.ID)
	if err != nil {
		t.Fatal(err)
	}
	bobTargets := []shoal.ID{bobTarget.Document.ID, view.Root.Section.ID}
	for _, span := range view.Root.Spans {
		bobTargets = append(bobTargets, span.ID)
	}
	for _, id := range bobTargets {
		cells = append(cells, authorizedLatentCell(bobSource.Document.ID, id, int64(len(cells)+1)))
	}
	if err := f.base.PutLatentLinkCells(admin, cells); err != nil {
		t.Fatal(err)
	}

	alice := f.alice(t)
	sourceGraph, err := f.clientA.BoundedNeighborhood(alice, explorer.BoundedNeighborhoodRequest{
		NodeIDs: []shoal.ID{source.Document.ID}, Depth: 1, Fanout: 8, MaxNodes: 8,
		EdgeTypes: []string{authorizedLatentEdgeType(t)},
		Direction: explorer.GraphDirectionOutgoing,
	})
	if err != nil || len(sourceGraph.Neighborhood.Assertions) != 1 {
		t.Fatalf("alice source graph = %+v, %v", sourceGraph.Neighborhood, err)
	}
	provenance, err := f.clientA.BoundedNeighborhood(alice, explorer.BoundedNeighborhoodRequest{
		NodeIDs: []shoal.ID{sourceGraph.Neighborhood.Assertions[0].ID()}, Depth: 1,
		Fanout: 8, MaxNodes: 8, EdgeTypes: []string{graph.EdgeTypeProduced},
		Direction: explorer.GraphDirectionIncoming,
	})
	if err != nil || len(provenance.Neighborhood.Edges) != 1 {
		t.Fatalf("alice assertion provenance = %+v, %v", provenance.Neighborhood, err)
	}
	aliceEdge := provenance.Neighborhood.Edges[0]
	producer := aliceEdge.From
	// Precondition: more hidden produced edges sort before alice's than the
	// first chunk of the lookup holds.
	everything, err := f.base.BoundedNeighborhood(admin, explorer.BoundedNeighborhoodRequest{
		NodeIDs: []shoal.ID{producer}, Depth: 1, Fanout: 4096, MaxNodes: 4097,
	})
	if err != nil {
		t.Fatal(err)
	}
	ids := make([]shoal.ID, 0, len(everything.Neighborhood.Edges))
	for _, edge := range everything.Neighborhood.Edges {
		ids = append(ids, edge.ID)
	}
	sort.Slice(ids, func(i, j int) bool { return shoal.CompareID(ids[i], ids[j]) < 0 })
	before := sort.Search(len(ids), func(i int) bool {
		return shoal.CompareID(ids[i], aliceEdge.ID) >= 0
	})
	if before <= 70 {
		t.Fatalf("fixture precondition: %d of %d produced edges (%d targets) precede alice's, want more than 70", before, len(ids), len(bobTargets))
	}

	result, err := f.clientA.BoundedNeighborhood(alice, explorer.BoundedNeighborhoodRequest{
		NodeIDs: []shoal.ID{source.Document.ID, producer}, Depth: 1,
		Fanout: 8, MaxNodes: 2,
	})
	if err != nil {
		t.Fatalf("seeds stopped by MaxNodes = %v", err)
	}
	if !result.Truncated || !hasNode(result.Neighborhood, producer) ||
		!hasNode(result.Neighborhood, source.Document.ID) {
		t.Fatalf("unexpanded provenance seed missing = %+v", result)
	}
}
