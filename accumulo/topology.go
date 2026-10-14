package accumulo

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strconv"
	"sync"

	"github.com/phrocker/shoal-oss/internal/zk"
)

// ServerKind names the Accumulo 4 server role that advertises a client
// service endpoint in ZooKeeper.
type ServerKind string

const (
	// TabletServerKind is a tablet server, advertised under "tservers".
	TabletServerKind ServerKind = "tserver"

	// ScanServerKind is a scan server, advertised under "sservers".
	ScanServerKind ServerKind = "sserver"

	// CompactorKind is a compactor, advertised under "compactors".
	CompactorKind ServerKind = "compactor"
)

// ServerConnection is one live Accumulo server endpoint discovered through
// ZooKeeper. It is the Go equivalent of Sharkbite's
// interconnect::ServerConnection, extended with the Accumulo 4 server role
// and resource group, which did not exist in the Accumulo 1.x layout
// Sharkbite reads.
type ServerConnection struct {
	// Kind is the server role that published the endpoint.
	Kind ServerKind

	// Group is the Accumulo 4 resource group the server belongs to.
	Group string

	// Host is the advertised host name or address.
	Host string

	// Port is the advertised client-service port.
	Port uint16
}

// HostPort returns the "host:port" form of the endpoint, matching
// Sharkbite's ServerConnection::toString.
func (s ServerConnection) HostPort() string {
	return net.JoinHostPort(s.Host, strconv.FormatUint(uint64(s.Port), 10))
}

// String implements fmt.Stringer.
func (s ServerConnection) String() string {
	return fmt.Sprintf("%s %s %s", s.Kind, s.Group, s.HostPort())
}

// TabletLocation is a resolved tablet assignment: the tablet server hosting
// the tablet and the session lock it holds.
type TabletLocation struct {
	// HostPort is the tablet server's client-service address.
	HostPort string

	// Session is the tablet server's lock ID for the assignment.
	Session string
}

// String implements fmt.Stringer.
func (l TabletLocation) String() string { return l.HostPort }

// RootTabletLocation returns the tablet server currently hosting the root
// tablet.
//
// Wire semantics: the location is read from the Accumulo 4 root-tablet znode
// /accumulo/<instance-id>/root_tablet, whose RootTabletMetadata JSON carries
// the current location under the "loc" column family. Sharkbite reads the
// Accumulo 1.x layout /<root>/root_tablet/location and splits the value on
// '|', a format Accumulo 4 no longer writes. When ctx is cancellable, the
// lookup reuses one authenticated operation-scoped ZooKeeper session for the
// whole read and closes it on return or cancellation.
//
// Returns ErrTabletNotLocated while the root tablet has no current
// assignment, which happens during tablet movement; callers should retry.
func (i *zkLocator) RootTabletLocation(ctx context.Context) (TabletLocation, error) {
	if err := ctx.Err(); err != nil {
		return TabletLocation{}, err
	}
	if i.closed.Load() {
		return TabletLocation{}, fmt.Errorf("%w: root tablet location", ErrInstanceClosed)
	}
	location, err := i.locator.RootTabletLocation(ctx)
	if err != nil {
		return TabletLocation{}, fmt.Errorf("accumulo: resolve root tablet location: %w", err)
	}
	if location == nil || location.HostPort == "" {
		return TabletLocation{}, fmt.Errorf("%w: root tablet", ErrTabletNotLocated)
	}
	return TabletLocation{HostPort: location.HostPort, Session: location.Session}, nil
}

// ManagerLocations returns the manager addresses advertised under the
// instance's manager lock path, ordered by lock sequence: the first entry
// holds the lock and is the active manager, and any later entries are queued
// candidates that would take over on failover.
//
// Wire semantics: lock children of /accumulo/<instance-id>/managers/lock are
// validated and ordered exactly as Accumulo's ServiceLock.validateAndSort
// does, and each lock node's ServiceLockData JSON is decoded to collect
// descriptors whose service is MANAGER. Bootstrap descriptors that advertise
// no address, or the "0.0.0.0:0" placeholder, make the active manager
// unavailable rather than promoting a queued candidate to the first slot.
// When ctx is cancellable, the traversal reuses one authenticated
// operation-scoped ZooKeeper session and closes it on return or cancellation.
//
// Returns ErrManagerUnavailable when no lock node advertises a usable
// manager. Sharkbite's getMasterLocations returns an empty vector in that
// case, which callers cannot distinguish from a transport failure; Shoal
// reports it explicitly.
func (i *zkLocator) ManagerLocations(ctx context.Context) ([]string, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if i.closed.Load() {
		return nil, fmt.Errorf("%w: manager locations", ErrInstanceClosed)
	}
	addresses, err := zk.ManagerAddresses(ctx, i.locator)
	if err != nil {
		return nil, mapManagerDiscoveryError(err)
	}
	return addresses, nil
}

