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
	"sort"
)

// VisibilityStampIterator rewrites every cell's ColumnVisibility to carry
// a tenant label, so many independent producers (e.g. local agents) can
// fan their RFiles into one engine while staying isolated: a scan only
// surfaces a tenant's cells when its Authorizations satisfy the stamped
// label (enforced by the VisibilityFilter at scan time).
//
// This is shoal's analogue of Accumulo's
// org.apache.accumulo.core.iterators.user.TransformingIterator. Stamping a
// label INTO the ColumnVisibility changes a key's sort position, because CV
// is a sort-significant field (PartialKey ROW_COLFAM_COLQUAL_COLVIS). A
// naive single-pass rewrite would therefore emit keys out of order and the
// RFile writer would reject them. Like the Java TransformingIterator, this
// iterator buffers exactly the keys that share the part of the key the
// transform does NOT change — here row/cf/cq (CompareRowCFCQ) — applies the
// CV transform to each, re-sorts that bounded window by the full key order,
// and emits it. Cross-group order is preserved untouched because row/cf/cq
// is never rewritten, so a group boundary can never move.
//
// The buffer holds only the versions/visibilities at a single (row,cf,cq)
// coordinate, which is a handful of cells in practice — not the whole file.
//
// Stamping modes (option "mode"):
//
//   - "and" (default): every emitted cell requires the tenant label. A cell
//     with existing CV E becomes "(E)&(label)"; an unlabeled cell becomes
//     "label". This only ever strengthens visibility, so it can never leak
//     a producer's data to a scan lacking the tenant auth.
//   - "whenEmpty": only unlabeled cells (empty CV) are stamped with the
//     label; cells that already carry a CV are left untouched. Use when the
//     producer has already applied its own intra-tenant labels and you only
//     want to backfill a default.
//
// Both modes are collision-free: "and" yields a distinct CV per distinct
// input CV, and "whenEmpty" only touches cells whose CV was empty, so no two
// originally-distinct coordinates can collapse onto the same key.
type VisibilityStampIterator struct {
	source SortedKeyValueIterator
	label  []byte
	mode   stampMode

	buf []Cell
	idx int
	err error
}

type stampMode int

const (
	stampAnd stampMode = iota
	stampWhenEmpty
)

// VisibilityStampLabelOption is the per-iterator option carrying the tenant
// label to stamp. Required; must be a valid unquoted CV label
// ([A-Za-z0-9_:./-]+) so the visibility evaluator can parse the result.
const VisibilityStampLabelOption = "label"

// VisibilityStampModeOption selects the stamping mode ("and" | "whenEmpty").
// Defaults to "and".
const VisibilityStampModeOption = "mode"

// NewVisibilityStampIterator constructs an un-Init'd stamper. Wire it with
// Init(source, {"label": "<tenant>"}, env).
func NewVisibilityStampIterator() *VisibilityStampIterator {
	return &VisibilityStampIterator{mode: stampAnd}
}

// Init records the source and parses the label/mode options.
func (s *VisibilityStampIterator) Init(source SortedKeyValueIterator, options map[string]string, env IteratorEnvironment) error {
	if source == nil {
		return errors.New("iterrt: VisibilityStampIterator requires a non-nil source")
	}
	label := options[VisibilityStampLabelOption]
	if label == "" {
		return errors.New("iterrt: VisibilityStampIterator requires a non-empty " + VisibilityStampLabelOption)
	}
	if !validCVLabel(label) {
		return errors.New("iterrt: VisibilityStampIterator label is not a valid CV label: " + label)
	}
	switch options[VisibilityStampModeOption] {
	case "", "and":
		s.mode = stampAnd
	case "whenEmpty":
		s.mode = stampWhenEmpty
	default:
		return errors.New("iterrt: VisibilityStampIterator bad " + VisibilityStampModeOption + ": " + options[VisibilityStampModeOption])
	}
	s.source = source
	s.label = []byte(label)
	return nil
}

