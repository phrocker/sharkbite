package rfile

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"testing"

	"github.com/phrocker/shoal-oss/internal/rfile/bcfile"
	"github.com/phrocker/shoal-oss/internal/rfile/bcfile/block"
	"github.com/phrocker/shoal-oss/internal/rfile/index"
	"github.com/phrocker/shoal-oss/internal/rfile/relkey"
	"github.com/phrocker/shoal-oss/internal/rfile/wire"
)

// cell is a (Key, Value) pair for fixture building.
type cell struct {
	K *Key
	V []byte
}

func mkCell(row, val string) cell {
	return cell{
		K: &Key{
			Row:             []byte(row),
			ColumnFamily:    []byte("cf"),
			ColumnQualifier: []byte("cq"),
			Timestamp:       1,
		},
		V: []byte(val),
	}
}

// encodeDataBlock relkey-encodes cells into a raw block byte slice.
// Cells must already be in Key order (caller's responsibility — relkey
// decoder expects monotonic non-decreasing keys per Accumulo invariant).
func encodeDataBlock(t *testing.T, cells []cell) []byte {
	t.Helper()
	var buf bytes.Buffer
	var prev *Key
	for _, c := range cells {
		if err := relkey.EncodeKey(&buf, prev, c.K, c.V); err != nil {
			t.Fatal(err)
		}
		prev = c.K.Clone()
	}
	return buf.Bytes()
}

// buildRFile assembles a complete in-memory v8 RFile from groups of cells
// where each group becomes one data block. Returns:
//   - the file bytes (suitable as io.ReaderAt via bytes.NewReader)
//   - the file length
func buildRFile(t *testing.T, blocks [][]cell) ([]byte, int64) {
	t.Helper()

	// Layout plan (computed up-front; no compression — codec="none"):
	//   [data block 0]
	//   [data block 1]
	//   ...
	//   [BCFile.index meta block (DataIndex)]
	//   [RFile.index meta block]
	//   [MetaIndex region]
	//   [crypto params placeholder]
	//   [trailer: 16B offsets + 4B version + 16B magic]

	var file bytes.Buffer

	// 1. Data blocks. Record their on-disk regions.
	dataRegions := make([]bcfile.BlockRegion, len(blocks))
	for i, b := range blocks {
		raw := encodeDataBlock(t, b)
		off := int64(file.Len())
		file.Write(raw)
		dataRegions[i] = bcfile.BlockRegion{
			Offset:         off,
			CompressedSize: int64(len(raw)),
			RawSize:        int64(len(raw)),
		}
	}

	// 2. BCFile.index meta block: holds the DataIndex.
	bcIdxOffset := int64(file.Len())
	dataIndex := &bcfile.DataIndex{
		DefaultCompression: block.CodecNone,
		Blocks:             dataRegions,
	}
	if err := bcfile.WriteDataIndex(&file, dataIndex); err != nil {
		t.Fatal(err)
	}
	bcIdxLen := int64(file.Len()) - bcIdxOffset

	// 3. RFile.index meta block: one default locality group with a single
	// flat IndexBlock pointing at the data blocks.
	rfIdxOffset := int64(file.Len())
	rfIdx := buildRFileIndexBlock(t, blocks, dataRegions)
	if err := writeRFileIndexMetaBlock(&file, rfIdx); err != nil {
		t.Fatal(err)
	}
	rfIdxLen := int64(file.Len()) - rfIdxOffset

	// 4. MetaIndex: lists both meta blocks.
	metaIndexOffset := int64(file.Len())
	mi := &bcfile.MetaIndex{Entries: map[string]bcfile.MetaIndexEntry{
		bcfile.DataIndexBlockName: {
			Name:            bcfile.DataIndexBlockName,
			CompressionAlgo: block.CodecNone,
			Region:          bcfile.BlockRegion{Offset: bcIdxOffset, CompressedSize: bcIdxLen, RawSize: bcIdxLen},
		},
		IndexMetaBlockName: {
			Name:            IndexMetaBlockName,
			CompressionAlgo: block.CodecNone,
			Region:          bcfile.BlockRegion{Offset: rfIdxOffset, CompressedSize: rfIdxLen, RawSize: rfIdxLen},
		},
	}}
	if err := bcfile.WriteMetaIndex(&file, mi); err != nil {
		t.Fatal(err)
	}

	// 5. Crypto params placeholder. v3 trailer expects an offset; the
	// content doesn't matter to bcfile.NewReader (it doesn't parse).
	cryptoOffset := int64(file.Len())
	file.Write(make([]byte, 8)) // 8 placeholder bytes

	// 6. Trailer.
	footer := bcfile.Footer{
		Version:            bcfile.APIVersion3,
		OffsetIndexMeta:    metaIndexOffset,
		OffsetCryptoParams: cryptoOffset,
	}
	if err := bcfile.WriteFooter(&file, footer); err != nil {
		t.Fatal(err)
	}
	return file.Bytes(), int64(file.Len())
}

