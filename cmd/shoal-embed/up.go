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
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"google.golang.org/grpc"

	"github.com/phrocker/shoal-oss/internal/embedpb"
	"github.com/phrocker/shoal-oss/internal/engine"
	"github.com/phrocker/shoal-oss/internal/obs"
)

// cmdUp is the one-command local profile: it opens the engine, auto-provisions
// a default table, and serves both the gRPC data plane and the observability
// HTTP surface until interrupted. It is the "veculo up" entry point — a single
// invocation yields a ready-to-use local memory engine with operability.
func cmdUp(args []string) {
	fs := flag.NewFlagSet("up", flag.ExitOnError)
	dataDir := fs.String("data", defaultDataDir(), "data directory")
	port := fs.Int("port", 9876, "gRPC listen port (ignored when --address is non-empty)")
	address := fs.String("address", "", "override gRPC bind host:port verbatim (e.g. 0.0.0.0:9876); when set, --port is ignored")
	metricsAddr := fs.String("metrics-address", "127.0.0.1:9877", "observability HTTP bind host:port")
	table := fs.String("table", "graph", "default table to auto-provision")
	splits := fs.String("splits", "evt:,ent:,idx:", "comma-separated split points for the default table")
	noDefaultTable := fs.Bool("no-default-table", false, "skip auto-provisioning the default table")
	syncDstRoot := fs.String("sync-dst-root", "", "enable event-driven RFile shipping to this destination root (empty disables sync)")
	syncBackend := fs.String("sync-dst-backend", "local", "sync destination backend: local | memory | gcs | s3 | azure | hdfs")
	syncInterval := fs.Duration("sync-interval", 60*time.Second, "safety-net poll between event-driven syncs")
	syncDebounce := fs.Duration("sync-debounce", 750*time.Millisecond, "coalesce a burst of flush/compact events before shipping")
	syncState := fs.String("sync-state", "", "sync watermark state file (default: <data>/.sync/<table>.json)")
	syncTenant := fs.String("sync-tenant", "", "stamp this tenant label into every shipped cell's column visibility")
	syncProducer := fs.String("sync-producer", "", "fan-in producer id prefixing shipped RFile names; must match [A-Za-z0-9_.-]+")
	syncCFSchema := fs.String("sync-cf-schema", "", "free-form column-family schema stamp on shipped manifests")
	syncTickTimeout := fs.Duration("sync-tick-timeout", 10*time.Minute, "per-tick timeout for event-driven sync")
	fs.Parse(args)

	logger := slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))

	eng, err := engine.Open(*dataDir, engine.Options{Logger: logger})
	if err != nil {
		die("up: %v", err)
	}

	// Ensure the default table exists (idempotent: ignore AlreadyExists).
	if !*noDefaultTable {
		opts := engine.TableOptions{}
		if *splits != "" {
			opts.Splits = engine.PrefixSplit(strings.Split(*splits, ",")...)
		}
		if err := eng.CreateTable(*table, opts); err != nil && !strings.Contains(err.Error(), "already exists") {
			eng.Close()
			die("up: provision table %q: %v", *table, err)
		}
	}

	grpcAddr := "127.0.0.1:" + strconv.Itoa(*port)
	if *address != "" {
		grpcAddr = *address
	}
	lis, err := net.Listen("tcp", grpcAddr)
	if err != nil {
		eng.Close()
		die("up: grpc listen: %v", err)
	}

	srv := grpc.NewServer()
	embedpb.RegisterShoalEmbedServer(srv, newEmbedServer(eng))

	// Observability HTTP server.
	obsSrv := obs.NewServer(eng)
	httpSrv := &http.Server{Addr: *metricsAddr, Handler: obsSrv.Handler()}
	obsLis, err := net.Listen("tcp", *metricsAddr)
	if err != nil {
		eng.Close()
		die("up: metrics listen: %v", err)
	}

	// Engine is open and tables are loaded — declare ready.
	obsSrv.SetReady(true)

	go func() {
		if err := httpSrv.Serve(obsLis); err != nil && err != http.ErrServerClosed {
			logger.Error("observability server stopped", slog.String("err", err.Error()))
		}
	}()

	logger.Info("shoal-embed up", slog.String("grpc", grpcAddr), slog.String("metrics", *metricsAddr), slog.String("data", *dataDir))
	fmt.Fprintf(os.Stderr, "veculo up — local memory engine ready\n")
	fmt.Fprintf(os.Stderr, "  gRPC      grpc://%s\n", grpcAddr)
	fmt.Fprintf(os.Stderr, "  metrics   http://%s/metrics\n", *metricsAddr)
	fmt.Fprintf(os.Stderr, "  health    http://%s/healthz  http://%s/readyz\n", *metricsAddr, *metricsAddr)
	fmt.Fprintf(os.Stderr, "  stats     http://%s/stats\n", *metricsAddr)

	// Root context cancelled on shutdown; drives the optional event-sync loop.
	syncCtx, syncCancel := context.WithCancel(context.Background())

	// Optional in-process event-driven RFile syncer: ships the default table's
	// RFiles as they land (flush/compaction) instead of via a separate daemon.
	if *syncDstRoot != "" {
		if *noDefaultTable {
			eng.Close()
			die("up: --sync-dst-root requires a default table (remove --no-default-table)")
		}
		state := *syncState
		if state == "" {
			state = filepath.Join(*dataDir, ".sync", *table+".json")
		}
		cfg := eventSyncConfig{
			Table:       *table,
			BackendName: *syncBackend,
			StatePath:   state,
			Interval:    *syncInterval,
			Debounce:    *syncDebounce,
			TickTimeout: *syncTickTimeout,
			Opts: engine.RFileExportOptions{
				DestinationRoot:      *syncDstRoot,
				CFSchema:             *syncCFSchema,
				StampVisibilityLabel: *syncTenant,
				ProducerID:           *syncProducer,
				EngineVersion:        version,
			},
		}
		go func() {
			if err := runEventSync(syncCtx, eng, logger, cfg); err != nil {
				logger.Error("event-sync exited", slog.String("err", err.Error()))
			}
		}()
		fmt.Fprintf(os.Stderr, "  sync      %s (backend %s, every %s)\n", *syncDstRoot, *syncBackend, syncInterval.String())
	}

	go func() {
		sigCh := make(chan os.Signal, 1)
		signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
		<-sigCh
		logger.Info("shutting down")
		obsSrv.SetReady(false)
		syncCancel()
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = httpSrv.Shutdown(ctx)
		srv.GracefulStop()
		eng.Close()
	}()

	if err := srv.Serve(lis); err != nil {
		die("up: %v", err)
	}
}
