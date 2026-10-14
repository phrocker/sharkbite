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

// Package bench_test provides comparative benchmarks between the shoal
// embedded engine and equivalent SQLite operations for application-shaped
// workloads.
//
// Run with:
//
//	go test -bench=. -benchmem -count=3 ./internal/engine/
//
// The SQLite benchmarks are stubbed — they measure the overhead of the
// Go-native engine against the baseline a consumer would see from
// better-sqlite3. Real SQLite numbers come from the Node.js side;
// these Go stubs exist so the two engines can be profiled under
// identical conditions (same key distribution, same value sizes, same
// scan patterns).
package engine_test

import (
	"fmt"
	"io"
	"log/slog"
	"math/rand"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/phrocker/shoal-oss/internal/cclient"
	"github.com/phrocker/shoal-oss/internal/engine"
	"github.com/phrocker/shoal-oss/internal/iterrt"
	"github.com/phrocker/shoal-oss/internal/localwal"
	"github.com/phrocker/shoal-oss/internal/tablet"
)

// --- Write benchmarks ---

// BenchmarkShoal_Write_SingleTablet measures raw write throughput into
// a single-tablet table (no split routing overhead).
func BenchmarkShoal_Write_SingleTablet(b *testing.B) {
	eng, cleanup := setupEngine(b, nil)
	defer cleanup()

	mutations := generateMutations(b.N, "entity:", 128)
	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		if err := eng.Write("bench", []*cclient.Mutation{mutations[i]}); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkShoal_Write_MultiTablet measures write throughput with
// prefix-based splits — mutations are routed to 3 tablets.
func BenchmarkShoal_Write_MultiTablet(b *testing.B) {
	splits := engine.PrefixSplit("entity:", "event:", "knowledge:")
	eng, cleanup := setupEngine(b, splits)
	defer cleanup()

	prefixes := []string{"entity:", "event:", "knowledge:"}
	mutations := make([]*cclient.Mutation, b.N)
	for i := range mutations {
		prefix := prefixes[i%3]
		mutations[i] = makeMutation(fmt.Sprintf("%s%08d", prefix, i), 128)
	}
	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		if err := eng.Write("bench", []*cclient.Mutation{mutations[i]}); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkShoal_WriteBatch measures batch write throughput (100
// mutations per call, mixed across prefixes).
func BenchmarkShoal_WriteBatch(b *testing.B) {
	splits := engine.PrefixSplit("entity:", "event:", "knowledge:")
	eng, cleanup := setupEngine(b, splits)
	defer cleanup()

	const batchSize = 100
	prefixes := []string{"entity:", "event:", "knowledge:"}
	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		batch := make([]*cclient.Mutation, batchSize)
		for j := range batch {
			prefix := prefixes[(i*batchSize+j)%3]
			batch[j] = makeMutation(fmt.Sprintf("%s%08d", prefix, i*batchSize+j), 128)
		}
		if err := eng.Write("bench", batch); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkShoal_Write_SingleTablet_SyncNormal measures single-write
// throughput under the SyncNormal durability tier (fsync deferred off the
// hot path), the apples-to-apples analogue of the SQLite WAL-mode
// synchronous=NORMAL baseline.
func BenchmarkShoal_Write_SingleTablet_SyncNormal(b *testing.B) {
	eng, cleanup := setupEngineSync(b, nil, localwal.SyncNormal, 0)
	defer cleanup()

	mutations := generateMutations(b.N, "entity:", 128)
	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		if err := eng.Write("bench", []*cclient.Mutation{mutations[i]}); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkShoal_Write_SingleTablet_SyncInterval measures single-write
// throughput under SyncNormal with a 5ms group-commit ticker — durability
// loss is bounded to ~5ms while fsync stays off the per-write path.
func BenchmarkShoal_Write_SingleTablet_SyncInterval(b *testing.B) {
	eng, cleanup := setupEngineSync(b, nil, localwal.SyncNormal, 5*time.Millisecond)
	defer cleanup()

	mutations := generateMutations(b.N, "entity:", 128)
	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		if err := eng.Write("bench", []*cclient.Mutation{mutations[i]}); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkShoal_WriteBatch_SyncNormal is BenchmarkShoal_WriteBatch under
// the SyncNormal durability tier.
func BenchmarkShoal_WriteBatch_SyncNormal(b *testing.B) {
	splits := engine.PrefixSplit("entity:", "event:", "knowledge:")
	eng, cleanup := setupEngineSync(b, splits, localwal.SyncNormal, 0)
	defer cleanup()

	const batchSize = 100
	prefixes := []string{"entity:", "event:", "knowledge:"}
	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		batch := make([]*cclient.Mutation, batchSize)
		for j := range batch {
			prefix := prefixes[(i*batchSize+j)%3]
			batch[j] = makeMutation(fmt.Sprintf("%s%08d", prefix, i*batchSize+j), 128)
		}
		if err := eng.Write("bench", batch); err != nil {
			b.Fatal(err)
		}
	}
}

// --- Scan benchmarks ---

// BenchmarkShoal_Scan_FullTable measures a full-table scan after
// writing N entries.
func BenchmarkShoal_Scan_FullTable(b *testing.B) {
	eng, cleanup := setupEngine(b, nil)
	defer cleanup()

	// Seed 10K entries
	seedEntries(b, eng, "entity:", 10_000, 128)
	eng.Flush("bench")
	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		sc, err := eng.Scan("bench", iterrt.InfiniteRange(), engine.ScanOptions{})
		if err != nil {
			b.Fatal(err)
		}
		count := 0
		for sc.Next() {
			count++
			if err := sc.Advance(); err != nil {
				b.Fatal(err)
			}
		}
		sc.Close()
		if count == 0 {
			b.Fatal("scan returned 0 results")
		}
	}
}

// BenchmarkShoal_Scan_PrefixRange measures a prefix-range scan
// (e.g. all entities) across a multi-tablet table.
func BenchmarkShoal_Scan_PrefixRange(b *testing.B) {
	splits := engine.PrefixSplit("entity:", "event:", "knowledge:")
	eng, cleanup := setupEngine(b, splits)
	defer cleanup()

	// Seed entries across all prefixes
	prefixes := []string{"entity:", "event:", "knowledge:"}
	for _, p := range prefixes {
		seedEntries(b, eng, p, 3_000, 128)
	}
	eng.Flush("bench")

	// Scan only entities
	startKey := &iterrt.Key{Row: []byte("entity:")}
	endKey := &iterrt.Key{Row: []byte("entity;")} // ':' + 1
	r := iterrt.Range{
		Start: startKey, StartInclusive: true,
		End: endKey, EndInclusive: false,
	}
	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		sc, err := eng.Scan("bench", r, engine.ScanOptions{})
		if err != nil {
			b.Fatal(err)
		}
		count := 0
		for sc.Next() {
			count++
			if err := sc.Advance(); err != nil {
				b.Fatal(err)
			}
		}
		sc.Close()
	}
}

// BenchmarkShoal_Scan_PointLookup measures single-row point lookups.
func BenchmarkShoal_Scan_PointLookup(b *testing.B) {
	eng, cleanup := setupEngine(b, nil)
	defer cleanup()

	seedEntries(b, eng, "entity:", 10_000, 128)
	eng.Flush("bench")

	rng := rand.New(rand.NewSource(42))
	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		rowID := rng.Intn(10_000)
		row := []byte(fmt.Sprintf("entity:%08d", rowID))
		key := &iterrt.Key{Row: row}
		// Exact row range
		r := iterrt.Range{
			Start: key, StartInclusive: true,
			End: key, EndInclusive: true,
		}
		sc, err := eng.Scan("bench", r, engine.ScanOptions{})
		if err != nil {
			b.Fatal(err)
		}
		for sc.Next() {
			sc.Advance()
		}
		sc.Close()
	}
}

// --- Graph traversal benchmarks ---
//
// These model the dominant knowledge-graph query: k-hop neighbor
// expansion (BFS) from a seed node. Each hop, for every node in the
// frontier, seeks the node's row and scans its "edge" column family to
// read out-neighbors, then chases those edges to the next hop. This is
// the compound point-lookup + within-row prefix-scan cost that a real
// graph search pays — far more representative of graph workloads than a
// single point-lookup micro-bench. Because shoal owns the RFile layout,
// this benchmark is also the target to optimize against if we later
// co-locate adjacency or add a per-node edge index to the format.

// graphHops is the BFS depth used by the traversal benchmarks.
const graphHops = 3

// BenchmarkShoal_Graph_KHopTraversal seeds a random directed graph
// (10K nodes, out-degree 8, adjacency stored as edge cells) and measures
// a graphHops-deep BFS from a random seed node per op.
func BenchmarkShoal_Graph_KHopTraversal(b *testing.B) {
	g := graphSpec{nodes: 10_000, degree: 8, seed: 1}
	eng, cleanup := setupEngine(b, nil)
	defer cleanup()

	seedGraph(b, eng, g, 128)
	eng.Flush("bench")

	edgeCF := [][]byte{[]byte("edge")}
	rng := rand.New(rand.NewSource(42))
	b.ResetTimer()

	var totalVisited int
	for i := 0; i < b.N; i++ {
		totalVisited += traverseShoal(b, eng, rng.Intn(g.nodes), graphHops, edgeCF)
	}
	b.StopTimer()
	if totalVisited == 0 {
		b.Fatal("traversal visited no nodes")
	}
}

// BenchmarkShoal_Graph_KHopTraversal_Parallel measures the same BFS as
// BenchmarkShoal_Graph_KHopTraversal but expands each hop's frontier
// concurrently across NumCPU workers — quantifying shoal's structural
// parallelism advantage over a single-threaded engine.
func BenchmarkShoal_Graph_KHopTraversal_Parallel(b *testing.B) {
	g := graphSpec{nodes: 10_000, degree: 8, seed: 1}
	eng, cleanup := setupEngine(b, nil)
	defer cleanup()

	seedGraph(b, eng, g, 128)
	eng.Flush("bench")

	edgeCF := [][]byte{[]byte("edge")}
	workers := runtime.NumCPU()
	rng := rand.New(rand.NewSource(42))
	b.ResetTimer()

	var totalVisited int
	for i := 0; i < b.N; i++ {
		totalVisited += traverseShoalParallel(b, eng, rng.Intn(g.nodes), graphHops, workers, edgeCF)
	}
	b.StopTimer()
	if totalVisited == 0 {
		b.Fatal("traversal visited no nodes")
	}
}

// BenchmarkShoal_Graph_KHopTraversal_Batch measures the same BFS using
// the LookupRows batch API — one source-stack open per hop instead of
// per node.
func BenchmarkShoal_Graph_KHopTraversal_Batch(b *testing.B) {
	g := graphSpec{nodes: 10_000, degree: 8, seed: 1}
	eng, cleanup := setupEngine(b, nil)
	defer cleanup()

	seedGraph(b, eng, g, 128)
	eng.Flush("bench")

	edgeCF := [][]byte{[]byte("edge")}
	rng := rand.New(rand.NewSource(42))
	b.ResetTimer()

	var totalVisited int
	for i := 0; i < b.N; i++ {
		totalVisited += traverseShoalBatch(b, eng, rng.Intn(g.nodes), graphHops, edgeCF)
	}
	b.StopTimer()
	if totalVisited == 0 {
		b.Fatal("traversal visited no nodes")
	}
}

// BenchmarkShoal_Graph_KHopTraversal_BatchParallel measures the combined
// #1+#3 path: parallel frontier expansion where each worker reuses one
// source stack across its chunk of rows.
func BenchmarkShoal_Graph_KHopTraversal_BatchParallel(b *testing.B) {
	g := graphSpec{nodes: 10_000, degree: 8, seed: 1}
	eng, cleanup := setupEngine(b, nil)
	defer cleanup()

	seedGraph(b, eng, g, 128)
	eng.Flush("bench")

	edgeCF := [][]byte{[]byte("edge")}
	workers := runtime.NumCPU()
	rng := rand.New(rand.NewSource(42))
	b.ResetTimer()

	var totalVisited int
	for i := 0; i < b.N; i++ {
		totalVisited += traverseShoalBatchParallel(b, eng, rng.Intn(g.nodes), graphHops, workers, edgeCF)
	}
	b.StopTimer()
	if totalVisited == 0 {
		b.Fatal("traversal visited no nodes")
	}
}

// BenchmarkShoal_Graph_KHopTraversal_Adjacency measures the same BFS
// served from the shoal.adjacency CSR index: each hop resolves its whole
// frontier with one engine.Neighbors call, which binary-searches each
// node's edge slice (no merge/versioning/relkey decode) and fans the
// per-node lookups out across NumCPU. This is the #1+#2+#3 path combined
// — the configuration meant to beat SQLite outright.
func BenchmarkShoal_Graph_KHopTraversal_Adjacency(b *testing.B) {
	g := graphSpec{nodes: 10_000, degree: 8, seed: 1}
	eng, cleanup := setupGraphEngine(b, nil)
	defer cleanup()

	seedGraph(b, eng, g, 128)
	eng.Flush("bench")

	edgeCF := []byte("edge")
	rng := rand.New(rand.NewSource(42))
	b.ResetTimer()

	var totalVisited int
	for i := 0; i < b.N; i++ {
		totalVisited += traverseShoalAdjacency(b, eng, rng.Intn(g.nodes), graphHops, edgeCF)
	}
	b.StopTimer()
	if totalVisited == 0 {
		b.Fatal("traversal visited no nodes")
	}
}

// --- Compaction benchmarks ---

// BenchmarkShoal_Compact measures compaction of multiple RFiles into one.
func BenchmarkShoal_Compact(b *testing.B) {
	for i := 0; i < b.N; i++ {
		b.StopTimer()
		eng, cleanup := setupEngine(b, nil)
		// Write enough to create multiple RFiles
		for batch := 0; batch < 5; batch++ {
			seedEntries(b, eng, "entity:", 1_000, 128)
			eng.Flush("bench")
		}
		b.StartTimer()

		if err := eng.Compact("bench", nil); err != nil {
			b.Fatal(err)
		}
		cleanup()
	}
}

// --- Parallel scan benchmarks (the key differentiator vs SQLite) ---

// BenchmarkShoal_ParallelScan_3Tablets measures concurrent scanning
// across 3 tablets — the parallelism advantage over SQLite.
func BenchmarkShoal_ParallelScan_3Tablets(b *testing.B) {
	splits := engine.PrefixSplit("entity:", "event:", "knowledge:")
	eng, cleanup := setupEngine(b, splits)
	defer cleanup()

	prefixes := []string{"entity:", "event:", "knowledge:"}
	for _, p := range prefixes {
		seedEntries(b, eng, p, 5_000, 128)
	}
	eng.Flush("bench")
	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		sc, err := eng.Scan("bench", iterrt.InfiniteRange(), engine.ScanOptions{})
		if err != nil {
			b.Fatal(err)
		}
		count := 0
		for sc.Next() {
			count++
			if err := sc.Advance(); err != nil {
				b.Fatal(err)
			}
		}
		sc.Close()
	}
}

// --- Realistic read-path benchmarks (with the system iterator stack) ---
//
// The Scan benchmarks above pass an empty Stack, so they measure the bare
// merge + cell decode — the storage-layer floor. A real Accumulo-model
// scan always applies system iterators at scan scope: VersioningIterator
// (newest-N per key) and a VisibilityFilter (auth-based cell drop). These
// variants stack both so we also see the production read path, which is
// the fairer comparison point against SQLite's single-row lookup.

// systemScanStack returns the scan-scope system iterator stack: keep the
// newest version per key, then apply visibility filtering.
func systemScanStack() []iterrt.IterSpec {
	return []iterrt.IterSpec{
		{Name: iterrt.IterVersioning, Options: map[string]string{iterrt.VersioningOption: "1"}},
		{Name: iterrt.IterVisibility},
	}
}

// BenchmarkShoal_Scan_FullTable_Versioned is Scan_FullTable with the
// system iterator stack (versioning + visibility) applied.
func BenchmarkShoal_Scan_FullTable_Versioned(b *testing.B) {
	eng, cleanup := setupEngine(b, nil)
	defer cleanup()

	seedEntries(b, eng, "entity:", 10_000, 128)
	eng.Flush("bench")
	opts := engine.ScanOptions{Stack: systemScanStack()}
	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		sc, err := eng.Scan("bench", iterrt.InfiniteRange(), opts)
		if err != nil {
			b.Fatal(err)
		}
		count := 0
		for sc.Next() {
			count++
			if err := sc.Advance(); err != nil {
				b.Fatal(err)
			}
		}
		sc.Close()
		if count == 0 {
			b.Fatal("scan returned 0 results")
		}
	}
}

