// Licensed under the Apache License, Version 2.0. See LICENSE and NOTICE.
package webapi

import (
	"context"
	"encoding/json"
	"io"
	"mime"
	"net/http"
	"slices"
	"strings"
	"time"

	decisionapi "github.com/phrocker/shoal-oss/pkg/decision/api"
	"github.com/phrocker/shoal-oss/pkg/explorer/auth"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

const OutcomesRoute = "/api/v1/outcomes"

// NewOutcomeHTTPHandler requires trusted authentication and a provider enforcing
// current prediction/evidence/reporting rights. This handler grants no authority.
func NewOutcomeHTTPHandler(provider decisionapi.OutcomeProvider, resolver auth.Resolver) (http.Handler, error) {
	if isAbsentInterface(provider) || isAbsentInterface(resolver) {
		return nil, shoal.NewError(shoal.ErrorInvalidArgument, "outcome provider and trusted resolver required")
	}
	return &outcomeHTTP{provider: provider, resolver: resolver}, nil
}
func (h *Handler) MountOutcomes(provider decisionapi.OutcomeProvider, resolver auth.Resolver) error {
	handler, err := NewOutcomeHTTPHandler(provider, resolver)
	if err != nil {
		return err
	}
	if h == nil || h.mux == nil || isAbsentInterface(h.authenticator) || isAbsentInterface(h.binder) {
		return shoal.NewError(shoal.ErrorInvalidArgument, "outcomes require authenticated transport")
	}
	for _, method := range []string{http.MethodPost, http.MethodGet, http.MethodDelete, http.MethodHead} {
		if exactMuxPatternRegistered(h.mux, method, OutcomesRoute) {
			return authenticatedMountConflict()
		}
	}
	if err = h.MountAuthenticated(OutcomesRoute+"/", handler); err != nil {
		return err
	}
	for _, method := range []string{http.MethodPost, http.MethodGet, http.MethodDelete} {
		h.mux.Handle(method+" "+OutcomesRoute, handler)
	}
	h.enableDecisionCapability(CapabilityOutcomes)
	return nil
}

type outcomeHTTP struct {
	provider decisionapi.OutcomeProvider
	resolver auth.Resolver
}

func outcomeHeaderValues(r *http.Request, name string) ([]string, bool) {
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
func invalidOutcomeWire() error {
	return shoal.NewError(shoal.ErrorInvalidArgument, "invalid outcome request")
}
func outcomePath(r *http.Request) (shoal.ID, shoal.ID, error) {
	if r.URL.RawPath != "" || r.URL.RawQuery != "" || r.URL.ForceQuery {
		return "", "", invalidOutcomeWire()
	}
	if r.URL.Path == OutcomesRoute {
		return "", "", nil
	}
	if !strings.HasPrefix(r.URL.Path, OutcomesRoute+"/") {
		return "", "", invalidOutcomeWire()
	}
	parts := strings.Split(strings.TrimPrefix(r.URL.Path, OutcomesRoute+"/"), "/")
	if len(parts) != 2 {
		return "", "", invalidOutcomeWire()
	}
	request, e := decisionapi.DecodeID(parts[0])
	if e != nil {
		return "", "", invalidOutcomeWire()
	}
	prediction, e := decisionapi.DecodeID(parts[1])
	if e != nil {
		return "", "", invalidOutcomeWire()
	}
	return request, prediction, nil
}
func (h *outcomeHTTP) ValidatePreAuthentication(r *http.Request) int {
	origins, op := outcomeHeaderValues(r, "Origin")
	sites, sp := outcomeHeaderValues(r, "Sec-Fetch-Site")
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
	if _, present := outcomeHeaderValues(r, "Cookie"); present {
		return http.StatusBadRequest
	}
	if _, _, err := outcomePath(r); err != nil {
		return http.StatusBadRequest
	}
	return 0
}
func outcomeWireError(w http.ResponseWriter, err error, uncertain bool) {
	code, status, message := shoal.ErrorUnavailable, http.StatusServiceUnavailable, "outcome service unavailable"
	switch {
	case shoal.IsErrorCode(err, shoal.ErrorInvalidArgument):
		code, status, message = shoal.ErrorInvalidArgument, http.StatusBadRequest, "invalid outcome request"
	case shoal.IsErrorCode(err, shoal.ErrorUnauthorized):
		code, status, message = shoal.ErrorUnauthorized, http.StatusUnauthorized, "authentication required"
	case shoal.IsErrorCode(err, shoal.ErrorNotFound):
		code, status, message = shoal.ErrorNotFound, http.StatusNotFound, "outcome not found"
	case shoal.IsErrorCode(err, shoal.ErrorConflict):
		code, status, message = shoal.ErrorConflict, http.StatusConflict, "outcome conflict"
	}
	if uncertain {
		code, status, message = shoal.ErrorUnavailable, http.StatusServiceUnavailable, "outcome requires reconciliation"
		w.Header().Set(CommitOutcomeHeader, CommitOutcomeIndeterminate)
	}
	writeResponse(w, status, struct {
		Code          shoal.ErrorCode `json:"code"`
		Message       string          `json:"message"`
		Indeterminate bool            `json:"indeterminate"`
	}{code, message, uncertain})
}
func (h *outcomeHTTP) resolve(ctx context.Context) (auth.Fingerprint, error) {
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
func decodeOutcomeWire(r *http.Request) (decisionapi.OutcomeObservation, shoal.ID, shoal.ID, []byte, error) {
	var zero decisionapi.OutcomeObservation
	request, prediction, err := outcomePath(r)
	if err != nil {
		return zero, "", "", nil, err
	}
	if _, present := outcomeHeaderValues(r, "Content-Encoding"); present {
		return zero, "", "", nil, invalidOutcomeWire()
	}
	keyString, err := decisionHeader(r, "Idempotency-Key")
	if err != nil {
		return zero, "", "", nil, invalidOutcomeWire()
	}
	key, err := decisionapi.DecodeKey(keyString)
	if err != nil {
		return zero, "", "", nil, invalidOutcomeWire()
	}
	if r.Method == http.MethodGet {
		if request == "" || prediction == "" || r.ContentLength > 0 {
			return zero, "", "", nil, invalidOutcomeWire()
		}
		if r.Body != nil {
			b, e := io.ReadAll(io.LimitReader(r.Body, 1))
			if e != nil || len(b) != 0 {
				return zero, "", "", nil, invalidOutcomeWire()
			}
		}
		return zero, request, prediction, key, nil
	}
	if r.Method != http.MethodPost || r.URL.Path != OutcomesRoute || r.Body == nil || r.ContentLength > decisionapi.MaxOutcomeRequestBytes {
		return zero, "", "", nil, invalidOutcomeWire()
	}
	contentType, err := decisionHeader(r, "Content-Type")
	if err != nil {
		return zero, "", "", nil, invalidOutcomeWire()
	}
	kind, params, err := mime.ParseMediaType(contentType)
	if err != nil || kind != "application/json" {
		return zero, "", "", nil, invalidOutcomeWire()
	}
	for name, value := range params {
		if name != "charset" || !strings.EqualFold(value, "utf-8") {
			return zero, "", "", nil, invalidOutcomeWire()
		}
	}
	raw, err := io.ReadAll(io.LimitReader(r.Body, decisionapi.MaxOutcomeRequestBytes+1))
	if err != nil || len(raw) > decisionapi.MaxOutcomeRequestBytes {
		return zero, "", "", nil, invalidOutcomeWire()
	}
	observation, err := decisionapi.DecodeOutcomeRequest(raw)
	if err != nil {
		return zero, "", "", nil, invalidOutcomeWire()
	}
	observation, err = decisionapi.NormalizeOutcomeObservation(observation)
	if err != nil {
		return zero, "", "", nil, invalidOutcomeWire()
	}
	return observation, observation.RequestID, observation.PredictionID, key, nil
}
func (h *outcomeHTTP) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if status := h.ValidatePreAuthentication(r); status != 0 {
		http.Error(w, http.StatusText(status), status)
		return
	}
	before, err := h.resolve(r.Context())
	if err != nil {
		outcomeWireError(w, err, false)
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
	observation, request, prediction, key, err := decodeOutcomeWire(r)
	if err != nil {
		outcomeWireError(w, err, false)
		return
	}
	// Request decoding can block. Recheck the actual caller immediately before
	// invoking the provider, as well as after its work and after encoding.
	current, err := h.resolve(r.Context())
	if err != nil || current != before {
		outcomeWireError(w, authenticationDenied(), false)
		return
	}
	expected := observation
	expected.EvidenceIDs = slices.Clone(observation.EvidenceIDs)
	if observation.Truth != nil {
		truth := *observation.Truth
		expected.Truth = &truth
	}
	appendInvoked := r.Method == http.MethodPost
	var receipt decisionapi.OutcomeReceipt
	if appendInvoked {
		receipt, err = h.provider.AppendOutcome(r.Context(), observation, key)
	} else {
		receipt, err = h.provider.ReadOutcome(r.Context(), request, prediction, key)
	}
	current, authErr := h.resolve(r.Context())
	if authErr != nil || current != before {
		outcomeWireError(w, authenticationDenied(), appendInvoked)
		return
	}
	if err != nil {
		outcomeWireError(w, err, appendInvoked)
		return
	}
	if receipt.Observation.RequestID != request || receipt.Observation.PredictionID != prediction {
		outcomeWireError(w, shoal.NewError(shoal.ErrorUnavailable, "invalid outcome response"), appendInvoked)
		return
	}
	if appendInvoked && !decisionapi.MatchOutcomeObservation(receipt.Observation, expected) {
		outcomeWireError(w, invalidOutcomeWire(), true)
		return
	}
	encoded, err := decisionapi.EncodeOutcomeReceipt(receipt)
	if err != nil || uint64(len(encoded))+1 > responseLimitFor(w) || len(encoded) > decisionapi.MaxOutcomeResponseBytes {
		outcomeWireError(w, shoal.NewError(shoal.ErrorUnavailable, "invalid outcome response"), appendInvoked)
		return
	}
	current, authErr = h.resolve(r.Context())
	if authErr != nil || current != before {
		outcomeWireError(w, authenticationDenied(), appendInvoked)
		return
	}
	writeResponse(w, http.StatusOK, json.RawMessage(encoded))
}
