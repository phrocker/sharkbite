package accumulo

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/phrocker/shoal-oss/internal/scanclient"
	"github.com/phrocker/shoal-oss/internal/thrift/gen/data"
)

// ErrStreamClosed reports iteration on a result stream after Close.
var ErrStreamClosed = errors.New("accumulo: result stream is closed")

// errSourcesReplanned reports that a source split its work into new sources
// instead of opening a session, so the stream must consult its queue again.
var errSourcesReplanned = errors.New("accumulo: scan sources replanned")

// maxSegmentReplans bounds how many times one planned segment may be re-split
// after a stale tablet assignment before the stream gives up.
const maxSegmentReplans = 3

// ResultStream is a forward-only cursor over scan results.
//
// A stream holds at most one server batch at a time, so its memory use is
// bounded by ScannerOptions.BatchSize rather than by the size of the scan.
// Entries arrive in the order the server returns them: for Scanner.Stream that
// is key order within the tablet, and for BatchScanner.Stream it is input-range
// order and then tablet order, unless ScannerOptions.UseMultiScan is set, in
// which case order within a server group is server-defined.
//
// The zero value is not usable; obtain a stream from Scanner.Stream or
// BatchScanner.Stream. A stream is not safe for concurrent iteration, but Close
// may be called from another goroutine while Next is blocked: it cancels the
// in-flight RPC first and then releases the server-side scan session.
type ResultStream struct {
	scanner *Scanner
	table   Table

	ctx    context.Context
	cancel context.CancelFunc

	mu           sync.Mutex
	sources      []streamSource
	next         int
	session      streamSession
	pending      []KeyValue
	consumed     int
	pendingFatal error
	current      KeyValue
	fatal        error
	cleanup      error
	closed       bool
	done         bool
}

// streamSource opens one server-side scan session.
type streamSource interface {
	open(ctx context.Context, stream *ResultStream) (streamSession, error)
}

// streamSession pulls batches from one open server-side scan session.
type streamSession interface {
	// fetch returns the next batch, whether the session has further batches,
	// and a terminal failure. Entries and a failure may be returned together,
	// in which case the entries are delivered before the failure surfaces.
	fetch(ctx context.Context) ([]KeyValue, bool, error)
	// followers returns sessions discovered while draining this one, such as
	// the tablet ranges a multi-scan reported as failed.
	followers(ctx context.Context) ([]streamSource, error)
	// close releases the server-side session, reporting a *CleanupError when
	// results already handed out remain usable.
	close(ctx context.Context) error
}

// Stream opens a cursor over a range that must fit within one tablet. The
// context governs every RPC the stream makes; cancelling it stops iteration,
// and Close still releases the server-side session.
func (s *Scanner) Stream(ctx context.Context, scanRange *Range) (*ResultStream, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if scanRange == nil {
		return nil, errors.New("accumulo: scan range is nil")
	}
	table, err := s.resolveTable(ctx)
	if err != nil {
		return nil, err
	}
	routingRow := scanRange.routingRow()
	tablet, err := s.locateTablet(ctx, table, routingRow)
	if err != nil {
		return nil, err
	}
	if !scanRange.fitsTablet(tablet) {
		return nil, fmt.Errorf(
			"%w: table=%s start=%q end=%q tabletEnd=%q",
			ErrRangeSpansTablets,
			table.ID,
			scanRange.StartRow(),
			scanRange.EndRow(),
			tablet.Extent.EndRow,
		)
	}
	source := tabletSource{segment: batchScanSegment{
		routingRow: routingRow,
		tablet:     tablet,
		scanRange:  cloneRange(scanRange),
	}}
	return newResultStream(ctx, s, table, []streamSource{source}), nil
}

