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

package effectsgateway

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"
)

// The dispatch golden fixtures pin two things about this client, captured
// before it moved onto pkg/dispatch/api (#446 slice 4):
//
//   - gateway/: exactly what it puts on the wire — method, path under a
//     path-prefixed base URL, every header, and the body bytes — for every
//     route it speaks;
//   - client/: what it makes of a scripted sequence of answers — how many
//     requests it sends, to which path, and the result or the error it
//     returns, including the error's text, kind, status, code, failure class
//     and the kinds of its cause chain. These are the error identities the
//     worker and the gateway command key on.
//
// Regenerate deliberately with
//
//	go test ./internal/effectsgateway -run TestDispatchWireGolden -update
//
// The client/ cases deliberately leave out the response shapes the move
// changes on purpose (duplicate keys, case-variant keys and invalid UTF-8 in
// a response, unknown fields inside a resolved descriptor's action); those
// are covered by pkg/dispatch/api's own tests.
var updateDispatchGolden = flag.Bool("update", false, "rewrite the dispatch wire golden fixtures")

const dispatchGoldenRoot = "../../pkg/dispatch/api/testdata/wire"

// goldenNow is a fixed, non-UTC clock: the client derives nothing from it but
// the deadline check, and the wire carries deadlines converted to UTC.
var goldenNow = time.Date(2026, 9, 6, 14, 0, 0, 123456789, time.FixedZone("CEST", 2*3600))

type goldenCall struct {
	method  string
	uri     string
	headers string
	body    []byte
}

func (c goldenCall) String() string {
	return fmt.Sprintf("%s %s\n%sbody: %q\n", c.method, c.uri, c.headers, c.body)
}

func goldenHeaders(header http.Header) string {
	names := make([]string, 0, len(header))
	for name := range header {
		names = append(names, name)
	}
	sort.Strings(names)
	var out strings.Builder
	for _, name := range names {
		for _, value := range header[name] {
			fmt.Fprintf(&out, "header %s: %s\n", name, value)
		}
	}
	return out.String()
}

// capturingExplorer answers every route with a record that satisfies the
// client, and keeps every byte it was sent.
type capturingExplorer struct {
	mu     sync.Mutex
	calls  []goldenCall
	server *httptest.Server
}

const goldenActionID = "YQD_" // {'a', 0, 255}

func goldenRecordJSON(state string, version, fence uint64, extra string) string {
	return `{"id":"YQD_","version":` + fmt.Sprint(version) + `,"state":"` + state +
		`","agent_id":"YWdlbnQ","agent_generation":3,"capability":"effects.gateway",` +
		`"action":"charge","request_id":"cmVxdWVzdA","correlation_id":"dHJhY2UtNTI3",` +
		`"deadline":"2026-09-06T12:30:00Z","created_at":"2026-09-06T12:00:00Z",` +
		`"updated_at":"2026-09-06T12:00:01Z","claim_id":"Z3cxfHBvZHxjbGFpbQ",` +
		`"claim_fence":` + fmt.Sprint(fence) + `,"claim_lease_until":"2026-09-06T12:05:00Z",` +
		`"effect_possible":true` + extra + `}`
}

const goldenExecutorKey = "AAcOFRwjKjE4P0ZNVFtiaXB3foWMk5qhqK-2vcTL0tk"

func newCapturingExplorer(t *testing.T) *capturingExplorer {
	t.Helper()
	explorer := &capturingExplorer{}
	explorer.server = httptest.NewServer(http.HandlerFunc(
		func(writer http.ResponseWriter, request *http.Request) {
			raw, _ := io.ReadAll(request.Body)
			header := request.Header.Clone()
			header.Del("Host")
			explorer.mu.Lock()
			explorer.calls = append(explorer.calls, goldenCall{
				method: request.Method, uri: request.RequestURI,
				headers: goldenHeaders(header), body: raw,
			})
			explorer.mu.Unlock()
			writer.Header().Set("Content-Type", "application/json; charset=utf-8")
			path := request.URL.Path
			switch {
			case strings.HasSuffix(path, "/actions/pull"):
				fmt.Fprint(writer, `{"actions":[`+goldenRecordJSON("queued", 1, 0, "")+`]}`)
			case strings.HasSuffix(path, "/claim"):
				fmt.Fprint(writer, goldenRecordJSON("claimed", 2, 1,
					`,"input":{"account":"a1"},"executor_key":"`+goldenExecutorKey+`"`))
			case strings.HasSuffix(path, "/extend"):
				fmt.Fprint(writer, goldenRecordJSON("claimed", 3, 1, ""))
			case strings.HasSuffix(path, "/complete"):
				if bytes.Contains(raw, []byte(`"failed":true`)) {
					extra := `,"error_code":"outcome_unknown"`
					if bytes.Contains(raw, []byte(`"effected"`)) {
						extra += `,"effected":{"bytes":2048,"chunks":3}`
					}
					fmt.Fprint(writer, goldenRecordJSON("failed", 4, 1, extra))
					return
				}
				fmt.Fprint(writer, goldenRecordJSON("succeeded", 4, 1, `,"output":{"reference":"ch_1"}`))
			case strings.HasSuffix(path, "/ambiguity"):
				fmt.Fprint(writer, goldenRecordJSON("claimed", 4, 2,
					`,"ambiguity_reports":[{"claim_fence":1,"outcome":"outcome_unknown","target":"api.example","reference":"ch_1","subject":"cw","actor":"YQ","reported_at":"2026-09-06T12:00:00Z"},`+
						`{"claim_fence":1,"outcome":"request_not_sent","subject":"cw","actor":"YQ","reported_at":"2026-09-06T12:00:00Z"}]`))
			case strings.HasSuffix(path, "/resolve"):
				fmt.Fprint(writer, `{"id":"YWdlbnQ","generation":7,"executor_ref":"gateway","capabilities":[]}`)
			default:
				http.NotFound(writer, request)
			}
		}))
	t.Cleanup(explorer.server.Close)
	return explorer
}

