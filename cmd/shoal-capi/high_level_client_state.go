package main

import (
	"context"
	"sync"
	"time"

	"github.com/phrocker/shoal-oss/accumulo"
)

type clientSnapshot struct {
	table          string
	authorizations [][]byte
	columns        []accumulo.Column
	threadCount    int32
}

type clientScanOneFunc func(
	context.Context,
	clientSnapshot,
	*accumulo.Range,
) ([]accumulo.KeyValue, error)

type clientScanManyFunc func(
	context.Context,
	clientSnapshot,
	[]*accumulo.Range,
) ([]accumulo.KeyValue, error)

type clientStreamOneFunc func(
	context.Context,
	clientSnapshot,
	*accumulo.Range,
) (scanCursorSource, error)

type clientStreamManyFunc func(
	context.Context,
	clientSnapshot,
	[]*accumulo.Range,
) (scanCursorSource, error)

type ownedClient struct {
	mu             sync.Mutex
	connector      *ownedConnector
	table          string
	authorizations [][]byte
	columns        []accumulo.Column
	threadCount    int32
	scanOne        clientScanOneFunc
	scanMany       clientScanManyFunc
	streamOne      clientStreamOneFunc
	streamMany     clientStreamManyFunc
	closed         bool
	closeOnce      sync.Once
	closeErr       error
}

func newOwnedClient(
	connector *ownedConnector,
	table string,
	authorizations [][]byte,
	threadCount int32,
) *ownedClient {
	client := &ownedClient{
		connector:      connector,
		table:          table,
		authorizations: cloneClientAuthorizations(authorizations),
		threadCount:    threadCount,
	}
	client.scanOne = func(
		ctx context.Context,
		snapshot clientSnapshot,
		scanRange *accumulo.Range,
	) ([]accumulo.KeyValue, error) {
		scanner, err := connector.connector.NewScanner(
			accumulo.Table{Name: snapshot.table},
			accumulo.ScannerOptions{
				Authorizations: snapshot.authorizations,
				Columns:        snapshot.columns,
			},
		)
		if err != nil {
			return nil, err
		}
		return scanner.Scan(ctx, scanRange)
	}
	client.scanMany = func(
		ctx context.Context,
		snapshot clientSnapshot,
		ranges []*accumulo.Range,
	) ([]accumulo.KeyValue, error) {
		scanner, err := connector.connector.NewBatchScanner(
			accumulo.Table{Name: snapshot.table},
			accumulo.ScannerOptions{
				Authorizations: snapshot.authorizations,
				Columns:        snapshot.columns,
				Parallelism:    int(snapshot.threadCount),
			},
		)
		if err != nil {
			return nil, err
		}
		return scanner.Scan(ctx, ranges)
	}
	client.streamOne = func(
		ctx context.Context,
		snapshot clientSnapshot,
		scanRange *accumulo.Range,
	) (scanCursorSource, error) {
		scanner, err := connector.connector.NewScanner(
			accumulo.Table{Name: snapshot.table},
			accumulo.ScannerOptions{
				Authorizations: snapshot.authorizations,
				Columns:        snapshot.columns,
			},
		)
		if err != nil {
			return nil, err
		}
		return scanner.Stream(ctx, scanRange)
	}
	client.streamMany = func(
		ctx context.Context,
		snapshot clientSnapshot,
		ranges []*accumulo.Range,
	) (scanCursorSource, error) {
		scanner, err := connector.connector.NewBatchScanner(
			accumulo.Table{Name: snapshot.table},
			accumulo.ScannerOptions{
				Authorizations: snapshot.authorizations,
				Columns:        snapshot.columns,
				Parallelism:    int(snapshot.threadCount),
			},
		)
		if err != nil {
			return nil, err
		}
		return scanner.Stream(ctx, ranges)
	}
	return client
}

func (c *ownedClient) setThreads(threadCount int32) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return accumulo.ErrConnectorClosed
	}
	c.threadCount = threadCount
	return nil
}

func (c *ownedClient) setTable(table string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return accumulo.ErrConnectorClosed
	}
	c.table = table
	return nil
}

func (c *ownedClient) checkOpen() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return accumulo.ErrConnectorClosed
	}
	return nil
}

func (c *ownedClient) setAuthorizations(authorizations [][]byte) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return accumulo.ErrConnectorClosed
	}
	c.authorizations = cloneClientAuthorizations(authorizations)
	return nil
}

