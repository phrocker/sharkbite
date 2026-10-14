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

package explorerconformance

import (
	"context"
	"math"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/phrocker/shoal-oss/pkg/document"
	"github.com/phrocker/shoal-oss/pkg/explorer"
	"github.com/phrocker/shoal-oss/pkg/graph"
	"github.com/phrocker/shoal-oss/pkg/retrieval"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

// Lifecycle owns one isolated Explorer client and its storage-neutral restart
// and cleanup hooks.
type Lifecycle struct {
	Client   explorer.Client
	IngestAt func(context.Context, explorer.Source, time.Time) (explorer.IngestResult, error)
	Restart  func(context.Context) (explorer.Client, error)
	Close    func() error
}

// ClientFactory opens one isolated client lifecycle for a conformance case.
// It receives independently owned, normalized fixture controls. Direct and
// loopback adapters can implement the same shape without exposing storage
// engines, rows, scanners, or transport internals.
type ClientFactory func(testing.TB, FixtureControls) (Lifecycle, error)

// Run executes the M1 storage-neutral public Explorer conformance suite.
func Run(t *testing.T, factory ClientFactory) {
	t.Helper()
	if factory == nil {
		t.Fatal("Explorer conformance factory is required")
	}
	tests := []struct {
		name string
		run  func(*testing.T, ClientFactory)
	}{
		{"winner_and_explicit_revision", winnerAndExplicitRevision},
		{"citation_source_integrity", citationSourceIntegrity},
		{"graph_association", graphAssociation},
		{"deterministic_ordering", deterministicOrdering},
		{"opaque_values", opaqueValues},
		{"normalization_and_errors", normalizationAndErrors},
		{"unsupported_retrieval", unsupportedRetrieval},
		{"restart", restart},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			test.run(t, factory)
		})
	}
}

