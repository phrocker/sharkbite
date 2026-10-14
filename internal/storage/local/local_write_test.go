package local

import (
	"bytes"
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/phrocker/shoal-oss/internal/storage"
)

func TestLocal_CreateAndReadBack(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "out.bin")
	be := New()

	w, err := be.Create(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	body := []byte("hello, write side")
	if _, err := w.Write(body); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}

	// File now exists with the bytes we wrote.
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, body) {
		t.Errorf("disk content = %q, want %q", got, body)
	}
}

func TestLocal_CreateMakesParentDirs(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "deep", "nested", "tree", "out.bin")
	be := New()

	w, err := be.Create(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write([]byte("nested")); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Errorf("Create did not produce file: %v", err)
	}
}

func TestLocal_CreateReplacesExisting(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "x.bin")
	if err := os.WriteFile(path, []byte("old contents that are longer"), 0o644); err != nil {
		t.Fatal(err)
	}

	be := New()
	w, err := be.Create(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write([]byte("new")); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "new" {
		t.Errorf("got %q, want \"new\" (Create should truncate)", got)
	}
}

func TestLocal_CreateUsesOpenFileModeSubjectToUmask(t *testing.T) {
	dir := t.TempDir()
	referencePath := filepath.Join(dir, "reference")
	reference, err := os.OpenFile(referencePath, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if err := reference.Close(); err != nil {
		t.Fatal(err)
	}

	path := filepath.Join(dir, "created")
	w, err := New().Create(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}

	referenceInfo, err := os.Stat(referencePath)
	if err != nil {
		t.Fatal(err)
	}
	createdInfo, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := createdInfo.Mode().Perm(), referenceInfo.Mode().Perm(); got != want {
		t.Fatalf("created mode = %04o, want OpenFile(0644) mode %04o", got, want)
	}
}

func TestLocal_CreateReplacementPreservesExistingMode(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "replaced")
	if err := os.WriteFile(path, []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	originalInfo, err := os.Stat(path)
	if err != nil {
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

	replacedInfo, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := replacedInfo.Mode().Perm(), originalInfo.Mode().Perm(); got != want {
		t.Fatalf("replacement mode = %04o, want existing mode %04o", got, want)
	}
}

func TestLocal_ReplacementKeepsTargetVisibleUntilAtomicPublish(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "target")
	if err := os.WriteFile(path, []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}

	w, err := New().Create(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	localWriter := w.(*writer)
	blockingOps := &blockingAtomicReplaceOps{
		replacementOps: localWriter.ops,
		entered:        make(chan struct{}),
		release:        make(chan struct{}),
	}
	localWriter.ops = blockingOps
	if _, err := w.Write([]byte("new")); err != nil {
		t.Fatal(err)
	}

	done := make(chan error, 1)
	go func() { done <- w.Close() }()
	<-blockingOps.entered
	for range 100 {
		got, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("observer saw target missing before publish: %v", err)
		}
		if string(got) != "old" {
			t.Fatalf("observer saw %q before atomic publish, want old", got)
		}
	}
	close(blockingOps.release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "new" {
		t.Fatalf("target contents = %q, want new", got)
	}
}

func TestLocal_AtomicReplaceFailureLeavesOldTargetIntact(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "target")
	if err := os.WriteFile(path, []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}

	w, err := New().Create(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	localWriter := w.(*writer)
	replaceErr := errors.New("injected atomic replace failure")
	localWriter.ops = atomicReplaceErrorOps{replacementOps: localWriter.ops, err: replaceErr}
	if _, err := w.Write([]byte("new")); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); !errors.Is(err, replaceErr) {
		t.Fatalf("Close error = %v, want %v", err, replaceErr)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "old" {
		t.Fatalf("target contents = %q, want old", got)
	}
	if err := w.(storage.Aborter).Abort(); err != nil {
		t.Fatal(err)
	}
}

func TestLocal_CreateRejectsDirectoryTarget(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "target")
	if err := os.Mkdir(path, 0o755); err != nil {
		t.Fatal(err)
	}

	w, err := New().Create(context.Background(), path)
	if err == nil || !strings.Contains(err.Error(), "not a regular file") {
		if w != nil {
			_ = w.(storage.Aborter).Abort()
		}
		t.Fatalf("Create error = %v, want non-regular target rejection", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if !info.IsDir() {
		t.Fatalf("target mode = %v, want directory preserved", info.Mode())
	}
}

func TestLocal_BackupRemovalFailureReturnsCommittedWriteError(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "target")
	if err := os.WriteFile(path, []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}

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
	if got := filepath.Dir(ops.backupPath); got != dir {
		t.Fatalf("backup directory = %s, want %s", got, dir)
	}
	if base := filepath.Base(ops.backupPath); !strings.HasPrefix(base, replacementBackupPrefix) {
		t.Fatalf("backup name = %q, want prefix %q", base, replacementBackupPrefix)
	} else if got, want := len(base), len(replacementBackupPrefix)+replacementNameTokenBytes*2; got != want {
		t.Fatalf("backup name length = %d, want %d", got, want)
	}

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "new" {
		t.Fatalf("target contents = %q, want committed replacement", got)
	}
	if got, err := os.ReadFile(ops.backupPath); err != nil || string(got) != "old" {
		t.Fatalf("backup contents = %q, %v; want old", got, err)
	}
	if err := w.(storage.Aborter).Abort(); err == nil || !strings.Contains(err.Error(), "already closed") {
		t.Fatalf("Abort after committed cleanup failure = %v, want already closed", err)
	}
}

func TestLocal_CloseDurablySyncsReplacementBeforeAndAfterRename(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "target")
	if err := os.WriteFile(path, []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}

	w, err := New().Create(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	localWriter := w.(*writer)
	ops := &durabilityRecordingOps{replacementOps: localWriter.ops}
	localWriter.ops = ops

	originalPreservePlatformMetadata := preservePlatformMetadataFn
	t.Cleanup(func() { preservePlatformMetadataFn = originalPreservePlatformMetadata })
	preservePlatformMetadataFn = func(_, _ string) error {
		ops.events = append(ops.events, "platform")
		return nil
	}

	if _, err := w.Write([]byte("new")); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if got, want := ops.events, []string{"platform", "chmod", "sync-temp", "replace", "sync-parent", "remove-backup", "sync-parent"}; !equalStringSlices(got, want) {
		t.Fatalf("durability events = %v, want %v", got, want)
	}
}

func TestLocal_CloseParentSyncFailureAfterRenameReturnsCommittedWriteError(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "target")
	if err := os.WriteFile(path, []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}

	w, err := New().Create(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	localWriter := w.(*writer)
	ops := &durabilityRecordingOps{
		replacementOps:      localWriter.ops,
		syncParentFailAfter: 1,
		syncParentErr:       errors.New("directory sync failed"),
	}
	localWriter.ops = ops

	if _, err := w.Write([]byte("new")); err != nil {
		t.Fatal(err)
	}
	err = w.Close()
	if !errors.Is(err, ops.syncParentErr) {
		t.Fatalf("Close error = %v, want %v", err, ops.syncParentErr)
	}
	if !storage.IsCommittedWriteError(err) {
		t.Fatalf("Close error = %v, want committed-write marker", err)
	}
	if got, err := os.ReadFile(path); err != nil || string(got) != "new" {
		t.Fatalf("target contents = %q, %v; want committed replacement", got, err)
	}
	if err := w.(storage.Aborter).Abort(); err == nil || !strings.Contains(err.Error(), "already closed") {
		t.Fatalf("Abort after committed parent-sync failure = %v, want already closed", err)
	}
}

func TestLocal_LongNameTargetsUseFixedLengthSiblingArtifacts(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("long NAME_MAX-style filename regression is not portable on Windows")
	}

	dir := t.TempDir()
	checkTemp := func(t *testing.T, w storage.Writer) {
		t.Helper()
		localWriter := w.(*writer)
		if got := filepath.Dir(localWriter.temp); got != dir {
			t.Fatalf("temporary directory = %s, want %s", got, dir)
		}
		if base := filepath.Base(localWriter.temp); !strings.HasPrefix(base, replacementTempPrefix) {
			t.Fatalf("temporary name = %q, want prefix %q", base, replacementTempPrefix)
		} else if got, want := len(base), len(replacementTempPrefix)+replacementNameTokenBytes*2; got != want {
			t.Fatalf("temporary name length = %d, want %d", got, want)
		}
	}

	createPath := filepath.Join(dir, strings.Repeat("c", 255))
	w, err := New().Create(context.Background(), createPath)
	if err != nil {
		t.Fatal(err)
	}
	checkTemp(t, w)
	if _, err := w.Write([]byte("create")); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	if got, err := os.ReadFile(createPath); err != nil || string(got) != "create" {
		t.Fatalf("created file = %q, %v; want create", got, err)
	}

	replacePath := filepath.Join(dir, strings.Repeat("r", 255))
	if err := os.WriteFile(replacePath, []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	w, err = New().Create(context.Background(), replacePath)
	if err != nil {
		t.Fatal(err)
	}
	checkTemp(t, w)
	if _, err := w.Write([]byte("replace")); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	if got, err := os.ReadFile(replacePath); err != nil || string(got) != "replace" {
		t.Fatalf("replaced file = %q, %v; want replace", got, err)
	}

	abortPath := filepath.Join(dir, strings.Repeat("a", 255))
	w, err = New().Create(context.Background(), abortPath)
	if err != nil {
		t.Fatal(err)
	}
	checkTemp(t, w)
	if _, err := w.Write([]byte("abort")); err != nil {
		t.Fatal(err)
	}
	if err := w.(storage.Aborter).Abort(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(abortPath); !os.IsNotExist(err) {
		t.Fatalf("aborted target exists: %v", err)
	}
	if matches, err := filepath.Glob(filepath.Join(dir, ".shoal-*")); err != nil {
		t.Fatal(err)
	} else if len(matches) != 0 {
		t.Fatalf("replacement artifacts remain: %v", matches)
	}
}

func TestLocal_TemporaryNameCollisionRetriesAndAbortCleansAllocatedSibling(t *testing.T) {
	dir := t.TempDir()
	collision := filepath.Join(dir, replacementTempPrefix+"collision")
	if err := os.WriteFile(collision, []byte("sentinel"), 0o600); err != nil {
		t.Fatal(err)
	}
	setReplacementNameTokens(t, "collision", "allocated")

	w, err := New().Create(context.Background(), filepath.Join(dir, "target"))
	if err != nil {
		t.Fatal(err)
	}
	temp := w.(*writer).temp
	if got := filepath.Base(temp); got != replacementTempPrefix+"allocated" {
		t.Fatalf("temporary sibling = %q, want allocated retry", got)
	}
	if err := w.(storage.Aborter).Abort(); err != nil {
		t.Fatal(err)
	}
	if got, err := os.ReadFile(collision); err != nil || string(got) != "sentinel" {
		t.Fatalf("colliding file = %q, %v; want preserved sentinel", got, err)
	}
	if _, err := os.Stat(temp); !os.IsNotExist(err) {
		t.Fatalf("allocated temporary sibling remains after abort: %v", err)
	}
}

func TestLocal_BackupNameCollisionRetriesAndCleansAllocatedBackup(t *testing.T) {
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

	w, err := New().Create(context.Background(), target)
	if err != nil {
		t.Fatal(err)
	}
	localWriter := w.(*writer)
	ops := &collisionOnceAtomicReplaceOps{replacementOps: localWriter.ops}
	localWriter.ops = ops
	if _, err := w.Write([]byte("new")); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	if got, err := os.ReadFile(target); err != nil || string(got) != "new" {
		t.Fatalf("target = %q, %v; want new", got, err)
	}
	if got, err := os.ReadFile(collision); err != nil || string(got) != "sentinel" {
		t.Fatalf("colliding backup = %q, %v; want preserved sentinel", got, err)
	}
	if len(ops.backups) != 2 {
		t.Fatalf("backup attempts = %v, want collision and allocated", ops.backups)
	}
	if got := filepath.Base(ops.backups[1]); got != replacementBackupPrefix+"allocated" {
		t.Fatalf("second backup = %q, want allocated retry", got)
	}
	if _, err := os.Stat(ops.backups[1]); !os.IsNotExist(err) {
		t.Fatalf("allocated backup remains after commit: %v", err)
	}
}

func setReplacementNameTokens(t *testing.T, tokens ...string) {
	t.Helper()
	original := randomReplacementNameToken
	index := 0
	randomReplacementNameToken = func() (string, error) {
		if index >= len(tokens) {
			return tokens[len(tokens)-1], nil
		}
		token := tokens[index]
		index++
		return token, nil
	}
	t.Cleanup(func() {
		randomReplacementNameToken = original
	})
}

func TestPlatformAtomicReplaceAndRestore(t *testing.T) {
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

	ops := osReplacementOps{}
	if err := ops.AtomicReplace(temp, target, backup, true); err != nil {
		t.Fatal(err)
	}
	if got, err := os.ReadFile(target); err != nil || string(got) != "new" {
		t.Fatalf("published target = %q, %v; want new", got, err)
	}
	if got, err := os.ReadFile(backup); err != nil || string(got) != "old" {
		t.Fatalf("backup = %q, %v; want old", got, err)
	}
	if err := ops.AtomicRestore(target, backup); err != nil {
		t.Fatal(err)
	}
	if got, err := os.ReadFile(target); err != nil || string(got) != "old" {
		t.Fatalf("restored target = %q, %v; want old", got, err)
	}
	if _, err := os.Stat(backup); !os.IsNotExist(err) {
		t.Fatalf("backup remains after restore: %v", err)
	}
}

func TestLocal_AbortPreservesExistingFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "x.bin")
	if err := os.WriteFile(path, []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}

	be := New()
	w, err := be.Create(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write([]byte("new")); err != nil {
		t.Fatal(err)
	}
	aborter, ok := w.(storage.Aborter)
	if !ok {
		t.Fatal("writer does not implement storage.Aborter")
	}
	if err := aborter.Abort(); err != nil {
		t.Fatal(err)
	}

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "old" {
		t.Fatalf("got %q, want original file preserved", got)
	}
}

func TestLocal_AbortAfterFailedCloseRemovesTemp(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "out.bin")
	w, err := New().Create(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}

	localWriter := w.(*writer)
	temp := localWriter.temp
	if err := localWriter.file.Close(); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err == nil {
		t.Fatal("Close succeeded after the temporary file was closed")
	}
	if err := w.(storage.Aborter).Abort(); err != nil {
		t.Fatalf("Abort after failed Close: %v", err)
	}
	if _, err := os.Stat(temp); !os.IsNotExist(err) {
		t.Fatalf("temporary file still exists after Abort: %v", err)
	}
}

func TestLocal_CloseErrorCleansUpTempViaReplacementOps(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "out.bin")
	w, err := New().Create(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}

	localWriter := w.(*writer)
	ops := &recordingRemoveOps{replacementOps: localWriter.ops}
	localWriter.ops = ops
	temp := localWriter.temp
	if err := localWriter.file.Close(); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err == nil {
		t.Fatal("Close succeeded after the temporary file was already closed")
	}
	if len(ops.removed) != 1 || ops.removed[0] != temp {
		t.Fatalf("Remove calls = %v, want [%s]", ops.removed, temp)
	}
	if _, err := os.Stat(temp); !os.IsNotExist(err) {
		t.Fatalf("temporary file still exists after Close cleanup: %v", err)
	}
	if err := w.Close(); err == nil || !strings.Contains(err.Error(), "discarded") {
		t.Fatalf("retried Close error = %v, want the discarded temporary file reported", err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("destination published after a failed close: %v", err)
	}
}

func TestCleanupTemporaryCreateFileJoinsCloseAndRemoveFailures(t *testing.T) {
	file := fakeNamedCloser{name: "temp", closeErr: errors.New("close failed")}
	ops := cleanupFailureOps{removeErr: errors.New("remove failed")}

	err := cleanupTemporaryCreateFile(file, ops)
	if err == nil || !strings.Contains(err.Error(), "close temporary file temp") || !strings.Contains(err.Error(), "remove temporary file temp") {
		t.Fatalf("cleanupTemporaryCreateFile error = %v, want joined close/remove failures", err)
	}
}

func TestCleanupTemporaryCreateFileIgnoresNotFoundRemoval(t *testing.T) {
	file := fakeNamedCloser{name: "temp"}
	ops := cleanupFailureOps{removeErr: fs.ErrNotExist}

	if err := cleanupTemporaryCreateFile(file, ops); err != nil {
		t.Fatalf("cleanupTemporaryCreateFile: %v", err)
	}
}

func TestLocal_WriterSyncForwardsToTemporaryFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "out.bin")
	w, err := New().Create(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	sy, ok := w.(interface{ Sync() error })
	if !ok {
		t.Fatal("Create writer does not implement Sync")
	}
	if _, err := w.Write([]byte("new")); err != nil {
		t.Fatal(err)
	}
	if err := sy.Sync(); err != nil {
		t.Fatalf("Sync: %v", err)
	}

	localWriter := w.(*writer)
	if err := localWriter.file.Close(); err != nil {
		t.Fatal(err)
	}
	localWriter.fileClosed = true
	if err := sy.Sync(); err == nil || !strings.Contains(err.Error(), "sync temporary file") {
		t.Fatalf("Sync after underlying file close error = %v, want sync temporary file failure", err)
	}
	if err := w.(storage.Aborter).Abort(); err != nil {
		t.Fatalf("Abort after Sync regression: %v", err)
	}
}

