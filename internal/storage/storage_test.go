package storage_test

import (
	"bytes"
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/phrocker/shoal-oss/internal/storage"
	"github.com/phrocker/shoal-oss/internal/storage/local"
	"github.com/phrocker/shoal-oss/internal/storage/memory"
)

// readOnlyBackend wraps a Backend without WritableBackend implementation,
// so we can verify Copy errors cleanly.
type readOnlyBackend struct{ inner storage.Backend }

func (r readOnlyBackend) Open(ctx context.Context, p string) (storage.File, error) {
	return r.inner.Open(ctx, p)
}

// faultyCloseBackend wraps a WritableBackend whose writes succeed but
// whose Close always fails, simulating a backend that only detects a
// commit/flush failure (e.g. an object-storage upload) when the writer is
// closed - the scenario storage.Copy must surface instead of discarding.
type faultyCloseBackend struct {
	inner interface {
		storage.Backend
		Create(ctx context.Context, path string) (storage.Writer, error)
	}
}

func (f faultyCloseBackend) Open(ctx context.Context, p string) (storage.File, error) {
	return f.inner.Open(ctx, p)
}

func (f faultyCloseBackend) Create(ctx context.Context, path string) (storage.Writer, error) {
	w, err := f.inner.Create(ctx, path)
	if err != nil {
		return nil, err
	}
	return faultyCloseWriter{w}, nil
}

type faultyCloseWriter struct{ storage.Writer }

var errFaultyClose = errors.New("faultyCloseWriter: simulated commit failure")

func (faultyCloseWriter) Close() error { return errFaultyClose }

type abortTrackingWriter struct {
	aborted bool
}

func (*abortTrackingWriter) Write(p []byte) (int, error) { return len(p), nil }
func (*abortTrackingWriter) Close() error                { return errors.New("unexpected commit") }
func (w *abortTrackingWriter) Abort() error {
	w.aborted = true
	return nil
}

type janitorBackend struct {
	result storage.ArtifactCleanupResult
}

func (janitorBackend) Open(context.Context, string) (storage.File, error) {
	return nil, storage.ErrNotFound
}
func (b janitorBackend) CleanupStaleArtifacts(context.Context, string, time.Time) (storage.ArtifactCleanupResult, error) {
	return b.result, nil
}

func TestCleanupUnsuccessfulWriteUsesAbort(t *testing.T) {
	primary := errors.New("write failed")
	w := &abortTrackingWriter{}
	if err := storage.CleanupUnsuccessfulWrite(primary, w); !errors.Is(err, primary) {
		t.Fatalf("CleanupUnsuccessfulWrite error = %v, want primary error", err)
	}
	if !w.aborted {
		t.Fatal("CleanupUnsuccessfulWrite did not abort transactional writer")
	}
}

func TestCleanupUnsuccessfulWriteSkipsAbortAfterCommittedWrite(t *testing.T) {
	primary := storage.MarkCommittedWrite(errors.New("write committed but cleanup failed"))
	w := &abortTrackingWriter{}
	if err := storage.CleanupUnsuccessfulWrite(primary, w); !errors.Is(err, primary) {
		t.Fatalf("CleanupUnsuccessfulWrite error = %v, want committed primary error", err)
	}
	if w.aborted {
		t.Fatal("CleanupUnsuccessfulWrite aborted a committed writer")
	}
}

func TestCopy_MemoryToMemory(t *testing.T) {
	src := memory.New()
	src.Put("/src/x", []byte("the contents of x"))
	dst := memory.New()

	n, err := storage.Copy(context.Background(), src, "/src/x", dst, "/dst/x")
	if err != nil {
		t.Fatal(err)
	}
	if n != int64(len("the contents of x")) {
		t.Errorf("Copy returned n=%d, want %d", n, len("the contents of x"))
	}
	f, err := dst.Open(context.Background(), "/dst/x")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	got := make([]byte, f.Size())
	if _, err := f.ReadAt(got, 0); err != nil {
		t.Fatal(err)
	}
	if string(got) != "the contents of x" {
		t.Errorf("got %q", got)
	}
}

func TestCopy_MemoryToLocal(t *testing.T) {
	src := memory.New()
	body := bytes.Repeat([]byte{0x55, 0xaa}, 5000) // 10KB — exercises the
	// chunk loop more than once (chunk = 64KB so this fits in 1, but we
	// still need >0 iterations to validate)
	src.Put("/big", body)

	dir := t.TempDir()
	dst := local.New()
	dstPath := filepath.Join(dir, "out", "blob.bin")

	n, err := storage.Copy(context.Background(), src, "/big", dst, dstPath)
	if err != nil {
		t.Fatal(err)
	}
	if n != int64(len(body)) {
		t.Errorf("n = %d, want %d", n, len(body))
	}
	// Read back via the local backend.
	f, err := dst.Open(context.Background(), dstPath)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	got := make([]byte, f.Size())
	if _, err := f.ReadAt(got, 0); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, body) {
		t.Errorf("local file content mismatch (size=%d)", f.Size())
	}
}

