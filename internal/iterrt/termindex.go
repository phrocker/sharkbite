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
	"sort"
	"strconv"

	"github.com/phrocker/shoal-oss/internal/rfile/wire"
)

// TermIndexIterator is the term-index (keyword) pushdown iterator described
// in docs/ai-knowledge-graph.md capability (1). It moves the inverted-index
// recall join server-side.
//
// A consumer maintains an inverted index as ordinary cells: posting rows
// (e.g. "idx:<term>") with one cell per matching primary row. Recall without
// pushdown means the client scans each posting row, collects ids, then issues
// N point lookups — streaming far more than it needs. This iterator does that
// join next to the data: given a set of posting rows, it walks their postings,
// resolves each to a primary row, and emits the **referenced primary rows'
// cells** directly. One Seek therefore returns the candidate nodes, not the
// whole table.
//
// The iterator defines only the mechanism (resolve postings -> primary rows).
// The posting/primary schema is the consumer's, supplied via options:
//
//	termCount      number of posting rows (N); rows are term.0 .. term.<N-1>
//	term.<i>       the i-th posting row key (raw bytes as a string)
//	primaryPrefix  prepended to each resolved id to form the primary row key
//	idSource       "qualifier" (default) | "value" — where the primary id lives
//	               in a posting cell
//	postingCF      optional: when set, only posting cells with this column
//	               family count as postings; empty = every cell in a posting
//	               row is a posting
//	phrase        "true" resolves the intersection of all posting rows rather
//	               than the original union. This is the iterator-runtime phrase
//	               approximation: a primary row qualifies when it appears in all
//	               term posting rows.
//	numericLower*, numericUpper*
//	               optional bounds applied to the resolved posting id parsed as
//	               a float64 before primaryPrefix is added.
//
// Resolution is the union of postings across all term rows, de-duplicated.
// The primary row key is primaryPrefix + id. Output is every cell of each
// resolved primary row that falls within the Seek range and column-family
// filter, sorted by wire.Key — so the result stream is identical in shape to
// an ordinary scan restricted to those rows.
//
// Source contract: the source must be re-seekable (the iterator seeks it once
// per posting row and once per resolved primary row). It is therefore hosted
// above a whole-table merge, not a forward-only scanner — postings and their
// primaries may live in different tablets.
type TermIndexIterator struct {
	source SortedKeyValueIterator

	termRows      [][]byte
	primaryPrefix []byte
	idFromValue   bool   // false = qualifier (default), true = value
	postingCF     []byte // nil/empty = any cf
	phrase        bool
	numericRange  *termNumericRange

	out      []Cell
	outIndex int
	err      error
}

// TermIndexIterator option keys.
const (
	// TermIndexCount is the number of posting rows supplied (term.0..term.N-1).
	TermIndexCount = "termCount"
	// TermIndexTermPrefix is the option-key prefix for each posting row;
	// the i-th posting row is at option "term.<i>".
	TermIndexTermPrefix = "term."
	// TermIndexPrimaryPrefix is prepended to each resolved id to form the
	// primary row key.
	TermIndexPrimaryPrefix = "primaryPrefix"
	// TermIndexIDSource selects where the primary id lives in a posting cell:
	// "qualifier" (default) or "value".
	TermIndexIDSource = "idSource"
	// TermIndexPostingCF optionally restricts which column family of a posting
	// row counts as a posting.
	TermIndexPostingCF = "postingCF"
	// TermIndexPhrase switches resolution from union to intersection.
	TermIndexPhrase = "phrase"
	// TermIndexNumericLower is the lower numeric bound.
	TermIndexNumericLower = "numericLower"
	// TermIndexNumericLowerSet enables the lower numeric bound.
	TermIndexNumericLowerSet = "numericLowerSet"
	// TermIndexNumericUpper is the upper numeric bound.
	TermIndexNumericUpper = "numericUpper"
	// TermIndexNumericUpperSet enables the upper numeric bound.
	TermIndexNumericUpperSet = "numericUpperSet"
	// TermIndexNumericLowerInclusive controls lower-bound inclusivity.
	TermIndexNumericLowerInclusive = "numericLowerInclusive"
	// TermIndexNumericUpperInclusive controls upper-bound inclusivity.
	TermIndexNumericUpperInclusive = "numericUpperInclusive"

	termIndexIDSourceQualifier = "qualifier"
	termIndexIDSourceValue     = "value"
)

