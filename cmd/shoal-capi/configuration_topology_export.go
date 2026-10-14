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

func bytesString(value C.shoal_bytes, name string) (string, error) {
	if value.data == nil && value.length != 0 {
		return "", fmt.Errorf("shoal: %s data is NULL with non-zero length", name)
	}
	if uint64(value.length) > uint64(^uint32(0)>>1) {
		return "", fmt.Errorf("shoal: %s is too large", name)
	}
	return C.GoStringN((*C.char)(unsafe.Pointer(value.data)), C.int(value.length)), nil
}

func configurationHandle(configuration *accumulo.Configuration) (*C.shoal_configuration, C.shoal_status, error) {
	id, ok := configurations.add(configuration)
	if !ok {
		return nil, C.SHOAL_STATUS_INTERNAL, errors.New("shoal: configuration handle space exhausted")
	}
	handle := C.shoal_bridge_configuration_alloc(C.uint64_t(id))
	if handle == nil {
		configurations.remove(id)
		return nil, C.SHOAL_STATUS_OUT_OF_MEMORY, errors.New("shoal: allocate configuration handle")
	}
	return handle, C.SHOAL_STATUS_OK, nil
}

func lookupConfiguration(handle *C.shoal_configuration) (*accumulo.Configuration, error) {
	if handle == nil {
		return nil, errors.New("shoal: configuration handle is NULL")
	}
	id := uint64(C.shoal_bridge_configuration_id(handle))
	configuration, ok := configurations.get(id)
	if id == 0 || !ok {
		return nil, errors.New("shoal: configuration handle is unknown or freed")
	}
	return configuration, nil
}

//export shoal_configuration_create
func shoal_configuration_create(out **C.shoal_configuration, outError **C.shoal_error) (status C.shoal_status) {
	clearError(outError)
	if out != nil {
		*out = nil
	}
	defer recoverStatus(&status, outError)
	if out == nil {
		return fail(outError, C.SHOAL_STATUS_INVALID_ARGUMENT, errors.New("shoal: out_configuration is required"))
	}
	handle, code, err := configurationHandle(accumulo.NewConfiguration())
	if err != nil {
		return fail(outError, code, err)
	}
	*out = handle
	return C.SHOAL_STATUS_OK
}

//export shoal_configuration_set
func shoal_configuration_set(handle *C.shoal_configuration, name C.shoal_bytes, value C.shoal_bytes, outError **C.shoal_error) (status C.shoal_status) {
	clearError(outError)
	defer recoverStatus(&status, outError)
	configuration, err := lookupConfiguration(handle)
	if err != nil {
		return fail(outError, C.SHOAL_STATUS_INVALID_HANDLE, err)
	}
	key, err := bytesString(name, "name")
	if err != nil {
		return fail(outError, C.SHOAL_STATUS_INVALID_ARGUMENT, err)
	}
	converted, err := bytesString(value, "value")
	if err != nil {
		return fail(outError, C.SHOAL_STATUS_INVALID_ARGUMENT, err)
	}
	configuration.Set(key, converted)
	return C.SHOAL_STATUS_OK
}

func configurationGet(handle *C.shoal_configuration, name C.shoal_bytes, fallback *C.shoal_bytes, out **C.shoal_bytes_result, outError **C.shoal_error) C.shoal_status {
	if out != nil {
		*out = nil
	}
	if out == nil {
		return fail(outError, C.SHOAL_STATUS_INVALID_ARGUMENT, errors.New("shoal: out_result is required"))
	}
	configuration, err := lookupConfiguration(handle)
	if err != nil {
		return fail(outError, C.SHOAL_STATUS_INVALID_HANDLE, err)
	}
	key, err := bytesString(name, "name")
	if err != nil {
		return fail(outError, C.SHOAL_STATUS_INVALID_ARGUMENT, err)
	}
	value := configuration.Get(key)
	if fallback != nil {
		def, convertErr := bytesString(*fallback, "default_value")
		if convertErr != nil {
			return fail(outError, C.SHOAL_STATUS_INVALID_ARGUMENT, convertErr)
		}
		value = configuration.GetOr(key, def)
	}
	result := C.shoal_bridge_bytes_result_alloc((*C.uint8_t)(unsafe.Pointer(unsafe.StringData(value))), C.size_t(len(value)))
	if result == nil {
		return fail(outError, C.SHOAL_STATUS_OUT_OF_MEMORY, errors.New("shoal: allocate bytes result"))
	}
	*out = result
	return C.SHOAL_STATUS_OK
}

