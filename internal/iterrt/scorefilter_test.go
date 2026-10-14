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
	"encoding/base64"
	"math"
	"testing"
)

func runScoreFilter(t *testing.T, cells []Cell, opts map[string]string) []Cell {
	t.Helper()
	leaf := NewSliceSource(sortedSlice(cells))
	if err := leaf.Init(nil, nil, IteratorEnvironment{Scope: ScopeScan}); err != nil {
		t.Fatalf("leaf init: %v", err)
	}
	it := NewScoreFilterIterator()
	if err := it.Init(leaf, opts, IteratorEnvironment{Scope: ScopeScan}); err != nil {
		t.Fatalf("scorefilter init: %v", err)
	}
	if err := it.Seek(InfiniteRange(), nil, false); err != nil {
		t.Fatalf("seek: %v", err)
	}
	var got []Cell
	for it.HasTop() {
		got = append(got, Cell{Key: it.GetTopKey().Clone(), Value: append([]byte(nil), it.GetTopValue()...)})
		if err := it.Next(); err != nil {
			t.Fatalf("next: %v", err)
		}
	}
	return got
}

func TestScoreFilterVectorSimTopK(t *testing.T) {
	got := runScoreFilter(t, []Cell{
		embCell("a", "score", 1, 0),
		embCell("b", "score", 1, 1),
		embCell("c", "other", 1, 0),
		embCell("d", "score", 0, 1),
	}, map[string]string{
		ScoreFilterMethod:  "vector_sim",
		ScoreFilterQuery:   base64.StdEncoding.EncodeToString(packBE(1, 0)),
		ScoreFilterTopK:    "2",
		ScoreFilterScoreCF: "score",
	})
	if len(got) != 2 || string(got[0].Key.Row) != "a" || string(got[1].Key.Row) != "b" {
		t.Fatalf("rows = %v, want [a b]", rowSet(got))
	}
	if s := unpackScore(t, got[0].Value); math.Abs(float64(s-1)) > 1e-5 {
		t.Fatalf("top score = %v, want 1", s)
	}
}

func TestScoreFilterTimeDecay(t *testing.T) {
	got := runScoreFilter(t, []Cell{
		tcell("new", "score", "x", "", 1000),
		tcell("half", "score", "x", "", 0),
		tcell("future", "score", "x", "", 1100),
	}, map[string]string{
		ScoreFilterMethod:            "time_decay",
		ScoreFilterTimestampAnchorMs: "1000",
		ScoreFilterHalfLifeMs:        "1000",
	})
	if len(got) != 3 {
		t.Fatalf("got %d cells, want 3", len(got))
	}
	if string(got[0].Key.Row) != "future" || string(got[1].Key.Row) != "new" || string(got[2].Key.Row) != "half" {
		t.Fatalf("rows = %v, want [future new half]", rowSet(got))
	}
	if s := unpackScore(t, got[2].Value); math.Abs(float64(s-0.5)) > 1e-5 {
		t.Fatalf("half-life score = %v, want 0.5", s)
	}
}

func TestScoreFilterLinearAndTieBreak(t *testing.T) {
	got := runScoreFilter(t, []Cell{
		embCell("b", "score", 2),
		embCell("a", "score", 2),
		embCell("c", "score", 1),
	}, map[string]string{
		ScoreFilterMethod:            "linear",
		ScoreFilterParamCount:        "1",
		ScoreFilterParamPrefix + "0": "2",
		ScoreFilterTopK:              "2",
	})
	if len(got) != 2 || string(got[0].Key.Row) != "a" || string(got[1].Key.Row) != "b" {
		t.Fatalf("rows = %v, want deterministic tie [a b]", rowSet(got))
	}
	if s := unpackScore(t, got[0].Value); math.Abs(float64(s-4)) > 1e-5 {
		t.Fatalf("linear score = %v, want 4", s)
	}
}
