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

package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"mime/multipart"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/phrocker/shoal-oss/pkg/explorer/auth"
	"github.com/phrocker/shoal-oss/pkg/explorer/webapi"
	"github.com/phrocker/shoal-oss/pkg/interaction"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

// TestListenAddressIsLoopback pins the classification that decides whether the
// development principal may be minted at all.
func TestListenAddressIsLoopback(t *testing.T) {
	cases := []struct {
		address  string
		loopback bool
	}{
		{":8080", false},
		{"0.0.0.0:8080", false},
		{"127.0.0.1:8080", true},
		{"127.0.0.2:8080", true},
		{"[::1]:8080", true},
		{"[::]:8080", false},
		{"localhost:8080", true},
		{"192.168.1.10:8080", false},
		{"[fe80::1]:8080", false},
		{"", false},
		{"127.0.0.1", false},
		{"nonexistent.invalid:8080", false},
	}
	for _, testCase := range cases {
		t.Run(testCase.address, func(t *testing.T) {
			if got := listenAddressIsLoopback(testCase.address); got != testCase.loopback {
				t.Fatalf(
					"listenAddressIsLoopback(%q) = %t, want %t",
					testCase.address, got, testCase.loopback)
			}
		})
	}
}

// TestListenAddressIsLoopbackResolvesHostnamesClosed pins the ambiguity rule
// for names: every resolved address must be loopback, and an unresolvable or
// empty answer is never loopback. DNS cannot be relied on to produce these
// answers, so resolution is stubbed.
func TestListenAddressIsLoopbackResolvesHostnamesClosed(t *testing.T) {
	original := lookupListenHost
	t.Cleanup(func() { lookupListenHost = original })
	cases := []struct {
		name     string
		resolved []net.IP
		err      error
		loopback bool
	}{
		{"only IPv4 loopback", []net.IP{net.ParseIP("127.0.0.1")}, nil, true},
		{
			"both loopback families",
			[]net.IP{net.ParseIP("127.0.0.1"), net.ParseIP("::1")},
			nil,
			true,
		},
		{
			"loopback and routable",
			[]net.IP{net.ParseIP("127.0.0.1"), net.ParseIP("10.0.0.5")},
			nil,
			false,
		},
		{
			"routable first",
			[]net.IP{net.ParseIP("203.0.113.7"), net.ParseIP("127.0.0.1")},
			nil,
			false,
		},
		{
			"loopback and unspecified",
			[]net.IP{net.ParseIP("127.0.0.1"), net.ParseIP("0.0.0.0")},
			nil,
			false,
		},
		{"no addresses", nil, nil, false},
		{"resolution failed", nil, errors.New("no such host"), false},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			lookupListenHost = func(string) ([]net.IP, error) {
				return testCase.resolved, testCase.err
			}
			if got := listenAddressIsLoopback(
				"workspace.example:8080"); got != testCase.loopback {
				t.Fatalf(
					"listenAddressIsLoopback = %t, want %t", got, testCase.loopback)
			}
		})
	}
}

// TestRunRefusesRequestedAddressWithoutBinding proves the refused address is
// classified before anything is bound, so no socket is ever opened on an
// address the workspace may not serve.
func TestRunRefusesRequestedAddressWithoutBinding(t *testing.T) {
	cases := []struct {
		name string
		args []string
	}{
		{"all interfaces", []string{"-listen", "0.0.0.0:0", "-dev-auth"}},
		{"bare port", []string{"-listen", ":0", "-dev-auth"}},
		{"IPv6 wildcard", []string{"-listen", "[::]:0", "-dev-auth"}},
		{"routable address", []string{"-listen", "203.0.113.7:0", "-dev-auth"}},
		{"no authenticator", []string{"-listen", "127.0.0.1:0"}},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			original := listenTCP
			t.Cleanup(func() { listenTCP = original })
			bound := false
			listenTCP = func(network, address string) (net.Listener, error) {
				bound = true
				return nil, errors.New("the refused address was bound")
			}
			err := runBounded(
				t, append(append([]string{}, testCase.args...), "-data", t.TempDir()))
			if err == nil {
				t.Fatal("a refused address started the server")
			}
			if bound {
				t.Fatalf("the refused address was bound before refusal: %v", err)
			}
			if !strings.Contains(err.Error(), "refusing to serve") {
				t.Fatalf("unclear diagnostic: %v", err)
			}
		})
	}
}

