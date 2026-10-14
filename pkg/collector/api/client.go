// Licensed under the Apache License, Version 2.0. See LICENSE and NOTICE.
package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/phrocker/shoal-oss/pkg/collector"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

type Config struct {
	BaseURL    string
	HTTPClient *http.Client
	Token      func(context.Context) (string, error)
}

// Client calls the collector routes. Like pkg/decision/api it never follows
// redirects and never uses a cookie jar, so the bearer token is the only
// authentication it presents.
type Client struct {
	base  string
	http  *http.Client
	token func(context.Context) (string, error)
}

// HTTPError exposes sanitized server errors and distinguishes uncertain writes.
type HTTPError struct {
	Status        int
	Code, Message string
	Indeterminate bool
	cause         error
}

func (e *HTTPError) Error() string {
	return fmt.Sprintf("collector API %s (HTTP %d): %s", e.Code, e.Status, e.Message)
}
func (e *HTTPError) Is(target error) bool {
	return (target == ErrIndeterminate && e.Indeterminate) || (target == ErrPermissionDenied && e.Code == CodePermissionDenied && !e.Indeterminate)
}
func (e *HTTPError) Unwrap() error { return e.cause }

func NewClient(c Config) (*Client, error) {
	u, e := url.Parse(c.BaseURL)
	if e != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") || u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || u.RawPath != "" || (u.Path != "" && u.Path != "/") || c.Token == nil {
		return nil, fmt.Errorf("invalid collector client configuration")
	}
	h := http.Client{}
	if c.HTTPClient != nil {
		h = *c.HTTPClient
	}
	h.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	h.Jar = nil
	return &Client{base: strings.TrimSuffix(c.BaseURL, "/"), http: &h, token: c.Token}, nil
}

// Enroll asks the server to enroll a provisioned collector. Requesting any
// authority policy ID that was not provisioned is refused with
// ErrPermissionDenied; it is never trimmed. key must be unique per enrollment
// attempt; retrying with the same key and request returns the original
// receipt. An attestation statement must carry collector.EnrollNonce for the
// same collector ID and key.
func (c *Client) Enroll(ctx context.Context, key []byte, r collector.EnrollRequest) (EnrollReceipt, error) {
	r, e := r.Canonical()
	if e != nil {
		return EnrollReceipt{}, e
	}
	if _, e = DecodeKey(EncodeKey(key)); e != nil {
		return EnrollReceipt{}, e
	}
	var out EnrollReceipt
	if e = c.call(ctx, http.MethodPost, EnrollRoute, key, EncodeEnroll(r), &out); e != nil {
		return EnrollReceipt{}, e
	}
	if out.CollectorID != EncodeID(r.CollectorID) {
		return EnrollReceipt{}, &HTTPError{Status: http.StatusOK, Code: "protocol_error", Message: "response identity mismatch", Indeterminate: true}
	}
	return out, nil
}

// SubmitArtifact records a raw artifact reference. Resubmitting the same
// reference returns the original receipt; a different reference under the
// same artifact ID conflicts.
func (c *Client) SubmitArtifact(ctx context.Context, collectorID shoal.ID, a collector.ArtifactRef) (ArtifactReceipt, error) {
	if e := a.Validate(); e != nil {
		return ArtifactReceipt{}, e
	}
	if _, e := DecodeID(EncodeID(collectorID)); e != nil {
		return ArtifactReceipt{}, e
	}
	var out ArtifactReceipt
	if e := c.call(ctx, http.MethodPost, ArtifactsRoute, nil, EncodeArtifact(collectorID, a), &out); e != nil {
		return ArtifactReceipt{}, e
	}
	if out.CollectorID != EncodeID(collectorID) || out.ArtifactID != EncodeID(a.ID) {
		return ArtifactReceipt{}, &HTTPError{Status: http.StatusOK, Code: "protocol_error", Message: "response identity mismatch", Indeterminate: true}
	}
	return out, nil
}

