package accumulobackend_test

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"os"
	"os/exec"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/phrocker/shoal-oss/accumulo"
	"github.com/phrocker/shoal-oss/internal/cclient"
	"github.com/phrocker/shoal-oss/internal/documentschema"
	"github.com/phrocker/shoal-oss/internal/embeddingspace"
	"github.com/phrocker/shoal-oss/internal/engine"
	"github.com/phrocker/shoal-oss/internal/graphschema"
	"github.com/phrocker/shoal-oss/internal/iterrt"
	"github.com/phrocker/shoal-oss/internal/shoalql"
	"github.com/phrocker/shoal-oss/internal/shoalql/accumulobackend"
	"github.com/phrocker/shoal-oss/internal/shoalql/enginebackend"
	"github.com/phrocker/shoal-oss/internal/tablet"
	"github.com/phrocker/shoal-oss/internal/vectorindex"
)

type corpusCell struct {
	table string
	key   accumulo.Key
	value []byte
}

type replayClient struct {
	mu        sync.Mutex
	cells     []corpusCell
	scans     []accumulo.ScannerOptions
	mutations int
}

func (r *replayClient) Scan(
	ctx context.Context,
	table accumulo.Table,
	ranges []*accumulo.Range,
	options accumulo.ScannerOptions,
) ([]accumulo.KeyValue, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	r.mu.Lock()
	r.scans = append(r.scans, options)
	r.mu.Unlock()
	var out []accumulo.KeyValue
	for _, cell := range r.cells {
		if cell.table != table.Name || !inRanges(cell.key, ranges) ||
			!selectedColumn(cell.key, options.Columns) {
			continue
		}
		out = append(out, accumulo.KeyValue{Key: cell.key.Clone(), Value: append([]byte(nil), cell.value...)})
	}
	return out, nil
}

func (r *replayClient) Write(
	ctx context.Context,
	_ accumulo.Table,
	mutations []*accumulo.Mutation,
	_ accumulo.BatchWriterOptions,
) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.mutations += len(mutations)
	return nil
}

func inRanges(key accumulo.Key, ranges []*accumulo.Range) bool {
	for _, scanRange := range ranges {
		if scanRange.Contains(key) {
			return true
		}
	}
	return false
}

func selectedColumn(key accumulo.Key, columns []accumulo.Column) bool {
	if len(columns) == 0 {
		return true
	}
	for _, column := range columns {
		if string(column.Family()) != string(key.ColumnFamily) {
			continue
		}
		qualifier := column.Qualifier()
		if qualifier == nil || string(qualifier) == string(key.ColumnQualifier) {
			return true
		}
	}
	return false
}

func packedVector(values ...float32) []byte {
	out := make([]byte, len(values)*4)
	for i, value := range values {
		binary.BigEndian.PutUint32(out[i*4:], math.Float32bits(value))
	}
	return out
}

