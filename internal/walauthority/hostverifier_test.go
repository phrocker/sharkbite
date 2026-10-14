package walauthority

import (
	"context"
	"errors"
	"testing"

	"github.com/phrocker/shoal-oss/internal/ingestrouter"
	"github.com/phrocker/shoal-oss/internal/tserver"
)

func TestHostedOwnerVerifierRejectsUnhostedAndLostAssignments(t *testing.T) {
	server := tserver.LockID{UUID: "11111111-1111-4111-8111-111111111111", Sequence: 3}
	manager := tserver.LockID{UUID: "22222222-2222-4222-8222-222222222222", Sequence: 7}
	host := tserver.NewHost()
	if err := host.AdoptLock(server); err != nil {
		t.Fatal(err)
	}
	if err := host.ObserveManagerLock(manager); err != nil {
		t.Fatal(err)
	}
	extent := tserver.Extent{TableID: "5", EndRow: []byte("z")}
	attempt, err := host.Assign(tserver.Fence{Server: server, Manager: manager}, extent)
	if err != nil {
		t.Fatal(err)
	}
	wireFence := ingestrouter.Fence{
		ServerGeneration: server.String(), ManagerGeneration: manager.String(),
		Assignment: attempt.Assignment(),
	}
	verify := HostedOwnerVerifier{
		Host: host, Fence: tserver.Fence{Server: server, Manager: manager}, Attempt: attempt,
	}
	recoveryVerify := AssignedOwnerVerifier(verify)
	if err := recoveryVerify.Verify(context.Background(), wireFence); err != nil {
		t.Fatalf("loading recovery Verify: %v", err)
	}
	if err := verify.Verify(context.Background(), wireFence); !errors.Is(err, tserver.ErrWrongState) {
		t.Fatalf("loading Verify = %v", err)
	}
	if err := host.LoadComplete(attempt); err != nil {
		t.Fatal(err)
	}
	if err := verify.Verify(context.Background(), wireFence); err != nil {
		t.Fatalf("hosted Verify: %v", err)
	}
	host.LoseLock(server)
	if err := verify.Verify(context.Background(), wireFence); err == nil {
		t.Fatal("Verify accepted lost assignment")
	}
}
