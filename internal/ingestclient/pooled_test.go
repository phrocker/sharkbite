package ingestclient

import (
	"context"
	"errors"
	"io"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/apache/thrift/lib/go/thrift"

	"github.com/phrocker/shoal-oss/internal/thrift/gen/data"
	"github.com/phrocker/shoal-oss/internal/thrift/gen/security"
	"github.com/phrocker/shoal-oss/internal/transportpool"
)

func TestPooledSessionKeepsExclusiveTransportThroughClose(t *testing.T) {
	pooled, pool := newTestPooled(t)
	defer pool.Close()

	rpc := &fakeIngestRPC{startID: 73}
	transport := &fakeIngestTransport{rpc: rpc}
	var dials atomic.Int32
	var gotKey transportpool.Key
	pooled.dial = func(_ context.Context, key transportpool.Key) (io.Closer, error) {
		gotKey = key
		dials.Add(1)
		return transport, nil
	}

	session, err := pooled.Start(context.Background(), "tablet-1:9997", DurabilitySync)
	if err != nil {
		t.Fatal(err)
	}
	if session.UpdateID() != 73 {
		t.Fatalf("UpdateID = %d, want 73", session.UpdateID())
	}
	if err := session.Apply(context.Background(), testExtent(), testMutations()); err != nil {
		t.Fatal(err)
	}
	if _, err := session.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if transport.closes.Load() != 0 {
		t.Fatal("healthy transport was closed instead of pooled")
	}

	second, err := pooled.Start(context.Background(), "tablet-1:9997", DurabilityDefault)
	if err != nil {
		t.Fatal(err)
	}
	if cancelled, err := second.Cancel(context.Background()); err != nil || !cancelled {
		t.Fatalf("Cancel = %v, %v", cancelled, err)
	}
	if dials.Load() != 1 {
		t.Fatalf("dials = %d, want one pooled transport", dials.Load())
	}
	wantKey := transportpool.Key{
		Address:         "tablet-1:9997",
		Service:         "ingest",
		InstanceID:      "uuid-1",
		ProtocolVersion: "4.0.0-SNAPSHOT",
	}
	if gotKey != wantKey {
		t.Fatalf("pool key = %+v, want %+v", gotKey, wantKey)
	}
	if got := rpc.calls(); !equalStrings(got, []string{"start", "apply", "close", "start", "cancel"}) {
		t.Fatalf("RPC calls = %v", got)
	}
}

