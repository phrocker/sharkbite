package scanserver

import (
	"context"
	"fmt"
	"sort"
	"testing"

	"github.com/phrocker/shoal-oss/internal/cache"
	"github.com/phrocker/shoal-oss/internal/metadata"
	"github.com/phrocker/shoal-oss/internal/storage/memory"
	"github.com/phrocker/shoal-oss/internal/thrift/gen/data"
)

// rowRange builds a TRange covering [startRow, stopRow) — start
// inclusive, stop exclusive — at maximum timestamp so the range
// pre-filter doesn't strip any version. Mirrors the way Accumulo's
// Range#exact and the BatchScanner construct row ranges.
func rowRange(startRow, stopRow string) *data.TRange {
	r := &data.TRange{
		StartKeyInclusive: true,
		StopKeyInclusive:  false,
	}
	if startRow != "" {
		r.Start = &data.TKey{Row: []byte(startRow), Timestamp: 1<<63 - 1}
	} else {
		r.InfiniteStartKey = true
	}
	if stopRow != "" {
		r.Stop = &data.TKey{Row: []byte(stopRow), Timestamp: 1<<63 - 1}
	} else {
		r.InfiniteStopKey = true
	}
	return r
}

// exactRow scans exactly one row by using row + row+0x00 as the half-
// open upper bound (the same followingPrefix construction the Java
// SDK uses for Scanner.setRange(new Range(row))).
func exactRow(row string) *data.TRange {
	return &data.TRange{
		Start:             &data.TKey{Row: []byte(row), Timestamp: 1<<63 - 1},
		Stop:              &data.TKey{Row: append([]byte(row), 0x00), Timestamp: 1<<63 - 1},
		StartKeyInclusive: true,
		StopKeyInclusive:  false,
	}
}

