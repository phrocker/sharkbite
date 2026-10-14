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
	"time"

	"github.com/phrocker/shoal-oss/accumulo"
)

const batchWriterFreeTimeout = 5 * time.Second

//export shoal_batch_writer_config_init
func shoal_batch_writer_config_init(config *C.shoal_batch_writer_config) {
	C.shoal_bridge_batch_writer_config_init(config)
}

//export shoal_mutation_create
func shoal_mutation_create(
	row C.shoal_bytes,
	outMutation **C.shoal_mutation,
	outError **C.shoal_error,
) (status C.shoal_status) {
	clearError(outError)
	if outMutation != nil {
		*outMutation = nil
	}
	defer recoverStatus(&status, outError)
	if outMutation == nil {
		return fail(outError, C.SHOAL_STATUS_INVALID_ARGUMENT, errors.New("shoal: out_mutation is required"))
	}
	rowCopy, err := copyByteValue(row, "mutation row")
	if err != nil {
		return fail(outError, C.SHOAL_STATUS_INVALID_ARGUMENT, err)
	}
	mutation, err := accumulo.NewMutation(rowCopy)
	if err != nil {
		return fail(outError, C.SHOAL_STATUS_INVALID_ARGUMENT, err)
	}
	id, ok := mutations.add(mutation)
	if !ok {
		return fail(outError, C.SHOAL_STATUS_INTERNAL, errors.New("shoal: mutation handle space exhausted"))
	}
	handle := C.shoal_bridge_mutation_alloc(C.uint64_t(id))
	if handle == nil {
		mutations.remove(id)
		return fail(outError, C.SHOAL_STATUS_OUT_OF_MEMORY, errors.New("shoal: allocate mutation handle"))
	}
	*outMutation = handle
	return C.SHOAL_STATUS_OK
}

//export shoal_mutation_put
func shoal_mutation_put(
	handle *C.shoal_mutation,
	columnFamily C.shoal_bytes,
	columnQualifier C.shoal_bytes,
	columnVisibility C.shoal_bytes,
	timestamp C.int64_t,
	value C.shoal_bytes,
	outError **C.shoal_error,
) (status C.shoal_status) {
	clearError(outError)
	defer recoverStatus(&status, outError)
	mutation, err := lookupMutation(handle)
	if err != nil {
		return fail(outError, C.SHOAL_STATUS_INVALID_HANDLE, err)
	}
	family, qualifier, visibility, valueCopy, err := parseMutationUpdate(
		columnFamily,
		columnQualifier,
		columnVisibility,
		&value,
	)
	if err != nil {
		return fail(outError, C.SHOAL_STATUS_INVALID_ARGUMENT, err)
	}
	mutation.Put(family, qualifier, visibility, int64(timestamp), valueCopy)
	return C.SHOAL_STATUS_OK
}

//export shoal_mutation_put_latest
func shoal_mutation_put_latest(
	handle *C.shoal_mutation,
	columnFamily C.shoal_bytes,
	columnQualifier C.shoal_bytes,
	columnVisibility C.shoal_bytes,
	value C.shoal_bytes,
	outError **C.shoal_error,
) (status C.shoal_status) {
	clearError(outError)
	defer recoverStatus(&status, outError)
	mutation, err := lookupMutation(handle)
	if err != nil {
		return fail(outError, C.SHOAL_STATUS_INVALID_HANDLE, err)
	}
	family, qualifier, visibility, valueCopy, err := parseMutationUpdate(
		columnFamily,
		columnQualifier,
		columnVisibility,
		&value,
	)
	if err != nil {
		return fail(outError, C.SHOAL_STATUS_INVALID_ARGUMENT, err)
	}
	mutation.PutLatest(family, qualifier, visibility, valueCopy)
	return C.SHOAL_STATUS_OK
}

