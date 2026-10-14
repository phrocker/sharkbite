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

func runAgg(t *testing.T, cells []kv, opts map[string]string) []kv {
	t.Helper()
	it, err := BuildStack(newSliceSource(cells...), []IterSpec{{Name: IterGraphAggregation, Options: opts}}, IteratorEnvironment{Scope: ScopeScan})
	if err != nil {
		t.Fatalf("BuildStack: %v", err)
	}
	if err := it.Seek(InfiniteRange(), nil, false); err != nil {
		t.Fatalf("Seek: %v", err)
	}
	got, err := drain(it)
	if err != nil {
		t.Fatalf("drain: %v", err)
	}
	return got
}

func TestGraphAggregation_Ops(t *testing.T) {
	cells := []kv{{mk("r1", "m", "score", "", 3), []byte("1")}, {mk("r1", "m", "score2", "", 2), []byte("3")}, {mk("r1", "m", "score3", "", 1), []byte("5")}}
	cases := map[string]string{"count": "3", "sum": "9", "min": "1", "max": "5", "avg": "3"}
	for op, want := range cases {
		got := runAgg(t, cells, map[string]string{GraphAggregationOp: op})
		if len(got) != 1 {
			t.Fatalf("%s: got %d results", op, len(got))
		}
		if string(got[0].v) != want {
			t.Fatalf("%s: got %q want %q", op, got[0].v, want)
		}
	}
}

func TestGraphAggregation_GroupingAndFiltering(t *testing.T) {
	cells := []kv{{mk("team:a", "m", "score", "", 1), []byte("2")}, {mk("team:b", "m", "score", "", 1), []byte("3")}, {mk("other:c", "x", "score", "", 1), []byte("100")}}
	got := runAgg(t, cells, map[string]string{GraphAggregationOp: "sum", GraphAggregationGroupBy: "rowPrefix", GraphAggregationValueCF: "m"})
	if len(got) != 1 {
		t.Fatalf("got %d groups, want 1", len(got))
	}
	if string(got[0].k.ColumnQualifier) != "team" || string(got[0].v) != "5" {
		t.Fatalf("got group %q value %q", got[0].k.ColumnQualifier, got[0].v)
	}
}
