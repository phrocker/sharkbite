// Licensed under the Apache License, Version 2.0. See LICENSE and NOTICE.
package webapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	decisionapi "github.com/phrocker/shoal-oss/pkg/decision/api"
	"github.com/phrocker/shoal-oss/pkg/explorer/auth"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

// learningStub records calls and answers with fixed JSON or an error.
type learningStub struct {
	calls  atomic.Int32
	last   string
	body   string
	err    error
	delay  time.Duration
	actor  string
	reply_ json.RawMessage
}

func (s *learningStub) reply(name string, body json.RawMessage) (json.RawMessage, error) {
	s.calls.Add(1)
	s.last, s.body = name, string(body)
	if s.delay > 0 {
		time.Sleep(s.delay)
	}
	if s.err != nil {
		return nil, s.err
	}
	if s.reply_ != nil {
		return s.reply_, nil
	}
	return json.RawMessage(`{"ok":"` + name + `"}`), nil
}

func (s *learningStub) SubmitPredictions(ctx context.Context, pin string, r json.RawMessage) (json.RawMessage, error) {
	if actor, ok := decisionapi.LearningActorFrom(ctx); ok {
		s.actor = actor.Actor
	}
	return s.reply("predictions:"+pin, r)
}
func (s *learningStub) Datasets(context.Context) (json.RawMessage, error) {
	return s.reply("datasets", nil)
}
func (s *learningStub) RequestExport(_ context.Context, id string) (json.RawMessage, error) {
	return s.reply("export:"+id, nil)
}
func (s *learningStub) Jobs(context.Context) (json.RawMessage, error) { return s.reply("jobs", nil) }
func (s *learningStub) Job(_ context.Context, pin string) (json.RawMessage, error) {
	return s.reply("job:"+pin, nil)
}
func (s *learningStub) SubmitJob(_ context.Context, m json.RawMessage) (json.RawMessage, error) {
	return s.reply("submit-job", m)
}
func (s *learningStub) SubmitReceipt(_ context.Context, pin string, r json.RawMessage) (json.RawMessage, error) {
	return s.reply("receipt:"+pin, r)
}
func (s *learningStub) Candidates(context.Context) (json.RawMessage, error) {
	return s.reply("candidates", nil)
}
func (s *learningStub) SubmitCandidate(_ context.Context, b json.RawMessage) (json.RawMessage, error) {
	return s.reply("submit-candidate", b)
}
func (s *learningStub) Evaluations(_ context.Context, id string) (json.RawMessage, error) {
	return s.reply("evaluations:"+id, nil)
}
func (s *learningStub) SubmitEvaluation(_ context.Context, b json.RawMessage) (json.RawMessage, error) {
	return s.reply("submit-evaluation", b)
}
func (s *learningStub) Releases(context.Context) (json.RawMessage, error) {
	return s.reply("releases", nil)
}
func (s *learningStub) Promote(ctx context.Context, b json.RawMessage) (json.RawMessage, error) {
	s.actor = ""
	if actor, ok := decisionapi.LearningActorFrom(ctx); ok {
		s.actor = actor.Actor
	}
	return s.reply("promote", b)
}
func (s *learningStub) Rollback(_ context.Context, b json.RawMessage) (json.RawMessage, error) {
	return s.reply("rollback", b)
}
func (s *learningStub) Evidence(_ context.Context, kind string) (json.RawMessage, error) {
	return s.reply("evidence:"+kind, nil)
}
func (s *learningStub) Bundles(context.Context) (json.RawMessage, error) {
	return s.reply("bundles", nil)
}
func (s *learningStub) ExportBundle(_ context.Context, b json.RawMessage) (json.RawMessage, error) {
	return s.reply("export-bundle", b)
}
func (s *learningStub) ImportBundle(_ context.Context, b json.RawMessage) (json.RawMessage, error) {
	return s.reply("import-bundle", b)
}

func learningFixture(t *testing.T) (*Handler, *learningStub, *auth.Authority, auth.Decision) {
	return learningFixtureWith(t, []auth.Operation{auth.OperationRead, auth.OperationLearningWrite}, time.Hour)
}

