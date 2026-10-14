package main

/*
#cgo CFLAGS: -I${SRCDIR}/../../capi/include
#include <stdlib.h>
#include "bridge.h"
*/
import "C"

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/phrocker/shoal-oss/accumulo"
)

//export shoal_connector_list_tables
func shoal_connector_list_tables(
	handle *C.shoal_connector,
	timeoutMilliseconds C.int64_t,
	outResult **C.shoal_table_list_result,
	outError **C.shoal_error,
) (status C.shoal_status) {
	clearError(outError)
	if outResult != nil {
		*outResult = nil
	}
	var result *C.shoal_table_list_result
	defer func() {
		if recovered := recover(); recovered != nil {
			if result != nil {
				C.shoal_bridge_table_list_free(result)
				result = nil
			}
			status = fail(
				outError,
				C.SHOAL_STATUS_INTERNAL,
				fmt.Errorf("shoal: internal panic: %v", recovered),
			)
		}
	}()
	if outResult == nil {
		return fail(outError, C.SHOAL_STATUS_INVALID_ARGUMENT, errors.New("shoal: out_result is required"))
	}
	connector, err := lookupConnector(handle)
	if err != nil {
		return fail(outError, C.SHOAL_STATUS_INVALID_HANDLE, err)
	}
	ctx, done, code, err := beginConnectorOperation(connector, timeoutMilliseconds)
	if err != nil {
		return fail(outError, code, err)
	}
	tables, err := func() ([]accumulo.Table, error) {
		defer done()
		return connector.connector.Tables(ctx)
	}()
	if err != nil {
		return failForError(outError, err)
	}
	result, code, err = buildTableListResult(tables)
	if err != nil {
		return fail(outError, code, err)
	}
	*outResult = result
	result = nil
	return C.SHOAL_STATUS_OK
}

//export shoal_connector_table_exists
func shoal_connector_table_exists(
	handle *C.shoal_connector,
	tableName *C.char,
	timeoutMilliseconds C.int64_t,
	outExists *C.uint8_t,
	outError **C.shoal_error,
) (status C.shoal_status) {
	clearError(outError)
	if outExists != nil {
		*outExists = 0
	}
	defer recoverStatus(&status, outError)
	if outExists == nil {
		return fail(outError, C.SHOAL_STATUS_INVALID_ARGUMENT, errors.New("shoal: out_exists is required"))
	}
	connector, err := lookupConnector(handle)
	if err != nil {
		return fail(outError, C.SHOAL_STATUS_INVALID_HANDLE, err)
	}
	name, err := requiredString(tableName, "table_name")
	if err != nil {
		return fail(outError, C.SHOAL_STATUS_INVALID_ARGUMENT, err)
	}
	ctx, done, code, err := beginConnectorOperation(connector, timeoutMilliseconds)
	if err != nil {
		return fail(outError, code, err)
	}
	exists, err := func() (bool, error) {
		defer done()
		return connector.connector.TableExists(ctx, name)
	}()
	if err != nil {
		return failForError(outError, err)
	}
	if exists {
		*outExists = 1
	}
	return C.SHOAL_STATUS_OK
}

//export shoal_connector_create_table
func shoal_connector_create_table(
	handle *C.shoal_connector,
	tableName *C.char,
	timeoutMilliseconds C.int64_t,
	outError **C.shoal_error,
) (status C.shoal_status) {
	return mutateTable(handle, tableName, nil, timeoutMilliseconds, outError, func(
		ctx context.Context,
		connector connectorAPI,
		name string,
		_ string,
	) error {
		return connector.CreateTable(ctx, name)
	})
}

//export shoal_connector_delete_table
func shoal_connector_delete_table(
	handle *C.shoal_connector,
	tableName *C.char,
	timeoutMilliseconds C.int64_t,
	outError **C.shoal_error,
) (status C.shoal_status) {
	return mutateTable(handle, tableName, nil, timeoutMilliseconds, outError, func(
		ctx context.Context,
		connector connectorAPI,
		name string,
		_ string,
	) error {
		return connector.DeleteTable(ctx, name)
	})
}

//export shoal_connector_rename_table
func shoal_connector_rename_table(
	handle *C.shoal_connector,
	tableName *C.char,
	newTableName *C.char,
	timeoutMilliseconds C.int64_t,
	outError **C.shoal_error,
) (status C.shoal_status) {
	return mutateTable(handle, tableName, newTableName, timeoutMilliseconds, outError, func(
		ctx context.Context,
		connector connectorAPI,
		name string,
		other string,
	) error {
		return connector.RenameTable(ctx, name, other)
	})
}

