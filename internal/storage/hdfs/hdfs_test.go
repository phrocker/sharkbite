package hdfs

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"os"
	"path"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	hdfsclient "github.com/colinmarc/hdfs/v2"
	"github.com/phrocker/shoal-oss/internal/storage"
)

func TestBackendOpenQualifiedPath(t *testing.T) {
	client := newFakeClient()
	client.files["/tables/1.rf"] = []byte("rfile")
	backend, err := New("hdfs://nn:8020", WithClient(client))
	if err != nil {
		t.Fatal(err)
	}

	f, err := backend.Open(context.Background(), "hdfs://nn:8020/tables/1.rf")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	got := make([]byte, f.Size())
	if _, err := f.ReadAt(got, 0); err != nil {
		t.Fatal(err)
	}
	if string(got) != "rfile" {
		t.Fatalf("got %q, want rfile", got)
	}
}

func TestBackendCreateReportsPublishAndRestoreFailures(t *testing.T) {
	client := newFakeClient()
	client.files["/tables/1.rf"] = []byte("old")
	client.failPublish = true
	client.failRestore = true
	backend, err := New("nn:8020", WithClient(client))
	if err != nil {
		t.Fatal(err)
	}

	w, err := backend.Create(context.Background(), "/tables/1.rf")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write([]byte("new")); err != nil {
		t.Fatal(err)
	}
	err = w.Close()
	if err == nil {
		t.Fatal("Close succeeded, want publish and restore failures")
	}
	if !strings.Contains(err.Error(), "publish /tables/1.rf") {
		t.Fatalf("Close error %q does not include publish failure", err)
	}
	if !strings.Contains(err.Error(), "restore existing file /tables/1.rf") {
		t.Fatalf("Close error %q does not include restore failure", err)
	}
}

func TestBackendCreateReplacesAndCreatesParents(t *testing.T) {
	client := newFakeClient()
	client.files["/tables/1.rf"] = []byte("old")
	backend, err := New("nn:8020", WithClient(client))
	if err != nil {
		t.Fatal(err)
	}

	w, err := backend.Create(context.Background(), "/tables/1.rf")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write([]byte("new")); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}

	if client.mkdir != "/tables" {
		t.Fatalf("MkdirAll path = %q, want /tables", client.mkdir)
	}
	if client.writerCloseCalls != 1 {
		t.Fatalf("writer Close calls = %d, want 1", client.writerCloseCalls)
	}
	if got := string(client.files["/tables/1.rf"]); got != "new" {
		t.Fatalf("created contents = %q, want new", got)
	}
}

func TestBackendCreateSecondCloseIsNoop(t *testing.T) {
	client := newFakeClient()
	backend, err := New("nn:8020", WithClient(client))
	if err != nil {
		t.Fatal(err)
	}

	w, err := backend.Create(context.Background(), "/tables/1.rf")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write([]byte("new")); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
	if client.writerCloseCalls != 1 {
		t.Fatalf("writer Close calls = %d, want 1", client.writerCloseCalls)
	}
}

func TestBackendCreateFailurePreservesExistingFile(t *testing.T) {
	client := newFakeClient()
	client.files["/tables/1.rf"] = []byte("old")
	client.failWriterClose = true
	backend, err := New("nn:8020", WithClient(client))
	if err != nil {
		t.Fatal(err)
	}

	w, err := backend.Create(context.Background(), "/tables/1.rf")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write([]byte("new")); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err == nil {
		t.Fatal("Close succeeded, want injected failure")
	}
	if got := string(client.files["/tables/1.rf"]); got != "old" {
		t.Fatalf("existing contents = %q, want old", got)
	}
	client.lastWriter.failClose = false
	if err := w.(storage.Aborter).Abort(); err != nil {
		t.Fatalf("Abort after failed Close: %v", err)
	}
}

func TestBackendCreateRetriesReplicationInProgress(t *testing.T) {
	client := newFakeClient()
	client.replicatingCloseFailures = 2
	backend, err := New("nn:8020", WithClient(client))
	if err != nil {
		t.Fatal(err)
	}

	w, err := backend.Create(context.Background(), "/tables/1.rf")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write([]byte("new")); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}

	if client.writerCloseCalls != 3 {
		t.Fatalf("writer Close calls = %d, want 3", client.writerCloseCalls)
	}
	if got := string(client.files["/tables/1.rf"]); got != "new" {
		t.Fatalf("created contents = %q, want new", got)
	}
}

func TestBackendLongNameTargetsUseFixedLengthSiblingArtifacts(t *testing.T) {
	t.Run("create", func(t *testing.T) {
		setReplacementNameTokens(
			t,
			strings.Repeat("c", replacementNameTokenBytes*2),
			strings.Repeat("1", replacementNameTokenBytes*2),
		)

		client := newFakeClient()
		backend, err := New("nn:8020", WithClient(client))
		if err != nil {
			t.Fatal(err)
		}

		target := "/tables/" + strings.Repeat("c", 255)
		w, err := backend.Create(context.Background(), target)
		if err != nil {
			t.Fatal(err)
		}
		checkReplacementSiblingPath(t, client.lastCreatePath, "/tables", replacementTempPrefix)
		if _, err := w.Write([]byte("create")); err != nil {
			t.Fatal(err)
		}
		if err := w.Close(); err != nil {
			t.Fatal(err)
		}
		if got := string(client.files[target]); got != "create" {
			t.Fatalf("created contents = %q, want create", got)
		}
	})

	t.Run("replace", func(t *testing.T) {
		setReplacementNameTokens(
			t,
			strings.Repeat("d", replacementNameTokenBytes*2),
			strings.Repeat("e", replacementNameTokenBytes*2),
		)

		client := newFakeClient()
		target := "/tables/" + strings.Repeat("r", 255)
		client.files[target] = []byte("old")
		backend, err := New("nn:8020", WithClient(client))
		if err != nil {
			t.Fatal(err)
		}

		w, err := backend.Create(context.Background(), target)
		if err != nil {
			t.Fatal(err)
		}
		checkReplacementSiblingPath(t, client.lastCreatePath, "/tables", replacementTempPrefix)
		if _, err := w.Write([]byte("replace")); err != nil {
			t.Fatal(err)
		}
		if err := w.Close(); err != nil {
			t.Fatal(err)
		}
		if got := string(client.files[target]); got != "replace" {
			t.Fatalf("replaced contents = %q, want replace", got)
		}
		if len(client.renameCalls) == 0 {
			t.Fatal("missing backup rename call")
		}
		checkReplacementSiblingPath(t, client.renameCalls[0].newpath, "/tables", replacementBackupPrefix)
	})

	t.Run("rollback", func(t *testing.T) {
		setReplacementNameTokens(
			t,
			strings.Repeat("f", replacementNameTokenBytes*2),
			strings.Repeat("0", replacementNameTokenBytes*2),
		)

		client := newFakeClient()
		target := "/tables/" + strings.Repeat("a", 255)
		client.files[target] = []byte("old")
		client.failBackupRemove = true
		backend, err := New("nn:8020", WithClient(client))
		if err != nil {
			t.Fatal(err)
		}

		err = storage.WriteAll(context.Background(), backend, target, []byte("rollback"))
		if err == nil || !strings.Contains(err.Error(), "remove replacement backup") {
			t.Fatalf("WriteAll error = %v, want backup cleanup failure", err)
		}
		checkReplacementSiblingPath(t, client.lastCreatePath, "/tables", replacementTempPrefix)
		if len(client.renameCalls) == 0 {
			t.Fatal("missing backup rename call")
		}
		checkReplacementSiblingPath(t, client.renameCalls[0].newpath, "/tables", replacementBackupPrefix)
		if got := string(client.files[target]); got != "old" {
			t.Fatalf("rolled back contents = %q, want old", got)
		}
	})
}

func TestBackendTemporarySiblingNameCollisionRetries(t *testing.T) {
	setReplacementNameTokens(t, "collision", "allocated")

	client := newFakeClient()
	client.files["/tables/"+replacementTempPrefix+"collision"] = []byte("sentinel")
	backend, err := New("nn:8020", WithClient(client))
	if err != nil {
		t.Fatal(err)
	}

	w, err := backend.Create(context.Background(), "/tables/target.rf")
	if err != nil {
		t.Fatal(err)
	}
	if got := client.lastCreatePath; got != "/tables/"+replacementTempPrefix+"allocated" {
		t.Fatalf("temporary path = %q, want allocated retry", got)
	}
	if err := w.(storage.Aborter).Abort(); err != nil {
		t.Fatalf("Abort: %v", err)
	}
}

func TestBackendBackupRemovalFailureRollsBackPublishedReplacement(t *testing.T) {
	client := newFakeClient()
	client.files["/tables/1.rf"] = []byte("old")
	client.failBackupRemove = true
	backend, err := New("nn:8020", WithClient(client))
	if err != nil {
		t.Fatal(err)
	}

	err = storage.WriteAll(context.Background(), backend, "/tables/1.rf", []byte("new"))
	if err == nil || !strings.Contains(err.Error(), "remove replacement backup") {
		t.Fatalf("WriteAll error = %v, want backup cleanup failure", err)
	}
	if got := string(client.files["/tables/1.rf"]); got != "old" {
		t.Fatalf("target contents = %q, want original data", got)
	}
	if _, ok := client.files[client.lastCreatePath]; ok {
		t.Fatalf("temporary file %s remains after abort", client.lastCreatePath)
	}
	for name := range client.files {
		if strings.Contains(name, ".shoal-backup-") {
			t.Fatalf("backup %s remains after rollback", name)
		}
	}
	if len(client.renameCalls) != 4 {
		t.Fatalf("Rename calls = %v, want preserve, publish, unpublish, restore", client.renameCalls)
	}
	backup := client.renameCalls[0].newpath
	wantRenames := []renameCall{
		{oldpath: "/tables/1.rf", newpath: backup},
		{oldpath: client.lastCreatePath, newpath: "/tables/1.rf"},
		{oldpath: "/tables/1.rf", newpath: client.lastCreatePath},
		{oldpath: backup, newpath: "/tables/1.rf"},
	}
	if !slices.Equal(client.renameCalls, wantRenames) {
		t.Fatalf("Rename calls = %v, want %v", client.renameCalls, wantRenames)
	}
}

func TestBackendAmbiguousBackupRenameRestoresTargetBeforeReturning(t *testing.T) {
	client := newFakeClient()
	client.files["/tables/1.rf"] = []byte("old")
	client.renameContextHook = func(_ context.Context, oldpath, newpath string) error {
		if oldpath == "/tables/1.rf" && strings.Contains(newpath, ".shoal-backup-") {
			client.files[newpath] = append([]byte(nil), client.files[oldpath]...)
			delete(client.files, oldpath)
			return context.DeadlineExceeded
		}
		return nil
	}
	backend, err := New("nn:8020", WithClient(client))
	if err != nil {
		t.Fatal(err)
	}

	w, err := backend.Create(context.Background(), "/tables/1.rf")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write([]byte("new")); err != nil {
		t.Fatal(err)
	}

	err = w.Close()
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Close error = %v, want context deadline exceeded", err)
	}
	if got := string(client.files["/tables/1.rf"]); got != "old" {
		t.Fatalf("target contents = %q, want old", got)
	}
	for name := range client.files {
		if strings.Contains(name, ".shoal-backup-") {
			t.Fatalf("backup %s remains after failed preserve rename", name)
		}
	}
	if _, ok := client.files[client.lastCreatePath]; !ok {
		t.Fatalf("temporary file %s missing after unsuccessful Close", client.lastCreatePath)
	}
	if err := w.(storage.Aborter).Abort(); err != nil {
		t.Fatalf("Abort after unsuccessful Close: %v", err)
	}
	if _, ok := client.files[client.lastCreatePath]; ok {
		t.Fatalf("temporary file %s remains after Abort", client.lastCreatePath)
	}
}

func TestBackendAmbiguousPublishRenameClassifiesCommittedTarget(t *testing.T) {
	for _, tc := range []struct {
		name       string
		existing   bool
		retainTemp bool
	}{
		{name: "replace-existing-target", existing: true, retainTemp: false},
		{name: "replace-existing-target-retained-temp", existing: true, retainTemp: true},
		{name: "publish-absent-target", existing: false, retainTemp: false},
		{name: "publish-absent-target-retained-temp", existing: false, retainTemp: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client := newFakeClient()
			if tc.existing {
				client.files["/tables/1.rf"] = []byte("old")
			}
			client.renameContextHook = func(_ context.Context, oldpath, newpath string) error {
				if strings.Contains(oldpath, replacementTempPrefix) && newpath == "/tables/1.rf" {
					client.files[newpath] = append([]byte(nil), client.files[oldpath]...)
					if !tc.retainTemp {
						delete(client.files, oldpath)
					}
					return context.DeadlineExceeded
				}
				return nil
			}
			backend, err := New("nn:8020", WithClient(client))
			if err != nil {
				t.Fatal(err)
			}

			if err := storage.WriteAll(context.Background(), backend, "/tables/1.rf", []byte("new")); err != nil {
				t.Fatalf("WriteAll: %v", err)
			}
			if got := string(client.files["/tables/1.rf"]); got != "new" {
				t.Fatalf("target contents = %q, want new", got)
			}
			if _, ok := client.files[client.lastCreatePath]; ok {
				t.Fatalf("temporary file %s remains after committed publish", client.lastCreatePath)
			}
			for name := range client.files {
				if strings.Contains(name, replacementBackupPrefix) {
					t.Fatalf("backup %s remains after committed publish", name)
				}
			}
		})
	}
}

func TestBackendAmbiguousPublishRenamePreservesConcurrentTargetAndBackup(t *testing.T) {
	client := newFakeClient()
	client.files["/tables/1.rf"] = []byte("old")
	client.renameContextHook = func(_ context.Context, oldpath, newpath string) error {
		if strings.Contains(oldpath, replacementTempPrefix) && newpath == "/tables/1.rf" {
			client.files[newpath] = []byte("concurrent")
			return context.DeadlineExceeded
		}
		return nil
	}
	backend, err := New("nn:8020", WithClient(client))
	if err != nil {
		t.Fatal(err)
	}

	w, err := backend.Create(context.Background(), "/tables/1.rf")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write([]byte("new")); err != nil {
		t.Fatal(err)
	}

	err = w.Close()
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Close error = %v, want context deadline exceeded", err)
	}
	if !strings.Contains(err.Error(), "refusing to delete destination or restore backup over it") {
		t.Fatalf("Close error = %v, want concurrent-target safeguard", err)
	}
	if got := string(client.files["/tables/1.rf"]); got != "concurrent" {
		t.Fatalf("target contents = %q, want concurrent data preserved", got)
	}
	if _, ok := client.files[client.lastCreatePath]; !ok {
		t.Fatalf("temporary file %s missing after unsuccessful Close", client.lastCreatePath)
	}
	if len(client.renameCalls) != 1 {
		t.Fatalf("Rename calls = %v, want preserve only", client.renameCalls)
	}
	backup := client.renameCalls[0].newpath
	if got := string(client.files[backup]); got != "old" {
		t.Fatalf("backup contents = %q, want preserved old data", got)
	}
	if err := w.(storage.Aborter).Abort(); err == nil || !strings.Contains(err.Error(), "cannot be safely aborted") {
		t.Fatalf("Abort after unsafe Close error = %v, want unabortable safeguard", err)
	}
}