// BenchmarkShoal_Scan_PointLookup_Versioned is Scan_PointLookup with the
// system iterator stack (versioning + visibility) applied — the realistic
// single-row read path.
func BenchmarkShoal_Scan_PointLookup_Versioned(b *testing.B) {
	eng, cleanup := setupEngine(b, nil)
	defer cleanup()

	seedEntries(b, eng, "entity:", 10_000, 128)
	eng.Flush("bench")
	opts := engine.ScanOptions{Stack: systemScanStack()}

	rng := rand.New(rand.NewSource(42))
	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		rowID := rng.Intn(10_000)
		row := []byte(fmt.Sprintf("entity:%08d", rowID))
		key := &iterrt.Key{Row: row}
		r := iterrt.Range{
			Start: key, StartInclusive: true,
			End: key, EndInclusive: true,
		}
		sc, err := eng.Scan("bench", r, opts)
		if err != nil {
			b.Fatal(err)
		}
		for sc.Next() {
			sc.Advance()
		}
		sc.Close()
	}
}

// --- Helpers ---

// benchVocab is a small word pool for synthesizing realistic, text-like
// property values — natural-language labels/descriptions with moderate
// entropy, the representative shape of knowledge-graph payloads. Used by
// the file-size comparison so both engines store the same realistic data.
var benchVocab = []string{
	"entity", "person", "organization", "location", "event", "document",
	"knowledge", "graph", "node", "edge", "relation", "concept", "topic",
	"the", "of", "and", "in", "with", "associated", "related", "primary",
	"secondary", "active", "inactive", "verified", "pending", "source",
	"target", "weight", "score", "label", "summary", "description", "title",
}

