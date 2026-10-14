package main

/*
#cgo CFLAGS: -I${SRCDIR}/../../capi/include
#include "bridge.h"
#include <string.h>
*/
import "C"

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"
	"unsafe"

	publicrfile "github.com/phrocker/shoal-oss/rfile"
)

var rfileFreeTimeout = 5 * time.Second

type rfileLifecycle struct {
	mu        sync.Mutex
	closed    bool
	nextID    uint64
	cancels   map[uint64]context.CancelFunc
	active    sync.WaitGroup
	closeOnce sync.Once
	closeErr  error
}

func (l *rfileLifecycle) begin(timeout C.int64_t) (context.Context, func(), C.shoal_status, error) {
	duration, err := durationMilliseconds(timeout, "timeout_ms", 0)
	if err != nil {
		return nil, nil, C.SHOAL_STATUS_INVALID_ARGUMENT, err
	}
	l.mu.Lock()
	if l.closed {
		l.mu.Unlock()
		return nil, nil, C.SHOAL_STATUS_CLOSED, publicrfile.ErrClosed
	}
	if l.nextID == 0 {
		l.nextID = 1
	}
	if l.cancels == nil {
		l.cancels = make(map[uint64]context.CancelFunc)
	}
	var id uint64
	for attempts := uint64(0); attempts < ^uint64(0); attempts++ {
		id = l.nextID
		l.nextID++
		if l.nextID == 0 {
			l.nextID = 1
		}
		if id != 0 {
			if _, exists := l.cancels[id]; !exists {
				break
			}
		}
		id = 0
	}
	if id == 0 {
		l.mu.Unlock()
		return nil, nil, C.SHOAL_STATUS_INTERNAL, errors.New("shoal: RFile operation space exhausted")
	}
	var ctx context.Context
	var cancel context.CancelFunc
	if duration == 0 {
		ctx, cancel = context.WithCancel(context.Background())
	} else {
		ctx, cancel = context.WithTimeout(context.Background(), duration)
	}
	l.cancels[id] = cancel
	l.active.Add(1)
	l.mu.Unlock()

	var once sync.Once
	return ctx, func() {
		once.Do(func() {
			l.mu.Lock()
			delete(l.cancels, id)
			l.mu.Unlock()
			cancel()
			l.active.Done()
		})
	}, C.SHOAL_STATUS_OK, nil
}

func (l *rfileLifecycle) retain() (func(), error) {
	l.mu.Lock()
	if l.closed {
		l.mu.Unlock()
		return nil, publicrfile.ErrClosed
	}
	l.active.Add(1)
	l.mu.Unlock()
	var once sync.Once
	return func() { once.Do(l.active.Done) }, nil
}

func (l *rfileLifecycle) close(closeFn func() error) error {
	l.closeOnce.Do(func() {
		l.mu.Lock()
		l.closed = true
		for _, cancel := range l.cancels {
			cancel()
		}
		l.mu.Unlock()
		l.active.Wait()
		l.closeErr = closeFn()
	})
	return l.closeErr
}

func (l *rfileLifecycle) closeBounded(closeFn func() error) {
	done := make(chan struct{})
	go func() {
		_ = l.close(closeFn)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(rfileFreeTimeout):
	}
}

type ownedRFileReader struct {
	lifecycle rfileLifecycle
	reader    *publicrfile.Reader
}

type ownedRFileWriter struct {
	lifecycle rfileLifecycle
	writer    *publicrfile.Writer
}

type ownedRFileSeekable struct {
	seekable  publicrfile.Seekable
	startKind rangeBoundKind
	endKind   rangeBoundKind
}

type rfileRegistry[T any] struct {
	mu     sync.RWMutex
	nextID uint64
	items  map[uint64]*T
}

func newRFileRegistry[T any]() *rfileRegistry[T] {
	return &rfileRegistry[T]{nextID: 1, items: make(map[uint64]*T)}
}

func (r *rfileRegistry[T]) add(value *T) (uint64, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for attempts := uint64(0); attempts < ^uint64(0); attempts++ {
		id := r.nextID
		r.nextID++
		if r.nextID == 0 {
			r.nextID = 1
		}
		if id != 0 {
			if _, exists := r.items[id]; !exists {
				r.items[id] = value
				return id, true
			}
		}
	}
	return 0, false
}

func (r *rfileRegistry[T]) get(id uint64) (*T, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	value, ok := r.items[id]
	return value, ok
}

