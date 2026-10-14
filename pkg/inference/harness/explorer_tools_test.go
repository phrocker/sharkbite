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

package harness

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/phrocker/shoal-oss/pkg/contextpack"
	"github.com/phrocker/shoal-oss/pkg/document"
	"github.com/phrocker/shoal-oss/pkg/explorer"
	"github.com/phrocker/shoal-oss/pkg/graph"
	"github.com/phrocker/shoal-oss/pkg/inference"
	"github.com/phrocker/shoal-oss/pkg/ontology"
	"github.com/phrocker/shoal-oss/pkg/retrieval"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

func toolContextForPack(pack inference.ContextPack, correlation shoal.ID) ToolContext {
	return ToolContext{
		requestID:     "request",
		contextPackID: pack.ID(),
		context:       pack,
		correlation:   correlation,
		snapshot:      pack.Snapshot(),
		auth:          pack.Authorization(),
	}
}

func TestExplorerToolHostUsesServerBoundedNeighbors(t *testing.T) {
	pack, _, _ := fixture(t)
	client := &boundedClientStub{
		snapshot: explorer.Snapshot{
			ID:   string(pack.Snapshot().ID()),
			AsOf: pack.Snapshot().AsOf(),
		},
	}
	host := &ExplorerToolHost{Client: client, BoundedClient: client}
	request, err := NewNeighborsRequest("node-a", 1, 2)
	if err != nil {
		t.Fatal(err)
	}
	result, err := host.Neighbors(context.Background(), ToolContext{
		requestID:     "request",
		contextPackID: pack.ID(),
		context:       pack,
		correlation:   "neighbors",
		snapshot:      pack.Snapshot(),
		auth:          pack.Authorization(),
	}, request)
	if err != nil {
		t.Fatal(err)
	}
	if client.last.Fanout != 2 || client.last.MaxNodes != 3 || client.last.MaxScannedEdges != 6 ||
		client.last.Direction != explorer.GraphDirectionOutgoing {
		t.Fatalf("bounds not propagated: %#v", client.last)
	}
	if result.Snapshot() != pack.Snapshot() || result.Authorization() != pack.Authorization() {
		t.Fatal("pins changed")
	}
	anchors := result.Anchors()
	if len(anchors) != 1 {
		t.Fatalf("anchors = %d", len(anchors))
	}
	path, ok := anchors[0].Path()
	if !ok || path.Nodes[0].ID != "node-a" || len(path.Edges) != 1 {
		t.Fatalf("unexpected path: %#v", path)
	}
}

func TestNewExplorerToolHostRespectsOptionalBoundedAvailability(t *testing.T) {
	client := &unavailableBoundedClient{}
	host, err := NewExplorerToolHost(client, contextpack.Builder{})
	if err != nil {
		t.Fatal(err)
	}
	if host.BoundedClient != nil {
		t.Fatal("unavailable bounded capability was enabled")
	}
}

func TestExplorerToolHostCacheIdentityDelimitsModesAndMetadata(t *testing.T) {
	client := &boundedClientStub{}
	withModes := &ExplorerToolHost{
		Client:         client,
		ClientIdentity: "client-v1",
		RetrievalModes: []retrieval.Mode{retrieval.ModeLexical, retrieval.ModeVector},
	}
	withMetadata := &ExplorerToolHost{
		Client:         client,
		ClientIdentity: "client-v1",
		Metadata:       shoal.Metadata{"lexical": "vector"},
	}
	modesIdentity, err := withModes.CacheIdentity()
	if err != nil {
		t.Fatal(err)
	}
	metadataIdentity, err := withMetadata.CacheIdentity()
	if err != nil {
		t.Fatal(err)
	}
	if modesIdentity == metadataIdentity {
		t.Fatal("cache identity collided across retrieval modes and metadata")
	}
}

