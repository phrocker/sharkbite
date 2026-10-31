// Licensed under the Apache License, Version 2.0. See LICENSE and NOTICE.
package webapi

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	decisionapi "github.com/phrocker/shoal-oss/pkg/decision/api"
	"github.com/phrocker/shoal-oss/pkg/explorer/auth"
	"github.com/phrocker/shoal-oss/pkg/explorer/fleet"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

type inboxApprovalStub struct {
	pages   [][]fleet.ApprovalRecord
	err     error
	calls   int
	limits  []int
	afters  [][]byte
	during  func()
	context fleet.RequestContext
}

func (s *inboxApprovalStub) Request(context.Context, fleet.EnqueueRequest) (fleet.ApprovalReceipt, error) {
	return fleet.ApprovalReceipt{}, errors.New("unused")
}
func (s *inboxApprovalStub) Decide(context.Context, fleet.ApprovalDecisionRequest) (fleet.ApprovalRecord, error) {
	return fleet.ApprovalRecord{}, errors.New("unused")
}
func (s *inboxApprovalStub) Status(context.Context, fleet.ApprovalStatusRequest) (fleet.ApprovalStatus, error) {
	return fleet.ApprovalStatus{}, errors.New("unused")
}
func (s *inboxApprovalStub) Pending(_ context.Context, r fleet.PendingApprovalsRequest) (fleet.PendingApprovalsPage, error) {
	s.calls++
	s.limits = append(s.limits, r.Limit)
	s.afters = append(s.afters, r.After)
	s.context = r.Context
	if s.during != nil {
		s.during()
	}
	if s.err != nil {
		return fleet.PendingApprovalsPage{}, s.err
	}
	index := 0
	if len(r.After) > 0 {
		fmt.Sscanf(string(r.After), "page-%d", &index)
	}
	if index >= len(s.pages) {
		return fleet.PendingApprovalsPage{}, nil
	}
	page := fleet.PendingApprovalsPage{Approvals: s.pages[index]}
	if index+1 < len(s.pages) {
		page.Next = []byte(fmt.Sprintf("page-%d", index+1))
	}
	return page, nil
}

var inboxNow = time.Date(2026, 10, 31, 12, 0, 0, 0, time.UTC)

func inboxApproval(id, capability, action string, requested time.Time) fleet.ApprovalRecord {
	return fleet.ApprovalRecord{ID: []byte(id), Version: 1, State: fleet.ApprovalPending, RequestDigest: []byte("digest-" + id), PolicyGeneration: 3, RequestedAt: requested, ExpiresAt: requested.Add(time.Hour), Request: fleet.ActionRecord{ID: []byte(id), AgentID: "gateway", AgentGeneration: 1, Capability: capability, Action: action, ObjectID: "release-7", Subject: "alice", Actor: "alice-agent", Deadline: requested.Add(3 * time.Hour), Input: json.RawMessage(`{"secret":"input"}`)}}
}

type inboxLearningStub struct {
	learningStub
	replies map[string]string
	errs    map[string]error
	jobs    []string
}

func (s *inboxLearningStub) answer(name string) (json.RawMessage, error) {
	if err := s.errs[name]; err != nil {
		return nil, err
	}
	if r, ok := s.replies[name]; ok {
		return json.RawMessage(r), nil
	}
	return json.RawMessage(`{}`), nil
}
func (s *inboxLearningStub) Candidates(context.Context) (json.RawMessage, error) {
	return s.answer("candidates")
}
func (s *inboxLearningStub) Releases(context.Context) (json.RawMessage, error) {
	return s.answer("releases")
}
func (s *inboxLearningStub) Job(_ context.Context, pin string) (json.RawMessage, error) {
	s.jobs = append(s.jobs, pin)
	return s.answer("job:" + pin)
}

