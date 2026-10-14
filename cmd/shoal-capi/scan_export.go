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
	"math"
	"os"
	"time"
	"unsafe"

	"github.com/phrocker/shoal-oss/accumulo"
	publichdfs "github.com/phrocker/shoal-oss/hdfs"
	"github.com/phrocker/shoal-oss/internal/storage"
	publicrfile "github.com/phrocker/shoal-oss/rfile"
)

//export shoal_scanner_config_init
func shoal_scanner_config_init(config *C.shoal_scanner_config) {
	C.shoal_bridge_scanner_config_init(config)
}

//export shoal_range_init
func shoal_range_init(scanRange *C.shoal_range) {
	C.shoal_bridge_range_init(scanRange)
}

//export shoal_connector_create_scanner
func shoal_connector_create_scanner(
	connectorHandle *C.shoal_connector,
	config *C.shoal_scanner_config,
	outScanner **C.shoal_scanner,
	outError **C.shoal_error,
) (status C.shoal_status) {
	clearError(outError)
	if outScanner != nil {
		*outScanner = nil
	}
	defer recoverStatus(&status, outError)
	if outScanner == nil {
		return fail(outError, C.SHOAL_STATUS_INVALID_ARGUMENT, errors.New("shoal: out_scanner is required"))
	}
	connector, err := lookupConnector(connectorHandle)
	if err != nil {
		return fail(outError, C.SHOAL_STATUS_INVALID_HANDLE, err)
	}
	table, options, err := parseScannerConfig(config)
	if err != nil {
		return fail(outError, C.SHOAL_STATUS_INVALID_ARGUMENT, err)
	}
	scanner, err := connector.connector.NewScanner(table, options)
	if err != nil {
		return failForError(outError, err)
	}
	owned := newOwnedScanner(scanner, nil, connector)
	id, ok := scanners.add(owned)
	if !ok {
		return fail(outError, C.SHOAL_STATUS_INTERNAL, errors.New("shoal: scanner handle space exhausted"))
	}
	handle := C.shoal_bridge_scanner_alloc(C.uint64_t(id))
	if handle == nil {
		scanners.remove(id)
		owned.close()
		return fail(outError, C.SHOAL_STATUS_OUT_OF_MEMORY, errors.New("shoal: allocate scanner handle"))
	}
	*outScanner = handle
	return C.SHOAL_STATUS_OK
}

//export shoal_connector_create_batch_scanner
func shoal_connector_create_batch_scanner(
	connectorHandle *C.shoal_connector,
	config *C.shoal_scanner_config,
	outScanner **C.shoal_batch_scanner,
	outError **C.shoal_error,
) (status C.shoal_status) {
	clearError(outError)
	if outScanner != nil {
		*outScanner = nil
	}
	defer recoverStatus(&status, outError)
	if outScanner == nil {
		return fail(outError, C.SHOAL_STATUS_INVALID_ARGUMENT, errors.New("shoal: out_scanner is required"))
	}
	connector, err := lookupConnector(connectorHandle)
	if err != nil {
		return fail(outError, C.SHOAL_STATUS_INVALID_HANDLE, err)
	}
	table, options, err := parseScannerConfig(config)
	if err != nil {
		return fail(outError, C.SHOAL_STATUS_INVALID_ARGUMENT, err)
	}
	scanner, err := connector.connector.NewBatchScanner(table, options)
	if err != nil {
		return failForError(outError, err)
	}
	owned := newOwnedScanner(nil, scanner, connector)
	id, ok := batchScanners.add(owned)
	if !ok {
		return fail(outError, C.SHOAL_STATUS_INTERNAL, errors.New("shoal: batch scanner handle space exhausted"))
	}
	handle := C.shoal_bridge_batch_scanner_alloc(C.uint64_t(id))
	if handle == nil {
		batchScanners.remove(id)
		owned.close()
		return fail(outError, C.SHOAL_STATUS_OUT_OF_MEMORY, errors.New("shoal: allocate batch scanner handle"))
	}
	*outScanner = handle
	return C.SHOAL_STATUS_OK
}

