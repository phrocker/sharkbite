package agentmem

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/phrocker/shoal-oss/internal/embeddingspace"
)

func TestOllamaEmbedder(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/embeddings" {
			t.Fatalf("path = %q, want /api/embeddings", r.URL.Path)
		}
		if r.Method != http.MethodPost {
			t.Fatalf("method = %q, want POST", r.Method)
		}
		var req map[string]string
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		if req["model"] != "embed-test" || req["prompt"] != "hello world" {
			t.Fatalf("request = %#v", req)
		}
		_ = json.NewEncoder(w).Encode(map[string][]float64{"embedding": {1.25, -2.5, 3}})
	}))
	defer server.Close()

	embedder := NewOllamaEmbedder(
		WithOllamaHost(server.URL),
		WithOllamaModel("embed-test"),
		WithOllamaTimeout(time.Second),
		WithOllamaHTTPClient(server.Client()),
	)
	got, err := embedder.Embed(context.Background(), "hello world")
	if err != nil {
		t.Fatalf("Embed returned error: %v", err)
	}
	want := []float32{1.25, -2.5, 3}
	if len(got) != len(want) {
		t.Fatalf("len = %d, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got[%d] = %v, want %v", i, got[i], want[i])
		}
	}
	if _, err := embedder.EmbeddingSpaceIdentity(); !errors.Is(
		err, embeddingspace.ErrQueryIdentityRequired) {
		t.Fatalf("unknown-dimension identity error = %v", err)
	}
	identified := NewOllamaEmbedder(
		WithOllamaHost(server.URL),
		WithOllamaModel("embed-test"),
		WithOllamaDimensions(3),
		WithOllamaHTTPClient(server.Client()),
	)
	if identity, err := identified.EmbeddingSpaceIdentity(); err != nil ||
		identity == "" {
		t.Fatalf("configured identity = %q, %v", identity, err)
	}
}

func TestOllamaLLM(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/generate" {
			t.Fatalf("path = %q, want /api/generate", r.URL.Path)
		}
		var req struct {
			Model  string `json:"model"`
			Prompt string `json:"prompt"`
			Stream bool   `json:"stream"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		if req.Model != "llm-test" || req.Prompt != "infer this" || req.Stream {
			t.Fatalf("request = %#v", req)
		}
		_ = json.NewEncoder(w).Encode(map[string]string{"response": "causal,entity"})
	}))
	defer server.Close()

	llm := NewOllamaLLM(
		WithOllamaHost(server.URL),
		WithOllamaModel("llm-test"),
		WithOllamaTimeout(time.Second),
		WithOllamaHTTPClient(server.Client()),
	)
	got, err := llm.Infer(context.Background(), "infer this")
	if err != nil {
		t.Fatalf("Infer returned error: %v", err)
	}
	if got != "causal,entity" {
		t.Fatalf("response = %q, want causal,entity", got)
	}
}

func TestOllamaNonOKError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer server.Close()

	_, err := NewOllamaEmbedder(
		WithOllamaHost(server.URL),
		WithOllamaHTTPClient(server.Client()),
	).Embed(context.Background(), "hello")
	if err == nil {
		t.Fatal("expected error")
	}
	msg := err.Error()
	if !strings.Contains(msg, "status 500") || strings.Contains(msg, "boom") {
		t.Fatalf("error = %q, want status and redacted body metadata", msg)
	}
}
