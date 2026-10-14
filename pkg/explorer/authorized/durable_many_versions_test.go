package authorized_test

import (
	"context"
	"fmt"
	"testing"

	"github.com/phrocker/shoal-oss/pkg/explorer/authorized"
)

// TestDurablePolicyStoreReopensAfterManyOverwritesOfOneRow is the #690
// repro: the label-migration marker lives on one catalog row, so every
// PutLabelMigration overwrites the same cell. Before the rfile writer fix,
// ~200 overwrites before a flush produced an rfile whose index repeated a
// key, and the reopened catalog refused to load ("scan policy records").
func TestDurablePolicyStoreReopensAfterManyOverwritesOfOneRow(t *testing.T) {
	for _, n := range []int{150, 199, 250} {
		t.Run(fmt.Sprint(n), func(t *testing.T) {
			dir := t.TempDir()
			ctx := context.Background()
			store, err := authorized.OpenDurablePolicyStore(dir)
			if err != nil {
				t.Fatal(err)
			}
			for v := 1; v <= n; v++ {
				record := authorized.LabelMigrationRecord{
					Version: uint32(v), Documents: v, Tightened: v % 7,
					Untranslatable: []authorized.UntranslatableLabel{{
						DocumentID: "doc-1", RevisionID: "rev-1",
						SourceURI:    "file:///catalog/source.md",
						EscapedLabel: `"bad label!"`, Reason: "does not parse",
					}},
				}
				if err := store.PutLabelMigration(ctx, record); err != nil {
					t.Fatalf("PutLabelMigration v%d: %v", v, err)
				}
			}
			if err := store.Close(); err != nil {
				t.Fatalf("close: %v", err)
			}

			reopened, err := authorized.OpenDurablePolicyStore(dir)
			if err != nil {
				t.Fatalf("reopen after %d overwrites: %v", n, err)
			}
			defer reopened.Close()
			got, ok, err := reopened.LabelMigration(ctx)
			if err != nil || !ok {
				t.Fatalf("LabelMigration after reopen = %v, %v", ok, err)
			}
			if got.Version != uint32(n) || got.Documents != n {
				t.Fatalf("marker after reopen = v%d documents=%d, want the newest (v%d)",
					got.Version, got.Documents, n)
			}
		})
	}
}
