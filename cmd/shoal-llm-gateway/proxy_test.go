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
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"

	admissionapi "github.com/phrocker/shoal-oss/pkg/admission/api"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

// fakePlane stands in for the explorer's admission surface.
type fakePlane struct {
	outcome  admissionapi.Outcome
	withhold []string
	token    *admissionapi.Token
	// echoTokenID mirrors what a real plane does: the claim is created under
	// the token ID the caller sent, so the grant carries that ID back. The fake
	// returned a fixed one, which made it unable to express the swapped-token
	// case at all. A probe that is specifically about the token's shape turns
	// this off, so the fixture does not repair the value under test.
	echoTokenID  bool
	status       int
	reportStatus int
	// errorBody, when set, is written with a non-200 status, as the plane's
	// own structured error answers are.
	errorBody string
	requests  []admissionapi.Request
	reports   []admissionapi.Report
	server    *httptest.Server
}

func newFakePlane(t *testing.T, outcome admissionapi.Outcome, withhold []string) *fakePlane {
	t.Helper()
	plane := &fakePlane{
		outcome: outcome, withhold: withhold,
		status: http.StatusOK, echoTokenID: true,
	}
	plane.token = &admissionapi.Token{
		ActionID: "YWN0aW9u", TokenID: "dG9rZW4", Version: 1,
		ExpiresAt: time.Now().Add(time.Minute),
	}
	plane.server = httptest.NewServer(http.HandlerFunc(
		func(writer http.ResponseWriter, request *http.Request) {
			if plane.status != http.StatusOK {
				if plane.errorBody != "" {
					writer.Header().Set("Content-Type", "application/json")
				}
				writer.WriteHeader(plane.status)
				_, _ = io.WriteString(writer, plane.errorBody)
				return
			}
			raw, _ := io.ReadAll(request.Body)
			switch {
			case strings.HasSuffix(request.URL.Path, "/request"):
				var decoded admissionapi.Request
				_ = json.Unmarshal(raw, &decoded)
				plane.requests = append(plane.requests, decoded)
				// As the handler does: withhold is always present.
				body := admissionapi.Grant{
					Outcome: plane.outcome, Withhold: append([]string{}, plane.withhold...),
				}
				if plane.outcome != admissionapi.OutcomeDenied {
					body.Token = plane.token
					if plane.echoTokenID && plane.token != nil {
						echoed := *plane.token
						echoed.TokenID = decoded.TokenID
						body.Token = &echoed
					}
				}
				_ = json.NewEncoder(writer).Encode(body)
			case strings.HasSuffix(request.URL.Path, "/report"):
				if plane.reportStatus != 0 {
					writer.WriteHeader(plane.reportStatus)
					return
				}
				var decoded admissionapi.Report
				_ = json.Unmarshal(raw, &decoded)
				plane.reports = append(plane.reports, decoded)
				// A real receipt, shaped as the handler encodes one: the client
				// checks that it acknowledges the reported admission.
				state := admissionapi.DispatchSucceeded
				if decoded.Failed {
					state = admissionapi.DispatchFailed
				}
				_ = json.NewEncoder(writer).Encode(admissionapi.Receipt{
					ActionID: decoded.Token.ActionID, Version: decoded.Token.Version + 1,
					State: state, ReportedAt: time.Now(),
				})
			default:
				writer.WriteHeader(http.StatusNotFound)
			}
		}))
	t.Cleanup(plane.server.Close)
	return plane
}

// fakeUpstream stands in for the model provider.
type fakeUpstream struct {
	received []byte
	status   int
	body     string
	calls    int
	// paths records what was actually requested. The fake answered every path
	// identically, which is why the acceptance test could not see the endpoint
	// being built with a duplicated version segment.
	paths    []string
	location string
	// headers the provider sets, so a test can assert what the proxy relays.
	headers map[string]string
	server  *httptest.Server
}

func newFakeUpstream(t *testing.T) *fakeUpstream {
	t.Helper()
	upstream := &fakeUpstream{status: http.StatusOK, body: `{"id":"done"}`}
	upstream.server = httptest.NewServer(http.HandlerFunc(
		func(writer http.ResponseWriter, request *http.Request) {
			upstream.calls++
			upstream.paths = append(upstream.paths, request.URL.Path)
			upstream.received, _ = io.ReadAll(request.Body)
			if upstream.location != "" {
				writer.Header().Set("Location", upstream.location)
			}
			for name, value := range upstream.headers {
				writer.Header().Set(name, value)
			}
			writer.WriteHeader(upstream.status)
			_, _ = io.WriteString(writer, upstream.body)
		}))
	t.Cleanup(upstream.server.Close)
	return upstream
}

func newTestProxy(t *testing.T, plane *fakePlane, upstream *fakeUpstream) (*proxy, *[]string) {
	t.Helper()
	var logged []string
	base, err := url.Parse(plane.server.URL)
	if err != nil {
		t.Fatal(err)
	}
	planeClient := newHTTPClient(5 * time.Second)
	planeClient.Transport = plane.server.Client().Transport
	client := &admissionClient{
		base: base, http: planeClient,
		credential:      func() (string, error) { return "plane-token", nil },
		agentID:         "agent",
		agentGeneration: 1,
		capability:      "llm.gateway", action: "complete",
		sourceID: []byte("source"), policyID: []byte("policy"),
		lease: time.Minute,
	}
	governed, err := newProxy(
		client, upstream.server.URL,
		func() (string, error) { return "upstream-key", nil },
		[]string{"example.test"},
		// The harness names a model, so the role/model classification tests
		// can tell a reported name from the marker.
		[]string{"gpt"},
		5*time.Second, time.Now,
		// The rendered line, not the format string. Capturing only the format
		// made TestNoPromptOrCompletionIsLogged unable to fail for the one
		// thing it is named for: content passed as an argument, which is how
		// content would actually reach a log.
		func(format string, values ...any) {
			logged = append(logged, fmt.Sprintf(format, values...))
		})
	if err != nil {
		t.Fatal(err)
	}
	// Only the transport is replaced. Assigning a whole http.Client here
	// discarded the production redirect policy, which is how a test asserting
	// that a redirect cannot carry the prompt passed against a client that
	// followed one.
	governed.client.Transport = upstream.server.Client().Transport
	return governed, &logged
}

func post(t *testing.T, governed *proxy, body string) *httptest.ResponseRecorder {
	t.Helper()
	request := httptest.NewRequest(
		http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	request.Host = "example.test"
	recorder := httptest.NewRecorder()
	governed.routes().ServeHTTP(recorder, request)
	return recorder
}

// Document IDs as the plane actually spells them. "doc-a" is not a reference:
// disclosures are decoded as unpadded base64url, so a readable name is a
// malformed request rather than an unknown document.
const (
	docA             = "ZG9jLWE"
	docB             = "ZG9jLWI"
	docNeverDeclared = "ZG9jLW5ldmVyLWRlY2xhcmVk"
)

const plainCall = `{"model":"gpt","messages":[{"role":"user","content":"hello"}]}`

// TestAnUnmodifiedClientIsGoverned is the acceptance criterion the proxy exists
// for: a caller that knows nothing about Shoal works through it unchanged.
func TestAnUnmodifiedClientIsGoverned(t *testing.T) {
	plane := newFakePlane(t, admissionapi.OutcomeAllowed, nil)
	upstream := newFakeUpstream(t)
	governed, _ := newTestProxy(t, plane, upstream)

	recorder := post(t, governed, plainCall)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", recorder.Code, recorder.Body.String())
	}
	if upstream.calls != 1 {
		t.Fatalf("upstream calls = %d, want 1", upstream.calls)
	}
	if recorder.Body.String() != `{"id":"done"}` {
		t.Fatalf("body = %q", recorder.Body.String())
	}
	if len(plane.requests) != 1 || len(plane.reports) != 1 {
		t.Fatalf("admissions = %d, reports = %d; want 1 and 1",
			len(plane.requests), len(plane.reports))
	}
}

// TestShoalNeverReceivesThePrompt is the property the whole design rests on.
//
// The admission surface takes a declaration precisely so the plane deciding
// whether content may be transmitted does not hold a copy of it. A proxy that
// forwarded the prompt for adjudication would defeat the thing it is built on.
func TestShoalNeverReceivesThePrompt(t *testing.T) {
	plane := newFakePlane(t, admissionapi.OutcomeAllowed, nil)
	upstream := newFakeUpstream(t)
	governed, _ := newTestProxy(t, plane, upstream)

	const secret = "the-sky-is-the-colour-of-television"
	post(t, governed, `{"model":"gpt","messages":[{"role":"user","content":"`+secret+`"}]}`)

	if len(plane.requests) != 1 {
		t.Fatalf("admissions = %d", len(plane.requests))
	}
	everything, err := json.Marshal(plane.requests[0])
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(everything), secret) {
		t.Fatalf("the prompt reached the decision plane: %s", everything)
	}
	// The declaration still has to be useful, or the plane is adjudicating
	// nothing.
	var declaration map[string]any
	if err := json.Unmarshal(plane.requests[0].Input, &declaration); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"model", "message_count", "content_bytes"} {
		if _, ok := declaration[key]; !ok {
			t.Fatalf("declaration omits %q: %s", key, plane.requests[0].Input)
		}
	}
}

