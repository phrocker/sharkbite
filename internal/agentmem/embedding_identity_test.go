package agentmem

import (
	"errors"
	"testing"

	"github.com/phrocker/shoal-oss/internal/embeddingspace"
)

func TestNewRejectsExplicitEmbeddingSpaceMismatch(t *testing.T) {
	_, err := New(Config{
		Store:          NewFakeStore(),
		Embedder:       FakeEmbedder{Dim: 16},
		EmbeddingSpace: "different-space",
	})
	if !errors.Is(err, embeddingspace.ErrMismatch) {
		t.Fatalf("New error = %v, want ErrMismatch", err)
	}
}
