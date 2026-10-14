package accumulo

import (
	"sort"
	"strings"
	"testing"
)

func TestNewColumnWithVisibilityCopiesEveryComponent(t *testing.T) {
	family := []byte("cf")
	qualifier := []byte("cq")
	visibility := []byte("cv")
	column := NewColumnWithVisibility(family, qualifier, visibility)
	family[0] = 'X'
	qualifier[0] = 'X'
	visibility[0] = 'X'
	if string(column.Family()) != "cf" || string(column.Qualifier()) != "cq" ||
		string(column.Visibility()) != "cv" {
		t.Fatalf("column followed the caller's slices: %+v", column)
	}
	returned := column.Visibility()
	returned[0] = 'X'
	if string(column.Visibility()) != "cv" {
		t.Fatalf("Visibility handed out its own storage: %q", column.Visibility())
	}
	// The narrower constructors leave the components they do not name unset.
	if NewColumnFamily([]byte("cf")).Visibility() != nil {
		t.Fatal("NewColumnFamily set a visibility")
	}
	if NewColumn([]byte("cf"), []byte("cq")).Visibility() != nil {
		t.Fatal("NewColumn set a visibility")
	}
}

func TestColumnSettersCopyAndReplace(t *testing.T) {
	var column Column
	family := []byte("cf")
	qualifier := []byte("cq")
	visibility := []byte("cv")
	column.SetFamily(family)
	column.SetQualifier(qualifier)
	column.SetVisibility(visibility)
	family[0] = 'X'
	qualifier[0] = 'X'
	visibility[0] = 'X'
	if string(column.Family()) != "cf" || string(column.Qualifier()) != "cq" ||
		string(column.Visibility()) != "cv" {
		t.Fatalf("setters aliased the caller's slices: %+v", column)
	}
	column.SetVisibility(nil)
	if column.Visibility() != nil {
		t.Fatalf("SetVisibility(nil) = %q", column.Visibility())
	}
}

func TestColumnCompareOrdersEveryComponent(t *testing.T) {
	base := NewColumnWithVisibility([]byte("cf"), []byte("cq"), []byte("cv"))
	cases := []struct {
		name  string
		other Column
		want  int
	}{
		{"identical", NewColumnWithVisibility([]byte("cf"), []byte("cq"), []byte("cv")), 0},
		{"family", NewColumnWithVisibility([]byte("cg"), []byte("cq"), []byte("cv")), -1},
		{"qualifier", NewColumnWithVisibility([]byte("cf"), []byte("cr"), []byte("cv")), -1},
		{"visibility", NewColumnWithVisibility([]byte("cf"), []byte("cq"), []byte("cw")), -1},
	}
	for _, tc := range cases {
		got := base.Compare(tc.other)
		if (got < 0) != (tc.want < 0) || (got == 0) != (tc.want == 0) {
			t.Fatalf("%s: Compare = %d, want sign of %d", tc.name, got, tc.want)
		}
		if tc.want == 0 {
			if !base.Equal(tc.other) || base.Less(tc.other) {
				t.Fatalf("%s: Equal/Less disagree with Compare", tc.name)
			}
			continue
		}
		if base.Equal(tc.other) {
			t.Fatalf("%s: differing columns compared equal", tc.name)
		}
		if !base.Less(tc.other) || tc.other.Less(base) {
			t.Fatalf("%s: ordering is not antisymmetric", tc.name)
		}
	}
	// A column that names fewer components sorts before one that names more.
	if !NewColumnFamily([]byte("cf")).Less(NewColumn([]byte("cf"), []byte("cq"))) {
		t.Fatal("a family-only column does not sort first")
	}
}

func TestColumnCompareSortsASlice(t *testing.T) {
	columns := []Column{
		NewColumnWithVisibility([]byte("b"), nil, nil),
		NewColumnWithVisibility([]byte("a"), []byte("q"), []byte("v")),
		NewColumnWithVisibility([]byte("a"), []byte("q"), nil),
		NewColumnFamily([]byte("a")),
	}
	sort.Slice(columns, func(i, j int) bool { return columns[i].Less(columns[j]) })
	want := []string{"a//", "a/q/", "a/q/v", "b//"}
	for index, column := range columns {
		got := strings.Join([]string{
			string(column.Family()),
			string(column.Qualifier()),
			string(column.Visibility()),
		}, "/")
		if got != want[index] {
			t.Fatalf("sorted[%d] = %s, want %s", index, got, want[index])
		}
	}
}

