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
	"unsafe"

	"github.com/phrocker/shoal-oss/accumulo"
)

var authorizationValues = newRFileRegistry[accumulo.Authorizations]()

//export shoal_key_value_init
func shoal_key_value_init(value *C.shoal_key_value) {
	if value != nil {
		C.memset(unsafe.Pointer(value), 0, C.size_t(C.SHOAL_KEY_VALUE_V1_SIZE))
		value.struct_size = C.SHOAL_KEY_VALUE_V1_SIZE
	}
}

func allocValueBytes(value string, out **C.shoal_bytes_result, outError **C.shoal_error) C.shoal_status {
	if out != nil {
		*out = nil
	}
	if out == nil {
		return fail(outError, C.SHOAL_STATUS_INVALID_ARGUMENT, errors.New("shoal: out_result is required"))
	}
	result, code, err := allocBytes(value)
	if err != nil {
		return fail(outError, code, err)
	}
	*out = result
	return C.SHOAL_STATUS_OK
}

//export shoal_key_to_string
func shoal_key_to_string(input *C.shoal_key, out **C.shoal_bytes_result, outError **C.shoal_error) (status C.shoal_status) {
	clearError(outError)
	defer recoverStatus(&status, outError)
	if input == nil {
		return fail(outError, C.SHOAL_STATUS_INVALID_ARGUMENT, errors.New("shoal: key is required"))
	}
	key, err := parseKey(*input, "key")
	if err != nil {
		return fail(outError, C.SHOAL_STATUS_INVALID_ARGUMENT, err)
	}
	return allocValueBytes(key.String(), out, outError)
}

func rangePredicate(input *C.shoal_range, keyInput *C.shoal_key, out *C.uint8_t, outError **C.shoal_error, after bool) C.shoal_status {
	if out != nil {
		*out = 0
	}
	if out == nil {
		return fail(outError, C.SHOAL_STATUS_INVALID_ARGUMENT, errors.New("shoal: out_value is required"))
	}
	keyRange, err := parseRange(input)
	if err != nil {
		return fail(outError, C.SHOAL_STATUS_INVALID_ARGUMENT, err)
	}
	if keyInput == nil {
		return fail(outError, C.SHOAL_STATUS_INVALID_ARGUMENT, errors.New("shoal: key is required"))
	}
	key, err := parseKey(*keyInput, "key")
	if err != nil {
		return fail(outError, C.SHOAL_STATUS_INVALID_ARGUMENT, err)
	}
	if after {
		*out = boolToCUint8(keyRange.AfterEndKey(*key))
	} else {
		*out = boolToCUint8(keyRange.BeforeStartKey(*key))
	}
	return C.SHOAL_STATUS_OK
}

//export shoal_range_after_end_key
func shoal_range_after_end_key(input *C.shoal_range, key *C.shoal_key, out *C.uint8_t, outError **C.shoal_error) (status C.shoal_status) {
	clearError(outError)
	defer recoverStatus(&status, outError)
	return rangePredicate(input, key, out, outError, true)
}

//export shoal_range_before_start_key
func shoal_range_before_start_key(input *C.shoal_range, key *C.shoal_key, out *C.uint8_t, outError **C.shoal_error) (status C.shoal_status) {
	clearError(outError)
	defer recoverStatus(&status, outError)
	return rangePredicate(input, key, out, outError, false)
}

//export shoal_range_to_string
func shoal_range_to_string(input *C.shoal_range, out **C.shoal_bytes_result, outError **C.shoal_error) (status C.shoal_status) {
	clearError(outError)
	defer recoverStatus(&status, outError)
	keyRange, err := parseRange(input)
	if err != nil {
		return fail(outError, C.SHOAL_STATUS_INVALID_ARGUMENT, err)
	}
	return allocValueBytes(keyRange.String(), out, outError)
}