// buildRFileIndexBlock constructs the RFile-level index for a list of
// data blocks. One IndexEntry per block, with the entry's Key = last
// key in the block (per Java convention).
func buildRFileIndexBlock(t *testing.T, blocks [][]cell, regions []bcfile.BlockRegion) *index.Reader {
	t.Helper()
	entries := make([]*index.IndexEntry, len(blocks))
	for i, b := range blocks {
		if len(b) == 0 {
			t.Fatalf("block %d is empty — not allowed", i)
		}
		lastKey := b[len(b)-1].K
		entries[i] = &index.IndexEntry{
			Key:            lastKey,
			NumEntries:     int32(len(b)),
			Offset:         regions[i].Offset,
			CompressedSize: regions[i].CompressedSize,
			RawSize:        regions[i].RawSize,
		}
	}

	// Encode entries into the IndexBlock's Data + Offsets[].
	var data bytes.Buffer
	offsets := make([]int32, 0, len(entries))
	for _, e := range entries {
		offsets = append(offsets, int32(data.Len()))
		if err := index.WriteIndexEntry(&data, e); err != nil {
			t.Fatal(err)
		}
	}
	root := &index.IndexBlock{
		Level:   0,
		Offset:  0,
		HasNext: false,
		Offsets: offsets,
		Data:    data.Bytes(),
	}

	// Total entry count across all blocks.
	var totalEntries int32
	for _, b := range blocks {
		totalEntries += int32(len(b))
	}

	lg := &index.LocalityGroup{
		IsDefault:       true,
		ColumnFamilies:  map[string]int64{}, // empty (cfCount=0)
		FirstKey:        blocks[0][0].K,
		NumTotalEntries: totalEntries,
		RootIndex:       root,
	}

	return &index.Reader{
		Version: index.V8,
		Groups:  []*index.LocalityGroup{lg},
	}
}

// writeRFileIndexMetaBlock emits the wire form of an RFile.index meta
// block: magic + version + N × LocalityGroup (no v8 trailers).
func writeRFileIndexMetaBlock(w io.Writer, rdr *index.Reader) error {
	if err := wire.WriteInt32(w, index.RIndexMagic); err != nil {
		return err
	}
	if err := wire.WriteInt32(w, rdr.Version); err != nil {
		return err
	}
	if err := wire.WriteInt32(w, int32(len(rdr.Groups))); err != nil {
		return err
	}
	for _, lg := range rdr.Groups {
		if err := index.WriteLocalityGroup(w, lg, rdr.Version); err != nil {
			return err
		}
	}
	return nil
}

// openSynthetic packages the build → bcfile.NewReader → rfile.Open
// pipeline so individual tests stay readable.
func openSynthetic(t *testing.T, blocks [][]cell) *Reader {
	t.Helper()
	bs, length := buildRFile(t, blocks)
	bc, err := bcfile.NewReader(bytes.NewReader(bs), length)
	if err != nil {
		t.Fatalf("bcfile.NewReader: %v", err)
	}
	r, err := Open(bc, block.Default())
	if err != nil {
		t.Fatalf("rfile.Open: %v", err)
	}
	return r
}

// drainAll exhausts r and returns every (key,val) pair seen.
func drainAll(t *testing.T, r *Reader) []cell {
	t.Helper()
	var out []cell
	for {
		k, v, err := r.Next()
		if errors.Is(err, io.EOF) {
			return out
		}
		if err != nil {
			t.Fatalf("Next: %v", err)
		}
		out = append(out, cell{K: k, V: v})
	}
}

