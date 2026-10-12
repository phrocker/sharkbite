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
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	dispatchapi "github.com/phrocker/shoal-oss/pkg/dispatch/api"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

// The dispatch client's wire behaviour is tested against the real explorer
// handler in cmd/shoal-explore-web (effects_gateway_client_test.go), and every
// answer sequence is pinned in dispatch_golden_test.go. These are the pure
// parts: the status mapping, which the real handler cannot be made to produce
// on demand for every row, and the local refusals.

func TestStatusErrorMapping(t *testing.T) {
	for _, row := range []struct {
		status int
		header http.Header
		body   string
		kind   DispatchErrorKind
		code   string
	}{
		{404, nil, `{"code":"not_found","message":"fleet action not found"}`, DispatchNotFound, "not_found"},
		{409, nil, `{"code":"conflict"}`, DispatchConflict, "conflict"},
		{503, http.Header{"Shoal-Commit-Outcome": {"indeterminate"}}, `{"code":"unavailable","indeterminate":true}`, DispatchIndeterminate, "unavailable"},
		{503, http.Header{"Shoal-Commit-Outcome": {" Indeterminate "}}, ``, DispatchIndeterminate, ""},
		{503, nil, `{"code":"unavailable"}`, DispatchUnavailable, "unavailable"},
		{503, http.Header{"Shoal-Commit-Outcome": {"committed"}}, ``, DispatchUnavailable, ""},
		{400, nil, `{"code":"invalid_argument"}`, DispatchInvalid, "invalid_argument"},
		{401, nil, ``, DispatchUnauthorized, ""},
		{403, nil, ``, DispatchUnauthorized, ""},
		{504, nil, ``, DispatchDeadline, ""},
		{500, nil, `{"code":"internal"}`, DispatchStatus, "internal"},
		{502, nil, `<html>bad gateway</html>`, DispatchStatus, ""},
		// A code that is not a short snake-case token is dropped, so a proxy
		// cannot put text into the error through it.
		{409, nil, `{"code":"Conflict: see https://evil.example"}`, DispatchConflict, ""},
	} {
		header := row.header
		if header == nil {
			header = http.Header{}
		}
		// Pull maps a status as it is: no resend, no re-pull.
		status, body := row.status, row.body
		transport := &scriptedTransport{replies: []func() (*http.Response, error){
			func() (*http.Response, error) {
				return &http.Response{StatusCode: status, Header: header,
					Body: io.NopCloser(strings.NewReader(body))}, nil
			},
		}}
		base, _ := url.Parse("https://explorer.invalid")
		client, err := NewDispatchClient(base, &http.Client{Transport: transport},
			func() (string, error) { return "token", nil }, nil)
		if err != nil {
			t.Fatal(err)
		}
		_, err = client.Pull(context.Background(), RequestContext{RequestID: []byte("r"),
			ReasonCode: "gateway_poll", Deadline: time.Now().Add(time.Minute)}, "", 1)
		var dispatchErr *DispatchError
		if !errors.As(err, &dispatchErr) || dispatchErr.Kind != row.kind ||
			dispatchErr.Code != row.code || dispatchErr.Status != row.status {
			t.Errorf("%d %v %s: %#v", row.status, row.header, row.body, err)
		}
		if DispatchKind(err) != row.kind {
			t.Errorf("%d: DispatchKind = %q", row.status, DispatchKind(err))
		}
	}
	if DispatchKind(errors.New("other")) != "" {
		t.Error("a foreign error has a dispatch kind")
	}
}

func TestDispatchErrorNeverFormatsItsCause(t *testing.T) {
	const secret = "/api/v1/fleet/actions/c2VjcmV0/claim?token=s3cret"
	cause := &url.Error{Op: "Post", URL: "https://explorer" + secret, Err: context.DeadlineExceeded}
	err := &DispatchError{Op: "claim", Kind: DispatchTransport, Failure: ClassifyFailure(cause), cause: cause}
	if strings.Contains(err.Error(), "s3cret") || strings.Contains(err.Error(), "c2VjcmV0") {
		t.Fatalf("error text carries the cause: %s", err)
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal("the cause is not reachable through errors.Is")
	}
	if !strings.Contains(err.Error(), "timeout") {
		t.Fatalf("error text lacks the classified kind: %s", err)
	}
}

