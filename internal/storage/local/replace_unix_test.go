//go:build aix || android || darwin || dragonfly || freebsd || illumos || ios || linux || netbsd || openbsd || solaris

package local

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/phrocker/shoal-oss/internal/storage"
)

func TestPlatformAtomicReplaceUsesInjectedHardLinkFastPath(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "target")
	temp := filepath.Join(dir, "temp")
	backup := filepath.Join(dir, "backup")
	if err := os.WriteFile(target, []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(temp, []byte("new"), 0o600); err != nil {
		t.Fatal(err)
	}

	var calls []string
	withReplacementSyscalls(
		t,
		func(oldpath, newpath string) error {
			calls = append(calls, "link")
			return os.Link(oldpath, newpath)
		},
		func(oldpath, newpath string) error {
			calls = append(calls, "rename")
			return os.Rename(oldpath, newpath)
		},
	)

	if err := platformAtomicReplace(temp, target, backup, true); err != nil {
		t.Fatalf("platformAtomicReplace: %v", err)
	}
	if got, err := os.ReadFile(target); err != nil || string(got) != "new" {
		t.Fatalf("target = %q, %v; want new", got, err)
	}
	if got, err := os.ReadFile(backup); err != nil || string(got) != "old" {
		t.Fatalf("backup = %q, %v; want old", got, err)
	}
	if got, want := calls, []string{"link", "rename"}; !equalStringSlices(got, want) {
		t.Fatalf("calls = %v, want %v", got, want)
	}
}

func TestPlatformAtomicReplaceFallsBackOnUnsupportedLinkErrors(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
	}{
		{name: "exdev", err: syscall.EXDEV},
		{name: "enotsup", err: syscall.ENOTSUP},
		{name: "eopnotsupp", err: syscall.EOPNOTSUPP},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			target := filepath.Join(dir, "target")
			temp := filepath.Join(dir, "temp")
			backup := filepath.Join(dir, "backup")
			if err := os.WriteFile(target, []byte("old"), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(temp, []byte("new"), 0o600); err != nil {
				t.Fatal(err)
			}

			var calls []string
			withReplacementSyscalls(
				t,
				func(oldpath, newpath string) error {
					calls = append(calls, "link")
					return &os.LinkError{Op: "link", Old: oldpath, New: newpath, Err: tc.err}
				},
				func(oldpath, newpath string) error {
					calls = append(calls, oldpath+"->"+newpath)
					return os.Rename(oldpath, newpath)
				},
			)

			if err := platformAtomicReplace(temp, target, backup, true); err != nil {
				t.Fatalf("platformAtomicReplace: %v", err)
			}
			if got, err := os.ReadFile(target); err != nil || string(got) != "new" {
				t.Fatalf("target = %q, %v; want new", got, err)
			}
			if got, err := os.ReadFile(backup); err != nil || string(got) != "old" {
				t.Fatalf("backup = %q, %v; want old", got, err)
			}
			if _, err := os.Stat(temp); !errors.Is(err, fs.ErrNotExist) {
				t.Fatalf("temp exists after fallback publish: %v", err)
			}
			if got, want := len(calls), 3; got != want {
				t.Fatalf("call count = %d, want %d", got, want)
			}
		})
	}
}

func TestPlatformAtomicReplaceFallbackRestoresTargetOnPublishFailure(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "target")
	temp := filepath.Join(dir, "temp")
	backup := filepath.Join(dir, "backup")
	if err := os.WriteFile(target, []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(temp, []byte("new"), 0o600); err != nil {
		t.Fatal(err)
	}

	publishErr := errors.New("publish failed")
	call := 0
	withReplacementSyscalls(
		t,
		func(oldpath, newpath string) error {
			return &os.LinkError{Op: "link", Old: oldpath, New: newpath, Err: syscall.ENOTSUP}
		},
		func(oldpath, newpath string) error {
			call++
			switch call {
			case 1:
				return os.Rename(oldpath, newpath)
			case 2:
				return publishErr
			case 3:
				return os.Rename(oldpath, newpath)
			default:
				t.Fatalf("unexpected rename call %d: %s -> %s", call, oldpath, newpath)
				return nil
			}
		},
	)

	err := platformAtomicReplace(temp, target, backup, true)
	if !errors.Is(err, publishErr) {
		t.Fatalf("platformAtomicReplace error = %v, want %v", err, publishErr)
	}
	if got, err := os.ReadFile(target); err != nil || string(got) != "old" {
		t.Fatalf("target = %q, %v; want old restored", got, err)
	}
	if got, err := os.ReadFile(temp); err != nil || string(got) != "new" {
		t.Fatalf("temp = %q, %v; want unpublished new bytes retained", got, err)
	}
	if _, err := os.Stat(backup); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("backup remains after restore: %v", err)
	}
}

