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
	"context"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"math"
	"testing"

	"github.com/phrocker/shoal-oss/internal/rfile/wire"
)

type cancelingVectorSource struct {
	SortedKeyValueIterator
	cancel context.CancelFunc
}

func (s *cancelingVectorSource) Next() error {
	err := s.SortedKeyValueIterator.Next()
	s.cancel()
	return err
}

func (s *cancelingVectorSource) DeepCopy(
	env IteratorEnvironment,
) SortedKeyValueIterator {
	return &cancelingVectorSource{
		SortedKeyValueIterator: s.SortedKeyValueIterator.DeepCopy(env),
		cancel:                 s.cancel,
	}
}

// packBE packs a float32 vector big-endian, matching the iterator's wire form.
func packBE(vec ...float32) []byte {
	b := make([]byte, len(vec)*4)
	for i, f := range vec {
		binary.BigEndian.PutUint32(b[i*4:], math.Float32bits(f))
	}
	return b
}

// embCell builds an embedding cell on row/cf with a packed-float32 value.
func embCell(row, cf string, vec ...float32) Cell {
	return Cell{
		Key:   &wire.Key{Row: []byte(row), ColumnFamily: []byte(cf), Timestamp: 1},
		Value: packBE(vec...),
	}
}

// unpackScore reads a 4-byte big-endian float32 score from an output value.
func unpackScore(t *testing.T, v []byte) float32 {
	t.Helper()
	if len(v) != 4 {
		t.Fatalf("score value len = %d, want 4", len(v))
	}
	return math.Float32frombits(binary.BigEndian.Uint32(v))
}