// TestADenialNeverReachesTheUpstream is the stop this proxy is for. Everywhere
// else in Shoal enforcement withholds from a response that is still produced.
func TestADenialNeverReachesTheUpstream(t *testing.T) {
	plane := newFakePlane(t, admissionapi.OutcomeDenied, nil)
	upstream := newFakeUpstream(t)
	governed, _ := newTestProxy(t, plane, upstream)

	recorder := post(t, governed, plainCall)
	if recorder.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", recorder.Code)
	}
	if upstream.calls != 0 {
		t.Fatal("a denied call reached the upstream")
	}
	// A denial carries no token, so there is nothing to report against — and
	// reporting would be a statement that an effect occurred.
	if len(plane.reports) != 0 {
		t.Fatalf("a denial was reported: %#v", plane.reports)
	}
	assertRefusalNamesNothing(t, recorder.Body.String())
}

// TestAnUnreachablePlaneDeniesAndSaysSo covers the fail-closed requirement and
// the distinction the issue insists on: a caller told "denied" will not retry,
// one told "unavailable" should, and an operator needs to tell an outage from a
// policy change.
func TestAnUnreachablePlaneDeniesAndSaysSo(t *testing.T) {
	plane := newFakePlane(t, admissionapi.OutcomeAllowed, nil)
	plane.status = http.StatusInternalServerError
	upstream := newFakeUpstream(t)
	governed, logged := newTestProxy(t, plane, upstream)

	recorder := post(t, governed, plainCall)
	if recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503 so the caller can tell this from a denial",
			recorder.Code)
	}
	if upstream.calls != 0 {
		t.Fatal("an unreachable plane still let the call through")
	}
	var operatorSaw bool
	for _, line := range *logged {
		if strings.Contains(line, "unavailable") {
			operatorSaw = true
		}
	}
	if !operatorSaw {
		t.Fatalf("the operator cannot tell this from a policy denial: %v", *logged)
	}
	assertRefusalNamesNothing(t, recorder.Body.String())
}

// TestAWithholdWithoutAttributionIsRefused is what a withhold obligation does
// for a caller that attributes nothing (#426).
//
// It replaces a test that asserted a withheld reference was removed from the
// forwarded body and the others kept — the metadata edit — and passed, while
// the material went upstream: shoal_references is a flat list of IDs, the
// material lives in messages[].content as free text, and the ID and the
// material are different strings. Without shoal_attribution nothing connects
// them, so the gateway cannot find the bytes and refuses. With it, see
// TestWithheldMaterialNeverReachesTheProvider.
func TestAWithholdWithoutAttributionIsRefused(t *testing.T) {
	const material = "the restricted paragraph that doc-b actually contains"
	plane := newFakePlane(t, admissionapi.OutcomeObligated, []string{docB})
	upstream := newFakeUpstream(t)
	governed, _ := newTestProxy(t, plane, upstream)

	recorder := post(t, governed,
		`{"model":"gpt","messages":[{"role":"user","content":"`+material+`"}],`+
			`"shoal_references":["`+docA+`","`+docB+`"]}`)

	if recorder.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403: %s", recorder.Code, recorder.Body.String())
	}
	if upstream.calls != 0 {
		t.Fatalf("the call was forwarded under an obligation nothing enforced: %s",
			upstream.received)
	}
	if strings.Contains(string(upstream.received), material) {
		t.Fatal("withheld material reached the provider")
	}
	// Reported as unsatisfiable, so the plane learns its obligation could not
	// be met rather than that the caller went dark.
	if len(plane.reports) != 1 || !plane.reports[0].Failed ||
		plane.reports[0].ErrorCode != "obligation_unsatisfiable" {
		t.Fatalf("reports = %+v, want one naming the unsatisfiable obligation",
			plane.reports)
	}

	// An allow with no obligations still forwards, or this has turned the
	// proxy into something that refuses everything the plane constrains and
	// also everything it does not.
	plane.withhold, plane.outcome = nil, admissionapi.OutcomeAllowed
	plane.reports = nil
	if response := post(t, governed, plainCall); response.Code != http.StatusOK {
		t.Fatalf("an unobligated call was refused: %d", response.Code)
	}
	if upstream.calls != 1 {
		t.Fatalf("upstream calls = %d, want 1", upstream.calls)
	}
}

// TestAnUnsatisfiableObligationRefusesAndReports is the one case where refusing
// the whole call is right. The plane believes it constrained this call, so
// ignoring an obligation it cannot satisfy would be worse than refusing.
func TestAnUnsatisfiableObligationRefusesAndReports(t *testing.T) {
	// The obligation names a reference the caller never declared. This is a
	// different diagnosis from the case above — the plane and the caller
	// disagree about what this call is, rather than the proxy being unable to
	// locate content — and both refuse.
	plane := newFakePlane(t, admissionapi.OutcomeObligated, []string{docNeverDeclared})
	upstream := newFakeUpstream(t)
	governed, _ := newTestProxy(t, plane, upstream)

	recorder := post(t, governed,
		`{"model":"gpt","messages":[{"role":"user","content":"hi"}],`+
			`"shoal_references":["`+docA+`"]}`)
	if recorder.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", recorder.Code)
	}
	if upstream.calls != 0 {
		t.Fatal("an unsatisfiable obligation still reached the upstream")
	}
	// Reported, so the plane learns the obligation was unsatisfiable rather
	// than that the caller went dark.
	if len(plane.reports) != 1 || !plane.reports[0].Failed {
		t.Fatalf("reports = %#v, want one failure", plane.reports)
	}
	if plane.reports[0].ErrorCode != "obligation_unsatisfiable" {
		t.Fatalf("error code = %q", plane.reports[0].ErrorCode)
	}
}

// TestAnUpstreamFailureIsReportedAsOne keeps the loop honest: the plane must be
// able to tell a call that happened and failed from one that never happened.
func TestAnUpstreamFailureIsReportedAsOne(t *testing.T) {
	plane := newFakePlane(t, admissionapi.OutcomeAllowed, nil)
	upstream := newFakeUpstream(t)
	upstream.status = http.StatusTooManyRequests
	governed, _ := newTestProxy(t, plane, upstream)

	post(t, governed, plainCall)
	if len(plane.reports) != 1 {
		t.Fatalf("reports = %d, want 1", len(plane.reports))
	}
	if !plane.reports[0].Failed || plane.reports[0].ErrorCode != "upstream_error" {
		t.Fatalf("report = %#v, want a reported upstream failure", plane.reports[0])
	}
}

// TestTheReportCarriesNoCompletion is the other half of the content property:
// the prompt never goes in, and the completion never comes back out.
//
// This one is belt-and-braces rather than the real guarantee. reportOutcome is
// never given the response body, and relay streams it through without
// buffering, so no variable at the report site holds a completion — leaking
// one would require changing a signature. Injecting an unrelated literal into
// the report does not fail this test, and should not: it asserts that the
// actual completion does not travel, which is the property that matters.
func TestTheReportCarriesNoCompletion(t *testing.T) {
	plane := newFakePlane(t, admissionapi.OutcomeAllowed, nil)
	upstream := newFakeUpstream(t)
	const secret = "a-completion-nobody-should-store"
	upstream.body = `{"choices":[{"text":"` + secret + `"}]}`
	governed, _ := newTestProxy(t, plane, upstream)

	post(t, governed, plainCall)
	if len(plane.reports) != 1 {
		t.Fatalf("reports = %d", len(plane.reports))
	}
	everything, err := json.Marshal(plane.reports[0])
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(everything), secret) {
		t.Fatalf("the completion reached the decision plane: %s", everything)
	}
}

// TestNoPromptOrCompletionIsLogged pins the default configuration. An audit
// record references the admission, not the payload.
func TestNoPromptOrCompletionIsLogged(t *testing.T) {
	plane := newFakePlane(t, admissionapi.OutcomeAllowed, nil)
	upstream := newFakeUpstream(t)
	const prompt = "prompt-text-that-must-not-be-logged"
	const completion = "completion-text-that-must-not-be-logged"
	upstream.body = `{"text":"` + completion + `"}`
	governed, logged := newTestProxy(t, plane, upstream)

	post(t, governed, `{"model":"gpt","messages":[{"role":"user","content":"`+prompt+`"}]}`)
	for _, line := range *logged {
		if strings.Contains(line, prompt) || strings.Contains(line, completion) {
			t.Fatalf("content reached the log: %q", line)
		}
	}
}

// TestAnUnrecognisedOutcomeIsNotAnAllowance covers a plane newer than this
// proxy. A build that met an outcome it predates and treated it as permission
// would make every future outcome default to the permissive reading — the
// opposite of how every other unknown value in Shoal is handled.
func TestAnUnrecognisedOutcomeIsNotAnAllowance(t *testing.T) {
	plane := newFakePlane(t, "allowed_pending_review", nil)
	upstream := newFakeUpstream(t)
	governed, _ := newTestProxy(t, plane, upstream)

	recorder := post(t, governed, plainCall)
	if recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503: an outcome this build does not know "+
			"is not permission", recorder.Code)
	}
	if upstream.calls != 0 {
		t.Fatal("an unrecognised outcome let the call through")
	}
}

