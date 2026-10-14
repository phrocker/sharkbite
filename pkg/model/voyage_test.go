package model

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

const testVoyageKey = "voyage-key-must-never-appear"

func TestVoyageRequestResponseAndRedaction(t *testing.T) {
	t.Setenv("SHOAL_TEST_VOYAGE_KEY", testVoyageKey)
	var requestBody map[string]interface{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != voyageEmbeddingPath ||
			r.Header.Get("Content-Type") != "application/json" {
			http.Error(w, "bad request metadata", http.StatusBadRequest)
			return
		}
		if r.Header.Get(authorizationHeaderName) != "Bearer "+testVoyageKey {
			http.Error(w, "bad authorization", http.StatusUnauthorized)
			return
		}
		if err := json.NewDecoder(r.Body).Decode(&requestBody); err != nil {
			http.Error(w, "bad json", http.StatusBadRequest)
			return
		}
		_, _ = io.WriteString(w, `{"data":[{"embedding":[0.25,-0.5,0.75],"index":0}],"model":"voyage-test","usage":{"total_tokens":6}}`)
	}))
	defer server.Close()

	embedder, err := NewVoyageEmbedder(VoyageConfig{
		BaseURL:          server.URL,
		Model:            "voyage-test",
		Dimensions:       3,
		InputType:        "document",
		APICredentialEnv: "SHOAL_TEST_VOYAGE_KEY",
		HTTPClient:       server.Client(),
	})
	if err != nil {
		t.Fatal(err)
	}
	var logs bytes.Buffer
	oldLog := log.Writer()
	log.SetOutput(&logs)
	result, err := embedder.Embed(context.Background(), EmbedRequest{Text: "hello"})
	log.SetOutput(oldLog)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Vector) != 3 || result.Vector[0] != 0.25 ||
		result.Usage != (Usage{InputTokens: 6, TotalTokens: 6}) ||
		result.Provenance != (Provenance{Provider: voyageProvider, Model: "voyage-test"}) {
		t.Fatalf("result = %#v", result)
	}
	input, ok := requestBody["input"].([]interface{})
	if !ok || len(input) != 1 || input[0] != "hello" ||
		requestBody["model"] != "voyage-test" ||
		requestBody["input_type"] != "document" ||
		requestBody["output_dimension"] != float64(3) {
		t.Fatalf("request body = %#v", requestBody)
	}
	if strings.Contains(logs.String(), testVoyageKey) {
		t.Fatalf("logs leaked credential: %q", logs.String())
	}

	cacheable, err := NewVoyageEmbedder(VoyageConfig{
		BaseURL:          "https://example.com",
		Model:            "voyage-test",
		Dimensions:       3,
		APICredentialEnv: "SHOAL_TEST_VOYAGE_KEY",
		HTTPClient:       &http.Client{Transport: identityTransport{identity: "transport-v1"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	identity, err := cacheable.CacheIdentity()
	if err != nil {
		t.Fatal(err)
	}
	space, err := cacheable.EmbeddingSpaceIdentity()
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(identity, testVoyageKey) || !strings.Contains(identity, voyageProvider) ||
		!strings.Contains(identity, "voyage-test") || !strings.Contains(identity, "3") ||
		!strings.Contains(identity, normalizationProviderNativeUnchanged) {
		t.Fatalf("cache identity = %q", identity)
	}
	for _, want := range []string{voyageProvider, "voyage-test", "3", normalizationProviderNativeUnchanged} {
		if !strings.Contains(space, want) {
			t.Fatalf("space identity %q missing %q", space, want)
		}
	}
}

func TestVoyageConfigCredentialAndResponseValidation(t *testing.T) {
	for _, cfg := range []VoyageConfig{
		{Model: "voyage-test", Dimensions: 3, APICredentialEnv: ""},
		{Model: "voyage-test", Dimensions: 0, APICredentialEnv: "SHOAL_TEST_VOYAGE_KEY"},
		{Model: "", Dimensions: 3, APICredentialEnv: "SHOAL_TEST_VOYAGE_KEY"},
		{BaseURL: "http://example.com", Model: "voyage-test", Dimensions: 3, APICredentialEnv: "SHOAL_TEST_VOYAGE_KEY"},
		{BaseURL: "https://example.com/path", Model: "voyage-test", Dimensions: 3, APICredentialEnv: "SHOAL_TEST_VOYAGE_KEY"},
		{Model: "voyage-test", Dimensions: 3, APICredentialEnv: "BAD=NAME"},
	} {
		if _, err := NewVoyageEmbedder(cfg); !errors.Is(err, ErrInvalidConfig) {
			t.Fatalf("config %#v error = %v, want ErrInvalidConfig", cfg, err)
		}
	}

	tests := []struct {
		name string
		body string
		want error
	}{
		{name: "model mismatch", body: `{"data":[{"embedding":[1,2,3],"index":0}],"model":"other","usage":{"total_tokens":1}}`, want: ErrMalformedResponse},
		{name: "dimension mismatch", body: `{"data":[{"embedding":[1,2],"index":0}],"model":"voyage-test","usage":{"total_tokens":1}}`, want: ErrMalformedResponse},
		{name: "missing index", body: `{"data":[{"embedding":[1,2,3]}],"model":"voyage-test","usage":{"total_tokens":1}}`, want: ErrMalformedResponse},
		{name: "invalid value", body: `{"data":[{"embedding":[1e400,2,3],"index":0}],"model":"voyage-test","usage":{"total_tokens":1}}`, want: ErrMalformedResponse},
		{name: "invalid usage", body: `{"data":[{"embedding":[1,2,3],"index":0}],"model":"voyage-test","usage":{"total_tokens":-1}}`, want: ErrMalformedResponse},
		{name: "oversized body", body: `{"data":[{"embedding":[1,2,3],"index":0}],"model":"voyage-test","usage":{"total_tokens":1}}`, want: ErrOversizedResponse},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Setenv("SHOAL_TEST_VOYAGE_KEY", testVoyageKey)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, _ = io.WriteString(w, test.body)
			}))
			defer server.Close()
			cfg := VoyageConfig{
				BaseURL:          server.URL,
				Model:            "voyage-test",
				Dimensions:       3,
				APICredentialEnv: "SHOAL_TEST_VOYAGE_KEY",
				HTTPClient:       server.Client(),
			}
			if test.name == "oversized body" {
				cfg.MaxResponseBytes = 8
			}
			embedder, err := NewVoyageEmbedder(cfg)
			if err != nil {
				t.Fatal(err)
			}
			_, err = embedder.Embed(context.Background(), EmbedRequest{Text: "hello"})
			if !errors.Is(err, test.want) {
				t.Fatalf("error = %v, want %v", err, test.want)
			}
			if strings.Contains(err.Error(), testVoyageKey) {
				t.Fatalf("error leaked credential: %q", err)
			}
		})
	}
}

