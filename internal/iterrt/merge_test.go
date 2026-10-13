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
	"fmt"
	"math"
	"math/rand"
	"sort"
	"testing"
)

func seekAll(t *testing.T, it SortedKeyValueIterator) {
	t.Helper()
	if err := it.Init(nil, nil, IteratorEnvironment{}); err != nil {
		t.Fatalf("Init: %v", err)
	}
	if err := it.Seek(InfiniteRange(), nil, false); err != nil {
		t.Fatalf("Seek: %v", err)
	}
}

// TestMerge_TwoSortedStreams: merging two key-disjoint sorted streams
// yields one fully-sorted stream.
func TestMerge_TwoSortedStreams(t *testing.T) {
	a := newSliceSource(
		kv{mk("r1", "cf", "a", "", 10), []byte("a1")},
		kv{mk("r3", "cf", "a", "", 10), []byte("a3")},
	)
	b := newSliceSource(
		kv{mk("r2", "cf", "a", "", 10), []byte("b2")},
		kv{mk("r4", "cf", "a", "", 10), []byte("b4")},
	)
	m := NewMergingIterator(a, b)
	seekAll(t, m)
	got, err := drain(m)
	if err != nil {
		t.Fatalf("drain: %v", err)
	}
	if gs := valStrings(got); !eqStr(gs, []string{"a1", "b2", "a3", "b4"}) {
		t.Fatalf("got %v", gs)
	}
}

// TestMerge_OverlappingCoordinatesTimestampOrder: when two sources carry
// the same coordinate at different timestamps, the merge interleaves
// them newest-first (wire.Key.Compare puts the newer timestamp first).
func TestMerge_OverlappingCoordinatesTimestampOrder(t *testing.T) {
	a := newSliceSource(
		kv{mk("r1", "cf", "a", "", 30), []byte("a-v30")},
		kv{mk("r1", "cf", "a", "", 10), []byte("a-v10")},
	)
	b := newSliceSource(
		kv{mk("r1", "cf", "a", "", 20), []byte("b-v20")},
	)
	m := NewMergingIterator(a, b)
	seekAll(t, m)
	got, err := drain(m)
	if err != nil {
		t.Fatalf("drain: %v", err)
	}
	if gs := valStrings(got); !eqStr(gs, []string{"a-v30", "b-v20", "a-v10"}) {
		t.Fatalf("got %v, want newest-first interleave", gs)
	}
}

// TestMerge_FeedsVersioning: a MergingIterator under a VersioningIterator
// is the compaction-stack core — N files merged then version-capped.
func TestMerge_FeedsVersioning(t *testing.T) {
	a := newSliceSource(
		kv{mk("r1", "cf", "a", "", 30), []byte("v30")},
		kv{mk("r1", "cf", "a", "", 10), []byte("v10")},
	)
	b := newSliceSource(
		kv{mk("r1", "cf", "a", "", 20), []byte("v20")},
		kv{mk("r2", "cf", "a", "", 5), []byte("r2")},
	)
	m := NewMergingIterator(a, b)
	v := NewVersioningIterator()
	if err := v.Init(m, map[string]string{VersioningOption: "1"}, IteratorEnvironment{Scope: ScopeMajc}); err != nil {
		t.Fatalf("vers Init: %v", err)
	}
	// MergingIterator is the source; Init it explicitly (Versioning.Init
	// does not fan Init into its source).
	if err := m.Init(nil, nil, IteratorEnvironment{Scope: ScopeMajc}); err != nil {
		t.Fatalf("merge Init: %v", err)
	}
	if err := v.Seek(InfiniteRange(), nil, false); err != nil {
		t.Fatalf("Seek: %v", err)
	}
	got, err := drain(v)
	if err != nil {
		t.Fatalf("drain: %v", err)
	}
	if gs := valStrings(got); !eqStr(gs, []string{"v30", "r2"}) {
		t.Fatalf("got %v, want newest-per-coord [v30 r2]", gs)
	}
}

// TestMerge_Empty: a zero-source merge is valid and immediately exhausted.
func TestMerge_Empty(t *testing.T) {
	m := NewMergingIterator()
	seekAll(t, m)
	if m.HasTop() {
		t.Fatal("empty merge should have no top")
	}
}

