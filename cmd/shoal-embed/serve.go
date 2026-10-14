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
	"crypto/tls"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"sync/atomic"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"

	"github.com/phrocker/shoal-oss/internal/embedpb"
	"github.com/phrocker/shoal-oss/internal/engine"
	"github.com/phrocker/shoal-oss/internal/obs"
)

// serveConfig configures a shoal-embed "serve" runtime: the gRPC data-plane
// listener plus the optional HTTP observability surface (/healthz, /readyz,
// /stats, /metrics) used by container/Kubernetes probes and Prometheus
// scraping.
type serveConfig struct {
	DataDir string

	// GRPCAddress is the bind host:port for the gRPC data plane, e.g.
	// "127.0.0.1:9876" (loopback-only, the CLI default) or "0.0.0.0:9876"
	// (reachable from other pods — required for a Kubernetes Service to
	// route to it).
	GRPCAddress string

	// MetricsAddress is the bind host:port for the observability HTTP
	// server (/healthz, /readyz, /stats, /metrics). Same loopback-vs-
	// 0.0.0.0 tradeoff as GRPCAddress. Empty disables the HTTP surface
	// entirely: callers that don't ask for it (e.g. a bare `shoal-embed
	// serve --port N`, matching the top-level README's quick-start) don't
	// gain a second listening port as a side effect of upgrading — see
	// cmdServe's flag handling, which only sets this when the caller
	// explicitly passed --metrics-port or --metrics-address.
	MetricsAddress string

	// UnaryInterceptor, when non-nil, is chained after the built-in
	// in-flight counter. Production callers leave this nil; tests use it to
	// emulate slow or non-cancellable unary RPC behavior.
	UnaryInterceptor grpc.UnaryServerInterceptor

	// TLS configures optional (mutual) TLS for both the gRPC and HTTP
	// listeners. A zero-value TLS (all three fields empty) disables TLS
	// entirely: both listeners stay plaintext, exactly as before this
	// field existed. See tlsFilesConfig's doc comment for the precise
	// enable/validation rules.
	TLS tlsFilesConfig

	Logger *slog.Logger
}

// serveHandle owns the engine, listeners, and servers started by
// startServe. Shutdown is two-phase — Drain then Stop — so an orchestrator's
// readiness probe (polling /readyz) has a chance to observe "not ready" and
// stop routing new work before the listeners actually close:
//
//	h, _ := startServe(cfg)
//	go h.Serve()
//	// ... on SIGTERM:
//	h.Drain()
//	h.Stop(ctx)
//
// Production callers should use RunUntilSignal instead of driving Serve /
// Drain / Stop directly: it owns the ordering above AND waits for Stop to
// fully finish before returning, which calling Serve directly does not (see
// RunUntilSignal's doc comment).
type serveHandle struct {
	// GRPCAddr and MetricsAddr are the actual bound addresses (useful when
	// the configured address used port 0 for an OS-assigned port, e.g. in
	// tests).
	GRPCAddr    string
	MetricsAddr string

	// TLSEnabled reports whether cfg.TLS was configured (both listeners
	// speak TLS) or left disabled (both stay plaintext) — set once, during
	// startServe, from the same decision that built (or didn't build) the
	// tls.Config below.
	TLSEnabled bool

	eng      *engine.Engine
	obs      *obs.Server
	grpcSrv  *grpc.Server
	grpcLis  net.Listener
	httpSrv  *http.Server
	httpLis  net.Listener
	logger   *slog.Logger
	inFlight *unaryInFlight

	closeEngine func() error

	// httpServeDone is closed when the goroutine Serve starts to run
	// h.httpSrv.Serve(h.httpLis) returns, i.e. once the HTTP accept loop
	// has actually exited — not merely once Stop has asked it to. It is
	// created here, during startServe's single-threaded construction
	// (never later, and never by Serve itself), specifically so Stop can
	// read the field without any synchronization: by the time either
	// Serve or Stop can run, construction has already finished and the
	// field is immutable. nil when the HTTP surface is disabled.
	httpServeDone chan struct{}

	// stopRequested is set by Stop before it stops the gRPC server, so
	// Serve can tell the one grpc.ErrServerStopped that means "stopped as
	// asked" from one that means something else stopped it. See serveResult.
	stopRequested atomic.Bool
}

