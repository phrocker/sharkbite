package compactexec

import (
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/phrocker/shoal-oss/internal/storage"
)

// BackendStore adapts a writable/removable storage backend to Store.
type BackendStore struct {
	Backend storage.Backend
}

func (s BackendStore) Read(ctx context.Context, path string) ([]byte, error) {
	if s.Backend == nil {
		return nil, fmt.Errorf("compactexec: backend is required")
	}
	return storage.ReadAll(ctx, s.Backend, path)
}

func (s BackendStore) Write(ctx context.Context, path string, data []byte) (err error) {
	if s.Backend == nil {
		return fmt.Errorf("compactexec: backend is required")
	}
	wb, ok := s.Backend.(storage.WritableBackend)
	if !ok {
		return storage.ErrReadOnly
	}
	w, err := wb.Create(ctx, path)
	if err != nil {
		return err
	}
	var state storage.WriteCleanupState
	defer storage.AbortOnError(&err, w, &state)
	for off := 0; off < len(data); {
		if err := ctx.Err(); err != nil {
			return err
		}
		n, writeErr := w.Write(data[off:])
		if n < 0 || n > len(data)-off {
			return fmt.Errorf("compactexec: writer returned invalid count %d", n)
		}
		off += n
		if writeErr != nil {
			return writeErr
		}
		if n == 0 && off < len(data) {
			return io.ErrNoProgress
		}
	}
	if syncer, ok := w.(interface{ Sync() error }); ok {
		if err := syncer.Sync(); err != nil {
			return err
		}
	}
	state.MarkCloseAttempted()
	if err := w.Close(); err != nil {
		return err
	}
	return nil
}

func (s BackendStore) Remove(ctx context.Context, path string) error {
	if s.Backend == nil {
		return fmt.Errorf("compactexec: backend is required")
	}
	remover, ok := s.Backend.(storage.Remover)
	if !ok {
		return fmt.Errorf("compactexec: backend cannot remove %s", path)
	}
	err := remover.Remove(ctx, path)
	if errors.Is(err, storage.ErrNotFound) {
		return nil
	}
	return err
}
