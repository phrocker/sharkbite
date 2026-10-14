package rfile

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"

	"github.com/phrocker/shoal-oss/internal/rfile/bcfile"
	"github.com/phrocker/shoal-oss/internal/rfile/bcfile/block"
)

// roundtrip writes cells through Writer, reopens via Reader, drains, and
// returns the read-back cells. Used as the workhorse for the writer
// tests below.
func roundtrip(t *testing.T, opts WriterOptions, cells []cell) []cell {
	t.Helper()
	var buf bytes.Buffer
	w, err := NewWriter(&buf, opts)
	if err != nil {
		t.Fatalf("NewWriter: %v", err)
	}
	for i, c := range cells {
		if err := w.Append(c.K, c.V); err != nil {
			t.Fatalf("Append %d: %v", i, err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	bc, err := bcfile.NewReader(bytes.NewReader(buf.Bytes()), int64(buf.Len()))
	if err != nil {
		t.Fatalf("bcfile.NewReader: %v", err)
	}
	r, err := Open(bc, block.Default())
	if err != nil {
		t.Fatalf("rfile.Open: %v", err)
	}
	defer r.Close()
	return drainAll(t, r)
}

func cellsEqual(t *testing.T, got, want []cell) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("count mismatch: got %d, want %d", len(got), len(want))
	}
	for i := range got {
		if !got[i].K.Equal(want[i].K) {
			t.Errorf("cell %d: key mismatch\n  got:  %+v\n  want: %+v", i, got[i].K, want[i].K)
		}
		if !bytes.Equal(got[i].V, want[i].V) {
			t.Errorf("cell %d: value mismatch\n  got:  %q\n  want: %q", i, got[i].V, want[i].V)
		}
	}
}

func TestWriter_RoundtripSingleCell(t *testing.T) {
	cells := []cell{mkCell("a", "value")}
	got := roundtrip(t, WriterOptions{}, cells)
	cellsEqual(t, got, cells)
}

func TestWriter_RoundtripManyCellsSingleBlock(t *testing.T) {
	cells := []cell{}
	for i := 0; i < 100; i++ {
		cells = append(cells, mkCell(fmt.Sprintf("row%03d", i), fmt.Sprintf("v%d", i)))
	}
	// Big block size so all cells fit in one block.
	got := roundtrip(t, WriterOptions{BlockSize: 1 << 20}, cells)
	cellsEqual(t, got, cells)
}

func TestWriter_RoundtripForcesMultipleBlocks(t *testing.T) {
	// Tiny block size + many cells = several flushed blocks. Verifies
	// the IndexEntry layout writes correctly across multiple blocks and
	// the reader walks them in order.
	cells := []cell{}
	for i := 0; i < 50; i++ {
		// Each cell ~30 bytes; with 200-byte blocks we should get
		// roughly 7-8 blocks.
		cells = append(cells, mkCell(fmt.Sprintf("row%03d", i), strings.Repeat("x", 10)))
	}
	got := roundtrip(t, WriterOptions{BlockSize: 200}, cells)
	cellsEqual(t, got, cells)
}

func TestWriter_RoundtripWithGzipCodec(t *testing.T) {
	// Use the gz codec on the data blocks. Reader's Default decompressor
	// supports gz, so this should round-trip cleanly.
	cells := []cell{}
	for i := 0; i < 30; i++ {
		// Highly compressible payload
		cells = append(cells, mkCell(fmt.Sprintf("k%03d", i), strings.Repeat("ab", 100)))
	}
	got := roundtrip(t, WriterOptions{Codec: block.CodecGzip}, cells)
	cellsEqual(t, got, cells)
}

func TestWriter_RoundtripWithAdditionalHadoopCodecs(t *testing.T) {
	cells := make([]cell, 0, 30)
	for i := 0; i < 30; i++ {
		cells = append(cells, mkCell(fmt.Sprintf("k%03d", i), strings.Repeat("ab", 100)))
	}
	for _, codec := range []string{block.CodecZstd, block.CodecLZ4} {
		t.Run(codec, func(t *testing.T) {
			got := roundtrip(t, WriterOptions{Codec: codec}, cells)
			cellsEqual(t, got, cells)
		})
	}
}

func TestWriter_AppendNilKeyErrors(t *testing.T) {
	w, _ := NewWriter(&bytes.Buffer{}, WriterOptions{})
	if err := w.Append(nil, []byte("v")); err == nil {
		t.Errorf("nil key: expected error")
	}
}

func TestWriter_AppendAfterCloseErrors(t *testing.T) {
	var buf bytes.Buffer
	w, _ := NewWriter(&buf, WriterOptions{})
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	if err := w.Append(mkCell("a", "v").K, []byte("v")); err == nil {
		t.Errorf("Append after Close: expected error")
	}
}

func TestWriter_CloseIdempotent(t *testing.T) {
	var buf bytes.Buffer
	w, _ := NewWriter(&buf, WriterOptions{})
	if err := w.Append(mkCell("a", "v").K, []byte("v")); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Errorf("second Close: %v", err)
	}
}

