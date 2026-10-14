// Package roleops provides the shared operational HTTP, readiness, and
// bounded-shutdown surface used by distributed Shoal roles.
package roleops

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/phrocker/shoal-oss/internal/promtext"
)

type Dependency struct {
	Name   string `json:"name"`
	Ready  bool   `json:"ready"`
	Detail string `json:"detail,omitempty"`
}

type Dependencies struct {
	mu      sync.RWMutex
	values  map[string]Dependency
	started atomic.Bool
}

func NewDependencies(names ...string) *Dependencies {
	d := &Dependencies{values: make(map[string]Dependency, len(names))}
	for _, name := range names {
		d.values[name] = Dependency{Name: name, Detail: "initializing"}
	}
	return d
}

func (d *Dependencies) Set(name string, ready bool, detail string) {
	d.mu.Lock()
	d.values[name] = Dependency{Name: name, Ready: ready, Detail: detail}
	d.mu.Unlock()
}

func (d *Dependencies) SetStarted(started bool) { d.started.Store(started) }

func (d *Dependencies) Snapshot() []Dependency {
	d.mu.RLock()
	out := make([]Dependency, 0, len(d.values))
	for _, value := range d.values {
		out = append(out, value)
	}
	d.mu.RUnlock()
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

func (d *Dependencies) Ready() bool {
	if !d.started.Load() {
		return false
	}
	for _, dependency := range d.Snapshot() {
		if !dependency.Ready {
			return false
		}
	}
	return true
}

type MetricsWriter func(*strings.Builder)

func Handler(dependencies *Dependencies, writeMetrics MetricsWriter) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = w.Write([]byte("ok\n"))
	})
	mux.HandleFunc("/startupz", func(w http.ResponseWriter, _ *http.Request) {
		status := http.StatusOK
		started := dependencies != nil && dependencies.started.Load()
		if !started {
			status = http.StatusServiceUnavailable
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(struct {
			Started      bool         `json:"started"`
			Dependencies []Dependency `json:"dependencies"`
		}{Started: started, Dependencies: dependenciesSnapshot(dependencies)})
	})
	mux.HandleFunc("/readyz", func(w http.ResponseWriter, _ *http.Request) {
		status := http.StatusOK
		if dependencies == nil || !dependencies.Ready() {
			status = http.StatusServiceUnavailable
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(struct {
			Ready        bool         `json:"ready"`
			Dependencies []Dependency `json:"dependencies"`
		}{Ready: status == http.StatusOK, Dependencies: dependenciesSnapshot(dependencies)})
	})
	mux.HandleFunc("/metrics", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", promtext.ContentType)
		var b strings.Builder
		b.WriteString("# HELP shoal_dependency_ready Whether an operational dependency is ready.\n")
		b.WriteString("# TYPE shoal_dependency_ready gauge\n")
		for _, dependency := range dependenciesSnapshot(dependencies) {
			value := 0
			if dependency.Ready {
				value = 1
			}
			fmt.Fprintf(&b, "shoal_dependency_ready{%s} %d\n", promtext.Label("name", dependency.Name), value)
		}
		if writeMetrics != nil {
			writeMetrics(&b)
		}
		_, _ = w.Write([]byte(b.String()))
	})
	return mux
}

func dependenciesSnapshot(dependencies *Dependencies) []Dependency {
	if dependencies == nil {
		return nil
	}
	return dependencies.Snapshot()
}

type Server struct {
	http *http.Server
	// done is closed once the serve loop has returned. It carries no value,
	// because a value can only be delivered to one receiver: when it did, a
	// caller watching Done and a caller in Shutdown raced for the serve error
	// and whichever lost observed nil.
	done chan struct{}
	// serveErr is written once before done is closed, so every goroutine that
	// observes the close is guaranteed to see it.
	serveErr error
	once     sync.Once
}

func Start(address string, handler http.Handler, tlsConfig *tls.Config) (*Server, error) {
	listener, err := net.Listen("tcp", address)
	if err != nil {
		return nil, err
	}
	if tlsConfig != nil {
		listener = tls.NewListener(listener, tlsConfig.Clone())
	}
	return serve(address, listener, handler), nil
}

// serve is the half of Start that does not bind, so a test can supply a
// listener that fails and assert what happens to the resulting serve error.
// Without a seam here the error path is unreachable, and a regression test for
// it can only assert the nil case, which passes whether or not the error is
// delivered at all.
func serve(address string, listener net.Listener, handler http.Handler) *Server {
	server := &Server{
		http: &http.Server{
			Addr:              address,
			Handler:           handler,
			ReadHeaderTimeout: 5 * time.Second,
		},
		done: make(chan struct{}),
	}
	go func() {
		err := server.http.Serve(listener)
		if errors.Is(err, http.ErrServerClosed) {
			err = nil
		}
		server.serveErr = err
		close(server.done)
	}()
	return server
}

// Done is closed when the serve loop has returned. Once it is readable the
// loop has exited, so a caller that shuts down and then releases the
// listener's resources is not racing it.
func (s *Server) Done() <-chan struct{} { return s.done }

// Err reports why the serve loop returned. It is only meaningful once Done is
// readable, and every caller sees the same value rather than the first one
// consuming it.
func (s *Server) Err() error {
	select {
	case <-s.done:
		return s.serveErr
	default:
		return nil
	}
}

func (s *Server) Shutdown(ctx context.Context) error {
	if s == nil {
		return nil
	}
	var shutdownErr error
	s.once.Do(func() {
		shutdownErr = s.http.Shutdown(ctx)
		if shutdownErr != nil {
			_ = s.http.Close()
		}
		select {
		case <-s.done:
			shutdownErr = errors.Join(shutdownErr, s.serveErr)
		case <-ctx.Done():
			// Close unblocks Serve, so the join below is bounded by the
			// goroutine finishing rather than by the caller's deadline.
			//
			// Returning on the context alone would let Shutdown outrun the
			// serve loop, which is the ordering this function exists to
			// guarantee. It is reachable whenever the context is already done
			// on entry, including a shutdown context reused after waiting on
			// another server, because a ready Done case can win the select
			// before the goroutine has closed s.done.
			_ = s.http.Close()
			<-s.done
			shutdownErr = errors.Join(shutdownErr, ctx.Err(), s.serveErr)
		}
	})
	return shutdownErr
}

func RunBounded(ctx context.Context, steps ...func(context.Context) error) error {
	var combined error
	for _, step := range steps {
		if step != nil {
			combined = errors.Join(combined, step(ctx))
		}
		if err := ctx.Err(); err != nil {
			return errors.Join(combined, err)
		}
	}
	return combined
}