// ---------- Tests ----------

func TestReader_Open_RejectsNilArgs(t *testing.T) {
	_, err := Open(nil, block.Default())
	if err == nil {
		t.Errorf("nil bc: expected error")
	}
	bs, length := buildRFile(t, [][]cell{{mkCell("a", "1")}})
	bc, _ := bcfile.NewReader(bytes.NewReader(bs), length)
	_, err = Open(bc, nil)
	if err == nil {
		t.Errorf("nil dec: expected error")
	}
}

func TestReader_SingleBlock_Iterate(t *testing.T) {
	cells := []cell{
		mkCell("a", "1"),
		mkCell("b", "2"),
		mkCell("c", "3"),
	}
	r := openSynthetic(t, [][]cell{cells})
	defer r.Close()

	got := drainAll(t, r)
	if len(got) != len(cells) {
		t.Fatalf("got %d cells, want %d", len(got), len(cells))
	}
	for i, c := range got {
		if string(c.K.Row) != string(cells[i].K.Row) || string(c.V) != string(cells[i].V) {
			t.Errorf("cell %d: got (%s, %s), want (%s, %s)",
				i, c.K.Row, c.V, cells[i].K.Row, cells[i].V)
		}
	}
}

func TestReader_MultipleBlocks_IterateInOrder(t *testing.T) {
	blocks := [][]cell{
		{mkCell("a", "1"), mkCell("b", "2"), mkCell("c", "3")},
		{mkCell("d", "4"), mkCell("e", "5")},
		{mkCell("f", "6"), mkCell("g", "7"), mkCell("h", "8")},
	}
	r := openSynthetic(t, blocks)
	defer r.Close()

	got := drainAll(t, r)
	want := []string{"a", "b", "c", "d", "e", "f", "g", "h"}
	if len(got) != len(want) {
		t.Fatalf("got %d cells, want %d", len(got), len(want))
	}
	for i, c := range got {
		if string(c.K.Row) != want[i] {
			t.Errorf("cell %d: got %q, want %q", i, c.K.Row, want[i])
		}
	}
}

func TestReader_SeekRow_ExactMatch(t *testing.T) {
	r := openSynthetic(t, [][]cell{{
		mkCell("a", "1"), mkCell("b", "2"), mkCell("c", "3"),
	}})
	defer r.Close()

	if err := r.SeekRow([]byte("b")); err != nil {
		t.Fatal(err)
	}
	got := drainAll(t, r)
	want := []string{"b", "c"}
	if len(got) != len(want) {
		t.Fatalf("got %d, want %d (got=%v)", len(got), len(want), formatRows(got))
	}
	for i, c := range got {
		if string(c.K.Row) != want[i] {
			t.Errorf("cell %d: got %q, want %q", i, c.K.Row, want[i])
		}
	}
}

func TestReader_SeekRow_BetweenKeys(t *testing.T) {
	// Target "ab" is not present; seek must land on first key >= "ab",
	// which is "b".
	r := openSynthetic(t, [][]cell{{
		mkCell("a", "1"), mkCell("b", "2"), mkCell("c", "3"),
	}})
	defer r.Close()

	if err := r.SeekRow([]byte("ab")); err != nil {
		t.Fatal(err)
	}
	k, _, err := r.Next()
	if err != nil {
		t.Fatal(err)
	}
	if string(k.Row) != "b" {
		t.Errorf("first cell after seek: got %q, want %q", k.Row, "b")
	}
}

func TestReader_SeekRow_PastEnd(t *testing.T) {
	r := openSynthetic(t, [][]cell{{
		mkCell("a", "1"), mkCell("b", "2"),
	}})
	defer r.Close()

	if err := r.SeekRow([]byte("zzz")); err != nil {
		t.Fatal(err)
	}
	_, _, err := r.Next()
	if !errors.Is(err, io.EOF) {
		t.Errorf("err = %v, want io.EOF", err)
	}
}