func TestWriter_UnregisteredCodecErrors(t *testing.T) {
	_, err := NewWriter(&bytes.Buffer{}, WriterOptions{Codec: "brotli"})
	if err == nil {
		t.Errorf("brotli not registered: expected error")
	}
}

// TestWriter_RoundtripSeek: write → reopen → seek → confirm the right
// cell comes back. Exercises the writer's index correctness end-to-end.
func TestWriter_RoundtripSeek(t *testing.T) {
	cells := []cell{}
	for i := 0; i < 20; i++ {
		cells = append(cells, mkCell(fmt.Sprintf("row%02d", i), fmt.Sprintf("v%02d", i)))
	}
	var buf bytes.Buffer
	w, _ := NewWriter(&buf, WriterOptions{BlockSize: 100})
	for _, c := range cells {
		if err := w.Append(c.K, c.V); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}

	bc, _ := bcfile.NewReader(bytes.NewReader(buf.Bytes()), int64(buf.Len()))
	r, err := Open(bc, block.Default())
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()

	// Seek to row10 and confirm we get rows 10..19.
	if err := r.SeekRow([]byte("row10")); err != nil {
		t.Fatal(err)
	}
	got := drainAll(t, r)
	if len(got) != 10 {
		t.Fatalf("got %d cells from seek, want 10", len(got))
	}
	if string(got[0].K.Row) != "row10" {
		t.Errorf("first cell after seek = %q, want row10", got[0].K.Row)
	}
}

// TestWriter_LocalityGroupMetadata: verify the LG metadata our writer
// produces (firstKey, ColumnFamilies, NumTotalEntries) matches what the
// reader sees.
func TestWriter_LocalityGroupMetadata(t *testing.T) {
	cells := []cell{
		mkCell("a", "1"), // cf="cf"
		mkCell("b", "2"),
		mkCell("c", "3"),
	}
	var buf bytes.Buffer
	w, _ := NewWriter(&buf, WriterOptions{})
	for _, c := range cells {
		_ = w.Append(c.K, c.V)
	}
	_ = w.Close()

	bc, _ := bcfile.NewReader(bytes.NewReader(buf.Bytes()), int64(buf.Len()))
	r, _ := Open(bc, block.Default())
	defer r.Close()

	lg := r.LocalityGroup()
	if !lg.IsDefault {
		t.Errorf("LG should be default")
	}
	// NumTotalEntries is the count of LEAF entries (= data blocks), not cells.
	// Three cells fit in one block, so we expect 1.
	if lg.NumTotalEntries != 1 {
		t.Errorf("NumTotalEntries = %d, want 1 (one data block)", lg.NumTotalEntries)
	}
	if lg.ColumnFamilies["cf"] != 3 {
		t.Errorf("CF count for 'cf' = %d, want 3", lg.ColumnFamilies["cf"])
	}
	if lg.FirstKey == nil || string(lg.FirstKey.Row) != "a" {
		t.Errorf("FirstKey row = %v, want 'a'", lg.FirstKey)
	}
}