// unaryInFlight counts currently-executing unary RPCs so Stop can report,
// rather than silently block on, the case that matters most for a bounded
// drain: Write/Flush/Compact run a single synchronous engine call and
// (unlike the streaming Scan handler) don't observe transport
// cancellation, so force-stopping the gRPC server does not interrupt one
// already in progress — Stop still has to wait for it to return before
// closing the engine, or risk closing it out from under that call.
type unaryInFlight struct{ n atomic.Int64 }

func (u *unaryInFlight) intercept(ctx context.Context, req any, _ *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
	u.n.Add(1)
	defer u.n.Add(-1)
	return handler(ctx, req)
}

func (u *unaryInFlight) count() int64 { return u.n.Load() }

// shutdownTimeoutError reports that the gRPC transport was force-stopped at
// the drain deadline while one or more unary RPC handlers were still running,
// so the engine was intentionally left open rather than being closed unsafely
// out from under them.
type shutdownTimeoutError struct {
	cause             error
	inFlightUnaryRPCs int64
}

func (e *shutdownTimeoutError) Error() string {
	return fmt.Sprintf("graceful shutdown timed out with %d in-flight unary RPC(s); force-stopped transport and skipped engine close", e.inFlightUnaryRPCs)
}

func (e *shutdownTimeoutError) Unwrap() error { return e.cause }

// startServe opens the engine, binds the gRPC and HTTP listeners, registers
// the ShoalEmbed and observability handlers, and marks the server ready. It
// does not block or start accepting connections — call Serve for that. The
// HTTP observability listener is only bound when cfg.MetricsAddress is
// non-empty; see its doc comment.
func startServe(cfg serveConfig) (*serveHandle, error) {
	logger := cfg.Logger
	if logger == nil {
		logger = slog.Default()
	}

	if err := cfg.TLS.validate(); err != nil {
		return nil, err
	}
	var tlsConfig *tls.Config
	if !cfg.TLS.empty() {
		var err error
		tlsConfig, err = buildServerTLSConfig(cfg.TLS.CertFile, cfg.TLS.KeyFile, cfg.TLS.ClientCAFile)
		if err != nil {
			return nil, err
		}
	}

	eng, err := engine.Open(cfg.DataDir, engine.Options{Logger: logger})
	if err != nil {
		return nil, fmt.Errorf("open engine: %w", err)
	}

	grpcLis, err := net.Listen("tcp", cfg.GRPCAddress)
	if err != nil {
		_ = eng.Close()
		return nil, fmt.Errorf("grpc listen: %w", err)
	}

	var httpLis net.Listener
	if cfg.MetricsAddress != "" {
		httpLis, err = net.Listen("tcp", cfg.MetricsAddress)
		if err != nil {
			_ = grpcLis.Close()
			_ = eng.Close()
			return nil, fmt.Errorf("metrics listen: %w", err)
		}
		if tlsConfig != nil {
			httpLis = tls.NewListener(httpLis, tlsConfig.Clone())
		}
	}

	inFlight := &unaryInFlight{}
	interceptors := []grpc.UnaryServerInterceptor{inFlight.intercept}
	if cfg.UnaryInterceptor != nil {
		interceptors = append(interceptors, cfg.UnaryInterceptor)
	}
	grpcOpts := []grpc.ServerOption{grpc.ChainUnaryInterceptor(interceptors...)}
	if tlsConfig != nil {
		grpcOpts = append(grpcOpts, grpc.Creds(credentials.NewTLS(tlsConfig.Clone())))
	}
	grpcSrv := grpc.NewServer(grpcOpts...)
	embedpb.RegisterShoalEmbedServer(grpcSrv, newEmbedServer(eng))

	obsSrv := obs.NewServer(eng)
	// obsSrv tracks the ready/not-ready latch regardless of whether the
	// HTTP surface is enabled, so Drain/SetReady never need to special-case
	// a disabled httpSrv — only the listener and *http.Server are
	// conditional.
	var httpSrv *http.Server
	var httpServeDone chan struct{}
	if httpLis != nil {
		httpSrv = &http.Server{
			Handler: obsSrv.Handler(),
			// This listener is bound to 0.0.0.0 by both production
			// manifests, so it's reachable from outside the pod: without
			// a read-header timeout, a client sending headers slowly
			// could hold a connection (and a goroutine) open
			// indefinitely. Matches the bound the repo's other exposed
			// metrics server already uses
			// (cmd/shoal-compactor-shadow/service.go).
			ReadHeaderTimeout: 5 * time.Second,
			// Routes connection-level errors (malformed requests and,
			// once TLS is enabled, handshake failures from plaintext or
			// garbage traffic reaching the port) through the same
			// structured logger as everything else here, instead of
			// net/http's default behavior of writing them straight to
			// os.Stderr via the global "log" package. newHTTPErrorLog
			// additionally downgrades the specific, expected "TLS
			// handshake error ...: EOF" line a bare tcpSocket probe
			// produces against a TLS-wrapped listener — see its doc
			// comment in tls.go.
			ErrorLog: newHTTPErrorLog(logger),
		}
		httpServeDone = make(chan struct{})
	}

	// The engine is open and the gRPC listener is bound: declare ready.
	// This mirrors "shoal-embed up", which flips the same latch once its
	// default table is provisioned.
	obsSrv.SetReady(true)

	metricsAddr := ""
	if httpLis != nil {
		metricsAddr = httpLis.Addr().String()
	}

	return &serveHandle{
		GRPCAddr:      grpcLis.Addr().String(),
		MetricsAddr:   metricsAddr,
		TLSEnabled:    tlsConfig != nil,
		eng:           eng,
		obs:           obsSrv,
		grpcSrv:       grpcSrv,
		grpcLis:       grpcLis,
		httpSrv:       httpSrv,
		httpLis:       httpLis,
		logger:        logger,
		inFlight:      inFlight,
		closeEngine:   eng.Close,
		httpServeDone: httpServeDone,
	}, nil
}