// realisticLabel fills n bytes with space-joined words from benchVocab.
func realisticLabel(rng *rand.Rand, n int) []byte {
	out := make([]byte, 0, n+16)
	for len(out) < n {
		if len(out) > 0 {
			out = append(out, ' ')
		}
		out = append(out, benchVocab[rng.Intn(len(benchVocab))]...)
	}
	return out[:n]
}

// dirRFileBytes returns the total size of every *.rf file beneath dir —
// the on-disk footprint of a flushed shoal table.
func dirRFileBytes(tb testing.TB, dir string) int64 {
	tb.Helper()
	var total int64
	err := filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if !info.IsDir() && filepath.Ext(path) == ".rf" {
			total += info.Size()
		}
		return nil
	})
	if err != nil {
		tb.Fatal(err)
	}
	return total
}

// benchLogger returns a logger that discards all output so engine INFO
// lines (flush/table-create) don't pollute benchmark result streams.
func benchLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// setupEngineSync is setupEngine with a configurable WAL durability tier
// so write benchmarks can compare fsync-per-write (SyncFull) against the
// group-commit tiers (SyncNormal, the closest analogue to SQLite's
// WAL-mode synchronous=NORMAL used by the SQLite baselines).
func setupEngineSync(tb testing.TB, splits [][]byte, mode localwal.SyncMode, interval time.Duration) (*engine.Engine, func()) {
	tb.Helper()
	dir, err := os.MkdirTemp("", "shoal-bench-*")
	if err != nil {
		tb.Fatal(err)
	}
	eng, err := engine.Open(dir, engine.Options{
		Logger:          benchLogger(),
		WALSyncMode:     mode,
		WALSyncInterval: interval,
	})
	if err != nil {
		os.RemoveAll(dir)
		tb.Fatal(err)
	}
	opts := engine.TableOptions{}
	if splits != nil {
		opts.Splits = splits
	}
	if err := eng.CreateTable("bench", opts); err != nil {
		eng.Close()
		os.RemoveAll(dir)
		tb.Fatal(err)
	}
	return eng, func() {
		eng.Close()
		os.RemoveAll(dir)
	}
}