//export shoal_connector_flush_table
func shoal_connector_flush_table(
	handle *C.shoal_connector,
	tableName *C.char,
	wait C.uint8_t,
	timeoutMilliseconds C.int64_t,
	outError **C.shoal_error,
) (status C.shoal_status) {
	clearError(outError)
	defer recoverStatus(&status, outError)
	connector, err := lookupConnector(handle)
	if err != nil {
		return fail(outError, C.SHOAL_STATUS_INVALID_HANDLE, err)
	}
	name, err := requiredString(tableName, "table_name")
	if err != nil {
		return fail(outError, C.SHOAL_STATUS_INVALID_ARGUMENT, err)
	}
	waitForCompletion, err := boolFlag(wait, "wait")
	if err != nil {
		return fail(outError, C.SHOAL_STATUS_INVALID_ARGUMENT, err)
	}
	ctx, done, code, err := beginConnectorOperation(connector, timeoutMilliseconds)
	if err != nil {
		return fail(outError, code, err)
	}
	err = func() error {
		defer done()
		return connector.connector.FlushTable(ctx, name, waitForCompletion)
	}()
	return failOrOK(outError, err)
}

//export shoal_connector_set_table_property
func shoal_connector_set_table_property(
	handle *C.shoal_connector,
	tableName *C.char,
	propertyName *C.char,
	propertyValue *C.char,
	timeoutMilliseconds C.int64_t,
	outError **C.shoal_error,
) (status C.shoal_status) {
	clearError(outError)
	defer recoverStatus(&status, outError)
	connector, err := lookupConnector(handle)
	if err != nil {
		return fail(outError, C.SHOAL_STATUS_INVALID_HANDLE, err)
	}
	name, property, code, err := parseTablePropertyMutationInputs(tableName, propertyName)
	if err != nil {
		return fail(outError, code, err)
	}
	value, err := requiredStringAllowEmpty(propertyValue, "property_value")
	if err != nil {
		return fail(outError, C.SHOAL_STATUS_INVALID_ARGUMENT, err)
	}
	ctx, done, code, err := beginConnectorOperation(connector, timeoutMilliseconds)
	if err != nil {
		return fail(outError, code, err)
	}
	err = func() error {
		defer done()
		return connector.connector.SetTableProperty(ctx, name, property, value)
	}()
	return failOrOK(outError, err)
}

//export shoal_connector_remove_table_property
func shoal_connector_remove_table_property(
	handle *C.shoal_connector,
	tableName *C.char,
	propertyName *C.char,
	timeoutMilliseconds C.int64_t,
	outError **C.shoal_error,
) (status C.shoal_status) {
	clearError(outError)
	defer recoverStatus(&status, outError)
	connector, err := lookupConnector(handle)
	if err != nil {
		return fail(outError, C.SHOAL_STATUS_INVALID_HANDLE, err)
	}
	name, property, code, err := parseTablePropertyMutationInputs(tableName, propertyName)
	if err != nil {
		return fail(outError, code, err)
	}
	ctx, done, code, err := beginConnectorOperation(connector, timeoutMilliseconds)
	if err != nil {
		return fail(outError, code, err)
	}
	err = func() error {
		defer done()
		return connector.connector.RemoveTableProperty(ctx, name, property)
	}()
	return failOrOK(outError, err)
}

//export shoal_connector_effective_table_properties
func shoal_connector_effective_table_properties(
	handle *C.shoal_connector,
	tableName *C.char,
	timeoutMilliseconds C.int64_t,
	outResult **C.shoal_table_properties_result,
	outError **C.shoal_error,
) (status C.shoal_status) {
	clearError(outError)
	if outResult != nil {
		*outResult = nil
	}
	var result *C.shoal_table_properties_result
	defer func() {
		if recovered := recover(); recovered != nil {
			if result != nil {
				C.shoal_bridge_table_properties_free(result)
				result = nil
			}
			status = fail(
				outError,
				C.SHOAL_STATUS_INTERNAL,
				fmt.Errorf("shoal: internal panic: %v", recovered),
			)
		}
	}()
	if outResult == nil {
		return fail(outError, C.SHOAL_STATUS_INVALID_ARGUMENT, errors.New("shoal: out_result is required"))
	}
	connector, err := lookupConnector(handle)
	if err != nil {
		return fail(outError, C.SHOAL_STATUS_INVALID_HANDLE, err)
	}
	name, err := requiredString(tableName, "table_name")
	if err != nil {
		return fail(outError, C.SHOAL_STATUS_INVALID_ARGUMENT, err)
	}
	ctx, done, code, err := beginConnectorOperation(connector, timeoutMilliseconds)
	if err != nil {
		return fail(outError, code, err)
	}
	properties, err := func() (map[string]string, error) {
		defer done()
		return connector.connector.EffectiveTableProperties(ctx, name)
	}()
	if err != nil {
		return failForError(outError, err)
	}
	result, code, err = buildTablePropertiesResult(properties)
	if err != nil {
		return fail(outError, code, err)
	}
	*outResult = result
	result = nil
	return C.SHOAL_STATUS_OK
}

