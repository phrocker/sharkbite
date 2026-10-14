package accumulo

import (
	"errors"
	"sync"

	"github.com/phrocker/shoal-oss/internal/ingestclient"
	"github.com/phrocker/shoal-oss/internal/managerclient"
	"github.com/phrocker/shoal-oss/internal/metadata"
	"github.com/phrocker/shoal-oss/internal/namespaces"
	"github.com/phrocker/shoal-oss/internal/scanclient"
	"github.com/phrocker/shoal-oss/internal/tablenames"
	"github.com/phrocker/shoal-oss/internal/transportpool"
)

// Connector is the root handle for Accumulo client operations.
//
// A Connector does not own the Instance passed to NewConnector. Callers may
// share an Instance across connectors and must close it separately.
type Connector struct {
	mu         sync.RWMutex
	passwordMu sync.Mutex
	// constraintMu serializes constraint allocation, whose check-then-write
	// sequence two callers must not interleave.
	constraintMu sync.Mutex
	instance     InstanceInfo
	credentials  Credentials
	options      normalizedConnectorOptions
	pool         *transportpool.Pool
	scan         scanclient.Adapter
	ingest       ingestclient.Adapter
	manager      managerclient.Adapter
	security     managerclient.SecurityAdapter
	managerAddr  managerAddressResolver
	clientAddr   clientServiceAddressResolver
	discovery    *connectorDiscovery
	closed       bool
}

// NewConnector validates and captures the instance and credentials used by
// future scanner, writer, and administration APIs.
func NewConnector(instance Instance, credentials Credentials, opts ConnectorOptions) (*Connector, error) {
	if instance == nil {
		return nil, errors.New("accumulo: instance is required")
	}
	info := instance.Info()
	if info.Name == "" || info.ID == "" {
		return nil, errors.New("accumulo: instance identity is incomplete")
	}
	if err := credentials.validate(); err != nil {
		return nil, err
	}
	normalized, err := normalizeConnectorOptions(opts)
	if err != nil {
		return nil, err
	}
	pool, err := transportpool.New(normalized.poolConfig)
	if err != nil {
		return nil, err
	}
	thriftCredentials, err := credentials.thrift(info.ID)
	if err != nil {
		_ = pool.Close()
		return nil, err
	}
	defer func() {
		for i := range thriftCredentials.Token {
			thriftCredentials.Token[i] = 0
		}
		thriftCredentials.Token = nil
	}()
	scan, err := scanclient.NewPooled(
		pool,
		info.ID,
		normalized.accumuloVersion,
		thriftCredentials,
		normalized.dialTimeout,
	)
	if err != nil {
		_ = pool.Close()
		return nil, err
	}
	ingest, err := ingestclient.NewPooled(
		pool,
		info.ID,
		normalized.accumuloVersion,
		thriftCredentials,
		normalized.dialTimeout,
	)
	if err != nil {
		_ = scan.Close()
		_ = pool.Close()
		return nil, err
	}
	managerAdapter, err := managerclient.NewPooled(
		pool,
		info.ID,
		normalized.accumuloVersion,
		thriftCredentials,
		normalized.dialTimeout,
	)
	if err != nil {
		_ = scan.Close()
		_ = ingest.Close()
		_ = pool.Close()
		return nil, err
	}
	connector := &Connector{
		instance:    info,
		credentials: credentials.clone(),
		options:     normalized,
		pool:        pool,
		scan:        scan,
		ingest:      ingest,
		manager:     managerAdapter,
		security:    managerAdapter,
	}
	if source, ok := instance.(discoveryInstance); ok {
		locator := source.discoveryLocator()
		if locator != nil {
			walker := metadata.NewWalkerWithLifecycle(locator, scan)
			namespaceNames := namespaces.NewResolver(locator)
			state, _ := locator.(tableStateReader)
			connector.discovery = newConnectorDiscovery(
				walker,
				namespaceNames,
				tablenames.NewResolver(locator, namespaceNames),
				state,
			)
			connector.managerAddr = zkManagerAddressResolver{locator: locator}
			connector.clientAddr = zkClientServiceAddressResolver{locator: locator}
		}
	}
	return connector, nil
}

// Instance returns the immutable identity captured by the connector.
func (c *Connector) Instance() InstanceInfo {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.instance
}

// Principal returns the authenticated principal name.
func (c *Connector) Principal() string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.credentials.Principal()
}

// Close releases connector-owned transports. It is safe to call repeatedly.
func (c *Connector) Close() error {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return nil
	}
	c.closed = true
	pool := c.pool
	discovery := c.discovery
	for i := range c.credentials.token {
		c.credentials.token[i] = 0
	}
	c.credentials.token = nil
	c.mu.Unlock()
	if discovery != nil {
		discovery.close()
	}
	return errors.Join(c.scan.Close(), c.ingest.Close(), c.manager.Close(), pool.Close())
}