func TestColumnVisibilityReachesTheWire(t *testing.T) {
	columns := []Column{NewColumnWithVisibility([]byte("cf"), []byte("cq"), []byte("cv"))}
	wire := columnsToThrift(columns)
	if len(wire) != 1 {
		t.Fatalf("columnsToThrift returned %d columns", len(wire))
	}
	if string(wire[0].ColumnFamily) != "cf" || string(wire[0].ColumnQualifier) != "cq" ||
		string(wire[0].ColumnVisibility) != "cv" {
		t.Fatalf("wire column = %+v", wire[0])
	}
	cloned := cloneColumns(columns)
	if len(cloned) != 1 || !cloned[0].Equal(columns[0]) {
		t.Fatalf("cloneColumns dropped a component: %+v", cloned)
	}
	cloned[0].SetVisibility([]byte("other"))
	if string(columns[0].Visibility()) != "cv" {
		t.Fatal("cloneColumns shares storage with its source")
	}
}

func TestNewKeyValueCopiesBothHalves(t *testing.T) {
	key := NewKey([]byte("row"))
	value := []byte("v")
	entry := NewKeyValue(key, value)
	key.Row[0] = 'X'
	value[0] = 'X'
	if string(entry.Key.Row) != "row" || string(entry.Value) != "v" {
		t.Fatalf("entry followed the caller's buffers: %+v", entry)
	}
	if entry.Value == nil {
		t.Fatal("NewKeyValue dropped the value")
	}
	if empty := NewKeyValue(Key{}, nil); empty.Value != nil {
		t.Fatalf("NewKeyValue invented a value: %q", empty.Value)
	}
}

func TestKeyValueCloneAndSetters(t *testing.T) {
	entry := NewKeyValue(NewKey([]byte("row")), []byte("v"))
	clone := entry.Clone()
	if !clone.Equal(entry) {
		t.Fatalf("clone = %+v", clone)
	}
	clone.Key.Row[0] = 'X'
	clone.Value[0] = 'X'
	if string(entry.Key.Row) != "row" || string(entry.Value) != "v" {
		t.Fatalf("clone shares storage with its source: %+v", entry)
	}
	key := NewKey([]byte("other"))
	value := []byte("w")
	clone.SetKey(key)
	clone.SetValue(value)
	key.Row[0] = 'X'
	value[0] = 'X'
	if string(clone.Key.Row) != "other" || string(clone.Value) != "w" {
		t.Fatalf("setters aliased the caller's buffers: %+v", clone)
	}
	clone.SetValue(nil)
	if clone.Value != nil {
		t.Fatalf("SetValue(nil) = %q", clone.Value)
	}
}

func TestKeyValueEqualComparesContentNotIdentity(t *testing.T) {
	// The pinned operator== compares the two key addresses and the two value
	// shared_ptrs, so entries built separately from identical bytes are never
	// equal. Go compares content.
	left := NewKeyValue(NewKey([]byte("row")), []byte("v"))
	right := NewKeyValue(NewKey([]byte("row")), []byte("v"))
	if !left.Equal(right) {
		t.Fatal("entries with identical content compared unequal")
	}
	if left.Less(right) || right.Less(left) {
		t.Fatal("equal entries are ordered")
	}
	other := NewKeyValue(NewKey([]byte("row")), []byte("w"))
	if left.Equal(other) {
		t.Fatal("entries with different values compared equal")
	}
	if !left.Less(other) || other.Less(left) {
		t.Fatal("entries that share a key are not ordered by value")
	}
}

func TestKeyValueCompareOrdersByKeyThenValue(t *testing.T) {
	entries := []KeyValue{
		NewKeyValue(NewKeyWithColumns([]byte("b"), nil, nil, nil, 1), []byte("a")),
		NewKeyValue(NewKeyWithColumns([]byte("a"), nil, nil, nil, 1), []byte("b")),
		NewKeyValue(NewKeyWithColumns([]byte("a"), nil, nil, nil, 1), []byte("a")),
		NewKeyValue(NewKeyWithColumns([]byte("a"), nil, nil, nil, 9), []byte("z")),
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Less(entries[j]) })
	want := []string{"a@9=z", "a@1=a", "a@1=b", "b@1=a"}
	for index, entry := range entries {
		got := string(entry.Key.Row) + "@" + itoa(entry.Key.Timestamp) + "=" + string(entry.Value)
		if got != want[index] {
			t.Fatalf("sorted[%d] = %s, want %s", index, got, want[index])
		}
	}
}

func TestKeyValueStringNamesBothHalves(t *testing.T) {
	entry := NewKeyValue(NewKeyWithColumns([]byte("row"), []byte("cf"), nil, nil, 5), []byte("val"))
	got := entry.String()
	if !strings.HasPrefix(got, "key is ") {
		t.Fatalf("String = %q", got)
	}
	if !strings.Contains(got, "row") || !strings.Contains(got, "value is 3 bytes") {
		t.Fatalf("String = %q", got)
	}
	if !strings.Contains(got, entry.Key.String()) {
		t.Fatalf("String = %q, want it to contain the key rendering %q", got, entry.Key.String())
	}
	if strings.Contains(got, "0x") {
		t.Fatalf("String = %q, want no pointer address", got)
	}
	empty := KeyValue{}
	if !strings.Contains(empty.String(), "value is 0 bytes") {
		t.Fatalf("empty String = %q", empty.String())
	}
}
