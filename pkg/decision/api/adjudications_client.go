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

// Adjudicate submits a proposal to the host's registered authority. Reconcile an
// uncertain write with the original proposal and key, including its original
// expected predecessor/version. Never rewrite it to the newest head on retry.
func (c *Client) Adjudicate(ctx context.Context, p AdjudicationProposal, key []byte) (AdjudicationReceipt, error) {
	p, e := NormalizeAdjudicationProposal(p)
	if e != nil {
		return AdjudicationReceipt{}, e
	}
	if len(key) == 0 || len(key) > shoal.MaxIDBytes {
		return AdjudicationReceipt{}, invalidAdjudication()
	}
	if _, e = DecodeKey(EncodeKey(key)); e != nil {
		return AdjudicationReceipt{}, e
	}
	body, e := EncodeAdjudicationRequest(p)
	if e != nil {
		return AdjudicationReceipt{}, e
	}
	raw, e := c.callAdjudication(ctx, http.MethodPost, "", key, body)
	if e != nil {
		return AdjudicationReceipt{}, e
	}
	r, e := DecodeAdjudicationReceipt(raw)
	if e != nil || !MatchAdjudicationProposal(p, r.Proposal) {
		return AdjudicationReceipt{}, &HTTPError{Status: 200, Code: "protocol_error", Message: "invalid adjudication receipt binding", Indeterminate: true, cause: e}
	}
	return r, nil
}

// AdjudicationHistory reads the complete currently authorized target history.
// This read neither reconciles a timed-out write nor guarantees no later append.
func (c *Client) AdjudicationHistory(ctx context.Context, target shoal.ID) (AdjudicationHistory, error) {
	if !adjudicationHashID(target, "decision:adjudication-target:v1:") {
		return AdjudicationHistory{}, invalidAdjudication()
	}
	raw, e := c.callAdjudication(ctx, http.MethodGet, target, nil, nil)
	if e != nil {
		return AdjudicationHistory{}, e
	}
	h, e := DecodeAdjudicationHistory(raw)
	if e != nil || h.TargetID != target {
		return AdjudicationHistory{}, &HTTPError{Status: 200, Code: "protocol_error", Message: "invalid adjudication history binding", cause: e}
	}
	return h, nil
}
func (c *Client) callAdjudication(ctx context.Context, method string, target shoal.ID, key, body []byte) ([]byte, error) {
	if c == nil {
		return nil, fmt.Errorf("nil decision client")
	}
	if e := ctx.Err(); e != nil {
		return nil, e
	}
	token, e := c.token(ctx)
	if e != nil {
		return nil, e
	}
	if token == "" || strings.ContainsAny(token, "\r\n") {
		return nil, fmt.Errorf("invalid bearer token")
	}
	endpoint := c.base + "/api/v1/adjudications"
	limit := MaxAdjudicationResponseBytes
	if method == http.MethodGet {
		endpoint += "/" + EncodeID(target)
		limit = MaxAdjudicationHistoryBytes
	}
	req, e := http.NewRequestWithContext(ctx, method, endpoint, bytes.NewReader(body))
	if e != nil {
		return nil, e
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/json")
	if method == http.MethodPost {
		req.Header.Set("Idempotency-Key", EncodeKey(key))
		req.Header.Set("Content-Type", "application/json")
		req.GetBody = nil
	}
	uncertain := method == http.MethodPost
	failure := func(status int, code, message string, cause error) ([]byte, error) {
		return nil, &HTTPError{Status: status, Code: code, Message: message, Indeterminate: uncertain, cause: cause}
	}
	response, e := c.http.Do(req)
	if e != nil {
		return failure(0, "transport_error", "adjudication request failed", e)
	}
	defer response.Body.Close()
	raw, e := io.ReadAll(io.LimitReader(response.Body, int64(limit)+1))
	if e != nil || len(raw) > limit {
		return failure(response.StatusCode, "response_error", "adjudication response unreadable or exceeds bound", e)
	}
	kind, _, e := mime.ParseMediaType(response.Header.Get("Content-Type"))
	if e != nil || kind != "application/json" || response.Header.Get("Content-Encoding") != "" {
		return failure(response.StatusCode, "protocol_error", "invalid adjudication response encoding", nil)
	}
	if response.StatusCode != http.StatusOK {
		if len(raw) > 16*1024 || preflightAdjudication(raw) != nil {
			return failure(response.StatusCode, "protocol_error", "invalid adjudication error response", nil)
		}
		var detail ErrorResponse
		if e := decodeStrict(raw, &detail); e != nil || detail.Code == "" || detail.Message == "" {
			return failure(response.StatusCode, "protocol_error", "invalid adjudication error response", e)
		}
		uncertain = method == http.MethodPost && (detail.Indeterminate || response.Header.Get("Shoal-Commit-Outcome") == "indeterminate" || response.StatusCode >= 500 || response.StatusCode < 400)
		return failure(response.StatusCode, detail.Code, detail.Message, nil)
	}
	if response.Header.Get("Shoal-Commit-Outcome") != "" {
		return failure(response.StatusCode, "protocol_error", "unexpected adjudication commit state", nil)
	}
	return raw, nil
}
