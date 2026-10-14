package shoalql

import (
	"context"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"math"
	"testing"

	"github.com/phrocker/shoal-oss/internal/embeddingspace"
	"github.com/phrocker/shoal-oss/internal/graphschema"
	"github.com/phrocker/shoal-oss/internal/iterrt"
)

func planFor(t *testing.T, sql string, opts PlanOptions) *Plan {
	t.Helper()
	st, err := Parse(sql)
	if err != nil {
		t.Fatalf("parse %q: %v", sql, err)
	}
	cat := NewGraphCatalog("graph")
	b, ok := cat.Binding(st.Table)
	if !ok {
		t.Fatalf("no binding for %q", st.Table)
	}
	p, err := PlanQuery(context.Background(), st, b, opts)
	if err != nil {
		t.Fatalf("plan %q: %v", sql, err)
	}
	return p
}

func TestPlan_TablePrefixRange(t *testing.T) {
	p := planFor(t, "SELECT * FROM events", PlanOptions{})
	if p.Table != "graph" {
		t.Errorf("table = %q", p.Table)
	}
	if p.Shape != ShapeScan {
		t.Errorf("shape = %v", p.Shape)
	}
	if p.Range.InfiniteStart || string(p.Range.Start.Row) != "evt:" {
		t.Errorf("start = %+v", p.Range.Start)
	}
	// successor("evt:") == "evt;"
	if p.Range.InfiniteEnd || string(p.Range.End.Row) != "evt;" {
		t.Errorf("end = %q", p.Range.End.Row)
	}
}

func TestPlan_PreservesExplainRequest(t *testing.T) {
	p := planFor(t, "EXPLAIN FORMAT JSON SELECT id FROM events WHERE id >= 'a'", PlanOptions{})
	if !p.Explain || p.ExplainFormat != ExplainJSON {
		t.Fatalf("explain=%v format=%q", p.Explain, p.ExplainFormat)
	}
	if p.Shape != ShapeScan || string(p.Range.Start.Row) != "evt:a" {
		t.Fatalf("shape=%v range=%+v", p.Shape, p.Range)
	}
}

func TestPlan_IdEquality(t *testing.T) {
	p := planFor(t, "SELECT id FROM events WHERE id = 'abc'", PlanOptions{})
	if string(p.Range.Start.Row) != "evt:abc" {
		t.Errorf("start = %q", p.Range.Start.Row)
	}
	if len(p.Range.End.Row) != len("evt:abc")+1 || p.Range.End.Row[len(p.Range.End.Row)-1] != 0x00 {
		t.Errorf("end = %v", p.Range.End.Row)
	}
	if len(p.Residual) != 0 {
		t.Errorf("residual = %v", p.Residual)
	}
}

func TestPlan_IdRange(t *testing.T) {
	p := planFor(t, "SELECT id FROM events WHERE id >= 'a' AND id < 'm'", PlanOptions{})
	if string(p.Range.Start.Row) != "evt:a" {
		t.Errorf("start = %q", p.Range.Start.Row)
	}
	if string(p.Range.End.Row) != "evt:m" {
		t.Errorf("end = %q", p.Range.End.Row)
	}
}

func TestPlan_IdLikePrefix(t *testing.T) {
	p := planFor(t, "SELECT id FROM events WHERE id LIKE 'evt:2%'", PlanOptions{})
	if string(p.Range.Start.Row) != "evt:evt:2" {
		t.Errorf("start = %q", p.Range.Start.Row)
	}
	if string(p.Range.End.Row) != "evt:evt:3" {
		t.Errorf("end = %q", p.Range.End.Row)
	}
}

func TestPlan_NonIdPredicateBecomesResidual(t *testing.T) {
	p := planFor(t, "SELECT id FROM events WHERE content = 'hi'", PlanOptions{})
	if len(p.Residual) != 1 || string(p.Residual[0].CF) != string(graphschema.ContentCF()) {
		t.Fatalf("residual = %+v", p.Residual)
	}
	if p.Residual[0].Str != "hi" || p.Residual[0].Op != OpEq {
		t.Errorf("residual = %+v", p.Residual[0])
	}
	// range stays at the full table prefix
	if string(p.Range.Start.Row) != "evt:" {
		t.Errorf("start = %q", p.Range.Start.Row)
	}
}