func TestNewClaimID(t *testing.T) {
	random := bytes.NewReader(bytes.Repeat([]byte{0xaa}, 64))
	id, nonce, err := NewClaimID("gateway-7f9c-0", random)
	if err != nil {
		t.Fatal(err)
	}
	want := append([]byte("gw1|gateway-7f9c-0|"), bytes.Repeat([]byte{0xaa}, ClaimNonceBytes)...)
	if !bytes.Equal(id, want) || nonce != (ClaimNonce{0xaa, 0xaa, 0xaa, 0xaa, 0xaa, 0xaa, 0xaa, 0xaa, 0xaa, 0xaa, 0xaa, 0xaa, 0xaa, 0xaa, 0xaa, 0xaa}) {
		t.Fatalf("claim ID = %q, nonce = %x", id, nonce)
	}
	// Fresh per attempt from the real source.
	a, _, err := NewClaimID("pod", nil)
	if err != nil {
		t.Fatal(err)
	}
	b, _, _ := NewClaimID("pod", nil)
	if bytes.Equal(a, b) {
		t.Fatal("two claim attempts produced one claim ID")
	}
	longest := strings.Repeat("p", dispatchapi.MaxActionIDBytes-len(claimIDPrefix)-1-ClaimNonceBytes)
	if id, _, err := NewClaimID(longest, nil); err != nil || len(id) != dispatchapi.MaxActionIDBytes {
		t.Fatalf("longest pod name: %d bytes, %v", len(id), err)
	}
	for _, pod := range []string{"", longest + "p", "a|b", "a b", "pod\n", "pöd"} {
		if _, _, err := NewClaimID(pod, nil); err == nil {
			t.Errorf("pod %q accepted", pod)
		}
	}
	if _, _, err := NewClaimID("pod", bytes.NewReader(nil)); err == nil {
		t.Fatal("an exhausted random source produced a claim ID")
	}
}

func TestRequestContextWire(t *testing.T) {
	now := time.Date(2026, 10, 8, 12, 0, 0, 0, time.FixedZone("EST", -5*3600))
	good := RequestContext{RequestID: []byte{0, 1}, ReasonCode: "gateway_claim", Deadline: now.Add(time.Minute)}
	resolve := func(request RequestContext) (*scriptedTransport, error) {
		transport := &scriptedTransport{replies: []func() (*http.Response, error){
			reply(404, `{"code":"not_found"}`)}}
		base, _ := url.Parse("https://explorer.invalid")
		client, err := NewDispatchClient(base, &http.Client{Transport: transport},
			func() (string, error) { return "token", nil }, func() time.Time { return now })
		if err != nil {
			t.Fatal(err)
		}
		if client, err = client.BindExecutor("gateway"); err != nil {
			t.Fatal(err)
		}
		_, err = client.Resolve(context.Background(), []byte("agent"), request)
		return transport, err
	}
	// The resolve body is the context alone.
	transport, err := resolve(good)
	if DispatchKind(err) != DispatchNotFound || len(transport.bodies) != 1 {
		t.Fatalf("resolve = %v after %d requests", err, len(transport.bodies))
	}
	if want := `{"request_id":"AAE","reason_code":"gateway_claim","deadline":"2026-10-08T17:01:00Z"}`; string(transport.bodies[0]) != want {
		t.Fatalf("context wire = %s, want %s (the deadline in UTC, no correlation)", transport.bodies[0], want)
	}
	for name, mutate := range map[string]func(*RequestContext){
		"no request ID":        func(r *RequestContext) { r.RequestID = nil },
		"long request ID":      func(r *RequestContext) { r.RequestID = make([]byte, dispatchapi.MaxActionIDBytes+1) },
		"long correlation ID":  func(r *RequestContext) { r.CorrelationID = make([]byte, shoal.MaxIDBytes+1) },
		"no reason":            func(r *RequestContext) { r.ReasonCode = "" },
		"untrimmed reason":     func(r *RequestContext) { r.ReasonCode = "claim " },
		"long reason":          func(r *RequestContext) { r.ReasonCode = strings.Repeat("r", dispatchapi.MaxReasonCodeBytes+1) },
		"deadline now":         func(r *RequestContext) { r.Deadline = now },
		"deadline in the past": func(r *RequestContext) { r.Deadline = now.Add(-time.Second) },
		"no deadline":          func(r *RequestContext) { r.Deadline = time.Time{} },
	} {
		request := good
		mutate(&request)
		transport, err := resolve(request)
		if DispatchKind(err) != DispatchRefusedLocal || len(transport.bodies) != 0 {
			t.Errorf("%s: %v after %d requests", name, err, len(transport.bodies))
		}
	}
}

