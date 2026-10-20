package model

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/phrocker/shoal-oss/internal/weightsbundle"
)

// StudentProvider is the provider name reported in every student identity.
const StudentProvider = "local-student"

// Student bounds and defaults.
const (
	DefaultStudentTimeout                = 30 * time.Second
	DefaultStudentMaxRequestBytes  int64 = 1 << 20
	DefaultStudentMaxResponseBytes int64 = 4 << 20
	DefaultStudentMaxInputTokens         = 4096
	MaxStudentHealthResponseBytes  int64 = 64 << 10
	studentAbsent                        = "absent"
)

// Student event kinds recorded against a release.
const (
	StudentEventLoadFailure    = weightsbundle.EventLoadFailure
	StudentEventDeviceFallback = "device_fallback"
	StudentEventHealthMismatch = "health_mismatch"
)

// StudentRelease is the exact release a provider serves. It comes from the
// release registry, never from a path an operator typed.
type StudentRelease struct {
	ID           string
	BundleDigest string
}

// StudentEventRecorder receives load failures, health mismatches, and
// device fallbacks against the release. weightsbundle.Registry satisfies it.
type StudentEventRecorder interface {
	RecordStudentEvent(ctx context.Context, releaseID, kind, detail string) error
}

// StudentConfig describes one pinned student runtime behind a local
// endpoint serving one promoted weights bundle.
type StudentConfig struct {
	BaseURL     string
	BearerToken string
	BundleRoot  string
	Release     StudentRelease
	ModelAlias  string
	CriteriaID  string
	// Device is the device the release was promoted for. A runtime that
	// reports another device is a fallback event, and serving is refused.
	Device                string
	DistributionTolerance float64
	HTTPClient            *http.Client
	Timeout               time.Duration
	MaxRequestBytes       int64
	MaxResponseBytes      int64
	MaxInputTokens        int
	MaxConcurrent         int
	// TokenEstimator counts tokens of the exact request serialization. The
	// default is the chars/4 heuristic used elsewhere in this repository.
	TokenEstimator func([]byte) int
	Recorder       StudentEventRecorder
}

// StudentReport is what the provider observed from its runtime.
type StudentReport struct {
	ActualDevice   string
	FallbackEvents []string
	Ready          bool
}

// StudentPredictor serves a verified weights bundle through a loopback
// runtime. Construction verifies every file digest and the bundle digest
// against the release; nothing is served from a bundle that differs from
// what was promoted.
type StudentPredictor struct {
	baseURL  string
	token    string
	id       TypedIdentity
	release  StudentRelease
	client   *http.Client
	timeout  time.Duration
	maxReq   int64
	maxResp  int64
	maxToks  int
	tokens   func([]byte) int
	gate     chan struct{}
	egresses bool
	recorder StudentEventRecorder

	mu     sync.Mutex
	report StudentReport
}

