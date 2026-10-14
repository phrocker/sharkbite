package main

/*
#cgo CFLAGS: -I${SRCDIR}/../../capi/include
#include "bridge.h"
*/
import "C"

import (
	"errors"
	"fmt"
	"time"

	"github.com/phrocker/shoal-oss/accumulo"
)

//export shoal_client_config_init
func shoal_client_config_init(config *C.shoal_client_config) {
	C.shoal_bridge_client_config_init(config)
}

func lookupClient(handle *C.shoal_client) (*ownedClient, error) {
	if handle == nil {
		return nil, errors.New("shoal: client handle is NULL")
	}
	id := uint64(C.shoal_bridge_client_id(handle))
	if id == 0 {
		return nil, errors.New("shoal: client handle is invalid")
	}
	client, ok := clients.get(id)
	if !ok {
		return nil, errors.New("shoal: client handle is stale")
	}
	return client, nil
}

//export shoal_client_create
func shoal_client_create(
	config *C.shoal_client_config,
	outClient **C.shoal_client,
	outError **C.shoal_error,
) (status C.shoal_status) {
	clearError(outError)
	if outClient != nil {
		*outClient = nil
	}
	defer recoverStatus(&status, outError)
	if outClient == nil {
		return fail(outError, C.SHOAL_STATUS_INVALID_ARGUMENT, errors.New("shoal: out_client is required"))
	}
	if config == nil {
		return fail(outError, C.SHOAL_STATUS_INVALID_ARGUMENT, errors.New("shoal: client config is required"))
	}
	requiredSize := uint64(C.shoal_bridge_client_config_v1_size())
	if uint64(config.struct_size) < requiredSize {
		return fail(
			outError,
			C.SHOAL_STATUS_INVALID_ARGUMENT,
			fmt.Errorf(
				"shoal: client config struct_size is %d, need at least %d",
				uint64(config.struct_size),
				requiredSize,
			),
		)
	}
	if config.connector == nil {
		return fail(outError, C.SHOAL_STATUS_INVALID_ARGUMENT, errors.New("shoal: connector config is required"))
	}
	if config.thread_count <= 0 {
		return fail(outError, C.SHOAL_STATUS_INVALID_ARGUMENT, errors.New("shoal: thread_count must be positive"))
	}
	authorizations, err := parseAuthorizations(
		config.authorizations,
		config.authorization_count,
	)
	if err != nil {
		return fail(outError, C.SHOAL_STATUS_INVALID_ARGUMENT, err)
	}
	parsed, err := parseConnectorConfig(config.connector)
	if err != nil {
		if errors.Is(err, accumulo.ErrUnsupportedVersion) {
			return fail(outError, C.SHOAL_STATUS_UNSUPPORTED, err)
		}
		return fail(outError, C.SHOAL_STATUS_INVALID_ARGUMENT, err)
	}
	defer zeroBytes(parsed.password)
	connector, code, err := openConnector(parsed)
	if err != nil {
		return fail(outError, code, err)
	}
	client := newOwnedClient(
		connector,
		optionalString(config.table_name),
		authorizations,
		int32(config.thread_count),
	)
	id, ok := clients.add(client)
	if !ok {
		_ = client.close()
		return fail(outError, C.SHOAL_STATUS_INTERNAL, errors.New("shoal: client handle space exhausted"))
	}
	handle := C.shoal_bridge_client_alloc(C.uint64_t(id))
	if handle == nil {
		clients.remove(id)
		_ = client.close()
		return fail(outError, C.SHOAL_STATUS_OUT_OF_MEMORY, errors.New("shoal: allocate client handle"))
	}
	*outClient = handle
	return C.SHOAL_STATUS_OK
}

//export shoal_client_set_threads
func shoal_client_set_threads(
	handle *C.shoal_client,
	threadCount C.int32_t,
	outError **C.shoal_error,
) (status C.shoal_status) {
	clearError(outError)
	defer recoverStatus(&status, outError)
	if threadCount <= 0 {
		return fail(outError, C.SHOAL_STATUS_INVALID_ARGUMENT, errors.New("shoal: thread_count must be positive"))
	}
	client, err := lookupClient(handle)
	if err != nil {
		return fail(outError, C.SHOAL_STATUS_INVALID_HANDLE, err)
	}
	return failOrOK(outError, client.setThreads(int32(threadCount)))
}

//export shoal_client_set_table
func shoal_client_set_table(
	handle *C.shoal_client,
	tableName *C.char,
	outError **C.shoal_error,
) (status C.shoal_status) {
	clearError(outError)
	defer recoverStatus(&status, outError)
	client, err := lookupClient(handle)
	if err != nil {
		return fail(outError, C.SHOAL_STATUS_INVALID_HANDLE, err)
	}
	if tableName == nil {
		return failOrOK(outError, client.checkOpen())
	}
	table := C.GoString(tableName)
	return failOrOK(outError, client.setTable(table))
}

//export shoal_client_set_authorizations
func shoal_client_set_authorizations(
	handle *C.shoal_client,
	values *C.shoal_bytes,
	count C.size_t,
	outError **C.shoal_error,
) (status C.shoal_status) {
	clearError(outError)
	defer recoverStatus(&status, outError)
	authorizations, err := parseAuthorizations(values, count)
	if err != nil {
		return fail(outError, C.SHOAL_STATUS_INVALID_ARGUMENT, err)
	}
	client, err := lookupClient(handle)
	if err != nil {
		return fail(outError, C.SHOAL_STATUS_INVALID_HANDLE, err)
	}
	return failOrOK(outError, client.setAuthorizations(authorizations))
}

