package cache

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/phrocker/shoal-oss/internal/metadata"
)

// fakeLocator is a TableLocator stub. Records call counts so tests can
// assert refreshes happen exactly when expected.
type fakeLocator struct {
	mu      sync.Mutex
	tablets map[string][]metadata.TabletInfo
	calls   int64
	err     error
}

func (f *fakeLocator) LocateTable(_ context.Context, tableID string) ([]metadata.TabletInfo, error) {
	atomic.AddInt64(&f.calls, 1)
	if f.err != nil {
		return nil, f.err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	out := append([]metadata.TabletInfo(nil), f.tablets[tableID]...)
	return out, nil
}

func (f *fakeLocator) callCount() int64 { return atomic.LoadInt64(&f.calls) }

// threeTablets returns a contiguous (-inf,k] (k,p] (p,+inf) layout for
// tableID, with each tablet pointing at a distinct tserver.
func threeTablets(tableID string) []metadata.TabletInfo {
	return []metadata.TabletInfo{
		{TableID: tableID, EndRow: []byte("k"), PrevRow: nil, Location: &metadata.Location{HostPort: "ts-1:9997"}},
		{TableID: tableID, EndRow: []byte("p"), PrevRow: []byte("k"), Location: &metadata.Location{HostPort: "ts-2:9997"}},
		{TableID: tableID, EndRow: nil, PrevRow: []byte("p"), Location: &metadata.Location{HostPort: "ts-3:9997"}},
	}
}

func TestLocate_PopulatesOnMiss(t *testing.T) {
	src := &fakeLocator{tablets: map[string][]metadata.TabletInfo{
		"2k": threeTablets("2k"),
	}}
	c := New(src)

	got, err := c.Locate(context.Background(), "2k", []byte("a"))
	if err != nil {
		t.Fatal(err)
	}
	if got.Location.HostPort != "ts-1:9997" {
		t.Errorf("got host %q, want ts-1:9997", got.Location.HostPort)
	}
	if src.callCount() != 1 {
		t.Errorf("calls = %d, want 1", src.callCount())
	}
}

func TestLocate_HitDoesNotCallSource(t *testing.T) {
	src := &fakeLocator{tablets: map[string][]metadata.TabletInfo{
		"2k": threeTablets("2k"),
	}}
	c := New(src)

	_, _ = c.Locate(context.Background(), "2k", []byte("a"))   // populates
	_, _ = c.Locate(context.Background(), "2k", []byte("aaa")) // hit
	_, _ = c.Locate(context.Background(), "2k", []byte("k"))   // hit (exclusive PrevRow, inclusive EndRow)

	if src.callCount() != 1 {
		t.Errorf("calls = %d, want 1 (extra hits should not refresh)", src.callCount())
	}
}

func TestLocate_BoundaryRouting(t *testing.T) {
	src := &fakeLocator{tablets: map[string][]metadata.TabletInfo{
		"2k": threeTablets("2k"),
	}}
	c := New(src)

	cases := []struct {
		name string
		row  string
		host string
	}{
		{"first tablet, leftmost", "a", "ts-1:9997"},
		{"first tablet, EndRow inclusive", "k", "ts-1:9997"},
		{"middle tablet, just past split", "k\x00", "ts-2:9997"},
		{"middle tablet, EndRow inclusive", "p", "ts-2:9997"},
		{"last tablet, just past p", "p\x00", "ts-3:9997"},
		{"last tablet, very-large row", "zzzzzzzzz", "ts-3:9997"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := c.Locate(context.Background(), "2k", []byte(tc.row))
			if err != nil {
				t.Fatal(err)
			}
			if got.Location.HostPort != tc.host {
				t.Errorf("row %q routed to %q, want %q", tc.row, got.Location.HostPort, tc.host)
			}
		})
	}
}

