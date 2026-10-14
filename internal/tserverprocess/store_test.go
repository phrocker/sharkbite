package tserverprocess

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/phrocker/shoal-oss/internal/ingestrouter"
	"github.com/phrocker/shoal-oss/internal/mincauthority"
	"github.com/phrocker/shoal-oss/internal/tabletloader"
	"github.com/phrocker/shoal-oss/internal/tserver"
	"github.com/phrocker/shoal-oss/internal/tserverrpc"
)

type openerFunc func(context.Context, tabletloader.Specification, tserver.Attempt) (ingestrouter.HostedTablet, error)

func (f openerFunc) Open(
	ctx context.Context,
	spec tabletloader.Specification,
	attempt tserver.Attempt,
) (ingestrouter.HostedTablet, error) {
	return f(ctx, spec, attempt)
}

type storeTestTablet struct {
	extent   ingestrouter.Extent
	fence    ingestrouter.Fence
	closed   bool
	flushed  bool
	closeErr error
	files    []mincauthority.DataFile
}

func (t *storeTestTablet) Extent() ingestrouter.Extent { return t.extent }
func (t *storeTestTablet) Fence() ingestrouter.Fence   { return t.fence }
func (t *storeTestTablet) Authority() ingestrouter.CommitAuthority {
	return ingestrouter.AuthorityAccumuloWAL
}
func (t *storeTestTablet) Commit(context.Context, ingestrouter.CommitRequest) error { return nil }
func (t *storeTestTablet) Close(context.Context) error {
	if t.closeErr != nil {
		return t.closeErr
	}
	t.closed = true
	return nil
}
func (t *storeTestTablet) Flush(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	t.flushed = true
	return nil
}
func (t *storeTestTablet) DataFiles() []mincauthority.DataFile {
	return append([]mincauthority.DataFile(nil), t.files...)
}

type loaderFunc func(context.Context, tserver.Extent) (tabletloader.Specification, error)

func (f loaderFunc) Load(ctx context.Context, extent tserver.Extent) (tabletloader.Specification, error) {
	return f(ctx, extent)
}

func TestStorePublishesOnlySuccessfulLoadsAndUnhostsIdempotently(t *testing.T) {
	extent := tserver.Extent{TableID: "5", EndRow: []byte("m")}
	store, err := NewStore(loaderFunc(func(context.Context, tserver.Extent) (tabletloader.Specification, error) {
		return tabletloader.Specification{
			Extent: extent, Generation: "g", Directory: "t-1", Time: "M0",
			Files: []tabletloader.DataFile{{Path: "file:///a.rf"}},
		}, nil
	}))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.LocateTable(context.Background(), "5"); !errors.Is(err, ErrNotHosted) {
		t.Fatalf("pre-load LocateTable error = %v", err)
	}
	if err := store.Load(context.Background(), extent); err != nil {
		t.Fatal(err)
	}
	tablets, err := store.LocateTable(context.Background(), "5")
	if err != nil || len(tablets) != 1 || tablets[0].Files[0].Path != "file:///a.rf" {
		t.Fatalf("hosted tablets = %#v, %v", tablets, err)
	}
	if err := store.Flush(context.Background(), extent); err != nil {
		t.Fatal(err)
	}
	if err := store.Unload(context.Background(), extent, tserverrpc.UnloadUnassigned); err != nil {
		t.Fatal(err)
	}
	if err := store.Unload(context.Background(), extent, tserverrpc.UnloadUnassigned); !errors.Is(err, tserverrpc.ErrNotServing) {
		t.Fatalf("duplicate unload error = %v", err)
	}
}

func TestStoreRejectsWALAndUnsupportedIteratorBeforePublishing(t *testing.T) {
	extent := tserver.Extent{TableID: "5"}
	tests := []tabletloader.Specification{
		{Extent: extent, Logs: []tabletloader.Log{{UUID: "wal-1"}}},
		{Extent: extent, Properties: []tabletloader.Property{{
			Name: "table.iterator.scan.bad", Value: "10,example.Unknown",
		}}},
		{Extent: extent, Properties: []tabletloader.Property{
			{Name: "table.iterator.scan.vers", Value: "20,org.apache.accumulo.core.iterators.user.VersioningIterator"},
			{Name: "table.iterator.scan.vers.opt.maxVersions", Value: "2"},
		}},
	}
	for _, spec := range tests {
		store, _ := NewStore(loaderFunc(func(context.Context, tserver.Extent) (tabletloader.Specification, error) {
			return spec, nil
		}))
		if err := store.Load(context.Background(), extent); err == nil {
			t.Fatal("unsafe specification unexpectedly loaded")
		}
		if len(store.Hosted()) != 0 {
			t.Fatal("failed load became scan-visible")
		}
	}
}