//export shoal_client_list_tables
func shoal_client_list_tables(
	handle *C.shoal_client,
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
			}
			status = fail(outError, C.SHOAL_STATUS_INTERNAL, fmt.Errorf("shoal: internal panic: %v", recovered))
		}
	}()
	if outResult == nil {
		return fail(outError, C.SHOAL_STATUS_INVALID_ARGUMENT, errors.New("shoal: out_result is required"))
	}
	client, err := lookupClient(handle)
	if err != nil {
		return fail(outError, C.SHOAL_STATUS_INVALID_HANDLE, err)
	}
	timeout, err := operationTimeout(timeoutMilliseconds)
	if err != nil {
		return fail(outError, C.SHOAL_STATUS_INVALID_ARGUMENT, err)
	}
	ctx, done, err := client.begin(timeout)
	if err != nil {
		return failForError(outError, err)
	}
	tables, err := func() ([]accumulo.Table, error) {
		defer done()
		return client.connector.connector.Tables(ctx)
	}()
	if err != nil {
		return failForError(outError, err)
	}
	var code C.shoal_status
	result, code, err = buildTableListResult(tables)
	if err != nil {
		return fail(outError, code, err)
	}
	*outResult = result
	result = nil
	return C.SHOAL_STATUS_OK
}

//export shoal_client_create_scanner
func shoal_client_create_scanner(
	handle *C.shoal_client,
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
	client, err := lookupClient(handle)
	if err != nil {
		return fail(outError, C.SHOAL_STATUS_INVALID_HANDLE, err)
	}
	snapshot, done, err := client.snapshot(true)
	if err != nil {
		return failForError(outError, err)
	}
	defer done()
	scanner, err := client.connector.connector.NewScanner(
		accumulo.Table{Name: snapshot.table},
		accumulo.ScannerOptions{
			Authorizations: snapshot.authorizations,
			Columns:        snapshot.columns,
			Parallelism:    int(snapshot.threadCount),
		},
	)
	if err != nil {
		return failForError(outError, err)
	}
	owned := newOwnedScanner(scanner, nil, client.connector)
	id, ok := scanners.add(owned)
	if !ok {
		owned.close()
		return fail(outError, C.SHOAL_STATUS_INTERNAL, errors.New("shoal: scanner handle space exhausted"))
	}
	scannerHandle := C.shoal_bridge_scanner_alloc(C.uint64_t(id))
	if scannerHandle == nil {
		scanners.remove(id)
		owned.close()
		return fail(outError, C.SHOAL_STATUS_OUT_OF_MEMORY, errors.New("shoal: allocate scanner handle"))
	}
	*outScanner = scannerHandle
	return C.SHOAL_STATUS_OK
}

//export shoal_client_create_batch_writer
func shoal_client_create_batch_writer(
	handle *C.shoal_client,
	outWriter **C.shoal_accumulo_writer,
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
	client, err := lookupClient(handle)
	if err != nil {
		return fail(outError, C.SHOAL_STATUS_INVALID_HANDLE, err)
	}
	snapshot, done, err := client.snapshot(true)
	if err != nil {
		return failForError(outError, err)
	}
	defer done()
	table := accumulo.Table{Name: snapshot.table}
	options := accumulo.BatchWriterOptions{MaxWriteThreads: int(snapshot.threadCount)}
	owned := newOwnedAccumuloWriter(func() (batchWriterAPI, error) {
		return client.connector.connector.NewBatchWriter(table, options)
	}, client.connector, time.Now)
	id, ok := accumuloWriters.add(owned)
	if !ok {
		_ = owned.close(batchWriterFreeTimeout)
		return fail(outError, C.SHOAL_STATUS_INTERNAL, errors.New("shoal: buffered writer handle space exhausted"))
	}
	writerHandle := C.shoal_bridge_accumulo_writer_alloc(C.uint64_t(id))
	if writerHandle == nil {
		accumuloWriters.remove(id)
		_ = owned.close(batchWriterFreeTimeout)
		return fail(outError, C.SHOAL_STATUS_OUT_OF_MEMORY, errors.New("shoal: allocate buffered writer handle"))
	}
	*outWriter = writerHandle
	return C.SHOAL_STATUS_OK
}

//export shoal_client_close
func shoal_client_close(
	handle *C.shoal_client,
	outError **C.shoal_error,
) (status C.shoal_status) {
	clearError(outError)
	defer recoverStatus(&status, outError)
	client, err := lookupClient(handle)
	if err != nil {
		return fail(outError, C.SHOAL_STATUS_INVALID_HANDLE, err)
	}
	if err := client.close(); err != nil {
		return fail(outError, C.SHOAL_STATUS_INTERNAL, fmt.Errorf("shoal: close client: %w", err))
	}
	return C.SHOAL_STATUS_OK
}

//export shoal_client_free
func shoal_client_free(handle **C.shoal_client) {
	defer func() { _ = recover() }()
	if handle == nil || *handle == nil {
		return
	}
	value := *handle
	*handle = nil
	id := uint64(C.shoal_bridge_client_id(value))
	if client, ok := clients.remove(id); ok {
		_ = client.closeBounded(connectorFreeTimeout)
	}
	C.shoal_bridge_client_free(value)
}
