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

	collectorapi "github.com/phrocker/shoal-oss/pkg/collector/api"
	"github.com/phrocker/shoal-oss/pkg/explorer/auth"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

// NewCollectorHTTPHandler serves the collector routes. The provider resolves
// the caller from the trusted context and owns every authority decision; no
// request field grants authority. It requires the host's trusted resolver even
// when used outside MountCollectors.
func NewCollectorHTTPHandler(provider collectorapi.Provider, resolver auth.Resolver) (http.Handler, error) {
	if isAbsentInterface(provider) || isAbsentInterface(resolver) {
		return nil, shoal.NewError(shoal.ErrorInvalidArgument, "collector provider and trusted resolver required")
	}
	return &collectorHTTP{provider: provider, resolver: resolver}, nil
}

// MountCollectors mounts the collector routes behind the existing host,
// authentication and origin gates. It refuses a handler without
// authentication.
func (h *Handler) MountCollectors(provider collectorapi.Provider, resolver auth.Resolver) error {
	handler, e := NewCollectorHTTPHandler(provider, resolver)
	if e != nil {
		return e
	}
	if h == nil || h.mux == nil || isAbsentInterface(h.authenticator) || isAbsentInterface(h.binder) {
		return shoal.NewError(shoal.ErrorInvalidArgument, "collectors require authenticated transport")
	}
	return h.MountAuthenticated(collectorapi.Route+"/", handler)
}

type collectorHTTP struct {
	provider collectorapi.Provider
	resolver auth.Resolver
}