func TestLocal_AbortRetriesTempRemovalAfterFailure(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "out.bin")
	w, err := New().Create(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}

	localWriter := w.(*writer)
	ops := &flakyRemoveOps{
		replacementOps: localWriter.ops,
		failPath:       localWriter.temp,
		failures:       1,
		err:            errors.New("transient remove failure"),
	}
	localWriter.ops = ops

	aborter := w.(storage.Aborter)
	err = aborter.Abort()
	if err == nil || !strings.Contains(err.Error(), "remove temporary file") {
		t.Fatalf("Abort error = %v, want remove temporary file failure", err)
	}
	if err := w.Close(); err == nil || !strings.Contains(err.Error(), "writer already aborted") {
		t.Fatalf("Close after failed Abort error = %v, want writer already aborted", err)
	}

	if err := aborter.Abort(); err != nil {
		t.Fatalf("second Abort: %v", err)
	}
	if ops.removeAttempts != 2 {
		t.Fatalf("Remove attempts = %d, want 2", ops.removeAttempts)
	}
	if _, err := os.Stat(localWriter.temp); !os.IsNotExist(err) {
		t.Fatalf("temporary file still exists after retry: %v", err)
	}
}

func TestPreserveExistingMetadataStripsPrivilegeBitsButPreservesStickyAfterPlatformMetadata(t *testing.T) {
	originalPreservePlatformMetadata := preservePlatformMetadataFn
	t.Cleanup(func() {
		preservePlatformMetadataFn = originalPreservePlatformMetadata
	})

	var calls []string
	preservePlatformMetadataFn = func(_, _ string) error {
		calls = append(calls, "platform")
		return nil
	}

	ops := &metadataRecordingOps{
		infos: map[string]os.FileInfo{
			"target": stubFileInfo{mode: os.ModeSetuid | os.ModeSetgid | os.ModeSticky | 0o640},
		},
		onChmod: func() {
			calls = append(calls, "chmod")
		},
	}

	hadOld, err := preserveExistingMetadata(ops, "temp", "target")
	if err != nil {
		t.Fatal(err)
	}
	if !hadOld {
		t.Fatal("preserveExistingMetadata reported no existing target")
	}
	if len(ops.chmodModes) != 1 {
		t.Fatalf("Chmod calls = %d, want 1", len(ops.chmodModes))
	}
	wantMode := os.ModeSticky | 0o640
	if ops.chmodModes[0] != wantMode {
		t.Fatalf("Chmod mode = %v, want %v", ops.chmodModes[0], wantMode)
	}
	if len(calls) != 2 || calls[0] != "platform" || calls[1] != "chmod" {
		t.Fatalf("metadata call order = %v, want [platform chmod]", calls)
	}
}