func (r *rfileRegistry[T]) remove(id uint64) (*T, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	value, ok := r.items[id]
	if ok {
		delete(r.items, id)
	}
	return value, ok
}

var (
	rfileReaders   = newRFileRegistry[ownedRFileReader]()
	rfileWriters   = newRFileRegistry[ownedRFileWriter]()
	rfileSeekables = newRFileRegistry[ownedRFileSeekable]()
)

//export shoal_rfile_writer_config_init
func shoal_rfile_writer_config_init(config *C.shoal_rfile_writer_config) {
	if config != nil {
		C.memset(unsafe.Pointer(config), 0, C.size_t(C.SHOAL_RFILE_WRITER_CONFIG_V1_SIZE))
		config.struct_size = C.SHOAL_RFILE_WRITER_CONFIG_V1_SIZE
	}
}

//export shoal_rfile_merge_config_init
func shoal_rfile_merge_config_init(config *C.shoal_rfile_merge_config) {
	if config != nil {
		C.memset(unsafe.Pointer(config), 0, C.size_t(C.SHOAL_RFILE_MERGE_CONFIG_V1_SIZE))
		config.struct_size = C.SHOAL_RFILE_MERGE_CONFIG_V1_SIZE
	}
}

//export shoal_rfile_entry_init
func shoal_rfile_entry_init(entry *C.shoal_rfile_entry) {
	if entry != nil {
		C.memset(unsafe.Pointer(entry), 0, C.size_t(C.SHOAL_RFILE_ENTRY_V1_SIZE))
		entry.struct_size = C.SHOAL_RFILE_ENTRY_V1_SIZE
	}
}

//export shoal_rfile_entry_view_init
func shoal_rfile_entry_view_init(view *C.shoal_rfile_entry_view) {
	if view != nil {
		C.memset(unsafe.Pointer(view), 0, C.size_t(C.SHOAL_RFILE_ENTRY_VIEW_V1_SIZE))
		view.struct_size = C.SHOAL_RFILE_ENTRY_VIEW_V1_SIZE
	}
}

func parseRFileWriterConfig(config *C.shoal_rfile_writer_config) (publicrfile.WriterOptions, error) {
	if config == nil {
		return publicrfile.WriterOptions{}, nil
	}
	if config.struct_size < C.SHOAL_RFILE_WRITER_CONFIG_V1_SIZE {
		return publicrfile.WriterOptions{}, errors.New("shoal: RFile writer config is too small")
	}
	if config.block_size < 0 {
		return publicrfile.WriterOptions{}, errors.New("shoal: RFile block_size must not be negative")
	}
	if int64(int(config.block_size)) != int64(config.block_size) {
		return publicrfile.WriterOptions{}, errors.New("shoal: RFile block_size is too large")
	}
	return publicrfile.WriterOptions{
		Codec:     optionalString(config.codec),
		BlockSize: int(config.block_size),
	}, nil
}

func parseRFileMergeConfig(config *C.shoal_rfile_merge_config) (publicrfile.MergeOptions, error) {
	if config == nil {
		return publicrfile.MergeOptions{}, nil
	}
	if config.struct_size < C.SHOAL_RFILE_MERGE_CONFIG_V1_SIZE {
		return publicrfile.MergeOptions{}, errors.New("shoal: RFile merge config is too small")
	}
	applyDeletes, err := boolFlag(config.apply_deletes, "apply_deletes")
	if err != nil {
		return publicrfile.MergeOptions{}, err
	}
	propagate, err := boolFlag(config.propagate, "propagate")
	if err != nil {
		return publicrfile.MergeOptions{}, err
	}
	return publicrfile.MergeOptions{
		Versions:     int(config.versions),
		ApplyDeletes: applyDeletes,
		Propagate:    propagate,
		MinTimestamp: int64(config.min_timestamp),
	}, nil
}

func openContext(timeout C.int64_t) (context.Context, context.CancelFunc, C.shoal_status, error) {
	duration, err := durationMilliseconds(timeout, "timeout_ms", 0)
	if err != nil {
		return nil, nil, C.SHOAL_STATUS_INVALID_ARGUMENT, err
	}
	if duration == 0 {
		ctx, cancel := context.WithCancel(context.Background())
		return ctx, cancel, C.SHOAL_STATUS_OK, nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), duration)
	return ctx, cancel, C.SHOAL_STATUS_OK, nil
}