// Serve runs the HTTP observability server in the background and the gRPC
// data-plane server in the foreground, blocking until the gRPC server stops
// (via Stop, or a fatal accept error). The HTTP server only runs if the
// observability surface was enabled (cfg.MetricsAddress was non-empty).
//
// Serve intentionally does NOT wait for the HTTP accept loop it starts:
// gRPC can stop entirely on its own (a fatal accept error, or GracefulStop
// called directly), with nothing yet telling the HTTP side to stop, so
// blocking here for it too would deadlock in exactly that case — nothing
// ever tells httpSrv to shut down otherwise. Stop is the function that
// actually requests a shutdown, so it — not Serve — joins the HTTP
// accept-loop goroutine on its non-timeout return paths (see Stop's doc
// comment).
func (h *serveHandle) Serve() error {
	if h.httpSrv != nil {
		go func() {
			defer close(h.httpServeDone)
			if err := h.httpSrv.Serve(h.httpLis); err != nil && err != http.ErrServerClosed {
				h.logger.Error("observability server stopped", slog.String("err", err.Error()))
			}
		}()
	}
	return h.serveResult(h.grpcSrv.Serve(h.grpcLis))
}

// serveResult maps grpc.Server.Serve's return to what Serve reports.
//
// grpc.Server.Serve returns grpc.ErrServerStopped immediately when the server
// was already stopped by the time it is invoked. Stop can win that race: a
// caller that sees the HTTP surface answering knows Serve was entered, not
// that it has reached the gRPC accept loop. After Stop was requested that
// error is the requested outcome, so it is reported as a clean return; before
// it, something else stopped the server, and the error stands (#473).
// RunUntilSignal reconstructs the same distinction for its own callers.
func (h *serveHandle) serveResult(err error) error {
	if errors.Is(err, grpc.ErrServerStopped) && h.stopRequested.Load() {
		return nil
	}
	return err
}

// Drain marks the server not-ready without closing anything. Call this
// first, on receipt of a shutdown signal, and give any readiness-polling
// consumer (an orchestrator, a load balancer, or the Accumulo-model
// manager/coordinator that actually owns shard-to-writer routing) a brief
// quiesce window to observe /readyz before Stop closes the listeners —
// RunUntilSignal does this for you when the HTTP readiness surface is
// enabled. Safe to call once, before Stop.
func (h *serveHandle) Drain() {
	h.obs.SetReady(false)
}

