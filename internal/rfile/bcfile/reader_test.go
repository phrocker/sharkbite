package bcfile

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"math"
	"strings"
	"testing"
)

// buildSyntheticBCFile assembles a minimal v3 BCFile in memory: some data
// bytes, a MetaIndex with one entry, then crypto-params (placeholder
// zero-length region), then the v3 trailer. We don't compress anything —
// the test only exercises container parsing.
func buildSyntheticBCFile(t *testing.T, mi *MetaIndex, dataBlock []byte, dataBlockOffset int64) ([]byte, Footer) {
	t.Helper()
	var buf bytes.Buffer

	// 1. Pad with leading bytes so dataBlockOffset is honored.
	if dataBlockOffset > 0 {
		buf.Write(bytes.Repeat([]byte{0xaa}, int(dataBlockOffset)))
	}
	// 2. Write the data block (raw — no codec for this test).
	buf.Write(dataBlock)
	// 3. MetaIndex block — capture its offset before writing.
	metaIndexOffset := int64(buf.Len())
	if err := WriteMetaIndex(&buf, mi); err != nil {
		t.Fatal(err)
	}
	// 4. Crypto params block (empty placeholder for v3). Record offset.
	cryptoOffset := int64(buf.Len())
	// Write a placeholder 8 bytes so the region isn't empty — we never
	// parse this; we just need an offset that's between MetaIndex end and
	// trailer start.
	binary.Write(&buf, binary.BigEndian, uint64(0))

	// 5. Trailer.
	footer := Footer{
		Version:            APIVersion3,
		OffsetIndexMeta:    metaIndexOffset,
		OffsetCryptoParams: cryptoOffset,
	}
	if err := WriteFooter(&buf, footer); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes(), footer
}

func TestReader_ParsesFooterAndMetaIndex(t *testing.T) {
	want := &MetaIndex{Entries: map[string]MetaIndexEntry{
		DataIndexBlockName: {
			Name:            DataIndexBlockName,
			CompressionAlgo: "gz",
			Region:          BlockRegion{Offset: 100, CompressedSize: 50, RawSize: 200},
		},
		"RFile.index": {
			Name:            "RFile.index",
			CompressionAlgo: "snappy",
			Region:          BlockRegion{Offset: 200, CompressedSize: 80, RawSize: 256},
		},
	}}
	dataBlock := bytes.Repeat([]byte{0x42}, 200)
	bs, footer := buildSyntheticBCFile(t, want, dataBlock, 100)

	r, err := NewReader(bytes.NewReader(bs), int64(len(bs)))
	if err != nil {
		t.Fatal(err)
	}

	if r.Footer() != footer {
		t.Errorf("Footer = %+v, want %+v", r.Footer(), footer)
	}
	got := r.MetaIndex()
	if len(got.Entries) != 2 {
		t.Fatalf("entries len = %d, want 2", len(got.Entries))
	}
	for name, w := range want.Entries {
		g, err := r.MetaBlockEntry(name)
		if err != nil {
			t.Errorf("MetaBlockEntry(%q): %v", name, err)
			continue
		}
		if g != w {
			t.Errorf("entry %q: got %+v, want %+v", name, g, w)
		}
	}
}

func TestReader_MissingMetaBlock(t *testing.T) {
	mi := &MetaIndex{Entries: map[string]MetaIndexEntry{
		DataIndexBlockName: {Name: DataIndexBlockName, CompressionAlgo: "gz"},
	}}
	bs, _ := buildSyntheticBCFile(t, mi, nil, 0)

	r, err := NewReader(bytes.NewReader(bs), int64(len(bs)))
	if err != nil {
		t.Fatal(err)
	}
	_, err = r.MetaBlockEntry("nonexistent")
	if !errors.Is(err, ErrNoSuchMetaBlock) {
		t.Errorf("err = %v, want ErrNoSuchMetaBlock", err)
	}
}