func TestBackendAmbiguousPublishRenamePreservesConcurrentTargetWithoutBackup(t *testing.T) {
	client := newFakeClient()
	client.renameContextHook = func(_ context.Context, oldpath, newpath string) error {
		if strings.Contains(oldpath, replacementTempPrefix) && newpath == "/tables/1.rf" {
			client.files[newpath] = []byte("concurrent")
			return context.DeadlineExceeded
		}
		return nil
	}
	backend, err := New("nn:8020", WithClient(client))
	if err != nil {
		t.Fatal(err)
	}

	w, err := backend.Create(context.Background(), "/tables/1.rf")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write([]byte("new")); err != nil {
		t.Fatal(err)
	}

	err = w.Close()
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Close error = %v, want context deadline exceeded", err)
	}
	if !strings.Contains(err.Error(), "refusing to delete destination") {
		t.Fatalf("Close error = %v, want concurrent-target safeguard", err)
	}
	if got := string(client.files["/tables/1.rf"]); got != "concurrent" {
		t.Fatalf("target contents = %q, want concurrent data preserved", got)
	}
	if _, ok := client.files[client.lastCreatePath]; !ok {
		t.Fatalf("temporary file %s missing after unsuccessful Close", client.lastCreatePath)
	}
	for name := range client.files {
		if strings.Contains(name, replacementBackupPrefix) {
			t.Fatalf("unexpected backup %s remains for absent original target", name)
		}
	}
	if err := w.(storage.Aborter).Abort(); err == nil || !strings.Contains(err.Error(), "cannot be safely aborted") {
		t.Fatalf("Abort after unsafe Close error = %v, want unabortable safeguard", err)
	}
}

func TestBackendAmbiguousPublishRenameRestoresBackupWhenTargetAbsentAndRetainsTempForAbort(t *testing.T) {
	client := newFakeClient()
	client.files["/tables/1.rf"] = []byte("old")
	client.renameContextHook = func(_ context.Context, oldpath, newpath string) error {
		if strings.Contains(oldpath, replacementTempPrefix) && newpath == "/tables/1.rf" {
			return context.DeadlineExceeded
		}
		return nil
	}
	backend, err := New("nn:8020", WithClient(client))
	if err != nil {
		t.Fatal(err)
	}

	w, err := backend.Create(context.Background(), "/tables/1.rf")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write([]byte("new")); err != nil {
		t.Fatal(err)
	}

	err = w.Close()
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Close error = %v, want context deadline exceeded", err)
	}
	if got := string(client.files["/tables/1.rf"]); got != "old" {
		t.Fatalf("target contents = %q, want restored old data", got)
	}
	if _, ok := client.files[client.lastCreatePath]; !ok {
		t.Fatalf("temporary file %s missing after unsuccessful Close", client.lastCreatePath)
	}
	for name := range client.files {
		if strings.Contains(name, replacementBackupPrefix) {
			t.Fatalf("backup %s remains after restore", name)
		}
	}
	if err := w.(storage.Aborter).Abort(); err != nil {
		t.Fatalf("Abort after unsuccessful Close: %v", err)
	}
	if _, ok := client.files[client.lastCreatePath]; ok {
		t.Fatalf("temporary file %s remains after Abort", client.lastCreatePath)
	}
}

func TestBackendCommittedBackupDeleteDoesNotRollbackReplacement(t *testing.T) {
	client := newFakeClient()
	client.files["/tables/1.rf"] = []byte("old")
	client.removeContextHook = func(_ context.Context, name string) error {
		if strings.Contains(name, ".shoal-backup-") {
			delete(client.files, name)
			return context.DeadlineExceeded
		}
		return nil
	}
	backend, err := New("nn:8020", WithClient(client))
	if err != nil {
		t.Fatal(err)
	}

	if err := storage.WriteAll(context.Background(), backend, "/tables/1.rf", []byte("new")); err != nil {
		t.Fatalf("WriteAll: %v", err)
	}
	if got := string(client.files["/tables/1.rf"]); got != "new" {
		t.Fatalf("target contents = %q, want new", got)
	}
	for name := range client.files {
		if strings.Contains(name, ".shoal-backup-") {
			t.Fatalf("backup %s remains after committed delete", name)
		}
	}
	if len(client.renameCalls) != 2 {
		t.Fatalf("Rename calls = %v, want preserve and publish only", client.renameCalls)
	}
}

func TestBackendBackupDeleteFailureRestoresOldTargetWhenPublishedTargetDisappeared(t *testing.T) {
	client := newFakeClient()
	client.files["/tables/1.rf"] = []byte("old")
	client.removeContextHook = func(_ context.Context, name string) error {
		if strings.Contains(name, ".shoal-backup-") {
			delete(client.files, "/tables/1.rf")
			return context.DeadlineExceeded
		}
		return nil
	}
	backend, err := New("nn:8020", WithClient(client))
	if err != nil {
		t.Fatal(err)
	}

	err = storage.WriteAll(context.Background(), backend, "/tables/1.rf", []byte("new"))
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("WriteAll error = %v, want context deadline exceeded", err)
	}
	if got := string(client.files["/tables/1.rf"]); got != "old" {
		t.Fatalf("target contents = %q, want restored old data", got)
	}
}

func TestBackendBackupDeleteFailureDoesNotRollbackConcurrentTarget(t *testing.T) {
	client := newFakeClient()
	client.files["/tables/1.rf"] = []byte("old")
	client.removeContextHook = func(_ context.Context, name string) error {
		if strings.Contains(name, ".shoal-backup-") {
			client.files["/tables/1.rf"] = []byte("concurrent")
			return context.DeadlineExceeded
		}
		return nil
	}
	backend, err := New("nn:8020", WithClient(client))
	if err != nil {
		t.Fatal(err)
	}

	err = storage.WriteAll(context.Background(), backend, "/tables/1.rf", []byte("new"))
	if err == nil || !strings.Contains(err.Error(), "changed concurrently") {
		t.Fatalf("WriteAll error = %v, want concurrent mutation safeguard", err)
	}
	if got := string(client.files["/tables/1.rf"]); got != "concurrent" {
		t.Fatalf("target contents = %q, want concurrent data", got)
	}
}

func TestBackendAmbiguousBackupRenameDoesNotOverwriteConcurrentTarget(t *testing.T) {
	client := newFakeClient()
	client.files["/tables/1.rf"] = []byte("old")
	client.renameContextHook = func(_ context.Context, oldpath, newpath string) error {
		if oldpath == "/tables/1.rf" && strings.Contains(newpath, ".shoal-backup-") {
			client.files[newpath] = append([]byte(nil), client.files[oldpath]...)
			client.files[oldpath] = []byte("concurrent")
			return context.DeadlineExceeded
		}
		return nil
	}
	backend, err := New("nn:8020", WithClient(client))
	if err != nil {
		t.Fatal(err)
	}

	err = storage.WriteAll(context.Background(), backend, "/tables/1.rf", []byte("new"))
	if err == nil || !strings.Contains(err.Error(), "concurrently recreated") {
		t.Fatalf("WriteAll error = %v, want concurrent recreation safeguard", err)
	}
	if got := string(client.files["/tables/1.rf"]); got != "concurrent" {
		t.Fatalf("target contents = %q, want concurrent data", got)
	}
}

func TestBackendListPreservesPathForm(t *testing.T) {
	client := newFakeClient()
	client.files["/tables/1.rf"] = []byte("one")
	client.files["/tables/2.rf"] = []byte("two")
	client.dirs["/tables/nested"] = true
	backend, err := New("nn:8020", WithClient(client))
	if err != nil {
		t.Fatal(err)
	}

	got, err := backend.List(context.Background(), "hdfs://nn:8020/tables")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"hdfs://nn:8020/tables/1.rf",
		"hdfs://nn:8020/tables/2.rf",
	}
	slices.Sort(got)
	slices.Sort(want)
	if len(got) != len(want) {
		t.Fatalf("List returned %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("List returned %v, want %v", got, want)
		}
	}
}

func TestBackendListHidesGeneratedReplacementArtifacts(t *testing.T) {
	client := newFakeClient()
	client.files["/tables/1.rf"] = []byte("one")
	client.files["/tables/.shl-user-visible"] = []byte("user")
	client.files["/tables/.shl-aaaaaaaaa"] = []byte("user")
	client.files["/tables/"+replacementTempPrefix+"visible"] = []byte("visible")
	client.files["/tables/"+replacementBackupPrefix+"visible"] = []byte("visible")
	client.files["/tables/"+replacementTempPrefix+strings.Repeat("A", replacementNameTokenBytes*2)] = []byte("visible")
	client.files["/tables/"+replacementBackupPrefix+strings.Repeat("B", replacementNameTokenBytes*2)] = []byte("visible")
	client.files["/tables/"+replacementTempPrefix+strings.Repeat("c", replacementNameTokenBytes*2+1)] = []byte("visible")
	client.files["/tables/"+replacementBackupPrefix+strings.Repeat("d", replacementNameTokenBytes*2+1)] = []byte("visible")
	client.files["/tables/"+replacementTempPrefix+strings.Repeat("a", replacementNameTokenBytes*2)] = []byte("temp")
	client.files["/tables/"+replacementBackupPrefix+strings.Repeat("b", replacementNameTokenBytes*2)] = []byte("backup")
	backend, err := New("nn:8020", WithClient(client))
	if err != nil {
		t.Fatal(err)
	}

	got, err := backend.List(context.Background(), "hdfs://nn:8020/tables")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"hdfs://nn:8020/tables/1.rf",
		"hdfs://nn:8020/tables/.shl-user-visible",
		"hdfs://nn:8020/tables/.shl-aaaaaaaaa",
		"hdfs://nn:8020/tables/" + replacementTempPrefix + "visible",
		"hdfs://nn:8020/tables/" + replacementBackupPrefix + "visible",
		"hdfs://nn:8020/tables/" + replacementTempPrefix + strings.Repeat("A", replacementNameTokenBytes*2),
		"hdfs://nn:8020/tables/" + replacementBackupPrefix + strings.Repeat("B", replacementNameTokenBytes*2),
		"hdfs://nn:8020/tables/" + replacementTempPrefix + strings.Repeat("c", replacementNameTokenBytes*2+1),
		"hdfs://nn:8020/tables/" + replacementBackupPrefix + strings.Repeat("d", replacementNameTokenBytes*2+1),
	}
	slices.Sort(got)
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Fatalf("List returned %v, want %v", got, want)
	}
}

func TestBackendCleanupStaleArtifacts(t *testing.T) {
	client := newFakeClient()
	now := time.Now()
	oldTemp := "/tables/" + replacementTempPrefix + "00112233445566778899aabbccddeeff"
	oldBackup := "/tables/" + replacementBackupPrefix + "ffeeddccbbaa99887766554433221100"
	recent := "/tables/" + replacementTempPrefix + "11112222333344445555666677778888"
	lookalike := "/tables/" + replacementTempPrefix + "not-a-token"
	uppercase := "/tables/" + replacementTempPrefix + "AABBCCDDEEFF00112233445566778899"
	for _, name := range []string{oldTemp, oldBackup, recent, lookalike, uppercase} {
		client.files[name] = []byte("x")
		client.modTimes[name] = now
	}
	client.modTimes[oldTemp] = now.Add(-2 * time.Hour)
	client.modTimes[oldBackup] = now.Add(-2 * time.Hour)
	backend, err := New("nn:8020", WithClient(client))
	if err != nil {
		t.Fatal(err)
	}

	result, err := backend.CleanupStaleArtifacts(context.Background(), "/tables", now.Add(-time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if result.Examined != 3 {
		t.Fatalf("Examined = %d, want 3", result.Examined)
	}
	want := []string{oldTemp}
	if fmt.Sprint(result.Removed) != fmt.Sprint(want) {
		t.Fatalf("Removed = %v, want %v", result.Removed, want)
	}
	if len(result.Recoverable) != 1 || result.Recoverable[0] != oldBackup {
		t.Fatalf("Recoverable = %v, want [%s]", result.Recoverable, oldBackup)
	}
	for _, name := range []string{oldBackup, recent, lookalike, uppercase} {
		if _, ok := client.files[name]; !ok {
			t.Fatalf("%s was removed", name)
		}
	}
	if err := client.Remove(recent); err != nil {
		t.Fatal(err)
	}
	result, err = backend.CleanupStaleArtifacts(context.Background(), "/tables", now.Add(-time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Removed) != 0 {
		t.Fatalf("second Removed = %v, want none", result.Removed)
	}
	if len(result.Recoverable) != 1 || result.Recoverable[0] != oldBackup {
		t.Fatalf("second Recoverable = %v, want [%s]", result.Recoverable, oldBackup)
	}
}

func TestBackendCleanupStaleArtifactsPreservesActiveAndReportsFailures(t *testing.T) {
	client := newFakeClient()
	backend, err := New("nn:8020", WithClient(client))
	if err != nil {
		t.Fatal(err)
	}
	out, err := backend.Create(context.Background(), "/tables/target")
	if err != nil {
		t.Fatal(err)
	}
	w := out.(*replaceWriter)
	oldTemp := "/tables/" + replacementTempPrefix + "11112222333344445555666677778888"
	oldBackup := "/tables/" + replacementBackupPrefix + "00112233445566778899aabbccddeeff"
	client.files[oldTemp] = []byte("temp")
	client.modTimes[oldTemp] = time.Now().Add(-2 * time.Hour)
	client.files[oldBackup] = []byte("old")
	client.modTimes[oldBackup] = time.Now().Add(-2 * time.Hour)
	removeErr := errors.New("janitor remove failed")
	client.removeContextHook = func(_ context.Context, name string) error {
		if name == oldTemp {
			return removeErr
		}
		return nil
	}

	result, err := backend.CleanupStaleArtifacts(context.Background(), "/tables", time.Now().Add(-time.Hour))
	if !errors.Is(err, removeErr) {
		t.Fatalf("cleanup with active writer error = %v, want remove failure", err)
	}
	if len(result.Removed) != 0 {
		t.Fatalf("Removed = %v, want none", result.Removed)
	}
	if len(result.Recoverable) != 1 || result.Recoverable[0] != oldBackup {
		t.Fatalf("Recoverable = %v, want [%s]", result.Recoverable, oldBackup)
	}
	if _, ok := client.files[w.temp]; !ok {
		t.Fatal("active temporary file was removed")
	}
	if err := out.(storage.Aborter).Abort(); err != nil {
		t.Fatal(err)
	}
	result, err = backend.CleanupStaleArtifacts(context.Background(), "/tables", time.Now().Add(-time.Hour))
	if !errors.Is(err, removeErr) {
		t.Fatalf("error = %v, want remove failure", err)
	}
	if len(result.Removed) != 0 {
		t.Fatalf("Removed after failure = %v, want none", result.Removed)
	}
	if len(result.Recoverable) != 1 || result.Recoverable[0] != oldBackup {
		t.Fatalf("Recoverable after failure = %v, want [%s]", result.Recoverable, oldBackup)
	}
	client.removeContextHook = nil

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := backend.CleanupStaleArtifacts(ctx, "/tables", time.Now()); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled cleanup error = %v, want context.Canceled", err)
	}
}

func TestBackendCreateRejectsReservedReplacementArtifactNames(t *testing.T) {
	client := newFakeClient()
	backend, err := New("nn:8020", WithClient(client))
	if err != nil {
		t.Fatal(err)
	}

	for _, name := range []string{
		replacementTempPrefix + strings.Repeat("a", replacementNameTokenBytes*2),
		replacementBackupPrefix + strings.Repeat("b", replacementNameTokenBytes*2),
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := backend.Create(context.Background(), "hdfs://nn:8020/tables/"+name); err == nil || !strings.Contains(err.Error(), "reserved internal namespace") {
				t.Fatalf("Create(%q) error = %v, want reserved internal namespace rejection", name, err)
			}
		})
	}
	if got := len(client.files); got != 0 {
		t.Fatalf("rejected Create should not stage files, got %d", got)
	}
}

