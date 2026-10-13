package rfile

import (
	"bytes"
	"errors"
	"io"
	"math"
	"math/rand"
	"testing"

	"github.com/phrocker/shoal-oss/internal/rfile/bcfile"
)

// --- property test ---

// checkVersionRunProperty writes the generated cells, then asserts the
// round trip, the leaf-key invariant, and seek correctness at every
// distinct key, just past it, at its row prefix, and past the end. With
// runCap 0 the default cap is far above any generated run, so leaf keys
// must be strictly increasing; a positive runCap lowers the cap so long
// runs are cut and equal adjacent keys appear.
func checkVersionRunProperty(t testing.TB, seed int64, blockSize, indexBlockSize, runCap int) {
	t.Helper()
	rng := rand.New(rand.NewSource(seed))
	cells := genVersionRunCells(rng)
	opts := WriterOptions{BlockSize: blockSize, IndexBlockSize: indexBlockSize}
	r := openBytes(t, writeCellsWithRunCap(t, opts, runCap, cells))
	defer r.Close()
	if runCap > 0 {
		assertCappedLeaves(t, r, runCap)
	} else {
		assertStrictLeaves(t, leavesOf(t, r))
	}
	if got := readUpTo(t, r, len(cells)+1); !sameCells(got, cells) {
		t.Fatalf("seed %d: round trip returned %d cells, want %d", seed, len(got), len(cells))
	}
	for i, c := range cells {
		if i > 0 && c.K.Equal(cells[i-1].K) {
			continue
		}
		expectSeek(t, r, cells, c.K, 4)
		expectSeek(t, r, cells, after(c.K), 4)
		expectSeek(t, r, cells, &Key{Row: c.K.Row}, 4)
	}
	expectSeek(t, r, cells, &Key{Row: []byte("zz")}, 0)
}

func TestWriter_VersionRunProperty(t *testing.T) {
	blockSizes := []int{1, 16, 64, 200, 1024, DefaultBlockSize}
	indexBlockSizes := []int{64, 150, 400, DefaultIndexBlockSize}
	runCaps := []int{0, 0, 256, 1500, 0}
	for seed := int64(0); seed < 60; seed++ {
		checkVersionRunProperty(t, seed,
			blockSizes[seed%int64(len(blockSizes))],
			indexBlockSizes[seed%int64(len(indexBlockSizes))],
			runCaps[seed%int64(len(runCaps))])
	}
}

func FuzzWriterVersionRuns(f *testing.F) {
	f.Add(int64(1), uint16(1), uint16(64), uint16(0))
	f.Add(int64(2), uint16(64), uint16(150), uint16(300))
	f.Add(int64(3), uint16(512), uint16(4096), uint16(0))
	f.Fuzz(func(t *testing.T, seed int64, blockSize, indexBlockSize, runCap uint16) {
		checkVersionRunProperty(t, seed, int(blockSize)%4096+1, int(indexBlockSize)%4096+1, int(runCap))
	})
}

// --- oversize runs ---

// TestWriter_OversizeRunIsCutAtCap: a run of identical keys longer than the
// run cap is cut once a block reaches the cap, so the block stays bounded;
// the equal adjacent index keys that result read back and seek correctly.
func TestWriter_OversizeRunIsCutAtCap(t *testing.T) {
	const runCap = 16 * 1024
	cells := versionRunCells(200, 600) // a run of ~120KB, several caps long
	r := openBytes(t, writeCellsWithRunCap(t, WriterOptions{}, runCap, cells))
	defer r.Close()
	assertCappedLeaves(t, r, runCap)
	leaves, err := r.collectLeaves()
	if err != nil {
		t.Fatal(err)
	}
	runBlocks := 0
	keys := make([]*Key, len(leaves))
	for i, leaf := range leaves {
		// A block is cut before the cell that would follow a full block,
		// so it exceeds the cap by at most one cell.
		if leaf.RawSize >= runCap+1024 {
			t.Fatalf("block of %d bytes exceeds the %d-byte cap by more than one cell", leaf.RawSize, runCap)
		}
		if string(leaf.Key.Row) == "m" {
			runBlocks++
		}
		keys[i] = leaf.Key
	}
	if runBlocks < 5 {
		t.Fatalf("run used %d blocks, want it cut at the cap into several", runBlocks)
	}
	if !hasEqualAdjacentLeaves(keys) {
		t.Fatal("an oversize run produced no equal adjacent index keys")
	}
	cellsEqual(t, drainAll(t, r), cells)

	run := cells[5].K
	expectSeek(t, r, cells, run, 0)
	expectSeek(t, r, cells, &Key{Row: []byte("m")}, 0)
	expectSeek(t, r, cells, after(run), 0)
	expectSeek(t, r, cells, &Key{Row: []byte("b")}, 0)
	expectSeek(t, r, cells, &Key{Row: []byte("zz")}, 0)
	for _, leaf := range keys {
		expectSeek(t, r, cells, leaf, 0)
		expectSeek(t, r, cells, after(leaf), 0)
	}
}

