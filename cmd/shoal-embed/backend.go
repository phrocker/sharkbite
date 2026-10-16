package main

import (
	"context"
	"flag"
	"fmt"
	"path/filepath"
	"strings"
	"sync"

	"github.com/phrocker/shoal-oss/internal/engine"
	"github.com/phrocker/shoal-oss/internal/storage"
	"github.com/phrocker/shoal-oss/internal/storage/local"
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

func (cfg backendConfig) open(ctx context.Context, dataDir string) (storage.Backend, func(), error) {
	name := cfg.Name
	if name == "" {
		name = "local"
	}
	if name != "local" && name != "memory" && cfg.Root == "" {
		return nil, nil, fmt.Errorf("-backend-root or SHOAL_BACKEND_ROOT is required for -backend %s", name)
	}
	be, cleanup, err := openStorageBackend(ctx, name, cfg.Root)
	if err != nil {
		return nil, nil, err
	}
	if name == "local" {
		return be, cleanup, nil
	}
	return &engineBackend{backend: be, local: local.New(), dataDir: dataDir, root: cfg.Root}, cleanup, nil
}

func (cfg backendConfig) openEngine(ctx context.Context, dataDir string, opts engine.Options) (*engine.Engine, func() error, error) {
	be, cleanup, err := cfg.open(ctx, dataDir)
	if err != nil {
		return nil, nil, err
	}
	opts.Backend = be
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

// engineBackend keeps tablet metadata on disk and maps only immutable objects
// from engine-local paths to the selected root. External import paths pass through.
type engineBackend struct {
	backend storage.Backend
	local   *local.Backend
	dataDir string
	root    string
}

func (b *engineBackend) relative(path string) (string, bool) {
	if storage.ExplicitPathScheme(b.backend, path) != "" {
		return "", false
	}
	rel, err := filepath.Rel(b.dataDir, path)
	return rel, err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

func (b *engineBackend) objectPath(path string) string {
	rel, inside := b.relative(path)
	if !inside || b.root == "" {
		return path
	}
	return strings.TrimRight(b.root, "/") + "/" + filepath.ToSlash(rel)
}

func (b *engineBackend) target(path string) (storage.Backend, string) {
	_, inside := b.relative(path)
	if inside && storage.ImmutableFormatForPath(path) == "" {
		return b.local, path
	}
	return b.backend, b.objectPath(path)
}

func (b *engineBackend) Open(ctx context.Context, path string) (storage.File, error) {
	be, key := b.target(path)
	return be.Open(ctx, key)
}

func (b *engineBackend) Create(ctx context.Context, path string) (storage.Writer, error) {
	be, key := b.target(path)
	writable, ok := be.(storage.WritableBackend)
	if !ok {
		return nil, storage.ErrReadOnly
	}
	return writable.Create(ctx, key)
}

func (b *engineBackend) Remove(ctx context.Context, path string) error {
	be, key := b.target(path)
	remover, ok := be.(storage.Remover)
	if !ok {
		return storage.ErrRemoverUnsupported
	}
	return remover.Remove(ctx, key)
}

func (b *engineBackend) List(ctx context.Context, dir string) ([]string, error) {
	lister, ok := b.backend.(storage.Lister)
	if !ok {
		return nil, fmt.Errorf("backend does not support listing")
	}
	keys, err := lister.List(ctx, b.objectPath(dir))
	if err != nil {
		return nil, err
	}
	if _, inside := b.relative(dir); !inside {
		return keys, nil
	}
	localKeys, err := b.local.List(ctx, dir)
	if err != nil {
		return nil, err
	}
	var paths []string
	for _, key := range localKeys {
		if storage.ImmutableFormatForPath(key) == "" {
			paths = append(paths, key)
		}
	}
	for _, key := range keys {
		if storage.ImmutableFormatForPath(key) != "" {
			paths = append(paths, filepath.Join(dir, filepath.Base(key)))
		}
	}
	return paths, nil
}