// NewStudentPredictor verifies the bundle and constructs the client. A
// bundle altered on disk after promotion fails here, and the failure is
// recorded against the release before the error returns. Construction
// performs no network access.
func NewStudentPredictor(ctx context.Context, cfg StudentConfig) (*StudentPredictor, error) {
	u, err := url.Parse(strings.TrimSpace(cfg.BaseURL))
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.Hostname() == "" {
		return nil, fmt.Errorf("%w: invalid student base URL", ErrInvalidConfig)
	}
	if cfg.Release.ID == "" || len(cfg.Release.BundleDigest) != 64 {
		return nil, fmt.Errorf("%w: student release identity is required", ErrInvalidConfig)
	}
	if strings.TrimSpace(cfg.ModelAlias) == "" || strings.TrimSpace(cfg.CriteriaID) == "" || strings.TrimSpace(cfg.Device) == "" {
		return nil, fmt.Errorf("%w: student alias, criteria, and device are required", ErrInvalidConfig)
	}
	if strings.ContainsAny(cfg.BearerToken, "\r\n") {
		return nil, fmt.Errorf("%w: invalid student bearer token", ErrInvalidConfig)
	}
	if cfg.Timeout < 0 || cfg.MaxRequestBytes < 0 || cfg.MaxResponseBytes < 0 || cfg.MaxInputTokens < 0 || cfg.MaxConcurrent < 0 {
		return nil, fmt.Errorf("%w: negative student bound", ErrInvalidConfig)
	}
	timeout, maxReq, maxResp, maxToks, concurrent := cfg.Timeout, cfg.MaxRequestBytes, cfg.MaxResponseBytes, cfg.MaxInputTokens, cfg.MaxConcurrent
	if timeout == 0 {
		timeout = DefaultStudentTimeout
	}
	if maxReq == 0 {
		maxReq = DefaultStudentMaxRequestBytes
	}
	if maxResp == 0 {
		maxResp = DefaultStudentMaxResponseBytes
	}
	if maxResp > 64<<20 || timeout > 10*time.Minute {
		return nil, fmt.Errorf("%w: student bound exceeds cap", ErrInvalidConfig)
	}
	if concurrent == 0 {
		concurrent = 1
	}
	tokens := cfg.TokenEstimator
	if tokens == nil {
		tokens = func(b []byte) int { return (len(b) + 3) / 4 }
	}
	manifest, digest, err := weightsbundle.Load(cfg.BundleRoot)
	if err == nil && digest != cfg.Release.BundleDigest {
		err = fmt.Errorf("%w: bundle digest %s does not match release %s digest %s", weightsbundle.ErrVerification, digest, cfg.Release.ID, cfg.Release.BundleDigest)
	}
	if err != nil {
		if cfg.Recorder != nil {
			_ = cfg.Recorder.RecordStudentEvent(ctx, cfg.Release.ID, StudentEventLoadFailure, err.Error())
		}
		return nil, fmt.Errorf("%w: %v", ErrTypedContract, err)
	}
	if maxToks == 0 {
		maxToks = studentMaxInputTokens(cfg.BundleRoot, manifest)
	}
	role := func(r string) string {
		if f, ok := manifest.File(r); ok {
			return f.SHA256
		}
		return studentAbsent
	}
	id := TypedIdentity{
		Provider: StudentProvider, ModelAlias: cfg.ModelAlias, Revision: cfg.Release.ID,
		WeightsDigest: role(weightsbundle.RoleWeights), TokenizerDigest: role(weightsbundle.RoleTokenizer),
		CriteriaID: cfg.CriteriaID, PreprocessingID: role(weightsbundle.RolePreprocessing),
		CalibrationID: role(weightsbundle.RoleCalibration), RuntimeID: role(weightsbundle.RoleRuntime),
		Device: cfg.Device, DistributionTolerance: cfg.DistributionTolerance,
	}
	if err := validateIdentity(id); err != nil {
		return nil, err
	}
	client := cfg.HTTPClient
	if client == nil {
		client = http.DefaultClient
	}
	copied := *client
	copied.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &StudentPredictor{
		baseURL: strings.TrimRight(cfg.BaseURL, "/"), token: strings.TrimSpace(cfg.BearerToken), id: id, release: cfg.Release,
		client: &copied, timeout: timeout, maxReq: maxReq, maxResp: maxResp, maxToks: maxToks, tokens: tokens,
		gate: make(chan struct{}, concurrent), egresses: egressesForConfiguredURL(cfg.BaseURL), recorder: cfg.Recorder,
	}, nil
}

// studentMaxInputTokens reads max_input_tokens from the bundle's runtime
// settings file when present. Settings travel with the bundle so the serving
// bound is part of the promoted identity.
func studentMaxInputTokens(root string, m weightsbundle.Manifest) int {
	f, ok := m.File(weightsbundle.RoleRuntime)
	if !ok {
		return DefaultStudentMaxInputTokens
	}
	raw, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(f.Path)))
	if err != nil {
		return DefaultStudentMaxInputTokens
	}
	var settings struct {
		MaxInputTokens int `json:"max_input_tokens"`
	}
	if json.Unmarshal(raw, &settings) != nil || settings.MaxInputTokens <= 0 {
		return DefaultStudentMaxInputTokens
	}
	return settings.MaxInputTokens
}

func (p *StudentPredictor) Identity() TypedIdentity { return p.id }

// Release reports the exact release this provider serves.
func (p *StudentPredictor) Release() StudentRelease { return p.release }

// EgressesOffHost is derived from the resolved endpoint: loopback is
// egress-free; any other host, including another pod in the same cluster
// speaking the identical protocol, is egressing.
func (p *StudentPredictor) EgressesOffHost() bool { return p == nil || p.egresses }

