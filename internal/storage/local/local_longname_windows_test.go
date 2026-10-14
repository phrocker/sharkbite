//go:build windows

package local

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf16"
)

func TestLocal_WindowsBoundaryNamesCreateAndReplace(t *testing.T) {
	dir := t.TempDir()
	const maxLegacyPath = 259
	basenameLength := min(255, maxLegacyPath-len(filepath.Clean(dir))-1)
	if basenameLength < 64 {
		t.Skip("temporary directory leaves insufficient room for a boundary path")
	}
	path := filepath.Join(dir, strings.Repeat("x", basenameLength))

	for _, prefix := range []string{replacementTempPrefix, replacementBackupPrefix} {
		component := prefix + strings.Repeat("0", replacementNameTokenBytes*2)
		if got := len(utf16.Encode([]rune(component))); got > 255 {
			t.Fatalf("sibling component length = %d, want <= 255", got)
		}
	}

	w, err := New().Create(context.Background(), path)
	if err != nil {
		t.Fatalf("Create boundary basename: %v", err)
	}
	if _, err := w.Write([]byte("first")); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("Close boundary basename create: %v", err)
	}

	w, err = New().Create(context.Background(), path)
	if err != nil {
		t.Fatalf("Create boundary replacement: %v", err)
	}
	if _, err := w.Write([]byte("second")); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("Close boundary replacement: %v", err)
	}
	if got, err := os.ReadFile(path); err != nil || string(got) != "second" {
		t.Fatalf("replacement contents = %q, %v; want second", got, err)
	}
	if matches, err := filepath.Glob(filepath.Join(dir, ".shoal-*")); err != nil {
		t.Fatal(err)
	} else if len(matches) != 0 {
		t.Fatalf("replacement artifacts remain: %v", matches)
	}
}