// TestAnAllowanceWithoutATokenIsRefused covers the other malformed grant. A
// call admitted with no token can never be reported, and an unreportable call
// is one the plane can never learn the outcome of.
func TestAnAllowanceWithoutATokenIsRefused(t *testing.T) {
	plane := newFakePlane(t, admissionapi.OutcomeAllowed, nil)
	plane.token = nil
	upstream := newFakeUpstream(t)
	governed, _ := newTestProxy(t, plane, upstream)

	recorder := post(t, governed, plainCall)
	if recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", recorder.Code)
	}
	if upstream.calls != 0 {
		t.Fatal("an allowance with no token let the call through")
	}
}

// TestAForeignHostIsRefusedBeforeAnythingHappens is the DNS-rebinding gate.
//
// The default listener is loopback, so without this a page in a browser can
// resolve a name it controls to 127.0.0.1, POST here, and spend the operator's
// upstream credential on a call nobody made. A JSON body is not protection: a
// form post with a text/plain content type reaches the same handler and needs
// no preflight.
func TestAForeignHostIsRefusedBeforeAnythingHappens(t *testing.T) {
	plane := newFakePlane(t, admissionapi.OutcomeAllowed, nil)
	upstream := newFakeUpstream(t)
	governed, _ := newTestProxy(t, plane, upstream)

	request := httptest.NewRequest(
		http.MethodPost, "/v1/chat/completions", strings.NewReader(plainCall))
	request.Host = "attacker.example"
	recorder := httptest.NewRecorder()
	governed.routes().ServeHTTP(recorder, request)

	if recorder.Code != http.StatusMisdirectedRequest {
		t.Fatalf("status = %d, want 421", recorder.Code)
	}
	if upstream.calls != 0 {
		t.Fatal("a misdirected request reached the upstream")
	}
	// Refused before the plane is even asked: the gate runs before routing, so
	// a rebound request costs no admission and no upstream credential.
	if len(plane.requests) != 0 {
		t.Fatalf("a misdirected request was adjudicated: %#v", plane.requests)
	}
	if strings.Contains(recorder.Body.String(), "attacker.example") {
		t.Fatalf("the refusal echoes the submitted authority: %s", recorder.Body.String())
	}
}

// assertRefusalNamesNothing checks a caller-facing refusal discloses nothing. A
// refusal that explains itself is an oracle over whatever it explains.
func assertRefusalNamesNothing(t *testing.T, body string) {
	t.Helper()
	for _, leak := range []string{"policy", "compartment", "doc-", "source", "agent"} {
		if strings.Contains(strings.ToLower(body), leak) {
			t.Fatalf("refusal names %q: %s", leak, body)
		}
	}
}

// deadlineRecorder records how long each outbound admission call was given.
//
// The bound cannot be observed from the plane's side: HTTP carries no client
// deadline, so a handler sees only its own connection's context. It has to be
// read from the outbound request before it leaves.
type deadlineRecorder struct {
	inner    http.RoundTripper
	headroom map[string]time.Duration
}

func (d *deadlineRecorder) RoundTrip(request *http.Request) (*http.Response, error) {
	key := "request"
	if strings.HasSuffix(request.URL.Path, "/report") {
		key = "report"
	}
	if deadline, ok := request.Context().Deadline(); ok {
		d.headroom[key] = time.Until(deadline)
	} else {
		d.headroom[key] = -1
	}
	return d.inner.RoundTrip(request)
}

// TestTheReportFitsInTheWindowReservedForIt closes the gap between the two
// numbers that have to agree.
//
// validateDurations withholds minimumReportWindow from the lease so the report
// can still be made after the upstream call returns. The report then has to fit
// in that margin — and it did not: it was given its own ten-second timeout
// against a window reserved as five, so a report that used the time it was
// granted outlived the lease it was closing. The failure needs an upstream call
// that runs to the edge of the timeout to appear in production, which is why no
// existing test saw it.
//
// Asserting an upper bound rather than an exact value is deliberate: a report
// bounded more tightly than the reservation is still correct, one bounded
// looser is the bug. The lower bound is here too, because an unbounded report
// hangs the goroutine that should be closing the grant.
func TestTheReportFitsInTheWindowReservedForIt(t *testing.T) {
	plane := newFakePlane(t, admissionapi.OutcomeAllowed, nil)
	upstream := newFakeUpstream(t)
	governed, _ := newTestProxy(t, plane, upstream)
	recorder := &deadlineRecorder{
		inner:    plane.server.Client().Transport,
		headroom: map[string]time.Duration{},
	}
	governed.admission.http = &http.Client{Transport: recorder}

	if response := post(t, governed, plainCall); response.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", response.Code, response.Body.String())
	}
	if len(plane.reports) != 1 {
		t.Fatalf("reports = %d, want 1", len(plane.reports))
	}

	headroom, seen := recorder.headroom["report"]
	if !seen {
		t.Fatal("the report never went out, so its bound was never exercised")
	}
	if headroom <= 0 {
		t.Fatal("the report carried no deadline; an unbounded report can " +
			"outlive the lease it closes and never return")
	}
	if headroom > minimumReportWindow {
		t.Fatalf("the report was given %s inside a window reserved as %s, so a "+
			"report that takes the time it is granted expires the lease",
			headroom, minimumReportWindow)
	}
}

// TestTheUpstreamPathIsTheDocumentedOne pins the endpoint the proxy builds.
//
// An OpenAI-compatible base URL conventionally carries the version segment —
// OPENAI_BASE_URL is https://api.openai.com/v1, and the chart's own case uses
// http://localhost:11434/v1. The proxy appended "v1/chat/completions" to that,
// producing /v1/v1/chat/completions: a 404 from every real provider, against
// the configuration the chart and the deployment guide both document.
//
// No test could see it because the fake upstream answered every path the same
// way. That is the recurring shape of the defects found on this PR — a fixture
// unable to express the condition, rather than a missing assertion — so the
// fake records the path now and this asserts it exactly.
func TestTheUpstreamPathIsTheDocumentedOne(t *testing.T) {
	plane := newFakePlane(t, admissionapi.OutcomeAllowed, nil)
	upstream := newFakeUpstream(t)
	governed, _ := newTestProxy(t, plane, upstream)
	// A base with the version segment, as every OpenAI-compatible client and
	// the chart's own rendering case supply it.
	base, err := url.Parse(upstream.server.URL + "/v1")
	if err != nil {
		t.Fatal(err)
	}
	governed.upstream = base

	if response := post(t, governed, plainCall); response.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", response.Code, response.Body.String())
	}
	if len(upstream.paths) != 1 || upstream.paths[0] != "/v1/chat/completions" {
		t.Fatalf("upstream path = %v, want [/v1/chat/completions]", upstream.paths)
	}
}

// TestARedirectCannotCarryThePromptOffTheCheckedTransport covers a hole in a
// rule that is otherwise enforced carefully.
//
// absoluteURL settles the scheme of the URL that is configured. A followed
// redirect is a different URL it never saw, and a 307 or 308 preserves the
// method and the body — so one hop to http:// replays the prompt, and on the
// upstream path the operator's credential with it, past a check that passed.
// Go keeps the Authorization header across a same-host redirect, which is the
// case that matters most.
//
// Both clients are covered, because the admission client had the same default
// and the declaration is still worth not leaking.
func TestARedirectCannotCarryThePromptOffTheCheckedTransport(t *testing.T) {
	// Where a followed redirect would land. Nothing may reach it.
	var landed int
	elsewhere := httptest.NewServer(http.HandlerFunc(
		func(writer http.ResponseWriter, _ *http.Request) {
			landed++
			writer.WriteHeader(http.StatusOK)
		}))
	t.Cleanup(elsewhere.Close)

	t.Run("upstream", func(t *testing.T) {
		plane := newFakePlane(t, admissionapi.OutcomeAllowed, nil)
		upstream := newFakeUpstream(t)
		upstream.status, upstream.location, upstream.body =
			http.StatusTemporaryRedirect, elsewhere.URL+"/v1/chat/completions", ""
		governed, _ := newTestProxy(t, plane, upstream)

		response := post(t, governed, plainCall)
		if landed != 0 {
			t.Fatal("the prompt was replayed to the redirect target")
		}
		// The 3xx is handed back as the provider's own answer, so the caller
		// sees what happened rather than a synthesised error.
		if response.Code != http.StatusTemporaryRedirect {
			t.Fatalf("status = %d, want the upstream's 307", response.Code)
		}
		// And it is still reported: an unreported grant is the outcome this
		// proxy exists to prevent, redirect or not.
		if len(plane.reports) != 1 {
			t.Fatalf("reports = %d, want 1", len(plane.reports))
		}
	})

	t.Run("admission", func(t *testing.T) {
		landed = 0
		plane := newFakePlane(t, admissionapi.OutcomeAllowed, nil)
		upstream := newFakeUpstream(t)
		governed, _ := newTestProxy(t, plane, upstream)
		redirecting := httptest.NewServer(http.HandlerFunc(
			func(writer http.ResponseWriter, request *http.Request) {
				writer.Header().Set("Location", elsewhere.URL+request.URL.Path)
				writer.WriteHeader(http.StatusTemporaryRedirect)
			}))
		t.Cleanup(redirecting.Close)
		base, err := url.Parse(redirecting.URL)
		if err != nil {
			t.Fatal(err)
		}
		governed.admission.base = base

		response := post(t, governed, plainCall)
		if landed != 0 {
			t.Fatal("the declaration and bearer token were replayed to the redirect target")
		}
		// No decision means no call. The redirect is not an allow.
		if upstream.calls != 0 {
			t.Fatal("the call was forwarded without a decision")
		}
		if response.Code != http.StatusServiceUnavailable {
			t.Fatalf("status = %d, want 503", response.Code)
		}
	})
}

