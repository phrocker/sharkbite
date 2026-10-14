package engine

import (
	"path/filepath"
	"testing"

	"github.com/phrocker/shoal-oss/internal/storage"
	"github.com/phrocker/shoal-oss/internal/storage/local"
	"github.com/phrocker/shoal-oss/internal/storage/memory"
)

type schemeAwareBackend struct {
	storage.Backend
	schemes []string
}

func (b schemeAwareBackend) BackendPathSchemes() []string {
	return b.schemes
}

func TestJoinBackendPathPreservesCustomSchemeRoots(t *testing.T) {
	got := joinBackendPath(memory.New(), "custom+backend://bucket/prefix", "nested/F0001.rf")
	if got != "custom+backend://bucket/prefix/nested/F0001.rf" {
		t.Fatalf("joinBackendPath(custom scheme) = %q, want %q", got, "custom+backend://bucket/prefix/nested/F0001.rf")
	}
}

func TestJoinBackendPathTreatsAuthoritylessHDFSRootCaseInsensitively(t *testing.T) {
	got := joinBackendPath(memory.New(), "HDFS:/bulk", "nested/F0001.rf")
	if got != "HDFS:/bulk/nested/F0001.rf" {
		t.Fatalf("joinBackendPath(HDFS:/bulk) = %q, want %q", got, "HDFS:/bulk/nested/F0001.rf")
	}
}

func TestJoinBackendPathTreatsWindowsDriveRootsAsLocal(t *testing.T) {
	got := joinBackendPath(local.New(), `C://bulk`, "F0001.rf")
	want := filepath.Join(`C://bulk`, "F0001.rf")
	if got != want {
		t.Fatalf("joinBackendPath(%q, %q) = %q, want %q", `C://bulk`, "F0001.rf", got, want)
	}
	if got == `C://bulk/F0001.rf` {
		t.Fatalf("joinBackendPath treated %q as a backend URL root", `C://bulk`)
	}
}

func TestJoinBackendPathPreservesDeclaredSingleCharacterSchemeRoot(t *testing.T) {
	backend := schemeAwareBackend{Backend: memory.New(), schemes: []string{"x"}}
	got := joinBackendPath(backend, "x://bucket/prefix", "nested/F0001.rf")
	if got != "x://bucket/prefix/nested/F0001.rf" {
		t.Fatalf("joinBackendPath(single-char scheme) = %q, want %q", got, "x://bucket/prefix/nested/F0001.rf")
	}
}
