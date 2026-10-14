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

package memtable

import (
	"io"
	"testing"

	"github.com/phrocker/shoal-oss/internal/cclient"
	"github.com/phrocker/shoal-oss/internal/iterrt"
	"github.com/phrocker/shoal-oss/internal/qwal"
	"github.com/phrocker/shoal-oss/internal/rfile/wire"
)

// putEntry builds a single-mutation MUTATION WAL entry for the given tablet
// and seq, with one Put column update.
func putEntry(tabletID int32, seq int64, row, cf, cq string, ts int64, val string) *qwal.Entry {
	m, err := cclient.NewMutation([]byte(row))
	if err != nil {
		panic(err)
	}
	m.Put([]byte(cf), []byte(cq), nil, ts, []byte(val))
	return &qwal.Entry{
		Key:   qwal.LogFileKey{Event: qwal.EventMutation, TabletID: tabletID, Seq: seq},
		Value: qwal.LogFileValue{Mutations: []*cclient.Mutation{m}},
	}
}

func delEntry(tabletID int32, seq int64, row, cf, cq string, ts int64) *qwal.Entry {
	m, err := cclient.NewMutation([]byte(row))
	if err != nil {
		panic(err)
	}
	m.Delete([]byte(cf), []byte(cq), nil, ts)
	return &qwal.Entry{
		Key:   qwal.LogFileKey{Event: qwal.EventMutation, TabletID: tabletID, Seq: seq},
		Value: qwal.LogFileValue{Mutations: []*cclient.Mutation{m}},
	}
}

// collect drains a freshly-Seek'd SKVI over the full table range and returns
// the (row, cf, cq, ts, deleted, value) tuples in iteration order.
type cell struct {
	row, cf, cq string
	ts          int64
	deleted     bool
	value       string
}

func collect(t *testing.T, it iterrt.SortedKeyValueIterator) []cell {
	t.Helper()
	if err := it.Seek(iterrt.InfiniteRange(), nil, false); err != nil {
		t.Fatalf("Seek: %v", err)
	}
	var out []cell
	for it.HasTop() {
		k := it.GetTopKey()
		out = append(out, cell{
			row: string(k.Row), cf: string(k.ColumnFamily), cq: string(k.ColumnQualifier),
			ts: k.Timestamp, deleted: k.Deleted, value: string(it.GetTopValue()),
		})
		if err := it.Next(); err != nil {
			t.Fatalf("Next: %v", err)
		}
	}
	return out
}

func TestMemtable_SortsAppendOrderIntoKeyOrder(t *testing.T) {
	mt := New(Options{TabletID: AllTablets, Watermark: NoWatermark, Seed: 1})
	// Ingested out of key order — WAL is append-order, not sorted.
	mt.Ingest(putEntry(1, 3, "row-c", "cf", "q", 10, "c"))
	mt.Ingest(putEntry(1, 1, "row-a", "cf", "q", 10, "a"))
	mt.Ingest(putEntry(1, 2, "row-b", "cf", "q", 10, "b"))

	got := collect(t, mt.Iterator())
	want := []string{"row-a", "row-b", "row-c"}
	if len(got) != len(want) {
		t.Fatalf("got %d cells, want %d", len(got), len(want))
	}
	for i, w := range want {
		if got[i].row != w {
			t.Errorf("cell %d row = %q, want %q", i, got[i].row, w)
		}
	}
}

func TestMemtable_TimestampDescendingAndTombstoneFirst(t *testing.T) {
	mt := New(Options{TabletID: AllTablets, Watermark: NoWatermark, Seed: 2})
	// Same coordinate, three versions plus a tombstone — ingested in a
	// deliberately scrambled order.
	mt.Ingest(putEntry(1, 1, "r", "cf", "q", 50, "v50"))
	mt.Ingest(putEntry(1, 2, "r", "cf", "q", 100, "v100"))
	mt.Ingest(delEntry(1, 3, "r", "cf", "q", 100))
	mt.Ingest(putEntry(1, 4, "r", "cf", "q", 75, "v75"))

	got := collect(t, mt.Iterator())
	if len(got) != 4 {
		t.Fatalf("expected 4 cells, got %d: %+v", len(got), got)
	}
	// ts DESCENDING; at ts=100 the tombstone sorts before the live cell.
	if !(got[0].ts == 100 && got[0].deleted) {
		t.Errorf("cell 0 should be the ts=100 tombstone, got %+v", got[0])
	}
	if !(got[1].ts == 100 && !got[1].deleted) {
		t.Errorf("cell 1 should be the ts=100 live cell, got %+v", got[1])
	}
	if got[2].ts != 75 || got[3].ts != 50 {
		t.Errorf("remaining cells should be ts=75 then ts=50, got %d,%d", got[2].ts, got[3].ts)
	}
}