// Stream opens a cursor over ranges after splitting them at tablet boundaries.
// Tablets are read one at a time so memory stays bounded;
// ScannerOptions.Parallelism bounds concurrent tablet scans in Scan and is not
// used here.
func (s *BatchScanner) Stream(ctx context.Context, ranges []*Range) (*ResultStream, error) {
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

	var sources []streamSource
	if scanner.options.UseMultiScan {
		multi, ok := scanner.connector.scan.(scanclient.MultiLifecycle)
		if !ok {
			return nil, errors.New("accumulo: scan adapter does not support multi-scan")
		}
		for _, group := range groupBatchSegments(segments) {
			sources = append(sources, multiSource{multi: multi, group: group})
		}
	} else {
		for _, segment := range segments {
			sources = append(sources, tabletSource{segment: segment, replannable: true})
		}
	}
	return newResultStream(ctx, &scanner, table, sources), nil
}

func newResultStream(
	ctx context.Context,
	scanner *Scanner,
	table Table,
	sources []streamSource,
) *ResultStream {
	streamCtx, cancel := context.WithCancel(ctx)
	return &ResultStream{
		scanner: scanner,
		table:   table,
		ctx:     streamCtx,
		cancel:  cancel,
		sources: sources,
	}
}

// Next advances the cursor and reports whether Entry holds a new result. It
// returns false at the end of the scan and on the first failure; call Err to
// tell those apart.
func (r *ResultStream) Next() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.consumed < len(r.pending) {
		r.current = r.pending[r.consumed]
		r.consumed++
		return true
	}
	r.pending = nil
	r.consumed = 0
	if r.pendingFatal != nil {
		r.recordClose()
		r.fatal = r.pendingFatal
		r.pendingFatal = nil
		return false
	}
	if r.closed {
		if r.fatal == nil && !r.done {
			r.fatal = ErrStreamClosed
		}
		return false
	}
	if r.done || r.fatal != nil {
		return false
	}
	for {
		if err := r.ctx.Err(); err != nil {
			r.recordClose()
			r.fatal = err
			return false
		}
		if r.session == nil {
			if !r.openNextSession() {
				return false
			}
			if r.session == nil {
				// The source replanned itself into new sources.
				continue
			}
		}
		entries, more, err := r.session.fetch(r.ctx)
		if len(entries) > 0 {
			r.pending = entries
			r.consumed = 1
			r.current = entries[0]
			switch {
			case err != nil:
				r.pendingFatal = err
			case !more:
				if !r.finishSession() {
					r.pendingFatal = r.fatal
					r.fatal = nil
				}
			}
			return true
		}
		if err != nil {
			r.recordClose()
			r.fatal = err
			return false
		}
		if more {
			continue
		}
		if !r.finishSession() {
			return false
		}
	}
}

// openNextSession opens the next queued source and reports whether iteration
// may continue. It sets done when no sources remain, and leaves session nil
// when a source replanned itself instead of opening.
func (r *ResultStream) openNextSession() bool {
	if r.next >= len(r.sources) {
		r.done = true
		return false
	}
	source := r.sources[r.next]
	r.next++
	session, err := source.open(r.ctx, r)
	if errors.Is(err, errSourcesReplanned) {
		return true
	}
	if err != nil {
		r.fatal = err
		return false
	}
	r.session = session
	return true
}

// prependSources queues sources ahead of the ones still waiting.
func (r *ResultStream) prependSources(sources []streamSource) {
	remaining := make([]streamSource, 0, len(sources)+len(r.sources)-r.next)
	remaining = append(remaining, sources...)
	remaining = append(remaining, r.sources[r.next:]...)
	r.sources = remaining
	r.next = 0
}

// finishSession drains the exhausted session, queues any follow-up sources it
// discovered, and reports whether iteration may continue.
func (r *ResultStream) finishSession() bool {
	session := r.session
	if session == nil {
		return true
	}
	followers, err := session.followers(r.ctx)
	r.recordClose()
	if err != nil {
		r.fatal = err
		return false
	}
	if len(followers) > 0 {
		r.prependSources(followers)
	}
	return true
}

