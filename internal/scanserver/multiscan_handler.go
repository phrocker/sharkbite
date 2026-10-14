package scanserver

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"math"
	"sort"
	"time"

	"github.com/phrocker/shoal-oss/internal/thrift/gen/client"
	"github.com/phrocker/shoal-oss/internal/thrift/gen/data"
	"github.com/phrocker/shoal-oss/internal/thrift/gen/security"
	"github.com/phrocker/shoal-oss/internal/thrift/gen/tabletscan"
	"github.com/phrocker/shoal-oss/internal/visfilter"
)

// StartMultiScan implements the BatchScanner-shaped server-side path.
// Each tablet is scanned independently. Successful results are paged
// behind one stable scan ID while recoverable tablet failures are
// returned in MultiScanResult.Failures for client retry.
//
// Compared to StartScan:
//   - Caller hands in a ScanBatch (map TKeyExtent → []TRange) instead of
//     a single (extent, range) pair.
//   - Per-tablet range lists are normalized (sort + merge overlaps),
//     then driven through scanTabletRanges.
//
// Result ordering is deterministic across map iteration so continuation
// boundaries are reproducible.
func (s *Server) StartMultiScan(
	ctx context.Context,
	tinfo *client.TInfo,
	credentials *security.TCredentials,
	batch data.ScanBatch,
	columns []*data.TColumn,
	ssiList []*data.IterInfo,
	ssio map[string]map[string]string,
	authorizations [][]byte,
	waitForWrites bool,
	samplerConfig *tabletscan.TSamplerConfiguration,
	batchTimeOut int64,
	classLoaderContext string,
	executionHints map[string]string,
	busyTimeout int64,
) (result *data.InitialMultiScan, retErr error) {
	t0 := time.Now()
	if !s.admitStart() {
		return nil, &tabletscan.ScanServerBusyException{}
	}
	defer s.endCall()
	defer func() { s.observeStart(t0, true, retErr) }()
	tableIDs := make([]string, 0, len(batch))
	seenTables := make(map[string]struct{}, len(batch))
	for extent := range batch {
		if extent == nil {
			continue
		}
		tableID := string(extent.Table)
		if _, seen := seenTables[tableID]; !seen {
			seenTables[tableID] = struct{}{}
			tableIDs = append(tableIDs, tableID)
		}
	}
	if err := s.validateCredentials(ctx, credentials, authorizations, tableIDs); err != nil {
		return nil, err
	}
	if len(batch) == 0 {
		scanID, err := s.multiScans.createCompleted(time.Now())
		if err != nil {
			return nil, err
		}
		return &data.InitialMultiScan{
			ScanID:  scanID,
			Result_: &data.MultiScanResult_{Results: nil, More: false},
		}, nil
	}

	auths := visfilter.NewAuthorizations(authorizations...)
	ev := visfilter.NewEvaluator(auths)

	allResults := make([]*data.TKeyValue, 0, 64)
	failures := make(data.ScanBatch)
	fullScans := make([]*data.TKeyExtent, 0, len(batch))
	totalBytes := 0
	tabletsScanned := 0

	// Iterator post-processor is built ONCE per multiscan but applied
	// per-tablet. Each tablet gets its own (top-K) result; the Java
	// client merges across tablets to form the global top-K. To avoid
	// shared state we instantiate a fresh post-processor per tablet
	// inside the loop.
	hasIterator := len(ssiList) > 0

	// V0.5 simplification: when the caller hands in a single TKeyExtent
	// that's table-id-only (no EndRow / no PrevEndRow), we fan out the
	// ranges across our own tablet map. This lets the Java SDK skip
	// client-side binning entirely — it sends "all ranges for table X"
	// and shoal does the per-row routing internally. Drops the need for
	// a TabletLocator on the SDK side, which is the whole point of
	// going around the AccumuloClient's ScanServerSelector.
	expanded := batch
	if len(batch) == 1 {
		for extent, ranges := range batch {
			if extent != nil && extent.EndRow == nil && extent.PrevEndRow == nil && len(ranges) > 0 {
				binned, berr := s.binRangesByTablet(ctx, extent, ranges)
				if berr != nil {
					return nil, fmt.Errorf("multiscan bin ranges (table=%s): %w", string(extent.Table), berr)
				}
				expanded = binned
			}
		}
	}

	type extentWork struct {
		extent *data.TKeyExtent
		ranges []*data.TRange
	}
	work := make([]extentWork, 0, len(expanded))
	for extent, ranges := range expanded {
		work = append(work, extentWork{extent: extent, ranges: ranges})
	}
	sort.Slice(work, func(i, j int) bool {
		return compareExtents(work[i].extent, work[j].extent) < 0
	})

	for _, item := range work {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		extent, ranges := item.extent, item.ranges
		if extent == nil || len(ranges) == 0 {
			continue
		}
		// Use the same per-extent file lookup as StartScan. The first
		// range hands lookupFiles a sample for the auto-locate path
		// when the caller didn't pre-resolve the extent.
		files, err := s.lookupFiles(ctx, extent, ranges[0])
		if err != nil {
			failures[cloneTKeyExtent(extent)] = cloneRanges(ranges)
			continue
		}
		var perTabletProc cellPostProcessor
		if hasIterator {
			pp, perr := buildPostProcessor(ssiList, ssio)
			if perr != nil {
				return nil, fmt.Errorf("multiscan: build iterator: %w", perr)
			}
			perTabletProc = pp
		}
		results, used, perTabletTrunc, err := s.scanTabletRanges(
			ctx, files, ranges, columns, ev, int(^uint(0)>>1), perTabletProc,
		)
		if err != nil {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			failures[cloneTKeyExtent(extent)] = cloneRanges(ranges)
			continue
		}
		if perTabletTrunc {
			return nil, fmt.Errorf("multiscan tablet (table=%s): unexpected internal truncation", string(extent.Table))
		}
		allResults = append(allResults, results...)
		totalBytes += used
		tabletsScanned++
		fullScans = append(fullScans, cloneTKeyExtent(extent))
	}

	complete := &data.MultiScanResult_{
		Results:   allResults,
		Failures:  failures,
		FullScans: fullScans,
	}
	page := splitMultiScanResult(complete, s.pages)
	var (
		scanID    data.ScanID
		createErr error
	)
	if page.result.More {
		if !s.Accepting() {
			return nil, s.rejectDraining()
		}
		scanID, createErr = s.multiScans.create(
			time.Now(),
			page.remaining,
			page.tail,
		)
	} else {
		scanID, createErr = s.multiScans.createCompleted(time.Now())
	}
	if createErr != nil {
		return nil, createErr
	}
	s.metrics.failuresTabletMulti.Add(uint64(len(failures)))

	s.logger.LogAttrs(ctx, slog.LevelInfo, "multiscan complete",
		slog.Int("tablets_in_batch", len(expanded)),
		slog.Int("tablets_scanned", tabletsScanned),
		slog.Int("tablets_failed", len(failures)),
		slog.Int("cells_returned", len(page.result.Results)),
		slog.Int("cells_total", len(allResults)),
		slog.Int("approx_bytes_total", totalBytes),
		slog.Bool("continued", page.result.More),
		slog.Duration("dur", time.Since(t0)),
		slog.Int("vis_cache", ev.CacheSize()),
	)

	return &data.InitialMultiScan{
		ScanID:  scanID,
		Result_: page.result,
	}, nil
}

