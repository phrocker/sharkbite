//go:build aix || android || darwin || dragonfly || freebsd || illumos || ios || linux || netbsd || openbsd || solaris

package local

import (
	"context"
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

func TestLocal_ReplacementPreservesOwnerAndGroup(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("requires root to assign an alternate owner/group")
	}

	dir := t.TempDir()
	path := filepath.Join(dir, "target")
	if err := os.WriteFile(path, []byte("old"), 0o640); err != nil {
		t.Fatal(err)
	}

	const wantUID = 12345
	const wantGID = 12346
	if err := os.Chown(path, wantUID, wantGID); err != nil {
		t.Skipf("cannot assign alternate owner/group: %v", err)
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

	info, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		t.Fatalf("stat payload = %T, want *syscall.Stat_t", info.Sys())
	}
	if got := int(stat.Uid); got != wantUID {
		t.Fatalf("uid = %d, want %d", got, wantUID)
	}
	if got := int(stat.Gid); got != wantGID {
		t.Fatalf("gid = %d, want %d", got, wantGID)
	}
}
