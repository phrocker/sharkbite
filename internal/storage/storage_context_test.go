package storage_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"sync"
	"testing"

	"github.com/phrocker/shoal-oss/internal/storage"
	"github.com/phrocker/shoal-oss/internal/storage/memory"
)

const testTransferChunkSize = 64 * 1024

func TestCopy_CanceledAbortableWriterAbortsWithoutClose(t *testing.T) {
	body := bytes.Repeat([]byte("x"), 2*testTransferChunkSize)
	src := memory.New()
	src.Put("/src", body)

	ctx, cancel := context.WithCancel(context.Background())
	writer := &recordingAbortWriter{cancel: cancel, cancelAfterWrites: 1}
	dst := writerBackend{writer: writer}

	n, err := storage.Copy(ctx, src, "/src", dst, "/dst")
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Copy error = %v, want context.Canceled", err)
	}
	if n != testTransferChunkSize {
		t.Fatalf("Copy wrote %d bytes, want %d", n, testTransferChunkSize)
	}
	if got := writer.buf.Len(); got != testTransferChunkSize {
		t.Fatalf("dst length = %d, want %d", got, testTransferChunkSize)
	}
	if writer.abortCalls != 1 {
		t.Fatalf("Abort calls = %d, want 1", writer.abortCalls)
	}
	if writer.closeCalls != 0 {
		t.Fatalf("Close calls = %d, want 0", writer.closeCalls)
	}
}

func TestCopy_PropagatesCloseError(t *testing.T) {
	src := memory.New()
	src.Put("/src", []byte("hello"))

	want := errors.New("close failed")
	dst := writerBackend{writer: &closeErrWriter{closeErr: want}}

	n, err := storage.Copy(context.Background(), src, "/src", dst, "/dst")
	if !errors.Is(err, want) {
		t.Fatalf("Copy error = %v, want %v", err, want)
	}
	if n != int64(len("hello")) {
		t.Fatalf("Copy wrote %d bytes, want %d", n, len("hello"))
	}
}

func TestCopy_CloseErrorDoesNotRetryCloseForLegacyWriter(t *testing.T) {
	src := memory.New()
	src.Put("/src", []byte("hello"))

	closeErr := errors.New("close failed")
	writer := &closeErrWriter{closeErr: closeErr, commitOnSecondClose: true}

	_, err := storage.Copy(context.Background(), src, "/src", writerBackend{writer: writer}, "/dst")
	if !errors.Is(err, closeErr) {
		t.Fatalf("Copy error = %v, want %v", err, closeErr)
	}
	if writer.closeCalls != 1 {
		t.Fatalf("Close calls = %d, want 1", writer.closeCalls)
	}
	if writer.committed {
		t.Fatal("Copy retried Close and committed a legacy writer after the first close failure")
	}
}

func TestCopy_CloseErrorAbortsAndJoinsCleanupError(t *testing.T) {
	src := memory.New()
	src.Put("/src", []byte("hello"))

	closeErr := errors.New("close failed")
	abortErr := errors.New("abort failed")
	writer := &recordingAbortWriter{closeErr: closeErr, abortErr: abortErr, commitOnSecondClose: true}

	_, err := storage.Copy(context.Background(), src, "/src", writerBackend{writer: writer}, "/dst")
	if !errors.Is(err, closeErr) || !errors.Is(err, abortErr) {
		t.Fatalf("Copy error = %v, want joined close and abort errors", err)
	}
	if writer.closeCalls != 1 || writer.abortCalls != 1 {
		t.Fatalf("Close calls = %d, Abort calls = %d; want 1 each", writer.closeCalls, writer.abortCalls)
	}
	if writer.committed {
		t.Fatal("Copy retried Close and committed after the first close failure")
	}
}

func TestCopy_SuccessfulAbortableWriterClosesWithoutAbort(t *testing.T) {
	src := memory.New()
	src.Put("/src", []byte("hello"))

	writer := &recordingAbortWriter{}
	n, err := storage.Copy(context.Background(), src, "/src", writerBackend{writer: writer}, "/dst")
	if err != nil {
		t.Fatal(err)
	}
	if n != int64(len("hello")) {
		t.Fatalf("Copy wrote %d bytes, want %d", n, len("hello"))
	}
	if writer.closeCalls != 1 {
		t.Fatalf("Close calls = %d, want 1", writer.closeCalls)
	}
	if writer.abortCalls != 0 {
		t.Fatalf("Abort calls = %d, want 0", writer.abortCalls)
	}
}

