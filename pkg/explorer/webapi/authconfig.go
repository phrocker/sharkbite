// Licensed to the Apache Software Foundation (ASF) under one
// or more contributor license agreements. See the NOTICE file
// distributed with this work for additional information
// regarding copyright ownership. The ASF licenses this file
// to you under the Apache License, Version 2.0 (the
// "License"); you may not use this file except in compliance
// with the License. You may obtain a copy of the License at
//
//     https://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing,
// software distributed under the License is distributed on an
// "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY
// KIND, either express or implied. See the License for the
// specific language governing permissions and limitations
// under the License.

package webapi

import (
	"net"
	"net/http"
	"net/url"
	"path"
	"strconv"
	"strings"
)

// BrowserAuthConfig carries the non-secret parameters a browser needs to begin
// an OIDC Authorization Code + PKCE login against a configured identity
// provider. Every field is public OIDC metadata or a public-client parameter;
// there is deliberately no client secret here. The browser performs a
// public-client PKCE exchange and the server only validates the bearer token
// that results; it never issues one.
type BrowserAuthConfig struct {
	// TenantID is retained for source and wire compatibility with the former
	// provider-specific browser contract. New integrations should leave it
	// empty and use the endpoint fields.
	TenantID string
	// ClientID is the public application identifier the browser authenticates as.
	ClientID string
	// Scope is the space-delimited scope string the browser requests.
	Scope string
	// AuthorizationEndpoint is the exact OIDC authorization endpoint.
	AuthorizationEndpoint string
	// TokenEndpoint is the exact OIDC token endpoint.
	TokenEndpoint string
	// Authority is retained for compatibility. When endpoint fields are empty,
	// the former v2 authorization and token paths are derived from this base.
	Authority string
}

// AuthConfigResponse is the body of GET /api/v1/auth-config. It is unauthenticated
// and non-secret by construction: it discloses only what an interactive login
// needs and nothing about the corpus, the policy catalog, or any principal.
// When no interactive login is configured it reports Configured false and the
// browser renders no login UI at all, which is the correct state for -dev-auth.
type AuthConfigResponse struct {
	Configured            bool   `json:"configured"`
	TenantID              string `json:"tenant_id,omitempty"`
	ClientID              string `json:"client_id,omitempty"`
	Scope                 string `json:"scope,omitempty"`
	Authority             string `json:"authority,omitempty"`
	AuthorizationEndpoint string `json:"authorization_endpoint,omitempty"`
	TokenEndpoint         string `json:"token_endpoint,omitempty"`
}

// SetBrowserAuthConfig supplies the non-secret parameters a browser needs to
// begin an interactive login. It is optional: when unset (or set to nil) the
// auth-config endpoint reports login as unconfigured, the connect-src policy
// stays at its strict local default, and the UI renders no login control. That
// is the seam that keeps local development a single command.
func (h *Handler) SetBrowserAuthConfig(config *BrowserAuthConfig) {
	h.browserAuth = config
}

// authConfigEndpoint answers GET /api/v1/auth-config. It is on the
// unauthenticated allowlist (see publiclyReachable) because the browser must
// read it before it can hold any token.
func (h *Handler) authConfigEndpoint(writer http.ResponseWriter, _ *http.Request) {
	if h.browserAuth == nil {
		writeResponse(writer, http.StatusOK, AuthConfigResponse{Configured: false})
		return
	}
	authorizationEndpoint, tokenEndpoint := h.browserAuth.endpoints()
	writeResponse(writer, http.StatusOK, AuthConfigResponse{
		Configured:            true,
		TenantID:              h.browserAuth.TenantID,
		ClientID:              h.browserAuth.ClientID,
		Scope:                 h.browserAuth.Scope,
		Authority:             h.browserAuth.Authority,
		AuthorizationEndpoint: authorizationEndpoint,
		TokenEndpoint:         tokenEndpoint,
	})
}

func (c *BrowserAuthConfig) endpoints() (string, string) {
	if c == nil {
		return "", ""
	}
	authorizationEndpoint := strings.TrimSpace(c.AuthorizationEndpoint)
	tokenEndpoint := strings.TrimSpace(c.TokenEndpoint)
	authority := strings.TrimRight(strings.TrimSpace(c.Authority), "/")
	if authority != "" {
		if authorizationEndpoint == "" {
			authorizationEndpoint = authority + "/oauth2/v2.0/authorize"
		}
		if tokenEndpoint == "" {
			tokenEndpoint = authority + "/oauth2/v2.0/token"
		}
	}
	return authorizationEndpoint, tokenEndpoint
}