func TestBackendCreateAllowsUserNamesOutsideReservedNamespace(t *testing.T) {
	names := []string{
		".shl-final.rf",
		".shl-aaaaaaaaa",
		replacementTempPrefix + strings.Repeat("A", replacementNameTokenBytes*2),
		replacementBackupPrefix + strings.Repeat("B", replacementNameTokenBytes*2),
		replacementTempPrefix + strings.Repeat("e", replacementNameTokenBytes*2+1),
		replacementBackupPrefix + strings.Repeat("f", replacementNameTokenBytes*2+1),
		"nested/.shl-final.rf",
	}
	for _, name := range names {
		t.Run(name, func(t *testing.T) {
			client := newFakeClient()
			backend, err := New("nn:8020", WithClient(client))
			if err != nil {
				t.Fatal(err)
			}
			w, err := backend.Create(context.Background(), "hdfs://nn:8020/tables/"+name)
			if err != nil {
				t.Fatalf("Create(%q): %v", name, err)
			}
			if _, err := w.Write([]byte("data")); err != nil {
				t.Fatalf("Write(%q): %v", name, err)
			}
			if err := w.Close(); err != nil {
				t.Fatalf("Close(%q): %v", name, err)
			}
			if got := string(client.files["/tables/"+name]); got != "data" {
				t.Fatalf("stored data for %q = %q, want data", name, got)
			}
		})
	}
}

func TestFileMatchesAppliesCleanupDeadlineBeforeRead(t *testing.T) {
	client := newFakeClient()
	data := []byte("published")
	client.files["/tables/target.rf"] = data
	deadline := time.Now().Add(time.Minute).Round(0)
	ctx, cancel := context.WithDeadline(context.Background(), deadline)
	defer cancel()
	digest := sha256.Sum256(data)

	matches, err := fileMatches(ctx, client, "/tables/target.rf", int64(len(data)), digest[:])
	if err != nil {
		t.Fatalf("fileMatches: %v", err)
	}
	if !matches {
		t.Fatal("fileMatches = false, want true")
	}
	if got := client.lastReader.deadline; !got.Equal(deadline) {
		t.Fatalf("reader deadline = %v, want %v", got, deadline)
	}
}

func TestBackendNotFoundSemantics(t *testing.T) {
	client := newFakeClient()
	backend, err := New("nn:8020", WithClient(client))
	if err != nil {
		t.Fatal(err)
	}

	if _, err := backend.Open(context.Background(), "/missing"); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("Open error = %v, want storage.ErrNotFound", err)
	}
	if got, err := backend.List(context.Background(), "/missing"); err != nil || len(got) != 0 {
		t.Fatalf("List = %v, %v; want empty, nil", got, err)
	}
	if err := backend.Remove(context.Background(), "/missing"); err != nil {
		t.Fatalf("Remove missing path: %v", err)
	}
}

func TestBackendRemoveUsesCallerContextForSharedClient(t *testing.T) {
	client := newFakeClient()
	client.files["/tables/1.rf"] = []byte("old")
	client.removeContextHook = func(ctx context.Context, name string) error {
		if name == "/tables/1.rf" {
			<-ctx.Done()
			return ctx.Err()
		}
		return nil
	}
	backend, err := New("nn:8020", WithClient(client))
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Millisecond)
	defer cancel()

	start := time.Now()
	err = backend.Remove(ctx, "/tables/1.rf")
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Remove error = %v, want context deadline exceeded", err)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("Remove took %v; caller context did not bound shared-client removal", elapsed)
	}
	if client.lastRemoveDeadline.IsZero() {
		t.Fatal("Remove did not pass a context deadline to RemoveContext")
	}
	if !slices.Contains(client.removeContextCalls, "/tables/1.rf") {
		t.Fatalf("RemoveContext calls = %v, want /tables/1.rf", client.removeContextCalls)
	}
	if got := string(client.files["/tables/1.rf"]); got != "old" {
		t.Fatalf("target contents = %q, want unchanged file after timed-out removal", got)
	}
}

func TestBackendRejectsDifferentAuthority(t *testing.T) {
	backend, err := New("nn1:8020", WithClient(newFakeClient()))
	if err != nil {
		t.Fatal(err)
	}
	_, err = backend.Open(context.Background(), "hdfs://nn2:8020/tables/1.rf")
	if err == nil {
		t.Fatal("Open accepted a path for a different namenode")
	}
}

func TestBackendAcceptsAuthoritylessHDFSPaths(t *testing.T) {
	client := newFakeClient()
	client.files["/tables/1.rf"] = []byte("rfile")
	backend, err := New("nn:8020", WithClient(client))
	if err != nil {
		t.Fatal(err)
	}

	for _, objectPath := range []string{
		"hdfs:/tables/1.rf",
		"hdfs:///tables/1.rf",
		"HDFS:/tables/1.rf",
		"HDFS:///tables/1.rf",
		"HDFS://nn:8020/tables/1.rf",
	} {
		f, err := backend.Open(context.Background(), objectPath)
		if err != nil {
			t.Fatalf("Open(%q): %v", objectPath, err)
		}
		_ = f.Close()
	}
}

func TestBackendResolveCanonicalizesCaseInsensitiveHDFSPaths(t *testing.T) {
	backend, err := New("nn:8020", WithClient(newFakeClient()))
	if err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		path          string
		wantResolved  string
		wantQualifier string
	}{
		{path: "HDFS:/tables/1.rf", wantResolved: "/tables/1.rf", wantQualifier: "hdfs:"},
		{path: "HDFS:///tables/1.rf", wantResolved: "/tables/1.rf", wantQualifier: "hdfs:"},
		{path: "HDFS://nn:8020/tables/1.rf", wantResolved: "/tables/1.rf", wantQualifier: "hdfs://nn:8020"},
	}
	for _, tt := range tests {
		t.Run(tt.path, func(t *testing.T) {
			resolved, qualifier, err := backend.resolve(tt.path)
			if err != nil {
				t.Fatalf("resolve(%q): %v", tt.path, err)
			}
			if resolved != tt.wantResolved || qualifier != tt.wantQualifier {
				t.Fatalf("resolve(%q) = (%q, %q), want (%q, %q)", tt.path, resolved, qualifier, tt.wantResolved, tt.wantQualifier)
			}
		})
	}
}

func TestBackendCreateAcceptsCaseInsensitiveHDFSPaths(t *testing.T) {
	tests := []struct {
		path string
		want string
	}{
		{path: "HDFS:/tables/1.rf", want: "/tables/1.rf"},
		{path: "HDFS://nn:8020/tables/2.rf", want: "/tables/2.rf"},
	}
	for _, tt := range tests {
		t.Run(tt.path, func(t *testing.T) {
			client := newFakeClient()
			backend, err := New("nn:8020", WithClient(client))
			if err != nil {
				t.Fatal(err)
			}

			w, err := backend.Create(context.Background(), tt.path)
			if err != nil {
				t.Fatalf("Create(%q): %v", tt.path, err)
			}
			if _, err := w.Write([]byte("new")); err != nil {
				t.Fatal(err)
			}
			if err := w.Close(); err != nil {
				t.Fatal(err)
			}
			if got := string(client.files[tt.want]); got != "new" {
				t.Fatalf("created contents at %q = %q, want new", tt.want, got)
			}
		})
	}
}

func TestBackendRejectsOpaqueHDFSPath(t *testing.T) {
	backend, err := New("nn:8020", WithClient(newFakeClient()))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := backend.Open(context.Background(), "hdfs:tables/1.rf"); err == nil {
		t.Fatal("Open accepted an opaque HDFS URI")
	}
}

func TestBackendRejectsQualifiedPathWithoutConfiguredNamenode(t *testing.T) {
	client := newFakeClient()
	client.files["/tables/1.rf"] = []byte("rfile")
	backend, err := New("", WithClient(client))
	if err != nil {
		t.Fatal(err)
	}

	if _, err := backend.List(context.Background(), "hdfs://configured-nn:8020/tables"); err == nil {
		t.Fatal("List accepted a qualified path without a configured namenode")
	}
}

func TestAddressFromPath(t *testing.T) {
	valid := []struct {
		path string
		want string
	}{
		{path: "hdfs://nn:8020/tables/1.rf", want: "nn:8020"},
		{path: "hdfs:/tables/1.rf", want: ""},
		{path: "hdfs:///tables/1.rf", want: ""},
		{path: "HDFS://nn:8020/tables/1.rf", want: "nn:8020"},
		{path: "HDFS:/tables/1.rf", want: ""},
		{path: "HDFS:///tables/1.rf", want: ""},
	}
	for _, test := range valid {
		got, err := AddressFromPath(test.path)
		if err != nil {
			t.Fatalf("AddressFromPath(%q): %v", test.path, err)
		}
		if got != test.want {
			t.Fatalf("AddressFromPath(%q) = %q, want %q", test.path, got, test.want)
		}
	}

	invalid := []string{
		"https://nn:8020/tables/1.rf",
		"hdfs:tables/1.rf",
		"hdfs://nn:8020/tables/1.rf?version=1",
		"hdfs://nn:8020/tables/1.rf#fragment",
	}
	for _, objectPath := range invalid {
		if _, err := AddressFromPath(objectPath); err == nil {
			t.Fatalf("AddressFromPath(%q) succeeded, want error", objectPath)
		}
	}
}

func TestNewAcceptsUppercaseHDFSAddress(t *testing.T) {
	backend, err := New("HDFS://nn:8020", WithClient(newFakeClient()))
	if err != nil {
		t.Fatalf("New(HDFS://nn:8020): %v", err)
	}
	if !strings.EqualFold(backend.Authority(), "nn:8020") {
		t.Fatalf("Authority() = %q, want nn:8020", backend.Authority())
	}
}

func TestFileSerializesConcurrentReadAt(t *testing.T) {
	reader := &overlapReader{}
	f := &file{reader: reader, size: 1}

	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			buf := make([]byte, 1)
			if _, err := f.ReadAt(buf, 0); err != nil {
				t.Errorf("ReadAt: %v", err)
			}
		}()
	}
	wg.Wait()

	if reader.overlapped.Load() {
		t.Fatal("underlying HDFS reader received concurrent ReadAt calls")
	}
}

func TestNewContextRejectsCanceledContextBeforeDial(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	dialed := false
	_, err := NewContext(ctx, "nn:8020", WithClientOptions(hdfsclient.ClientOptions{
		User: "shoal-test",
		NamenodeDialFunc: func(dialCtx context.Context, network, addr string) (net.Conn, error) {
			dialed = true
			return nil, dialCtx.Err()
		},
	}))
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("NewContext error = %v, want context.Canceled", err)
	}
	if dialed {
		t.Fatal("NewContext dialed after construction context was canceled")
	}
}

func TestNewContextKeepsCleanupClientAliveUntilBackendClose(t *testing.T) {
	constructorCtx, cancel := context.WithCancel(context.Background())
	var cleanupCtx context.Context
	client := newFakeClient()
	backend, err := NewContext(constructorCtx, "nn:8020", func(c *config) {
		c.clientLeaseFactory = func(ctx context.Context) (*leasedClient, error) {
			if cleanupCtx == nil {
				cleanupCtx = ctx
			}
			return newSharedLease(client), nil
		}
	})
	if err != nil {
		t.Fatal(err)
	}

	cancel()
	select {
	case <-cleanupCtx.Done():
		t.Fatal("cleanup client context ended with constructor context")
	default:
	}
	if err := backend.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-cleanupCtx.Done():
	case <-time.After(time.Second):
		t.Fatal("Backend.Close did not cancel cleanup client context")
	}
}

func TestNewContextOperationClientReadStopsOnEitherContext(t *testing.T) {
	for _, tt := range []struct {
		name   string
		cancel func(context.CancelFunc, context.CancelFunc)
	}{
		{
			name: "operation",
			cancel: func(_ context.CancelFunc, cancelOp context.CancelFunc) {
				cancelOp()
			},
		},
		{
			name: "backend",
			cancel: func(cancelBackend context.CancelFunc, _ context.CancelFunc) {
				cancelBackend()
			},
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			constructorCtx, cancelConstructor := context.WithCancel(context.Background())
			opCtx, cancelOperation := context.WithCancel(context.Background())
			defer cancelConstructor()
			defer cancelOperation()

			leaseCalls := 0
			var cleanupCtx context.Context
			backend, err := NewContext(constructorCtx, "nn:8020", func(c *config) {
				c.clientLeaseFactory = func(ctx context.Context) (*leasedClient, error) {
					leaseCalls++
					if leaseCalls == 1 {
						cleanupCtx = ctx
						return &leasedClient{client: newFakeClient(), release: func() error { return nil }}, nil
					}
					return &leasedClient{client: &contextReadClient{ctx: ctx}, release: func() error { return nil }}, nil
				}
			})
			if err != nil {
				t.Fatal(err)
			}
			defer backend.Close()

			f, err := backend.Open(opCtx, "/tables/1.rf")
			if err != nil {
				t.Fatal(err)
			}
			defer f.Close()

			done := make(chan error, 1)
			go func() {
				_, err := f.ReadAt(make([]byte, 1), 0)
				done <- err
			}()

			tt.cancel(cancelConstructor, cancelOperation)

			select {
			case err := <-done:
				if !errors.Is(err, context.Canceled) {
					t.Fatalf("ReadAt error = %v, want context.Canceled", err)
				}
			case <-time.After(time.Second):
				t.Fatal("ReadAt did not stop after context cancellation")
			}

			if tt.name == "backend" {
				select {
				case <-cleanupCtx.Done():
					t.Fatal("cleanup client context ended with backend lifetime cancellation")
				default:
				}
			}
		})
	}
}

func TestDialContextSourceStoresMixedContextTypes(t *testing.T) {
	source := newDialContextSource(nil)
	if got := source.Context(); got == nil {
		t.Fatal("Context returned nil for default background context")
	}

	backgroundCtx := context.Background()
	source.Store(backgroundCtx)
	if got := source.Context(); got == nil || got.Err() != nil {
		t.Fatalf("background Context = %v, want non-nil uncanceled context", got)
	}

	cancelCtx, cancel := context.WithCancel(context.Background())
	source.Store(cancelCtx)
	cancel()
	if !errors.Is(source.Context().Err(), context.Canceled) {
		t.Fatalf("cancel Context err = %v, want context.Canceled", source.Context().Err())
	}

	timeoutCtx, timeoutCancel := context.WithTimeout(context.Background(), time.Minute)
	defer timeoutCancel()
	source.Store(timeoutCtx)
	if deadline, ok := source.Context().Deadline(); !ok || deadline.IsZero() {
		t.Fatalf("timeout Context deadline = %v, %v; want non-zero deadline", deadline, ok)
	}

	source.Store(nil)
	if got := source.Context(); got == nil || got.Err() != nil {
		t.Fatalf("nil Store Context = %v, want background context", got)
	}
}

