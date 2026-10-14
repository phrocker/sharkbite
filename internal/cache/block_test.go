package cache

import (
	"bytes"
	"fmt"
	"sync"
	"testing"
)

func TestBlockCache_HitMiss(t *testing.T) {
	c := NewBlockCache(1024)
	if _, ok := c.Get("a.rf", 0); ok {
		t.Errorf("empty cache should miss")
	}
	c.Put("a.rf", 0, []byte("hello"))
	if v, ok := c.Get("a.rf", 0); !ok || !bytes.Equal(v, []byte("hello")) {
		t.Errorf("Get after Put = %v %v", v, ok)
	}
	st := c.Stats()
	if st.Hits != 1 || st.Misses != 1 {
		t.Errorf("Stats hits=%d misses=%d; want 1, 1", st.Hits, st.Misses)
	}
}

func TestBlockCache_LRUEviction(t *testing.T) {
	// Capacity 100; put four 30-byte entries → fourth pushes the first out.
	c := NewBlockCache(100)
	for i := 0; i < 4; i++ {
		c.Put("f.rf", int64(i*1000), bytes.Repeat([]byte{byte('a' + i)}, 30))
	}
	// First entry should be gone; last three remain.
	if _, ok := c.Get("f.rf", 0); ok {
		t.Errorf("oldest entry should have been evicted")
	}
	for i := 1; i < 4; i++ {
		if _, ok := c.Get("f.rf", int64(i*1000)); !ok {
			t.Errorf("entry %d should still be cached", i)
		}
	}
	if c.Stats().Evicts == 0 {
		t.Errorf("Stats.Evicts should be > 0")
	}
}

func TestBlockCache_MoveToFrontProtectsRecent(t *testing.T) {
	// Fill cache, touch the oldest, add a new one — the newly-touched
	// entry survives because it just moved to front.
	c := NewBlockCache(60)
	c.Put("f.rf", 0, bytes.Repeat([]byte("a"), 30)) // oldest
	c.Put("f.rf", 1, bytes.Repeat([]byte("b"), 30)) // mid
	// Touch the oldest — moves to front.
	if _, ok := c.Get("f.rf", 0); !ok {
		t.Fatal("warm Get failed")
	}
	c.Put("f.rf", 2, bytes.Repeat([]byte("c"), 30)) // adds; should evict the MID entry now (oldest by LRU order)
	if _, ok := c.Get("f.rf", 0); !ok {
		t.Errorf("entry 0 was just touched and should survive")
	}
	if _, ok := c.Get("f.rf", 1); ok {
		t.Errorf("entry 1 should have been evicted (oldest after touch)")
	}
	if _, ok := c.Get("f.rf", 2); !ok {
		t.Errorf("freshly-inserted entry 2 should be present")
	}
}

func TestBlockCache_DropPath(t *testing.T) {
	c := NewBlockCache(1<<16)
	c.Put("a.rf", 0, []byte("a0"))
	c.Put("a.rf", 1, []byte("a1"))
	c.Put("b.rf", 0, []byte("b0"))
	c.Drop("a.rf")
	if _, ok := c.Get("a.rf", 0); ok {
		t.Errorf("a.rf:0 should be dropped")
	}
	if _, ok := c.Get("a.rf", 1); ok {
		t.Errorf("a.rf:1 should be dropped")
	}
	if _, ok := c.Get("b.rf", 0); !ok {
		t.Errorf("b.rf:0 should remain")
	}
}

func TestBlockCache_ZeroCapacityDisabled(t *testing.T) {
	c := NewBlockCache(0)
	c.Put("f.rf", 0, []byte("x"))
	if _, ok := c.Get("f.rf", 0); ok {
		t.Errorf("zero-capacity cache should not store")
	}
	st := c.Stats()
	if st.Entries != 0 {
		t.Errorf("zero-capacity cache should have 0 entries; got %d", st.Entries)
	}
}

func TestBlockCache_ConcurrentAccess(t *testing.T) {
	c := NewBlockCache(1 << 20)
	var wg sync.WaitGroup
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < 1000; i++ {
				path := fmt.Sprintf("f%d.rf", g%4)
				offset := int64(i % 16)
				if _, ok := c.Get(path, offset); !ok {
					c.Put(path, offset, bytes.Repeat([]byte{byte('a' + g)}, 64))
				}
			}
		}(g)
	}
	wg.Wait()
	// No assertions on contents — race-detector is the test.
	t.Logf("post-stress stats: %+v", c.Stats())
}

func TestBlockCache_Replace(t *testing.T) {
	c := NewBlockCache(100)
	c.Put("f.rf", 0, bytes.Repeat([]byte("a"), 50))
	c.Put("f.rf", 0, bytes.Repeat([]byte("b"), 30))
	v, ok := c.Get("f.rf", 0)
	if !ok || len(v) != 30 || v[0] != 'b' {
		t.Errorf("replaced payload not picked up: ok=%v v=%v", ok, v)
	}
	if c.Stats().BytesUsed != 30 {
		t.Errorf("BytesUsed = %d; want 30 (old 50 was replaced)", c.Stats().BytesUsed)
	}
}
