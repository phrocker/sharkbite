package rfile

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"math"
	"math/rand"
	"os"
	"sort"
	"testing"

	"github.com/phrocker/shoal-oss/internal/rfile/bcfile"
	"github.com/phrocker/shoal-oss/internal/rfile/bcfile/block"
)

// Regression tests for #690: a run of identical keys (every PutLatest write
// to one cell carries timestamp math.MaxInt64, and the memtable keeps them
// all) used to be cut across data blocks, so consecutive index entries
// carried the same key and the reader refused the file.

func writeCells(t testing.TB, opts WriterOptions, cells []cell) []byte {
	t.Helper()
	return writeCellsWithRunCap(t, opts, 0, cells)
}

// writeCellsWithRunCap is writeCells with the writer's run-block cap
// (MaxRunBlockSize by default) lowered to runCap when runCap > 0.
func writeCellsWithRunCap(t testing.TB, opts WriterOptions, runCap int, cells []cell) []byte {
	t.Helper()
	var buf bytes.Buffer
	w, err := NewWriter(&buf, opts)
	if err != nil {
		t.Fatalf("NewWriter: %v", err)
	}
	if runCap > 0 {
		w.runBlockCap = runCap
	}
	for i, c := range cells {
		if err := w.Append(c.K, c.V); err != nil {
			t.Fatalf("Append %d: %v", i, err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	return buf.Bytes()
}

func openBytes(t testing.TB, data []byte) *Reader {
	t.Helper()
	bc, err := bcfile.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		t.Fatalf("bcfile.NewReader: %v", err)
	}
	r, err := Open(bc, block.Default())
	if err != nil {
		t.Fatalf("rfile.Open: %v", err)
	}
	return r
}

func leavesOf(t testing.TB, r *Reader) []*Key {
	t.Helper()
	leaves, err := r.collectLeaves()
	if err != nil {
		t.Fatalf("collect leaves: %v", err)
	}
	keys := make([]*Key, len(leaves))
	for i, leaf := range leaves {
		keys[i] = leaf.Key
	}
	return keys
}

// assertStrictLeaves is the invariant the writer now guarantees.
func assertStrictLeaves(t testing.TB, leaves []*Key) {
	t.Helper()
	for i := 1; i < len(leaves); i++ {
		if leaves[i].Compare(leaves[i-1]) <= 0 {
			t.Fatalf("leaf %d key %v is not strictly after leaf %d key %v",
				i, leaves[i], i-1, leaves[i-1])
		}
	}
}

// assertCappedLeaves is the invariant with a run cap in play: leaf keys
// never decrease, and two adjacent leaves share a key only when the
// earlier block reached the cap (an oversize run was cut).
func assertCappedLeaves(t testing.TB, r *Reader, runCap int) {
	t.Helper()
	leaves, err := r.collectLeaves()
	if err != nil {
		t.Fatalf("collect leaves: %v", err)
	}
	for i := 1; i < len(leaves); i++ {
		c := leaves[i].Key.Compare(leaves[i-1].Key)
		if c < 0 {
			t.Fatalf("leaf %d key %v sorts before leaf %d key %v",
				i, leaves[i].Key, i-1, leaves[i-1].Key)
		}
		if c == 0 && leaves[i-1].RawSize < int64(runCap) {
			t.Fatalf("leaves %d and %d share key %v although block %d is %d bytes, under the %d-byte cap",
				i-1, i, leaves[i].Key, i-1, leaves[i-1].RawSize, runCap)
		}
	}
}

func hasEqualAdjacentLeaves(leaves []*Key) bool {
	for i := 1; i < len(leaves); i++ {
		if leaves[i].Equal(leaves[i-1]) {
			return true
		}
	}
	return false
}

// readUpTo returns at most n cells from the reader's current position.
func readUpTo(t testing.TB, r *Reader, n int) []cell {
	t.Helper()
	var out []cell
	for len(out) < n {
		k, v, err := r.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("Next: %v", err)
		}
		out = append(out, cell{K: k, V: v})
	}
	return out
}

func sameCells(a, b []cell) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if !a[i].K.Equal(b[i].K) || !bytes.Equal(a[i].V, b[i].V) {
			return false
		}
	}
	return true
}

