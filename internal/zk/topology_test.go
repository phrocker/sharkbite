package zk

import (
	"context"
	"errors"
	"fmt"
	"path"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	gozk "github.com/go-zookeeper/zk"
)

// topologyLocator serves a fixed ZooKeeper tree: children by path and raw
// data by path. Missing paths behave like ZooKeeper's ErrNoNode.
type topologyLocator struct {
	children map[string][]string
	data     map[string][]byte
	failOn   string
	reads    []string
}

func (l *topologyLocator) InstancePath() string { return "/accumulo/uuid-1" }

func (l *topologyLocator) GetRaw(_ context.Context, p string) ([]byte, error) {
	l.reads = append(l.reads, p)
	if l.failOn == p {
		return nil, errors.New("transport failure")
	}
	data, ok := l.data[p]
	if !ok {
		return nil, fmt.Errorf("get %s: %w", p, gozk.ErrNoNode)
	}
	return data, nil
}

func (l *topologyLocator) Children(_ context.Context, p string) ([]string, error) {
	if l.failOn == p {
		return nil, errors.New("transport failure")
	}
	children, ok := l.children[p]
	if !ok {
		return nil, fmt.Errorf("children %s: %w", p, gozk.ErrNoNode)
	}
	return children, nil
}

type cancelAfterMissingLockLocator struct {
	children        map[string][]string
	data            map[string][]byte
	cancel          context.CancelFunc
	missingLockPath string
}

func (l *cancelAfterMissingLockLocator) InstancePath() string { return "/accumulo/uuid-1" }

func (l *cancelAfterMissingLockLocator) GetRaw(ctx context.Context, p string) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if p == l.missingLockPath {
		l.cancel()
		return nil, fmt.Errorf("get %s: %w", p, gozk.ErrNoNode)
	}
	data, ok := l.data[p]
	if !ok {
		return nil, fmt.Errorf("get %s: %w", p, gozk.ErrNoNode)
	}
	return data, nil
}

func (l *cancelAfterMissingLockLocator) Children(ctx context.Context, p string) ([]string, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	children, ok := l.children[p]
	if !ok {
		return nil, fmt.Errorf("children %s: %w", p, gozk.ErrNoNode)
	}
	return children, nil
}

func lockNode(sequence string) string {
	return "zlock#f50c7911-a203-4e3d-b006-bdb30848f5bd#" + sequence
}

func managerLockData(addresses ...string) []byte {
	descriptors := make([]string, 0, len(addresses)+1)
	descriptors = append(descriptors, `{"uuid":"1","service":"FATE_CLIENT","address":"manager:9999"}`)
	for _, address := range addresses {
		descriptors = append(descriptors,
			fmt.Sprintf(`{"uuid":"1","service":"MANAGER","address":%q}`, address))
	}
	return []byte(`{"descriptors":[` + strings.Join(descriptors, ",") + `]}`)
}

func clientLockData(address string) []byte {
	return []byte(fmt.Sprintf(
		`{"descriptors":[{"uuid":"1","service":"TABLET_SCAN","address":"ignored:1"},`+
			`{"uuid":"1","service":"CLIENT","address":%q}]}`, address))
}

func TestManagerAddressesOrdersByLockSequenceAndDeduplicates(t *testing.T) {
	lockPath := "/accumulo/uuid-1/managers/lock"
	locator := &topologyLocator{
		children: map[string][]string{
			lockPath: {
				"not-a-lock",
				lockNode("0000000003"),
				lockNode("0000000001"),
				lockNode("0000000002"),
			},
		},
		data: map[string][]byte{
			path.Join(lockPath, lockNode("0000000001")): managerLockData("manager-a:9997", "manager-a:9997"),
			path.Join(lockPath, lockNode("0000000002")): managerLockData("manager-b:9997"),
			// 0000000003 is missing: a candidate that dropped its lock.
		},
	}

	got, err := ManagerAddresses(context.Background(), locator)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"manager-a:9997", "manager-b:9997"}
	if len(got) != len(want) {
		t.Fatalf("addresses = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("addresses = %v, want %v", got, want)
		}
	}
	if first, err := ManagerAddress(context.Background(), locator); err != nil || first != want[0] {
		t.Fatalf("ManagerAddress = %q, %v; want %q", first, err, want[0])
	}
}

func TestManagerAddressesSkipsQueuedBootstrapDescriptors(t *testing.T) {
	lockPath := "/accumulo/uuid-1/managers/lock"
	locator := &topologyLocator{
		children: map[string][]string{lockPath: {lockNode("0000000001"), lockNode("0000000002")}},
		data: map[string][]byte{
			path.Join(lockPath, lockNode("0000000001")): managerLockData("manager-a:9997"),
			path.Join(lockPath, lockNode("0000000002")): managerLockData("0.0.0.0:0", ""),
		},
	}
	got, err := ManagerAddresses(context.Background(), locator)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0] != "manager-a:9997" {
		t.Fatalf("addresses = %v, want [manager-a:9997]", got)
	}
}

