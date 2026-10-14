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
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"strings"
	"syscall"
	"testing"
	"testing/iotest"
	"time"
)

const classifyTable = `[
 {"action":"charge","method":"POST","path":"/v1/charges",
  "effects":["external","egresses-content"],"idempotency":"key",
  "conflict":{"status":[400],"pointer":"/error/type","equals":["idempotency_error"]},
  "retryable":[429,503],
  "reference":{"pointer":"/id","pattern":"ch_[a-z0-9]+"}},
 {"action":"lock","method":"POST","path":"/v1/locks",
  "effects":["external","egresses-content"],"idempotency":"key",
  "conflict":{"status":[409],"pointer":"/error/code","equals":["already_locked"]},
  "retryable":[409,423],
  "reference":{"header":"location","pattern":"/v1/locks/[0-9]+"}},
 {"action":"remove","method":"DELETE","path":"/v1/items/{id}",
  "effects":["external","egresses-content"],"idempotency":"natural",
  "conflict":{"status":[404],"pointer":"/error","equals":["not_found"]},"retryable":[502]},
 {"action":"notify","method":"POST","path":"/v1/notify",
  "effects":["external","egresses-content"],"idempotency":"unprotected",
  "retryable":[503],
  "reference":{"pointer":"/id","pattern":"n_[0-9]+"}}
]`

func respond(status int, body string, header ...string) Observation {
	h := http.Header{}
	for i := 0; i+1 < len(header); i += 2 {
		h.Add(header[i], header[i+1])
	}
	return Observation{Written: true, Response: &Response{Status: status, Header: h, Body: []byte(body)}}
}

func oversize(status int, body string) Observation {
	observation := respond(status, body)
	observation.Response.Oversize = true
	return observation
}

func bodyFailed(status int) Observation {
	observation := respond(status, "")
	observation.Response.BodyErr = io.ErrUnexpectedEOF
	return observation
}

var (
	dialFailure  = &net.OpError{Op: "dial", Net: "tcp", Err: syscall.ECONNREFUSED}
	resetFailure = &net.OpError{Op: "read", Net: "tcp", Err: syscall.ECONNRESET}
)