type termNumericRange struct {
	lower, upper                   float64
	lowerSet, upperSet             bool
	lowerInclusive, upperInclusive bool
}

// NewTermIndexIterator constructs an un-Init'd iterator.
func NewTermIndexIterator() *TermIndexIterator {
	return &TermIndexIterator{}
}

// Init wires the source and parses options. termCount must be a non-negative
// integer; each term.<i> for i in [0,termCount) must be present. idSource, if
// set, must be "qualifier" or "value".
func (t *TermIndexIterator) Init(source SortedKeyValueIterator, options map[string]string, env IteratorEnvironment) error {
	if source == nil {
		return errors.New("iterrt: TermIndexIterator requires a non-nil source")
	}
	t.source = source

	n := 0
	if s, ok := options[TermIndexCount]; ok && s != "" {
		v, err := strconv.Atoi(s)
		if err != nil || v < 0 {
			return fmt.Errorf("iterrt: TermIndexIterator bad %s=%q", TermIndexCount, s)
		}
		n = v
	}
	t.termRows = make([][]byte, 0, n)
	for i := 0; i < n; i++ {
		key := fmt.Sprintf("%s%d", TermIndexTermPrefix, i)
		s, ok := options[key]
		if !ok {
			return fmt.Errorf("iterrt: TermIndexIterator missing option %q (termCount=%d)", key, n)
		}
		t.termRows = append(t.termRows, []byte(s))
	}

	if s, ok := options[TermIndexPrimaryPrefix]; ok {
		t.primaryPrefix = []byte(s)
	}
	if s, ok := options[TermIndexPostingCF]; ok && s != "" {
		t.postingCF = []byte(s)
	}
	switch options[TermIndexIDSource] {
	case "", termIndexIDSourceQualifier:
		t.idFromValue = false
	case termIndexIDSourceValue:
		t.idFromValue = true
	default:
		return fmt.Errorf("iterrt: TermIndexIterator bad %s=%q (want %q or %q)",
			TermIndexIDSource, options[TermIndexIDSource], termIndexIDSourceQualifier, termIndexIDSourceValue)
	}
	if s, ok := options[TermIndexPhrase]; ok && s != "" {
		v, err := strconv.ParseBool(s)
		if err != nil {
			return fmt.Errorf("iterrt: TermIndexIterator bad %s=%q", TermIndexPhrase, s)
		}
		t.phrase = v
	}
	nr := &termNumericRange{}
	if s, ok := options[TermIndexNumericLowerSet]; ok && s != "" {
		v, err := strconv.ParseBool(s)
		if err != nil {
			return fmt.Errorf("iterrt: TermIndexIterator bad %s=%q", TermIndexNumericLowerSet, s)
		}
		nr.lowerSet = v
	}
	if s, ok := options[TermIndexNumericUpperSet]; ok && s != "" {
		v, err := strconv.ParseBool(s)
		if err != nil {
			return fmt.Errorf("iterrt: TermIndexIterator bad %s=%q", TermIndexNumericUpperSet, s)
		}
		nr.upperSet = v
	}
	if s, ok := options[TermIndexNumericLower]; ok && s != "" {
		v, err := strconv.ParseFloat(s, 64)
		if err != nil {
			return fmt.Errorf("iterrt: TermIndexIterator bad %s=%q", TermIndexNumericLower, s)
		}
		nr.lower = v
	}
	if s, ok := options[TermIndexNumericUpper]; ok && s != "" {
		v, err := strconv.ParseFloat(s, 64)
		if err != nil {
			return fmt.Errorf("iterrt: TermIndexIterator bad %s=%q", TermIndexNumericUpper, s)
		}
		nr.upper = v
	}
	if s, ok := options[TermIndexNumericLowerInclusive]; ok && s != "" {
		v, err := strconv.ParseBool(s)
		if err != nil {
			return fmt.Errorf("iterrt: TermIndexIterator bad %s=%q", TermIndexNumericLowerInclusive, s)
		}
		nr.lowerInclusive = v
	}
	if s, ok := options[TermIndexNumericUpperInclusive]; ok && s != "" {
		v, err := strconv.ParseBool(s)
		if err != nil {
			return fmt.Errorf("iterrt: TermIndexIterator bad %s=%q", TermIndexNumericUpperInclusive, s)
		}
		nr.upperInclusive = v
	}
	if nr.lowerSet || nr.upperSet {
		t.numericRange = nr
	}
	return nil
}

