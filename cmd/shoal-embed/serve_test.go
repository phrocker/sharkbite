// Licensed to the Apache Software Foundation (ASF) under one
// or more contributor license agreements.  See the NOTICE file
// distributed with this work for additional information
// regarding copyright ownership.  The ASF licenses this file
// to you under the Apache License, Version 2.0 (the
// "License"); you may not use this file except in compliance
// with the License.  You may obtain a copy of the License at
//
//   https://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing,
// software distributed under the License is distributed on an
// "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY
// KIND, either express or implied.  See the License for the
// specific language governing permissions and limitations
// under the License.

package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"

	"github.com/phrocker/shoal-oss/internal/agentmem"
	"github.com/phrocker/shoal-oss/internal/embedpb"
)

// newRawTestServeHandle starts a serveHandle bound to OS-assigned loopback
// ports without starting its accept loops. Most tests want
// newTestServeHandle instead; this exists for tests (RunUntilSignal's) that
// need to drive Serve indirectly via a method that calls it internally,
// where calling Serve a second time here too would race it.
func newRawTestServeHandle(t *testing.T) *serveHandle {
	t.Helper()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	h, err := startServe(serveConfig{
		DataDir:        t.TempDir(),
		GRPCAddress:    "127.0.0.1:0",
		MetricsAddress: "127.0.0.1:0",
		Logger:         logger,
	})
	if err != nil {
		t.Fatalf("startServe: %v", err)
	}
	return h
}

// newTestServeHandle starts a serveHandle bound to OS-assigned loopback
// ports and runs its accept loops in the background, returning the handle
// plus the channel Serve's return value will arrive on. It performs no
// teardown, so callers control the Drain/Stop sequence themselves.
func newTestServeHandle(t *testing.T) (*serveHandle, <-chan error) {
	t.Helper()
	h := newRawTestServeHandle(t)
	serveErrCh := make(chan error, 1)
	go func() { serveErrCh <- h.Serve() }()
	return h, serveErrCh
}

// startTestServe starts a serveHandle and arranges for it to be drained and
// stopped when the test completes, for tests that only need a running
// server and don't themselves exercise the Drain/Stop sequence.
func startTestServe(t *testing.T) *serveHandle {
	t.Helper()
	h, serveErrCh := newTestServeHandle(t)
	t.Cleanup(func() {
		h.Drain()
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := h.Stop(ctx); err != nil {
			t.Errorf("Stop(ctx) = %v, want nil during cleanup", err)
		}
		select {
		case <-serveErrCh:
		case <-time.After(5 * time.Second):
			t.Error("Serve did not return within 5s of Stop")
		}
	})
	return h
}