//export shoal_mutation_delete
func shoal_mutation_delete(
	handle *C.shoal_mutation,
	columnFamily C.shoal_bytes,
	columnQualifier C.shoal_bytes,
	columnVisibility C.shoal_bytes,
	timestamp C.int64_t,
	outError **C.shoal_error,
) (status C.shoal_status) {
	clearError(outError)
	defer recoverStatus(&status, outError)
	mutation, err := lookupMutation(handle)
	if err != nil {
		return fail(outError, C.SHOAL_STATUS_INVALID_HANDLE, err)
	}
	family, qualifier, visibility, _, err := parseMutationUpdate(
		columnFamily,
		columnQualifier,
		columnVisibility,
		nil,
	)
	if err != nil {
		return fail(outError, C.SHOAL_STATUS_INVALID_ARGUMENT, err)
	}
	mutation.Delete(family, qualifier, visibility, int64(timestamp))
	return C.SHOAL_STATUS_OK
}

//export shoal_mutation_delete_latest
func shoal_mutation_delete_latest(
	handle *C.shoal_mutation,
	columnFamily C.shoal_bytes,
	columnQualifier C.shoal_bytes,
	columnVisibility C.shoal_bytes,
	outError **C.shoal_error,
) (status C.shoal_status) {
	clearError(outError)
	defer recoverStatus(&status, outError)
	mutation, err := lookupMutation(handle)
	if err != nil {
		return fail(outError, C.SHOAL_STATUS_INVALID_HANDLE, err)
	}
	family, qualifier, visibility, _, err := parseMutationUpdate(
		columnFamily,
		columnQualifier,
		columnVisibility,
		nil,
	)
	if err != nil {
		return fail(outError, C.SHOAL_STATUS_INVALID_ARGUMENT, err)
	}
	mutation.DeleteLatest(family, qualifier, visibility)
	return C.SHOAL_STATUS_OK
}

//export shoal_mutation_size
func shoal_mutation_size(
	handle *C.shoal_mutation,
	outSize *C.size_t,
	outError **C.shoal_error,
) (status C.shoal_status) {
	clearError(outError)
	if outSize != nil {
		*outSize = 0
	}
	defer recoverStatus(&status, outError)
	if outSize == nil {
		return fail(outError, C.SHOAL_STATUS_INVALID_ARGUMENT, errors.New("shoal: out_size is required"))
	}
	mutation, err := lookupMutation(handle)
	if err != nil {
		return fail(outError, C.SHOAL_STATUS_INVALID_HANDLE, err)
	}
	*outSize = C.size_t(mutation.Size())
	return C.SHOAL_STATUS_OK
}

//export shoal_mutation_free
func shoal_mutation_free(handle **C.shoal_mutation) {
	defer func() { _ = recover() }()
	if handle == nil || *handle == nil {
		return
	}
	value := *handle
	*handle = nil
	id := uint64(C.shoal_bridge_mutation_id(value))
	mutations.remove(id)
	C.shoal_bridge_mutation_free(value)
}

//export shoal_connector_create_batch_writer
func shoal_connector_create_batch_writer(
	connectorHandle *C.shoal_connector,
	config *C.shoal_batch_writer_config,
	outWriter **C.shoal_batch_writer,
	outError **C.shoal_error,
) (status C.shoal_status) {
	clearError(outError)
	if outWriter != nil {
		*outWriter = nil
	}
	defer recoverStatus(&status, outError)
	if outWriter == nil {
		return fail(outError, C.SHOAL_STATUS_INVALID_ARGUMENT, errors.New("shoal: out_writer is required"))
	}
	connector, err := lookupConnector(connectorHandle)
	if err != nil {
		return fail(outError, C.SHOAL_STATUS_INVALID_HANDLE, err)
	}
	table, options, err := parseBatchWriterConfig(config)
	if err != nil {
		return fail(outError, C.SHOAL_STATUS_INVALID_ARGUMENT, err)
	}
	writer, err := connector.connector.NewBatchWriter(table, options)
	if err != nil {
		return failForError(outError, err)
	}
	owned := newOwnedBatchWriter(writer, connector)
	id, ok := batchWriters.add(owned)
	if !ok {
		_ = owned.close(batchWriterFreeTimeout)
		return fail(outError, C.SHOAL_STATUS_INTERNAL, errors.New("shoal: batch writer handle space exhausted"))
	}
	handle := C.shoal_bridge_batch_writer_alloc(C.uint64_t(id))
	if handle == nil {
		batchWriters.remove(id)
		_ = owned.close(batchWriterFreeTimeout)
		return fail(outError, C.SHOAL_STATUS_OUT_OF_MEMORY, errors.New("shoal: allocate batch writer handle"))
	}
	*outWriter = handle
	return C.SHOAL_STATUS_OK
}