// TestALoopbackProviderNeedsNoCredential covers a configuration the chart
// asserts renders and the binary refused on every call.
//
// validate-chart.sh has a case named "loopback upstream needs no credential"
// and the deployment guide says a loopback provider needs none — a local model
// server generally has no notion of one. The forward path demanded a credential
// unconditionally, so that documented configuration spent an admission and then
// refused the call it had just been granted.
func TestALoopbackProviderNeedsNoCredential(t *testing.T) {
	absent := func() (string, error) { return "", ErrNoCredential }
	// Configured and unreadable, which must not be mistaken for absent.
	broken := func() (string, error) {
		return "", errors.New("/run/secrets/key is unreadable: permission denied")
	}

	t.Run("loopback forwards unauthenticated", func(t *testing.T) {
		plane := newFakePlane(t, admissionapi.OutcomeAllowed, nil)
		upstream := newFakeUpstream(t)
		governed, _ := newTestProxy(t, plane, upstream)
		governed.credential = absent

		if response := post(t, governed, plainCall); response.Code != http.StatusOK {
			t.Fatalf("status = %d: %s", response.Code, response.Body.String())
		}
		if upstream.calls != 1 {
			t.Fatalf("upstream calls = %d, want 1", upstream.calls)
		}
	})

	// The case the first version of this fix got wrong, and the reason the
	// exemption is keyed on a sentinel rather than on any error at all.
	//
	// A loopback upstream whose credential file has the wrong permissions is
	// not a loopback upstream that wants no credential. Treating every error as
	// absence forwarded the prompt with no Authorization header, and a comment
	// claimed the opposite while nothing in the code could make it true.
	t.Run("a broken credential is not an absent one", func(t *testing.T) {
		plane := newFakePlane(t, admissionapi.OutcomeAllowed, nil)
		upstream := newFakeUpstream(t)
		governed, _ := newTestProxy(t, plane, upstream)
		governed.credential = broken

		if response := post(t, governed, plainCall); response.Code != http.StatusBadGateway {
			t.Fatalf("status = %d, want 502", response.Code)
		}
		if upstream.calls != 0 {
			t.Fatal("a prompt was forwarded unauthenticated past a broken credential")
		}
		if len(plane.reports) != 1 ||
			plane.reports[0].ErrorCode != "upstream_credential_unavailable" {
			t.Fatalf("reports = %+v, want one naming the credential", plane.reports)
		}
	})

	// The same property through the real credential readers rather than a stub.
	//
	// A mutation showed this was needed: making credentialFromFile's unreadable
	// error wrap ErrNoCredential passed every test above, because all of them
	// supplied the error directly. The sentinel's meaning is a contract between
	// two functions, so a test that writes the error itself cannot check that
	// the producer honours it — only that the consumer reads it.
	t.Run("the real readers classify themselves correctly", func(t *testing.T) {
		plane := newFakePlane(t, admissionapi.OutcomeAllowed, nil)
		upstream := newFakeUpstream(t)
		governed, _ := newTestProxy(t, plane, upstream)

		// A named file that cannot be read is a configured credential, so the
		// loopback exemption must not apply to it.
		governed.credential = credentialFromFile(
			filepath.Join(t.TempDir(), "never-created"))
		if response := post(t, governed, plainCall); response.Code != http.StatusBadGateway {
			t.Fatalf("an unreadable key file was treated as no key file: %d",
				response.Code)
		}
		if upstream.calls != 0 {
			t.Fatal("a prompt was forwarded unauthenticated past an unreadable key file")
		}

		// An unset variable is absence: the chart renders no env entry at all
		// when no Secret is named, which is how a loopback upstream is meant
		// to be configured. This is the case that must still forward.
		governed.credential = credentialFromEnv(
			"SHOAL_TEST_UPSTREAM_KEY_DELIBERATELY_UNSET")
		if response := post(t, governed, plainCall); response.Code != http.StatusOK {
			t.Fatalf("a loopback upstream with no credential was refused: %d",
				response.Code)
		}
		if upstream.calls != 1 {
			t.Fatalf("upstream calls = %d, want 1", upstream.calls)
		}
	})

	// Optional is not ignored. A remote provider with no credential fails here
	// rather than being sent an unauthenticated prompt, which would reach a
	// third party and be rejected — after the egress.
	t.Run("a remote provider still requires one", func(t *testing.T) {
		plane := newFakePlane(t, admissionapi.OutcomeAllowed, nil)
		upstream := newFakeUpstream(t)
		governed, _ := newTestProxy(t, plane, upstream)
		governed.credential = absent
		remote, err := url.Parse("https://api.example.test/v1")
		if err != nil {
			t.Fatal(err)
		}
		governed.upstream = remote

		if response := post(t, governed, plainCall); response.Code != http.StatusBadGateway {
			t.Fatalf("status = %d, want 502", response.Code)
		}
		if upstream.calls != 0 {
			t.Fatal("a prompt was sent to a remote provider with no credential")
		}
		if len(plane.reports) != 1 || plane.reports[0].ErrorCode != "upstream_credential_unavailable" {
			t.Fatalf("reports = %+v, want one naming the missing credential", plane.reports)
		}
	})
}

