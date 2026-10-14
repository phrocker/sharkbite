package accumulo

import (
	"context"
	"errors"
	"fmt"
	"path"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	gozk "github.com/go-zookeeper/zk"

	"github.com/phrocker/shoal-oss/internal/zk"
)

// topologyLocator is a locator backed by a fixed ZooKeeper tree.
type topologyLocator struct {
	id         string
	root       *zk.Location
	rootErr    error
	children   map[string][]string
	data       map[string][]byte
	closes     atomic.Int64
	rootCalls  atomic.Int64
	childCalls atomic.Int64
}

func (l *topologyLocator) InstanceID() string { return l.id }
func (l *topologyLocator) Close()             { l.closes.Add(1) }

func (l *topologyLocator) RootTabletLocation(context.Context) (*zk.Location, error) {
	l.rootCalls.Add(1)
	return l.root, l.rootErr
}

func (l *topologyLocator) InstancePath() string { return zk.InstanceRoot(l.id) }

func (l *topologyLocator) GetRaw(_ context.Context, p string) ([]byte, error) {
	data, ok := l.data[p]
	if !ok {
		return nil, fmt.Errorf("get %s: %w", p, gozk.ErrNoNode)
	}
	return data, nil
}

func (l *topologyLocator) Children(_ context.Context, p string) ([]string, error) {
	l.childCalls.Add(1)
	children, ok := l.children[p]
	if !ok {
		return nil, fmt.Errorf("children %s: %w", p, gozk.ErrNoNode)
	}
	return children, nil
}

const topologyLockNode = "zlock#f50c7911-a203-4e3d-b006-bdb30848f5bd#0000000001"

func topologyLockNodeAt(sequence string) string {
	return "zlock#f50c7911-a203-4e3d-b006-bdb30848f5bd#" + sequence
}

func managerLock(addresses ...string) []byte {
	descriptors := make([]string, 0, len(addresses))
	for _, address := range addresses {
		descriptors = append(descriptors,
			fmt.Sprintf(`{"uuid":"1","service":"MANAGER","address":%q}`, address))
	}
	return []byte(`{"descriptors":[` + strings.Join(descriptors, ",") + `]}`)
}

func clientLock(address string) []byte {
	return []byte(fmt.Sprintf(
		`{"descriptors":[{"uuid":"1","service":"CLIENT","address":%q}]}`, address))
}

func newTopologyInstance(t *testing.T, tree *topologyLocator, cfg ZooKeeperConfig) Instance {
	t.Helper()
	if cfg.InstanceName == "" {
		cfg.InstanceName = "accumulo"
	}
	if len(cfg.Servers) == 0 {
		cfg.Servers = []string{"zk-a:2181", "zk-b:2181"}
	}
	instance, err := newZooKeeperInstance(
		context.Background(),
		cfg,
		func(ZooKeeperConfig) (locator, error) { return tree, nil },
	)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = instance.Close() })
	return instance
}

func topologyTree(id string) *topologyLocator {
	root := zk.InstanceRoot(id)
	managerLockPath := path.Join(root, "managers", "lock")
	return &topologyLocator{
		id:   id,
		root: &zk.Location{HostPort: "tserver-a:9997", Session: "lock-1"},
		children: map[string][]string{
			managerLockPath:                                              {topologyLockNodeAt("0000000001"), topologyLockNodeAt("0000000002")},
			path.Join(root, "tservers"):                                  {"default"},
			path.Join(root, "tservers", "default"):                       {"tserver-a:9997"},
			path.Join(root, "tservers", "default", "tserver-a:9997"):     {topologyLockNode},
			path.Join(root, "sservers"):                                  {"query"},
			path.Join(root, "sservers", "query"):                         {"sserver-a:9996"},
			path.Join(root, "sservers", "query", "sserver-a:9996"):       {topologyLockNode},
			path.Join(root, "compactors"):                                {"default"},
			path.Join(root, "compactors", "default"):                     {"compactor-a:9995"},
			path.Join(root, "compactors", "default", "compactor-a:9995"): {topologyLockNode},
		},
		data: map[string][]byte{
			path.Join(managerLockPath, topologyLockNodeAt("0000000001")):                   managerLock("manager-a:9997"),
			path.Join(managerLockPath, topologyLockNodeAt("0000000002")):                   managerLock("manager-b:9997"),
			path.Join(root, "tservers", "default", "tserver-a:9997", topologyLockNode):     clientLock("tserver-a:9997"),
			path.Join(root, "sservers", "query", "sserver-a:9996", topologyLockNode):       clientLock("sserver-a:9996"),
			path.Join(root, "compactors", "default", "compactor-a:9995", topologyLockNode): clientLock("compactor-a:9995"),
		},
	}
}

