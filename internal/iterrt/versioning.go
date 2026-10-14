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
	"errors"
	"strconv"
)

// VersioningIterator keeps the newest maxVersions cells per key
// coordinate (row+cf+cq+cv), dropping older versions. Direct port of
// org.apache.accumulo.core.iterators.user.VersioningIterator.
//
// Semantics confirmed against the Java source:
//
//   - maxVersions comes from the per-iterator option "maxVersions";
//     default 1, must be >= 1 (Java throws IllegalArgumentException
//     otherwise — we return an error from Init).
//   - "Newest" is well defined because wire.Key.Compare sorts a
//     coordinate's versions by timestamp DESCENDING — the source hands
//     them to us newest-first, so we emit the first maxVersions and skip
//     the rest of that coordinate.
//   - Keys identical down to the timestamp (every PutLatest overwrite of
//     a cell carries ts=MaxInt64) are NOT collapsed: each counts as a
//     version, exactly as in Java. Their relative order comes from the
//     source — newest write first within a memtable or file, and newest
//     source first across a MergingIterator — so maxVersions=1 keeps the
//     latest write and maxVersions=N returns the N most recent writes,
//     newest first. Collapsing them here (or at flush/compaction) is
//     deliberately not done: it would diverge from Java compaction output,
//     and pkg/explorer reads the older copy of an interaction record that
//     a later same-key write marked deleted.
//   - The Java VersioningIterator is NOT delete-aware: a tombstone is
//     just another version and counts toward maxVersions. It does no
//     scope branching — same behaviour at scan/minc/majc. We replicate
//     that exactly. (Delete *suppression* — a tombstone swallowing older
//     live cells — is a separate concern handled by the
//     DeletingIterator / a later compaction, not here. See the design
//     doc; honoring IteratorScope for delete-dropping is a DeletingIterator
//     job, intentionally not folded into this port.)
//
// The newest-N semantic is identical at every IteratorScope, so env.Scope
// is recorded but does not change behaviour — matching Java.
type VersioningIterator struct {
	source      SortedKeyValueIterator
	maxVersions int

	// coord is the (row,cf,cq,cv) of the run currently being counted.
	coord       *Key
	numVersions int
	err         error
}

// VersioningOption is the per-iterator option key for the version cap.
const VersioningOption = "maxVersions"

// NewVersioningIterator constructs an un-Init'd VersioningIterator. Wire
// it with Init(source, {"maxVersions": "N"}, env).
func NewVersioningIterator() *VersioningIterator {
	return &VersioningIterator{maxVersions: 1}
}

// Init records the source and parses maxVersions. Mirrors Java's init:
// missing option => 1; present => parsed int; < 1 => error.
func (v *VersioningIterator) Init(source SortedKeyValueIterator, options map[string]string, env IteratorEnvironment) error {
	if source == nil {
		return errors.New("iterrt: VersioningIterator requires a non-nil source")
	}
	v.source = source
	v.maxVersions = 1
	if s, ok := options[VersioningOption]; ok && s != "" {
		n, err := strconv.Atoi(s)
		if err != nil {
			return errors.New("iterrt: VersioningIterator bad integer maxVersions: " + s)
		}
		v.maxVersions = n
	}
	if v.maxVersions < 1 {
		return errors.New("iterrt: maxVersions for versioning iterator must be >= 1")
	}
	return nil
}

// Seek seeks the source and resets the version run. Java maximizes the
// start key's timestamp so a seek never lands mid-coordinate; the iterrt
// Range is already key-granular and our callers seek on coordinate
// boundaries, so we pass the range through unchanged and just reset the
// run counter — the first cell after the seek begins a fresh coordinate.
func (v *VersioningIterator) Seek(r Range, columnFamilies [][]byte, inclusive bool) error {
	v.err = nil
	if err := v.source.Seek(r, columnFamilies, inclusive); err != nil {
		v.err = err
		return err
	}
	v.resetRun()
	return nil
}

// Next advances to the next cell to surface. If the current coordinate
// still has version budget, advance one cell; if the source stays on the
// same coordinate, the version count increments, otherwise a new run
// starts. If the budget is spent, skip the rest of this coordinate's
// versions before starting the next run. Mirrors Java's next().
func (v *VersioningIterator) Next() error {
	if v.err != nil {
		return v.err
	}
	if !v.source.HasTop() {
		return errors.New("iterrt: VersioningIterator.Next called without a top")
	}
	if v.numVersions >= v.maxVersions {
		if err := v.skipCoordinate(); err != nil {
			return err
		}
		v.resetRun()
		return nil
	}
	if err := v.source.Next(); err != nil {
		v.err = err
		return err
	}
	if v.source.HasTop() {
		if v.coord != nil && v.source.GetTopKey().CompareRowCFCQCV(v.coord) == 0 {
			v.numVersions++
		} else {
			v.resetRun()
		}
	}
	return nil
}

// skipCoordinate advances the source past every remaining version of the
// current coordinate. Java reseeks past a long run after a few next()
// probes; shoal's leaf readers have no expensive-reseek property worth
// the extra complexity at C1, so we just next() through — a coordinate
// rarely has more than a handful of versions.
func (v *VersioningIterator) skipCoordinate() error {
	skip := v.coord
	if err := v.source.Next(); err != nil {
		v.err = err
		return err
	}
	for v.source.HasTop() && skip != nil && v.source.GetTopKey().CompareRowCFCQCV(skip) == 0 {
		if err := v.source.Next(); err != nil {
			v.err = err
			return err
		}
	}
	return nil
}

// resetRun records the source's current coordinate as the active run and
// sets the version count to 1 (the current cell is version one).
func (v *VersioningIterator) resetRun() {
	if v.source.HasTop() {
		v.coord = v.source.GetTopKey().Clone()
	} else {
		v.coord = nil
	}
	v.numVersions = 1
}

// HasTop reports whether a cell is available.
func (v *VersioningIterator) HasTop() bool {
	return v.err == nil && v.source.HasTop()
}

// GetTopKey returns the current top key (passthrough from the source).
func (v *VersioningIterator) GetTopKey() *Key {
	if !v.source.HasTop() {
		return nil
	}
	return v.source.GetTopKey()
}

// GetTopValue returns the current top value (passthrough from the source).
func (v *VersioningIterator) GetTopValue() []byte {
	if !v.source.HasTop() {
		return nil
	}
	return v.source.GetTopValue()
}

// DeepCopy returns an independent VersioningIterator over a DeepCopy'd
// source, carrying maxVersions forward. Mirrors Java's deepCopy.
func (v *VersioningIterator) DeepCopy(env IteratorEnvironment) SortedKeyValueIterator {
	return &VersioningIterator{
		source:      v.source.DeepCopy(env),
		maxVersions: v.maxVersions,
	}
}