func TestReadAll_CanceledReadWaitsForBlockedCall(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	file := newBlockingFile()
	readErr := errors.New("backend read failed")
	file.readErr = readErr
	done := make(chan error, 1)

	go func() {
		_, err := storage.ReadAll(ctx, fileBackend{file: file}, "/src")
		done <- err
	}()

	<-file.entered
	cancel()

	select {
	case err := <-done:
		t.Fatalf("ReadAll returned before the blocked read finished: %v", err)
	default:
	}

	close(file.release)
	if err := <-done; !errors.Is(err, context.Canceled) || !errors.Is(err, readErr) {
		t.Fatalf("ReadAll error = %v, want joined cancellation and backend error", err)
	}
}

func TestReadAll_BoundsReadRequestsAndChecksFinalCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	file := &recordingReadFile{
		data:   bytes.Repeat([]byte("x"), testTransferChunkSize+1),
		cancel: cancel,
	}

	_, err := storage.ReadAll(ctx, fileBackend{file: file}, "/src")
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("ReadAll error = %v, want context.Canceled", err)
	}
	if file.maxRequest != testTransferChunkSize {
		t.Fatalf("largest ReadAt request = %d, want %d", file.maxRequest, testTransferChunkSize)
	}
	if file.readCalls != 2 {
		t.Fatalf("ReadAt calls = %d, want 2", file.readCalls)
	}
}

func TestReadAll_CanceledDuringOpenDoesNotReturnEmptySuccess(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	backend := openFuncBackend(func(context.Context, string) (storage.File, error) {
		cancel()
		return &recordingReadFile{}, nil
	})

	got, err := storage.ReadAll(ctx, backend, "/empty")
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("ReadAll = %v, %v; want context.Canceled", got, err)
	}
}

func TestReadAll_CanceledOpenJoinsBackendError(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	openErr := errors.New("open failed")
	entered := make(chan struct{})
	backend := openFuncBackend(func(ctx context.Context, _ string) (storage.File, error) {
		close(entered)
		<-ctx.Done()
		return nil, openErr
	})
	done := make(chan error, 1)

	go func() {
		_, err := storage.ReadAll(ctx, backend, "/src")
		done <- err
	}()

	<-entered
	cancel()

	if err := <-done; !errors.Is(err, openErr) || !errors.Is(err, context.Canceled) {
		t.Fatalf("ReadAll error = %v, want joined open error and context.Canceled", err)
	}
}

func TestReadAll_CanceledReadJoinsBackendError(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	readErr := errors.New("read failed")

	_, err := storage.ReadAll(ctx, fileBackend{file: &readFuncFile{
		size: 1,
		read: func(p []byte, _ int64) (int, error) {
			p[0] = 'x'
			cancel()
			return 1, readErr
		},
	}}, "/src")
	if !errors.Is(err, readErr) || !errors.Is(err, context.Canceled) {
		t.Fatalf("ReadAll error = %v, want joined read error and context.Canceled", err)
	}
}

func TestWriteAll_CanceledAbortableWriterAbortsWithoutClose(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	data := bytes.Repeat([]byte("x"), 2*testTransferChunkSize)
	writer := &recordingAbortWriter{cancel: cancel, cancelAfterWrites: 1}

	err := storage.WriteAll(ctx, writerBackend{writer: writer}, "/dst", data)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("WriteAll error = %v, want context.Canceled", err)
	}
	if got := writer.buf.Len(); got != testTransferChunkSize {
		t.Fatalf("WriteAll wrote %d bytes, want %d", got, testTransferChunkSize)
	}
	if writer.abortCalls != 1 {
		t.Fatalf("Abort calls = %d, want 1", writer.abortCalls)
	}
	if writer.closeCalls != 0 {
		t.Fatalf("Close calls = %d, want 0", writer.closeCalls)
	}
}

