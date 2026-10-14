// Licensed under the Apache License, Version 2.0. See LICENSE and NOTICE.
package webapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	decisionapi "github.com/phrocker/shoal-oss/pkg/decision/api"
	"github.com/phrocker/shoal-oss/pkg/explorer/auth"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

type decisionHTTPStub struct {
	calls  int
	id     shoal.ID
	key    []byte
	reply  decisionapi.Response
	err    error
	during func()
}

func (p *decisionHTTPStub) Evaluate(_ context.Context, id shoal.ID, key []byte) (decisionapi.Response, error) {
	p.calls++
	p.id = id
	p.key = append([]byte{}, key...)
	if p.during != nil {
		p.during()
	}
	return p.reply, p.err
}
func (p *decisionHTTPStub) Read(ctx context.Context, id shoal.ID, key []byte) (decisionapi.Response, error) {
	return p.Evaluate(ctx, id, key)
}
func decisionHTTPReply(id shoal.ID) decisionapi.Response {
	now := time.Now().UTC()
	return decisionapi.Response{Schema: 1, Receipt: decisionapi.Receipt{ID: "receipt:" + strings.Repeat("a", 64), Version: 1, State: "pending", RequestID: decisionapi.EncodeID(id), TaskID: decisionapi.EncodeID("task"), PictureID: decisionapi.EncodeID("picture"), PredictorID: decisionapi.EncodeID("predictor"), CreatedAt: now, UpdatedAt: now, LeaseUntil: now.Add(time.Minute)}}
}
func decisionHTTPFixture(t *testing.T) (*Handler, *decisionHTTPStub, *auth.Authority, auth.Decision) {
	t.Helper()
	a := auth.NewAuthority()
	d, e := auth.NewDecision(auth.DecisionConfig{Subject: "subject", Actor: "actor", AuthorizationDomain: []byte("domain"), AllowedOperations: []auth.Operation{auth.OperationInvoke, auth.OperationRead}, PermittedSourceIDs: [][]byte{[]byte("source")}, PermittedPolicyIDs: [][]byte{[]byte("policy")}, PolicyGeneration: 1, AuthenticationExpires: time.Now().UTC().Add(time.Hour), RequestID: "request"})
	if e != nil {
		t.Fatal(e)
	}
	h, e := NewAuthenticatedHandler(&stubWorkspaceService{}, AuthenticatorFunc(func(*http.Request) (auth.Decision, error) { return d, nil }), a.Binder(), "example.test")
	if e != nil {
		t.Fatal(e)
	}
	p := &decisionHTTPStub{reply: decisionHTTPReply("request")}
	if e = h.MountDecisions(p, a.Resolver()); e != nil {
		t.Fatal(e)
	}
	return h, p, a, d
}
func decisionHTTPRequest(method string) *http.Request {
	path := DecisionsRoute
	body := ""
	if method == http.MethodPost {
		body = `{"request_id":"` + decisionapi.EncodeID("request") + `"}`
	} else {
		path += "/" + decisionapi.EncodeID("request")
	}
	r := httptest.NewRequest(method, "http://example.test"+path, strings.NewReader(body))
	r.Header.Set("Idempotency-Key", decisionapi.EncodeKey([]byte{'k', 0, 255}))
	if method == http.MethodPost {
		r.Header.Set("Content-Type", "application/json")
	}
	return r
}
func TestDecisionHTTPMountRequiresTrustedAuthentication(t *testing.T) {
	h, p, a, d := decisionHTTPFixture(t)
	anonymous, e := NewHandler(&stubWorkspaceService{}, "example.test")
	if e != nil {
		t.Fatal(e)
	}
	if e = anonymous.MountDecisions(p, a.Resolver()); e == nil {
		t.Fatal("anonymous mount accepted")
	}
	if e = h.MountDecisions(p, a.Resolver()); e == nil {
		t.Fatal("duplicate mount accepted")
	}
	var nilProvider *decisionHTTPStub
	if _, e = NewDecisionHTTPHandler(nilProvider, a.Resolver()); e == nil {
		t.Fatal("typed nil provider accepted")
	}
	if _, e = NewDecisionHTTPHandler(p, nil); e == nil {
		t.Fatal("nil resolver accepted")
	}
	standalone, e := NewDecisionHTTPHandler(p, a.Resolver())
	if e != nil {
		t.Fatal(e)
	}
	r := decisionHTTPRequest(http.MethodPost)
	w := httptest.NewRecorder()
	standalone.ServeHTTP(w, r)
	if w.Code != 401 || p.calls != 0 {
		t.Fatal("unbound context reached provider")
	}
	other := auth.NewAuthority()
	ctx, e := other.Binder().Bind(r.Context(), d)
	if e != nil {
		t.Fatal(e)
	}
	w = httptest.NewRecorder()
	standalone.ServeHTTP(w, r.WithContext(ctx))
	if w.Code != 401 || p.calls != 0 {
		t.Fatal("foreign binder context accepted")
	}
}
func TestDecisionHTTPPreservesOpaqueIDsAndPendingState(t *testing.T) {
	h, p, _, _ := decisionHTTPFixture(t)
	id := shoal.ID("r\x00/世界")
	p.reply = decisionHTTPReply(id)
	for _, method := range []string{http.MethodPost, http.MethodGet} {
		r := decisionHTTPRequest(method)
		if method == http.MethodPost {
			r.Body = http.NoBody
			r.Body = ioNopString(`{"request_id":"` + decisionapi.EncodeID(id) + `"}`)
		} else {
			r.URL.Path = DecisionsRoute + "/" + decisionapi.EncodeID(id)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != 202 {
			t.Fatalf("status%d %s", w.Code, w.Body.String())
		}
		if p.id != id || !bytes.Equal(p.key, []byte{'k', 0, 255}) {
			t.Fatal("opaque ID/key changed")
		}
		if w.Header().Get("Location") != "" || w.Header().Get("Cache-Control") != "no-store" {
			t.Fatal("redirect or cacheable result")
		}
		if strings.Contains(w.Body.String(), "claim") {
			t.Fatal("claim material leaked")
		}
	}
}

type stringBody struct{ *strings.Reader }

func (stringBody) Close() error       { return nil }
func ioNopString(s string) stringBody { return stringBody{strings.NewReader(s)} }
func TestDecisionHTTPRejectsAmbiguousRequestsBeforeProvider(t *testing.T) {
	h, p, _, _ := decisionHTTPFixture(t)
	cases := map[string]func(*http.Request){
		"duplicate":       func(r *http.Request) { r.Body = ioNopString(`{"request_id":"cg","request_id":"cg"}`) },
		"casealias":       func(r *http.Request) { r.Body = ioNopString(`{"Request_ID":"cg"}`) },
		"null":            func(r *http.Request) { r.Body = ioNopString(`{"request_id":null}`) },
		"missing":         func(r *http.Request) { r.Body = ioNopString(`{}`) },
		"unknown":         func(r *http.Request) { r.Body = ioNopString(`{"request_id":"cg","principal":"admin"}`) },
		"tail":            func(r *http.Request) { r.Body = ioNopString(`{"request_id":"cg"}{}`) },
		"utf8":            func(r *http.Request) { r.Body = ioNopString("{\"request_id\":\"\xff\"}") },
		"surrogate":       func(r *http.Request) { r.Body = ioNopString(`{"request_id":"\ud800"}`) },
		"oversize":        func(r *http.Request) { r.Body = ioNopString(strings.Repeat(" ", decisionBodyLimit+1)) },
		"keyduplicate":    func(r *http.Request) { r.Header.Add("Idempotency-Key", r.Header.Get("Idempotency-Key")) },
		"keymissing":      func(r *http.Request) { r.Header.Del("Idempotency-Key") },
		"keynoncanonical": func(r *http.Request) { r.Header.Set("Idempotency-Key", "aw==") },
		"query":           func(r *http.Request) { r.URL.RawQuery = "principal=admin" },
		"rawpath":         func(r *http.Request) { r.URL.RawPath = "/api/v1/%64ecisions" },
		"contenttype":     func(r *http.Request) { r.Header.Set("Content-Type", "text/plain") },
		"compression":     func(r *http.Request) { r.Header.Set("Content-Encoding", "gzip") },
		"getbody": func(r *http.Request) {
			r.Method = http.MethodGet
			r.URL.Path = DecisionsRoute + "/" + decisionapi.EncodeID("request")
		},
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			r := decisionHTTPRequest(http.MethodPost)
			mutate(r)
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			if w.Code != 400 || p.calls != 0 {
				t.Fatalf("status%d calls%d %s", w.Code, p.calls, w.Body.String())
			}
		})
	}
}
func TestDecisionHTTPHostOriginAndMethodGates(t *testing.T) {
	h, p, _, _ := decisionHTTPFixture(t)
	for _, tc := range []struct {
		name   string
		change func(*http.Request)
		status int
	}{{"host", func(r *http.Request) { r.Host = "evil.test" }, 421}, {"origin", func(r *http.Request) { r.Header.Set("Origin", "http://evil.test") }, 403}, {"fetch", func(r *http.Request) { r.Header.Set("Sec-Fetch-Site", "cross-site") }, 403}, {"method", func(r *http.Request) { r.Method = http.MethodDelete }, 405}} {
		t.Run(tc.name, func(t *testing.T) {
			r := decisionHTTPRequest(http.MethodPost)
			tc.change(r)
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			if w.Code != tc.status || p.calls != 0 {
				t.Fatalf("status%d calls%d", w.Code, p.calls)
			}
		})
	}
	r := decisionHTTPRequest(http.MethodPost)
	r.Header.Set("Origin", "http://example.test")
	r.Header.Set("Sec-Fetch-Site", "same-origin")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != 202 {
		t.Fatal(w.Code, w.Body.String())
	}
}
func TestDecisionHTTPErrorSanitizationAndCommitUncertainty(t *testing.T) {
	h, p, _, _ := decisionHTTPFixture(t)
	for _, tc := range []struct {
		err       error
		status    int
		uncertain bool
	}{{errors.New("private token and storage details"), 503, true}, {shoal.NewError(shoal.ErrorNotFound, "secret source"), 503, true}, {shoal.NewError(shoal.ErrorConflict, "secret key"), 503, true}, {decisionapi.ErrIndeterminate, 503, true}, {context.Canceled, 503, true}} {
		p.err = tc.err
		r := decisionHTTPRequest(http.MethodPost)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != tc.status || strings.Contains(w.Body.String(), "secret") || strings.Contains(w.Body.String(), "private") {
			t.Fatal(w.Code, w.Body.String())
		}
		if (w.Header().Get(CommitOutcomeHeader) == CommitOutcomeIndeterminate) != tc.uncertain {
			t.Fatal("uncertainty header mismatch")
		}
	}
	p.err = context.Canceled
	w := httptest.NewRecorder()
	h.ServeHTTP(w, decisionHTTPRequest(http.MethodGet))
	if w.Header().Get(CommitOutcomeHeader) != "" {
		t.Fatal("read-only cancellation marked commit")
	}
}
func TestDecisionHTTPPostProviderCancellationWithholdsReceipt(t *testing.T) {
	h, p, _, _ := decisionHTTPFixture(t)
	for _, method := range []string{http.MethodPost, http.MethodGet} {
		r := decisionHTTPRequest(method)
		ctx, cancel := context.WithCancel(r.Context())
		p.during = cancel
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r.WithContext(ctx))
		if strings.Contains(w.Body.String(), p.reply.Receipt.ID) {
			t.Fatal("canceled request disclosed receipt")
		}
		if (w.Header().Get(CommitOutcomeHeader) == CommitOutcomeIndeterminate) != (method == http.MethodPost) {
			t.Fatal("cancellation uncertainty mismatch")
		}
	}
}
func TestDecisionHTTPWorkspaceOverflowPreservesUncertainty(t *testing.T) {
	_, p, a, d := decisionHTTPFixture(t)
	standalone, e := NewDecisionHTTPHandler(p, a.Resolver())
	if e != nil {
		t.Fatal(e)
	}
	r := decisionHTTPRequest(http.MethodPost)
	ctx, e := a.Binder().Bind(r.Context(), d)
	if e != nil {
		t.Fatal(e)
	}
	w := httptest.NewRecorder()
	wrapped := workspaceResponseWriter{ResponseWriter: w, maxResponseBytes: 128, indeterminateOnOverflow: requestMayCommit(r.Method, r.URL.Path)}
	standalone.ServeHTTP(wrapped, r.WithContext(ctx))
	if w.Code != 503 || w.Header().Get(CommitOutcomeHeader) != CommitOutcomeIndeterminate || w.Body.Len() > 128 {
		t.Fatal(w.Code, w.Header(), w.Body.Len())
	}
	if requestMayCommit(http.MethodGet, DecisionsRoute+"/id") {
		t.Fatal("read route may commit")
	}
}
func TestDecisionHTTPRejectsSubstitutedResponse(t *testing.T) {
	h, p, _, _ := decisionHTTPFixture(t)
	p.reply.Receipt.RequestID = decisionapi.EncodeID("other")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, decisionHTTPRequest(http.MethodPost))
	var body map[string]any
	json.Unmarshal(w.Body.Bytes(), &body)
	if w.Code != 503 || body["indeterminate"] != true || strings.Contains(w.Body.String(), "receipt:") {
		t.Fatal(w.Code, w.Body.String())
	}
}

