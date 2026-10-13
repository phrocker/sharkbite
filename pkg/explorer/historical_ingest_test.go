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

// Re-ingesting content that resolves to an existing revision reports whether
// that revision is the document's current one (#674). A non-current one is
// IngestHistorical: a successful result that changed nothing and did not make
// the old revision current again.

import (
	"context"
	"sync"
	"testing"

	"github.com/phrocker/shoal-oss/pkg/explorer"
	"github.com/phrocker/shoal-oss/pkg/model"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

func historicalSource(content string) explorer.Source {
	return explorer.Source{
		URI: "file:///historical.txt", MediaType: explorer.MediaTypeText,
		Content: content,
	}
}

func requireCurrentRevision(
	t *testing.T, corpus *explorer.Explorer, documentID, revisionID shoal.ID,
) {
	t.Helper()
	summaries, err := corpus.Documents(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(summaries) != 1 || summaries[0].Document.ID != documentID ||
		summaries[0].Revision.ID != revisionID {
		t.Fatalf("current revisions = %+v, want %s", summaries, revisionID)
	}
	view, err := corpus.Document(context.Background(), documentID, "")
	if err != nil {
		t.Fatal(err)
	}
	if view.Revision.ID != revisionID {
		t.Fatalf("current document view revision = %s, want %s", view.Revision.ID, revisionID)
	}
}

// TestIngestReportsAnEarlierRevisionAsHistorical is the plain stale retry: a
// delayed retry of R1 after R2 superseded it. It still succeeds (no error),
// so callers that only check the error keep working, but it says historical,
// changes nothing, and does not roll the document back to R1, before or
// after a restart.
func TestIngestReportsAnEarlierRevisionAsHistorical(t *testing.T) {
	ctx := context.Background()
	dataDir := t.TempDir()
	corpus, err := explorer.Open(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	first, err := corpus.Ingest(ctx, historicalSource("first revision"))
	if err != nil || first.Disposition != explorer.IngestApplied {
		t.Fatalf("first ingest = %+v, %v", first, err)
	}
	second, err := corpus.Ingest(ctx, historicalSource("second revision"))
	if err != nil || second.Disposition != explorer.IngestApplied {
		t.Fatalf("second ingest = %+v, %v", second, err)
	}
	before, err := corpus.Snapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}

	retry, err := corpus.Ingest(ctx, historicalSource("first revision"))
	if err != nil {
		t.Fatalf("stale retry: %v", err)
	}
	if retry.Disposition != explorer.IngestHistorical ||
		retry.Document.ID != first.Document.ID ||
		retry.Revision.ID != first.Revision.ID ||
		retry.SectionCount != first.SectionCount ||
		retry.SpanCount != first.SpanCount {
		t.Fatalf("stale retry = %+v, want historical %+v", retry, first)
	}
	requireCurrentRevision(t, corpus, second.Document.ID, second.Revision.ID)
	after, err := corpus.Snapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if after != before {
		t.Fatalf("a historical ingest changed the snapshot: %+v -> %+v", before, after)
	}
	unchanged, err := corpus.Ingest(ctx, historicalSource("second revision"))
	if err != nil || unchanged.Disposition != explorer.IngestUnchanged ||
		unchanged.Revision.ID != second.Revision.ID {
		t.Fatalf("current retry = %+v, %v, want unchanged", unchanged, err)
	}
	if err := corpus.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := explorer.Open(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	for _, test := range []struct {
		content string
		want    explorer.IngestDisposition
		id      shoal.ID
	}{
		{"first revision", explorer.IngestHistorical, first.Revision.ID},
		{"second revision", explorer.IngestUnchanged, second.Revision.ID},
	} {
		result, err := reopened.Ingest(ctx, historicalSource(test.content))
		if err != nil || result.Disposition != test.want || result.Revision.ID != test.id {
			t.Fatalf("reopened %q = %+v, %v, want %s", test.content, result, err, test.want)
		}
	}
	requireCurrentRevision(t, reopened, second.Document.ID, second.Revision.ID)

	// A newer revision makes the previously current one historical too.
	third, err := reopened.Ingest(ctx, historicalSource("third revision"))
	if err != nil || third.Disposition != explorer.IngestApplied {
		t.Fatalf("third ingest = %+v, %v", third, err)
	}
	for _, content := range []string{"first revision", "second revision"} {
		result, err := reopened.Ingest(ctx, historicalSource(content))
		if err != nil || result.Disposition != explorer.IngestHistorical {
			t.Fatalf("%q after a third revision = %+v, %v, want historical", content, result, err)
		}
	}
	requireCurrentRevision(t, reopened, third.Document.ID, third.Revision.ID)
}

// gatedEmbedder blocks the first embedding of gate until release closes, so
// a test can publish other revisions while that ingest sits between its
// read-locked revision check and its write-locked one.
type gatedEmbedder struct {
	model.FakeEmbedder
	gate    string
	once    sync.Once
	entered chan struct{}
	release chan struct{}
}

func (e *gatedEmbedder) Embed(
	ctx context.Context, request model.EmbedRequest,
) (model.EmbedResult, error) {
	if request.Text == e.gate {
		blocked := false
		e.once.Do(func() { blocked = true })
		if blocked {
			close(e.entered)
			select {
			case <-e.release:
			case <-ctx.Done():
				return model.EmbedResult{}, ctx.Err()
			}
		}
	}
	return e.FakeEmbedder.Embed(ctx, request)
}

// TestIngestReportsHistoricalAfterLosingAPublicationRace covers the second,
// write-locked revision check. Ingest A of R1 content is parked in the
// embedder after its read-locked check found nothing. Meanwhile R1 is
// published by ingest B and superseded by R2. When A resumes, its write-locked
// check finds R1 present but not current, and must report historical.
func TestIngestReportsHistoricalAfterLosingAPublicationRace(t *testing.T) {
	ctx := context.Background()
	embedder := &gatedEmbedder{
		FakeEmbedder: model.FakeEmbedder{Model: "historical-race", Dimensions: 4},
		gate:         "raced revision",
		entered:      make(chan struct{}),
		release:      make(chan struct{}),
	}
	corpus, err := explorer.OpenWithOptions(t.TempDir(), explorer.Options{Embedder: embedder})
	if err != nil {
		t.Fatal(err)
	}
	defer corpus.Close()

	type outcome struct {
		result explorer.IngestResult
		err    error
	}
	parked := make(chan outcome, 1)
	go func() {
		result, err := corpus.Ingest(ctx, historicalSource("raced revision"))
		parked <- outcome{result, err}
	}()
	<-embedder.entered
	first, err := corpus.Ingest(ctx, historicalSource("raced revision"))
	if err != nil || first.Disposition != explorer.IngestApplied {
		t.Fatalf("racing ingest = %+v, %v", first, err)
	}
	second, err := corpus.Ingest(ctx, historicalSource("superseding revision"))
	if err != nil || second.Disposition != explorer.IngestApplied {
		t.Fatalf("superseding ingest = %+v, %v", second, err)
	}
	close(embedder.release)
	resumed := <-parked
	if resumed.err != nil {
		t.Fatalf("parked ingest: %v", resumed.err)
	}
	if resumed.result.Disposition != explorer.IngestHistorical ||
		resumed.result.Revision.ID != first.Revision.ID {
		t.Fatalf("parked ingest = %+v, want historical %s", resumed.result, first.Revision.ID)
	}
	requireCurrentRevision(t, corpus, second.Document.ID, second.Revision.ID)
}