//export shoal_batch_writer_add
func shoal_batch_writer_add(
	handle *C.shoal_batch_writer,
	mutationHandle *C.shoal_mutation,
	timeoutMilliseconds C.int64_t,
	outFailure **C.shoal_write_failure,
	outError **C.shoal_error,
) (status C.shoal_status) {
	clearWriteOutputs(outFailure, outError)
	defer recoverWriteStatus(&status, outFailure, outError)
	writer, err := lookupBatchWriter(handle)
	if err != nil {
		return fail(outError, C.SHOAL_STATUS_INVALID_HANDLE, err)
	}
	mutation, err := lookupMutation(mutationHandle)
	if err != nil {
		return fail(outError, C.SHOAL_STATUS_INVALID_HANDLE, err)
	}
	timeout, err := operationTimeout(timeoutMilliseconds)
	if err != nil {
		return fail(outError, C.SHOAL_STATUS_INVALID_ARGUMENT, err)
	}
	ctx, done, err := writer.begin(timeout)
	if err != nil {
		return finishWrite(err, outFailure, outError)
	}
	err = func() error {
		defer done()
		return writer.writer.Add(ctx, mutation)
	}()
	return finishWrite(err, outFailure, outError)
}

//export shoal_batch_writer_flush
func shoal_batch_writer_flush(
	handle *C.shoal_batch_writer,
	timeoutMilliseconds C.int64_t,
	outFailure **C.shoal_write_failure,
	outError **C.shoal_error,
) (status C.shoal_status) {
	clearWriteOutputs(outFailure, outError)
	defer recoverWriteStatus(&status, outFailure, outError)
	writer, err := lookupBatchWriter(handle)
	if err != nil {
		return fail(outError, C.SHOAL_STATUS_INVALID_HANDLE, err)
	}
	timeout, err := operationTimeout(timeoutMilliseconds)
	if err != nil {
		return fail(outError, C.SHOAL_STATUS_INVALID_ARGUMENT, err)
	}
	ctx, done, err := writer.begin(timeout)
	if err != nil {
		return finishWrite(err, outFailure, outError)
	}
	err = func() error {
		defer done()
		return writer.writer.Flush(ctx)
	}()
	return finishWrite(err, outFailure, outError)
}

//export shoal_batch_writer_size
func shoal_batch_writer_size(
	handle *C.shoal_batch_writer,
	timeoutMilliseconds C.int64_t,
	outSize *C.size_t,
	outError **C.shoal_error,
) (status C.shoal_status) {
	clearError(outError)
	if outSize != nil {
		*outSize = 0
	}
	defer recoverStatus(&status, outError)
	if outSize == nil {
		return fail(outError, C.SHOAL_STATUS_INVALID_ARGUMENT, errors.New("shoal: out_size is required"))
	}
	writer, err := lookupBatchWriter(handle)
	if err != nil {
		return fail(outError, C.SHOAL_STATUS_INVALID_HANDLE, err)
	}
	timeout, err := operationTimeout(timeoutMilliseconds)
	if err != nil {
		return fail(outError, C.SHOAL_STATUS_INVALID_ARGUMENT, err)
	}
	ctx, done, err := writer.begin(timeout)
	if err != nil {
		return failForError(outError, err)
	}
	defer done()
	sizer, ok := writer.writer.(interface {
		Size(context.Context) (int, error)
	})
	if !ok {
		return fail(outError, C.SHOAL_STATUS_UNSUPPORTED, errors.New("shoal: batch writer size is unsupported"))
	}
	size, err := sizer.Size(ctx)
	if err != nil {
		return failForError(outError, err)
	}
	*outSize = C.size_t(size)
	return C.SHOAL_STATUS_OK
}

