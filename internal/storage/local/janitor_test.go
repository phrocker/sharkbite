package local

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestCleanupStaleArtifactsRemovesOnlyOldReservedFiles(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()
	oldTemp := filepath.Join(dir, replacementTempPrefix+"00112233445566778899aabbccddeeff")
	oldBackup := filepath.Join(dir, replacementBackupPrefix+"ffeeddccbbaa99887766554433221100")
	recent := filepath.Join(dir, replacementTempPrefix+"11112222333344445555666677778888")
	lookalike := filepath.Join(dir, replacementTempPrefix+"not-a-token")
	uppercase := filepath.Join(dir, replacementTempPrefix+"AABBCCDDEEFF00112233445566778899")
	for _, name := range []string{oldTemp, oldBackup, recent, lookalike, uppercase} {
		if err := os.WriteFile(name, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	old := now.Add(-2 * time.Hour)
	for _, name := range []string{oldTemp, oldBackup, lookalike, uppercase} {
		if err := os.Chtimes(name, old, old); err != nil {
			t.Fatal(err)
		}
	}

	result, err := New().CleanupStaleArtifacts(context.Background(), dir, now.Add(-time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if result.Examined != 3 {
		t.Fatalf("Examined = %d, want 3", result.Examined)
	}
	if len(result.Removed) != 1 || result.Removed[0] != oldTemp {
		t.Fatalf("Removed = %v, want old temporary artifact", result.Removed)
	}
	if len(result.Recoverable) != 1 || result.Recoverable[0] != oldBackup {
		t.Fatalf("Recoverable = %v, want [%s]", result.Recoverable, oldBackup)
	}
	if _, err := os.Stat(oldTemp); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("%s still exists: %v", oldTemp, err)
	}
	for _, name := range []string{oldBackup, recent, lookalike, uppercase} {
		if _, err := os.Stat(name); err != nil {
			t.Fatalf("%s was removed: %v", name, err)
		}
	}
	if err := os.Remove(recent); err != nil {
		t.Fatal(err)
	}
	result, err = New().CleanupStaleArtifacts(context.Background(), dir, now.Add(-time.Hour))
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

func TestCleanupStaleArtifactsPreservesActiveWriter(t *testing.T) {
	dir := t.TempDir()
	out, err := New().Create(context.Background(), filepath.Join(dir, "target"))
	if err != nil {
		t.Fatal(err)
	}
	w := out.(*writer)
	if _, err := w.Write([]byte("active")); err != nil {
		t.Fatal(err)
	}
	result, err := New().CleanupStaleArtifacts(context.Background(), dir, time.Now().Add(-time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Removed) != 0 {
		t.Fatalf("removed active writer artifacts: %v", result.Removed)
	}
	if _, err := os.Stat(w.temp); err != nil {
		t.Fatalf("active temporary file missing: %v", err)
	}
	if err := w.Abort(); err != nil {
		t.Fatal(err)
	}
}

func TestCleanupStaleArtifactsReportsPartialFailuresAndCancellation(t *testing.T) {
	dir := t.TempDir()
	first := filepath.Join(dir, replacementTempPrefix+"00112233445566778899aabbccddeeff")
	second := filepath.Join(dir, replacementTempPrefix+"11112222333344445555666677778888")
	backup := filepath.Join(dir, replacementBackupPrefix+"ffeeddccbbaa99887766554433221100")
	old := time.Now().Add(-2 * time.Hour)
	for _, name := range []string{first, second, backup} {
		if err := os.WriteFile(name, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(name, old, old); err != nil {
			t.Fatal(err)
		}
	}
	removeErr := errors.New("remove failed")
	result, err := cleanupStaleArtifacts(context.Background(), dir, time.Now().Add(-time.Hour), os.ReadDir, func(name string) error {
		if name == second {
			return removeErr
		}
		return os.Remove(name)
	})
	if !errors.Is(err, removeErr) {
		t.Fatalf("error = %v, want remove failure", err)
	}
	if len(result.Removed) != 1 || result.Removed[0] != first {
		t.Fatalf("Removed = %v, want [%s]", result.Removed, first)
	}
	if len(result.Recoverable) != 1 || result.Recoverable[0] != backup {
		t.Fatalf("Recoverable = %v, want [%s]", result.Recoverable, backup)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := New().CleanupStaleArtifacts(ctx, dir, time.Now()); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled cleanup error = %v, want context.Canceled", err)
	}
}