func learningFixtureWith(t *testing.T, operations []auth.Operation, lifetime time.Duration) (*Handler, *learningStub, *auth.Authority, auth.Decision) {
	t.Helper()
	a := auth.NewAuthority()
	d, e := auth.NewDecision(auth.DecisionConfig{Subject: "subject", Actor: "actor", AuthorizationDomain: []byte("domain"), AllowedOperations: operations, PermittedSourceIDs: [][]byte{[]byte("source")}, PermittedPolicyIDs: [][]byte{[]byte("policy")}, PolicyGeneration: 1, AuthenticationExpires: time.Now().Add(lifetime), RequestID: "request"})
	if e != nil {
		t.Fatal(e)
	}
	h, e := NewAuthenticatedHandler(&stubWorkspaceService{}, AuthenticatorFunc(func(r *http.Request) (auth.Decision, error) {
		if r.Header.Get("Authorization") == "" {
			return auth.Decision{}, authenticationDenied()
		}
		return d, nil
	}), a.Binder(), "example.test")
	if e != nil {
		t.Fatal(e)
	}
	stub := &learningStub{}
	if e := h.MountLearningSurface(LearningSurface{Resolver: a.Resolver(), Provider: stub}); e != nil {
		t.Fatal(e)
	}
	return h, stub, a, d
}

func learningRequest(method, path, body string) *http.Request {
	var r *http.Request
	if body == "" {
		r = httptest.NewRequest(method, "http://example.test"+path, nil)
	} else {
		r = httptest.NewRequest(method, "http://example.test"+path, strings.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
	}
	r.Header.Set("Authorization", "Bearer token")
	return r
}

func TestLearningRoutesDispatchToTheProvider(t *testing.T) {
	h, stub, _, _ := learningFixture(t)
	cases := []struct {
		method, path, body, want string
	}{
		{"GET", "/api/v1/learning/datasets", "", "datasets"},
		{"POST", "/api/v1/learning/datasets/exports", `{"cohort_id":"c1"}`, "export:c1"},
		{"GET", "/api/v1/learning/jobs", "", "jobs"},
		{"POST", "/api/v1/learning/jobs", `{"schema":1}`, "submit-job"},
		{"GET", "/api/v1/learning/jobs/pin1", "", "job:pin1"},
		{"POST", "/api/v1/learning/jobs/pin1/predictions", `{"schema":1}`, "predictions:pin1"},
		{"POST", "/api/v1/learning/jobs/pin1/receipt", `{"schema":1}`, "receipt:pin1"},
		{"GET", "/api/v1/learning/candidates", "", "candidates"},
		{"POST", "/api/v1/learning/candidates", `{"candidate":{}}`, "submit-candidate"},
		{"GET", "/api/v1/learning/candidates/c1/evaluations", "", "evaluations:c1"},
		{"POST", "/api/v1/learning/evaluations", `{"schema":1}`, "submit-evaluation"},
		{"GET", "/api/v1/learning/releases", "", "releases"},
		{"POST", "/api/v1/learning/releases/promote", `{"candidate_id":"c"}`, "promote"},
		{"POST", "/api/v1/learning/releases/rollback", `{"prior_release_id":"r"}`, "rollback"},
		{"GET", "/api/v1/learning/evidence/quality", "", "evidence:quality"},
		{"GET", "/api/v1/learning/bundles", "", "bundles"},
		{"POST", "/api/v1/learning/bundles/export", `{"name":"b"}`, "export-bundle"},
		{"POST", "/api/v1/learning/bundles/import", `{"name":"b"}`, "import-bundle"},
	}
	for _, c := range cases {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, learningRequest(c.method, c.path, c.body))
		if w.Code != 200 || !strings.Contains(w.Body.String(), `"ok":"`+c.want+`"`) {
			t.Fatalf("%s %s: %d %s", c.method, c.path, w.Code, w.Body.String())
		}
		if c.body != "" && !strings.HasPrefix(c.want, "export:") && stub.body != c.body {
			t.Fatalf("%s: body %q forwarded as %q", c.path, c.body, stub.body)
		}
		if w.Header().Get("Cache-Control") != "no-store" {
			t.Fatal("learning responses must not be cached")
		}
	}
	// Every write carries the authenticated actor, never a name from the body.
	if stub.actor != "actor" {
		t.Fatalf("actor bound as %q", stub.actor)
	}
	var meta struct {
		Capabilities map[string]bool `json:"capabilities"`
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, learningRequest("GET", "/api/v1/meta", ""))
	if err := json.Unmarshal(w.Body.Bytes(), &meta); err != nil || !meta.Capabilities["decision_learning"] {
		t.Fatalf("capability not advertised: %d %s", w.Code, w.Body.String())
	}
}

