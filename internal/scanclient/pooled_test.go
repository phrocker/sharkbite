package scanclient

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

func TestPooledConstructsScanKey(t *testing.T) {
	pooled, pool := newTestPooled(t, transportpool.Config{MaxIdlePerEndpoint: 1})
	defer pool.Close()

	var got transportpool.Key
	pooled.dial = func(_ context.Context, key transportpool.Key) (io.Closer, error) {
		got = key
		return &fakePooledTransport{rpc: successfulRPC()}, nil
	}
	pooled.newClient = clientFromFakeTransport

	if _, err := pooled.Start(context.Background(), "tablet-1:9997", validStartRequest()); err != nil {
		t.Fatal(err)
	}
	want := transportpool.Key{
		Address:         "tablet-1:9997",
		Service:         "scan",
		InstanceID:      "uuid-1",
		ProtocolVersion: "4.0.0-SNAPSHOT",
	}
	if got != want {
		t.Fatalf("key = %+v, want %+v", got, want)
	}
}

func TestPooledUpdateCredentialsCopiesReplacementForNextRPCAndRejectsClosed(t *testing.T) {
	pooled, pool := newTestPooled(t, transportpool.Config{MaxIdlePerEndpoint: 1})
	defer pool.Close()

	rpc := successfulRPC()
	pooled.dial = func(context.Context, transportpool.Key) (io.Closer, error) {
		return &fakePooledTransport{rpc: rpc}, nil
	}
	pooled.newClient = clientFromFakeTransport

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
	if _, err := pooled.Start(context.Background(), "tablet-1:9997", validStartRequest()); err != nil {
		t.Fatal(err)
	}
	if got := rpc.startCredentialToken(); string(got) != "replacement" || string(got) == "secret" {
		t.Fatalf("RPC token = %q, want isolated replacement", got)
	}
	if err := pooled.Close(); err != nil {
		t.Fatal(err)
	}
	if err := pooled.UpdateCredentials(&security.TCredentials{Token: []byte("later")}); err == nil ||
		err.Error() != "scanclient: pooled client is closed" {
		t.Fatalf("closed update error = %v", err)
	}
}

func TestPooledReusesTransportAcrossLifecycle(t *testing.T) {
	pooled, pool := newTestPooled(t, transportpool.Config{MaxIdlePerEndpoint: 1})
	defer pool.Close()

	var dials atomic.Int32
	rpc := &fakeScanRPC{
		startResult:    &data.InitialScan{ScanID: 41},
		continueResult: &data.ScanResult_{More: true},
	}
	pooled.dial = func(context.Context, transportpool.Key) (io.Closer, error) {
		dials.Add(1)
		return &fakePooledTransport{rpc: rpc}, nil
	}
	pooled.newClient = clientFromFakeTransport

	initial, err := pooled.Start(context.Background(), "tablet-1:9997", validStartRequest())
	if err != nil {
		t.Fatal(err)
	}
	if initial.ScanID != 41 {
		t.Fatalf("scan ID = %d, want 41", initial.ScanID)
	}
	batch, err := pooled.Continue(context.Background(), "tablet-1:9997", initial.ScanID, 17)
	if err != nil {
		t.Fatal(err)
	}
	if batch == nil || !batch.More {
		t.Fatalf("continue result = %+v, want More", batch)
	}
	if err := pooled.CloseScan(context.Background(), "tablet-1:9997", initial.ScanID); err != nil {
		t.Fatal(err)
	}
	if dials.Load() != 1 {
		t.Fatalf("dials = %d, want 1", dials.Load())
	}
	if rpc.startCalls.Load() != 1 || rpc.continueCalls.Load() != 1 || rpc.closeCalls.Load() != 1 {
		t.Fatalf(
			"calls start/continue/close = %d/%d/%d, want 1/1/1",
			rpc.startCalls.Load(),
			rpc.continueCalls.Load(),
			rpc.closeCalls.Load(),
		)
	}
}