// Stop gracefully stops the HTTP and gRPC servers, then closes the engine.
// Call Drain first so a readiness-polling consumer has a chance to observe
// the drained state.
//
// ctx bounds two things precisely: how long new connections keep being
// accepted, and how long a stuck-but-cancellable in-flight RPC (a
// streaming Scan blocked on client backpressure, for example, which
// notices its stream erroring out) is allowed to keep running before being
// force-aborted. http.Server.Shutdown and grpc.Server.GracefulStop run
// concurrently under the same ctx — not sequentially — so a slow /stats or
// /metrics request cannot eat into the budget meant for draining gRPC
// writes, and vice versa. If ctx ends first (deadline or cancellation),
// Stop force-closes the HTTP server and force-stops the gRPC server
// (aborting connections and cancellable RPCs).
//
// ctx does NOT bound a unary RPC (Write, Flush, Compact) that is already
// executing: those run a single synchronous engine call, ignore their
// request context today, and don't observe transport cancellation the way
// a streaming Send/Recv does. If ctx ends first while such handlers are
// still running, Stop force-stops the transport and returns a
// shutdownTimeoutError instead of waiting indefinitely; in that case it
// deliberately skips engine close rather than racing a close against
// still-running engine work.
//
// On its non-timeout return paths, Stop also joins the HTTP accept-loop
// goroutine Serve started (if the observability surface is enabled):
// Shutdown/Close only ask that goroutine to stop, asynchronously, and
// don't reliably wait for it to have actually returned — Shutdown's own
// "no more listeners" check races the goroutine's own startup if Stop is
// called early enough, and Close doesn't wait for it at all, by design
// (see net/http's docs). Without this join, Stop — and RunUntilSignal,
// which relies on Stop to fully finish — could report shutdown complete
// while that goroutine, and the listener it owns, were technically still
// open. The shutdownTimeoutError early return above deliberately skips
// this join too, consistent with everything else it already skips
// waiting for (gRPC's own Stop, the engine close) in that pathological
// case.
func (h *serveHandle) Stop(ctx context.Context) error {
	h.stopRequested.Store(true)
	var httpErrCh chan error
	if h.httpSrv != nil {
		httpErrCh = make(chan error, 1)
		go func() {
			err := h.httpSrv.Shutdown(ctx)
			if err != nil && ctx.Err() != nil {
				// http.Server.Shutdown closes the listener first, but if ctx
				// ends before active handlers finish it returns ctx.Err()
				// while those connections are still open. Force-close them so
				// Stop never returns with the HTTP surface still serving.
				_ = h.httpSrv.Close()
			}
			httpErrCh <- err
		}()
	}

	gracefulDone := make(chan struct{})
	go func() {
		h.grpcSrv.GracefulStop()
		close(gracefulDone)
	}()
	select {
	case <-gracefulDone:
	case <-ctx.Done():
		if h.httpSrv != nil {
			h.logger.Warn("drain deadline exceeded or canceled; forcing http close", slog.String("err", ctx.Err().Error()))
			_ = h.httpSrv.Close()
		}
		if n := h.inFlight.count(); n > 0 {
			_ = h.grpcLis.Close()
			go h.grpcSrv.Stop()
			h.logger.Warn("drain deadline exceeded with non-cancellable unary RPC(s) still running; transport force-stopped and engine left open",
				slog.Int64("inFlightUnaryRPCs", n), slog.String("err", ctx.Err().Error()))
			if httpErrCh != nil {
				select {
				case err := <-httpErrCh:
					if err != nil && !errors.Is(err, http.ErrServerClosed) && !errors.Is(err, context.DeadlineExceeded) && !errors.Is(err, context.Canceled) {
						h.logger.Warn("observability server shutdown", slog.String("err", err.Error()))
					}
				default:
				}
			}
			return &shutdownTimeoutError{cause: ctx.Err(), inFlightUnaryRPCs: n}
		}
		h.logger.Warn("drain deadline exceeded or canceled; forcing grpc stop", slog.String("err", ctx.Err().Error()))
		h.grpcSrv.Stop()
	}

	if httpErrCh != nil {
		if err := <-httpErrCh; err != nil && !errors.Is(err, http.ErrServerClosed) {
			switch {
			case errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled):
				// Expected once ctx ends before HTTP shutdown can complete
				// gracefully; the force-close path above or in the goroutine
				// handles the remaining active connections.
			default:
				h.logger.Warn("observability server shutdown", slog.String("err", err.Error()))
			}
		}
	}

	// httpErrCh, above, only confirms the goroutine that called
	// Shutdown/Close has returned from that call — not that the HTTP
	// accept-loop goroutine itself (started by Serve) has exited; see this
	// method's doc comment for why that's a real gap, not a theoretical
	// one. h.httpServeDone is nil when the HTTP surface is disabled, and
	// already closed (so this returns immediately) in the common case
	// where Serve's accept loop has long since noticed the listener close
	// by the time Shutdown/Close returns.
	if h.httpServeDone != nil {
		<-h.httpServeDone
	}

	if err := h.closeEngine(); err != nil {
		h.logger.Warn("engine close", slog.String("err", err.Error()))
		return fmt.Errorf("engine close: %w", err)
	}
	return nil
}