//export shoal_scanner_close
func shoal_scanner_close(
	handle *C.shoal_scanner,
	outError **C.shoal_error,
) (status C.shoal_status) {
	clearError(outError)
	defer recoverStatus(&status, outError)
	scanner, err := lookupScanner(handle)
	if err != nil {
		return fail(outError, C.SHOAL_STATUS_INVALID_HANDLE, err)
	}
	scanner.close()
	return C.SHOAL_STATUS_OK
}

//export shoal_scanner_free
func shoal_scanner_free(handle **C.shoal_scanner) {
	defer func() { _ = recover() }()
	if handle == nil || *handle == nil {
		return
	}
	value := *handle
	*handle = nil
	id := uint64(C.shoal_bridge_scanner_id(value))
	if scanner, ok := scanners.remove(id); ok {
		scanner.close()
	}
	C.shoal_bridge_scanner_free(value)
}

//export shoal_batch_scanner_close
func shoal_batch_scanner_close(
	handle *C.shoal_batch_scanner,
	outError **C.shoal_error,
) (status C.shoal_status) {
	clearError(outError)
	defer recoverStatus(&status, outError)
	scanner, err := lookupBatchScanner(handle)
	if err != nil {
		return fail(outError, C.SHOAL_STATUS_INVALID_HANDLE, err)
	}
	scanner.close()
	return C.SHOAL_STATUS_OK
}

//export shoal_batch_scanner_free
func shoal_batch_scanner_free(handle **C.shoal_batch_scanner) {
	defer func() { _ = recover() }()
	if handle == nil || *handle == nil {
		return
	}
	value := *handle
	*handle = nil
	id := uint64(C.shoal_bridge_batch_scanner_id(value))
	if scanner, ok := batchScanners.remove(id); ok {
		scanner.close()
	}
	C.shoal_bridge_batch_scanner_free(value)
}

//export shoal_scanner_scan
func shoal_scanner_scan(
	handle *C.shoal_scanner,
	cRange *C.shoal_range,
	timeoutMilliseconds C.int64_t,
	outResult **C.shoal_scan_result,
	outError **C.shoal_error,
) (status C.shoal_status) {
	return scannerScan(handle, cRange, timeoutMilliseconds, nil, false, outResult, outError)
}

//export shoal_scanner_scan_with_cancellation
func shoal_scanner_scan_with_cancellation(
	handle *C.shoal_scanner,
	cRange *C.shoal_range,
	timeoutMilliseconds C.int64_t,
	cancellationHandle *C.shoal_cancellation,
	outResult **C.shoal_scan_result,
	outError **C.shoal_error,
) (status C.shoal_status) {
	return scannerScan(
		handle, cRange, timeoutMilliseconds, cancellationHandle, true,
		outResult, outError,
	)
}

func scannerScan(
	handle *C.shoal_scanner,
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
	ctx, done, err := scanner.beginCancelable(timeout, cancellation)
	if err != nil {
		return failForError(outError, err)
	}
	values, scanErr := func() ([]accumulo.KeyValue, error) {
		defer done()
		return scanner.single.Scan(ctx, scanRange)
	}()
	return finishScan(values, scanErr, outResult, outError)
}

//export shoal_batch_scanner_scan
func shoal_batch_scanner_scan(
	handle *C.shoal_batch_scanner,
	cRanges *C.shoal_range,
	rangeCount C.size_t,
	timeoutMilliseconds C.int64_t,
	outResult **C.shoal_scan_result,
	outError **C.shoal_error,
) (status C.shoal_status) {
	return batchScannerScan(
		handle, cRanges, rangeCount, timeoutMilliseconds, nil, false,
		outResult, outError,
	)
}

//export shoal_batch_scanner_scan_with_cancellation
func shoal_batch_scanner_scan_with_cancellation(
	handle *C.shoal_batch_scanner,
	cRanges *C.shoal_range,
	rangeCount C.size_t,
	timeoutMilliseconds C.int64_t,
	cancellationHandle *C.shoal_cancellation,
	outResult **C.shoal_scan_result,
	outError **C.shoal_error,
) (status C.shoal_status) {
	return batchScannerScan(
		handle, cRanges, rangeCount, timeoutMilliseconds, cancellationHandle,
		true, outResult, outError,
	)
}

