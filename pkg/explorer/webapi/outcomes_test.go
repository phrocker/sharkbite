// Licensed under the Apache License, Version 2.0. See LICENSE and NOTICE.
package webapi

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	decisionapi "github.com/phrocker/shoal-oss/pkg/decision/api"
	"github.com/phrocker/shoal-oss/pkg/explorer/auth"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

type outcomeHTTPStub struct {
	calls  int
	reply  decisionapi.OutcomeReceipt
	err    error
	during func(*decisionapi.OutcomeObservation)
}

func (p *outcomeHTTPStub) AppendOutcome(_ context.Context, o decisionapi.OutcomeObservation, key []byte) (decisionapi.OutcomeReceipt, error) {
	p.calls++
	if p.during != nil {
		p.during(&o)
	}
	return p.reply, p.err
}
func (p *outcomeHTTPStub) ReadOutcome(ctx context.Context, r, pred shoal.ID, key []byte) (decisionapi.OutcomeReceipt, error) {
	return p.AppendOutcome(ctx, decisionapi.OutcomeObservation{}, key)
}
func outcomeHTTPReply() decisionapi.OutcomeReceipt {
	f := false
	now := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	return decisionapi.OutcomeReceipt{ID: shoal.ID("outcome-receipt:" + strings.Repeat("a", 64)), ObservationID: shoal.ID("decision:outcome:v1:" + strings.Repeat("b", 64)), Observation: decisionapi.OutcomeObservation{RequestID: "request", PredictionID: "prediction", SubjectID: "subject", Kind: "correctness", QuestionID: "question", Truth: &f, EvidenceIDs: []shoal.ID{"evidence"}, ObservedAt: now, AssertedProvenance: decisionapi.OutcomeProvenance{ReporterID: "reporter"}}, SubmitterID: "subject", ActorID: "actor", AuthorizationFingerprint: "auth-sha256:" + strings.Repeat("c", 64), ReceivedAt: now.Add(time.Second), State: "proposed"}
}
func outcomeHTTPFixture(t *testing.T) (*Handler, *outcomeHTTPStub, *auth.Authority, auth.Decision) {
	t.Helper()
	h, _, a, d := decisionHTTPFixture(t)
	p := &outcomeHTTPStub{reply: outcomeHTTPReply()}
	if e := h.MountOutcomes(p, a.Resolver()); e != nil {
		t.Fatal(e)
	}
	return h, p, a, d
}
func outcomeHTTPRequest(method string) *http.Request {
	path := OutcomesRoute
	body := ""
	if method == http.MethodPost {
		b, e := decisionapi.EncodeOutcomeRequest(outcomeHTTPReply().Observation)
		if e != nil {
			panic(e)
		}
		body = string(b)
	} else {
		path += "/" + decisionapi.EncodeID("request") + "/" + decisionapi.EncodeID("prediction")
	}
	r := httptest.NewRequest(method, "http://example.test"+path, strings.NewReader(body))
	r.Header.Set("Idempotency-Key", decisionapi.EncodeKey([]byte{0, 255, 'k'}))
	if method == http.MethodPost {
		r.Header.Set("Content-Type", "application/json")
	}
	return r
}
func TestOutcomeHTTPRoundTrip(t *testing.T) {
	for _, method := range []string{http.MethodPost, http.MethodGet} {
		t.Run(method, func(t *testing.T) {
			h, p, _, _ := outcomeHTTPFixture(t)
			w := httptest.NewRecorder()
			h.ServeHTTP(w, outcomeHTTPRequest(method))
			if w.Code != 200 {
				t.Fatalf("%d %s", w.Code, w.Body)
			}
			got, e := decisionapi.DecodeOutcomeReceipt(w.Body.Bytes())
			if e != nil || got.ID != p.reply.ID {
				t.Fatalf("%v %#v", e, got)
			}
			if w.Header().Get("Cache-Control") != "no-store" || p.calls != 1 {
				t.Fatal("cache or calls")
			}
		})
	}
}
func TestOutcomeHTTPMountTrust(t *testing.T) {
	h, p, a, _ := outcomeHTTPFixture(t)
	anon, _ := NewHandler(&stubWorkspaceService{}, "example.test")
	if anon.MountOutcomes(p, a.Resolver()) == nil || h.MountOutcomes(p, a.Resolver()) == nil {
		t.Fatal("unsafe mount")
	}
	var nilP *outcomeHTTPStub
	if _, e := NewOutcomeHTTPHandler(nilP, a.Resolver()); e == nil {
		t.Fatal("nil provider")
	}
	if _, e := NewOutcomeHTTPHandler(p, nil); e == nil {
		t.Fatal("nil resolver")
	}
	standalone, _ := NewOutcomeHTTPHandler(p, a.Resolver())
	w := httptest.NewRecorder()
	standalone.ServeHTTP(w, outcomeHTTPRequest(http.MethodPost))
	if w.Code != 401 || p.calls != 0 {
		t.Fatalf("unbound: %d %d", w.Code, p.calls)
	}
}
func TestOutcomeHTTPRejectsBeforeProvider(t *testing.T) {
	cases := map[string]func(*http.Request){
		"query": func(r *http.Request) { r.URL.RawQuery = "x=1" }, "empty-query": func(r *http.Request) { r.URL.ForceQuery = true }, "encoded-path": func(r *http.Request) { r.URL.RawPath = "/api/v1/%6futcomes" }, "slash": func(r *http.Request) { r.URL.Path += "/" },
		"cookie": func(r *http.Request) { r.Header["Cookie"] = []string{""} }, "origin": func(r *http.Request) { r.Header.Set("Origin", "http://evil.test") }, "fetch": func(r *http.Request) { r.Header.Set("Sec-Fetch-Site", "cross-site") }, "duplicate-origin": func(r *http.Request) { r.Header["Origin"] = []string{"http://example.test", "http://example.test"} },
		"missing-key": func(r *http.Request) { r.Header.Del("Idempotency-Key") }, "padded-key": func(r *http.Request) { r.Header.Set("Idempotency-Key", "aw==") }, "duplicate-key": func(r *http.Request) { r.Header["idempotency-key"] = []string{"aw"} }, "content-encoding": func(r *http.Request) { r.Header.Set("Content-Encoding", "identity") }, "type": func(r *http.Request) { r.Header.Set("Content-Type", "text/plain") }, "oversize": func(r *http.Request) { r.ContentLength = decisionapi.MaxOutcomeRequestBytes + 1 },
		"case-field": func(r *http.Request) {
			b, _ := io.ReadAll(r.Body)
			r.Body = io.NopCloser(strings.NewReader(strings.Replace(string(b), `"schema"`, `"Schema"`, 1)))
		}, "duplicate-field": func(r *http.Request) {
			b, _ := io.ReadAll(r.Body)
			r.Body = io.NopCloser(strings.NewReader(strings.Replace(string(b), `"schema":1`, `"schema":1,"schema":1`, 1)))
		}, "null": func(r *http.Request) { r.Body = io.NopCloser(strings.NewReader(`null`)) },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			h, p, _, _ := outcomeHTTPFixture(t)
			r := outcomeHTTPRequest(http.MethodPost)
			mutate(r)
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			if w.Code < 400 || p.calls != 0 || w.Header().Get(CommitOutcomeHeader) != "" {
				t.Fatalf("status=%d calls=%d body=%s", w.Code, p.calls, w.Body)
			}
		})
	}
	for _, method := range []string{http.MethodHead, http.MethodDelete} {
		h, p, _, _ := outcomeHTTPFixture(t)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, outcomeHTTPRequest(method))
		if w.Code != 405 || p.calls != 0 {
			t.Fatalf("%s: %d", method, w.Code)
		}
	}
	h, p, _, _ := outcomeHTTPFixture(t)
	r := outcomeHTTPRequest(http.MethodGet)
	r.Body = io.NopCloser(strings.NewReader("x"))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != 400 || p.calls != 0 {
		t.Fatal("GET body accepted")
	}
}
func TestOutcomeHTTPProviderFailuresAndBinding(t *testing.T) {
	for _, method := range []string{http.MethodPost, http.MethodGet} {
		for _, kind := range []string{"error", "notfound", "binding", "projection", "cancel", "mutation"} {
			t.Run(method+kind, func(t *testing.T) {
				h, p, _, _ := outcomeHTTPFixture(t)
				r := outcomeHTTPRequest(method)
				switch kind {
				case "error":
					p.err = errors.New("private diagnostic")
				case "notfound":
					p.err = shoal.NewError(shoal.ErrorNotFound, "secret")
				case "binding":
					p.reply.Observation.PredictionID = "substituted"
				case "projection":
					p.reply.ID = "bad"
				case "cancel":
					ctx, cancel := context.WithCancel(r.Context())
					r = r.WithContext(ctx)
					p.during = func(*decisionapi.OutcomeObservation) { cancel() }
				case "mutation":
					if method == http.MethodGet {
						return
					}
					p.during = func(o *decisionapi.OutcomeObservation) {
						*o.Truth = true
						o.EvidenceIDs[0] = "substituted"
						p.reply.Observation = *o
					}
				}
				w := httptest.NewRecorder()
				h.ServeHTTP(w, r)
				uncertain := method == http.MethodPost
				if w.Code < 400 || p.calls != 1 || (w.Header().Get(CommitOutcomeHeader) == CommitOutcomeIndeterminate) != uncertain {
					t.Fatalf("%d %s", w.Code, w.Body)
				}
				if uncertain && w.Code != 503 {
					t.Fatal("append failure not 503")
				}
				if strings.Contains(w.Body.String(), "private") || strings.Contains(w.Body.String(), "secret") {
					t.Fatal("error leaked")
				}
			})
		}
	}
}

