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

// shoal-compactor-shadow is the shoal shadow-compaction oracle. It runs
// in two modes:
//
//   - Phase 1 (single-shot): explicit (inputs, iterator stack, optional
//     java-output) args; runs the four-tier parity oracle once and
//     prints a structured report.
//
//   - Phase 2 (service): long-running daemon that watches Accumulo
//     metadata for compaction events on operator-listed tables, runs
//     the oracle automatically on each, uploads structured reports,
//     and exposes Prometheus metrics.
//
// The oracle entry point itself (shadow.Compare) is shared between
// modes.
//
// Examples:
//
//	# Phase 1 sanity (T2/T3 skipped; just confirms shoal can compact):
//	shoal-compactor-shadow \
//	    --inputs gs://example-bucket/tenants/<cluster>/.../A001.rf,gs://.../A002.rf \
//	    --iterators "versioning:maxVersions=10" \
//	    --scope majc \
//	    --output-rfile gs://example-bucket/shoal-output/$(uuidgen).rf
//
//	# Phase 1 full T1+T2+T3 with the Java validator wired:
//	export SHOAL_JAVA_RFILE_VALIDATE='accumulo file rfile-info $RFILE'
//	shoal-compactor-shadow \
//	    --inputs gs://.../A001.rf,gs://.../A002.rf \
//	    --java-output gs://.../A003.rf \
//	    --iterators "versioning:maxVersions=10;deleting" \
//	    --scope majc --full-major=false
//
//	# Phase 2 service:
//	shoal-compactor-shadow --service \
//	    --zk zk-0:2181,zk-1:2181,zk-2:2181 \
//	    --instance accumulo \
//	    --accumulo-version 4.0.0-SNAPSHOT \
//	    --user root \
//	    --tables graph_vidx,graph \
//	    --poll-interval 5s \
//	    --report-prefix gs://example-bucket/shadow-reports \
//	    --http-listen :9810
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"
	"time"

	"github.com/phrocker/shoal-oss/internal/compaction"
	"github.com/phrocker/shoal-oss/internal/iterrt"
	"github.com/phrocker/shoal-oss/internal/shadow"
	"github.com/phrocker/shoal-oss/internal/storage"
	"github.com/phrocker/shoal-oss/internal/storage/gcs"
	"github.com/phrocker/shoal-oss/internal/storage/hdfs"
	"github.com/phrocker/shoal-oss/internal/storage/local"
)