// TestAStreamedResponseReachesTheCallerAsItArrives covers the streaming branch
// of relay, which coverage put at 11% — effectively only the non-streaming
// io.Copy path had ever run.
//
// #390 requires streaming explicitly, and the acceptance criterion is specific:
// "a streamed response is reported in full after completion, and the report is
// what feeds the next admission." Both halves need a real server. An
// httptest.ResponseRecorder is not an http.Flusher, so under a recorder the
// flush is skipped and the test cannot distinguish a proxy that streams from
// one that buffers the whole body and writes it at the end.
//
// The upstream here holds the second chunk back until the first has been read
// through the proxy. If the proxy buffered, that read would block until the
// upstream gave up, so the first chunk arriving is itself the assertion.
func TestAStreamedResponseReachesTheCallerAsItArrives(t *testing.T) {
	released := make(chan struct{})
	firstSeen := make(chan struct{})
	plane := newFakePlane(t, admissionapi.OutcomeAllowed, nil)

	upstream := httptest.NewServer(http.HandlerFunc(
		func(writer http.ResponseWriter, _ *http.Request) {
			writer.Header().Set("Content-Type", "text/event-stream")
			writer.WriteHeader(http.StatusOK)
			flusher, ok := writer.(http.Flusher)
			if !ok {
				t.Error("the test upstream cannot flush, so it proves nothing")
				return
			}
			_, _ = io.WriteString(writer, "data: first\n\n")
			flusher.Flush()
			select {
			case <-released:
			case <-time.After(5 * time.Second):
				t.Error("the first chunk never reached the caller, so the proxy buffered")
			}
			_, _ = io.WriteString(writer, "data: second\n\n")
			flusher.Flush()
		}))
	t.Cleanup(upstream.Close)

	base, err := url.Parse(plane.server.URL)
	if err != nil {
		t.Fatal(err)
	}
	planeClient := newHTTPClient(5 * time.Second)
	planeClient.Transport = plane.server.Client().Transport
	governed, err := newProxy(
		&admissionClient{
			base: base, http: planeClient,
			credential:      func() (string, error) { return "plane-token", nil },
			agentID:         "agent",
			agentGeneration: 1,
			capability:      "llm.gateway", action: "complete",
			sourceID: []byte("source"), policyID: []byte("policy"),
			lease: time.Minute,
		},
		upstream.URL,
		func() (string, error) { return "upstream-key", nil },
		[]string{"example.test"}, []string{"gpt"}, 10*time.Second, time.Now,
		func(string, ...any) {})
	if err != nil {
		t.Fatal(err)
	}
	governed.client.Transport = upstream.Client().Transport

	// A real server, because the flush has to be observable end to end.
	front := httptest.NewServer(governed.routes())
	t.Cleanup(front.Close)

	request, err := http.NewRequest(http.MethodPost, front.URL+"/v1/chat/completions",
		strings.NewReader(`{"model":"gpt","stream":true,`+
			`"messages":[{"role":"user","content":"hi"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	request.Host = "example.test"
	response, err := front.Client().Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", response.StatusCode)
	}

	// Read the first event before the upstream has produced the second.
	go func() {
		buffer := make([]byte, 64)
		read, readErr := response.Body.Read(buffer)
		if readErr == nil && read > 0 && strings.Contains(string(buffer[:read]), "first") {
			close(firstSeen)
		}
	}()
	select {
	case <-firstSeen:
	case <-time.After(5 * time.Second):
		t.Fatal("no chunk arrived before the stream completed: relay is buffering")
	}
	close(released)

	rest, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(rest), "second") {
		t.Fatalf("the rest of the stream was lost: %q", rest)
	}

	// Reported once, after the stream finished, and with the full byte count.
	// Reporting before the end would record an outcome the proxy had not yet
	// observed, which is why forward reports in exactly one place.
	deadline := time.Now().Add(5 * time.Second)
	for len(plane.reports) == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if len(plane.reports) != 1 {
		t.Fatalf("reports = %d, want exactly 1 after completion", len(plane.reports))
	}
	var outcome struct {
		Status int   `json:"upstream_status"`
		Bytes  int64 `json:"response_bytes"`
	}
	if err = json.Unmarshal(plane.reports[0].Outcome, &outcome); err != nil {
		t.Fatal(err)
	}
	const total = len("data: first\n\n") + len("data: second\n\n")
	if outcome.Bytes != int64(total) {
		t.Fatalf("reported %d bytes, want %d: a partial count means the report "+
			"does not describe the egress that happened", outcome.Bytes, total)
	}
	if outcome.Status != http.StatusOK {
		t.Fatalf("reported status = %d", outcome.Status)
	}
	// The completion itself is never in the report. That is structural — the
	// report is not given the body — and worth asserting against a change that
	// would quietly add it.
	if strings.Contains(string(plane.reports[0].Outcome), "first") ||
		strings.Contains(string(plane.reports[0].Outcome), "second") {
		t.Fatalf("the completion reached the plane: %s", plane.reports[0].Outcome)
	}
}

// TestAnInfrastructuralFailureIsNeverReportedAsAPolicyDenial is the acceptance
// criterion from #390 that had no test: "an unreachable decision plane denies,
// and the operator can tell that apart from a policy denial."
//
// The distinction is load-bearing in both directions. A caller told "denied"
// will not retry; one told "unavailable" should. And the operator needs it to
// tell an outage from a policy change — this is the one surface in Shoal where
// a denial means the work does not happen at all, so a fail-closed outage is a
// total outage for everyone behind the proxy.
//
// The subtlest entry is 401/403 from the plane. Those look like denials and are
// not: the plane refused *this proxy's* credential, so the caller's request was
// never adjudicated. Classifying them as policy denials would tell every caller
// their request was refused on the merits while the real fault was a stale
// token in the proxy's own Secret.
func TestAnInfrastructuralFailureIsNeverReportedAsAPolicyDenial(t *testing.T) {
	for _, probe := range []struct {
		name   string
		status int
	}{
		{"the plane rejects the proxy's own credential", http.StatusUnauthorized},
		{"the plane forbids the proxy itself", http.StatusForbidden},
		{"the plane is broken", http.StatusInternalServerError},
		{"the admission route is absent", http.StatusNotFound},
		{"the plane is unavailable", http.StatusServiceUnavailable},
	} {
		plane := newFakePlane(t, admissionapi.OutcomeAllowed, nil)
		plane.status = probe.status
		upstream := newFakeUpstream(t)
		governed, logged := newTestProxy(t, plane, upstream)

		recorder := post(t, governed, plainCall)
		if recorder.Code != http.StatusServiceUnavailable {
			t.Fatalf("%s: status = %d, want 503 — a 403 would tell the caller "+
				"its request was refused on the merits", probe.name, recorder.Code)
		}
		if strings.Contains(recorder.Body.String(), `"denied"`) {
			t.Fatalf("%s: the refusal is shaped as a policy denial: %s",
				probe.name, recorder.Body.String())
		}
		if !strings.Contains(recorder.Body.String(), "plane_unavailable") {
			t.Fatalf("%s: body = %s", probe.name, recorder.Body.String())
		}
		if upstream.calls != 0 {
			t.Fatalf("%s: the call was forwarded with no decision", probe.name)
		}
		// And the operator's half of the criterion: the log says which kind of
		// failure it was. Without this the two are indistinguishable from
		// outside, since both end in a refusal.
		joined := strings.Join(*logged, "\n")
		if !strings.Contains(joined, "unavailable") {
			t.Fatalf("%s: nothing in the log names this as infrastructural: %q",
				probe.name, joined)
		}
		if strings.Contains(joined, "admission denied") {
			t.Fatalf("%s: logged as a policy denial: %q", probe.name, joined)
		}
	}

	// The contrast, or the assertions above only prove the proxy refuses
	// everything. A real policy denial is 403, shaped as denied, and logged as
	// a denial.
	plane := newFakePlane(t, admissionapi.OutcomeDenied, nil)
	upstream := newFakeUpstream(t)
	governed, logged := newTestProxy(t, plane, upstream)
	recorder := post(t, governed, plainCall)
	if recorder.Code != http.StatusForbidden {
		t.Fatalf("a policy denial = %d, want 403", recorder.Code)
	}
	if !strings.Contains(recorder.Body.String(), "denied") {
		t.Fatalf("a policy denial is not shaped as one: %s", recorder.Body.String())
	}
	if upstream.calls != 0 {
		t.Fatal("a denied call reached the upstream")
	}
	if joined := strings.Join(*logged, "\n"); !strings.Contains(joined, "admission denied") {
		t.Fatalf("a policy denial was not logged as one: %q", joined)
	}
}

// TestAMalformedRequestIsRefusedWithoutSpendingAnAdmission covers the 400 paths
// and the bound on what the proxy will read.
//
// The classification matters as much as the refusal: a malformed request is the
// caller's fault and must not be reported as a plane problem, and it must not
// consume a decision — asking the plane about a request that cannot be parsed
// spends a grant on a call that was never going to happen.
func TestAMalformedRequestIsRefusedWithoutSpendingAnAdmission(t *testing.T) {
	for _, probe := range []struct{ name, body string }{
		{"not JSON", `{"model":`},
		{"no model", `{"messages":[{"role":"user","content":"hi"}]}`},
		{"blank model", `{"model":"  ","messages":[{"role":"user","content":"hi"}]}`},
		{"no messages", `{"model":"gpt"}`},
		{"empty messages", `{"model":"gpt","messages":[]}`},
		{"messages of the wrong type", `{"model":"gpt","messages":"hello"}`},
		{"stream of the wrong type", `{"model":"gpt","stream":"yes",` +
			`"messages":[{"role":"user","content":"hi"}]}`},
		{"references of the wrong type", `{"model":"gpt",` +
			`"messages":[{"role":"user","content":"hi"}],"shoal_references":"` +
			docA + `"}`},
		// A readable name, which is what anyone would pass and what no
		// reference can be: the plane decodes disclosures as unpadded
		// base64url, so this is permanently malformed. Without the local
		// check it reached the plane as a 400 that the admission client turns into
		// ErrPlaneUnreachable, and the caller was told 503 — retry — for
		// something no retry can fix.
		{"a readable reference name", `{"model":"gpt",` +
			`"messages":[{"role":"user","content":"hi"}],` +
			`"shoal_references":["doc-a"]}`},
		{"a padded reference", `{"model":"gpt",` +
			`"messages":[{"role":"user","content":"hi"}],` +
			`"shoal_references":["ZG9jLWE="]}`},
		{"a blank reference", `{"model":"gpt",` +
			`"messages":[{"role":"user","content":"hi"}],"shoal_references":["  "]}`},
	} {
		plane := newFakePlane(t, admissionapi.OutcomeAllowed, nil)
		upstream := newFakeUpstream(t)
		governed, _ := newTestProxy(t, plane, upstream)

		recorder := post(t, governed, probe.body)
		if recorder.Code != http.StatusBadRequest {
			t.Fatalf("%s: status = %d, want 400", probe.name, recorder.Code)
		}
		if len(plane.requests) != 0 {
			t.Fatalf("%s: an unparseable request consumed a decision", probe.name)
		}
		if upstream.calls != 0 {
			t.Fatalf("%s: it reached the upstream anyway", probe.name)
		}
	}

	// The bound. Without it a caller can make the proxy hold arbitrary memory
	// before any decision is taken, and the refusal has to come from the limit
	// rather than from the body failing to parse afterwards.
	plane := newFakePlane(t, admissionapi.OutcomeAllowed, nil)
	upstream := newFakeUpstream(t)
	governed, _ := newTestProxy(t, plane, upstream)
	oversized := `{"model":"gpt","messages":[{"role":"user","content":"` +
		strings.Repeat("A", maxRequestBytes) + `"}]}`
	recorder := post(t, governed, oversized)
	if recorder.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("an oversized body = %d, want 413", recorder.Code)
	}
	if len(plane.requests) != 0 {
		t.Fatal("an oversized body consumed a decision")
	}
	// A body just under the bound is still served, or the limit is a denial of
	// the feature rather than a bound on it.
	plane.requests = nil
	justUnder := `{"model":"gpt","messages":[{"role":"user","content":"` +
		strings.Repeat("A", maxRequestBytes/2) + `"}]}`
	if recorder = post(t, governed, justUnder); recorder.Code != http.StatusOK {
		t.Fatalf("a body under the bound = %d, want 200", recorder.Code)
	}
}

// TestATruncatedStreamIsReportedAsTruncated covers the one failure that happens
// after the egress and can still be told to the plane.
//
// The report is what feeds the next admission (#389), so "the caller received
// the whole completion" and "the connection broke halfway" must not arrive as
// the same outcome. The proxy cannot recall tokens already sent, which is
// exactly why it has to be accurate about how much went.
func TestATruncatedStreamIsReportedAsTruncated(t *testing.T) {
	plane := newFakePlane(t, admissionapi.OutcomeAllowed, nil)
	upstream := newFakeUpstream(t)
	governed, _ := newTestProxy(t, plane, upstream)

	// failAfter is 0 — the caller is gone before any byte is written.
	//
	// It was 1 at first, with an upstream sending four chunks, and the test
	// failed: the fake wrote the whole body in a single call, so relay did one
	// Write and the first-write allowance was never used up. The fixture could
	// not express the condition, which is the fourth time that exact shape has
	// appeared on this branch. The multi-write case is covered separately
	// below, against an upstream that really does flush twice.
	failing := &failingWriter{header: http.Header{}, failAfter: 0}
	request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"gpt","stream":true,`+
			`"messages":[{"role":"user","content":"hi"}]}`))
	request.Host = "example.test"
	upstream.body = strings.Repeat("data: chunk\n\n", 4)
	governed.routes().ServeHTTP(failing, request)

	if len(plane.reports) != 1 {
		t.Fatalf("reports = %d, want 1", len(plane.reports))
	}
	if !plane.reports[0].Failed ||
		plane.reports[0].ErrorCode != "response_truncated" {
		t.Fatalf("report = %+v, want one naming the truncation", plane.reports[0])
	}

	// Truncated partway, against an upstream that flushes twice so relay
	// really does write more than once. The reported byte count must be what
	// actually left, not what the upstream offered: the plane is told how much
	// of the completion escaped, and an over-count is as wrong as a failure
	// that goes unreported.
	const firstChunk = "data: one\n\n"
	streaming := httptest.NewServer(http.HandlerFunc(
		func(writer http.ResponseWriter, _ *http.Request) {
			flusher, ok := writer.(http.Flusher)
			if !ok {
				t.Error("the test upstream cannot flush")
				return
			}
			writer.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(writer, firstChunk)
			flusher.Flush()
			time.Sleep(50 * time.Millisecond)
			_, _ = io.WriteString(writer, "data: two\n\n")
			flusher.Flush()
		}))
	t.Cleanup(streaming.Close)

	plane.reports = nil
	streamBase, err := url.Parse(streaming.URL)
	if err != nil {
		t.Fatal(err)
	}
	governed.upstream = streamBase
	governed.client.Transport = streaming.Client().Transport

	partial := &failingWriter{header: http.Header{}, failAfter: 1}
	request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"gpt","stream":true,`+
			`"messages":[{"role":"user","content":"hi"}]}`))
	request.Host = "example.test"
	governed.routes().ServeHTTP(partial, request)

	if len(plane.reports) != 1 {
		t.Fatalf("reports = %d, want 1", len(plane.reports))
	}
	if plane.reports[0].ErrorCode != "response_truncated" {
		t.Fatalf("report = %+v, want one naming the truncation", plane.reports[0])
	}

	// The count no longer travels in an outcome — a failed report still
	// carries none, which the admission surface refuses — but as the report's
	// effected volume (#427). This harness's upstream is loopback, so the
	// proxy declares no egress and the volume is omitted here; the counted
	// cases are in egress_test.go, against a proxy declaring egress.
	if len(plane.reports[0].Outcome) != 0 {
		t.Fatalf("a failed report carried an outcome, which the admission "+
			"surface refuses: %s", plane.reports[0].Outcome)
	}
	if plane.reports[0].Effected != nil {
		t.Fatalf("a proxy declaring no egress reported a volume the plane "+
			"would refuse: %+v", plane.reports[0].Effected)
	}
	_ = firstChunk
}

