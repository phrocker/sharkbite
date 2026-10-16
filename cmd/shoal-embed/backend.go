package main

import (
	"context"
	"flag"
	"fmt"
	"strings"
	"sync"

	"github.com/phrocker/shoal-oss/internal/engine"
	"github.com/phrocker/shoal-oss/internal/storage"
)

type backendConfig struct {
	Name string
	Root string
}

func registerBackendFlags(fs *flag.FlagSet) *backendConfig {
	cfg := &backendConfig{}
	name := flagOrEnv("", false, "SHOAL_BACKEND")
	if name == "" {
		name = "local"
	}
	fs.StringVar(&cfg.Name, "backend", name, "backend for immutable tablet files: local | gcs | s3 | azure | hdfs (defaults to SHOAL_BACKEND, then local)")
	fs.StringVar(&cfg.Root, "backend-root", flagOrEnv("", false, "SHOAL_BACKEND_ROOT"), "prefix the engine stores tablet files under, e.g. s3://bucket/prefix or a directory outside -data (required for cloud backends; defaults to SHOAL_BACKEND_ROOT)")
	return cfg
}

// engineBackendNames are the backends an engine may store tablet files on.
// memory is deliberately absent: it is per-process, so a flush would be
// lost at exit and an existing local table would read as empty.
var engineBackendNames = map[string]bool{"local": true, "gcs": true, "s3": true, "azure": true, "azblob": true, "az": true, "hdfs": true}

func (cfg backendConfig) name() string {
	name := strings.ToLower(strings.TrimSpace(cfg.Name))
	if name == "" {
		return "local"
	}
	return name
}

// open resolves the configured backend. pathHint is a backend path a
// command already has (an import manifest, for example); it stands in for
// the root when none is given, as it did before -backend-root existed.
func (cfg backendConfig) open(ctx context.Context, pathHint string) (storage.Backend, func(), error) {
	name := cfg.name()
	if name == "memory" {
		return nil, nil, fmt.Errorf("-backend memory is in-process only and would lose every flushed file at exit; use local")
	}
	if !engineBackendNames[name] {
		return nil, nil, fmt.Errorf("unknown -backend %q (local | gcs | s3 | azure | hdfs)", cfg.Name)
	}
	hint := cfg.Root
	if hint == "" {
		hint = pathHint
	}
	if name != "local" && hint == "" {
		return nil, nil, fmt.Errorf("-backend-root or SHOAL_BACKEND_ROOT is required for -backend %s", name)
	}
	return openStorageBackend(ctx, name, hint)
}

func (cfg backendConfig) openEngine(ctx context.Context, dataDir string, opts engine.Options) (*engine.Engine, func() error, error) {
	be, cleanup, err := cfg.open(ctx, "")
	if err != nil {
		return nil, nil, err
	}
	if cfg.name() != "local" && cfg.Root == "" {
		cleanup()
		return nil, nil, fmt.Errorf("-backend-root or SHOAL_BACKEND_ROOT is required for -backend %s", cfg.name())
	}
	opts.Backend = be
	opts.BackendRoot = cfg.Root
	eng, err := engine.Open(dataDir, opts)
	if err != nil {
		cleanup()
		return nil, nil, err
	}
	var once sync.Once
	var closeErr error
	return eng, func() error {
		once.Do(func() {
			defer cleanup()
			closeErr = eng.Close()
		})
		return closeErr
	}, nil
}
