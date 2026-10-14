// Licensed under the Apache License, Version 2.0. See LICENSE and NOTICE.

// Package api is the public wire contract of executor attestation
// presentation (POST /api/v1/fleet/executors/attestation,
// docs/executor-attestation.md) and a Go client for it.
//
// An executor that will claim actions requiring attestation presents a
// statement an operator verifier signed for it. The server verifies it
// against its trust file and records it as the caller's latest attestation for
// that executor ref. The principal is never in the request: the server takes
// it from the authentication decision, so a caller can only attest itself.
//
// Every refusal — a bad signature, an unpinned image, a replayed or expired
// statement, a statement for another principal — is the same opaque answer:
// code "unauthorized", message "attestation refused". The reason is audited
// server-side and never returned.
//
// This package imports only the standard library, pkg/shoal and
// pkg/executorref (itself only the standard library), so an extension may
// depend on it (internal/importboundary enforces that).
package api

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/phrocker/shoal-oss/pkg/executorref"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

// Route is the presentation route. It is mounted under the authenticated
// fleet prefix and gated exactly as the other execute routes: the caller needs
// OperationExecute.
const Route = "/api/v1/fleet/executors/attestation"

// Limits the server applies, mirrored here; a parity test in
// pkg/explorer/webapi fails if they drift.
const (
	MaxExecutorRefBytes    = executorref.MaxExecutorRefBytes
	MaxIdempotencyKeyBytes = 1024
	MaxReportBytes         = 64 << 10
	MaxRequestBytes        = 128 << 10
	MaxResponseBytes       = 64 << 10
)

// RefusedMessage is the one message every verification refusal carries.
const RefusedMessage = "attestation refused"

// ErrRefused is returned by Client.Present for the opaque refusal.
var ErrRefused = errors.New("attestation refused")

// Request is the presentation body. IdempotencyKey is unpadded base64url of
// the key bytes the statement's nonce binds; Report is unpadded base64url of
// the signed statement envelope, byte for byte as the operator's signer
// produced it.
type Request struct {
	ExecutorRef    string `json:"executor_ref"`
	IdempotencyKey string `json:"idempotency_key"`
	Report         string `json:"report"`
}

// Receipt is the recorded attestation.
type Receipt struct {
	AttestationID string    `json:"attestation_id"`
	ExpiresAt     time.Time `json:"expires_at"`
}

// ErrorResponse is the body of every non-2xx answer.
type ErrorResponse struct {
	Code          shoal.ErrorCode `json:"code"`
	Message       string          `json:"message"`
	Indeterminate bool            `json:"indeterminate,omitempty"`
}

// Encode renders bytes as the wire spells them.
func Encode(value []byte) string { return base64.RawURLEncoding.EncodeToString(value) }

// Decode parses a non-empty unpadded base64url wire value.
func Decode(value string) ([]byte, error) {
	if value == "" {
		return nil, errors.New("is required")
	}
	decoded, err := base64.RawURLEncoding.Strict().DecodeString(value)
	if err != nil {
		return nil, errors.New("must be unpadded base64url")
	}
	if len(decoded) == 0 {
		return nil, errors.New("is required")
	}
	return decoded, nil
}

// NewRequest builds a request, checking it against the limits.
func NewRequest(executorRef string, idempotencyKey, report []byte) (Request, error) {
	if err := executorref.ValidExecutorRef(executorRef); err != nil {
		return Request{}, err
	}
	if len(idempotencyKey) == 0 || len(idempotencyKey) > MaxIdempotencyKeyBytes {
		return Request{}, errors.New("idempotency key is outside its bound")
	}
	if len(report) == 0 || len(report) > MaxReportBytes {
		return Request{}, errors.New("report is outside its bound")
	}
	return Request{ExecutorRef: executorRef, IdempotencyKey: Encode(idempotencyKey), Report: Encode(report)}, nil
}

// Config configures a Client. BaseURL is the plane's root (no path), as for
// the other public clients; the redirect policy and cookie jar of HTTPClient
// are replaced so the bearer token is the only credential presented.
type Config struct {
	BaseURL    string
	HTTPClient *http.Client
	Token      func(context.Context) (string, error)
}

// Client presents executor attestations.
type Client struct {
	base  string
	http  *http.Client
	token func(context.Context) (string, error)
}

