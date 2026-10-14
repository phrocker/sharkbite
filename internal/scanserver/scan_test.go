package scanserver

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/phrocker/shoal-oss/internal/cache"
	"github.com/phrocker/shoal-oss/internal/metadata"
	"github.com/phrocker/shoal-oss/internal/rfile"
	"github.com/phrocker/shoal-oss/internal/rfile/bcfile/block"
	"github.com/phrocker/shoal-oss/internal/rfile/wire"
	"github.com/phrocker/shoal-oss/internal/storage/memory"
	"github.com/phrocker/shoal-oss/internal/thrift/gen/data"
	"github.com/phrocker/shoal-oss/internal/thrift/gen/tabletserver"
	"github.com/phrocker/shoal-oss/internal/tserverprocess"
)

// stubLocator returns a fixed tablet list for one table. Satisfies
// cache.TableLocator.
type stubLocator struct {
	tablets map[string][]metadata.TabletInfo
}

func (s *stubLocator) LocateTable(_ context.Context, tableID string) ([]metadata.TabletInfo, error) {
	if t, ok := s.tablets[tableID]; ok {
		return t, nil
	}
	return nil, fmt.Errorf("stubLocator: unknown table %q", tableID)
}

// writeRFileToMemory writes cells through the rfile.Writer into a
// memory backend at the given path. Returns nothing — caller fetches
// via the same path.
func writeRFileToMemory(t *testing.T, mem *memory.Backend, path string, cells []cellSpec) {
	t.Helper()
	var buf bytes.Buffer
	w, err := rfile.NewWriter(&buf, rfile.WriterOptions{Codec: block.CodecGzip})
	if err != nil {
		t.Fatal(err)
	}
	for i, c := range cells {
		k := &wire.Key{
			Row:              []byte(c.row),
			ColumnFamily:     []byte(c.cf),
			ColumnQualifier:  []byte(c.cq),
			ColumnVisibility: []byte(c.cv),
			Timestamp:        c.ts,
			Deleted:          c.deleted,
		}
		if err := w.Append(k, []byte(c.value)); err != nil {
			t.Fatalf("append cell %d: %v", i, err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	mem.Put(path, buf.Bytes())
}

type cellSpec struct {
	row, cf, cq, cv, value string
	ts                     int64
	deleted                bool
}

// TestStartScan_SingleFileFullRange writes one RFile, scans the whole
// range, and verifies all cells come back in order with correct values.
func TestStartScan_SingleFileFullRange(t *testing.T) {
	mem := memory.New()
	const path = "gs://test-bucket/tables/1/t-aaa/A0000.rf"

	cells := []cellSpec{
		{row: "r01", cf: "cf", cq: "cq", value: "v01", ts: 1},
		{row: "r02", cf: "cf", cq: "cq", value: "v02", ts: 2},
		{row: "r03", cf: "cf", cq: "cq", value: "v03", ts: 3},
	}
	writeRFileToMemory(t, mem, path, cells)

	loc := &stubLocator{
		tablets: map[string][]metadata.TabletInfo{
			"1": {
				{TableID: "1", EndRow: nil, PrevRow: nil, Files: []metadata.FileEntry{{Path: path}}},
			},
		},
	}
	srv, err := NewServer(Options{
		Locator:    loc,
		BlockCache: cache.NewBlockCache(1 << 20),
		Storage:    mem,
	})
	if err != nil {
		t.Fatal(err)
	}

	resp, err := srv.StartScan(context.Background(), nil, nil,
		&data.TKeyExtent{Table: []byte("1")},
		&data.TRange{InfiniteStartKey: true, InfiniteStopKey: true},
		nil, 0, nil, nil, nil, false, false, 0, nil, 0, "", nil, 0,
	)
	if err != nil {
		t.Fatal(err)
	}
	if resp.Result_.More {
		t.Errorf("More should be false in single-shot V0")
	}
	if got := len(resp.Result_.Results); got != 3 {
		t.Fatalf("got %d cells; want 3", got)
	}
	for i, kv := range resp.Result_.Results {
		wantRow := cells[i].row
		if string(kv.Key.Row) != wantRow {
			t.Errorf("cell %d row=%q; want %q", i, kv.Key.Row, wantRow)
		}
		if string(kv.Value) != cells[i].value {
			t.Errorf("cell %d value=%q; want %q", i, kv.Value, cells[i].value)
		}
	}
}

func TestStartScan_FiltersRequestedColumns(t *testing.T) {
	mem := memory.New()
	const filePath = "gs://test-bucket/tables/1/t-aaa/A0000.rf"
	writeRFileToMemory(t, mem, filePath, []cellSpec{
		{row: "r01", cf: "file", cq: "a", value: "file", ts: 1},
		{row: "r01", cf: "loc", cq: "session", value: "host:9997", ts: 1},
		{row: "r01", cf: "srv", cq: "flush", value: "7", ts: 1},
		{row: "r01", cf: "srv", cq: "lock", value: "lock", ts: 1},
	})
	srv, err := NewServer(Options{
		Locator: &stubLocator{tablets: map[string][]metadata.TabletInfo{
			"1": {{TableID: "1", Files: []metadata.FileEntry{{Path: filePath}}}},
		}},
		Storage: mem,
	})
	if err != nil {
		t.Fatal(err)
	}

	resp, err := srv.StartScan(context.Background(), nil, nil,
		&data.TKeyExtent{Table: []byte("1")},
		&data.TRange{InfiniteStartKey: true, InfiniteStopKey: true},
		[]*data.TColumn{
			{ColumnFamily: []byte("loc")},
			{ColumnFamily: []byte("srv"), ColumnQualifier: []byte("flush")},
		},
		0, nil, nil, nil, false, false, 0, nil, 0, "", nil, 0,
	)
	if err != nil {
		t.Fatal(err)
	}
	if got := resp.Result_.Results; len(got) != 2 ||
		string(got[0].Key.ColFamily) != "loc" ||
		string(got[1].Key.ColFamily) != "srv" ||
		string(got[1].Key.ColQualifier) != "flush" {
		t.Fatalf("filtered results = %#v", got)
	}
}

// TestStartScan_MultiFileMergeAndDedupe: two RFiles in one tablet; the
// second has an updated version (higher ts) of one cell. Scan should
// emit each (row,cf,cq,cv) once, with the latest version.
func TestStartScan_MultiFileMergeAndDedupe(t *testing.T) {
	mem := memory.New()
	pathA := "gs://test/a.rf"
	pathB := "gs://test/b.rf"
	// File A: r01 v=old ts=1, r02 v=A2 ts=1
	writeRFileToMemory(t, mem, pathA, []cellSpec{
		{row: "r01", cf: "cf", cq: "cq", value: "old01", ts: 1},
		{row: "r02", cf: "cf", cq: "cq", value: "A2", ts: 1},
	})
	// File B: r01 v=new ts=5  (later compaction), r03 v=B3 ts=2
	writeRFileToMemory(t, mem, pathB, []cellSpec{
		{row: "r01", cf: "cf", cq: "cq", value: "new01", ts: 5},
		{row: "r03", cf: "cf", cq: "cq", value: "B3", ts: 2},
	})

	loc := &stubLocator{
		tablets: map[string][]metadata.TabletInfo{
			"1": {
				{TableID: "1", Files: []metadata.FileEntry{{Path: pathA}, {Path: pathB}}},
			},
		},
	}
	srv, _ := NewServer(Options{Locator: loc, Storage: mem})

	resp, err := srv.StartScan(context.Background(), nil, nil,
		&data.TKeyExtent{Table: []byte("1")},
		&data.TRange{InfiniteStartKey: true, InfiniteStopKey: true},
		nil, 0, nil, nil, nil, false, false, 0, nil, 0, "", nil, 0,
	)
	if err != nil {
		t.Fatal(err)
	}
	if got := len(resp.Result_.Results); got != 3 {
		t.Fatalf("got %d cells; want 3 (deduped)", got)
	}
	want := map[string]string{"r01": "new01", "r02": "A2", "r03": "B3"}
	for _, kv := range resp.Result_.Results {
		row := string(kv.Key.Row)
		if got := string(kv.Value); got != want[row] {
			t.Errorf("row=%s value=%q want %q (latest version)", row, got, want[row])
		}
	}
}

// TestStartScan_VisibilityFilter: cells under different CV labels;
// scanner with auths={A} sees only r01 (A) + r03 (empty); rejects r02 (B).
func TestStartScan_VisibilityFilter(t *testing.T) {
	mem := memory.New()
	const path = "gs://test/v.rf"
	writeRFileToMemory(t, mem, path, []cellSpec{
		{row: "r01", cf: "cf", cq: "cq", cv: "A", value: "v01", ts: 1},
		{row: "r02", cf: "cf", cq: "cq", cv: "B", value: "v02", ts: 2},
		{row: "r03", cf: "cf", cq: "cq", cv: "", value: "v03", ts: 3}, // empty CV always visible
	})

	loc := &stubLocator{
		tablets: map[string][]metadata.TabletInfo{
			"1": {{TableID: "1", Files: []metadata.FileEntry{{Path: path}}}},
		},
	}
	srv, _ := NewServer(Options{Locator: loc, Storage: mem})

	resp, err := srv.StartScan(context.Background(), nil, nil,
		&data.TKeyExtent{Table: []byte("1")},
		&data.TRange{InfiniteStartKey: true, InfiniteStopKey: true},
		nil, 0, nil, nil,
		[][]byte{[]byte("A")}, // auths
		false, false, 0, nil, 0, "", nil, 0,
	)
	if err != nil {
		t.Fatal(err)
	}
	rows := []string{}
	for _, kv := range resp.Result_.Results {
		rows = append(rows, string(kv.Key.Row))
	}
	wantRows := []string{"r01", "r03"}
	if len(rows) != len(wantRows) {
		t.Fatalf("got rows %v; want %v", rows, wantRows)
	}
	for i := range rows {
		if rows[i] != wantRows[i] {
			t.Errorf("row %d = %q; want %q", i, rows[i], wantRows[i])
		}
	}
}

// TestStartScan_BoundedRange: scan a sub-range, confirm only matching
// cells come back.
func TestStartScan_BoundedRange(t *testing.T) {
	mem := memory.New()
	const path = "gs://test/r.rf"
	cells := []cellSpec{}
	for i := 0; i < 20; i++ {
		cells = append(cells, cellSpec{
			row: fmt.Sprintf("row%02d", i), cf: "cf", cq: "cq",
			value: fmt.Sprintf("v%02d", i), ts: int64(i + 1),
		})
	}
	writeRFileToMemory(t, mem, path, cells)
	loc := &stubLocator{
		tablets: map[string][]metadata.TabletInfo{
			"1": {{TableID: "1", Files: []metadata.FileEntry{{Path: path}}}},
		},
	}
	srv, _ := NewServer(Options{Locator: loc, Storage: mem})

	// Scan [row05, row10) — non-inclusive stop.
	resp, err := srv.StartScan(context.Background(), nil, nil,
		&data.TKeyExtent{Table: []byte("1")},
		&data.TRange{
			Start:             &data.TKey{Row: []byte("row05"), ColFamily: nil, ColQualifier: nil, ColVisibility: nil, Timestamp: 1<<63 - 1},
			Stop:              &data.TKey{Row: []byte("row10"), ColFamily: nil, ColQualifier: nil, ColVisibility: nil, Timestamp: 1<<63 - 1},
			StartKeyInclusive: true,
			StopKeyInclusive:  false,
		},
		nil, 0, nil, nil, nil, false, false, 0, nil, 0, "", nil, 0,
	)
	if err != nil {
		t.Fatal(err)
	}
	if got := len(resp.Result_.Results); got != 5 {
		t.Errorf("got %d cells; want 5 (row05..row09)", got)
	}
	if len(resp.Result_.Results) > 0 {
		first := string(resp.Result_.Results[0].Key.Row)
		last := string(resp.Result_.Results[len(resp.Result_.Results)-1].Key.Row)
		if first != "row05" || last != "row09" {
			t.Errorf("range got [%s, %s]; want [row05, row09]", first, last)
		}
	}
}

// TestStartScan_DeletionSwallowsCoord: a tombstone for r02 in file B
// (ts=10) should suppress r02's older live version in file A (ts=1).
func TestStartScan_DeletionSwallowsCoord(t *testing.T) {
	mem := memory.New()
	pathA := "gs://test/del-a.rf"
	pathB := "gs://test/del-b.rf"
	writeRFileToMemory(t, mem, pathA, []cellSpec{
		{row: "r01", cf: "cf", cq: "cq", value: "v01", ts: 1},
		{row: "r02", cf: "cf", cq: "cq", value: "v02-old", ts: 1},
		{row: "r03", cf: "cf", cq: "cq", value: "v03", ts: 1},
	})
	writeRFileToMemory(t, mem, pathB, []cellSpec{
		{row: "r02", cf: "cf", cq: "cq", value: "", ts: 10, deleted: true},
	})

	loc := &stubLocator{
		tablets: map[string][]metadata.TabletInfo{
			"1": {{TableID: "1", Files: []metadata.FileEntry{{Path: pathA}, {Path: pathB}}}},
		},
	}
	srv, _ := NewServer(Options{Locator: loc, Storage: mem})

	resp, err := srv.StartScan(context.Background(), nil, nil,
		&data.TKeyExtent{Table: []byte("1")},
		&data.TRange{InfiniteStartKey: true, InfiniteStopKey: true},
		nil, 0, nil, nil, nil, false, false, 0, nil, 0, "", nil, 0,
	)
	if err != nil {
		t.Fatal(err)
	}
	rows := []string{}
	for _, kv := range resp.Result_.Results {
		rows = append(rows, string(kv.Key.Row))
	}
	wantRows := []string{"r01", "r03"} // r02 should be tombstoned out
	if len(rows) != len(wantRows) {
		t.Fatalf("got rows %v; want %v (r02 deletion should suppress)", rows, wantRows)
	}
}

// TestStartScan_NoMatchingTablet returns the retryable exception Accumulo
// expects when the extent is not hosted by this server.
func TestStartScan_NoMatchingTablet(t *testing.T) {
	loc := &stubLocator{tablets: map[string][]metadata.TabletInfo{
		"1": {{TableID: "1", EndRow: []byte("k"), PrevRow: nil}},
	}}
	srv, _ := NewServer(Options{Locator: loc, Storage: memory.New()})

	_, err := srv.StartScan(context.Background(), nil, nil,
		&data.TKeyExtent{Table: []byte("1"), EndRow: []byte("z"), PrevEndRow: nil},
		&data.TRange{InfiniteStartKey: true, InfiniteStopKey: true},
		nil, 0, nil, nil, nil, false, false, 0, nil, 0, "", nil, 0,
	)
	var notServing *tabletserver.NotServingTabletException
	if !errors.As(err, &notServing) {
		t.Fatalf("StartScan error = %v, want NotServingTabletException", err)
	}
}

type errorLocator struct {
	err error
}

func (l errorLocator) LocateTable(context.Context, string) ([]metadata.TabletInfo, error) {
	return nil, l.err
}

func TestStartScan_NotHostedReturnsNotServingTablet(t *testing.T) {
	srv, _ := NewServer(Options{
		Locator: errorLocator{err: tserverprocess.ErrNotHosted},
		Storage: memory.New(),
	})
	extent := &data.TKeyExtent{Table: []byte("1")}

	_, err := srv.StartScan(context.Background(), nil, nil, extent,
		&data.TRange{InfiniteStartKey: true, InfiniteStopKey: true},
		nil, 0, nil, nil, nil, false, false, 0, nil, 0, "", nil, 0,
	)
	var notServing *tabletserver.NotServingTabletException
	if !errors.As(err, &notServing) {
		t.Fatalf("StartScan error = %v, want NotServingTabletException", err)
	}
	if notServing.Extent != extent {
		t.Fatalf("exception extent = %#v, want %#v", notServing.Extent, extent)
	}
}