type blockingAtomicReplaceOps struct {
	replacementOps
	entered chan struct{}
	release chan struct{}
}

type fakeNamedCloser struct {
	name     string
	closeErr error
}

func (f fakeNamedCloser) Name() string { return f.name }
func (f fakeNamedCloser) Close() error { return f.closeErr }

type cleanupFailureOps struct {
	removeErr error
}

func (cleanupFailureOps) Lstat(string) (os.FileInfo, error)                { return nil, fs.ErrNotExist }
func (cleanupFailureOps) Chmod(string, os.FileMode) error                  { return nil }
func (o cleanupFailureOps) Remove(string) error                            { return o.removeErr }
func (cleanupFailureOps) AtomicReplace(string, string, string, bool) error { return nil }
func (cleanupFailureOps) AtomicRestore(string, string) error               { return nil }
func (cleanupFailureOps) AtomicRestoreIfAbsent(string, string) error       { return nil }

func (o *blockingAtomicReplaceOps) AtomicReplace(temp, target, backup string, hadOld bool) error {
	close(o.entered)
	<-o.release
	return o.replacementOps.AtomicReplace(temp, target, backup, hadOld)
}

type atomicReplaceErrorOps struct {
	replacementOps
	err error
}

func (o atomicReplaceErrorOps) AtomicReplace(string, string, string, bool) error {
	return o.err
}

