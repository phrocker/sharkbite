package accumulo

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/phrocker/shoal-oss/internal/scanclient"
	"github.com/phrocker/shoal-oss/internal/thrift/gen/data"
)

// BatchScanner scans one or more ranges across tablet boundaries.
// The default single-scan path returns results in input-range and tablet order.
// UseMultiScan uses server-defined result order.
type BatchScanner struct {
	scanner *Scanner
}

type batchScanSegment struct {
	routingRow []byte
	tablet     Tablet
	scanRange  *Range
}

type batchScanResult struct {
	values []KeyValue
	err    error
}

type batchMultiScanGroup struct {
	address  string
	segments []batchScanSegment
	batch    data.ScanBatch
}

type tabletExtentMapKey struct {
	tableID    string
	prevRow    string
	prevRowSet bool
	endRow     string
	endRowSet  bool
}

// NewBatchScanner constructs a multi-tablet scanner for table.
func (c *Connector) NewBatchScanner(table Table, options ScannerOptions) (*BatchScanner, error) {
	scanner, err := c.NewScanner(table, options)
	if err != nil {
		return nil, err
	}
	return &BatchScanner{scanner: scanner}, nil
}

// Scan reads ranges after splitting them at tablet boundaries. Cleanup errors
// are accumulated while scanning continues because returned results remain
// usable.
func (s *BatchScanner) Scan(ctx context.Context, ranges []*Range) ([]KeyValue, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if len(ranges) == 0 {
		return nil, errors.New("accumulo: batch scan requires at least one range")
	}
	table, err := s.scanner.resolveTable(ctx)
	if err != nil {
		return nil, err
	}
	scanner := *s.scanner
	scanner.table = table

	segments, err := scanner.planBatchScan(ctx, table, ranges)
	if err != nil {
		return nil, err
	}
	var results []batchScanResult
	if scanner.options.UseMultiScan {
		multi, ok := scanner.connector.scan.(scanclient.MultiLifecycle)
		if !ok {
			return nil, errors.New("accumulo: scan adapter does not support multi-scan")
		}
		results = scanner.executeMultiBatchScan(ctx, table, multi, groupBatchSegments(segments))
	} else {
		results = scanner.executeBatchScan(ctx, table, segments)
	}

	var values []KeyValue
	var cleanupErr error
	for _, result := range results {
		values = append(values, result.values...)
		if result.err == nil {
			continue
		}
		if !onlyCleanupErrors(result.err) {
			return values, errors.Join(cleanupErr, result.err)
		}
		cleanupErr = errors.Join(cleanupErr, result.err)
	}
	return values, cleanupErr
}

func (s *Scanner) planBatchScan(
	ctx context.Context,
	table Table,
	ranges []*Range,
) ([]batchScanSegment, error) {
	var segments []batchScanSegment
	for index, scanRange := range ranges {
		if scanRange == nil {
			return nil, fmt.Errorf("accumulo: batch scan range %d is nil", index)
		}
		remaining := cloneRange(scanRange)
		for {
			routingRow := remaining.routingRow()
			tablet, err := s.locateTablet(ctx, table, routingRow)
			if err != nil {
				return nil, err
			}
			segment, done := clipRangeToTablet(remaining, tablet)
			segments = append(segments, batchScanSegment{
				routingRow: routingRow,
				tablet:     tablet,
				scanRange:  segment,
			})
			if done {
				break
			}
			remaining = &Range{
				startKey:       keyForRow(tablet.Extent.EndRow),
				endKey:         cloneKey(remaining.endKey),
				startRowOnly:   true,
				endRowOnly:     remaining.endRowOnly,
				startInclusive: false,
				endInclusive:   remaining.endInclusive,
			}
		}
	}
	return segments, nil
}

func (s *Scanner) executeBatchScan(
	ctx context.Context,
	table Table,
	segments []batchScanSegment,
) []batchScanResult {
	results := make([]batchScanResult, len(segments))
	parallelism := s.options.Parallelism
	if parallelism == 0 {
		parallelism = 1
	}
	if parallelism > len(segments) {
		parallelism = len(segments)
	}

	jobs := make(chan int)
	var workers sync.WaitGroup
	workers.Add(parallelism)
	for range parallelism {
		go func() {
			defer workers.Done()
			for index := range jobs {
				segment := segments[index]
				results[index].values, results[index].err = s.scanLocated(
					ctx,
					table,
					segment.routingRow,
					segment.tablet,
					segment.scanRange,
				)
			}
		}()
	}
	for index := range segments {
		jobs <- index
	}
	close(jobs)
	workers.Wait()
	return results
}

