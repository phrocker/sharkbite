package rfile

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"testing"

	"github.com/phrocker/shoal-oss/internal/rfile/bcfile"
	"github.com/phrocker/shoal-oss/internal/rfile/bcfile/block"
	"github.com/phrocker/shoal-oss/internal/visfilter"
)

// TestVisfilter_EndToEnd writes an RFile with cells under a few CV
// labels, opens it with a scoped Authorizations set, and confirms only
// the visible cells come back. Exercises the relkey filter pushdown
// path + visfilter package together.
func TestVisfilter_EndToEnd(t *testing.T) {
	cells := []struct {
		row, cv string
	}{
		{"r01", "A"},
		{"r02", "B"},
		{"r03", "A&B"},
		{"r04", "A|B"},
		{"r05", "C"},
		{"r06", ""}, // empty CV — always visible
		{"r07", "A&C"},
		{"r08", "(A&C)|B"},
	}
	var buf bytes.Buffer
	w, err := NewWriter(&buf, WriterOptions{})
	if err != nil {
		t.Fatal(err)
	}
	for i, c := range cells {
		k := &Key{
			Row:              []byte(c.row),
			ColumnFamily:     []byte("cf"),
			ColumnQualifier:  []byte("cq"),
			ColumnVisibility: []byte(c.cv),
			Timestamp:        int64(i + 1),
		}
		if err := w.Append(k, []byte(fmt.Sprintf("v%02d", i))); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}

	// Scenario: scanner has only auth A. Visible: r01 (A), r04 (A|B),
	// r06 (empty CV).
	auths := visfilter.NewAuthorizations([]byte("A"))
	ev := visfilter.NewEvaluator(auths)
	got := walkWithFilter(t, buf.Bytes(), func(k *Key) bool {
		return ev.Visible(k.ColumnVisibility)
	})
	want := []string{"r01", "r04", "r06"}
	if !equalRows(got, want) {
		t.Errorf("auth=A visible rows = %v; want %v", got, want)
	}

	// Scenario: auths A + B. Visible: r01 r02 r03 r04 r06 r08.
	auths2 := visfilter.NewAuthorizations([]byte("A"), []byte("B"))
	ev2 := visfilter.NewEvaluator(auths2)
	got = walkWithFilter(t, buf.Bytes(), func(k *Key) bool {
		return ev2.Visible(k.ColumnVisibility)
	})
	want = []string{"r01", "r02", "r03", "r04", "r06", "r08"}
	if !equalRows(got, want) {
		t.Errorf("auth=A,B visible rows = %v; want %v", got, want)
	}

	// Scenario: no auths. Visible: r06 only (empty CV).
	auths3 := visfilter.NewAuthorizations()
	ev3 := visfilter.NewEvaluator(auths3)
	got = walkWithFilter(t, buf.Bytes(), func(k *Key) bool {
		return ev3.Visible(k.ColumnVisibility)
	})
	if !equalRows(got, []string{"r06"}) {
		t.Errorf("no-auths visible rows = %v; want [r06]", got)
	}
}

// TestVisfilter_FilterDoesNotBreakPrevState confirms that rejected cells
// still update prev correctly — the next non-rejected cell's
// prefix-decompression must work. Stresses by filtering out 50% in a
// pattern that interleaves accept/reject.
func TestVisfilter_FilterDoesNotBreakPrevState(t *testing.T) {
	cells := []struct{ row, cv string }{}
	// 200 cells, alternating CV labels.
	for i := 0; i < 200; i++ {
		cv := "A"
		if i%2 == 0 {
			cv = "B"
		}
		cells = append(cells, struct{ row, cv string }{
			row: fmt.Sprintf("row%04d", i),
			cv:  cv,
		})
	}
	var buf bytes.Buffer
	// Tiny block size so we span many blocks (exercises seek + filter
	// across block boundaries).
	w, _ := NewWriter(&buf, WriterOptions{BlockSize: 100})
	for i, c := range cells {
		k := &Key{
			Row:              []byte(c.row),
			ColumnFamily:     []byte("cf"),
			ColumnQualifier:  []byte("cq"),
			ColumnVisibility: []byte(c.cv),
			Timestamp:        int64(i + 1),
		}
		_ = w.Append(k, []byte(c.row))
	}
	_ = w.Close()

	// Accept only "A" cells. Should yield exactly the odd indices.
	auths := visfilter.NewAuthorizations([]byte("A"))
	ev := visfilter.NewEvaluator(auths)
	got := walkWithFilter(t, buf.Bytes(), func(k *Key) bool {
		return ev.Visible(k.ColumnVisibility)
	})
	if len(got) != 100 {
		t.Fatalf("got %d accepted; want 100", len(got))
	}
	for i, row := range got {
		want := fmt.Sprintf("row%04d", 2*i+1)
		if row != want {
			t.Errorf("accepted[%d] = %s; want %s", i, row, want)
		}
	}
}

func walkWithFilter(t *testing.T, bs []byte, f func(*Key) bool) []string {
	t.Helper()
	bc, err := bcfile.NewReader(bytes.NewReader(bs), int64(len(bs)))
	if err != nil {
		t.Fatal(err)
	}
	r, err := Open(bc, block.Default())
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	r.SetFilter(f)

	var rows []string
	for {
		k, _, err := r.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		rows = append(rows, string(k.Row))
	}
	return rows
}

func equalRows(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}
