package engine_test

import (
	"bytes"
	"fmt"
	"testing"

	"github.com/phrocker/shoal-oss/internal/cclient"
	"github.com/phrocker/shoal-oss/internal/engine"
	"github.com/phrocker/shoal-oss/internal/iterrt"
)

// writeVersions overwrites one cell with versions from..to (inclusive)
// using PutLatest. Every write carries the same full key (PutLatest stamps
// math.MaxInt64), so the memtable holds identical keys, newest first, and
// a flush hands the rfile writer one run of identical keys. The versions
// go in one batch to keep the test fast; the memtable orders duplicates
// within a batch exactly as it orders separate writes.
func writeVersions(t *testing.T, eng *engine.Engine, table string, from, to int) {
	t.Helper()
	var batch []*cclient.Mutation
	for i := from; i <= to; i++ {
		m, err := cclient.NewMutation([]byte("meta/record"))
		if err != nil {
			t.Fatal(err)
		}
		m.PutLatest([]byte("record"), []byte("v1"), nil, versionValue(i))
		batch = append(batch, m)
	}
	if err := eng.Write(table, batch); err != nil {
		t.Fatalf("write versions %d..%d: %v", from, to, err)
	}
}

// versionValue is large enough that a hundred versions fill a
// default-size (100KB) data block.
func versionValue(i int) []byte {
	return append([]byte(fmt.Sprintf("version-%06d:", i)), bytes.Repeat([]byte{'x'}, 1024)...)
}

// scanAll returns every raw cell value of table, in scan order.
func scanAll(t *testing.T, eng *engine.Engine, table string) [][]byte {
	t.Helper()
	sc, err := eng.Scan(table, iterrt.InfiniteRange(), engine.ScanOptions{})
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	defer sc.Close()
	var out [][]byte
	for sc.Next() {
		out = append(out, append([]byte(nil), sc.Value()...))
		if err := sc.Advance(); err != nil {
			t.Fatalf("advance: %v", err)
		}
	}
	return out
}

// assertNewestFirst checks that the scan holds every version, newest first.
func assertNewestFirst(t *testing.T, got [][]byte, n int) {
	t.Helper()
	if len(got) != n {
		t.Fatalf("scan returned %d cells, want %d", len(got), n)
	}
	for i, v := range got {
		if want := versionValue(n - i); !bytes.Equal(v, want) {
			t.Fatalf("cell %d = %.20q, want %.20q", i, v, want)
		}
	}
}

func reopenAndCheck(t *testing.T, dir, table string, n int) {
	t.Helper()
	reopened, err := engine.Open(dir, engine.Options{})
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer reopened.Close()
	assertNewestFirst(t, scanAll(t, reopened, table), n)
}

// TestEngine_ManyVersionsOfOneCellSurviveFlushAndReopen is the engine-level
// regression for #690: enough identical keys to span more than one data
// block used to produce an rfile whose index repeated a key, and the table
// could not be read after reopening.
func TestEngine_ManyVersionsOfOneCellSurviveFlushAndReopen(t *testing.T) {
	for _, n := range []int{1, 99, 100, 101, 250, 600} {
		t.Run(fmt.Sprint(n), func(t *testing.T) {
			dir := t.TempDir()
			eng, err := engine.Open(dir, engine.Options{})
			if err != nil {
				t.Fatal(err)
			}
			if err := eng.CreateTable("catalog", engine.TableOptions{}); err != nil {
				t.Fatal(err)
			}
			writeVersions(t, eng, "catalog", 1, n)
			if err := eng.Flush("catalog"); err != nil {
				t.Fatalf("flush: %v", err)
			}
			if err := eng.Close(); err != nil {
				t.Fatalf("close: %v", err)
			}
			reopenAndCheck(t, dir, "catalog", n)
		})
	}
}

// TestEngine_ManyVersionsOfOneCellSurviveCompaction covers the compaction
// writer. Each flushed file holds 60 versions (about 64KB, one data block),
// so the inputs were readable even under the old writer; only the
// compaction output's 120-version run spans more than one block.
func TestEngine_ManyVersionsOfOneCellSurviveCompaction(t *testing.T) {
	dir := t.TempDir()
	eng, err := engine.Open(dir, engine.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if err := eng.CreateTable("catalog", engine.TableOptions{}); err != nil {
		t.Fatal(err)
	}
	writeVersions(t, eng, "catalog", 1, 60)
	if err := eng.Flush("catalog"); err != nil {
		t.Fatalf("first flush: %v", err)
	}
	writeVersions(t, eng, "catalog", 61, 120)
	if err := eng.Flush("catalog"); err != nil {
		t.Fatalf("second flush: %v", err)
	}
	if err := eng.Compact("catalog", nil); err != nil {
		t.Fatalf("compact: %v", err)
	}
	if err := eng.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	reopened, err := engine.Open(dir, engine.Options{})
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer reopened.Close()
	// The merge does not order identical keys from different input files,
	// so this checks that every version survives exactly once rather than
	// their order.
	got := scanAll(t, reopened, "catalog")
	if len(got) != 120 {
		t.Fatalf("scan returned %d cells after compaction, want 120", len(got))
	}
	seen := map[string]bool{}
	for _, v := range got {
		seen[string(v)] = true
	}
	for i := 1; i <= 120; i++ {
		if !seen[string(versionValue(i))] {
			t.Fatalf("version %d missing after compaction", i)
		}
	}
}