func TestManagerAddressesDoesNotPromoteQueuedCandidateOverPlaceholderHolder(t *testing.T) {
	lockPath := "/accumulo/uuid-1/managers/lock"
	locator := &topologyLocator{
		children: map[string][]string{lockPath: {lockNode("0000000001"), lockNode("0000000002")}},
		data: map[string][]byte{
			path.Join(lockPath, lockNode("0000000001")): managerLockData("0.0.0.0:0"),
			path.Join(lockPath, lockNode("0000000002")): managerLockData("manager-b:9997"),
		},
	}
	if _, err := ManagerAddresses(context.Background(), locator); !errors.Is(err, ErrManagerUnavailable) {
		t.Fatalf("error = %v, want ErrManagerUnavailable", err)
	}
}

func TestManagerAddressesUnavailable(t *testing.T) {
	lockPath := "/accumulo/uuid-1/managers/lock"
	for name, locator := range map[string]*topologyLocator{
		"no lock path": {children: map[string][]string{}},
		"no valid lock nodes": {
			children: map[string][]string{lockPath: {"not-a-lock"}},
		},
		"lock without manager descriptor": {
			children: map[string][]string{lockPath: {lockNode("0000000001")}},
			data: map[string][]byte{
				path.Join(lockPath, lockNode("0000000001")): []byte(`{"descriptors":[]}`),
			},
		},
		"every lock node vanished": {
			children: map[string][]string{lockPath: {lockNode("0000000001")}},
			data:     map[string][]byte{},
		},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := ManagerAddresses(context.Background(), locator); !errors.Is(err, ErrManagerUnavailable) {
				t.Fatalf("error = %v, want ErrManagerUnavailable", err)
			}
		})
	}
}

func TestManagerAddressesPropagatesFailuresAndCancellation(t *testing.T) {
	lockPath := "/accumulo/uuid-1/managers/lock"

	listFailure := &topologyLocator{failOn: lockPath}
	if _, err := ManagerAddresses(context.Background(), listFailure); err == nil ||
		errors.Is(err, ErrManagerUnavailable) {
		t.Fatalf("error = %v, want a transport failure", err)
	}

	nodePath := path.Join(lockPath, lockNode("0000000001"))
	readFailure := &topologyLocator{
		children: map[string][]string{lockPath: {lockNode("0000000001")}},
		data:     map[string][]byte{nodePath: managerLockData("manager:9997")},
		failOn:   nodePath,
	}
	if _, err := ManagerAddresses(context.Background(), readFailure); err == nil ||
		errors.Is(err, ErrManagerUnavailable) {
		t.Fatalf("error = %v, want a transport failure", err)
	}

	decodeFailure := &topologyLocator{
		children: map[string][]string{lockPath: {lockNode("0000000001")}},
		data:     map[string][]byte{nodePath: []byte("not json")},
	}
	if _, err := ManagerAddresses(context.Background(), decodeFailure); err == nil ||
		errors.Is(err, ErrManagerUnavailable) {
		t.Fatalf("error = %v, want a decode failure", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := ManagerAddresses(ctx, &topologyLocator{}); !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want context.Canceled", err)
	}
	if _, err := ManagerAddresses(context.Background(), nil); err == nil {
		t.Fatal("nil locator accepted")
	}
}

func TestManagerAddressesUsesSingleScopedConnection(t *testing.T) {
	lockPath := "/accumulo/uuid-1/managers/lock"
	var connects atomic.Int32
	var closes atomic.Int32
	locator := &Locator{
		instanceID: "uuid-1",
		rawConnFactory: func() (rawZKConn, error) {
			connects.Add(1)
			return &staticRawTopologyConn{
				children: map[string][]string{
					lockPath: {lockNode("0000000002"), lockNode("0000000001")},
				},
				data: map[string][]byte{
					path.Join(lockPath, lockNode("0000000001")): managerLockData("manager-a:9997"),
					path.Join(lockPath, lockNode("0000000002")): managerLockData("manager-b:9997"),
				},
				closes: &closes,
			}, nil
		},
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	got, err := ManagerAddresses(ctx, locator)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"manager-a:9997", "manager-b:9997"}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("addresses = %v, want %v", got, want)
	}
	if connects.Load() != 1 || closes.Load() != 1 {
		t.Fatalf("connects/closes = %d/%d, want 1/1", connects.Load(), closes.Load())
	}
}