type collisionOnceAtomicReplaceOps struct {
	replacementOps
	backups []string
}

func (o *collisionOnceAtomicReplaceOps) AtomicReplace(temp, target, backup string, hadOld bool) error {
	o.backups = append(o.backups, backup)
	if len(o.backups) == 1 {
		return fs.ErrExist
	}
	return o.replacementOps.AtomicReplace(temp, target, backup, hadOld)
}

type blockingRollbackOps struct {
	replacementOps
	err        error
	entered    chan struct{}
	release    chan struct{}
	backupPath string
}

func (o *blockingRollbackOps) Remove(name string) error {
	if strings.Contains(name, replacementBackupPrefix) {
		o.backupPath = name
		return o.err
	}
	return o.replacementOps.Remove(name)
}

func (o *blockingRollbackOps) AtomicReplace(temp, target, backup string, hadOld bool) error {
	o.backupPath = backup
	return o.replacementOps.AtomicReplace(temp, target, backup, hadOld)
}

func (o *blockingRollbackOps) AtomicRestore(target, backup string) error {
	close(o.entered)
	<-o.release
	return o.replacementOps.AtomicRestore(target, backup)
}

type committedCleanupFailureOps struct {
	replacementOps
	err        error
	backupPath string
}

func (o *committedCleanupFailureOps) Remove(name string) error {
	if strings.Contains(name, replacementBackupPrefix) {
		o.backupPath = name
		return o.err
	}
	return o.replacementOps.Remove(name)
}

type recordingRemoveOps struct {
	replacementOps
	removed []string
}

func (o *recordingRemoveOps) Remove(name string) error {
	o.removed = append(o.removed, name)
	return o.replacementOps.Remove(name)
}

type flakyRemoveOps struct {
	replacementOps
	failPath       string
	failures       int
	err            error
	removeAttempts int
}

func (o *flakyRemoveOps) Remove(name string) error {
	o.removeAttempts++
	if name == o.failPath && o.failures > 0 {
		o.failures--
		return o.err
	}
	return o.replacementOps.Remove(name)
}

type metadataRecordingOps struct {
	infos      map[string]os.FileInfo
	chmodModes []os.FileMode
	onChmod    func()
}

func (o *metadataRecordingOps) Lstat(name string) (os.FileInfo, error) {
	info, ok := o.infos[name]
	if !ok {
		return nil, os.ErrNotExist
	}
	return info, nil
}

func (o *metadataRecordingOps) Chmod(_ string, mode os.FileMode) error {
	if o.onChmod != nil {
		o.onChmod()
	}
	o.chmodModes = append(o.chmodModes, mode)
	return nil
}

func (*metadataRecordingOps) Remove(string) error                              { return nil }
func (*metadataRecordingOps) AtomicReplace(string, string, string, bool) error { return nil }
func (*metadataRecordingOps) AtomicRestore(string, string) error               { return nil }
func (*metadataRecordingOps) AtomicRestoreIfAbsent(string, string) error       { return nil }

type durabilityRecordingOps struct {
	replacementOps
	events              []string
	syncParentCalls     int
	syncParentFailAfter int
	syncParentErr       error
}

func (o *durabilityRecordingOps) Chmod(name string, mode os.FileMode) error {
	o.events = append(o.events, "chmod")
	return o.replacementOps.Chmod(name, mode)
}

