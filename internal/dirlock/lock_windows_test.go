//go:build windows

package dirlock

import (
	"errors"
	"testing"

	"golang.org/x/sys/windows"
)

// TestLockErrorIsLockedOnlyOnContention: a held range
// (ERROR_LOCK_VIOLATION) is ErrLocked; any other LockFileEx failure is
// reported as itself, never as a holder to stop.
func TestLockErrorIsLockedOnlyOnContention(t *testing.T) {
	if err := lockError(windows.ERROR_LOCK_VIOLATION); !errors.Is(err, ErrLocked) {
		t.Fatalf("lock violation = %v, want ErrLocked", err)
	}
	for _, other := range []error{
		windows.ERROR_ACCESS_DENIED, windows.ERROR_INVALID_HANDLE,
		windows.ERROR_NOT_SUPPORTED,
	} {
		err := lockError(other)
		if errors.Is(err, ErrLocked) || !errors.Is(err, other) {
			t.Fatalf("lockError(%v) = %v, want the error itself", other, err)
		}
	}
}
