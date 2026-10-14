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
	"encoding/binary"
	"math"
	"testing"
)

// embed encodes a float32 slice as the BIG_ENDIAN byte stream that the
// iterator expects (matches Java VectorIndexWriter / parseEmbedding).
func embed(vec ...float32) []byte {
	out := make([]byte, 4*len(vec))
	for i, f := range vec {
		binary.BigEndian.PutUint32(out[i*4:], math.Float32bits(f))
	}
	return out
}

// vidxRow builds an embedding-row Key: row="<cellId>:<vertexId>", CF="V",
// CQ="_embedding".
func vidxRow(cellID, vertexID string, ts int64) *Key {
	return mk(cellID+":"+vertexID, "V", "_embedding", "", ts)
}

func initLatent(t *testing.T, src SortedKeyValueIterator, opts map[string]string) *LatentEdgeDiscoveryIterator {
	t.Helper()
	l := NewLatentEdgeDiscoveryIterator()
	if err := l.Init(src, opts, IteratorEnvironment{Scope: ScopeMajc, FullMajorCompaction: true}); err != nil {
		t.Fatalf("Init: %v", err)
	}
	if err := l.Seek(InfiniteRange(), nil, false); err != nil {
		t.Fatalf("Seek: %v", err)
	}
	return l
}

// countLinkPairs counts the bidirectional link cells emitted; each pair
// produces TWO cells (rowA→rowB and rowB→rowA).
func countLinkPairs(cells []kv) int {
	n := 0
	for _, c := range cells {
		if string(c.k.ColumnFamily) == "link" {
			n++
		}
	}
	return n
}

// TestLatentEdge_EmitsLinkAboveThreshold: two near-identical embeddings
// in the same cell at threshold=0.9 should produce one symmetric pair
// (2 cells).
func TestLatentEdge_EmitsLinkAboveThreshold(t *testing.T) {
	cells := []kv{
		{vidxRow("abc", "v1", 100), embed(1, 0, 0)},
		{vidxRow("abc", "v2", 100), embed(0.999, 0.01, 0)}, // cos ~ 1.0
	}
	l := initLatent(t, newSliceSource(cells...), map[string]string{
		LatentEdgeSimilarityThreshold: "0.9",
	})
	got, err := drain(l)
	if err != nil {
		t.Fatalf("drain: %v", err)
	}
	// 2 originals + 2 link cells.
	if len(got) != 4 {
		t.Fatalf("expected 4 cells, got %d", len(got))
	}
	if countLinkPairs(got) != 2 {
		t.Fatalf("expected 2 link cells, got %d (%v)", countLinkPairs(got), got)
	}
}

// TestLatentEdge_BelowThresholdNoEmit: an orthogonal pair (cos=0) must
// NOT produce link cells, regardless of threshold.
func TestLatentEdge_BelowThresholdNoEmit(t *testing.T) {
	cells := []kv{
		{vidxRow("abc", "v1", 100), embed(1, 0, 0)},
		{vidxRow("abc", "v2", 100), embed(0, 1, 0)},
	}
	l := initLatent(t, newSliceSource(cells...), nil) // default 0.85
	got, _ := drain(l)
	if len(got) != 2 {
		t.Fatalf("expected 2 cells (originals only), got %d", len(got))
	}
	if countLinkPairs(got) != 0 {
		t.Fatalf("expected 0 link cells, got %d", countLinkPairs(got))
	}
}

// TestLatentEdge_CellBoundaryIsolation: similar embeddings across
// DIFFERENT tessellation cells must NOT be linked — the algorithm only
// runs pairwise comparisons within a cell.
func TestLatentEdge_CellBoundaryIsolation(t *testing.T) {
	cells := []kv{
		{vidxRow("abc", "v1", 100), embed(1, 0, 0)},
		{vidxRow("xyz", "v2", 100), embed(1, 0, 0)}, // identical, different cell
	}
	l := initLatent(t, newSliceSource(cells...), nil)
	got, _ := drain(l)
	if countLinkPairs(got) != 0 {
		t.Fatalf("expected 0 link cells across cells, got %d", countLinkPairs(got))
	}
}

