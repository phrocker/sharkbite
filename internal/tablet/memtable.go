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

package tablet

import (
	"math/rand"

	"github.com/phrocker/shoal-oss/internal/iterrt"
	"github.com/phrocker/shoal-oss/internal/rfile/wire"
)

// skiplistMemtable is a direct-insert wrapper around a skiplist for the
// embedded engine. Unlike internal/memtable (which ingests from qwal
// streams), this accepts wire.Key + value pairs directly from
// cclient.Mutation.Cells().
//
// The skiplist implementation mirrors memtable/skiplist.go — same
// ordering (wire.Key.Compare), same level probability, same structure.
// We duplicate rather than export because memtable's skiplist is
// intentionally internal, and the embedded engine needs a simpler
// contract (no WAL sequence filtering, no tablet ID filtering).
type skiplistMemtable struct {
	head  *slNode
	level int
	count int
	rng   *rand.Rand
}

const (
	slMaxLevel  = 24
	slLevelProb = 0.25
)

type slNode struct {
	key   wire.Key
	value []byte
	next  []*slNode
}

func newSkiplistMemtable() *skiplistMemtable {
	return &skiplistMemtable{
		head: &slNode{next: make([]*slNode, slMaxLevel)},
		rng:  rand.New(rand.NewSource(0)),
	}
}

func (sl *skiplistMemtable) randomLevel() int {
	lvl := 1
	for lvl < slMaxLevel && sl.rng.Float64() < slLevelProb {
		lvl++
	}
	return lvl
}

// Insert adds one cell to the skiplist. Duplicate keys are permitted
// (two writes to the same coordinate produce two entries; the
// VersioningIterator collapses them at scan time).
func (sl *skiplistMemtable) Insert(key wire.Key, value []byte) {
	var update [slMaxLevel]*slNode
	cur := sl.head
	for i := sl.level - 1; i >= 0; i-- {
		for cur.next[i] != nil && cur.next[i].key.Compare(&key) < 0 {
			cur = cur.next[i]
		}
		update[i] = cur
	}

	lvl := sl.randomLevel()
	if lvl > sl.level {
		for i := sl.level; i < lvl; i++ {
			update[i] = sl.head
		}
		sl.level = lvl
	}

	node := &slNode{
		key:   key,
		value: value,
		next:  make([]*slNode, lvl),
	}
	for i := 0; i < lvl; i++ {
		node.next[i] = update[i].next[i]
		update[i].next[i] = node
	}
	sl.count++
}

// Len returns the number of cells inserted.
func (sl *skiplistMemtable) Len() int { return sl.count }

// Iterator returns an SKVI over the sorted contents. Multiple
// iterators are safe (the skiplist is only appended to, never
// modified in-place).
func (sl *skiplistMemtable) Iterator() iterrt.SortedKeyValueIterator {
	return &slIter{head: sl.head}
}

// slIter is the SKVI leaf over the embedded skiplist.
type slIter struct {
	head *slNode
	cur  *slNode
	rng  iterrt.Range
}

func (it *slIter) Init(_ iterrt.SortedKeyValueIterator, _ map[string]string, _ iterrt.IteratorEnvironment) error {
	return nil
}

func (it *slIter) Seek(r iterrt.Range, columnFamilies [][]byte, inclusive bool) error {
	it.rng = r

	if r.InfiniteStart || r.Start == nil {
		it.cur = it.head.next[0]
	} else {
		// Walk level 0 to find the first key >= Start
		it.cur = it.head.next[0]
		for it.cur != nil && it.cur.key.Compare(r.Start) < 0 {
			it.cur = it.cur.next[0]
		}
		if !r.StartInclusive {
			for it.cur != nil && it.cur.key.Compare(r.Start) == 0 {
				it.cur = it.cur.next[0]
			}
		}
	}
	// Check if we've already passed the end
	it.checkEnd()
	return nil
}

func (it *slIter) Next() error {
	if it.cur == nil {
		return nil
	}
	it.cur = it.cur.next[0]
	it.checkEnd()
	return nil
}

func (it *slIter) checkEnd() {
	if it.cur == nil {
		return
	}
	if it.rng.InfiniteEnd || it.rng.End == nil {
		return
	}
	c := it.cur.key.Compare(it.rng.End)
	if c > 0 || (c == 0 && !it.rng.EndInclusive) {
		it.cur = nil
	}
}

func (it *slIter) HasTop() bool           { return it.cur != nil }
func (it *slIter) GetTopKey() *iterrt.Key { return &it.cur.key }
func (it *slIter) GetTopValue() []byte    { return it.cur.value }

func (it *slIter) DeepCopy(_ iterrt.IteratorEnvironment) iterrt.SortedKeyValueIterator {
	return &slIter{head: it.head}
}
