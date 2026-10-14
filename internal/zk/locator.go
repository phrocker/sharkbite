// Package zk wraps github.com/go-zookeeper/zk and resolves the root-tablet
// tserver location from the bootstrap chain
//
//	/accumulo/instances/<name>      -> instance UUID (bytes)
//	/accumulo/<uuid>/root_tablet    -> JSON RootTabletMetadata
//
// Reference: core/.../client/clientImpl/RootClientTabletCache.java:115-148
// Schema:    core/.../metadata/schema/RootTabletMetadata.java
package zk

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"sync"
	"time"

	gozk "github.com/go-zookeeper/zk"
)

// ZK paths. Mirror core/.../Constants.java + RootTable.java.
const (
	zRoot       = "/accumulo"
	zInstances  = "/instances"
	zRootTablet = "/root_tablet"

	// Column-family names from MetadataSchema.java.
	cfCurrentLocation = "loc"
	cfFutureLocation  = "future" //nolint:unused // reserved for future fallback

	// RootTabletMetadata.VERSION (Java: private static final int VERSION = 1).
	rootTabletMetadataVersion = 1
)

// Location is a resolved tablet location: which tserver hosts it and which
// session lock it holds.
type Location struct {
	HostPort string // e.g. "tserver-3.namespace.svc.cluster.local:9997"
	Session  string // lockId (qualifier under the "loc" column family)
}

// Locator resolves Accumulo metadata from ZooKeeper. Construct with New
// and Close when done.
type Locator struct {
	conn           *gozk.Conn
	instanceName   string
	instanceID     string
	servers        []string
	sessionTimeout time.Duration
	instanceSecret string
	rawConnFactory func() (rawZKConn, error)

	mu     sync.Mutex
	closed bool
	scopes map[*scopedTopologyReader]struct{}
}

// ErrClosed reports a read attempted after the locator was closed. Close
// releases the instance permanently: it must not be possible to reconnect
// through a scoped read afterwards.
var ErrClosed = errors.New("zk: locator closed")

// New connects to a ZK quorum, resolves the instance name to its instance
// UUID, and returns a Locator ready to issue further lookups.
func New(servers []string, instanceName string, sessionTimeout time.Duration) (*Locator, error) {
	return NewWithAuth(servers, instanceName, sessionTimeout, "")
}

// NewWithAuth is New plus an optional digest-auth secret. When non-empty
// the locator's ZK session adds ("digest", "accumulo:"+secret) before
// the first GetRaw / Children call, matching Accumulo's instance-secret
// scheme (core ZooSession.digestAuth). World-readable znodes (root
// tablet, namespaces JSON, instance UUID) don't need this; per-table
// /config znodes do.
func NewWithAuth(servers []string, instanceName string, sessionTimeout time.Duration, instanceSecret string) (*Locator, error) {
	conn, _, err := gozk.Connect(servers, sessionTimeout)
	if err != nil {
		return nil, fmt.Errorf("zk connect: %w", err)
	}
	if instanceSecret != "" {
		if err := conn.AddAuth("digest", []byte("accumulo:"+instanceSecret)); err != nil {
			conn.Close()
			return nil, fmt.Errorf("zk add digest auth: %w", err)
		}
	}

	l := &Locator{
		conn:           conn,
		instanceName:   instanceName,
		servers:        append([]string(nil), servers...),
		sessionTimeout: sessionTimeout,
		instanceSecret: instanceSecret,
	}
	l.rawConnFactory = func() (rawZKConn, error) {
		conn, _, err := gozk.Connect(l.servers, l.sessionTimeout)
		return conn, err
	}
	id, err := l.lookupInstanceID()
	if err != nil {
		conn.Close()
		return nil, err
	}
	l.instanceID = id
	return l, nil
}

// InstanceID returns the resolved Accumulo instance UUID.
func (l *Locator) InstanceID() string { return l.instanceID }

// Close terminates the ZK session and permanently disables further reads.
// Scoped connections handed out for cancellable reads are closed too, so an
// in-flight topology read is released rather than outliving the locator.
func (l *Locator) Close() {
	l.mu.Lock()
	if l.closed {
		l.mu.Unlock()
		return
	}
	l.mu.Unlock()

	l.closeScopes()
	if l.conn != nil {
		l.conn.Close()
	}
}