// TestClassifyTable is the classifier's whole table, row by row and mode by
// mode. The rows whose wrong answer is a double effect or a false failure are
// marked; a change that flips one of them is a change to the safety argument,
// not a refactor.
func TestClassifyTable(t *testing.T) {
	table := mustParseRoutes(t, classifyTable)
	type want struct {
		outcome     Outcome
		kind        Kind
		code        string
		idempotency string
		reference   string
		retryAfter  time.Duration
	}
	for _, row := range []struct {
		name        string
		action      string
		observation Observation
		want        want
	}{
		// Never-sent: retried in every mode, because nothing left.
		{"key: dial failure before write", "charge",
			Observation{Err: dialFailure}, want{outcome: OutcomeRetry, kind: KindNotSent}},
		{"natural: dial failure before write", "remove",
			Observation{Err: dialFailure}, want{outcome: OutcomeRetry, kind: KindNotSent}},
		{"unprotected: dial failure before write", "notify",
			Observation{Err: dialFailure}, want{outcome: OutcomeRetry, kind: KindNotSent}},
		{"unprotected: egress refused before write", "notify",
			Observation{Err: ErrEgressRefused}, want{outcome: OutcomeRetry, kind: KindNotSent}},

		// Written, then silence. DOUBLE EFFECT if the unprotected row retried.
		{"key: written then reset", "charge",
			Observation{Written: true, Err: resetFailure},
			want{outcome: OutcomeRetry, kind: KindWrittenNoResponse}},
		{"natural: written then timeout", "remove",
			Observation{Written: true, Err: errTimeout{}},
			want{outcome: OutcomeRetry, kind: KindWrittenNoResponse}},
		{"unprotected: written then reset", "notify",
			Observation{Written: true, Err: resetFailure},
			want{outcome: OutcomeFailed, kind: KindWrittenNoResponse, code: ErrorOutcomeUnknown}},
		{"unprotected: written then timeout", "notify",
			Observation{Written: true, Err: errTimeout{}},
			want{outcome: OutcomeFailed, kind: KindWrittenNoResponse, code: ErrorOutcomeUnknown}},

		// 2xx.
		{"key: 200 with reference", "charge", respond(200, `{"id":"ch_42"}`),
			want{outcome: OutcomeSucceeded, kind: KindSuccess, idempotency: "key", reference: "ch_42"}},
		{"key: 201 replayed", "charge", respond(201, `{"id":"ch_42"}`, "Idempotent-Replayed", "true"),
			want{outcome: OutcomeSucceeded, kind: KindReplayed, idempotency: "replayed", reference: "ch_42"}},
		{"key: replayed header any case", "charge", respond(200, `{}`, "Idempotent-Replayed", " TRUE "),
			want{outcome: OutcomeSucceeded, kind: KindReplayed, idempotency: "replayed"}},
		{"key: replayed false", "charge", respond(200, `{}`, "Idempotent-Replayed", "false"),
			want{outcome: OutcomeSucceeded, kind: KindSuccess, idempotency: "key"}},
		{"natural: 204", "remove", respond(204, ``),
			want{outcome: OutcomeSucceeded, kind: KindSuccess, idempotency: "natural"}},
		{"unprotected: 200", "notify", respond(200, `{"id":"n_7"}`),
			want{outcome: OutcomeSucceeded, kind: KindSuccess, idempotency: "unprotected", reference: "n_7"}},
		{"unprotected: replay marker ignored", "notify", respond(200, `{}`, "Idempotent-Replayed", "true"),
			want{outcome: OutcomeSucceeded, kind: KindSuccess, idempotency: "unprotected"}},

		// FALSE SUCCESS if a replayed non-2xx counted: a replayed decline.
		{"key: replayed 402 is the original decline", "charge",
			respond(402, `{"error":{"type":"card_error"}}`, "Idempotent-Replayed", "true"),
			want{outcome: OutcomeFailed, kind: KindRejected, code: "target_rejected_402"}},

		// Conflict-is-success. FALSE FAILURE (and a second effect on the
		// operator's retry) if these read as failures.
		{"key: idempotency conflict by pointer", "charge",
			respond(400, `{"error":{"type":"idempotency_error","message":"keys reused"}}`),
			want{outcome: OutcomeSucceeded, kind: KindConflict, idempotency: "conflict"}},
		{"key: conflict on a second rule", "lock", respond(409, `{"error":{"code":"already_locked"}}`),
			want{outcome: OutcomeSucceeded, kind: KindConflict, idempotency: "conflict"}},
		// One status for "already done" and "still in flight". FALSE SUCCESS
		// if the in-flight answer counted as the conflict; FALSE FAILURE if it
		// were target_rejected while the original may still succeed. Listed
		// retryable too, it retries under the same key.
		{"key: same status, in-flight marker, retryable", "lock",
			respond(409, `{"error":{"code":"idempotency_key_in_use"}}`, "Retry-After", "2"),
			want{outcome: OutcomeRetry, kind: KindRetryableStatus, retryAfter: 2 * time.Second}},
		{"key: same status, no body, retryable", "lock", respond(409, ``),
			want{outcome: OutcomeRetry, kind: KindRetryableStatus}},
		{"key: same status, body oversize, retryable", "lock", oversize(409, `{"error":`),
			want{outcome: OutcomeRetry, kind: KindRetryableStatus}},
		{"key: retryable status outside the conflict rule", "lock", respond(423, ``),
			want{outcome: OutcomeRetry, kind: KindRetryableStatus}},
		{"natural: DELETE of what is already gone", "remove", respond(404, `{"error":"not_found"}`),
			want{outcome: OutcomeSucceeded, kind: KindConflict, idempotency: "conflict"}},
		// Not listed retryable: unknown, never target_rejected.
		{"natural: 404 without the marker", "remove", respond(404, ``),
			want{outcome: OutcomeFailed, kind: KindConflictUnmatched, code: ErrorOutcomeUnknown}},
		// A conflict status without the marker, not listed retryable: the
		// outcome is unknown, never a definite rejection.
		{"key: conflict status, other error type", "charge",
			respond(400, `{"error":{"type":"invalid_request_error"}}`), want{outcome: OutcomeFailed, kind: KindConflictUnmatched, code: ErrorOutcomeUnknown}},
		{"key: conflict status, pointer absent", "charge", respond(400, `{"error":{}}`), want{outcome: OutcomeFailed, kind: KindConflictUnmatched, code: ErrorOutcomeUnknown}},
		{"key: conflict status, pointer not a string", "charge",
			respond(400, `{"error":{"type":["idempotency_error"]}}`), want{outcome: OutcomeFailed, kind: KindConflictUnmatched, code: ErrorOutcomeUnknown}},
		{"key: conflict status, body not JSON", "charge", respond(400, `<html>`), want{outcome: OutcomeFailed, kind: KindConflictUnmatched, code: ErrorOutcomeUnknown}},
		{"key: conflict status, body has trailing data", "charge",
			respond(400, `{"error":{"type":"idempotency_error"}} {}`), want{outcome: OutcomeFailed, kind: KindConflictUnmatched, code: ErrorOutcomeUnknown}},
		{"key: status outside the conflict rule is still rejected", "charge", respond(402, `{}`),
			want{outcome: OutcomeFailed, kind: KindRejected, code: "target_rejected_402"}},
		// Unverifiable: neither guess is safe, so unknown.
		{"key: conflict status, body oversize", "charge",
			oversize(400, `{"error":{"type":"idempotency_error"`),
			want{outcome: OutcomeFailed, kind: KindConflictUnverified, code: ErrorOutcomeUnknown}},
		{"key: conflict status, body unreadable", "charge", bodyFailed(400),
			want{outcome: OutcomeFailed, kind: KindConflictUnverified, code: ErrorOutcomeUnknown}},
		{"unprotected: 409 is never a conflict", "notify", respond(409, ``),
			want{outcome: OutcomeFailed, kind: KindRejected, code: "target_rejected_409"}},

		// Retryable statuses.
		{"key: 429 with Retry-After", "charge", respond(429, ``, "Retry-After", "7"),
			want{outcome: OutcomeRetry, kind: KindRetryableStatus, retryAfter: 7 * time.Second}},
		{"key: 503 with an HTTP-date Retry-After", "charge",
			respond(503, ``, "Retry-After", "Wed, 21 Oct 2015 07:28:00 GMT"),
			want{outcome: OutcomeRetry, kind: KindRetryableStatus}},
		{"natural: 502", "remove", respond(502, ``),
			want{outcome: OutcomeRetry, kind: KindRetryableStatus}},
		// DOUBLE EFFECT if retried: a 503 does not prove nothing happened.
		{"unprotected: retryable status is unknown", "notify", respond(503, ``),
			want{outcome: OutcomeFailed, kind: KindRetryableStatus, code: ErrorOutcomeUnknown}},

		// Redirects are never followed and never success.
		{"key: 303", "charge", respond(303, ``, "Location", "/v1/charges/ch_1"),
			want{outcome: OutcomeFailed, kind: KindRedirect, code: ErrorOutcomeUnknown}},
		{"unprotected: 307", "notify", respond(307, ``),
			want{outcome: OutcomeFailed, kind: KindRedirect, code: ErrorOutcomeUnknown}},

		// Everything else is the target's final word.
		{"key: 500 not configured retryable", "charge", respond(500, ``),
			want{outcome: OutcomeFailed, kind: KindRejected, code: "target_rejected_500"}},
		{"natural: 503 not configured retryable", "remove", respond(503, ``),
			want{outcome: OutcomeFailed, kind: KindRejected, code: "target_rejected_503"}},
		{"key: 422", "charge", respond(422, `{"error":"bad"}`),
			want{outcome: OutcomeFailed, kind: KindRejected, code: "target_rejected_422"}},
		{"unprotected: 401", "notify", respond(401, ``),
			want{outcome: OutcomeFailed, kind: KindRejected, code: "target_rejected_401"}},

		// Out of range.
		{"key: 1xx", "charge", respond(102, ``),
			want{outcome: OutcomeFailed, kind: KindInvalid, code: ErrorOutcomeUnknown}},
		{"key: status 600", "charge", respond(600, ``),
			want{outcome: OutcomeFailed, kind: KindInvalid, code: ErrorOutcomeUnknown}},
		{"key: no error and no response", "charge", Observation{Written: true},
			want{outcome: OutcomeFailed, kind: KindInvalid, code: ErrorOutcomeUnknown}},
		{"key: both an error and a response", "charge",
			Observation{Written: true, Err: resetFailure, Response: &Response{Status: 200}},
			want{outcome: OutcomeFailed, kind: KindInvalid, code: ErrorOutcomeUnknown}},
	} {
		got := Classify(mustRoute(t, table, row.action), row.observation)
		if got.Outcome != row.want.outcome || got.Kind != row.want.kind ||
			got.ErrorCode != row.want.code || got.RetryAfter != row.want.retryAfter {
			t.Errorf("%s: got %s/%s/%q retry-after %s, want %s/%s/%q retry-after %s",
				row.name, got.Outcome, got.Kind, got.ErrorCode, got.RetryAfter,
				row.want.outcome, row.want.kind, row.want.code, row.want.retryAfter)
			continue
		}
		if got.ErrorCode != "" && !ValidErrorCode(got.ErrorCode) {
			t.Errorf("%s: produced an off-vocabulary code %q", row.name, got.ErrorCode)
		}
		if got.Outcome != OutcomeSucceeded {
			if got.Output != nil {
				t.Errorf("%s: a non-success carries output", row.name)
			}
			continue
		}
		var document map[string]any
		if err := json.Unmarshal(got.Output, &document); err != nil {
			t.Errorf("%s: output is not JSON: %v", row.name, err)
			continue
		}
		if document["idempotency"] != row.want.idempotency {
			t.Errorf("%s: idempotency = %v, want %q", row.name, document["idempotency"], row.want.idempotency)
		}
		reference, _ := document["reference"].(string)
		if reference != row.want.reference {
			t.Errorf("%s: reference = %q, want %q", row.name, reference, row.want.reference)
		}
		if status, _ := document["status"].(float64); int(status) != row.observation.Response.Status {
			t.Errorf("%s: status = %v", row.name, document["status"])
		}
		for key := range document {
			switch key {
			case "status", "idempotency", "reference":
			default:
				t.Errorf("%s: output carries %q outside the closed shape", row.name, key)
			}
		}
	}
}