// A principal without learning_write may read everything and write nothing;
// the refusal is a 403 that names the operation, and the provider is never
// reached.
func TestLearningWritesRequireTheOperation(t *testing.T) {
	h, stub, _, _ := learningFixtureWith(t, []auth.Operation{auth.OperationRead}, time.Hour)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, learningRequest("GET", "/api/v1/learning/releases", ""))
	if w.Code != 200 {
		t.Fatalf("read refused: %d %s", w.Code, w.Body.String())
	}
	before := stub.calls.Load()
	for _, path := range []string{"/api/v1/learning/releases/promote", "/api/v1/learning/jobs", "/api/v1/learning/bundles/import", "/api/v1/learning/datasets/exports"} {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, learningRequest("POST", path, `{"owner":"anyone"}`))
		if w.Code != 403 || !strings.Contains(w.Body.String(), "learning_write") {
			t.Fatalf("%s: %d %s", path, w.Code, w.Body.String())
		}
	}
	if stub.calls.Load() != before {
		t.Fatal("provider reached by an unauthorized write")
	}
}

// A decision that expires while the provider works refuses the response:
// the fingerprint after the call must be the fingerprint before it.
func TestLearningRefusesResponseWhenAuthenticationExpiresMidCall(t *testing.T) {
	h, stub, _, _ := learningFixtureWith(t, []auth.Operation{auth.OperationRead, auth.OperationLearningWrite}, 80*time.Millisecond)
	stub.delay = 150 * time.Millisecond
	w := httptest.NewRecorder()
	h.ServeHTTP(w, learningRequest("GET", "/api/v1/learning/releases", ""))
	if w.Code != 401 || strings.Contains(w.Body.String(), `"ok"`) {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
}

// An oversized or invalid provider response is a permanent failure the
// console must not describe as the service being unavailable.
func TestLearningOversizedResponseIsNotAnOutage(t *testing.T) {
	h, stub, _, _ := learningFixture(t)
	stub.reply_ = json.RawMessage(`{"big":"` + strings.Repeat("x", 65<<20) + `"}`)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, learningRequest("GET", "/api/v1/learning/jobs", ""))
	if w.Code != 500 || !strings.Contains(w.Body.String(), "exceeds this host's response limit") || strings.Contains(w.Body.String(), "unavailable") {
		t.Fatalf("%d %s", w.Code, w.Body.String()[:min(200, w.Body.Len())])
	}
}

