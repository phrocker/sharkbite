package enginebackend

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"strings"
	"testing"

	"github.com/phrocker/shoal-oss/internal/cclient"
	"github.com/phrocker/shoal-oss/internal/embeddingspace"
	"github.com/phrocker/shoal-oss/internal/engine"
	"github.com/phrocker/shoal-oss/internal/graphschema"
	"github.com/phrocker/shoal-oss/internal/shoalql"
	"github.com/phrocker/shoal-oss/internal/tablet"
)

const exactTestSpace = "test-provider:test-model-v1:2:l2"

func packVec(v ...float32) []byte {
	b := make([]byte, 4*len(v))
	for i, f := range v {
		binary.BigEndian.PutUint32(b[i*4:], math.Float32bits(f))
	}
	return b
}

// writeEvent writes one event row with a content cell and an embedding cell.
func writeEvent(t *testing.T, eng *engine.Engine, id, content string, vec []float32) {
	t.Helper()
	m, _ := cclient.NewMutation([]byte(graphschema.EventRowPrefix + id))
	m.PutLatest(graphschema.ContentCF(), []byte{}, nil, []byte(content))
	m.PutLatest(graphschema.VectorCF(), []byte{}, nil, packVec(vec...))
	if err := eng.Write("graph", []*cclient.Mutation{m}); err != nil {
		t.Fatal(err)
	}
}

func newEngineWithEvents(t *testing.T) (*engine.Engine, *shoalql.Executor, shoalql.Catalog) {
	return newEngineWithEventsFormat(t, tablet.FormatRFile)
}

