// Package model defines provider-neutral, low-level model I/O contracts.
// Anthropic publishes no embeddings API; hosted embedding integrations here
// are OpenAI-compatible endpoints and Voyage, not an Anthropic embedder.
package model

import (
	"context"
	"errors"
	"fmt"
	"time"
)

var (
	ErrInvalidConfig     = errors.New("model: invalid configuration")
	ErrInvalidRequest    = errors.New("model: invalid request")
	ErrUnavailable       = errors.New("model: provider unavailable")
	ErrTimeout           = fmt.Errorf("model: request timed out: %w", context.DeadlineExceeded)
	ErrCanceled          = fmt.Errorf("model: request canceled: %w", context.Canceled)
	ErrMalformedResponse = errors.New("model: malformed response")
	ErrOversizedResponse = errors.New("model: oversized response")
	ErrCredential        = errors.New("model: credential unavailable")
	ErrAuthentication    = errors.New("model: authentication failed")
	ErrRateLimited       = errors.New("model: rate limited")
)

// TextGenerator generates text from a bounded provider-neutral request.
type TextGenerator interface {
	Generate(context.Context, GenerateRequest) (GenerateResult, error)
}

// CacheIdentityProvider returns a stable, non-secret identity for
// behavior-bearing provider configuration. Harness caches use it structurally
// and bypass caching when a provider cannot supply one.
type CacheIdentityProvider interface {
	CacheIdentity() (string, error)
}

// EmbeddingSpaceIdentityProvider returns a stable, non-secret identity for the
// vector space produced by an embedder. The identity is suitable for metadata
// persistence and must distinguish provider kind, model identity/version pin,
// dimensionality, and normalization convention.
type EmbeddingSpaceIdentityProvider interface {
	EmbeddingSpaceIdentity() (string, error)
}

// Embedder creates a vector embedding from a bounded provider-neutral request.
type Embedder interface {
	Embed(context.Context, EmbedRequest) (EmbedResult, error)
}

type GenerateRequest struct {
	Prompt          string
	MaxOutputTokens int
}

type GenerateResult struct {
	Text       string
	Provenance Provenance
	Usage      Usage
}

type EmbedRequest struct {
	Text string
}

type EmbedResult struct {
	Vector     []float32
	Provenance Provenance
	Usage      Usage
}

type Provenance struct {
	Provider string
	Model    string
}

type Usage struct {
	InputTokens  int
	OutputTokens int
	TotalTokens  int
}

type Error struct {
	Kind       error
	Operation  string
	StatusCode int
	Detail     string
	Retryable  bool
	RetryAfter time.Duration
}

func (e *Error) Error() string {
	if e == nil {
		return "<nil>"
	}
	msg := "model: " + e.Operation + ": " + e.Kind.Error()
	if e.StatusCode != 0 {
		msg += " (status " + statusText(e.StatusCode) + ")"
	}
	if e.Detail != "" {
		msg += ": " + e.Detail
	}
	return msg
}

func (e *Error) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Kind
}

func statusText(code int) string {
	const digits = "0123456789"
	if code == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	for code > 0 {
		i--
		buf[i] = digits[code%10]
		code /= 10
	}
	return string(buf[i:])
}
