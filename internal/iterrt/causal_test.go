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
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strings"
	"testing"
)

type causalResultJSON struct {
	Chain []struct {
		VertexID string  `json:"vertex_id"`
		EdgeType *string `json:"edge_type"`
		Score    float32 `json:"score"`
		Role     string  `json:"role"`
	} `json:"chain"`
	Direction      string  `json:"direction"`
	CausalStrength float32 `json:"causal_strength"`
	HopCount       int     `json:"hop_count"`
}

func initCausal(t *testing.T, src SortedKeyValueIterator, opts map[string]string) *CausalInferenceIterator {
	t.Helper()
	it := NewCausalInferenceIterator()
	if err := it.Init(src, opts, IteratorEnvironment{Scope: ScopeScan}); err != nil {
		t.Fatalf("Init: %v", err)
	}
	if err := it.Seek(InfiniteRange(), nil, false); err != nil {
		t.Fatalf("Seek: %v", err)
	}
	return it
}

func causalQuery(vec ...float32) string {
	return base64.StdEncoding.EncodeToString(embed(vec...))
}

func sortCausalInput(cells []kv) []kv {
	out := append([]kv(nil), cells...)
	sort.Slice(out, func(i, j int) bool { return out[i].k.Compare(out[j].k) < 0 })
	return out
}

