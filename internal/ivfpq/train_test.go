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

package ivfpq

import (
	"bytes"
	"math"
	"math/rand"
	"testing"
)

// ── helpers ─────────────────────────────────────────────────────────────────

// makeSamples generates n random unit-normalized dim-dimensional vectors
// seeded deterministically so tests are reproducible.
func makeSamples(n, dim int, seed int64) [][]float32 {
	rng := rand.New(rand.NewSource(seed))
	out := make([][]float32, n)
	for i := range out {
		v := make([]float32, dim)
		for d := range v {
			v[d] = rng.Float32()*2 - 1
		}
		normInPlace(v)
		out[i] = v
	}
	return out
}

// ── VectorPQ.Bytes / New round-trip ─────────────────────────────────────────

func TestVectorPQ_Bytes_RoundTrip(t *testing.T) {
	cb := makeCodebook(4, 8, 4)
	pq, err := New(cb, 16, 3)
	if err != nil {
		t.Fatal(err)
	}
	b, err := pq.Bytes()
	if err != nil {
		t.Fatal(err)
	}
	pq2, err := FromBytes(b)
	if err != nil {
		t.Fatal(err)
	}
	if pq2.M() != pq.M() || pq2.Ks() != pq.Ks() || pq2.Dim() != pq.Dim() {
		t.Errorf("dims mismatch after Bytes round-trip")
	}
	if pq2.CodebookVersion() != 3 {
		t.Errorf("codebookVersion = %d; want 3", pq2.CodebookVersion())
	}
	// Verify Bytes output is identical to WriteTo output.
	var buf bytes.Buffer
	pq.WriteTo(&buf)
	if !bytes.Equal(b, buf.Bytes()) {
		t.Error("Bytes() != WriteTo() output")
	}
}

func TestNew_Validations(t *testing.T) {
	// dim not divisible by m
	cb := makeCodebook(3, 4, 3)
	if _, err := New(cb, 10, 0); err == nil {
		t.Error("expected error for dim not divisible by m")
	}
	// empty codebook
	if _, err := New(nil, 4, 0); err == nil {
		t.Error("expected error for nil codebook")
	}
	// wrong entry length
	bad := [][][]float32{{{1, 2, 3}}}
	if _, err := New(bad, 2, 0); err == nil {
		t.Error("expected error for wrong entry length")
	}
}

// ── Encode ───────────────────────────────────────────────────────────────────

func TestEncode_Basic(t *testing.T) {
	// 2 subspaces, 4 centroids each, dsub=3 → dim=6
	cb := makeCodebook(2, 4, 3)
	pq, err := New(cb, 6, 0)
	if err != nil {
		t.Fatal(err)
	}
	vec := make([]float32, 6)
	for i := range vec {
		vec[i] = float32(i)
	}
	code, err := pq.Encode(vec)
	if err != nil {
		t.Fatal(err)
	}
	if len(code) != 2 {
		t.Fatalf("code length = %d; want 2", len(code))
	}
	// Verify each byte is the argmin squared Euclidean in its subspace.
	for s := 0; s < 2; s++ {
		sub := vec[s*3 : (s+1)*3]
		wantIdx := 0
		wantDist := float32(math.MaxFloat32)
		for c := 0; c < 4; c++ {
			d := sqEuclidean(sub, cb[s][c])
			if d < wantDist {
				wantDist = d
				wantIdx = c
			}
		}
		if int(code[s]) != wantIdx {
			t.Errorf("subspace %d: code=%d want=%d", s, code[s], wantIdx)
		}
	}
}

func TestEncode_TieBreakLowestIndex(t *testing.T) {
	// Build a codebook where two centroids are identical → tie must resolve to 0.
	cb := [][][]float32{
		{
			{1.0, 0.0}, // centroid 0
			{1.0, 0.0}, // centroid 1 — tie with 0
		},
	}
	pq, err := New(cb, 2, 0)
	if err != nil {
		t.Fatal(err)
	}
	code, err := pq.Encode([]float32{1.0, 0.0})
	if err != nil {
		t.Fatal(err)
	}
	if code[0] != 0 {
		t.Errorf("tie-break: want code[0]=0, got %d", code[0])
	}
}

func TestEncode_WrongDim(t *testing.T) {
	pq, _ := New(makeCodebook(2, 4, 3), 6, 0)
	if _, err := pq.Encode([]float32{1, 2, 3}); err == nil {
		t.Error("expected error for wrong dim")
	}
}

// ── Centroids round-trip ─────────────────────────────────────────────────────

