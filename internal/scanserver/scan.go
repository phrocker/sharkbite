package scanserver

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/phrocker/shoal-oss/internal/metadata"
	"github.com/phrocker/shoal-oss/internal/rfile"
	"github.com/phrocker/shoal-oss/internal/rfile/wire"
	"github.com/phrocker/shoal-oss/internal/thrift/gen/client"
	"github.com/phrocker/shoal-oss/internal/thrift/gen/data"
	"github.com/phrocker/shoal-oss/internal/thrift/gen/security"
	"github.com/phrocker/shoal-oss/internal/thrift/gen/tabletscan"
	"github.com/phrocker/shoal-oss/internal/thrift/gen/tabletserver"
	"github.com/phrocker/shoal-oss/internal/tserverprocess"
	"github.com/phrocker/shoal-oss/internal/visfilter"
)

// MaxResultBytes is the soft cap on aggregate TKeyValue payload bytes
// per StartScan / StartMultiScan response. Mirrors Accumulo's default
// 4MB scan batch.
const MaxResultBytes = 4 << 20

var errTabletNotHosted = errors.New("scanserver: tablet is not hosted")

// StartScan evaluates the requested range and retains results beyond the
// response budget for lossless ContinueScan paging.
//
// Algorithm:
//  1. Look up the tablet's RFile list via metadata walker (locator-cache-
//     backed). The TKeyExtent identifies the tablet by table+endRow+prevRow.
//  2. Compile a visfilter.Evaluator over the request's authorizations.
//  3. Open each RFile (via storage backend), construct rfile.Reader with
//     the block cache, install the visibility filter at relkey level.
//  4. Seek every reader to the requested range start.
//  5. Heap-merge by Key. For each unique (row,cf,cq,cv) keep only the
//     highest-timestamp version (Accumulo's standard scan semantic; the
//     versioning iterator typically caps to 1 by default for most tables).
//  6. Skip deleted cells (tombstones) — by V0's simplified semantics, a
//     deletion marker swallows the whole coordinate.
//  7. Stop at the requested range end, then split the materialized result
//     on whole-key/value boundaries.
//  8. Return the first page and retain any remainder under the scan ID.
func (s *Server) StartScan(
	ctx context.Context,
	tinfo *client.TInfo,
	credentials *security.TCredentials,
	extent *data.TKeyExtent,
	rangeArg *data.TRange,
	columns []*data.TColumn,
	batchSize int32,
	ssiList []*data.IterInfo,
	ssio map[string]map[string]string,
	authorizations [][]byte,
	waitForWrites bool,
	isolated bool,
	readaheadThreshold int64,
	samplerConfig *tabletscan.TSamplerConfiguration,
	batchTimeOut int64,
	classLoaderContext string,
	executionHints map[string]string,
	busyTimeout int64,
) (result *data.InitialScan, retErr error) {
	t0 := time.Now()
	if !s.admitStart() {
		return nil, &tabletscan.ScanServerBusyException{}
	}
	defer s.endCall()
	defer func() { s.observeStart(t0, false, retErr) }()
	if extent == nil {
		return nil, fmt.Errorf("scanserver: nil extent")
	}
	if err := s.validateCredentials(ctx, credentials, authorizations, []string{string(extent.Table)}); err != nil {
		return nil, err
	}
	if rangeArg == nil {
		return nil, fmt.Errorf("scanserver: nil range")
	}

	files, err := s.lookupFiles(ctx, extent, rangeArg)
	if err != nil {
		if errors.Is(err, tserverprocess.ErrNotHosted) || errors.Is(err, errTabletNotHosted) {
			return nil, &tabletserver.NotServingTabletException{Extent: extent}
		}
		return nil, fmt.Errorf("lookup files: %w", err)
	}
	s.logger.LogAttrs(ctx, slog.LevelDebug, "scan: tablet→files",
		slog.String("table", string(extent.Table)),
		slog.Int("file_count", len(files)),
	)

	// Opt-in WAL-merged read path (Phase W2). Selected only by the
	// route hint; the default RFile-only path below is left untouched.
	if walRouteRequested(executionHints) {
		tablet, terr := s.lookupTablet(ctx, extent, rangeArg)
		if terr != nil {
			return nil, fmt.Errorf("lookup tablet: %w", terr)
		}
		results, approxBytes, _, werr := s.scanTabletRangeWAL(
			ctx, tablet, rangeArg, columns, authorizations, int(^uint(0)>>1),
		)
		if werr != nil {
			return nil, werr
		}
		page := splitScanResults(results, s.pages)
		scanID := data.ScanID(0)
		if page.more {
			if !s.Accepting() {
				return nil, s.rejectDraining()
			}
			scanID, err = s.scans.create(time.Now(), page.remaining)
			if err != nil {
				return nil, err
			}
		}
		s.logger.LogAttrs(ctx, slog.LevelInfo, "scan complete (wal route)",
			slog.String("table", string(extent.Table)),
			slog.Int("cells_returned", len(page.results)),
			slog.Int("cells_total", len(results)),
			slog.Int("approx_bytes_total", approxBytes),
			slog.Bool("continued", page.more),
			slog.Duration("dur", time.Since(t0)),
		)
		return &data.InitialScan{
			ScanID:  scanID,
			Result_: &data.ScanResult_{Results: page.results, More: page.more},
		}, nil
	}

	auths := visfilter.NewAuthorizations(authorizations...)
	ev := visfilter.NewEvaluator(auths)

	postProc, err := buildPostProcessor(ssiList, ssio)
	if err != nil {
		return nil, fmt.Errorf("scan: build iterator: %w", err)
	}

	results, approxBytes, _, err := s.scanTabletRanges(
		ctx, files, []*data.TRange{rangeArg}, columns, ev, int(^uint(0)>>1), postProc,
	)
	if err != nil {
		return nil, err
	}

	page := splitScanResults(results, s.pages)
	scanID := data.ScanID(0)
	if page.more {
		if !s.Accepting() {
			return nil, s.rejectDraining()
		}
		scanID, err = s.scans.create(time.Now(), page.remaining)
		if err != nil {
			return nil, err
		}
	}

	s.logger.LogAttrs(ctx, slog.LevelInfo, "scan complete",
		slog.String("table", string(extent.Table)),
		slog.Int("files", len(files)),
		slog.Int("cells_returned", len(page.results)),
		slog.Int("cells_total", len(results)),
		slog.Int("approx_bytes_total", approxBytes),
		slog.Bool("continued", page.more),
		slog.Duration("dur", time.Since(t0)),
		slog.Int("vis_cache", ev.CacheSize()),
	)

	return &data.InitialScan{
		ScanID:  scanID,
		Result_: &data.ScanResult_{Results: page.results, More: page.more},
	}, nil
}

