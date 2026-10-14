package scanserver

import (
	"container/list"
	"sync"
	"sync/atomic"
)

// fileCache holds the raw bytes of opened RFiles keyed by storage path,
// LRU-evicted when total cached bytes exceed the cap.
//
// Why this exists: tserver wins hedge races today not because shoal is
// slow at decode but because tserver already has the file blocks in its
// own in-memory + PVC + page caches. Shoal pulls 32MB cold from GCS on
// every scan, taking ~800ms. Caching the FULL file bytes (not just
// decompressed blocks) lets warm shoal scans skip the GCS round-trip
// entirely; opens drop from ~800ms to a few ms.
//
// Trade-off: we hold the full compressed file in memory. A 32MB file
// per tablet × dozens of tablets = hundreds of MB. The default cap is
// 1GB; tune via scanserver.Options.FileBytesCap.
//
// Eviction: LRU on total bytes. When inserting would push over the cap,
// the least-recently-used files get dropped until under cap. Critical
// invariant: an in-flight scan holding a reference to evicted bytes
// keeps using them; eviction just removes the cache entry, the bytes
// stay alive until GC reclaims them.
type fileCache struct {
	mu       sync.Mutex
	cap      int64
	bytes    int64
	lru      *list.List
	lookup   map[string]*list.Element

	hits   atomic.Int64
	misses atomic.Int64
	evicts atomic.Int64
}

type fileEntry struct {
	path  string
	bytes []byte
}

func newFileCache(capBytes int64) *fileCache {
	return &fileCache{
		cap:    capBytes,
		lru:    list.New(),
		lookup: make(map[string]*list.Element),
	}
}

// Get returns the cached bytes for path, or (nil, false). Move-to-front on hit.
func (c *fileCache) Get(path string) ([]byte, bool) {
	if c == nil || c.cap <= 0 {
		c.misses.Add(1)
		return nil, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	el, ok := c.lookup[path]
	if !ok {
		c.misses.Add(1)
		return nil, false
	}
	c.lru.MoveToFront(el)
	c.hits.Add(1)
	return el.Value.(*fileEntry).bytes, true
}

// Put stores bytes for path. Caller hands ownership; do not mutate after.
func (c *fileCache) Put(path string, bytes []byte) {
	if c == nil || c.cap <= 0 || len(bytes) == 0 {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if el, ok := c.lookup[path]; ok {
		old := el.Value.(*fileEntry).bytes
		c.bytes -= int64(len(old))
		el.Value.(*fileEntry).bytes = bytes
		c.bytes += int64(len(bytes))
		c.lru.MoveToFront(el)
	} else {
		entry := &fileEntry{path: path, bytes: bytes}
		el := c.lru.PushFront(entry)
		c.lookup[path] = el
		c.bytes += int64(len(bytes))
	}
	for c.bytes > c.cap && c.lru.Len() > 0 {
		c.evictOldestLocked()
	}
}

func (c *fileCache) evictOldestLocked() {
	el := c.lru.Back()
	if el == nil {
		return
	}
	entry := el.Value.(*fileEntry)
	c.lru.Remove(el)
	delete(c.lookup, entry.path)
	c.bytes -= int64(len(entry.bytes))
	c.evicts.Add(1)
}

func (c *fileCache) Stats() (hits, misses, evicts, bytesUsed int64, entries int) {
	if c == nil {
		return 0, 0, 0, 0, 0
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.hits.Load(), c.misses.Load(), c.evicts.Load(), c.bytes, c.lru.Len()
}
