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
	"io"
	"time"
	"unsafe"

	publichdfs "github.com/phrocker/shoal-oss/hdfs"
)

type ownedHDFSClient struct {
	lifecycle rfileLifecycle
	client    *publichdfs.Client
}

type ownedHDFSInput struct {
	lifecycle rfileLifecycle
	stream    *publichdfs.InputStream
}

type ownedHDFSOutput struct {
	lifecycle rfileLifecycle
	stream    *publichdfs.OutputStream
}

var (
	hdfsClients = newRFileRegistry[ownedHDFSClient]()
	hdfsInputs  = newRFileRegistry[ownedHDFSInput]()
	hdfsOutputs = newRFileRegistry[ownedHDFSOutput]()
)

//export shoal_hdfs_dir_entry_view_init
func shoal_hdfs_dir_entry_view_init(view *C.shoal_hdfs_dir_entry_view) {
	if view != nil {
		C.memset(unsafe.Pointer(view), 0, C.size_t(C.SHOAL_HDFS_DIR_ENTRY_VIEW_V1_SIZE))
		view.struct_size = C.SHOAL_HDFS_DIR_ENTRY_VIEW_V1_SIZE
	}
}

func lookupHDFSClient(handle *C.shoal_hdfs_client) (*ownedHDFSClient, error) {
	if handle == nil {
		return nil, errors.New("shoal: HDFS client handle is NULL")
	}
	id := uint64(C.shoal_bridge_hdfs_client_id(handle))
	value, ok := hdfsClients.get(id)
	if id == 0 || !ok {
		return nil, errors.New("shoal: HDFS client handle is unknown or freed")
	}
	return value, nil
}

func lookupHDFSInput(handle *C.shoal_hdfs_input_stream) (*ownedHDFSInput, error) {
	if handle == nil {
		return nil, errors.New("shoal: HDFS input stream handle is NULL")
	}
	id := uint64(C.shoal_bridge_hdfs_input_stream_id(handle))
	value, ok := hdfsInputs.get(id)
	if id == 0 || !ok {
		return nil, errors.New("shoal: HDFS input stream handle is unknown or freed")
	}
	return value, nil
}

func lookupHDFSOutput(handle *C.shoal_hdfs_output_stream) (*ownedHDFSOutput, error) {
	if handle == nil {
		return nil, errors.New("shoal: HDFS output stream handle is NULL")
	}
	id := uint64(C.shoal_bridge_hdfs_output_stream_id(handle))
	value, ok := hdfsOutputs.get(id)
	if id == 0 || !ok {
		return nil, errors.New("shoal: HDFS output stream handle is unknown or freed")
	}
	return value, nil
}

func allocHDFSEntry(entry publichdfs.DirEntry) *C.shoal_hdfs_dir_entry_result {
	name := C.CString(entry.Name)
	owner := C.CString(entry.Owner)
	group := C.CString(entry.Group)
	defer C.free(unsafe.Pointer(name))
	defer C.free(unsafe.Pointer(owner))
	defer C.free(unsafe.Pointer(group))
	if name == nil || owner == nil || group == nil {
		return nil
	}
	var directory C.uint8_t
	if entry.IsDir {
		directory = 1
	}
	return C.shoal_bridge_hdfs_dir_entry_alloc(
		name, owner, group, C.int64_t(entry.Size),
		C.int64_t(entry.ModTime.UnixMilli()), C.uint32_t(entry.Mode.Perm()),
		directory,
	)
}

//export shoal_hdfs_client_create
func shoal_hdfs_client_create(address *C.char, timeout C.int64_t, out **C.shoal_hdfs_client, outError **C.shoal_error) (status C.shoal_status) {
	clearError(outError)
	if out != nil {
		*out = nil
	}
	defer recoverStatus(&status, outError)
	if out == nil {
		return fail(outError, C.SHOAL_STATUS_INVALID_ARGUMENT, errors.New("shoal: out_client is required"))
	}
	ctx, cancel, code, err := openContext(timeout)
	if err != nil {
		return fail(outError, code, err)
	}
	defer cancel()
	client, err := publichdfs.NewContext(ctx, optionalString(address))
	if err != nil {
		return failForError(outError, err)
	}
	owned := &ownedHDFSClient{client: client}
	id, ok := hdfsClients.add(owned)
	if !ok {
		_ = client.Close()
		return fail(outError, C.SHOAL_STATUS_INTERNAL, errors.New("shoal: HDFS client handle space exhausted"))
	}
	handle := C.shoal_bridge_hdfs_client_alloc(C.uint64_t(id))
	if handle == nil {
		hdfsClients.remove(id)
		_ = client.Close()
		return fail(outError, C.SHOAL_STATUS_OUT_OF_MEMORY, errors.New("shoal: allocate HDFS client handle"))
	}
	*out = handle
	return C.SHOAL_STATUS_OK
}

