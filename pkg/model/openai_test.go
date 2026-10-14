package model

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

const testAPIKey = "test-key-must-never-appear"

func staticCredential(value string) CredentialResolver {
	return CredentialResolverFunc(func(context.Context) ([]byte, error) {
		return []byte(value), nil
	})
}

func openAITestConfig(server *httptest.Server) OpenAIConfig {
	return OpenAIConfig{
		BaseURL:         server.URL,
		GenerationModel: "generation-model",
		EmbeddingModel:  "embedding-model",
		Organization:    "organization-id",
		Project:         "project-id",
		Credentials:     staticCredential(testAPIKey),
		HTTPClient:      server.Client(),
	}
}

func TestOpenAIHTTPShapeHeadersUsageAndProvenance(t *testing.T) {
	var mu sync.Mutex
	bodies := make(map[string]map[string]interface{})
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.Header.Get("Content-Type") != "application/json" {
			http.Error(w, "bad request metadata", http.StatusBadRequest)
			return
		}
		authorization := r.Header.Get("Authorization")
		if authorization != "Bearer "+testAPIKey {
			http.Error(w, "bad authorization", http.StatusUnauthorized)
			return
		}
		if r.Header.Get("OpenAI-Organization") != "organization-id" ||
			r.Header.Get("OpenAI-Project") != "project-id" {
			http.Error(w, "bad typed headers", http.StatusBadRequest)
			return
		}
		var body map[string]interface{}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, "bad json", http.StatusBadRequest)
			return
		}
		mu.Lock()
		bodies[r.URL.Path] = body
		mu.Unlock()
		switch r.URL.Path {
		case openAIGeneratePath:
			_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"answer"}}],"usage":{"prompt_tokens":3,"completion_tokens":2,"total_tokens":5}}`)
		case openAIEmbeddingPath:
			_, _ = io.WriteString(w, `{"data":[{"embedding":[1.25,-2.5],"index":0}],"usage":{"prompt_tokens":4,"total_tokens":4}}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	cfg := openAITestConfig(server)
	generator, err := NewOpenAIGenerator(cfg)
	if err != nil {
		t.Fatal(err)
	}
	generated, err := generator.Generate(context.Background(), GenerateRequest{Prompt: "hello", MaxOutputTokens: 7})
	if err != nil {
		t.Fatal(err)
	}
	if generated.Text != "answer" || generated.Usage != (Usage{InputTokens: 3, OutputTokens: 2, TotalTokens: 5}) ||
		generated.Provenance != (Provenance{Provider: openAIProvider, Model: "generation-model"}) {
		t.Fatalf("generate result = %#v", generated)
	}

	embedder, err := NewOpenAIEmbedder(cfg)
	if err != nil {
		t.Fatal(err)
	}
	embedded, err := embedder.Embed(context.Background(), EmbedRequest{Text: "world"})
	if err != nil {
		t.Fatal(err)
	}
	if len(embedded.Vector) != 2 || embedded.Vector[0] != 1.25 || embedded.Vector[1] != -2.5 ||
		embedded.Usage != (Usage{InputTokens: 4, TotalTokens: 4}) ||
		embedded.Provenance != (Provenance{Provider: openAIProvider, Model: "embedding-model"}) {
		t.Fatalf("embed result = %#v", embedded)
	}

	mu.Lock()
	defer mu.Unlock()
	generateBody := bodies[openAIGeneratePath]
	if len(generateBody) != 4 || generateBody["model"] != "generation-model" ||
		generateBody["stream"] != false || generateBody["max_tokens"] != float64(7) {
		t.Fatalf("generate body has unexpected shape: keys=%d model=%v stream=%v max_tokens=%v",
			len(generateBody), generateBody["model"], generateBody["stream"], generateBody["max_tokens"])
	}
	messages, ok := generateBody["messages"].([]interface{})
	if !ok || len(messages) != 1 {
		t.Fatalf("messages shape = %T, count unknown", generateBody["messages"])
	}
	message := messages[0].(map[string]interface{})
	if len(message) != 2 || message["role"] != "user" || message["content"] != "hello" {
		t.Fatalf("message shape = %#v", message)
	}
	embedBody := bodies[openAIEmbeddingPath]
	if len(embedBody) != 2 || embedBody["model"] != "embedding-model" || embedBody["input"] != "world" {
		t.Fatalf("embedding body = %#v", embedBody)
	}
}