func readCausalResult(t *testing.T, it SortedKeyValueIterator) (kv, causalResultJSON) {
	t.Helper()
	got, err := drain(it)
	if err != nil {
		t.Fatalf("drain: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("result count = %d, want one terminal JSON cell", len(got))
	}
	var result causalResultJSON
	if err := json.Unmarshal(got[0].v, &result); err != nil {
		t.Fatalf("Unmarshal(%q): %v", got[0].v, err)
	}
	return got[0], result
}

func causalResultString(cell kv) string {
	return fmt.Sprintf("%x|%x|%x|%x|%d|%t|%s",
		cell.k.Row,
		cell.k.ColumnFamily,
		cell.k.ColumnQualifier,
		cell.k.ColumnVisibility,
		cell.k.Timestamp,
		cell.k.Deleted,
		cell.v)
}

func assertCausalClose(t *testing.T, name string, got, want float32) {
	t.Helper()
	if math.Abs(float64(got-want)) > 1e-6 {
		t.Fatalf("%s: got %.9g want %.9g", name, got, want)
	}
}

func unitFromDegrees(deg float64) []byte {
	rad := deg * math.Pi / 180
	return embed(float32(math.Cos(rad)), float32(math.Sin(rad)))
}

func TestCausalInference_ForwardTraceChoosesBestAndRoles(t *testing.T) {
	cells := sortCausalInput([]kv{
		{mk("A", "V", "_embedding", "visA", 10), embed(1, 0)},
		{mk("A", "E_link", "B", "", 11), []byte("{}")},
		{mk("A", "E_link", "C", "", 12), []byte("{}")},
		{mk("B", "V", "_embedding", "", 13), embed(0.8, 0)},
		{mk("B", "E_link", "D", "", 14), []byte("{}")},
		{mk("C", "V", "_embedding", "", 15), embed(0, 1)},
		{mk("D", "V", "_embedding", "", 16), embed(0.5, 0)},
	})
	cell, result := readCausalResult(t, initCausal(t, newSliceSource(cells...), map[string]string{
		CausalInferenceQuery:       causalQuery(1, 0),
		CausalInferenceStartVertex: "A",
		CausalInferenceMaxDepth:    "2",
		CausalInferenceThreshold:   "0.1",
	}))
	if string(cell.k.Row) != "A" || string(cell.k.ColumnFamily) != "V" || string(cell.k.ColumnQualifier) != "_causal" {
		t.Fatalf("result key = %+v", cell.k)
	}
	if string(cell.k.ColumnVisibility) != "visA" {
		t.Fatalf("result visibility = %q, want start embedding visibility", cell.k.ColumnVisibility)
	}
	if result.Direction != "forward" || result.HopCount != 3 {
		t.Fatalf("result direction/count = %q/%d", result.Direction, result.HopCount)
	}
	for i, want := range []struct {
		vertex string
		role   string
		edge   *string
	}{
		{"A", "cause", nil},
		{"B", "effect", ptr("link")},
		{"D", "effect", ptr("link")},
	} {
		got := result.Chain[i]
		if got.VertexID != want.vertex || got.Role != want.role ||
			(got.EdgeType == nil) != (want.edge == nil) ||
			(got.EdgeType != nil && *got.EdgeType != *want.edge) {
			t.Fatalf("hop %d = %+v", i, got)
		}
	}
	assertCausalClose(t, "B score", result.Chain[1].Score, 1)
	assertCausalClose(t, "D score", result.Chain[2].Score, 1)
	assertCausalClose(t, "strength", result.CausalStrength, 1)
}

func TestCausalInference_StrengthMultipliesHopScores(t *testing.T) {
	cells := sortCausalInput([]kv{
		{mk("A", "V", "_embedding", "", 1), embed(1, 0)},
		{mk("A", "E_link", "B", "", 2), []byte("{}")},
		{mk("B", "V", "_embedding", "", 3), embed(float32(math.Sqrt(0.25)), float32(math.Sqrt(0.75)))},
		{mk("B", "E_link", "C", "", 4), []byte("{}")},
		{mk("C", "V", "_embedding", "", 5), embed(float32(math.Sqrt(0.25)), float32(math.Sqrt(0.75)))},
	})
	_, result := readCausalResult(t, initCausal(t, newSliceSource(cells...), map[string]string{
		CausalInferenceQuery:       causalQuery(1, 0),
		CausalInferenceStartVertex: "A",
		CausalInferenceMaxDepth:    "2",
		CausalInferenceThreshold:   "0",
	}))
	if result.HopCount != 3 {
		t.Fatalf("hop count = %d, want 3", result.HopCount)
	}
	want := result.Chain[1].Score * result.Chain[2].Score
	assertCausalClose(t, "strength", result.CausalStrength, want)
	if result.CausalStrength == (result.Chain[1].Score+result.Chain[2].Score)/2 {
		t.Fatal("strength unexpectedly equals average of hop scores")
	}
}

func TestCausalInference_BackwardUsesInverseEdgesAndRoles(t *testing.T) {
	cells := sortCausalInput([]kv{
		{mk("A", "V", "_embedding", "", 1), embed(1, 0)},
		{mk("B", "V", "_embedding", "", 2), embed(1, 0)},
		{mk("B", "E_link", "wrong", "", 3), []byte("{}")},
		{mk("B", "EI_link", "A", "", 4), []byte("{}")},
		{mk("wrong", "V", "_embedding", "", 5), embed(1, 0)},
	})
	_, result := readCausalResult(t, initCausal(t, newSliceSource(cells...), map[string]string{
		CausalInferenceQuery:       causalQuery(1, 0),
		CausalInferenceStartVertex: "B",
		CausalInferenceDirection:   "backward",
		CausalInferenceMaxDepth:    "1",
		CausalInferenceThreshold:   "0",
	}))
	if result.Direction != "backward" || result.Chain[0].Role != "effect" {
		t.Fatalf("bad backward metadata: %+v", result)
	}
	if result.HopCount != 2 || result.Chain[1].VertexID != "A" || result.Chain[1].Role != "cause" {
		t.Fatalf("backward chain = %+v", result.Chain)
	}
}

func TestCausalInference_ForwardExcludesInversePrefix(t *testing.T) {
	cells := sortCausalInput([]kv{
		{mk("start", "V", "_embedding", "", 1), embed(1, 0)},
		{mk("start", "EI_bad", "aaaInverse", "", 2), []byte("{}")},
		{mk("start", "E_good", "forward", "", 3), []byte("{}")},
		{mk("forward", "V", "_embedding", "", 4), embed(1, 0)},
		{mk("aaaInverse", "V", "_embedding", "", 5), embed(1, 0)},
	})
	_, result := readCausalResult(t, initCausal(t, newSliceSource(cells...), map[string]string{
		CausalInferenceQuery:               causalQuery(1, 0),
		CausalInferenceStartVertex:         "start",
		CausalInferenceMaxDepth:            "1",
		CausalInferenceThreshold:           "0",
		CausalInferenceEdgeCFPrefix:        "E",
		CausalInferenceInverseEdgeCFPrefix: "EI",
	}))
	if result.HopCount != 2 || result.Chain[1].VertexID != "forward" {
		t.Fatalf("forward chain used inverse-prefixed edge: %+v", result.Chain)
	}
}

func TestCausalInference_BackwardUsesDifferentHiddenAlpha(t *testing.T) {
	cells := sortCausalInput([]kv{
		{mk("S", "V", "_embedding", "", 1), embed(0, 1)},
		{mk("S", "EI_link", "nearBackward", "", 2), []byte("{}")},
		{mk("S", "EI_link", "nearForward", "", 3), []byte("{}")},
		{mk("nearBackward", "V", "_embedding", "", 4), unitFromDegrees(14)},
		{mk("nearForward", "V", "_embedding", "", 5), unitFromDegrees(10)},
	})
	_, result := readCausalResult(t, initCausal(t, newSliceSource(cells...), map[string]string{
		CausalInferenceQuery:       causalQuery(1, 0),
		CausalInferenceStartVertex: "S",
		CausalInferenceDirection:   "backward",
		CausalInferenceMaxDepth:    "1",
		CausalInferenceThreshold:   "0",
	}))
	if result.HopCount != 2 || result.Chain[1].VertexID != "nearBackward" {
		t.Fatalf("backward alpha did not favor backward-angle candidate: %+v", result.Chain)
	}
}

func TestCausalInference_MissingEmbeddingGetsFallbackScore(t *testing.T) {
	cells := sortCausalInput([]kv{
		{mk("A", "V", "_embedding", "", 1), embed(1, 0)},
		{mk("A", "E_link", "missing", "", 2), []byte("{}")},
		{mk("A", "E_link", "orthogonal", "", 3), []byte("{}")},
		{mk("orthogonal", "V", "_embedding", "", 4), embed(0, 1)},
	})
	_, result := readCausalResult(t, initCausal(t, newSliceSource(cells...), map[string]string{
		CausalInferenceQuery:       causalQuery(1, 0),
		CausalInferenceStartVertex: "A",
		CausalInferenceMaxDepth:    "1",
		CausalInferenceThreshold:   "0.2",
	}))
	if result.HopCount != 2 || result.Chain[1].VertexID != "missing" {
		t.Fatalf("missing-embedding fallback was not selected: %+v", result.Chain)
	}
	assertCausalClose(t, "fallback score", result.Chain[1].Score, 0.21)
}

func TestCausalInference_DeterministicAcrossInputOrders(t *testing.T) {
	cells := []kv{
		{mk("A", "V", "_embedding", "", 10), embed(1, 0)},
		{mk("A", "E_link", "B", "", 11), []byte("{}")},
		{mk("B", "V", "_embedding", "", 13), embed(1, 0)},
		{mk("B", "V", "_embedding", "", 12), embed(0, 1)},
		{mk("B", "E_link", "C", "", 14), []byte("{}")},
		{mk("C", "V", "_embedding", "", 15), embed(1, 0)},
	}
	reversed := append([]kv(nil), cells...)
	for i, j := 0, len(reversed)-1; i < j; i, j = i+1, j-1 {
		reversed[i], reversed[j] = reversed[j], reversed[i]
	}

	var want string
	for i, input := range [][]kv{cells, reversed, sortCausalInput(cells)} {
		for run := 0; run < 10; run++ {
			cell, _ := readCausalResult(t, initCausal(t, newSliceSource(input...), map[string]string{
				CausalInferenceQuery:       causalQuery(1, 0),
				CausalInferenceStartVertex: "A",
				CausalInferenceMaxDepth:    "2",
				CausalInferenceThreshold:   "0",
			}))
			got := causalResultString(cell)
			if i == 0 && run == 0 {
				want = got
			} else if got != want {
				t.Fatalf("output differed for input %d run %d\nwant %s\ngot  %s", i, run, want, got)
			}
		}
	}
}

func TestCausalInference_NewestEmbeddingWins(t *testing.T) {
	cells := sortCausalInput([]kv{
		{mk("A", "V", "_embedding", "", 1), embed(1, 0)},
		{mk("A", "E_link", "B", "", 2), []byte("{}")},
		{mk("A", "E_link", "C", "", 3), []byte("{}")},
		{mk("B", "V", "_embedding", "", 20), embed(1, 0)},
		{mk("B", "V", "_embedding", "", 10), embed(0, 1)},
		{mk("C", "V", "_embedding", "", 4), embed(0.5, 0.8660254)},
	})
	_, result := readCausalResult(t, initCausal(t, newSliceSource(cells...), map[string]string{
		CausalInferenceQuery:       causalQuery(1, 0),
		CausalInferenceStartVertex: "A",
		CausalInferenceMaxDepth:    "1",
		CausalInferenceThreshold:   "0",
	}))
	if result.HopCount != 2 || result.Chain[1].VertexID != "B" {
		t.Fatalf("newest embedding was not kept: %+v", result.Chain)
	}
}

func TestCausalInference_TieBreaksByNeighbor(t *testing.T) {
	cells := sortCausalInput([]kv{
		{mk("S", "V", "_embedding", "", 1), embed(1, 0)},
		{mk("S", "E_z", "a", "", 2), []byte("{}")},
		{mk("S", "E_a", "b", "", 3), []byte("{}")},
		{mk("a", "V", "_embedding", "", 4), embed(1, 0)},
		{mk("b", "V", "_embedding", "", 5), embed(1, 0)},
	})
	_, result := readCausalResult(t, initCausal(t, newSliceSource(cells...), map[string]string{
		CausalInferenceQuery:       causalQuery(1, 0),
		CausalInferenceStartVertex: "S",
		CausalInferenceMaxDepth:    "1",
		CausalInferenceThreshold:   "0",
	}))
	if result.HopCount != 2 || result.Chain[1].VertexID != "a" {
		t.Fatalf("tie did not choose lexicographically first neighbor: %+v", result.Chain)
	}
}

func TestCausalInference_SkipsSelfLoopsAndCycles(t *testing.T) {
	cells := sortCausalInput([]kv{
		{mk("A", "V", "_embedding", "", 1), embed(1, 0)},
		{mk("A", "E_link", "A", "", 2), []byte("{}")},
		{mk("A", "E_link", "B", "", 3), []byte("{}")},
		{mk("B", "V", "_embedding", "", 4), embed(1, 0)},
		{mk("B", "E_link", "A", "", 5), []byte("{}")},
	})
	_, result := readCausalResult(t, initCausal(t, newSliceSource(cells...), map[string]string{
		CausalInferenceQuery:       causalQuery(1, 0),
		CausalInferenceStartVertex: "A",
		CausalInferenceMaxDepth:    "3",
		CausalInferenceThreshold:   "0",
	}))
	if result.HopCount != 2 || result.Chain[1].VertexID != "B" {
		t.Fatalf("self-loop/cycle was not skipped: %+v", result.Chain)
	}
}

func TestCausalInference_MaxDepthBoundary(t *testing.T) {
	cells := sortCausalInput([]kv{
		{mk("A", "V", "_embedding", "", 1), embed(1, 0)},
		{mk("A", "E_link", "B", "", 2), []byte("{}")},
		{mk("B", "V", "_embedding", "", 3), embed(1, 0)},
	})
	for _, tc := range []struct {
		depth string
		want  int
	}{
		{"0", 1},
		{"1", 2},
	} {
		t.Run(tc.depth, func(t *testing.T) {
			_, result := readCausalResult(t, initCausal(t, newSliceSource(cells...), map[string]string{
				CausalInferenceQuery:       causalQuery(1, 0),
				CausalInferenceStartVertex: "A",
				CausalInferenceMaxDepth:    tc.depth,
				CausalInferenceThreshold:   "0",
			}))
			if result.HopCount != tc.want {
				t.Fatalf("hop count = %d, want %d", result.HopCount, tc.want)
			}
		})
	}
}

func TestCausalInference_MaxVerticesReturnsStartOnly(t *testing.T) {
	cells := sortCausalInput([]kv{
		{mk("A", "V", "_embedding", "", 1), embed(1, 0)},
		{mk("A", "E_link", "B", "", 2), []byte("{}")},
		{mk("B", "V", "_embedding", "", 3), embed(1, 0)},
	})
	_, result := readCausalResult(t, initCausal(t, newSliceSource(cells...), map[string]string{
		CausalInferenceQuery:       causalQuery(1, 0),
		CausalInferenceStartVertex: "A",
		CausalInferenceMaxDepth:    "1",
		CausalInferenceThreshold:   "0",
		CausalInferenceMaxVertices: "1",
	}))
	if result.HopCount != 1 || result.Chain[0].VertexID != "A" {
		t.Fatalf("maxVertices guard result = %+v", result)
	}
}

func TestCausalInference_DerivedTimestamp(t *testing.T) {
	cells := sortCausalInput([]kv{
		{mk("A", "V", "_embedding", "", 10), embed(1, 0)},
		{mk("A", "E_link", "B", "", 21), []byte("{}")},
		{mk("B", "V", "_embedding", "", 20), embed(1, 0)},
	})
	cell, _ := readCausalResult(t, initCausal(t, newSliceSource(cells...), map[string]string{
		CausalInferenceQuery:       causalQuery(1, 0),
		CausalInferenceStartVertex: "A",
		CausalInferenceMaxDepth:    "1",
		CausalInferenceThreshold:   "0",
	}))
	if cell.k.Timestamp != 22 {
		t.Fatalf("timestamp = %d, want max source timestamp + 1", cell.k.Timestamp)
	}
}

func TestCausalInference_EmptyInputStillReturnsStart(t *testing.T) {
	cell, result := readCausalResult(t, initCausal(t, newSliceSource(), map[string]string{
		CausalInferenceQuery:       causalQuery(1, 0),
		CausalInferenceStartVertex: "missing",
	}))
	if cell.k.Timestamp != 1 || result.HopCount != 1 || result.Chain[0].VertexID != "missing" {
		t.Fatalf("empty result cell=%+v result=%+v", cell.k, result)
	}
}

func TestCausalInference_CustomSchemaAndEdgeType(t *testing.T) {
	cells := sortCausalInput([]kv{
		{mk("S", "VX", "emb", "vis", 1), embed(1, 0)},
		{mk("S", "OUT:keep", "A", "", 2), []byte("{}")},
		{mk("S", "OUT:skip", "B", "", 3), []byte("{}")},
		{mk("A", "VX", "emb", "", 4), embed(1, 0)},
		{mk("B", "VX", "emb", "", 5), embed(1, 0)},
	})
	cell, result := readCausalResult(t, initCausal(t, newSliceSource(cells...), map[string]string{
		CausalInferenceQuery:        causalQuery(1, 0),
		CausalInferenceStartVertex:  "S",
		CausalInferenceMaxDepth:     "1",
		CausalInferenceThreshold:    "0",
		CausalInferenceVertexCF:     "VX",
		CausalInferenceEmbeddingCQ:  "emb",
		CausalInferenceEdgeCFPrefix: "OUT:",
		CausalInferenceEdgeType:     "keep",
		CausalInferenceResultCF:     "R",
		CausalInferenceResultCQ:     "causal",
	}))
	if string(cell.k.ColumnFamily) != "R" || string(cell.k.ColumnQualifier) != "causal" ||
		string(cell.k.ColumnVisibility) != "vis" {
		t.Fatalf("custom output key = %+v", cell.k)
	}
	if result.HopCount != 2 || result.Chain[1].VertexID != "A" || *result.Chain[1].EdgeType != "keep" {
		t.Fatalf("custom schema result = %+v", result.Chain)
	}
}

func TestCausalInference_BadOptionsRejected(t *testing.T) {
	valid := map[string]string{
		CausalInferenceQuery:       causalQuery(1, 0),
		CausalInferenceStartVertex: "A",
	}
	tests := []struct {
		name   string
		option string
		value  string
	}{
		{"bad query base64", CausalInferenceQuery, "not-base64!"},
		{"empty query", CausalInferenceQuery, ""},
		{"empty start", CausalInferenceStartVertex, ""},
		{"bad direction", CausalInferenceDirection, "sideways"},
		{"bad maxDepth", CausalInferenceMaxDepth, "NaN"},
		{"negative maxDepth", CausalInferenceMaxDepth, "-1"},
		{"bad threshold", CausalInferenceThreshold, "NaNish"},
		{"bad maxVertices", CausalInferenceMaxVertices, "many"},
		{"negative maxVertices", CausalInferenceMaxVertices, "-1"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			opts := map[string]string{}
			for k, v := range valid {
				opts[k] = v
			}
			opts[tt.option] = tt.value
			err := NewCausalInferenceIterator().Init(newSliceSource(), opts, IteratorEnvironment{Scope: ScopeScan})
			if err == nil {
				t.Fatal("expected Init error")
			}
			if !strings.Contains(err.Error(), "CausalInferenceIterator") {
				t.Fatalf("Init error = %v", err)
			}
		})
	}
}