// runKNN wires a SliceSource → VectorKNNIterator over the full range and drains.
func runKNN(t *testing.T, cells []Cell, opts map[string]string) []Cell {
	t.Helper()
	if _, ok := opts[VectorKNNEmbeddingSpace]; !ok {
		opts[VectorKNNEmbeddingSpace] = "test-provider:test-model-v1:2:l2"
	}
	leaf := NewSliceSource(sortedSlice(cells))
	if err := leaf.Init(nil, nil, IteratorEnvironment{Scope: ScopeScan}); err != nil {
		t.Fatalf("leaf init: %v", err)
	}
	it := NewVectorKNNIterator()
	if err := it.Init(leaf, opts, IteratorEnvironment{Scope: ScopeScan}); err != nil {
		t.Fatalf("vectorknn init: %v", err)
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

func b64(vec ...float32) string {
	return base64.StdEncoding.EncodeToString(packBE(vec...))
}

func TestVectorKNNSeekHonorsCancellationWhileScoring(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	leaf := NewSliceSource(sortedSlice([]Cell{
		embCell("a", "V", 1, 0),
		embCell("b", "V", 0, 1),
	}))
	if err := leaf.Init(nil, nil, IteratorEnvironment{Scope: ScopeScan}); err != nil {
		t.Fatal(err)
	}
	source := &cancelingVectorSource{
		SortedKeyValueIterator: leaf,
		cancel:                 cancel,
	}
	it := NewVectorKNNIterator()
	if err := it.Init(source, map[string]string{
		VectorKNNQuery:          b64(1, 0),
		VectorKNNEmbeddingSpace: "test-space",
	}, IteratorEnvironment{Context: ctx, Scope: ScopeScan}); err != nil {
		t.Fatal(err)
	}
	if err := it.Seek(
		InfiniteRange(), nil, false,
	); !errors.Is(err, context.Canceled) {
		t.Fatalf("Seek cancellation error = %v", err)
	}
}

// TestVectorKNNCosineRanking verifies cosine top-k ordering: the query is the
// unit x axis, so rows closest in direction rank first and the value carries
// the cosine score.
func TestVectorKNNCosineRanking(t *testing.T) {
	cells := []Cell{
		embCell("a", "V", 1, 0),  // cosine 1.0
		embCell("b", "V", 1, 1),  // cosine ~0.707
		embCell("c", "V", 0, 1),  // cosine 0.0
		embCell("d", "V", -1, 0), // cosine -1.0
	}
	got := runKNN(t, cells, map[string]string{
		VectorKNNQuery:  b64(1, 0),
		VectorKNNTopK:   "3",
		VectorKNNMetric: "cosine",
	})
	if len(got) != 3 {
		t.Fatalf("got %d cells, want 3", len(got))
	}
	wantRows := []string{"a", "b", "c"}
	for i, w := range wantRows {
		if string(got[i].Key.Row) != w {
			t.Errorf("rank %d row = %q, want %q", i, got[i].Key.Row, w)
		}
	}
	// Scores must be descending and a's must be ~1.0.
	if s := unpackScore(t, got[0].Value); math.Abs(float64(s)-1.0) > 1e-5 {
		t.Errorf("top score = %v, want ~1.0", s)
	}
	prev := float32(math.Inf(1))
	for i, c := range got {
		s := unpackScore(t, c.Value)
		if s > prev {
			t.Errorf("score at %d (%v) > previous (%v); not descending", i, s, prev)
		}
		prev = s
	}
}

// TestVectorKNNMinScore drops cells below the threshold.
func TestVectorKNNMinScore(t *testing.T) {
	cells := []Cell{
		embCell("a", "V", 1, 0),  // 1.0
		embCell("b", "V", 1, 1),  // ~0.707
		embCell("c", "V", 0, 1),  // 0.0
		embCell("d", "V", -1, 0), // -1.0
	}
	got := runKNN(t, cells, map[string]string{
		VectorKNNQuery:    b64(1, 0),
		VectorKNNTopK:     "10",
		VectorKNNMinScore: "0.5",
	})
	if len(got) != 2 {
		t.Fatalf("got %d cells, want 2 (a,b above 0.5)", len(got))
	}
	if string(got[0].Key.Row) != "a" || string(got[1].Key.Row) != "b" {
		t.Errorf("rows = %q,%q; want a,b", got[0].Key.Row, got[1].Key.Row)
	}
}

// TestVectorKNNEmbeddingCFFilter ignores cells outside the configured CF and
// skips embedding cells whose dimension does not match the query.
func TestVectorKNNEmbeddingCFFilter(t *testing.T) {
	cells := []Cell{
		embCell("a", "V", 1, 0),     // counted
		embCell("b", "other", 1, 0), // wrong cf, ignored
		embCell("c", "V", 1, 0, 0),  // wrong dim, skipped
		embCell("d", "V", 0, 1),     // counted
	}
	got := runKNN(t, cells, map[string]string{
		VectorKNNQuery:       b64(1, 0),
		VectorKNNTopK:        "10",
		VectorKNNEmbeddingCF: "V",
	})
	if len(got) != 2 {
		t.Fatalf("got %d cells, want 2 (a,d)", len(got))
	}
	if string(got[0].Key.Row) != "a" || string(got[1].Key.Row) != "d" {
		t.Errorf("rows = %q,%q; want a,d", got[0].Key.Row, got[1].Key.Row)
	}
}

// TestVectorKNNDotAndL2 checks the alternate metrics rank as expected.
func TestVectorKNNDotAndL2(t *testing.T) {
	cells := []Cell{
		embCell("a", "V", 2, 0), // dot 2, l2 dist 1 -> score -1
		embCell("b", "V", 1, 0), // dot 1, l2 dist 0 -> score 0 (closest)
		embCell("c", "V", 5, 0), // dot 5 (largest), l2 dist 16 -> score -16
	}
	// Dot: largest projection wins.
	dotGot := runKNN(t, cells, map[string]string{
		VectorKNNQuery:  b64(1, 0),
		VectorKNNTopK:   "1",
		VectorKNNMetric: "dot",
	})
	if len(dotGot) != 1 || string(dotGot[0].Key.Row) != "c" {
		t.Errorf("dot top = %v, want c", dotGot)
	}
	// L2: nearest in Euclidean distance wins.
	l2Got := runKNN(t, cells, map[string]string{
		VectorKNNQuery:  b64(1, 0),
		VectorKNNTopK:   "1",
		VectorKNNMetric: "l2",
	})
	if len(l2Got) != 1 || string(l2Got[0].Key.Row) != "b" {
		t.Errorf("l2 top = %v, want b", l2Got)
	}
}

// TestVectorKNNInitErrors rejects malformed options.
func TestVectorKNNInitErrors(t *testing.T) {
	leaf := NewSliceSource(nil)
	_ = leaf.Init(nil, nil, IteratorEnvironment{Scope: ScopeScan})
	cases := []struct {
		name string
		opts map[string]string
	}{
		{"missing query", map[string]string{VectorKNNEmbeddingSpace: "space-a"}},
		{"missing identity", map[string]string{VectorKNNQuery: b64(1, 0)}},
		{"non-canonical identity", map[string]string{
			VectorKNNQuery: b64(1, 0), VectorKNNEmbeddingSpace: " space-a ",
		}},
		{"bad base64", map[string]string{VectorKNNQuery: "!!!", VectorKNNEmbeddingSpace: "space-a"}},
		{"bad topK", map[string]string{VectorKNNQuery: b64(1, 0), VectorKNNEmbeddingSpace: "space-a", VectorKNNTopK: "0"}},
		{"bad metric", map[string]string{VectorKNNQuery: b64(1, 0), VectorKNNEmbeddingSpace: "space-a", VectorKNNMetric: "manhattan"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			it := NewVectorKNNIterator()
			if err := it.Init(leaf, tc.opts, IteratorEnvironment{Scope: ScopeScan}); err == nil {
				t.Errorf("expected Init error for %s", tc.name)
			}
		})
	}
}
