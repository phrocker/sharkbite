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
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	dispatchapi "github.com/phrocker/shoal-oss/pkg/dispatch/api"
	"github.com/phrocker/shoal-oss/pkg/interaction"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

// The sequences of answers below are what the real handler cannot be made to
// produce on demand. Every wire shape, and the behaviour against the real
// explorer, is pinned in cmd/shoal-explore-web
// (effects_gateway_ops_test.go).

// headerTransport is scriptedTransport that also keeps each request's
// correlation header.
type headerTransport struct {
	scriptedTransport
	correlations []string
}

func (h *headerTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	h.correlations = append(h.correlations, request.Header.Get(CorrelationIDHeader))
	return h.scriptedTransport.RoundTrip(request)
}

func opsClient(t *testing.T, replies ...func() (*http.Response, error)) (*DispatchClient, *headerTransport) {
	t.Helper()
	transport := &headerTransport{scriptedTransport: scriptedTransport{replies: replies}}
	base, _ := url.Parse("https://explorer.invalid")
	client, err := NewDispatchClient(base, &http.Client{Transport: transport},
		func() (string, error) { return "token", nil }, nil)
	if err != nil {
		t.Fatal(err)
	}
	bound, err := client.BindExecutor("gateway")
	if err != nil {
		t.Fatal(err)
	}
	return bound, transport
}

func opsContext() RequestContext {
	return RequestContext{RequestID: []byte("r"), ReasonCode: "gateway_op",
		Deadline: time.Now().Add(time.Minute), CorrelationID: []byte("trace-391")}
}

// claimedRecord is a claimed record at version under fence 1, with a lease
// end and deadline, and the ambiguity reports given.
func claimedRecord(t *testing.T, version uint64, fence uint64, leaseUntil, deadline time.Time,
	reports ...map[string]any) string {
	t.Helper()
	record := map[string]any{
		"id": base64.RawURLEncoding.EncodeToString([]byte("action")), "version": version,
		"state": dispatchapi.DispatchClaimed, "agent_id": "",
		"claim_id":    base64.RawURLEncoding.EncodeToString([]byte("claim")),
		"claim_fence": fence, "claim_lease_until": leaseUntil, "deadline": deadline,
		"effect_possible": true,
		"correlation_id":  base64.RawURLEncoding.EncodeToString([]byte("trace-391")),
	}
	if len(reports) > 0 {
		record["ambiguity_reports"] = reports
	}
	encoded, err := json.Marshal(record)
	if err != nil {
		t.Fatal(err)
	}
	return string(encoded)
}

