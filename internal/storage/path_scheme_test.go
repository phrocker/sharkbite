package storage

import (
	"context"
	"testing"
)

type localSemanticBackend struct{}

func (localSemanticBackend) Open(context.Context, string) (File, error) { return nil, ErrNotFound }
func (localSemanticBackend) UsesLocalPathSemantics() bool               { return true }

func TestExplicitPathSchemeRecognizesAuthoritylessHDFSCaseInsensitively(t *testing.T) {
	if got := ExplicitPathScheme(nil, "HDFS:/bulk"); got != "hdfs" {
		t.Fatalf("ExplicitPathScheme(HDFS:/bulk) = %q, want %q", got, "hdfs")
	}
	if !UsesBackendPathJoin(nil, "HDFS:/bulk") {
		t.Fatal("UsesBackendPathJoin(HDFS:/bulk) = false, want true")
	}
	if !UsesBackendPathJoin(nil, "HDFS:/") {
		t.Fatal("UsesBackendPathJoin(HDFS:/) = false, want true")
	}
}

func TestExplicitPathSchemePreservesLocalAndGenericOverrides(t *testing.T) {
	if got := ExplicitPathScheme(nil, `C://bulk`); got != "" {
		t.Fatalf("ExplicitPathScheme(C://bulk) = %q, want empty for a Windows-style local drive path", got)
	}
	if got := ExplicitPathScheme(nil, "memory://bucket"); got != "memory" {
		t.Fatalf("ExplicitPathScheme(memory://bucket) = %q, want %q", got, "memory")
	}
	if got := ExplicitPathScheme(localSemanticBackend{}, "HDFS:/bulk"); got != "" {
		t.Fatalf("ExplicitPathScheme(local semantic backend, HDFS:/bulk) = %q, want empty", got)
	}
	if UsesBackendPathJoin(localSemanticBackend{}, "HDFS:/bulk") {
		t.Fatal("UsesBackendPathJoin(local semantic backend, HDFS:/bulk) = true, want false")
	}
}
