//go:build windows

package dirlock

import (
	"errors"
	"os"
	"strings"

	"golang.org/x/sys/windows"
)

// isContention reports whether a LOCKFILE_FAIL_IMMEDIATELY LockFileEx failed
// because another handle holds the range.
func isContention(err error) bool {
	return errors.Is(err, windows.ERROR_LOCK_VIOLATION)
}

func pathIdentity(path string) string {
	return strings.ToLower(path)
}

func tryLockFile(file *os.File) error {
	var overlapped windows.Overlapped
	return windows.LockFileEx(
		windows.Handle(file.Fd()),
		windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY,
		0,
		1,
		0,
		&overlapped,
	)
}

func unlockFile(file *os.File) error {
	var overlapped windows.Overlapped
	return windows.UnlockFileEx(
		windows.Handle(file.Fd()), 0, 1, 0, &overlapped,
	)
}