// Report returns the last observed device and fallback events.
func (p *StudentPredictor) Report() StudentReport {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := p.report
	out.FallbackEvents = append([]string(nil), p.report.FallbackEvents...)
	return out
}

type studentHealth struct {
	Status         string   `json:"status"`
	ReleaseID      string   `json:"release_id"`
	BundleDigest   string   `json:"bundle_digest"`
	ModelAlias     string   `json:"model_alias"`
	Device         string   `json:"device"`
	FallbackEvents []string `json:"fallback_events"`
}

// CheckHealth asks the runtime what it serves and refuses a runtime whose
// release, bundle digest, or device differs from the pinned identity. A
// device difference is recorded as a fallback event against the release.
func (p *StudentPredictor) CheckHealth(ctx context.Context) error {
	if p == nil {
		return fmt.Errorf("%w: nil student predictor", ErrInvalidConfig)
	}
	callCtx, cancel := context.WithTimeout(ctx, p.timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(callCtx, http.MethodGet, p.baseURL+"/health", nil)
	if err != nil {
		return &Error{Kind: ErrInvalidRequest, Operation: "student health"}
	}
	p.authorize(req)
	resp, err := p.client.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return contextError("student health", ctx.Err())
		}
		p.setReady(false)
		return &Error{Kind: ErrUnavailable, Operation: "student health", Detail: err.Error(), Retryable: true}
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, MaxStudentHealthResponseBytes+1))
	if err != nil {
		return &Error{Kind: ErrUnavailable, Operation: "student health", Detail: err.Error(), Retryable: true}
	}
	if int64(len(body)) > MaxStudentHealthResponseBytes {
		return &Error{Kind: ErrOversizedResponse, Operation: "student health"}
	}
	if resp.StatusCode != http.StatusOK {
		p.setReady(false)
		return &Error{Kind: ErrUnavailable, Operation: "student health", StatusCode: resp.StatusCode, Retryable: resp.StatusCode >= 500}
	}
	var health studentHealth
	if err := json.Unmarshal(body, &health); err != nil {
		return &Error{Kind: ErrMalformedResponse, Operation: "student health", Detail: err.Error()}
	}
	return p.checkServed(ctx, health.ReleaseID, health.BundleDigest, health.ModelAlias, health.Device, health.Status == "ready", health.FallbackEvents)
}

func (p *StudentPredictor) checkServed(ctx context.Context, releaseID, digest, alias, device string, ready bool, fallbacks []string) error {
	p.mu.Lock()
	p.report.ActualDevice = device
	p.report.Ready = ready
	for _, f := range fallbacks {
		if !contains(p.report.FallbackEvents, f) {
			p.report.FallbackEvents = append(p.report.FallbackEvents, f)
		}
	}
	p.mu.Unlock()
	if releaseID != p.release.ID || digest != p.release.BundleDigest || alias != p.id.ModelAlias {
		detail := fmt.Sprintf("runtime serves release %q bundle %q alias %q; pinned release %q bundle %q alias %q", releaseID, digest, alias, p.release.ID, p.release.BundleDigest, p.id.ModelAlias)
		p.record(ctx, StudentEventHealthMismatch, detail)
		return &Error{Kind: ErrUnavailable, Operation: "student identity", Detail: detail}
	}
	if device != p.id.Device {
		detail := fmt.Sprintf("runtime reports device %q; release promoted for %q", device, p.id.Device)
		p.mu.Lock()
		if !contains(p.report.FallbackEvents, detail) {
			p.report.FallbackEvents = append(p.report.FallbackEvents, detail)
		}
		p.mu.Unlock()
		p.record(ctx, StudentEventDeviceFallback, detail)
		return &Error{Kind: ErrUnavailable, Operation: "student device", Detail: detail}
	}
	if !ready {
		return &Error{Kind: ErrUnavailable, Operation: "student readiness", Detail: "runtime not ready", Retryable: true}
	}
	return nil
}

func (p *StudentPredictor) setReady(ready bool) {
	p.mu.Lock()
	p.report.Ready = ready
	p.mu.Unlock()
}

func (p *StudentPredictor) record(ctx context.Context, kind, detail string) {
	if p.recorder != nil {
		_ = p.recorder.RecordStudentEvent(ctx, p.release.ID, kind, detail)
	}
}

