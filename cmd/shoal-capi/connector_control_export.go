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

type connectorInvalidationAPI interface {
	InvalidateTable(accumulo.Table) error
	InvalidateDiscovery() error
}

func lookupCancellation(
	handle *C.shoal_cancellation,
) (*ownedCancellation, error) {
	if handle == nil {
		return nil, errors.New("shoal: cancellation handle is NULL")
	}
	id := uint64(C.shoal_bridge_cancellation_id(handle))
	if id == 0 {
		return nil, errors.New("shoal: cancellation handle is invalid")
	}
	cancellation, ok := cancellations.get(id)
	if !ok {
		return nil, errors.New("shoal: cancellation handle is stale")
	}
	return cancellation, nil
}

//export shoal_cancellation_create
func shoal_cancellation_create(
	outCancellation **C.shoal_cancellation,
	outError **C.shoal_error,
) (status C.shoal_status) {
	clearError(outError)
	if outCancellation != nil {
		*outCancellation = nil
	}
	defer recoverStatus(&status, outError)
	if outCancellation == nil {
		return fail(outError, C.SHOAL_STATUS_INVALID_ARGUMENT, errors.New("shoal: out_cancellation is required"))
	}
	owned := newOwnedCancellation()
	id, ok := cancellations.add(owned)
	if !ok {
		return fail(outError, C.SHOAL_STATUS_INTERNAL, errors.New("shoal: cancellation handle space exhausted"))
	}
	handle := C.shoal_bridge_cancellation_alloc(C.uint64_t(id))
	if handle == nil {
		cancellations.remove(id)
		return fail(outError, C.SHOAL_STATUS_OUT_OF_MEMORY, errors.New("shoal: allocate cancellation handle"))
	}
	*outCancellation = handle
	return C.SHOAL_STATUS_OK
}

//export shoal_cancellation_cancel
func shoal_cancellation_cancel(
	handle *C.shoal_cancellation,
	outError **C.shoal_error,
) (status C.shoal_status) {
	clearError(outError)
	defer recoverStatus(&status, outError)
	cancellation, err := lookupCancellation(handle)
	if err != nil {
		return fail(outError, C.SHOAL_STATUS_INVALID_HANDLE, err)
	}
	cancellation.cancel()
	return C.SHOAL_STATUS_OK
}

//export shoal_cancellation_is_cancelled
func shoal_cancellation_is_cancelled(
	handle *C.shoal_cancellation,
	outCancelled *C.uint8_t,
	outError **C.shoal_error,
) (status C.shoal_status) {
	clearError(outError)
	if outCancelled != nil {
		*outCancelled = 0
	}
	defer recoverStatus(&status, outError)
	if outCancelled == nil {
		return fail(outError, C.SHOAL_STATUS_INVALID_ARGUMENT, errors.New("shoal: out_cancelled is required"))
	}
	cancellation, err := lookupCancellation(handle)
	if err != nil {
		return fail(outError, C.SHOAL_STATUS_INVALID_HANDLE, err)
	}
	if cancellation.isCancelled() {
		*outCancelled = 1
	}
	return C.SHOAL_STATUS_OK
}

//export shoal_cancellation_free
func shoal_cancellation_free(handle **C.shoal_cancellation) {
	defer func() { _ = recover() }()
	if handle == nil || *handle == nil {
		return
	}
	value := *handle
	*handle = nil
	id := uint64(C.shoal_bridge_cancellation_id(value))
	if cancellation, ok := cancellations.remove(id); ok {
		cancellation.close()
	}
	C.shoal_bridge_cancellation_free(value)
}

func lookupConnectorInvalidation(
	handle *C.shoal_connector,
) (*ownedConnector, connectorInvalidationAPI, C.shoal_status, error) {
	connector, err := lookupConnector(handle)
	if err != nil {
		return nil, nil, C.SHOAL_STATUS_INVALID_HANDLE, err
	}
	invalidation, ok := connector.connector.(connectorInvalidationAPI)
	if !ok {
		return nil, nil, C.SHOAL_STATUS_UNSUPPORTED, errors.New("shoal: connector invalidation is unsupported")
	}
	return connector, invalidation, C.SHOAL_STATUS_OK, nil
}

//export shoal_connector_invalidate_table
func shoal_connector_invalidate_table(
	handle *C.shoal_connector,
	tableID *C.char,
	outError **C.shoal_error,
) (status C.shoal_status) {
	clearError(outError)
	defer recoverStatus(&status, outError)
	id, err := requiredString(tableID, "table_id")
	if err != nil {
		return fail(outError, C.SHOAL_STATUS_INVALID_ARGUMENT, err)
	}
	connector, invalidation, code, err := lookupConnectorInvalidation(handle)
	if err != nil {
		return fail(outError, code, err)
	}
	done, err := connector.retain()
	if err != nil {
		return failForError(outError, err)
	}
	defer done()
	return failOrOK(outError, invalidation.InvalidateTable(accumulo.Table{ID: id}))
}

//export shoal_connector_invalidate_discovery
func shoal_connector_invalidate_discovery(
	handle *C.shoal_connector,
	outError **C.shoal_error,
) (status C.shoal_status) {
	clearError(outError)
	defer recoverStatus(&status, outError)
	connector, invalidation, code, err := lookupConnectorInvalidation(handle)
	if err != nil {
		return fail(outError, code, err)
	}
	done, err := connector.retain()
	if err != nil {
		return failForError(outError, err)
	}
	defer done()
	return failOrOK(outError, invalidation.InvalidateDiscovery())
}