func TestCausalInference_BuildStackRegistered(t *testing.T) {
	cells := sortCausalInput([]kv{
		{mk("A", "V", "_embedding", "", 1), embed(1, 0)},
		{mk("A", "E_link", "B", "", 2), []byte("{}")},
		{mk("B", "V", "_embedding", "", 3), embed(1, 0)},
	})
	stack, err := BuildStack(newSliceSource(cells...), []IterSpec{
		{Name: IterCausalInference, Options: map[string]string{
			CausalInferenceQuery:       causalQuery(1, 0),
			CausalInferenceStartVertex: "A",
			CausalInferenceMaxDepth:    "1",
		}},
	}, IteratorEnvironment{Scope: ScopeScan})
	if err != nil {
		t.Fatalf("BuildStack: %v", err)
	}
	if err := stack.Seek(InfiniteRange(), nil, false); err != nil {
		t.Fatalf("Seek: %v", err)
	}
	_, result := readCausalResult(t, stack)
	if result.HopCount != 2 {
		t.Fatalf("hop count via stack = %d, want 2", result.HopCount)
	}
}

func TestCausalInference_DeepCopy(t *testing.T) {
	cells := sortCausalInput([]kv{
		{mk("A", "V", "_embedding", "", 1), embed(1, 0)},
		{mk("A", "E_link", "B", "", 2), []byte("{}")},
		{mk("B", "V", "_embedding", "", 3), embed(1, 0)},
	})
	original := NewCausalInferenceIterator()
	opts := map[string]string{
		CausalInferenceQuery:       causalQuery(1, 0),
		CausalInferenceStartVertex: "A",
		CausalInferenceMaxDepth:    "1",
	}
	if err := original.Init(newSliceSource(cells...), opts, IteratorEnvironment{Scope: ScopeScan}); err != nil {
		t.Fatalf("Init: %v", err)
	}
	copy := original.DeepCopy(IteratorEnvironment{Scope: ScopeScan})
	if err := original.Seek(InfiniteRange(), nil, false); err != nil {
		t.Fatalf("original Seek: %v", err)
	}
	if err := copy.Seek(InfiniteRange(), nil, false); err != nil {
		t.Fatalf("copy Seek: %v", err)
	}
	originalCell, _ := readCausalResult(t, original)
	copyCell, _ := readCausalResult(t, copy)
	if causalResultString(originalCell) != causalResultString(copyCell) {
		t.Fatalf("DeepCopy output differed\noriginal: %s\ncopy: %s", causalResultString(originalCell), causalResultString(copyCell))
	}
}

func ptr(s string) *string {
	return &s
}