func TestWriteAll_AbortErrorJoinsPrimaryError(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	data := bytes.Repeat([]byte("x"), 2*testTransferChunkSize)
	abortErr := errors.New("abort failed")
	writer := &recordingAbortWriter{cancel: cancel, cancelAfterWrites: 1, abortErr: abortErr}

	err := storage.WriteAll(ctx, writerBackend{writer: writer}, "/dst", data)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("WriteAll error = %v, want context.Canceled", err)
	}
	if !errors.Is(err, abortErr) {
		t.Fatalf("WriteAll error = %v, want joined abort error", err)
	}
	if writer.abortCalls != 1 {
		t.Fatalf("Abort calls = %d, want 1", writer.abortCalls)
	}
	if writer.closeCalls != 0 {
		t.Fatalf("Close calls = %d, want 0", writer.closeCalls)
	}
}

func TestWriteAll_CloseErrorAbortsAndJoinsCleanupError(t *testing.T) {
	closeErr := errors.New("close failed")
	abortErr := errors.New("abort failed")
	writer := &recordingAbortWriter{closeErr: closeErr, abortErr: abortErr, commitOnSecondClose: true}

	err := storage.WriteAll(context.Background(), writerBackend{writer: writer}, "/dst", []byte("hello"))
	if !errors.Is(err, closeErr) || !errors.Is(err, abortErr) {
		t.Fatalf("WriteAll error = %v, want joined close and abort errors", err)
	}
	if writer.closeCalls != 1 || writer.abortCalls != 1 {
		t.Fatalf("Close calls = %d, Abort calls = %d; want 1 each", writer.closeCalls, writer.abortCalls)
	}
	if writer.committed {
		t.Fatal("WriteAll retried Close and committed after the first close failure")
	}
}

func TestWriteAll_CloseErrorDoesNotRetryCloseForLegacyWriter(t *testing.T) {
	closeErr := errors.New("close failed")
	writer := &closeErrWriter{closeErr: closeErr, commitOnSecondClose: true}

	err := storage.WriteAll(context.Background(), writerBackend{writer: writer}, "/dst", []byte("hello"))
	if !errors.Is(err, closeErr) {
		t.Fatalf("WriteAll error = %v, want %v", err, closeErr)
	}
	if writer.closeCalls != 1 {
		t.Fatalf("Close calls = %d, want 1", writer.closeCalls)
	}
	if writer.committed {
		t.Fatal("WriteAll retried Close and committed a legacy writer after the first close failure")
	}
}

func TestWriteAll_SuccessfulAbortableWriterClosesWithoutAbort(t *testing.T) {
	writer := &recordingAbortWriter{}
	err := storage.WriteAll(context.Background(), writerBackend{writer: writer}, "/dst", []byte("hello"))
	if err != nil {
		t.Fatal(err)
	}
	if writer.closeCalls != 1 {
		t.Fatalf("Close calls = %d, want 1", writer.closeCalls)
	}
	if writer.abortCalls != 0 {
		t.Fatalf("Abort calls = %d, want 0", writer.abortCalls)
	}
}

func TestWriteAll_CanceledWriteWaitsForBlockedCall(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	data := bytes.Repeat([]byte("x"), 2*testTransferChunkSize)
	writer := newBlockingWriter()
	done := make(chan error, 1)

	go func() {
		done <- storage.WriteAll(ctx, writerBackend{writer: writer}, "/dst", data)
	}()

	<-writer.entered
	cancel()

	select {
	case err := <-done:
		t.Fatalf("WriteAll returned before the blocked write finished: %v", err)
	default:
	}

	close(writer.release)
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("WriteAll error = %v, want context.Canceled", err)
	}
	if !writer.closed {
		t.Fatal("WriteAll did not close the writer")
	}
}

func TestCopy_CanceledReadJoinsBackendError(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	readErr := errors.New("read failed")
	writer := &writeFuncWriter{
		write: func([]byte) (int, error) {
			t.Fatal("Copy wrote data after a canceled read")
			return 0, nil
		},
	}

	_, err := storage.Copy(ctx, fileBackend{file: &readFuncFile{
		size: 1,
		read: func([]byte, int64) (int, error) {
			cancel()
			return 0, readErr
		},
	}}, "/src", writerBackend{writer: writer}, "/dst")
	if !errors.Is(err, readErr) || !errors.Is(err, context.Canceled) {
		t.Fatalf("Copy error = %v, want joined read error and context.Canceled", err)
	}
	if writer.abortCalls != 1 {
		t.Fatalf("Abort calls = %d, want 1", writer.abortCalls)
	}
}

