package mincauthority

import (
	"context"
	"errors"
	"testing"

	"github.com/phrocker/shoal-oss/internal/ingestrouter"
	"github.com/phrocker/shoal-oss/internal/tserver"
)

func TestHostedOwnerVerifierRejectsUnloadAndReplacement(t *testing.T) {
	host := tserver.NewHost()
	server := tserver.LockID{UUID: "11111111-1111-1111-1111-111111111111", Sequence: 2}
	manager := tserver.LockID{UUID: "22222222-2222-2222-2222-222222222222", Sequence: 4}
	if err := host.AdoptLock(server); err != nil {
		t.Fatal(err)
	}
	if err := host.ObserveManagerLock(manager); err != nil {
		t.Fatal(err)
	}
	extent := tserver.Extent{TableID: "5", PrevEndRow: []byte("a"), EndRow: []byte("z")}
	fence := tserver.Fence{Server: server, Manager: manager}
	attempt, err := host.Assign(fence, extent)
	if err != nil {
		t.Fatal(err)
	}
	if err := host.LoadComplete(attempt); err != nil {
		t.Fatal(err)
	}
	requestFence := ingestrouter.Fence{
		ServerGeneration: server.String(), ManagerGeneration: manager.String(), Assignment: attempt.Assignment(),
	}
	verifier := HostedOwnerVerifier{Host: host, Fence: fence, Attempt: attempt}
	if err := verifier.Verify(context.Background(), requestFence); err != nil {
		t.Fatal(err)
	}
	if _, err := host.Unassign(fence, extent, tserver.UnloadGraceful); err != nil {
		t.Fatal(err)
	}
	if err := verifier.Verify(context.Background(), requestFence); !errors.Is(err, ErrStaleOwner) {
		t.Fatalf("got %v, want stale owner", err)
	}
}
