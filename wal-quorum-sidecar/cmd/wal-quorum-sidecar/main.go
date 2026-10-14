// Licensed to the Apache Software Foundation (ASF) under one
// or more contributor license agreements.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"syscall"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/keepalive"
	"google.golang.org/grpc/reflection"

	"github.com/accumulo/wal-quorum-sidecar/internal/config"
	"github.com/accumulo/wal-quorum-sidecar/internal/gcs"
	"github.com/accumulo/wal-quorum-sidecar/internal/health"
	"github.com/accumulo/wal-quorum-sidecar/internal/metrics"
	"github.com/accumulo/wal-quorum-sidecar/internal/replication"
	"github.com/accumulo/wal-quorum-sidecar/internal/segment"
	"github.com/accumulo/wal-quorum-sidecar/internal/server"
)

func main() {
	var (
		udsPath        = flag.String("uds", "/var/run/wal-quorum/wal-quorum.sock", "Unix domain socket path for local gRPC server")
		peerPort       = flag.Int("peer-port", 9710, "TCP port for peer-to-peer gRPC server")
		walDir         = flag.String("wal-dir", "/mnt/data/accumulo/wal", "Directory for WAL segment storage")
		logLevel       = flag.String("log-level", "info", "Log level: debug, info, warn, error")
		gcsPrefix      = flag.String("gcs-prefix", "wal", "GCS object prefix for uploaded segments")
		gcsWorkers     = flag.Int("gcs-workers", 2, "Number of concurrent GCS upload workers")
	)
	flag.Parse()

	// Set up structured logging.
	level := parseLogLevel(*logLevel)
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: level}))
	slog.SetDefault(logger)

	slog.Info("wal-quorum-sidecar starting",
		"uds", *udsPath,
		"peer_port", *peerPort,
		"wal_dir", *walDir,
	)

	// Load configuration from environment.
	cfg, err := config.Load()
	if err != nil {
		slog.Error("failed to load configuration", "error", err)
		os.Exit(1)
	}
	slog.Info("configuration loaded",
		"pod", cfg.PodName,
		"namespace", cfg.PodNamespace,
		"replicas", cfg.TotalReplicas,
		"peers", cfg.Peers(),
		"gcs_bucket", cfg.GCSBucket,
	)

	// Initialize metrics.
	metrics.Init()

	// Ensure WAL directory exists.
	if err := os.MkdirAll(*walDir, 0o750); err != nil {
		slog.Error("failed to create WAL directory", "error", err, "dir", *walDir)
		os.Exit(1)
	}

	// Create the segment manager.
	mgr := segment.NewManager(*walDir, logger)

	// Create the health tracker.
	ht := health.NewTracker(mgr)

	// Create peer pool and quorum writer for replication.
	pool := replication.NewPeerPool(cfg, logger)
	quorum := replication.NewQuorumWriter(pool, 0, logger) // 0 = default 500ms timeout

	// Create GCS uploader if a bucket is configured.
	var uploader *gcs.Uploader
	if cfg.GCSBucket != "" {
		var err error
		uploader, err = gcs.NewUploader(context.Background(), cfg.GCSBucket, *gcsPrefix, *gcsWorkers, logger)
		if err != nil {
			slog.Error("failed to create GCS uploader", "error", err)
			os.Exit(1)
		}
		slog.Info("GCS uploader initialized",
			"bucket", cfg.GCSBucket,
			"prefix", *gcsPrefix,
			"workers", *gcsWorkers,
		)

		purger := gcs.NewPurger(uploader, mgr, logger)
		go purger.RunPurgeLoop(context.Background(), cfg.Peers())
	} else {
		slog.Info("GCS upload disabled (no GCS_BUCKET configured)")
	}

	// --- Local gRPC server (Unix domain socket) ---
	if err := os.MkdirAll(dirOf(*udsPath), 0o750); err != nil {
		slog.Error("failed to create UDS directory", "error", err)
		os.Exit(1)
	}
	// Remove stale socket file if present.
	_ = os.Remove(*udsPath)

	udsListener, err := net.Listen("unix", *udsPath)
	if err != nil {
		slog.Error("failed to listen on UDS", "error", err, "path", *udsPath)
		os.Exit(1)
	}
	// Allow the TServer (non-root) to connect.
	if err := os.Chmod(*udsPath, 0o770); err != nil {
		slog.Warn("failed to chmod UDS", "error", err)
	}

	localGRPC := grpc.NewServer(
		grpc.MaxRecvMsgSize(4 * 1024 * 1024), // 4 MB
		grpc.MaxSendMsgSize(4 * 1024 * 1024),
	)
	localSrv := server.NewLocalServer(mgr, cfg, pool, quorum, uploader, logger)
	localSrv.Register(localGRPC)
	health.RegisterServer(localGRPC, ht)
	reflection.Register(localGRPC)

	// --- Peer gRPC server (TCP) ---
	peerListener, err := net.Listen("tcp", fmt.Sprintf(":%d", *peerPort))
	if err != nil {
		slog.Error("failed to listen on peer port", "error", err, "port", *peerPort)
		os.Exit(1)
	}

	peerGRPC := grpc.NewServer(
		grpc.MaxRecvMsgSize(4*1024*1024),
		grpc.MaxSendMsgSize(4*1024*1024),
		grpc.KeepaliveEnforcementPolicy(keepalive.EnforcementPolicy{
			MinTime:             30 * time.Second,
			PermitWithoutStream: true,
		}),
	)
	peerSrv := server.NewPeerServer(mgr, logger)
	peerSrv.Register(peerGRPC)
	health.RegisterServer(peerGRPC, ht)
	reflection.Register(peerGRPC)

	// Start serving.
	errCh := make(chan error, 2)
	go func() {
		slog.Info("local gRPC server listening", "path", *udsPath)
		errCh <- localGRPC.Serve(udsListener)
	}()
	go func() {
		slog.Info("peer gRPC server listening", "port", *peerPort)
		errCh <- peerGRPC.Serve(peerListener)
	}()

	// Wait for shutdown signal.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()

	select {
	case <-ctx.Done():
		slog.Info("received shutdown signal, draining...")
	case err := <-errCh:
		slog.Error("server exited unexpectedly", "error", err)
	}

	// Graceful shutdown with a deadline.
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// Stop accepting new RPCs, let in-flight ones drain.
	localGRPC.GracefulStop()
	peerGRPC.GracefulStop()

	// Seal any open segments.
	if err := mgr.SealAll(shutdownCtx); err != nil {
		slog.Error("error sealing segments during shutdown", "error", err)
	}

	// Drain GCS upload queue and shut down uploader.
	if uploader != nil {
		slog.Info("draining GCS upload queue before shutdown...")
		if err := uploader.Close(); err != nil {
			slog.Error("error closing GCS uploader", "error", err)
		}
	}

	// Close peer connections.
	pool.Close()

	_ = udsListener.Close()
	_ = peerListener.Close()
	slog.Info("wal-quorum-sidecar stopped")
}

// parseLogLevel converts a string to an slog.Level.
func parseLogLevel(s string) slog.Level {
	switch s {
	case "debug":
		return slog.LevelDebug
	case "warn":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}

// dirOf returns the directory portion of a file path.
func dirOf(path string) string {
	for i := len(path) - 1; i >= 0; i-- {
		if path[i] == '/' {
			return path[:i]
		}
	}
	return "."
}