// Servers returns the live Accumulo servers that advertise the client
// service: tablet servers, scan servers, and compactors.
//
// Wire semantics: each server role publishes lock znodes under
// /accumulo/<instance-id>/{tservers,sservers,compactors}/<resource-group>/<server>,
// and the lowest-sequence lock node of every server carries ServiceLockData
// JSON whose CLIENT descriptor holds the client-service address. Results are
// ordered by role, then resource group, then the publishing ZooKeeper server
// child identity. When ctx is cancellable, the traversal reuses one
// authenticated operation-scoped ZooKeeper session and closes it on return or
// cancellation. Sharkbite reads the flat Accumulo 1.x /<root>/tservers
// layout, which has no resource groups and no scan servers or compactors.
//
// Returns ErrClientServiceUnavailable when no server advertises the client
// service.
func (i *zkLocator) Servers(ctx context.Context) ([]ServerConnection, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if i.closed.Load() {
		return nil, fmt.Errorf("%w: servers", ErrInstanceClosed)
	}
	services, err := zk.ClientServices(ctx, i.locator)
	if err != nil {
		return nil, mapClientServiceDiscoveryError(err)
	}
	servers := make([]ServerConnection, 0, len(services))
	for _, service := range services {
		server, err := newServerConnection(service)
		if err != nil {
			return nil, err
		}
		servers = append(servers, server)
	}
	return servers, nil
}

// ZooKeepers returns a copy of the ZooKeeper servers this instance was
// configured with, mirroring Sharkbite's ZookeeperInstance::getZookeepers.
func (i *zkLocator) ZooKeepers() []string {
	return append([]string(nil), i.zooKeepers...)
}

// Root returns the instance's ZooKeeper root path,
// "/accumulo/<instance-id>", mirroring Sharkbite's
// ZookeeperInstance::getRoot.
func (i *zkLocator) Root() string { return i.locator.InstancePath() }

// Configuration returns the client configuration this instance was created
// with, mirroring Sharkbite's ZookeeperInstance::getConfiguration. The
// returned Configuration is the instance's own stable mutable pointer:
// mutating it changes what later calls observe, and it is never aliased to
// the caller's original.
func (i *zkLocator) Configuration() *Configuration { return i.configuration }

// RootTabletLocation reports ErrDiscoveryUnavailable: a static instance has
// no ZooKeeper session to resolve the root tablet with.
func (i *staticInstance) RootTabletLocation(ctx context.Context) (TabletLocation, error) {
	if err := ctx.Err(); err != nil {
		return TabletLocation{}, err
	}
	return TabletLocation{}, fmt.Errorf("%w: root tablet location", ErrDiscoveryUnavailable)
}

// ManagerLocations reports ErrDiscoveryUnavailable: a static instance has no
// ZooKeeper session to read manager locks from.
func (i *staticInstance) ManagerLocations(ctx context.Context) ([]string, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return nil, fmt.Errorf("%w: manager locations", ErrDiscoveryUnavailable)
}

// Servers reports ErrDiscoveryUnavailable: a static instance has no
// ZooKeeper session to enumerate live servers with.
func (i *staticInstance) Servers(ctx context.Context) ([]ServerConnection, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return nil, fmt.Errorf("%w: servers", ErrDiscoveryUnavailable)
}

// ZooKeepers returns nil: a static instance is not backed by ZooKeeper.
func (i *staticInstance) ZooKeepers() []string { return nil }

// Root returns the instance's ZooKeeper root path, which is derived from the
// instance ID and therefore well defined even without a ZooKeeper session.
func (i *staticInstance) Root() string { return zk.InstanceRoot(i.info.ID) }