func TestVoyageStatusMappingAndCredentialFailuresRedacted(t *testing.T) {
	tests := []struct {
		status    int
		want      error
		retryable bool
	}{
		{status: http.StatusUnauthorized, want: ErrAuthentication},
		{status: http.StatusForbidden, want: ErrAuthentication},
		{status: http.StatusTooManyRequests, want: ErrRateLimited, retryable: true},
		{status: http.StatusInternalServerError, want: ErrUnavailable, retryable: true},
	}
	for _, test := range tests {
		t.Run(http.StatusText(test.status), func(t *testing.T) {
			t.Setenv("SHOAL_TEST_VOYAGE_KEY", testVoyageKey)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(test.status)
				_, _ = io.WriteString(w, strings.Repeat(testVoyageKey, 100))
			}))
			defer server.Close()
			embedder, err := NewVoyageEmbedder(VoyageConfig{
				BaseURL:           server.URL,
				Model:             "voyage-test",
				Dimensions:        3,
				APICredentialEnv:  "SHOAL_TEST_VOYAGE_KEY",
				HTTPClient:        server.Client(),
				ErrorSnippetBytes: 16,
			})
			if err != nil {
				t.Fatal(err)
			}
			_, err = embedder.Embed(context.Background(), EmbedRequest{Text: "hello"})
			if !errors.Is(err, test.want) {
				t.Fatalf("error = %v, want %v", err, test.want)
			}
			var modelErr *Error
			if !errors.As(err, &modelErr) || modelErr.StatusCode != test.status || modelErr.Retryable != test.retryable {
				t.Fatalf("typed error = %#v", modelErr)
			}
			if strings.Contains(err.Error(), testVoyageKey) || len(err.Error()) > 200 {
				t.Fatalf("error leaked or was unbounded: %q", err)
			}
		})
	}

	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Fatal("request must not be sent without a credential")
	}))
	defer server.Close()
	embedder, err := NewVoyageEmbedder(VoyageConfig{
		BaseURL:          server.URL,
		Model:            "voyage-test",
		Dimensions:       3,
		APICredentialEnv: "SHOAL_TEST_MISSING_VOYAGE_KEY",
		HTTPClient:       server.Client(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := embedder.Embed(context.Background(), EmbedRequest{Text: "hello"}); !errors.Is(err, ErrCredential) ||
		strings.Contains(err.Error(), testVoyageKey) {
		t.Fatalf("credential error = %v", err)
	}
}

func TestVoyageCancellation(t *testing.T) {
	t.Setenv("SHOAL_TEST_VOYAGE_KEY", testVoyageKey)
	embedder, err := NewVoyageEmbedder(VoyageConfig{
		BaseURL:          "https://example.com",
		Model:            "voyage-test",
		Dimensions:       3,
		APICredentialEnv: "SHOAL_TEST_VOYAGE_KEY",
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := embedder.Embed(ctx, EmbedRequest{Text: "hello"}); !errors.Is(err, ErrCanceled) {
		t.Fatalf("cancel error = %v", err)
	}
}