// RunUntilSignal is the production entry point for a shoal-embed serve
// process: it blocks running the server until sigCh delivers a shutdown
// signal, then performs the full two-phase drain — Drain, an optional
// quiesce pause (only when the HTTP readiness surface is enabled, so a
// readiness-polling consumer can observe /readyz before anything actually
// stops accepting work), and a Stop bounded by drainTimeout. It does not
// return until that sequence has finished, whether successfully or with an
// explicit timeout error describing why engine close was skipped.
//
// That covers one real early-exit race: Serve only reflects
// grpc.Server's own internal stop bookkeeping — it says nothing about the
// *additional* work this package's Stop wrapper does around that (waiting
// for the HTTP server to finish shutting down, then closing the engine).
// Those can still be in progress, e.g. blocked on a slow HTTP request or a
// non-cancellable unary RPC, when Serve returns. A caller that reacts to
// Serve returning by exiting the process (returning from main, for
// example) can therefore terminate every other goroutine, including the
// one still finishing Stop, before that work is done.
//
// It also covers the same race from the other direction: Serve can return
// a non-nil error *before* sigCh ever fires — either because the signal
// arrives (or, as here, was already queued) early enough that this
// method's own Drain/quiesce/Stop sequence stops the gRPC server before
// Serve is even called (grpc.Server.Serve returns grpc.ErrServerStopped
// immediately if the server is already stopped when it's invoked), or
// because of a genuine, signal-independent fatal accept error. Both the
// signal arriving and Serve returning are therefore raced against each
// other explicitly, via select, rather than left to chance: Serve runs in
// its own goroutine so a pending signal — even one already queued before
// this method is called at all — can be observed without first waiting
// for Serve's goroutine to be scheduled and to actually execute, so a
// signal that raced ahead of Serve is reliably still recognized as the
// reason for shutting down rather than mistaken for a Serve failure.
// Whichever wins, this method runs the Drain/quiesce/Stop sequence
// exactly once. On a clean Stop it also waits for Serve's goroutine to
// return before returning, so a caller can never see this method return
// and exit while shutdown work is still in progress. If Stop instead
// returns a shutdownTimeoutError because non-cancellable unary RPCs were
// still in flight at the drain deadline, this method propagates that
// explicit error immediately rather than waiting indefinitely for Serve to
// unwind. A signal-triggered shutdown therefore reports success (nil) only
// when Stop finishes cleanly; a genuinely signal-independent Serve failure
// still reports its real error after the resulting Drain/Stop has
// completed so the engine and HTTP server are never left open.
func (h *serveHandle) RunUntilSignal(sigCh <-chan os.Signal, quiesce, drainTimeout time.Duration) error {
	serveErrCh := make(chan error, 1)
	go func() { serveErrCh <- h.Serve() }()

	var serveErr error
	signaled := false
	select {
	case <-sigCh:
		signaled = true
	default:
	}
	if !signaled {
		select {
		case <-sigCh:
			signaled = true
		case serveErr = <-serveErrCh:
		}
	}

	h.logger.Info("draining")
	h.Drain()
	if h.httpSrv != nil && quiesce > 0 {
		time.Sleep(quiesce)
	}
	ctx, cancel := context.WithTimeout(context.Background(), drainTimeout)
	defer cancel()
	if err := h.Stop(ctx); err != nil {
		return err
	}

	if !signaled {
		return serveErr
	}
	// The signal branch above never consumed serveErrCh. On a clean
	// shutdown, Stop's GracefulStop/Stop calls guarantee Serve returns
	// shortly if it hasn't already; wait so this method still doesn't
	// return until the accept loop goroutine itself has exited too.
	<-serveErrCh
	return nil
}
