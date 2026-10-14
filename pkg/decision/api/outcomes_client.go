// Licensed under the Apache License, Version 2.0. See LICENSE and NOTICE.
package api

import (
	"bytes"
	"context"
	"fmt"
	"github.com/phrocker/shoal-oss/pkg/shoal"
	"io"
	"mime"
	"net/http"
	"strings"
)

// AppendOutcome submits a proposed observation. On ErrIndeterminate, reconcile
// by retrying this same observation and key under the same principal. The
// client never automatically retries a write.
func (c *Client) AppendOutcome(ctx context.Context, o OutcomeObservation, key []byte) (OutcomeReceipt, error) {
	normalized, e := NormalizeOutcomeObservation(o)
	if e != nil {
		return OutcomeReceipt{}, e
	}
	body, e := EncodeOutcomeRequest(normalized)
	if e != nil {
		return OutcomeReceipt{}, e
	}
	return c.callOutcome(ctx, http.MethodPost, normalized.RequestID, normalized.PredictionID, key, body, &normalized)
}

// ReadOutcome looks up the original principal-scoped receipt. A successful read
// does not repair or prove completion of a pending inventory publication.
func (c *Client) ReadOutcome(ctx context.Context, request, prediction shoal.ID, key []byte) (OutcomeReceipt, error) {
	return c.callOutcome(ctx, http.MethodGet, request, prediction, key, nil, nil)
}
func (c *Client) callOutcome(ctx context.Context, method string, request, prediction shoal.ID, key, body []byte, expected *OutcomeObservation) (OutcomeReceipt, error) {
	var zero OutcomeReceipt
	if c == nil {
		return zero, fmt.Errorf("nil decision client")
	}
	if !outcomeID(request) || !outcomeID(prediction) {
		return zero, invalidOutcome()
	}
	if _, e := DecodeKey(EncodeKey(key)); e != nil {
		return zero, e
	}
	if e := ctx.Err(); e != nil {
		return zero, e
	}
	token, e := c.token(ctx)
	if e != nil {
		return zero, e
	}
	if token == "" || strings.ContainsAny(token, "\r\n") {
		return zero, fmt.Errorf("invalid bearer token")
	}
	endpoint := c.base + "/api/v1/outcomes"
	if method == http.MethodGet {
		endpoint += "/" + EncodeID(request) + "/" + EncodeID(prediction)
	}
	req, e := http.NewRequestWithContext(ctx, method, endpoint, bytes.NewReader(body))
	if e != nil {
		return zero, e
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Idempotency-Key", EncodeKey(key))
	req.Header.Set("Accept", "application/json")
	if method == http.MethodPost {
		req.Header.Set("Content-Type", "application/json")
		req.GetBody = nil
	}
	uncertain := method == http.MethodPost
	failure := func(status int, code, message string, cause error) (OutcomeReceipt, error) {
		return zero, &HTTPError{Status: status, Code: code, Message: message, Indeterminate: uncertain, cause: cause}
	}
	response, e := c.http.Do(req)
	if e != nil {
		return failure(0, "transport_error", "outcome request failed", e)
	}
	defer response.Body.Close()
	raw, e := io.ReadAll(io.LimitReader(response.Body, MaxOutcomeResponseBytes+1))
	if e != nil || len(raw) > MaxOutcomeResponseBytes {
		return failure(response.StatusCode, "response_error", "outcome response unreadable or exceeds bound", e)
	}
	contentType, _, mediaErr := mime.ParseMediaType(response.Header.Get("Content-Type"))
	if mediaErr != nil || contentType != "application/json" || response.Header.Get("Content-Encoding") != "" {
		return failure(response.StatusCode, "protocol_error", "invalid outcome response encoding", nil)
	}
	if response.StatusCode != http.StatusOK {
		var detail ErrorResponse
		if e = decodeStrict(raw, &detail); e != nil || detail.Code == "" || detail.Message == "" {
			return failure(response.StatusCode, "protocol_error", "invalid outcome error response", e)
		}
		uncertain = method == http.MethodPost && (detail.Indeterminate || response.Header.Get("Shoal-Commit-Outcome") == "indeterminate" || (response.StatusCode >= 500 || response.StatusCode < 400))
		return failure(response.StatusCode, detail.Code, detail.Message, nil)
	}
	r, e := DecodeOutcomeReceipt(raw)
	if e != nil {
		return failure(response.StatusCode, "protocol_error", "invalid outcome receipt", e)
	}
	if r.Observation.RequestID != request || r.Observation.PredictionID != prediction || (expected != nil && !MatchOutcomeObservation(*expected, r.Observation)) {
		return failure(response.StatusCode, "protocol_error", "outcome response binding mismatch", nil)
	}
	if response.Header.Get("Shoal-Commit-Outcome") != "" {
		return failure(response.StatusCode, "protocol_error", "unexpected outcome commit state", nil)
	}
	return r, nil
}