// NewClient validates c and returns a client.
func NewClient(c Config) (*Client, error) {
	u, err := url.Parse(c.BaseURL)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") ||
		u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" ||
		u.RawPath != "" || (u.Path != "" && u.Path != "/") || c.Token == nil {
		return nil, errors.New("invalid attestation client configuration")
	}
	client := http.Client{}
	if c.HTTPClient != nil {
		client = *c.HTTPClient
	}
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	client.Jar = nil
	return &Client{base: strings.TrimSuffix(c.BaseURL, "/"), http: &client, token: c.Token}, nil
}

// HTTPError is a non-200 answer other than the opaque refusal.
type HTTPError struct {
	Status        int
	Code          string
	Message       string
	Indeterminate bool
}

func (e *HTTPError) Error() string {
	return fmt.Sprintf("attestation API %s (HTTP %d): %s", e.Code, e.Status, e.Message)
}

// Present presents report for executorRef under idempotencyKey. The statement
// inside report must carry the nonce for this caller, ref and key. Presenting
// the same statement again is idempotent. It returns ErrRefused for the opaque
// refusal, an *HTTPError for any other non-200 answer, or a transport error.
func (c *Client) Present(ctx context.Context, executorRef string, idempotencyKey, report []byte) (Receipt, error) {
	if c == nil {
		return Receipt{}, errors.New("nil attestation client")
	}
	body, err := NewRequest(executorRef, idempotencyKey, report)
	if err != nil {
		return Receipt{}, err
	}
	encoded, err := json.Marshal(body)
	if err != nil {
		return Receipt{}, err
	}
	token, err := c.token(ctx)
	if err != nil {
		return Receipt{}, err
	}
	if token == "" || strings.ContainsAny(token, "\r\n") {
		return Receipt{}, errors.New("invalid bearer token")
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+Route, bytes.NewReader(encoded))
	if err != nil {
		return Receipt{}, err
	}
	request.Header.Set("Authorization", "Bearer "+token)
	request.Header.Set("Content-Type", "application/json")
	response, err := c.http.Do(request)
	if err != nil {
		return Receipt{}, err
	}
	defer response.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(response.Body, MaxResponseBytes+1))
	if err != nil || len(raw) > MaxResponseBytes {
		return Receipt{}, &HTTPError{Status: response.StatusCode, Code: "response_error", Message: "response could not be read"}
	}
	if response.StatusCode != http.StatusOK {
		var detail ErrorResponse
		failure := &HTTPError{Status: response.StatusCode,
			Indeterminate: response.Header.Get("Shoal-Commit-Outcome") == "indeterminate"}
		if decodeStrict(raw, &detail) == nil {
			failure.Code, failure.Message = string(detail.Code), detail.Message
			failure.Indeterminate = failure.Indeterminate || detail.Indeterminate
		}
		if response.StatusCode == http.StatusUnauthorized &&
			detail.Code == shoal.ErrorUnauthorized &&
			strings.HasSuffix(detail.Message, RefusedMessage) {
			return Receipt{}, ErrRefused
		}
		return Receipt{}, failure
	}
	var receipt Receipt
	if err := decodeStrict(raw, &receipt); err != nil || receipt.AttestationID == "" || receipt.ExpiresAt.IsZero() {
		return Receipt{}, &HTTPError{Status: response.StatusCode, Code: "protocol_error", Message: "invalid response"}
	}
	return receipt, nil
}

// decodeStrict decodes one JSON value, refusing duplicate keys at the top
// level and trailing data. Unknown fields are ignored, so a newer server may
// say more than this client understands.
func decodeStrict(raw []byte, out any) error {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	token, err := decoder.Token()
	if err != nil {
		return err
	}
	if delimiter, ok := token.(json.Delim); !ok || delimiter != '{' {
		return errors.New("response is not a JSON object")
	}
	seen := map[string]bool{}
	for decoder.More() {
		key, err := decoder.Token()
		if err != nil {
			return err
		}
		name, _ := key.(string)
		folded := strings.ToLower(name)
		if seen[folded] {
			return errors.New("response repeats a key")
		}
		seen[folded] = true
		var skip json.RawMessage
		if err := decoder.Decode(&skip); err != nil {
			return err
		}
	}
	if _, err := decoder.Token(); err != nil {
		return err
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		return errors.New("response carries data after the JSON value")
	}
	return json.Unmarshal(raw, out)
}