type errTimeout struct{}

func (errTimeout) Error() string   { return "i/o timeout" }
func (errTimeout) Timeout() bool   { return true }
func (errTimeout) Temporary() bool { return true }

// TestClassifyNeverCopiesTargetTextIntoTheRecord: a hostile target controls
// the body and every header. Only a pattern-matched, bounded reference may
// reach Output, and nothing reaches ErrorCode.
func TestClassifyNeverCopiesTargetTextIntoTheRecord(t *testing.T) {
	table := mustParseRoutes(t, classifyTable)
	charge := mustRoute(t, table, "charge")
	lock := mustRoute(t, table, "lock")
	long := "ch_" + strings.Repeat("a", MaxReferenceBytes)
	for _, row := range []struct {
		name        string
		route       *Route
		observation Observation
	}{
		{"pattern mismatch", charge, respond(200, `{"id":"<script>alert(1)</script>"}`)},
		{"partial pattern match", charge, respond(200, `{"id":"ch_1 and more"}`)},
		{"longer than the bound", charge, respond(200, `{"id":"`+long+`"}`)},
		{"non-string", charge, respond(200, `{"id":12345}`)},
		{"oversize body", charge, oversize(200, `{"id":"ch_1"}`)},
		{"unreadable body", charge, bodyFailed(200)},
		{"two header values", lock, respond(200, ``, "Location", "/v1/locks/1", "Location", "/v1/locks/2")},
		{"header mismatch", lock, respond(200, ``, "Location", "https://evil.example/v1/locks/1")},
	} {
		got := Classify(row.route, row.observation)
		if got.Outcome != OutcomeSucceeded {
			t.Errorf("%s: an unusable reference turned a success into %s", row.name, got.Outcome)
			continue
		}
		var document map[string]any
		_ = json.Unmarshal(got.Output, &document)
		if _, present := document["reference"]; present {
			t.Errorf("%s: reference recorded: %v", row.name, document["reference"])
		}
	}
	got := Classify(lock, respond(201, ``, "Location", "/v1/locks/77"))
	if !strings.Contains(string(got.Output), `"reference":"/v1/locks/77"`) {
		t.Fatalf("a matching header reference was not recorded: %s", got.Output)
	}
	exact := "ch_" + strings.Repeat("a", MaxReferenceBytes-3)
	got = Classify(charge, respond(200, `{"id":"`+exact+`"}`))
	if !strings.Contains(string(got.Output), exact) {
		t.Fatal("a reference of exactly the bound was dropped")
	}
	rejected := Classify(charge, respond(418, `{"error":"target_rejected_200; DROP TABLE"}`))
	if rejected.ErrorCode != "target_rejected_418" {
		t.Fatalf("error code = %q", rejected.ErrorCode)
	}
}

