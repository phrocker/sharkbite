package accumulo

import (
	"errors"
	"sort"
	"testing"
)

func TestNewKeyCopiesItsInput(t *testing.T) {
	row := []byte("r")
	key := NewKey(row)
	row[0] = 'X'
	if string(key.Row) != "r" {
		t.Fatalf("Row = %q", key.Row)
	}
	if key.Timestamp != DefaultKeyTimestamp {
		t.Fatalf("Timestamp = %d, want %d", key.Timestamp, DefaultKeyTimestamp)
	}
	if key.ColumnFamily != nil || key.ColumnQualifier != nil || key.ColumnVisibility != nil {
		t.Fatal("NewKey populated a column")
	}
}

func TestNewKeyWithColumnsCopiesEveryComponent(t *testing.T) {
	row := []byte("r")
	family := []byte("cf")
	qualifier := []byte("cq")
	visibility := []byte("cv")
	key := NewKeyWithColumns(row, family, qualifier, visibility, 7)
	row[0] = 'X'
	family[0] = 'X'
	qualifier[0] = 'X'
	visibility[0] = 'X'
	if string(key.Row) != "r" || string(key.ColumnFamily) != "cf" ||
		string(key.ColumnQualifier) != "cq" || string(key.ColumnVisibility) != "cv" {
		t.Fatalf("key followed the caller's slices: %+v", key)
	}
	if key.Timestamp != 7 {
		t.Fatalf("Timestamp = %d", key.Timestamp)
	}
}

func TestKeyCloneSharesNoStorage(t *testing.T) {
	key := NewKeyWithColumns([]byte("r"), []byte("cf"), []byte("cq"), []byte("cv"), 3)
	key.Deleted = true
	clone := key.Clone()
	if !clone.Equal(key) || clone.Deleted != key.Deleted {
		t.Fatalf("clone = %+v", clone)
	}
	clone.Row[0] = 'X'
	clone.ColumnFamily[0] = 'X'
	clone.ColumnQualifier[0] = 'X'
	clone.ColumnVisibility[0] = 'X'
	if string(key.Row) != "r" || string(key.ColumnFamily) != "cf" ||
		string(key.ColumnQualifier) != "cq" || string(key.ColumnVisibility) != "cv" {
		t.Fatalf("clone shares storage with its source: %+v", key)
	}
}

func TestKeySettersCopyAndReplace(t *testing.T) {
	var key Key
	row := []byte("r")
	family := []byte("cf")
	qualifier := []byte("cq")
	visibility := []byte("cv")
	key.SetRow(row)
	key.SetColumnFamily(family)
	key.SetColumnQualifier(qualifier)
	key.SetColumnVisibility(visibility)
	key.SetTimestamp(11)
	key.SetDeleted(true)
	row[0] = 'X'
	family[0] = 'X'
	qualifier[0] = 'X'
	visibility[0] = 'X'
	if string(key.Row) != "r" || string(key.ColumnFamily) != "cf" ||
		string(key.ColumnQualifier) != "cq" || string(key.ColumnVisibility) != "cv" {
		t.Fatalf("setters aliased the caller's slices: %+v", key)
	}
	if key.Timestamp != 11 || !key.Deleted {
		t.Fatalf("timestamp = %d deleted = %v", key.Timestamp, key.Deleted)
	}
	key.SetDeleted(false)
	if key.Deleted {
		t.Fatal("SetDeleted(false) did not clear the marker")
	}
	key.SetRow(nil)
	if key.Row != nil {
		t.Fatalf("SetRow(nil) = %q", key.Row)
	}
}

func TestKeyEmptyTracksEveryComponent(t *testing.T) {
	var key Key
	if !key.Empty() {
		t.Fatal("the zero key is not empty")
	}
	for _, populate := range []func(*Key){
		func(k *Key) { k.SetRow([]byte("r")) },
		func(k *Key) { k.SetColumnFamily([]byte("cf")) },
		func(k *Key) { k.SetColumnQualifier([]byte("cq")) },
		func(k *Key) { k.SetColumnVisibility([]byte("cv")) },
	} {
		var populated Key
		populate(&populated)
		if populated.Empty() {
			t.Fatalf("a key with one populated component is empty: %+v", populated)
		}
	}
	// Neither the timestamp nor the deletion marker makes a key non-empty.
	var marked Key
	marked.SetTimestamp(7)
	marked.SetDeleted(true)
	if !marked.Empty() {
		t.Fatal("the timestamp or the deletion marker made the key non-empty")
	}
	key.SetRow([]byte("r"))
	key.SetRow(nil)
	if !key.Empty() {
		t.Fatal("clearing the row did not empty the key")
	}
}