func TestOpenAIKeyRotationAndResolverPerRequest(t *testing.T) {
	var calls atomic.Int32
	var seenMu sync.Mutex
	var seen []string
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenMu.Lock()
		seen = append(seen, r.Header.Get("Authorization"))
		seenMu.Unlock()
		_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"ok"}}],"usage":{}}`)
	}))
	defer server.Close()
	cfg := openAITestConfig(server)
	cfg.Credentials = CredentialResolverFunc(func(context.Context) ([]byte, error) {
		call := calls.Add(1)
		return []byte("rotated-key-" + string(rune('0'+call))), nil
	})
	generator, err := NewOpenAIGenerator(cfg)
	if err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if _, err := generator.Generate(context.Background(), GenerateRequest{}); err != nil {
			t.Fatal(err)
		}
	}
	seenMu.Lock()
	defer seenMu.Unlock()
	if calls.Load() != 2 || len(seen) != 2 || seen[0] == seen[1] {
		t.Fatalf("resolver calls=%d headers differ=%v", calls.Load(), len(seen) == 2 && seen[0] != seen[1])
	}
}

func TestOpenAICredentialFailuresAreRedacted(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Fatal("request must not be sent")
	}))
	defer server.Close()
	tests := []CredentialResolver{
		CredentialResolverFunc(nil),
		CredentialResolverFunc(func(context.Context) ([]byte, error) {
			return nil, errors.New("resolver failed with " + testAPIKey)
		}),
		staticCredential(""),
		staticCredential(" whitespace "),
		staticCredential("line\r\nbreak"),
		staticCredential("control\x01byte"),
		staticCredential("delete\x7fbyte"),
		staticCredential(strings.Repeat("x", maxCredentialBytes+1)),
	}
	for i, resolver := range tests {
		cfg := openAITestConfig(server)
		cfg.Credentials = resolver
		generator, err := NewOpenAIGenerator(cfg)
		if err != nil {
			t.Fatal(err)
		}
		_, err = generator.Generate(context.Background(), GenerateRequest{})
		if !errors.Is(err, ErrCredential) || strings.Contains(err.Error(), testAPIKey) {
			t.Fatalf("case %d credential error = %q", i, err)
		}
	}
}

func TestOpenAIStatusClassificationRetryMetadataAndRedaction(t *testing.T) {
	tests := []struct {
		status     int
		retryAfter string
		want       error
		retryable  bool
		duration   time.Duration
	}{
		{status: http.StatusUnauthorized, want: ErrAuthentication},
		{status: http.StatusForbidden, want: ErrAuthentication},
		{status: http.StatusTooManyRequests, retryAfter: "17", want: ErrRateLimited, retryable: true, duration: 17 * time.Second},
		{status: http.StatusInternalServerError, want: ErrUnavailable, retryable: true},
		{status: http.StatusBadGateway, want: ErrUnavailable, retryable: true},
	}
	for _, test := range tests {
		t.Run(http.StatusText(test.status), func(t *testing.T) {
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Retry-After", test.retryAfter)
				w.WriteHeader(test.status)
				_, _ = io.WriteString(w, strings.Repeat(testAPIKey, 100))
			}))
			defer server.Close()
			cfg := openAITestConfig(server)
			cfg.ErrorSnippetBytes = 16
			generator, err := NewOpenAIGenerator(cfg)
			if err != nil {
				t.Fatal(err)
			}
			_, err = generator.Generate(context.Background(), GenerateRequest{})
			if !errors.Is(err, test.want) {
				t.Fatalf("error = %v, want %v", err, test.want)
			}
			var modelErr *Error
			if !errors.As(err, &modelErr) || modelErr.StatusCode != test.status ||
				modelErr.Retryable != test.retryable || modelErr.RetryAfter != test.duration {
				t.Fatalf("typed error = %#v", modelErr)
			}
			if strings.Contains(err.Error(), testAPIKey) || len(err.Error()) > 200 {
				t.Fatalf("error leaked or was unbounded: %q", err)
			}
		})
	}
}

func TestOpenAICancellationTimeoutAndBodyTimeout(t *testing.T) {
	started := make(chan struct{}, 1)
	release := make(chan struct{})
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		started <- struct{}{}
		<-release
	}))
	defer func() {
		close(release)
		server.Close()
	}()
	cfg := openAITestConfig(server)
	cfg.Timeout = time.Second
	generator, err := NewOpenAIGenerator(cfg)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, callErr := generator.Generate(ctx, GenerateRequest{})
		done <- callErr
	}()
	<-started
	cancel()
	if err := <-done; !errors.Is(err, ErrCanceled) || !errors.Is(err, context.Canceled) {
		t.Fatalf("cancel error = %v", err)
	}

	timeoutRelease := make(chan struct{})
	timeoutServer := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		<-timeoutRelease
	}))
	defer func() {
		close(timeoutRelease)
		timeoutServer.Close()
	}()
	cfg = openAITestConfig(timeoutServer)
	cfg.Timeout = 10 * time.Millisecond
	generator, err = NewOpenAIGenerator(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := generator.Generate(context.Background(), GenerateRequest{}); !errors.Is(err, ErrTimeout) {
		t.Fatalf("timeout error = %v", err)
	}
}

func TestOpenAIResponseBoundsAndMalformedJSON(t *testing.T) {
	tests := []struct {
		name     string
		body     string
		maxBytes int64
		want     error
	}{
		{name: "malformed", body: `{`, want: ErrMalformedResponse},
		{name: "invalid utf8", body: `{"choices":[{"message":{"content":"` + string([]byte{0xff}) + `"}}]}`, want: ErrMalformedResponse},
		{name: "trailing json", body: `{"choices":[]} {}`, want: ErrMalformedResponse},
		{name: "oversized", body: `{"choices":[{"message":{"content":"answer"}}]}`, maxBytes: 8, want: ErrOversizedResponse},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, _ = io.WriteString(w, test.body)
			}))
			defer server.Close()
			cfg := openAITestConfig(server)
			cfg.MaxResponseBytes = test.maxBytes
			generator, err := NewOpenAIGenerator(cfg)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := generator.Generate(context.Background(), GenerateRequest{}); !errors.Is(err, test.want) {
				t.Fatalf("error = %v, want %v", err, test.want)
			}
		})
	}
}

func TestOpenAIGenerationValidation(t *testing.T) {
	tests := []struct {
		name string
		body string
		want error
	}{
		{name: "empty choices", body: `{"choices":[]}`, want: ErrMalformedResponse},
		{name: "multiple choices", body: `{"choices":[{"message":{"content":"a"}},{"message":{"content":"b"}}]}`, want: ErrMalformedResponse},
		{name: "missing content", body: `{"choices":[{"message":{}}]}`, want: ErrMalformedResponse},
		{name: "empty content", body: `{"choices":[{"message":{"content":""}}]}`, want: ErrMalformedResponse},
		{name: "oversized text", body: `{"choices":[{"message":{"content":"four"}}]}`, want: ErrOversizedResponse},
		{name: "invalid usage", body: `{"choices":[{"message":{"content":"ok"}}],"usage":{"prompt_tokens":-1}}`, want: ErrMalformedResponse},
		{name: "inconsistent usage", body: `{"choices":[{"message":{"content":"ok"}}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":3}}`, want: ErrMalformedResponse},
		{name: "explicit zero total", body: `{"choices":[{"message":{"content":"ok"}}],"usage":{"prompt_tokens":1,"total_tokens":0}}`, want: ErrMalformedResponse},
		{name: "output limit", body: `{"choices":[{"message":{"content":"ok"}}],"usage":{"completion_tokens":2,"total_tokens":2}}`, want: ErrOversizedResponse},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, _ = io.WriteString(w, test.body)
			}))
			defer server.Close()
			cfg := openAITestConfig(server)
			if test.name == "oversized text" {
				cfg.MaxTextBytes = 3
			}
			generator, err := NewOpenAIGenerator(cfg)
			if err != nil {
				t.Fatal(err)
			}
			request := GenerateRequest{}
			if test.name == "output limit" {
				request.MaxOutputTokens = 1
			}
			if _, err := generator.Generate(context.Background(), request); !errors.Is(err, test.want) {
				t.Fatalf("error = %v, want %v", err, test.want)
			}
		})
	}
}

