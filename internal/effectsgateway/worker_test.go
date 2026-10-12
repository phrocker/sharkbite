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
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	dispatchapi "github.com/phrocker/shoal-oss/pkg/dispatch/api"
)

func (h *workerHarness) state(id string) dispatchapi.DispatchState {
	return h.explorer.record(id).State
}

func (h *workerHarness) assertNoSecrets() {
	h.t.Helper()
	for _, secret := range []string{"SECRET", "cus_", "4200", "sk_test"} {
		if strings.Contains(h.logs.String(), secret) {
			h.t.Fatalf("the log carries %q:\n%s", secret, h.logs.String())
		}
	}
	data, _ := os.ReadFile(filepath.Join(h.dir, UnrecordedFileName))
	for _, secret := range []string{"SECRET", "cus_", "4200", "sk_test"} {
		if bytes.Contains(data, []byte(secret)) {
			h.t.Fatalf("the unrecorded log carries %q", secret)
		}
	}
}

// TestWorkerHappyPath: PULL → FILTER → PRECHECK → CLAIM → BIND → SEND →
// CLASSIFY → COMPLETE, with the action's ExecutorKey as the idempotency key,
// the record's correlation on the claim, the claim fence on the completion,
// and nothing input-derived in the log.
func TestWorkerHappyPath(t *testing.T) {
	h := newWorkerHarness(t, nil)
	h.explorer.enqueue("a1", "charge", chargeInput, workerEpoch.Add(time.Hour))
	h.start()
	eventually(t, "the action to succeed", func() bool { return h.state("a1") == dispatchapi.DispatchSucceeded })

	completion := h.completion(1)
	if completion.Failed || completion.ClaimFence != 1 || completion.ExpectedVersion != 2 {
		t.Fatalf("completion = %+v", completion)
	}
	var output map[string]any
	if err := json.Unmarshal(completion.Output, &output); err != nil ||
		output["reference"] != "ch_1" || output["idempotency"] != "key" || output["status"] != float64(200) {
		t.Fatalf("output = %s", completion.Output)
	}
	effects, requests, keys := h.target.stats()
	if effects != 1 || requests != 1 || keys[0] != testExecutorKey(t, "a1").HeaderValue() {
		t.Fatalf("target saw %d effects, %d requests, keys %q", effects, requests, keys)
	}
	h.explorer.mu.Lock()
	claim := h.explorer.claims[0]
	h.explorer.mu.Unlock()
	if !bytes.HasPrefix(claim.ClaimID, []byte("gw1|gw-0|")) || string(claim.Context.CorrelationID) != "trace-a1" ||
		claim.Lease != 60*time.Second || string(completion.Context.CorrelationID) != "trace-a1" {
		t.Fatalf("claim = %q %q %v", claim.ClaimID, claim.Context.CorrelationID, claim.Lease)
	}
	if ready, reason := h.worker.Ready(); !ready {
		t.Fatalf("not ready: %s", reason)
	}
	for _, event := range []string{"claimed", "sent", "completed"} {
		eventually(t, "the "+event+" event", func() bool { return h.logs.has(event) })
	}
	h.assertNoSecrets()
	if err := h.stop(); err != nil {
		t.Fatal(err)
	}
	if ready, reason := h.worker.Ready(); ready || reason != NotReadyStopped {
		t.Fatalf("after stop: %v %s", ready, reason)
	}
}

// TestWorkerFiltersAndPrechecksBeforeClaiming: another agent's, another
// capability's and an unrouted action are never claimed; a key route whose
// deadline outlives the retention, and an action whose deadline cannot hold
// T + ReportWindow, are skipped and logged once each.
func TestWorkerFiltersAndPrechecksBeforeClaiming(t *testing.T) {
	h := newWorkerHarness(t, nil)
	far := workerEpoch.Add(time.Hour)
	h.explorer.enqueueFor("other-agent", "ledger-agent", testCapability, "charge", chargeInput, far)
	h.explorer.enqueueFor("other-cap", testAgent, "ops", "charge", chargeInput, far)
	h.explorer.enqueueFor("unrouted", testAgent, testCapability, "transfer", chargeInput, far)
	h.explorer.enqueue("retention", "charge", chargeInput, workerEpoch.Add(25*time.Hour))
	h.explorer.enqueue("late", "charge", chargeInput, workerEpoch.Add(20*time.Second))
	// Unprotected: retention does not apply.
	h.explorer.enqueue("refund", "refund", `{"body":{"charge":"x"}}`, workerEpoch.Add(25*time.Hour))
	h.start()
	eventually(t, "the refund to succeed", func() bool { return h.state("refund") == dispatchapi.DispatchSucceeded })
	for i := 0; i < 3; i++ {
		h.clock.Advance(MaxBackoff)
		settle()
	}
	pulls, claims, _, _, _ := h.explorer.counts()
	if claims != 1 || pulls < 3 {
		t.Fatalf("%d claims over %d pulls, want only the refund", claims, pulls)
	}
	gates := map[string]int{}
	for _, line := range h.logs.lines() {
		if line["event"] == "skipped" {
			gates[line["gate"].(string)]++
		}
	}
	if gates["retention"] != 1 || gates["deadline"] != 1 || len(gates) != 2 {
		t.Fatalf("skipped gates = %v, want retention and deadline once each", gates)
	}
}

