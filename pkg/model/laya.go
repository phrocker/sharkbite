package model

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

const DefaultLayaMaxResponseBytes int64 = 4 << 20
const MaxLayaHealthResponseBytes int64 = 64 << 10

// LayaConfig describes one explicitly pinned, private Laya worker. It does
// not discover checkpoints or fall back to a hosted provider.
type LayaConfig struct {
	BaseURL          string
	BearerToken      string
	Identity         TypedIdentity
	HTTPClient       *http.Client
	MaxResponseBytes int64
}

type LayaPredictor struct {
	baseURL string
	token   string
	id      TypedIdentity
	client  *http.Client
	maxBody int64
}

func NewLayaPredictor(cfg LayaConfig) (*LayaPredictor, error) {
	u, err := url.Parse(strings.TrimSpace(cfg.BaseURL))
	if err != nil || u.Scheme != "http" && u.Scheme != "https" || u.Host == "" {
		return nil, fmt.Errorf("%w: invalid Laya base URL", ErrInvalidConfig)
	}
	if err := validateIdentity(cfg.Identity); err != nil {
		return nil, err
	}
	token := strings.TrimSpace(cfg.BearerToken)
	if token == "" || strings.ContainsAny(token, "\r\n") {
		return nil, fmt.Errorf("%w: Laya bearer token is required", ErrInvalidConfig)
	}
	maxBody := cfg.MaxResponseBytes
	if maxBody == 0 {
		maxBody = DefaultLayaMaxResponseBytes
	}
	if maxBody < 1 || maxBody > 64<<20 {
		return nil, fmt.Errorf("%w: invalid Laya response bound", ErrInvalidConfig)
	}
	client := cfg.HTTPClient
	if client == nil {
		client = http.DefaultClient
	}
	return &LayaPredictor{baseURL: strings.TrimRight(cfg.BaseURL, "/"), token: token, id: cfg.Identity, client: client, maxBody: maxBody}, nil
}

func (p *LayaPredictor) Identity() TypedIdentity { return p.id }

// CheckHealth performs the worker's authenticated transport/readiness check.
// A successful HTTP status proves only that the worker answered its health
// endpoint; model quality and checkpoint correctness remain pinned by Identity
// and are not inferred from this call.
func (p *LayaPredictor) CheckHealth(ctx context.Context) error {
	if p == nil {
		return fmt.Errorf("%w: nil Laya predictor", ErrInvalidConfig)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, p.baseURL+"/health", nil)
	if err != nil {
		return &Error{Kind: ErrInvalidRequest, Operation: "laya health"}
	}
	req.Header.Set("Authorization", "Bearer "+p.token)
	resp, err := p.client.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return contextError("laya health", ctx.Err())
		}
		return &Error{Kind: ErrUnavailable, Operation: "laya health", Detail: err.Error(), Retryable: true}
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, MaxLayaHealthResponseBytes+1))
	if err != nil {
		return &Error{Kind: ErrUnavailable, Operation: "laya health", Detail: err.Error(), Retryable: true}
	}
	if int64(len(body)) > MaxLayaHealthResponseBytes {
		return &Error{Kind: ErrOversizedResponse, Operation: "laya health"}
	}
	if resp.StatusCode != http.StatusOK {
		return &Error{Kind: ErrUnavailable, Operation: "laya health", StatusCode: resp.StatusCode, Retryable: resp.StatusCode >= 500}
	}
	return nil
}

func (p *LayaPredictor) Predict(ctx context.Context, req TypedRequest) (TypedResult, error) {
	if p == nil {
		return TypedResult{}, fmt.Errorf("%w: nil Laya predictor", ErrInvalidConfig)
	}
	if err := ValidateTypedRequest(req, p.id); err != nil {
		return TypedResult{}, err
	}
	if !json.Valid(req.State) {
		return TypedResult{}, &Error{Kind: ErrInvalidRequest, Operation: "laya encode"}
	}
	payload := layaRequest{Model: p.id.ModelAlias, State: json.RawMessage(req.State), Questions: make(map[string]layaQuestion, len(req.Questions))}
	for _, q := range req.Questions {
		instructions := q.Instructions
		if instructions == "" {
			instructions = q.ID
		}
		lq := layaQuestion{Instructions: instructions}
		switch q.Kind {
		case TypedChoice:
			lq.Type = "choice"
			criteria := make(map[string]string, len(q.Options))
			for _, option := range q.Options {
				criteria[option] = option
				if description, ok := q.OptionDescriptions[option]; ok {
					criteria[option] = description
				}
			}
			lq.Criteria = criteria
		case TypedOrdinalScore:
			lq.Type = "score"
			criteria := make([]string, len(q.Options))
			for i, option := range q.Options {
				criteria[i] = option
				if description, ok := q.OptionDescriptions[option]; ok {
					criteria[i] = description
				}
			}
		case TypedPropositionProbability:
			lq.Type = "noul"
			lq.Criteria = map[string]string{"true": "true", "false": "false"}
		}
		payload.Questions[q.ID] = lq
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return TypedResult{}, &Error{Kind: ErrInvalidRequest, Operation: "laya encode"}
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, p.baseURL+"/v1/systemone", bytes.NewReader(body))
	if err != nil {
		return TypedResult{}, &Error{Kind: ErrInvalidRequest, Operation: "laya request"}
	}
	httpReq.Header.Set("Content-Type", "application/json")
	if p.token != "" {
		httpReq.Header.Set("Authorization", "Bearer "+p.token)
	}
	resp, err := p.client.Do(httpReq)
	if err != nil {
		if ctx.Err() != nil {
			return TypedResult{}, contextError("laya request", ctx.Err())
		}
		return TypedResult{}, &Error{Kind: ErrUnavailable, Operation: "laya request", Detail: err.Error(), Retryable: true}
	}
	defer resp.Body.Close()
	limited := io.LimitReader(resp.Body, p.maxBody+1)
	responseBody, readErr := io.ReadAll(limited)
	if readErr != nil {
		return TypedResult{}, &Error{Kind: ErrUnavailable, Operation: "laya read", Detail: readErr.Error(), Retryable: true}
	}
	if int64(len(responseBody)) > p.maxBody {
		return TypedResult{}, &Error{Kind: ErrOversizedResponse, Operation: "laya read"}
	}
	if resp.StatusCode != http.StatusOK {
		return TypedResult{}, &Error{Kind: ErrUnavailable, Operation: "laya response", StatusCode: resp.StatusCode, Retryable: resp.StatusCode >= 500}
	}
	var wire layaResponse
	if err := json.Unmarshal(responseBody, &wire); err != nil {
		return TypedResult{}, &Error{Kind: ErrMalformedResponse, Operation: "laya decode", Detail: err.Error()}
	}
	result, err := decodeLayaResult(req, wire)
	if err != nil {
		return TypedResult{}, err
	}
	result.Identity = p.id
	return result, nil
}