func TestCopy_LargeFile_MultipleChunks(t *testing.T) {
	// 200KB — forces 4 chunk-loop iterations (chunk = 64KB).
	body := bytes.Repeat([]byte{0x42}, 200*1024)
	src := memory.New()
	src.Put("/big", body)
	dst := memory.New()

	n, err := storage.Copy(context.Background(), src, "/big", dst, "/dst")
	if err != nil {
		t.Fatal(err)
	}
	if n != int64(len(body)) {
		t.Errorf("n = %d, want %d", n, len(body))
	}
	f, _ := dst.Open(context.Background(), "/dst")
	defer f.Close()
	if f.Size() != int64(len(body)) {
		t.Errorf("dst size = %d, want %d", f.Size(), len(body))
	}
}

func TestCopy_RejectsNonWritableDst(t *testing.T) {
	src := memory.New()
	src.Put("/x", []byte("data"))
	dst := readOnlyBackend{inner: memory.New()} // wraps memory but hides WritableBackend

	_, err := storage.Copy(context.Background(), src, "/x", dst, "/dst")
	if !errors.Is(err, storage.ErrReadOnly) {
		t.Errorf("err = %v, want ErrReadOnly", err)
	}
}

func TestCopy_SrcNotFound(t *testing.T) {
	src := memory.New()
	dst := memory.New()
	_, err := storage.Copy(context.Background(), src, "/missing", dst, "/wherever")
	if !errors.Is(err, storage.ErrNotFound) {
		t.Errorf("err = %v, want chain to storage.ErrNotFound", err)
	}
}

func TestCopy_EmptyFile(t *testing.T) {
	src := memory.New()
	src.Put("/empty", nil)
	dst := memory.New()
	n, err := storage.Copy(context.Background(), src, "/empty", dst, "/dst")
	if err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Errorf("n = %d, want 0", n)
	}
	f, err := dst.Open(context.Background(), "/dst")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if f.Size() != 0 {
		t.Errorf("dst size = %d, want 0", f.Size())
	}
}

// TestCopy_PropagatesDestinationCloseError guards against silently
// discarding a destination writer's Close error (e.g. a backend that only
// surfaces an upload/commit failure on Close): Copy must report it
// instead of returning a false success.
func TestCopy_PropagatesDestinationCloseError(t *testing.T) {
	src := memory.New()
	src.Put("/x", []byte("data"))
	dst := faultyCloseBackend{inner: memory.New()}

	_, err := storage.Copy(context.Background(), src, "/x", dst, "/dst")
	if !errors.Is(err, errFaultyClose) {
		t.Fatalf("err = %v, want chain to errFaultyClose", err)
	}
}

func TestValidateArtifactCleanupCutoffRejectsRecentCutoffs(t *testing.T) {
	err := storage.ValidateArtifactCleanupCutoff(time.Now(), time.Now().Add(-time.Second))
	if !errors.Is(err, storage.ErrArtifactCleanupCutoffTooRecent) {
		t.Fatalf("ValidateArtifactCleanupCutoff error = %v, want ErrArtifactCleanupCutoffTooRecent", err)
	}
}

func TestCleanupStaleArtifactsRejectsUnsupportedBackend(t *testing.T) {
	_, err := storage.CleanupStaleArtifacts(
		context.Background(),
		memory.New(),
		"/root",
		time.Now().Add(-storage.RecommendedArtifactCleanupAge),
	)
	if !errors.Is(err, storage.ErrArtifactCleanerUnsupported) {
		t.Fatalf("CleanupStaleArtifacts error = %v, want ErrArtifactCleanerUnsupported", err)
	}
}

func TestCleanupStaleArtifactsUsesCleanerCapability(t *testing.T) {
	want := storage.ArtifactCleanupResult{
		Examined:    2,
		Removed:     []string{"/root/.tmp"},
		Recoverable: []string{"/root/.bak"},
	}
	got, err := storage.CleanupStaleArtifacts(
		context.Background(),
		janitorBackend{result: want},
		"/root",
		time.Now().Add(-storage.RecommendedArtifactCleanupAge),
	)
	if err != nil {
		t.Fatalf("CleanupStaleArtifacts: %v", err)
	}
	if got.Examined != want.Examined ||
		len(got.Removed) != 1 || got.Removed[0] != want.Removed[0] ||
		len(got.Recoverable) != 1 || got.Recoverable[0] != want.Recoverable[0] {
		t.Fatalf("CleanupStaleArtifacts = %+v, want %+v", got, want)
	}
}
