//go:build linux

// Licensed under the Apache License, Version 2.0. See LICENSE and NOTICE.
package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestInquiryLockExclusionAndStableInode(t *testing.T) {
	path := filepath.Join(t.TempDir(), "lock")
	first, e := acquireInquiryLock(path)
	if e != nil {
		t.Fatal(e)
	}
	if second, e := acquireInquiryLock(path); e == nil {
		second.Close()
		t.Fatal("concurrent engine owner")
	}
	before, e := os.Stat(path)
	if e != nil {
		t.Fatal(e)
	}
	if e = first.Close(); e != nil {
		t.Fatal(e)
	}
	reopened, e := acquireInquiryLock(path)
	if e != nil {
		t.Fatal(e)
	}
	defer reopened.Close()
	after, e := os.Stat(path)
	if e != nil || !os.SameFile(before, after) {
		t.Fatal("changed lock inode")
	}
}