type layaRequest struct {
	Model     string                  `json:"model"`
	State     json.RawMessage         `json:"state"`
	Questions map[string]layaQuestion `json:"questions"`
}
type layaQuestion struct {
	Type         string `json:"type"`
	Instructions string `json:"instructions"`
	Criteria     any    `json:"criteria"`
}
type layaResponse struct {
	Answers map[string]layaAnswer `json:"answers"`
}
type layaAnswer struct {
	Type             string             `json:"type"`
	Choice           string             `json:"choice"`
	Score            *float64           `json:"score"`
	Noul             *float64           `json:"noul"`
	Confidence       float64            `json:"confidence"`
	AnswerConfidence float64            `json:"answer_confidence"`
	Probabilities    map[string]float64 `json:"probabilities"`
}

func decodeLayaResult(req TypedRequest, wire layaResponse) (TypedResult, error) {
	if len(wire.Answers) != len(req.Questions) {
		return TypedResult{}, fmt.Errorf("%w: Laya answer count mismatch", ErrMalformedResponse)
	}
	result := TypedResult{Identity: TypedIdentity{}, Status: TypedCompleted, Answers: make([]TypedAnswer, 0, len(req.Questions))}
	// Identity is supplied by the caller after transport decoding; the adapter
	// fills it in at the boundary so ValidateTypedResult can pin it.
	for _, q := range req.Questions {
		answer, ok := wire.Answers[q.ID]
		if !ok {
			return TypedResult{}, fmt.Errorf("%w: missing Laya answer %q", ErrMalformedResponse, q.ID)
		}
		ta := TypedAnswer{QuestionID: q.ID, Choice: answer.Choice, Confidence: answer.Confidence, AnswerConfidence: answer.AnswerConfidence}
		switch q.Kind {
		case TypedChoice:
			if answer.Type != "choice" {
				return TypedResult{}, fmt.Errorf("%w: wrong Laya answer type", ErrMalformedResponse)
			}
			if len(answer.Probabilities) != len(q.Options) {
				return TypedResult{}, fmt.Errorf("%w: unexpected Laya probability labels", ErrMalformedResponse)
			}
			for _, label := range q.Options {
				value, ok := answer.Probabilities[label]
				if !ok {
					return TypedResult{}, fmt.Errorf("%w: missing Laya probability", ErrMalformedResponse)
				}
				ta.Distribution = append(ta.Distribution, TypedProbability{Label: label, Value: value})
			}
		case TypedOrdinalScore:
			if answer.Type != "score" {
				return TypedResult{}, fmt.Errorf("%w: wrong Laya answer type", ErrMalformedResponse)
			}
			if len(answer.Probabilities) != len(q.Options) {
				return TypedResult{}, fmt.Errorf("%w: unexpected Laya score labels", ErrMalformedResponse)
			}
			if answer.Score == nil {
				return TypedResult{}, fmt.Errorf("%w: missing Laya score", ErrMalformedResponse)
			}
			ta.Score = *answer.Score
			for i := range q.Options {
				key := strconv.Itoa(i)
				value, ok := answer.Probabilities[key]
				if !ok {
					return TypedResult{}, fmt.Errorf("%w: missing Laya score probability", ErrMalformedResponse)
				}
				ta.Distribution = append(ta.Distribution, TypedProbability{Label: q.Options[i], Value: value})
			}
		case TypedPropositionProbability:
			if answer.Type != "noul" {
				return TypedResult{}, fmt.Errorf("%w: wrong Laya answer type", ErrMalformedResponse)
			}
			if answer.Noul == nil {
				return TypedResult{}, fmt.Errorf("%w: missing Laya proposition probability", ErrMalformedResponse)
			}
			ta.PTrue = *answer.Noul
		default:
			return TypedResult{}, fmt.Errorf("%w: unsupported question kind", ErrMalformedResponse)
		}
		result.Answers = append(result.Answers, ta)
	}
	return result, nil
}