func setupEngine(tb testing.TB, splits [][]byte) (*engine.Engine, func()) {
	tb.Helper()
	dir, err := os.MkdirTemp("", "shoal-bench-*")
	if err != nil {
		tb.Fatal(err)
	}
	eng, err := engine.Open(dir, engine.Options{Logger: benchLogger()})
	if err != nil {
		os.RemoveAll(dir)
		tb.Fatal(err)
	}
	opts := engine.TableOptions{}
	if splits != nil {
		opts.Splits = splits
	}
	if err := eng.CreateTable("bench", opts); err != nil {
		eng.Close()
		os.RemoveAll(dir)
		tb.Fatal(err)
	}
	return eng, func() {
		eng.Close()
		os.RemoveAll(dir)
	}
}

// setupGraphEngine is setupEngine with the shoal.adjacency out-edge index
// enabled for the "edge" column family, so flush + compaction emit the CSR
// index that engine.Neighbors consults.
func setupGraphEngine(tb testing.TB, splits [][]byte) (*engine.Engine, func()) {
	tb.Helper()
	dir, err := os.MkdirTemp("", "shoal-bench-*")
	if err != nil {
		tb.Fatal(err)
	}
	eng, err := engine.Open(dir, engine.Options{Logger: benchLogger()})
	if err != nil {
		os.RemoveAll(dir)
		tb.Fatal(err)
	}
	opts := engine.TableOptions{
		TabletOptions: tablet.Options{AdjacencyEdgeCF: "edge"},
	}
	if splits != nil {
		opts.Splits = splits
	}
	if err := eng.CreateTable("bench", opts); err != nil {
		eng.Close()
		os.RemoveAll(dir)
		tb.Fatal(err)
	}
	return eng, func() {
		eng.Close()
		os.RemoveAll(dir)
	}
}

