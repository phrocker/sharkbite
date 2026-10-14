package promotion

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
)

var intentIDPattern = regexp.MustCompile(`^[A-Za-z0-9_.-]+$`)

// LocalIntentStore is an append-only, fsync-backed intent journal. OS file
// locking serializes compare-and-swap across processes; an incomplete final
// line from a crash is ignored, leaving the prior durable revision intact.
type LocalIntentStore struct {
	dir string
}

func NewLocalIntentStore(dir string) (*LocalIntentStore, error) {
	if dir == "" {
		return nil, errors.New("promotion: empty intent-store directory")
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("promotion: create intent-store directory: %w", err)
	}
	return &LocalIntentStore{dir: dir}, nil
}

func (s *LocalIntentStore) path(id string) (string, error) {
	if !intentIDPattern.MatchString(id) {
		return "", fmt.Errorf("promotion: invalid intent identity %q", id)
	}
	return filepath.Join(s.dir, id+".jsonl"), nil
}

func (s *LocalIntentStore) Acquire(ctx context.Context, id string) (func() error, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	p, err := s.path(id)
	if err != nil {
		return nil, err
	}
	f, err := os.OpenFile(p+".run.lock", os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := lockIntentFile(f); err != nil {
		_ = f.Close()
		return nil, err
	}
	return func() error {
		return errors.Join(unlockIntentFile(f), f.Close())
	}, nil
}

func (s *LocalIntentStore) Load(ctx context.Context, id string) (*Intent, error) {
	p, err := s.path(id)
	if err != nil {
		return nil, err
	}
	f, err := os.OpenFile(p, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	if err := lockIntentFile(f); err != nil {
		return nil, err
	}
	defer unlockIntentFile(f)
	return readLatestIntent(ctx, f)
}

func (s *LocalIntentStore) Create(ctx context.Context, intent *Intent) error {
	return s.update(ctx, intent.ID, func(current *Intent) error {
		if current != nil {
			return ErrIntentConflict
		}
		return nil
	}, intent)
}

func (s *LocalIntentStore) CompareAndSwap(ctx context.Context, id string, revision uint64, next *Intent) error {
	return s.update(ctx, id, func(current *Intent) error {
		if current == nil {
			return ErrIntentNotFound
		}
		if current.Revision != revision {
			return ErrIntentConflict
		}
		return nil
	}, next)
}

func (s *LocalIntentStore) update(ctx context.Context, id string, check func(*Intent) error, next *Intent) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	p, err := s.path(id)
	if err != nil {
		return err
	}
	f, err := os.OpenFile(p, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	if err := lockIntentFile(f); err != nil {
		return err
	}
	defer unlockIntentFile(f)
	current, err := readLatestIntent(ctx, f)
	if errors.Is(err, ErrIntentNotFound) {
		current, err = nil, nil
	}
	if err != nil {
		return err
	}
	if err := check(current); err != nil {
		return err
	}
	raw, err := json.Marshal(next)
	if err != nil {
		return err
	}
	if _, err := f.Seek(0, 2); err != nil {
		return err
	}
	if _, err := f.Write(append(raw, '\n')); err != nil {
		return err
	}
	if err := f.Sync(); err != nil {
		return err
	}
	return nil
}

func readLatestIntent(ctx context.Context, f *os.File) (*Intent, error) {
	if _, err := f.Seek(0, 0); err != nil {
		return nil, err
	}
	raw, err := io.ReadAll(f)
	if err != nil {
		return nil, err
	}
	lines := bytes.Split(raw, []byte{'\n'})
	var latest *Intent
	for index, line := range lines {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if len(line) == 0 {
			continue
		}
		var candidate Intent
		if err := json.Unmarshal(line, &candidate); err != nil {
			if index == len(lines)-1 && len(raw) > 0 && raw[len(raw)-1] != '\n' {
				break
			}
			return nil, fmt.Errorf("promotion: corrupt intent journal revision at line %d: %w", index+1, err)
		}
		if latest == nil || candidate.Revision > latest.Revision {
			latest = &candidate
		}
	}
	if latest == nil {
		return nil, ErrIntentNotFound
	}
	return cloneIntent(latest), nil
}