func batchScannerScan(
	handle *C.shoal_batch_scanner,
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
	scanner, err := lookupBatchScanner(handle)
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
	var cancellation *ownedCancellation
	if requireCancellation {
		cancellation, err = lookupCancellation(cancellationHandle)
		if err != nil {
			return fail(outError, C.SHOAL_STATUS_INVALID_HANDLE, err)
		}
	}
	ctx, done, err := scanner.beginCancelable(timeout, cancellation)
	if err != nil {
		return failForError(outError, err)
	}
	values, scanErr := func() ([]accumulo.KeyValue, error) {
		defer done()
		return scanner.batch.Scan(ctx, ranges)
	}()
	return finishScan(values, scanErr, outResult, outError)
}

//export shoal_scan_result_count
func shoal_scan_result_count(result *C.shoal_scan_result) C.size_t {
	return C.shoal_bridge_scan_result_count(result)
}

//export shoal_scan_result_get
func shoal_scan_result_get(
	result *C.shoal_scan_result,
	index C.size_t,
	outValue *C.shoal_key_value_view,
	outError **C.shoal_error,
) (status C.shoal_status) {
	clearError(outError)
	defer recoverStatus(&status, outError)
	if result == nil {
		return fail(outError, C.SHOAL_STATUS_INVALID_ARGUMENT, errors.New("shoal: scan result is NULL"))
	}
	if outValue == nil {
		return fail(outError, C.SHOAL_STATUS_INVALID_ARGUMENT, errors.New("shoal: out_value is required"))
	}
	if C.shoal_bridge_scan_result_get(result, index, outValue) == 0 {
		return fail(
			outError,
			C.SHOAL_STATUS_INVALID_ARGUMENT,
			fmt.Errorf("shoal: scan result index %d is out of bounds", uint64(index)),
		)
	}
	return C.SHOAL_STATUS_OK
}

//export shoal_scan_result_free
func shoal_scan_result_free(result **C.shoal_scan_result) {
	if result == nil || *result == nil {
		return
	}
	C.shoal_bridge_scan_result_free(*result)
	*result = nil
}

func parseScannerConfig(config *C.shoal_scanner_config) (accumulo.Table, accumulo.ScannerOptions, error) {
	if config == nil {
		return accumulo.Table{}, accumulo.ScannerOptions{}, errors.New("shoal: scanner config is required")
	}
	requiredSize := uint64(C.shoal_bridge_scanner_config_v1_size())
	if uint64(config.struct_size) < requiredSize {
		return accumulo.Table{}, accumulo.ScannerOptions{}, fmt.Errorf(
			"shoal: scanner config struct_size is %d, need at least %d",
			uint64(config.struct_size),
			requiredSize,
		)
	}
	tableName := optionalString(config.table_name)
	tableID := optionalString(config.table_id)
	if (tableName == "") == (tableID == "") {
		return accumulo.Table{}, accumulo.ScannerOptions{}, errors.New(
			"shoal: exactly one of table_name and table_id is required",
		)
	}
	if config.use_multi_scan > 1 {
		return accumulo.Table{}, accumulo.ScannerOptions{}, errors.New("shoal: use_multi_scan must be 0 or 1")
	}

	authorizations, err := parseAuthorizations(config.authorizations, config.authorization_count)
	if err != nil {
		return accumulo.Table{}, accumulo.ScannerOptions{}, err
	}
	columns, err := parseColumns(config.columns, config.column_count)
	if err != nil {
		return accumulo.Table{}, accumulo.ScannerOptions{}, err
	}
	iterators, err := parseIterators(config.iterators, config.iterator_count)
	if err != nil {
		return accumulo.Table{}, accumulo.ScannerOptions{}, err
	}
	return accumulo.Table{Name: tableName, ID: tableID}, accumulo.ScannerOptions{
		BatchSize:      int32(config.batch_size),
		Authorizations: authorizations,
		Columns:        columns,
		Iterators:      iterators,
		Parallelism:    int(config.parallelism),
		UseMultiScan:   config.use_multi_scan != 0,
	}, nil
}

