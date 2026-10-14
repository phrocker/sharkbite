package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"time"

	"github.com/phrocker/shoal-oss/internal/engine"
	"github.com/phrocker/shoal-oss/internal/storage"
	"github.com/phrocker/shoal-oss/internal/storage/azure"
	"github.com/phrocker/shoal-oss/internal/storage/gcs"
	"github.com/phrocker/shoal-oss/internal/storage/hdfs"
	"github.com/phrocker/shoal-oss/internal/storage/local"
	"github.com/phrocker/shoal-oss/internal/storage/memory"
	"github.com/phrocker/shoal-oss/internal/storage/s3"
)

func cmdExport(args []string) {
	fs := flag.NewFlagSet("export", flag.ExitOnError)
	dataDir := fs.String("data", defaultDataDir(), "source engine data directory")
	tableName := fs.String("table", "", "table name (required)")
	dstBackendName := fs.String("dst-backend", "local", "destination backend: local | memory | gcs | s3 | azure | hdfs")
	dstRoot := fs.String("dst-root", "", "destination engine/object root (required)")
	manifestPath := fs.String("manifest", "", "manifest path on destination backend (default: <dst-root>/manifest.json)")
	cfSchema := fs.String("cf-schema", "", "free-form column-family schema stamp")
	visStamp := fs.String("visibility-stamp", "", "optional column-visibility/tenant stamp (manifest metadata)")
	authzStamp := fs.String("authz-stamp", "", "optional authorizations stamp (manifest metadata)")
	tenant := fs.String("tenant", "", "stamp this tenant label into every exported cell's column visibility (enforced at scan); must be a bare CV label [A-Za-z0-9_:./-]+")
	stampMode := fs.String("stamp-mode", "and", "tenant stamp mode: and (require label on every cell) | whenEmpty (only stamp cells with no existing visibility)")
	producerID := fs.String("producer", "", "fan-in producer id: prefixes exported RFile names so multiple producers can share one destination without millisecond collisions; must match [A-Za-z0-9_.-]+")
	timeout := fs.Duration("timeout", 10*time.Minute, "overall export timeout")
	fs.Parse(args)

	if *tableName == "" || *dstRoot == "" {
		die("export: --table and --dst-root are required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()
	dst, cleanup, err := openStorageBackend(ctx, *dstBackendName, *dstRoot)
	if err != nil {
		die("export: %v", err)
	}
	defer cleanup()

	eng, err := engine.Open(*dataDir, engine.Options{
		Logger: slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn})),
	})
	if err != nil {
		die("export: %v", err)
	}
	defer eng.Close()

	manifest, err := eng.ExportRFiles(ctx, *tableName, dst, engine.RFileExportOptions{
		DestinationRoot:      *dstRoot,
		CFSchema:             *cfSchema,
		VisibilityStamp:      *visStamp,
		AuthorizationsStamp:  *authzStamp,
		StampVisibilityLabel: *tenant,
		StampMode:            *stampMode,
		ProducerID:           *producerID,
		EngineVersion:        version,
		ManifestPath:         *manifestPath,
	})
	if err != nil {
		die("export: %v", err)
	}
	if *tenant != "" {
		fmt.Fprintf(os.Stderr, "stamped tenant visibility %q (mode %s) into exported cells\n", *tenant, *stampMode)
	}
	fmt.Fprintf(os.Stderr, "exported %d RFile(s) for table %q\n", len(manifest.RFiles), manifest.SourceTable)
	_ = json.NewEncoder(os.Stdout).Encode(manifest)
}

func cmdImport(args []string) {
	fs := flag.NewFlagSet("import", flag.ExitOnError)
	dataDir := fs.String("data", defaultDataDir(), "destination engine data directory")
	backendName := fs.String("backend", "local", "destination backend containing manifest/RFiles: local | memory | gcs | s3 | azure | hdfs")
	manifestPath := fs.String("manifest", "", "manifest path on backend (required)")
	timeout := fs.Duration("timeout", 10*time.Minute, "overall import timeout")
	fs.Parse(args)

	if *manifestPath == "" {
		die("import: --manifest is required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()
	be, cleanup, err := openStorageBackend(ctx, *backendName, *manifestPath)
	if err != nil {
		die("import: %v", err)
	}
	defer cleanup()
	data, err := storage.ReadAll(ctx, be, *manifestPath)
	if err != nil {
		die("import: read manifest: %v", err)
	}
	var manifest engine.RFileExportManifest
	if err := json.Unmarshal(data, &manifest); err != nil {
		die("import: decode manifest: %v", err)
	}
	eng, err := engine.Open(*dataDir, engine.Options{Backend: be})
	if err != nil {
		die("import: %v", err)
	}
	defer eng.Close()
	if err := eng.ImportRFileManifest(ctx, &manifest); err != nil {
		die("import: %v", err)
	}
	fmt.Printf("imported table %q with %d RFile(s)\n", manifest.SourceTable, len(manifest.RFiles))
}

func openStorageBackend(ctx context.Context, name, pathHint string) (storage.Backend, func(), error) {
	switch name {
	case "local":
		return local.New(), func() {}, nil
	case "memory":
		return memory.New(), func() {}, nil
	case "gcs":
		be, err := gcs.New(ctx)
		if err != nil {
			return nil, nil, err
		}
		return be, func() { _ = be.Close() }, nil
	case "s3":
		be, err := s3.New(ctx)
		if err != nil {
			return nil, nil, err
		}
		return be, func() { _ = be.Close() }, nil
	case "azure", "azblob", "az":
		be, err := azure.New(ctx)
		if err != nil {
			return nil, nil, err
		}
		return be, func() { _ = be.Close() }, nil
	case "hdfs":
		address := os.Getenv("SHOAL_HDFS_NAMENODE")
		if address == "" && strings.HasPrefix(pathHint, "hdfs:") {
			var err error
			address, err = hdfs.AddressFromPath(pathHint)
			if err != nil {
				return nil, nil, err
			}
		}
		be, err := hdfs.NewContext(ctx, address)
		if err != nil {
			return nil, nil, err
		}
		return be, func() { _ = be.Close() }, nil
	default:
		return nil, nil, fmt.Errorf("unknown backend %q", name)
	}
}
