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

package engine_test

import (
	"fmt"
	"os"
	"testing"

	"github.com/phrocker/shoal-oss/internal/cclient"
	"github.com/phrocker/shoal-oss/internal/engine"
	"github.com/phrocker/shoal-oss/internal/iterrt"
	"github.com/phrocker/shoal-oss/internal/tablet"
)

// scanEdges returns the set of resolved (cq|value) edge cells for row via
// the reference Scan path (default versioning + delete suppression),
// restricted to the edge column family.
func scanEdges(t *testing.T, eng *engine.Engine, table string, row []byte) map[string]bool {
	t.Helper()
	opts := engine.ScanOptions{
		ColumnFamilies:          [][]byte{[]byte("edge")},
		ColumnFamiliesInclusive: true,
		// Resolve like Neighbors does: suppress tombstones (deleting) and
		// keep only the newest version per column (versioning). Without
		// this stack a raw Scan returns every version + tombstone.
		Stack: []iterrt.IterSpec{
			{Name: iterrt.IterDeleting, Options: map[string]string{iterrt.DeletingOptionPropagate: "false"}},
			{Name: iterrt.IterVersioning},
		},
	}
	cells := scanRowCells(t, eng, table, row, opts)
	out := map[string]bool{}
	for k := range cells {
		out[k] = true
	}
	return out
}

// neighborSet returns the (row|edge|cq|value) set produced by Neighbors,
// keyed identically to scanRowCells so the two can be compared directly.
func neighborSet(row string, ns []engine.Neighbor) map[string]bool {
	out := map[string]bool{}
	for _, n := range ns {
		out[cellKey(row, "edge", string(n.Target), n.Value)] = true
	}
	return out
}

// TestNeighbors_MatchesScan verifies the shoal.adjacency-backed Neighbors
// API returns exactly the resolved edges a Scan over (row, "edge") would,
// across multi-file (with updates + deletes), the un-flushed memtable, and
// both single- and multi-tablet layouts.
func TestNeighbors_MatchesScan(t *testing.T) {
	cases := []struct {
		name   string
		splits [][]byte
	}{
		{"single-tablet", nil},
		{"multi-tablet", engine.PrefixSplit("entity:")},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir, err := os.MkdirTemp("", "shoal-neighbors-*")
			if err != nil {
				t.Fatal(err)
			}
			defer os.RemoveAll(dir)
			eng, err := engine.Open(dir, engine.Options{})
			if err != nil {
				t.Fatal(err)
			}
			defer eng.Close()

			topts := engine.TableOptions{
				TabletOptions: tablet.Options{AdjacencyEdgeCF: "edge"},
			}
			if tc.splits != nil {
				topts.Splits = tc.splits
			}
			if err := eng.CreateTable("g", topts); err != nil {
				t.Fatal(err)
			}

			id := func(i int) string { return fmt.Sprintf("entity:%08d", i) }
			edge := func(m *cclient.Mutation, target int, ts int64, weight string) {
				m.Put([]byte("edge"), []byte(id(target)), nil, ts, []byte(weight))
			}

			// File 1: base graph. entity:0 -> {1,2,3}; entity:1 -> {2};
			// entity:2 -> {3,4}; entity:5 -> {6}.
			var batch []*cclient.Mutation
			m0, _ := cclient.NewMutation([]byte(id(0)))
			edge(m0, 1, 100, "w1")
			edge(m0, 2, 100, "w2")
			edge(m0, 3, 100, "w3")
			m1, _ := cclient.NewMutation([]byte(id(1)))
			edge(m1, 2, 100, "w2")
			m2, _ := cclient.NewMutation([]byte(id(2)))
			edge(m2, 3, 100, "w3")
			edge(m2, 4, 100, "w4")
			m5, _ := cclient.NewMutation([]byte(id(5)))
			edge(m5, 6, 100, "w6")
			batch = append(batch, m0, m1, m2, m5)
			if err := eng.Write("g", batch); err != nil {
				t.Fatal(err)
			}
			eng.Flush("g")

			// File 2: update entity:0 -> 2 weight (newer ts), delete
			// entity:0 -> 1, add entity:2 -> 7.
			batch = nil
			u0, _ := cclient.NewMutation([]byte(id(0)))
			u0.Put([]byte("edge"), []byte(id(2)), nil, 200, []byte("w2-new"))
			u0.Delete([]byte("edge"), []byte(id(1)), nil, 200)
			u2, _ := cclient.NewMutation([]byte(id(2)))
			edge(u2, 7, 200, "w7")
			batch = append(batch, u0, u2)
			if err := eng.Write("g", batch); err != nil {
				t.Fatal(err)
			}
			eng.Flush("g")

			// Memtable (un-flushed): entity:5 gets a new edge, entity:8
			// is brand new, entity:2 -> 4 is deleted.
			batch = nil
			mm5, _ := cclient.NewMutation([]byte(id(5)))
			edge(mm5, 9, 300, "w9")
			mm8, _ := cclient.NewMutation([]byte(id(8)))
			edge(mm8, 1, 300, "w1")
			mm2, _ := cclient.NewMutation([]byte(id(2)))
			mm2.Delete([]byte("edge"), []byte(id(4)), nil, 300)
			batch = append(batch, mm5, mm8, mm2)
			if err := eng.Write("g", batch); err != nil {
				t.Fatal(err)
			}

			rows := []int{0, 1, 2, 5, 8, 999 /* missing */}
			byteRows := make([][]byte, len(rows))
			for i, r := range rows {
				byteRows[i] = []byte(id(r))
			}

			got, err := eng.Neighbors("g", byteRows, []byte("edge"), engine.ScanOptions{})
			if err != nil {
				t.Fatal(err)
			}

			for i, r := range rows {
				want := scanEdges(t, eng, "g", byteRows[i])
				have := neighborSet(id(r), got[i])
				if !setsEqual(want, have) {
					t.Errorf("row %s: Neighbors != Scan\n want=%v\n got =%v",
						id(r), keys(want), keys(have))
				}
			}
		})
	}
}