// Seek seeks the source and loads the first transformed group.
func (s *VisibilityStampIterator) Seek(r Range, columnFamilies [][]byte, inclusive bool) error {
	s.err = nil
	s.buf = nil
	s.idx = 0
	if err := s.source.Seek(r, columnFamilies, inclusive); err != nil {
		s.err = err
		return err
	}
	return s.fill()
}

// Next advances within the current transformed group, refilling from the
// source once the group is drained.
func (s *VisibilityStampIterator) Next() error {
	if s.err != nil {
		return s.err
	}
	if s.idx >= len(s.buf) {
		return errors.New("iterrt: VisibilityStampIterator.Next called without a top")
	}
	s.idx++
	if s.idx >= len(s.buf) {
		return s.fill()
	}
	return nil
}

// fill consumes the next (row,cf,cq) group from the source, stamps each
// cell's CV, and re-sorts the group into buf. A group is bounded: it is
// only the cells sharing row/cf/cq, i.e. the versions and visibilities of
// one cell coordinate.
func (s *VisibilityStampIterator) fill() error {
	s.buf = s.buf[:0]
	s.idx = 0
	if !s.source.HasTop() {
		return nil
	}
	group := s.source.GetTopKey().Clone()
	for s.source.HasTop() {
		k := s.source.GetTopKey()
		if k.CompareRowCFCQ(group) != 0 {
			break
		}
		nk := k.Clone()
		nk.ColumnVisibility = s.stamp(nk.ColumnVisibility)
		v := append([]byte(nil), s.source.GetTopValue()...)
		s.buf = append(s.buf, Cell{Key: nk, Value: v})
		if err := s.source.Next(); err != nil {
			s.err = err
			return err
		}
	}
	sort.SliceStable(s.buf, func(i, j int) bool {
		return s.buf[i].Key.Compare(s.buf[j].Key) < 0
	})
	return nil
}

// stamp combines an existing CV with the tenant label per the active mode.
// The returned slice is freshly allocated.
func (s *VisibilityStampIterator) stamp(existing []byte) []byte {
	if len(existing) == 0 {
		return append([]byte(nil), s.label...)
	}
	if s.mode == stampWhenEmpty {
		return existing
	}
	// "and": (existing)&(label). Idempotent guard: if the cell already
	// carries exactly the bare label, leave it as-is.
	if string(existing) == string(s.label) {
		return existing
	}
	out := make([]byte, 0, len(existing)+len(s.label)+5)
	out = append(out, '(')
	out = append(out, existing...)
	out = append(out, ')', '&', '(')
	out = append(out, s.label...)
	out = append(out, ')')
	return out
}

// HasTop reports whether a transformed cell is available.
func (s *VisibilityStampIterator) HasTop() bool {
	return s.err == nil && s.idx < len(s.buf)
}

// GetTopKey returns the current transformed top key.
func (s *VisibilityStampIterator) GetTopKey() *Key {
	if s.idx >= len(s.buf) {
		return nil
	}
	return s.buf[s.idx].Key
}

// GetTopValue returns the current top value.
func (s *VisibilityStampIterator) GetTopValue() []byte {
	if s.idx >= len(s.buf) {
		return nil
	}
	return s.buf[s.idx].Value
}

// DeepCopy returns an independent, un-seeked stamper over a DeepCopy'd
// source, carrying the label and mode forward.
func (s *VisibilityStampIterator) DeepCopy(env IteratorEnvironment) SortedKeyValueIterator {
	return &VisibilityStampIterator{
		source: s.source.DeepCopy(env),
		label:  append([]byte(nil), s.label...),
		mode:   s.mode,
	}
}

// validCVLabel reports whether b is a bare Accumulo CV label — the byte set
// the visfilter parser accepts unquoted ([A-Za-z0-9_:./-]+). Quoted-string
// labels are intentionally rejected here to keep stamped CVs parseable.
func validCVLabel(b string) bool {
	if len(b) == 0 {
		return false
	}
	for i := 0; i < len(b); i++ {
		c := b[i]
		switch {
		case c >= 'A' && c <= 'Z':
		case c >= 'a' && c <= 'z':
		case c >= '0' && c <= '9':
		case c == '_' || c == ':' || c == '.' || c == '/' || c == '-':
		default:
			return false
		}
	}
	return true
}
