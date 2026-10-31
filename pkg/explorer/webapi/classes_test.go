// Licensed under the Apache License, Version 2.0. See LICENSE and NOTICE.
package webapi

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/phrocker/shoal-oss/pkg/decision"
	decisionapi "github.com/phrocker/shoal-oss/pkg/decision/api"
	"github.com/phrocker/shoal-oss/pkg/explorer/auth"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

type classHTTPStub struct {
	calls  int
	req    decisionapi.ClassRecordRequest
	reply  decisionapi.ClassRecords
	err    error
	during func()
}

func (p *classHTTPStub) ClassRecords(_ context.Context, req decisionapi.ClassRecordRequest) (decisionapi.ClassRecords, error) {
	p.calls++
	p.req = req
	if p.during != nil {
		p.during()
	}
	return p.reply, p.err
}

var classHTTPNow = time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)

func classHTTPReply(id shoal.ID) decisionapi.ClassRecords {
	class := decision.DecisionClass{TaskID: "task", QuestionID: "q", PolicyID: "policy", Label: "inspect"}
	cid, _ := class.ID()
	p := 1.0
	return decisionapi.ClassRecords{CohortID: id, TaskID: "task", QuestionID: "q", PolicyID: "policy", Cutoff: classHTTPNow, From: classHTTPNow.Add(-time.Hour), To: classHTTPNow, Members: 1, InWindow: 1, Classes: []decisionapi.ClassRecord{{ClassID: cid, Class: class, Targets: 1, Adjudication: decisionapi.ClassAdjudication{VerifiedAgree: 1}, Outcomes: decisionapi.ClassOutcomes{Receipts: 1, Correctness: 1}, Precision: &p, FirstObserved: classHTTPNow.Add(-time.Minute), LastObserved: classHTTPNow.Add(-time.Minute)}}}
}
func classHTTPBody(id shoal.ID) string {
	b, _ := decisionapi.EncodeClassRecordRequest(decisionapi.ClassRecordRequest{CohortID: id, From: classHTTPNow.Add(-time.Hour), To: classHTTPNow})
	return string(b)
}
func classHTTPRequest() *http.Request {
	r := httptest.NewRequest(http.MethodPost, "http://example.test"+DecisionClassesRoute, strings.NewReader(classHTTPBody("cohort")))
	r.Header.Set("Content-Type", "application/json")
	return r
}
func classHTTPFixture(t *testing.T) (*Handler, *classHTTPStub, *auth.Authority, auth.Decision) {
	t.Helper()
	a := auth.NewAuthority()
	d, e := auth.NewDecision(auth.DecisionConfig{Subject: "subject", Actor: "actor", AuthorizationDomain: []byte("domain"), AllowedOperations: []auth.Operation{auth.OperationRead}, PermittedSourceIDs: [][]byte{[]byte("source")}, PermittedPolicyIDs: [][]byte{[]byte("policy")}, PolicyGeneration: 1, AuthenticationExpires: time.Now().Add(time.Hour), RequestID: "request"})
	if e != nil {
		t.Fatal(e)
	}
	h, e := NewAuthenticatedHandler(&stubWorkspaceService{}, AuthenticatorFunc(func(*http.Request) (auth.Decision, error) { return d, nil }), a.Binder(), "example.test")
	if e != nil {
		t.Fatal(e)
	}
	p := &classHTTPStub{reply: classHTTPReply("cohort")}
	if e = h.MountDecisionClasses(p, a.Resolver()); e != nil {
		t.Fatal(e)
	}
	return h, p, a, d
}

func TestClassHTTPServesRecordsAndAdvertisesTheCapability(t *testing.T) {
	h, p, _, _ := classHTTPFixture(t)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, classHTTPRequest())
	if w.Code != 200 || p.calls != 1 || p.req.CohortID != "cohort" || !p.req.To.Equal(classHTTPNow) {
		t.Fatal(w.Code, w.Body.String())
	}
	back, e := decisionapi.DecodeClassRecords(w.Body.Bytes())
	if e != nil || len(back.Classes) != 1 || back.Classes[0].Adjudication.VerifiedAgree != 1 || w.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("%v %+v", e, back)
	}
	if !h.decisionCapabilities.Classes || !h.decisionCapabilities.Supports(CapabilityClasses) {
		t.Fatal("capability not advertised")
	}
	if mergeDecisionCapabilities(Capabilities{}, h.decisionCapabilities).Classes != true {
		t.Fatal("capability not merged into metadata")
	}
	if e := h.MountDecisionClasses(p, auth.NewAuthority().Resolver()); e == nil {
		t.Fatal("duplicate mount")
	}
}

func TestClassHTTPAuthenticationAndMounting(t *testing.T) {
	_, p, a, d := classHTTPFixture(t)
	anonymous, e := NewHandler(&stubWorkspaceService{}, "example.test")
	if e != nil {
		t.Fatal(e)
	}
	if e = anonymous.MountDecisionClasses(p, a.Resolver()); e == nil {
		t.Fatal("anonymous mount")
	}
	var nilProvider *classHTTPStub
	if _, e = NewDecisionClassHTTPHandler(nilProvider, a.Resolver()); e == nil {
		t.Fatal("typed nil provider")
	}
	handler, e := NewDecisionClassHTTPHandler(p, a.Resolver())
	if e != nil {
		t.Fatal(e)
	}
	r := classHTTPRequest()
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)
	if w.Code != 401 || p.calls != 0 {
		t.Fatal("anonymous reached provider")
	}
	other := auth.NewAuthority()
	ctx, e := other.Binder().Bind(r.Context(), d)
	if e != nil {
		t.Fatal(e)
	}
	w = httptest.NewRecorder()
	handler.ServeHTTP(w, r.WithContext(ctx))
	if w.Code != 401 || p.calls != 0 {
		t.Fatal("foreign binder accepted")
	}
}

