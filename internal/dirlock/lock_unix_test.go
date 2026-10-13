//go:build !windows

package dirlock

import (
	"errors"
	"testing"

	"golang.org/x/sys/unix"
)

// TestLockErrorIsLockedOnlyOnContention: a held lock (EWOULDBLOCK, EAGAIN)
// is ErrLocked; any other flock failure is reported as itself, never as a
// holder to stop. Real contention is pinned end to end by
// TestAcquireRejectsCrossProcessDuplicate.
func TestLockErrorIsLockedOnlyOnContention(t *testing.T) {
	for _, contention := range []error{unix.EWOULDBLOCK, unix.EAGAIN} {
		if err := lockError(contention); !errors.Is(err, ErrLocked) ||
			!errors.Is(err, contention) {
			t.Fatalf("lockError(%v) = %v, want ErrLocked", contention, err)
		}
	}
	for _, other := range []error{unix.ENOLCK, unix.EINVAL, unix.EACCES, unix.EBADF} {
		err := lockError(other)
		if errors.Is(err, ErrLocked) || !errors.Is(err, other) {
			t.Fatalf("lockError(%v) = %v, want the error itself", other, err)
		}
	}
}
