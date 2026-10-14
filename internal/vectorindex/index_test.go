package vectorindex

import (
	"context"
	"errors"
	"fmt"
	"math"
	"reflect"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/phrocker/shoal-oss/internal/embeddingspace"
)

const testEmbeddingSpace = "test-provider:test-model-v1:normalized"

func testConfig() Config {
	return Config{
		NList: 8, Subspaces: 2, CentroidsPerSpace: 16, MaxIterations: 20,
		Seed: 73, TrainingSamples: 0, ShardCount: 4,
		CreatedAt: func() time.Time { return time.UnixMilli(1700000000000) },
	}
}

func corpus(n int) []VectorRecord {
	out := make([]VectorRecord, n)
	for i := range out {
		angle := 2 * math.Pi * float64(i) / float64(n)
		vector := []float32{
			float32(math.Cos(angle)), float32(math.Sin(angle)),
			float32(math.Cos(2 * angle)), float32(math.Sin(2 * angle)),
			float32(math.Cos(3 * angle)), float32(math.Sin(3 * angle)),
			float32(math.Cos(5 * angle)), float32(math.Sin(5 * angle)),
		}
		out[i] = VectorRecord{
			ID: fmt.Sprintf("doc-%03d", i), Vector: vector,
			EmbeddingSpace: testEmbeddingSpace, Timestamp: int64(100 + i),
			Document: DocumentRef{
				Row: fmt.Sprintf("evt:%03d", i), Shard: fmt.Sprintf("20240101_%d", i%5),
				Datatype: "email", UID: fmt.Sprintf("u%03d", i),
				Metadata: map[string]string{"ordinal": fmt.Sprint(i)},
			},
		}
	}
	return out
}

func TestBuildIsDeterministicAndFormatNeutral(t *testing.T) {
	ctx := context.Background()
	input := corpus(96)
	build := func(records []VectorRecord) (*MemoryStore, Manifest) {
		store := NewMemoryStore()
		manager := New(store, testConfig())
		manifest, err := manager.Build(ctx, "docs_ivf", records, 500)
		if err != nil {
			t.Fatal(err)
		}
		return store, manifest
	}
	left, lm := build(input)
	reversed := append([]VectorRecord(nil), input...)
	sort.Slice(reversed, func(i, j int) bool { return reversed[i].ID > reversed[j].ID })
	right, rm := build(reversed)
	if lm.CodebookVersion != rm.CodebookVersion {
		t.Fatalf("codebook versions differ: %s != %s", lm.CodebookVersion, rm.CodebookVersion)
	}
	if !reflect.DeepEqual(left.Export("docs_ivf", 0), right.Export("docs_ivf", 0)) {
		t.Fatal("deterministic builds produced different persisted records")
	}
}

func TestTrainingSampleClampsCodebookSizes(t *testing.T) {
	cfg := testConfig()
	cfg.NList = 32
	cfg.CentroidsPerSpace = 32
	cfg.TrainingSamples = 8
	store := NewMemoryStore()
	manifest, err := New(store, cfg).Build(context.Background(), "sampled", corpus(64), 1)
	if err != nil {
		t.Fatal(err)
	}
	if manifest.NList != 8 || manifest.CentroidsPerSpace != 8 {
		t.Fatalf("sampled manifest = %+v", manifest)
	}
}