func TestReader_SeekRow_BeforeFirst(t *testing.T) {
	// Seek to a row before the smallest — should yield everything.
	r := openSynthetic(t, [][]cell{{
		mkCell("m", "1"), mkCell("n", "2"), mkCell("o", "3"),
	}})
	defer r.Close()

	if err := r.SeekRow([]byte("a")); err != nil {
		t.Fatal(err)
	}
	got := drainAll(t, r)
	if len(got) != 3 {
		t.Errorf("got %d, want 3", len(got))
	}
}

// TestReader_SeekIntoLaterBlock targets a row whose block is NOT the
// first — exercises block-skip during seek.
func TestReader_SeekIntoLaterBlock(t *testing.T) {
	blocks := [][]cell{
		{mkCell("a", "1"), mkCell("b", "2")}, // block 0: a..b
		{mkCell("d", "3"), mkCell("e", "4")}, // block 1: d..e
		{mkCell("g", "5"), mkCell("h", "6")}, // block 2: g..h
	}
	r := openSynthetic(t, blocks)
	defer r.Close()

	if err := r.SeekRow([]byte("e")); err != nil {
		t.Fatal(err)
	}
	got := drainAll(t, r)
	want := []string{"e", "g", "h"}
	if len(got) != len(want) {
		t.Fatalf("got %d, want %d", len(got), len(want))
	}
	for i, c := range got {
		if string(c.K.Row) != want[i] {
			t.Errorf("cell %d: got %q, want %q", i, c.K.Row, want[i])
		}
	}
}

// TestReader_SeekBetweenBlocks: target falls in the gap between two
// blocks (after block N's last key, before block N+1's first key).
// Should land on block N+1's first key.
func TestReader_SeekBetweenBlocks(t *testing.T) {
	blocks := [][]cell{
		{mkCell("a", "1"), mkCell("b", "2")},
		{mkCell("m", "3"), mkCell("n", "4")},
	}
	r := openSynthetic(t, blocks)
	defer r.Close()

	// "f" is between blocks (b < f < m). Seek must land on "m".
	if err := r.SeekRow([]byte("f")); err != nil {
		t.Fatal(err)
	}
	k, _, err := r.Next()
	if err != nil {
		t.Fatal(err)
	}
	if string(k.Row) != "m" {
		t.Errorf("got %q, want %q", k.Row, "m")
	}
}

func TestReader_NextWithoutSeekImplicitlyStartsAtBeginning(t *testing.T) {
	r := openSynthetic(t, [][]cell{{
		mkCell("a", "1"), mkCell("b", "2"),
	}})
	defer r.Close()

	k, _, err := r.Next() // no Seek
	if err != nil {
		t.Fatal(err)
	}
	if string(k.Row) != "a" {
		t.Errorf("first cell = %q, want %q", k.Row, "a")
	}
}

func TestReader_EOFIsSticky(t *testing.T) {
	r := openSynthetic(t, [][]cell{{mkCell("a", "1")}})
	defer r.Close()

	_, _, _ = r.Next() // consume the only cell
	for i := 0; i < 5; i++ {
		_, _, err := r.Next()
		if !errors.Is(err, io.EOF) {
			t.Errorf("call %d: got %v, want io.EOF", i, err)
		}
	}
}

func TestReader_ReseekAfterDrain(t *testing.T) {
	cells := []cell{
		mkCell("a", "1"), mkCell("b", "2"), mkCell("c", "3"),
	}
	r := openSynthetic(t, [][]cell{cells})
	defer r.Close()

	// Drain.
	_ = drainAll(t, r)
	// Reseek to start; should produce all cells again.
	if err := r.SeekRow([]byte("a")); err != nil {
		t.Fatal(err)
	}
	got := drainAll(t, r)
	if len(got) != len(cells) {
		t.Errorf("after reseek: got %d, want %d", len(got), len(cells))
	}
}

