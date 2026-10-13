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
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/phrocker/shoal-oss/pkg/explorer"
	"github.com/phrocker/shoal-oss/pkg/explorer/auth"
	"github.com/phrocker/shoal-oss/pkg/explorer/authorized"
	"github.com/phrocker/shoal-oss/pkg/explorer/webapi"
	"github.com/phrocker/shoal-oss/pkg/graph"
	"github.com/phrocker/shoal-oss/pkg/retrieval"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

const authnPrincipalHeader = "X-Test-Principal"

var (
	authnDomain        = []byte("authn-domain")
	authnSourceGranted = []byte("authn-source-granted")
	authnPolicyGranted = []byte("authn-policy-granted")
	authnSourceOther   = []byte("authn-source-other")
	authnPolicyOther   = []byte("authn-policy-other")
	authnAllOperations = []auth.Operation{
		auth.OperationIngest,
		auth.OperationList,
		auth.OperationRead,
		auth.OperationConnect,
		auth.OperationNeighborhood,
		auth.OperationRetrieve,
	}
)

type authnGenerationReader struct{}

func (authnGenerationReader) CurrentPolicyGeneration(
	ctx context.Context, domain []byte,
) (int64, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	if !bytes.Equal(domain, authnDomain) {
		return 0, nil
	}
	return 1, nil
}

type authnFixture struct {
	server     *httptest.Server
	documentID shoal.ID
	from       shoal.ID
	to         shoal.ID
	secret     string
}

// newAuthnFixture wires the decision-enforcing Explorer client behind the
// authenticated transport exactly as the command does, then ingests content
// that only a principal holding the workspace grants may observe.
func newAuthnFixture(t *testing.T) *authnFixture {
	t.Helper()
	corpus, err := explorer.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = corpus.Close() })
	authority := auth.NewAuthority()
	selector, err := authorized.NewStaticPolicySelector(
		authnSourceGranted, authnPolicyGranted)
	if err != nil {
		t.Fatal(err)
	}
	scorer, _ := any(corpus).(authorized.VectorScorer)
	client, err := authorized.NewClient(authorized.Config{
		Base:             corpus,
		VectorScorer:     scorer,
		Resolver:         authority.Resolver(),
		PolicySelector:   selector,
		PolicyStore:      authorized.NewMemoryPolicyStore(),
		GenerationReader: authnGenerationReader{},
		Clock:            time.Now,
	})
	if err != nil {
		t.Fatal(err)
	}
	service, err := webapi.NewEmbeddedService(client)
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewUnstartedServer(nil)
	handler, err := webapi.NewAuthenticatedHandler(
		service,
		webapi.AuthenticatorFunc(authnAuthenticate),
		authority.Binder(),
		server.Listener.Addr().String(),
	)
	if err != nil {
		server.Close()
		t.Fatal(err)
	}
	server.Config.Handler = handler
	server.Start()
	t.Cleanup(server.Close)

	fixture := &authnFixture{
		server: server,
		secret: "classified retrieval evidence",
	}
	ctx, err := authority.Binder().Bind(
		context.Background(), authnPrincipal(t, "granted"))
	if err != nil {
		t.Fatal(err)
	}
	first, err := client.Ingest(ctx, explorer.Source{
		URI:       "file:///granted.md",
		MediaType: explorer.MediaTypeMarkdown,
		Content:   "# Granted\n\n" + fixture.secret + "\n",
	})
	if err != nil {
		t.Fatal(err)
	}
	second, err := client.Ingest(ctx, explorer.Source{
		URI:       "file:///related.md",
		MediaType: explorer.MediaTypeMarkdown,
		Content:   "# Related\n\nThe related note cites the guide.\n",
	})
	if err != nil {
		t.Fatal(err)
	}
	firstView, err := client.Document(ctx, first.Document.ID, first.Revision.ID)
	if err != nil {
		t.Fatal(err)
	}
	secondView, err := client.Document(ctx, second.Document.ID, second.Revision.ID)
	if err != nil {
		t.Fatal(err)
	}
	fixture.documentID = first.Document.ID
	fixture.from = firstSpanID(t, firstView.Root)
	fixture.to = firstSpanID(t, secondView.Root)
	// Connect accepts only IDs in the caller's namespace (#699).
	edgeID, err := client.ApplicationEdgeID(ctx, "granted-edge")
	if err != nil {
		t.Fatal(err)
	}
	if err := client.Connect(ctx, graph.Edge{
		ID: edgeID, From: fixture.from, To: fixture.to,
		Type: "informs", Weight: 1,
	}); err != nil {
		t.Fatal(err)
	}
	return fixture
}