func TestRunAcceptsDeprecatedEntraFlags(t *testing.T) {
	err := run(context.Background(), []string{
		"-entra-tenant", "tenant-123",
		"-entra-issuer", "http://invalid.example",
		"-entra-client-id", "client-456",
		"-entra-jwks-uri", "https://keys.example/jwks",
		"-entra-allowed-algs", "RS256",
		"-entra-clock-skew", "30s",
		"-entra-reader-roles", "Shoal.Reader",
		"-entra-contributor-roles", "Shoal.Contributor",
		"-entra-scope", "openid profile",
	}, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "issuer must use https") {
		t.Fatalf("deprecated Entra flags were not parsed and translated: %v", err)
	}
}

func TestRunAcceptsDeprecatedEntraEnvironment(t *testing.T) {
	for _, name := range []string{
		"SHOAL_OIDC_ISSUER", "SHOAL_OIDC_AUDIENCE",
		"SHOAL_OIDC_AUTHORIZATION_CLAIM", "SHOAL_OIDC_READER_VALUES",
		"SHOAL_OIDC_CONTRIBUTOR_VALUES",
	} {
		t.Setenv(name, "")
	}
	t.Setenv("SHOAL_ENTRA_TENANT", "tenant-123")
	t.Setenv("SHOAL_ENTRA_ISSUER", "http://invalid.example")
	t.Setenv("SHOAL_ENTRA_CLIENT_ID", "client-456")
	t.Setenv("SHOAL_ENTRA_JWKS_URI", "https://keys.example/jwks")
	t.Setenv("SHOAL_ENTRA_READER_ROLES", "Shoal.Reader")
	t.Setenv("SHOAL_ENTRA_CONTRIBUTOR_ROLES", "Shoal.Contributor")
	t.Setenv("SHOAL_ENTRA_SCOPE", "openid profile")
	err := run(context.Background(), nil, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "issuer must use https") {
		t.Fatalf("deprecated Entra environment was not translated: %v", err)
	}
}

// widenedListener reports a resolved address wider than the one that was
// requested, standing in for a bind that opens more than it was asked to.
type widenedListener struct {
	net.Listener
	closed bool
}

func (l *widenedListener) Addr() net.Addr {
	return &net.TCPAddr{IP: net.ParseIP("198.51.100.7"), Port: 9999}
}

func (l *widenedListener) Close() error {
	l.closed = true
	return l.Listener.Close()
}

// TestRunRefusesWidenedResolvedAddress proves the post-bind check is
// independently load-bearing: a listener whose resolved address is not
// loopback is refused and closed even though the requested address passed.
func TestRunRefusesWidenedResolvedAddress(t *testing.T) {
	original := listenTCP
	t.Cleanup(func() { listenTCP = original })
	var widened *widenedListener
	listenTCP = func(network, address string) (net.Listener, error) {
		inner, err := original(network, address)
		if err != nil {
			return nil, err
		}
		widened = &widenedListener{Listener: inner}
		return widened, nil
	}
	err := runBounded(t, []string{
		"-listen", "127.0.0.1:0", "-dev-auth", "-data", t.TempDir(),
	})
	if err == nil {
		t.Fatal("the server served a listener resolved to a non-loopback address")
	}
	if !strings.Contains(err.Error(), "refusing to serve") {
		t.Fatalf("unclear diagnostic: %v", err)
	}
	if widened == nil {
		t.Fatal("the listener was never opened")
	}
	if !widened.closed {
		t.Fatal("the refused listener was left open")
	}
}