func parseAuthorizations(values *C.shoal_bytes, count C.size_t) ([][]byte, error) {
	length, err := arrayLength(count, values != nil, "authorizations")
	if err != nil {
		return nil, err
	}
	inputs := unsafe.Slice(values, length)
	result := make([][]byte, length)
	for index := range inputs {
		result[index], err = copyByteValue(inputs[index], fmt.Sprintf("authorization %d", index))
		if err != nil {
			return nil, err
		}
	}
	return result, nil
}

func parseColumns(values *C.shoal_column, count C.size_t) ([]accumulo.Column, error) {
	length, err := arrayLength(count, values != nil, "columns")
	if err != nil {
		return nil, err
	}
	inputs := unsafe.Slice(values, length)
	result := make([]accumulo.Column, length)
	for index, input := range inputs {
		if input.has_qualifier > 1 {
			return nil, fmt.Errorf("shoal: column %d has_qualifier must be 0 or 1", index)
		}
		family, err := copyByteValue(input.family, fmt.Sprintf("column %d family", index))
		if err != nil {
			return nil, err
		}
		if input.has_qualifier == 0 {
			result[index] = accumulo.NewColumnFamily(family)
			continue
		}
		qualifier, err := copyByteValue(input.qualifier, fmt.Sprintf("column %d qualifier", index))
		if err != nil {
			return nil, err
		}
		result[index] = accumulo.NewColumn(family, qualifier)
	}
	return result, nil
}

func parseIterators(values *C.shoal_iterator_setting, count C.size_t) ([]accumulo.IteratorSetting, error) {
	length, err := arrayLength(count, values != nil, "iterators")
	if err != nil {
		return nil, err
	}
	inputs := unsafe.Slice(values, length)
	result := make([]accumulo.IteratorSetting, length)
	names := make(map[string]struct{}, length)
	priorities := make(map[int32]struct{}, length)
	for index, input := range inputs {
		name, err := requiredString(input.name, fmt.Sprintf("iterator %d name", index))
		if err != nil {
			return nil, err
		}
		className, err := requiredString(input.class_name, fmt.Sprintf("iterator %d class_name", index))
		if err != nil {
			return nil, err
		}
		priority := int32(input.priority)
		if _, exists := names[name]; exists {
			return nil, fmt.Errorf("shoal: duplicate iterator name %q", name)
		}
		if _, exists := priorities[priority]; exists {
			return nil, fmt.Errorf("shoal: duplicate iterator priority %d", priority)
		}
		options, err := parseIteratorOptions(index, input.options, input.option_count)
		if err != nil {
			return nil, err
		}
		result[index], err = accumulo.NewIteratorSetting(name, className, priority, options)
		if err != nil {
			return nil, fmt.Errorf("shoal: iterator %d: %w", index, err)
		}
		names[name] = struct{}{}
		priorities[priority] = struct{}{}
	}
	return result, nil
}

func parseIteratorOptions(
	iteratorIndex int,
	values *C.shoal_iterator_option,
	count C.size_t,
) (map[string]string, error) {
	length, err := arrayLength(count, values != nil, fmt.Sprintf("iterator %d options", iteratorIndex))
	if err != nil {
		return nil, err
	}
	if length == 0 {
		return nil, nil
	}
	inputs := unsafe.Slice(values, length)
	result := make(map[string]string, length)
	for optionIndex, input := range inputs {
		key, err := requiredString(
			input.key,
			fmt.Sprintf("iterator %d option %d key", iteratorIndex, optionIndex),
		)
		if err != nil {
			return nil, err
		}
		if input.value == nil {
			return nil, fmt.Errorf("shoal: iterator %d option %d value is required", iteratorIndex, optionIndex)
		}
		if _, exists := result[key]; exists {
			return nil, fmt.Errorf("shoal: iterator %d has duplicate option %q", iteratorIndex, key)
		}
		result[key] = C.GoString(input.value)
	}
	return result, nil
}

