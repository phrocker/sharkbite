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

import (
	"fmt"
	"sort"
	"strconv"
	"testing"

	"github.com/phrocker/shoal-oss/internal/rfile/wire"
)

// cell is a tiny constructor for a test cell.
func tcell(row, cf, cq, val string, ts int64) Cell {
	return Cell{
		Key: &wire.Key{
			Row:             []byte(row),
			ColumnFamily:    []byte(cf),
			ColumnQualifier: []byte(cq),
			Timestamp:       ts,
		},
		Value: []byte(val),
	}
}

// sortedSlice sorts cells into wire.Key order so they can back a SliceSource.
func sortedSlice(cells []Cell) []Cell {
	out := append([]Cell(nil), cells...)
	sort.SliceStable(out, func(i, j int) bool {
		return out[i].Key.Compare(out[j].Key) < 0
	})
	return out
}

// runTermIndex wires a SliceSource → TermIndexIterator over r and drains it.
func runTermIndex(t *testing.T, cells []Cell, opts map[string]string, r Range) []Cell {
	t.Helper()
	leaf := NewSliceSource(sortedSlice(cells))
	if err := leaf.Init(nil, nil, IteratorEnvironment{Scope: ScopeScan}); err != nil {
		t.Fatalf("leaf init: %v", err)
	}
	it := NewTermIndexIterator()
	if err := it.Init(leaf, opts, IteratorEnvironment{Scope: ScopeScan}); err != nil {
		t.Fatalf("termindex init: %v", err)
	}
	if err := it.Seek(r, nil, false); err != nil {
		t.Fatalf("seek: %v", err)
	}
	var got []Cell
	for it.HasTop() {
		got = append(got, Cell{
			Key:   it.GetTopKey().Clone(),
			Value: append([]byte(nil), it.GetTopValue()...),
		})
		if err := it.Next(); err != nil {
			t.Fatalf("next: %v", err)
		}
	}
	return got
}

// rowsOf returns the distinct row strings of cells, in arrival order.
func rowsOf(cells []Cell) []string {
	var out []string
	seen := map[string]bool{}
	for _, c := range cells {
		r := string(c.Key.Row)
		if !seen[r] {
			seen[r] = true
			out = append(out, r)
		}
	}
	return out
}

// graphFixture builds a small node table plus two inverted-index posting rows.
//
//	node:1 attr:name=alice  attr:age=30
//	node:2 attr:name=bob
//	node:3 attr:name=carol
//	idx:dev  p:1  p:3   (postings: column qualifier = node id)
//	idx:ml   p:2  p:3
func graphFixture() []Cell {
	return []Cell{
		tcell("node:1", "attr", "name", "alice", 10),
		tcell("node:1", "attr", "age", "30", 10),
		tcell("node:2", "attr", "name", "bob", 10),
		tcell("node:3", "attr", "name", "carol", 10),
		tcell("idx:dev", "p", "1", "", 5),
		tcell("idx:dev", "p", "3", "", 5),
		tcell("idx:ml", "p", "2", "", 5),
		tcell("idx:ml", "p", "3", "", 5),
	}
}

func devMlOpts() map[string]string {
	return map[string]string{
		TermIndexCount:            "2",
		TermIndexTermPrefix + "0": "idx:dev",
		TermIndexTermPrefix + "1": "idx:ml",
		TermIndexPrimaryPrefix:    "node:",
	}
}

// TestTermIndex_UnionResolvesPrimaryRows verifies the core behaviour: the
// union of postings across the two term rows ({1,3} ∪ {2,3} = {1,2,3}) is
// resolved to primary rows, de-duplicated, and every primary cell is emitted
// in wire.Key order. The posting rows themselves must NOT appear.
func TestTermIndex_UnionResolvesPrimaryRows(t *testing.T) {
	got := runTermIndex(t, graphFixture(), devMlOpts(), InfiniteRange())

	gotRows := rowsOf(got)
	wantRows := []string{"node:1", "node:2", "node:3"}
	if fmt.Sprint(gotRows) != fmt.Sprint(wantRows) {
		t.Fatalf("rows: got %v want %v", gotRows, wantRows)
	}
	// All four primary cells (node:1 has two), no posting cells.
	if len(got) != 4 {
		t.Fatalf("cell count: got %d want 4 (cells: %v)", len(got), cellsToStrings(got))
	}
	for _, c := range got {
		if string(c.Key.ColumnFamily) == "p" {
			t.Errorf("posting cell leaked into output: %s", cellToString(c))
		}
	}

	// Output must be globally sorted.
	for i := 1; i < len(got); i++ {
		if got[i-1].Key.Compare(got[i].Key) >= 0 {
			t.Errorf("output not sorted at %d: %s !< %s",
				i, cellToString(got[i-1]), cellToString(got[i]))
		}
	}
}

