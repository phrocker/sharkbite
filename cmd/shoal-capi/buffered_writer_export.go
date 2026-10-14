package main

/*
#cgo CFLAGS: -I${SRCDIR}/../../capi/include
#include "bridge.h"
*/
import "C"

import (
	"bytes"
	"context"
	"errors"
	"sync"
	"time"

	"github.com/phrocker/shoal-oss/accumulo"
)

type bufferedWriterFactory func() (batchWriterAPI, error)

type ownedAccumuloWriter struct {
	factory bufferedWriterFactory
	owner   *ownedConnector
	now     func() time.Time

	mu      sync.Mutex
	closed  bool
	nextID  uint64
	cancels map[uint64]context.CancelFunc
	active  int
	idle    chan struct{}

	gate       chan struct{}
	writer     batchWriterAPI
	pending    *accumulo.Mutation
	pendingRow []byte

	closeOnce sync.Once
	closeErr  error
}

var accumuloWriters = newRFileRegistry[ownedAccumuloWriter]()

func newOwnedAccumuloWriter(
	factory bufferedWriterFactory,
	owner *ownedConnector,
	now func() time.Time,
) *ownedAccumuloWriter {
	idle := make(chan struct{})
	close(idle)
	gate := make(chan struct{}, 1)
	gate <- struct{}{}
	if now == nil {
		now = time.Now
	}
	return &ownedAccumuloWriter{
		factory: factory,
		owner:   owner,
		now:     now,
		nextID:  1,
		cancels: make(map[uint64]context.CancelFunc),
		idle:    idle,
		gate:    gate,
	}
}

func (w *ownedAccumuloWriter) begin(timeout time.Duration) (context.Context, func(), error) {
	var base context.Context
	var baseDone func()
	var err error
	if w.owner != nil {
		base, baseDone, err = w.owner.begin(timeout)
		if err != nil {
			return nil, nil, err
		}
	} else if timeout == 0 {
		var cancel context.CancelFunc
		base, cancel = context.WithCancel(context.Background())
		baseDone = cancel
	} else {
		var cancel context.CancelFunc
		base, cancel = context.WithTimeout(context.Background(), timeout)
		baseDone = cancel
	}

	w.mu.Lock()
	if w.closed {
		w.mu.Unlock()
		baseDone()
		return nil, nil, accumulo.ErrBatchWriterClosed
	}
	id := w.nextOperationIDLocked()
	if id == 0 {
		w.mu.Unlock()
		baseDone()
		return nil, nil, errors.New("shoal: buffered writer operation space exhausted")
	}
	ctx, cancel := context.WithCancel(base)
	if w.active == 0 {
		w.idle = make(chan struct{})
	}
	w.active++
	w.cancels[id] = cancel
	w.mu.Unlock()

	var once sync.Once
	done := func() {
		once.Do(func() {
			w.mu.Lock()
			delete(w.cancels, id)
			w.active--
			if w.active == 0 {
				close(w.idle)
			}
			w.mu.Unlock()
			cancel()
			baseDone()
		})
	}
	return ctx, done, nil
}

func (w *ownedAccumuloWriter) nextOperationIDLocked() uint64 {
	for attempts := uint64(0); attempts < ^uint64(0); attempts++ {
		id := w.nextID
		w.nextID++
		if w.nextID == 0 {
			w.nextID = 1
		}
		if id != 0 {
			if _, exists := w.cancels[id]; !exists {
				return id
			}
		}
	}
	return 0
}

func (w *ownedAccumuloWriter) acquire(ctx context.Context) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-w.gate:
		return nil
	}
}

func (w *ownedAccumuloWriter) release() {
	w.gate <- struct{}{}
}

func (w *ownedAccumuloWriter) ensureWriter(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if w.writer != nil {
		return nil
	}
	writer, err := w.factory()
	if err != nil {
		return err
	}
	if writer == nil {
		return errors.New("shoal: buffered writer factory returned no writer")
	}
	if err := ctx.Err(); err != nil {
		_ = writer.Close(context.Background())
		return err
	}
	w.writer = writer
	return nil
}

