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
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"reflect"
	"strings"
	"syscall"
	"testing"
	"time"

	dispatchapi "github.com/phrocker/shoal-oss/pkg/dispatch/api"
)

// TestLoggingNeverCarriesAFilledURL is the url.Error case the policy exists
// for. The target client's error for a request to a path built from input
// embeds that path; the logger is handed only what the error classifies to.
func TestLoggingNeverCarriesAFilledURL(t *testing.T) {
	const secret = "acct-9f8e7d-SECRET"
	table := mustParseRoutes(t, bindTable)
	binder := mustBinder(t, "https://api.example.com")
	charge := mustRoute(t, table, "charge")
	bound, err := binder.Bind(charge,
		json.RawMessage(`{"path":{"account":"`+secret+`"},"query":{"mode":"`+secret+`"},"body":{"pin":"`+secret+`"}}`),
		testKey(t))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(bound.URL, secret) {
		t.Fatal("fixture: the secret is not in the URL, so the test proves nothing")
	}

	// What a real client returns for this request: url.Error carries the URL.
	failures := []error{
		&url.Error{Op: "Post", URL: bound.URL, Err: context.DeadlineExceeded},
		&url.Error{Op: "Post", URL: bound.URL, Err: &net.OpError{Op: "dial", Net: "tcp", Err: syscall.ECONNREFUSED}},
		&url.Error{Op: "Post", URL: bound.URL, Err: &net.DNSError{Name: "api.example.com", Err: secret}},
		&url.Error{Op: "Post", URL: bound.URL, Err: fmt.Errorf("%w: %s", ErrEgressRefused, secret)},
		&url.Error{Op: "Post", URL: bound.URL, Err: errors.New("unclassifiable " + secret)},
	}
	for _, failure := range failures {
		if !strings.Contains(failure.Error(), secret) {
			t.Fatal("fixture: url.Error no longer embeds the URL; revisit the policy comment")
		}
		var out bytes.Buffer
		logger := NewLogger(&out, func() time.Time { return serverEpoch })
		logger.Log(LogRecord{
			Event: EventClassify, ActionID: []byte("action"), Fence: 3,
			ClaimNonce: ClaimNonce{1, 2, 3}, Route: charge, Status: 0,
			Classification: KindWrittenNoResponse, Failure: ClassifyFailure(failure),
			RequestBytes: int64(len(bound.Body)), Duration: 1500 * time.Millisecond,
		})
		line := out.String()
		if strings.Contains(line, secret) {
			t.Fatalf("a log line carried input: %s", line)
		}
		if !strings.Contains(line, `"path_template":"/v1/accounts/{account}/charges"`) {
			t.Fatalf("the path template is missing: %s", line)
		}
	}
}

func TestClassifyFailure(t *testing.T) {
	for _, row := range []struct {
		err  error
		want FailureKind
	}{
		{nil, FailureNone},
		{fmt.Errorf("x: %w", ErrEgressRefused), FailureEgressRefused},
		{&url.Error{Op: "Post", URL: "u", Err: context.Canceled}, FailureCanceled},
		{&url.Error{Op: "Post", URL: "u", Err: context.DeadlineExceeded}, FailureTimeout},
		{&net.DNSError{Name: "x", Err: "no such host"}, FailureDNS},
		{&net.OpError{Op: "dial", Err: syscall.ECONNREFUSED}, FailureRefused},
		{&net.OpError{Op: "read", Err: syscall.ECONNRESET}, FailureReset},
		{&url.Error{Op: "Post", URL: "u", Err: io.EOF}, FailureReset},
		{&net.OpError{Op: "dial", Err: errors.New("no route")}, FailureDial},
		{&net.OpError{Op: "read", Err: errTimeout{}}, FailureTimeout},
		{tls.RecordHeaderError{Msg: "not tls"}, FailureTLS},
		{&tls.CertificateVerificationError{}, FailureTLS},
		{errors.New("anything else"), FailureOther},
	} {
		if got := ClassifyFailure(row.err); got != row.want {
			t.Errorf("%v: %q, want %q", row.err, got, row.want)
		}
	}
}

