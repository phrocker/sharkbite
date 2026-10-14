package scanserver

import (
	"container/heap"
	"context"
	"fmt"
	"sort"

	"github.com/phrocker/shoal-oss/internal/metadata"
	"github.com/phrocker/shoal-oss/internal/rfile/wire"
	"github.com/phrocker/shoal-oss/internal/thrift/gen/data"
	"github.com/phrocker/shoal-oss/internal/visfilter"
)

// scanTabletRanges drives the heap-merge engine across one tablet's
// RFiles for an arbitrary list of ranges. Caller hands in:
//
//   - files: the tablet's RFile entries (post-locator-lookup).
//   - ranges: requested ranges within the tablet. May be unsorted /
//     overlapping; we sort + merge defensively.
//   - columns: requested CFs (for LG/CF pushdown).
//   - ev: per-scan visibility evaluator.
//   - budgetBytes: soft cap on aggregate KV bytes; truncates with a
//     more=true return once exceeded.
//
// Returns (results, bytesUsed, truncated, error). truncated=true means
// the scan stopped because budget was exhausted, not because all data
// was consumed.
//
// Used by both StartScan (with one range) and StartMultiScan (with
// per-tablet range lists from the ScanBatch).
func (s *Server) scanTabletRanges(
	ctx context.Context,
	files []metadata.FileEntry,
	ranges []*data.TRange,
	columns []*data.TColumn,
	ev *visfilter.Evaluator,
	budgetBytes int,
	postProc cellPostProcessor,
) (results []*data.TKeyValue, bytesUsed int, truncated bool, err error) {
	if budgetBytes <= 0 || len(ranges) == 0 {
		return nil, 0, false, nil
	}

	merged := normalizeRanges(ranges)
	if len(merged) == 0 {
		return nil, 0, false, nil
	}

	wantedCFs := wantedCFsFromColumns(columns)
	columnMatcher := buildColumnMatcher(columns)

	// Open every file → one fileIter per LG (with CF pushdown). We open
	// without seeking; per-range Seek lands them at each range's start.
	iters := make([]*fileIter, 0, len(files))
	for _, f := range files {
		fits, oerr := s.openFileIters(ctx, f.Path, ev, nil, false, wantedCFs)
		if oerr != nil {
			for _, opened := range iters {
				opened.close()
			}
			return nil, 0, false, fmt.Errorf("open %q: %w", f.Path, oerr)
		}
		iters = append(iters, fits...)
	}
	defer func() {
		for _, it := range iters {
			it.close()
		}
	}()

	results = make([]*data.TKeyValue, 0, 64)
	approxBytes := 0
	var lastEmitted *wire.Key

	for _, r := range merged {
		startKey, _, hasStart := lowerBound(r)
		stopKey, stopInclusive, hasStop := upperBound(r)

		// Seek each iter to this range's start. Then rebuild the heap
		// over iters that landed on a cell.
		h := &iterHeap{}
		for _, it := range iters {
			if hasStart {
				if serr := it.seek(startKey); serr != nil {
					continue
				}
			}
			if pk, _ := it.peek(); pk != nil {
				heap.Push(h, it)
			}
		}

		for h.Len() > 0 {
			it := heap.Pop(h).(*fileIter)
			k, v := it.peek()

			if hasStop && pastUpperBound(k, stopKey, stopInclusive) {
				// Don't push back — this iter is past the range stop. It
				// stays out of the heap for the rest of THIS range, but
				// the next range will Seek it again.
				continue
			}

			if columnMatcher != nil && !columnMatcher.match(k.ColumnFamily, k.ColumnQualifier) {
				it.advance()
				if pk, _ := it.peek(); pk != nil {
					heap.Push(h, it)
				}
				continue
			}

			if lastEmitted != nil && sameCoord(k, lastEmitted) {
				it.advance()
				if pk, _ := it.peek(); pk != nil {
					heap.Push(h, it)
				}
				continue
			}

			if k.Deleted {
				lastEmitted = k.Clone()
				it.advance()
				if pk, _ := it.peek(); pk != nil {
					heap.Push(h, it)
				}
				continue
			}

			if postProc != nil {
				// Iterator-driven path. Feed every visible cell to the
				// post-processor; final output comes from drain() after
				// the scan completes. The byte budget is irrelevant
				// here because the iterator emits a bounded result
				// (top-K) regardless of input size.
				postProc.offer(k, v)
				if err := postProc.err(); err != nil {
					return nil, 0, false, err
				}
			} else {
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
			}
			lastEmitted = k.Clone()

			it.advance()
			if pk, _ := it.peek(); pk != nil {
				heap.Push(h, it)
			}

			// Only the streaming path is byte-bounded. The iterator
			// path can scan arbitrary inputs (top-K is bounded by
			// construction).
			if postProc == nil && approxBytes >= budgetBytes {
				return results, approxBytes, true, nil
			}
		}
	}

	if postProc != nil {
		drained := postProc.drain()
		if err := postProc.err(); err != nil {
			return nil, 0, false, err
		}
		// Recompute byte count for the iterator's output so callers
		// can still report something useful in the More=truncated
		// telemetry. Almost always under budget (top-K × tiny score
		// values), but worth tracking.
		bytes := 0
		for _, kv := range drained {
			bytes += approxKVSize(kv)
		}
		return drained, bytes, false, nil
	}
	return results, approxBytes, false, nil
}

