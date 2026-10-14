package main

/*
#cgo CFLAGS: -I${SRCDIR}/../../capi/include
#include "bridge.h"
*/
import "C"

import (
	"errors"
	"sync"

	"github.com/phrocker/shoal-oss/accumulo"
)

type ownedKeyState struct {
	mu  sync.RWMutex
	key accumulo.Key
}

func (state *ownedKeyState) snapshot() accumulo.Key {
	state.mu.RLock()
	defer state.mu.RUnlock()
	return state.key.Clone()
}

func (state *ownedKeyState) mutate(mutate func(*accumulo.Key)) {
	state.mu.Lock()
	defer state.mu.Unlock()
	mutate(&state.key)
}

var ownedKeys = newRFileRegistry[ownedKeyState]()

func addOwnedKey(key accumulo.Key, out **C.shoal_owned_key, outError **C.shoal_error) C.shoal_status {
	state := &ownedKeyState{key: key.Clone()}
	id, ok := ownedKeys.add(state)
	if !ok {
		return fail(outError, C.SHOAL_STATUS_INTERNAL, errors.New("shoal: owned key handle space exhausted"))
	}
	handle := C.shoal_bridge_owned_key_alloc(C.uint64_t(id))
	if handle == nil {
		ownedKeys.remove(id)
		return fail(outError, C.SHOAL_STATUS_OUT_OF_MEMORY, errors.New("shoal: allocate owned key handle"))
	}
	*out = handle
	return C.SHOAL_STATUS_OK
}

func lookupOwnedKey(handle *C.shoal_owned_key) (*ownedKeyState, error) {
	if handle == nil {
		return nil, errors.New("shoal: owned key handle is NULL")
	}
	id := uint64(C.shoal_bridge_owned_key_id(handle))
	value, ok := ownedKeys.get(id)
	if id == 0 || !ok {
		return nil, errors.New("shoal: owned key handle is unknown or freed")
	}
	return value, nil
}

func ownedKeySnapshot(handle *C.shoal_owned_key) (accumulo.Key, error) {
	value, err := lookupOwnedKey(handle)
	if err != nil {
		return accumulo.Key{}, err
	}
	return value.snapshot(), nil
}

//export shoal_owned_key_create
func shoal_owned_key_create(row C.shoal_bytes, out **C.shoal_owned_key, outError **C.shoal_error) (status C.shoal_status) {
	clearError(outError)
	if out != nil {
		*out = nil
	}
	defer recoverStatus(&status, outError)
	if out == nil {
		return fail(outError, C.SHOAL_STATUS_INVALID_ARGUMENT, errors.New("shoal: out_key is required"))
	}
	value, err := copyByteValue(row, "key row")
	if err != nil {
		return fail(outError, C.SHOAL_STATUS_INVALID_ARGUMENT, err)
	}
	return addOwnedKey(accumulo.NewKey(value), out, outError)
}

//export shoal_owned_key_create_full
func shoal_owned_key_create_full(row, family, qualifier, visibility C.shoal_bytes, timestamp C.int64_t, out **C.shoal_owned_key, outError **C.shoal_error) (status C.shoal_status) {
	clearError(outError)
	if out != nil {
		*out = nil
	}
	defer recoverStatus(&status, outError)
	if out == nil {
		return fail(outError, C.SHOAL_STATUS_INVALID_ARGUMENT, errors.New("shoal: out_key is required"))
	}
	values := make([][]byte, 4)
	inputs := []C.shoal_bytes{row, family, qualifier, visibility}
	names := []string{"key row", "key column family", "key column qualifier", "key column visibility"}
	var err error
	for index := range inputs {
		values[index], err = copyByteValue(inputs[index], names[index])
		if err != nil {
			return fail(outError, C.SHOAL_STATUS_INVALID_ARGUMENT, err)
		}
	}
	return addOwnedKey(accumulo.NewKeyWithColumns(values[0], values[1], values[2], values[3], int64(timestamp)), out, outError)
}