// TestMerge_DeepCopyIndependent: a DeepCopy'd merge seeks without
// disturbing the original's position.
func TestMerge_DeepCopyIndependent(t *testing.T) {
	a := newSliceSource(kv{mk("r1", "cf", "a", "", 10), []byte("a1")})
	b := newSliceSource(kv{mk("r2", "cf", "a", "", 10), []byte("b2")})
	m := NewMergingIterator(a, b)
	seekAll(t, m)
	if err := m.Next(); err != nil { // advance original past r1
		t.Fatalf("Next: %v", err)
	}
	cp := m.DeepCopy(IteratorEnvironment{})
	if err := cp.Seek(InfiniteRange(), nil, false); err != nil {
		t.Fatalf("copy Seek: %v", err)
	}
	got, err := drain(cp)
	if err != nil {
		t.Fatalf("copy drain: %v", err)
	}
	if gs := valStrings(got); !eqStr(gs, []string{"a1", "b2"}) {
		t.Fatalf("copy got %v, want full stream", gs)
	}
	// Original still positioned at r2.
	if !m.HasTop() || string(m.GetTopValue()) != "b2" {
		t.Fatalf("original disturbed by DeepCopy")
	}
}

// TestMerge_ManySourcesFuzzParity: merge of K random sorted streams
// equals a global sort of all cells.
func TestMerge_ManySourcesFuzzParity(t *testing.T) {
	const k = 6
	var all []kv
	srcs := make([]SortedKeyValueIterator, k)
	for s := 0; s < k; s++ {
		var cells []kv
		for i := s; i < 200; i += k {
			c := kv{mk(rowN(i), "cf", "a", "", int64(1000-i)), []byte(rowN(i))}
			cells = append(cells, c)
			all = append(all, c)
		}
		srcs[s] = newSliceSource(cells...)
	}
	m := NewMergingIterator(srcs...)
	seekAll(t, m)
	got, err := drain(m)
	if err != nil {
		t.Fatalf("drain: %v", err)
	}
	sort.SliceStable(all, func(i, j int) bool { return all[i].k.Compare(all[j].k) < 0 })
	if len(got) != len(all) {
		t.Fatalf("count %d != %d", len(got), len(all))
	}
	for i := range all {
		if got[i].k.Compare(all[i].k) != 0 {
			t.Fatalf("cell %d: merge %+v != sorted %+v", i, got[i].k, all[i].k)
		}
	}
}

func rowN(i int) string {
	const digits = "0123456789"
	b := []byte("r000")
	b[1] = digits[(i/100)%10]
	b[2] = digits[(i/10)%10]
	b[3] = digits[i%10]
	return string(b)
}

// TestMerge_IdenticalKeysEarlierSourceFirst: keys identical down to the
// timestamp and delete flag (what PutLatest produces for every overwrite)
// surface in source order — the earlier (newer) source first — regardless
// of how many sources there are or where the heap places them (#692).
func TestMerge_IdenticalKeysEarlierSourceFirst(t *testing.T) {
	for n := 2; n <= 9; n++ {
		srcs := make([]SortedKeyValueIterator, n)
		want := make([]string, n)
		for i := 0; i < n; i++ {
			v := fmt.Sprintf("src%d", i)
			srcs[i] = newSliceSource(
				kv{mk("a", "cf", "q", "", 5), []byte("a" + v)},
				kv{mk("r", "cf", "q", "", math.MaxInt64), []byte(v)},
				kv{mk("z", "cf", "q", "", 5), []byte("z" + v)},
			)
			want[i] = v
		}
		m := NewMergingIterator(srcs...)
		seekAll(t, m)
		// Seek straight onto the identical run as well as from the start.
		ranges := []Range{
			InfiniteRange(),
			{Start: mk("r", "cf", "q", "", math.MaxInt64), StartInclusive: true, InfiniteEnd: true},
		}
		for _, r := range ranges {
			if err := m.Seek(r, nil, false); err != nil {
				t.Fatal(err)
			}
			got, err := drain(m)
			if err != nil {
				t.Fatal(err)
			}
			var run []string
			for _, c := range got {
				if string(c.k.Row) == "r" {
					run = append(run, string(c.v))
				}
			}
			if !eqStr(run, want) {
				t.Fatalf("n=%d: identical keys surfaced %v, want source order %v", n, run, want)
			}
		}
	}
}

