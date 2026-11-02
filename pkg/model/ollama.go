package model

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

const (
	DefaultOllamaBaseURL       = "http://localhost:11434"
	DefaultOllamaEmbedModel    = "nomic-embed-text"
	DefaultOllamaGenerateModel = "llama3"
	DefaultTimeout             = 30 * time.Second
	DefaultMaxTextBytes        = 1 << 20
	DefaultMaxRequestBytes     = (1 << 20) + (16 << 10)
	DefaultMaxResponseBytes    = 4 << 20
	DefaultErrorSnippetBytes   = 512
	MaxVectorDimensions        = 1 << 20
	maxConfiguredTextBytes     = 16 << 20
	maxConfiguredRequestBytes  = 17 << 20
	maxConfiguredResponseBytes = 64 << 20
	maxConfiguredSnippetBytes  = 4 << 10
	maxConfiguredTimeout       = 10 * time.Minute
	maxConfiguredOutputTokens  = 1 << 20
	maxReportedUsageTokens     = 16 << 20
	maxModelBytes              = 256
	maxFormatSchemaBytes       = 256 << 10
	maxConfiguredNumCtx        = 1 << 21
	maxConfiguredNumGPU        = 1 << 12
	// OllamaPromptBytesPerToken is the conservative bytes-per-token ratio
	// OllamaGenerator uses to check a prompt against NumCtx. Harness prompts
	// are JSON with base64 IDs: llama3.1 measured about 2.3 bytes per token on
	// ontology-discovery prompts (32,381 bytes, 13,945 tokens), English prose
	// about 4. Overestimating tokens fails early; underestimating would let
	// Ollama truncate the prompt silently.
	OllamaPromptBytesPerToken = 2
)

// OllamaPromptTokenEstimate is the token count OllamaGenerator assumes for a
// prompt of the given byte length when checking it against NumCtx.
func OllamaPromptTokenEstimate(promptBytes int) int {
	if promptBytes <= 0 {
		return 0
	}
	return (promptBytes + OllamaPromptBytesPerToken - 1) / OllamaPromptBytesPerToken
}

type OllamaConfig struct {
	BaseURL             string
	Model               string
	Dimensions          int
	HTTPClient          *http.Client
	Timeout             time.Duration
	MaxTextBytes        int64
	MaxRequestBytes     int64
	MaxResponseBytes    int64
	MaxVectorDimensions int
	ErrorSnippetBytes   int64

	// Generation options, sent by OllamaGenerator only (the embedder ignores
	// them). Zero values send nothing and leave Ollama's own defaults.
	//
	// NumCtx is the context window in tokens (options.num_ctx). Ollama's
	// default is small (2,048 on 0.3.x) and it truncates a longer prompt
	// without an error, so set it for long prompts. When it is set, Generate
	// refuses a prompt whose estimated tokens (OllamaPromptTokenEstimate)
	// plus MaxOutputTokens exceed it, and rejects a response whose reported
	// prompt and output tokens exceed it, instead of returning a truncated
	// answer. When it is zero no such check is possible.
	NumCtx int
	// NumGPU is the number of layers offloaded to the GPU (options.num_gpu);
	// nil lets Ollama decide.
	NumGPU *int
	// Temperature (options.temperature) and Seed (options.seed); nil leaves
	// Ollama's defaults.
	Temperature *float64
	Seed        *int
	// Format is "" (free text) or "json" (Ollama constrains the output to
	// JSON).
	Format string
	// FormatSchema constrains generation to a single JSON schema object.
	// It cannot be combined with Format: "json".
	FormatSchema json.RawMessage
	// PromptBytesPerToken adjusts NumCtx preflight for the selected model.
	// Zero preserves OllamaPromptBytesPerToken.
	PromptBytesPerToken float64
}

type OllamaGenerator struct {
	cfg                 OllamaConfig
	endpoint            string
	httpClientIdentity  string
	cacheIdentityUnsafe bool
	egresses            bool
}

type OllamaEmbedder struct {
	cfg                 OllamaConfig
	endpoint            string
	httpClientIdentity  string
	cacheIdentityUnsafe bool
	egresses            bool
}

