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
	"os"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

const (
	DefaultVoyageBaseURL  = "https://api.voyageai.com"
	voyageProvider        = "voyage"
	voyageEmbeddingPath   = "/v1/embeddings"
	maxInputTypeBytes     = 64
	maxCredentialEnvBytes = 256
)

type VoyageConfig struct {
	BaseURL          string
	Model            string
	Dimensions       int
	InputType        string
	APICredentialEnv string
	HTTPClient       *http.Client
	Timeout          time.Duration

	MaxTextBytes      int64
	MaxRequestBytes   int64
	MaxResponseBytes  int64
	ErrorSnippetBytes int64
}

type VoyageEmbedder struct {
	cfg                 VoyageConfig
	endpoint            string
	httpClientIdentity  string
	cacheIdentityUnsafe bool
	egresses            bool
}

func NewVoyageEmbedder(cfg VoyageConfig) (*VoyageEmbedder, error) {
	httpIdentity, cacheable, err := httpClientCacheIdentity(cfg.HTTPClient)
	if err != nil {
		return nil, err
	}
	cfg, endpoint, err := validateVoyageConfig(cfg)
	if err != nil {
		return nil, err
	}
	return &VoyageEmbedder{
		cfg: cfg, endpoint: endpoint, httpClientIdentity: httpIdentity, cacheIdentityUnsafe: !cacheable,
		egresses: egressesForConfiguredURL(cfg.BaseURL),
	}, nil
}

func (v *VoyageEmbedder) CacheIdentity() (string, error) {
	if v == nil {
		return "", ErrInvalidConfig
	}
	if v.cacheIdentityUnsafe {
		return "", ErrInvalidConfig
	}
	space, err := v.EmbeddingSpaceIdentity()
	if err != nil {
		return "", err
	}
	return framedModelIdentity(
		"voyage-embedder-v1",
		v.endpoint,
		space,
		v.cfg.InputType,
		v.cfg.APICredentialEnv,
		v.httpClientIdentity,
		v.cfg.Timeout.String(),
		strconv.FormatInt(v.cfg.MaxTextBytes, 10),
		strconv.FormatInt(v.cfg.MaxRequestBytes, 10),
		strconv.FormatInt(v.cfg.MaxResponseBytes, 10),
		strconv.FormatInt(v.cfg.ErrorSnippetBytes, 10),
	), nil
}

func (v *VoyageEmbedder) EmbeddingSpaceIdentity() (string, error) {
	if v == nil {
		return "", ErrInvalidConfig
	}
	return embeddingSpaceIdentity(voyageProvider, v.cfg.Model, v.cfg.Dimensions, normalizationProviderNativeUnchanged)
}