func TestDialContextSourceConcurrentLoadStore(t *testing.T) {
	source := newDialContextSource(context.Background())
	cancelCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	timeoutCtx, timeoutCancel := context.WithTimeout(context.Background(), time.Minute)
	defer timeoutCancel()

	contexts := []context.Context{context.Background(), cancelCtx, timeoutCtx, nil}

	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := range 4 {
		wg.Add(1)
		go func(offset int) {
			defer wg.Done()
			<-start
			for j := range 1000 {
				source.Store(contexts[(offset+j)%len(contexts)])
			}
		}(i)
	}
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			for range 1000 {
				if source.Context() == nil {
					t.Error("Context returned nil during concurrent load/store")
					return
				}
			}
		}()
	}
	close(start)
	wg.Wait()
}

func TestNewContextCancelsStalledInitialCleanupDial(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	started := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		_, err := NewContext(ctx, "nn:8020", WithClientOptions(hdfsclient.ClientOptions{
			User: "shoal-test",
			NamenodeDialFunc: func(dialCtx context.Context, network, addr string) (net.Conn, error) {
				close(started)
				<-dialCtx.Done()
				return nil, dialCtx.Err()
			},
		}))
		done <- err
	}()

	<-started
	cancel()

	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("NewContext error = %v, want context.Canceled", err)
		}
	case <-time.After(time.Second):
		t.Fatal("NewContext did not unblock after constructor cancellation")
	}
}

func TestBindDialContextBoundsBlockedHandshakeOnDeadline(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Millisecond)
	defer cancel()

	local, remote := net.Pipe()
	defer remote.Close()

	dial := bindDialContext(ctx, func(context.Context, string, string) (net.Conn, error) {
		return local, nil
	})
	conn, err := dial(context.Background(), "tcp", "dn:9866")
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	start := time.Now()
	_, err = conn.Read(make([]byte, 1))
	if err == nil {
		t.Fatal("blocked handshake read succeeded, want deadline failure")
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("blocked handshake read took %v; want bounded deadline failure", elapsed)
	}
}

func TestBindDialContextInterruptsBlockedHandshakeOnCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	local, remote := net.Pipe()
	defer remote.Close()

	dial := bindDialContext(ctx, func(context.Context, string, string) (net.Conn, error) {
		return local, nil
	})
	conn, err := dial(context.Background(), "tcp", "dn:9866")
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	done := make(chan error, 1)
	go func() {
		_, err := conn.Read(make([]byte, 1))
		done <- err
	}()

	cancel()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("blocked handshake read succeeded after cancel, want failure")
		}
	case <-time.After(time.Second):
		t.Fatal("blocked handshake read did not unblock after cancel")
	}
}

func TestBindDialContextReconnectUsesRequestDeadline(t *testing.T) {
	boundCtx, boundCancel := context.WithCancel(context.Background())
	defer boundCancel()

	dial := bindDialContext(boundCtx, func(ctx context.Context, network, addr string) (net.Conn, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	})

	requestCtx, cancel := context.WithTimeout(context.Background(), 25*time.Millisecond)
	defer cancel()

	start := time.Now()
	_, err := dial(requestCtx, "tcp", "nn:8020")
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("dial error = %v, want context deadline exceeded", err)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("dial took %v; request deadline did not bound reconnect", elapsed)
	}
}

func TestDialContextSourceRebindsEstablishedConnection(t *testing.T) {
	constructorCtx, cancelConstructor := context.WithCancel(context.Background())
	cleanupCtx, cancelCleanup := context.WithCancel(context.Background())
	defer cancelConstructor()
	defer cancelCleanup()

	source := newDialContextSource(constructorCtx)
	local, remote := net.Pipe()
	defer remote.Close()

	dial := bindDialContextWithDialContextSource(source, func(context.Context, string, string) (net.Conn, error) {
		return local, nil
	})
	conn, err := dial(context.Background(), "tcp", "nn:8020")
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	source.Store(cleanupCtx)
	done := make(chan error, 1)
	go func() {
		_, err := conn.Read(make([]byte, 1))
		done <- err
	}()

	cancelConstructor()
	select {
	case err := <-done:
		t.Fatalf("constructor cancellation closed the rebound connection: %v", err)
	case <-time.After(50 * time.Millisecond):
	}

	cancelCleanup()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("cleanup cancellation left the rebound connection readable")
		}
	case <-time.After(time.Second):
		t.Fatal("cleanup cancellation did not close the rebound connection")
	}
}

func TestDialContextSourceRebindClearsEstablishedDeadline(t *testing.T) {
	constructorCtx, cancelConstructor := context.WithDeadline(context.Background(), time.Now().Add(time.Minute))
	defer cancelConstructor()
	cleanupCtx, cancelCleanup := context.WithCancel(context.Background())
	defer cancelCleanup()

	source := newDialContextSource(constructorCtx)
	conn := &recordingConn{}
	dial := bindDialContextWithDialContextSource(source, func(context.Context, string, string) (net.Conn, error) {
		return conn, nil
	})
	bound, err := dial(context.Background(), "tcp", "nn:8020")
	if err != nil {
		t.Fatal(err)
	}
	defer bound.Close()

	if got := conn.lastDeadline(); got.IsZero() {
		t.Fatal("initial dial did not apply constructor deadline")
	}

	source.Store(cleanupCtx)
	if got := conn.lastDeadline(); !got.IsZero() {
		t.Fatalf("rebound deadline = %v, want cleared zero deadline", got)
	}
}

func TestDialContextSourceRebindRecomputesEarliestDeadline(t *testing.T) {
	requestDeadline := time.Now().Add(2 * time.Minute)
	requestCtx, cancelRequest := context.WithDeadline(context.Background(), requestDeadline)
	defer cancelRequest()

	source := newDialContextSource(context.Background())
	conn := &recordingConn{}
	dial := bindDialContextWithDialContextSource(source, func(context.Context, string, string) (net.Conn, error) {
		return conn, nil
	})
	bound, err := dial(requestCtx, "tcp", "nn:8020")
	if err != nil {
		t.Fatal(err)
	}
	defer bound.Close()

	if got := conn.lastDeadline(); !got.Equal(requestDeadline) {
		t.Fatalf("initial deadline = %v, want request deadline %v", got, requestDeadline)
	}

	sourceDeadline := time.Now().Add(time.Minute)
	sourceCtx, cancelSource := context.WithDeadline(context.Background(), sourceDeadline)
	defer cancelSource()
	source.Store(sourceCtx)
	if got := conn.lastDeadline(); !got.Equal(sourceDeadline) {
		t.Fatalf("updated deadline = %v, want rebound source deadline %v", got, sourceDeadline)
	}

	source.Store(context.Background())
	if got := conn.lastDeadline(); !got.Equal(requestDeadline) {
		t.Fatalf("restored deadline = %v, want request deadline %v", got, requestDeadline)
	}
}

func TestBackendOpenAppliesContextDeadline(t *testing.T) {
	client := newFakeClient()
	client.files["/tables/1.rf"] = []byte("rfile")
	backend, err := New("nn:8020", WithClient(client))
	if err != nil {
		t.Fatal(err)
	}

	deadline := time.Now().Add(time.Minute)
	ctx, cancel := context.WithDeadline(context.Background(), deadline)
	defer cancel()

	f, err := backend.Open(ctx, "/tables/1.rf")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	if client.lastReader == nil {
		t.Fatal("Open did not record a reader")
	}
	if !client.lastReader.deadline.Equal(deadline) {
		t.Fatalf("reader deadline = %v, want %v", client.lastReader.deadline, deadline)
	}
}

func TestBackendCreateAppliesContextDeadline(t *testing.T) {
	client := newFakeClient()
	backend, err := New("nn:8020", WithClient(client))
	if err != nil {
		t.Fatal(err)
	}

	deadline := time.Now().Add(time.Minute)
	ctx, cancel := context.WithDeadline(context.Background(), deadline)
	defer cancel()

	w, err := backend.Create(ctx, "/tables/1.rf")
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	if client.lastWriter == nil {
		t.Fatal("Create did not record a writer")
	}
	if !client.lastWriter.deadline.Equal(deadline) {
		t.Fatalf("writer deadline = %v, want %v", client.lastWriter.deadline, deadline)
	}
}

func TestBackendCreateStopsReplicationRetryOnContextDeadline(t *testing.T) {
	client := newFakeClient()
	client.replicatingCloseFailures = 1000
	backend, err := New("nn:8020", WithClient(client))
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	w, err := backend.Create(ctx, "/tables/1.rf")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write([]byte("new")); err != nil {
		t.Fatal(err)
	}
	err = w.Close()
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Close error = %v, want context deadline exceeded", err)
	}
	if client.writerCloseCalls == 0 {
		t.Fatal("Close never retried the temporary file writer")
	}
}

func TestBackendCloseJoinsCustomErrorAfterContextDeadlineWithoutPublish(t *testing.T) {
	client := newFakeClient()
	client.files["/tables/1.rf"] = []byte("old")
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Millisecond)
	defer cancel()
	customTimeout := errors.New("writer-specific timeout")
	client.writerCloseHook = func() error {
		<-ctx.Done()
		return customTimeout
	}
	backend, err := New("nn:8020", WithClient(client))
	if err != nil {
		t.Fatal(err)
	}

	err = storage.WriteAll(ctx, backend, "/tables/1.rf", []byte("new"))
	if !errors.Is(err, context.DeadlineExceeded) || !errors.Is(err, customTimeout) {
		t.Fatalf("WriteAll error = %v, want deadline and custom timeout", err)
	}
	if got := string(client.files["/tables/1.rf"]); got != "old" {
		t.Fatalf("target contents = %q, want old", got)
	}
	if len(client.renameCalls) != 0 {
		t.Fatalf("Rename calls = %v, want no publish", client.renameCalls)
	}
	if _, ok := client.files[client.lastCreatePath]; ok {
		t.Fatalf("temporary file %s remains after abort", client.lastCreatePath)
	}
}

func TestBackendCloseReturningNilAfterCancellationDoesNotPublish(t *testing.T) {
	client := newFakeClient()
	client.files["/tables/1.rf"] = []byte("old")
	var releaseCalls atomic.Int32
	ctx, cancel := context.WithCancel(context.Background())
	client.writerCloseHook = func() error {
		cancel()
		return nil
	}
	backend, err := NewContext(context.Background(), "nn:8020",
		WithClient(client),
		func(c *config) {
			c.clientLeaseFactory = func(context.Context) (*leasedClient, error) {
				return &leasedClient{
					client: client,
					release: func() error {
						releaseCalls.Add(1)
						return nil
					},
				}, nil
			}
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	defer backend.Close()

	err = storage.WriteAll(ctx, backend, "/tables/1.rf", []byte("new"))
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("WriteAll error = %v, want context.Canceled", err)
	}
	if client.writerCloseCalls != 1 {
		t.Fatalf("writer Close calls = %d, want 1", client.writerCloseCalls)
	}
	if got := releaseCalls.Load(); got != 1 {
		t.Fatalf("operation lease release calls = %d, want 1", got)
	}
	if got := string(client.files["/tables/1.rf"]); got != "old" {
		t.Fatalf("target contents = %q, want old", got)
	}
	if len(client.renameCalls) != 0 {
		t.Fatalf("Rename calls = %v, want no publish", client.renameCalls)
	}
	if _, ok := client.files[client.lastCreatePath]; ok {
		t.Fatalf("temporary file %s remains after abort", client.lastCreatePath)
	}
}

func TestBackendCloseBoundsStalledCompleteAndCleansUpTemp(t *testing.T) {
	cleanupClient := newFakeClient()
	state := &stalledCompleteState{cleanup: cleanupClient}
	backend, err := NewContext(context.Background(), "nn:8020",
		WithClient(cleanupClient),
		func(c *config) {
			c.clientLeaseFactory = func(ctx context.Context) (*leasedClient, error) {
				return &leasedClient{
					client:  &stalledCompleteClient{ctx: ctx, state: state},
					release: func() error { return nil },
				}, nil
			}
		},
	)
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Millisecond)
	defer cancel()

	w, err := backend.Create(ctx, "/tables/1.rf")
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	err = w.Close()
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Close error = %v, want context deadline exceeded", err)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("Close took %v; want bounded stalled complete failure", elapsed)
	}
	if state.writerCloseCalls != 1 {
		t.Fatalf("writer Close calls = %d, want 1", state.writerCloseCalls)
	}
	if !slices.Contains(cleanupClient.removeCalls, state.lastCreatePath) {
		t.Fatalf("cleanup Remove calls = %v, want %s", cleanupClient.removeCalls, state.lastCreatePath)
	}
	if _, ok := cleanupClient.files[state.lastCreatePath]; ok {
		t.Fatalf("temporary file %s remains after Close cleanup", state.lastCreatePath)
	}
	if len(cleanupClient.renameCalls) != 0 {
		t.Fatalf("cleanup Rename calls = %v, want none", cleanupClient.renameCalls)
	}
}

func TestBackendAbortBoundsStalledCompleteAndCleansUpTemp(t *testing.T) {
	cleanupClient := newFakeClient()
	state := &stalledCompleteState{cleanup: cleanupClient}
	backend, err := NewContext(context.Background(), "nn:8020",
		WithClient(cleanupClient),
		func(c *config) {
			c.clientLeaseFactory = func(ctx context.Context) (*leasedClient, error) {
				return &leasedClient{
					client:  &stalledCompleteClient{ctx: ctx, state: state},
					release: func() error { return nil },
				}, nil
			}
		},
	)
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Millisecond)
	defer cancel()

	w, err := backend.Create(ctx, "/tables/1.rf")
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	err = w.(storage.Aborter).Abort()
	// The invariant is the bound below: Abort gives up on a stalled complete
	// instead of blocking on it. Which context error carries that up is a race
	// the test must not adjudicate — Abort cancels its own work to stop the
	// stall, and the caller's deadline is running at the same time, so either
	// can be the one observed. Asserting DeadlineExceeded specifically made a
	// real invariant depend on which of the two won inside a 25ms budget, and
	// it lost that race on CI (#411) while passing hundreds of local runs.
	//
	// What must hold is that Abort failed because the caller's context ended,
	// by whichever route.
	if !errors.Is(err, context.DeadlineExceeded) && !errors.Is(err, context.Canceled) {
		t.Fatalf("Abort error = %v, want the caller's context to have ended", err)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("Abort took %v; want bounded stalled complete failure", elapsed)
	}
	if state.writerCloseCalls != 1 {
		t.Fatalf("writer Close calls = %d, want 1", state.writerCloseCalls)
	}
	if !slices.Contains(cleanupClient.removeCalls, state.lastCreatePath) {
		t.Fatalf("cleanup Remove calls = %v, want %s", cleanupClient.removeCalls, state.lastCreatePath)
	}
	if _, ok := cleanupClient.files[state.lastCreatePath]; ok {
		t.Fatalf("temporary file %s remains after Abort cleanup", state.lastCreatePath)
	}
	if len(cleanupClient.renameCalls) != 0 {
		t.Fatalf("cleanup Rename calls = %v, want none", cleanupClient.renameCalls)
	}
}

