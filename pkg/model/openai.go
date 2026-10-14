package model

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"math"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

const (
	openAIProvider          = "openai-compatible"
	openAIGeneratePath      = "/v1/chat/completions"
	openAIEmbeddingPath     = "/v1/embeddings"
	maxOrganizationBytes    = 256
	maxProjectBytes         = 256
	maxCredentialBytes      = 16 << 10
	maxRetryAfter           = 24 * time.Hour
	authorizationHeaderName = "Authorization"
)

// CredentialResolver returns a fresh API credential for each provider request.
// Implementations must be safe for concurrent use. Callers retain ownership of
// the returned slice; the provider immediately copies it and clears its copy.
type CredentialResolver interface {
	ResolveCredential(context.Context) ([]byte, error)
}

// CredentialResolverFunc adapts a function to CredentialResolver.
type CredentialResolverFunc func(context.Context) ([]byte, error)

func (f CredentialResolverFunc) ResolveCredential(ctx context.Context) ([]byte, error) {
	if f == nil {
		return nil, ErrCredential
	}
	return f(ctx)
}

// OpenAIConfig configures an OpenAI-compatible adapter. Credentials are
// optional only for embedding requests to local loopback compatible servers;
// when Credentials is nil in that case, requests do not include an
// Authorization header. It intentionally exposes no arbitrary-header escape
// hatch.
type OpenAIConfig struct {
	BaseURL         string
	GenerationModel string
	EmbeddingModel  string
	// EmbeddingDimensions, when non-zero, requires embedding responses to have
	// exactly this dimensionality and includes that dimensionality in cache
	// identity.
	EmbeddingDimensions int
	Organization        string
	Project             string
	Credentials         CredentialResolver
	HTTPClient          *http.Client
	Timeout             time.Duration

	MaxTextBytes        int64
	MaxRequestBytes     int64
	MaxResponseBytes    int64
	MaxVectorDimensions int
	ErrorSnippetBytes   int64
}

type openAIClient struct {
	baseURL             string
	generationModel     string
	embeddingModel      string
	embeddingDimensions int
	organization        string
	project             string
	credentials         CredentialResolver
	httpClient          *http.Client
	httpClientIdentity  string
	cacheIdentityUnsafe bool
	egresses            bool
	timeout             time.Duration
	maxTextBytes        int64
	maxRequestBytes     int64
	maxResponseBytes    int64
	maxVectorDimensions int
	errorSnippetBytes   int64
}

type OpenAIGenerator struct {
	client *openAIClient
}

type OpenAIEmbedder struct {
	client *openAIClient
}

func NewOpenAIGenerator(cfg OpenAIConfig) (*OpenAIGenerator, error) {
	httpIdentity, httpCacheable, err := httpClientCacheIdentity(cfg.HTTPClient)
	if err != nil {
		return nil, err
	}
	_, credentialCacheable, err := openAICredentialCacheIdentity(cfg.Credentials)
	if err != nil {
		return nil, err
	}
	client, err := validateOpenAIConfig(cfg, true, false)
	if err != nil {
		return nil, err
	}
	client.httpClientIdentity = httpIdentity
	client.cacheIdentityUnsafe = !httpCacheable || !credentialCacheable
	return &OpenAIGenerator{client: client}, nil
}

func NewOpenAIEmbedder(cfg OpenAIConfig) (*OpenAIEmbedder, error) {
	httpIdentity, httpCacheable, err := httpClientCacheIdentity(cfg.HTTPClient)
	if err != nil {
		return nil, err
	}
	_, credentialCacheable, err := openAICredentialCacheIdentity(cfg.Credentials)
	if err != nil {
		return nil, err
	}
	client, err := validateOpenAIConfig(cfg, false, true)
	if err != nil {
		return nil, err
	}
	client.httpClientIdentity = httpIdentity
	client.cacheIdentityUnsafe = !httpCacheable || !credentialCacheable
	return &OpenAIEmbedder{client: client}, nil
}

func (o *OpenAIGenerator) CacheIdentity() (string, error) {
	if o == nil || o.client == nil {
		return "", ErrInvalidConfig
	}
	if o.client.cacheIdentityUnsafe {
		return "", ErrInvalidConfig
	}
	credentialIdentity, credentialCacheable, _ := openAICredentialCacheIdentity(o.client.credentials)
	if !credentialCacheable {
		return "", ErrInvalidConfig
	}
	return openAICacheIdentity("openai-compatible-generator-v1", o.client, o.client.generationModel, credentialIdentity), nil
}