const inboxCandidates = `{"candidates":[
 {"candidate":{"id":"cand-shadow","task_ids":["task"],"dataset_id":"ds","training_manifest_sha256":"pin-1"},"evaluations":[{"id":"eval-held","disposition":"held"},{"id":"eval-ok","disposition":"shadow_candidate"}],"lineage":{"dataset_id":"ds"}},
 {"candidate":{"id":"cand-released","training_manifest_sha256":"pin-2"},"evaluations":[{"id":"eval-r","disposition":"shadow_candidate"}]},
 {"candidate":{"id":"cand-held","training_manifest_sha256":"pin-3"},"evaluations":[{"id":"eval-h","disposition":"held"}]},
 {"candidate":{"id":"cand-unevaluated","training_manifest_sha256":"pin-4"},"evaluations":[]}
]}`
const inboxReleases = `{"active":{"release":{"candidate_id":"cand-released"}},"history":[]}`
const inboxJob = `{"job":{"manifest_pin":"pin-1","exports":[{"cohort_id":"cohort","manifest_sha256":"x"},{"cohort_id":"other","manifest_sha256":"y"},{"cohort_id":"cohort","manifest_sha256":"x"}]}}`

type inboxClassStub struct {
	calls int
	reqs  []decisionapi.ClassRecordRequest
	err   map[string]error
}

func (s *inboxClassStub) ClassRecords(_ context.Context, req decisionapi.ClassRecordRequest) (decisionapi.ClassRecords, error) {
	s.calls++
	s.reqs = append(s.reqs, req)
	if err := s.err[string(req.CohortID)]; err != nil {
		return decisionapi.ClassRecords{}, err
	}
	r := classHTTPReply(req.CohortID)
	r.From = req.From
	r.Cutoff, r.To = inboxNow, inboxNow
	r.Classes[0].FirstObserved, r.Classes[0].LastObserved = inboxNow.Add(-time.Minute), inboxNow.Add(-time.Minute)
	return r, nil
}

func inboxBody() string {
	return `{"context":{"request_id":"` + base64.RawURLEncoding.EncodeToString([]byte("inbox-read")) + `","reason_code":"review","deadline":"` + inboxNow.Add(time.Hour).Format(time.RFC3339Nano) + `"}}`
}
func inboxRequest(body string) *http.Request {
	r := httptest.NewRequest(http.MethodPost, "http://example.test"+InboxRoute, strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	return r
}

type inboxFixture struct {
	handler   *Handler
	approvals *inboxApprovalStub
	learning  *inboxLearningStub
	classes   *inboxClassStub
	authority *auth.Authority
	decision  auth.Decision
}

func newInboxFixture(t *testing.T, mutate func(*InboxSurface)) inboxFixture {
	t.Helper()
	a := auth.NewAuthority()
	d, e := auth.NewDecision(auth.DecisionConfig{Subject: "bob", Actor: "bob-console", AuthorizationDomain: []byte("domain"), AllowedOperations: []auth.Operation{auth.OperationActionApprove}, PermittedSourceIDs: [][]byte{[]byte("source")}, PermittedPolicyIDs: [][]byte{[]byte("policy")}, PolicyGeneration: 3, AuthenticationExpires: inboxNow.Add(time.Hour), RequestID: "request"})
	if e != nil {
		t.Fatal(e)
	}
	h, e := NewAuthenticatedHandler(&stubWorkspaceService{}, AuthenticatorFunc(func(*http.Request) (auth.Decision, error) { return d, nil }), a.Binder(), "example.test")
	if e != nil {
		t.Fatal(e)
	}
	f := inboxFixture{handler: h, authority: a, decision: d}
	f.approvals = &inboxApprovalStub{pages: [][]fleet.ApprovalRecord{{inboxApproval("older", "ops", "deploy", inboxNow.Add(-2*time.Hour)), inboxApproval("newer", "ops", "deploy", inboxNow.Add(-time.Hour)), inboxApproval("alpha", "billing", "refund", inboxNow.Add(-time.Hour))}}}
	f.learning = &inboxLearningStub{replies: map[string]string{"candidates": inboxCandidates, "releases": inboxReleases, "job:pin-1": inboxJob}}
	f.classes = &inboxClassStub{err: map[string]error{"other": auth.ObjectNotFound()}}
	surface := InboxSurface{Resolver: a.Resolver(), Approvals: f.approvals, Learning: f.learning, Classes: f.classes, Now: func() time.Time { return inboxNow }, RecordWindow: 30 * 24 * time.Hour}
	if mutate != nil {
		mutate(&surface)
	}
	if e = h.MountInbox(surface); e != nil {
		t.Fatal(e)
	}
	return f
}

func (f inboxFixture) read(t *testing.T, body string) (int, map[string]any) {
	t.Helper()
	w := httptest.NewRecorder()
	f.handler.ServeHTTP(w, inboxRequest(body))
	var out map[string]any
	if w.Body.Len() > 0 {
		if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
			t.Fatalf("not JSON: %s", w.Body.String())
		}
	}
	return w.Code, out
}