type decisionResolverFunc func(context.Context) (auth.Decision, error)

func (f decisionResolverFunc) Resolve(ctx context.Context) (auth.Decision, error) { return f(ctx) }
func TestDecisionHTTPRevalidatesTrustedContextAfterProvider(t *testing.T) {
	_, p, a, d := decisionHTTPFixture(t)
	for _, method := range []string{http.MethodPost, http.MethodGet} {
		calls := 0
		resolver := decisionResolverFunc(func(ctx context.Context) (auth.Decision, error) {
			calls++
			if calls > 1 {
				return auth.Decision{}, auth.ObjectNotFound()
			}
			return a.Resolver().Resolve(ctx)
		})
		handler, e := NewDecisionHTTPHandler(p, resolver)
		if e != nil {
			t.Fatal(e)
		}
		r := decisionHTTPRequest(method)
		ctx, e := a.Binder().Bind(r.Context(), d)
		if e != nil {
			t.Fatal(e)
		}
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r.WithContext(ctx))
		want := 401
		if method == http.MethodPost {
			want = 503
		}
		if w.Code != want || strings.Contains(w.Body.String(), "receipt:") {
			t.Fatalf("revocation disclosed response %d %s", w.Code, w.Body.String())
		}
	}
}
func TestDecisionHTTPCommittedReceiptReturns200(t *testing.T) {
	h, p, _, _ := decisionHTTPFixture(t)
	r := &p.reply.Receipt
	r.State = "committed"
	r.PredictionID = decisionapi.EncodeID("prediction")
	r.UpdatedAt = r.CreatedAt.Add(time.Second)
	r.Result = &decisionapi.Result{RequestID: r.RequestID, PredictorID: r.PredictorID, EffectiveDevice: "cpu", Status: "completed", CompletedAt: r.UpdatedAt, Answers: []decisionapi.Answer{{SubjectID: decisionapi.EncodeID("subject"), QuestionID: decisionapi.EncodeID("priority"), Status: "answered", Label: "retain"}}}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, decisionHTTPRequest(http.MethodPost))
	if w.Code != 200 || w.Header().Get(CommitOutcomeHeader) != "" {
		t.Fatal(w.Code, w.Body.String())
	}
}