func TestReader_LargeFile_OneThousandRows_FiveBlocks(t *testing.T) {
	const N = 1000
	const blocksN = 5
	perBlock := N / blocksN

	blocks := make([][]cell, blocksN)
	for b := 0; b < blocksN; b++ {
		blocks[b] = make([]cell, perBlock)
		for i := 0; i < perBlock; i++ {
			rowIdx := b*perBlock + i
			blocks[b][i] = mkCell(fmt.Sprintf("row%05d", rowIdx), fmt.Sprintf("v%d", rowIdx))
		}
	}
	r := openSynthetic(t, blocks)
	defer r.Close()

	got := drainAll(t, r)
	if len(got) != N {
		t.Fatalf("got %d, want %d", len(got), N)
	}
	// Confirm sorted.
	if !sort.SliceIsSorted(got, func(i, j int) bool {
		return bytes.Compare(got[i].K.Row, got[j].K.Row) < 0
	}) {
		t.Errorf("output not sorted")
	}
	// Spot-check a seek.
	if err := r.SeekRow([]byte("row00500")); err != nil {
		t.Fatal(err)
	}
	k, _, err := r.Next()
	if err != nil {
		t.Fatal(err)
	}
	if string(k.Row) != "row00500" {
		t.Errorf("seek: got %q, want row00500", k.Row)
	}
}

func TestReader_NoDefaultLG_Errors(t *testing.T) {
	// Construct an RFile with only a NAMED LG (no default). Open should
	// reject — V0 only iterates the default.
	cells := []cell{mkCell("a", "1"), mkCell("b", "2")}
	dataBytes := encodeDataBlock(t, cells)

	var file bytes.Buffer
	dataOff := int64(file.Len())
	file.Write(dataBytes)
	dataRegion := bcfile.BlockRegion{Offset: dataOff, CompressedSize: int64(len(dataBytes)), RawSize: int64(len(dataBytes))}

	bcIdxOff := int64(file.Len())
	if err := bcfile.WriteDataIndex(&file, &bcfile.DataIndex{
		DefaultCompression: block.CodecNone,
		Blocks:             []bcfile.BlockRegion{dataRegion},
	}); err != nil {
		t.Fatal(err)
	}
	bcIdxLen := int64(file.Len()) - bcIdxOff

	rfIdxOff := int64(file.Len())
	// Build a NAMED-only LG.
	var data bytes.Buffer
	offsets := []int32{0}
	if err := index.WriteIndexEntry(&data, &index.IndexEntry{
		Key: cells[len(cells)-1].K, NumEntries: int32(len(cells)),
		Offset: dataRegion.Offset, CompressedSize: dataRegion.CompressedSize, RawSize: dataRegion.RawSize,
	}); err != nil {
		t.Fatal(err)
	}
	rdr := &index.Reader{
		Version: index.V8,
		Groups: []*index.LocalityGroup{{
			IsDefault:       false,
			Name:            "named-only",
			ColumnFamilies:  map[string]int64{"cf": 1},
			FirstKey:        cells[0].K,
			NumTotalEntries: int32(len(cells)),
			RootIndex: &index.IndexBlock{
				Level: 0, Offsets: offsets, Data: data.Bytes(),
			},
		}},
	}
	if err := writeRFileIndexMetaBlock(&file, rdr); err != nil {
		t.Fatal(err)
	}
	rfIdxLen := int64(file.Len()) - rfIdxOff

	miOff := int64(file.Len())
	if err := bcfile.WriteMetaIndex(&file, &bcfile.MetaIndex{Entries: map[string]bcfile.MetaIndexEntry{
		bcfile.DataIndexBlockName: {Name: bcfile.DataIndexBlockName, CompressionAlgo: block.CodecNone,
			Region: bcfile.BlockRegion{Offset: bcIdxOff, CompressedSize: bcIdxLen, RawSize: bcIdxLen}},
		IndexMetaBlockName: {Name: IndexMetaBlockName, CompressionAlgo: block.CodecNone,
			Region: bcfile.BlockRegion{Offset: rfIdxOff, CompressedSize: rfIdxLen, RawSize: rfIdxLen}},
	}}); err != nil {
		t.Fatal(err)
	}
	cryptoOff := int64(file.Len())
	file.Write(make([]byte, 8))
	if err := bcfile.WriteFooter(&file, bcfile.Footer{
		Version: bcfile.APIVersion3, OffsetIndexMeta: miOff, OffsetCryptoParams: cryptoOff,
	}); err != nil {
		t.Fatal(err)
	}

	bc, err := bcfile.NewReader(bytes.NewReader(file.Bytes()), int64(file.Len()))
	if err != nil {
		t.Fatal(err)
	}
	_, err = Open(bc, block.Default())
	if err == nil {
		t.Errorf("expected error when no default LG present")
	}
	if !strings.Contains(err.Error(), "default") {
		t.Errorf("err = %v; expected 'default' in message", err)
	}
}