func TestDispatchClientRefusesLocallyBeforeSending(t *testing.T) {
	base, _ := url.Parse("https://explorer.invalid")
	transport := &countingTransport{}
	client, err := NewDispatchClient(base, &http.Client{Transport: transport},
		func() (string, error) { return "token", nil }, nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	request := RequestContext{RequestID: []byte("r"), ReasonCode: "x", Deadline: time.Now().Add(time.Minute), CorrelationID: []byte("trace")}
	claimID := []byte("claim")
	checks := map[string]error{}
	_, checks["pull limit 0"] = client.Pull(ctx, request, "", 0)
	_, checks["pull limit 257"] = client.Pull(ctx, request, "", dispatchapi.MaxDispatchListResults+1)
	_, checks["pull bad cursor"] = client.Pull(ctx, request, "!!", 1)
	_, checks["claim no ID"] = client.Claim(ctx, nil, ClaimRequest{Context: request, ExpectedVersion: 1, ClaimID: claimID, Lease: time.Minute})
	_, checks["claim no version"] = client.Claim(ctx, []byte("a"), ClaimRequest{Context: request, ClaimID: claimID, Lease: time.Minute})
	_, checks["claim no claim ID"] = client.Claim(ctx, []byte("a"), ClaimRequest{Context: request, ExpectedVersion: 1, Lease: time.Minute})
	_, checks["claim zero lease"] = client.Claim(ctx, []byte("a"), ClaimRequest{Context: request, ExpectedVersion: 1, ClaimID: claimID})
	_, checks["claim lease over ceiling"] = client.Claim(ctx, []byte("a"), ClaimRequest{Context: request, ExpectedVersion: 1, ClaimID: claimID, Lease: dispatchapi.MaxActionClaimTTL + 1})
	_, checks["complete both output and failure"] = client.Complete(ctx, []byte("a"), Completion{Context: request, ExpectedVersion: 1, ClaimID: claimID, ClaimFence: 1, Output: []byte(`{}`), Failed: true, ErrorCode: ErrorOutcomeUnknown})
	_, checks["complete success with code"] = client.Complete(ctx, []byte("a"), Completion{Context: request, ExpectedVersion: 1, ClaimID: claimID, ClaimFence: 1, Output: []byte(`{}`), ErrorCode: ErrorOutcomeUnknown})
	_, checks["complete success without output"] = client.Complete(ctx, []byte("a"), Completion{Context: request, ExpectedVersion: 1, ClaimID: claimID})
	_, checks["complete failure off vocabulary"] = client.Complete(ctx, []byte("a"), Completion{Context: request, ExpectedVersion: 1, ClaimID: claimID, ClaimFence: 1, Failed: true, ErrorCode: "executor_error"})
	_, checks["complete failure without code"] = client.Complete(ctx, []byte("a"), Completion{Context: request, ExpectedVersion: 1, ClaimID: claimID, ClaimFence: 1, Failed: true})
	_, checks["resolve no agent"] = client.Resolve(ctx, nil, request)
	for name, err := range checks {
		if DispatchKind(err) != DispatchRefusedLocal {
			t.Errorf("%s: %v", name, err)
		}
	}
	if transport.calls != 0 {
		t.Fatalf("%d requests left for locally refused calls", transport.calls)
	}

	noToken, _ := NewDispatchClient(base, &http.Client{Transport: transport},
		func() (string, error) { return "", ErrNoCredential }, nil)
	if _, err := noToken.Pull(ctx, request, "", 1); DispatchKind(err) != DispatchNoCredential {
		t.Fatalf("no credential = %v", err)
	}
	injected, _ := NewDispatchClient(base, &http.Client{Transport: transport},
		func() (string, error) { return "t\r\nX-Evil: 1", nil }, nil)
	if _, err := injected.Pull(ctx, request, "", 1); DispatchKind(err) != DispatchNoCredential {
		t.Fatalf("credential with a line break = %v", err)
	}
	if transport.calls != 0 {
		t.Fatal("a request left without a usable credential")
	}
}

type countingTransport struct{ calls int }

func (c *countingTransport) RoundTrip(*http.Request) (*http.Response, error) {
	c.calls++
	return nil, errors.New("no network in this test")
}

// scriptedTransport answers each request with the next scripted reply. The
// completion recovery is about what the client does with a sequence of
// answers, which the real handler cannot be made to produce on demand; the
// wire shapes themselves are pinned against the real handler.
type scriptedTransport struct {
	replies  []func() (*http.Response, error)
	bodies   [][]byte
	requests []string // method and path of every request, in order
}

func (s *scriptedTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	body, _ := io.ReadAll(request.Body)
	s.bodies = append(s.bodies, body)
	s.requests = append(s.requests, request.Method+" "+request.URL.Path)
	if len(s.replies) == 0 {
		return nil, errors.New("script exhausted")
	}
	next := s.replies[0]
	s.replies = s.replies[1:]
	return next()
}

func reply(status int, body string, header ...string) func() (*http.Response, error) {
	return func() (*http.Response, error) {
		h := http.Header{"Content-Type": {"application/json"}}
		for i := 0; i+1 < len(header); i += 2 {
			h.Set(header[i], header[i+1])
		}
		return &http.Response{StatusCode: status, Header: h,
			Body: io.NopCloser(strings.NewReader(body))}, nil
	}
}

func lost() (*http.Response, error) { return nil, io.ErrUnexpectedEOF }

var indeterminate = []string{"Shoal-Commit-Outcome", "indeterminate"}

func committed(t *testing.T, version uint64, state dispatchapi.DispatchState, code, output string) string {
	t.Helper()
	return committedUnder(t, 1, version, state, code, output)
}

// committedUnder is committed() under a chosen claim fence.
func committedUnder(t *testing.T, fence, version uint64, state dispatchapi.DispatchState, code, output string) string {
	t.Helper()
	record := map[string]any{
		"id": base64.RawURLEncoding.EncodeToString([]byte("action")), "version": version,
		"state": state, "agent_id": "", "claim_id": base64.RawURLEncoding.EncodeToString([]byte("claim")),
		"claim_fence": fence, "effect_possible": true,
	}
	if code != "" {
		record["error_code"] = code
	}
	if output != "" {
		record["output"] = json.RawMessage(output)
	}
	encoded, err := json.Marshal(record)
	if err != nil {
		t.Fatal(err)
	}
	return string(encoded)
}

func TestCompleteRecoversOnlyWhatTheResendConfirms(t *testing.T) {
	failure := Completion{ExpectedVersion: 2, ClaimID: []byte("claim"), ClaimFence: 1, Failed: true,
		ErrorCode: TargetRejected(422)}
	success := Completion{ExpectedVersion: 2, ClaimID: []byte("claim"), ClaimFence: 1,
		Output: json.RawMessage(`{"status":200,"idempotency":"key","reference":"ch_1"}`)}
	failedRecord := committed(t, 3, dispatchapi.DispatchFailed, "target_rejected_422", "")
	successRecord := committed(t, 3, dispatchapi.DispatchSucceeded, "", `{"idempotency":"key","reference":"ch_1","status":200}`)
	for _, row := range []struct {
		name       string
		completion Completion
		replies    []func() (*http.Response, error)
		kind       DispatchErrorKind
		calls      int
	}{
		{"clean success", success, []func() (*http.Response, error){reply(200, successRecord)}, "", 1},
		{"stored output re-encoded is the same output", success,
			[]func() (*http.Response, error){reply(200, successRecord)}, "", 1},
		// The defect: the resend's lost answer was replaced by the first
		// attempt's 500, a definite-looking status for a report that may
		// have committed.
		{"500 then indeterminate 503", failure,
			[]func() (*http.Response, error){reply(500, `{"code":"internal"}`), reply(503, `{}`, indeterminate...)},
			DispatchIndeterminate, 2},
		{"500 then transport loss", failure,
			[]func() (*http.Response, error){reply(500, `{}`), lost}, DispatchIndeterminate, 2},
		{"400 then indeterminate 503", success,
			[]func() (*http.Response, error){reply(400, `{}`), reply(503, `{}`, indeterminate...)},
			DispatchIndeterminate, 2},
		{"#492 workaround: 500 then the recorded failure", failure,
			[]func() (*http.Response, error){reply(500, `{}`), reply(200, failedRecord)}, "", 2},
		{"lost response then the committed record", success,
			[]func() (*http.Response, error){lost, reply(200, successRecord)}, "", 2},
		{"indeterminate then the committed record", failure,
			[]func() (*http.Response, error){reply(503, `{}`, indeterminate...), reply(200, failedRecord)}, "", 2},
		{"lost twice", success, []func() (*http.Response, error){lost, lost}, DispatchIndeterminate, 2},
		{"indeterminate twice", success,
			[]func() (*http.Response, error){reply(503, `{}`, indeterminate...), reply(503, `{}`, indeterminate...)},
			DispatchIndeterminate, 2},
		// A #492 400/500 after a lost first attempt is not definite: the
		// record is read once more through the replay branch.
		{"lost then 500 then the recorded failure", failure,
			[]func() (*http.Response, error){lost, reply(500, `{}`), reply(200, failedRecord)}, "", 3},
		{"lost then 400 then lost", success,
			[]func() (*http.Response, error){lost, reply(400, `{}`), lost}, DispatchIndeterminate, 3},
		{"lost then 500 then 500", failure,
			[]func() (*http.Response, error){lost, reply(500, `{}`), reply(500, `{}`)}, DispatchIndeterminate, 3},
		{"indeterminate then 400 then the record", success,
			[]func() (*http.Response, error){reply(503, `{}`, indeterminate...), reply(400, `{}`),
				reply(200, committed(t, 3, dispatchapi.DispatchFailed, "invalid_executor_output", ""))},
			DispatchRecordedOtherwise, 3},
		// A refusal answering the resend is not definite after a
		// possibly-committed first attempt: the first write may still be in
		// flight (ErrExecutionAmbiguous). One third read; only a record
		// settles it.
		{"lost then conflict then conflict", success,
			[]func() (*http.Response, error){lost, reply(409, `{"code":"conflict"}`), reply(409, `{"code":"conflict"}`)},
			DispatchIndeterminate, 3},
		{"lost then conflict then the record", success,
			[]func() (*http.Response, error){lost, reply(409, `{"code":"conflict"}`), reply(200, successRecord)}, "", 3},
		{"500 then conflict then conflict", failure,
			[]func() (*http.Response, error){reply(500, `{}`), reply(409, `{}`), reply(409, `{}`)},
			DispatchIndeterminate, 3},
		// The first 500 may have been a genuine error that committed nothing
		// while the resend committed and answered 500 (#492): not definite.
		{"500 then 500 then 500", failure,
			[]func() (*http.Response, error){reply(500, `{}`), reply(500, `{}`), reply(500, `{}`)},
			DispatchIndeterminate, 3},
		{"500 then 500 then the recorded failure", failure,
			[]func() (*http.Response, error){reply(500, `{}`), reply(500, `{}`), reply(200, failedRecord)}, "", 3},
		{"400 then 400 then 409", success,
			[]func() (*http.Response, error){reply(400, `{}`), reply(400, `{}`), reply(409, `{}`)},
			DispatchIndeterminate, 3},
		{"500 then 400 then the record otherwise", success,
			[]func() (*http.Response, error){reply(500, `{}`), reply(400, `{}`),
				reply(200, committed(t, 3, dispatchapi.DispatchFailed, "invalid_executor_output", ""))},
			DispatchRecordedOtherwise, 3},
		// Interim until #505: a 503 without the indeterminate header may
		// hide a committed write (ErrExecutionAmbiguous, ErrActionCommitted),
		// so it is resent like a lost response and never returned as a
		// refusal.
		{"plain 503 then the committed record", success,
			[]func() (*http.Response, error){reply(503, `{"code":"unavailable"}`), reply(200, successRecord)}, "", 2},
		{"plain 503 then the record otherwise", success,
			[]func() (*http.Response, error){reply(503, `{}`),
				reply(200, committed(t, 3, dispatchapi.DispatchFailed, "invalid_executor_output", ""))},
			DispatchRecordedOtherwise, 2},
		{"plain 503 twice", failure,
			[]func() (*http.Response, error){reply(503, `{}`), reply(503, `{}`)}, DispatchIndeterminate, 2},
		{"plain 503 then indeterminate 503", failure,
			[]func() (*http.Response, error){reply(503, `{}`), reply(503, `{}`, indeterminate...)}, DispatchIndeterminate, 2},
		{"plain 503 then lost", failure,
			[]func() (*http.Response, error){reply(503, `{}`), lost}, DispatchIndeterminate, 2},
		{"lost then plain 503", success,
			[]func() (*http.Response, error){lost, reply(503, `{}`)}, DispatchIndeterminate, 2},
		{"500 then plain 503", failure,
			[]func() (*http.Response, error){reply(500, `{}`), reply(503, `{}`)}, DispatchIndeterminate, 2},
		{"plain 503 then conflict then conflict", success,
			[]func() (*http.Response, error){reply(503, `{}`), reply(409, `{"code":"conflict"}`), reply(409, `{"code":"conflict"}`)},
			DispatchIndeterminate, 3},
		{"plain 503 then conflict then the record", failure,
			[]func() (*http.Response, error){reply(503, `{}`), reply(409, `{}`), reply(200, failedRecord)}, "", 3},
		{"plain 503 then conflict then plain 503", failure,
			[]func() (*http.Response, error){reply(503, `{}`), reply(409, `{}`), reply(503, `{}`)}, DispatchIndeterminate, 3},
		{"plain 503 then 404 then 404", success,
			[]func() (*http.Response, error){reply(503, `{}`), reply(404, `{}`), reply(404, `{}`)}, DispatchIndeterminate, 3},
		{"plain 503 then 500 then the recorded failure", failure,
			[]func() (*http.Response, error){reply(503, `{}`), reply(500, `{}`), reply(200, failedRecord)}, "", 3},
		{"plain 503 then 400 then 400", success,
			[]func() (*http.Response, error){reply(503, `{}`), reply(400, `{}`), reply(400, `{}`)}, DispatchIndeterminate, 3},
		// No possibly-committed answer before it: a refusal is definite and
		// nothing is resent.
		{"404", success, []func() (*http.Response, error){reply(404, `{}`)}, DispatchNotFound, 1},
		{"409", success, []func() (*http.Response, error){reply(409, `{"code":"conflict"}`)}, DispatchConflict, 1},
		// Recorded otherwise: every reported field is compared.
		{"failure recorded with another code", failure,
			[]func() (*http.Response, error){reply(200, committed(t, 3, dispatchapi.DispatchFailed, "outcome_unknown", ""))},
			DispatchRecordedOtherwise, 1},
		{"success recorded with other output", success,
			[]func() (*http.Response, error){reply(200, committed(t, 3, dispatchapi.DispatchSucceeded, "", `{"status":200,"idempotency":"key"}`))},
			DispatchRecordedOtherwise, 1},
		{"success recorded as failed", success,
			[]func() (*http.Response, error){reply(400, `{}`), reply(200, committed(t, 3, dispatchapi.DispatchFailed, "invalid_executor_output", ""))},
			DispatchRecordedOtherwise, 2},
		// With the fence, 200 comes for this claim generation's terminal
		// record at any version past ExpectedVersion: an extension or an
		// ambiguity report may have moved the version since the worker last
		// saw it, and the fence, not the version, binds the completion.
		{"version past the report under the fence", failure,
			[]func() (*http.Response, error){reply(200, committed(t, 5, dispatchapi.DispatchFailed, "target_rejected_422", ""))},
			"", 1},
		// Another fence is another generation's record, never this report's.
		// But the route answered success, so something committed: resend and
		// read the record through the replay branch, never protocol alone.
		{"another fence then the record", failure,
			[]func() (*http.Response, error){reply(200, committedUnder(t, 2, 3, dispatchapi.DispatchFailed, "target_rejected_422", "")),
				reply(200, failedRecord)}, "", 2},
		{"another fence twice then the record", failure,
			[]func() (*http.Response, error){reply(200, committedUnder(t, 2, 3, dispatchapi.DispatchFailed, "target_rejected_422", "")),
				reply(200, committedUnder(t, 2, 3, dispatchapi.DispatchFailed, "target_rejected_422", "")), reply(200, failedRecord)}, "", 3},
		{"another fence three times", success,
			[]func() (*http.Response, error){reply(200, committedUnder(t, 2, 3, dispatchapi.DispatchSucceeded, "", `{}`)),
				reply(200, committedUnder(t, 2, 3, dispatchapi.DispatchSucceeded, "", `{}`)),
				reply(200, committedUnder(t, 2, 3, dispatchapi.DispatchSucceeded, "", `{}`))},
			DispatchIndeterminate, 3},
		{"version not past the report, three times", failure,
			[]func() (*http.Response, error){reply(200, committed(t, 2, dispatchapi.DispatchFailed, "target_rejected_422", "")),
				reply(200, committed(t, 2, dispatchapi.DispatchFailed, "target_rejected_422", "")),
				reply(200, committed(t, 2, dispatchapi.DispatchFailed, "target_rejected_422", ""))},
			DispatchIndeterminate, 3},
		{"undecodable 2xx then the record", success,
			[]func() (*http.Response, error){reply(200, `not json`), reply(200, successRecord)}, "", 2},
		{"undecodable 2xx then the record otherwise", success,
			[]func() (*http.Response, error){reply(201, `{"id":"!!"}`),
				reply(200, committed(t, 3, dispatchapi.DispatchFailed, "invalid_executor_output", ""))},
			DispatchRecordedOtherwise, 2},
		{"undecodable 2xx then lost", success,
			[]func() (*http.Response, error){reply(200, `not json`), lost}, DispatchIndeterminate, 2},
		{"undecodable 2xx then 409 then 409", success,
			[]func() (*http.Response, error){reply(200, `{}`), reply(409, `{}`), reply(409, `{}`)}, DispatchIndeterminate, 3},
		{"undecodable 2xx three times", success,
			[]func() (*http.Response, error){reply(200, `[]`), reply(200, `[]`), reply(200, `[]`)}, DispatchIndeterminate, 3},
		{"lost then undecodable 2xx then the record", failure,
			[]func() (*http.Response, error){lost, reply(200, `nope`), reply(200, failedRecord)}, "", 3},
		// A proxy can answer 502 or 504 after the explorer processed the
		// request: possibly committed, like a lost response.
		{"502 then the record", failure,
			[]func() (*http.Response, error){reply(502, `<html>bad gateway</html>`), reply(200, failedRecord)}, "", 2},
		{"502 twice", success,
			[]func() (*http.Response, error){reply(502, ``), reply(502, ``)}, DispatchIndeterminate, 2},
		{"502 then 409 then 409", success,
			[]func() (*http.Response, error){reply(502, ``), reply(409, `{}`), reply(409, `{}`)}, DispatchIndeterminate, 3},
		{"502 then 409 then the record", success,
			[]func() (*http.Response, error){reply(502, ``), reply(409, `{}`), reply(200, successRecord)}, "", 3},
		{"504 then the record", success,
			[]func() (*http.Response, error){reply(504, ``), reply(200, successRecord)}, "", 2},
		{"504 then 504", failure,
			[]func() (*http.Response, error){reply(504, ``), reply(504, ``)}, DispatchIndeterminate, 2},
		{"504 then 404 then 404", failure,
			[]func() (*http.Response, error){reply(504, ``), reply(404, `{}`), reply(404, `{}`)}, DispatchIndeterminate, 3},
		{"lost then 502", success,
			[]func() (*http.Response, error){lost, reply(502, ``)}, DispatchIndeterminate, 2},
		{"500 then 504", failure,
			[]func() (*http.Response, error){reply(500, `{}`), reply(504, ``)}, DispatchIndeterminate, 2},
		{"plain 503 then 502", failure,
			[]func() (*http.Response, error){reply(503, `{}`), reply(502, ``)}, DispatchIndeterminate, 2},
	} {
		transport := &scriptedTransport{replies: row.replies}
		base, _ := url.Parse("https://explorer.invalid")
		client, err := NewDispatchClient(base, &http.Client{Transport: transport},
			func() (string, error) { return "token", nil }, nil)
		if err != nil {
			t.Fatal(err)
		}
		completion := row.completion
		completion.Context = RequestContext{CorrelationID: []byte("trace"), RequestID: []byte("r"), ReasonCode: "gateway_complete",
			Deadline: time.Now().Add(time.Minute)}
		action, err := client.Complete(context.Background(), []byte("action"), completion)
		if DispatchKind(err) != row.kind {
			t.Errorf("%s: %v (kind %q), want kind %q", row.name, err, DispatchKind(err), row.kind)
		}
		if len(transport.bodies) != row.calls {
			t.Errorf("%s: %d requests, want %d", row.name, len(transport.bodies), row.calls)
		}
		for i := 1; i < len(transport.bodies); i++ {
			if !bytes.Equal(transport.bodies[i], transport.bodies[0]) {
				t.Errorf("%s: the resend was not the identical body", row.name)
			}
		}
		if row.kind == DispatchRecordedOtherwise && action.State == "" {
			t.Errorf("%s: recorded_otherwise without the committed record", row.name)
		}
	}
}

// A server with #547 answers the completion of a reported failure, and of a
// success it records otherwise, with 200 and the committed record. The client
// takes that record from the one answer: exactly one completion request, no
// resend, no read. (The wire shape against the real handler is pinned in
// cmd/shoal-explore-web, TestEffectsGatewayClientRecordsAFailureInOneRequest.)
//
// The pre-#547 answer — 500 or 400 while the record committed — still
// converges through the resend: see "#492 workaround: 500 then the recorded
// failure" and "success recorded as failed" in
// TestCompleteRecoversOnlyWhatTheResendConfirms.
func TestCompleteTakesARecordedOutcomeFromOneRequest(t *testing.T) {
	for _, row := range []struct {
		name       string
		completion Completion
		record     string
		kind       DispatchErrorKind
		state      dispatchapi.DispatchState
		code       string
	}{
		{"reported failure", Completion{ExpectedVersion: 2, ClaimID: []byte("claim"), ClaimFence: 1, Failed: true,
			ErrorCode: TargetRejected(422)},
			committed(t, 3, dispatchapi.DispatchFailed, "target_rejected_422", ""),
			"", dispatchapi.DispatchFailed, "target_rejected_422"},
		{"success recorded otherwise", Completion{ExpectedVersion: 2, ClaimID: []byte("claim"), ClaimFence: 1,
			Output: json.RawMessage(`{"status":200}`)},
			committed(t, 3, dispatchapi.DispatchFailed, "invalid_executor_output", ""),
			DispatchRecordedOtherwise, dispatchapi.DispatchFailed, "invalid_executor_output"},
	} {
		transport := &scriptedTransport{replies: []func() (*http.Response, error){
			reply(200, row.record),
			// Anything past the first answer is a resend or a read, which
			// this test exists to rule out; these keep such a request from
			// failing for a different reason.
			reply(200, row.record), reply(200, row.record),
		}}
		base, _ := url.Parse("https://explorer.invalid")
		client, err := NewDispatchClient(base, &http.Client{Transport: transport},
			func() (string, error) { return "token", nil }, nil)
		if err != nil {
			t.Fatal(err)
		}
		completion := row.completion
		completion.Context = RequestContext{CorrelationID: []byte("trace"), RequestID: []byte("r"), ReasonCode: "gateway_complete",
			Deadline: time.Now().Add(time.Minute)}
		action, err := client.Complete(context.Background(), []byte("action"), completion)
		want := "POST /api/v1/fleet/actions/" + base64.RawURLEncoding.EncodeToString([]byte("action")) + "/complete"
		if len(transport.requests) != 1 || transport.requests[0] != want {
			t.Errorf("%s: requests %q, want exactly [%q]", row.name, transport.requests, want)
		}
		if DispatchKind(err) != row.kind {
			t.Errorf("%s: %v (kind %q), want kind %q", row.name, err, DispatchKind(err), row.kind)
		}
		if !bytes.Equal(action.ID, []byte("action")) || !bytes.Equal(action.ClaimID, []byte("claim")) ||
			action.Version != 3 || action.State != row.state || action.ErrorCode != row.code {
			t.Errorf("%s: returned %#v, want the committed record", row.name, action)
		}
	}
}

// Every answer after which a claim may have committed — any 503 (a bare one
// may hide ErrActionCommitted until #505), a proxy's 502 or 504, a transport
// error — is a re-pull signal, never a definite failure, and the caller still
// holds nothing: no Action, and nothing is resent. The original kind is kept
// as the cause.
func TestClaimPossiblyCommittedIsARepullSignal(t *testing.T) {
	for _, row := range []struct {
		name  string
		reply func() (*http.Response, error)
		kind  DispatchErrorKind
	}{
		{"plain 503", reply(503, `{"code":"unavailable"}`), DispatchRepull},
		{"503 with another outcome header", reply(503, `{}`, "Shoal-Commit-Outcome", "committed"), DispatchRepull},
		{"indeterminate 503", reply(503, `{}`, indeterminate...), DispatchRepull},
		{"502", reply(502, `<html>bad gateway</html>`), DispatchRepull},
		{"504", reply(504, ``), DispatchRepull},
		{"lost", lost, DispatchRepull},
		{"lost the race", reply(404, `{}`), DispatchNotFound},
		{"version moved", reply(409, `{}`), DispatchConflict},
		{"500", reply(500, `{}`), DispatchStatus},
	} {
		transport := &scriptedTransport{replies: []func() (*http.Response, error){row.reply}}
		base, _ := url.Parse("https://explorer.invalid")
		client, err := NewDispatchClient(base, &http.Client{Transport: transport},
			func() (string, error) { return "token", nil }, nil)
		if err != nil {
			t.Fatal(err)
		}
		action, err := client.Claim(context.Background(), []byte("action"), ClaimRequest{
			Context: RequestContext{CorrelationID: []byte("trace"), RequestID: []byte("r"), ReasonCode: "gateway_claim",
				Deadline: time.Now().Add(time.Minute)},
			ExpectedVersion: 1, ClaimID: []byte("claim"), Lease: time.Minute,
		})
		if DispatchKind(err) != row.kind {
			t.Errorf("%s: %v (kind %q), want kind %q", row.name, err, DispatchKind(err), row.kind)
		}
		if action.ID != nil || action.State != "" || action.ClaimID != nil {
			t.Errorf("%s: a failed claim returned an action", row.name)
		}
		if len(transport.bodies) != 1 {
			t.Errorf("%s: %d requests, want 1", row.name, len(transport.bodies))
		}
		var dispatchErr *DispatchError
		if row.kind == DispatchRepull {
			if !errors.As(err, &dispatchErr) || dispatchErr.Op != "claim" {
				t.Fatalf("%s: %#v", row.name, err)
			}
			original := errors.Unwrap(err)
			var cause *DispatchError
			found := errors.As(original, &cause)
			lostKind := found && (cause.Kind == DispatchTransport ||
				cause.Kind == DispatchIndeterminate || cause.Kind == DispatchUnavailable ||
				cause.Status == http.StatusBadGateway || cause.Status == http.StatusGatewayTimeout)
			if !found || cause.Kind == DispatchRepull ||
				!lostKind || cause.Status != dispatchErr.Status {
				t.Errorf("%s: repull error lost its original kind: %#v", row.name, original)
			}
		}
	}
}