// expectSeek seeks to target and checks the next window cells (or all
// remaining cells when window <= 0) against the first cells of want at or
// after target.
func expectSeek(t testing.TB, r *Reader, want []cell, target *Key, window int) {
	t.Helper()
	start := sort.Search(len(want), func(i int) bool { return want[i].K.Compare(target) >= 0 })
	expected := want[start:]
	if window > 0 && len(expected) > window {
		expected = expected[:window]
	}
	if err := r.Seek(target); err != nil {
		t.Fatalf("Seek(%v): %v", target, err)
	}
	limit := len(expected)
	if window <= 0 {
		limit = len(want) + 1
	}
	got := readUpTo(t, r, limit)
	if !sameCells(got, expected) {
		t.Fatalf("Seek(%v): got %d cells, want %d starting at input cell %d",
			target, len(got), len(expected), start)
	}
}

// after returns a key that sorts immediately after k: same coordinate, the
// next older timestamp. Used to probe "between blocks" seek targets.
func after(k *Key) *Key {
	out := k.Clone()
	if out.Timestamp > math.MinInt64 {
		out.Timestamp--
	} else {
		out.ColumnVisibility = append(out.ColumnVisibility, 0)
	}
	out.Deleted = false
	return out
}

func latestCell(row string, i int, size int) cell {
	return cell{
		K: &Key{
			Row: []byte(row), ColumnFamily: []byte("record"), ColumnQualifier: []byte("v1"),
			Timestamp: math.MaxInt64,
		},
		V: append([]byte(fmt.Sprintf("%s-%06d:", row, i)), bytes.Repeat([]byte{'x'}, size)...),
	}
}

// versionRunCells is "a" cells, a run of n identical "m" keys, then "z"
// cells: the shape of a catalog row overwritten n times between flushes.
func versionRunCells(n, valueSize int) []cell {
	var cells []cell
	for i := 0; i < 5; i++ {
		c := latestCell("a", i, valueSize)
		c.K.Timestamp = int64(100 - i)
		cells = append(cells, c)
	}
	for i := 0; i < n; i++ {
		cells = append(cells, latestCell("m", n-i, valueSize))
	}
	for i := 0; i < 5; i++ {
		c := latestCell("z", i, valueSize)
		c.K.Timestamp = int64(100 - i)
		cells = append(cells, c)
	}
	return cells
}

func TestWriter_IdenticalKeyRunStaysInOneBlock(t *testing.T) {
	// 600-byte values: the old writer failed from 171 identical keys on
	// with the default 100KB block size.
	for _, n := range []int{1, 170, 171, 172, 200, 500} {
		t.Run(fmt.Sprint(n), func(t *testing.T) {
			var cells []cell
			for i := 0; i < n; i++ {
				cells = append(cells, latestCell("r", n-i, 600))
			}
			r := openBytes(t, writeCells(t, WriterOptions{}, cells))
			defer r.Close()
			leaves := leavesOf(t, r)
			assertStrictLeaves(t, leaves)
			if len(leaves) != 1 {
				t.Fatalf("a run of one key used %d blocks, want 1", len(leaves))
			}
			cellsEqual(t, drainAll(t, r), cells)
		})
	}
}

func TestWriter_IdenticalKeyRunBetweenOtherKeys(t *testing.T) {
	cells := versionRunCells(400, 600)
	r := openBytes(t, writeCells(t, WriterOptions{}, cells))
	defer r.Close()
	leaves := leavesOf(t, r)
	assertStrictLeaves(t, leaves)
	cellsEqual(t, drainAll(t, r), cells)

	// The run starts and ends a block boundary of its own: the block
	// before it is cut just before the first "m" (it was full), and the
	// run's block is cut just before the first "z".
	run := cells[5].K
	expectSeek(t, r, cells, run, 0)                     // first version
	expectSeek(t, r, cells, &Key{Row: []byte("m")}, 0)  // row prefix
	expectSeek(t, r, cells, after(run), 0)              // just past the run
	expectSeek(t, r, cells, &Key{Row: []byte("b")}, 0)  // between "a" and the run
	expectSeek(t, r, cells, &Key{Row: []byte("zz")}, 0) // past end
	expectSeek(t, r, cells, cells[len(cells)-1].K, 0)   // last cell
	expectSeek(t, r, cells, after(cells[len(cells)-1].K), 0)
	for _, leaf := range leaves {
		expectSeek(t, r, cells, leaf, 0)
		expectSeek(t, r, cells, after(leaf), 0)
	}
}

