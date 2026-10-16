package main

import (
	"context"
	"flag"
	"fmt"
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
	fs.StringVar(&cfg.Name, "backend", name, "destination backend containing manifest/RFiles: local | memory | gcs | s3 | azure | hdfs")
	fs.StringVar(&cfg.Root, "backend-root", flagOrEnv("", false, "SHOAL_BACKEND_ROOT"), "bucket/prefix or path root for backend objects (required for non-local, non-memory backends; defaults to SHOAL_BACKEND_ROOT)")
	return cfg
}

func (cfg backendConfig) open(ctx context.Context) (storage.Backend, func(), error) {
	name := cfg.Name
	if name == "" {
		name = "local"
	}
	if name != "local" && name != "memory" && cfg.Root == "" {
		return nil, nil, fmt.Errorf("-backend-root or SHOAL_BACKEND_ROOT is required for -backend %s", name)
	}
	return openStorageBackend(ctx, name, cfg.Root)
}

func (cfg backendConfig) openEngine(ctx context.Context, dataDir string, opts engine.Options) (*engine.Engine, func() error, error) {
	be, cleanup, err := cfg.open(ctx)
	if err != nil {
		return nil, nil, err
	}
	opts.Backend = be
	// TODO: Pass cfg.Root as engine.Options.BackendRoot once #131 merges.
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
