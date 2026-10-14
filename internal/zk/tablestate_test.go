package zk

import (
	"context"
	"errors"
	"path"
	"testing"
	"time"

	gozk "github.com/go-zookeeper/zk"
)

type staticRawConn struct {
	data []byte
	stat *gozk.Stat
	err  error
	path string
}

func (c *staticRawConn) AddAuth(string, []byte) error { return nil }

func (c *staticRawConn) Children(string) ([]string, *gozk.Stat, error) {
	return nil, nil, gozk.ErrNoNode
}

func (c *staticRawConn) Get(znodePath string) ([]byte, *gozk.Stat, error) {
	c.path = znodePath
	return c.data, c.stat, c.err
}

func (c *staticRawConn) Close() {}

func TestTableStateWithContextCancelsStalledRead(t *testing.T) {
	conn := &blockingRawConn{
		started: make(chan struct{}),
		closed:  make(chan struct{}),
		done:    make(chan struct{}),
	}
	loc := &Locator{
		instanceID: "uuid-1",
		rawConnFactory: func() (rawZKConn, error) {
			return conn, nil
		},
	}
	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() {
		_, err := loc.TableState(ctx, "5")
		result <- err
	}()

	<-conn.started
	cancel()

	if err := <-result; !errors.Is(err, context.Canceled) {
		t.Fatalf("TableState error = %v, want context.Canceled", err)
	}
	select {
	case <-conn.done:
	default:
		t.Fatal("TableState returned before its ZooKeeper read worker exited")
	}
}

func TestTableStateWithContextDeadlineReturnsFromStalledRead(t *testing.T) {
	conn := &blockingRawConn{
		started: make(chan struct{}),
		closed:  make(chan struct{}),
		done:    make(chan struct{}),
	}
	loc := &Locator{
		instanceID: "uuid-1",
		rawConnFactory: func() (rawZKConn, error) {
			return conn, nil
		},
	}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	result := make(chan error, 1)
	go func() {
		_, err := loc.TableState(ctx, "5")
		result <- err
	}()

	<-conn.started

	if err := <-result; !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("TableState error = %v, want context.DeadlineExceeded", err)
	}
	select {
	case <-conn.done:
	default:
		t.Fatal("TableState returned before its ZooKeeper read worker exited")
	}
}

func TestTableStatePreservesMissingAndVersionSemantics(t *testing.T) {
	t.Run("existing state", func(t *testing.T) {
		conn := &staticRawConn{
			data: []byte(" OFFLINE \n"),
			stat: &gozk.Stat{Version: 7},
		}
		loc := &Locator{
			instanceID: "uuid-1",
			rawConnFactory: func() (rawZKConn, error) {
				return conn, nil
			},
		}
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()

		got, err := loc.TableState(ctx, "5")
		if err != nil {
			t.Fatal(err)
		}
		if !got.Exists || got.State != TableStateOffline || got.Version != 7 {
			t.Fatalf("TableState = %+v, want Exists/OFFLINE/version 7", got)
		}
		wantPath := path.Join(zRoot, "uuid-1", zTables, "5", zTableState)
		if conn.path != wantPath {
			t.Fatalf("path = %q, want %q", conn.path, wantPath)
		}
	})

	t.Run("missing state znode", func(t *testing.T) {
		loc := &Locator{
			instanceID: "uuid-1",
			rawConnFactory: func() (rawZKConn, error) {
				return &staticRawConn{err: gozk.ErrNoNode}, nil
			},
		}
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()

		got, err := loc.TableState(ctx, "missing")
		if err != nil {
			t.Fatal(err)
		}
		if got.Exists {
			t.Fatalf("TableState = %+v, want Exists=false", got)
		}
	})
}