// failingWriter writes a bounded number of times and then fails, standing in
// for a caller that disconnects mid-stream.
type failingWriter struct {
	header    http.Header
	writes    int
	failAfter int
	status    int
}

func (f *failingWriter) Header() http.Header  { return f.header }
func (f *failingWriter) WriteHeader(code int) { f.status = code }
func (f *failingWriter) Write(data []byte) (int, error) {
	f.writes++
	if f.writes > f.failAfter {
		return 0, errors.New("connection reset by peer")
	}
	return len(data), nil
}

// TestTheHostGateHonoursEveryClaimItsCommentMakes pins the three defenses the
// gate documents and had no test for.
//
// guardHost's comment says "exact match, no wildcard and no suffix form, and
// X-Forwarded-Host is never consulted; each of those is a known bypass." Only
// a wholly foreign authority was covered, which the first of those three would
// catch on its own — so the suffix and header claims were assertions in a
// comment. On this branch that has been the reliable predictor of a defect, so
// they are assertions in a test now.
func TestTheHostGateHonoursEveryClaimItsCommentMakes(t *testing.T) {
	// Two authorities, one with a port, so the port-sensitivity of an exact
	// match is covered in both directions rather than assumed.
	allowed := []string{"example.test", "gateway.internal:8100"}

	for _, probe := range []struct {
		name      string
		host      string
		forwarded string
		admit     bool
	}{
		{"the configured authority", "example.test", "", true},
		{"the configured authority with a port", "gateway.internal:8100", "", true},

		// Case and the FQDN root are folded, symmetrically, because hostnames
		// are case-insensitive and "example.test." names the same host.
		{"uppercase", "EXAMPLE.TEST", "", true},
		{"mixed case", "Example.Test", "", true},
		{"a trailing dot", "example.test.", "", true},
		{"uppercase with a trailing dot", "EXAMPLE.TEST.", "", true},

		// No suffix form. These are the rebinding payloads that a naive
		// HasSuffix or Contains check admits, and each is a distinct trick.
		{"a prefixed label", "evil.example.test", "", false},
		{"the authority as a prefix of a longer one", "example.test.attacker.com", "", false},
		{"a hyphen-glued neighbour", "evil-example.test", "", false},
		{"the authority inside a longer label", "notexample.test", "", false},

		// Exact includes the port. An allow-list entry names the authority
		// clients actually send, and a port-less entry does not stand in for
		// every port on that host.
		{"a port where none was configured", "example.test:8100", "", false},
		{"the wrong port", "gateway.internal:9999", "", false},
		{"no port where one was configured", "gateway.internal", "", false},

		// Degenerate authorities must not match anything, including a
		// configured entry that was somehow blank.
		{"an empty authority", "", "", false},
		{"whitespace", "   ", "", false},
		{"a bare port", ":8100", "", false},

		// X-Forwarded-Host is never consulted, in either direction. Trusting
		// it lets anything that can set a header name its own authority; and
		// a legitimate request must not be refused because a proxy in front
		// added one.
		{"a foreign host claiming an allowed authority", "attacker.example", "example.test", false},
		{"an allowed host with a foreign forwarded header", "example.test", "attacker.example", true},
		{"an empty host with an allowed forwarded header", "", "example.test", false},
	} {
		plane := newFakePlane(t, admissionapi.OutcomeAllowed, nil)
		upstream := newFakeUpstream(t)
		governed, _ := newTestProxy(t, plane, upstream)
		governed.allowedHosts = allowed

		request := httptest.NewRequest(
			http.MethodPost, "/v1/chat/completions", strings.NewReader(plainCall))
		request.Host = probe.host
		if probe.forwarded != "" {
			request.Header.Set("X-Forwarded-Host", probe.forwarded)
		}
		recorder := httptest.NewRecorder()
		governed.routes().ServeHTTP(recorder, request)

		if probe.admit {
			if recorder.Code != http.StatusOK {
				t.Fatalf("%s (%q): status = %d, want 200 — a gate nothing valid "+
					"passes is a gate against the feature", probe.name, probe.host,
					recorder.Code)
			}
			continue
		}
		if recorder.Code != http.StatusMisdirectedRequest {
			t.Fatalf("%s (%q): status = %d, want 421", probe.name, probe.host,
				recorder.Code)
		}
		// Refused before routing, so a rebound request costs no admission and
		// no upstream credential. This is the half that makes the gate cheap.
		if len(plane.requests) != 0 {
			t.Fatalf("%s: a misdirected request was adjudicated", probe.name)
		}
		if upstream.calls != 0 {
			t.Fatalf("%s: it reached the upstream", probe.name)
		}
		if probe.host != "" && strings.Contains(recorder.Body.String(), probe.host) {
			t.Fatalf("%s: the refusal echoes the submitted authority: %s",
				probe.name, recorder.Body.String())
		}
	}
}

