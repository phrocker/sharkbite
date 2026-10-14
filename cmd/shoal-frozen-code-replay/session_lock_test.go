//go:build linux

// Licensed under the Apache License, Version 2.0. See LICENSE and NOTICE.
package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestSessionLockChild(t *testing.T) {
	dir := os.Getenv("SHOAL_SESSION_LOCK_PROBE")
	if dir == "" {
		return
	}
	_, e := acquireSessionLock(dir)
	if e != nil {
		os.Exit(17)
	}
	// No Go cleanup: kernel must release the lock when this process exits.
	os.Exit(0)
}
func TestSessionLockExcludesProcessesAndCanReopen(t *testing.T) {
	dir := t.TempDir()
	lock, e := acquireSessionLock(dir)
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { lock.Close() })
	if second, e := acquireSessionLock(dir); e == nil {
		second.Close()
		t.Fatal("second handle acquired session")
	}
	child := func() error {
		exe, e := os.Executable()
		if e != nil {
			t.Fatal(e)
		}
		c := exec.Command(exe, "-test.run=^TestSessionLockChild$")
		c.Env = append(os.Environ(), "SHOAL_SESSION_LOCK_PROBE="+dir)
		return c.Run()
	}
	if e = child(); e == nil {
		t.Fatal("another process acquired owned session")
	} else if exit, ok := e.(*exec.ExitError); !ok || exit.ExitCode() != 17 {
		t.Fatalf("child failure: %v", e)
	}
	if e = lock.Close(); e != nil {
		t.Fatal(e)
	}
	if e = child(); e != nil {
		t.Fatalf("lock not released: %v", e)
	}
	if _, e = os.Stat(filepath.Join(dir, "service-session.lock")); e != nil {
		t.Fatal("lock inode removed", e)
	}
}
func TestSessionLockRefusesSymlink(t *testing.T) {
	dir := t.TempDir()
	outside := filepath.Join(t.TempDir(), "target")
	if e := os.WriteFile(outside, []byte("unchanged"), 0600); e != nil {
		t.Fatal(e)
	}
	if e := os.Symlink(outside, filepath.Join(dir, "service-session.lock")); e != nil {
		t.Fatal(e)
	}
	if lock, e := acquireSessionLock(dir); e == nil {
		lock.Close()
		t.Fatal("lock symlink accepted")
	}
}
func TestConcurrentInquiryRejectedBeforeStateMutation(t *testing.T) {
	dir, mh, ph, sh := serviceFixture(t)
	state := t.TempDir()
	lock, e := acquireSessionLock(state)
	if e != nil {
		t.Fatal(e)
	}
	defer lock.Close()
	if _, e = inquireServiceRows(dir, mh, ph, sh, state, 1); e == nil {
		t.Fatal("inquiry ignored occupied state")
	}
	for _, name := range []string{"service-state.json", "engine"} {
		if _, e = os.Stat(filepath.Join(state, name)); !os.IsNotExist(e) {
			t.Fatalf("contender mutated %s: %v", name, e)
		}
	}
}