func registerRFileWriter(writer *publicrfile.Writer) (*C.shoal_rfile_writer, C.shoal_status, error) {
	owned := &ownedRFileWriter{writer: writer}
	id, ok := rfileWriters.add(owned)
	if !ok {
		_ = writer.Close()
		return nil, C.SHOAL_STATUS_INTERNAL, errors.New("shoal: RFile writer handle space exhausted")
	}
	handle := C.shoal_bridge_rfile_writer_alloc(C.uint64_t(id))
	if handle == nil {
		rfileWriters.remove(id)
		_ = writer.Close()
		return nil, C.SHOAL_STATUS_OUT_OF_MEMORY, errors.New("shoal: allocate RFile writer handle")
	}
	return handle, C.SHOAL_STATUS_OK, nil
}

func registerRFileReader(reader *publicrfile.Reader) (*C.shoal_rfile_reader, C.shoal_status, error) {
	owned := &ownedRFileReader{reader: reader}
	id, ok := rfileReaders.add(owned)
	if !ok {
		_ = reader.Close()
		return nil, C.SHOAL_STATUS_INTERNAL, errors.New("shoal: RFile reader handle space exhausted")
	}
	handle := C.shoal_bridge_rfile_reader_alloc(C.uint64_t(id))
	if handle == nil {
		rfileReaders.remove(id)
		_ = reader.Close()
		return nil, C.SHOAL_STATUS_OUT_OF_MEMORY, errors.New("shoal: allocate RFile reader handle")
	}
	return handle, C.SHOAL_STATUS_OK, nil
}

func lookupRFileWriter(handle *C.shoal_rfile_writer) (*ownedRFileWriter, error) {
	if handle == nil {
		return nil, errors.New("shoal: RFile writer handle is NULL")
	}
	id := uint64(C.shoal_bridge_rfile_writer_id(handle))
	value, ok := rfileWriters.get(id)
	if id == 0 || !ok {
		return nil, errors.New("shoal: RFile writer handle is unknown or freed")
	}
	return value, nil
}

func lookupRFileReader(handle *C.shoal_rfile_reader) (*ownedRFileReader, error) {
	if handle == nil {
		return nil, errors.New("shoal: RFile reader handle is NULL")
	}
	id := uint64(C.shoal_bridge_rfile_reader_id(handle))
	value, ok := rfileReaders.get(id)
	if id == 0 || !ok {
		return nil, errors.New("shoal: RFile reader handle is unknown or freed")
	}
	return value, nil
}

func lookupRFileSeekable(handle *C.shoal_rfile_seekable) (*ownedRFileSeekable, error) {
	if handle == nil {
		return nil, errors.New("shoal: RFile seekable handle is NULL")
	}
	id := uint64(C.shoal_bridge_rfile_seekable_id(handle))
	value, ok := rfileSeekables.get(id)
	if id == 0 || !ok {
		return nil, errors.New("shoal: RFile seekable handle is unknown or freed")
	}
	return value, nil
}

//export shoal_rfile_writer_create
func shoal_rfile_writer_create(path *C.char, config *C.shoal_rfile_writer_config, timeout C.int64_t, out **C.shoal_rfile_writer, outError **C.shoal_error) (status C.shoal_status) {
	clearError(outError)
	if out != nil {
		*out = nil
	}
	defer recoverStatus(&status, outError)
	if out == nil {
		return fail(outError, C.SHOAL_STATUS_INVALID_ARGUMENT, errors.New("shoal: out_writer is required"))
	}
	name, err := requiredString(path, "path")
	if err != nil {
		return fail(outError, C.SHOAL_STATUS_INVALID_ARGUMENT, err)
	}
	options, err := parseRFileWriterConfig(config)
	if err != nil {
		return fail(outError, C.SHOAL_STATUS_INVALID_ARGUMENT, err)
	}
	ctx, cancel, code, err := openContext(timeout)
	if err != nil {
		return fail(outError, code, err)
	}
	defer cancel()
	writer, err := publicrfile.Create(ctx, name, options)
	if err != nil {
		return failForError(outError, err)
	}
	handle, code, err := registerRFileWriter(writer)
	if err != nil {
		return fail(outError, code, err)
	}
	*out = handle
	return C.SHOAL_STATUS_OK
}

