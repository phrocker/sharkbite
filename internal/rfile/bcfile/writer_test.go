package bcfile

import (
	"bytes"
	"strings"
	"testing"
)

// TestWriter_RoundtripEmpty: a BCFile with zero data blocks + zero
// extra meta blocks. Smallest legal output. The DataIndex meta block
// is always present.
func TestWriter_RoundtripEmpty(t *testing.T) {
	var buf bytes.Buffer
	w := NewWriter(&buf, "none")
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}

	r, err := NewReader(bytes.NewReader(buf.Bytes()), int64(buf.Len()))
	if err != nil {
		t.Fatalf("NewReader: %v", err)
	}
	if r.Footer().Version != APIVersion3 {
		t.Errorf("Version = %v, want %v", r.Footer().Version, APIVersion3)
	}
	// The DataIndex meta block must be present in the MetaIndex even
	// for an empty file.
	if _, err := r.MetaBlockEntry(DataIndexBlockName); err != nil {
		t.Errorf("expected %q meta entry, got %v", DataIndexBlockName, err)
	}
}

// TestWriter_RoundtripDataBlocks: write N data blocks (uncompressed),
// reopen, confirm DataIndex lists them at the right offsets/sizes.
func TestWriter_RoundtripDataBlocks(t *testing.T) {
	var buf bytes.Buffer
	w := NewWriter(&buf, "none")

	blocks := [][]byte{
		[]byte("data block zero"),
		[]byte("data block one — slightly longer"),
		[]byte("data block two with even more padding to vary sizes"),
	}
	regions := make([]BlockRegion, len(blocks))
	for i, b := range blocks {
		region, err := w.AppendDataBlock(b, int64(len(b)))
		if err != nil {
			t.Fatalf("AppendDataBlock %d: %v", i, err)
		}
		regions[i] = region
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}

	r, err := NewReader(bytes.NewReader(buf.Bytes()), int64(buf.Len()))
	if err != nil {
		t.Fatal(err)
	}

	// Pull the DataIndex back via the BCFile.index meta block.
	diEntry, err := r.MetaBlockEntry(DataIndexBlockName)
	if err != nil {
		t.Fatal(err)
	}
	diBytes, err := r.RawBlock(diEntry.Region)
	if err != nil {
		t.Fatal(err)
	}
	di, err := ReadDataIndex(bytes.NewReader(diBytes))
	if err != nil {
		t.Fatal(err)
	}
	if di.DefaultCompression != "none" {
		t.Errorf("DefaultCompression = %q, want none", di.DefaultCompression)
	}
	if len(di.Blocks) != len(blocks) {
		t.Fatalf("DataIndex blocks = %d, want %d", len(di.Blocks), len(blocks))
	}

	// Verify each block reads back at the right position with the right size.
	for i, region := range di.Blocks {
		if region.Offset != regions[i].Offset {
			t.Errorf("block %d offset = %d, want %d", i, region.Offset, regions[i].Offset)
		}
		got, err := r.RawBlock(region)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(got, blocks[i]) {
			t.Errorf("block %d content mismatch:\n  got: %q\n  want: %q", i, got, blocks[i])
		}
	}
}

// TestWriter_RoundtripMetaBlocks: write multiple named meta blocks +
// reopen, confirm the MetaIndex has them all with the right codec name.
func TestWriter_RoundtripMetaBlocks(t *testing.T) {
	var buf bytes.Buffer
	w := NewWriter(&buf, "none")

	blocks := map[string]struct {
		codec string
		body  []byte
	}{
		"RFile.index":      {"none", []byte("rfile index opaque body")},
		"my-other-meta":    {"gz", []byte("would-be gzipped bytes")},
		"third-meta-block": {"none", []byte("a third one")},
	}

	for name, b := range blocks {
		if _, err := w.AppendMetaBlock(name, b.codec, b.body, int64(len(b.body))); err != nil {
			t.Fatalf("AppendMetaBlock %q: %v", name, err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}

	r, err := NewReader(bytes.NewReader(buf.Bytes()), int64(buf.Len()))
	if err != nil {
		t.Fatal(err)
	}

	for name, want := range blocks {
		entry, err := r.MetaBlockEntry(name)
		if err != nil {
			t.Errorf("MetaBlockEntry(%q): %v", name, err)
			continue
		}
		if entry.CompressionAlgo != want.codec {
			t.Errorf("entry %q: codec = %q, want %q", name, entry.CompressionAlgo, want.codec)
		}
		got, err := r.RawBlock(entry.Region)
		if err != nil {
			t.Errorf("RawBlock(%q): %v", name, err)
			continue
		}
		if !bytes.Equal(got, want.body) {
			t.Errorf("block %q content mismatch", name)
		}
	}
}

func TestWriter_DuplicateMetaBlockErrors(t *testing.T) {
	var buf bytes.Buffer
	w := NewWriter(&buf, "none")
	if _, err := w.AppendMetaBlock("name", "none", []byte("x"), 1); err != nil {
		t.Fatal(err)
	}
	_, err := w.AppendMetaBlock("name", "none", []byte("y"), 1)
	if err == nil {
		t.Errorf("duplicate meta block name should error")
	}
	if !strings.Contains(err.Error(), "already exists") {
		t.Errorf("err = %v; expected 'already exists'", err)
	}
}

func TestWriter_PostCloseAppendErrors(t *testing.T) {
	var buf bytes.Buffer
	w := NewWriter(&buf, "none")
	_ = w.Close()

	if _, err := w.AppendDataBlock([]byte("x"), 1); err == nil {
		t.Errorf("AppendDataBlock after Close: expected error")
	}
	if _, err := w.AppendMetaBlock("n", "none", []byte("x"), 1); err == nil {
		t.Errorf("AppendMetaBlock after Close: expected error")
	}
	// Idempotent close.
	if err := w.Close(); err != nil {
		t.Errorf("second Close: %v", err)
	}
}

func TestWriter_LargeFile_HundredsOfBlocks(t *testing.T) {
	var buf bytes.Buffer
	w := NewWriter(&buf, "none")

	const N = 200
	wantSizes := make([]int64, N)
	for i := 0; i < N; i++ {
		body := []byte(strings.Repeat("ab", 100+i)) // varying size
		region, err := w.AppendDataBlock(body, int64(len(body)))
		if err != nil {
			t.Fatal(err)
		}
		wantSizes[i] = region.CompressedSize
		if region.CompressedSize != int64(len(body)) {
			t.Errorf("block %d size mismatch", i)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}

	r, err := NewReader(bytes.NewReader(buf.Bytes()), int64(buf.Len()))
	if err != nil {
		t.Fatal(err)
	}
	diEntry, _ := r.MetaBlockEntry(DataIndexBlockName)
	diBytes, _ := r.RawBlock(diEntry.Region)
	di, err := ReadDataIndex(bytes.NewReader(diBytes))
	if err != nil {
		t.Fatal(err)
	}
	if len(di.Blocks) != N {
		t.Fatalf("got %d blocks, want %d", len(di.Blocks), N)
	}
	for i, block := range di.Blocks {
		if block.CompressedSize != wantSizes[i] {
			t.Errorf("block %d size = %d, want %d", i, block.CompressedSize, wantSizes[i])
		}
	}
}
