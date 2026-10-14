package wire

import "testing"

// TestKeyCompare walks every branch of the PartialKey
// ROW_COLFAM_COLQUAL_COLVIS_TIME_DEL ordering. Cases pin both directions
// of each comparison so a sign flip in either field surfaces immediately.
func TestKeyCompare(t *testing.T) {
	mk := func(row, cf, cq, cv string, ts int64, del bool) *Key {
		return &Key{
			Row:              []byte(row),
			ColumnFamily:     []byte(cf),
			ColumnQualifier:  []byte(cq),
			ColumnVisibility: []byte(cv),
			Timestamp:        ts,
			Deleted:          del,
		}
	}

	cases := []struct {
		name string
		a, b *Key
		want int
	}{
		// Row dominates.
		{"row_lt", mk("a", "x", "x", "x", 0, false), mk("b", "x", "x", "x", 0, false), -1},
		{"row_gt", mk("b", "x", "x", "x", 0, false), mk("a", "x", "x", "x", 0, false), 1},
		// CF tiebreaks when row matches.
		{"cf_lt", mk("r", "a", "x", "x", 0, false), mk("r", "b", "x", "x", 0, false), -1},
		{"cf_gt", mk("r", "b", "x", "x", 0, false), mk("r", "a", "x", "x", 0, false), 1},
		// CQ tiebreaks next.
		{"cq_lt", mk("r", "f", "a", "x", 0, false), mk("r", "f", "b", "x", 0, false), -1},
		{"cq_gt", mk("r", "f", "b", "x", 0, false), mk("r", "f", "a", "x", 0, false), 1},
		// CV tiebreaks next.
		{"cv_lt", mk("r", "f", "q", "a", 0, false), mk("r", "f", "q", "b", 0, false), -1},
		{"cv_gt", mk("r", "f", "q", "b", 0, false), mk("r", "f", "q", "a", 0, false), 1},
		// Timestamp DESCENDING — newer (larger) sorts FIRST.
		{"ts_newer_first", mk("r", "f", "q", "v", 100, false), mk("r", "f", "q", "v", 50, false), -1},
		{"ts_older_last", mk("r", "f", "q", "v", 50, false), mk("r", "f", "q", "v", 100, false), 1},
		// Deleted: true sorts BEFORE false at otherwise identical coord.
		{"deleted_first", mk("r", "f", "q", "v", 100, true), mk("r", "f", "q", "v", 100, false), -1},
		{"live_after_tombstone", mk("r", "f", "q", "v", 100, false), mk("r", "f", "q", "v", 100, true), 1},
		// Equal everywhere.
		{"equal", mk("r", "f", "q", "v", 100, false), mk("r", "f", "q", "v", 100, false), 0},
		{"equal_deleted", mk("r", "f", "q", "v", 100, true), mk("r", "f", "q", "v", 100, true), 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := c.a.Compare(c.b); sign(got) != c.want {
				t.Errorf("%s: got %d, want %d", c.name, got, c.want)
			}
			// Anti-symmetry: a.Compare(b) == -b.Compare(a).
			if sign(c.a.Compare(c.b)) != -sign(c.b.Compare(c.a)) {
				t.Errorf("%s: not anti-symmetric (a<->b = %d, %d)", c.name, c.a.Compare(c.b), c.b.Compare(c.a))
			}
		})
	}
}

// TestKeyEqual ensures Equal treats nil and empty slices the same and is
// strict on every field.
func TestKeyEqual(t *testing.T) {
	a := &Key{Row: []byte("r"), ColumnFamily: nil, ColumnQualifier: []byte{}, ColumnVisibility: []byte{}, Timestamp: 1, Deleted: false}
	b := &Key{Row: []byte("r"), ColumnFamily: []byte{}, ColumnQualifier: []byte{}, ColumnVisibility: nil, Timestamp: 1, Deleted: false}
	if !a.Equal(b) {
		t.Error("nil vs empty CF/CV should compare equal")
	}
	c := *b
	c.Timestamp = 2
	if a.Equal(&c) {
		t.Error("different timestamp must not compare equal")
	}
	c = *b
	c.Deleted = true
	if a.Equal(&c) {
		t.Error("different deleted flag must not compare equal")
	}
}

// TestKeyClone defends against the most subtle aliasing bug in the relkey
// reader: prevKey holding a slice into the wire buffer and getting
// overwritten by the next decode.
func TestKeyClone(t *testing.T) {
	src := []byte("rowdata")
	k := &Key{Row: src, Timestamp: 7, Deleted: true}
	cloned := k.Clone()
	src[0] = 'x'
	if cloned.Row[0] != 'r' {
		t.Errorf("Clone shares backing array: got %q", cloned.Row)
	}
	if cloned.Timestamp != 7 || !cloned.Deleted {
		t.Errorf("scalar fields lost in Clone: %+v", cloned)
	}
}

func TestCompareRowCFCQCV(t *testing.T) {
	a := &Key{Row: []byte("r"), ColumnFamily: []byte("f"), ColumnQualifier: []byte("q"), ColumnVisibility: []byte("v"), Timestamp: 100}
	b := &Key{Row: []byte("r"), ColumnFamily: []byte("f"), ColumnQualifier: []byte("q"), ColumnVisibility: []byte("v"), Timestamp: 50, Deleted: true}
	// Differ in ts and deleted but the prefix comparator must say equal.
	if got := a.CompareRowCFCQCV(b); got != 0 {
		t.Errorf("CompareRowCFCQCV ignored ts/deleted: got %d", got)
	}
	c := &Key{Row: []byte("r"), ColumnFamily: []byte("f"), ColumnQualifier: []byte("q"), ColumnVisibility: []byte("w")}
	if got := a.CompareRowCFCQCV(c); got >= 0 {
		t.Errorf("CompareRowCFCQCV(v,w) = %d, want < 0", got)
	}
}

func sign(n int) int {
	switch {
	case n < 0:
		return -1
	case n > 0:
		return 1
	default:
		return 0
	}
}