// Seek resolves the configured posting rows to primary rows and buffers the
// primary rows' cells. The posting scan ignores the requested range and
// column-family filter (postings are an internal index, addressed by their
// own row keys); the requested range r and the column-family filter bound
// only the emitted primary cells. Subsequent HasTop/GetTopKey/GetTopValue/Next
// walk the buffer.
func (t *TermIndexIterator) Seek(r Range, columnFamilies [][]byte, inclusive bool) error {
	t.out = t.out[:0]
	t.outIndex = 0
	t.err = nil

	// Phase 1: collect candidate primary row keys from every posting row.
	seen := map[string]struct{}{}
	var primaries [][]byte
	ids, err := t.collectCandidateIDs()
	if err != nil {
		t.err = err
		return err
	}
	for _, id := range ids {
		pr := make([]byte, 0, len(t.primaryPrefix)+len(id))
		pr = append(pr, t.primaryPrefix...)
		pr = append(pr, id...)
		if _, dup := seen[string(pr)]; dup {
			continue
		}
		seen[string(pr)] = struct{}{}
		primaries = append(primaries, pr)
	}

	// Deterministic output order: resolve primary rows in sorted-key order so
	// the buffer is globally wire.Key-sorted (cells within a row already are).
	sort.Slice(primaries, func(i, j int) bool {
		return bytes.Compare(primaries[i], primaries[j]) < 0
	})

	// Phase 2: fetch each primary row's cells, bounded by r and the cf filter.
	for _, pr := range primaries {
		if err := t.collectRow(pr, r, columnFamilies, inclusive); err != nil {
			t.err = err
			return err
		}
	}
	return nil
}

func (t *TermIndexIterator) collectCandidateIDs() ([][]byte, error) {
	if !t.phrase {
		var ids [][]byte
		for _, tr := range t.termRows {
			rowIDs, err := t.collectPostings(tr)
			if err != nil {
				return nil, err
			}
			ids = append(ids, rowIDs...)
		}
		return ids, nil
	}
	if len(t.termRows) == 0 {
		return nil, nil
	}
	counts := map[string]int{}
	firstOrder := [][]byte{}
	for i, tr := range t.termRows {
		rowIDs, err := t.collectPostings(tr)
		if err != nil {
			return nil, err
		}
		rowSeen := map[string][]byte{}
		for _, id := range rowIDs {
			rowSeen[string(id)] = id
		}
		if i == 0 {
			for _, id := range rowIDs {
				if _, ok := counts[string(id)]; !ok {
					firstOrder = append(firstOrder, id)
				}
			}
		}
		for key := range rowSeen {
			counts[key]++
		}
	}
	var out [][]byte
	for _, id := range firstOrder {
		if counts[string(id)] == len(t.termRows) {
			out = append(out, id)
		}
	}
	return out, nil
}

