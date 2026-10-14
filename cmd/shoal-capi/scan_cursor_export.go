package main

/*
#cgo CFLAGS: -I${SRCDIR}/../../capi/include
#include "bridge.h"
*/
import "C"

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"unsafe"

	"github.com/phrocker/shoal-oss/accumulo"
)

type scanCursorSource interface {
	Next() bool
	Entry() accumulo.KeyValue
	Err() error
	Close() error
}

type ownedScanCursor struct {
	source scanCursorSource
	done   func()

	nextMu  sync.Mutex
	mu      sync.Mutex
	closed  bool
	ended   bool
	stopErr error
	active  int
	idle    chan struct{}

	interruptOnce sync.Once
	interruptErr  error
	completeOnce  sync.Once
	stopped       chan struct{}
}

func newOwnedScanCursor(
	ctx context.Context,
	source scanCursorSource,
	done func(),
) *ownedScanCursor {
	idle := make(chan struct{})
	close(idle)
	cursor := &ownedScanCursor{
		source:  source,
		done:    done,
		idle:    idle,
		stopped: make(chan struct{}),
	}
	go func() {
		select {
		case <-ctx.Done():
			cursor.abort(ctx.Err())
		case <-cursor.stopped:
		}
	}()
	return cursor
}

func (c *ownedScanCursor) interrupt() error {
	c.interruptOnce.Do(func() {
		c.interruptErr = c.source.Close()
	})
	return c.interruptErr
}

func (c *ownedScanCursor) complete() {
	c.completeOnce.Do(func() {
		c.done()
		close(c.stopped)
	})
}

func (c *ownedScanCursor) abort(err error) {
	c.mu.Lock()
	if c.closed || c.ended {
		c.mu.Unlock()
		return
	}
	c.closed = true
	c.stopErr = err
	idle := c.idle
	c.mu.Unlock()
	_ = c.interrupt()
	<-idle
	c.complete()
}

func (c *ownedScanCursor) close() error {
	c.mu.Lock()
	c.closed = true
	idle := c.idle
	c.mu.Unlock()
	err := c.interrupt()
	<-idle
	c.complete()
	return err
}

func (c *ownedScanCursor) requestClose() {
	c.mu.Lock()
	c.closed = true
	complete := c.active == 0
	c.mu.Unlock()
	_ = c.interrupt()
	if complete {
		c.complete()
	}
}

func (c *ownedScanCursor) beginPull() (bool, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		if c.stopErr != nil {
			return false, c.stopErr
		}
		return false, accumulo.ErrStreamClosed
	}
	if c.ended {
		return true, nil
	}
	if c.active == 0 {
		c.idle = make(chan struct{})
	}
	c.active++
	return false, nil
}

func (c *ownedScanCursor) endPull() {
	c.mu.Lock()
	c.active--
	if c.active == 0 {
		close(c.idle)
	}
	complete := c.active == 0 && (c.closed || c.ended)
	c.mu.Unlock()
	if complete {
		c.complete()
	}
}

func (c *ownedScanCursor) next(maxEntries int) ([]accumulo.KeyValue, bool, error) {
	c.nextMu.Lock()
	defer c.nextMu.Unlock()

	c.mu.Lock()
	if c.closed {
		err := c.stopErr
		c.mu.Unlock()
		if err == nil {
			err = accumulo.ErrStreamClosed
		}
		return nil, false, err
	}
	if c.ended {
		c.mu.Unlock()
		return nil, true, nil
	}
	c.mu.Unlock()

	values := make([]accumulo.KeyValue, 0)
	for len(values) < maxEntries {
		if c.source.Next() {
			values = append(values, c.source.Entry())
			continue
		}
		err := c.source.Err()
		c.mu.Lock()
		c.ended = true
		complete := c.active == 0
		c.mu.Unlock()
		closeErr := c.interrupt()
		if complete {
			c.complete()
		}
		return values, true, errors.Join(err, closeErr)
	}
	return values, false, nil
}

