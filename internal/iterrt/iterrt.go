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

// Package iterrt is shoal's Go-native iterator runtime: the interface a
// compaction stack (Bet 1) and a WAL/RFile merge stack (Bet 2) are both
// composed from.
//
// SortedKeyValueIterator mirrors Java's
// org.apache.accumulo.core.iterators.SortedKeyValueIterator (design doc
// "Decisions (locked 2026-05-13)" item 2). The contract is intentionally a
// near-1:1 port so a Go iterator can be parity-tested against its Java
// reference with no impedance mismatch: same inputs, same outputs.
//
// Semantics carried over verbatim from the Java interface:
//
//   - An iterator is constructed, then Init'd with its source (the iterator
//     below it in the stack), its per-iterator options, and an environment.
//   - Seek positions the iterator. After a successful Seek, HasTop reports
//     whether a top key/value is available; GetTopKey/GetTopValue read it.
//   - Next advances past the current top. After Next, HasTop must be
//     re-checked — it goes false at end-of-range.
//   - GetTopKey/GetTopValue are only valid when HasTop is true, and only
//     until the next Seek/Next call. Callers that retain a key past that
//     point must Clone it (see wire.Key.Clone).
//   - DeepCopy returns an independent iterator over the same data, used to
//     run multiple concurrent seeks (e.g. source-of-a-source fan-out).
package iterrt

import (
	"context"

	"github.com/phrocker/shoal-oss/internal/rfile/wire"
)

// Key is the cell coordinate type threaded through the runtime. Aliased to
// wire.Key so RFile readers, the WAL merger, and iterators all speak one
// type. Ordering is Accumulo's PartialKey ROW_COLFAM_COLQUAL_COLVIS_TIME_DEL
// (see wire.Key.Compare).
type Key = wire.Key

// IteratorScope identifies the compaction/scan context an iterator stack is
// running in. Mirrors Java's IteratorUtil.IteratorScope. Some iterators
// behave differently per scope (e.g. VersioningIterator is a no-op at
// majc-with-no-deletes vs. a filter at scan).
type IteratorScope int

const (
	// ScopeScan: serving a client read. The full stack runs; deletes are
	// honored but not dropped (a later compaction does that).
	ScopeScan IteratorScope = iota
	// ScopeMinc: minor compaction — flushing a memtable to a new RFile.
	ScopeMinc
	// ScopeMajc: major compaction — merging RFiles. When the output is the
	// tablet's last file, deletes can be dropped entirely.
	ScopeMajc
)

func (s IteratorScope) String() string {
	switch s {
	case ScopeScan:
		return "scan"
	case ScopeMinc:
		return "minc"
	case ScopeMajc:
		return "majc"
	default:
		return "unknown"
	}
}

// IteratorEnvironment is the context handed to each iterator at Init. Mirrors
// the subset of Java's IteratorEnvironment that shoal's iterators actually
// consult. It is read-only from an iterator's point of view.
type IteratorEnvironment struct {
	// Context cancels request-scoped iterator work. Nil means Background.
	Context context.Context

	// Scope is the compaction/scan context (see IteratorScope).
	Scope IteratorScope

	// FullMajorCompaction is true only when Scope == ScopeMajc AND the output
	// RFile is the tablet's sole remaining file. VersioningIterator and any
	// delete-aware iterator use this to decide whether a tombstone may be
	// dropped (safe only when nothing older could exist elsewhere).
	FullMajorCompaction bool

	// Authorizations is the caller's auth set, used by the visibility filter
	// iterator. Nil means "system context" — every cell is visible. Only
	// meaningful at ScopeScan; compaction scopes never filter by visibility.
	Authorizations [][]byte

	// TableConfig is the table's iterator-relevant configuration (e.g.
	// table.iterator.* properties already resolved to key/value). Iterators
	// that need table-level settings beyond their own per-iterator options
	// read them here. May be nil.
	TableConfig map[string]string
}