func NewOllamaGenerator(cfg OllamaConfig) (*OllamaGenerator, error) {
	httpIdentity, cacheable, err := httpClientCacheIdentity(cfg.HTTPClient)
	if err != nil {
		return nil, err
	}
	cfg, endpoint, err := validateOllamaConfig(cfg, "/api/generate")
	if err != nil {
		return nil, err
	}
	return &OllamaGenerator{
		cfg: cfg, endpoint: endpoint,
		httpClientIdentity: httpIdentity, cacheIdentityUnsafe: !cacheable,
		egresses: egressesForConfiguredURL(cfg.BaseURL),
	}, nil
}

func NewOllamaEmbedder(cfg OllamaConfig) (*OllamaEmbedder, error) {
	httpIdentity, cacheable, err := httpClientCacheIdentity(cfg.HTTPClient)
	if err != nil {
		return nil, err
	}
	cfg, endpoint, err := validateOllamaConfig(cfg, "/api/embeddings")
	if err != nil {
		return nil, err
	}
	return &OllamaEmbedder{
		cfg: cfg, endpoint: endpoint,
		httpClientIdentity: httpIdentity, cacheIdentityUnsafe: !cacheable,
		egresses: egressesForConfiguredURL(cfg.BaseURL),
	}, nil
}

func (o *OllamaGenerator) CacheIdentity() (string, error) {
	if o == nil {
		return "", ErrInvalidConfig
	}
	if o.cacheIdentityUnsafe {
		return "", ErrInvalidConfig
	}
	identity := ollamaCacheIdentity("ollama-generator-v1", o.cfg, o.endpoint, o.httpClientIdentity)
	if options := ollamaGenerationIdentity(o.cfg); options != "" {
		identity = framedModelIdentity(identity, options)
	}
	if len(o.cfg.FormatSchema) != 0 {
		sum := sha256.Sum256(o.cfg.FormatSchema)
		identity = framedModelIdentity(identity, fmt.Sprintf("schema-sha256:%x", sum))
	}
	return identity, nil
}

func (*OllamaGenerator) SupportsFormatSchema() bool { return true }

// ollamaGenerationIdentity is empty when no generation option is set, so
// existing cache identities are unchanged.
func ollamaGenerationIdentity(cfg OllamaConfig) string {
	if cfg.NumCtx == 0 && cfg.NumGPU == nil && cfg.Temperature == nil && cfg.Seed == nil && cfg.Format == "" && cfg.PromptBytesPerToken == 0 {
		return ""
	}
	optional := func(value string, set bool) string {
		if !set {
			return "-"
		}
		return value
	}
	numGPU, seed, temperature := "", "", ""
	if cfg.NumGPU != nil {
		numGPU = strconv.Itoa(*cfg.NumGPU)
	}
	if cfg.Seed != nil {
		seed = strconv.Itoa(*cfg.Seed)
	}
	if cfg.Temperature != nil {
		temperature = strconv.FormatFloat(*cfg.Temperature, 'g', -1, 64)
	}
	identity := framedModelIdentity("ollama-generate-options-v1", strconv.Itoa(cfg.NumCtx),
		optional(numGPU, cfg.NumGPU != nil), optional(temperature, cfg.Temperature != nil),
		optional(seed, cfg.Seed != nil), cfg.Format)
	if cfg.PromptBytesPerToken != 0 {
		identity = framedModelIdentity(identity, "prompt-bytes-per-token:"+strconv.FormatFloat(cfg.PromptBytesPerToken, 'g', -1, 64))
	}
	return identity
}

func (o *OllamaEmbedder) CacheIdentity() (string, error) {
	if o == nil {
		return "", ErrInvalidConfig
	}
	if o.cacheIdentityUnsafe {
		return "", ErrInvalidConfig
	}
	space, err := o.EmbeddingSpaceIdentity()
	if err != nil {
		return "", err
	}
	return ollamaEmbeddingCacheIdentity(o.cfg, o.endpoint, o.httpClientIdentity, space), nil
}

func (o *OllamaEmbedder) EmbeddingSpaceIdentity() (string, error) {
	if o == nil || o.cfg.Dimensions == 0 {
		return "", ErrInvalidConfig
	}
	return embeddingSpaceIdentity("ollama", o.cfg.Model, o.cfg.Dimensions, normalizationProviderNativeUnchanged)
}

