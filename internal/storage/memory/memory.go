// Package memory is an in-memory storage.Backend for tests. Maps a string
// path to a byte slice; Open returns a File backed by bytes.Reader.
//
// Useful when integration-testing the rfile reader against synthetic
// RFile bytes without touching disk or GCS. Production code should not
// import this package.
package memory

import (
	"bytes"
	"context"
	"fmt"
	"strings"
	"sync"

	"github.com/phrocker/shoal-oss/internal/storage"
)

// Backend is an in-memory map[path]bytes. Concurrent-safe Put/Open.
type Backend struct {
	mu      sync.RWMutex
	objects map[string][]byte
}

// New returns an empty Backend ready for Put.
func New() *Backend {
	return &Backend{objects: map[string][]byte{}}
}

// Put registers data at path. Subsequent Open calls for path will return
// a File reading those bytes. Replaces any existing entry.
//
// Stores a defensive copy so callers can mutate data afterwards without
// affecting the registered fixture.
func (b *Backend) Put(path string, data []byte) {
	cp := make([]byte, len(data))
	copy(cp, data)
	b.mu.Lock()
	defer b.mu.Unlock()
	b.objects[path] = cp
}

// Delete removes path. No-op if absent.
func (b *Backend) Delete(path string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	delete(b.objects, path)
}

// List returns the keys directly under prefix. "Directly under" means the
// key starts with prefix and the remainder contains no further path
// separator (forward slash or OS separator) — so a flat tablet directory
// lists its RFiles without descending into nested keys. Ordering is
// unspecified. Satisfies storage.Lister.
func (b *Backend) List(_ context.Context, prefix string) ([]string, error) {
	b.mu.RLock()
	defer b.mu.RUnlock()
	var out []string
	for k := range b.objects {
		if !strings.HasPrefix(k, prefix) {
			continue
		}
		rest := strings.TrimPrefix(k, prefix)
		rest = strings.TrimLeft(rest, `/\`)
		if strings.ContainsAny(rest, `/\`) {
			continue // nested deeper than the immediate prefix
		}
		out = append(out, k)
	}
	return out, nil
}

// Remove deletes path. No-op if absent. Satisfies storage.Remover.
func (b *Backend) Remove(_ context.Context, path string) error {
	b.Delete(path)
	return nil
}

// Keys returns every registered object key. Test helper for asserting on
// the backend's contents; ordering is unspecified.
func (b *Backend) Keys() []string {
	b.mu.RLock()
	defer b.mu.RUnlock()
	out := make([]string, 0, len(b.objects))
	for k := range b.objects {
		out = append(out, k)
	}
	return out
}

// Open returns a File reading bytes registered at path. Returns
// storage.ErrNotFound if no such path is registered.
func (b *Backend) Open(_ context.Context, path string) (storage.File, error) {
	b.mu.RLock()
	data, ok := b.objects[path]
	b.mu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("%w: %s", storage.ErrNotFound, path)
	}
	return &file{r: bytes.NewReader(data), size: int64(len(data))}, nil
}

// OpenSnapshot is safe because Put and writer Close publish fresh byte slices;
// an open bytes.Reader retains the previous slice across a replacement.
func (b *Backend) OpenSnapshot(ctx context.Context, path string) (storage.File, error) {
	return b.Open(ctx, path)
}

// Create returns a Writer that, on Close, registers its accumulated
// bytes at path (replacing any prior value). Until Close, Open(path)
// still sees the old bytes — the new entry is only published atomically
// at flush time, mirroring most cloud-storage write semantics.
func (b *Backend) Create(_ context.Context, path string) (storage.Writer, error) {
	return &writer{b: b, path: path, buf: &bytes.Buffer{}}, nil
}

// CreateExclusive creates path on Close only if it is still absent.
func (b *Backend) CreateExclusive(_ context.Context, path string) (storage.Writer, error) {
	return &writer{b: b, path: path, buf: &bytes.Buffer{}, exclusive: true}, nil
}

// writer is the memory-backend's Writer. Buffers writes; on Close
// publishes the buffered bytes to b.objects[path].
type writer struct {
	b         *Backend
	path      string
	buf       *bytes.Buffer
	closed    bool
	aborted   bool
	exclusive bool
}

func (w *writer) Write(p []byte) (int, error) {
	if w.closed || w.aborted {
		return 0, fmt.Errorf("memory: write after close")
	}
	return w.buf.Write(p)
}

func (w *writer) Close() error {
	if w.aborted {
		return fmt.Errorf("memory: writer already aborted")
	}
	if w.closed {
		return nil
	}
	if w.exclusive {
		w.b.mu.Lock()
		defer w.b.mu.Unlock()
		if _, exists := w.b.objects[w.path]; exists {
			// Leave the writer open so a repeated Close reports the
			// conflict again instead of success, and Abort still works.
			return fmt.Errorf("%w: %s", storage.ErrExists, w.path)
		}
		w.b.objects[w.path] = bytes.Clone(w.buf.Bytes())
		w.closed = true
		return nil
	}
	w.closed = true
	w.b.Put(w.path, w.buf.Bytes())
	return nil
}

func (w *writer) Abort() error {
	if w.aborted {
		return nil
	}
	if w.closed {
		return fmt.Errorf("memory: writer already closed")
	}
	w.aborted = true
	w.buf.Reset()
	return nil
}

// file wraps bytes.Reader as a storage.File. bytes.Reader already
// satisfies io.ReaderAt; we add Close (no-op) and Size.
type file struct {
	r    *bytes.Reader
	size int64
}

func (m *file) ReadAt(p []byte, off int64) (int, error) { return m.r.ReadAt(p, off) }
func (m *file) Close() error                            { return nil }
func (m *file) Size() int64                             { return m.size }
