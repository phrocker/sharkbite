package parquetfile

import (
	"bytes"
	"testing"

	"github.com/phrocker/shoal-oss/internal/iterrt"
	"github.com/phrocker/shoal-oss/internal/rfile/wire"
)

func TestEncodeDecodePreservesAccumuloKey(t *testing.T) {
	cells := []iterrt.Cell{
		{
			Key: &wire.Key{
				Row:              []byte{0x00, 0xff},
				ColumnFamily:     []byte("cf"),
				ColumnQualifier:  []byte("cq"),
				ColumnVisibility: []byte("private"),
				Timestamp:        42,
				Deleted:          true,
			},
			Value: []byte{0xfe, 0x00},
		},
	}
	src := iterrt.NewSliceSource(cells)
	if err := src.Init(nil, nil, iterrt.IteratorEnvironment{}); err != nil {
		t.Fatal(err)
	}
	if err := src.Seek(iterrt.InfiniteRange(), nil, false); err != nil {
		t.Fatal(err)
	}

	data, count, err := Encode(src)
	if err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("count = %d, want 1", count)
	}
	got, err := Decode(data)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || !got[0].Key.Equal(cells[0].Key) || !bytes.Equal(got[0].Value, cells[0].Value) {
		t.Fatalf("round trip = %+v", got)
	}
}