func registerScanCursor(
	ctx context.Context,
	source scanCursorSource,
	done func(),
	outCursor **C.shoal_scan_cursor,
	outError **C.shoal_error,
) C.shoal_status {
	cursor := newOwnedScanCursor(ctx, source, done)
	id, ok := scanCursors.add(cursor)
	if !ok {
		_ = cursor.close()
		return fail(outError, C.SHOAL_STATUS_INTERNAL, errors.New("shoal: scan cursor handle space exhausted"))
	}
	handle := C.shoal_bridge_scan_cursor_alloc(C.uint64_t(id))
	if handle == nil {
		scanCursors.remove(id)
		_ = cursor.close()
		return fail(outError, C.SHOAL_STATUS_OUT_OF_MEMORY, errors.New("shoal: allocate scan cursor handle"))
	}
	*outCursor = handle
	return C.SHOAL_STATUS_OK
}

func lookupScanCursor(handle *C.shoal_scan_cursor) (*ownedScanCursor, error) {
	if handle == nil {
		return nil, errors.New("shoal: scan cursor handle is NULL")
	}
	id := uint64(C.shoal_bridge_scan_cursor_id(handle))
	cursor, ok := scanCursors.get(id)
	if !ok {
		return nil, errors.New("shoal: scan cursor handle is unknown or freed")
	}
	return cursor, nil
}

//export shoal_scanner_stream
func shoal_scanner_stream(
	handle *C.shoal_scanner,
	cRange *C.shoal_range,
	timeoutMilliseconds C.int64_t,
	outCursor **C.shoal_scan_cursor,
	outError **C.shoal_error,
) C.shoal_status {
	return scannerStream(handle, cRange, timeoutMilliseconds, nil, false, outCursor, outError)
}

//export shoal_scanner_stream_with_cancellation
func shoal_scanner_stream_with_cancellation(
	handle *C.shoal_scanner,
	cRange *C.shoal_range,
	timeoutMilliseconds C.int64_t,
	cancellationHandle *C.shoal_cancellation,
	outCursor **C.shoal_scan_cursor,
	outError **C.shoal_error,
) C.shoal_status {
	return scannerStream(handle, cRange, timeoutMilliseconds, cancellationHandle, true, outCursor, outError)
}

func scannerStream(
	handle *C.shoal_scanner,
	cRange *C.shoal_range,
	timeoutMilliseconds C.int64_t,
	cancellationHandle *C.shoal_cancellation,
	requireCancellation bool,
	outCursor **C.shoal_scan_cursor,
	outError **C.shoal_error,
) (status C.shoal_status) {
	clearCursorOutputs(outCursor, outError)
	defer recoverCursorStatus(&status, outCursor, outError)
	if outCursor == nil {
		return fail(outError, C.SHOAL_STATUS_INVALID_ARGUMENT, errors.New("shoal: out_cursor is required"))
	}
	scanner, err := lookupScanner(handle)
	if err != nil {
		return fail(outError, C.SHOAL_STATUS_INVALID_HANDLE, err)
	}
	scanRange, err := parseRange(cRange)
	if err != nil {
		return fail(outError, C.SHOAL_STATUS_INVALID_ARGUMENT, err)
	}
	timeout, err := operationTimeout(timeoutMilliseconds)
	if err != nil {
		return fail(outError, C.SHOAL_STATUS_INVALID_ARGUMENT, err)
	}
	var cancellation *ownedCancellation
	if requireCancellation {
		cancellation, err = lookupCancellation(cancellationHandle)
		if err != nil {
			return fail(outError, C.SHOAL_STATUS_INVALID_HANDLE, err)
		}
	}
	ctx, done, err := scanner.beginStream(timeout, cancellation)
	if err != nil {
		return failForError(outError, err)
	}
	source, err := scanner.streamOne(ctx, scanRange)
	if err != nil {
		done()
		return failForError(outError, err)
	}
	return registerScanCursor(ctx, source, done, outCursor, outError)
}

//export shoal_batch_scanner_stream
func shoal_batch_scanner_stream(
	handle *C.shoal_batch_scanner,
	cRanges *C.shoal_range,
	rangeCount C.size_t,
	timeoutMilliseconds C.int64_t,
	outCursor **C.shoal_scan_cursor,
	outError **C.shoal_error,
) C.shoal_status {
	return batchScannerStream(handle, cRanges, rangeCount, timeoutMilliseconds, nil, false, outCursor, outError)
}

