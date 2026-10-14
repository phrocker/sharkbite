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

	"github.com/phrocker/shoal-oss/internal/documentschema"
	"github.com/phrocker/shoal-oss/internal/rfile/wire"
)

// DocumentIndexIterator is the DataWave-style document retrieval pushdown
// iterator: it resolves a boolean set of field=value predicates against the
// in-shard field index (fi) and reconstructs each matching document from its
// event field cells, next to the data.
//
// It targets the documentschema physical layout (see that package):
//
//	Event field:  row=shard  cf=datatype\x00uid  cq=FIELD\x00value  val=empty
//	Field index:  row=shard  cf=fi\x00FIELD      cq=value\x00datatype\x00uid  val=empty
//
// Reading matching documents without pushdown means the client scans each
// candidate shard's field index, collects (datatype,uid) tuples, intersects
// them for a conjunction, then issues one range scan per surviving document to
// reconstruct it — streaming the entire field index and round-tripping per
// document. This iterator does that join and reconstruction server-side: given
// a set of candidate shards and a set of equality terms combined by AND or OR,
// it walks the fi entries per shard, combines the per-term (datatype,uid) sets,
// and emits the **event field cells of every surviving document** directly. One
// Seek therefore returns the matching documents' cells, not the whole shard.
//
// The consumer supplies the candidate shards (typically resolved upstream from
// the global forward index) and the terms via options:
//
//	shardCount        number of candidate shard rows (S); rows are
//	                  shard.0 .. shard.<S-1>
//	shard.<i>         the i-th candidate shard row key
//	termCount         number of equality terms (T)
//	term.<i>.field    the i-th term's FIELD name
//	term.<i>.value    the i-th term's exact value (may contain NUL bytes)
//	boolOp            "and" (default) intersects the per-term document sets
//	                  within a shard; "or" unions them
//
// Because a field value may itself contain NUL bytes, the field-index scan
// prefix-seeks value+NUL and then re-parses each qualifier via
// documentschema.ParseFieldIndexCQ, keeping only exact value matches — a naive
// prefix match would over-select across NUL boundaries.
//
// Output is every event cell of each surviving document that falls within the
// Seek range and column-family filter, globally wire.Key-sorted: shards are
// resolved in sorted order and documents within a shard in sorted event-CF
// (datatype\x00uid) order, so cells stream in the same shape as an ordinary
// scan restricted to those documents.
//
// Source contract: re-seekable (seeked once per (shard,term) fi lookup and once
// per resolved document), so it is hosted above a whole-table merge.
type DocumentIndexIterator struct {
	source SortedKeyValueIterator

	shards [][]byte
	terms  []docIndexTerm
	orMode bool

	out      []Cell
	outIndex int
	err      error
}

type docIndexTerm struct {
	field string
	value string
}

// DocumentIndexIterator option keys.
const (
	// DocumentIndexShardCount is the number of candidate shard rows supplied
	// (shard.0..shard.S-1).
	DocumentIndexShardCount = "shardCount"
	// DocumentIndexShardPrefix is the option-key prefix for each candidate
	// shard row; the i-th is at option "shard.<i>".
	DocumentIndexShardPrefix = "shard."
	// DocumentIndexTermCount is the number of equality terms supplied.
	DocumentIndexTermCount = "termCount"
	// DocumentIndexTermPrefix is the option-key prefix for each term; the
	// i-th term's field/value live at "term.<i>.field" / "term.<i>.value".
	DocumentIndexTermPrefix = "term."
	// DocumentIndexTermFieldSuffix is appended to a term key to name its field.
	DocumentIndexTermFieldSuffix = ".field"
	// DocumentIndexTermValueSuffix is appended to a term key to name its value.
	DocumentIndexTermValueSuffix = ".value"
	// DocumentIndexBoolOp selects how per-term document sets combine within a
	// shard: "and" (default, intersection) or "or" (union).
	DocumentIndexBoolOp = "boolOp"

	docIndexBoolAnd = "and"
	docIndexBoolOr  = "or"
)

// NewDocumentIndexIterator constructs an un-Init'd iterator.
func NewDocumentIndexIterator() *DocumentIndexIterator {
	return &DocumentIndexIterator{}
}

