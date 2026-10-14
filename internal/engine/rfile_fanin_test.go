package engine

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/phrocker/shoal-oss/internal/storage/memory"
)

// TestImportFanInMergesReimport proves the fan-in fix: a second import of a
// table the engine already serves MERGES newly shipped RFiles instead of
// silently dropping them. Sequence:
//  1. producer A exports table "graph" to a shared destination
//  2. cluster imports A  -> sees A's rows
//  3. producer B exports MORE RFiles for "graph" to the same destination
//  4. cluster re-imports  -> previously a no-op; now merges B's rows
//  5. cluster scan sees the UNION
func TestImportFanInMergesReimport(t *testing.T) {
	ctx := context.Background()
	dstDir := filepath.Join(t.TempDir(), "cluster")
	dstBackend := memory.New()

	exportInto := func(rows []string, seed int) *RFileExportManifest {
		srcDir := filepath.Join(t.TempDir(), "src")
		src, err := Open(srcDir, Options{})
		if err != nil {
			t.Fatalf("Open source: %v", err)
		}
		defer src.Close()
		if err := src.CreateTable("graph", TableOptions{}); err != nil {
			t.Fatalf("CreateTable: %v", err)
		}
		for i, row := range rows {
			writeTenantRow(t, src, "graph", row, "v-"+row, int64(seed+i))
		}
		m, err := src.ExportRFiles(ctx, "graph", dstBackend, RFileExportOptions{
			DestinationRoot: dstDir,
			EngineVersion:   "test",
		})
		if err != nil {
			t.Fatalf("ExportRFiles: %v", err)
		}
		return m
	}

	manifestA := exportInto([]string{"a", "b"}, 100)
	dstBackend.Put(filepath.Join(dstDir, "graph", "t-0000", "orphan.rf"), []byte("not an rfile"))

	cluster, err := Open(dstDir, Options{Backend: dstBackend})
	if err != nil {
		t.Fatalf("Open cluster: %v", err)
	}
	defer cluster.Close()
	if err := cluster.ImportRFileManifest(ctx, manifestA); err != nil {
		t.Fatalf("import A: %v", err)
	}
	if got := scanAll(t, cluster, "graph"); len(got) != 2 {
		t.Fatalf("after import A: %v, want 2 cells", got)
	}

	// Producer B ships additional RFiles into the same destination.
	manifestB := exportInto([]string{"c", "d"}, 200)

	// Re-import registers only checksum-verified manifest entries. The
	// unrelated orphan under the same prefix must never become queryable.
	if err := cluster.ImportRFileManifest(ctx, manifestB); err != nil {
		t.Fatalf("re-import B: %v", err)
	}
	got := scanAll(t, cluster, "graph")
	if len(got) != 4 {
		t.Fatalf("after merge import B: %v, want 4 cells (union of A and B)", got)
	}

	// Idempotent: re-importing A again changes nothing.
	if err := cluster.ImportRFileManifest(ctx, manifestA); err != nil {
		t.Fatalf("idempotent re-import A: %v", err)
	}
	if again := scanAll(t, cluster, "graph"); len(again) != 4 {
		t.Fatalf("idempotent re-import changed cell count: %v, want 4", again)
	}
}