// TestSelectAuthenticatorFailsClosed proves the workspace never serves without
// an authenticator and never mints the development principal off-host.
func TestSelectAuthenticatorFailsClosed(t *testing.T) {
	if _, err := selectAuthenticator(false, "", oidcConfig{}, "127.0.0.1:8080", time.Now); err == nil {
		t.Fatal("loopback without an authenticator was accepted")
	}
	if _, err := selectAuthenticator(false, "", oidcConfig{}, "0.0.0.0:8080", time.Now); err == nil {
		t.Fatal("non-loopback without an authenticator was accepted")
	}
	for _, address := range []string{":8080", "0.0.0.0:8080", "[::]:8080", "10.0.0.5:8080"} {
		authenticator, err := selectAuthenticator(true, "", oidcConfig{}, address, time.Now)
		if err == nil {
			t.Fatalf("-dev-auth was accepted on %s", address)
		}
		if authenticator != nil {
			t.Fatalf("refused %s still returned an authenticator", address)
		}
		if !strings.Contains(err.Error(), "refusing to serve") {
			t.Fatalf("unclear diagnostic for %s: %v", address, err)
		}
	}
	authenticator, err := selectAuthenticator(true, "", oidcConfig{}, "127.0.0.1:8080", time.Now)
	if err != nil {
		t.Fatal(err)
	}
	request, err := http.NewRequest(http.MethodGet, "http://127.0.0.1/api/v1/meta", nil)
	if err != nil {
		t.Fatal(err)
	}
	decision, err := authenticator.Authenticate(request)
	if err != nil {
		t.Fatal(err)
	}
	if decision.Subject() != developmentSubject {
		t.Fatalf("development subject = %q", decision.Subject())
	}
	if !operationsContain(
		decision.AllowedOperations(), auth.OperationDelegate,
	) {
		t.Fatal("development principal cannot register delegated agents")
	}
	second, err := authenticator.Authenticate(request)
	if err != nil {
		t.Fatal(err)
	}
	if second.RequestID() == decision.RequestID() {
		t.Fatal("two requests shared one request identity")
	}
}

// TestRunRefusesNonLoopbackWithoutRealAuthenticator proves the server exits
// with a diagnostic instead of exposing the development principal.
func TestRunRefusesNonLoopbackWithoutRealAuthenticator(t *testing.T) {
	err := runBounded(
		t, []string{"-listen", "0.0.0.0:0", "-dev-auth", "-data", t.TempDir()})
	if err == nil {
		t.Fatal("the server started on a non-loopback address with -dev-auth")
	}
	if !strings.Contains(err.Error(), "refusing to serve") {
		t.Fatalf("unclear diagnostic: %v", err)
	}
}

// TestRunRefusesLoopbackWithoutAuthenticator proves that omitting every
// authenticator is refused rather than serving anonymously.
func TestRunRefusesLoopbackWithoutAuthenticator(t *testing.T) {
	err := runBounded(t, []string{"-listen", "127.0.0.1:0", "-data", t.TempDir()})
	if err == nil {
		t.Fatal("the server started without authentication")
	}
	if !strings.Contains(err.Error(), "without authentication") {
		t.Fatalf("unclear diagnostic: %v", err)
	}
}

// TestRunRefusesRemoteBackend records the deliberate decision to close the
// remote path rather than forward calls with no caller identity.
func TestRunRefusesRemoteBackend(t *testing.T) {
	err := runBounded(t, []string{
		"-listen", "127.0.0.1:0", "-dev-auth",
		"-backend", "remote", "-remote", "http://127.0.0.1:9/",
	})
	if err == nil {
		t.Fatal("the remote backend served without propagating identity")
	}
	if !strings.Contains(err.Error(), "backend remote is unavailable") {
		t.Fatalf("unclear diagnostic: %v", err)
	}
}

// runBounded runs the command and requires it to return on its own. A refusal
// returns immediately; a server that starts is shut down and reported as
// having started, so a weakened guard fails instead of hanging the suite.
func runBounded(t *testing.T, args []string) error {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- run(ctx, args, io.Discard) }()
	select {
	case err := <-done:
		return err
	case <-time.After(2 * time.Second):
	}
	cancel()
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("the server did not shut down")
	}
	return nil
}