func TestTermIndex_PhraseIntersection(t *testing.T) {
	opts := devMlOpts()
	opts[TermIndexPhrase] = "true"
	got := runTermIndex(t, graphFixture(), opts, InfiniteRange())
	wantRows := []string{"node:3"}
	if fmt.Sprint(rowsOf(got)) != fmt.Sprint(wantRows) {
		t.Fatalf("rows: got %v want %v", rowsOf(got), wantRows)
	}
}

func TestTermIndex_NumericRangeBounds(t *testing.T) {
	cells := []Cell{
		tcell("node:1", "attr", "name", "one", 10),
		tcell("node:2", "attr", "name", "two", 10),
		tcell("node:3", "attr", "name", "three", 10),
		tcell("idx:n", "p", "1", "", 5),
		tcell("idx:n", "p", "2", "", 5),
		tcell("idx:n", "p", "3", "", 5),
	}
	base := map[string]string{
		TermIndexCount:                 "1",
		TermIndexTermPrefix + "0":      "idx:n",
		TermIndexPrimaryPrefix:         "node:",
		TermIndexNumericLowerSet:       "true",
		TermIndexNumericLower:          "1",
		TermIndexNumericUpperSet:       "true",
		TermIndexNumericUpper:          "3",
		TermIndexNumericLowerInclusive: "false",
		TermIndexNumericUpperInclusive: "false",
	}
	got := runTermIndex(t, cells, base, InfiniteRange())
	if fmt.Sprint(rowsOf(got)) != fmt.Sprint([]string{"node:2"}) {
		t.Fatalf("exclusive rows: got %v want [node:2]", rowsOf(got))
	}
	base[TermIndexNumericLowerInclusive] = "true"
	base[TermIndexNumericUpperInclusive] = "true"
	got = runTermIndex(t, cells, base, InfiniteRange())
	if fmt.Sprint(rowsOf(got)) != fmt.Sprint([]string{"node:1", "node:2", "node:3"}) {
		t.Fatalf("inclusive rows: got %v want [node:1 node:2 node:3]", rowsOf(got))
	}
}

// TestTermIndex_RangeBoundsOutput verifies the requested Seek range bounds the
// emitted primary rows: even though dev∪ml resolves {1,2,3}, a range over
// node:2 only yields node:2's cells.
func TestTermIndex_RangeBoundsOutput(t *testing.T) {
	r := Range{
		Start:          &wire.Key{Row: []byte("node:2")},
		StartInclusive: true,
		End:            &wire.Key{Row: []byte("node:3")},
		EndInclusive:   false,
	}
	got := runTermIndex(t, graphFixture(), devMlOpts(), r)
	wantRows := []string{"node:2"}
	if fmt.Sprint(rowsOf(got)) != fmt.Sprint(wantRows) {
		t.Fatalf("rows: got %v want %v", rowsOf(got), wantRows)
	}
}

// TestTermIndex_IDSourceValue resolves the primary id from the posting cell
// VALUE instead of the column qualifier.
func TestTermIndex_IDSourceValue(t *testing.T) {
	cells := []Cell{
		tcell("node:1", "attr", "name", "alice", 10),
		tcell("node:2", "attr", "name", "bob", 10),
		// postings carry the id in the value, qualifier is a meaningless seq no.
		tcell("idx:dev", "p", "0001", "1", 5),
		tcell("idx:dev", "p", "0002", "2", 5),
	}
	opts := map[string]string{
		TermIndexCount:            "1",
		TermIndexTermPrefix + "0": "idx:dev",
		TermIndexPrimaryPrefix:    "node:",
		TermIndexIDSource:         "value",
	}
	got := runTermIndex(t, cells, opts, InfiniteRange())
	if fmt.Sprint(rowsOf(got)) != fmt.Sprint([]string{"node:1", "node:2"}) {
		t.Fatalf("rows: got %v", rowsOf(got))
	}
}

// TestTermIndex_PostingCFRestriction ensures only cells with the configured
// posting column family count as postings; other cells in the posting row are
// ignored.
func TestTermIndex_PostingCFRestriction(t *testing.T) {
	cells := []Cell{
		tcell("node:1", "attr", "name", "alice", 10),
		tcell("node:2", "attr", "name", "bob", 10),
		tcell("idx:dev", "p", "1", "", 5),    // valid posting
		tcell("idx:dev", "meta", "2", "", 5), // wrong cf — must be ignored
	}
	opts := map[string]string{
		TermIndexCount:            "1",
		TermIndexTermPrefix + "0": "idx:dev",
		TermIndexPrimaryPrefix:    "node:",
		TermIndexPostingCF:        "p",
	}
	got := runTermIndex(t, cells, opts, InfiniteRange())
	if fmt.Sprint(rowsOf(got)) != fmt.Sprint([]string{"node:1"}) {
		t.Fatalf("rows: got %v want [node:1] (meta cell should be ignored)", rowsOf(got))
	}
}

