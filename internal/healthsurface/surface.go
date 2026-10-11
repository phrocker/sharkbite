// Licensed to the Apache Software Foundation (ASF) under one
// or more contributor license agreements. See the NOTICE file
// distributed with this work for additional information
// regarding copyright ownership. The ASF licenses this file
// to you under the Apache License, Version 2.0 (the
// "License"); you may not use this file except in compliance
// with the License. You may obtain a copy of the License at
//
//     https://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing,
// software distributed under the License is distributed on an
// "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY
// KIND, either express or implied. See the License for the
// specific language governing permissions and limitations
// under the License.

// Package healthsurface is the orchestrator probe listener shared by every
// Shoal process that serves a request surface a kubelet cannot address.
//
// It is a package rather than a copy in each command because the two
// properties that make it correct are orderings, not values — readiness drops
// before the server stops accepting, and shutdown joins the serve loop before
// reading its error. A second implementation would have to keep both
// agreements, and a matched pair of implementations is a promise someone
// eventually breaks. The dispatch and admission claim paths drifted twice
// exactly that way.
package healthsurface

import (
	"context"
	"errors"
	"net"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/phrocker/shoal-oss/internal/promtext"
)

// The health surface is deliberately a second listener rather than two more
// routes on the workspace handler.
//
// The workspace handler refuses any request whose Host is not an exactly
// matching configured authority (webapi.Handler.ServeHTTP consults
// hostAuthority.permits before routing, authentication, or anything else). A
// kubelet probe addresses the pod by its runtime-assigned IP, which no
// statically configured authority list can name, so every probe against the
// workspace port answers 421 and the pod never becomes ready. Widening the
// authority list to admit the pod IP would weaken the control that exists to
// stop misdirected and DNS-rebound requests, to fix an operational problem.
//
// A separate listener keeps that control untouched. It also separates the two
// audiences: the workspace port serves callers who hold a decision, and the
// health port serves an orchestrator that holds nothing and must learn
// nothing. This mirrors the read fleet, which already answers /healthz and
// /readyz on its metrics listener rather than on its Thrift port.
//
// The surface discloses nothing. Both routes answer with a status code and a
// fixed string; neither consults the corpus, the policy catalog, the
// authenticator, or the request. An unauthenticated prober learns that a Shoal
// workspace process is up, which the open TCP port already tells it.
//
// The one optional route, /metrics, exists only when the host supplies a
// Config.Metrics writer, and that writer carries the disclosure obligation for
// what it emits (see Config).

// healthState is the readiness bit shared between the serving path and the
// health handler. It is false until the workspace is actually serving and
// false again as soon as shutdown begins, so a draining pod is removed from
// Service endpoints before it stops accepting connections.
type State struct {
	ready atomic.Bool
}

func (s *State) MarkReady()    { s.ready.Store(true) }
func (s *State) MarkDraining() { s.ready.Store(false) }

// MetricsWriter renders Prometheus text exposition samples into the builder.
//
// It is the same shape as roleops.MetricsWriter, and an alias of the unnamed
// function type, so a roleops.MetricsWriter value is assignable here without a
// conversion and one producer can serve both surfaces. It is called once per
// scrape, concurrently with every other scrape and with whatever updates the
// values it reads, so it must be safe for that. Escape label values with
// promtext.
type MetricsWriter = func(*strings.Builder)

// Config is the optional part of the surface.
//
// The zero value is the surface as it always was: two routes, and every other
// path a 404.
type Config struct {
	// Metrics, when set, is served as GET /metrics on the health listener.
	//
	// Whatever it writes is answered to anyone who can reach the health port,
	// unauthenticated — the same audience as the probes. It must therefore
	// carry operational counts only: no payload, identity, corpus or policy
	// content. Once this is set, the surface's no-disclosure property is the
	// producer's to keep.
	//
	// "Operational counts" means counters of *this process's own behaviour*:
	// errors, skips, refusals by closed reason, readiness. What it excludes is
	// anything tracking tenant workload — pending or in-flight counts, queue
	// depth, per-action or per-principal anything — because volume over an
	// open port is a load oracle even when it carries no identifier, and a
	// per-object series is an existence oracle outright (#398).
	//
	// The line is drawn there rather than at "contains no IDs" because the
	// first signal proposed for this surface passed that weaker test and
	// failed this one: an oldest-pending-transition age names nothing and
	// still reports how much work a tenant has outstanding. A signal over
	// pending work belongs behind authentication; see #642 and #647.
	Metrics MetricsWriter
}