// Init wires the source and parses options. shardCount and termCount must be
// non-negative integers with every shard.<i> and term.<i>.field present;
// boolOp, if set, must be "and" or "or".
func (d *DocumentIndexIterator) Init(source SortedKeyValueIterator, options map[string]string, env IteratorEnvironment) error {
	if source == nil {
		return errors.New("iterrt: DocumentIndexIterator requires a non-nil source")
	}
	d.source = source

	sn := 0
	if s, ok := options[DocumentIndexShardCount]; ok && s != "" {
		v, err := strconv.Atoi(s)
		if err != nil || v < 0 {
			return fmt.Errorf("iterrt: DocumentIndexIterator bad %s=%q", DocumentIndexShardCount, s)
		}
		sn = v
	}
	d.shards = make([][]byte, 0, sn)
	for i := 0; i < sn; i++ {
		key := fmt.Sprintf("%s%d", DocumentIndexShardPrefix, i)
		s, ok := options[key]
		if !ok {
			return fmt.Errorf("iterrt: DocumentIndexIterator missing option %q (shardCount=%d)", key, sn)
		}
		d.shards = append(d.shards, []byte(s))
	}

	tn := 0
	if s, ok := options[DocumentIndexTermCount]; ok && s != "" {
		v, err := strconv.Atoi(s)
		if err != nil || v < 0 {
			return fmt.Errorf("iterrt: DocumentIndexIterator bad %s=%q", DocumentIndexTermCount, s)
		}
		tn = v
	}
	d.terms = make([]docIndexTerm, 0, tn)
	for i := 0; i < tn; i++ {
		fieldKey := fmt.Sprintf("%s%d%s", DocumentIndexTermPrefix, i, DocumentIndexTermFieldSuffix)
		valueKey := fmt.Sprintf("%s%d%s", DocumentIndexTermPrefix, i, DocumentIndexTermValueSuffix)
		field, ok := options[fieldKey]
		if !ok || field == "" {
			return fmt.Errorf("iterrt: DocumentIndexIterator missing option %q (termCount=%d)", fieldKey, tn)
		}
		d.terms = append(d.terms, docIndexTerm{field: field, value: options[valueKey]})
	}

	switch options[DocumentIndexBoolOp] {
	case "", docIndexBoolAnd:
		d.orMode = false
	case docIndexBoolOr:
		d.orMode = true
	default:
		return fmt.Errorf("iterrt: DocumentIndexIterator bad %s=%q (want %q or %q)",
			DocumentIndexBoolOp, options[DocumentIndexBoolOp], docIndexBoolAnd, docIndexBoolOr)
	}
	return nil
}

// Seek resolves matching documents per shard via the field index and buffers
// their event cells. The fi scan ignores the requested range and column-family
// filter (the index is addressed by its own keys); the requested range r and
// the column-family filter bound only the emitted document cells. Subsequent
// HasTop/GetTopKey/GetTopValue/Next walk the buffer.
func (d *DocumentIndexIterator) Seek(r Range, columnFamilies [][]byte, inclusive bool) error {
	d.out = d.out[:0]
	d.outIndex = 0
	d.err = nil

	if len(d.terms) == 0 {
		return nil
	}

	shards := make([][]byte, len(d.shards))
	copy(shards, d.shards)
	sort.Slice(shards, func(i, j int) bool { return bytes.Compare(shards[i], shards[j]) < 0 })

	for _, shard := range shards {
		docs, err := d.resolveShard(shard)
		if err != nil {
			d.err = err
			return err
		}
		// Emit surviving documents in sorted event-CF order so the buffer is
		// globally wire.Key-sorted.
		sort.Slice(docs, func(i, j int) bool { return docs[i] < docs[j] })
		for _, cf := range docs {
			if err := d.reconstruct(shard, []byte(cf), r, columnFamilies, inclusive); err != nil {
				d.err = err
				return err
			}
		}
	}
	return nil
}

// resolveShard returns the event column families (datatype\x00uid) of the
// documents in shard that satisfy the term set under the configured boolean op.
func (d *DocumentIndexIterator) resolveShard(shard []byte) ([]string, error) {
	var acc map[string]struct{}
	for ti, term := range d.terms {
		set, err := d.resolveTerm(shard, term)
		if err != nil {
			return nil, err
		}
		if d.orMode {
			if acc == nil {
				acc = map[string]struct{}{}
			}
			for k := range set {
				acc[k] = struct{}{}
			}
			continue
		}
		// AND: intersect.
		if ti == 0 {
			acc = set
		} else {
			for k := range acc {
				if _, ok := set[k]; !ok {
					delete(acc, k)
				}
			}
		}
		if len(acc) == 0 {
			return nil, nil
		}
	}
	out := make([]string, 0, len(acc))
	for k := range acc {
		out = append(out, k)
	}
	return out, nil
}