func compareExtents(a, b *data.TKeyExtent) int {
	if a == nil {
		if b == nil {
			return 0
		}
		return -1
	}
	if b == nil {
		return 1
	}
	if c := bytes.Compare(a.Table, b.Table); c != 0 {
		return c
	}
	if c := compareOptionalRows(a.PrevEndRow, b.PrevEndRow, true); c != 0 {
		return c
	}
	return compareOptionalRows(a.EndRow, b.EndRow, false)
}

func compareOptionalRows(a, b []byte, nilFirst bool) int {
	if a == nil {
		if b == nil {
			return 0
		}
		if nilFirst {
			return -1
		}
		return 1
	}
	if b == nil {
		if nilFirst {
			return 1
		}
		return -1
	}
	return bytes.Compare(a, b)
}

func cloneRanges(ranges []*data.TRange) []*data.TRange {
	cloned := make([]*data.TRange, len(ranges))
	for i, r := range ranges {
		cloned[i] = cloneTRange(r)
	}
	return cloned
}

// binRangesByTablet fans ranges out across the table's tablets. Each output
// extent is a fully-resolved
// TKeyExtent (table + endRow + prevEndRow) so downstream lookupFiles
// finds it directly. The original range is applied to each tablet's disjoint
// files. Ranges are clipped to each tablet's (PrevRow, EndRow] extent because
// split tablets may still reference the same immutable RFile.
func (s *Server) binRangesByTablet(ctx context.Context, extent *data.TKeyExtent, ranges []*data.TRange) (data.ScanBatch, error) {
	tableID := string(extent.Table)
	tablets, err := s.locator.LocateTable(ctx, tableID)
	if err != nil {
		return nil, fmt.Errorf("locate table %q: %w", tableID, err)
	}
	if len(tablets) == 0 {
		return nil, fmt.Errorf("multiscan binner: no tablets found for table %q", tableID)
	}

	out := make(data.ScanBatch, len(tablets))
	for _, tablet := range tablets {
		resolved := &data.TKeyExtent{
			Table:      extent.Table,
			EndRow:     tablet.EndRow,
			PrevEndRow: tablet.PrevRow,
		}
		for _, r := range ranges {
			if clipped := clipRangeToExtent(r, resolved); clipped != nil {
				out = appendToScanBatch(out, resolved, clipped)
			}
		}
	}
	return out, nil
}