//export shoal_hdfs_client_close
func shoal_hdfs_client_close(handle *C.shoal_hdfs_client, outError **C.shoal_error) (status C.shoal_status) {
	clearError(outError)
	defer recoverStatus(&status, outError)
	client, err := lookupHDFSClient(handle)
	if err != nil {
		return fail(outError, C.SHOAL_STATUS_INVALID_HANDLE, err)
	}
	if err := client.lifecycle.close(client.client.Close); err != nil {
		return failForError(outError, err)
	}
	return C.SHOAL_STATUS_OK
}

//export shoal_hdfs_client_free
func shoal_hdfs_client_free(handle **C.shoal_hdfs_client) {
	if handle == nil || *handle == nil {
		return
	}
	id := uint64(C.shoal_bridge_hdfs_client_id(*handle))
	if client, ok := hdfsClients.remove(id); ok {
		client.lifecycle.closeBounded(client.client.Close)
	}
	C.shoal_bridge_hdfs_client_free(*handle)
	*handle = nil
}

//export shoal_hdfs_client_open
func shoal_hdfs_client_open(handle *C.shoal_hdfs_client, path *C.char, timeout C.int64_t, out **C.shoal_hdfs_input_stream, outError **C.shoal_error) (status C.shoal_status) {
	clearError(outError)
	if out != nil {
		*out = nil
	}
	defer recoverStatus(&status, outError)
	if out == nil {
		return fail(outError, C.SHOAL_STATUS_INVALID_ARGUMENT, errors.New("shoal: out_stream is required"))
	}
	client, err := lookupHDFSClient(handle)
	if err != nil {
		return fail(outError, C.SHOAL_STATUS_INVALID_HANDLE, err)
	}
	name, err := requiredString(path, "path")
	if err != nil {
		return fail(outError, C.SHOAL_STATUS_INVALID_ARGUMENT, err)
	}
	ctx, done, code, err := client.lifecycle.begin(timeout)
	if err != nil {
		return fail(outError, code, err)
	}
	defer done()
	stream, err := client.client.Open(ctx, name)
	if err != nil {
		return failForError(outError, err)
	}
	owned := &ownedHDFSInput{stream: stream}
	id, ok := hdfsInputs.add(owned)
	if !ok {
		_ = stream.Close()
		return fail(outError, C.SHOAL_STATUS_INTERNAL, errors.New("shoal: HDFS input handle space exhausted"))
	}
	streamHandle := C.shoal_bridge_hdfs_input_stream_alloc(C.uint64_t(id))
	if streamHandle == nil {
		hdfsInputs.remove(id)
		_ = stream.Close()
		return fail(outError, C.SHOAL_STATUS_OUT_OF_MEMORY, errors.New("shoal: allocate HDFS input handle"))
	}
	*out = streamHandle
	return C.SHOAL_STATUS_OK
}

