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
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/phrocker/shoal-oss/internal/cred"
	"github.com/phrocker/shoal-oss/internal/iterrt"
	"github.com/phrocker/shoal-oss/internal/metadata"
	"github.com/phrocker/shoal-oss/internal/shadow"
	"github.com/phrocker/shoal-oss/internal/shadow/itercfg"
	shmetrics "github.com/phrocker/shoal-oss/internal/shadow/metrics"
	"github.com/phrocker/shoal-oss/internal/shadow/poller"
	"github.com/phrocker/shoal-oss/internal/storage"
	"github.com/phrocker/shoal-oss/internal/zk"
)

// serviceOptions is the parsed --service-mode CLI surface. Built in
// main() from the same flagset; this file owns no flags of its own.
type serviceOptions struct {
	zkServers         []string
	instanceName      string
	accumuloVersion   string
	user              string
	password          string
	zkTimeout         time.Duration
	tables            []string
	pollInterval      time.Duration
	reportPrefix      string
	httpListen        string
	oracleConcurrency int
	lookupSamples     int
	lookupSeed        int64
	itercfgTTL        time.Duration
	uploadShoalOutput bool
	// maxInputBytes caps the total input-file size the oracle will
	// shadow. Larger events are skipped + counted via
	// shadow_inputs_oversized_total. Zero disables the cap.
	maxInputBytes int64
}

// runService is the long-running daemon entry point. Bootstraps the
// shared ZK locator + metadata.Walker, builds one poller per requested
// table, and runs an oracle worker pool that consumes the poller
// channel. Returns when ctx is cancelled (graceful shutdown).
func runService(ctx context.Context, opts serviceOptions, logger *slog.Logger) error {
	logger.Info("shadow service starting",
		slog.Any("tables", opts.tables),
		slog.Duration("poll_interval", opts.pollInterval),
		slog.Int("oracle_concurrency", opts.oracleConcurrency),
		slog.String("report_prefix", opts.reportPrefix),
	)

	// Accumulo's table /config znodes carry ZK ACLs that require
	// digest auth as "accumulo:<instance-secret>". On the deployments
	// we target the instance secret is the same value the root user
	// authenticates with, so SHOAL_PASSWORD doubles as the ZK secret.
	loc, err := zk.NewWithAuth(opts.zkServers, opts.instanceName, opts.zkTimeout, opts.password)
	if err != nil {
		return fmt.Errorf("zk.New: %w", err)
	}
	defer loc.Close()
	logger.Info("zk connected", slog.String("instance_id", loc.InstanceID()))

	creds := cred.NewPasswordCreds(opts.user, opts.password, loc.InstanceID())
	walker := metadata.NewWalker(loc, creds, opts.accumuloVersion).WithLogger(
		logger.With(slog.String("subsystem", "walker")),
	)

	resolver := itercfg.NewResolver(loc, opts.itercfgTTL, logger.With(slog.String("subsystem", "itercfg")))
	reg := shmetrics.New()

	// Resolve table names → IDs up front. A missing table is fatal —
	// no point running a poller for a table that doesn't exist.
	tableIDs := make([]string, 0, len(opts.tables))
	tableNameOf := map[string]string{} // id → human-readable name
	for _, name := range opts.tables {
		id, err := resolver.ResolveTableID(ctx, name)
		if err != nil {
			return fmt.Errorf("resolve table %q: %w", name, err)
		}
		tableIDs = append(tableIDs, id)
		tableNameOf[id] = name
		logger.Info("watch table",
			slog.String("name", name),
			slog.String("id", id),
		)
	}

	// Shared event channel — pollers fan in, oracle workers fan out.
	// Buffer = (#tables * 4) gives a small backlog without letting a
	// stuck worker pool back up the metadata walks.
	bufSize := len(tableIDs) * 4
	if bufSize < 8 {
		bufSize = 8
	}
	events := make(chan poller.Event, bufSize)

	// HTTP introspection: /metrics (Prometheus), /metrics-json (JSON),
	// /healthz. The handler closes when ctx is cancelled.
	if opts.httpListen != "" {
		go serveHTTP(ctx, opts.httpListen, reg, logger)
	}

	// Start one poller per watched table.
	var pollerWG sync.WaitGroup
	for _, id := range tableIDs {
		p, err := poller.New(poller.Config{
			TableID:      id,
			PollInterval: opts.pollInterval,
			Logger:       logger.With(slog.String("subsystem", "poller")),
			Metrics:      reg,
		}, walker, events)
		if err != nil {
			return fmt.Errorf("poller for %s: %w", id, err)
		}
		pollerWG.Add(1)
		go func(id string) {
			defer pollerWG.Done()
			if err := p.Run(ctx); err != nil && !errors.Is(err, context.Canceled) {
				logger.Error("poller exited",
					slog.String("table", id), slog.Any("err", err))
			}
		}(id)
	}

	// Oracle worker pool. Bounded concurrency keeps GCS fetch + shoal
	// compaction work from saturating CPU when many compactions land
	// at once (e.g. operator runs `compact -t graph_vidx`).
	workerCount := opts.oracleConcurrency
	if workerCount <= 0 {
		workerCount = 4
	}
	var workerWG sync.WaitGroup
	for i := 0; i < workerCount; i++ {
		workerWG.Add(1)
		go func(wid int) {
			defer workerWG.Done()
			workerLogger := logger.With(
				slog.String("subsystem", "oracle"),
				slog.Int("worker", wid),
			)
			for {
				select {
				case <-ctx.Done():
					return
				case ev, ok := <-events:
					if !ok {
						return
					}
					handleEvent(ctx, ev, resolver, reg, opts, tableNameOf, workerLogger)
				}
			}
		}(i)
	}

	// Wait for shutdown signal.
	<-ctx.Done()
	logger.Info("shutdown signal received; draining")

	// Pollers exit on ctx.Done; wait for them, then close events so
	// workers can drain.
	pollerWG.Wait()
	close(events)
	workerWG.Wait()
	logger.Info("shadow service exit clean")
	return nil
}