// A source can be revoked after a durable commit while the request's immutable
// authorization fingerprint remains unchanged. The transport cannot classify a
// provider NotFound as proof that nothing committed.
func TestDecisionHTTPCommitThenNotFoundRemainsIndeterminateThroughSDK(t *testing.T) {
	h, p, _, _ := decisionHTTPFixture(t)
	commits := 0
	p.during = func() { commits++ }
	p.err = shoal.NewError(shoal.ErrorNotFound, "post-commit source revoked")
	client, e := decisionapi.NewClient(decisionapi.Config{BaseURL: "http://example.test", HTTPClient: &http.Client{Transport: decisionLoopbackTransport{h}}, Token: func(context.Context) (string, error) { return "test-token", nil }})
	if e != nil {
		t.Fatal(e)
	}
	result, e := client.Evaluate(context.Background(), "request", []byte("same-key"))
	if !errors.Is(e, decisionapi.ErrIndeterminate) || commits != 1 || result.Receipt.ID != "" {
		t.Fatalf("commit followed by denial lost uncertainty: commits=%d result=%+v err=%v", commits, result, e)
	}
	var httpErr *decisionapi.HTTPError
	if !errors.As(e, &httpErr) || httpErr.Status != 503 || !httpErr.Indeterminate {
		t.Fatalf("missing transport uncertainty: %v", e)
	}
	if strings.Contains(e.Error(), "revoked") {
		t.Fatal("provider details disclosed")
	}
	// Read is side-effect free; an ordinary source denial stays a normal 404.
	_, e = client.Read(context.Background(), "request", []byte("same-key"))
	if errors.Is(e, decisionapi.ErrIndeterminate) || !errors.As(e, &httpErr) || httpErr.Status != 404 {
		t.Fatalf("read-only denial became mutation uncertainty: %v", e)
	}
}

type decisionLoopbackTransport struct{ handler http.Handler }

func (t decisionLoopbackTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	w := httptest.NewRecorder()
	t.handler.ServeHTTP(w, r)
	return w.Result(), nil
}
