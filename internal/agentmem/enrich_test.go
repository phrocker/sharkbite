package agentmem

import (
	"bytes"
	"context"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/phrocker/shoal-oss/internal/embedpb"
	"github.com/phrocker/shoal-oss/internal/graphschema"
)

func TestHeuristicEnricherEntities(t *testing.T) {
	ctx := context.Background()
	tests := []struct {
		name string
		text string
		want []Entity
	}{
		{
			name: "dedupes and canonicalizes",
			text: "Alice met Alice at GitHub in New York. The Index failed.",
			want: []Entity{
				{ID: "concept:index", Label: "Index", Type: "CONCEPT"},
				{ID: "location:new_york", Label: "New York", Type: "LOCATION"},
				{ID: "organization:github", Label: "GitHub", Type: "ORGANIZATION"},
				{ID: "person:alice", Label: "Alice", Type: "PERSON"},
			},
		},
		{
			name: "skips sentence initial common words",
			text: "The service called Project Atlas. Then Bob fixed it.",
			want: []Entity{
				{ID: "concept:project_atlas", Label: "Project Atlas", Type: "CONCEPT"},
				{ID: "person:bob", Label: "Bob", Type: "PERSON"},
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := (HeuristicEnricher{}).Entities(ctx, tt.text)
			if err != nil {
				t.Fatal(err)
			}
			if len(got) != len(tt.want) {
				t.Fatalf("len=%d want %d: %#v", len(got), len(tt.want), got)
			}
			for i := range tt.want {
				if got[i] != tt.want[i] {
					t.Fatalf("got[%d]=%#v want %#v (all=%#v)", i, got[i], tt.want[i], got)
				}
			}
			again, _ := (HeuristicEnricher{}).Entities(ctx, tt.text)
			if !sameEntities(got, again) {
				t.Fatalf("not deterministic: %#v vs %#v", got, again)
			}
		})
	}
}

func TestHeuristicEnricherSummarize(t *testing.T) {
	ctx := context.Background()
	got, err := (HeuristicEnricher{}).Summarize(ctx, "First sentence. Second sentence.")
	if err != nil {
		t.Fatal(err)
	}
	if got != "First sentence." {
		t.Fatalf("summary=%q", got)
	}

	long := strings.Repeat("word ", 60) + "tail. Second."
	got, err = (HeuristicEnricher{}).Summarize(ctx, long)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) > 200 {
		t.Fatalf("summary too long: %d", len(got))
	}
	if strings.HasSuffix(got, " ") || strings.Contains(got, "...") {
		t.Fatalf("bad truncation: %q", got)
	}
}

func TestIngestEnrichmentEndToEnd(t *testing.T) {
	ctx := context.Background()
	store := NewFakeStore()
	c, err := New(Config{Store: store, Table: "graph"})
	if err != nil {
		t.Fatal(err)
	}
	if err := c.EnsureTable(ctx); err != nil {
		t.Fatal(err)
	}
	base := time.Date(2026, 6, 24, 12, 0, 0, 0, time.UTC)
	res, err := c.Ingest(ctx, IngestRequest{Text: "Alice deployed Graph Service at GitHub. It succeeded.", Time: base})
	if err != nil {
		t.Fatal(err)
	}
	if res.Summary != "Alice deployed Graph Service at GitHub." {
		t.Fatalf("summary=%q", res.Summary)
	}
	if len(res.Entities) == 0 {
		t.Fatal("expected entities")
	}

	cells, err := store.Scan(ctx, "graph", &embedpb.ScanRequest{})
	if err != nil {
		t.Fatal(err)
	}
	alice := "person:alice"
	if !hasCell(cells, graphschema.EntityRow(alice), graphschema.AttributeCF(), []byte("type"), []byte("PERSON")) {
		t.Fatalf("missing Alice type cell in %#v", cells)
	}
	if !hasCell(cells, graphschema.EntityRow(alice), graphschema.AttributeCF(), []byte("extractedFrom"), []byte(res.ID)) {
		t.Fatalf("missing Alice extractedFrom cell")
	}
	if !hasCell(cells, []byte(res.Row), graphschema.EntityEdgeCF(), []byte(alice), graphschema.PackWeight(1)) {
		t.Fatalf("missing event->entity edge")
	}
	if !hasCell(cells, []byte(res.Row), graphschema.ContentCF(), []byte("summary"), []byte(res.Summary)) {
		t.Fatalf("missing summary cell")
	}
}

func TestIngestEnrichmentIdempotentMutations(t *testing.T) {
	ctx := context.Background()
	base := time.Date(2026, 6, 24, 12, 0, 0, 0, time.UTC)
	first := ingestCellSignatures(t, ctx, "Alice deployed Graph Service at GitHub.", base)
	second := ingestCellSignatures(t, ctx, "Alice deployed Graph Service at GitHub.", base)
	if len(first) != len(second) {
		t.Fatalf("cell counts differ: %d vs %d", len(first), len(second))
	}
	for i := range first {
		if first[i] != second[i] {
			t.Fatalf("cell[%d] differs:\n%s\n%s", i, first[i], second[i])
		}
	}
}

func sameEntities(a, b []Entity) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func hasCell(cells []*embedpb.Cell, row, cf, cq, value []byte) bool {
	for _, c := range cells {
		if bytes.Equal(c.Row, row) && bytes.Equal(c.ColumnFamily, cf) && bytes.Equal(c.ColumnQualifier, cq) && bytes.Equal(c.Value, value) {
			return true
		}
	}
	return false
}

func ingestCellSignatures(t *testing.T, ctx context.Context, text string, at time.Time) []string {
	t.Helper()
	store := NewFakeStore()
	c, err := New(Config{Store: store, Table: "graph"})
	if err != nil {
		t.Fatal(err)
	}
	if err := c.EnsureTable(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Ingest(ctx, IngestRequest{Text: text, Time: at}); err != nil {
		t.Fatal(err)
	}
	cells, err := store.Scan(ctx, "graph", &embedpb.ScanRequest{})
	if err != nil {
		t.Fatal(err)
	}
	out := make([]string, 0, len(cells))
	for _, c := range cells {
		out = append(out, string(c.Row)+"|"+string(c.ColumnFamily)+"|"+string(c.ColumnQualifier)+"|"+string(c.Value))
	}
	sort.Strings(out)
	return out
}