func TestExtendTakesTheExplorersLeaseEndAndClassifiesRefusals(t *testing.T) {
	deadline := time.Date(2026, 10, 9, 12, 3, 0, 0, time.UTC)
	granted := deadline // clamped
	renewed := claimedRecord(t, 3, 1, granted, deadline)
	request := ExtendRequest{Context: opsContext(), ClaimID: []byte("claim"),
		ClaimFence: 1, Lease: dispatchapi.MaxActionClaimTTL}
	for _, row := range []struct {
		name    string
		replies []func() (*http.Response, error)
		kind    DispatchErrorKind
		status  int
		calls   int
	}{
		{"renewed", []func() (*http.Response, error){reply(200, renewed)}, "", 0, 1},
		{"claim lost", []func() (*http.Response, error){reply(409, `{"code":"conflict"}`)}, DispatchFenceLost, 409, 1},
		{"not held, rebound or gone", []func() (*http.Response, error){reply(404, `{"code":"not_found"}`)}, DispatchFenceLost, 404, 1},
		{"does not move the lease", []func() (*http.Response, error){reply(400, `{"code":"invalid_argument"}`)}, DispatchInvalid, 400, 1},
		{"unauthorized", []func() (*http.Response, error){reply(401, `{}`)}, DispatchUnauthorized, 401, 1},
		{"lost then renewed", []func() (*http.Response, error){lost, reply(200, renewed)}, "", 0, 2},
		{"plain 503 then renewed", []func() (*http.Response, error){reply(503, `{}`), reply(200, renewed)}, "", 0, 2},
		{"indeterminate then renewed", []func() (*http.Response, error){reply(503, `{}`, indeterminate...), reply(200, renewed)}, "", 0, 2},
		// The first attempt committed and moved the version: the resend's 409
		// is that, never a refusal.
		{"lost then conflict", []func() (*http.Response, error){lost, reply(409, `{}`)}, DispatchIndeterminate, 0, 2},
		{"lost then not found", []func() (*http.Response, error){lost, reply(404, `{}`)}, DispatchIndeterminate, 0, 2},
		{"lost twice", []func() (*http.Response, error){lost, lost}, DispatchIndeterminate, 0, 2},
		{"502 then 504", []func() (*http.Response, error){reply(502, ``), reply(504, ``)}, DispatchIndeterminate, 0, 2},
		// A 2xx that is not this renewal: under another fence, at another
		// version, past the deadline, or not decodable.
		{"another fence then renewed", []func() (*http.Response, error){
			reply(200, claimedRecord(t, 3, 2, granted, deadline)), reply(200, renewed)}, "", 0, 2},
		// Bound on the fence, a renewal at a version the worker never saw
		// (another holder's ambiguity report moved it) is this renewal.
		{"renewed past an unseen version", []func() (*http.Response, error){
			reply(200, claimedRecord(t, 9, 1, granted, deadline))}, "", 0, 1},
		{"a lease past the deadline twice", []func() (*http.Response, error){
			reply(200, claimedRecord(t, 3, 1, deadline.Add(time.Second), deadline)),
			reply(200, claimedRecord(t, 3, 1, deadline.Add(time.Second), deadline))}, DispatchIndeterminate, 0, 2},
		{"undecodable then renewed", []func() (*http.Response, error){reply(200, `nope`), reply(200, renewed)}, "", 0, 2},
	} {
		client, transport := opsClient(t, row.replies...)
		action, err := client.Extend(context.Background(), []byte("action"), request)
		if DispatchKind(err) != row.kind {
			t.Errorf("%s: %v (kind %q), want %q", row.name, err, DispatchKind(err), row.kind)
		}
		var dispatchErr *DispatchError
		if row.status != 0 && (!errors.As(err, &dispatchErr) || dispatchErr.Status != row.status) {
			t.Errorf("%s: status not kept: %#v", row.name, err)
		}
		if len(transport.bodies) != row.calls {
			t.Errorf("%s: %d requests, want %d", row.name, len(transport.bodies), row.calls)
		}
		for i := 1; i < len(transport.bodies); i++ {
			if !bytes.Equal(transport.bodies[i], transport.bodies[0]) {
				t.Errorf("%s: the resend was not the identical body", row.name)
			}
		}
		// #629: the renewal binds on the fence and pins no version.
		var sent map[string]any
		if err := json.Unmarshal(transport.bodies[0], &sent); err != nil {
			t.Fatal(err)
		}
		if _, pinned := sent["expected_version"]; pinned || sent["claim_fence"] != float64(1) {
			t.Errorf("%s: extend body %s, want claim_fence 1 and no expected_version", row.name, transport.bodies[0])
		}
		if row.kind == "" && !action.ClaimLeaseUntil.Equal(granted) {
			t.Errorf("%s: lease end %v, want the explorer's %v", row.name, action.ClaimLeaseUntil, granted)
		}
		if row.kind != "" && action.ID != nil {
			t.Errorf("%s: a refused extension returned a record", row.name)
		}
	}
}

func ambiguityReportWire(fence uint64, outcome dispatchapi.AmbiguityOutcome, reference string) map[string]any {
	return map[string]any{"claim_fence": fence, "outcome": outcome, "reference": reference,
		"subject": "c3ViamVjdA", "actor": "YWN0b3I", "reported_at": time.Now().UTC()}
}