//export shoal_batch_scanner_stream_with_cancellation
func shoal_batch_scanner_stream_with_cancellation(
	handle *C.shoal_batch_scanner,
	cRanges *C.shoal_range,
	rangeCount C.size_t,
	timeoutMilliseconds C.int64_t,
	cancellationHandle *C.shoal_cancellation,
	outCursor **C.shoal_scan_cursor,
	outError **C.shoal_error,
) C.shoal_status {
	return batchScannerStream(handle, cRanges, rangeCount, timeoutMilliseconds, cancellationHandle, true, outCursor, outError)
}

func batchScannerStream(
	handle *C.shoal_batch_scanner,
	cRanges *C.shoal_range,
	rangeCount C.size_t,
	timeoutMilliseconds C.int64_t,
	cancellationHandle *C.shoal_cancellation,
	requireCancellation bool,
	outCursor **C.shoal_scan_cursor,
	outError **C.shoal_error,
) (status C.shoal_status) {
	clearCursorOutputs(outCursor, outError)
	defer recoverCursorStatus(&status, outCursor, outError)
	if outCursor == nil {
		return fail(outError, C.SHOAL_STATUS_INVALID_ARGUMENT, errors.New("shoal: out_cursor is required"))
	}
	scanner, err := lookupBatchScanner(handle)
	if err != nil {
		return fail(outError, C.SHOAL_STATUS_INVALID_HANDLE, err)
	}
	ranges, err := parseCursorRanges(cRanges, rangeCount)
	if err != nil {
		return fail(outError, C.SHOAL_STATUS_INVALID_ARGUMENT, err)
	}
	timeout, err := operationTimeout(timeoutMilliseconds)
	if err != nil {
		return fail(outError, C.SHOAL_STATUS_INVALID_ARGUMENT, err)
	}
	var cancellation *ownedCancellation
	if requireCancellation {
		cancellation, err = lookupCancellation(cancellationHandle)
		if err != nil {
			return fail(outError, C.SHOAL_STATUS_INVALID_HANDLE, err)
		}
	}
	ctx, done, err := scanner.beginStream(timeout, cancellation)
	if err != nil {
		return failForError(outError, err)
	}
	source, err := scanner.streamMany(ctx, ranges)
	if err != nil {
		done()
		return failForError(outError, err)
	}
	return registerScanCursor(ctx, source, done, outCursor, outError)
}

//export shoal_client_stream_range
func shoal_client_stream_range(
	handle *C.shoal_client,
	cRange *C.shoal_range,
	timeoutMilliseconds C.int64_t,
	outCursor **C.shoal_scan_cursor,
	outError **C.shoal_error,
) C.shoal_status {
	return clientStreamRange(handle, cRange, timeoutMilliseconds, nil, false, outCursor, outError)
}

//export shoal_client_stream_range_with_cancellation
func shoal_client_stream_range_with_cancellation(
	handle *C.shoal_client,
	cRange *C.shoal_range,
	timeoutMilliseconds C.int64_t,
	cancellationHandle *C.shoal_cancellation,
	outCursor **C.shoal_scan_cursor,
	outError **C.shoal_error,
) C.shoal_status {
	return clientStreamRange(handle, cRange, timeoutMilliseconds, cancellationHandle, true, outCursor, outError)
}