// handleEvent fetches the event's inputs + output, resolves the
// iterator stack from ZK, runs the oracle, and uploads the report.
// Errors are logged but never propagated — the service keeps running.
func handleEvent(
	ctx context.Context,
	ev poller.Event,
	resolver *itercfg.Resolver,
	reg *shmetrics.Registry,
	opts serviceOptions,
	tableNameOf map[string]string,
	logger *slog.Logger,
) {
	t := ev.Tablet
	tm := reg.For(t.TableID)
	tm.OracleRuns.Add(1)

	logger = logger.With(
		slog.String("table_id", t.TableID),
		slog.String("table", tableNameOf[t.TableID]),
		slog.String("kind", ev.Kind.String()),
		slog.String("end_row", metadata.PrintableBytes(t.EndRow)),
	)

	// Determine scope. Compactions are majc; flushes are minc.
	scope := iterrt.ScopeMajc
	if ev.Kind == poller.KindFlush {
		scope = iterrt.ScopeMinc
	}

	resolved, err := resolver.Resolve(ctx, t.TableID, scope)
	if err != nil {
		reg.OracleError()
		logger.Warn("itercfg resolve failed", slog.Any("err", err))
		return
	}
	if !resolved.HasShoalCoverage() {
		// Log once-per-event but skip the actual oracle run — the
		// stack contains iterators we don't yet implement, so any
		// diff would be invalid.
		logger.Warn("table has iterators not in shoal allowlist; skipping",
			slog.Any("skipped", resolved.Skipped))
		return
	}

	// Compactions always have exactly one Java-produced output in
	// NewFiles[0] in steady state. Multiple outputs would mean a
	// split-during-compaction race; skip those — too rare to handle.
	if len(ev.NewFiles) != 1 {
		logger.Warn("event has non-1 output; skipping",
			slog.Int("outputs", len(ev.NewFiles)))
		return
	}
	javaOutPath := ev.NewFiles[0].Path

	// Memory guard: the oracle buffers every input file in memory, plus
	// the Java output, plus shoal's output, plus a cloned-cells slice
	// for T2 walking. Pathological compactions (large `graph` tablets)
	// can balloon that working set past the pod's limit and OOM. Skip
	// events where the sum of input sizes (reported by metadata) blows
	// past opts.maxInputBytes; surface as a metric so operators see
	// they're losing coverage on the big tablets.
	var totalInputBytes int64
	for _, f := range ev.OldFiles {
		totalInputBytes += f.Size
	}
	if opts.maxInputBytes > 0 && totalInputBytes > opts.maxInputBytes {
		tm.InputsOversized.Add(1)
		logger.Info("event inputs exceed memory cap; skipping",
			slog.Int64("total_input_bytes", totalInputBytes),
			slog.Int64("cap_bytes", opts.maxInputBytes),
			slog.Int("input_count", len(ev.OldFiles)),
		)
		return
	}

	// Fetch inputs + output. ErrNotFound on any input is the
	// "GC raced us" signal — bump the inputs-missing counter and
	// quietly skip.
	inputBlobs := make([]shadow.InputBlob, 0, len(ev.OldFiles))
	for _, f := range ev.OldFiles {
		b, err := readBlob(ctx, f.Path)
		if err != nil {
			if errors.Is(err, storage.ErrNotFound) {
				tm.InputsMissing.Add(1)
				logger.Info("input file already GC'd; skipping event",
					slog.String("path", f.Path))
				return
			}
			reg.OracleError()
			logger.Warn("fetch input failed",
				slog.String("path", f.Path), slog.Any("err", err))
			return
		}
		inputBlobs = append(inputBlobs, shadow.InputBlob{Name: f.Path, Bytes: b})
	}
	javaBytes, err := readBlob(ctx, javaOutPath)
	if err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			tm.InputsMissing.Add(1)
			logger.Info("java output already GC'd; skipping event",
				slog.String("path", javaOutPath))
			return
		}
		reg.OracleError()
		logger.Warn("fetch java output failed",
			slog.String("path", javaOutPath), slog.Any("err", err))
		return
	}

	// Flushes write to a single new file with no prior inputs. The
	// oracle expects at least one input to compose a stack over; skip
	// flush events for now (they're a Phase 2-extension target).
	if ev.Kind == poller.KindFlush {
		logger.Info("flush event: oracle currently shadows compactions only")
		return
	}

	spec := shadow.CompareSpec{
		Inputs:              inputBlobs,
		Stack:               resolved.Stack,
		Scope:               scope,
		FullMajorCompaction: false, // unknown from metadata; conservative.
		LookupSamples:       opts.lookupSamples,
		LookupSeed:          opts.lookupSeed,
	}

	report, err := shadow.Compare(spec, javaBytes)
	if err != nil {
		reg.OracleError()
		logger.Error("oracle errored", slog.Any("err", err))
		return
	}

	// Record outcomes.
	if report.T1.Attempted {
		if !report.T1.Passed {
			tm.T1Failures.Add(1)
		}
	}
	if report.T2.Attempted {
		if report.T2.Passed {
			tm.T2Matches.Add(1)
		} else {
			tm.T2Mismatches.Add(1)
		}
	}
	if report.T3.Attempted {
		tm.T3LookupsProbed.Add(int64(report.T3.LookupsTotal))
		tm.T3LookupsMatched.Add(int64(report.T3.LookupsMatched))
	}

	t3Total := report.T3.LookupsTotal
	t3Match := report.T3.LookupsMatched

	level := slog.LevelInfo
	if !report.T2.Passed || (report.T1.Attempted && !report.T1.Passed) {
		level = slog.LevelError
	}
	logger.Log(ctx, level, "shadow report",
		slog.Int("inputs", len(inputBlobs)),
		slog.String("output", javaOutPath),
		slog.Int64("shoal_cells", report.ShoalCells),
		slog.Int64("java_cells", report.JavaCells),
		slog.Bool("t1_pass", !report.T1.Attempted || report.T1.Passed),
		slog.Bool("t2_match", report.T2.Passed),
		slog.String("t3_match", fmt.Sprintf("%d/%d", t3Match, t3Total)),
		slog.Int64("dur_ms", report.ShoalCompactMs),
	)

	// Optional report upload — the gs://prefix/<table>/<uuid>.json
	// shape is the Phase 3 runbook's contract.
	if opts.reportPrefix != "" {
		if err := uploadReport(ctx, opts.reportPrefix, t.TableID, ev, report, javaOutPath); err != nil {
			logger.Warn("report upload failed",
				slog.String("prefix", opts.reportPrefix), slog.Any("err", err))
		}
	}
}

