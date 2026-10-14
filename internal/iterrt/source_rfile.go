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
	"fmt"
	"io"

	"github.com/phrocker/shoal-oss/internal/rfile"
)

// RFileOpener re-opens an independent *rfile.Reader over the same
// underlying RFile. RFileSource needs this for DeepCopy: rfile.Reader is
// single-consumer, so a concurrent seek requires a genuinely separate
// reader. The opener typically closes over a *bcfile.Reader + a
// block.Decompressor (both cheap to share — they're stateless w.r.t.
// iteration) and calls rfile.Open. See parity_harness_lib_test.go's
// openRFile for the canonical construction.
type RFileOpener func() (*rfile.Reader, error)

// RFileSource adapts a *rfile.Reader to SortedKeyValueIterator. It is the
// leaf of a compaction/scan stack — the iterator everything else sits on
// top of. Init takes a nil source (leaves have none).
//
// rfile.Reader exposes Seek(*Key)/Next() with io.EOF termination and
// transient keys; this adapter layers the iterrt one-cell-lookahead
// contract (HasTop/GetTopKey/GetTopValue valid until the next Seek/Next)
// on top, and clones each surfaced key so it survives past the reader's
// internal block churn.
type RFileSource struct {
	rdr    *rfile.Reader
	opener RFileOpener

	rng       Range
	cfs       [][]byte
	inclusive bool

	topKey *Key
	topVal []byte
	hasTop bool
	err    error
}

// NewRFileSource builds a leaf iterator over rdr. opener may be nil — in
// that case DeepCopy fails loudly rather than handing back a reader that
// would race the original. Callers that need DeepCopy (any stack with a
// MergingIterator above a single shared file, or a parent that re-seeks
// its source) must supply one.
func NewRFileSource(rdr *rfile.Reader, opener RFileOpener) *RFileSource {
	return &RFileSource{rdr: rdr, opener: opener}
}

// Init records env and options. A leaf takes no source; passing one is a
// usage error. RFileSource has no per-iterator options today.
func (s *RFileSource) Init(source SortedKeyValueIterator, options map[string]string, env IteratorEnvironment) error {
	if source != nil {
		return errors.New("iterrt: RFileSource is a leaf iterator, source must be nil")
	}
	if s.rdr == nil {
		return errors.New("iterrt: RFileSource has nil rfile.Reader")
	}
	return nil
}

// Seek positions the underlying reader at the range start, then advances
// past any cell that sorts before the (possibly key-granular) lower
// bound — rfile.Reader.Seek only takes a single key, so column-family
// restriction and the inclusive/exclusive boundary are applied here.
func (s *RFileSource) Seek(r Range, columnFamilies [][]byte, inclusive bool) error {
	s.rng = r
	s.cfs = columnFamilies
	s.inclusive = inclusive
	s.err = nil
	s.clearTop()

	var start *Key
	if !r.InfiniteStart {
		start = r.Start
	}
	if err := s.rdr.Seek(start); err != nil {
		s.err = err
		return err
	}
	return s.advance()
}

// Next advances past the current top.
func (s *RFileSource) Next() error {
	if s.err != nil {
		return s.err
	}
	if !s.hasTop {
		return errors.New("iterrt: RFileSource.Next called without a top")
	}
	s.clearTop()
	return s.advance()
}

// advance pulls cells from the reader until one satisfies the range
// bounds + column-family filter, or the reader/range is exhausted.
func (s *RFileSource) advance() error {
	for {
		k, v, err := s.rdr.Next()
		if errors.Is(err, io.EOF) {
			s.clearTop()
			return nil
		}
		if err != nil {
			s.err = err
			return err
		}
		if s.rng.BeforeStart(k) {
			continue
		}
		if s.rng.AfterEnd(k) {
			s.clearTop()
			return nil
		}
		if !cfAllowed(k.ColumnFamily, s.cfs, s.inclusive) {
			continue
		}
		// rfile.Reader.Next already clones, but the contract is "owned by
		// the iterator until the next Seek/Next" — clone defensively so a
		// future reader change can't break callers.
		s.topKey = k.Clone()
		s.topVal = append([]byte(nil), v...)
		s.hasTop = true
		return nil
	}
}

func (s *RFileSource) clearTop() {
	s.topKey, s.topVal, s.hasTop = nil, nil, false
}

// HasTop reports whether a top cell is available.
func (s *RFileSource) HasTop() bool { return s.hasTop }

// GetTopKey returns the current top key. Valid only when HasTop is true.
func (s *RFileSource) GetTopKey() *Key { return s.topKey }

// GetTopValue returns the current top value. Valid only when HasTop.
func (s *RFileSource) GetTopValue() []byte { return s.topVal }

// DeepCopy returns an independent RFileSource over a freshly-opened
// reader. Panics on a nil opener — a silently-shared reader would race
// the original under concurrent seeks, which is worse than a loud fail.
func (s *RFileSource) DeepCopy(env IteratorEnvironment) SortedKeyValueIterator {
	if s.opener == nil {
		panic("iterrt: RFileSource.DeepCopy with nil RFileOpener")
	}
	rdr, err := s.opener()
	if err != nil {
		panic(fmt.Sprintf("iterrt: RFileSource.DeepCopy reopen failed: %v", err))
	}
	return &RFileSource{rdr: rdr, opener: s.opener}
}

// cfAllowed applies the Seek column-family filter. An empty/nil set with
// inclusive=false means "no restriction" (the full-scan case).
func cfAllowed(cf []byte, families [][]byte, inclusive bool) bool {
	if len(families) == 0 {
		return !inclusive
	}
	in := false
	for _, f := range families {
		if string(f) == string(cf) {
			in = true
			break
		}
	}
	if inclusive {
		return in
	}
	return !in
}
