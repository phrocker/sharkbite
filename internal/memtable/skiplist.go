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

// skiplist.go is the ordered structure backing the WAL merger. The design
// doc ("Mutation merger") calls for an in-memory red-black tree or skiplist
// keyed by RowKey; a skiplist is used here because it gives O(log n) insert
// plus a trivial in-order forward walk, which is exactly the SKVI access
// pattern (seek once, then Next repeatedly).
//
// Ordering is wire.Key.Compare verbatim: row/cf/cq/cv ascending, timestamp
// DESCENDING (newest first), and a tombstone sorts before the matching live
// cell. The skiplist does not collapse versions or apply deletes — that is
// the job of the VersioningIterator stacked on top by the parallel iterrt
// track. This is strictly the sorted leaf.
package memtable

import (
	"math/rand"

	"github.com/phrocker/shoal-oss/internal/rfile/wire"
)

const (
	maxLevel  = 24   // supports ~16M entries before levels saturate
	levelProb = 0.25 // fraction of nodes promoted to the next level
)

// node is one cell in the skiplist. next[i] is the successor at level i.
type node struct {
	key   wire.Key
	value []byte
	next  []*node
}

// skiplist is a forward-only ordered multimap of wire.Key -> value. Duplicate
// keys are permitted (two WAL entries can write the identical coordinate);
// they are kept as distinct adjacent nodes in insertion order so the SKVI
// surfaces every version the WAL recorded.
type skiplist struct {
	head   *node
	level  int // highest level currently in use (0-based)
	length int
	rng    *rand.Rand
}

func newSkiplist(seed int64) *skiplist {
	return &skiplist{
		head:  &node{next: make([]*node, maxLevel)},
		level: 0,
		rng:   rand.New(rand.NewSource(seed)),
	}
}

// randomLevel picks a node height with geometric distribution (p=levelProb).
func (s *skiplist) randomLevel() int {
	lvl := 0
	for lvl < maxLevel-1 && s.rng.Float64() < levelProb {
		lvl++
	}
	return lvl
}

// insert adds key/value. Exactly-equal keys (same row/cf/cq/cv/ts/deleted)
// are genuine duplicate versions; they are kept as distinct adjacent nodes —
// the new node is linked just before any existing equal keys. The
// VersioningIterator stacked above collapses duplicates, so the relative
// order among bit-identical coordinates is not load-bearing here, only that
// none are dropped.
func (s *skiplist) insert(key wire.Key, value []byte) {
	update := make([]*node, maxLevel)
	x := s.head
	for i := s.level; i >= 0; i-- {
		// Advance while the next node sorts strictly before key. On a tie we
		// stop, so the new node links before existing equal keys.
		for x.next[i] != nil && x.next[i].key.Compare(&key) < 0 {
			x = x.next[i]
		}
		update[i] = x
	}

	lvl := s.randomLevel()
	if lvl > s.level {
		for i := s.level + 1; i <= lvl; i++ {
			update[i] = s.head
		}
		s.level = lvl
	}
	n := &node{key: key, value: value, next: make([]*node, lvl+1)}
	for i := 0; i <= lvl; i++ {
		n.next[i] = update[i].next[i]
		update[i].next[i] = n
	}
	s.length++
}

// seekFirst returns the first node whose key is >= target under
// wire.Key.Compare, or nil if none. Used to position the SKVI at a Range
// start.
func (s *skiplist) seekFirst(target *wire.Key) *node {
	x := s.head
	for i := s.level; i >= 0; i-- {
		for x.next[i] != nil && x.next[i].key.Compare(target) < 0 {
			x = x.next[i]
		}
	}
	return x.next[0]
}

// first returns the lowest-ordered node, or nil if empty.
func (s *skiplist) first() *node { return s.head.next[0] }

// len reports the number of cells held.
func (s *skiplist) len() int { return s.length }