func TestOpenAIEmbeddingValidation(t *testing.T) {
	tests := []struct {
		name               string
		body               string
		maxDimensions      int
		expectedDimensions int
		want               error
	}{
		{name: "empty data", body: `{"data":[]}`, want: ErrMalformedResponse},
		{name: "multiple embeddings", body: `{"data":[{"embedding":[1],"index":0},{"embedding":[2],"index":1}]}`, want: ErrMalformedResponse},
		{name: "wrong index", body: `{"data":[{"embedding":[1],"index":1}]}`, want: ErrMalformedResponse},
		{name: "missing index", body: `{"data":[{"embedding":[1]}]}`, want: ErrMalformedResponse},
		{name: "empty vector", body: `{"data":[{"embedding":[],"index":0}]}`, want: ErrMalformedResponse},
		{name: "null vector value", body: `{"data":[{"embedding":[null],"index":0}]}`, want: ErrMalformedResponse},
		{name: "oversized vector", body: `{"data":[{"embedding":[1,2],"index":0}]}`, maxDimensions: 1, want: ErrOversizedResponse},
		{name: "dimension mismatch", body: `{"data":[{"embedding":[1,2],"index":0}]}`, expectedDimensions: 3, want: ErrMalformedResponse},
		{name: "overflow", body: `{"data":[{"embedding":[1e400],"index":0}]}`, want: ErrMalformedResponse},
		{name: "invalid usage", body: `{"data":[{"embedding":[1],"index":0}],"usage":{"prompt_tokens":-1}}`, want: ErrMalformedResponse},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, _ = io.WriteString(w, test.body)
			}))
			defer server.Close()
			cfg := openAITestConfig(server)
			cfg.MaxVectorDimensions = test.maxDimensions
			cfg.EmbeddingDimensions = test.expectedDimensions
			embedder, err := NewOpenAIEmbedder(cfg)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := embedder.Embed(context.Background(), EmbedRequest{}); !errors.Is(err, test.want) {
				t.Fatalf("error = %v, want %v", err, test.want)
			}
		})
	}

	for _, body := range []string{
		`{"data":[{"embedding":[NaN],"index":0}]}`,
		`{"data":[{"embedding":[Infinity],"index":0}]}`,
		`{"data":[{"embedding":[-Infinity],"index":0}]}`,
	} {
		server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, _ = io.WriteString(w, body)
		}))
		cfg := openAITestConfig(server)
		embedder, err := NewOpenAIEmbedder(cfg)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := embedder.Embed(context.Background(), EmbedRequest{}); !errors.Is(err, ErrMalformedResponse) {
			t.Fatalf("non-finite error = %v", err)
		}
		server.Close()
	}
}

