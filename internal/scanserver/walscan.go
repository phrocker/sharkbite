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

// walscan.go is the Phase W2 opt-in read path: a scan that merges the
// tablet's in-flight QWAL segments on top of its flushed RFiles, so a
// read sees writes that have not yet been flushed to an RFile — the
// tserver-bypass "shoal+wal" route from the design doc (Bet 2, Phase W2).
//
// It is a SEPARATE code path from StartScan's default fileIter/iterHeap
// engine. The default route is byte-for-byte unchanged; this path runs
// only when executionHints carries the route flag (see RouteHintKey).
//
// Stack shape, leaves → top:
//
//	RFileSource (one per locality group, per file)  ─┐
//	memtable leaf (one per tablet, fed by qwal)      ├─ MergingIterator
//	                                                 ┘
//	  → VersioningIterator (newest-N per coordinate)
//	  → VisibilityFilter   (drops cells the auth set can't satisfy)
//
// This mirrors the tserver's scan stack: merge every source, then
// version-collapse, then visibility-filter.
package scanserver

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"

	"github.com/phrocker/shoal-oss/internal/iterrt"
	"github.com/phrocker/shoal-oss/internal/memtable"
	"github.com/phrocker/shoal-oss/internal/metadata"
	"github.com/phrocker/shoal-oss/internal/qwal"
	"github.com/phrocker/shoal-oss/internal/rfile"
	"github.com/phrocker/shoal-oss/internal/thrift/gen/data"
)

// RouteHintKey is the executionHints key a client sets to opt into the
// WAL-resident read path. RouteWAL is the value that selects it. Any
// other value (or an absent key) leaves the default RFile-only route.
//
// The design doc writes this as "?route=shoal+wal"; on the Thrift scan()
// surface it is carried as executionHints[RouteHintKey]=RouteWAL.
const (
	RouteHintKey = "shoal.route"
	RouteWAL     = "wal"
)

// walRouteRequested reports whether executionHints opted into the WAL
// route. Missing key / any other value => false (default route).
func walRouteRequested(executionHints map[string]string) bool {
	if executionHints == nil {
		return false
	}
	return executionHints[RouteHintKey] == RouteWAL
}

// walEntrySource is the minimal qwal-stream shape buildWALMemtable
// consumes. Both *qwal.EntryStream and an in-test fake satisfy it; the
// opener returns one per segment and the memtable drains them through
// qwal.NewMergedStream.
type walEntrySource interface {
	qwal.SegmentSource
	Close() error
}

// walSegmentOpener opens one WAL segment as an entry stream. Real
// production wires Server.openQWALSegment (qwal.Reader.Open); tests
// wire an in-memory fake via newServerWithOpener.
type walSegmentOpener func(ctx context.Context, peers []string, uuid, walPath string) (walEntrySource, error)