func parseRange(input *C.shoal_range) (*accumulo.Range, error) {
	if input == nil {
		return nil, errors.New("shoal: range is required")
	}
	requiredSize := uint64(C.shoal_bridge_range_v1_size())
	if uint64(input.struct_size) < requiredSize {
		return nil, fmt.Errorf(
			"shoal: range struct_size is %d, need at least %d",
			uint64(input.struct_size),
			requiredSize,
		)
	}
	if input.start_inclusive > 1 || input.end_inclusive > 1 {
		return nil, errors.New("shoal: range flags must be 0 or 1")
	}
	start, err := parseRangeBound(input.start, "range start")
	if err != nil {
		return nil, err
	}
	end, err := parseRangeBound(input.end, "range end")
	if err != nil {
		return nil, err
	}
	if start.kind != C.SHOAL_RANGE_BOUND_UNBOUNDED &&
		end.kind != C.SHOAL_RANGE_BOUND_UNBOUNDED &&
		start.kind != end.kind {
		return nil, errors.New("shoal: range may not mix row and key bounds")
	}
	keyRange := start.kind == C.SHOAL_RANGE_BOUND_KEY || end.kind == C.SHOAL_RANGE_BOUND_KEY
	if keyRange {
		return accumulo.NewKeyRange(
			start.key,
			input.start_inclusive != 0,
			end.key,
			input.end_inclusive != 0,
		)
	}
	return accumulo.NewRange(
		start.row,
		input.start_inclusive != 0,
		end.row,
		input.end_inclusive != 0,
	)
}

type parsedRangeBound struct {
	kind C.shoal_range_bound_kind
	row  []byte
	key  *accumulo.Key
}

func parseRangeBound(input C.shoal_range_bound, name string) (parsedRangeBound, error) {
	switch input.kind {
	case C.SHOAL_RANGE_BOUND_UNBOUNDED:
		if !emptyBytes(input.row) || !emptyCKey(input.key) {
			return parsedRangeBound{}, fmt.Errorf("shoal: %s is unbounded but provides bound data", name)
		}
		return parsedRangeBound{kind: input.kind}, nil
	case C.SHOAL_RANGE_BOUND_ROW:
		if !emptyCKey(input.key) {
			return parsedRangeBound{}, fmt.Errorf("shoal: %s row bound provides key data", name)
		}
		row, err := copyByteValue(input.row, name+" row")
		if err != nil {
			return parsedRangeBound{}, err
		}
		if row == nil {
			row = make([]byte, 0)
		}
		return parsedRangeBound{kind: input.kind, row: row}, nil
	case C.SHOAL_RANGE_BOUND_KEY:
		if !emptyBytes(input.row) {
			return parsedRangeBound{}, fmt.Errorf("shoal: %s key bound provides row data", name)
		}
		key, err := parseKey(input.key, name+" key")
		if err != nil {
			return parsedRangeBound{}, err
		}
		return parsedRangeBound{kind: input.kind, key: key}, nil
	default:
		return parsedRangeBound{}, fmt.Errorf("shoal: %s has unsupported kind %d", name, int32(input.kind))
	}
}

func parseKey(input C.shoal_key, name string) (*accumulo.Key, error) {
	row, err := copyByteValue(input.row, name+" row")
	if err != nil {
		return nil, err
	}
	family, err := copyByteValue(input.column_family, name+" column_family")
	if err != nil {
		return nil, err
	}
	qualifier, err := copyByteValue(input.column_qualifier, name+" column_qualifier")
	if err != nil {
		return nil, err
	}
	visibility, err := copyByteValue(input.column_visibility, name+" column_visibility")
	if err != nil {
		return nil, err
	}
	return &accumulo.Key{
		Row:              row,
		ColumnFamily:     family,
		ColumnQualifier:  qualifier,
		ColumnVisibility: visibility,
		Timestamp:        int64(input.timestamp),
	}, nil
}