// uploadReport writes a JSON report to opts.reportPrefix. The path is
// <prefix>/<table-id>/<unix-nanos>.json — collision-free under sub-
// nanosecond throughput, and chronologically sortable for the runbook's
// "show me the last 24h of reports" recipe.
func uploadReport(
	ctx context.Context,
	prefix, tableID string,
	ev poller.Event,
	report *shadow.Report,
	javaOutPath string,
) error {
	type wireReport struct {
		Table      string         `json:"table"`
		Kind       string         `json:"kind"`
		EndRow     string         `json:"end_row"`
		Inputs     []string       `json:"inputs"`
		JavaOutput string         `json:"java_output"`
		ObservedAt string         `json:"observed_at"`
		Report     *shadow.Report `json:"report"`
	}
	inputs := make([]string, 0, len(ev.OldFiles))
	for _, f := range ev.OldFiles {
		inputs = append(inputs, f.Path)
	}
	wr := wireReport{
		Table:      tableID,
		Kind:       ev.Kind.String(),
		EndRow:     metadata.PrintableBytes(ev.Tablet.EndRow),
		Inputs:     inputs,
		JavaOutput: javaOutPath,
		ObservedAt: ev.ObservedAt.UTC().Format(time.RFC3339Nano),
		Report:     report,
	}
	b, err := json.MarshalIndent(wr, "", "  ")
	if err != nil {
		return err
	}
	dst := joinPrefix(prefix, tableID, fmt.Sprintf("%d.json", ev.ObservedAt.UnixNano()))
	be, dstPath, closer, err := backendFor(ctx, dst)
	if err != nil {
		return err
	}
	if closer != nil {
		defer closer()
	}
	return storage.WriteAll(ctx, be, dstPath, b)
}

