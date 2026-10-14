//go:build shoal_capi_test

package main

/*
#cgo CFLAGS: -I${SRCDIR}/../../capi/include -I${SRCDIR}/../../capi/tests
#include "bridge.h"
#include "test_seam.h"
*/
import "C"

import (
	"context"
	"errors"
	"sync"

	"github.com/phrocker/shoal-oss/accumulo"
)

type testBatchWriter struct {
	mode int

	mu       sync.Mutex
	closeErr error
	pending  int
}

func (w *testBatchWriter) Size(context.Context) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.pending, nil
}

func (w *testBatchWriter) Add(_ context.Context, mutation *accumulo.Mutation) error {
	if mutation == nil || mutation.Size() == 0 {
		return errors.New("accumulo: mutation must contain at least one update")
	}
	w.mu.Lock()
	w.pending++
	w.mu.Unlock()
	return nil
}

func (w *testBatchWriter) Flush(context.Context) error {
	if w.mode != int(C.SHOAL_TEST_WRITER_STRUCTURED_FAILURE) {
		w.mu.Lock()
		w.pending = 0
		w.mu.Unlock()
		return nil
	}
	return testStructuredWriterError()
}

func testStructuredWriterError() error {
	return errors.Join(
		accumulo.ErrBatchWriterFailed,
		accumulo.ErrBatchWriterRetryExhausted,
		accumulo.ErrBatchWriterAutoFlush,
		&accumulo.MutationRejectionError{
			Server: "server:9997",
			FailedExtents: []accumulo.FailedExtent{{
				Extent: accumulo.TabletExtent{
					TableID: "5",
					PrevRow: []byte("a"),
					EndRow:  []byte("z"),
				},
				Submitted: 3,
				Committed: 2,
			}},
			ConstraintViolations: []accumulo.ConstraintViolation{{
				ConstraintClass:            "Constraint",
				ViolationCode:              7,
				Description:                "bad mutation",
				NumberOfViolatingMutations: 4,
			}},
			AuthorizationFailures: []accumulo.AuthorizationFailure{{
				Extent: accumulo.TabletExtent{TableID: "5"},
				Code:   "PERMISSION_DENIED",
			}},
		},
		&accumulo.BatchWriterCleanupError{
			Server: "server:9997",
			Err:    errors.New("cancel failed"),
		},
	)
}

func (w *testBatchWriter) Close(context.Context) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closeErr != nil {
		return w.closeErr
	}
	if w.mode == int(C.SHOAL_TEST_WRITER_STRUCTURED_FAILURE) {
		return testStructuredWriterError()
	}
	if w.mode == int(C.SHOAL_TEST_WRITER_STICKY_DEADLINE) {
		w.closeErr = context.DeadlineExceeded
	}
	return w.closeErr
}

//export shoal_test_batch_writer_create
func shoal_test_batch_writer_create(
	mode C.int,
	outWriter **C.shoal_batch_writer,
) C.int {
	if outWriter == nil {
		return 0
	}
	*outWriter = nil
	if mode < C.SHOAL_TEST_WRITER_SUCCESS ||
		mode > C.SHOAL_TEST_WRITER_CONNECTOR_CLOSED {
		return 0
	}
	var owner *ownedConnector
	if mode == C.SHOAL_TEST_WRITER_CONNECTOR_CLOSED {
		owner = &ownedConnector{}
		owner.closed.Store(true)
	}
	owned := newOwnedBatchWriter(&testBatchWriter{mode: int(mode)}, owner)
	id, ok := batchWriters.add(owned)
	if !ok {
		return 0
	}
	handle := C.shoal_bridge_batch_writer_alloc(C.uint64_t(id))
	if handle == nil {
		batchWriters.remove(id)
		return 0
	}
	*outWriter = handle
	return 1
}

//export shoal_test_accumulo_writer_create
func shoal_test_accumulo_writer_create(
	mode C.int,
	outWriter **C.shoal_accumulo_writer,
) C.int {
	if outWriter == nil {
		return 0
	}
	*outWriter = nil
	if mode < C.SHOAL_TEST_WRITER_SUCCESS ||
		mode > C.SHOAL_TEST_WRITER_STICKY_DEADLINE {
		return 0
	}
	owned := newOwnedAccumuloWriter(func() (batchWriterAPI, error) {
		return &testBatchWriter{mode: int(mode)}, nil
	}, nil, nil)
	id, ok := accumuloWriters.add(owned)
	if !ok {
		return 0
	}
	handle := C.shoal_bridge_accumulo_writer_alloc(C.uint64_t(id))
	if handle == nil {
		accumuloWriters.remove(id)
		return 0
	}
	*outWriter = handle
	return 1
}