// TestWriter_LargeRoundtrip is a bigger stress test — 5000 cells across
// many blocks with gzip — to surface any latent bugs that small files
// don't expose.
func TestWriter_LargeRoundtrip(t *testing.T) {
	const N = 5000
	cells := []cell{}
	for i := 0; i < N; i++ {
		cells = append(cells, mkCell(
			fmt.Sprintf("row%06d", i),
			fmt.Sprintf("value-%d-with-some-padding-%s", i, strings.Repeat("x", 30)),
		))
	}
	got := roundtrip(t, WriterOptions{Codec: block.CodecGzip, BlockSize: 4096}, cells)
	cellsEqual(t, got, cells)
}

// TestWriter_RFileIsValidBCFile: cross-check that a fresh writer's
// output is a valid BCFile (footer parses, MetaIndex reachable). Catches
// regressions where the writer mangles the trailer.
func TestWriter_RFileIsValidBCFile(t *testing.T) {
	var buf bytes.Buffer
	w, _ := NewWriter(&buf, WriterOptions{})
	if err := w.Append(mkCell("a", "v").K, []byte("v")); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}

	bc, err := bcfile.NewReader(bytes.NewReader(buf.Bytes()), int64(buf.Len()))
	if err != nil {
		t.Fatal(err)
	}
	if bc.Footer().Version != bcfile.APIVersion3 {
		t.Errorf("Version = %v, want %v", bc.Footer().Version, bcfile.APIVersion3)
	}
	mustHave := []string{IndexMetaBlockName, bcfile.DataIndexBlockName}
	for _, name := range mustHave {
		if _, err := bc.MetaBlockEntry(name); err != nil {
			t.Errorf("missing meta block %q: %v", name, err)
		}
	}
}

// TestWriter_MultiLevelRoundtrip forces the multi-level index by
// driving BlockSize tiny (so we get many leaf entries) AND IndexBlockSize
// tiny (so the level-0 IndexBlock overflows quickly). Verifies that a
// multi-level structure round-trips through the reader: every cell
// preserved + monotonic key order + correct count.
func TestWriter_MultiLevelRoundtrip(t *testing.T) {
	const N = 500
	cells := make([]cell, 0, N)
	for i := 0; i < N; i++ {
		cells = append(cells, mkCell(
			fmt.Sprintf("row%05d", i),
			fmt.Sprintf("v%d-%s", i, strings.Repeat("p", 20)),
		))
	}
	got := roundtrip(t, WriterOptions{
		BlockSize:      120, // ~3-5 cells/block → ~120-160 leaf entries
		IndexBlockSize: 200, // forces multi-level cascade
	}, cells)
	cellsEqual(t, got, cells)
}

// TestWriter_MultiLevelTreeDepth confirms the writer actually produces
// a > 1 level tree under the stress params. We crack open the RFile
// directly to inspect the root IndexBlock's Level field.
func TestWriter_MultiLevelTreeDepth(t *testing.T) {
	const N = 1000
	var buf bytes.Buffer
	w, err := NewWriter(&buf, WriterOptions{
		BlockSize:      80, // tiny → 1-2 cells per block
		IndexBlockSize: 150,
	})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < N; i++ {
		c := mkCell(fmt.Sprintf("row%06d", i), fmt.Sprintf("v%d", i))
		if err := w.Append(c.K, c.V); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}

	bc, _ := bcfile.NewReader(bytes.NewReader(buf.Bytes()), int64(buf.Len()))
	r, err := Open(bc, block.Default())
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()

	root := r.LocalityGroup().RootIndex
	if root.Level == 0 {
		t.Fatalf("root.Level = 0; expected multi-level (Level > 0) under stress params")
	}
	t.Logf("root.Level = %d, root.NumEntries = %d, totalLeaves (NumTotalEntries) = %d",
		root.Level, root.NumEntries(), r.LocalityGroup().NumTotalEntries)
}

