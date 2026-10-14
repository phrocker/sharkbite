// LocatorCache: per-table tablet-location cache with exception-driven
// invalidation. Mirrors the sharkbite LocatorCache.h pattern: sharkbite
// uses a per-table sorted map keyed by EndRow and invalidates individual
// entries on NotServingTablet / TabletNotFoundException.
//
// Lookups are O(log N) via binary search on EndRow. Population is lazy:
// a miss triggers a TableLocator.LocateTable call; on subsequent
// NotServingTablet errors the caller invokes Invalidate(table, row)
// (or InvalidateTable / InvalidateAll for coarser drops).
//
// The cache is intentionally pure — it knows nothing about Thrift
// exception types. Callers (the scan layer) translate exceptions to
// invalidation calls. That keeps the cache reusable across transports
// and unit-testable without a Thrift stack.
package cache

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"

	"github.com/phrocker/shoal-oss/internal/metadata"
)

// TableLocator pulls the live tablet list for a single table. The walker
// in internal/metadata satisfies this; tests stub it.
type TableLocator interface {
	LocateTable(ctx context.Context, tableID string) ([]metadata.TabletInfo, error)
}

// ErrNoTabletCovers is returned by Locate when, after a populate, no
// tablet's (PrevRow, EndRow] range contains the requested row. Almost
// always indicates a malformed table (split gap) or a row from the wrong
// table — not a stale cache, so the cache layer surfaces it as-is.
var ErrNoTabletCovers = errors.New("no tablet covers row")

// ErrTableNotFound is returned when the metadata source has no tablets for
// the requested table ID.
var ErrTableNotFound = errors.New("table not found")

// LocatorCache caches metadata.TabletInfo per (tableID, row).
// Concurrency: safe for many concurrent readers (RWMutex); a single
// writer holds the lock during populate.
type LocatorCache struct {
	src TableLocator

	mu      sync.RWMutex
	byTable map[string]tabletList // sorted by EndRow ascending (nil/+inf last)
}

// tabletList exists so we can attach methods without poisoning external
// signatures with a slice alias.
type tabletList []metadata.TabletInfo

// New constructs an empty cache backed by src.
func New(src TableLocator) *LocatorCache {
	if src == nil {
		panic("cache.New: nil TableLocator")
	}
	return &LocatorCache{
		src:     src,
		byTable: map[string]tabletList{},
	}
}

// Locate returns the cached TabletInfo whose (PrevRow, EndRow] range
// contains row. On miss, populates the cache for tableID via the
// underlying source and retries. Returns ErrNoTabletCovers if even after
// populating no tablet covers the row.
//
// Caller MUST treat returned TabletInfo as read-only — it shares storage
// with the cache. Mutate at your own peril.
func (c *LocatorCache) Locate(ctx context.Context, tableID string, row []byte) (metadata.TabletInfo, error) {
	if t, ok := c.lookup(tableID, row); ok {
		return t, nil
	}
	if _, err := c.refresh(ctx, tableID); err != nil {
		return metadata.TabletInfo{}, fmt.Errorf("populate cache for table %s: %w", tableID, err)
	}
	if t, ok := c.lookup(tableID, row); ok {
		return t, nil
	}
	return metadata.TabletInfo{}, fmt.Errorf("%w: table=%s row=%q", ErrNoTabletCovers, tableID, row)
}

// Invalidate drops the cached tablet whose range contains row. If no
// such tablet is cached, it's a no-op. Use this on NotServingTablet —
// the next Locate for any row in the affected range will repopulate.
//
// V0 shortcut: invalidating a single tablet drops the whole table from
// the cache. Sharkbite drops just the entry, but doing partial drops
// safely requires us to keep range-by-range invariants intact across
// concurrent populates. Full-table drop is correct and cheap given
// metadata churn is rare; we can sharpen later.
func (c *LocatorCache) Invalidate(tableID string, row []byte) {
	c.mu.Lock()
	defer c.mu.Unlock()
	tabs, ok := c.byTable[tableID]
	if !ok {
		return
	}
	if findContaining(tabs, row) < 0 {
		return // unknown row — nothing to invalidate
	}
	delete(c.byTable, tableID)
}

// InvalidateTable drops every cached tablet for tableID. Use after wide
// metadata events (table re-org, manager recovery, repeated NotServing
// across multiple rows) where tablet boundaries themselves likely moved.
func (c *LocatorCache) InvalidateTable(tableID string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.byTable, tableID)
}