func (p *StudentPredictor) authorize(req *http.Request) {
	if p.token != "" {
		req.Header.Set("Authorization", "Bearer "+p.token)
	}
}

type studentRequest struct {
	Model        string                     `json:"model"`
	ReleaseID    string                     `json:"release_id"`
	BundleDigest string                     `json:"bundle_digest"`
	State        json.RawMessage            `json:"state"`
	Questions    map[string]studentQuestion `json:"questions"`
}

type studentQuestion struct {
	Type         string   `json:"type"`
	Instructions string   `json:"instructions"`
	Options      []string `json:"options,omitempty"`
}

type studentResponse struct {
	ReleaseID    string                   `json:"release_id"`
	BundleDigest string                   `json:"bundle_digest"`
	ModelAlias   string                   `json:"model_alias"`
	Device       string                   `json:"device"`
	Status       string                   `json:"status"`
	Reason       string                   `json:"reason"`
	Answers      map[string]studentAnswer `json:"answers"`
}

type studentAnswer struct {
	Type          string             `json:"type"`
	Choice        string             `json:"choice"`
	Score         *float64           `json:"score"`
	PTrue         *float64           `json:"p_true"`
	Confidence    float64            `json:"confidence"`
	Probabilities map[string]float64 `json:"probabilities"`
}

// Predict runs one bounded, deadline-limited, concurrency-gated call. The
// request names the exact release and bundle digest; the response must echo
// them, so every result carries the release identity it came from.
func (p *StudentPredictor) Predict(ctx context.Context, req TypedRequest) (TypedResult, error) {
	if p == nil {
		return TypedResult{}, fmt.Errorf("%w: nil student predictor", ErrInvalidConfig)
	}
	if err := ValidateTypedRequest(req, p.id); err != nil {
		return TypedResult{}, err
	}
	if !json.Valid(req.State) {
		return TypedResult{}, &Error{Kind: ErrInvalidRequest, Operation: "student encode"}
	}
	payload := studentRequest{Model: p.id.ModelAlias, ReleaseID: p.release.ID, BundleDigest: p.release.BundleDigest, State: json.RawMessage(req.State), Questions: make(map[string]studentQuestion, len(req.Questions))}
	for _, q := range req.Questions {
		instructions := q.Instructions
		if instructions == "" {
			instructions = q.ID
		}
		sq := studentQuestion{Instructions: instructions, Options: append([]string(nil), q.Options...)}
		switch q.Kind {
		case TypedChoice:
			sq.Type = "choice"
		case TypedOrdinalScore:
			sq.Type = "score"
		case TypedPropositionProbability:
			sq.Type = "proposition"
		}
		payload.Questions[q.ID] = sq
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return TypedResult{}, &Error{Kind: ErrInvalidRequest, Operation: "student encode"}
	}
	if int64(len(body)) > p.maxReq {
		return TypedResult{}, &Error{Kind: ErrInvalidRequest, Operation: "student encode", Detail: "request exceeds bound"}
	}
	if n := p.tokens(body); n > p.maxToks {
		return TypedResult{}, &Error{Kind: ErrInvalidRequest, Operation: "student preflight", Detail: fmt.Sprintf("%d tokens exceed %d", n, p.maxToks)}
	}
	select {
	case p.gate <- struct{}{}:
		defer func() { <-p.gate }()
	case <-ctx.Done():
		return TypedResult{}, contextError("student gate", ctx.Err())
	}
	callCtx, cancel := context.WithTimeout(ctx, p.timeout)
	defer cancel()
	httpReq, err := http.NewRequestWithContext(callCtx, http.MethodPost, p.baseURL+"/v1/predict", bytes.NewReader(body))
	if err != nil {
		return TypedResult{}, &Error{Kind: ErrInvalidRequest, Operation: "student request"}
	}
	httpReq.Header.Set("Content-Type", "application/json")
	p.authorize(httpReq)
	resp, err := p.client.Do(httpReq)
	if err != nil {
		if ctx.Err() != nil {
			return TypedResult{}, contextError("student request", ctx.Err())
		}
		if errors.Is(err, context.DeadlineExceeded) || callCtx.Err() != nil {
			return TypedResult{}, &Error{Kind: ErrTimeout, Operation: "student request", Detail: err.Error(), Retryable: true}
		}
		return TypedResult{}, &Error{Kind: ErrUnavailable, Operation: "student request", Detail: err.Error(), Retryable: true}
	}
	defer resp.Body.Close()
	responseBody, readErr := io.ReadAll(io.LimitReader(resp.Body, p.maxResp+1))
	if readErr != nil {
		return TypedResult{}, &Error{Kind: ErrUnavailable, Operation: "student read", Detail: readErr.Error(), Retryable: true}
	}
	if int64(len(responseBody)) > p.maxResp {
		return TypedResult{}, &Error{Kind: ErrOversizedResponse, Operation: "student read"}
	}
	if resp.StatusCode != http.StatusOK {
		return TypedResult{}, &Error{Kind: ErrUnavailable, Operation: "student response", StatusCode: resp.StatusCode, Retryable: resp.StatusCode >= 500}
	}
	var wire studentResponse
	if err := json.Unmarshal(responseBody, &wire); err != nil {
		return TypedResult{}, &Error{Kind: ErrMalformedResponse, Operation: "student decode", Detail: err.Error()}
	}
	if err := p.checkServed(ctx, wire.ReleaseID, wire.BundleDigest, wire.ModelAlias, wire.Device, true, nil); err != nil {
		return TypedResult{}, err
	}
	switch wire.Status {
	case "completed", "":
	case string(TypedAbstained), string(TypedFailed):
		if wire.Reason == "" {
			return TypedResult{}, fmt.Errorf("%w: %s without reason", ErrMalformedResponse, wire.Status)
		}
		return TypedResult{Identity: p.id, Status: TypedStatus(wire.Status), Reason: wire.Reason}, nil
	default:
		return TypedResult{}, fmt.Errorf("%w: unknown student status %q", ErrMalformedResponse, wire.Status)
	}
	result, err := decodeStudentResult(req, wire)
	if err != nil {
		return TypedResult{}, err
	}
	result.Identity = p.id
	return result, nil
}