//export shoal_owned_key_clone
func shoal_owned_key_clone(handle *C.shoal_owned_key, out **C.shoal_owned_key, outError **C.shoal_error) (status C.shoal_status) {
	clearError(outError)
	if out != nil {
		*out = nil
	}
	defer recoverStatus(&status, outError)
	if out == nil {
		return fail(outError, C.SHOAL_STATUS_INVALID_ARGUMENT, errors.New("shoal: out_key is required"))
	}
	key, err := ownedKeySnapshot(handle)
	if err != nil {
		return fail(outError, C.SHOAL_STATUS_INVALID_HANDLE, err)
	}
	return addOwnedKey(key, out, outError)
}

func mutateOwnedKey(handle *C.shoal_owned_key, outError **C.shoal_error, mutate func(*accumulo.Key)) C.shoal_status {
	value, err := lookupOwnedKey(handle)
	if err != nil {
		return fail(outError, C.SHOAL_STATUS_INVALID_HANDLE, err)
	}
	value.mutate(mutate)
	return C.SHOAL_STATUS_OK
}

func setOwnedKeyBytes(handle *C.shoal_owned_key, input C.shoal_bytes, name string, outError **C.shoal_error, mutate func(*accumulo.Key, []byte)) C.shoal_status {
	value, err := copyByteValue(input, name)
	if err != nil {
		return fail(outError, C.SHOAL_STATUS_INVALID_ARGUMENT, err)
	}
	return mutateOwnedKey(handle, outError, func(key *accumulo.Key) { mutate(key, value) })
}

//export shoal_owned_key_set_row
func shoal_owned_key_set_row(handle *C.shoal_owned_key, input C.shoal_bytes, outError **C.shoal_error) (status C.shoal_status) {
	clearError(outError)
	defer recoverStatus(&status, outError)
	return setOwnedKeyBytes(handle, input, "key row", outError, func(key *accumulo.Key, value []byte) { key.SetRow(value) })
}

//export shoal_owned_key_set_column_family
func shoal_owned_key_set_column_family(handle *C.shoal_owned_key, input C.shoal_bytes, outError **C.shoal_error) (status C.shoal_status) {
	clearError(outError)
	defer recoverStatus(&status, outError)
	return setOwnedKeyBytes(handle, input, "key column family", outError, func(key *accumulo.Key, value []byte) { key.SetColumnFamily(value) })
}

//export shoal_owned_key_set_column_qualifier
func shoal_owned_key_set_column_qualifier(handle *C.shoal_owned_key, input C.shoal_bytes, outError **C.shoal_error) (status C.shoal_status) {
	clearError(outError)
	defer recoverStatus(&status, outError)
	return setOwnedKeyBytes(handle, input, "key column qualifier", outError, func(key *accumulo.Key, value []byte) { key.SetColumnQualifier(value) })
}

//export shoal_owned_key_set_column_visibility
func shoal_owned_key_set_column_visibility(handle *C.shoal_owned_key, input C.shoal_bytes, outError **C.shoal_error) (status C.shoal_status) {
	clearError(outError)
	defer recoverStatus(&status, outError)
	return setOwnedKeyBytes(handle, input, "key column visibility", outError, func(key *accumulo.Key, value []byte) { key.SetColumnVisibility(value) })
}

func ownedKeyBytes(handle *C.shoal_owned_key, out **C.shoal_bytes_result, outError **C.shoal_error, get func(accumulo.Key) []byte) C.shoal_status {
	if out != nil {
		*out = nil
	}
	if out == nil {
		return fail(outError, C.SHOAL_STATUS_INVALID_ARGUMENT, errors.New("shoal: out_result is required"))
	}
	key, err := ownedKeySnapshot(handle)
	if err != nil {
		return fail(outError, C.SHOAL_STATUS_INVALID_HANDLE, err)
	}
	return allocValueBytes(string(get(key)), out, outError)
}

