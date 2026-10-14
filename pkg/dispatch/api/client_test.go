// Licensed under the Apache License, Version 2.0. See LICENSE and NOTICE.

package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

var testNow = time.Date(2026, 9, 6, 14, 0, 0, 0, time.FixedZone("CEST", 2*3600))

func testContext() Context {
	return Context{
		RequestID: []byte("request"), CorrelationID: []byte("trace-527"),
		ReasonCode: "worker_op", Deadline: testNow.Add(time.Minute),
	}
}

const testRecord = `{"id":"YQD_","version":2,"state":"claimed","agent_id":"YWdlbnQ",` +
	`"agent_generation":3,"capability":"c","action":"a","input":{"k":1},` +
	`"request_id":"cmVxdWVzdA","correlation_id":"dHJhY2UtNTI3",` +
	`"deadline":"2026-09-06T12:30:00Z","created_at":"2026-09-06T12:00:00Z",` +
	`"updated_at":"2026-09-06T12:00:01Z","executor_key":"AAcOFRwjKjE4P0ZNVFtiaXB3foWMk5qhqK-2vcTL0tk",` +
	`"claim_id":"Y2xhaW0","claim_fence":1,"claim_lease_until":"2026-09-06T12:05:00Z",` +
	`"effect_possible":true}`

type scripted struct {
	replies  []func(*http.Request) (*http.Response, error)
	requests []*http.Request
	bodies   [][]byte
}

func (s *scripted) RoundTrip(request *http.Request) (*http.Response, error) {
	var body []byte
	if request.Body != nil {
		body, _ = io.ReadAll(request.Body)
	}
	s.requests = append(s.requests, request)
	s.bodies = append(s.bodies, body)
	if len(s.replies) == 0 {
		return nil, errors.New("script exhausted")
	}
	next := s.replies[0]
	s.replies = s.replies[1:]
	return next(request)
}

func respond(status int, body string, header ...string) func(*http.Request) (*http.Response, error) {
	return func(*http.Request) (*http.Response, error) {
		h := http.Header{"Content-Type": {"application/json"}}
		for i := 0; i+1 < len(header); i += 2 {
			h.Set(header[i], header[i+1])
		}
		return &http.Response{StatusCode: status, Header: h,
			Body: io.NopCloser(strings.NewReader(body))}, nil
	}
}

func testClient(t *testing.T, replies ...func(*http.Request) (*http.Response, error)) (*Client, *scripted) {
	t.Helper()
	transport := &scripted{replies: replies}
	client, err := NewClient(Config{
		BaseURL: "https://explorer.invalid/shoal/", HTTPClient: &http.Client{Transport: transport},
		Token: func(context.Context) (string, error) { return "token", nil },
		Clock: func() time.Time { return testNow },
	})
	if err != nil {
		t.Fatal(err)
	}
	return client, transport
}

func claimCall() Claim {
	return Claim{Context: testContext(), ExpectedVersion: 1, ClaimID: []byte("claim"), Lease: time.Minute}
}

func TestNewClientRefusesUnusableConfiguration(t *testing.T) {
	token := func(context.Context) (string, error) { return "t", nil }
	for name, base := range map[string]string{
		"relative": "/shoal", "no host": "https://", "scheme": "ftp://explorer.test",
		"userinfo": "https://user:pw@explorer.test", "query": "https://explorer.test/?a=b",
		"empty query": "https://explorer.test/?", "fragment": "https://explorer.test/#f",
		"opaque": "https:explorer.test",
	} {
		if _, err := NewClient(Config{BaseURL: base, Token: token}); err == nil {
			t.Errorf("%s: %q accepted", name, base)
		}
	}
	if _, err := NewClient(Config{BaseURL: "https://explorer.test"}); err == nil {
		t.Error("a client without a token source was accepted")
	}
	if _, err := NewClient(Config{BaseURL: "https://explorer.test", Token: token,
		UserAgent: "a\r\nX-Injected: 1"}); err == nil {
		t.Error("a user agent with a line break was accepted")
	}
}