func (e *capturingExplorer) take(t *testing.T) goldenCall {
	t.Helper()
	e.mu.Lock()
	defer e.mu.Unlock()
	if len(e.calls) != 1 {
		t.Fatalf("explorer calls = %d, want 1", len(e.calls))
	}
	call := e.calls[0]
	e.calls = nil
	return call
}

func goldenRequestContext(reason string) RequestContext {
	return RequestContext{
		RequestID: []byte("request"), CorrelationID: []byte("trace-527"),
		ReasonCode: reason, ReasonDetail: "detail",
		Deadline: goldenNow.Add(90 * time.Second),
	}
}

var goldenPollCorrelation = regexp.MustCompile(`gw-poll-[0-9a-f]{32}`)

// TestDispatchWireGolden pins the bytes this client sends and what it makes of
// the answers. See updateDispatchGolden.
func TestDispatchWireGolden(t *testing.T) {
	t.Run("gateway", testDispatchGatewayGolden)
	t.Run("client", testDispatchClientGolden)
}

func testDispatchGatewayGolden(t *testing.T) {
	explorer := newCapturingExplorer(t)
	base, err := url.Parse(explorer.server.URL + "/shoal/")
	if err != nil {
		t.Fatal(err)
	}
	unbound, err := NewDispatchClient(base, NewExplorerClient(5*time.Second),
		func() (string, error) { return "dispatch-token", nil },
		func() time.Time { return goldenNow })
	if err != nil {
		t.Fatal(err)
	}
	client, err := unbound.BindExecutor("gateway")
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	actionID := []byte{'a', 0, 255}
	claimID := []byte("gw1|pod|claim")
	check := func(name string, call goldenCall) {
		t.Helper()
		checkDispatchGolden(t, filepath.Join(dispatchGoldenRoot, "gateway", name+".golden"), call.String())
	}

	if _, err := client.Pull(ctx, goldenRequestContext("gateway_poll"), "Y3Vyc29y", 10); err != nil {
		t.Fatal(err)
	}
	check("pull_with_cursor", explorer.take(t))

	// A poll with no correlation mints its own, fresh per poll. The nonce is
	// random, so it is checked for shape and for header/body agreement, then
	// folded onto a placeholder.
	poll := goldenRequestContext("gateway_poll")
	poll.CorrelationID = nil
	if _, err := client.Pull(ctx, poll, "", 256); err != nil {
		t.Fatal(err)
	}
	minted := explorer.take(t)
	header := goldenPollCorrelation.FindString(minted.headers)
	if header == "" || !bytes.Contains(minted.body,
		[]byte(`"correlation_id":"`+base64.RawURLEncoding.EncodeToString([]byte(header))+`"`)) {
		t.Fatalf("minted poll correlation is not one value in header and body:\n%s", minted)
	}
	minted.headers = strings.ReplaceAll(minted.headers, header, "gw-poll-<nonce>")
	minted.body = bytes.ReplaceAll(minted.body,
		[]byte(base64.RawURLEncoding.EncodeToString([]byte(header))), []byte("<base64url(gw-poll-<nonce>)>"))
	check("pull_minted_correlation", minted)

	claim := ClaimRequest{
		Context: goldenRequestContext("gateway_claim"), ExpectedVersion: 1,
		ClaimID: claimID, Lease: 90 * time.Second,
	}
	if _, err := client.Claim(ctx, actionID, claim); err != nil {
		t.Fatal(err)
	}
	check("claim", explorer.take(t))

	if _, err := client.Extend(ctx, actionID, ExtendRequest{
		Context: goldenRequestContext("gateway_extend"), ClaimID: claimID,
		ClaimFence: 1, Lease: 2 * time.Minute,
	}); err != nil {
		t.Fatal(err)
	}
	check("extend", explorer.take(t))

	if _, err := client.Complete(ctx, actionID, Completion{
		Context: goldenRequestContext("gateway_complete"), ExpectedVersion: 3,
		ClaimID: claimID, ClaimFence: 1,
		Output: []byte(`{"reference":"ch_1"}`),
	}); err != nil {
		t.Fatal(err)
	}
	check("complete_succeeded", explorer.take(t))

	if _, err := client.Complete(ctx, actionID, Completion{
		Context: goldenRequestContext("gateway_complete"), ExpectedVersion: 3,
		ClaimID: claimID, ClaimFence: 1, Failed: true, ErrorCode: ErrorOutcomeUnknown,
	}); err != nil {
		t.Fatal(err)
	}
	check("complete_failed", explorer.take(t))

	effected := Completion{
		Context: goldenRequestContext("gateway_complete"), ExpectedVersion: 3,
		ClaimID: claimID, ClaimFence: 1, Failed: true, ErrorCode: ErrorOutcomeUnknown,
	}
	effected.Effected.Bytes, effected.Effected.Chunks = 2048, 3
	if _, err := client.Complete(ctx, actionID, effected); err != nil {
		t.Fatal(err)
	}
	check("complete_failed_effected", explorer.take(t))

	if _, err := client.ReportAmbiguity(ctx, actionID, AmbiguityReport{
		Context: goldenRequestContext("gateway_ambiguity"), ClaimFence: 1,
		Outcome: "outcome_unknown", Target: "api.example", Reference: "ch_1",
	}); err != nil {
		t.Fatal(err)
	}
	check("ambiguity", explorer.take(t))

	if _, err := client.ReportAmbiguity(ctx, actionID, AmbiguityReport{
		Context: goldenRequestContext("gateway_ambiguity"), ClaimFence: 1,
		Outcome: "request_not_sent",
	}); err != nil {
		t.Fatal(err)
	}
	check("ambiguity_minimal", explorer.take(t))

	resolve := goldenRequestContext("gateway_startup")
	resolve.CorrelationID = nil
	if _, err := client.Resolve(ctx, []byte("agent"), resolve); err != nil {
		t.Fatal(err)
	}
	check("resolve", explorer.take(t))
}

