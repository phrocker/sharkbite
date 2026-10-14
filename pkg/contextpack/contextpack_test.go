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

package contextpack

import (
	"context"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/phrocker/shoal-oss/pkg/document"
	"github.com/phrocker/shoal-oss/pkg/explorer"
	"github.com/phrocker/shoal-oss/pkg/graph"
	"github.com/phrocker/shoal-oss/pkg/inference"
	"github.com/phrocker/shoal-oss/pkg/interaction"
	"github.com/phrocker/shoal-oss/pkg/ontology"
	"github.com/phrocker/shoal-oss/pkg/retrieval"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

var testDirectorySequence atomic.Uint64

func TestBuildFromEmbeddedExplorerExactEvidenceAndDeterminism(t *testing.T) {
	client, request, response, pins := embeddedFixture(t)
	builder := Builder{Reader: client}
	input := InitialRequest{
		Request: request, Response: response, Pins: pins,
		Metadata: shoal.Metadata{"application": "test"},
	}

	first, err := builder.Build(context.Background(), input)
	if err != nil {
		t.Fatal(err)
	}
	second, err := builder.Build(context.Background(), input)
	if err != nil {
		t.Fatal(err)
	}
	if first.ID() != second.ID() {
		t.Fatalf("repeated build IDs differ: %q != %q", first.ID(), second.ID())
	}
	if first.Snapshot().ID() != pins.Snapshot.ID() ||
		first.Authorization().Fingerprint() != pins.Authorization.Fingerprint() {
		t.Fatal("pack lost snapshot or authorization identity")
	}
	if first.Metadata()[metadataRequestKey] != encodeID(response.RequestID) {
		t.Fatal("pack lost retrieval request identity")
	}

	var documentCount, graphCount int
	for _, anchor := range first.Evidence() {
		switch anchor.Kind() {
		case inference.AnchorDocument:
			documentCount++
			citation, quote, ok := anchor.Document()
			if !ok || citation.SpanID == "" || quote == "" {
				t.Fatal("document anchor lost exact citation or quote")
			}
		case inference.AnchorGraph:
			graphCount++
			path, ok := anchor.Path()
			if !ok || len(path.Nodes) == 0 {
				t.Fatal("graph anchor lost exact path")
			}
		default:
			t.Fatalf("unexpected anchor kind %q", anchor.Kind())
		}
	}
	if documentCount == 0 || graphCount == 0 {
		t.Fatalf("document anchors = %d, graph anchors = %d", documentCount, graphCount)
	}

	reversed := cloneResponse(response)
	reverseResults(reversed.Results)
	for index := range reversed.Results {
		reverseEvidence(reversed.Results[index].Evidence)
	}
	reordered, err := builder.Build(context.Background(), InitialRequest{
		Request: request, Response: reversed, Pins: pins,
		Metadata: shoal.Metadata{"application": "test"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if first.ID() != reordered.ID() {
		t.Fatal("input result/evidence order changed canonical pack identity")
	}
	modeOrder := request
	modeOrder.Modes = []retrieval.Mode{
		retrieval.ModeGraph, retrieval.ModeTree, retrieval.ModeLexical,
	}
	reorderedModes, err := builder.Build(context.Background(), InitialRequest{
		Request: modeOrder, Response: response, Pins: pins,
		Metadata: shoal.Metadata{"application": "test"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if first.ID() != reorderedModes.ID() {
		t.Fatal("retrieval mode order changed canonical pack identity")
	}
}

func TestBuildPinsEmbeddingSpaceIdentity(t *testing.T) {
	client, request, response, pins := embeddedFixture(t)
	request.Modes = []retrieval.Mode{retrieval.ModeVector}
	constituent, err := retrieval.EmbeddingSpaceIdentityID("space-v3")
	if err != nil {
		t.Fatal(err)
	}
	response.EmbeddingSpaceID, err = retrieval.EmbeddingSpaceSetID(constituent)
	if err != nil {
		t.Fatal(err)
	}
	response.EmbeddingSpaceIDs = []shoal.ID{constituent}
	pack, err := (Builder{Reader: client}).Build(
		context.Background(),
		InitialRequest{Request: request, Response: response, Pins: pins},
	)
	if err != nil {
		t.Fatal(err)
	}
	identity, ok, err := EmbeddingSpaceID(pack)
	if err != nil {
		t.Fatal(err)
	}
	if !ok || identity != response.EmbeddingSpaceID {
		t.Fatalf("embedding space = %q, %t", identity, ok)
	}
	malformedMetadata := pack.Metadata()
	delete(malformedMetadata, metadataEmbeddingSpacesKey)
	malformed, err := inference.NewContextPack(
		pack.Query(), pack.Evidence(), nil, pack.Snapshot(),
		pack.Authorization(), malformedMetadata,
	)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := EmbeddingSpaceID(malformed); err == nil {
		t.Fatal("aggregate-only context metadata was accepted")
	}
	constituents, err := EmbeddingSpaceIDs(pack)
	if err != nil {
		t.Fatal(err)
	}
	if len(constituents) != 1 || constituents[0] != constituent {
		t.Fatalf("embedding space constituents = %v", constituents)
	}
	pins.EmbeddingSpaceID = "different-space"
	if _, err := (Builder{Reader: client}).Build(
		context.Background(),
		InitialRequest{Request: request, Response: response, Pins: pins},
	); err == nil {
		t.Fatal("mismatched trusted embedding-space pin was accepted")
	}

	pins.EmbeddingSpaceID = response.EmbeddingSpaceID
	pins.EmbeddingSpaceIDs = nil
	if _, err := (Builder{Reader: client}).Build(
		context.Background(),
		InitialRequest{Request: request, Response: response, Pins: pins},
	); err == nil {
		t.Fatal("aggregate-only trusted embedding-space pin was accepted")
	}

	_, lexical, lexicalResponse, lexicalPins := embeddedFixture(t)
	lexicalPins.EmbeddingSpaceID = response.EmbeddingSpaceID
	lexicalPins.EmbeddingSpaceIDs = []shoal.ID{constituent}
	if _, err := (Builder{Reader: client}).Build(
		context.Background(),
		InitialRequest{
			Request: lexical, Response: lexicalResponse, Pins: lexicalPins,
		},
	); err == nil {
		t.Fatal("non-vector retrieval accepted embedding-space pins")
	}
}

func TestMergeEmbeddingSpaceMetadataIsASetUnion(t *testing.T) {
	spaceA, err := retrieval.EmbeddingSpaceIdentityID("space-a")
	if err != nil {
		t.Fatal(err)
	}
	spaceB, err := retrieval.EmbeddingSpaceIdentityID("space-b")
	if err != nil {
		t.Fatal(err)
	}
	metadata, err := MergeEmbeddingSpaceMetadata(nil, []shoal.ID{spaceA})
	if err != nil {
		t.Fatal(err)
	}
	metadata, err = MergeEmbeddingSpaceMetadata(
		metadata, []shoal.ID{spaceB})
	if err != nil {
		t.Fatal(err)
	}
	metadata, err = MergeEmbeddingSpaceMetadata(
		metadata, []shoal.ID{spaceA})
	if err != nil {
		t.Fatal(err)
	}
	ids, err := embeddingSpaceIDsFromMetadata(metadata)
	if err != nil {
		t.Fatal(err)
	}
	if len(ids) != 2 ||
		!((ids[0] == spaceA && ids[1] == spaceB) ||
			(ids[0] == spaceB && ids[1] == spaceA)) {
		t.Fatalf("merged constituent IDs = %v", ids)
	}
	got, ok, err := embeddingSpaceIDFromMetadata(metadata)
	if err != nil {
		t.Fatal(err)
	}
	want, err := retrieval.EmbeddingSpaceSetID(spaceA, spaceB)
	if err != nil {
		t.Fatal(err)
	}
	if !ok || got != want {
		t.Fatalf("merged set ID = %q, %t, want %q", got, ok, want)
	}
	corrupt := cloneMetadata(metadata)
	corrupt[metadataEmbeddingSpaceKey] = encodeID(spaceA)
	if _, err := MergeEmbeddingSpaceMetadata(
		corrupt, []shoal.ID{spaceA},
	); err == nil {
		t.Fatal("mismatched aggregate and constituents were repaired")
	}
	legacy := shoal.Metadata{
		metadataEmbeddingSpaceKey: encodeID(want),
	}
	if _, err := MergeEmbeddingSpaceMetadata(
		legacy, []shoal.ID{spaceA},
	); err == nil {
		t.Fatal("aggregate-only provenance was merged without constituents")
	}
	if _, err := MergeEmbeddingSpaceMetadata(legacy, nil); err == nil {
		t.Fatal("aggregate-only provenance survived an empty merge")
	}
}

func TestBuildDocumentGraphAndMixedSelection(t *testing.T) {
	client, request, response, pins := embeddedFixture(t)
	builder := Builder{Reader: client}
	for name, selection := range map[string]EvidenceSelection{
		"document": {Documents: true},
		"graph":    {Paths: true},
		"mixed":    {},
	} {
		t.Run(name, func(t *testing.T) {
			pack, err := builder.Build(context.Background(), InitialRequest{
				Request: request, Response: response, Selection: selection, Pins: pins,
			})
			if err != nil {
				t.Fatal(err)
			}
			for _, anchor := range pack.Evidence() {
				if name == "document" && anchor.Kind() != inference.AnchorDocument {
					t.Fatal("document-only pack contains graph evidence")
				}
				if name == "graph" && anchor.Kind() != inference.AnchorGraph {
					t.Fatal("graph-only pack contains document evidence")
				}
			}
		})
	}
}

func TestBuildRejectsStaleCitationRangeRevisionAndPath(t *testing.T) {
	client, request, response, pins := embeddedFixture(t)
	builder := Builder{Reader: client}

	staleRevision := cloneResponse(response)
	staleRevision.Results[0].Evidence[0].Citation.RevisionID = "stale-revision"
	_, err := builder.Build(context.Background(), InitialRequest{
		Request: request, Response: staleRevision, Pins: pins,
	})
	assertCode(t, err, shoal.ErrorNotFound)

	badRange := cloneResponse(response)
	evidence := &badRange.Results[0].Evidence[0]
	evidence.Citation.Range.Start.Offset++
	evidence.Citation.Range.End.Offset++
	_, err = builder.Build(context.Background(), InitialRequest{
		Request: request, Response: badRange, Pins: pins,
	})
	assertCode(t, err, shoal.ErrorInvalidArgument)

	_, err = (Builder{Reader: client, Limits: Limits{MaxSpans: 1}}).
		Build(context.Background(), InitialRequest{
			Request: request, Response: response,
			Selection: EvidenceSelection{Documents: true}, Pins: pins,
		})
	assertCode(t, err, shoal.ErrorInvalidArgument)

	_, err = (Builder{Reader: client, Limits: Limits{MaxHydrationBytes: 1}}).
		Build(context.Background(), InitialRequest{
			Request: request, Response: response,
			Selection: EvidenceSelection{Documents: true}, Pins: pins,
		})
	assertCode(t, err, shoal.ErrorInvalidArgument)

	badPath := cloneResponse(response)
	badPath.Results[0].Evidence[0].Path.Nodes[0].Labels =
		append(badPath.Results[0].Evidence[0].Path.Nodes[0].Labels, "stale")
	_, err = builder.Build(context.Background(), InitialRequest{
		Request: request, Response: badPath,
		Selection: EvidenceSelection{Paths: true}, Pins: pins,
	})
	assertCode(t, err, shoal.ErrorInvalidArgument)
}

func TestOpenSectionAndExpandNeighborsAreExplicitBoundedAndImmutable(t *testing.T) {
	client, request, response, pins := embeddedFixture(t)
	builder := Builder{Reader: client}
	initial, err := builder.Build(context.Background(), InitialRequest{
		Request: request, Response: response,
		Selection: EvidenceSelection{Documents: true}, Pins: pins,
	})
	if err != nil {
		t.Fatal(err)
	}
	citation := firstDocumentCitation(t, initial)
	view, err := client.Document(context.Background(), citation.DocumentID, citation.RevisionID)
	if err != nil {
		t.Fatal(err)
	}
	sectionID := firstSectionWithSpans(t, view.Root)
	opened, err := builder.OpenSection(context.Background(), initial, OpenSectionRequest{
		DocumentID: citation.DocumentID,
		RevisionID: citation.RevisionID,
		SectionIDs: []shoal.ID{sectionID},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(opened.Evidence()) < len(initial.Evidence()) {
		t.Fatal("opening a section removed existing evidence")
	}
	if initial.ID() == opened.ID() && len(opened.Evidence()) != len(initial.Evidence()) {
		t.Fatal("expanded immutable pack retained its old identity")
	}

	full, err := builder.Build(context.Background(), InitialRequest{
		Request: request, Response: response, Pins: pins,
	})
	if err != nil {
		t.Fatal(err)
	}
	path := firstGraphPath(t, full)
	expanded, err := builder.ExpandNeighbors(context.Background(), full, ExpandNeighborsRequest{
		NodeIDs: []shoal.ID{path.Nodes[0].ID},
		Depth:   1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(expanded.Evidence()) < len(full.Evidence()) {
		t.Fatal("neighbor expansion removed existing evidence")
	}
	if len(full.Evidence()) == 0 {
		t.Fatal("input pack was mutated")
	}
	if _, err := (Builder{
		Reader: client, Limits: Limits{MaxPathNodes: 2},
	}).ExpandNeighbors(context.Background(), initial, ExpandNeighborsRequest{
		NodeIDs: []shoal.ID{path.Nodes[0].ID},
		Depth:   2,
	}); err != nil {
		t.Fatalf("neighborhood depth was incorrectly treated as path length: %v", err)
	}

	_, err = (Builder{Reader: client, Limits: Limits{MaxHierarchyDepth: 1}}).
		OpenSection(context.Background(), initial, OpenSectionRequest{
			DocumentID: citation.DocumentID, RevisionID: citation.RevisionID,
			SectionIDs: []shoal.ID{sectionID}, Depth: 2,
		})
	assertCode(t, err, shoal.ErrorInvalidArgument)
	_, err = builder.OpenSection(context.Background(), initial, OpenSectionRequest{
		DocumentID: citation.DocumentID, RevisionID: citation.RevisionID,
		SectionIDs: []shoal.ID{sectionID, sectionID},
	})
	assertCode(t, err, shoal.ErrorInvalidArgument)
	_, err = (Builder{
		Reader: client, Limits: Limits{MaxProvenanceBytes: 1},
	}).OpenSection(context.Background(), initial, OpenSectionRequest{
		DocumentID: citation.DocumentID, RevisionID: citation.RevisionID,
		SectionIDs: []shoal.ID{sectionID},
	})
	assertCode(t, err, shoal.ErrorInvalidArgument)
	boundedAnchors := initial.Evidence()
	graphAnchor, err := inference.NewGraphAnchor(path)
	if err != nil {
		t.Fatal(err)
	}
	err = appendAnchor(
		&boundedAnchors, anchorMap(boundedAnchors), graphAnchor, len(boundedAnchors))
	assertCode(t, err, shoal.ErrorInvalidArgument)
	_, err = builder.ExpandNeighbors(context.Background(), full, ExpandNeighborsRequest{
		NodeIDs: []shoal.ID{path.Nodes[0].ID, path.Nodes[0].ID},
	})
	assertCode(t, err, shoal.ErrorInvalidArgument)

	reader := &recordingReader{}
	tooManySections := make([]shoal.ID, DefaultMaxSections+1)
	for index := range tooManySections {
		tooManySections[index] = shoal.ID(fmt.Sprintf("section-%d", index))
	}
	_, err = (Builder{Reader: reader}).OpenSection(
		context.Background(), initial, OpenSectionRequest{
			DocumentID: citation.DocumentID, RevisionID: citation.RevisionID,
			SectionIDs: tooManySections,
		})
	assertCode(t, err, shoal.ErrorInvalidArgument)
	tooManyNodes := make([]shoal.ID, DefaultMaxGraphNodes+1)
	for index := range tooManyNodes {
		tooManyNodes[index] = shoal.ID(fmt.Sprintf("node-%d", index))
	}
	_, err = (Builder{Reader: reader}).ExpandNeighbors(
		context.Background(), full, ExpandNeighborsRequest{NodeIDs: tooManyNodes})
	assertCode(t, err, shoal.ErrorInvalidArgument)
	if reader.documentCalls != 0 || reader.neighborhoodCalls != 0 {
		t.Fatal("oversized explicit expansion reached the hydration seam")
	}
}

func TestContextByteLimitIncludesPinsAndCanonicalFraming(t *testing.T) {
	client, request, response, pins := embeddedFixture(t)
	request.Text = "  exact   context  "
	pack, err := (Builder{Reader: client}).Build(context.Background(), InitialRequest{
		Request: request, Response: response, Pins: pins,
	})
	if err != nil {
		t.Fatal(err)
	}
	if pack.Query() != "exact context" {
		t.Fatalf("normalized query = %q", pack.Query())
	}
	size, _, _, _, err := contextPackByteSize(
		pack.Query(), pack.Evidence(), pins, pack.Metadata(), DefaultMaxPathNodes)
	if err != nil {
		t.Fatal(err)
	}
	if err := enforcePackBounds(
		pack.Query(), pack.Evidence(), pins, pack.Metadata(),
		mustLimits(t, Limits{MaxContextBytes: size}),
	); err != nil {
		t.Fatalf("exact context byte limit rejected: %v", err)
	}
	if _, err := (Builder{
		Reader: client, Limits: Limits{MaxContextBytes: size},
	}).Build(context.Background(), InitialRequest{
		Request: request, Response: response, Pins: pins,
	}); err != nil {
		t.Fatalf("normalized query rejected at exact context byte limit: %v", err)
	}
	if err := enforcePackBounds(
		pack.Query(), pack.Evidence(), pins, pack.Metadata(),
		mustLimits(t, Limits{MaxContextBytes: size - 1}),
	); !shoal.IsErrorCode(err, shoal.ErrorInvalidArgument) {
		t.Fatalf("over-limit context error = %v", err)
	}
}

func TestRetrievalIdentityPreservesOpaqueIDs(t *testing.T) {
	request, err := (retrieval.Request{Text: "query"}).Normalize()
	if err != nil {
		t.Fatal(err)
	}
	left := retrieval.Response{Results: []retrieval.Result{{ID: shoal.ID("\xff")}}}
	right := retrieval.Response{Results: []retrieval.Result{{ID: shoal.ID("\xfe")}}}
	leftIdentity, err := retrievalIdentity(request, left)
	if err != nil {
		t.Fatal(err)
	}
	rightIdentity, err := retrievalIdentity(request, right)
	if err != nil {
		t.Fatal(err)
	}
	if leftIdentity == rightIdentity {
		t.Fatal("opaque result IDs collided in retrieval identity")
	}
}

func TestOpaquePinAndRequestIDsRemainLosslessMetadata(t *testing.T) {
	client, request, response, pins := embeddedFixture(t)
	response.RequestID = shoal.ID("\xff")
	pins.PolicyID = shoal.ID("\xfe")
	pack, err := (Builder{Reader: client}).Build(context.Background(), InitialRequest{
		Request: request, Response: response, Pins: pins,
	})
	if err != nil {
		t.Fatal(err)
	}
	if pack.Metadata()[metadataRequestKey] != "hex:ff" ||
		pack.Metadata()[metadataPolicyKey] != "hex:fe" {
		t.Fatal("opaque IDs were not preserved with an ASCII encoding")
	}
}

func TestFollowUpCanonicalizesGraphLabels(t *testing.T) {
	pins := testPins(t)
	node := graph.Node{ID: "node", Labels: []string{"z", "a"}}
	second := graph.Node{ID: "second"}
	edge := graph.Edge{ID: "edge", From: node.ID, To: second.ID, Type: "related"}
	anchor, err := inference.NewGraphAnchor(graph.Path{
		Nodes: []graph.Node{node, second}, Edges: []graph.Edge{edge},
	})
	if err != nil {
		t.Fatal(err)
	}
	pack, err := inference.NewContextPack(
		"query", []inference.EvidenceAnchor{anchor}, nil,
		pins.Snapshot, pins.Authorization,
		shoal.Metadata{metadataPolicyKey: encodeID(pins.PolicyID)},
	)
	if err != nil {
		t.Fatal(err)
	}
	hydrated := node
	hydrated.Properties = shoal.Metadata{}
	hydratedEdge := edge
	hydratedEdge.Properties = shoal.Metadata{}
	reader := &recordingReader{
		neighborhood: explorer.Neighborhood{
			Nodes: []graph.Node{hydrated, second}, Edges: []graph.Edge{hydratedEdge},
		},
	}
	overbroad := &recordingReader{
		neighborhood: explorer.Neighborhood{
			Nodes: []graph.Node{hydrated, second, {ID: "outside"}},
			Edges: []graph.Edge{hydratedEdge},
		},
	}
	if _, err := (Builder{Reader: overbroad}).ExpandNeighbors(
		context.Background(), pack, ExpandNeighborsRequest{NodeIDs: []shoal.ID{node.ID}},
	); !shoal.IsErrorCode(err, shoal.ErrorInvalidArgument) {
		t.Fatalf("over-broad neighborhood error = %v", err)
	}
	if _, err := (Builder{Reader: reader}).ExpandNeighbors(
		context.Background(), pack, ExpandNeighborsRequest{NodeIDs: []shoal.ID{node.ID}},
	); err != nil {
		t.Fatalf("canonical graph labels rejected during follow-up: %v", err)
	}
}

func TestBoundsFailClosed(t *testing.T) {
	client, request, response, pins := embeddedFixture(t)
	if len(response.Results) < 2 {
		t.Fatal("fixture requires at least two results")
	}
	_, err := (Builder{Reader: client, Limits: Limits{MaxResults: 1}}).
		Build(context.Background(), InitialRequest{
			Request: request, Response: response, Pins: pins,
		})
	assertCode(t, err, shoal.ErrorInvalidArgument)

	_, err = (Builder{Reader: client, Limits: Limits{MaxAnchors: 1}}).
		Build(context.Background(), InitialRequest{
			Request: request, Response: response, Pins: pins,
		})
	assertCode(t, err, shoal.ErrorInvalidArgument)

	_, err = (Builder{Reader: client, Limits: Limits{MaxSections: 1}}).
		Build(context.Background(), InitialRequest{
			Request: request, Response: response,
			Selection: EvidenceSelection{Documents: true}, Pins: pins,
		})
	assertCode(t, err, shoal.ErrorInvalidArgument)

	_, err = (Builder{Reader: client, Limits: Limits{MaxQuoteBytes: 1}}).
		Build(context.Background(), InitialRequest{
			Request: request, Response: response,
			Selection: EvidenceSelection{Documents: true}, Pins: pins,
		})
	assertCode(t, err, shoal.ErrorInvalidArgument)

	_, err = (Builder{Reader: client, Limits: Limits{MaxContextBytes: 32}}).
		Build(context.Background(), InitialRequest{
			Request: request, Response: response, Pins: pins,
		})
	assertCode(t, err, shoal.ErrorInvalidArgument)

	_, err = (Builder{Reader: client, Limits: Limits{MaxGraphNodes: 1}}).
		Build(context.Background(), InitialRequest{
			Request: request, Response: response,
			Selection: EvidenceSelection{Paths: true}, Pins: pins,
		})
	assertCode(t, err, shoal.ErrorInvalidArgument)

	_, err = (Builder{Reader: client, Limits: Limits{MaxProvenanceBytes: 1}}).
		Build(context.Background(), InitialRequest{
			Request: request, Response: response, Pins: pins,
		})
	assertCode(t, err, shoal.ErrorInvalidArgument)

	oversized := cloneResponse(response)
	oversized.Results = oversized.Results[:1]
	evidence := oversized.Results[0].Evidence[0]
	oversized.Results[0].Evidence = make([]retrieval.Evidence, DefaultMaxAnchors+1)
	for index := range oversized.Results[0].Evidence {
		oversized.Results[0].Evidence[index] = evidence
	}
	reader := &recordingReader{}
	_, err = (Builder{Reader: reader}).Build(context.Background(), InitialRequest{
		Request: request, Response: oversized, Pins: pins,
	})
	assertCode(t, err, shoal.ErrorInvalidArgument)
	if reader.documentCalls != 0 || reader.neighborhoodCalls != 0 {
		t.Fatal("oversized evidence reached the hydration seam")
	}

	graphBytes := 0
	for _, result := range response.Results {
		for _, evidence := range result.Evidence {
			if pathPresent(evidence.Path) {
				graphBytes += pathPayloadBytes(evidence.Path)
			}
		}
	}
	reader = &recordingReader{}
	_, err = (Builder{
		Reader: reader, Limits: Limits{MaxHydrationBytes: graphBytes - 1},
	}).Build(context.Background(), InitialRequest{
		Request: request, Response: response, Pins: pins,
	})
	assertCode(t, err, shoal.ErrorInvalidArgument)
	if reader.documentCalls != 0 || reader.neighborhoodCalls != 0 {
		t.Fatal("oversized graph evidence reached the hydration seam")
	}

	oversizedExplanation := cloneResponse(response)
	oversizedExplanation.Results[0].Explanation = &retrieval.Explanation{
		Modes: make([]retrieval.Mode, DefaultMaxProvenanceBytes/8+1),
	}
	for index := range oversizedExplanation.Results[0].Explanation.Modes {
		oversizedExplanation.Results[0].Explanation.Modes[index] = retrieval.ModeLexical
	}
	reader = &recordingReader{}
	_, err = (Builder{Reader: reader}).Build(context.Background(), InitialRequest{
		Request: request, Response: oversizedExplanation, Pins: pins,
	})
	assertCode(t, err, shoal.ErrorInvalidArgument)
	if reader.documentCalls != 0 || reader.neighborhoodCalls != 0 {
		t.Fatal("oversized explanation reached the hydration seam")
	}

	neighborhood := explorer.Neighborhood{
		Nodes: []graph.Node{{
			ID: "node", Properties: shoal.Metadata{"payload": strings.Repeat("x", 32)},
		}},
	}
	neighborhoodBytes, err := neighborhoodPayloadBytes(neighborhood)
	if err != nil {
		t.Fatal(err)
	}
	limits := mustLimits(t, Limits{MaxHydrationBytes: neighborhoodBytes - 1})
	if _, err := newVerifier(
		context.Background(), nil, limits, nil, []explorer.Neighborhood{neighborhood},
	); !shoal.IsErrorCode(err, shoal.ErrorInvalidArgument) {
		t.Fatalf("oversized hydrated graph error = %v", err)
	}
	limits = mustLimits(t, Limits{MaxHydrationBytes: neighborhoodBytes})
	if _, err := newVerifier(
		context.Background(), nil, limits, nil,
		[]explorer.Neighborhood{neighborhood, neighborhood},
	); err != nil {
		t.Fatalf("exact duplicate graph hydration consumed bytes twice: %v", err)
	}
}

func TestAssertionHydrationChargesRetainedReferences(t *testing.T) {
	firstNeighborhood, firstAssertion := assertionNeighborhood(
		t, "assertion-source-a", "assertion-target-a", "assertion-edge-a",
		"assertion-payload-a")
	secondNeighborhood, secondAssertion := assertionNeighborhood(
		t, "assertion-source-b", "assertion-target-b", "assertion-edge-b",
		"assertion-payload-b")
	nodeEdgeBytes := 0
	for _, neighborhood := range []explorer.Neighborhood{
		firstNeighborhood, secondNeighborhood,
	} {
		for _, node := range neighborhood.Nodes {
			nodeEdgeBytes += nodePayloadBytes(node)
		}
		for _, edge := range neighborhood.Edges {
			nodeEdgeBytes += edgePayloadBytes(edge)
		}
	}
	assertionBytes :=
		assertionHydrationPayloadBytes(assertionHydration{
			assertion: firstAssertion,
			edgeIDs:   []shoal.ID{"assertion-edge-a"},
		}) +
			assertionHydrationPayloadBytes(assertionHydration{
				assertion: secondAssertion,
				edgeIDs:   []shoal.ID{"assertion-edge-b"},
			})
	limits := mustLimits(t, Limits{
		MaxHydrationBytes: nodeEdgeBytes + assertionBytes - 1,
	})
	if _, err := newVerifier(
		context.Background(), nil, limits, nil,
		[]explorer.Neighborhood{firstNeighborhood, secondNeighborhood},
	); !shoal.IsErrorCode(err, shoal.ErrorInvalidArgument) {
		t.Fatalf("oversized assertion hydration error = %v", err)
	}

	limits = mustLimits(t, Limits{
		MaxHydrationBytes: nodeEdgeBytes + assertionBytes,
	})
	verifier, err := newVerifier(
		context.Background(), nil, limits, nil,
		[]explorer.Neighborhood{
			firstNeighborhood,
			secondNeighborhood,
			firstNeighborhood,
			secondNeighborhood,
		},
	)
	if err != nil {
		t.Fatalf("deduplicated assertion hydration error = %v", err)
	}
	if verifier.bytes != nodeEdgeBytes+assertionBytes {
		t.Fatalf(
			"hydration bytes = %d, want %d",
			verifier.bytes, nodeEdgeBytes+assertionBytes,
		)
	}
}

func TestAssertionHydrationDeduplicatesExactReferences(t *testing.T) {
	neighborhood, assertion := assertionNeighborhood(
		t, "assertion-source", "assertion-target", "assertion-edge",
		"assertion-payload")
	neighborhood.Assertions = append(neighborhood.Assertions, assertion)
	verifier, err := newVerifier(
		context.Background(), nil, mustLimits(t, Limits{}), nil,
		[]explorer.Neighborhood{neighborhood},
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(verifier.assertionStates) != 1 ||
		len(verifier.assertions["assertion-edge"]) != 1 {
		t.Fatalf(
			"deduplicated assertion state = %+v / %+v",
			verifier.assertionStates, verifier.assertions,
		)
	}
}

func assertionNeighborhood(
	t *testing.T,
	source, target, edgeID shoal.ID,
	payload string,
) (explorer.Neighborhood, ontology.Assertion) {
	t.Helper()
	evidence, err := ontology.NewEvidenceRef(document.Citation{
		DocumentID: "document-1",
		RevisionID: "revision-1",
		SectionID:  "section-1",
		SpanID:     "span-1",
		Range: document.SourceRange{
			Start: document.SourcePosition{Offset: 0, Page: 1},
			End:   document.SourcePosition{Offset: 5, Page: 1},
		},
	}, payload, nil)
	if err != nil {
		t.Fatal(err)
	}
	provenance, err := ontology.NewExtractionProvenance(
		"provider", "model", "v1", "prompt", "v1", "extractor", "v1", nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	concept, err := ontology.NewConceptDefinition(
		"assertion-node", "Assertion Node", "A test assertion node", nil, nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	relationship, err := ontology.NewRelationshipDefinition(
		"supports", "Supports", "Supports another graph node",
		[]shoal.ID{concept.ID()}, []shoal.ID{concept.ID()}, nil, true, nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	value, err := ontology.NewReferenceValue(target)
	if err != nil {
		t.Fatal(err)
	}
	assertion, err := ontology.NewAssertion(
		source, relationship.ID(), value, ontology.AssertionExplicit, 1,
		[]ontology.EvidenceRef{evidence}, provenance,
		shoal.Metadata{
			"payload":             payload,
			"shoal.graph.edge_id": string(edgeID),
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	return explorer.Neighborhood{
		Nodes: []graph.Node{{ID: source}, {ID: target}},
		Edges: []graph.Edge{{
			ID: edgeID, From: source, To: target,
			Type: string(relationship.ID()), Weight: 1,
		}},
		Assertions: []ontology.Assertion{assertion},
	}, assertion
}

func TestAssertionHydrationEnforcesCumulativeCount(t *testing.T) {
	first, firstAssertion := assertionNeighborhood(
		t, "shared-source", "shared-target", "shared-edge", "first")
	second, secondAssertion := assertionNeighborhood(
		t, "shared-source", "shared-target", "shared-edge", "second")
	if firstAssertion.ID() == secondAssertion.ID() {
		t.Fatal("fixture assertions unexpectedly share an identity")
	}
	limits := mustLimits(t, Limits{
		MaxGraphNodes: 4, MaxGraphEdges: 1,
	})
	verifier, err := newVerifier(
		context.Background(), nil, limits, nil,
		[]explorer.Neighborhood{first},
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := verifier.addNeighborhood(second); !shoal.IsErrorCode(
		err, shoal.ErrorInvalidArgument,
	) {
		t.Fatalf("cumulative assertion count error = %v", err)
	}
}

func TestAssertionHydrationChargesStringObjectPayload(t *testing.T) {
	derivation, err := ontology.NewAssertionDerivation(
		"embedding-model", "v1", "cosine", 0.8, "cell-1", 0.9,
		"source", "target", "iterator", nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	evidence, err := ontology.NewDerivationEvidenceRef(derivation, nil)
	if err != nil {
		t.Fatal(err)
	}
	provenance, err := ontology.NewExtractionProvenance(
		"provider", "model", "v1", "prompt", "v1", "extractor", "v1", nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	value, err := ontology.NewStringValue(strings.Repeat("x", 1024))
	if err != nil {
		t.Fatal(err)
	}
	property, err := ontology.NewPropertyDefinition(
		"string-property", "String Property", "A string assertion property",
		ontology.ValueString, nil, nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	assertion, err := ontology.NewAssertion(
		"source", property.ID(), value, ontology.AssertionDerived, 0.9,
		[]ontology.EvidenceRef{evidence}, provenance, nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	emptyValue, err := ontology.NewStringValue("")
	if err != nil {
		t.Fatal(err)
	}
	emptyAssertion, err := ontology.NewAssertion(
		"source", property.ID(), emptyValue, ontology.AssertionDerived, 0.9,
		[]ontology.EvidenceRef{evidence}, provenance, nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	if got := assertionPayloadBytes(assertion) -
		assertionPayloadBytes(emptyAssertion); got != 1024 {
		t.Fatalf("string object payload bytes = %d, want 1024", got)
	}
	nodes := []graph.Node{{ID: "producer"}, {ID: assertion.ID()}}
	edge := graph.Edge{
		ID: "produced-string", From: "producer", To: assertion.ID(),
		Type: graph.EdgeTypeProduced, Weight: 1,
		Properties: shoal.Metadata{
			"ontology.assertion.id": string(assertion.ID()),
		},
	}
	neighborhood := explorer.Neighborhood{
		Nodes: nodes, Edges: []graph.Edge{edge},
		Assertions: []ontology.Assertion{assertion},
	}
	requiredBytes := nodePayloadBytes(nodes[0]) + nodePayloadBytes(nodes[1]) +
		edgePayloadBytes(edge) +
		assertionHydrationPayloadBytes(assertionHydration{
			assertion: assertion, edgeIDs: []shoal.ID{edge.ID},
		})
	if _, err := newVerifier(
		context.Background(), nil,
		mustLimits(t, Limits{MaxHydrationBytes: requiredBytes - 1}),
		nil, []explorer.Neighborhood{neighborhood},
	); !shoal.IsErrorCode(err, shoal.ErrorInvalidArgument) {
		t.Fatalf("under-budget string assertion error = %v", err)
	}
	if _, err := newVerifier(
		context.Background(), nil,
		mustLimits(t, Limits{MaxHydrationBytes: requiredBytes}),
		nil, []explorer.Neighborhood{neighborhood},
	); err != nil {
		t.Fatalf("exact-budget string assertion error = %v", err)
	}
}

func TestMutationIsolationAndNoUncitedExplanationText(t *testing.T) {
	client, request, response, pins := embeddedFixture(t)
	const uncited = "this explanation is not evidence"
	response.Results[0].Explanation.Summary = uncited
	builder := Builder{Reader: client}
	pack, err := builder.Build(context.Background(), InitialRequest{
		Request: request, Response: response, Pins: pins,
	})
	if err != nil {
		t.Fatal(err)
	}

	beforeID := pack.ID()
	response.Results[0].Evidence[0].Quote = "mutated"
	response.Results[0].Evidence[0].Path.Nodes[0].Labels = []string{"mutated"}
	response.Results[0].Explanation.Summary = "mutated"
	if pack.ID() != beforeID {
		t.Fatal("caller mutation changed pack identity")
	}
	for key, value := range pack.Metadata() {
		if strings.Contains(key, uncited) || strings.Contains(value, uncited) {
			t.Fatal("uncited explanation prose entered context metadata")
		}
	}
	for _, anchor := range pack.Evidence() {
		if _, quote, ok := anchor.Document(); ok && strings.Contains(quote, uncited) {
			t.Fatal("uncited explanation prose entered evidence")
		}
	}
}

func TestGraphAnchorRejectsAuthoritativeDerivedAssertion(t *testing.T) {
	value, err := ontology.NewReferenceValue("target")
	if err != nil {
		t.Fatal(err)
	}
	derivation, err := ontology.NewAssertionDerivation(
		"embedding-model", "v1", "cosine", 0.8, "cell-1", 0.9,
		"source", "target", "iterator", nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	evidence, err := ontology.NewDerivationEvidenceRef(derivation, nil)
	if err != nil {
		t.Fatal(err)
	}
	provenance, err := ontology.NewExtractionProvenance(
		"provider", "model", "v1", "prompt", "v1", "extractor", "v1", nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	concept, err := ontology.NewConceptDefinition(
		"graph-node", "Graph Node", "A graph node", nil, nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	relationship, err := ontology.NewRelationshipDefinition(
		"related-to", "Related To", "Relates two graph nodes",
		[]shoal.ID{concept.ID()}, []shoal.ID{concept.ID()}, nil, true, nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	predicate := relationship.ID()
	assertion, err := ontology.NewAssertion(
		"source", predicate, value, ontology.AssertionDerived, 0.9,
		[]ontology.EvidenceRef{evidence}, provenance, nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	path := graph.Path{
		Nodes: []graph.Node{{ID: "source"}, {ID: "target"}},
		Edges: []graph.Edge{{
			ID: assertion.ID(), From: "source", To: "target",
			Type: string(predicate), Weight: 0.9,
		}},
	}
	verifier, err := newVerifier(
		context.Background(), nil, mustLimits(t, Limits{}), nil,
		[]explorer.Neighborhood{{
			Nodes: path.Nodes, Edges: path.Edges,
			Assertions: []ontology.Assertion{assertion},
		}},
	)
	if err != nil {
		t.Fatal(err)
	}
	producedEdge := graph.Edge{
		ID:     "produced-edge",
		From:   "producer",
		To:     assertion.ID(),
		Type:   graph.EdgeTypeProduced,
		Weight: 1,
		Properties: shoal.Metadata{
			"ontology.assertion.id": string(assertion.ID()),
		},
	}
	producedNodes := []graph.Node{{ID: "producer"}, {ID: assertion.ID()}}
	requiredBytes := verifier.bytes +
		nodePayloadBytes(producedNodes[0]) +
		nodePayloadBytes(producedNodes[1]) +
		edgePayloadBytes(producedEdge) +
		assertionReferencePayloadBytes(
			assertion.ID(), producedEdge.ID, assertion.Origin())
	verifier.limits.MaxHydrationBytes = requiredBytes - 1
	if err := verifier.addNeighborhood(explorer.Neighborhood{
		Nodes:      producedNodes,
		Edges:      []graph.Edge{producedEdge},
		Assertions: []ontology.Assertion{assertion},
	}); !shoal.IsErrorCode(err, shoal.ErrorInvalidArgument) {
		t.Fatalf("uncharged page-local assertion edge = %v", err)
	}
	verifier.limits.MaxHydrationBytes = requiredBytes
	if err := verifier.addNeighborhood(explorer.Neighborhood{
		Nodes:      producedNodes,
		Edges:      []graph.Edge{producedEdge},
		Assertions: []ontology.Assertion{assertion},
	}); err != nil {
		t.Fatalf("page-local assertion edge hydration = %v", err)
	}
	state := verifier.assertionStates[assertion.ID()]
	if len(state.edgeIDs) != 2 ||
		state.edgeIDs[0] != assertion.ID() ||
		state.edgeIDs[1] != producedEdge.ID {
		t.Fatalf("accumulated assertion edges = %v", state.edgeIDs)
	}
	if _, err := verifier.graphAnchor(path); !shoal.IsErrorCode(
		err, shoal.ErrorInvalidArgument,
	) {
		t.Fatalf("derived assertion graph anchor error = %v", err)
	}
}

func TestGraphAnchorCarriesAuthoritativeExplicitAssertion(t *testing.T) {
	testGraphAnchorCarriesAuthoritativeAssertion(t, ontology.AssertionExplicit)
}

func TestGraphAnchorCarriesAuthoritativeInferredAssertion(t *testing.T) {
	testGraphAnchorCarriesAuthoritativeAssertion(t, ontology.AssertionInferred)
}

func testGraphAnchorCarriesAuthoritativeAssertion(t *testing.T, origin ontology.AssertionOrigin) {
	t.Helper()
	_, _, response, _ := embeddedFixture(t)
	citation := response.Results[0].Evidence[0].Citation
	quote := response.Results[0].Evidence[0].Quote
	evidence, err := ontology.NewEvidenceRef(citation, quote, nil)
	if err != nil {
		t.Fatal(err)
	}
	provenance, err := ontology.NewExtractionProvenance(
		"provider", "model", "v1", "prompt", "v1", "extractor", "v1", nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	concept, err := ontology.NewConceptDefinition(
		"source-node", "Source Node", "A source graph node", nil, nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	relationship, err := ontology.NewRelationshipDefinition(
		"supports", "Supports", "Supports another graph node",
		[]shoal.ID{concept.ID()}, []shoal.ID{concept.ID()}, nil, true, nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	value, err := ontology.NewReferenceValue("target")
	if err != nil {
		t.Fatal(err)
	}
	assertion, err := ontology.NewAssertion(
		"source", relationship.ID(), value, origin, 1,
		[]ontology.EvidenceRef{evidence}, provenance,
		shoal.Metadata{"shoal.graph.edge_id": "edge-1"},
	)
	if err != nil {
		t.Fatal(err)
	}
	path := graph.Path{
		Nodes: []graph.Node{{ID: "source"}, {ID: "target"}},
		Edges: []graph.Edge{{
			ID: "edge-1", From: "source", To: "target",
			Type: string(relationship.ID()), Weight: 1,
		}},
	}
	verifier, err := newVerifier(
		context.Background(), nil, mustLimits(t, Limits{}), nil,
		[]explorer.Neighborhood{{
			Nodes: path.Nodes, Edges: path.Edges,
			Assertions: []ontology.Assertion{assertion},
		}},
	)
	if err != nil {
		t.Fatal(err)
	}
	anchor, err := verifier.graphAnchor(path)
	if err != nil {
		t.Fatal(err)
	}
	reference, err := anchor.EvidenceReference()
	if err != nil {
		t.Fatal(err)
	}
	if len(reference.Assertions) != 1 ||
		reference.Assertions[0].AssertionID != assertion.ID() ||
		reference.Assertions[0].EdgeID != "edge-1" ||
		reference.Assertions[0].Origin != origin {
		t.Fatalf("authoritative assertion reference = %+v", reference.Assertions)
	}
	changed, err := ontology.NewAssertion(
		"source", relationship.ID(), value, origin, 1,
		[]ontology.EvidenceRef{evidence}, provenance,
		shoal.Metadata{
			"shoal.graph.edge_id": "edge-1",
			"annotation":          "changed without changing assertion identity",
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if changed.ID() != assertion.ID() {
		t.Fatal("fixture metadata unexpectedly changed the assertion identity")
	}
	if err := verifier.addNeighborhood(explorer.Neighborhood{
		Nodes: path.Nodes, Edges: path.Edges,
		Assertions: []ontology.Assertion{changed},
	}); !shoal.IsErrorCode(err, shoal.ErrorInvalidArgument) {
		t.Fatalf("changed assertion metadata across hydrations = %v", err)
	}
	remapped, err := ontology.NewAssertion(
		"source", relationship.ID(), value, origin, 1,
		[]ontology.EvidenceRef{evidence}, provenance,
		shoal.Metadata{"shoal.graph.edge_id": "edge-2"},
	)
	if err != nil {
		t.Fatal(err)
	}
	remappedEdge := path.Edges[0]
	remappedEdge.ID = "edge-2"
	if err := verifier.addNeighborhood(explorer.Neighborhood{
		Nodes: path.Nodes, Edges: []graph.Edge{remappedEdge},
		Assertions: []ontology.Assertion{remapped},
	}); !shoal.IsErrorCode(err, shoal.ErrorInvalidArgument) {
		t.Fatalf("changed assertion edge mapping across hydrations = %v", err)
	}
	if _, err := newVerifier(
		context.Background(), nil, mustLimits(t, Limits{}), nil,
		[]explorer.Neighborhood{{
			Nodes: path.Nodes, Edges: path.Edges,
			Assertions: []ontology.Assertion{assertion, changed},
		}},
	); !shoal.IsErrorCode(err, shoal.ErrorInvalidArgument) {
		t.Fatalf("duplicate assertion identity in one hydration = %v", err)
	}
}

func TestCancellationAndAuthorizedNotFoundShape(t *testing.T) {
	_, request, response, pins := embeddedFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	reader := &recordingReader{}
	_, err := (Builder{Reader: reader}).Build(ctx, InitialRequest{
		Request: request, Response: response, Pins: pins,
	})
	assertCode(t, err, shoal.ErrorUnavailable)
	if reader.documentCalls != 0 || reader.neighborhoodCalls != 0 {
		t.Fatal("canceled construction called the Explorer seam")
	}

	notFound := shoal.NewError(shoal.ErrorNotFound, "document revision not found")
	for _, documentID := range []shoal.ID{"hidden", "absent"} {
		reader := &recordingReader{documentErr: notFound}
		modified := cloneResponse(response)
		modified.Results[0].Evidence[0].Citation.DocumentID = documentID
		_, err := (Builder{Reader: reader}).Build(context.Background(), InitialRequest{
			Request: request, Response: modified,
			Selection: EvidenceSelection{Documents: true}, Pins: pins,
		})
		if err == nil || err.Error() !=
			fmt.Sprintf("result %q document evidence: %v", modified.Results[0].ID, notFound) {
			t.Fatalf("%s error shape = %v", documentID, err)
		}
	}
}

func TestHydratedDuplicatesRequireExactContentAndRequestedIdentity(t *testing.T) {
	client, request, response, pins := embeddedFixture(t)
	citation := response.Results[0].Evidence[0].Citation
	view, err := client.Document(context.Background(), citation.DocumentID, citation.RevisionID)
	if err != nil {
		t.Fatal(err)
	}
	conflict := cloneView(view)
	conflict.Document.Title += " changed"
	_, err = (Builder{Reader: client}).Build(context.Background(), InitialRequest{
		Request: request, Response: response, Pins: pins,
		Documents: []explorer.DocumentView{view, conflict},
	})
	assertCode(t, err, shoal.ErrorInvalidArgument)

	nilMetadata := cloneView(view)
	nilMetadata.Document.Metadata = nil
	emptyMetadata := cloneView(view)
	emptyMetadata.Document.Metadata = shoal.Metadata{}
	limits := mustLimits(t, Limits{})
	if _, err := newVerifier(
		context.Background(), nil, limits,
		[]explorer.DocumentView{nilMetadata, emptyMetadata}, nil,
	); err != nil {
		t.Fatalf("canonical empty metadata was treated as conflicting: %v", err)
	}
	sectionCount, _, _ := sectionViewStats(view)
	exactLimits := mustLimits(t, Limits{MaxSections: sectionCount})
	if _, err := newVerifier(
		context.Background(), nil, exactLimits,
		[]explorer.DocumentView{view, cloneView(view)}, nil,
	); err != nil {
		t.Fatalf("exact duplicate exceeded remaining section budget: %v", err)
	}
	localTime := cloneView(view)
	localTime.Revision.CreatedAt = localTime.Revision.CreatedAt.In(
		time.FixedZone("offset", 2*60*60))
	if _, err := newVerifier(
		context.Background(), nil, limits,
		[]explorer.DocumentView{view, localTime}, nil,
	); err != nil {
		t.Fatalf("equivalent revision timestamps were treated as conflicting: %v", err)
	}
	negativeZero := shoal.Score(math.Copysign(0, -1))
	positiveZero := shoal.Score(0)
	nodes := []graph.Node{{ID: "left"}, {ID: "right"}}
	leftEdge := graph.Edge{
		ID: "edge", From: "left", To: "right", Type: "related", Weight: negativeZero,
	}
	rightEdge := leftEdge
	rightEdge.Weight = positiveZero
	if _, err := newVerifier(
		context.Background(), nil, limits, nil,
		[]explorer.Neighborhood{
			{Nodes: nodes, Edges: []graph.Edge{leftEdge}},
			{Nodes: nodes, Edges: []graph.Edge{rightEdge}},
		},
	); !shoal.IsErrorCode(err, shoal.ErrorInvalidArgument) {
		t.Fatalf("distinct signed-zero edge weights error = %v", err)
	}

	opaqueLeft := cloneView(view)
	opaqueLeft.Document.Metadata = shoal.Metadata{"opaque": "\xff"}
	opaqueRight := cloneView(view)
	opaqueRight.Document.Metadata = shoal.Metadata{"opaque": "\xfe"}
	_, err = (Builder{Reader: client}).Build(context.Background(), InitialRequest{
		Request: request, Response: response, Pins: pins,
		Documents: []explorer.DocumentView{opaqueLeft, opaqueRight},
	})
	assertCode(t, err, shoal.ErrorInvalidArgument)

	mismatched := cloneView(view)
	mismatched.Document.ID = "different-document"
	reader := &recordingReader{documentView: mismatched}
	_, err = (Builder{Reader: reader}).Build(context.Background(), InitialRequest{
		Request: request, Response: response,
		Selection: EvidenceSelection{Documents: true}, Pins: pins,
	})
	assertCode(t, err, shoal.ErrorInvalidArgument)
}

func TestVerifyGraphRejectsDigestOnlyEdgeVisibility(t *testing.T) {
	nodes := []graph.Node{{ID: "left"}, {ID: "right"}}
	edge := graph.Edge{
		ID: "edge", From: "left", To: "right", Type: "related", Weight: 1,
		Properties: shoal.Metadata{
			interaction.PropertyVisibilityDigest: interaction.Digest("secret"),
			interaction.PropertyVisibilityCount:  "1",
		},
	}
	path := graph.Path{Nodes: nodes, Edges: []graph.Edge{edge}}
	anchor, err := inference.NewGraphAnchor(path)
	if err != nil {
		t.Fatal(err)
	}
	limits := mustLimits(t, Limits{})
	verifier, err := newVerifier(
		context.Background(), nil, limits, nil,
		[]explorer.Neighborhood{{Nodes: nodes, Edges: []graph.Edge{edge}}},
	)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := verifier.verifyAnchor(anchor, false); !shoal.IsErrorCode(err, shoal.ErrorUnavailable) {
		t.Fatalf("digest-only edge visibility error = %v", err)
	}
}

func TestOpenSectionSkipsValidEmptySpans(t *testing.T) {
	client, request, response, pins := embeddedFixture(t)
	response.Results = response.Results[:1]
	initial, err := (Builder{Reader: client}).Build(context.Background(), InitialRequest{
		Request: request, Response: response,
		Selection: EvidenceSelection{Documents: true}, Pins: pins,
	})
	if err != nil {
		t.Fatal(err)
	}
	citation := firstDocumentCitation(t, initial)
	view, err := client.Document(context.Background(), citation.DocumentID, citation.RevisionID)
	if err != nil {
		t.Fatal(err)
	}
	sectionID := firstSectionWithSpans(t, view.Root)
	if !appendEmptySpan(&view.Root, sectionID) {
		t.Fatal("section was not found")
	}
	reader := &recordingReader{documentView: view}
	if _, err := (Builder{Reader: reader}).OpenSection(
		context.Background(), initial, OpenSectionRequest{
			DocumentID: citation.DocumentID,
			RevisionID: citation.RevisionID,
			SectionIDs: []shoal.ID{sectionID},
		},
	); err != nil {
		t.Fatalf("valid empty span rejected section expansion: %v", err)
	}
}

func TestTokenBudgetAppliesToInitialAndFollowUpPacks(t *testing.T) {
	client, request, response, pins := embeddedFixture(t)
	limited := Builder{
		Reader: client, Limits: Limits{MaxContextTokens: 1},
		TokenEstimator: fixedTokenEstimator(2),
	}
	_, err := limited.Build(context.Background(), InitialRequest{
		Request: request, Response: response, Pins: pins,
	})
	assertCode(t, err, shoal.ErrorInvalidArgument)

	initial, err := (Builder{Reader: client}).Build(context.Background(), InitialRequest{
		Request: request, Response: response, Pins: pins,
	})
	if err != nil {
		t.Fatal(err)
	}
	citation := firstDocumentCitation(t, initial)
	view, err := client.Document(context.Background(), citation.DocumentID, citation.RevisionID)
	if err != nil {
		t.Fatal(err)
	}
	_, err = limited.OpenSection(context.Background(), initial, OpenSectionRequest{
		DocumentID: citation.DocumentID,
		RevisionID: citation.RevisionID,
		SectionIDs: []shoal.ID{firstSectionWithSpans(t, view.Root)},
	})
	assertCode(t, err, shoal.ErrorInvalidArgument)

	_, err = (Builder{
		Reader: client, Limits: Limits{MaxContextTokens: 1},
	}).Build(context.Background(), InitialRequest{
		Request: request, Response: response, Pins: pins,
	})
	assertCode(t, err, shoal.ErrorInvalidArgument)
}

type fixedTokenEstimator int

func (e fixedTokenEstimator) EstimateTokens(
	context.Context, inference.ContextPack,
) (int, error) {
	return int(e), nil
}

type recordingReader struct {
	documentView      explorer.DocumentView
	documentErr       error
	neighborhood      explorer.Neighborhood
	neighborhoodErr   error
	documentCalls     int
	neighborhoodCalls int
}

func (r *recordingReader) Document(
	context.Context, shoal.ID, shoal.ID,
) (explorer.DocumentView, error) {
	r.documentCalls++
	return cloneView(r.documentView), r.documentErr
}

func (r *recordingReader) Neighborhood(
	context.Context, explorer.NeighborhoodRequest,
) (explorer.Neighborhood, error) {
	r.neighborhoodCalls++
	return r.neighborhood, r.neighborhoodErr
}

func embeddedFixture(
	t *testing.T,
) (*explorer.Explorer, retrieval.Request, retrieval.Response, Pins) {
	t.Helper()
	path := filepath.Join(
		"testdata",
		fmt.Sprintf("contextpack-%d-%d", os.Getpid(), testDirectorySequence.Add(1)),
	)
	if err := os.RemoveAll(path); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	client, err := explorer.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := client.Close(); err != nil {
			t.Errorf("close Explorer: %v", err)
		}
		if err := os.RemoveAll(path); err != nil {
			t.Errorf("remove Explorer fixture: %v", err)
		}
	})
	for index, source := range []explorer.Source{
		{
			URI: "memory://alpha", Title: "Alpha", MediaType: explorer.MediaTypeMarkdown,
			Content: "# Alpha\n\nGrounded context uses exact citations.\n\n## Details\n\nGraph paths stay exact.\n",
		},
		{
			URI: "memory://beta", Title: "Beta", MediaType: explorer.MediaTypeMarkdown,
			Content: "# Beta\n\nExact context is deterministic and bounded.\n",
		},
	} {
		if _, err := client.IngestWithOptions(context.Background(), source, explorer.IngestOptions{
			CreatedAt: time.Date(2026, 8, 28, 10, index, 0, 0, time.UTC),
		}); err != nil {
			t.Fatal(err)
		}
	}
	request, err := (retrieval.Request{
		Text:    "exact context",
		TopK:    10,
		Modes:   []retrieval.Mode{retrieval.ModeLexical, retrieval.ModeTree, retrieval.ModeGraph},
		Explain: true,
	}).Normalize()
	if err != nil {
		t.Fatal(err)
	}
	response, err := client.Retrieve(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	if len(response.Results) < 2 {
		t.Fatalf("retrieval returned %d results", len(response.Results))
	}
	pins := testPins(t)
	return client, request, response, pins
}

func testPins(t *testing.T) Pins {
	t.Helper()
	snapshot, err := inference.NewSnapshotPin(
		"snapshot:test", time.Date(2026, 8, 28, 10, 5, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	auth, err := inference.NewAuthPin(
		"auth:test", time.Date(2026, 8, 28, 11, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	return Pins{
		Snapshot: snapshot, Authorization: auth, PolicyID: "policy:test",
	}
}

func firstSectionWithSpans(t *testing.T, root explorer.SectionView) shoal.ID {
	t.Helper()
	if len(root.Spans) > 0 {
		return root.Section.ID
	}
	for _, child := range root.Children {
		if id := firstSectionWithSpans(t, child); id != "" {
			return id
		}
	}
	t.Fatal("document has no section with spans")
	return ""
}

func appendEmptySpan(view *explorer.SectionView, sectionID shoal.ID) bool {
	if view.Section.ID == sectionID {
		offset := view.Section.Range.Start.Offset
		view.Spans = append(view.Spans, document.Span{
			ID: "empty-span", DocumentID: view.Section.DocumentID,
			RevisionID: view.Section.RevisionID, SectionID: sectionID,
			Order: ^uint32(0),
			Range: document.SourceRange{
				Start: document.SourcePosition{Offset: offset},
				End:   document.SourcePosition{Offset: offset},
			},
		})
		return true
	}
	for index := range view.Children {
		if appendEmptySpan(&view.Children[index], sectionID) {
			return true
		}
	}
	return false
}

func firstGraphPath(t *testing.T, pack inference.ContextPack) graph.Path {
	t.Helper()
	for _, anchor := range pack.Evidence() {
		if path, ok := anchor.Path(); ok {
			return path
		}
	}
	t.Fatal("pack has no graph path")
	return graph.Path{}
}

func firstDocumentCitation(t *testing.T, pack inference.ContextPack) document.Citation {
	t.Helper()
	for _, anchor := range pack.Evidence() {
		if citation, _, ok := anchor.Document(); ok {
			return citation
		}
	}
	t.Fatal("pack has no document citation")
	return document.Citation{}
}

func reverseResults(values []retrieval.Result) {
	for left, right := 0, len(values)-1; left < right; left, right = left+1, right-1 {
		values[left], values[right] = values[right], values[left]
	}
}

func reverseEvidence(values []retrieval.Evidence) {
	for left, right := 0, len(values)-1; left < right; left, right = left+1, right-1 {
		values[left], values[right] = values[right], values[left]
	}
}

func assertCode(t *testing.T, err error, code shoal.ErrorCode) {
	t.Helper()
	if !shoal.IsErrorCode(err, code) {
		t.Fatalf("error = %v, want code %q", err, code)
	}
}

func mustLimits(t *testing.T, limits Limits) Limits {
	t.Helper()
	normalized, err := normalizeLimits(limits)
	if err != nil {
		t.Fatal(err)
	}
	return normalized
}