// SubmitObservation records one observation. Its ID is derived from its
// content, so resubmission is idempotent.
func (c *Client) SubmitObservation(ctx context.Context, o collector.Observation) (ObservationReceipt, error) {
	if e := o.Validate(); e != nil {
		return ObservationReceipt{}, e
	}
	var out ObservationReceipt
	if e := c.call(ctx, http.MethodPost, ObservationsRoute, nil, ObservationRequest{EncodeObservation(o)}, &out); e != nil {
		return ObservationReceipt{}, e
	}
	if out.ObservationID != EncodeID(o.ID()) {
		return ObservationReceipt{}, &HTTPError{Status: http.StatusOK, Code: "protocol_error", Message: "response identity mismatch", Indeterminate: true}
	}
	return out, nil
}

// ReadObservation returns a stored observation with its current status.
func (c *Client) ReadObservation(ctx context.Context, id shoal.ID) (ObservationRecord, error) {
	if !collector.ValidObservationID(id) {
		return ObservationRecord{}, fmt.Errorf("invalid observation ID")
	}
	var out ObservationRecord
	if e := c.call(ctx, http.MethodGet, ObservationsRoute+"/"+EncodeID(id), nil, nil, &out); e != nil {
		return ObservationRecord{}, e
	}
	if out.ObservationID != EncodeID(id) {
		return ObservationRecord{}, &HTTPError{Status: http.StatusOK, Code: "protocol_error", Message: "response identity mismatch"}
	}
	return out, nil
}

type validated interface{ Validate() error }

func (c *Client) call(ctx context.Context, method, path string, key []byte, in any, out validated) error {
	if c == nil {
		return fmt.Errorf("nil collector client")
	}
	if e := ctx.Err(); e != nil {
		return e
	}
	token, e := c.token(ctx)
	if e != nil {
		return e
	}
	if token == "" || strings.ContainsAny(token, "\r\n") {
		return fmt.Errorf("invalid bearer token")
	}
	var body io.Reader
	if in != nil {
		b, e := json.Marshal(in)
		if e != nil {
			return e
		}
		if len(b) > MaxRequestBytes {
			return fmt.Errorf("request exceeds byte limit")
		}
		body = bytes.NewReader(b)
	}
	req, e := http.NewRequestWithContext(ctx, method, c.base+path, body)
	if e != nil {
		return e
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/json")
	if key != nil {
		req.Header.Set("Idempotency-Key", EncodeKey(key))
	}
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
		req.GetBody = nil
	}
	uncertain := method == http.MethodPost
	failure := func(status int, code, message string, cause error) error {
		return &HTTPError{Status: status, Code: code, Message: message, Indeterminate: uncertain, cause: cause}
	}
	response, e := c.http.Do(req)
	if e != nil {
		return failure(0, "transport_error", "request failed", e)
	}
	defer response.Body.Close()
	raw, e := io.ReadAll(io.LimitReader(response.Body, MaxResponseBytes+1))
	if e != nil {
		return failure(response.StatusCode, "response_error", "response could not be read", e)
	}
	if len(raw) > MaxResponseBytes {
		return failure(response.StatusCode, "response_error", "response exceeds byte limit", nil)
	}
	if response.StatusCode != http.StatusOK {
		var detail ErrorResponse
		if e := decodeStrict(raw, &detail); e != nil || detail.Code == "" || detail.Message == "" {
			return failure(response.StatusCode, "protocol_error", "invalid error response", e)
		}
		// Collector writes report definite failures only before any durable
		// attempt; anything else is flagged by the server.
		uncertain = detail.Indeterminate || response.Header.Get("Shoal-Commit-Outcome") == "indeterminate" || (uncertain && response.StatusCode >= 500)
		return failure(response.StatusCode, detail.Code, detail.Message, nil)
	}
	if e := decodeStrict(raw, out); e != nil {
		return failure(response.StatusCode, "protocol_error", "invalid response", e)
	}
	if e := out.Validate(); e != nil {
		return failure(response.StatusCode, "protocol_error", "invalid response", e)
	}
	return nil
}
