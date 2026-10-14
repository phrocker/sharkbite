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

// merge.go merges the EntryStreams of the multiple WAL segments that make up
// a tablet's full pre-flush WAL set into one ordered stream.
//
// Ordering mirrors SortedLogRecovery's recovery path: RecoveryLogsIterator
// does Iterators.mergeSorted(logs, Entry.comparingByKey()), i.e. it merges by
// LogFileKey.compareTo. That comparator is
// (eventType, tabletId, seq) — eventType groups OPEN(0), DEFINE_TABLET(1),
// COMPACTION_*(2), MUTATION/MANY_MUTATIONS(3) so all of a tablet's mutations
// land after its DEFINE_TABLET, then ascending by per-tablet seq.
//
// Within one segment WAL entries are already in append order and a segment's
// seq is monotonic per tablet, so a k-way merge of the per-segment streams by
// that comparator yields the same total order recovery replays. The segment
// index breaks ties deterministically (two segments should never carry the
// same (event,tablet,seq) for committed data, but a heap needs a total order).
package qwal

import (
	"container/heap"
	"io"
)

// compareLogFileKey is a Go port of LogFileKey.compareTo
// (server/tserver/.../logger/LogFileKey.java). It returns -1, 0, or 1.
func compareLogFileKey(a, b LogFileKey) int {
	ea, eb := eventType(a.Event), eventType(b.Event)
	if ea != eb {
		if ea < eb {
			return -1
		}
		return 1
	}
	// OPEN entries compare equal beyond event type (Java returns 0 here).
	if a.Event == EventOpen {
		return 0
	}
	if a.TabletID != b.TabletID {
		if a.TabletID < b.TabletID {
			return -1
		}
		return 1
	}
	if a.Seq != b.Seq {
		if a.Seq < b.Seq {
			return -1
		}
		return 1
	}
	return 0
}

// eventType groups events for the recovery sort order — a direct port of
// LogFileKey.eventType: START, TABLET_DEFINITIONS, COMPACTIONS, then
// MUTATIONS.
func eventType(e LogEvent) int {
	switch e {
	case EventMutation, EventManyMutations:
		return 3
	case EventDefineTablet:
		return 1
	case EventOpen:
		return 0
	default: // COMPACTION_START, COMPACTION_FINISH
		return 2
	}
}

// SegmentSource is anything that yields WAL entries in append order.
// *EntryStream satisfies it; tests and the scanserver's W2 read path
// supply in-memory or adapter fixtures that drain the same shape into
// the MergedStream.
type SegmentSource interface {
	Next() (*Entry, error)
}

// segmentSource is the original (now-deprecated) internal alias. Retained
// for the existing tests; new external consumers use SegmentSource.
type segmentSource = SegmentSource

// mergeItem is one peeked entry from a segment, parked in the heap.
type mergeItem struct {
	entry   *Entry
	segment int // index into MergedStream.sources; the tiebreaker
}

type mergeHeap []mergeItem

func (h mergeHeap) Len() int { return len(h) }
func (h mergeHeap) Less(i, j int) bool {
	if c := compareLogFileKey(h[i].entry.Key, h[j].entry.Key); c != 0 {
		return c < 0
	}
	// Equal keys: earlier segment first, so the merged order is stable.
	return h[i].segment < h[j].segment
}
func (h mergeHeap) Swap(i, j int) { h[i], h[j] = h[j], h[i] }
func (h *mergeHeap) Push(x any)   { *h = append(*h, x.(mergeItem)) }
func (h *mergeHeap) Pop() any {
	old := *h
	n := len(old)
	it := old[n-1]
	*h = old[:n-1]
	return it
}

// MergedStream is a k-way merge of multiple segment EntryStreams in the
// recovery sort order. It surfaces the same Next/io.EOF contract as a single
// EntryStream so the W1 memtable consumes either shape unchanged.
type MergedStream struct {
	sources []segmentSource
	h       mergeHeap
	started bool
	err     error
}

// NewMergedStream builds a merge over the given per-segment sources. The
// sources should be a tablet's full WAL set; order of the slice only affects
// the tiebreak between entries with an identical (event,tablet,seq).
func NewMergedStream(sources ...SegmentSource) *MergedStream {
	return &MergedStream{sources: sources}
}

// MergeStreams is the convenience constructor over resolved *EntryStreams.
func MergeStreams(streams ...*EntryStream) *MergedStream {
	srcs := make([]segmentSource, len(streams))
	for i, s := range streams {
		srcs[i] = s
	}
	return &MergedStream{sources: srcs}
}

// prime pulls the first entry from every source into the heap. A source that
// is empty (immediate io.EOF) is simply absent from the heap. The first hard
// decode error from any source is recorded and surfaced from Next.
func (m *MergedStream) prime() {
	m.started = true
	m.h = make(mergeHeap, 0, len(m.sources))
	for i, src := range m.sources {
		e, err := src.Next()
		if err == io.EOF {
			continue
		}
		if err != nil {
			m.err = err
			return
		}
		m.h = append(m.h, mergeItem{entry: e, segment: i})
	}
	heap.Init(&m.h)
}

// Next returns the next entry in merged recovery order, io.EOF at the end of
// every source, or the first decode error encountered on any source.
func (m *MergedStream) Next() (*Entry, error) {
	if !m.started {
		m.prime()
	}
	if m.err != nil {
		return nil, m.err
	}
	if m.h.Len() == 0 {
		return nil, io.EOF
	}
	top := heap.Pop(&m.h).(mergeItem)
	// Refill from the segment the popped entry came from.
	next, err := m.sources[top.segment].Next()
	if err == nil {
		heap.Push(&m.h, mergeItem{entry: next, segment: top.segment})
	} else if err != io.EOF {
		m.err = err
	}
	return top.entry, nil
}

// Close closes every underlying *EntryStream. Sources that are not
// io.Closer (test fixtures) are skipped.
func (m *MergedStream) Close() error {
	var first error
	for _, src := range m.sources {
		if c, ok := src.(io.Closer); ok {
			if err := c.Close(); err != nil && first == nil {
				first = err
			}
		}
	}
	return first
}