func clipRangeToExtent(r *data.TRange, extent *data.TKeyExtent) *data.TRange {
	if r == nil || extent == nil {
		return nil
	}
	clipped := cloneTRange(r)
	if extent.PrevEndRow != nil {
		tabletStart := &data.TKey{
			Row:       append(append([]byte(nil), extent.PrevEndRow...), 0),
			Timestamp: math.MaxInt64,
		}
		start, inclusive, bounded := lowerBound(clipped)
		if !bounded || start.Compare(tkeyToWireKey(tabletStart)) < 0 {
			clipped.Start = tabletStart
			clipped.StartKeyInclusive = true
			clipped.InfiniteStartKey = false
		} else {
			clipped.StartKeyInclusive = inclusive
		}
	}
	if extent.EndRow != nil {
		tabletStop := &data.TKey{
			Row:       append(append([]byte(nil), extent.EndRow...), 0),
			Timestamp: math.MaxInt64,
		}
		stop, inclusive, bounded := upperBound(clipped)
		if !bounded || stop.Compare(tkeyToWireKey(tabletStop)) > 0 {
			clipped.Stop = tabletStop
			clipped.StopKeyInclusive = false
			clipped.InfiniteStopKey = false
		} else {
			clipped.StopKeyInclusive = inclusive
		}
	}
	start, startInclusive, hasStart := lowerBound(clipped)
	stop, stopInclusive, hasStop := upperBound(clipped)
	if hasStart && hasStop {
		switch comparison := start.Compare(stop); {
		case comparison > 0:
			return nil
		case comparison == 0 && !(startInclusive && stopInclusive):
			return nil
		}
	}
	return clipped
}

// appendToScanBatch ensures we group ranges under one extent value
// (TKeyExtent has no usable hash key as a *pointer*, so value-compare).
func appendToScanBatch(b data.ScanBatch, extent *data.TKeyExtent, r *data.TRange) data.ScanBatch {
	for k, v := range b {
		if k != nil && extentEqual(k, extent) {
			b[k] = append(v, r)
			return b
		}
	}
	b[extent] = []*data.TRange{r}
	return b
}

func extentEqual(a, b *data.TKeyExtent) bool {
	return string(a.Table) == string(b.Table) &&
		string(a.EndRow) == string(b.EndRow) &&
		string(a.PrevEndRow) == string(b.PrevEndRow)
}
