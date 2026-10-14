package engine

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/phrocker/shoal-oss/internal/storage/memory"
)

// TestExportProducerNamespacing proves the fan-in name-collision fix: two
// producers exporting the same table into one shared destination get distinct
// object names because each RFile's base name is prefixed with its producer id.
// Both manifests then import-merge into the union without clobbering each other.
func TestExportProducerNamespacing(t *testing.T) {
	ctx := context.Background()
	dstDir := filepath.Join(t.TempDir(), "cluster")
	dstBackend := memory.New()

	exportInto := func(producer string, rows []string, seed int) *RFileExportManifest {
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
			ProducerID:      producer,
			EngineVersion:   "test",
			// Distinct manifest paths so the second export doesn't overwrite
			// the first producer's manifest at the shared destination.
			ManifestPath: filepath.Join(dstDir, "manifest-"+producer+".json"),
		})
		if err != nil {
			t.Fatalf("ExportRFiles(%s): %v", producer, err)
		}
		return m
	}

	manifestA := exportInto("agentA", []string{"a", "b"}, 100)
	manifestB := exportInto("agentB", []string{"c", "d"}, 200)

	// Every exported RFile must carry its producer prefix on the base name.
	for _, f := range manifestA.RFiles {
		if !strings.Contains(filepath.Base(f.RelativePath), "agentA~") {
			t.Fatalf("producer A file not namespaced: %q", f.RelativePath)
		}
	}
	for _, f := range manifestB.RFiles {
		if !strings.Contains(filepath.Base(f.RelativePath), "agentB~") {
			t.Fatalf("producer B file not namespaced: %q", f.RelativePath)
		}
	}

	// The two producers must not share any destination object key, even if
	// their RFiles were minted at the same millisecond.
	seen := map[string]bool{}
	for _, f := range append(append([]RFileExportFile{}, manifestA.RFiles...), manifestB.RFiles...) {
		if seen[f.DestinationPath] {
			t.Fatalf("destination collision on %q", f.DestinationPath)
		}
		seen[f.DestinationPath] = true
	}

	cluster, err := Open(dstDir, Options{Backend: dstBackend})
	if err != nil {
		t.Fatalf("Open cluster: %v", err)
	}
	defer cluster.Close()
	if err := cluster.ImportRFileManifest(ctx, manifestA); err != nil {
		t.Fatalf("import A: %v", err)
	}
	if err := cluster.ImportRFileManifest(ctx, manifestB); err != nil {
		t.Fatalf("import B: %v", err)
	}
	if got := scanAll(t, cluster, "graph"); len(got) != 4 {
		t.Fatalf("after fan-in import: %v, want 4 cells (union of A and B)", got)
	}
}

func TestValidateProducerID(t *testing.T) {
	ok := []string{"", "agentA", "agent-1", "agent_1", "a.b.c", "AGENT.01"}
	for _, id := range ok {
		if err := validateProducerID(id); err != nil {
			t.Errorf("validateProducerID(%q) = %v, want nil", id, err)
		}
	}
	bad := []string{"a/b", "a~b", "a b", "a:b", "a*b", "../x"}
	for _, id := range bad {
		if err := validateProducerID(id); err == nil {
			t.Errorf("validateProducerID(%q) = nil, want error", id)
		}
	}
}