func buildCorpus() []corpusCell {
	var cells []corpusCell
	put := func(table, row, cf, cq, cv string, ts int64, value []byte) {
		cells = append(cells, corpusCell{
			table: table,
			key:   accumulo.NewKeyWithColumns([]byte(row), []byte(cf), []byte(cq), []byte(cv), ts),
			value: append([]byte(nil), value...),
		})
	}
	for _, event := range []struct {
		id      string
		content string
		vector  []byte
	}{
		{"0001", "retry hit a timeout", packedVector(1, 0)},
		{"0002", "everything was fine", packedVector(0, 1)},
		{"0003", "another timeout retry loop", packedVector(.9, .1)},
	} {
		row := graphschema.EventRowPrefix + event.id
		put("graph", row, string(graphschema.ContentCF()), "", "", 100, []byte(event.content))
		put("graph", row, string(graphschema.VectorCF()), "", "", 100, event.vector)
	}
	put("graph", graphschema.EventRowPrefix+"0001", string(graphschema.SemanticEdgeCF()), graphschema.EventRowPrefix+"0002", "", 100, []byte("1"))
	put("graph", graphschema.EventRowPrefix+"0001", string(graphschema.SemanticEdgeCF()), graphschema.EventRowPrefix+"0003", "", 100, []byte("0.9"))
	put("graph", graphschema.EventRowPrefix+"0004", string(graphschema.ContentCF()), "", "", 100, []byte("old value"))
	put("graph", graphschema.EventRowPrefix+"0004", string(graphschema.ContentCF()), "", "", 200, []byte("new value"))
	put("graph", graphschema.EventRowPrefix+"0004", string(graphschema.VectorCF()), "", "", 100, packedVector(1, 0))
	put("graph", graphschema.EventRowPrefix+"0004", string(graphschema.VectorCF()), "", "", 200, packedVector(0, 1))
	deletedEdge := accumulo.NewKeyWithColumns(
		[]byte(graphschema.EventRowPrefix+"0001"),
		graphschema.SemanticEdgeCF(),
		[]byte(graphschema.EventRowPrefix+"0005"),
		nil,
		200,
	)
	deletedEdge.Deleted = true
	cells = append(cells,
		corpusCell{"graph", accumulo.NewKeyWithColumns(
			[]byte(graphschema.EventRowPrefix+"0001"),
			graphschema.SemanticEdgeCF(),
			[]byte(graphschema.EventRowPrefix+"0005"),
			nil,
			100,
		), []byte("0.8")},
		corpusCell{"graph", deletedEdge, nil},
	)

	docs := []struct {
		shard, uid, sender, subject, body string
	}{
		{"20240101_1", "u1", "alice", "hello", "the timeout retry loop"},
		{"20240101_2", "u2", "bob", "hello", "everything was fine"},
		{"20240102_1", "u3", "alice", "bye", "another timeout retry"},
	}
	for _, doc := range docs {
		eventCF := string(documentschema.EventCF("email", doc.uid))
		for field, value := range map[string]string{"SENDER": doc.sender, "SUBJECT": doc.subject} {
			put("docs", doc.shard, eventCF, string(documentschema.EventCQ(field, value)), "", 100, nil)
			put("docs", doc.shard, string(documentschema.FieldIndexCF(field)),
				string(documentschema.FieldIndexCQ(value, "email", doc.uid)), "", 100, nil)
			uidList := documentschema.UidList{Count: 1, UIDs: []string{doc.uid}}
			put("docsIndex", value, string(documentschema.IndexCF(field)),
				string(documentschema.IndexCQ(doc.shard, "email")), "", 100, uidList.Encode())
		}
		put("docs", doc.shard, eventCF, string(documentschema.EventCQ("BODY", doc.body)), "", 100, nil)
		for _, token := range strings.Fields(doc.body) {
			put("docs", doc.shard, string(documentschema.FieldIndexCF("BODY")),
				string(documentschema.FieldIndexCQ(token, "email", doc.uid)), "", 100, nil)
			uidList := documentschema.UidList{Count: 1, UIDs: []string{doc.uid}}
			put("docsIndex", token, string(documentschema.IndexCF("BODY")),
				string(documentschema.IndexCQ(doc.shard, "email")), "", 100, uidList.Encode())
		}
	}
	sort.SliceStable(cells, func(i, j int) bool {
		if cells[i].table != cells[j].table {
			return cells[i].table < cells[j].table
		}
		return cells[i].key.Compare(cells[j].key) < 0
	})
	return cells
}

func openCorpusEngine(t *testing.T, mode string, cells []corpusCell) *engine.Engine {
	t.Helper()
	eng, err := engine.Open(t.TempDir(), engine.Options{})
	if err != nil {
		t.Fatal(err)
	}
	format := tablet.FormatRFile
	if mode == "parquet" {
		format = tablet.FormatParquet
	}
	for _, tableName := range []string{"graph", "docs", "docsIndex"} {
		if err := eng.CreateTable(tableName, engine.TableOptions{
			TabletOptions: tablet.Options{FileFormat: format},
		}); err != nil {
			t.Fatal(err)
		}
	}
	mid := len(cells)
	if mode == "mixed" {
		mid /= 2
	}
	writeCorpusCells(t, eng, cells[:mid])
	for _, tableName := range []string{"graph", "docs", "docsIndex"} {
		if err := eng.Flush(tableName); err != nil {
			t.Fatal(err)
		}
	}
	if mode == "mixed" {
		for _, tableName := range []string{"graph", "docs", "docsIndex"} {
			if err := eng.SetTableFileFormat(tableName, tablet.FormatParquet); err != nil {
				t.Fatal(err)
			}
		}
		writeCorpusCells(t, eng, cells[mid:])
		for _, tableName := range []string{"graph", "docs", "docsIndex"} {
			if err := eng.Flush(tableName); err != nil {
				t.Fatal(err)
			}
		}
	}
	return eng
}

