// Licensed under the Apache License, Version 2.0. See LICENSE and NOTICE.
package webapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	decisionapi "github.com/phrocker/shoal-oss/pkg/decision/api"
	"github.com/phrocker/shoal-oss/pkg/explorer/auth"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

const DatasetExportsRoute = "/api/v1/dataset-exports"
const datasetRequestLimit = 4096

// NewDatasetExportHTTPHandler exposes only registered cohort identities. The
// provider must independently enforce current all-source and training-purpose
// authorization plus complete inventory checks; resolver checks do not replace
// those requirements. Export is read-only and creates no training job.
func NewDatasetExportHTTPHandler(provider decisionapi.DatasetProvider, resolver auth.Resolver) (http.Handler, error) {
	if isAbsentInterface(provider) || isAbsentInterface(resolver) {
		return nil, shoal.NewError(shoal.ErrorInvalidArgument, "dataset provider and trusted resolver required")
	}
	return &datasetHTTP{provider, resolver}, nil
}

// MountDatasetExports adds the exact collection route behind the host's trusted
// authentication and origin gates. No trailing-slash redirect is registered.
func (h *Handler) MountDatasetExports(provider decisionapi.DatasetProvider, resolver auth.Resolver) error {
	handler, e := NewDatasetExportHTTPHandler(provider, resolver)
	if e != nil {
		return e
	}
	if e = h.MountAuthenticated(DatasetExportsRoute, handler); e != nil {
		return e
	}
	h.enableDecisionCapability(CapabilityDatasets)
	return nil
}

type datasetHTTP struct {
	provider decisionapi.DatasetProvider
	resolver auth.Resolver
}

