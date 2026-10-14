package zk

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	gozk "github.com/go-zookeeper/zk"
)

type blockingRawConn struct {
	started chan struct{}
	closed  chan struct{}
	done    chan struct{}
	once    sync.Once
}

func (c *blockingRawConn) AddAuth(string, []byte) error { return nil }

func (c *blockingRawConn) Get(string) ([]byte, *gozk.Stat, error) {
	close(c.started)
	<-c.closed
	close(c.done)
	return nil, nil, gozk.ErrClosing
}

func (c *blockingRawConn) Children(string) ([]string, *gozk.Stat, error) {
	close(c.started)
	<-c.closed
	close(c.done)
	return nil, nil, gozk.ErrClosing
}

func (c *blockingRawConn) Close() {
	c.once.Do(func() { close(c.closed) })
}

type staticRawTopologyConn struct {
	data     map[string][]byte
	children map[string][]string
	closes   *atomic.Int32
	auths    *atomic.Int32
	authErr  error
}

func (c *staticRawTopologyConn) AddAuth(string, []byte) error {
	if c.auths != nil {
		c.auths.Add(1)
	}
	return c.authErr
}

func (c *staticRawTopologyConn) Get(znodePath string) ([]byte, *gozk.Stat, error) {
	data, ok := c.data[znodePath]
	if !ok {
		return nil, nil, fmt.Errorf("get %s: %w", znodePath, gozk.ErrNoNode)
	}
	return data, &gozk.Stat{}, nil
}

func (c *staticRawTopologyConn) Children(znodePath string) ([]string, *gozk.Stat, error) {
	children, ok := c.children[znodePath]
	if !ok {
		return nil, nil, fmt.Errorf("children %s: %w", znodePath, gozk.ErrNoNode)
	}
	return append([]string(nil), children...), &gozk.Stat{}, nil
}

func (c *staticRawTopologyConn) Close() {
	if c.closes != nil {
		c.closes.Add(1)
	}
}

type blockingAuthConn struct {
	started chan struct{}
	closed  chan struct{}
	done    chan struct{}
	once    sync.Once
	authErr error
	closes  atomic.Int32
}

func (c *blockingAuthConn) AddAuth(string, []byte) error {
	close(c.started)
	<-c.closed
	close(c.done)
	return c.authErr
}

func (c *blockingAuthConn) Get(string) ([]byte, *gozk.Stat, error) {
	return nil, nil, gozk.ErrClosing
}

func (c *blockingAuthConn) Children(string) ([]string, *gozk.Stat, error) {
	return nil, nil, gozk.ErrClosing
}

func (c *blockingAuthConn) Close() {
	c.once.Do(func() {
		c.closes.Add(1)
		close(c.closed)
	})
}

