// Licensed to the Apache Software Foundation (ASF) under one or more
// contributor license agreements. See the NOTICE file distributed with this
// work for additional information regarding copyright ownership.
package explorerfleet

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/phrocker/shoal-oss/pkg/explorer/auth"
	"github.com/phrocker/shoal-oss/pkg/explorer/fleet"
	"github.com/phrocker/shoal-oss/pkg/explorer/webapi"
)

// TestADurablyRecordedFailureAnswersTheWorkerWithItsRecord is #492, driven
// through the real durable store *and* the real HTTP surface because the
// transport is what discarded the fix.
//
// The service already returned the committed record alongside the execution
// error. The /complete handler then did what any handler does with a non-nil
// error — wrote the error and dropped the record — so a worker that reported a
// failure got a 500, and one whose output failed the declared schema got a
// 400. In both the action was terminal and the write had landed, and in both
// the worker was told it had been refused. It could not tell "committed as
// failed" from "your report was rejected, nothing happened", which is the one
// ambiguity the whole dispatch design exists to remove — and it is worst for a
// gateway worker that has just performed an irreversible external effect.
//
// A service-level test cannot catch a regression here, because the service was
// already correct; the discarding happened one layer up. So this drives the
// bytes: a real worker's JSON in, the HTTP status and response body out.
func TestADurablyRecordedFailureAnswersTheWorkerWithItsRecord(t *testing.T) {
	for _, probe := range []struct {
		name string
		// body is the worker's report, as JSON on the wire.
		report func(version, fence uint64, now time.Time) string
		// wantCode is what the record must carry afterwards.
		wantCode   string
		wantOrigin string
	}{
		{
			// The worker says the work failed. Shoal recorded that, so the
			// reporting operation succeeded. The error the service used to
			// propagate was one it synthesised itself to represent the
			// worker's own report.
			name: "a reported failure",
			report: func(version, fence uint64, now time.Time) string {
				return completionBody(version, fence, now,
					`"failed":true,"error_code":"gateway_refused"`)
			},
			wantCode:   "gateway_refused",
			wantOrigin: "executor",
		},
		{
			// A success whose output the declared OutputSchema refuses. The
			// service adjudicates, commits the record as failed with its own
			// code, and the worker reads that code off the record rather than
			// off a 400 that also means its request never happened.
			name: "an output the schema refuses",
			report: func(version, fence uint64, now time.Time) string {
				return completionBody(version, fence, now,
					`"output":"not an object"`)
			},
			wantCode:   "invalid_executor_output",
			wantOrigin: "service",
		},
	} {
		t.Run(probe.name, func(t *testing.T) {
			now := time.Date(2026, 9, 6, 12, 0, 0, 0, time.UTC)
			runtime := openFleetDispatchRuntime(t, t.TempDir())
			defer func() { _ = runtime.Close() }()
			authority, err := auth.NewAuthorityWithClock(
				func() time.Time { return now })
			if err != nil {
				t.Fatal(err)
			}
			registry, dispatch := composeIntegratedServices(
				t, runtime, authority, &integratedExecutor{}, now)
			decision := integratedDecision(t, now)
			ctx, err := authority.Binder().Bind(context.Background(), decision)
			if err != nil {
				t.Fatal(err)
			}
			descriptor := registerIntegratedAgent(t, registry, ctx, now)
			queued, err := dispatch.Enqueue(ctx, integratedEnqueue(
				now, descriptor, []byte("http-action"), []byte("http-key")))
			if err != nil {
				t.Fatal(err)
			}
			claimed, err := dispatch.Claim(ctx, fleet.ClaimRequest{
				ID: queued.ID, ExpectedVersion: queued.Version,
				ClaimID: []byte("claim"), Lease: time.Minute,
				Context: integratedContext(now),
			})
			if err != nil {
				t.Fatal(err)
			}

			// The real handler over the real service. Mounted without an
			// authentication boundary and served a request carrying the bound
			// decision, which is what MountAuthenticated arranges in the
			// hosted path — the point here is the transport, not the authn.
			handler, err := webapi.NewFleetDispatchHandler(dispatch)
			if err != nil {
				t.Fatal(err)
			}
			route := "http://fleet.test/api/v1/fleet/actions/" +
				base64.RawURLEncoding.EncodeToString(queued.ID) + "/complete"
			body := probe.report(claimed.Version, claimed.ClaimFence, now)
			request := httptest.NewRequest(
				http.MethodPost, route, bytes.NewReader([]byte(body)))
			request.Header.Set("Content-Type", "application/json")
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request.WithContext(ctx))

			if response.Code != http.StatusOK {
				t.Fatalf("a committed %s was answered %d, so the worker "+
					"cannot tell it from a rejected report: %s",
					probe.wantCode, response.Code, response.Body.String())
			}

			// The body has to carry the outcome, or a 200 is worse than the
			// error it replaced: the worker would learn nothing at all.
			var wire struct {
				State           string `json:"state"`
				ErrorCode       string `json:"error_code"`
				ErrorCodeOrigin string `json:"error_code_origin"`
				Version         uint64 `json:"version"`
			}
			if err := json.Unmarshal(response.Body.Bytes(), &wire); err != nil {
				t.Fatalf("response is not a record: %v (%s)",
					err, response.Body.String())
			}
			if wire.State != string(fleet.DispatchFailed) {
				t.Fatalf("response state = %q, want %q", wire.State,
					fleet.DispatchFailed)
			}
			if wire.ErrorCode != probe.wantCode {
				t.Fatalf("response error_code = %q, want %q",
					wire.ErrorCode, probe.wantCode)
			}
			// Which of the two decided the code is the thing a 200 must not
			// blur: the worker's own reason and the service's adjudication
			// read identically without it (#508).
			if wire.ErrorCodeOrigin != probe.wantOrigin {
				t.Fatalf("response error_code_origin = %q, want %q",
					wire.ErrorCodeOrigin, probe.wantOrigin)
			}

			// And it must be the record that actually landed, read back from
			// the durable store rather than from the response.
			stored, err := dispatch.Status(ctx, fleet.StatusRequest{
				ID: queued.ID, Context: integratedContext(now),
			})
			if err != nil {
				t.Fatal(err)
			}
			if stored.State != fleet.DispatchFailed ||
				stored.ErrorCode != probe.wantCode ||
				stored.Version != wire.Version {
				t.Fatalf("the response does not match the durable record: "+
					"%q/%q/v%d stored vs %q/%q/v%d answered",
					stored.State, stored.ErrorCode, stored.Version,
					wire.State, wire.ErrorCode, wire.Version)
			}
		})
	}
}

// completionBody writes a worker's /complete request the way a worker does,
// as bytes rather than through the unexported wire struct, so a rename or a
// changed JSON tag shows up here as a refusal instead of silently passing.
func completionBody(version, fence uint64, now time.Time, extra string) string {
	return fmt.Sprintf(
		`{"context":{"request_id":%q,"correlation_id":%q,`+
			`"reason_code":"operator_request","deadline":%q},`+
			`"expected_version":%d,"claim_fence":%d,"claim_id":%q,%s}`,
		base64.RawURLEncoding.EncodeToString([]byte("request")),
		base64.RawURLEncoding.EncodeToString([]byte("correlation")),
		now.Add(time.Hour).Format(time.RFC3339Nano),
		version, fence,
		base64.RawURLEncoding.EncodeToString([]byte("claim")),
		extra)
}