// TestTheUpstreamCallFitsTheGrantItWasActuallyGiven covers the gap between the
// lease that was asked for and the one that came back.
//
// validateDurations makes the *configured* lease outlast the configured timeout
// with room to report. That is not enough: the fleet clamps the granted expiry
// to the deadline this proxy sent before the admission round trip
// (dispatch_service.go:381-383), so a slow plane spends the headroom. With the
// chart's own 60s lease and 30s timeout, a 28s admission leaves 32s — reportable
// says yes, 32s exceeds the 5s window — and a 30s upstream call then leaves 2s.
// The egress happens and the report is refused as expired, which is the one
// outcome this proxy exists to prevent, reached through a slow plane rather than
// through misconfiguration.
func TestTheUpstreamCallFitsTheGrantItWasActuallyGiven(t *testing.T) {
	// A grant that was reportable when it arrived and is not by the time the
	// call would start.
	//
	// The two checks share a threshold — reportable wants at least the report
	// window remaining, and a positive budget wants more than it — so this is
	// only reachable once time has passed between the grant and the forward.
	// That is the real case: the clamped expiry is fixed at the moment the
	// request was sent, and obligation handling, a retry, or a loaded host all
	// consume it afterwards. The proxy's clock is moved on to model it, which
	// is what that seam is for.
	t.Run("a grant with no room left is refused before the egress", func(t *testing.T) {
		plane := newFakePlane(t, admissionapi.OutcomeAllowed, nil)
		upstream := newFakeUpstream(t)
		governed, logged := newTestProxy(t, plane, upstream)
		// Six seconds remaining when the plane answers, so reportable accepts
		// it; two seconds gone by the time the call would be made, leaving
		// less than the report window.
		plane.token.ExpiresAt = time.Now().Add(minimumReportWindow + time.Second)
		governed.clock = func() time.Time { return time.Now().Add(2 * time.Second) }

		recorder := post(t, governed, plainCall)
		if upstream.calls != 0 {
			t.Fatal("the call was forwarded under a grant that could not outlive it")
		}
		if recorder.Code != http.StatusServiceUnavailable {
			t.Fatalf("status = %d, want 503", recorder.Code)
		}
		// Reported, so the plane learns the grant was returned unused rather
		// than that the proxy went dark holding it.
		if len(plane.reports) != 1 ||
			plane.reports[0].ErrorCode != "grant_window_exhausted" {
			t.Fatalf("reports = %+v, want one naming the exhausted window",
				plane.reports)
		}
		if joined := strings.Join(*logged, "\n"); !strings.Contains(joined, "grant window") {
			t.Fatalf("nothing in the log explains the refusal: %q", joined)
		}
	})

	// The budget is applied to the call, not merely checked once. An upstream
	// slower than the grant's remaining life must be cut off while the report
	// is still possible, rather than running to the configured timeout.
	t.Run("the call is bounded by the grant, not the timeout", func(t *testing.T) {
		plane := newFakePlane(t, admissionapi.OutcomeAllowed, nil)
		// Released by cleanup as well as by cancellation. Blocking only on the
		// request context deadlocked Close, which waits for outstanding
		// handlers: the proxy's client gives up on the grant's budget, and the
		// server-side context did not always observe that in time.
		stop := make(chan struct{})
		slow := httptest.NewServer(http.HandlerFunc(
			func(writer http.ResponseWriter, request *http.Request) {
				select {
				case <-request.Context().Done():
				case <-stop:
				}
			}))
		t.Cleanup(func() {
			close(stop)
			slow.Close()
		})
		upstream := newFakeUpstream(t)
		governed, _ := newTestProxy(t, plane, upstream)
		base, err := url.Parse(slow.URL)
		if err != nil {
			t.Fatal(err)
		}
		governed.upstream = base
		governed.client.Transport = slow.Client().Transport
		// The client timeout in the harness is 5s. A grant leaving 5.4s means
		// the budget is 400ms, so a cut-off well under a second proves the
		// grant bounded the call rather than the configured timeout.
		plane.token.ExpiresAt = time.Now().Add(minimumReportWindow + 400*time.Millisecond)

		started := time.Now()
		recorder := post(t, governed, plainCall)
		elapsed := time.Since(started)

		if elapsed > 3*time.Second {
			t.Fatalf("the call ran for %s: the configured timeout bounded it, "+
				"not the grant", elapsed)
		}
		if recorder.Code != http.StatusBadGateway {
			t.Fatalf("status = %d, want 502", recorder.Code)
		}
		if len(plane.reports) != 1 || !plane.reports[0].Failed {
			t.Fatalf("reports = %+v, want one failure", plane.reports)
		}
	})

	// And a healthy grant is untouched, or this is a refusal rather than a
	// bound: the window must not shorten calls it has room for.
	t.Run("a grant with room is not interfered with", func(t *testing.T) {
		plane := newFakePlane(t, admissionapi.OutcomeAllowed, nil)
		upstream := newFakeUpstream(t)
		governed, _ := newTestProxy(t, plane, upstream)
		if recorder := post(t, governed, plainCall); recorder.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200: %s", recorder.Code, recorder.Body.String())
		}
		if upstream.calls != 1 {
			t.Fatalf("upstream calls = %d, want 1", upstream.calls)
		}
	})
}

// TestANonTwoHundredUpstreamStatusIsNeverReportedAsWork covers a consequence of
// refusing redirects.
//
// A 3xx used to pass the failure classification, which only looked at 4xx and
// above. Since redirects are deliberately not followed, a 3xx is a response
// with no completion in it — and reporting it as successful work would feed a
// result into the next admission for a call that produced nothing.
func TestANonTwoHundredUpstreamStatusIsNeverReportedAsWork(t *testing.T) {
	for _, probe := range []struct {
		status int
		isWork bool
	}{
		{http.StatusOK, true},
		{http.StatusCreated, true},
		{http.StatusNoContent, true},
		// The redirects the proxy refuses to follow.
		{http.StatusMovedPermanently, false},
		{http.StatusFound, false},
		{http.StatusTemporaryRedirect, false},
		{http.StatusPermanentRedirect, false},
		{http.StatusNotModified, false},
		{http.StatusTooManyRequests, false},
		{http.StatusInternalServerError, false},
	} {
		plane := newFakePlane(t, admissionapi.OutcomeAllowed, nil)
		upstream := newFakeUpstream(t)
		upstream.status = probe.status
		governed, _ := newTestProxy(t, plane, upstream)

		post(t, governed, plainCall)
		if len(plane.reports) != 1 {
			t.Fatalf("status %d: reports = %d, want 1", probe.status, len(plane.reports))
		}
		report := plane.reports[0]
		if probe.isWork {
			if report.Failed || report.ErrorCode != "" {
				t.Fatalf("status %d was reported as a failure: %+v",
					probe.status, report)
			}
			continue
		}
		if !report.Failed || report.ErrorCode != "upstream_error" {
			t.Fatalf("status %d was reported as successful work: %+v",
				probe.status, report)
		}
	}
}

// TestTheDeclaredEffectsFollowTheConfiguredProvider pins a contract the fleet
// states and this proxy violated.
//
// pkg/explorer/fleet/model.go: EffectEgressesContent "is not a property of the
// code. The same executor is egress-free against a loopback model provider and
// egress-bearing against a hosted one, so an executor declaring this must
// derive it from the provider it was actually configured with."
//
// It declared both classes unconditionally, defended by a comment arguing that
// declaring less than you do is the understatement #385's effect floor refuses.
// That is right about understatement and wrong here: the floor refuses too
// little, so too much is never refused — it is silently *denied*. A policy
// forbidding egress then denies every call on a deployment where nothing leaves
// the host, and the chart supports exactly that deployment.
func TestTheDeclaredEffectsFollowTheConfiguredProvider(t *testing.T) {
	for _, probe := range []struct {
		name   string
		host   string
		egress bool
	}{
		{"a hosted provider", "https://api.example.test/v1", true},
		{"an IPv4 loopback sidecar", "http://127.0.0.1:11434/v1", false},
		{"localhost", "http://localhost:11434/v1", false},
		{"an IPv6 loopback sidecar", "http://[::1]:11434/v1", false},
		{"elsewhere in 127.0.0.0/8", "http://127.5.5.5:11434/v1", false},
		// A DNS name that merely looks local is not loopback, the same
		// distinction the chart guard had to learn.
		{"a host that only starts with localhost", "https://localhost.example/v1", true},
	} {
		parsed, err := url.Parse(probe.host)
		if err != nil {
			t.Fatal(err)
		}
		effects := declaredEffects(parsed)
		hasEgress := false
		hasCorpus := false
		for _, effect := range effects {
			switch effect {
			case admissionapi.EffectEgressesContent:
				hasEgress = true
			case admissionapi.EffectReadsCorpus:
				hasCorpus = true
			}
		}
		if hasEgress != probe.egress {
			t.Fatalf("%s (%s): egress declared = %v, want %v",
				probe.name, probe.host, hasEgress, probe.egress)
		}
		// reads-corpus is unconditional: the proxy reads the references the
		// caller declared whatever the provider is. Dropping it would be the
		// understatement the effect floor actually exists to refuse.
		if !hasCorpus {
			t.Fatalf("%s: reads-corpus was not declared: %v", probe.name, effects)
		}
	}

	// And it reaches the wire, which testing declaredEffects alone does not
	// prove — the same gap a mutation found between absoluteURL and newProxy.
	plane := newFakePlane(t, admissionapi.OutcomeAllowed, nil)
	upstream := newFakeUpstream(t)
	governed, _ := newTestProxy(t, plane, upstream)
	// The harness upstream is a loopback httptest server, so this is the
	// sidecar case.
	post(t, governed, plainCall)
	if len(plane.requests) != 1 {
		t.Fatalf("admissions = %d", len(plane.requests))
	}
	for _, effect := range plane.requests[0].Effects {
		if effect == admissionapi.EffectEgressesContent {
			t.Fatalf("a loopback provider declared egress on the wire: %v",
				plane.requests[0].Effects)
		}
	}

	plane.requests = nil
	remote, err := url.Parse("https://api.example.test/v1")
	if err != nil {
		t.Fatal(err)
	}
	governed.admission.effects = declaredEffects(remote)
	post(t, governed, plainCall)
	found := false
	for _, effect := range plane.requests[0].Effects {
		if effect == admissionapi.EffectEgressesContent {
			found = true
		}
	}
	if !found {
		t.Fatalf("a hosted provider did not declare egress: %v",
			plane.requests[0].Effects)
	}
}