type outcomeTestResolver struct {
	base          auth.Resolver
	calls, denyAt int
}

func (r *outcomeTestResolver) Resolve(ctx context.Context) (auth.Decision, error) {
	r.calls++
	if r.calls == r.denyAt {
		return auth.Decision{}, authenticationDenied()
	}
	return r.base.Resolve(ctx)
}
func TestOutcomeHTTPRechecksAtEveryBoundary(t *testing.T) {
	for _, method := range []string{http.MethodGet, http.MethodPost} {
		for _, denyAt := range []int{1, 2, 3, 4} {
			t.Run(method+string(rune('0'+denyAt)), func(t *testing.T) {
				_, p, a, d := outcomeHTTPFixture(t)
				resolver := &outcomeTestResolver{base: a.Resolver(), denyAt: denyAt}
				h, _ := NewOutcomeHTTPHandler(p, resolver)
				r := outcomeHTTPRequest(method)
				ctx, e := a.Binder().Bind(r.Context(), d)
				if e != nil {
					t.Fatal(e)
				}
				r = r.WithContext(ctx)
				w := httptest.NewRecorder()
				h.ServeHTTP(w, r)
				expectedCalls := 0
				if denyAt >= 3 {
					expectedCalls = 1
				}
				uncertain := method == http.MethodPost && expectedCalls == 1
				if p.calls != expectedCalls || w.Code < 400 || (w.Header().Get(CommitOutcomeHeader) != "") != uncertain {
					t.Fatalf("calls=%d status=%d header=%s", p.calls, w.Code, w.Header().Get(CommitOutcomeHeader))
				}
				if strings.Contains(w.Body.String(), string(p.reply.ID)) {
					t.Fatal("disclosed after authorization failure")
				}
			})
		}
	}
}
func TestOutcomeHTTPOutputBoundFailure(t *testing.T) {
	for _, method := range []string{http.MethodPost, http.MethodGet} {
		_, p, a, d := outcomeHTTPFixture(t)
		h, _ := NewOutcomeHTTPHandler(p, a.Resolver())
		r := outcomeHTTPRequest(method)
		ctx, _ := a.Binder().Bind(r.Context(), d)
		w := httptest.NewRecorder()
		h.ServeHTTP(workspaceResponseWriter{ResponseWriter: w, maxResponseBytes: 256}, r.WithContext(ctx))
		if w.Code != 503 || p.calls != 1 || (w.Header().Get(CommitOutcomeHeader) != "") != (method == http.MethodPost) {
			t.Fatalf("%s %d %s", method, w.Code, w.Body)
		}
		if strings.Contains(w.Body.String(), string(p.reply.ID)) {
			t.Fatal("oversize disclosed")
		}
	}
}
