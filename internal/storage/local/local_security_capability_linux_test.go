//go:build linux && !android

package local

import (
	"context"
	"encoding/binary"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/unix"
)

const (
	vfsCapRevision2      = 0x02000000
	vfsCapRevision3      = 0x03000000
	vfsCapFlagsEffective = 0x00000001
)

func TestLocal_ReplacementDropsSecurityCapability(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("requires root or CAP_SETFCAP to assign file capabilities")
	}

	dir := t.TempDir()
	path := filepath.Join(dir, "target")
	if err := os.WriteFile(path, []byte("old"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := setSecurityCapabilityXattr(path); err != nil {
		if errors.Is(err, unix.ENOTSUP) || errors.Is(err, unix.EOPNOTSUPP) || errors.Is(err, unix.EPERM) || errors.Is(err, unix.EINVAL) {
			t.Skipf("security.capability unavailable: %v", err)
		}
		t.Fatal(err)
	}
	if _, err := getXattr(path, "security.capability"); err != nil {
		t.Fatalf("get security.capability before rewrite: %v", err)
	}
	if err := unix.Setxattr(path, "user.shoal-keep", []byte("target"), 0); err != nil {
		if errors.Is(err, unix.ENOTSUP) || errors.Is(err, unix.EOPNOTSUPP) || errors.Is(err, unix.EPERM) {
			t.Skipf("user xattrs unavailable: %v", err)
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

	if _, err := getXattr(path, "security.capability"); !isMissingXattr(err) {
		t.Fatalf("security.capability after rewrite = %v, want missing xattr", err)
	}
	if got, err := getXattr(path, "user.shoal-keep"); err != nil || string(got) != "target" {
		t.Fatalf("user xattr after rewrite = %q, %v; want target", got, err)
	}
}

func setSecurityCapabilityXattr(path string) error {
	for _, payload := range [][]byte{
		capabilityXattrRevision3(),
		capabilityXattrRevision2(),
	} {
		if err := unix.Setxattr(path, "security.capability", payload, 0); err == nil {
			return nil
		} else if !errors.Is(err, unix.EINVAL) {
			return err
		}
	}
	return unix.EINVAL
}

func capabilityXattrRevision2() []byte {
	payload := make([]byte, 20)
	binary.LittleEndian.PutUint32(payload[0:4], vfsCapRevision2|vfsCapFlagsEffective)
	binary.LittleEndian.PutUint32(payload[4:8], 1<<uint(unix.CAP_CHOWN))
	return payload
}

func capabilityXattrRevision3() []byte {
	payload := make([]byte, 24)
	binary.LittleEndian.PutUint32(payload[0:4], vfsCapRevision3|vfsCapFlagsEffective)
	binary.LittleEndian.PutUint32(payload[4:8], 1<<uint(unix.CAP_CHOWN))
	binary.LittleEndian.PutUint32(payload[20:24], 0)
	return payload
}