func clientServiceTree() *topologyLocator {
	root := "/accumulo/uuid-1"
	return &topologyLocator{
		children: map[string][]string{
			path.Join(root, "tservers"):                                  {"default", "ingest"},
			path.Join(root, "tservers", "default"):                       {"tserver-a:9997", "tserver-b:9997"},
			path.Join(root, "tservers", "default", "tserver-a:9997"):     {lockNode("0000000001")},
			path.Join(root, "tservers", "default", "tserver-b:9997"):     {"not-a-lock"},
			path.Join(root, "tservers", "ingest"):                        {"tserver-c:9997"},
			path.Join(root, "tservers", "ingest", "tserver-c:9997"):      {lockNode("0000000002")},
			path.Join(root, "sservers"):                                  {"query"},
			path.Join(root, "sservers", "query"):                         {"sserver-a:9996"},
			path.Join(root, "sservers", "query", "sserver-a:9996"):       {lockNode("0000000001")},
			path.Join(root, "compactors"):                                {"default"},
			path.Join(root, "compactors", "default"):                     {"compactor-a:9995"},
			path.Join(root, "compactors", "default", "compactor-a:9995"): {lockNode("0000000001")},
		},
		data: map[string][]byte{
			path.Join(root, "tservers", "default", "tserver-a:9997", lockNode("0000000001")):     clientLockData("tserver-a:9997"),
			path.Join(root, "tservers", "ingest", "tserver-c:9997", lockNode("0000000002")):      clientLockData("tserver-c:9997"),
			path.Join(root, "sservers", "query", "sserver-a:9996", lockNode("0000000001")):       clientLockData("sserver-a:9996"),
			path.Join(root, "compactors", "default", "compactor-a:9995", lockNode("0000000001")): clientLockData("compactor-a:9995"),
		},
	}
}

func TestClientServicesReportsKindGroupAndOrder(t *testing.T) {
	got, err := ClientServices(context.Background(), clientServiceTree())
	if err != nil {
		t.Fatal(err)
	}
	want := []ClientService{
		{Kind: TabletServerKind, Group: "default", Address: "tserver-a:9997"},
		{Kind: TabletServerKind, Group: "ingest", Address: "tserver-c:9997"},
		{Kind: ScanServerKind, Group: "query", Address: "sserver-a:9996"},
		{Kind: CompactorKind, Group: "default", Address: "compactor-a:9995"},
	}
	if len(got) != len(want) {
		t.Fatalf("services = %+v, want %+v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("services[%d] = %+v, want %+v", i, got[i], want[i])
		}
	}
}

func TestClientServicesOrdersByServerIdentity(t *testing.T) {
	root := "/accumulo/uuid-1"
	locator := &topologyLocator{
		children: map[string][]string{
			path.Join(root, "tservers"):                        {"default"},
			path.Join(root, "tservers", "default"):             {"server-b", "server-a"},
			path.Join(root, "tservers", "default", "server-b"): {lockNode("0000000001")},
			path.Join(root, "tservers", "default", "server-a"): {lockNode("0000000002")},
		},
		data: map[string][]byte{
			path.Join(root, "tservers", "default", "server-b", lockNode("0000000001")): clientLockData("alpha:9997"),
			path.Join(root, "tservers", "default", "server-a", lockNode("0000000002")): clientLockData("zeta:9997"),
		},
	}
	got, err := ClientServices(context.Background(), locator)
	if err != nil {
		t.Fatal(err)
	}
	want := []ClientService{
		{Kind: TabletServerKind, Group: "default", Address: "zeta:9997"},
		{Kind: TabletServerKind, Group: "default", Address: "alpha:9997"},
	}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("services = %+v, want %+v", got, want)
	}
}

func TestClientServiceAddressesMatchesClientServices(t *testing.T) {
	locator := clientServiceTree()
	addresses, err := ClientServiceAddresses(context.Background(), locator)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"compactor-a:9995", "sserver-a:9996", "tserver-a:9997", "tserver-c:9997"}
	if len(addresses) != len(want) {
		t.Fatalf("addresses = %v, want %v", addresses, want)
	}
	for i := range want {
		if addresses[i] != want[i] {
			t.Fatalf("addresses = %v, want %v", addresses, want)
		}
	}
}

func TestClientServicesDeduplicatesWithinAGroup(t *testing.T) {
	root := "/accumulo/uuid-1"
	locator := &topologyLocator{
		children: map[string][]string{
			path.Join(root, "tservers"):                              {"default"},
			path.Join(root, "tservers", "default"):                   {"tserver-a:9997", "tserver-b:9997"},
			path.Join(root, "tservers", "default", "tserver-a:9997"): {lockNode("0000000001")},
			path.Join(root, "tservers", "default", "tserver-b:9997"): {lockNode("0000000002")},
		},
		data: map[string][]byte{
			path.Join(root, "tservers", "default", "tserver-a:9997", lockNode("0000000001")): clientLockData("shared:9997"),
			path.Join(root, "tservers", "default", "tserver-b:9997", lockNode("0000000002")): clientLockData("shared:9997"),
		},
	}
	got, err := ClientServices(context.Background(), locator)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Address != "shared:9997" {
		t.Fatalf("services = %+v, want one shared:9997 entry", got)
	}
}

