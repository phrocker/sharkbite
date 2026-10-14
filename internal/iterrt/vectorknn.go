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
	"container/heap"
	"context"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"sort"
	"strconv"

	"github.com/phrocker/shoal-oss/internal/embeddingspace"
)

// VectorKNNIterator is the brute-force vector k-NN pushdown iterator described
// in docs/ai-knowledge-graph.md capability (2). It moves a nearest-neighbor
// search server-side, next to the data.
//
// A consumer co-locates an embedding with its primary row as an ordinary cell:
// a reserved column family whose value is the embedding packed as big-endian
// float32 (length = dim*4). Search without pushdown means the client streams
// every embedding cell back and ranks them itself. This iterator does the rank
// next to the data: given a query vector and k, it scores every embedding cell
// in range against the query, keeps the k best, and emits those cells with the
// cell value replaced by the big-endian float32 similarity score. One Seek
// therefore returns the k nearest rows + their scores, not the whole table.
//
// The iterator defines only the mechanism (score cells -> top-k). The
// embedding schema is the consumer's, supplied via options:
//
//	query.b64    base64 of the query vector, packed big-endian float32
//	embeddingSpace stable identity of the model space that produced the query
//	topK         number of nearest cells to return (k); default 10
//	embeddingCF  optional column family holding embedding cells; empty means
//	             every cell in range is treated as an embedding
//	metric       "cosine" (default) | "dot" | "l2"
//	minScore     optional float; cells scoring below it are dropped. Scores are
//	             oriented so larger is better (l2 uses negative squared
//	             distance), so minScore filters the same way for every metric.
//
// Output is score-descending (best first), so VectorKNNIterator is a TERMINAL
// ranking iterator: it must sit at the top of a scan stack and its result is
// streamed straight to the client — it does not preserve the key-ascending
// order a mid-stack iterator would. This mirrors the IVF-PQ distance
// iterator's drain semantics.
//
// Source contract: re-seekable, hosted above a whole-table merge so the search
// sees every embedding cell regardless of which tablet holds it.
type VectorKNNIterator struct {
	source SortedKeyValueIterator
	ctx    context.Context

	query       []float32
	queryNorm   float32 // precomputed |query| for cosine
	querySpace  string
	topK        int
	embeddingCF []byte // nil/empty = any cf
	metric      string
	minScore    float32

	out      []Cell
	outIndex int
	err      error
}

// VectorKNNIterator option keys.
const (
	// VectorKNNQuery is the base64-encoded query vector (packed big-endian
	// float32, length dim*4).
	VectorKNNQuery = "query.b64"
	// VectorKNNEmbeddingSpace is the stable identity of the model space that
	// produced query. Identity-aware planners supply it even when dimensions
	// happen to match; storage backends use it to validate per-file metadata.
	VectorKNNEmbeddingSpace = "embeddingSpace"
	// VectorKNNTopK is the number of nearest cells to return (k); default 10.
	VectorKNNTopK = "topK"
	// VectorKNNEmbeddingCF optionally restricts which column family is treated
	// as an embedding cell. Empty = every cell in range.
	VectorKNNEmbeddingCF = "embeddingCF"
	// VectorKNNMetric selects the similarity metric: "cosine" (default),
	// "dot", or "l2".
	VectorKNNMetric = "metric"
	// VectorKNNMinScore optionally drops cells scoring below the given float.
	VectorKNNMinScore = "minScore"

	vectorKNNMetricCosine = "cosine"
	vectorKNNMetricDot    = "dot"
	vectorKNNMetricL2     = "l2"
)

// NewVectorKNNIterator constructs an un-Init'd iterator.
func NewVectorKNNIterator() *VectorKNNIterator {
	return &VectorKNNIterator{}
}

