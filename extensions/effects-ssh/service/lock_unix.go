// Licensed under the Apache License, Version 2.0. See LICENSE and NOTICE.

//go:build linux || freebsd || netbsd || openbsd || dragonfly

package service

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

// ErrJournalLocked is returned when another process holds the journal.
var ErrJournalLocked = errors.New("journal directory is locked by another process")

// dirLock is an exclusive, non-blocking flock on the directory's lock file:
// the pattern of internal/dirlock, which an extension may not import. flock
// conflicts between open file descriptions, so a second open in the same
// process is refused too. It is advisory and local: a journal on a network
// filesystem shared by two nodes is not protected, which is why the
// deployment uses a ReadWriteOnce volume.
type dirLock struct{ file *os.File }

func lockDir(dir string) (*dirLock, error) {
	file, err := os.OpenFile(filepath.Join(dir, LockFileName), os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, fmt.Errorf("journal lock cannot be opened: %w", err)
	}
	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = file.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, ErrJournalLocked
		}
		return nil, fmt.Errorf("journal lock: %w", err)
	}
	return &dirLock{file: file}, nil
}

func (l *dirLock) release() error {
	if l == nil || l.file == nil {
		return nil
	}
	err := syscall.Flock(int(l.file.Fd()), syscall.LOCK_UN)
	return errors.Join(err, l.file.Close())
}