// authnAuthenticate mints a decision for the named test principal. An unknown
// or absent principal is an authentication failure.
func authnAuthenticate(request *http.Request) (auth.Decision, error) {
	name := request.Header.Get(authnPrincipalHeader)
	if name == "" {
		return auth.Decision{}, shoal.NewError(
			shoal.ErrorUnauthorized, "no credential presented")
	}
	return authnDecisionFor(name)
}

func authnDecisionFor(name string) (auth.Decision, error) {
	config := auth.DecisionConfig{
		Subject:               shoal.ID(name),
		Actor:                 shoal.ID(name + "-actor"),
		AuthorizationDomain:   authnDomain,
		AllowedOperations:     authnAllOperations,
		PermittedSourceIDs:    [][]byte{authnSourceGranted},
		PermittedPolicyIDs:    [][]byte{authnPolicyGranted},
		PolicyGeneration:      1,
		AuthenticationExpires: time.Now().Add(time.Hour),
		RequestID:             shoal.ID(name + "-request"),
	}
	switch name {
	case "granted":
	case "other-grant":
		config.PermittedSourceIDs = [][]byte{authnSourceOther}
		config.PermittedPolicyIDs = [][]byte{authnPolicyOther}
	case "no-retrieve":
		config.AllowedOperations = []auth.Operation{
			auth.OperationIngest,
			auth.OperationList,
			auth.OperationRead,
			auth.OperationConnect,
			auth.OperationNeighborhood,
		}
	case "no-ingest":
		config.AllowedOperations = []auth.Operation{
			auth.OperationList,
			auth.OperationRead,
			auth.OperationConnect,
			auth.OperationNeighborhood,
			auth.OperationRetrieve,
		}
	case "ingest-only":
		config.AllowedOperations = []auth.Operation{auth.OperationIngest}
	default:
		return auth.Decision{}, shoal.NewError(
			shoal.ErrorUnauthorized, "unknown principal")
	}
	return auth.NewDecision(config)
}

func authnPrincipal(t *testing.T, name string) auth.Decision {
	t.Helper()
	decision, err := authnDecisionFor(name)
	if err != nil {
		t.Fatal(err)
	}
	return decision
}

type authnResponse struct {
	status int
	body   string
}

type authnRoute struct {
	name   string
	method string
	path   string
	body   string
}

func (f *authnFixture) routes(t *testing.T) []authnRoute {
	t.Helper()
	return []authnRoute{
		{"meta", http.MethodGet, "/api/v1/meta", ""},
		{
			"documents", http.MethodPost, "/api/v1/documents",
			authnJSON(t, webapi.DocumentsRequest{
				Page: webapi.PageRequest{Limit: 10},
			}),
		},
		{
			"document", http.MethodPost, "/api/v1/document",
			authnJSON(t, webapi.DocumentRequest{DocumentID: f.documentID}),
		},
		{
			"retrieve", http.MethodPost, "/api/v1/retrieve",
			authnJSON(t, webapi.RetrievalRequest{
				Query: retrieval.Request{
					Text: "classified", TopK: 5,
					Modes: []retrieval.Mode{
						retrieval.ModeLexical, retrieval.ModeTree,
					},
				},
			}),
		},
		{
			"neighborhood", http.MethodPost, "/api/v1/neighborhood",
			authnJSON(t, webapi.NeighborhoodRequest{
				NodeIDs: []shoal.ID{f.from}, Depth: 1, Fanout: 4, MaxNodes: 8,
			}),
		},
		{
			"path", http.MethodPost, "/api/v1/path",
			authnJSON(t, webapi.PathRequest{
				From: f.from, To: f.to, MaxDepth: 1, Fanout: 4,
				EdgeTypes: []string{"informs"},
			}),
		},
	}
}

// publicRoutes are the request lines the transport serves without a trusted
// decision: the HTML shell, the static assets, and the non-secret auth-config
// endpoint. They are enumerated separately from routes so the denial test can
// assert they are reachable unauthenticated while every API route is not.
func (f *authnFixture) publicRoutes() []authnRoute {
	return []authnRoute{
		{"workspace", http.MethodGet, "/", ""},
		{"assets", http.MethodGet, "/assets/app.js", ""},
		{"auth-config", http.MethodGet, "/api/v1/auth-config", ""},
	}
}

func authnJSON(t *testing.T, value any) string {
	t.Helper()
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return string(encoded)
}

// authnWireID renders an identifier the way the transport does, so leakage
// checks compare against what a caller would actually receive.
func authnWireID(t *testing.T, id shoal.ID) string {
	t.Helper()
	if id == "" {
		t.Fatal("identifier is required")
	}
	return base64.RawURLEncoding.EncodeToString([]byte(id))
}