func TestReportAmbiguityClassifiesEveryAnswer(t *testing.T) {
	now := time.Now().UTC()
	report := AmbiguityReport{Context: opsContext(), ClaimFence: 1,
		Outcome: dispatchapi.AmbiguityOutcomeUnknown, Reference: "ch_42"}
	recorded := claimedRecord(t, 4, 2, now.Add(time.Minute), now.Add(time.Hour),
		ambiguityReportWire(1, dispatchapi.AmbiguityOutcomeUnknown, "ch_42"))
	// A record that does not carry this report: another reference.
	without := claimedRecord(t, 4, 2, now.Add(time.Minute), now.Add(time.Hour),
		ambiguityReportWire(1, dispatchapi.AmbiguityOutcomeUnknown, "ch_43"))
	for _, row := range []struct {
		name       string
		replies    []func() (*http.Response, error)
		kind       DispatchErrorKind
		unrecorded bool
		calls      int
	}{
		// Accepted, and an identical replay (#542): both are the record.
		{"accepted", []func() (*http.Response, error){reply(200, recorded)}, "", false, 1},
		// Refused or not found: definite, unrecorded, never success (#514).
		{"budget spent", []func() (*http.Response, error){reply(400, `{"code":"invalid_argument"}`)}, DispatchAmbiguityUnrecorded, true, 1},
		{"holder evicted or never held", []func() (*http.Response, error){reply(404, `{"code":"not_found"}`)}, DispatchAmbiguityUnrecorded, true, 1},
		{"unauthorized", []func() (*http.Response, error){reply(403, `{}`)}, DispatchAmbiguityUnrecorded, true, 1},
		// A concurrent write raced the compare-and-set: nothing written,
		// resend once.
		{"conflict then accepted", []func() (*http.Response, error){reply(409, `{}`), reply(200, recorded)}, "", false, 2},
		{"conflict then refused", []func() (*http.Response, error){reply(409, `{}`), reply(400, `{}`)}, DispatchAmbiguityUnrecorded, true, 2},
		{"conflict twice", []func() (*http.Response, error){reply(409, `{}`), reply(409, `{}`)}, DispatchAmbiguityUnrecorded, true, 2},
		{"conflict then lost", []func() (*http.Response, error){reply(409, `{}`), lost}, DispatchIndeterminate, false, 2},
		// Possibly recorded: the identical resend replays.
		{"lost then the replay", []func() (*http.Response, error){lost, reply(200, recorded)}, "", false, 2},
		{"indeterminate then the replay", []func() (*http.Response, error){reply(503, `{}`, indeterminate...), reply(200, recorded)}, "", false, 2},
		{"500 then the replay", []func() (*http.Response, error){reply(500, `{}`), reply(200, recorded)}, "", false, 2},
		{"lost twice", []func() (*http.Response, error){lost, lost}, DispatchIndeterminate, false, 2},
		{"lost then refused", []func() (*http.Response, error){lost, reply(400, `{}`)}, DispatchIndeterminate, false, 2},
		{"plain 503 then not found", []func() (*http.Response, error){reply(503, `{}`), reply(404, `{}`)}, DispatchIndeterminate, false, 2},
		{"a record without this report twice", []func() (*http.Response, error){reply(200, without), reply(200, without)}, DispatchIndeterminate, false, 2},
		{"a record without this report then the record", []func() (*http.Response, error){reply(200, without), reply(200, recorded)}, "", false, 2},
	} {
		client, transport := opsClient(t, row.replies...)
		action, err := client.ReportAmbiguity(context.Background(), []byte("action"), report)
		if DispatchKind(err) != row.kind {
			t.Errorf("%s: %v (kind %q), want %q", row.name, err, DispatchKind(err), row.kind)
		}
		if errors.Is(err, ErrAmbiguityUnrecorded) != row.unrecorded {
			t.Errorf("%s: errors.Is(ErrAmbiguityUnrecorded) = %v", row.name, !row.unrecorded)
		}
		if row.kind == "" && len(action.AmbiguityReports) == 0 {
			t.Errorf("%s: success without the record", row.name)
		}
		if row.kind != "" && action.ID != nil {
			t.Errorf("%s: a failure returned a record", row.name)
		}
		if len(transport.bodies) != row.calls {
			t.Errorf("%s: %d requests, want %d", row.name, len(transport.bodies), row.calls)
		}
		for i := range transport.bodies {
			if !bytes.Equal(transport.bodies[i], transport.bodies[0]) {
				t.Errorf("%s: the resend was not the identical body", row.name)
			}
			var body map[string]any
			_ = json.Unmarshal(transport.bodies[i], &body)
			// Always an append: present and zero.
			if version, ok := body["expected_version"]; !ok || version != float64(0) {
				t.Errorf("%s: expected_version = %v, want an explicit 0", row.name, version)
			}
		}
	}
}