//export shoal_hdfs_client_create_file
func shoal_hdfs_client_create_file(handle *C.shoal_hdfs_client, path *C.char, timeout C.int64_t, out **C.shoal_hdfs_output_stream, outError **C.shoal_error) (status C.shoal_status) {
	clearError(outError)
	if out != nil {
		*out = nil
	}
	defer recoverStatus(&status, outError)
	if out == nil {
		return fail(outError, C.SHOAL_STATUS_INVALID_ARGUMENT, errors.New("shoal: out_stream is required"))
	}
	client, err := lookupHDFSClient(handle)
	if err != nil {
		return fail(outError, C.SHOAL_STATUS_INVALID_HANDLE, err)
	}
	name, err := requiredString(path, "path")
	if err != nil {
		return fail(outError, C.SHOAL_STATUS_INVALID_ARGUMENT, err)
	}
	ctx, done, code, err := client.lifecycle.begin(timeout)
	if err != nil {
		return fail(outError, code, err)
	}
	defer done()
	stream, err := client.client.Create(ctx, name)
	if err != nil {
		return failForError(outError, err)
	}
	owned := &ownedHDFSOutput{stream: stream}
	id, ok := hdfsOutputs.add(owned)
	if !ok {
		_ = stream.Close()
		return fail(outError, C.SHOAL_STATUS_INTERNAL, errors.New("shoal: HDFS output handle space exhausted"))
	}
	streamHandle := C.shoal_bridge_hdfs_output_stream_alloc(C.uint64_t(id))
	if streamHandle == nil {
		hdfsOutputs.remove(id)
		_ = stream.Close()
		return fail(outError, C.SHOAL_STATUS_OUT_OF_MEMORY, errors.New("shoal: allocate HDFS output handle"))
	}
	*out = streamHandle
	return C.SHOAL_STATUS_OK
}

func beginHDFSClient(handle *C.shoal_hdfs_client, timeout C.int64_t) (*ownedHDFSClient, context.Context, func(), C.shoal_status, error) {
	client, err := lookupHDFSClient(handle)
	if err != nil {
		return nil, nil, nil, C.SHOAL_STATUS_INVALID_HANDLE, err
	}
	ctx, done, code, err := client.lifecycle.begin(timeout)
	return client, ctx, done, code, err
}

//export shoal_hdfs_client_list
func shoal_hdfs_client_list(handle *C.shoal_hdfs_client, path *C.char, timeout C.int64_t, out **C.shoal_hdfs_dir_list_result, outError **C.shoal_error) (status C.shoal_status) {
	clearError(outError)
	if out != nil {
		*out = nil
	}
	defer recoverStatus(&status, outError)
	if out == nil {
		return fail(outError, C.SHOAL_STATUS_INVALID_ARGUMENT, errors.New("shoal: out_result is required"))
	}
	name, err := requiredString(path, "path")
	if err != nil {
		return fail(outError, C.SHOAL_STATUS_INVALID_ARGUMENT, err)
	}
	client, ctx, done, code, err := beginHDFSClient(handle, timeout)
	if err != nil {
		return fail(outError, code, err)
	}
	defer done()
	entries, err := client.client.List(ctx, name)
	if err != nil {
		return failForError(outError, err)
	}
	result := C.shoal_bridge_hdfs_dir_list_alloc(C.size_t(len(entries)))
	if result == nil {
		return fail(outError, C.SHOAL_STATUS_OUT_OF_MEMORY, errors.New("shoal: allocate HDFS directory list"))
	}
	for index, entry := range entries {
		entryResult := allocHDFSEntry(entry)
		if entryResult == nil {
			C.shoal_bridge_hdfs_dir_list_free(result)
			return fail(outError, C.SHOAL_STATUS_OUT_OF_MEMORY, errors.New("shoal: allocate HDFS directory entry"))
		}
		var view C.shoal_hdfs_dir_entry_view
		shoal_hdfs_dir_entry_view_init(&view)
		_ = C.shoal_bridge_hdfs_dir_entry_get(entryResult, &view)
		if C.shoal_bridge_hdfs_dir_list_set(result, C.size_t(index), view.name, view.owner, view.group, view.size, view.modification_time_ms, view.mode, view.is_directory) == 0 {
			C.shoal_bridge_hdfs_dir_entry_free(entryResult)
			C.shoal_bridge_hdfs_dir_list_free(result)
			return fail(outError, C.SHOAL_STATUS_OUT_OF_MEMORY, errors.New("shoal: copy HDFS directory entry"))
		}
		C.shoal_bridge_hdfs_dir_entry_free(entryResult)
	}
	*out = result
	return C.SHOAL_STATUS_OK
}