func TestGetRawWithContextCancelsInFlightReadAndJoinsWorker(t *testing.T) {
	conn := &blockingRawConn{
		started: make(chan struct{}),
		closed:  make(chan struct{}),
		done:    make(chan struct{}),
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	result := make(chan error, 1)
	go func() {
		_, err := getRawWithContext(ctx, "/accumulo/uuid/namespaces", "", func() (rawZKConn, error) {
			return conn, nil
		})
		result <- err
	}()
	<-conn.started
	cancel()
	if err := <-result; !errors.Is(err, context.Canceled) {
		t.Fatalf("GetRaw error = %v, want context.Canceled", err)
	}
	select {
	case <-conn.done:
	default:
		t.Fatal("GetRaw returned before its ZooKeeper read worker exited")
	}
}

func TestLocatorRootTabletLocationUsesSingleScopedConnection(t *testing.T) {
	var connects atomic.Int32
	var closes atomic.Int32
	var auths atomic.Int32
	loc := &Locator{
		instanceID:     "uuid-1",
		instanceSecret: "secret",
		rawConnFactory: func() (rawZKConn, error) {
			connects.Add(1)
			return &staticRawTopologyConn{
				data: map[string][]byte{
					"/accumulo/uuid-1/root_tablet": []byte(`{"version":1,"columnValues":{"loc":{"session-1":"tserver-a:9997"}}}`),
				},
				closes: &closes,
				auths:  &auths,
			}, nil
		},
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	location, err := loc.RootTabletLocation(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if location == nil || location.HostPort != "tserver-a:9997" || location.Session != "session-1" {
		t.Fatalf("location = %+v, want tserver-a:9997/session-1", location)
	}
	if connects.Load() != 1 || closes.Load() != 1 {
		t.Fatalf("connects/closes = %d/%d, want 1/1", connects.Load(), closes.Load())
	}
	if auths.Load() != 1 {
		t.Fatalf("auth calls = %d, want 1", auths.Load())
	}
}

func TestTopologyReadScopeCancelsBlockingAddAuth(t *testing.T) {
	conn := &blockingAuthConn{
		started: make(chan struct{}),
		closed:  make(chan struct{}),
		done:    make(chan struct{}),
	}
	loc := &Locator{
		instanceID:     "uuid-1",
		instanceSecret: "secret",
		rawConnFactory: func() (rawZKConn, error) {
			return conn, nil
		},
	}
	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() {
		_, closeScope, err := loc.topologyReadScope(ctx)
		if closeScope != nil {
			defer closeScope()
		}
		result <- err
	}()
	<-conn.started
	cancel()
	if err := <-result; !errors.Is(err, context.Canceled) {
		t.Fatalf("topologyReadScope error = %v, want context.Canceled", err)
	}
	select {
	case <-conn.done:
	default:
		t.Fatal("topologyReadScope returned before AddAuth worker exited")
	}
}

func TestTopologyReadScopeReturnsAddAuthError(t *testing.T) {
	want := errors.New("auth failed")
	loc := &Locator{
		instanceID:     "uuid-1",
		instanceSecret: "secret",
		rawConnFactory: func() (rawZKConn, error) {
			return &staticRawTopologyConn{authErr: want}, nil
		},
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	_, closeScope, err := loc.topologyReadScope(ctx)
	if closeScope != nil {
		defer closeScope()
	}
	if !errors.Is(err, want) {
		t.Fatalf("topologyReadScope error = %v, want %v", err, want)
	}
}

func TestTopologyReadScopeCloseInterruptsBlockingAddAuth(t *testing.T) {
	conn := &blockingAuthConn{
		started: make(chan struct{}),
		closed:  make(chan struct{}),
		done:    make(chan struct{}),
	}
	loc := &Locator{
		instanceID:     "uuid-1",
		instanceSecret: "secret",
		rawConnFactory: func() (rawZKConn, error) {
			return conn, nil
		},
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	result := make(chan error, 1)
	go func() {
		_, closeScope, err := loc.topologyReadScope(ctx)
		if closeScope != nil {
			defer closeScope()
		}
		result <- err
	}()

	<-conn.started
	loc.Close()

	if err := <-result; !errors.Is(err, ErrClosed) {
		t.Fatalf("topologyReadScope error = %v, want ErrClosed", err)
	}
	select {
	case <-conn.done:
	default:
		t.Fatal("topologyReadScope returned before AddAuth worker exited")
	}
	if conn.closes.Load() != 1 {
		t.Fatalf("close calls = %d, want 1", conn.closes.Load())
	}
	loc.mu.Lock()
	closed := loc.closed
	tracked := len(loc.scopes)
	loc.mu.Unlock()
	if !closed || tracked != 0 {
		t.Fatalf("closed/tracked = %t/%d, want true/0", closed, tracked)
	}
}

func TestParseRootTabletMetadata_CurrentLocation(t *testing.T) {
	json := `{
		"version": 1,
		"columnValues": {
			"loc": {
				"/accumulo/abc/tservers/tserver-3:9997/lock-0000000123$deadbeef":
					"tserver-3.namespace.svc.cluster.local:9997"
			}
		}
	}`
	loc, err := parseRootTabletMetadata([]byte(json))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if loc == nil {
		t.Fatal("expected location, got nil")
	}
	if loc.HostPort != "tserver-3.namespace.svc.cluster.local:9997" {
		t.Errorf("HostPort = %q", loc.HostPort)
	}
	if !strings.HasSuffix(loc.Session, "$deadbeef") {
		t.Errorf("Session = %q, expected to end with $deadbeef", loc.Session)
	}
}

func TestParseRootTabletMetadata_NoLocation(t *testing.T) {
	// Tablet mid-move: "loc" absent, only "future" populated. Our V0 returns
	// (nil, nil) so the caller retries — we don't chase "future".
	json := `{
		"version": 1,
		"columnValues": {
			"future": {
				"sess1": "tserver-7:9997"
			}
		}
	}`
	loc, err := parseRootTabletMetadata([]byte(json))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if loc != nil {
		t.Errorf("expected nil location, got %+v", loc)
	}
}

func TestParseRootTabletMetadata_EmptyColumnValues(t *testing.T) {
	loc, err := parseRootTabletMetadata([]byte(`{"version":1,"columnValues":{}}`))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if loc != nil {
		t.Errorf("expected nil location, got %+v", loc)
	}
}

func TestParseRootTabletMetadata_VersionMismatch(t *testing.T) {
	loc, err := parseRootTabletMetadata([]byte(`{"version":2,"columnValues":{}}`))
	if err == nil {
		t.Fatal("expected version error, got nil")
	}
	if !strings.Contains(err.Error(), "unsupported RootTabletMetadata version") {
		t.Errorf("error = %v", err)
	}
	if loc != nil {
		t.Errorf("expected nil location on error, got %+v", loc)
	}
}

func TestParseRootTabletMetadata_MalformedJSON(t *testing.T) {
	_, err := parseRootTabletMetadata([]byte(`{not json`))
	if err == nil {
		t.Fatal("expected parse error, got nil")
	}
	if !strings.Contains(err.Error(), "parse RootTabletMetadata json") {
		t.Errorf("error = %v", err)
	}
}

func TestParseRootTabletMetadata_EmptyBytes(t *testing.T) {
	_, err := parseRootTabletMetadata(nil)
	if err == nil {
		t.Fatal("expected error on empty bytes, got nil")
	}
	if !strings.Contains(err.Error(), "empty") {
		t.Errorf("error = %v", err)
	}
}

func TestParseRootTabletMetadata_MultipleLocations(t *testing.T) {
	// Java enforces at-most-one location across loc+future. If "loc" itself
	// has multiple entries, that's a bug elsewhere — we surface it.
	json := `{
		"version": 1,
		"columnValues": {
			"loc": {
				"sess1": "tserver-3:9997",
				"sess2": "tserver-5:9997"
			}
		}
	}`
	_, err := parseRootTabletMetadata([]byte(json))
	if err == nil {
		t.Fatal("expected error on multiple locations, got nil")
	}
	if !strings.Contains(err.Error(), "2 current locations") {
		t.Errorf("error = %v", err)
	}
}