// TestWriter_MultiLevelSeek confirms seek correctness across the
// multi-level index: the walker must descend levels correctly and land
// in the right leaf.
func TestWriter_MultiLevelSeek(t *testing.T) {
	const N = 600
	cells := make([]cell, 0, N)
	for i := 0; i < N; i++ {
		cells = append(cells, mkCell(fmt.Sprintf("row%05d", i), fmt.Sprintf("v%d", i)))
	}

	var buf bytes.Buffer
	w, _ := NewWriter(&buf, WriterOptions{BlockSize: 100, IndexBlockSize: 250})
	for _, c := range cells {
		if err := w.Append(c.K, c.V); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}

	bc, _ := bcfile.NewReader(bytes.NewReader(buf.Bytes()), int64(buf.Len()))
	r, err := Open(bc, block.Default())
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()

	if r.LocalityGroup().RootIndex.Level == 0 {
		t.Fatalf("expected multi-level index for this test; root.Level = 0")
	}

	probe := []string{"row00000", "row00150", "row00300", "row00450", "row00599"}
	for _, target := range probe {
		if err := r.SeekRow([]byte(target)); err != nil {
			t.Fatalf("seek %q: %v", target, err)
		}
		k, _, err := r.Next()
		if err != nil {
			t.Fatalf("next after seek %q: %v", target, err)
		}
		if string(k.Row) != target {
			t.Errorf("seek %q → got row %q", target, string(k.Row))
		}
	}
}

// TestWriter_MultiLevelTotalAdded asserts that NumTotalEntries equals the
// number of LEAF data blocks (Java's totalAdded), not the number of
// cells. Critical for correct cross-tool reporting (Java's PrintInfo
// reads this as "Num blocks").
func TestWriter_MultiLevelTotalAdded(t *testing.T) {
	const N = 50
	cells := make([]cell, 0, N)
	for i := 0; i < N; i++ {
		cells = append(cells, mkCell(fmt.Sprintf("k%03d", i), "v"))
	}
	var buf bytes.Buffer
	w, _ := NewWriter(&buf, WriterOptions{BlockSize: 50}) // very small → many blocks
	for _, c := range cells {
		_ = w.Append(c.K, c.V)
	}
	_ = w.Close()

	bc, _ := bcfile.NewReader(bytes.NewReader(buf.Bytes()), int64(buf.Len()))
	r, _ := Open(bc, block.Default())
	defer r.Close()

	got := r.LocalityGroup().NumTotalEntries
	if got >= int32(N) {
		t.Errorf("NumTotalEntries=%d > N=%d cells; should be the BLOCK count, much smaller", got, N)
	}
	if got <= 0 {
		t.Errorf("NumTotalEntries=%d; expected > 0", got)
	}
	t.Logf("NumTotalEntries = %d (cells = %d)", got, N)
}

// TestWriter_EmptyFileRoundtripsToEmptyReader: a writer that gets no
// Append calls should still produce a valid (parseable) RFile that
// iterates zero cells.
func TestWriter_EmptyFileRoundtripsToEmptyReader(t *testing.T) {
	var buf bytes.Buffer
	w, _ := NewWriter(&buf, WriterOptions{})
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}

	bc, err := bcfile.NewReader(bytes.NewReader(buf.Bytes()), int64(buf.Len()))
	if err != nil {
		t.Fatal(err)
	}
	r, err := Open(bc, block.Default())
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()

	_, _, err = r.Next()
	if !errors.Is(err, io.EOF) {
		t.Errorf("err = %v, want io.EOF", err)
	}
}
