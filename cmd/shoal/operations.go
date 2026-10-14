package main

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/phrocker/shoal-oss/internal/metadata"
	"github.com/phrocker/shoal-oss/internal/scanserver"
	"github.com/phrocker/shoal-oss/internal/storage"
)

type readinessState struct {
	mu        sync.RWMutex
	zookeeper error
	metadata  error
	storage   error
	serving   bool
	lastCheck time.Time
	draining  bool
}

func (r *readinessState) update(zkErr, metadataErr, storageErr error) {
	r.mu.Lock()
	r.zookeeper, r.metadata, r.storage = zkErr, metadataErr, storageErr
	r.lastCheck = time.Now()
	r.mu.Unlock()
}

func (r *readinessState) setServing(v bool) {
	r.mu.Lock()
	r.serving = v
	r.mu.Unlock()
}

func (r *readinessState) beginDrain() {
	r.mu.Lock()
	r.draining = true
	r.mu.Unlock()
}

type readinessSnapshot struct {
	Ready      bool              `json:"ready"`
	Draining   bool              `json:"draining"`
	LastCheck  time.Time         `json:"last_check"`
	Components map[string]string `json:"components"`
}

func (r *readinessState) snapshot(accepting bool) readinessSnapshot {
	r.mu.RLock()
	defer r.mu.RUnlock()
	components := map[string]string{
		"zookeeper": statusText(r.zookeeper),
		"metadata":  statusText(r.metadata),
		"storage":   statusText(r.storage),
		"listener":  boolStatus(r.serving),
		"sessions":  boolStatus(accepting && !r.draining),
	}
	return readinessSnapshot{
		Ready: r.zookeeper == nil && r.metadata == nil && r.storage == nil &&
			r.serving && accepting && !r.draining && !r.lastCheck.IsZero(),
		Draining:   r.draining,
		LastCheck:  r.lastCheck,
		Components: components,
	}
}

func statusText(err error) string {
	if err == nil {
		return "ready"
	}
	return err.Error()
}

func boolStatus(ok bool) string {
	if ok {
		return "ready"
	}
	return "not ready"
}

type readinessChecks struct {
	zookeeper func(context.Context) error
	metadata  func(context.Context) (map[string][]metadata.TabletInfo, error)
	storage   storage.Backend
}

func (c readinessChecks) run(ctx context.Context) (error, error, error) {
	zkErr := c.zookeeper(ctx)
	tablets, metadataErr := c.metadata(ctx)
	var storageErr error
	if metadataErr == nil {
		storageErr = probeStorage(ctx, c.storage, tablets)
	} else {
		storageErr = errors.New("metadata unavailable")
	}
	return zkErr, metadataErr, storageErr
}

func probeStorage(ctx context.Context, backend storage.Backend, tablets map[string][]metadata.TabletInfo) error {
	var paths []string
	for _, table := range tablets {
		for _, tablet := range table {
			for _, file := range tablet.Files {
				if file.Path != "" {
					paths = append(paths, file.Path)
				}
			}
		}
	}
	if len(paths) == 0 {
		return nil
	}
	sort.Strings(paths)
	f, err := backend.Open(ctx, paths[0])
	if err != nil {
		return fmt.Errorf("open readiness object: %w", err)
	}
	defer f.Close()
	if f.Size() == 0 {
		return nil
	}
	var one [1]byte
	if _, err := f.ReadAt(one[:], 0); err != nil && err != io.EOF {
		return fmt.Errorf("read readiness object: %w", err)
	}
	return nil
}

func runReadinessLoop(ctx context.Context, state *readinessState, checks readinessChecks, interval time.Duration) {
	if interval <= 0 {
		interval = 10 * time.Second
	}
	refresh := func() {
		probeCtx, cancel := context.WithTimeout(ctx, interval)
		defer cancel()
		state.update(checks.run(probeCtx))
	}
	refresh()
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			refresh()
		}
	}
}

func newOperationsHandler(state *readinessState, scans *scanserver.Server) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		fmt.Fprintln(w, "ok")
	})
	mux.HandleFunc("/readyz", func(w http.ResponseWriter, _ *http.Request) {
		snap := state.snapshot(scans.Accepting())
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		if !snap.Ready {
			w.WriteHeader(http.StatusServiceUnavailable)
		}
		_ = json.NewEncoder(w).Encode(snap)
	})
	mux.HandleFunc("/metrics", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
		fmt.Fprint(w, renderScanMetrics(scans.Metrics(), scans.Accepting()))
	})
	return mux
}