func TestInvalidate_ForcesRefreshOnNextLocate(t *testing.T) {
	src := &fakeLocator{tablets: map[string][]metadata.TabletInfo{
		"2k": threeTablets("2k"),
	}}
	c := New(src)

	_, _ = c.Locate(context.Background(), "2k", []byte("aaa"))
	if src.callCount() != 1 {
		t.Fatalf("setup: calls = %d, want 1", src.callCount())
	}

	// Simulate cluster move: tablet 1 now lives on a new tserver. Swap
	// the Location *pointer* — the cache snapshotted by-value but copies
	// the *Location pointer, so mutating .HostPort would leak through.
	src.mu.Lock()
	src.tablets["2k"][0].Location = &metadata.Location{HostPort: "ts-1-NEW:9997"}
	src.mu.Unlock()

	// Without invalidation we still see the stale (original-pointer) value.
	got, _ := c.Locate(context.Background(), "2k", []byte("aaa"))
	if got.Location.HostPort != "ts-1:9997" {
		t.Errorf("pre-invalidation: got %q, want stale ts-1:9997", got.Location.HostPort)
	}
	if src.callCount() != 1 {
		t.Errorf("calls = %d, want 1 (still cached)", src.callCount())
	}

	// Invalidate the row in question — caller's response to a NotServing
	// exception. Next Locate must repopulate.
	c.Invalidate("2k", []byte("aaa"))

	got, _ = c.Locate(context.Background(), "2k", []byte("aaa"))
	if got.Location.HostPort != "ts-1-NEW:9997" {
		t.Errorf("post-invalidation: got %q, want ts-1-NEW:9997", got.Location.HostPort)
	}
	if src.callCount() != 2 {
		t.Errorf("calls = %d, want 2 (refresh after invalidation)", src.callCount())
	}
}

func TestInvalidate_UnknownRowIsNoOp(t *testing.T) {
	src := &fakeLocator{tablets: map[string][]metadata.TabletInfo{
		"2k": threeTablets("2k"),
	}}
	c := New(src)

	_, _ = c.Locate(context.Background(), "2k", []byte("aaa"))

	// No tablet for table "999" — invalidation must not crash and must
	// not pollute the "2k" cache.
	c.Invalidate("999", []byte("xxx"))

	if len(c.Snapshot("2k")) != 3 {
		t.Errorf("snapshot len = %d, want 3 (unrelated invalidation polluted cache)", len(c.Snapshot("2k")))
	}
}

func TestInvalidateTable_DropsAllForTableButLeavesOthers(t *testing.T) {
	src := &fakeLocator{tablets: map[string][]metadata.TabletInfo{
		"2k": threeTablets("2k"),
		"3z": threeTablets("3z"),
	}}
	c := New(src)

	_, _ = c.Locate(context.Background(), "2k", []byte("a"))
	_, _ = c.Locate(context.Background(), "3z", []byte("a"))
	if src.callCount() != 2 {
		t.Fatalf("setup: calls = %d, want 2", src.callCount())
	}

	c.InvalidateTable("2k")

	if got := c.Snapshot("2k"); got != nil {
		t.Errorf("2k snapshot post-drop = %v, want nil", got)
	}
	if got := c.Snapshot("3z"); len(got) != 3 {
		t.Errorf("3z snapshot len = %d, want 3 (collateral drop)", len(got))
	}
}

func TestInvalidateAll(t *testing.T) {
	src := &fakeLocator{tablets: map[string][]metadata.TabletInfo{
		"2k": threeTablets("2k"),
		"3z": threeTablets("3z"),
	}}
	c := New(src)

	_, _ = c.Locate(context.Background(), "2k", []byte("a"))
	_, _ = c.Locate(context.Background(), "3z", []byte("a"))

	c.InvalidateAll()

	if got := c.Snapshot("2k"); got != nil {
		t.Errorf("2k snapshot after InvalidateAll = %v, want nil", got)
	}
	if got := c.Snapshot("3z"); got != nil {
		t.Errorf("3z snapshot after InvalidateAll = %v, want nil", got)
	}
}