func emptyBytes(value C.shoal_bytes) bool {
	return value.data == nil && value.length == 0
}

func emptyCKey(value C.shoal_key) bool {
	return emptyBytes(value.row) &&
		emptyBytes(value.column_family) &&
		emptyBytes(value.column_qualifier) &&
		emptyBytes(value.column_visibility) &&
		value.timestamp == 0
}

func finishScan(
	values []accumulo.KeyValue,
	scanErr error,
	outResult **C.shoal_scan_result,
	outError **C.shoal_error,
) C.shoal_status {
	if scanErr != nil && len(values) == 0 {
		return failForError(outError, scanErr)
	}
	result, err := allocateScanResult(values)
	if err != nil {
		return fail(outError, C.SHOAL_STATUS_OUT_OF_MEMORY, err)
	}
	*outResult = result
	if scanErr != nil {
		return failForError(outError, scanErr)
	}
	return C.SHOAL_STATUS_OK
}

func allocateScanResult(values []accumulo.KeyValue) (*C.shoal_scan_result, error) {
	result := C.shoal_bridge_scan_result_alloc(C.size_t(len(values)))
	if result == nil {
		return nil, errors.New("shoal: allocate scan result")
	}
	for index, value := range values {
		if C.shoal_bridge_scan_result_set(
			result,
			C.size_t(index),
			bytePointer(value.Key.Row),
			C.size_t(len(value.Key.Row)),
			bytePointer(value.Key.ColumnFamily),
			C.size_t(len(value.Key.ColumnFamily)),
			bytePointer(value.Key.ColumnQualifier),
			C.size_t(len(value.Key.ColumnQualifier)),
			bytePointer(value.Key.ColumnVisibility),
			C.size_t(len(value.Key.ColumnVisibility)),
			C.int64_t(value.Key.Timestamp),
			bytePointer(value.Value),
			C.size_t(len(value.Value)),
		) == 0 {
			C.shoal_bridge_scan_result_free(result)
			return nil, fmt.Errorf("shoal: allocate scan result entry %d", index)
		}
	}
	return result, nil
}

func lookupScanner(handle *C.shoal_scanner) (*ownedScanner, error) {
	if handle == nil {
		return nil, errors.New("shoal: scanner handle is NULL")
	}
	id := uint64(C.shoal_bridge_scanner_id(handle))
	if id == 0 {
		return nil, errors.New("shoal: scanner handle is invalid")
	}
	scanner, ok := scanners.get(id)
	if !ok {
		return nil, errors.New("shoal: scanner handle is unknown or freed")
	}
	return scanner, nil
}

func lookupBatchScanner(handle *C.shoal_batch_scanner) (*ownedScanner, error) {
	if handle == nil {
		return nil, errors.New("shoal: batch scanner handle is NULL")
	}
	id := uint64(C.shoal_bridge_batch_scanner_id(handle))
	if id == 0 {
		return nil, errors.New("shoal: batch scanner handle is invalid")
	}
	scanner, ok := batchScanners.get(id)
	if !ok {
		return nil, errors.New("shoal: batch scanner handle is unknown or freed")
	}
	return scanner, nil
}

func operationTimeout(value C.int64_t) (time.Duration, error) {
	milliseconds := int64(value)
	if milliseconds < 0 {
		return 0, errors.New("shoal: timeout_ms must not be negative")
	}
	if milliseconds == 0 {
		return 0, nil
	}
	if milliseconds > math.MaxInt64/int64(time.Millisecond) {
		return 0, errors.New("shoal: timeout_ms is too large")
	}
	return time.Duration(milliseconds) * time.Millisecond, nil
}

func arrayLength(count C.size_t, pointerPresent bool, name string) (int, error) {
	if uint64(count) > uint64(math.MaxInt) {
		return 0, fmt.Errorf("shoal: %s count is too large", name)
	}
	length := int(count)
	if length != 0 && !pointerPresent {
		return 0, fmt.Errorf("shoal: %s is NULL with non-zero count", name)
	}
	return length, nil
}

