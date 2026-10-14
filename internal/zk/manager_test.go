package zk

import (
	"context"
	"errors"
	"fmt"
	"testing"

	gozk "github.com/go-zookeeper/zk"
)

type managerLocator struct {
	children    []string
	childrenErr error
	data        []byte
	dataErr     error
	path        string
}

func (l *managerLocator) InstancePath() string { return "/accumulo/uuid-1" }

func (l *managerLocator) GetRaw(_ context.Context, path string) ([]byte, error) {
	l.path = path
	return l.data, l.dataErr
}

func (l *managerLocator) Children(_ context.Context, path string) ([]string, error) {
	l.path = path
	return l.children, l.childrenErr
}

func TestManagerAddress(t *testing.T) {
	locator := &managerLocator{
		children: []string{
			"not-a-lock",
			"zlock#f50c7911-a203-4e3d-b006-bdb30848f5bd#0000000002",
			"zlock#407b5e9b-9d92-4aaa-bff0-d9ba9be206a6#0000000001",
		},
		data: []byte(`{
		"descriptors": [
			{"uuid":"1","service":"FATE_CLIENT","address":"manager:9999","group":"default"},
			{"uuid":"1","service":"MANAGER","address":"manager:9997","group":"default"}
		]
	}`),
	}
	got, err := ManagerAddress(context.Background(), locator)
	if err != nil {
		t.Fatal(err)
	}
	if got != "manager:9997" {
		t.Fatalf("address = %q, want manager:9997", got)
	}
	if locator.path != "/accumulo/uuid-1/managers/lock/zlock#407b5e9b-9d92-4aaa-bff0-d9ba9be206a6#0000000001" {
		t.Fatalf("path = %q", locator.path)
	}
}

func TestManagerAddressUnavailableAndCancellation(t *testing.T) {
	for _, data := range []string{
		`{"descriptors":[]}`,
		`{"descriptors":[{"service":"MANAGER","address":"0.0.0.0:0"}]}`,
	} {
		_, err := ManagerAddress(context.Background(), &managerLocator{
			children: []string{"zlock#407b5e9b-9d92-4aaa-bff0-d9ba9be206a6#0000000001"},
			data:     []byte(data),
		})
		if !errors.Is(err, ErrManagerUnavailable) {
			t.Fatalf("error = %v, want ErrManagerUnavailable", err)
		}
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := ManagerAddress(ctx, &managerLocator{}); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled error = %v", err)
	}
}

// TestCoordinatorAddress reads the CompactionCoordinator endpoint out of
// the same manager lock Java's ExternalCompactionUtil uses, selecting the
// COORDINATOR descriptor rather than whichever descriptor comes first.
// A real manager publishes MANAGER and COORDINATOR at the same address;
// they differ here only to pin the selection.
func TestCoordinatorAddress(t *testing.T) {
	locator := &managerLocator{
		children: []string{
			"zlock#f50c7911-a203-4e3d-b006-bdb30848f5bd#0000000002",
			"zlock#407b5e9b-9d92-4aaa-bff0-d9ba9be206a6#0000000001",
		},
		data: []byte(`{
		"descriptors": [
			{"uuid":"1","service":"MANAGER","address":"manager:9997","group":"default"},
			{"uuid":"1","service":"COORDINATOR","address":"manager:9998","group":"default"},
			{"uuid":"1","service":"FATE_CLIENT","address":"manager:9999","group":"default"}
		]
	}`),
	}
	got, err := CoordinatorAddress(context.Background(), locator)
	if err != nil {
		t.Fatal(err)
	}
	if got != "manager:9998" {
		t.Fatalf("address = %q, want manager:9998", got)
	}
	// The held lock is the lowest-sequence node, matching ServiceLock.
	if locator.path != "/accumulo/uuid-1/managers/lock/zlock#407b5e9b-9d92-4aaa-bff0-d9ba9be206a6#0000000001" {
		t.Fatalf("path = %q", locator.path)
	}
}