// recordClose releases the current session and accumulates any close failure in
// cleanup, which both Err and Close report. Iteration failures stay in fatal so
// the two are never confused.
func (r *ResultStream) recordClose() {
	if closeErr := r.closeSession(); closeErr != nil {
		r.cleanup = errors.Join(r.cleanup, closeErr)
	}
}

// closeSession releases the current server session and clears it. The close
// RPC survives cancellation of the stream context so sessions do not leak.
func (r *ResultStream) closeSession() error {
	if r.session == nil {
		return nil
	}
	session := r.session
	r.session = nil
	closeCtx, cancel := context.WithTimeout(
		context.WithoutCancel(r.ctx),
		scannerCloseTimeout,
	)
	defer cancel()
	return session.close(closeCtx)
}

// Entry returns the result the last successful Next produced. Its slices are
// copies the caller owns.
func (r *ResultStream) Entry() KeyValue {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.current
}

// Err returns the failure that stopped iteration, joined with any scan-session
// cleanup failures. It is nil after a stream is drained cleanly.
func (r *ResultStream) Err() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	return errors.Join(r.fatal, r.cleanup)
}

// Close releases the server-side scan session. It is idempotent and safe to
// call while Next is blocked: the in-flight RPC is cancelled first. Close
// returns the accumulated cleanup failures, and a stream closed before it
// drained reports ErrStreamClosed from Err.
func (r *ResultStream) Close() error {
	r.cancel()
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return r.cleanup
	}
	r.closed = true
	r.recordClose()
	r.pending = nil
	r.consumed = 0
	if r.pendingFatal != nil {
		r.fatal = r.pendingFatal
		r.pendingFatal = nil
	}
	if r.fatal == nil && !r.done {
		r.fatal = ErrStreamClosed
	}
	return r.cleanup
}

// tabletSource opens a single-tablet scan session, retrying a stale tablet
// assignment once, exactly as Scanner.Scan does. The retry happens before any
// entry of the segment is delivered, so no row is repeated, and the relocated
// tablet is rechecked against the range: if the tablet split, a batch-planned
// segment is replanned across the new extents and a single-tablet stream
// reports ErrRangeSpansTablets rather than scanning part of the range.
type tabletSource struct {
	segment batchScanSegment
	// replannable marks segments that came from planBatchScan and may be split
	// again when their tablet changes underneath them.
	replannable bool
	replans     int
}

func (t tabletSource) open(ctx context.Context, stream *ResultStream) (streamSession, error) {
	scanner := stream.scanner
	tablet := t.segment.tablet
	var priorErr error
	for attempt := 0; attempt < 2; attempt++ {
		if tablet.Server == nil || tablet.Server.HostPort == "" {
			return nil, errors.Join(priorErr, fmt.Errorf(
				"%w: table=%s",
				ErrTabletNotLocated,
				tablet.Extent.TableID,
			))
		}
		if !t.segment.scanRange.fitsTablet(tablet) {
			return t.handleSplitTablet(ctx, stream, tablet, priorErr)
		}
		session, err := startTabletSession(ctx, scanner, tablet, t.segment.scanRange)
		if err == nil {
			return session, nil
		}
		if attempt == 1 || !isStaleScanError(err) {
			return nil, errors.Join(priorErr, err)
		}
		priorErr = err
		if invalidateErr := scanner.connector.InvalidateTablet(
			stream.table,
			t.segment.routingRow,
		); invalidateErr != nil {
			return nil, errors.Join(priorErr, invalidateErr)
		}
		located, locateErr := scanner.locateTablet(ctx, stream.table, t.segment.routingRow)
		if locateErr != nil {
			return nil, errors.Join(priorErr, locateErr)
		}
		tablet = located
	}
	return nil, errors.Join(priorErr, ErrTabletNotLocated)
}