//export shoal_batch_writer_close
func shoal_batch_writer_close(
	handle *C.shoal_batch_writer,
	timeoutMilliseconds C.int64_t,
	outFailure **C.shoal_write_failure,
	outError **C.shoal_error,
) (status C.shoal_status) {
	clearWriteOutputs(outFailure, outError)
	defer recoverWriteStatus(&status, outFailure, outError)
	writer, err := lookupBatchWriter(handle)
	if err != nil {
		return fail(outError, C.SHOAL_STATUS_INVALID_HANDLE, err)
	}
	timeout, err := operationTimeout(timeoutMilliseconds)
	if err != nil {
		return fail(outError, C.SHOAL_STATUS_INVALID_ARGUMENT, err)
	}
	return finishWrite(writer.close(timeout), outFailure, outError)
}

//export shoal_batch_writer_free
func shoal_batch_writer_free(handle **C.shoal_batch_writer) {
	defer func() { _ = recover() }()
	if handle == nil || *handle == nil {
		return
	}
	value := *handle
	*handle = nil
	id := uint64(C.shoal_bridge_batch_writer_id(value))
	if writer, ok := batchWriters.remove(id); ok {
		_ = writer.close(batchWriterFreeTimeout)
	}
	C.shoal_bridge_batch_writer_free(value)
}

//export shoal_write_failure_get_flags
func shoal_write_failure_get_flags(failure *C.shoal_write_failure) C.shoal_write_failure_flags {
	return C.shoal_bridge_write_failure_flags(failure)
}

//export shoal_write_failure_failed_extent_count
func shoal_write_failure_failed_extent_count(failure *C.shoal_write_failure) C.size_t {
	return C.shoal_bridge_write_failure_failed_extent_count(failure)
}

//export shoal_write_failure_get_failed_extent
func shoal_write_failure_get_failed_extent(
	failure *C.shoal_write_failure,
	index C.size_t,
	outExtent *C.shoal_failed_extent_view,
	outError **C.shoal_error,
) (status C.shoal_status) {
	return getFailureEntry(
		failure,
		outExtent != nil,
		C.shoal_bridge_write_failure_get_failed_extent(failure, index, outExtent) != 0,
		uint64(index),
		"failed extent",
		outError,
	)
}

//export shoal_write_failure_constraint_count
func shoal_write_failure_constraint_count(failure *C.shoal_write_failure) C.size_t {
	return C.shoal_bridge_write_failure_constraint_count(failure)
}

//export shoal_write_failure_get_constraint
func shoal_write_failure_get_constraint(
	failure *C.shoal_write_failure,
	index C.size_t,
	outViolation *C.shoal_constraint_violation_view,
	outError **C.shoal_error,
) (status C.shoal_status) {
	return getFailureEntry(
		failure,
		outViolation != nil,
		C.shoal_bridge_write_failure_get_constraint(failure, index, outViolation) != 0,
		uint64(index),
		"constraint violation",
		outError,
	)
}

//export shoal_write_failure_authorization_count
func shoal_write_failure_authorization_count(failure *C.shoal_write_failure) C.size_t {
	return C.shoal_bridge_write_failure_authorization_count(failure)
}

//export shoal_write_failure_get_authorization
func shoal_write_failure_get_authorization(
	failure *C.shoal_write_failure,
	index C.size_t,
	outAuthorization *C.shoal_authorization_failure_view,
	outError **C.shoal_error,
) (status C.shoal_status) {
	return getFailureEntry(
		failure,
		outAuthorization != nil,
		C.shoal_bridge_write_failure_get_authorization(failure, index, outAuthorization) != 0,
		uint64(index),
		"authorization failure",
		outError,
	)
}

//export shoal_write_failure_cleanup_count
func shoal_write_failure_cleanup_count(failure *C.shoal_write_failure) C.size_t {
	return C.shoal_bridge_write_failure_cleanup_count(failure)
}

//export shoal_write_failure_get_cleanup
func shoal_write_failure_get_cleanup(
	failure *C.shoal_write_failure,
	index C.size_t,
	outCleanup *C.shoal_cleanup_failure_view,
	outError **C.shoal_error,
) (status C.shoal_status) {
	return getFailureEntry(
		failure,
		outCleanup != nil,
		C.shoal_bridge_write_failure_get_cleanup(failure, index, outCleanup) != 0,
		uint64(index),
		"cleanup failure",
		outError,
	)
}