// SortedKeyValueIterator is the composition unit of the runtime — a Go port
// of Java's interface of the same name. See the package doc for the full
// contract; method docs below cover the per-method invariants.
type SortedKeyValueIterator interface {
	// Init wires this iterator on top of source, applies its per-iterator
	// options, and records env. It must be called exactly once, before any
	// Seek. Implementations that wrap a source must retain it and call
	// through. Leaf iterators (RFile reader adapter, memtable) take a nil
	// source.
	Init(source SortedKeyValueIterator, options map[string]string, env IteratorEnvironment) error

	// Seek positions the iterator at the start of r, restricted to the given
	// column families. If inclusive is true the iterator yields only cells
	// whose column family is in columnFamilies; if false it yields only cells
	// whose column family is NOT in the set. An empty/nil columnFamilies with
	// inclusive=false means "no restriction" (the common full-scan case).
	//
	// After Seek returns nil, the caller must check HasTop before reading.
	Seek(r Range, columnFamilies [][]byte, inclusive bool) error

	// Next advances past the current top key/value. Only valid when HasTop is
	// true. After Next, HasTop must be re-checked.
	Next() error

	// HasTop reports whether GetTopKey/GetTopValue currently have a value to
	// return. Goes false at end-of-range.
	HasTop() bool

	// GetTopKey returns the current top key. Only valid when HasTop is true.
	// The returned *Key is owned by the iterator and is invalidated by the
	// next Seek/Next — callers retaining it must Clone.
	GetTopKey() *Key

	// GetTopValue returns the current top value. Only valid when HasTop is
	// true. Same transient-lifetime rule as GetTopKey.
	GetTopValue() []byte

	// DeepCopy returns an independent iterator positioned identically to a
	// freshly-Init'd copy (i.e. NOT yet Seek'd) over the same underlying data.
	// env replaces the original environment for the copy. Used when a parent
	// iterator needs to seek its source more than once concurrently.
	DeepCopy(env IteratorEnvironment) SortedKeyValueIterator
}

// Range is a Key-granular scan range — a Go port of Java's
// org.apache.accumulo.core.data.Range. Unlike cclient.Range (which is
// row-only and used at the Thrift boundary), iterator seeking needs full Key
// bounds so a stack can be re-seeked mid-row.
//
// A nil Start with InfiniteStart, or a nil End with InfiniteEnd, denotes an
// unbounded side. The zero Range (both infinite) is the full-table range.
type Range struct {
	// Start is the lower bound key. Ignored when InfiniteStart is true.
	Start *Key
	// StartInclusive reports whether Start itself is in range.
	StartInclusive bool
	// InfiniteStart, when true, means the range has no lower bound.
	InfiniteStart bool

	// End is the upper bound key. Ignored when InfiniteEnd is true.
	End *Key
	// EndInclusive reports whether End itself is in range.
	EndInclusive bool
	// InfiniteEnd, when true, means the range has no upper bound.
	InfiniteEnd bool
}

// InfiniteRange returns the full-table range (both ends unbounded).
func InfiniteRange() Range {
	return Range{InfiniteStart: true, InfiniteEnd: true}
}

// AfterKey returns a range that starts strictly after k and is unbounded
// above. Used to re-seek a source past a key an iterator has consumed.
func AfterKey(k *Key) Range {
	return Range{Start: k, StartInclusive: false, InfiniteEnd: true}
}

// Contains reports whether k falls within the range under Accumulo's
// PartialKey ROW_COLFAM_COLQUAL_COLVIS_TIME_DEL ordering (wire.Key.Compare).
func (r Range) Contains(k *Key) bool {
	if !r.InfiniteStart && r.Start != nil {
		c := k.Compare(r.Start)
		if c < 0 || (c == 0 && !r.StartInclusive) {
			return false
		}
	}
	if !r.InfiniteEnd && r.End != nil {
		c := k.Compare(r.End)
		if c > 0 || (c == 0 && !r.EndInclusive) {
			return false
		}
	}
	return true
}

// BeforeStart reports whether k sorts strictly before the range's lower
// bound — i.e. a source iterator positioned at k still needs to advance.
// Always false for an infinite-start range.
func (r Range) BeforeStart(k *Key) bool {
	if r.InfiniteStart || r.Start == nil {
		return false
	}
	c := k.Compare(r.Start)
	return c < 0 || (c == 0 && !r.StartInclusive)
}

// AfterEnd reports whether k sorts strictly after the range's upper bound —
// i.e. a source iterator at k has run past the range and iteration is done.
// Always false for an infinite-end range.
func (r Range) AfterEnd(k *Key) bool {
	if r.InfiniteEnd || r.End == nil {
		return false
	}
	c := k.Compare(r.End)
	return c > 0 || (c == 0 && !r.EndInclusive)
}