func parseRFileEntry(input *C.shoal_rfile_entry) (publicrfile.Entry, error) {
	if input == nil {
		return publicrfile.Entry{}, errors.New("shoal: RFile entry is required")
	}
	if input.struct_size < C.SHOAL_RFILE_ENTRY_V1_SIZE {
		return publicrfile.Entry{}, errors.New("shoal: RFile entry is too small")
	}
	key, err := parseKey(input.key, "RFile entry key")
	if err != nil {
		return publicrfile.Entry{}, err
	}
	value, err := copyBytes(input.value.data, input.value.length, "RFile entry value")
	if err != nil {
		return publicrfile.Entry{}, err
	}
	deleted, err := boolFlag(input.deleted, "deleted")
	if err != nil {
		return publicrfile.Entry{}, err
	}
	return publicrfile.Entry{Key: *key, Value: value, Deleted: deleted}, nil
}

//export shoal_rfile_writer_append
func shoal_rfile_writer_append(handle *C.shoal_rfile_writer, input *C.shoal_rfile_entry, timeout C.int64_t, outError **C.shoal_error) (status C.shoal_status) {
	clearError(outError)
	defer recoverStatus(&status, outError)
	writer, err := lookupRFileWriter(handle)
	if err != nil {
		return fail(outError, C.SHOAL_STATUS_INVALID_HANDLE, err)
	}
	entry, err := parseRFileEntry(input)
	if err != nil {
		return fail(outError, C.SHOAL_STATUS_INVALID_ARGUMENT, err)
	}
	ctx, done, code, err := writer.lifecycle.begin(timeout)
	if err != nil {
		return fail(outError, code, err)
	}
	defer done()
	if err := writer.writer.Append(ctx, entry); err != nil {
		return failForError(outError, err)
	}
	return C.SHOAL_STATUS_OK
}

//export shoal_rfile_writer_add_locality_group
func shoal_rfile_writer_add_locality_group(handle *C.shoal_rfile_writer, groupName *C.char, timeout C.int64_t, outError **C.shoal_error) (status C.shoal_status) {
	clearError(outError)
	defer recoverStatus(&status, outError)
	writer, err := lookupRFileWriter(handle)
	if err != nil {
		return fail(outError, C.SHOAL_STATUS_INVALID_HANDLE, err)
	}
	name, err := requiredString(groupName, "name")
	if err != nil {
		return fail(outError, C.SHOAL_STATUS_INVALID_ARGUMENT, err)
	}
	ctx, done, code, err := writer.lifecycle.begin(timeout)
	if err != nil {
		return fail(outError, code, err)
	}
	defer done()
	if err := ctx.Err(); err != nil {
		return failForError(outError, err)
	}
	if err := writer.writer.AddLocalityGroup(name); err != nil {
		return failForError(outError, err)
	}
	return C.SHOAL_STATUS_OK
}

//export shoal_rfile_writer_entries
func shoal_rfile_writer_entries(handle *C.shoal_rfile_writer) C.int64_t {
	writer, err := lookupRFileWriter(handle)
	if err != nil {
		return 0
	}
	done, err := writer.lifecycle.retain()
	if err != nil {
		return 0
	}
	defer done()
	return C.int64_t(writer.writer.Entries())
}

//export shoal_rfile_writer_close
func shoal_rfile_writer_close(handle *C.shoal_rfile_writer, outError **C.shoal_error) (status C.shoal_status) {
	clearError(outError)
	defer recoverStatus(&status, outError)
	writer, err := lookupRFileWriter(handle)
	if err != nil {
		return fail(outError, C.SHOAL_STATUS_INVALID_HANDLE, err)
	}
	if err := writer.lifecycle.close(writer.writer.Close); err != nil {
		return failForError(outError, err)
	}
	return C.SHOAL_STATUS_OK
}

//export shoal_rfile_writer_free
func shoal_rfile_writer_free(handle **C.shoal_rfile_writer) {
	if handle == nil || *handle == nil {
		return
	}
	value := *handle
	*handle = nil
	id := uint64(C.shoal_bridge_rfile_writer_id(value))
	if writer, ok := rfileWriters.remove(id); ok {
		writer.lifecycle.closeBounded(writer.writer.Close)
	}
	C.shoal_bridge_rfile_writer_free(value)
}

type rfileOpenMode int

const (
	rfileOpenRandom rfileOpenMode = iota
	rfileOpenSequential
)