//export shoal_key_value_create
func shoal_key_value_create(input *C.shoal_key_value, out **C.shoal_key_value_result, outError **C.shoal_error) (status C.shoal_status) {
	clearError(outError)
	if out != nil {
		*out = nil
	}
	defer recoverStatus(&status, outError)
	if out == nil {
		return fail(outError, C.SHOAL_STATUS_INVALID_ARGUMENT, errors.New("shoal: out_result is required"))
	}
	if input == nil || input.struct_size < C.SHOAL_KEY_VALUE_V1_SIZE {
		return fail(outError, C.SHOAL_STATUS_INVALID_ARGUMENT, errors.New("shoal: key/value input is missing or too small"))
	}
	key, err := parseKey(input.key, "key/value key")
	if err != nil {
		return fail(outError, C.SHOAL_STATUS_INVALID_ARGUMENT, err)
	}
	value, err := copyByteValue(input.value, "key/value value")
	if err != nil {
		return fail(outError, C.SHOAL_STATUS_INVALID_ARGUMENT, err)
	}
	result := C.shoal_bridge_key_value_alloc(
		bytePointer(key.Row), C.size_t(len(key.Row)),
		bytePointer(key.ColumnFamily), C.size_t(len(key.ColumnFamily)),
		bytePointer(key.ColumnQualifier), C.size_t(len(key.ColumnQualifier)),
		bytePointer(key.ColumnVisibility), C.size_t(len(key.ColumnVisibility)),
		C.int64_t(key.Timestamp), bytePointer(value), C.size_t(len(value)),
	)
	if result == nil {
		return fail(outError, C.SHOAL_STATUS_OUT_OF_MEMORY, errors.New("shoal: allocate key/value result"))
	}
	*out = result
	return C.SHOAL_STATUS_OK
}

//export shoal_key_value_result_get
func shoal_key_value_result_get(result *C.shoal_key_value_result, out *C.shoal_key_value_view, outError **C.shoal_error) (status C.shoal_status) {
	clearError(outError)
	defer recoverStatus(&status, outError)
	if C.shoal_bridge_key_value_get(result, out) == 0 {
		return fail(outError, C.SHOAL_STATUS_INVALID_ARGUMENT, errors.New("shoal: invalid key/value result access"))
	}
	return C.SHOAL_STATUS_OK
}

//export shoal_key_value_result_free
func shoal_key_value_result_free(result **C.shoal_key_value_result) {
	if result != nil && *result != nil {
		C.shoal_bridge_key_value_free(*result)
		*result = nil
	}
}

func lookupAuthorizations(handle *C.shoal_authorizations) (*accumulo.Authorizations, error) {
	if handle == nil {
		return nil, errors.New("shoal: authorizations handle is NULL")
	}
	id := uint64(C.shoal_bridge_authorizations_id(handle))
	value, ok := authorizationValues.get(id)
	if id == 0 || !ok {
		return nil, errors.New("shoal: authorizations handle is unknown or freed")
	}
	return value, nil
}

//export shoal_authorizations_create
func shoal_authorizations_create(labels *C.shoal_bytes, count C.size_t, out **C.shoal_authorizations, outError **C.shoal_error) (status C.shoal_status) {
	clearError(outError)
	if out != nil {
		*out = nil
	}
	defer recoverStatus(&status, outError)
	if out == nil {
		return fail(outError, C.SHOAL_STATUS_INVALID_ARGUMENT, errors.New("shoal: out_authorizations is required"))
	}
	length, err := arrayLength(count, labels != nil, "authorization labels")
	if err != nil {
		return fail(outError, C.SHOAL_STATUS_INVALID_ARGUMENT, err)
	}
	inputs := unsafe.Slice(labels, length)
	values := make([][]byte, length)
	for index := range inputs {
		values[index], err = copyByteValue(inputs[index], fmt.Sprintf("authorization label %d", index))
		if err != nil {
			return fail(outError, C.SHOAL_STATUS_INVALID_ARGUMENT, err)
		}
	}
	authorizations := accumulo.NewAuthorizations(values...)
	id, ok := authorizationValues.add(authorizations)
	if !ok {
		return fail(outError, C.SHOAL_STATUS_INTERNAL, errors.New("shoal: authorizations handle space exhausted"))
	}
	handle := C.shoal_bridge_authorizations_alloc(C.uint64_t(id))
	if handle == nil {
		authorizationValues.remove(id)
		return fail(outError, C.SHOAL_STATUS_OUT_OF_MEMORY, errors.New("shoal: allocate authorizations handle"))
	}
	*out = handle
	return C.SHOAL_STATUS_OK
}