func TestLearningRoutesRefuseWhatTheyMust(t *testing.T) {
	h, stub, _, _ := learningFixture(t)
	cases := []struct {
		name, method, path, body string
		headers                  map[string]string
		status                   int
	}{
		{"unknown route", "GET", "/api/v1/learning/nothing", "", nil, 404},
		{"delete", "DELETE", "/api/v1/learning/datasets", "", nil, 405},
		{"query", "GET", "/api/v1/learning/datasets?x=1", "", nil, 400},
		{"cross-site", "GET", "/api/v1/learning/datasets", "", map[string]string{"Sec-Fetch-Site": "cross-site"}, 403},
		{"foreign origin", "GET", "/api/v1/learning/datasets", "", map[string]string{"Origin": "https://evil.test"}, 403},
		{"not an object", "POST", "/api/v1/learning/jobs", `[1]`, nil, 400},
		{"trailing data", "POST", "/api/v1/learning/jobs", `{} {}`, nil, 400},
		{"wrong content type", "POST", "/api/v1/learning/jobs", `{}`, map[string]string{"Content-Type": "text/plain"}, 400},
		{"oversized", "POST", "/api/v1/learning/jobs", `{"a":"` + strings.Repeat("x", 1<<20) + `"}`, nil, 400},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			before := stub.calls.Load()
			r := learningRequest(c.method, c.path, c.body)
			for k, v := range c.headers {
				r.Header.Set(k, v)
			}
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			if w.Code != c.status {
				t.Fatalf("got %d %s, want %d", w.Code, w.Body.String(), c.status)
			}
			if stub.calls.Load() != before {
				t.Fatal("provider reached by a refused request")
			}
		})
	}
	// A provider refusal reaches the caller verbatim, with its code.
	stub.err = shoal.NewError(shoal.ErrorConflict, "promotion requires its evaluation: evaluation x is not recorded")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, learningRequest("POST", "/api/v1/learning/releases/promote", `{"candidate_id":"c","owner":"o"}`))
	if w.Code != 409 || !strings.Contains(w.Body.String(), `"message":"promotion requires its evaluation: evaluation x is not recorded"`) {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
	stub.err = shoal.NewError(shoal.ErrorUnavailable, "no dataset exporter is mounted on this host")
	w = httptest.NewRecorder()
	h.ServeHTTP(w, learningRequest("POST", "/api/v1/learning/datasets/exports", `{"cohort_id":"c"}`))
	if w.Code != 503 || !strings.Contains(w.Body.String(), "no dataset exporter") {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
}

func TestLearningSurfaceIsAllOrNothing(t *testing.T) {
	a := auth.NewAuthority()
	anonymous, err := NewHandler(&stubWorkspaceService{}, "example.test")
	if err != nil {
		t.Fatal(err)
	}
	if err := anonymous.MountLearningSurface(LearningSurface{Resolver: a.Resolver(), Provider: &learningStub{}}); err == nil {
		t.Fatal("anonymous handler accepted a learning surface")
	}
	h, stub, _, _ := learningFixture(t)
	if err := h.MountLearningSurface(LearningSurface{Resolver: a.Resolver(), Provider: stub}); err == nil {
		t.Fatal("double mount accepted")
	}
	d, _ := auth.NewDecision(auth.DecisionConfig{Subject: "subject", Actor: "actor", AuthorizationDomain: []byte("domain"), AllowedOperations: []auth.Operation{auth.OperationRead}, PermittedSourceIDs: [][]byte{[]byte("source")}, PermittedPolicyIDs: [][]byte{[]byte("policy")}, PolicyGeneration: 1, AuthenticationExpires: time.Now().Add(time.Hour), RequestID: "request"})
	bare, _ := NewAuthenticatedHandler(&stubWorkspaceService{}, AuthenticatorFunc(func(r *http.Request) (auth.Decision, error) {
		if r.Header.Get("Authorization") == "" {
			return auth.Decision{}, authenticationDenied()
		}
		return d, nil
	}), a.Binder(), "example.test")
	for _, surface := range []LearningSurface{{}, {Resolver: a.Resolver()}, {Provider: &learningStub{}}, {Resolver: a.Resolver(), Provider: (*learningStub)(nil)}} {
		if err := bare.MountLearningSurface(surface); err == nil {
			t.Fatalf("incomplete surface %+v mounted", surface)
		}
	}
	// With nothing mounted, every learning route is unavailable: 401 unauthenticated, 404 authenticated.
	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "http://example.test/api/v1/learning/datasets", nil)
	bare.ServeHTTP(w, r)
	if w.Code != 401 {
		t.Fatalf("unauthenticated: %d", w.Code)
	}
	w = httptest.NewRecorder()
	bare.ServeHTTP(w, learningRequest("GET", "/api/v1/learning/datasets", ""))
	if w.Code != 404 {
		t.Fatalf("unmounted: %d", w.Code)
	}
	w = httptest.NewRecorder()
	bare.ServeHTTP(w, learningRequest("GET", "/api/v1/meta", ""))
	if strings.Contains(w.Body.String(), `"decision_learning":true`) {
		t.Fatal("unmounted surface advertised")
	}
	// Unauthenticated requests to a mounted surface still receive 401.
	mounted, _, _, _ := learningFixture(t)
	w = httptest.NewRecorder()
	mounted.ServeHTTP(w, httptest.NewRequest("GET", "http://example.test/api/v1/learning/datasets", nil))
	if w.Code != 401 || !strings.Contains(w.Header().Get("WWW-Authenticate"), "Bearer") {
		t.Fatalf("mounted unauthenticated: %d", w.Code)
	}
}
