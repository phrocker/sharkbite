package transportpool

import (
	"context"
	"errors"
	"io"
	"sync/atomic"
	"testing"
	"time"
)

var testKey = Key{
	Address:         "tserver:9997",
	Service:         "scan",
	InstanceID:      "uuid-1",
	ProtocolVersion: "4.0",
}

func TestPoolReusesIdleTransport(t *testing.T) {
	pool, err := New(Config{MaxIdlePerEndpoint: 1})
	if err != nil {
		t.Fatal(err)
	}
	var dials atomic.Int32
	dial := func(context.Context, Key) (io.Closer, error) {
		dials.Add(1)
		return &fakeTransport{}, nil
	}

	first, err := pool.Acquire(context.Background(), testKey, dial)
	if err != nil {
		t.Fatal(err)
	}
	transport := first.Transport()
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	second, err := pool.Acquire(context.Background(), testKey, dial)
	if err != nil {
		t.Fatal(err)
	}
	if second.Transport() != transport {
		t.Fatal("Acquire did not reuse idle transport")
	}
	_ = second.Close()
	if dials.Load() != 1 {
		t.Fatalf("dials = %d, want 1", dials.Load())
	}
}

func TestPoolLeasesAreExclusive(t *testing.T) {
	pool, _ := New(Config{MaxIdlePerEndpoint: 2})
	var dials atomic.Int32
	dial := func(context.Context, Key) (io.Closer, error) {
		dials.Add(1)
		return &fakeTransport{}, nil
	}
	first, _ := pool.Acquire(context.Background(), testKey, dial)
	second, _ := pool.Acquire(context.Background(), testKey, dial)
	if first.Transport() == second.Transport() {
		t.Fatal("concurrent leases shared a transport")
	}
	if dials.Load() != 2 {
		t.Fatalf("dials = %d, want 2", dials.Load())
	}
	_ = first.Close()
	_ = second.Close()
}

func TestPoolBoundsIdleTransportsPerEndpoint(t *testing.T) {
	pool, _ := New(Config{MaxIdlePerEndpoint: 1})
	dial := func(context.Context, Key) (io.Closer, error) {
		return &fakeTransport{}, nil
	}
	first, _ := pool.Acquire(context.Background(), testKey, dial)
	second, _ := pool.Acquire(context.Background(), testKey, dial)
	firstTransport := first.Transport().(*fakeTransport)
	secondTransport := second.Transport().(*fakeTransport)

	_ = first.Close()
	_ = second.Close()
	if firstTransport.closes.Load()+secondTransport.closes.Load() != 1 {
		t.Fatal("pool did not close the transport beyond its idle bound")
	}
	_ = pool.Close()
	if firstTransport.closes.Load() != 1 || secondTransport.closes.Load() != 1 {
		t.Fatal("pool did not close both transports exactly once")
	}
}

func TestPoolInvalidationEvictsTransport(t *testing.T) {
	pool, _ := New(Config{MaxIdlePerEndpoint: 1})
	var dials atomic.Int32
	dial := func(context.Context, Key) (io.Closer, error) {
		dials.Add(1)
		return &fakeTransport{}, nil
	}
	first, _ := pool.Acquire(context.Background(), testKey, dial)
	firstTransport := first.Transport().(*fakeTransport)
	if err := first.Invalidate(); err != nil {
		t.Fatal(err)
	}
	if firstTransport.closes.Load() != 1 {
		t.Fatal("invalidated transport was not closed")
	}
	second, _ := pool.Acquire(context.Background(), testKey, dial)
	if dials.Load() != 2 {
		t.Fatalf("dials = %d, want 2", dials.Load())
	}
	_ = second.Close()
}

func TestPoolSeparatesKeys(t *testing.T) {
	pool, _ := New(Config{MaxIdlePerEndpoint: 2})
	var dials atomic.Int32
	dial := func(context.Context, Key) (io.Closer, error) {
		dials.Add(1)
		return &fakeTransport{}, nil
	}
	first, _ := pool.Acquire(context.Background(), testKey, dial)
	_ = first.Close()
	other := testKey
	other.Service = "manager"
	second, _ := pool.Acquire(context.Background(), other, dial)
	if dials.Load() != 2 {
		t.Fatalf("dials = %d, want 2", dials.Load())
	}
	_ = second.Close()
}