// TestWorkerRepullsOnAnIndeterminateOrLostClaim: a repull (indeterminate),
// a 409 and a 404 each send the worker back to PULL, and each claim attempt
// carries a fresh claim ID.
func TestWorkerRepullsOnAnIndeterminateOrLostClaim(t *testing.T) {
	h := newWorkerHarness(t, nil)
	h.explorer.onClaim = func(n int, _ []byte, _ ClaimRequest) (Action, error, bool) {
		switch n {
		case 1:
			return Action{}, &DispatchError{Op: "claim", Kind: DispatchRepull, Status: 503}, true
		case 2:
			return Action{}, conflict("claim", http.StatusConflict), true
		case 3:
			return Action{}, conflict("claim", http.StatusNotFound), true
		}
		return Action{}, nil, false
	}
	h.explorer.enqueue("a1", "charge", chargeInput, workerEpoch.Add(time.Hour))
	h.start()
	for i := 0; i < 6 && h.state("a1") != dispatchapi.DispatchSucceeded; i++ {
		settle()
		h.clock.Advance(MaxBackoff)
	}
	eventually(t, "the action to succeed", func() bool { return h.state("a1") == dispatchapi.DispatchSucceeded })
	h.explorer.mu.Lock()
	defer h.explorer.mu.Unlock()
	seen := map[string]bool{}
	for _, claim := range h.explorer.claims {
		if seen[string(claim.ClaimID)] {
			t.Fatalf("claim ID reused across attempts")
		}
		seen[string(claim.ClaimID)] = true
	}
	if len(h.explorer.claims) != 4 {
		t.Fatalf("%d claims, want 4", len(h.explorer.claims))
	}
	if effects, _, _ := h.target.stats(); effects != 1 {
		t.Fatalf("%d effects", effects)
	}
}

// TestWorkerRecordsATargetRefusal: a 422 completes failed/target_rejected_422
// with the volume that may have left (#427).
func TestWorkerRecordsATargetRefusal(t *testing.T) {
	h := newWorkerHarness(t, nil)
	h.target.script = func(_ int, w http.ResponseWriter, _ *http.Request) bool {
		w.WriteHeader(http.StatusUnprocessableEntity)
		_, _ = w.Write([]byte(`{"error":"card_declined SECRET"}`))
		return true
	}
	h.explorer.enqueue("a1", "charge", chargeInput, workerEpoch.Add(time.Hour))
	h.start()
	eventually(t, "the action to fail", func() bool { return h.state("a1") == dispatchapi.DispatchFailed })
	completion := h.completion(1)
	body := `{"amount":4200,"memo":"SECRET-MEMO"}`
	if !completion.Failed || completion.ErrorCode != "target_rejected_422" ||
		completion.Effected != (dispatchapi.Effected{Bytes: int64(len(body)), Chunks: 1}) {
		t.Fatalf("completion = %+v", completion)
	}
	h.assertNoSecrets()
}

// TestWorkerCapsRetryAfterAndResendsTheSameKey: a 503 asking for an hour is
// retried after MaxRetryAfter, under the same key.
func TestWorkerCapsRetryAfterAndResendsTheSameKey(t *testing.T) {
	h := newWorkerHarness(t, nil)
	h.target.script = func(n int, w http.ResponseWriter, _ *http.Request) bool {
		if n == 1 {
			w.Header().Set("Retry-After", "3600")
			w.WriteHeader(http.StatusServiceUnavailable)
			return true
		}
		return false
	}
	h.explorer.enqueue("a1", "charge", chargeInput, workerEpoch.Add(time.Hour))
	h.start()
	eventually(t, "the first request", func() bool { _, n, _ := h.target.stats(); return n == 1 })
	settle()
	h.clock.Advance(MaxBackoff - time.Second)
	settle()
	if _, n, _ := h.target.stats(); n != 1 {
		t.Fatalf("retried after %v", MaxBackoff-time.Second)
	}
	h.clock.Advance(time.Second)
	eventually(t, "the action to succeed", func() bool { return h.state("a1") == dispatchapi.DispatchSucceeded })
	if _, _, keys := h.target.stats(); len(keys) != 2 || keys[0] != keys[1] {
		t.Fatalf("keys = %q", keys)
	}
}