func TestPooledUpdateCredentialsCopiesReplacementForNextRPCAndRejectsClosed(t *testing.T) {
	pooled, pool := newTestPooled(t)
	defer pool.Close()

	rpc := &fakeIngestRPC{startID: 73}
	pooled.dial = func(context.Context, transportpool.Key) (io.Closer, error) {
		return &fakeIngestTransport{rpc: rpc}, nil
	}
	replacement := &security.TCredentials{
		Principal:      "root",
		TokenClassName: "PasswordToken",
		Token:          []byte("replacement"),
		InstanceId:     "uuid-1",
	}
	if err := pooled.UpdateCredentials(replacement); err != nil {
		t.Fatal(err)
	}
	replacement.Token[0] = 'X'
	session, err := pooled.Start(context.Background(), "tablet-1:9997", DurabilityDefault)
	if err != nil {
		t.Fatal(err)
	}
	if got := rpc.startCredentialToken(); string(got) != "replacement" || string(got) == "secret" {
		t.Fatalf("RPC token = %q, want isolated replacement", got)
	}
	if _, err := session.Cancel(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := pooled.Close(); err != nil {
		t.Fatal(err)
	}
	if err := pooled.UpdateCredentials(&security.TCredentials{Token: []byte("later")}); !errors.Is(err, ErrClosed) {
		t.Fatalf("closed update error = %v, want ErrClosed", err)
	}
}

func TestPooledStartApplicationFailurePreservesTransport(t *testing.T) {
	pooled, pool := newTestPooled(t)
	defer pool.Close()

	rpc := &fakeIngestRPC{
		startID:  9,
		startErr: thrift.NewTApplicationException(thrift.INTERNAL_ERROR, "server failure"),
	}
	var dials atomic.Int32
	pooled.dial = func(context.Context, transportpool.Key) (io.Closer, error) {
		dials.Add(1)
		return &fakeIngestTransport{rpc: rpc}, nil
	}

	if _, err := pooled.Start(context.Background(), "tablet-1:9997", DurabilityDefault); err == nil {
		t.Fatal("expected start failure")
	}
	rpc.mu.Lock()
	rpc.startErr = nil
	rpc.mu.Unlock()
	session, err := pooled.Start(context.Background(), "tablet-1:9997", DurabilityDefault)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := session.Cancel(context.Background()); err != nil {
		t.Fatal(err)
	}
	if dials.Load() != 1 {
		t.Fatalf("dials = %d, want application failure to preserve transport", dials.Load())
	}
}

func TestSessionApplyWireFailureInvalidatesTransport(t *testing.T) {
	pooled, pool := newTestPooled(t)
	defer pool.Close()

	var dials atomic.Int32
	var first *fakeIngestTransport
	pooled.dial = func(context.Context, transportpool.Key) (io.Closer, error) {
		number := dials.Add(1)
		rpc := &fakeIngestRPC{startID: data.UpdateID(number)}
		if number == 1 {
			rpc.applyErr = thrift.NewTTransportExceptionFromError(errors.New("connection reset"))
		}
		transport := &fakeIngestTransport{rpc: rpc}
		if number == 1 {
			first = transport
		}
		return transport, nil
	}

	session, err := pooled.Start(context.Background(), "tablet-1:9997", DurabilityDefault)
	if err != nil {
		t.Fatal(err)
	}
	if err := session.Apply(context.Background(), testExtent(), testMutations()); err == nil {
		t.Fatal("expected apply failure")
	}
	if first.closes.Load() != 1 {
		t.Fatal("wire-broken transport was not invalidated")
	}
	if _, err := session.Close(context.Background()); !errors.Is(err, ErrSessionClosed) {
		t.Fatalf("Close error = %v, want ErrSessionClosed", err)
	}

	next, err := pooled.Start(context.Background(), "tablet-1:9997", DurabilityDefault)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := next.Cancel(context.Background()); err != nil {
		t.Fatal(err)
	}
	if dials.Load() != 2 {
		t.Fatalf("dials = %d, want replacement transport", dials.Load())
	}
}

func TestSessionCloseCancelRaceIssuesOneTerminalRPC(t *testing.T) {
	for iteration := 0; iteration < 50; iteration++ {
		pooled, pool := newTestPooled(t)
		rpc := &fakeIngestRPC{startID: 1}
		pooled.dial = func(context.Context, transportpool.Key) (io.Closer, error) {
			return &fakeIngestTransport{rpc: rpc}, nil
		}
		session, err := pooled.Start(context.Background(), "tablet-1:9997", DurabilityDefault)
		if err != nil {
			t.Fatal(err)
		}

		start := make(chan struct{})
		var workers sync.WaitGroup
		workers.Add(2)
		go func() {
			defer workers.Done()
			<-start
			_, _ = session.Close(context.Background())
		}()
		go func() {
			defer workers.Done()
			<-start
			_, _ = session.Cancel(context.Background())
		}()
		close(start)
		workers.Wait()

		rpc.mu.Lock()
		terminalCalls := rpc.closeCalls + rpc.cancelCalls
		rpc.mu.Unlock()
		if terminalCalls != 1 {
			t.Fatalf("iteration %d terminal calls = %d, want 1", iteration, terminalCalls)
		}
		if err := pool.Close(); err != nil {
			t.Fatal(err)
		}
	}
}

func TestSessionApplyCloseRaceNeverAppliesAfterClose(t *testing.T) {
	for iteration := 0; iteration < 50; iteration++ {
		pooled, pool := newTestPooled(t)
		rpc := &fakeIngestRPC{startID: 1}
		pooled.dial = func(context.Context, transportpool.Key) (io.Closer, error) {
			return &fakeIngestTransport{rpc: rpc}, nil
		}
		session, err := pooled.Start(context.Background(), "tablet-1:9997", DurabilityDefault)
		if err != nil {
			t.Fatal(err)
		}

		start := make(chan struct{})
		var workers sync.WaitGroup
		workers.Add(2)
		go func() {
			defer workers.Done()
			<-start
			_ = session.Apply(context.Background(), testExtent(), testMutations())
		}()
		go func() {
			defer workers.Done()
			<-start
			_, _ = session.Close(context.Background())
		}()
		close(start)
		workers.Wait()

		calls := rpc.calls()
		for index := range calls {
			if calls[index] == "close" && index+1 < len(calls) && calls[index+1] == "apply" {
				t.Fatalf("iteration %d RPC calls = %v", iteration, calls)
			}
		}
		if err := pool.Close(); err != nil {
			t.Fatal(err)
		}
	}
}

func TestPooledCloseRacesStart(t *testing.T) {
	pooled, pool := newTestPooled(t)
	defer pool.Close()
	pooled.dial = func(context.Context, transportpool.Key) (io.Closer, error) {
		return &fakeIngestTransport{rpc: &fakeIngestRPC{startID: 1}}, nil
	}

	start := make(chan struct{})
	var workers sync.WaitGroup
	for range 32 {
		workers.Add(1)
		go func() {
			defer workers.Done()
			<-start
			session, err := pooled.Start(
				context.Background(),
				"tablet-1:9997",
				DurabilityDefault,
			)
			if err == nil {
				_, _ = session.Cancel(context.Background())
			} else if !errors.Is(err, ErrClosed) {
				t.Errorf("Start error = %v, want nil or ErrClosed", err)
			}
		}()
	}
	workers.Add(1)
	go func() {
		defer workers.Done()
		<-start
		_ = pooled.Close()
	}()
	close(start)
	workers.Wait()
}

func newTestPooled(t *testing.T) (*Pooled, *transportpool.Pool) {
	t.Helper()
	pool, err := transportpool.New(transportpool.Config{MaxIdlePerEndpoint: 1})
	if err != nil {
		t.Fatal(err)
	}
	pooled, err := NewPooled(
		pool,
		"uuid-1",
		"4.0.0-SNAPSHOT",
		&security.TCredentials{Principal: "root", Token: []byte("secret")},
		0,
	)
	if err != nil {
		t.Fatal(err)
	}
	pooled.newClient = func(transport io.Closer) (ingestRPC, error) {
		return transport.(*fakeIngestTransport).rpc, nil
	}
	return pooled, pool
}

func testExtent() *data.TKeyExtent {
	return &data.TKeyExtent{Table: []byte("1")}
}

func testMutations() []*data.TMutation {
	return []*data.TMutation{{Row: []byte("row"), Entries: 1}}
}

type fakeIngestTransport struct {
	rpc    *fakeIngestRPC
	closes atomic.Int32
}

func (t *fakeIngestTransport) Close() error {
	t.closes.Add(1)
	return nil
}

type fakeIngestRPC struct {
	mu sync.Mutex

	startID   data.UpdateID
	startErr  error
	applyErr  error
	closeErr  error
	cancelErr error

	order       []string
	closeCalls  int
	cancelCalls int
	startToken  []byte
}

func (r *fakeIngestRPC) Start(
	_ context.Context,
	credentials *security.TCredentials,
	_ Durability,
) (data.UpdateID, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.order = append(r.order, "start")
	r.startToken = append([]byte(nil), credentials.Token...)
	return r.startID, r.startErr
}

func (r *fakeIngestRPC) startCredentialToken() []byte {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]byte(nil), r.startToken...)
}

func (r *fakeIngestRPC) Apply(
	context.Context,
	data.UpdateID,
	*data.TKeyExtent,
	[]*data.TMutation,
) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.order = append(r.order, "apply")
	return r.applyErr
}

func (r *fakeIngestRPC) Close(
	context.Context,
	data.UpdateID,
) (*data.UpdateErrors, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.order = append(r.order, "close")
	r.closeCalls++
	return &data.UpdateErrors{}, r.closeErr
}

func (r *fakeIngestRPC) Cancel(context.Context, data.UpdateID) (bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.order = append(r.order, "cancel")
	r.cancelCalls++
	return true, r.cancelErr
}

func (r *fakeIngestRPC) calls() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.order...)
}

func equalStrings(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}