func (f *authnFixture) do(
	t *testing.T, method, path, principal, body string,
) authnResponse {
	t.Helper()
	var reader io.Reader
	if body != "" {
		reader = strings.NewReader(body)
	}
	request, err := http.NewRequest(method, f.server.URL+path, reader)
	if err != nil {
		t.Fatal(err)
	}
	if body != "" {
		request.Header.Set("Content-Type", "application/json")
	}
	request.Header.Set("X-Shoal-Workspace-Request", "1")
	if principal != "" {
		request.Header.Set(authnPrincipalHeader, principal)
	}
	return authnSend(t, request)
}

func (f *authnFixture) upload(t *testing.T, principal string) authnResponse {
	t.Helper()
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	part, err := writer.CreateFormFile("files", "upload.md")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := part.Write([]byte("# Upload\n\nnew content\n")); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	request, err := http.NewRequest(
		http.MethodPost, f.server.URL+"/api/v1/ingest", &body)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Content-Type", writer.FormDataContentType())
	request.Header.Set("X-Shoal-Workspace-Request", "1")
	if principal != "" {
		request.Header.Set(authnPrincipalHeader, principal)
	}
	return authnSend(t, request)
}

func authnSend(t *testing.T, request *http.Request) authnResponse {
	t.Helper()
	client := http.Client{Timeout: 10 * time.Second}
	response, err := client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	payload, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	return authnResponse{status: response.StatusCode, body: string(payload)}
}

// TestUnauthenticatedRequestsAreDenied proves that no route serves anything to
// a caller whose identity cannot be established.
// TestNilAuthenticatorAdapterIsRejectedNotPanicked proves the mandatory
// authentication constructor refuses an adapter that carries no function, and
// that the adapter itself denies rather than panics if one ever reaches a
// request. A typed nil is a non-nil interface, so a plain nil comparison would
// admit it and turn every request into a crash instead of the documented 401.
func TestNilAuthenticatorAdapterIsRejectedNotPanicked(t *testing.T) {
	authority := auth.NewAuthority()
	var absent webapi.AuthenticatorFunc
	if absent == nil {
		// Kept explicit: the value is nil as a func but not as an interface.
		t.Log("the adapter holds no function")
	}
	_, err := webapi.NewAuthenticatedHandler(
		nil, absent, authority.Binder(), "127.0.0.1:0")
	if !shoal.IsErrorCode(err, shoal.ErrorInvalidArgument) ||
		!strings.Contains(err.Error(), "authenticator") {
		t.Fatalf("nil authenticator adapter error = %v", err)
	}

	var absentBinder *authnAbsentBinder
	_, err = webapi.NewAuthenticatedHandler(
		nil,
		webapi.AuthenticatorFunc(authnAuthenticate),
		absentBinder,
		"127.0.0.1:0",
	)
	if !shoal.IsErrorCode(err, shoal.ErrorInvalidArgument) ||
		!strings.Contains(err.Error(), "binder") {
		t.Fatalf("nil binder error = %v", err)
	}

	request := httptest.NewRequest(http.MethodGet, "/api/v1/meta", nil)
	decision, err := absent.Authenticate(request)
	if !shoal.IsErrorCode(err, shoal.ErrorUnauthorized) {
		t.Fatalf("nil adapter authenticate error = %v", err)
	}
	if decision.Subject() != "" {
		t.Fatalf("nil adapter returned a subject: %q", decision.Subject())
	}
}

// authnAbsentBinder exists only to be used as a typed nil pointer.
type authnAbsentBinder struct{}

func (*authnAbsentBinder) Bind(
	ctx context.Context, _ auth.Decision,
) (context.Context, error) {
	return ctx, nil
}

func TestUnauthenticatedRequestsAreDenied(t *testing.T) {
	fixture := newAuthnFixture(t)
	for _, route := range fixture.routes(t) {
		t.Run(route.name, func(t *testing.T) {
			response := fixture.do(t, route.method, route.path, "", route.body)
			if response.status != http.StatusUnauthorized {
				t.Fatalf("status = %d body = %s", response.status, response.body)
			}
			fixture.assertDenied(t, response.body)
		})
	}
	// The shell, its assets and the non-secret auth-config endpoint are the only
	// routes reachable without a decision, so the browser can load and begin a
	// login. They must not be denied here, or a real authenticator makes the UI
	// itself unreachable.
	for _, route := range fixture.publicRoutes() {
		t.Run("public/"+route.name, func(t *testing.T) {
			response := fixture.do(t, route.method, route.path, "", route.body)
			if response.status != http.StatusOK {
				t.Fatalf("public route %s status = %d body = %s",
					route.path, response.status, response.body)
			}
		})
	}
	t.Run("ingest", func(t *testing.T) {
		response := fixture.upload(t, "")
		if response.status != http.StatusUnauthorized {
			t.Fatalf("status = %d body = %s", response.status, response.body)
		}
		fixture.assertDenied(t, response.body)
	})
	t.Run("unknown principal", func(t *testing.T) {
		response := fixture.do(
			t, http.MethodPost, "/api/v1/documents", "nobody",
			authnJSON(t, webapi.DocumentsRequest{
				Page: webapi.PageRequest{Limit: 10},
			}))
		if response.status != http.StatusUnauthorized {
			t.Fatalf("status = %d body = %s", response.status, response.body)
		}
		fixture.assertDenied(t, response.body)
	})
}

