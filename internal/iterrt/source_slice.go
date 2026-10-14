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

import "errors"

// Cell is an in-memory (Key, Value) pair. It is the unit a SliceSource
// iterates and the shape the parity harness threads through an iterator
// stack before handing cells to the RFile writer.
type Cell struct {
	Key   *Key
	Value []byte
}

// SliceSource is a leaf SortedKeyValueIterator backed by an in-memory,
// already-sorted cell slice. Two consumers:
//
//   - the RFile parity harness, which applies an iterator stack to a
//     generated cell stream before writing it (so the shoal write side
//     and the Java write side both feed the post-iterator cells);
//   - Bet 2's memtable, which is exactly "a sorted in-memory cell slice
//     exposed as a SortedKeyValueIterator" once WAL entries are merged.
//
// The input slice MUST be in wire.Key.Compare order — SliceSource does
// not sort. It is not goroutine-safe; DeepCopy hands back an independent
// cursor over the same (immutable) backing slice.
type SliceSource struct {
	cells []Cell
	pos   int

	rng       Range
	cfs       [][]byte
	inclusive bool
}

// NewSliceSource builds a leaf over cells. cells must be Key-sorted.
func NewSliceSource(cells []Cell) *SliceSource {
	return &SliceSource{cells: cells}
}

// Init records env; a leaf takes no source.
func (s *SliceSource) Init(source SortedKeyValueIterator, options map[string]string, env IteratorEnvironment) error {
	if source != nil {
		return errors.New("iterrt: SliceSource is a leaf iterator, source must be nil")
	}
	return nil
}

// Seek positions at the range start and applies the column-family filter.
func (s *SliceSource) Seek(r Range, columnFamilies [][]byte, inclusive bool) error {
	s.rng = r
	s.cfs = columnFamilies
	s.inclusive = inclusive
	s.pos = 0
	s.skip()
	return nil
}

// Next advances past the current top.
func (s *SliceSource) Next() error {
	if s.pos >= len(s.cells) {
		return errors.New("iterrt: SliceSource.Next called without a top")
	}
	s.pos++
	s.skip()
	return nil
}

// skip advances pos past cells outside the range or rejected by the cf
// filter, and clamps pos to len once the range upper bound is crossed.
func (s *SliceSource) skip() {
	for s.pos < len(s.cells) {
		k := s.cells[s.pos].Key
		if s.rng.BeforeStart(k) {
			s.pos++
			continue
		}
		if s.rng.AfterEnd(k) {
			s.pos = len(s.cells)
			return
		}
		if !cfAllowed(k.ColumnFamily, s.cfs, s.inclusive) {
			s.pos++
			continue
		}
		return
	}
}

// HasTop reports whether a cell is available.
func (s *SliceSource) HasTop() bool { return s.pos < len(s.cells) }

// GetTopKey returns the current top key.
func (s *SliceSource) GetTopKey() *Key {
	if s.pos >= len(s.cells) {
		return nil
	}
	return s.cells[s.pos].Key
}

// GetTopValue returns the current top value.
func (s *SliceSource) GetTopValue() []byte {
	if s.pos >= len(s.cells) {
		return nil
	}
	return s.cells[s.pos].Value
}

// DeepCopy returns an independent cursor over the same backing slice.
func (s *SliceSource) DeepCopy(env IteratorEnvironment) SortedKeyValueIterator {
	return &SliceSource{cells: s.cells}
}
