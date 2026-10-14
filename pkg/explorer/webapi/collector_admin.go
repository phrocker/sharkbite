// Licensed under the Apache License, Version 2.0. See LICENSE and NOTICE.
package webapi

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"

	collectorapi "github.com/phrocker/shoal-oss/pkg/collector/api"
	"github.com/phrocker/shoal-oss/pkg/explorer/auth"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

// NewCollectorAdminHTTPHandler serves the collector admin routes (#446 slice
// 5). The provider resolves the admin from the trusted context and owns every
// authorization decision, including the collector_admin operation; no request
// field grants authority.
func NewCollectorAdminHTTPHandler(provider collectorapi.AdminProvider, resolver auth.Resolver) (http.Handler, error) {
	if isAbsentInterface(provider) || isAbsentInterface(resolver) {
		return nil, shoal.NewError(shoal.ErrorInvalidArgument, "collector admin provider and trusted resolver required")
	}
	return &collectorAdminHTTP{collectorHTTP: collectorHTTP{resolver: resolver}, admin: provider}, nil
}

// MountCollectorAdmin mounts the collector admin routes behind the existing
// host, authentication and origin gates, under collectorapi.AdminRoute. It is
// separate from MountCollectors, so a host serves collectors without serving
// administration, and it changes none of the collector routes: without it,
// an admin path answers not found from the collector handler as before.
//
// No authenticator this repository ships mints collector_admin. A host that
// mounts these routes mints its own admin decisions, under the same operator
// trust as the Go provisioning API.
func (h *Handler) MountCollectorAdmin(provider collectorapi.AdminProvider, resolver auth.Resolver) error {
	handler, e := NewCollectorAdminHTTPHandler(provider, resolver)
	if e != nil {
		return e
	}
	if h == nil || h.mux == nil || isAbsentInterface(h.authenticator) || isAbsentInterface(h.binder) {
		return shoal.NewError(shoal.ErrorInvalidArgument, "collector admin requires authenticated transport")
	}
	return h.MountAuthenticated(collectorapi.AdminRoute+"/", handler)
}

type collectorAdminHTTP struct {
	collectorHTTP
	admin collectorapi.AdminProvider
}

func (h *collectorAdminHTTP) ServeHTTP(w http.ResponseWriter, r *http.Request) {
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
	case post && r.URL.Path == collectorapi.AdminProvisionRoute:
		var body collectorapi.ProvisionRequest
		if e = collectorBody(r, &body); e == nil {
			p, decodeErr := collectorapi.DecodeProvision(body)
			if e = decodeErr; e == nil {
				called = true
				result, e = h.admin.ProvisionCollector(r.Context(), p)
			}
		}
	case post && r.URL.Path == collectorapi.AdminRevokeRoute:
		var body collectorapi.RevokeRequest
		if e = collectorBody(r, &body); e == nil {
			id, generation, decodeErr := collectorapi.DecodeRevoke(body)
			if e = decodeErr; e == nil {
				called = true
				result, e = h.admin.RevokeCollector(r.Context(), id, generation)
			}
		}
	case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, collectorapi.AdminRegistrationsRoute+"/"):
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
		id, decodeErr := collectorapi.DecodeID(strings.TrimPrefix(r.URL.Path, collectorapi.AdminRegistrationsRoute+"/"))
		if decodeErr != nil {
			e = invalidCollectorWire()
			break
		}
		called = true
		result, e = h.admin.ReadRegistration(r.Context(), id)
	case post || r.Method == http.MethodGet:
		collectorWireError(w, auth.ObjectNotFound(), false)
		return
	default:
		w.Header().Set("Allow", "GET, POST")
		writeResponse(w, http.StatusMethodNotAllowed, collectorapi.ErrorResponse{Code: string(shoal.ErrorInvalidArgument), Message: "method not allowed"})
		return
	}
	// Recheck even an error response: errors can disclose state after the
	// admin's authority was withdrawn.
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