func TestClientServicesUnavailableAndFailures(t *testing.T) {
	if _, err := ClientServices(context.Background(), &topologyLocator{}); !errors.Is(err, ErrClientServiceUnavailable) {
		t.Fatalf("error = %v, want ErrClientServiceUnavailable", err)
	}

	root := "/accumulo/uuid-1"
	failing := clientServiceTree()
	failing.failOn = path.Join(root, "tservers", "default")
	if _, err := ClientServices(context.Background(), failing); err == nil ||
		errors.Is(err, ErrClientServiceUnavailable) {
		t.Fatalf("error = %v, want a transport failure", err)
	}

	decodeFailure := clientServiceTree()
	decodeFailure.data[path.Join(root, "tservers", "default", "tserver-a:9997", lockNode("0000000001"))] = []byte("not json")
	if _, err := ClientServices(context.Background(), decodeFailure); err == nil ||
		errors.Is(err, ErrClientServiceUnavailable) {
		t.Fatalf("error = %v, want a decode failure", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := ClientServices(ctx, clientServiceTree()); !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want context.Canceled", err)
	}
	if _, err := ClientServices(context.Background(), nil); err == nil {
		t.Fatal("nil locator accepted")
	}
}

func TestClientServicesUsesSingleScopedConnection(t *testing.T) {
	root := "/accumulo/uuid-1"
	var connects atomic.Int32
	var closes atomic.Int32
	locator := &Locator{
		instanceID: "uuid-1",
		rawConnFactory: func() (rawZKConn, error) {
			connects.Add(1)
			return &staticRawTopologyConn{
				children: map[string][]string{
					path.Join(root, "tservers"):                        {"default"},
					path.Join(root, "tservers", "default"):             {"server-b", "server-a"},
					path.Join(root, "tservers", "default", "server-b"): {lockNode("0000000001")},
					path.Join(root, "tservers", "default", "server-a"): {lockNode("0000000002")},
					path.Join(root, "sservers"):                        {"query"},
					path.Join(root, "sservers", "query"):               {"scan"},
					path.Join(root, "sservers", "query", "scan"):       {lockNode("0000000001")},
				},
				data: map[string][]byte{
					path.Join(root, "tservers", "default", "server-b", lockNode("0000000001")): clientLockData("zeta:9997"),
					path.Join(root, "tservers", "default", "server-a", lockNode("0000000002")): clientLockData("alpha:9997"),
					path.Join(root, "sservers", "query", "scan", lockNode("0000000001")):       clientLockData("scan:9996"),
				},
				closes: &closes,
			}, nil
		},
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	got, err := ClientServices(ctx, locator)
	if err != nil {
		t.Fatal(err)
	}
	want := []ClientService{
		{Kind: TabletServerKind, Group: "default", Address: "alpha:9997"},
		{Kind: TabletServerKind, Group: "default", Address: "zeta:9997"},
		{Kind: ScanServerKind, Group: "query", Address: "scan:9996"},
	}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("services = %+v, want %+v", got, want)
	}
	if connects.Load() != 1 || closes.Load() != 1 {
		t.Fatalf("connects/closes = %d/%d, want 1/1", connects.Load(), closes.Load())
	}
}

func TestInstanceRoot(t *testing.T) {
	if got := InstanceRoot("uuid-1"); got != "/accumulo/uuid-1" {
		t.Fatalf("InstanceRoot = %q", got)
	}
	if got := InstanceRoot(""); got != "/accumulo" {
		t.Fatalf("InstanceRoot(\"\") = %q", got)
	}
}

// blockingChildrenConn blocks inside Children until the connection is closed,
// so a caller can prove that cancelling the context releases it.
type blockingChildrenConn struct {
	started chan struct{}
	closed  chan struct{}
	done    chan struct{}
	once    sync.Once
}

func (c *blockingChildrenConn) AddAuth(string, []byte) error { return nil }

func (c *blockingChildrenConn) Get(string) ([]byte, *gozk.Stat, error) {
	return nil, nil, gozk.ErrClosing
}

func (c *blockingChildrenConn) Children(string) ([]string, *gozk.Stat, error) {
	close(c.started)
	<-c.closed
	close(c.done)
	return nil, nil, gozk.ErrClosing
}

func (c *blockingChildrenConn) Close() {
	c.once.Do(func() { close(c.closed) })
}

func TestChildrenWithContextCancelsInFlightReadAndJoinsWorker(t *testing.T) {
	conn := &blockingChildrenConn{
		started: make(chan struct{}),
		closed:  make(chan struct{}),
		done:    make(chan struct{}),
	}
	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() {
		_, err := childrenWithContext(ctx, "/accumulo/uuid-1/managers/lock", "", func() (rawZKConn, error) {
			return conn, nil
		})
		result <- err
	}()
	<-conn.started
	cancel()
	if err := <-result; !errors.Is(err, context.Canceled) {
		t.Fatalf("Children error = %v, want context.Canceled", err)
	}
	select {
	case <-conn.done:
	default:
		t.Fatal("Children returned before its ZooKeeper read worker exited")
	}
}

func TestLocatorChildrenCancelsInFlightRead(t *testing.T) {
	conn := &blockingChildrenConn{
		started: make(chan struct{}),
		closed:  make(chan struct{}),
		done:    make(chan struct{}),
	}
	locator := &Locator{
		instanceID: "uuid-1",
		rawConnFactory: func() (rawZKConn, error) {
			return conn, nil
		},
	}
	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() {
		_, err := locator.Children(ctx, "/accumulo/uuid-1/tservers")
		result <- err
	}()
	<-conn.started
	cancel()
	if err := <-result; !errors.Is(err, context.Canceled) {
		t.Fatalf("Children error = %v, want context.Canceled", err)
	}
	<-conn.done
}

func TestTopologyAccessorsCancelBlockingAddAuth(t *testing.T) {
	tests := []struct {
		name string
		call func(context.Context, *Locator) error
	}{
		{
			name: "RootTabletLocation",
			call: func(ctx context.Context, locator *Locator) error {
				_, err := locator.RootTabletLocation(ctx)
				return err
			},
		},
		{
			name: "ManagerAddresses",
			call: func(ctx context.Context, locator *Locator) error {
				_, err := ManagerAddresses(ctx, locator)
				return err
			},
		},
		{
			name: "ClientServices",
			call: func(ctx context.Context, locator *Locator) error {
				_, err := ClientServices(ctx, locator)
				return err
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			conn := &blockingAuthConn{
				started: make(chan struct{}),
				closed:  make(chan struct{}),
				done:    make(chan struct{}),
			}
			var connects atomic.Int32
			locator := &Locator{
				instanceID:     "uuid-1",
				instanceSecret: "secret",
				rawConnFactory: func() (rawZKConn, error) {
					connects.Add(1)
					return conn, nil
				},
			}
			ctx, cancel := context.WithCancel(context.Background())
			result := make(chan error, 1)
			go func() {
				result <- test.call(ctx, locator)
			}()
			<-conn.started
			cancel()
			if err := <-result; !errors.Is(err, context.Canceled) {
				t.Fatalf("error = %v, want context.Canceled", err)
			}
			select {
			case <-conn.done:
			default:
				t.Fatal("accessor returned before AddAuth worker exited")
			}
			if connects.Load() != 1 {
				t.Fatalf("connects = %d, want 1", connects.Load())
			}
		})
	}
}

func TestManagerAddressesCancelsInFlightWithSingleScopedConnection(t *testing.T) {
	conn := &blockingChildrenConn{
		started: make(chan struct{}),
		closed:  make(chan struct{}),
		done:    make(chan struct{}),
	}
	var connects atomic.Int32
	locator := &Locator{
		instanceID: "uuid-1",
		rawConnFactory: func() (rawZKConn, error) {
			connects.Add(1)
			return conn, nil
		},
	}
	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() {
		_, err := ManagerAddresses(ctx, locator)
		result <- err
	}()
	<-conn.started
	cancel()
	if err := <-result; !errors.Is(err, context.Canceled) {
		t.Fatalf("ManagerAddresses error = %v, want context.Canceled", err)
	}
	<-conn.done
	if connects.Load() != 1 {
		t.Fatalf("connects = %d, want 1", connects.Load())
	}
}

func TestClientServicesCancelsInFlightWithSingleScopedConnection(t *testing.T) {
	conn := &blockingChildrenConn{
		started: make(chan struct{}),
		closed:  make(chan struct{}),
		done:    make(chan struct{}),
	}
	var connects atomic.Int32
	locator := &Locator{
		instanceID: "uuid-1",
		rawConnFactory: func() (rawZKConn, error) {
			connects.Add(1)
			return conn, nil
		},
	}
	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() {
		_, err := ClientServices(ctx, locator)
		result <- err
	}()
	<-conn.started
	cancel()
	if err := <-result; !errors.Is(err, context.Canceled) {
		t.Fatalf("ClientServices error = %v, want context.Canceled", err)
	}
	<-conn.done
	if connects.Load() != 1 {
		t.Fatalf("connects = %d, want 1", connects.Load())
	}
}

func TestLocatorRootTabletLocationCancelsInFlightRead(t *testing.T) {
	conn := &blockingGetConn{
		started: make(chan struct{}),
		closed:  make(chan struct{}),
		done:    make(chan struct{}),
	}
	var connects atomic.Int32
	locator := &Locator{
		instanceID: "uuid-1",
		rawConnFactory: func() (rawZKConn, error) {
			connects.Add(1)
			return conn, nil
		},
	}
	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() {
		_, err := locator.RootTabletLocation(ctx)
		result <- err
	}()
	<-conn.started
	cancel()
	if err := <-result; !errors.Is(err, context.Canceled) {
		t.Fatalf("RootTabletLocation error = %v, want context.Canceled", err)
	}
	<-conn.done
	if connects.Load() != 1 {
		t.Fatalf("connects = %d, want 1", connects.Load())
	}
}

// blockingGetConn blocks inside Get until the connection is closed.
type blockingGetConn struct {
	started chan struct{}
	closed  chan struct{}
	done    chan struct{}
	once    sync.Once
}

func (c *blockingGetConn) AddAuth(string, []byte) error { return nil }

func (c *blockingGetConn) Get(string) ([]byte, *gozk.Stat, error) {
	close(c.started)
	<-c.closed
	close(c.done)
	return nil, nil, gozk.ErrClosing
}

func (c *blockingGetConn) Children(string) ([]string, *gozk.Stat, error) {
	return nil, nil, gozk.ErrClosing
}

func (c *blockingGetConn) Close() {
	c.once.Do(func() { close(c.closed) })
}

type scriptedRawTopologyConn struct {
	children map[string][]string
	get      func(string) ([]byte, error)
	closes   *atomic.Int32
}

func (c *scriptedRawTopologyConn) AddAuth(string, []byte) error { return nil }

func (c *scriptedRawTopologyConn) Get(znodePath string) ([]byte, *gozk.Stat, error) {
	if c.get == nil {
		return nil, nil, fmt.Errorf("get %s: %w", znodePath, gozk.ErrNoNode)
	}
	data, err := c.get(znodePath)
	if err != nil {
		return nil, nil, err
	}
	return data, &gozk.Stat{}, nil
}

func (c *scriptedRawTopologyConn) Children(znodePath string) ([]string, *gozk.Stat, error) {
	children, ok := c.children[znodePath]
	if !ok {
		return nil, nil, fmt.Errorf("children %s: %w", znodePath, gozk.ErrNoNode)
	}
	return append([]string(nil), children...), &gozk.Stat{}, nil
}

func (c *scriptedRawTopologyConn) Close() {
	if c.closes != nil {
		c.closes.Add(1)
	}
}

func TestClientServicesPromotesNextSurvivingServerLock(t *testing.T) {
	root := "/accumulo/uuid-1"
	serverPath := path.Join(root, "tservers", "default", "tserver-a:9997")
	locator := &topologyLocator{
		children: map[string][]string{
			path.Join(root, "tservers"):            {"default"},
			path.Join(root, "tservers", "default"): {"tserver-a:9997"},
			// Both lock nodes are listed, but the lowest-sequence node is
			// released before it can be read: the server handed its lock to
			// the next candidate between the Children call and the read.
			serverPath: {lockNode("0000000001"), lockNode("0000000002")},
		},
		data: map[string][]byte{
			path.Join(serverPath, lockNode("0000000002")): clientLockData("tserver-a:9997"),
		},
	}

	services, err := ClientServices(context.Background(), locator)
	if err != nil {
		t.Fatal(err)
	}
	want := ClientService{Kind: TabletServerKind, Group: "default", Address: "tserver-a:9997"}
	if len(services) != 1 || services[0] != want {
		t.Fatalf("services = %+v, want [%+v]; a lock handoff must not drop the server", services, want)
	}

	addresses, err := ClientServiceAddresses(context.Background(), locator)
	if err != nil {
		t.Fatal(err)
	}
	if len(addresses) != 1 || addresses[0] != "tserver-a:9997" {
		t.Fatalf("addresses = %v, want [tserver-a:9997]", addresses)
	}
}

func TestClientServicesUsesOnlyTheSurvivingHolderLock(t *testing.T) {
	root := "/accumulo/uuid-1"
	serverPath := path.Join(root, "tservers", "default", "tserver-a:9997")
	locator := &topologyLocator{
		children: map[string][]string{
			path.Join(root, "tservers"):            {"default"},
			path.Join(root, "tservers", "default"): {"tserver-a:9997"},
			serverPath:                             {lockNode("0000000001"), lockNode("0000000002")},
		},
		data: map[string][]byte{
			// The holder is authoritative: the queued candidate's stale
			// address must never be reported alongside it.
			path.Join(serverPath, lockNode("0000000001")): clientLockData("tserver-a:9997"),
			path.Join(serverPath, lockNode("0000000002")): clientLockData("stale:9997"),
		},
	}
	services, err := ClientServices(context.Background(), locator)
	if err != nil {
		t.Fatal(err)
	}
	if len(services) != 1 || services[0].Address != "tserver-a:9997" {
		t.Fatalf("services = %+v, want only the holder's address", services)
	}
}

func TestClientServicesSkipsPlaceholderHolderDuringHandoff(t *testing.T) {
	root := "/accumulo/uuid-1"
	serverPath := path.Join(root, "tservers", "default", "tserver-a:9997")
	locator := &topologyLocator{
		children: map[string][]string{
			path.Join(root, "tservers"):            {"default"},
			path.Join(root, "tservers", "default"): {"tserver-a:9997"},
			serverPath:                             {lockNode("0000000001"), lockNode("0000000002")},
		},
		data: map[string][]byte{
			path.Join(serverPath, lockNode("0000000001")): clientLockData("0.0.0.0:0"),
			path.Join(serverPath, lockNode("0000000002")): clientLockData("queued:9997"),
		},
	}
	if _, err := ClientServices(context.Background(), locator); !errors.Is(err, ErrClientServiceUnavailable) {
		t.Fatalf("error = %v, want ErrClientServiceUnavailable", err)
	}
	if _, err := ClientServiceAddresses(context.Background(), locator); !errors.Is(err, ErrClientServiceUnavailable) {
		t.Fatalf("address error = %v, want ErrClientServiceUnavailable", err)
	}
}

func TestClientServicesSkipsServerWithNoSurvivingLock(t *testing.T) {
	root := "/accumulo/uuid-1"
	serverPath := path.Join(root, "tservers", "default", "tserver-a:9997")
	locator := &topologyLocator{
		children: map[string][]string{
			path.Join(root, "tservers"):            {"default"},
			path.Join(root, "tservers", "default"): {"tserver-a:9997"},
			serverPath:                             {lockNode("0000000001"), lockNode("0000000002")},
		},
		data: map[string][]byte{},
	}
	if _, err := ClientServices(context.Background(), locator); !errors.Is(err, ErrClientServiceUnavailable) {
		t.Fatalf("error = %v, want ErrClientServiceUnavailable", err)
	}
	if _, err := ClientServiceAddresses(context.Background(), locator); !errors.Is(err, ErrClientServiceUnavailable) {
		t.Fatalf("address error = %v, want ErrClientServiceUnavailable", err)
	}
}

func TestClientServicesCancellationWinsMidHandoffIteration(t *testing.T) {
	root := "/accumulo/uuid-1"
	serverPath := path.Join(root, "tservers", "default", "tserver-a:9997")
	ctx, cancel := context.WithCancel(context.Background())
	locator := &cancelAfterMissingLockLocator{
		children: map[string][]string{
			path.Join(root, "tservers"):            {"default"},
			path.Join(root, "tservers", "default"): {"tserver-a:9997"},
			serverPath:                             {lockNode("0000000001"), lockNode("0000000002")},
		},
		data: map[string][]byte{
			path.Join(serverPath, lockNode("0000000002")): clientLockData("tserver-a:9997"),
		},
		cancel:          cancel,
		missingLockPath: path.Join(serverPath, lockNode("0000000001")),
	}
	if _, err := ClientServices(ctx, locator); !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want context.Canceled", err)
	}
}