// goldenTransport answers each request with the next scripted reply and
// records the method and path.
type goldenTransport struct {
	replies  []func() (*http.Response, error)
	requests []string
}

func (s *goldenTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	if request.Body != nil {
		_, _ = io.ReadAll(request.Body)
	}
	s.requests = append(s.requests, request.Method+" "+request.URL.Path)
	if len(s.replies) == 0 {
		return nil, errors.New("script exhausted")
	}
	next := s.replies[0]
	s.replies = s.replies[1:]
	return next()
}

func answer(status int, body string, header ...string) func() (*http.Response, error) {
	return func() (*http.Response, error) {
		h := http.Header{"Content-Type": {"application/json"}}
		for i := 0; i+1 < len(header); i += 2 {
			h.Set(header[i], header[i+1])
		}
		return &http.Response{StatusCode: status, Header: h,
			Body: io.NopCloser(strings.NewReader(body))}, nil
	}
}

func answerLostBody(status int) func() (*http.Response, error) {
	return func() (*http.Response, error) {
		return &http.Response{StatusCode: status, Header: http.Header{},
			Body: io.NopCloser(io.MultiReader(strings.NewReader(`{"id":`),
				errReader{io.ErrUnexpectedEOF}))}, nil
	}
}

type errReader struct{ err error }

func (r errReader) Read([]byte) (int, error) { return 0, r.err }

func dropped() (*http.Response, error) { return nil, io.ErrUnexpectedEOF }

func timedOut() (*http.Response, error) {
	return nil, &url.Error{Op: "Post", URL: "https://explorer.invalid/x", Err: context.DeadlineExceeded}
}

var commitIndeterminate = []string{"Shoal-Commit-Outcome", "indeterminate"}

type goldenScenario struct {
	name    string
	replies []func() (*http.Response, error)
	// call runs one client operation and describes its result.
	call       func(*DispatchClient) string
	credential func() (string, error)
	unbound    bool
}

func describeAction(action Action) string {
	var out strings.Builder
	fmt.Fprintf(&out, "id=%x version=%d state=%s agent=%q generation=%d capability=%q action=%q\n",
		action.ID, action.Version, action.State, action.AgentID, action.AgentGeneration,
		action.Capability, action.Action)
	fmt.Fprintf(&out, "input=%s output=%s error_code=%q\n", action.Input, action.Output, action.ErrorCode)
	fmt.Fprintf(&out, "deadline=%s created=%s updated=%s\n",
		action.Deadline.Format(time.RFC3339Nano), action.CreatedAt.Format(time.RFC3339Nano),
		action.UpdatedAt.Format(time.RFC3339Nano))
	fmt.Fprintf(&out, "executor_key=%s header=%s\n", action.ExecutorKey, action.ExecutorKey.HeaderValue())
	fmt.Fprintf(&out, "claim_id=%q fence=%d lease_until=%s effect_possible=%t\n",
		action.ClaimID, action.ClaimFence, action.ClaimLeaseUntil.Format(time.RFC3339Nano),
		action.EffectPossible)
	fmt.Fprintf(&out, "effected=%d/%d correlation=%q\n",
		action.Effected.Bytes, action.Effected.Chunks, action.CorrelationID)
	for _, report := range action.AmbiguityReports {
		fmt.Fprintf(&out, "report fence=%d outcome=%s target=%q reference=%q\n",
			report.ClaimFence, report.Outcome, report.Target, report.Reference)
	}
	return out.String()
}

func describeError(err error) string {
	if err == nil {
		return "error: <nil>\n"
	}
	var out strings.Builder
	fmt.Fprintf(&out, "error: %s\n", err)
	fmt.Fprintf(&out, "kind: %s\n", DispatchKind(err))
	var dispatchErr *DispatchError
	if errors.As(err, &dispatchErr) {
		fmt.Fprintf(&out, "status: %d code: %q failure: %q\n",
			dispatchErr.Status, dispatchErr.Code, dispatchErr.Failure)
	}
	fmt.Fprintf(&out, "is ambiguity unrecorded: %t\n", errors.Is(err, ErrAmbiguityUnrecorded))
	fmt.Fprintf(&out, "is unexpected EOF: %t is deadline: %t\n",
		errors.Is(err, io.ErrUnexpectedEOF), errors.Is(err, context.DeadlineExceeded))
	// The kinds along the cause chain, outermost first.
	var chain []string
	for cause := err; cause != nil; cause = errors.Unwrap(cause) {
		var link *DispatchError
		if errors.As(cause, &link) && link == cause {
			chain = append(chain, fmt.Sprintf("%s(%d)", link.Kind, link.Status))
		}
	}
	fmt.Fprintf(&out, "chain: %s\n", strings.Join(chain, " > "))
	return out.String()
}

func describe(action Action, err error) string {
	if err != nil {
		return describeError(err)
	}
	return describeAction(action) + describeError(nil)
}

