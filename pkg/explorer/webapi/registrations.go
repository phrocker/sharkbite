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

const RegistrationsRoute = "/api/v1/decision-registrations"

// NewRegistrationHTTPHandler requires trusted authentication and a provider enforcing
// current prediction, evidence, and registration rights. This handler grants no authority.
func NewRegistrationHTTPHandler(provider decisionapi.RegistrationProvider, resolver auth.Resolver) (http.Handler, error) {
	if isAbsentInterface(provider) || isAbsentInterface(resolver) {
		return nil, shoal.NewError(shoal.ErrorInvalidArgument, "registration provider and trusted resolver required")
	}
	return &registrationHTTP{provider: provider, resolver: resolver}, nil
}
func (h *Handler) MountRegistrations(provider decisionapi.RegistrationProvider, resolver auth.Resolver) error {
	handler, err := NewRegistrationHTTPHandler(provider, resolver)
	if err != nil {
		return err
	}
	if h == nil || h.mux == nil || isAbsentInterface(h.authenticator) || isAbsentInterface(h.binder) {
		return shoal.NewError(shoal.ErrorInvalidArgument, "registrations require authenticated transport")
	}
	for _, method := range []string{http.MethodPost, http.MethodGet, http.MethodDelete, http.MethodHead} {
		if exactMuxPatternRegistered(h.mux, method, RegistrationsRoute) {
			return authenticatedMountConflict()
		}
	}
	if err = h.MountAuthenticated(RegistrationsRoute+"/", handler); err != nil {
		return err
	}
	for _, method := range []string{http.MethodPost, http.MethodGet, http.MethodDelete} {
		h.mux.Handle(method+" "+RegistrationsRoute, handler)
	}
	h.enableDecisionCapability(CapabilityRegistrations)
	return nil
}

type registrationHTTP struct {
	provider decisionapi.RegistrationProvider
	resolver auth.Resolver
}