// InvalidateAll clears the entire cache. Reserved for "we don't trust
// anything" events — e.g. cluster restart, ZK session loss.
func (c *LocatorCache) InvalidateAll() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.byTable = map[string]tabletList{}
}

// LocateTable satisfies the TableLocator interface so a LocatorCache
// can be drop-in-replaced for the underlying walker. Cache hit serves
// from the in-memory tablet list; miss triggers a refresh that calls
// the underlying source's LocateTable and stashes the result.
//
// Designed for the scan server's hot path: every scan does a tablet
// lookup, and tablet topology is mostly static. Without this cache
// shoal pays ~30ms per scan in metadata-walk RPCs against tservers
// (root → metadata × N), which makes hedge races unwinnable.
func (c *LocatorCache) LocateTable(ctx context.Context, tableID string) ([]metadata.TabletInfo, error) {
	if tabs := c.Snapshot(tableID); tabs != nil {
		return tabs, nil
	}
	tablets, err := c.refresh(ctx, tableID)
	if err != nil {
		return nil, err
	}
	out := make([]metadata.TabletInfo, len(tablets))
	copy(out, tablets)
	return out, nil
}

// Snapshot returns a defensive copy of the cached tablets for tableID,
// for diagnostics and tests. Callers may mutate the returned slice; the
// cache's internal state is unaffected.
func (c *LocatorCache) Snapshot(tableID string) []metadata.TabletInfo {
	c.mu.RLock()
	defer c.mu.RUnlock()
	tabs := c.byTable[tableID]
	if len(tabs) == 0 {
		return nil
	}
	out := make([]metadata.TabletInfo, len(tabs))
	copy(out, tabs)
	return out
}

func (c *LocatorCache) lookup(tableID string, row []byte) (metadata.TabletInfo, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	tabs, ok := c.byTable[tableID]
	if !ok {
		return metadata.TabletInfo{}, false
	}
	idx := findContaining(tabs, row)
	if idx < 0 {
		return metadata.TabletInfo{}, false
	}
	return tabs[idx], true
}

func (c *LocatorCache) refresh(ctx context.Context, tableID string) (tabletList, error) {
	tablets, err := c.src.LocateTable(ctx, tableID)
	if err != nil {
		return nil, err
	}
	if len(tablets) == 0 {
		// Do not cache absence: a newly-created table must become visible on
		// the next lookup without an explicit invalidation.
		return nil, fmt.Errorf("%w: table=%s", ErrTableNotFound, tableID)
	}
	out := make(tabletList, len(tablets))
	copy(out, tablets)
	sort.SliceStable(out, func(i, j int) bool {
		return endRowLess(out[i].EndRow, out[j].EndRow)
	})
	c.mu.Lock()
	c.byTable[tableID] = out
	c.mu.Unlock()
	return out, nil
}

// endRowLess orders EndRows ascending with nil ("default tablet" /+inf)
// sorted last. Matches sharkbite's tablet-ordering convention.
func endRowLess(a, b []byte) bool {
	switch {
	case a == nil && b == nil:
		return false
	case a == nil:
		return false // a == +inf, can't be less
	case b == nil:
		return true // b == +inf, a always less
	default:
		return bytes.Compare(a, b) < 0
	}
}

// findContaining returns the index of the tablet whose (PrevRow, EndRow]
// range contains row, or -1.
//
// Binary search: find the first tablet with EndRow >= row (treating nil
// EndRow as +inf, i.e. always >=). That tablet's PrevRow then bounds the
// lower side; if row > PrevRow, it's a match.
func findContaining(tablets tabletList, row []byte) int {
	if len(tablets) == 0 {
		return -1
	}
	idx := sort.Search(len(tablets), func(i int) bool {
		end := tablets[i].EndRow
		if end == nil {
			return true // +inf is >= anything
		}
		return bytes.Compare(end, row) >= 0
	})
	if idx == len(tablets) {
		return -1
	}
	t := tablets[idx]
	if t.PrevRow != nil && bytes.Compare(row, t.PrevRow) <= 0 {
		// Row is at-or-before the previous tablet's EndRow ⇒ should be
		// in an earlier tablet, but we landed on this one ⇒ gap. Treat
		// as miss (will trigger refresh).
		return -1
	}
	return idx
}