// waitForStatus polls url until it returns wantStatus or the deadline
// elapses, so tests don't race the Serve goroutine's accept loop startup.
func waitForStatus(t *testing.T, url string, wantStatus int) *http.Response {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	var lastErr error
	for time.Now().Before(deadline) {
		resp, err := http.Get(url)
		if err != nil {
			lastErr = err
			time.Sleep(20 * time.Millisecond)
			continue
		}
		if resp.StatusCode == wantStatus {
			return resp
		}
		resp.Body.Close()
		lastErr = fmt.Errorf("got status %d", resp.StatusCode)
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("GET %s never returned %d: %v", url, wantStatus, lastErr)
	return nil
}

func waitForStatusRPC(t *testing.T, addr string) (*grpc.ClientConn, embedpb.ShoalEmbedClient) {
	t.Helper()
	conn, err := grpc.NewClient(addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("grpc.NewClient: %v", err)
	}
	client := embedpb.NewShoalEmbedClient(conn)
	deadline := time.Now().Add(5 * time.Second)
	var lastErr error
	for time.Now().Before(deadline) {
		ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
		_, err := client.Status(ctx, &embedpb.StatusRequest{})
		cancel()
		if err == nil {
			return conn, client
		}
		lastErr = err
		time.Sleep(20 * time.Millisecond)
	}
	conn.Close()
	t.Fatalf("Status RPC to %s never succeeded: %v", addr, lastErr)
	return nil, nil
}

type blockingUnaryInterceptor struct {
	enabled atomic.Bool
	entered chan struct{}
	release chan struct{}
}

func newBlockingUnaryInterceptor() *blockingUnaryInterceptor {
	return &blockingUnaryInterceptor{
		entered: make(chan struct{}, 1),
		release: make(chan struct{}),
	}
}

func (b *blockingUnaryInterceptor) intercept(ctx context.Context, req any, _ *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
	if b.enabled.Load() {
		select {
		case b.entered <- struct{}{}:
		default:
		}
		select {
		case <-b.release:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	return handler(ctx, req)
}

func TestStartServeReadyHealthAndMetrics(t *testing.T) {
	h := startTestServe(t)

	resp := waitForStatus(t, "http://"+h.MetricsAddr+"/readyz", http.StatusOK)
	resp.Body.Close()

	resp, err := http.Get("http://" + h.MetricsAddr + "/healthz")
	if err != nil {
		t.Fatalf("GET /healthz: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("/healthz = %d, want 200", resp.StatusCode)
	}

	resp, err = http.Get("http://" + h.MetricsAddr + "/metrics")
	if err != nil {
		t.Fatalf("GET /metrics: %v", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read /metrics body: %v", err)
	}
	if !strings.Contains(string(body), "shoal_tables") {
		t.Errorf("/metrics missing shoal_tables gauge:\n%s", body)
	}

	conn, err := grpc.NewClient(h.GRPCAddr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("grpc.NewClient: %v", err)
	}
	defer conn.Close()
	client := embedpb.NewShoalEmbedClient(conn)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := client.Status(ctx, &embedpb.StatusRequest{}); err != nil {
		t.Fatalf("Status RPC: %v", err)
	}
}

func TestStartServeWithoutMetricsListener(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	h, err := startServe(serveConfig{
		DataDir:     t.TempDir(),
		GRPCAddress: "127.0.0.1:0",
		Logger:      logger,
	})
	if err != nil {
		t.Fatalf("startServe: %v", err)
	}
	if h.MetricsAddr != "" {
		t.Fatalf("MetricsAddr = %q, want empty when observability listener is disabled", h.MetricsAddr)
	}

	serveErrCh := make(chan error, 1)
	go func() { serveErrCh <- h.Serve() }()

	conn, err := grpc.NewClient(h.GRPCAddr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("grpc.NewClient: %v", err)
	}
	defer conn.Close()
	client := embedpb.NewShoalEmbedClient(conn)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := client.Status(ctx, &embedpb.StatusRequest{}); err != nil {
		t.Fatalf("Status RPC: %v", err)
	}

	h.Drain()
	stopCtx, stopCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer stopCancel()
	if err := h.Stop(stopCtx); err != nil {
		t.Fatalf("Stop(ctx) = %v, want nil", err)
	}

	select {
	case err := <-serveErrCh:
		if err != nil {
			t.Fatalf("Serve() returned %v, want nil after Stop", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Serve did not return within 5s of Stop")
	}
}

func TestServeHandleDrainThenStop(t *testing.T) {
	h, serveErrCh := newTestServeHandle(t)

	// Ready before draining.
	resp := waitForStatus(t, "http://"+h.MetricsAddr+"/readyz", http.StatusOK)
	resp.Body.Close()

	// Drain flips readiness without closing the listeners: the endpoint
	// stays reachable (an orchestrator's readiness probe must still be able
	// to observe the not-ready state) but now reports not-ready.
	h.Drain()
	resp, err := http.Get("http://" + h.MetricsAddr + "/readyz")
	if err != nil {
		t.Fatalf("GET /readyz after Drain: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("/readyz after Drain = %d, want 503 (not ready but still reachable)", resp.StatusCode)
	}

	// Stop closes the listeners and the engine.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := h.Stop(ctx); err != nil {
		t.Fatalf("Stop(ctx) = %v, want nil", err)
	}

	select {
	case err := <-serveErrCh:
		if err != nil {
			t.Errorf("Serve() returned %v, want nil after a clean Stop", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Serve did not return within 5s of Stop")
	}

	if _, err := http.Get("http://" + h.MetricsAddr + "/readyz"); err == nil {
		t.Error("GET /readyz succeeded after Stop, want connection error (listener closed)")
	}
}

// startStuckStreamingScan starts a real streaming Scan RPC against h, using
// a client pinned to a small static (non-BDP-adaptive) flow-control
// window, and deliberately stops reading after the first message so the
// still-running server-side handler blocks on send backpressure trying to
// deliver the rest — a real in-flight, non-cancellable-by-closing-the-
// connection-alone RPC that only unblocks once the connection itself is
// torn down (e.g. by conn.Close, or by the server force-stopping). Callers
// use this to exercise Stop's/RunUntilSignal's force-path deterministically
// instead of racing a real slow client.
func startStuckStreamingScan(t *testing.T, h *serveHandle) {
	t.Helper()
	const staticWindow = 128 * 1024 // bytes; disables BDP-based auto-growth.
	conn, err := grpc.NewClient(h.GRPCAddr,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithInitialWindowSize(staticWindow),
		grpc.WithInitialConnWindowSize(staticWindow),
	)
	if err != nil {
		t.Fatalf("grpc.NewClient: %v", err)
	}
	t.Cleanup(func() { conn.Close() })
	client := embedpb.NewShoalEmbedClient(conn)

	setupCtx, setupCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer setupCancel()
	if _, err := client.CreateTable(setupCtx, &embedpb.CreateTableRequest{Table: "t"}); err != nil {
		t.Fatalf("CreateTable: %v", err)
	}

	// rows*valueSize is several times staticWindow, so once the client stops
	// reading, the handler's remaining Send calls block on flow control
	// well before finishing — a real in-flight RPC, not a race.
	const rows, valueSize = 100, 8192
	value := []byte(strings.Repeat("v", valueSize))
	muts := make([]*embedpb.Mutation, rows)
	for i := 0; i < rows; i++ {
		muts[i] = &embedpb.Mutation{
			Row: []byte(fmt.Sprintf("row-%04d", i)),
			Entries: []*embedpb.Entry{{
				ColumnFamily:    []byte("cf"),
				ColumnQualifier: []byte("cq"),
				Timestamp:       1,
				Value:           value,
			}},
		}
	}
	if _, err := client.Write(setupCtx, &embedpb.WriteRequest{Table: "t", Mutations: muts}); err != nil {
		t.Fatalf("Write: %v", err)
	}

	scanCtx, scanCancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(scanCancel)
	stream, err := client.Scan(scanCtx, &embedpb.ScanRequest{Table: "t", BatchSize: 1})
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if _, err := stream.Recv(); err != nil {
		t.Fatalf("first Scan Recv: %v", err)
	}
	// Deliberately never call Recv again: give the still-running handler
	// goroutine time to fill the pinned window and block in Send.
	time.Sleep(200 * time.Millisecond)
}

func TestVectorScanRequiresVersionedRPC(t *testing.T) {
	h := startTestServe(t)
	conn, err := grpc.NewClient(
		h.GRPCAddr,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	client := embedpb.NewShoalEmbedClient(conn)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	const identity = "test:model:v1:1:normalized"
	if _, err := client.CreateTable(ctx, &embedpb.CreateTableRequest{
		Table: "vectors", DefaultEmbedding: "has_embeddings:" + identity,
	}); status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("legacy embedding CreateTable error = %v", err)
	}
	if _, err := client.CreateTableV2(ctx, &embedpb.CreateTableRequest{
		Table: "vectors", DefaultEmbedding: "has_embeddings:" + identity,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := client.Write(ctx, &embedpb.WriteRequest{
		Table: "vectors",
		Mutations: []*embedpb.Mutation{{
			Row: []byte("row"),
			Entries: []*embedpb.Entry{{
				ColumnFamily: []byte("vec"),
				Value:        agentmem.PackVector([]float32{1}),
			}},
		}},
	}); err != nil {
		t.Fatal(err)
	}
	request := &embedpb.ScanRequest{
		Table: "vectors",
		VectorSearch: &embedpb.VectorSearch{
			Query:          agentmem.PackVector([]float32{1}),
			EmbeddingCf:    []byte("vec"),
			EmbeddingSpace: identity,
		},
	}
	legacy, err := client.Scan(ctx, request)
	if err == nil {
		_, err = legacy.Recv()
	}
	if status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("legacy vector Scan error = %v", err)
	}
	safe, err := client.ScanV2(ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	response, err := safe.Recv()
	if err != nil {
		t.Fatal(err)
	}
	if len(response.Cells) != 1 {
		t.Fatalf("ScanV2 cells = %d, want 1", len(response.Cells))
	}
}

// TestServeHandleStopIsBoundedByDrainTimeout guards a real bug found while
// implementing Stop: grpc.Server.GracefulStop blocks until every in-flight
// RPC finishes and accepts no deadline of its own, so a single slow or
// stuck client could otherwise hang Stop — and shutdown — forever,
// regardless of --drain-timeout. It starts a real streaming Scan RPC over a
// client pinned to a small static (non-BDP-adaptive) flow-control window,
// deliberately stops reading after the first message so the still-running
// handler blocks on backpressure trying to send the rest, then asserts Stop
// still returns close to the short deadline it was given instead of
// hanging until the client resumes reading (it never does here).
func TestServeHandleStopIsBoundedByDrainTimeout(t *testing.T) {
	h, serveErrCh := newTestServeHandle(t)
	startStuckStreamingScan(t, h)

	h.Drain()
	const drainDeadline = 150 * time.Millisecond
	stopCtx, stopCancel := context.WithTimeout(context.Background(), drainDeadline)
	defer stopCancel()

	start := time.Now()
	if err := h.Stop(stopCtx); err != nil {
		t.Fatalf("Stop(ctx) = %v, want nil for transport-cancellable streaming RPCs", err)
	}
	elapsed := time.Since(start)

	// Without the force-stop fallback this would hang until the client
	// resumed reading (it never does) or the process was killed. A bounded
	// Stop must return within a small multiple of its deadline instead.
	if elapsed > 2*time.Second {
		t.Errorf("Stop(ctx) took %v with a %v deadline; want bounded (force-stop fallback did not engage)", elapsed, drainDeadline)
	}

	select {
	case <-serveErrCh:
	case <-time.After(5 * time.Second):
		t.Fatal("Serve did not return within 5s of a forced Stop")
	}
}

// TestRunUntilSignalWaitsForFullShutdownBeforeReturning guards the critical
// bug the Copilot review's "suppressed" finding identified: the pre-fix
// main.go reacted to Serve() returning by letting main() return right
// behind it, from a goroutine that ran the drain/stop sequence but was
// never joined — so the process could exit mid-drain, before eng.Close
// ever ran. grpc.Server.Serve's return is driven entirely by the gRPC
// server's own internal bookkeeping and is independent of the HTTP side
// and of Stop's own subsequent work (waiting for the HTTP shutdown to
// finish, then closing the engine) — with an otherwise-idle gRPC server,
// Serve can return within microseconds of Stop being called. This test
// makes that gap observable: it swaps in a test-controlled HTTP handler
// that blocks a request indefinitely (via the unexported httpSrv field —
// this file is in package main), which deterministically forces Stop's
// httpSrv.Shutdown to block for the full --drain-timeout budget while the
// idle gRPC server has nothing to wait for. If RunUntilSignal returned as
// soon as Serve unblocked (the bug) rather than waiting for the whole
// shutdown sequence — including that HTTP wait and the subsequent engine
// close — to finish, it would return in microseconds instead of waiting
// out the deadline.
func TestRunUntilSignalWaitsForFullShutdownBeforeReturning(t *testing.T) {
	h := newRawTestServeHandle(t)

	orig := h.httpSrv.Handler
	requestReceived := make(chan struct{})
	release := make(chan struct{})
	var releaseOnce sync.Once
	releaseFn := func() { releaseOnce.Do(func() { close(release) }) }
	defer releaseFn()
	h.httpSrv.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/__test_slow__" {
			close(requestReceived)
			<-release
			w.WriteHeader(http.StatusOK)
			return
		}
		orig.ServeHTTP(w, r)
	})

	sigCh := make(chan os.Signal, 1)
	const drainDeadline = 150 * time.Millisecond

	runErrCh := make(chan error, 1)
	go func() { runErrCh <- h.RunUntilSignal(sigCh, 0, drainDeadline) }()

	// Wait for RunUntilSignal's internal Serve call to actually start
	// accepting before using the server or sending the signal.
	waitForStatus(t, "http://"+h.MetricsAddr+"/readyz", http.StatusOK).Body.Close()

	// Start (and deliberately never finish, until releaseFn is called) the
	// slow request so its connection stays "active" — never idle — from
	// net/http's point of view.
	slowDone := make(chan struct{})
	go func() {
		resp, err := http.Get("http://" + h.MetricsAddr + "/__test_slow__")
		if err == nil {
			resp.Body.Close()
		}
		close(slowDone)
	}()
	<-requestReceived

	start := time.Now()
	sigCh <- syscall.SIGTERM

	select {
	case err := <-runErrCh:
		elapsed := time.Since(start)
		if err != nil {
			t.Errorf("RunUntilSignal returned %v, want nil after a clean shutdown", err)
		}
		// The stuck request forces Stop's httpDone.Wait() to block until
		// ctx's drainDeadline expires and httpSrv.Close() force-closes it.
		// If RunUntilSignal returned as soon as the (otherwise idle) gRPC
		// server's Serve call unblocked (the bug) rather than waiting for
		// the whole shutdown sequence to finish, elapsed would be far
		// under drainDeadline instead.
		if elapsed < drainDeadline {
			t.Errorf("RunUntilSignal returned after %v, want at least the %v drain deadline (returned before the shutdown sequence, including engine close, finished)", elapsed, drainDeadline)
		}
	case <-time.After(5 * time.Second):
		releaseFn()
		t.Fatal("RunUntilSignal did not return within 5s of SIGTERM")
	}

	releaseFn()
	<-slowDone

	if _, err := http.Get("http://" + h.MetricsAddr + "/readyz"); err == nil {
		t.Error("GET /readyz succeeded after RunUntilSignal returned, want connection error (listener closed)")
	}
}

// TestRunUntilSignalAppliesQuiesceDelay proves the quiesce pause runs
// strictly between Drain and Stop: /readyz must already report not-ready,
// while the gRPC port must still accept a brand new connection and RPC,
// throughout the quiesce window. That ordering is what actually gives a
// readiness-polling consumer a chance to react before anything stops
// accepting work — a quiesce implemented after Stop (or not at all) would
// fail this.
func TestRunUntilSignalAppliesQuiesceDelay(t *testing.T) {
	h := newRawTestServeHandle(t)
	sigCh := make(chan os.Signal, 1)
	const quiesce = 1500 * time.Millisecond
	const drainDeadline = 5 * time.Second

	runErrCh := make(chan error, 1)
	go func() { runErrCh <- h.RunUntilSignal(sigCh, quiesce, drainDeadline) }()
	waitForStatus(t, "http://"+h.MetricsAddr+"/readyz", http.StatusOK).Body.Close()

	sigCh <- syscall.SIGTERM

	// Drain runs synchronously as soon as the signal is received, before
	// the quiesce sleep — so /readyz should flip to 503 almost
	// immediately. Poll briefly, well inside the quiesce window, rather
	// than reusing the shared 5s-deadline waitForStatus helper: a slow
	// poll here must not itself eat into the window the rest of this test
	// depends on.
	pollDeadline := time.Now().Add(250 * time.Millisecond)
	var readyzStatus int
	for time.Now().Before(pollDeadline) {
		resp, err := http.Get("http://" + h.MetricsAddr + "/readyz")
		if err == nil {
			readyzStatus = resp.StatusCode
			resp.Body.Close()
			if readyzStatus == http.StatusServiceUnavailable {
				break
			}
		}
		time.Sleep(5 * time.Millisecond)
	}
	if readyzStatus != http.StatusServiceUnavailable {
		t.Fatalf("/readyz = %d shortly after SIGTERM, want 503 (Drain runs before the quiesce sleep)", readyzStatus)
	}

	// Still well inside the quiesce window: the gRPC listener must still be
	// open and accept a brand new connection and RPC, since Stop (which
	// closes it) only runs once quiesce elapses.
	conn, err := grpc.NewClient(h.GRPCAddr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("grpc.NewClient during quiesce window: %v", err)
	}
	defer conn.Close()
	client := embedpb.NewShoalEmbedClient(conn)
	rpcCtx, rpcCancel := context.WithTimeout(context.Background(), 400*time.Millisecond)
	defer rpcCancel()
	if _, err := client.Status(rpcCtx, &embedpb.StatusRequest{}); err != nil {
		t.Errorf("Status RPC during quiesce window: %v, want gRPC port still accepting connections", err)
	}

	select {
	case err := <-runErrCh:
		if err != nil {
			t.Errorf("RunUntilSignal returned %v, want nil", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("RunUntilSignal did not return within 5s")
	}
}

func TestRunUntilSignalSkipsQuiesceWithoutMetrics(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	h, err := startServe(serveConfig{
		DataDir:     t.TempDir(),
		GRPCAddress: "127.0.0.1:0",
		Logger:      logger,
	})
	if err != nil {
		t.Fatalf("startServe: %v", err)
	}

	sigCh := make(chan os.Signal, 1)
	const quiesce = 1500 * time.Millisecond
	runErrCh := make(chan error, 1)
	go func() { runErrCh <- h.RunUntilSignal(sigCh, quiesce, 5*time.Second) }()

	conn, client := waitForStatusRPC(t, h.GRPCAddr)
	defer conn.Close()

	start := time.Now()
	sigCh <- syscall.SIGTERM

	select {
	case err := <-runErrCh:
		if err != nil {
			t.Fatalf("RunUntilSignal returned %v, want nil", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("RunUntilSignal did not return within 5s")
	}

	if elapsed := time.Since(start); elapsed >= quiesce/2 {
		t.Fatalf("RunUntilSignal took %v with the HTTP surface disabled, want it to skip the %v quiesce delay", elapsed, quiesce)
	}

	rpcCtx, rpcCancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer rpcCancel()
	if _, err := client.Status(rpcCtx, &embedpb.StatusRequest{}); err == nil {
		t.Error("Status RPC succeeded after RunUntilSignal returned, want gRPC listener closed")
	}
}

func TestServeHandleStopReturnsTimeoutErrorWithStuckUnary(t *testing.T) {
	blocker := newBlockingUnaryInterceptor()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	h, err := startServe(serveConfig{
		DataDir:          t.TempDir(),
		GRPCAddress:      "127.0.0.1:0",
		Logger:           logger,
		UnaryInterceptor: blocker.intercept,
	})
	if err != nil {
		t.Fatalf("startServe: %v", err)
	}

	closeEngineCalled := make(chan struct{}, 1)
	origCloseEngine := h.closeEngine
	h.closeEngine = func() error {
		select {
		case closeEngineCalled <- struct{}{}:
		default:
		}
		return nil
	}

	serveErrCh := make(chan error, 1)
	go func() { serveErrCh <- h.Serve() }()

	conn, client := waitForStatusRPC(t, h.GRPCAddr)
	defer conn.Close()

	blocker.enabled.Store(true)
	rpcErrCh := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_, err := client.Status(ctx, &embedpb.StatusRequest{})
		rpcErrCh <- err
	}()

	select {
	case <-blocker.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("blocking unary RPC never entered the interceptor")
	}

	h.Drain()
	stopCtx, stopCancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer stopCancel()

	start := time.Now()
	err = h.Stop(stopCtx)
	elapsed := time.Since(start)

	if elapsed > 2*time.Second {
		t.Fatalf("Stop(ctx) took %v with a %v deadline; want bounded return", elapsed, 150*time.Millisecond)
	}

	var timeoutErr *shutdownTimeoutError
	if !errors.As(err, &timeoutErr) {
		t.Fatalf("Stop(ctx) = %v, want shutdownTimeoutError", err)
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Stop(ctx) = %v, want it to unwrap context.DeadlineExceeded", err)
	}
	if timeoutErr.inFlightUnaryRPCs < 1 {
		t.Fatalf("shutdownTimeoutError.inFlightUnaryRPCs = %d, want >= 1", timeoutErr.inFlightUnaryRPCs)
	}

	select {
	case <-closeEngineCalled:
		t.Fatal("Stop(ctx) called closeEngine despite a stuck unary RPC still being in flight")
	default:
	}

	blocker.enabled.Store(false)
	transportDeadline := time.Now().Add(5 * time.Second)
	transportStopped := false
	for time.Now().Before(transportDeadline) {
		probeCtx, probeCancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
		_, probeErr := client.Status(probeCtx, &embedpb.StatusRequest{})
		probeContextErr := probeCtx.Err()
		probeCancel()
		if probeErr != nil && probeContextErr == nil {
			transportStopped = true
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !transportStopped {
		t.Fatal("transport still accepted fresh unary RPCs after Stop(ctx) returned")
	}

	close(blocker.release)

	select {
	case <-rpcErrCh:
		// Once a unary call has already been admitted, the response may win
		// the transport-teardown race even though fresh RPCs are rejected.
	case <-time.After(5 * time.Second):
		t.Fatal("blocked unary RPC did not return after release")
	}

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) && h.inFlight.count() != 0 {
		time.Sleep(10 * time.Millisecond)
	}
	if h.inFlight.count() != 0 {
		t.Fatalf("in-flight unary count stuck at %d after releasing the blocked RPC", h.inFlight.count())
	}

	select {
	case <-serveErrCh:
	case <-time.After(5 * time.Second):
		t.Fatal("Serve did not return within 5s of releasing the blocked unary RPC")
	}

	if err := origCloseEngine(); err != nil {
		t.Fatalf("cleanup engine close: %v", err)
	}
}

// TestUnaryInFlightInterceptorTracksActiveCalls is a direct unit test of
// unaryInFlight's counting logic — no real gRPC transport needed — proving
// the count reflects exactly the handlers currently running, which is the
// signal Stop's deadline-exceeded log line depends on for the Write/Flush/
// Compact case it can't otherwise detect.
func TestUnaryInFlightInterceptorTracksActiveCalls(t *testing.T) {
	var u unaryInFlight
	if got := u.count(); got != 0 {
		t.Fatalf("count() before any calls = %d, want 0", got)
	}

	handlerEntered := make(chan struct{})
	release := make(chan struct{})
	done := make(chan struct{})
	go func() {
		_, _ = u.intercept(context.Background(), nil, &grpc.UnaryServerInfo{}, func(ctx context.Context, req any) (any, error) {
			close(handlerEntered)
			<-release
			return nil, nil
		})
		close(done)
	}()

	<-handlerEntered
	if got := u.count(); got != 1 {
		t.Errorf("count() while handler is running = %d, want 1", got)
	}

	close(release)
	<-done

	if got := u.count(); got != 0 {
		t.Errorf("count() after handler returns = %d, want 0", got)
	}
}

// TestStopForceClosesHTTPOnCanceledContext guards a real bug: http.Server.
// Shutdown(ctx) returns ctx.Err() — which can be either context.
// DeadlineExceeded or context.Canceled — once ctx is done, even if active
// connections haven't finished closing gracefully on their own yet. Stop
// used to force-close the HTTP server only when that error was exactly
// context.DeadlineExceeded, so an explicitly-canceled (rather than merely
// expired) ctx left an in-flight request/connection open indefinitely
// instead of being force-closed, letting Stop return with the HTTP
// surface still up.
func TestStopForceClosesHTTPOnCanceledContext(t *testing.T) {
	h, serveErrCh := newTestServeHandle(t)
	waitForStatus(t, "http://"+h.MetricsAddr+"/readyz", http.StatusOK).Body.Close()

	orig := h.httpSrv.Handler
	requestReceived := make(chan struct{})
	release := make(chan struct{})
	var releaseOnce sync.Once
	releaseFn := func() { releaseOnce.Do(func() { close(release) }) }
	defer releaseFn()
	h.httpSrv.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/__test_slow__" {
			close(requestReceived)
			<-release
			w.WriteHeader(http.StatusOK)
			return
		}
		orig.ServeHTTP(w, r)
	})

	// A stuck request keeps an HTTP connection "active" (never idle), so
	// Shutdown cannot complete gracefully on its own and must wait for ctx
	// — forcing Stop to decide what to do when ctx ends instead of racing
	// a shutdown that would have finished on its own regardless.
	slowResultCh := make(chan error, 1)
	go func() {
		resp, err := http.Get("http://" + h.MetricsAddr + "/__test_slow__")
		if err == nil {
			resp.Body.Close()
		}
		slowResultCh <- err
	}()
	<-requestReceived

	h.Drain()
	// Already canceled, not merely time-limited — the exact case the
	// fix's ctx.Err() check (rather than an err == context.DeadlineExceeded
	// equality check) is needed for.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	stopDone := make(chan struct{})
	go func() {
		h.Stop(ctx)
		close(stopDone)
	}()

	select {
	case <-stopDone:
	case <-time.After(5 * time.Second):
		releaseFn()
		t.Fatal("Stop did not return within 5s of an already-canceled ctx")
	}

	// http.Server.Shutdown closes the listener as its very first step
	// regardless of what happens next, so a fresh connection attempt would
	// fail either way and can't distinguish the fix from the bug. What
	// only the fix does is force-close the *already-accepted, still
	// in-flight* connection: releasing the stuck handler now must not let
	// its response reach the client cleanly. Under the bug, Shutdown's
	// context.Canceled error was only logged, the connection was left
	// alone, and the client below would instead observe a normal 200.
	releaseFn()
	select {
	case err := <-slowResultCh:
		if err == nil {
			t.Error("GET /__test_slow__ completed successfully after Stop with an already-canceled ctx, want a connection error (HTTP server should have force-closed the still in-flight connection)")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("slow request goroutine did not finish within 5s of releasing the handler")
	}

	select {
	case <-serveErrCh:
	case <-time.After(5 * time.Second):
		t.Fatal("Serve did not return within 5s of a forced Stop")
	}
}

// TestStopWaitsForHTTPAcceptLoopBeforeClosingEngine guards a real gap:
// http.Server.Shutdown/Close only ask the accept-loop goroutine Serve
// started to stop — they don't reliably wait for it to have actually
// returned. Close never waits for it at all, by net/http's own design;
// Shutdown's own wait (numListeners() == 0) races that goroutine's
// startup if Stop is called early enough. Without an explicit join, Stop
// — and RunUntilSignal, which depends on Stop to fully finish — could
// report a clean shutdown, and close the engine, while that goroutine
// (and the listener it owns) were technically still running. This
// constructs a handle via newRawTestServeHandle, which deliberately never
// calls Serve, so nothing else will ever close httpServeDone on its own —
// proving Stop actually blocks on it, rather than treating it as already
// satisfied, before it will call closeEngine.
func TestStopWaitsForHTTPAcceptLoopBeforeClosingEngine(t *testing.T) {
	h := newRawTestServeHandle(t)
	t.Cleanup(func() {
		if h.httpLis != nil {
			h.httpLis.Close()
		}
	})
	if h.httpServeDone == nil {
		t.Fatal("httpServeDone is nil, want non-nil when MetricsAddress is set")
	}

	closeEngineCalled := make(chan struct{})
	h.closeEngine = func() error {
		close(closeEngineCalled)
		return nil
	}

	stopErrCh := make(chan error, 1)
	go func() { stopErrCh <- h.Stop(context.Background()) }()

	select {
	case <-closeEngineCalled:
		t.Fatal("Stop called closeEngine before httpServeDone was closed, want it to wait for the HTTP accept-loop goroutine first")
	case <-time.After(200 * time.Millisecond):
	}

	close(h.httpServeDone)

	select {
	case err := <-stopErrCh:
		if err != nil {
			t.Fatalf("Stop(ctx) = %v, want nil", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Stop did not return within 5s of httpServeDone closing")
	}

	select {
	case <-closeEngineCalled:
	default:
		t.Error("closeEngine was never called after Stop returned")
	}
}

// TestRunUntilSignalRunsFullShutdownWhenServeFailsWithoutASignal guards a
// real early-exit bug: the pre-fix RunUntilSignal only waited for the
// Drain/quiesce/Stop sequence when Serve returned nil, so a Serve failure
// with no signal involved at all returned that error immediately without
// ever draining, stopping, or closing the engine — abandoning the HTTP
// server and engine open instead of cleanly closing them before
// propagating the failure. This forces that scenario deterministically —
// grpc.Server.Serve returns grpc.ErrServerStopped immediately if the
// server was already stopped before Serve was ever invoked — rather than
// relying on a real, timing-dependent accept failure.
func TestRunUntilSignalRunsFullShutdownWhenServeFailsWithoutASignal(t *testing.T) {
	h := newRawTestServeHandle(t)
	h.grpcSrv.GracefulStop()

	sigCh := make(chan os.Signal, 1) // deliberately never signaled

	runErrCh := make(chan error, 1)
	go func() { runErrCh <- h.RunUntilSignal(sigCh, 0, 5*time.Second) }()

	select {
	case err := <-runErrCh:
		if err != grpc.ErrServerStopped {
			t.Errorf("RunUntilSignal returned %v, want grpc.ErrServerStopped (Serve's real, signal-independent failure)", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("RunUntilSignal did not return within 5s")
	}

	// The full Drain/Stop sequence must still have run even though Serve
	// failed entirely on its own: the HTTP listener, which this test
	// never touched directly, can only be closed if it did.
	if _, err := http.Get("http://" + h.MetricsAddr + "/readyz"); err == nil {
		t.Error("GET /readyz succeeded after RunUntilSignal returned, want connection error (Stop should have closed the HTTP listener even on a signal-independent Serve failure)")
	}
}

// TestRunUntilSignalReportsCleanShutdownWhenSignalRacesServeFailure guards
// the other half of the same early-exit bug, from the opposite direction:
// a signal-triggered shutdown is a clean, expected outcome even if Serve's
// own return races into a non-nil error as a side effect of that very
// shutdown — concretely, if this method's own Stop call (triggered by the
// signal) happens to stop the gRPC server before Serve is ever invoked,
// Serve returns grpc.ErrServerStopped despite nothing having actually gone
// wrong. The pre-fix code treated any non-nil Serve error as fatal
// (returning it immediately, without even waiting for the shutdown
// sequence to run), which would have made a perfectly normal shutdown look
// like a crash — exactly backwards from what a caller like cmdServe, which
// calls die()/os.Exit(1) on a non-nil error, needs. This test reproduces
// the race deterministically instead of relying on signal-delivery timing:
// it forces Serve to fail via the same pre-stopped-server trick as above,
// and queues the shutdown signal before RunUntilSignal is even called, so
// the signal is reliably observed before Serve's goroutine can be
// scheduled to run and report its (spurious, in this case) failure.
func TestRunUntilSignalReportsCleanShutdownWhenSignalRacesServeFailure(t *testing.T) {
	h := newRawTestServeHandle(t)
	h.grpcSrv.GracefulStop()

	sigCh := make(chan os.Signal, 1)
	sigCh <- syscall.SIGTERM // already queued before RunUntilSignal starts

	runErrCh := make(chan error, 1)
	go func() { runErrCh <- h.RunUntilSignal(sigCh, 0, 5*time.Second) }()

	select {
	case err := <-runErrCh:
		if err != nil {
			t.Errorf("RunUntilSignal returned %v, want nil — a signal-triggered shutdown must report success even when Serve's own return raced ahead into grpc.ErrServerStopped as a side effect of that same shutdown", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("RunUntilSignal did not return within 5s")
	}

	// The full Drain/Stop sequence — not just the pre-forced gRPC stop —
	// must actually have run: Stop closes the HTTP listener, which this
	// test never touched directly, so it can only be closed if the real
	// sequence executed.
	if _, err := http.Get("http://" + h.MetricsAddr + "/readyz"); err == nil {
		t.Error("GET /readyz succeeded after RunUntilSignal returned, want connection error (Stop's real shutdown sequence should have closed the HTTP listener)")
	}
}

// TestStartServeWithoutMetricsAddressDisablesHTTPSurface guards the fix for
// a real deployability regression: shoal-embed serve used to bind the
// observability HTTP port unconditionally, which would have broken any
// pre-existing single-port invocation (e.g. the top-level README's
// `shoal-embed serve --data ... --port 9876` quick-start) by adding a
// second listening port as a side effect of upgrading. An empty
// MetricsAddress must disable the HTTP surface entirely — no listener, no
// *http.Server, and MetricsAddr left "" — while leaving the gRPC data
// plane fully functional.
func TestStartServeWithoutMetricsAddressDisablesHTTPSurface(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	h, err := startServe(serveConfig{
		DataDir:        t.TempDir(),
		GRPCAddress:    "127.0.0.1:0",
		MetricsAddress: "",
		Logger:         logger,
	})
	if err != nil {
		t.Fatalf("startServe: %v", err)
	}
	if h.MetricsAddr != "" {
		t.Fatalf("MetricsAddr = %q, want empty when MetricsAddress was not set", h.MetricsAddr)
	}
	if h.httpSrv != nil {
		t.Error("httpSrv is non-nil, want nil when the observability HTTP surface is disabled")
	}
	if h.httpLis != nil {
		t.Error("httpLis is non-nil, want nil when the observability HTTP surface is disabled")
	}

	serveErrCh := make(chan error, 1)
	go func() { serveErrCh <- h.Serve() }()

	// The gRPC data plane must still work with no HTTP surface at all.
	conn, err := grpc.NewClient(h.GRPCAddr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("grpc.NewClient: %v", err)
	}
	defer conn.Close()
	client := embedpb.NewShoalEmbedClient(conn)
	rpcCtx, rpcCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer rpcCancel()
	if _, err := client.Status(rpcCtx, &embedpb.StatusRequest{}); err != nil {
		t.Fatalf("Status RPC with HTTP surface disabled: %v", err)
	}

	// Drain and Stop must not panic on the nil httpSrv/httpLis.
	h.Drain()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := h.Stop(ctx); err != nil {
		t.Fatalf("Stop(ctx) = %v, want nil", err)
	}

	select {
	case err := <-serveErrCh:
		if err != nil {
			t.Errorf("Serve() returned %v, want nil after a clean Stop", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Serve did not return within 5s of Stop")
	}
}

// TestStartServeSetsObservabilityReadHeaderTimeout guards a real
// robustness gap: the observability HTTP server had no read-header
// timeout of its own, so a client that opens a connection and sends
// request headers arbitrarily slowly (or never finishes them) could hold
// a connection — and the goroutine net/http spins up per connection —
// open indefinitely. This listener is bound to 0.0.0.0 by both production
// manifests, so it's reachable from outside the pod, not just from
// trusted local scrapers.
func TestStartServeSetsObservabilityReadHeaderTimeout(t *testing.T) {
	h := newRawTestServeHandle(t)
	t.Cleanup(func() {
		h.grpcSrv.Stop()
		if h.httpLis != nil {
			h.httpLis.Close()
		}
	})
	if h.httpSrv == nil {
		t.Fatal("httpSrv is nil, want non-nil when MetricsAddress is set")
	}
	if h.httpSrv.ReadHeaderTimeout <= 0 {
		t.Errorf("httpSrv.ReadHeaderTimeout = %v, want > 0 (unbounded header reads let a slow client hold a connection open indefinitely)", h.httpSrv.ReadHeaderTimeout)
	}
}

// TestMetricsAddrFromFlags proves the CLI opt-in contract cmdServe relies
// on: the observability HTTP surface only turns on when the caller
// explicitly passes --metrics-port or --metrics-address, even if
// --metrics-port happens to be set to its own default value.
func TestMetricsAddrFromFlags(t *testing.T) {
	tests := []struct {
		name string
		args []string
		want string
	}{
		{
			name: "no flags disables the HTTP surface",
			args: nil,
			want: "",
		},
		{
			name: "explicit metrics-port enables it on loopback",
			args: []string{"--metrics-port=8888"},
			want: "127.0.0.1:8888",
		},
		{
			name: "explicit metrics-port matching the default still counts as explicit",
			args: []string{"--metrics-port=9877"},
			want: "127.0.0.1:9877",
		},
		{
			name: "explicit metrics-address enables it verbatim and wins over metrics-port",
			args: []string{"--metrics-address=0.0.0.0:9999", "--metrics-port=1234"},
			want: "0.0.0.0:9999",
		},
		{
			name: "unrelated flags don't count as explicit",
			args: []string{"--data=/tmp/x"},
			want: "",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fs := flag.NewFlagSet("serve", flag.ContinueOnError)
			fs.String("data", "", "")
			metricsPort := fs.Int("metrics-port", 9877, "")
			metricsAddress := fs.String("metrics-address", "", "")
			if err := fs.Parse(tt.args); err != nil {
				t.Fatalf("fs.Parse(%v): %v", tt.args, err)
			}
			got := metricsAddrFromFlags(fs, *metricsPort, *metricsAddress)
			if got != tt.want {
				t.Errorf("metricsAddrFromFlags(%v) = %q, want %q", tt.args, got, tt.want)
			}
		})
	}
}

// TestServeResultAttributesStoppedOnlyToARequestedStop pins the distinction
// serveResult draws (#473): grpc.ErrServerStopped is a clean return only once
// Stop was requested, and every other error is reported as is.
func TestServeResultAttributesStoppedOnlyToARequestedStop(t *testing.T) {
	other := errors.New("accept: too many open files")
	for _, tc := range []struct {
		name    string
		stopped bool
		err     error
		want    error
	}{
		{"clean return", false, nil, nil},
		{"stopped before Stop was requested", false, grpc.ErrServerStopped, grpc.ErrServerStopped},
		{"stopped after Stop was requested", true, grpc.ErrServerStopped, nil},
		{"another error after Stop was requested", true, other, other},
		{"another error without Stop", false, other, other},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := &serveHandle{}
			h.stopRequested.Store(tc.stopped)
			if got := h.serveResult(tc.err); !errors.Is(got, tc.want) || (tc.want == nil) != (got == nil) {
				t.Fatalf("serveResult(%v) with stop requested=%v = %v, want %v", tc.err, tc.stopped, got, tc.want)
			}
		})
	}
}

// TestServeHandleStopBeforeServeReachesGRPC is the race #473 reported, with
// the ordering forced the bad way round: Stop is requested before Serve is
// even called, so gRPC is stopped before Serve can reach its accept loop.
// Serve and Stop must both still report a clean shutdown.
func TestServeHandleStopBeforeServeReachesGRPC(t *testing.T) {
	h := newRawTestServeHandle(t)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	stopErrCh := make(chan error, 1)
	go func() { stopErrCh <- h.Stop(ctx) }()
	deadline := time.Now().Add(5 * time.Second)
	for !h.stopRequested.Load() {
		if time.Now().After(deadline) {
			t.Fatal("Stop did not record the request within 5s")
		}
		time.Sleep(time.Millisecond)
	}

	serveErrCh := make(chan error, 1)
	go func() { serveErrCh <- h.Serve() }()
	select {
	case err := <-serveErrCh:
		if err != nil {
			t.Fatalf("Serve() = %v after Stop was requested, want nil", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Serve did not return within 5s of a requested Stop")
	}
	select {
	case err := <-stopErrCh:
		if err != nil {
			t.Fatalf("Stop(ctx) = %v, want nil", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Stop did not return within 5s")
	}
}
