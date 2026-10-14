package dirlock

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestAcquireRejectsCanonicalDuplicateAndReleases(t *testing.T) {
	directory, err := os.MkdirTemp(".", ".dirlock-test-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(directory) })
	relative, err := filepath.Rel(".", directory)
	if err != nil {
		t.Fatal(err)
	}
	first, err := Acquire(relative, ".store.lock")
	if err != nil {
		t.Fatal(err)
	}
	absolute, err := filepath.Abs(directory)
	if err != nil {
		t.Fatal(err)
	}
	if second, err := Acquire(absolute, ".store.lock"); !errors.Is(err, ErrLocked) {
		if second != nil {
			_ = second.Close()
		}
		t.Fatalf("duplicate acquire = %v", err)
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	if err := first.Close(); err != nil {
		t.Fatalf("idempotent close = %v", err)
	}
	reopened, err := Acquire(absolute, ".store.lock")
	if err != nil {
		t.Fatalf("reopen after release = %v", err)
	}
	if err := reopened.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestAcquireRejectsInvalidLockName(t *testing.T) {
	if lock, err := Acquire(".", `nested\lock`); err == nil {
		_ = lock.Close()
		t.Fatal("nested lock name succeeded")
	}
}

func TestAcquireRejectsCrossProcessDuplicate(t *testing.T) {
	directory, err := os.MkdirTemp(".", ".dirlock-process-test-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(directory) })

	command := exec.Command(os.Args[0], "-test.run=^TestAcquireHelperProcess$")
	command.Env = append(
		os.Environ(),
		"SHOAL_DIRLOCK_HELPER=1",
		"SHOAL_DIRLOCK_DIRECTORY="+directory,
	)
	stdin, err := command.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := command.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	var stderr bytes.Buffer
	command.Stderr = &stderr
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	waited := false
	t.Cleanup(func() {
		if waited {
			return
		}
		_ = stdin.Close()
		_ = command.Wait()
	})
	line, err := bufio.NewReader(stdout).ReadString('\n')
	if err != nil || strings.TrimSpace(line) != "locked" {
		_ = stdin.Close()
		waitErr := command.Wait()
		waited = true
		t.Fatalf("lock helper startup = %q, %v, wait %v, stderr %q", line, err, waitErr, stderr.String())
	}
	if lock, err := Acquire(directory, ".store.lock"); !errors.Is(err, ErrLocked) {
		if lock != nil {
			_ = lock.Close()
		}
		t.Fatalf("cross-process duplicate acquire = %v", err)
	}
	if err := stdin.Close(); err != nil {
		t.Fatal(err)
	}
	if err := command.Wait(); err != nil {
		t.Fatalf("lock helper exit = %v, stderr %q", err, stderr.String())
	}
	waited = true
	reopened, err := Acquire(directory, ".store.lock")
	if err != nil {
		t.Fatalf("acquire after helper release = %v", err)
	}
	if err := reopened.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestAcquireHelperProcess(t *testing.T) {
	if os.Getenv("SHOAL_DIRLOCK_HELPER") != "1" {
		return
	}
	lock, err := Acquire(
		os.Getenv("SHOAL_DIRLOCK_DIRECTORY"),
		".store.lock",
	)
	if err != nil {
		_, _ = fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	_, _ = fmt.Fprintln(os.Stdout, "locked")
	_, _ = io.Copy(io.Discard, os.Stdin)
	if err := lock.Close(); err != nil {
		_, _ = fmt.Fprintln(os.Stderr, err)
		os.Exit(3)
	}
	os.Exit(0)
}

// TestAcquireExistingNeverCreatesTheDirectory: a missing directory is an
// fs.ErrNotExist error and stays missing; an existing one locks as Acquire
// does.
func TestAcquireExistingNeverCreatesTheDirectory(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "missing")
	if _, err := AcquireExisting(missing, ".store.lock"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("missing directory: %v", err)
	}
	if _, err := os.Stat(missing); !os.IsNotExist(err) {
		t.Fatalf("AcquireExisting created the directory: %v", err)
	}
	dir := t.TempDir()
	lock, err := AcquireExisting(dir, ".store.lock")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Acquire(dir, ".store.lock"); !errors.Is(err, ErrLocked) {
		t.Fatalf("second acquire: %v", err)
	}
	if err := lock.Close(); err != nil {
		t.Fatal(err)
	}
}

// TestAcquireExistingFileCreatesNothing: a missing lock file (or directory)
// is an fs.ErrNotExist error and stays missing; an existing lock file
// excludes Acquire and is excluded by it.
func TestAcquireExistingFileCreatesNothing(t *testing.T) {
	dir := t.TempDir()
	if _, err := AcquireExistingFile(dir, ".store.lock"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("missing lock file: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, ".store.lock")); !os.IsNotExist(err) {
		t.Fatalf("AcquireExistingFile created the lock file: %v", err)
	}
	missing := filepath.Join(dir, "missing")
	if _, err := AcquireExistingFile(missing, ".store.lock"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("missing directory: %v", err)
	}
	if _, err := os.Stat(missing); !os.IsNotExist(err) {
		t.Fatalf("AcquireExistingFile created the directory: %v", err)
	}
	owner, err := Acquire(dir, ".store.lock")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := AcquireExistingFile(dir, ".store.lock"); !errors.Is(err, ErrLocked) {
		t.Fatalf("acquire under an owner: %v", err)
	}
	if err := owner.Close(); err != nil {
		t.Fatal(err)
	}
	reader, err := AcquireExistingFile(dir, ".store.lock")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Acquire(dir, ".store.lock"); !errors.Is(err, ErrLocked) {
		t.Fatalf("acquire under a reader: %v", err)
	}
	if err := reader.Close(); err != nil {
		t.Fatal(err)
	}
}
