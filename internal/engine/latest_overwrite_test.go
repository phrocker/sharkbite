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
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/phrocker/shoal-oss/internal/cclient"
	"github.com/phrocker/shoal-oss/internal/embeddingspace"
	"github.com/phrocker/shoal-oss/internal/engine"
	"github.com/phrocker/shoal-oss/internal/iterrt"
	"github.com/phrocker/shoal-oss/internal/localwal"
	"github.com/phrocker/shoal-oss/internal/storage/memory"
	"github.com/phrocker/shoal-oss/internal/tablet"
)

// Issue #692: PutLatest stamps every write with ts=MaxInt64, so every
// overwrite of one cell is the SAME full key. When those overwrites live in
// different sources (memtable and immutable files), the read path must still
// surface the newest one first. These tests overwrite one cell repeatedly,
// interleaving flushes and compactions in every order, then read it through
// the normal versioned read path.

const overwriteTable = "overwrites"

// overwriteStep is what happens after one PutLatest write.
type overwriteStep int

const (
	stepNone    overwriteStep = iota // value stays in the memtable
	stepFlush                        // value flushed to its own file
	stepCompact                      // flush, then compact every file into one
)

func (s overwriteStep) String() string {
	return [...]string{"mem", "flush", "compact"}[s]
}

func putLatest(t *testing.T, eng *engine.Engine, row, value string) {
	t.Helper()
	m, err := cclient.NewMutation([]byte(row))
	if err != nil {
		t.Fatal(err)
	}
	m.PutLatest([]byte("cf"), []byte("cq"), nil, []byte(value))
	if err := eng.Write(overwriteTable, []*cclient.Mutation{m}); err != nil {
		t.Fatal(err)
	}
}

func applyStep(t *testing.T, eng *engine.Engine, step overwriteStep) {
	t.Helper()
	switch step {
	case stepFlush:
		if err := eng.Flush(overwriteTable); err != nil {
			t.Fatal(err)
		}
	case stepCompact:
		if err := eng.Flush(overwriteTable); err != nil {
			t.Fatal(err)
		}
		if err := eng.Compact(overwriteTable, nil); err != nil {
			t.Fatal(err)
		}
	}
}