func TestPlan_MatchResidual(t *testing.T) {
	p := planFor(t, "SELECT id FROM events WHERE MATCH(content, 'retry timeout')", PlanOptions{})
	if len(p.Residual) != 1 || !p.Residual[0].IsMatch {
		t.Fatalf("residual = %+v", p.Residual)
	}
	if p.Residual[0].Terms != "retry timeout" {
		t.Errorf("terms = %q", p.Residual[0].Terms)
	}
}

func TestPlan_AsOf(t *testing.T) {
	p := planFor(t, "SELECT * FROM events AS OF 1700 WHERE id = 'x'", PlanOptions{})
	if p.AsOf == nil || *p.AsOf != 1700 {
		t.Fatalf("asof = %v", p.AsOf)
	}
	found := false
	for _, s := range p.Stack {
		if s.Name == iterrt.IterAsOf && s.Options[iterrt.AsOfOption] == "1700" {
			found = true
		}
	}
	if !found {
		t.Errorf("asof iterspec missing: %+v", p.Stack)
	}
}

func TestPlan_GroupByCount(t *testing.T) {
	p := planFor(t, "SELECT cf, count(*) AS n FROM events GROUP BY cf", PlanOptions{})
	if p.Shape != ShapeAggregate || p.GroupByColumn != "cf" {
		t.Fatalf("shape=%v group=%q", p.Shape, p.GroupByColumn)
	}
	var agg *iterrt.IterSpec
	for i := range p.Stack {
		if p.Stack[i].Name == iterrt.IterGraphAggregation {
			agg = &p.Stack[i]
		}
	}
	if agg == nil {
		t.Fatal("no aggregation iterspec")
	}
	if agg.Options[iterrt.GraphAggregationOp] != "count" {
		t.Errorf("op = %q", agg.Options[iterrt.GraphAggregationOp])
	}
	if agg.Options[iterrt.GraphAggregationGroupBy] != "cf" {
		t.Errorf("groupBy = %q", agg.Options[iterrt.GraphAggregationGroupBy])
	}
	if len(p.Projection) != 2 || p.Projection[0].Kind != OutGroupKey || p.Projection[1].Kind != OutCount {
		t.Errorf("projection = %+v", p.Projection)
	}
	if p.Projection[1].Name != "n" {
		t.Errorf("count name = %q", p.Projection[1].Name)
	}
}

func TestPlan_GroupByUnsupportedColumn(t *testing.T) {
	st, _ := Parse("SELECT content, count(*) FROM events GROUP BY content")
	b, _ := NewGraphCatalog("graph").Binding("events")
	if _, err := PlanQuery(context.Background(), st, b, PlanOptions{}); err == nil {
		t.Fatal("expected error grouping by arbitrary attribute value")
	}
}

func TestPlan_VectorKNNLiteral(t *testing.T) {
	p := planFor(t, "SELECT id, content FROM events ORDER BY embedding <-> [1, 0, -1] LIMIT 5", PlanOptions{
		Vector: VectorOptions{EmbeddingSpace: "space-a"},
	})
	if p.Shape != ShapeVectorKNN {
		t.Fatalf("shape = %v", p.Shape)
	}
	if !p.NeedsHydration {
		t.Errorf("expected hydration for content projection")
	}
	var knn *iterrt.IterSpec
	for i := range p.Stack {
		if p.Stack[i].Name == iterrt.IterVectorKNN {
			knn = &p.Stack[i]
		}
	}
	if knn == nil {
		t.Fatal("no knn iterspec")
	}
	if knn.Options[iterrt.VectorKNNTopK] != "5" {
		t.Errorf("topK = %q", knn.Options[iterrt.VectorKNNTopK])
	}
	if knn.Options[iterrt.VectorKNNEmbeddingCF] != string(graphschema.VectorCF()) {
		t.Errorf("embeddingCF = %q", knn.Options[iterrt.VectorKNNEmbeddingCF])
	}
	if knn.Options[iterrt.VectorKNNEmbeddingSpace] != "space-a" {
		t.Errorf("embeddingSpace = %q", knn.Options[iterrt.VectorKNNEmbeddingSpace])
	}
	// verify packed query vector round-trips
	raw, err := base64.StdEncoding.DecodeString(knn.Options[iterrt.VectorKNNQuery])
	if err != nil {
		t.Fatal(err)
	}
	want := []float32{1, 0, -1}
	if len(raw) != 4*len(want) {
		t.Fatalf("packed len = %d", len(raw))
	}
	for i, w := range want {
		got := math.Float32frombits(binary.BigEndian.Uint32(raw[i*4:]))
		if got != w {
			t.Errorf("v[%d] = %v want %v", i, got, w)
		}
	}
}