func TestPoolExpiresIdleTransport(t *testing.T) {
	pool, _ := New(Config{MaxIdlePerEndpoint: 1, IdleTimeout: 5 * time.Millisecond})
	var dials atomic.Int32
	dial := func(context.Context, Key) (io.Closer, error) {
		dials.Add(1)
		return &fakeTransport{}, nil
	}
	first, _ := pool.Acquire(context.Background(), testKey, dial)
	firstTransport := first.Transport().(*fakeTransport)
	_ = first.Close()
	deadline := time.Now().Add(time.Second)
	for firstTransport.closes.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if firstTransport.closes.Load() != 1 {
		t.Fatal("idle transport was not closed after its timeout")
	}
	second, _ := pool.Acquire(context.Background(), testKey, dial)
	if dials.Load() != 2 {
		t.Fatalf("dials = %d, want 2", dials.Load())
	}
	_ = second.Close()
}

func TestPoolRejectsCancelledContextBeforeIdleReuse(t *testing.T) {
	pool, _ := New(Config{MaxIdlePerEndpoint: 1})
	dial := func(context.Context, Key) (io.Closer, error) {
		return &fakeTransport{}, nil
	}
	lease, _ := pool.Acquire(context.Background(), testKey, dial)
	transport := lease.Transport()
	_ = lease.Close()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := pool.Acquire(ctx, testKey, dial); !errors.Is(err, context.Canceled) {
		t.Fatalf("Acquire error = %v, want context.Canceled", err)
	}
	next, err := pool.Acquire(context.Background(), testKey, dial)
	if err != nil {
		t.Fatal(err)
	}
	if next.Transport() != transport {
		t.Fatal("cancelled Acquire removed the idle transport")
	}
	_ = next.Close()
}

func TestPoolCloseClosesIdleAndLeasedTransports(t *testing.T) {
	pool, _ := New(Config{MaxIdlePerEndpoint: 1})
	dial := func(context.Context, Key) (io.Closer, error) {
		return &fakeTransport{}, nil
	}
	idle, _ := pool.Acquire(context.Background(), testKey, dial)
	idleTransport := idle.Transport().(*fakeTransport)
	_ = idle.Close()

	other := testKey
	other.Address = "tserver-2:9997"
	leased, _ := pool.Acquire(context.Background(), other, dial)
	leasedTransport := leased.Transport().(*fakeTransport)

	if err := pool.Close(); err != nil {
		t.Fatal(err)
	}
	if err := pool.Close(); err != nil {
		t.Fatal(err)
	}
	if idleTransport.closes.Load() != 1 || leasedTransport.closes.Load() != 1 {
		t.Fatal("Close did not close all transports exactly once")
	}
	if err := leased.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestPoolDoesNotHoldLockWhileDialing(t *testing.T) {
	pool, _ := New(Config{MaxIdlePerEndpoint: 1})
	started := make(chan struct{})
	release := make(chan struct{})
	result := make(chan error, 1)

	go func() {
		_, err := pool.Acquire(context.Background(), testKey, func(context.Context, Key) (io.Closer, error) {
			close(started)
			<-release
			return &fakeTransport{}, nil
		})
		result <- err
	}()
	<-started

	closed := make(chan struct{})
	go func() {
		_ = pool.Close()
		close(closed)
	}()
	select {
	case <-closed:
	case <-time.After(time.Second):
		t.Fatal("Close blocked while dial held the pool lock")
	}
	close(release)
	if err := <-result; !errors.Is(err, ErrClosed) {
		t.Fatalf("Acquire error = %v, want ErrClosed", err)
	}
}

type fakeTransport struct {
	closes atomic.Int32
}

func (t *fakeTransport) Close() error {
	t.closes.Add(1)
	return nil
}
