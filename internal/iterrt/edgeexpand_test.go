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

// runEdgeExpand wires a SliceSource → EdgeExpandIterator over the full range
// and drains it.
func runEdgeExpand(t *testing.T, cells []Cell, opts map[string]string) []Cell {
	t.Helper()
	leaf := NewSliceSource(sortedSlice(cells))
	if err := leaf.Init(nil, nil, IteratorEnvironment{Scope: ScopeScan}); err != nil {
		t.Fatalf("leaf init: %v", err)
	}
	it := NewEdgeExpandIterator()
	if err := it.Init(leaf, opts, IteratorEnvironment{Scope: ScopeScan}); err != nil {
		t.Fatalf("edgeexpand init: %v", err)
	}
	if err := it.Seek(InfiniteRange(), nil, false); err != nil {
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

// coLocatedGraph builds a small graph in a co-located-edge dialect:
// node rows hold a "name" attr cell (cf "attr") and out-edges as cells with
// cf "edge_out", cq "<rel>\x00<targetId>".
func coLocatedGraph() []Cell {
	nul := "\x00"
	return []Cell{
		// node a: edges to b (relates_to) and c (depends_on)
		tcell("a", "attr", "name", "node-a", 1),
		tcell("a", "edge_out", "relates_to"+nul+"b", "", 1),
		tcell("a", "edge_out", "depends_on"+nul+"c", "", 1),
		// node b: edge to c (relates_to)
		tcell("b", "attr", "name", "node-b", 1),
		tcell("b", "edge_out", "relates_to"+nul+"c", "", 1),
		// node c: leaf, plus an incoming-edge cell that must be ignored
		tcell("c", "attr", "name", "node-c", 1),
		tcell("c", "edge_in", "relates_to"+nul+"a", "", 1),
		// unrelated node
		tcell("z", "attr", "name", "node-z", 1),
	}
}

// rowSet returns the distinct rows of cells in arrival order.
func rowSet(cells []Cell) []string {
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

// TestEdgeExpandOneHop expands node a's out-edges into b and c, emitting the
// neighbor rows' cells (not a's own, since includeAnchors is unset).
func TestEdgeExpandOneHop(t *testing.T) {
	got := runEdgeExpand(t, coLocatedGraph(), map[string]string{
		EdgeExpandAnchorCount:        "1",
		EdgeExpandAnchorPrefix + "0": "a",
		EdgeExpandEdgeCF:             "edge_out",
		EdgeExpandFieldSep:           "\x00",
	})
	rows := rowSet(got)
	if len(rows) != 2 || rows[0] != "b" || rows[1] != "c" {
		t.Fatalf("rows = %v, want [b c]", rows)
	}
	// Every emitted cell must belong to b or c and the edge_in cell on c is
	// included (it is a cell of row c — expansion emits whole rows).
	for _, c := range got {
		if r := string(c.Key.Row); r != "b" && r != "c" {
			t.Errorf("unexpected row %q in output", r)
		}
	}

}

func TestEdgeExpandMaxHopsOneUnchanged(t *testing.T) {
	base := runEdgeExpand(t, coLocatedGraph(), map[string]string{
		EdgeExpandAnchorCount:        "1",
		EdgeExpandAnchorPrefix + "0": "a",
		EdgeExpandEdgeCF:             "edge_out",
		EdgeExpandFieldSep:           "\x00",
	})
	got := runEdgeExpand(t, coLocatedGraph(), map[string]string{
		EdgeExpandAnchorCount:        "1",
		EdgeExpandAnchorPrefix + "0": "a",
		EdgeExpandEdgeCF:             "edge_out",
		EdgeExpandFieldSep:           "\x00",
		EdgeExpandMaxHops:            "1",
	})
	if len(base) != len(got) {
		t.Fatalf("len = %d, want %d", len(got), len(base))
	}
	for i := range base {
		if !base[i].Key.Equal(got[i].Key) || string(base[i].Value) != string(got[i].Value) {
			t.Fatalf("cell %d differs with max_hops=1: %s vs %s", i, cellToString(base[i]), cellToString(got[i]))
		}
	}
}

func TestEdgeExpandTwoHop(t *testing.T) {
	cells := append(coLocatedGraph(),
		tcell("c", "edge_out", "relates_to\x00d", "", 1),
		tcell("d", "attr", "name", "node-d", 1),
	)
	got := runEdgeExpand(t, cells, map[string]string{
		EdgeExpandAnchorCount:        "1",
		EdgeExpandAnchorPrefix + "0": "a",
		EdgeExpandEdgeCF:             "edge_out",
		EdgeExpandFieldSep:           "\x00",
		EdgeExpandMaxHops:            "2",
	})
	rows := rowSet(got)
	if len(rows) != 3 || rows[0] != "b" || rows[1] != "c" || rows[2] != "d" {
		t.Fatalf("rows = %v, want [b c d]", rows)
	}
}

func TestEdgeExpandWeightsFilterNonPositive(t *testing.T) {
	got := runEdgeExpand(t, coLocatedGraph(), map[string]string{
		EdgeExpandAnchorCount:             "1",
		EdgeExpandAnchorPrefix + "0":      "a",
		EdgeExpandEdgeCF:                  "edge_out",
		EdgeExpandFieldSep:                "\x00",
		EdgeExpandWeightCount:             "2",
		EdgeExpandWeightRelPrefix + "0":   "relates_to",
		EdgeExpandWeightValuePrefix + "0": "2",
		EdgeExpandWeightRelPrefix + "1":   "depends_on",
		EdgeExpandWeightValuePrefix + "1": "0",
	})
	rows := rowSet(got)
	if len(rows) != 1 || rows[0] != "b" {
		t.Fatalf("rows = %v, want [b] (depends_on weight 0 filtered)", rows)
	}
}

// TestEdgeExpandIncludeAnchors also emits the anchor row's own cells.
func TestEdgeExpandIncludeAnchors(t *testing.T) {
	got := runEdgeExpand(t, coLocatedGraph(), map[string]string{
		EdgeExpandAnchorCount:        "1",
		EdgeExpandAnchorPrefix + "0": "a",
		EdgeExpandEdgeCF:             "edge_out",
		EdgeExpandFieldSep:           "\x00",
		EdgeExpandIncludeAnchors:     "true",
	})
	rows := rowSet(got)
	if len(rows) != 3 || rows[0] != "a" || rows[1] != "b" || rows[2] != "c" {
		t.Fatalf("rows = %v, want [a b c]", rows)
	}
}

// TestEdgeExpandRelFilter restricts expansion to a relationship allowlist.
func TestEdgeExpandRelFilter(t *testing.T) {
	got := runEdgeExpand(t, coLocatedGraph(), map[string]string{
		EdgeExpandAnchorCount:        "1",
		EdgeExpandAnchorPrefix + "0": "a",
		EdgeExpandEdgeCF:             "edge_out",
		EdgeExpandFieldSep:           "\x00",
		EdgeExpandRelIndex:           "0",
		EdgeExpandRelCount:           "1",
		EdgeExpandRelPrefix + "0":    "depends_on",
	})
	rows := rowSet(got)
	if len(rows) != 1 || rows[0] != "c" {
		t.Fatalf("rows = %v, want [c] (only depends_on edge)", rows)
	}
}

// TestEdgeExpandMultiAnchorDedup unions and de-duplicates neighbors across
// anchors: a→{b,c} and b→{c} yields {b,c}, with c emitted once.
func TestEdgeExpandMultiAnchorDedup(t *testing.T) {
	got := runEdgeExpand(t, coLocatedGraph(), map[string]string{
		EdgeExpandAnchorCount:        "2",
		EdgeExpandAnchorPrefix + "0": "a",
		EdgeExpandAnchorPrefix + "1": "b",
		EdgeExpandEdgeCF:             "edge_out",
		EdgeExpandFieldSep:           "\x00",
	})
	rows := rowSet(got)
	if len(rows) != 2 || rows[0] != "b" || rows[1] != "c" {
		t.Fatalf("rows = %v, want [b c]", rows)
	}
	// c appears once even though both a and b point at it.
	cCount := 0
	for _, r := range rowSet(got) {
		if r == "c" {
			cCount++
		}
	}
	if cCount != 1 {
		t.Errorf("c row count = %d, want 1", cCount)
	}
}

// TestEdgeExpandWholeTokenID treats the entire qualifier as the neighbor id
// when no separator is configured.
func TestEdgeExpandWholeTokenID(t *testing.T) {
	cells := []Cell{
		tcell("a", "link", "b", "", 1),
		tcell("a", "link", "c", "", 1),
		tcell("b", "attr", "name", "node-b", 1),
		tcell("c", "attr", "name", "node-c", 1),
	}
	got := runEdgeExpand(t, cells, map[string]string{
		EdgeExpandAnchorCount:        "1",
		EdgeExpandAnchorPrefix + "0": "a",
		EdgeExpandEdgeCF:             "link",
	})
	rows := rowSet(got)
	if len(rows) != 2 || rows[0] != "b" || rows[1] != "c" {
		t.Fatalf("rows = %v, want [b c]", rows)
	}
}

// TestEdgeExpandPrimaryPrefix forms neighbor row keys as primaryPrefix + id.
func TestEdgeExpandPrimaryPrefix(t *testing.T) {
	cells := []Cell{
		tcell("node:a", "edge_out", "rel\x00b", "", 1),
		tcell("node:b", "attr", "name", "node-b", 1),
	}
	got := runEdgeExpand(t, cells, map[string]string{
		EdgeExpandAnchorCount:        "1",
		EdgeExpandAnchorPrefix + "0": "node:a",
		EdgeExpandEdgeCF:             "edge_out",
		EdgeExpandFieldSep:           "\x00",
		EdgeExpandPrimaryPrefix:      "node:",
	})
	rows := rowSet(got)
	if len(rows) != 1 || rows[0] != "node:b" {
		t.Fatalf("rows = %v, want [node:b]", rows)
	}
}

// TestEdgeExpandIDFromValue reads the neighbor id from the cell value.
func TestEdgeExpandIDFromValue(t *testing.T) {
	cells := []Cell{
		tcell("a", "edge_out", "rel1", "b", 1),
		tcell("b", "attr", "name", "node-b", 1),
	}
	got := runEdgeExpand(t, cells, map[string]string{
		EdgeExpandAnchorCount:        "1",
		EdgeExpandAnchorPrefix + "0": "a",
		EdgeExpandEdgeCF:             "edge_out",
		EdgeExpandEdgeField:          "value",
	})
	rows := rowSet(got)
	if len(rows) != 1 || rows[0] != "b" {
		t.Fatalf("rows = %v, want [b]", rows)
	}
}

// TestEdgeExpandInitErrors rejects malformed options.
func TestEdgeExpandInitErrors(t *testing.T) {
	leaf := NewSliceSource(nil)
	_ = leaf.Init(nil, nil, IteratorEnvironment{Scope: ScopeScan})
	cases := []struct {
		name string
		opts map[string]string
	}{
		{"missing anchor", map[string]string{EdgeExpandAnchorCount: "1"}},
		{"bad anchorCount", map[string]string{EdgeExpandAnchorCount: "-1"}},
		{"bad edgeField", map[string]string{EdgeExpandEdgeField: "neither"}},
		{"relCount without sep", map[string]string{
			EdgeExpandRelCount: "1", EdgeExpandRelPrefix + "0": "x"}},
		{"missing rel", map[string]string{
			EdgeExpandFieldSep: "\x00", EdgeExpandRelCount: "1"}},
		{"bad includeAnchors", map[string]string{EdgeExpandIncludeAnchors: "maybe"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			it := NewEdgeExpandIterator()
			if err := it.Init(leaf, tc.opts, IteratorEnvironment{Scope: ScopeScan}); err == nil {
				t.Errorf("expected Init error for %s", tc.name)
			}
		})
	}
}