func TestClientServicesHandoffUsesSingleScopedConnection(t *testing.T) {
	root := "/accumulo/uuid-1"
	serverPath := path.Join(root, "tservers", "default", "tserver-a:9997")
	var connects atomic.Int32
	var closes atomic.Int32
	locator := &Locator{
		instanceID: "uuid-1",
		rawConnFactory: func() (rawZKConn, error) {
			connects.Add(1)
			return &scriptedRawTopologyConn{
				children: map[string][]string{
					path.Join(root, "tservers"):            {"default"},
					path.Join(root, "tservers", "default"): {"tserver-a:9997"},
					serverPath:                             {lockNode("0000000001"), lockNode("0000000002")},
				},
				get: func(znodePath string) ([]byte, error) {
					switch znodePath {
					case path.Join(serverPath, lockNode("0000000001")):
						return nil, fmt.Errorf("get %s: %w", znodePath, gozk.ErrNoNode)
					case path.Join(serverPath, lockNode("0000000002")):
						return clientLockData("tserver-a:9997"), nil
					default:
						return nil, fmt.Errorf("get %s: %w", znodePath, gozk.ErrNoNode)
					}
				},
				closes: &closes,
			}, nil
		},
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	services, err := ClientServices(ctx, locator)
	if err != nil {
		t.Fatal(err)
	}
	want := ClientService{Kind: TabletServerKind, Group: "default", Address: "tserver-a:9997"}
	if len(services) != 1 || services[0] != want {
		t.Fatalf("services = %+v, want [%+v]", services, want)
	}
	if connects.Load() != 1 || closes.Load() != 1 {
		t.Fatalf("connects/closes = %d/%d, want 1/1", connects.Load(), closes.Load())
	}
}

func TestLocatorRejectsReadsAfterClose(t *testing.T) {
	factoryCalls := 0
	locator := &Locator{
		instanceID: "uuid-1",
		conn:       nil,
		rawConnFactory: func() (rawZKConn, error) {
			factoryCalls++
			return &countingConn{}, nil
		},
	}
	// Close without a shared connection: mark the locator closed directly,
	// which is what Close does before dropping the session.
	locator.mu.Lock()
	locator.closed = true
	locator.mu.Unlock()

	cancellable, cancel := context.WithCancel(context.Background())
	defer cancel()
	for name, ctx := range map[string]context.Context{
		"background":  context.Background(),
		"cancellable": cancellable,
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := locator.Children(ctx, "/accumulo/uuid-1/tservers"); !errors.Is(err, ErrClosed) {
				t.Fatalf("Children error = %v, want ErrClosed", err)
			}
			if _, err := locator.GetRaw(ctx, "/accumulo/uuid-1/root_tablet"); !errors.Is(err, ErrClosed) {
				t.Fatalf("GetRaw error = %v, want ErrClosed", err)
			}
			if _, err := locator.RootTabletLocation(ctx); !errors.Is(err, ErrClosed) {
				t.Fatalf("RootTabletLocation error = %v, want ErrClosed", err)
			}
			if _, _, err := locator.topologyReadScope(ctx); !errors.Is(err, ErrClosed) {
				t.Fatalf("topologyReadScope error = %v, want ErrClosed", err)
			}
		})
	}
	if factoryCalls != 0 {
		t.Fatalf("closed locator opened %d ZooKeeper connections, want 0", factoryCalls)
	}
}