//export shoal_owned_key_row
func shoal_owned_key_row(handle *C.shoal_owned_key, out **C.shoal_bytes_result, outError **C.shoal_error) (status C.shoal_status) {
	clearError(outError)
	defer recoverStatus(&status, outError)
	return ownedKeyBytes(handle, out, outError, func(key accumulo.Key) []byte { return key.Row })
}

//export shoal_owned_key_column_family
func shoal_owned_key_column_family(handle *C.shoal_owned_key, out **C.shoal_bytes_result, outError **C.shoal_error) (status C.shoal_status) {
	clearError(outError)
	defer recoverStatus(&status, outError)
	return ownedKeyBytes(handle, out, outError, func(key accumulo.Key) []byte { return key.ColumnFamily })
}

//export shoal_owned_key_column_qualifier
func shoal_owned_key_column_qualifier(handle *C.shoal_owned_key, out **C.shoal_bytes_result, outError **C.shoal_error) (status C.shoal_status) {
	clearError(outError)
	defer recoverStatus(&status, outError)
	return ownedKeyBytes(handle, out, outError, func(key accumulo.Key) []byte { return key.ColumnQualifier })
}

//export shoal_owned_key_column_visibility
func shoal_owned_key_column_visibility(handle *C.shoal_owned_key, out **C.shoal_bytes_result, outError **C.shoal_error) (status C.shoal_status) {
	clearError(outError)
	defer recoverStatus(&status, outError)
	return ownedKeyBytes(handle, out, outError, func(key accumulo.Key) []byte { return key.ColumnVisibility })
}

//export shoal_owned_key_set_timestamp
func shoal_owned_key_set_timestamp(handle *C.shoal_owned_key, timestamp C.int64_t, outError **C.shoal_error) (status C.shoal_status) {
	clearError(outError)
	defer recoverStatus(&status, outError)
	return mutateOwnedKey(handle, outError, func(key *accumulo.Key) { key.SetTimestamp(int64(timestamp)) })
}

//export shoal_owned_key_set_deleted
func shoal_owned_key_set_deleted(handle *C.shoal_owned_key, deleted C.uint8_t, outError **C.shoal_error) (status C.shoal_status) {
	clearError(outError)
	defer recoverStatus(&status, outError)
	if deleted > 1 {
		return fail(outError, C.SHOAL_STATUS_INVALID_ARGUMENT, errors.New("shoal: deleted must be 0 or 1"))
	}
	return mutateOwnedKey(handle, outError, func(key *accumulo.Key) { key.SetDeleted(deleted != 0) })
}

func ownedKeyScalar(handle *C.shoal_owned_key, outError **C.shoal_error, set func(accumulo.Key)) C.shoal_status {
	key, err := ownedKeySnapshot(handle)
	if err != nil {
		return fail(outError, C.SHOAL_STATUS_INVALID_HANDLE, err)
	}
	set(key)
	return C.SHOAL_STATUS_OK
}

//export shoal_owned_key_timestamp
func shoal_owned_key_timestamp(handle *C.shoal_owned_key, out *C.int64_t, outError **C.shoal_error) (status C.shoal_status) {
	clearError(outError)
	if out != nil {
		*out = 0
	}
	defer recoverStatus(&status, outError)
	if out == nil {
		return fail(outError, C.SHOAL_STATUS_INVALID_ARGUMENT, errors.New("shoal: out_timestamp is required"))
	}
	return ownedKeyScalar(handle, outError, func(key accumulo.Key) { *out = C.int64_t(key.Timestamp) })
}

func ownedKeyBool(handle *C.shoal_owned_key, out *C.uint8_t, outError **C.shoal_error, get func(accumulo.Key) bool) C.shoal_status {
	if out != nil {
		*out = 0
	}
	if out == nil {
		return fail(outError, C.SHOAL_STATUS_INVALID_ARGUMENT, errors.New("shoal: out_value is required"))
	}
	return ownedKeyScalar(handle, outError, func(key accumulo.Key) { *out = boolToCUint8(get(key)) })
}

