package engine_test

import (
	"fmt"
	"strings"
	"testing"

	"github.com/phrocker/shoal-oss/internal/cclient"
	"github.com/phrocker/shoal-oss/internal/engine"
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

func TestEngine_BackendRoot_RequiresBackend(t *testing.T) {
	if _, err := engine.Open(t.TempDir(), engine.Options{BackendRoot: "bucket/x"}); err == nil {
		t.Fatal("BackendRoot without Backend accepted")
	}
}