func TestWriteAll_CanceledCreateJoinsBackendError(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	createErr := errors.New("create failed")
	entered := make(chan struct{})
	backend := createFuncBackend(func(ctx context.Context, _ string) (storage.Writer, error) {
		close(entered)
		<-ctx.Done()
		return nil, createErr
	})
	done := make(chan error, 1)

	go func() {
		done <- storage.WriteAll(ctx, backend, "/dst", []byte("hello"))
	}()

	<-entered
	cancel()

	if err := <-done; !errors.Is(err, createErr) || !errors.Is(err, context.Canceled) {
		t.Fatalf("WriteAll error = %v, want joined create error and context.Canceled", err)
	}
}

func TestCopy_ReadErrorClosesLegacyWriterOnce(t *testing.T) {
	readErr := errors.New("read failed")
	writer := &legacyWriteFuncWriter{
		write: func([]byte) (int, error) {
			t.Fatal("Copy wrote data after a read failure")
			return 0, nil
		},
	}

	_, err := storage.Copy(context.Background(), fileBackend{file: &readFuncFile{
		size: 1,
		read: func([]byte, int64) (int, error) {
			return 0, readErr
		},
	}}, "/src", writerBackend{writer: writer}, "/dst")
	if !errors.Is(err, readErr) {
		t.Fatalf("Copy error = %v, want %v", err, readErr)
	}
	if writer.closeCalls != 1 {
		t.Fatalf("Close calls = %d, want 1", writer.closeCalls)
	}
}

func TestCopy_CanceledEOFReadDoesNotWriteAndJoinsEOF(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	writer := &writeFuncWriter{}

	_, err := storage.Copy(ctx, fileBackend{file: &readFuncFile{
		size: 1,
		read: func(p []byte, _ int64) (int, error) {
			p[0] = 'x'
			cancel()
			return 1, io.EOF
		},
	}}, "/src", writerBackend{writer: writer}, "/dst")
	if !errors.Is(err, io.EOF) || !errors.Is(err, context.Canceled) {
		t.Fatalf("Copy error = %v, want joined EOF and context.Canceled", err)
	}
	if writer.writeCalls != 0 {
		t.Fatalf("Write calls = %d, want 0", writer.writeCalls)
	}
	if writer.abortCalls != 1 {
		t.Fatalf("Abort calls = %d, want 1", writer.abortCalls)
	}
}

func TestCopy_PrematureEOFBeforeAdvertisedSizeAbortsDestination(t *testing.T) {
	dst := memory.New()

	n, err := storage.Copy(context.Background(), fileBackend{file: &readFuncFile{
		size: 2,
		read: func(p []byte, _ int64) (int, error) {
			p[0] = 'x'
			return 1, io.EOF
		},
	}}, "/src", dst, "/dst")
	if !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("Copy error = %v, want %v", err, io.ErrUnexpectedEOF)
	}
	if n != 1 {
		t.Fatalf("Copy wrote %d bytes, want 1", n)
	}
	if _, openErr := dst.Open(context.Background(), "/dst"); !errors.Is(openErr, storage.ErrNotFound) {
		t.Fatalf("destination open error = %v, want storage.ErrNotFound after abort", openErr)
	}
}

func TestCopy_CanceledShortWriteJoinsContextError(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	src := memory.New()
	src.Put("/src", []byte("hello"))
	writer := &writeFuncWriter{
		write: func(p []byte) (int, error) {
			cancel()
			return len(p) - 1, nil
		},
	}

	n, err := storage.Copy(ctx, src, "/src", writerBackend{writer: writer}, "/dst")
	if !errors.Is(err, io.ErrShortWrite) || !errors.Is(err, context.Canceled) {
		t.Fatalf("Copy error = %v, want joined short write and context.Canceled", err)
	}
	if n != int64(len("hell")) {
		t.Fatalf("Copy wrote %d bytes, want %d", n, len("hell"))
	}
	if writer.abortCalls != 1 {
		t.Fatalf("Abort calls = %d, want 1", writer.abortCalls)
	}
}

