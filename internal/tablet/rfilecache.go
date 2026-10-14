// Licensed to the Apache Software Foundation (ASF) under one
// or more contributor license agreements.  See the NOTICE file
// distributed with this work for additional information
// regarding copyright ownership.  The ASF licenses this file
// to you under the Apache License, Version 2.0 (the
// "License"); you may not use this file except in compliance
// with the License.  You may obtain a copy of the License at
//
//   https://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing,
// software distributed under the License is distributed on an
// "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY
// KIND, either express or implied.  See the License for the
// specific language governing permissions and limitations
// under the License.

package tablet

import (
	"bytes"
	"context"
	"sync"

	"github.com/phrocker/shoal-oss/internal/cache"
	"github.com/phrocker/shoal-oss/internal/rfile"
	"github.com/phrocker/shoal-oss/internal/rfile/bcfile"
	"github.com/phrocker/shoal-oss/internal/rfile/bcfile/block"
	"github.com/phrocker/shoal-oss/internal/storage"
	"github.com/phrocker/shoal-oss/internal/storage/local"
)

// DefaultCacheBytes is the default budget shared between the file-bytes
// cache and the decompressed-block cache when a Cache is created with a
// non-positive capacity.
const DefaultCacheBytes = 256 << 20 // 256 MB

// Cache makes repeated scans against the same tablet cheap. Without it,
// every Scan re-reads each RFile from disk (os.ReadFile) and re-inflates
// its data blocks (snappy) — fixed per-scan overhead that dominates
// point lookups, where the answer is a single cell but the whole file is
// touched. Profiling a random point-lookup workload showed ~73% of CPU
// in the file-read syscall and ~40% of allocations in block
// decompression; both are eliminated on a warm cache.
//
// Two layers, both keyed by the immutable RFile path:
//
//   - files: the raw RFile bytes, so the bcfile reader's footer/index
//     parse runs against memory instead of a fresh disk read.
//   - blocks: a cache.BlockCache of decompressed data blocks, so a
//     re-seek into an already-touched block skips snappy entirely.
//
// RFiles are immutable once written — a flush or compaction always emits
// a brand-new, uniquely named path and never rewrites an existing one —
// so cached entries never need invalidation for correctness. Drop is a
// memory hygiene call made when a compaction deletes an input file.
//
// A Cache is safe for concurrent use and is meant to be shared by every
// tablet of an engine so the byte budget is global, not per-tablet.
type Cache struct {
	mu    sync.Mutex
	files map[string][]byte
	order []string // insertion order, for FIFO eviction
	used  int64
	cap   int64

	blocks *cache.BlockCache

	// backend is the object store RFile bytes are faulted from on a cache
	// miss. Defaults to the local filesystem so a path is read with
	// os.Open semantics; a memory/cloud backend makes the same cache serve
	// objects that never touch local disk. Immutable after construction.
	backend storage.Backend

	// shared holds the parse-once immutable index state per path so a
	// point lookup builds a cheap cursor instead of re-parsing the RFile
	// index and re-collecting every leaf on each Seek. Guarded by sfMu
	// (separate from mu: building one entry reads file bytes + blocks and
	// must not block byte-cache lookups for other paths).
	sfMu   sync.Mutex
	shared map[string]*rfile.SharedFile
}

// NewCache returns a Cache sharing capBytes between the file-bytes and
// block caches, faulting RFile bytes from the local filesystem. A
// non-positive capBytes uses DefaultCacheBytes.
func NewCache(capBytes int64) *Cache {
	return NewCacheWithBackend(capBytes, nil)
}

// NewCacheWithBackend is NewCache with an explicit object-store backend
// that RFile bytes are read from on a miss. A nil backend defaults to the
// local filesystem (preserving os.Open behavior).
func NewCacheWithBackend(capBytes int64, backend storage.Backend) *Cache {
	if capBytes <= 0 {
		capBytes = DefaultCacheBytes
	}
	if backend == nil {
		backend = local.New()
	}
	return &Cache{
		files:   make(map[string][]byte),
		cap:     capBytes,
		blocks:  cache.NewBlockCache(capBytes),
		shared:  make(map[string]*rfile.SharedFile),
		backend: backend,
	}
}

// blockCache returns the shared decompressed-block cache, or nil for a
// nil Cache (callers treat nil as "no caching").
func (c *Cache) blockCache() *cache.BlockCache {
	if c == nil {
		return nil
	}
	return c.blocks
}