func clientStreamRange(
	handle *C.shoal_client,
	cRange *C.shoal_range,
	timeoutMilliseconds C.int64_t,
	cancellationHandle *C.shoal_cancellation,
	requireCancellation bool,
	outCursor **C.shoal_scan_cursor,
	outError **C.shoal_error,
) (status C.shoal_status) {
	clearCursorOutputs(outCursor, outError)
	defer recoverCursorStatus(&status, outCursor, outError)
	if outCursor == nil {
		return fail(outError, C.SHOAL_STATUS_INVALID_ARGUMENT, errors.New("shoal: out_cursor is required"))
	}
	client, err := lookupClient(handle)
	if err != nil {
		return fail(outError, C.SHOAL_STATUS_INVALID_HANDLE, err)
	}
	scanRange, err := parseRange(cRange)
	if err != nil {
		return fail(outError, C.SHOAL_STATUS_INVALID_ARGUMENT, err)
	}
	timeout, err := operationTimeout(timeoutMilliseconds)
	if err != nil {
		return fail(outError, C.SHOAL_STATUS_INVALID_ARGUMENT, err)
	}
	cancellation, status := clientScanCancellation(cancellationHandle, requireCancellation, outError)
	if status != C.SHOAL_STATUS_OK {
		return status
	}
	ctx, snapshot, done, err := client.beginSnapshot(true, timeout)
	if err != nil {
		return failForError(outError, err)
	}
	ctx, done, err = attachClientCancellation(ctx, done, cancellation)
	if err != nil {
		return failForError(outError, err)
	}
	source, err := client.streamOne(ctx, snapshot, scanRange)
	if err != nil {
		done()
		return failForError(outError, err)
	}
	return registerScanCursor(ctx, source, done, outCursor, outError)
}

//export shoal_client_stream_ranges
func shoal_client_stream_ranges(
	handle *C.shoal_client,
	cRanges *C.shoal_range,
	rangeCount C.size_t,
	timeoutMilliseconds C.int64_t,
	outCursor **C.shoal_scan_cursor,
	outError **C.shoal_error,
) C.shoal_status {
	return clientStreamRanges(handle, cRanges, rangeCount, timeoutMilliseconds, nil, false, outCursor, outError)
}

//export shoal_client_stream_ranges_with_cancellation
func shoal_client_stream_ranges_with_cancellation(
	handle *C.shoal_client,
	cRanges *C.shoal_range,
	rangeCount C.size_t,
	timeoutMilliseconds C.int64_t,
	cancellationHandle *C.shoal_cancellation,
	outCursor **C.shoal_scan_cursor,
	outError **C.shoal_error,
) C.shoal_status {
	return clientStreamRanges(handle, cRanges, rangeCount, timeoutMilliseconds, cancellationHandle, true, outCursor, outError)
}

func clientStreamRanges(
	handle *C.shoal_client,
	cRanges *C.shoal_range,
	rangeCount C.size_t,
	timeoutMilliseconds C.int64_t,
	cancellationHandle *C.shoal_cancellation,
	requireCancellation bool,
	outCursor **C.shoal_scan_cursor,
	outError **C.shoal_error,
) (status C.shoal_status) {
	clearCursorOutputs(outCursor, outError)
	defer recoverCursorStatus(&status, outCursor, outError)
	if outCursor == nil {
		return fail(outError, C.SHOAL_STATUS_INVALID_ARGUMENT, errors.New("shoal: out_cursor is required"))
	}
	client, err := lookupClient(handle)
	if err != nil {
		return fail(outError, C.SHOAL_STATUS_INVALID_HANDLE, err)
	}
	ranges, err := parseCursorRanges(cRanges, rangeCount)
	if err != nil {
		return fail(outError, C.SHOAL_STATUS_INVALID_ARGUMENT, err)
	}
	timeout, err := operationTimeout(timeoutMilliseconds)
	if err != nil {
		return fail(outError, C.SHOAL_STATUS_INVALID_ARGUMENT, err)
	}
	cancellation, status := clientScanCancellation(cancellationHandle, requireCancellation, outError)
	if status != C.SHOAL_STATUS_OK {
		return status
	}
	ctx, snapshot, done, err := client.beginSnapshot(true, timeout)
	if err != nil {
		return failForError(outError, err)
	}
	ctx, done, err = attachClientCancellation(ctx, done, cancellation)
	if err != nil {
		return failForError(outError, err)
	}
	source, err := client.streamMany(ctx, snapshot, ranges)
	if err != nil {
		done()
		return failForError(outError, err)
	}
	return registerScanCursor(ctx, source, done, outCursor, outError)
}