//export shoal_table_list_count
func shoal_table_list_count(result *C.shoal_table_list_result) C.size_t {
	return C.shoal_bridge_table_list_count(result)
}

//export shoal_table_list_get
func shoal_table_list_get(
	result *C.shoal_table_list_result,
	index C.size_t,
	outTable *C.shoal_table_view,
	outError **C.shoal_error,
) (status C.shoal_status) {
	clearError(outError)
	defer recoverStatus(&status, outError)
	if result == nil {
		return fail(outError, C.SHOAL_STATUS_INVALID_ARGUMENT, errors.New("shoal: table list result is NULL"))
	}
	if outTable == nil {
		return fail(outError, C.SHOAL_STATUS_INVALID_ARGUMENT, errors.New("shoal: out_table is required"))
	}
	if C.shoal_bridge_table_list_get(result, index, outTable) == 0 {
		return fail(
			outError,
			C.SHOAL_STATUS_INVALID_ARGUMENT,
			fmt.Errorf("shoal: table list index %d is out of bounds", uint64(index)),
		)
	}
	return C.SHOAL_STATUS_OK
}

//export shoal_table_list_free
func shoal_table_list_free(result **C.shoal_table_list_result) {
	if result == nil || *result == nil {
		return
	}
	C.shoal_bridge_table_list_free(*result)
	*result = nil
}

//export shoal_table_properties_count
func shoal_table_properties_count(result *C.shoal_table_properties_result) C.size_t {
	return C.shoal_bridge_table_properties_count(result)
}

//export shoal_table_properties_get
func shoal_table_properties_get(
	result *C.shoal_table_properties_result,
	index C.size_t,
	outProperty *C.shoal_table_property_view,
	outError **C.shoal_error,
) (status C.shoal_status) {
	clearError(outError)
	defer recoverStatus(&status, outError)
	if result == nil {
		return fail(outError, C.SHOAL_STATUS_INVALID_ARGUMENT, errors.New("shoal: table properties result is NULL"))
	}
	if outProperty == nil {
		return fail(outError, C.SHOAL_STATUS_INVALID_ARGUMENT, errors.New("shoal: out_property is required"))
	}
	if C.shoal_bridge_table_properties_get(result, index, outProperty) == 0 {
		return fail(
			outError,
			C.SHOAL_STATUS_INVALID_ARGUMENT,
			fmt.Errorf("shoal: table properties index %d is out of bounds", uint64(index)),
		)
	}
	return C.SHOAL_STATUS_OK
}

//export shoal_table_properties_free
func shoal_table_properties_free(result **C.shoal_table_properties_result) {
	if result == nil || *result == nil {
		return
	}
	C.shoal_bridge_table_properties_free(*result)
	*result = nil
}

func beginConnectorOperation(
	connector *ownedConnector,
	timeoutMilliseconds C.int64_t,
) (context.Context, func(), C.shoal_status, error) {
	timeout, err := operationTimeout(timeoutMilliseconds)
	if err != nil {
		return nil, nil, C.SHOAL_STATUS_INVALID_ARGUMENT, err
	}
	ctx, done, err := connector.begin(timeout)
	if err != nil {
		return nil, nil, statusForError(err), err
	}
	return ctx, done, C.SHOAL_STATUS_OK, nil
}

func mutateTable(
	handle *C.shoal_connector,
	tableName *C.char,
	otherName *C.char,
	timeoutMilliseconds C.int64_t,
	outError **C.shoal_error,
	call func(context.Context, connectorAPI, string, string) error,
) (status C.shoal_status) {
	clearError(outError)
	defer recoverStatus(&status, outError)
	connector, err := lookupConnector(handle)
	if err != nil {
		return fail(outError, C.SHOAL_STATUS_INVALID_HANDLE, err)
	}
	name, err := requiredString(tableName, "table_name")
	if err != nil {
		return fail(outError, C.SHOAL_STATUS_INVALID_ARGUMENT, err)
	}
	other := ""
	if otherName != nil {
		other, err = requiredString(otherName, "new_table_name")
		if err != nil {
			return fail(outError, C.SHOAL_STATUS_INVALID_ARGUMENT, err)
		}
	}
	ctx, done, code, err := beginConnectorOperation(connector, timeoutMilliseconds)
	if err != nil {
		return fail(outError, code, err)
	}
	err = func() error {
		defer done()
		return call(ctx, connector.connector, name, other)
	}()
	return failOrOK(outError, err)
}