// connectSources is the connect-src value for the Content-Security-Policy. With
// no interactive login configured it is exactly 'self', byte-identical to the
// local development posture. When a login is configured, only the token
// endpoint origin is added because it is the sole cross-origin fetch; the
// authorization endpoint is reached by top-level navigation and does not need
// connect-src permission. A malformed or non-https token endpoint contributes
// nothing, failing closed to 'self'.
func (h *Handler) connectSources() string {
	if h.browserAuth == nil {
		return "'self'"
	}
	_, tokenEndpoint := h.browserAuth.endpoints()
	origin := endpointOrigin(tokenEndpoint)
	if origin == "" {
		return "'self'"
	}
	return "'self' " + origin
}

func endpointOrigin(endpoint string) string {
	parsed, err := url.Parse(strings.TrimSpace(endpoint))
	if err != nil || parsed.Scheme != "https" || parsed.Host == "" ||
		parsed.User != nil || strings.ContainsAny(
		parsed.Host, "*'\"; \t\r\n\\",
	) || !validEndpointHost(parsed.Hostname()) {
		return ""
	}
	if port := parsed.Port(); port != "" {
		number, err := strconv.Atoi(port)
		if err != nil || number < 1 || number > 65535 {
			return ""
		}
	}
	return parsed.Scheme + "://" + parsed.Host
}

func validEndpointHost(host string) bool {
	if net.ParseIP(host) != nil {
		return true
	}
	host = strings.TrimSuffix(host, ".")
	if host == "" || len(host) > 253 {
		return false
	}
	for _, label := range strings.Split(host, ".") {
		if len(label) == 0 || len(label) > 63 ||
			label[0] == '-' || label[len(label)-1] == '-' {
			return false
		}
		for _, character := range label {
			if (character < 'a' || character > 'z') &&
				(character < 'A' || character > 'Z') &&
				(character < '0' || character > '9') &&
				character != '-' {
				return false
			}
		}
	}
	return true
}

// publiclyReachable is the fail-closed allowlist of request lines the transport
// serves without a trusted decision. It is expressed as an allowlist consulted
// before authenticate, so a route that is not named here is authenticated by
// default: a contributor who adds a new /api/v1/* handler gets authentication
// for free and cannot make it public by omission.
//
// The match is exact on method+path for the two named routes and a single
// prefix for the immutable asset tree. The prefix is "/assets/" only, which can
// never overlap an "/api/v1/*" path, and the whole predicate is gated to GET,
// so no state-changing route is ever reachable unauthenticated. The request
// path is cleaned before matching, so a traversal such as
// /assets/../api/v1/documents is judged as the API route it resolves to, not as
// an asset, and remains authenticated.
//
// The path.Clean call is load-bearing and non-obvious, so it must not be
// removed. net/http hands ServeHTTP the raw, un-normalized request path, but the
// ServeMux that routes the request afterward cleans it. Without the Clean here
// the gate and the router disagree about which route a request is: the gate
// would see /assets/../api/v1/meta as an asset and skip authentication, then the
// mux would resolve it to the real /api/v1/meta handler. That disagreement is
// the vulnerability, and it is invisible from reading either the gate or the mux
// alone. Cleaning here makes the gate judge the same route the mux will serve.
// TestPubliclyReachableNormalizesTraversal and
// TestAuthGateAuthenticatesTraversalIntoAPIRoutes pin this in both directions.
//
// This does not bypass the host-authority gate, which runs first and
// unconditionally in ServeHTTP; it only decides whether authentication is
// required for a request the host gate has already admitted.
func (h *Handler) publiclyReachable(request *http.Request) bool {
	if request.Method != http.MethodGet {
		return false
	}
	cleaned := request.URL.Path
	if cleaned == "" {
		cleaned = "/"
	}
	cleaned = path.Clean(cleaned)
	switch cleaned {
	case "/", "/api/v1/auth-config":
		return true
	}
	return cleaned == "/assets" || strings.HasPrefix(cleaned, "/assets/")
}