func TestReader_RawBlockBoundsCheck(t *testing.T) {
	mi := &MetaIndex{Entries: map[string]MetaIndexEntry{
		DataIndexBlockName: {Name: DataIndexBlockName, CompressionAlgo: "gz"},
	}}
	bs, _ := buildSyntheticBCFile(t, mi, nil, 0)
	r, err := NewReader(bytes.NewReader(bs), int64(len(bs)))
	if err != nil {
		t.Fatal(err)
	}
	// Region claims more bytes than the file has.
	_, err = r.RawBlock(BlockRegion{Offset: 0, CompressedSize: int64(len(bs) + 1), RawSize: 1})
	if err == nil {
		t.Errorf("expected out-of-bounds error")
	}
	// Negative CompressedSize.
	_, err = r.RawBlock(BlockRegion{Offset: 0, CompressedSize: -1})
	if err == nil {
		t.Errorf("expected negative-size error")
	}
	// Offset + size must be checked without overflowing int64.
	_, err = r.RawBlock(BlockRegion{Offset: 1, CompressedSize: math.MaxInt64})
	if err == nil || !strings.Contains(err.Error(), "exceeds") {
		t.Errorf("overflowing block region error = %v, want allocation-limit rejection", err)
	}
	// Raw size is bounded before any decompressor can preallocate it.
	_, err = r.RawBlock(BlockRegion{
		Offset: 0, CompressedSize: 0, RawSize: MaxRawBlockSize + 1,
	})
	if err == nil || !strings.Contains(err.Error(), "raw block size") {
		t.Errorf("oversized raw block error = %v, want raw-size rejection", err)
	}
}

func TestReaderRejectsOversizedIndexedRegionBeforeAllocation(t *testing.T) {
	mi := &MetaIndex{Entries: map[string]MetaIndexEntry{
		"RFile.index": {
			Name:            "RFile.index",
			CompressionAlgo: "gz",
			Region: BlockRegion{
				Offset: 0, CompressedSize: 0, RawSize: MaxRawBlockSize + 1,
			},
		},
	}}
	bs, _ := buildSyntheticBCFile(t, mi, nil, 0)

	_, err := NewReader(bytes.NewReader(bs), int64(len(bs)))
	if err == nil || !strings.Contains(err.Error(), "raw block size") {
		t.Fatalf("NewReader oversized region error = %v, want preallocation rejection", err)
	}
}

func TestReaderRejectsMetaBlockInsideMetaIndex(t *testing.T) {
	mi := &MetaIndex{Entries: map[string]MetaIndexEntry{
		DataIndexBlockName: {
			Name:            DataIndexBlockName,
			CompressionAlgo: "none",
			Region:          BlockRegion{Offset: 1, CompressedSize: 1, RawSize: 1},
		},
	}}
	_, footer := buildSyntheticBCFile(t, mi, nil, 0)
	mi.Entries[DataIndexBlockName] = MetaIndexEntry{
		Name:            DataIndexBlockName,
		CompressionAlgo: "none",
		Region: BlockRegion{
			Offset: footer.OffsetIndexMeta, CompressedSize: 1, RawSize: 1,
		},
	}
	bs, _ := buildSyntheticBCFile(t, mi, nil, 0)

	_, err := NewReader(bytes.NewReader(bs), int64(len(bs)))
	if err == nil || !strings.Contains(err.Error(), "out of bounds") {
		t.Fatalf("NewReader metadata overlap error = %v, want bounds rejection", err)
	}
}

func TestReaderRejectsOverlappingMetaBlocks(t *testing.T) {
	mi := &MetaIndex{Entries: map[string]MetaIndexEntry{
		DataIndexBlockName: {
			Name:            DataIndexBlockName,
			CompressionAlgo: "none",
			Region:          BlockRegion{Offset: 10, CompressedSize: 20, RawSize: 20},
		},
		"RFile.index": {
			Name:            "RFile.index",
			CompressionAlgo: "none",
			Region:          BlockRegion{Offset: 20, CompressedSize: 20, RawSize: 20},
		},
	}}
	bs, _ := buildSyntheticBCFile(t, mi, bytes.Repeat([]byte{0x42}, 50), 0)

	_, err := NewReader(bytes.NewReader(bs), int64(len(bs)))
	if err == nil || !strings.Contains(err.Error(), "overlap") {
		t.Fatalf("NewReader overlapping metadata error = %v, want overlap rejection", err)
	}
}