func main() {
	// Phase 1 (single-shot) flags.
	inputs := flag.String("inputs", "", "comma-separated list of input RFile paths (gs://, hdfs://, or local)")
	javaOutput := flag.String("java-output", "", "java-produced RFile to diff against (optional; gs://, hdfs://, or local). Empty = shoal-only sanity (T2/T3 skipped).")
	iterSpec := flag.String("iterators", "",
		"semicolon-separated iterator stack, each item is 'name[:opt=val,opt=val]'. Built bottom-up.\n"+
			"e.g. 'versioning:maxVersions=10;deleting' applies versioning then deleting.")
	scopeArg := flag.String("scope", "majc", "compaction scope: scan | minc | majc")
	fullMajor := flag.Bool("full-major", false, "true when the compaction's output is the tablet's sole remaining file (matters only at scope=majc)")
	codec := flag.String("codec", "", "output RFile block codec: '' | 'none' | 'gz' | 'snappy'")
	lookupSamples := flag.Int("lookup-samples", shadow.DefaultLookupSamples, "T3 random point-lookup probe count (0 uses default)")
	lookupSeed := flag.Int64("lookup-seed", 1, "T3 RNG seed for reproducible probes")
	outputRFile := flag.String("output-rfile", "",
		"optional gs:// or local path to upload shoal's output bytes to (for later inspection by operator)")
	reportOut := flag.String("report-out", "", "optional gs:// or local path to write the JSON report; empty = stdout only")
	minLookupMatch := flag.Float64("min-lookup-match", 1.0,
		"minimum T3 (matched/total) ratio required for exit-0; below this, exit code = 3 (only when --java-output set)")
	timeout := flag.Duration("timeout", 10*time.Minute, "overall deadline for fetching inputs + running oracle (Phase 1 only)")

	// Phase 2 (service) flags. Inactive unless --service is set.
	serviceMode := flag.Bool("service", false,
		"run as a long-running shadow service that auto-detects compactions via metadata polling")
	zkServers := flag.String("zk", "", "comma-separated ZK quorum (service mode)")
	instanceName := flag.String("instance", "accumulo", "Accumulo instance name (service mode)")
	accVersion := flag.String("accumulo-version", "4.0.0-SNAPSHOT", "server major.minor must match (service mode)")
	user := flag.String("user", "root", "principal for metadata scans (service mode)")
	password := flag.String("password", "", "password for the metadata-scan principal (service mode; prefer SHOAL_PASSWORD env)")
	zkTimeout := flag.Duration("zk-timeout", 30*time.Second, "ZK session timeout (service mode)")
	tables := flag.String("tables", "",
		"comma-separated table NAMES to watch (service mode). E.g. 'graph_vidx,graph'.")
	pollInterval := flag.Duration("poll-interval", 5*time.Second,
		"how often the poller scans metadata for compactions (service mode)")
	reportPrefix := flag.String("report-prefix", "",
		"gs:// or local prefix where per-event JSON reports are uploaded (service mode). Empty = log-only.")
	httpListen := flag.String("http-listen", ":9810",
		"address for the metrics + healthz HTTP listener (service mode); empty disables")
	oracleConcurrency := flag.Int("oracle-concurrency", 4,
		"oracle worker pool size for concurrent compactions (service mode)")
	itercfgTTL := flag.Duration("itercfg-ttl", 5*time.Minute,
		"how long resolved iterator stacks are cached before re-reading ZK (service mode)")
	maxInputBytes := flag.Int64("max-input-bytes", 500*1024*1024,
		"skip events whose total input file bytes exceed this cap. 0 disables. "+
			"Default ~500 MB protects a 4 GiB pod from OOM on large `graph` tablets; "+
			"raise if memory headroom allows or operator wants full coverage.")
	logLevel := flag.String("log-level", "info",
		"slog level: debug, info, warn, error (service mode)")

	flag.Parse()

	if *serviceMode {
		runServiceMain(
			*serviceMode, *zkServers, *instanceName, *accVersion,
			*user, *password, *zkTimeout, *tables, *pollInterval,
			*reportPrefix, *httpListen, *oracleConcurrency, *itercfgTTL,
			*logLevel, *lookupSamples, *lookupSeed, *maxInputBytes,
		)
		return
	}

	if *inputs == "" {
		fmt.Fprintln(os.Stderr, "shoal-compactor-shadow: --inputs is required (or pass --service for daemon mode)")
		flag.Usage()
		os.Exit(2)
	}
	scope, err := parseScope(*scopeArg)
	if err != nil {
		fmt.Fprintf(os.Stderr, "shoal-compactor-shadow: %v\n", err)
		os.Exit(2)
	}
	stack, err := parseIteratorStack(*iterSpec)
	if err != nil {
		fmt.Fprintf(os.Stderr, "shoal-compactor-shadow: --iterators: %v\n", err)
		os.Exit(2)
	}

	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()

	// Fetch inputs in order.
	inputPaths := splitCSV(*inputs)
	inputBlobs := make([]shadow.InputBlob, 0, len(inputPaths))
	for _, p := range inputPaths {
		b, err := readBlob(ctx, p)
		if err != nil {
			fmt.Fprintf(os.Stderr, "shoal-compactor-shadow: read input %s: %v\n", p, err)
			os.Exit(1)
		}
		inputBlobs = append(inputBlobs, shadow.InputBlob{Name: p, Bytes: b})
	}
	var javaBytes []byte
	if *javaOutput != "" {
		javaBytes, err = readBlob(ctx, *javaOutput)
		if err != nil {
			fmt.Fprintf(os.Stderr, "shoal-compactor-shadow: read java output %s: %v\n", *javaOutput, err)
			os.Exit(1)
		}
	}

	spec := shadow.CompareSpec{
		Inputs:              inputBlobs,
		Stack:               stack,
		Scope:               scope,
		FullMajorCompaction: *fullMajor,
		Codec:               *codec,
		LookupSamples:       *lookupSamples,
		LookupSeed:          *lookupSeed,
	}

	report, err := shadow.Compare(spec, javaBytes)
	if err != nil {
		fmt.Fprintf(os.Stderr, "shoal-compactor-shadow: oracle error: %v\n", err)
		os.Exit(1)
	}

	// Optional upload of shoal's output for operator inspection. We run
	// shoal compaction inside Compare; the output bytes aren't returned
	// because the oracle doesn't need them after the diff. If the user
	// wants to inspect them, we run a second compaction.
	if *outputRFile != "" {
		if err := writeShoalOutput(ctx, spec, *outputRFile); err != nil {
			fmt.Fprintf(os.Stderr, "shoal-compactor-shadow: write output rfile: %v\n", err)
			os.Exit(1)
		}
	}

	if err := emitReport(ctx, report, *reportOut, inputPaths, *javaOutput); err != nil {
		fmt.Fprintf(os.Stderr, "shoal-compactor-shadow: emit report: %v\n", err)
		os.Exit(1)
	}

	// Exit-code policy:
	//   0 — passed (or no Java side supplied)
	//   2 — usage error (handled above)
	//   3 — divergence past gates
	//   1 — internal error (handled above)
	if javaBytes != nil {
		switch {
		case report.T1.Attempted && !report.T1.Passed:
			os.Exit(3)
		case !report.T2.Passed:
			os.Exit(3)
		case report.T3.LookupsTotal > 0:
			ratio := float64(report.T3.LookupsMatched) / float64(report.T3.LookupsTotal)
			if ratio < *minLookupMatch {
				os.Exit(3)
			}
		}
	}
}