//export shoal_configuration_get
func shoal_configuration_get(handle *C.shoal_configuration, name C.shoal_bytes, out **C.shoal_bytes_result, outError **C.shoal_error) (status C.shoal_status) {
	clearError(outError)
	defer recoverStatus(&status, outError)
	return configurationGet(handle, name, nil, out, outError)
}

//export shoal_configuration_get_or
func shoal_configuration_get_or(handle *C.shoal_configuration, name C.shoal_bytes, fallback C.shoal_bytes, out **C.shoal_bytes_result, outError **C.shoal_error) (status C.shoal_status) {
	clearError(outError)
	defer recoverStatus(&status, outError)
	return configurationGet(handle, name, &fallback, out, outError)
}

func configurationUint32(handle *C.shoal_configuration, name C.shoal_bytes, fallback *uint32, out *C.uint32_t, outError **C.shoal_error) C.shoal_status {
	if out != nil {
		*out = 0
	}
	if out == nil {
		return fail(outError, C.SHOAL_STATUS_INVALID_ARGUMENT, errors.New("shoal: out_value is required"))
	}
	configuration, err := lookupConfiguration(handle)
	if err != nil {
		return fail(outError, C.SHOAL_STATUS_INVALID_HANDLE, err)
	}
	key, err := bytesString(name, "name")
	if err != nil {
		return fail(outError, C.SHOAL_STATUS_INVALID_ARGUMENT, err)
	}
	if fallback == nil {
		*out = C.uint32_t(configuration.GetUint32(key))
	} else {
		*out = C.uint32_t(configuration.GetUint32Or(key, *fallback))
	}
	return C.SHOAL_STATUS_OK
}

//export shoal_configuration_get_uint32
func shoal_configuration_get_uint32(handle *C.shoal_configuration, name C.shoal_bytes, out *C.uint32_t, outError **C.shoal_error) (status C.shoal_status) {
	clearError(outError)
	defer recoverStatus(&status, outError)
	return configurationUint32(handle, name, nil, out, outError)
}

//export shoal_configuration_get_uint32_or
func shoal_configuration_get_uint32_or(handle *C.shoal_configuration, name C.shoal_bytes, fallback C.uint32_t, out *C.uint32_t, outError **C.shoal_error) (status C.shoal_status) {
	clearError(outError)
	defer recoverStatus(&status, outError)
	value := uint32(fallback)
	return configurationUint32(handle, name, &value, out, outError)
}

//export shoal_configuration_free
func shoal_configuration_free(handle **C.shoal_configuration) {
	if handle == nil || *handle == nil {
		return
	}
	value := *handle
	*handle = nil
	configurations.remove(uint64(C.shoal_bridge_configuration_id(value)))
	C.shoal_bridge_configuration_free(value)
}

//export shoal_bytes_result_get
func shoal_bytes_result_get(result *C.shoal_bytes_result) C.shoal_bytes {
	return C.shoal_bridge_bytes_result_get(result)
}

//export shoal_bytes_result_free
func shoal_bytes_result_free(result **C.shoal_bytes_result) {
	if result != nil && *result != nil {
		C.shoal_bridge_bytes_result_free(*result)
		*result = nil
	}
}

func allocBytes(value string) (*C.shoal_bytes_result, C.shoal_status, error) {
	result := C.shoal_bridge_bytes_result_alloc((*C.uint8_t)(unsafe.Pointer(unsafe.StringData(value))), C.size_t(len(value)))
	if result == nil {
		return nil, C.SHOAL_STATUS_OUT_OF_MEMORY, errors.New("shoal: allocate bytes result")
	}
	return result, C.SHOAL_STATUS_OK, nil
}

func allocStringList(values []string) (*C.shoal_string_list_result, C.shoal_status, error) {
	result := C.shoal_bridge_string_list_alloc(C.size_t(len(values)))
	if result == nil {
		return nil, C.SHOAL_STATUS_OUT_OF_MEMORY, errors.New("shoal: allocate string list")
	}
	for index, value := range values {
		if C.shoal_bridge_string_list_set(result, C.size_t(index), (*C.uint8_t)(unsafe.Pointer(unsafe.StringData(value))), C.size_t(len(value))) == 0 {
			C.shoal_bridge_string_list_free(result)
			return nil, C.SHOAL_STATUS_OUT_OF_MEMORY, errors.New("shoal: allocate string list entry")
		}
	}
	return result, C.SHOAL_STATUS_OK, nil
}

//export shoal_string_list_count
func shoal_string_list_count(result *C.shoal_string_list_result) C.size_t {
	return C.shoal_bridge_string_list_count(result)
}