func registrationHeaderValues(r *http.Request, name string) ([]string, bool) {
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
func invalidRegistrationWire() error {
	return shoal.NewError(shoal.ErrorInvalidArgument, "invalid registration request")
}
func registrationPath(r *http.Request) (shoal.ID, error) {
	if r.URL.RawPath != "" || r.URL.RawQuery != "" || r.URL.ForceQuery {
		return "", invalidRegistrationWire()
	}
	if r.URL.Path == RegistrationsRoute {
		return "", nil
	}
	if !strings.HasPrefix(r.URL.Path, RegistrationsRoute+"/") {
		return "", invalidRegistrationWire()
	}
	target, err := decisionapi.DecodeID(strings.TrimPrefix(r.URL.Path, RegistrationsRoute+"/"))
	if err != nil {
		return "", invalidRegistrationWire()
	}
	return target, nil
}
func (h *registrationHTTP) ValidatePreAuthentication(r *http.Request) int {
	origins, op := registrationHeaderValues(r, "Origin")
	sites, sp := registrationHeaderValues(r, "Sec-Fetch-Site")
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
	if _, present := registrationHeaderValues(r, "Cookie"); present {
		return http.StatusBadRequest
	}
	if _, err := registrationPath(r); err != nil {
		return http.StatusBadRequest
	}
	return 0
}
func registrationWireError(w http.ResponseWriter, err error, uncertain bool) {
	code, status, message := shoal.ErrorUnavailable, http.StatusServiceUnavailable, "registration service unavailable"
	switch {
	case shoal.IsErrorCode(err, shoal.ErrorInvalidArgument):
		code, status, message = shoal.ErrorInvalidArgument, http.StatusBadRequest, "invalid registration request"
	case shoal.IsErrorCode(err, shoal.ErrorUnauthorized):
		code, status, message = shoal.ErrorUnauthorized, http.StatusUnauthorized, "authentication required"
	case shoal.IsErrorCode(err, shoal.ErrorNotFound):
		code, status, message = shoal.ErrorNotFound, http.StatusNotFound, "registration not found"
	case shoal.IsErrorCode(err, shoal.ErrorConflict):
		code, status, message = shoal.ErrorConflict, http.StatusConflict, "registration conflict"
	}
	if uncertain {
		code, status, message = shoal.ErrorUnavailable, http.StatusServiceUnavailable, "registration requires reconciliation"
		w.Header().Set(CommitOutcomeHeader, CommitOutcomeIndeterminate)
	}
	writeResponse(w, status, struct {
		Code          shoal.ErrorCode `json:"code"`
		Message       string          `json:"message"`
		Indeterminate bool            `json:"indeterminate"`
	}{code, message, uncertain})
}
func (h *registrationHTTP) resolve(ctx context.Context) (auth.Fingerprint, error) {
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
func decodeRegistrationWire(r *http.Request) (decisionapi.RegistrationSelection, shoal.ID, []byte, error) {
	var zero decisionapi.RegistrationSelection
	target, err := registrationPath(r)
	if err != nil {
		return zero, "", nil, err
	}
	if _, present := registrationHeaderValues(r, "Content-Encoding"); present {
		return zero, "", nil, invalidRegistrationWire()
	}
	if r.Method == http.MethodGet {
		if _, present := registrationHeaderValues(r, "Idempotency-Key"); present {
			return zero, "", nil, invalidRegistrationWire()
		}
		if target == "" || r.ContentLength > 0 {
			return zero, "", nil, invalidRegistrationWire()
		}
		if r.Body != nil {
			b, e := io.ReadAll(io.LimitReader(r.Body, 1))
			if e != nil || len(b) != 0 {
				return zero, "", nil, invalidRegistrationWire()
			}
		}
		return zero, target, nil, nil
	}
	if r.Method != http.MethodPost || r.URL.Path != RegistrationsRoute || r.Body == nil || r.ContentLength > decisionapi.MaxRegistrationRequestBytes {
		return zero, "", nil, invalidRegistrationWire()
	}
	keyString, err := decisionHeader(r, "Idempotency-Key")
	if err != nil {
		return zero, "", nil, invalidRegistrationWire()
	}
	key, err := decisionapi.DecodeKey(keyString)
	if err != nil {
		return zero, "", nil, invalidRegistrationWire()
	}
	contentType, err := decisionHeader(r, "Content-Type")
	if err != nil {
		return zero, "", nil, invalidRegistrationWire()
	}
	kind, params, err := mime.ParseMediaType(contentType)
	if err != nil || kind != "application/json" {
		return zero, "", nil, invalidRegistrationWire()
	}
	for name, value := range params {
		if name != "charset" || !strings.EqualFold(value, "utf-8") {
			return zero, "", nil, invalidRegistrationWire()
		}
	}
	raw, err := io.ReadAll(io.LimitReader(r.Body, decisionapi.MaxRegistrationRequestBytes+1))
	if err != nil || len(raw) > decisionapi.MaxRegistrationRequestBytes {
		return zero, "", nil, invalidRegistrationWire()
	}
	proposal, err := decisionapi.DecodeRegistrationRequest(raw)
	if err != nil {
		return zero, "", nil, invalidRegistrationWire()
	}
	proposal, err = decisionapi.NormalizeRegistrationSelection(proposal)
	if err != nil {
		return zero, "", nil, invalidRegistrationWire()
	}
	return proposal, "", key, nil
}
func (h *registrationHTTP) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if status := h.ValidatePreAuthentication(r); status != 0 {
		http.Error(w, http.StatusText(status), status)
		return
	}
	before, err := h.resolve(r.Context())
	if err != nil {
		registrationWireError(w, err, false)
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
	proposal, target, key, err := decodeRegistrationWire(r)
	if err != nil {
		registrationWireError(w, err, false)
		return
	}
	// Request decoding can block. Recheck the actual caller immediately before
	// invoking the provider, as well as after its work and after encoding.
	current, err := h.resolve(r.Context())
	if err != nil || current != before {
		registrationWireError(w, authenticationDenied(), false)
		return
	}
	// Keep a detached normalized copy before giving the provider mutable slices.
	expected, err := decisionapi.NormalizeRegistrationSelection(proposal)
	appendInvoked := r.Method == http.MethodPost
	var receipt decisionapi.RegistrationReceipt
	if appendInvoked {
		if err != nil {
			registrationWireError(w, invalidRegistrationWire(), false)
			return
		}
		receipt, err = h.provider.Register(r.Context(), proposal, key)
	} else {
		receipt, err = h.provider.Read(r.Context(), target)
	}
	current, authErr := h.resolve(r.Context())
	if authErr != nil || current != before {
		registrationWireError(w, authenticationDenied(), appendInvoked)
		return
	}
	if err != nil {
		registrationWireError(w, err, appendInvoked)
		return
	}
	var encoded []byte
	limit := decisionapi.MaxRegistrationResponseBytes
	if appendInvoked {
		if !decisionapi.MatchRegistrationSelection(expected, receipt) {
			registrationWireError(w, invalidRegistrationWire(), true)
			return
		}
		encoded, err = decisionapi.EncodeRegistrationReceipt(receipt)
	} else {
		if receipt.Snapshot.RequestID != target {
			registrationWireError(w, shoal.NewError(shoal.ErrorUnavailable, "invalid registration response"), false)
			return
		}
		encoded, err = decisionapi.EncodeRegistrationReceipt(receipt)
	}
	if err != nil || uint64(len(encoded))+1 > responseLimitFor(w) || len(encoded) > limit {
		registrationWireError(w, shoal.NewError(shoal.ErrorUnavailable, "invalid registration response"), appendInvoked)
		return
	}
	current, authErr = h.resolve(r.Context())
	if authErr != nil || current != before {
		registrationWireError(w, authenticationDenied(), appendInvoked)
		return
	}
	writeResponse(w, http.StatusOK, json.RawMessage(encoded))
}
