// Licensed under the Apache License, Version 2.0. See LICENSE and NOTICE.
package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

var testNow = time.Date(2026, 9, 6, 14, 0, 0, 0, time.FixedZone("", 2*3600))

func testToken(context.Context) (string, error) { return "plane-token", nil }

type fakePlane struct {
	mu       sync.Mutex
	status   int
	header   map[string]string
	body     func(route string, raw []byte) string
	requests []*http.Request
	bodies   [][]byte
	server   *httptest.Server
}

func newFakePlane(t *testing.T) *fakePlane {
	t.Helper()
	plane := &fakePlane{status: http.StatusOK}
	plane.body = func(route string, raw []byte) string {
		switch route {
		case "request":
			var sent Request
			_ = json.Unmarshal(raw, &sent)
			return fmt.Sprintf(`{"outcome":"allowed","token":{"action_id":"YQD_","token_id":%q,"version":2,"expires_at":%q},"withhold":[]}`,
				sent.TokenID, testNow.Add(time.Minute).Format(time.RFC3339Nano))
		case "report":
			return `{"action_id":"YQD_","version":3,"state":"succeeded","reported_at":"2026-09-06T12:00:01+02:00"}`
		default:
			return `{"admissions":[]}`
		}
	}
	plane.server = httptest.NewServer(http.HandlerFunc(
		func(w http.ResponseWriter, r *http.Request) {
			raw, _ := io.ReadAll(r.Body)
			plane.mu.Lock()
			plane.requests = append(plane.requests, r)
			plane.bodies = append(plane.bodies, raw)
			status, header, body := plane.status, plane.header, plane.body
			plane.mu.Unlock()
			for name, value := range header {
				w.Header().Set(name, value)
			}
			w.Header().Set("Content-Type", "application/json; charset=utf-8")
			w.WriteHeader(status)
			route := r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]
			_, _ = io.WriteString(w, body(route, raw))
		}))
	t.Cleanup(plane.server.Close)
	return plane
}

func (p *fakePlane) client(t *testing.T, suffix string) *Client {
	t.Helper()
	client, err := NewClient(Config{
		BaseURL: p.server.URL + suffix, HTTPClient: p.server.Client(),
		Token: testToken,
	})
	if err != nil {
		t.Fatal(err)
	}
	return client
}

func testRequest() Request {
	return Request{
		Context: RequestContext{
			RequestID: "cmVxdWVzdA", ReasonCode: "test",
			Deadline: testNow.Add(time.Minute),
		},
		ID: "YWRtaXNzaW9u", IdempotencyKey: "a2V5", TokenID: "dG9rZW4",
		AgentID: "YWdlbnQ", AgentGeneration: 1, Capability: "llm.gateway",
		Action: "complete", ObjectID: "b2JqZWN0",
		Effects: []string{EffectReadsCorpus},
		Input:   json.RawMessage(`{}`), Lease: time.Minute,
	}
}

func fixed() time.Time { return testNow }

func TestNewClientValidatesConfiguration(t *testing.T) {
	for _, base := range []string{
		"", "shoal.example", "/relative", "ftp://shoal.example",
		"https://user:pass@shoal.example", "https://shoal.example/?q=1",
		"https://shoal.example/?", "https://shoal.example/#frag", "https://",
		"http://:80",
	} {
		if _, err := NewClient(Config{BaseURL: base, Token: testToken}); err == nil {
			t.Errorf("base URL %q accepted", base)
		}
	}
	if _, err := NewClient(Config{BaseURL: "https://shoal.example"}); err == nil {
		t.Error("missing token source accepted")
	}
	for _, base := range []string{
		"https://shoal.example", "https://shoal.example/",
		"http://127.0.0.1:8080/prefix", "https://gw.example/shoal/",
	} {
		if _, err := NewClient(Config{BaseURL: base, Token: testToken}); err != nil {
			t.Errorf("base URL %q refused: %v", base, err)
		}
	}
}