func (c *ownedClient) addColumn(column accumulo.Column) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return accumulo.ErrConnectorClosed
	}
	c.columns = append(c.columns, cloneClientColumn(column))
	return nil
}

func (c *ownedClient) snapshot(requireTable bool) (clientSnapshot, func(), error) {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return clientSnapshot{}, nil, accumulo.ErrConnectorClosed
	}
	if requireTable && c.table == "" {
		c.mu.Unlock()
		return clientSnapshot{}, nil, accumulo.ErrTableNotFound
	}
	done, err := c.connector.retain()
	if err != nil {
		c.mu.Unlock()
		return clientSnapshot{}, nil, err
	}
	snapshot := clientSnapshot{
		table:          c.table,
		authorizations: cloneClientAuthorizations(c.authorizations),
		columns:        cloneClientColumns(c.columns),
		threadCount:    c.threadCount,
	}
	c.mu.Unlock()
	return snapshot, done, nil
}

func (c *ownedClient) beginSnapshot(
	requireTable bool,
	timeout time.Duration,
) (context.Context, clientSnapshot, func(), error) {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return nil, clientSnapshot{}, nil, accumulo.ErrConnectorClosed
	}
	if requireTable && c.table == "" {
		c.mu.Unlock()
		return nil, clientSnapshot{}, nil, accumulo.ErrTableNotFound
	}
	ctx, done, err := c.connector.begin(timeout)
	if err != nil {
		c.mu.Unlock()
		return nil, clientSnapshot{}, nil, err
	}
	snapshot := clientSnapshot{
		table:          c.table,
		authorizations: cloneClientAuthorizations(c.authorizations),
		columns:        cloneClientColumns(c.columns),
		threadCount:    c.threadCount,
	}
	c.mu.Unlock()
	return ctx, snapshot, done, nil
}

func (c *ownedClient) begin(timeout time.Duration) (context.Context, func(), error) {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return nil, nil, accumulo.ErrConnectorClosed
	}
	ctx, done, err := c.connector.begin(timeout)
	c.mu.Unlock()
	return ctx, done, err
}

func (c *ownedClient) close() error {
	c.closeOnce.Do(func() {
		c.mu.Lock()
		c.closed = true
		c.mu.Unlock()
		c.closeErr = c.connector.close()
	})
	return c.closeErr
}

func (c *ownedClient) closeBounded(timeout time.Duration) error {
	c.closeOnce.Do(func() {
		c.mu.Lock()
		c.closed = true
		c.mu.Unlock()
		c.closeErr = c.connector.closeBounded(timeout)
	})
	return c.closeErr
}

func cloneClientAuthorizations(authorizations [][]byte) [][]byte {
	result := make([][]byte, len(authorizations))
	for index := range authorizations {
		result[index] = append([]byte(nil), authorizations[index]...)
	}
	return result
}

func cloneClientColumn(column accumulo.Column) accumulo.Column {
	family := column.Family()
	qualifier := column.Qualifier()
	if qualifier == nil {
		return accumulo.NewColumnFamily(family)
	}
	return accumulo.NewColumn(family, qualifier)
}

func cloneClientColumns(columns []accumulo.Column) []accumulo.Column {
	result := make([]accumulo.Column, len(columns))
	for index := range columns {
		result[index] = cloneClientColumn(columns[index])
	}
	return result
}

type clientRegistry struct {
	mu     sync.RWMutex
	nextID uint64
	items  map[uint64]*ownedClient
}

func newClientRegistry() *clientRegistry {
	return &clientRegistry{
		nextID: 1,
		items:  make(map[uint64]*ownedClient),
	}
}

func (r *clientRegistry) add(client *ownedClient) (uint64, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for attempts := uint64(0); attempts < ^uint64(0); attempts++ {
		id := r.nextID
		r.nextID++
		if r.nextID == 0 {
			r.nextID = 1
		}
		if id != 0 {
			if _, exists := r.items[id]; !exists {
				r.items[id] = client
				return id, true
			}
		}
	}
	return 0, false
}

func (r *clientRegistry) get(id uint64) (*ownedClient, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	client, ok := r.items[id]
	return client, ok
}

func (r *clientRegistry) remove(id uint64) (*ownedClient, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	client, ok := r.items[id]
	delete(r.items, id)
	return client, ok
}

var clients = newClientRegistry()