// collectPostings seeks the source to posting row tr and returns the resolved
// primary ids (column qualifier or value, per idSource), restricted to
// postingCF when configured.
func (t *TermIndexIterator) collectPostings(tr []byte) ([][]byte, error) {
	start := &wire.Key{Row: append([]byte(nil), tr...)}
	if err := t.source.Seek(Range{Start: start, StartInclusive: true, InfiniteEnd: true}, nil, false); err != nil {
		return nil, err
	}
	var ids [][]byte
	for t.source.HasTop() {
		k := t.source.GetTopKey()
		if !bytes.Equal(k.Row, tr) {
			break // past the posting row
		}
		if len(t.postingCF) == 0 || bytes.Equal(k.ColumnFamily, t.postingCF) {
			var id []byte
			if t.idFromValue {
				id = append([]byte(nil), t.source.GetTopValue()...)
			} else {
				id = append([]byte(nil), k.ColumnQualifier...)
			}
			if len(id) > 0 && t.numericOK(id) {
				ids = append(ids, id)
			}
		}
		if err := t.source.Next(); err != nil {
			return nil, err
		}
	}
	return ids, nil
}

func (t *TermIndexIterator) numericOK(id []byte) bool {
	if t.numericRange == nil {
		return true
	}
	v, err := strconv.ParseFloat(string(id), 64)
	if err != nil {
		return false
	}
	nr := t.numericRange
	if nr.lowerSet {
		if nr.lowerInclusive {
			if v < nr.lower {
				return false
			}
		} else if v <= nr.lower {
			return false
		}
	}
	if nr.upperSet {
		if nr.upperInclusive {
			if v > nr.upper {
				return false
			}
		} else if v >= nr.upper {
			return false
		}
	}
	return true
}

// collectRow seeks the source to primary row pr and appends its cells (within
// range r and allowed by the cf filter) to the output buffer.
func (t *TermIndexIterator) collectRow(pr []byte, r Range, cfs [][]byte, inclusive bool) error {
	start := &wire.Key{Row: append([]byte(nil), pr...)}
	if err := t.source.Seek(Range{Start: start, StartInclusive: true, InfiniteEnd: true}, cfs, inclusive); err != nil {
		return err
	}
	for t.source.HasTop() {
		k := t.source.GetTopKey()
		if !bytes.Equal(k.Row, pr) {
			break // past the primary row
		}
		if r.Contains(k) {
			t.out = append(t.out, Cell{
				Key:   k.Clone(),
				Value: append([]byte(nil), t.source.GetTopValue()...),
			})
		}
		if err := t.source.Next(); err != nil {
			return err
		}
	}
	return nil
}

// HasTop reports whether a cell is available.
func (t *TermIndexIterator) HasTop() bool {
	return t.err == nil && t.outIndex < len(t.out)
}

// GetTopKey returns the current top key, or nil when exhausted.
func (t *TermIndexIterator) GetTopKey() *Key {
	if !t.HasTop() {
		return nil
	}
	return t.out[t.outIndex].Key
}

// GetTopValue returns the current top value, or nil when exhausted.
func (t *TermIndexIterator) GetTopValue() []byte {
	if !t.HasTop() {
		return nil
	}
	return t.out[t.outIndex].Value
}

// Next advances past the current top.
func (t *TermIndexIterator) Next() error {
	if t.err != nil {
		return t.err
	}
	if !t.HasTop() {
		return errors.New("iterrt: TermIndexIterator.Next called without a top")
	}
	t.outIndex++
	return nil
}

// DeepCopy returns an un-Seeked iterator over a DeepCopy'd source, carrying
// the same resolved options forward.
func (t *TermIndexIterator) DeepCopy(env IteratorEnvironment) SortedKeyValueIterator {
	cp := &TermIndexIterator{
		source:        t.source.DeepCopy(env),
		primaryPrefix: t.primaryPrefix,
		idFromValue:   t.idFromValue,
		postingCF:     t.postingCF,
		phrase:        t.phrase,
		numericRange:  t.numericRange,
	}
	cp.termRows = make([][]byte, len(t.termRows))
	copy(cp.termRows, t.termRows)
	return cp
}
