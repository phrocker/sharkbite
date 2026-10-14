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

// AsOfIterator is a time-travel filter: it drops every cell whose timestamp
// is strictly greater than a configured ceiling, reconstructing the source as
// it existed at-or-before that instant. It is the analogue of an Accumulo
// per-cell timestamp Filter and is meant to sit at the BOTTOM of a scan stack
// (closest to the source, beneath Deleting and Versioning):
//
//   - AsOf drops versions newer than the ceiling. Because a coordinate's
//     versions arrive timestamp-DESCENDING (wire.Key.Compare), skipping the
//     too-new ones lands on the newest surviving version <= ceiling, leaving
//     older versions intact for the iterators above.
//   - Deleting (above) then resolves tombstones among the surviving cells: a
//     tombstone written AFTER the ceiling has already been filtered out, so a
//     delete that had not yet happened at the as-of instant does not suppress
//     the live cell. A tombstone at-or-before the ceiling still applies.
//   - Versioning (top) keeps the newest surviving version per coordinate.
//
// That ordering yields exactly the table's state as of the ceiling timestamp.
//
// The filter operates per-cell and never reorders, so it is correct at any
// IteratorScope; env is recorded but does not change behaviour.
type AsOfIterator struct {
	source  SortedKeyValueIterator
	ceiling int64
	err     error
}

// AsOfOption is the per-iterator option key carrying the timestamp ceiling.
// A missing or non-positive value disables filtering (the iterator becomes a
// pass-through), so wiring it unconditionally is harmless.
const AsOfOption = "asOfTimestamp"

// NewAsOfIterator constructs an un-Init'd AsOfIterator.
func NewAsOfIterator() *AsOfIterator { return &AsOfIterator{} }

// Init records the source and parses the ceiling. A missing/empty option or a
// value <= 0 leaves ceiling at 0, which Init treats as "no ceiling" (every
// cell passes). A malformed integer is an error.
func (a *AsOfIterator) Init(source SortedKeyValueIterator, options map[string]string, _ IteratorEnvironment) error {
	if source == nil {
		return errors.New("iterrt: AsOfIterator requires a non-nil source")
	}
	a.source = source
	a.ceiling = 0
	if s, ok := options[AsOfOption]; ok && s != "" {
		n, err := strconv.ParseInt(s, 10, 64)
		if err != nil {
			return errors.New("iterrt: AsOfIterator bad integer " + AsOfOption + ": " + s)
		}
		if n > 0 {
			a.ceiling = n
		}
	}
	return nil
}

// active reports whether a ceiling is in effect.
func (a *AsOfIterator) active() bool { return a.ceiling > 0 }

// Seek seeks the source and skips any leading too-new cells.
func (a *AsOfIterator) Seek(r Range, columnFamilies [][]byte, inclusive bool) error {
	a.err = nil
	if err := a.source.Seek(r, columnFamilies, inclusive); err != nil {
		a.err = err
		return err
	}
	return a.findTop()
}

// Next advances past the current top, then skips any too-new cells.
func (a *AsOfIterator) Next() error {
	if a.err != nil {
		return a.err
	}
	if !a.source.HasTop() {
		return errors.New("iterrt: AsOfIterator.Next called without a top")
	}
	if err := a.source.Next(); err != nil {
		a.err = err
		return err
	}
	return a.findTop()
}

// findTop advances the source past every cell whose timestamp exceeds the
// ceiling. A no-op when no ceiling is configured.
func (a *AsOfIterator) findTop() error {
	if !a.active() {
		return nil
	}
	for a.source.HasTop() && a.source.GetTopKey().Timestamp > a.ceiling {
		if err := a.source.Next(); err != nil {
			a.err = err
			return err
		}
	}
	return nil
}

// HasTop reports whether a cell is available.
func (a *AsOfIterator) HasTop() bool {
	return a.err == nil && a.source.HasTop()
}

// GetTopKey returns the current top key (passthrough from the source).
func (a *AsOfIterator) GetTopKey() *Key {
	if !a.source.HasTop() {
		return nil
	}
	return a.source.GetTopKey()
}

// GetTopValue returns the current top value (passthrough from the source).
func (a *AsOfIterator) GetTopValue() []byte {
	if !a.source.HasTop() {
		return nil
	}
	return a.source.GetTopValue()
}

// DeepCopy returns an independent AsOfIterator over a DeepCopy'd source,
// carrying the ceiling forward.
func (a *AsOfIterator) DeepCopy(env IteratorEnvironment) SortedKeyValueIterator {
	return &AsOfIterator{
		source:  a.source.DeepCopy(env),
		ceiling: a.ceiling,
	}
}
