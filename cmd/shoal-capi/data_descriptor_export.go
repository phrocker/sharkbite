package main

/*
#cgo CFLAGS: -I${SRCDIR}/../../capi/include
#include "bridge.h"
#include <string.h>
*/
import "C"

import (
	"errors"
	"fmt"
	"sort"
	"unsafe"

	"github.com/phrocker/shoal-oss/accumulo"
)

//export shoal_range_view_init
func shoal_range_view_init(view *C.shoal_range_view) {
	if view == nil {
		return
	}
	C.memset(unsafe.Pointer(view), 0, C.size_t(C.SHOAL_RANGE_VIEW_V1_SIZE))
	view.struct_size = C.SHOAL_RANGE_VIEW_V1_SIZE
}

//export shoal_iterator_setting_view_init
func shoal_iterator_setting_view_init(view *C.shoal_iterator_setting_view) {
	if view == nil {
		return
	}
	C.memset(unsafe.Pointer(view), 0, C.size_t(C.SHOAL_ITERATOR_SETTING_VIEW_V1_SIZE))
	view.struct_size = C.SHOAL_ITERATOR_SETTING_VIEW_V1_SIZE
}

//export shoal_range_create
func shoal_range_create(input *C.shoal_range, out **C.shoal_range_result, outError **C.shoal_error) (status C.shoal_status) {
	clearError(outError)
	if out != nil {
		*out = nil
	}
	defer recoverStatus(&status, outError)
	if out == nil {
		return fail(outError, C.SHOAL_STATUS_INVALID_ARGUMENT, errors.New("shoal: out_result is required"))
	}
	scanRange, err := parseRange(input)
	if err != nil {
		return fail(outError, C.SHOAL_STATUS_INVALID_ARGUMENT, err)
	}
	result, err := buildRangeResult(
		scanRange,
		rangeBoundKind(input.start.kind),
		rangeBoundKind(input.end.kind),
	)
	if err != nil {
		return fail(outError, C.SHOAL_STATUS_OUT_OF_MEMORY, err)
	}
	*out = result
	return C.SHOAL_STATUS_OK
}

//export shoal_range_get
func shoal_range_get(result *C.shoal_range_result, out *C.shoal_range_view, outError **C.shoal_error) (status C.shoal_status) {
	clearError(outError)
	defer recoverStatus(&status, outError)
	if result == nil {
		return fail(outError, C.SHOAL_STATUS_INVALID_ARGUMENT, errors.New("shoal: range result is NULL"))
	}
	if out == nil {
		return fail(outError, C.SHOAL_STATUS_INVALID_ARGUMENT, errors.New("shoal: out_range is required"))
	}
	if out.struct_size < C.SHOAL_RANGE_VIEW_V1_SIZE {
		return fail(outError, C.SHOAL_STATUS_INVALID_ARGUMENT, fmt.Errorf(
			"shoal: range view struct_size %d is smaller than %d",
			uint64(out.struct_size), uint64(C.SHOAL_RANGE_VIEW_V1_SIZE),
		))
	}
	if C.shoal_bridge_range_result_get(result, out) == 0 {
		return fail(outError, C.SHOAL_STATUS_INVALID_ARGUMENT, errors.New("shoal: invalid range result access"))
	}
	return C.SHOAL_STATUS_OK
}

//export shoal_range_free
func shoal_range_free(result **C.shoal_range_result) {
	if result == nil || *result == nil {
		return
	}
	C.shoal_bridge_range_result_free(*result)
	*result = nil
}

//export shoal_iterator_setting_create
func shoal_iterator_setting_create(input *C.shoal_iterator_setting, out **C.shoal_iterator_setting_result, outError **C.shoal_error) (status C.shoal_status) {
	clearError(outError)
	if out != nil {
		*out = nil
	}
	defer recoverStatus(&status, outError)
	if out == nil {
		return fail(outError, C.SHOAL_STATUS_INVALID_ARGUMENT, errors.New("shoal: out_result is required"))
	}
	if input == nil {
		return fail(outError, C.SHOAL_STATUS_INVALID_ARGUMENT, errors.New("shoal: iterator setting is required"))
	}
	settings, err := parseIterators(input, 1)
	if err != nil {
		return fail(outError, C.SHOAL_STATUS_INVALID_ARGUMENT, err)
	}
	result, err := buildIteratorSettingResult(settings[0])
	if err != nil {
		return fail(outError, C.SHOAL_STATUS_OUT_OF_MEMORY, err)
	}
	*out = result
	return C.SHOAL_STATUS_OK
}