func groupBatchSegments(segments []batchScanSegment) []batchMultiScanGroup {
	var groups []batchMultiScanGroup
	groupIndexes := make(map[string]int)
	extentIndexes := make([]map[tabletExtentMapKey]*data.TKeyExtent, 0)
	for _, segment := range segments {
		address := segment.tablet.Server.HostPort
		groupIndex, ok := groupIndexes[address]
		if !ok {
			groupIndex = len(groups)
			groupIndexes[address] = groupIndex
			groups = append(groups, batchMultiScanGroup{
				address: address,
				batch:   make(data.ScanBatch),
			})
			extentIndexes = append(
				extentIndexes,
				make(map[tabletExtentMapKey]*data.TKeyExtent),
			)
		}
		group := &groups[groupIndex]
		group.segments = append(group.segments, segment)
		extentKey := tabletExtentKey(segment.tablet)
		extent := extentIndexes[groupIndex][extentKey]
		if extent == nil {
			extent = tabletExtentToThrift(segment.tablet)
			extentIndexes[groupIndex][extentKey] = extent
		}
		group.batch[extent] = append(group.batch[extent], segment.scanRange.toThrift())
	}
	return groups
}

func (s *Scanner) executeMultiBatchScan(
	ctx context.Context,
	table Table,
	multi scanclient.MultiLifecycle,
	groups []batchMultiScanGroup,
) []batchScanResult {
	results := make([]batchScanResult, len(groups))
	parallelism := s.options.Parallelism
	if parallelism == 0 {
		parallelism = 1
	}
	if parallelism > len(groups) {
		parallelism = len(groups)
	}

	jobs := make(chan int)
	var workers sync.WaitGroup
	workers.Add(parallelism)
	for range parallelism {
		go func() {
			defer workers.Done()
			for index := range jobs {
				results[index].values, results[index].err = s.scanMultiGroup(
					ctx,
					table,
					multi,
					groups[index],
				)
			}
		}()
	}
	for index := range groups {
		jobs <- index
	}
	close(jobs)
	workers.Wait()
	return results
}

func (s *Scanner) scanMultiGroup(
	ctx context.Context,
	table Table,
	multi scanclient.MultiLifecycle,
	group batchMultiScanGroup,
) ([]KeyValue, error) {
	iterators, iteratorOptions := iteratorsToThrift(s.options.Iterators)
	initial, err := multi.StartMulti(ctx, group.address, scanclient.MultiStartRequest{
		Batch:           group.batch,
		Columns:         columnsToThrift(s.options.Columns),
		Iterators:       iterators,
		IteratorOptions: iteratorOptions,
		Authorizations:  cloneByteSlices(s.options.Authorizations),
	})
	if initial == nil && err != nil {
		return nil, err
	}
	if initial == nil {
		return nil, errors.New("accumulo: start multi-scan returned no result")
	}

	var values []KeyValue
	var failures data.ScanBatch
	resultErr := err
	result := initial.Result_
	for {
		if result != nil {
			values, err = appendKeyValues(values, result.Results)
			resultErr = errors.Join(resultErr, err)
			failures = appendScanBatch(failures, result.Failures)
		}
		if err != nil || result == nil || !result.More {
			break
		}
		result, err = multi.ContinueMulti(ctx, group.address, initial.ScanID, 0)
		if err != nil {
			resultErr = errors.Join(resultErr, err)
			break
		}
	}
	if result != nil && result.PartScan != nil {
		resultErr = errors.Join(
			resultErr,
			errors.New("accumulo: multi-scan ended with an incomplete tablet range"),
		)
	}

	var cleanupErr error
	if initial.ScanID != 0 {
		closeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), scannerCloseTimeout)
		defer cancel()
		if err := multi.CloseMultiScan(closeCtx, group.address, initial.ScanID); err != nil {
			cleanupErr = &CleanupError{ScanID: int64(initial.ScanID), Err: err}
		}
	}
	if resultErr != nil {
		return values, errors.Join(resultErr, cleanupErr)
	}

	failedSegments, err := matchFailedSegments(group.segments, failures)
	if err != nil {
		return values, errors.Join(err, cleanupErr)
	}
	for _, segment := range failedSegments {
		if err := s.connector.InvalidateTablet(table, segment.routingRow); err != nil {
			return values, errors.Join(err, cleanupErr)
		}
		replanned, err := s.planBatchScan(ctx, table, []*Range{segment.scanRange})
		if err != nil {
			return values, errors.Join(err, cleanupErr)
		}
		for _, retrySegment := range replanned {
			segmentValues, err := s.scanLocated(
				ctx,
				table,
				retrySegment.routingRow,
				retrySegment.tablet,
				retrySegment.scanRange,
			)
			values = append(values, segmentValues...)
			if err != nil {
				return values, errors.Join(err, cleanupErr)
			}
		}
	}
	return values, cleanupErr
}

