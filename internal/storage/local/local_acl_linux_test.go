//go:build linux

package local

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestLocal_ReplacementPreservesACLs(t *testing.T) {
	setfacl, err := exec.LookPath("setfacl")
	if err != nil {
		t.Skip("setfacl is not installed")
	}
	getfacl, err := exec.LookPath("getfacl")
	if err != nil {
		t.Skip("getfacl is not installed")
	}

	dir := t.TempDir()
	path := filepath.Join(dir, "target")
	if err := os.WriteFile(path, []byte("old"), 0o640); err != nil {
		t.Fatal(err)
	}

	if out, err := exec.Command(setfacl, "-m", "u:0:r--", path).CombinedOutput(); err != nil {
		t.Skipf("acl manipulation unavailable: %v (%s)", err, strings.TrimSpace(string(out)))
	}
	before, err := exec.Command(getfacl, "-c", path).CombinedOutput()
	if err != nil {
		t.Fatalf("getfacl before replace: %v (%s)", err, strings.TrimSpace(string(before)))
	}

	w, err := New().Create(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write([]byte("new")); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}

	after, err := exec.Command(getfacl, "-c", path).CombinedOutput()
	if err != nil {
		t.Fatalf("getfacl after replace: %v (%s)", err, strings.TrimSpace(string(after)))
	}
	if strings.TrimSpace(string(after)) != strings.TrimSpace(string(before)) {
		t.Fatalf("acl changed after replacement\nbefore:\n%s\nafter:\n%s", before, after)
	}
}

func TestLocal_ReplacementRemovesInheritedDefaultACLs(t *testing.T) {
	setfacl, err := exec.LookPath("setfacl")
	if err != nil {
		t.Skip("setfacl is not installed")
	}
	getfacl, err := exec.LookPath("getfacl")
	if err != nil {
		t.Skip("getfacl is not installed")
	}

	dir := t.TempDir()
	path := filepath.Join(dir, "target")
	if err := os.WriteFile(path, []byte("old"), 0o640); err != nil {
		t.Fatal(err)
	}
	before, err := exec.Command(getfacl, "-c", path).CombinedOutput()
	if err != nil {
		t.Fatalf("getfacl before replace: %v (%s)", err, strings.TrimSpace(string(before)))
	}

	defaultACL := fmt.Sprintf("u:%d:r--", os.Getuid())
	if out, err := exec.Command(setfacl, "-d", "-m", defaultACL, dir).CombinedOutput(); err != nil {
		t.Skipf("default acl manipulation unavailable: %v (%s)", err, strings.TrimSpace(string(out)))
	}
	probe := filepath.Join(dir, "probe")
	if err := os.WriteFile(probe, []byte("probe"), 0o640); err != nil {
		t.Fatal(err)
	}
	probeACL, err := exec.Command(getfacl, "-c", probe).CombinedOutput()
	if err != nil {
		t.Fatalf("getfacl probe: %v (%s)", err, strings.TrimSpace(string(probeACL)))
	}
	if !strings.Contains(string(probeACL), defaultACL) {
		t.Skipf("default ACL did not apply to probe file:\n%s", probeACL)
	}

	w, err := New().Create(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write([]byte("new")); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}

	after, err := exec.Command(getfacl, "-c", path).CombinedOutput()
	if err != nil {
		t.Fatalf("getfacl after replace: %v (%s)", err, strings.TrimSpace(string(after)))
	}
	if strings.TrimSpace(string(after)) != strings.TrimSpace(string(before)) {
		t.Fatalf("acl changed after replacement with inherited default ACL\nbefore:\n%s\nafter:\n%s", before, after)
	}
}
