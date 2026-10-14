// Licensed to the Apache Software Foundation (ASF) under one
// or more contributor license agreements.  See the NOTICE file
// distributed with this work for additional information
// regarding copyright ownership.  The ASF licenses this file
// to you under the Apache License, Version 2.0 (the
// "License"); you may not use this file except in compliance
// with the License.  You may obtain a copy of the License at
//
//   https://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing,
// software distributed under the License is distributed on an
// "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY
// KIND, either express or implied.  See the License for the
// specific language governing permissions and limitations
// under the License.

package embedstore_test

import (
	"context"
	"errors"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/phrocker/shoal-oss/internal/agentmem"
	"github.com/phrocker/shoal-oss/internal/embedpb"
	"github.com/phrocker/shoal-oss/internal/embedstore"
	"github.com/phrocker/shoal-oss/internal/engine"
	"github.com/phrocker/shoal-oss/internal/graphschema"
	"github.com/phrocker/shoal-oss/internal/storage/memory"
)

// TestAgentmemRoundTripExportImport is the headline "unlock" proof, run fully
// offline (FakeEmbedder/FakeLLM, in-memory backend): an agentmem client ingests
// memories into a local engine through the shared embedstore translation; the
// engine's RFiles are exported and bulk-imported into a fresh engine; and a
// second agentmem client querying the imported engine recovers the same graph —
// with no re-ingest and no re-embed. This is the local-memory-graph-is-platform
// -data guarantee gated in CI.
func TestScannerContextCancelsEmbeddingMetadataRead(t *testing.T) {
	ctx := context.Background()
	eng, err := engine.Open(t.TempDir(), engine.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer eng.Close()
	store := embedstore.New(eng)
	const identity = "test:model:v1:1:normalized"
	if err := store.CreateTableWithEmbedding(
		ctx, "vectors", nil, "has_embeddings:"+identity); err != nil {
		t.Fatal(err)
	}
	if err := store.Write(ctx, "vectors", []*embedpb.Mutation{{
		Row: []byte("row"),
		Entries: []*embedpb.Entry{{
			ColumnFamily: []byte("vec"),
			Value:        []byte{0, 0, 0, 0},
		}},
	}}); err != nil {
		t.Fatal(err)
	}
	if err := store.Flush(ctx, "vectors"); err != nil {
		t.Fatal(err)
	}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	_, err = store.ScannerContext(canceled, "vectors", &embedpb.ScanRequest{
		VectorSearch: &embedpb.VectorSearch{
			Query:          []byte{0, 0, 0, 0},
			EmbeddingSpace: identity,
		},
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("ScannerContext error = %v, want context.Canceled", err)
	}
}

func TestScanAcceptsNilContext(t *testing.T) {
	eng, err := engine.Open(t.TempDir(), engine.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer eng.Close()
	store := embedstore.New(eng)
	if err := store.CreateTable(context.Background(), "rows", nil); err != nil {
		t.Fatal(err)
	}
	if err := store.Write(context.Background(), "rows", []*embedpb.Mutation{{
		Row: []byte("row"),
		Entries: []*embedpb.Entry{{
			ColumnFamily: []byte("cf"),
			Value:        []byte("value"),
		}},
	}}); err != nil {
		t.Fatal(err)
	}
	cells, err := store.Scan(nil, "rows", &embedpb.ScanRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if len(cells) != 1 {
		t.Fatalf("cells = %d, want 1", len(cells))
	}
}

func TestAgentmemRoundTripExportImport(t *testing.T) {
	ctx := context.Background()
	srcDir := filepath.Join(t.TempDir(), "src")
	dstDir := filepath.Join(t.TempDir(), "dst")
	const table = "graph"

	// 1. Local producer: agentmem ingests through an engine-backed store.
	src, err := engine.Open(srcDir, engine.Options{})
	if err != nil {
		t.Fatalf("open source engine: %v", err)
	}
	defer src.Close()
	srcStore := embedstore.New(src)
	producer, err := agentmem.New(agentmem.Config{Store: srcStore, Table: table, MaxDepth: 1})
	if err != nil {
		t.Fatalf("new producer client: %v", err)
	}
	if err := producer.EnsureTable(ctx); err != nil {
		t.Fatalf("ensure table: %v", err)
	}

	base := time.Date(2026, 6, 23, 12, 0, 0, 0, time.UTC)
	docs := []string{
		"Alice deployed the graph service",
		"The graph service failed because the index was stale",
		"Alice rebuilt the index after the failure",
	}
	for i, text := range docs {
		if _, err := producer.Ingest(ctx, agentmem.IngestRequest{Text: text, Time: base.Add(time.Duration(i) * time.Millisecond)}); err != nil {
			t.Fatalf("ingest %d: %v", i, err)
		}
	}

	// 2. Flush to immutable RFiles, then export a manifest into an object store.
	if err := srcStore.Flush(ctx, table); err != nil {
		t.Fatalf("flush: %v", err)
	}
	dstBackend := memory.New()
	manifest, err := src.ExportRFiles(ctx, table, dstBackend, engine.RFileExportOptions{
		DestinationRoot: dstDir,
		CFSchema:        "graphschema/v1",
		EngineVersion:   "e2e-test",
	})
	if err != nil {
		t.Fatalf("export rfiles: %v", err)
	}
	if len(manifest.RFiles) == 0 {
		t.Fatalf("export produced no RFiles")
	}
	if err := engine.VerifyRFileExport(ctx, dstBackend, manifest); err != nil {
		t.Fatalf("verify export: %v", err)
	}

	// 3. Platform consumer: a fresh engine bulk-imports the manifest — no
	//    re-ingest, no re-embed; it opens the producer's bytes directly.
	dst, err := engine.Open(dstDir, engine.Options{Backend: dstBackend})
	if err != nil {
		t.Fatalf("open dest engine: %v", err)
	}
	defer dst.Close()
	if err := dst.ImportRFileManifest(ctx, manifest); err != nil {
		t.Fatalf("import manifest: %v", err)
	}
	dstStore := embedstore.New(dst)

	// 3a. Byte-level integrity: the imported event content cells match exactly
	//     what was ingested, proving the RFiles round-tripped losslessly.
	wantContent := map[string]bool{}
	for _, d := range docs {
		wantContent[d] = true
	}
	gotContent := contentTexts(t, ctx, dstStore, table)
	if len(gotContent) != len(wantContent) {
		t.Fatalf("imported content count = %d, want %d (%v)", len(gotContent), len(wantContent), gotContent)
	}
	for _, c := range gotContent {
		if !wantContent[c] {
			t.Fatalf("imported content %q was never ingested", c)
		}
	}

	// 3b. Source and destination engines hold an identical cell image.
	if got, want := rawScan(t, ctx, dstStore, table), rawScan(t, ctx, srcStore, table); strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("src/dst cell image mismatch\ndst:\n%s\nsrc:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}

	// 4. Full agentic loop against the imported engine: a new client (same
	//    deterministic embedder) recovers a synthesized answer referencing the
	//    ingested memories — the policy layer works identically post-import.
	consumer, err := agentmem.New(agentmem.Config{Store: dstStore, Table: table, MaxDepth: 1, TokenBudget: 60})
	if err != nil {
		t.Fatalf("new consumer client: %v", err)
	}
	res, err := consumer.Query(ctx, agentmem.QueryRequest{Text: "why did the graph service fail", Time: base})
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if res.Intent != agentmem.IntentWhy {
		t.Fatalf("intent = %s, want WHY", res.Intent)
	}
	if len(res.Anchors) == 0 {
		t.Fatalf("query returned no anchors")
	}
	if !strings.Contains(res.Context, "<ref:") {
		t.Fatalf("synthesized context missing citations: %q", res.Context)
	}
	if !strings.Contains(strings.ToLower(res.Context), "service") {
		t.Fatalf("synthesized context did not recover ingested memory: %q", res.Context)
	}
}

// contentTexts returns the text values of every event content cell in table.
func contentTexts(t *testing.T, ctx context.Context, store *embedstore.EngineStore, table string) []string {
	t.Helper()
	cells, err := store.Scan(ctx, table, &embedpb.ScanRequest{RowPrefix: graphschema.EventRowPrefix})
	if err != nil {
		t.Fatalf("scan content: %v", err)
	}
	var out []string
	for _, c := range cells {
		if string(c.ColumnFamily) == graphschema.ContentCFName && string(c.ColumnQualifier) == "text" {
			out = append(out, string(c.Value))
		}
	}
	sort.Strings(out)
	return out
}

// rawScan returns a stable, comparable string image of every cell in table.
func rawScan(t *testing.T, ctx context.Context, store *embedstore.EngineStore, table string) []string {
	t.Helper()
	cells, err := store.Scan(ctx, table, &embedpb.ScanRequest{})
	if err != nil {
		t.Fatalf("raw scan: %v", err)
	}
	var out []string
	for _, c := range cells {
		out = append(out, strings.Join([]string{
			string(c.Row), string(c.ColumnFamily), string(c.ColumnQualifier), string(c.Value),
		}, "|"))
	}
	sort.Strings(out)
	return out
}