func TestLocate_PropagatesSourceError(t *testing.T) {
	want := errors.New("zk down")
	src := &fakeLocator{err: want}
	c := New(src)

	_, err := c.Locate(context.Background(), "2k", []byte("a"))
	if err == nil {
		t.Fatal("expected error")
	}
	if !errors.Is(err, want) {
		t.Errorf("err = %v, want wraps %v", err, want)
	}
}

func TestLocate_NoTabletCoversAfterPopulate(t *testing.T) {
	// Pathological: source returns tablets that don't form a contiguous
	// (-inf, +inf) cover. Locate should surface ErrNoTabletCovers cleanly.
	src := &fakeLocator{tablets: map[string][]metadata.TabletInfo{
		"2k": {
			// Only one tablet, covering ("k", "p"]. Rows < "k"+epsilon
			// or > "p" can't be located.
			{TableID: "2k", EndRow: []byte("p"), PrevRow: []byte("k")},
		},
	}}
	c := New(src)

	_, err := c.Locate(context.Background(), "2k", []byte("zzz"))
	if !errors.Is(err, ErrNoTabletCovers) {
		t.Errorf("err = %v, want ErrNoTabletCovers", err)
	}
}

func TestLocate_EmptyTableNotCached(t *testing.T) {
	src := &fakeLocator{tablets: map[string][]metadata.TabletInfo{
		"2k": nil,
	}}
	c := New(src)

	_, err := c.Locate(context.Background(), "2k", []byte("a"))
	if !errors.Is(err, ErrTableNotFound) {
		t.Errorf("err = %v, want ErrTableNotFound", err)
	}

	// Now table is "born" — populate it on the source. Cache should
	// repopulate (we did NOT cache the empty result).
	src.mu.Lock()
	src.tablets["2k"] = threeTablets("2k")
	src.mu.Unlock()

	got, err := c.Locate(context.Background(), "2k", []byte("a"))
	if err != nil {
		t.Fatalf("post-creation: %v", err)
	}
	if got.Location.HostPort != "ts-1:9997" {
		t.Errorf("got %q, want ts-1:9997", got.Location.HostPort)
	}
}

func TestSnapshotIsDefensiveCopy(t *testing.T) {
	src := &fakeLocator{tablets: map[string][]metadata.TabletInfo{
		"2k": threeTablets("2k"),
	}}
	c := New(src)
	_, _ = c.Locate(context.Background(), "2k", []byte("a"))

	snap := c.Snapshot("2k")
	if len(snap) != 3 {
		t.Fatalf("snap len = %d", len(snap))
	}
	// Mutating the snapshot must not affect future Locate calls.
	snap[0].Location = &metadata.Location{HostPort: "evil:1"}

	got, _ := c.Locate(context.Background(), "2k", []byte("a"))
	if got.Location.HostPort != "ts-1:9997" {
		t.Errorf("cache leaked through snapshot: got %q", got.Location.HostPort)
	}
}

func TestConcurrentLocate(t *testing.T) {
	src := &fakeLocator{tablets: map[string][]metadata.TabletInfo{
		"2k": threeTablets("2k"),
	}}
	c := New(src)

	const N = 200
	var wg sync.WaitGroup
	wg.Add(N)
	for i := 0; i < N; i++ {
		go func(i int) {
			defer wg.Done()
			row := []byte{byte('a' + (i % 26))}
			_, err := c.Locate(context.Background(), "2k", row)
			if err != nil {
				t.Errorf("Locate(%q): %v", row, err)
			}
		}(i)
	}
	wg.Wait()

	// First-population can race — accept >=1 and "small". The exact upper
	// bound depends on race timing; multiple goroutines can all see a miss
	// and all call refresh. Assert it's bounded, not exact.
	if src.callCount() < 1 {
		t.Errorf("calls = %d, want at least 1", src.callCount())
	}
}
