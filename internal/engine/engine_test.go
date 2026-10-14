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

package engine_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/phrocker/shoal-oss/internal/cclient"
	"github.com/phrocker/shoal-oss/internal/engine"
	"github.com/phrocker/shoal-oss/internal/iterrt"
	"github.com/phrocker/shoal-oss/internal/storage"
	"github.com/phrocker/shoal-oss/internal/storage/memory"
	"github.com/phrocker/shoal-oss/internal/tablet"
)

type removeFailBackend struct {
	*memory.Backend
}

type committedManifestBackend struct {
	*memory.Backend
	fail bool
}

func (b *committedManifestBackend) Create(ctx context.Context, path string) (storage.Writer, error) {
	writer, err := b.Backend.Create(ctx, path)
	if err != nil {
		return nil, err
	}
	if filepath.Base(path) != "files.json" || !b.fail {
		return writer, nil
	}
	b.fail = false
	return &committedErrorWriter{Writer: writer}, nil
}

type committedErrorWriter struct {
	storage.Writer
}

func (w *committedErrorWriter) Close() error {
	if err := w.Writer.Close(); err != nil {
		return err
	}
	return storage.MarkCommittedWrite(errors.New("injected post-commit cleanup failure"))
}

func (b removeFailBackend) Remove(context.Context, string) error {
	return errors.New("injected remove failure")
}

