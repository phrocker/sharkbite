//go:build !windows

package dirlock

import (
	"errors"
	"os"

	"golang.org/x/sys/unix"
)

func pathIdentity(path string) string {
	return path
}

func tryLockFile(file *os.File) error {
	return unix.Flock(int(file.Fd()), unix.LOCK_EX|unix.LOCK_NB)
}

// isContention reports whether a LOCK_NB flock failed because another
// descriptor holds the lock.
func isContention(err error) bool {
	return errors.Is(err, unix.EWOULDBLOCK) || errors.Is(err, unix.EAGAIN)
}

func unlockFile(file *os.File) error {
	return unix.Flock(int(file.Fd()), unix.LOCK_UN)
}
