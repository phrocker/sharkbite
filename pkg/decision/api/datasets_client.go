// Licensed under the Apache License, Version 2.0. See LICENSE and NOTICE.
package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/phrocker/shoal-oss/pkg/shoal"
)

// ExportDataset retrieves a read-only authorized serialization. It does not
// launch a training job or imply an uncertain write when transport fails.
func (c *Client) ExportDataset(ctx context.Context, id shoal.ID) (DatasetExport, error) {
	var zero DatasetExport
	if c == nil || !datasetID(id) {
		return zero, datasetInvalid()
	}
	if err := ctx.Err(); err != nil {
		return zero, err
	}
	token, err := c.token(ctx)
	if err != nil {
		return zero, err
	}
	if token == "" || strings.ContainsAny(token, "\r\n") {
		return zero, fmt.Errorf("invalid bearer token")
	}
	body, _ := json.Marshal(struct {
		CohortID string `json:"cohort_id"`
	}{EncodeID(id)})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+"/api/v1/dataset-exports", bytes.NewReader(body))
	if err != nil {
		return zero, err
	}
	req.GetBody = nil
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/json")
	fail := func(status int, code, message string, cause error) (DatasetExport, error) {
		return zero, &HTTPError{Status: status, Code: code, Message: message, Indeterminate: false, cause: cause}
	}
	response, err := c.http.Do(req)
	if err != nil {
		return fail(0, "transport_error", "request failed", err)
	}
	defer response.Body.Close()
	limit := MaxDatasetResponseBytes
	if response.StatusCode != http.StatusOK {
		limit = 64 << 10
	}
	raw, err := io.ReadAll(io.LimitReader(response.Body, int64(limit)+1))
	if err != nil {
		return fail(response.StatusCode, "response_error", "response could not be read", err)
	}
	if len(raw) > limit {
		return fail(response.StatusCode, "response_error", "response exceeds byte limit", nil)
	}
	if response.StatusCode != http.StatusOK {
		var detail ErrorResponse
		if decodeStrict(raw, &detail) != nil || detail.Code == "" || detail.Message == "" {
			return fail(response.StatusCode, "protocol_error", "invalid error response", nil)
		}
		// Server uncertainty flags concern other APIs and cannot turn this read-only
		// endpoint into a submitted job or an indeterminate mutation.
		code := "server_error"
		switch detail.Code {
		case "invalid_argument", "not_found", "unauthorized", "forbidden", "unavailable", "conflict", "resource_exhausted":
			code = detail.Code
		}
		return fail(response.StatusCode, code, "dataset export failed", nil)
	}
	result, err := DecodeDatasetExport(raw)
	if err != nil {
		return fail(response.StatusCode, "protocol_error", "invalid dataset response", nil)
	}
	if result.CohortID != id {
		return fail(response.StatusCode, "protocol_error", "response cohort mismatch", nil)
	}
	if err = ctx.Err(); err != nil {
		return fail(response.StatusCode, "response_error", "request canceled", err)
	}
	return result, nil
}
