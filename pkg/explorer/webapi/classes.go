// Licensed under the Apache License, Version 2.0. See LICENSE and NOTICE.
package webapi

import (
	"context"
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

// DecisionClassesRoute serves the measured record of every decision class in
// a registered cohort over a declared observation window. It is a read over
// outcome and adjudication records that already exist; it writes nothing and
// promotes nothing.
const DecisionClassesRoute = "/api/v1/decision-classes"
const classRequestLimit = 4096

// NewDecisionClassHTTPHandler exposes class records. The provider must refuse
// a cohort the caller may not read in full, under the same authority as a
// dataset export; resolver checks do not replace that requirement.
func NewDecisionClassHTTPHandler(provider decisionapi.ClassRecordProvider, resolver auth.Resolver) (http.Handler, error) {
	if isAbsentInterface(provider) || isAbsentInterface(resolver) {
		return nil, shoal.NewError(shoal.ErrorInvalidArgument, "class record provider and trusted resolver required")
	}
	return &classHTTP{provider, resolver}, nil
}

// MountDecisionClasses adds the exact collection route behind the host's
// trusted authentication and origin gates and advertises the capability.
func (h *Handler) MountDecisionClasses(provider decisionapi.ClassRecordProvider, resolver auth.Resolver) error {
	handler, e := NewDecisionClassHTTPHandler(provider, resolver)
	if e != nil {
		return e
	}
	if e = h.MountAuthenticated(DecisionClassesRoute, handler); e != nil {
		return e
	}
	h.enableDecisionCapability(CapabilityClasses)
	return nil
}

type classHTTP struct {
	provider decisionapi.ClassRecordProvider
	resolver auth.Resolver
}

func (h *classHTTP) ValidatePreAuthentication(r *http.Request) int {
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
	if r.URL.Path != DecisionClassesRoute || r.URL.RawPath != "" || r.URL.RawQuery != "" || r.URL.ForceQuery {
		return http.StatusBadRequest
	}
	return 0
}

func classWireError(w http.ResponseWriter, e error) {
	code, status, message := shoal.ErrorUnavailable, http.StatusServiceUnavailable, "class record unavailable"
	switch {
	case shoal.IsErrorCode(e, shoal.ErrorInvalidArgument):
		code, status, message = shoal.ErrorInvalidArgument, http.StatusBadRequest, "invalid class record request"
	case shoal.IsErrorCode(e, shoal.ErrorUnauthorized):
		code, status, message = shoal.ErrorUnauthorized, http.StatusUnauthorized, "authentication required"
	case shoal.IsErrorCode(e, shoal.ErrorNotFound):
		code, status, message = shoal.ErrorNotFound, http.StatusNotFound, "class record not found"
	}
	writeResponse(w, status, struct {
		Code    shoal.ErrorCode `json:"code"`
		Message string          `json:"message"`
	}{code, message})
}
func invalidClassRequest() error {
	return shoal.NewError(shoal.ErrorInvalidArgument, "invalid class record request")
}
func decodeClassRequest(r *http.Request) (decisionapi.ClassRecordRequest, error) {
	var zero decisionapi.ClassRecordRequest
	if r.Method != http.MethodPost || r.URL.Path != DecisionClassesRoute || r.URL.RawPath != "" || r.URL.RawQuery != "" || r.URL.ForceQuery {
		return zero, invalidClassRequest()
	}
	for key := range r.Header {
		if strings.EqualFold(key, "Content-Encoding") {
			return zero, invalidClassRequest()
		}
	}
	contentType, e := decisionHeader(r, "Content-Type")
	if e != nil {
		return zero, invalidClassRequest()
	}
	kind, params, e := mime.ParseMediaType(contentType)
	if e != nil || kind != "application/json" {
		return zero, invalidClassRequest()
	}
	for key, value := range params {
		if key != "charset" || !strings.EqualFold(value, "utf-8") {
			return zero, invalidClassRequest()
		}
	}
	if r.Body == nil {
		return zero, invalidClassRequest()
	}
	raw, e := io.ReadAll(io.LimitReader(r.Body, classRequestLimit+1))
	if e != nil || len(raw) > classRequestLimit || !utf8.Valid(raw) {
		return zero, invalidClassRequest()
	}
	req, e := decisionapi.DecodeClassRecordRequest(raw)
	if e != nil {
		return zero, invalidClassRequest()
	}
	return req, nil
}
func (h *classHTTP) resolve(ctx context.Context) (auth.Fingerprint, error) {
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
func (h *classHTTP) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if status := h.ValidatePreAuthentication(r); status != 0 {
		http.Error(w, http.StatusText(status), status)
		return
	}
	before, e := h.resolve(r.Context())
	if e != nil {
		classWireError(w, e)
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
	req, e := decodeClassRequest(r)
	if e != nil {
		classWireError(w, e)
		return
	}
	if r.Context().Err() != nil {
		classWireError(w, authenticationDenied())
		return
	}
	result, e := h.provider.ClassRecords(r.Context(), req)
	after, authErr := h.resolve(r.Context())
	if authErr != nil || after != before {
		classWireError(w, authenticationDenied())
		return
	}
	if e != nil {
		classWireError(w, e)
		return
	}
	if result.CohortID != req.CohortID || !result.From.Equal(req.From) || (!req.To.IsZero() && !result.To.Equal(req.To)) {
		classWireError(w, errors.New("substituted provider cohort or window"))
		return
	}
	encoded, e := decisionapi.EncodeClassRecords(result)
	if e != nil || uint64(len(encoded)) > responseLimitFor(w) {
		classWireError(w, errors.New("invalid or oversized class record response"))
		return
	}
	after, authErr = h.resolve(r.Context())
	if authErr != nil || after != before {
		classWireError(w, authenticationDenied())
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(encoded)
}
