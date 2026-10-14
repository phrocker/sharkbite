//go:build !embed

package cclient

import (
	"bytes"
	"testing"
)

func TestColumn_AllEmptyMeansAllFamilies(t *testing.T) {
	c := NewColumn(nil, nil, nil)
	tc := c.ToThrift()
	if len(tc.ColumnFamily) != 0 || len(tc.ColumnQualifier) != 0 || len(tc.ColumnVisibility) != 0 {
		t.Errorf("wildcard column produced non-empty fields: %+v", tc)
	}
}

func TestColumn_PopulatedFields(t *testing.T) {
	c := NewColumn([]byte("loc"), []byte("server"), []byte("PUBLIC"))
	tc := c.ToThrift()
	if !bytes.Equal(tc.ColumnFamily, []byte("loc")) {
		t.Errorf("CF = %q", tc.ColumnFamily)
	}
	if !bytes.Equal(tc.ColumnQualifier, []byte("server")) {
		t.Errorf("CQ = %q", tc.ColumnQualifier)
	}
	if !bytes.Equal(tc.ColumnVisibility, []byte("PUBLIC")) {
		t.Errorf("CV = %q", tc.ColumnVisibility)
	}
}

func TestColumn_ToFromThriftRoundtrip(t *testing.T) {
	c := NewColumn([]byte("cf"), []byte("cq"), nil)
	got := ColumnFromThrift(c.ToThrift())
	if !got.Equal(c) {
		t.Errorf("roundtrip mismatch: got cf=%q cq=%q cv=%q", got.CF(), got.CQ(), got.CV())
	}
}

func TestColumn_ConvenienceCtors(t *testing.T) {
	cf := NewColumnCF([]byte("loc"))
	if !bytes.Equal(cf.CF(), []byte("loc")) || cf.CQ() != nil || cf.CV() != nil {
		t.Errorf("NewColumnCF wrong: %+v", cf)
	}
	cfcq := NewColumnCFCQ([]byte("loc"), []byte("server"))
	if !bytes.Equal(cfcq.CF(), []byte("loc")) || !bytes.Equal(cfcq.CQ(), []byte("server")) || cfcq.CV() != nil {
		t.Errorf("NewColumnCFCQ wrong: %+v", cfcq)
	}
}

func TestColumn_Equal(t *testing.T) {
	a := NewColumn([]byte("cf"), []byte("cq"), []byte("cv"))
	b := NewColumn([]byte("cf"), []byte("cq"), []byte("cv"))
	c := NewColumn([]byte("cf"), []byte("cq"), nil)
	if !a.Equal(b) {
		t.Error("a == b")
	}
	if a.Equal(c) {
		t.Error("a != c")
	}
	if a.Equal(nil) {
		t.Error("a != nil")
	}
}

func TestColumnFromThrift_Nil(t *testing.T) {
	if got := ColumnFromThrift(nil); got != nil {
		t.Errorf("FromThrift(nil) = %v, want nil", got)
	}
}

func TestColumnsToThrift_NilForEmpty(t *testing.T) {
	if got := ColumnsToThrift(nil); got != nil {
		t.Errorf("nil input → %v, want nil", got)
	}
	if got := ColumnsToThrift([]*Column{}); got != nil {
		t.Errorf("empty input → %v, want nil", got)
	}
}

func TestColumnsToThrift_PreservesOrder(t *testing.T) {
	cols := []*Column{
		NewColumnCF([]byte("a")),
		NewColumnCF([]byte("b")),
		NewColumnCF([]byte("c")),
	}
	wire := ColumnsToThrift(cols)
	if len(wire) != 3 {
		t.Fatalf("len = %d", len(wire))
	}
	for i, want := range []string{"a", "b", "c"} {
		if string(wire[i].ColumnFamily) != want {
			t.Errorf("[%d] = %q, want %q", i, wire[i].ColumnFamily, want)
		}
	}
}
