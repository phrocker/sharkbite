//go:build (linux && !android) || (darwin && !ios)

package local

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/unix"
)

func TestLocal_ReplacementPreservesExtendedAttributes(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "target")
	if err := os.WriteFile(path, []byte("old"), 0o640); err != nil {
		t.Fatal(err)
	}

	const attr = "user.shoal-preserve"
	want := []byte("value")
	if err := setXattr(path, attr, want, 0); err != nil {
		if errors.Is(err, unix.ENOTSUP) || errors.Is(err, unix.EOPNOTSUPP) || errors.Is(err, unix.EPERM) {
			t.Skipf("extended attributes unavailable: %v", err)
		}
		t.Fatal(err)
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

	got, err := getXattr(path, attr)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(want) {
		t.Fatalf("xattr %s = %q, want %q", attr, got, want)
	}
}

func TestPreservePlatformXattrsRemovesAttributesAbsentFromTarget(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "target")
	temp := filepath.Join(dir, "temp")
	if err := os.WriteFile(target, []byte("old"), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(temp, []byte("new"), 0o640); err != nil {
		t.Fatal(err)
	}

	const (
		keepAttr  = "user.shoal-keep"
		extraAttr = "user.shoal-inherited"
	)
	if err := setXattr(target, keepAttr, []byte("target"), 0); err != nil {
		if errors.Is(err, unix.ENOTSUP) || errors.Is(err, unix.EOPNOTSUPP) || errors.Is(err, unix.EPERM) {
			t.Skipf("extended attributes unavailable: %v", err)
		}
		t.Fatal(err)
	}
	if err := setXattr(temp, keepAttr, []byte("inherited"), 0); err != nil {
		t.Fatal(err)
	}
	if err := setXattr(temp, extraAttr, []byte("remove"), 0); err != nil {
		t.Fatal(err)
	}

	if err := preservePlatformXattrs(temp, target); err != nil {
		t.Fatal(err)
	}
	if got, err := getXattr(temp, keepAttr); err != nil || string(got) != "target" {
		t.Fatalf("preserved attribute = %q, %v; want target", got, err)
	}
	if _, err := getXattr(temp, extraAttr); !isMissingXattr(err) {
		t.Fatalf("extra inherited attribute remains: %v", err)
	}
}