func TestCopy_CanceledOpenJoinsBackendError(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	openErr := errors.New("open failed")
	entered := make(chan struct{})
	src := openFuncBackend(func(ctx context.Context, _ string) (storage.File, error) {
		close(entered)
		<-ctx.Done()
		return nil, openErr
	})
	done := make(chan struct {
		n   int64
		err error
	}, 1)

	go func() {
		n, err := storage.Copy(ctx, src, "/src", writerBackend{writer: &writeFuncWriter{}}, "/dst")
		done <- struct {
			n   int64
			err error
		}{n: n, err: err}
	}()

	<-entered
	cancel()

	result := <-done
	if result.n != 0 {
		t.Fatalf("Copy wrote %d bytes, want 0", result.n)
	}
	if !errors.Is(result.err, openErr) || !errors.Is(result.err, context.Canceled) {
		t.Fatalf("Copy error = %v, want joined open error and context.Canceled", result.err)
	}
}

func TestCopy_CanceledCreateJoinsBackendError(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	createErr := errors.New("create failed")
	entered := make(chan struct{})
	dst := createFuncBackend(func(ctx context.Context, _ string) (storage.Writer, error) {
		close(entered)
		<-ctx.Done()
		return nil, createErr
	})
	done := make(chan struct {
		n   int64
		err error
	}, 1)

	go func() {
		n, err := storage.Copy(ctx, fileBackend{file: &recordingReadFile{}}, "/src", dst, "/dst")
		done <- struct {
			n   int64
			err error
		}{n: n, err: err}
	}()

	<-entered
	cancel()

	result := <-done
	if result.n != 0 {
		t.Fatalf("Copy wrote %d bytes, want 0", result.n)
	}
	if !errors.Is(result.err, createErr) || !errors.Is(result.err, context.Canceled) {
		t.Fatalf("Copy error = %v, want joined create error and context.Canceled", result.err)
	}
}

func TestWriteAll_CanceledWriteJoinsBackendError(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	writeErr := errors.New("write failed")
	writer := &writeFuncWriter{
		write: func([]byte) (int, error) {
			cancel()
			return 0, writeErr
		},
	}

	err := storage.WriteAll(ctx, writerBackend{writer: writer}, "/dst", []byte("hello"))
	if !errors.Is(err, writeErr) || !errors.Is(err, context.Canceled) {
		t.Fatalf("WriteAll error = %v, want joined write error and context.Canceled", err)
	}
	if writer.abortCalls != 1 {
		t.Fatalf("Abort calls = %d, want 1", writer.abortCalls)
	}
}

func TestWriteAll_WriteErrorClosesLegacyWriterOnce(t *testing.T) {
	writeErr := errors.New("write failed")
	writer := &legacyWriteFuncWriter{
		write: func([]byte) (int, error) {
			return 0, writeErr
		},
	}

	err := storage.WriteAll(context.Background(), writerBackend{writer: writer}, "/dst", []byte("hello"))
	if !errors.Is(err, writeErr) {
		t.Fatalf("WriteAll error = %v, want %v", err, writeErr)
	}
	if writer.closeCalls != 1 {
		t.Fatalf("Close calls = %d, want 1", writer.closeCalls)
	}
}

type writerBackend struct {
	writer storage.Writer
}

func (b writerBackend) Open(context.Context, string) (storage.File, error) {
	return nil, errors.New("not implemented")
}

func (b writerBackend) Create(context.Context, string) (storage.Writer, error) {
	return b.writer, nil
}

type fileBackend struct {
	file storage.File
}

func (b fileBackend) Open(context.Context, string) (storage.File, error) {
	return b.file, nil
}

type openFuncBackend func(context.Context, string) (storage.File, error)

func (f openFuncBackend) Open(ctx context.Context, path string) (storage.File, error) {
	return f(ctx, path)
}

type createFuncBackend func(context.Context, string) (storage.Writer, error)

func (createFuncBackend) Open(context.Context, string) (storage.File, error) {
	return nil, errors.New("not implemented")
}

func (f createFuncBackend) Create(ctx context.Context, path string) (storage.Writer, error) {
	return f(ctx, path)
}

type recordingAbortWriter struct {
	buf                 bytes.Buffer
	cancel              context.CancelFunc
	writes              int
	cancelAfterWrites   int
	closeErr            error
	abortErr            error
	commitOnSecondClose bool
	committed           bool
	closeCalls          int
	abortCalls          int
}