func appendScanBatch(target, source data.ScanBatch) data.ScanBatch {
	if len(source) == 0 {
		return target
	}
	if target == nil {
		target = make(data.ScanBatch)
	}
	for extent, ranges := range source {
		target[extent] = append(target[extent], ranges...)
	}
	return target
}

func matchFailedSegments(
	segments []batchScanSegment,
	failures data.ScanBatch,
) ([]batchScanSegment, error) {
	if len(failures) == 0 {
		return nil, nil
	}
	matched := make([]bool, len(segments))
	var failed []batchScanSegment
	for extent, ranges := range failures {
		for _, failedRange := range ranges {
			found := false
			for index, segment := range segments {
				if !thriftExtentEqual(extent, tabletExtentToThrift(segment.tablet)) ||
					!thriftRangeEqual(failedRange, segment.scanRange.toThrift()) {
					continue
				}
				if !matched[index] {
					matched[index] = true
					failed = append(failed, segment)
				}
				found = true
				break
			}
			if !found {
				return nil, errors.New("accumulo: multi-scan returned an unknown failed range")
			}
		}
	}
	return failed, nil
}

func thriftExtentEqual(left, right *data.TKeyExtent) bool {
	return left != nil && right != nil &&
		bytes.Equal(left.Table, right.Table) &&
		optionalBytesEqual(left.EndRow, right.EndRow) &&
		optionalBytesEqual(left.PrevEndRow, right.PrevEndRow)
}

func optionalBytesEqual(left, right []byte) bool {
	return (left == nil) == (right == nil) && bytes.Equal(left, right)
}

func thriftRangeEqual(left, right *data.TRange) bool {
	if left == nil || right == nil ||
		left.StartKeyInclusive != right.StartKeyInclusive ||
		left.StopKeyInclusive != right.StopKeyInclusive ||
		left.InfiniteStartKey != right.InfiniteStartKey ||
		left.InfiniteStopKey != right.InfiniteStopKey {
		return false
	}
	return thriftKeyEqual(left.Start, right.Start) && thriftKeyEqual(left.Stop, right.Stop)
}

func thriftKeyEqual(left, right *data.TKey) bool {
	if left == nil || right == nil {
		return left == right
	}
	return bytes.Equal(left.Row, right.Row) &&
		bytes.Equal(left.ColFamily, right.ColFamily) &&
		bytes.Equal(left.ColQualifier, right.ColQualifier) &&
		bytes.Equal(left.ColVisibility, right.ColVisibility) &&
		left.Timestamp == right.Timestamp
}

func tabletExtentKey(tablet Tablet) tabletExtentMapKey {
	return tabletExtentMapKey{
		tableID:    tablet.Extent.TableID,
		prevRow:    string(tablet.Extent.PrevRow),
		prevRowSet: tablet.Extent.PrevRow != nil,
		endRow:     string(tablet.Extent.EndRow),
		endRowSet:  tablet.Extent.EndRow != nil,
	}
}

func clipRangeToTablet(scanRange *Range, tablet Tablet) (*Range, bool) {
	if scanRange.fitsTablet(tablet) {
		return cloneRange(scanRange), true
	}
	return &Range{
		startKey:       cloneKey(scanRange.startKey),
		endKey:         keyForRow(tablet.Extent.EndRow),
		startRowOnly:   scanRange.startRowOnly,
		endRowOnly:     true,
		startInclusive: scanRange.startInclusive,
		endInclusive:   true,
	}, false
}

func cloneRange(scanRange *Range) *Range {
	return &Range{
		startKey:       cloneKey(scanRange.startKey),
		endKey:         cloneKey(scanRange.endKey),
		startRowOnly:   scanRange.startRowOnly,
		endRowOnly:     scanRange.endRowOnly,
		startInclusive: scanRange.startInclusive,
		endInclusive:   scanRange.endInclusive,
	}
}

func onlyCleanupErrors(err error) bool {
	if err == nil {
		return true
	}
	if _, ok := err.(*CleanupError); ok {
		return true
	}
	joined, ok := err.(interface{ Unwrap() []error })
	if !ok {
		return false
	}
	children := joined.Unwrap()
	if len(children) == 0 {
		return false
	}
	for _, child := range children {
		if !onlyCleanupErrors(child) {
			return false
		}
	}
	return true
}