// TestWriter_DistinctVersionsSeek covers many versions of one row/column
// with distinct timestamps: blocks may split inside the coordinate, since
// every key differs, and a seek to any version must land on it.
func TestWriter_DistinctVersionsSeek(t *testing.T) {
	const n = 300
	var cells []cell
	for i := 0; i < n; i++ {
		c := latestCell("r", n-i, 600)
		c.K.Timestamp = int64(n - i)
		cells = append(cells, c)
	}
	r := openBytes(t, writeCells(t, WriterOptions{}, cells))
	defer r.Close()
	leaves := leavesOf(t, r)
	assertStrictLeaves(t, leaves)
	if len(leaves) < 2 {
		t.Fatalf("distinct versions used %d blocks, want several", len(leaves))
	}
	cellsEqual(t, drainAll(t, r), cells)
	for _, i := range []int{0, n / 2, n - 1} {
		expectSeek(t, r, cells, cells[i].K, 0)
	}
	for _, leaf := range leaves {
		expectSeek(t, r, cells, leaf, 0)
		expectSeek(t, r, cells, after(leaf), 0)
	}
}

// TestWriter_IdenticalKeyRunMultiLevelIndex forces a multi-level index so
// the parent-level keys (each child's last leaf key) are exercised too.
func TestWriter_IdenticalKeyRunMultiLevelIndex(t *testing.T) {
	var cells []cell
	for i := 0; i < 400; i++ {
		row := fmt.Sprintf("row%03d", i/4)
		c := latestCell(row, i, 20)
		if i%40 >= 4 {
			// Rows 1..9 of every ten keep the same key four times over;
			// row 0 of every ten has distinct timestamps.
			c.K.Timestamp = math.MaxInt64
		} else {
			c.K.Timestamp = int64(1000 - i)
		}
		cells = append(cells, c)
	}
	opts := WriterOptions{BlockSize: 40, IndexBlockSize: 200}
	r := openBytes(t, writeCells(t, opts, cells))
	defer r.Close()
	if r.lg.RootIndex.Level == 0 {
		t.Fatal("expected a multi-level index")
	}
	leaves := leavesOf(t, r)
	assertStrictLeaves(t, leaves)
	cellsEqual(t, drainAll(t, r), cells)
	for _, leaf := range leaves {
		expectSeek(t, r, cells, leaf, 8)
		expectSeek(t, r, cells, after(leaf), 8)
	}
}

