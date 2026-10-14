package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/phrocker/shoal-oss/internal/agentmem"
)

func TestConfigureProvidersFakeCompatibility(t *testing.T) {
	cfg := agentmem.Config{}
	if err := configureProviders(&cfg, "fake", "fake", "", "", "", nil); err != nil {
		t.Fatal(err)
	}
	if cfg.Embedder != nil || cfg.LLM != nil {
		t.Fatal("fake selection must preserve agentmem defaults")
	}
}

func TestConfigureProvidersOllamaCompatibility(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/embeddings":
			var request struct {
				Model  string `json:"model"`
				Prompt string `json:"prompt"`
			}
			_ = json.NewDecoder(r.Body).Decode(&request)
			if request.Model != "embed-model" || request.Prompt != "text" {
				t.Errorf("embed request = %#v", request)
			}
			_, _ = w.Write([]byte(`{"embedding":[1,2]}`))
		case "/api/generate":
			var request struct {
				Model  string `json:"model"`
				Prompt string `json:"prompt"`
				Stream bool   `json:"stream"`
			}
			_ = json.NewDecoder(r.Body).Decode(&request)
			if request.Model != "llm-model" || request.Prompt != "prompt" || request.Stream {
				t.Errorf("generate request = %#v", request)
			}
			_, _ = w.Write([]byte(`{"response":"answer"}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	cfg := agentmem.Config{}
	if err := configureProviders(&cfg, "ollama", "ollama", server.URL, "embed-model", "llm-model", server.Client()); err != nil {
		t.Fatal(err)
	}
	vector, err := cfg.Embedder.Embed(context.Background(), "text")
	if err != nil || len(vector) != 2 {
		t.Fatalf("Embed = %#v, %v", vector, err)
	}
	text, err := cfg.LLM.Infer(context.Background(), "prompt")
	if err != nil || text != "answer" {
		t.Fatalf("Infer = %q, %v", text, err)
	}
}

func TestConfigureProvidersBlankOllamaValuesUseDefaults(t *testing.T) {
	cfg := agentmem.Config{}
	if err := configureProviders(&cfg, "ollama", "ollama", " ", "", "\t", nil); err != nil {
		t.Fatalf("blank Ollama values should use defaults: %v", err)
	}
	if cfg.Embedder == nil || cfg.LLM == nil {
		t.Fatal("Ollama providers were not configured")
	}
}