// TestLogRecordAdmitsOnlyThePolicyFields fails if a field is added to
// LogRecord that could carry free text. Adding one is a change to the logging
// policy and should be argued for there, not slipped in here.
func TestLogRecordAdmitsOnlyThePolicyFields(t *testing.T) {
	allowed := map[string]reflect.Type{
		"Event":          reflect.TypeOf(Event("")),
		"ActionID":       reflect.TypeOf([]byte(nil)),
		"Fence":          reflect.TypeOf(uint64(0)),
		"ClaimNonce":     reflect.TypeOf(ClaimNonce{}),
		"Route":          reflect.TypeOf((*Route)(nil)),
		"Status":         reflect.TypeOf(0),
		"Classification": reflect.TypeOf(Kind("")),
		"Failure":        reflect.TypeOf(FailureKind("")),
		"Gate":           reflect.TypeOf(GateRefusal("")),
		"DispatchError":  reflect.TypeOf(DispatchErrorKind("")),
		// Closed sets and a count, added with the worker loop.
		"Ambiguity":     reflect.TypeOf(dispatchapi.AmbiguityOutcome("")),
		"Unrecorded":    reflect.TypeOf(0),
		"NotReady":      reflect.TypeOf(NotReadyReason("")),
		"Abandoned":     reflect.TypeOf(0),
		"RequestBytes":  reflect.TypeOf(int64(0)),
		"ResponseBytes": reflect.TypeOf(int64(0)),
		"Duration":      reflect.TypeOf(time.Duration(0)),
	}
	record := reflect.TypeOf(LogRecord{})
	if record.NumField() != len(allowed) {
		t.Fatalf("LogRecord has %d fields, the policy allows %d", record.NumField(), len(allowed))
	}
	for i := 0; i < record.NumField(); i++ {
		field := record.Field(i)
		want, ok := allowed[field.Name]
		if !ok || field.Type != want {
			t.Fatalf("LogRecord.%s %s is outside the logging policy", field.Name, field.Type)
		}
	}
}

func TestLoggerLaundersNoTextThroughAConversion(t *testing.T) {
	var out bytes.Buffer
	logger := NewLogger(&out, func() time.Time { return serverEpoch })
	const smuggled = "Bearer s3cret"
	logger.Log(LogRecord{
		Event: Event(smuggled), Classification: Kind(smuggled),
		Failure: FailureKind(smuggled), Gate: GateRefusal(smuggled),
		DispatchError: DispatchErrorKind(smuggled),
	})
	line := out.String()
	if strings.Contains(line, "s3cret") {
		t.Fatalf("a converted string reached the log: %s", line)
	}
	var decoded map[string]any
	if err := json.Unmarshal([]byte(line), &decoded); err != nil {
		t.Fatalf("log line is not JSON: %v", err)
	}
	for _, key := range []string{"event", "classification", "failure", "gate", "dispatch_error"} {
		if decoded[key] != "invalid" {
			t.Errorf("%s = %v, want invalid", key, decoded[key])
		}
	}
}

func TestLoggerWritesTheAllowedFields(t *testing.T) {
	var out bytes.Buffer
	logger := NewLogger(&out, func() time.Time { return serverEpoch })
	table := mustParseRoutes(t, bindTable)
	logger.Log(LogRecord{
		Event: EventCompleted, ActionID: []byte{0xff, 0x00, 'a'}, Fence: 2,
		ClaimNonce: ClaimNonce{0xab}, Route: mustRoute(t, table, "replace"),
		Status: http.StatusOK, Classification: KindSuccess,
		Gate: GateLease, DispatchError: DispatchConflict,
		RequestBytes: 10, ResponseBytes: 20, Duration: 2 * time.Second,
	})
	var decoded map[string]any
	if err := json.Unmarshal(out.Bytes(), &decoded); err != nil {
		t.Fatal(err)
	}
	want := map[string]any{
		"time": "2026-10-08T12:00:00Z", "event": "completed", "action_id": "_wBh",
		"fence": float64(2), "claim_nonce": "ab000000000000000000000000000000",
		"route_action": "replace", "method": "PUT", "path_template": "/v1/items/{id}",
		"status": float64(200), "classification": "success", "gate": "lease",
		"dispatch_error": "conflict", "request_bytes": float64(10),
		"response_bytes": float64(20), "duration_ms": float64(2000),
	}
	if !reflect.DeepEqual(decoded, want) {
		t.Fatalf("log line\n got %v\nwant %v", decoded, want)
	}
}