// TestAuthenticatedRequestsAreServed keeps the denial tests honest: the same
// routes succeed once a decision carrying the required grants is bound.
func TestAuthenticatedRequestsAreServed(t *testing.T) {
	fixture := newAuthnFixture(t)
	for _, route := range fixture.routes(t) {
		t.Run(route.name, func(t *testing.T) {
			response := fixture.do(
				t, route.method, route.path, "granted", route.body)
			if response.status != http.StatusOK {
				t.Fatalf("status = %d body = %s", response.status, response.body)
			}
		})
	}
	// The public routes serve identically whether or not a principal is bound.
	for _, route := range fixture.publicRoutes() {
		t.Run("public/"+route.name, func(t *testing.T) {
			response := fixture.do(
				t, route.method, route.path, "granted", route.body)
			if response.status != http.StatusOK {
				t.Fatalf("status = %d body = %s", response.status, response.body)
			}
		})
	}
	documents := fixture.do(
		t, http.MethodPost, "/api/v1/documents", "granted",
		authnJSON(t, webapi.DocumentsRequest{Page: webapi.PageRequest{Limit: 10}}))
	if !strings.Contains(documents.body, authnWireID(t, fixture.documentID)) {
		t.Fatalf(
			"granted principal cannot see its own document: %s", documents.body)
	}
}

// TestDecisionWithoutGrantIsDeniedNotServed proves that authentication alone
// confers nothing: each missing grant denies the exact operation.
func TestDecisionWithoutGrantIsDeniedNotServed(t *testing.T) {
	fixture := newAuthnFixture(t)

	documents := fixture.do(
		t, http.MethodPost, "/api/v1/documents", "other-grant",
		authnJSON(t, webapi.DocumentsRequest{Page: webapi.PageRequest{Limit: 10}}))
	if documents.status != http.StatusOK {
		t.Fatalf(
			"documents status = %d body = %s", documents.status, documents.body)
	}
	fixture.assertNoCorpusContent(t, documents.body)

	document := fixture.do(
		t, http.MethodPost, "/api/v1/document", "other-grant",
		authnJSON(t, webapi.DocumentRequest{DocumentID: fixture.documentID}))
	if document.status != http.StatusNotFound {
		t.Fatalf("document status = %d body = %s", document.status, document.body)
	}
	fixture.assertDenied(t, document.body)

	neighborhood := fixture.do(
		t, http.MethodPost, "/api/v1/neighborhood", "other-grant",
		authnJSON(t, webapi.NeighborhoodRequest{
			NodeIDs: []shoal.ID{fixture.from}, Depth: 1, Fanout: 4, MaxNodes: 8,
		}))
	if neighborhood.status == http.StatusOK {
		fixture.assertNoCorpusContent(t, neighborhood.body)
	}

	retrieve := fixture.do(
		t, http.MethodPost, "/api/v1/retrieve", "no-retrieve",
		authnJSON(t, webapi.RetrievalRequest{
			Query: retrieval.Request{Text: "classified", TopK: 5},
		}))
	if retrieve.status != http.StatusUnauthorized {
		t.Fatalf("retrieve status = %d body = %s", retrieve.status, retrieve.body)
	}
	fixture.assertDenied(t, retrieve.body)

	upload := fixture.upload(t, "no-ingest")
	if upload.status == http.StatusOK {
		t.Fatalf("ungranted principal ingested: %s", upload.body)
	}
}

// assertDenied requires a denied response to carry only the error envelope.
func (f *authnFixture) assertDenied(t *testing.T, body string) {
	t.Helper()
	f.assertNoCorpusContent(t, body)
	for _, forbidden := range []string{
		"\"documents\"", "\"snapshot\"", "\"retrieval\"",
		"\"neighborhood\"", "\"path\"", "<!doctype", "<html",
	} {
		if strings.Contains(strings.ToLower(body), strings.ToLower(forbidden)) {
			t.Fatalf("denied response leaked %q: %s", forbidden, body)
		}
	}
}

func (f *authnFixture) assertNoCorpusContent(t *testing.T, body string) {
	t.Helper()
	for _, forbidden := range []string{
		f.secret,
		authnWireID(t, f.documentID),
		authnWireID(t, f.from),
		authnWireID(t, f.to),
	} {
		if strings.Contains(body, forbidden) {
			t.Fatalf("response leaked %q: %s", forbidden, body)
		}
	}
}
