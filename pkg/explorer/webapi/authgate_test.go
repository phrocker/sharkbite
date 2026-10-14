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

package webapi_test

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/phrocker/shoal-oss/pkg/explorer/webapi"
)

// gateGet issues a GET against the fixture server, optionally as the named test
// principal, and returns the full response so the caller can inspect headers as
// well as status. The caller closes the body.
func gateGet(t *testing.T, fixture *authnFixture, path, principal string) *http.Response {
	t.Helper()
	request, err := http.NewRequest(http.MethodGet, fixture.server.URL+path, nil)
	if err != nil {
		t.Fatal(err)
	}
	if principal != "" {
		request.Header.Set(authnPrincipalHeader, principal)
	}
	response, err := fixture.server.Client().Do(request)
	if err != nil {
		t.Fatal(err)
	}
	return response
}

func gatePost(t *testing.T, fixture *authnFixture, path, principal, body string) *http.Response {
	t.Helper()
	request, err := http.NewRequest(
		http.MethodPost, fixture.server.URL+path, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Content-Type", "application/json")
	if principal != "" {
		request.Header.Set(authnPrincipalHeader, principal)
	}
	response, err := fixture.server.Client().Do(request)
	if err != nil {
		t.Fatal(err)
	}
	return response
}

// TestAuthGateServesPublicRoutesWithoutAuthentication pins the fail-closed
// allowlist from the reachable direction: exactly the shell, its assets and the
// auth-config endpoint load without a token. Delete the allowlist check in
// ServeHTTP and these become 401.
func TestAuthGateServesPublicRoutesWithoutAuthentication(t *testing.T) {
	fixture := newAuthnFixture(t)
	for _, path := range []string{"/", "/assets/app.js", "/api/v1/auth-config"} {
		response := gateGet(t, fixture, path, "")
		func() {
			defer response.Body.Close()
			if response.StatusCode != http.StatusOK {
				t.Fatalf("public %s status = %d, want 200", path, response.StatusCode)
			}
		}()
	}
}

// TestAuthGateRequiresAuthenticationForAPIRoutes pins the allowlist from the
// protected direction. It includes a route that does not exist
// (/api/v1/does-not-exist) to prove authentication is the default for any new
// API route: the gate runs before the mux, so an unlisted path is refused with
// 401 rather than reaching a 404. The GET on /api/v1/documents is the specific
// prefix-regression guard the review calls for: were the allowlist a loose
// prefix, that path could leak as public; here it must stay 401.
func TestAuthGateRequiresAuthenticationForAPIRoutes(t *testing.T) {
	fixture := newAuthnFixture(t)
	cases := []struct {
		method, path string
	}{
		{http.MethodGet, "/api/v1/meta"},
		{http.MethodGet, "/api/v1/identity"},
		{http.MethodGet, "/api/v1/documents"},
		{http.MethodGet, "/api/v1/does-not-exist"},
		{http.MethodPost, "/api/v1/documents"},
		{http.MethodPost, "/api/v1/retrieve"},
	}
	for _, testCase := range cases {
		var response *http.Response
		if testCase.method == http.MethodGet {
			response = gateGet(t, fixture, testCase.path, "")
		} else {
			response = gatePost(t, fixture, testCase.path, "", "{}")
		}
		func() {
			defer response.Body.Close()
			if response.StatusCode != http.StatusUnauthorized {
				t.Fatalf("%s %s status = %d, want 401",
					testCase.method, testCase.path, response.StatusCode)
			}
			if got := response.Header.Get("WWW-Authenticate"); !strings.Contains(
				strings.ToLower(got), "bearer") {
				t.Fatalf("%s %s missing bearer challenge, got %q",
					testCase.method, testCase.path, got)
			}
		}()
	}
}

// TestAuthGateAuthenticatesTraversalIntoAPIRoutes is the end-to-end companion to
// TestPubliclyReachableNormalizesTraversal. net/http delivers the raw path to
// the gate, so a request that traverses out of /assets onto an API route must
// still be authenticated. The client is configured NOT to follow redirects, so
// the assertion is made on the FIRST response the gate produces. With the gate's
// path.Clean intact that first response is a 401 with a bearer challenge (the
// gate resolved the same API route the mux would). If path.Clean is lost, the
// gate treats the request as a public asset, skips authentication, and the mux
// then redirects the traversal to the real handler — so the first response stops
// being a 401 and this test fails. Asserting the specific 401 + bearer, rather
// than merely "not 200", keeps the reason precise: the request was refused
// because authentication is required, not for some unrelated reason.
func TestAuthGateAuthenticatesTraversalIntoAPIRoutes(t *testing.T) {
	fixture := newAuthnFixture(t)
	client := *fixture.server.Client()
	client.CheckRedirect = func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}
	targets := []string{
		"/assets/../api/v1/meta",
		"/assets/../api/v1/identity",
		"/assets/../../api/v1/meta",
		"/assets/%2e%2e/api/v1/meta",
	}
	for _, target := range targets {
		request, err := http.NewRequest(http.MethodGet, fixture.server.URL+target, nil)
		if err != nil {
			t.Fatal(err)
		}
		response, err := client.Do(request)
		if err != nil {
			t.Fatalf("GET %s: %v", target, err)
		}
		func() {
			defer response.Body.Close()
			if response.StatusCode != http.StatusUnauthorized {
				t.Fatalf("GET %s first-response status = %d, want 401 "+
					"(traversal must not be served as a public asset)",
					target, response.StatusCode)
			}
			if got := response.Header.Get("WWW-Authenticate"); !strings.Contains(
				strings.ToLower(got), "bearer") {
				t.Fatalf("GET %s missing bearer challenge, got %q", target, got)
			}
		}()
	}
}