func TestReportAmbiguityBoundsTheTextAsTheExplorerDoes(t *testing.T) {
	for name, row := range map[string]struct {
		target, reference string
		ok                bool
	}{
		"empty":                    {"", "", true},
		"at the bound":             {strings.Repeat("t", dispatchapi.MaxAmbiguityTargetBytes), strings.Repeat("r", dispatchapi.MaxAmbiguityReferenceBytes), true},
		"a space and non-ASCII":    {"api.stripe.com", "ch 42 é", true},
		"reference past the bound": {"", strings.Repeat("r", dispatchapi.MaxAmbiguityReferenceBytes+1), false},
		"target past the bound":    {strings.Repeat("t", dispatchapi.MaxAmbiguityTargetBytes+1), "", false},
		"a control character":      {"", "ch_42\n", false},
		"a NUL":                    {"", "ch\x00", false},
		"a bidi override":          {"", "ch_‮24", false},
		"a zero-width space":       {"", "ch​42", false},
		"invalid UTF-8":            {"", "ch\xff", false},
		"a bidi override target":   {"host⁦", "", false},
	} {
		client, transport := opsClient(t, reply(200, `{}`), reply(200, `{}`))
		_, err := client.ReportAmbiguity(context.Background(), []byte("action"), AmbiguityReport{
			Context: opsContext(), ClaimFence: 1, Outcome: dispatchapi.AmbiguityEffectObserved,
			Target: row.target, Reference: row.reference,
		})
		refused := DispatchKind(err) == DispatchRefusedLocal
		if refused == row.ok {
			t.Errorf("%s: refused locally = %v, want %v (%v)", name, refused, !row.ok, err)
		}
		if refused && len(transport.bodies) != 0 {
			t.Errorf("%s: a refused report was sent", name)
		}
	}
}