func TestInstanceRootTabletLocation(t *testing.T) {
	locator := topologyTree("uuid-1")
	instance := newTopologyInstance(t, locator, ZooKeeperConfig{})

	location, err := instance.RootTabletLocation(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if location.HostPort != "tserver-a:9997" || location.Session != "lock-1" {
		t.Fatalf("location = %+v", location)
	}
	if got := location.String(); got != "tserver-a:9997" {
		t.Fatalf("String = %q", got)
	}
	if locator.rootCalls.Load() != 1 {
		t.Fatalf("root tablet resolved %d times, want 1 (no caching)", locator.rootCalls.Load())
	}
	if _, err := instance.RootTabletLocation(context.Background()); err != nil {
		t.Fatal(err)
	}
	if locator.rootCalls.Load() != 2 {
		t.Fatalf("root tablet resolved %d times, want 2", locator.rootCalls.Load())
	}
}

func TestInstanceRootTabletLocationUnassignedAndFailures(t *testing.T) {
	unassigned := topologyTree("uuid-1")
	unassigned.root = nil
	instance := newTopologyInstance(t, unassigned, ZooKeeperConfig{})
	if _, err := instance.RootTabletLocation(context.Background()); !errors.Is(err, ErrTabletNotLocated) {
		t.Fatalf("error = %v, want ErrTabletNotLocated", err)
	}

	empty := topologyTree("uuid-1")
	empty.root = &zk.Location{}
	emptyInstance := newTopologyInstance(t, empty, ZooKeeperConfig{})
	if _, err := emptyInstance.RootTabletLocation(context.Background()); !errors.Is(err, ErrTabletNotLocated) {
		t.Fatalf("error = %v, want ErrTabletNotLocated", err)
	}

	failing := topologyTree("uuid-1")
	failing.rootErr = errors.New("transport failure")
	failingInstance := newTopologyInstance(t, failing, ZooKeeperConfig{})
	if _, resolveErr := failingInstance.RootTabletLocation(context.Background()); resolveErr == nil ||
		errors.Is(resolveErr, ErrTabletNotLocated) {
		t.Fatalf("error = %v, want the transport failure", resolveErr)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := instance.RootTabletLocation(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want context.Canceled", err)
	}
}

func TestInstanceManagerLocations(t *testing.T) {
	locator := topologyTree("uuid-1")
	instance := newTopologyInstance(t, locator, ZooKeeperConfig{})

	got, err := instance.ManagerLocations(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"manager-a:9997", "manager-b:9997"}
	if len(got) != len(want) {
		t.Fatalf("locations = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("locations = %v, want %v (active manager first)", got, want)
		}
	}

	unavailable := topologyTree("uuid-1")
	delete(unavailable.children, path.Join(zk.InstanceRoot("uuid-1"), "managers", "lock"))
	unavailableInstance := newTopologyInstance(t, unavailable, ZooKeeperConfig{})
	if _, err := unavailableInstance.ManagerLocations(context.Background()); !errors.Is(err, ErrManagerUnavailable) {
		t.Fatalf("error = %v, want ErrManagerUnavailable", err)
	}

	corrupt := topologyTree("uuid-1")
	corrupt.data[path.Join(zk.InstanceRoot("uuid-1"), "managers", "lock", topologyLockNodeAt("0000000001"))] = []byte("not json")
	corruptInstance := newTopologyInstance(t, corrupt, ZooKeeperConfig{})
	if _, err := corruptInstance.ManagerLocations(context.Background()); err == nil ||
		errors.Is(err, ErrManagerUnavailable) {
		t.Fatalf("error = %v, want a decode failure", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := instance.ManagerLocations(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want context.Canceled", err)
	}
}

func TestInstanceServers(t *testing.T) {
	locator := topologyTree("uuid-1")
	instance := newTopologyInstance(t, locator, ZooKeeperConfig{})

	servers, err := instance.Servers(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	want := []ServerConnection{
		{Kind: TabletServerKind, Group: "default", Host: "tserver-a", Port: 9997},
		{Kind: ScanServerKind, Group: "query", Host: "sserver-a", Port: 9996},
		{Kind: CompactorKind, Group: "default", Host: "compactor-a", Port: 9995},
	}
	if len(servers) != len(want) {
		t.Fatalf("servers = %+v, want %+v", servers, want)
	}
	for i := range want {
		if servers[i] != want[i] {
			t.Fatalf("servers[%d] = %+v, want %+v", i, servers[i], want[i])
		}
	}
	if got := servers[0].HostPort(); got != "tserver-a:9997" {
		t.Fatalf("HostPort = %q", got)
	}
	if got := servers[0].String(); got != "tserver default tserver-a:9997" {
		t.Fatalf("String = %q", got)
	}

	unavailable := &topologyLocator{id: "uuid-1"}
	unavailableInstance := newTopologyInstance(t, unavailable, ZooKeeperConfig{})
	if _, err := unavailableInstance.Servers(context.Background()); !errors.Is(err, ErrClientServiceUnavailable) {
		t.Fatalf("error = %v, want ErrClientServiceUnavailable", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := instance.Servers(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want context.Canceled", err)
	}
}

func TestInstanceServersRejectsMalformedAddresses(t *testing.T) {
	root := zk.InstanceRoot("uuid-1")
	for name, address := range map[string]string{
		"no port":      "tserver-a",
		"bad port":     "tserver-a:port",
		"port too big": "tserver-a:70000",
		"no host":      ":9997",
		"port zero":    "tserver-a:0",
	} {
		t.Run(name, func(t *testing.T) {
			locator := &topologyLocator{
				id: "uuid-1",
				children: map[string][]string{
					path.Join(root, "tservers"):                      {"default"},
					path.Join(root, "tservers", "default"):           {"server"},
					path.Join(root, "tservers", "default", "server"): {topologyLockNode},
				},
				data: map[string][]byte{
					path.Join(root, "tservers", "default", "server", topologyLockNode): clientLock(address),
				},
			}
			instance := newTopologyInstance(t, locator, ZooKeeperConfig{})
			_, err := instance.Servers(context.Background())
			if err == nil || errors.Is(err, ErrClientServiceUnavailable) {
				t.Fatalf("error = %v, want a parse failure", err)
			}
			if !strings.Contains(err.Error(), address) {
				t.Fatalf("error %v does not name the address %q", err, address)
			}
		})
	}
}

func TestInstanceZooKeepersRootAndConfiguration(t *testing.T) {
	config := NewConfiguration()
	config.Set("FILE_SYSTEM_ROOT", "/accumulo")
	servers := []string{"zk-a:2181", "zk-b:2181"}
	locator := topologyTree("uuid-1")
	instance := newTopologyInstance(t, locator, ZooKeeperConfig{
		Servers:       servers,
		Configuration: config,
	})

	got := instance.ZooKeepers()
	if len(got) != 2 || got[0] != "zk-a:2181" || got[1] != "zk-b:2181" {
		t.Fatalf("ZooKeepers = %v", got)
	}
	got[0] = "mutated"
	if again := instance.ZooKeepers(); again[0] != "zk-a:2181" {
		t.Fatalf("ZooKeepers returned an aliased slice: %v", again)
	}
	servers[1] = "mutated"
	if again := instance.ZooKeepers(); again[1] != "zk-b:2181" {
		t.Fatalf("ZooKeepers aliased the caller's slice: %v", again)
	}

	if root := instance.Root(); root != "/accumulo/uuid-1" {
		t.Fatalf("Root = %q", root)
	}

	instanceConfig := instance.Configuration()
	if instanceConfig == nil {
		t.Fatal("Configuration is nil")
	}
	if value := instanceConfig.Get("FILE_SYSTEM_ROOT"); value != "/accumulo" {
		t.Fatalf("Configuration lost a value: %q", value)
	}
	config.Set("FILE_SYSTEM_ROOT", "/mutated")
	config.Set("added", "value")
	if value := instance.Configuration().Get("FILE_SYSTEM_ROOT"); value != "/accumulo" {
		t.Fatalf("instance configuration aliased the caller's configuration: %q", value)
	}
	if _, ok := instance.Configuration().Lookup("added"); ok {
		t.Fatal("instance configuration observed a key added after construction")
	}
	instanceConfig.Set("instance-only", "1")
	if value := instance.Configuration().Get("instance-only"); value != "1" {
		t.Fatal("Configuration did not return the instance's own configuration")
	}
	if value := config.Get("instance-only"); value != "" {
		t.Fatal("instance configuration wrote through to the caller's configuration")
	}
}

func TestInstanceWithoutConfigurationReportsAnEmptyOne(t *testing.T) {
	instance := newTopologyInstance(t, topologyTree("uuid-1"), ZooKeeperConfig{})
	config := instance.Configuration()
	if config == nil {
		t.Fatal("Configuration is nil")
	}
	if config.Len() != 0 {
		t.Fatalf("Configuration has %d entries, want 0", config.Len())
	}
}

func TestStaticInstanceTopology(t *testing.T) {
	instance, err := NewStaticInstance("accumulo", "uuid-1")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := instance.RootTabletLocation(context.Background()); !errors.Is(err, ErrDiscoveryUnavailable) {
		t.Fatalf("RootTabletLocation error = %v, want ErrDiscoveryUnavailable", err)
	}
	if _, err := instance.ManagerLocations(context.Background()); !errors.Is(err, ErrDiscoveryUnavailable) {
		t.Fatalf("ManagerLocations error = %v, want ErrDiscoveryUnavailable", err)
	}
	if _, err := instance.Servers(context.Background()); !errors.Is(err, ErrDiscoveryUnavailable) {
		t.Fatalf("Servers error = %v, want ErrDiscoveryUnavailable", err)
	}
	if got := instance.ZooKeepers(); got != nil {
		t.Fatalf("ZooKeepers = %v, want nil", got)
	}
	if got := instance.Root(); got != "/accumulo/uuid-1" {
		t.Fatalf("Root = %q", got)
	}
	config := instance.Configuration()
	if config == nil || config.Len() != 0 {
		t.Fatal("Configuration is not an empty configuration")
	}
	config.Set("key", "value")
	if got := instance.Configuration().Get("key"); got != "value" {
		t.Fatal("static instance configuration is not stable across calls")
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := instance.RootTabletLocation(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("RootTabletLocation error = %v, want context.Canceled", err)
	}
	if _, err := instance.ManagerLocations(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("ManagerLocations error = %v, want context.Canceled", err)
	}
	if _, err := instance.Servers(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("Servers error = %v, want context.Canceled", err)
	}
}

func TestNoTopologyStub(t *testing.T) {
	stub := NewNoTopology()
	if _, err := stub.RootTabletLocation(context.Background()); !errors.Is(err, ErrDiscoveryUnavailable) {
		t.Fatalf("RootTabletLocation error = %v", err)
	}
	if _, err := stub.ManagerLocations(context.Background()); !errors.Is(err, ErrDiscoveryUnavailable) {
		t.Fatalf("ManagerLocations error = %v", err)
	}
	if _, err := stub.Servers(context.Background()); !errors.Is(err, ErrDiscoveryUnavailable) {
		t.Fatalf("Servers error = %v", err)
	}
	if stub.ZooKeepers() != nil || stub.Root() != "" {
		t.Fatal("stub reported wiring it does not have")
	}
	config := stub.Configuration()
	if config == nil || config.Len() != 0 {
		t.Fatal("stub configuration is not an empty configuration")
	}
	config.Set("key", "value")
	if got := stub.Configuration().Get("key"); got != "value" {
		t.Fatal("stub configuration is not stable across calls")
	}

	other := NewNoTopology()
	if _, ok := other.Configuration().Lookup("key"); ok {
		t.Fatal("stub configurations leaked across instances")
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := stub.RootTabletLocation(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("RootTabletLocation error = %v, want context.Canceled", err)
	}
	if _, err := stub.ManagerLocations(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("ManagerLocations error = %v, want context.Canceled", err)
	}
	if _, err := stub.Servers(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("Servers error = %v, want context.Canceled", err)
	}
}

func TestNoTopologyNilConfigurationPanics(t *testing.T) {
	var stub *NoTopology
	defer func() {
		recovered := recover()
		if recovered == nil {
			t.Fatal("Configuration did not panic on nil *NoTopology")
		}
		if !strings.Contains(fmt.Sprint(recovered), "nil NoTopology") {
			t.Fatalf("panic = %v, want nil NoTopology guidance", recovered)
		}
	}()
	_ = stub.Configuration()
}

func TestNoTopologyNilEmbeddedPointerPanicsOnConfiguration(t *testing.T) {
	type embeddingStub struct {
		*NoTopology
	}

	var stub embeddingStub
	defer func() {
		recovered := recover()
		if recovered == nil {
			t.Fatal("promoted Configuration did not panic on nil embedded *NoTopology")
		}
		if !strings.Contains(fmt.Sprint(recovered), "NewNoTopology") {
			t.Fatalf("panic = %v, want initialization guidance", recovered)
		}
	}()
	_ = stub.Configuration()
}

func TestInstanceTopologyIsConcurrentlyUsable(t *testing.T) {
	instance := newTopologyInstance(t, topologyTree("uuid-1"), ZooKeeperConfig{})

	var wg sync.WaitGroup
	for worker := 0; worker < 8; worker++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 50; i++ {
				if _, err := instance.RootTabletLocation(context.Background()); err != nil {
					t.Error(err)
					return
				}
				if _, err := instance.ManagerLocations(context.Background()); err != nil {
					t.Error(err)
					return
				}
				if _, err := instance.Servers(context.Background()); err != nil {
					t.Error(err)
					return
				}
				_ = instance.ZooKeepers()
				_ = instance.Root()
				instance.Configuration().Set("worker", "1")
			}
		}()
	}
	wg.Wait()
}

func TestInstanceTopologyAfterCloseRejectsLiveDiscovery(t *testing.T) {
	locator := topologyTree("uuid-1")
	instance := newTopologyInstance(t, locator, ZooKeeperConfig{})
	if err := instance.Close(); err != nil {
		t.Fatal(err)
	}

	// Both a cancellable and a background context must fail: Close releases
	// the instance permanently, and the outcome must not depend on whether
	// the caller passed a context that can be cancelled.
	cancellable, cancel := context.WithCancel(context.Background())
	defer cancel()
	for name, ctx := range map[string]context.Context{
		"background":  context.Background(),
		"cancellable": cancellable,
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := instance.RootTabletLocation(ctx); !errors.Is(err, ErrInstanceClosed) {
				t.Fatalf("RootTabletLocation error = %v, want ErrInstanceClosed", err)
			}
			if _, err := instance.ManagerLocations(ctx); !errors.Is(err, ErrInstanceClosed) {
				t.Fatalf("ManagerLocations error = %v, want ErrInstanceClosed", err)
			}
			if _, err := instance.Servers(ctx); !errors.Is(err, ErrInstanceClosed) {
				t.Fatalf("Servers error = %v, want ErrInstanceClosed", err)
			}
		})
	}
	if locator.rootCalls.Load() != 0 || locator.childCalls.Load() != 0 {
		t.Fatalf("closed instance still reached ZooKeeper: root=%d children=%d",
			locator.rootCalls.Load(), locator.childCalls.Load())
	}
}

func TestInstanceTopologyAfterCloseStillReportsStaticWiring(t *testing.T) {
	locator := topologyTree("uuid-1")
	instance := newTopologyInstance(t, locator, ZooKeeperConfig{})
	if err := instance.Close(); err != nil {
		t.Fatal(err)
	}
	if got := instance.Root(); got != "/accumulo/uuid-1" {
		t.Fatalf("Root after Close = %q", got)
	}
	if got := instance.ZooKeepers(); len(got) != 2 {
		t.Fatalf("ZooKeepers after Close = %v", got)
	}
	if config := instance.Configuration(); config == nil {
		t.Fatal("Configuration after Close is nil")
	}
	if locator.closes.Load() != 1 {
		t.Fatalf("locator closed %d times, want 1", locator.closes.Load())
	}
}

// blockingLocator blocks inside every live-state read until it is released,
// so a caller can prove that cancelling the context releases the accessor.
type blockingLocator struct {
	id      string
	started chan struct{}
	release chan struct{}
	once    sync.Once
}

func (l *blockingLocator) InstanceID() string { return l.id }
func (l *blockingLocator) Close()             {}

func (l *blockingLocator) block(ctx context.Context) error {
	l.once.Do(func() { close(l.started) })
	select {
	case <-l.release:
		return errors.New("released")
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (l *blockingLocator) RootTabletLocation(ctx context.Context) (*zk.Location, error) {
	return nil, l.block(ctx)
}

func (l *blockingLocator) InstancePath() string { return zk.InstanceRoot(l.id) }

func (l *blockingLocator) GetRaw(ctx context.Context, _ string) ([]byte, error) {
	return nil, l.block(ctx)
}

func (l *blockingLocator) Children(ctx context.Context, _ string) ([]string, error) {
	return nil, l.block(ctx)
}

func TestInstanceTopologyCancelsInFlightReads(t *testing.T) {
	calls := map[string]func(Instance, context.Context) error{
		"RootTabletLocation": func(instance Instance, ctx context.Context) error {
			_, err := instance.RootTabletLocation(ctx)
			return err
		},
		"ManagerLocations": func(instance Instance, ctx context.Context) error {
			_, err := instance.ManagerLocations(ctx)
			return err
		},
		"Servers": func(instance Instance, ctx context.Context) error {
			_, err := instance.Servers(ctx)
			return err
		},
	}
	for name, call := range calls {
		t.Run(name, func(t *testing.T) {
			blocking := &blockingLocator{
				id:      "uuid-1",
				started: make(chan struct{}),
				release: make(chan struct{}),
			}
			instance, err := newZooKeeperInstance(
				context.Background(),
				ZooKeeperConfig{Servers: []string{"zk:2181"}, InstanceName: "accumulo"},
				func(ZooKeeperConfig) (locator, error) { return blocking, nil },
			)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				close(blocking.release)
				_ = instance.Close()
			})

			ctx, cancel := context.WithCancel(context.Background())
			result := make(chan error, 1)
			go func() { result <- call(instance, ctx) }()
			<-blocking.started
			cancel()
			select {
			case err := <-result:
				if !errors.Is(err, context.Canceled) {
					t.Fatalf("error = %v, want context.Canceled", err)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("accessor did not return after its context was cancelled")
			}
		})
	}
}
