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
	"unicode/utf8"

	decisionapi "github.com/phrocker/shoal-oss/pkg/decision/api"
	"github.com/phrocker/shoal-oss/pkg/explorer/auth"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

const DecisionsRoute = "/api/v1/decisions"
const decisionBodyLimit = 4096

// NewDecisionHTTPHandler exposes only previously registered request identities.
// It requires the host's trusted resolver even when used outside MountDecisions.
// The provider must enforce task and every contributing source's CURRENT access;
// this transport's context checks are additional gates, not that authorization.
func NewDecisionHTTPHandler(provider decisionapi.Provider, resolver auth.Resolver) (http.Handler, error) {
	if isAbsentInterface(provider) || isAbsentInterface(resolver) {
		return nil, shoal.NewError(shoal.ErrorInvalidArgument, "decision provider and trusted resolver required")
	}
	return &decisionHTTP{provider: provider, resolver: resolver}, nil
}

// MountDecisions mounts both canonical collection and item paths behind the
// existing host/authentication/origin gates. Collection POST never redirects.
func (h *Handler) MountDecisions(provider decisionapi.Provider, resolver auth.Resolver) error {
	handler, e := NewDecisionHTTPHandler(provider, resolver)
	if e != nil {
		return e
	}
	if h == nil || h.mux == nil || isAbsentInterface(h.authenticator) || isAbsentInterface(h.binder) {
		return shoal.NewError(shoal.ErrorInvalidArgument, "decisions require authenticated transport")
	}
	for _, method := range []string{http.MethodPost, http.MethodGet, http.MethodDelete, http.MethodHead} {
		if exactMuxPatternRegistered(h.mux, method, DecisionsRoute) {
			return authenticatedMountConflict()
		}
	}
	if e = h.MountAuthenticated(DecisionsRoute+"/", handler); e != nil {
		return e
	}
	for _, method := range []string{http.MethodPost, http.MethodGet, http.MethodDelete} {
		h.mux.Handle(method+" "+DecisionsRoute, handler)
	}
	h.enableDecisionCapability(CapabilityDecisions)
	return nil
}

type decisionHTTP struct {
	provider decisionapi.Provider
	resolver auth.Resolver
}

// Cross-origin browser requests do not become a second authentication channel.
// Non-browser API clients may omit Origin and Fetch Metadata entirely.
func (h *decisionHTTP) ValidatePreAuthentication(r *http.Request) int {
	origins := r.Header.Values("Origin")
	sites := r.Header.Values("Sec-Fetch-Site")
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
	return 0
}