func TestTheClientJoinsRoutesOntoAPathPrefixAndSendsItsHeaders(t *testing.T) {
	transport := &scripted{replies: []func(*http.Request) (*http.Response, error){respond(200, testRecord)}}
	client, err := NewClient(Config{
		BaseURL: "https://explorer.invalid/a/b/", HTTPClient: &http.Client{Transport: transport},
		Token:     func(context.Context) (string, error) { return "tok", nil },
		UserAgent: "worker/1", Clock: func() time.Time { return testNow },
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.Claim(context.Background(), []byte{'a', 0, 255}, claimCall()); err != nil {
		t.Fatal(err)
	}
	request := transport.requests[0]
	if request.URL.String() != "https://explorer.invalid/a/b/api/v1/fleet/actions/YQD_/claim" {
		t.Fatalf("url = %s", request.URL)
	}
	for header, want := range map[string]string{
		"Authorization": "Bearer tok", "Content-Type": "application/json",
		"Accept": "application/json", "User-Agent": "worker/1",
		CorrelationIDHeader: "trace-527",
	} {
		if got := request.Header.Get(header); got != want {
			t.Errorf("%s = %q, want %q", header, got, want)
		}
	}
	want := `{"context":{"request_id":"cmVxdWVzdA","correlation_id":"dHJhY2UtNTI3",` +
		`"reason_code":"worker_op","deadline":"2026-09-06T12:01:00Z"},` +
		`"expected_version":1,"claim_id":"Y2xhaW0","lease":60000000000}`
	if string(transport.bodies[0]) != want {
		t.Fatalf("body = %s", transport.bodies[0])
	}
}

func TestTheClientFollowsNoRedirectAndUsesNoCookieJar(t *testing.T) {
	var followed atomic.Bool
	target := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		followed.Store(true)
	}))
	defer target.Close()
	var sawCookie atomic.Bool
	explorer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, err := r.Cookie("session"); err == nil {
			sawCookie.Store(true)
		}
		http.Redirect(w, r, target.URL, http.StatusTemporaryRedirect)
	}))
	defer explorer.Close()
	jar, _ := cookiejar.New(nil)
	base, _ := url.Parse(explorer.URL)
	jar.SetCookies(base, []*http.Cookie{{Name: "session", Value: "ambient"}})
	caller := &http.Client{Jar: jar} // follows redirects by default
	client, err := NewClient(Config{BaseURL: explorer.URL + "/", HTTPClient: caller,
		Token: func(context.Context) (string, error) { return "t", nil },
		Clock: func() time.Time { return testNow }})
	if err != nil {
		t.Fatal(err)
	}
	_, err = client.Claim(context.Background(), []byte("a"), claimCall())
	if Kind(err) != KindStatus {
		t.Fatalf("redirect = %v, want unexpected_status", err)
	}
	var dispatchErr *Error
	if !errors.As(err, &dispatchErr) || dispatchErr.Status != http.StatusTemporaryRedirect {
		t.Fatalf("redirect status = %#v", err)
	}
	if followed.Load() {
		t.Fatal("the redirect was followed")
	}
	if sawCookie.Load() {
		t.Fatal("the caller's cookie jar was used")
	}
	if caller.CheckRedirect != nil || caller.Jar == nil {
		t.Fatal("the caller's client was modified")
	}
}

func TestDecodeResponseRefusesAmbiguityAndIgnoresTheUnknown(t *testing.T) {
	for name, body := range map[string]string{
		"duplicate key":       `{"id":"YQ","id":"Yg"}`,
		"fold-equal keys":     `{"future":1,"FUTURE":2}`,
		"unicode fold":        `{"ſtate":"claimed","state":"claimed"}`,
		"case variant":        `{"ID":"YQ"}`,
		"nested duplicate":    `{"id":"YQ","effected":{"bytes":1,"bytes":2}}`,
		"nested case variant": `{"id":"YQ","ambiguity_reports":[{"Claim_Fence":1}]}`,
		"invalid utf8":        "{\"id\":\"YQ\",\"capability\":\"\xff\"}",
		"trailing value":      `{"id":"YQ"}{}`,
		"trailing garbage":    `{"id":"YQ"}x`,
		"not json":            `{`,
		"empty":               ``,
	} {
		var out Action
		if err := decodeResponse([]byte(body), &out); err == nil {
			t.Errorf("%s: accepted %s", name, body)
		}
	}
	for name, body := range map[string]string{
		"unknown field":             `{"id":"YQ","future":{"x":[1,{"y":2}]}}`,
		"unknown huge number":       `{"id":"YQ","future":1e999999}`,
		"fold-equal keys in input":  `{"id":"YQ","input":{"Name":1,"name":2}}`,
		"deep input":                `{"id":"YQ","input":` + strings.Repeat(`[`, 200) + strings.Repeat(`]`, 200) + `}`,
		"duplicate inside unknown":  `{"id":"YQ","future":{"a":1,"a":2}}`,
		"trailing whitespace":       `{"id":"YQ"}` + " \n\t",
		"null":                      `null`,
		"unknown inside known list": `{"id":"YQ","ambiguity_reports":[{"claim_fence":1,"later":true}]}`,
	} {
		var out Action
		if err := decodeResponse([]byte(body), &out); err != nil {
			t.Errorf("%s: refused %s: %v", name, body, err)
		}
	}
}