// scanTabletRangeWAL serves one range through the WAL-merged iterrt
// stack. It is invoked by StartScan only when walRouteRequested is true.
//
// Returns (results, approxBytes, truncated, error). truncated mirrors the
// default path: true means the byte budget stopped the scan early.
func (s *Server) scanTabletRangeWAL(
	ctx context.Context,
	tablet metadata.TabletInfo,
	rangeArg *data.TRange,
	columns []*data.TColumn,
	authorizations [][]byte,
	maxBytes int,
) (results []*data.TKeyValue, approxBytes int, truncated bool, err error) {
	if maxBytes <= 0 {
		return nil, 0, false, nil
	}

	wantedCFs := wantedCFsFromColumns(columns)
	colMatcher := buildColumnMatcher(columns)

	// Leaves: one RFileSource per locality group of every flushed RFile.
	// We reuse openFileIters' LG-pushdown indirectly by opening readers
	// the same way, but adapt each rfile.Reader to an iterrt leaf rather
	// than the fileIter heap.
	var leaves []iterrt.SortedKeyValueIterator
	var closers []func()
	cleanup := func() {
		for _, c := range closers {
			c()
		}
	}

	for _, f := range tablet.Files {
		readers, oerr := s.openRFileReaders(ctx, f.Path)
		if oerr != nil {
			cleanup()
			return nil, 0, false, fmt.Errorf("open %q: %w", f.Path, oerr)
		}
		for _, r := range readers {
			rdr := r
			closers = append(closers, func() { _ = rdr.Close() })
			leaves = append(leaves, iterrt.NewRFileSource(rdr, nil))
		}
	}

	// Memtable leaf: drain every WAL segment of the tablet through the
	// k-way MergedStream into one per-tablet Memtable, then take its
	// iterator. A tablet with no log: entries simply contributes nothing.
	memLeaf, walErr := s.buildWALMemtable(ctx, tablet)
	if walErr != nil {
		cleanup()
		return nil, 0, false, walErr
	}
	if memLeaf != nil {
		// The WAL memtable is the newest source: put it first so it wins
		// ties between identical keys (iterrt.MergingIterator surfaces
		// identical keys in leaf order).
		leaves = append([]iterrt.SortedKeyValueIterator{memLeaf}, leaves...)
	}
	defer cleanup()

	s.logger.LogAttrs(ctx, slog.LevelInfo, "scan: wal route",
		slog.String("table", tablet.TableID),
		slog.Int("rfile_leaves", len(leaves)-boolToInt(memLeaf != nil)),
		slog.Int("wal_segments", len(tablet.Logs)),
		slog.Bool("wal_leaf", memLeaf != nil),
	)

	// Stack: MergingIterator → VersioningIterator → VisibilityFilter.
	merge := iterrt.NewMergingIterator(leaves...)
	env := iterrt.IteratorEnvironment{
		Scope:          iterrt.ScopeScan,
		Authorizations: authorizations,
	}
	stack, serr := iterrt.BuildStack(merge, []iterrt.IterSpec{
		{Name: iterrt.IterVersioning, Options: map[string]string{iterrt.VersioningOption: "1"}},
		{Name: iterrt.IterVisibility},
	}, env)
	if serr != nil {
		return nil, 0, false, fmt.Errorf("scan: wal stack: %w", serr)
	}
	if err := merge.Init(nil, nil, env); err != nil {
		return nil, 0, false, fmt.Errorf("scan: wal stack init: %w", err)
	}

	rng, wantCFs, inclusive := trangeToIterRange(rangeArg, wantedCFs)
	if err := stack.Seek(rng, wantCFs, inclusive); err != nil {
		return nil, 0, false, fmt.Errorf("scan: wal seek: %w", err)
	}

	results = make([]*data.TKeyValue, 0, 64)
	for stack.HasTop() {
		k := stack.GetTopKey()
		v := stack.GetTopValue()

		// Cell-level column pushdown — same contract as the default path:
		// LG pushdown is CF-broad, the matcher narrows to (cf,cq) pairs.
		if colMatcher != nil && !colMatcher.match(k.ColumnFamily, k.ColumnQualifier) {
			if err := stack.Next(); err != nil {
				return nil, 0, false, fmt.Errorf("scan: wal next: %w", err)
			}
			continue
		}

		kv := &data.TKeyValue{
			Key: &data.TKey{
				Row:           dupBytes(k.Row),
				ColFamily:     dupBytes(k.ColumnFamily),
				ColQualifier:  dupBytes(k.ColumnQualifier),
				ColVisibility: dupBytes(k.ColumnVisibility),
				Timestamp:     k.Timestamp,
			},
			Value: dupBytes(v),
		}
		results = append(results, kv)
		approxBytes += approxKVSize(kv)
		if approxBytes >= maxBytes {
			return results, approxBytes, true, nil
		}
		if err := stack.Next(); err != nil {
			return nil, 0, false, fmt.Errorf("scan: wal next: %w", err)
		}
	}
	return results, approxBytes, false, nil
}

