// Licensed under the Apache License, Version 2.0. See LICENSE and NOTICE.
package api

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"mime"
	"net/http"
	"strings"

	"github.com/phrocker/shoal-oss/pkg/shoal"
)

// Register submits a source-bound registration. A failed POST is indeterminate
// because the server may have durably accepted the request before replying.
func (c *Client) Register(ctx context.Context, selection RegistrationSelection, key []byte) (RegistrationReceipt, error) {
	var zero RegistrationReceipt
	body, err := EncodeRegistrationRequest(selection)
	if err != nil {
		return zero, err
	}
	if c == nil || len(key) == 0 {
		return zero, fmt.Errorf("registration client or idempotency key unavailable")
	}
	return c.callRegistration(ctx, http.MethodPost, "", key, body, &selection)
}

// ReadRegistration reconciles a registration without sending an idempotency
// key or request body. Reads never claim an uncertain write succeeded.
func (c *Client) ReadRegistration(ctx context.Context, requestID shoal.ID) (RegistrationReceipt, error) {
	if c == nil {
		return RegistrationReceipt{}, fmt.Errorf("nil decision client")
	}
	if !strings.HasPrefix(string(requestID), "decision-registration:") || !outcomeDigest(strings.TrimPrefix(string(requestID), "decision-registration:")) {
		return RegistrationReceipt{}, invalidRegistrationProtocol()
	}
	return c.callRegistration(ctx, http.MethodGet, EncodeID(requestID), nil, nil, nil)
}

func (c *Client) callRegistration(ctx context.Context, method, path string, key, body []byte, expected *RegistrationSelection) (RegistrationReceipt, error) {
	var zero RegistrationReceipt
	if err := ctx.Err(); err != nil {
		return zero, err
	}
	token, err := c.token(ctx)
	if err != nil || token == "" || strings.ContainsAny(token, "\r\n") {
		return zero, fmt.Errorf("invalid bearer token")
	}
	endpoint := c.base + "/api/v1/decision-registrations"
	if method == http.MethodGet {
		endpoint += "/" + path
	}
	req, err := http.NewRequestWithContext(ctx, method, endpoint, bytes.NewReader(body))
	if err != nil {
		return zero, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/json")
	if method == http.MethodPost {
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Idempotency-Key", EncodeKey(key))
	}
	response, err := c.http.Do(req)
	if err != nil {
		return zero, &HTTPError{Status: 0, Code: "transport_error", Message: "registration request failed", Indeterminate: method == http.MethodPost, cause: err}
	}
	defer response.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(response.Body, MaxRegistrationResponseBytes+1))
	if err != nil || len(raw) > MaxRegistrationResponseBytes {
		return zero, &HTTPError{Status: response.StatusCode, Code: "response_error", Message: "registration response unreadable or exceeds bound", Indeterminate: method == http.MethodPost, cause: err}
	}
	contentType, _, mediaErr := mime.ParseMediaType(response.Header.Get("Content-Type"))
	if mediaErr != nil || contentType != "application/json" || response.Header.Get("Content-Encoding") != "" {
		return zero, &HTTPError{Status: response.StatusCode, Code: "protocol_error", Message: "invalid registration response encoding", Indeterminate: method == http.MethodPost}
	}
	if response.StatusCode != http.StatusOK {
		var detail ErrorResponse
		if err := decodeStrict(raw, &detail); err != nil || detail.Code == "" || detail.Message == "" {
			return zero, &HTTPError{Status: response.StatusCode, Code: "protocol_error", Message: "invalid registration error response", Indeterminate: method == http.MethodPost, cause: err}
		}
		uncertain := method == http.MethodPost && (detail.Indeterminate || response.StatusCode >= 500)
		return zero, &HTTPError{Status: response.StatusCode, Code: detail.Code, Message: detail.Message, Indeterminate: uncertain}
	}
	receipt, err := DecodeRegistrationReceipt(raw)
	if err != nil || (expected != nil && !MatchRegistrationSelection(*expected, receipt)) {
		return zero, &HTTPError{Status: response.StatusCode, Code: "protocol_error", Message: "registration response binding mismatch", Indeterminate: method == http.MethodPost, cause: err}
	}
	return receipt, nil
}