//export shoal_authorizations_contains
func shoal_authorizations_contains(handle *C.shoal_authorizations, label C.shoal_bytes) C.uint8_t {
	authorizations, err := lookupAuthorizations(handle)
	if err != nil {
		return 0
	}
	value, err := copyByteValue(label, "authorization label")
	if err != nil {
		return 0
	}
	return boolToCUint8(authorizations.Contains(value))
}

//export shoal_authorizations_count
func shoal_authorizations_count(handle *C.shoal_authorizations) C.size_t {
	authorizations, err := lookupAuthorizations(handle)
	if err != nil {
		return 0
	}
	return C.size_t(authorizations.Len())
}

//export shoal_authorizations_list
func shoal_authorizations_list(handle *C.shoal_authorizations, out **C.shoal_bytes_list_result, outError **C.shoal_error) (status C.shoal_status) {
	clearError(outError)
	if out != nil {
		*out = nil
	}
	defer recoverStatus(&status, outError)
	if out == nil {
		return fail(outError, C.SHOAL_STATUS_INVALID_ARGUMENT, errors.New("shoal: out_result is required"))
	}
	authorizations, err := lookupAuthorizations(handle)
	if err != nil {
		return fail(outError, C.SHOAL_STATUS_INVALID_HANDLE, err)
	}
	values := authorizations.List()
	result := C.shoal_bridge_bytes_list_alloc(C.size_t(len(values)))
	if result == nil {
		return fail(outError, C.SHOAL_STATUS_OUT_OF_MEMORY, errors.New("shoal: allocate authorization list"))
	}
	for index, value := range values {
		if C.shoal_bridge_bytes_list_set(result, C.size_t(index), bytePointer(value), C.size_t(len(value))) == 0 {
			C.shoal_bridge_bytes_list_free(result)
			return fail(outError, C.SHOAL_STATUS_OUT_OF_MEMORY, errors.New("shoal: copy authorization list"))
		}
	}
	*out = result
	return C.SHOAL_STATUS_OK
}

//export shoal_authorizations_empty
func shoal_authorizations_empty(handle *C.shoal_authorizations) C.uint8_t {
	authorizations, err := lookupAuthorizations(handle)
	if err != nil {
		return 0
	}
	return boolToCUint8(authorizations.Empty())
}

//export shoal_authorizations_equal
func shoal_authorizations_equal(leftHandle *C.shoal_authorizations, rightHandle *C.shoal_authorizations) C.uint8_t {
	left, err := lookupAuthorizations(leftHandle)
	if err != nil {
		return 0
	}
	right, err := lookupAuthorizations(rightHandle)
	if err != nil {
		return 0
	}
	return boolToCUint8(left.Equal(right))
}

//export shoal_authorization_character_is_valid
func shoal_authorization_character_is_valid(character C.uint8_t) C.uint8_t {
	return boolToCUint8(accumulo.ValidAuthorizationCharacter(byte(character)))
}

//export shoal_authorizations_free
func shoal_authorizations_free(handle **C.shoal_authorizations) {
	if handle == nil || *handle == nil {
		return
	}
	value := *handle
	*handle = nil
	authorizationValues.remove(uint64(C.shoal_bridge_authorizations_id(value)))
	C.shoal_bridge_authorizations_free(value)
}
