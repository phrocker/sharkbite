package enginebackend

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/phrocker/shoal-oss/internal/shoalql"
)

func explainDetails(t *testing.T, cat shoalql.Catalog, exec *shoalql.Executor, sql string) shoalql.ExplainDetails {
	t.Helper()
	opts := shoalql.PlanOptions{}
	if strings.Contains(sql, "<->") {
		opts.Vector.EmbeddingSpace = exactTestSpace
	}
	res := runE2E(t, cat, exec, "EXPLAIN FORMAT JSON "+sql, opts)
	if len(res.Rows) != 1 {
		t.Fatalf("explain rows = %d", len(res.Rows))
	}
	var details shoalql.ExplainDetails
	if err := json.Unmarshal([]byte(res.Rows[0][0].Str), &details); err != nil {
		t.Fatalf("decode explain: %v", err)
	}
	return details
}

func TestLocalBackendCapabilityContract(t *testing.T) {
	eng, _, _ := newEngineWithEvents(t)
	defer eng.Close()

	info := New(eng).BackendInfo()
	if info.Name != "embedded-engine" || info.Mode != "local" ||
		info.SelectedStorageFormat != "rfile" {
		t.Fatalf("backend info = %+v", info)
	}
	want := []shoalql.Capability{
		shoalql.CapabilityRangeScan,
		shoalql.CapabilityExactVectorKNN,
		shoalql.CapabilityDocumentIndex,
	}
	for _, capability := range want {
		found := false
		for _, declared := range info.Capabilities {
			found = found || declared == capability
		}
		if !found {
			t.Errorf("missing capability %q in %v", capability, info.Capabilities)
		}
	}
}

func TestLocalBackendExplainWorkloadFixtures(t *testing.T) {
	graphEng, graphExec, graphCat := newEngineWithEvents(t)
	defer graphEng.Close()
	docEng, docExec, docCat := newEngineWithDocs(t)
	defer docEng.Close()

	fixtures := []struct {
		name        string
		cat         shoalql.Catalog
		exec        *shoalql.Executor
		sql         string
		shape       string
		pushdown    string
		materialize string
	}{
		{
			name: "graph range",
			cat:  graphCat, exec: graphExec,
			sql:   "SELECT id FROM events WHERE id >= '0002'",
			shape: "scan", pushdown: `["evt:0002","evt;")`,
		},
		{
			name: "graph expansion",
			cat:  graphCat, exec: graphExec,
			sql:         "SELECT expand(id, 'semantic') FROM events WHERE id = '0001'",
			shape:       "scan",
			materialize: "resolve graph neighbors",
		},
		{
			name: "term fallback",
			cat:  graphCat, exec: graphExec,
			sql:   "SELECT id FROM events WHERE MATCH(content, 'timeout retry')",
			shape: "scan", materialize: "evaluate MATCH",
		},
		{
			name: "exact vector",
			cat:  graphCat, exec: graphExec,
			sql:   "SELECT id, content FROM events ORDER BY embedding <-> [1,0] LIMIT 2",
			shape: "vector_knn", pushdown: "exact vector KNN", materialize: "hydrate",
		},
		{
			name: "document index",
			cat:  docCat, exec: docExec,
			sql:   "SELECT id FROM emails WHERE SENDER = 'alice'",
			shape: "document", pushdown: `document index term SENDER="alice"`,
			materialize: "reconstruct and project documents",
		},
	}
	for _, fixture := range fixtures {
		t.Run(fixture.name, func(t *testing.T) {
			details := explainDetails(t, fixture.cat, fixture.exec, fixture.sql)
			if details.Backend.Name != "embedded-engine" || details.Backend.Mode != "local" {
				t.Fatalf("backend = %+v", details.Backend)
			}
			if details.Shape != fixture.shape {
				t.Fatalf("shape = %q want %q", details.Shape, fixture.shape)
			}
			if fixture.pushdown != "" &&
				!strings.Contains(strings.Join(details.Pushdowns, " "), fixture.pushdown) {
				t.Fatalf("pushdowns = %v", details.Pushdowns)
			}
			if fixture.materialize != "" &&
				!strings.Contains(strings.Join(details.LocalMaterialization, " "), fixture.materialize) {
				t.Fatalf("materialization = %v", details.LocalMaterialization)
			}
		})
	}
}