func newEngineWithEventsFormat(t *testing.T, format tablet.FileFormat) (*engine.Engine, *shoalql.Executor, shoalql.Catalog) {
	t.Helper()
	eng, err := engine.Open(t.TempDir(), engine.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if err := eng.CreateTable("graph", engine.TableOptions{
		TabletOptions:    tablet.Options{FileFormat: format},
		DefaultEmbedding: embeddingspace.Has(exactTestSpace),
	}); err != nil {
		t.Fatal(err)
	}
	writeEvent(t, eng, "0001", "retry hit a timeout", []float32{1, 0})
	writeEvent(t, eng, "0002", "everything was fine", []float32{0, 1})
	writeEvent(t, eng, "0003", "another timeout retry loop", []float32{0.9, 0.1})
	if err := eng.Flush("graph"); err != nil {
		t.Fatal(err)
	}
	exec := shoalql.NewExecutor(New(eng))
	return eng, exec, shoalql.NewGraphCatalog("graph")
}

func TestE2E_RFileParquetQueryParity(t *testing.T) {
	queries := []string{
		"SELECT id, content FROM events",
		"SELECT id FROM events WHERE id >= '0002'",
		"SELECT id FROM events WHERE MATCH(content, 'timeout retry')",
		"SELECT id, content FROM events ORDER BY embedding <-> [1, 0] LIMIT 2",
		"SELECT cf, count(*) AS n FROM events GROUP BY cf",
	}
	run := func(format tablet.FileFormat) []string {
		eng, exec, cat := newEngineWithEventsFormat(t, format)
		defer eng.Close()
		results := make([]string, len(queries))
		for i, query := range queries {
			opts := shoalql.PlanOptions{}
			if strings.Contains(query, "<->") {
				opts.Vector.EmbeddingSpace = exactTestSpace
			}
			result := runE2E(t, cat, exec, query, opts)
			results[i] = fmt.Sprint(result.Columns, result.Rows)
		}
		return results
	}
	rfileResults := run(tablet.FormatRFile)
	parquetResults := run(tablet.FormatParquet)
	if fmt.Sprint(parquetResults) != fmt.Sprint(rfileResults) {
		t.Fatalf("Parquet SQL results differ\nrfile:  %v\nparquet: %v", rfileResults, parquetResults)
	}

	mixedEngine, err := engine.Open(t.TempDir(), engine.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer mixedEngine.Close()
	if err := mixedEngine.CreateTable("graph", engine.TableOptions{
		DefaultEmbedding: embeddingspace.Has(exactTestSpace),
	}); err != nil {
		t.Fatal(err)
	}
	writeEvent(t, mixedEngine, "0001", "retry hit a timeout", []float32{1, 0})
	if err := mixedEngine.Flush("graph"); err != nil {
		t.Fatal(err)
	}
	if err := mixedEngine.SetTableFileFormat("graph", tablet.FormatParquet); err != nil {
		t.Fatal(err)
	}
	writeEvent(t, mixedEngine, "0002", "everything was fine", []float32{0, 1})
	writeEvent(t, mixedEngine, "0003", "another timeout retry loop", []float32{0.9, 0.1})
	if err := mixedEngine.Flush("graph"); err != nil {
		t.Fatal(err)
	}
	mixedExec := shoalql.NewExecutor(New(mixedEngine))
	mixedCatalog := shoalql.NewGraphCatalog("graph")
	mixedResults := make([]string, len(queries))
	for i, query := range queries {
		opts := shoalql.PlanOptions{}
		if strings.Contains(query, "<->") {
			opts.Vector.EmbeddingSpace = exactTestSpace
		}
		result := runE2E(t, mixedCatalog, mixedExec, query, opts)
		mixedResults[i] = fmt.Sprint(result.Columns, result.Rows)
	}
	if fmt.Sprint(mixedResults) != fmt.Sprint(rfileResults) {
		t.Fatalf("mixed SQL results differ\nrfile: %v\nmixed: %v", rfileResults, mixedResults)
	}
}

func runE2E(t *testing.T, cat shoalql.Catalog, exec *shoalql.Executor, sql string, opts shoalql.PlanOptions) *shoalql.Result {
	t.Helper()
	stmt, err := shoalql.Parse(sql)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	b, ok := cat.Binding(stmt.Table)
	if !ok {
		t.Fatalf("no binding for %q", stmt.Table)
	}
	p, err := shoalql.PlanQuery(context.Background(), stmt, b, opts)
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	res, err := exec.Run(context.Background(), p)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	return res
}

func TestE2E_ScanAndProject(t *testing.T) {
	eng, exec, cat := newEngineWithEvents(t)
	defer eng.Close()

	res := runE2E(t, cat, exec, "SELECT id, content FROM events", shoalql.PlanOptions{})
	if len(res.Rows) != 3 {
		t.Fatalf("rows = %d", len(res.Rows))
	}
	if res.Rows[0][0].Str != "0001" || res.Rows[0][1].Str != "retry hit a timeout" {
		t.Errorf("row0 = %v", res.Rows[0])
	}
}

func TestE2E_IdRange(t *testing.T) {
	eng, exec, cat := newEngineWithEvents(t)
	defer eng.Close()

	res := runE2E(t, cat, exec, "SELECT id FROM events WHERE id >= '0002'", shoalql.PlanOptions{})
	if len(res.Rows) != 2 {
		t.Fatalf("rows = %d: %+v", len(res.Rows), res.Rows)
	}
	if res.Rows[0][0].Str != "0002" || res.Rows[1][0].Str != "0003" {
		t.Errorf("rows = %+v", res.Rows)
	}
}

func TestE2E_MatchResidual(t *testing.T) {
	eng, exec, cat := newEngineWithEvents(t)
	defer eng.Close()

	res := runE2E(t, cat, exec, "SELECT id FROM events WHERE MATCH(content, 'timeout retry')", shoalql.PlanOptions{})
	if len(res.Rows) != 2 {
		t.Fatalf("rows = %d: %+v", len(res.Rows), res.Rows)
	}
}

func TestE2E_VectorKNN(t *testing.T) {
	eng, exec, cat := newEngineWithEvents(t)
	defer eng.Close()

	// Query near [1,0]; events 0001 and 0003 are closest by cosine.
	res := runE2E(t, cat, exec,
		"SELECT id, content FROM events ORDER BY embedding <-> [1, 0] LIMIT 2",
		shoalql.PlanOptions{Vector: shoalql.VectorOptions{
			EmbeddingSpace: exactTestSpace,
		}})
	if len(res.Rows) != 2 {
		t.Fatalf("rows = %d: %+v", len(res.Rows), res.Rows)
	}
	got := map[string]bool{res.Rows[0][0].Str: true, res.Rows[1][0].Str: true}
	if !got["0001"] || !got["0003"] {
		t.Errorf("expected 0001 and 0003 nearest, got %+v", res.Rows)
	}
	// content must be hydrated (non-empty)
	for _, r := range res.Rows {
		if r[1].Str == "" {
			t.Errorf("content not hydrated: %+v", r)
		}
	}
}

func TestE2E_ExactVectorRejectsSameDimensionDifferentModels(t *testing.T) {
	eng, err := engine.Open(t.TempDir(), engine.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer eng.Close()
	if err := eng.CreateTable("graph", engine.TableOptions{
		DefaultEmbedding: embeddingspace.Has(exactTestSpace),
	}); err != nil {
		t.Fatal(err)
	}
	writeEvent(t, eng, "0001", "space a", []float32{1, 0})
	if err := eng.Flush("graph"); err != nil {
		t.Fatal(err)
	}
	const other = "test-provider:different-model-v1:2:l2"
	if err := eng.SetTableDefaultEmbedding(
		"graph", embeddingspace.Has(other)); err != nil {
		t.Fatal(err)
	}
	writeEvent(t, eng, "0002", "space b", []float32{1, 0})
	if err := eng.Flush("graph"); err != nil {
		t.Fatal(err)
	}

	stmt, _ := shoalql.Parse(
		"SELECT id FROM events ORDER BY embedding <-> [1,0] LIMIT 2")
	binding, _ := shoalql.NewGraphCatalog("graph").Binding("events")
	plan, err := shoalql.PlanQuery(
		context.Background(), stmt, binding,
		shoalql.PlanOptions{Vector: shoalql.VectorOptions{
			EmbeddingSpace: exactTestSpace,
		}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := shoalql.NewExecutor(New(eng)).Run(
		context.Background(), plan,
	); !errors.Is(err, embeddingspace.ErrMismatch) {
		t.Fatalf("exact mixed-space error = %v, want ErrMismatch", err)
	}
}

func TestE2E_ExactVectorFailsClosedOnUnknownAndNoEmbeddings(t *testing.T) {
	for _, tc := range []struct {
		name  string
		state embeddingspace.FileState
		want  error
	}{
		{"unknown", embeddingspace.FileState{}, embeddingspace.ErrQuerySpaceUnknown},
		{"no embeddings", embeddingspace.NoEmbeddings(), embeddingspace.ErrQueryNoEmbeddings},
	} {
		t.Run(tc.name, func(t *testing.T) {
			eng, err := engine.Open(t.TempDir(), engine.Options{})
			if err != nil {
				t.Fatal(err)
			}
			defer eng.Close()
			if err := eng.CreateTable("graph", engine.TableOptions{
				DefaultEmbedding: tc.state,
			}); err != nil {
				t.Fatal(err)
			}
			writeEvent(t, eng, "0001", tc.name, []float32{1, 0})
			if err := eng.Flush("graph"); err != nil {
				t.Fatal(err)
			}
			stmt, _ := shoalql.Parse(
				"SELECT id FROM events ORDER BY embedding <-> [1,0] LIMIT 1")
			binding, _ := shoalql.NewGraphCatalog("graph").Binding("events")
			plan, err := shoalql.PlanQuery(
				context.Background(), stmt, binding,
				shoalql.PlanOptions{Vector: shoalql.VectorOptions{
					EmbeddingSpace: exactTestSpace,
				}})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := shoalql.NewExecutor(New(eng)).Run(
				context.Background(), plan,
			); !errors.Is(err, tc.want) {
				t.Fatalf("error = %v, want %v", err, tc.want)
			}
		})
	}
}

func TestE2E_AggregateCountByCF(t *testing.T) {
	eng, exec, cat := newEngineWithEvents(t)
	defer eng.Close()

	res := runE2E(t, cat, exec, "SELECT cf, count(*) AS n FROM events GROUP BY cf", shoalql.PlanOptions{})
	// two CFs: content and vec, each with 3 cells.
	counts := map[string]float64{}
	for _, r := range res.Rows {
		counts[r[0].Str] = r[1].Num
	}
	if counts[string(graphschema.ContentCF())] != 3 || counts[string(graphschema.VectorCF())] != 3 {
		t.Errorf("counts = %+v", counts)
	}
}