func TestInboxListsWhatAwaitsAPerson(t *testing.T) {
	f := newInboxFixture(t, nil)
	code, out := f.read(t, inboxBody())
	if code != 200 {
		t.Fatal(code, out)
	}
	approvals := out["approvals"].(map[string]any)
	if approvals["mounted"] != true || approvals["available"] != true || approvals["truncated"] != false {
		t.Fatalf("%v", approvals)
	}
	groups := approvals["groups"].([]any)
	if len(groups) != 2 {
		t.Fatalf("groups %v", groups)
	}
	billing, ops := groups[0].(map[string]any), groups[1].(map[string]any)
	if billing["capability"] != "billing" || ops["capability"] != "ops" || ops["action"] != "deploy" {
		t.Fatalf("group order %v %v", billing, ops)
	}
	items := ops["items"].([]any)
	first, second := items[0].(map[string]any), items[1].(map[string]any)
	if first["kind"] != "approval" || first["waiting"] != "decision" || first["id"] != base64.RawURLEncoding.EncodeToString([]byte("newer")) || second["id"] != base64.RawURLEncoding.EncodeToString([]byte("older")) {
		t.Fatalf("items %v", items)
	}
	approval := first["approval"].(map[string]any)
	if approval["request_digest"] != base64.RawURLEncoding.EncodeToString([]byte("digest-newer")) || approval["policy_generation"] != float64(3) || approval["input"] != nil || strings.Contains(fmt.Sprint(out), "secret") {
		t.Fatalf("approval row %v", approval)
	}
	if string(f.approvals.context.RequestID) != "inbox-read" || f.approvals.context.ReasonCode != "review" || f.approvals.limits[0] != fleet.MaxApprovalListResults {
		t.Fatalf("context %+v limits %v", f.approvals.context, f.approvals.limits)
	}
	candidates := out["candidates"].(map[string]any)
	citems := candidates["items"].([]any)
	if candidates["available"] != true || len(citems) != 1 {
		t.Fatalf("candidates %v", candidates)
	}
	item := citems[0].(map[string]any)
	cand := item["candidate"].(map[string]any)
	if item["id"] != "cand-shadow" || item["waiting"] != "decision" || cand["promotable_evaluation_id"] != "eval-ok" || cand["disposition"] != "shadow_candidate" || cand["training_manifest_sha256"] != "pin-1" {
		t.Fatalf("candidate %v", item)
	}
	classes := item["classes"].([]any)
	if len(classes) != 2 || f.classes.calls != 2 || len(f.learning.jobs) != 1 {
		t.Fatalf("classes %v calls=%d jobs=%v", classes, f.classes.calls, f.learning.jobs)
	}
	served, missing := classes[0].(map[string]any), classes[1].(map[string]any)
	if served["cohort_id"] != "cohort" || served["records"] == nil || served["error"] != nil || missing["cohort_id"] != "other" || missing["error"] != "class record not found" || missing["records"] != nil {
		t.Fatalf("records %v %v", served, missing)
	}
	records := served["records"].(map[string]any)
	if records["cohort_id"] != decisionapi.EncodeID("cohort") || !strings.HasPrefix(records["from"].(string), inboxNow.Add(-30*24*time.Hour).Format("2006-01-02")) {
		t.Fatalf("record window %v", records)
	}
	if f.classes.reqs[0].To != (time.Time{}) || !f.classes.reqs[0].From.Equal(inboxNow.Add(-30*24*time.Hour)) {
		t.Fatalf("window request %+v", f.classes.reqs[0])
	}
	if !f.handler.decisionCapabilities.Inbox || !mergeDecisionCapabilities(Capabilities{}, f.handler.decisionCapabilities).Supports(CapabilityInbox) {
		t.Fatal("capability not advertised")
	}
}