func testDispatchClientGolden(t *testing.T) {
	actionID := []byte{'a', 0, 255}
	claimID := []byte("gw1|pod|claim")
	ctx := context.Background()
	request := func(reason string) RequestContext { return goldenRequestContext(reason) }

	claim := func(mutate ...func(*ClaimRequest)) func(*DispatchClient) string {
		return func(c *DispatchClient) string {
			r := ClaimRequest{Context: request("gateway_claim"), ExpectedVersion: 1,
				ClaimID: claimID, Lease: 90 * time.Second}
			for _, m := range mutate {
				m(&r)
			}
			return describe(c.Claim(ctx, actionID, r))
		}
	}
	extend := func(mutate ...func(*ExtendRequest)) func(*DispatchClient) string {
		return func(c *DispatchClient) string {
			r := ExtendRequest{Context: request("gateway_extend"), ClaimID: claimID,
				ClaimFence: 1, Lease: 2 * time.Minute}
			for _, m := range mutate {
				m(&r)
			}
			return describe(c.Extend(ctx, actionID, r))
		}
	}
	complete := func(mutate ...func(*Completion)) func(*DispatchClient) string {
		return func(c *DispatchClient) string {
			r := Completion{Context: request("gateway_complete"), ExpectedVersion: 3,
				ClaimID: claimID, ClaimFence: 1, Output: []byte(`{"reference":"ch_1","n":1}`)}
			for _, m := range mutate {
				m(&r)
			}
			return describe(c.Complete(ctx, actionID, r))
		}
	}
	failedCompletion := func(r *Completion) {
		r.Output, r.Failed, r.ErrorCode = nil, true, ErrorOutcomeUnknown
	}
	ambiguity := func(mutate ...func(*AmbiguityReport)) func(*DispatchClient) string {
		return func(c *DispatchClient) string {
			r := AmbiguityReport{Context: request("gateway_ambiguity"), ClaimFence: 1,
				Outcome: "outcome_unknown", Target: "api.example", Reference: "ch_1"}
			for _, m := range mutate {
				m(&r)
			}
			return describe(c.ReportAmbiguity(ctx, actionID, r))
		}
	}
	pull := func(after string, limit int) func(*DispatchClient) string {
		return func(c *DispatchClient) string {
			page, err := c.Pull(ctx, request("gateway_poll"), after, limit)
			if err != nil {
				return describeError(err)
			}
			var out strings.Builder
			fmt.Fprintf(&out, "next=%q date=%q received=%s\n", page.Next, page.Header.Get("Date"),
				page.ReceivedAt.Format(time.RFC3339Nano))
			for _, action := range page.Actions {
				out.WriteString(describeAction(action))
			}
			return out.String() + describeError(nil)
		}
	}
	resolve := func(agent string) func(*DispatchClient) string {
		return func(c *DispatchClient) string {
			r := request("gateway_startup")
			r.CorrelationID = nil
			descriptor, err := c.Resolve(ctx, []byte(agent), r)
			if err != nil {
				return describeError(err)
			}
			var out strings.Builder
			fmt.Fprintf(&out, "id=%q generation=%d ref=%q\n", descriptor.ID, descriptor.Generation,
				descriptor.ExecutorRef)
			for _, capability := range descriptor.Capabilities {
				actions, _ := descriptor.Actions(capability.Name)
				for _, action := range actions {
					fmt.Fprintf(&out, "capability=%q action=%q effects=%v input=%s output=%s\n",
						capability.Name, action.Name, action.Effects, action.InputSchema, action.OutputSchema)
				}
			}
			return out.String() + describeError(nil)
		}
	}

	claimed := goldenRecordJSON("claimed", 2, 1,
		`,"input":{"account":"a1"},"executor_key":"`+goldenExecutorKey+`"`)
	renewed := goldenRecordJSON("claimed", 5, 1, "")
	succeeded := goldenRecordJSON("succeeded", 4, 1, `,"output":{"n":1,"reference":"ch_1"}`)
	failed := goldenRecordJSON("failed", 4, 1, `,"error_code":"outcome_unknown"`)
	reported := goldenRecordJSON("claimed", 6, 2,
		`,"ambiguity_reports":[{"claim_fence":1,"outcome":"outcome_unknown","target":"api.example","reference":"ch_1","subject":"cw","actor":"YQ","reported_at":"2026-09-06T12:00:00Z"}]`)
	notReported := goldenRecordJSON("claimed", 6, 2, "")
	descriptor := func(ref, actions string) string {
		return `{"id":"YWdlbnQ","generation":7,"executor_ref":"` + ref +
			`","capabilities":[{"name":"effects.gateway","actions":[` + actions + `]}]}`
	}
	chargeAction := `{"name":"charge","input_schema":{"type":"object"},"output_schema":{"type":"object"},"effects":["egresses-content","external"],"effect":"external","requires_attestation":true}`

	statuses := []struct {
		name    string
		replies []func() (*http.Response, error)
	}{
		{"404", r(answer(404, `{"code":"not_found","message":"fleet action not found"}`))},
		{"404_malformed_code", r(answer(404, `{"code":"Not Found: see https://evil.example"}`))},
		{"409", r(answer(409, `{"code":"conflict","message":"fleet action conflict"}`))},
		{"400", r(answer(400, `{"code":"invalid_argument","message":"bad"}`))},
		{"401", r(answer(401, `{"code":"unauthorized"}`))},
		{"403", r(answer(403, ``))},
		{"500", r(answer(500, `{"code":"internal"}`))},
		{"502", r(answer(502, `<html>bad gateway</html>`))},
		{"503", r(answer(503, `{"code":"unavailable"}`))},
		{"503_indeterminate", r(answer(503, `{"code":"unavailable","indeterminate":true}`, commitIndeterminate...))},
		{"503_indeterminate_spelling", r(answer(503, ``, "Shoal-Commit-Outcome", " Indeterminate "))},
		{"503_body_flag_only", r(answer(503, `{"code":"unavailable","indeterminate":true}`))},
		{"504", r(answer(504, ``))},
		{"302", r(answer(302, ``, "Location", "https://elsewhere.invalid/"))},
		{"418", r(answer(418, `{"code":"teapot"}`))},
		{"transport", r(dropped)},
		{"timeout", r(timedOut)},
	}

	var scenarios []goldenScenario
	add := func(name string, call func(*DispatchClient) string, replies ...func() (*http.Response, error)) {
		scenarios = append(scenarios, goldenScenario{name: name, call: call, replies: replies})
	}

	// Claim.
	add("claim_ok", claim(), answer(200, claimed))
	add("claim_ok_201", claim(), answer(201, claimed))
	add("claim_ok_unknown_field", claim(), answer(200, strings.TrimSuffix(claimed, "}")+`,"future":{"x":[1]}}`))
	add("claim_ok_padded_std_executor_key", claim(), answer(200, strings.Replace(claimed, goldenExecutorKey,
		base64.StdEncoding.EncodeToString(mustDecodeRawURL(goldenExecutorKey)), 1)))
	add("claim_ok_no_executor_key", claim(), answer(200, strings.Replace(claimed,
		`,"executor_key":"`+goldenExecutorKey+`"`, "", 1)))
	add("claim_bad_executor_key", claim(), answer(200, strings.Replace(claimed, goldenExecutorKey, "AAAA", 1)))
	add("claim_executor_key_whitespace", claim(), answer(200, strings.Replace(claimed, goldenExecutorKey,
		goldenExecutorKey[:20]+`\n`+goldenExecutorKey[20:], 1)))
	add("claim_other_claim_id", claim(), answer(200, strings.Replace(claimed, "Z3cxfHBvZHxjbGFpbQ", "b3RoZXI", 1)))
	add("claim_other_action", claim(), answer(200, strings.Replace(claimed, `"id":"YQD_"`, `"id":"YWI"`, 1)))
	add("claim_state_queued", claim(), answer(200, strings.Replace(claimed, `"claimed"`, `"queued"`, 1)))
	add("claim_fence_zero", claim(), answer(200, strings.Replace(claimed, `"claim_fence":1`, `"claim_fence":0`, 1)))
	add("claim_padded_action_id", claim(), answer(200, strings.Replace(claimed, `"id":"YQD_"`, `"id":"YQD_="`, 1)))
	add("claim_bad_agent_id", claim(), answer(200, strings.Replace(claimed, `"agent_id":"YWdlbnQ"`, `"agent_id":"!"`, 1)))
	add("claim_bad_correlation", claim(), answer(200, strings.Replace(claimed, `"dHJhY2UtNTI3"`, `"dHJhY2UtNTI3=="`, 1)))
	add("claim_not_json", claim(), answer(200, `not json`))
	add("claim_trailing_data", claim(), answer(200, claimed+`{}`))
	add("claim_trailing_whitespace", claim(), answer(200, claimed+" \n"))
	add("claim_wrong_type", claim(), answer(200, strings.Replace(claimed, `"version":2`, `"version":"2"`, 1)))
	add("claim_null", claim(), answer(200, `null`))
	add("claim_empty_body", claim(), answer(200, ``))
	add("claim_oversize", claim(), answer(200, `{"pad":"`+strings.Repeat("x", 8<<20)+`"}`))
	add("claim_body_read_fails", claim(), answerLostBody(200))
	for _, s := range statuses {
		add("claim_"+s.name, claim(), s.replies...)
	}
	add("claim_error_body_read_fails", claim(), answerLostBody(409))

	// Extend.
	add("extend_ok", extend(), answer(200, renewed))
	add("extend_lease_past_deadline_then_ok", extend(),
		answer(200, strings.Replace(renewed, `"claim_lease_until":"2026-09-06T12:05:00Z"`,
			`"claim_lease_until":"2026-09-06T12:31:00Z"`, 1)), answer(200, renewed))
	add("extend_no_lease_then_lost", extend(),
		answer(200, strings.Replace(renewed, `,"claim_lease_until":"2026-09-06T12:05:00Z"`, "", 1)), dropped)
	add("extend_other_fence_then_ok", extend(),
		answer(200, strings.Replace(renewed, `"claim_fence":1`, `"claim_fence":2`, 1)), answer(200, renewed))
	add("extend_state_succeeded_then_404", extend(),
		answer(200, strings.Replace(renewed, `"claimed"`, `"succeeded"`, 1)), answer(404, `{"code":"not_found"}`))
	add("extend_lost_then_ok", extend(), dropped, answer(200, renewed))
	add("extend_lost_then_lost", extend(), dropped, dropped)
	add("extend_lost_then_409", extend(), dropped, answer(409, `{"code":"conflict"}`))
	add("extend_503_then_ok", extend(), answer(503, `{"code":"unavailable"}`), answer(200, renewed))
	add("extend_502_then_404", extend(), answer(502, ``), answer(404, `{"code":"not_found"}`))
	add("extend_indeterminate_then_ok", extend(), answer(503, ``, commitIndeterminate...), answer(200, renewed))
	for _, s := range statuses {
		replies := append(append([]func() (*http.Response, error){}, s.replies...), answer(200, renewed))
		add("extend_"+s.name, extend(), replies...)
	}

	// Complete.
	add("complete_ok", complete(), answer(200, succeeded))
	add("complete_ok_reordered_output", complete(), answer(200, strings.Replace(succeeded,
		`{"n":1,"reference":"ch_1"}`, `{"reference":"ch_1","n":1.0}`, 1)))
	add("complete_recorded_failed", complete(), answer(200, failed))
	add("complete_recorded_other_output", complete(), answer(200, strings.Replace(succeeded,
		`"ch_1"`, `"ch_2"`, 1)))
	add("complete_failed_ok", complete(failedCompletion), answer(200, failed))
	add("complete_failed_recorded_other_code", complete(failedCompletion), answer(200, strings.Replace(failed,
		`"outcome_unknown"`, `"invalid_executor_output"`, 1)))
	add("complete_failed_effected_ok", complete(failedCompletion, func(r *Completion) {
		r.Effected.Bytes, r.Effected.Chunks = 50, 2
	}), answer(200, strings.TrimSuffix(failed, "}")+`,"effected":{"bytes":50,"chunks":2}}`))
	add("complete_failed_effected_dropped", complete(failedCompletion, func(r *Completion) {
		r.Effected.Bytes = 50
	}), answer(200, failed))
	add("complete_version_not_past", complete(), answer(200, strings.Replace(succeeded, `"version":4`, `"version":3`, 1)),
		answer(200, succeeded))
	add("complete_other_fence_then_ok", complete(), answer(200, strings.Replace(succeeded,
		`"claim_fence":1`, `"claim_fence":2`, 1)), answer(200, succeeded))
	add("complete_claimed_then_claimed_then_ok", complete(), answer(200, renewed), answer(200, renewed),
		answer(200, succeeded))
	add("complete_not_json_then_ok", complete(), answer(200, `{`), answer(200, succeeded))
	add("complete_lost_then_ok", complete(), dropped, answer(200, succeeded))
	add("complete_lost_then_lost", complete(), dropped, dropped)
	add("complete_lost_then_409_then_ok", complete(), dropped, answer(409, `{"code":"conflict"}`), answer(200, succeeded))
	add("complete_lost_then_409_then_409", complete(), dropped, answer(409, `{"code":"conflict"}`),
		answer(409, `{"code":"conflict"}`))
	add("complete_lost_then_404_then_lost", complete(), dropped, answer(404, `{"code":"not_found"}`), dropped)
	add("complete_503_then_ok", complete(), answer(503, `{"code":"unavailable"}`), answer(200, succeeded))
	add("complete_indeterminate_then_recorded_failed", complete(), answer(503, ``, commitIndeterminate...),
		answer(200, failed))
	add("complete_400_then_ok", complete(), answer(400, `{"code":"invalid_argument"}`), answer(200, succeeded))
	add("complete_500_then_400_then_404", complete(), answer(500, ``), answer(400, ``), answer(404, ``))
	add("complete_502_then_504", complete(), answer(502, ``), answer(504, ``))
	add("complete_timeout_then_timeout", complete(), timedOut, timedOut)
	for _, s := range statuses {
		replies := append(append([]func() (*http.Response, error){}, s.replies...),
			answer(200, succeeded), answer(200, succeeded))
		add("complete_"+s.name, complete(), replies...)
	}

	// Ambiguity.
	add("ambiguity_ok", ambiguity(), answer(200, reported))
	add("ambiguity_not_carried_then_ok", ambiguity(), answer(200, notReported), answer(200, reported))
	add("ambiguity_not_carried_twice", ambiguity(), answer(200, notReported), answer(200, notReported))
	add("ambiguity_other_action_then_ok", ambiguity(), answer(200, strings.Replace(reported,
		`"id":"YQD_"`, `"id":"YWI"`, 1)), answer(200, reported))
	add("ambiguity_409_then_ok", ambiguity(), answer(409, `{"code":"conflict"}`), answer(200, reported))
	add("ambiguity_409_then_409", ambiguity(), answer(409, `{"code":"conflict"}`), answer(409, `{"code":"conflict"}`))
	add("ambiguity_409_then_400", ambiguity(), answer(409, `{"code":"conflict"}`), answer(400, `{"code":"invalid_argument"}`))
	add("ambiguity_409_then_lost", ambiguity(), answer(409, `{"code":"conflict"}`), dropped)
	add("ambiguity_409_then_not_carried", ambiguity(), answer(409, ``), answer(200, notReported))
	add("ambiguity_lost_then_ok", ambiguity(), dropped, answer(200, reported))
	add("ambiguity_lost_then_400", ambiguity(), dropped, answer(400, `{"code":"invalid_argument"}`))
	add("ambiguity_500_then_500", ambiguity(), answer(500, ``), answer(500, ``))
	add("ambiguity_not_json_then_404", ambiguity(), answer(200, `[`), answer(404, `{"code":"not_found"}`))
	for _, s := range statuses {
		replies := append(append([]func() (*http.Response, error){}, s.replies...), answer(200, reported))
		add("ambiguity_"+s.name, ambiguity(), replies...)
	}

	// Pull.
	add("pull_ok", pull("Y3Vyc29y", 10), answer(200, `{"actions":[`+goldenRecordJSON("queued", 1, 0, "")+`,`+
		renewed+`],"next":"bmV4dA"}`, "Date", "Sun, 06 Sep 2026 12:00:00 GMT", "X-Other", "dropped"))
	add("pull_empty", pull("", 1), answer(200, `{"actions":[]}`))
	add("pull_null_actions", pull("", 1), answer(200, `{"actions":null}`))
	add("pull_no_actions_key", pull("", 1), answer(200, `{}`))
	add("pull_bad_record", pull("", 1), answer(200, `{"actions":[{"id":""}]}`))
	add("pull_bad_record_claim_id", pull("", 1), answer(200, `{"actions":[`+
		strings.Replace(renewed, "Z3cxfHBvZHxjbGFpbQ", "!!", 1)+`]}`))
	for _, s := range statuses {
		add("pull_"+s.name, pull("", 1), s.replies...)
	}

	// Resolve.
	add("resolve_ok", resolve("agent"), answer(200, descriptor("gateway", chargeAction)))
	add("resolve_legacy_effect_only", resolve("agent"), answer(200, descriptor("gateway",
		`{"name":"charge","input_schema":{},"output_schema":{},"effect":"external"}`)))
	add("resolve_legacy_effect_empty", resolve("agent"), answer(200, descriptor("gateway",
		`{"name":"charge","input_schema":{},"output_schema":{},"effect":""}`)))
	add("resolve_inconsistent_effect", resolve("agent"), answer(200, descriptor("gateway",
		`{"name":"charge","input_schema":{},"output_schema":{},"effects":["reads-corpus"],"effect":"external"}`)))
	add("resolve_unknown_effect", resolve("agent"), answer(200, descriptor("gateway",
		`{"name":"charge","input_schema":{},"output_schema":{},"effects":["invented"]}`)))
	add("resolve_other_ref", resolve("agent"), answer(200, descriptor("other", chargeAction)))
	add("resolve_other_agent", resolve("agent"), answer(200, strings.Replace(descriptor("gateway", chargeAction),
		`"id":"YWdlbnQ"`, `"id":"b3RoZXI"`, 1)))
	add("resolve_bad_id", resolve("agent"), answer(200, `{"id":"!","executor_ref":"gateway"}`))
	add("resolve_unknown_top_level_field", resolve("agent"), answer(200, strings.TrimSuffix(
		descriptor("gateway", chargeAction), "}")+`,"subject":"cw","scopes":[]}`))
	for _, s := range statuses {
		add("resolve_"+s.name, resolve("agent"), s.replies...)
	}
	scenarios = append(scenarios, goldenScenario{name: "resolve_unbound", call: resolve("agent"), unbound: true})

	// Local refusals: nothing is sent.
	refusals := map[string]func(*DispatchClient) string{
		"claim_no_action_id":       func(c *DispatchClient) string { return describe(c.Claim(ctx, nil, ClaimRequest{})) },
		"claim_overlong_action_id": func(c *DispatchClient) string { return describe(c.Claim(ctx, make([]byte, 257), ClaimRequest{})) },
		"claim_no_correlation":     claim(func(r *ClaimRequest) { r.Context.CorrelationID = nil }),
		"claim_bad_correlation":    claim(func(r *ClaimRequest) { r.Context.CorrelationID = []byte("has space") }),
		"claim_overlong_correlation": claim(func(r *ClaimRequest) {
			r.Context.CorrelationID = bytes.Repeat([]byte("c"), 1025)
		}),
		"claim_max_correlation_sent": claim(func(r *ClaimRequest) {
			r.Context.CorrelationID = bytes.Repeat([]byte("c"), 1024)
		}),
		"claim_no_version":            claim(func(r *ClaimRequest) { r.ExpectedVersion = 0 }),
		"claim_no_claim_id":           claim(func(r *ClaimRequest) { r.ClaimID = nil }),
		"claim_overlong_claim":        claim(func(r *ClaimRequest) { r.ClaimID = make([]byte, 257) }),
		"claim_zero_lease":            claim(func(r *ClaimRequest) { r.Lease = 0 }),
		"claim_long_lease":            claim(func(r *ClaimRequest) { r.Lease = 5*time.Minute + 1 }),
		"claim_no_request_id":         claim(func(r *ClaimRequest) { r.Context.RequestID = nil }),
		"claim_long_request_id":       claim(func(r *ClaimRequest) { r.Context.RequestID = make([]byte, 257) }),
		"claim_no_reason":             claim(func(r *ClaimRequest) { r.Context.ReasonCode = "" }),
		"claim_padded_reason":         claim(func(r *ClaimRequest) { r.Context.ReasonCode = " gateway" }),
		"claim_long_reason":           claim(func(r *ClaimRequest) { r.Context.ReasonCode = strings.Repeat("r", 65) }),
		"claim_past_deadline":         claim(func(r *ClaimRequest) { r.Context.Deadline = goldenNow }),
		"claim_zero_deadline":         claim(func(r *ClaimRequest) { r.Context.Deadline = time.Time{} }),
		"extend_no_claim_id":          extend(func(r *ExtendRequest) { r.ClaimID = nil }),
		"extend_no_fence":             extend(func(r *ExtendRequest) { r.ClaimFence = 0 }),
		"extend_long_lease":           extend(func(r *ExtendRequest) { r.Lease = 5*time.Minute + 1 }),
		"extend_no_correlation":       extend(func(r *ExtendRequest) { r.Context.CorrelationID = nil }),
		"extend_past_deadline":        extend(func(r *ExtendRequest) { r.Context.Deadline = goldenNow.Add(-time.Second) }),
		"complete_no_version":         complete(func(r *Completion) { r.ExpectedVersion = 0 }),
		"complete_no_claim_id":        complete(func(r *Completion) { r.ClaimID = nil }),
		"complete_no_fence":           complete(func(r *Completion) { r.ClaimFence = 0 }),
		"complete_no_correlation":     complete(func(r *Completion) { r.Context.CorrelationID = nil }),
		"complete_failed_with_output": complete(func(r *Completion) { r.Failed, r.ErrorCode = true, ErrorOutcomeUnknown }),
		"complete_failed_bad_code":    complete(failedCompletion, func(r *Completion) { r.ErrorCode = "made_up" }),
		"complete_ok_with_code":       complete(func(r *Completion) { r.ErrorCode = ErrorOutcomeUnknown }),
		"complete_ok_no_output":       complete(func(r *Completion) { r.Output = nil }),
		"complete_ok_effected":        complete(func(r *Completion) { r.Effected.Bytes = 1 }),
		"complete_negative_effected":  complete(failedCompletion, func(r *Completion) { r.Effected.Bytes = -1 }),
		"complete_huge_effected":      complete(failedCompletion, func(r *Completion) { r.Effected.Bytes = 1<<40 + 1 }),
		"complete_many_chunks":        complete(failedCompletion, func(r *Completion) { r.Effected.Bytes, r.Effected.Chunks = 1, 1<<24+1 }),
		"complete_chunks_no_bytes":    complete(failedCompletion, func(r *Completion) { r.Effected.Chunks = 1 }),
		"complete_bad_context":        complete(func(r *Completion) { r.Context.ReasonCode = "" }),
		"complete_unencodable_output": complete(func(r *Completion) { r.Output = []byte(`{`) }),
		"ambiguity_no_fence":          ambiguity(func(r *AmbiguityReport) { r.ClaimFence = 0 }),
		"ambiguity_bad_outcome":       ambiguity(func(r *AmbiguityReport) { r.Outcome = "made_up" }),
		"ambiguity_long_target":       ambiguity(func(r *AmbiguityReport) { r.Target = strings.Repeat("t", 257) }),
		"ambiguity_control_target":    ambiguity(func(r *AmbiguityReport) { r.Target = "a\x00b" }),
		"ambiguity_bidi_reference":    ambiguity(func(r *AmbiguityReport) { r.Reference = "a‮b" }),
		"ambiguity_long_reference":    ambiguity(func(r *AmbiguityReport) { r.Reference = strings.Repeat("r", 257) }),
		"ambiguity_no_correlation":    ambiguity(func(r *AmbiguityReport) { r.Context.CorrelationID = nil }),
		"pull_zero_limit":             pull("", 0),
		"pull_big_limit":              pull("", 257),
		"pull_padded_cursor":          pull("YWI=", 1),
		"resolve_no_agent":            resolve(""),
	}
	names := make([]string, 0, len(refusals))
	for name := range refusals {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		scenarios = append(scenarios, goldenScenario{name: "local_" + name, call: refusals[name],
			replies: r(answer(200, claimed))})
	}

	// Credentials.
	for name, credential := range map[string]func() (string, error){
		"credential_fails":     func() (string, error) { return "", errors.New("/secret/path unreadable") },
		"credential_empty":     func() (string, error) { return "", nil },
		"credential_line_feed": func() (string, error) { return "tok\nen", nil },
		"credential_return":    func() (string, error) { return "tok\ren", nil },
	} {
		scenarios = append(scenarios, goldenScenario{name: name, call: claim(), credential: credential,
			replies: r(answer(200, claimed))})
	}

	seen := map[string]bool{}
	for _, scenario := range scenarios {
		if seen[scenario.name] {
			t.Fatalf("duplicate scenario %s", scenario.name)
		}
		seen[scenario.name] = true
		t.Run(scenario.name, func(t *testing.T) {
			transport := &goldenTransport{replies: scenario.replies}
			base, _ := url.Parse("https://explorer.invalid/shoal")
			credential := scenario.credential
			if credential == nil {
				credential = func() (string, error) { return "token", nil }
			}
			// Production hands the client NewExplorerClient, which follows
			// no redirect; so does this one.
			client, err := NewDispatchClient(base,
				&http.Client{Transport: transport, CheckRedirect: refuseRedirect},
				credential, func() time.Time { return goldenNow })
			if err != nil {
				t.Fatal(err)
			}
			if !scenario.unbound {
				if client, err = client.BindExecutor("gateway"); err != nil {
					t.Fatal(err)
				}
			}
			result := scenario.call(client)
			var out strings.Builder
			fmt.Fprintf(&out, "requests: %d\n", len(transport.requests))
			for _, request := range transport.requests {
				fmt.Fprintf(&out, "request: %s\n", request)
			}
			out.WriteString(result)
			checkDispatchGolden(t, filepath.Join(dispatchGoldenRoot, "client", scenario.name+".golden"), out.String())
		})
	}
	if *updateDispatchGolden {
		return
	}
	entries, err := os.ReadDir(filepath.Join(dispatchGoldenRoot, "client"))
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if name := strings.TrimSuffix(entry.Name(), ".golden"); !seen[name] {
			t.Errorf("orphaned golden fixture %s", entry.Name())
		}
	}
}

func r(replies ...func() (*http.Response, error)) []func() (*http.Response, error) { return replies }

func mustDecodeRawURL(value string) []byte {
	decoded, err := base64.RawURLEncoding.DecodeString(value)
	if err != nil {
		panic(err)
	}
	return decoded
}

var _ = hex.EncodeToString

func checkDispatchGolden(t *testing.T, path, got string) {
	t.Helper()
	if *updateDispatchGolden {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("missing golden fixture (run with -update): %v", err)
	}
	if !bytes.Equal(want, []byte(got)) {
		t.Fatalf("dispatch wire changed for %s\n--- want\n%s\n--- got\n%s", filepath.Base(path), want, got)
	}
}
