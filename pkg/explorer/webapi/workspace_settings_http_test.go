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
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/phrocker/shoal-oss/pkg/explorer"
	"github.com/phrocker/shoal-oss/pkg/explorer/auth"
	"github.com/phrocker/shoal-oss/pkg/explorer/webapi"
	"github.com/phrocker/shoal-oss/pkg/explorer/workspace"
	"github.com/phrocker/shoal-oss/pkg/ontology"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

type settingsStubService struct{}

type indeterminateSettingsProvider struct {
	webapi.WorkspaceSettingsProvider
}

func (indeterminateSettingsProvider) Update(
	context.Context,
	shoal.ID,
	workspace.UpdateRequest,
) (workspace.Settings, error) {
	return workspace.Settings{Revision: 2}, explorer.MarkIndeterminateCommit(
		shoal.NewError(
			shoal.ErrorUnauthorized,
			"authorization changed after workspace settings commit",
		),
	)
}

type settingsGenerationReader int64

func (r settingsGenerationReader) CurrentPolicyGeneration(
	ctx context.Context,
	_ []byte,
) (int64, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	return int64(r), nil
}

func settingsProviderOptions(
	resolver auth.Resolver,
	now time.Time,
) workspace.ProviderOptions {
	return workspace.ProviderOptions{
		Resolver:         resolver,
		GenerationReader: settingsGenerationReader(1),
		Clock:            func() time.Time { return now },
	}
}

func (settingsStubService) Documents(
	context.Context,
	webapi.DocumentsRequest,
) (webapi.DocumentsResponse, error) {
	return webapi.DocumentsResponse{}, nil
}

func (settingsStubService) Document(
	context.Context,
	webapi.DocumentRequest,
) (webapi.DocumentResponse, error) {
	return webapi.DocumentResponse{}, nil
}

func (settingsStubService) Retrieve(
	context.Context,
	webapi.RetrievalRequest,
) (webapi.RetrievalResponse, error) {
	return webapi.RetrievalResponse{}, nil
}

func (settingsStubService) Neighborhood(
	context.Context,
	webapi.NeighborhoodRequest,
) (webapi.NeighborhoodResponse, error) {
	return webapi.NeighborhoodResponse{}, nil
}

func (settingsStubService) Path(
	context.Context,
	webapi.PathRequest,
) (webapi.PathResponse, error) {
	return webapi.PathResponse{}, nil
}