func (o *OllamaGenerator) Generate(ctx context.Context, req GenerateRequest) (GenerateResult, error) {
	if err := validateTextRequest("ollama generate", req.Prompt, req.MaxOutputTokens, o.cfg.MaxTextBytes); err != nil {
		return GenerateResult{}, err
	}
	if o.cfg.NumCtx > 0 {
		bytesPerToken := o.cfg.PromptBytesPerToken
		if bytesPerToken == 0 {
			bytesPerToken = OllamaPromptBytesPerToken
		}
		estimated := int(math.Ceil(float64(len(req.Prompt)) / bytesPerToken))
		if estimated+req.MaxOutputTokens > o.cfg.NumCtx || estimated >= o.cfg.NumCtx {
			return GenerateResult{}, &Error{Kind: ErrInvalidRequest, Operation: "ollama generate", Detail: fmt.Sprintf(
				"prompt of about %d tokens (%d bytes at %g bytes/token) plus %d output tokens exceeds num_ctx %d; "+
					"Ollama would truncate it silently: raise NumCtx or shorten the prompt",
				estimated, len(req.Prompt), bytesPerToken, req.MaxOutputTokens, o.cfg.NumCtx)}
		}
	}
	format := any(nil)
	if len(req.FormatSchema) != 0 {
		if o.cfg.Format != "" {
			return GenerateResult{}, &Error{Kind: ErrInvalidRequest, Operation: "ollama generate", Detail: "format and schema cannot be combined"}
		}
		schema, err := compactFormatSchema(req.FormatSchema)
		if err != nil {
			return GenerateResult{}, &Error{Kind: ErrInvalidRequest, Operation: "ollama generate", Detail: err.Error()}
		}
		format = schema
	} else if len(o.cfg.FormatSchema) != 0 {
		format = o.cfg.FormatSchema
	} else if o.cfg.Format != "" {
		format = o.cfg.Format
	}
	type generateOptions struct {
		NumPredict  int      `json:"num_predict,omitempty"`
		NumCtx      int      `json:"num_ctx,omitempty"`
		NumGPU      *int     `json:"num_gpu,omitempty"`
		Temperature *float64 `json:"temperature,omitempty"`
		Seed        *int     `json:"seed,omitempty"`
	}
	payload := struct {
		Model   string           `json:"model"`
		Prompt  string           `json:"prompt"`
		Stream  bool             `json:"stream"`
		Format  any              `json:"format,omitempty"`
		Options *generateOptions `json:"options,omitempty"`
	}{Model: o.cfg.Model, Prompt: req.Prompt, Stream: false, Format: format}
	options := generateOptions{
		NumCtx: o.cfg.NumCtx, NumGPU: o.cfg.NumGPU, Temperature: o.cfg.Temperature, Seed: o.cfg.Seed,
	}
	if req.MaxOutputTokens > 0 {
		options.NumPredict = req.MaxOutputTokens
	}
	if options != (generateOptions{}) {
		payload.Options = &options
	}
	var out struct {
		Response        *string `json:"response"`
		PromptEvalCount int     `json:"prompt_eval_count"`
		EvalCount       int     `json:"eval_count"`
	}
	if err := ollamaPost(ctx, o.cfg, o.endpoint, "ollama generate", payload, &out); err != nil {
		return GenerateResult{}, err
	}
	if out.Response == nil {
		return GenerateResult{}, &Error{Kind: ErrMalformedResponse, Operation: "ollama generate", Detail: "missing response"}
	}
	if int64(len(*out.Response)) > o.cfg.MaxTextBytes {
		return GenerateResult{}, &Error{Kind: ErrOversizedResponse, Operation: "ollama generate"}
	}
	if !validUsage(out.PromptEvalCount, out.EvalCount) {
		return GenerateResult{}, &Error{Kind: ErrMalformedResponse, Operation: "ollama generate", Detail: "invalid usage"}
	}
	if req.MaxOutputTokens > 0 && out.EvalCount > req.MaxOutputTokens {
		return GenerateResult{}, &Error{Kind: ErrOversizedResponse, Operation: "ollama generate", Detail: "output token limit exceeded"}
	}
	if o.cfg.NumCtx > 0 && out.PromptEvalCount+out.EvalCount > o.cfg.NumCtx {
		return GenerateResult{}, &Error{Kind: ErrInvalidRequest, Operation: "ollama generate", Detail: fmt.Sprintf(
			"reported %d prompt + %d output tokens exceed num_ctx %d; the context was truncated or shifted",
			out.PromptEvalCount, out.EvalCount, o.cfg.NumCtx)}
	}
	return GenerateResult{
		Text:       *out.Response,
		Provenance: Provenance{Provider: "ollama", Model: o.cfg.Model},
		Usage: Usage{
			InputTokens:  nonNegative(out.PromptEvalCount),
			OutputTokens: nonNegative(out.EvalCount),
			TotalTokens:  nonNegative(out.PromptEvalCount) + nonNegative(out.EvalCount),
		},
	}, nil
}