func TestExhausted(t *testing.T) {
	if got := Exhausted(false); got.Outcome != OutcomeFailed || got.ErrorCode != ErrorRequestNotSent {
		t.Fatalf("exhausted with nothing written = %#v; nothing reached the target and the record must say so", got)
	}
	if got := Exhausted(true); got.Outcome != OutcomeFailed || got.ErrorCode != ErrorRetryExhausted {
		t.Fatalf("exhausted after a write = %#v", got)
	}
}

func TestValidErrorCode(t *testing.T) {
	for code, want := range map[string]bool{
		ErrorRequestNotSent: true, ErrorOutcomeUnknown: true, ErrorRetryExhausted: true,
		ErrorInputInvalid: true, "target_rejected_400": true, "target_rejected_599": true,
		"target_rejected_399": false, "target_rejected_600": false, "target_rejected_40": false,
		"target_rejected_4000": false, "target_rejected_+40": false, "target_rejected_04a": false,
		"": false, "executor_error": false, "Outcome_Unknown": false,
		"target_rejected_400 ": false, "request_not_sent\n": false,
	} {
		if got := ValidErrorCode(code); got != want {
			t.Errorf("ValidErrorCode(%q) = %v, want %v", code, got, want)
		}
	}
	for status := 400; status <= 599; status++ {
		if !ValidErrorCode(TargetRejected(status)) {
			t.Fatalf("TargetRejected(%d) is outside the vocabulary", status)
		}
	}
}

