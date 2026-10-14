package rfile

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"

	internalrfile "github.com/phrocker/shoal-oss/internal/rfile"
	"github.com/phrocker/shoal-oss/internal/rfile/bcfile"
	"github.com/phrocker/shoal-oss/internal/rfile/bcfile/block"
	"github.com/phrocker/shoal-oss/internal/rfile/index"
	"github.com/phrocker/shoal-oss/internal/rfile/relkey"
	"github.com/phrocker/shoal-oss/internal/rfile/wire"

	"github.com/phrocker/shoal-oss/accumulo"
)

// groupCell is one cell of a synthetic locality group.
type groupCell struct {
	key   *wire.Key
	value []byte
}

func cellOf(row, family, value string) groupCell {
	return groupCell{
		key: &wire.Key{
			Row:             []byte(row),
			ColumnFamily:    []byte(family),
			ColumnQualifier: []byte("cq"),
			Timestamp:       1,
		},
		value: []byte(value),
	}
}

// TestReadsEveryLocalityGroup pins that the public readers surface the cells of
// a named locality group. Shoal's writer emits the default group only
// ([SB-RFILE-012]), but Accumulo and other writers produce multi-group RFiles,
// and a reader that walked the default group alone would silently drop rows.
func TestReadsEveryLocalityGroup(t *testing.T) {
	path := filepath.Join(t.TempDir(), "multigroup.rf")
	contents := buildMultiGroupRFile(t, [][]groupCell{
		{cellOf("row1", "cf", "a"), cellOf("row3", "cf", "c")},
		{cellOf("row2", "vertex", "b"), cellOf("row4", "vertex", "d")},
	}, []string{"", "vertex"})
	if err := os.WriteFile(path, contents, 0o600); err != nil {
		t.Fatal(err)
	}

	reader, err := OpenSequential(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()

	var got []string
	for reader.HasTop() {
		top, err := reader.Top()
		if err != nil {
			t.Fatal(err)
		}
		got = append(got, string(top.Key.Row)+"="+string(top.Value))
		if err := reader.Next(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	want := []string{"row1=a", "row2=b", "row3=c", "row4=d"}
	if len(got) != len(want) {
		t.Fatalf("read %v, want %v: the named locality group must be merged in", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("read %v, want %v", got, want)
		}
	}

	// The merged stream must also seek, and every group's handle must be
	// released by one Close.
	seekable, err := NewSeekable(mustRowRange(t, "row3"))
	if err != nil {
		t.Fatal(err)
	}
	if err := reader.Seek(context.Background(), seekable); err != nil {
		t.Fatal(err)
	}
	if !reader.HasTop() {
		t.Fatal("seek to row3 found nothing")
	}
	top, err := reader.Top()
	if err != nil {
		t.Fatal(err)
	}
	if string(top.Key.Row) != "row3" {
		t.Fatalf("seek landed on %q, want row3", top.Key.Row)
	}
	if err := reader.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatalf("a group handle was left open: %v", err)
	}
}

func mustRowRange(t *testing.T, row string) *accumulo.Range {
	t.Helper()
	keyRange, err := accumulo.NewRange([]byte(row), true, nil, true)
	if err != nil {
		t.Fatal(err)
	}
	return keyRange
}

// buildMultiGroupRFile assembles a v8 RFile whose locality groups each hold one
// data block, mirroring internal/rfile's synthetic-file helper. groups[i] is
// written as one block owned by the locality group named names[i]; an empty
// name is the default group.
func buildMultiGroupRFile(t *testing.T, groups [][]groupCell, names []string) []byte {
	t.Helper()
	if len(groups) != len(names) {
		t.Fatalf("%d groups but %d names", len(groups), len(names))
	}

	var file bytes.Buffer
	regions := make([]bcfile.BlockRegion, len(groups))
	for i, cells := range groups {
		var raw bytes.Buffer
		var prev *wire.Key
		for _, c := range cells {
			if err := relkey.EncodeKey(&raw, prev, c.key, c.value); err != nil {
				t.Fatal(err)
			}
			prev = c.key.Clone()
		}
		offset := int64(file.Len())
		file.Write(raw.Bytes())
		regions[i] = bcfile.BlockRegion{
			Offset:         offset,
			CompressedSize: int64(raw.Len()),
			RawSize:        int64(raw.Len()),
		}
	}

	bcIndexOffset := int64(file.Len())
	if err := bcfile.WriteDataIndex(&file, &bcfile.DataIndex{
		DefaultCompression: block.CodecNone,
		Blocks:             regions,
	}); err != nil {
		t.Fatal(err)
	}
	bcIndexLen := int64(file.Len()) - bcIndexOffset

	localityGroups := make([]*index.LocalityGroup, 0, len(groups))
	for i, cells := range groups {
		var data bytes.Buffer
		entry := &index.IndexEntry{
			Key:            cells[len(cells)-1].key,
			NumEntries:     int32(len(cells)),
			Offset:         regions[i].Offset,
			CompressedSize: regions[i].CompressedSize,
			RawSize:        regions[i].RawSize,
		}
		if err := index.WriteIndexEntry(&data, entry); err != nil {
			t.Fatal(err)
		}
		families := map[string]int64{}
		for _, c := range cells {
			families[string(c.key.ColumnFamily)]++
		}
		localityGroups = append(localityGroups, &index.LocalityGroup{
			IsDefault:       names[i] == "",
			Name:            names[i],
			ColumnFamilies:  families,
			FirstKey:        cells[0].key,
			NumTotalEntries: int32(len(cells)),
			RootIndex: &index.IndexBlock{
				Level:   0,
				Offsets: []int32{0},
				Data:    data.Bytes(),
			},
		})
	}

	rfIndexOffset := int64(file.Len())
	if err := wire.WriteInt32(&file, index.RIndexMagic); err != nil {
		t.Fatal(err)
	}
	if err := wire.WriteInt32(&file, index.V8); err != nil {
		t.Fatal(err)
	}
	if err := wire.WriteInt32(&file, int32(len(localityGroups))); err != nil {
		t.Fatal(err)
	}
	for _, lg := range localityGroups {
		if err := index.WriteLocalityGroup(&file, lg, index.V8); err != nil {
			t.Fatal(err)
		}
	}
	rfIndexLen := int64(file.Len()) - rfIndexOffset

	metaIndexOffset := int64(file.Len())
	if err := bcfile.WriteMetaIndex(&file, &bcfile.MetaIndex{Entries: map[string]bcfile.MetaIndexEntry{
		bcfile.DataIndexBlockName: {
			Name:            bcfile.DataIndexBlockName,
			CompressionAlgo: block.CodecNone,
			Region:          bcfile.BlockRegion{Offset: bcIndexOffset, CompressedSize: bcIndexLen, RawSize: bcIndexLen},
		},
		internalrfile.IndexMetaBlockName: {
			Name:            internalrfile.IndexMetaBlockName,
			CompressionAlgo: block.CodecNone,
			Region:          bcfile.BlockRegion{Offset: rfIndexOffset, CompressedSize: rfIndexLen, RawSize: rfIndexLen},
		},
	}}); err != nil {
		t.Fatal(err)
	}

	cryptoOffset := int64(file.Len())
	file.Write(make([]byte, 8))
	if err := bcfile.WriteFooter(&file, bcfile.Footer{
		Version:            bcfile.APIVersion3,
		OffsetIndexMeta:    metaIndexOffset,
		OffsetCryptoParams: cryptoOffset,
	}); err != nil {
		t.Fatal(err)
	}
	return file.Bytes()
}
