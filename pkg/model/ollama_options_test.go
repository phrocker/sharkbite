package model

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func TestOllamaGeneratorSendsGenerationOptions(t *testing.T) {
	var body map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		_, _ = io.WriteString(w, `{"response":"{}","prompt_eval_count":3,"eval_count":2}`)
	}))
	defer server.Close()
	numGPU, seed, temperature := 22, 7, 0.0
	generator, err := NewOllamaGenerator(OllamaConfig{
		BaseURL: server.URL, Model: "llama3.1", HTTPClient: server.Client(),
		NumCtx: 32768, NumGPU: &numGPU, Seed: &seed, Temperature: &temperature, Format: "json",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := generator.Generate(context.Background(), GenerateRequest{Prompt: "hello", MaxOutputTokens: 4096}); err != nil {
		t.Fatal(err)
	}
	if body["format"] != "json" || body["stream"] != false {
		t.Fatalf("generate body = %#v", body)
	}
	options, _ := body["options"].(map[string]any)
	want := map[string]any{
		"num_ctx": float64(32768), "num_gpu": float64(22), "seed": float64(7),
		"temperature": float64(0), "num_predict": float64(4096),
	}
	if len(options) != len(want) {
		t.Fatalf("options = %#v", options)
	}
	for key, value := range want {
		if options[key] != value {
			t.Fatalf("options[%s] = %#v, want %#v (all: %#v)", key, options[key], value, options)
		}
	}

	client := &http.Client{Transport: identityTransport{identity: "transport-v1"}}
	plain, err := NewOllamaGenerator(OllamaConfig{BaseURL: server.URL, Model: "llama3.1", HTTPClient: client})
	if err != nil {
		t.Fatal(err)
	}
	configured, err := NewOllamaGenerator(OllamaConfig{BaseURL: server.URL, Model: "llama3.1", HTTPClient: client, NumCtx: 32768, Format: "json"})
	if err != nil {
		t.Fatal(err)
	}
	left, leftErr := plain.CacheIdentity()
	right, rightErr := configured.CacheIdentity()
	if leftErr != nil || rightErr != nil || left == right {
		t.Fatalf("generation options do not change the generator cache identity: %v %v", leftErr, rightErr)
	}
}

func TestOllamaGeneratorRefusesPromptOverContext(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		_, _ = io.WriteString(w, `{"response":"{}","prompt_eval_count":3,"eval_count":2}`)
	}))
	defer server.Close()
	generator, err := NewOllamaGenerator(OllamaConfig{BaseURL: server.URL, Model: "llama3.1", HTTPClient: server.Client(), NumCtx: 2048})
	if err != nil {
		t.Fatal(err)
	}
	for name, request := range map[string]GenerateRequest{
		// 4,096 bytes estimate to 2,048 tokens: the prompt alone fills the window.
		"prompt alone":      {Prompt: strings.Repeat("x", 4096)},
		"prompt and output": {Prompt: strings.Repeat("x", 3000), MaxOutputTokens: 600},
	} {
		_, err := generator.Generate(context.Background(), request)
		if !errors.Is(err, ErrInvalidRequest) || !strings.Contains(err.Error(), "exceeds num_ctx 2048") {
			t.Errorf("%s: over-long prompt was not refused: %v", name, err)
		}
	}
	if calls.Load() != 0 {
		t.Fatal("an over-long prompt was sent to Ollama")
	}
	if _, err := generator.Generate(context.Background(), GenerateRequest{Prompt: strings.Repeat("x", 3000), MaxOutputTokens: 500}); err != nil {
		t.Fatalf("prompt within num_ctx refused: %v", err)
	}
}

func TestOllamaGeneratorRejectsReportedContextOverflow(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"response":"{}","prompt_eval_count":2000,"eval_count":100}`)
	}))
	defer server.Close()
	generator, err := NewOllamaGenerator(OllamaConfig{BaseURL: server.URL, Model: "llama3.1", HTTPClient: server.Client(), NumCtx: 2048})
	if err != nil {
		t.Fatal(err)
	}
	_, err = generator.Generate(context.Background(), GenerateRequest{Prompt: "short", MaxOutputTokens: 100})
	if !errors.Is(err, ErrInvalidRequest) || !strings.Contains(err.Error(), "exceed num_ctx 2048") {
		t.Fatalf("reported context overflow accepted: %v", err)
	}
}

func TestOllamaGenerationOptionsAreValidated(t *testing.T) {
	negative, huge, nan := -2, maxConfiguredNumGPU+1, 0.0
	nan = nan / nan
	for name, cfg := range map[string]OllamaConfig{
		"num_ctx":     {NumCtx: -1},
		"num_gpu":     {NumGPU: &negative},
		"num_gpu_max": {NumGPU: &huge},
		"temperature": {Temperature: &nan},
		"format":      {Format: "yaml"},
	} {
		cfg.BaseURL, cfg.Model = "http://127.0.0.1:11434", "llama3.1"
		if _, err := NewOllamaGenerator(cfg); !errors.Is(err, ErrInvalidConfig) {
			t.Errorf("%s: invalid option accepted: %v", name, err)
		}
	}
}