//export shoal_iterator_setting_get
func shoal_iterator_setting_get(result *C.shoal_iterator_setting_result, out *C.shoal_iterator_setting_view, outError **C.shoal_error) (status C.shoal_status) {
	clearError(outError)
	defer recoverStatus(&status, outError)
	if result == nil {
		return fail(outError, C.SHOAL_STATUS_INVALID_ARGUMENT, errors.New("shoal: iterator setting result is NULL"))
	}
	if out == nil {
		return fail(outError, C.SHOAL_STATUS_INVALID_ARGUMENT, errors.New("shoal: out_setting is required"))
	}
	if out.struct_size < C.SHOAL_ITERATOR_SETTING_VIEW_V1_SIZE {
		return fail(outError, C.SHOAL_STATUS_INVALID_ARGUMENT, fmt.Errorf(
			"shoal: iterator setting view struct_size %d is smaller than %d",
			uint64(out.struct_size), uint64(C.SHOAL_ITERATOR_SETTING_VIEW_V1_SIZE),
		))
	}
	if C.shoal_bridge_iterator_setting_result_get(result, out) == 0 {
		return fail(outError, C.SHOAL_STATUS_INVALID_ARGUMENT, errors.New("shoal: invalid iterator setting result access"))
	}
	return C.SHOAL_STATUS_OK
}

//export shoal_iterator_setting_free
func shoal_iterator_setting_free(result **C.shoal_iterator_setting_result) {
	if result == nil || *result == nil {
		return
	}
	C.shoal_bridge_iterator_setting_result_free(*result)
	*result = nil
}

type rangeBoundKind int32

const (
	rangeBoundUnbounded rangeBoundKind = rangeBoundKind(C.SHOAL_RANGE_BOUND_UNBOUNDED)
	rangeBoundRow       rangeBoundKind = rangeBoundKind(C.SHOAL_RANGE_BOUND_ROW)
	rangeBoundKey       rangeBoundKind = rangeBoundKind(C.SHOAL_RANGE_BOUND_KEY)
)

func buildRangeResult(scanRange *accumulo.Range, startKind, endKind rangeBoundKind) (*C.shoal_range_result, error) {
	result := C.shoal_bridge_range_result_alloc()
	if result == nil {
		return nil, errors.New("shoal: allocate range result")
	}
	if start := scanRange.StartKey(); start != nil {
		if !setRangeResultKey(result, true, start) {
			C.shoal_bridge_range_result_free(result)
			return nil, errors.New("shoal: allocate range start key")
		}
	}
	if end := scanRange.EndKey(); end != nil {
		if !setRangeResultKey(result, false, end) {
			C.shoal_bridge_range_result_free(result)
			return nil, errors.New("shoal: allocate range end key")
		}
	}
	C.shoal_bridge_range_result_set_metadata(
		result,
		C.shoal_range_bound_kind(startKind),
		C.shoal_range_bound_kind(endKind),
		boolToCUint8(scanRange.StartInclusive()),
		boolToCUint8(scanRange.EndInclusive()),
	)
	return result, nil
}

func setRangeResultKey(result *C.shoal_range_result, start bool, key *accumulo.Key) bool {
	var ok C.int
	if start {
		ok = C.shoal_bridge_range_result_set_start(
			result,
			bytePointer(key.Row), C.size_t(len(key.Row)),
			bytePointer(key.ColumnFamily), C.size_t(len(key.ColumnFamily)),
			bytePointer(key.ColumnQualifier), C.size_t(len(key.ColumnQualifier)),
			bytePointer(key.ColumnVisibility), C.size_t(len(key.ColumnVisibility)),
			C.int64_t(key.Timestamp),
		)
	} else {
		ok = C.shoal_bridge_range_result_set_end(
			result,
			bytePointer(key.Row), C.size_t(len(key.Row)),
			bytePointer(key.ColumnFamily), C.size_t(len(key.ColumnFamily)),
			bytePointer(key.ColumnQualifier), C.size_t(len(key.ColumnQualifier)),
			bytePointer(key.ColumnVisibility), C.size_t(len(key.ColumnVisibility)),
			C.int64_t(key.Timestamp),
		)
	}
	return ok != 0
}