// seek positions the underlying reader at the smallest key >= target,
// then loads the cell into curK/curV. Marks done on error or EOF.
func (it *fileIter) seek(target *wire.Key) error {
	if it.done {
		return nil
	}
	if err := it.rdr.Seek(target); err != nil {
		it.done = true
		it.curK, it.curV = nil, nil
		return err
	}
	// rdr.Seek lands without consuming; advance loads the first cell.
	it.done = false
	it.advance()
	return nil
}

// normalizeRanges sorts the input ranges by start key and merges any
// that overlap or touch. Returns a fresh slice (input is never mutated).
func normalizeRanges(in []*data.TRange) []*data.TRange {
	if len(in) == 0 {
		return nil
	}
	out := make([]*data.TRange, 0, len(in))
	for _, r := range in {
		if r != nil {
			out = append(out, r)
		}
	}
	if len(out) <= 1 {
		return out
	}
	sort.SliceStable(out, func(i, j int) bool { return rangeLess(out[i], out[j]) })

	merged := out[:1]
	for _, r := range out[1:] {
		last := merged[len(merged)-1]
		if rangesOverlap(last, r) {
			merged[len(merged)-1] = mergeRange(last, r)
		} else {
			merged = append(merged, r)
		}
	}
	return merged
}

// rangeLess orders ranges by start key. Infinite-start sorts first.
// Equal starts sort inclusive before exclusive (matching Accumulo's
// comparator semantics).
func rangeLess(a, b *data.TRange) bool {
	aInf := a.InfiniteStartKey || a.Start == nil
	bInf := b.InfiniteStartKey || b.Start == nil
	if aInf != bInf {
		return aInf
	}
	if aInf {
		return false
	}
	c := tkeyToWireKey(a.Start).Compare(tkeyToWireKey(b.Start))
	if c != 0 {
		return c < 0
	}
	if a.StartKeyInclusive != b.StartKeyInclusive {
		return a.StartKeyInclusive && !b.StartKeyInclusive
	}
	return false
}

// rangesOverlap: a is sorted to come no later than b. Returns true iff
// their key intervals intersect or touch (so merging is safe).
func rangesOverlap(a, b *data.TRange) bool {
	aStopInf := a.InfiniteStopKey || a.Stop == nil
	if aStopInf {
		return true
	}
	bStartInf := b.InfiniteStartKey || b.Start == nil
	if bStartInf {
		return true
	}
	cmp := tkeyToWireKey(a.Stop).Compare(tkeyToWireKey(b.Start))
	if cmp < 0 {
		return false
	}
	if cmp > 0 {
		return true
	}
	// Equal endpoints: overlap iff at least one boundary is inclusive.
	return a.StopKeyInclusive || b.StartKeyInclusive
}

// mergeRange: precondition = rangesOverlap(a, b) and a is sorted no
// later than b. Returns a TRange covering both.
func mergeRange(a, b *data.TRange) *data.TRange {
	out := &data.TRange{
		Start:             a.Start,
		StartKeyInclusive: a.StartKeyInclusive,
		InfiniteStartKey:  a.InfiniteStartKey,
	}
	aStopInf := a.InfiniteStopKey || a.Stop == nil
	bStopInf := b.InfiniteStopKey || b.Stop == nil
	if aStopInf || bStopInf {
		out.InfiniteStopKey = true
		out.Stop = nil
		return out
	}
	ak, bk := tkeyToWireKey(a.Stop), tkeyToWireKey(b.Stop)
	c := ak.Compare(bk)
	switch {
	case c < 0:
		out.Stop = b.Stop
		out.StopKeyInclusive = b.StopKeyInclusive
	case c > 0:
		out.Stop = a.Stop
		out.StopKeyInclusive = a.StopKeyInclusive
	default:
		out.Stop = a.Stop
		out.StopKeyInclusive = a.StopKeyInclusive || b.StopKeyInclusive
	}
	return out
}

func wantedCFsFromColumns(columns []*data.TColumn) map[string]struct{} {
	out := make(map[string]struct{}, len(columns))
	for _, c := range columns {
		if c != nil && len(c.ColumnFamily) > 0 {
			out[string(c.ColumnFamily)] = struct{}{}
		}
	}
	return out
}
