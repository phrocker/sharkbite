package cache

import (
	"container/list"
	"sync"
	"sync/atomic"
)

// BlockCache is a per-replica byte-bounded LRU of decompressed RFile
// blocks. Keyed by (file path, block offset). The decompressed payload
// is held by reference (no copy on Put / Get) — callers MUST treat the
// returned []byte as read-only.
//
// Why per-replica (not shared via Redis): hedged reads depend on the
// fastest replica winning. A shared cache means every replica gets
// hot-path identical performance, defeating the hedge benefit. Per-
// replica cache lets the request that lands on a warm replica beat
// the cold one cleanly.
//
// Concurrency: Mutex (not RWMutex) because every Get is also a write
// (move-to-front on the LRU list). Hit rate dominates; the lock is
// rarely contended in practice.
//
// Eviction: simple LRU on insert. When `bytes + new payload > capacity`,
// oldest entries are dropped until under cap. Single-pass, no async
// reaping. If a single block exceeds the entire capacity, it's stored
// anyway and immediately evicts everything else (caller can configure
// per-block max via reading code if that's a problem).
type BlockCache struct {
	mu       sync.Mutex
	capacity int64
	bytes    int64
	lru      *list.List
	lookup   map[blockKey]*list.Element

	hits   atomic.Int64
	misses atomic.Int64
	evicts atomic.Int64
}

// blockKey identifies one cached block. We use a struct (not a string)
// so the path's underlying bytes don't escape into per-Get allocations.
type blockKey struct {
	path   string
	offset int64
}

// blockEntry holds the cached payload + bookkeeping. The payload is the
// FULLY DECOMPRESSED block bytes, owned by the cache.
type blockEntry struct {
	key     blockKey
	payload []byte
}

// NewBlockCache constructs a cache with the given byte budget. A
// capacity of zero or negative disables caching (Get always misses,
// Put is a no-op).
func NewBlockCache(capacityBytes int64) *BlockCache {
	return &BlockCache{
		capacity: capacityBytes,
		lru:      list.New(),
		lookup:   make(map[blockKey]*list.Element),
	}
}

// Get returns the cached block for (path, offset) if present, otherwise
// (nil, false). On hit, the entry is moved to the front of the LRU.
func (c *BlockCache) Get(path string, offset int64) ([]byte, bool) {
	if c == nil || c.capacity <= 0 {
		c.misses.Add(1)
		return nil, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	el, ok := c.lookup[blockKey{path, offset}]
	if !ok {
		c.misses.Add(1)
		return nil, false
	}
	c.lru.MoveToFront(el)
	c.hits.Add(1)
	return el.Value.(*blockEntry).payload, true
}

// Put inserts (path, offset) → payload into the cache. If a prior entry
// exists for the same key, it's replaced (rare — every block has a
// stable offset and is read once per file). Triggers eviction if the
// new size pushes total bytes over capacity.
//
// The payload is NOT copied — caller hands ownership to the cache.
// Don't mutate after Put.
func (c *BlockCache) Put(path string, offset int64, payload []byte) {
	if c == nil || c.capacity <= 0 || len(payload) == 0 {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	key := blockKey{path, offset}
	if el, ok := c.lookup[key]; ok {
		// Replace: subtract the old size, install the new payload.
		old := el.Value.(*blockEntry).payload
		c.bytes -= int64(len(old))
		el.Value.(*blockEntry).payload = payload
		c.bytes += int64(len(payload))
		c.lru.MoveToFront(el)
	} else {
		entry := &blockEntry{key: key, payload: payload}
		el := c.lru.PushFront(entry)
		c.lookup[key] = el
		c.bytes += int64(len(payload))
	}
	for c.bytes > c.capacity && c.lru.Len() > 0 {
		c.evictOldestLocked()
	}
}

func (c *BlockCache) evictOldestLocked() {
	el := c.lru.Back()
	if el == nil {
		return
	}
	entry := el.Value.(*blockEntry)
	c.lru.Remove(el)
	delete(c.lookup, entry.key)
	c.bytes -= int64(len(entry.payload))
	c.evicts.Add(1)
}

// Stats reports cumulative hit/miss/evict counts. Useful for log lines
// at scan completion or a /metrics endpoint.
type Stats struct {
	Hits, Misses, Evicts int64
	BytesUsed, Capacity  int64
	Entries              int
}

func (c *BlockCache) Stats() Stats {
	if c == nil {
		return Stats{}
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return Stats{
		Hits:      c.hits.Load(),
		Misses:    c.misses.Load(),
		Evicts:    c.evicts.Load(),
		BytesUsed: c.bytes,
		Capacity:  c.capacity,
		Entries:   c.lru.Len(),
	}
}

// Drop removes all entries for the given path — used when a file has
// been replaced (e.g., compaction produced a new RFile and the old one
// is being deleted). O(N) over the entire cache; rare operation.
func (c *BlockCache) Drop(path string) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	for el := c.lru.Front(); el != nil; {
		next := el.Next()
		entry := el.Value.(*blockEntry)
		if entry.key.path == path {
			c.lru.Remove(el)
			delete(c.lookup, entry.key)
			c.bytes -= int64(len(entry.payload))
		}
		el = next
	}
}