func copyByteValue(value C.shoal_bytes, name string) ([]byte, error) {
	return copyBytes(value.data, value.length, name)
}

func bytePointer(value []byte) *C.uint8_t {
	if len(value) == 0 {
		return nil
	}
	return (*C.uint8_t)(unsafe.Pointer(&value[0]))
}

func clearScanOutputs(outResult **C.shoal_scan_result, outError **C.shoal_error) {
	clearError(outError)
	if outResult != nil {
		*outResult = nil
	}
}

func recoverStatus(status *C.shoal_status, outError **C.shoal_error) {
	if recovered := recover(); recovered != nil {
		*status = fail(
			outError,
			C.SHOAL_STATUS_INTERNAL,
			fmt.Errorf("shoal: internal panic: %v", recovered),
		)
	}
}

func recoverScanStatus(
	status *C.shoal_status,
	outResult **C.shoal_scan_result,
	outError **C.shoal_error,
) {
	if recovered := recover(); recovered != nil {
		if outResult != nil && *outResult != nil {
			C.shoal_bridge_scan_result_free(*outResult)
			*outResult = nil
		}
		*status = fail(
			outError,
			C.SHOAL_STATUS_INTERNAL,
			fmt.Errorf("shoal: internal panic: %v", recovered),
		)
	}
}

func failForError(outError **C.shoal_error, err error) C.shoal_status {
	return fail(outError, statusForError(err), err)
}

