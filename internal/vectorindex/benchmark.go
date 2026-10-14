package vectorindex

import (
	"context"
	"fmt"
	"sort"

	"github.com/phrocker/shoal-oss/internal/embeddingspace"
)

type BenchmarkQuery struct {
	Name           string
	Vector         []float32
	EmbeddingSpace string
}

type BenchmarkResult struct {
	Recall RecallContract
	Passed bool
}

// BenchmarkRecall compares approximate results with exhaustive cosine search.
// It is the only API that populates a measured recall claim.
func BenchmarkRecall(ctx context.Context, manager *Manager, index string, corpus []VectorRecord, queries []BenchmarkQuery, topK, nprobe int, minimum float64, benchmarkRef string) (BenchmarkResult, error) {
	if len(corpus) == 0 || len(queries) == 0 || benchmarkRef == "" {
		return BenchmarkResult{}, fmt.Errorf("vectorindex: benchmark requires corpus, queries, and benchmark reference")
	}
	manifest, err := manager.Describe(ctx, index)
	if err != nil {
		return BenchmarkResult{}, err
	}
	if err := validateManifestEmbeddingSpace(manifest); err != nil {
		return BenchmarkResult{}, err
	}
	live := make([]VectorRecord, 0, len(corpus))
	for _, record := range corpus {
		if !record.Tombstone {
			live = append(live, record)
		}
	}
	if _, err := embeddingSpaceForRecords("benchmark vector index", live); err != nil {
		return BenchmarkResult{}, err
	}
	for _, record := range corpus {
		if record.Tombstone {
			continue
		}
		if len(record.Vector) != manifest.Dimension {
			return BenchmarkResult{}, fmt.Errorf(
				"vectorindex: benchmark record %q dimension %d, want %d",
				record.ID, len(record.Vector), manifest.Dimension)
		}
		if err := embeddingspace.EnsureSameIdentity(
			"benchmark vector index", manifest.EmbeddingSpace,
			record.EmbeddingSpace); err != nil {
			return BenchmarkResult{}, err
		}
	}
	var total float64
	for _, query := range queries {
		if err := ctx.Err(); err != nil {
			return BenchmarkResult{}, err
		}
		if err := requireEmbeddingSpace(
			"benchmark query", query.EmbeddingSpace); err != nil {
			return BenchmarkResult{}, err
		}
		if err := embeddingspace.EnsureSameIdentity(
			"benchmark query", manifest.EmbeddingSpace,
			query.EmbeddingSpace); err != nil {
			return BenchmarkResult{}, err
		}
		approx, _, err := manager.Search(ctx, index, Query{
			Vector: query.Vector, EmbeddingSpace: query.EmbeddingSpace,
			TopK: topK, NProbe: nprobe,
		})
		if err != nil {
			return BenchmarkResult{}, err
		}
		exact := exactTopK(corpus, query.Vector, topK)
		wanted := map[string]bool{}
		for _, hit := range exact {
			wanted[hit.ID] = true
		}
		matches := 0
		for _, hit := range approx {
			if wanted[hit.ID] {
				matches++
			}
		}
		denominator := len(exact)
		if denominator > 0 {
			total += float64(matches) / float64(denominator)
		}
	}
	measured := total / float64(len(queries))
	contract := RecallContract{
		Corpus: "deterministic:" + index, TopK: topK, NProbe: nprobe,
		Minimum: minimum, Measured: measured, Queries: len(queries), BenchmarkRef: benchmarkRef,
		EmbeddingSpace: manifest.EmbeddingSpace,
		Generation:     manifest.Generation, CodebookVersion: manifest.CodebookVersion,
	}
	return BenchmarkResult{Recall: contract, Passed: measured >= minimum}, nil
}

func exactTopK(corpus []VectorRecord, query []float32, topK int) []Hit {
	query = normalize(query)
	hits := make([]Hit, 0, len(corpus))
	for _, record := range corpus {
		if record.Tombstone {
			continue
		}
		hits = append(hits, Hit{ID: record.ID, Score: dot(normalize(record.Vector), query), Document: cloneDocument(record.Document)})
	}
	sort.SliceStable(hits, func(i, j int) bool {
		if hits[i].Score != hits[j].Score {
			return hits[i].Score > hits[j].Score
		}
		return hits[i].ID < hits[j].ID
	})
	if len(hits) > topK {
		hits = hits[:topK]
	}
	return hits
}

func dot(a, b []float32) float32 {
	n := len(a)
	if len(b) < n {
		n = len(b)
	}
	var out float32
	for i := 0; i < n; i++ {
		out += a[i] * b[i]
	}
	return out
}