func TestKeySizesReportComponentLengths(t *testing.T) {
	key := NewKeyWithColumns([]byte("row"), []byte("cf"), []byte("q"), []byte(""), 1)
	if key.RowSize() != 3 || key.ColumnFamilySize() != 2 ||
		key.ColumnQualifierSize() != 1 || key.ColumnVisibilitySize() != 0 {
		t.Fatalf("sizes = %d %d %d %d", key.RowSize(), key.ColumnFamilySize(),
			key.ColumnQualifierSize(), key.ColumnVisibilitySize())
	}
	if key.Length() != 3+2+1+0+8 {
		t.Fatalf("Length = %d", key.Length())
	}
	if key.Size() != key.Length() {
		t.Fatalf("Size = %d, Length = %d", key.Size(), key.Length())
	}
	var empty Key
	if empty.Length() != 8 || empty.RowSize() != 0 {
		t.Fatalf("empty key length = %d", empty.Length())
	}
	// A row set from a larger buffer reports its length, not the capacity the
	// source slice happened to have.
	buffer := make([]byte, 3, 64)
	copy(buffer, "row")
	key.SetRow(buffer)
	if key.RowSize() != 3 {
		t.Fatalf("RowSize after a large-capacity source = %d", key.RowSize())
	}
}

func TestKeyCompareOrdersLikeAccumulo(t *testing.T) {
	base := func() Key {
		return NewKeyWithColumns([]byte("r"), []byte("cf"), []byte("cq"), []byte("cv"), 5)
	}
	cases := []struct {
		name  string
		left  Key
		right Key
		want  int
	}{
		{"identical", base(), base(), 0},
		{"row", NewKey([]byte("a")), NewKey([]byte("b")), -1},
		{"family", func() Key { k := base(); k.SetColumnFamily([]byte("aa")); return k }(), base(), -1},
		{"qualifier", func() Key { k := base(); k.SetColumnQualifier([]byte("aa")); return k }(), base(), -1},
		{"visibility", func() Key { k := base(); k.SetColumnVisibility([]byte("aa")); return k }(), base(), -1},
		{"newer timestamp first", func() Key { k := base(); k.SetTimestamp(9); return k }(), base(), -1},
		{"older timestamp last", func() Key { k := base(); k.SetTimestamp(1); return k }(), base(), 1},
		{"deleted first", func() Key { k := base(); k.SetDeleted(true); return k }(), base(), -1},
		{"live last", base(), func() Key { k := base(); k.SetDeleted(true); return k }(), 1},
	}
	for _, tc := range cases {
		got := tc.left.Compare(tc.right)
		if (got < 0) != (tc.want < 0) || (got > 0) != (tc.want > 0) || (got == 0) != (tc.want == 0) {
			t.Fatalf("%s: Compare = %d, want sign of %d", tc.name, got, tc.want)
		}
		if reverse := tc.right.Compare(tc.left); (reverse > 0) != (tc.want < 0) {
			t.Fatalf("%s: reverse Compare = %d", tc.name, reverse)
		}
	}
}

func TestKeyCompareOrdersVisibilityBeforeTheTimestamp(t *testing.T) {
	left := NewKeyWithColumns([]byte("r"), []byte("cf"), []byte("cq"), []byte("a"), 5)
	right := NewKeyWithColumns([]byte("r"), []byte("cf"), []byte("cq"), []byte("b"), 5)
	if left.Compare(right) >= 0 {
		t.Fatalf("Compare = %d, want negative", left.Compare(right))
	}
	if left.Equal(right) {
		t.Fatal("cells with distinct visibilities compared equal")
	}
	// Visibility outranks the timestamp: a newer version of the "b" label
	// still sorts after every "a" label.
	newerRight := right.Clone()
	newerRight.SetTimestamp(9)
	if left.Compare(newerRight) >= 0 {
		t.Fatalf("Compare across visibilities = %d", left.Compare(newerRight))
	}
	if left.CompareToVisibility(right) >= 0 {
		t.Fatalf("CompareToVisibility = %d", left.CompareToVisibility(right))
	}
	// CompareToVisibility, and only it, ignores the timestamp and the marker.
	older := right.Clone()
	older.SetTimestamp(1)
	older.SetDeleted(true)
	if right.CompareToVisibility(older) != 0 {
		t.Fatalf("CompareToVisibility across versions = %d", right.CompareToVisibility(older))
	}
	if right.Compare(older) >= 0 {
		t.Fatalf("Compare across versions = %d", right.Compare(older))
	}
}

