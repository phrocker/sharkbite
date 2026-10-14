package vectorindex

import (
	"context"
	"errors"
	"testing"

	"github.com/phrocker/shoal-oss/internal/embeddingspace"
)

func TestBenchmarkRejectsQueryFromDifferentSpace(t *testing.T) {
	ctx := context.Background()
	records := corpus(32)
	manager := New(NewMemoryStore(), testConfig())
	if _, err := manager.Build(ctx, "idx", records, 1); err != nil {
		t.Fatal(err)
	}
	_, err := BenchmarkRecall(ctx, manager, "idx", records, []BenchmarkQuery{{
		Name: "foreign", Vector: records[0].Vector,
		EmbeddingSpace: "test-provider:different-model-v1:normalized",
	}}, 1, 1, 0, "foreign-query")
	if !errors.Is(err, embeddingspace.ErrMismatch) {
		t.Fatalf("BenchmarkRecall error = %v, want ErrMismatch", err)
	}
}

func TestEmbeddingSpaceContractRejectsNonCanonicalWhitespace(t *testing.T) {
	records := corpus(16)
	for i := range records {
		records[i].EmbeddingSpace = " " + testEmbeddingSpace + " "
	}
	if _, err := New(NewMemoryStore(), testConfig()).Build(
		context.Background(), "idx", records, 1,
	); !errors.Is(err, ErrEmbeddingSpace) {
		t.Fatalf("Build error = %v, want ErrEmbeddingSpace", err)
	}
}