// TestLatentEdge_LinkCellPassthroughNotReprocessed: an existing link cell
// from a prior compaction passes through unchanged AND is NOT used as
// input for further pairwise sweeps.
func TestLatentEdge_LinkCellPassthroughNotReprocessed(t *testing.T) {
	// Pre-existing link cell (CF=link) + one embedding cell.
	cells := []kv{
		{mk("abc:v1", "link", "v2", "", 100), []byte("0.9500")},
		{vidxRow("abc", "v3", 100), embed(1, 0, 0)},
	}
	l := initLatent(t, newSliceSource(cells...), nil)
	got, _ := drain(l)
	// Both originals pass through; no NEW link cells (only one embedding).
	if len(got) != 2 {
		t.Fatalf("expected 2 cells (originals only), got %d", len(got))
	}
}

// TestLatentEdge_NonEmbeddingPassThrough: cells with other CF/CQ (e.g.
// V:_label or M:_cellId) pass through and don't participate in pairing.
func TestLatentEdge_NonEmbeddingPassThrough(t *testing.T) {
	cells := []kv{
		{mk("abc:v1", "V", "_label", "", 100), []byte("hello")},
		{vidxRow("abc", "v1", 100), embed(1, 0, 0)},
		{vidxRow("abc", "v2", 100), embed(0.999, 0.01, 0)},
	}
	l := initLatent(t, newSliceSource(cells...), map[string]string{
		LatentEdgeSimilarityThreshold: "0.9",
	})
	got, _ := drain(l)
	// 3 originals + 2 link cells.
	if len(got) != 5 {
		t.Fatalf("expected 5 cells, got %d", len(got))
	}
	if countLinkPairs(got) != 2 {
		t.Fatalf("expected 2 link cells, got %d", countLinkPairs(got))
	}
}

// TestLatentEdge_MaxPairsCap: maxPairsPerCell bounds the work even when
// the cell has more than that many pairs at/over threshold.
func TestLatentEdge_MaxPairsCap(t *testing.T) {
	// 4 nearly-identical embeddings in one cell => 6 candidate pairs.
	cells := []kv{
		{vidxRow("abc", "v1", 100), embed(1.000, 0.001, 0)},
		{vidxRow("abc", "v2", 100), embed(0.999, 0.002, 0)},
		{vidxRow("abc", "v3", 100), embed(0.998, 0.003, 0)},
		{vidxRow("abc", "v4", 100), embed(0.997, 0.004, 0)},
	}
	l := initLatent(t, newSliceSource(cells...), map[string]string{
		LatentEdgeSimilarityThreshold: "0.5",
		LatentEdgeMaxPairsPerCell:     "2", // cap at 2 pairs checked
	})
	got, _ := drain(l)
	// 4 originals + up to 2 pairs × 2 cells = at most 8.
	if countLinkPairs(got) > 4 {
		t.Fatalf("expected at most 2 pairs (4 link cells), got %d link cells", countLinkPairs(got))
	}
}

// TestLatentEdge_MaxCellBufferCap: maxCellBuffer caps how many embeddings
// per cell are retained for pairing.
func TestLatentEdge_MaxCellBufferCap(t *testing.T) {
	cells := []kv{
		{vidxRow("abc", "v1", 100), embed(1, 0, 0)},
		{vidxRow("abc", "v2", 100), embed(0.999, 0, 0)},
		{vidxRow("abc", "v3", 100), embed(0.998, 0, 0)},
	}
	l := initLatent(t, newSliceSource(cells...), map[string]string{
		LatentEdgeSimilarityThreshold: "0.5",
		LatentEdgeMaxCellBuffer:       "2",
	})
	got, _ := drain(l)
	// Only 2 embeddings buffered => 1 pair => 2 link cells. 3 originals.
	if countLinkPairs(got) != 2 {
		t.Fatalf("expected 1 pair (2 link cells) with buffer cap=2, got %d link cells", countLinkPairs(got))
	}
}

