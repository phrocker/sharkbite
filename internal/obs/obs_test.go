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

package obs_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/phrocker/shoal-oss/internal/engine"
	"github.com/phrocker/shoal-oss/internal/obs"
)

type fakeSource struct {
	stats   []engine.TableStat
	metrics engine.Metrics
}

func (f fakeSource) Stats() []engine.TableStat { return f.stats }
func (f fakeSource) Metrics() engine.Metrics   { return f.metrics }

func newTestServer() (*obs.Server, http.Handler) {
	src := fakeSource{
		stats: []engine.TableStat{
			{Name: "graph", Tablets: 3, RFiles: 5},
			{Name: "log", Tablets: 1, RFiles: 2},
		},
		metrics: engine.Metrics{Writes: 10, Mutations: 25, CellsWritten: 40, Scans: 7, Flushes: 2, Compactions: 1},
	}
	s := obs.NewServer(src)
	return s, s.Handler()
}

func get(t *testing.T, h http.Handler, path string) (*httptest.ResponseRecorder, string) {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
	return rec, rec.Body.String()
}

func TestHealthz(t *testing.T) {
	_, h := newTestServer()
	rec, _ := get(t, h, "/healthz")
	if rec.Code != http.StatusOK {
		t.Errorf("/healthz = %d, want 200", rec.Code)
	}
}

func TestReadyzTransition(t *testing.T) {
	s, h := newTestServer()
	rec, _ := get(t, h, "/readyz")
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("/readyz before ready = %d, want 503", rec.Code)
	}
	s.SetReady(true)
	rec, _ = get(t, h, "/readyz")
	if rec.Code != http.StatusOK {
		t.Errorf("/readyz after ready = %d, want 200", rec.Code)
	}
}

func TestStatsJSON(t *testing.T) {
	_, h := newTestServer()
	rec, body := get(t, h, "/stats")
	if rec.Code != http.StatusOK {
		t.Fatalf("/stats = %d, want 200", rec.Code)
	}
	var got struct {
		Tables []struct {
			Name    string `json:"name"`
			Tablets int    `json:"tablets"`
			RFiles  int    `json:"rfiles"`
		} `json:"tables"`
		Totals struct {
			Tables  int `json:"tables"`
			Tablets int `json:"tablets"`
			RFiles  int `json:"rfiles"`
		} `json:"totals"`
		Counters engine.Metrics `json:"counters"`
	}
	if err := json.Unmarshal([]byte(body), &got); err != nil {
		t.Fatalf("decode /stats: %v\n%s", err, body)
	}
	if len(got.Tables) != 2 {
		t.Errorf("tables len = %d, want 2", len(got.Tables))
	}
	if got.Totals.Tables != 2 || got.Totals.Tablets != 4 || got.Totals.RFiles != 7 {
		t.Errorf("totals = %+v, want {2 4 7}", got.Totals)
	}
	if got.Counters.Writes != 10 || got.Counters.CellsWritten != 40 {
		t.Errorf("counters = %+v", got.Counters)
	}
}

func TestMetricsText(t *testing.T) {
	_, h := newTestServer()
	rec, body := get(t, h, "/metrics")
	if rec.Code != http.StatusOK {
		t.Fatalf("/metrics = %d, want 200", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/plain") {
		t.Errorf("content-type = %q", ct)
	}
	want := []string{
		"shoal_tables 2",
		"shoal_tablets_total 4",
		"shoal_rfiles_total 7",
		`shoal_table_tablets{table="graph"} 3`,
		`shoal_table_rfiles{table="log"} 2`,
		"shoal_writes_total 10",
		"shoal_mutations_total 25",
		"shoal_cells_written_total 40",
		"shoal_scans_total 7",
		"shoal_flushes_total 2",
		"shoal_compactions_total 1",
		"# TYPE shoal_writes_total counter",
		"# TYPE shoal_tables gauge",
	}
	for _, w := range want {
		if !strings.Contains(body, w) {
			t.Errorf("/metrics missing %q\n---\n%s", w, body)
		}
	}
}
