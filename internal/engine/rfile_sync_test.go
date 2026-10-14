package engine

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/phrocker/shoal-oss/internal/cclient"
	"github.com/phrocker/shoal-oss/internal/storage/memory"
	"github.com/phrocker/shoal-oss/internal/tablet"
)

func writeRow(t *testing.T, eng *Engine, table, row string, i int) {
	t.Helper()
	m, err := cclient.NewMutation([]byte(row))
	if err != nil {
		t.Fatalf("NewMutation: %v", err)
	}
	m.Put([]byte("cf"), []byte(fmt.Sprintf("cq-%d", i)), []byte("tenantA"), int64(100+i), []byte("value-"+row))
	if err := eng.Write(table, []*cclient.Mutation{m}); err != nil {
		t.Fatalf("Write: %v", err)
	}
}

func TestExportRFilesIncremental(t *testing.T) {
	ctx := context.Background()
	srcDir := filepath.Join(t.TempDir(), "src")
	dstDir := filepath.Join(t.TempDir(), "dst")

	src, err := Open(srcDir, Options{})
	if err != nil {
		t.Fatalf("Open source: %v", err)
	}

	defer src.Close()
	if err := src.CreateTable("graph", TableOptions{}); err != nil {
		t.Fatalf("CreateTable: %v", err)
	}

	dst := memory.New()
	opts := RFileExportOptions{DestinationRoot: dstDir, EngineVersion: "test"}

	// First tick: everything is new.
	writeRow(t, src, "graph", "evt:a", 0)
	res1, err := src.ExportRFilesIncremental(ctx, "graph", dst, opts, nil)
	if err != nil {
		t.Fatalf("tick1: %v", err)
	}
	if res1.State.Sequence != 1 {
		t.Fatalf("tick1 sequence = %d, want 1", res1.State.Sequence)
	}
	if len(res1.Uploaded) == 0 {
		t.Fatalf("tick1 uploaded nothing")
	}
	if len(res1.Skipped) != 0 {
		t.Fatalf("tick1 skipped = %v, want none", res1.Skipped)
	}
	if err := VerifyRFileExport(ctx, dst, res1.Manifest); err != nil {
		t.Fatalf("tick1 verify: %v", err)
	}

	// Second tick with no new data: nothing flushes to a new RFile, so all
	// previously shipped files are skipped and none re-uploaded.
	res2, err := src.ExportRFilesIncremental(ctx, "graph", dst, opts, res1.State)
	if err != nil {
		t.Fatalf("tick2: %v", err)
	}
	if res2.State.Sequence != 2 {
		t.Fatalf("tick2 sequence = %d, want 2", res2.State.Sequence)
	}
	if len(res2.Uploaded) != 0 {
		t.Fatalf("tick2 uploaded = %v, want none (no new data)", res2.Uploaded)
	}
	if len(res2.Skipped) != len(res1.Uploaded) {
		t.Fatalf("tick2 skipped = %d, want %d", len(res2.Skipped), len(res1.Uploaded))
	}

	// Third tick after new data: a new RFile is flushed and shipped; the prior
	// RFile is still skipped (immutable).
	writeRow(t, src, "graph", "evt:b", 1)
	res3, err := src.ExportRFilesIncremental(ctx, "graph", dst, opts, res2.State)
	if err != nil {
		t.Fatalf("tick3: %v", err)
	}
	if len(res3.Uploaded) == 0 {
		t.Fatalf("tick3 uploaded nothing despite new data")
	}
	if len(res3.Skipped) == 0 {
		t.Fatalf("tick3 skipped nothing; prior RFile should be reused")
	}
	if err := VerifyRFileExport(ctx, dst, res3.Manifest); err != nil {
		t.Fatalf("tick3 verify: %v", err)
	}

	// The latest manifest must describe the full current file set and import
	// cleanly into a fresh engine, reproducing every cell.
	want := scanAll(t, src, "graph")
	imp, err := Open(dstDir, Options{Backend: dst})
	if err != nil {
		t.Fatalf("Open import: %v", err)
	}
	defer imp.Close()
	if err := imp.ImportRFileManifest(ctx, res3.Manifest); err != nil {
		t.Fatalf("ImportRFileManifest: %v", err)
	}
	got := scanAll(t, imp, "graph")
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("imported scan mismatch\ngot  %v\nwant %v", got, want)
	}
}

func TestSyncStateRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "graph.json")

	// Absent file → (nil, nil).
	got, err := LoadSyncState(path)
	if err != nil {
		t.Fatalf("LoadSyncState(absent): %v", err)
	}
	if got != nil {
		t.Fatalf("LoadSyncState(absent) = %v, want nil", got)
	}

	state := &SyncState{
		Version:  SyncStateVersion,
		Table:    "graph",
		Sequence: 7,
		Shipped: map[string]RFileExportFile{
			"graph/t-0000/F0000000000001.rf": {RelativePath: "graph/t-0000/F0000000000001.rf", Size: 42, SHA256: "abc"},
		},
	}
	if err := SaveSyncState(path, state); err != nil {
		t.Fatalf("SaveSyncState: %v", err)
	}
	reloaded, err := LoadSyncState(path)
	if err != nil {
		t.Fatalf("LoadSyncState: %v", err)
	}
	if reloaded.Sequence != 7 || reloaded.Table != "graph" || len(reloaded.Shipped) != 1 {
		t.Fatalf("reloaded state mismatch: %+v", reloaded)
	}
}

func TestExportRFilesIncrementalParquetMetadataAndImport(t *testing.T) {
	ctx := context.Background()
	srcDir := filepath.Join(t.TempDir(), "src")
	dstDir := filepath.Join(t.TempDir(), "dst")
	src, err := Open(srcDir, Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer src.Close()
	if err := src.CreateTable("analytics", TableOptions{Workload: WorkloadAnalytical}); err != nil {
		t.Fatal(err)
	}
	writeRow(t, src, "analytics", "evt:a", 0)
	dst := memory.New()
	result, err := src.ExportRFilesIncremental(ctx, "analytics", dst, RFileExportOptions{
		DestinationRoot: dstDir,
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if result.Manifest.FileFormat != tablet.FormatParquet ||
		result.Manifest.RFileCompatibility != "parquet/shoal" ||
		len(result.Manifest.RFiles) != 1 ||
		result.Manifest.RFiles[0].Format != string(tablet.FormatParquet) {
		t.Fatalf("incremental parquet manifest = %+v", result.Manifest)
	}

	imported, err := Open(dstDir, Options{Backend: dst})
	if err != nil {
		t.Fatal(err)
	}
	defer imported.Close()
	if err := imported.ImportRFileManifest(ctx, result.Manifest); err != nil {
		t.Fatal(err)
	}
	if got := scanAll(t, imported, "analytics"); fmt.Sprint(got) != fmt.Sprint(scanAll(t, src, "analytics")) {
		t.Fatalf("incremental parquet import = %v", got)
	}
}

func TestOpenRejectsIncompleteManifestLayout(t *testing.T) {
	dir := t.TempDir()
	tableDir := filepath.Join(dir, "broken")
	if err := os.MkdirAll(filepath.Join(tableDir, "t-0000"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := writeTableManifest(tableDir, tableManifest{
		Version:    tableManifestVersion,
		Splits:     [][]byte{[]byte("m")},
		FileFormat: tablet.FormatRFile,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(dir, Options{}); err == nil || !strings.Contains(err.Error(), "incomplete layout") {
		t.Fatalf("Open error = %v, want incomplete layout", err)
	}
}
