package shoalql

import (
	"context"
	"testing"
)

func planDoc(t *testing.T, sql string) *Plan {
	t.Helper()
	stmt, err := Parse(sql)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	b, ok := NewDocumentCatalog("emails", "docs", "docsIndex").Binding("emails")
	if !ok {
		t.Fatal("no binding")
	}
	p, err := PlanQuery(context.Background(), stmt, b, PlanOptions{})
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	return p
}

func TestPlanDoc_ShapeAndTables(t *testing.T) {
	p := planDoc(t, "SELECT id, SUBJECT FROM emails WHERE SENDER = 'alice'")
	if p.Shape != ShapeDocument {
		t.Fatalf("shape = %v", p.Shape)
	}
	if p.Table != "docs" || p.IndexTable != "docsIndex" {
		t.Errorf("tables = %q / %q", p.Table, p.IndexTable)
	}
	if len(p.DocTerms) != 1 || p.DocTerms[0].Field != "SENDER" || p.DocTerms[0].Value != "alice" {
		t.Errorf("terms = %+v", p.DocTerms)
	}
	if len(p.Projection) != 2 || p.Projection[0].Kind != OutDocID ||
		p.Projection[1].Kind != OutDocField || p.Projection[1].Field != "SUBJECT" {
		t.Errorf("proj = %+v", p.Projection)
	}
}

func TestPlanDoc_MatchExpandsToTokenTerms(t *testing.T) {
	p := planDoc(t, "SELECT id FROM emails WHERE MATCH(BODY, 'timeout retry loop')")
	if len(p.DocTerms) != 3 {
		t.Fatalf("terms = %+v", p.DocTerms)
	}
	for i, want := range []string{"timeout", "retry", "loop"} {
		if p.DocTerms[i].Field != "BODY" || p.DocTerms[i].Value != want {
			t.Errorf("term %d = %+v", i, p.DocTerms[i])
		}
	}
}

func TestPlanDoc_IdTypeResiduals(t *testing.T) {
	p := planDoc(t, "SELECT id, type FROM emails WHERE SENDER = 'alice' AND id = 'u3' AND type = 'email'")
	if len(p.DocTerms) != 1 {
		t.Fatalf("terms = %+v", p.DocTerms)
	}
	if len(p.DocResidual) != 2 {
		t.Fatalf("residuals = %+v", p.DocResidual)
	}
	seen := map[string]string{}
	for _, r := range p.DocResidual {
		seen[r.Special] = r.Value
	}
	if seen["id"] != "u3" || seen["type"] != "email" {
		t.Errorf("residuals = %+v", p.DocResidual)
	}
}

func TestPlanDoc_Star(t *testing.T) {
	p := planDoc(t, "SELECT * FROM emails WHERE SENDER = 'bob'")
	if !p.DocStar {
		t.Fatal("expected DocStar")
	}
	if len(p.Projection) != 0 {
		t.Errorf("star projection should be deferred, got %+v", p.Projection)
	}
}

func TestPlanDoc_RequiresIndexedPredicate(t *testing.T) {
	stmt, err := Parse("SELECT id FROM emails WHERE id = 'u1'")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	b, _ := NewDocumentCatalog("emails", "docs", "docsIndex").Binding("emails")
	if _, err := PlanQuery(context.Background(), stmt, b, PlanOptions{}); err == nil {
		t.Fatal("want error: id-only query has no indexed predicate")
	}
}

func TestPlanDoc_RejectsRangeFilter(t *testing.T) {
	stmt, _ := Parse("SELECT id FROM emails WHERE SENDER > 'a'")
	b, _ := NewDocumentCatalog("emails", "docs", "docsIndex").Binding("emails")
	if _, err := PlanQuery(context.Background(), stmt, b, PlanOptions{}); err == nil {
		t.Fatal("want error: document filters support = and MATCH only")
	}
}

func TestPlanDoc_RejectsGroupByAndOrder(t *testing.T) {
	b, _ := NewDocumentCatalog("emails", "docs", "docsIndex").Binding("emails")
	for _, sql := range []string{
		"SELECT type, count(*) FROM emails WHERE SENDER = 'a' GROUP BY type",
		"SELECT id FROM emails WHERE SENDER = 'a' ORDER BY embedding <-> [1,0]",
	} {
		stmt, err := Parse(sql)
		if err != nil {
			continue // grammar may reject some forms earlier; that is fine
		}
		if _, err := PlanQuery(context.Background(), stmt, b, PlanOptions{}); err == nil {
			t.Errorf("want error for %q", sql)
		}
	}
}
