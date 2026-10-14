package accumulo

import (
	"errors"
	"sort"
	"testing"
)

func TestMetadataEntryEncodesBothTabletShapes(t *testing.T) {
	if got := MetadataEntry("1", []byte("k")); got != "1;k" {
		t.Fatalf("MetadataEntry with an end row = %q", got)
	}
	if got := MetadataEntry("1", nil); got != "1<" {
		t.Fatalf("MetadataEntry with no end row = %q", got)
	}
	// A non-nil empty slice is the empty row, not an absent bound.
	if got := MetadataEntry("1", []byte{}); got != "1;" {
		t.Fatalf("MetadataEntry with an empty end row = %q", got)
	}
	extent, err := NewTabletExtent("1", []byte("k"), nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := extent.MetadataEntry(); got != "1;k" {
		t.Fatalf("extent.MetadataEntry = %q", got)
	}
}

func TestNewTabletExtentValidatesAndCopies(t *testing.T) {
	endRow := []byte("m")
	prevRow := []byte("a")
	extent, err := NewTabletExtent("1", endRow, prevRow)
	if err != nil {
		t.Fatal(err)
	}
	endRow[0] = 'X'
	prevRow[0] = 'X'
	if string(extent.EndRow) != "m" || string(extent.PrevRow) != "a" {
		t.Fatalf("extent followed the caller's slices: %+v", extent)
	}
	if _, err := NewTabletExtent("", nil, nil); !errors.Is(err, ErrInvalidTabletExtent) {
		t.Fatalf("empty table id = %v", err)
	}
	if _, err := NewTabletExtent("1", []byte("a"), []byte("m")); !errors.Is(err, ErrInvalidTabletExtent) {
		t.Fatalf("prev row after end row = %v", err)
	}
	if _, err := NewTabletExtent("1", []byte("a"), []byte("a")); !errors.Is(err, ErrInvalidTabletExtent) {
		t.Fatalf("prev row equal to end row = %v", err)
	}
	// An absent bound is always legal, in either position, and nil is the only
	// spelling of absent: an empty slice is the empty row.
	for _, pair := range [][2][]byte{{nil, nil}, {[]byte("a"), nil}, {nil, []byte("z")}} {
		if _, err := NewTabletExtent("1", pair[0], pair[1]); err != nil {
			t.Fatalf("NewTabletExtent(1, %q, %q) = %v", pair[0], pair[1], err)
		}
	}
	empty, err := NewTabletExtent("1", []byte{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if empty.EndRow == nil {
		t.Fatal("an empty end row collapsed into an absent one")
	}
	if got := empty.MetadataEntry(); got != "1;" {
		t.Fatalf("empty end row metadata entry = %q", got)
	}
	if _, err := NewTabletExtent("1", []byte{}, []byte{}); !errors.Is(err, ErrInvalidTabletExtent) {
		t.Fatalf("equal empty bounds = %v", err)
	}
}

func TestParseTabletExtentDecodesMetadataRows(t *testing.T) {
	cases := []struct {
		row     string
		tableID string
		endRow  string
	}{
		{"1;k", "1", "k"},
		{"1<", "1", ""},
		{"!0<", "!0", ""},
		{"1;k;with;semicolons", "1", "k;with;semicolons"},
		{"1;k<", "1", "k<"},
	}
	for _, tc := range cases {
		extent, err := ParseTabletExtent(tc.row, []byte("a"))
		if err != nil {
			t.Fatalf("ParseTabletExtent(%q) = %v", tc.row, err)
		}
		if extent.TableID != tc.tableID || string(extent.EndRow) != tc.endRow {
			t.Fatalf("ParseTabletExtent(%q) = %+v", tc.row, extent)
		}
		if string(extent.PrevRow) != "a" {
			t.Fatalf("ParseTabletExtent(%q) prev row = %q", tc.row, extent.PrevRow)
		}
	}
	// A trailing separator names the empty end row, which is a bound and so
	// cannot sit at or before a previous end row.
	emptyEnd, err := ParseTabletExtent("1;", nil)
	if err != nil {
		t.Fatal(err)
	}
	if emptyEnd.EndRow == nil || len(emptyEnd.EndRow) != 0 {
		t.Fatalf("trailing separator decoded to %+v", emptyEnd)
	}
	if _, err := ParseTabletExtent("1;", []byte("a")); !errors.Is(err, ErrInvalidTabletExtent) {
		t.Fatalf("empty end row after a previous end row = %v", err)
	}

	// The bounded form wins, so an end row that ends in '<' round trips.
	roundTrip, err := ParseTabletExtent(MetadataEntry("1", []byte("k<")), nil)
	if err != nil {
		t.Fatal(err)
	}
	if roundTrip.TableID != "1" || string(roundTrip.EndRow) != "k<" {
		t.Fatalf("round trip = %+v", roundTrip)
	}
	if roundTrip.EndRow == nil {
		t.Fatal("an end row ending in '<' decoded as an absent bound")
	}
	for _, bad := range []string{"", "no-separator", "<", ";k"} {
		if _, err := ParseTabletExtent(bad, nil); !errors.Is(err, ErrInvalidTabletExtent) {
			t.Fatalf("ParseTabletExtent(%q) = %v", bad, err)
		}
	}
}

func TestParseTabletExtentHandlesLongRows(t *testing.T) {
	// The pinned decoder stores separator positions in an int16, so a row
	// longer than 32767 bytes wraps the position negative.
	long := make([]byte, 40000)
	for index := range long {
		long[index] = 'r'
	}
	row := "1;" + string(long)
	extent, err := ParseTabletExtent(row, nil)
	if err != nil {
		t.Fatal(err)
	}
	if extent.TableID != "1" || len(extent.EndRow) != len(long) {
		t.Fatalf("long row decoded to table %q with a %d byte end row", extent.TableID, len(extent.EndRow))
	}
	prefixed := make([]byte, 40000)
	for index := range prefixed {
		prefixed[index] = 'i'
	}
	extent, err = ParseTabletExtent(string(prefixed)+";k", nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(extent.TableID) != len(prefixed) || string(extent.EndRow) != "k" {
		t.Fatalf("long table id decoded to %d bytes with end row %q", len(extent.TableID), extent.EndRow)
	}
}

func TestTabletExtentCloneAndSetters(t *testing.T) {
	extent, err := NewTabletExtent("1", []byte("m"), []byte("a"))
	if err != nil {
		t.Fatal(err)
	}
	clone := extent.Clone()
	if !clone.Equal(extent) {
		t.Fatalf("clone = %+v", clone)
	}
	clone.EndRow[0] = 'X'
	clone.PrevRow[0] = 'X'
	if string(extent.EndRow) != "m" || string(extent.PrevRow) != "a" {
		t.Fatalf("clone shares storage with its source: %+v", extent)
	}
	prevRow := []byte("b")
	clone.SetTableID("2")
	clone.SetPrevRow(prevRow)
	prevRow[0] = 'X'
	if clone.TableID != "2" || string(clone.PrevRow) != "b" {
		t.Fatalf("setters = %+v", clone)
	}
	clone.SetPrevRow(nil)
	if clone.PrevRow != nil {
		t.Fatalf("SetPrevRow(nil) = %q", clone.PrevRow)
	}
}

func TestTabletExtentRangeCoversTheTablet(t *testing.T) {
	extent, err := NewTabletExtent("1", []byte("m"), []byte("a"))
	if err != nil {
		t.Fatal(err)
	}
	tabletRange, err := extent.Range()
	if err != nil {
		t.Fatal(err)
	}
	if string(tabletRange.StartRow()) != "a" || string(tabletRange.EndRow()) != "m" {
		t.Fatalf("range = %s", tabletRange)
	}
	if tabletRange.Contains(Key{Row: []byte("a")}) {
		t.Fatal("the previous end row is inside the tablet")
	}
	if !tabletRange.Contains(Key{Row: []byte("m")}) {
		t.Fatal("the end row is outside the tablet")
	}
	last, err := NewTabletExtent("1", nil, []byte("m"))
	if err != nil {
		t.Fatal(err)
	}
	lastRange, err := last.Range()
	if err != nil {
		t.Fatal(err)
	}
	if lastRange.EndRow() != nil {
		t.Fatalf("the last tablet's range is bounded above: %s", lastRange)
	}
}

func TestTabletExtentContains(t *testing.T) {
	middle, err := NewTabletExtent("1", []byte("m"), []byte("a"))
	if err != nil {
		t.Fatal(err)
	}
	for row, want := range map[string]bool{"a": false, "a\x00": true, "b": true, "m": true, "n": false} {
		if got := middle.Contains([]byte(row)); got != want {
			t.Fatalf("Contains(%q) = %v, want %v", row, got, want)
		}
	}
	first, err := NewTabletExtent("1", []byte("m"), nil)
	if err != nil {
		t.Fatal(err)
	}
	if !first.Contains([]byte("")) || !first.Contains([]byte("a")) || first.Contains([]byte("z")) {
		t.Fatal("the first tablet does not cover everything up to its end row")
	}
	last, err := NewTabletExtent("1", nil, []byte("m"))
	if err != nil {
		t.Fatal(err)
	}
	if last.Contains([]byte("m")) || !last.Contains([]byte("z")) {
		t.Fatal("the last tablet does not cover everything after its previous end row")
	}
	whole, err := NewTabletExtent("1", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !whole.Contains(nil) || !whole.Contains([]byte("anything")) {
		t.Fatal("an unbounded tablet does not contain every row")
	}
}

func TestTabletExtentCompareSortsTabletsInTabletOrder(t *testing.T) {
	must := func(tableID, end, prev string) TabletExtent {
		t.Helper()
		var endRow, prevRow []byte
		if end != "" {
			endRow = []byte(end)
		}
		if prev != "" {
			prevRow = []byte(prev)
		}
		extent, err := NewTabletExtent(tableID, endRow, prevRow)
		if err != nil {
			t.Fatal(err)
		}
		return extent
	}
	first := must("1", "k", "")
	middle := must("1", "p", "k")
	// The pinned operator< compares end rows as plain strings, so the last
	// tablet - whose end row is empty - sorts before every other tablet.
	last := must("1", "", "p")
	other := must("2", "a", "")

	extents := []TabletExtent{other, last, middle, first}
	sort.Slice(extents, func(i, j int) bool { return extents[i].Less(extents[j]) })
	want := []string{"1;k", "1;p", "1<", "2;a"}
	for index, extent := range extents {
		if got := extent.MetadataEntry(); got != want[index] {
			t.Fatalf("sorted[%d] = %s, want %s", index, got, want[index])
		}
	}
	if !first.Less(last) || last.Less(first) {
		t.Fatal("the last tablet does not sort after a bounded one")
	}
	if first.Equal(middle) || !first.Equal(first.Clone()) {
		t.Fatal("Equal disagrees with identity")
	}
	// A tablet with no lower bound sorts before one that has it.
	if !must("1", "k", "").Less(must("1", "k", "a")) {
		t.Fatal("the first tablet does not sort before a later one with the same end row")
	}
}

func TestTabletExtentString(t *testing.T) {
	extent, err := NewTabletExtent("1", []byte("m"), []byte("a"))
	if err != nil {
		t.Fatal(err)
	}
	if got := extent.String(); got != "tableId:1 end:m prev:a" {
		t.Fatalf("String = %q", got)
	}
	unbounded, err := NewTabletExtent("1", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := unbounded.String(); got != "tableId:1 end:< prev:<" {
		t.Fatalf("unbounded String = %q", got)
	}
}

func TestRootTabletExtent(t *testing.T) {
	root := RootTabletExtent()
	if root.TableID != MetadataTableID {
		t.Fatalf("RootTabletExtent table = %q", root.TableID)
	}
	if string(root.EndRow) != MetadataTableID+"<" {
		t.Fatalf("RootTabletExtent end row = %q", root.EndRow)
	}
	if root.PrevRow != nil {
		t.Fatalf("RootTabletExtent prev row = %q", root.PrevRow)
	}
	if got := root.MetadataEntry(); got != "!0;!0<" {
		t.Fatalf("RootTabletExtent metadata entry = %q", got)
	}
	// Each call hands out a value the caller owns, so one consumer cannot
	// rewrite the extent the next one reads.
	root.EndRow[0] = 'X'
	root.SetTableID("2")
	fresh := RootTabletExtent()
	if fresh.TableID != MetadataTableID || string(fresh.EndRow) != MetadataTableID+"<" {
		t.Fatalf("a mutated copy changed the root extent: %+v", fresh)
	}
}