// TestClientJoinsRoutesOntoAPathPrefix pins the property that distinguishes
// this client from the other public ones: a path-routed plane works.
func TestClientJoinsRoutesOntoAPathPrefix(t *testing.T) {
	for suffix, want := range map[string]string{
		"":                "/api/v1/admission/request",
		"/":               "/api/v1/admission/request",
		"/shoal":          "/shoal/api/v1/admission/request",
		"/shoal/":         "/shoal/api/v1/admission/request",
		"/a/b//":          "/a/b/api/v1/admission/request",
		"/with%20space/x": "/with%20space/x/api/v1/admission/request",
	} {
		plane := newFakePlane(t)
		if _, err := plane.client(t, suffix).Request(
			context.Background(), testRequest(), RequestOptions{Clock: fixed},
		); err != nil {
			t.Fatalf("%q: %v", suffix, err)
		}
		request := plane.requests[0]
		if request.RequestURI != want || request.Method != http.MethodPost {
			t.Errorf("%q: %s %s, want POST %s", suffix, request.Method, request.RequestURI, want)
		}
		if got := request.Header.Get("Authorization"); got != "Bearer plane-token" {
			t.Errorf("authorization = %q", got)
		}
		if got := request.Header.Get("Content-Type"); got != "application/json" {
			t.Errorf("content type = %q", got)
		}
		if _, ok := request.Header["Accept"]; ok {
			t.Error("client sent an Accept header the deployed wire never carried")
		}
	}
}

func TestRequestReturnsTheGrant(t *testing.T) {
	plane := newFakePlane(t)
	plane.body = func(_ string, raw []byte) string {
		return `{"outcome":"allowed_with_obligations","token":{"action_id":"YQD_","token_id":"dG9rZW4","version":2,"expires_at":"2026-09-06T14:01:00+02:00"},"withhold":["ZG9jLWE","not-declared"],"future":{"x":1}}`
	}
	grant, err := plane.client(t, "").Request(context.Background(), testRequest(),
		RequestOptions{MinReportWindow: 5 * time.Second, Clock: fixed})
	if err != nil {
		t.Fatal(err)
	}
	if grant.Outcome != OutcomeObligated || grant.Token == nil ||
		grant.Token.ActionID != "YQD_" || len(grant.Withhold) != 2 ||
		grant.Withhold[1] != "not-declared" {
		t.Fatalf("grant = %#v", grant)
	}
	var sent Request
	if err := json.Unmarshal(plane.bodies[0], &sent); err != nil ||
		sent.TokenID != "dG9rZW4" {
		t.Fatalf("sent %s (%v)", plane.bodies[0], err)
	}
}

func TestRequestDenialIsErrDeniedWithNoToken(t *testing.T) {
	plane := newFakePlane(t)
	plane.body = func(string, []byte) string {
		// A token on a denial is never acted on.
		return `{"outcome":"denied","token":{"action_id":"YQ","token_id":"dG9rZW4","version":1,"expires_at":"2026-09-06T14:01:00+02:00"},"withhold":[]}`
	}
	grant, err := plane.client(t, "").Request(
		context.Background(), testRequest(), RequestOptions{Clock: fixed})
	if !errors.Is(err, ErrDenied) || grant.Outcome != OutcomeDenied || grant.Token != nil {
		t.Fatalf("denial = %#v, %v", grant, err)
	}
	var protocol *ProtocolError
	if errors.As(err, &protocol) {
		t.Fatal("a denial is an answer, not a protocol failure")
	}
}

func TestRequestRefusesGrantsItCannotActOn(t *testing.T) {
	token := func(fields string) string {
		return `{"outcome":"allowed","token":{` + fields + `},"withhold":[]}`
	}
	const (
		ids     = `"action_id":"YQD_","token_id":"dG9rZW4",`
		expires = `"expires_at":"2026-09-06T14:01:00+02:00"`
	)
	for name, body := range map[string]string{
		"unknown outcome":    `{"outcome":"allowed_maybe","token":{` + ids + `"version":2,` + expires + `},"withhold":[]}`,
		"empty outcome":      `{"token":{` + ids + `"version":2,` + expires + `},"withhold":[]}`,
		"missing token":      `{"outcome":"allowed","withhold":[]}`,
		"null token":         `{"outcome":"allowed","token":null,"withhold":[]}`,
		"empty token":        `{"outcome":"allowed","token":{},"withhold":[]}`,
		"missing expiry":     token(ids + `"version":2`),
		"zero version":       token(ids + `"version":0,` + expires),
		"padded action id":   token(`"action_id":"YQ==","token_id":"dG9rZW4","version":2,` + expires),
		"overlong action id": token(`"action_id":"` + strings.Repeat("QUFB", 85) + `QUE","token_id":"dG9rZW4","version":2,` + expires),
		"echo mismatch":      token(`"action_id":"YQD_","token_id":"b3RoZXI","version":2,` + expires),
		"short window":       token(ids + `"version":2,"expires_at":"2026-09-06T14:00:04+02:00"`),
		"already expired":    token(ids + `"version":2,"expires_at":"2026-09-06T11:59:00Z"`),
		"duplicate outcome":  `{"outcome":"denied","outcome":"allowed","token":{` + ids + `"version":2,` + expires + `},"withhold":[]}`,
		"case variant key":   `{"OUTCOME":"allowed","token":{` + ids + `"version":2,` + expires + `},"withhold":[]}`,
		"fold variant key":   `{"outcome":"allowed","token":{` + ids + `"ver` + "ſ" + `ion":2,` + expires + `},"withhold":[]}`,
		"fold duplicate":     `{"outcome":"allowed","Outcome":"denied","token":{` + ids + `"version":2,` + expires + `},"withhold":[]}`,
		"trailing data":      token(ids+`"version":2,`+expires) + ` {}`,
		"trailing garbage":   token(ids+`"version":2,`+expires) + `x`,
		"not json":           `<html>`,
		"empty body":         ``,
		"wrong type":         `{"outcome":1}`,
	} {
		t.Run(name, func(t *testing.T) {
			plane := newFakePlane(t)
			plane.body = func(string, []byte) string { return body }
			grant, err := plane.client(t, "").Request(context.Background(), testRequest(),
				RequestOptions{MinReportWindow: 5 * time.Second, Clock: fixed})
			var protocol *ProtocolError
			if !errors.As(err, &protocol) || !protocol.Committed ||
				protocol.Route != "request" || errors.Is(err, ErrDenied) {
				t.Fatalf("err = %v", err)
			}
			if grant.Token != nil || grant.Outcome != "" {
				t.Fatalf("refused grant leaked %#v", grant)
			}
		})
	}
}

