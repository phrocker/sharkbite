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

package explorer_test

import (
	"context"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/phrocker/shoal-oss/pkg/explorer"
	"github.com/phrocker/shoal-oss/pkg/graph"
	"github.com/phrocker/shoal-oss/pkg/model"
	"github.com/phrocker/shoal-oss/pkg/retrieval"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

const sampleMarkdown = `# Operations Guide

Use bounded retries for transient failures.

## Retry policy

Retry requests three times with exponential backoff.

### Timeouts

Each attempt has a five second timeout.
`

func TestIngestExploreRetrievePersists(t *testing.T) {
	ctx := context.Background()
	dataDir := t.TempDir()
	corpus, err := explorer.Open(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	source := explorer.Source{
		URI:       "file:///operations.md",
		MediaType: explorer.MediaTypeMarkdown,
		Content:   sampleMarkdown,
	}
	first, err := corpus.Ingest(ctx, source)
	if err != nil {
		t.Fatal(err)
	}
	if first.Disposition != explorer.IngestApplied {
		t.Fatalf("first disposition = %q", first.Disposition)
	}
	second, err := corpus.Ingest(ctx, source)
	if err != nil {
		t.Fatal(err)
	}
	if second.Disposition != explorer.IngestUnchanged ||
		second.Revision.ID != first.Revision.ID {
		t.Fatalf("idempotent ingest = %+v", second)
	}

	view, err := corpus.Document(ctx, first.Document.ID, "")
	if err != nil {
		t.Fatal(err)
	}
	if view.Root.Section.Heading != "Operations Guide" ||
		len(view.Root.Children) != 1 ||
		view.Root.Children[0].Section.Heading != "Operations Guide" ||
		len(view.Root.Children[0].Children) != 1 ||
		view.Root.Children[0].Children[0].Section.Heading != "Retry policy" {
		t.Fatalf("unexpected outline: %+v", view.Root)
	}

	response, err := corpus.Retrieve(ctx, retrieval.Request{
		Text: "exponential backoff",
		TopK: 3,
		Modes: []retrieval.Mode{
			retrieval.ModeLexical, retrieval.ModeTree, retrieval.ModeGraph,
		},
		Explain: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(response.Results) != 1 {
		t.Fatalf("results = %+v", response.Results)
	}
	result := response.Results[0]
	if len(result.Evidence) != 1 ||
		result.Evidence[0].Quote != "Retry requests three times with exponential backoff." {
		t.Fatalf("evidence = %+v", result.Evidence)
	}
	citation := result.Evidence[0].Citation
	if err := citation.Validate(); err != nil {
		t.Fatalf("citation: %v", err)
	}
	start, end := citation.Range.Start.Offset, citation.Range.End.Offset
	if got := sampleMarkdown[int(start):int(end)]; got != result.Evidence[0].Quote {
		t.Fatalf("citation slice = %q, quote = %q", got, result.Evidence[0].Quote)
	}
	if err := result.Evidence[0].Path.Validate(); err != nil {
		t.Fatalf("path: %v", err)
	}
	if result.Explanation == nil ||
		result.Explanation.Scores[string(retrieval.ModeGraph)] == 0 {
		t.Fatalf("explanation = %+v", result.Explanation)
	}
	if err := corpus.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := explorer.Open(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	documents, err := reopened.Documents(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(documents) != 1 || documents[0].Revision.ID != first.Revision.ID {
		t.Fatalf("reopened documents = %+v", documents)
	}
	reopenedResponse, err := reopened.Retrieve(ctx, retrieval.Request{
		Text: "bounded retries", TopK: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(reopenedResponse.Results) != 1 {
		t.Fatalf("reopened results = %+v", reopenedResponse.Results)
	}
}

func TestCrossDocumentNeighborhoodPersists(t *testing.T) {
	ctx := context.Background()
	dataDir := t.TempDir()
	corpus, err := explorer.Open(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	first, err := corpus.Ingest(ctx, explorer.Source{
		URI: "file:///a.txt", MediaType: explorer.MediaTypeText,
		Content: "Service A calls Service B.",
	})
	if err != nil {
		t.Fatal(err)
	}
	second, err := corpus.Ingest(ctx, explorer.Source{
		URI: "file:///b.txt", MediaType: explorer.MediaTypeText,
		Content: "Service B stores account records.",
	})
	if err != nil {
		t.Fatal(err)
	}
	edge := graph.Edge{
		ID: "service-dependency", From: first.Document.ID, To: second.Document.ID,
		Type: "depends_on", Weight: 1,
	}
	if err := corpus.Connect(ctx, edge); err != nil {
		t.Fatal(err)
	}
	if err := corpus.Connect(ctx, edge); err != nil {
		t.Fatalf("idempotent connect: %v", err)
	}
	if err := corpus.Close(); err != nil {
		t.Fatal(err)
	}
	corpus, err = explorer.Open(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	defer corpus.Close()
	neighborhood, err := corpus.Neighborhood(ctx, explorer.NeighborhoodRequest{
		NodeIDs:   []shoal.ID{first.Document.ID},
		Depth:     1,
		EdgeTypes: []string{"depends_on"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(neighborhood.Nodes) != 2 || len(neighborhood.Edges) != 1 ||
		neighborhood.Edges[0].ID != edge.ID {
		t.Fatalf("neighborhood = %+v", neighborhood)
	}
	incoming, err := corpus.Neighborhood(ctx, explorer.NeighborhoodRequest{
		NodeIDs:   []shoal.ID{second.Document.ID},
		EdgeTypes: []string{"depends_on"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(incoming.Nodes) != 2 || len(incoming.Edges) != 1 ||
		incoming.Edges[0].ID != edge.ID {
		t.Fatalf("incoming neighborhood = %+v", incoming)
	}
	if shoal.CompareID(incoming.Nodes[0].ID, incoming.Nodes[1].ID) >= 0 {
		t.Fatalf("nodes are not in raw ID order: %+v", incoming.Nodes)
	}
	combined, err := corpus.Neighborhood(ctx, explorer.NeighborhoodRequest{
		NodeIDs: []shoal.ID{first.Document.ID, second.Document.ID},
	})
	if err != nil {
		t.Fatal(err)
	}
	for index := 1; index < len(combined.Nodes); index++ {
		if shoal.CompareID(combined.Nodes[index-1].ID, combined.Nodes[index].ID) >= 0 {
			t.Fatalf("combined nodes are not in raw ID order: %+v", combined.Nodes)
		}
	}
	for index := 1; index < len(combined.Edges); index++ {
		if shoal.CompareID(combined.Edges[index-1].ID, combined.Edges[index].ID) >= 0 {
			t.Fatalf("combined edges are not in raw ID order: %+v", combined.Edges)
		}
	}
}

func TestVectorModeFailsExplicitly(t *testing.T) {
	corpus, err := explorer.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}

	defer corpus.Close()
	_, err = corpus.Retrieve(context.Background(), retrieval.Request{
		Text: "query", Modes: []retrieval.Mode{retrieval.ModeVector},
	})
	if !shoal.IsErrorCode(err, shoal.ErrorUnavailable) {
		t.Fatalf("expected unavailable, got %v", err)
	}
}

func TestVectorRetrievalWithFakeEmbedderPersistsExactEvidence(t *testing.T) {
	ctx := context.Background()
	dataDir := t.TempDir()
	corpus := openVectorCorpus(t, dataDir, "unit", 8)
	source := explorer.Source{
		URI:       "file:///vector.md",
		MediaType: explorer.MediaTypeMarkdown,
		Content:   "# Vector\n\nAlpha beta semantic target.\n\n## Other\n\nUnrelated filler text.",
	}
	ingested, err := corpus.Ingest(ctx, source)
	if err != nil {
		t.Fatal(err)
	}

	response, err := corpus.Retrieve(ctx, retrieval.Request{
		Text: "Alpha beta semantic target.",
		TopK: 1,
		Modes: []retrieval.Mode{
			retrieval.ModeVector,
		},
		Explain: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	assertVectorExactEvidence(t, source.Content, response, ingested.Document.ID)
	if response.Results[0].Explanation == nil ||
		response.Results[0].Explanation.Scores[string(retrieval.ModeVector)] <= 0.99 {
		t.Fatalf("vector explanation = %+v", response.Results[0].Explanation)
	}
	if err := corpus.Close(); err != nil {
		t.Fatal(err)
	}

	reopened := openVectorCorpus(t, dataDir, "unit", 8)
	defer reopened.Close()
	reopenedResponse, err := reopened.Retrieve(ctx, retrieval.Request{
		Text: "Alpha beta semantic target.",
		TopK: 1,
		Modes: []retrieval.Mode{
			retrieval.ModeVector,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	assertVectorExactEvidence(t, source.Content, reopenedResponse, ingested.Document.ID)
	if reopenedResponse.Results[0].ID != response.Results[0].ID {
		t.Fatalf("persisted vector result changed: %#v then %#v", response, reopenedResponse)
	}
}

func TestHybridVectorRankingIsDeterministic(t *testing.T) {
	ctx := context.Background()
	corpus := openVectorCorpus(t, t.TempDir(), "determinism", 12)
	defer corpus.Close()
	if _, err := corpus.Ingest(ctx, explorer.Source{
		URI: "file:///hybrid.md", MediaType: explorer.MediaTypeMarkdown,
		Content: "# Hybrid\n\nRepeatable ranking target.\n\n## Notes\n\nRepeatable lexical heading.",
	}); err != nil {
		t.Fatal(err)
	}
	request := retrieval.Request{
		Text: "Repeatable ranking target.",
		TopK: 5,
		Modes: []retrieval.Mode{
			retrieval.ModeVector, retrieval.ModeLexical,
			retrieval.ModeTree, retrieval.ModeGraph,
		},
		Explain: true,
	}
	first, err := corpus.Retrieve(ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 5; i++ {
		next, err := corpus.Retrieve(ctx, request)
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(next, first) {
			t.Fatalf("hybrid response changed on run %d:\n%#v\n%#v", i, first, next)
		}
	}
}

func TestVectorRetrievalRejectsIncompatibleEmbeddingSpaces(t *testing.T) {
	ctx := context.Background()
	dataDir := t.TempDir()
	corpus := openVectorCorpus(t, dataDir, "space-a", 8)
	if _, err := corpus.Ingest(ctx, explorer.Source{
		URI:       "file:///space-a.txt",
		MediaType: explorer.MediaTypeText,
		Content:   "same embedding space",
	}); err != nil {
		t.Fatal(err)
	}
	if err := corpus.Close(); err != nil {
		t.Fatal(err)
	}

	modelMismatch := openVectorCorpus(t, dataDir, "space-b", 8)
	_, err := modelMismatch.Retrieve(ctx, retrieval.Request{
		Text:  "same embedding space",
		Modes: []retrieval.Mode{retrieval.ModeVector},
	})
	if !shoal.IsErrorCode(err, shoal.ErrorConflict) {
		t.Fatalf("model mismatch error = %v", err)
	}
	if err := modelMismatch.Close(); err != nil {
		t.Fatal(err)
	}

	dimensionMismatch := openVectorCorpus(t, dataDir, "space-a", 16)
	defer dimensionMismatch.Close()
	_, err = dimensionMismatch.Ingest(ctx, explorer.Source{
		URI:       "file:///space-b.txt",
		MediaType: explorer.MediaTypeText,
		Content:   "different embedding dimensions",
	})
	if !shoal.IsErrorCode(err, shoal.ErrorConflict) {
		t.Fatalf("dimension mismatch error = %v", err)
	}
}

func TestVectorRetrievalUsesEmbeddingSpaceIdentity(t *testing.T) {
	ctx := context.Background()
	dataDir := t.TempDir()
	first, err := model.NewLexicalEmbedder(model.LexicalConfig{
		Dimensions:   16,
		MaxTextBytes: 1024,
	})
	if err != nil {
		t.Fatal(err)
	}
	corpus, err := explorer.OpenWithOptions(dataDir, explorer.Options{Embedder: first})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := corpus.Ingest(ctx, explorer.Source{
		URI:       "file:///lexical-space.txt",
		MediaType: explorer.MediaTypeText,
		Content:   "lexical embedding space survives cache-only configuration changes",
	}); err != nil {
		t.Fatal(err)
	}
	if err := corpus.Close(); err != nil {
		t.Fatal(err)
	}

	sameSpace, err := model.NewLexicalEmbedder(model.LexicalConfig{
		Dimensions:   16,
		MaxTextBytes: 2048,
	})
	if err != nil {
		t.Fatal(err)
	}
	reopened, err := explorer.OpenWithOptions(dataDir, explorer.Options{Embedder: sameSpace})
	if err != nil {
		t.Fatal(err)
	}
	response, err := reopened.Retrieve(ctx, retrieval.Request{
		Text:  "lexical embedding space",
		Modes: []retrieval.Mode{retrieval.ModeVector},
	})
	if err != nil {
		t.Fatalf("cache-only configuration change was treated as incompatible: %v", err)
	}
	identity, err := sameSpace.EmbeddingSpaceIdentity()
	if err != nil {
		t.Fatal(err)
	}
	if identity == "" || len(response.Results) == 0 {
		t.Fatalf("vector retrieval did not use persisted space %q: %+v",
			identity, response)
	}
	if err := reopened.Close(); err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		name     string
		embedder model.Embedder
	}{
		{
			name: "provider",
			embedder: spaceIdentityEmbedder{
				provider: "other-provider", modelName: model.DefaultLexicalModel,
				dimensions: 16, identity: "provider=other-provider|model=hashing-lexical-v1|dim=16|norm=l2",
			},
		},
		{
			name: "model",
			embedder: spaceIdentityEmbedder{
				provider: "local-lexical", modelName: "other-model",
				dimensions: 16, identity: "provider=local-lexical|model=other-model|dim=16|norm=l2",
			},
		},
		{
			name: "dimension",
			embedder: spaceIdentityEmbedder{
				provider: "local-lexical", modelName: model.DefaultLexicalModel,
				dimensions: 32, identity: "provider=local-lexical|model=hashing-lexical-v1|dim=32|norm=l2",
			},
		},
		{
			name: "normalization",
			embedder: spaceIdentityEmbedder{
				provider: "local-lexical", modelName: model.DefaultLexicalModel,
				dimensions: 16, identity: "provider=local-lexical|model=hashing-lexical-v1|dim=16|norm=none",
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			corpus, err := explorer.OpenWithOptions(dataDir, explorer.Options{
				Embedder: tc.embedder,
			})
			if err != nil {
				t.Fatal(err)
			}
			defer corpus.Close()
			_, err = corpus.Retrieve(ctx, retrieval.Request{
				Text:  "lexical embedding space",
				Modes: []retrieval.Mode{retrieval.ModeVector},
			})
			if !shoal.IsErrorCode(err, shoal.ErrorConflict) {
				t.Fatalf("%s mismatch error = %v", tc.name, err)
			}
		})
	}
}

func TestVectorRetrievalReportsDisabledAndPartiallyPopulatedEmbeddings(t *testing.T) {
	ctx := context.Background()
	dataDir := t.TempDir()
	corpus, err := explorer.Open(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	legacy, err := corpus.Ingest(ctx, explorer.Source{
		URI:       "file:///legacy.txt",
		MediaType: explorer.MediaTypeText,
		Content:   "legacy document without embeddings",
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = corpus.Retrieve(ctx, retrieval.Request{
		Text:  "legacy",
		Modes: []retrieval.Mode{retrieval.ModeVector},
	})
	if !shoal.IsErrorCode(err, shoal.ErrorUnavailable) {
		t.Fatalf("disabled vector error = %v", err)
	}
	if err := corpus.Close(); err != nil {
		t.Fatal(err)
	}

	enabled := openVectorCorpus(t, dataDir, "partial", 8)
	defer enabled.Close()
	current, err := enabled.Ingest(ctx, explorer.Source{
		URI:       "file:///current.txt",
		MediaType: explorer.MediaTypeText,
		Content:   "current vector enabled document",
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = enabled.Retrieve(ctx, retrieval.Request{
		Text:  "current vector enabled document",
		Modes: []retrieval.Mode{retrieval.ModeVector},
	})
	if !shoal.IsErrorCode(err, shoal.ErrorUnavailable) {
		t.Fatalf("partial embeddings error = %v", err)
	}
	scoped, err := enabled.Retrieve(ctx, retrieval.Request{
		Text:  "current vector enabled document",
		Modes: []retrieval.Mode{retrieval.ModeVector},
		Scope: retrieval.Scope{DocumentIDs: []shoal.ID{
			current.Document.ID,
		}},
	})
	if err != nil {
		t.Fatalf("scoped vector retrieval: %v", err)
	}
	if len(scoped.Results) != 1 || scoped.Results[0].Evidence[0].Citation.DocumentID !=
		current.Document.ID {
		t.Fatalf("scoped vector response = %+v, legacy = %s", scoped, legacy.Document.ID)
	}
}

func TestVectorRetrievalNodeScopeIgnoresUnrelatedMissingEmbeddings(t *testing.T) {
	ctx := context.Background()
	dataDir := t.TempDir()
	legacy, err := explorer.Open(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := legacy.Ingest(ctx, explorer.Source{
		URI:       "file:///legacy-node-scope.txt",
		MediaType: explorer.MediaTypeText,
		Content:   "legacy missing embeddings outside node scope",
	}); err != nil {
		t.Fatal(err)
	}
	if err := legacy.Close(); err != nil {
		t.Fatal(err)
	}

	enabled := openVectorCorpus(t, dataDir, "node-scope", 8)
	defer enabled.Close()
	current, err := enabled.Ingest(ctx, explorer.Source{
		URI:       "file:///current-node-scope.txt",
		MediaType: explorer.MediaTypeText,
		Content:   "current scoped vector evidence",
	})
	if err != nil {
		t.Fatal(err)
	}
	view, err := enabled.Document(ctx, current.Document.ID, current.Revision.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(view.Root.Spans) != 1 {
		t.Fatalf("test document spans = %#v", view.Root.Spans)
	}
	response, err := enabled.Retrieve(ctx, retrieval.Request{
		Text:  "current scoped vector evidence",
		Modes: []retrieval.Mode{retrieval.ModeVector},
		Scope: retrieval.Scope{NodeIDs: []shoal.ID{
			view.Root.Spans[0].ID,
		}},
	})
	if err != nil {
		t.Fatalf("node-scoped vector retrieval: %v", err)
	}
	if len(response.Results) != 1 ||
		response.Results[0].Evidence[0].Citation.SpanID != view.Root.Spans[0].ID {
		t.Fatalf("node-scoped vector response = %#v", response)
	}
}

func TestIdempotentIngestChecksExistingRevisionBeforeEmbedding(t *testing.T) {
	ctx := context.Background()
	dataDir := t.TempDir()
	source := explorer.Source{
		URI:       "file:///retry.txt",
		MediaType: explorer.MediaTypeText,
		Content:   "retry should not need embedding provider",
	}
	corpus, err := explorer.Open(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	first, err := corpus.Ingest(ctx, source)
	if err != nil {
		t.Fatal(err)
	}
	if err := corpus.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := explorer.OpenWithOptions(dataDir, explorer.Options{
		Embedder: failingEmbedder{},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	second, err := reopened.Ingest(ctx, source)
	if err != nil {
		t.Fatalf("idempotent ingest used failing embedder: %v", err)
	}
	if second.Disposition != explorer.IngestUnchanged ||
		second.Revision.ID != first.Revision.ID {
		t.Fatalf("idempotent ingest = %+v, first = %+v", second, first)
	}
}

func TestOpenWithOptionsRejectsEmbedderWithoutStableIdentity(t *testing.T) {
	_, err := explorer.OpenWithOptions(t.TempDir(), explorer.Options{
		Embedder: noIdentityEmbedder{},
	})
	if !shoal.IsErrorCode(err, shoal.ErrorInvalidArgument) {
		t.Fatalf("missing embedding identity error = %v", err)
	}
}

func TestDocumentRejectsOversizedIdentifiers(t *testing.T) {
	corpus, err := explorer.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer corpus.Close()

	_, err = corpus.Document(
		context.Background(),
		shoal.ID(strings.Repeat("x", shoal.MaxIDBytes+1)),
		"",
	)
	if !shoal.IsErrorCode(err, shoal.ErrorInvalidArgument) {
		t.Fatalf("oversized document ID error = %v", err)
	}
	_, err = corpus.Document(
		context.Background(),
		"document",
		shoal.ID(strings.Repeat("x", shoal.MaxIDBytes+1)),
	)
	if !shoal.IsErrorCode(err, shoal.ErrorInvalidArgument) {
		t.Fatalf("oversized revision ID error = %v", err)
	}
}

func TestUnseededStandaloneTreeAndGraphFailExplicitly(t *testing.T) {
	corpus, err := explorer.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer corpus.Close()
	for _, mode := range []retrieval.Mode{
		retrieval.ModeTree, retrieval.ModeGraph,
	} {
		_, err := corpus.Retrieve(context.Background(), retrieval.Request{
			Text: "query", Modes: []retrieval.Mode{mode},
		})
		if !shoal.IsErrorCode(err, shoal.ErrorUnavailable) {
			t.Fatalf("%s error = %v", mode, err)
		}
	}
}

func TestRetrieveNormalizationKeepsIdentityAndScoresStable(t *testing.T) {
	corpus, err := explorer.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer corpus.Close()
	if _, err := corpus.Ingest(context.Background(), explorer.Source{
		URI: "file:///normalize.txt", MediaType: explorer.MediaTypeText,
		Content: "stable lexical score",
	}); err != nil {
		t.Fatal(err)
	}
	requests := []retrieval.Request{
		{Text: "stable"},
		{
			Text: "stable", TopK: retrieval.DefaultTopK,
			Modes: []retrieval.Mode{retrieval.ModeLexical},
		},
		{
			Text: "stable", TopK: retrieval.DefaultTopK,
			Modes: []retrieval.Mode{
				retrieval.ModeLexical, retrieval.ModeLexical,
			},
		},
	}
	var first retrieval.Response
	for index, request := range requests {
		response, err := corpus.Retrieve(context.Background(), request)
		if err != nil {
			t.Fatal(err)
		}
		if index == 0 {
			first = response
			continue
		}
		if response.RequestID != first.RequestID ||
			len(response.Results) != len(first.Results) ||
			response.Results[0].Score != first.Results[0].Score {
			t.Fatalf("normalized response %d = %#v, first = %#v", index, response, first)
		}
	}
}

func TestRevisionIdentityPreservesComponentBoundaries(t *testing.T) {
	ctx := context.Background()
	corpus, err := explorer.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer corpus.Close()

	first, err := corpus.Ingest(ctx, explorer.Source{
		URI: "file:///collision.txt", Title: "a\x00b",
		MediaType: explorer.MediaTypeText, Content: "c",
	})
	if err != nil {
		t.Fatal(err)
	}
	second, err := corpus.Ingest(ctx, explorer.Source{
		URI: "file:///collision.txt", Title: "a",
		MediaType: explorer.MediaTypeText, Content: "b\x00c",
	})
	if err != nil {
		t.Fatal(err)
	}
	if first.Revision.ID == second.Revision.ID {
		t.Fatalf("distinct component sequences produced revision %q", first.Revision.ID)
	}
	if second.Disposition != explorer.IngestApplied {
		t.Fatalf("second disposition = %q", second.Disposition)
	}
}

func TestRetrieveEnforcesMaximumTopK(t *testing.T) {
	corpus, err := explorer.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer corpus.Close()
	if _, err := corpus.Ingest(context.Background(), explorer.Source{
		URI: "file:///topk.txt", MediaType: explorer.MediaTypeText,
		Content: "maximum result limit",
	}); err != nil {
		t.Fatal(err)
	}
	response, err := corpus.Retrieve(context.Background(), retrieval.Request{
		Text: "maximum", TopK: retrieval.MaxTopK,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(response.Results) != 1 {
		t.Fatalf("results = %+v", response.Results)
	}
	if _, err := corpus.Retrieve(context.Background(), retrieval.Request{
		Text: "maximum", TopK: retrieval.MaxTopK + 1,
	}); !shoal.IsErrorCode(err, shoal.ErrorInvalidArgument) {
		t.Fatalf("oversized TopK error = %v", err)
	}
}

func TestRequestIdentityPreservesScopeBoundariesAndAllFields(t *testing.T) {
	corpus, err := explorer.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer corpus.Close()
	ctx := context.Background()
	base := retrieval.Request{
		Text:  "query",
		TopK:  1,
		Modes: []retrieval.Mode{retrieval.ModeLexical},
		Scope: retrieval.Scope{
			DocumentIDs: []shoal.ID{"a b", "c"},
			NodeIDs:     []shoal.ID{"node"},
		},
	}
	baseResponse, err := corpus.Retrieve(ctx, base)
	if err != nil {
		t.Fatal(err)
	}

	scopeCollision := base
	scopeCollision.Scope.DocumentIDs = []shoal.ID{"a", "b c"}
	explainChange := base
	explainChange.Explain = true
	topKChange := base
	topKChange.TopK = 2
	modeChange := base
	modeChange.Modes = []retrieval.Mode{retrieval.ModeTree}
	nodeChange := base
	nodeChange.Scope.NodeIDs = []shoal.ID{"other"}
	for name, request := range map[string]retrieval.Request{
		"scope boundaries": scopeCollision,
		"explain":          explainChange,
		"top k":            topKChange,
		"modes":            modeChange,
		"node scope":       nodeChange,
	} {
		response, err := corpus.Retrieve(ctx, request)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if response.RequestID == baseResponse.RequestID {
			t.Errorf("%s did not change request ID %q", name, response.RequestID)
		}
	}

	equivalent := base
	equivalent.Modes = []retrieval.Mode{
		retrieval.ModeLexical, retrieval.ModeLexical,
	}
	equivalent.Scope.DocumentIDs = []shoal.ID{"a b", "c", "a b"}
	equivalent.Scope.NodeIDs = []shoal.ID{"node", "node"}
	equivalentResponse, err := corpus.Retrieve(ctx, equivalent)
	if err != nil {
		t.Fatal(err)
	}
	if equivalentResponse.RequestID != baseResponse.RequestID {
		t.Fatalf(
			"equivalent requests have IDs %q and %q",
			equivalentResponse.RequestID, baseResponse.RequestID,
		)
	}
}

func TestExplanationReportsWeightedModeContribution(t *testing.T) {
	corpus, err := explorer.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer corpus.Close()
	ingested, err := corpus.Ingest(context.Background(), explorer.Source{
		URI: "file:///weighted.md", MediaType: explorer.MediaTypeMarkdown,
		Content: "# Heading\n\nEvidence without the relationship term.\n",
	})
	if err != nil {
		t.Fatal(err)
	}
	response, err := corpus.Retrieve(context.Background(), retrieval.Request{
		Text: "contains", TopK: 1,
		Modes: []retrieval.Mode{retrieval.ModeGraph},
		Scope: retrieval.Scope{
			NodeIDs: []shoal.ID{ingested.Document.ID},
		},
		Explain: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(response.Results) != 1 {
		t.Fatalf("results = %+v", response.Results)
	}
	result := response.Results[0]
	if result.Explanation == nil {
		t.Fatal("missing explanation")
	}
	if got := result.Explanation.Scores[string(retrieval.ModeGraph)]; got != result.Score {
		t.Fatalf("graph contribution = %v, result score = %v", got, result.Score)
	}
	if result.Score != shoal.Score(0.25) {
		t.Fatalf("weighted graph score = %v", result.Score)
	}
}

func TestRetrieveRejectsAsOfWithoutPublicationFrontier(t *testing.T) {
	corpus, err := explorer.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer corpus.Close()
	_, err = corpus.Retrieve(context.Background(), retrieval.Request{
		Text: "query",
		AsOf: time.Date(2026, 8, 25, 15, 0, 0, 0, time.UTC),
	})
	if !shoal.IsErrorCode(err, shoal.ErrorUnavailable) {
		t.Fatalf("AsOf error = %v", err)
	}
}

func TestNeighborhoodRequestNormalization(t *testing.T) {
	request := explorer.NeighborhoodRequest{
		NodeIDs: []shoal.ID{"node\x00", "node\x00", shoal.ID("\xff")},
		EdgeTypes: []string{
			"links", "links",
		},
	}
	normalized, err := request.Normalize()
	if err != nil {
		t.Fatal(err)
	}
	if normalized.Depth != 1 ||
		len(normalized.NodeIDs) != 2 ||
		len(normalized.EdgeTypes) != 1 {
		t.Fatalf("normalized request = %#v", normalized)
	}
	normalized.NodeIDs[0] = "changed"
	if request.NodeIDs[0] != "node\x00" {
		t.Fatal("Normalize mutated neighborhood request")
	}
	request.Depth = 17
	if _, err := request.Normalize(); !shoal.IsErrorCode(
		err, shoal.ErrorInvalidArgument,
	) {
		t.Fatalf("depth error = %v", err)
	}
	for _, edgeType := range []string{" \t ", string([]byte{0xff})} {
		request := explorer.NeighborhoodRequest{
			NodeIDs:   []shoal.ID{"node"},
			EdgeTypes: []string{edgeType},
		}
		if _, err := request.Normalize(); !shoal.IsErrorCode(
			err, shoal.ErrorInvalidArgument,
		) {
			t.Fatalf("edge type %q error = %v", edgeType, err)
		}
	}
}

func TestMarkdownHeadingsPreserveHashesAndIgnoreFences(t *testing.T) {
	ctx := context.Background()
	corpus, err := explorer.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer corpus.Close()

	result, err := corpus.Ingest(ctx, explorer.Source{
		URI:       "file:///languages.md",
		MediaType: explorer.MediaTypeMarkdown,
		Content:   "# C#\n\n```markdown\n# Not a section\n```\n\n## Runtime ###\n\nDetails.\n",
	})
	if err != nil {
		t.Fatal(err)
	}
	view, err := corpus.Document(ctx, result.Document.ID, result.Revision.ID)
	if err != nil {
		t.Fatal(err)
	}
	if view.Document.Title != "C#" {
		t.Fatalf("document title = %q", view.Document.Title)
	}
	if len(view.Root.Children) != 1 {
		t.Fatalf("root children = %+v", view.Root.Children)
	}
	top := view.Root.Children[0]
	if top.Section.Heading != "C#" || len(top.Children) != 1 ||
		top.Children[0].Section.Heading != "Runtime" {
		t.Fatalf("outline = %+v", view.Root)
	}
	if got := top.Spans[0].Text; !strings.Contains(got, "# Not a section") {
		t.Fatalf("fenced content span = %q", got)
	}
}

func openVectorCorpus(
	t *testing.T, dataDir, modelName string, dimensions int,
) *explorer.Explorer {
	t.Helper()
	corpus, err := explorer.OpenWithOptions(dataDir, explorer.Options{
		Embedder: model.FakeEmbedder{Model: modelName, Dimensions: dimensions},
	})
	if err != nil {
		t.Fatal(err)
	}
	return corpus
}

func assertVectorExactEvidence(
	t *testing.T,
	source string,
	response retrieval.Response,
	documentID shoal.ID,
) {
	t.Helper()
	if len(response.Results) != 1 {
		t.Fatalf("vector results = %+v", response.Results)
	}
	result := response.Results[0]
	if len(result.Evidence) != 1 {
		t.Fatalf("vector evidence = %+v", result.Evidence)
	}
	evidence := result.Evidence[0]
	if evidence.Citation.DocumentID != documentID ||
		evidence.Quote != "Alpha beta semantic target." {
		t.Fatalf("vector evidence = %+v", evidence)
	}
	start, end := evidence.Citation.Range.Start.Offset, evidence.Citation.Range.End.Offset
	if got := source[int(start):int(end)]; got != evidence.Quote {
		t.Fatalf("citation slice = %q, quote = %q", got, evidence.Quote)
	}
	if err := evidence.Citation.Validate(); err != nil {
		t.Fatalf("citation: %v", err)
	}
	if err := evidence.Path.Validate(); err != nil {
		t.Fatalf("path: %v", err)
	}
}

type failingEmbedder struct{}

func (failingEmbedder) CacheIdentity() (string, error) {
	return "failing-embedder-v1", nil
}

func (failingEmbedder) EmbeddingSpaceIdentity() (string, error) {
	return "failing-embedding-space-v1", nil
}

type noIdentityEmbedder struct{}

func (noIdentityEmbedder) Embed(
	context.Context, model.EmbedRequest,
) (model.EmbedResult, error) {
	return model.EmbedResult{
		Vector: []float32{1},
		Provenance: model.Provenance{
			Provider: "custom",
			Model:    "missing-identity",
		},
	}, nil
}

func (failingEmbedder) Embed(
	context.Context, model.EmbedRequest,
) (model.EmbedResult, error) {
	return model.EmbedResult{}, model.ErrUnavailable
}

type spaceIdentityEmbedder struct {
	provider   string
	modelName  string
	dimensions int
	identity   string
}

func (e spaceIdentityEmbedder) EmbeddingSpaceIdentity() (string, error) {
	return e.identity, nil
}

func (e spaceIdentityEmbedder) Embed(
	context.Context, model.EmbedRequest,
) (model.EmbedResult, error) {
	vector := make([]float32, e.dimensions)
	if len(vector) > 0 {
		vector[0] = 1
	}
	return model.EmbedResult{
		Vector: vector,
		Provenance: model.Provenance{
			Provider: e.provider,
			Model:    e.modelName,
		},
	}, nil
}