//export shoal_string_list_get
func shoal_string_list_get(result *C.shoal_string_list_result, index C.size_t, out *C.shoal_bytes, outError **C.shoal_error) (status C.shoal_status) {
	clearError(outError)
	defer recoverStatus(&status, outError)
	if out == nil || C.shoal_bridge_string_list_get(result, index, out) == 0 {
		return fail(outError, C.SHOAL_STATUS_INVALID_ARGUMENT, errors.New("shoal: invalid string list access"))
	}
	return C.SHOAL_STATUS_OK
}

//export shoal_string_list_free
func shoal_string_list_free(result **C.shoal_string_list_result) {
	if result != nil && *result != nil {
		C.shoal_bridge_string_list_free(*result)
		*result = nil
	}
}

//export shoal_server_view_init
func shoal_server_view_init(view *C.shoal_server_view) {
	if view != nil {
		C.memset(unsafe.Pointer(view), 0, C.size_t(C.SHOAL_SERVER_VIEW_V1_SIZE))
		view.struct_size = C.SHOAL_SERVER_VIEW_V1_SIZE
	}
}

//export shoal_server_list_count
func shoal_server_list_count(result *C.shoal_server_list_result) C.size_t {
	return C.shoal_bridge_server_list_count(result)
}

//export shoal_server_list_get
func shoal_server_list_get(result *C.shoal_server_list_result, index C.size_t, out *C.shoal_server_view, outError **C.shoal_error) (status C.shoal_status) {
	clearError(outError)
	defer recoverStatus(&status, outError)
	if out == nil || out.struct_size < C.SHOAL_SERVER_VIEW_V1_SIZE {
		return fail(outError, C.SHOAL_STATUS_INVALID_ARGUMENT, errors.New("shoal: server view is missing or too small"))
	}
	if C.shoal_bridge_server_list_get(result, index, out) == 0 {
		return fail(outError, C.SHOAL_STATUS_INVALID_ARGUMENT, errors.New("shoal: invalid server list access"))
	}
	return C.SHOAL_STATUS_OK
}

//export shoal_server_list_free
func shoal_server_list_free(result **C.shoal_server_list_result) {
	if result != nil && *result != nil {
		C.shoal_bridge_server_list_free(*result)
		*result = nil
	}
}

func connectorInstance(handle *C.shoal_connector, outError **C.shoal_error) (*ownedConnector, func(), C.shoal_status) {
	connector, err := lookupConnector(handle)
	if err != nil {
		return nil, nil, fail(outError, C.SHOAL_STATUS_INVALID_HANDLE, err)
	}
	done, err := connector.retain()
	if err != nil {
		return nil, nil, failForError(outError, err)
	}
	if connector.instance == nil {
		done()
		return nil, nil, fail(outError, C.SHOAL_STATUS_DISCOVERY_UNAVAILABLE, accumulo.ErrDiscoveryUnavailable)
	}
	return connector, done, C.SHOAL_STATUS_OK
}

//export shoal_connector_get_root_tablet_location
func shoal_connector_get_root_tablet_location(handle *C.shoal_connector, timeout C.int64_t, out **C.shoal_bytes_result, outError **C.shoal_error) (status C.shoal_status) {
	clearError(outError)
	if out != nil {
		*out = nil
	}
	defer recoverStatus(&status, outError)
	if out == nil {
		return fail(outError, C.SHOAL_STATUS_INVALID_ARGUMENT, errors.New("shoal: out_result is required"))
	}
	connector, err := lookupConnector(handle)
	if err != nil {
		return fail(outError, C.SHOAL_STATUS_INVALID_HANDLE, err)
	}
	ctx, done, code, err := beginConnectorOperation(connector, timeout)
	if err != nil {
		return fail(outError, code, err)
	}
	defer done()
	location, err := connector.instance.RootTabletLocation(ctx)
	if err != nil {
		return failForError(outError, err)
	}
	result, code, err := allocBytes(location.HostPort)
	if err != nil {
		return fail(outError, code, err)
	}
	*out = result
	return C.SHOAL_STATUS_OK
}

//export shoal_connector_get_manager_locations
func shoal_connector_get_manager_locations(handle *C.shoal_connector, timeout C.int64_t, out **C.shoal_string_list_result, outError **C.shoal_error) (status C.shoal_status) {
	clearError(outError)
	if out != nil {
		*out = nil
	}
	defer recoverStatus(&status, outError)
	if out == nil {
		return fail(outError, C.SHOAL_STATUS_INVALID_ARGUMENT, errors.New("shoal: out_result is required"))
	}
	connector, err := lookupConnector(handle)
	if err != nil {
		return fail(outError, C.SHOAL_STATUS_INVALID_HANDLE, err)
	}
	ctx, done, code, err := beginConnectorOperation(connector, timeout)
	if err != nil {
		return fail(outError, code, err)
	}
	defer done()
	values, err := connector.instance.ManagerLocations(ctx)
	if err != nil {
		return failForError(outError, err)
	}
	result, code, err := allocStringList(values)
	if err != nil {
		return fail(outError, code, err)
	}
	*out = result
	return C.SHOAL_STATUS_OK
}