// TestWorkerResendsALostResponseUnderTheSameKey: the target performs the
// effect and the response is lost; the resend under the same key is
// replayed, and there is one effect.
func TestWorkerResendsALostResponseUnderTheSameKey(t *testing.T) {
	h := newWorkerHarness(t, nil)
	h.target.dropFirst = true
	h.explorer.enqueue("a1", "charge", chargeInput, workerEpoch.Add(time.Hour))
	h.start()
	eventually(t, "the first request", func() bool { _, n, _ := h.target.stats(); return n >= 1 })
	for i := 0; i < 5 && h.state("a1") != dispatchapi.DispatchSucceeded; i++ {
		settle()
		h.clock.Advance(time.Second)
	}
	eventually(t, "the action to succeed", func() bool { return h.state("a1") == dispatchapi.DispatchSucceeded })
	effects, requests, _ := h.target.stats()
	var output map[string]any
	_ = json.Unmarshal(h.completion(1).Output, &output)
	if effects != 1 || requests != 2 || output["idempotency"] != "replayed" || output["reference"] != "ch_1" {
		t.Fatalf("effects %d, requests %d, output %v", effects, requests, output)
	}
}

// TestWorkerNeverResendsAWrittenUnprotectedRequest: the same loss on an
// unprotected route is outcome_unknown, sent once.
func TestWorkerNeverResendsAWrittenUnprotectedRequest(t *testing.T) {
	h := newWorkerHarness(t, nil)
	h.target.dropFirst = true
	h.explorer.enqueue("r1", "refund", `{"body":{"charge":"ch_9"}}`, workerEpoch.Add(time.Hour))
	h.start()
	eventually(t, "the action to fail", func() bool { return h.state("r1") == dispatchapi.DispatchFailed })
	completion := h.completion(1)
	if _, requests, _ := h.target.stats(); requests != 1 || completion.ErrorCode != ErrorOutcomeUnknown ||
		completion.Effected.Bytes == 0 {
		t.Fatalf("requests %d, completion %+v", requests, completion)
	}
}

// startGated starts the harness with the target holding its first request
// after the effect, and waits for it.
func (h *workerHarness) startGated() {
	h.t.Helper()
	h.target.gate = make(chan struct{})
	h.t.Cleanup(h.release)
	h.start()
	select {
	case <-h.target.entered:
	case <-time.After(10 * time.Second):
		h.t.Fatal("the target saw no request")
	}
	settle()
}

// release opens the target's gate, once; a failing test's cleanup opens it
// too, so the target's Close is never left waiting on a held request.
func (h *workerHarness) release() {
	h.target.mu.Lock()
	gate := h.target.gate
	h.target.gate = nil
	h.target.mu.Unlock()
	if gate != nil {
		close(gate)
	}
}

// TestWorkerExtendsToTheExplorersClampedEnd: the extension at L/2 is granted
// only to the action's deadline (75s), and the worker's lease ends there —
// not at the extension's send time plus the lease it asked for (90s). The
// claim is held with nothing sent yet, so the lost fence is reported
// request_not_sent and nothing is sent or completed.
func TestWorkerExtendsToTheExplorersClampedEnd(t *testing.T) {
	hold, entered := make(chan struct{}), make(chan struct{}, 1)
	h := newWorkerHarness(t, nil)
	first := true
	h.credential = func() {
		if first {
			first = false
			entered <- struct{}{}
			<-hold
		}
	}
	h.explorer.enqueue("a1", "charge", chargeInput, workerEpoch.Add(75*time.Second))
	h.start()
	<-entered
	settle()
	h.clock.Advance(30 * time.Second)
	eventually(t, "the extension", func() bool { _, _, n, _, _ := h.explorer.counts(); return n == 1 })
	settle()
	if got := h.explorer.record("a1").ClaimLeaseUntil; !got.Equal(workerEpoch.Add(75 * time.Second)) {
		t.Fatalf("granted %v", got)
	}
	h.clock.Advance(44 * time.Second) // 74s
	settle()
	if h.logs.has("fence_lost") {
		t.Fatal("the lease ended before the explorer's end")
	}
	h.clock.Advance(2 * time.Second) // 76s: past the clamped end, well before 90s
	eventually(t, "the fence to be lost at the clamped end", func() bool { return h.logs.has("fence_lost") })
	if _, _, extends, _, _ := h.explorer.counts(); extends != 1 {
		t.Fatalf("%d extensions of a lease clamped to the deadline", extends)
	}
	close(hold)
	eventually(t, "the report", func() bool { return len(h.reportsMade()) == 1 })
	settle()
	if report := h.reportsMade()[0]; report.Outcome != dispatchapi.AmbiguityRequestNotSent || report.ClaimFence != 1 {
		t.Fatalf("report = %+v", report)
	}
	if _, requests, _ := h.target.stats(); requests != 0 {
		t.Fatal("sent after the lease ended")
	}
	if _, _, _, completions, _ := h.explorer.counts(); completions != 0 {
		t.Fatalf("%d completions after the lease ended", completions)
	}
}

