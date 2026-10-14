package walauthority

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"sync"
)

// LocalStore is a durable filesystem Store. It uses exclusive WAL creation,
// append+fsync for every record, and directory fsync for create/remove.
type LocalStore struct {
	mu sync.Mutex
}

func NewLocalStore() *LocalStore { return &LocalStore{} }

func (s *LocalStore) Create(ctx context.Context, path string, initial []byte) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o640)
	if err != nil {
		return err
	}
	if n, writeErr := f.Write(initial); writeErr != nil {
		err = writeErr
	} else if n != len(initial) {
		err = io.ErrShortWrite
	}
	if err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err == nil {
		err = closeErr
	}
	if err != nil {
		_ = os.Remove(path)
		return err
	}
	return syncDir(filepath.Dir(path))
}

func (s *LocalStore) Append(ctx context.Context, path string, data []byte) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		return err
	}
	if n, writeErr := f.Write(data); writeErr != nil {
		err = writeErr
	} else if n != len(data) {
		err = io.ErrShortWrite
	}
	if err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err == nil {
		err = closeErr
	}
	if err != nil {
		return fmt.Errorf("append %s: %w", path, err)
	}
	if ctx.Err() != nil {
		return errors.Join(ErrAmbiguous, ctx.Err())
	}
	return nil
}

func (s *LocalStore) Read(ctx context.Context, path string) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return os.ReadFile(path)
}

func (s *LocalStore) Truncate(ctx context.Context, path string, size int64) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := os.Truncate(path, size); err != nil {
		return err
	}
	return s.syncPath(path)
}

func (s *LocalStore) Sync(ctx context.Context, path string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.syncPath(path)
}

func (s *LocalStore) syncPath(path string) error {
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		return err
	}
	err = f.Sync()
	closeErr := f.Close()
	if err != nil {
		return err
	}
	return closeErr
}

func (s *LocalStore) Remove(ctx context.Context, path string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return syncDir(filepath.Dir(path))
}

func syncDir(path string) error {
	f, err := os.Open(path)
	if err != nil {
		if runtime.GOOS == "windows" {
			return nil
		}
		return err
	}
	err = f.Sync()
	closeErr := f.Close()
	if runtime.GOOS == "windows" {
		return nil
	}
	if err != nil {
		return err
	}
	return closeErr
}