//export shoal_owned_key_empty
func shoal_owned_key_empty(handle *C.shoal_owned_key, out *C.uint8_t, outError **C.shoal_error) (status C.shoal_status) {
	clearError(outError)
	defer recoverStatus(&status, outError)
	return ownedKeyBool(handle, out, outError, func(key accumulo.Key) bool { return key.Empty() })
}

//export shoal_owned_key_is_deleted
func shoal_owned_key_is_deleted(handle *C.shoal_owned_key, out *C.uint8_t, outError **C.shoal_error) (status C.shoal_status) {
	clearError(outError)
	defer recoverStatus(&status, outError)
	return ownedKeyBool(handle, out, outError, func(key accumulo.Key) bool { return key.Deleted })
}

func compareOwnedKeyStates(left, right *ownedKeyState, compare func(accumulo.Key, accumulo.Key) int) int {
	leftKey := left.snapshot()
	if left == right {
		return compare(leftKey, leftKey)
	}
	return compare(leftKey, right.snapshot())
}

func compareOwnedKeys(leftHandle, rightHandle *C.shoal_owned_key, outError **C.shoal_error, compare func(accumulo.Key, accumulo.Key) int) (int, C.shoal_status) {
	left, err := lookupOwnedKey(leftHandle)
	if err != nil {
		return 0, fail(outError, C.SHOAL_STATUS_INVALID_HANDLE, err)
	}
	right, err := lookupOwnedKey(rightHandle)
	if err != nil {
		return 0, fail(outError, C.SHOAL_STATUS_INVALID_HANDLE, err)
	}
	return compareOwnedKeyStates(left, right, compare), C.SHOAL_STATUS_OK
}

//export shoal_owned_key_compare
func shoal_owned_key_compare(left, right *C.shoal_owned_key, out *C.int32_t, outError **C.shoal_error) (status C.shoal_status) {
	clearError(outError)
	if out != nil {
		*out = 0
	}
	defer recoverStatus(&status, outError)
	if out == nil {
		return fail(outError, C.SHOAL_STATUS_INVALID_ARGUMENT, errors.New("shoal: out_order is required"))
	}
	order, code := compareOwnedKeys(left, right, outError, func(a, b accumulo.Key) int { return a.Compare(b) })
	if code == C.SHOAL_STATUS_OK {
		*out = C.int32_t(order)
	}
	return code
}

//export shoal_owned_key_compare_visibility
func shoal_owned_key_compare_visibility(left, right *C.shoal_owned_key, out *C.int32_t, outError **C.shoal_error) (status C.shoal_status) {
	clearError(outError)
	if out != nil {
		*out = 0
	}
	defer recoverStatus(&status, outError)
	if out == nil {
		return fail(outError, C.SHOAL_STATUS_INVALID_ARGUMENT, errors.New("shoal: out_order is required"))
	}
	order, code := compareOwnedKeys(left, right, outError, func(a, b accumulo.Key) int { return a.CompareToVisibility(b) })
	if code == C.SHOAL_STATUS_OK {
		*out = C.int32_t(order)
	}
	return code
}

func ownedKeyComparison(left, right *C.shoal_owned_key, out *C.uint8_t, outError **C.shoal_error, predicate func(int) bool) C.shoal_status {
	if out != nil {
		*out = 0
	}
	if out == nil {
		return fail(outError, C.SHOAL_STATUS_INVALID_ARGUMENT, errors.New("shoal: out_value is required"))
	}
	order, code := compareOwnedKeys(left, right, outError, func(a, b accumulo.Key) int { return a.Compare(b) })
	if code == C.SHOAL_STATUS_OK {
		*out = boolToCUint8(predicate(order))
	}
	return code
}

//export shoal_owned_key_less
func shoal_owned_key_less(left, right *C.shoal_owned_key, out *C.uint8_t, outError **C.shoal_error) (status C.shoal_status) {
	clearError(outError)
	defer recoverStatus(&status, outError)
	return ownedKeyComparison(left, right, out, outError, func(order int) bool { return order < 0 })
}