// TestWorkerReportsAnExtensionRefusedMidFlight: a refused extension while the
// request is in flight lets it finish, then reports effect_observed, once,
// and does not complete.
func TestWorkerReportsAnExtensionRefusedMidFlight(t *testing.T) {
	h := newWorkerHarness(t, nil)
	h.explorer.onExtend = func(int, []byte, ExtendRequest) (Action, error, bool) {
		return Action{}, &DispatchError{Op: "extend", Kind: DispatchFenceLost, Status: 409}, true
	}
	h.explorer.enqueue("a1", "charge", chargeInput, workerEpoch.Add(time.Hour))
	h.startGated()
	h.clock.Advance(30 * time.Second)
	eventually(t, "the fence to be lost", func() bool { return h.logs.has("fence_lost") })
	if reports := h.reportsMade(); len(reports) != 0 {
		t.Fatalf("reported before the request finished: %+v", reports)
	}
	h.release()
	eventually(t, "the report", func() bool { return len(h.reportsMade()) == 1 })
	settle()
	if reports := h.reportsMade(); len(reports) != 1 || reports[0].Outcome != dispatchapi.AmbiguityEffectObserved ||
		reports[0].Reference != "ch_1" {
		t.Fatalf("reports = %+v", reports)
	}
	if _, _, _, completions, _ := h.explorer.counts(); completions != 0 {
		t.Fatalf("completed after the fence was lost")
	}
	eventually(t, "the ambiguity_reported event", func() bool { return h.logs.has("ambiguity_reported") })
}

// TestWorkerReportsRequestNotSentWhenTheFenceIsLostFirst: the fence is lost
// before anything left; the request is never sent, request_not_sent is
// reported, and the claim is left to lapse.
func TestWorkerReportsRequestNotSentWhenTheFenceIsLostFirst(t *testing.T) {
	hold, entered := make(chan struct{}), make(chan struct{}, 1)
	h := newWorkerHarness(t, nil)
	first := true
	h.credential = func() {
		if first {
			first = false
			entered <- struct{}{}
			<-hold
		}
	}
	h.explorer.onExtend = func(int, []byte, ExtendRequest) (Action, error, bool) {
		return Action{}, &DispatchError{Op: "extend", Kind: DispatchFenceLost, Status: 404}, true
	}
	h.explorer.enqueue("a1", "charge", chargeInput, workerEpoch.Add(time.Hour))
	h.start()
	<-entered
	settle()
	h.clock.Advance(30 * time.Second)
	eventually(t, "the fence to be lost", func() bool { return h.logs.has("fence_lost") })
	close(hold)
	eventually(t, "the report", func() bool { return len(h.reportsMade()) == 1 })
	settle()
	if reports := h.reportsMade(); reports[0].Outcome != dispatchapi.AmbiguityRequestNotSent {
		t.Fatalf("report = %+v", reports[0])
	}
	if _, requests, _ := h.target.stats(); requests != 0 {
		t.Fatalf("%d requests left after the fence was lost", requests)
	}
	if _, _, _, completions, _ := h.explorer.counts(); completions != 0 {
		t.Fatal("completed a lost claim")
	}
}

// TestWorkerLosesTheFenceAtTheLocalLeaseEnd: extensions that never get an
// answer leave the lease where it was; reaching it is FENCE_LOST.
func TestWorkerLosesTheFenceAtTheLocalLeaseEnd(t *testing.T) {
	h := newWorkerHarness(t, nil)
	h.explorer.onExtend = func(int, []byte, ExtendRequest) (Action, error, bool) {
		return Action{}, &DispatchError{Op: "extend", Kind: DispatchIndeterminate}, true
	}
	h.explorer.enqueue("a1", "charge", chargeInput, workerEpoch.Add(time.Hour))
	h.startGated()
	for elapsed := time.Duration(0); elapsed < 60*time.Second; elapsed += 5 * time.Second {
		h.clock.Advance(5 * time.Second)
		settle()
	}
	eventually(t, "the fence to be lost", func() bool { return h.logs.has("fence_lost") })
	if _, _, extends, _, _ := h.explorer.counts(); extends < 2 {
		t.Fatalf("%d extensions attempted", extends)
	}
	h.release()
	eventually(t, "the report", func() bool { return len(h.reportsMade()) == 1 })
	if report := h.reportsMade()[0]; report.Outcome != dispatchapi.AmbiguityEffectObserved {
		t.Fatalf("report = %+v", report)
	}
}