// TestMerge_IdenticalKeysWithinSourceKeepOrder: a source that itself holds
// identical keys (a memtable with several PutLatest overwrites, or a file
// flushed from one) keeps its own order, and the whole run sits between
// the runs of the sources before and after it.
func TestMerge_IdenticalKeysWithinSourceKeepOrder(t *testing.T) {
	key := mk("r", "cf", "q", "", math.MaxInt64)
	newer := newSliceSource(kv{key, []byte("n1")}, kv{key, []byte("n2")})
	middle := newSliceSource(kv{key, []byte("m1")}, kv{key, []byte("m2")}, kv{key, []byte("m3")})
	older := newSliceSource(kv{key, []byte("o1")})
	m := NewMergingIterator(newer, middle, older)
	seekAll(t, m)
	got, err := drain(m)
	if err != nil {
		t.Fatal(err)
	}
	if gs := valStrings(got); !eqStr(gs, []string{"n1", "n2", "m1", "m2", "m3", "o1"}) {
		t.Fatalf("got %v", gs)
	}
}

// TestMerge_IdenticalKeysUnderVersioning: maxVersions=1 over identical keys
// keeps the earliest source's value.
func TestMerge_IdenticalKeysUnderVersioning(t *testing.T) {
	key := mk("r", "cf", "q", "", math.MaxInt64)
	m := NewMergingIterator(
		newSliceSource(kv{key, []byte("newest")}),
		newSliceSource(kv{key, []byte("middle")}),
		newSliceSource(kv{key, []byte("oldest")}),
	)
	if err := m.Init(nil, nil, IteratorEnvironment{}); err != nil {
		t.Fatal(err)
	}
	v := NewVersioningIterator()
	if err := v.Init(m, map[string]string{VersioningOption: "1"}, IteratorEnvironment{}); err != nil {
		t.Fatal(err)
	}
	if err := v.Seek(InfiniteRange(), nil, false); err != nil {
		t.Fatal(err)
	}
	got, err := drain(v)
	if err != nil {
		t.Fatal(err)
	}
	if gs := valStrings(got); !eqStr(gs, []string{"newest"}) {
		t.Fatalf("got %v, want [newest]", gs)
	}
}

// TestMerge_FuzzStableByPriority: the merge of K random sorted streams
// with many identical keys equals a stable sort of the concatenation of
// the sources in priority order — key order, then source order, then
// in-source order. Also checked through a DeepCopy.
func TestMerge_FuzzStableByPriority(t *testing.T) {
	rng := rand.New(rand.NewSource(692))
	for iter := 0; iter < 200; iter++ {
		k := 1 + rng.Intn(8)
		var all []kv
		srcs := make([]SortedKeyValueIterator, k)
		for s := 0; s < k; s++ {
			var cells []kv
			n := rng.Intn(40)
			for i := 0; i < n; i++ {
				ts := int64(math.MaxInt64)
				if rng.Intn(3) == 0 {
					ts = int64(rng.Intn(3))
				}
				key := mk(rowN(rng.Intn(6)), "cf", "q", "", ts)
				if rng.Intn(5) == 0 {
					key.Deleted = true
				}
				cells = append(cells, kv{key, []byte(fmt.Sprintf("s%d-%d", s, i))})
			}
			sort.SliceStable(cells, func(i, j int) bool { return cells[i].k.Compare(cells[j].k) < 0 })
			all = append(all, cells...)
			srcs[s] = newSliceSource(cells...)
		}
		sort.SliceStable(all, func(i, j int) bool { return all[i].k.Compare(all[j].k) < 0 })
		m := NewMergingIterator(srcs...)
		seekAll(t, m)
		for pass, it := range []SortedKeyValueIterator{m, m.DeepCopy(IteratorEnvironment{})} {
			if pass == 1 {
				seekAll(t, it)
			}
			got, err := drain(it)
			if err != nil {
				t.Fatal(err)
			}
			if gs, ws := valStrings(got), valStrings(all); !eqStr(gs, ws) {
				t.Fatalf("iter %d pass %d:\n got %v\nwant %v", iter, pass, gs, ws)
			}
		}
	}
}