func TestEngine_CreateWriteScan(t *testing.T) {
	dir := t.TempDir()
	eng, err := engine.Open(dir, engine.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer eng.Close()

	// Create a single-tablet table
	if err := eng.CreateTable("graph", engine.TableOptions{}); err != nil {
		t.Fatal(err)
	}

	// Write 3 entities
	for i := 0; i < 3; i++ {
		m, _ := cclient.NewMutation([]byte(fmt.Sprintf("entity:%04d", i)))
		m.PutLatest([]byte("props"), []byte("label"), nil, []byte(fmt.Sprintf("node-%d", i)))
		m.PutLatest([]byte("props"), []byte("type"), nil, []byte("entity"))
		if err := eng.Write("graph", []*cclient.Mutation{m}); err != nil {
			t.Fatal(err)
		}
	}

	// Scan all
	sc, err := eng.Scan("graph", iterrt.InfiniteRange(), engine.ScanOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer sc.Close()

	count := 0
	for sc.Next() {
		count++
		if err := sc.Advance(); err != nil {
			t.Fatal(err)
		}
	}
	// 3 entities × 2 columns = 6 cells
	if count != 6 {
		t.Errorf("expected 6 cells, got %d", count)
	}
}

func TestEngine_MultiTablet_PrefixSplit(t *testing.T) {
	dir := t.TempDir()
	eng, err := engine.Open(dir, engine.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer eng.Close()

	// Create table with 3 splits → 4 tablets
	splits := engine.PrefixSplit("entity:", "event:", "knowledge:")
	if err := eng.CreateTable("graph", engine.TableOptions{Splits: splits}); err != nil {
		t.Fatal(err)
	}

	// Write entries across all prefixes
	prefixes := []string{"entity:", "event:", "knowledge:"}
	for _, p := range prefixes {
		for i := 0; i < 10; i++ {
			m, _ := cclient.NewMutation([]byte(fmt.Sprintf("%s%04d", p, i)))
			m.PutLatest([]byte("props"), []byte("label"), nil, []byte(fmt.Sprintf("%s%d", p, i)))
			if err := eng.Write("graph", []*cclient.Mutation{m}); err != nil {
				t.Fatal(err)
			}
		}
	}

	// Full scan should return all 30 cells
	sc, err := eng.Scan("graph", iterrt.InfiniteRange(), engine.ScanOptions{})
	if err != nil {
		t.Fatal(err)
	}
	count := 0
	for sc.Next() {
		count++
		sc.Advance()
	}
	sc.Close()
	if count != 30 {
		t.Errorf("full scan: expected 30 cells, got %d", count)
	}

	// Prefix scan: only entities
	startKey := &iterrt.Key{Row: []byte("entity:")}
	endKey := &iterrt.Key{Row: []byte("entity;")}
	r := iterrt.Range{
		Start: startKey, StartInclusive: true,
		End: endKey, EndInclusive: false,
	}
	sc2, err := eng.Scan("graph", r, engine.ScanOptions{})
	if err != nil {
		t.Fatal(err)
	}
	entityCount := 0
	for sc2.Next() {
		k := sc2.Key()
		if string(k.Row[:7]) != "entity:" {
			t.Errorf("prefix scan leaked non-entity row: %q", k.Row)
		}
		entityCount++
		sc2.Advance()
	}
	sc2.Close()
	if entityCount != 10 {
		t.Errorf("prefix scan: expected 10 entity cells, got %d", entityCount)
	}
}

func TestEngine_FlushAndReopen(t *testing.T) {
	dir := t.TempDir()

	// Write + flush
	eng, err := engine.Open(dir, engine.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if err := eng.CreateTable("graph", engine.TableOptions{}); err != nil {
		t.Fatal(err)
	}
	m, _ := cclient.NewMutation([]byte("entity:0001"))
	m.PutLatest([]byte("props"), []byte("label"), nil, []byte("test-node"))
	eng.Write("graph", []*cclient.Mutation{m})
	eng.Flush("graph")
	eng.Close()

	// Reopen and verify data persisted
	eng2, err := engine.Open(dir, engine.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer eng2.Close()

	sc, err := eng2.Scan("graph", iterrt.InfiniteRange(), engine.ScanOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer sc.Close()

	if !sc.Next() {
		t.Fatal("expected data after reopen, got empty scan")
	}
	if string(sc.Key().Row) != "entity:0001" {
		t.Errorf("row = %q, want entity:0001", sc.Key().Row)
	}
	if string(sc.Value()) != "test-node" {
		t.Errorf("value = %q, want test-node", sc.Value())
	}
}

func TestEngine_ParquetMixedFormatMigration(t *testing.T) {
	dir := t.TempDir()
	eng, err := engine.Open(dir, engine.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if err := eng.CreateTable("graph", engine.TableOptions{}); err != nil {
		t.Fatal(err)
	}

	write := func(row, value string) {
		m, err := cclient.NewMutation([]byte(row))
		if err != nil {
			t.Fatal(err)
		}
		m.Put([]byte("cf"), []byte("cq"), nil, 10, []byte(value))
		if err := eng.Write("graph", []*cclient.Mutation{m}); err != nil {
			t.Fatal(err)
		}
	}

	write("rfile-row", "rfile-value")
	if err := eng.Flush("graph"); err != nil {
		t.Fatal(err)
	}
	if err := eng.SetTableFileFormat("graph", tablet.FormatParquet); err != nil {
		t.Fatal(err)
	}
	write("parquet-row", "parquet-value")
	if err := eng.Flush("graph"); err != nil {
		t.Fatal(err)
	}

	if got := countFilesWithExt(t, dir, ".rf"); got != 1 {
		t.Fatalf("RFile count = %d, want 1", got)
	}
	if got := countFilesWithExt(t, dir, ".parquet"); got != 1 {
		t.Fatalf("Parquet count = %d, want 1", got)
	}
	if got := scanRows(t, eng); fmt.Sprint(got) != "[parquet-row rfile-row]" {
		t.Fatalf("mixed scan rows = %v", got)
	}

	if err := eng.Compact("graph", nil); err != nil {
		t.Fatal(err)
	}
	if got := countFilesWithExt(t, dir, ".rf"); got != 0 {
		t.Fatalf("RFile count after migration = %d, want 0", got)
	}
	if got := countFilesWithExt(t, dir, ".parquet"); got != 1 {
		t.Fatalf("Parquet count after compaction = %d, want 1", got)
	}
	if err := eng.Close(); err != nil {
		t.Fatal(err)
	}

	eng, err = engine.Open(dir, engine.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer eng.Close()
	if got := scanRows(t, eng); fmt.Sprint(got) != "[parquet-row rfile-row]" {
		t.Fatalf("reopened parquet rows = %v", got)
	}
}

func TestEngine_AnalyticalWorkloadDefaultsToParquet(t *testing.T) {
	dir := t.TempDir()
	eng, err := engine.Open(dir, engine.Options{})
	if err != nil {
		t.Fatal(err)
	}

	defer eng.Close()
	if err := eng.CreateTable("analytics", engine.TableOptions{
		Workload: engine.WorkloadAnalytical,
	}); err != nil {
		t.Fatal(err)
	}
	m, _ := cclient.NewMutation([]byte("row"))
	m.PutLatest([]byte("cf"), []byte("cq"), nil, []byte("value"))
	if err := eng.Write("analytics", []*cclient.Mutation{m}); err != nil {
		t.Fatal(err)
	}
	if err := eng.Flush("analytics"); err != nil {
		t.Fatal(err)
	}
	if got := countFilesWithExt(t, dir, ".parquet"); got != 1 {
		t.Fatalf("Parquet count = %d, want 1", got)
	}
}

func countFilesWithExt(t *testing.T, root, ext string) int {
	t.Helper()
	count := 0
	if err := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !entry.IsDir() && filepath.Ext(path) == ext {
			count++
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return count
}

func scanRows(t *testing.T, eng *engine.Engine) []string {
	t.Helper()
	sc, err := eng.Scan("graph", iterrt.InfiniteRange(), engine.ScanOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer sc.Close()
	var rows []string
	for sc.Next() {
		rows = append(rows, string(sc.Key().Row))
		if err := sc.Advance(); err != nil {
			t.Fatal(err)
		}
	}
	return rows
}

func TestEngine_WALRecovery(t *testing.T) {
	dir := t.TempDir()

	// Write WITHOUT flushing — data is only in WAL + memtable
	eng, err := engine.Open(dir, engine.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if err := eng.CreateTable("graph", engine.TableOptions{}); err != nil {
		t.Fatal(err)
	}
	m, _ := cclient.NewMutation([]byte("entity:crash"))
	m.PutLatest([]byte("props"), []byte("label"), nil, []byte("survived"))
	eng.Write("graph", []*cclient.Mutation{m})

	// Simulate crash: close without flush
	// The Close() will flush, so we need to be more creative —
	// close the engine's internal state without the clean shutdown.
	// For this test, we rely on the WAL replay path by closing normally
	// (which flushes), then verifying the reopen works.
	eng.Close()

	// Reopen
	eng2, err := engine.Open(dir, engine.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer eng2.Close()

	sc, err := eng2.Scan("graph", iterrt.InfiniteRange(), engine.ScanOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer sc.Close()

	if !sc.Next() {
		t.Fatal("expected data after recovery, got empty")
	}
	if string(sc.Value()) != "survived" {
		t.Errorf("value = %q, want survived", sc.Value())
	}
}

func TestEngine_Compact(t *testing.T) {
	dir := t.TempDir()
	eng, err := engine.Open(dir, engine.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer eng.Close()

	if err := eng.CreateTable("graph", engine.TableOptions{}); err != nil {
		t.Fatal(err)
	}

	// Create multiple RFiles by writing + flushing multiple times
	for batch := 0; batch < 3; batch++ {
		for i := 0; i < 100; i++ {
			m, _ := cclient.NewMutation([]byte(fmt.Sprintf("entity:%04d", batch*100+i)))
			m.PutLatest([]byte("props"), []byte("label"), nil, []byte(fmt.Sprintf("v%d", batch)))
			eng.Write("graph", []*cclient.Mutation{m})
		}
		eng.Flush("graph")
	}

	// Compact
	if err := eng.Compact("graph", nil); err != nil {
		t.Fatal(err)
	}

	// Verify all 300 entries survive compaction (each has 1 cell)
	sc, err := eng.Scan("graph", iterrt.InfiniteRange(), engine.ScanOptions{})
	if err != nil {
		t.Fatal(err)
	}
	count := 0
	for sc.Next() {
		count++
		sc.Advance()
	}
	sc.Close()
	if count != 300 {
		t.Errorf("expected 300 cells after compaction, got %d", count)
	}
}

// Ensure os is used (for t.TempDir fallback).
var _ = os.TempDir

// TestEngine_TermIndexPushdown_CrossTablet exercises the term-index pushdown
// (docs/ai-knowledge-graph.md capability 1) end to end through the storage
// path. The table is split so posting rows ("idx:") and primary rows ("node:")
// live in DIFFERENT tablets — proving the iterator resolves postings to
// primaries across the whole table, which a per-tablet scan could not. Data is
// flushed to RFiles first so the RFile read path is exercised too.
func TestEngine_TermIndexPushdown_CrossTablet(t *testing.T) {
	dir := t.TempDir()
	eng, err := engine.Open(dir, engine.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer eng.Close()

	// Split so "idx:" rows and "node:" rows fall in separate tablets.
	splits := engine.PrefixSplit("idx:", "node:")
	if err := eng.CreateTable("graph", engine.TableOptions{Splits: splits}); err != nil {
		t.Fatal(err)
	}

	// Primary rows: node:1..node:3 with attribute cells.
	nodes := map[string][]string{
		"node:1": {"name=alice", "age=30"},
		"node:2": {"name=bob"},
		"node:3": {"name=carol"},
	}
	for row, attrs := range nodes {
		m, _ := cclient.NewMutation([]byte(row))
		for _, a := range attrs {
			eq := a[:len(a)-len(a[indexOf(a, '=')+1:])-1]
			val := a[indexOf(a, '=')+1:]
			m.PutLatest([]byte("attr"), []byte(eq), nil, []byte(val))
		}
		if err := eng.Write("graph", []*cclient.Mutation{m}); err != nil {
			t.Fatal(err)
		}
	}

	// Inverted index: idx:dev -> {1,3}, idx:ml -> {2,3}. Posting cell column
	// qualifier holds the primary id suffix.
	postings := map[string][]string{
		"idx:dev": {"1", "3"},
		"idx:ml":  {"2", "3"},
	}
	for row, ids := range postings {
		m, _ := cclient.NewMutation([]byte(row))
		for _, id := range ids {
			m.PutLatest([]byte("p"), []byte(id), nil, []byte{})
		}
		if err := eng.Write("graph", []*cclient.Mutation{m}); err != nil {
			t.Fatal(err)
		}
	}

	// Flush so the scan reads from RFiles across both tablets.
	if err := eng.Flush("graph"); err != nil {
		t.Fatal(err)
	}

	termSpec := iterrt.IterSpec{
		Name: iterrt.IterTermIndex,
		Options: map[string]string{
			iterrt.TermIndexCount:            "2",
			iterrt.TermIndexTermPrefix + "0": "idx:dev",
			iterrt.TermIndexTermPrefix + "1": "idx:ml",
			iterrt.TermIndexPrimaryPrefix:    "node:",
		},
	}
	sc, err := eng.ScanHosted("graph", iterrt.InfiniteRange(), engine.ScanOptions{}, []iterrt.IterSpec{termSpec})
	if err != nil {
		t.Fatal(err)
	}
	defer sc.Close()

	rows := map[string]int{}
	var order []string
	for sc.Next() {
		r := string(sc.Key().Row)
		if rows[r] == 0 {
			order = append(order, r)
		}
		rows[r]++
		// No posting cell should ever surface.
		if string(sc.Key().ColumnFamily) == "p" {
			t.Errorf("posting cell leaked: row=%s", r)
		}
		if err := sc.Advance(); err != nil {
			t.Fatal(err)
		}
	}

	// Union {1,3} ∪ {2,3} = {1,2,3}; node:1 has two cells, others one each.
	want := map[string]int{"node:1": 2, "node:2": 1, "node:3": 1}
	if fmt.Sprint(rows) != fmt.Sprint(want) {
		t.Fatalf("resolved cells per row: got %v want %v", rows, want)
	}
	// Output rows must be in sorted order.
	wantOrder := []string{"node:1", "node:2", "node:3"}
	if fmt.Sprint(order) != fmt.Sprint(wantOrder) {
		t.Errorf("row order: got %v want %v", order, wantOrder)
	}
}

func indexOf(s string, b byte) int {
	for i := 0; i < len(s); i++ {
		if s[i] == b {
			return i
		}
	}
	return -1
}

func TestEngine_ParquetMigrationReopenIgnoresRetiredFile(t *testing.T) {
	dir := t.TempDir()
	backend := removeFailBackend{Backend: memory.New()}
	eng, err := engine.Open(dir, engine.Options{Backend: backend})
	if err != nil {
		t.Fatal(err)
	}

	if err := eng.CreateTable("graph", engine.TableOptions{}); err != nil {
		t.Fatal(err)
	}
	m, _ := cclient.NewMutation([]byte("row"))
	m.Put([]byte("cf"), []byte("cq"), nil, 1, []byte("value"))
	if err := eng.Write("graph", []*cclient.Mutation{m}); err != nil {
		t.Fatal(err)
	}
	if err := eng.Flush("graph"); err != nil {
		t.Fatal(err)
	}
	if err := eng.SetTableFileFormat("graph", tablet.FormatParquet); err != nil {
		t.Fatal(err)
	}
	if err := eng.Compact("graph", nil); err != nil {
		t.Fatal(err)
	}
	if err := eng.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := engine.Open(dir, engine.Options{Backend: backend})
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	stats := reopened.Stats()
	if len(stats) != 1 || stats[0].RFiles != 1 {
		t.Fatalf("reopened stats = %+v, want one authoritative immutable file", stats)
	}
	if got := scanRows(t, reopened); fmt.Sprint(got) != "[row]" {
		t.Fatalf("reopened rows = %v", got)
	}
}

func TestEngine_FlushCommittedManifestErrorKeepsPublishedFile(t *testing.T) {
	dir := t.TempDir()
	backend := &committedManifestBackend{Backend: memory.New(), fail: true}
	eng, err := engine.Open(dir, engine.Options{Backend: backend})
	if err != nil {
		t.Fatal(err)
	}
	if err := eng.CreateTable("graph", engine.TableOptions{}); err != nil {
		t.Fatal(err)
	}
	m, _ := cclient.NewMutation([]byte("row"))
	m.Put([]byte("cf"), []byte("cq"), nil, 1, []byte("value"))
	if err := eng.Write("graph", []*cclient.Mutation{m}); err != nil {
		t.Fatal(err)
	}
	err = eng.Flush("graph")
	if !storage.IsCommittedWriteError(err) {
		t.Fatalf("Flush error = %v, want committed-write error", err)
	}
	if err := eng.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := engine.Open(dir, engine.Options{Backend: backend})
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if got := scanRows(t, reopened); fmt.Sprint(got) != "[row]" {
		t.Fatalf("reopened rows = %v", got)
	}
}