func TestOpenAIConfigAndRequestValidation(t *testing.T) {
	validServer := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	defer validServer.Close()
	base := openAITestConfig(validServer)
	tests := []OpenAIConfig{
		func() OpenAIConfig { c := base; c.BaseURL = "http://example.com"; return c }(),
		func() OpenAIConfig { c := base; c.BaseURL = "https://:443"; return c }(),
		func() OpenAIConfig { c := base; c.BaseURL += "/prefix"; return c }(),
		func() OpenAIConfig { c := base; c.BaseURL += "?query=1"; return c }(),
		func() OpenAIConfig { c := base; c.BaseURL += "?"; return c }(),
		func() OpenAIConfig { c := base; c.BaseURL += "#"; return c }(),
		func() OpenAIConfig { c := base; c.GenerationModel = ""; return c }(),
		func() OpenAIConfig { c := base; c.GenerationModel = string([]byte{0xff}); return c }(),
		func() OpenAIConfig { c := base; c.Organization = "bad\r\nheader"; return c }(),
		func() OpenAIConfig { c := base; c.Organization = "bad\x01header"; return c }(),
		func() OpenAIConfig { c := base; c.Project = "bad\x7fheader"; return c }(),
		func() OpenAIConfig { c := base; c.Credentials = nil; return c }(),
		func() OpenAIConfig { c := base; c.EmbeddingDimensions = MaxVectorDimensions + 1; return c }(),
	}
	for i, cfg := range tests {
		if _, err := NewOpenAIGenerator(cfg); !errors.Is(err, ErrInvalidConfig) {
			t.Fatalf("config %d error = %v", i, err)
		}
	}
	cfg := base
	cfg.MaxTextBytes = 3
	generator, err := NewOpenAIGenerator(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := generator.Generate(context.Background(), GenerateRequest{Prompt: "four"}); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("request error = %v", err)
	}
	if _, err := generator.Generate(context.Background(), GenerateRequest{Prompt: string([]byte{0xff})}); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("invalid UTF-8 request error = %v", err)
	}
	embedder, err := NewOpenAIEmbedder(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := embedder.Embed(context.Background(), EmbedRequest{Text: string([]byte{0xff})}); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("invalid UTF-8 embedding request error = %v", err)
	}
	hostedNoCredential := base
	hostedNoCredential.BaseURL = "https://example.com"
	hostedNoCredential.Credentials = nil
	if _, err := NewOpenAIEmbedder(hostedNoCredential); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("hosted no-credential embedder error = %v", err)
	}
}