// TestStartMultiScan_SingleTabletMultipleRanges issues a multi-scan
// against one tablet with three disjoint ranges. Verifies the result
// contains exactly the cells in those ranges, in sorted order, and
// that More=false (everything fit in budget).
func TestStartMultiScan_SingleTabletMultipleRanges(t *testing.T) {
	mem := memory.New()
	const path = "gs://test/multi-single.rf"
	cells := []cellSpec{}
	for i := 0; i < 30; i++ {
		cells = append(cells, cellSpec{
			row: fmt.Sprintf("row%02d", i),
			cf:  "cf", cq: "cq",
			value: fmt.Sprintf("v%02d", i),
			ts:    int64(i + 1),
		})
	}
	writeRFileToMemory(t, mem, path, cells)

	loc := &stubLocator{
		tablets: map[string][]metadata.TabletInfo{
			"1": {{TableID: "1", Files: []metadata.FileEntry{{Path: path}}}},
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

	extent := &data.TKeyExtent{Table: []byte("1")}
	batch := data.ScanBatch{
		extent: []*data.TRange{
			rowRange("row01", "row04"), // r01..r03
			rowRange("row10", "row12"), // r10..r11
			rowRange("row20", "row23"), // r20..r22
		},
	}
	resp, err := srv.StartMultiScan(context.Background(), nil, nil, batch,
		nil, nil, nil, nil, false, nil, 0, "", nil, 0,
	)
	if err != nil {
		t.Fatal(err)
	}
	if resp.Result_.More {
		t.Errorf("More should be false; everything fit")
	}
	want := []string{"row01", "row02", "row03", "row10", "row11", "row20", "row21", "row22"}
	got := rowsOf(resp.Result_.Results)
	if !equalStrings(got, want) {
		t.Errorf("rows = %v\nwant   %v", got, want)
	}
}

// TestStartMultiScan_TwoTablets exercises the multi-tablet fan-out
// path: tablet A holds rows row00..row09, tablet B holds row10..row19.
// A single ScanBatch with one range per tablet should hit both files
// and concatenate results in arrival order (per-tablet sorted; the V0.5
// engine doesn't globally re-sort across tablets, but each tablet's
// own range list is monotonic).
func TestStartMultiScan_TwoTablets(t *testing.T) {
	mem := memory.New()
	pathA := "gs://test/multi-A.rf"
	pathB := "gs://test/multi-B.rf"
	cellsA := []cellSpec{}
	for i := 0; i < 10; i++ {
		cellsA = append(cellsA, cellSpec{
			row: fmt.Sprintf("row%02d", i), cf: "cf", cq: "cq",
			value: fmt.Sprintf("a%02d", i), ts: int64(i + 1),
		})
	}
	cellsB := []cellSpec{}
	for i := 10; i < 20; i++ {
		cellsB = append(cellsB, cellSpec{
			row: fmt.Sprintf("row%02d", i), cf: "cf", cq: "cq",
			value: fmt.Sprintf("b%02d", i), ts: int64(i + 1),
		})
	}
	writeRFileToMemory(t, mem, pathA, cellsA)
	writeRFileToMemory(t, mem, pathB, cellsB)

	// Tablet A covers (-inf, row09], tablet B covers (row09, +inf).
	loc := &stubLocator{
		tablets: map[string][]metadata.TabletInfo{
			"1": {
				{TableID: "1", PrevRow: nil, EndRow: []byte("row09"),
					Files: []metadata.FileEntry{{Path: pathA}}},
				{TableID: "1", PrevRow: []byte("row09"), EndRow: nil,
					Files: []metadata.FileEntry{{Path: pathB}}},
			},
		},
	}
	srv, _ := NewServer(Options{Locator: loc, Storage: mem})

	extentA := &data.TKeyExtent{Table: []byte("1"), EndRow: []byte("row09")}
	extentB := &data.TKeyExtent{Table: []byte("1"), PrevEndRow: []byte("row09")}
	batch := data.ScanBatch{
		extentA: []*data.TRange{rowRange("row02", "row05")}, // 02..04
		extentB: []*data.TRange{rowRange("row12", "row15")}, // 12..14
	}
	resp, err := srv.StartMultiScan(context.Background(), nil, nil, batch,
		nil, nil, nil, nil, false, nil, 0, "", nil, 0,
	)
	if err != nil {
		t.Fatal(err)
	}
	got := rowsOf(resp.Result_.Results)
	// Per-tablet iteration order is hash-map-arbitrary; sort and compare
	// against the multi-set of expected rows.
	sort.Strings(got)
	want := []string{"row02", "row03", "row04", "row12", "row13", "row14"}
	if !equalStrings(got, want) {
		t.Errorf("rows = %v\nwant   %v", got, want)
	}
}

// TestStartMultiScan_OverlappingRangesDedup: two ranges that share
// rows should still emit each row exactly once. Confirms normalizeRanges
// merges overlapping ranges before the heap-merge pass.
func TestStartMultiScan_OverlappingRangesDedup(t *testing.T) {
	mem := memory.New()
	const path = "gs://test/multi-overlap.rf"
	cells := []cellSpec{}
	for i := 0; i < 10; i++ {
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

	extent := &data.TKeyExtent{Table: []byte("1")}
	batch := data.ScanBatch{
		extent: []*data.TRange{
			rowRange("row02", "row06"), // 02..05
			rowRange("row04", "row08"), // 04..07 (overlaps 04..05)
		},
	}
	resp, err := srv.StartMultiScan(context.Background(), nil, nil, batch,
		nil, nil, nil, nil, false, nil, 0, "", nil, 0,
	)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"row02", "row03", "row04", "row05", "row06", "row07"}
	got := rowsOf(resp.Result_.Results)
	if !equalStrings(got, want) {
		t.Errorf("rows = %v\nwant   %v", got, want)
	}
}

// TestStartMultiScan_EmptyBatchReturnsEmpty: empty input → empty result
// with More=false, no error.
func TestStartMultiScan_EmptyBatch(t *testing.T) {
	srv, _ := NewServer(Options{Locator: &stubLocator{}, Storage: memory.New()})
	resp, err := srv.StartMultiScan(context.Background(), nil, nil, nil,
		nil, nil, nil, nil, false, nil, 0, "", nil, 0,
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.Result_.Results) != 0 || resp.Result_.More {
		t.Errorf("empty batch should be 0 results, More=false; got %d, %v",
			len(resp.Result_.Results), resp.Result_.More)
	}
}

// TestStartMultiScan_ExactRowsAcrossTablet: BatchScanner-shaped query
// where the caller wants several individual rows. Confirms the exact-
// row range form (row, row+0x00) flowing through normalize + heap-merge
// returns each row's cells.
func TestStartMultiScan_ExactRowsAcrossTablet(t *testing.T) {
	mem := memory.New()
	const path = "gs://test/multi-exact.rf"
	cells := []cellSpec{
		{row: "alpha", cf: "V", cq: "name", value: "Alpha", ts: 1},
		{row: "alpha", cf: "V", cq: "type", value: "Concept", ts: 1},
		{row: "beta", cf: "V", cq: "name", value: "Beta", ts: 2},
		{row: "gamma", cf: "V", cq: "name", value: "Gamma", ts: 3},
		{row: "delta", cf: "V", cq: "name", value: "Delta", ts: 4},
	}
	// rfile.Writer requires sorted input; re-sort by (row, cq).
	sort.SliceStable(cells, func(i, j int) bool {
		if cells[i].row != cells[j].row {
			return cells[i].row < cells[j].row
		}
		return cells[i].cq < cells[j].cq
	})
	writeRFileToMemory(t, mem, path, cells)
	loc := &stubLocator{
		tablets: map[string][]metadata.TabletInfo{
			"7": {{TableID: "7", Files: []metadata.FileEntry{{Path: path}}}},
		},
	}
	srv, _ := NewServer(Options{Locator: loc, Storage: mem})

	extent := &data.TKeyExtent{Table: []byte("7")}
	batch := data.ScanBatch{
		extent: []*data.TRange{
			exactRow("alpha"),
			exactRow("gamma"),
		},
	}
	resp, err := srv.StartMultiScan(context.Background(), nil, nil, batch,
		nil, nil, nil, nil, false, nil, 0, "", nil, 0,
	)
	if err != nil {
		t.Fatal(err)
	}
	got := rowsOf(resp.Result_.Results)
	want := []string{"alpha", "alpha", "gamma"} // 2 cells for alpha, 1 for gamma
	if !equalStrings(got, want) {
		t.Errorf("rows = %v\nwant   %v", got, want)
	}
}

// TestNormalizeRanges_MergesAndSorts unit-tests the helper directly.
func TestNormalizeRanges_MergesAndSorts(t *testing.T) {
	rs := []*data.TRange{
		rowRange("row20", "row25"),
		rowRange("row01", "row05"),
		rowRange("row03", "row07"), // overlaps row01..05 → merge to row01..07
		rowRange("row22", "row28"), // overlaps row20..25 → merge to row20..28
	}
	out := normalizeRanges(rs)
	if len(out) != 2 {
		t.Fatalf("expected 2 merged ranges, got %d", len(out))
	}
	if string(out[0].Start.Row) != "row01" || string(out[0].Stop.Row) != "row07" {
		t.Errorf("merged[0] = (%s, %s), want (row01, row07)",
			out[0].Start.Row, out[0].Stop.Row)
	}
	if string(out[1].Start.Row) != "row20" || string(out[1].Stop.Row) != "row28" {
		t.Errorf("merged[1] = (%s, %s), want (row20, row28)",
			out[1].Start.Row, out[1].Stop.Row)
	}
}

// TestStartMultiScan_AutoBinAcrossTablets: caller hands in a single
// table-id-only TKeyExtent with ranges spanning two tablets. shoal's
// auto-binner should fan out the ranges per-tablet without the caller
// pre-resolving anything. This is the V1 "Java SDK skips client-side
// binning" path.
func TestStartMultiScan_AutoBinAcrossTablets(t *testing.T) {
	mem := memory.New()
	pathA := "gs://test/autobin-A.rf"
	pathB := "gs://test/autobin-B.rf"
	cellsA := []cellSpec{}
	for i := 0; i < 10; i++ {
		cellsA = append(cellsA, cellSpec{
			row: fmt.Sprintf("row%02d", i), cf: "V", cq: "name",
			value: fmt.Sprintf("a%02d", i), ts: int64(i + 1),
		})
	}
	cellsB := []cellSpec{}
	for i := 10; i < 20; i++ {
		cellsB = append(cellsB, cellSpec{
			row: fmt.Sprintf("row%02d", i), cf: "V", cq: "name",
			value: fmt.Sprintf("b%02d", i), ts: int64(i + 1),
		})
	}
	writeRFileToMemory(t, mem, pathA, cellsA)
	writeRFileToMemory(t, mem, pathB, cellsB)

	// Two tablets: A = (-inf, row09], B = (row09, +inf)
	loc := &stubLocator{
		tablets: map[string][]metadata.TabletInfo{
			"7": {
				{TableID: "7", PrevRow: nil, EndRow: []byte("row09"),
					Files: []metadata.FileEntry{{Path: pathA}}},
				{TableID: "7", PrevRow: []byte("row09"), EndRow: nil,
					Files: []metadata.FileEntry{{Path: pathB}}},
			},
		},
	}
	srv, _ := NewServer(Options{Locator: loc, Storage: mem})

	// Single extent, no End/Prev → triggers auto-binner. Three exact-
	// row probes spanning both tablets.
	extent := &data.TKeyExtent{Table: []byte("7")}
	batch := data.ScanBatch{
		extent: []*data.TRange{
			exactRow("row03"),
			exactRow("row14"),
			exactRow("row07"),
		},
	}
	resp, err := srv.StartMultiScan(context.Background(), nil, nil, batch,
		nil, nil, nil, nil, false, nil, 0, "", nil, 0,
	)
	if err != nil {
		t.Fatal(err)
	}
	got := rowsOf(resp.Result_.Results)
	sort.Strings(got)
	want := []string{"row03", "row07", "row14"}
	if !equalStrings(got, want) {
		t.Errorf("rows = %v\nwant   %v", got, want)
	}
}

// TestNormalizeRanges_KeepsDisjoint: non-overlapping ranges should be
// preserved (just sorted), not merged.
func TestNormalizeRanges_KeepsDisjoint(t *testing.T) {
	rs := []*data.TRange{
		rowRange("row50", "row55"),
		rowRange("row10", "row15"),
		rowRange("row30", "row35"),
	}
	out := normalizeRanges(rs)
	if len(out) != 3 {
		t.Fatalf("expected 3 disjoint ranges preserved, got %d", len(out))
	}
	starts := []string{
		string(out[0].Start.Row),
		string(out[1].Start.Row),
		string(out[2].Start.Row),
	}
	want := []string{"row10", "row30", "row50"}
	if !equalStrings(starts, want) {
		t.Errorf("starts = %v, want %v", starts, want)
	}
}

func TestClipRangeToExtentUsesExclusivePreviousAndInclusiveEndRows(t *testing.T) {
	clipped := clipRangeToExtent(
		&data.TRange{InfiniteStartKey: true, InfiniteStopKey: true},
		&data.TKeyExtent{Table: []byte("1"), PrevEndRow: []byte("m"), EndRow: []byte("t")},
	)
	if clipped == nil || string(clipped.Start.Row) != "m\x00" ||
		!clipped.StartKeyInclusive || string(clipped.Stop.Row) != "t\x00" ||
		clipped.StopKeyInclusive {
		t.Fatalf("clipped range = %#v", clipped)
	}
	if got := clipRangeToExtent(
		exactRow("a"),
		&data.TKeyExtent{Table: []byte("1"), PrevEndRow: []byte("m"), EndRow: []byte("t")},
	); got != nil {
		t.Fatalf("non-overlapping range = %#v, want nil", got)
	}
}

// rowsOf extracts row bytes from a result list.
func rowsOf(kvs []*data.TKeyValue) []string {
	out := make([]string, 0, len(kvs))
	for _, kv := range kvs {
		out = append(out, string(kv.Key.Row))
	}
	return out
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