func TestErrorTextNeverCarriesTheCause(t *testing.T) {
	const secret = "s3cret-path"
	client, _ := testClient(t, func(*http.Request) (*http.Response, error) {
		return nil, &url.Error{Op: "Post", URL: "https://x/" + secret, Err: context.DeadlineExceeded}
	})
	_, err := client.Claim(context.Background(), []byte("a"), claimCall())
	if strings.Contains(err.Error(), secret) {
		t.Fatalf("error text carries the cause: %s", err)
	}
	if Kind(err) != KindRepull || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("claim transport failure = %v", err)
	}
	failing, _ := testClient(t)
	failing.token = func(context.Context) (string, error) { return "", errors.New(secret) }
	_, err = failing.Claim(context.Background(), []byte("a"), claimCall())
	if Kind(err) != KindNoCredential || strings.Contains(err.Error(), secret) {
		t.Fatalf("token failure = %v", err)
	}
}

func TestTheErrorCodeVocabularyIsAppliedBeforeSending(t *testing.T) {
	client, transport := testClient(t)
	client.errorCodes = func(code string) bool { return code == "allowed_code" }
	completion := Completion{Context: testContext(), ExpectedVersion: 1, ClaimID: []byte("claim"),
		ClaimFence: 1, Failed: true, ErrorCode: "other_code"}
	if _, err := client.Complete(context.Background(), []byte("a"), completion); Kind(err) != KindRefused {
		t.Fatalf("code outside the vocabulary = %v", err)
	}
	client.errorCodes = nil
	completion.ErrorCode = ""
	if _, err := client.Complete(context.Background(), []byte("a"), completion); Kind(err) != KindRefused {
		t.Fatalf("empty code with no vocabulary = %v", err)
	}
	if len(transport.requests) != 0 {
		t.Fatal("a refused completion was sent")
	}
}