func TestApproximateRecallContractRequiresBenchmark(t *testing.T) {
	ctx := context.Background()
	records := corpus(128)
	store := NewMemoryStore()
	manager := New(store, testConfig())
	if _, err := manager.Build(ctx, "docs_ivf", records, 1000); err != nil {
		t.Fatal(err)
	}
	queries := make([]BenchmarkQuery, 0, 24)
	for i := 0; i < 24; i++ {
		queries = append(queries, BenchmarkQuery{
			Name: records[i*3].ID, Vector: records[i*3].Vector,
			EmbeddingSpace: testEmbeddingSpace,
		})
	}

	result, err := BenchmarkRecall(ctx, manager, "docs_ivf", records, queries, 10, 8, 0.80, "test-corpus-v1")
	if err != nil {
		t.Fatal(err)
	}

	if !result.Passed {
		t.Fatalf("recall %.4f below threshold %.4f", result.Recall.Measured, result.Recall.Minimum)
	}
	if result.Recall.EmbeddingSpace != testEmbeddingSpace ||
		result.Recall.Generation == 0 ||
		result.Recall.CodebookVersion == "" {
		t.Fatalf("benchmark identity binding missing: %+v", result.Recall)
	}
	manifest, err := manager.SetRecallContract(ctx, "docs_ivf", result.Recall)
	if err != nil {
		t.Fatal(err)
	}
	if manifest.Recall.Generation != manifest.Generation {
		t.Fatalf("recall evidence generation=%d, manifest=%d",
			manifest.Recall.Generation, manifest.Generation)
	}
	_, evidence, err := manager.Search(ctx, "docs_ivf", Query{
		Vector: records[7].Vector, EmbeddingSpace: testEmbeddingSpace,
		TopK: 10, NProbe: 8,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !evidence.RecallClaimed || evidence.Recall.BenchmarkRef != "test-corpus-v1" {
		t.Fatalf("benchmark evidence missing: %+v", evidence)
	}
	foreign := result.Recall
	foreign.EmbeddingSpace = "test-provider:foreign-model:normalized"
	if _, err := manager.SetRecallContract(
		ctx, "docs_ivf", foreign,
	); !errors.Is(err, embeddingspace.ErrMismatch) {
		t.Fatalf("foreign recall contract error = %v", err)
	}
	if _, err := manager.Update(ctx, "docs_ivf", []VectorRecord{{
		ID: "removed", Tombstone: true, Timestamp: 2000,
	}}, 2000); err != nil {
		t.Fatal(err)
	}
	_, evidence, err = manager.Search(ctx, "docs_ivf", Query{
		Vector: records[7].Vector, EmbeddingSpace: testEmbeddingSpace,
		TopK: 10, NProbe: 8,
	})
	if err != nil {
		t.Fatal(err)
	}
	if evidence.RecallClaimed {
		t.Fatalf("updated corpus retained stale recall: %+v", evidence)
	}
	if _, err := manager.SetRecallContract(
		ctx, "docs_ivf", result.Recall,
	); err == nil || !strings.Contains(err.Error(), "generation/codebook") {
		t.Fatalf("stale recall contract error = %v", err)
	}
}

func TestBenchmarkRecallRejectsMalformedCorpusDimension(t *testing.T) {
	ctx := context.Background()
	records := corpus(32)
	manager := New(NewMemoryStore(), testConfig())
	if _, err := manager.Build(ctx, "docs_ivf", records, 1000); err != nil {
		t.Fatal(err)
	}
	malformed := append([]VectorRecord(nil), records...)
	malformed[7] = records[7]
	malformed[7].Vector = append([]float32(nil), records[7].Vector[:len(records[7].Vector)-1]...)
	_, err := BenchmarkRecall(
		ctx,
		manager,
		"docs_ivf",
		malformed,
		[]BenchmarkQuery{{
			Name:           records[0].ID,
			Vector:         records[0].Vector,
			EmbeddingSpace: testEmbeddingSpace,
		}},
		10,
		8,
		0.80,
		"test-corpus-v1",
	)
	if err == nil || !strings.Contains(err.Error(), "dimension 7, want 8") {
		t.Fatalf("BenchmarkRecall malformed dimension error = %v", err)
	}
}

func TestUpdatesVisibilityAsOfTombstonesAndFreshness(t *testing.T) {
	ctx := context.Background()
	cfg := testConfig()
	cfg.NList, cfg.CentroidsPerSpace = 2, 2
	store := NewMemoryStore()
	manager := New(store, cfg)
	base := []VectorRecord{
		{ID: "a", Vector: []float32{1, 0, 0, 0}, EmbeddingSpace: testEmbeddingSpace, Timestamp: 10, Document: DocumentRef{Row: "evt:a"}},
		{ID: "b", Vector: []float32{0, 1, 0, 0}, EmbeddingSpace: testEmbeddingSpace, Timestamp: 10, Document: DocumentRef{Row: "evt:b"}},
	}
	if _, err := manager.Build(ctx, "idx", base, 10); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Update(ctx, "idx", []VectorRecord{
		{ID: "a", Vector: []float32{0, 1, 0, 0}, EmbeddingSpace: testEmbeddingSpace, Timestamp: 20, Visibility: "secret", Document: DocumentRef{Row: "evt:a"}},
	}, 20); err != nil {
		t.Fatal(err)
	}
	hits, _, err := manager.Search(ctx, "idx", Query{
		Vector: []float32{1, 0, 0, 0}, EmbeddingSpace: testEmbeddingSpace,
		TopK: 2, NProbe: 2,
	})
	if err != nil || len(hits) == 0 || hits[0].ID != "a" {
		t.Fatalf("public view should retain old visible version: hits=%v err=%v", hits, err)
	}
	hits, _, err = manager.Search(ctx, "idx", Query{
		Vector: []float32{0, 1, 0, 0}, EmbeddingSpace: testEmbeddingSpace,
		TopK: 2, NProbe: 2,
		Authorizations: map[string]bool{"secret": true},
	})
	if err != nil || len(hits) == 0 || hits[0].ID != "a" {
		t.Fatalf("authorized view should use newest version: hits=%v err=%v", hits, err)
	}
	if _, err := manager.Update(ctx, "idx", []VectorRecord{{ID: "a", Timestamp: 30, Tombstone: true}}, 30); err != nil {
		t.Fatal(err)
	}
	hits, _, err = manager.Search(ctx, "idx", Query{
		Vector: []float32{1, 0, 0, 0}, EmbeddingSpace: testEmbeddingSpace,
		TopK: 2, NProbe: 2,
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, hit := range hits {
		if hit.ID == "a" {
			t.Fatal("tombstoned id returned")
		}
	}
	asOf := int64(25)
	hits, _, err = manager.Search(ctx, "idx", Query{
		Vector: []float32{1, 0, 0, 0}, EmbeddingSpace: testEmbeddingSpace,
		TopK: 2, NProbe: 2, AsOf: &asOf,
	})
	if err != nil || len(hits) == 0 || hits[0].ID != "a" {
		t.Fatalf("AS OF should recover pre-tombstone version: hits=%v err=%v", hits, err)
	}
	_, evidence, err := manager.Search(ctx, "idx", Query{
		Vector: []float32{1, 0, 0, 0}, EmbeddingSpace: testEmbeddingSpace,
		TopK: 1, NProbe: 1,
		Freshness: Freshness{SourceWatermark: 50, MaxLag: 5}, ExactFallback: true,
	})
	if !errors.Is(err, ErrExactFallback) || !evidence.ExactFallback || evidence.FallbackReason == "" {
		t.Fatalf("expected explicit exact fallback: evidence=%+v err=%v", evidence, err)
	}
}

func TestReplayParityLocalMixedAndAccumuloOrder(t *testing.T) {
	ctx := context.Background()
	records := corpus(80)
	source := NewMemoryStore()
	sourceManager := New(source, testConfig())
	if _, err := sourceManager.Build(ctx, "idx", records, 77); err != nil {
		t.Fatal(err)
	}
	persisted := source.Export("idx", 0)
	variants := map[string][]Record{
		"rfile":           append([]Record(nil), persisted...),
		"parquet":         append([]Record(nil), persisted...),
		"mixed":           append([]Record(nil), persisted...),
		"accumulo-replay": append([]Record(nil), persisted...),
	}
	sort.Slice(variants["parquet"], func(i, j int) bool { return variants["parquet"][i].Row > variants["parquet"][j].Row })
	mixed := variants["mixed"]
	variants["mixed"] = append(append([]Record(nil), mixed[len(mixed)/2:]...), mixed[:len(mixed)/2]...)
	sort.SliceStable(variants["accumulo-replay"], func(i, j int) bool {
		if variants["accumulo-replay"][i].Timestamp != variants["accumulo-replay"][j].Timestamp {
			return variants["accumulo-replay"][i].Timestamp > variants["accumulo-replay"][j].Timestamp
		}
		return variants["accumulo-replay"][i].Row < variants["accumulo-replay"][j].Row
	})
	var want []Hit
	for name, stream := range variants {
		store := NewMemoryStore()
		if err := store.Import(ctx, stream); err != nil {
			t.Fatalf("%s import: %v", name, err)
		}
		hits, _, err := New(store, testConfig()).Search(ctx, "idx", Query{
			Vector: records[17].Vector, EmbeddingSpace: testEmbeddingSpace,
			TopK: 12, NProbe: 8,
		})
		if err != nil {
			t.Fatalf("%s search: %v", name, err)
		}
		if want == nil {
			want = hits
		} else if !reflect.DeepEqual(want, hits) {
			t.Fatalf("%s differs:\nwant=%+v\ngot=%+v", name, want, hits)
		}
	}
}

func TestCancellationFaultAtomicityAndGenerationRace(t *testing.T) {
	ctx := context.Background()
	store := NewMemoryStore()
	manager := New(store, testConfig())
	records := corpus(256)
	if _, err := manager.Build(ctx, "idx", records, 1); err != nil {
		t.Fatal(err)
	}

	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if _, _, err := manager.Search(cancelled, "idx", Query{
		Vector: records[0].Vector, EmbeddingSpace: testEmbeddingSpace,
		TopK: 10, NProbe: 8,
	}); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancel error = %v", err)
	}

	store.Fail = func(stage string) error {
		if stage == "before-publish" {
			return errors.New("injected")
		}
		return nil
	}
	if _, err := manager.Update(ctx, "idx", []VectorRecord{{
		ID: "new", Vector: records[0].Vector,
		EmbeddingSpace: testEmbeddingSpace, Timestamp: 999,
	}}, 2); err == nil {
		t.Fatal("faulted update succeeded")
	}
	store.Fail = nil
	active, err := store.Active(ctx, "idx")
	if err != nil || active.Manifest.Generation != 1 {
		t.Fatalf("fault changed active generation: %+v err=%v", active.Manifest, err)
	}

	var conflicts atomic.Int32
	var successes atomic.Int32
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			_, err := manager.Update(ctx, "idx", []VectorRecord{{
				ID: fmt.Sprintf("race-%d", i), Vector: records[i].Vector,
				EmbeddingSpace: testEmbeddingSpace, Timestamp: int64(2000 + i),
			}}, 3)
			switch {
			case err == nil:
				successes.Add(1)
			case errors.Is(err, ErrGenerationConflict):
				conflicts.Add(1)
			default:
				t.Errorf("update %d: %v", i, err)
			}
		}(i)
	}
	close(start)
	wg.Wait()
	if successes.Load() != 1 || conflicts.Load() != 1 {
		t.Fatalf("successes=%d conflicts=%d", successes.Load(), conflicts.Load())
	}
}

func TestEmbeddingSpaceContractRejectsSameDimensionDifferentModel(t *testing.T) {
	ctx := context.Background()
	store := NewMemoryStore()
	manager := New(store, testConfig())
	records := corpus(32)
	records[1].EmbeddingSpace = "test-provider:different-model-v1:normalized"
	if _, err := manager.Build(ctx, "idx", records, 1); !errors.Is(err, embeddingspace.ErrMismatch) {
		t.Fatalf("Build error = %v, want ErrMismatch", err)
	}

	records[1].EmbeddingSpace = testEmbeddingSpace
	manifest, err := manager.Build(ctx, "idx", records, 1)
	if err != nil {
		t.Fatal(err)
	}
	if manifest.EmbeddingSpace != testEmbeddingSpace {
		t.Fatalf("manifest embedding space = %q", manifest.EmbeddingSpace)
	}
	_, evidence, err := manager.Search(ctx, "idx", Query{
		Vector:         records[0].Vector,
		EmbeddingSpace: "test-provider:different-model-v1:normalized",
		TopK:           1,
	})
	if !errors.Is(err, embeddingspace.ErrMismatch) {
		t.Fatalf("Search error = %v, want ErrMismatch", err)
	}
	if evidence.EmbeddingSpace != testEmbeddingSpace {
		t.Fatalf("evidence embedding space = %q", evidence.EmbeddingSpace)
	}
	if _, err := manager.Update(ctx, "idx", []VectorRecord{{
		ID: "new", Vector: records[0].Vector,
		EmbeddingSpace: "test-provider:different-model-v1:normalized",
		Timestamp:      99,
	}}, 2); !errors.Is(err, embeddingspace.ErrMismatch) {
		t.Fatalf("Update error = %v, want ErrMismatch", err)
	}
}

func TestEmbeddingSpaceContractFailsClosedOnUnknown(t *testing.T) {
	ctx := context.Background()
	records := corpus(16)
	records[0].EmbeddingSpace = ""
	if _, err := New(NewMemoryStore(), testConfig()).Build(
		ctx, "idx", records, 1); !errors.Is(err, ErrEmbeddingSpace) {
		t.Fatalf("Build error = %v, want ErrEmbeddingSpace", err)
	}

	store := NewMemoryStore()
	manager := New(store, testConfig())
	records[0].EmbeddingSpace = testEmbeddingSpace
	if _, err := manager.Build(ctx, "idx", records, 1); err != nil {
		t.Fatal(err)
	}

	if _, _, err := manager.Search(ctx, "idx", Query{
		Vector: records[0].Vector, TopK: 1,
	}); !errors.Is(err, ErrEmbeddingSpace) {
		t.Fatalf("Search error = %v, want ErrEmbeddingSpace", err)
	}

}

func TestManifestRecordCarriesEmbeddingSpaceWithoutIndexBody(t *testing.T) {
	ctx := context.Background()
	store := NewMemoryStore()
	manifest, err := New(store, testConfig()).Build(
		ctx, "idx", corpus(16), 1)
	if err != nil {
		t.Fatal(err)
	}
	var manifestRecord Record
	for _, record := range store.Export("idx", 0) {
		if record.CF == recordCFManifest {
			manifestRecord = record
			break
		}
	}
	if manifestRecord.CF == "" {
		t.Fatal("manifest record not found")
	}
	decoded, err := DecodeSnapshot([]Record{manifestRecord})
	if err != nil {
		t.Fatal(err)
	}
	if decoded.Manifest.EmbeddingSpace != testEmbeddingSpace ||
		decoded.Manifest.EmbeddingSpace != manifest.EmbeddingSpace {
		t.Fatalf("decoded manifest space = %q", decoded.Manifest.EmbeddingSpace)
	}

	otherRecords := corpus(16)
	for i := range otherRecords {
		otherRecords[i].EmbeddingSpace =
			"test-provider:different-model-v1:normalized"
	}
	other, err := New(NewMemoryStore(), testConfig()).Build(
		ctx, "idx", otherRecords, 1)
	if err != nil {
		t.Fatal(err)
	}
	if other.CodebookVersion == manifest.CodebookVersion {
		t.Fatal("different embedding spaces shared a codebook version")
	}
}

func TestLegacyManifestWithoutEmbeddingSpaceFailsClosed(t *testing.T) {
	store := NewMemoryStore()
	if err := store.Commit(context.Background(), Snapshot{Manifest: Manifest{
		FormatVersion: 1,
		Index:         "legacy",
		Generation:    1,
		Dimension:     2,
	}}); err != nil {
		t.Fatal(err)
	}
	_, _, err := New(store, testConfig()).Search(
		context.Background(),
		"legacy",
		Query{
			Vector: []float32{1, 0}, EmbeddingSpace: testEmbeddingSpace,
		},
	)
	if !errors.Is(err, ErrEmbeddingSpace) {
		t.Fatalf("Search error = %v, want ErrEmbeddingSpace", err)
	}
	if _, err := New(store, testConfig()).Describe(
		context.Background(), "legacy"); !errors.Is(err, ErrEmbeddingSpace) {
		t.Fatalf("Describe error = %v, want ErrEmbeddingSpace", err)
	}
}