func TestRetryAfterIsBounded(t *testing.T) {
	for raw, want := range map[string]time.Duration{
		"0": 0, "120": 2 * time.Minute, "999999999999": maxRetryAfter,
		"99999999999999999999999": maxRetryAfter,
	} {
		got, ok := retryAfter(http.Header{"Retry-After": {raw}})
		if !ok || got != want {
			t.Errorf("Retry-After %q = %s, %v; want %s", raw, got, ok, want)
		}
	}
	for _, raw := range []string{"", "-1", "1.5", "soon", "Wed, 21 Oct 2015 07:28:00 GMT"} {
		if _, ok := retryAfter(http.Header{"Retry-After": {raw}}); ok {
			t.Errorf("Retry-After %q was read as a delay", raw)
		}
	}
}

func TestReadBoundedNeverReadsPastTheLimit(t *testing.T) {
	counter := &countingReader{reader: strings.NewReader(strings.Repeat("x", 1<<20))}
	data, over, err := ReadBounded(counter, 1024)
	if err != nil || !over || len(data) != 1024 {
		t.Fatalf("oversize read = %d bytes, oversize %v, %v", len(data), over, err)
	}
	if counter.read > 1025 {
		t.Fatalf("read %d bytes from the target for a 1024-byte bound", counter.read)
	}
	data, over, err = ReadBounded(strings.NewReader("exact"), 5)
	if err != nil || over || string(data) != "exact" {
		t.Fatalf("exact read = %q, %v, %v", data, over, err)
	}
	_, _, err = ReadBounded(iotest.ErrReader(io.ErrUnexpectedEOF), 5)
	if !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("read error = %v", err)
	}
	if _, _, err := ReadBounded(bytes.NewReader(nil), 0); err == nil {
		t.Fatal("a zero limit was accepted")
	}
}

type countingReader struct {
	reader io.Reader
	read   int
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.reader.Read(p)
	c.read += n
	return n, err
}
