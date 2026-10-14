package main

/*
#cgo CFLAGS: -I${SRCDIR}/../../capi/include
#include "bridge.h"
#include <stdlib.h>
*/
import "C"

import (
	"encoding/json"
	"errors"
	"unsafe"

	"github.com/phrocker/shoal-oss/accumulo"
)

//export shoal_logging_set_level
func shoal_logging_set_level(
	level C.shoal_log_level,
	outError **C.shoal_error,
) (status C.shoal_status) {
	clearError(outError)
	defer recoverStatus(&status, outError)
	if level < C.SHOAL_LOG_LEVEL_OFF || level > C.SHOAL_LOG_LEVEL_TRACE {
		return fail(outError, C.SHOAL_STATUS_INVALID_ARGUMENT, errors.New("shoal: invalid log level"))
	}
	accumulo.SetLogLevel(accumulo.LogLevel(level))
	return C.SHOAL_STATUS_OK
}

//export shoal_logging_get_level
func shoal_logging_get_level() C.shoal_log_level {
	return C.shoal_log_level(accumulo.CurrentLogLevel())
}

//export shoal_logging_set_callback
func shoal_logging_set_callback(
	callback C.shoal_log_callback,
	context unsafe.Pointer,
	outError **C.shoal_error,
) (status C.shoal_status) {
	clearError(outError)
	defer recoverStatus(&status, outError)
	if callback == nil {
		accumulo.SetLogSink(nil)
		return C.SHOAL_STATUS_OK
	}
	accumulo.SetLogSink(func(event accumulo.LogEvent) {
		attributes, err := json.Marshal(event.Attributes)
		if err != nil {
			attributes = []byte("{}")
		}
		name := C.CString(event.Message)
		payload := C.CString(string(attributes))
		if name == nil || payload == nil {
			if name != nil {
				C.free(unsafe.Pointer(name))
			}
			if payload != nil {
				C.free(unsafe.Pointer(payload))
			}
			return
		}
		C.shoal_bridge_log_callback_invoke(
			callback,
			C.shoal_log_level(event.Level),
			name,
			payload,
			context,
		)
		C.free(unsafe.Pointer(name))
		C.free(unsafe.Pointer(payload))
	})
	return C.SHOAL_STATUS_OK
}