func renderScanMetrics(m scanserver.Metrics, accepting bool) string {
	var b strings.Builder
	gauge := func(name, help string, value any, labels string) {
		fmt.Fprintf(&b, "# HELP %s %s\n# TYPE %s gauge\n%s%s %v\n", name, help, name, name, labels, value)
	}
	counter := func(name, help string, value uint64, labels string) {
		fmt.Fprintf(&b, "# HELP %s %s\n# TYPE %s counter\n%s%s %d\n", name, help, name, name, labels, value)
	}
	acceptingValue := 0
	if accepting {
		acceptingValue = 1
	}
	gauge("shoal_read_accepting_sessions", "Whether the read role accepts new scan sessions.", acceptingValue, "")
	gauge("shoal_scan_sessions_active", "Active retained scan sessions.", m.ActiveSingle, `{kind="single"}`)
	fmt.Fprintf(&b, "shoal_scan_sessions_active{kind=\"multi\"} %d\n", m.ActiveMulti)
	counter("shoal_scan_sessions_expired_total", "Scan sessions expired by idle timeout.", m.ExpiredSingleTotal, `{kind="single"}`)
	fmt.Fprintf(&b, "shoal_scan_sessions_expired_total{kind=\"multi\"} %d\n", m.ExpiredMultiTotal)
	counter("shoal_scan_sessions_canceled_total", "Scan sessions canceled by close or forced drain.", m.CanceledSingleTotal, `{kind="single"}`)
	fmt.Fprintf(&b, "shoal_scan_sessions_canceled_total{kind=\"multi\"} %d\n", m.CanceledMultiTotal)
	counter("shoal_scan_continuations_total", "Continuation RPC calls.", m.ContinuationsSingle, `{kind="single"}`)
	fmt.Fprintf(&b, "shoal_scan_continuations_total{kind=\"multi\"} %d\n", m.ContinuationsMulti)
	counter("shoal_scan_failures_total", "Failed scan RPC calls.", m.FailuresStartSingle, `{operation="start",kind="single"}`)
	fmt.Fprintf(&b, "shoal_scan_failures_total{operation=\"start\",kind=\"multi\"} %d\n", m.FailuresStartMulti)
	fmt.Fprintf(&b, "shoal_scan_failures_total{operation=\"tablet\",kind=\"multi\"} %d\n", m.FailuresTabletMulti)
	fmt.Fprintf(&b, "shoal_scan_failures_total{operation=\"continue\",kind=\"all\"} %d\n", m.FailuresContinue)
	counter("shoal_scan_backpressure_total", "Scan admission rejected by bounded capacity or drain.", m.BackpressureCapacity, `{reason="capacity"}`)
	fmt.Fprintf(&b, "shoal_scan_backpressure_total{reason=\"draining\"} %d\n", m.BackpressureDraining)
	counter("shoal_scan_rpc_latency_seconds_count", "Observed scan RPC latency samples.", m.LatencyStartCount, `{operation="start"}`)
	fmt.Fprintf(&b, "shoal_scan_rpc_latency_seconds_count{operation=\"continue\"} %d\n", m.LatencyContinueCount)
	fmt.Fprintln(&b, "# HELP shoal_scan_rpc_latency_seconds_sum Cumulative scan RPC latency in seconds.")
	fmt.Fprintln(&b, "# TYPE shoal_scan_rpc_latency_seconds_sum counter")
	fmt.Fprintf(&b, "shoal_scan_rpc_latency_seconds_sum{operation=\"start\"} %g\n", float64(m.LatencyStartNanos)/float64(time.Second))
	fmt.Fprintf(&b, "shoal_scan_rpc_latency_seconds_sum{operation=\"continue\"} %g\n", float64(m.LatencyContinueNanos)/float64(time.Second))
	return b.String()
}

func startOperationsServer(address string, handler http.Handler, tlsConfig *tls.Config, logger *slog.Logger) (*http.Server, net.Listener, error) {
	if address == "" {
		return nil, nil, nil
	}
	lis, err := net.Listen("tcp", address)
	if err != nil {
		return nil, nil, err
	}
	if tlsConfig != nil {
		lis = tls.NewListener(lis, tlsConfig.Clone())
	}
	server := &http.Server{
		Handler:           handler,
		ReadHeaderTimeout: 5 * time.Second,
	}
	go func() {
		if err := server.Serve(lis); err != nil && !errorsIsServerClosed(err) {
			logger.Error("read operations server stopped", slog.Any("err", err))
		}
	}()
	return server, lis, nil
}

func errorsIsServerClosed(err error) bool { return err == http.ErrServerClosed }