//export shoal_connector_get_zookeepers
func shoal_connector_get_zookeepers(handle *C.shoal_connector, out **C.shoal_string_list_result, outError **C.shoal_error) (status C.shoal_status) {
	clearError(outError)
	if out != nil {
		*out = nil
	}
	defer recoverStatus(&status, outError)
	if out == nil {
		return fail(outError, C.SHOAL_STATUS_INVALID_ARGUMENT, errors.New("shoal: out_result is required"))
	}
	connector, done, code := connectorInstance(handle, outError)
	if code != C.SHOAL_STATUS_OK {
		return code
	}
	defer done()
	result, code, err := allocStringList(connector.instance.ZooKeepers())
	if err != nil {
		return fail(outError, code, err)
	}
	*out = result
	return C.SHOAL_STATUS_OK
}

//export shoal_connector_get_configuration
func shoal_connector_get_configuration(handle *C.shoal_connector, out **C.shoal_configuration, outError **C.shoal_error) (status C.shoal_status) {
	clearError(outError)
	if out != nil {
		*out = nil
	}
	defer recoverStatus(&status, outError)
	if out == nil {
		return fail(outError, C.SHOAL_STATUS_INVALID_ARGUMENT, errors.New("shoal: out_configuration is required"))
	}
	connector, done, code := connectorInstance(handle, outError)
	if code != C.SHOAL_STATUS_OK {
		return code
	}
	defer done()
	result, code, err := configurationHandle(connector.instance.Configuration())
	if err != nil {
		return fail(outError, code, err)
	}
	*out = result
	return C.SHOAL_STATUS_OK
}

//export shoal_connector_get_servers
func shoal_connector_get_servers(handle *C.shoal_connector, timeout C.int64_t, out **C.shoal_server_list_result, outError **C.shoal_error) (status C.shoal_status) {
	clearError(outError)
	if out != nil {
		*out = nil
	}
	defer recoverStatus(&status, outError)
	if out == nil {
		return fail(outError, C.SHOAL_STATUS_INVALID_ARGUMENT, errors.New("shoal: out_result is required"))
	}
	connector, err := lookupConnector(handle)
	if err != nil {
		return fail(outError, C.SHOAL_STATUS_INVALID_HANDLE, err)
	}
	ctx, done, code, err := beginConnectorOperation(connector, timeout)
	if err != nil {
		return fail(outError, code, err)
	}
	defer done()
	servers, err := connector.instance.Servers(ctx)
	if err != nil {
		return failForError(outError, err)
	}
	result := C.shoal_bridge_server_list_alloc(C.size_t(len(servers)))
	if result == nil {
		return fail(outError, C.SHOAL_STATUS_OUT_OF_MEMORY, errors.New("shoal: allocate server list"))
	}
	for index, server := range servers {
		kind, group, host := string(server.Kind), server.Group, server.Host
		if C.shoal_bridge_server_list_set(result, C.size_t(index),
			(*C.uint8_t)(unsafe.Pointer(unsafe.StringData(kind))), C.size_t(len(kind)),
			(*C.uint8_t)(unsafe.Pointer(unsafe.StringData(group))), C.size_t(len(group)),
			(*C.uint8_t)(unsafe.Pointer(unsafe.StringData(host))), C.size_t(len(host)), C.uint16_t(server.Port)) == 0 {
			C.shoal_bridge_server_list_free(result)
			return fail(outError, C.SHOAL_STATUS_OUT_OF_MEMORY, errors.New("shoal: allocate server list entry"))
		}
	}
	*out = result
	return C.SHOAL_STATUS_OK
}

//export shoal_connector_get_root
func shoal_connector_get_root(handle *C.shoal_connector, out **C.shoal_bytes_result, outError **C.shoal_error) (status C.shoal_status) {
	clearError(outError)
	if out != nil {
		*out = nil
	}
	defer recoverStatus(&status, outError)
	if out == nil {
		return fail(outError, C.SHOAL_STATUS_INVALID_ARGUMENT, errors.New("shoal: out_result is required"))
	}
	connector, done, code := connectorInstance(handle, outError)
	if code != C.SHOAL_STATUS_OK {
		return code
	}
	defer done()
	result, code, err := allocBytes(connector.instance.Root())
	if err != nil {
		return fail(outError, code, err)
	}
	*out = result
	return C.SHOAL_STATUS_OK
}
