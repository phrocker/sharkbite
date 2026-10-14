// Licensed under the Apache License, Version 2.0. See LICENSE and NOTICE.
package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/phrocker/shoal-oss/pkg/collector"
	"github.com/phrocker/shoal-oss/pkg/collector/api"
	"github.com/phrocker/shoal-oss/pkg/sdk"
)

func TestTailerReturnsOnlyCompleteNewLines(t *testing.T) {
	path := filepath.Join(t.TempDir(), "app.log")
	if err := os.WriteFile(path, []byte("one\ntw"), 0o600); err != nil {
		t.Fatal(err)
	}
	tail := &tailer{path: path}
	lines, err := tail.next()
	if err != nil || len(lines) != 1 || string(lines[0]) != "one" {
		t.Fatalf("first read: %q %v", lines, err)
	}
	f, _ := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	_, _ = f.WriteString("o\nthree\n")
	_ = f.Close()
	lines, err = tail.next()
	if err != nil || len(lines) != 2 || string(lines[0]) != "two" || string(lines[1]) != "three" {
		t.Fatalf("second read: %q %v", lines, err)
	}
}

func TestExtractorDispositions(t *testing.T) {
	now := time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)
	a := artifactFor([]byte("GET /healthz 200"), now)
	o, err := observationFor("collector:tail", a, []byte("GET /healthz 200"), now)
	if err != nil || o.Config().Confidence.Disposition != collector.Extracted || string(o.Config().Payload) != `{"length":16,"fields":3}` {
		t.Fatalf("extracted: %+v %v", o.Config(), err)
	}
	o, err = observationFor("collector:tail", a, []byte(strings.Repeat("x", maxLine+1)), now)
	if err != nil || o.Config().Confidence.Disposition != collector.Unextractable || len(o.Config().Payload) != 0 {
		t.Fatalf("overlong: %+v %v", o.Config(), err)
	}
}

// TestReportsThroughSDK checks the wire requests the SDK sends for one line
// against a stand-in server that speaks the public protocol.
func TestReportsThroughSDK(t *testing.T) {
	var mu sync.Mutex
	var paths []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		paths = append(paths, r.URL.Path)
		mu.Unlock()
		raw, _ := io.ReadAll(r.Body)
		now := time.Now().UTC().Truncate(time.Second)
		var out any
		switch r.URL.Path {
		case api.ArtifactsRoute:
			var in api.ArtifactRequest
			if api.DecodeStrict(raw, &in) != nil {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			out = api.ArtifactReceipt{Schema: api.Schema, CollectorID: in.CollectorID, ArtifactID: in.Artifact.ID, Generation: 1, ReceivedAt: now}
		case api.ObservationsRoute:
			var in api.ObservationRequest
			if api.DecodeStrict(raw, &in) != nil {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			o, err := api.DecodeObservation(in.Observation)
			if err != nil {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			out = api.ObservationReceipt{Schema: api.Schema, ObservationID: api.EncodeID(o.ID()), CollectorID: in.Observation.CollectorID, ArtifactID: in.Observation.ArtifactID, Generation: 1, ReceivedAt: now}
		default:
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_ = json.NewEncoder(w).Encode(out)
	}))
	defer server.Close()
	client, err := sdk.New(sdk.Config{BaseURL: server.URL, Token: func(context.Context) (string, error) { return "token", nil }})
	if err != nil {
		t.Fatal(err)
	}
	if err := report(context.Background(), client, "collector:tail", []byte("GET / 200"), time.Now().UTC().Round(0)); err != nil {
		t.Fatal(err)
	}
	if strings.Join(paths, ",") != api.ArtifactsRoute+","+api.ObservationsRoute {
		t.Fatalf("requests: %v", paths)
	}
}
