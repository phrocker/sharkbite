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

package healthsurface

import (
	"context"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/phrocker/shoal-oss/internal/promtext"
	"github.com/phrocker/shoal-oss/internal/roleops"
)

// A producer written for roleops must plug into the health surface unchanged.
// This is a compile-time assertion: it fails to build if the shapes diverge.
var _ MetricsWriter = roleops.MetricsWriter(nil)

const sampleExposition = "# HELP shoal_test_total A test counter.\n" +
	"# TYPE shoal_test_total counter\n" +
	"shoal_test_total{name=\"a\\\"b\"} 3\n"

func writeSample(builder *strings.Builder) { builder.WriteString(sampleExposition) }

// startSurface starts a real health server, so the tests exercise the
// listener the binaries use rather than the mux alone.
func startSurface(t *testing.T, start func() (*Server, error)) string {
	t.Helper()
	health, err := start()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = health.Shutdown(ctx)
	})
	return "http://" + health.Address()
}

func startMetricsSurface(t *testing.T, config Config) string {
	t.Helper()
	return startSurface(t, func() (*Server, error) {
		return StartWithConfig("127.0.0.1:0", &State{}, config)
	})
}

type metricsResponse struct {
	status int
	body   string
	header http.Header
}

func doMetrics(t *testing.T, method, url string) metricsResponse {
	t.Helper()
	request, err := http.NewRequest(method, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatalf("%s %s: %v", method, url, err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	return metricsResponse{status: response.StatusCode, body: string(body), header: response.Header}
}

func TestMetricsServesTheWritersOutput(t *testing.T) {
	base := startMetricsSurface(t, Config{Metrics: writeSample})

	got := doMetrics(t, http.MethodGet, base+"/metrics")
	if got.status != http.StatusOK {
		t.Fatalf("GET /metrics = %d, want 200", got.status)
	}
	if got.body != sampleExposition {
		t.Fatalf("body = %q, want %q", got.body, sampleExposition)
	}
	if ct := got.header.Get("Content-Type"); ct != promtext.ContentType {
		t.Fatalf("content type = %q, want %q", ct, promtext.ContentType)
	}
	if cc := got.header.Get("Cache-Control"); cc != "no-store" {
		t.Fatalf("cache control = %q, want no-store", cc)
	}

	head := doMetrics(t, http.MethodHead, base+"/metrics")
	if head.status != http.StatusOK || head.body != "" {
		t.Fatalf("HEAD /metrics = %d %q, want 200 with no body", head.status, head.body)
	}
	if ct := head.header.Get("Content-Type"); ct != promtext.ContentType {
		t.Fatalf("HEAD content type = %q", ct)
	}

	// The probes are unchanged by the extra route.
	if got := doMetrics(t, http.MethodGet, base+"/healthz"); got.status != http.StatusOK {
		t.Fatalf("/healthz = %d", got.status)
	}
}

// TestMetricsIsAbsentWithoutAWriter pins the default: a host that supplies no
// writer has exactly the surface it had before, for every method and through
// every constructor.
func TestMetricsIsAbsentWithoutAWriter(t *testing.T) {
	bases := []string{
		startMetricsSurface(t, Config{}),
		startSurface(t, func() (*Server, error) { return Start("127.0.0.1:0", &State{}) }),
	}
	for _, base := range bases {
		for _, method := range []string{http.MethodGet, http.MethodHead, http.MethodPost} {
			if got := doMetrics(t, method, base+"/metrics"); got.status != http.StatusNotFound {
				t.Fatalf("%s /metrics with no writer = %d, want 404", method, got.status)
			}
		}
	}
	for name, handler := range map[string]http.Handler{
		"NewHandler":           NewHandler(&State{}),
		"NewHandlerWithConfig": NewHandlerWithConfig(&State{}, Config{}),
	} {
		if got := recordHealth(t, handler, "/metrics"); got.status != http.StatusNotFound {
			t.Fatalf("%s /metrics = %d, want 404", name, got.status)
		}
	}
}

func TestMetricsRefusesOtherMethodsWithoutRendering(t *testing.T) {
	var calls atomic.Int32
	base := startMetricsSurface(t, Config{Metrics: func(builder *strings.Builder) {
		calls.Add(1)
		writeSample(builder)
	}})
	for _, method := range []string{http.MethodPost, http.MethodPut, http.MethodDelete, http.MethodPatch} {
		got := doMetrics(t, method, base+"/metrics")
		if got.status != http.StatusMethodNotAllowed {
			t.Fatalf("%s /metrics = %d, want 405", method, got.status)
		}
		if allow := got.header.Get("Allow"); !strings.Contains(allow, "GET") || !strings.Contains(allow, "HEAD") {
			t.Fatalf("%s /metrics Allow = %q, want GET and HEAD", method, allow)
		}
	}
	if n := calls.Load(); n != 0 {
		t.Fatalf("writer called %d times for refused methods", n)
	}
}

// TestMetricsContainsAPanickingWriter asserts the panic becomes a status a
// scraper can alert on, with no partial exposition, and that the listener is
// still serving afterwards — both probes and further scrapes.
func TestMetricsContainsAPanickingWriter(t *testing.T) {
	base := startMetricsSurface(t, Config{Metrics: func(builder *strings.Builder) {
		builder.WriteString("shoal_partial 1\n")
		panic("writer exploded")
	}})
	for attempt := range 2 {
		got := doMetrics(t, http.MethodGet, base+"/metrics")
		if got.status != http.StatusInternalServerError {
			t.Fatalf("attempt %d: GET /metrics = %d, want 500", attempt, got.status)
		}
		if got.body != "metrics unavailable\n" {
			t.Fatalf("attempt %d: body = %q, want the fixed failure body", attempt, got.body)
		}
		if ct := got.header.Get("Content-Type"); ct == promtext.ContentType {
			t.Fatalf("attempt %d: a failed scrape was labelled as an exposition", attempt)
		}
		if got := doMetrics(t, http.MethodGet, base+"/healthz"); got.status != http.StatusOK {
			t.Fatalf("attempt %d: /healthz after a writer panic = %d", attempt, got.status)
		}
	}
}

// TestDrainIsNotHeldByAScrapeInFlight runs Drain against the real health
// server while a scrape is blocked inside its writer. Readiness must still
// drop before the workspace is asked to stop, and the health close must not
// wait for the writer: without the shutdown signal, this Drain sits out its
// whole deadline and returns it as the error.
func TestDrainIsNotHeldByAScrapeInFlight(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	defer close(release)
	state := &State{}
	health, err := StartWithConfig("127.0.0.1:0", state, Config{Metrics: func(builder *strings.Builder) {
		close(entered)
		<-release
		writeSample(builder)
	}})
	if err != nil {
		t.Fatal(err)
	}
	state.MarkReady()

	scraped := make(chan metricsResponse, 1)
	go func() {
		response, err := http.Get("http://" + health.Address() + "/metrics")
		if err != nil {
			scraped <- metricsResponse{status: -1, body: err.Error()}
			return
		}
		defer response.Body.Close()
		body, _ := io.ReadAll(response.Body)
		scraped <- metricsResponse{status: response.StatusCode, body: string(body)}
	}()
	<-entered

	const deadline = 3 * time.Second
	ctx, cancel := context.WithTimeout(context.Background(), deadline)
	defer cancel()
	workspace := &recordingServer{state: state}
	began := time.Now()
	if err := Drain(ctx, state, workspace, health); err != nil {
		t.Fatalf("drain with a scrape in flight: %v", err)
	}
	if elapsed := time.Since(began); elapsed >= deadline/2 {
		t.Fatalf("drain took %v: the in-flight scrape held the health close", elapsed)
	}
	if !workspace.calledShutdown || workspace.readyAtShutdown {
		t.Fatalf("drain ordering broken: shutdown=%v readyAtShutdown=%v",
			workspace.calledShutdown, workspace.readyAtShutdown)
	}
	select {
	case got := <-scraped:
		if got.status != http.StatusServiceUnavailable {
			t.Fatalf("in-flight scrape = %d %q, want 503", got.status, got.body)
		}
	case <-time.After(deadline):
		t.Fatal("the in-flight scrape was never answered")
	}
}