func (o *durabilityRecordingOps) AtomicReplace(temp, target, backup string, hadOld bool) error {
	o.events = append(o.events, "replace")
	return o.replacementOps.AtomicReplace(temp, target, backup, hadOld)
}

func (o *durabilityRecordingOps) Remove(name string) error {
	if strings.Contains(name, replacementBackupPrefix) {
		o.events = append(o.events, "remove-backup")
	}
	return o.replacementOps.Remove(name)
}

func (o *durabilityRecordingOps) SyncPath(string) error {
	o.events = append(o.events, "sync-temp")
	return nil
}

func (o *durabilityRecordingOps) SyncParent(string) error {
	o.events = append(o.events, "sync-parent")
	o.syncParentCalls++
	if o.syncParentFailAfter > 0 && o.syncParentCalls >= o.syncParentFailAfter {
		return o.syncParentErr
	}
	return nil
}

type stubFileInfo struct {
	mode os.FileMode
}

func (i stubFileInfo) Name() string       { return "target" }
func (i stubFileInfo) Size() int64        { return 0 }
func (i stubFileInfo) Mode() os.FileMode  { return i.mode }
func (i stubFileInfo) ModTime() time.Time { return time.Time{} }
func (i stubFileInfo) IsDir() bool        { return i.mode.IsDir() }
func (i stubFileInfo) Sys() any           { return nil }

func equalStringSlices(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}

func TestLocal_PublishFailureRestoresStrandedBackup(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "target")
	if err := os.WriteFile(path, []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}

	w, err := New().Create(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	localWriter := w.(*writer)
	publishErr := errors.New("injected publish failure")
	localWriter.ops = &strandedBackupOps{replacementOps: localWriter.ops, err: publishErr}
	if _, err := w.Write([]byte("new")); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); !errors.Is(err, publishErr) {
		t.Fatalf("Close error = %v, want %v", err, publishErr)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("destination missing after failed publish: %v", err)
	}
	if string(got) != "old" {
		t.Fatalf("target contents = %q, want old", got)
	}
	if err := w.(storage.Aborter).Abort(); err != nil {
		t.Fatal(err)
	}
}