func buildIteratorSettingResult(setting accumulo.IteratorSetting) (*C.shoal_iterator_setting_result, error) {
	options := setting.Options()
	keys := make([]string, 0, len(options))
	for key := range options {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	result := C.shoal_bridge_iterator_setting_result_alloc(C.size_t(len(keys)))
	if result == nil {
		return nil, errors.New("shoal: allocate iterator setting result")
	}
	if err := setIteratorSettingIdentity(result, setting); err != nil {
		C.shoal_bridge_iterator_setting_result_free(result)
		return nil, err
	}
	for index, key := range keys {
		if err := setIteratorSettingOption(result, index, key, options[key]); err != nil {
			C.shoal_bridge_iterator_setting_result_free(result)
			return nil, err
		}
	}
	return result, nil
}

func setIteratorSettingIdentity(result *C.shoal_iterator_setting_result, setting accumulo.IteratorSetting) error {
	name, _, err := bridgeCString(setting.Name(), "iterator name")
	if err != nil {
		return err
	}
	defer C.shoal_bridge_string_free(name)
	className, _, err := bridgeCString(setting.ClassName(), "iterator class")
	if err != nil {
		return err
	}
	defer C.shoal_bridge_string_free(className)
	if C.shoal_bridge_iterator_setting_result_set_identity(
		result, name, className, C.int32_t(setting.Priority()),
	) == 0 {
		return errors.New("shoal: allocate iterator setting identity")
	}
	return nil
}

func setIteratorSettingOption(result *C.shoal_iterator_setting_result, index int, key, value string) error {
	cKey, _, err := bridgeCString(key, "iterator option key")
	if err != nil {
		return err
	}
	defer C.shoal_bridge_string_free(cKey)
	cValue, _, err := bridgeCString(value, "iterator option value")
	if err != nil {
		return err
	}
	defer C.shoal_bridge_string_free(cValue)
	if C.shoal_bridge_iterator_setting_result_set_option(
		result, C.size_t(index), cKey, cValue,
	) == 0 {
		return fmt.Errorf("shoal: allocate iterator option %d", index)
	}
	return nil
}

func boolToCUint8(value bool) C.uint8_t {
	if value {
		return 1
	}
	return 0
}

type rangeResultSnapshot struct {
	startKind      rangeBoundKind
	hasStart       bool
	start          accumulo.Key
	endKind        rangeBoundKind
	hasEnd         bool
	end            accumulo.Key
	startInclusive bool
	endInclusive   bool
}

func snapshotRangeResult(result *C.shoal_range_result) (rangeResultSnapshot, error) {
	var view C.shoal_range_view
	var outError *C.shoal_error
	view.struct_size = C.SHOAL_RANGE_VIEW_V1_SIZE
	if shoal_range_get(result, &view, &outError) != C.SHOAL_STATUS_OK {
		message := "shoal: snapshot range result"
		if outError != nil {
			message = C.GoString(C.shoal_bridge_error_message(outError))
			C.shoal_bridge_error_free(outError)
		}
		return rangeResultSnapshot{}, errors.New(message)
	}
	return rangeResultSnapshot{
		startKind:      rangeBoundKind(view.start_kind),
		hasStart:       view.has_start_key != 0,
		start:          goKey(view.start_key),
		endKind:        rangeBoundKind(view.end_kind),
		hasEnd:         view.has_end_key != 0,
		end:            goKey(view.end_key),
		startInclusive: view.start_inclusive != 0,
		endInclusive:   view.end_inclusive != 0,
	}, nil
}

func freeBuiltRangeResult(result *C.shoal_range_result) {
	C.shoal_bridge_range_result_free(result)
}

type iteratorSettingResultSnapshot struct {
	name      string
	className string
	priority  int32
	options   map[string]string
}

func snapshotIteratorSettingResult(result *C.shoal_iterator_setting_result) (iteratorSettingResultSnapshot, error) {
	var view C.shoal_iterator_setting_view
	var outError *C.shoal_error
	view.struct_size = C.SHOAL_ITERATOR_SETTING_VIEW_V1_SIZE
	if shoal_iterator_setting_get(result, &view, &outError) != C.SHOAL_STATUS_OK {
		message := "shoal: snapshot iterator setting result"
		if outError != nil {
			message = C.GoString(C.shoal_bridge_error_message(outError))
			C.shoal_bridge_error_free(outError)
		}
		return iteratorSettingResultSnapshot{}, errors.New(message)
	}
	options := make(map[string]string, int(view.option_count))
	for _, option := range unsafe.Slice(view.options, int(view.option_count)) {
		options[C.GoString(option.key)] = C.GoString(option.value)
	}
	return iteratorSettingResultSnapshot{
		name:      C.GoString(view.name),
		className: C.GoString(view.class_name),
		priority:  int32(view.priority),
		options:   options,
	}, nil
}

func freeBuiltIteratorSettingResult(result *C.shoal_iterator_setting_result) {
	C.shoal_bridge_iterator_setting_result_free(result)
}

func goKey(key C.shoal_key) accumulo.Key {
	return accumulo.Key{
		Row:              copyViewBytes(key.row),
		ColumnFamily:     copyViewBytes(key.column_family),
		ColumnQualifier:  copyViewBytes(key.column_qualifier),
		ColumnVisibility: copyViewBytes(key.column_visibility),
		Timestamp:        int64(key.timestamp),
	}
}

func copyViewBytes(value C.shoal_bytes) []byte {
	if value.length == 0 {
		return nil
	}
	data := (*byte)(unsafe.Pointer(value.data))
	return append([]byte(nil), unsafe.Slice(data, int(value.length))...)
}