func TestExplorerToolHostAllowsFanoutOneMultiHopPath(t *testing.T) {
	pack, _, _ := fixture(t)
	custom := budgets()
	custom.MaxEvidence = 5
	custom.MaxGraphNodes = 3
	client := &boundedClientStub{
		snapshot: explorer.Snapshot{ID: string(pack.Snapshot().ID()), AsOf: pack.Snapshot().AsOf()},
		boundedResponse: explorer.BoundedNeighborhood{Neighborhood: explorer.Neighborhood{
			Nodes: []graph.Node{
				{ID: "node-a", Kind: "entity"},
				{ID: "node-b", Kind: "entity"},
				{ID: "node-c", Kind: "entity"},
			},
			Edges: []graph.Edge{
				{ID: "edge-a-b", From: "node-a", To: "node-b", Type: "related", Weight: 1},
				{ID: "edge-b-c", From: "node-b", To: "node-c", Type: "related", Weight: 1},
			},
		}},
	}
	host := &ExplorerToolHost{Client: client, BoundedClient: client}
	request, err := NewNeighborsRequest("node-a", 2, 1)
	if err != nil {
		t.Fatal(err)
	}
	result, err := host.Neighbors(context.Background(), ToolContext{
		requestID:     "request",
		contextPackID: pack.ID(),
		context:       pack,
		budgets:       custom,
		correlation:   "neighbors",
		snapshot:      pack.Snapshot(),
		auth:          pack.Authorization(),
	}, request)
	if err != nil {
		t.Fatal(err)
	}
	if client.last.MaxNodes != 3 {
		t.Fatalf("max nodes = %d", client.last.MaxNodes)
	}
	if len(result.Anchors()) != 2 {
		t.Fatalf("anchors = %d", len(result.Anchors()))
	}
}

func TestExplorerToolHostAllowsEmptyRetrieveResult(t *testing.T) {
	pack, _, _ := fixture(t)
	client := &boundedClientStub{
		retrieveResponse: retrieval.Response{},
		embeddingEvent: explorer.EmbeddingQueryEvent{
			SpaceIdentities: []string{"candidate-space"},
			Completed:       []string{"candidate-space"},
		},
	}
	host := &ExplorerToolHost{
		Client: client, RetrievalModes: []retrieval.Mode{retrieval.ModeVector}}
	request, err := NewRetrieveRequest("no hits", 1)
	if err != nil {
		t.Fatal(err)
	}
	result, err := host.Retrieve(context.Background(), ToolContext{
		requestID:     "request",
		contextPackID: pack.ID(),
		context:       pack,
		correlation:   "retrieve",
		snapshot:      pack.Snapshot(),
		auth:          pack.Authorization(),
	}, request)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Anchors()) != 0 || !client.retrieve.AsOf.Equal(pack.Snapshot().AsOf()) {
		t.Fatalf("empty retrieve was not pinned and empty: %#v", result)
	}
	if got := result.EmbeddingSpaces(); len(got.Identities) != 0 ||
		got.Digest != "" {
		t.Fatalf("empty result embedding spaces = %+v", got)
	}
}

func TestEmbeddingParticipationCollectorUsesOnlyParticipatingSpaces(t *testing.T) {
	collector := &embeddingParticipationCollector{}
	collector.observe(explorer.EmbeddingQueryEvent{
		SpaceIdentities: []string{"candidate-only", "space-b", "space-a"},
		Completed:       []string{"candidate-only", "space-b", "space-a"},
		Participating:   []string{"space-b", "space-a"},
	})
	got, err := collector.result(true)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Identities) != 2 ||
		got.Identities[0] != "space-a" ||
		got.Identities[1] != "space-b" ||
		got.Digest == "" {
		t.Fatalf("participating embedding spaces = %+v", got)
	}

	empty := &embeddingParticipationCollector{}
	empty.observe(explorer.EmbeddingQueryEvent{
		SpaceIdentities: []string{"candidate-only"},
		Completed:       []string{"candidate-only"},
	})
	if _, err := empty.result(true); err == nil {
		t.Fatal("scored vector result without participation was accepted")
	}
}

func TestExplorerToolHostRetrievesFromEmbeddedExplorerWithSnapshotPin(t *testing.T) {
	corpus, err := explorer.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer corpus.Close()
	_, err = corpus.Ingest(context.Background(), explorer.Source{
		URI:       "memory://doc",
		Title:     "Quartz",
		MediaType: explorer.MediaTypeMarkdown,
		Content:   "# Quartz\n\nThe quartz relay uses grounded evidence.",
	})
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := corpus.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	snapshotPin, err := inference.NewSnapshotPin(shoal.ID(snapshot.ID), snapshot.AsOf)
	if err != nil {
		t.Fatal(err)
	}
	authPin, err := inference.NewAuthPin("auth", snapshot.AsOf.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	host, err := NewExplorerToolHost(corpus, contextpack.Builder{})
	if err != nil {
		t.Fatal(err)
	}
	host.PolicyID = "policy"
	request, err := NewRetrieveRequest("quartz relay", 1)
	if err != nil {
		t.Fatal(err)
	}
	result, err := host.Retrieve(context.Background(), ToolContext{
		requestID:   "request",
		correlation: "retrieve",
		snapshot:    snapshotPin,
		auth:        authPin,
		budgets:     budgets(),
	}, request)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Anchors()) == 0 {
		t.Fatal("embedded Explorer retrieval returned no evidence")
	}
	if result.Snapshot() != snapshotPin || result.Authorization() != authPin {
		t.Fatal("retrieve pins changed")
	}
}