func TestClassHTTPRejectsPathsHeadersAndBodies(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(*http.Request)
		status int
	}{
		{"query", func(r *http.Request) { r.URL.RawQuery = "cohort=x" }, 400}, {"slash", func(r *http.Request) { r.URL.Path += "/" }, 400}, {"encoded path", func(r *http.Request) { r.URL.RawPath = "/api/v1/%64ecision-classes" }, 400},
		{"host", func(r *http.Request) { r.Host = "evil.test" }, 421}, {"origin", func(r *http.Request) { r.Header.Set("Origin", "http://evil.test") }, 403}, {"fetch site", func(r *http.Request) { r.Header.Set("Sec-Fetch-Site", "cross-site") }, 403},
		{"encoding", func(r *http.Request) { r.Header["content-encoding"] = []string{"identity"} }, 400}, {"content type", func(r *http.Request) { r.Header.Set("Content-Type", "text/plain") }, 400}, {"GET", func(r *http.Request) { r.Method = "GET" }, 405},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, p, _, _ := classHTTPFixture(t)
			r := classHTTPRequest()
			tc.change(r)
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			if w.Code != tc.status || p.calls != 0 {
				t.Fatalf("code=%d calls=%d body=%s", w.Code, p.calls, w.Body.String())
			}
		})
	}
	undeclared := `{"cohort_id":"` + decisionapi.EncodeID("cohort") + `"}`
	for _, body := range []string{`null`, `{}`, undeclared, strings.Replace(classHTTPBody("cohort"), `"to"`, `"until"`, 1), classHTTPBody("cohort") + " {}", strings.Repeat(" ", 4097), "{\"cohort_id\":\"\xff\"}"} {
		h, p, _, _ := classHTTPFixture(t)
		r := classHTTPRequest()
		r.Body = ioNopString(body)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != 400 || p.calls != 0 {
			t.Fatalf("accepted body %q: %d", body, w.Code)
		}
	}
}

func TestClassHTTPProviderErrorsAndSubstitutions(t *testing.T) {
	for _, tc := range []struct {
		err    error
		status int
	}{{auth.ObjectNotFound(), 404}, {errors.New("private secret"), 503}, {shoal.NewError(shoal.ErrorInvalidArgument, "private window"), 400}} {
		h, p, _, _ := classHTTPFixture(t)
		p.err = tc.err
		w := httptest.NewRecorder()
		h.ServeHTTP(w, classHTTPRequest())
		if w.Code != tc.status || strings.Contains(w.Body.String(), "private") {
			t.Fatal(w.Code, w.Body.String())
		}
	}
	for name, change := range map[string]func(*classHTTPStub){
		"cohort": func(p *classHTTPStub) { p.reply = classHTTPReply("other") },
		"window": func(p *classHTTPStub) { p.reply.To = classHTTPNow.Add(-time.Minute); p.reply.Cutoff = p.reply.To },
		"open-ended window not at the cutoff": func(p *classHTTPStub) {
			p.reply.To = classHTTPNow.Add(-time.Minute)
			p.reply.Classes[0].FirstObserved, p.reply.Classes[0].LastObserved = p.reply.To, p.reply.To
		},
		"invalid": func(p *classHTTPStub) { p.reply.InWindow = 5 },
	} {
		h, p, _, _ := classHTTPFixture(t)
		change(p)
		r := classHTTPRequest()
		if strings.HasPrefix(name, "open-ended") {
			b, _ := decisionapi.EncodeClassRecordRequest(decisionapi.ClassRecordRequest{CohortID: "cohort", From: classHTTPNow.Add(-time.Hour)})
			r.Body = ioNopString(string(b))
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != 503 || strings.Contains(w.Body.String(), "classes") {
			t.Fatalf("%s: substituted or invalid provider result disclosed: %d %s", name, w.Code, w.Body.String())
		}
	}
}

func TestClassHTTPRevocationAndCancellationAfterProvider(t *testing.T) {
	for _, cancelled := range []bool{false, true} {
		_, p, _, d := classHTTPFixture(t)
		revoked := false
		resolver, e := auth.NewHostResolver(func(context.Context) (auth.Decision, error) {
			if revoked {
				return auth.Decision{}, auth.ObjectNotFound()
			}
			return d, nil
		})
		if e != nil {
			t.Fatal(e)
		}
		handler, e := NewDecisionClassHTTPHandler(p, resolver)
		if e != nil {
			t.Fatal(e)
		}
		r := classHTTPRequest()
		ctx, cancel := context.WithCancel(r.Context())
		defer cancel()
		p.during = func() {
			if cancelled {
				cancel()
			} else {
				revoked = true
			}
		}
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r.WithContext(ctx))
		if w.Code != 401 || p.calls != 1 || strings.Contains(w.Body.String(), "classes") {
			t.Fatal(w.Code, w.Body.String())
		}
	}
}