func TestMemtable_WatermarkWindowsOutFlushedSeqs(t *testing.T) {
	// Watermark 10: only entries with Seq > 10 are retained.
	mt := New(Options{TabletID: 1, Watermark: 10, Seed: 3})
	mt.Ingest(putEntry(1, 8, "r", "cf", "old8", 1, "x"))   // dropped
	mt.Ingest(putEntry(1, 10, "r", "cf", "at10", 1, "x"))  // dropped (== watermark)
	mt.Ingest(putEntry(1, 11, "r", "cf", "new11", 1, "x")) // kept
	mt.Ingest(putEntry(1, 20, "r", "cf", "new20", 1, "x")) // kept

	if mt.Len() != 2 {
		t.Errorf("Len = %d, want 2 (only Seq>10 retained)", mt.Len())
	}
	if mt.Skipped() != 2 {
		t.Errorf("Skipped = %d, want 2", mt.Skipped())
	}
	got := collect(t, mt.Iterator())
	for _, c := range got {
		if c.cq == "old8" || c.cq == "at10" {
			t.Errorf("flushed entry %q should not be served from the WAL", c.cq)
		}
	}
}

func TestMemtable_FiltersByTablet(t *testing.T) {
	mt := New(Options{TabletID: 7, Watermark: NoWatermark, Seed: 4})
	mt.Ingest(putEntry(7, 1, "r", "cf", "mine", 1, "x"))
	mt.Ingest(putEntry(8, 1, "r", "cf", "other", 1, "x")) // different tablet, dropped
	mt.Ingest(putEntry(7, 2, "r", "cf", "mine2", 1, "x"))

	if mt.Len() != 2 {
		t.Fatalf("Len = %d, want 2 (only tablet 7)", mt.Len())
	}
	for _, c := range collect(t, mt.Iterator()) {
		if c.cq == "other" {
			t.Error("entry from another tablet leaked into the memtable")
		}
	}
}

func TestMemtable_IgnoresNonMutationEvents(t *testing.T) {
	mt := New(Options{TabletID: AllTablets, Watermark: NoWatermark, Seed: 5})
	mt.Ingest(&qwal.Entry{Key: qwal.LogFileKey{Event: qwal.EventOpen}})
	mt.Ingest(&qwal.Entry{Key: qwal.LogFileKey{Event: qwal.EventDefineTablet, TabletID: 1, Seq: 0}})
	mt.Ingest(&qwal.Entry{Key: qwal.LogFileKey{Event: qwal.EventCompactionStart, TabletID: 1, Seq: 1}})
	if mt.Len() != 0 {
		t.Errorf("non-mutation events should contribute no cells, Len = %d", mt.Len())
	}
}

func TestMemtable_SeekRangeBounds(t *testing.T) {
	mt := New(Options{TabletID: AllTablets, Watermark: NoWatermark, Seed: 6})
	for i, r := range []string{"a", "b", "c", "d", "e"} {
		mt.Ingest(putEntry(1, int64(i), r, "cf", "q", 1, r))
	}
	it := mt.Iterator()

	// Inclusive start "b", inclusive end "d".
	start := &wire.Key{Row: []byte("b"), ColumnFamily: []byte("cf"), ColumnQualifier: []byte("q"), Timestamp: 1}
	end := &wire.Key{Row: []byte("d"), ColumnFamily: []byte("cf"), ColumnQualifier: []byte("q"), Timestamp: 1}
	r := iterrt.Range{Start: start, StartInclusive: true, End: end, EndInclusive: true}
	if err := it.Seek(r, nil, false); err != nil {
		t.Fatalf("Seek: %v", err)
	}
	var rows []string
	for it.HasTop() {
		rows = append(rows, string(it.GetTopKey().Row))
		it.Next()
	}
	if len(rows) != 3 || rows[0] != "b" || rows[2] != "d" {
		t.Errorf("inclusive [b,d] range = %v, want [b c d]", rows)
	}

	// Exclusive start "b" should drop "b".
	r2 := iterrt.Range{Start: start, StartInclusive: false, InfiniteEnd: true}
	if err := it.Seek(r2, nil, false); err != nil {
		t.Fatalf("Seek: %v", err)
	}
	if !it.HasTop() || string(it.GetTopKey().Row) != "c" {
		t.Errorf("exclusive start should land on c, got hasTop=%v", it.HasTop())
	}
}

