/*
 * Licensed to the Apache Software Foundation (ASF) under one
 * or more contributor license agreements. See the NOTICE file
 * distributed with this work for additional information
 * regarding copyright ownership. The ASF licenses this file
 * to you under the Apache License, Version 2.0 (the
 * "License"); you may not use this file except in compliance
 * with the License. You may obtain a copy of the License at
 *
 *     https://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing,
 * software distributed under the License is distributed on an
 * "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY
 * KIND, either express or implied. See the License for the
 * specific language governing permissions and limitations
 * under the License.
 */

package webapi

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/phrocker/shoal-oss/pkg/explorer/auth"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

type mountedTestHandler struct {
	calls int
}

func (h *mountedTestHandler) ServeHTTP(
	writer http.ResponseWriter, _ *http.Request,
) {
	h.calls++
	writer.WriteHeader(http.StatusNoContent)
}

func (*mountedTestHandler) ValidatePreAuthentication(*http.Request) int {
	return 0
}

func TestMountAuthenticatedRejectsNonLiteralPatterns(t *testing.T) {
	for _, pattern := range []string{
		"/", "/{$}", "/mcp/{session}", "/mcp/{rest...}",
		"/api//mcp", "/api/../mcp", "/mcp%2fchild",
		"/mcp?query", "/mcp#fragment", "POST /mcp", "/mcp\\child",
	} {
		t.Run(pattern, func(t *testing.T) {
			handler := authenticatedTestHandler(
				t, &stubWorkspaceService{}, auth.OperationRead)
			if err := handler.MountAuthenticated(
				pattern, &mountedTestHandler{},
			); err == nil {
				t.Fatalf("MountAuthenticated(%q) succeeded", pattern)
			}
			if len(handler.preAuth) != 0 {
				t.Fatalf("invalid mount %q installed pre-auth state", pattern)
			}
		})
	}
}

func TestMountAuthenticatedAllowsNormalizedSubtree(t *testing.T) {
	handler := authenticatedTestHandler(
		t, &stubWorkspaceService{}, auth.OperationRead)
	mounted := &mountedTestHandler{}
	if err := handler.MountAuthenticated("/api/v1/fleet/", mounted); err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(
		http.MethodPost, "http://example.test/api/v1/fleet/actions", nil)
	response := httptest.NewRecorder()
	handler.mux.ServeHTTP(response, request)
	if mounted.calls != 1 {
		t.Fatalf("subtree handler calls = %d", mounted.calls)
	}
}

func TestMountAuthenticatedConflictIsAtomic(t *testing.T) {
	handler := authenticatedTestHandler(
		t, &stubWorkspaceService{}, auth.OperationRead)
	handler.mux.HandleFunc("GET /mcp", func(
		writer http.ResponseWriter, _ *http.Request,
	) {
		writer.WriteHeader(http.StatusAccepted)
	})
	mounted := &mountedTestHandler{}
	if err := handler.MountAuthenticated("/mcp", mounted); err == nil {
		t.Fatal("conflicting authenticated mount succeeded")
	}
	if len(handler.preAuth) != 0 {
		t.Fatal("failed mount installed pre-auth state")
	}
	response := httptest.NewRecorder()
	handler.mux.ServeHTTP(
		response, httptest.NewRequest(http.MethodPost, "/mcp", nil))
	if mounted.calls != 0 {
		t.Fatal("failed mount left a live partial route")
	}
	if response.Code != http.StatusMethodNotAllowed {
		t.Fatalf("POST after failed mount = %d, want 405", response.Code)
	}
}

func TestMountAuthenticatedDispatchesOnlySupportedMethods(t *testing.T) {
	handler := authenticatedTestHandler(
		t, &stubWorkspaceService{}, auth.OperationRead)
	mounted := &mountedTestHandler{}
	if err := handler.MountAuthenticated("/mcp", mounted); err != nil {
		t.Fatal(err)
	}
	if handler.preAuth["/mcp"] == nil {
		t.Fatal("valid mount did not install pre-auth validator")
	}
	for _, method := range []string{
		http.MethodGet, http.MethodHead, http.MethodPost, http.MethodDelete,
	} {
		response := httptest.NewRecorder()
		handler.mux.ServeHTTP(
			response, httptest.NewRequest(method, "/mcp", nil))
		if response.Code != http.StatusNoContent {
			t.Fatalf("%s /mcp = %d, want 204", method, response.Code)
		}
	}
	response := httptest.NewRecorder()
	handler.mux.ServeHTTP(
		response, httptest.NewRequest(http.MethodPut, "/mcp", nil))
	if response.Code != http.StatusMethodNotAllowed {
		t.Fatalf("PUT /mcp = %d, want 405", response.Code)
	}
	if mounted.calls != 4 {
		t.Fatalf("mounted handler calls = %d, want 4", mounted.calls)
	}
}

func TestMountAuthenticatedRejectsAnonymousHandler(t *testing.T) {
	handler, err := NewHandler(&stubWorkspaceService{}, "example.test")
	if err != nil {
		t.Fatal(err)
	}
	err = handler.MountAuthenticated("/mcp", &mountedTestHandler{})
	if !shoal.IsErrorCode(err, shoal.ErrorInvalidArgument) {
		t.Fatalf("anonymous authenticated mount = %v", err)
	}
}

func authenticatedTestHandler(
	t *testing.T, service Service, operations ...auth.Operation,
) *Handler {
	t.Helper()
	now := time.Date(2026, 9, 7, 2, 0, 0, 0, time.UTC)
	authority, err := auth.NewAuthorityWithClock(func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	decision, err := auth.NewDecision(auth.DecisionConfig{
		Subject: "subject", Actor: "actor", AuthorizationDomain: []byte("domain"),
		AllowedOperations: operations,
		PolicyGeneration:  1, AuthenticationExpires: now.Add(time.Hour),
		RequestID: "request", CorrelationID: "correlation",
	})
	if err != nil {
		t.Fatal(err)
	}
	handler, err := NewAuthenticatedHandler(
		service,
		AuthenticatorFunc(func(*http.Request) (auth.Decision, error) {
			return decision, nil
		}),
		authority.Binder(), "example.test",
	)
	if err != nil {
		t.Fatal(err)
	}
	return handler
}