func (v *VoyageEmbedder) Embed(ctx context.Context, req EmbedRequest) (EmbedResult, error) {
	const op = "voyage embed"
	if v == nil {
		return EmbedResult{}, &Error{Kind: ErrInvalidConfig, Operation: op}
	}
	if err := validateTextRequest(op, req.Text, 0, v.cfg.MaxTextBytes); err != nil {
		return EmbedResult{}, err
	}
	payload := struct {
		Model           string   `json:"model"`
		Input           []string `json:"input"`
		InputType       string   `json:"input_type,omitempty"`
		OutputDimension int      `json:"output_dimension,omitempty"`
	}{
		Model:           v.cfg.Model,
		Input:           []string{req.Text},
		InputType:       v.cfg.InputType,
		OutputDimension: v.cfg.Dimensions,
	}
	var out struct {
		Data []struct {
			Embedding []strictFloat64 `json:"embedding"`
			Index     *int            `json:"index"`
		} `json:"data"`
		Model string `json:"model"`
		Usage struct {
			TotalTokens int `json:"total_tokens"`
		} `json:"usage"`
	}
	if err := v.post(ctx, op, payload, &out); err != nil {
		return EmbedResult{}, err
	}
	if out.Model != v.cfg.Model {
		return EmbedResult{}, &Error{Kind: ErrMalformedResponse, Operation: op, Detail: "model mismatch"}
	}
	if len(out.Data) != 1 || out.Data[0].Index == nil || *out.Data[0].Index != 0 ||
		len(out.Data[0].Embedding) == 0 {
		return EmbedResult{}, &Error{Kind: ErrMalformedResponse, Operation: op, Detail: "expected exactly one non-empty embedding"}
	}
	if len(out.Data[0].Embedding) != v.cfg.Dimensions {
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
	if !validUsage(out.Usage.TotalTokens) {
		return EmbedResult{}, &Error{Kind: ErrMalformedResponse, Operation: op, Detail: "invalid usage"}
	}
	return EmbedResult{
		Vector:     append([]float32(nil), vector...),
		Provenance: Provenance{Provider: voyageProvider, Model: v.cfg.Model},
		Usage:      Usage{InputTokens: out.Usage.TotalTokens, TotalTokens: out.Usage.TotalTokens},
	}, nil
}

func validateVoyageConfig(cfg VoyageConfig) (VoyageConfig, string, error) {
	const op = "configure voyage"
	cfg.BaseURL = strings.TrimSpace(cfg.BaseURL)
	if cfg.BaseURL == "" {
		cfg.BaseURL = DefaultVoyageBaseURL
	}
	cfg.Model = strings.TrimSpace(cfg.Model)
	cfg.InputType = strings.TrimSpace(cfg.InputType)
	cfg.APICredentialEnv = strings.TrimSpace(cfg.APICredentialEnv)
	if !validConfigValue(cfg.Model, maxModelBytes) ||
		(cfg.InputType != "" && !validConfigValue(cfg.InputType, maxInputTypeBytes)) ||
		!validCredentialEnvName(cfg.APICredentialEnv) ||
		cfg.Dimensions < 1 || cfg.Dimensions > MaxVectorDimensions {
		return VoyageConfig{}, "", &Error{Kind: ErrInvalidConfig, Operation: op}
	}
	u, err := url.Parse(cfg.BaseURL)
	if err != nil || !u.IsAbs() || u.Hostname() == "" || u.User != nil ||
		u.RawQuery != "" || u.ForceQuery || strings.Contains(cfg.BaseURL, "#") ||
		u.Fragment != "" || (u.Path != "" && u.Path != "/") {
		return VoyageConfig{}, "", &Error{Kind: ErrInvalidConfig, Operation: op}
	}
	if u.Scheme != "https" && !(u.Scheme == "http" && isLoopbackHost(u.Hostname())) {
		return VoyageConfig{}, "", &Error{Kind: ErrInvalidConfig, Operation: op}
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
	if cfg.ErrorSnippetBytes == 0 {
		cfg.ErrorSnippetBytes = DefaultErrorSnippetBytes
	}
	if cfg.Timeout < time.Millisecond || cfg.Timeout > maxConfiguredTimeout ||
		cfg.MaxTextBytes < 1 || cfg.MaxTextBytes > maxConfiguredTextBytes ||
		cfg.MaxRequestBytes < 1 || cfg.MaxRequestBytes > maxConfiguredRequestBytes ||
		cfg.MaxResponseBytes < 1 || cfg.MaxResponseBytes > maxConfiguredResponseBytes ||
		cfg.ErrorSnippetBytes < 1 || cfg.ErrorSnippetBytes > maxConfiguredSnippetBytes {
		return VoyageConfig{}, "", &Error{Kind: ErrInvalidConfig, Operation: op}
	}
	u.Path = voyageEmbeddingPath
	return cfg, u.String(), nil
}

func validCredentialEnvName(value string) bool {
	if value == "" || len(value) > maxCredentialEnvBytes || !utf8.ValidString(value) {
		return false
	}
	for i := 0; i < len(value); i++ {
		b := value[i]
		if (b >= 'A' && b <= 'Z') || (b >= 'a' && b <= 'z') || b == '_' ||
			(i > 0 && b >= '0' && b <= '9') {
			continue
		}
		return false
	}
	return true
}

func (v *VoyageEmbedder) post(ctx context.Context, op string, payload, out interface{}) error {
	if err := ctx.Err(); err != nil {
		return contextError(op, err)
	}
	body, err := json.Marshal(payload)
	if err != nil || int64(len(body)) > v.cfg.MaxRequestBytes {
		return &Error{Kind: ErrInvalidRequest, Operation: op}
	}
	callCtx, cancel := context.WithTimeout(ctx, v.cfg.Timeout)
	defer cancel()

	value, ok := os.LookupEnv(v.cfg.APICredentialEnv)
	if !ok {
		return &Error{Kind: ErrCredential, Operation: op}
	}
	credential := []byte(value)
	if len(credential) > maxCredentialBytes {
		return &Error{Kind: ErrCredential, Operation: op}
	}
	credential = append([]byte(nil), credential...)
	defer clear(credential)
	if !validCredential(credential) {
		return &Error{Kind: ErrCredential, Operation: op}
	}

	request, err := http.NewRequestWithContext(callCtx, http.MethodPost, v.endpoint, bytes.NewReader(body))
	if err != nil {
		return &Error{Kind: ErrInvalidConfig, Operation: op}
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set(authorizationHeaderName, "Bearer "+string(credential))
	response, err := v.cfg.HTTPClient.Do(request)
	if err != nil {
		return classifyTransportError(ctx, callCtx, op, err)
	}
	defer response.Body.Close()
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		return voyageResponseError(ctx, callCtx, op, response, v.cfg.ErrorSnippetBytes)
	}
	limited := &io.LimitedReader{R: response.Body, N: v.cfg.MaxResponseBytes + 1}
	data, err := io.ReadAll(limited)
	if err != nil {
		return classifyTransportError(ctx, callCtx, op, err)
	}
	if int64(len(data)) > v.cfg.MaxResponseBytes {
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

func voyageResponseError(ctx, callCtx context.Context, op string, response *http.Response, snippetBytes int64) error {
	snippet, truncated, err := readSnippet(response.Body, snippetBytes)
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

var (
	_ Embedder                       = (*VoyageEmbedder)(nil)
	_ EmbeddingSpaceIdentityProvider = (*VoyageEmbedder)(nil)
)

// EgressesOffHost reports whether this Voyage endpoint is remote.
//
// The obvious answer is "always": Voyage is a hosted API. But
// validateVoyageConfig deliberately accepts a loopback HTTP base URL, the same
// as the Ollama and OpenAI-compatible providers, so a local Voyage-compatible
// service is a configuration this package supports — and the existing tests
// run against exactly that through httptest. A constant here would report
// those as transmitting off-host when nothing leaves the machine.
//
// So this classifies the validated configuration like every other provider.
// That is the whole point of the class: egress is a property of where the
// endpoint is, not of which vendor's wire format it speaks.
func (v *VoyageEmbedder) EgressesOffHost() bool {
	return v == nil || v.egresses
}