func (o *OllamaEmbedder) Embed(ctx context.Context, req EmbedRequest) (EmbedResult, error) {
	if err := validateTextRequest("ollama embed", req.Text, 0, o.cfg.MaxTextBytes); err != nil {
		return EmbedResult{}, err
	}
	payload := struct {
		Model  string `json:"model"`
		Prompt string `json:"prompt"`
	}{Model: o.cfg.Model, Prompt: req.Text}
	var out struct {
		Embedding       []float64 `json:"embedding"`
		PromptEvalCount int       `json:"prompt_eval_count"`
	}
	if err := ollamaPost(ctx, o.cfg, o.endpoint, "ollama embed", payload, &out); err != nil {
		return EmbedResult{}, err
	}
	if len(out.Embedding) == 0 {
		return EmbedResult{}, &Error{Kind: ErrMalformedResponse, Operation: "ollama embed", Detail: "empty vector"}
	}
	if len(out.Embedding) > o.cfg.MaxVectorDimensions {
		return EmbedResult{}, &Error{Kind: ErrOversizedResponse, Operation: "ollama embed", Detail: "vector exceeds configured dimensions"}
	}
	if o.cfg.Dimensions != 0 && len(out.Embedding) != o.cfg.Dimensions {
		return EmbedResult{}, &Error{Kind: ErrMalformedResponse, Operation: "ollama embed", Detail: "vector dimensions mismatch"}
	}
	if !validUsage(out.PromptEvalCount) {
		return EmbedResult{}, &Error{Kind: ErrMalformedResponse, Operation: "ollama embed", Detail: "invalid usage"}
	}
	vec := make([]float32, len(out.Embedding))
	for i, value := range out.Embedding {
		if math.IsNaN(value) || math.IsInf(value, 0) || value > math.MaxFloat32 || value < -math.MaxFloat32 {
			return EmbedResult{}, &Error{Kind: ErrMalformedResponse, Operation: "ollama embed", Detail: "invalid vector value"}
		}
		vec[i] = float32(value)
	}
	usage := nonNegative(out.PromptEvalCount)
	return EmbedResult{
		Vector:     append([]float32(nil), vec...),
		Provenance: Provenance{Provider: "ollama", Model: o.cfg.Model},
		Usage:      Usage{InputTokens: usage, TotalTokens: usage},
	}, nil
}

func ollamaCacheIdentity(kind string, cfg OllamaConfig, endpoint, httpClientIdentity string) string {
	return framedModelIdentity(
		kind,
		endpoint,
		cfg.Model,
		httpClientIdentity,
		cfg.Timeout.String(),
		strconv.FormatInt(cfg.MaxTextBytes, 10),
		strconv.FormatInt(cfg.MaxRequestBytes, 10),
		strconv.FormatInt(cfg.MaxResponseBytes, 10),
		strconv.Itoa(cfg.MaxVectorDimensions),
		strconv.FormatInt(cfg.ErrorSnippetBytes, 10),
	)
}

func ollamaEmbeddingCacheIdentity(cfg OllamaConfig, endpoint, httpClientIdentity, spaceIdentity string) string {
	return framedModelIdentity(
		"ollama-embedder-v1",
		endpoint,
		spaceIdentity,
		httpClientIdentity,
		cfg.Timeout.String(),
		strconv.FormatInt(cfg.MaxTextBytes, 10),
		strconv.FormatInt(cfg.MaxRequestBytes, 10),
		strconv.FormatInt(cfg.MaxResponseBytes, 10),
		strconv.Itoa(cfg.MaxVectorDimensions),
		strconv.FormatInt(cfg.ErrorSnippetBytes, 10),
	)
}

