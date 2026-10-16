package storage_test

import (
	"context"
	"errors"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/phrocker/shoal-oss/internal/storage"
	"github.com/phrocker/shoal-oss/internal/storage/memory"
)

func TestRooted_MapsPathsUnderLocalDir(t *testing.T) {
	dir := t.TempDir()
	mb := memory.New()
	r, err := storage.NewRooted(mb, dir, "bucket/prefix/")
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	p := filepath.Join(dir, "graph", "t-0000", "0001.rf")
	if err := storage.WriteAll(ctx, r, p, []byte("rf")); err != nil {
		t.Fatal(err)
	}
	if got := mb.Keys(); len(got) != 1 || got[0] != "bucket/prefix/graph/t-0000/0001.rf" {
		t.Fatalf("memory keys = %v", got)
	}
	data, err := storage.ReadAll(ctx, r, p)
	if err != nil || string(data) != "rf" {
		t.Fatalf("ReadAll = %q, %v", data, err)
	}
	keys, err := r.List(ctx, filepath.Join(dir, "graph", "t-0000"))
	if err != nil || len(keys) != 1 || keys[0] != p {
		t.Fatalf("List = %v, %v; want [%s]", keys, err, p)
	}
	if err := r.Remove(ctx, p); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Open(ctx, p); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("Open after Remove = %v, want ErrNotFound", err)
	}
}

func TestRooted_PathsOutsideLocalDirPassThrough(t *testing.T) {
	dir := t.TempDir()
	mb := memory.New()
	r, err := storage.NewRooted(mb, dir, "bucket/prefix")
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	outside := filepath.Join(t.TempDir(), "import", "a.rf")
	if err := storage.WriteAll(ctx, r, outside, []byte("x")); err != nil {
		t.Fatal(err)
	}
	if got := mb.Keys(); len(got) != 1 || got[0] != outside {
		t.Fatalf("memory keys = %v, want [%s]", got, outside)
	}
	if got := r.ToInner("s3://other/key"); got != "s3://other/key" {
		t.Fatalf("scheme-qualified path rewritten to %q", got)
	}
	if got := r.ToInner(dir); got != "bucket/prefix" {
		t.Fatalf("ToInner(dir) = %q", got)
	}
}

// schemeLister returns keys spelled with a scheme regardless of how the
// prefix was given, as the S3 backend does.
type schemeLister struct{ storage.Backend }

func (schemeLister) List(_ context.Context, prefix string) ([]string, error) {
	prefix = strings.TrimPrefix(prefix, "s3://")
	return []string{"s3://" + prefix + "/0001.rf", "s3://" + prefix + "/files.json"}, nil
}

func TestRooted_ListMapsSchemeQualifiedKeysBack(t *testing.T) {
	dir := t.TempDir()
	r, err := storage.NewRooted(schemeLister{memory.New()}, dir, "s3://bucket/prefix")
	if err != nil {
		t.Fatal(err)
	}
	keys, err := r.List(context.Background(), filepath.Join(dir, "graph", "t-0000"))
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(keys)
	want := []string{
		filepath.Join(dir, "graph", "t-0000", "0001.rf"),
		filepath.Join(dir, "graph", "t-0000", "files.json"),
	}
	if len(keys) != 2 || keys[0] != want[0] || keys[1] != want[1] {
		t.Fatalf("List = %v, want %v", keys, want)
	}
}

func TestRooted_Validation(t *testing.T) {
	if _, err := storage.NewRooted(nil, t.TempDir(), "b/p"); err == nil {
		t.Fatal("nil inner accepted")
	}
	if _, err := storage.NewRooted(memory.New(), "", "b/p"); err == nil {
		t.Fatal("empty localDir accepted")
	}
	if _, err := storage.NewRooted(memory.New(), t.TempDir(), "s3://"); err == nil {
		t.Fatal("empty root accepted")
	}
}
