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
	"strconv"
	"testing"

	"github.com/phrocker/shoal-oss/internal/documentschema"
)

// docCells builds the physical cells (event field entries + field index
// entries) for one document in a shard.
func docCells(shard, datatype, uid string, fields map[string]string) []Cell {
	var cells []Cell
	eventCF := string(documentschema.EventCF(datatype, uid))
	for field, value := range fields {
		// Event field entry.
		cells = append(cells, tcell(shard, eventCF, string(documentschema.EventCQ(field, value)), "", 0))
		// Field index entry.
		fiCF := string(documentschema.FieldIndexCF(field))
		fiCQ := string(documentschema.FieldIndexCQ(value, datatype, uid))
		cells = append(cells, tcell(shard, fiCF, fiCQ, "", 0))
	}
	return cells
}

// runDocumentIndex wires a SliceSource → DocumentIndexIterator and drains it.
func runDocumentIndex(t *testing.T, cells []Cell, opts map[string]string, r Range) []Cell {
	t.Helper()
	leaf := NewSliceSource(sortedSlice(cells))
	if err := leaf.Init(nil, nil, IteratorEnvironment{Scope: ScopeScan}); err != nil {
		t.Fatalf("leaf init: %v", err)
	}
	it := NewDocumentIndexIterator()
	if err := it.Init(leaf, opts, IteratorEnvironment{Scope: ScopeScan}); err != nil {
		t.Fatalf("documentindex init: %v", err)
	}
	if err := it.Seek(r, nil, false); err != nil {
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

// docsOf extracts the distinct (datatype,uid) documents present in a cell
// stream, parsed from the event column family, in arrival order.
func docsOf(cells []Cell) []string {
	var out []string
	seen := map[string]bool{}
	for _, c := range cells {
		dt, uid, ok := documentschema.ParseEventCF(c.Key.ColumnFamily)
		if !ok {
			continue
		}
		key := dt + "/" + uid
		if !seen[key] {
			seen[key] = true
			out = append(out, key)
		}
	}
	return out
}

// termOpts builds option maps for a single shard and a list of terms.
func docOpts(shard string, boolOp string, terms ...[2]string) map[string]string {
	opts := map[string]string{
		DocumentIndexShardCount:        "1",
		DocumentIndexShardPrefix + "0": shard,
		DocumentIndexTermCount:         strconv.Itoa(len(terms)),
	}
	if boolOp != "" {
		opts[DocumentIndexBoolOp] = boolOp
	}
	for i, tm := range terms {
		opts[DocumentIndexTermPrefix+strconv.Itoa(i)+DocumentIndexTermFieldSuffix] = tm[0]
		opts[DocumentIndexTermPrefix+strconv.Itoa(i)+DocumentIndexTermValueSuffix] = tm[1]
	}
	return opts
}

func TestDocumentIndex_SingleTerm(t *testing.T) {
	shard := "20240101_0"
	var cells []Cell
	cells = append(cells, docCells(shard, "email", "u1", map[string]string{"FROM": "alice", "SUBJECT": "hello"})...)
	cells = append(cells, docCells(shard, "email", "u2", map[string]string{"FROM": "bob", "SUBJECT": "hello"})...)

	got := runDocumentIndex(t, cells, docOpts(shard, "", [2]string{"FROM", "alice"}), InfiniteRange())
	docs := docsOf(got)
	if len(docs) != 1 || docs[0] != "email/u1" {
		t.Fatalf("want [email/u1], got %v", docs)
	}
	// Reconstruction must include all of u1's fields (FROM + SUBJECT).
	if len(got) != 2 {
		t.Fatalf("want 2 reconstructed event cells for u1, got %d: %+v", len(got), got)
	}
}

func TestDocumentIndex_And(t *testing.T) {
	shard := "20240101_0"
	var cells []Cell
	cells = append(cells, docCells(shard, "email", "u1", map[string]string{"FROM": "alice", "SUBJECT": "hello"})...)
	cells = append(cells, docCells(shard, "email", "u2", map[string]string{"FROM": "alice", "SUBJECT": "bye"})...)
	cells = append(cells, docCells(shard, "email", "u3", map[string]string{"FROM": "bob", "SUBJECT": "hello"})...)

	got := runDocumentIndex(t, cells, docOpts(shard, "and",
		[2]string{"FROM", "alice"}, [2]string{"SUBJECT", "hello"}), InfiniteRange())
	docs := docsOf(got)
	if len(docs) != 1 || docs[0] != "email/u1" {
		t.Fatalf("AND want [email/u1], got %v", docs)
	}
}

func TestDocumentIndex_Or(t *testing.T) {
	shard := "20240101_0"
	var cells []Cell
	cells = append(cells, docCells(shard, "email", "u1", map[string]string{"FROM": "alice"})...)
	cells = append(cells, docCells(shard, "email", "u2", map[string]string{"FROM": "bob"})...)
	cells = append(cells, docCells(shard, "email", "u3", map[string]string{"FROM": "carol"})...)

	got := runDocumentIndex(t, cells, docOpts(shard, "or",
		[2]string{"FROM", "alice"}, [2]string{"FROM", "carol"}), InfiniteRange())
	docs := docsOf(got)
	if len(docs) != 2 {
		t.Fatalf("OR want 2 docs, got %v", docs)
	}
	if docs[0] != "email/u1" || docs[1] != "email/u3" {
		t.Fatalf("OR want sorted [email/u1, email/u3], got %v", docs)
	}
}

func TestDocumentIndex_NoMatch(t *testing.T) {
	shard := "20240101_0"
	cells := docCells(shard, "email", "u1", map[string]string{"FROM": "alice"})
	got := runDocumentIndex(t, cells, docOpts(shard, "", [2]string{"FROM", "zzz"}), InfiniteRange())
	if len(got) != 0 {
		t.Fatalf("want no cells, got %v", got)
	}
}

func TestDocumentIndex_ValueWithNul(t *testing.T) {
	// Two documents whose FROM values share a NUL-boundary prefix: "ab" and
	// "ab\x00c". A prefix scan of "ab\x00" over-selects; exact re-parse must
	// keep only the true "ab" match.
	shard := "20240101_0"
	var cells []Cell
	cells = append(cells, docCells(shard, "t", "u1", map[string]string{"FROM": "ab"})...)
	cells = append(cells, docCells(shard, "t", "u2", map[string]string{"FROM": "ab\x00c"})...)

	got := runDocumentIndex(t, cells, docOpts(shard, "", [2]string{"FROM", "ab"}), InfiniteRange())
	docs := docsOf(got)
	if len(docs) != 1 || docs[0] != "t/u1" {
		t.Fatalf("want only [t/u1] (exact 'ab'), got %v", docs)
	}

	// The NUL-containing value itself must be matchable exactly.
	got2 := runDocumentIndex(t, cells, docOpts(shard, "", [2]string{"FROM", "ab\x00c"}), InfiniteRange())
	docs2 := docsOf(got2)
	if len(docs2) != 1 || docs2[0] != "t/u2" {
		t.Fatalf("want only [t/u2] (exact 'ab\\x00c'), got %v", docs2)
	}
}

func TestDocumentIndex_MultiShard_SortedOutput(t *testing.T) {
	var cells []Cell
	cells = append(cells, docCells("20240102_0", "t", "u9", map[string]string{"K": "v"})...)
	cells = append(cells, docCells("20240101_0", "t", "u1", map[string]string{"K": "v"})...)

	opts := map[string]string{
		DocumentIndexShardCount:                                      "2",
		DocumentIndexShardPrefix + "0":                               "20240102_0",
		DocumentIndexShardPrefix + "1":                               "20240101_0",
		DocumentIndexTermCount:                                       "1",
		DocumentIndexTermPrefix + "0" + DocumentIndexTermFieldSuffix: "K",
		DocumentIndexTermPrefix + "0" + DocumentIndexTermValueSuffix: "v",
	}
	got := runDocumentIndex(t, cells, opts, InfiniteRange())
	// Output must be globally row-sorted regardless of option order.
	for i := 1; i < len(got); i++ {
		if got[i-1].Key.Compare(got[i].Key) > 0 {
			t.Fatalf("output not wire.Key-sorted at %d: %q then %q",
				i, got[i-1].Key.Row, got[i].Key.Row)
		}
	}
	docs := docsOf(got)
	if len(docs) != 2 {
		t.Fatalf("want 2 docs across shards, got %v", docs)
	}
}

func TestDocumentIndex_AndShortCircuitEmpty(t *testing.T) {
	shard := "20240101_0"
	cells := docCells(shard, "t", "u1", map[string]string{"A": "1", "B": "2"})
	// A=1 matches u1 but C=9 matches nothing → AND empty.
	got := runDocumentIndex(t, cells, docOpts(shard, "and",
		[2]string{"A", "1"}, [2]string{"C", "9"}), InfiniteRange())
	if len(got) != 0 {
		t.Fatalf("AND with an unsatisfiable term must be empty, got %v", got)
	}
}

func TestDocumentIndex_BadBoolOp(t *testing.T) {
	leaf := NewSliceSource(nil)
	_ = leaf.Init(nil, nil, IteratorEnvironment{Scope: ScopeScan})
	it := NewDocumentIndexIterator()
	err := it.Init(leaf, map[string]string{
		DocumentIndexShardCount: "0",
		DocumentIndexTermCount:  "0",
		DocumentIndexBoolOp:     "xor",
	}, IteratorEnvironment{Scope: ScopeScan})
	if err == nil {
		t.Fatal("want error for bad boolOp")
	}
}
