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
	"sort"
	"testing"

	"github.com/phrocker/shoal-oss/internal/cclient"
	"github.com/phrocker/shoal-oss/internal/engine"
	"github.com/phrocker/shoal-oss/internal/iterrt"
)

// cellKey is a string form of a cell's identity for set comparison.
func cellKey(row, cf, cq string, val []byte) string {
	return fmt.Sprintf("%s|%s|%s|%x", row, cf, cq, val)
}

// scanRowCells returns the set of cells in a single row via the per-row
// Scan path — the reference the batch path must match.
func scanRowCells(t *testing.T, eng *engine.Engine, table string, row []byte, opts engine.ScanOptions) map[string]bool {
	t.Helper()
	start := &iterrt.Key{Row: row}
	end := &iterrt.Key{Row: append(append([]byte{}, row...), 0x00)}
	r := iterrt.Range{Start: start, StartInclusive: true, End: end, EndInclusive: false}
	sc, err := eng.Scan(table, r, opts)
	if err != nil {
		t.Fatal(err)
	}
	defer sc.Close()
	out := map[string]bool{}
	for sc.Next() {
		k := sc.Key()
		out[cellKey(string(k.Row), string(k.ColumnFamily), string(k.ColumnQualifier), sc.Value())] = true
		if err := sc.Advance(); err != nil {
			t.Fatal(err)
		}
	}
	return out
}

// TestLookupRows_MatchesScan verifies the batch LookupRows fast path
// returns exactly the same cells as N independent per-row Scans, across
// both single-tablet and multi-tablet tables, with and without a column
// family filter.
func TestLookupRows_MatchesScan(t *testing.T) {
	cases := []struct {
		name   string
		splits [][]byte
	}{
		{"single-tablet", nil},
		{"multi-tablet", engine.PrefixSplit("entity:")},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir, err := os.MkdirTemp("", "shoal-lookup-*")
			if err != nil {
				t.Fatal(err)
			}
			defer os.RemoveAll(dir)
			eng, err := engine.Open(dir, engine.Options{})
			if err != nil {
				t.Fatal(err)
			}
			defer eng.Close()
			topts := engine.TableOptions{}
			if tc.splits != nil {
				topts.Splits = tc.splits
			}
			if err := eng.CreateTable("g", topts); err != nil {
				t.Fatal(err)
			}

			// Seed a small graph: each node has props + a few edges.
			g := graphSpec{nodes: 200, degree: 5, seed: 7}
			const batch = 50
			for i := 0; i < g.nodes; i += batch {
				ms := make([]*cclient.Mutation, 0, batch)
				for j := i; j < i+batch && j < g.nodes; j++ {
					ms = append(ms, makeGraphMutation(fmt.Sprintf("entity:%08d", j), g.neighbors(j), 32))
				}
				if err := eng.Write("g", ms); err != nil {
					t.Fatal(err)
				}
			}
			eng.Flush("g")

			// Look up a mix of rows (some adjacent, some not, plus a
			// missing one) and compare against per-row Scan.
			ids := []int{3, 3, 198, 0, 77, 12, 199, 5000 /* missing */}
			rows := make([][]byte, len(ids))
			for i, id := range ids {
				rows[i] = []byte(fmt.Sprintf("entity:%08d", id))
			}

			for _, opts := range []engine.ScanOptions{
				{}, // all cells
				{ColumnFamilies: [][]byte{[]byte("edge")}, ColumnFamiliesInclusive: true},
				{ColumnFamilies: [][]byte{[]byte("props")}, ColumnFamiliesInclusive: true},
			} {
				// Reference: per-row Scan.
				want := map[int]map[string]bool{}
				for i, row := range rows {
					want[i] = scanRowCells(t, eng, "g", row, opts)
				}

				// Batch.
				got := map[int]map[string]bool{}
				err := eng.LookupRows("g", rows, opts, func(idx int, k *iterrt.Key, v []byte) {
					if got[idx] == nil {
						got[idx] = map[string]bool{}
					}
					got[idx][cellKey(string(k.Row), string(k.ColumnFamily), string(k.ColumnQualifier), v)] = true
				})
				if err != nil {
					t.Fatal(err)
				}

				for i := range rows {
					if !setsEqual(want[i], got[i]) {
						t.Errorf("opts=%v row idx %d (%s): cell sets differ\n want=%v\n got =%v",
							opts.ColumnFamilies, i, rows[i], keys(want[i]), keys(got[i]))
					}
				}
			}
		})
	}
}

func setsEqual(a, b map[string]bool) bool {
	if len(a) != len(b) {
		return false
	}
	for k := range a {
		if !b[k] {
			return false
		}
	}
	return true
}

func keys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