func decisionWireError(w http.ResponseWriter, e error, indeterminate bool) {
	code := shoal.ErrorUnavailable
	status := http.StatusServiceUnavailable
	message := "decision service unavailable"
	switch {
	case shoal.IsErrorCode(e, shoal.ErrorInvalidArgument):
		code, status, message = shoal.ErrorInvalidArgument, http.StatusBadRequest, "invalid decision request"
	case shoal.IsErrorCode(e, shoal.ErrorUnauthorized):
		code, status, message = shoal.ErrorUnauthorized, http.StatusUnauthorized, "authentication required"
	case shoal.IsErrorCode(e, shoal.ErrorNotFound):
		code, status, message = shoal.ErrorNotFound, http.StatusNotFound, "decision not found"
	case shoal.IsErrorCode(e, shoal.ErrorConflict):
		code, status, message = shoal.ErrorConflict, http.StatusConflict, "decision request conflict"
	}
	if indeterminate || errors.Is(e, decisionapi.ErrIndeterminate) {
		indeterminate = true
		status = http.StatusServiceUnavailable
		code = shoal.ErrorUnavailable
		message = "decision outcome requires reconciliation"
		w.Header().Set(CommitOutcomeHeader, CommitOutcomeIndeterminate)
	}
	writeResponse(w, status, struct {
		Code          shoal.ErrorCode `json:"code"`
		Message       string          `json:"message"`
		Indeterminate bool            `json:"indeterminate"`
	}{code, message, indeterminate})
}
func invalidDecisionWire() error {
	return shoal.NewError(shoal.ErrorInvalidArgument, "invalid decision request")
}
func decisionHeader(r *http.Request, name string) (string, error) {
	var values []string
	for key, v := range r.Header {
		if strings.EqualFold(key, name) {
			values = append(values, v...)
		}
	}
	if len(values) != 1 || values[0] == "" {
		return "", invalidDecisionWire()
	}
	return values[0], nil
}
func decodeDecisionRequest(r *http.Request) (shoal.ID, []byte, error) {
	if r.URL.RawQuery != "" || r.URL.ForceQuery || r.URL.RawPath != "" || len(r.Header.Values("Content-Encoding")) != 0 {
		return "", nil, invalidDecisionWire()
	}
	encodedKey, e := decisionHeader(r, "Idempotency-Key")
	if e != nil {
		return "", nil, e
	}
	key, e := decisionapi.DecodeKey(encodedKey)
	if e != nil {
		return "", nil, invalidDecisionWire()
	}
	var encodedID string
	switch r.Method {
	case http.MethodPost:
		if r.URL.Path != DecisionsRoute {
			return "", nil, invalidDecisionWire()
		}
		contentType, e := decisionHeader(r, "Content-Type")
		if e != nil {
			return "", nil, e
		}
		kind, params, e := mime.ParseMediaType(contentType)
		if e != nil || kind != "application/json" {
			return "", nil, invalidDecisionWire()
		}
		for name, value := range params {
			if name != "charset" || !strings.EqualFold(value, "utf-8") {
				return "", nil, invalidDecisionWire()
			}
		}
		if r.Body == nil {
			return "", nil, invalidDecisionWire()
		}
		raw, e := io.ReadAll(io.LimitReader(r.Body, decisionBodyLimit+1))
		if e != nil || len(raw) > decisionBodyLimit || !utf8.Valid(raw) {
			return "", nil, invalidDecisionWire()
		}
		d := json.NewDecoder(bytes.NewReader(raw))
		tok, e := d.Token()
		if e != nil || tok != json.Delim('{') {
			return "", nil, invalidDecisionWire()
		}
		tok, e = d.Token()
		if e != nil || tok != "request_id" {
			return "", nil, invalidDecisionWire()
		}
		tok, e = d.Token()
		if e != nil {
			return "", nil, invalidDecisionWire()
		}
		var ok bool
		encodedID, ok = tok.(string)
		if !ok || d.More() {
			return "", nil, invalidDecisionWire()
		}
		tok, e = d.Token()
		if e != nil || tok != json.Delim('}') {
			return "", nil, invalidDecisionWire()
		}
		if _, e = d.Token(); e != io.EOF {
			return "", nil, invalidDecisionWire()
		}
	case http.MethodGet:
		if !strings.HasPrefix(r.URL.Path, DecisionsRoute+"/") {
			return "", nil, invalidDecisionWire()
		}
		encodedID = strings.TrimPrefix(r.URL.Path, DecisionsRoute+"/")
		if r.Body != nil {
			raw, e := io.ReadAll(io.LimitReader(r.Body, 1))
			if e != nil || len(raw) != 0 {
				return "", nil, invalidDecisionWire()
			}
		}
	default:
		return "", nil, invalidDecisionWire()
	}
	id, e := decisionapi.DecodeID(encodedID)
	if e != nil {
		return "", nil, invalidDecisionWire()
	}
	return id, key, nil
}
func (h *decisionHTTP) resolve(ctx context.Context) (auth.Fingerprint, error) {
	if e := ctx.Err(); e != nil {
		return auth.Fingerprint{}, e
	}
	d, e := h.resolver.Resolve(ctx)
	if e != nil {
		return auth.Fingerprint{}, authenticationDenied()
	}
	fp, e := auth.AuthorizationFingerprint(d)
	if e != nil {
		return auth.Fingerprint{}, authenticationDenied()
	}
	return fp, nil
}
func (h *decisionHTTP) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if status := h.ValidatePreAuthentication(r); status != 0 {
		http.Error(w, http.StatusText(status), status)
		return
	}
	before, e := h.resolve(r.Context())
	if e != nil {
		decisionWireError(w, authenticationDenied(), false)
		return
	}
	if r.Method != http.MethodGet && r.Method != http.MethodPost {
		w.Header().Set("Allow", "GET, POST")
		writeResponse(w, http.StatusMethodNotAllowed, struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		}{"invalid_argument", "method not allowed"})
		return
	}
	id, key, e := decodeDecisionRequest(r)
	if e != nil {
		decisionWireError(w, e, false)
		return
	}
	if e = r.Context().Err(); e != nil {
		decisionWireError(w, e, false)
		return
	}
	var result decisionapi.Response
	mutationPossible := r.Method == http.MethodPost
	if mutationPossible {
		result, e = h.provider.Evaluate(r.Context(), id, key)
	} else {
		result, e = h.provider.Read(r.Context(), id, key)
	}
	// Recheck even an error response: errors can disclose state after revocation.
	after, authErr := h.resolve(r.Context())
	if authErr != nil || after != before {
		decisionWireError(w, authenticationDenied(), mutationPossible)
		return
	}
	if e != nil {
		// The provider may commit and then lose source authorization or fail
		// another disclosure check. No provider error proves that Evaluate
		// made no durable change, even when the error looks like a 404/409.
		decisionWireError(w, e, mutationPossible)
		return
	}
	if e = result.Validate(); e != nil || result.Receipt.RequestID != decisionapi.EncodeID(id) {
		decisionWireError(w, errors.New("invalid provider response"), mutationPossible)
		return
	}
	// Marshal completely within the protocol bound before disclosing headers.
	var encoded limitedResponseBuffer
	encoded.limit = int64(min(uint64(decisionapi.MaxResponseBytes), responseLimitFor(w)))
	if e = json.NewEncoder(&encoded).Encode(result); e != nil {
		decisionWireError(w, e, mutationPossible)
		return
	}
	after, authErr = h.resolve(r.Context())
	if authErr != nil || after != before {
		decisionWireError(w, authenticationDenied(), mutationPossible)
		return
	}
	status := http.StatusOK
	if result.Receipt.State == "pending" {
		status = http.StatusAccepted
	}
	writeResponse(w, status, json.RawMessage(encoded.Bytes()))
}