//export shoal_scan_cursor_next
func shoal_scan_cursor_next(
	handle *C.shoal_scan_cursor,
	maxEntries C.size_t,
	outResult **C.shoal_scan_result,
	outExhausted *C.uint8_t,
	outError **C.shoal_error,
) (status C.shoal_status) {
	clearScanOutputs(outResult, outError)
	if outExhausted != nil {
		*outExhausted = 0
	}
	defer recoverScanStatus(&status, outResult, outError)
	if outResult == nil || outExhausted == nil {
		return fail(outError, C.SHOAL_STATUS_INVALID_ARGUMENT, errors.New("shoal: out_result and out_exhausted are required"))
	}
	if uint64(maxEntries) == 0 || uint64(maxEntries) > uint64(maxInt()) {
		return fail(outError, C.SHOAL_STATUS_INVALID_ARGUMENT, errors.New("shoal: max_entries must be between 1 and INT_MAX"))
	}
	cursor, err := lookupScanCursor(handle)
	if err != nil {
		return fail(outError, C.SHOAL_STATUS_INVALID_HANDLE, err)
	}
	exhausted, err := cursor.beginPull()
	if err != nil {
		return failForError(outError, err)
	}
	if exhausted {
		*outExhausted = 1
		return C.SHOAL_STATUS_OK
	}
	defer cursor.endPull()
	values, exhausted, scanErr := cursor.next(int(maxEntries))
	if len(values) > 0 {
		result, allocErr := allocateScanResult(values)
		if allocErr != nil {
			cursor.requestClose()
			return fail(outError, C.SHOAL_STATUS_OUT_OF_MEMORY, allocErr)
		}
		*outResult = result
	}
	if exhausted {
		*outExhausted = 1
	}
	if scanErr != nil {
		return failForError(outError, scanErr)
	}
	return C.SHOAL_STATUS_OK
}

//export shoal_scan_cursor_close
func shoal_scan_cursor_close(
	handle *C.shoal_scan_cursor,
	outError **C.shoal_error,
) (status C.shoal_status) {
	clearError(outError)
	defer recoverStatus(&status, outError)
	cursor, err := lookupScanCursor(handle)
	if err != nil {
		return fail(outError, C.SHOAL_STATUS_INVALID_HANDLE, err)
	}
	if err := cursor.close(); err != nil {
		return failForError(outError, err)
	}
	return C.SHOAL_STATUS_OK
}

//export shoal_scan_cursor_free
func shoal_scan_cursor_free(handle **C.shoal_scan_cursor) {
	defer func() { _ = recover() }()
	if handle == nil || *handle == nil {
		return
	}
	value := *handle
	*handle = nil
	id := uint64(C.shoal_bridge_scan_cursor_id(value))
	if cursor, ok := scanCursors.remove(id); ok {
		_ = cursor.close()
	}
	C.shoal_bridge_scan_cursor_free(value)
}

func parseCursorRanges(cRanges *C.shoal_range, rangeCount C.size_t) ([]*accumulo.Range, error) {
	count, err := arrayLength(rangeCount, cRanges != nil, "ranges")
	if err != nil {
		return nil, err
	}
	if count == 0 {
		return nil, errors.New("shoal: at least one range is required")
	}
	inputs := unsafe.Slice(cRanges, count)
	ranges := make([]*accumulo.Range, count)
	for index := range inputs {
		ranges[index], err = parseRange(&inputs[index])
		if err != nil {
			return nil, fmt.Errorf("shoal: range %d: %w", index, err)
		}
	}
	return ranges, nil
}

func clearCursorOutputs(outCursor **C.shoal_scan_cursor, outError **C.shoal_error) {
	clearError(outError)
	if outCursor != nil {
		*outCursor = nil
	}
}

func recoverCursorStatus(
	status *C.shoal_status,
	outCursor **C.shoal_scan_cursor,
	outError **C.shoal_error,
) {
	if recovered := recover(); recovered != nil {
		if outCursor != nil && *outCursor != nil {
			shoal_scan_cursor_free(outCursor)
		}
		*status = fail(outError, C.SHOAL_STATUS_INTERNAL, fmt.Errorf("shoal: internal panic: %v", recovered))
	}
}

func maxInt() int {
	return int(^uint(0) >> 1)
}