//export shoal_write_failure_free
func shoal_write_failure_free(failure **C.shoal_write_failure) {
	if failure == nil || *failure == nil {
		return
	}
	C.shoal_bridge_write_failure_free(*failure)
	*failure = nil
}

func parseMutationUpdate(
	columnFamily C.shoal_bytes,
	columnQualifier C.shoal_bytes,
	columnVisibility C.shoal_bytes,
	value *C.shoal_bytes,
) ([]byte, []byte, []byte, []byte, error) {
	family, err := copyByteValue(columnFamily, "column_family")
	if err != nil {
		return nil, nil, nil, nil, err
	}
	qualifier, err := copyByteValue(columnQualifier, "column_qualifier")
	if err != nil {
		return nil, nil, nil, nil, err
	}
	visibility, err := copyByteValue(columnVisibility, "column_visibility")
	if err != nil {
		return nil, nil, nil, nil, err
	}
	var valueCopy []byte
	if value != nil {
		valueCopy, err = copyByteValue(*value, "value")
		if err != nil {
			return nil, nil, nil, nil, err
		}
	}
	return family, qualifier, visibility, valueCopy, nil
}

func parseBatchWriterConfig(config *C.shoal_batch_writer_config) (accumulo.Table, accumulo.BatchWriterOptions, error) {
	if config == nil {
		return accumulo.Table{}, accumulo.BatchWriterOptions{}, errors.New("shoal: batch writer config is required")
	}
	requiredSize := uint64(C.shoal_bridge_batch_writer_config_v1_size())
	if uint64(config.struct_size) < requiredSize {
		return accumulo.Table{}, accumulo.BatchWriterOptions{}, fmt.Errorf(
			"shoal: batch writer config struct_size is %d, need at least %d",
			uint64(config.struct_size),
			requiredSize,
		)
	}
	tableName := optionalString(config.table_name)
	tableID := optionalString(config.table_id)
	if (tableName == "") == (tableID == "") {
		return accumulo.Table{}, accumulo.BatchWriterOptions{}, errors.New(
			"shoal: exactly one of table_name and table_id is required",
		)
	}
	for _, field := range []struct {
		name  string
		value int64
	}{
		{"max_memory_bytes", int64(config.max_memory_bytes)},
		{"max_batch_bytes", int64(config.max_batch_bytes)},
		{"max_write_threads", int64(config.max_write_threads)},
		{"max_retries", int64(config.max_retries)},
	} {
		if field.value < 0 {
			return accumulo.Table{}, accumulo.BatchWriterOptions{}, fmt.Errorf(
				"shoal: %s must not be negative",
				field.name,
			)
		}
	}
	maxLatency, err := durationMilliseconds(config.max_latency_ms, "max_latency_ms", 0)
	if err != nil {
		return accumulo.Table{}, accumulo.BatchWriterOptions{}, err
	}
	retryBackoff, err := durationMilliseconds(config.retry_backoff_ms, "retry_backoff_ms", 0)
	if err != nil {
		return accumulo.Table{}, accumulo.BatchWriterOptions{}, err
	}
	durability, err := parseDurability(config.durability)
	if err != nil {
		return accumulo.Table{}, accumulo.BatchWriterOptions{}, err
	}
	options := accumulo.BatchWriterOptions{
		MaxMemoryBytes:  int64(config.max_memory_bytes),
		MaxBatchBytes:   int64(config.max_batch_bytes),
		MaxLatency:      maxLatency,
		MaxWriteThreads: int(config.max_write_threads),
		MaxRetries:      int(config.max_retries),
		RetryBackoff:    retryBackoff,
		Durability:      durability,
	}
	return accumulo.Table{Name: tableName, ID: tableID}, options, nil
}

func parseDurability(value C.shoal_durability) (accumulo.Durability, error) {
	switch value {
	case C.SHOAL_DURABILITY_DEFAULT:
		return accumulo.DurabilityDefault, nil
	case C.SHOAL_DURABILITY_SYNC:
		return accumulo.DurabilitySync, nil
	case C.SHOAL_DURABILITY_FLUSH:
		return accumulo.DurabilityFlush, nil
	case C.SHOAL_DURABILITY_LOG:
		return accumulo.DurabilityLog, nil
	case C.SHOAL_DURABILITY_NONE:
		return accumulo.DurabilityNone, nil
	default:
		return 0, fmt.Errorf("shoal: unsupported durability %d", int32(value))
	}
}