//export shoal_hdfs_client_stat
func shoal_hdfs_client_stat(handle *C.shoal_hdfs_client, path *C.char, timeout C.int64_t, out **C.shoal_hdfs_dir_entry_result, outError **C.shoal_error) (status C.shoal_status) {
	clearError(outError)
	if out != nil {
		*out = nil
	}
	defer recoverStatus(&status, outError)
	if out == nil {
		return fail(outError, C.SHOAL_STATUS_INVALID_ARGUMENT, errors.New("shoal: out_result is required"))
	}
	name, err := requiredString(path, "path")
	if err != nil {
		return fail(outError, C.SHOAL_STATUS_INVALID_ARGUMENT, err)
	}
	client, ctx, done, code, err := beginHDFSClient(handle, timeout)
	if err != nil {
		return fail(outError, code, err)
	}
	defer done()
	entry, err := client.client.Stat(ctx, name)
	if err != nil {
		return failForError(outError, err)
	}
	result := allocHDFSEntry(entry)
	if result == nil {
		return fail(outError, C.SHOAL_STATUS_OUT_OF_MEMORY, errors.New("shoal: allocate HDFS stat result"))
	}
	*out = result
	return C.SHOAL_STATUS_OK
}

//export shoal_hdfs_client_remove
func shoal_hdfs_client_remove(handle *C.shoal_hdfs_client, path *C.char, recursive C.uint8_t, timeout C.int64_t, outError **C.shoal_error) (status C.shoal_status) {
	clearError(outError)
	defer recoverStatus(&status, outError)
	name, err := requiredString(path, "path")
	if err != nil {
		return fail(outError, C.SHOAL_STATUS_INVALID_ARGUMENT, err)
	}
	flag, err := boolFlag(recursive, "recursive")
	if err != nil {
		return fail(outError, C.SHOAL_STATUS_INVALID_ARGUMENT, err)
	}
	client, ctx, done, code, err := beginHDFSClient(handle, timeout)
	if err != nil {
		return fail(outError, code, err)
	}
	defer done()
	if err := client.client.Remove(ctx, name, flag); err != nil {
		return failForError(outError, err)
	}
	return C.SHOAL_STATUS_OK
}

//export shoal_hdfs_client_rename
func shoal_hdfs_client_rename(handle *C.shoal_hdfs_client, oldPath *C.char, newPath *C.char, timeout C.int64_t, outError **C.shoal_error) (status C.shoal_status) {
	clearError(outError)
	defer recoverStatus(&status, outError)
	oldName, err := requiredString(oldPath, "old_path")
	if err != nil {
		return fail(outError, C.SHOAL_STATUS_INVALID_ARGUMENT, err)
	}
	newName, err := requiredString(newPath, "new_path")
	if err != nil {
		return fail(outError, C.SHOAL_STATUS_INVALID_ARGUMENT, err)
	}
	client, ctx, done, code, err := beginHDFSClient(handle, timeout)
	if err != nil {
		return fail(outError, code, err)
	}
	defer done()
	if err := client.client.Rename(ctx, oldName, newName); err != nil {
		return failForError(outError, err)
	}
	return C.SHOAL_STATUS_OK
}

//export shoal_hdfs_client_mkdir
func shoal_hdfs_client_mkdir(handle *C.shoal_hdfs_client, path *C.char, timeout C.int64_t, outError **C.shoal_error) (status C.shoal_status) {
	clearError(outError)
	defer recoverStatus(&status, outError)
	name, err := requiredString(path, "path")
	if err != nil {
		return fail(outError, C.SHOAL_STATUS_INVALID_ARGUMENT, err)
	}
	client, ctx, done, code, err := beginHDFSClient(handle, timeout)
	if err != nil {
		return fail(outError, code, err)
	}
	defer done()
	if err := client.client.Mkdir(ctx, name); err != nil {
		return failForError(outError, err)
	}
	return C.SHOAL_STATUS_OK
}