func openRFileReader(path *C.char, timeout C.int64_t, out **C.shoal_rfile_reader, outError **C.shoal_error, mode rfileOpenMode) C.shoal_status {
	if out != nil {
		*out = nil
	}
	if out == nil {
		return fail(outError, C.SHOAL_STATUS_INVALID_ARGUMENT, errors.New("shoal: out_reader is required"))
	}
	name, err := requiredString(path, "path")
	if err != nil {
		return fail(outError, C.SHOAL_STATUS_INVALID_ARGUMENT, err)
	}
	ctx, cancel, code, err := openContext(timeout)
	if err != nil {
		return fail(outError, code, err)
	}
	defer cancel()
	var reader *publicrfile.Reader
	if mode == rfileOpenSequential {
		reader, err = publicrfile.OpenSequential(ctx, name)
	} else {
		reader, err = publicrfile.Open(ctx, name)
	}
	if err != nil {
		return failForError(outError, err)
	}
	handle, code, err := registerRFileReader(reader)
	if err != nil {
		return fail(outError, code, err)
	}
	*out = handle
	return C.SHOAL_STATUS_OK
}

//export shoal_rfile_reader_open
func shoal_rfile_reader_open(path *C.char, timeout C.int64_t, out **C.shoal_rfile_reader, outError **C.shoal_error) (status C.shoal_status) {
	clearError(outError)
	defer recoverStatus(&status, outError)
	return openRFileReader(path, timeout, out, outError, rfileOpenRandom)
}

//export shoal_rfile_reader_open_sequential
func shoal_rfile_reader_open_sequential(path *C.char, timeout C.int64_t, out **C.shoal_rfile_reader, outError **C.shoal_error) (status C.shoal_status) {
	clearError(outError)
	defer recoverStatus(&status, outError)
	return openRFileReader(path, timeout, out, outError, rfileOpenSequential)
}

//export shoal_rfile_reader_open_many
func shoal_rfile_reader_open_many(paths **C.char, count C.size_t, config *C.shoal_rfile_merge_config, timeout C.int64_t, out **C.shoal_rfile_reader, outError **C.shoal_error) (status C.shoal_status) {
	clearError(outError)
	if out != nil {
		*out = nil
	}
	defer recoverStatus(&status, outError)
	if out == nil {
		return fail(outError, C.SHOAL_STATUS_INVALID_ARGUMENT, errors.New("shoal: out_reader is required"))
	}
	if paths == nil || count == 0 {
		return fail(outError, C.SHOAL_STATUS_INVALID_ARGUMENT, errors.New("shoal: at least one RFile path is required"))
	}
	if uint64(count) > uint64(^uint32(0)>>1) {
		return fail(outError, C.SHOAL_STATUS_INVALID_ARGUMENT, errors.New("shoal: RFile path count is too large"))
	}
	inputs := unsafe.Slice(paths, int(count))
	names := make([]string, len(inputs))
	for index, path := range inputs {
		name, err := requiredString(path, fmt.Sprintf("paths[%d]", index))
		if err != nil {
			return fail(outError, C.SHOAL_STATUS_INVALID_ARGUMENT, err)
		}
		names[index] = name
	}
	options, err := parseRFileMergeConfig(config)
	if err != nil {
		return fail(outError, C.SHOAL_STATUS_INVALID_ARGUMENT, err)
	}
	ctx, cancel, code, err := openContext(timeout)
	if err != nil {
		return fail(outError, code, err)
	}
	defer cancel()
	reader, err := publicrfile.OpenMany(ctx, names, options)
	if err != nil {
		return failForError(outError, err)
	}
	handle, code, err := registerRFileReader(reader)
	if err != nil {
		return fail(outError, code, err)
	}
	*out = handle
	return C.SHOAL_STATUS_OK
}

//export shoal_rfile_seekable_create
func shoal_rfile_seekable_create(input *C.shoal_range, families *C.shoal_bytes, count C.size_t, inclusive C.uint8_t, out **C.shoal_rfile_seekable, outError **C.shoal_error) (status C.shoal_status) {
	clearError(outError)
	if out != nil {
		*out = nil
	}
	defer recoverStatus(&status, outError)
	if out == nil {
		return fail(outError, C.SHOAL_STATUS_INVALID_ARGUMENT, errors.New("shoal: out_seekable is required"))
	}
	keyRange, err := parseRange(input)
	if err != nil {
		return fail(outError, C.SHOAL_STATUS_INVALID_ARGUMENT, err)
	}
	columns, err := parseRFileFamilies(families, count)
	if err != nil {
		return fail(outError, C.SHOAL_STATUS_INVALID_ARGUMENT, err)
	}

	include, err := boolFlag(inclusive, "inclusive")
	if err != nil {
		return fail(outError, C.SHOAL_STATUS_INVALID_ARGUMENT, err)
	}
	seekable, err := publicrfile.NewSeekableColumns(keyRange, columns, include)
	if err != nil {
		return failForError(outError, err)
	}
	owned := &ownedRFileSeekable{
		seekable:  seekable,
		startKind: rangeBoundKind(input.start.kind),
		endKind:   rangeBoundKind(input.end.kind),
	}
	id, ok := rfileSeekables.add(owned)
	if !ok {
		return fail(outError, C.SHOAL_STATUS_INTERNAL, errors.New("shoal: RFile seekable handle space exhausted"))
	}
	handle := C.shoal_bridge_rfile_seekable_alloc(C.uint64_t(id))
	if handle == nil {
		rfileSeekables.remove(id)
		return fail(outError, C.SHOAL_STATUS_OUT_OF_MEMORY, errors.New("shoal: allocate RFile seekable handle"))
	}
	*out = handle
	return C.SHOAL_STATUS_OK
}

