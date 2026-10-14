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
	"unsafe"

	"github.com/phrocker/shoal-oss/accumulo"
)

//export shoal_client_select_column
func shoal_client_select_column(
	handle *C.shoal_client,
	familyValue C.shoal_bytes,
	qualifierValue *C.shoal_bytes,
	outError **C.shoal_error,
) (status C.shoal_status) {
	clearError(outError)
	defer recoverStatus(&status, outError)
	client, err := lookupClient(handle)
	if err != nil {
		return fail(outError, C.SHOAL_STATUS_INVALID_HANDLE, err)
	}
	family, err := copyByteValue(familyValue, "column family")
	if err != nil {
		return fail(outError, C.SHOAL_STATUS_INVALID_ARGUMENT, err)
	}
	var column accumulo.Column
	if qualifierValue == nil {
		column = accumulo.NewColumnFamily(family)
	} else {
		qualifier, copyErr := copyByteValue(*qualifierValue, "column qualifier")
		if copyErr != nil {
			return fail(outError, C.SHOAL_STATUS_INVALID_ARGUMENT, copyErr)
		}
		column = accumulo.NewColumn(family, qualifier)
	}
	return failOrOK(outError, client.addColumn(column))
}

//export shoal_client_scan_range
func shoal_client_scan_range(
	handle *C.shoal_client,
	cRange *C.shoal_range,
	timeoutMilliseconds C.int64_t,
	outResult **C.shoal_scan_result,
	outError **C.shoal_error,
) (status C.shoal_status) {
	return clientScanRange(
		handle, cRange, timeoutMilliseconds, nil, false, outResult, outError,
	)
}

//export shoal_client_scan_range_with_cancellation
func shoal_client_scan_range_with_cancellation(
	handle *C.shoal_client,
	cRange *C.shoal_range,
	timeoutMilliseconds C.int64_t,
	cancellationHandle *C.shoal_cancellation,
	outResult **C.shoal_scan_result,
	outError **C.shoal_error,
) (status C.shoal_status) {
	return clientScanRange(
		handle, cRange, timeoutMilliseconds, cancellationHandle, true,
		outResult, outError,
	)
}

func clientScanRange(
	handle *C.shoal_client,
	cRange *C.shoal_range,
	timeoutMilliseconds C.int64_t,
	cancellationHandle *C.shoal_cancellation,
	requireCancellation bool,
	outResult **C.shoal_scan_result,
	outError **C.shoal_error,
) (status C.shoal_status) {
	clearScanOutputs(outResult, outError)
	defer recoverScanStatus(&status, outResult, outError)
	if outResult == nil {
		return fail(outError, C.SHOAL_STATUS_INVALID_ARGUMENT, errors.New("shoal: out_result is required"))
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
	cancellation, status := clientScanCancellation(
		cancellationHandle, requireCancellation, outError,
	)
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
	values, scanErr := func() ([]accumulo.KeyValue, error) {
		defer done()
		return client.scanOne(ctx, snapshot, scanRange)
	}()
	return finishScan(values, scanErr, outResult, outError)
}

//export shoal_client_scan_ranges
func shoal_client_scan_ranges(
	handle *C.shoal_client,
	cRanges *C.shoal_range,
	rangeCount C.size_t,
	timeoutMilliseconds C.int64_t,
	outResult **C.shoal_scan_result,
	outError **C.shoal_error,
) (status C.shoal_status) {
	return clientScanRanges(
		handle, cRanges, rangeCount, timeoutMilliseconds, nil, false,
		outResult, outError,
	)
}

//export shoal_client_scan_ranges_with_cancellation
func shoal_client_scan_ranges_with_cancellation(
	handle *C.shoal_client,
	cRanges *C.shoal_range,
	rangeCount C.size_t,
	timeoutMilliseconds C.int64_t,
	cancellationHandle *C.shoal_cancellation,
	outResult **C.shoal_scan_result,
	outError **C.shoal_error,
) (status C.shoal_status) {
	return clientScanRanges(
		handle, cRanges, rangeCount, timeoutMilliseconds, cancellationHandle,
		true, outResult, outError,
	)
}

func clientScanRanges(
	handle *C.shoal_client,
	cRanges *C.shoal_range,
	rangeCount C.size_t,
	timeoutMilliseconds C.int64_t,
	cancellationHandle *C.shoal_cancellation,
	requireCancellation bool,
	outResult **C.shoal_scan_result,
	outError **C.shoal_error,
) (status C.shoal_status) {
	clearScanOutputs(outResult, outError)
	defer recoverScanStatus(&status, outResult, outError)
	if outResult == nil {
		return fail(outError, C.SHOAL_STATUS_INVALID_ARGUMENT, errors.New("shoal: out_result is required"))
	}
	client, err := lookupClient(handle)
	if err != nil {
		return fail(outError, C.SHOAL_STATUS_INVALID_HANDLE, err)
	}
	count, err := arrayLength(rangeCount, cRanges != nil, "ranges")
	if err != nil {
		return fail(outError, C.SHOAL_STATUS_INVALID_ARGUMENT, err)
	}
	if count == 0 {
		return fail(outError, C.SHOAL_STATUS_INVALID_ARGUMENT, errors.New("shoal: at least one range is required"))
	}
	inputs := unsafe.Slice(cRanges, count)
	ranges := make([]*accumulo.Range, count)
	for index := range inputs {
		ranges[index], err = parseRange(&inputs[index])
		if err != nil {
			return fail(
				outError,
				C.SHOAL_STATUS_INVALID_ARGUMENT,
				fmt.Errorf("shoal: range %d: %w", index, err),
			)
		}
	}
	timeout, err := operationTimeout(timeoutMilliseconds)
	if err != nil {
		return fail(outError, C.SHOAL_STATUS_INVALID_ARGUMENT, err)
	}
	cancellation, status := clientScanCancellation(
		cancellationHandle, requireCancellation, outError,
	)
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
	values, scanErr := func() ([]accumulo.KeyValue, error) {
		defer done()
		return client.scanMany(ctx, snapshot, ranges)
	}()
	return finishScan(values, scanErr, outResult, outError)
}

func clientScanCancellation(
	handle *C.shoal_cancellation,
	required bool,
	outError **C.shoal_error,
) (*ownedCancellation, C.shoal_status) {
	if !required {
		return nil, C.SHOAL_STATUS_OK
	}
	cancellation, err := lookupCancellation(handle)
	if err != nil {
		return nil, fail(outError, C.SHOAL_STATUS_INVALID_HANDLE, err)
	}
	return cancellation, C.SHOAL_STATUS_OK
}

func attachClientCancellation(
	parent context.Context,
	parentDone func(),
	cancellation *ownedCancellation,
) (context.Context, func(), error) {
	if cancellation == nil {
		return parent, parentDone, nil
	}
	ctx, cancellationDone, err := cancellation.attach(parent)
	if err != nil {
		parentDone()
		return nil, nil, err
	}
	return ctx, func() {
		cancellationDone()
		parentDone()
	}, nil
}
