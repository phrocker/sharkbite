package model

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestOllamaHTTPShapeAndUsage(t *testing.T) {
	var mu sync.Mutex
	seen := make(map[string]map[string]interface{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "wrong method", http.StatusMethodNotAllowed)
			return
		}
		var body map[string]interface{}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		mu.Lock()
		seen[r.URL.Path] = body
		mu.Unlock()
		switch r.URL.Path {
		case "/api/generate":
			_, _ = io.WriteString(w, `{"response":"answer","prompt_eval_count":3,"eval_count":2}`)
		case "/api/embeddings":
			_, _ = io.WriteString(w, `{"embedding":[1.25,-2.5],"prompt_eval_count":4}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	generator, err := NewOllamaGenerator(OllamaConfig{
		BaseURL: server.URL, Model: "generate-model", HTTPClient: server.Client(),
	})
	if err != nil {
		t.Fatal(err)
	}
	generated, err := generator.Generate(context.Background(), GenerateRequest{Prompt: "hello", MaxOutputTokens: 7})
	if err != nil {
		t.Fatal(err)
	}
	if generated.Text != "answer" || generated.Usage.TotalTokens != 5 ||
		generated.Provenance != (Provenance{Provider: "ollama", Model: "generate-model"}) {
		t.Fatalf("generate result = %#v", generated)
	}

	embedder, err := NewOllamaEmbedder(OllamaConfig{
		BaseURL: server.URL, Model: "embed-model", HTTPClient: server.Client(),
	})
	if err != nil {
		t.Fatal(err)
	}
	embedded, err := embedder.Embed(context.Background(), EmbedRequest{Text: "world"})
	if err != nil {
		t.Fatal(err)
	}
	if len(embedded.Vector) != 2 || embedded.Vector[0] != 1.25 || embedded.Usage.TotalTokens != 4 {
		t.Fatalf("embed result = %#v", embedded)
	}

	mu.Lock()
	defer mu.Unlock()
	generateBody := seen["/api/generate"]
	if len(generateBody) != 4 || generateBody["model"] != "generate-model" || generateBody["prompt"] != "hello" || generateBody["stream"] != false {
		t.Fatalf("generate body = %#v", generateBody)
	}
	options, ok := generateBody["options"].(map[string]interface{})
	if !ok || len(options) != 1 || options["num_predict"] != float64(7) {
		t.Fatalf("generate options = %#v", generateBody["options"])
	}
	embedBody := seen["/api/embeddings"]
	if len(embedBody) != 2 || embedBody["model"] != "embed-model" || embedBody["prompt"] != "world" {
		t.Fatalf("embed body = %#v", embedBody)
	}
}

func TestOllamaCancellationAndTimeouts(t *testing.T) {
	started := make(chan struct{}, 1)
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		started <- struct{}{}
		select {
		case <-r.Context().Done():
		case <-release:
		}
	}))
	defer func() {
		close(release)
		server.Close()
	}()

	embedder, err := NewOllamaEmbedder(OllamaConfig{
		BaseURL: server.URL, Model: "model", HTTPClient: server.Client(), Timeout: time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, callErr := embedder.Embed(ctx, EmbedRequest{Text: "secret"})
		done <- callErr
	}()
	<-started
	cancel()
	if err := <-done; !errors.Is(err, ErrCanceled) {
		t.Fatalf("cancel error = %v", err)
	} else if !errors.Is(err, context.Canceled) {
		t.Fatalf("cancel error does not preserve context.Canceled: %v", err)
	}

	timeoutServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(100 * time.Millisecond)
	}))
	defer timeoutServer.Close()
	generator, err := NewOllamaGenerator(OllamaConfig{
		BaseURL: timeoutServer.URL, Model: "model", HTTPClient: timeoutServer.Client(), Timeout: 10 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := generator.Generate(context.Background(), GenerateRequest{}); !errors.Is(err, ErrTimeout) {
		t.Fatalf("deadline error = %v", err)
	}
	deadlineCtx, deadlineCancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer deadlineCancel()
	if _, err := generator.Generate(deadlineCtx, GenerateRequest{}); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("caller deadline error = %v", err)
	}

	client := timeoutServer.Client()
	client.Timeout = 10 * time.Millisecond
	generator, err = NewOllamaGenerator(OllamaConfig{
		BaseURL: timeoutServer.URL, Model: "model", HTTPClient: client, Timeout: time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := generator.Generate(context.Background(), GenerateRequest{}); !errors.Is(err, ErrTimeout) {
		t.Fatalf("client timeout error = %v", err)
	}
}

func TestOllamaBodyReadTimeouts(t *testing.T) {
	for _, status := range []int{http.StatusOK, http.StatusInternalServerError} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			release := make(chan struct{})
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(status)
				w.(http.Flusher).Flush()
				<-release
			}))
			defer func() {
				close(release)
				server.Close()
			}()
			generator, err := NewOllamaGenerator(OllamaConfig{
				BaseURL: server.URL, Model: "model", HTTPClient: server.Client(), Timeout: 10 * time.Millisecond,
			})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := generator.Generate(context.Background(), GenerateRequest{}); !errors.Is(err, ErrTimeout) {
				t.Fatalf("body timeout error = %v", err)
			}
		})
	}
}

func TestOllamaResponseValidationAndRedaction(t *testing.T) {
	tests := []struct {
		name   string
		status int
		body   string
		config func(*OllamaConfig)
		want   error
	}{
		{name: "non-2xx", status: http.StatusBadGateway, body: strings.Repeat("secret-prompt", 200), want: ErrUnavailable},
		{name: "malformed", status: http.StatusOK, body: `{`, want: ErrMalformedResponse},
		{name: "oversized body", status: http.StatusOK, body: `{"embedding":[1,2,3]}`, config: func(c *OllamaConfig) { c.MaxResponseBytes = 8 }, want: ErrOversizedResponse},
		{name: "empty vector", status: http.StatusOK, body: `{"embedding":[]}`, want: ErrMalformedResponse},
		{name: "NaN vector", status: http.StatusOK, body: `{"embedding":[NaN]}`, want: ErrMalformedResponse},
		{name: "positive infinity vector", status: http.StatusOK, body: `{"embedding":[Infinity]}`, want: ErrMalformedResponse},
		{name: "negative infinity vector", status: http.StatusOK, body: `{"embedding":[-Infinity]}`, want: ErrMalformedResponse},
		{name: "overflow vector", status: http.StatusOK, body: `{"embedding":[1e400]}`, want: ErrMalformedResponse},
		{name: "oversized vector", status: http.StatusOK, body: `{"embedding":[1,2,3]}`, config: func(c *OllamaConfig) { c.MaxVectorDimensions = 2 }, want: ErrOversizedResponse},
		{name: "dimension mismatch", status: http.StatusOK, body: `{"embedding":[1,2,3]}`, config: func(c *OllamaConfig) { c.Dimensions = 2 }, want: ErrMalformedResponse},
		{name: "invalid usage", status: http.StatusOK, body: `{"embedding":[1],"prompt_eval_count":-1}`, want: ErrMalformedResponse},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(test.status)
				_, _ = io.WriteString(w, test.body)
			}))
			defer server.Close()
			cfg := OllamaConfig{
				BaseURL: server.URL, Model: "model", HTTPClient: server.Client(), ErrorSnippetBytes: 16,
			}
			if test.config != nil {
				test.config(&cfg)
			}
			embedder, err := NewOllamaEmbedder(cfg)
			if err != nil {
				t.Fatal(err)
			}
			_, err = embedder.Embed(context.Background(), EmbedRequest{Text: "secret-prompt"})
			if !errors.Is(err, test.want) {
				t.Fatalf("error = %v, want %v", err, test.want)
			}
			if strings.Contains(err.Error(), "secret-prompt") || len(err.Error()) > 200 {
				t.Fatalf("error leaked or was unbounded: %q", err)
			}
		})
	}
}

func TestOllamaEmbeddingSpaceIdentity(t *testing.T) {
	embedder, err := NewOllamaEmbedder(OllamaConfig{
		BaseURL:    "http://localhost:11434",
		Model:      "nomic-embed-text",
		Dimensions: 768,
		HTTPClient: &http.Client{Transport: identityTransport{identity: "transport-v1"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	identity, err := embedder.CacheIdentity()
	if err != nil {
		t.Fatal(err)
	}
	space, err := embedder.EmbeddingSpaceIdentity()
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"ollama", "nomic-embed-text", "768", normalizationProviderNativeUnchanged} {
		if !strings.Contains(identity, want) {
			t.Fatalf("identity %q missing %q", identity, want)
		}
		if !strings.Contains(space, want) {
			t.Fatalf("space identity %q missing %q", space, want)
		}
	}
}

func TestOllamaRejectsRedirects(t *testing.T) {
	var redirected atomic.Bool
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		redirected.Store(true)
		_, _ = io.WriteString(w, `{"embedding":[1]}`)
	}))
	defer target.Close()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL, http.StatusTemporaryRedirect)
	}))
	defer server.Close()
	embedder, err := NewOllamaEmbedder(OllamaConfig{
		BaseURL: server.URL, Model: "model", HTTPClient: server.Client(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := embedder.Embed(context.Background(), EmbedRequest{Text: "do not forward"}); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("redirect error = %v", err)
	}
	if redirected.Load() {
		t.Fatal("Ollama client followed a redirect")
	}
}

func TestOllamaConfigAndRequestValidation(t *testing.T) {
	for _, cfg := range []OllamaConfig{
		{BaseURL: "http://example.com", Model: "model"},
		{BaseURL: "https://user:pass@example.com", Model: "model"},
		{BaseURL: "https://example.com/path", Model: "model"},
		{BaseURL: "https://example.com", Model: ""},
	} {
		if _, err := NewOllamaEmbedder(cfg); !errors.Is(err, ErrInvalidConfig) {
			t.Fatalf("config %#v error = %v", cfg, err)
		}
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer server.Close()
	embedder, err := NewOllamaEmbedder(OllamaConfig{
		BaseURL: server.URL, Model: "model", HTTPClient: server.Client(), MaxTextBytes: 3,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := embedder.Embed(context.Background(), EmbedRequest{Text: "four"}); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("request error = %v", err)
	}
	generator, err := NewOllamaGenerator(OllamaConfig{
		BaseURL: server.URL, Model: "model", HTTPClient: server.Client(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := generator.Generate(context.Background(), GenerateRequest{MaxOutputTokens: maxConfiguredOutputTokens + 1}); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("token limit error = %v", err)
	}
}

func TestOllamaGenerationResponseValidation(t *testing.T) {
	tests := []struct {
		name string
		body string
		want error
	}{
		{name: "missing response", body: `{}`, want: ErrMalformedResponse},
		{name: "output tokens exceed request", body: `{"response":"too much","eval_count":3}`, want: ErrOversizedResponse},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, _ = io.WriteString(w, test.body)
			}))
			defer server.Close()
			generator, err := NewOllamaGenerator(OllamaConfig{
				BaseURL: server.URL, Model: "model", HTTPClient: server.Client(),
			})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := generator.Generate(context.Background(), GenerateRequest{
				Prompt: "prompt", MaxOutputTokens: 2,
			}); !errors.Is(err, test.want) {
				t.Fatalf("error = %v, want %v", err, test.want)
			}
		})
	}
}

func TestOllamaConcurrentSafetyAndResultIsolation(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"embedding":[1,2,3]}`)
	}))
	defer server.Close()
	embedder, err := NewOllamaEmbedder(OllamaConfig{
		BaseURL: server.URL, Model: "model", HTTPClient: server.Client(),
	})
	if err != nil {
		t.Fatal(err)
	}
	first, err := embedder.Embed(context.Background(), EmbedRequest{Text: "one"})
	if err != nil {
		t.Fatal(err)
	}
	first.Vector[0] = 99
	second, err := embedder.Embed(context.Background(), EmbedRequest{Text: "two"})
	if err != nil {
		t.Fatal(err)
	}
	if second.Vector[0] != 1 {
		t.Fatal("response vectors share mutable storage")
	}

	const workers = 32
	var wg sync.WaitGroup
	errs := make(chan error, workers)
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			result, callErr := embedder.Embed(context.Background(), EmbedRequest{Text: "concurrent"})
			if callErr != nil {
				errs <- callErr
				return
			}
			if len(result.Vector) != 3 {
				errs <- errors.New("unexpected vector length")
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
}