func TestKeyOrderingAgreesWithEquality(t *testing.T) {
	// The pinned operator< stops after the column qualifier while operator==
	// compares timestamps, so two versions of one cell are neither ordered nor
	// equal. Go's ordering is derived from Compare, so that cannot happen.
	newer := NewKeyWithColumns([]byte("r"), []byte("cf"), []byte("cq"), nil, 9)
	older := NewKeyWithColumns([]byte("r"), []byte("cf"), []byte("cq"), nil, 1)
	if newer.Equal(older) {
		t.Fatal("versions compared equal")
	}
	if !newer.Less(older) {
		t.Fatal("the newer version does not sort first")
	}
	if older.Less(newer) {
		t.Fatal("both orderings are true")
	}
	if !newer.LessOrEqual(older) || !newer.LessOrEqual(newer) {
		t.Fatal("LessOrEqual disagrees with Less and Equal")
	}
	if older.LessOrEqual(newer) {
		t.Fatal("LessOrEqual is not antisymmetric")
	}
}

func TestKeyCompareSortsASlice(t *testing.T) {
	keys := []Key{
		NewKeyWithColumns([]byte("b"), nil, nil, nil, 1),
		NewKeyWithColumns([]byte("a"), []byte("z"), nil, nil, 1),
		NewKeyWithColumns([]byte("a"), []byte("a"), nil, nil, 1),
		NewKeyWithColumns([]byte("a"), []byte("a"), nil, nil, 9),
	}
	sort.Slice(keys, func(i, j int) bool { return keys[i].Less(keys[j]) })
	want := []string{"a/a@9", "a/a@1", "a/z@1", "b/@1"}
	for index, key := range keys {
		got := string(key.Row) + "/" + string(key.ColumnFamily) + "@" +
			itoa(key.Timestamp)
		if got != want[index] {
			t.Fatalf("sorted[%d] = %s, want %s", index, got, want[index])
		}
	}
}

func TestKeyEqualComparesEveryOrderedComponent(t *testing.T) {
	base := NewKeyWithColumns([]byte("r"), []byte("cf"), []byte("cq"), []byte("cv"), 5)
	if !base.Equal(base.Clone()) {
		t.Fatal("a clone is not equal")
	}
	for _, mutate := range []func(Key) Key{
		func(k Key) Key { k.SetRow([]byte("other")); return k },
		func(k Key) Key { k.SetColumnFamily([]byte("other")); return k },
		func(k Key) Key { k.SetColumnQualifier([]byte("other")); return k },
		func(k Key) Key { k.SetColumnVisibility([]byte("other")); return k },
		func(k Key) Key { k.SetTimestamp(6); return k },
		func(k Key) Key { k.SetDeleted(true); return k },
	} {
		if base.Equal(mutate(base.Clone())) {
			t.Fatal("a mutated key compared equal")
		}
	}
}

func TestRangeRejectsDeletionMarkedBounds(t *testing.T) {
	deleted := NewKey([]byte("a"))
	deleted.SetDeleted(true)
	live := NewKey([]byte("z"))
	if _, err := NewKeyRange(&deleted, true, &live, true); !errors.Is(err, ErrDeletedRangeBound) {
		t.Fatalf("deleted start = %v", err)
	}
	if _, err := NewKeyRange(&live, true, &deleted, true); !errors.Is(err, ErrDeletedRangeBound) {
		t.Fatalf("deleted end = %v", err)
	}
	start := NewKey([]byte("a"))
	scanRange, err := NewKeyRange(&start, true, &live, true)
	if err != nil {
		t.Fatal(err)
	}
	if bound := scanRange.StartKey(); bound == nil || bound.Deleted {
		t.Fatalf("StartKey = %+v", bound)
	}
	// cloneKey still carries the marker for keys that are not range bounds, so
	// range predicates agree with the ordering the key type publishes.
	clone := cloneKey(&deleted)
	if clone == nil || !clone.Deleted {
		t.Fatalf("cloneKey dropped the deletion marker: %+v", clone)
	}
	if compareKeys(deleted, start) != deleted.Compare(start) {
		t.Fatalf("compareKeys = %d, Key.Compare = %d",
			compareKeys(deleted, start), deleted.Compare(start))
	}
}

func itoa(value int64) string {
	if value == 0 {
		return "0"
	}
	negative := value < 0
	if negative {
		value = -value
	}
	var digits []byte
	for value > 0 {
		digits = append([]byte{byte('0' + value%10)}, digits...)
		value /= 10
	}
	if negative {
		return "-" + string(digits)
	}
	return string(digits)
}