//export shoal_hdfs_client_chown
func shoal_hdfs_client_chown(handle *C.shoal_hdfs_client, path *C.char, owner *C.char, group *C.char, timeout C.int64_t, outError **C.shoal_error) (status C.shoal_status) {
	clearError(outError)
	defer recoverStatus(&status, outError)
	name, err := requiredString(path, "path")
	if err != nil {
		return fail(outError, C.SHOAL_STATUS_INVALID_ARGUMENT, err)
	}
	ownerName := optionalString(owner)
	groupName := optionalString(group)
	if ownerName == "" && groupName == "" {
		return fail(outError, C.SHOAL_STATUS_INVALID_ARGUMENT, errors.New("shoal: owner or group is required"))
	}
	client, ctx, done, code, err := beginHDFSClient(handle, timeout)
	if err != nil {
		return fail(outError, code, err)
	}
	defer done()
	if err := client.client.Chown(ctx, name, ownerName, groupName); err != nil {
		return failForError(outError, err)
	}
	return C.SHOAL_STATUS_OK
}

//export shoal_hdfs_input_stream_read
func shoal_hdfs_input_stream_read(handle *C.shoal_hdfs_input_stream, length C.size_t, timeout C.int64_t, out **C.shoal_bytes_result, outError **C.shoal_error) (status C.shoal_status) {
	clearError(outError)
	if out != nil {
		*out = nil
	}
	defer recoverStatus(&status, outError)
	if out == nil {
		return fail(outError, C.SHOAL_STATUS_INVALID_ARGUMENT, errors.New("shoal: out_result is required"))
	}
	if uint64(length) > uint64(^uint(0)>>1) {
		return fail(outError, C.SHOAL_STATUS_INVALID_ARGUMENT, errors.New("shoal: read length is too large"))
	}
	stream, err := lookupHDFSInput(handle)
	if err != nil {
		return fail(outError, C.SHOAL_STATUS_INVALID_HANDLE, err)
	}
	ctx, done, code, err := stream.lifecycle.begin(timeout)
	if err != nil {
		return fail(outError, code, err)
	}
	defer done()
	buf := make([]byte, int(length))
	n, readErr := stream.stream.Read(ctx, buf)
	if readErr != nil && !errors.Is(readErr, io.EOF) {
		return failForError(outError, readErr)
	}
	result := C.shoal_bridge_bytes_result_alloc(bytePointer(buf[:n]), C.size_t(n))
	if result == nil {
		return fail(outError, C.SHOAL_STATUS_OUT_OF_MEMORY, errors.New("shoal: allocate HDFS read result"))
	}
	*out = result
	return C.SHOAL_STATUS_OK
}

//export shoal_hdfs_input_stream_close
func shoal_hdfs_input_stream_close(handle *C.shoal_hdfs_input_stream, outError **C.shoal_error) (status C.shoal_status) {
	clearError(outError)
	defer recoverStatus(&status, outError)
	stream, err := lookupHDFSInput(handle)
	if err != nil {
		return fail(outError, C.SHOAL_STATUS_INVALID_HANDLE, err)
	}
	if err := stream.lifecycle.close(stream.stream.Close); err != nil {
		return failForError(outError, err)
	}
	return C.SHOAL_STATUS_OK
}

//export shoal_hdfs_input_stream_free
func shoal_hdfs_input_stream_free(handle **C.shoal_hdfs_input_stream) {
	if handle == nil || *handle == nil {
		return
	}
	id := uint64(C.shoal_bridge_hdfs_input_stream_id(*handle))
	if stream, ok := hdfsInputs.remove(id); ok {
		stream.lifecycle.closeBounded(stream.stream.Close)
	}
	C.shoal_bridge_hdfs_input_stream_free(*handle)
	*handle = nil
}

//export shoal_hdfs_output_stream_write
func shoal_hdfs_output_stream_write(handle *C.shoal_hdfs_output_stream, value C.shoal_bytes, timeout C.int64_t, outWritten *C.size_t, outError **C.shoal_error) (status C.shoal_status) {
	clearError(outError)
	if outWritten != nil {
		*outWritten = 0
	}
	defer recoverStatus(&status, outError)
	if outWritten == nil {
		return fail(outError, C.SHOAL_STATUS_INVALID_ARGUMENT, errors.New("shoal: out_written is required"))
	}
	data, err := copyBytes(value.data, value.length, "value")
	if err != nil {
		return fail(outError, C.SHOAL_STATUS_INVALID_ARGUMENT, err)
	}
	stream, err := lookupHDFSOutput(handle)
	if err != nil {
		return fail(outError, C.SHOAL_STATUS_INVALID_HANDLE, err)
	}
	ctx, done, code, err := stream.lifecycle.begin(timeout)
	if err != nil {
		return fail(outError, code, err)
	}
	defer done()
	n, err := stream.stream.Write(ctx, data)
	*outWritten = C.size_t(n)
	if err != nil {
		return failForError(outError, err)
	}
	if n != len(data) {
		return failForError(outError, io.ErrShortWrite)
	}
	return C.SHOAL_STATUS_OK
}