// TestAuthGateDistinguishesReauthenticationFromDenial is the crux of the
// re-authenticate-vs-denied contract. Both are HTTP 401, so they are told apart
// only by the bearer challenge the gate sets and the service does not: a token
// failure carries WWW-Authenticate, a governance denial from the service does
// not. Setting the header unconditionally, or removing it, breaks one half of
// this test.
func TestAuthGateDistinguishesReauthenticationFromDenial(t *testing.T) {
	fixture := newAuthnFixture(t)

	gate := gateGet(t, fixture, "/api/v1/meta", "")
	defer gate.Body.Close()
	if gate.StatusCode != http.StatusUnauthorized {
		t.Fatalf("auth-gate status = %d, want 401", gate.StatusCode)
	}
	if got := gate.Header.Get("WWW-Authenticate"); !strings.Contains(
		strings.ToLower(got), "bearer") {
		t.Fatalf("auth-gate 401 must carry a bearer challenge, got %q", got)
	}

	// The no-retrieve principal authenticates but is not authorized to
	// retrieve, so the service denies the operation with its own 401.
	denial := gatePost(t, fixture, "/api/v1/retrieve", "no-retrieve",
		authnJSON(t, webapi.RetrievalRequest{}))
	defer denial.Body.Close()
	if denial.StatusCode != http.StatusUnauthorized {
		t.Fatalf("governance denial status = %d, want 401", denial.StatusCode)
	}
	if got := denial.Header.Get("WWW-Authenticate"); got != "" {
		t.Fatalf(
			"governance denial must not carry a bearer challenge, got %q", got)
	}
}

// gateStubService is a do-nothing Service used to exercise transport concerns
// (auth-config, CSP) without a corpus.
type gateStubService struct{}

func (gateStubService) Documents(context.Context, webapi.DocumentsRequest) (webapi.DocumentsResponse, error) {
	return webapi.DocumentsResponse{}, nil
}
func (gateStubService) Document(context.Context, webapi.DocumentRequest) (webapi.DocumentResponse, error) {
	return webapi.DocumentResponse{}, nil
}
func (gateStubService) Retrieve(context.Context, webapi.RetrievalRequest) (webapi.RetrievalResponse, error) {
	return webapi.RetrievalResponse{}, nil
}
func (gateStubService) Neighborhood(context.Context, webapi.NeighborhoodRequest) (webapi.NeighborhoodResponse, error) {
	return webapi.NeighborhoodResponse{}, nil
}
func (gateStubService) Path(context.Context, webapi.PathRequest) (webapi.PathResponse, error) {
	return webapi.PathResponse{}, nil
}

func newStubServer(t *testing.T, configure func(*webapi.Handler)) *httptest.Server {
	t.Helper()
	server := httptest.NewUnstartedServer(nil)
	handler, err := webapi.NewHandler(
		gateStubService{}, server.Listener.Addr().String())
	if err != nil {
		server.Close()
		t.Fatal(err)
	}
	if configure != nil {
		configure(handler)
	}
	server.Config.Handler = handler
	server.Start()
	t.Cleanup(server.Close)
	return server
}

func getJSON(t *testing.T, server *httptest.Server, path string) (*http.Response, string) {
	t.Helper()
	response, err := server.Client().Get(server.URL + path)
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(response.Body)
	response.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	return response, string(body)
}

// TestAuthConfigReportsUnconfiguredWithoutBrowserAuth proves the local seam:
// with no browser auth configured, the endpoint reports unconfigured so the UI
// renders no login flow.
func TestAuthConfigReportsUnconfiguredWithoutBrowserAuth(t *testing.T) {
	server := newStubServer(t, nil)
	response, body := getJSON(t, server, "/api/v1/auth-config")
	if response.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", response.StatusCode)
	}
	if !strings.Contains(body, `"configured":false`) {
		t.Fatalf("auth-config = %s, want configured:false", body)
	}
}