func generateMutations(n int, prefix string, valSize int) []*cclient.Mutation {
	muts := make([]*cclient.Mutation, n)
	val := make([]byte, valSize)
	for i := range val {
		val[i] = byte(i % 256)
	}
	for i := range muts {
		muts[i] = makeMutation(fmt.Sprintf("%s%08d", prefix, i), valSize)
	}
	return muts
}

func makeMutation(row string, valSize int) *cclient.Mutation {
	m, _ := cclient.NewMutation([]byte(row))
	val := make([]byte, valSize)
	for i := range val {
		val[i] = byte(i % 256)
	}
	m.Put([]byte("props"), []byte("label"), nil, cclient.MutationLatestTimestamp, val)
	m.Put([]byte("props"), []byte("type"), nil, cclient.MutationLatestTimestamp, []byte("entity"))
	m.Put([]byte("props"), []byte("salience"), nil, cclient.MutationLatestTimestamp, []byte("0.85"))
	return m
}

func seedEntries(tb testing.TB, eng *engine.Engine, prefix string, count, valSize int) {
	tb.Helper()
	const batchSize = 500
	for i := 0; i < count; i += batchSize {
		end := i + batchSize
		if end > count {
			end = count
		}
		batch := make([]*cclient.Mutation, end-i)
		for j := range batch {
			batch[j] = makeMutation(fmt.Sprintf("%s%08d", prefix, i+j), valSize)
		}
		if err := eng.Write("bench", batch); err != nil {
			tb.Fatal(err)
		}
	}
}