func TestCentroids_BytesRoundTrip(t *testing.T) {
	samples := makeSamples(20, 8, 42)
	c, err := TrainCentroids(samples, 4, 10, 42, 7)
	if err != nil {
		t.Fatal(err)
	}
	b, err := c.Bytes()
	if err != nil {
		t.Fatal(err)
	}
	c2, err := CentroidsFromBytes(b)
	if err != nil {
		t.Fatal(err)
	}
	if c2.K() != c.K() || c2.Dim() != c.Dim() || c2.CodebookVersion() != c.CodebookVersion() {
		t.Errorf("fields mismatch: K=%d/%d Dim=%d/%d Ver=%d/%d",
			c2.K(), c.K(), c2.Dim(), c.Dim(), c2.CodebookVersion(), c.CodebookVersion())
	}
	// Spot-check a centroid.
	for d := 0; d < c.Dim(); d++ {
		if math.Abs(float64(c2.vecs[0][d]-c.vecs[0][d])) > 1e-6 {
			t.Errorf("centroid[0][%d] mismatch: %f vs %f", d, c2.vecs[0][d], c.vecs[0][d])
		}
	}
}

func TestCentroidsFromBytes_BadFormat(t *testing.T) {
	var buf bytes.Buffer
	writeI32(&buf, 99) // wrong format version
	writeI32(&buf, 1)
	writeI32(&buf, 4)
	writeI32(&buf, 8)
	if _, err := CentroidsFromBytes(buf.Bytes()); err == nil {
		t.Error("expected error for bad format version")
	}
}

// ── Assign / NProbe ─────────────────────────────────────────────────────────

func TestAssign_ArgmaxIP(t *testing.T) {
	// Three unit-ish centroids; query close to centroid 1.
	vecs := [][]float32{
		{1, 0, 0},
		{0, 1, 0},
		{0, 0, 1},
	}
	c := &Centroids{vecs: vecs, k: 3, dim: 3, codebookVersion: 0}
	query := []float32{0.1, 0.9, 0.1}
	normInPlace(query)
	got := c.Assign(query)
	if got != 1 {
		t.Errorf("Assign = %d; want 1", got)
	}
}

func TestAssign_TieBreakLowestIndex(t *testing.T) {
	vecs := [][]float32{
		{1, 0},
		{1, 0}, // identical to centroid 0 → tie, must resolve to 0
	}
	c := &Centroids{vecs: vecs, k: 2, dim: 2, codebookVersion: 0}
	got := c.Assign([]float32{1, 0})
	if got != 0 {
		t.Errorf("Assign tie-break = %d; want 0", got)
	}
}

func TestNProbe_OrderAndTie(t *testing.T) {
	vecs := [][]float32{
		{1, 0, 0},
		{0, 1, 0},
		{0, 0, 1},
	}
	c := &Centroids{vecs: vecs, k: 3, dim: 3, codebookVersion: 0}
	// Query closest to centroid 2, then 0, then 1.
	query := []float32{0.4, 0.1, 0.9}
	normInPlace(query)
	got := c.NProbe(query, 2)
	if len(got) != 2 {
		t.Fatalf("NProbe len = %d; want 2", len(got))
	}
	// Centroid 2 should be first (highest ip with [0,0,1]).
	if got[0] != 2 {
		t.Errorf("NProbe[0] = %d; want 2", got[0])
	}
}

func TestNProbe_CapAtK(t *testing.T) {
	vecs := [][]float32{{1, 0}, {0, 1}}
	c := &Centroids{vecs: vecs, k: 2, dim: 2, codebookVersion: 0}
	got := c.NProbe([]float32{1, 0}, 10)
	if len(got) != 2 {
		t.Errorf("NProbe cap: len = %d; want 2", len(got))
	}
}

// ── TrainCentroids ──────────────────────────────────────────────────────────

func TestTrainCentroids_Basic(t *testing.T) {
	samples := makeSamples(50, 16, 0)
	c, err := TrainCentroids(samples, 4, 10, 42, 1)
	if err != nil {
		t.Fatal(err)
	}
	if c.K() != 4 || c.Dim() != 16 {
		t.Errorf("K=%d Dim=%d; want 4 16", c.K(), c.Dim())
	}
	if c.CodebookVersion() != 1 {
		t.Errorf("CodebookVersion = %d; want 1", c.CodebookVersion())
	}
	// Centroids should be unit-normalised.
	for i, v := range c.vecs {
		norm := float64(0)
		for _, x := range v {
			norm += float64(x) * float64(x)
		}
		if math.Abs(math.Sqrt(norm)-1.0) > 1e-5 {
			t.Errorf("centroid %d: norm = %f; want ~1.0", i, math.Sqrt(norm))
		}
	}
}

func TestTrainCentroids_Validations(t *testing.T) {
	if _, err := TrainCentroids(nil, 4, 5, 0, 0); err == nil {
		t.Error("expected error for nil samples")
	}
	samples := makeSamples(5, 4, 0)
	if _, err := TrainCentroids(samples, 10, 5, 0, 0); err == nil {
		t.Error("expected error for k > n")
	}
}

// ── TrainPQ ──────────────────────────────────────────────────────────────────

