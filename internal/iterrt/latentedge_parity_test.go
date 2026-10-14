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
	"fmt"
	"math"
	"math/rand"
	"sort"
	"strings"
	"testing"

	"github.com/phrocker/shoal-oss/internal/rfile/wire"
)

// This file pins down latentEdge's behavior at the cap-active regime
// (cell with > maxCellBuffer vertices, generating > maxPairsPerCell
// pairs). It exists because cluster-side shadow oracle T2 hash compares
// kept failing on graph_vidx with ~14% delta in link-cell counts, and I
// want a local repro that doesn't need cluster round-trips.

// embeddingBytes encodes a float32 slice in BIG_ENDIAN, matching
// VectorIndexWriter's wire format that the iterator parses.
func embeddingBytes(v []float32) []byte {
	out := make([]byte, len(v)*4)
	for i, f := range v {
		binary.BigEndian.PutUint32(out[i*4:i*4+4], math.Float32bits(f))
	}
	return out
}

// randomUnitVec generates a unit-length d-dim float32 vector from rng.
// Unit length so cosine similarity == dot product (simpler reasoning).
func randomUnitVec(rng *rand.Rand, d int) []float32 {
	v := make([]float32, d)
	var n2 float64
	for i := range v {
		v[i] = float32(rng.NormFloat64())
		n2 += float64(v[i]) * float64(v[i])
	}
	n := float32(math.Sqrt(n2))
	for i := range v {
		v[i] /= n
	}
	return v
}

// synthCells builds an in-memory sliceSource of cells emulating one
// graph_vidx tessellation cell with N distinct vertices, each having a
// single V:_embedding cell with the given vector. Vertex IDs are
// zero-padded "v0000".."vN-1", in sorted order; cells are emitted in
// the order RFile-merge would yield (row, cf, cq, ts-DESC).
//
// cellID is the tessellation prefix (e.g. "00000000000000a7").
func synthCells(cellID string, vecs [][]float32, ts int64) []Cell {
	out := make([]Cell, 0, len(vecs))
	for i, v := range vecs {
		row := []byte(fmt.Sprintf("%s:v%05d", cellID, i))
		out = append(out, Cell{
			Key: &wire.Key{
				Row:             row,
				ColumnFamily:    []byte("V"),
				ColumnQualifier: []byte("_embedding"),
				Timestamp:       ts,
			},
			Value: embeddingBytes(v),
		})
	}
	// Cells are already in (row, cf, cq) sorted order — sorted by
	// zero-padded vertex index. RFile-merge would yield the same.
	return out
}

// runLatentEdge wires a SliceSource → LatentEdgeDiscoveryIterator and
// drains it, returning the emitted CF=link cells (the "Mark" cells) in
// the order the iterator produced them after its internal sort.
func runLatentEdge(t *testing.T, src []Cell, opts map[string]string) []Cell {
	t.Helper()
	leaf := NewSliceSource(src)
	if err := leaf.Init(nil, nil, IteratorEnvironment{Scope: ScopeMajc}); err != nil {
		t.Fatalf("leaf init: %v", err)
	}
	it := NewLatentEdgeDiscoveryIterator()
	if err := it.Init(leaf, opts, IteratorEnvironment{Scope: ScopeMajc}); err != nil {
		t.Fatalf("latentedge init: %v", err)
	}
	if err := it.Seek(InfiniteRange(), nil, false); err != nil {
		t.Fatalf("seek: %v", err)
	}
	var links []Cell
	for it.HasTop() {
		k := it.GetTopKey().Clone()
		v := append([]byte(nil), it.GetTopValue()...)
		if string(k.ColumnFamily) == "link" {
			links = append(links, Cell{Key: k, Value: v})
		}
		if err := it.Next(); err != nil {
			t.Fatalf("next: %v", err)
		}
	}
	return links
}

// expectedPairs computes the set of (vertexA, vertexB) pairs that an
// ORACLE implementation (sort-then-enumerate, cap at maxPairs, threshold
// filter) WOULD emit. Used to pin both Java and Go to a shared spec.
//
// Returns the pair set as a sorted []string of "vA|vB" with vA < vB.
func expectedPairs(vecs [][]float32, vertices []string, maxBuffer, maxPairs int, threshold float32) []string {
	if maxBuffer > len(vertices) {
		maxBuffer = len(vertices)
	}
	// Buffer = first maxBuffer in source-arrival order. With vertices
	// already in sorted order (per synthCells), "first N" == lex-first N.
	bufVerts := append([]string(nil), vertices[:maxBuffer]...)
	bufVecs := vecs[:maxBuffer]

	// Lex-sort vertex IDs — must match the in-iterator sort.
	type pair struct {
		i int
		v string
	}
	ps := make([]pair, len(bufVerts))
	for i, v := range bufVerts {
		ps[i] = pair{i, v}
	}
	sort.SliceStable(ps, func(a, b int) bool { return ps[a].v < ps[b].v })

	emitted := []string{}
	checked := 0
	for i := 0; i < len(ps) && checked < maxPairs; i++ {
		for j := i + 1; j < len(ps) && checked < maxPairs; j++ {
			va := ps[i].v
			vb := ps[j].v
			ea := bufVecs[ps[i].i]
			eb := bufVecs[ps[j].i]
			sim := dot(ea, eb) // unit vectors: cosine == dot
			checked++
			if sim >= threshold {
				emitted = append(emitted, va+"|"+vb)
			}
		}
	}
	sort.Strings(emitted)
	return emitted
}

func dot(a, b []float32) float32 {
	var s float32
	for i := range a {
		s += a[i] * b[i]
	}
	return s
}

