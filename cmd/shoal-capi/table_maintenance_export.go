package main

/*
#cgo CFLAGS: -I${SRCDIR}/../../capi/include
#include "bridge.h"
#include <stdlib.h>
#include <string.h>
*/
import "C"

import (
	"context"
	"errors"
	"fmt"
	"unsafe"

	"github.com/phrocker/shoal-oss/accumulo"
)

type tableMaintenanceAPI interface {
	FlushTableRange(context.Context, string, []byte, []byte, bool) error
	AddConstraint(context.Context, string, string) (int32, error)
	ListConstraints(context.Context, string) ([]accumulo.Constraint, error)
	RemoveConstraint(context.Context, string, int32) error
}

func lookupTableMaintenance(
	handle *C.shoal_connector,
) (*ownedConnector, tableMaintenanceAPI, C.shoal_status, error) {
	connector, err := lookupConnector(handle)
	if err != nil {
		return nil, nil, C.SHOAL_STATUS_INVALID_HANDLE, err
	}
	maintenance, ok := connector.connector.(tableMaintenanceAPI)
	if !ok {
		return nil, nil, C.SHOAL_STATUS_UNSUPPORTED, errors.New("shoal: table maintenance is unsupported")
	}
	return connector, maintenance, C.SHOAL_STATUS_OK, nil
}

func optionalRowBound(input *C.shoal_bytes, name string) ([]byte, error) {
	if input == nil {
		return nil, nil
	}
	value, err := copyByteValue(*input, name)
	if err != nil {
		return nil, err
	}
	if value == nil {
		value = []byte{}
	}
	return value, nil
}

//export shoal_connector_flush_table_range
func shoal_connector_flush_table_range(
	handle *C.shoal_connector,
	tableName *C.char,
	startInput *C.shoal_bytes,
	endInput *C.shoal_bytes,
	wait C.uint8_t,
	timeoutMilliseconds C.int64_t,
	outError **C.shoal_error,
) (status C.shoal_status) {
	clearError(outError)
	defer recoverStatus(&status, outError)
	if wait > 1 {
		return fail(outError, C.SHOAL_STATUS_INVALID_ARGUMENT, errors.New("shoal: wait must be 0 or 1"))
	}
	name, err := requiredString(tableName, "table_name")
	if err != nil {
		return fail(outError, C.SHOAL_STATUS_INVALID_ARGUMENT, err)
	}
	start, err := optionalRowBound(startInput, "start_row")
	if err != nil {
		return fail(outError, C.SHOAL_STATUS_INVALID_ARGUMENT, err)
	}
	end, err := optionalRowBound(endInput, "end_row")
	if err != nil {
		return fail(outError, C.SHOAL_STATUS_INVALID_ARGUMENT, err)
	}
	connector, maintenance, code, err := lookupTableMaintenance(handle)
	if err != nil {
		return fail(outError, code, err)
	}
	ctx, done, code, err := beginConnectorOperation(connector, timeoutMilliseconds)
	if err != nil {
		return fail(outError, code, err)
	}
	err = func() error {
		defer done()
		return maintenance.FlushTableRange(ctx, name, start, end, wait != 0)
	}()
	return failOrOK(outError, err)
}

//export shoal_table_constraint_view_init
func shoal_table_constraint_view_init(view *C.shoal_table_constraint_view) {
	if view != nil {
		C.memset(unsafe.Pointer(view), 0, C.size_t(C.SHOAL_TABLE_CONSTRAINT_VIEW_V1_SIZE))
		view.struct_size = C.SHOAL_TABLE_CONSTRAINT_VIEW_V1_SIZE
	}
}

//export shoal_connector_add_table_constraint
func shoal_connector_add_table_constraint(
	handle *C.shoal_connector,
	tableName *C.char,
	className *C.char,
	timeoutMilliseconds C.int64_t,
	outNumber *C.int32_t,
	outError **C.shoal_error,
) (status C.shoal_status) {
	clearError(outError)
	if outNumber != nil {
		*outNumber = 0
	}
	defer recoverStatus(&status, outError)
	if outNumber == nil {
		return fail(outError, C.SHOAL_STATUS_INVALID_ARGUMENT, errors.New("shoal: out_number is required"))
	}
	name, err := requiredString(tableName, "table_name")
	if err != nil {
		return fail(outError, C.SHOAL_STATUS_INVALID_ARGUMENT, err)
	}
	class, err := requiredString(className, "class_name")
	if err != nil {
		return fail(outError, C.SHOAL_STATUS_INVALID_ARGUMENT, err)
	}
	connector, maintenance, code, err := lookupTableMaintenance(handle)
	if err != nil {
		return fail(outError, code, err)
	}
	ctx, done, code, err := beginConnectorOperation(connector, timeoutMilliseconds)
	if err != nil {
		return fail(outError, code, err)
	}
	number, err := func() (int32, error) {
		defer done()
		return maintenance.AddConstraint(ctx, name, class)
	}()
	if err != nil {
		return failForError(outError, err)
	}
	*outNumber = C.int32_t(number)
	return C.SHOAL_STATUS_OK
}