// buildWALMemtable opens every WAL segment referenced by the tablet,
// k-way merges them in recovery order (qwal.MergedStream), and ingests
// the result into a per-tablet Memtable. Returns a nil leaf (no error)
// when the tablet has no log: entries.
//
// Watermark: W2 has no clean per-tablet flush-sequence source — the
// metadata "srv:flush" column holds an opaque flush ID, not a WAL
// LogFileKey.Seq, so it cannot gate the WAL window. We therefore default
// the watermark to NoWatermark (retain every WAL mutation). This is
// safe-but-slow: over-inclusion only re-serves cells that are also in an
// RFile, and the VersioningIterator collapses the duplicate to the
// newest version. Under-inclusion would lose data — so erring toward
// over-inclusion is the correct W2 trade-off. W4 wires a real watermark.
func (s *Server) buildWALMemtable(ctx context.Context, tablet metadata.TabletInfo) (iterrt.SortedKeyValueIterator, error) {
	if len(tablet.Logs) == 0 {
		return nil, nil
	}
	opener := s.walOpener
	if opener == nil {
		return nil, errors.New("scanserver: WAL route requested but qwal reader is not configured")
	}

	var streams []walEntrySource
	closeAll := func() {
		for _, st := range streams {
			_ = st.Close()
		}
	}
	for _, le := range tablet.Logs {
		peers := s.resolvePeers(le)
		if len(peers) == 0 {
			closeAll()
			return nil, fmt.Errorf("scanserver: WAL segment %s has no peer addresses", le.UUID)
		}
		st, err := opener(ctx, peers, le.UUID, le.WALPath)
		if err != nil {
			closeAll()
			return nil, fmt.Errorf("scanserver: open WAL segment %s: %w", le.UUID, err)
		}
		streams = append(streams, st)
		s.logger.LogAttrs(ctx, slog.LevelInfo, "scan: wal segment opened",
			slog.String("segment", le.UUID),
		)
	}
	defer closeAll()

	srcs := make([]qwal.SegmentSource, len(streams))
	for i, st := range streams {
		srcs[i] = st
	}
	merged := qwal.NewMergedStream(srcs...)
	mt := memtable.New(memtable.Options{
		TabletID:  memtable.AllTablets,
		Watermark: memtable.NoWatermark,
	})
	if err := mt.IngestStream(merged); err != nil && err != io.EOF {
		return nil, fmt.Errorf("scanserver: ingest WAL into memtable: %w", err)
	}
	s.logger.LogAttrs(ctx, slog.LevelInfo, "scan: wal memtable built",
		slog.String("table", tablet.TableID),
		slog.Int("cells", mt.Len()),
		slog.Int("skipped", mt.Skipped()),
	)
	return mt.Iterator(), nil
}

// resolvePeers turns a LogEntry's recorded peer list into dialable
// "host:port" addresses. It mirrors QuorumWalLogCloser's parse: a peer
// string already carrying ":port" is used as-is; a bare hostname gets
// the fallback peer port appended. An entry with no recorded peers is
// left empty — the caller surfaces that as an error (W2 does not yet
// derive peers from the DNS pattern; that legacy fallback is W4).
func (s *Server) resolvePeers(le metadata.LogEntry) []string {
	out := make([]string, 0, len(le.Peers))
	for _, p := range le.Peers {
		if p == "" {
			continue
		}
		if hasPort(p) {
			out = append(out, p)
		} else {
			out = append(out, fmt.Sprintf("%s:%d", p, s.walPeerPort))
		}
	}
	return out
}

// hasPort reports whether addr already carries a ":port" suffix. Mirrors
// QuorumWalLogCloser's lastIndexOf(':') > 0 check.
func hasPort(addr string) bool {
	for i := len(addr) - 1; i >= 0; i-- {
		switch addr[i] {
		case ':':
			return i > 0
		case '0', '1', '2', '3', '4', '5', '6', '7', '8', '9':
			continue
		default:
			return false
		}
	}
	return false
}