// linksToPairs reduces a set of bidirectional link cells (cellID:vA, link, vB)
// + (cellID:vB, link, vA) to the deduplicated pair set "vA|vB" with vA<vB.
func linksToPairs(links []Cell, cellID string) []string {
	prefix := cellID + ":"
	seen := map[string]struct{}{}
	for _, c := range links {
		row := string(c.Key.Row)
		if !strings.HasPrefix(row, prefix) {
			continue
		}
		a := strings.TrimPrefix(row, prefix)
		b := string(c.Key.ColumnQualifier)
		if a > b {
			a, b = b, a
		}
		seen[a+"|"+b] = struct{}{}
	}
	out := make([]string, 0, len(seen))
	for p := range seen {
		out = append(out, p)
	}
	sort.Strings(out)
	return out
}

// TestLatentEdgeParity_CapHits exercises the iterator with N=250 vertices
// and maxCellBuffer=200, maxPairsPerCell=500. The buffer fills (drops the
// last 50 vertices), the pair cap fires (only 500 of C(200,2)=19,900
// pairs checked). After my canonical-order fix, the iterator's emitted
// pair set MUST equal the spec's expected pair set.
//
// If this test fails, the iterator port has a bug that doesn't depend on
// Java for repro.
func TestLatentEdgeParity_CapHits(t *testing.T) {
	const (
		N         = 250
		dim       = 768
		buffer    = 200
		pairs     = 500
		threshold = float32(0.85)
		seed      = int64(20260515)
		cellID    = "00000000000000a7"
		ts        = int64(1_700_000_000_000)
	)
	rng := rand.New(rand.NewSource(seed))

	// Generate N unit vectors with engineered correlation: most are
	// near-orthogonal random unit vecs (cosine sim ~0), but every 5th
	// vertex is a small perturbation of an earlier vertex (cosine sim
	// > 0.85). This guarantees enough high-similarity pairs to make the
	// pair cap matter.
	vecs := make([][]float32, N)
	for i := 0; i < N; i++ {
		if i%5 == 0 || i < 4 {
			vecs[i] = randomUnitVec(rng, dim)
		} else {
			// perturb vertex (i-1) by a small random vec, then renormalize.
			base := vecs[i-1]
			perturb := randomUnitVec(rng, dim)
			out := make([]float32, dim)
			var n2 float64
			for k := range out {
				out[k] = base[k] + 0.1*perturb[k]
				n2 += float64(out[k]) * float64(out[k])
			}
			n := float32(math.Sqrt(n2))
			for k := range out {
				out[k] /= n
			}
			vecs[i] = out
		}
	}

	vertices := make([]string, N)
	for i := 0; i < N; i++ {
		vertices[i] = fmt.Sprintf("v%05d", i)
	}

	src := synthCells(cellID, vecs, ts)
	got := linksToPairs(runLatentEdge(t, src, map[string]string{
		LatentEdgeSimilarityThreshold: fmt.Sprintf("%f", threshold),
		LatentEdgeMaxPairsPerCell:     fmt.Sprintf("%d", pairs),
		LatentEdgeMaxCellBuffer:       fmt.Sprintf("%d", buffer),
	}), cellID)
	want := expectedPairs(vecs, vertices, buffer, pairs, threshold)

	if len(got) != len(want) {
		t.Errorf("pair count: got=%d want=%d", len(got), len(want))
	}
	mismatch := 0
	for i := 0; i < len(got) && i < len(want); i++ {
		if got[i] != want[i] {
			if mismatch < 5 {
				t.Errorf("pair[%d]: got=%q want=%q", i, got[i], want[i])
			}
			mismatch++
		}
	}
	if mismatch > 0 {
		t.Errorf("total mismatched pairs: %d (of %d)", mismatch, len(got))
	}
	if t.Failed() {
		t.Logf("first 10 got:  %v", got[:min(10, len(got))])
		t.Logf("first 10 want: %v", want[:min(10, len(want))])
	}
}

// TestLatentEdgeParity_SortingIsNoOp asserts the property that motivated
// our earlier debugging: vertex IDs arrive in sorted source order, so
// the in-iterator sort.Strings is a no-op (the iterator was deterministic
// even before the canonical-order patch). The patch matters for Java side
// only.
func TestLatentEdgeParity_SortingIsNoOp(t *testing.T) {
	const (
		N      = 50
		dim    = 16
		cellID = "ce11"
		ts     = int64(1)
	)
	rng := rand.New(rand.NewSource(1))
	vecs := make([][]float32, N)
	for i := range vecs {
		vecs[i] = randomUnitVec(rng, dim)
	}
	src := synthCells(cellID, vecs, ts)

	// Drain leaf manually, collect vertex-IDs in arrival order; they
	// must be lex-sorted by the synthCells generator + sliceSource.
	leaf := NewSliceSource(src)
	if err := leaf.Init(nil, nil, IteratorEnvironment{Scope: ScopeMajc}); err != nil {
		t.Fatalf("leaf init: %v", err)
	}
	if err := leaf.Seek(InfiniteRange(), nil, false); err != nil {
		t.Fatalf("leaf seek: %v", err)
	}
	var arrived []string
	for leaf.HasTop() {
		row := string(leaf.GetTopKey().Row)
		// row is "ce11:vNNNNN"
		_, _, _ = row, cellID, ts
		arrived = append(arrived, row)
		if err := leaf.Next(); err != nil {
			t.Fatalf("leaf next: %v", err)
		}
	}
	for i := 1; i < len(arrived); i++ {
		if arrived[i-1] >= arrived[i] {
			t.Fatalf("arrival[%d]=%q not before arrival[%d]=%q — synth order broken",
				i-1, arrived[i-1], i, arrived[i])
		}
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