func TestLocal_RetriedCloseAfterPublishFailureRepublishesStagedData(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "target")
	if err := os.WriteFile(path, []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}

	w, err := New().Create(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	localWriter := w.(*writer)
	publishErr := errors.New("injected publish failure")
	localWriter.ops = &onceFailingReplaceOps{replacementOps: localWriter.ops, err: publishErr}
	if _, err := w.Write([]byte("new")); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); !errors.Is(err, publishErr) {
		t.Fatalf("Close error = %v, want %v", err, publishErr)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("retried Close error = %v, want the staged write to be republished", err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "new" {
		t.Fatalf("target contents = %q, want new", got)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != filepath.Base(path) {
		t.Fatalf("directory entries = %v, want only the published target", entries)
	}
}

func TestLocal_PublishFailureRetainsBackupWhenRestoreFails(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "target")
	if err := os.WriteFile(path, []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}

	w, err := New().Create(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	localWriter := w.(*writer)
	publishErr := errors.New("injected publish failure")
	restoreErr := errors.New("injected restore failure")
	ops := &strandedBackupOps{replacementOps: localWriter.ops, err: publishErr, restoreErr: restoreErr}
	localWriter.ops = ops
	if _, err := w.Write([]byte("new")); err != nil {
		t.Fatal(err)
	}
	err = w.Close()
	if !errors.Is(err, publishErr) || !errors.Is(err, restoreErr) {
		t.Fatalf("Close error = %v, want join of %v and %v", err, publishErr, restoreErr)
	}
	if ops.backup == "" {
		t.Fatal("no replacement backup recorded")
	}
	got, err := os.ReadFile(ops.backup)
	if err != nil {
		t.Fatalf("replacement backup not retained for recovery: %v", err)
	}
	if string(got) != "old" {
		t.Fatalf("retained backup contents = %q, want old", got)
	}
	if err := w.(storage.Aborter).Abort(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(ops.backup); err != nil {
		t.Fatalf("abort must not discard the retained backup: %v", err)
	}
}

func TestLocal_PublishFailureDoesNotOverwriteConcurrentRestoreTarget(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "target")
	if err := os.WriteFile(path, []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}

	w, err := New().Create(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	localWriter := w.(*writer)
	publishErr := errors.New("injected publish failure")
	ops := &strandedBackupOps{
		replacementOps:         localWriter.ops,
		err:                    publishErr,
		restoreConcurrentBytes: []byte("concurrent"),
	}
	localWriter.ops = ops
	if _, err := w.Write([]byte("new")); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); !errors.Is(err, publishErr) {
		t.Fatalf("Close error = %v, want %v", err, publishErr)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "concurrent" {
		t.Fatalf("concurrent destination contents = %q, want concurrent", got)
	}
	got, err = os.ReadFile(ops.backup)
	if err != nil {
		t.Fatalf("backup was not retained: %v", err)
	}
	if string(got) != "old" {
		t.Fatalf("backup contents = %q, want old", got)
	}
}

func TestLocal_PublishFailureDiscardsBackupWhenOriginalTargetIsIntact(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "target")
	if err := os.WriteFile(path, []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}

	w, err := New().Create(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	localWriter := w.(*writer)
	publishErr := errors.New("injected publish failure")
	ops := &linkedBackupPublishFailureOps{replacementOps: localWriter.ops, err: publishErr}
	localWriter.ops = ops
	if _, err := w.Write([]byte("new")); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); !errors.Is(err, publishErr) {
		t.Fatalf("Close error = %v, want %v", err, publishErr)
	}
	if ops.backup == "" {
		t.Fatal("no replacement backup recorded")
	}
	if _, err := os.Lstat(ops.backup); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("replacement backup %s still exists: %v", ops.backup, err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "old" {
		t.Fatalf("target contents = %q, want old", got)
	}
}

func TestLocal_PublishFailureRetainsBackupWhenPublishedReplacementIsInstalled(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "target")
	if err := os.WriteFile(path, []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}

	w, err := New().Create(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	localWriter := w.(*writer)
	publishErr := errors.New("injected publish failure")
	ops := &publishedReplacementErrorOps{replacementOps: localWriter.ops, err: publishErr}
	localWriter.ops = ops
	if _, err := w.Write([]byte("new")); err != nil {
		t.Fatal(err)
	}
	err = w.Close()
	if !errors.Is(err, publishErr) || !strings.Contains(err.Error(), "backup retained for recovery") {
		t.Fatalf("Close error = %v, want publish failure with retained backup", err)
	}
	if ops.backup == "" {
		t.Fatal("no replacement backup recorded")
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "new" {
		t.Fatalf("target contents = %q, want new", got)
	}
	backup, err := os.ReadFile(ops.backup)
	if err != nil {
		t.Fatalf("replacement backup missing: %v", err)
	}
	if string(backup) != "old" {
		t.Fatalf("backup contents = %q, want old", backup)
	}
}

func TestLocal_PublishFailureRetainsBackupWhenDestinationChanged(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "target")
	if err := os.WriteFile(path, []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}

	w, err := New().Create(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	localWriter := w.(*writer)
	publishErr := errors.New("injected publish failure")
	ops := &strandedBackupOps{
		replacementOps:  localWriter.ops,
		err:             publishErr,
		concurrentBytes: []byte("concurrent"),
	}
	localWriter.ops = ops
	if _, err := w.Write([]byte("new")); err != nil {
		t.Fatal(err)
	}
	err = w.Close()
	if !errors.Is(err, publishErr) || !strings.Contains(err.Error(), "backup retained for recovery") {
		t.Fatalf("Close error = %v, want publish failure with retained backup", err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "concurrent" {
		t.Fatalf("target contents = %q, want concurrent", got)
	}
	backup, err := os.ReadFile(ops.backup)
	if err != nil {
		t.Fatalf("replacement backup missing: %v", err)
	}
	if string(backup) != "old" {
		t.Fatalf("backup contents = %q, want old", backup)
	}
}

func TestLocal_PublishFailureUsesContentFallbackWhenPhysicalIdentityUnavailable(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "target")
	if err := os.WriteFile(path, []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}

	w, err := New().Create(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	localWriter := w.(*writer)
	publishErr := errors.New("injected publish failure")
	ops := &fallbackIdentityPublishFailureOps{replacementOps: localWriter.ops, err: publishErr}
	localWriter.ops = ops
	originalSameFile := sameReplacementFile
	sameReplacementFile = func(os.FileInfo, os.FileInfo) bool { return false }
	defer func() { sameReplacementFile = originalSameFile }()

	if _, err := w.Write([]byte("new")); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); !errors.Is(err, publishErr) {
		t.Fatalf("Close error = %v, want %v", err, publishErr)
	}
	if ops.backup == "" {
		t.Fatal("no replacement backup recorded")
	}
	if _, err := os.Lstat(ops.backup); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("replacement backup %s still exists: %v", ops.backup, err)
	}
}

func legacyLocalCreateReplacesSymlinkReferentsAndRetainsLinks(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name  string
		setup func(*testing.T, string) (string, string, map[string]string)
	}{
		{
			name: "absolute",
			setup: func(t *testing.T, dir string) (string, string, map[string]string) {
				dataDir := filepath.Join(dir, "data")
				linkDir := filepath.Join(dir, "links")
				if err := os.MkdirAll(dataDir, 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.MkdirAll(linkDir, 0o755); err != nil {
					t.Fatal(err)
				}
				target := filepath.Join(dataDir, "target.rf")
				link := filepath.Join(linkDir, "absolute.rf")
				if err := os.WriteFile(target, []byte("old"), 0o600); err != nil {
					t.Fatal(err)
				}
				createFileSymlinkOrSkip(t, target, link)
				return link, target, map[string]string{link: target}
			},
		},
		{
			name: "relative",
			setup: func(t *testing.T, dir string) (string, string, map[string]string) {
				target := filepath.Join(dir, "target.rf")
				link := filepath.Join(dir, "relative.rf")
				if err := os.WriteFile(target, []byte("old"), 0o600); err != nil {
					t.Fatal(err)
				}
				createFileSymlinkOrSkip(t, filepath.Base(target), link)
				return link, target, map[string]string{link: filepath.Base(target)}
			},
		},
		{
			name: "chained",
			setup: func(t *testing.T, dir string) (string, string, map[string]string) {
				target := filepath.Join(dir, "target.rf")
				mid := filepath.Join(dir, "mid.rf")
				link := filepath.Join(dir, "top.rf")
				if err := os.WriteFile(target, []byte("old"), 0o600); err != nil {
					t.Fatal(err)
				}
				createFileSymlinkOrSkip(t, filepath.Base(target), mid)
				createFileSymlinkOrSkip(t, filepath.Base(mid), link)
				return link, target, map[string]string{
					mid:  filepath.Base(target),
					link: filepath.Base(mid),
				}
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			link, target, links := tc.setup(t, dir)
			originalInfo, err := os.Stat(target)
			if err != nil {
				t.Fatal(err)
			}

			w, err := New().Create(context.Background(), link)
			if err != nil {
				t.Fatal(err)
			}
			localWriter := w.(*writer)
			if got, want := filepath.Dir(localWriter.temp), filepath.Dir(target); got != want {
				t.Fatalf("temporary file dir = %q, want referent dir %q", got, want)
			}
			if got, want := localWriter.target, target; got != want {
				t.Fatalf("resolved target = %q, want %q", got, want)
			}
			if _, err := w.Write([]byte("new")); err != nil {
				t.Fatal(err)
			}
			if err := w.Close(); err != nil {
				t.Fatal(err)
			}

			got, err := os.ReadFile(target)
			if err != nil {
				t.Fatal(err)
			}
			if string(got) != "new" {
				t.Fatalf("referent contents = %q, want new", got)
			}
			replacedInfo, err := os.Stat(target)
			if err != nil {
				t.Fatal(err)
			}
			if got, want := replacedInfo.Mode().Perm(), originalInfo.Mode().Perm(); got != want {
				t.Fatalf("referent mode = %04o, want %04o", got, want)
			}
			for linkPath, wantTarget := range links {
				assertSymlinkTarget(t, linkPath, wantTarget)
			}
			assertLocalOpenBytes(t, link, []byte("new"))
			listed, err := New().List(context.Background(), filepath.Dir(link))
			if err != nil {
				t.Fatal(err)
			}
			if !slices.Contains(listed, link) {
				t.Fatalf("List(%q) missing symlink path %q from %v", filepath.Dir(link), link, listed)
			}
			assertNoReplacementArtifacts(t, filepath.Dir(target))
		})
	}
}

func TestLocal_CreateRejectsExistingSymlinkDestination(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "target.rf")
	link := filepath.Join(dir, "link.rf")
	if err := os.WriteFile(target, []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	createFileSymlinkOrSkip(t, filepath.Base(target), link)

	w, err := New().Create(context.Background(), link)
	if err == nil || !strings.Contains(err.Error(), "symlink destination") {
		if w != nil {
			_ = w.(storage.Aborter).Abort()
		}
		t.Fatalf("Create(%q) error = %v, want atomic symlink rejection", link, err)
	}
	assertLocalOpenBytes(t, link, []byte("old"))
	assertSymlinkTarget(t, link, filepath.Base(target))
	assertNoReplacementArtifacts(t, dir)
}

func TestLocal_CreateRejectsInvalidSymlinkReferents(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name    string
		setup   func(*testing.T, string) string
		wantErr string
	}{
		{
			name: "dangling",
			setup: func(t *testing.T, dir string) string {
				link := filepath.Join(dir, "dangling.rf")
				createFileSymlinkOrSkip(t, "missing.rf", link)
				return link
			},
			wantErr: "symlink destination",
		},
		{
			name: "loop",
			setup: func(t *testing.T, dir string) string {
				first := filepath.Join(dir, "first.rf")
				second := filepath.Join(dir, "second.rf")
				createFileSymlinkOrSkip(t, filepath.Base(second), first)
				createFileSymlinkOrSkip(t, filepath.Base(first), second)
				return first
			},
			wantErr: "symlink destination",
		},
		{
			name: "nonregular",
			setup: func(t *testing.T, dir string) string {
				targetDir := filepath.Join(dir, "target-dir")
				link := filepath.Join(dir, "dir-link")
				if err := os.Mkdir(targetDir, 0o755); err != nil {
					t.Fatal(err)
				}
				createFileSymlinkOrSkip(t, filepath.Base(targetDir), link)
				return link
			},
			wantErr: "symlink destination",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			link := tc.setup(t, dir)
			w, err := New().Create(context.Background(), link)
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				if w != nil {
					_ = w.(storage.Aborter).Abort()
				}
				t.Fatalf("Create(%q) error = %v, want substring %q", link, err, tc.wantErr)
			}
			info, statErr := os.Lstat(link)
			if statErr != nil || info.Mode()&os.ModeSymlink == 0 {
				t.Fatalf("symlink changed after rejection: info=%v err=%v", info, statErr)
			}
		})
	}
}

func legacyLocalCreateAbortsWhenSymlinkRetargetedBeforeClose(t *testing.T) {
	dir := t.TempDir()
	dataDir := filepath.Join(dir, "data")
	linkDir := filepath.Join(dir, "links")
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(linkDir, 0o755); err != nil {
		t.Fatal(err)
	}
	oldTarget := filepath.Join(dataDir, "old.rf")
	newTarget := filepath.Join(dataDir, "new.rf")
	link := filepath.Join(linkDir, "current.rf")
	if err := os.WriteFile(oldTarget, []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(newTarget, []byte("other"), 0o600); err != nil {
		t.Fatal(err)
	}
	createFileSymlinkOrSkip(t, oldTarget, link)

	w, err := New().Create(context.Background(), link)
	if err != nil {
		t.Fatal(err)
	}
	localWriter := w.(*writer)
	if _, err := w.Write([]byte("new")); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(link); err != nil {
		t.Fatal(err)
	}
	createFileSymlinkOrSkip(t, newTarget, link)

	err = w.Close()
	if err == nil || !strings.Contains(err.Error(), "changed before publish") {
		t.Fatalf("Close error = %v, want symlink-retarget rejection", err)
	}
	if got, readErr := os.ReadFile(oldTarget); readErr != nil || string(got) != "old" {
		t.Fatalf("old referent = %q, %v; want old", got, readErr)
	}
	if got, readErr := os.ReadFile(newTarget); readErr != nil || string(got) != "other" {
		t.Fatalf("new referent = %q, %v; want other", got, readErr)
	}
	assertSymlinkTarget(t, link, newTarget)
	if err := w.(storage.Aborter).Abort(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(localWriter.temp); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("temporary file %s still exists: %v", localWriter.temp, err)
	}
	assertNoReplacementArtifacts(t, filepath.Dir(oldTarget))
}

func legacyLocalCreateAbortsWhenSymlinkReferentChangesBeforeClose(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "target.rf")
	previous := filepath.Join(dir, "previous.rf")
	link := filepath.Join(dir, "current.rf")
	if err := os.WriteFile(target, []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	createFileSymlinkOrSkip(t, filepath.Base(target), link)

	w, err := New().Create(context.Background(), link)
	if err != nil {
		t.Fatal(err)
	}
	localWriter := w.(*writer)
	if _, err := w.Write([]byte("new")); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(target, previous); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(target, []byte("concurrent"), 0o600); err != nil {
		t.Fatal(err)
	}

	err = w.Close()
	if err == nil || !strings.Contains(err.Error(), "referent") {
		t.Fatalf("Close error = %v, want referent-change rejection", err)
	}
	if got, readErr := os.ReadFile(target); readErr != nil || string(got) != "concurrent" {
		t.Fatalf("current referent = %q, %v; want concurrent", got, readErr)
	}
	if got, readErr := os.ReadFile(previous); readErr != nil || string(got) != "old" {
		t.Fatalf("previous referent = %q, %v; want old", got, readErr)
	}
	assertSymlinkTarget(t, link, filepath.Base(target))
	if err := w.(storage.Aborter).Abort(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(localWriter.temp); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("temporary file %s still exists: %v", localWriter.temp, err)
	}
	assertNoReplacementArtifacts(t, dir)
}

func legacyLocalAbortRemovesSymlinkArtifactsFromReferentDir(t *testing.T) {
	dir := t.TempDir()
	dataDir := filepath.Join(dir, "data")
	linkDir := filepath.Join(dir, "links")
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(linkDir, 0o755); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(dataDir, "target.rf")
	link := filepath.Join(linkDir, "target.rf")
	if err := os.WriteFile(target, []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	createFileSymlinkOrSkip(t, target, link)

	w, err := New().Create(context.Background(), link)
	if err != nil {
		t.Fatal(err)
	}
	localWriter := w.(*writer)
	if got, want := filepath.Dir(localWriter.temp), dataDir; got != want {
		t.Fatalf("temporary dir = %q, want referent dir %q", got, want)
	}
	if _, err := w.Write([]byte("new")); err != nil {
		t.Fatal(err)
	}
	if err := w.(storage.Aborter).Abort(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(localWriter.temp); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("temporary file %s still exists: %v", localWriter.temp, err)
	}
	assertSymlinkTarget(t, link, target)
	assertNoReplacementArtifacts(t, dataDir)
}

// strandedBackupOps mimics the rename fallback failing after the old target has
// already been moved aside, leaving the backup as the only copy.
type onceFailingReplaceOps struct {
	replacementOps
	err   error
	calls int
}

func (o *onceFailingReplaceOps) AtomicReplace(temp, target, backup string, hadOld bool) error {
	o.calls++
	if o.calls > 1 {
		return o.replacementOps.AtomicReplace(temp, target, backup, hadOld)
	}
	if hadOld {
		if err := os.Rename(target, backup); err != nil {
			return err
		}
	}
	return o.err
}

type strandedBackupOps struct {
	replacementOps
	err                    error
	restoreErr             error
	backup                 string
	concurrentBytes        []byte
	restoreConcurrentBytes []byte
}

func (o *strandedBackupOps) AtomicReplace(temp, target, backup string, hadOld bool) error {
	if !hadOld {
		return o.replacementOps.AtomicReplace(temp, target, backup, hadOld)
	}
	if err := os.Rename(target, backup); err != nil {
		return err
	}
	o.backup = backup
	if o.concurrentBytes != nil {
		if err := os.WriteFile(target, o.concurrentBytes, 0o600); err != nil {
			return err
		}
	}
	return o.err
}

func (o *strandedBackupOps) AtomicRestore(target, backup string) error {
	if o.restoreErr != nil {
		return o.restoreErr
	}
	return o.replacementOps.AtomicRestore(target, backup)
}

func (o *strandedBackupOps) AtomicRestoreIfAbsent(target, backup string) error {
	if o.restoreErr != nil {
		return o.restoreErr
	}
	if o.restoreConcurrentBytes != nil {
		if err := os.WriteFile(target, o.restoreConcurrentBytes, 0o600); err != nil {
			return err
		}
		o.restoreConcurrentBytes = nil
	}
	return o.replacementOps.AtomicRestoreIfAbsent(target, backup)
}

type linkedBackupPublishFailureOps struct {
	replacementOps
	err    error
	backup string
}

func (o *linkedBackupPublishFailureOps) AtomicReplace(temp, target, backup string, hadOld bool) error {
	if !hadOld {
		return o.replacementOps.AtomicReplace(temp, target, backup, hadOld)
	}
	if err := os.Link(target, backup); err != nil {
		return err
	}
	o.backup = backup
	return o.err
}

type publishedReplacementErrorOps struct {
	replacementOps
	err    error
	backup string
}

func (o *publishedReplacementErrorOps) AtomicReplace(temp, target, backup string, hadOld bool) error {
	if !hadOld {
		return o.replacementOps.AtomicReplace(temp, target, backup, hadOld)
	}
	if err := os.Rename(target, backup); err != nil {
		return err
	}
	o.backup = backup
	if err := os.Rename(temp, target); err != nil {
		return err
	}
	return o.err
}

type fallbackIdentityPublishFailureOps struct {
	replacementOps
	err    error
	backup string
}

func (o *fallbackIdentityPublishFailureOps) AtomicReplace(temp, target, backup string, hadOld bool) error {
	if !hadOld {
		return o.replacementOps.AtomicReplace(temp, target, backup, hadOld)
	}
	info, err := os.Lstat(target)
	if err != nil {
		return err
	}
	body, err := os.ReadFile(target)
	if err != nil {
		return err
	}
	if err := os.WriteFile(backup, body, info.Mode()&os.ModePerm); err != nil {
		return err
	}
	o.backup = backup
	return o.err
}

func (o *fallbackIdentityPublishFailureOps) Lstat(name string) (os.FileInfo, error) {
	info, err := o.replacementOps.Lstat(name)
	if err != nil {
		return nil, err
	}
	if name == o.backup {
		return noSysFileInfo{FileInfo: info}, nil
	}
	return noSysFileInfo{FileInfo: info}, nil
}

type noSysFileInfo struct{ os.FileInfo }

func (i noSysFileInfo) Sys() any { return nil }

func createFileSymlinkOrSkip(t *testing.T, target, link string) {
	t.Helper()
	if err := os.Symlink(target, link); err != nil {
		if runtime.GOOS == "windows" {
			t.Skipf("symlink creation unavailable: %v", err)
		}
		t.Fatalf("Symlink(%q, %q): %v", target, link, err)
	}
}

func assertSymlinkTarget(t *testing.T, link, want string) {
	t.Helper()
	info, err := os.Lstat(link)
	if err != nil {
		t.Fatalf("Lstat(%q): %v", link, err)
	}
	if info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("%q mode = %v, want symlink", link, info.Mode())
	}
	got, err := os.Readlink(link)
	if err != nil {
		t.Fatalf("Readlink(%q): %v", link, err)
	}
	if got != want {
		t.Fatalf("Readlink(%q) = %q, want %q", link, got, want)
	}
}

func assertLocalOpenBytes(t *testing.T, path string, want []byte) {
	t.Helper()
	f, err := New().Open(context.Background(), path)
	if err != nil {
		t.Fatalf("Open(%q): %v", path, err)
	}
	defer f.Close()
	got := make([]byte, f.Size())
	if _, err := f.ReadAt(got, 0); err != nil {
		t.Fatalf("ReadAt(%q): %v", path, err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("Open(%q) = %q, want %q", path, got, want)
	}
}

func assertNoReplacementArtifacts(t *testing.T, dir string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir(%q): %v", dir, err)
	}
	for _, entry := range entries {
		name := entry.Name()
		if strings.HasPrefix(name, replacementTempPrefix) || strings.HasPrefix(name, replacementBackupPrefix) {
			t.Fatalf("unexpected replacement artifact %q in %s", name, dir)
		}
	}
}