// Init wires the source and parses options. query.b64 is required and must
// decode to a non-empty multiple of 4 bytes; topK, if set, must be a positive
// integer; metric, if set, must be a recognized name.
func (v *VectorKNNIterator) Init(source SortedKeyValueIterator, options map[string]string, env IteratorEnvironment) error {
	if source == nil {
		return errors.New("iterrt: VectorKNNIterator requires a non-nil source")
	}
	v.source = source
	v.ctx = env.Context
	if v.ctx == nil {
		v.ctx = context.Background()
	}

	qB64, ok := options[VectorKNNQuery]
	if !ok || qB64 == "" {
		return fmt.Errorf("iterrt: VectorKNNIterator missing option %q", VectorKNNQuery)
	}
	qBytes, err := base64.StdEncoding.DecodeString(qB64)
	if err != nil {
		return fmt.Errorf("iterrt: VectorKNNIterator bad %s: %w", VectorKNNQuery, err)
	}
	v.query, err = unpackFloat32BE(qBytes)
	if err != nil {
		return fmt.Errorf("iterrt: VectorKNNIterator %s: %w", VectorKNNQuery, err)
	}
	if len(v.query) == 0 {
		return fmt.Errorf("iterrt: VectorKNNIterator %s is empty", VectorKNNQuery)
	}
	v.queryNorm = knnNorm(v.query)
	v.querySpace = options[VectorKNNEmbeddingSpace]
	if err := embeddingspace.ValidateQueryStates(
		"initialize exact vector iterator", v.querySpace); err != nil {
		return fmt.Errorf("iterrt: VectorKNNIterator: %w", err)
	}

	v.topK = 10
	if s, ok := options[VectorKNNTopK]; ok && s != "" {
		n, err := strconv.Atoi(s)
		if err != nil || n <= 0 {
			return fmt.Errorf("iterrt: VectorKNNIterator bad %s=%q", VectorKNNTopK, s)
		}
		v.topK = n
	}

	if s, ok := options[VectorKNNEmbeddingCF]; ok && s != "" {
		v.embeddingCF = []byte(s)
	}

	v.metric = vectorKNNMetricCosine
	switch options[VectorKNNMetric] {
	case "", vectorKNNMetricCosine:
		v.metric = vectorKNNMetricCosine
	case vectorKNNMetricDot:
		v.metric = vectorKNNMetricDot
	case vectorKNNMetricL2:
		v.metric = vectorKNNMetricL2
	default:
		return fmt.Errorf("iterrt: VectorKNNIterator bad %s=%q (want %q, %q or %q)",
			VectorKNNMetric, options[VectorKNNMetric],
			vectorKNNMetricCosine, vectorKNNMetricDot, vectorKNNMetricL2)
	}

	v.minScore = float32(math.Inf(-1))
	if s, ok := options[VectorKNNMinScore]; ok && s != "" {
		f, err := strconv.ParseFloat(s, 32)
		if err != nil {
			return fmt.Errorf("iterrt: VectorKNNIterator bad %s=%q", VectorKNNMinScore, s)
		}
		v.minScore = float32(f)
	}
	return nil
}

// Seek scores every embedding cell within range r (and allowed by the cf
// filter) against the query, keeps the top-k, and buffers them score-
// descending. Subsequent HasTop/GetTopKey/GetTopValue/Next walk the buffer.
func (v *VectorKNNIterator) Seek(r Range, columnFamilies [][]byte, inclusive bool) error {
	v.out = v.out[:0]
	v.outIndex = 0
	v.err = nil

	if err := v.ctx.Err(); err != nil {
		v.err = err
		return err
	}
	if err := v.source.Seek(r, columnFamilies, inclusive); err != nil {
		v.err = err
		return err
	}

	h := &knnHeap{cap: v.topK}
	for v.source.HasTop() {
		if err := v.ctx.Err(); err != nil {
			v.err = err
			return err
		}
		k := v.source.GetTopKey()
		if len(v.embeddingCF) == 0 || bytesEqual(k.ColumnFamily, v.embeddingCF) {
			if vec, err := unpackFloat32BE(v.source.GetTopValue()); err == nil && len(vec) == len(v.query) {
				score := v.score(vec)
				if score >= v.minScore {
					h.offer(&knnScored{key: k.Clone(), score: score})
				}
			}
		}
		if err := v.source.Next(); err != nil {
			v.err = err
			return err
		}
	}

	scored := h.drainDescending()
	for _, s := range scored {
		buf := make([]byte, 4)
		binary.BigEndian.PutUint32(buf, math.Float32bits(s.score))
		v.out = append(v.out, Cell{Key: s.key, Value: buf})
	}
	return nil
}

// score computes the similarity of vec to the query under the configured
// metric. Larger is always better: cosine and dot are returned directly; l2
// is returned as the negated squared distance.
func (v *VectorKNNIterator) score(vec []float32) float32 {
	switch v.metric {
	case vectorKNNMetricDot:
		return knnDot(v.query, vec)
	case vectorKNNMetricL2:
		var sum float32
		for i := range vec {
			d := v.query[i] - vec[i]
			sum += d * d
		}
		return -sum
	default: // cosine
		den := v.queryNorm * knnNorm(vec)
		if den == 0 {
			return 0
		}
		return knnDot(v.query, vec) / den
	}
}