// sharedFile returns the parse-once SharedFile for path, building and
// memoizing it on a miss. data is the cached RFile bytes; blocks is the
// shared decompressed-block cache (may be nil). For a nil Cache the file
// is parsed fresh (no memoization) — still once per scan rather than once
// per cursor. The returned SharedFile is immutable and safe to share
// across concurrent cursor Readers.
func (c *Cache) sharedFile(path string, data []byte, blocks *cache.BlockCache) (*rfile.SharedFile, error) {
	build := func() (*rfile.SharedFile, error) {
		bc, err := bcfile.NewReader(bytes.NewReader(data), int64(len(data)))
		if err != nil {
			return nil, err
		}
		var opts []rfile.OpenOption
		if blocks != nil {
			opts = append(opts, rfile.WithBlockCache(blocks, path))
		}
		return rfile.OpenShared(bc, block.Default(), opts...)
	}

	if c == nil {
		return build()
	}

	c.sfMu.Lock()
	defer c.sfMu.Unlock()
	if sf, ok := c.shared[path]; ok {
		return sf, nil
	}
	sf, err := build()
	if err != nil {
		return nil, err
	}
	c.shared[path] = sf
	return sf, nil
}

// sharedForPath returns the memoized SharedFile for path, loading the
// file bytes (from cache or disk) and wiring the block cache as needed.
// Mirrors the open sequence in openRFileSource but yields just the
// shared, immutable parse — used by Neighbors to consult a file's
// adjacency index without opening a scan cursor.
func (c *Cache) sharedForPath(path string) (*rfile.SharedFile, error) {
	data, err := c.fileBytes(path)
	if err != nil {
		return nil, err
	}
	return c.sharedFile(path, data, c.blockCache())
}

// miss. The returned slice is shared and MUST NOT be mutated by callers
// (RFile bytes are immutable, so this is safe — readers wrap it in a
// read-only bytes.Reader). Bytes are faulted from the configured backend
// (local filesystem by default). A nil Cache reads straight from the
// backend with no memoization.
func (c *Cache) fileBytes(path string) ([]byte, error) {
	return c.fileBytesContext(context.Background(), path)
}

func (c *Cache) fileBytesContext(ctx context.Context, path string) ([]byte, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if c == nil {
		return storage.ReadAll(ctx, local.New(), path)
	}
	c.mu.Lock()
	if b, ok := c.files[path]; ok {
		c.mu.Unlock()
		return b, nil
	}
	c.mu.Unlock()

	// Read outside the lock; concurrent readers of the same fresh path
	// may both read from the backend, but they converge on one cached copy.
	b, err := storage.ReadAll(ctx, c.backend, path)
	if err != nil {
		return nil, err
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	if existing, ok := c.files[path]; ok {
		return existing, nil // lost the race; reuse the winner
	}
	c.evictToFitLocked(int64(len(b)))
	c.files[path] = b
	c.order = append(c.order, path)
	c.used += int64(len(b))
	return b, nil
}

// evictToFitLocked drops oldest entries until incoming bytes fit within
// the budget. Caller holds c.mu. A single file larger than the budget is
// still admitted (the alternative — never caching it — defeats the
// purpose for small working sets).
func (c *Cache) evictToFitLocked(incoming int64) {
	for c.used+incoming > c.cap && len(c.order) > 0 {
		oldest := c.order[0]
		c.order = c.order[1:]
		if b, ok := c.files[oldest]; ok {
			c.used -= int64(len(b))
			delete(c.files, oldest)
			c.blocks.Drop(oldest)
			c.dropShared(oldest)
		}
	}
}

// Drop evicts a path from both layers. Called when a compaction deletes
// an input RFile so its bytes/blocks don't linger until FIFO eviction.
func (c *Cache) Drop(path string) {
	if c == nil {
		return
	}
	c.mu.Lock()
	if b, ok := c.files[path]; ok {
		c.used -= int64(len(b))
		delete(c.files, path)
		for i, p := range c.order {
			if p == path {
				c.order = append(c.order[:i], c.order[i+1:]...)
				break
			}
		}
	}
	c.mu.Unlock()
	c.blocks.Drop(path)
	c.dropShared(path)
}

// dropShared removes the memoized SharedFile for path, if any. Safe for a
// nil Cache. Held separately from mu since it guards the shared map.
func (c *Cache) dropShared(path string) {
	if c == nil {
		return
	}
	c.sfMu.Lock()
	delete(c.shared, path)
	c.sfMu.Unlock()
}
