package diskcache

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/phrocker/shoal-oss/internal/storage"
	"github.com/phrocker/shoal-oss/internal/storage/memory"
)

// openCountingBackend wraps a storage.Backend and counts Open calls, so a
// test can prove a cache hit avoids re-fetching from the inner backend.
type openCountingBackend struct {
	inner storage.Backend
	opens *atomic.Int64
}

func (c *openCountingBackend) Open(ctx context.Context, path string) (storage.File, error) {
	c.opens.Add(1)
	return c.inner.Open(ctx, path)
}

func newMem(t *testing.T, path string, data []byte) *memory.Backend {
	t.Helper()
	mem := memory.New()
	mem.Put(path, data)
	return mem
}

func readAll(t *testing.T, b *Backend, path string, size int64) []byte {
	t.Helper()
	f, err := b.Open(context.Background(), path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer f.Close()
	buf := make([]byte, size)
	if _, err := f.ReadAt(buf, 0); err != nil {
		t.Fatalf("ReadAt: %v", err)
	}
	return buf
}

func TestDiskCache_HitAvoidsInnerRefetch(t *testing.T) {
	const path = "gs://b/obj1"
	data := bytes.Repeat([]byte("shoal-"), 5000) // 30KB
	mem := newMem(t, path, data)

	var opens atomic.Int64
	dc, err := New(&openCountingBackend{inner: mem, opens: &opens}, t.TempDir(), 1<<30)
	if err != nil {
		t.Fatal(err)
	}

	if got := readAll(t, dc, path, int64(len(data))); !bytes.Equal(got, data) {
		t.Fatal("first read mismatch")
	}
	if got := readAll(t, dc, path, int64(len(data))); !bytes.Equal(got, data) {
		t.Fatal("second read mismatch")
	}
	if opens.Load() != 1 {
		t.Errorf("inner opened %d times; want 1 (2nd served from disk)", opens.Load())
	}
	if st := dc.Stats(); st.Hits != 1 || st.Misses != 1 || st.Entries != 1 {
		t.Errorf("stats hits=%d misses=%d entries=%d; want 1/1/1", st.Hits, st.Misses, st.Entries)
	}
}

func TestDiskCache_EvictsByBytes(t *testing.T) {
	mem := memory.New()
	for i := 0; i < 3; i++ {
		mem.Put(fmt.Sprintf("gs://b/obj%d", i), bytes.Repeat([]byte("x"), 20000))
	}
	dc, err := New(mem, t.TempDir(), 30000) // budget holds one 20KB object
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		readAll(t, dc, fmt.Sprintf("gs://b/obj%d", i), 20000)
	}
	st := dc.Stats()
	if st.Entries != 1 {
		t.Errorf("entries=%d; want 1 (budget holds one 20KB object)", st.Entries)
	}
	if st.Evicts == 0 {
		t.Error("expected evictions under a tight byte budget")
	}
	if st.BytesUsed > st.MaxBytes {
		t.Errorf("bytesUsed=%d exceeds maxBytes=%d", st.BytesUsed, st.MaxBytes)
	}
}

func TestDiskCache_AdoptsExistingOnRestart(t *testing.T) {
	dir := t.TempDir()
	const path = "gs://b/persist"
	data := bytes.Repeat([]byte("z"), 12345)
	mem := newMem(t, path, data)

	var opens atomic.Int64
	dc1, err := New(&openCountingBackend{inner: mem, opens: &opens}, dir, 1<<30)
	if err != nil {
		t.Fatal(err)
	}
	readAll(t, dc1, path, int64(len(data))) // downloads once

	// New instance over the SAME dir (simulates restart with a PVC).
	dc2, err := New(&openCountingBackend{inner: mem, opens: &opens}, dir, 1<<30)
	if err != nil {
		t.Fatal(err)
	}
	if dc2.Stats().Entries != 1 {
		t.Fatalf("rebuilt index entries = %d, want 1", dc2.Stats().Entries)
	}
	if got := readAll(t, dc2, path, int64(len(data))); !bytes.Equal(got, data) {
		t.Fatal("post-restart read mismatch")
	}
	if opens.Load() != 1 {
		t.Errorf("inner opened %d times; want 1 (restart reused cached file)", opens.Load())
	}
}