// TestCoordinatorAddressUnavailable covers every state a compactor can
// observe while no manager advertises a coordinator. All of them must
// collapse to ErrCoordinatorUnavailable — and stay distinguishable from
// ErrManagerUnavailable — so the poll loop backs off instead of dialing
// a dead address or exiting.
func TestCoordinatorAddressUnavailable(t *testing.T) {
	tests := []struct {
		name    string
		locator *managerLocator
	}{
		{"no lock znode", &managerLocator{childrenErr: gozk.ErrNoNode}},
		{"no lock holder", &managerLocator{children: []string{"not-a-lock"}}},
		{"lock deleted mid-read", &managerLocator{
			children: []string{"zlock#407b5e9b-9d92-4aaa-bff0-d9ba9be206a6#0000000001"},
			dataErr:  gozk.ErrNoNode,
		}},
		{"bootstrap descriptor only", &managerLocator{
			children: []string{"zlock#407b5e9b-9d92-4aaa-bff0-d9ba9be206a6#0000000001"},
			data:     []byte(`{"descriptors":[{"service":"NONE","address":"0.0.0.0:0"}]}`),
		}},
		{"manager without coordinator", &managerLocator{
			children: []string{"zlock#407b5e9b-9d92-4aaa-bff0-d9ba9be206a6#0000000001"},
			data:     []byte(`{"descriptors":[{"service":"MANAGER","address":"manager:9997"}]}`),
		}},
		{"unbound coordinator address", &managerLocator{
			children: []string{"zlock#407b5e9b-9d92-4aaa-bff0-d9ba9be206a6#0000000001"},
			data:     []byte(`{"descriptors":[{"service":"COORDINATOR","address":"0.0.0.0:0"}]}`),
		}},
		{"empty coordinator address", &managerLocator{
			children: []string{"zlock#407b5e9b-9d92-4aaa-bff0-d9ba9be206a6#0000000001"},
			data:     []byte(`{"descriptors":[{"service":"COORDINATOR","address":""}]}`),
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := CoordinatorAddress(context.Background(), tt.locator)
			if !errors.Is(err, ErrCoordinatorUnavailable) {
				t.Fatalf("error = %v, want ErrCoordinatorUnavailable", err)
			}
			if errors.Is(err, ErrManagerUnavailable) {
				t.Fatal("coordinator sentinel must stay distinct from ErrManagerUnavailable")
			}
		})
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := CoordinatorAddress(ctx, &managerLocator{}); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled error = %v", err)
	}
}

// TestCoordinatorAddressFollowsManagerFailover replays a failover: the
// old primary's lock disappears, briefly nothing is advertised, then the
// new primary publishes its own address. Re-reading picks up the new
// address with no cached state to invalidate.
func TestCoordinatorAddressFollowsManagerFailover(t *testing.T) {
	locator := &managerLocator{
		children: []string{"zlock#407b5e9b-9d92-4aaa-bff0-d9ba9be206a6#0000000001"},
		data:     []byte(`{"descriptors":[{"service":"COORDINATOR","address":"manager-a:9998"}]}`),
	}
	got, err := CoordinatorAddress(context.Background(), locator)
	if err != nil || got != "manager-a:9998" {
		t.Fatalf("first address = %q, %v", got, err)
	}

	// Old primary gone, new one has not published descriptors yet.
	locator.children = nil
	if _, err := CoordinatorAddress(context.Background(), locator); !errors.Is(err, ErrCoordinatorUnavailable) {
		t.Fatalf("failover-window error = %v, want ErrCoordinatorUnavailable", err)
	}

	// New primary took the lock with a fresh sequence number.
	locator.children = []string{"zlock#f50c7911-a203-4e3d-b006-bdb30848f5bd#0000000004"}
	locator.data = []byte(`{"descriptors":[{"service":"COORDINATOR","address":"manager-b:9998"}]}`)
	got, err = CoordinatorAddress(context.Background(), locator)
	if err != nil {
		t.Fatal(err)
	}
	if got != "manager-b:9998" {
		t.Fatalf("address after failover = %q, want manager-b:9998", got)
	}
}