func TestOpenAILocalCompatibleServerWithoutCredentials(t *testing.T) {
	var sawAuthorization bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sawAuthorization = r.Header.Get(authorizationHeaderName) != ""
		if r.URL.Path != openAIEmbeddingPath {
			http.NotFound(w, r)
			return
		}
		_, _ = io.WriteString(w, `{"data":[{"embedding":[1,2,3],"index":0}],"usage":{"prompt_tokens":2,"total_tokens":2}}`)
	}))
	defer server.Close()
	embedder, err := NewOpenAIEmbedder(OpenAIConfig{
		BaseURL:             server.URL,
		EmbeddingModel:      "llama-cpp-embedding",
		EmbeddingDimensions: 3,
		HTTPClient:          server.Client(),
	})
	if err != nil {
		t.Fatal(err)
	}
	result, err := embedder.Embed(context.Background(), EmbedRequest{Text: "local text"})
	if err != nil {
		t.Fatal(err)
	}
	if sawAuthorization {
		t.Fatal("local no-credential request unexpectedly sent Authorization")
	}
	if len(result.Vector) != 3 || result.Provenance != (Provenance{Provider: openAIProvider, Model: "llama-cpp-embedding"}) {
		t.Fatalf("result = %#v", result)
	}
}

func TestOpenAINoCredentialCacheIdentity(t *testing.T) {
	embedder, err := NewOpenAIEmbedder(OpenAIConfig{
		BaseURL:             "https://localhost",
		EmbeddingModel:      "local-embedding",
		EmbeddingDimensions: 3,
		HTTPClient:          &http.Client{Transport: identityTransport{identity: "transport-v1"}},
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
	for _, want := range []string{openAIProvider, "local-embedding", "3", normalizationProviderNativeUnchanged} {
		if !strings.Contains(identity, want) {
			t.Fatalf("identity %q missing %q", identity, want)
		}
		if !strings.Contains(space, want) {
			t.Fatalf("space identity %q missing %q", space, want)
		}
	}
	if strings.Contains(identity, testAPIKey) {
		t.Fatalf("identity leaked credential: %q", identity)
	}

	withoutDimensions, err := NewOpenAIEmbedder(OpenAIConfig{
		BaseURL:        "https://localhost",
		EmbeddingModel: "local-embedding",
		HTTPClient:     &http.Client{Transport: identityTransport{identity: "transport-v1"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := withoutDimensions.CacheIdentity(); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("cache identity without dimensions error = %v", err)
	}
}

func TestOpenAIRedirectRejectedWithoutCredentialForwarding(t *testing.T) {
	var targetCalls atomic.Int32
	target := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		targetCalls.Add(1)
		_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"bad"}}]}`)
	}))
	defer target.Close()
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL, http.StatusTemporaryRedirect)
	}))
	defer server.Close()
	cfg := openAITestConfig(server)
	generator, err := NewOpenAIGenerator(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := generator.Generate(context.Background(), GenerateRequest{}); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("redirect error = %v", err)
	}
	if targetCalls.Load() != 0 {
		t.Fatal("credential-bearing redirect was followed")
	}
}

func TestOpenAIResultIsolationAndConcurrency(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"data":[{"embedding":[1,2,3],"index":0}],"usage":{}}`)
	}))
	defer server.Close()
	cfg := openAITestConfig(server)
	embedder, err := NewOpenAIEmbedder(cfg)
	if err != nil {
		t.Fatal(err)
	}
	first, err := embedder.Embed(context.Background(), EmbedRequest{})
	if err != nil {
		t.Fatal(err)
	}
	first.Vector[0] = 99
	second, err := embedder.Embed(context.Background(), EmbedRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if second.Vector[0] != 1 {
		t.Fatal("response vectors share mutable storage")
	}

	const workers = 32
	var wg sync.WaitGroup
	errs := make(chan error, workers)
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			result, callErr := embedder.Embed(context.Background(), EmbedRequest{})
			if callErr != nil {
				errs <- callErr
			} else if len(result.Vector) != 3 || math.IsNaN(float64(result.Vector[0])) {
				errs <- errors.New("unexpected vector")
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
}

type roundTripperFunc func(*http.Request) (*http.Response, error)

func (f roundTripperFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return f(request)
}

func TestOpenAIRequestNotMutatedAfterTransportReturn(t *testing.T) {
	var captured *http.Request
	client := &http.Client{Transport: roundTripperFunc(func(request *http.Request) (*http.Response, error) {
		captured = request
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     make(http.Header),
			Body:       io.NopCloser(strings.NewReader(`{"choices":[{"message":{"content":"ok"}}],"usage":{}}`)),
			Request:    request,
		}, nil
	})}
	generator, err := NewOpenAIGenerator(OpenAIConfig{
		BaseURL:         "https://example.com",
		GenerationModel: "model",
		Credentials:     staticCredential(testAPIKey),
		HTTPClient:      client,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := generator.Generate(context.Background(), GenerateRequest{}); err != nil {
		t.Fatal(err)
	}
	if captured == nil || captured.Header.Get(authorizationHeaderName) == "" {
		t.Fatal("provider mutated a request after the transport returned")
	}
}

func TestOpenAIInterfaces(t *testing.T) {
	var _ TextGenerator = (*OpenAIGenerator)(nil)
	var _ Embedder = (*OpenAIEmbedder)(nil)
}