func TestCloseAfterReplicationSuccessfulCloseUnaffected(t *testing.T) {
	client := newFakeClient()
	writer := &fakeWriter{client: client}
	closed, err := closeAfterReplication(context.Background(), writer)
	if err != nil {
		t.Fatal(err)
	}
	if !closed {
		t.Fatal("closeAfterReplication reported writer open after successful close")
	}
	if client.writerCloseCalls != 1 {
		t.Fatalf("Close calls = %d, want 1", client.writerCloseCalls)
	}
}

func TestCloseAfterReplicationReportsClosedWhenContextCanceledAfterClose(t *testing.T) {
	client := newFakeClient()
	ctx, cancel := context.WithCancel(context.Background())
	writer := &fakeWriter{
		client: client,
		closeHook: func() error {
			cancel()
			return nil
		},
	}

	closed, err := closeAfterReplication(ctx, writer)
	if !closed {
		t.Fatal("closeAfterReplication did not report successful underlying close")
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("closeAfterReplication error = %v, want context.Canceled", err)
	}
	if client.writerCloseCalls != 1 {
		t.Fatalf("Close calls = %d, want 1", client.writerCloseCalls)
	}
}

func TestCloseAfterReplicationAppliesCallerDeadline(t *testing.T) {
	writer := &deadlineBlockingWriter{}
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Millisecond)
	defer cancel()
	callerDeadline, _ := ctx.Deadline()

	start := time.Now()
	closed, err := closeAfterReplication(ctx, writer)
	if closed {
		t.Fatal("closeAfterReplication reported writer closed on deadline failure")
	}
	if !errors.Is(err, os.ErrDeadlineExceeded) {
		t.Fatalf("closeAfterReplication error = %v, want deadline exceeded", err)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("closeAfterReplication took %v; blocked Close was not interrupted", elapsed)
	}
	if len(writer.deadlines) != 1 {
		t.Fatalf("SetDeadline calls = %v, want the caller deadline only", writer.deadlines)
	}
	if !writer.deadlines[0].Equal(callerDeadline) {
		t.Fatalf("applied deadline = %v, want caller deadline %v", writer.deadlines[0], callerDeadline)
	}
}

func TestCloseAfterReplicationDoesNotShortenGenerousCallerDeadline(t *testing.T) {
	writer := &immediateErrorDeadlineWriter{}
	ctx, cancel := context.WithTimeout(context.Background(), 10*replicationRetryBudget)
	defer cancel()
	callerDeadline, _ := ctx.Deadline()

	closed, err := closeAfterReplication(ctx, writer)
	if closed {
		t.Fatal("closeAfterReplication reported writer closed on injected close failure")
	}
	if !errors.Is(err, errInjectedClose) {
		t.Fatalf("closeAfterReplication error = %v, want %v", err, errInjectedClose)
	}
	if len(writer.deadlines) != 1 {
		t.Fatalf("SetDeadline calls = %v, want the caller deadline only", writer.deadlines)
	}
	if !writer.deadlines[0].Equal(callerDeadline) {
		t.Fatalf("applied deadline = %v, want caller deadline %v; commit must not inherit the retry budget", writer.deadlines[0], callerDeadline)
	}
}

func TestCloseAfterReplicationClearsDeadlineForBackgroundContext(t *testing.T) {
	writer := &immediateErrorDeadlineWriter{}
	closed, err := closeAfterReplication(context.Background(), writer)
	if closed {
		t.Fatal("closeAfterReplication reported writer closed on injected close failure")
	}
	if !errors.Is(err, errInjectedClose) {
		t.Fatalf("closeAfterReplication error = %v, want %v", err, errInjectedClose)
	}
	if len(writer.deadlines) != 1 {
		t.Fatalf("SetDeadline calls = %v, want a single clearing call", writer.deadlines)
	}
	if !writer.deadlines[0].IsZero() {
		t.Fatalf("deadlines = %v, want a cleared deadline", writer.deadlines)
	}
}

func TestCloseAfterReplicationUsesContextAwareClose(t *testing.T) {
	writer := &contextCloseWriter{}
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Millisecond)
	defer cancel()

	start := time.Now()
	closed, err := closeAfterReplication(ctx, writer)
	if closed {
		t.Fatal("closeAfterReplication reported writer closed after context-aware failure")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("closeAfterReplication error = %v, want context deadline exceeded", err)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("closeAfterReplication took %v; context-aware Close was not bounded", elapsed)
	}
	if writer.contextCloseCalls != 1 {
		t.Fatalf("CloseContext calls = %d, want 1", writer.contextCloseCalls)
	}
	if writer.closeCalls != 0 {
		t.Fatalf("Close calls = %d, want 0", writer.closeCalls)
	}
}

func TestCleanupWriterAfterDeadlineFailureJoinsCleanupErrors(t *testing.T) {
	removeErr := errors.New("remove failed")
	err := cleanupWriterAfterDeadlineFailure(
		"/tables/1.rf",
		&cleanupFailureWriter{closeErr: errInjectedClose},
		func(context.Context) error {
			return fmt.Errorf("hdfs: remove temporary file /tables/1.rf: %w", removeErr)
		},
		func() error { return errInjectedRelease },
	)
	if !errors.Is(err, errInjectedClose) {
		t.Fatalf("cleanupWriterAfterDeadlineFailure missing close error: %v", err)
	}
	if !errors.Is(err, removeErr) {
		t.Fatalf("cleanupWriterAfterDeadlineFailure missing remove error: %v", err)
	}
	if !errors.Is(err, errInjectedRelease) {
		t.Fatalf("cleanupWriterAfterDeadlineFailure missing release error: %v", err)
	}
}

func TestCleanupReaderAfterDeadlineFailureJoinsCleanupErrors(t *testing.T) {
	closeErr := errors.New("reader close failed")
	err := cleanupReaderAfterDeadlineFailure(
		"/tables/1.rf",
		&cleanupFailureReader{closeErr: closeErr},
		func() error { return errInjectedRelease },
	)
	if !errors.Is(err, closeErr) {
		t.Fatalf("cleanupReaderAfterDeadlineFailure missing close error: %v", err)
	}
	if !errors.Is(err, errInjectedRelease) {
		t.Fatalf("cleanupReaderAfterDeadlineFailure missing release error: %v", err)
	}
}

func TestFileMatchesAppliesCleanupDeadlineBeforeAndDuringReads(t *testing.T) {
	reader := &perReadDeadlineReader{
		data: []byte("ab"),
		info: fakeInfo{name: "1.rf", size: 2},
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	sum := sha256.Sum256(reader.data)

	matches, err := fileMatches(ctx, &openReaderClient{reader: reader}, "/tables/1.rf", int64(len(reader.data)), sum[:])
	if err != nil {
		t.Fatalf("fileMatches: %v", err)
	}
	if !matches {
		t.Fatal("fileMatches returned false, want true")
	}
	if got, want := len(reader.deadlines), 3; got != want {
		t.Fatalf("SetDeadline calls = %d, want %d (open + each read)", got, want)
	}
	for i, deadline := range reader.deadlines {
		if deadline.IsZero() {
			t.Fatalf("deadline %d = zero, want cleanup deadline", i)
		}
	}
}

func TestFileMatchesBlockedReadUsesCleanupDeadline(t *testing.T) {
	reader := &deadlineBlockingReadReader{info: fakeInfo{name: "1.rf", size: 1}}
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Millisecond)
	defer cancel()
	sum := sha256.Sum256([]byte("x"))

	start := time.Now()
	matches, err := fileMatches(ctx, &openReaderClient{reader: reader}, "/tables/1.rf", 1, sum[:])
	if matches {
		t.Fatal("fileMatches succeeded, want bounded read failure")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("fileMatches error = %v, want context deadline exceeded", err)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("fileMatches took %v; cleanup deadline did not bound ReadAt", elapsed)
	}
	if len(reader.deadlines) < 2 {
		t.Fatalf("SetDeadline calls = %d, want open + read deadline application", len(reader.deadlines))
	}
}

func TestBackendAbortPreservesExistingTargetAndRemovesTemp(t *testing.T) {
	client := newFakeClient()
	client.files["/tables/1.rf"] = []byte("old")
	backend, err := New("nn:8020", WithClient(client))
	if err != nil {
		t.Fatal(err)
	}

	w, err := backend.Create(context.Background(), "/tables/1.rf")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write([]byte("new")); err != nil {
		t.Fatal(err)
	}
	aborter, ok := w.(storage.Aborter)
	if !ok {
		t.Fatal("Create writer does not implement storage.Aborter")
	}
	if err := aborter.Abort(); err != nil {
		t.Fatal(err)
	}
	if err := aborter.Abort(); err != nil {
		t.Fatalf("second Abort: %v", err)
	}
	if err := w.Close(); err == nil || !strings.Contains(err.Error(), "writer already aborted") {
		t.Fatalf("Close after Abort error = %v, want writer already aborted", err)
	}

	if got := string(client.files["/tables/1.rf"]); got != "old" {
		t.Fatalf("existing contents = %q, want old", got)
	}
	if client.lastCreatePath == "" {
		t.Fatal("Create did not record the temporary path")
	}
	if _, ok := client.files[client.lastCreatePath]; ok {
		t.Fatalf("temporary file %s still exists after abort", client.lastCreatePath)
	}
	if client.writerCloseCalls != 1 {
		t.Fatalf("writer Close calls = %d, want 1", client.writerCloseCalls)
	}
	if !slices.Contains(client.removeCalls, client.lastCreatePath) {
		t.Fatalf("Remove calls = %v, want %s", client.removeCalls, client.lastCreatePath)
	}
	if len(client.renameCalls) != 0 {
		t.Fatalf("Abort must not rename files, got %v", client.renameCalls)
	}
}

func TestBackendAbortReportsCleanupFailure(t *testing.T) {
	client := newFakeClient()
	client.files["/tables/1.rf"] = []byte("old")
	backend, err := New("nn:8020", WithClient(client))
	if err != nil {
		t.Fatal(err)
	}

	w, err := backend.Create(context.Background(), "/tables/1.rf")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write([]byte("new")); err != nil {
		t.Fatal(err)
	}
	client.failRemovePath = client.lastCreatePath
	client.failRemoveErr = errors.New("injected remove failure")

	aborter := w.(storage.Aborter)
	err = aborter.Abort()
	if err == nil {
		t.Fatal("Abort succeeded, want cleanup failure")
	}
	if !strings.Contains(err.Error(), "remove temporary file") {
		t.Fatalf("Abort error = %v, want remove failure", err)
	}
	if got := string(client.files["/tables/1.rf"]); got != "old" {
		t.Fatalf("existing contents = %q, want old", got)
	}
	if len(client.renameCalls) != 0 {
		t.Fatalf("Abort must not rename files, got %v", client.renameCalls)
	}
}

func TestBackendAbortUsesBoundedRemoveContext(t *testing.T) {
	client := newFakeClient()
	backend, err := New("nn:8020", WithClient(client))
	if err != nil {
		t.Fatal(err)
	}
	w, err := backend.Create(context.Background(), "/tables/1.rf")
	if err != nil {
		t.Fatal(err)
	}

	if err := w.(storage.Aborter).Abort(); err != nil {
		t.Fatal(err)
	}
	if client.lastRemoveDeadline.IsZero() {
		t.Fatal("Abort removed the temporary file without a deadline")
	}
	remaining := time.Until(client.lastRemoveDeadline)
	if remaining <= 0 || remaining > cleanupTimeout {
		t.Fatalf("remove deadline remaining = %v, want within %v", remaining, cleanupTimeout)
	}
}

func TestBackendCleanupClientOutlivesConstructorContext(t *testing.T) {
	constructorCtx, cancelConstructor := context.WithCancel(context.Background())
	factory := newCleanupLifetimeFactory(cancelConstructor)
	backend, err := NewContext(constructorCtx, "nn:8020", func(c *config) {
		c.clientLeaseFactory = factory.lease
	})
	if err != nil {
		t.Fatal(err)
	}
	defer backend.Close()

	w, err := backend.Create(constructorCtx, "/tables/1.rf")
	if err != nil {
		t.Fatal(err)
	}
	err = w.Close()
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Close error = %v, want context.Canceled", err)
	}
	if factory.state.cleanupClientCtxErr != nil {
		t.Fatalf("cleanup client observed canceled constructor context: %v", factory.state.cleanupClientCtxErr)
	}
	if !slices.Contains(factory.state.removeCalls, factory.state.lastCreatePath) {
		t.Fatalf("cleanup Remove calls = %v, want %s", factory.state.removeCalls, factory.state.lastCreatePath)
	}
	if _, ok := factory.state.files[factory.state.lastCreatePath]; ok {
		t.Fatalf("temporary file %s remains after cleanup", factory.state.lastCreatePath)
	}
}

