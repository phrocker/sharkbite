// Licensed to the Apache Software Foundation (ASF) under one
// or more contributor license agreements.  See the NOTICE file
// distributed with this work for additional information
// regarding copyright ownership.  The ASF licenses this file
// to you under the Apache License, Version 2.0 (the
// "License"); you may not use this file except in compliance
// with the License.  You may obtain a copy of the License at
//
//   https://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing,
// software distributed under the License is distributed on an
// "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY
// KIND, either express or implied.  See the License for the
// specific language governing permissions and limitations
// under the License.

package iterrt

import "testing"

// stamp builds, Inits, and Seeks a VisibilityStampIterator over src.
func stamp(t *testing.T, src SortedKeyValueIterator, opts map[string]string) *VisibilityStampIterator {
	t.Helper()
	if err := src.Seek(InfiniteRange(), nil, false); err != nil {
		t.Fatalf("source seek: %v", err)
	}
	it := NewVisibilityStampIterator()
	if err := it.Init(src, opts, IteratorEnvironment{Scope: ScopeMajc}); err != nil {
		t.Fatalf("init: %v", err)
	}
	if err := it.Seek(InfiniteRange(), nil, false); err != nil {
		t.Fatalf("seek: %v", err)
	}
	return it
}

func assertSorted(t *testing.T, cells []kv) {
	t.Helper()
	for i := 1; i < len(cells); i++ {
		if cells[i-1].k.Compare(cells[i].k) > 0 {
			t.Fatalf("output not in key order at %d: %q > %q",
				i, cells[i-1].k.ColumnVisibility, cells[i].k.ColumnVisibility)
		}
	}
}

// TestVisibilityStampReordersWithinCoordinate is the core correctness case:
// stamping CV changes the sort position of cells at the same row/cf/cq, so
// the iterator must re-sort that window or it would emit out-of-order keys
// (which the RFile writer rejects).
func TestVisibilityStampReordersWithinCoordinate(t *testing.T) {
	// Same coordinate, two CVs at the same ts. Input order is CV "" < "Z".
	src := newSliceSource(
		kv{k: mk("r", "f", "q", "", 10)},
		kv{k: mk("r", "f", "q", "Z", 10)},
	)
	it := stamp(t, src, map[string]string{VisibilityStampLabelOption: "T"})
	got, err := drain(it)
	if err != nil {
		t.Fatalf("drain: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("want 2 cells, got %d", len(got))
	}
	// "" -> "T"; "Z" -> "(Z)&(T)". "(" (0x28) < "T" (0x54), so the stamped
	// "Z" cell must now sort FIRST — order flipped relative to input.
	if string(got[0].k.ColumnVisibility) != "(Z)&(T)" {
		t.Fatalf("first CV = %q, want (Z)&(T)", got[0].k.ColumnVisibility)
	}
	if string(got[1].k.ColumnVisibility) != "T" {
		t.Fatalf("second CV = %q, want T", got[1].k.ColumnVisibility)
	}
	assertSorted(t, got)
}

// TestVisibilityStampAndMode stamps every cell across multiple coordinates
// and checks the union is preserved, order is maintained, and labels are
// combined with AND.
func TestVisibilityStampAndMode(t *testing.T) {
	src := newSliceSource(
		kv{k: mk("a", "f", "q", "", 5), v: []byte("1")},
		kv{k: mk("b", "f", "q", "X", 5), v: []byte("2")},
		kv{k: mk("c", "f", "q", "", 5), v: []byte("3")},
	)
	it := stamp(t, src, map[string]string{VisibilityStampLabelOption: "agent1"})
	got, err := drain(it)
	if err != nil {
		t.Fatalf("drain: %v", err)
	}
	want := []string{"agent1", "(X)&(agent1)", "agent1"}
	if len(got) != len(want) {
		t.Fatalf("want %d cells, got %d", len(want), len(got))
	}
	for i := range want {
		if string(got[i].k.ColumnVisibility) != want[i] {
			t.Fatalf("cell %d CV = %q, want %q", i, got[i].k.ColumnVisibility, want[i])
		}
	}
	assertSorted(t, got)
	if string(got[0].v) != "1" || string(got[2].v) != "3" {
		t.Fatalf("values not preserved: %q %q", got[0].v, got[2].v)
	}
}

// TestVisibilityStampWhenEmptyMode only stamps unlabeled cells.
func TestVisibilityStampWhenEmptyMode(t *testing.T) {
	src := newSliceSource(
		kv{k: mk("r", "f", "q", "", 9)},
		kv{k: mk("r", "f", "q", "keep", 9)},
	)
	it := stamp(t, src, map[string]string{
		VisibilityStampLabelOption: "deflt",
		VisibilityStampModeOption:  "whenEmpty",
	})
	got, err := drain(it)
	if err != nil {
		t.Fatalf("drain: %v", err)
	}
	cvs := map[string]bool{}
	for _, c := range got {
		cvs[string(c.k.ColumnVisibility)] = true
	}
	if !cvs["deflt"] || !cvs["keep"] {
		t.Fatalf("whenEmpty produced %v, want {deflt, keep}", cvs)
	}
	assertSorted(t, got)
}

// TestVisibilityStampIdempotent re-stamping a cell that already carries the
// bare label is a no-op (no double-wrapping).
func TestVisibilityStampIdempotent(t *testing.T) {
	src := newSliceSource(kv{k: mk("r", "f", "q", "T", 1)})
	it := stamp(t, src, map[string]string{VisibilityStampLabelOption: "T"})
	got, err := drain(it)
	if err != nil {
		t.Fatalf("drain: %v", err)
	}
	if len(got) != 1 || string(got[0].k.ColumnVisibility) != "T" {
		t.Fatalf("got %d cells, CV %q; want single bare T", len(got), got[0].k.ColumnVisibility)
	}
}

func TestVisibilityStampInitValidation(t *testing.T) {
	it := NewVisibilityStampIterator()
	if err := it.Init(newSliceSource(), nil, IteratorEnvironment{}); err == nil {
		t.Fatal("missing label should error")
	}
	it = NewVisibilityStampIterator()
	if err := it.Init(newSliceSource(), map[string]string{VisibilityStampLabelOption: "bad label"}, IteratorEnvironment{}); err == nil {
		t.Fatal("invalid label (space) should error")
	}
	it = NewVisibilityStampIterator()
	if err := it.Init(newSliceSource(), map[string]string{VisibilityStampLabelOption: "ok", VisibilityStampModeOption: "nope"}, IteratorEnvironment{}); err == nil {
		t.Fatal("bad mode should error")
	}
}
