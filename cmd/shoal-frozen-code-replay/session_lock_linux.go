//go:build linux

// Licensed under the Apache License, Version 2.0. See LICENSE and NOTICE.
package main

import (
	"errors"
	"golang.org/x/sys/unix"
	"os"
)

func openLockFile(path string) (*os.File, error) {
	return os.OpenFile(path, os.O_CREATE|os.O_RDWR|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0600)
}
func lockSessionFile(f *os.File) error {
	if err := unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		if errors.Is(err, unix.EWOULDBLOCK) {
			return errors.New("another inquiry owns this state directory")
		}
		return err
	}
	return nil
}