func TestPlan_VectorKNNParam(t *testing.T) {
	opts := PlanOptions{
		Params: map[string][]float32{"q": {0.5, 0.5}},
		Vector: VectorOptions{EmbeddingSpace: "space-a"},
	}
	p := planFor(t, "SELECT id FROM events ORDER BY vec <-> :q LIMIT 3", opts)
	if p.Shape != ShapeVectorKNN {
		t.Fatalf("shape = %v", p.Shape)
	}
	if p.NeedsHydration {
		t.Errorf("id-only projection should not hydrate")
	}
}

func TestPlan_VectorKNNTextNeedsEmbedder(t *testing.T) {
	st, _ := Parse("SELECT id FROM events ORDER BY embedding <-> 'hello' LIMIT 3")
	b, _ := NewGraphCatalog("graph").Binding("events")
	if _, err := PlanQuery(context.Background(), st, b, PlanOptions{}); err == nil {
		t.Fatal("expected error without embedder")
	}
}

type fakeEmbedder struct {
	v        []float32
	identity string
	calls    *int
}

func (f fakeEmbedder) Embed(_ context.Context, _ string) ([]float32, error) {
	if f.calls != nil {
		(*f.calls)++
	}
	return f.v, nil
}

func (f fakeEmbedder) EmbeddingSpaceIdentity() (string, error) {
	if f.identity == "" {
		return "space-a", nil
	}
	return f.identity, nil
}

func TestPlan_VectorKNNTextWithEmbedder(t *testing.T) {
	opts := PlanOptions{
		Embedder: fakeEmbedder{v: []float32{1, 2, 3}},
		Vector:   VectorOptions{EmbeddingSpace: "space-a"},
	}
	p := planFor(t, "SELECT id FROM events ORDER BY embedding <-> 'hello' LIMIT 3", opts)
	if p.Shape != ShapeVectorKNN {
		t.Fatalf("shape = %v", p.Shape)
	}
}

func TestPlan_VectorKNNTextDerivesAndVerifiesEmbedderIdentity(t *testing.T) {
	var calls int
	opts := PlanOptions{
		Embedder: fakeEmbedder{
			v: []float32{1, 2, 3}, identity: "space-b", calls: &calls,
		},
		Vector: VectorOptions{EmbeddingSpace: "space-a"},
	}
	st, _ := Parse("SELECT id FROM events ORDER BY embedding <-> 'hello' LIMIT 3")
	b, _ := NewGraphCatalog("graph").Binding("events")
	if _, err := PlanQuery(
		context.Background(), st, b, opts,
	); !errors.Is(err, embeddingspace.ErrMismatch) {
		t.Fatalf("identity mismatch error = %v", err)
	}
	if calls != 0 {
		t.Fatalf("embed calls = %d, want 0 before identity validation", calls)
	}

	opts.Vector.EmbeddingSpace = ""
	p, err := PlanQuery(context.Background(), st, b, opts)
	if err != nil {
		t.Fatal(err)
	}
	if p.VectorEmbeddingSpace != "space-b" || calls != 1 {
		t.Fatalf(
			"derived identity/calls = %q/%d, want space-b/1",
			p.VectorEmbeddingSpace, calls)
	}
}

type identitylessFakeEmbedder struct{ calls *int }