// --- Graph helpers (shared by shoal + SQLite traversal benchmarks) ---

// graphSpec describes a deterministic random directed graph. The same
// spec produces byte-identical adjacency in both the shoal and SQLite
// benchmarks, so the head-to-head compares identical work.
type graphSpec struct {
	nodes  int
	degree int
	seed   int64
}

// neighbors returns node i's out-neighbor ids, generated deterministically
// from (seed, i) so the result is independent of seeding order and
// identical across engines.
func (g graphSpec) neighbors(i int) []int {
	rng := rand.New(rand.NewSource(g.seed + int64(i)))
	out := make([]int, g.degree)
	for j := range out {
		out[j] = rng.Intn(g.nodes)
	}
	return out
}

// edgeWeight is the small fixed payload stored on every edge cell.
var edgeWeight = []byte{0, 0, 0, 1}

// makeGraphMutation builds one node: its three prop cells (mirroring
// makeMutation) plus one edge cell per out-neighbor under CF "edge",
// CQ = the neighbor's row key.
func makeGraphMutation(row string, neighbors []int, valSize int) *cclient.Mutation {
	m, _ := cclient.NewMutation([]byte(row))
	val := make([]byte, valSize)
	for i := range val {
		val[i] = byte(i % 256)
	}
	m.Put([]byte("props"), []byte("label"), nil, cclient.MutationLatestTimestamp, val)
	m.Put([]byte("props"), []byte("type"), nil, cclient.MutationLatestTimestamp, []byte("entity"))
	m.Put([]byte("props"), []byte("salience"), nil, cclient.MutationLatestTimestamp, []byte("0.85"))
	for _, t := range neighbors {
		m.Put([]byte("edge"), []byte(fmt.Sprintf("entity:%08d", t)), nil, cclient.MutationLatestTimestamp, edgeWeight)
	}
	return m
}

// seedGraph writes g.nodes nodes (props + edges) into the bench table.
func seedGraph(tb testing.TB, eng *engine.Engine, g graphSpec, valSize int) {
	tb.Helper()
	const batchSize = 500
	for i := 0; i < g.nodes; i += batchSize {
		end := i + batchSize
		if end > g.nodes {
			end = g.nodes
		}
		batch := make([]*cclient.Mutation, end-i)
		for j := range batch {
			node := i + j
			batch[j] = makeGraphMutation(fmt.Sprintf("entity:%08d", node), g.neighbors(node), valSize)
		}
		if err := eng.Write("bench", batch); err != nil {
			tb.Fatal(err)
		}
	}
}

// entityID parses the integer id out of an "entity:%08d" row/qualifier.
func entityID(b []byte) int {
	const prefix = "entity:"
	if len(b) <= len(prefix) {
		return -1
	}
	n, err := strconv.Atoi(string(b[len(prefix):]))
	if err != nil {
		return -1
	}
	return n
}

