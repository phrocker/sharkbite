package agentmem

import (
	"context"
	"errors"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/phrocker/shoal-oss/internal/embeddingspace"
	"github.com/phrocker/shoal-oss/internal/embedpb"
	"github.com/phrocker/shoal-oss/internal/graphschema"
	modelio "github.com/phrocker/shoal-oss/pkg/model"
)

func TestRRF(t *testing.T) {
	s := RRF([][]string{{"a", "b"}, {"b", "c"}}, 60)
	if s["b"] <= s["a"] || s["b"] <= s["c"] {
		t.Fatalf("expected shared item b to win: %#v", s)
	}
}

func TestRuleClassifier(t *testing.T) {
	cases := map[string]Intent{"why did it fail": IntentWhy, "when was it done": IntentWhen, "which entity owns it": IntentEntity, "summarize graph": IntentGeneral}
	for q, want := range cases {
		if got := (RuleClassifier{}).Classify(q); got != want {
			t.Fatalf("Classify(%q)=%s want %s", q, got, want)
		}
	}
}

type identitylessModelEmbedder struct{}

func (identitylessModelEmbedder) Embed(
	context.Context,
	modelio.EmbedRequest,
) (modelio.EmbedResult, error) {
	return modelio.EmbedResult{Vector: []float32{1, 0}}, nil
}

func TestAdaptedEmbedderAcceptsExplicitIdentityFallback(t *testing.T) {
	const identity = "custom:model:v1:2:normalized"
	client, err := New(Config{
		Store:          NewFakeStore(),
		Embedder:       AdaptEmbedder(identitylessModelEmbedder{}),
		EmbeddingSpace: identity,
	})
	if err != nil {
		t.Fatalf("New with explicit identity: %v", err)
	}
	if client.cfg.EmbeddingSpace != identity {
		t.Fatalf("embedding space = %q, want %q", client.cfg.EmbeddingSpace, identity)
	}

	_, err = New(Config{
		Store:    NewFakeStore(),
		Embedder: AdaptEmbedder(identitylessModelEmbedder{}),
	})
	if !errors.Is(err, embeddingspace.ErrQueryIdentityRequired) {
		t.Fatalf("New without explicit identity error = %v", err)
	}
}

func TestEnsureTableRequiresEmbeddingAwareStore(t *testing.T) {
	legacy := struct{ EmbedStore }{EmbedStore: NewFakeStore()}
	client, err := New(Config{Store: legacy})
	if err != nil {
		t.Fatal(err)
	}
	if err := client.EnsureTable(
		context.Background(),
	); !errors.Is(err, embeddingspace.ErrQueryMetadataMissing) {
		t.Fatalf("EnsureTable legacy store error = %v", err)
	}
}

func TestPackVectorRoundTrip(t *testing.T) {
	want := []float32{1.25, -2.5, 0.125}
	got, err := UnpackVector(PackVector(want))
	if err != nil {
		t.Fatal(err)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("index %d got %v want %v", i, got[i], want[i])
		}
	}
}

func TestSynthesisBudget(t *testing.T) {
	nodes := []ScoredNode{{ID: "a", Content: "one two three four five", Timestamp: 1, Score: 1}, {ID: "b", Content: "six seven eight", Timestamp: 2, Score: .5}}
	got := Synthesize(nodes, IntentWhen, 5)
	if strings.Contains(got, "six") || !strings.Contains(got, "<ref:a>") {
		t.Fatalf("budget not enforced: %q", got)
	}
}

func TestBeamDeterministicOrdering(t *testing.T) {
	ctx := context.Background()
	store := NewFakeStore()
	c, _ := New(Config{Store: store, Table: "graph", MaxDepth: 1, BeamWidth: 4})
	_ = c.EnsureTable(ctx)
	base := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	a, _ := c.Ingest(ctx, IngestRequest{Text: "alpha caused beta", Time: base})
	b, _ := c.Ingest(ctx, IngestRequest{Text: "beta result", Time: base.Add(time.Millisecond)})
	_ = store.Write(ctx, "graph", []*embedpb.Mutation{{Row: []byte(a.Row), Entries: []*embedpb.Entry{{ColumnFamily: graphschema.CausalEdgeCF(), ColumnQualifier: []byte(b.ID), Timestamp: base.UnixMilli(), Value: graphschema.PackWeight(1)}}}})
	an, _ := c.analyze(ctx, "why beta")
	one, err := c.beam(ctx, []string{a.Row}, an)
	if err != nil {
		t.Fatal(err)
	}
	two, err := c.beam(ctx, []string{a.Row}, an)
	if err != nil {
		t.Fatal(err)
	}
	if len(one) != len(two) {
		t.Fatalf("lengths differ")
	}
	for i := range one {
		if one[i].Row != two[i].Row || math.Abs(float64(one[i].Score-two[i].Score)) > 1e-6 {
			t.Fatalf("not deterministic: %#v %#v", one, two)
		}
	}
}

func TestEndToEndFakeStore(t *testing.T) {
	// The real shoal-embed server lives in cmd and wires internal engine packages;
	// importing it here would break the requested engine/policy boundary. This e2e uses a
	// minimal in-memory EmbedStore that implements the ShoalEmbed Write/Scan shape,
	// keeping tests offline and deterministic while exercising the gRPC contract data.
	ctx := context.Background()
	store := NewFakeStore()
	c, _ := New(Config{Store: store, Table: "graph", MaxDepth: 1, TokenBudget: 30})
	_ = c.EnsureTable(ctx)
	base := time.Date(2026, 6, 23, 12, 0, 0, 0, time.UTC)
	_, _ = c.Ingest(ctx, IngestRequest{Text: "Alice deployed the graph service", Time: base})
	_, _ = c.Ingest(ctx, IngestRequest{Text: "The service failed because the index was stale", Time: base.Add(time.Millisecond)})
	_, _ = c.Ingest(ctx, IngestRequest{Text: "Alice rebuilt the index after the failure", Time: base.Add(2 * time.Millisecond)})
	res, err := c.Query(ctx, QueryRequest{Text: "why did the graph service fail", Time: base})
	if err != nil {
		t.Fatal(err)
	}
	if res.Intent != IntentWhy {
		t.Fatalf("intent=%s", res.Intent)
	}
	if len(res.Anchors) == 0 || !strings.Contains(res.Context, "<ref:") || !strings.Contains(strings.ToLower(res.Context), "service") {
		t.Fatalf("bad result: %#v context=%q", res, res.Context)
	}
}