// newHealthHandler builds the health mux. Any path other than the two routes
// is a 404 from the mux, so the surface cannot grow by accident.
func NewHandler(state *State) http.Handler {
	return NewHandlerWithConfig(state, Config{})
}

// NewHandlerWithConfig is NewHandler with the optional routes config names.
// /metrics exists only when config.Metrics is set; otherwise the path is a 404
// like any other unregistered one.
func NewHandlerWithConfig(state *State, config Config) http.Handler {
	return newHandler(state, config, nil)
}

// newHandler takes the shutdown signal separately because only Start has one:
// a handler mounted on a caller's own server is never told that server is
// closing, and relies on the request context alone.
func newHandler(state *State, config Config, closing <-chan struct{}) http.Handler {
	mux := http.NewServeMux()
	// Liveness answers for the process, not the workspace. It stays 200 while
	// draining: a pod that is shedding traffic on purpose must not be killed
	// and restarted for it.
	mux.HandleFunc("GET /healthz", func(writer http.ResponseWriter, _ *http.Request) {
		writeHealth(writer, http.StatusOK, "ok")
	})
	// Readiness answers for the workspace listener. It is 503 both before the
	// workspace serves and after shutdown begins.
	mux.HandleFunc("GET /readyz", func(writer http.ResponseWriter, _ *http.Request) {
		if !state.ready.Load() {
			writeHealth(writer, http.StatusServiceUnavailable, "not ready")
			return
		}
		writeHealth(writer, http.StatusOK, "ready")
	})
	if config.Metrics != nil {
		// "GET /metrics" also matches HEAD; every other method is a 405 with
		// an Allow header from the mux, as for the probe routes.
		mux.Handle("GET /metrics", metricsHandler{write: config.Metrics, closing: closing})
	}
	return mux
}

// metricsHandler serves one scrape.
//
// Three properties differ from roleops' /metrics, each deliberately:
//
//   - It answers GET and HEAD only. roleops accepts any method.
//   - It is marked no-store. A cached scrape is a stale reading that looks
//     current.
//   - A panicking writer is a 500 with a fixed body. roleops lets the panic
//     reach net/http, which keeps the server alive but aborts the connection,
//     so the scraper sees a reset rather than a status it can alert on.
//
// The body is rendered completely before anything is written, so a writer
// that panics part-way never leaves a truncated exposition behind a 200.
//
// The writer also runs off the request goroutine, so that it cannot hold the
// listener's graceful close: a scrape still inside its writer when shutdown
// begins is answered 503 and its connection released, rather than keeping
// Drain waiting until its deadline. The writer itself is not interrupted — a
// function of a builder cannot be — and finishes into a buffer nobody reads.
type metricsHandler struct {
	write   MetricsWriter
	closing <-chan struct{}
}

type metricsResult struct {
	body     string
	panicked bool
}

func (h metricsHandler) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	rendered := make(chan metricsResult, 1)
	go func() {
		result := metricsResult{panicked: true}
		defer func() {
			// recover only stops the panic. Its value is not reported: the
			// body is fixed, and the surface discloses nothing it was not
			// built to.
			_ = recover()
			rendered <- result
		}()
		var builder strings.Builder
		h.write(&builder)
		result = metricsResult{body: builder.String()}
	}()

	select {
	case result := <-rendered:
		if result.panicked {
			writeMetricsFailure(writer, http.StatusInternalServerError, "metrics unavailable")
			return
		}
		writer.Header().Set("Content-Type", promtext.ContentType)
		writer.Header().Set("Cache-Control", "no-store")
		writer.Header().Set("X-Content-Type-Options", "nosniff")
		writer.WriteHeader(http.StatusOK)
		_, _ = writer.Write([]byte(result.body))
	case <-h.closing:
		writeMetricsFailure(writer, http.StatusServiceUnavailable, "shutting down")
	case <-request.Context().Done():
		// The scraper is gone; there is no one to answer.
	}
}

func writeMetricsFailure(writer http.ResponseWriter, status int, body string) {
	writer.Header().Set("Cache-Control", "no-store")
	writeHealth(writer, status, body)
}