func TestPooledReusesTransportAcrossMultiLifecycle(t *testing.T) {
	pooled, pool := newTestPooled(t, transportpool.Config{MaxIdlePerEndpoint: 1})
	defer pool.Close()

	var dials atomic.Int32
	rpc := &fakeScanRPC{
		multiStartResult:    &data.InitialMultiScan{ScanID: 51},
		multiContinueResult: &data.MultiScanResult_{More: true},
	}
	pooled.dial = func(context.Context, transportpool.Key) (io.Closer, error) {
		dials.Add(1)
		return &fakePooledTransport{rpc: rpc}, nil
	}
	pooled.newClient = clientFromFakeTransport

	initial, err := pooled.StartMulti(context.Background(), "tablet-1:9997", validMultiStartRequest())
	if err != nil {
		t.Fatal(err)
	}
	if initial.ScanID != 51 {
		t.Fatalf("scan ID = %d, want 51", initial.ScanID)
	}
	batch, err := pooled.ContinueMulti(context.Background(), "tablet-1:9997", initial.ScanID, 17)
	if err != nil {
		t.Fatal(err)
	}
	if batch == nil || !batch.More {
		t.Fatalf("continue result = %+v, want More", batch)
	}
	if err := pooled.CloseMultiScan(context.Background(), "tablet-1:9997", initial.ScanID); err != nil {
		t.Fatal(err)
	}
	if dials.Load() != 1 {
		t.Fatalf("dials = %d, want 1", dials.Load())
	}
	if rpc.multiStartCalls.Load() != 1 ||
		rpc.multiContinueCalls.Load() != 1 ||
		rpc.multiCloseCalls.Load() != 1 {
		t.Fatalf(
			"multi calls start/continue/close = %d/%d/%d, want 1/1/1",
			rpc.multiStartCalls.Load(),
			rpc.multiContinueCalls.Load(),
			rpc.multiCloseCalls.Load(),
		)
	}
}

func TestPooledInvalidatesWireFailures(t *testing.T) {
	tests := []struct {
		name string
		err  error
	}{
		{
			name: "transport",
			err:  thrift.NewTTransportExceptionFromError(errors.New("connection reset")),
		},
		{
			name: "protocol",
			err:  thrift.NewTProtocolException(errors.New("bad frame")),
		},
		{
			name: "unknown thrift",
			err:  thrift.WrapTException(errors.New("rpc exchange failed")),
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			pooled, pool := newTestPooled(t, transportpool.Config{MaxIdlePerEndpoint: 1})
			defer pool.Close()

			var dials atomic.Int32
			var first *fakePooledTransport
			pooled.dial = func(context.Context, transportpool.Key) (io.Closer, error) {
				n := dials.Add(1)
				rpc := successfulRPC()
				if n == 1 {
					rpc.startErr = tt.err
				}
				transport := &fakePooledTransport{rpc: rpc}
				if n == 1 {
					first = transport
				}
				return transport, nil
			}
			pooled.newClient = clientFromFakeTransport

			if _, err := pooled.Start(context.Background(), "tablet-1:9997", validStartRequest()); err == nil {
				t.Fatal("expected wire error")
			}
			if first.closes.Load() != 1 {
				t.Fatalf("invalidated transport closes = %d, want 1", first.closes.Load())
			}
			if _, err := pooled.Start(context.Background(), "tablet-1:9997", validStartRequest()); err != nil {
				t.Fatal(err)
			}
			if dials.Load() != 2 {
				t.Fatalf("dials = %d, want 2", dials.Load())
			}
		})
	}
}

func TestPooledChecksCancellationBeforeRPCs(t *testing.T) {
	pooled, pool := newTestPooled(t, transportpool.Config{MaxIdlePerEndpoint: 1})
	defer pool.Close()

	var dials atomic.Int32
	rpc := successfulRPC()
	pooled.dial = func(context.Context, transportpool.Key) (io.Closer, error) {
		dials.Add(1)
		return &fakePooledTransport{rpc: rpc}, nil
	}
	pooled.newClient = clientFromFakeTransport

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := pooled.Start(ctx, "tablet-1:9997", validStartRequest()); !errors.Is(err, context.Canceled) {
		t.Fatalf("Start error = %v, want context.Canceled", err)
	}
	if _, err := pooled.Continue(ctx, "tablet-1:9997", 1, 0); !errors.Is(err, context.Canceled) {
		t.Fatalf("Continue error = %v, want context.Canceled", err)
	}
	if err := pooled.CloseScan(ctx, "tablet-1:9997", 1); !errors.Is(err, context.Canceled) {
		t.Fatalf("CloseScan error = %v, want context.Canceled", err)
	}
	if _, err := pooled.StartMulti(ctx, "tablet-1:9997", validMultiStartRequest()); !errors.Is(err, context.Canceled) {
		t.Fatalf("StartMulti error = %v, want context.Canceled", err)
	}
	if _, err := pooled.ContinueMulti(ctx, "tablet-1:9997", 1, 0); !errors.Is(err, context.Canceled) {
		t.Fatalf("ContinueMulti error = %v, want context.Canceled", err)
	}
	if err := pooled.CloseMultiScan(ctx, "tablet-1:9997", 1); !errors.Is(err, context.Canceled) {
		t.Fatalf("CloseMultiScan error = %v, want context.Canceled", err)
	}
	if dials.Load() != 0 {
		t.Fatalf("dials = %d, want 0", dials.Load())
	}
	if rpc.totalCalls() != 0 {
		t.Fatalf("RPC calls = %d, want 0", rpc.totalCalls())
	}
}