// Configuration returns the client configuration this instance was created
// with. It is never nil.
func (i *staticInstance) Configuration() *Configuration { return i.configuration }

// NoTopology implements the cluster-topology half of Instance for
// implementations that are not backed by a ZooKeeper session: every
// live-state accessor reports ErrDiscoveryUnavailable instead of pretending
// to resolve anything. Embed it by value, or initialize pointer fields with
// NewNoTopology, to satisfy Instance and override the methods that a
// particular implementation can answer.
type NoTopology struct {
	configurationOnce sync.Once
	configuration     *Configuration
}

// NewNoTopology returns an initialized topology stub whose Configuration
// method owns a stable empty Configuration.
func NewNoTopology() *NoTopology { return &NoTopology{} }

// RootTabletLocation reports ErrDiscoveryUnavailable.
func (*NoTopology) RootTabletLocation(ctx context.Context) (TabletLocation, error) {
	if err := ctx.Err(); err != nil {
		return TabletLocation{}, err
	}
	return TabletLocation{}, fmt.Errorf("%w: root tablet location", ErrDiscoveryUnavailable)
}

// ManagerLocations reports ErrDiscoveryUnavailable.
func (*NoTopology) ManagerLocations(ctx context.Context) ([]string, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return nil, fmt.Errorf("%w: manager locations", ErrDiscoveryUnavailable)
}

// Servers reports ErrDiscoveryUnavailable.
func (*NoTopology) Servers(ctx context.Context) ([]ServerConnection, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return nil, fmt.Errorf("%w: servers", ErrDiscoveryUnavailable)
}

// ZooKeepers returns nil.
func (*NoTopology) ZooKeepers() []string { return nil }

// Root returns the empty string, because no instance root is known.
func (*NoTopology) Root() string { return "" }

// Configuration returns this stub's own empty Configuration, stable across
// calls, so writes through it are observed by later calls exactly as they are
// for a real instance. Embedders that carry client configuration should
// override it.
//
// A nil *NoTopology has nowhere to keep that configuration, so it panics
// instead of silently returning a fresh Configuration whose writes would be
// lost. Embed NoTopology by value, or initialize pointer fields with
// NewNoTopology; see the type documentation.
func (i *NoTopology) Configuration() *Configuration {
	if i == nil {
		panic("accumulo: nil NoTopology has no stable Configuration; embed NoTopology by value or initialize it with NewNoTopology")
	}
	i.configurationOnce.Do(func() {
		i.configuration = NewConfiguration()
	})
	return i.configuration
}

func newServerConnection(service zk.ClientService) (ServerConnection, error) {
	host, portText, err := net.SplitHostPort(service.Address)
	if err != nil {
		return ServerConnection{}, fmt.Errorf(
			"accumulo: parse %s client service address %q: %w",
			service.Kind, service.Address, err,
		)
	}
	if host == "" {
		return ServerConnection{}, fmt.Errorf(
			"accumulo: %s client service address %q has no host",
			service.Kind, service.Address,
		)
	}
	port, err := strconv.ParseUint(portText, 10, 16)
	if err != nil {
		return ServerConnection{}, fmt.Errorf(
			"accumulo: parse %s client service port %q: %w",
			service.Kind, service.Address, err,
		)
	}
	if port == 0 {
		return ServerConnection{}, fmt.Errorf(
			"accumulo: %s client service address %q has port 0, which cannot be dialled",
			service.Kind, service.Address,
		)
	}
	return ServerConnection{
		Kind:  ServerKind(service.Kind),
		Group: service.Group,
		Host:  host,
		Port:  uint16(port),
	}, nil
}

func mapManagerDiscoveryError(err error) error {
	if errors.Is(err, zk.ErrManagerUnavailable) {
		return fmt.Errorf("%w: no manager lock advertises an address", ErrManagerUnavailable)
	}
	return fmt.Errorf("accumulo: resolve manager locations: %w", err)
}

func mapClientServiceDiscoveryError(err error) error {
	if errors.Is(err, zk.ErrClientServiceUnavailable) {
		return fmt.Errorf("%w: no server advertises the client service", ErrClientServiceUnavailable)
	}
	return fmt.Errorf("accumulo: resolve servers: %w", err)
}