func TestLocatorCloseReleasesOutstandingScopes(t *testing.T) {
	conn := &countingConn{}
	locator := &Locator{
		instanceID:     "uuid-1",
		conn:           nil,
		rawConnFactory: func() (rawZKConn, error) { return conn, nil },
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	_, release, err := locator.topologyReadScope(ctx)
	if err != nil {
		t.Fatal(err)
	}
	locator.mu.Lock()
	tracked := len(locator.scopes)
	locator.mu.Unlock()
	if tracked != 1 {
		t.Fatalf("tracked scopes = %d, want 1", tracked)
	}

	locator.closeScopes()
	if conn.closes != 1 {
		t.Fatalf("scope connection closed %d times, want 1", conn.closes)
	}
	// Releasing after Close must stay safe and must not resurrect tracking.
	release()
	locator.mu.Lock()
	tracked = len(locator.scopes)
	locator.mu.Unlock()
	if tracked != 0 {
		t.Fatalf("tracked scopes after close = %d, want 0", tracked)
	}
}

func TestLocatorCloseRejectsPendingCancellableReconnect(t *testing.T) {
	tests := []struct {
		name string
		call func(context.Context, *Locator) error
	}{
		{
			name: "GetRaw",
			call: func(ctx context.Context, locator *Locator) error {
				_, err := locator.GetRaw(ctx, "/accumulo/uuid-1/root_tablet")
				return err
			},
		},
		{
			name: "RootTabletLocation",
			call: func(ctx context.Context, locator *Locator) error {
				_, err := locator.RootTabletLocation(ctx)
				return err
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			started := make(chan struct{})
			release := make(chan struct{})
			conn := &countingConn{}
			locator := &Locator{
				instanceID: "uuid-1",
				rawConnFactory: func() (rawZKConn, error) {
					close(started)
					<-release
					return conn, nil
				},
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()

			errCh := make(chan error, 1)
			go func() {
				errCh <- test.call(ctx, locator)
			}()

			<-started
			locator.closeScopes()
			close(release)

			if err := <-errCh; !errors.Is(err, ErrClosed) {
				t.Fatalf("error = %v, want ErrClosed", err)
			}
			if conn.closes != 1 {
				t.Fatalf("connection closed %d times, want 1", conn.closes)
			}
			locator.mu.Lock()
			tracked := len(locator.scopes)
			closed := locator.closed
			locator.mu.Unlock()
			if !closed || tracked != 0 {
				t.Fatalf("closed/tracked = %t/%d, want true/0", closed, tracked)
			}
		})
	}
}

type countingConn struct {
	closes int
}

func (c *countingConn) AddAuth(string, []byte) error { return nil }
func (c *countingConn) Get(string) ([]byte, *gozk.Stat, error) {
	return nil, nil, gozk.ErrNoNode
}
func (c *countingConn) Children(string) ([]string, *gozk.Stat, error) {
	return nil, nil, gozk.ErrNoNode
}
func (c *countingConn) Close() { c.closes++ }