// TestWorkerMakesOneReportPerFence: input that cannot be bound is reported
// request_not_sent and completed input_invalid; when that completion is
// refused, the fence has already had its report, and gets no second.
func TestWorkerMakesOneReportPerFence(t *testing.T) {
	h := newWorkerHarness(t, nil)
	h.explorer.onComplete = func(int, []byte, Completion) (Action, error, bool) {
		return Action{}, conflict("complete", http.StatusConflict), true
	}
	h.explorer.enqueue("a1", "charge", `{"path":{}}`, workerEpoch.Add(time.Hour))
	h.start()
	eventually(t, "the completion", func() bool { _, _, _, n, _ := h.explorer.counts(); return n == 1 })
	settle()
	if completion := h.completion(1); completion.ErrorCode != ErrorInputInvalid {
		t.Fatalf("completion = %+v", completion)
	}
	if reports := h.reportsMade(); len(reports) != 1 || reports[0].Outcome != dispatchapi.AmbiguityRequestNotSent {
		t.Fatalf("reports = %+v, want exactly one", reports)
	}
	if _, requests, _ := h.target.stats(); requests != 0 {
		t.Fatal("unbindable input was sent")
	}
}

// TestWorkerHoldsAnUnrecordedReport: the explorer refuses the report (#514);
// it goes to the unrecorded log with only policy fields, the gauge counts it,
// and it survives a reopen.
func TestWorkerHoldsAnUnrecordedReport(t *testing.T) {
	h := newWorkerHarness(t, nil)
	h.explorer.onExtend = func(int, []byte, ExtendRequest) (Action, error, bool) {
		return Action{}, &DispatchError{Op: "extend", Kind: DispatchFenceLost, Status: 409}, true
	}
	h.explorer.onAmbiguity = func(int, []byte, AmbiguityReport) (Action, error, bool) {
		return Action{}, &DispatchError{Op: "ambiguity", Kind: DispatchAmbiguityUnrecorded, Status: 400}, true
	}
	h.explorer.enqueue("a1", "charge", chargeInput, workerEpoch.Add(time.Hour))
	h.startGated()
	h.clock.Advance(30 * time.Second)
	eventually(t, "the fence to be lost", func() bool { return h.logs.has("fence_lost") })
	h.release()
	eventually(t, "the unrecorded entry", func() bool { return h.worker.UnrecordedEntries() == 1 })
	entry := h.log.Entries()[0]
	if string(entry.ActionID) != "a1" || entry.Fence != 1 || entry.ClaimNonce == (ClaimNonce{}) ||
		entry.RouteAction != "charge" || entry.Method != "POST" ||
		entry.PathTemplate != "/v1/customers/{customer}/charges" ||
		entry.Outcome != dispatchapi.AmbiguityEffectObserved || entry.Reference != "ch_1" ||
		entry.Target != testSurface || entry.Status != 400 ||
		entry.DispatchError != DispatchAmbiguityUnrecorded || entry.FirstAt.IsZero() ||
		string(entry.CorrelationID) != "trace-a1" {
		t.Fatalf("entry = %+v", entry)
	}
	eventually(t, "the unrecorded event", func() bool { return h.logs.has("unrecorded") })
	var gauge float64 = -1
	for _, line := range h.logs.lines() {
		if line["event"] == "unrecorded" {
			gauge, _ = line["unrecorded_entries"].(float64)
		}
	}
	if gauge != 1 {
		t.Fatalf("unrecorded gauge = %v:\n%s", gauge, h.logs)
	}
	if reports := h.reportsMade(); len(reports) != 1 {
		t.Fatalf("%d report calls for an unrecorded refusal, want 1", len(reports))
	}
	h.assertNoSecrets()
	if err := h.stop(); err != nil {
		t.Fatal(err)
	}
	_ = h.log.Close()
	reopened, err := OpenUnrecordedLog(h.dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if reopened.Len() != 1 {
		t.Fatalf("%d entries after reopen", reopened.Len())
	}
}

// TestWorkerHoldsAnIndeterminateReportAfterRetrying: a report whose answer
// is lost on every attempt is retried, then held.
func TestWorkerHoldsAnIndeterminateReportAfterRetrying(t *testing.T) {
	h := newWorkerHarness(t, nil)
	h.explorer.onExtend = func(int, []byte, ExtendRequest) (Action, error, bool) {
		return Action{}, &DispatchError{Op: "extend", Kind: DispatchFenceLost, Status: 409}, true
	}
	h.explorer.onAmbiguity = func(int, []byte, AmbiguityReport) (Action, error, bool) {
		return Action{}, &DispatchError{Op: "ambiguity", Kind: DispatchIndeterminate}, true
	}
	h.explorer.enqueue("a1", "charge", chargeInput, workerEpoch.Add(time.Hour))
	h.startGated()
	h.clock.Advance(30 * time.Second)
	eventually(t, "the fence to be lost", func() bool { return h.logs.has("fence_lost") })
	h.release()
	eventually(t, "the unrecorded entry", func() bool { return h.worker.UnrecordedEntries() == 1 })
	if reports := h.reportsMade(); len(reports) != reportAttempts {
		t.Fatalf("%d report calls, want %d", len(reports), reportAttempts)
	}
	if entry := h.log.Entries()[0]; entry.DispatchError != DispatchIndeterminate {
		t.Fatalf("entry = %+v", entry)
	}
}

// fillUnrecorded writes n entries straight into the log file.
func fillUnrecorded(t *testing.T, dir string, n int) {
	t.Helper()
	var data bytes.Buffer
	for i := 0; i < n; i++ {
		line, err := encodeEntry(UnrecordedEntry{
			ActionID: []byte("held-" + string(rune('a'+i%26)) + "-" + itoa(i)), Fence: 1,
			RouteAction: "charge", Method: "POST", PathTemplate: "/v1/charges",
			Outcome: dispatchapi.AmbiguityOutcomeUnknown, CorrelationID: []byte("trace"),
			FirstAt: workerEpoch, LastAt: workerEpoch, Attempts: 1,
		})
		if err != nil {
			t.Fatal(err)
		}
		data.Write(line)
	}
	if err := os.WriteFile(filepath.Join(dir, UnrecordedFileName), data.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
}

func itoa(i int) string { return strings.TrimSpace(strings.Repeat(" ", 0) + jsonNumber(i)) }

func jsonNumber(i int) string {
	encoded, _ := json.Marshal(i)
	return string(encoded)
}

// TestWorkerStopsClaimingWhenTheUnrecordedLogIsFull: a full log means no
// claim and not-ready; acknowledging an entry resumes work.
func TestWorkerStopsClaimingWhenTheUnrecordedLogIsFull(t *testing.T) {
	clock := newTimerClock()
	dir := t.TempDir()
	fillUnrecorded(t, dir, UnrecordedMaxEntries)
	h := &workerHarness{t: t, clock: clock, explorer: newFakeExplorer(t, clock),
		target: newFakeTarget(t), dir: dir, logs: &lockedBuffer{}}
	h.explorer.onAmbiguity = func(int, []byte, AmbiguityReport) (Action, error, bool) {
		return Action{}, &DispatchError{Op: "ambiguity", Kind: DispatchAmbiguityUnrecorded, Status: 404}, true
	}
	h.build(nil)
	h.explorer.enqueue("a1", "charge", chargeInput, workerEpoch.Add(time.Hour))
	h.start()
	eventually(t, "not-ready", func() bool {
		_, reason := h.worker.Ready()
		return reason == NotReadyUnrecordedFull
	})
	h.clock.Advance(MaxBackoff)
	settle()
	if pulls, claims, _, _, _ := h.explorer.counts(); claims != 0 || pulls != 0 {
		t.Fatalf("%d pulls and %d claims with a full log", pulls, claims)
	}
	// Every held entry was retried once at start, and each failed retry was
	// noted on its entry.
	if reports := h.reportsMade(); len(reports) != UnrecordedMaxEntries {
		t.Fatalf("%d retries at start", len(reports))
	}
	if entry := h.log.Entries()[0]; entry.Attempts != 2 || entry.Status != 404 {
		t.Fatalf("entry after a failed retry = %+v", entry)
	}
	first := h.log.Entries()[0]
	if removed, err := h.worker.AckUnrecorded(first.ActionID, first.Fence); err != nil || !removed {
		t.Fatalf("ack = %v, %v", removed, err)
	}
	if ready, reason := h.worker.Ready(); !ready {
		t.Fatalf("still not ready after an ack: %s", reason)
	}
	for i := 0; i < 3 && h.state("a1") != dispatchapi.DispatchSucceeded; i++ {
		h.clock.Advance(MaxBackoff)
		settle()
	}
	eventually(t, "the action to succeed", func() bool { return h.state("a1") == dispatchapi.DispatchSucceeded })
	if !h.logs.has("unrecorded_cleared") {
		t.Fatal("no unrecorded_cleared event for the ack")
	}
}

// TestWorkerRetriesHeldReportsAtStart: on start a held report is presented
// again; one the explorer now records (an identical replay is accepted) is
// cleared, one it still refuses stays.
func TestWorkerRetriesHeldReportsAtStart(t *testing.T) {
	clock := newTimerClock()
	dir := t.TempDir()
	log, err := OpenUnrecordedLog(dir, clock.Now)
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"recordable", "refused"} {
		if err := log.Append(UnrecordedEntry{ActionID: []byte(id), Fence: 3,
			RouteAction: "charge", Method: "POST", PathTemplate: "/v1/charges",
			Outcome: dispatchapi.AmbiguityEffectObserved, Reference: "ch_7", Target: testSurface,
			CorrelationID: []byte("trace-" + id)}); err != nil {
			t.Fatal(err)
		}
	}
	h := &workerHarness{t: t, clock: clock, explorer: newFakeExplorer(t, clock),
		target: newFakeTarget(t), dir: dir, logs: &lockedBuffer{}, log: log}
	h.explorer.enqueue("recordable", "charge", chargeInput, workerEpoch.Add(time.Hour))
	h.build(nil)
	h.start()
	eventually(t, "the recordable entry to clear and the other's retry to be noted", func() bool {
		entries := h.log.Entries()
		return len(entries) == 1 && entries[0].Attempts == 2
	})
	if remaining := h.log.Entries()[0]; string(remaining.ActionID) != "refused" || remaining.Attempts != 2 {
		t.Fatalf("remaining = %+v", remaining)
	}
	reports := h.reportsMade()
	if len(reports) != 2 || reports[0].ClaimFence != 3 || reports[0].Reference != "ch_7" ||
		string(reports[0].Context.CorrelationID) != "trace-recordable" {
		t.Fatalf("retries = %+v", reports)
	}
	if !h.logs.has("unrecorded_cleared") {
		t.Fatal("no unrecorded_cleared event")
	}
}

// TestWorkerDrainsInFlightWork: on shutdown the worker stops pulling and is
// not ready; the request in flight finishes and completes.
func TestWorkerDrainsInFlightWork(t *testing.T) {
	h := newWorkerHarness(t, nil)
	h.explorer.enqueue("a1", "charge", chargeInput, workerEpoch.Add(time.Hour))
	h.startGated()
	pulls, _, _, _, _ := h.explorer.counts()
	h.cancel()
	eventually(t, "draining", func() bool { _, reason := h.worker.Ready(); return reason == NotReadyDraining })
	h.release()
	if err := h.stop(); err != nil {
		t.Fatal(err)
	}
	if h.state("a1") != dispatchapi.DispatchSucceeded {
		t.Fatalf("state = %s", h.state("a1"))
	}
	if after, _, _, _, _ := h.explorer.counts(); after != pulls {
		t.Fatalf("pulled while draining")
	}
}

// TestWorkerDrainReportsUnsentClaims: a claim with nothing sent when the
// drain begins is reported request_not_sent and left to lapse.
func TestWorkerDrainReportsUnsentClaims(t *testing.T) {
	hold, entered := make(chan struct{}), make(chan struct{}, 1)
	h := newWorkerHarness(t, nil)
	first := true
	h.credential = func() {
		if first {
			first = false
			entered <- struct{}{}
			<-hold
		}
	}
	h.explorer.enqueue("a1", "charge", chargeInput, workerEpoch.Add(time.Hour))
	h.start()
	<-entered
	h.cancel()
	eventually(t, "draining", func() bool { _, reason := h.worker.Ready(); return reason == NotReadyDraining })
	close(hold)
	if err := h.stop(); err != nil {
		t.Fatal(err)
	}
	if reports := h.reportsMade(); len(reports) != 1 || reports[0].Outcome != dispatchapi.AmbiguityRequestNotSent {
		t.Fatalf("reports = %+v", reports)
	}
	if _, requests, _ := h.target.stats(); requests != 0 {
		t.Fatal("sent while draining")
	}
	if h.state("a1") != dispatchapi.DispatchClaimed {
		t.Fatalf("state = %s, want the claim left to lapse", h.state("a1"))
	}
}

// TestWorkerWaitsOutTheLeaseAfterAnIndeterminateCompletion: a completion that
// may have committed is followed, after the lease, by an ambiguity report
// (#506); not before.
func TestWorkerWaitsOutTheLeaseAfterAnIndeterminateCompletion(t *testing.T) {
	h := newWorkerHarness(t, nil)
	h.explorer.onComplete = func(int, []byte, Completion) (Action, error, bool) {
		return Action{}, &DispatchError{Op: "complete", Kind: DispatchIndeterminate}, true
	}
	h.explorer.enqueue("a1", "charge", chargeInput, workerEpoch.Add(time.Hour))
	h.start()
	eventually(t, "the completion", func() bool { _, _, _, n, _ := h.explorer.counts(); return n == 1 })
	settle()
	h.clock.Advance(59 * time.Second)
	settle()
	if reports := h.reportsMade(); len(reports) != 0 {
		t.Fatalf("reported inside the lease: %+v", reports)
	}
	if _, _, extends, _, _ := h.explorer.counts(); extends != 0 {
		t.Fatalf("renewed a lease it was waiting out")
	}
	h.clock.Advance(time.Second)
	eventually(t, "the report", func() bool { return len(h.reportsMade()) == 1 })
	if report := h.reportsMade()[0]; report.Outcome != dispatchapi.AmbiguityEffectObserved || report.Reference != "ch_1" {
		t.Fatalf("report = %+v", report)
	}
}

// TestWorkerAttestsBeforeClaimingAndBeforeExtending: an action requiring
// attestation is presented for before its claim, and again before an
// extension whose lease would outlive the attestation.
func TestWorkerAttestsBeforeClaimingAndBeforeExtending(t *testing.T) {
	h := newWorkerHarness(t, func(cfg *WorkerConfig) {
		cfg.RequiresAttestation = func(action string) bool { return action == "charge" }
		cfg.AttestationStatementFile, cfg.AttestationKeyFile = "statement", "key"
	})
	h.explorer.attestValidity = 90 * time.Second
	h.explorer.enqueue("a1", "charge", chargeInput, workerEpoch.Add(time.Hour))
	h.startGated()
	if calls := h.explorer.callLog(); strings.Join(calls[:3], ",") != "pull,attest,claim" {
		t.Fatalf("calls = %v", calls)
	}
	h.clock.Advance(30 * time.Second)
	eventually(t, "the extension", func() bool { _, _, n, _, _ := h.explorer.counts(); return n == 1 })
	calls := h.explorer.callLog()
	var sequence []string
	for _, call := range calls {
		if call == "attest" || call == "extend" {
			sequence = append(sequence, call)
		}
	}
	if strings.Join(sequence, ",") != "attest,attest,extend" {
		t.Fatalf("attest/extend order = %v", sequence)
	}
	h.release()
	eventually(t, "the action to succeed", func() bool { return h.state("a1") == dispatchapi.DispatchSucceeded })
}

// TestWorkerRefusesToClaimUnderAShortAttestation: an attestation expiring
// before a lease would end is not claimed under.
func TestWorkerRefusesToClaimUnderAShortAttestation(t *testing.T) {
	h := newWorkerHarness(t, func(cfg *WorkerConfig) {
		cfg.RequiresAttestation = func(string) bool { return true }
	})
	h.explorer.attestValidity = 30 * time.Second
	h.explorer.enqueue("a1", "charge", chargeInput, workerEpoch.Add(time.Hour))
	h.start()
	eventually(t, "an attestation", func() bool { h.explorer.mu.Lock(); defer h.explorer.mu.Unlock(); return h.explorer.attests > 0 })
	settle()
	if _, claims, _, _, _ := h.explorer.counts(); claims != 0 {
		t.Fatal("claimed under an attestation shorter than the lease")
	}
}

// TestWorkerPullsOnlyWithAFreeSlot: with every slot held, nothing is pulled.
func TestWorkerPullsOnlyWithAFreeSlot(t *testing.T) {
	h := newWorkerHarness(t, func(cfg *WorkerConfig) { cfg.MaxInFlight = 2 })
	for _, id := range []string{"a1", "a2", "a3"} {
		h.explorer.enqueue(id, "charge", chargeInput, workerEpoch.Add(time.Hour))
	}
	h.target.gate = make(chan struct{})
	t.Cleanup(h.release)
	h.start()
	for i := 0; i < 2; i++ {
		select {
		case <-h.target.entered:
		case <-time.After(10 * time.Second):
			t.Fatal("no request")
		}
	}
	settle()
	pulls, claims, _, _, _ := h.explorer.counts()
	h.clock.Advance(5 * time.Second)
	settle()
	if after, afterClaims, _, _, _ := h.explorer.counts(); after != pulls || claims != 2 || afterClaims != 2 {
		t.Fatalf("pulls %d→%d, claims %d→%d with every slot held", pulls, after, claims, afterClaims)
	}
	h.release()
	for i := 0; i < 3 && h.state("a3") != dispatchapi.DispatchSucceeded; i++ {
		settle()
		h.clock.Advance(MaxBackoff)
	}
	eventually(t, "all three", func() bool {
		return h.state("a1") == dispatchapi.DispatchSucceeded && h.state("a2") == dispatchapi.DispatchSucceeded &&
			h.state("a3") == dispatchapi.DispatchSucceeded
	})
}

// TestBackoffIsJitteredExponentialAndCapped.
func TestBackoffIsJitteredExponentialAndCapped(t *testing.T) {
	low := &backoff{base: time.Second, random: bytes.NewReader(make([]byte, 1024))}
	want := []time.Duration{500 * time.Millisecond, time.Second, 2 * time.Second, 4 * time.Second,
		8 * time.Second, 15 * time.Second, 15 * time.Second}
	for i, expected := range want {
		if got := low.next(); got != expected {
			t.Fatalf("step %d = %v, want %v", i, got, expected)
		}
	}
	random := &backoff{base: time.Second, random: nil}
	random.random = cryptoReader{}
	for i := 0; i < 50; i++ {
		got := random.next()
		if got < random.current/2 || got > random.current || got > MaxBackoff {
			t.Fatalf("delay %v outside [%v, %v]", got, random.current/2, random.current)
		}
	}
	random.reset()
	if random.current != 0 {
		t.Fatal("reset did not reset")
	}
}

type cryptoReader struct{}

func (cryptoReader) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = byte(time.Now().UnixNano() >> (i % 8))
	}
	return len(p), nil
}

// TestNewWorkerRequiresTheUnrecordedLog: the log's lock is the single-replica
// guarantee, so there is no worker without it.
func TestNewWorkerRequiresTheUnrecordedLog(t *testing.T) {
	h := newWorkerHarness(t, nil)
	cfg := h.cfg
	cfg.Unrecorded = nil
	if _, err := NewWorker(cfg); err == nil {
		t.Fatal("a worker without the unrecorded log")
	}
	_ = h.log.Close()
	_ = errors.New
	_ = context.Background
}