// TestCorrelationIsSentFromTheRecordAndCheckedFirst: claim, extend, complete
// and ambiguity send the record's correlation; pull mints one per poll; a
// missing or malformed one is refused before anything is sent.
func TestCorrelationIsSentFromTheRecordAndCheckedFirst(t *testing.T) {
	ctx := context.Background()
	now := time.Now().UTC()
	pulled := `{"actions":[` + claimedRecord(t, 1, 1, now, now.Add(time.Hour)) + `]}`
	client, transport := opsClient(t, reply(200, pulled), reply(200, pulled))
	request := opsContext()
	request.CorrelationID = nil
	page, err := client.Pull(ctx, request, "", 8)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.Pull(ctx, request, "", 8); err != nil {
		t.Fatal(err)
	}
	if len(transport.correlations) != 2 || transport.correlations[0] == transport.correlations[1] {
		t.Fatalf("pull correlations = %q, want one minted per poll", transport.correlations)
	}
	for _, minted := range transport.correlations {
		if !strings.HasPrefix(minted, pollCorrelationPrefix) ||
			interaction.ValidateCorrelationID(shoal.ID(minted)) != nil {
			t.Fatalf("minted correlation %q is not recordable", minted)
		}
		var body struct {
			Context struct {
				CorrelationID string `json:"correlation_id"`
			} `json:"context"`
		}
		_ = json.Unmarshal(transport.bodies[0], &body)
		if decoded, _ := base64.RawURLEncoding.DecodeString(body.Context.CorrelationID); string(decoded) != transport.correlations[0] {
			t.Fatalf("the body's correlation %q differs from the header's", decoded)
		}
	}
	action := page.Actions[0]
	if string(action.CorrelationID) != "trace-391" {
		t.Fatalf("the pulled record's correlation = %q", action.CorrelationID)
	}

	// The record's correlation, on every request about it.
	terminal := committedUnder(t, 1, 3, dispatchapi.DispatchSucceeded, "", `{}`)
	client, transport = opsClient(t,
		reply(200, claimedRecord(t, 2, 1, now.Add(time.Minute), now.Add(time.Hour))),
		reply(200, claimedRecord(t, 3, 1, now.Add(2*time.Minute), now.Add(time.Hour))),
		reply(200, claimedRecord(t, 4, 1, now.Add(2*time.Minute), now.Add(time.Hour),
			ambiguityReportWire(1, dispatchapi.AmbiguityOutcomeUnknown, ""))),
		reply(200, terminal))
	base := RequestContext{RequestID: []byte("r"), ReasonCode: "gateway_op", Deadline: time.Now().Add(time.Minute)}
	if _, err := client.Claim(ctx, action.ID, ClaimRequest{Context: action.Correlate(base),
		ExpectedVersion: 1, ClaimID: []byte("claim"), Lease: time.Minute}); err != nil {
		t.Fatalf("claim: %v", err)
	}
	if _, err := client.Extend(ctx, action.ID, ExtendRequest{Context: action.Correlate(base),
		ClaimID: []byte("claim"), ClaimFence: 1, Lease: time.Minute}); err != nil {
		t.Fatalf("extend: %v", err)
	}
	if _, err := client.ReportAmbiguity(ctx, action.ID, AmbiguityReport{Context: action.Correlate(base),
		ClaimFence: 1, Outcome: dispatchapi.AmbiguityOutcomeUnknown}); err != nil {
		t.Fatalf("ambiguity: %v", err)
	}
	if _, err := client.Complete(ctx, action.ID, Completion{Context: action.Correlate(base),
		ExpectedVersion: 2, ClaimID: []byte("claim"), ClaimFence: 1, Output: json.RawMessage(`{}`)}); err != nil {
		t.Fatalf("complete: %v", err)
	}
	if strings.Join(transport.correlations, ",") != "trace-391,trace-391,trace-391,trace-391" {
		t.Fatalf("correlations sent = %q", transport.correlations)
	}
	// The completion carries the claim's fence (#484); the real-handler test
	// pins what the explorer does with it.
	if !strings.Contains(string(transport.bodies[3]), `"claim_fence":1`) {
		t.Fatalf("the completion carried no fence: %s", transport.bodies[3])
	}

	// Missing or malformed: refused locally, nothing sent.
	client, transport = opsClient(t)
	for name, correlation := range map[string][]byte{
		"missing": nil, "a space": []byte("trace 391"), "a newline": []byte("trace\n391"),
		"invalid UTF-8": []byte("trace\xff"), "a control": []byte("trace\x7f"),
		"too long": bytes.Repeat([]byte("c"), shoal.MaxIDBytes+1),
	} {
		malformed := base
		malformed.CorrelationID = correlation
		checks := map[string]error{}
		_, checks["claim"] = client.Claim(ctx, []byte("a"), ClaimRequest{Context: malformed,
			ExpectedVersion: 1, ClaimID: []byte("claim"), Lease: time.Minute})
		_, checks["extend"] = client.Extend(ctx, []byte("a"), ExtendRequest{Context: malformed,
			ClaimID: []byte("claim"), ClaimFence: 1, Lease: time.Minute})
		_, checks["complete"] = client.Complete(ctx, []byte("a"), Completion{Context: malformed,
			ExpectedVersion: 1, ClaimID: []byte("claim"), ClaimFence: 1, Output: json.RawMessage(`{}`)})
		_, checks["ambiguity"] = client.ReportAmbiguity(ctx, []byte("a"), AmbiguityReport{Context: malformed,
			ClaimFence: 1, Outcome: dispatchapi.AmbiguityRequestNotSent})
		if correlation != nil {
			_, checks["pull"] = client.Pull(ctx, malformed, "", 1)
		}
		for op, err := range checks {
			if DispatchKind(err) != DispatchRefusedLocal {
				t.Errorf("%s correlation on %s: %v", name, op, err)
			}
		}
	}
	if len(transport.bodies) != 0 {
		t.Fatalf("%d requests left with a missing or malformed correlation", len(transport.bodies))
	}
}

