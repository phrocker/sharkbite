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

package explorer

import (
	"context"
	"testing"

	"github.com/phrocker/shoal-oss/pkg/graph"
	"github.com/phrocker/shoal-oss/pkg/ontology"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

func TestLatentLinkAssertionsReachBoundedGraphReadPath(t *testing.T) {
	ctx := context.Background()
	corpus, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer corpus.Close()
	source := ingestLatentReadPathDocument(t, corpus, "source")
	target := ingestLatentReadPathDocument(t, corpus, "target")
	cell := latentReadPathCell(source.Document.ID, target.Document.ID, 42, false)
	if err := corpus.PutLatentLinkCells(ctx, []LatentLinkCell{cell}); err != nil {
		t.Fatal(err)
	}

	got, err := corpus.BoundedNeighborhood(ctx, BoundedNeighborhoodRequest{
		NodeIDs: []shoal.ID{source.Document.ID}, Depth: 1, Fanout: 8, MaxNodes: 8,
		EdgeTypes: []string{latentReadPathEdgeType(t)},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Neighborhood.Edges) != 1 || len(got.Neighborhood.Assertions) != 1 {
		t.Fatalf("derived graph read = %+v, want one edge and one assertion", got)
	}
	assertion := got.Neighborhood.Assertions[0]
	if assertion.Origin() != ontology.AssertionDerived {
		t.Fatalf("assertion origin = %q, want derived", assertion.Origin())
	}
	derivation, ok := assertion.Evidence()[0].Derivation()
	if !ok {
		t.Fatal("derived assertion evidence lost derivation")
	}
	if derivation.SourceEndpoint() != source.Document.ID ||
		derivation.TargetEndpoint() != target.Document.ID ||
		derivation.IteratorName() != LatentLinkDefaultIteratorName {
		t.Fatalf("derivation = %+v, want source/target/iterator provenance", derivation)
	}
	if got.Neighborhood.Edges[0].Properties[latentAssertionEdgePropertyOrigin] != "derived" {
		t.Fatalf("edge properties = %+v, want derived origin", got.Neighborhood.Edges[0].Properties)
	}
}

func TestLatentLinkProducerProvenanceReachableFromBoundedGraph(t *testing.T) {
	ctx := context.Background()
	corpus, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer corpus.Close()
	source := ingestLatentReadPathDocument(t, corpus, "producer-source")
	target := ingestLatentReadPathDocument(t, corpus, "producer-target")
	if err := corpus.PutLatentLinkCells(ctx, []LatentLinkCell{
		latentReadPathCell(source.Document.ID, target.Document.ID, 42, false),
	}); err != nil {
		t.Fatal(err)
	}

	first, err := corpus.BoundedNeighborhood(ctx, BoundedNeighborhoodRequest{
		NodeIDs: []shoal.ID{source.Document.ID}, Depth: 1, Fanout: 8, MaxNodes: 8,
		EdgeTypes: []string{latentReadPathEdgeType(t)},
		Direction: GraphDirectionOutgoing,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(first.Neighborhood.Assertions) != 1 {
		t.Fatalf("source graph assertions = %+v, want one", first.Neighborhood)
	}
	assertion := first.Neighborhood.Assertions[0]
	derivation, ok := assertion.Evidence()[0].Derivation()
	if !ok {
		t.Fatal("missing derivation")
	}
	producer, assertionNode, producedEdge, ok, err :=
		ProducerGraphElementsForAssertion(assertion)
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatal("derived assertion did not yield producer graph elements")
	}

	produced, err := corpus.BoundedNeighborhood(ctx, BoundedNeighborhoodRequest{
		NodeIDs: []shoal.ID{producer.ID}, Depth: 1, Fanout: 8, MaxNodes: 8,
		EdgeTypes: []string{graph.EdgeTypeProduced},
		Direction: GraphDirectionOutgoing,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !hasLatentNode(produced.Neighborhood, producer.ID, graph.NodeKindProducer) ||
		!hasLatentNode(produced.Neighborhood, assertionNode.ID, graph.NodeKindDerivedAssertion) {
		t.Fatalf("producer graph nodes = %+v, want producer and assertion node", produced.Neighborhood.Nodes)
	}
	if len(produced.Neighborhood.Edges) != 1 ||
		produced.Neighborhood.Edges[0].ID != producedEdge.ID ||
		produced.Neighborhood.Edges[0].From != producer.ID ||
		produced.Neighborhood.Edges[0].To != assertion.ID() {
		t.Fatalf("producer derivation edges = %+v, want %s -> %s",
			produced.Neighborhood.Edges, producer.ID, assertion.ID())
	}
	if len(produced.Neighborhood.Assertions) != 1 ||
		produced.Neighborhood.Assertions[0].ID() != assertion.ID() {
		t.Fatalf("producer graph assertions = %+v, want produced assertion", produced.Neighborhood.Assertions)
	}
	if produced.Neighborhood.Edges[0].Properties[latentAssertionEdgePropertyDerivationID] !=
		string(derivation.ID()) {
		t.Fatalf("producer edge properties = %+v, want derivation %s",
			produced.Neighborhood.Edges[0].Properties, derivation.ID())
	}
}

func TestProducerGraphNodeIDCanonicalizesMetadataAndOptions(t *testing.T) {
	first := producerNodeForProjection(t, LatentLinkAssertionProjection{
		IteratorOptions: shoal.Metadata{"zeta": "last", "alpha": "first"},
		Provenance: provenanceForProducerTest(t, "v1", shoal.Metadata{
			"right": "2", "left": "1",
		}),
	})
	for index := 0; index < 50; index++ {
		again := producerNodeForProjection(t, LatentLinkAssertionProjection{
			IteratorOptions: shoal.Metadata{"alpha": "first", "zeta": "last"},
			Provenance: provenanceForProducerTest(t, "v1", shoal.Metadata{
				"left": "1", "right": "2",
			}),
		})
		if again.ID != first.ID {
			t.Fatalf("producer ID changed with metadata insertion order: got %s want %s",
				again.ID, first.ID)
		}
	}
	changedThreshold := producerNodeForProjection(t, LatentLinkAssertionProjection{
		SimilarityThreshold: 0.92,
		IteratorOptions:     shoal.Metadata{"alpha": "first", "zeta": "last"},
		Provenance: provenanceForProducerTest(t, "v1", shoal.Metadata{
			"left": "1", "right": "2",
		}),
	})
	if changedThreshold.ID == first.ID {
		t.Fatalf("threshold change did not change producer ID %s", first.ID)
	}
	changedExtractor := producerNodeForProjection(t, LatentLinkAssertionProjection{
		IteratorOptions: shoal.Metadata{"alpha": "first", "zeta": "last"},
		Provenance: provenanceForProducerTest(t, "v2", shoal.Metadata{
			"left": "1", "right": "2",
		}),
	})
	if changedExtractor.ID == first.ID {
		t.Fatalf("extractor version change did not change producer ID %s", first.ID)
	}
}

func TestLatentLinkGraphReadSkipsTombstonedCells(t *testing.T) {
	ctx := context.Background()
	corpus, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer corpus.Close()
	source := ingestLatentReadPathDocument(t, corpus, "deleted-source")
	target := ingestLatentReadPathDocument(t, corpus, "deleted-target")
	live := latentReadPathCell(source.Document.ID, target.Document.ID, 10, false)
	deleted := latentReadPathCell(source.Document.ID, target.Document.ID, 20, true)
	if err := corpus.PutLatentLinkCells(ctx, []LatentLinkCell{live, deleted}); err != nil {
		t.Fatal(err)
	}

	got, err := corpus.BoundedNeighborhood(ctx, BoundedNeighborhoodRequest{
		NodeIDs: []shoal.ID{source.Document.ID}, Depth: 1, Fanout: 8, MaxNodes: 8,
		EdgeTypes: []string{latentReadPathEdgeType(t)},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Neighborhood.Edges) != 0 || len(got.Neighborhood.Assertions) != 0 {
		t.Fatalf("tombstoned latent link was returned: %+v", got.Neighborhood)
	}
}

// TestLatentLinkGraphReadDropsLabelledCells is #569 end to end, through the
// read path the leak actually travelled.
//
// The latent-edge iterator writes the compound visibility of the two
// embeddings onto the link cell. scanLatentLinkCellsLocked scans in system
// context (nil Authorizations), so the cell is visible to the scan, and
// nothing past the projection carries the label — the graph edge is gated
// only by its endpoints' AccessRules. A reader authorized on both endpoints
// therefore saw a labelled relationship and its similarity score.
//
// Both links are written in one corpus on purpose. Asserting only that the
// labelled edge is absent would pass if the latent read path were broken for
// any other reason — a changed edge type, a projection error, the assertion
// cap — so the unlabelled link is here to prove the path still reaches the
// graph, and the labelled one to prove the refusal turns on the label.
func TestLatentLinkGraphReadDropsLabelledCells(t *testing.T) {
	ctx := context.Background()
	corpus, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer corpus.Close()
	source := ingestLatentReadPathDocument(t, corpus, "labelled-source")
	plain := ingestLatentReadPathDocument(t, corpus, "labelled-plain-target")
	secret := ingestLatentReadPathDocument(t, corpus, "labelled-secret-target")

	labelled := latentReadPathCell(source.Document.ID, secret.Document.ID, 20, false)
	labelled.ColumnVisibility = []byte("A&B")
	if err := corpus.PutLatentLinkCells(ctx, []LatentLinkCell{
		latentReadPathCell(source.Document.ID, plain.Document.ID, 10, false),
		labelled,
	}); err != nil {
		t.Fatal(err)
	}

	got, err := corpus.BoundedNeighborhood(ctx, BoundedNeighborhoodRequest{
		NodeIDs: []shoal.ID{source.Document.ID}, Depth: 1, Fanout: 8, MaxNodes: 8,
		EdgeTypes: []string{latentReadPathEdgeType(t)},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Neighborhood.Assertions) != 1 {
		t.Fatalf("derived assertions = %d, want 1: the unlabelled link must "+
			"still reach the graph and the labelled one must not",
			len(got.Neighborhood.Assertions))
	}
	derivation, ok := got.Neighborhood.Assertions[0].Evidence()[0].Derivation()
	if !ok {
		t.Fatal("derived assertion evidence lost its derivation")
	}
	// Named, not counted. A count of one also holds if the labelled link
	// survived and the unlabelled one was dropped instead.
	if derivation.TargetEndpoint() != plain.Document.ID {
		t.Fatalf("the surviving derivation points at %q, want the unlabelled "+
			"target %q: a labelled relationship reached a reader authorized "+
			"only on its endpoints",
			derivation.TargetEndpoint(), plain.Document.ID)
	}
	for _, edge := range got.Neighborhood.Edges {
		if edge.To == secret.Document.ID || edge.From == secret.Document.ID {
			t.Fatalf("a graph edge still reaches the labelled endpoint %q",
				secret.Document.ID)
		}
	}
}

func TestLatentLinkGraphReadErrorsAtExplicitCap(t *testing.T) {
	ctx := context.Background()
	corpus, err := OpenWithOptions(t.TempDir(), Options{
		MaxLatentDerivedAssertionsPerGraphRead: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer corpus.Close()
	source := ingestLatentReadPathDocument(t, corpus, "cap-source")
	first := ingestLatentReadPathDocument(t, corpus, "cap-first")
	second := ingestLatentReadPathDocument(t, corpus, "cap-second")
	if err := corpus.PutLatentLinkCells(ctx, []LatentLinkCell{
		latentReadPathCell(source.Document.ID, first.Document.ID, 10, false),
		latentReadPathCell(source.Document.ID, second.Document.ID, 11, false),
	}); err != nil {
		t.Fatal(err)
	}

	_, err = corpus.BoundedNeighborhood(ctx, BoundedNeighborhoodRequest{
		NodeIDs: []shoal.ID{source.Document.ID}, Depth: 1, Fanout: 8, MaxNodes: 8,
		EdgeTypes: []string{latentReadPathEdgeType(t)},
	})
	if !shoal.IsErrorCode(err, shoal.ErrorUnavailable) {
		t.Fatalf("cap error = %v, want unavailable", err)
	}
}

func ingestLatentReadPathDocument(
	t *testing.T,
	corpus *Explorer,
	name string,
) IngestResult {
	t.Helper()
	result, err := corpus.Ingest(context.Background(), Source{
		URI:       "file:///" + name + ".txt",
		Title:     name,
		MediaType: MediaTypeText,
		Content:   name,
	})
	if err != nil {
		t.Fatal(err)
	}
	return result
}

func latentReadPathCell(
	source, target shoal.ID,
	timestamp int64,
	deleted bool,
) LatentLinkCell {
	return LatentLinkCell{
		Row:             []byte("cell-a:" + string(source)),
		ColumnFamily:    []byte("link"),
		ColumnQualifier: []byte(target),
		Timestamp:       timestamp,
		Deleted:         deleted,
		Value:           []byte("0.91"),
	}
}

func latentReadPathEdgeType(t *testing.T) string {
	t.Helper()
	projection, err := DefaultLatentLinkAssertionProjection()
	if err != nil {
		t.Fatal(err)
	}
	return string(projection.Predicate)
}

func hasLatentNode(neighborhood Neighborhood, id shoal.ID, kind string) bool {
	for _, node := range neighborhood.Nodes {
		if node.ID == id && node.Kind == kind {
			return true
		}
	}
	return false
}

func producerNodeForProjection(
	t *testing.T,
	projection LatentLinkAssertionProjection,
) graph.Node {
	t.Helper()
	defaultProjection, err := DefaultLatentLinkAssertionProjection()
	if err != nil {
		t.Fatal(err)
	}
	if projection.EmbeddingModel == "" {
		projection.EmbeddingModel = defaultProjection.EmbeddingModel
	}
	if projection.EmbeddingModelVersion == "" {
		projection.EmbeddingModelVersion = defaultProjection.EmbeddingModelVersion
	}
	if projection.SimilarityThreshold == 0 {
		projection.SimilarityThreshold = defaultProjection.SimilarityThreshold
	}
	if projection.Predicate == "" {
		projection.Predicate = defaultProjection.Predicate
	}
	if projection.SubjectType == "" {
		projection.SubjectType = defaultProjection.SubjectType
	}
	if projection.ObjectType == "" {
		projection.ObjectType = defaultProjection.ObjectType
	}
	if projection.Provenance.Provider() == "" {
		projection.Provenance = defaultProjection.Provenance
	}
	cells := []LatentLinkCell{{
		Row:             []byte("cell-17:entity:source"),
		ColumnFamily:    []byte("link"),
		ColumnQualifier: []byte("entity:target"),
		Timestamp:       7,
		Value:           []byte("0.95"),
	}}
	assertions, err := ProjectLatentLinkAssertions(cells, projection)
	if err != nil {
		t.Fatal(err)
	}
	derivation, ok := assertions[0].Evidence()[0].Derivation()
	if !ok {
		t.Fatal("missing derivation")
	}
	node, err := producerGraphNode(assertions[0], derivation)
	if err != nil {
		t.Fatal(err)
	}
	return node
}

func provenanceForProducerTest(
	t *testing.T,
	extractorVersion string,
	metadata shoal.Metadata,
) ontology.ExtractionProvenance {
	t.Helper()
	provenance, err := ontology.NewExtractionProvenance(
		"shoal",
		"latent-edge-projection",
		"v1",
		"latent-link-projection",
		"v1",
		"ProjectLatentLinkAssertions",
		extractorVersion,
		metadata,
	)
	if err != nil {
		t.Fatal(err)
	}
	return provenance
}
