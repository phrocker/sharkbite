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

// Package memtable is shoal's in-memory WAL merger — the Phase W1 piece that
// turns a tablet's append-order WAL stream into a key-sorted, SKVI-readable
// MemTable-equivalent (design doc, Bet 2 "Mutation merger").
//
// A Memtable ingests decoded WAL entries (qwal.Entry values) for ONE tablet,
// sorts every column update into a skiplist keyed by wire.Key.Compare, and
// exposes the sorted contents as an iterrt.SortedKeyValueIterator. That leaf
// SKVI is what the parallel iterrt track stacks VersioningIterator + the
// visibility filter on top of before merging with the RFile cache.
//
// Flush watermark: data already flushed to RFiles must not be re-served from
// the WAL. The tserver flushes a tablet up to some per-tablet WAL sequence;
// only WAL entries with LogFileKey.Seq strictly greater than that watermark
// are retained. Entries at or below it are dropped at Ingest time.
package memtable

import (
	"io"

	"github.com/phrocker/shoal-oss/internal/cclient"
	"github.com/phrocker/shoal-oss/internal/iterrt"
	"github.com/phrocker/shoal-oss/internal/qwal"
	"github.com/phrocker/shoal-oss/internal/rfile/wire"
)

// entrySource is the qwal stream shape the Memtable drains — satisfied by
// both *qwal.EntryStream and *qwal.MergedStream.
type entrySource interface {
	Next() (*qwal.Entry, error)
}

// Memtable is the in-memory sorted merge of a single tablet's pre-flush WAL
// mutations. It is not safe for concurrent Ingest; build it fully, then take
// any number of independent Iterator()s to read it.
type Memtable struct {
	// tabletID scopes ingestion: WAL entries are append-ordered across all of
	// a tserver's tablets, so a multi-tablet segment stream must be filtered
	// to this tablet. -1 means "accept every tablet" (single-tablet fixtures).
	tabletID int32

	// watermark is the per-tablet flush sequence: only entries with
	// LogFileKey.Seq > watermark are retained (older seqs are already in
	// RFiles). A watermark of -1 retains everything.
	watermark int64

	list *skiplist

	// skipped counts entries dropped by the watermark window — surfaced for
	// observability (a persistently high count means shoal is reading WAL it
	// no longer needs to).
	skipped int
}

// Options configures a Memtable.
type Options struct {
	// TabletID restricts ingestion to one tablet. Use AllTablets for fixtures
	// or single-tablet segments where filtering is unnecessary.
	TabletID int32
	// Watermark is the tablet's last-flushed WAL sequence; entries with
	// Seq <= Watermark are dropped. Use NoWatermark to retain everything.
	Watermark int64
	// Seed seeds the skiplist's level RNG. Zero is fine for production; tests
	// pin it for deterministic structure.
	Seed int64
}

const (
	// AllTablets disables the per-tablet ingestion filter.
	AllTablets int32 = -1
	// NoWatermark disables the flush-window filter (retain every entry).
	NoWatermark int64 = -1
)

// New builds an empty Memtable for the given tablet/watermark.
func New(opts Options) *Memtable {
	return &Memtable{
		tabletID:  opts.TabletID,
		watermark: opts.Watermark,
		list:      newSkiplist(opts.Seed),
	}
}

// Ingest folds one decoded WAL entry into the sorted structure. Non-mutation
// events (OPEN, DEFINE_TABLET, COMPACTION_*) carry no cells and are ignored.
// Mutation/ManyMutations entries outside this Memtable's tablet, or at or
// below the flush watermark, are dropped.
func (m *Memtable) Ingest(e *qwal.Entry) {
	switch e.Key.Event {
	case qwal.EventMutation, qwal.EventManyMutations:
	default:
		return
	}
	if m.tabletID != AllTablets && e.Key.TabletID != m.tabletID {
		return
	}
	// Seq is per-tablet monotonic; everything <= the flush watermark is
	// already durable in an RFile and must not be double-served.
	if m.watermark != NoWatermark && e.Key.Seq <= m.watermark {
		m.skipped++
		return
	}
	for _, mut := range e.Value.Mutations {
		m.ingestMutation(mut)
	}
}

// ingestMutation projects one Mutation's column updates onto wire.Keys and
// inserts each. The cclient.Cell slices alias the Mutation's buffers; the
// Mutation came from a per-entry decode (qwal.readValue allocates fresh
// backing arrays per record), so aliasing is safe for the Memtable's
// lifetime — it holds the only reference once Ingest returns.
func (m *Memtable) ingestMutation(mut *cclient.Mutation) {
	for _, c := range mut.Cells() {
		m.list.insert(c.Key, c.Value)
	}
}