func writeHealth(writer http.ResponseWriter, status int, body string) {
	writer.Header().Set("Content-Type", "text/plain; charset=utf-8")
	writer.Header().Set("X-Content-Type-Options", "nosniff")
	writer.WriteHeader(status)
	_, _ = writer.Write([]byte(body + "\n"))
}

// healthServer owns the health listener and its serve loop.
//
// done is closed exactly once, when the serve loop returns, and serveErr is
// written before that close. Shutdown therefore waits on a single signal and
// reads a settled value, rather than racing a watcher and a caller for the
// same error over a channel — the failure mode fixed in the roleops shutdown
// path (#382).
type Server struct {
	listener net.Listener
	server   *http.Server
	done     chan struct{}
	serveErr error
}

// startHealthServer binds the health address and begins serving.
//
// It is called only after the corpus, policy catalog and workspace handler are
// constructed. Binding later is the point: the health port accepting a
// connection at all already means construction finished, so a probe cannot see
// a ready-looking socket while the workspace is still opening its corpus. The
// workspace listener does not have that property, because it is bound early so
// an address the workspace may not serve is refused before the corpus opens.
func Start(address string, state *State) (*Server, error) {
	return StartWithConfig(address, state, Config{})
}

// StartWithConfig is Start with the optional routes config names. The serve
// loop and the shutdown ordering are Start's exactly.
func StartWithConfig(address string, state *State, config Config) (*Server, error) {
	listener, err := ListenTCP("tcp", address)
	if err != nil {
		return nil, err
	}
	// closing is closed by net/http as soon as Shutdown is called, through
	// RegisterOnShutdown, so a scrape blocked in its writer lets go of its
	// connection instead of holding the graceful close. Shutdown itself is
	// unchanged. net/http runs the hook on every Shutdown call, hence the
	// Once.
	closing := make(chan struct{})
	var closeOnce sync.Once
	health := &Server{
		listener: listener,
		server: &http.Server{
			Handler:           newHandler(state, config, closing),
			ReadHeaderTimeout: 5 * time.Second,
			ReadTimeout:       10 * time.Second,
			WriteTimeout:      10 * time.Second,
			IdleTimeout:       30 * time.Second,
		},
		done: make(chan struct{}),
	}
	health.server.RegisterOnShutdown(func() { closeOnce.Do(func() { close(closing) }) })
	go func() {
		defer close(health.done)
		err := health.server.Serve(listener)
		if !errors.Is(err, http.ErrServerClosed) {
			health.serveErr = err
		}
	}()
	return health, nil
}

// listenHealthTCP is a variable so tests can prove a bind failure is reported
// rather than swallowed.
var ListenTCP = net.Listen

// address reports the resolved listen address, which differs from the
// requested one whenever the request named port zero.
func (h *Server) Address() string {
	if h == nil {
		return ""
	}
	return h.listener.Addr().String()
}

// shutdown stops the health server and reports the first error that mattered:
// a serve loop that died on its own outranks a slow graceful close, because it
// is the one that says the surface stopped answering for a reason nobody asked
// for.
func (h *Server) Shutdown(ctx context.Context) error {
	if h == nil {
		return nil
	}
	closeErr := h.server.Shutdown(ctx)
	<-h.done
	if h.serveErr != nil {
		return h.serveErr
	}
	return closeErr
}

// gracefulServer is the part of *http.Server that drain needs, so a test can
// observe the readiness bit at the moment the workspace is asked to stop.
type GracefulServer interface {
	Shutdown(context.Context) error
}

// drain runs the shutdown sequence in the order the readiness contract
// requires.
//
// Readiness drops first, before the workspace is asked to stop. That ordering
// is the whole point: the endpoints controller needs a not-ready reading to
// stop routing new requests here, and it can only get one while this process
// is still answering probes. Shutting the workspace down first would close the
// door on in-flight work that was routed during the gap.
//
// The health surface closes last, after the workspace has finished its
// graceful close, so the not-ready answer stays available for as long as there
// is a process to ask.
func Drain(ctx context.Context, state *State, workspace GracefulServer, health *Server) error {
	state.MarkDraining()
	closeErr := workspace.Shutdown(ctx)
	if healthErr := health.Shutdown(ctx); closeErr == nil {
		closeErr = healthErr
	}
	return closeErr
}
