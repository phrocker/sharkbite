// Package diskcache provides a read-through local-disk cache in front of a
// storage.Backend. RFile bytes are cached on a local SSD/PVC dir, so a
// "cold" read is a local-disk hit instead of a remote object-store round
// trip. shoal reads raw from the object store otherwise, which is why cold
// shoal scans (~800ms-1s/file) lose to a local-disk read (~ms); this closes
// that gap.
//
// Caching model: whole-file, keyed by a hash of the object path. RFiles are
// immutable (compaction writes new paths), so a cache entry never goes
// stale — a present, fully-written cache file is authoritative and needs no
// revalidation against the backend. The cache is bounded by total bytes and
// LRU-evicted.
//
// It is backend-agnostic: because it wraps any storage.Backend by
// composition, the same cache tier works over GCS, S3, Azure Blob, or local
// storage.
package diskcache

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"

	"github.com/phrocker/shoal-oss/internal/storage"
)

// Backend wraps an inner storage.Backend with an on-disk read-through cache.
type Backend struct {
	inner    storage.Backend
	dir      string
	maxBytes int64

	mu      sync.Mutex
	cond    *sync.Cond // signals when an in-flight download finishes
	lru     *lruList
	index   map[string]*lruNode // hash → node
	flights map[string]bool     // hashes with an in-flight download
	bytes   int64

	hits, misses, evicts, downloadErrs, coalesced uint64
}

var _ storage.Backend = (*Backend)(nil)

// InnerBackend exposes the wrapped backend so callers that need to reason
// about the original path semantics (for example local-vs-remote alias
// detection) can unwrap the cache layer.
func (b *Backend) InnerBackend() storage.Backend { return b.inner }

// New constructs a disk cache over inner, storing files under dir (created
// if absent) and bounding total resident bytes to maxBytes. Existing files
// in dir are adopted into the LRU so a restart with a persistent (PVC)
// cache dir reuses prior downloads. maxBytes <= 0 is an error.
func New(inner storage.Backend, dir string, maxBytes int64) (*Backend, error) {
	if inner == nil {
		return nil, errors.New("diskcache: nil inner backend")
	}
	if dir == "" {
		return nil, errors.New("diskcache: empty cache dir")
	}
	if maxBytes <= 0 {
		return nil, errors.New("diskcache: maxBytes must be > 0")
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("diskcache: mkdir %q: %w", dir, err)
	}
	b := &Backend{
		inner:    inner,
		dir:      dir,
		maxBytes: maxBytes,
		lru:      newLRUList(),
		index:    make(map[string]*lruNode),
		flights:  make(map[string]bool),
	}
	b.cond = sync.NewCond(&b.mu)
	b.adoptExisting()
	return b, nil
}

