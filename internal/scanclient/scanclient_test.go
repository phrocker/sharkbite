package scanclient

import (
	"context"
	"errors"
	"io"
	"net"
	"strings"
	"testing"

	"github.com/phrocker/shoal-oss/internal/thrift/gen/data"
	"github.com/phrocker/shoal-oss/internal/thrift/gen/security"
)

func TestDial_RejectsEmptyArgs(t *testing.T) {
	cases := []struct {
		name, addr, instance, version, want string
	}{
		{"no addr", "", "uuid", "4.0.0", "empty addr"},
		{"no instance", "host:9997", "", "4.0.0", "empty instanceID"},
		{"no version", "host:9997", "uuid", "", "empty accumuloVersion"},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := Dial(c.addr, c.instance, c.version)
			if err == nil {
				t.Fatal("expected error, got nil")
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Errorf("error = %v, want substring %q", err, c.want)
			}
		})
	}
}

func TestSimpleScanReturnsResultAndCloseFailure(t *testing.T) {
	closeErr := errors.New("close failed")
	want := &data.InitialScan{ScanID: 23}
	c := &Client{rpc: &fakeScanRPC{
		startResult: want,
		closeErr:    closeErr,
	}}

	got, err := c.SimpleScan(context.Background(), SimpleScanRequest{
		Credentials: &security.TCredentials{},
		Extent:      &data.TKeyExtent{},
		Range:       &data.TRange{},
	})
	if got != want {
		t.Fatalf("result = %p, want %p", got, want)
	}

	if !errors.Is(err, closeErr) {
		t.Fatalf("error = %v, want close failure", err)
	}
	var cleanupErr *CleanupError
	if !errors.As(err, &cleanupErr) {
		t.Fatalf("error type = %T, want *CleanupError", err)
	}
	if cleanupErr.ScanID != want.ScanID {
		t.Fatalf("cleanup scan ID = %d, want %d", cleanupErr.ScanID, want.ScanID)
	}
}

func TestSimpleScanDrainsStatefulContinuation(t *testing.T) {
	rpc := &fakeScanRPC{
		startResult: &data.InitialScan{
			ScanID: 23,
			Result_: &data.ScanResult_{
				Results: []*data.TKeyValue{{Value: []byte("first")}},
				More:    true,
			},
		},
		continueResult: &data.ScanResult_{
			Results: []*data.TKeyValue{{Value: []byte("second")}},
		},
	}
	got, err := (&Client{rpc: rpc}).SimpleScan(context.Background(), SimpleScanRequest{
		Credentials: &security.TCredentials{},
		Extent:      &data.TKeyExtent{},
		Range:       &data.TRange{},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got.ScanID != 0 || got.Result_.More || len(got.Result_.Results) != 2 {
		t.Fatalf("drained result = %#v", got)
	}
	if rpc.continueCalls.Load() != 1 || rpc.closeCalls.Load() != 1 {
		t.Fatalf("continue/close calls = %d/%d", rpc.continueCalls.Load(), rpc.closeCalls.Load())
	}
}

func TestDial_RejectsUnreachableAddr(t *testing.T) {
	// 127.0.0.1:1 — reserved low port that should be unreachable.
	_, err := Dial("127.0.0.1:1", "uuid", "4.0.0")
	if err == nil {
		t.Fatal("expected error connecting to 127.0.0.1:1, got nil")
	}
	if !strings.Contains(err.Error(), "open transport") {
		t.Errorf("error = %v, want substring %q", err, "open transport")
	}
}

func TestDialTransportClosesConnectionCanceledAfterDial(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	clientConn, serverConn := net.Pipe()
	t.Cleanup(func() { _ = serverConn.Close() })

	transport, err := dialTransportWith(
		ctx,
		"tablet-1:9997",
		func(context.Context, string, string) (net.Conn, error) {
			cancel()
			return clientConn, nil
		},
	)
	if transport != nil {
		t.Fatalf("transport = %T, want nil", transport)
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want context.Canceled", err)
	}

	buf := make([]byte, 1)
	if _, err := serverConn.Read(buf); !errors.Is(err, io.EOF) {
		t.Fatalf("peer read error = %v, want io.EOF after client close", err)
	}
}

func TestSimpleScan_RejectsNilFields(t *testing.T) {
	// Construct a Client with a nil transport just to exercise the
	// validation path. We never reach the wire so transport-nil is fine.
	c := &Client{}

	cases := []struct {
		name string
		req  SimpleScanRequest
		want string
	}{
		{"nil credentials", SimpleScanRequest{Extent: &data.TKeyExtent{}, Range: &data.TRange{}}, "nil Credentials"},
		{"nil extent", SimpleScanRequest{Credentials: &security.TCredentials{}, Range: &data.TRange{}}, "nil Extent"},
		{"nil range", SimpleScanRequest{Credentials: &security.TCredentials{}, Extent: &data.TKeyExtent{}}, "nil Range"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := c.SimpleScan(context.Background(), tc.req)
			if err == nil {
				t.Fatal("expected validation error, got nil")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %v, want substring %q", err, tc.want)
			}
		})
	}
}

func TestStartMulti_RejectsNilFields(t *testing.T) {
	c := &Client{}

	cases := []struct {
		name string
		req  MultiStartRequest
		want string
	}{
		{"nil credentials", MultiStartRequest{Batch: data.ScanBatch{}}, "nil Credentials"},
		{"nil batch", MultiStartRequest{Credentials: &security.TCredentials{}}, "nil Batch"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := c.StartMulti(context.Background(), tc.req)
			if err == nil {
				t.Fatal("expected validation error, got nil")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %v, want substring %q", err, tc.want)
			}
		})
	}
}