// TestAuthConfigReportsConfiguredValues proves the endpoint returns exactly the
// non-secret login parameters and nothing else.
func TestAuthConfigReportsConfiguredValues(t *testing.T) {
	server := newStubServer(t, func(handler *webapi.Handler) {
		handler.SetBrowserAuthConfig(&webapi.BrowserAuthConfig{
			ClientID:              "client-456",
			Scope:                 "openid profile shoal.read",
			AuthorizationEndpoint: "https://identity.example/authorize",
			TokenEndpoint:         "https://tokens.example/token",
		})
	})
	response, body := getJSON(t, server, "/api/v1/auth-config")
	if response.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", response.StatusCode)
	}

	for _, want := range []string{
		`"configured":true`,
		`"client_id":"client-456"`,
		`"scope":"openid profile shoal.read"`,
		`"authorization_endpoint":"https://identity.example/authorize"`,
		`"token_endpoint":"https://tokens.example/token"`,
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("auth-config = %s, missing %s", body, want)
		}
	}
}

func TestAuthConfigPreservesDeprecatedBrowserFields(t *testing.T) {
	server := newStubServer(t, func(handler *webapi.Handler) {
		handler.SetBrowserAuthConfig(&webapi.BrowserAuthConfig{
			TenantID:  "tenant-123",
			ClientID:  "client-456",
			Scope:     "openid profile client-456/.default",
			Authority: "https://login.example/tenant-123",
		})
	})
	response, body := getJSON(t, server, "/api/v1/auth-config")
	if response.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", response.StatusCode)
	}
	for _, want := range []string{
		`"tenant_id":"tenant-123"`,
		`"authority":"https://login.example/tenant-123"`,
		`"authorization_endpoint":"https://login.example/tenant-123/oauth2/v2.0/authorize"`,
		`"token_endpoint":"https://login.example/tenant-123/oauth2/v2.0/token"`,
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("legacy auth-config = %s, missing %s", body, want)
		}
	}
	csp := response.Header.Get("Content-Security-Policy")
	if !strings.Contains(csp, "connect-src 'self' https://login.example;") {
		t.Fatalf("legacy CSP = %q", csp)
	}
}

// TestContentSecurityPolicyScopesToConfiguredEndpoints proves connect-src stays
// at the strict local default unless login is configured, then admits only the
// token endpoint origin used by the cross-origin fetch. The authorization
// endpoint is a top-level navigation and must not widen connect-src.
func TestContentSecurityPolicyScopesToConfiguredEndpoints(t *testing.T) {
	baseline := newStubServer(t, nil)
	response, _ := getJSON(t, baseline, "/api/v1/auth-config")
	if csp := response.Header.Get("Content-Security-Policy"); !strings.Contains(
		csp, "connect-src 'self';") {
		t.Fatalf("baseline CSP = %q, want connect-src 'self';", csp)
	}

	configured := newStubServer(t, func(handler *webapi.Handler) {
		handler.SetBrowserAuthConfig(&webapi.BrowserAuthConfig{
			ClientID:              "client-456",
			Scope:                 "openid",
			AuthorizationEndpoint: "https://identity.example/tenant/authorize",
			TokenEndpoint:         "https://tokens.example/oauth/token",
		})
	})
	response, _ = getJSON(t, configured, "/api/v1/auth-config")
	csp := response.Header.Get("Content-Security-Policy")
	if !strings.Contains(
		csp,
		"connect-src 'self' https://tokens.example;",
	) {
		t.Fatalf("configured CSP = %q, want token origin in connect-src", csp)
	}
	if strings.Contains(csp, "https://identity.example") {
		t.Fatalf("authorization origin unnecessarily widened connect-src: %q", csp)
	}
	if strings.Contains(csp, "/tenant/") || strings.Contains(csp, "/oauth/") {
		t.Fatalf("CSP leaked endpoint paths, must be origins only: %q", csp)
	}

	wildcard := newStubServer(t, func(handler *webapi.Handler) {
		handler.SetBrowserAuthConfig(&webapi.BrowserAuthConfig{
			ClientID:              "client-456",
			Scope:                 "openid",
			AuthorizationEndpoint: "https://identity.example/authorize",
			TokenEndpoint:         "https://*/token",
		})
	})
	response, _ = getJSON(t, wildcard, "/api/v1/auth-config")
	csp = response.Header.Get("Content-Security-Policy")
	if !strings.Contains(csp, "connect-src 'self';") ||
		strings.Contains(csp, "https://*") {
		t.Fatalf("wildcard token endpoint widened CSP: %q", csp)
	}
}