// adoptExisting scans the cache dir and registers complete cache files
// (skipping *.tmp partials) into the LRU. Best-effort; unreadable entries
// are ignored. Evicts down to budget if a prior run left more than fits.
func (b *Backend) adoptExisting() {
	entries, err := os.ReadDir(b.dir)
	if err != nil {
		return
	}
	for _, e := range entries {
		if e.IsDir() || filepath.Ext(e.Name()) == ".tmp" {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		node := &lruNode{hash: e.Name(), size: info.Size()}
		b.index[e.Name()] = node
		b.lru.pushFront(node)
		b.bytes += info.Size()
	}
	b.evictLocked()
}

func hashPath(path string) string {
	sum := sha256.Sum256([]byte(path))
	return hex.EncodeToString(sum[:])
}

// Open returns a File for path, served from the local cache. On a hit the
// bytes are read from disk with no backend call. On a miss the whole object
// is streamed once from the inner backend into the cache (bounded memory),
// then served locally.
//
// Concurrent misses for the same path are coalesced (single-flight): the
// first caller downloads while the rest wait on the condition variable and
// then serve from the freshly cached file. This avoids N redundant backend
// fetches of the same cold RFile under scan fan-out.
func (b *Backend) Open(ctx context.Context, path string) (storage.File, error) {
	hash := hashPath(path)
	cachePath := filepath.Join(b.dir, hash)

	b.mu.Lock()
	for {
		// Hit: present in index. Trust a present file (RFiles immutable).
		if node, ok := b.index[hash]; ok {
			b.lru.moveFront(node)
			size := node.size
			b.mu.Unlock()
			if f, err := openCached(cachePath, size); err == nil {
				b.mu.Lock()
				b.hits++
				b.mu.Unlock()
				return f, nil
			}
			// File vanished under us (evicted/removed) — drop the stale
			// index entry and retry as a miss.
			b.mu.Lock()
			if cur, ok := b.index[hash]; ok && cur == node {
				b.removeLocked(cur)
			}
			continue
		}
		// A download for this key is already in flight — wait for it,
		// then re-check the index.
		if b.flights[hash] {
			b.coalesced++
			b.cond.Wait()
			continue
		}
		// We own the download for this key.
		b.flights[hash] = true
		b.misses++
		break
	}
	b.mu.Unlock()

	// Miss: fetch the whole object once and materialize it on disk.
	size, err := b.download(ctx, path, cachePath)

	b.mu.Lock()
	delete(b.flights, hash)
	if err == nil {
		if _, ok := b.index[hash]; !ok {
			node := &lruNode{hash: hash, size: size}
			b.index[hash] = node
			b.lru.pushFront(node)
			b.bytes += size
			b.evictLocked()
		}
	} else {
		b.downloadErrs++
	}
	b.cond.Broadcast()
	b.mu.Unlock()

	if err != nil {
		return nil, err
	}
	f, err := openCached(cachePath, size)
	if err != nil {
		return nil, fmt.Errorf("diskcache: reopen %q: %w", cachePath, err)
	}
	return f, nil
}

// openCached opens a cached file for random-access reads, tagged with its
// known size.
func openCached(cachePath string, size int64) (storage.File, error) {
	f, err := os.Open(cachePath)
	if err != nil {
		return nil, err
	}
	return &diskFile{f: f, size: size}, nil
}

// download streams the whole object from the inner backend into a temp file
// in the cache dir, then atomically renames it into place. Streaming (not a
// single big buffer) keeps memory bounded so a 460MB RFile doesn't spike the
// heap. A size mismatch or write error leaves no cache entry.
func (b *Backend) download(ctx context.Context, path, cachePath string) (int64, error) {
	in, err := b.inner.Open(ctx, path)
	if err != nil {
		return 0, fmt.Errorf("diskcache: open inner %q: %w", path, err)
	}
	defer in.Close()
	size := in.Size()

	tmp, err := os.CreateTemp(b.dir, filepath.Base(cachePath)+"-*.tmp")
	if err != nil {
		return 0, fmt.Errorf("diskcache: create temp: %w", err)
	}
	tmpName := tmp.Name()
	// Best-effort cleanup if we don't reach the successful rename.
	defer func() {
		if tmpName != "" {
			_ = os.Remove(tmpName)
		}
	}()

	const chunk = 4 << 20 // 4MB — bounded transient memory
	buf := make([]byte, chunk)
	var off int64
	for off < size {
		want := int64(chunk)
		if off+want > size {
			want = size - off
		}
		n, rerr := in.ReadAt(buf[:want], off)
		if n > 0 {
			if _, werr := tmp.Write(buf[:n]); werr != nil {
				_ = tmp.Close()
				return 0, fmt.Errorf("diskcache: write temp: %w", werr)
			}
			off += int64(n)
		}
		if rerr != nil {
			if errors.Is(rerr, io.EOF) {
				break
			}
			_ = tmp.Close()
			return 0, fmt.Errorf("diskcache: read inner off=%d: %w", off, rerr)
		}
	}
	if err := tmp.Close(); err != nil {
		return 0, fmt.Errorf("diskcache: close temp: %w", err)
	}
	if off != size {
		return 0, fmt.Errorf("diskcache: short download %q: got %d want %d", path, off, size)
	}
	if err := os.Rename(tmpName, cachePath); err != nil {
		// Lost a concurrent-miss race: another goroutine already placed
		// this (immutable) object at cachePath. On Linux rename would
		// atomically replace it; on Windows it fails when the target
		// exists or is open. Either way, if a complete file is already
		// there, our download succeeded in effect — drop our temp and
		// use the winner's file.
		if st, serr := os.Stat(cachePath); serr == nil && st.Size() == size {
			return size, nil
		}
		return 0, fmt.Errorf("diskcache: rename into place: %w", err)
	}
	tmpName = "" // renamed — don't remove
	return size, nil
}

// evictLocked drops LRU-tail entries until total bytes are within budget.
// Deleting an open file is safe on Linux — in-flight readers keep their fd.
// Caller holds b.mu.
func (b *Backend) evictLocked() {
	for b.bytes > b.maxBytes {
		node := b.lru.back()
		if node == nil {
			return
		}
		b.removeLocked(node)
		_ = os.Remove(filepath.Join(b.dir, node.hash))
		b.evicts++
	}
}

func (b *Backend) removeLocked(node *lruNode) {
	if _, ok := b.index[node.hash]; !ok {
		return
	}
	delete(b.index, node.hash)
	b.lru.remove(node)
	b.bytes -= node.size
}

// Stats is a snapshot of cache counters for the metrics endpoint.
type Stats struct {
	Hits, Misses, Evicts, DownloadErrs, Coalesced uint64
	BytesUsed, MaxBytes                           int64
	Entries                                       int
}

func (b *Backend) Stats() Stats {
	b.mu.Lock()
	defer b.mu.Unlock()
	return Stats{
		Hits: b.hits, Misses: b.misses, Evicts: b.evicts,
		DownloadErrs: b.downloadErrs, Coalesced: b.coalesced,
		BytesUsed: b.bytes, MaxBytes: b.maxBytes, Entries: len(b.index),
	}
}

// diskFile serves reads from a cached local file. os.File.ReadAt uses pread,
// so it is safe for concurrent use across scan goroutines.
type diskFile struct {
	f    *os.File
	size int64
}

func (d *diskFile) ReadAt(p []byte, off int64) (int, error) { return d.f.ReadAt(p, off) }
func (d *diskFile) Size() int64                             { return d.size }
func (d *diskFile) Close() error                            { return d.f.Close() }