//export shoal_connector_list_table_constraints
func shoal_connector_list_table_constraints(
	handle *C.shoal_connector,
	tableName *C.char,
	timeoutMilliseconds C.int64_t,
	outResult **C.shoal_table_constraint_list_result,
	outError **C.shoal_error,
) (status C.shoal_status) {
	clearError(outError)
	if outResult != nil {
		*outResult = nil
	}
	var result *C.shoal_table_constraint_list_result
	defer func() {
		if recovered := recover(); recovered != nil {
			if result != nil {
				C.shoal_bridge_table_constraint_list_free(result)
			}
			status = fail(outError, C.SHOAL_STATUS_INTERNAL, fmt.Errorf("shoal: internal panic: %v", recovered))
		}
	}()
	if outResult == nil {
		return fail(outError, C.SHOAL_STATUS_INVALID_ARGUMENT, errors.New("shoal: out_result is required"))
	}
	name, err := requiredString(tableName, "table_name")
	if err != nil {
		return fail(outError, C.SHOAL_STATUS_INVALID_ARGUMENT, err)
	}
	connector, maintenance, code, err := lookupTableMaintenance(handle)
	if err != nil {
		return fail(outError, code, err)
	}
	ctx, done, code, err := beginConnectorOperation(connector, timeoutMilliseconds)
	if err != nil {
		return fail(outError, code, err)
	}
	constraints, err := func() ([]accumulo.Constraint, error) {
		defer done()
		return maintenance.ListConstraints(ctx, name)
	}()
	if err != nil {
		return failForError(outError, err)
	}
	result = C.shoal_bridge_table_constraint_list_alloc(C.size_t(len(constraints)))
	if result == nil {
		return fail(outError, C.SHOAL_STATUS_OUT_OF_MEMORY, errors.New("shoal: allocate table constraint list"))
	}
	for index, constraint := range constraints {
		class := C.CString(constraint.ClassName)
		if class == nil {
			C.shoal_bridge_table_constraint_list_free(result)
			result = nil
			return fail(outError, C.SHOAL_STATUS_OUT_OF_MEMORY, errors.New("shoal: allocate constraint class"))
		}
		ok := C.shoal_bridge_table_constraint_list_set(
			result, C.size_t(index), C.int32_t(constraint.Number), class,
		)
		C.free(unsafe.Pointer(class))
		if ok == 0 {
			C.shoal_bridge_table_constraint_list_free(result)
			result = nil
			return fail(outError, C.SHOAL_STATUS_OUT_OF_MEMORY, errors.New("shoal: copy table constraint list"))
		}
	}
	*outResult = result
	result = nil
	return C.SHOAL_STATUS_OK
}

//export shoal_table_constraint_list_count
func shoal_table_constraint_list_count(
	result *C.shoal_table_constraint_list_result,
) C.size_t {
	return C.shoal_bridge_table_constraint_list_count(result)
}

//export shoal_table_constraint_list_get
func shoal_table_constraint_list_get(
	result *C.shoal_table_constraint_list_result,
	index C.size_t,
	out *C.shoal_table_constraint_view,
	outError **C.shoal_error,
) (status C.shoal_status) {
	clearError(outError)
	defer recoverStatus(&status, outError)
	if out == nil || out.struct_size < C.SHOAL_TABLE_CONSTRAINT_VIEW_V1_SIZE {
		return fail(outError, C.SHOAL_STATUS_INVALID_ARGUMENT, errors.New("shoal: constraint view is missing or too small"))
	}
	if C.shoal_bridge_table_constraint_list_get(result, index, out) == 0 {
		return fail(outError, C.SHOAL_STATUS_INVALID_ARGUMENT, errors.New("shoal: invalid constraint list access"))
	}
	return C.SHOAL_STATUS_OK
}

//export shoal_table_constraint_list_free
func shoal_table_constraint_list_free(
	result **C.shoal_table_constraint_list_result,
) {
	if result != nil && *result != nil {
		C.shoal_bridge_table_constraint_list_free(*result)
		*result = nil
	}
}

//export shoal_connector_remove_table_constraint
func shoal_connector_remove_table_constraint(
	handle *C.shoal_connector,
	tableName *C.char,
	number C.int32_t,
	timeoutMilliseconds C.int64_t,
	outError **C.shoal_error,
) (status C.shoal_status) {
	clearError(outError)
	defer recoverStatus(&status, outError)
	if number <= 0 {
		return fail(outError, C.SHOAL_STATUS_INVALID_ARGUMENT, errors.New("shoal: constraint number must be positive"))
	}
	name, err := requiredString(tableName, "table_name")
	if err != nil {
		return fail(outError, C.SHOAL_STATUS_INVALID_ARGUMENT, err)
	}
	connector, maintenance, code, err := lookupTableMaintenance(handle)
	if err != nil {
		return fail(outError, code, err)
	}
	ctx, done, code, err := beginConnectorOperation(connector, timeoutMilliseconds)
	if err != nil {
		return fail(outError, code, err)
	}
	err = func() error {
		defer done()
		return maintenance.RemoveConstraint(ctx, name, int32(number))
	}()
	return failOrOK(outError, err)
}
