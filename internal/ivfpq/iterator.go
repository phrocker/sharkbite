package ivfpq

import (
	"bytes"
	"container/heap"
	"encoding/base64"
	"encoding/binary"
	"fmt"
	"math"
	"sort"
	"strconv"
)

// Iterator class name as registered in Accumulo on the Java side. The
// Go SDK matches against this exact string when deciding whether to
// engage the shoal-side IVF PQ path.
const IteratorClassName = "org.apache.accumulo.core.graph.ann.IvfPqDistanceIterator"

// Option keys (mirror the constants in IvfPqDistanceIterator.java).
const (
	OptQuery     = "query.b64"
	OptPQCodebook = "pq.b64"
	OptTopK      = "topK"
	OptThreshold = "threshold"
)

// Iterator is the Go equivalent of IvfPqDistanceIterator. Construct
// once per scan via NewFromOptions, then call Score on each (rowKey,
// pqCode) pair, finally call Drain to recover the top-K in score-
// descending order.
//
// Lifetime: NOT goroutine-safe. One Iterator per scan-server scan.
type Iterator struct {
	pq        *VectorPQ
	query     []float32
	ipTable   [][]float32
	topK      int
	threshold float32

	heap *minHeap
}

// scoredCell is what the iterator emits — the original row + the
// approximate cosine score. The Java side stores Key.row(), but shoal
// at this level only has bytes; downstream code assembles the TKey
// before wire-shipping.
type scoredCell struct {
	row   []byte
	cf    []byte
	cq    []byte
	cv    []byte
	ts    int64
	score float32
}

// NewFromOptions parses the iterator config (the same map<String,
// String> the Java side stores in IteratorSetting options) and
// instantiates a ready-to-score iterator. Returns an error if any
// required option is missing or malformed.
func NewFromOptions(opts map[string]string) (*Iterator, error) {
	queryB64, ok := opts[OptQuery]
	if !ok {
		return nil, fmt.Errorf("ivfpq iterator: missing %q", OptQuery)
	}
	pqB64, ok := opts[OptPQCodebook]
	if !ok {
		return nil, fmt.Errorf("ivfpq iterator: missing %q", OptPQCodebook)
	}
	pqBytes, err := base64.StdEncoding.DecodeString(pqB64)
	if err != nil {
		return nil, fmt.Errorf("ivfpq iterator: decode %q: %w", OptPQCodebook, err)
	}
	pq, err := FromBytes(pqBytes)
	if err != nil {
		return nil, fmt.Errorf("ivfpq iterator: parse codebook: %w", err)
	}
	queryBytes, err := base64.StdEncoding.DecodeString(queryB64)
	if err != nil {
		return nil, fmt.Errorf("ivfpq iterator: decode %q: %w", OptQuery, err)
	}
	if len(queryBytes) != pq.dim*4 {
		return nil, fmt.Errorf("ivfpq iterator: query bytes %d != dim*4 %d",
			len(queryBytes), pq.dim*4)
	}
	query := make([]float32, pq.dim)
	if err := binary.Read(bytes.NewReader(queryBytes), binary.BigEndian, query); err != nil {
		return nil, fmt.Errorf("ivfpq iterator: parse query: %w", err)
	}
	ipTable, err := pq.InnerProductTable(query)
	if err != nil {
		return nil, fmt.Errorf("ivfpq iterator: build IP table: %w", err)
	}

	topK := 10
	if v, ok := opts[OptTopK]; ok {
		n, err := strconv.Atoi(v)
		if err != nil || n <= 0 {
			return nil, fmt.Errorf("ivfpq iterator: bad %q=%q", OptTopK, v)
		}
		topK = n
	}
	threshold := float32(-3.4028235e38) // -Float.MAX_VALUE
	if v, ok := opts[OptThreshold]; ok {
		f, err := strconv.ParseFloat(v, 32)
		if err != nil {
			return nil, fmt.Errorf("ivfpq iterator: bad %q=%q", OptThreshold, v)
		}
		threshold = float32(f)
	}

	return &Iterator{
		pq:        pq,
		query:     query,
		ipTable:   ipTable,
		topK:      topK,
		threshold: threshold,
		heap:      &minHeap{cap: topK},
	}, nil
}

// Offer evaluates one cell against the iterator. The cell is a (row,
// cf, cq, cv, ts, value) tuple where value is expected to be a PQ-
// encoded byte vector of length M. If the value is the wrong length,
// or the score is below threshold, the cell is silently dropped (same
// behavior as the Java iterator).
func (it *Iterator) Offer(row, cf, cq, cv []byte, ts int64, value []byte) {
	if len(value) != it.pq.m {
		return
	}
	score := it.pq.Dot(value, it.ipTable)
	if score < it.threshold {
		return
	}
	it.heap.push(&scoredCell{
		row: row, cf: cf, cq: cq, cv: cv, ts: ts, score: score,
	})
}

// Drain returns the top-K cells in score-descending order, then
// resets the iterator's heap so the same instance can be reused.
//
// The encoded Value carries the score as a big-endian float32 — same
// wire format the Java client decodes via IvfPqDistanceIterator.decodeScore.
type DrainedCell struct {
	Row   []byte
	CF    []byte
	CQ    []byte
	CV    []byte
	TS    int64
	Value []byte // 4 bytes, big-endian float32 score
}

func (it *Iterator) Drain() []DrainedCell {
	scored := it.heap.collect()
	sort.SliceStable(scored, func(i, j int) bool {
		return scored[i].score > scored[j].score // descending
	})
	out := make([]DrainedCell, len(scored))
	for i, s := range scored {
		buf := make([]byte, 4)
		binary.BigEndian.PutUint32(buf, math.Float32bits(s.score))
		out[i] = DrainedCell{
			Row:   s.row,
			CF:    s.cf,
			CQ:    s.cq,
			CV:    s.cv,
			TS:    s.ts,
			Value: buf,
		}
	}
	return out
}

// minHeap is a bounded min-heap keyed on score. When size exceeds cap,
// the smallest element is evicted on every push — leaving the K
// largest. Sort + reverse on collect gives top-K descending.
type minHeap struct {
	items []*scoredCell
	cap   int
}

func (h *minHeap) Len() int           { return len(h.items) }
func (h *minHeap) Less(i, j int) bool { return h.items[i].score < h.items[j].score }
func (h *minHeap) Swap(i, j int)      { h.items[i], h.items[j] = h.items[j], h.items[i] }
func (h *minHeap) Push(x any)         { h.items = append(h.items, x.(*scoredCell)) }
func (h *minHeap) Pop() any {
	n := len(h.items)
	x := h.items[n-1]
	h.items = h.items[:n-1]
	return x
}

func (h *minHeap) push(s *scoredCell) {
	heap.Push(h, s)
	if h.cap > 0 && h.Len() > h.cap {
		heap.Pop(h)
	}
}

func (h *minHeap) collect() []*scoredCell {
	out := make([]*scoredCell, 0, h.Len())
	out = append(out, h.items...)
	h.items = h.items[:0]
	return out
}