// lookupFiles returns the RFile entries for the tablet identified by
// extent. Uses the metadata walker (which goes through the locator
// cache when configured).
//
// Auto-locate: when both extent.EndRow AND extent.PrevEndRow are nil,
// shoal interprets this as "I don't know the tablet — find one that
// covers the range start." This pragmatic V0 hack lets Java clients
// issue single-row reads without pre-resolving the tablet location.
// Multi-tablet ranges still need the caller to fan out, but that's
// the same contract Accumulo's standard scanner has.
func (s *Server) lookupFiles(ctx context.Context, extent *data.TKeyExtent, rangeArg *data.TRange) ([]metadata.FileEntry, error) {
	tabletID := string(extent.Table)
	tablets, err := s.locator.LocateTable(ctx, tabletID)
	if err != nil {
		return nil, fmt.Errorf("locate table %q: %w", tabletID, err)
	}

	// Auto-locate path: caller didn't pre-resolve the tablet. Find the
	// tablet covering the range start. (PrevRow, EndRow] semantics —
	// row r belongs to tablet t iff prevRow < r <= endRow.
	if extent.EndRow == nil && extent.PrevEndRow == nil && rangeArg != nil &&
		!rangeArg.InfiniteStartKey && rangeArg.Start != nil && len(rangeArg.Start.Row) > 0 {
		row := rangeArg.Start.Row
		for _, t := range tablets {
			if rowInTablet(row, t) {
				return t.Files, nil
			}
		}
		return nil, fmt.Errorf("%w: no tablet covers row %q in table %q", errTabletNotHosted,
			string(row), tabletID)
	}

	for _, t := range tablets {
		if extentMatchesTablet(extent, t) {
			return t.Files, nil
		}
	}
	return nil, fmt.Errorf("%w: no tablet matches extent table=%q prev=%q end=%q", errTabletNotHosted,
		string(extent.Table), string(extent.PrevEndRow), string(extent.EndRow))
}