func TestExplorerToolHostRejectsStaleSnapshotBeforeTools(t *testing.T) {
	pack, _, _ := fixture(t)
	client := &boundedClientStub{snapshot: explorer.Snapshot{
		ID:   "other-snapshot",
		AsOf: pack.Snapshot().AsOf(),
	}}
	host := &ExplorerToolHost{Client: client, BoundedClient: client}
	retrieve, err := NewRetrieveRequest("query", 1)
	if err != nil {
		t.Fatal(err)
	}
	_, err = host.Retrieve(context.Background(), toolContextForPack(pack, "retrieve"), retrieve)
	if !errors.Is(err, ErrInvalid) {
		t.Fatalf("retrieve error = %v", err)
	}
	open, err := NewOpenSectionRequest("document", "revision", "section-initial")
	if err != nil {
		t.Fatal(err)
	}
	_, err = host.OpenSection(context.Background(), toolContextForPack(pack, "open"), open)
	if !errors.Is(err, ErrInvalid) {
		t.Fatalf("open error = %v", err)
	}
}

func TestExplorerToolHostValidatesEmptyRetrieveResponse(t *testing.T) {
	pack, _, _ := fixture(t)
	client := &boundedClientStub{
		retrieveResponse: retrieval.Response{RequestID: shoal.ID(strings.Repeat("x", shoal.MaxIDBytes+1))},
	}
	host := &ExplorerToolHost{Client: client}
	request, err := NewRetrieveRequest("no hits", 1)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := host.Retrieve(context.Background(), toolContextForPack(pack, "retrieve"), request); err == nil {
		t.Fatal("malformed empty response accepted")
	}
}

func TestExplorerToolHostPreflightsRetrieveShapeBeforeGraphVerification(t *testing.T) {
	pack, _, _ := fixture(t)
	custom := budgets()
	custom.MaxEvidence = 1
	custom.MaxGraphNodes = 2
	client := &boundedClientStub{
		snapshot: explorer.Snapshot{ID: string(pack.Snapshot().ID()), AsOf: pack.Snapshot().AsOf()},
		retrieveResponse: retrieval.Response{Results: []retrieval.Result{{
			ID: "result",
			Evidence: []retrieval.Evidence{{
				Path: graph.Path{
					Nodes: []graph.Node{
						{ID: "node-a", Kind: "entity"},
						{ID: "node-b", Kind: "entity"},
						{ID: "node-c", Kind: "entity"},
					},
					Edges: []graph.Edge{
						{ID: "edge-a-b", From: "node-a", To: "node-b", Type: "related", Weight: 1},
						{ID: "edge-b-c", From: "node-b", To: "node-c", Type: "related", Weight: 1},
					},
				},
			}},
		}}},
	}
	host := &ExplorerToolHost{Client: client, BoundedClient: client}
	request, err := NewRetrieveRequest("query", 1)
	if err != nil {
		t.Fatal(err)
	}
	call := toolContextForPack(pack, "retrieve")
	call.budgets = custom
	_, err = host.Retrieve(context.Background(), call, request)
	if !errors.Is(err, ErrBudgetExhausted) {
		t.Fatalf("error = %v", err)
	}
	if client.last.NodeIDs != nil {
		t.Fatalf("graph verification ran before preflight: %#v", client.last)
	}
}

func TestExplorerToolHostContextBuilderUsesBoundedGraphReader(t *testing.T) {
	client := &boundedClientStub{}
	host := &ExplorerToolHost{Client: client, BoundedClient: client}
	reader := host.builderFor(2, 3).Reader
	if _, err := reader.Neighborhood(context.Background(), explorer.NeighborhoodRequest{
		NodeIDs: []shoal.ID{"node-a"},
		Depth:   1,
	}); err != nil {
		t.Fatal(err)
	}
	if client.neighborhoodCalls != 0 {
		t.Fatal("legacy unbounded Neighborhood was called")
	}
	if client.last.Fanout != 2 || client.last.MaxNodes != 3 || client.last.MaxScannedEdges != 6 {
		t.Fatalf("bounded graph limits not propagated: %#v", client.last)
	}
}