func lookupMutation(handle *C.shoal_mutation) (*accumulo.Mutation, error) {
	if handle == nil {
		return nil, errors.New("shoal: mutation handle is NULL")
	}
	id := uint64(C.shoal_bridge_mutation_id(handle))
	if id == 0 {
		return nil, errors.New("shoal: mutation handle is invalid")
	}
	mutation, ok := mutations.get(id)
	if !ok {
		return nil, errors.New("shoal: mutation handle is unknown or freed")
	}
	return mutation, nil
}

func lookupBatchWriter(handle *C.shoal_batch_writer) (*ownedBatchWriter, error) {
	if handle == nil {
		return nil, errors.New("shoal: batch writer handle is NULL")
	}
	id := uint64(C.shoal_bridge_batch_writer_id(handle))
	if id == 0 {
		return nil, errors.New("shoal: batch writer handle is invalid")
	}
	writer, ok := batchWriters.get(id)
	if !ok {
		return nil, errors.New("shoal: batch writer handle is unknown or freed")
	}
	return writer, nil
}

type flattenedFailedExtent struct {
	server string
	value  accumulo.FailedExtent
}

type flattenedConstraint struct {
	server string
	value  accumulo.ConstraintViolation
}

type flattenedAuthorization struct {
	server string
	value  accumulo.AuthorizationFailure
}

type flattenedCleanup struct {
	server  string
	message string
}

type writeFailureData struct {
	flags          C.shoal_write_failure_flags
	failedExtents  []flattenedFailedExtent
	constraints    []flattenedConstraint
	authorizations []flattenedAuthorization
	cleanups       []flattenedCleanup
}

func flattenWriteFailure(err error) writeFailureData {
	var result writeFailureData
	if errors.Is(err, accumulo.ErrBatchWriterFailed) {
		result.flags |= C.SHOAL_WRITE_FAILURE_AMBIGUOUS_COMMIT
	}
	if errors.Is(err, accumulo.ErrBatchWriterRetryExhausted) {
		result.flags |= C.SHOAL_WRITE_FAILURE_RETRY_EXHAUSTED
	}
	if errors.Is(err, accumulo.ErrBatchWriterAutoFlush) {
		result.flags |= C.SHOAL_WRITE_FAILURE_AUTOMATIC_FLUSH
	}
	walkErrors(err, func(item error) {
		switch typed := item.(type) {
		case *accumulo.MutationRejectionError:
			for _, extent := range typed.FailedExtents {
				result.failedExtents = append(result.failedExtents, flattenedFailedExtent{
					server: typed.Server,
					value:  extent,
				})
			}
			for _, violation := range typed.ConstraintViolations {
				result.constraints = append(result.constraints, flattenedConstraint{
					server: typed.Server,
					value:  violation,
				})
			}
			for _, authorization := range typed.AuthorizationFailures {
				result.authorizations = append(result.authorizations, flattenedAuthorization{
					server: typed.Server,
					value:  authorization,
				})
			}
		case *accumulo.BatchWriterCleanupError:
			result.cleanups = append(result.cleanups, flattenedCleanup{
				server:  typed.Server,
				message: typed.Err.Error(),
			})
		}
	})
	return result
}

func walkErrors(err error, visit func(error)) {
	if err == nil {
		return
	}
	visit(err)
	if many, ok := err.(interface{ Unwrap() []error }); ok {
		for _, child := range many.Unwrap() {
			walkErrors(child, visit)
		}
		return
	}
	if one, ok := err.(interface{ Unwrap() error }); ok {
		walkErrors(one.Unwrap(), visit)
	}
}

func finishWrite(
	err error,
	outFailure **C.shoal_write_failure,
	outError **C.shoal_error,
) C.shoal_status {
	if err == nil {
		return C.SHOAL_STATUS_OK
	}
	if outFailure != nil {
		data := flattenWriteFailure(err)
		if data.flags != 0 || len(data.failedExtents) != 0 || len(data.constraints) != 0 ||
			len(data.authorizations) != 0 || len(data.cleanups) != 0 {
			failure, code, allocErr := allocateWriteFailure(data)
			if allocErr != nil {
				return fail(outError, code, allocErr)
			}
			*outFailure = failure
		}
	}
	return failForError(outError, err)
}