func TestReader_LocalityGroup_Accessor(t *testing.T) {
	r := openSynthetic(t, [][]cell{{mkCell("a", "1")}})
	defer r.Close()
	lg := r.LocalityGroup()
	if lg == nil || !lg.IsDefault {
		t.Errorf("LocalityGroup should be the default LG")
	}
}

func TestValidateDataRegionsRejectsMetadataOverlapAndDataOverlap(t *testing.T) {
	tests := []struct {
		name    string
		regions []bcfile.BlockRegion
		end     int64
	}{
		{
			name: "metadata overlap",
			regions: []bcfile.BlockRegion{
				{Offset: 90, CompressedSize: 11, RawSize: 11},
			},
			end: 100,
		},
		{
			name: "data overlap",
			regions: []bcfile.BlockRegion{
				{Offset: 10, CompressedSize: 20, RawSize: 20},
				{Offset: 29, CompressedSize: 10, RawSize: 10},
			},
			end: 100,
		},
		{
			name: "out of order",
			regions: []bcfile.BlockRegion{
				{Offset: 30, CompressedSize: 10, RawSize: 10},
				{Offset: 10, CompressedSize: 10, RawSize: 10},
			},
			end: 100,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := validateDataRegions(tt.regions, tt.end); err == nil {
				t.Fatal("expected invalid data regions to be rejected")
			}
		})
	}
}

func TestFetchDataBlockRequiresValidatedDataRegion(t *testing.T) {
	r := openSynthetic(t, [][]cell{{mkCell("a", "1")}})
	defer r.Close()

	unlisted := r.dataBlocks[0]
	unlisted.RawSize++
	if _, err := r.fetchDataBlock(unlisted, r.dataCodec); err == nil ||
		!strings.Contains(err.Error(), "not declared by BCFile.index") {
		t.Fatalf("fetchDataBlock unlisted region error = %v, want declaration rejection", err)
	}

	meta, err := r.bc.MetaBlockEntry(IndexMetaBlockName)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.fetchDataBlock(meta.Region, meta.CompressionAlgo); err == nil ||
		!strings.Contains(err.Error(), "block region") {
		t.Fatalf("fetchDataBlock metadata region error = %v, want declaration rejection", err)
	}
}

func TestValidateReadableIndexRejectsUnverifiedExtensions(t *testing.T) {
	tests := []struct {
		name string
		idx  *index.Reader
	}{
		{
			name: "vector extension",
			idx: &index.Reader{VectorExtension: &index.OpaqueExtension{
				Kind: index.OpaqueV8VectorAndTessellation,
			}},
		},
		{
			name: "sample groups",
			idx:  &index.Reader{SampleGroups: []*index.LocalityGroup{{}}},
		},
		{
			name: "sampler configuration",
			idx:  &index.Reader{SamplerConfiguration: &index.SamplerConfiguration{}},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if err := validateReadableIndex(test.idx); err == nil {
				t.Fatal("expected unsupported index feature rejection")
			}
		})
	}
}

func buildValidationIndexBlock(t *testing.T, entries ...*index.IndexEntry) *index.IndexBlock {
	t.Helper()
	var data bytes.Buffer
	offsets := make([]int32, 0, len(entries))
	for _, entry := range entries {
		offsets = append(offsets, int32(data.Len()))
		if err := index.WriteIndexEntry(&data, entry); err != nil {
			t.Fatal(err)
		}
	}
	return &index.IndexBlock{Offsets: offsets, Data: data.Bytes()}
}