func TestExplorerToolHostVerifiesRetrievalGraphNodes(t *testing.T) {
	client := &boundedClientStub{}
	host := &ExplorerToolHost{Client: client, BoundedClient: client}
	err := host.verifyRetrievalPaths(context.Background(), retrieval.Response{
		Results: []retrieval.Result{{
			ID: "result",
			Evidence: []retrieval.Evidence{{
				Path: graph.Path{Nodes: []graph.Node{{ID: "node-a", Kind: "forged"}}},
			}},
		}},
	}, budgets())
	if !errors.Is(err, ErrInvalid) {
		t.Fatalf("forged node error = %v", err)
	}
	err = host.verifyRetrievalPaths(context.Background(), retrieval.Response{
		Results: []retrieval.Result{{
			ID: "result",
			Evidence: []retrieval.Evidence{{
				Path: graph.Path{Nodes: []graph.Node{{ID: "node-a", Kind: "entity"}}},
			}},
		}},
	}, budgets())
	if err != nil {
		t.Fatalf("canonical edge-free path rejected: %v", err)
	}
}

func TestGraphAnchorsFromNeighborhoodKeepsMultiHopPaths(t *testing.T) {
	anchors, err := graphAnchorsFromNeighborhood("node-a", explorer.Neighborhood{
		Nodes: []graph.Node{
			{ID: "node-a", Kind: "entity"},
			{ID: "node-b", Kind: "entity"},
			{ID: "node-c", Kind: "entity"},
		},
		Edges: []graph.Edge{
			{ID: "edge-a-b", From: "node-a", To: "node-b", Type: "related", Weight: 1},
			{ID: "edge-b-c", From: "node-b", To: "node-c", Type: "related", Weight: 1},
		},
	}, 2, 4)
	if err != nil {
		t.Fatal(err)
	}
	if len(anchors) != 2 {
		t.Fatalf("anchors = %d", len(anchors))
	}
	path, ok := anchors[1].Path()
	if !ok || len(path.Edges) != 2 || path.Nodes[0].ID != "node-a" || path.Nodes[2].ID != "node-c" {
		t.Fatalf("multi-hop path omitted: %#v", path)
	}
}