func winnerAndExplicitRevision(t *testing.T, factory ClientFactory) {
	ctx := context.Background()
	controls := FixtureControls{
		Clock: FakeClock{Instants: []time.Time{
			time.Date(
				2099, time.January, 1, 0, 0, 0, 0,
				time.FixedZone("future-source", 60*60),
			),
			time.Date(
				2001, time.January, 1, 0, 0, 0, 0,
				time.FixedZone("past-source", -5*60*60),
			),
		}},
		Authorities: WriterAuthorityHistory{{
			Generation: 1,
			Mode:       WriterAuthorityEmbeddedPrimary,
			Holder:     "embedded-fixture",
			Fence:      1,
		}},
	}
	lifecycle := openLifecycle(t, factory, controls)
	controls, err := controls.Normalize()
	if err != nil {
		t.Fatal(err)
	}
	firstClock, _ := controls.Clock.At(0)
	secondClock, _ := controls.Clock.At(1)
	first, err := lifecycle.IngestAt(ctx, explorer.Source{
		URI:       "file:///winner.txt",
		MediaType: explorer.MediaTypeText,
		Content:   "firstonly",
	}, firstClock)
	if err != nil {
		t.Fatal(err)
	}
	second, err := lifecycle.IngestAt(ctx, explorer.Source{
		URI:       "file:///winner.txt",
		MediaType: explorer.MediaTypeText,
		Content:   "secondonly",
	}, secondClock)
	if err != nil {
		t.Fatal(err)
	}
	if first.Document.ID != second.Document.ID ||
		first.Revision.ID == second.Revision.ID {
		t.Fatalf("revision identities = first %+v, second %+v", first, second)
	}
	if !firstClock.After(secondClock) {
		t.Fatal("fixture must put the earlier publication timestamp after the winner timestamp")
	}
	if !first.Revision.CreatedAt.Equal(firstClock) ||
		!second.Revision.CreatedAt.Equal(secondClock) ||
		!first.Revision.CreatedAt.After(second.Revision.CreatedAt) {
		t.Fatalf(
			"revision CreatedAt values = first %v, second %v",
			first.Revision.CreatedAt, second.Revision.CreatedAt,
		)
	}
	restartLifecycle(t, lifecycle)
	documents, err := lifecycle.Client.Documents(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(documents) != 1 || documents[0].Revision.ID != second.Revision.ID {
		t.Fatalf("current documents = %+v", documents)
	}
	if !documents[0].Revision.CreatedAt.Equal(secondClock) {
		t.Fatalf("winner CreatedAt after restart = %v", documents[0].Revision.CreatedAt)
	}
	current, err := lifecycle.Client.Document(ctx, second.Document.ID, "")
	if err != nil {
		t.Fatal(err)
	}
	if current.Revision.ID != second.Revision.ID ||
		!viewContains(current, "secondonly") {
		t.Fatalf("current view = %+v", current)
	}
	firstView, err := lifecycle.Client.Document(
		ctx, first.Document.ID, first.Revision.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !viewContains(firstView, "firstonly") {
		t.Fatalf("explicit first revision = %+v", firstView)
	}
	if !firstView.Revision.CreatedAt.Equal(firstClock) {
		t.Fatalf("explicit first CreatedAt = %v", firstView.Revision.CreatedAt)
	}
	secondView, err := lifecycle.Client.Document(
		ctx, second.Document.ID, second.Revision.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !viewContains(secondView, "secondonly") {
		t.Fatalf("explicit second revision = %+v", secondView)
	}
	if !secondView.Revision.CreatedAt.Equal(secondClock) {
		t.Fatalf("explicit second CreatedAt = %v", secondView.Revision.CreatedAt)
	}
	oldResponse, err := lifecycle.Client.Retrieve(ctx, retrieval.Request{
		Text: "firstonly", TopK: 10,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(oldResponse.Results) != 0 {
		t.Fatalf("superseded revision was retrieved: %+v", oldResponse.Results)
	}
	currentResponse, err := lifecycle.Client.Retrieve(ctx, retrieval.Request{
		Text: "secondonly", TopK: 10,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(currentResponse.Results) != 1 ||
		len(currentResponse.Results[0].Evidence) != 1 ||
		currentResponse.Results[0].Evidence[0].Citation.RevisionID !=
			second.Revision.ID {
		t.Fatalf("current retrieval = %+v", currentResponse)
	}
}

func citationSourceIntegrity(t *testing.T, factory ClientFactory) {
	lifecycle := openLifecycle(t, factory, standardControls())
	ctx := context.Background()
	source := explorer.Source{
		URI:       "file:///citation.md",
		MediaType: explorer.MediaTypeMarkdown,
		Content: "# Café\n\nCafé βeta evidence stays byte exact.\n\n" +
			"## Other\n\nUnrelated text.\n",
		Metadata: shoal.Metadata{"fixture": "citation"},
	}
	ingested, err := lifecycle.Client.Ingest(ctx, source)
	if err != nil {
		t.Fatal(err)
	}
	response, err := lifecycle.Client.Retrieve(ctx, retrieval.Request{
		Text: "βeta evidence",
		TopK: 1,
		Modes: []retrieval.Mode{
			retrieval.ModeLexical, retrieval.ModeTree, retrieval.ModeGraph,
		},
		Explain: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(response.Results) != 1 ||
		len(response.Results[0].Evidence) != 1 {
		t.Fatalf("retrieval response = %+v", response)
	}
	evidence := response.Results[0].Evidence[0]
	view, err := lifecycle.Client.Document(
		ctx, ingested.Document.ID, ingested.Revision.ID)
	if err != nil {
		t.Fatal(err)
	}
	sections, spans := flattenView(view.Root)
	if err := document.ValidateCitationQuote(
		source.Content,
		view.Document,
		view.Revision,
		sections,
		spans,
		evidence.Citation,
		evidence.Quote,
	); err != nil {
		t.Fatalf("citation quote integrity: %v", err)
	}
	if evidence.Citation.DocumentID != view.Document.ID ||
		evidence.Citation.RevisionID != view.Revision.ID {
		t.Fatalf("citation ownership = %+v, view = %+v", evidence.Citation, view)
	}
	if err := evidence.Citation.Range.ValidateSource(source.Content); err != nil {
		t.Fatalf("citation range: %v", err)
	}
	start := evidence.Citation.Range.Start.Offset
	end := evidence.Citation.Range.End.Offset
	if got := source.Content[int(start):int(end)]; got != evidence.Quote {
		t.Fatalf("source slice = %q, quote = %q", got, evidence.Quote)
	}
	if err := evidence.Path.Validate(); err != nil {
		t.Fatalf("evidence path: %v", err)
	}
	if len(evidence.Path.Nodes) < 2 ||
		evidence.Path.Nodes[0].ID != view.Document.ID ||
		evidence.Path.Nodes[len(evidence.Path.Nodes)-1].ID !=
			evidence.Citation.SpanID {
		t.Fatalf("evidence path endpoints = %+v", evidence.Path)
	}
	if response.Results[0].Explanation == nil ||
		!strings.Contains(
			response.Results[0].Explanation.Summary,
			retrieval.UnicodeTermAnalyzerVersion,
		) ||
		!strings.Contains(
			response.Results[0].Explanation.Summary,
			retrieval.CoverageFusionScorerVersion,
		) {
		t.Fatalf("versioned explanation = %+v", response.Results[0].Explanation)
	}
}

func graphAssociation(t *testing.T, factory ClientFactory) {
	lifecycle := openLifecycle(t, factory, standardControls())
	ctx := context.Background()
	first, err := lifecycle.Client.Ingest(ctx, explorer.Source{
		URI: "file:///association-a.txt", MediaType: explorer.MediaTypeText,
		Content: "association alpha needle",
	})
	if err != nil {
		t.Fatal(err)
	}
	second, err := lifecycle.Client.Ingest(ctx, explorer.Source{
		URI: "file:///association-b.txt", MediaType: explorer.MediaTypeText,
		Content: "association beta needle",
	})
	if err != nil {
		t.Fatal(err)
	}
	scoped, err := lifecycle.Client.Retrieve(ctx, retrieval.Request{
		Text: "alpha",
		Scope: retrieval.Scope{
			DocumentIDs: []shoal.ID{first.Document.ID},
			NodeIDs:     []shoal.ID{first.Document.ID},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(scoped.Results) != 1 ||
		len(scoped.Results[0].Evidence) != 1 ||
		scoped.Results[0].Evidence[0].Citation.DocumentID != first.Document.ID {
		t.Fatalf("associated scope result = %+v", scoped)
	}
	mismatched, err := lifecycle.Client.Retrieve(ctx, retrieval.Request{
		Text: "alpha",
		Scope: retrieval.Scope{
			DocumentIDs: []shoal.ID{first.Document.ID},
			NodeIDs:     []shoal.ID{second.Document.ID},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(mismatched.Results) != 0 {
		t.Fatalf("mismatched association result = %+v", mismatched)
	}
	edge := graph.Edge{
		ID:   "public-association",
		From: first.Document.ID, To: second.Document.ID,
		Type: "associated_with", Weight: 1,
	}
	if err := lifecycle.Client.Connect(ctx, edge); err != nil {
		t.Fatal(err)
	}
	neighborhood, err := lifecycle.Client.Neighborhood(
		ctx, explorer.NeighborhoodRequest{
			NodeIDs:   []shoal.ID{second.Document.ID},
			EdgeTypes: []string{"associated_with"},
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(neighborhood.Nodes) != 2 || len(neighborhood.Edges) != 1 ||
		neighborhood.Edges[0].ID != edge.ID ||
		neighborhood.Edges[0].From != first.Document.ID ||
		neighborhood.Edges[0].To != second.Document.ID {
		t.Fatalf("incoming neighborhood = %+v", neighborhood)
	}
	assertNodeOrder(t, neighborhood.Nodes)
	filtered, err := lifecycle.Client.Neighborhood(
		ctx, explorer.NeighborhoodRequest{
			NodeIDs:   []shoal.ID{second.Document.ID},
			EdgeTypes: []string{"different_type"},
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(filtered.Nodes) != 1 || filtered.Nodes[0].ID != second.Document.ID ||
		len(filtered.Edges) != 0 {
		t.Fatalf("filtered neighborhood = %+v", filtered)
	}
}

func deterministicOrdering(t *testing.T, factory ClientFactory) {
	lifecycle := openLifecycle(t, factory, standardControls())
	ctx := context.Background()
	for _, name := range []string{"charlie", "alpha", "bravo"} {
		if _, err := lifecycle.Client.Ingest(ctx, explorer.Source{
			URI: "file:///" + name + ".txt", MediaType: explorer.MediaTypeText,
			Content: "deterministic tie token",
		}); err != nil {
			t.Fatal(err)
		}
	}
	request := retrieval.Request{
		Text: "deterministic tie token", TopK: 10,
		Modes: []retrieval.Mode{retrieval.ModeLexical},
	}
	first, err := lifecycle.Client.Retrieve(ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	if len(first.Results) != 3 {
		t.Fatalf("results = %+v", first.Results)
	}
	if err := first.ValidateFor(request); err != nil {
		t.Fatalf("response validation: %v", err)
	}
	assertResultOrder(t, first.Results)
	for _, result := range first.Results {
		if math.IsNaN(float64(result.Score)) ||
			math.IsInf(float64(result.Score), 0) ||
			result.Score != 1 ||
			len(result.Evidence) != 1 ||
			result.Evidence[0].Score != result.Score {
			t.Fatalf("result score/evidence = %+v", result)
		}
	}
	for attempt := 0; attempt < 4; attempt++ {
		next, err := lifecycle.Client.Retrieve(ctx, request)
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(next, first) {
			t.Fatalf("retrieval attempt %d changed:\n%+v\n%+v", attempt, first, next)
		}
	}
	restartLifecycle(t, lifecycle)
	reopened, err := lifecycle.Client.Retrieve(ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(reopened, first) {
		t.Fatalf("retrieval changed after restart:\n%+v\n%+v", first, reopened)
	}
}

func opaqueValues(t *testing.T, factory ClientFactory) {
	lifecycle := openLifecycle(t, factory, standardControls())
	ctx := context.Background()
	metadata := shoal.Metadata{
		string([]byte{'k', 0, 0xff}): string([]byte{0xfd, 0, 'v'}),
	}
	first, err := lifecycle.Client.Ingest(ctx, explorer.Source{
		URI: "file:///opaque-a.txt", MediaType: explorer.MediaTypeText,
		Content: "opaque first", Metadata: metadata,
	})
	if err != nil {
		t.Fatal(err)
	}
	second, err := lifecycle.Client.Ingest(ctx, explorer.Source{
		URI: "file:///opaque-b.txt", MediaType: explorer.MediaTypeText,
		Content: "opaque second",
	})
	if err != nil {
		t.Fatal(err)
	}
	opaqueID := shoal.ID(string([]byte{'e', '/', 0, 0xff}))
	edge := graph.Edge{
		ID: opaqueID, From: first.Document.ID, To: second.Document.ID,
		Type: "opaque_link", Weight: 1, Properties: metadata,
	}
	if err := lifecycle.Client.Connect(ctx, edge); err != nil {
		t.Fatal(err)
	}
	restartLifecycle(t, lifecycle)
	view, err := lifecycle.Client.Document(
		ctx, first.Document.ID, first.Revision.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(view.Document.Metadata, metadata) ||
		!reflect.DeepEqual(view.Revision.Metadata, metadata) {
		t.Fatalf("opaque metadata = document %#v, revision %#v",
			view.Document.Metadata, view.Revision.Metadata)
	}
	neighborhood, err := lifecycle.Client.Neighborhood(
		ctx, explorer.NeighborhoodRequest{
			NodeIDs:   []shoal.ID{first.Document.ID},
			EdgeTypes: []string{"opaque_link"},
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(neighborhood.Edges) != 1 ||
		neighborhood.Edges[0].ID != opaqueID ||
		!reflect.DeepEqual(neighborhood.Edges[0].Properties, metadata) {
		t.Fatalf("opaque edge = %+v", neighborhood)
	}
	if _, err := lifecycle.Client.Document(
		ctx, shoal.ID(string([]byte{0xff, 0, 'x'})), "missing-revision",
	); !shoal.IsErrorCode(err, shoal.ErrorNotFound) {
		t.Fatalf("opaque missing ID error = %v", err)
	}
}

func normalizationAndErrors(t *testing.T, factory ClientFactory) {
	lifecycle := openLifecycle(t, factory, standardControls())
	ctx := context.Background()
	ingested, err := lifecycle.Client.Ingest(ctx, explorer.Source{
		URI: "file:///normalization.txt", MediaType: explorer.MediaTypeText,
		Content: "normalization stable token",
	})
	if err != nil {
		t.Fatal(err)
	}
	canonicalRequest := retrieval.Request{
		Text: "stable", TopK: retrieval.DefaultTopK,
		Modes: []retrieval.Mode{retrieval.ModeLexical},
		Scope: retrieval.Scope{DocumentIDs: []shoal.ID{ingested.Document.ID}},
	}
	canonical, err := lifecycle.Client.Retrieve(ctx, canonicalRequest)
	if err != nil {
		t.Fatal(err)
	}
	duplicate, err := lifecycle.Client.Retrieve(ctx, retrieval.Request{
		Text: "stable", TopK: retrieval.DefaultTopK,
		Modes: []retrieval.Mode{
			retrieval.ModeLexical, retrieval.ModeLexical,
		},
		Scope: retrieval.Scope{DocumentIDs: []shoal.ID{
			ingested.Document.ID, ingested.Document.ID,
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(duplicate, canonical) {
		t.Fatalf("normalized retrieval differs: %+v != %+v", duplicate, canonical)
	}
	canonicalNeighborhood, err := lifecycle.Client.Neighborhood(
		ctx, explorer.NeighborhoodRequest{
			NodeIDs:   []shoal.ID{ingested.Document.ID},
			Depth:     1,
			EdgeTypes: []string{"contains"},
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	duplicateNeighborhood, err := lifecycle.Client.Neighborhood(
		ctx, explorer.NeighborhoodRequest{
			NodeIDs: []shoal.ID{
				ingested.Document.ID, ingested.Document.ID,
			},
			EdgeTypes: []string{"contains", "contains"},
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(duplicateNeighborhood, canonicalNeighborhood) {
		t.Fatalf("normalized neighborhood differs: %+v != %+v",
			duplicateNeighborhood, canonicalNeighborhood)
	}
	if _, err := lifecycle.Client.Ingest(ctx, explorer.Source{
		URI: "file:///invalid.txt", MediaType: explorer.MediaTypeText,
		Content: " ",
	}); !shoal.IsErrorCode(err, shoal.ErrorInvalidArgument) {
		t.Fatalf("invalid source error = %v", err)
	}
	if _, err := lifecycle.Client.Document(
		ctx, "missing-document", "missing-revision",
	); !shoal.IsErrorCode(err, shoal.ErrorNotFound) {
		t.Fatalf("missing document error = %v", err)
	}
	if err := lifecycle.Client.Connect(ctx, graph.Edge{
		ID: "missing-endpoint", From: ingested.Document.ID, To: "missing",
		Type: "links", Weight: 1,
	}); !shoal.IsErrorCode(err, shoal.ErrorNotFound) {
		t.Fatalf("missing endpoint error = %v", err)
	}
	conflicting := graph.Edge{
		ID: "conflicting-edge", From: ingested.Document.ID,
		To: ingested.Document.ID, Type: "links", Weight: 1,
	}
	if err := lifecycle.Client.Connect(ctx, conflicting); err != nil {
		t.Fatal(err)
	}
	conflicting.Type = "different"
	if err := lifecycle.Client.Connect(ctx, conflicting); !shoal.IsErrorCode(err, shoal.ErrorConflict) {
		t.Fatalf("edge conflict error = %v", err)
	}
	if _, err := lifecycle.Client.Retrieve(ctx, retrieval.Request{
		Text: "stable", TopK: retrieval.MaxTopK + 1,
	}); !shoal.IsErrorCode(err, shoal.ErrorInvalidArgument) {
		t.Fatalf("TopK bound error = %v", err)
	}
	if _, err := lifecycle.Client.Document(
		ctx, shoal.ID(strings.Repeat("x", shoal.MaxIDBytes+1)), "",
	); !shoal.IsErrorCode(err, shoal.ErrorInvalidArgument) {
		t.Fatalf("ID bound error = %v", err)
	}
	if _, err := lifecycle.Client.Neighborhood(
		ctx, explorer.NeighborhoodRequest{
			NodeIDs: []shoal.ID{ingested.Document.ID}, Depth: 17,
		},
	); !shoal.IsErrorCode(err, shoal.ErrorInvalidArgument) {
		t.Fatalf("depth bound error = %v", err)
	}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := lifecycle.Client.Documents(canceled); !shoal.IsErrorCode(err, shoal.ErrorCanceled) {
		t.Fatalf("canceled error = %v", err)
	}
	expired, cancelExpired := context.WithDeadline(
		ctx, time.Now().Add(-time.Second))
	defer cancelExpired()
	if _, err := lifecycle.Client.Documents(expired); !shoal.IsErrorCode(err, shoal.ErrorDeadline) {
		t.Fatalf("deadline error = %v", err)
	}
}

func unsupportedRetrieval(t *testing.T, factory ClientFactory) {
	lifecycle := openLifecycle(t, factory, standardControls())
	ctx := context.Background()
	if _, err := lifecycle.Client.Retrieve(ctx, retrieval.Request{
		Text: "query", Modes: []retrieval.Mode{retrieval.ModeVector},
	}); !shoal.IsErrorCode(err, shoal.ErrorUnavailable) {
		t.Fatalf("vector error = %v", err)
	}
	if _, err := lifecycle.Client.Retrieve(ctx, retrieval.Request{
		Text: "query",
		AsOf: time.Date(2026, time.August, 25, 12, 0, 0, 0, time.UTC),
	}); !shoal.IsErrorCode(err, shoal.ErrorUnavailable) {
		t.Fatalf("as-of error = %v", err)
	}
	for _, mode := range []retrieval.Mode{
		retrieval.ModeTree, retrieval.ModeGraph,
	} {
		if _, err := lifecycle.Client.Retrieve(ctx, retrieval.Request{
			Text: "query", Modes: []retrieval.Mode{mode},
		}); !shoal.IsErrorCode(err, shoal.ErrorUnavailable) {
			t.Fatalf("%s seed error = %v", mode, err)
		}
	}
}

func restart(t *testing.T, factory ClientFactory) {
	lifecycle := openLifecycle(t, factory, standardControls())
	ctx := context.Background()
	source := explorer.Source{
		URI: "file:///restart.txt", MediaType: explorer.MediaTypeText,
		Content:  "restart preserves public values",
		Metadata: shoal.Metadata{"restart": "true"},
	}
	ingested, err := lifecycle.Client.Ingest(ctx, source)
	if err != nil {
		t.Fatal(err)
	}
	edge := graph.Edge{
		ID: "restart-edge", From: ingested.Document.ID, To: ingested.Document.ID,
		Type: "restart_link", Weight: 1,
	}
	if err := lifecycle.Client.Connect(ctx, edge); err != nil {
		t.Fatal(err)
	}
	request := retrieval.Request{Text: "restart preserves", TopK: 10}
	before, err := lifecycle.Client.Retrieve(ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	restartLifecycle(t, lifecycle)
	documents, err := lifecycle.Client.Documents(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(documents) != 1 ||
		documents[0].Revision.ID != ingested.Revision.ID {
		t.Fatalf("restarted documents = %+v", documents)
	}
	view, err := lifecycle.Client.Document(
		ctx, ingested.Document.ID, ingested.Revision.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !viewContains(view, source.Content) ||
		!reflect.DeepEqual(view.Document.Metadata, source.Metadata) {
		t.Fatalf("restarted document = %+v", view)
	}
	after, err := lifecycle.Client.Retrieve(ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(after, before) {
		t.Fatalf("retrieval changed across restart: %+v != %+v", before, after)
	}
	neighborhood, err := lifecycle.Client.Neighborhood(
		ctx, explorer.NeighborhoodRequest{
			NodeIDs:   []shoal.ID{ingested.Document.ID},
			EdgeTypes: []string{"restart_link"},
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(neighborhood.Edges) != 1 || neighborhood.Edges[0].ID != edge.ID {
		t.Fatalf("restarted neighborhood = %+v", neighborhood)
	}
	unchanged, err := lifecycle.Client.Ingest(ctx, source)
	if err != nil {
		t.Fatal(err)
	}
	if unchanged.Disposition != explorer.IngestUnchanged ||
		unchanged.Revision.ID != ingested.Revision.ID {
		t.Fatalf("restarted idempotent ingest = %+v", unchanged)
	}
	// Once superseded, the same content is historical: a successful result
	// that does not make the earlier revision current again (#674).
	superseding := source
	superseding.Content = "restart superseding revision"
	newer, err := lifecycle.Client.Ingest(ctx, superseding)
	if err != nil || newer.Disposition != explorer.IngestApplied {
		t.Fatalf("superseding ingest = %+v, %v", newer, err)
	}
	historical, err := lifecycle.Client.Ingest(ctx, source)
	if err != nil {
		t.Fatal(err)
	}
	if historical.Disposition != explorer.IngestHistorical ||
		historical.Revision.ID != ingested.Revision.ID {
		t.Fatalf("superseded ingest = %+v, want historical", historical)
	}
	documents, err = lifecycle.Client.Documents(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(documents) != 1 || documents[0].Revision.ID != newer.Revision.ID {
		t.Fatalf("a historical ingest changed the current revision: %+v", documents)
	}
}

func openLifecycle(
	t *testing.T, factory ClientFactory, controls FixtureControls,
) *Lifecycle {
	t.Helper()
	normalized, err := controls.Normalize()
	if err != nil {
		t.Fatalf("normalize Explorer fixture controls: %v", err)
	}
	lifecycle, err := factory(t, normalized)
	if err != nil {
		t.Fatalf("open Explorer client: %v", err)
	}
	if lifecycle.Client == nil || lifecycle.IngestAt == nil ||
		lifecycle.Restart == nil || lifecycle.Close == nil {
		t.Fatal("Explorer lifecycle requires client, exact-time ingest, restart, and close hooks")
	}
	t.Cleanup(func() {
		if err := lifecycle.Close(); err != nil {
			t.Errorf("close Explorer client: %v", err)
		}
	})
	return &lifecycle
}

func standardControls() FixtureControls {
	return FixtureControls{
		Clock: FakeClock{Instants: []time.Time{
			time.Date(
				2026, time.August, 26, 12, 0, 0, 0,
				time.FixedZone("fixture", -4*60*60),
			),
		}},
		Authorities: WriterAuthorityHistory{{
			Generation: 1,
			Mode:       WriterAuthorityEmbeddedPrimary,
			Holder:     "embedded-fixture",
			Fence:      1,
		}},
	}
}

func restartLifecycle(t *testing.T, lifecycle *Lifecycle) {
	t.Helper()
	client, err := lifecycle.Restart(context.Background())
	if err != nil {
		t.Fatalf("restart Explorer client: %v", err)
	}
	if client == nil {
		t.Fatal("restart returned a nil Explorer client")
	}
	lifecycle.Client = client
}

func flattenView(root explorer.SectionView) ([]document.Section, []document.Span) {
	sections := []document.Section{root.Section}
	spans := append([]document.Span(nil), root.Spans...)
	for _, child := range root.Children {
		childSections, childSpans := flattenView(child)
		sections = append(sections, childSections...)
		spans = append(spans, childSpans...)
	}
	return sections, spans
}

func viewContains(view explorer.DocumentView, text string) bool {
	_, spans := flattenView(view.Root)
	for _, span := range spans {
		if span.Text == text || strings.Contains(span.Text, text) {
			return true
		}
	}
	return false
}

func assertNodeOrder(t *testing.T, nodes []graph.Node) {
	t.Helper()
	for index := 1; index < len(nodes); index++ {
		if shoal.CompareID(nodes[index-1].ID, nodes[index].ID) >= 0 {
			t.Fatalf("nodes are not in raw ID order: %+v", nodes)
		}
	}
}

func assertResultOrder(t *testing.T, results []retrieval.Result) {
	t.Helper()
	for index := 1; index < len(results); index++ {
		if retrieval.CompareResult(results[index-1], results[index]) >= 0 {
			t.Fatalf("results are not in deterministic order: %+v", results)
		}
	}
	for _, result := range results {
		for index := 1; index < len(result.Evidence); index++ {
			if retrieval.CompareEvidence(
				result.Evidence[index-1], result.Evidence[index],
			) >= 0 {
				t.Fatalf("evidence is not in deterministic order: %+v", result.Evidence)
			}
		}
	}
}