// neighborsShoal fetches one node's out-neighbor ids by scanning its
// edge column family. Safe for concurrent use: each call builds its own
// independent Scanner over the (concurrency-safe) engine.
func neighborsShoal(b *testing.B, eng *engine.Engine, node int, edgeCF [][]byte) []int {
	row := []byte(fmt.Sprintf("entity:%08d", node))
	// Whole-row range: [{row}, {row\x00}) covers every cell in the
	// row (a bare {row} key sorts before the row's non-empty-CF
	// cells, so it can't serve as an inclusive upper bound).
	start := &iterrt.Key{Row: row}
	end := &iterrt.Key{Row: append(append([]byte{}, row...), 0x00)}
	r := iterrt.Range{Start: start, StartInclusive: true, End: end, EndInclusive: false}
	sc, err := eng.Scan("bench", r, engine.ScanOptions{
		ColumnFamilies:          edgeCF,
		ColumnFamiliesInclusive: true,
	})
	if err != nil {
		b.Fatal(err)
	}
	var out []int
	for sc.Next() {
		if t := entityID(sc.Key().ColumnQualifier); t >= 0 {
			out = append(out, t)
		}
		sc.Advance()
	}
	sc.Close()
	return out
}

// traverseShoal runs a hops-deep BFS from start, expanding each frontier
// node by scanning its edge column family. Returns the number of distinct
// nodes visited.
func traverseShoal(b *testing.B, eng *engine.Engine, start, hops int, edgeCF [][]byte) int {
	b.Helper()
	visited := map[int]bool{start: true}
	frontier := []int{start}
	for h := 0; h < hops && len(frontier) > 0; h++ {
		var next []int
		for _, node := range frontier {
			for _, t := range neighborsShoal(b, eng, node, edgeCF) {
				if !visited[t] {
					visited[t] = true
					next = append(next, t)
				}
			}
		}
		frontier = next
	}
	return len(visited)
}

// traverseShoalParallel runs the same BFS as traverseShoal but expands
// each hop's frontier concurrently across up to `workers` goroutines.
// Neighbor fetches (the I/O-bound part) run in parallel; dedup into the
// shared visited set happens sequentially at the hop boundary, so the
// visited set is identical to the sequential traversal — same logical
// work, just parallel reads. This is shoal's structural edge over a
// single-threaded engine like SQLite.
func traverseShoalParallel(b *testing.B, eng *engine.Engine, start, hops, workers int, edgeCF [][]byte) int {
	b.Helper()
	visited := map[int]bool{start: true}
	frontier := []int{start}
	for h := 0; h < hops && len(frontier) > 0; h++ {
		results := make([][]int, len(frontier))
		sem := make(chan struct{}, workers)
		var wg sync.WaitGroup
		for idx, node := range frontier {
			wg.Add(1)
			sem <- struct{}{}
			go func(idx, node int) {
				defer wg.Done()
				defer func() { <-sem }()
				results[idx] = neighborsShoal(b, eng, node, edgeCF)
			}(idx, node)
		}
		wg.Wait()

		var next []int
		for _, ns := range results {
			for _, t := range ns {
				if !visited[t] {
					visited[t] = true
					next = append(next, t)
				}
			}
		}
		frontier = next
	}
	return len(visited)
}

// traverseShoalBatch runs the same BFS as traverseShoal but expands each
// hop's entire frontier with ONE LookupRows call — opening the tablet
// source stack once per hop instead of once per node.
func traverseShoalBatch(b *testing.B, eng *engine.Engine, start, hops int, edgeCF [][]byte) int {
	b.Helper()
	visited := map[int]bool{start: true}
	frontier := []int{start}
	opts := engine.ScanOptions{ColumnFamilies: edgeCF, ColumnFamiliesInclusive: true}
	for h := 0; h < hops && len(frontier) > 0; h++ {
		rows := make([][]byte, len(frontier))
		for i, node := range frontier {
			rows[i] = []byte(fmt.Sprintf("entity:%08d", node))
		}
		var collected []int
		err := eng.LookupRows("bench", rows, opts, func(_ int, k *iterrt.Key, _ []byte) {
			if t := entityID(k.ColumnQualifier); t >= 0 {
				collected = append(collected, t)
			}
		})
		if err != nil {
			b.Fatal(err)
		}
		var next []int
		for _, t := range collected {
			if !visited[t] {
				visited[t] = true
				next = append(next, t)
			}
		}
		frontier = next
	}
	return len(visited)
}