func TestOpsRefuseLocallyBeforeSending(t *testing.T) {
	client, transport := opsClient(t)
	ctx := context.Background()
	request := opsContext()
	extend := ExtendRequest{Context: request, ClaimID: []byte("claim"),
		ClaimFence: 1, Lease: time.Minute}
	checks := map[string]error{}
	for name, mutate := range map[string]func(*ExtendRequest){
		"no claim":   func(r *ExtendRequest) { r.ClaimID = nil },
		"no fence":   func(r *ExtendRequest) { r.ClaimFence = 0 },
		"zero lease": func(r *ExtendRequest) { r.Lease = 0 },
		"over TTL":   func(r *ExtendRequest) { r.Lease = dispatchapi.MaxActionClaimTTL + 1 },
	} {
		changed := extend
		mutate(&changed)
		_, checks["extend "+name] = client.Extend(ctx, []byte("a"), changed)
	}
	_, checks["ambiguity no fence"] = client.ReportAmbiguity(ctx, []byte("a"),
		AmbiguityReport{Context: request, Outcome: dispatchapi.AmbiguityOutcomeUnknown})
	_, checks["ambiguity open outcome"] = client.ReportAmbiguity(ctx, []byte("a"),
		AmbiguityReport{Context: request, ClaimFence: 1, Outcome: "it_went_fine"})
	_, checks["complete no fence"] = client.Complete(ctx, []byte("a"), Completion{Context: request,
		ExpectedVersion: 1, ClaimID: []byte("claim"), Output: json.RawMessage(`{}`)})
	unbound, _ := NewDispatchClient(client.base, client.http, client.credential, nil)
	_, checks["resolve unbound"] = unbound.Resolve(ctx, []byte("agent"), request)
	_, checks["attest unbound"] = unbound.PresentAttestation(ctx, "statement", "key")
	_, checks["attest missing files"] = client.PresentAttestation(ctx,
		filepath.Join(t.TempDir(), "absent"), filepath.Join(t.TempDir(), "absent"))
	for name, err := range checks {
		if DispatchKind(err) != DispatchRefusedLocal {
			t.Errorf("%s: %v", name, err)
		}
	}
	if len(transport.bodies) != 0 {
		t.Fatalf("%d requests left for locally refused calls", len(transport.bodies))
	}
	if _, err := client.BindExecutor("not a ref"); err == nil {
		t.Fatal("an invalid executor ref was bound")
	}
}

// TestResolveAdoptsOnlyItsOwnDescriptor: a descriptor naming another ref is
// refused, never returned.
func TestResolveAdoptsOnlyItsOwnDescriptor(t *testing.T) {
	descriptor := func(ref string) string {
		return `{"id":"` + base64.RawURLEncoding.EncodeToString([]byte("agent")) +
			`","generation":1,"executor_ref":"` + ref + `","capabilities":[]}`
	}
	client, _ := opsClient(t, reply(200, descriptor("gateway")), reply(200, descriptor("ledger")))
	got, err := client.Resolve(context.Background(), []byte("agent"), opsContext())
	if err != nil || got.ExecutorRef != "gateway" {
		t.Fatalf("own descriptor = %#v, %v", got, err)
	}
	got, err = client.Resolve(context.Background(), []byte("agent"), opsContext())
	if DispatchKind(err) != DispatchRefusedLocal || got.ID != nil {
		t.Fatalf("another ref's descriptor = %#v, %v", got, err)
	}
}