func allocateWriteFailure(
	data writeFailureData,
) (*C.shoal_write_failure, C.shoal_status, error) {
	failure := C.shoal_bridge_write_failure_alloc(
		data.flags,
		C.size_t(len(data.failedExtents)),
		C.size_t(len(data.constraints)),
		C.size_t(len(data.authorizations)),
		C.size_t(len(data.cleanups)),
	)
	if failure == nil {
		return nil, C.SHOAL_STATUS_OUT_OF_MEMORY, errors.New("shoal: allocate write failure")
	}
	for index, entry := range data.failedExtents {
		code, err := setFailedExtent(failure, index, entry)
		if err != nil {
			C.shoal_bridge_write_failure_free(failure)
			return nil, code, err
		}
	}
	for index, entry := range data.constraints {
		server, code, err := bridgeCString(entry.server, fmt.Sprintf("constraint %d server", index))
		if err != nil {
			C.shoal_bridge_write_failure_free(failure)
			return nil, code, err
		}
		className, code, err := bridgeCString(
			entry.value.ConstraintClass,
			fmt.Sprintf("constraint %d class", index),
		)
		if err != nil {
			C.shoal_bridge_string_free(server)
			C.shoal_bridge_write_failure_free(failure)
			return nil, code, err
		}
		description, code, err := bridgeCString(
			entry.value.Description,
			fmt.Sprintf("constraint %d description", index),
		)
		if err != nil {
			C.shoal_bridge_string_free(server)
			C.shoal_bridge_string_free(className)
			C.shoal_bridge_write_failure_free(failure)
			return nil, code, err
		}
		ok := C.shoal_bridge_write_failure_set_constraint(
			failure,
			C.size_t(index),
			server,
			className,
			C.int16_t(entry.value.ViolationCode),
			description,
			C.int64_t(entry.value.NumberOfViolatingMutations),
		) != 0
		C.shoal_bridge_string_free(server)
		C.shoal_bridge_string_free(className)
		C.shoal_bridge_string_free(description)
		if !ok {
			C.shoal_bridge_write_failure_free(failure)
			return nil, C.SHOAL_STATUS_OUT_OF_MEMORY, fmt.Errorf("shoal: allocate constraint %d entry", index)
		}
	}
	for index, entry := range data.authorizations {
		code, err := setAuthorizationFailure(failure, index, entry)
		if err != nil {
			C.shoal_bridge_write_failure_free(failure)
			return nil, code, err
		}
	}
	for index, entry := range data.cleanups {
		server, code, err := bridgeCString(entry.server, fmt.Sprintf("cleanup %d server", index))
		if err != nil {
			C.shoal_bridge_write_failure_free(failure)
			return nil, code, err
		}
		message, code, err := bridgeCString(entry.message, fmt.Sprintf("cleanup %d message", index))
		if err != nil {
			C.shoal_bridge_string_free(server)
			C.shoal_bridge_write_failure_free(failure)
			return nil, code, err
		}
		ok := C.shoal_bridge_write_failure_set_cleanup(
			failure,
			C.size_t(index),
			server,
			message,
		) != 0
		C.shoal_bridge_string_free(server)
		C.shoal_bridge_string_free(message)
		if !ok {
			C.shoal_bridge_write_failure_free(failure)
			return nil, C.SHOAL_STATUS_OUT_OF_MEMORY, fmt.Errorf("shoal: allocate cleanup %d entry", index)
		}
	}
	return failure, C.SHOAL_STATUS_OK, nil
}