// lookupTablet resolves the full TabletInfo (including any WAL log:
// entries) for the tablet identified by extent. It mirrors lookupFiles'
// matching — auto-locate when both boundaries are nil, exact extent
// match otherwise — but returns the whole tablet, which the WAL-merged
// read path needs for its log segments.
func (s *Server) lookupTablet(ctx context.Context, extent *data.TKeyExtent, rangeArg *data.TRange) (metadata.TabletInfo, error) {
	tabletID := string(extent.Table)
	tablets, err := s.locator.LocateTable(ctx, tabletID)
	if err != nil {
		return metadata.TabletInfo{}, fmt.Errorf("locate table %q: %w", tabletID, err)
	}

	if extent.EndRow == nil && extent.PrevEndRow == nil && rangeArg != nil &&
		!rangeArg.InfiniteStartKey && rangeArg.Start != nil && len(rangeArg.Start.Row) > 0 {
		row := rangeArg.Start.Row
		for _, t := range tablets {
			if rowInTablet(row, t) {
				return t, nil
			}
		}
		return metadata.TabletInfo{}, fmt.Errorf("scanserver: auto-locate: no tablet covers row %q in table %q",
			string(row), tabletID)
	}

	for _, t := range tablets {
		if extentMatchesTablet(extent, t) {
			return t, nil
		}
	}
	return metadata.TabletInfo{}, fmt.Errorf("scanserver: no tablet matches extent table=%q prev=%q end=%q",
		string(extent.Table), string(extent.PrevEndRow), string(extent.EndRow))
}

// rowInTablet returns true iff row falls within the tablet's
// (prevRow, endRow] range. nil prev = -inf; nil end = +inf.
func rowInTablet(row []byte, t metadata.TabletInfo) bool {
	if t.PrevRow != nil && string(row) <= string(t.PrevRow) {
		return false
	}
	if t.EndRow != nil && string(row) > string(t.EndRow) {
		return false
	}
	return true
}

func extentMatchesTablet(e *data.TKeyExtent, t metadata.TabletInfo) bool {
	if string(e.Table) != t.TableID {
		return false
	}
	if !rowsEqualOrBothAbsent(e.EndRow, t.EndRow) {
		return false
	}
	if !rowsEqualOrBothAbsent(e.PrevEndRow, t.PrevRow) {
		return false
	}
	return true
}

// rowsEqualOrBothAbsent: nil and empty are NOT the same — Accumulo
// treats nil as ±infinity. But a nil-valued boundary in the extent
// also matches a missing-value boundary in the tablet.
func rowsEqualOrBothAbsent(a, b []byte) bool {
	if a == nil && b == nil {
		return true
	}
	if a == nil || b == nil {
		return false
	}
	return string(a) == string(b)
}

// lowerBound extracts the start key from a TRange. Returns (key, inclusive, hasBound).
// hasBound=false means scan from the very beginning of the tablet.
func lowerBound(r *data.TRange) (*wire.Key, bool, bool) {
	if r.InfiniteStartKey || r.Start == nil {
		return nil, false, false
	}
	return tkeyToWireKey(r.Start), r.StartKeyInclusive, true
}

