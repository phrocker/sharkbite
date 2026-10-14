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

// Package obs is the local observability surface for a running shoal-embed
// engine: liveness/readiness probes, a JSON stats endpoint, and a hand-rolled
// Prometheus text-format /metrics endpoint. It adds zero dependencies — the
// metrics are derived from engine.Stats() and engine.Metrics() and rendered as
// plain text.
package obs

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"

	"github.com/phrocker/shoal-oss/internal/engine"
)

// Source is the read-only engine view the observability server reports on.
// *engine.Engine satisfies it.
type Source interface {
	Stats() []engine.TableStat
	Metrics() engine.Metrics
}

// Server renders observability endpoints for an engine. Readiness is a latch
// the owner flips once the engine is open and its tables are loaded.
type Server struct {
	src   Source
	ready atomic.Bool
}

// NewServer binds an observability server to a stats source.
func NewServer(src Source) *Server {
	return &Server{src: src}
}

// SetReady marks the server ready (true) or not-ready (false). /readyz returns
// 200 only while ready.
func (s *Server) SetReady(ready bool) { s.ready.Store(ready) }

// Handler returns an http.Handler exposing /healthz, /readyz, /stats and
// /metrics.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", s.handleHealthz)
	mux.HandleFunc("/readyz", s.handleReadyz)
	mux.HandleFunc("/stats", s.handleStats)
	mux.HandleFunc("/metrics", s.handleMetrics)
	return mux
}

func (s *Server) handleHealthz(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	fmt.Fprintln(w, "ok")
}

func (s *Server) handleReadyz(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	if !s.ready.Load() {
		w.WriteHeader(http.StatusServiceUnavailable)
		fmt.Fprintln(w, "not ready")
		return
	}
	w.WriteHeader(http.StatusOK)
	fmt.Fprintln(w, "ready")
}

// statsJSON is the shape of the /stats response.
type statsJSON struct {
	Tables   []tableStatJSON `json:"tables"`
	Totals   totalsJSON      `json:"totals"`
	Counters engine.Metrics  `json:"counters"`
}

type tableStatJSON struct {
	Name    string `json:"name"`
	Tablets int    `json:"tablets"`
	RFiles  int    `json:"rfiles"`
}

type totalsJSON struct {
	Tables  int `json:"tables"`
	Tablets int `json:"tablets"`
	RFiles  int `json:"rfiles"`
}

func (s *Server) snapshot() statsJSON {
	stats := s.src.Stats()
	out := statsJSON{Counters: s.src.Metrics()}
	out.Tables = make([]tableStatJSON, 0, len(stats))
	for _, t := range stats {
		out.Tables = append(out.Tables, tableStatJSON{Name: t.Name, Tablets: t.Tablets, RFiles: t.RFiles})
		out.Totals.Tablets += t.Tablets
		out.Totals.RFiles += t.RFiles
	}
	out.Totals.Tables = len(stats)
	return out
}

func (s *Server) handleStats(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(s.snapshot())
}

func (s *Server) handleMetrics(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	snap := s.snapshot()
	var b strings.Builder

	gauge(&b, "shoal_tables", "Number of tables open in the engine.", float64(snap.Totals.Tables), nil)
	gauge(&b, "shoal_tablets_total", "Total tablets across all tables.", float64(snap.Totals.Tablets), nil)
	gauge(&b, "shoal_rfiles_total", "Total RFiles across all tables.", float64(snap.Totals.RFiles), nil)

	// Per-table gauges share one HELP/TYPE header each.
	tables := make([]tableStatJSON, len(snap.Tables))
	copy(tables, snap.Tables)
	sort.Slice(tables, func(i, j int) bool { return tables[i].Name < tables[j].Name })
	writeHeader(&b, "shoal_table_tablets", "Tablets in a table.", "gauge")
	for _, t := range tables {
		writeSample(&b, "shoal_table_tablets", float64(t.Tablets), map[string]string{"table": t.Name})
	}
	writeHeader(&b, "shoal_table_rfiles", "RFiles in a table.", "gauge")
	for _, t := range tables {
		writeSample(&b, "shoal_table_rfiles", float64(t.RFiles), map[string]string{"table": t.Name})
	}

	c := snap.Counters
	counter(&b, "shoal_writes_total", "Total Write calls.", float64(c.Writes))
	counter(&b, "shoal_mutations_total", "Total mutations written.", float64(c.Mutations))
	counter(&b, "shoal_cells_written_total", "Total cells written.", float64(c.CellsWritten))
	counter(&b, "shoal_scans_total", "Total Scan calls.", float64(c.Scans))
	counter(&b, "shoal_flushes_total", "Total Flush calls.", float64(c.Flushes))
	counter(&b, "shoal_compactions_total", "Total Compact calls.", float64(c.Compactions))

	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(b.String()))
}

func writeHeader(b *strings.Builder, name, help, typ string) {
	fmt.Fprintf(b, "# HELP %s %s\n# TYPE %s %s\n", name, help, name, typ)
}

func writeSample(b *strings.Builder, name string, v float64, labels map[string]string) {
	if len(labels) == 0 {
		fmt.Fprintf(b, "%s %s\n", name, formatFloat(v))
		return
	}
	keys := make([]string, 0, len(labels))
	for k := range labels {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, fmt.Sprintf("%s=%q", k, escapeLabel(labels[k])))
	}
	fmt.Fprintf(b, "%s{%s} %s\n", name, strings.Join(parts, ","), formatFloat(v))
}

func gauge(b *strings.Builder, name, help string, v float64, labels map[string]string) {
	writeHeader(b, name, help, "gauge")
	writeSample(b, name, v, labels)
}

func counter(b *strings.Builder, name, help string, v float64) {
	writeHeader(b, name, help, "counter")
	writeSample(b, name, v, nil)
}

func formatFloat(v float64) string {
	return strconv.FormatFloat(v, 'g', -1, 64)
}

// escapeLabel escapes a Prometheus label value (backslash, double-quote,
// newline) per the text exposition format.
func escapeLabel(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, `"`, `\"`)
	s = strings.ReplaceAll(s, "\n", `\n`)
	return s
}
