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
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/phrocker/shoal-oss/pkg/explorer/auth"
	"github.com/phrocker/shoal-oss/pkg/explorer/webapi"
)

const forgedHost = "attacker.invalid"

// newHostGatedServer starts a handler whose single allowed authority is the
// listener's own address, so the client's default requests match and a forged
// Host does not.
func newHostGatedServer(t *testing.T) *httptest.Server {
	t.Helper()
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
	return server
}

// doWithHost issues a request to path with an explicit Host header, without
// following redirects, and returns the status and body.
func doWithHost(t *testing.T, url, host string) (int, string) {
	t.Helper()
	request, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	if host != "" {
		request.Host = host
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	return response.StatusCode, string(body)
}

// TestForgedHostRefusedOnEveryRoute proves the host-authority gate runs in
// ServeHTTP before routing, so a forged Host is refused with 421 Misdirected
// Request on every path — registered API routes, static assets, the workspace
// shell, and an unregistered path alike. A route added outside the gate would
// answer the forged Host with something other than 421 and fail here.
//
// The paired good-Host column asserts the same paths are NOT refused with 421,
// so the 421 is attributable to the Host and not to the path being unreachable:
// the unregistered path yields 404 (routing was reached) rather than 421.
func TestForgedHostRefusedOnEveryRoute(t *testing.T) {
	server := newHostGatedServer(t)
	goodHost := server.Listener.Addr().String()

	paths := []string{
		"/",
		"/api/v1/meta",
		"/api/v1/identity",
		"/api/v1/documents",
		"/api/v1/retrieve",
		"/assets/app.js",
		"/no-such-route",
	}
	for _, path := range paths {
		forgedStatus, body := doWithHost(t, server.URL+path, forgedHost)
		if forgedStatus != http.StatusMisdirectedRequest {
			t.Fatalf("forged Host on %s status = %d, want 421", path, forgedStatus)
		}
		if strings.Contains(body, forgedHost) {
			t.Fatalf("refusal body for %s echoed the forged Host: %q", path, body)
		}

		goodStatus, _ := doWithHost(t, server.URL+path, goodHost)
		if goodStatus == http.StatusMisdirectedRequest {
			t.Fatalf("allowed Host on %s was refused with 421", path)
		}
	}

	// The unregistered path must reach routing under the allowed Host, proving
	// the forged-Host 421 above was the gate and not an unreachable path.
	if status, _ := doWithHost(t, server.URL+"/no-such-route", goodHost); status != http.StatusNotFound {
		t.Fatalf("allowed Host on unregistered path status = %d, want 404", status)
	}
}

// TestForgedHostRefusalBodyIsFixed proves the refusal is a plain, fixed message
// that does not reflect the attacker-supplied Host into the response, keeping
// the gate from becoming a reflected-content surface of its own.
func TestForgedHostRefusalBodyIsFixed(t *testing.T) {
	server := newHostGatedServer(t)
	status, body := doWithHost(t, server.URL+"/api/v1/meta", forgedHost)
	if status != http.StatusMisdirectedRequest {
		t.Fatalf("status = %d, want 421", status)
	}
	if strings.TrimSpace(body) != "misdirected request" {
		t.Fatalf("refusal body = %q, want the fixed 'misdirected request'", body)
	}
}

// TestForgedHostWrongPortRefused proves the port participates in the match over
// the real HTTP path: the correct hostname on a different port is refused with
// 421, not admitted.
func TestForgedHostWrongPortRefused(t *testing.T) {
	server := newHostGatedServer(t)
	host, _, ok := strings.Cut(server.Listener.Addr().String(), ":")
	if !ok {
		t.Fatalf("unexpected listener address %q", server.Listener.Addr().String())
	}
	status, _ := doWithHost(t, server.URL+"/api/v1/meta", host+":1")
	if status != http.StatusMisdirectedRequest {
		t.Fatalf("correct host wrong port status = %d, want 421", status)
	}
}

// TestXForwardedHostAloneDoesNotGrantAccess proves the gate reads only the
// request authority (Request.Host) and never X-Forwarded-Host: a forged Host is
// refused even when X-Forwarded-Host carries the allowed authority, so a proxy
// header alone cannot bypass the allow-list.
func TestXForwardedHostAloneDoesNotGrantAccess(t *testing.T) {
	server := newHostGatedServer(t)
	request, err := http.NewRequest(http.MethodGet, server.URL+"/api/v1/meta", nil)
	if err != nil {
		t.Fatal(err)
	}
	request.Host = forgedHost
	request.Header.Set("X-Forwarded-Host", server.Listener.Addr().String())
	request.Header.Set("Forwarded", "host="+server.Listener.Addr().String())
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusMisdirectedRequest {
		t.Fatalf("X-Forwarded-Host bypass status = %d, want 421", response.StatusCode)
	}
}

// denyAllAuthenticator denies every request, so a permitted Host reaches
// authentication and is answered with 401 while a forged Host is stopped earlier
// with 421.
type denyAllAuthenticator struct{}

func (denyAllAuthenticator) Authenticate(*http.Request) (auth.Decision, error) {
	return auth.Decision{}, errors.New("denied for test")
}

// TestHostGatePrecedesAuthentication proves the two defence layers produce
// distinguishable, separately assertable outcomes: with an authenticator that
// denies everything, a forged Host yields 421 (the host gate, which runs first)
// while the allowed Host yields 401 (authentication). If the host gate were
// removed, the forged Host would fall through to the same 401 and this test's
// 421 assertion would fail — so the gate is independently observable here even
// though authentication would also refuse the request.
func TestHostGatePrecedesAuthentication(t *testing.T) {
	server := httptest.NewUnstartedServer(nil)
	handler, err := webapi.NewAuthenticatedHandler(
		capabilityOnlyService{},
		denyAllAuthenticator{},
		auth.NewAuthority().Binder(),
		server.Listener.Addr().String(),
	)
	if err != nil {
		server.Close()
		t.Fatal(err)
	}
	server.Config.Handler = handler
	server.Start()
	t.Cleanup(server.Close)

	if status, _ := doWithHost(t, server.URL+"/api/v1/meta", forgedHost); status != http.StatusMisdirectedRequest {
		t.Fatalf("forged Host status = %d, want 421 (host gate before auth)", status)
	}
	if status, _ := doWithHost(
		t, server.URL+"/api/v1/meta", server.Listener.Addr().String()); status != http.StatusUnauthorized {
		t.Fatalf("allowed Host status = %d, want 401 (authentication layer)", status)
	}
}
