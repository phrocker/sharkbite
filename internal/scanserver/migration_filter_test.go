package scanserver

import (
	"testing"

	"github.com/phrocker/shoal-oss/internal/metadata"
	"github.com/phrocker/shoal-oss/internal/rfile/wire"
)

func TestHasMigrationProcessorFiltersRows(t *testing.T) {
	processor := newMetadataColumnFilter(metadata.CFServer, "migration")
	processor.offer(&wire.Key{
		Row: []byte("1;a"), ColumnFamily: []byte(metadata.CFTabletSection),
		ColumnQualifier: []byte(metadata.CQPrevRow),
	}, []byte{0})
	processor.offer(&wire.Key{
		Row: []byte("1;b"), ColumnFamily: []byte(metadata.CFServer),
		ColumnQualifier: []byte("migration"),
	}, []byte("tserver:9997[session]"))
	processor.offer(&wire.Key{
		Row: []byte("1;b"), ColumnFamily: []byte(metadata.CFTabletSection),
		ColumnQualifier: []byte(metadata.CQPrevRow),
	}, []byte{1, 'a'})

	got := processor.drain()
	if len(got) != 2 {
		t.Fatalf("migration filter returned %d cells, want 2", len(got))
	}
	for _, cell := range got {
		if string(cell.Key.Row) != "1;b" {
			t.Fatalf("migration filter returned row %q", cell.Key.Row)
		}
	}
}

func TestGcWalsFilterFiltersRows(t *testing.T) {
	processor := newGcWalsFilter("live:9997[abc],other:9997[def]")
	offer := func(row, cf, cq, value string) {
		t.Helper()
		processor.offer(&wire.Key{
			Row: []byte(row), ColumnFamily: []byte(cf), ColumnQualifier: []byte(cq),
		}, []byte(value))
	}

	offer("1;dead", metadata.CFCurrentLocation, "123", "dead:9997")
	offer("1;dead", metadata.CFTabletSection, metadata.CQPrevRow, "")
	offer("1;live", metadata.CFCurrentLocation, "abc", "live:9997")
	offer("1;live", metadata.CFTabletSection, metadata.CQPrevRow, "")
	offer("1;logs", metadata.CFCurrentLocation, "def", "other:9997")
	offer("1;logs", metadata.CFLog, "wal", "")
	offer("1;unassigned", metadata.CFTabletSection, metadata.CQPrevRow, "")
	offer("1;future-dead", metadata.CFFutureLocation, "456", "future:9997")

	got := processor.drain()
	rows := make(map[string]int)
	for _, cell := range got {
		rows[string(cell.Key.Row)]++
	}
	if len(rows) != 3 || rows["1;dead"] != 2 || rows["1;logs"] != 2 ||
		rows["1;future-dead"] != 1 {
		t.Fatalf("GC WAL filter returned rows %#v", rows)
	}
}
