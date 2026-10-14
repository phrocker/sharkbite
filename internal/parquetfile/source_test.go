package parquetfile

import (
	"bytes"
	"context"
	"fmt"
	"testing"

	"github.com/phrocker/shoal-oss/internal/iterrt"
	"github.com/phrocker/shoal-oss/internal/rfile/wire"
	"github.com/phrocker/shoal-oss/internal/storage"
	"github.com/phrocker/shoal-oss/internal/storage/memory"
)

func TestSourcePrunesRowGroupsForPointSeek(t *testing.T) {
	cells := make([]iterrt.Cell, 20)
	for i := range cells {
		row := []byte(fmt.Sprintf("row-%02d", i))
		cells[i] = iterrt.Cell{
			Key:   &wire.Key{Row: row, ColumnFamily: []byte("cf"), Timestamp: 1},
			Value: []byte(fmt.Sprintf("value-%02d", i)),
		}
	}
	iter := iterrt.NewSliceSource(cells)
	if err := iter.Init(nil, nil, iterrt.IteratorEnvironment{}); err != nil {
		t.Fatal(err)
	}
	if err := iter.Seek(iterrt.InfiniteRange(), nil, false); err != nil {
		t.Fatal(err)
	}
	data, _, err := EncodeWithOptions(iter, EncodeOptions{RowsPerRowGroup: 4})
	if err != nil {
		t.Fatal(err)
	}

	backend := memory.New()
	backend.Put("table.parquet", data)
	open := func() (file storage.File, err error) {
		return backend.Open(context.Background(), "table.parquet")
	}
	file, err := open()
	if err != nil {
		t.Fatal(err)
	}
	src, err := NewSource(file, open)
	if err != nil {
		t.Fatal(err)
	}
	defer src.Close()
	if err := src.Init(nil, nil, iterrt.IteratorEnvironment{}); err != nil {
		t.Fatal(err)
	}

	row := []byte("row-13")
	end := append(append([]byte(nil), row...), 0)
	rng := iterrt.Range{
		Start:          &wire.Key{Row: row},
		StartInclusive: true,
		End:            &wire.Key{Row: end},
		EndInclusive:   false,
	}
	if err := src.Seek(rng, nil, false); err != nil {
		t.Fatal(err)
	}
	if !src.HasTop() || string(src.GetTopKey().Row) != "row-13" || string(src.GetTopValue()) != "value-13" {
		t.Fatalf("unexpected top: key=%v value=%q", src.GetTopKey(), src.GetTopValue())
	}
	if err := src.Next(); err != nil {
		t.Fatal(err)
	}
	if src.HasTop() {
		t.Fatalf("point seek returned extra row %q", src.GetTopKey().Row)
	}
	stats := src.Stats()
	if stats.RowGroupsTotal != 5 || stats.RowGroupsRead != 1 || stats.RowGroupsPruned < 4 {
		t.Fatalf("unexpected pruning stats: %+v", stats)
	}
	if stats.RowsDecoded > 4 {
		t.Fatalf("decoded %d rows for a four-row group", stats.RowsDecoded)
	}
}

func TestSourceUsesPageIndexWithinRowGroup(t *testing.T) {
	cells := make([]iterrt.Cell, 100)
	for i := range cells {
		row := []byte(fmt.Sprintf("row-%03d-%0120d", i, i))
		cells[i] = iterrt.Cell{
			Key:   &wire.Key{Row: row, ColumnFamily: []byte("cf"), Timestamp: 1},
			Value: []byte("value"),
		}

	}
	iter := iterrt.NewSliceSource(cells)
	if err := iter.Init(nil, nil, iterrt.IteratorEnvironment{}); err != nil {
		t.Fatal(err)
	}
	if err := iter.Seek(iterrt.InfiniteRange(), nil, false); err != nil {
		t.Fatal(err)
	}
	data, _, err := EncodeWithOptions(iter, EncodeOptions{
		RowsPerRowGroup: 100,
		PageBufferSize:  256,
	})
	if err != nil {
		t.Fatal(err)
	}

	backend := memory.New()
	backend.Put("pages.parquet", data)
	open := func() (storage.File, error) {
		return backend.Open(context.Background(), "pages.parquet")
	}
	file, err := open()
	if err != nil {
		t.Fatal(err)
	}
	src, err := NewSource(file, open)
	if err != nil {
		t.Fatal(err)
	}
	defer src.Close()

	row := cells[90].Key.Row
	end := append(append([]byte(nil), row...), 0)
	if err := src.Seek(iterrt.Range{
		Start:          &wire.Key{Row: row},
		StartInclusive: true,
		End:            &wire.Key{Row: end},
		EndInclusive:   false,
	}, nil, false); err != nil {
		t.Fatal(err)
	}
	if !src.HasTop() || !bytes.Equal(src.GetTopKey().Row, row) {
		t.Fatalf("point seek returned %v", src.GetTopKey())
	}
	stats := src.Stats()
	if stats.RowsSkippedPageIndex == 0 {
		t.Fatalf("page index did not skip rows: %+v", stats)
	}
	if stats.RowsDecoded >= 90 {
		t.Fatalf("decoded too many rows after page seek: %+v", stats)
	}
}

func BenchmarkSourcePruning(b *testing.B) {
	cells := make([]iterrt.Cell, 8192)
	for i := range cells {
		row := []byte(fmt.Sprintf("row-%05d-%080d", i, i))
		cells[i] = iterrt.Cell{
			Key:   &wire.Key{Row: row, ColumnFamily: []byte("cf"), Timestamp: 1},
			Value: []byte("value"),
		}
	}
	iter := iterrt.NewSliceSource(cells)
	if err := iter.Init(nil, nil, iterrt.IteratorEnvironment{}); err != nil {
		b.Fatal(err)
	}
	if err := iter.Seek(iterrt.InfiniteRange(), nil, false); err != nil {
		b.Fatal(err)
	}
	data, _, err := EncodeWithOptions(iter, EncodeOptions{
		RowsPerRowGroup: 256,
		PageBufferSize:  1024,
	})
	if err != nil {
		b.Fatal(err)
	}
	backend := memory.New()
	backend.Put("bench.parquet", data)
	open := func() (storage.File, error) {
		return backend.Open(context.Background(), "bench.parquet")
	}
	file, err := open()
	if err != nil {
		b.Fatal(err)
	}
	src, err := NewSource(file, open)
	if err != nil {
		b.Fatal(err)
	}
	defer src.Close()

	b.Run("point-row-group-bloom-page-index", func(b *testing.B) {
		row := cells[len(cells)-17].Key.Row
		end := append(append([]byte(nil), row...), 0)
		rng := iterrt.Range{
			Start:          &wire.Key{Row: row},
			StartInclusive: true,
			End:            &wire.Key{Row: end},
			EndInclusive:   false,
		}
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			if err := src.Seek(rng, nil, false); err != nil {
				b.Fatal(err)
			}
			for src.HasTop() {
				if err := src.Next(); err != nil {
					b.Fatal(err)
				}
			}
		}
	})
	b.Run("full-scan", func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			if err := src.Seek(iterrt.InfiniteRange(), nil, false); err != nil {
				b.Fatal(err)
			}
			for src.HasTop() {
				if err := src.Next(); err != nil {
					b.Fatal(err)
				}
			}
		}
	})
}