func writeCorpusCells(t *testing.T, eng *engine.Engine, cells []corpusCell) {
	t.Helper()
	byTable := map[string]map[string]*cclient.Mutation{}
	for _, cell := range cells {
		rows := byTable[cell.table]
		if rows == nil {
			rows = map[string]*cclient.Mutation{}
			byTable[cell.table] = rows
		}
		mutation := rows[string(cell.key.Row)]
		if mutation == nil {
			mutation, _ = cclient.NewMutation(cell.key.Row)
			rows[string(cell.key.Row)] = mutation
		}
		if cell.key.Deleted {
			mutation.Delete(cell.key.ColumnFamily, cell.key.ColumnQualifier,
				cell.key.ColumnVisibility, cell.key.Timestamp)
		} else {
			mutation.Put(cell.key.ColumnFamily, cell.key.ColumnQualifier,
				cell.key.ColumnVisibility, cell.key.Timestamp, cell.value)
		}
	}
	for tableName, rows := range byTable {
		mutations := make([]*cclient.Mutation, 0, len(rows))
		for _, mutation := range rows {
			mutations = append(mutations, mutation)
		}
		if err := eng.Write(tableName, mutations); err != nil {
			t.Fatal(err)
		}
	}
}

func runQuery(
	t *testing.T,
	executor *shoalql.Executor,
	catalog shoalql.Catalog,
	query string,
) string {
	t.Helper()
	stmt, err := shoalql.Parse(query)
	if err != nil {
		t.Fatal(err)
	}
	binding, ok := catalog.Binding(stmt.Table)
	if !ok {
		t.Fatalf("missing binding for %s", stmt.Table)
	}
	plan, err := shoalql.PlanQuery(
		context.Background(), stmt, binding,
		shoalql.PlanOptions{Vector: shoalql.VectorOptions{
			EmbeddingSpace: "test-provider:test-model-v1:2:l2",
		}})
	if err != nil {
		t.Fatal(err)
	}
	result, err := executor.Run(context.Background(), plan)
	if err != nil {
		t.Fatal(err)
	}
	return fmt.Sprint(result.Columns, result.Rows)
}

func TestCrossBackendCorpusParity(t *testing.T) {
	cells := buildCorpus()
	graphQueries := []string{
		"SELECT id, content FROM events",
		"SELECT id FROM events WHERE id >= '0002'",
		"SELECT id FROM events WHERE MATCH(content, 'timeout retry')",
		"SELECT expand(id, 'semantic') FROM events WHERE id = '0001'",
		"SELECT id, content FROM events AS OF 150 WHERE id = '0004'",
		"SELECT expand(id, 'semantic') FROM events AS OF 150 WHERE id = '0001'",
	}
	documentQueries := []string{
		"SELECT id, SUBJECT FROM emails WHERE SENDER = 'alice'",
		"SELECT id FROM emails WHERE MATCH(BODY, 'timeout retry')",
	}
	var baseline []string
	for _, mode := range []string{"rfile", "parquet", "mixed"} {
		eng := openCorpusEngine(t, mode, cells)
		executor := shoalql.NewExecutor(enginebackend.New(eng))
		var got []string
		for _, query := range graphQueries {
			got = append(got, runQuery(t, executor, shoalql.NewGraphCatalog("graph"), query))
		}
		for _, query := range documentQueries {
			got = append(got, runQuery(t, executor,
				shoalql.NewDocumentCatalog("emails", "docs", "docsIndex"), query))
		}
		eng.Close()
		if baseline == nil {
			baseline = got
		} else if fmt.Sprint(got) != fmt.Sprint(baseline) {
			t.Fatalf("%s corpus differs\nbaseline=%v\ngot=%v", mode, baseline, got)
		}
	}

	replay := &replayClient{cells: cells}
	backend := accumulobackend.New(replay, accumulobackend.Options{
		BatchSize:          2,
		Parallelism:        3,
		StorageFormats:     []string{"rfile", "parquet"},
		HistoricalVersions: true,
	})
	executor := shoalql.NewExecutor(backend)
	var distributed []string
	for _, query := range graphQueries {
		distributed = append(distributed,
			runQuery(t, executor, shoalql.NewGraphCatalog("graph"), query))
	}
	for _, query := range documentQueries {
		distributed = append(distributed, runQuery(t, executor,
			shoalql.NewDocumentCatalog("emails", "docs", "docsIndex"), query))
	}
	if fmt.Sprint(distributed) != fmt.Sprint(baseline) {
		t.Fatalf("Accumulo replay differs\nlocal=%v\naccumulo=%v", baseline, distributed)
	}
	if !strings.Contains(distributed[3], "0002") || !strings.Contains(distributed[3], "0003") {
		t.Fatalf("graph traversal corpus did not exercise neighbors: %s", distributed[3])
	}
	if len(replay.scans) == 0 || replay.scans[0].BatchSize != 2 {
		t.Fatalf("scanner pagination options not propagated: %+v", replay.scans)
	}
}