func (f identitylessFakeEmbedder) Embed(
	_ context.Context,
	_ string,
) ([]float32, error) {
	(*f.calls)++
	return []float32{1, 2, 3}, nil
}

func TestPlan_VectorKNNTextRequiresIdentityReportingEmbedder(t *testing.T) {
	var calls int
	st, _ := Parse("SELECT id FROM events ORDER BY embedding <-> 'hello' LIMIT 3")
	b, _ := NewGraphCatalog("graph").Binding("events")
	_, err := PlanQuery(context.Background(), st, b, PlanOptions{
		Embedder: identitylessFakeEmbedder{calls: &calls},
		Vector:   VectorOptions{EmbeddingSpace: "space-a"},
	})
	if !errors.Is(err, embeddingspace.ErrQueryIdentityRequired) {
		t.Fatalf("identityless embedder error = %v", err)
	}
	if calls != 0 {
		t.Fatalf("embed calls = %d, want 0 before identity validation", calls)
	}
}

func TestPlan_VectorKNNRequiresEmbeddingIdentity(t *testing.T) {
	st, _ := Parse("SELECT id FROM events ORDER BY embedding <-> [1,0] LIMIT 3")
	b, _ := NewGraphCatalog("graph").Binding("events")
	if _, err := PlanQuery(
		context.Background(), st, b, PlanOptions{},
	); !errors.Is(err, embeddingspace.ErrQueryIdentityRequired) {
		t.Fatalf("error = %v, want ErrQueryIdentityRequired", err)
	}
}

func TestPlan_VectorKNNRejectsNonCanonicalEmbeddingIdentity(t *testing.T) {
	st, _ := Parse("SELECT id FROM events ORDER BY embedding <-> [1,0] LIMIT 3")
	b, _ := NewGraphCatalog("graph").Binding("events")
	if _, err := PlanQuery(context.Background(), st, b, PlanOptions{
		Vector: VectorOptions{EmbeddingSpace: " space-a "},
	}); !errors.Is(err, embeddingspace.ErrInvalidState) {
		t.Fatalf("non-canonical identity error = %v", err)
	}
}

func TestPlan_OrderByNonVectorColumnErrors(t *testing.T) {
	st, _ := Parse("SELECT id FROM events ORDER BY content <-> [1,2] LIMIT 3")
	b, _ := NewGraphCatalog("graph").Binding("events")
	if _, err := PlanQuery(context.Background(), st, b, PlanOptions{}); err == nil {
		t.Fatal("expected error ordering by non-vector column")
	}
}

func TestPlan_Expand(t *testing.T) {
	p := planFor(t, "SELECT expand(id, 'semantic') AS nbr FROM events WHERE id = 'evt:1'", PlanOptions{})
	if len(p.Projection) != 1 {
		t.Fatalf("proj = %+v", p.Projection)
	}
	c := p.Projection[0]
	if c.Kind != OutExpand || c.Name != "nbr" {
		t.Fatalf("col = %+v", c)
	}
	if string(c.EdgeCF) != string(graphschema.SemanticEdgeCF()) {
		t.Errorf("edgeCF = %q", c.EdgeCF)
	}
}

func TestPlan_StarProjection(t *testing.T) {
	p := planFor(t, "SELECT * FROM events", PlanOptions{})
	if len(p.Projection) != 2 {
		t.Fatalf("proj = %+v", p.Projection)
	}
	if p.Projection[0].Kind != OutRowKey || p.Projection[1].Kind != OutCell {
		t.Errorf("proj = %+v", p.Projection)
	}
	// star => no CF restriction
	if p.ColumnFamilies != nil {
		t.Errorf("cfs = %v", p.ColumnFamilies)
	}
}

func TestPlan_CFRestrictionForContentOnly(t *testing.T) {
	p := planFor(t, "SELECT id, content FROM events", PlanOptions{})
	if len(p.ColumnFamilies) != 1 || !p.CFInclusive {
		t.Fatalf("cfs = %v inclusive=%v", p.ColumnFamilies, p.CFInclusive)
	}
	if string(p.ColumnFamilies[0]) != string(graphschema.ContentCF()) {
		t.Errorf("cf = %q", p.ColumnFamilies[0])
	}
}
