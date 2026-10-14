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
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/phrocker/shoal-oss/pkg/document"
	"github.com/phrocker/shoal-oss/pkg/model"
	"github.com/phrocker/shoal-oss/pkg/retrieval"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

func TestVectorAvailabilityCachesProbeForEmptyCorpus(t *testing.T) {
	ctx := context.Background()
	embedder := &countingEmbedder{}
	corpus, err := OpenWithOptions(t.TempDir(), Options{Embedder: embedder})
	if err != nil {
		t.Fatal(err)
	}
	defer corpus.Close()
	for i := 0; i < 2; i++ {
		available, err := corpus.VectorAvailable(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if !available {
			t.Fatal("configured empty corpus should be vector-capable")
		}
	}
	if embedder.calls != 1 {
		t.Fatalf("capability probes = %d, want 1", embedder.calls)
	}
}

func TestVectorAvailabilityCoalescesConcurrentProbes(t *testing.T) {
	ctx := context.Background()
	embedder := &countingEmbedder{delay: 10 * time.Millisecond}
	corpus, err := OpenWithOptions(t.TempDir(), Options{Embedder: embedder})
	if err != nil {
		t.Fatal(err)
	}
	defer corpus.Close()
	var wait sync.WaitGroup
	errorsSeen := make(chan error, 8)
	for i := 0; i < 8; i++ {
		wait.Add(1)
		go func() {
			defer wait.Done()
			available, err := corpus.VectorAvailable(ctx)
			if err == nil && !available {
				err = shoal.NewError(shoal.ErrorInternal, "vector unavailable")
			}
			errorsSeen <- err
		}()
	}
	wait.Wait()
	close(errorsSeen)
	for err := range errorsSeen {
		if err != nil {
			t.Fatal(err)
		}
	}
	if embedder.calls != 1 {
		t.Fatalf("coalesced capability probes = %d, want 1", embedder.calls)
	}
}

func TestEmbeddingAggregateBoundRejectsUnpersistableIndex(t *testing.T) {
	err := validateEmbeddingAggregateBound(int(maxExplorerEmbeddingBytes/4)+1, 1)
	if !shoal.IsErrorCode(err, shoal.ErrorInvalidArgument) {
		t.Fatalf("aggregate bound error = %v", err)
	}
}

func TestEmbeddingSpanCountBoundStopsBeforeProviderCalls(t *testing.T) {
	embedder := &countingEmbedder{}
	corpus, err := OpenWithOptions(t.TempDir(), Options{Embedder: embedder})
	if err != nil {
		t.Fatal(err)
	}
	defer corpus.Close()
	_, err = corpus.embedParsedSpans(
		context.Background(),
		make([]document.Span, maxExplorerEmbeddingSpans+1),
	)
	if !shoal.IsErrorCode(err, shoal.ErrorInvalidArgument) {
		t.Fatalf("span count bound error = %v", err)
	}
	if embedder.calls != 0 {
		t.Fatalf("provider calls before span bound = %d", embedder.calls)
	}
}

func TestEmbeddingProvenanceStringsAreBounded(t *testing.T) {
	ctx := context.Background()
	embedder := &countingEmbedder{
		model:    strings.Repeat("x", shoal.MaxSemanticStringBytes+1),
		identity: "counting-oversized-provenance",
	}
	corpus, err := OpenWithOptions(t.TempDir(), Options{Embedder: embedder})
	if err != nil {
		t.Fatal(err)
	}
	defer corpus.Close()
	_, err = corpus.Ingest(ctx, Source{
		URI:       "file:///oversized-provenance.txt",
		MediaType: MediaTypeText,
		Content:   "oversized provenance",
	})
	if !shoal.IsErrorCode(err, shoal.ErrorUnavailable) {
		t.Fatalf("oversized provenance error = %v", err)
	}

	_, err = OpenWithOptions(t.TempDir(), Options{
		Embedder: &countingEmbedder{
			identity: strings.Repeat("i", shoal.MaxSemanticStringBytes+1),
		},
	})
	if !shoal.IsErrorCode(err, shoal.ErrorInvalidArgument) {
		t.Fatalf("oversized identity error = %v", err)
	}
}

func TestEmbeddingSpaceMismatchStopsAfterFirstProviderCall(t *testing.T) {
	ctx := context.Background()
	dataDir := t.TempDir()
	corpus, err := OpenWithOptions(dataDir, Options{
		Embedder: model.FakeEmbedder{Model: "space-a", Dimensions: 8},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := corpus.Ingest(ctx, Source{
		URI:       "file:///space-a.txt",
		MediaType: MediaTypeText,
		Content:   "existing vector space",
	}); err != nil {
		t.Fatal(err)
	}
	if err := corpus.Close(); err != nil {
		t.Fatal(err)
	}

	embedder := &countingEmbedder{model: "space-b", dimensions: 16}
	reopened, err := OpenWithOptions(dataDir, Options{Embedder: embedder})
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	_, err = reopened.Ingest(ctx, Source{
		URI:       "file:///space-b.md",
		MediaType: MediaTypeMarkdown,
		Content:   "# Mismatch\n\nfirst paragraph\n\nsecond paragraph\n\nthird paragraph",
	})
	if !shoal.IsErrorCode(err, shoal.ErrorConflict) {
		t.Fatalf("space mismatch error = %v", err)
	}
	if embedder.calls != 1 {
		t.Fatalf("provider calls before mismatch = %d, want 1", embedder.calls)
	}
}

func TestQueryProviderCountRequiresActualEmbedInvocation(t *testing.T) {
	embedder := &countingEmbedder{
		model: "actual", dimensions: 2, identity: "space-actual",
	}
	corpus := &Explorer{
		embedders: map[string]model.Embedder{
			"space-expected": embedder,
		},
		queryEmbeddingCache: make(map[embeddingQueryCacheKey]cachedQueryEmbedding),
	}
	_, _, cacheHit, providerCalled, err := corpus.cachedEmbedQueryInSpace(
		context.Background(),
		"query",
		persistedEmbeddingProvenance{
			Provider: "counting", Model: "expected",
			Identity: "space-expected", Dimensions: 2,
		},
	)
	if !shoal.IsErrorCode(err, shoal.ErrorConflict) {
		t.Fatalf("identity mismatch error = %v", err)
	}
	if cacheHit || providerCalled || embedder.calls != 0 {
		t.Fatalf(
			"cache/provider/calls = %v/%v/%d, want false/false/0",
			cacheHit, providerCalled, embedder.calls)
	}
}

func TestVectorAvailabilityIncludesHistoricalEmbeddingSpace(t *testing.T) {
	ctx := context.Background()
	dataDir := t.TempDir()
	corpus, err := OpenWithOptions(dataDir, Options{
		Embedder: model.FakeEmbedder{Model: "historical-a", Dimensions: 8},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := corpus.Ingest(ctx, Source{
		URI:       "file:///historical-space.md",
		MediaType: MediaTypeMarkdown,
		Content:   "# Historical\n\nold embedded span",
	}); err != nil {
		t.Fatal(err)
	}
	if err := corpus.Close(); err != nil {
		t.Fatal(err)
	}
	legacy, err := Open(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := legacy.Ingest(ctx, Source{
		URI:       "file:///historical-space.md",
		MediaType: MediaTypeMarkdown,
		Content:   "# Historical\n",
	}); err != nil {
		t.Fatal(err)
	}
	if err := legacy.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := OpenWithOptions(dataDir, Options{
		Embedder: model.FakeEmbedder{Model: "historical-b", Dimensions: 8},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	available, err := reopened.VectorAvailable(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if available {
		t.Fatal("historical embedding mismatch advertised as available")
	}
}

func TestVectorRetrievalRejectsStaleEmbeddings(t *testing.T) {
	ctx := context.Background()
	corpus, err := OpenWithOptions(t.TempDir(), Options{
		Embedder: model.FakeEmbedder{Model: "stale", Dimensions: 8},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer corpus.Close()
	ingested, err := corpus.Ingest(ctx, Source{
		URI:       "file:///stale.txt",
		MediaType: MediaTypeText,
		Content:   "stale vector quote",
	})
	if err != nil {
		t.Fatal(err)
	}

	corpus.mu.Lock()
	record, err := latestRevision(corpus.documents[ingested.Document.ID])
	if err != nil {
		corpus.mu.Unlock()
		t.Fatal(err)
	}
	record.Embeddings.Spans[0].TextDigest = "stale"
	corpus.mu.Unlock()

	available, err := corpus.VectorAvailable(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if available {
		t.Fatal("stale embeddings advertised as vector-capable")
	}
	_, err = corpus.Retrieve(ctx, retrieval.Request{
		Text:  "stale vector quote",
		Modes: []retrieval.Mode{retrieval.ModeVector},
	})
	if !shoal.IsErrorCode(err, shoal.ErrorUnavailable) {
		t.Fatalf("stale vector error = %v", err)
	}
}

func TestClosedExplorerDoesNotCallEmbedderForVectorRequests(t *testing.T) {
	ctx := context.Background()
	embedder := &countingEmbedder{}
	corpus, err := OpenWithOptions(t.TempDir(), Options{Embedder: embedder})
	if err != nil {
		t.Fatal(err)
	}
	if err := corpus.Close(); err != nil {
		t.Fatal(err)
	}
	_, err = corpus.Retrieve(ctx, retrieval.Request{
		Text:  "closed",
		Modes: []retrieval.Mode{retrieval.ModeVector},
	})
	if !shoal.IsErrorCode(err, shoal.ErrorUnavailable) {
		t.Fatalf("closed retrieve error = %v", err)
	}
	_, err = corpus.VectorScores(ctx, VectorScoreRequest{Text: "closed"})
	if !shoal.IsErrorCode(err, shoal.ErrorUnavailable) {
		t.Fatalf("closed vector scores error = %v", err)
	}
	if embedder.calls != 0 {
		t.Fatalf("embedder called after close %d times", embedder.calls)
	}
}

func TestSingleSpaceVectorRetrievalFastPathEmbedsQueryOnce(t *testing.T) {
	ctx := context.Background()
	embedder := &countingEmbedder{model: "single", dimensions: 2, identity: "space-single"}
	corpus, err := OpenWithOptions(t.TempDir(), Options{Embedder: embedder})
	if err != nil {
		t.Fatal(err)
	}
	defer corpus.Close()
	for _, content := range []string{"alpha single vector", "beta single vector"} {
		if _, err := corpus.Ingest(ctx, Source{
			URI:       "file:///" + content + ".txt",
			MediaType: MediaTypeText,
			Content:   content,
		}); err != nil {
			t.Fatal(err)
		}
	}
	before := embedder.calls
	response, err := corpus.Retrieve(ctx, retrieval.Request{
		Text:    "single vector",
		Modes:   []retrieval.Mode{retrieval.ModeVector},
		Explain: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if embedder.calls-before != 1 {
		t.Fatalf("query embeddings = %d, want 1", embedder.calls-before)
	}
	if len(response.Results) == 0 || response.Results[0].Explanation == nil {
		t.Fatalf("missing explanation: %+v", response)
	}
	summary := response.Results[0].Explanation.Summary
	if !strings.Contains(summary, "embedding_spaces=1") ||
		!strings.Contains(summary, "vector_merge=score") ||
		strings.Contains(summary, "unbenchmarked") {
		t.Fatalf("single-space explanation = %q", summary)
	}
}

func TestMixedSpaceVectorRetrievalUsesDeterministicRankFusion(t *testing.T) {
	ctx := context.Background()
	embedA := &countingEmbedder{model: "a", dimensions: 2, identity: "space-a"}
	embedB := &countingEmbedder{model: "b", dimensions: 2, identity: "space-b"}
	var events []EmbeddingQueryEvent
	corpus, err := OpenWithOptions(t.TempDir(), Options{
		Embedder:                embedA,
		EmbeddingProviders:      []model.Embedder{embedB},
		RecallEvidence:          map[string]string{"space-a": "benchmarked"},
		MaxEmbeddingSpaceFanout: 4,
		EmbeddingQueryObserver: func(event EmbeddingQueryEvent) {
			events = append(events, event)
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer corpus.Close()
	first, err := corpus.Ingest(ctx, Source{URI: "file:///a.txt", MediaType: MediaTypeText, Content: "alpha"})
	if err != nil {
		t.Fatal(err)
	}
	second, err := corpus.Ingest(ctx, Source{URI: "file:///b.txt", MediaType: MediaTypeText, Content: "bravo"})
	if err != nil {
		t.Fatal(err)
	}
	corpus.mu.Lock()
	firstRecord := corpus.documents[first.Document.ID][first.Revision.ID]
	secondRecord := corpus.documents[second.Document.ID][second.Revision.ID]
	low, high := firstRecord, secondRecord
	if shoal.CompareID(firstRecord.Spans[0].ID, secondRecord.Spans[0].ID) > 0 {
		low, high = secondRecord, firstRecord
	}
	low.Embeddings.Provenance = persistedEmbeddingProvenance{
		Provider: "counting", Model: "a", Identity: "space-a", Dimensions: 2,
	}
	low.Embeddings.Spans[0].Vector = []float32{0, 1}
	high.Embeddings.Provenance = persistedEmbeddingProvenance{
		Provider: "counting", Model: "b", Identity: "space-b", Dimensions: 2,
	}
	high.Embeddings.Spans[0].Vector = []float32{1, 0}
	wantFirst := low.Spans[0].ID
	corpus.embeddingSpace = embeddingSpaceCache{}
	corpus.mu.Unlock()

	var previous []shoal.ID
	for run := 0; run < 3; run++ {
		response, err := corpus.Retrieve(ctx, retrieval.Request{
			Text:    "query",
			Modes:   []retrieval.Mode{retrieval.ModeVector},
			TopK:    2,
			Explain: true,
		})
		if err != nil {
			t.Fatal(err)
		}
		if got := response.Results[0].ID; got != wantFirst {
			t.Fatalf("first result = %s, want deterministic RRF tiebreak %s", got, wantFirst)
		}
		if response.Results[0].Score != response.Results[1].Score {
			t.Fatalf("rank fusion scores = %v, want equal per-space rank-one scores", response.Results)
		}
		summary := response.Results[0].Explanation.Summary
		if !strings.Contains(summary, "embedding_spaces=2") ||
			!strings.Contains(summary, "vector_merge=rank-fusion") ||
			!strings.Contains(summary, "unbenchmarked; no recall claim") {
			t.Fatalf("mixed-space explanation = %q", summary)
		}
		current := []shoal.ID{response.Results[0].ID, response.Results[1].ID}
		if previous != nil && (previous[0] != current[0] || previous[1] != current[1]) {
			t.Fatalf("run %d results = %v, previous = %v", run, current, previous)
		}
		previous = current
	}
	if embedA.calls != 3 || embedB.calls != 1 {
		t.Fatalf("provider calls: a=%d b=%d, want cached a=3 b=1", embedA.calls, embedB.calls)
	}
	for _, event := range events {
		if fmt.Sprint(event.Participating) != "[space-a space-b]" {
			t.Fatalf("participating spaces = %v", event.Participating)
		}
	}
}

func TestEmbeddingQueryParticipatingSpacesAreEmptyWithoutScoredResults(t *testing.T) {
	var events []EmbeddingQueryEvent
	corpus, err := OpenWithOptions(t.TempDir(), Options{
		Embedder: &countingEmbedder{
			model: "empty", dimensions: 2, identity: "space-empty",
		},
		EmbeddingQueryObserver: func(event EmbeddingQueryEvent) {
			events = append(events, event)
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer corpus.Close()
	response, err := corpus.Retrieve(context.Background(), retrieval.Request{
		Text: "empty", Modes: []retrieval.Mode{retrieval.ModeVector},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(response.Results) != 0 || len(events) != 1 ||
		len(events[0].Participating) != 0 {
		t.Fatalf("empty retrieval response/events = %+v / %+v", response, events)
	}
	if fmt.Sprint(events[0].Completed) != "[space-empty]" {
		t.Fatalf("completed spaces = %v, want query-only space", events[0].Completed)
	}
}

func TestNonVectorRetrievalEmitsNoEmbeddingParticipation(t *testing.T) {
	var events []EmbeddingQueryEvent
	corpus, err := OpenWithOptions(t.TempDir(), Options{
		EmbeddingQueryObserver: func(event EmbeddingQueryEvent) {
			events = append(events, event)
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer corpus.Close()
	if _, err := corpus.Ingest(context.Background(), Source{
		URI: "file:///lexical.txt", MediaType: MediaTypeText,
		Content: "lexical evidence",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := corpus.Retrieve(context.Background(), retrieval.Request{
		Text: "lexical", Modes: []retrieval.Mode{retrieval.ModeLexical},
	}); err != nil {
		t.Fatal(err)
	}
	if len(events) != 0 {
		t.Fatalf("non-vector retrieval emitted embedding events: %+v", events)
	}
}

func TestEmbeddingQueryCacheUsesSpaceAndNormalizedQuery(t *testing.T) {
	ctx := context.Background()
	embedder := &countingEmbedder{
		model: "cache", dimensions: 2, identity: "space-cache",
	}
	var events []EmbeddingQueryEvent
	corpus, err := OpenWithOptions(t.TempDir(), Options{
		Embedder: embedder,
		EmbeddingQueryObserver: func(event EmbeddingQueryEvent) {
			events = append(events, event)
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer corpus.Close()
	if _, err := corpus.Ingest(ctx, Source{
		URI: "file:///cache.txt", MediaType: MediaTypeText, Content: "cache",
	}); err != nil {
		t.Fatal(err)
	}

	for _, query := range []string{"  cache query  ", "cache query"} {
		if _, err := corpus.Retrieve(ctx, retrieval.Request{
			Text: query, Modes: []retrieval.Mode{retrieval.ModeVector},
		}); err != nil {
			t.Fatal(err)
		}
	}
	if embedder.calls != 2 {
		t.Fatalf("embedder calls = %d, want ingest plus one normalized query", embedder.calls)
	}
	if len(events) != 2 ||
		events[0].ProviderCalls != 1 || events[0].CacheHits != 0 ||
		events[1].ProviderCalls != 0 || events[1].CacheHits != 1 {
		t.Fatalf("query events = %+v", events)
	}
	if fmt.Sprint(events[1].SpaceIdentities) != "[space-cache]" {
		t.Fatalf("cached event spaces = %v", events[1].SpaceIdentities)
	}
}

func TestEmbeddingQueryCacheIsBounded(t *testing.T) {
	ctx := context.Background()
	embedder := &countingEmbedder{
		model: "cache", dimensions: 2, identity: "space-cache",
	}
	corpus, err := OpenWithOptions(t.TempDir(), Options{
		Embedder: embedder, MaxEmbeddingQueryCacheEntries: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer corpus.Close()
	if _, err := corpus.Ingest(ctx, Source{
		URI: "file:///cache.txt", MediaType: MediaTypeText, Content: "cache",
	}); err != nil {
		t.Fatal(err)
	}
	for _, query := range []string{"first", "second", "first"} {
		if _, err := corpus.Retrieve(ctx, retrieval.Request{
			Text: query, Modes: []retrieval.Mode{retrieval.ModeVector},
		}); err != nil {
			t.Fatal(err)
		}
	}
	if embedder.calls != 4 {
		t.Fatalf("embedder calls = %d, want ingest plus three cache misses", embedder.calls)
	}
	corpus.queryEmbeddingMu.Lock()
	defer corpus.queryEmbeddingMu.Unlock()
	if len(corpus.queryEmbeddingCache) != 1 ||
		len(corpus.queryEmbeddingOrder) != 1 {
		t.Fatalf(
			"bounded cache = %d entries / %d order",
			len(corpus.queryEmbeddingCache), len(corpus.queryEmbeddingOrder))
	}
}

func TestEmbeddingQueryFanoutExceededIsObservableAndCallsNoProvider(t *testing.T) {
	ctx := context.Background()
	embedA := &countingEmbedder{model: "a", dimensions: 2, identity: "space-a"}
	embedB := &countingEmbedder{model: "b", dimensions: 2, identity: "space-b"}
	var events []EmbeddingQueryEvent
	corpus, err := OpenWithOptions(t.TempDir(), Options{
		Embedder: embedA, EmbeddingProviders: []model.Embedder{embedB},
		MaxEmbeddingSpaceFanout: 1,
		EmbeddingQueryObserver: func(event EmbeddingQueryEvent) {
			events = append(events, event)
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer corpus.Close()
	first, err := corpus.Ingest(ctx, Source{
		URI: "file:///a.txt", MediaType: MediaTypeText, Content: "alpha",
	})
	if err != nil {
		t.Fatal(err)
	}
	second, err := corpus.Ingest(ctx, Source{
		URI: "file:///b.txt", MediaType: MediaTypeText, Content: "bravo",
	})
	if err != nil {
		t.Fatal(err)
	}
	corpus.mu.Lock()
	corpus.documents[first.Document.ID][first.Revision.ID].Embeddings.Provenance =
		persistedEmbeddingProvenance{
			Provider: "counting", Model: "a", Identity: "space-a", Dimensions: 2,
		}
	corpus.documents[second.Document.ID][second.Revision.ID].Embeddings.Provenance =
		persistedEmbeddingProvenance{
			Provider: "counting", Model: "b", Identity: "space-b", Dimensions: 2,
		}
	corpus.embeddingSpace = embeddingSpaceCache{}
	corpus.mu.Unlock()

	beforeA, beforeB := embedA.calls, embedB.calls
	_, err = corpus.Retrieve(ctx, retrieval.Request{
		Text: "query", Modes: []retrieval.Mode{retrieval.ModeVector},
	})
	if !shoal.IsErrorCode(err, shoal.ErrorUnavailable) {
		t.Fatalf("fan-out error = %v", err)
	}
	if embedA.calls != beforeA || embedB.calls != beforeB {
		t.Fatalf("providers called after fan-out refusal: a=%d b=%d", embedA.calls-beforeA, embedB.calls-beforeB)
	}
	if len(events) != 1 || !events[0].FanoutExceeded ||
		events[0].FanoutLimit != 1 || events[0].ProviderCalls != 0 ||
		fmt.Sprint(events[0].SpaceIdentities) != "[space-a space-b]" {
		t.Fatalf("fan-out events = %+v", events)
	}
}

func TestUnavailableEmbeddingSpaceIsNamedAndObservable(t *testing.T) {
	ctx := context.Background()
	embedA := &countingEmbedder{model: "a", dimensions: 2, identity: "space-a"}
	embedB := &countingEmbedder{
		model: "b", dimensions: 2, identity: "space-b",
		err: model.ErrUnavailable,
	}
	var events []EmbeddingQueryEvent
	corpus, err := OpenWithOptions(t.TempDir(), Options{
		Embedder: embedA, EmbeddingProviders: []model.Embedder{embedB},
		MaxEmbeddingSpaceFanout: 4,
		EmbeddingQueryObserver: func(event EmbeddingQueryEvent) {
			events = append(events, event)
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer corpus.Close()
	first, err := corpus.Ingest(ctx, Source{
		URI: "file:///a.txt", MediaType: MediaTypeText, Content: "alpha",
	})
	if err != nil {
		t.Fatal(err)
	}
	second, err := corpus.Ingest(ctx, Source{
		URI: "file:///b.txt", MediaType: MediaTypeText, Content: "bravo",
	})
	if err != nil {
		t.Fatal(err)
	}
	corpus.mu.Lock()
	corpus.documents[first.Document.ID][first.Revision.ID].Embeddings.Provenance =
		persistedEmbeddingProvenance{
			Provider: "counting", Model: "a", Identity: "space-a", Dimensions: 2,
		}
	corpus.documents[second.Document.ID][second.Revision.ID].Embeddings.Provenance =
		persistedEmbeddingProvenance{
			Provider: "counting", Model: "b", Identity: "space-b", Dimensions: 2,
		}
	corpus.embeddingSpace = embeddingSpaceCache{}
	corpus.mu.Unlock()

	_, err = corpus.Retrieve(ctx, retrieval.Request{
		Text: "query", Modes: []retrieval.Mode{retrieval.ModeVector},
	})
	if !shoal.IsErrorCode(err, shoal.ErrorUnavailable) ||
		!strings.Contains(err.Error(), `"space-b"`) {
		t.Fatalf("unavailable space error = %v", err)
	}
	if len(events) != 1 || events[0].ProviderCalls != 2 ||
		fmt.Sprint(events[0].Unavailable) != "[space-b]" ||
		fmt.Sprint(events[0].Attempted) != "[space-a space-b]" ||
		fmt.Sprint(events[0].Completed) != "[space-a]" {
		t.Fatalf("unavailable events = %+v", events)
	}
}

func TestVectorRetrievalScopeDoesNotProbeUnscopedEmbeddingSpace(t *testing.T) {
	ctx := context.Background()
	embedder := &countingEmbedder{model: "a", dimensions: 2, identity: "space-a"}
	corpus, err := OpenWithOptions(t.TempDir(), Options{Embedder: embedder})
	if err != nil {
		t.Fatal(err)
	}
	defer corpus.Close()
	visible, err := corpus.Ingest(ctx, Source{URI: "file:///visible.txt", MediaType: MediaTypeText, Content: "visible"})
	if err != nil {
		t.Fatal(err)
	}
	hidden, err := corpus.Ingest(ctx, Source{URI: "file:///hidden.txt", MediaType: MediaTypeText, Content: "hidden"})
	if err != nil {
		t.Fatal(err)
	}
	corpus.mu.Lock()
	corpus.documents[hidden.Document.ID][hidden.Revision.ID].Embeddings.Provenance =
		persistedEmbeddingProvenance{Provider: "counting", Model: "b", Identity: "space-b", Dimensions: 2}
	corpus.embeddingSpace = embeddingSpaceCache{}
	corpus.mu.Unlock()

	before := embedder.calls
	response, err := corpus.Retrieve(ctx, retrieval.Request{
		Text:  "visible",
		Modes: []retrieval.Mode{retrieval.ModeVector},
		TopK:  1,
		Scope: retrieval.Scope{DocumentIDs: []shoal.ID{visible.Document.ID}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(response.Results) != 1 ||
		response.Results[0].Evidence[0].Citation.DocumentID != visible.Document.ID {
		t.Fatalf("scoped vector retrieval widened scope: %+v", response)
	}
	if embedder.calls-before != 1 {
		t.Fatalf("query embeddings = %d, want only scoped space", embedder.calls-before)
	}
}

type countingEmbedder struct {
	calls      int
	model      string
	dimensions int
	identity   string
	delay      time.Duration
	err        error
}

func (e *countingEmbedder) CacheIdentity() (string, error) {
	if e.identity != "" {
		return e.identity, nil
	}
	return "counting-embedder-v1:" + e.model, nil
}

func (e *countingEmbedder) EmbeddingSpaceIdentity() (string, error) {
	if e.identity != "" {
		return e.identity, nil
	}
	return "counting-embedding-space-v1:" + e.model, nil
}

func (e *countingEmbedder) Embed(
	ctx context.Context, req model.EmbedRequest,
) (model.EmbedResult, error) {
	if err := ctx.Err(); err != nil {
		return model.EmbedResult{}, err
	}
	if e.delay > 0 {
		timer := time.NewTimer(e.delay)
		select {
		case <-timer.C:
		case <-ctx.Done():
			timer.Stop()
			return model.EmbedResult{}, ctx.Err()
		}
	}
	e.calls++
	if e.err != nil {
		return model.EmbedResult{}, e.err
	}
	dimensions := e.dimensions
	if dimensions == 0 {
		dimensions = 2
	}
	vector := make([]float32, dimensions)
	vector[0] = 1
	modelName := e.model
	if modelName == "" {
		modelName = "deterministic"
	}
	return model.EmbedResult{
		Vector: vector,
		Provenance: model.Provenance{
			Provider: "counting",
			Model:    modelName,
		},
	}, nil
}
