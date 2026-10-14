//go:build shoal_capi_test

package main

/*
#cgo CFLAGS: -I${SRCDIR}/../../capi/include
#include "bridge.h"
*/
import "C"

import (
	"errors"

	"github.com/phrocker/shoal-oss/accumulo"
)

func testErrorMessageAllocFailAfter(successfulAllocations uint) {
	C.shoal_bridge_test_error_message_alloc_fail_after(C.size_t(successfulAllocations))
}

func testErrorMessageAllocReset() {
	C.shoal_bridge_test_error_message_alloc_reset()
}

func testErrorAllocFailAfter(successfulAllocations uint) {
	C.shoal_bridge_test_error_alloc_fail_after(C.size_t(successfulAllocations))
}

func testErrorAllocReset() {
	C.shoal_bridge_test_error_alloc_reset()
}

func testStringAllocFailAfter(successfulAllocations uint) {
	C.shoal_bridge_test_string_alloc_fail_after(C.size_t(successfulAllocations))
}

func testStringAllocReset() {
	C.shoal_bridge_test_string_alloc_reset()
}

func testFailResult(message string) (status int32, outErrorNil bool, errorMessage string) {
	var outError *C.shoal_error
	result := fail(&outError, C.SHOAL_STATUS_INVALID_ARGUMENT, errors.New(message))
	if outError != nil {
		errorMessage = C.GoString(C.shoal_bridge_error_message(outError))
		C.shoal_bridge_error_free(outError)
		return int32(result), false, errorMessage
	}
	return int32(result), true, ""
}

func structuredWriteFailureError() error {
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

func testFinishWriteWithStructuredFailure() (status int32, outFailureNil bool, outErrorNil bool, message string) {
	var outFailure *C.shoal_write_failure
	var outError *C.shoal_error
	result := finishWrite(structuredWriteFailureError(), &outFailure, &outError)
	if outFailure != nil {
		C.shoal_bridge_write_failure_free(outFailure)
	}
	if outError != nil {
		message = C.GoString(C.shoal_bridge_error_message(outError))
		C.shoal_bridge_error_free(outError)
		return int32(result), outFailure == nil, false, message
	}
	return int32(result), outFailure == nil, true, ""
}

func testAllocateWriteFailure(data writeFailureData) (status int32, message string, failureNil bool) {
	failure, code, err := allocateWriteFailure(data)
	if failure != nil {
		C.shoal_bridge_write_failure_free(failure)
		return int32(code), "", false
	}
	if err == nil {
		return int32(code), "", true
	}
	return int32(code), err.Error(), true
}