func TestHTTPWorkspaceSettingsRoundTripAuthorizationAndRestart(t *testing.T) {
	now := time.Date(2026, 9, 5, 18, 0, 0, 0, time.UTC)
	dir := filepath.Join(t.TempDir(), "settings")
	authority, err := auth.NewAuthorityWithClock(func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	store, err := workspace.OpenDurableStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	provider, err := workspace.NewProvider(
		store, settingsProviderOptions(authority.Resolver(), now))
	if err != nil {
		t.Fatal(err)
	}
	authenticator := webapi.AuthenticatorFunc(func(
		request *http.Request,
	) (auth.Decision, error) {
		subject := shoal.ID(request.Header.Get("X-Test-Subject"))
		if subject == "" {
			subject = "owner"
		}
		return settingsHTTPDecision(t, now, subject), nil
	})
	handler, err := webapi.NewAuthenticatedHandler(
		settingsStubService{}, authenticator, authority.Binder(), "example.test")
	if err != nil {
		t.Fatal(err)
	}
	if err := handler.SetWorkspaceSettingsProvider(provider); err != nil {
		t.Fatal(err)
	}
	if err := handler.MountWorkspaceSettings(
		webapi.WorkspaceSettingsHTTPConfig{Provider: provider},
	); !shoal.IsErrorCode(err, shoal.ErrorConflict) {
		t.Fatalf("duplicate mount error = %v, want conflict", err)
	}

	workspacePath := base64.RawURLEncoding.EncodeToString([]byte("workspace-http"))
	mutationID := base64.RawURLEncoding.EncodeToString([]byte("mutation-http"))
	sourceID, _ := auth.EncodeComponent([]byte("source-a"))
	policyID, _ := auth.EncodeComponent([]byte("policy-a"))
	body := map[string]any{
		"expected_revision": uint64(0),
		"mutation_id":       mutationID,
		"settings": map[string]any{
			"permitted_source_ids": []string{},
			"permitted_policy_ids": []string{policyID},
			"budgets": map[string]any{
				"retrieval_top_k": uint32(8),
			},
			"output_policies": []map[string]any{{
				"source_id": sourceID, "grant_policy_id": policyID,
				"epoch": int64(1),
			}},
		},
	}
	response := settingsRequest(
		t, handler, http.MethodPut,
		"/api/v1/workspaces/"+workspacePath+"/settings", body, "owner", "http://example.test")
	if response.Code != http.StatusCreated {
		t.Fatalf("PUT status = %d, body = %s", response.Code, response.Body.String())
	}
	var created webapi.WorkspaceSettingsResponse
	if err := json.Unmarshal(response.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	if created.Revision != 1 ||
		created.Settings.PermittedSourceIDs == nil ||
		len(*created.Settings.PermittedSourceIDs) != 0 {
		t.Fatalf("created response = %#v", created)
	}

	response = settingsRequest(
		t, handler, http.MethodGet,
		"/api/v1/workspaces/"+workspacePath+"/settings", nil, "owner", "")
	if response.Code != http.StatusOK {
		t.Fatalf("GET status = %d, body = %s", response.Code, response.Body.String())
	}
	var read webapi.WorkspaceSettingsResponse
	if err := json.Unmarshal(response.Body.Bytes(), &read); err != nil {
		t.Fatal(err)
	}
	if read.SettingsID != created.SettingsID ||
		read.LastMutationID != created.LastMutationID ||
		read.Settings.Budgets.RetrievalTopK == nil ||
		*read.Settings.Budgets.RetrievalTopK != 8 {
		t.Fatalf("read response = %#v", read)
	}

	response = settingsRequest(
		t, handler, http.MethodGet,
		"/api/v1/workspaces/"+workspacePath+"/settings", nil, "other", "")
	if response.Code != http.StatusNotFound {
		t.Fatalf("cross-owner GET status = %d, body = %s",
			response.Code, response.Body.String())
	}
	response = settingsRequest(
		t, handler, http.MethodPut,
		"/api/v1/workspaces/"+workspacePath+"/settings", body, "other", "http://example.test")
	if response.Code != http.StatusNotFound {
		t.Fatalf("cross-owner PUT status = %d, body = %s",
			response.Code, response.Body.String())
	}

	staleBody := cloneJSONMap(t, body)
	staleBody["mutation_id"] = base64.RawURLEncoding.EncodeToString([]byte("stale"))
	response = settingsRequest(
		t, handler, http.MethodPut,
		"/api/v1/workspaces/"+workspacePath+"/settings",
		staleBody, "owner", "http://example.test")
	if response.Code != http.StatusConflict {
		t.Fatalf("stale PUT status = %d, body = %s",
			response.Code, response.Body.String())
	}
	response = settingsRequest(
		t, handler, http.MethodPut,
		"/api/v1/workspaces/"+workspacePath+"/settings",
		body, "owner", "https://attacker.example")
	if response.Code != http.StatusUnauthorized {
		t.Fatalf("cross-origin PUT status = %d, body = %s",
			response.Code, response.Body.String())
	}
	response = settingsRequest(
		t, handler, http.MethodPut,
		"/api/v1/workspaces/"+workspacePath+"/settings",
		body, "owner", "https://example.test")
	if response.Code != http.StatusUnauthorized {
		t.Fatalf("untrusted cross-scheme PUT status = %d, body = %s",
			response.Code, response.Body.String())
	}
	response = settingsRequestWithFetchSite(
		t, handler, http.MethodPut,
		"/api/v1/workspaces/"+workspacePath+"/settings",
		body, "owner", "https://example.test", "same-origin")
	if response.Code != http.StatusCreated {
		t.Fatalf("TLS-terminating proxy PUT status = %d, body = %s",
			response.Code, response.Body.String())
	}
	response = settingsRequestWithFetchSite(
		t, handler, http.MethodPut,
		"/api/v1/workspaces/"+workspacePath+"/settings",
		body, "owner", "https://example.test:443", "same-origin")
	if response.Code != http.StatusCreated {
		t.Fatalf("default-port proxy PUT status = %d, body = %s",
			response.Code, response.Body.String())
	}
	response = settingsRequestWithFetchSite(
		t, handler, http.MethodPut,
		"/api/v1/workspaces/"+workspacePath+"/settings",
		body, "owner", "https://example.test", "same-site")
	if response.Code != http.StatusUnauthorized {
		t.Fatalf("cross-scheme PUT status = %d, body = %s",
			response.Code, response.Body.String())
	}

	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := workspace.OpenDurableStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	restartedProvider, err := workspace.NewProvider(
		reopened, settingsProviderOptions(authority.Resolver(), now))
	if err != nil {
		t.Fatal(err)
	}
	restartedHandler, err := webapi.NewAuthenticatedHandler(
		settingsStubService{}, authenticator, authority.Binder(), "example.test")
	if err != nil {
		t.Fatal(err)
	}
	if err := restartedHandler.SetWorkspaceSettingsProvider(
		restartedProvider); err != nil {
		t.Fatal(err)
	}
	response = settingsRequest(
		t, restartedHandler, http.MethodGet,
		"/api/v1/workspaces/"+workspacePath+"/settings", nil, "owner", "")
	if response.Code != http.StatusOK {
		t.Fatalf("restart GET status = %d, body = %s",
			response.Code, response.Body.String())
	}
	var restarted webapi.WorkspaceSettingsResponse
	if err := json.Unmarshal(response.Body.Bytes(), &restarted); err != nil {
		t.Fatal(err)
	}
	if restarted.Settings.PermittedSourceIDs == nil ||
		len(*restarted.Settings.PermittedSourceIDs) != 0 {
		t.Fatalf("restart collapsed explicit empty scope: %#v", restarted)
	}
}

func TestHTTPWorkspaceSettingsRejectsBoundsAndUnknownFields(t *testing.T) {
	now := time.Date(2026, 9, 5, 18, 0, 0, 0, time.UTC)
	authority, _ := auth.NewAuthorityWithClock(func() time.Time { return now })
	store, err := workspace.OpenDurableStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	provider, err := workspace.NewProvider(
		store, settingsProviderOptions(authority.Resolver(), now))
	if err != nil {
		t.Fatal(err)
	}
	plain, err := webapi.NewHandler(settingsStubService{}, "example.test")
	if err != nil {
		t.Fatal(err)
	}
	if err := plain.SetWorkspaceSettingsProvider(provider); !shoal.IsErrorCode(
		err, shoal.ErrorInvalidArgument,
	) {
		t.Fatalf("unauthenticated settings handler error = %v", err)
	}
	handler, err := webapi.NewAuthenticatedHandler(
		settingsStubService{},
		webapi.AuthenticatorFunc(func(*http.Request) (auth.Decision, error) {
			return settingsHTTPDecision(t, now, "owner"), nil
		}),
		authority.Binder(),
		"example.test",
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := handler.SetWorkspaceSettingsProvider(provider); err != nil {
		t.Fatal(err)
	}
	path := "/api/v1/workspaces/" +
		base64.RawURLEncoding.EncodeToString([]byte("bounded")) + "/settings"
	noncanonicalPath := settingsRequest(
		t, handler, http.MethodGet,
		"/api/v1/workspaces/YR/settings", nil, "owner", "")
	if noncanonicalPath.Code != http.StatusBadRequest {
		t.Fatalf("noncanonical workspace status = %d, body = %s",
			noncanonicalPath.Code, noncanonicalPath.Body.String())
	}
	for _, test := range []struct {
		name string
		body map[string]any
	}{
		{
			name: "budget",
			body: map[string]any{
				"expected_revision": 0,
				"mutation_id":       base64.RawURLEncoding.EncodeToString([]byte("budget")),
				"settings": map[string]any{
					"budgets": map[string]any{
						"retrieval_top_k": workspace.MaxRetrievalTopK + 1,
					},
				},
			},
		},
		{
			name: "unknown field",
			body: map[string]any{
				"expected_revision": 0,
				"mutation_id":       base64.RawURLEncoding.EncodeToString([]byte("unknown")),
				"settings":          map[string]any{"ambient_lens": "forbidden"},
			},
		},
		{
			name: "noncanonical mutation ID",
			body: map[string]any{
				"expected_revision": 0,
				"mutation_id":       "YR",
				"settings":          map[string]any{},
			},
		},
		{
			name: "unknown ontology selection",
			body: map[string]any{
				"expected_revision": 0,
				"mutation_id": base64.RawURLEncoding.EncodeToString(
					[]byte("unknown-ontology")),
				"settings": map[string]any{
					"selected_ontology": map[string]any{
						"known":   false,
						"reading": string(ontology.OntologyUnresolved),
					},
				},
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			response := settingsRequest(
				t, handler, http.MethodPut, path, test.body, "owner",
				"http://example.test")
			if response.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, body = %s",
					response.Code, response.Body.String())
			}
		})
	}
}

func TestHTTPWorkspaceSettingsReportsIndeterminateCommit(t *testing.T) {
	now := time.Date(2026, 9, 5, 18, 0, 0, 0, time.UTC)
	authority, _ := auth.NewAuthorityWithClock(func() time.Time { return now })
	handler, err := webapi.NewAuthenticatedHandler(
		settingsStubService{},
		webapi.AuthenticatorFunc(func(*http.Request) (auth.Decision, error) {
			return settingsHTTPDecision(t, now, "owner"), nil
		}),
		authority.Binder(),
		"example.test",
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := handler.SetWorkspaceSettingsProvider(
		indeterminateSettingsProvider{},
	); err != nil {
		t.Fatal(err)
	}
	response := settingsRequest(
		t, handler, http.MethodPut,
		"/api/v1/workspaces/"+
			base64.RawURLEncoding.EncodeToString([]byte("indeterminate"))+
			"/settings",
		map[string]any{
			"expected_revision": 1,
			"mutation_id": base64.RawURLEncoding.EncodeToString(
				[]byte("retry-same-mutation")),
			"settings": map[string]any{},
		},
		"owner", "http://example.test")
	if response.Code != http.StatusServiceUnavailable ||
		response.Header().Get(webapi.CommitOutcomeHeader) !=
			webapi.CommitOutcomeIndeterminate {
		t.Fatalf("indeterminate status = %d, header = %q, body = %s",
			response.Code,
			response.Header().Get(webapi.CommitOutcomeHeader),
			response.Body.String())
	}
	var envelope struct {
		Code          shoal.ErrorCode `json:"code"`
		Indeterminate bool            `json:"indeterminate"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &envelope); err != nil {
		t.Fatal(err)
	}
	if envelope.Code != shoal.ErrorUnavailable || !envelope.Indeterminate {
		t.Fatalf("indeterminate envelope = %#v", envelope)
	}
}

func TestWorkspaceSettingsHTTPHandlerIsIndependentlyMountable(t *testing.T) {
	if _, err := webapi.NewWorkspaceSettingsHTTPHandler(
		webapi.WorkspaceSettingsHTTPConfig{},
	); !shoal.IsErrorCode(err, shoal.ErrorInvalidArgument) {
		t.Fatalf("nil provider error = %v, want invalid_argument", err)
	}

	now := time.Date(2026, 9, 6, 9, 0, 0, 0, time.UTC)
	authority, _ := auth.NewAuthorityWithClock(func() time.Time { return now })
	store, err := workspace.OpenDurableStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	provider, err := workspace.NewProvider(
		store, settingsProviderOptions(authority.Resolver(), now))
	if err != nil {
		t.Fatal(err)
	}
	decision := settingsHTTPDecision(t, now, "owner")
	ctx, err := authority.Binder().Bind(context.Background(), decision)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := provider.Update(ctx, "mounted", workspace.UpdateRequest{
		MutationID: "create-mounted",
	}); err != nil {
		t.Fatal(err)
	}
	handler, err := webapi.NewWorkspaceSettingsHTTPHandler(
		webapi.WorkspaceSettingsHTTPConfig{Provider: provider})
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(
		http.MethodGet,
		"http://example.test/api/v1/workspaces/"+
			base64.RawURLEncoding.EncodeToString([]byte("mounted"))+
			"/settings",
		nil,
	).WithContext(ctx)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("mounted settings status = %d, body = %s",
			response.Code, response.Body.String())
	}
}

func TestClampWorkspaceRequestLimits(t *testing.T) {
	got := webapi.ClampWorkspaceRequestLimits(
		workspace.Limits{
			GraphDepth:  2,
			GraphFanout: 10,
		},
		workspace.Limits{
			RetrievalTopK: 8,
			GraphDepth:    4,
			GraphFanout:   6,
			GraphNodes:    100,
			OutputBytes:   4096,
		},
		workspace.Limits{
			RetrievalTopK: 5,
			GraphDepth:    3,
			GraphFanout:   4,
			GraphNodes:    25,
			OutputBytes:   1024,
		},
	)
	want := workspace.Limits{
		RetrievalTopK: 5,
		GraphDepth:    2,
		GraphFanout:   4,
		GraphNodes:    25,
		OutputBytes:   1024,
	}
	if got != want {
		t.Fatalf("limits = %#v, want %#v", got, want)
	}
}

func settingsHTTPDecision(
	t *testing.T,
	now time.Time,
	subject shoal.ID,
) auth.Decision {
	t.Helper()
	decision, err := auth.NewDecision(auth.DecisionConfig{
		Subject: subject, Actor: "actor",
		AuthorizationDomain: []byte("domain"),
		AllowedOperations: []auth.Operation{
			auth.OperationRead,
			auth.OperationWorkspaceSettingsRead,
			auth.OperationWorkspaceSettingsWrite,
		},
		PermittedSourceIDs:    [][]byte{[]byte("source-a")},
		PermittedPolicyIDs:    [][]byte{[]byte("policy-a")},
		PolicyGeneration:      1,
		AuthenticationExpires: now.Add(time.Hour),
		RequestID:             "request",
	})
	if err != nil {
		t.Fatal(err)
	}
	return decision
}

func settingsRequest(
	t *testing.T,
	handler http.Handler,
	method, path string,
	body any,
	subject, origin string,
) *httptest.ResponseRecorder {
	return settingsRequestWithFetchSite(
		t, handler, method, path, body, subject, origin, "")
}

func settingsRequestWithFetchSite(
	t *testing.T,
	handler http.Handler,
	method, path string,
	body any,
	subject, origin, fetchSite string,
) *httptest.ResponseRecorder {
	t.Helper()
	var encoded []byte
	if body != nil {
		var err error
		encoded, err = json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
	}
	request := httptest.NewRequest(method, "http://example.test"+path, bytes.NewReader(encoded))
	request.Host = "example.test"
	request.Header.Set("X-Test-Subject", subject)
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	if origin != "" {
		request.Header.Set("Origin", origin)
	}
	if fetchSite != "" {
		request.Header.Set("Sec-Fetch-Site", fetchSite)
	}
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	return response
}

func cloneJSONMap(t *testing.T, value map[string]any) map[string]any {
	t.Helper()
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	var result map[string]any
	if err := json.Unmarshal(encoded, &result); err != nil {
		t.Fatal(err)
	}
	return result
}