func (w *ownedAccumuloWriter) update(
	ctx context.Context,
	row []byte,
	apply func(*accumulo.Mutation),
) error {
	if err := w.acquire(ctx); err != nil {
		return err
	}
	defer w.release()
	if err := w.ensureWriter(ctx); err != nil {
		return err
	}
	if w.pending != nil && !bytes.Equal(w.pendingRow, row) {
		if err := w.writer.Add(ctx, w.pending); err != nil {
			return err
		}
		w.pending = nil
		w.pendingRow = nil
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if w.pending == nil {
		mutation, err := accumulo.NewMutation(row)
		if err != nil {
			return err
		}
		w.pending = mutation
		w.pendingRow = append([]byte(nil), row...)
	}
	apply(w.pending)
	return nil
}

func (w *ownedAccumuloWriter) putTimestamp(timestamp int64) int64 {
	if timestamp != 0 {
		return timestamp
	}
	return w.now().UnixMilli()
}

func (w *ownedAccumuloWriter) close(timeout time.Duration) error {
	w.closeOnce.Do(func() {
		w.closeErr = w.closeFirst(timeout)
	})
	return w.closeErr
}

func (w *ownedAccumuloWriter) closeFirst(timeout time.Duration) error {
	var ctx context.Context
	var cancel context.CancelFunc
	if timeout == 0 {
		ctx, cancel = context.WithCancel(context.Background())
	} else {
		ctx, cancel = context.WithTimeout(context.Background(), timeout)
	}
	defer cancel()

	w.mu.Lock()
	w.closed = true
	for _, activeCancel := range w.cancels {
		activeCancel()
	}
	idle := w.idle
	w.mu.Unlock()

	select {
	case <-idle:
		return w.finishClose(ctx)
	case <-ctx.Done():
		go w.finishCloseAfterTimeout(idle)
		return ctx.Err()
	}
}

func (w *ownedAccumuloWriter) finishClose(ctx context.Context) error {
	if err := w.acquire(ctx); err != nil {
		return err
	}
	defer w.release()
	if w.writer == nil {
		return nil
	}
	var err error
	if w.pending != nil {
		if addErr := w.writer.Add(ctx, w.pending); addErr != nil {
			err = errors.Join(err, addErr)
		} else {
			w.pending = nil
			w.pendingRow = nil
		}
	}
	err = errors.Join(err, w.writer.Close(ctx))
	return err
}

func (w *ownedAccumuloWriter) finishCloseAfterTimeout(idle <-chan struct{}) {
	<-idle
	ctx, cancel := context.WithTimeout(context.Background(), batchWriterFreeTimeout)
	defer cancel()
	_ = w.finishClose(ctx)
}

func lookupAccumuloWriter(handle *C.shoal_accumulo_writer) (*ownedAccumuloWriter, error) {
	if handle == nil {
		return nil, errors.New("shoal: buffered writer handle is NULL")
	}
	id := uint64(C.shoal_bridge_accumulo_writer_id(handle))
	writer, ok := accumuloWriters.get(id)
	if id == 0 || !ok {
		return nil, errors.New("shoal: buffered writer handle is unknown or freed")
	}
	return writer, nil
}

//export shoal_connector_create_accumulo_writer
func shoal_connector_create_accumulo_writer(
	connectorHandle *C.shoal_connector,
	config *C.shoal_batch_writer_config,
	outWriter **C.shoal_accumulo_writer,
	outError **C.shoal_error,
) (status C.shoal_status) {
	clearError(outError)
	if outWriter != nil {
		*outWriter = nil
	}
	defer recoverStatus(&status, outError)
	if outWriter == nil {
		return fail(outError, C.SHOAL_STATUS_INVALID_ARGUMENT, errors.New("shoal: out_writer is required"))
	}
	connector, err := lookupConnector(connectorHandle)
	if err != nil {
		return fail(outError, C.SHOAL_STATUS_INVALID_HANDLE, err)
	}
	release, err := connector.retain()
	if err != nil {
		return failForError(outError, err)
	}
	defer release()
	table, options, err := parseBatchWriterConfig(config)
	if err != nil {
		return fail(outError, C.SHOAL_STATUS_INVALID_ARGUMENT, err)
	}
	writer := newOwnedAccumuloWriter(func() (batchWriterAPI, error) {
		return connector.connector.NewBatchWriter(table, options)
	}, connector, time.Now)
	id, ok := accumuloWriters.add(writer)
	if !ok {
		return fail(outError, C.SHOAL_STATUS_INTERNAL, errors.New("shoal: buffered writer handle space exhausted"))
	}
	handle := C.shoal_bridge_accumulo_writer_alloc(C.uint64_t(id))
	if handle == nil {
		accumuloWriters.remove(id)
		_ = writer.close(batchWriterFreeTimeout)
		return fail(outError, C.SHOAL_STATUS_OUT_OF_MEMORY, errors.New("shoal: allocate buffered writer handle"))
	}
	*outWriter = handle
	return C.SHOAL_STATUS_OK
}

func bufferedWriterInputs(
	row C.shoal_bytes,
	columnFamily C.shoal_bytes,
	columnQualifier C.shoal_bytes,
	columnVisibility C.shoal_bytes,
	value *C.shoal_bytes,
) ([]byte, []byte, []byte, []byte, []byte, error) {
	rowCopy, err := copyByteValue(row, "buffered writer row")
	if err != nil {
		return nil, nil, nil, nil, nil, err
	}
	if len(rowCopy) == 0 {
		return nil, nil, nil, nil, nil, errors.New("shoal: buffered writer row must be non-empty")
	}
	family, qualifier, visibility, valueCopy, err := parseMutationUpdate(
		columnFamily,
		columnQualifier,
		columnVisibility,
		value,
	)
	return rowCopy, family, qualifier, visibility, valueCopy, err
}

func runBufferedWriterUpdate(
	handle *C.shoal_accumulo_writer,
	timeoutMilliseconds C.int64_t,
	outFailure **C.shoal_write_failure,
	outError **C.shoal_error,
	run func(context.Context, *ownedAccumuloWriter) error,
) C.shoal_status {
	writer, err := lookupAccumuloWriter(handle)
	if err != nil {
		return fail(outError, C.SHOAL_STATUS_INVALID_HANDLE, err)
	}
	timeout, err := operationTimeout(timeoutMilliseconds)
	if err != nil {
		return fail(outError, C.SHOAL_STATUS_INVALID_ARGUMENT, err)
	}
	ctx, done, err := writer.begin(timeout)
	if err != nil {
		return finishWrite(err, outFailure, outError)
	}
	defer done()
	return finishWrite(run(ctx, writer), outFailure, outError)
}

//export shoal_accumulo_writer_put
func shoal_accumulo_writer_put(
	handle *C.shoal_accumulo_writer,
	row C.shoal_bytes,
	columnFamily C.shoal_bytes,
	columnQualifier C.shoal_bytes,
	columnVisibility C.shoal_bytes,
	timestamp C.int64_t,
	value C.shoal_bytes,
	timeoutMilliseconds C.int64_t,
	outFailure **C.shoal_write_failure,
	outError **C.shoal_error,
) (status C.shoal_status) {
	clearWriteOutputs(outFailure, outError)
	defer recoverWriteStatus(&status, outFailure, outError)
	rowCopy, family, qualifier, visibility, valueCopy, err := bufferedWriterInputs(
		row, columnFamily, columnQualifier, columnVisibility, &value,
	)
	if err != nil {
		return fail(outError, C.SHOAL_STATUS_INVALID_ARGUMENT, err)
	}
	return runBufferedWriterUpdate(handle, timeoutMilliseconds, outFailure, outError,
		func(ctx context.Context, writer *ownedAccumuloWriter) error {
			effectiveTimestamp := writer.putTimestamp(int64(timestamp))
			return writer.update(ctx, rowCopy, func(mutation *accumulo.Mutation) {
				mutation.Put(family, qualifier, visibility, effectiveTimestamp, valueCopy)
			})
		})
}

//export shoal_accumulo_writer_put_delete
func shoal_accumulo_writer_put_delete(
	handle *C.shoal_accumulo_writer,
	row C.shoal_bytes,
	columnFamily C.shoal_bytes,
	columnQualifier C.shoal_bytes,
	columnVisibility C.shoal_bytes,
	timestamp C.int64_t,
	timeoutMilliseconds C.int64_t,
	outFailure **C.shoal_write_failure,
	outError **C.shoal_error,
) (status C.shoal_status) {
	clearWriteOutputs(outFailure, outError)
	defer recoverWriteStatus(&status, outFailure, outError)
	rowCopy, family, qualifier, visibility, _, err := bufferedWriterInputs(
		row, columnFamily, columnQualifier, columnVisibility, nil,
	)
	if err != nil {
		return fail(outError, C.SHOAL_STATUS_INVALID_ARGUMENT, err)
	}
	return runBufferedWriterUpdate(handle, timeoutMilliseconds, outFailure, outError,
		func(ctx context.Context, writer *ownedAccumuloWriter) error {
			return writer.update(ctx, rowCopy, func(mutation *accumulo.Mutation) {
				mutation.Delete(family, qualifier, visibility, int64(timestamp))
			})
		})
}

//export shoal_accumulo_writer_delete
func shoal_accumulo_writer_delete(
	handle *C.shoal_accumulo_writer,
	input *C.shoal_key,
	timeoutMilliseconds C.int64_t,
	outFailure **C.shoal_write_failure,
	outError **C.shoal_error,
) (status C.shoal_status) {
	clearWriteOutputs(outFailure, outError)
	defer recoverWriteStatus(&status, outFailure, outError)
	if input == nil {
		return fail(outError, C.SHOAL_STATUS_INVALID_ARGUMENT, errors.New("shoal: key is required"))
	}
	key, err := parseKey(*input, "buffered writer key")
	if err != nil {
		return fail(outError, C.SHOAL_STATUS_INVALID_ARGUMENT, err)
	}
	return runBufferedWriterUpdate(handle, timeoutMilliseconds, outFailure, outError,
		func(ctx context.Context, writer *ownedAccumuloWriter) error {
			return writer.update(ctx, key.Row, func(mutation *accumulo.Mutation) {
				mutation.Delete(
					key.ColumnFamily,
					key.ColumnQualifier,
					key.ColumnVisibility,
					key.Timestamp,
				)
			})
		})
}

//export shoal_accumulo_writer_close
func shoal_accumulo_writer_close(
	handle *C.shoal_accumulo_writer,
	timeoutMilliseconds C.int64_t,
	outFailure **C.shoal_write_failure,
	outError **C.shoal_error,
) (status C.shoal_status) {
	clearWriteOutputs(outFailure, outError)
	defer recoverWriteStatus(&status, outFailure, outError)
	writer, err := lookupAccumuloWriter(handle)
	if err != nil {
		return fail(outError, C.SHOAL_STATUS_INVALID_HANDLE, err)
	}
	timeout, err := operationTimeout(timeoutMilliseconds)
	if err != nil {
		return fail(outError, C.SHOAL_STATUS_INVALID_ARGUMENT, err)
	}
	return finishWrite(writer.close(timeout), outFailure, outError)
}

//export shoal_accumulo_writer_free
func shoal_accumulo_writer_free(handle **C.shoal_accumulo_writer) {
	defer func() { _ = recover() }()
	if handle == nil || *handle == nil {
		return
	}
	value := *handle
	*handle = nil
	id := uint64(C.shoal_bridge_accumulo_writer_id(value))
	if writer, ok := accumuloWriters.remove(id); ok {
		_ = writer.close(batchWriterFreeTimeout)
	}
	C.shoal_bridge_accumulo_writer_free(value)
}