func validateOllamaConfig(cfg OllamaConfig, path string) (OllamaConfig, string, error) {
	op := "configure ollama"
	cfg.BaseURL = strings.TrimSpace(cfg.BaseURL)
	cfg.Model = strings.TrimSpace(cfg.Model)
	if cfg.BaseURL == "" || cfg.Model == "" || len(cfg.Model) > maxModelBytes || strings.ContainsAny(cfg.Model, "\r\n\x00") {
		return OllamaConfig{}, "", &Error{Kind: ErrInvalidConfig, Operation: op}
	}
	if cfg.Format != "" && len(cfg.FormatSchema) != 0 {
		return OllamaConfig{}, "", &Error{Kind: ErrInvalidConfig, Operation: op, Detail: "format and schema cannot be combined"}
	}
	if len(cfg.FormatSchema) != 0 {
		schema, err := compactFormatSchema(cfg.FormatSchema)
		if err != nil {
			return OllamaConfig{}, "", &Error{Kind: ErrInvalidConfig, Operation: op, Detail: err.Error()}
		}
		cfg.FormatSchema = schema
	}
	u, err := url.Parse(cfg.BaseURL)
	if err != nil || !u.IsAbs() || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Path != "" && u.Path != "/") {
		return OllamaConfig{}, "", &Error{Kind: ErrInvalidConfig, Operation: op}
	}
	if u.Scheme != "https" && !(u.Scheme == "http" && isLoopbackHost(u.Hostname())) {
		return OllamaConfig{}, "", &Error{Kind: ErrInvalidConfig, Operation: op}
	}
	if cfg.HTTPClient == nil {
		cfg.HTTPClient = &http.Client{}
	}
	client := *cfg.HTTPClient
	client.CheckRedirect = func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}
	cfg.HTTPClient = &client
	if cfg.Timeout == 0 {
		cfg.Timeout = DefaultTimeout
	}
	if cfg.MaxTextBytes == 0 {
		cfg.MaxTextBytes = DefaultMaxTextBytes
	}
	if cfg.MaxRequestBytes == 0 {
		cfg.MaxRequestBytes = DefaultMaxRequestBytes
	}
	if cfg.MaxResponseBytes == 0 {
		cfg.MaxResponseBytes = DefaultMaxResponseBytes
	}
	if cfg.MaxVectorDimensions == 0 {
		cfg.MaxVectorDimensions = MaxVectorDimensions
	}
	if cfg.ErrorSnippetBytes == 0 {
		cfg.ErrorSnippetBytes = DefaultErrorSnippetBytes
	}
	if cfg.Timeout < time.Millisecond || cfg.Timeout > maxConfiguredTimeout ||
		cfg.MaxTextBytes < 1 || cfg.MaxTextBytes > maxConfiguredTextBytes ||
		cfg.MaxRequestBytes < 1 || cfg.MaxRequestBytes > maxConfiguredRequestBytes ||
		cfg.MaxResponseBytes < 1 || cfg.MaxResponseBytes > maxConfiguredResponseBytes ||
		cfg.Dimensions < 0 || cfg.Dimensions > MaxVectorDimensions ||
		cfg.MaxVectorDimensions < 1 || cfg.MaxVectorDimensions > MaxVectorDimensions ||
		(cfg.Dimensions != 0 && cfg.Dimensions > cfg.MaxVectorDimensions) ||
		cfg.ErrorSnippetBytes < 1 || cfg.ErrorSnippetBytes > maxConfiguredSnippetBytes ||
		cfg.NumCtx < 0 || cfg.NumCtx > maxConfiguredNumCtx ||
		(cfg.NumGPU != nil && (*cfg.NumGPU < -1 || *cfg.NumGPU > maxConfiguredNumGPU)) ||
		(cfg.Temperature != nil && (math.IsNaN(*cfg.Temperature) || *cfg.Temperature < 0 || *cfg.Temperature > 100)) ||
		(cfg.Format != "" && cfg.Format != "json") ||
		math.IsNaN(cfg.PromptBytesPerToken) || math.IsInf(cfg.PromptBytesPerToken, 0) ||
		cfg.PromptBytesPerToken < 0 || cfg.PromptBytesPerToken > 32 ||
		(cfg.PromptBytesPerToken > 0 && cfg.PromptBytesPerToken < 1) {
		return OllamaConfig{}, "", &Error{Kind: ErrInvalidConfig, Operation: op}
	}
	u.Path = path
	return cfg, u.String(), nil
}

func compactFormatSchema(raw json.RawMessage) (json.RawMessage, error) {
	if len(raw) > maxFormatSchemaBytes || !json.Valid(raw) || len(bytes.TrimSpace(raw)) == 0 || bytes.TrimSpace(raw)[0] != '{' {
		return nil, errors.New("format schema must be a single JSON object of at most 256 KB")
	}
	var compact bytes.Buffer
	if err := json.Compact(&compact, raw); err != nil {
		return nil, err
	}
	return json.RawMessage(compact.Bytes()), nil
}

