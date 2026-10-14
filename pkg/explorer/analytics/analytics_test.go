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

package analytics

import (
	"context"
	"errors"
	"fmt"
	"math"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/phrocker/shoal-oss/pkg/document"
	"github.com/phrocker/shoal-oss/pkg/explorer"
	"github.com/phrocker/shoal-oss/pkg/explorer/auth"
	"github.com/phrocker/shoal-oss/pkg/graph"
	"github.com/phrocker/shoal-oss/pkg/ontology"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

func TestAnalyzeEmptyGraph(t *testing.T) {
	got, err := Analyze(context.Background(), explorer.Neighborhood{}, PageRankOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Nodes) != 0 || len(got.WeaklyConnectedComponents) != 0 ||
		!got.PageRank.Converged || got.PageRank.Iterations != 0 {
		t.Fatalf("empty analysis = %#v", got)
	}
}

func TestAnalyticsResultIDIncludesPageRankContract(t *testing.T) {
	base := Result{
		Scope: ScopeMetadata{
			SnapshotID: "snapshot", Complete: true,
		},
		Nodes: []NodeSummary{{
			NodeID: "node", Degree: 0, PageRank: 1,
		}},
		PageRank: PageRankSummary{
			DampingFactor:        0.85,
			ConvergenceTolerance: 1e-9,
			MaxIterations:        100,
			Iterations:           10,
			Converged:            true,
		},
	}
	baseID := analyticsResultID(base)
	mutations := []struct {
		name   string
		mutate func(*Result)
	}{
		{"damping", func(result *Result) { result.PageRank.DampingFactor = 0.9 }},
		{"tolerance", func(result *Result) { result.PageRank.ConvergenceTolerance = 1e-6 }},
		{"maximum iterations", func(result *Result) { result.PageRank.MaxIterations = 101 }},
		{"actual iterations", func(result *Result) { result.PageRank.Iterations = 11 }},
		{"convergence", func(result *Result) { result.PageRank.Converged = false }},
	}
	for _, test := range mutations {
		t.Run(test.name, func(t *testing.T) {
			changed := base
			test.mutate(&changed)
			if got := analyticsResultID(changed); got == baseID {
				t.Fatalf("result ID did not change for %s", test.name)
			}
		})
	}
}

func TestAuthorizedScopeSnapshotBindsAssertionAnnotationMetadata(t *testing.T) {
	value, err := ontology.NewStringValue("value")
	if err != nil {
		t.Fatal(err)
	}
	provenance, err := ontology.NewExtractionProvenance(
		"provider", "model", "1", "prompt", "1", "extractor", "1", nil)
	if err != nil {
		t.Fatal(err)
	}
	property, err := ontology.NewPropertyDefinition(
		"value", "Value", "", ontology.ValueString, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	evidence, err := ontology.NewEvidenceRef(document.Citation{
		DocumentID: "document", RevisionID: "revision",
		SectionID: "section", SpanID: "span",
		Range: document.SourceRange{
			Start: document.SourcePosition{Offset: 0, Page: 1},
			End:   document.SourcePosition{Offset: 5, Page: 1},
		},
	}, "value", nil)
	if err != nil {
		t.Fatal(err)
	}
	makeAssertion := func(metadata shoal.Metadata) ontology.Assertion {
		assertion, err := ontology.NewAssertion(
			"subject", property.ID(), value, ontology.AssertionExplicit, 1,
			[]ontology.EvidenceRef{evidence}, provenance, metadata)
		if err != nil {
			t.Fatal(err)
		}
		return assertion
	}
	first := makeAssertion(shoal.Metadata{"annotation": "first"})
	second := makeAssertion(shoal.Metadata{"annotation": "second"})
	if first.ID() != second.ID() {
		t.Fatal("test requires annotation metadata outside assertion identity")
	}
	var fingerprint [32]byte
	scope := Scope{NodeIDs: []shoal.ID{"subject"}}
	firstID := authorizedScopeSnapshotID(
		scope, fingerprint, 1, ontology.OntologyIdentity{}, false,
		explorer.Neighborhood{Assertions: []ontology.Assertion{first}})
	secondID := authorizedScopeSnapshotID(
		scope, fingerprint, 1, ontology.OntologyIdentity{}, false,
		explorer.Neighborhood{Assertions: []ontology.Assertion{second}})
	if firstID == secondID {
		t.Fatal("snapshot ID did not bind assertion annotation metadata")
	}
}

func TestDerivedAssertionEvidenceUsesAuthoritativeAssertionEdge(t *testing.T) {
	value, err := ontology.NewReferenceValue("target")
	if err != nil {
		t.Fatal(err)
	}
	derivation, err := ontology.NewAssertionDerivation(
		"model", "1", "cosine", 0.5, "cell", 0.9,
		"subject", "target", "iterator", nil)
	if err != nil {
		t.Fatal(err)
	}
	evidence, err := ontology.NewDerivationEvidenceRef(derivation, nil)
	if err != nil {
		t.Fatal(err)
	}
	provenance, err := ontology.NewExtractionProvenance(
		"provider", "model", "1", "prompt", "1", "extractor", "1", nil)
	if err != nil {
		t.Fatal(err)
	}
	sourceConcept, err := ontology.NewConceptDefinition(
		"source", "Source", "", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	targetConcept, err := ontology.NewConceptDefinition(
		"target", "Target", "", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	relationship, err := ontology.NewRelationshipDefinition(
		"related", "Related", "",
		[]shoal.ID{sourceConcept.ID()}, []shoal.ID{targetConcept.ID()},
		nil, true, nil)
	if err != nil {
		t.Fatal(err)
	}
	assertion, err := ontology.NewAssertion(
		"subject", relationship.ID(), value, ontology.AssertionDerived, 0.9,
		[]ontology.EvidenceRef{evidence}, provenance, nil)
	if err != nil {
		t.Fatal(err)
	}
	reference, err := InteractionAssertionEvidence(assertion)
	if err != nil {
		t.Fatal(err)
	}
	if reference.EdgeID != assertion.ID() {
		t.Fatalf("derived assertion edge = %q, want %q",
			reference.EdgeID, assertion.ID())
	}
}

func TestAnalyzeDanglingGraphConvergesAndPreservesMass(t *testing.T) {
	got, err := Analyze(context.Background(), explorer.Neighborhood{
		Nodes: []graph.Node{{ID: "A"}, {ID: "B"}},
		Edges: []graph.Edge{{
			ID: "A-B", From: "A", To: "B", Type: "link", Weight: 1,
		}},
	}, PageRankOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Nodes) != 2 || got.Nodes[0].NodeID != "B" ||
		got.Nodes[1].NodeID != "A" {
		t.Fatalf("rank order = %#v", got.Nodes)
	}
	if math.Abs(got.Nodes[0].PageRank-0.6491228070) > 1e-8 ||
		math.Abs(got.Nodes[1].PageRank-0.3508771930) > 1e-8 {
		t.Fatalf("dangling ranks = %#v", got.Nodes)
	}
	if got.Nodes[0].InDegree != 1 || got.Nodes[0].OutDegree != 0 ||
		got.Nodes[1].InDegree != 0 || got.Nodes[1].OutDegree != 1 {
		t.Fatalf("degrees = %#v", got.Nodes)
	}
	if len(got.WeaklyConnectedComponents) != 1 ||
		got.WeaklyConnectedComponents[0].EdgeCount != 1 {
		t.Fatalf("components = %#v", got.WeaklyConnectedComponents)
	}
}

func TestAnalyzeDisconnectedCycleIsDeterministic(t *testing.T) {
	first := explorer.Neighborhood{
		Nodes: []graph.Node{{ID: "D"}, {ID: "B"}, {ID: "A"}, {ID: "C"}},
		Edges: []graph.Edge{
			{ID: "C-A", From: "C", To: "A", Type: "link", Weight: 1},
			{ID: "B-C", From: "B", To: "C", Type: "link", Weight: 1},
			{ID: "A-B", From: "A", To: "B", Type: "link", Weight: 1},
		},
	}
	second := explorer.Neighborhood{
		Nodes: []graph.Node{{ID: "C"}, {ID: "A"}, {ID: "D"}, {ID: "B"}},
		Edges: []graph.Edge{
			first.Edges[2], first.Edges[0], first.Edges[1],
		},
	}
	left, err := Analyze(context.Background(), first, PageRankOptions{})
	if err != nil {
		t.Fatal(err)
	}
	right, err := Analyze(context.Background(), second, PageRankOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(left, right) {
		t.Fatalf("input order changed result:\nleft=%#v\nright=%#v", left, right)
	}
	if len(left.WeaklyConnectedComponents) != 2 ||
		!reflect.DeepEqual(
			left.WeaklyConnectedComponents[0].NodeIDs,
			[]shoal.ID{"A", "B", "C"},
		) ||
		!reflect.DeepEqual(
			left.WeaklyConnectedComponents[1].NodeIDs,
			[]shoal.ID{"D"},
		) {
		t.Fatalf("components = %#v", left.WeaklyConnectedComponents)
	}
	for index, want := range []shoal.ID{"A", "B", "C", "D"} {
		if left.Nodes[index].NodeID != want {
			t.Fatalf("tie order[%d] = %q, want %q", index, left.Nodes[index].NodeID, want)
		}
	}
}

func TestAnalyzeRejectsDanglingEdgeEndpoint(t *testing.T) {
	_, err := Analyze(context.Background(), explorer.Neighborhood{
		Nodes: []graph.Node{{ID: "A"}},
		Edges: []graph.Edge{{
			ID: "A-missing", From: "A", To: "missing", Type: "link", Weight: 1,
		}},
	}, PageRankOptions{})
	if !shoal.IsErrorCode(err, shoal.ErrorInvalidArgument) {
		t.Fatalf("error = %v", err)
	}
}

func TestAnalyzeFailsOnNonconvergence(t *testing.T) {
	_, err := Analyze(context.Background(), explorer.Neighborhood{
		Nodes: []graph.Node{{ID: "A"}, {ID: "B"}},
		Edges: []graph.Edge{{
			ID: "A-B", From: "A", To: "B", Type: "link", Weight: 1,
		}},
	}, PageRankOptions{
		DampingFactor: 0.85, ConvergenceTolerance: MinPageRankTolerance,
		MaxIterations: 1,
	})
	if !shoal.IsErrorCode(err, shoal.ErrorUnavailable) {
		t.Fatalf("error = %v", err)
	}
}

func TestValidateResultRejectsPerComponentEdgeMismatch(t *testing.T) {
	request, result := validResultForValidation(t)
	result.WeaklyConnectedComponents[0].EdgeCount = 2
	result.WeaklyConnectedComponents[1].EdgeCount = 0
	if err := ValidateResult(request, result, DefaultLimits()); !shoal.IsErrorCode(
		err, shoal.ErrorInternal,
	) {
		t.Fatalf("component edge mismatch error = %v", err)
	}
}

func TestValidateResultRejectsOversizedInteractionID(t *testing.T) {
	request, result := validResultForValidation(t)
	result.Recording.Recorded = true
	result.Recording.InteractionID = shoal.ID(strings.Repeat("x", shoal.MaxIDBytes+1))
	if err := ValidateResult(request, result, DefaultLimits()); !shoal.IsErrorCode(
		err, shoal.ErrorInternal,
	) {
		t.Fatalf("oversized interaction ID error = %v", err)
	}
}

func TestAuthorizedScopeSnapshotIncludesAssertionEvidence(t *testing.T) {
	left := snapshotAssertion(t, ontology.AssertionExplicit)
	right := snapshotAssertion(t, ontology.AssertionInferred)
	request := boundedRequest("node").Scope
	var fingerprint auth.Fingerprint
	fingerprint[0] = 1
	leftID := authorizedScopeSnapshotID(
		request, fingerprint, 1, ontology.OntologyIdentity{}, false,
		explorer.Neighborhood{Assertions: []ontology.Assertion{left}},
	)
	rightID := authorizedScopeSnapshotID(
		request, fingerprint, 1, ontology.OntologyIdentity{}, false,
		explorer.Neighborhood{Assertions: []ontology.Assertion{right}},
	)
	if leftID == rightID {
		t.Fatal("different durable assertion evidence produced the same snapshot")
	}
}

func TestAnalyzeHonorsCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := Analyze(ctx, explorer.Neighborhood{}, PageRankOptions{})
	if !shoal.IsErrorCode(err, shoal.ErrorCanceled) {
		t.Fatalf("error = %v", err)
	}
}

func TestValidateRequestBoundsExtremes(t *testing.T) {
	limits := DefaultLimits()
	nodeIDs := make([]shoal.ID, limits.MaxSeeds)
	for index := range nodeIDs {
		nodeIDs[index] = shoal.ID(string(rune('a' + index)))
	}
	maximum := Request{
		Scope: Scope{
			NodeIDs: nodeIDs, Depth: limits.MaxDepth,
			Direction: explorer.GraphDirectionBoth,
			Fanout:    limits.MaxFanout, MaxNodes: limits.MaxNodes,
			MaxEdges:               limits.MaxEdges,
			MaxScannedEdgesPerNode: limits.MaxScannedEdgesPerNode,
			EdgeTypes:              make([]string, limits.MaxEdgeTypes),
		},
		PageRank: PageRankOptions{
			DampingFactor:        0.5,
			ConvergenceTolerance: limits.MinPageRankTolerance,
			MaxIterations:        limits.MaxPageRankIterations,
		},
	}
	for index := range maximum.Scope.EdgeTypes {
		maximum.Scope.EdgeTypes[index] = fmt.Sprintf("edge-%d", index)
	}
	if err := ValidateRequest(maximum, limits); err != nil {
		t.Fatalf("maximum request: %v", err)
	}
	tests := []Request{
		func() Request {
			value := maximum
			value.Scope.Depth++
			return value
		}(),
		func() Request {
			value := maximum
			value.Scope.MaxEdges++
			return value
		}(),
		func() Request {
			value := maximum
			value.PageRank.MaxIterations++
			return value
		}(),
		func() Request {
			value := maximum
			value.Scope.MaxScannedEdgesPerNode = value.Scope.Fanout - 1
			return value
		}(),
		func() Request {
			value := maximum
			value.Scope.MaxNodes = 0
			return value
		}(),
	}
	for index, request := range tests {
		if err := ValidateRequest(request, limits); !shoal.IsErrorCode(
			err, shoal.ErrorInvalidArgument,
		) {
			t.Fatalf("invalid request %d error = %v", index, err)
		}
	}
}

func TestNormalizePageRankDefaultsRespectProviderLimits(t *testing.T) {
	limits := DefaultLimits()
	limits.MaxPageRankIterations = 10
	limits.MinPageRankTolerance = 1e-8
	normalized, err := normalizePageRank(PageRankOptions{}, limits)
	if err != nil {
		t.Fatal(err)
	}
	if normalized.DampingFactor != DefaultDampingFactor ||
		normalized.ConvergenceTolerance != limits.MinPageRankTolerance ||
		normalized.MaxIterations != limits.MaxPageRankIterations {
		t.Fatalf("normalized defaults = %#v", normalized)
	}
}

func TestServiceRejectsProviderMarkedIncomplete(t *testing.T) {
	source := &staticMaterializer{materialization: Materialization{}}
	service, err := NewService(Config{Source: source, Limits: DefaultLimits()})
	if err != nil {
		t.Fatal(err)
	}
	_, err = service.Run(context.Background(), boundedRequest("node"))
	if !shoal.IsErrorCode(err, shoal.ErrorUnavailable) {
		t.Fatalf("incomplete materialization error = %v", err)
	}
	if source.revalidations != 0 {
		t.Fatalf("incomplete materialization was revalidated %d times", source.revalidations)
	}
}

func TestServiceReportsUnresolvedOntologySemantics(t *testing.T) {
	schema, err := ontology.NewOntologySchema("analytics", "Analytics", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	version, err := ontology.NewOntologyVersion(
		schema, "1", time.Date(2026, time.September, 5, 18, 0, 0, 0, time.UTC),
		nil, nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	lens, _ := ontology.NewOntologyIdentity(version)
	property, err := ontology.NewPropertyDefinition(
		"name", "Name", "", ontology.ValueString, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	value, err := ontology.NewStringValue("alpha")
	if err != nil {
		t.Fatal(err)
	}
	evidence, err := ontology.NewEvidenceRef(document.Citation{
		DocumentID: "document", RevisionID: "revision",
		SectionID: "section", SpanID: "span",
		Range: document.SourceRange{
			Start: document.SourcePosition{Offset: 0, Page: 1},
			End:   document.SourcePosition{Offset: 5, Page: 1},
		},
	}, "alpha", nil)
	if err != nil {
		t.Fatal(err)
	}
	provenance, err := ontology.NewExtractionProvenance(
		"provider", "model", "1", "prompt", "1", "extractor", "1", nil)
	if err != nil {
		t.Fatal(err)
	}
	assertion, err := ontology.NewAssertion(
		"entity", property.ID(), value, ontology.AssertionExplicit, 1,
		[]ontology.EvidenceRef{evidence}, provenance, nil)
	if err != nil {
		t.Fatal(err)
	}
	var fingerprint auth.Fingerprint
	fingerprint[0] = 1
	source := &staticMaterializer{materialization: Materialization{
		Snapshot: explorer.Snapshot{ID: "internal"},
		Neighborhood: explorer.Neighborhood{
			Nodes:      []graph.Node{{ID: "node"}},
			Assertions: []ontology.Assertion{assertion},
			Interpretations: []ontology.AssertionInterpretation{
				ontology.UnresolvedInterpretation(
					assertion, lens, "no unique published morphism path"),
			},
		},
		AuthorizationFingerprint: fingerprint,
		PolicyGeneration:         1,
		SelectedOntology:         lens,
		Complete:                 true,
	}}
	service, err := NewService(Config{Source: source, Limits: DefaultLimits()})
	if err != nil {
		t.Fatal(err)
	}
	result, err := service.Run(context.Background(), boundedRequest("node"))
	if err != nil {
		t.Fatal(err)
	}
	if result.Scope.Ontology == nil ||
		result.Scope.Ontology.VersionID != lens.VersionID() ||
		result.Scope.ResolvedAssertionCount != 0 ||
		len(result.Scope.UnresolvedAssertions) != 1 ||
		result.Scope.UnresolvedAssertions[0].AssertionID != assertion.ID() ||
		result.Scope.UnresolvedAssertions[0].Reason !=
			"no unique published morphism path" {
		t.Fatalf("ontology scope = %#v", result.Scope)
	}
	if source.revalidations != 1 || result.Recording.Recorded {
		t.Fatalf("revalidations=%d recording=%#v",
			source.revalidations, result.Recording)
	}
}

func TestServicePreservesIndeterminateRecordingFailure(t *testing.T) {
	var fingerprint auth.Fingerprint
	fingerprint[0] = 1
	source := &staticMaterializer{materialization: Materialization{
		Neighborhood: explorer.Neighborhood{
			Nodes: []graph.Node{{ID: "node"}},
		},
		AuthorizationFingerprint: fingerprint,
		PolicyGeneration:         1,
		Complete:                 true,
	}}
	failure := errors.New("unknown durable outcome")
	service, err := NewService(Config{
		Source: source, Limits: DefaultLimits(),
		Recorder: recorderStub(func(
			context.Context,
			Record,
		) (RecordingReceipt, error) {
			return RecordingReceipt{},
				explorer.MarkIndeterminateCommit(failure)
		}),
		RequireRecording: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = service.Run(context.Background(), boundedRequest("node"))
	if !explorer.IsIndeterminateCommit(err) ||
		!shoal.IsErrorCode(err, shoal.ErrorUnavailable) ||
		!errors.Is(err, failure) {
		t.Fatalf("recording failure = %v", err)
	}
}

func boundedRequest(nodeID shoal.ID) Request {
	return Request{Scope: Scope{
		NodeIDs: []shoal.ID{nodeID}, Depth: 1,
		Direction: explorer.GraphDirectionBoth,
		Fanout:    4, MaxNodes: 8, MaxEdges: 16,
		MaxScannedEdgesPerNode: 32,
	}}
}

func validResultForValidation(t *testing.T) (Request, Result) {
	t.Helper()
	neighborhood := explorer.Neighborhood{
		Nodes: []graph.Node{{ID: "A"}, {ID: "B"}, {ID: "C"}, {ID: "D"}},
		Edges: []graph.Edge{
			{ID: "A-B", From: "A", To: "B", Type: "link", Weight: 1},
			{ID: "C-D", From: "C", To: "D", Type: "link", Weight: 1},
		},
	}
	analysis, err := Analyze(context.Background(), neighborhood, PageRankOptions{})
	if err != nil {
		t.Fatal(err)
	}
	request := boundedRequest("A")
	result := Result{
		Scope: ScopeMetadata{
			SnapshotID: "snapshot", AuthorizationFingerprint: "fingerprint",
			PolicyGeneration: 1, SeedNodeIDs: append(
				[]shoal.ID(nil), request.Scope.NodeIDs...),
			Depth: request.Scope.Depth, Direction: request.Scope.Direction,
			Fanout: request.Scope.Fanout, MaxNodes: request.Scope.MaxNodes,
			MaxEdges:               request.Scope.MaxEdges,
			MaxScannedEdgesPerNode: request.Scope.MaxScannedEdgesPerNode,
			NodeCount:              uint32(len(neighborhood.Nodes)),
			EdgeCount:              uint32(len(neighborhood.Edges)), Complete: true,
		},
		Nodes:                     analysis.Nodes,
		WeaklyConnectedComponents: analysis.WeaklyConnectedComponents,
		PageRank:                  analysis.PageRank,
	}
	return request, result
}

func snapshotAssertion(
	t *testing.T,
	origin ontology.AssertionOrigin,
) ontology.Assertion {
	t.Helper()
	property, err := ontology.NewPropertyDefinition(
		"name", "Name", "", ontology.ValueString, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	value, err := ontology.NewStringValue("alpha")
	if err != nil {
		t.Fatal(err)
	}
	evidence, err := ontology.NewEvidenceRef(document.Citation{
		DocumentID: "document", RevisionID: "revision",
		SectionID: "section", SpanID: "span",
		Range: document.SourceRange{
			Start: document.SourcePosition{Offset: 0, Page: 1},
			End:   document.SourcePosition{Offset: 5, Page: 1},
		},
	}, "alpha", nil)
	if err != nil {
		t.Fatal(err)
	}
	provenance, err := ontology.NewExtractionProvenance(
		"provider", "model", "1", "prompt", "1", "extractor", "1", nil)
	if err != nil {
		t.Fatal(err)
	}
	assertion, err := ontology.NewAssertion(
		"entity", property.ID(), value, origin, 1,
		[]ontology.EvidenceRef{evidence}, provenance, nil)
	if err != nil {
		t.Fatal(err)
	}
	return assertion
}

type staticMaterializer struct {
	materialization Materialization
	revalidations   int
}

type recorderStub func(
	context.Context,
	Record,
) (RecordingReceipt, error)

func (f recorderStub) RecordAnalytics(
	ctx context.Context,
	record Record,
) (RecordingReceipt, error) {
	return f(ctx, record)
}

func (s *staticMaterializer) MaterializeAnalytics(
	context.Context,
	explorer.BoundedNeighborhoodRequest,
	uint32,
	uint64,
) (Materialization, error) {
	return s.materialization, nil
}

func (s *staticMaterializer) RevalidateAnalytics(
	context.Context,
	Materialization,
) error {
	s.revalidations++
	return nil
}