func TestBackendCloseCancelsActiveReadAndRejectsNewOperations(t *testing.T) {
	cleanupClient := newFakeClient()
	leaseCalls := 0
	backend, err := NewContext(context.Background(), "nn:8020", func(c *config) {
		c.clientLeaseFactory = func(ctx context.Context) (*leasedClient, error) {
			leaseCalls++
			if leaseCalls == 1 {
				return &leasedClient{client: cleanupClient, release: func() error { return nil }}, nil
			}
			return &leasedClient{client: &contextReadClient{ctx: ctx}, release: func() error { return nil }}, nil
		}
	})
	if err != nil {
		t.Fatal(err)
	}

	f, err := backend.Open(context.Background(), "/tables/1.rf")
	if err != nil {
		t.Fatal(err)
	}
	readDone := make(chan error, 1)
	go func() {
		_, err := f.ReadAt(make([]byte, 1), 0)
		readDone <- err
	}()

	closeDone := make(chan error, 1)
	go func() {
		closeDone <- backend.Close()
	}()

	select {
	case err := <-readDone:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("ReadAt error = %v, want context.Canceled", err)
		}
	case <-time.After(time.Second):
		t.Fatal("ReadAt did not unblock after Backend.Close")
	}

	select {
	case err := <-closeDone:
		if err != nil {
			t.Fatalf("Backend.Close error = %v, want nil", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Backend.Close did not return after canceling the active read")
	}

	if err := f.Close(); err != nil {
		t.Fatalf("file Close after Backend.Close: %v", err)
	}
	if _, err := backend.Open(context.Background(), "/tables/1.rf"); !errors.Is(err, errBackendClosed) {
		t.Fatalf("Open after Backend.Close error = %v, want errBackendClosed", err)
	}
	if _, err := backend.Create(context.Background(), "/tables/1.rf"); !errors.Is(err, errBackendClosed) {
		t.Fatalf("Create after Backend.Close error = %v, want errBackendClosed", err)
	}
	if _, err := backend.List(context.Background(), "/tables"); !errors.Is(err, errBackendClosed) {
		t.Fatalf("List after Backend.Close error = %v, want errBackendClosed", err)
	}
	if err := backend.Remove(context.Background(), "/tables/1.rf"); !errors.Is(err, errBackendClosed) {
		t.Fatalf("Remove after Backend.Close error = %v, want errBackendClosed", err)
	}
}

func TestBackendReadCloseDoesNotSurfaceExpectedOperationClientClose(t *testing.T) {
	backend, err := NewContext(context.Background(), "nn:8020",
		WithClient(newFakeClient()),
		func(c *config) {
			c.clientLeaseFactory = func(ctx context.Context) (*leasedClient, error) {
				client := newFakeClient()
				client.files["/tables/1.rf"] = []byte("data")
				return &leasedClient{
					client: client,
					release: func() error {
						if ctx.Err() != nil {
							return net.ErrClosed
						}
						return nil
					},
				}, nil
			}
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	defer backend.Close()

	f, err := backend.Open(context.Background(), "/tables/1.rf")
	if err != nil {
		t.Fatal(err)
	}
	got := make([]byte, 4)
	if _, err := f.ReadAt(got, 0); err != nil {
		t.Fatalf("ReadAt: %v", err)
	}
	if string(got) != "data" {
		t.Fatalf("ReadAt data = %q, want data", got)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("Close error = %v, want nil", err)
	}
}

func TestBackendReaderCloseIgnoresExpectedOperationClientCloseAfterCancellation(t *testing.T) {
	t.Run("request cancellation", func(t *testing.T) {
		backend, err := NewContext(context.Background(), "nn:8020",
			WithClient(newFakeClient()),
			func(c *config) {
				c.clientLeaseFactory = func(ctx context.Context) (*leasedClient, error) {
					client := newFakeClient()
					client.files["/tables/1.rf"] = []byte("data")
					return &leasedClient{
						client: client,
						release: func() error {
							if ctx.Err() != nil {
								return errors.New("use of closed network connection")
							}
							return nil
						},
					}, nil
				}
			},
		)
		if err != nil {
			t.Fatal(err)
		}
		defer backend.Close()

		opCtx, cancel := context.WithCancel(context.Background())
		f, err := backend.Open(opCtx, "/tables/1.rf")
		if err != nil {
			t.Fatal(err)
		}
		cancel()
		if err := f.Close(); err != nil {
			t.Fatalf("Close after cancel error = %v, want nil", err)
		}
	})

	t.Run("backend close", func(t *testing.T) {
		backend, err := NewContext(context.Background(), "nn:8020",
			WithClient(newFakeClient()),
			func(c *config) {
				c.clientLeaseFactory = func(ctx context.Context) (*leasedClient, error) {
					client := newFakeClient()
					client.files["/tables/1.rf"] = []byte("data")
					return &leasedClient{
						client: client,
						release: func() error {
							if ctx.Err() != nil {
								return errors.New("use of closed network connection")
							}
							return nil
						},
					}, nil
				}
			},
		)
		if err != nil {
			t.Fatal(err)
		}

		f, err := backend.Open(context.Background(), "/tables/1.rf")
		if err != nil {
			t.Fatal(err)
		}
		if err := backend.Close(); err != nil {
			t.Fatalf("Backend.Close error = %v, want nil", err)
		}
		if err := f.Close(); err != nil {
			t.Fatalf("file Close after Backend.Close: %v", err)
		}
	})
}

func TestReaderCloseRacingBackendCloseSuppressesForcedTransportError(t *testing.T) {
	for _, backendFirst := range []bool{false, true} {
		t.Run(fmt.Sprintf("backend-first=%t", backendFirst), func(t *testing.T) {
			reader := &controlledCloseReader{
				Reader:       bytes.NewReader([]byte("data")),
				info:         fakeInfo{name: "1.rf", size: 4},
				closeStarted: make(chan struct{}),
				allowClose:   make(chan struct{}),
				closeErr:     errors.New("close tcp 127.0.0.1:1234->127.0.0.1:8020: use of closed network connection"),
			}
			client := &controlledCloseClient{fakeClient: newFakeClient(), reader: reader}
			client.files["/tables/1.rf"] = []byte("data")
			backend, err := New("nn:8020", WithClient(client))
			if err != nil {
				t.Fatal(err)
			}
			f, err := backend.Open(context.Background(), "/tables/1.rf")
			if err != nil {
				t.Fatal(err)
			}

			readerDone := make(chan error, 1)
			backendDone := make(chan error, 1)
			if backendFirst {
				go func() { backendDone <- backend.Close() }()
			} else {
				go func() { readerDone <- f.Close() }()
			}
			<-reader.closeStarted
			if backendFirst {
				go func() { readerDone <- f.Close() }()
			} else {
				go func() { backendDone <- backend.Close() }()
			}
			close(reader.allowClose)

			if err := <-readerDone; err != nil {
				t.Fatalf("reader Close error = %v, want nil", err)
			}
			if err := <-backendDone; err != nil {
				t.Fatalf("Backend.Close error = %v, want nil", err)
			}
			if err := f.Close(); err != nil {
				t.Fatalf("second reader Close error = %v, want nil", err)
			}
		})
	}
}

func TestReaderClosePreservesGenuineErrorAndIsIdempotent(t *testing.T) {
	closeErr := errors.New("reader checksum finalization failed")
	reader := &controlledCloseReader{
		Reader:   bytes.NewReader([]byte("data")),
		info:     fakeInfo{name: "1.rf", size: 4},
		closeErr: closeErr,
	}
	client := &controlledCloseClient{fakeClient: newFakeClient(), reader: reader}
	client.files["/tables/1.rf"] = []byte("data")
	backend, err := New("nn:8020", WithClient(client))
	if err != nil {
		t.Fatal(err)
	}
	defer backend.Close()
	f, err := backend.Open(context.Background(), "/tables/1.rf")
	if err != nil {
		t.Fatal(err)
	}

	if err := f.Close(); !errors.Is(err, closeErr) {
		t.Fatalf("reader Close error = %v, want %v", err, closeErr)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("second reader Close error = %v, want nil", err)
	}
}

func TestBackendCloseFiltersOnlyExpectedBaseClientTransportErrors(t *testing.T) {
	t.Run("already closed transport", func(t *testing.T) {
		closeCalls := 0
		backend := &Backend{
			activeHandles: make(map[uint64]activeHandle),
			closeClient: func() error {
				closeCalls++
				return errors.New("close tcp 127.0.0.1:1234->127.0.0.1:8020: use of closed network connection")
			},
		}
		if err := backend.Close(); err != nil {
			t.Fatalf("Backend.Close error = %v, want nil", err)
		}
		if err := backend.Close(); err != nil {
			t.Fatalf("second Backend.Close error = %v, want nil", err)
		}
		if closeCalls != 1 {
			t.Fatalf("close client calls = %d, want 1", closeCalls)
		}
	})

	t.Run("genuine client failure", func(t *testing.T) {
		wantErr := errors.New("namenode finalization failed")
		backend := &Backend{
			activeHandles: make(map[uint64]activeHandle),
			closeClient:   func() error { return wantErr },
		}
		err := backend.Close()
		if !errors.Is(err, wantErr) {
			t.Fatalf("Backend.Close error = %v, want wrapped %v", err, wantErr)
		}
		if !strings.Contains(err.Error(), "close client") {
			t.Fatalf("Backend.Close error = %v, want close-client context", err)
		}
	})
}

func TestBackendCloseAbortsActiveWriterWithoutLeakingTemp(t *testing.T) {
	client := newFakeClient()
	backend, err := New("nn:8020", WithClient(client))
	if err != nil {
		t.Fatal(err)
	}

	w, err := backend.Create(context.Background(), "/tables/1.rf")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write([]byte("new")); err != nil {
		t.Fatal(err)
	}

	if err := backend.Close(); err != nil {
		t.Fatalf("Backend.Close error = %v, want nil", err)
	}
	if client.lastCreatePath == "" {
		t.Fatal("Create did not record the temporary path")
	}
	if _, ok := client.files[client.lastCreatePath]; ok {
		t.Fatalf("temporary file %s remains after Backend.Close", client.lastCreatePath)
	}
	if len(client.renameCalls) != 0 {
		t.Fatalf("Backend.Close should abort the active writer without publishing, got rename calls %v", client.renameCalls)
	}
	if err := w.(storage.Aborter).Abort(); err != nil {
		t.Fatalf("Abort after Backend.Close: %v", err)
	}
	if err := w.Close(); err == nil || !strings.Contains(err.Error(), "writer already aborted") {
		t.Fatalf("Close after Backend.Close error = %v, want writer already aborted", err)
	}
}

func TestBackendCloseRestoresBackupAfterShutdownCancelsPublish(t *testing.T) {
	client := newFakeClient()
	client.files["/tables/1.rf"] = []byte("old")
	publishStarted := make(chan struct{})
	backend, err := NewContext(context.Background(), "nn:8020",
		WithClient(client),
		func(c *config) {
			c.clientLeaseFactory = func(ctx context.Context) (*leasedClient, error) {
				return &leasedClient{
					client: &shutdownPublishClient{
						fakeClient:     client,
						ctx:            ctx,
						publishStarted: publishStarted,
					},
					release: func() error { return nil },
				}, nil
			}
		},
	)
	if err != nil {
		t.Fatal(err)
	}

	w, err := backend.Create(context.Background(), "/tables/1.rf")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write([]byte("new")); err != nil {
		t.Fatal(err)
	}

	closeErr := make(chan error, 1)
	go func() {
		closeErr <- w.Close()
	}()
	<-publishStarted

	backendErr := make(chan error, 1)
	go func() {
		backendErr <- backend.Close()
	}()

	if err := <-closeErr; !errors.Is(err, context.Canceled) {
		t.Fatalf("Close error = %v, want context.Canceled", err)
	}
	if err := <-backendErr; err != nil {
		t.Fatalf("Backend.Close error = %v, want nil", err)
	}
	if got := string(client.files["/tables/1.rf"]); got != "old" {
		t.Fatalf("target contents = %q, want restored old data", got)
	}
	if _, ok := client.files[client.lastCreatePath]; ok {
		t.Fatalf("temporary file %s remains after Backend.Close", client.lastCreatePath)
	}
	for name := range client.files {
		if strings.Contains(name, replacementBackupPrefix) {
			t.Fatalf("backup %s remains after shutdown cleanup", name)
		}
	}
}

func TestBackendCloseIgnoresOperationClientReleaseErrorAfterCommit(t *testing.T) {
	client := newFakeClient()
	backend := newReleaseErrorBackend(t, client)
	w, err := backend.Create(context.Background(), "/tables/1.rf")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write([]byte("new")); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("Close error = %v, want nil", err)
	}
	if got := string(client.files["/tables/1.rf"]); got != "new" {
		t.Fatalf("target contents = %q, want new", got)
	}
}

func TestBackendAbortIgnoresOperationClientReleaseErrorAfterCleanup(t *testing.T) {
	client := newFakeClient()
	backend := newReleaseErrorBackend(t, client)
	w, err := backend.Create(context.Background(), "/tables/1.rf")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write([]byte("new")); err != nil {
		t.Fatal(err)
	}
	if err := w.(storage.Aborter).Abort(); err != nil {
		t.Fatalf("Abort error = %v, want nil", err)
	}
	if _, ok := client.files[client.lastCreatePath]; ok {
		t.Fatalf("temporary file %s remains after abort", client.lastCreatePath)
	}
}

func TestBackendAbortRetriesTempRemovalAfterFailure(t *testing.T) {
	client := newFakeClient()
	backend, err := New("nn:8020", WithClient(client))
	if err != nil {
		t.Fatal(err)
	}

	w, err := backend.Create(context.Background(), "/tables/1.rf")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write([]byte("new")); err != nil {
		t.Fatal(err)
	}
	client.failRemovePath = client.lastCreatePath
	client.failRemoveErr = errors.New("transient remove failure")

	aborter := w.(storage.Aborter)
	err = aborter.Abort()
	if err == nil || !strings.Contains(err.Error(), "remove temporary file") {
		t.Fatalf("Abort error = %v, want remove temporary file failure", err)
	}
	if err := w.Close(); err == nil || !strings.Contains(err.Error(), "writer already aborted") {
		t.Fatalf("Close after failed Abort error = %v, want writer already aborted", err)
	}

	client.failRemoveErr = nil
	if err := aborter.Abort(); err != nil {
		t.Fatalf("second Abort: %v", err)
	}
	if countCalls(client.removeCalls, client.lastCreatePath) != 2 {
		t.Fatalf("Remove calls = %v, want 2 calls for %s", client.removeCalls, client.lastCreatePath)
	}
	if _, ok := client.files[client.lastCreatePath]; ok {
		t.Fatalf("temporary file %s remains after retry", client.lastCreatePath)
	}
}

func TestBackendCloseBoundsStalledRestoreBackupRename(t *testing.T) {
	setCleanupTimeoutForTest(t, 25*time.Millisecond)

	client := newFakeClient()
	client.files["/tables/1.rf"] = []byte("old")
	client.failPublish = true
	client.renameContextHook = func(ctx context.Context, oldpath, newpath string) error {
		if strings.Contains(oldpath, ".shoal-backup-") && newpath == "/tables/1.rf" {
			<-ctx.Done()
			return ctx.Err()
		}
		return nil
	}
	backend, err := New("nn:8020", WithClient(client))
	if err != nil {
		t.Fatal(err)
	}

	w, err := backend.Create(context.Background(), "/tables/1.rf")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write([]byte("new")); err != nil {
		t.Fatal(err)
	}

	start := time.Now()
	err = w.Close()
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Close error = %v, want context deadline exceeded", err)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("Close took %v; stalled restore rename was not bounded", elapsed)
	}
	if client.lastRenameDeadline.IsZero() {
		t.Fatal("restore backup rename did not receive a cleanup deadline")
	}
}

func TestBackendCloseBoundsStalledRollbackRename(t *testing.T) {
	setCleanupTimeoutForTest(t, 25*time.Millisecond)

	client := newFakeClient()
	client.files["/tables/1.rf"] = []byte("old")
	client.failBackupRemove = true
	client.renameContextHook = func(ctx context.Context, oldpath, newpath string) error {
		if oldpath == "/tables/1.rf" && strings.Contains(newpath, ".shoal-tmp-") {
			<-ctx.Done()
			return ctx.Err()
		}
		return nil
	}
	backend, err := New("nn:8020", WithClient(client))
	if err != nil {
		t.Fatal(err)
	}

	w, err := backend.Create(context.Background(), "/tables/1.rf")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write([]byte("new")); err != nil {
		t.Fatal(err)
	}

	start := time.Now()
	err = w.Close()
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Close error = %v, want context deadline exceeded", err)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("Close took %v; stalled rollback rename was not bounded", elapsed)
	}
	if client.lastRenameDeadline.IsZero() {
		t.Fatal("rollback rename did not receive a cleanup deadline")
	}
}

func TestBackendCloseUsesOperationContextForBackupRemovalRollback(t *testing.T) {
	setCleanupTimeoutForTest(t, 100*time.Millisecond)

	client := newFakeClient()
	client.files["/tables/1.rf"] = []byte("old")
	client.removeContextHook = func(ctx context.Context, name string) error {
		if strings.Contains(name, ".shoal-backup-") {
			<-ctx.Done()
			return ctx.Err()
		}
		return nil
	}
	backend, err := New("nn:8020", WithClient(client))
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Millisecond)
	defer cancel()
	w, err := backend.Create(ctx, "/tables/1.rf")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write([]byte("new")); err != nil {
		t.Fatal(err)
	}

	start := time.Now()
	err = w.Close()
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Close error = %v, want context deadline exceeded", err)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("Close took %v; backup removal was not bounded by the operation context", elapsed)
	}
	if client.lastRemoveDeadline.IsZero() {
		t.Fatal("backup removal did not receive the operation deadline")
	}
	if client.lastRenameDeadline.IsZero() {
		t.Fatal("rollback rename did not receive a cleanup deadline")
	}
	if !client.lastRenameDeadline.After(client.lastRemoveDeadline) {
		t.Fatalf("rollback deadline = %v, want later than backup remove deadline %v", client.lastRenameDeadline, client.lastRemoveDeadline)
	}
	if len(client.removeContextCalls) != 1 || !strings.Contains(client.removeContextCalls[0], ".shoal-backup-") {
		t.Fatalf("RemoveContext calls = %v, want single backup removal via context-aware delete", client.removeContextCalls)
	}
	if got := string(client.files["/tables/1.rf"]); got != "old" {
		t.Fatalf("target contents = %q, want rollback to restore old data", got)
	}
	if err := w.(storage.Aborter).Abort(); err != nil {
		t.Fatalf("Abort after bounded rollback: %v", err)
	}
}