func parseScope(s string) (iterrt.IteratorScope, error) {
	switch strings.ToLower(s) {
	case "scan":
		return iterrt.ScopeScan, nil
	case "minc":
		return iterrt.ScopeMinc, nil
	case "majc":
		return iterrt.ScopeMajc, nil
	default:
		return 0, fmt.Errorf("--scope: unknown %q (want scan|minc|majc)", s)
	}
}

// parseIteratorStack accepts the human-friendly form:
//
//	"versioning:maxVersions=10;deleting;latentEdgeDiscovery:similarityThreshold=0.85"
//
// Each ';'-separated item is one IterSpec; the part before ':' is the
// iterator name, the rest is comma-separated key=val options.
func parseIteratorStack(s string) ([]iterrt.IterSpec, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil, nil
	}
	out := []iterrt.IterSpec{}
	for _, item := range strings.Split(s, ";") {
		item = strings.TrimSpace(item)
		if item == "" {
			continue
		}
		name, rest, _ := strings.Cut(item, ":")
		spec := iterrt.IterSpec{Name: strings.TrimSpace(name)}
		if rest != "" {
			spec.Options = map[string]string{}
			for _, kv := range strings.Split(rest, ",") {
				kv = strings.TrimSpace(kv)
				if kv == "" {
					continue
				}
				k, v, ok := strings.Cut(kv, "=")
				if !ok {
					return nil, fmt.Errorf("iterator %q: option %q lacks '='", name, kv)
				}
				spec.Options[strings.TrimSpace(k)] = strings.TrimSpace(v)
			}
		}
		out = append(out, spec)
	}
	return out, nil
}

