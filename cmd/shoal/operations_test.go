package main

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/phrocker/shoal-oss/internal/metadata"
	"github.com/phrocker/shoal-oss/internal/scanserver"
	"github.com/phrocker/shoal-oss/internal/storage"
)

func TestSemanticReadinessRequiresEveryComponentAndAcceptance(t *testing.T) {
	scans := newOperationsTestScanServer(t)
	state := &readinessState{}
	state.setServing(true)
	state.update(nil, nil, nil)
	handler := newOperationsHandler(state, scans)

	req := httptest.NewRequest(http.MethodGet, "/readyz", nil)
	ok := httptest.NewRecorder()
	handler.ServeHTTP(ok, req)
	if ok.Code != http.StatusOK {
		t.Fatalf("ready status = %d, body=%s", ok.Code, ok.Body.String())
	}

	state.update(errors.New("zk unavailable"), nil, nil)
	notReady := httptest.NewRecorder()
	handler.ServeHTTP(notReady, req)
	if notReady.Code != http.StatusServiceUnavailable || !strings.Contains(notReady.Body.String(), "zk unavailable") {
		t.Fatalf("not-ready response = %d %s", notReady.Code, notReady.Body.String())
	}

	state.update(nil, nil, nil)
	scans.BeginDrain()
	draining := httptest.NewRecorder()
	handler.ServeHTTP(draining, req)
	if draining.Code != http.StatusServiceUnavailable || !strings.Contains(draining.Body.String(), `"sessions":"not ready"`) {
		t.Fatalf("draining response = %d %s", draining.Code, draining.Body.String())
	}
}

func TestMetricsExposeStableScanNames(t *testing.T) {
	scans := newOperationsTestScanServer(t)
	body := renderScanMetrics(scans.Metrics(), scans.Accepting())
	for _, name := range []string{
		"shoal_read_accepting_sessions",
		"shoal_scan_sessions_active",
		"shoal_scan_sessions_expired_total",
		"shoal_scan_sessions_canceled_total",
		"shoal_scan_continuations_total",
		"shoal_scan_failures_total",
		"shoal_scan_rpc_latency_seconds",
		"shoal_scan_backpressure_total",
	} {
		if !strings.Contains(body, name) {
			t.Errorf("metrics missing %q:\n%s", name, body)
		}
	}
}

func TestReadinessLoopUpdatesDeterministically(t *testing.T) {
	state := &readinessState{}
	ctx, cancel := context.WithCancel(context.Background())
	checks := readinessChecks{
		zookeeper: func(context.Context) error { return nil },
		metadata: func(context.Context) (map[string][]metadata.TabletInfo, error) {
			return map[string][]metadata.TabletInfo{}, nil
		},
		storage: &failingStorage{},
	}
	done := make(chan struct{})
	go func() {
		runReadinessLoop(ctx, state, checks, time.Hour)
		close(done)
	}()
	deadline := time.After(time.Second)
	for state.snapshot(true).LastCheck.IsZero() {
		select {
		case <-deadline:
			t.Fatal("readiness loop did not perform initial check")
		default:
			time.Sleep(time.Millisecond)
		}
	}
	cancel()
	<-done
}

type failingStorage struct{}

func (*failingStorage) Open(context.Context, string) (storage.File, error) {
	return nil, errors.New("unexpected open")
}

func newOperationsTestScanServer(t *testing.T) *scanserver.Server {
	t.Helper()
	s, err := scanserver.NewServer(scanserver.Options{
		Locator: testLocator{},
		Storage: &failingStorage{},
	})
	if err != nil {
		t.Fatal(err)
	}
	return s
}

type testLocator struct{}

func (testLocator) LocateTable(context.Context, string) ([]metadata.TabletInfo, error) {
	return nil, nil
}