func TestWriterCloseFailureKeepsOperationClientForRetryableAbort(t *testing.T) {
	baseClient := newFakeClient()
	opClient := newFakeClient()
	var released atomic.Bool
	var releaseCalls atomic.Int32
	closeAttempts := 0
	opClient.writerCloseHook = func() error {
		closeAttempts++
		if released.Load() {
			return errors.New("operation client already closed")
		}
		if closeAttempts == 1 {
			return errors.New("injected close failure")
		}
		return nil
	}
	backend, err := New("nn:8020",
		WithClient(baseClient),
		func(c *config) {
			c.clientLeaseFactory = func(context.Context) (*leasedClient, error) {
				return &leasedClient{
					client: opClient,
					release: func() error {
						releaseCalls.Add(1)
						released.Store(true)
						return nil
					},
				}, nil
			}
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	defer backend.Close()

	w, err := backend.Create(context.Background(), "/tables/1.rf")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write([]byte("data")); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err == nil {
		t.Fatal("Close succeeded, want injected temporary-file close failure")
	}
	if got := releaseCalls.Load(); got != 0 {
		t.Fatalf("operation release calls after failed Close = %d, want 0 so Abort can retry", got)
	}

	if err := w.(storage.Aborter).Abort(); err != nil {
		t.Fatalf("Abort after failed Close: %v", err)
	}
	if closeAttempts != 2 {
		t.Fatalf("temporary writer close attempts = %d, want 2", closeAttempts)
	}
	if got := releaseCalls.Load(); got != 1 {
		t.Fatalf("operation release calls after Abort = %d, want 1", got)
	}
	if err := w.(storage.Aborter).Abort(); err != nil {
		t.Fatalf("second Abort: %v", err)
	}
	if got := releaseCalls.Load(); got != 1 {
		t.Fatalf("operation release calls after repeated Abort = %d, want 1", got)
	}
	if _, exists := opClient.files["/tables/1.rf"]; exists {
		t.Fatal("aborted write published the destination")
	}
}

func TestBackendCloseReleasesActiveOperationClientsAndRejectsNewOperations(t *testing.T) {
	baseClient := newFakeClient()
	var factoryCalls atomic.Int32
	var releaseCalls atomic.Int32
	backend, err := New("nn:8020",
		WithClient(baseClient),
		func(c *config) {
			c.clientLeaseFactory = func(context.Context) (*leasedClient, error) {
				factoryCalls.Add(1)
				client := newFakeClient()
				client.files["/tables/1.rf"] = []byte("data")
				return &leasedClient{
					client: client,
					release: func() error {
						releaseCalls.Add(1)
						return client.Close()
					},
				}, nil
			}
		},
	)
	if err != nil {
		t.Fatal(err)
	}

	reader, err := backend.Open(context.Background(), "/tables/1.rf")
	if err != nil {
		t.Fatal(err)
	}
	writer, err := backend.Create(context.Background(), "/tables/2.rf")
	if err != nil {
		t.Fatal(err)
	}
	if got := factoryCalls.Load(); got != 2 {
		t.Fatalf("operation factory calls = %d, want 2", got)
	}

	if err := backend.Close(); err != nil {
		t.Fatal(err)
	}
	if got := releaseCalls.Load(); got != 2 {
		t.Fatalf("operation release calls after Backend.Close = %d, want 2", got)
	}
	if err := backend.Close(); err != nil {
		t.Fatalf("second Backend.Close: %v", err)
	}

	if _, err := backend.Open(context.Background(), "/tables/1.rf"); !errors.Is(err, errBackendClosed) {
		t.Fatalf("Open after Close error = %v, want errBackendClosed", err)
	}
	if _, err := backend.Create(context.Background(), "/tables/3.rf"); !errors.Is(err, errBackendClosed) {
		t.Fatalf("Create after Close error = %v, want errBackendClosed", err)
	}
	if _, err := backend.List(context.Background(), "/tables"); !errors.Is(err, errBackendClosed) {
		t.Fatalf("List after Close error = %v, want errBackendClosed", err)
	}
	if err := backend.Remove(context.Background(), "/tables/1.rf"); !errors.Is(err, errBackendClosed) {
		t.Fatalf("Remove after Close error = %v, want errBackendClosed", err)
	}
	if got := factoryCalls.Load(); got != 2 {
		t.Fatalf("post-Close operations invoked factory; calls = %d, want 2", got)
	}

	if err := reader.Close(); err != nil {
		t.Fatalf("reader Close after Backend.Close: %v", err)
	}
	if err := writer.(storage.Aborter).Abort(); err != nil {
		t.Fatalf("writer Abort after Backend.Close: %v", err)
	}
	if got := releaseCalls.Load(); got != 2 {
		t.Fatalf("resources released operation clients again; calls = %d, want 2", got)
	}
}

func TestBackendCloseWinsRaceWithOperationClientConstruction(t *testing.T) {
	baseClient := newFakeClient()
	factoryEntered := make(chan struct{})
	allowFactoryReturn := make(chan struct{})
	var releaseCalls atomic.Int32
	backend, err := New("nn:8020",
		WithClient(baseClient),
		func(c *config) {
			c.clientLeaseFactory = func(context.Context) (*leasedClient, error) {
				close(factoryEntered)
				<-allowFactoryReturn
				client := newFakeClient()
				client.files["/tables/1.rf"] = []byte("data")
				return &leasedClient{
					client: client,
					release: func() error {
						releaseCalls.Add(1)
						return nil
					},
				}, nil
			}
		},
	)
	if err != nil {
		t.Fatal(err)
	}

	openDone := make(chan error, 1)
	go func() {
		_, err := backend.Open(context.Background(), "/tables/1.rf")
		openDone <- err
	}()
	<-factoryEntered
	if err := backend.Close(); err != nil {
		t.Fatal(err)
	}
	close(allowFactoryReturn)

	select {
	case err := <-openDone:
		if !errors.Is(err, errBackendClosed) {
			t.Fatalf("racing Open error = %v, want errBackendClosed", err)
		}
	case <-time.After(time.Second):
		t.Fatal("racing Open did not finish after Backend.Close")
	}
	if got := releaseCalls.Load(); got != 1 {
		t.Fatalf("late operation lease release calls = %d, want 1", got)
	}
}

type fakeClient struct {
	files                    map[string][]byte
	modTimes                 map[string]time.Time
	dirs                     map[string]bool
	mkdir                    string
	failWriterClose          bool
	failPublish              bool
	failRestore              bool
	replicatingCloseFailures int
	writerCloseCalls         int
	lastReader               *fakeReader
	lastWriter               *fakeWriter
	lastCreatePath           string
	removeCalls              []string
	renameCalls              []renameCall
	failRemovePath           string
	failRemoveErr            error
	failBackupRemove         bool
	writerCloseHook          func() error
	lastRemoveDeadline       time.Time
	removeContextCalls       []string
	removeContextHook        func(context.Context, string) error
	lastRenameDeadline       time.Time
	renameContextCalls       []renameCall
	renameContextHook        func(context.Context, string, string) error
}

func newReleaseErrorBackend(t *testing.T, client *fakeClient) *Backend {
	t.Helper()

	backend, err := NewContext(context.Background(), "nn:8020",
		WithClient(client),
		func(c *config) {
			c.clientLeaseFactory = func(context.Context) (*leasedClient, error) {
				return &leasedClient{
					client:  client,
					release: func() error { return errInjectedRelease },
				}, nil
			}
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	return backend
}

func newFakeClient() *fakeClient {
	return &fakeClient{
		files:    make(map[string][]byte),
		modTimes: make(map[string]time.Time),
		dirs:     make(map[string]bool),
	}
}

func (c *fakeClient) Open(name string) (Reader, error) {
	data, ok := c.files[name]
	if !ok {
		return nil, &os.PathError{Op: "open", Path: name, Err: fs.ErrNotExist}
	}
	reader := &fakeReader{
		Reader: bytes.NewReader(data),
		info:   fakeInfo{name: path.Base(name), size: int64(len(data))},
	}
	c.lastReader = reader
	return reader, nil
}

func (c *fakeClient) Create(name string) (storage.Writer, error) {
	if _, exists := c.files[name]; exists {
		return nil, &os.PathError{Op: "create", Path: name, Err: fs.ErrExist}
	}
	c.files[name] = nil
	c.modTimes[name] = time.Now()
	writer := &fakeWriter{close: func(data []byte) {
		c.files[name] = append([]byte(nil), data...)
		c.modTimes[name] = time.Now()
	}, client: c, failClose: c.failWriterClose, closeHook: c.writerCloseHook}
	c.lastWriter = writer
	c.lastCreatePath = name
	return writer, nil
}

func (c *fakeClient) MkdirAll(dirname string, _ os.FileMode) error {
	c.mkdir = dirname
	c.dirs[dirname] = true
	return nil
}

func (c *fakeClient) ReadDir(dirname string) ([]os.FileInfo, error) {
	var out []os.FileInfo
	prefix := dirname + "/"
	for name, data := range c.files {
		if path.Dir(name) == dirname {
			out = append(out, fakeInfo{name: path.Base(name), size: int64(len(data)), modTime: c.modTimes[name]})
		}
	}
	for name := range c.dirs {
		if path.Dir(name) == dirname && name != dirname && len(name) > len(prefix) {
			out = append(out, fakeInfo{name: path.Base(name), dir: true})
		}
	}
	sortFileInfo(out)
	if len(out) == 0 {
		return nil, &os.PathError{Op: "readdir", Path: dirname, Err: fs.ErrNotExist}
	}
	return out, nil
}

func (c *fakeClient) Remove(name string) error {
	c.removeCalls = append(c.removeCalls, name)
	if c.failBackupRemove && strings.Contains(name, ".shoal-backup-") {
		return &os.PathError{Op: "remove", Path: name, Err: errors.New("injected backup removal failure")}
	}
	if c.failRemovePath != "" && name == c.failRemovePath && c.failRemoveErr != nil {
		return &os.PathError{Op: "remove", Path: name, Err: c.failRemoveErr}
	}
	if _, ok := c.files[name]; !ok {
		return &os.PathError{Op: "remove", Path: name, Err: fs.ErrNotExist}
	}
	delete(c.files, name)
	delete(c.modTimes, name)
	return nil
}

func (c *fakeClient) RemoveContext(ctx context.Context, name string) error {
	c.lastRemoveDeadline, _ = ctx.Deadline()
	c.removeContextCalls = append(c.removeContextCalls, name)
	if c.removeContextHook != nil {
		if err := c.removeContextHook(ctx, name); err != nil {
			return err
		}
	}
	return c.Remove(name)
}

func (c *fakeClient) RenameContext(ctx context.Context, oldpath, newpath string) error {
	c.lastRenameDeadline, _ = ctx.Deadline()
	c.renameContextCalls = append(c.renameContextCalls, renameCall{oldpath: oldpath, newpath: newpath})
	if c.renameContextHook != nil {
		if err := c.renameContextHook(ctx, oldpath, newpath); err != nil {
			return err
		}
	}
	return c.Rename(oldpath, newpath)
}

func (c *fakeClient) Rename(oldpath, newpath string) error {
	c.renameCalls = append(c.renameCalls, renameCall{oldpath: oldpath, newpath: newpath})
	if c.failPublish && strings.Contains(oldpath, ".shoal-tmp-") {
		return &os.PathError{Op: "rename", Path: oldpath, Err: errors.New("injected publish failure")}
	}
	if c.failRestore && strings.Contains(oldpath, ".shoal-backup-") {
		return &os.PathError{Op: "rename", Path: oldpath, Err: errors.New("injected restore failure")}
	}
	data, ok := c.files[oldpath]
	if !ok {
		return &os.PathError{Op: "rename", Path: oldpath, Err: fs.ErrNotExist}
	}
	if _, exists := c.files[newpath]; exists {
		return &os.PathError{Op: "rename", Path: newpath, Err: fs.ErrExist}
	}
	c.files[newpath] = data
	c.modTimes[newpath] = c.modTimes[oldpath]
	delete(c.files, oldpath)
	delete(c.modTimes, oldpath)
	return nil
}

func (c *fakeClient) Close() error { return nil }

type shutdownPublishClient struct {
	*fakeClient
	ctx            context.Context
	publishStarted chan struct{}
	publishOnce    sync.Once
}

func (c *shutdownPublishClient) RenameContext(ctx context.Context, oldpath, newpath string) error {
	if strings.Contains(oldpath, replacementTempPrefix) {
		c.publishOnce.Do(func() { close(c.publishStarted) })
		<-c.ctx.Done()
		return c.ctx.Err()
	}
	return c.fakeClient.RenameContext(ctx, oldpath, newpath)
}

type stalledCompleteState struct {
	cleanup          *fakeClient
	lastCreatePath   string
	writerCloseCalls int
}

type stalledCompleteClient struct {
	ctx   context.Context
	state *stalledCompleteState
}

func (c *stalledCompleteClient) Open(string) (Reader, error) {
	return nil, errors.New("not implemented")
}

func (c *stalledCompleteClient) Create(name string) (storage.Writer, error) {
	c.state.lastCreatePath = name
	c.state.cleanup.files[name] = []byte("temp")
	return &stalledCompleteWriter{ctx: c.ctx, state: c.state}, nil
}

func (c *stalledCompleteClient) MkdirAll(string, os.FileMode) error { return nil }

func (c *stalledCompleteClient) ReadDir(string) ([]os.FileInfo, error) {
	return nil, errors.New("not implemented")
}

func (c *stalledCompleteClient) Remove(string) error { return nil }

func (c *stalledCompleteClient) Rename(string, string) error { return nil }

func (c *stalledCompleteClient) Close() error { return nil }

type stalledCompleteWriter struct {
	ctx   context.Context
	state *stalledCompleteState
}

func (w *stalledCompleteWriter) Write(p []byte) (int, error) {
	return len(p), nil
}

func (w *stalledCompleteWriter) Close() error {
	w.state.writerCloseCalls++
	<-w.ctx.Done()
	return w.ctx.Err()
}

type cleanupLifetimeState struct {
	files               map[string][]byte
	lastCreatePath      string
	removeCalls         []string
	cleanupClientCtxErr error
}

type cleanupLifetimeFactory struct {
	mu         sync.Mutex
	state      *cleanupLifetimeState
	cancelSeed context.CancelFunc
	seeded     bool
}

func newCleanupLifetimeFactory(cancelSeed context.CancelFunc) *cleanupLifetimeFactory {
	return &cleanupLifetimeFactory{
		state: &cleanupLifetimeState{
			files: make(map[string][]byte),
		},
		cancelSeed: cancelSeed,
	}
}

func (f *cleanupLifetimeFactory) lease(ctx context.Context) (*leasedClient, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	if !f.seeded {
		f.seeded = true
		return &leasedClient{
			client:  &cleanupLifetimeClient{ctx: ctx, state: f.state},
			release: func() error { return nil },
		}, nil
	}
	return &leasedClient{
		client:  &cleanupLifetimeOperationClient{ctx: ctx, state: f.state, cancelSeed: f.cancelSeed},
		release: func() error { return nil },
	}, nil
}

type cleanupLifetimeClient struct {
	ctx   context.Context
	state *cleanupLifetimeState
}

func (*cleanupLifetimeClient) Open(string) (Reader, error) { return nil, errors.New("not implemented") }

func (*cleanupLifetimeClient) Create(string) (storage.Writer, error) {
	return nil, errors.New("not implemented")
}

func (*cleanupLifetimeClient) MkdirAll(string, os.FileMode) error { return nil }

func (*cleanupLifetimeClient) ReadDir(string) ([]os.FileInfo, error) {
	return nil, errors.New("not implemented")
}

func (c *cleanupLifetimeClient) Remove(name string) error {
	return c.RemoveContext(context.Background(), name)
}

func (c *cleanupLifetimeClient) RemoveContext(_ context.Context, name string) error {
	c.state.removeCalls = append(c.state.removeCalls, name)
	c.state.cleanupClientCtxErr = c.ctx.Err()
	if c.ctx.Err() != nil {
		return c.ctx.Err()
	}
	if _, ok := c.state.files[name]; !ok {
		return &os.PathError{Op: "remove", Path: name, Err: fs.ErrNotExist}
	}
	delete(c.state.files, name)
	return nil
}

func (c *cleanupLifetimeClient) Rename(oldpath, newpath string) error {
	if c.ctx.Err() != nil {
		return c.ctx.Err()
	}
	data, ok := c.state.files[oldpath]
	if !ok {
		return &os.PathError{Op: "rename", Path: oldpath, Err: fs.ErrNotExist}
	}
	c.state.files[newpath] = data
	delete(c.state.files, oldpath)
	return nil
}

func (*cleanupLifetimeClient) Close() error { return nil }

type cleanupLifetimeOperationClient struct {
	ctx        context.Context
	state      *cleanupLifetimeState
	cancelSeed context.CancelFunc
}

func (*cleanupLifetimeOperationClient) Open(string) (Reader, error) {
	return nil, errors.New("not implemented")
}

func (c *cleanupLifetimeOperationClient) Create(name string) (storage.Writer, error) {
	c.state.lastCreatePath = name
	c.state.files[name] = []byte("temp")
	return &cancelOnCloseWriter{ctx: c.ctx, cancel: c.cancelSeed}, nil
}

func (*cleanupLifetimeOperationClient) MkdirAll(string, os.FileMode) error { return nil }

func (*cleanupLifetimeOperationClient) ReadDir(string) ([]os.FileInfo, error) {
	return nil, errors.New("not implemented")
}

func (*cleanupLifetimeOperationClient) Remove(string) error { return errors.New("not implemented") }

func (*cleanupLifetimeOperationClient) Rename(string, string) error {
	return errors.New("not implemented")
}

func (*cleanupLifetimeOperationClient) Close() error { return nil }

type cancelOnCloseWriter struct {
	ctx    context.Context
	cancel context.CancelFunc
}

func (*cancelOnCloseWriter) Write(p []byte) (int, error) { return len(p), nil }

func (w *cancelOnCloseWriter) Close() error {
	w.cancel()
	<-w.ctx.Done()
	return w.ctx.Err()
}

type fakeReader struct {
	*bytes.Reader
	info     os.FileInfo
	deadline time.Time
}

type controlledCloseClient struct {
	*fakeClient
	reader Reader
}

func (c *controlledCloseClient) Open(string) (Reader, error) {
	return c.reader, nil
}

type controlledCloseReader struct {
	*bytes.Reader
	info         os.FileInfo
	closeStarted chan struct{}
	allowClose   chan struct{}
	closeErr     error
	startOnce    sync.Once
}

func (r *controlledCloseReader) Close() error {
	if r.closeStarted != nil {
		r.startOnce.Do(func() { close(r.closeStarted) })
	}
	if r.allowClose != nil {
		<-r.allowClose
	}
	return r.closeErr
}

func (r *controlledCloseReader) Stat() os.FileInfo { return r.info }

func (r *fakeReader) Close() error      { return nil }
func (r *fakeReader) Stat() os.FileInfo { return r.info }
func (r *fakeReader) SetDeadline(t time.Time) error {
	r.deadline = t
	return nil
}

type contextReadClient struct {
	ctx context.Context
}

func (c *contextReadClient) Open(name string) (Reader, error) {
	return &contextReadReader{
		ctx:  c.ctx,
		info: fakeInfo{name: path.Base(name), size: 1},
	}, nil
}

func (*contextReadClient) Create(string) (storage.Writer, error) {
	return nil, errors.New("not implemented")
}

func (*contextReadClient) MkdirAll(string, os.FileMode) error { return nil }

func (*contextReadClient) ReadDir(string) ([]os.FileInfo, error) {
	return nil, errors.New("not implemented")
}

func (*contextReadClient) Remove(string) error { return errors.New("not implemented") }

func (*contextReadClient) Rename(string, string) error { return errors.New("not implemented") }

func (*contextReadClient) Close() error { return nil }

type contextReadReader struct {
	ctx  context.Context
	info os.FileInfo
}

func (r *contextReadReader) ReadAt([]byte, int64) (int, error) {
	<-r.ctx.Done()
	return 0, r.ctx.Err()
}

func (*contextReadReader) Close() error        { return nil }
func (r *contextReadReader) Stat() os.FileInfo { return r.info }

type openReaderClient struct {
	reader Reader
}

func (c *openReaderClient) Open(string) (Reader, error) { return c.reader, nil }
func (*openReaderClient) Create(string) (storage.Writer, error) {
	return nil, errors.New("not implemented")
}
func (*openReaderClient) MkdirAll(string, os.FileMode) error { return nil }
func (*openReaderClient) ReadDir(string) ([]os.FileInfo, error) {
	return nil, errors.New("not implemented")
}
func (*openReaderClient) Remove(string) error         { return errors.New("not implemented") }
func (*openReaderClient) Rename(string, string) error { return errors.New("not implemented") }
func (*openReaderClient) Close() error                { return nil }

type perReadDeadlineReader struct {
	data      []byte
	info      os.FileInfo
	deadlines []time.Time
}

func (r *perReadDeadlineReader) ReadAt(p []byte, off int64) (int, error) {
	if len(r.deadlines) == 0 || r.deadlines[len(r.deadlines)-1].IsZero() {
		return 0, errors.New("missing read deadline")
	}
	if off >= int64(len(r.data)) {
		return 0, io.EOF
	}
	n := copy(p, r.data[off:off+1])
	if off+int64(n) >= int64(len(r.data)) {
		return n, io.EOF
	}
	return n, nil
}

func (*perReadDeadlineReader) Close() error        { return nil }
func (r *perReadDeadlineReader) Stat() os.FileInfo { return r.info }
func (r *perReadDeadlineReader) SetDeadline(deadline time.Time) error {
	r.deadlines = append(r.deadlines, deadline)
	return nil
}

type deadlineBlockingReadReader struct {
	info      os.FileInfo
	deadlines []time.Time
}

func (r *deadlineBlockingReadReader) ReadAt([]byte, int64) (int, error) {
	if len(r.deadlines) == 0 || r.deadlines[len(r.deadlines)-1].IsZero() {
		return 0, errors.New("missing read deadline")
	}
	timer := time.NewTimer(time.Until(r.deadlines[len(r.deadlines)-1]))
	defer timer.Stop()
	<-timer.C
	return 0, context.DeadlineExceeded
}

func (*deadlineBlockingReadReader) Close() error        { return nil }
func (r *deadlineBlockingReadReader) Stat() os.FileInfo { return r.info }
func (r *deadlineBlockingReadReader) SetDeadline(deadline time.Time) error {
	r.deadlines = append(r.deadlines, deadline)
	return nil
}

type fakeWriter struct {
	bytes.Buffer
	close     func([]byte)
	client    *fakeClient
	failClose bool
	deadline  time.Time
	closeHook func() error
}

type deadlineBlockingWriter struct {
	deadlines []time.Time
}

func (w *deadlineBlockingWriter) Write(p []byte) (int, error) { return len(p), nil }
func (w *deadlineBlockingWriter) Close() error {
	deadline := w.deadlines[len(w.deadlines)-1]
	timer := time.NewTimer(time.Until(deadline))
	defer timer.Stop()
	<-timer.C
	return os.ErrDeadlineExceeded
}
func (w *deadlineBlockingWriter) SetDeadline(deadline time.Time) error {
	w.deadlines = append(w.deadlines, deadline)
	return nil
}

var errInjectedClose = errors.New("injected close failure")
var errInjectedRelease = errors.New("injected release failure")

func setCleanupTimeoutForTest(t *testing.T, timeout time.Duration) {
	t.Helper()
	original := cleanupTimeout
	cleanupTimeout = timeout
	t.Cleanup(func() {
		cleanupTimeout = original
	})
}

func countCalls(values []string, want string) int {
	count := 0
	for _, value := range values {
		if value == want {
			count++
		}
	}
	return count
}

func setReplacementNameTokens(t *testing.T, tokens ...string) {
	t.Helper()

	original := randomReplacementNameToken
	index := 0
	randomReplacementNameToken = func() (string, error) {
		if index >= len(tokens) {
			return "", fmt.Errorf("replacement token call %d exceeded test inputs", index+1)
		}
		token := tokens[index]
		index++
		return token, nil
	}
	t.Cleanup(func() {
		randomReplacementNameToken = original
	})
}

func checkReplacementSiblingPath(t *testing.T, sibling, wantDir, wantPrefix string) {
	t.Helper()

	if got := path.Dir(sibling); got != wantDir {
		t.Fatalf("sibling directory = %q, want %q", got, wantDir)
	}
	base := path.Base(sibling)
	if !strings.HasPrefix(base, wantPrefix) {
		t.Fatalf("sibling base = %q, want prefix %q", base, wantPrefix)
	}
	if got, want := len(base), len(wantPrefix)+replacementNameTokenBytes*2; got != want {
		t.Fatalf("sibling base length = %d, want %d", got, want)
	}
	if len(base) > 255 {
		t.Fatalf("sibling base length = %d, want <= 255", len(base))
	}
}

type recordingConn struct {
	deadlines []time.Time
}

func (*recordingConn) Read([]byte) (int, error)         { return 0, nil }
func (*recordingConn) Write(p []byte) (int, error)      { return len(p), nil }
func (*recordingConn) Close() error                     { return nil }
func (*recordingConn) LocalAddr() net.Addr              { return fakeConnAddr("local") }
func (*recordingConn) RemoteAddr() net.Addr             { return fakeConnAddr("remote") }
func (*recordingConn) SetReadDeadline(time.Time) error  { return nil }
func (*recordingConn) SetWriteDeadline(time.Time) error { return nil }
func (c *recordingConn) SetDeadline(deadline time.Time) error {
	c.deadlines = append(c.deadlines, deadline)
	return nil
}

func (c *recordingConn) lastDeadline() time.Time {
	if len(c.deadlines) == 0 {
		return time.Time{}
	}
	return c.deadlines[len(c.deadlines)-1]
}

type fakeConnAddr string

func (a fakeConnAddr) Network() string { return "tcp" }
func (a fakeConnAddr) String() string  { return string(a) }

type immediateErrorDeadlineWriter struct {
	deadlines []time.Time
}

type contextCloseWriter struct {
	closeCalls        int
	contextCloseCalls int
}

type cleanupFailureWriter struct {
	closeErr error
}

type cleanupFailureReader struct {
	closeErr error
}

func (w *contextCloseWriter) Write(p []byte) (int, error) { return len(p), nil }
func (w *contextCloseWriter) Close() error {
	w.closeCalls++
	return errors.New("unexpected Close call")
}
func (w *contextCloseWriter) CloseContext(ctx context.Context) error {
	w.contextCloseCalls++
	<-ctx.Done()
	return ctx.Err()
}
func (w *contextCloseWriter) SetDeadline(time.Time) error { return nil }

func (w *immediateErrorDeadlineWriter) Write(p []byte) (int, error) { return len(p), nil }
func (w *immediateErrorDeadlineWriter) Close() error                { return errInjectedClose }
func (w *immediateErrorDeadlineWriter) SetDeadline(deadline time.Time) error {
	w.deadlines = append(w.deadlines, deadline)
	return nil
}

func (w *cleanupFailureWriter) Write(p []byte) (int, error) { return len(p), nil }
func (w *cleanupFailureWriter) Close() error                { return w.closeErr }

func (*cleanupFailureReader) ReadAt([]byte, int64) (int, error) { return 0, nil }
func (r *cleanupFailureReader) Close() error                    { return r.closeErr }
func (*cleanupFailureReader) Stat() os.FileInfo                 { return fakeInfo{name: "reader"} }

func (w *fakeWriter) Close() error {
	w.client.writerCloseCalls++
	if w.closeHook != nil {
		if err := w.closeHook(); err != nil {
			return err
		}
	}
	if w.client.replicatingCloseFailures > 0 {
		w.client.replicatingCloseFailures--
		return &os.PathError{Op: "create", Path: "temporary", Err: hdfsclient.ErrReplicating}
	}
	if w.failClose {
		return errors.New("injected close failure")
	}
	if w.close != nil {
		w.close(w.Bytes())
	}
	return nil
}

func (w *fakeWriter) SetDeadline(t time.Time) error {
	w.deadline = t
	return nil
}

type renameCall struct {
	oldpath string
	newpath string
}

type fakeInfo struct {
	name    string
	size    int64
	dir     bool
	modTime time.Time
}

func (i fakeInfo) Name() string { return i.name }
func (i fakeInfo) Size() int64  { return i.size }
func (i fakeInfo) Mode() os.FileMode {
	if i.dir {
		return os.ModeDir | 0o755
	}
	return 0o644
}
func (i fakeInfo) ModTime() time.Time { return i.modTime }
func (i fakeInfo) IsDir() bool        { return i.dir }
func (i fakeInfo) Sys() any           { return nil }

func sortFileInfo(entries []os.FileInfo) {
	for i := 1; i < len(entries); i++ {
		for j := i; j > 0 && entries[j].Name() < entries[j-1].Name(); j-- {
			entries[j], entries[j-1] = entries[j-1], entries[j]
		}
	}
}

type overlapReader struct {
	active     atomic.Int32
	overlapped atomic.Bool
}

func (r *overlapReader) ReadAt(p []byte, _ int64) (int, error) {
	if r.active.Add(1) != 1 {
		r.overlapped.Store(true)
	}
	time.Sleep(time.Millisecond)
	r.active.Add(-1)
	p[0] = 1
	return 1, nil
}

func (r *overlapReader) Close() error      { return nil }
func (r *overlapReader) Stat() os.FileInfo { return fakeInfo{name: "file", size: 1} }