// TestWriter_RunUnderCapStaysStrict: the same run with a cap above its
// size keeps one block and strictly increasing index keys.
func TestWriter_RunUnderCapStaysStrict(t *testing.T) {
	cells := versionRunCells(200, 600)
	r := openBytes(t, writeCellsWithRunCap(t, WriterOptions{}, 1<<20, cells))
	defer r.Close()
	leaves := leavesOf(t, r)
	assertStrictLeaves(t, leaves)
	runBlocks := 0
	for _, leaf := range leaves {
		if string(leaf.Row) == "m" {
			runBlocks++
		}
	}
	if runBlocks != 1 {
		t.Fatalf("run under the cap used %d blocks, want 1", runBlocks)
	}
	cellsEqual(t, drainAll(t, r), cells)
}

func TestWriter_RunCapDefaults(t *testing.T) {
	for _, tc := range []struct {
		blockSize, want int
	}{
		{0, MaxRunBlockSize},
		{DefaultBlockSize, MaxRunBlockSize},
		{MaxRunBlockSize + 1, MaxRunBlockSize + 1},
	} {
		w, err := NewWriter(io.Discard, WriterOptions{BlockSize: tc.blockSize})
		if err != nil {
			t.Fatal(err)
		}
		if w.runBlockCap != tc.want {
			t.Fatalf("BlockSize %d: run cap = %d, want %d", tc.blockSize, w.runBlockCap, tc.want)
		}
	}
	// The cap must leave ample room under the reader's block limit.
	if int64(MaxRunBlockSize)*4 > bcfile.MaxRawBlockSize {
		t.Fatalf("MaxRunBlockSize %d is not well under bcfile.MaxRawBlockSize %d",
			MaxRunBlockSize, bcfile.MaxRawBlockSize)
	}
}

// TestWriter_HugeRunAtDefaultCap is the review repro at real sizes: a run of
// identical 1MiB cells past MaxRunBlockSize used to grow one block toward
// bcfile.MaxRawBlockSize. With the default cap it is cut and reads back.
func TestWriter_HugeRunAtDefaultCap(t *testing.T) {
	if testing.Short() {
		t.Skip("writes ~70MiB")
	}
	value := bytes.Repeat([]byte{'v'}, 1<<20)
	n := MaxRunBlockSize/len(value) + 6
	key := &Key{
		Row: []byte("r"), ColumnFamily: []byte("record"), ColumnQualifier: []byte("v1"),
		Timestamp: math.MaxInt64,
	}
	var buf bytes.Buffer
	w, err := NewWriter(&buf, WriterOptions{})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < n; i++ {
		if err := w.Append(key, value); err != nil {
			t.Fatalf("Append %d: %v", i, err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	r := openBytes(t, buf.Bytes())
	defer r.Close()
	assertCappedLeaves(t, r, MaxRunBlockSize)
	leaves, err := r.collectLeaves()
	if err != nil {
		t.Fatal(err)
	}
	if len(leaves) != 2 {
		t.Fatalf("run of %d MiB used %d blocks, want 2", n, len(leaves))
	}
	count := 0
	for {
		_, _, err := r.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		count++
	}
	if count != n {
		t.Fatalf("read %d cells, want %d", count, n)
	}
}