// closeScopes marks the locator closed and closes every scoped connection it
// handed out, so no cancellable read can outlive Close or reconnect after it.
func (l *Locator) closeScopes() {
	l.mu.Lock()
	l.closed = true
	scopes := make([]*scopedTopologyReader, 0, len(l.scopes))
	for scope := range l.scopes {
		scopes = append(scopes, scope)
	}
	l.scopes = nil
	l.mu.Unlock()

	for _, scope := range scopes {
		scope.Close()
	}
}

func (l *Locator) isClosed() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.closed
}

func (l *Locator) trackScope(scope *scopedTopologyReader) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closed {
		return ErrClosed
	}
	if l.scopes == nil {
		l.scopes = make(map[*scopedTopologyReader]struct{})
	}
	l.scopes[scope] = struct{}{}
	return nil
}

func (l *Locator) releaseScope(scope *scopedTopologyReader) {
	l.mu.Lock()
	if l.scopes != nil {
		delete(l.scopes, scope)
	}
	l.mu.Unlock()
	scope.Close()
}

func (l *Locator) lookupInstanceID() (string, error) {
	p := path.Join(zRoot, zInstances, l.instanceName)
	data, _, err := l.conn.Get(p)
	if err != nil {
		return "", fmt.Errorf("get %s: %w", p, err)
	}
	if len(data) == 0 {
		return "", fmt.Errorf("instance %q not found at %s", l.instanceName, p)
	}
	return string(data), nil
}

// RootTabletLocation reads the root-tablet znode and returns the current
// tserver location. Returns (nil, nil) if no current location is set —
// e.g. during tablet movement; caller should retry.
func (l *Locator) RootTabletLocation(ctx context.Context) (*Location, error) {
	p := path.Join(zRoot, l.instanceID, zRootTablet)
	reader, closeScope, err := l.topologyReadScope(ctx)
	if err != nil {
		return nil, err
	}
	defer closeScope()
	data, err := reader.GetRaw(ctx, p)
	if err != nil {
		return nil, err
	}
	return parseRootTabletMetadata(data)
}

// InstanceRoot returns the absolute ZK path for an instance ID, i.e.
// "/accumulo/<instance-id>". It is the path form used by every
// instance-scoped subtree (root tablet, manager locks, server locks,
// table config).
func InstanceRoot(instanceID string) string {
	return path.Join(zRoot, instanceID)
}

// InstancePath returns the absolute ZK path for the bound instance,
// i.e. "/accumulo/<instance-id>". Useful for callers that need to walk
// instance-scoped subtrees (table config, etc.).
func (l *Locator) InstancePath() string {
	return path.Join(zRoot, l.instanceID)
}

// GetRaw fetches the raw znode bytes at an absolute path. ErrNoNode from
// the underlying ZK client is surfaced as a wrapped error so callers can
// distinguish "missing" from transport failures via errors.Is(err,
// gozk.ErrNoNode). Cancellable contexts use a dedicated ZooKeeper connection
// so cancellation can close the in-flight read without disrupting the shared
// locator session.
func (l *Locator) GetRaw(ctx context.Context, znodePath string) ([]byte, error) {
	data, _, err := l.get(ctx, znodePath)
	return data, err
}

func (l *Locator) get(ctx context.Context, znodePath string) ([]byte, *gozk.Stat, error) {
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
	if l.isClosed() {
		return nil, nil, ErrClosed
	}
	if ctx.Done() != nil {
		scope, err := l.newScopedTopologyReader(ctx)
		if err != nil {
			return nil, nil, err
		}
		defer l.releaseScope(scope)
		return scope.get(ctx, znodePath)
	}
	data, stat, err := l.conn.Get(znodePath)
	if err != nil {
		return nil, nil, fmt.Errorf("get %s: %w", znodePath, err)
	}
	return data, stat, nil
}

type rawZKConn interface {
	AddAuth(string, []byte) error
	Get(string) ([]byte, *gozk.Stat, error)
	Children(string) ([]string, *gozk.Stat, error)
	Close()
}

type rawResult struct {
	data []byte
	stat *gozk.Stat
	err  error
}

type authResult struct {
	err error
}

type scopedTopologyReader struct {
	instancePath string
	conn         rawZKConn
	closeOnce    sync.Once
}

func (r *scopedTopologyReader) InstancePath() string { return r.instancePath }

func (r *scopedTopologyReader) GetRaw(ctx context.Context, znodePath string) ([]byte, error) {
	data, _, err := r.get(ctx, znodePath)
	return data, err
}

