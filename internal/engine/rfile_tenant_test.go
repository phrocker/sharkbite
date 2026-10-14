package engine

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/phrocker/shoal-oss/internal/cclient"
	"github.com/phrocker/shoal-oss/internal/iterrt"
	"github.com/phrocker/shoal-oss/internal/storage/memory"
)

// writeTenantRow writes a single cell (with no source visibility) for a row.
func writeTenantRow(t *testing.T, eng *Engine, table, row, value string, ts int64) {
	t.Helper()
	m, err := cclient.NewMutation([]byte(row))
	if err != nil {
		t.Fatalf("NewMutation: %v", err)
	}
	m.Put([]byte("cf"), []byte("cq"), nil, ts, []byte(value))
	if err := eng.Write(table, []*cclient.Mutation{m}); err != nil {
		t.Fatalf("Write: %v", err)
	}
}

// scanAuth scans a table with the given authorization labels, applying the
// visibility filter so stamped cells are enforced. The filter is only active
// with a non-nil auth set (nil == system context, sees everything), matching
// the read-fleet serving path.
func scanAuth(t *testing.T, eng *Engine, table string, auths ...string) []string {
	t.Helper()
	var as [][]byte
	for _, a := range auths {
		as = append(as, []byte(a))
	}
	sc, err := eng.Scan(table, iterrt.InfiniteRange(), ScanOptions{
		Authorizations: as,
		Stack:          []iterrt.IterSpec{{Name: iterrt.IterVisibility}},
	})
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	defer sc.Close()
	var out []string
	for sc.Next() {
		k := sc.Key()
		out = append(out, fmt.Sprintf("%s|%s", k.Row, sc.Value()))
		if err := sc.Advance(); err != nil {
			t.Fatalf("Advance: %v", err)
		}
	}
	return out
}

// TestExportTenantStampEnforcesIsolation exports a table with a tenant
// visibility stamp, re-imports it, and proves the stamped cells are only
// visible to a scan carrying the tenant authorization — the mechanism that
// lets many local agents fan into one engine safely.
func TestExportTenantStampEnforcesIsolation(t *testing.T) {
	ctx := context.Background()
	srcDir := filepath.Join(t.TempDir(), "src")
	dstDir := filepath.Join(t.TempDir(), "dst")

	src, err := Open(srcDir, Options{})
	if err != nil {
		t.Fatalf("Open source: %v", err)
	}
	if err := src.CreateTable("mem", TableOptions{}); err != nil {
		t.Fatalf("CreateTable: %v", err)
	}
	writeTenantRow(t, src, "mem", "evt:1", "alpha", 100)
	writeTenantRow(t, src, "mem", "evt:2", "beta", 101)

	// Source cells have no visibility, so they're visible to anyone.
	if got := scanAuth(t, src, "mem"); len(got) != 2 {
		t.Fatalf("source scan = %v, want 2 cells", got)
	}

	dstBackend := memory.New()
	manifest, err := src.ExportRFiles(ctx, "mem", dstBackend, RFileExportOptions{
		DestinationRoot:      dstDir,
		StampVisibilityLabel: "agentA",
		EngineVersion:        "test",
	})
	if err != nil {
		t.Fatalf("ExportRFiles: %v", err)
	}
	defer src.Close()

	// Stamping must populate the manifest visibility metadata.
	if manifest.VisibilityStamp != "agentA" || manifest.AuthorizationsStamp != "agentA" {
		t.Fatalf("manifest stamps = %q/%q, want agentA/agentA",
			manifest.VisibilityStamp, manifest.AuthorizationsStamp)
	}
	if err := VerifyRFileExport(ctx, dstBackend, manifest); err != nil {
		t.Fatalf("VerifyRFileExport: %v", err)
	}

	dst, err := Open(dstDir, Options{Backend: dstBackend})
	if err != nil {
		t.Fatalf("Open destination: %v", err)
	}
	defer dst.Close()
	if err := dst.ImportRFileManifest(ctx, manifest); err != nil {
		t.Fatalf("ImportRFileManifest: %v", err)
	}

	// Every imported cell now carries the agentA stamp.
	for _, line := range scanAll(t, dst, "mem") {
		if !strings.Contains(line, "|agentA|") {
			t.Fatalf("imported cell missing agentA stamp: %q", line)
		}
	}

	// A scan with the agentA auth sees both cells.
	if got := scanAuth(t, dst, "mem", "agentA"); len(got) != 2 {
		t.Fatalf("scan with agentA auth = %v, want 2 cells", got)
	}
	// A scan with a different (non-nil) tenant's auth sees nothing —
	// isolation enforced by the stamped visibility.
	if got := scanAuth(t, dst, "mem", "agentB"); len(got) != 0 {
		t.Fatalf("scan with agentB auth = %v, want 0 cells (isolation)", got)
	}
}

// TestExportTenantStampAndModeCombinesExistingCV verifies "and" mode keeps a
// producer's own intra-tenant labels AND requires the tenant label, so a scan
// must hold both to see the cell.
func TestExportTenantStampAndModeCombinesExistingCV(t *testing.T) {
	ctx := context.Background()
	srcDir := filepath.Join(t.TempDir(), "src")
	dstDir := filepath.Join(t.TempDir(), "dst")

	src, err := Open(srcDir, Options{})
	if err != nil {
		t.Fatalf("Open source: %v", err)
	}
	if err := src.CreateTable("mem", TableOptions{}); err != nil {
		t.Fatalf("CreateTable: %v", err)
	}
	m, err := cclient.NewMutation([]byte("evt:1"))
	if err != nil {
		t.Fatalf("NewMutation: %v", err)
	}
	m.Put([]byte("cf"), []byte("cq"), []byte("secret"), 100, []byte("v"))
	if err := src.Write("mem", []*cclient.Mutation{m}); err != nil {
		t.Fatalf("Write: %v", err)
	}

	dstBackend := memory.New()
	manifest, err := src.ExportRFiles(ctx, "mem", dstBackend, RFileExportOptions{
		DestinationRoot:      dstDir,
		StampVisibilityLabel: "agentA",
		EngineVersion:        "test",
	})
	if err != nil {
		t.Fatalf("ExportRFiles: %v", err)
	}
	defer src.Close()

	dst, err := Open(dstDir, Options{Backend: dstBackend})
	if err != nil {
		t.Fatalf("Open destination: %v", err)
	}
	defer dst.Close()
	if err := dst.ImportRFileManifest(ctx, manifest); err != nil {
		t.Fatalf("ImportRFileManifest: %v", err)
	}

	// Holding only one of the two required labels is insufficient.
	if got := scanAuth(t, dst, "mem", "secret"); len(got) != 0 {
		t.Fatalf("scan with only secret = %v, want 0", got)
	}
	if got := scanAuth(t, dst, "mem", "agentA"); len(got) != 0 {
		t.Fatalf("scan with only agentA = %v, want 0", got)
	}
	// Holding both labels satisfies (secret)&(agentA).
	if got := scanAuth(t, dst, "mem", "secret", "agentA"); len(got) != 1 {
		t.Fatalf("scan with both = %v, want 1", got)
	}
}
