// Licensed under the Apache License, Version 2.0. See LICENSE and NOTICE.

//go:build !(linux || freebsd || netbsd || openbsd || dragonfly)

package service

import "errors"

// ErrJournalLocked is returned when another process holds the journal.
var ErrJournalLocked = errors.New("journal directory is locked by another process")

type dirLock struct{}

// lockDir refuses, so the journal does not run here. Without an exclusive
// directory lock two processes could share a journal and each run the same
// key. darwin is deliberately on this side: its fsync does not flush the
// drive's cache (only F_FULLFSYNC does), so a "durable" started record could
// be lost on power failure, and the journal's guarantee would not hold.
func lockDir(string) (*dirLock, error) {
	return nil, errors.New("the journal lock is not implemented on this platform")
}

func (*dirLock) release() error { return nil }
