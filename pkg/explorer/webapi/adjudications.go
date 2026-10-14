// Licensed under the Apache License, Version 2.0. See LICENSE and NOTICE.
package webapi

import (
	"context"
	"encoding/json"
	"io"
	"mime"
	"net/http"
	"strings"
	"time"

	decisionapi "github.com/phrocker/shoal-oss/pkg/decision/api"
	"github.com/phrocker/shoal-oss/pkg/explorer/auth"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

const AdjudicationsRoute = "/api/v1/adjudications"

// NewAdjudicationHTTPHandler requires trusted authentication and a provider enforcing
// current prediction, evidence, and adjudication rights. This handler grants no authority.
func NewAdjudicationHTTPHandler(provider decisionapi.AdjudicationProvider, resolver auth.Resolver) (http.Handler, error) {
	if isAbsentInterface(provider) || isAbsentInterface(resolver) {
		return nil, shoal.NewError(shoal.ErrorInvalidArgument, "adjudication provider and trusted resolver required")
	}
	return &adjudicationHTTP{provider: provider, resolver: resolver}, nil
}
func (h *Handler) MountAdjudications(provider decisionapi.AdjudicationProvider, resolver auth.Resolver) error {
	handler, err := NewAdjudicationHTTPHandler(provider, resolver)
	if err != nil {
		return err
	}
	if h == nil || h.mux == nil || isAbsentInterface(h.authenticator) || isAbsentInterface(h.binder) {
		return shoal.NewError(shoal.ErrorInvalidArgument, "adjudications require authenticated transport")
	}
	for _, method := range []string{http.MethodPost, http.MethodGet, http.MethodDelete, http.MethodHead} {
		if exactMuxPatternRegistered(h.mux, method, AdjudicationsRoute) {
			return authenticatedMountConflict()
		}
	}
	if err = h.MountAuthenticated(AdjudicationsRoute+"/", handler); err != nil {
		return err
	}
	for _, method := range []string{http.MethodPost, http.MethodGet, http.MethodDelete} {
		h.mux.Handle(method+" "+AdjudicationsRoute, handler)
	}
	h.enableDecisionCapability(CapabilityAdjudications)
	return nil
}

type adjudicationHTTP struct {
	provider decisionapi.AdjudicationProvider
	resolver auth.Resolver
}

func adjudicationHeaderValues(r *http.Request, name string) ([]string, bool) {
	var values []string
	present := false
	for k, v := range r.Header {
		if strings.EqualFold(k, name) {
			present = true
			values = append(values, v...)
		}
	}
	return values, present
}
func invalidAdjudicationWire() error {
	return shoal.NewError(shoal.ErrorInvalidArgument, "invalid adjudication request")
}
func adjudicationPath(r *http.Request) (shoal.ID, error) {
	if r.URL.RawPath != "" || r.URL.RawQuery != "" || r.URL.ForceQuery {
		return "", invalidAdjudicationWire()
	}
	if r.URL.Path == AdjudicationsRoute {
		return "", nil
	}
	if !strings.HasPrefix(r.URL.Path, AdjudicationsRoute+"/") {
		return "", invalidAdjudicationWire()
	}
	target, err := decisionapi.DecodeID(strings.TrimPrefix(r.URL.Path, AdjudicationsRoute+"/"))
	if err != nil {
		return "", invalidAdjudicationWire()
	}
	return target, nil
}
func (h *adjudicationHTTP) ValidatePreAuthentication(r *http.Request) int {
	origins, op := adjudicationHeaderValues(r, "Origin")
	sites, sp := adjudicationHeaderValues(r, "Sec-Fetch-Site")
	if (op && len(origins) != 1) || (sp && len(sites) != 1) {
		return http.StatusForbidden
	}
	if sp && sites[0] != "same-origin" && sites[0] != "none" {
		return http.StatusForbidden
	}
	if op {
		scheme := "http"
		if r.TLS != nil {
			scheme = "https"
		}
		if origins[0] != scheme+"://"+r.Host {
			return http.StatusForbidden
		}
	}
	if _, present := adjudicationHeaderValues(r, "Cookie"); present {
		return http.StatusBadRequest
	}
	if _, err := adjudicationPath(r); err != nil {
		return http.StatusBadRequest
	}
	return 0
}
func adjudicationWireError(w http.ResponseWriter, err error, uncertain bool) {
	code, status, message := shoal.ErrorUnavailable, http.StatusServiceUnavailable, "adjudication service unavailable"
	switch {
	case shoal.IsErrorCode(err, shoal.ErrorInvalidArgument):
		code, status, message = shoal.ErrorInvalidArgument, http.StatusBadRequest, "invalid adjudication request"
	case shoal.IsErrorCode(err, shoal.ErrorUnauthorized):
		code, status, message = shoal.ErrorUnauthorized, http.StatusUnauthorized, "authentication required"
	case shoal.IsErrorCode(err, shoal.ErrorNotFound):
		code, status, message = shoal.ErrorNotFound, http.StatusNotFound, "adjudication not found"
	case shoal.IsErrorCode(err, shoal.ErrorConflict):
		code, status, message = shoal.ErrorConflict, http.StatusConflict, "adjudication conflict"
	}
	if uncertain {
		code, status, message = shoal.ErrorUnavailable, http.StatusServiceUnavailable, "adjudication requires reconciliation"
		w.Header().Set(CommitOutcomeHeader, CommitOutcomeIndeterminate)
	}
	writeResponse(w, status, struct {
		Code          shoal.ErrorCode `json:"code"`
		Message       string          `json:"message"`
		Indeterminate bool            `json:"indeterminate"`
	}{code, message, uncertain})
}
func (h *adjudicationHTTP) resolve(ctx context.Context) (auth.Fingerprint, error) {
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
func decodeAdjudicationWire(r *http.Request) (decisionapi.AdjudicationProposal, shoal.ID, []byte, error) {
	var zero decisionapi.AdjudicationProposal
	target, err := adjudicationPath(r)
	if err != nil {
		return zero, "", nil, err
	}
	if _, present := adjudicationHeaderValues(r, "Content-Encoding"); present {
		return zero, "", nil, invalidAdjudicationWire()
	}
	if r.Method == http.MethodGet {
		if _, present := adjudicationHeaderValues(r, "Idempotency-Key"); present {
			return zero, "", nil, invalidAdjudicationWire()
		}
		if target == "" || r.ContentLength > 0 {
			return zero, "", nil, invalidAdjudicationWire()
		}
		if r.Body != nil {
			b, e := io.ReadAll(io.LimitReader(r.Body, 1))
			if e != nil || len(b) != 0 {
				return zero, "", nil, invalidAdjudicationWire()
			}
		}
		return zero, target, nil, nil
	}
	if r.Method != http.MethodPost || r.URL.Path != AdjudicationsRoute || r.Body == nil || r.ContentLength > decisionapi.MaxAdjudicationRequestBytes {
		return zero, "", nil, invalidAdjudicationWire()
	}
	keyString, err := decisionHeader(r, "Idempotency-Key")
	if err != nil {
		return zero, "", nil, invalidAdjudicationWire()
	}
	key, err := decisionapi.DecodeKey(keyString)
	if err != nil {
		return zero, "", nil, invalidAdjudicationWire()
	}
	contentType, err := decisionHeader(r, "Content-Type")
	if err != nil {
		return zero, "", nil, invalidAdjudicationWire()
	}
	kind, params, err := mime.ParseMediaType(contentType)
	if err != nil || kind != "application/json" {
		return zero, "", nil, invalidAdjudicationWire()
	}
	for name, value := range params {
		if name != "charset" || !strings.EqualFold(value, "utf-8") {
			return zero, "", nil, invalidAdjudicationWire()
		}
	}
	raw, err := io.ReadAll(io.LimitReader(r.Body, decisionapi.MaxAdjudicationRequestBytes+1))
	if err != nil || len(raw) > decisionapi.MaxAdjudicationRequestBytes {
		return zero, "", nil, invalidAdjudicationWire()
	}
	proposal, err := decisionapi.DecodeAdjudicationRequest(raw)
	if err != nil {
		return zero, "", nil, invalidAdjudicationWire()
	}
	proposal, err = decisionapi.NormalizeAdjudicationProposal(proposal)
	if err != nil {
		return zero, "", nil, invalidAdjudicationWire()
	}
	return proposal, "", key, nil
}
func (h *adjudicationHTTP) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if status := h.ValidatePreAuthentication(r); status != 0 {
		http.Error(w, http.StatusText(status), status)
		return
	}
	before, err := h.resolve(r.Context())
	if err != nil {
		adjudicationWireError(w, err, false)
		return
	}
	if r.Method != http.MethodPost && r.Method != http.MethodGet {
		w.Header().Set("Allow", "GET, POST")
		writeResponse(w, http.StatusMethodNotAllowed, struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		}{"invalid_argument", "method not allowed"})
		return
	}
	proposal, target, key, err := decodeAdjudicationWire(r)
	if err != nil {
		adjudicationWireError(w, err, false)
		return
	}
	// Request decoding can block. Recheck the actual caller immediately before
	// invoking the provider, as well as after its work and after encoding.
	current, err := h.resolve(r.Context())
	if err != nil || current != before {
		adjudicationWireError(w, authenticationDenied(), false)
		return
	}
	// Keep a detached normalized copy before giving the provider mutable slices.
	expected, err := decisionapi.NormalizeAdjudicationProposal(proposal)
	appendInvoked := r.Method == http.MethodPost
	var receipt decisionapi.AdjudicationReceipt
	var history decisionapi.AdjudicationHistory
	if appendInvoked {
		if err != nil {
			adjudicationWireError(w, invalidAdjudicationWire(), false)
			return
		}
		receipt, err = h.provider.Adjudicate(r.Context(), proposal, key)
	} else {
		history, err = h.provider.AdjudicationHistory(r.Context(), target)
	}
	current, authErr := h.resolve(r.Context())
	if authErr != nil || current != before {
		adjudicationWireError(w, authenticationDenied(), appendInvoked)
		return
	}
	if err != nil {
		adjudicationWireError(w, err, appendInvoked)
		return
	}
	var encoded []byte
	limit := decisionapi.MaxAdjudicationResponseBytes
	if appendInvoked {
		if !decisionapi.MatchAdjudicationProposal(receipt.Proposal, expected) {
			adjudicationWireError(w, invalidAdjudicationWire(), true)
			return
		}
		encoded, err = decisionapi.EncodeAdjudicationReceipt(receipt)
	} else {
		if history.TargetID != target {
			adjudicationWireError(w, shoal.NewError(shoal.ErrorUnavailable, "invalid adjudication response"), false)
			return
		}
		encoded, err = decisionapi.EncodeAdjudicationHistory(history)
		limit = decisionapi.MaxAdjudicationHistoryBytes
	}
	if err != nil || uint64(len(encoded))+1 > responseLimitFor(w) || len(encoded) > limit {
		adjudicationWireError(w, shoal.NewError(shoal.ErrorUnavailable, "invalid adjudication response"), appendInvoked)
		return
	}
	current, authErr = h.resolve(r.Context())
	if authErr != nil || current != before {
		adjudicationWireError(w, authenticationDenied(), appendInvoked)
		return
	}
	writeResponse(w, http.StatusOK, json.RawMessage(encoded))
}