func TestValidateReadableIndexRejectsEntryCountRootMismatch(t *testing.T) {
	tests := map[string]*index.LocalityGroup{
		"zero count with nonempty root": {
			IsDefault:       true,
			NumTotalEntries: 0,
			RootIndex: buildValidationIndexBlock(t, &index.IndexEntry{
				Key: mkCell("a", "1").K, NumEntries: 1,
			}),
		},
		"positive count with empty root": {
			IsDefault:       true,
			NumTotalEntries: 1,
			RootIndex:       &index.IndexBlock{},
		},
	}
	for name, group := range tests {
		t.Run(name, func(t *testing.T) {
			err := validateReadableIndex(&index.Reader{
				Groups: []*index.LocalityGroup{group},
			})
			if err == nil || !strings.Contains(err.Error(), "inconsistent with its root index") {
				t.Fatalf("validateReadableIndex() = %v, want count/root mismatch", err)
			}
		})
	}
}

func TestValidateReadableIndexRejectsUnorderedRoot(t *testing.T) {
	root := buildValidationIndexBlock(
		t,
		&index.IndexEntry{Key: mkCell("z", "1").K, NumEntries: 1},
		&index.IndexEntry{Key: mkCell("a", "1").K, NumEntries: 1},
	)
	err := validateReadableIndex(&index.Reader{Groups: []*index.LocalityGroup{{
		IsDefault:       true,
		NumTotalEntries: 2,
		RootIndex:       root,
	}}})
	if err == nil || !strings.Contains(err.Error(), "sorts before") {
		t.Fatalf("validateReadableIndex() = %v, want key-order rejection", err)
	}
}

func TestValidateReadableIndexRejectsInvalidLocalityGroupLayouts(t *testing.T) {
	emptyRoot := func() *index.IndexBlock {
		return &index.IndexBlock{}
	}
	tests := map[string]struct {
		groups []*index.LocalityGroup
		want   string
	}{
		"missing default": {
			groups: []*index.LocalityGroup{{
				Name:           "named",
				ColumnFamilies: map[string]int64{"cf": 1},
				RootIndex:      emptyRoot(),
			}},
			want: "exactly one default locality group",
		},
		"duplicate default": {
			groups: []*index.LocalityGroup{
				{IsDefault: true, RootIndex: emptyRoot()},
				{IsDefault: true, RootIndex: emptyRoot()},
			},
			want: "exactly one default locality group",
		},
		"named default": {
			groups: []*index.LocalityGroup{{
				IsDefault: true,
				Name:      "invalid",
				RootIndex: emptyRoot(),
			}},
			want: "default locality group 0 has name",
		},
		"empty named group name": {
			groups: []*index.LocalityGroup{
				{IsDefault: true, RootIndex: emptyRoot()},
				{RootIndex: emptyRoot()},
			},
			want: "named locality group 1 has an empty name",
		},
		"duplicate named group": {
			groups: []*index.LocalityGroup{
				{IsDefault: true, RootIndex: emptyRoot()},
				{Name: "named", RootIndex: emptyRoot()},
				{Name: "named", RootIndex: emptyRoot()},
			},
			want: "duplicate name",
		},
		"column family in multiple groups": {
			groups: []*index.LocalityGroup{
				{
					IsDefault:       true,
					ColumnFamilies:  map[string]int64{"shared": 1},
					NumTotalEntries: 1,
					RootIndex: buildValidationIndexBlock(t, &index.IndexEntry{
						Key: mkCell("a", "1").K, NumEntries: 1,
					}),
				},
				{
					Name:            "named",
					ColumnFamilies:  map[string]int64{"shared": 1},
					NumTotalEntries: 1,
					RootIndex: buildValidationIndexBlock(t, &index.IndexEntry{
						Key: mkCell("b", "1").K, NumEntries: 1,
					}),
				},
			},
			want: "belongs to locality groups 0 and 1",
		},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			err := validateReadableIndex(&index.Reader{Groups: test.groups})
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("validateReadableIndex() = %v, want error containing %q", err, test.want)
			}
		})
	}
}

func TestReader_Close_Idempotent(t *testing.T) {
	r := openSynthetic(t, [][]cell{{mkCell("a", "1")}})
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	if err := r.Close(); err != nil {
		t.Errorf("second Close: %v", err)
	}
}

// formatRows is a tiny diagnostic helper used by failure messages.
func formatRows(cells []cell) []string {
	out := make([]string, len(cells))
	for i, c := range cells {
		out[i] = string(c.K.Row)
	}
	return out
}