// upperBound extracts the stop key. (key, inclusive, hasBound).
func upperBound(r *data.TRange) (*wire.Key, bool, bool) {
	if r.InfiniteStopKey || r.Stop == nil {
		return nil, false, false
	}
	return tkeyToWireKey(r.Stop), r.StopKeyInclusive, true
}

// tkeyToWireKey converts a Thrift TKey into our internal wire.Key.
// TKey doesn't carry Deleted; default to false.
func tkeyToWireKey(t *data.TKey) *wire.Key {
	if t == nil {
		return nil
	}
	return &wire.Key{
		Row:              t.Row,
		ColumnFamily:     t.ColFamily,
		ColumnQualifier:  t.ColQualifier,
		ColumnVisibility: t.ColVisibility,
		Timestamp:        t.Timestamp,
	}
}

// pastUpperBound: cell key has crossed the range's stop boundary.
// stopInclusive = true keeps cells <= stop; false keeps cells < stop.
func pastUpperBound(k, stop *wire.Key, stopInclusive bool) bool {
	c := k.Compare(stop)
	if stopInclusive {
		return c > 0
	}
	return c >= 0
}

// sameCoord returns true if two keys share (row, cf, cq, cv) — the
// "coordinate" Accumulo dedupes on. Timestamps and the deleted flag
// are not part of the coord.
func sameCoord(a, b *wire.Key) bool {
	return a.CompareRowCFCQCV(b) == 0
}

func dupBytes(b []byte) []byte {
	if b == nil {
		return nil
	}
	out := make([]byte, len(b))
	copy(out, b)
	return out
}

// approxKVSize estimates the wire bytes of a TKeyValue — close enough
// for the result-size cap; we don't need to be exact since the cap is
// a soft hint.
func approxKVSize(kv *data.TKeyValue) int {
	if kv == nil || kv.Key == nil {
		return 0
	}
	return len(kv.Key.Row) + len(kv.Key.ColFamily) + len(kv.Key.ColQualifier) +
		len(kv.Key.ColVisibility) + 8 + len(kv.Value) + 16 // +16 for envelope overhead
}

// fileIter wraps an rfile.Reader with one-cell lookahead. peek() returns
// the next cell without advancing; advance() moves past it.
type fileIter struct {
	rdr     *rfile.Reader
	curK    *wire.Key
	curV    []byte
	done    bool
	closeFn func()
}

func (it *fileIter) peek() (*wire.Key, []byte) {
	if it.done {
		return nil, nil
	}
	return it.curK, it.curV
}

func (it *fileIter) advance() {
	if it.done {
		return
	}
	for {
		k, v, err := it.rdr.Next()
		if err != nil {
			it.done = true
			it.curK, it.curV = nil, nil
			return
		}
		// We Clone here because we may hold the key across heap pops
		// while the underlying block could change. Backwards-compat
		// rfile.Reader.Next already clones, so this is a no-op cost
		// at this layer; preserved for explicitness.
		it.curK = k
		it.curV = v
		return
	}
}

func (it *fileIter) close() {
	if it.closeFn != nil {
		it.closeFn()
	}
}

// iterHeap is a min-heap of fileIter ordered by current cell's Key.
// Standard container/heap interface.
type iterHeap []*fileIter

func (h iterHeap) Len() int { return len(h) }

func (h iterHeap) Less(i, j int) bool {
	ki, _ := h[i].peek()
	kj, _ := h[j].peek()
	if ki == nil {
		return false
	}
	if kj == nil {
		return true
	}
	return ki.Compare(kj) < 0
}

func (h iterHeap) Swap(i, j int) { h[i], h[j] = h[j], h[i] }

func (h *iterHeap) Push(x any) { *h = append(*h, x.(*fileIter)) }
func (h *iterHeap) Pop() any {
	old := *h
	n := len(old)
	x := old[n-1]
	*h = old[:n-1]
	return x
}