func TestMemtable_SeekColumnFamilyFilter(t *testing.T) {
	mt := New(Options{TabletID: AllTablets, Watermark: NoWatermark, Seed: 7})
	mt.Ingest(putEntry(1, 0, "r", "cfA", "q", 1, "a"))
	mt.Ingest(putEntry(1, 1, "r", "cfB", "q", 1, "b"))
	mt.Ingest(putEntry(1, 2, "r", "cfC", "q", 1, "c"))
	it := mt.Iterator()

	// inclusive=true: only cfB passes.
	if err := it.Seek(iterrt.InfiniteRange(), [][]byte{[]byte("cfB")}, true); err != nil {
		t.Fatalf("Seek: %v", err)
	}
	var incl []string
	for it.HasTop() {
		incl = append(incl, string(it.GetTopKey().ColumnFamily))
		it.Next()
	}
	if len(incl) != 1 || incl[0] != "cfB" {
		t.Errorf("inclusive cf filter = %v, want [cfB]", incl)
	}

	// inclusive=false: cfB excluded, cfA and cfC pass.
	if err := it.Seek(iterrt.InfiniteRange(), [][]byte{[]byte("cfB")}, false); err != nil {
		t.Fatalf("Seek: %v", err)
	}
	var excl []string
	for it.HasTop() {
		excl = append(excl, string(it.GetTopKey().ColumnFamily))
		it.Next()
	}
	if len(excl) != 2 || excl[0] != "cfA" || excl[1] != "cfC" {
		t.Errorf("exclusive cf filter = %v, want [cfA cfC]", excl)
	}
}

func TestMemtable_DeepCopyIsIndependent(t *testing.T) {
	mt := New(Options{TabletID: AllTablets, Watermark: NoWatermark, Seed: 8})
	mt.Ingest(putEntry(1, 0, "a", "cf", "q", 1, "a"))
	mt.Ingest(putEntry(1, 1, "b", "cf", "q", 1, "b"))

	it := mt.Iterator()
	it.Seek(iterrt.InfiniteRange(), nil, false)
	it.Next() // advance original to "b"

	cp := it.DeepCopy(iterrt.IteratorEnvironment{})
	// Copy is un-Seek'd per the SKVI contract.
	if cp.HasTop() {
		t.Error("DeepCopy result must not have a top before Seek")
	}
	cp.Seek(iterrt.InfiniteRange(), nil, false)
	if !cp.HasTop() || string(cp.GetTopKey().Row) != "a" {
		t.Error("copy should start at the first row independently of the original")
	}
	if !it.HasTop() || string(it.GetTopKey().Row) != "b" {
		t.Error("original iterator position should be unaffected by DeepCopy")
	}
}

func TestMemtable_IngestStreamFromMergedSegments(t *testing.T) {
	// End-to-end: two append-ordered WAL segments merged, then ingested.
	segA := &fakeStream{entries: []*qwal.Entry{
		putEntry(1, 0, "row-a", "cf", "q", 1, "a"),
		putEntry(1, 2, "row-c", "cf", "q", 1, "c"),
	}}
	segB := &fakeStream{entries: []*qwal.Entry{
		putEntry(1, 1, "row-b", "cf", "q", 1, "b"),
		putEntry(1, 3, "row-d", "cf", "q", 1, "d"),
	}}
	merged := qwal.NewMergedStream(segA, segB)

	mt := New(Options{TabletID: 1, Watermark: NoWatermark, Seed: 9})
	if err := mt.IngestStream(merged); err != nil {
		t.Fatalf("IngestStream: %v", err)
	}
	got := collect(t, mt.Iterator())
	want := []string{"row-a", "row-b", "row-c", "row-d"}
	if len(got) != len(want) {
		t.Fatalf("got %d cells, want %d", len(got), len(want))
	}
	for i, w := range want {
		if got[i].row != w {
			t.Errorf("cell %d = %q, want %q", i, got[i].row, w)
		}
	}
}

// fakeStream is an in-memory qwal segmentSource for the IngestStream test.
type fakeStream struct {
	entries []*qwal.Entry
	pos     int
}

func (f *fakeStream) Next() (*qwal.Entry, error) {
	if f.pos >= len(f.entries) {
		return nil, io.EOF
	}
	e := f.entries[f.pos]
	f.pos++
	return e, nil
}
