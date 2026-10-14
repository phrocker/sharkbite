package accumulo

import (
	"context"
	"errors"
	"fmt"

	"github.com/phrocker/shoal-oss/internal/cache"
	"github.com/phrocker/shoal-oss/internal/metadata"
	"github.com/phrocker/shoal-oss/internal/tablenames"
	"github.com/phrocker/shoal-oss/internal/zk"
)

// Namespace is an Accumulo namespace identity.
type Namespace struct {
	Name string
	ID   string
}

// VersionedProperties is an Accumulo property snapshot and its persistent
// property-store version.
type VersionedProperties struct {
	Version    int64
	Properties map[string]string
}

// Table is an Accumulo table identity.
type Table struct {
	Name string
	ID   string
}

// TabletExtent identifies an Accumulo tablet's (PrevRow, EndRow] range.
// Nil PrevRow means negative infinity; nil EndRow means positive infinity.
type TabletExtent struct {
	TableID string
	PrevRow []byte
	EndRow  []byte
}

// TabletServer is a tablet's current server assignment.
type TabletServer struct {
	HostPort string
	// Session is the current-location metadata qualifier/generation.
	Session string
	// ServerLock is the full srv:lock identity returned by conditional sessions.
	ServerLock string
}

// Tablet is the public, Go-native discovery view of an Accumulo tablet.
// Server is nil while the tablet has no current assignment.
type Tablet struct {
	Extent TabletExtent
	Server *TabletServer
}

type namespaceResolver interface {
	ResolveID(context.Context, string) (string, error)
	ResolveName(context.Context, string) (string, error)
	List(context.Context) (map[string]string, error)
	Invalidate()
}

type tableStateReader interface {
	TableState(context.Context, string) (zk.TableStateResult, error)
}

type tableNameResolver interface {
	ResolveID(context.Context, string) (string, error)
	ResolveName(context.Context, string) (string, error)
	List(context.Context) (map[string]string, error)
	ListNamespace(context.Context, string) (map[string]string, error)
	Invalidate()
}

type freshTableNameResolver interface {
	ResolveIDFresh(context.Context, string) (string, error)
}

type connectorDiscovery struct {
	tablets    *cache.LocatorCache
	namespaces namespaceResolver
	tables     tableNameResolver
	states     tableStateReader
}

func newConnectorDiscovery(
	tablets cache.TableLocator,
	namespaces namespaceResolver,
	tables tableNameResolver,
	states tableStateReader,
) *connectorDiscovery {
	return &connectorDiscovery{
		tablets:    cache.New(tablets),
		namespaces: namespaces,
		tables:     tables,
		states:     states,
	}
}

func (d *connectorDiscovery) close() {
	d.invalidateAll()
}

func (d *connectorDiscovery) invalidateNames() {
	d.namespaces.Invalidate()
	d.tables.Invalidate()
}

func (d *connectorDiscovery) invalidateAll() {
	d.namespaces.Invalidate()
	d.tablets.InvalidateAll()
	d.tables.Invalidate()
}

// TableByName resolves a qualified or default-namespace table name.
func (c *Connector) TableByName(ctx context.Context, name string) (Table, error) {
	if err := ctx.Err(); err != nil {
		return Table{}, err
	}
	if name == "" {
		return Table{}, fmt.Errorf("%w: empty table name", ErrTableNotFound)
	}
	discovery, err := c.discoveryState()
	if err != nil {
		return Table{}, err
	}
	id, err := discovery.tables.ResolveID(ctx, name)
	if err != nil {
		if errors.Is(err, tablenames.ErrTableNotFound) {
			return Table{}, fmt.Errorf("%w: table name %q", ErrTableNotFound, name)
		}
		return Table{}, fmt.Errorf("accumulo: resolve table name %q: %w", name, err)
	}
	return Table{Name: name, ID: id}, nil
}

// ResolveTableID forces a fresh resolution of name's current table ID,
// invalidating any table-name-to-ID mapping this connection has already
// cached before resolving — unlike TableByName, which is happy to
// return an already-cached value with no enforced TTL (see
// internal/tablenames.Resolver: it only refreshes on an explicit
// Invalidate call or a namespace-generation bump). AddTableSplits
// already does this same invalidate-then-resolve internally, for the
// same reason: to observe a concurrent delete-and-recreate of the
// table under the same name rather than silently reusing a stale
// mapping. Callers that need to detect such a change across a window
// of their own — see internal/promotion.Promote's destination table
// identity pin — should call this instead of TableByName at each
// checkpoint; calling TableByName twice would risk observing the same
// stale cached ID both times and never detecting the change.
func (c *Connector) ResolveTableID(ctx context.Context, name string) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if name == "" {
		return "", fmt.Errorf("%w: empty table name", ErrTableNotFound)
	}
	discovery, err := c.discoveryState()
	if err != nil {
		return "", err
	}
	id, err := resolveFreshTableID(ctx, discovery.tables, name)
	if err != nil {
		if errors.Is(err, tablenames.ErrTableNotFound) {
			return "", fmt.Errorf("%w: table name %q", ErrTableNotFound, name)
		}
		return "", fmt.Errorf("accumulo: resolve table name %q: %w", name, err)
	}
	return id, nil
}