func TestAccumuloExactVectorFailsWithoutPerFileMetadata(t *testing.T) {
	cells := buildCorpus()
	replay := &replayClient{cells: cells}
	executor := shoalql.NewExecutor(accumulobackend.New(replay, accumulobackend.Options{
		Authorizations: [][]byte{[]byte("A")},
		BatchSize:      1,
	}))
	stmt, err := shoalql.Parse(
		"SELECT id FROM events ORDER BY embedding <-> [1,0] LIMIT 2")
	if err != nil {
		t.Fatal(err)
	}
	binding, _ := shoalql.NewGraphCatalog("graph").Binding("events")
	plan, err := shoalql.PlanQuery(
		context.Background(), stmt, binding,
		shoalql.PlanOptions{Vector: shoalql.VectorOptions{
			EmbeddingSpace: "test-provider:test-model-v1:2:l2",
		}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := executor.Run(
		context.Background(), plan,
	); !errors.Is(err, embeddingspace.ErrQueryMetadataMissing) {
		t.Fatalf("error = %v, want ErrQueryMetadataMissing", err)
	}
	if len(replay.scans) != 0 {
		t.Fatalf("unsafe exact vector query reached scanner: %+v", replay.scans)
	}
}

func TestEmptyAuthorizationsDoNotBecomeSystemVisibility(t *testing.T) {
	cells := []corpusCell{
		{"graph", accumulo.NewKeyWithColumns([]byte("evt:public"), graphschema.ContentCF(), nil, nil, 1), []byte("public")},
		{"graph", accumulo.NewKeyWithColumns([]byte("evt:secret"), graphschema.ContentCF(), nil, []byte("SECRET"), 1), []byte("secret")},
	}
	executor := shoalql.NewExecutor(accumulobackend.New(&replayClient{cells: cells}, accumulobackend.Options{}))
	got := runQuery(t, executor, shoalql.NewGraphCatalog("graph"), "SELECT id, content FROM events")
	if !strings.Contains(got, "public") || strings.Contains(got, "secret") {
		t.Fatalf("empty authorization result = %s", got)
	}
}

func TestNeighborsPreserveDuplicateInputAlignment(t *testing.T) {
	cells := buildCorpus()
	backend := accumulobackend.New(&replayClient{cells: cells}, accumulobackend.Options{})
	row := []byte(graphschema.EventRowPrefix + "0001")
	got, err := backend.Neighbors(context.Background(), "graph", [][]byte{row, row},
		graphschema.SemanticEdgeCF())
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || len(got[0]) != 2 || len(got[1]) != 2 {
		t.Fatalf("duplicate neighbor alignment = %+v", got)
	}
}

func TestCancellationAndWriterIntegration(t *testing.T) {
	replay := &replayClient{}
	backend := accumulobackend.New(replay, accumulobackend.Options{})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := backend.Scan(ctx, "graph", shoalqlRangeAll(), shoalql.ScanRequest{})
	if err == nil {
		t.Fatal("cancelled scan succeeded")
	}
	key := accumulo.NewKeyWithColumns([]byte("r"), []byte("f"), []byte("q"), nil, 1)
	err = backend.WriteCells(context.Background(), "graph", []shoalql.Cell{{
		Key: (&keyAsIterator{Key: key}).iteratorKey(),
	}}, accumulo.BatchWriterOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if replay.mutations != 1 {
		t.Fatalf("writer mutations = %d", replay.mutations)
	}
}

func TestExplainDeclaresExactFallbackAndApproximateUnsupported(t *testing.T) {
	backend := accumulobackend.New(&replayClient{}, accumulobackend.Options{})
	executor := shoalql.NewExecutor(backend)
	stmt, err := shoalql.Parse(
		"EXPLAIN FORMAT JSON SELECT id FROM events ORDER BY embedding <-> [1,0] LIMIT 2",
	)
	if err != nil {
		t.Fatal(err)
	}

	binding, _ := shoalql.NewGraphCatalog("graph").Binding("events")
	plan, err := shoalql.PlanQuery(
		context.Background(), stmt, binding,
		shoalql.PlanOptions{Vector: shoalql.VectorOptions{
			EmbeddingSpace: "test-provider:test-model-v1:2:l2",
		}})
	if err != nil {
		t.Fatal(err)
	}

	result, err := executor.Run(context.Background(), plan)
	if err != nil {
		t.Fatal(err)
	}
	explain := result.Rows[0][0].Str
	for _, want := range []string{
		`"mode":"distributed"`,
		`"approximate_vector_index"`,
		`distributed IVF-PQ build, freshness, and routing lifecycle is unavailable`,
		`exact vector search is refused until metadata-table per-file embedding states are supplied`,
		`globally key-sorted`,
		`exact vector execution is refused because the backend cannot prove per-file embedding identity`,
		`"vector_embedding_space":"test-provider:test-model-v1:2:l2"`,
	} {
		if !strings.Contains(explain, want) {
			t.Fatalf("EXPLAIN missing %q: %s", want, explain)
		}
	}
}

func TestAsOfRequiresHistoricalScannerContract(t *testing.T) {
	backend := accumulobackend.New(&replayClient{cells: buildCorpus()}, accumulobackend.Options{})
	executor := shoalql.NewExecutor(backend)
	stmt, err := shoalql.Parse("SELECT id FROM events AS OF 150 WHERE id = '0004'")
	if err != nil {
		t.Fatal(err)
	}

	binding, _ := shoalql.NewGraphCatalog("graph").Binding("events")
	plan, err := shoalql.PlanQuery(context.Background(), stmt, binding, shoalql.PlanOptions{})
	if err != nil {
		t.Fatal(err)
	}
	_, err = executor.Run(context.Background(), plan)
	if !errors.Is(err, accumulobackend.ErrHistoricalVersionsUnavailable) {
		t.Fatalf("error = %v, want historical-version contract error", err)
	}
}

func TestConfiguredVectorIndexDeclaresAndRunsDistributedApproximateSearch(t *testing.T) {
	const embeddingSpace = "test-provider:test-model-v1:4:l2"
	store := vectorindex.NewMemoryStore()
	manager := vectorindex.New(store, vectorindex.Config{
		NList: 2, Subspaces: 2, CentroidsPerSpace: 2, MaxIterations: 8,
		Seed: 9, ShardCount: 2,
	})
	records := []vectorindex.VectorRecord{
		{ID: "evt:a", Vector: []float32{1, 0, 0, 0}, EmbeddingSpace: embeddingSpace, Timestamp: 10, Visibility: "public",
			Document: vectorindex.DocumentRef{Row: "evt:a"}},
		{ID: "evt:b", Vector: []float32{0, 1, 0, 0}, EmbeddingSpace: embeddingSpace, Timestamp: 10, Visibility: "secret",
			Document: vectorindex.DocumentRef{Row: "evt:b"}},
	}
	if _, err := manager.Build(context.Background(), "graph_ivf", records, 10); err != nil {
		t.Fatal(err)
	}
	backend := accumulobackend.New(&replayClient{}, accumulobackend.Options{
		Authorizations: [][]byte{[]byte("public")},
		VectorSearcher: shoalql.ManagedVectorSearcher{Manager: manager},
	})
	info := backend.BackendInfo()
	if !containsCapability(info.Capabilities, shoalql.CapabilityApproximateVector) ||
		!containsCapability(info.Capabilities, shoalql.CapabilityDistributedTopK) {
		t.Fatalf("vector capabilities missing: %v", info.Capabilities)
	}
	hits, evidence, err := backend.SearchVector(context.Background(), shoalql.VectorSearchRequest{
		Index: "graph_ivf", Query: []float32{1, 0, 0, 0}, TopK: 2, NProbe: 2,
		EmbeddingSpace: embeddingSpace,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 1 || hits[0].ID != "evt:a" || evidence.Generation != 1 {
		t.Fatalf("hits=%+v evidence=%+v", hits, evidence)
	}
}

func containsCapability(values []shoalql.Capability, want shoalql.Capability) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

// These tiny helpers avoid coupling this external test package to unexported
// planner range constructors.
func shoalqlRangeAll() iterrt.Range { return iterrt.InfiniteRange() }

type keyAsIterator struct{ Key accumulo.Key }

func (k *keyAsIterator) iteratorKey() *iterrt.Key {
	return &iterrt.Key{
		Row:              k.Key.Row,
		ColumnFamily:     k.Key.ColumnFamily,
		ColumnQualifier:  k.Key.ColumnQualifier,
		ColumnVisibility: k.Key.ColumnVisibility,
		Timestamp:        k.Key.Timestamp,
	}
}

func TestLiveAccumuloHarness(t *testing.T) {
	if os.Getenv("SHOAL_ACCUMULO_LIVE") == "" {
		t.Skip("set SHOAL_ACCUMULO_LIVE=1 to run the optional live harness")
	}
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("live Accumulo harness unsupported: Docker is unavailable")
	}
	t.Skip("live harness requires an externally configured Accumulo endpoint")
}