//export shoal_hdfs_output_stream_close
func shoal_hdfs_output_stream_close(handle *C.shoal_hdfs_output_stream, outError **C.shoal_error) (status C.shoal_status) {
	clearError(outError)
	defer recoverStatus(&status, outError)
	stream, err := lookupHDFSOutput(handle)
	if err != nil {
		return fail(outError, C.SHOAL_STATUS_INVALID_HANDLE, err)
	}
	if err := stream.lifecycle.close(stream.stream.Close); err != nil {
		return failForError(outError, err)
	}
	return C.SHOAL_STATUS_OK
}

//export shoal_hdfs_output_stream_free
func shoal_hdfs_output_stream_free(handle **C.shoal_hdfs_output_stream) {
	if handle == nil || *handle == nil {
		return
	}
	id := uint64(C.shoal_bridge_hdfs_output_stream_id(*handle))
	if stream, ok := hdfsOutputs.remove(id); ok {
		stream.lifecycle.closeBounded(stream.stream.Close)
	}
	C.shoal_bridge_hdfs_output_stream_free(*handle)
	*handle = nil
}

//export shoal_hdfs_dir_list_count
func shoal_hdfs_dir_list_count(result *C.shoal_hdfs_dir_list_result) C.size_t {
	return C.shoal_bridge_hdfs_dir_list_count(result)
}

//export shoal_hdfs_dir_list_get
func shoal_hdfs_dir_list_get(result *C.shoal_hdfs_dir_list_result, index C.size_t, out *C.shoal_hdfs_dir_entry_view, outError **C.shoal_error) (status C.shoal_status) {
	clearError(outError)
	defer recoverStatus(&status, outError)
	if out == nil || out.struct_size < C.SHOAL_HDFS_DIR_ENTRY_VIEW_V1_SIZE {
		return fail(outError, C.SHOAL_STATUS_INVALID_ARGUMENT, errors.New("shoal: HDFS directory entry view is required"))
	}
	if C.shoal_bridge_hdfs_dir_list_get(result, index, out) == 0 {
		return fail(outError, C.SHOAL_STATUS_INVALID_ARGUMENT, errors.New("shoal: HDFS directory entry index is out of range"))
	}
	return C.SHOAL_STATUS_OK
}

//export shoal_hdfs_dir_list_free
func shoal_hdfs_dir_list_free(result **C.shoal_hdfs_dir_list_result) {
	if result != nil && *result != nil {
		C.shoal_bridge_hdfs_dir_list_free(*result)
		*result = nil
	}
}

//export shoal_hdfs_dir_entry_result_get
func shoal_hdfs_dir_entry_result_get(result *C.shoal_hdfs_dir_entry_result, out *C.shoal_hdfs_dir_entry_view, outError **C.shoal_error) (status C.shoal_status) {
	clearError(outError)
	defer recoverStatus(&status, outError)
	if out == nil || out.struct_size < C.SHOAL_HDFS_DIR_ENTRY_VIEW_V1_SIZE {
		return fail(outError, C.SHOAL_STATUS_INVALID_ARGUMENT, errors.New("shoal: HDFS directory entry view is required"))
	}
	if C.shoal_bridge_hdfs_dir_entry_get(result, out) == 0 {
		return fail(outError, C.SHOAL_STATUS_INVALID_ARGUMENT, errors.New("shoal: HDFS directory entry result is required"))
	}
	return C.SHOAL_STATUS_OK
}

//export shoal_hdfs_dir_entry_result_free
func shoal_hdfs_dir_entry_result_free(result **C.shoal_hdfs_dir_entry_result) {
	if result != nil && *result != nil {
		C.shoal_bridge_hdfs_dir_entry_free(*result)
		*result = nil
	}
}

var _ = time.Second