// TestLatentEdge_OutputSortedByKey: emitted output must be in wire.Key
// order — the iterrt SKVI contract requires monotonic Seek/Next.
func TestLatentEdge_OutputSortedByKey(t *testing.T) {
	cells := []kv{
		{vidxRow("abc", "v1", 100), embed(1, 0, 0)},
		{vidxRow("abc", "v2", 100), embed(0.999, 0.01, 0)},
	}
	l := initLatent(t, newSliceSource(cells...), map[string]string{
		LatentEdgeSimilarityThreshold: "0.9",
	})
	got, _ := drain(l)
	for i := 1; i < len(got); i++ {
		if got[i-1].k.Compare(got[i].k) > 0 {
			t.Fatalf("output not sorted at index %d: %+v then %+v", i, got[i-1].k, got[i].k)
		}
	}
}

// TestLatentEdge_DimensionMismatchSkipped: pairs with mismatched embedding
// dimensions count against the pair budget but emit no link.
func TestLatentEdge_DimensionMismatchSkipped(t *testing.T) {
	cells := []kv{
		{vidxRow("abc", "v1", 100), embed(1, 0)},
		{vidxRow("abc", "v2", 100), embed(1, 0, 0)},
	}
	l := initLatent(t, newSliceSource(cells...), map[string]string{
		LatentEdgeSimilarityThreshold: "0.1",
	})
	got, _ := drain(l)
	if countLinkPairs(got) != 0 {
		t.Fatalf("expected 0 link cells for dim-mismatch, got %d", countLinkPairs(got))
	}
}

// TestLatentEdge_ScoreFormat: the value on emitted link cells must match
// Java's %.4f format exactly so wire-readable content is bit-identical.
func TestLatentEdge_ScoreFormat(t *testing.T) {
	cells := []kv{
		{vidxRow("abc", "v1", 100), embed(1, 0, 0)},
		{vidxRow("abc", "v2", 100), embed(1, 0, 0)}, // cos == 1
	}
	l := initLatent(t, newSliceSource(cells...), map[string]string{
		LatentEdgeSimilarityThreshold: "0.5",
	})
	got, _ := drain(l)
	for _, c := range got {
		if string(c.k.ColumnFamily) != "link" {
			continue
		}
		if string(c.v) != "1.0000" {
			t.Fatalf("expected score=\"1.0000\", got %q", c.v)
		}
	}
}

// TestLatentEdge_BuildStackRegistered: BuildStack honors the iterator
// name and option pass-through.
func TestLatentEdge_BuildStackRegistered(t *testing.T) {
	cells := []kv{
		{vidxRow("abc", "v1", 100), embed(1, 0, 0)},
		{vidxRow("abc", "v2", 100), embed(0.999, 0.01, 0)},
	}
	stack, err := BuildStack(newSliceSource(cells...), []IterSpec{
		{Name: IterLatentEdgeDiscovery, Options: map[string]string{LatentEdgeSimilarityThreshold: "0.9"}},
	}, IteratorEnvironment{Scope: ScopeMajc, FullMajorCompaction: true})
	if err != nil {
		t.Fatalf("BuildStack: %v", err)
	}
	if err := stack.Seek(InfiniteRange(), nil, false); err != nil {
		t.Fatalf("Seek: %v", err)
	}
	got, _ := drain(stack)
	if countLinkPairs(got) != 2 {
		t.Fatalf("expected 2 link cells via stack, got %d", countLinkPairs(got))
	}
}

// TestLatentEdge_RowWithoutColonSkipped: malformed rows lacking the
// "<cellId>:<vertexId>" separator pass through without crashing.
func TestLatentEdge_RowWithoutColonSkipped(t *testing.T) {
	cells := []kv{
		{mk("malformed-row", "V", "_embedding", "", 100), embed(1, 0, 0)},
	}
	l := initLatent(t, newSliceSource(cells...), nil)
	got, _ := drain(l)
	if len(got) != 1 || countLinkPairs(got) != 0 {
		t.Fatalf("expected 1 passthrough cell, no links; got %d cells, %d links", len(got), countLinkPairs(got))
	}
}