func TestReader_RawBlockReturnsExactBytes(t *testing.T) {
	dataBlock := []byte("hello, bcfile data block")
	mi := &MetaIndex{Entries: map[string]MetaIndexEntry{
		DataIndexBlockName: {Name: DataIndexBlockName, CompressionAlgo: "none"},
	}}
	bs, _ := buildSyntheticBCFile(t, mi, dataBlock, 50)
	r, err := NewReader(bytes.NewReader(bs), int64(len(bs)))
	if err != nil {
		t.Fatal(err)
	}
	got, err := r.RawBlock(BlockRegion{Offset: 50, CompressedSize: int64(len(dataBlock)), RawSize: int64(len(dataBlock))})
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, dataBlock) {
		t.Errorf("RawBlock = %q, want %q", got, dataBlock)
	}
}

func TestReader_NotABCFile(t *testing.T) {
	junk := bytes.Repeat([]byte{0xff}, 200)
	_, err := NewReader(bytes.NewReader(junk), int64(len(junk)))
	if !errors.Is(err, ErrBadMagic) {
		t.Errorf("err = %v, want ErrBadMagic", err)
	}
}

func TestReaderRejectsHostileFooterOffsets(t *testing.T) {
	tests := []struct {
		name   string
		footer Footer
	}{
		{
			name: "uint64 conversion overflow",
			footer: Footer{
				Version:            APIVersion3,
				OffsetIndexMeta:    -1,
				OffsetCryptoParams: 8,
			},
		},
		{
			name: "negative crypto offset",
			footer: Footer{
				Version:            APIVersion3,
				OffsetIndexMeta:    1,
				OffsetCryptoParams: -1,
			},
		},
		{
			name: "non-monotonic offsets",
			footer: Footer{
				Version:            APIVersion3,
				OffsetIndexMeta:    16,
				OffsetCryptoParams: 8,
			},
		},
		{
			name: "huge offset outside tiny file",
			footer: Footer{
				Version:            APIVersion3,
				OffsetIndexMeta:    1,
				OffsetCryptoParams: math.MaxInt64,
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var image bytes.Buffer
			image.Write(make([]byte, 32))
			if err := WriteFooter(&image, tc.footer); err != nil {
				t.Fatalf("WriteFooter: %v", err)
			}
			_, err := NewReader(bytes.NewReader(image.Bytes()), int64(image.Len()))
			if !errors.Is(err, ErrInvalidFooterOffsets) {
				t.Fatalf("NewReader() = %v, want ErrInvalidFooterOffsets", err)
			}
		})
	}
}

func TestReaderRejectsHugeMetaIndexBeforeAllocation(t *testing.T) {
	fileLength := int64(math.MaxInt64)
	footer := Footer{
		Version:            APIVersion3,
		OffsetIndexMeta:    1,
		OffsetCryptoParams: fileLength - int64(FooterMinSizeV3),
	}
	src := newSparseFooterReader(t, fileLength, footer)

	_, err := NewReader(src, fileLength)
	if !errors.Is(err, ErrMetaIndexTooLarge) {
		t.Fatalf("NewReader() = %v, want ErrMetaIndexTooLarge", err)
	}
	if src.reads != 2 {
		t.Fatalf("ReadAt calls = %d, want only the two footer reads", src.reads)
	}
}

type sparseFooterReader struct {
	fileLength  int64
	trailer     []byte
	trailerBase int64
	reads       int
}

func newSparseFooterReader(t *testing.T, fileLength int64, footer Footer) *sparseFooterReader {
	t.Helper()
	var trailer bytes.Buffer
	if err := WriteFooter(&trailer, footer); err != nil {
		t.Fatalf("WriteFooter: %v", err)
	}
	return &sparseFooterReader{
		fileLength:  fileLength,
		trailer:     trailer.Bytes(),
		trailerBase: fileLength - int64(trailer.Len()),
	}
}

func (r *sparseFooterReader) ReadAt(p []byte, off int64) (int, error) {
	r.reads++
	if off < r.trailerBase || off > r.fileLength-int64(len(p)) {
		return 0, io.EOF
	}
	start := off - r.trailerBase
	n := copy(p, r.trailer[start:])
	if n != len(p) {
		return n, io.EOF
	}
	return n, nil
}
