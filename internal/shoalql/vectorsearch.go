package shoalql

import (
	"context"

	"github.com/phrocker/shoal-oss/internal/vectorindex"
)

// ManagedVectorSearcher adapts the shared IVF-PQ manager to ShoalQL.
type ManagedVectorSearcher struct {
	Manager *vectorindex.Manager
}

func (s ManagedVectorSearcher) SearchVector(ctx context.Context, request VectorSearchRequest) ([]VectorHit, vectorindex.Evidence, error) {
	hits, evidence, err := s.Manager.Search(ctx, request.Index, vectorindex.Query{
		Vector: request.Query, EmbeddingSpace: request.EmbeddingSpace,
		TopK: request.TopK, NProbe: request.NProbe,
		Authorizations: request.Authorizations, AsOf: request.AsOf,
		Freshness: request.Freshness, ExactFallback: request.ExactFallback,
		AllowedDocuments: request.AllowedDocuments,
	})
	if err != nil {
		return nil, evidence, err
	}
	out := make([]VectorHit, len(hits))
	for i, hit := range hits {
		row := hit.Document.Row
		if row == "" {
			row = hit.ID
		}
		out[i] = VectorHit{
			Row: []byte(row), ID: hit.ID, Score: float64(hit.Score),
			Document: hit.Document,
		}
	}
	return out, evidence, nil
}

func (s ManagedVectorSearcher) DescribeVector(ctx context.Context, index string) (vectorindex.Manifest, error) {
	return s.Manager.Describe(ctx, index)
}