// TestPresentAttestationRereadsItsFiles: each call reads both files, sends
// them byte for byte, and returns the receipt's expires_at.
func TestPresentAttestationRereadsItsFiles(t *testing.T) {
	dir := t.TempDir()
	statementFile, keyFile := filepath.Join(dir, "statement"), filepath.Join(dir, "key")
	write := func(path, content string) {
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	first := time.Date(2026, 10, 9, 13, 0, 0, 0, time.UTC)
	second := first.Add(time.Hour)
	receipt := func(at time.Time) func() (*http.Response, error) {
		return reply(200, `{"attestation_id":"a","expires_at":"`+at.Format(time.RFC3339)+`"}`)
	}
	var bodies []map[string]string
	transport := roundTripFunc(func(request *http.Request) (*http.Response, error) {
		var body map[string]string
		data, _ := io.ReadAll(request.Body)
		_ = json.Unmarshal(data, &body)
		bodies = append(bodies, body)
		if request.URL.Path != "/api/v1/fleet/executors/attestation" {
			return reply(404, `{}`)()
		}
		if len(bodies) == 1 {
			return receipt(first)()
		}
		return receipt(second)()
	})
	base, _ := url.Parse("https://explorer.invalid")
	client, _ := NewDispatchClient(base, &http.Client{Transport: transport},
		func() (string, error) { return "token", nil }, nil)
	client, _ = client.BindExecutor("gateway")

	write(statementFile, `{"statement":1}`)
	write(keyFile, "key-1\n")
	expires, err := client.PresentAttestation(context.Background(), statementFile, keyFile)
	if err != nil || !expires.Equal(first) {
		t.Fatalf("first presentation = %v, %v", expires, err)
	}
	write(statementFile, `{"statement":2}`)
	write(keyFile, "key-2")
	expires, err = client.PresentAttestation(context.Background(), statementFile, keyFile)
	if err != nil || !expires.Equal(second) {
		t.Fatalf("second presentation = %v, %v", expires, err)
	}
	decode := func(value string) string {
		decoded, _ := base64.RawURLEncoding.DecodeString(value)
		return string(decoded)
	}
	if len(bodies) != 2 || bodies[0]["executor_ref"] != "gateway" ||
		decode(bodies[0]["idempotency_key"]) != "key-1\n" || decode(bodies[1]["idempotency_key"]) != "key-2" ||
		decode(bodies[0]["report"]) != `{"statement":1}` || decode(bodies[1]["report"]) != `{"statement":2}` {
		t.Fatalf("presented %v", bodies)
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) { return f(request) }

// TestResolveRefusesAGateItCannotSee: an action field the gateway does not
// know may be a gate (requires_approval and requires_attestation arrived that
// way), so the descriptor is refused and the gateway does not start.
func TestResolveRefusesAGateItCannotSee(t *testing.T) {
	client, _ := opsClient(t, reply(200, `{"id":"YWdlbnQ","generation":1,"executor_ref":"gateway",`+
		`"capabilities":[{"name":"effects.gateway","actions":[{"name":"charge","input_schema":{},`+
		`"output_schema":{},"effects":["external"],"requires_new_gate":true}]}]}`))
	request := opsContext()
	request.CorrelationID = nil
	if _, err := client.Resolve(context.Background(), []byte("agent"), request); DispatchKind(err) != DispatchProtocol {
		t.Fatalf("resolve with an unknown gate = %v, want protocol", err)
	}
}