func parseRFileFamilies(values *C.shoal_bytes, count C.size_t) ([][]byte, error) {
	if count == 0 {
		return [][]byte{}, nil
	}
	if values == nil {
		return nil, errors.New("shoal: column_families is NULL with non-zero count")
	}
	if uint64(count) > uint64(^uint32(0)>>1) {
		return nil, errors.New("shoal: column_families count is too large")
	}
	input := unsafe.Slice(values, int(count))
	result := make([][]byte, len(input))
	for index, value := range input {
		copied, err := copyBytes(value.data, value.length, fmt.Sprintf("column_families[%d]", index))
		if err != nil {
			return nil, err
		}
		if copied == nil {
			copied = []byte{}
		}
		result[index] = copied
	}
	return result, nil
}

//export shoal_rfile_reader_seek
func shoal_rfile_reader_seek(readerHandle *C.shoal_rfile_reader, seekableHandle *C.shoal_rfile_seekable, timeout C.int64_t, outError **C.shoal_error) (status C.shoal_status) {
	clearError(outError)
	defer recoverStatus(&status, outError)
	reader, err := lookupRFileReader(readerHandle)
	if err != nil {
		return fail(outError, C.SHOAL_STATUS_INVALID_HANDLE, err)
	}
	seekable, err := lookupRFileSeekable(seekableHandle)
	if err != nil {
		return fail(outError, C.SHOAL_STATUS_INVALID_HANDLE, err)
	}
	ctx, done, code, err := reader.lifecycle.begin(timeout)
	if err != nil {
		return fail(outError, code, err)
	}
	defer done()
	if err := reader.reader.Seek(ctx, seekable.seekable); err != nil {
		return failForError(outError, err)
	}
	return C.SHOAL_STATUS_OK
}

//export shoal_rfile_reader_has_top
func shoal_rfile_reader_has_top(handle *C.shoal_rfile_reader) C.uint8_t {
	reader, err := lookupRFileReader(handle)
	if err != nil {
		return 0
	}
	done, err := reader.lifecycle.retain()
	if err != nil {
		return 0
	}
	defer done()
	return boolToCUint8(reader.reader.HasTop())
}

func allocRFileEntry(entry publicrfile.Entry) (*C.shoal_rfile_entry_result, error) {
	key := entry.Key
	result := C.shoal_bridge_rfile_entry_alloc(
		bytesPointer(key.Row), C.size_t(len(key.Row)),
		bytesPointer(key.ColumnFamily), C.size_t(len(key.ColumnFamily)),
		bytesPointer(key.ColumnQualifier), C.size_t(len(key.ColumnQualifier)),
		bytesPointer(key.ColumnVisibility), C.size_t(len(key.ColumnVisibility)),
		C.int64_t(key.Timestamp),
		bytesPointer(entry.Value), C.size_t(len(entry.Value)),
		boolToCUint8(entry.Deleted),
	)
	if result == nil {
		return nil, errors.New("shoal: allocate RFile entry result")
	}
	return result, nil
}

func bytesPointer(value []byte) *C.uint8_t {
	if len(value) == 0 {
		return nil
	}
	return (*C.uint8_t)(unsafe.Pointer(&value[0]))
}