func decodeStudentResult(req TypedRequest, wire studentResponse) (TypedResult, error) {
	if len(wire.Answers) != len(req.Questions) {
		return TypedResult{}, fmt.Errorf("%w: student answer count mismatch", ErrMalformedResponse)
	}
	result := TypedResult{Status: TypedCompleted, Answers: make([]TypedAnswer, 0, len(req.Questions))}
	for _, q := range req.Questions {
		answer, ok := wire.Answers[q.ID]
		if !ok {
			return TypedResult{}, fmt.Errorf("%w: missing student answer %q", ErrMalformedResponse, q.ID)
		}
		ta := TypedAnswer{QuestionID: q.ID, Choice: answer.Choice, Confidence: answer.Confidence, AnswerConfidence: answer.Confidence}
		switch q.Kind {
		case TypedChoice, TypedOrdinalScore:
			want := "choice"
			if q.Kind == TypedOrdinalScore {
				want = "score"
				if answer.Score == nil {
					return TypedResult{}, fmt.Errorf("%w: missing student score", ErrMalformedResponse)
				}
				ta.Score = *answer.Score
			}
			if answer.Type != want {
				return TypedResult{}, fmt.Errorf("%w: wrong student answer type", ErrMalformedResponse)
			}
			if len(answer.Probabilities) != len(q.Options) {
				return TypedResult{}, fmt.Errorf("%w: unexpected student probability labels", ErrMalformedResponse)
			}
			for _, label := range q.Options {
				value, ok := answer.Probabilities[label]
				if !ok {
					return TypedResult{}, fmt.Errorf("%w: missing student probability", ErrMalformedResponse)
				}
				ta.Distribution = append(ta.Distribution, TypedProbability{Label: label, Value: value})
			}
		case TypedPropositionProbability:
			if answer.Type != "proposition" || answer.PTrue == nil {
				return TypedResult{}, fmt.Errorf("%w: missing student proposition probability", ErrMalformedResponse)
			}
			ta.PTrue = *answer.PTrue
		default:
			return TypedResult{}, fmt.Errorf("%w: unsupported question kind", ErrMalformedResponse)
		}
		result.Answers = append(result.Answers, ta)
	}
	return result, nil
}