func setFailedExtent(
	failure *C.shoal_write_failure,
	index int,
	entry flattenedFailedExtent,
) (C.shoal_status, error) {
	server, code, err := bridgeCString(entry.server, fmt.Sprintf("failed extent %d server", index))
	if err != nil {
		return code, err
	}
	tableID, code, err := bridgeCString(entry.value.Extent.TableID, fmt.Sprintf("failed extent %d table id", index))
	if err != nil {
		C.shoal_bridge_string_free(server)
		return code, err
	}
	ok := C.shoal_bridge_write_failure_set_failed_extent(
		failure,
		C.size_t(index),
		server,
		tableID,
		bytePointer(entry.value.Extent.PrevRow),
		C.size_t(len(entry.value.Extent.PrevRow)),
		boolByte(entry.value.Extent.PrevRow != nil),
		bytePointer(entry.value.Extent.EndRow),
		C.size_t(len(entry.value.Extent.EndRow)),
		boolByte(entry.value.Extent.EndRow != nil),
		C.size_t(entry.value.Submitted),
		C.int64_t(entry.value.Committed),
	) != 0
	C.shoal_bridge_string_free(server)
	C.shoal_bridge_string_free(tableID)
	if !ok {
		return C.SHOAL_STATUS_OUT_OF_MEMORY, fmt.Errorf("shoal: allocate failed extent %d entry", index)
	}
	return C.SHOAL_STATUS_OK, nil
}

func setAuthorizationFailure(
	failure *C.shoal_write_failure,
	index int,
	entry flattenedAuthorization,
) (C.shoal_status, error) {
	server, code, err := bridgeCString(entry.server, fmt.Sprintf("authorization %d server", index))
	if err != nil {
		return code, err
	}
	tableID, code, err := bridgeCString(entry.value.Extent.TableID, fmt.Sprintf("authorization %d table id", index))
	if err != nil {
		C.shoal_bridge_string_free(server)
		return code, err
	}
	codeValue, code, err := bridgeCString(entry.value.Code, fmt.Sprintf("authorization %d code", index))
	if err != nil {
		C.shoal_bridge_string_free(server)
		C.shoal_bridge_string_free(tableID)
		return code, err
	}
	ok := C.shoal_bridge_write_failure_set_authorization(
		failure,
		C.size_t(index),
		server,
		tableID,
		bytePointer(entry.value.Extent.PrevRow),
		C.size_t(len(entry.value.Extent.PrevRow)),
		boolByte(entry.value.Extent.PrevRow != nil),
		bytePointer(entry.value.Extent.EndRow),
		C.size_t(len(entry.value.Extent.EndRow)),
		boolByte(entry.value.Extent.EndRow != nil),
		codeValue,
	) != 0
	C.shoal_bridge_string_free(server)
	C.shoal_bridge_string_free(tableID)
	C.shoal_bridge_string_free(codeValue)
	if !ok {
		return C.SHOAL_STATUS_OUT_OF_MEMORY, fmt.Errorf("shoal: allocate authorization %d entry", index)
	}
	return C.SHOAL_STATUS_OK, nil
}

func boolByte(value bool) C.uint8_t {
	if value {
		return 1
	}
	return 0
}

func clearWriteOutputs(outFailure **C.shoal_write_failure, outError **C.shoal_error) {
	clearError(outError)
	if outFailure != nil {
		*outFailure = nil
	}
}

func recoverWriteStatus(
	status *C.shoal_status,
	outFailure **C.shoal_write_failure,
	outError **C.shoal_error,
) {
	if recovered := recover(); recovered != nil {
		if outFailure != nil && *outFailure != nil {
			C.shoal_bridge_write_failure_free(*outFailure)
			*outFailure = nil
		}
		*status = fail(
			outError,
			C.SHOAL_STATUS_INTERNAL,
			fmt.Errorf("shoal: internal panic: %v", recovered),
		)
	}
}

func getFailureEntry(
	failure *C.shoal_write_failure,
	outputPresent bool,
	found bool,
	index uint64,
	name string,
	outError **C.shoal_error,
) C.shoal_status {
	clearError(outError)
	if failure == nil {
		return fail(outError, C.SHOAL_STATUS_INVALID_ARGUMENT, errors.New("shoal: write failure is NULL"))
	}
	if !outputPresent {
		return fail(outError, C.SHOAL_STATUS_INVALID_ARGUMENT, fmt.Errorf("shoal: out_%s is required", name))
	}
	if !found {
		return fail(
			outError,
			C.SHOAL_STATUS_INVALID_ARGUMENT,
			fmt.Errorf("shoal: write failure %s index %d is out of bounds", name, index),
		)
	}
	return C.SHOAL_STATUS_OK
}
