package engine_test

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/phrocker/shoal-oss/internal/cclient"
	"github.com/phrocker/shoal-oss/internal/engine"
	"github.com/phrocker/shoal-oss/internal/storage"
	"github.com/phrocker/shoal-oss/internal/storage/memory"
)

func TestEngine_BackendRoot_StoresObjectsUnderRoot(t *testing.T) {
	dir := t.TempDir()
	mb := memory.New()
	const root = "bucket/shoal"

	open := func() *engine.Engine {
		eng, err := engine.Open(dir, engine.Options{Backend: mb, BackendRoot: root})
		if err != nil {
			t.Fatal(err)
		}
		return eng
	}
	eng := open()
	if err := eng.CreateTable("graph", engine.TableOptions{}); err != nil {
		t.Fatal(err)
	}
	const n = 40
	for i := 0; i < n; i++ {
		m, _ := cclient.NewMutation([]byte(fmt.Sprintf("entity:%04d", i)))
		m.PutLatest([]byte("props"), []byte("label"), nil, []byte(fmt.Sprintf("node-%d", i)))
		if err := eng.Write("graph", []*cclient.Mutation{m}); err != nil {
			t.Fatal(err)
		}
	}
	if err := eng.Flush("graph"); err != nil {
		t.Fatal(err)
	}
	if got := countRFilesOnDisk(t, dir); got != 0 {
		t.Fatalf("expected 0 .rf files on local disk, found %d", got)
	}
	keys := mb.Keys()
	if len(keys) == 0 {
		t.Fatal("memory backend holds no objects")
	}
	for _, k := range keys {
		if !strings.HasPrefix(k, root+"/graph/") {
			t.Fatalf("object %q is not under %s/graph/", k, root)
		}
		if strings.Contains(k, dir) {
			t.Fatalf("object %q leaks the local data directory", k)
		}
	}
	if err := eng.Close(); err != nil {
		t.Fatal(err)
	}

	eng = open()
	defer eng.Close()
	if got := len(scanAll(t, eng, "graph")); got != n {
		t.Fatalf("after reopen scanned %d entries, want %d", got, n)
	}
}

func TestEngine_BackendRoot_MismatchIsRefused(t *testing.T) {
	dir := t.TempDir()
	mb := memory.New()
	eng, err := engine.Open(dir, engine.Options{Backend: mb, BackendRoot: "bucket/A"})
	if err != nil {
		t.Fatal(err)
	}
	if err := eng.CreateTable("graph", engine.TableOptions{}); err != nil {
		t.Fatal(err)
	}
	m, _ := cclient.NewMutation([]byte("entity:0001"))
	m.PutLatest([]byte("props"), []byte("label"), nil, []byte("node"))
	if err := eng.Write("graph", []*cclient.Mutation{m}); err != nil {
		t.Fatal(err)
	}
	if err := eng.Flush("graph"); err != nil {
		t.Fatal(err)
	}
	if err := eng.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := engine.Open(dir, engine.Options{Backend: mb, BackendRoot: "bucket/B"}); err == nil || !strings.Contains(err.Error(), "backend root") {
		t.Fatalf("reopen under a different root = %v, want a backend-root mismatch error", err)
	}
	if _, err := engine.Open(dir, engine.Options{Backend: mb}); err == nil || !strings.Contains(err.Error(), "backend root") {
		t.Fatalf("reopen without a root = %v, want a backend-root mismatch error", err)
	}
	eng, err = engine.Open(dir, engine.Options{Backend: mb, BackendRoot: "bucket/A"})
	if err != nil {
		t.Fatalf("reopen under the original root: %v", err)
	}
	defer eng.Close()
	if got := len(scanAll(t, eng, "graph")); got != 1 {
		t.Fatalf("scanned %d rows, want 1", got)
	}
}

func TestEngine_BackendRoot_LegacyTableWithoutRootIsRefused(t *testing.T) {
	dir := t.TempDir()
	mb := memory.New()
	eng, err := engine.Open(dir, engine.Options{Backend: mb})
	if err != nil {
		t.Fatal(err)
	}
	if err := eng.CreateTable("graph", engine.TableOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := eng.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := engine.Open(dir, engine.Options{Backend: mb, BackendRoot: "bucket/A"}); err == nil || !strings.Contains(err.Error(), "backend root") {
		t.Fatalf("opening a rootless table with a root = %v, want a backend-root mismatch error", err)
	}
}

// openOnly mimics a read-through cache: Open only, no optional capabilities.
type openOnly struct{ inner *memory.Backend }

func (o openOnly) Open(ctx context.Context, p string) (storage.File, error) {
	return o.inner.Open(ctx, p)
}

func TestEngine_BackendRoot_OpenOnlyInnerStillCreatesTables(t *testing.T) {
	dir := t.TempDir()
	eng, err := engine.Open(dir, engine.Options{Backend: openOnly{memory.New()}, BackendRoot: "bucket/x"})
	if err != nil {
		t.Fatal(err)
	}
	defer eng.Close()
	if err := eng.CreateTable("graph", engine.TableOptions{}); err != nil {
		t.Fatalf("CreateTable through an open-only rooted backend: %v", err)
	}
}

func TestEngine_DotDirectoriesAreNotTables(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, ".sync"), 0o755); err != nil {
		t.Fatal(err)
	}
	eng, err := engine.Open(dir, engine.Options{Backend: memory.New(), BackendRoot: "bucket/x"})
	if err != nil {
		t.Fatalf("a dot-prefixed state directory was opened as a table: %v", err)
	}
	defer eng.Close()
	for _, name := range eng.TableNames() {
		if name == ".sync" {
			t.Fatal(".sync listed as a table")
		}
	}
	if err := eng.CreateTable(".hidden", engine.TableOptions{}); err == nil {
		t.Fatal("dot-prefixed table name accepted; it would be invisible on reopen")
	}
}

func TestEngine_BackendRoot_RequiresBackend(t *testing.T) {
	if _, err := engine.Open(t.TempDir(), engine.Options{BackendRoot: "bucket/x"}); err == nil {
		t.Fatal("BackendRoot without Backend accepted")
	}
}