// resolveTerm scans the field index in shard for term.field=term.value and
// returns the set of matching event column families (datatype\x00uid).
func (d *DocumentIndexIterator) resolveTerm(shard []byte, term docIndexTerm) (map[string]struct{}, error) {
	fiCF := documentschema.FieldIndexCF(term.field)
	prefix := append([]byte(term.value), documentschema.NUL)
	start := &wire.Key{
		Row:             append([]byte(nil), shard...),
		ColumnFamily:    fiCF,
		ColumnQualifier: append([]byte(nil), prefix...),
	}
	if err := d.source.Seek(Range{Start: start, StartInclusive: true, InfiniteEnd: true}, [][]byte{fiCF}, true); err != nil {
		return nil, err
	}
	set := map[string]struct{}{}
	for d.source.HasTop() {
		k := d.source.GetTopKey()
		if !bytes.Equal(k.Row, shard) {
			break // past the shard
		}
		if !bytes.Equal(k.ColumnFamily, fiCF) {
			break // past this field's index (cf filter keeps us in fiCF, but be safe)
		}
		if !bytes.HasPrefix(k.ColumnQualifier, prefix) {
			break // past the value-prefixed run (qualifiers are sorted)
		}
		value, datatype, uid, ok := documentschema.ParseFieldIndexCQ(k.ColumnQualifier)
		if ok && value == term.value {
			set[string(documentschema.EventCF(datatype, uid))] = struct{}{}
		}
		if err := d.source.Next(); err != nil {
			return nil, err
		}
	}
	return set, nil
}

// reconstruct seeks the source to a document's event cells (row=shard,
// cf=eventCF) and appends them (within range r and allowed by the output cf
// filter) to the output buffer.
func (d *DocumentIndexIterator) reconstruct(shard, eventCF []byte, r Range, cfs [][]byte, inclusive bool) error {
	start := &wire.Key{
		Row:          append([]byte(nil), shard...),
		ColumnFamily: append([]byte(nil), eventCF...),
	}
	if err := d.source.Seek(Range{Start: start, StartInclusive: true, InfiniteEnd: true}, [][]byte{eventCF}, true); err != nil {
		return err
	}
	for d.source.HasTop() {
		k := d.source.GetTopKey()
		if !bytes.Equal(k.Row, shard) {
			break // past the shard
		}
		if !bytes.Equal(k.ColumnFamily, eventCF) {
			break // past this document's cells
		}
		if r.Contains(k) && cfAllowed(k.ColumnFamily, cfs, inclusive) {
			d.out = append(d.out, Cell{
				Key:   k.Clone(),
				Value: append([]byte(nil), d.source.GetTopValue()...),
			})
		}
		if err := d.source.Next(); err != nil {
			return err
		}
	}
	return nil
}

// HasTop reports whether a cell is available.
func (d *DocumentIndexIterator) HasTop() bool {
	return d.err == nil && d.outIndex < len(d.out)
}

// GetTopKey returns the current top key, or nil when exhausted.
func (d *DocumentIndexIterator) GetTopKey() *Key {
	if !d.HasTop() {
		return nil
	}
	return d.out[d.outIndex].Key
}

// GetTopValue returns the current top value, or nil when exhausted.
func (d *DocumentIndexIterator) GetTopValue() []byte {
	if !d.HasTop() {
		return nil
	}
	return d.out[d.outIndex].Value
}

// Next advances past the current top.
func (d *DocumentIndexIterator) Next() error {
	if d.err != nil {
		return d.err
	}
	if !d.HasTop() {
		return errors.New("iterrt: DocumentIndexIterator.Next called without a top")
	}
	d.outIndex++
	return nil
}

// DeepCopy returns an un-Seeked iterator over a DeepCopy'd source, carrying the
// same resolved options forward.
func (d *DocumentIndexIterator) DeepCopy(env IteratorEnvironment) SortedKeyValueIterator {
	cp := &DocumentIndexIterator{
		source: d.source.DeepCopy(env),
		orMode: d.orMode,
	}
	cp.shards = make([][]byte, len(d.shards))
	copy(cp.shards, d.shards)
	cp.terms = make([]docIndexTerm, len(d.terms))
	copy(cp.terms, d.terms)
	return cp
}