func readerTop(handle *C.shoal_rfile_reader, keyOnly bool, out **C.shoal_rfile_entry_result, outError **C.shoal_error) C.shoal_status {
	if out != nil {
		*out = nil
	}
	if out == nil {
		return fail(outError, C.SHOAL_STATUS_INVALID_ARGUMENT, errors.New("shoal: out_result is required"))
	}
	reader, err := lookupRFileReader(handle)
	if err != nil {
		return fail(outError, C.SHOAL_STATUS_INVALID_HANDLE, err)
	}
	done, err := reader.lifecycle.retain()
	if err != nil {
		return failForError(outError, err)
	}
	defer done()
	var entry publicrfile.Entry
	if keyOnly {
		key, topErr := reader.reader.TopKey()
		if topErr != nil {
			return failForError(outError, topErr)
		}
		entry.Key = key
	} else {
		entry, err = reader.reader.Top()
		if err != nil {
			return failForError(outError, err)
		}
	}
	result, err := allocRFileEntry(entry)
	if err != nil {
		return fail(outError, C.SHOAL_STATUS_OUT_OF_MEMORY, err)
	}
	*out = result
	return C.SHOAL_STATUS_OK
}

//export shoal_rfile_reader_top
func shoal_rfile_reader_top(handle *C.shoal_rfile_reader, out **C.shoal_rfile_entry_result, outError **C.shoal_error) (status C.shoal_status) {
	clearError(outError)
	defer recoverStatus(&status, outError)
	return readerTop(handle, false, out, outError)
}

//export shoal_rfile_reader_top_key
func shoal_rfile_reader_top_key(handle *C.shoal_rfile_reader, out **C.shoal_rfile_entry_result, outError **C.shoal_error) (status C.shoal_status) {
	clearError(outError)
	defer recoverStatus(&status, outError)
	return readerTop(handle, true, out, outError)
}

//export shoal_rfile_reader_top_value
func shoal_rfile_reader_top_value(handle *C.shoal_rfile_reader, out **C.shoal_bytes_result, outError **C.shoal_error) (status C.shoal_status) {
	clearError(outError)
	if out != nil {
		*out = nil
	}
	defer recoverStatus(&status, outError)
	if out == nil {
		return fail(outError, C.SHOAL_STATUS_INVALID_ARGUMENT, errors.New("shoal: out_result is required"))
	}
	reader, err := lookupRFileReader(handle)
	if err != nil {
		return fail(outError, C.SHOAL_STATUS_INVALID_HANDLE, err)
	}
	done, err := reader.lifecycle.retain()
	if err != nil {
		return failForError(outError, err)
	}
	defer done()
	value, err := reader.reader.TopValue()
	if err != nil {
		return failForError(outError, err)
	}
	result := C.shoal_bridge_bytes_result_alloc(bytesPointer(value), C.size_t(len(value)))
	if result == nil {
		return fail(outError, C.SHOAL_STATUS_OUT_OF_MEMORY, errors.New("shoal: allocate RFile value result"))
	}
	*out = result
	return C.SHOAL_STATUS_OK
}

//export shoal_rfile_reader_next
func shoal_rfile_reader_next(handle *C.shoal_rfile_reader, timeout C.int64_t, outError **C.shoal_error) (status C.shoal_status) {
	clearError(outError)
	defer recoverStatus(&status, outError)
	reader, err := lookupRFileReader(handle)
	if err != nil {
		return fail(outError, C.SHOAL_STATUS_INVALID_HANDLE, err)
	}
	ctx, done, code, err := reader.lifecycle.begin(timeout)
	if err != nil {
		return fail(outError, code, err)
	}
	defer done()
	if err := reader.reader.Next(ctx); err != nil {
		return failForError(outError, err)
	}
	return C.SHOAL_STATUS_OK
}

//export shoal_rfile_reader_close
func shoal_rfile_reader_close(handle *C.shoal_rfile_reader, outError **C.shoal_error) (status C.shoal_status) {
	clearError(outError)
	defer recoverStatus(&status, outError)
	reader, err := lookupRFileReader(handle)
	if err != nil {
		return fail(outError, C.SHOAL_STATUS_INVALID_HANDLE, err)
	}
	if err := reader.lifecycle.close(reader.reader.Close); err != nil {
		return failForError(outError, err)
	}
	return C.SHOAL_STATUS_OK
}

//export shoal_rfile_reader_free
func shoal_rfile_reader_free(handle **C.shoal_rfile_reader) {
	if handle == nil || *handle == nil {
		return
	}
	value := *handle
	*handle = nil
	id := uint64(C.shoal_bridge_rfile_reader_id(value))
	if reader, ok := rfileReaders.remove(id); ok {
		reader.lifecycle.closeBounded(reader.reader.Close)
	}
	C.shoal_bridge_rfile_reader_free(value)
}