// handleSplitTablet reacts to a relocated tablet that no longer covers the
// segment. Batch-planned segments are re-split across the new extents; a
// single-tablet stream cannot silently read part of its range.
func (t tabletSource) handleSplitTablet(
	ctx context.Context,
	stream *ResultStream,
	tablet Tablet,
	priorErr error,
) (streamSession, error) {
	spanErr := fmt.Errorf(
		"%w: table=%s start=%q end=%q tabletEnd=%q",
		ErrRangeSpansTablets,
		stream.table.ID,
		t.segment.scanRange.StartRow(),
		t.segment.scanRange.EndRow(),
		tablet.Extent.EndRow,
	)
	if !t.replannable || t.replans >= maxSegmentReplans {
		return nil, errors.Join(priorErr, spanErr)
	}
	replanned, err := stream.scanner.planBatchScan(
		ctx,
		stream.table,
		[]*Range{t.segment.scanRange},
	)
	if err != nil {
		return nil, errors.Join(priorErr, err)
	}
	sources := make([]streamSource, 0, len(replanned))
	for _, segment := range replanned {
		sources = append(sources, tabletSource{
			segment:     segment,
			replannable: true,
			replans:     t.replans + 1,
		})
	}
	stream.prependSources(sources)
	return nil, errSourcesReplanned
}

func startTabletSession(
	ctx context.Context,
	scanner *Scanner,
	tablet Tablet,
	scanRange *Range,
) (*tabletSession, error) {
	batchSize := scanner.options.BatchSize
	if batchSize == 0 {
		batchSize = defaultScannerBatchSize
	}
	iterators, iteratorOptions := iteratorsToThrift(scanner.options.Iterators)
	initial, err := scanner.connector.scan.Start(
		ctx,
		tablet.Server.HostPort,
		scanclient.StartRequest{
			Extent:             tabletExtentToThrift(tablet),
			Range:              scanRange.toThrift(),
			Columns:            columnsToThrift(scanner.options.Columns),
			BatchSize:          batchSize,
			Iterators:          iterators,
			IteratorOptions:    iteratorOptions,
			Authorizations:     cloneByteSlices(scanner.options.Authorizations),
			ReadaheadThreshold: int64(batchSize),
		},
	)
	if initial == nil {
		if err == nil {
			err = errors.New("accumulo: start scan returned no result")
		}
		return nil, err
	}
	return &tabletSession{
		scanner:  scanner,
		address:  tablet.Server.HostPort,
		scanID:   initial.ScanID,
		initial:  initial.Result_,
		startErr: err,
	}, nil
}

type tabletSession struct {
	scanner *Scanner
	address string
	scanID  data.ScanID

	initial  *data.ScanResult_
	startErr error
	started  bool
	closed   bool
}

func (t *tabletSession) fetch(ctx context.Context) ([]KeyValue, bool, error) {
	var result *data.ScanResult_
	var rpcErr error
	if !t.started {
		t.started = true
		result, rpcErr = t.initial, t.startErr
		t.initial, t.startErr = nil, nil
	} else {
		// Continue may return a usable batch together with an error, for
		// example when the pooled client fails to release its transport lease
		// after a successful RPC. Decode the batch either way so the entries
		// reach the caller before the failure does.
		result, rpcErr = t.scanner.connector.scan.Continue(ctx, t.address, t.scanID, 0)
	}
	entries, appendErr := appendScanResult(nil, result)
	if err := errors.Join(rpcErr, appendErr); err != nil {
		return entries, false, err
	}
	more := result != nil && result.More
	if more && t.scanID == 0 {
		return entries, false, errors.New("accumulo: continuing scan has zero scan ID")
	}
	return entries, more, nil
}

func (t *tabletSession) followers(context.Context) ([]streamSource, error) { return nil, nil }

func (t *tabletSession) close(ctx context.Context) error {
	if t.closed || t.scanID == 0 {
		t.closed = true
		return nil
	}
	t.closed = true
	if err := t.scanner.connector.scan.CloseScan(ctx, t.address, t.scanID); err != nil {
		return &CleanupError{ScanID: int64(t.scanID), Err: err}
	}
	return nil
}