// TestRunServesLoopbackDevelopmentPrincipal is the end-to-end counterpart: on
// a loopback listener, -dev-auth authenticates real requests.
func TestRunServesLoopbackDevelopmentPrincipal(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	reader, writer := io.Pipe()
	defer writer.Close()
	done := make(chan error, 1)
	go func() {
		done <- run(
			ctx,
			[]string{"-listen", "127.0.0.1:0", "-dev-auth", "-data", t.TempDir()},
			writer,
		)
	}()
	address := awaitListenAddress(t, reader)
	go func() { _, _ = io.Copy(io.Discard, reader) }()

	client := http.Client{Timeout: 10 * time.Second}
	response, err := client.Post(
		"http://"+address+"/api/v1/documents",
		"application/json",
		strings.NewReader(`{"page":{"limit":10}}`),
	)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusOK {
		t.Fatalf("documents status = %s body = %s", response.Status, body)
	}
	uploadDevelopmentDocument(t, address)
	listed := postJSON(t, "http://"+address+"/api/v1/documents", `{"page":{"limit":10}}`)
	if !strings.Contains(string(listed), "uploaded.md") {
		t.Fatalf("workspace upload was not authorized for listing: %s", listed)
	}
	for _, route := range []string{
		"/api/v1/fleet/agents",
		"/api/v1/fleet/events/subscriptions",
		"/api/v1/fleet/events/publish",
	} {
		response, err := client.Post(
			"http://"+address+route,
			"application/json",
			strings.NewReader(`{}`),
		)
		if err != nil {
			t.Fatalf("%s: %v", route, err)
		}
		_, _ = io.Copy(io.Discard, response.Body)
		response.Body.Close()
		if response.StatusCode == http.StatusNotFound ||
			response.StatusCode == http.StatusMethodNotAllowed {
			t.Fatalf("%s was not mounted: status = %s", route, response.Status)
		}
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("server exit = %v", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("the server did not shut down")
	}
}

func awaitListenAddress(t *testing.T, reader io.Reader) string {
	t.Helper()
	scanner := bufio.NewScanner(reader)
	for scanner.Scan() {
		line := scanner.Text()
		_, address, found := strings.Cut(line, "listening at http://")
		if found {
			return strings.TrimSpace(address)
		}
	}
	t.Fatal("the server never reported a listen address")
	return ""
}

// uploadDevelopmentDocument ingests through the running workspace so the
// authorized client registers policy for the new revision.
func uploadDevelopmentDocument(t *testing.T, address string) {
	t.Helper()
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	part, err := writer.CreateFormFile("files", "uploaded.md")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := part.Write([]byte("# Uploaded\n\nauthorized content\n")); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	request, err := http.NewRequest(
		http.MethodPost, "http://"+address+"/api/v1/ingest", &body)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Content-Type", writer.FormDataContentType())
	request.Header.Set("X-Shoal-Workspace-Request", "1")
	client := http.Client{Timeout: 30 * time.Second}
	response, err := client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	payload, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusOK {
		t.Fatalf("upload status = %s body = %s", response.Status, payload)
	}
}

// TestEveryShippedAuthenticatorMintsACorrelationID is the test whose absence
// let #524 ship.
//
// Every dispatch, admission and approval route requires a correlation ID, and
// neither shipped authenticator set one, so every such request was refused
// with "dispatch correlation ID is required". The bound wrappers overwrite the
// request body's value with the decision's — correctly, so a body cannot forge
// provenance — which meant no client-side workaround existed either.
//
// It survived because every end-to-end test in this package mints a decision
// directly rather than going through an authenticator. So this asserts the
// property at the mint, and TestTheDispatchSurfaceAcceptsAnAuthenticatedRequest
// asserts it end to end.
func TestEveryShippedAuthenticatorMintsACorrelationID(t *testing.T) {
	now := time.Date(2026, 9, 6, 12, 0, 0, 0, time.UTC)
	development, err := newDevelopmentAuthenticator(fixedClock(now))
	if err != nil {
		t.Fatal(err)
	}

	request := httptest.NewRequest(http.MethodPost, "/api/v1/fleet/actions", nil)
	decision, err := development.Authenticate(request)
	if err != nil {
		t.Fatal(err)
	}
	if decision.CorrelationID() == "" {
		t.Fatal("the development authenticator mints no correlation ID, so " +
			"every dispatch, admission and approval route refuses it")
	}
	// The generated value is one the interaction audit records (#532): the
	// mint and the session boundary apply one rule.
	if err := interaction.ValidateCorrelationID(
		decision.CorrelationID()); err != nil ||
		interaction.RecordableCorrelationID(decision.CorrelationID()) !=
			decision.CorrelationID() {
		t.Fatalf("generated correlation %q is not recordable: %v",
			decision.CorrelationID(), err)
	}
	if decision.RequestID() == decision.CorrelationID() {
		t.Fatalf("the request and correlation identities are the same value "+
			"(%q), so the correlation threads nothing and cannot be told "+
			"apart in an audit record", decision.CorrelationID())
	}

	// Two requests are separate traces, which is what the authenticator's own
	// doc comment promises about request identity.
	second, err := development.Authenticate(
		httptest.NewRequest(http.MethodPost, "/api/v1/fleet/actions", nil))
	if err != nil {
		t.Fatal(err)
	}
	if second.CorrelationID() == decision.CorrelationID() {
		t.Fatal("two unrelated requests were given one correlation ID")
	}

	// A supplied header threads the caller's trace across the hop, which is
	// the field's entire purpose for a gateway or an agent acting for a user.
	supplied := httptest.NewRequest(
		http.MethodPost, "/api/v1/fleet/actions", nil)
	supplied.Header.Set(CorrelationIDHeader, "upstream-trace-7")
	threaded, err := development.Authenticate(supplied)
	if err != nil {
		t.Fatal(err)
	}
	if threaded.CorrelationID() != "upstream-trace-7" {
		t.Fatalf("a supplied correlation was not honoured: %q",
			threaded.CorrelationID())
	}

	// And a malformed one is refused rather than silently replaced, so a
	// caller that meant to thread a trace learns that it did not.
	for _, probe := range []struct {
		name   string
		values []string
	}{
		{"empty", []string{""}},
		{"padded", []string{" trace "}},
		{"a space inside", []string{"two words"}},
		{"a control character", []string{"trace\x00"}},
		{"a bidi override", []string{"trace‮txt"}},
		// Ranging over the string decodes a bad byte to U+FFFD, which is
		// printable; net/http admits it as obs-text (#532 review).
		{"invalid UTF-8", []string{"bad\xffbyte"}},
		{"a truncated rune", []string{"trace\xe2\x80"}},
		{"repeated", []string{"one", "two"}},
	} {
		t.Run(probe.name, func(t *testing.T) {
			refused := httptest.NewRequest(
				http.MethodPost, "/api/v1/fleet/actions", nil)
			for _, value := range probe.values {
				refused.Header.Add(CorrelationIDHeader, value)
			}
			if _, err := development.Authenticate(refused); err == nil {
				t.Fatal("a malformed correlation ID was accepted into a " +
					"decision, and from there into a durable audit record")
			} else if !shoal.IsErrorCode(err, shoal.ErrorInvalidArgument) {
				t.Fatalf("refused as %v, want an invalid argument", err)
			}
		})
	}
}

// TestTheDispatchSurfaceAcceptsAnAuthenticatedRequest drives a real request
// through the real development authenticator into the real bound dispatch
// provider, which is the path #524 broke and which nothing exercised.
//
// Every other end-to-end test in this package injects a bound context instead
// of authenticating — `h.as(who)` — so the authenticator's output never
// reached a dispatch route. With no correlation ID minted, every such route
// answered "dispatch correlation ID is required", and the bound wrappers
// overwrite the body's value with the decision's, so no client could work
// around it.
//
// The assertion is deliberately narrow: this decision is not scoped to the
// harness's registered agent, so the request may well be refused for
// authorization or for an unknown action. What it must not be refused for is a
// missing correlation ID — that is the gate above all of those.
func TestTheDispatchSurfaceAcceptsAnAuthenticatedRequest(t *testing.T) {
	h := newApprovalHarness(t)
	development, err := newDevelopmentAuthenticator(h.now)
	if err != nil {
		t.Fatal(err)
	}
	handler, err := webapi.NewAuthenticatedHandler(
		h.opened.service, development, h.authority.Binder(), "example.test")
	if err != nil {
		t.Fatal(err)
	}
	if err := handler.MountFleetDispatch(h.opened.fleetDispatch); err != nil {
		t.Fatal(err)
	}

	body, err := json.Marshal(map[string]any{
		"context": map[string]any{
			"request_id": base64.RawURLEncoding.EncodeToString(
				[]byte("authenticated-request")),
			"reason_code": "operator_request",
			"deadline":    h.now().Add(time.Hour).Format(time.RFC3339Nano),
		},
		"id": base64.RawURLEncoding.EncodeToString([]byte("authenticated")),
		"idempotency_key": base64.RawURLEncoding.EncodeToString(
			[]byte("authenticated-key")),
		"agent_id":         base64.RawURLEncoding.EncodeToString([]byte("gateway")),
		"agent_generation": 1, "capability": "ops", "action": "deploy",
		"source_id": workspaceSourceID, "policy_id": workspaceGrantPolicyID,
		"object_id": base64.RawURLEncoding.EncodeToString([]byte("release-7")),
		"input":     json.RawMessage(`{"version":"7"}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost,
		"http://example.test/api/v1/fleet/actions", bytes.NewReader(body))
	request.Host = "example.test"
	request.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)

	if strings.Contains(recorder.Body.String(), "correlation ID is required") {
		t.Fatalf("an authenticated request was refused for a missing "+
			"correlation ID, so the whole dispatch surface is unreachable "+
			"through the shipped authenticators: %d %s",
			recorder.Code, recorder.Body)
	}
}