func parseTablePropertyMutationInputs(
	tableName *C.char,
	propertyName *C.char,
) (string, string, C.shoal_status, error) {
	name, err := requiredString(tableName, "table_name")
	if err != nil {
		return "", "", C.SHOAL_STATUS_INVALID_ARGUMENT, err
	}
	property, err := requiredString(propertyName, "property_name")
	if err != nil {
		return "", "", C.SHOAL_STATUS_INVALID_ARGUMENT, err
	}
	return name, property, C.SHOAL_STATUS_OK, nil
}

func buildTableListResult(
	tables []accumulo.Table,
) (*C.shoal_table_list_result, C.shoal_status, error) {
	result := C.shoal_bridge_table_list_alloc(C.size_t(len(tables)))
	if result == nil {
		return nil, C.SHOAL_STATUS_OUT_OF_MEMORY, errors.New("shoal: allocate table list result")
	}
	for index, table := range tables {
		cName, code, err := bridgeCString(table.Name, fmt.Sprintf("table %d name", index))
		if err != nil {
			C.shoal_bridge_table_list_free(result)
			return nil, code, err
		}
		cID, code, err := bridgeCString(table.ID, fmt.Sprintf("table %d id", index))
		if err != nil {
			C.shoal_bridge_string_free(cName)
			C.shoal_bridge_table_list_free(result)
			return nil, code, err
		}
		ok := C.shoal_bridge_table_list_set(result, C.size_t(index), cName, cID) != 0
		C.shoal_bridge_string_free(cName)
		C.shoal_bridge_string_free(cID)
		if !ok {
			C.shoal_bridge_table_list_free(result)
			return nil, C.SHOAL_STATUS_OUT_OF_MEMORY, errors.New("shoal: allocate table list entry")
		}
	}
	return result, C.SHOAL_STATUS_OK, nil
}

func buildTablePropertiesResult(
	properties map[string]string,
) (*C.shoal_table_properties_result, C.shoal_status, error) {
	keys := make([]string, 0, len(properties))
	for key := range properties {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	result := C.shoal_bridge_table_properties_alloc(C.size_t(len(keys)))
	if result == nil {
		return nil, C.SHOAL_STATUS_OUT_OF_MEMORY, errors.New("shoal: allocate table properties result")
	}
	for index, key := range keys {
		cKey, code, err := bridgeCString(key, fmt.Sprintf("property %d key", index))
		if err != nil {
			C.shoal_bridge_table_properties_free(result)
			return nil, code, err
		}
		cValue, code, err := bridgeCString(properties[key], fmt.Sprintf("property %d value", index))
		if err != nil {
			C.shoal_bridge_string_free(cKey)
			C.shoal_bridge_table_properties_free(result)
			return nil, code, err
		}
		ok := C.shoal_bridge_table_properties_set(result, C.size_t(index), cKey, cValue) != 0
		C.shoal_bridge_string_free(cKey)
		C.shoal_bridge_string_free(cValue)
		if !ok {
			C.shoal_bridge_table_properties_free(result)
			return nil, C.SHOAL_STATUS_OUT_OF_MEMORY, errors.New("shoal: allocate table properties entry")
		}
	}
	return result, C.SHOAL_STATUS_OK, nil
}

func bridgeCString(value, name string) (*C.char, C.shoal_status, error) {
	if strings.IndexByte(value, 0) >= 0 {
		return nil, C.SHOAL_STATUS_INTERNAL, fmt.Errorf("shoal: %s contains NUL", name)
	}
	result := C.shoal_bridge_string_alloc(cStringData(value), C.size_t(len(value)))
	if result == nil {
		return nil, C.SHOAL_STATUS_OUT_OF_MEMORY, fmt.Errorf("shoal: allocate %s", name)
	}
	return result, C.SHOAL_STATUS_OK, nil
}

func boolFlag(value C.uint8_t, name string) (bool, error) {
	switch value {
	case 0:
		return false, nil
	case 1:
		return true, nil
	default:
		return false, fmt.Errorf("shoal: %s must be 0 or 1", name)
	}
}

func failOrOK(outError **C.shoal_error, err error) C.shoal_status {
	if err == nil {
		return C.SHOAL_STATUS_OK
	}
	return failForError(outError, err)
}