// TestCoordinatorAddressSurfacesZooKeeperFailures keeps real ZooKeeper
// faults out of the "no coordinator yet" bucket so operators see them.
func TestCoordinatorAddressSurfacesZooKeeperFailures(t *testing.T) {
	const lockNode = "zlock#407b5e9b-9d92-4aaa-bff0-d9ba9be206a6#0000000001"
	tests := []struct {
		name    string
		locator *managerLocator
	}{
		{"list failed", &managerLocator{childrenErr: errors.New("connection refused")}},
		{"read failed", &managerLocator{children: []string{lockNode}, dataErr: errors.New("session expired")}},
		{"corrupt lock data", &managerLocator{children: []string{lockNode}, data: []byte("not-json")}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := CoordinatorAddress(context.Background(), tt.locator)
			if err == nil || errors.Is(err, ErrCoordinatorUnavailable) {
				t.Fatalf("error = %v, want a surfaced ZooKeeper failure", err)
			}
		})
	}
}

func TestFirstLockNodeUsesAccumuloSequenceRange(t *testing.T) {
	const uuid = "407b5e9b-9d92-4aaa-bff0-d9ba9be206a6"
	maxSequence := "zlock#" + uuid + "#2147483647"
	if got := firstLockNode([]string{maxSequence}); got != maxSequence {
		t.Fatalf("max Accumulo sequence = %q, want %q", got, maxSequence)
	}
	overflow := "zlock#" + uuid + "#2147483648"
	if got := firstLockNode([]string{overflow}); got != "" {
		t.Fatalf("overflow sequence = %q, want invalid", got)
	}
}

type clientServiceLocator struct {
	children map[string][]string
	data     map[string][]byte
	err      map[string]error
}

func (l *clientServiceLocator) InstancePath() string { return "/accumulo/uuid-1" }

func (l *clientServiceLocator) Children(ctx context.Context, path string) ([]string, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := l.err[path]; err != nil {
		return nil, err
	}
	children, ok := l.children[path]
	if !ok {
		return nil, gozk.ErrNoNode
	}
	return append([]string(nil), children...), nil
}

func (l *clientServiceLocator) GetRaw(ctx context.Context, path string) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := l.err[path]; err != nil {
		return nil, err
	}
	data, ok := l.data[path]
	if !ok {
		return nil, gozk.ErrNoNode
	}
	return append([]byte(nil), data...), nil
}

func TestClientServiceAddressesUsesAllAccumulo4ServerTypes(t *testing.T) {
	const lock = "zlock#407b5e9b-9d92-4aaa-bff0-d9ba9be206a6#0000000001"
	locator := &clientServiceLocator{
		children: map[string][]string{},
		data:     map[string][]byte{},
		err:      map[string]error{},
	}
	servers := []struct {
		root, server, address string
	}{
		{"/tservers", "tablet:9997", "tablet-client:9997"},
		{"/sservers", "scan:9997", "scan-client:9997"},
		{"/compactors", "compactor:9997", "compactor-client:9997"},
	}
	for _, server := range servers {
		root := "/accumulo/uuid-1" + server.root
		group := root + "/default"
		serverPath := group + "/" + server.server
		lockPath := serverPath + "/" + lock
		locator.children[root] = []string{"default"}
		locator.children[group] = []string{server.server}
		locator.children[serverPath] = []string{"invalid", lock}
		locator.data[lockPath] = []byte(fmt.Sprintf(`{
			"descriptors":[
				{"service":"TABLET_SCAN","address":"ignored:9997"},
				{"service":"CLIENT","address":%q}
			]
		}`, server.address))
	}

	got, err := ClientServiceAddresses(context.Background(), locator)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"compactor-client:9997", "scan-client:9997", "tablet-client:9997"}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("addresses = %v, want %v", got, want)
	}
}

func TestClientServiceAddressesUnavailableErrorsAndCancellation(t *testing.T) {
	locator := &clientServiceLocator{
		children: map[string][]string{},
		data:     map[string][]byte{},
		err:      map[string]error{},
	}
	if _, err := ClientServiceAddresses(context.Background(), locator); !errors.Is(err, ErrClientServiceUnavailable) {
		t.Fatalf("empty error = %v, want ErrClientServiceUnavailable", err)
	}

	locator.err["/accumulo/uuid-1/tservers"] = errors.New("zk unavailable")
	if _, err := ClientServiceAddresses(context.Background(), locator); err == nil ||
		errors.Is(err, ErrClientServiceUnavailable) {
		t.Fatalf("zookeeper error = %v, want surfaced discovery failure", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := ClientServiceAddresses(ctx, locator); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled error = %v, want context.Canceled", err)
	}
}
