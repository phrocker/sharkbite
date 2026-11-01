package model

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

type schemaIdentityTransport struct{ inner http.RoundTripper }

func (t schemaIdentityTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	return t.inner.RoundTrip(req)
}
func (schemaIdentityTransport) CacheIdentity() (string, error) { return "schema-test-transport", nil }

func TestOllamaSchemaFormatAndIdentity(t *testing.T) {
	var format json.RawMessage
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Format json.RawMessage `json:"format"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode request: %v", err)
		}
		format = append(format[:0], body.Format...)
		_, _ = w.Write([]byte(`{"response":"ok","prompt_eval_count":1,"eval_count":1}`))
	}))
	defer server.Close()
	cfg := OllamaConfig{BaseURL: server.URL, Model: "test", HTTPClient: &http.Client{Transport: schemaIdentityTransport{inner: http.DefaultTransport}}}
	plain, err := NewOllamaGenerator(cfg)
	if err != nil {
		t.Fatal(err)
	}
	baseline, err := plain.CacheIdentity()
	if err != nil {
		t.Fatal(err)
	}
	schema := json.RawMessage(` { "type" : "object", "properties": { "answer": {"type":"string"} } } `)
	cfg.FormatSchema = schema
	configured, err := NewOllamaGenerator(cfg)
	if err != nil {
		t.Fatal(err)
	}
	configuredID, err := configured.CacheIdentity()
	if err != nil {
		t.Fatal(err)
	}
	if configuredID == baseline {
		t.Fatal("schema did not change cache identity")
	}
	if _, err := configured.Generate(context.Background(), GenerateRequest{Prompt: "hello"}); err != nil {
		t.Fatal(err)
	}
	if string(format) != `{"type":"object","properties":{"answer":{"type":"string"}}}` {
		t.Fatalf("format = %s", format)
	}
	if _, err := plain.Generate(context.Background(), GenerateRequest{Prompt: "hello", FormatSchema: schema}); err != nil {
		t.Fatal(err)
	}
	if string(format) != `{"type":"object","properties":{"answer":{"type":"string"}}}` {
		t.Fatalf("override format = %s", format)
	}
	if current, _ := plain.CacheIdentity(); current != baseline {
		t.Fatal("per-request override mutated generator identity")
	}
	cfg.Format = "json"
	if _, err := NewOllamaGenerator(cfg); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("combined format error = %v", err)
	}
	for _, bad := range []json.RawMessage{json.RawMessage(`[]`), json.RawMessage(`{} {}`), json.RawMessage(strings.Repeat("x", 256<<10+1))} {
		cfg.Format = ""
		cfg.FormatSchema = bad
		if _, err := NewOllamaGenerator(cfg); !errors.Is(err, ErrInvalidConfig) {
			t.Fatalf("schema %q error = %v", string(bad[:min(len(bad), 10)]), err)
		}
	}
}

func TestOllamaConfigurablePromptEstimate(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"response":"ok","prompt_eval_count":80,"eval_count":1}`))
	}))
	defer server.Close()
	cfg := OllamaConfig{BaseURL: server.URL, Model: "test", HTTPClient: server.Client(), NumCtx: 100}
	defaultGenerator, err := NewOllamaGenerator(cfg)
	if err != nil {
		t.Fatal(err)
	}
	request := GenerateRequest{Prompt: strings.Repeat("x", 250), MaxOutputTokens: 10}
	if _, err := defaultGenerator.Generate(context.Background(), request); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("default estimate error = %v", err)
	}
	cfg.PromptBytesPerToken = 3
	adjusted, err := NewOllamaGenerator(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := adjusted.Generate(context.Background(), request); err != nil {
		t.Fatalf("adjusted estimate refused fitting prompt: %v", err)
	}
}
