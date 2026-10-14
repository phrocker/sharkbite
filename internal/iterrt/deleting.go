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
	"bytes"
	"errors"
	"fmt"
	"math"
	"strconv"
)

// DeletingIterator implements Accumulo's tombstone-suppression contract.
// Direct port of org.apache.accumulo.core.iteratorsImpl.system.DeletingIterator.
//
// Semantics:
//
//   - A tombstone (Key.Deleted=true) at coord (row,cf,cq,cv) suppresses
//     every cell at the same coord that the source surfaces AFTER it.
//     Because wire.Key.Compare sorts a coord's versions newest-first AND
//     ranks deleted=true BEFORE deleted=false at the same timestamp, a
//     tombstone is always seen first within its coord — the swallow is a
//     forward sweep, not a look-back.
//   - The tombstone itself is emitted iff propagateDeletes is true. A full
//     major compaction (the tablet's final output, where no older RFile
//     remains) sets propagateDeletes=false: nothing older than the
//     tombstone could exist, so dropping the tombstone is safe. Every
//     other scope keeps it so the next level (compaction or scan) can do
//     its own suppression against RFiles this stack didn't see.
//   - Behavior=FAIL is the safety mode for tables that should never carry
//     deletes (e.g. accumulo.metadata): seeing one is an error.
//
// Scope mapping at Init (matching IteratorUtil's setup):
//
//   - ScopeMajc && env.FullMajorCompaction => propagateDeletes=false
//   - everything else                       => propagateDeletes=true
//
// Both flags can be overridden by per-iterator options "propagateDeletes"
// and "behavior" — the option always wins.
type DeletingIterator struct {
	source           SortedKeyValueIterator
	propagateDeletes bool
	behavior         DeletingBehavior
	err              error
}

// DeletingBehavior mirrors Java's DeletingIterator.Behavior. PROCESS is the
// normal "apply the suppression rules" mode; FAIL turns the iterator into a
// guard that surfaces an error on any tombstone it sees (used on tables
// where deletes are not expected).
type DeletingBehavior int

const (
	// DeletingProcess is the default — suppress per the propagateDeletes
	// flag, like Java's PROCESS.
	DeletingProcess DeletingBehavior = iota
	// DeletingFail makes GetTopKey return an error sentinel via the err
	// field if it would surface a tombstone. Used by callers that consider
	// any delete on the input a corruption (TABLE_DELETE_BEHAVIOR=FAIL in
	// Java).
	DeletingFail
)

// Per-iterator option keys.
const (
	// DeletingOptionPropagate overrides the env-derived propagateDeletes
	// flag. Values: "true" or "false".
	DeletingOptionPropagate = "propagateDeletes"
	// DeletingOptionBehavior overrides the default behavior. Values:
	// "process" or "fail".
	DeletingOptionBehavior = "behavior"
)

// ErrUnexpectedDelete is the sentinel returned by a behavior=fail
// DeletingIterator when its source surfaces a tombstone.
var ErrUnexpectedDelete = errors.New("iterrt: DeletingIterator saw unexpected delete (behavior=fail)")

// NewDeletingIterator constructs an un-Init'd DeletingIterator.
func NewDeletingIterator() *DeletingIterator {
	return &DeletingIterator{behavior: DeletingProcess}
}

// Init wires source and derives propagateDeletes from env (full-major =>
// drop deletes; everything else => keep them) unless an explicit option
// overrides.
func (d *DeletingIterator) Init(source SortedKeyValueIterator, options map[string]string, env IteratorEnvironment) error {
	if source == nil {
		return errors.New("iterrt: DeletingIterator requires a non-nil source")
	}
	d.source = source
	d.propagateDeletes = !(env.Scope == ScopeMajc && env.FullMajorCompaction)
	if s, ok := options[DeletingOptionPropagate]; ok && s != "" {
		b, err := strconv.ParseBool(s)
		if err != nil {
			return fmt.Errorf("iterrt: DeletingIterator bad %s=%q", DeletingOptionPropagate, s)
		}
		d.propagateDeletes = b
	}
	d.behavior = DeletingProcess
	if s, ok := options[DeletingOptionBehavior]; ok && s != "" {
		switch s {
		case "process", "PROCESS":
			d.behavior = DeletingProcess
		case "fail", "FAIL":
			d.behavior = DeletingFail
		default:
			return fmt.Errorf("iterrt: DeletingIterator bad %s=%q (want process|fail)", DeletingOptionBehavior, s)
		}
	}
	return nil
}