func resolveFreshTableID(ctx context.Context, resolver tableNameResolver, name string) (string, error) {
	if fresh, ok := resolver.(freshTableNameResolver); ok {
		return fresh.ResolveIDFresh(ctx, name)
	}
	resolver.Invalidate()
	return resolver.ResolveID(ctx, name)
}

// TableByID validates a table ID and resolves its qualified name.
func (c *Connector) TableByID(ctx context.Context, id string) (Table, error) {
	if err := ctx.Err(); err != nil {
		return Table{}, err
	}
	if id == "" {
		return Table{}, fmt.Errorf("%w: empty table ID", ErrTableNotFound)
	}
	discovery, err := c.discoveryState()
	if err != nil {
		return Table{}, err
	}
	if _, err := discovery.tablets.LocateTable(ctx, id); err != nil {
		return Table{}, mapTabletDiscoveryError(id, nil, err)
	}
	name, err := discovery.tables.ResolveName(ctx, id)
	if err != nil {
		if errors.Is(err, tablenames.ErrTableNotFound) {
			return Table{}, fmt.Errorf("%w: table ID %q", ErrTableNotFound, id)
		}
		return Table{}, fmt.Errorf("accumulo: resolve table ID %q: %w", id, err)
	}
	return Table{Name: name, ID: id}, nil
}

// Tablets lists the current tablet extents and assignments for table.
func (c *Connector) Tablets(ctx context.Context, table Table) ([]Tablet, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if table.ID == "" {
		return nil, fmt.Errorf("%w: empty table ID", ErrTableNotFound)
	}
	discovery, err := c.discoveryState()
	if err != nil {
		return nil, err
	}
	tablets, err := discovery.tablets.LocateTable(ctx, table.ID)
	if err != nil {
		return nil, mapTabletDiscoveryError(table.ID, nil, err)
	}
	out := make([]Tablet, len(tablets))
	for i := range tablets {
		out[i] = publicTablet(tablets[i])
	}
	return out, nil
}

// LocateTablet returns the tablet whose (PrevRow, EndRow] range contains row.
func (c *Connector) LocateTablet(ctx context.Context, table Table, row []byte) (Tablet, error) {
	if err := ctx.Err(); err != nil {
		return Tablet{}, err
	}
	if table.ID == "" {
		return Tablet{}, fmt.Errorf("%w: empty table ID", ErrTableNotFound)
	}
	discovery, err := c.discoveryState()
	if err != nil {
		return Tablet{}, err
	}
	tablet, err := discovery.tablets.Locate(ctx, table.ID, row)
	if err != nil {
		return Tablet{}, mapTabletDiscoveryError(table.ID, row, err)
	}
	if tablet.Location == nil || tablet.Location.HostPort == "" {
		return Tablet{}, fmt.Errorf("%w: table=%s row=%q", ErrTabletNotLocated, table.ID, row)
	}
	return publicTablet(tablet), nil
}

// InvalidateTablet invalidates the cached assignment covering row.
func (c *Connector) InvalidateTablet(table Table, row []byte) error {
	if table.ID == "" {
		return fmt.Errorf("%w: empty table ID", ErrTableNotFound)
	}
	discovery, err := c.discoveryState()
	if err != nil {
		return err
	}
	discovery.tablets.Invalidate(table.ID, row)
	return nil
}

// InvalidateTable invalidates every cached tablet for table.
func (c *Connector) InvalidateTable(table Table) error {
	if table.ID == "" {
		return fmt.Errorf("%w: empty table ID", ErrTableNotFound)
	}
	discovery, err := c.discoveryState()
	if err != nil {
		return err
	}
	discovery.tablets.InvalidateTable(table.ID)
	return nil
}

// InvalidateDiscovery clears all tablet, table-name, and namespace discovery caches.
func (c *Connector) InvalidateDiscovery() error {
	discovery, err := c.discoveryState()
	if err != nil {
		return err
	}
	discovery.invalidateAll()
	return nil
}

func (c *Connector) discoveryState() (*connectorDiscovery, error) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if c.closed {
		return nil, ErrConnectorClosed
	}
	if c.discovery == nil {
		return nil, ErrDiscoveryUnavailable
	}
	return c.discovery, nil
}

func publicTablet(tablet metadata.TabletInfo) Tablet {
	out := Tablet{
		Extent: TabletExtent{
			TableID: tablet.TableID,
			PrevRow: cloneRow(tablet.PrevRow),
			EndRow:  cloneRow(tablet.EndRow),
		},
	}
	if tablet.Location != nil {
		out.Server = &TabletServer{
			HostPort:   tablet.Location.HostPort,
			Session:    tablet.Location.Session,
			ServerLock: tablet.ServerLock,
		}
	}
	return out
}

func cloneRow(row []byte) []byte {
	if row == nil {
		return nil
	}
	return append([]byte{}, row...)
}

func mapTabletDiscoveryError(tableID string, row []byte, err error) error {
	switch {
	case errors.Is(err, cache.ErrTableNotFound):
		return fmt.Errorf("%w: table=%s: %w", ErrTableNotFound, tableID, err)
	case errors.Is(err, cache.ErrNoTabletCovers):
		return fmt.Errorf("%w: table=%s row=%q: %w", ErrNoTabletCoversRow, tableID, row, err)
	default:
		return fmt.Errorf("accumulo: discover tablets for table %s: %w", tableID, err)
	}
}