// TestRetryAfterReachesTheCaller covers a header the proxy swallowed.
//
// Only Content-Type and Cache-Control were relayed, so a provider's backoff
// instruction never arrived. This repository's own OpenAI client reads the
// header (pkg/model/openai.go:584), so a Shoal-built caller behind this proxy
// would retry a 429 immediately against a provider that asked it to wait — the
// proxy turning a well-behaved client into a badly-behaved one, which is the
// opposite of "an unmodified client is governed unchanged".
func TestRetryAfterReachesTheCaller(t *testing.T) {
	plane := newFakePlane(t, admissionapi.OutcomeAllowed, nil)
	upstream := newFakeUpstream(t)
	upstream.status = http.StatusTooManyRequests
	upstream.headers = map[string]string{"Retry-After": "42"}
	governed, _ := newTestProxy(t, plane, upstream)

	recorder := post(t, governed, plainCall)
	if recorder.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429", recorder.Code)
	}
	if got := recorder.Header().Get("Retry-After"); got != "42" {
		t.Fatalf("Retry-After = %q, want 42", got)
	}
	// Still reported as an upstream error, since no completion was produced.
	if len(plane.reports) != 1 || plane.reports[0].ErrorCode != "upstream_error" {
		t.Fatalf("reports = %+v", plane.reports)
	}
}

// TestReferenceBoundsAreEnforcedLocally closes the half of the reference
// contract that checking the encoding left open.
//
// A reference decoding to more than shoal.MaxIDBytes, or more references than
// admissionapi.MaxDisclosures, is refused by the plane with a 400 that the admission client
// turns into ErrPlaneUnreachable — so the caller is told 503, which means
// retry, for a request no retry can fix. Exactly the failure the base64url
// check was added for, in the dimension that check did not cover.
func TestReferenceBoundsAreEnforcedLocally(t *testing.T) {
	oversized := base64.RawURLEncoding.EncodeToString(
		bytes.Repeat([]byte("a"), shoal.MaxIDBytes+1))
	tooMany := make([]string, admissionapi.MaxDisclosures+1)
	for i := range tooMany {
		tooMany[i] = base64.RawURLEncoding.EncodeToString(
			[]byte(fmt.Sprintf("doc-%d", i)))
	}
	encodedMany, err := json.Marshal(tooMany)
	if err != nil {
		t.Fatal(err)
	}

	for _, probe := range []struct{ name, body string }{
		{"a reference over the ID size bound", `{"model":"gpt",` +
			`"messages":[{"role":"user","content":"hi"}],` +
			`"shoal_references":["` + oversized + `"]}`},
		{"more references than the plane accepts", `{"model":"gpt",` +
			`"messages":[{"role":"user","content":"hi"}],` +
			`"shoal_references":` + string(encodedMany) + `}`},
	} {
		plane := newFakePlane(t, admissionapi.OutcomeAllowed, nil)
		upstream := newFakeUpstream(t)
		governed, _ := newTestProxy(t, plane, upstream)

		recorder := post(t, governed, probe.body)
		if recorder.Code != http.StatusBadRequest {
			t.Fatalf("%s: status = %d, want 400 — a 503 tells the caller to "+
				"retry a request no retry can fix", probe.name, recorder.Code)
		}
		if len(plane.requests) != 0 {
			t.Fatalf("%s: it consumed a decision anyway", probe.name)
		}
	}

	// And the bounds themselves are still reachable, or this refuses the
	// feature rather than bounding it.
	atLimit := make([]string, admissionapi.MaxDisclosures)
	for i := range atLimit {
		atLimit[i] = base64.RawURLEncoding.EncodeToString(
			[]byte(fmt.Sprintf("doc-%d", i)))
	}
	encodedLimit, err := json.Marshal(atLimit)
	if err != nil {
		t.Fatal(err)
	}
	plane := newFakePlane(t, admissionapi.OutcomeAllowed, nil)
	upstream := newFakeUpstream(t)
	governed, _ := newTestProxy(t, plane, upstream)
	recorder := post(t, governed,
		`{"model":"gpt","messages":[{"role":"user","content":"hi"}],`+
			`"shoal_references":`+string(encodedLimit)+`}`)
	if recorder.Code != http.StatusOK {
		t.Fatalf("a request exactly at the bound was refused: %d %s",
			recorder.Code, recorder.Body.String())
	}
	atSize := base64.RawURLEncoding.EncodeToString(
		bytes.Repeat([]byte("a"), shoal.MaxIDBytes))
	recorder = post(t, governed,
		`{"model":"gpt","messages":[{"role":"user","content":"hi"}],`+
			`"shoal_references":["`+atSize+`"]}`)
	if recorder.Code != http.StatusOK {
		t.Fatalf("an ID exactly at the size bound was refused: %d", recorder.Code)
	}
}

// TestTheShoalExtensionDoesNotReachTheProvider covers a disclosure the proxy
// was adding on its own account.
//
// shoal_references is proxy metadata and was forwarded verbatim. The IDs name
// governed documents the call concerns, so sending them to a third-party
// provider tells that provider which of them this request is about — the proxy
// declaring egresses-content and then adding a little more egress of its own,
// as metadata, to the one party the egress policy is about. A strict
// OpenAI-compatible server also rejects an unknown top-level field, so the
// configuration most likely to be governed is the one most likely to break.
//
// The other half matters as much: genuinely unknown fields must survive. The
// request body is a map rather than a struct precisely so the proxy is not the
// reason a provider feature nobody here has heard of stops working.
func TestTheShoalExtensionDoesNotReachTheProvider(t *testing.T) {
	plane := newFakePlane(t, admissionapi.OutcomeAllowed, nil)
	upstream := newFakeUpstream(t)
	governed, _ := newTestProxy(t, plane, upstream)

	post(t, governed,
		`{"model":"gpt","messages":[{"role":"user","content":"hi"}],`+
			`"shoal_references":["`+docA+`"],`+
			`"logprobs":true,"a_future_provider_field":{"nested":"keep-me"}}`)

	forwarded := string(upstream.received)
	if strings.Contains(forwarded, shoalReferencesField) {
		t.Fatalf("the Shoal extension reached the provider: %s", forwarded)
	}
	if strings.Contains(forwarded, docA) {
		t.Fatalf("a governed document ID reached the provider: %s", forwarded)
	}
	// Everything else is untouched, including a field this build knows nothing
	// about and a nested value inside it.
	for _, kept := range []string{`"model":"gpt"`, "logprobs", "a_future_provider_field", "keep-me", "hi"} {
		if !strings.Contains(forwarded, kept) {
			t.Fatalf("%s was dropped along with the extension: %s", kept, forwarded)
		}
	}

	// And the plane still receives the reference count, because stripping the
	// field from the egress must not stop the proxy declaring it.
	if len(plane.requests) != 1 {
		t.Fatalf("admissions = %d", len(plane.requests))
	}
	if !strings.Contains(string(plane.requests[0].Input), `"reference_count":1`) {
		t.Fatalf("the declaration lost the reference count: %s",
			plane.requests[0].Input)
	}
	if len(plane.requests[0].Disclosures) != 1 ||
		plane.requests[0].Disclosures[0] != docA {
		t.Fatalf("the disclosure was not declared: %v", plane.requests[0].Disclosures)
	}

	// A request with no references is forwarded unchanged rather than rebuilt,
	// so the common path cannot reorder or lose anything.
	upstream.received = nil
	post(t, governed, plainCall)
	if strings.Contains(string(upstream.received), shoalReferencesField) {
		t.Fatalf("a field appeared from nowhere: %s", upstream.received)
	}
	if !strings.Contains(string(upstream.received), `"model":"gpt"`) {
		t.Fatalf("the plain request was damaged: %s", upstream.received)
	}
}