func TestPooledRechecksCancellationAfterAcquisition(t *testing.T) {
	pooled, pool := newTestPooled(t, transportpool.Config{MaxIdlePerEndpoint: 1})
	defer pool.Close()

	rpc := successfulRPC()
	pooled.dial = func(context.Context, transportpool.Key) (io.Closer, error) {
		return &fakePooledTransport{rpc: rpc}, nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	var once sync.Once
	pooled.newClient = func(transport io.Closer) (scanRPC, error) {
		once.Do(cancel)
		return clientFromFakeTransport(transport)
	}

	if _, err := pooled.Continue(ctx, "tablet-1:9997", 1, 0); !errors.Is(err, context.Canceled) {
		t.Fatalf("Continue error = %v, want context.Canceled", err)
	}
	if rpc.continueCalls.Load() != 0 {
		t.Fatalf("continue calls = %d, want 0", rpc.continueCalls.Load())
	}
}

func TestPooledCloseScanSurfacesRPCFailureAndKeepsHealthyTransport(t *testing.T) {
	pooled, pool := newTestPooled(t, transportpool.Config{MaxIdlePerEndpoint: 1})
	defer pool.Close()

	closeErr := errors.New("close scan failed")
	var dials atomic.Int32
	rpc := successfulRPC()
	rpc.closeErr = closeErr
	pooled.dial = func(context.Context, transportpool.Key) (io.Closer, error) {
		dials.Add(1)
		return &fakePooledTransport{rpc: rpc}, nil
	}
	pooled.newClient = clientFromFakeTransport

	if err := pooled.CloseScan(context.Background(), "tablet-1:9997", 7); !errors.Is(err, closeErr) {
		t.Fatalf("CloseScan error = %v, want %v", err, closeErr)
	}
	if _, err := pooled.Start(context.Background(), "tablet-1:9997", validStartRequest()); err != nil {
		t.Fatal(err)
	}
	if dials.Load() != 1 {
		t.Fatalf("dials = %d, want application failure to preserve transport", dials.Load())
	}
}

func TestPooledKeepsTransportForApplicationException(t *testing.T) {
	pooled, pool := newTestPooled(t, transportpool.Config{MaxIdlePerEndpoint: 1})
	defer pool.Close()

	var dials atomic.Int32
	rpc := successfulRPC()
	rpc.startErr = thrift.NewTApplicationException(thrift.INTERNAL_ERROR, "server failure")
	pooled.dial = func(context.Context, transportpool.Key) (io.Closer, error) {
		dials.Add(1)
		return &fakePooledTransport{rpc: rpc}, nil
	}
	pooled.newClient = clientFromFakeTransport

	if _, err := pooled.Start(context.Background(), "tablet-1:9997", validStartRequest()); err == nil {
		t.Fatal("expected application exception")
	}
	rpc.startErr = nil
	if _, err := pooled.Start(context.Background(), "tablet-1:9997", validStartRequest()); err != nil {
		t.Fatal(err)
	}
	if dials.Load() != 1 {
		t.Fatalf("dials = %d, want application exception to preserve transport", dials.Load())
	}
}

func TestPooledPreservesResultWhenLeaseCleanupFails(t *testing.T) {
	pooled, pool := newTestPooled(t, transportpool.Config{})
	defer pool.Close()

	cleanupErr := errors.New("transport close failed")
	want := &data.InitialScan{ScanID: 19}
	pooled.dial = func(context.Context, transportpool.Key) (io.Closer, error) {
		return &fakePooledTransport{
			rpc:      &fakeScanRPC{startResult: want},
			closeErr: cleanupErr,
		}, nil
	}
	pooled.newClient = clientFromFakeTransport

	got, err := pooled.Start(context.Background(), "tablet-1:9997", validStartRequest())
	if got != want {
		t.Fatalf("result = %p, want %p", got, want)
	}
	if !errors.Is(err, cleanupErr) {
		t.Fatalf("error = %v, want cleanup error", err)
	}
}

func newTestPooled(t *testing.T, config transportpool.Config) (*Pooled, *transportpool.Pool) {
	t.Helper()
	pool, err := transportpool.New(config)
	if err != nil {
		t.Fatal(err)
	}
	pooled, err := NewPooled(
		pool,
		"uuid-1",
		"4.0.0-SNAPSHOT",
		&security.TCredentials{
			Principal:      "root",
			TokenClassName: "PasswordToken",
			Token:          []byte("secret"),
			InstanceId:     "uuid-1",
		},
		0,
	)
	if err != nil {
		t.Fatal(err)
	}
	return pooled, pool
}

func validStartRequest() StartRequest {
	return StartRequest{
		Extent: &data.TKeyExtent{},
		Range:  &data.TRange{},
	}
}

func validMultiStartRequest() MultiStartRequest {
	return MultiStartRequest{
		Batch: data.ScanBatch{
			&data.TKeyExtent{}: []*data.TRange{{}},
		},
	}
}

func successfulRPC() *fakeScanRPC {
	return &fakeScanRPC{
		startResult:         &data.InitialScan{ScanID: 1},
		continueResult:      &data.ScanResult_{},
		multiStartResult:    &data.InitialMultiScan{ScanID: 2},
		multiContinueResult: &data.MultiScanResult_{},
	}
}

type fakePooledTransport struct {
	rpc      scanRPC
	closeErr error
	closes   atomic.Int32
}

func (t *fakePooledTransport) Close() error {
	t.closes.Add(1)
	return t.closeErr
}

func clientFromFakeTransport(transport io.Closer) (scanRPC, error) {
	return transport.(*fakePooledTransport).rpc, nil
}

type fakeScanRPC struct {
	startResult         *data.InitialScan
	startErr            error
	multiStartResult    *data.InitialMultiScan
	multiStartErr       error
	continueResult      *data.ScanResult_
	continueErr         error
	multiContinueResult *data.MultiScanResult_
	multiContinueErr    error
	closeErr            error
	multiCloseErr       error
	startCalls          atomic.Int32
	multiStartCalls     atomic.Int32
	continueCalls       atomic.Int32
	multiContinueCalls  atomic.Int32
	closeCalls          atomic.Int32
	multiCloseCalls     atomic.Int32
	credentialsMu       sync.Mutex
	startToken          []byte
}

func (c *fakeScanRPC) Start(_ context.Context, req StartRequest) (*data.InitialScan, error) {
	c.startCalls.Add(1)
	c.credentialsMu.Lock()
	c.startToken = append([]byte(nil), req.Credentials.Token...)
	c.credentialsMu.Unlock()
	return c.startResult, c.startErr
}

func (c *fakeScanRPC) startCredentialToken() []byte {
	c.credentialsMu.Lock()
	defer c.credentialsMu.Unlock()
	return append([]byte(nil), c.startToken...)
}

func (c *fakeScanRPC) StartMulti(
	context.Context,
	MultiStartRequest,
) (*data.InitialMultiScan, error) {
	c.multiStartCalls.Add(1)
	return c.multiStartResult, c.multiStartErr
}

func (c *fakeScanRPC) Continue(
	context.Context,
	data.ScanID,
	int64,
) (*data.ScanResult_, error) {
	c.continueCalls.Add(1)
	return c.continueResult, c.continueErr
}

func (c *fakeScanRPC) ContinueMulti(
	context.Context,
	data.ScanID,
	int64,
) (*data.MultiScanResult_, error) {
	c.multiContinueCalls.Add(1)
	return c.multiContinueResult, c.multiContinueErr
}

func (c *fakeScanRPC) Close(context.Context, data.ScanID) error {
	c.closeCalls.Add(1)
	return c.closeErr
}

func (c *fakeScanRPC) CloseMulti(context.Context, data.ScanID) error {
	c.multiCloseCalls.Add(1)
	return c.multiCloseErr
}

func (c *fakeScanRPC) totalCalls() int32 {
	return c.startCalls.Load() +
		c.multiStartCalls.Load() +
		c.continueCalls.Load() +
		c.multiContinueCalls.Load() +
		c.closeCalls.Load() +
		c.multiCloseCalls.Load()
}