//export shoal_owned_key_less_or_equal
func shoal_owned_key_less_or_equal(left, right *C.shoal_owned_key, out *C.uint8_t, outError **C.shoal_error) (status C.shoal_status) {
	clearError(outError)
	defer recoverStatus(&status, outError)
	return ownedKeyComparison(left, right, out, outError, func(order int) bool { return order <= 0 })
}

//export shoal_owned_key_equal
func shoal_owned_key_equal(left, right *C.shoal_owned_key, out *C.uint8_t, outError **C.shoal_error) (status C.shoal_status) {
	clearError(outError)
	defer recoverStatus(&status, outError)
	return ownedKeyComparison(left, right, out, outError, func(order int) bool { return order == 0 })
}

//export shoal_owned_key_not_equal
func shoal_owned_key_not_equal(left, right *C.shoal_owned_key, out *C.uint8_t, outError **C.shoal_error) (status C.shoal_status) {
	clearError(outError)
	defer recoverStatus(&status, outError)
	return ownedKeyComparison(left, right, out, outError, func(order int) bool { return order != 0 })
}

func ownedKeySize(handle *C.shoal_owned_key, out *C.size_t, outError **C.shoal_error, get func(accumulo.Key) int) C.shoal_status {
	if out != nil {
		*out = 0
	}
	if out == nil {
		return fail(outError, C.SHOAL_STATUS_INVALID_ARGUMENT, errors.New("shoal: out_size is required"))
	}
	return ownedKeyScalar(handle, outError, func(key accumulo.Key) { *out = C.size_t(get(key)) })
}

//export shoal_owned_key_size
func shoal_owned_key_size(handle *C.shoal_owned_key, out *C.size_t, outError **C.shoal_error) (status C.shoal_status) {
	clearError(outError)
	defer recoverStatus(&status, outError)
	return ownedKeySize(handle, out, outError, func(key accumulo.Key) int { return key.Size() })
}

//export shoal_owned_key_length
func shoal_owned_key_length(handle *C.shoal_owned_key, out *C.size_t, outError **C.shoal_error) (status C.shoal_status) {
	clearError(outError)
	defer recoverStatus(&status, outError)
	return ownedKeySize(handle, out, outError, func(key accumulo.Key) int { return key.Length() })
}

//export shoal_owned_key_row_size
func shoal_owned_key_row_size(handle *C.shoal_owned_key, out *C.size_t, outError **C.shoal_error) (status C.shoal_status) {
	clearError(outError)
	defer recoverStatus(&status, outError)
	return ownedKeySize(handle, out, outError, func(key accumulo.Key) int { return key.RowSize() })
}

//export shoal_owned_key_column_family_size
func shoal_owned_key_column_family_size(handle *C.shoal_owned_key, out *C.size_t, outError **C.shoal_error) (status C.shoal_status) {
	clearError(outError)
	defer recoverStatus(&status, outError)
	return ownedKeySize(handle, out, outError, func(key accumulo.Key) int { return key.ColumnFamilySize() })
}

//export shoal_owned_key_column_qualifier_size
func shoal_owned_key_column_qualifier_size(handle *C.shoal_owned_key, out *C.size_t, outError **C.shoal_error) (status C.shoal_status) {
	clearError(outError)
	defer recoverStatus(&status, outError)
	return ownedKeySize(handle, out, outError, func(key accumulo.Key) int { return key.ColumnQualifierSize() })
}

//export shoal_owned_key_column_visibility_size
func shoal_owned_key_column_visibility_size(handle *C.shoal_owned_key, out *C.size_t, outError **C.shoal_error) (status C.shoal_status) {
	clearError(outError)
	defer recoverStatus(&status, outError)
	return ownedKeySize(handle, out, outError, func(key accumulo.Key) int { return key.ColumnVisibilitySize() })
}

//export shoal_owned_key_free
func shoal_owned_key_free(handle **C.shoal_owned_key) {
	if handle == nil || *handle == nil {
		return
	}
	id := uint64(C.shoal_bridge_owned_key_id(*handle))
	ownedKeys.remove(id)
	C.shoal_bridge_owned_key_free(*handle)
	*handle = nil
}