// IngestStream drains a qwal stream (single segment or a MergedStream) into
// the Memtable, returning the first decode error or nil at clean EOF.
func (m *Memtable) IngestStream(s entrySource) error {
	for {
		e, err := s.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		m.Ingest(e)
	}
}

// Len reports the number of cells retained (post-watermark, post-tablet
// filter). Counts duplicate coordinates separately — it is a cell count, not
// a distinct-key count.
func (m *Memtable) Len() int { return m.list.len() }

// Skipped reports how many mutation entries the flush watermark dropped.
func (m *Memtable) Skipped() int { return m.skipped }

// Iterator returns an independent leaf iterrt.SortedKeyValueIterator over the
// sorted contents. Multiple iterators over one Memtable are safe (the
// skiplist is read-only once built). The returned iterator is NOT yet
// Seek'd, per the SKVI contract.
func (m *Memtable) Iterator() iterrt.SortedKeyValueIterator {
	return &memIter{list: m.list}
}

// memIter is the leaf SKVI over a built skiplist. It honors the Range bounds
// and the column-family include/exclude filter from Seek; it does no version
// collapsing or visibility filtering — those are separate stacked iterators.
type memIter struct {
	list *skiplist

	rng    iterrt.Range
	cfSet  map[string]bool
	cfIncl bool
	hasCF  bool

	cur *node
}

// Init records nothing — the leaf has no source and no options. It exists to
// satisfy the SKVI contract.
func (it *memIter) Init(source iterrt.SortedKeyValueIterator, options map[string]string, env iterrt.IteratorEnvironment) error {
	return nil
}

// Seek positions at the first cell >= the range start that also passes the
// column-family filter, then advances past anything beyond the range end.
func (it *memIter) Seek(r iterrt.Range, columnFamilies [][]byte, inclusive bool) error {
	it.rng = r
	it.hasCF = len(columnFamilies) > 0
	it.cfIncl = inclusive
	if it.hasCF {
		it.cfSet = make(map[string]bool, len(columnFamilies))
		for _, cf := range columnFamilies {
			it.cfSet[string(cf)] = true
		}
	} else {
		it.cfSet = nil
	}

	if r.InfiniteStart || r.Start == nil {
		it.cur = it.list.first()
	} else {
		it.cur = it.list.seekFirst(r.Start)
		// seekFirst lands on the first key >= Start; if Start is exclusive,
		// skip any cells equal to Start.
		if !r.StartInclusive {
			for it.cur != nil && it.cur.key.Compare(r.Start) == 0 {
				it.cur = it.cur.next[0]
			}
		}
	}
	it.skipToVisible()
	return nil
}

// Next advances past the current top.
func (it *memIter) Next() error {
	if it.cur == nil {
		return nil
	}
	it.cur = it.cur.next[0]
	it.skipToVisible()
	return nil
}

// skipToVisible advances cur past cells the CF filter excludes, and clears it
// once iteration runs past the range end.
func (it *memIter) skipToVisible() {
	for it.cur != nil {
		if it.rng.AfterEnd(&it.cur.key) {
			it.cur = nil
			return
		}
		if it.cfPasses(&it.cur.key) {
			return
		}
		it.cur = it.cur.next[0]
	}
}

// cfPasses applies the Seek column-family include/exclude semantics: with
// inclusive=true only families in the set pass; with inclusive=false only
// families NOT in the set pass; with no set, everything passes.
func (it *memIter) cfPasses(k *wire.Key) bool {
	if !it.hasCF {
		return true
	}
	in := it.cfSet[string(k.ColumnFamily)]
	if it.cfIncl {
		return in
	}
	return !in
}

// HasTop reports whether a top cell is available.
func (it *memIter) HasTop() bool { return it.cur != nil }

// GetTopKey returns the current key. Owned by the iterator — invalidated by
// the next Seek/Next. The skiplist itself is immutable, so the *Key stays
// valid as long as the caller does not advance; callers retaining it past an
// advance must Clone (the SKVI contract).
func (it *memIter) GetTopKey() *iterrt.Key {
	if it.cur == nil {
		return nil
	}
	return &it.cur.key
}

// GetTopValue returns the current value, same transient-lifetime rule.
func (it *memIter) GetTopValue() []byte {
	if it.cur == nil {
		return nil
	}
	return it.cur.value
}

// DeepCopy returns an independent, un-Seek'd iterator over the same
// (immutable) skiplist.
func (it *memIter) DeepCopy(env iterrt.IteratorEnvironment) iterrt.SortedKeyValueIterator {
	return &memIter{list: it.list}
}