func (w *recordingAbortWriter) Write(p []byte) (int, error) {
	w.writes++
	if w.cancelAfterWrites > 0 && w.writes == w.cancelAfterWrites && w.cancel != nil {
		w.cancel()
	}
	return w.buf.Write(p)
}

func (w *recordingAbortWriter) Close() error {
	w.closeCalls++
	if w.commitOnSecondClose && w.closeCalls > 1 {
		w.committed = true
		return nil
	}
	return w.closeErr
}

func (w *recordingAbortWriter) Abort() error {
	w.abortCalls++
	return w.abortErr
}

type closeErrWriter struct {
	buf                 bytes.Buffer
	closeErr            error
	commitOnSecondClose bool
	committed           bool
	closeCalls          int
}

func (w *closeErrWriter) Write(p []byte) (int, error) {
	return w.buf.Write(p)
}

func (w *closeErrWriter) Close() error {
	w.closeCalls++
	if w.commitOnSecondClose && w.closeCalls > 1 {
		w.committed = true
		return nil
	}
	return w.closeErr
}

type legacyWriteFuncWriter struct {
	write      func([]byte) (int, error)
	closeErr   error
	writeCalls int
	closeCalls int
}

func (w *legacyWriteFuncWriter) Write(p []byte) (int, error) {
	w.writeCalls++
	if w.write != nil {
		return w.write(p)
	}
	return len(p), nil
}

func (w *legacyWriteFuncWriter) Close() error {
	w.closeCalls++
	return w.closeErr
}

type blockingFile struct {
	entered chan struct{}
	release chan struct{}
	once    sync.Once
	readErr error
}

func newBlockingFile() *blockingFile {
	return &blockingFile{
		entered: make(chan struct{}),
		release: make(chan struct{}),
	}
}

func (f *blockingFile) ReadAt(p []byte, off int64) (int, error) {
	if off > 0 {
		p[0] = 'y'
		return 1, io.EOF
	}
	f.once.Do(func() { close(f.entered) })
	<-f.release
	p[0] = 'x'
	return 1, f.readErr
}

func (f *blockingFile) Close() error { return nil }
func (f *blockingFile) Size() int64  { return 2 }

type recordingReadFile struct {
	data       []byte
	cancel     context.CancelFunc
	readCalls  int
	maxRequest int
}

func (f *recordingReadFile) ReadAt(p []byte, off int64) (int, error) {
	f.readCalls++
	f.maxRequest = max(f.maxRequest, len(p))
	n := copy(p, f.data[off:])
	if off+int64(n) == int64(len(f.data)) && f.cancel != nil {
		f.cancel()
	}
	if n < len(p) {
		return n, io.EOF
	}
	return n, nil
}

func (f *recordingReadFile) Close() error { return nil }
func (f *recordingReadFile) Size() int64  { return int64(len(f.data)) }

type readFuncFile struct {
	size int64
	read func([]byte, int64) (int, error)
}

func (f *readFuncFile) ReadAt(p []byte, off int64) (int, error) {
	return f.read(p, off)
}

func (*readFuncFile) Close() error { return nil }
func (f *readFuncFile) Size() int64 {
	return f.size
}

type blockingWriter struct {
	buf     bytes.Buffer
	entered chan struct{}
	release chan struct{}
	once    sync.Once
	closed  bool
}

func newBlockingWriter() *blockingWriter {
	return &blockingWriter{
		entered: make(chan struct{}),
		release: make(chan struct{}),
	}
}

func (w *blockingWriter) Write(p []byte) (int, error) {
	w.once.Do(func() { close(w.entered) })
	<-w.release
	return w.buf.Write(p)
}

func (w *blockingWriter) Close() error {
	w.closed = true
	return nil
}

type writeFuncWriter struct {
	write      func([]byte) (int, error)
	closeErr   error
	abortErr   error
	writeCalls int
	closeCalls int
	abortCalls int
}

func (w *writeFuncWriter) Write(p []byte) (int, error) {
	w.writeCalls++
	if w.write != nil {
		return w.write(p)
	}
	return len(p), nil
}

func (w *writeFuncWriter) Close() error {
	w.closeCalls++
	return w.closeErr
}

func (w *writeFuncWriter) Abort() error {
	w.abortCalls++
	return w.abortErr
}