func statusForError(err error) C.shoal_status {
	switch {
	case err == nil:
		return C.SHOAL_STATUS_OK
	case errors.Is(err, accumulo.ErrBatchWriterFailed):
		return C.SHOAL_STATUS_AMBIGUOUS_WRITE
	case errors.Is(err, context.DeadlineExceeded):
		return C.SHOAL_STATUS_DEADLINE_EXCEEDED
	case errors.Is(err, context.Canceled):
		return C.SHOAL_STATUS_CANCELLED
	case errors.Is(err, accumulo.ErrConnectorClosed):
		return C.SHOAL_STATUS_CLOSED
	case errors.Is(err, accumulo.ErrStreamClosed):
		return C.SHOAL_STATUS_CLOSED
	case errors.Is(err, publicrfile.ErrClosed):
		return C.SHOAL_STATUS_CLOSED
	case errors.Is(err, publichdfs.ErrClosed):
		return C.SHOAL_STATUS_CLOSED
	case errors.Is(err, publicrfile.ErrNoTop):
		return C.SHOAL_STATUS_NOT_FOUND
	case errors.Is(err, publicrfile.ErrInvalidSeekable),
		errors.Is(err, publicrfile.ErrInvalidPath),
		errors.Is(err, publicrfile.ErrOutOfOrder),
		errors.Is(err, publicrfile.ErrInvalidLocalityGroup),
		errors.Is(err, publichdfs.ErrInvalidArgument),
		errors.Is(err, accumulo.ErrVisibilityParse):
		return C.SHOAL_STATUS_INVALID_ARGUMENT
	case errors.Is(err, publicrfile.ErrUnsupportedCodec):
		return C.SHOAL_STATUS_UNSUPPORTED
	case errors.Is(err, storage.ErrNotFound):
		return C.SHOAL_STATUS_NOT_FOUND
	case errors.Is(err, os.ErrNotExist):
		return C.SHOAL_STATUS_NOT_FOUND
	case errors.Is(err, os.ErrPermission):
		return C.SHOAL_STATUS_PERMISSION_DENIED
	case errors.Is(err, accumulo.ErrBatchWriterClosed):
		return C.SHOAL_STATUS_CLOSED
	case errors.Is(err, accumulo.ErrBatchWriterRetryExhausted):
		return C.SHOAL_STATUS_RETRY_EXHAUSTED
	case hasMutationRejection(err):
		return C.SHOAL_STATUS_MUTATION_REJECTED
	case errors.Is(err, accumulo.ErrTableNotFound):
		return C.SHOAL_STATUS_NOT_FOUND
	case errors.Is(err, accumulo.ErrTableExists):
		return C.SHOAL_STATUS_ALREADY_EXISTS
	case errors.Is(err, accumulo.ErrNamespaceExists),
		errors.Is(err, accumulo.ErrUserExists):
		return C.SHOAL_STATUS_ALREADY_EXISTS
	case errors.Is(err, accumulo.ErrInvalidTableName),
		errors.Is(err, accumulo.ErrInvalidProperty),
		errors.Is(err, accumulo.ErrInvalidTableRange),
		errors.Is(err, accumulo.ErrInvalidNamespaceName),
		errors.Is(err, accumulo.ErrInvalidTableSplit),
		errors.Is(err, accumulo.ErrInvalidUser),
		errors.Is(err, accumulo.ErrInvalidPassword),
		errors.Is(err, accumulo.ErrInvalidAuthorizations),
		errors.Is(err, accumulo.ErrInvalidPermission):
		return C.SHOAL_STATUS_INVALID_ARGUMENT
	case errors.Is(err, accumulo.ErrConstraintNumberUnavailable):
		return C.SHOAL_STATUS_RETRY_EXHAUSTED
	case errors.Is(err, accumulo.ErrNamespaceNotFound):
		return C.SHOAL_STATUS_NOT_FOUND
	case errors.Is(err, accumulo.ErrNamespaceNotEmpty):
		return C.SHOAL_STATUS_NAMESPACE_NOT_EMPTY
	case errors.Is(err, accumulo.ErrUserNotFound):
		return C.SHOAL_STATUS_USER_NOT_FOUND
	case errors.Is(err, accumulo.ErrBadCredentials):
		return C.SHOAL_STATUS_BAD_CREDENTIALS
	case errors.Is(err, accumulo.ErrPermissionDenied):
		return C.SHOAL_STATUS_PERMISSION_DENIED
	case errors.Is(err, accumulo.ErrUnsupportedOperation):
		return C.SHOAL_STATUS_UNSUPPORTED
	case errors.Is(err, accumulo.ErrSecurityUnavailable):
		return C.SHOAL_STATUS_SECURITY_UNAVAILABLE
	case errors.Is(err, accumulo.ErrTableOffline):
		return C.SHOAL_STATUS_TABLE_OFFLINE
	case errors.Is(err, accumulo.ErrTableSplitsIncomplete):
		return C.SHOAL_STATUS_INCOMPLETE
	case errors.Is(err, accumulo.ErrDiscoveryUnavailable):
		return C.SHOAL_STATUS_DISCOVERY_UNAVAILABLE
	case errors.Is(err, accumulo.ErrManagerUnavailable),
		errors.Is(err, accumulo.ErrClientServiceUnavailable):
		return C.SHOAL_STATUS_UNAVAILABLE
	case errors.Is(err, accumulo.ErrTabletNotLocated),
		errors.Is(err, accumulo.ErrNoTabletCoversRow):
		return C.SHOAL_STATUS_TABLET_UNAVAILABLE
	case errors.Is(err, accumulo.ErrRangeSpansTablets):
		return C.SHOAL_STATUS_RANGE_SPANS_TABLETS
	case onlyCleanupErrors(err):
		return C.SHOAL_STATUS_CLEANUP_FAILED
	default:
		return C.SHOAL_STATUS_OPERATION_FAILED
	}
}

func hasMutationRejection(err error) bool {
	var rejection *accumulo.MutationRejectionError
	return errors.As(err, &rejection)
}

func onlyCleanupErrors(err error) bool {
	if err == nil {
		return false
	}
	var cleanup *accumulo.CleanupError
	if errors.As(err, &cleanup) {
		if joined, ok := err.(interface{ Unwrap() []error }); ok {
			children := joined.Unwrap()
			if len(children) == 0 {
				return false
			}
			for _, child := range children {
				if !onlyCleanupErrors(child) {
					return false
				}
			}
		}
		return true
	}
	return false
}