// Seek maximizes the start key's timestamp so the source always lands at
// the START of the seek-target's coordinate — that's the position any
// tombstone for the coord would occupy under wire.Key ordering. Then we
// advance past anything the user didn't actually want (the maximize may
// have landed us at a newer version than the caller requested).
//
// Mirrors Java DeletingIterator.seek + IteratorUtil.maximizeStartKeyTimeStamp.
func (d *DeletingIterator) Seek(r Range, columnFamilies [][]byte, inclusive bool) error {
	d.err = nil
	seekRange := r
	if !r.InfiniteStart && r.Start != nil {
		maxed := r.Start.Clone()
		maxed.Timestamp = math.MaxInt64
		maxed.Deleted = false
		seekRange = Range{
			Start:          maxed,
			StartInclusive: true,
			End:            r.End,
			EndInclusive:   r.EndInclusive,
			InfiniteEnd:    r.InfiniteEnd,
		}
	}
	if err := d.source.Seek(seekRange, columnFamilies, inclusive); err != nil {
		d.err = err
		return err
	}
	if err := d.findTop(); err != nil {
		return err
	}
	// Advance past the timestamps the maximize-rewrite over-included.
	if !r.InfiniteStart && r.Start != nil {
		for d.source.HasTop() && compareRowCFCQCVTime(d.source.GetTopKey(), r.Start) < 0 {
			if err := d.advance(); err != nil {
				return err
			}
		}
		for d.HasTop() && r.BeforeStart(d.GetTopKey()) {
			if err := d.advance(); err != nil {
				return err
			}
		}
	}
	return nil
}

// Next advances past the current top. If the current top is a tombstone we
// jump to the next coordinate (the tombstone "swallows" what follows at
// this coord) regardless of propagateDeletes — propagateDeletes only
// controls whether the tombstone itself was visible to the caller, not the
// swallow behaviour. Mirrors Java's next().
func (d *DeletingIterator) Next() error {
	return d.advance()
}

// advance is the shared body of Next + post-Seek catch-up.
func (d *DeletingIterator) advance() error {
	if d.err != nil {
		return d.err
	}
	if !d.source.HasTop() {
		return errors.New("iterrt: DeletingIterator.Next called without a top")
	}
	if d.source.GetTopKey().Deleted {
		if err := d.skipCoordinate(); err != nil {
			return err
		}
	} else {
		if err := d.source.Next(); err != nil {
			d.err = err
			return err
		}
	}
	return d.findTop()
}

// findTop skips tombstones-and-their-coords when propagateDeletes is off.
// When on, deletes pass through to the caller untouched.
func (d *DeletingIterator) findTop() error {
	if d.propagateDeletes {
		return nil
	}
	for d.source.HasTop() && d.source.GetTopKey().Deleted {
		if err := d.skipCoordinate(); err != nil {
			return err
		}
	}
	return nil
}

// skipCoordinate consumes the current top (assumed to be a delete) and
// every following cell at the same (row,cf,cq,cv).
func (d *DeletingIterator) skipCoordinate() error {
	skip := d.source.GetTopKey().Clone()
	if err := d.source.Next(); err != nil {
		d.err = err
		return err
	}
	for d.source.HasTop() && d.source.GetTopKey().CompareRowCFCQCV(skip) == 0 {
		if err := d.source.Next(); err != nil {
			d.err = err
			return err
		}
	}
	return nil
}

// HasTop reports whether a cell is available. In behavior=fail mode, a
// pending unexpected-delete error suppresses the top (and is reported by
// the caller's next interaction with the iterator).
func (d *DeletingIterator) HasTop() bool {
	if d.err != nil {
		return false
	}
	if !d.source.HasTop() {
		return false
	}
	if d.behavior == DeletingFail && d.source.GetTopKey().Deleted {
		d.err = ErrUnexpectedDelete
		return false
	}
	return true
}

// GetTopKey returns the current top key. Valid only when HasTop is true.
func (d *DeletingIterator) GetTopKey() *Key {
	if !d.source.HasTop() {
		return nil
	}
	return d.source.GetTopKey()
}

// GetTopValue returns the current top value. Valid only when HasTop is
// true.
func (d *DeletingIterator) GetTopValue() []byte {
	if !d.source.HasTop() {
		return nil
	}
	return d.source.GetTopValue()
}

// DeepCopy returns an independent DeletingIterator over a DeepCopy'd
// source, carrying propagateDeletes + behavior forward. Mirrors Java's
// deepCopy.
func (d *DeletingIterator) DeepCopy(env IteratorEnvironment) SortedKeyValueIterator {
	return &DeletingIterator{
		source:           d.source.DeepCopy(env),
		propagateDeletes: d.propagateDeletes,
		behavior:         d.behavior,
	}
}

// Err exposes the latched error (e.g. ErrUnexpectedDelete for behavior=fail).
func (d *DeletingIterator) Err() error { return d.err }

// compareRowCFCQCVTime is the ROW_COLFAM_COLQUAL_COLVIS_TIME comparator —
// like wire.Key.Compare but without the deleted-flag tiebreak. Mirrors
// PartialKey.ROW_COLFAM_COLQUAL_COLVIS_TIME used by Java DeletingIterator's
// boundary catch-up loop.
func compareRowCFCQCVTime(a, b *Key) int {
	if c := bytes.Compare(a.Row, b.Row); c != 0 {
		return c
	}
	if c := bytes.Compare(a.ColumnFamily, b.ColumnFamily); c != 0 {
		return c
	}
	if c := bytes.Compare(a.ColumnQualifier, b.ColumnQualifier); c != 0 {
		return c
	}
	if c := bytes.Compare(a.ColumnVisibility, b.ColumnVisibility); c != 0 {
		return c
	}
	// Timestamp DESCENDING (matches wire.Key.Compare).
	if a.Timestamp != b.Timestamp {
		if b.Timestamp < a.Timestamp {
			return -1
		}
		return 1
	}
	return 0
}
