package bcfile

import (
	"bytes"
	"errors"
	"strings"
	"testing"
)

func TestStringRoundtrip(t *testing.T) {
	cases := []string{
		"",
		"BCFile.index",
		"snappy",
		"a longer string that crosses single-byte vint length encoding",
	}
	for _, s := range cases {
		t.Run(s, func(t *testing.T) {
			var buf bytes.Buffer
			if err := WriteString(&buf, s); err != nil {
				t.Fatal(err)
			}
			got, ok, err := ReadString(&buf)
			if err != nil {
				t.Fatal(err)
			}
			if !ok {
				t.Errorf("ok = false, want true (only -1 sentinel produces nil)")
			}
			if got != s {
				t.Errorf("got %q, want %q", got, s)
			}
		})
	}
}

func TestNullStringRoundtrip(t *testing.T) {
	var buf bytes.Buffer
	if err := WriteNullString(&buf); err != nil {
		t.Fatal(err)
	}
	got, ok, err := ReadString(&buf)
	if err != nil {
		t.Fatal(err)
	}
	if ok {
		t.Errorf("ok = true, want false (null sentinel)")
	}
	if got != "" {
		t.Errorf("got %q, want empty", got)
	}
}

func TestBlockRegionRoundtrip(t *testing.T) {
	cases := []BlockRegion{
		{Offset: 0, CompressedSize: 0, RawSize: 0},
		{Offset: 1, CompressedSize: 2, RawSize: 3},
		{Offset: 1 << 40, CompressedSize: 1 << 16, RawSize: 1 << 17},
	}
	for _, br := range cases {
		var buf bytes.Buffer
		if err := WriteBlockRegion(&buf, br); err != nil {
			t.Fatal(err)
		}
		got, err := ReadBlockRegion(&buf)
		if err != nil {
			t.Fatal(err)
		}
		if got != br {
			t.Errorf("got %+v, want %+v", got, br)
		}
	}
}

func TestMetaIndexRoundtrip(t *testing.T) {
	want := &MetaIndex{Entries: map[string]MetaIndexEntry{
		"BCFile.index": {
			Name:            "BCFile.index",
			CompressionAlgo: "gz",
			Region:          BlockRegion{Offset: 1024, CompressedSize: 256, RawSize: 1024},
		},
		"RFile.index": {
			Name:            "RFile.index",
			CompressionAlgo: "snappy",
			Region:          BlockRegion{Offset: 2048, CompressedSize: 512, RawSize: 1500},
		},
	}}
	var buf bytes.Buffer
	if err := WriteMetaIndex(&buf, want); err != nil {
		t.Fatal(err)
	}
	got, err := ReadMetaIndex(&buf)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Entries) != len(want.Entries) {
		t.Fatalf("entries len = %d, want %d", len(got.Entries), len(want.Entries))
	}
	for name, w := range want.Entries {
		g, ok := got.Lookup(name)
		if !ok {
			t.Errorf("missing entry %q", name)
			continue
		}
		if g != w {
			t.Errorf("entry %q: got %+v, want %+v", name, g, w)
		}
	}
}

func TestMetaIndexMissingPrefix(t *testing.T) {
	// Manually craft a MetaIndex byte stream with a name missing "data:".
	var buf bytes.Buffer
	bw := byteWriterShim{w: &buf}
	if _, err := WriteVInt(bw, 1); err != nil { // count = 1
		t.Fatal(err)
	}
	// Write "wrongprefix:foo" instead of "data:foo".
	if err := WriteString(&buf, "wrongprefix:foo"); err != nil {
		t.Fatal(err)
	}
	if err := WriteString(&buf, "gz"); err != nil {
		t.Fatal(err)
	}
	if err := WriteBlockRegion(&buf, BlockRegion{}); err != nil {
		t.Fatal(err)
	}

	_, err := ReadMetaIndex(&buf)
	if !errors.Is(err, ErrMissingDataPrefix) {
		t.Errorf("err = %v, want ErrMissingDataPrefix", err)
	}
}

func TestMetaIndexRejectsExcessiveEntryCount(t *testing.T) {
	var buf bytes.Buffer
	if _, err := WriteVInt(byteWriterShim{w: &buf}, maxMetaIndexEntries+1); err != nil {
		t.Fatal(err)
	}

	_, err := ReadMetaIndex(&buf)
	if err == nil {
		t.Fatal("ReadMetaIndex accepted excessive entry count")
	}
}

func TestMetaIndexRejectsDuplicateNames(t *testing.T) {
	var buf bytes.Buffer
	if _, err := WriteVInt(byteWriterShim{w: &buf}, 2); err != nil {
		t.Fatal(err)
	}
	for _, region := range []BlockRegion{
		{Offset: 10, CompressedSize: 5, RawSize: 20},
		{Offset: 30, CompressedSize: 7, RawSize: 25},
	} {
		if err := WriteString(&buf, metaNamePrefix+"duplicate"); err != nil {
			t.Fatal(err)
		}
		if err := WriteString(&buf, "gz"); err != nil {
			t.Fatal(err)
		}
		if err := WriteBlockRegion(&buf, region); err != nil {
			t.Fatal(err)
		}
	}

	if _, err := ReadMetaIndex(&buf); err == nil ||
		!strings.Contains(err.Error(), "duplicate MetaIndex entry") {
		t.Fatalf("ReadMetaIndex() = %v, want duplicate-name error", err)
	}
}

func TestDataIndexRoundtrip(t *testing.T) {
	want := &DataIndex{
		DefaultCompression: "gz",
		Blocks: []BlockRegion{
			{Offset: 100, CompressedSize: 50, RawSize: 200},
			{Offset: 200, CompressedSize: 80, RawSize: 256},
			{Offset: 400, CompressedSize: 70, RawSize: 200},
		},
	}
	var buf bytes.Buffer
	if err := WriteDataIndex(&buf, want); err != nil {
		t.Fatal(err)
	}
	got, err := ReadDataIndex(&buf)
	if err != nil {
		t.Fatal(err)
	}
	if got.DefaultCompression != want.DefaultCompression {
		t.Errorf("DefaultCompression = %q, want %q", got.DefaultCompression, want.DefaultCompression)
	}
	if len(got.Blocks) != len(want.Blocks) {
		t.Fatalf("Blocks len = %d, want %d", len(got.Blocks), len(want.Blocks))
	}
	for i, w := range want.Blocks {
		if got.Blocks[i] != w {
			t.Errorf("block %d: got %+v, want %+v", i, got.Blocks[i], w)
		}
	}
}

func TestDataIndexEmpty(t *testing.T) {
	want := &DataIndex{DefaultCompression: "none", Blocks: nil}
	var buf bytes.Buffer
	if err := WriteDataIndex(&buf, want); err != nil {
		t.Fatal(err)
	}
	got, err := ReadDataIndex(&buf)
	if err != nil {
		t.Fatal(err)
	}
	if got.DefaultCompression != "none" || len(got.Blocks) != 0 {
		t.Errorf("empty roundtrip: got %+v", got)
	}
}

func TestDataIndexRejectsExcessiveBlockCount(t *testing.T) {
	var buf bytes.Buffer
	if err := WriteString(&buf, "none"); err != nil {
		t.Fatal(err)
	}
	if _, err := WriteVInt(byteWriterShim{w: &buf}, maxDataIndexBlocks+1); err != nil {
		t.Fatal(err)
	}

	_, err := ReadDataIndex(&buf)
	if err == nil {
		t.Fatal("ReadDataIndex accepted excessive block count")
	}
}