func TestDiskCache_ConcurrentColdReads(t *testing.T) {
	const path = "gs://b/concurrent"
	data := bytes.Repeat([]byte("q"), 100000)
	mem := newMem(t, path, data)
	dc, err := New(mem, t.TempDir(), 1<<30)
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if got := readAll(t, dc, path, int64(len(data))); !bytes.Equal(got, data) {
				t.Error("concurrent read mismatch")
			}
		}()
	}
	wg.Wait()
	if st := dc.Stats(); st.Entries != 1 {
		t.Errorf("entries=%d; want 1", st.Entries)
	}
}

func TestDiskCache_NewRejectsBadArgs(t *testing.T) {
	if _, err := New(nil, t.TempDir(), 1<<20); err == nil {
		t.Error("nil inner backend should error")
	}
	if _, err := New(memory.New(), "", 1<<20); err == nil {
		t.Error("empty dir should error")
	}
	if _, err := New(memory.New(), t.TempDir(), 0); err == nil {
		t.Error("maxBytes <= 0 should error")
	}
}

func TestDiskCache_StaleFileTriggersRefetch(t *testing.T) {
	dir := t.TempDir()
	const path = "gs://b/stale"
	data := []byte("value")
	mem := newMem(t, path, data)

	var opens atomic.Int64
	dc, err := New(&openCountingBackend{inner: mem, opens: &opens}, dir, 1<<30)
	if err != nil {
		t.Fatal(err)
	}
	readAll(t, dc, path, int64(len(data)))

	// Manually delete the on-disk cache file while the index still holds it.
	if err := os.Remove(filepath.Join(dir, hashPath(path))); err != nil {
		t.Fatal(err)
	}
	if got := string(readAll(t, dc, path, int64(len(data)))); got != "value" {
		t.Fatalf("post-purge read got %q", got)
	}
	if opens.Load() != 2 {
		t.Errorf("stale file should force re-fetch; inner opens = %d, want 2", opens.Load())
	}
}

func TestDiskCache_NotFoundPropagates(t *testing.T) {
	dc, err := New(memory.New(), t.TempDir(), 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	_, err = dc.Open(context.Background(), "missing")
	if !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}

// gateBackend blocks the first Open until release() is called, so a test can
// pile multiple concurrent misses onto a single in-flight download and prove
// single-flight coalescing.
type gateBackend struct {
	inner   storage.Backend
	opens   atomic.Int64
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func (g *gateBackend) Open(ctx context.Context, path string) (storage.File, error) {
	g.opens.Add(1)
	g.once.Do(func() { close(g.entered) })
	<-g.release
	return g.inner.Open(ctx, path)
}

func TestDiskCache_SingleFlightCoalesces(t *testing.T) {
	const path = "gs://b/coalesce"
	data := bytes.Repeat([]byte("m"), 40000)
	g := &gateBackend{
		inner:   newMem(t, path, data),
		entered: make(chan struct{}),
		release: make(chan struct{}),
	}
	dc, err := New(g, t.TempDir(), 1<<30)
	if err != nil {
		t.Fatal(err)
	}

	const n = 12
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if got := readAll(t, dc, path, int64(len(data))); !bytes.Equal(got, data) {
				t.Error("coalesced read mismatch")
			}
		}()
	}
	// Wait until the single downloader is inside the backend, give the
	// other goroutines a moment to queue on the in-flight download, then
	// release it.
	<-g.entered
	for dc.Stats().Coalesced < n-1 {
		time.Sleep(time.Millisecond)
	}
	close(g.release)
	wg.Wait()

	if g.opens.Load() != 1 {
		t.Errorf("inner opened %d times; want 1 (single-flight)", g.opens.Load())
	}
	st := dc.Stats()
	if st.Misses != 1 {
		t.Errorf("misses = %d; want 1", st.Misses)
	}
	if st.Coalesced != n-1 {
		t.Errorf("coalesced = %d; want %d", st.Coalesced, n-1)
	}
	if st.Entries != 1 {
		t.Errorf("entries = %d; want 1", st.Entries)
	}
}