//export shoal_rfile_seekable_get_range
func shoal_rfile_seekable_get_range(handle *C.shoal_rfile_seekable, out **C.shoal_range_result, outError **C.shoal_error) (status C.shoal_status) {
	clearError(outError)
	if out != nil {
		*out = nil
	}
	defer recoverStatus(&status, outError)
	if out == nil {
		return fail(outError, C.SHOAL_STATUS_INVALID_ARGUMENT, errors.New("shoal: out_result is required"))
	}
	seekable, err := lookupRFileSeekable(handle)
	if err != nil {
		return fail(outError, C.SHOAL_STATUS_INVALID_HANDLE, err)
	}
	result, err := buildRangeResult(seekable.seekable.Range(), seekable.startKind, seekable.endKind)
	if err != nil {
		return fail(outError, C.SHOAL_STATUS_OUT_OF_MEMORY, err)
	}
	*out = result
	return C.SHOAL_STATUS_OK
}

//export shoal_rfile_seekable_column_family_count
func shoal_rfile_seekable_column_family_count(handle *C.shoal_rfile_seekable) C.size_t {
	seekable, err := lookupRFileSeekable(handle)
	if err != nil {
		return 0
	}
	return C.size_t(len(seekable.seekable.ColumnFamilies()))
}

//export shoal_rfile_seekable_get_column_family
func shoal_rfile_seekable_get_column_family(handle *C.shoal_rfile_seekable, index C.size_t, out **C.shoal_bytes_result, outError **C.shoal_error) (status C.shoal_status) {
	clearError(outError)
	defer recoverStatus(&status, outError)
	if out != nil {
		*out = nil
	}
	if out == nil {
		return fail(outError, C.SHOAL_STATUS_INVALID_ARGUMENT, errors.New("shoal: out_result is required"))
	}
	seekable, err := lookupRFileSeekable(handle)
	if err != nil {
		return fail(outError, C.SHOAL_STATUS_INVALID_HANDLE, err)
	}
	families := seekable.seekable.ColumnFamilies()
	if uint64(index) >= uint64(len(families)) {
		return fail(outError, C.SHOAL_STATUS_INVALID_ARGUMENT, errors.New("shoal: column family index is out of range"))
	}
	family := families[int(index)]
	result := C.shoal_bridge_bytes_result_alloc(bytesPointer(family), C.size_t(len(family)))
	if result == nil {
		return fail(outError, C.SHOAL_STATUS_OUT_OF_MEMORY, errors.New("shoal: allocate RFile column family result"))
	}
	*out = result
	return C.SHOAL_STATUS_OK
}

//export shoal_rfile_seekable_is_inclusive
func shoal_rfile_seekable_is_inclusive(handle *C.shoal_rfile_seekable) C.uint8_t {
	seekable, err := lookupRFileSeekable(handle)
	if err != nil {
		return 0
	}
	return boolToCUint8(seekable.seekable.Inclusive())
}

//export shoal_rfile_seekable_free
func shoal_rfile_seekable_free(handle **C.shoal_rfile_seekable) {
	if handle == nil || *handle == nil {
		return
	}
	value := *handle
	*handle = nil
	rfileSeekables.remove(uint64(C.shoal_bridge_rfile_seekable_id(value)))
	C.shoal_bridge_rfile_seekable_free(value)
}

//export shoal_rfile_entry_result_get
func shoal_rfile_entry_result_get(result *C.shoal_rfile_entry_result, out *C.shoal_rfile_entry_view, outError **C.shoal_error) (status C.shoal_status) {
	clearError(outError)
	defer recoverStatus(&status, outError)
	if out == nil || out.struct_size < C.SHOAL_RFILE_ENTRY_VIEW_V1_SIZE {
		return fail(outError, C.SHOAL_STATUS_INVALID_ARGUMENT, errors.New("shoal: RFile entry view is missing or too small"))
	}
	if C.shoal_bridge_rfile_entry_get(result, out) == 0 {
		return fail(outError, C.SHOAL_STATUS_INVALID_ARGUMENT, errors.New("shoal: invalid RFile entry result access"))
	}
	return C.SHOAL_STATUS_OK
}

//export shoal_rfile_entry_result_free
func shoal_rfile_entry_result_free(result **C.shoal_rfile_entry_result) {
	if result != nil && *result != nil {
		C.shoal_bridge_rfile_entry_free(*result)
		*result = nil
	}
}