// readLatest returns the values the versioned read path surfaces for row,
// newest first.
func readLatest(t *testing.T, eng *engine.Engine, row string, maxVersions int) []string {
	t.Helper()
	sc, err := eng.Scan(overwriteTable, iterrt.InfiniteRange(), engine.ScanOptions{
		Stack: []iterrt.IterSpec{{
			Name:    iterrt.IterVersioning,
			Options: map[string]string{iterrt.VersioningOption: fmt.Sprint(maxVersions)},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer sc.Close()
	var out []string
	for sc.Next() {
		if string(sc.Key().Row) == row {
			out = append(out, string(sc.Value()))
		}
		if err := sc.Advance(); err != nil {
			t.Fatal(err)
		}
	}
	return out
}

// overwritePatterns enumerates every assignment of a step to each of n
// writes, plus whether a final compaction runs after the last write.
func overwritePatterns(n int) [][]overwriteStep {
	var out [][]overwriteStep
	var rec func(prefix []overwriteStep)
	rec = func(prefix []overwriteStep) {
		if len(prefix) == n {
			out = append(out, append([]overwriteStep(nil), prefix...))
			return
		}
		for _, s := range []overwriteStep{stepNone, stepFlush, stepCompact} {
			rec(append(prefix, s))
		}
	}
	rec(nil)
	return out
}

func patternName(p []overwriteStep) string {
	parts := make([]string, len(p))
	for i, s := range p {
		parts[i] = s.String()
	}
	return strings.Join(parts, "-")
}

func openOverwriteEngine(t *testing.T, dir string, splits [][]byte) *engine.Engine {
	t.Helper()
	eng, err := engine.Open(dir, engine.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if err := eng.CreateTable(overwriteTable, engine.TableOptions{Splits: splits}); err != nil {
		t.Fatal(err)
	}
	return eng
}

// TestPutLatestOverwriteAcrossSourcesReadsNewest overwrites one cell four
// times with PutLatest, following each write with every combination of
// staying in the memtable, a flush, or a flush plus full compaction, and
// requires the read path to return the last value every time. It also
// requires that maxVersions>1 reports the overwrites newest first.
func TestPutLatestOverwriteAcrossSourcesReadsNewest(t *testing.T) {
	const writes = 4
	var failed []string
	for _, pattern := range overwritePatterns(writes) {
		pattern := pattern
		ok := t.Run(patternName(pattern), func(t *testing.T) {
			eng := openOverwriteEngine(t, t.TempDir(), nil)
			defer eng.Close()
			// A second, untouched cell keeps each file non-trivial.
			putLatest(t, eng, "other", "x")
			for i, step := range pattern {
				putLatest(t, eng, "cell", fmt.Sprintf("v%d", i+1))
				applyStep(t, eng, step)
				want := fmt.Sprintf("v%d", i+1)
				if got := readLatest(t, eng, "cell", 1); len(got) != 1 || got[0] != want {
					t.Fatalf("after write %d (%s): latest = %v, want [%s]", i+1, step, got, want)
				}
			}
			got := readLatest(t, eng, "cell", writes)
			if len(got) == 0 || got[0] != fmt.Sprintf("v%d", writes) {
				t.Fatalf("maxVersions=%d: got %v, want newest v%d first", writes, got, writes)
			}
			// Flush and nil-stack compaction deliberately keep every
			// identical-key copy (no collapse): pkg/explorer reads the
			// pre-delete copy of an interaction record that a later
			// same-key write superseded. Pin that retention.
			if len(got) != writes {
				t.Fatalf("maxVersions=%d: retained %d copies %v, want all %d", writes, len(got), got, writes)
			}
			// Duplicates of one key must come newest first.
			for i := 1; i < len(got); i++ {
				if got[i-1] < got[i] {
					t.Fatalf("maxVersions=%d: versions not newest first: %v", writes, got)
				}
			}
			if got := readLatest(t, eng, "other", 1); len(got) != 1 || got[0] != "x" {
				t.Fatalf("untouched cell: got %v, want [x]", got)
			}
		})
		if !ok {
			failed = append(failed, patternName(pattern))
		}
	}
	if len(failed) > 0 {
		t.Logf("%d/%d patterns returned a stale value: %v", len(failed), len(overwritePatterns(writes)), failed)
	}
}

// TestPutLatestOverwriteSurvivesReopen checks that the recency of immutable
// files is durable: after a reopen (files rediscovered from files.json, the
// memtable rebuilt from the WAL) the newest overwrite still wins.
func TestPutLatestOverwriteSurvivesReopen(t *testing.T) {
	for _, pattern := range overwritePatterns(3) {
		pattern := pattern
		t.Run(patternName(pattern), func(t *testing.T) {
			dir := t.TempDir()
			eng := openOverwriteEngine(t, dir, nil)
			for i, step := range pattern {
				putLatest(t, eng, "cell", fmt.Sprintf("v%d", i+1))
				applyStep(t, eng, step)
			}
			// Leave the last value only in the WAL-backed memtable.
			putLatest(t, eng, "cell", "final")
			// Simulate a crash: do not Close (which would flush).
			reopened, err := engine.Open(dir, engine.Options{})
			if err != nil {
				t.Fatal(err)
			}
			defer reopened.Close()
			defer eng.Close()
			got := readLatestOn(t, reopened, "cell")
			if got != "final" {
				t.Fatalf("after reopen: latest = %q, want final", got)
			}
			if err := reopened.Flush(overwriteTable); err != nil {
				t.Fatal(err)
			}
			if got := readLatestOn(t, reopened, "cell"); got != "final" {
				t.Fatalf("after reopen+flush: latest = %q, want final", got)
			}
			if err := reopened.Compact(overwriteTable, nil); err != nil {
				t.Fatal(err)
			}
			if got := readLatestOn(t, reopened, "cell"); got != "final" {
				t.Fatalf("after reopen+compact: latest = %q, want final", got)
			}
		})
	}
}

func readLatestOn(t *testing.T, eng *engine.Engine, row string) string {
	t.Helper()
	got := readLatest(t, eng, row, 1)
	if len(got) != 1 {
		t.Fatalf("row %s: got %d values %v, want 1", row, len(got), got)
	}
	return got[0]
}

// TestPutLatestOverwriteMultiTablet runs the overwrite sequence on a split
// table, where the engine merges per-tablet scanners on top of each tablet's
// own memtable+file merge.
func TestPutLatestOverwriteMultiTablet(t *testing.T) {
	eng := openOverwriteEngine(t, t.TempDir(), [][]byte{[]byte("m")})
	defer eng.Close()
	for _, row := range []string{"cell", "zcell"} {
		for i, step := range []overwriteStep{stepFlush, stepNone, stepFlush, stepCompact, stepNone} {
			putLatest(t, eng, row, fmt.Sprintf("v%d", i+1))
			applyStep(t, eng, step)
			if got := readLatestOn(t, eng, row); got != fmt.Sprintf("v%d", i+1) {
				t.Fatalf("%s write %d: latest = %q", row, i+1, got)
			}
		}
	}
}

// TestBackToBackFlushesKeepEveryFile flushes distinct cells in quick
// succession. Flush file names are derived from the wall clock in
// milliseconds; two flushes in the same millisecond must not overwrite one
// another's file.
func TestBackToBackFlushesKeepEveryFile(t *testing.T) {
	// A memory backend and an unsynced WAL make flushes fast enough that
	// several land in the same millisecond.
	eng, err := engine.Open(t.TempDir(), engine.Options{
		Backend:     memory.New(),
		WALSyncMode: localwal.SyncOff,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer eng.Close()
	if err := eng.CreateTable(overwriteTable, engine.TableOptions{}); err != nil {
		t.Fatal(err)
	}
	const n = 20
	for i := 0; i < n; i++ {
		putLatest(t, eng, fmt.Sprintf("row%02d", i), "v")
		if err := eng.Flush(overwriteTable); err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < n; i++ {
		row := fmt.Sprintf("row%02d", i)
		if got := readLatest(t, eng, row, 1); len(got) != 1 {
			t.Fatalf("%s: got %v after back-to-back flushes, want one value", row, got)
		}
	}
}

// TestPutLatestOverwriteAcrossEmbeddingRuns: compaction never merges files
// with different embedding states, so a mixed tablet compacts into several
// outputs. Each output must keep its inputs' place in the recency order —
// merging non-adjacent same-state files (A1, B2, A3 -> A13 + B2) would let
// B2's value outrank A3's.
func TestPutLatestOverwriteAcrossEmbeddingRuns(t *testing.T) {
	a, b := embeddingspace.Has("space-a"), embeddingspace.Has("space-b")
	cases := []struct {
		states   []embeddingspace.FileState
		wantRuns int
	}{
		{[]embeddingspace.FileState{a, b, a}, 3},
		{[]embeddingspace.FileState{a, a, b}, 2},
		{[]embeddingspace.FileState{b, a, a}, 2},
		{[]embeddingspace.FileState{a, b, b, a}, 3},
		{[]embeddingspace.FileState{b, a, b, a, b}, 5},
		{[]embeddingspace.FileState{a, a, a}, 1},
	}
	for _, tc := range cases {
		tc := tc
		var name []string
		for _, s := range tc.states {
			name = append(name, strings.TrimPrefix(s.String(), "has_embeddings:space-"))
		}
		t.Run(strings.Join(name, "-"), func(t *testing.T) {
			dir := t.TempDir()
			eng := openOverwriteEngine(t, dir, nil)
			for i, state := range tc.states {
				if err := eng.SetTableDefaultEmbedding(overwriteTable, state); err != nil {
					t.Fatal(err)
				}
				putLatest(t, eng, "cell", fmt.Sprintf("v%d", i+1))
				// A second cell written only by the first file.
				if i == 0 {
					putLatest(t, eng, "first", "only")
				}
				applyStep(t, eng, stepFlush)
			}
			want := fmt.Sprintf("v%d", len(tc.states))
			for round := 0; round < 2; round++ {
				if err := eng.Compact(overwriteTable, nil); err != nil {
					t.Fatal(err)
				}
				if got := readLatestOn(t, eng, "cell"); got != want {
					t.Fatalf("round %d: latest = %q, want %q", round, got, want)
				}
				if got := readLatestOn(t, eng, "first"); got != "only" {
					t.Fatalf("round %d: untouched cell = %q", round, got)
				}
			}
			snapshot, err := eng.TableEmbeddingStateSnapshot(context.Background(), overwriteTable)
			if err != nil {
				t.Fatal(err)
			}
			if len(snapshot.Files) != tc.wantRuns {
				t.Fatalf("compacted into %d files, want one per run (%d)", len(snapshot.Files), tc.wantRuns)
			}
			if err := eng.Close(); err != nil {
				t.Fatal(err)
			}
			reopened, err := engine.Open(dir, engine.Options{})
			if err != nil {
				t.Fatal(err)
			}
			defer reopened.Close()
			if got := readLatestOn(t, reopened, "cell"); got != want {
				t.Fatalf("after reopen: latest = %q, want %q", got, want)
			}
		})
	}
}

// TestConditionalWriteSeesNewestOverwrite: the CAS read inside
// ConditionalWrite uses the same merge, so a stale tie would make a
// compare-and-set against the current value fail and one against an
// overwritten value succeed.
func TestConditionalWriteSeesNewestOverwrite(t *testing.T) {
	eng := openOverwriteEngine(t, t.TempDir(), nil)
	defer eng.Close()
	for i := 1; i <= 3; i++ {
		putLatest(t, eng, "cell", fmt.Sprintf("v%d", i))
		applyStep(t, eng, stepFlush)
	}
	cas := func(expect, next string) bool {
		t.Helper()
		m, err := cclient.NewMutation([]byte("cell"))
		if err != nil {
			t.Fatal(err)
		}
		m.PutLatest([]byte("cf"), []byte("cq"), nil, []byte(next))
		ok, err := eng.ConditionalWrite(overwriteTable, []engine.ConditionalMutation{{
			Mutation: m,
			Conditions: []engine.Condition{{
				ColumnFamily:    []byte("cf"),
				ColumnQualifier: []byte("cq"),
				Kind:            engine.ConditionValueEquals,
				Value:           []byte(expect),
			}},
		}})
		if err != nil {
			t.Fatal(err)
		}
		return ok[0]
	}
	if cas("v1", "stale-cas") {
		t.Fatal("CAS against an overwritten value v1 succeeded")
	}
	if !cas("v3", "v4") {
		t.Fatal("CAS against the current value v3 failed")
	}
	if got := readLatestOn(t, eng, "cell"); got != "v4" {
		t.Fatalf("latest = %q, want v4", got)
	}
}

// TestNeighborsPutLatestOverwrite: the adjacency-index fast path resolves
// an edge rewritten with PutLatest to its latest value, like a scan.
func TestNeighborsPutLatestOverwrite(t *testing.T) {
	for _, pattern := range overwritePatterns(3) {
		pattern := pattern
		t.Run(patternName(pattern), func(t *testing.T) {
			eng, err := engine.Open(t.TempDir(), engine.Options{})
			if err != nil {
				t.Fatal(err)
			}
			defer eng.Close()
			if err := eng.CreateTable(overwriteTable, engine.TableOptions{
				TabletOptions: tablet.Options{AdjacencyEdgeCF: "edge"},
			}); err != nil {
				t.Fatal(err)
			}
			for i, step := range pattern {
				m, err := cclient.NewMutation([]byte("node"))
				if err != nil {
					t.Fatal(err)
				}
				m.PutLatest([]byte("edge"), []byte("target"), nil, []byte(fmt.Sprintf("w%d", i+1)))
				if err := eng.Write(overwriteTable, []*cclient.Mutation{m}); err != nil {
					t.Fatal(err)
				}
				applyStep(t, eng, step)
				got, err := eng.Neighbors(overwriteTable, [][]byte{[]byte("node")}, []byte("edge"), engine.ScanOptions{})
				if err != nil {
					t.Fatal(err)
				}
				want := fmt.Sprintf("w%d", i+1)
				if len(got[0]) != 1 || string(got[0][0].Value) != want {
					t.Fatalf("after write %d (%s): neighbors = %+v, want one edge %s", i+1, step, got[0], want)
				}
			}
		})
	}
}

// TestExportImportPreservesOverwriteRecency: an import stores files under
// content-addressed names whose sort order is unrelated to their age, so
// the export manifest's order is the only recency the importer has. Both
// the first import (new table) and a fan-in import into the open table
// must resolve the overwritten cell to its latest value.
func TestExportImportPreservesOverwriteRecency(t *testing.T) {
	ctx := context.Background()
	for attempt := 0; attempt < 8; attempt++ {
		srcDir := filepath.Join(t.TempDir(), "src")
		dstDir := filepath.Join(t.TempDir(), "dst")
		src := openOverwriteEngine(t, srcDir, nil)
		for i := 1; i <= 4; i++ {
			putLatest(t, src, "cell", fmt.Sprintf("v%d", i))
			// A unique salt cell gives every file (and so its content-addressed
			// import name) a fresh hash, so name order varies per attempt.
			putLatest(t, src, "salt", fmt.Sprintf("%d-%d-%d", attempt, i, time.Now().UnixNano()))
			applyStep(t, src, stepFlush)
		}
		dstBackend := memory.New()
		export := func() *engine.RFileExportManifest {
			t.Helper()
			manifest, err := src.ExportRFiles(ctx, overwriteTable, dstBackend, engine.RFileExportOptions{
				DestinationRoot: dstDir,
				EngineVersion:   "test",
			})
			if err != nil {
				t.Fatal(err)
			}
			return manifest
		}
		dst, err := engine.Open(dstDir, engine.Options{Backend: dstBackend})
		if err != nil {
			t.Fatal(err)
		}
		if err := dst.ImportRFileManifest(ctx, export()); err != nil {
			t.Fatal(err)
		}
		if got := readLatestOn(t, dst, "cell"); got != "v4" {
			t.Fatalf("attempt %d: imported latest = %q, want v4", attempt, got)
		}
		// Fan-in: more overwrites on the source, re-exported and merged
		// into the already-open imported table.
		for i := 5; i <= 6; i++ {
			putLatest(t, src, "cell", fmt.Sprintf("v%d", i))
			// A unique salt cell gives every file (and so its content-addressed
			// import name) a fresh hash, so name order varies per attempt.
			putLatest(t, src, "salt", fmt.Sprintf("%d-%d-%d", attempt, i, time.Now().UnixNano()))
			applyStep(t, src, stepFlush)
		}
		if err := dst.ImportRFileManifest(ctx, export()); err != nil {
			t.Fatal(err)
		}
		if got := readLatestOn(t, dst, "cell"); got != "v6" {
			t.Fatalf("attempt %d: fan-in latest = %q, want v6", attempt, got)
		}
		if err := dst.Close(); err != nil {
			t.Fatal(err)
		}
		reopened, err := engine.Open(dstDir, engine.Options{Backend: dstBackend})
		if err != nil {
			t.Fatal(err)
		}
		if got := readLatestOn(t, reopened, "cell"); got != "v6" {
			t.Fatalf("attempt %d: reopened import latest = %q, want v6", attempt, got)
		}
		_ = reopened.Close()
		_ = src.Close()
	}
}

// TestReexportKeepsRecencyWhenNamesDisagree: an engine that imported a
// producer's file after flushing its own holds a newer file whose name
// (".shoal-import-...") sorts BEFORE its older flush ("F..."). Re-exporting
// it must carry the recency order, not the name order, to the next engine.
func TestReexportKeepsRecencyWhenNamesDisagree(t *testing.T) {
	ctx := context.Background()
	exportTo := func(src *engine.Engine, dst *memory.Backend, root string) *engine.RFileExportManifest {
		t.Helper()
		manifest, err := src.ExportRFiles(ctx, overwriteTable, dst, engine.RFileExportOptions{
			DestinationRoot: root,
			EngineVersion:   "test",
		})
		if err != nil {
			t.Fatal(err)
		}
		return manifest
	}
	openOn := func(dir string, backend *memory.Backend) *engine.Engine {
		t.Helper()
		eng, err := engine.Open(dir, engine.Options{Backend: backend})
		if err != nil {
			t.Fatal(err)
		}
		return eng
	}

	middleDir, middleBackend := filepath.Join(t.TempDir(), "middle"), memory.New()
	middle := openOn(middleDir, middleBackend)
	defer middle.Close()
	if err := middle.CreateTable(overwriteTable, engine.TableOptions{}); err != nil {
		t.Fatal(err)
	}
	putLatest(t, middle, "cell", "own-older")
	applyStep(t, middle, stepFlush)

	producer := openOverwriteEngine(t, filepath.Join(t.TempDir(), "producer"), nil)
	defer producer.Close()
	putLatest(t, producer, "cell", "imported-newer")
	if err := middle.ImportRFileManifest(ctx, exportTo(producer, middleBackend, middleDir)); err != nil {
		t.Fatal(err)
	}
	if got := readLatestOn(t, middle, "cell"); got != "imported-newer" {
		t.Fatalf("middle latest = %q, want imported-newer", got)
	}

	for _, incremental := range []bool{false, true} {
		finalDir, finalBackend := filepath.Join(t.TempDir(), "final"), memory.New()
		final := openOn(finalDir, finalBackend)
		manifest := (*engine.RFileExportManifest)(nil)
		if incremental {
			result, err := middle.ExportRFilesIncremental(ctx, overwriteTable, finalBackend, engine.RFileExportOptions{
				DestinationRoot: finalDir,
				EngineVersion:   "test",
			}, nil)
			if err != nil {
				t.Fatal(err)
			}
			manifest = result.Manifest
		} else {
			manifest = exportTo(middle, finalBackend, finalDir)
		}
		if err := final.ImportRFileManifest(ctx, manifest); err != nil {
			t.Fatal(err)
		}
		if got := readLatestOn(t, final, "cell"); got != "imported-newer" {
			t.Fatalf("incremental=%v: re-exported latest = %q, want imported-newer", incremental, got)
		}
		_ = final.Close()
	}
}

// TestPutLatestOverwriteParquet repeats the overwrite sequences on a
// Parquet-format table, whose flush/compaction writer and reader must also
// keep identical keys in source order.
func TestPutLatestOverwriteParquet(t *testing.T) {
	for _, pattern := range overwritePatterns(3) {
		pattern := pattern
		t.Run(patternName(pattern), func(t *testing.T) {
			eng, err := engine.Open(t.TempDir(), engine.Options{})
			if err != nil {
				t.Fatal(err)
			}
			defer eng.Close()
			if err := eng.CreateTable(overwriteTable, engine.TableOptions{
				TabletOptions: tablet.Options{FileFormat: tablet.FormatParquet},
			}); err != nil {
				t.Fatal(err)
			}
			for i, step := range pattern {
				putLatest(t, eng, "cell", fmt.Sprintf("v%d", i+1))
				applyStep(t, eng, step)
				if got := readLatestOn(t, eng, "cell"); got != fmt.Sprintf("v%d", i+1) {
					t.Fatalf("after write %d (%s): latest = %q", i+1, step, got)
				}
			}
			got := readLatest(t, eng, "cell", len(pattern))
			for i := range got {
				if want := fmt.Sprintf("v%d", len(pattern)-i); got[i] != want {
					t.Fatalf("versions = %v, want newest first", got)
				}
			}
		})
	}
}