func TestInboxSaysWhenTheCallerIsNotAnApprover(t *testing.T) {
	f := newInboxFixture(t, nil)
	f.approvals.err = shoal.NewError(shoal.ErrorUnauthorized, "not an approver")
	code, out := f.read(t, inboxBody())
	approvals := out["approvals"].(map[string]any)
	if code != 200 || approvals["available"] != false || approvals["note"] == "" || len(approvals["groups"].([]any)) != 0 {
		t.Fatal(code, out)
	}
	if out["candidates"].(map[string]any)["available"] != true {
		t.Fatal("candidates withheld from a non-approver")
	}
	f.approvals.err = errors.New("store broke: private detail")
	code, out = f.read(t, inboxBody())
	if code == 200 || strings.Contains(fmt.Sprint(out), "private") {
		t.Fatal(code, out)
	}
}

func TestInboxWalksPagesAndStops(t *testing.T) {
	f := newInboxFixture(t, nil)
	var pages [][]fleet.ApprovalRecord
	for p := 0; p < 5; p++ {
		var page []fleet.ApprovalRecord
		for i := 0; i < fleet.MaxApprovalListResults; i++ {
			page = append(page, inboxApproval(fmt.Sprintf("a-%d-%d", p, i), "ops", "deploy", inboxNow.Add(-time.Duration(p*100+i)*time.Second)))
		}
		pages = append(pages, page)
	}
	f.approvals.pages = pages
	code, out := f.read(t, inboxBody())
	approvals := out["approvals"].(map[string]any)
	items := approvals["groups"].([]any)[0].(map[string]any)["items"].([]any)
	if code != 200 || len(items) != MaxInboxApprovals || approvals["truncated"] != true || f.approvals.calls != 4 {
		t.Fatalf("code=%d items=%d truncated=%v calls=%d", code, len(items), approvals["truncated"], f.approvals.calls)
	}
	if string(f.approvals.afters[1]) != "page-1" {
		t.Fatalf("cursor not carried: %q", f.approvals.afters)
	}
}

func TestInboxRequestContextAndMounting(t *testing.T) {
	f := newInboxFixture(t, nil)
	for _, body := range []string{`{}`, `{"context":{}}`, `{"context":{"request_id":"bad=","reason_code":"x","deadline":"2026-10-31T13:00:00Z"}}`, `null`, `{"extra":1}`} {
		if code, _ := f.read(t, body); code != 400 {
			t.Fatalf("accepted %s: %d", body, code)
		}
	}
	if f.approvals.calls != 0 {
		t.Fatal("provider reached without a context")
	}
	// Without an approval service the context is not needed.
	g := newInboxFixture(t, func(s *InboxSurface) { s.Approvals = nil })
	code, out := g.read(t, `{}`)
	approvals := out["approvals"].(map[string]any)
	if code != 200 || approvals["mounted"] != false || approvals["note"] == "" {
		t.Fatal(code, out)
	}
	anonymous, e := NewHandler(&stubWorkspaceService{}, "example.test")
	if e != nil {
		t.Fatal(e)
	}
	if e = anonymous.MountInbox(InboxSurface{Resolver: f.authority.Resolver(), Approvals: f.approvals}); e == nil {
		t.Fatal("anonymous mount")
	}
	if _, e = NewInboxHandler(InboxSurface{Resolver: f.authority.Resolver()}); e == nil {
		t.Fatal("inbox with nothing to list")
	}
	var nilApprovals *inboxApprovalStub
	if _, e = NewInboxHandler(InboxSurface{Resolver: f.authority.Resolver(), Approvals: nilApprovals}); e == nil {
		t.Fatal("typed nil provider")
	}
	if _, e = NewInboxHandler(InboxSurface{Resolver: f.authority.Resolver(), Approvals: f.approvals, RecordWindow: -time.Hour}); e == nil {
		t.Fatal("negative window")
	}
	w := httptest.NewRecorder()
	r := inboxRequest(inboxBody())
	r.Method = http.MethodGet
	f.handler.ServeHTTP(w, r)
	if w.Code != 405 && w.Code != 404 {
		t.Fatalf("GET served: %d", w.Code)
	}
}