func (o *OpenAIEmbedder) CacheIdentity() (string, error) {
	if o == nil || o.client == nil {
		return "", ErrInvalidConfig
	}
	if o.client.cacheIdentityUnsafe {
		return "", ErrInvalidConfig
	}
	space, err := o.EmbeddingSpaceIdentity()
	if err != nil {
		return "", err
	}
	credentialIdentity, credentialCacheable, _ := openAICredentialCacheIdentity(o.client.credentials)
	if !credentialCacheable {
		return "", ErrInvalidConfig
	}
	return openAIEmbeddingCacheIdentity(o.client, space, credentialIdentity), nil
}

func (o *OpenAIEmbedder) EmbeddingSpaceIdentity() (string, error) {
	if o == nil || o.client == nil || o.client.embeddingDimensions == 0 {
		return "", ErrInvalidConfig
	}
	return embeddingSpaceIdentity(
		openAIProvider,
		o.client.embeddingModel,
		o.client.embeddingDimensions,
		normalizationProviderNativeUnchanged,
	)
}

func (o *OpenAIGenerator) Generate(ctx context.Context, req GenerateRequest) (GenerateResult, error) {
	const op = "openai-compatible generate"
	if err := validateTextRequest(op, req.Prompt, req.MaxOutputTokens, o.client.maxTextBytes); err != nil {
		return GenerateResult{}, err
	}
	payload := struct {
		Model    string `json:"model"`
		Messages []struct {
			Role    string `json:"role"`
			Content string `json:"content"`
		} `json:"messages"`
		Stream    bool `json:"stream"`
		MaxTokens int  `json:"max_tokens,omitempty"`
	}{
		Model: o.client.generationModel,
		Messages: []struct {
			Role    string `json:"role"`
			Content string `json:"content"`
		}{{Role: "user", Content: req.Prompt}},
		Stream:    false,
		MaxTokens: req.MaxOutputTokens,
	}
	var out struct {
		Choices []struct {
			Message struct {
				Content *string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
		Usage openAIUsage `json:"usage"`
	}
	if err := o.client.post(ctx, openAIGeneratePath, op, payload, &out); err != nil {
		return GenerateResult{}, err
	}
	if len(out.Choices) != 1 || out.Choices[0].Message.Content == nil {
		return GenerateResult{}, &Error{Kind: ErrMalformedResponse, Operation: op, Detail: "expected exactly one text choice"}
	}
	text := *out.Choices[0].Message.Content
	if text == "" {
		return GenerateResult{}, &Error{Kind: ErrMalformedResponse, Operation: op, Detail: "empty generated text"}
	}
	if int64(len(text)) > o.client.maxTextBytes {
		return GenerateResult{}, &Error{Kind: ErrOversizedResponse, Operation: op, Detail: "generated text exceeds configured limit"}
	}
	usage, err := mapOpenAIUsage(op, out.Usage)
	if err != nil {
		return GenerateResult{}, err
	}
	if req.MaxOutputTokens > 0 && usage.OutputTokens > req.MaxOutputTokens {
		return GenerateResult{}, &Error{Kind: ErrOversizedResponse, Operation: op, Detail: "output token limit exceeded"}
	}
	return GenerateResult{
		Text:       text,
		Provenance: Provenance{Provider: openAIProvider, Model: o.client.generationModel},
		Usage:      usage,
	}, nil
}

func (o *OpenAIEmbedder) Embed(ctx context.Context, req EmbedRequest) (EmbedResult, error) {
	const op = "openai-compatible embed"
	if err := validateTextRequest(op, req.Text, 0, o.client.maxTextBytes); err != nil {
		return EmbedResult{}, err
	}
	payload := struct {
		Model string `json:"model"`
		Input string `json:"input"`
	}{
		Model: o.client.embeddingModel,
		Input: req.Text,
	}
	var out struct {
		Data []struct {
			Embedding []strictFloat64 `json:"embedding"`
			Index     *int            `json:"index"`
		} `json:"data"`
		Usage openAIUsage `json:"usage"`
	}
	if err := o.client.post(ctx, openAIEmbeddingPath, op, payload, &out); err != nil {
		return EmbedResult{}, err
	}
	if len(out.Data) != 1 || out.Data[0].Index == nil || *out.Data[0].Index != 0 ||
		len(out.Data[0].Embedding) == 0 {
		return EmbedResult{}, &Error{Kind: ErrMalformedResponse, Operation: op, Detail: "expected exactly one non-empty embedding"}
	}
	if len(out.Data[0].Embedding) > o.client.maxVectorDimensions {
		return EmbedResult{}, &Error{Kind: ErrOversizedResponse, Operation: op, Detail: "vector exceeds configured dimensions"}
	}
	if o.client.embeddingDimensions != 0 && len(out.Data[0].Embedding) != o.client.embeddingDimensions {
		return EmbedResult{}, &Error{Kind: ErrMalformedResponse, Operation: op, Detail: "vector dimensions mismatch"}
	}
	vector := make([]float32, len(out.Data[0].Embedding))
	for i, encoded := range out.Data[0].Embedding {
		value := float64(encoded)
		if math.IsNaN(value) || math.IsInf(value, 0) || value > math.MaxFloat32 || value < -math.MaxFloat32 {
			return EmbedResult{}, &Error{Kind: ErrMalformedResponse, Operation: op, Detail: "invalid vector value"}
		}
		vector[i] = float32(value)
	}
	usage, err := mapOpenAIUsage(op, out.Usage)
	if err != nil {
		return EmbedResult{}, err
	}
	return EmbedResult{
		Vector:     append([]float32(nil), vector...),
		Provenance: Provenance{Provider: openAIProvider, Model: o.client.embeddingModel},
		Usage:      usage,
	}, nil
}

func openAICacheIdentity(kind string, client *openAIClient, model, credentialIdentity string) string {
	return framedModelIdentity(
		kind,
		client.baseURL,
		model,
		client.organization,
		client.project,
		credentialIdentity,
		client.httpClientIdentity,
		client.timeout.String(),
		strconv.FormatInt(client.maxTextBytes, 10),
		strconv.FormatInt(client.maxRequestBytes, 10),
		strconv.FormatInt(client.maxResponseBytes, 10),
		strconv.Itoa(client.maxVectorDimensions),
		strconv.FormatInt(client.errorSnippetBytes, 10),
	)
}

func openAIEmbeddingCacheIdentity(client *openAIClient, spaceIdentity, credentialIdentity string) string {
	return framedModelIdentity(
		"openai-compatible-embedder-v1",
		client.baseURL,
		spaceIdentity,
		client.organization,
		client.project,
		credentialIdentity,
		client.httpClientIdentity,
		client.timeout.String(),
		strconv.FormatInt(client.maxTextBytes, 10),
		strconv.FormatInt(client.maxRequestBytes, 10),
		strconv.FormatInt(client.maxResponseBytes, 10),
		strconv.Itoa(client.maxVectorDimensions),
		strconv.FormatInt(client.errorSnippetBytes, 10),
	)
}

type openAIUsage struct {
	PromptTokens     int  `json:"prompt_tokens"`
	CompletionTokens int  `json:"completion_tokens"`
	TotalTokens      *int `json:"total_tokens"`
}

func mapOpenAIUsage(op string, value openAIUsage) (Usage, error) {
	if !validUsage(value.PromptTokens, value.CompletionTokens) {
		return Usage{}, &Error{Kind: ErrMalformedResponse, Operation: op, Detail: "invalid usage"}
	}
	calculated := value.PromptTokens + value.CompletionTokens
	if value.TotalTokens != nil {
		if *value.TotalTokens < 0 || *value.TotalTokens > maxReportedUsageTokens ||
			*value.TotalTokens != calculated {
			return Usage{}, &Error{Kind: ErrMalformedResponse, Operation: op, Detail: "inconsistent usage"}
		}
	}
	return Usage{
		InputTokens:  value.PromptTokens,
		OutputTokens: value.CompletionTokens,
		TotalTokens:  calculated,
	}, nil
}

type strictFloat64 float64

func (f *strictFloat64) UnmarshalJSON(data []byte) error {
	if bytes.Equal(data, []byte("null")) {
		return errors.New("null number")
	}
	var value float64
	if err := json.Unmarshal(data, &value); err != nil {
		return err
	}
	*f = strictFloat64(value)
	return nil
}

func validateOpenAIConfig(cfg OpenAIConfig, needGeneration, needEmbedding bool) (*openAIClient, error) {
	const op = "configure openai-compatible"
	baseURL := strings.TrimSpace(cfg.BaseURL)
	generationModel := strings.TrimSpace(cfg.GenerationModel)
	embeddingModel := strings.TrimSpace(cfg.EmbeddingModel)
	organization := strings.TrimSpace(cfg.Organization)
	project := strings.TrimSpace(cfg.Project)
	if baseURL == "" ||
		(needGeneration && !validConfigValue(generationModel, maxModelBytes)) ||
		(needEmbedding && !validConfigValue(embeddingModel, maxModelBytes)) ||
		(organization != "" && !validHTTPHeaderValue(organization, maxOrganizationBytes)) ||
		(project != "" && !validHTTPHeaderValue(project, maxProjectBytes)) {
		return nil, &Error{Kind: ErrInvalidConfig, Operation: op}
	}
	u, err := url.Parse(baseURL)
	if err != nil || !u.IsAbs() || u.Hostname() == "" || u.User != nil ||
		u.RawQuery != "" || u.ForceQuery || strings.Contains(baseURL, "#") ||
		u.Fragment != "" || (u.Path != "" && u.Path != "/") {
		return nil, &Error{Kind: ErrInvalidConfig, Operation: op}
	}
	if u.Scheme != "https" && !(u.Scheme == "http" && isLoopbackHost(u.Hostname())) {
		return nil, &Error{Kind: ErrInvalidConfig, Operation: op}
	}
	if cfg.Credentials == nil && !(needEmbedding && !needGeneration && isLoopbackHost(u.Hostname())) {
		return nil, &Error{Kind: ErrInvalidConfig, Operation: op}
	}
	if cfg.HTTPClient == nil {
		cfg.HTTPClient = &http.Client{}
	}
	httpClient := *cfg.HTTPClient
	httpClient.CheckRedirect = func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}
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
		cfg.MaxVectorDimensions < 1 || cfg.MaxVectorDimensions > MaxVectorDimensions ||
		cfg.EmbeddingDimensions < 0 || cfg.EmbeddingDimensions > cfg.MaxVectorDimensions ||
		cfg.ErrorSnippetBytes < 1 || cfg.ErrorSnippetBytes > maxConfiguredSnippetBytes {
		return nil, &Error{Kind: ErrInvalidConfig, Operation: op}
	}
	return &openAIClient{
		baseURL:             strings.TrimSuffix(baseURL, "/"),
		egresses:            egressesForConfiguredURL(baseURL),
		generationModel:     generationModel,
		embeddingModel:      embeddingModel,
		embeddingDimensions: cfg.EmbeddingDimensions,
		organization:        organization,
		project:             project,
		credentials:         cfg.Credentials,
		httpClient:          &httpClient,
		timeout:             cfg.Timeout,
		maxTextBytes:        cfg.MaxTextBytes,
		maxRequestBytes:     cfg.MaxRequestBytes,
		maxResponseBytes:    cfg.MaxResponseBytes,
		maxVectorDimensions: cfg.MaxVectorDimensions,
		errorSnippetBytes:   cfg.ErrorSnippetBytes,
	}, nil
}

func openAICredentialCacheIdentity(credentials CredentialResolver) (string, bool, error) {
	if credentials == nil {
		return "none", true, nil
	}
	return configuredCacheIdentity(credentials)
}

func validConfigValue(value string, limit int) bool {
	if value == "" || len(value) > limit || !utf8.ValidString(value) {
		return false
	}
	for i := 0; i < len(value); i++ {
		if value[i] < ' ' || value[i] == 0x7f {
			return false
		}
	}
	return true
}

func validHTTPHeaderValue(value string, limit int) bool {
	if value == "" || len(value) > limit || !utf8.ValidString(value) {
		return false
	}
	for i := 0; i < len(value); i++ {
		if (value[i] < ' ' && value[i] != '\t') || value[i] == 0x7f {
			return false
		}
	}
	return true
}

func validCredential(value []byte) bool {
	if len(value) == 0 || len(value) > maxCredentialBytes ||
		len(bytes.TrimSpace(value)) != len(value) {
		return false
	}
	for _, b := range value {
		if (b < ' ' && b != '\t') || b == 0x7f {
			return false
		}
	}
	return true
}

func (o *openAIClient) post(ctx context.Context, path, op string, payload, out interface{}) error {
	if err := ctx.Err(); err != nil {
		return contextError(op, err)
	}
	body, err := json.Marshal(payload)
	if err != nil || int64(len(body)) > o.maxRequestBytes {
		return &Error{Kind: ErrInvalidRequest, Operation: op}
	}
	callCtx, cancel := context.WithTimeout(ctx, o.timeout)
	defer cancel()

	var credential []byte
	if o.credentials != nil {
		resolved, err := o.credentials.ResolveCredential(callCtx)
		if err != nil {
			if ctx.Err() != nil || callCtx.Err() != nil {
				return classifyTransportError(ctx, callCtx, op, err)
			}
			return &Error{Kind: ErrCredential, Operation: op}
		}
		if len(resolved) > maxCredentialBytes {
			return &Error{Kind: ErrCredential, Operation: op}
		}
		credential = append([]byte(nil), resolved...)
		defer clear(credential)
		if !validCredential(credential) {
			return &Error{Kind: ErrCredential, Operation: op}
		}
	}

	request, err := http.NewRequestWithContext(callCtx, http.MethodPost, o.baseURL+path, bytes.NewReader(body))
	if err != nil {
		return &Error{Kind: ErrInvalidConfig, Operation: op}
	}
	request.Header.Set("Content-Type", "application/json")
	if credential != nil {
		request.Header.Set(authorizationHeaderName, "Bearer "+string(credential))
	}
	if o.organization != "" {
		request.Header.Set("OpenAI-Organization", o.organization)
	}
	if o.project != "" {
		request.Header.Set("OpenAI-Project", o.project)
	}
	response, err := o.httpClient.Do(request)
	if err != nil {
		return classifyTransportError(ctx, callCtx, op, err)
	}
	defer response.Body.Close()
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		return o.responseError(ctx, callCtx, op, response)
	}
	limited := &io.LimitedReader{R: response.Body, N: o.maxResponseBytes + 1}
	data, err := io.ReadAll(limited)
	if err != nil {
		return classifyTransportError(ctx, callCtx, op, err)
	}
	if int64(len(data)) > o.maxResponseBytes {
		return &Error{Kind: ErrOversizedResponse, Operation: op}
	}
	if !utf8.Valid(data) {
		return &Error{Kind: ErrMalformedResponse, Operation: op}
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

func (o *openAIClient) responseError(ctx, callCtx context.Context, op string, response *http.Response) error {
	snippet, truncated, err := readSnippet(response.Body, o.errorSnippetBytes)
	if err != nil {
		return classifyTransportError(ctx, callCtx, op, err)
	}
	kind := ErrUnavailable
	retryable := response.StatusCode >= http.StatusInternalServerError
	switch response.StatusCode {
	case http.StatusUnauthorized, http.StatusForbidden:
		kind = ErrAuthentication
	case http.StatusTooManyRequests:
		kind = ErrRateLimited
		retryable = true
	}
	detail := "response body redacted (" + strconv.Itoa(len(snippet)) + " bytes"
	if truncated {
		detail += "+"
	}
	detail += ")"
	return &Error{
		Kind:       kind,
		Operation:  op,
		StatusCode: response.StatusCode,
		Detail:     detail,
		Retryable:  retryable,
		RetryAfter: parseRetryAfter(response.Header.Get("Retry-After"), time.Now()),
	}
}

func parseRetryAfter(value string, now time.Time) time.Duration {
	value = strings.TrimSpace(value)
	if value == "" {
		return 0
	}
	if seconds, err := strconv.ParseInt(value, 10, 64); err == nil {
		if seconds < 0 {
			return 0
		}
		if seconds >= int64(maxRetryAfter/time.Second) {
			return maxRetryAfter
		}
		duration := time.Duration(seconds) * time.Second
		return duration
	}
	when, err := http.ParseTime(value)
	if err != nil || !when.After(now) {
		return 0
	}
	duration := when.Sub(now)
	if duration > maxRetryAfter {
		return maxRetryAfter
	}
	return duration
}

var (
	_ TextGenerator                  = (*OpenAIGenerator)(nil)
	_ Embedder                       = (*OpenAIEmbedder)(nil)
	_ EmbeddingSpaceIdentityProvider = (*OpenAIEmbedder)(nil)
)

// EgressesOffHost reports whether this OpenAI-compatible endpoint is remote.
//
// The class is configuration, not code: the same client against a loopback
// inference server transmits nothing, and against a hosted API transmits every
// prompt it is given. A nil client is unconfigured rather than local, so it
// fails closed.
func (o *OpenAIGenerator) EgressesOffHost() bool {
	return o == nil || o.client == nil || o.client.egresses
}

func (o *OpenAIEmbedder) EgressesOffHost() bool {
	return o == nil || o.client == nil || o.client.egresses
}
