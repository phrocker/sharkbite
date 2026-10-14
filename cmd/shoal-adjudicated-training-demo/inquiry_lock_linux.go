//go:build linux

// Licensed under the Apache License, Version 2.0. See LICENSE and NOTICE.
package main

import (
	"errors"
	"golang.org/x/sys/unix"
	"os"
)

// Never unlink the lock: all contenders must contend on the same inode. Hold it
// through engine Close; independent embedded engine instances cannot share CAS.
func acquireInquiryLock(path string) (*os.File, error) {
	f, e := os.OpenFile(path, os.O_CREATE|os.O_RDWR|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0600)
	if e != nil {
		return nil, e
	}
	st, e := f.Stat()
	if e != nil || !st.Mode().IsRegular() {
		f.Close()
		return nil, errors.New("invalid inquiry lock file")
	}
	if e = unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB); e != nil {
		f.Close()
		return nil, errors.New("another inquiry owns this state")
	}
	return f, nil
}