func TestCorrelationIDsFollowTheExplorersRule(t *testing.T) {
	for _, good := range []string{"a", "trace-527", "ünïcode", strings.Repeat("c", MaxCorrelationIDBytes)} {
		if err := CheckCorrelationID([]byte(good)); err != nil {
			t.Errorf("%q refused: %v", good, err)
		}
	}
	for _, bad := range []string{"", "has space", "tab\t", "nl\n", "\xff", "bidi‮",
		strings.Repeat("c", MaxCorrelationIDBytes+1)} {
		if err := CheckCorrelationID([]byte(bad)); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

func TestParseExecutorKeyAcceptsBothSpellingsAndNothingElse(t *testing.T) {
	raw := bytes.Repeat([]byte{0xfb}, ExecutorKeyBytes)
	urlSpelling, err := ParseExecutorKey(EncodeID(raw))
	if err != nil {
		t.Fatal(err)
	}
	padded := "+/v7+/v7+/v7+/v7+/v7+/v7+/v7+/v7+/v7+/v7+/s="
	stdSpelling, err := ParseExecutorKey(padded)
	if err != nil {
		t.Fatal(err)
	}
	if !EqualExecutorKeys(urlSpelling, stdSpelling) || !bytes.Equal(urlSpelling, raw) {
		t.Fatal("one key decoded to two")
	}
	if EqualExecutorKeys(nil, nil) {
		t.Fatal("two absent keys compare equal")
	}
	if _, err := ParseExecutorKey(""); !errors.Is(err, ErrExecutorKeyMissing) {
		t.Fatalf("empty key = %v", err)
	}
	for _, bad := range []string{EncodeID(raw[:31]), EncodeID(raw) + "\n", "AAAA", padded[:43] + "==",
		strings.Replace(EncodeID(raw), "-", "+", 1)} {
		if _, err := ParseExecutorKey(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

func TestStatusAndResolve(t *testing.T) {
	client, transport := testClient(t,
		respond(200, testRecord),
		respond(200, `{"id":"YWdlbnQ","generation":7,"executor_ref":"gateway","subject":"cw",`+
			`"capabilities":[{"name":"c","actions":[{"name":"a","input_schema":{},"output_schema":{},`+
			`"effect":"external"},{"name":"b","input_schema":{},"output_schema":{},`+
			`"effects":["reads-corpus"],"effect":""}]}]}`),
		respond(200, `{"id":"YWdlbnQ","capabilities":[{"name":"c","actions":[`+
			`{"name":"a","effects":["reads-corpus"],"effect":"external"}]}]}`),
	)
	record, err := client.Status(context.Background(), []byte{'a', 0, 255}, testContext())
	if err != nil || record.State != DispatchClaimed || len(record.ExecutorKey) != ExecutorKeyBytes {
		t.Fatalf("status = %+v, %v", record, err)
	}
	if transport.requests[0].URL.Path != "/shoal/api/v1/fleet/actions/YQD_/status" {
		t.Fatalf("status path = %s", transport.requests[0].URL.Path)
	}
	descriptor, err := client.Resolve(context.Background(), []byte("agent"), Context{
		RequestID: []byte("r"), ReasonCode: "startup", Deadline: testNow.Add(time.Minute)})
	if err != nil {
		t.Fatal(err)
	}
	actions := descriptor.Capabilities[0].Actions
	if len(actions[0].Effects) != 1 || actions[0].Effects[0] != EffectMutatesExternal ||
		len(actions[1].Effects) != 1 || actions[1].Effects[0] != EffectReadsCorpus {
		t.Fatalf("effects = %+v", actions)
	}
	if transport.requests[1].Header.Get(CorrelationIDHeader) != "" {
		t.Fatal("resolve sent a correlation header")
	}
	if _, err := client.Resolve(context.Background(), []byte("agent"), testContext()); Kind(err) != KindProtocol {
		t.Fatalf("inconsistent effects = %v", err)
	}
}

func TestReportAmbiguityUnrecordedMatchesTheSentinel(t *testing.T) {
	client, _ := testClient(t, respond(400, `{"code":"invalid_argument"}`))
	_, err := client.ReportAmbiguity(context.Background(), []byte("a"), Ambiguity{
		Context: testContext(), ClaimFence: 1, Outcome: AmbiguityOutcomeUnknown})
	if !errors.Is(err, ErrAmbiguityUnrecorded) || Kind(err) != KindAmbiguityUnrecorded {
		t.Fatalf("refused report = %v", err)
	}
	if errors.Is(&Error{Kind: KindIndeterminate}, ErrAmbiguityUnrecorded) {
		t.Fatal("an indeterminate error matches the unrecorded sentinel")
	}
}

// TestTheTypesReencodeEveryPinnedServerBody decodes every 2xx and error body
// the handler fixtures pin into this package's types and re-encodes it: the
// bytes must come back identical, which is what makes these the wire types.
func TestTheTypesReencodeEveryPinnedServerBody(t *testing.T) {
	entries, err := os.ReadDir(filepath.Join("testdata", "wire", "server"))
	if err != nil {
		t.Fatal(err)
	}
	checked := 0
	for _, entry := range entries {
		raw, err := os.ReadFile(filepath.Join("testdata", "wire", "server", entry.Name()))
		if err != nil {
			t.Fatal(err)
		}
		var status int
		var path, body string
		for _, line := range strings.Split(string(raw), "\n") {
			switch {
			case strings.HasPrefix(line, "POST "), strings.HasPrefix(line, "GET "):
				_, path, _ = strings.Cut(line, " ")
			case strings.HasPrefix(line, "status: "):
				status, _ = strconv.Atoi(strings.TrimPrefix(line, "status: "))
			case strings.HasPrefix(line, "body: "):
				if body, err = strconv.Unquote(strings.TrimPrefix(line, "body: ")); err != nil {
					t.Fatalf("%s: %v", entry.Name(), err)
				}
			}
		}
		if body == "" || !strings.HasPrefix(body, "{") {
			continue
		}
		var value any
		switch {
		case status != http.StatusOK:
			value = &ErrorResponse{}
		case strings.HasSuffix(path, "/resolve"):
			continue // the descriptor type names only what a worker reads
		case strings.HasSuffix(path, "/pull"):
			value = &Page{}
		default:
			value = &Action{}
		}
		if err := decodeResponse([]byte(body), value); err != nil {
			t.Errorf("%s: %v", entry.Name(), err)
			continue
		}
		var out bytes.Buffer
		if err := json.NewEncoder(&out).Encode(value); err != nil {
			t.Fatal(err)
		}
		if out.String() != body {
			t.Errorf("%s re-encodes differently\n--- pinned\n%s--- api\n%s", entry.Name(), body, out.String())
		}
		checked++
	}
	if checked < 200 {
		t.Fatalf("only %d pinned bodies checked; the fixtures are not where this test looks", checked)
	}
}

// TestResolveRefusesAnActionCarryingAFieldItDoesNotKnow: new gates on an
// action arrive as new fields, so an action field this client cannot read is
// a gate it cannot honour, and the descriptor is refused rather than served
// without it. Unknown fields elsewhere in the descriptor are still ignored.
func TestResolveRefusesAnActionCarryingAFieldItDoesNotKnow(t *testing.T) {
	descriptor := func(action, top string) string {
		return `{"id":"YWdlbnQ","generation":1,"executor_ref":"gateway"` + top +
			`,"capabilities":[{"name":"c","later":true,"actions":[` + action + `]}]}`
	}
	const known = `{"name":"a","input_schema":{},"output_schema":{},"effects":["external"]`
	for name, row := range map[string]struct {
		body string
		ok   bool
	}{
		"known fields":            {descriptor(known+`}`, ""), true},
		"unknown top-level field": {descriptor(known+`}`, `,"subject":"cw"`), true},
		"unknown action field":    {descriptor(known+`,"requires_new_gate":true}`, ""), false},
		"unknown null field":      {descriptor(known+`,"requires_new_gate":null}`, ""), false},
	} {
		client, _ := testClient(t, respond(201, row.body))
		_, err := client.Resolve(context.Background(), []byte("agent"), testContext())
		var dispatchErr *Error
		switch {
		case row.ok && err != nil:
			t.Errorf("%s: %v", name, err)
		case !row.ok && (!errors.As(err, &dispatchErr) || dispatchErr.Kind != KindProtocol ||
			dispatchErr.Status != 201):
			t.Errorf("%s: %v, want a protocol error carrying the answer's status", name, err)
		}
	}
}

// TestAFieldNoDecisionReadsCannotFailTheDecode: the client decodes a record
// into the fields a worker's decisions read, so a later explorer that
// reshapes any other field — evidence, the evidence snapshot, request_id,
// error_code_origin, a report's reporter — cannot turn an answer into a
// protocol error. On /complete that would make every completion resend and
// end indeterminate. The envelope checks still hold on those fields.
func TestAFieldNoDecisionReadsCannotFailTheDecode(t *testing.T) {
	const succeeded = `{"id":"YQ","version":3,"state":"succeeded","agent_id":"","agent_generation":1,` +
		`"capability":"c","action":"a","output":{"ok":true},"request_id":"cmVx",` +
		`"deadline":"2026-09-06T12:30:00Z","created_at":"2026-09-06T12:00:00Z",` +
		`"updated_at":"2026-09-06T12:00:01Z","claim_id":"Y2xhaW0","claim_fence":1,` +
		`"effect_possible":true`
	completion := Completion{Context: testContext(), ExpectedVersion: 2, ClaimID: []byte("claim"),
		ClaimFence: 1, Output: []byte(`{"ok":true}`)}
	for name, extra := range map[string]string{
		"evidence of the wrong type":    `,"evidence":"x"`,
		"evidence of the wrong shape":   `,"evidence":[{"anchor_id":5,"kind":[],"citation":"c"}]`,
		"an empty snapshot time":        `,"evidence_snapshot_as_of":""`,
		"a snapshot ID of another type": `,"evidence_snapshot_id":{"v":1}`,
		"a request ID of another type":  `,"request_id":7`,
		"an origin of another type":     `,"error_code_origin":{"by":"service"}`,
		"a report's reporter reshaped": `,"ambiguity_reports":[{"claim_fence":1,"outcome":"outcome_unknown",` +
			`"subject":1,"actor":[],"reported_at":"yesterday"}]`,
	} {
		body := strings.Replace(succeeded, `,"request_id":"cmVx"`, "", 1) + extra + `}`
		if !strings.Contains(extra, "request_id") {
			body = succeeded + extra + `}`
		}
		client, transport := testClient(t, respond(200, body), respond(200, body), respond(200, body))
		record, err := client.Complete(context.Background(), []byte("a"), completion)
		if err != nil || record.State != DispatchSucceeded || len(transport.requests) != 1 {
			t.Errorf("%s: %v after %d requests, want the record from one", name, err, len(transport.requests))
		}
	}
	// A key the decisions do not read is still held to the envelope rules.
	client, _ := testClient(t, respond(200, succeeded+`,"evidence":[],"evidence":[]}`),
		respond(200, succeeded+`}`))
	if _, err := client.Complete(context.Background(), []byte("a"), completion); err != nil {
		t.Fatalf("the resend did not settle a duplicated key: %v", err)
	}
	if err := decodeResponse([]byte(succeeded+`,"evidence":[],"evidence":[]}`), &recordWire{}); err == nil {
		t.Fatal("a duplicated evidence key was accepted")
	}
	// The full wire type still decodes typed evidence for a caller that wants it.
	var full Action
	if err := decodeResponse([]byte(succeeded+`,"evidence":"x"}`), &full); err == nil {
		t.Fatal("the full Action type accepted evidence of the wrong type")
	}
}