func validateTextRequest(op, text string, maxOutputTokens int, maxTextBytes int64) error {
	if !utf8.ValidString(text) || int64(len(text)) > maxTextBytes ||
		maxOutputTokens < 0 || maxOutputTokens > maxConfiguredOutputTokens {
		return &Error{Kind: ErrInvalidRequest, Operation: op}
	}
	return nil
}

func ollamaPost(ctx context.Context, cfg OllamaConfig, endpoint, op string, payload, out interface{}) error {
	if err := ctx.Err(); err != nil {
		return contextError(op, err)
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return &Error{Kind: ErrInvalidRequest, Operation: op}
	}
	if int64(len(body)) > cfg.MaxRequestBytes {
		return &Error{Kind: ErrInvalidRequest, Operation: op}
	}
	callCtx, cancel := context.WithTimeout(ctx, cfg.Timeout)
	defer cancel()
	request, err := http.NewRequestWithContext(callCtx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return &Error{Kind: ErrInvalidConfig, Operation: op}
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := cfg.HTTPClient.Do(request)
	if err != nil {
		if ctx.Err() != nil {
			return contextError(op, ctx.Err())
		}
		if callCtx.Err() != nil || isTimeout(err) {
			return &Error{Kind: ErrTimeout, Operation: op}
		}
		return &Error{Kind: ErrUnavailable, Operation: op}
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		snippet, truncated, err := readSnippet(response.Body, cfg.ErrorSnippetBytes)
		if err != nil {
			return classifyTransportError(ctx, callCtx, op, err)
		}
		detail := fmt.Sprintf("response body redacted (%d bytes", len(snippet))
		if truncated {
			detail += "+"
		}
		detail += ")"
		return &Error{Kind: ErrUnavailable, Operation: op, StatusCode: response.StatusCode, Detail: detail}
	}
	limited := &io.LimitedReader{R: response.Body, N: cfg.MaxResponseBytes + 1}
	data, err := io.ReadAll(limited)
	if err != nil {
		return classifyTransportError(ctx, callCtx, op, err)
	}
	if int64(len(data)) > cfg.MaxResponseBytes {
		return &Error{Kind: ErrOversizedResponse, Operation: op}
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	if err := decoder.Decode(out); err != nil {
		return &Error{Kind: ErrMalformedResponse, Operation: op}
	}
	var trailing interface{}
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return &Error{Kind: ErrMalformedResponse, Operation: op}
	}
	return nil
}

func readSnippet(r io.Reader, limit int64) ([]byte, bool, error) {
	data, err := io.ReadAll(io.LimitReader(r, limit+1))
	if err != nil {
		return nil, false, err
	}
	if int64(len(data)) > limit {
		return data[:limit], true, nil
	}
	return data, false, nil
}

func contextError(op string, err error) error {
	if errors.Is(err, context.Canceled) {
		return &Error{Kind: ErrCanceled, Operation: op}
	}
	return &Error{Kind: ErrTimeout, Operation: op}
}

func isTimeout(err error) bool {
	var netErr net.Error
	return errors.As(err, &netErr) && netErr.Timeout()
}

func classifyTransportError(ctx, callCtx context.Context, op string, err error) error {
	if ctx.Err() != nil {
		return contextError(op, ctx.Err())
	}
	if callCtx.Err() != nil || isTimeout(err) {
		return &Error{Kind: ErrTimeout, Operation: op}
	}
	return &Error{Kind: ErrUnavailable, Operation: op}
}

func isLoopbackHost(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func nonNegative(value int) int {
	if value < 0 {
		return 0
	}
	return value
}

func validUsage(values ...int) bool {
	total := 0
	for _, value := range values {
		if value < 0 || value > maxReportedUsageTokens || total > maxReportedUsageTokens-value {
			return false
		}
		total += value
	}
	return true
}

var (
	_ TextGenerator                  = (*OllamaGenerator)(nil)
	_ Embedder                       = (*OllamaEmbedder)(nil)
	_ EmbeddingSpaceIdentityProvider = (*OllamaEmbedder)(nil)
)

// EgressesOffHost reports whether this Ollama endpoint is remote. A loopback
// Ollama is the common local-first deployment and transmits nothing; the same
// code against a hosted endpoint transmits everything it is given.
func (o *OllamaGenerator) EgressesOffHost() bool {
	return o == nil || o.egresses
}

func (o *OllamaEmbedder) EgressesOffHost() bool {
	return o == nil || o.egresses
}
