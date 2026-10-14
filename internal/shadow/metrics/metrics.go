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

// Package metrics implements the shadow-service metrics surface. It's
// intentionally minimal — atomic counters indexed by table name, plus
// a Prometheus-exposition Render() method so a single goroutine can
// scrape the same data the JSON dashboard reads.
//
// We don't pull in github.com/prometheus/client_golang for V0 — that
// library is heavy, and the metrics in this package are flat counters +
// last-update timestamps. If we later need histograms or labelled
// gauges, switching is a one-package change (the Render shape is the
// only API the operator runbook references).
package metrics

import (
	"fmt"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Registry holds the shadow service's counters. Construct with New;
// safe for concurrent updates.
type Registry struct {
	mu     sync.RWMutex
	tables map[string]*TableMetrics
	// Lifetime totals (not per table) — useful for at-a-glance "is the
	// poller alive" health checks.
	pollerLoops   atomic.Int64
	pollerErrors  atomic.Int64
	oracleErrors  atomic.Int64
	pollerStarted time.Time
}

// New returns a Registry ready for updates.
func New() *Registry {
	return &Registry{
		tables:        map[string]*TableMetrics{},
		pollerStarted: time.Now(),
	}
}

// TableMetrics is the per-watched-table counter set. All fields are
// atomically updatable; Snapshot reads a coherent point-in-time view.
type TableMetrics struct {
	// CompactionsObserved is the number of (oldFiles → newFiles)
	// diffs the poller detected since startup.
	CompactionsObserved atomic.Int64

	// OracleRuns is the number of times the Compare() oracle was
	// invoked for this table. Equal to CompactionsObserved minus
	// races where inputs were GC'd before fetch.
	OracleRuns atomic.Int64

	// T1Failures counts cases where the Java rfile-info validator
	// rejected shoal's output (wire-format bug).
	T1Failures atomic.Int64

	// T2Matches / T2Mismatches gate cell-sequence equivalence outcomes.
	T2Matches    atomic.Int64
	T2Mismatches atomic.Int64

	// T3LookupsProbed is the total number of point-lookups attempted
	// across all oracle runs for this table.
	T3LookupsProbed   atomic.Int64
	T3LookupsMatched  atomic.Int64

	// InputsMissing counts compactions we couldn't shadow because the
	// input files had already been GC'd by the time we tried to fetch.
	// High counts mean the GC window is shorter than oracle latency —
	// operator should reduce poll interval or increase Accumulo GC
	// hold-time.
	InputsMissing atomic.Int64

	// InputsOversized counts compactions skipped because their total
	// input byte size exceeded the configured cap. The oracle buffers
	// every input in memory (plus the Java output, plus shoal's output)
	// for the diff; very large compactions OOM the pod. Skipping them
	// is preferable to dying. Operators inspect this counter to know
	// they're losing coverage on the biggest tablets — the typical
	// follow-up is to either bump memory or sample-shadow large events.
	InputsOversized atomic.Int64

	// LastObservedUnix is the wall-clock epoch second of the most
	// recent observed compaction for this table.
	LastObservedUnix atomic.Int64
}

// PollerLoopOK records one successful poller iteration.
func (r *Registry) PollerLoopOK() { r.pollerLoops.Add(1) }

// PollerError records one transient poller error (ZK / metadata scan
// failure). Not fatal — the poller retries on its next tick.
func (r *Registry) PollerError() { r.pollerErrors.Add(1) }

// OracleError records one oracle invocation that failed (couldn't
// fetch inputs, shoal compaction crashed, etc.).
func (r *Registry) OracleError() { r.oracleErrors.Add(1) }

// For returns the per-table counter set for tableID, allocating it on
// first use.
func (r *Registry) For(tableID string) *TableMetrics {
	r.mu.RLock()
	tm, ok := r.tables[tableID]
	r.mu.RUnlock()
	if ok {
		return tm
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if tm, ok = r.tables[tableID]; ok {
		return tm
	}
	tm = &TableMetrics{}
	r.tables[tableID] = tm
	return tm
}

// Snapshot is a JSON-friendly point-in-time copy of the registry's
// contents. Suitable for the /metrics-json endpoint that operator UIs
// can pull.
type Snapshot struct {
	Uptime       time.Duration            `json:"uptime"`
	PollerLoops  int64                    `json:"poller_loops_total"`
	PollerErrors int64                    `json:"poller_errors_total"`
	OracleErrors int64                    `json:"oracle_errors_total"`
	Tables       map[string]TableSnapshot `json:"tables"`
}

// TableSnapshot is one table's metric values at a moment in time.
type TableSnapshot struct {
	CompactionsObserved int64 `json:"compactions_observed_total"`
	OracleRuns          int64 `json:"oracle_runs_total"`
	T1Failures          int64 `json:"t1_failures_total"`
	T2Matches           int64 `json:"t2_matches_total"`
	T2Mismatches        int64 `json:"t2_mismatches_total"`
	T3LookupsProbed     int64 `json:"t3_lookups_probed_total"`
	T3LookupsMatched    int64 `json:"t3_lookups_matched_total"`
	InputsMissing       int64 `json:"inputs_missing_total"`
	InputsOversized     int64 `json:"inputs_oversized_total"`
	LastObservedUnix    int64 `json:"last_observed_unix"`
}

// Snapshot returns a deep copy of the registry's current values.
func (r *Registry) Snapshot() Snapshot {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := Snapshot{
		Uptime:       time.Since(r.pollerStarted),
		PollerLoops:  r.pollerLoops.Load(),
		PollerErrors: r.pollerErrors.Load(),
		OracleErrors: r.oracleErrors.Load(),
		Tables:       map[string]TableSnapshot{},
	}
	for id, tm := range r.tables {
		out.Tables[id] = TableSnapshot{
			CompactionsObserved: tm.CompactionsObserved.Load(),
			OracleRuns:          tm.OracleRuns.Load(),
			T1Failures:          tm.T1Failures.Load(),
			T2Matches:           tm.T2Matches.Load(),
			T2Mismatches:        tm.T2Mismatches.Load(),
			T3LookupsProbed:     tm.T3LookupsProbed.Load(),
			T3LookupsMatched:    tm.T3LookupsMatched.Load(),
			InputsMissing:       tm.InputsMissing.Load(),
			InputsOversized:     tm.InputsOversized.Load(),
			LastObservedUnix:    tm.LastObservedUnix.Load(),
		}
	}
	return out
}

// Render formats the snapshot as Prometheus exposition text. Counters
// labelled by table; bare counters at the top.
//
// The HELP / TYPE lines follow Prometheus conventions; alert rules in
// the Phase 3 runbook reference these metric names byte-exactly.
func (r *Registry) Render() string {
	snap := r.Snapshot()
	var b strings.Builder

	writeCounter := func(name, help string, val int64) {
		fmt.Fprintf(&b, "# HELP %s %s\n", name, help)
		fmt.Fprintf(&b, "# TYPE %s counter\n", name)
		fmt.Fprintf(&b, "%s %d\n", name, val)
	}
	writeGauge := func(name, help string, val float64) {
		fmt.Fprintf(&b, "# HELP %s %s\n", name, help)
		fmt.Fprintf(&b, "# TYPE %s gauge\n", name)
		fmt.Fprintf(&b, "%s %g\n", name, val)
	}

	writeCounter("shadow_poller_loops_total", "Successful poller iterations.", snap.PollerLoops)
	writeCounter("shadow_poller_errors_total", "Transient poller errors.", snap.PollerErrors)
	writeCounter("shadow_oracle_errors_total", "Oracle invocations that errored out.", snap.OracleErrors)
	writeGauge("shadow_uptime_seconds", "Shadow service uptime.", snap.Uptime.Seconds())

	// Stable iteration order so scrape output is byte-deterministic.
	ids := make([]string, 0, len(snap.Tables))
	for id := range snap.Tables {
		ids = append(ids, id)
	}
	sort.Strings(ids)

	tableLabel := func(name, table string) string {
		return fmt.Sprintf("%s{table=%q}", name, table)
	}

	for _, id := range ids {
		t := snap.Tables[id]
		writeCounter(tableLabel("shadow_compactions_observed_total", id),
			"Compactions detected by the poller, per table.", t.CompactionsObserved)
		writeCounter(tableLabel("shadow_oracle_runs_total", id),
			"Oracle invocations attempted, per table.", t.OracleRuns)
		writeCounter(tableLabel("shadow_t1_failures_total", id),
			"Cases where Java rfile-info rejected shoal output.", t.T1Failures)
		writeCounter(tableLabel("shadow_t2_matches_total", id),
			"Cell-sequence equivalence successes.", t.T2Matches)
		writeCounter(tableLabel("shadow_t2_mismatches_total", id),
			"Cell-sequence equivalence failures.", t.T2Mismatches)
		writeCounter(tableLabel("shadow_t3_lookups_probed_total", id),
			"T3 random point-lookup probes attempted.", t.T3LookupsProbed)
		writeCounter(tableLabel("shadow_t3_lookups_matched_total", id),
			"T3 probes that agreed between shoal and Java.", t.T3LookupsMatched)
		writeCounter(tableLabel("shadow_inputs_missing_total", id),
			"Compactions skipped because input files were GC'd before fetch.", t.InputsMissing)
		writeCounter(tableLabel("shadow_inputs_oversized_total", id),
			"Compactions skipped because total input bytes exceeded the oracle's memory cap.", t.InputsOversized)
		writeGauge(tableLabel("shadow_last_observed_unix", id),
			"Wall-clock epoch second of the latest observed compaction.", float64(t.LastObservedUnix))
	}
	return b.String()
}