func (r *scopedTopologyReader) get(ctx context.Context, znodePath string) ([]byte, *gozk.Stat, error) {
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
	result := make(chan rawResult, 1)
	go func() {
		data, stat, err := r.conn.Get(znodePath)
		if err != nil {
			err = fmt.Errorf("get %s: %w", znodePath, err)
		}
		result <- rawResult{data: data, stat: stat, err: err}
	}()

	select {
	case response := <-result:
		if err := ctx.Err(); err != nil {
			r.Close()
			return nil, nil, err
		}
		return response.data, response.stat, response.err
	case <-ctx.Done():
		r.Close()
		<-result
		return nil, nil, ctx.Err()
	}
}

func (r *scopedTopologyReader) Children(ctx context.Context, znodePath string) ([]string, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	result := make(chan rawChildrenResult, 1)
	go func() {
		names, _, err := r.conn.Children(znodePath)
		if err != nil {
			err = fmt.Errorf("children %s: %w", znodePath, err)
		}
		result <- rawChildrenResult{names: names, err: err}
	}()

	select {
	case response := <-result:
		if err := ctx.Err(); err != nil {
			r.Close()
			return nil, err
		}
		return response.names, response.err
	case <-ctx.Done():
		r.Close()
		<-result
		return nil, ctx.Err()
	}
}

func (r *scopedTopologyReader) Close() {
	r.closeOnce.Do(func() { r.conn.Close() })
}

func getRawWithContext(
	ctx context.Context,
	znodePath, instanceSecret string,
	connect func() (rawZKConn, error),
) ([]byte, error) {
	data, _, err := getWithContext(ctx, znodePath, instanceSecret, connect)
	return data, err
}

func getWithContext(
	ctx context.Context,
	znodePath, instanceSecret string,
	connect func() (rawZKConn, error),
) ([]byte, *gozk.Stat, error) {
	conn, err := connect()
	if err != nil {
		return nil, nil, fmt.Errorf("zk connect: %w", err)
	}
	result := make(chan rawResult, 1)
	go func() {
		if instanceSecret != "" {
			if err := conn.AddAuth("digest", []byte("accumulo:"+instanceSecret)); err != nil {
				result <- rawResult{err: fmt.Errorf("zk add digest auth: %w", err)}
				return
			}
		}
		data, stat, err := conn.Get(znodePath)
		if err != nil {
			err = fmt.Errorf("get %s: %w", znodePath, err)
		}
		result <- rawResult{data: data, stat: stat, err: err}
	}()

	select {
	case response := <-result:
		conn.Close()
		if err := ctx.Err(); err != nil {
			return nil, nil, err
		}
		return response.data, response.stat, response.err
	case <-ctx.Done():
		conn.Close()
		<-result
		return nil, nil, ctx.Err()
	}
}

// Children returns the child znode names of znodePath. Names are NOT
// joined with znodePath; callers join as needed. Empty slice for a
// childless znode; ErrNoNode for a missing znode (wrapped).
func (l *Locator) Children(ctx context.Context, znodePath string) ([]string, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if l.isClosed() {
		return nil, ErrClosed
	}
	if ctx.Done() != nil {
		scope, err := l.newScopedTopologyReader(ctx)
		if err != nil {
			return nil, err
		}
		defer l.releaseScope(scope)
		return scope.Children(ctx, znodePath)
	}
	names, _, err := l.conn.Children(znodePath)
	if err != nil {
		return nil, fmt.Errorf("children %s: %w", znodePath, err)
	}
	return names, nil
}

func (l *Locator) rawConnFactoryOrDefault() func() (rawZKConn, error) {
	if l.rawConnFactory != nil {
		return l.rawConnFactory
	}
	return func() (rawZKConn, error) {
		conn, _, err := gozk.Connect(l.servers, l.sessionTimeout)
		return conn, err
	}
}

func (l *Locator) topologyReadScope(ctx context.Context) (LockReader, func(), error) {
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
	if l.isClosed() {
		return nil, nil, ErrClosed
	}
	if ctx.Done() == nil {
		return l, func() {}, nil
	}
	scope, err := l.newScopedTopologyReader(ctx)
	if err != nil {
		return nil, nil, err
	}
	return scope, func() { l.releaseScope(scope) }, nil
}