// multiSource opens one multi-scan session for the ranges a single tablet
// server hosts.
type multiSource struct {
	multi scanclient.MultiLifecycle
	group batchMultiScanGroup
}

func (m multiSource) open(ctx context.Context, stream *ResultStream) (streamSession, error) {
	scanner := stream.scanner
	iterators, iteratorOptions := iteratorsToThrift(scanner.options.Iterators)
	initial, err := m.multi.StartMulti(ctx, m.group.address, scanclient.MultiStartRequest{
		Batch:           m.group.batch,
		Columns:         columnsToThrift(scanner.options.Columns),
		Iterators:       iterators,
		IteratorOptions: iteratorOptions,
		Authorizations:  cloneByteSlices(scanner.options.Authorizations),
	})
	if initial == nil {
		if err == nil {
			err = errors.New("accumulo: start multi-scan returned no result")
		}
		return nil, err
	}
	return &multiSession{
		stream:   stream,
		multi:    m.multi,
		group:    m.group,
		address:  m.group.address,
		scanID:   initial.ScanID,
		initial:  initial.Result_,
		startErr: err,
	}, nil
}

type multiSession struct {
	stream  *ResultStream
	multi   scanclient.MultiLifecycle
	group   batchMultiScanGroup
	address string
	scanID  data.ScanID

	initial  *data.MultiScanResult_
	startErr error
	started  bool
	failures data.ScanBatch
	closed   bool
}

func (m *multiSession) fetch(ctx context.Context) ([]KeyValue, bool, error) {
	var result *data.MultiScanResult_
	var rpcErr error
	if !m.started {
		m.started = true
		result, rpcErr = m.initial, m.startErr
		m.initial, m.startErr = nil, nil
	} else {
		// ContinueMulti has the same partial-success shape as Continue.
		result, rpcErr = m.multi.ContinueMulti(ctx, m.address, m.scanID, 0)
	}
	var entries []KeyValue
	var appendErr error
	if result != nil {
		entries, appendErr = appendKeyValues(nil, result.Results)
	}
	if err := errors.Join(rpcErr, appendErr); err != nil {
		return entries, false, err
	}
	if result == nil {
		return nil, false, nil
	}
	m.failures = appendScanBatch(m.failures, result.Failures)
	if result.More && m.scanID == 0 {
		return entries, false, errors.New("accumulo: continuing multi-scan has zero scan ID")
	}
	if !result.More && result.PartScan != nil {
		return entries, false, errors.New(
			"accumulo: multi-scan ended with an incomplete tablet range",
		)
	}
	return entries, result.More, nil
}

// followers replans the ranges the server reported as failed so the stream
// reads them from their new tablet before moving on.
func (m *multiSession) followers(ctx context.Context) ([]streamSource, error) {
	if len(m.failures) == 0 {
		return nil, nil
	}
	failures := m.failures
	m.failures = nil
	failed, err := matchFailedSegments(m.group.segments, failures)
	if err != nil {
		return nil, err
	}
	scanner := m.stream.scanner
	var sources []streamSource
	for _, segment := range failed {
		if err := scanner.connector.InvalidateTablet(
			m.stream.table,
			segment.routingRow,
		); err != nil {
			return nil, err
		}
		replanned, err := scanner.planBatchScan(ctx, m.stream.table, []*Range{segment.scanRange})
		if err != nil {
			return nil, err
		}
		for _, retry := range replanned {
			sources = append(sources, tabletSource{segment: retry, replannable: true})
		}
	}
	return sources, nil
}

func (m *multiSession) close(ctx context.Context) error {
	if m.closed || m.scanID == 0 {
		m.closed = true
		return nil
	}
	m.closed = true
	if err := m.multi.CloseMultiScan(ctx, m.address, m.scanID); err != nil {
		return &CleanupError{ScanID: int64(m.scanID), Err: err}
	}
	return nil
}