func TestTrainPQ_Basic(t *testing.T) {
	samples := makeSamples(40, 16, 1)
	pq, err := TrainPQ(samples, 4, 8, 10, 42, 2)
	if err != nil {
		t.Fatal(err)
	}
	if pq.M() != 4 || pq.Ks() != 8 || pq.Dim() != 16 {
		t.Errorf("M=%d Ks=%d Dim=%d; want 4 8 16", pq.M(), pq.Ks(), pq.Dim())
	}
	if pq.CodebookVersion() != 2 {
		t.Errorf("CodebookVersion = %d; want 2", pq.CodebookVersion())
	}
	// Encode every sample — must succeed without error.
	for i, s := range samples {
		if _, err := pq.Encode(s); err != nil {
			t.Errorf("sample %d Encode error: %v", i, err)
		}
	}
}

func TestTrainPQ_Validations(t *testing.T) {
	samples := makeSamples(5, 8, 0)
	if _, err := TrainPQ(samples, 3, 4, 5, 0, 0); err == nil {
		t.Error("expected error: dim not divisible by m")
	}
	if _, err := TrainPQ(samples, 2, 0, 5, 0, 0); err == nil {
		t.Error("expected error: ks=0")
	}
	if _, err := TrainPQ(samples, 2, 257, 5, 0, 0); err == nil {
		t.Error("expected error: ks>256")
	}
	if _, err := TrainPQ(samples, 2, 8, 5, 0, 0); err == nil {
		t.Error("expected error: ks > n")
	}
}

// ── Determinism / golden test ────────────────────────────────────────────────

// TestTrainDeterminism is the canonical golden/repeatability test: two
// independent runs with identical inputs MUST produce byte-identical codebook
// blobs and identical PQ codes for every sample.
func TestTrainDeterminism(t *testing.T) {
	const (
		n       = 60
		dim     = 16
		k       = 5
		m       = 4
		ks      = 8
		maxIter = 10
		seed    = int64(42)
		version = int32(1)
	)
	samples := makeSamples(n, dim, 99)

	// Run 1.
	c1, err := TrainCentroids(samples, k, maxIter, seed, version)
	if err != nil {
		t.Fatal(err)
	}
	pq1, err := TrainPQ(samples, m, ks, maxIter, seed, version)
	if err != nil {
		t.Fatal(err)
	}

	// Run 2 — fresh objects, same inputs.
	c2, err := TrainCentroids(samples, k, maxIter, seed, version)
	if err != nil {
		t.Fatal(err)
	}
	pq2, err := TrainPQ(samples, m, ks, maxIter, seed, version)
	if err != nil {
		t.Fatal(err)
	}

	// Byte-identical codebook blobs.
	cb1, _ := c1.Bytes()
	cb2, _ := c2.Bytes()
	if !bytes.Equal(cb1, cb2) {
		t.Error("Centroids.Bytes() not identical across two runs")
	}
	pqb1, _ := pq1.Bytes()
	pqb2, _ := pq2.Bytes()
	if !bytes.Equal(pqb1, pqb2) {
		t.Error("VectorPQ.Bytes() not identical across two runs")
	}

	// Identical PQ codes for every sample.
	for i, s := range samples {
		code1, err1 := pq1.Encode(s)
		code2, err2 := pq2.Encode(s)
		if err1 != nil || err2 != nil {
			t.Fatalf("sample %d Encode errors: %v / %v", i, err1, err2)
		}
		if !bytes.Equal(code1, code2) {
			t.Errorf("sample %d: codes differ across runs", i)
		}
	}
}

// ── table helpers ────────────────────────────────────────────────────────────

func TestTableHelpers(t *testing.T) {
	if got := IvfTableName("mygraph"); got != "mygraph_ivf" {
		t.Errorf("IvfTableName = %q; want mygraph_ivf", got)
	}
	if got := ConfigTableName("mygraph"); got != "mygraph_ann_config" {
		t.Errorf("ConfigTableName = %q; want mygraph_ann_config", got)
	}
	if got := CentroidsRow(3); got != "centroids_v3" {
		t.Errorf("CentroidsRow(3) = %q; want centroids_v3", got)
	}
	if got := PQRow(2); got != "pq_v2" {
		t.Errorf("PQRow(2) = %q; want pq_v2", got)
	}
	if got := EmbeddingSpaceRow(2); got != "embedding_space_v2" {
		t.Errorf("EmbeddingSpaceRow(2) = %q; want embedding_space_v2", got)
	}
	if got := FormatClusterID(0); got != "00000000" {
		t.Errorf("FormatClusterID(0) = %q; want 00000000", got)
	}
	if got := FormatClusterID(255); got != "000000ff" {
		t.Errorf("FormatClusterID(255) = %q; want 000000ff", got)
	}
	if got := FormatClusterID(0xdeadbeef); got != "deadbeef" {
		t.Errorf("FormatClusterID(0xdeadbeef) = %q; want deadbeef", got)
	}
	key := RowKey(3, "evt:abc")
	if key != "00000003:evt:abc" {
		t.Errorf("RowKey = %q; want 00000003:evt:abc", key)
	}
}

// ── helper for building bytes in tests ──────────────────────────────────────

func writeI32(buf *bytes.Buffer, v int32) {
	buf.WriteByte(byte(v >> 24))
	buf.WriteByte(byte(v >> 16))
	buf.WriteByte(byte(v >> 8))
	buf.WriteByte(byte(v))
}