func (l *Locator) newScopedTopologyReader(ctx context.Context) (*scopedTopologyReader, error) {
	conn, err := l.rawConnFactoryOrDefault()()
	if err != nil {
		return nil, fmt.Errorf("zk connect: %w", err)
	}
	scope := &scopedTopologyReader{
		instancePath: l.InstancePath(),
		conn:         conn,
	}
	// Closing between the connect and here must not leave a live session
	// behind: trackScope rejects the scope and the connection is dropped.
	if err := l.trackScope(scope); err != nil {
		scope.Close()
		return nil, err
	}
	if err := addAuthWithContext(ctx, conn, l.instanceSecret); err != nil {
		l.releaseScope(scope)
		if l.isClosed() {
			return nil, ErrClosed
		}
		return nil, err
	}
	if l.isClosed() {
		l.releaseScope(scope)
		return nil, ErrClosed
	}
	return scope, nil
}

func addAuthWithContext(ctx context.Context, conn rawZKConn, instanceSecret string) error {
	if instanceSecret == "" {
		return nil
	}
	result := make(chan authResult, 1)
	go func() {
		err := conn.AddAuth("digest", []byte("accumulo:"+instanceSecret))
		if err != nil {
			err = fmt.Errorf("zk add digest auth: %w", err)
		}
		result <- authResult{err: err}
	}()

	select {
	case response := <-result:
		if err := ctx.Err(); err != nil {
			conn.Close()
			return err
		}
		return response.err
	case <-ctx.Done():
		conn.Close()
		<-result
		return ctx.Err()
	}
}

type rawChildrenResult struct {
	names []string
	err   error
}

// childrenWithContext lists children on a dedicated ZooKeeper connection so
// that cancelling ctx closes the in-flight read instead of leaving the caller
// blocked on the shared locator session.
func childrenWithContext(
	ctx context.Context,
	znodePath, instanceSecret string,
	connect func() (rawZKConn, error),
) ([]string, error) {
	conn, err := connect()
	if err != nil {
		return nil, fmt.Errorf("zk connect: %w", err)
	}
	result := make(chan rawChildrenResult, 1)
	go func() {
		if instanceSecret != "" {
			if err := conn.AddAuth("digest", []byte("accumulo:"+instanceSecret)); err != nil {
				result <- rawChildrenResult{err: fmt.Errorf("zk add digest auth: %w", err)}
				return
			}
		}
		names, _, err := conn.Children(znodePath)
		if err != nil {
			err = fmt.Errorf("children %s: %w", znodePath, err)
		}
		result <- rawChildrenResult{names: names, err: err}
	}()

	select {
	case response := <-result:
		conn.Close()
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		return response.names, response.err
	case <-ctx.Done():
		conn.Close()
		<-result
		return nil, ctx.Err()
	}
}

// rootTabletJSON mirrors RootTabletMetadata.Data (Gson struct).
type rootTabletJSON struct {
	Version      int                          `json:"version"`
	ColumnValues map[string]map[string]string `json:"columnValues"`
}

// parseRootTabletMetadata extracts the current tserver location from the
// JSON stored at /accumulo/<uuid>/root_tablet. Returns (nil, nil) if no
// "loc" column-family entry is present.
//
// Reference: RootTabletMetadata.java — schema, version check, and the
// invariant that there is at most one location across "loc" + "future".
func parseRootTabletMetadata(data []byte) (*Location, error) {
	if len(data) == 0 {
		return nil, errors.New("root_tablet znode is empty")
	}

	var rt rootTabletJSON
	if err := json.Unmarshal(data, &rt); err != nil {
		return nil, fmt.Errorf("parse RootTabletMetadata json: %w", err)
	}
	if rt.Version != rootTabletMetadataVersion {
		return nil, fmt.Errorf("unsupported RootTabletMetadata version: got %d, want %d",
			rt.Version, rootTabletMetadataVersion)
	}

	loc, ok := rt.ColumnValues[cfCurrentLocation]
	if !ok || len(loc) == 0 {
		return nil, nil // no current location (likely mid-move); caller retries
	}
	if len(loc) > 1 {
		// Java enforces this invariant; report rather than guess which to use.
		return nil, fmt.Errorf("root tablet has %d current locations, expected at most 1", len(loc))
	}
	for session, hostPort := range loc {
		return &Location{HostPort: hostPort, Session: session}, nil
	}
	return nil, nil // unreachable
}