// TestRequestMapsStatusesToHTTPError pins that every non-200 answer — the
// plane refusing this caller's credential, a redirect, a conflict, an outage —
// is an *HTTPError and never a denial.
func TestRequestMapsStatusesToHTTPError(t *testing.T) {
	for _, probe := range []struct {
		status        int
		header        map[string]string
		body          string
		code          string
		indeterminate bool
	}{
		{http.StatusUnauthorized, nil, `{"code":"unauthorized","message":"no"}`, "unauthorized", false},
		{http.StatusForbidden, nil, `forbidden`, "", false},
		{http.StatusConflict, nil, `{"code":"conflict","message":"admission token is not live"}`, "conflict", false},
		{http.StatusServiceUnavailable,
			map[string]string{CommitOutcomeHeader: CommitOutcomeIndeterminate},
			`{"code":"unavailable","message":"fleet action outcome requires reconciliation","indeterminate":true}`,
			"unavailable", true},
		// Either signal alone is enough.
		{http.StatusServiceUnavailable,
			map[string]string{CommitOutcomeHeader: CommitOutcomeIndeterminate}, ``, "", true},
		{http.StatusServiceUnavailable, nil,
			`{"code":"unavailable","message":"x","indeterminate":true}`, "unavailable", true},
		{http.StatusServiceUnavailable, nil, `{"code":"unavailable","message":"x"}`, "unavailable", false},
		{http.StatusCreated, nil, `{"outcome":"allowed"}`, "", false},
		{http.StatusFound, map[string]string{"Location": "/elsewhere"}, ``, "", false},
	} {
		plane := newFakePlane(t)
		plane.status, plane.header = probe.status, probe.header
		plane.body = func(string, []byte) string { return probe.body }
		_, err := plane.client(t, "").Request(
			context.Background(), testRequest(), RequestOptions{Clock: fixed})
		var failure *HTTPError
		if !errors.As(err, &failure) || failure.Status != probe.status ||
			failure.Code != probe.code || failure.Indeterminate != probe.indeterminate {
			t.Errorf("%d: err = %#v", probe.status, err)
		}
		if errors.Is(err, ErrDenied) {
			t.Errorf("%d read as a denial", probe.status)
		}
		if len(plane.requests) != 1 {
			t.Errorf("%d: %d requests, a redirect was followed", probe.status, len(plane.requests))
		}
	}
}