func TestWriter_AppendRejectsDecreasingKey(t *testing.T) {
	var buf bytes.Buffer
	w, err := NewWriter(&buf, WriterOptions{BlockSize: 1})
	if err != nil {
		t.Fatal(err)
	}
	if err := w.Append(mkCell("b", "1").K, nil); err != nil {
		t.Fatal(err)
	}
	// Equal keys are allowed.
	if err := w.Append(mkCell("b", "2").K, nil); err != nil {
		t.Fatalf("equal key: %v", err)
	}
	// BlockSize 1 makes the block full, so the check must hold across a
	// would-be block boundary too.
	err = w.Append(mkCell("a", "3").K, nil)
	if !errors.Is(err, ErrKeyOutOfOrder) {
		t.Fatalf("decreasing key error = %v, want ErrKeyOutOfOrder", err)
	}
	if err := w.Append(mkCell("c", "4").K, nil); err != nil {
		t.Fatalf("key after rejected one: %v", err)
	}
	if err := w.Append(mkCell("b", "5").K, nil); !errors.Is(err, ErrKeyOutOfOrder) {
		t.Fatalf("key before previous block's last key error = %v, want ErrKeyOutOfOrder", err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	r := openBytes(t, buf.Bytes())
	defer r.Close()
	got := drainAll(t, r)
	if len(got) != 3 {
		t.Fatalf("file holds %d cells, want the 3 accepted ones", len(got))
	}
}

func TestWriter_LocalityGroupsRestartKeyOrder(t *testing.T) {
	var buf bytes.Buffer
	w, err := NewWriter(&buf, WriterOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if err := w.Append(mkCell("z", "1").K, nil); err != nil {
		t.Fatal(err)
	}
	if err := w.AddLocalityGroup("lg"); err != nil {
		t.Fatal(err)
	}
	k := mkCell("a", "2").K
	k.ColumnFamily = []byte("other")
	if err := w.Append(k, nil); err != nil {
		t.Fatalf("a new locality group may restart at any key: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
}

// --- files written before the fix ---

const (
	legacySingleLevelFixture = "testdata/legacy_equal_index_keys.rf"
	legacyMultiLevelFixture  = "testdata/legacy_equal_index_keys_multilevel.rf"
)

// legacyFixtureCells is the input the checked-in legacy fixtures were
// written from, with the writer as it was before #690 was fixed (commit
// 777add94, internal/rfile/writer.go) and the options in
// legacyFixtureOptions. The current writer cannot reproduce them: it never
// cuts a block inside a run of equal keys.
func legacyFixtureCells() []cell {
	var cells []cell
	for i := 0; i < 3; i++ {
		c := latestCell("a", i, 10)
		c.K.Timestamp = int64(10 - i)
		cells = append(cells, c)
	}
	for i := 0; i < 40; i++ {
		cells = append(cells, latestCell("m", 40-i, 10))
	}
	for i := 0; i < 3; i++ {
		c := latestCell("z", i, 10)
		c.K.Timestamp = int64(10 - i)
		cells = append(cells, c)
	}
	return cells
}

var legacyFixtureOptions = map[string]WriterOptions{
	legacySingleLevelFixture: {BlockSize: 128},
	legacyMultiLevelFixture:  {BlockSize: 64, IndexBlockSize: 120},
}

func readFixture(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return data
}

// TestReader_AcceptsLegacyEqualIndexKeys reads files produced by the old
// writer, whose indexes repeat a key across consecutive blocks. The reader
// accepts equal adjacent index keys (never decreasing ones), and every seek
// must still land correctly.
func TestReader_AcceptsLegacyEqualIndexKeys(t *testing.T) {
	cells := legacyFixtureCells()
	for path := range legacyFixtureOptions {
		t.Run(path, func(t *testing.T) {
			r := openBytes(t, readFixture(t, path))
			defer r.Close()
			leaves := leavesOf(t, r)
			if !hasEqualAdjacentLeaves(leaves) {
				t.Fatal("fixture has no equal adjacent index keys; it no longer covers #690")
			}
			if path == legacyMultiLevelFixture && r.lg.RootIndex.Level == 0 {
				t.Fatal("multi-level fixture has a single-level index")
			}
			cellsEqual(t, drainAll(t, r), cells)

			run := cells[3].K
			expectSeek(t, r, cells, run, 0)
			expectSeek(t, r, cells, &Key{Row: []byte("m")}, 0)
			expectSeek(t, r, cells, after(run), 0)
			expectSeek(t, r, cells, &Key{Row: []byte("b")}, 0)
			expectSeek(t, r, cells, &Key{Row: []byte("zz")}, 0)
			for _, leaf := range leaves {
				expectSeek(t, r, cells, leaf, 0)
				expectSeek(t, r, cells, after(leaf), 0)
			}

			// The current writer rewrites the same cells with strictly
			// increasing index keys: compaction repairs legacy files.
			rewritten := openBytes(t, writeCells(t, legacyFixtureOptions[path], cells))
			defer rewritten.Close()
			assertStrictLeaves(t, leavesOf(t, rewritten))
			cellsEqual(t, drainAll(t, rewritten), cells)
		})
	}
}

// --- property test ---

// genVersionRunCells builds a sorted cell stream over a small key space
// where some keys repeat heavily (runs of identical keys) and others carry
// many distinct-timestamp versions. Values are unique per cell so order
// within a run is checked too.
func genVersionRunCells(rng *rand.Rand) []cell {
	var keys []*Key
	rows := 1 + rng.Intn(12)
	for row := 0; row < rows; row++ {
		for _, cq := range []string{"p", "q"} {
			if rng.Intn(3) == 0 {
				continue
			}
			versions := 1 + rng.Intn(6)
			if rng.Intn(4) == 0 {
				versions += rng.Intn(60)
			}
			for v := 0; v < versions; v++ {
				ts := int64(math.MaxInt64)
				if rng.Intn(2) == 0 {
					ts = int64(rng.Intn(50))
				}
				keys = append(keys, &Key{
					Row:             []byte(fmt.Sprintf("row%02d", row)),
					ColumnFamily:    []byte("cf"),
					ColumnQualifier: []byte(cq),
					Timestamp:       ts,
					Deleted:         rng.Intn(10) == 0,
				})
			}
		}
	}
	sort.SliceStable(keys, func(i, j int) bool { return keys[i].Compare(keys[j]) < 0 })
	var cells []cell
	for _, k := range keys {
		repeat := 1
		if rng.Intn(3) == 0 {
			repeat += rng.Intn(120)
		}
		for i := 0; i < repeat; i++ {
			v := append([]byte(fmt.Sprintf("%d:", len(cells))), bytes.Repeat([]byte{'v'}, rng.Intn(48))...)
			cells = append(cells, cell{K: k.Clone(), V: v})
		}
	}
	return cells
}
