// Licensed to the Apache Software Foundation (ASF) under one
// or more contributor license agreements. See the NOTICE file
// distributed with this work for additional information
// regarding copyright ownership. The ASF licenses this file
// to you under the Apache License, Version 2.0 (the
// "License"); you may not use this file except in compliance
// with the License. You may obtain a copy of the License at
//
//     https://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing,
// software distributed under the License is distributed on an
// "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY
// KIND, either express or implied. See the License for the
// specific language governing permissions and limitations
// under the License.

package explorer

import (
	"context"
	"math"
	"testing"
	"time"

	"github.com/phrocker/shoal-oss/pkg/graph"
	"github.com/phrocker/shoal-oss/pkg/interaction"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

func TestSnapshotStableAcrossRestart(t *testing.T) {
	dataDir := t.TempDir()
	corpus, err := Open(dataDir)
	if err != nil {
		t.Fatal(err)
	}

	before, err := corpus.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if err := corpus.Close(); err != nil {
		t.Fatal(err)
	}
	corpus, err = Open(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	defer corpus.Close()
	after, err := corpus.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if before != after {
		t.Fatalf("snapshot changed across restart: before=%+v after=%+v", before, after)
	}
}

func TestHistoricalSnapshotValidationSurvivesRestart(t *testing.T) {
	ctx := context.Background()
	dataDir := t.TempDir()
	corpus, err := Open(dataDir)
	if err != nil {
		t.Fatal(err)
	}

	firstReceipt, err := corpus.Ingest(ctx, Source{
		URI: "file:///snapshot-one.txt", MediaType: MediaTypeText,
		Content: "snapshot one",
	})
	if err != nil {
		t.Fatal(err)
	}
	firstView, err := corpus.Document(
		ctx, firstReceipt.Document.ID, firstReceipt.Revision.ID)
	if err != nil {
		t.Fatal(err)
	}
	first, err := corpus.Snapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	secondReceipt, err := corpus.Ingest(ctx, Source{
		URI: "file:///snapshot-two.txt", MediaType: MediaTypeText,
		Content: "snapshot two",
	})
	if err != nil {
		t.Fatal(err)
	}
	secondView, err := corpus.Document(
		ctx, secondReceipt.Document.ID, secondReceipt.Revision.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := corpus.Snapshot(ctx); err != nil {
		t.Fatal(err)
	}
	if err := corpus.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if err := reopened.ValidateSnapshot(
		ctx, shoal.ID(first.ID), first.AsOf,
		[]shoal.ID{firstView.Root.Spans[0].ID},
	); err != nil {
		t.Fatalf("historical snapshot was not durable: %v", err)
	}
	if err := reopened.ValidateSnapshot(
		ctx, "forged-snapshot", first.AsOf, nil,
	); !shoal.IsErrorCode(err, shoal.ErrorConflict) {
		t.Fatalf("forged snapshot validation error = %v", err)
	}
	if err := reopened.ValidateSnapshot(
		ctx, shoal.ID(first.ID), first.AsOf,
		[]shoal.ID{secondView.Root.Spans[0].ID},
	); !shoal.IsErrorCode(err, shoal.ErrorConflict) {
		t.Fatalf("post-snapshot source validation error = %v", err)
	}
}

func TestHistoricalSnapshotRejectsReusedIDsWithChangedSourceState(t *testing.T) {
	ctx := context.Background()
	dataDir := t.TempDir()
	corpus, err := Open(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	source := Source{
		URI: "file:///snapshot-reclassified.txt", MediaType: MediaTypeText,
		Content: "stable content",
		Metadata: shoal.Metadata{
			interaction.PropertyVisibility: "restricted",
		},
	}
	firstReceipt, err := corpus.Ingest(ctx, source)
	if err != nil {
		t.Fatal(err)
	}
	firstView, err := corpus.Document(
		ctx, firstReceipt.Document.ID, firstReceipt.Revision.ID)
	if err != nil {
		t.Fatal(err)
	}
	neighborhood, err := corpus.Neighborhood(
		ctx, NeighborhoodRequest{
			NodeIDs: []shoal.ID{firstReceipt.Document.ID}, Depth: 2,
		})
	if err != nil {
		t.Fatal(err)
	}
	if len(neighborhood.Edges) == 0 {
		t.Fatal("fixture produced no source edge")
	}
	snapshot, err := corpus.Snapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}

	source.Metadata[interaction.PropertyVisibility] = "public"
	secondReceipt, err := corpus.Ingest(ctx, source)
	if err != nil {
		t.Fatal(err)
	}
	secondView, err := corpus.Document(
		ctx, secondReceipt.Document.ID, secondReceipt.Revision.ID)
	if err != nil {
		t.Fatal(err)
	}
	firstSpan := firstView.Root.Spans[0].ID
	if secondView.Root.Spans[0].ID != firstSpan {
		t.Fatal("fixture did not preserve the source span ID")
	}
	if err := corpus.ValidateSnapshot(
		ctx, shoal.ID(snapshot.ID), snapshot.AsOf, []shoal.ID{firstSpan},
	); !shoal.IsErrorCode(err, shoal.ErrorConflict) {
		t.Fatalf("changed source state validation error = %v", err)
	}
	if err := corpus.ValidateEvidenceSnapshot(
		ctx, shoal.ID(snapshot.ID), snapshot.AsOf, nil,
		[]shoal.ID{neighborhood.Edges[0].ID}, nil,
	); err != nil {
		t.Fatalf("unchanged source edge validation error = %v", err)
	}
	corpus.mu.Lock()
	changedEdge := corpus.graphEdges[neighborhood.Edges[0].ID]
	changedEdge.Properties = shoal.Metadata{"changed": "true"}
	corpus.graphEdges[changedEdge.ID] = changedEdge
	corpus.mu.Unlock()
	if err := corpus.ValidateEvidenceSnapshot(
		ctx, shoal.ID(snapshot.ID), snapshot.AsOf, nil,
		[]shoal.ID{neighborhood.Edges[0].ID}, nil,
	); !shoal.IsErrorCode(err, shoal.ErrorConflict) {
		t.Fatalf("changed source edge validation error = %v", err)
	}
	if err := corpus.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if err := reopened.ValidateSnapshot(
		ctx, shoal.ID(snapshot.ID), snapshot.AsOf, []shoal.ID{firstSpan},
	); !shoal.IsErrorCode(err, shoal.ErrorConflict) {
		t.Fatalf("reopened changed source state validation error = %v", err)
	}
}

func TestSnapshotHashIncludesCollectionBoundaries(t *testing.T) {
	anchor := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	first := &Explorer{
		graphNodes: map[shoal.ID]graph.Node{
			"node": {ID: "node", Labels: []string{"a"}, Properties: shoal.Metadata{"b": "c"}},
		},
		graphEdges: make(map[shoal.ID]graph.Edge), snapshotAnchor: anchor,
	}
	first.refreshSnapshotLocked()
	second := &Explorer{
		graphNodes: map[shoal.ID]graph.Node{
			"node": {ID: "node", Labels: []string{"a", "b", "c"}},
		},
		graphEdges: make(map[shoal.ID]graph.Edge), snapshotAnchor: anchor,
	}
	second.refreshSnapshotLocked()
	if first.snapshot.ID == second.snapshot.ID {
		t.Fatal("distinct graph collections produced the same snapshot")
	}
}

func TestGraphIndexIsLazyForIngestOnlyClients(t *testing.T) {
	corpus, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer corpus.Close()
	for _, content := range []string{"first", "second", "third"} {
		if _, err := corpus.Ingest(context.Background(), Source{
			URI:       "file:///" + content + ".txt",
			MediaType: MediaTypeText, Content: content,
		}); err != nil {
			t.Fatal(err)
		}
		if corpus.graphInitialized {
			t.Fatal("ingest eagerly initialized the optional graph index")
		}
	}
	if _, err := corpus.Snapshot(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !corpus.graphInitialized {
		t.Fatal("snapshot did not initialize the graph index")
	}
}

func TestBoundedNeighborhoodCountsSelfEdgeOnce(t *testing.T) {
	corpus, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer corpus.Close()
	ctx := context.Background()
	first, err := corpus.Ingest(ctx, Source{
		URI: "file:///first.txt", MediaType: MediaTypeText, Content: "first",
	})
	if err != nil {
		t.Fatal(err)
	}
	second, err := corpus.Ingest(ctx, Source{
		URI: "file:///second.txt", MediaType: MediaTypeText, Content: "second",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := corpus.Connect(ctx, graph.Edge{
		ID: "a-self", From: first.Document.ID, To: first.Document.ID,
		Type: "self", Weight: 1,
	}); err != nil {
		t.Fatal(err)
	}
	if err := corpus.Connect(ctx, graph.Edge{
		ID: "b-neighbor", From: first.Document.ID, To: second.Document.ID,
		Type: "neighbor", Weight: 1,
	}); err != nil {
		t.Fatal(err)
	}
	result, err := corpus.BoundedNeighborhood(ctx, BoundedNeighborhoodRequest{
		NodeIDs: []shoal.ID{first.Document.ID}, Depth: 1, Fanout: 2,
		MaxNodes: 2, Direction: GraphDirectionOutgoing,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Neighborhood.Edges) != 2 {
		t.Fatalf("edges = %+v", result.Neighborhood.Edges)
	}
	if _, err := corpus.BoundedNeighborhood(ctx, BoundedNeighborhoodRequest{
		NodeIDs: []shoal.ID{first.Document.ID}, Depth: 1, Fanout: 1,
		MaxNodes: math.MaxUint32,
	}); err != nil {
		t.Fatalf("large max_nodes should not drive allocation: %v", err)
	}
}

func TestSelfEdgeDoesNotCreateFalseContinuation(t *testing.T) {
	edge := graph.Edge{ID: "self", From: "node", To: "node", Type: "self", Weight: 1}
	corpus := &Explorer{
		graphInitialized: true,
		graphNodes: map[shoal.ID]graph.Node{
			"node": {ID: "node"},
		},
		graphEdges: map[shoal.ID]graph.Edge{"self": edge},
		outgoing:   map[shoal.ID][]shoal.ID{"node": {"self"}},
		incoming:   map[shoal.ID][]shoal.ID{"node": {"self"}},
	}
	result, err := corpus.BoundedNeighborhood(
		context.Background(),
		BoundedNeighborhoodRequest{
			NodeIDs: []shoal.ID{"node"}, Depth: 1, Fanout: 1, MaxNodes: 1,
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if result.Truncated || result.NextAfterEdgeID != "" {
		t.Fatalf("self edge produced continuation: %+v", result)
	}
}

// TestBoundedNeighborhoodExcludesInteractionAdjacencyBeforeFanout pins that
// a source node's bounded scan never examines its interaction adjacency
// (#698): the interaction edge neither takes the fanout slot nor counts as
// scanned, so the visible edge comes back on the first page with an exact
// count, as it would if the interaction did not exist. An explicit
// interaction seed still serves its own edges, and a source node expanded
// from it still reaches back to it.
func TestBoundedNeighborhoodExcludesInteractionAdjacencyBeforeFanout(t *testing.T) {
	corpus := &Explorer{
		graphInitialized: true,
		graphNodes: map[shoal.ID]graph.Node{
			"source":  {ID: "source"},
			"hidden":  {ID: "hidden", Kind: "interaction.session"},
			"visible": {ID: "visible"},
		},
		graphEdges: map[shoal.ID]graph.Edge{
			"a-hidden": {
				ID: "a-hidden", From: "hidden", To: "source",
				Type: "interaction.retrieved", Weight: 1,
			},
			"b-visible": {
				ID: "b-visible", From: "source", To: "visible",
				Type: "related", Weight: 1,
			},
		},
		outgoing: map[shoal.ID][]shoal.ID{
			"source": {"b-visible"},
		},
		incoming: map[shoal.ID][]shoal.ID{
			"visible": {"b-visible"},
		},
		interactionOutgoing: map[shoal.ID][]shoal.ID{
			"hidden": {"a-hidden"},
		},
		interactionIncoming: map[shoal.ID][]shoal.ID{
			"source": {"a-hidden"},
		},
	}
	first, err := corpus.BoundedNeighborhood(
		context.Background(),
		BoundedNeighborhoodRequest{
			NodeIDs: []shoal.ID{"source"}, Depth: 1,
			Fanout: 1, MaxNodes: 2, Direction: GraphDirectionBoth,
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if first.ScannedEdges != 1 || !first.ScannedEdgesKnown ||
		len(first.Neighborhood.Edges) != 1 ||
		first.Neighborhood.Edges[0].ID != "b-visible" ||
		first.Truncated || first.Continuation || first.NextAfterEdgeID != "" {
		t.Fatalf("interaction adjacency was scanned = %#v", first)
	}
	// The reported node adjacency counts what such a scan examines: source
	// adjacency in each requested direction, and an interaction seed's own.
	for _, probe := range []struct {
		node      shoal.ID
		direction GraphDirection
		want      uint64
	}{
		{"source", GraphDirectionBoth, 1},
		{"source", GraphDirectionIncoming, 0},
		{"visible", GraphDirectionBoth, 1},
		{"visible", GraphDirectionOutgoing, 0},
		{"hidden", GraphDirectionBoth, 1},
		{"hidden", GraphDirectionOutgoing, 1},
		{"hidden", GraphDirectionIncoming, 0},
	} {
		result, err := corpus.BoundedNeighborhood(context.Background(),
			BoundedNeighborhoodRequest{
				NodeIDs: []shoal.ID{probe.node}, Depth: 1, Fanout: 1,
				MaxNodes: 2, Direction: probe.direction, AfterEdgeID: "z",
			})
		if err != nil {
			t.Fatal(err)
		}
		if !result.NodeAdjacencyKnown || result.NodeAdjacency != probe.want {
			t.Fatalf("%s %s node adjacency = %d (known %t), want %d",
				probe.node, probe.direction, result.NodeAdjacency,
				result.NodeAdjacencyKnown, probe.want)
		}
	}
	seeded, err := corpus.BoundedNeighborhood(
		context.Background(),
		BoundedNeighborhoodRequest{
			NodeIDs: []shoal.ID{"hidden"}, Depth: 2,
			Fanout: 4, MaxNodes: 4, Direction: GraphDirectionBoth,
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(seeded.Neighborhood.Edges) != 2 || seeded.ScannedEdges != 3 ||
		seeded.Truncated {
		t.Fatalf("explicit interaction seed = %#v", seeded)
	}
	both, err := corpus.BoundedNeighborhood(
		context.Background(),
		BoundedNeighborhoodRequest{
			NodeIDs: []shoal.ID{"source", "hidden"}, Depth: 1,
			Fanout: 4, MaxNodes: 4, Direction: GraphDirectionIncoming,
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	// source's incoming scan reaches the explicit interaction seed's edge;
	// hidden has no incoming edges.
	if len(both.Neighborhood.Edges) != 1 ||
		both.Neighborhood.Edges[0].ID != "a-hidden" || both.ScannedEdges != 1 {
		t.Fatalf("source expanded beside an interaction seed = %#v", both)
	}
}

func TestConnectClonesRetainedEdgeProperties(t *testing.T) {
	corpus, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer corpus.Close()
	ctx := context.Background()
	first, err := corpus.Ingest(ctx, Source{
		URI: "file:///first.txt", MediaType: MediaTypeText, Content: "first",
	})
	if err != nil {
		t.Fatal(err)
	}
	properties := shoal.Metadata{"state": "persisted"}
	if err := corpus.Connect(ctx, graph.Edge{
		ID: "self", From: first.Document.ID, To: first.Document.ID,
		Type: "self", Weight: 1, Properties: properties,
	}); err != nil {
		t.Fatal(err)
	}
	properties["state"] = "mutated"
	if _, err := corpus.Ingest(ctx, Source{
		URI: "file:///second.txt", MediaType: MediaTypeText, Content: "second",
	}); err != nil {
		t.Fatal(err)
	}
	result, err := corpus.BoundedNeighborhood(ctx, BoundedNeighborhoodRequest{
		NodeIDs: []shoal.ID{first.Document.ID}, Depth: 1, Fanout: 10, MaxNodes: 10,
	})
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, edge := range result.Neighborhood.Edges {
		if edge.ID == "self" && edge.Properties["state"] != "persisted" {
			t.Fatalf("retained edge properties = %+v", edge.Properties)
		}
		if edge.ID == "self" {
			found = true
		}
	}
	if !found {
		t.Fatal("retained edge not found")
	}
}
