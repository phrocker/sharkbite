// Licensed under the Apache License, Version 2.0. See LICENSE and NOTICE.
package main

import (
	"errors"
	"os"
	"path/filepath"
)

// The embedded engine's CAS coordinates workers sharing an engine instance, not
// separately opened processes. Hold this advisory lock for the entire session,
// including engine Close. Never unlink the lock file: waiters must use one inode.
type sessionLock struct{ file *os.File }

func acquireSessionLock(dir string) (*sessionLock, error) {
	if err := makeStateDirectory(dir, syncDirectory); err != nil {
		return nil, err
	}
	path := filepath.Join(dir, "service-session.lock")
	f, err := openLockFile(path)
	if err != nil {
		return nil, err
	}
	ok := false
	defer func() {
		if !ok {
			f.Close()
		}
	}()
	st, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !st.Mode().IsRegular() {
		return nil, errors.New("session lock must be a regular file")
	}
	if err = lockSessionFile(f); err != nil {
		return nil, err
	}
	ok = true
	return &sessionLock{f}, nil
}
func (l *sessionLock) Close() error { return l.file.Close() }