func splitCSV(s string) []string {
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

// readBlob accepts gs:// (GCS) or any other string (treated as local FS).
func readBlob(ctx context.Context, path string) ([]byte, error) {
	be, srcPath, closer, err := backendFor(ctx, path)
	if err != nil {
		return nil, err
	}
	if closer != nil {
		defer closer()
	}
	f, err := be.Open(ctx, srcPath)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	buf := make([]byte, f.Size())
	_, err = f.ReadAt(buf, 0)
	if err != nil && !errors.Is(err, io.EOF) {
		return nil, err
	}
	return buf, nil
}

// backendFor picks the correct storage backend for the given path and
// returns a closer for the backend-specific cleanup (GCS client).
func backendFor(ctx context.Context, path string) (storage.Backend, string, func(), error) {
	if strings.HasPrefix(path, "gs://") {
		be, err := gcs.New(ctx)
		if err != nil {
			return nil, "", nil, fmt.Errorf("gcs.New: %w", err)
		}
		return be, path, func() { _ = be.Close() }, nil
	}
	if strings.HasPrefix(path, "hdfs:") {
		address := os.Getenv("SHOAL_HDFS_NAMENODE")
		if address == "" {
			var err error
			address, err = hdfs.AddressFromPath(path)
			if err != nil {
				return nil, "", nil, err
			}
		}
		be, err := hdfs.NewContext(ctx, address)
		if err != nil {
			return nil, "", nil, fmt.Errorf("hdfs.New: %w", err)
		}
		return be, path, func() { _ = be.Close() }, nil
	}
	return local.New(), path, nil, nil
}

func writeShoalOutput(ctx context.Context, spec shadow.CompareSpec, dstPath string) error {
	// Re-run shoal compaction to get the output bytes. shadow.Compare
	// discards them after the diff — kept simple by keeping the oracle
	// pure. The extra pass is negligible vs. the GCS fetch + diff work.
	inputs := make([]compaction.Input, len(spec.Inputs))
	for i, b := range spec.Inputs {
		inputs[i] = compaction.Input{Name: b.Name, Bytes: b.Bytes}
	}
	res, err := compaction.Compact(compaction.Spec{
		Inputs:              inputs,
		Stack:               spec.Stack,
		Scope:               spec.Scope,
		FullMajorCompaction: spec.FullMajorCompaction,
		Codec:               spec.Codec,
	})
	if err != nil {
		return fmt.Errorf("shoal compact: %w", err)
	}
	be, dst, closer, err := backendFor(ctx, dstPath)
	if err != nil {
		return err
	}
	if closer != nil {
		defer closer()
	}
	return storage.WriteAll(ctx, be, dst, res.Output)
}

func emitReport(ctx context.Context, report *shadow.Report, dstPath string, inputs []string, javaOutput string) error {
	type wireReport struct {
		Inputs     []string       `json:"inputs"`
		JavaOutput string         `json:"java_output,omitempty"`
		Report     *shadow.Report `json:"report"`
		Time       string         `json:"time"`
	}
	wr := wireReport{
		Inputs:     inputs,
		JavaOutput: javaOutput,
		Report:     report,
		Time:       time.Now().UTC().Format(time.RFC3339),
	}
	b, err := json.MarshalIndent(wr, "", "  ")
	if err != nil {
		return err
	}
	// Always print to stdout for the operator.
	fmt.Println(string(b))

	if dstPath == "" {
		return nil
	}
	be, dst, closer, err := backendFor(ctx, dstPath)
	if err != nil {
		return err
	}
	if closer != nil {
		defer closer()
	}
	return storage.WriteAll(ctx, be, dst, b)
}

// runServiceMain is the --service-mode entry point. Wraps runService
// with flag validation + signal-driven context cancellation.
func runServiceMain(
	_ bool,
	zkServers, instanceName, accVersion, user, password string,
	zkTimeout time.Duration,
	tables string,
	pollInterval time.Duration,
	reportPrefix, httpListen string,
	oracleConcurrency int,
	itercfgTTL time.Duration,
	logLevel string,
	lookupSamples int,
	lookupSeed int64,
	maxInputBytes int64,
) {
	level := parseLogLevelService(logLevel)
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))
	slog.SetDefault(logger)

	if zkServers == "" {
		dieFatal("shoal-compactor-shadow --service: --zk is required")
	}
	if tables == "" {
		dieFatal("shoal-compactor-shadow --service: --tables is required")
	}
	if password == "" {
		password = os.Getenv("SHOAL_PASSWORD")
	}
	if password == "" {
		dieFatal("shoal-compactor-shadow --service: password required (--password or SHOAL_PASSWORD env)")
	}

	opts := serviceOptions{
		zkServers:         splitCSV(zkServers),
		instanceName:      instanceName,
		accumuloVersion:   accVersion,
		user:              user,
		password:          password,
		zkTimeout:         zkTimeout,
		tables:            splitCSV(tables),
		pollInterval:      pollInterval,
		reportPrefix:      reportPrefix,
		httpListen:        httpListen,
		oracleConcurrency: oracleConcurrency,
		lookupSamples:     lookupSamples,
		lookupSeed:        lookupSeed,
		itercfgTTL:        itercfgTTL,
		maxInputBytes:     maxInputBytes,
	}

	ctx, cancel := installSignalContext(context.Background())
	defer cancel()

	if err := runService(ctx, opts, logger); err != nil && !errors.Is(err, context.Canceled) {
		logger.Error("service exited with error", slog.Any("err", err))
		os.Exit(1)
	}
}

func parseLogLevelService(s string) slog.Level {
	switch strings.ToLower(s) {
	case "debug":
		return slog.LevelDebug
	case "warn", "warning":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}

func dieFatal(msg string) {
	fmt.Fprintln(os.Stderr, msg)
	os.Exit(2)
}