func TestGraphAnchorsFromNeighborhoodPreservesAuthoritativeAssertions(
	t *testing.T,
) {
	evidence, err := ontology.NewEvidenceRef(
		document.Citation{
			DocumentID: "document-1", RevisionID: "revision-1",
			SectionID: "section-1", SpanID: "span-1",
			Range: document.SourceRange{
				Start: document.SourcePosition{Offset: 0, Page: 1},
				End:   document.SourcePosition{Offset: 7, Page: 1},
			},
		},
		"node-a",
		nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	provenance, err := ontology.NewExtractionProvenance(
		"provider", "model", "v1", "prompt", "v1", "extractor", "v1", nil)
	if err != nil {
		t.Fatal(err)
	}
	concept, err := ontology.NewConceptDefinition(
		"graph-node", "Graph Node", "A graph node", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	relationship, err := ontology.NewRelationshipDefinition(
		"related", "Related", "Relates graph nodes",
		[]shoal.ID{concept.ID()}, []shoal.ID{concept.ID()}, nil, true, nil)
	if err != nil {
		t.Fatal(err)
	}
	value, err := ontology.NewReferenceValue("node-b")
	if err != nil {
		t.Fatal(err)
	}
	for _, origin := range []ontology.AssertionOrigin{
		ontology.AssertionExplicit,
		ontology.AssertionInferred,
	} {
		assertion, assertionErr := ontology.NewAssertion(
			"node-a", relationship.ID(), value, origin, 1,
			[]ontology.EvidenceRef{evidence}, provenance,
			shoal.Metadata{"shoal.graph.edge_id": "edge-a-b"},
		)
		if assertionErr != nil {
			t.Fatal(assertionErr)
		}
		anchors, anchorErr := graphAnchorsFromNeighborhood(
			"node-a",
			explorer.Neighborhood{
				Nodes: []graph.Node{
					{ID: "node-a", Kind: "entity"},
					{ID: "node-b", Kind: "entity"},
				},
				Edges: []graph.Edge{{
					ID: "edge-a-b", From: "node-a", To: "node-b",
					Type: string(relationship.ID()), Weight: 1,
				}},
				Assertions: []ontology.Assertion{assertion},
			},
			1,
			1,
		)
		if anchorErr != nil {
			t.Fatal(anchorErr)
		}
		reference, referenceErr := anchors[0].EvidenceReference()
		if referenceErr != nil {
			t.Fatal(referenceErr)
		}
		if len(reference.Assertions) != 1 ||
			reference.Assertions[0].AssertionID != assertion.ID() ||
			reference.Assertions[0].EdgeID != "edge-a-b" ||
			reference.Assertions[0].Origin != origin {
			t.Fatalf("%s assertion reference = %#v", origin, reference.Assertions)
		}
	}
}

func TestGraphAnchorsFromNeighborhoodBoundsCyclicPaths(t *testing.T) {
	anchors, err := graphAnchorsFromNeighborhood("node-a", explorer.Neighborhood{
		Nodes: []graph.Node{
			{ID: "node-a", Kind: "entity"},
			{ID: "node-b", Kind: "entity"},
			{ID: "node-c", Kind: "entity"},
		},
		Edges: []graph.Edge{
			{ID: "edge-a-b", From: "node-a", To: "node-b", Type: "related", Weight: 1},
			{ID: "edge-a-c", From: "node-a", To: "node-c", Type: "related", Weight: 1},
			{ID: "edge-b-a", From: "node-b", To: "node-a", Type: "related", Weight: 1},
			{ID: "edge-b-c", From: "node-b", To: "node-c", Type: "related", Weight: 1},
			{ID: "edge-c-a", From: "node-c", To: "node-a", Type: "related", Weight: 1},
			{ID: "edge-c-b", From: "node-c", To: "node-b", Type: "related", Weight: 1},
		},
	}, 3, 3)
	if err != nil {
		t.Fatal(err)
	}
	if len(anchors) > 3 {
		t.Fatalf("anchors exceeded bound: %d", len(anchors))
	}
	for _, anchor := range anchors {
		path, ok := anchor.Path()
		if !ok || len(path.Edges) > 3 {
			t.Fatalf("invalid bounded path: %#v", path)
		}
	}
}

func TestGraphAnchorsFromNeighborhoodRejectsDuplicateGraphIDs(t *testing.T) {
	_, err := graphAnchorsFromNeighborhood("node-a", explorer.Neighborhood{
		Nodes: []graph.Node{
			{ID: "node-a", Kind: "entity"},
			{ID: "node-a", Kind: "other"},
		},
	}, 1, 1)
	if !errors.Is(err, ErrInvalid) {
		t.Fatalf("duplicate node error = %v", err)
	}
	_, err = graphAnchorsFromNeighborhood("node-a", explorer.Neighborhood{
		Nodes: []graph.Node{
			{ID: "node-a", Kind: "entity"},
			{ID: "node-b", Kind: "entity"},
			{ID: "node-c", Kind: "entity"},
		},
		Edges: []graph.Edge{
			{ID: "edge-duplicate", From: "node-a", To: "node-b", Type: "related", Weight: 1},
			{ID: "edge-duplicate", From: "node-a", To: "node-c", Type: "related", Weight: 1},
		},
	}, 1, 2)
	if !errors.Is(err, ErrInvalid) {
		t.Fatalf("duplicate edge error = %v", err)
	}
}

func TestValidateBoundedNeighborhoodResponseEnforcesServerBounds(t *testing.T) {
	request := explorer.BoundedNeighborhoodRequest{
		NodeIDs: []shoal.ID{"node-a"}, Depth: 1, Fanout: 1, MaxNodes: 2,
		Direction: explorer.GraphDirectionOutgoing,
	}
	err := validateBoundedNeighborhoodResponse(request, explorer.Neighborhood{
		Nodes: []graph.Node{
			{ID: "node-a", Kind: "entity"},
			{ID: "node-b", Kind: "entity"},
			{ID: "node-c", Kind: "entity"},
		},
	})
	if !errors.Is(err, ErrBudgetExhausted) {
		t.Fatalf("node bound error = %v", err)
	}
	fanoutRequest := request
	fanoutRequest.MaxNodes = 3
	err = validateBoundedNeighborhoodResponse(fanoutRequest, explorer.Neighborhood{
		Nodes: []graph.Node{
			{ID: "node-a", Kind: "entity"},
			{ID: "node-b", Kind: "entity"},
			{ID: "node-c", Kind: "entity"},
		},
		Edges: []graph.Edge{
			{ID: "edge-a-b", From: "node-a", To: "node-b", Type: "related", Weight: 1},
			{ID: "edge-a-c", From: "node-a", To: "node-c", Type: "related", Weight: 1},
		},
	})
	if !errors.Is(err, ErrBudgetExhausted) {
		t.Fatalf("fanout bound error = %v", err)
	}
}

func TestSectionAnchorsHonorsContextAndEvidenceBudget(t *testing.T) {
	view := explorer.DocumentView{
		Document: document.Document{
			ID: "document", RevisionID: "revision", Title: "Document",
			RootSectionID: "section",
		},
		Revision: document.Revision{ID: "revision", DocumentID: "document"},
		Root: explorer.SectionView{
			Section: document.Section{ID: "section", DocumentID: "document", RevisionID: "revision"},
			Spans: []document.Span{
				{
					ID: "span-a", DocumentID: "document", RevisionID: "revision", SectionID: "section",
					Text: "a", Range: document.SourceRange{End: document.SourcePosition{Offset: 1}},
				},
				{
					ID: "span-b", DocumentID: "document", RevisionID: "revision", SectionID: "section",
					Text: "b", Range: document.SourceRange{Start: document.SourcePosition{Offset: 1}, End: document.SourcePosition{Offset: 2}},
				},
			},
		},
	}
	request, err := NewOpenSectionRequest("document", "revision", "section")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := sectionAnchors(ctx, view, request, 1); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled error = %v", err)
	}
	if _, err := sectionAnchors(context.Background(), view, request, 1); !errors.Is(err, ErrBudgetExhausted) {
		t.Fatalf("budget error = %v", err)
	}
}

type boundedClientStub struct {
	last              explorer.BoundedNeighborhoodRequest
	retrieve          retrieval.Request
	retrieveResponse  retrieval.Response
	embeddingEvent    explorer.EmbeddingQueryEvent
	snapshot          explorer.Snapshot
	boundedResponse   explorer.BoundedNeighborhood
	neighborhoodCalls int
}

type unavailableBoundedClient struct {
	boundedClientStub
}

func (*unavailableBoundedClient) BoundedAvailable() bool { return false }

func (b *boundedClientStub) Retrieve(ctx context.Context, request retrieval.Request) (retrieval.Response, error) {
	b.retrieve = request
	explorer.ReportEmbeddingQueryEvent(ctx, b.embeddingEvent)
	return b.retrieveResponse, nil
}

func (b *boundedClientStub) Ingest(context.Context, explorer.Source) (explorer.IngestResult, error) {
	return explorer.IngestResult{}, nil
}

func (b *boundedClientStub) Documents(context.Context) ([]explorer.DocumentSummary, error) {
	return nil, nil
}

func (b *boundedClientStub) Document(context.Context, shoal.ID, shoal.ID) (explorer.DocumentView, error) {
	return explorer.DocumentView{}, nil
}

func (b *boundedClientStub) Connect(context.Context, graph.Edge) error { return nil }

func (b *boundedClientStub) Neighborhood(context.Context, explorer.NeighborhoodRequest) (explorer.Neighborhood, error) {
	b.neighborhoodCalls++
	return explorer.Neighborhood{}, nil
}

func (b *boundedClientStub) Snapshot(context.Context) (explorer.Snapshot, error) {
	return b.snapshot, nil
}

func (b *boundedClientStub) BoundedNeighborhood(
	_ context.Context,
	request explorer.BoundedNeighborhoodRequest,
) (explorer.BoundedNeighborhood, error) {
	b.last = request
	if len(b.boundedResponse.Neighborhood.Nodes) > 0 {
		return b.boundedResponse, nil
	}
	return explorer.BoundedNeighborhood{Neighborhood: explorer.Neighborhood{
		Nodes: []graph.Node{
			{ID: "node-a", Kind: "entity"},
			{ID: "node-b", Kind: "entity"},
		},
		Edges: []graph.Edge{{
			ID: "edge-a-b", From: "node-a", To: "node-b",
			Type: "related", Weight: 1,
		}},
	}}, nil
}