// Cross-origin browser requests do not become a second authentication channel.
func (h *collectorHTTP) ValidatePreAuthentication(r *http.Request) int {
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

func invalidCollectorWire() error {
	return shoal.NewError(shoal.ErrorInvalidArgument, "invalid collector request")
}

func collectorWireError(w http.ResponseWriter, e error, indeterminate bool) {
	code, status, message := string(shoal.ErrorUnavailable), http.StatusServiceUnavailable, "collector registry unavailable"
	switch {
	case errors.Is(e, collectorapi.ErrIndeterminate):
		indeterminate = true
	case errors.Is(e, collectorapi.ErrPermissionDenied):
		code, status, message = collectorapi.CodePermissionDenied, http.StatusForbidden, "collector permission denied"
	case shoal.IsErrorCode(e, shoal.ErrorInvalidArgument):
		code, status, message = string(shoal.ErrorInvalidArgument), http.StatusBadRequest, "invalid collector request"
	case shoal.IsErrorCode(e, shoal.ErrorUnauthorized):
		code, status, message = string(shoal.ErrorUnauthorized), http.StatusUnauthorized, "authentication required"
	case shoal.IsErrorCode(e, shoal.ErrorNotFound):
		code, status, message = string(shoal.ErrorNotFound), http.StatusNotFound, "collector object not found"
	case shoal.IsErrorCode(e, shoal.ErrorConflict):
		code, status, message = string(shoal.ErrorConflict), http.StatusConflict, "collector record conflict"
	}
	if indeterminate {
		code, status, message = string(shoal.ErrorUnavailable), http.StatusServiceUnavailable, "collector write requires reconciliation"
		w.Header().Set(CommitOutcomeHeader, CommitOutcomeIndeterminate)
	}
	writeResponse(w, status, collectorapi.ErrorResponse{Code: code, Message: message, Indeterminate: indeterminate})
}

func (h *collectorHTTP) fingerprint(ctx context.Context) (auth.Fingerprint, error) {
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

// collectorBody reads one bounded strict JSON body into out.
func collectorBody(r *http.Request, out any) error {
	if r.URL.RawQuery != "" || r.URL.ForceQuery || r.URL.RawPath != "" || len(r.Header.Values("Content-Encoding")) != 0 || r.Body == nil {
		return invalidCollectorWire()
	}
	contentType, e := decisionHeader(r, "Content-Type")
	if e != nil {
		return e
	}
	kind, params, e := mime.ParseMediaType(contentType)
	if e != nil || kind != "application/json" {
		return invalidCollectorWire()
	}
	for name, value := range params {
		if name != "charset" || !strings.EqualFold(value, "utf-8") {
			return invalidCollectorWire()
		}
	}
	raw, e := io.ReadAll(io.LimitReader(r.Body, collectorapi.MaxRequestBytes+1))
	if e != nil || len(raw) > collectorapi.MaxRequestBytes || !utf8.Valid(raw) {
		return invalidCollectorWire()
	}
	if collectorapi.DecodeStrict(raw, out) != nil {
		return invalidCollectorWire()
	}
	return nil
}

func (h *collectorHTTP) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if status := h.ValidatePreAuthentication(r); status != 0 {
		http.Error(w, http.StatusText(status), status)
		return
	}
	before, e := h.fingerprint(r.Context())
	if e != nil {
		collectorWireError(w, authenticationDenied(), false)
		return
	}
	post := r.Method == http.MethodPost
	var result any
	called := false
	switch {
	case post && r.URL.Path == collectorapi.EnrollRoute:
		var body collectorapi.EnrollRequest
		e = collectorBody(r, &body)
		var key []byte
		if e == nil {
			var encoded string
			if encoded, e = decisionHeader(r, "Idempotency-Key"); e == nil {
				if key, e = collectorapi.DecodeKey(encoded); e != nil {
					e = invalidCollectorWire()
				}
			}
		}
		if e == nil {
			req, decodeErr := collectorapi.DecodeEnroll(body)
			if e = decodeErr; e == nil {
				called = true
				result, e = h.provider.Enroll(r.Context(), key, req)
			}
		}
	case post && r.URL.Path == collectorapi.ArtifactsRoute:
		var body collectorapi.ArtifactRequest
		if e = collectorBody(r, &body); e == nil {
			id, ref, decodeErr := collectorapi.DecodeArtifact(body)
			if e = decodeErr; e == nil {
				called = true
				result, e = h.provider.SubmitArtifact(r.Context(), id, ref)
			}
		}
	case post && r.URL.Path == collectorapi.ObservationsRoute:
		var body collectorapi.ObservationRequest
		if e = collectorBody(r, &body); e == nil {
			o, decodeErr := collectorapi.DecodeObservation(body.Observation)
			if e = decodeErr; e == nil {
				called = true
				result, e = h.provider.SubmitObservation(r.Context(), o)
			}
		}
	case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, collectorapi.ObservationsRoute+"/"):
		if r.URL.RawQuery != "" || r.URL.ForceQuery || r.URL.RawPath != "" {
			e = invalidCollectorWire()
			break
		}
		if r.Body != nil {
			if raw, readErr := io.ReadAll(io.LimitReader(r.Body, 1)); readErr != nil || len(raw) != 0 {
				e = invalidCollectorWire()
				break
			}
		}
		id, decodeErr := collectorapi.DecodeID(strings.TrimPrefix(r.URL.Path, collectorapi.ObservationsRoute+"/"))
		if decodeErr != nil {
			e = invalidCollectorWire()
			break
		}
		called = true
		result, e = h.provider.ReadObservation(r.Context(), id)
	case post || r.Method == http.MethodGet:
		collectorWireError(w, auth.ObjectNotFound(), false)
		return
	default:
		w.Header().Set("Allow", "GET, POST")
		writeResponse(w, http.StatusMethodNotAllowed, collectorapi.ErrorResponse{Code: string(shoal.ErrorInvalidArgument), Message: "method not allowed"})
		return
	}
	// Recheck even an error response: errors can disclose state after revocation.
	after, authErr := h.fingerprint(r.Context())
	if authErr != nil || after != before {
		collectorWireError(w, authenticationDenied(), post && called)
		return
	}
	if e != nil {
		collectorWireError(w, e, false)
		return
	}
	var encoded bytes.Buffer
	if e = json.NewEncoder(&encoded).Encode(result); e != nil || encoded.Len() > collectorapi.MaxResponseBytes {
		collectorWireError(w, errors.New("invalid provider response"), post)
		return
	}
	writeResponse(w, http.StatusOK, json.RawMessage(encoded.Bytes()))
}