// joinPrefix concatenates a gs:// or local prefix with subdir + leaf.
// Local paths use os.PathSeparator; gs:// always uses '/'.
func joinPrefix(prefix, sub, leaf string) string {
	if strings.HasPrefix(prefix, "gs://") {
		return strings.TrimRight(prefix, "/") + "/" + sub + "/" + leaf
	}
	return path.Join(prefix, sub, leaf)
}

// serveHTTP exposes /metrics (Prometheus), /metrics-json (JSON), and
// /healthz. Returns when ctx is cancelled.
func serveHTTP(ctx context.Context, addr string, reg *shmetrics.Registry, logger *slog.Logger) {
	mux := http.NewServeMux()
	mux.HandleFunc("/metrics", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; version=0.0.4")
		_, _ = io.WriteString(w, reg.Render())
	})
	mux.HandleFunc("/metrics-json", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(reg.Snapshot())
	})
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "ok\n")
	})

	srv := &http.Server{
		Addr:              addr,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}
	go func() {
		<-ctx.Done()
		shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutCtx)
	}()
	logger.Info("metrics endpoint up", slog.String("addr", addr))
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		logger.Error("metrics endpoint failed", slog.Any("err", err))
	}
}

// installSignalContext wraps parent with a context that cancels on
// SIGINT or SIGTERM. The returned cleanup stops listening for signals.
func installSignalContext(parent context.Context) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancel(parent)
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		select {
		case <-ch:
			cancel()
		case <-ctx.Done():
		}
		signal.Stop(ch)
	}()
	return ctx, cancel
}