// HasTop reports whether a cell is available.
func (v *VectorKNNIterator) HasTop() bool {
	return v.err == nil && v.outIndex < len(v.out)
}

// GetTopKey returns the current top key, or nil when exhausted.
func (v *VectorKNNIterator) GetTopKey() *Key {
	if !v.HasTop() {
		return nil
	}
	return v.out[v.outIndex].Key
}

// GetTopValue returns the current top value (4-byte big-endian float32 score),
// or nil when exhausted.
func (v *VectorKNNIterator) GetTopValue() []byte {
	if !v.HasTop() {
		return nil
	}
	return v.out[v.outIndex].Value
}

// Next advances past the current top.
func (v *VectorKNNIterator) Next() error {
	if v.err != nil {
		return v.err
	}
	if !v.HasTop() {
		return errors.New("iterrt: VectorKNNIterator.Next called without a top")
	}
	v.outIndex++
	return nil
}

// DeepCopy returns an un-Seeked iterator over a DeepCopy'd source, carrying
// the same resolved options forward.
func (v *VectorKNNIterator) DeepCopy(env IteratorEnvironment) SortedKeyValueIterator {
	cp := &VectorKNNIterator{
		source:      v.source.DeepCopy(env),
		ctx:         env.Context,
		query:       v.query,
		queryNorm:   v.queryNorm,
		querySpace:  v.querySpace,
		topK:        v.topK,
		embeddingCF: v.embeddingCF,
		metric:      v.metric,
		minScore:    v.minScore,
	}
	if cp.ctx == nil {
		cp.ctx = context.Background()
	}
	return cp
}

// ── helpers ──────────────────────────────────────────────────────────────────

// unpackFloat32BE parses a packed big-endian float32 vector. The byte length
// must be a non-negative multiple of 4.
func unpackFloat32BE(b []byte) ([]float32, error) {
	if len(b)%4 != 0 {
		return nil, fmt.Errorf("byte length %d is not a multiple of 4", len(b))
	}
	out := make([]float32, len(b)/4)
	for i := range out {
		out[i] = math.Float32frombits(binary.BigEndian.Uint32(b[i*4:]))
	}
	return out, nil
}

func knnDot(a, b []float32) float32 {
	var s float32
	for i := range a {
		s += a[i] * b[i]
	}
	return s
}

func knnNorm(a []float32) float32 {
	return float32(math.Sqrt(float64(knnDot(a, a))))
}

func bytesEqual(a, b []byte) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// knnScored is one candidate retained by the bounded heap.
type knnScored struct {
	key   *Key
	score float32
}

// knnHeap is a bounded min-heap keyed on score: when size exceeds cap the
// smallest element is evicted on every offer, leaving the cap largest. A
// stable tie-break on the cell key keeps the result deterministic when scores
// are equal.
type knnHeap struct {
	items []*knnScored
	cap   int
}

func (h *knnHeap) Len() int { return len(h.items) }
func (h *knnHeap) Less(i, j int) bool {
	if h.items[i].score != h.items[j].score {
		return h.items[i].score < h.items[j].score
	}
	// Larger key sorts "smaller" so it is evicted first — keeps the
	// lexicographically smaller key among equal scores (deterministic).
	return h.items[i].key.Compare(h.items[j].key) > 0
}
func (h *knnHeap) Swap(i, j int) { h.items[i], h.items[j] = h.items[j], h.items[i] }
func (h *knnHeap) Push(x any)    { h.items = append(h.items, x.(*knnScored)) }
func (h *knnHeap) Pop() any {
	n := len(h.items)
	x := h.items[n-1]
	h.items = h.items[:n-1]
	return x
}

func (h *knnHeap) offer(s *knnScored) {
	heap.Push(h, s)
	if h.cap > 0 && h.Len() > h.cap {
		heap.Pop(h)
	}
}

// drainDescending returns the retained cells in score-descending order, ties
// broken by ascending cell key for determinism.
func (h *knnHeap) drainDescending() []*knnScored {
	out := make([]*knnScored, len(h.items))
	copy(out, h.items)
	h.items = h.items[:0]
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].score != out[j].score {
			return out[i].score > out[j].score
		}
		return out[i].key.Compare(out[j].key) < 0
	})
	return out
}