// openRFileReaders opens one RFile and returns one rfile.Reader per
// locality group — the iterrt-leaf equivalent of openFileIters, minus
// the fileIter wrapping. No CF/LG pushdown here: the WAL route keeps the
// stack simple and lets the column matcher drop unwanted cells at the
// top. (LG pushdown for the WAL route is a follow-on optimization.)
func (s *Server) openRFileReaders(ctx context.Context, path string) ([]*rfile.Reader, error) {
	bc, _, err := s.openRFile(ctx, path)
	if err != nil {
		return nil, fmt.Errorf("open: %w", err)
	}
	readers, err := rfile.OpenAll(bc, s.dec, rfile.WithBlockCache(s.blocks, path))
	if err != nil {
		return nil, fmt.Errorf("rfile.OpenAll: %w", err)
	}
	return readers, nil
}

// trangeToIterRange converts the Thrift TRange + wanted-CF set into an
// iterrt.Range plus the column-family include list. The iterrt stack's
// CF filter is include-only (inclusive=true), which is what a non-empty
// wantedCFs maps to; an empty set means "no restriction" (inclusive=false).
func trangeToIterRange(r *data.TRange, wantedCFs map[string]struct{}) (iterrt.Range, [][]byte, bool) {
	var rng iterrt.Range
	if startKey, startIncl, hasStart := lowerBound(r); hasStart {
		rng.Start = startKey
		rng.StartInclusive = startIncl
	} else {
		rng.InfiniteStart = true
	}
	if stopKey, stopIncl, hasStop := upperBound(r); hasStop {
		rng.End = stopKey
		rng.EndInclusive = stopIncl
	} else {
		rng.InfiniteEnd = true
	}

	if len(wantedCFs) == 0 {
		return rng, nil, false
	}
	cfs := make([][]byte, 0, len(wantedCFs))
	for cf := range wantedCFs {
		cfs = append(cfs, []byte(cf))
	}
	return rng, cfs, true
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// columnMatcher narrows scan cells to the requested columns. A TColumn
// carrying only a ColumnFamily matches that whole family; one that also
// sets a ColumnQualifier matches only that exact (cf,cq) pair. A family
// requested whole takes precedence over any exact-CQ request for the
// same family.
type columnMatcher struct {
	families map[string]struct{}            // CF-only: whole family
	exact    map[string]map[string]struct{} // CF → set of exact CQs
}

// match reports whether (cf,cq) is among the requested columns. A nil
// matcher matches everything (no column restriction was requested).
func (m *columnMatcher) match(cf, cq []byte) bool {
	if m == nil {
		return true
	}
	if _, ok := m.families[string(cf)]; ok {
		return true
	}
	if cqs, ok := m.exact[string(cf)]; ok {
		_, ok := cqs[string(cq)]
		return ok
	}
	return false
}

// buildColumnMatcher returns a matcher for the requested columns, or nil
// when no usable columns were requested — nil means "keep every cell".
// Columns with an empty ColumnFamily are ignored.
func buildColumnMatcher(columns []*data.TColumn) *columnMatcher {
	m := &columnMatcher{
		families: make(map[string]struct{}),
		exact:    make(map[string]map[string]struct{}),
	}
	any := false
	for _, c := range columns {
		if c == nil || len(c.ColumnFamily) == 0 {
			continue
		}
		any = true
		cf := string(c.ColumnFamily)
		if len(c.ColumnQualifier) == 0 {
			m.families[cf] = struct{}{}
			continue
		}
		cqs := m.exact[cf]
		if cqs == nil {
			cqs = make(map[string]struct{})
			m.exact[cf] = cqs
		}
		cqs[string(c.ColumnQualifier)] = struct{}{}
	}
	if !any {
		return nil
	}
	return m
}