// traverseShoalBatchParallel combines #1 (reused stack via LookupRows)
// and #3 (parallel frontier): each hop's frontier is split across workers,
// and each worker resolves its chunk with one reused source stack. This
// both amortizes stack setup AND spreads the irreducible per-row
// seek+decode cost across cores.
func traverseShoalBatchParallel(b *testing.B, eng *engine.Engine, start, hops, workers int, edgeCF [][]byte) int {
	b.Helper()
	visited := map[int]bool{start: true}
	frontier := []int{start}
	opts := engine.ScanOptions{ColumnFamilies: edgeCF, ColumnFamiliesInclusive: true}
	for h := 0; h < hops && len(frontier) > 0; h++ {
		chunks := chunkNodes(frontier, workers)
		results := make([][]int, len(chunks))
		var wg sync.WaitGroup
		for ci := range chunks {
			wg.Add(1)
			go func(ci int) {
				defer wg.Done()
				chunk := chunks[ci]
				rows := make([][]byte, len(chunk))
				for i, node := range chunk {
					rows[i] = []byte(fmt.Sprintf("entity:%08d", node))
				}
				var local []int
				if err := eng.LookupRows("bench", rows, opts, func(_ int, k *iterrt.Key, _ []byte) {
					if t := entityID(k.ColumnQualifier); t >= 0 {
						local = append(local, t)
					}
				}); err != nil {
					b.Error(err)
				}
				results[ci] = local
			}(ci)
		}
		wg.Wait()

		var next []int
		for _, ns := range results {
			for _, t := range ns {
				if !visited[t] {
					visited[t] = true
					next = append(next, t)
				}
			}
		}
		frontier = next
	}
	return len(visited)
}

// traverseShoalAdjacency runs the hops-deep BFS expanding each hop's
// entire frontier with a single engine.Neighbors call — served from the
// shoal.adjacency index and fanned out across cores internally.
func traverseShoalAdjacency(b *testing.B, eng *engine.Engine, start, hops int, edgeCF []byte) int {
	b.Helper()
	visited := map[int]bool{start: true}
	frontier := []int{start}
	for h := 0; h < hops && len(frontier) > 0; h++ {
		rows := make([][]byte, len(frontier))
		for i, node := range frontier {
			rows[i] = []byte(fmt.Sprintf("entity:%08d", node))
		}
		results, err := eng.Neighbors("bench", rows, edgeCF, engine.ScanOptions{})
		if err != nil {
			b.Fatal(err)
		}
		var next []int
		for _, ns := range results {
			for _, n := range ns {
				if t := entityID(n.Target); t >= 0 && !visited[t] {
					visited[t] = true
					next = append(next, t)
				}
			}
		}
		frontier = next
	}
	return len(visited)
}

// chunkNodes splits nodes into at most n roughly-equal contiguous chunks
// (empty chunks omitted).
func chunkNodes(nodes []int, n int) [][]int {
	if n < 1 {
		n = 1
	}
	if len(nodes) < n {
		n = len(nodes)
	}
	if n == 0 {
		return nil
	}
	chunks := make([][]int, 0, n)
	size := (len(nodes) + n - 1) / n
	for i := 0; i < len(nodes); i += size {
		end := i + size
		if end > len(nodes) {
			end = len(nodes)
		}
		chunks = append(chunks, nodes[i:end])
	}
	return chunks
}

// --- SQLite comparison stubs ---
// These document the equivalent SQLite operations that a consumer performs
// today. The Go stubs below are placeholders for a future cgo-sqlite
// benchmark that would test under identical conditions.

func BenchmarkSQLite_Write_Stub(b *testing.B) {
	b.Skip("SQLite comparison requires cgo build with -tags sqlite")
}

func BenchmarkSQLite_Scan_Stub(b *testing.B) {
	b.Skip("SQLite comparison requires cgo build with -tags sqlite")
}

// --- Benchmark result annotation ---

func init() {
	// Print comparison context when benchmarks run
	if testing.Testing() {
		return
	}
}

// BenchmarkReadme documents what each benchmark measures.
//
//	Write_SingleTablet:       Single-tablet ingestion (no routing)
//	Write_MultiTablet:        3-tablet ingestion with prefix routing
//	WriteBatch:               100-mutation batches across 3 tablets
//	Scan_FullTable:           Full scan after 10K entries + flush
//	Scan_PrefixRange:         Prefix scan (entity: only) across 3 tablets
//	Scan_PointLookup:         Random single-row lookups
//	Compact:                  Merge 5 RFiles into 1
//	ParallelScan_3Tablets:    Full scan across 3 tablets (parallel merge)
//
// Expected advantages over SQLite:
//   - Write: comparable (both are append-to-WAL + memtable/WAL-mode)
//   - Scan: faster for prefix scans (tablet-level skip vs row scan)
//   - Parallel: significantly faster (3 goroutines vs 1 SQLite reader)
//   - Compact: no SQLite equivalent (VACUUM is the closest, but different)
var _ = filepath.Join // keep filepath import for bench temp dirs