func TestRedirectIsNeverFollowed(t *testing.T) {
	target := newFakePlane(t)
	plane := newFakePlane(t)
	plane.status = http.StatusTemporaryRedirect
	plane.header = map[string]string{"Location": target.server.URL + "/api/v1/admission/request"}
	jar := &recordingJar{}
	client, err := NewClient(Config{
		BaseURL: plane.server.URL, Token: testToken,
		HTTPClient: &http.Client{
			Jar: jar,
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return nil // The caller's permissive policy is replaced.
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = client.Request(context.Background(), testRequest(), RequestOptions{Clock: fixed})
	var failure *HTTPError
	if !errors.As(err, &failure) || failure.Status != http.StatusTemporaryRedirect {
		t.Fatalf("err = %v", err)
	}
	if len(target.requests) != 0 {
		t.Fatal("the redirect carried the bearer token to its target")
	}
	if jar.used {
		t.Fatal("the caller's cookie jar was consulted")
	}
}

type recordingJar struct{ used bool }

func (j *recordingJar) SetCookies(*url.URL, []*http.Cookie) { j.used = true }
func (j *recordingJar) Cookies(*url.URL) []*http.Cookie     { j.used = true; return nil }

func TestTokenSourceFailuresSendNothing(t *testing.T) {
	plane := newFakePlane(t)
	for _, source := range []func(context.Context) (string, error){
		func(context.Context) (string, error) { return "", errors.New("unreadable") },
		func(context.Context) (string, error) { return "", nil },
		func(context.Context) (string, error) { return "a\r\nX-Injected: 1", nil },
	} {
		client, err := NewClient(Config{BaseURL: plane.server.URL, Token: source})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := client.Request(context.Background(), testRequest(), RequestOptions{}); err == nil {
			t.Fatal("request sent without a usable token")
		}
	}
	if len(plane.requests) != 0 {
		t.Fatalf("%d requests sent", len(plane.requests))
	}
}

func TestReportReturnsTheReceipt(t *testing.T) {
	plane := newFakePlane(t)
	receipt, err := plane.client(t, "/p").Report(context.Background(), Report{
		Context: testRequest().Context,
		Token: Token{ActionID: "YQD_", TokenID: "dG9rZW4", Version: 2,
			ExpiresAt: testNow.Add(time.Minute)},
		Outcome: json.RawMessage(`{"tokens":1}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	if receipt.ActionID != "YQD_" || receipt.Version != 3 ||
		receipt.State != DispatchSucceeded {
		t.Fatalf("receipt = %#v", receipt)
	}
	if plane.requests[0].RequestURI != "/p/api/v1/admission/report" {
		t.Fatalf("path = %s", plane.requests[0].RequestURI)
	}
}

// TestReportUndecodableSuccessIsCommitted pins that a 200 the client cannot
// read is never presented as "nothing happened": the plane may have recorded
// the report, and re-reporting an effect that already happened is the failure
// the indeterminate signal exists to prevent.
func TestReportUndecodableSuccessIsCommitted(t *testing.T) {
	for name, body := range map[string]string{
		"empty":         ``,
		"not json":      `ok`,
		"other action":  `{"action_id":"Yg","version":3,"state":"succeeded","reported_at":"2026-09-06T12:00:01Z"}`,
		"zero version":  `{"action_id":"YQD_","version":0,"state":"succeeded","reported_at":"2026-09-06T12:00:01Z"}`,
		"duplicate key": `{"action_id":"YQD_","action_id":"YQD_","version":3}`,
	} {
		plane := newFakePlane(t)
		plane.body = func(string, []byte) string { return body }
		_, err := plane.client(t, "").Report(context.Background(), Report{
			Token: Token{ActionID: "YQD_", TokenID: "dG9rZW4", Version: 2},
		})
		var protocol *ProtocolError
		if !errors.As(err, &protocol) || !protocol.Committed {
			t.Errorf("%s: err = %v", name, err)
		}
	}
}

func TestOutstandingPages(t *testing.T) {
	plane := newFakePlane(t)
	plane.body = func(string, []byte) string {
		return `{"admissions":[{"action_id":"YQD_","token_id":"dG9rZW4","version":2,"admitted_at":"2026-09-06T12:00:00+02:00","expires_at":"2026-09-06T12:01:00+02:00","expired":true}],"next":"bgD_"}`
	}
	page, err := plane.client(t, "").Outstanding(context.Background(),
		OutstandingRequest{Context: testRequest().Context, Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Admissions) != 1 || !page.Admissions[0].Expired || page.Next != "bgD_" {
		t.Fatalf("page = %#v", page)
	}
	plane.body = func(string, []byte) string { return `{"next":"x"}` }
	_, err = plane.client(t, "").Outstanding(context.Background(), OutstandingRequest{})
	var protocol *ProtocolError
	if !errors.As(err, &protocol) || protocol.Committed {
		t.Fatalf("missing list = %v", err)
	}
}

func TestTokenValidate(t *testing.T) {
	valid := Token{ActionID: "YQ", TokenID: "dA", Version: 1, ExpiresAt: testNow}
	if err := valid.Validate(); err != nil {
		t.Fatal(err)
	}
	max := strings.Repeat("QUFB", 85) + "QQ" // 256 bytes
	if err := (Token{ActionID: max, TokenID: max, Version: 1, ExpiresAt: testNow}).Validate(); err != nil {
		t.Fatalf("256-byte IDs refused: %v", err)
	}
	for name, token := range map[string]Token{
		"empty action": {TokenID: "dA", Version: 1, ExpiresAt: testNow},
		"empty token":  {ActionID: "YQ", Version: 1, ExpiresAt: testNow},
		"padded":       {ActionID: "YQ==", TokenID: "dA", Version: 1, ExpiresAt: testNow},
		"std alphabet": {ActionID: "+/8", TokenID: "dA", Version: 1, ExpiresAt: testNow},
		"overlong":     {ActionID: max + "QQ", TokenID: "dA", Version: 1, ExpiresAt: testNow},
		"no version":   {ActionID: "YQ", TokenID: "dA", ExpiresAt: testNow},
		"no expiry":    {ActionID: "YQ", TokenID: "dA", Version: 1},
	} {
		if err := token.Validate(); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
}

// TestDecodeResponseIgnoresUnknownFields pins the deliberate asymmetry: a newer
// plane may add response fields, and a deployed client must keep working.
func TestDecodeResponseIgnoresUnknownFields(t *testing.T) {
	var receipt Receipt
	if err := decodeResponse([]byte(
		`{"action_id":"YQ","added":{"nested":[1,{"x":null}]},"version":1,"Unrelated":true,"huge":1e400}`,
	), &receipt); err != nil || receipt.ActionID != "YQ" || receipt.Version != 1 {
		t.Fatalf("receipt = %#v, %v", receipt, err)
	}
	var page OutstandingPage
	if err := decodeResponse([]byte(
		`{"admissions":[{"action_id":"YQ","Action_ID":"Yg"}]}`), &page); err == nil {
		t.Fatal("case variant of a known nested field accepted")
	}
	deep := strings.Repeat("[", maxResponseDepth+2) + strings.Repeat("]", maxResponseDepth+2)
	if err := decodeResponse([]byte(`{"x":`+deep+`}`), &receipt); err == nil {
		t.Fatal("unbounded nesting accepted")
	}
	if err := decodeResponse([]byte("{\"action_id\":\"\xff\"}"), &receipt); err == nil {
		t.Fatal("invalid UTF-8 accepted")
	}
}

// TestContractsRoundTripTheServerGoldens proves the types here are the wire:
// every 200 body the handler was pinned to emit decodes into the matching type
// and re-encodes to the same bytes, and so does every body the gateway was
// pinned to send.
func TestContractsRoundTripTheServerGoldens(t *testing.T) {
	files, err := filepath.Glob(filepath.Join("testdata", "wire", "*", "*.golden"))
	if err != nil || len(files) < 10 {
		t.Fatalf("golden fixtures = %d (%v)", len(files), err)
	}
	checked := 0
	for _, file := range files {
		raw, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		fields := map[string]string{}
		var route string
		for _, line := range strings.Split(string(raw), "\n") {
			if strings.HasPrefix(line, "POST ") {
				route = strings.TrimPrefix(line, "POST ")
			}
			if key, value, ok := strings.Cut(line, ": "); ok {
				fields[key] = value
			}
		}
		roundTrip := func(quoted string, value any, newline bool) {
			t.Helper()
			body, err := strconv.Unquote(quoted)
			if err != nil {
				t.Fatalf("%s: %v", file, err)
			}
			if err := decodeResponse([]byte(body), value); err != nil {
				t.Fatalf("%s: %v", file, err)
			}
			encoded, err := json.Marshal(value)
			if err != nil {
				t.Fatal(err)
			}
			if newline {
				encoded = append(encoded, '\n')
			}
			if string(encoded) != body {
				t.Fatalf("%s: round trip\n got %s\nwant %s", file, encoded, body)
			}
			checked++
		}
		switch filepath.Base(filepath.Dir(file)) {
		case "server":
			if fields["status"] == "200" {
				switch route {
				case RequestRoute:
					roundTrip(fields["body"], &Grant{}, true)
				case ReportRoute:
					roundTrip(fields["body"], &Receipt{}, true)
				case OutstandingRoute:
					roundTrip(fields["body"], &OutstandingPage{}, true)
				}
			} else if body, _ := strconv.Unquote(fields["body"]); body != "" {
				roundTrip(fields["body"], &ErrorResponse{}, true)
			}
		case "gateway":
			if strings.HasSuffix(route, "/request") {
				roundTrip(fields["body"], &Request{}, false)
			} else {
				roundTrip(fields["body"], &Report{}, false)
			}
		}
	}
	if checked < 50 {
		t.Fatalf("only %d bodies round-tripped", checked)
	}
}