func TestInboxWithholdsEverythingWhenAuthorityMoves(t *testing.T) {
	d, e := auth.NewDecision(auth.DecisionConfig{Subject: "bob", Actor: "bob-console", AuthorizationDomain: []byte("domain"), AllowedOperations: []auth.Operation{auth.OperationActionApprove}, PermittedSourceIDs: [][]byte{[]byte("source")}, PermittedPolicyIDs: [][]byte{[]byte("policy")}, PolicyGeneration: 3, AuthenticationExpires: inboxNow.Add(time.Hour), RequestID: "request"})
	if e != nil {
		t.Fatal(e)
	}
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
	approvals := &inboxApprovalStub{pages: [][]fleet.ApprovalRecord{{inboxApproval("x", "ops", "deploy", inboxNow)}}}
	approvals.during = func() { revoked = true }
	handler, e := NewInboxHandler(InboxSurface{Resolver: resolver, Approvals: approvals, Now: func() time.Time { return inboxNow }})
	if e != nil {
		t.Fatal(e)
	}
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, inboxRequest(inboxBody()))
	if w.Code != 401 || strings.Contains(w.Body.String(), "groups") {
		t.Fatal(w.Code, w.Body.String())
	}
}

func TestInboxCandidateFailuresAreTheFailure(t *testing.T) {
	f := newInboxFixture(t, nil)
	f.learning.errs = map[string]error{"candidates": errors.New("private disk path")}
	code, out := f.read(t, inboxBody())
	if code != 503 || strings.Contains(fmt.Sprint(out), "private") {
		t.Fatal(code, out)
	}
	f.learning.errs = map[string]error{"job:pin-1": errors.New("gone")}
	code, out = f.read(t, inboxBody())
	item := out["candidates"].(map[string]any)["items"].([]any)[0].(map[string]any)
	if code != 200 || item["classes"] != nil {
		t.Fatalf("a job that cannot be read still produced records: %d %v", code, item)
	}
	f.learning.errs = nil
	f.classes.err = map[string]error{"cohort": shoal.NewError(shoal.ErrorInvalidArgument, "window"), "other": errors.New("private")}
	_, out = f.read(t, inboxBody())
	classes := out["candidates"].(map[string]any)["items"].([]any)[0].(map[string]any)["classes"].([]any)
	if classes[0].(map[string]any)["error"] != "the declared window does not reach this cohort" || classes[1].(map[string]any)["error"] != "class record unavailable" || strings.Contains(fmt.Sprint(out), "private") {
		t.Fatalf("%v", classes)
	}
	// Without a class provider no record is attempted.
	g := newInboxFixture(t, func(s *InboxSurface) { s.Classes = nil })
	_, out = g.read(t, inboxBody())
	if out["candidates"].(map[string]any)["items"].([]any)[0].(map[string]any)["classes"] != nil || len(g.learning.jobs) != 0 {
		t.Fatal("records attempted without a provider")
	}
}
