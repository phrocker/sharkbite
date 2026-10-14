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
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/phrocker/shoal-oss/pkg/explorer/webapi"
)

// TestIdentityEndpointSurfacesTheBoundDecision proves the transport projects
// the trusted per-request decision back to the browser so the workspace can
// show a caller who they are. It reads only the decision the authenticator
// minted, not process state.
func TestIdentityEndpointSurfacesTheBoundDecision(t *testing.T) {
	fixture := newAuthnFixture(t)
	response := fixture.do(t, http.MethodGet, "/api/v1/identity", "granted", "")
	if response.status != http.StatusOK {
		t.Fatalf("status = %d body = %s", response.status, response.body)
	}
	var identity webapi.IdentityResponse
	if err := json.Unmarshal([]byte(response.body), &identity); err != nil {
		t.Fatalf("decode identity: %v body = %s", err, response.body)
	}
	if !identity.Authenticated {
		t.Fatalf("identity is not authenticated: %s", response.body)
	}
	if identity.Subject != "granted" {
		t.Fatalf("subject = %q", identity.Subject)
	}
	if identity.Actor != "granted-actor" {
		t.Fatalf("actor = %q", identity.Actor)
	}
	if identity.AuthorizationDomain != "authn-domain" {
		t.Fatalf("authorization domain = %q", identity.AuthorizationDomain)
	}
	if !containsString(identity.Operations, "retrieve") ||
		!containsString(identity.Operations, "list") {
		t.Fatalf("operations = %v", identity.Operations)
	}
	// The projection must never leak the grant identifiers that define a
	// principal's visibility, only the identity the caller may see for itself.
	for _, secret := range []string{"authn-source-granted", "authn-policy-granted"} {
		if strings.Contains(response.body, secret) {
			t.Fatalf("identity leaked grant material %q: %s", secret, response.body)
		}
	}
}

// TestIdentityEndpointRequiresAuthentication proves the identity route is
// gated exactly like every other route: a caller whose identity cannot be
// established learns nothing, not even a blank identity envelope.
func TestIdentityEndpointRequiresAuthentication(t *testing.T) {
	fixture := newAuthnFixture(t)
	response := fixture.do(t, http.MethodGet, "/api/v1/identity", "", "")
	if response.status != http.StatusUnauthorized {
		t.Fatalf("status = %d body = %s", response.status, response.body)
	}
	fixture.assertDenied(t, response.body)
}

// TestIdentityEndpointReportsNoPrincipalWithoutAuthentication proves a handler
// built without an authenticator answers the identity route with an explicit
// unauthenticated projection rather than a misleading blank subject.
func TestIdentityEndpointReportsNoPrincipalWithoutAuthentication(t *testing.T) {
	server := httptest.NewUnstartedServer(nil)
	handler, err := webapi.NewHandler(
		capabilityOnlyService{}, server.Listener.Addr().String())
	if err != nil {
		server.Close()
		t.Fatal(err)
	}
	server.Config.Handler = handler
	server.Start()
	t.Cleanup(server.Close)

	request, err := http.NewRequest(
		http.MethodGet, server.URL+"/api/v1/identity", nil)
	if err != nil {
		t.Fatal(err)
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", response.StatusCode)
	}
	var identity webapi.IdentityResponse
	if err := json.NewDecoder(response.Body).Decode(&identity); err != nil {
		t.Fatal(err)
	}
	if identity.Authenticated {
		t.Fatalf("unauthenticated handler reported an authenticated principal: %+v", identity)
	}
	if identity.Operations == nil {
		t.Fatalf("operations must serialize as a non-nil JSON array")
	}
}

func containsString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}