// TestTermIndex_MissingTermAndPrimary verifies graceful handling: a term row
// with no postings, and a posting referencing a non-existent primary row,
// both contribute nothing rather than erroring.
func TestTermIndex_MissingTermAndPrimary(t *testing.T) {
	cells := []Cell{
		tcell("node:1", "attr", "name", "alice", 10),
		tcell("idx:dev", "p", "1", "", 5),
		tcell("idx:dev", "p", "99", "", 5), // node:99 does not exist
	}
	opts := map[string]string{
		TermIndexCount:            "2",
		TermIndexTermPrefix + "0": "idx:dev",
		TermIndexTermPrefix + "1": "idx:absent", // no postings
		TermIndexPrimaryPrefix:    "node:",
	}
	got := runTermIndex(t, cells, opts, InfiniteRange())
	if fmt.Sprint(rowsOf(got)) != fmt.Sprint([]string{"node:1"}) {
		t.Fatalf("rows: got %v want [node:1]", rowsOf(got))
	}
}

// TestTermIndex_DeterministicOrder pins down that output is a deterministic,
// globally-sorted function of input regardless of the order term rows are
// listed or postings are discovered — the property the shadow oracle relies
// on for parity. We run the same fixture with the two term rows in both
// orders and require byte-identical output.
func TestTermIndex_DeterministicOrder(t *testing.T) {
	forward := runTermIndex(t, graphFixture(), map[string]string{
		TermIndexCount:            "2",
		TermIndexTermPrefix + "0": "idx:dev",
		TermIndexTermPrefix + "1": "idx:ml",
		TermIndexPrimaryPrefix:    "node:",
	}, InfiniteRange())
	reverse := runTermIndex(t, graphFixture(), map[string]string{
		TermIndexCount:            "2",
		TermIndexTermPrefix + "0": "idx:ml",
		TermIndexTermPrefix + "1": "idx:dev",
		TermIndexPrimaryPrefix:    "node:",
	}, InfiniteRange())

	if len(forward) != len(reverse) {
		t.Fatalf("length differs: %d vs %d", len(forward), len(reverse))
	}
	for i := range forward {
		if !forward[i].Key.Equal(reverse[i].Key) || string(forward[i].Value) != string(reverse[i].Value) {
			t.Fatalf("cell %d differs by term order: %s vs %s",
				i, cellToString(forward[i]), cellToString(reverse[i]))
		}
	}
}

// TestTermIndex_DeepCopyReSeek verifies the iterator is re-seekable via
// DeepCopy: the copy resolves the same primaries independently.
func TestTermIndex_DeepCopyReSeek(t *testing.T) {
	leaf := NewSliceSource(sortedSlice(graphFixture()))
	if err := leaf.Init(nil, nil, IteratorEnvironment{Scope: ScopeScan}); err != nil {
		t.Fatalf("leaf init: %v", err)
	}
	it := NewTermIndexIterator()
	if err := it.Init(leaf, devMlOpts(), IteratorEnvironment{Scope: ScopeScan}); err != nil {
		t.Fatalf("init: %v", err)
	}
	cp := it.DeepCopy(IteratorEnvironment{Scope: ScopeScan})
	if err := cp.Seek(InfiniteRange(), nil, false); err != nil {
		t.Fatalf("copy seek: %v", err)
	}
	var n int
	for cp.HasTop() {
		n++
		if err := cp.Next(); err != nil {
			t.Fatalf("copy next: %v", err)
		}
	}
	if n != 4 {
		t.Fatalf("deep copy emitted %d cells, want 4", n)
	}
}

// TestTermIndex_BadOptions checks Init rejects malformed configuration.
func TestTermIndex_BadOptions(t *testing.T) {
	leaf := NewSliceSource(nil)
	_ = leaf.Init(nil, nil, IteratorEnvironment{})
	cases := []map[string]string{
		{TermIndexCount: "notanumber"},
		{TermIndexCount: "2", TermIndexTermPrefix + "0": "a"}, // missing term.1
		{TermIndexCount: "1", TermIndexTermPrefix + "0": "a", TermIndexIDSource: "bogus"},
	}
	for i, opts := range cases {
		it := NewTermIndexIterator()
		if err := it.Init(leaf, opts, IteratorEnvironment{}); err == nil {
			t.Errorf("case %d (%v): expected Init error, got nil", i, opts)
		}
	}
}

func cellToString(c Cell) string {
	return fmt.Sprintf("%s/%s:%s=%s@%s",
		c.Key.Row, c.Key.ColumnFamily, c.Key.ColumnQualifier, c.Value,
		strconv.FormatInt(c.Key.Timestamp, 10))
}

func cellsToStrings(cells []Cell) []string {
	out := make([]string, len(cells))
	for i, c := range cells {
		out[i] = cellToString(c)
	}
	return out
}