func TestPlatformAtomicReplacePropagatesUnexpectedLinkErrorWithoutMutation(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "target")
	temp := filepath.Join(dir, "temp")
	backup := filepath.Join(dir, "backup")
	if err := os.WriteFile(target, []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(temp, []byte("new"), 0o600); err != nil {
		t.Fatal(err)
	}

	linkErr := errors.New("link permission denied")
	renameCalls := 0
	withReplacementSyscalls(
		t,
		func(oldpath, newpath string) error {
			return &os.LinkError{Op: "link", Old: oldpath, New: newpath, Err: linkErr}
		},
		func(oldpath, newpath string) error {
			renameCalls++
			return os.Rename(oldpath, newpath)
		},
	)

	err := platformAtomicReplace(temp, target, backup, true)
	if !errors.Is(err, linkErr) {
		t.Fatalf("platformAtomicReplace error = %v, want %v", err, linkErr)
	}
	if renameCalls != 0 {
		t.Fatalf("renameCalls = %d, want 0", renameCalls)
	}
	if got, err := os.ReadFile(target); err != nil || string(got) != "old" {
		t.Fatalf("target = %q, %v; want old", got, err)
	}
	if got, err := os.ReadFile(temp); err != nil || string(got) != "new" {
		t.Fatalf("temp = %q, %v; want new", got, err)
	}
	if _, err := os.Stat(backup); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("backup exists after unexpected link error: %v", err)
	}
}

func TestLocal_BackupRemovalFailureReturnsCommittedWriteErrorAfterRenameFallback(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "target")
	if err := os.WriteFile(path, []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}

	withReplacementSyscalls(
		t,
		func(oldpath, newpath string) error {
			return &os.LinkError{Op: "link", Old: oldpath, New: newpath, Err: syscall.EXDEV}
		},
		os.Rename,
	)

	w, err := New().Create(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	localWriter := w.(*writer)
	removeErr := errors.New("injected backup removal failure")
	ops := &committedCleanupFailureOps{
		replacementOps: localWriter.ops,
		err:            removeErr,
	}
	localWriter.ops = ops
	if _, err := w.Write([]byte("new")); err != nil {
		t.Fatal(err)
	}
	err = w.Close()
	if !errors.Is(err, removeErr) {
		t.Fatalf("Close error = %v, want %v", err, removeErr)
	}
	if !storage.IsCommittedWriteError(err) {
		t.Fatalf("Close error = %v, want committed-write marker", err)
	}
	if got, err := os.ReadFile(path); err != nil || string(got) != "new" {
		t.Fatalf("target = %q, %v; want committed replacement", got, err)
	}
	if got, err := os.ReadFile(ops.backupPath); err != nil || string(got) != "old" {
		t.Fatalf("backup = %q, %v; want preserved old data", got, err)
	}
}

func TestLocal_BackupNameCollisionRetriesAfterRenameFallback(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "target")
	if err := os.WriteFile(target, []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	collision := filepath.Join(dir, replacementBackupPrefix+"collision")
	if err := os.WriteFile(collision, []byte("sentinel"), 0o600); err != nil {
		t.Fatal(err)
	}
	setReplacementNameTokens(t, "temporary", "collision", "allocated")
	withReplacementSyscalls(
		t,
		func(oldpath, newpath string) error {
			return &os.LinkError{Op: "link", Old: oldpath, New: newpath, Err: syscall.ENOTSUP}
		},
		os.Rename,
	)

	w, err := New().Create(context.Background(), target)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write([]byte("new")); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if got, err := os.ReadFile(target); err != nil || string(got) != "new" {
		t.Fatalf("target = %q, %v; want new", got, err)
	}
	if got, err := os.ReadFile(collision); err != nil || string(got) != "sentinel" {
		t.Fatalf("colliding backup = %q, %v; want preserved sentinel", got, err)
	}
	if matches, err := filepath.Glob(filepath.Join(dir, replacementBackupPrefix+"*")); err != nil {
		t.Fatal(err)
	} else if len(matches) != 1 || matches[0] != collision {
		t.Fatalf("replacement backups = %v, want only preserved collision %s", matches, collision)
	}
}

func withReplacementSyscalls(
	t *testing.T,
	linkFn func(string, string) error,
	renameFn func(string, string) error,
) {
	t.Helper()
	originalLink := linkReplacementPath
	originalRename := renameReplacementPath
	linkReplacementPath = originalLink
	renameReplacementPath = originalRename
	if linkFn != nil {
		linkReplacementPath = linkFn
	}
	if renameFn != nil {
		renameReplacementPath = renameFn
	}
	t.Cleanup(func() {
		linkReplacementPath = originalLink
		renameReplacementPath = originalRename
	})
}