func (h *datasetHTTP) ValidatePreAuthentication(r *http.Request) int {
	origins, sites := []string{}, []string{}
	for key, values := range r.Header {
		if strings.EqualFold(key, "Origin") {
			origins = append(origins, values...)
		}
		if strings.EqualFold(key, "Sec-Fetch-Site") {
			sites = append(sites, values...)
		}
	}
	if len(origins) > 1 || len(sites) > 1 {
		return http.StatusForbidden
	}
	if len(sites) == 1 && sites[0] != "same-origin" && sites[0] != "none" {
		return http.StatusForbidden
	}
	if len(origins) == 1 {
		scheme := "http"
		if r.TLS != nil {
			scheme = "https"
		}
		if origins[0] != scheme+"://"+r.Host {
			return http.StatusForbidden
		}
	}
	if r.URL.Path != DatasetExportsRoute || r.URL.RawPath != "" || r.URL.RawQuery != "" || r.URL.ForceQuery {
		return http.StatusBadRequest
	}
	return 0
}
func datasetWireError(w http.ResponseWriter, e error) {
	code, status, message := shoal.ErrorUnavailable, http.StatusServiceUnavailable, "dataset export unavailable"
	switch {
	case shoal.IsErrorCode(e, shoal.ErrorInvalidArgument):
		code, status, message = shoal.ErrorInvalidArgument, http.StatusBadRequest, "invalid dataset export request"
	case shoal.IsErrorCode(e, shoal.ErrorUnauthorized):
		code, status, message = shoal.ErrorUnauthorized, http.StatusUnauthorized, "authentication required"
	case shoal.IsErrorCode(e, shoal.ErrorNotFound):
		code, status, message = shoal.ErrorNotFound, http.StatusNotFound, "dataset export not found"
	}
	writeResponse(w, status, struct {
		Code    shoal.ErrorCode `json:"code"`
		Message string          `json:"message"`
	}{code, message})
}
func invalidDatasetRequest() error {
	return shoal.NewError(shoal.ErrorInvalidArgument, "invalid dataset export request")
}
func decodeDatasetRequest(r *http.Request) (shoal.ID, error) {
	if r.Method != http.MethodPost || r.URL.Path != DatasetExportsRoute || r.URL.RawPath != "" || r.URL.RawQuery != "" || r.URL.ForceQuery {
		return "", invalidDatasetRequest()
	}
	// Header maps constructed by embedded hosts may contain noncanonical keys.
	for key := range r.Header {
		if strings.EqualFold(key, "Content-Encoding") {
			return "", invalidDatasetRequest()
		}
	}
	contentType, e := decisionHeader(r, "Content-Type")
	if e != nil {
		return "", invalidDatasetRequest()
	}
	kind, params, e := mime.ParseMediaType(contentType)
	if e != nil || kind != "application/json" {
		return "", invalidDatasetRequest()
	}
	for key, value := range params {
		if key != "charset" || !strings.EqualFold(value, "utf-8") {
			return "", invalidDatasetRequest()
		}
	}
	if r.Body == nil {
		return "", invalidDatasetRequest()
	}
	raw, e := io.ReadAll(io.LimitReader(r.Body, datasetRequestLimit+1))
	if e != nil || len(raw) > datasetRequestLimit || !utf8.Valid(raw) {
		return "", invalidDatasetRequest()
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	token, e := d.Token()
	if e != nil || token != json.Delim('{') {
		return "", invalidDatasetRequest()
	}
	token, e = d.Token()
	if e != nil || token != "cohort_id" {
		return "", invalidDatasetRequest()
	}
	token, e = d.Token()
	if e != nil {
		return "", invalidDatasetRequest()
	}
	id, ok := token.(string)
	if !ok || d.More() {
		return "", invalidDatasetRequest()
	}
	token, e = d.Token()
	if e != nil || token != json.Delim('}') {
		return "", invalidDatasetRequest()
	}
	if _, e = d.Token(); e != io.EOF {
		return "", invalidDatasetRequest()
	}
	decoded, e := decisionapi.DecodeID(id)
	if e != nil {
		return "", invalidDatasetRequest()
	}
	return decoded, nil
}
func (h *datasetHTTP) resolve(ctx context.Context) (auth.Fingerprint, error) {
	if ctx.Err() != nil {
		return auth.Fingerprint{}, authenticationDenied()
	}
	d, e := h.resolver.Resolve(ctx)
	if e != nil || ctx.Err() != nil || !time.Now().Before(d.AuthenticationExpires()) {
		return auth.Fingerprint{}, authenticationDenied()
	}
	fp, e := auth.AuthorizationFingerprint(d)
	if e != nil {
		return auth.Fingerprint{}, authenticationDenied()
	}
	return fp, nil
}
func (h *datasetHTTP) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if status := h.ValidatePreAuthentication(r); status != 0 {
		http.Error(w, http.StatusText(status), status)
		return
	}
	before, e := h.resolve(r.Context())
	if e != nil {
		datasetWireError(w, e)
		return
	}
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		writeResponse(w, http.StatusMethodNotAllowed, struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		}{"invalid_argument", "method not allowed"})
		return
	}
	id, e := decodeDatasetRequest(r)
	if e != nil {
		datasetWireError(w, e)
		return
	}
	if r.Context().Err() != nil {
		datasetWireError(w, authenticationDenied())
		return
	}
	result, e := h.provider.Export(r.Context(), id)
	after, authErr := h.resolve(r.Context())
	if authErr != nil || after != before {
		datasetWireError(w, authenticationDenied())
		return
	}
	if e != nil {
		datasetWireError(w, e)
		return
	}
	if result.CohortID != id {
		datasetWireError(w, errors.New("substituted provider cohort"))
		return
	}
	encoded, e := decisionapi.EncodeDatasetExport(result)
	if e != nil || uint64(len(encoded)) > responseLimitFor(w) {
		datasetWireError(w, errors.New("invalid or oversized dataset response"))
		return
	}
	after, authErr = h.resolve(r.Context())
	if authErr != nil || after != before {
		datasetWireError(w, authenticationDenied())
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(encoded)
}
