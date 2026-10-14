package main

import (
	"context"
	"errors"
	"flag"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/phrocker/shoal-oss/internal/engine"
)

// cmdSync continuously ships a table's newly flushed/compacted RFiles to an
// object-storage destination, maintaining a watermark so each tick only uploads
// files that aren't already there. This is the "feed a veculo cluster over
// time" path: a local agent keeps writing, and sync drips deltas to the cluster.
func cmdSync(args []string) {
	fs := flag.NewFlagSet("sync", flag.ExitOnError)
	dataDir := fs.String("data", defaultDataDir(), "source engine data directory")
	tableName := fs.String("table", "", "table name (required)")
	dstBackendName := fs.String("dst-backend", "local", "destination backend: local | memory | gcs | s3 | azure | hdfs")
	dstRoot := fs.String("dst-root", "", "destination object/engine root (required)")
	statePath := fs.String("state", "", "sync state file (default: <data>/.sync/<table>.json)")
	interval := fs.Duration("interval", 30*time.Second, "interval between sync ticks (ignored with --once)")
	once := fs.Bool("once", false, "run a single sync tick and exit")
	cfSchema := fs.String("cf-schema", "", "free-form column-family schema stamp")
	visStamp := fs.String("visibility-stamp", "", "optional column-visibility/tenant stamp (manifest metadata)")
	authzStamp := fs.String("authz-stamp", "", "optional authorizations stamp (manifest metadata)")
	tenant := fs.String("tenant", "", "stamp this tenant label into every shipped cell's column visibility (enforced at scan); must be a bare CV label [A-Za-z0-9_:./-]+")
	stampMode := fs.String("stamp-mode", "and", "tenant stamp mode: and (require label on every cell) | whenEmpty (only stamp cells with no existing visibility)")
	producerID := fs.String("producer", "", "fan-in producer id: prefixes shipped RFile names so multiple producers can share one destination without millisecond collisions; must match [A-Za-z0-9_.-]+")
	manifestPath := fs.String("manifest", "", "latest-manifest path on destination (default: <dst-root>/manifest.json)")
	tickTimeout := fs.Duration("tick-timeout", 10*time.Minute, "per-tick timeout")
	fs.Parse(args)

	if *tableName == "" || *dstRoot == "" {
		die("sync: --table and --dst-root are required")
	}
	state := *statePath
	if state == "" {
		state = filepath.Join(*dataDir, ".sync", *tableName+".json")
	}

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	eng, err := engine.Open(*dataDir, engine.Options{
		Logger: slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn})),
	})
	if err != nil {
		die("sync: %v", err)
	}
	defer eng.Close()

	prior, err := engine.LoadSyncState(state)
	if err != nil {
		die("sync: %v", err)
	}

	opts := engine.RFileExportOptions{
		DestinationRoot:      *dstRoot,
		CFSchema:             *cfSchema,
		VisibilityStamp:      *visStamp,
		AuthorizationsStamp:  *authzStamp,
		StampVisibilityLabel: *tenant,
		StampMode:            *stampMode,
		ProducerID:           *producerID,
		EngineVersion:        version,
		ManifestPath:         *manifestPath,
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	tick := func() error {
		tctx, cancel := context.WithTimeout(ctx, *tickTimeout)
		defer cancel()
		dst, cleanup, err := openStorageBackend(tctx, *dstBackendName, *dstRoot)
		if err != nil {
			return err
		}
		defer cleanup()
		res, err := eng.ExportRFilesIncremental(tctx, *tableName, dst, opts, prior)
		if err != nil {
			return err
		}
		prior = res.State
		if err := engine.SaveSyncState(state, res.State); err != nil {
			return err
		}
		logger.Info("sync tick",
			"table", *tableName,
			"sequence", res.State.Sequence,
			"uploaded", len(res.Uploaded),
			"skipped", len(res.Skipped),
			"retired", len(res.Retired),
			"rfiles", len(res.Manifest.RFiles),
			"manifest", res.ManifestPath,
		)
		return nil
	}

	if *once {
		if err := tick(); err != nil {
			die("sync: %v", err)
		}
		return
	}

	logger.Info("sync started", "table", *tableName, "dst", *dstRoot, "interval", interval.String(), "state", state)
	for {
		if err := tick(); err != nil {
			if errors.Is(err, context.Canceled) {
				logger.Info("sync stopped")
				return
			}
			logger.Error("sync tick failed", "err", err)
		}
		select {
		case <-ctx.Done():
			logger.Info("sync stopped")
			return
		case <-time.After(*interval):
		}
	}
}
