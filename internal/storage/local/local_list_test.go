package local

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestLocal_ListHidesGeneratedReplacementArtifacts(t *testing.T) {
	dir := t.TempDir()
	files := []string{
		"1.rf",
		".shl-user-visible",
		".shl-aaaaaaaaa",
		replacementTempPrefix + "visible",
		replacementBackupPrefix + "visible",
		replacementTempPrefix + strings.Repeat("A", replacementNameTokenBytes*2),
		replacementBackupPrefix + strings.Repeat("B", replacementNameTokenBytes*2),
		replacementTempPrefix + strings.Repeat("c", replacementNameTokenBytes*2+1),
		replacementBackupPrefix + strings.Repeat("d", replacementNameTokenBytes*2+1),
		replacementTempPrefix + strings.Repeat("a", replacementNameTokenBytes*2),
		replacementBackupPrefix + strings.Repeat("b", replacementNameTokenBytes*2),
	}
	for _, name := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(name), 0o644); err != nil {
			t.Fatalf("WriteFile(%q): %v", name, err)
		}
	}

	got, err := New().List(context.Background(), dir)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	want := []string{
		filepath.Join(dir, "1.rf"),
		filepath.Join(dir, ".shl-user-visible"),
		filepath.Join(dir, ".shl-aaaaaaaaa"),
		filepath.Join(dir, replacementTempPrefix+"visible"),
		filepath.Join(dir, replacementBackupPrefix+"visible"),
		filepath.Join(dir, replacementTempPrefix+strings.Repeat("A", replacementNameTokenBytes*2)),
		filepath.Join(dir, replacementBackupPrefix+strings.Repeat("B", replacementNameTokenBytes*2)),
		filepath.Join(dir, replacementTempPrefix+strings.Repeat("c", replacementNameTokenBytes*2+1)),
		filepath.Join(dir, replacementBackupPrefix+strings.Repeat("d", replacementNameTokenBytes*2+1)),
	}
	slices.Sort(got)
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Fatalf("List = %v, want %v", got, want)
	}
}

func TestLocal_CreateRejectsReservedReplacementArtifactNames(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{
		replacementTempPrefix + strings.Repeat("a", replacementNameTokenBytes*2),
		replacementBackupPrefix + strings.Repeat("b", replacementNameTokenBytes*2),
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := New().Create(context.Background(), filepath.Join(dir, name)); err == nil || !strings.Contains(err.Error(), "reserved internal namespace") {
				t.Fatalf("Create(%q) error = %v, want reserved internal namespace rejection", name, err)
			}
		})
	}
}

func TestLocal_CreateAllowsUserNamesOutsideReservedNamespace(t *testing.T) {
	dir := t.TempDir()
	names := []string{
		".shl-final.rf",
		".shl-aaaaaaaaa",
		replacementTempPrefix + strings.Repeat("A", replacementNameTokenBytes*2),
		replacementBackupPrefix + strings.Repeat("B", replacementNameTokenBytes*2),
		replacementTempPrefix + strings.Repeat("e", replacementNameTokenBytes*2+1),
		replacementBackupPrefix + strings.Repeat("f", replacementNameTokenBytes*2+1),
		filepath.Join("nested", ".shl-final.rf"),
	}
	for _, name := range names {
		t.Run(name, func(t *testing.T) {
			fullPath := filepath.Join(dir, name)
			w, err := New().Create(context.Background(), fullPath)
			if err != nil {
				t.Fatalf("Create(%q): %v", name, err)
			}
			if _, err := w.Write([]byte("data")); err != nil {
				t.Fatalf("Write(%q): %v", name, err)
			}
			if err := w.Close(); err != nil {
				t.Fatalf("Close(%q): %v", name, err)
			}
			got, err := os.ReadFile(fullPath)
			if err != nil {
				t.Fatalf("ReadFile(%q): %v", name, err)
			}
			if string(got) != "data" {
				t.Fatalf("file content for %q = %q, want data", name, string(got))
			}
		})
	}

	listed, err := New().List(context.Background(), dir)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	for _, name := range names {
		if filepath.Dir(name) != "." {
			continue
		}
		fullPath := filepath.Join(dir, name)
		if !slices.Contains(listed, fullPath) {
			t.Fatalf("List missing user-visible path %q from %v", fullPath, listed)
		}
	}
}