func TestStoreStaleCanceledLoadCannotReplaceSuccessor(t *testing.T) {
	extent := tserver.Extent{TableID: "5"}
	firstStarted := make(chan struct{})
	releaseFirst := make(chan struct{})
	var mu sync.Mutex
	call := 0
	store, _ := NewStore(loaderFunc(func(context.Context, tserver.Extent) (tabletloader.Specification, error) {
		mu.Lock()
		call++
		n := call
		mu.Unlock()
		if n == 1 {
			close(firstStarted)
			<-releaseFirst
			return tabletloader.Specification{Extent: extent, Directory: "old"}, nil
		}
		return tabletloader.Specification{Extent: extent, Directory: "new"}, nil
	}))

	firstDone := make(chan error, 1)
	go func() { firstDone <- store.Load(context.Background(), extent) }()
	<-firstStarted
	if err := store.Unload(context.Background(), extent, tserverrpc.UnloadUnassigned); !errors.Is(err, tserverrpc.ErrNotServing) {
		t.Fatalf("cancel loading unload = %v", err)
	}
	if err := store.Load(context.Background(), extent); err != nil {
		t.Fatal(err)
	}
	close(releaseFirst)
	if err := <-firstDone; !errors.Is(err, context.Canceled) {
		t.Fatalf("stale load error = %v", err)
	}
	got := store.Hosted()
	if len(got) != 1 || got[0].Directory != "new" {
		t.Fatalf("hosted = %#v", got)
	}
}

func TestStoreLoadFailureRemainsUnhosted(t *testing.T) {
	want := errors.New("metadata unavailable")
	store, _ := NewStore(loaderFunc(func(context.Context, tserver.Extent) (tabletloader.Specification, error) {
		return tabletloader.Specification{}, want
	}))
	if err := store.Load(context.Background(), tserver.Extent{TableID: "5"}); !errors.Is(err, want) {
		t.Fatalf("Load error = %v", err)
	}
	if len(store.Hosted()) != 0 {
		t.Fatal("failed tablet published")
	}
}

func TestWritableStorePublishesOnlyOpenedFencedTablet(t *testing.T) {
	extent := tserver.Extent{TableID: "5", EndRow: []byte("z")}
	host := tserver.NewHost()
	server := tserver.LockID{UUID: uuid.NewString(), Sequence: 1}
	manager := tserver.LockID{UUID: uuid.NewString(), Sequence: 2}
	if err := host.AdoptLock(server); err != nil {
		t.Fatal(err)
	}
	if err := host.ObserveManagerLock(manager); err != nil {
		t.Fatal(err)
	}
	attempt, err := host.Assign(tserver.Fence{Server: server, Manager: manager}, extent)
	if err != nil {
		t.Fatal(err)
	}
	tablet := &storeTestTablet{
		extent: ingestrouter.Extent{TableID: "5", EndRow: []byte("z")},
		fence: ingestrouter.Fence{
			ServerGeneration: server.String(), ManagerGeneration: manager.String(),
			Assignment: attempt.Assignment(),
		},
	}
	store, err := NewWritableStore(
		loaderFunc(func(context.Context, tserver.Extent) (tabletloader.Specification, error) {
			return tabletloader.Specification{
				Extent: extent, Logs: []tabletloader.Log{{UUID: "wal-1", Path: "wal"}},
			}, nil
		}),
		openerFunc(func(context.Context, tabletloader.Specification, tserver.Attempt) (ingestrouter.HostedTablet, error) {
			return tablet, nil
		}),
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.LoadAssigned(context.Background(), extent, attempt); err != nil {
		t.Fatal(err)
	}
	got, err := store.Lookup(context.Background(), tablet.extent)
	if err != nil || got != tablet {
		t.Fatalf("Lookup = %#v, %v", got, err)
	}
	if err := store.Flush(context.Background(), extent); err != nil || !tablet.flushed {
		t.Fatalf("Flush = %v, flushed=%t", err, tablet.flushed)
	}
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	if err := store.Flush(cancelled, extent); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled Flush = %v", err)
	}
	tablet.files = []mincauthority.DataFile{{Path: "rfiles/new.rf", Size: 10, Entries: 1}}
	tablets, err := store.LocateTable(context.Background(), "5")
	if err != nil || len(tablets) != 1 || len(tablets[0].Files) != 1 ||
		tablets[0].Files[0].Path != "rfiles/new.rf" {
		t.Fatalf("runtime files = %#v, %v", tablets, err)
	}
	if err := store.UnloadAssigned(context.Background(), extent, attempt, tserverrpc.UnloadSuspended); !errors.Is(err, tserverrpc.ErrUnsupported) {
		t.Fatalf("suspended unload = %v", err)
	}
	tablet.closeErr = errors.New("transient close")
	if err := store.UnloadAssigned(context.Background(), extent, attempt, tserverrpc.UnloadUnassigned); !errors.Is(err, tablet.closeErr) {
		t.Fatalf("failed unload = %v", err)
	}
	if _, err := store.Lookup(context.Background(), tablet.extent); err != nil {
		t.Fatalf("tablet lost after failed unload: %v", err)
	}
	tablet.closeErr = nil
	if err := store.UnloadAssigned(context.Background(), extent, attempt, tserverrpc.UnloadUnassigned); err != nil {
		t.Fatal(err)
	}
	if !tablet.closed {
		t.Fatal("writable tablet was not closed during unload")
	}
	if _, err := store.Lookup(context.Background(), tablet.extent); !errors.Is(err, ingestrouter.ErrNotHosted) {
		t.Fatalf("post-unload lookup = %v", err)
	}
}
