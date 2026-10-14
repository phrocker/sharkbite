// Licensed under the Apache License, Version 2.0. See LICENSE and NOTICE.
package webapi

import (
	"context"
	"errors"
	decisionapi "github.com/phrocker/shoal-oss/pkg/decision/api"
	"github.com/phrocker/shoal-oss/pkg/explorer/auth"
	"github.com/phrocker/shoal-oss/pkg/shoal"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

type adjudicationHTTPStub struct {
	calls   int
	reply   decisionapi.AdjudicationReceipt
	history decisionapi.AdjudicationHistory
	err     error
	during  func(*decisionapi.AdjudicationProposal)
}

func (p *adjudicationHTTPStub) Adjudicate(_ context.Context, o decisionapi.AdjudicationProposal, key []byte) (decisionapi.AdjudicationReceipt, error) {
	p.calls++
	if p.during != nil {
		p.during(&o)
	}
	return p.reply, p.err
}
func (p *adjudicationHTTPStub) AdjudicationHistory(_ context.Context, target shoal.ID) (decisionapi.AdjudicationHistory, error) {
	p.calls++
	if p.during != nil {
		p.during(nil)
	}
	return p.history, p.err
}
func adjudicationHTTPReply() decisionapi.AdjudicationReceipt {
	f := false
	return decisionapi.AdjudicationReceipt{ID: shoal.ID("adjudication-receipt:" + strings.Repeat("a", 64)), BasisID: shoal.ID("decision:adjudication-basis:v1:" + strings.Repeat("d", 64)), Version: 1, TargetID: shoal.ID("decision:adjudication-target:v1:" + strings.Repeat("e", 64)), TaskID: "task", PictureID: "picture", PolicyID: shoal.ID("decision:label-policy:v1:" + strings.Repeat("f", 64)), ProposalID: shoal.ID("decision:adjudication-proposal:v1:" + strings.Repeat("1", 64)), Proposal: decisionapi.AdjudicationProposal{RequestID: "request", PredictionID: "prediction", SubjectID: "subject", QuestionID: "question", ObservationReceiptIDs: []shoal.ID{shoal.ID("outcome-receipt:" + strings.Repeat("b", 64))}, WitnessIDs: []shoal.ID{"witness"}, Disposition: "verified", Truth: &f}, Adjudicator: decisionapi.AdjudicationAttribution{SubjectID: "judge", ActorID: "actor", AuthorizationFingerprint: "auth-sha256:" + strings.Repeat("c", 64)}, ReceivedAt: time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)}
}
func adjudicationHTTPFixture(t *testing.T) (*Handler, *adjudicationHTTPStub, *auth.Authority, auth.Decision) {
	t.Helper()
	h, _, a, d := decisionHTTPFixture(t)
	p := &adjudicationHTTPStub{reply: adjudicationHTTPReply()}
	p.history = decisionapi.AdjudicationHistory{TargetID: p.reply.TargetID, Receipts: []decisionapi.AdjudicationReceipt{p.reply}}
	if e := h.MountAdjudications(p, a.Resolver()); e != nil {
		t.Fatal(e)
	}
	return h, p, a, d
}
func adjudicationHTTPRequest(method string) *http.Request {
	path := AdjudicationsRoute
	body := ""
	if method == http.MethodPost {
		b, e := decisionapi.EncodeAdjudicationRequest(adjudicationHTTPReply().Proposal)
		if e != nil {
			panic(e)
		}
		body = string(b)
	} else {
		path += "/" + decisionapi.EncodeID(adjudicationHTTPReply().TargetID)
	}
	r := httptest.NewRequest(method, "http://example.test"+path, strings.NewReader(body))
	if method == http.MethodPost {
		r.Header.Set("Idempotency-Key", decisionapi.EncodeKey([]byte{0, 255, 'k'}))
		r.Header.Set("Content-Type", "application/json")
	}
	return r
}
func TestAdjudicationHTTPRoundTrip(t *testing.T) {
	for _, method := range []string{http.MethodPost, http.MethodGet} {
		t.Run(method, func(t *testing.T) {
			h, p, _, _ := adjudicationHTTPFixture(t)
			w := httptest.NewRecorder()
			h.ServeHTTP(w, adjudicationHTTPRequest(method))
			if w.Code != 200 {
				t.Fatalf("%d %s", w.Code, w.Body)
			}
			if method == http.MethodPost {
				got, e := decisionapi.DecodeAdjudicationReceipt(w.Body.Bytes())
				if e != nil || got.ID != p.reply.ID {
					t.Fatalf("%v %#v", e, got)
				}
			} else {
				got, e := decisionapi.DecodeAdjudicationHistory(w.Body.Bytes())
				if e != nil || len(got.Receipts) != 1 || got.Receipts[0].ID != p.reply.ID {
					t.Fatalf("%v %#v", e, got)
				}
			}
			if w.Header().Get("Cache-Control") != "no-store" || p.calls != 1 {
				t.Fatal("cache/calls")
			}
		})
	}
}
func TestAdjudicationHTTPRequiresTrustedMount(t *testing.T) {
	h, p, a, _ := adjudicationHTTPFixture(t)
	anon, _ := NewHandler(&stubWorkspaceService{}, "example.test")
	if anon.MountAdjudications(p, a.Resolver()) == nil || h.MountAdjudications(p, a.Resolver()) == nil {
		t.Fatal("unsafe mount")
	}
	var nilP *adjudicationHTTPStub
	if _, e := NewAdjudicationHTTPHandler(nilP, a.Resolver()); e == nil {
		t.Fatal("typednil")
	}
	if _, e := NewAdjudicationHTTPHandler(p, nil); e == nil {
		t.Fatal("nil resolver")
	}
	standalone, _ := NewAdjudicationHTTPHandler(p, a.Resolver())
	w := httptest.NewRecorder()
	standalone.ServeHTTP(w, adjudicationHTTPRequest(http.MethodPost))
	if w.Code != 401 || p.calls != 0 {
		t.Fatal("unbound request reached provider")
	}
}
func TestAdjudicationHTTPRejectsBeforeProvider(t *testing.T) {
	cases := map[string]func(*http.Request){
		"query": func(r *http.Request) { r.URL.RawQuery = "x=1" }, "emptyquery": func(r *http.Request) { r.URL.ForceQuery = true }, "encoded": func(r *http.Request) { r.URL.RawPath = "/api/v1/%61djudications" }, "slash": func(r *http.Request) { r.URL.Path += "/" }, "item": func(r *http.Request) { r.URL.Path += "/" + decisionapi.EncodeID(adjudicationHTTPReply().TargetID) }, "cookie": func(r *http.Request) { r.Header["Cookie"] = []string{""} }, "origin": func(r *http.Request) { r.Header.Set("Origin", "http://evil.test") }, "fetch": func(r *http.Request) { r.Header.Set("Sec-Fetch-Site", "cross-site") }, "duplicateorigin": func(r *http.Request) { r.Header["Origin"] = []string{"http://example.test", "http://example.test"} }, "missingkey": func(r *http.Request) { r.Header.Del("Idempotency-Key") }, "paddedkey": func(r *http.Request) { r.Header.Set("Idempotency-Key", "aw==") }, "duplicatekey": func(r *http.Request) { r.Header["idempotency-key"] = []string{"aw"} }, "encoding": func(r *http.Request) { r.Header.Set("Content-Encoding", "identity") }, "type": func(r *http.Request) { r.Header.Set("Content-Type", "text/plain") }, "oversize": func(r *http.Request) { r.ContentLength = decisionapi.MaxAdjudicationRequestBytes + 1 }, "host": func(r *http.Request) { r.Host = "evil.test" },
		"case": func(r *http.Request) {
			b, _ := io.ReadAll(r.Body)
			r.Body = io.NopCloser(strings.NewReader(strings.Replace(string(b), `"schema"`, `"Schema"`, 1)))
		}, "duplicate": func(r *http.Request) {
			b, _ := io.ReadAll(r.Body)
			r.Body = io.NopCloser(strings.NewReader(strings.Replace(string(b), `"schema":1`, `"schema":1,"schema":1`, 1)))
		}, "null": func(r *http.Request) { r.Body = io.NopCloser(strings.NewReader(`null`)) }}
	for name, change := range cases {
		t.Run(name, func(t *testing.T) {
			h, p, _, _ := adjudicationHTTPFixture(t)
			r := adjudicationHTTPRequest(http.MethodPost)
			change(r)
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			if w.Code < 400 || p.calls != 0 || w.Header().Get(CommitOutcomeHeader) != "" {
				t.Fatalf("%d calls%d %s", w.Code, p.calls, w.Body)
			}
		})
	}
	for _, method := range []string{http.MethodHead, http.MethodDelete} {
		h, p, _, _ := adjudicationHTTPFixture(t)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, adjudicationHTTPRequest(method))
		if w.Code != 405 || p.calls != 0 {
			t.Fatalf("%s %d", method, w.Code)
		}
	}
	for _, mode := range []string{"key", "body", "collection", "extra-path", "padding"} {
		t.Run("GET"+mode, func(t *testing.T) {
			h, p, _, _ := adjudicationHTTPFixture(t)
			r := adjudicationHTTPRequest(http.MethodGet)
			switch mode {
			case "key":
				r.Header["Idempotency-Key"] = []string{""}
			case "body":
				r.Body = io.NopCloser(strings.NewReader("x"))
			case "collection":
				r.URL.Path = AdjudicationsRoute
			case "extra-path":
				r.URL.Path += "/other"
			case "padding":
				r.URL.Path += "="
			}
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			if w.Code != 400 || p.calls != 0 {
				t.Fatalf("%d %d", w.Code, p.calls)
			}
		})
	}
}
func TestAdjudicationHTTPPostFailuresAreUncertain(t *testing.T) {
	for _, kind := range []string{"error", "notfound", "conflict", "binding", "projection", "cancel", "mutation"} {
		t.Run(kind, func(t *testing.T) {
			h, p, _, _ := adjudicationHTTPFixture(t)
			r := adjudicationHTTPRequest(http.MethodPost)
			switch kind {
			case "error":
				p.err = errors.New("secret internal path")
			case "notfound":
				p.err = shoal.NewError(shoal.ErrorNotFound, "secret")
			case "conflict":
				p.err = shoal.NewError(shoal.ErrorConflict, "secret")
			case "binding":
				p.reply.Proposal.PredictionID = "substituted"
			case "projection":
				p.reply.ID = "bad"
			case "cancel":
				ctx, cancel := context.WithCancel(r.Context())
				r = r.WithContext(ctx)
				p.during = func(*decisionapi.AdjudicationProposal) { cancel() }
			case "mutation":
				p.during = func(o *decisionapi.AdjudicationProposal) {
					*o.Truth = true
					o.WitnessIDs[0] = "changed"
					o.ObservationReceiptIDs[0] = shoal.ID("outcome-receipt:" + strings.Repeat("f", 64))
					p.reply.Proposal = *o
				}
			}
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			if w.Code != 503 || p.calls != 1 || w.Header().Get(CommitOutcomeHeader) != CommitOutcomeIndeterminate || !strings.Contains(w.Body.String(), `"indeterminate":true`) {
				t.Fatalf("%d %s", w.Code, w.Body)
			}
			if strings.Contains(w.Body.String(), "secret") {
				t.Fatal("leaked")
			}
		})
	}
}
func TestAdjudicationHTTPHistoryAllOrNothing(t *testing.T) {
	for _, kind := range []string{"error", "wrong-target", "gap", "duplicate", "bad-head", "bad-receipt"} {
		t.Run(kind, func(t *testing.T) {
			h, p, _, _ := adjudicationHTTPFixture(t)
			switch kind {
			case "error":
				p.err = errors.New("secret")
			case "wrong-target":
				p.history.TargetID = "other"
			case "gap":
				p.history.Receipts[0].Version = 2
			case "duplicate":
				p.history.Receipts = append(p.history.Receipts, p.history.Receipts[0])
			case "bad-head":
				p.history.Receipts[0].Proposal.ExpectedHeadID = "adjudication-receipt:" + shoal.ID(strings.Repeat("f", 64))
			case "bad-receipt":
				p.history.Receipts[0].BasisID = ""
			}
			w := httptest.NewRecorder()
			h.ServeHTTP(w, adjudicationHTTPRequest(http.MethodGet))
			if w.Code != 503 || p.calls != 1 || w.Header().Get(CommitOutcomeHeader) != "" || strings.Contains(w.Body.String(), string(p.reply.ID)) {
				t.Fatalf("%d %s", w.Code, w.Body)
			}
		})
	}
}
func TestAdjudicationHTTPRechecksAndOutputBounds(t *testing.T) {
	for _, method := range []string{http.MethodGet, http.MethodPost} {
		for _, denyAt := range []int{1, 2, 3, 4, 5} {
			t.Run(method+string(rune('0'+denyAt)), func(t *testing.T) {
				_, p, a, d := adjudicationHTTPFixture(t)
				resolver := &outcomeTestResolver{base: a.Resolver(), denyAt: denyAt}
				h, _ := NewAdjudicationHTTPHandler(p, resolver)
				r := adjudicationHTTPRequest(method)
				ctx, e := a.Binder().Bind(r.Context(), d)
				if e != nil {
					t.Fatal(e)
				}
				r = r.WithContext(ctx)
				w := httptest.NewRecorder()
				var writer http.ResponseWriter = w
				if denyAt == 5 {
					writer = workspaceResponseWriter{ResponseWriter: w, maxResponseBytes: 256}
				}
				h.ServeHTTP(writer, r)
				calls := 0
				if denyAt >= 3 {
					calls = 1
				}
				uncertain := method == http.MethodPost && calls == 1
				if p.calls != calls || w.Code < 400 || (w.Header().Get(CommitOutcomeHeader) != "") != uncertain {
					t.Fatalf("calls%d status%d %s", p.calls, w.Code, w.Body)
				}
				if strings.Contains(w.Body.String(), string(p.reply.ID)) {
					t.Fatal("disclosed after auth/bounds failure")
				}
			})
		}
	}
	if !requestMayCommit(http.MethodPost, AdjudicationsRoute) || requestMayCommit(http.MethodGet, AdjudicationsRoute+"/target") {
		t.Fatal("workspace commit classification")
	}
}
