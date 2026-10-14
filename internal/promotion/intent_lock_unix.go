//go:build !windows

package promotion

import (
	"os"

	"golang.org/x/sys/unix"
)

func lockIntentFile(f *os.File) error {
	return unix.Flock(int(f.Fd()), unix.LOCK_EX)
}

func unlockIntentFile(f *os.File) error {
	return unix.Flock(int(f.Fd()), unix.LOCK_UN)
}
