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
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"sync/atomic"
	"testing"
	"time"

	dispatchapi "github.com/phrocker/shoal-oss/pkg/dispatch/api"
)

func (h *workerHarness) runResult() error {
	h.t.Helper()
	select {
	case err := <-h.done:
		h.done <- err
		return err
	case <-time.After(10 * time.Second):
		h.t.Fatal("the worker did not stop")
	}
	return nil
}

func (h *workerHarness) abandonedCount() float64 {
	for _, line := range h.logs.lines() {
		if line["event"] == "abandoned" {
			count, _ := line["abandoned_runs"].(float64)
			return count
		}
	}
	return -1
}

// TestADrainThatRunsOutWritesWhatMayHaveHappened: the target answered, and
// the completion hangs, ignoring its deadline, as a misbehaving dependency
// might. The drain gives up at the grace period less the exit margin — not at
// the grace period, by which time the kubelet's SIGKILL may already have
// landed — and the run, whose request reached the target, is written to the
// unrecorded log as outcome_unknown in that same instant. The abandonment is
// logged with its count, nothing more is reported for the run, and Run says
// so.
func TestADrainThatRunsOutWritesWhatMayHaveHappened(t *testing.T) {
	h := newWorkerHarness(t, nil)
	hang := make(chan struct{})
	t.Cleanup(func() { close(hang) })
	h.explorer.setGate(func(_ context.Context, op string) error {
		if op == "complete" {
			<-hang
		}
		return nil
	})
	h.explorer.enqueue("a1", "charge", chargeInput, workerEpoch.Add(time.Hour))
	h.startGated()
	sigterm := h.clock.Now()
	h.cancel()
	eventually(t, "draining", func() bool { _, reason := h.worker.Ready(); return reason == NotReadyDraining })
	h.release()
	settle()
	bound := GracePeriod(h.cfg.OperationTimeout, h.cfg.PlaneTimeout) - exitMargin
	// Stepped, so the renewal, which keeps the claim alive meanwhile, runs.
	for elapsed := time.Duration(0); elapsed < bound-time.Second; elapsed += time.Second {
		h.clock.Advance(time.Second)
		settle()
	}
	if h.worker.UnrecordedEntries() != 0 || h.abandonedCount() != -1 {
		t.Fatalf("abandoned a second before the drain bound (%s after SIGTERM)", bound)
	}
	h.clock.Advance(time.Second)
	if err := h.runResult(); !errors.Is(err, ErrDrainAbandoned) {
		t.Fatalf("Run = %v, want ErrDrainAbandoned", err)
	}
	entries := h.log.Entries()
	if len(entries) != 1 || string(entries[0].ActionID) != "a1" || entries[0].Fence != 1 ||
		entries[0].Outcome != dispatchapi.AmbiguityOutcomeUnknown || entries[0].RouteAction != "charge" ||
		string(entries[0].CorrelationID) != "trace-a1" {
		t.Fatalf("entries = %+v", entries)
	}
	if written := entries[0].LastAt.Sub(sigterm); written > bound {
		t.Fatalf("the abandonment wrote at %s after SIGTERM, past the drain bound %s", written, bound)
	}
	if count := h.abandonedCount(); count != 1 {
		t.Fatalf("abandoned count = %v:\n%s", count, h.logs)
	}
	if _, _, _, _, reports := h.explorer.counts(); reports != 0 {
		t.Fatalf("an abandoned run reported %d times", reports)
	}
	h.assertNoSecrets()
}

// TestAbandonWritesOnce: every abandoned run goes into one durable rewrite of
// the log, not one per run.
func TestAbandonWritesOnce(t *testing.T) {
	h := newWorkerHarness(t, nil)
	for _, id := range []string{"a1", "a2", "a3"} {
		h.explorer.enqueue(id, "charge", chargeInput, workerEpoch.Add(time.Hour))
	}
	run := func(id string) *claimRun {
		route, _ := h.cfg.Routes.Lookup("charge")
		return &claimRun{action: Action{ID: []byte(id), ClaimFence: 1,
			CorrelationID: []byte("trace-" + id)}, route: route, attempted: true}
	}
	for _, id := range []string{"a1", "a2", "a3"} {
		h.worker.runs[run(id)] = struct{}{}
	}
	var synced int
	previous := fileSyncer
	fileSyncer = func(file *os.File) error { synced++; return file.Sync() }
	t.Cleanup(func() { fileSyncer = previous })
	if abandoned := h.worker.abandon(); abandoned != 3 {
		t.Fatalf("abandoned %d", abandoned)
	}
	// One rewrite: the temporary file and the directory.
	if synced != 2 || h.log.Len() != 3 {
		t.Fatalf("%d fsyncs for %d entries, want one rewrite (2 fsyncs)", synced, h.log.Len())
	}
	_ = h.log.Close()
}

// TestADrainThatRunsOutWritesNothingForAnUnsentRun: a run abandoned before
// any request left is counted, and not written: nothing can have happened.
func TestADrainThatRunsOutWritesNothingForAnUnsentRun(t *testing.T) {
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
	t.Cleanup(func() { close(hold) })
	h.explorer.enqueue("a1", "charge", chargeInput, workerEpoch.Add(time.Hour))
	h.start()
	<-entered
	h.cancel()
	eventually(t, "draining", func() bool { _, reason := h.worker.Ready(); return reason == NotReadyDraining })
	// The drain arms its bound after it reports draining: wait for the timer
	// before passing it, or the advance can land first and nothing fires.
	bound := DrainBound(h.cfg.OperationTimeout, h.cfg.PlaneTimeout)
	eventually(t, "the drain's bound", func() bool { return h.clock.waiting(bound) })
	h.clock.Advance(bound)
	if err := h.runResult(); !errors.Is(err, ErrDrainAbandoned) {
		t.Fatalf("Run = %v", err)
	}
	if h.worker.UnrecordedEntries() != 0 {
		t.Fatalf("an unsent run was written: %+v", h.log.Entries())
	}
	if count := h.abandonedCount(); count != 1 {
		t.Fatalf("abandoned count = %v", count)
	}
}

// TestTheStartUpRetryStopsOnShutdown: SIGTERM during the retry of held
// reports stops it; the entries not reached stay on disk.
func TestTheStartUpRetryStopsOnShutdown(t *testing.T) {
	clock := newTimerClock()
	dir := t.TempDir()
	fillUnrecorded(t, dir, 10)
	h := &workerHarness{t: t, clock: clock, explorer: newFakeExplorer(t, clock),
		target: newFakeTarget(t), dir: dir, logs: &lockedBuffer{}}
	h.build(nil)
	h.explorer.onAmbiguity = func(n int, _ []byte, _ AmbiguityReport) (Action, error, bool) {
		if n == 1 {
			h.cancel()
		}
		return Action{}, &DispatchError{Op: "ambiguity", Kind: DispatchAmbiguityUnrecorded, Status: 404}, true
	}
	h.start()
	if err := h.runResult(); err != nil {
		t.Fatalf("Run = %v", err)
	}
	if reports := h.reportsMade(); len(reports) != 1 {
		t.Fatalf("%d retries after shutdown began, want 1", len(reports))
	}
	if h.log.Len() != 10 {
		t.Fatalf("%d entries left, want all 10", h.log.Len())
	}
	if pulls, _, _, _, _ := h.explorer.counts(); pulls != 0 {
		t.Fatalf("pulled %d times after shutdown", pulls)
	}
}

// TestStoppingTheRenewalCancelsItsCallInFlight: the renewal's extension is in
// flight, blocked until its context ends, when a completion during the drain
// comes back indeterminate. The fallback report starts at once — on the
// worker's clock, with no time passing — because stopping the renewal cancels
// its call rather than waiting out the call's timeout, which the grace period
// does not budget for.
func TestStoppingTheRenewalCancelsItsCallInFlight(t *testing.T) {
	h := newWorkerHarness(t, nil)
	extending := make(chan struct{}, 1)
	h.explorer.setGate(func(ctx context.Context, op string) error {
		switch op {
		case "extend":
			extending <- struct{}{}
			<-ctx.Done()
			return &DispatchError{Op: "extend", Kind: DispatchIndeterminate}
		case "complete":
			return &DispatchError{Op: "complete", Kind: DispatchIndeterminate}
		}
		return nil
	})
	h.explorer.enqueue("a1", "charge", chargeInput, workerEpoch.Add(time.Hour))
	h.startGated()
	h.clock.Advance(30 * time.Second)
	select {
	case <-extending:
	case <-time.After(10 * time.Second):
		t.Fatal("no extension started")
	}
	h.cancel()
	eventually(t, "draining", func() bool { _, reason := h.worker.Ready(); return reason == NotReadyDraining })
	stopped := h.clock.Now()
	h.release()
	eventually(t, "the fallback report", func() bool { return len(h.explorer.reportTimes()) > 0 })
	if started := h.explorer.reportTimes()[0]; started.Sub(stopped) > 0 {
		t.Fatalf("the report started %s after the completion, waiting on the renewal", started.Sub(stopped))
	}
}

// TestTheWorstShutdownPathFitsTheDrainBound drives the real worker down the
// slowest path a drain can take, with every call blocking until its own
// timeout on the worker's clock: SIGTERM with a request in flight; the target
// never answers, so the request runs out its operation timeout; the
// completion runs out its whole budget and is indeterminate; the renewal, due
// in the middle of it, blocks too; each fallback report runs out its window;
// and the report lands in the unrecorded log. That last write, measured from
// SIGTERM on the same clock, must come no later than the grace period less
// the exit margin. The bound is measured here, not restated from the
// formula.
func TestTheWorstShutdownPathFitsTheDrainBound(t *testing.T) {
	const operation, plane = 25 * time.Second, time.Second
	h := newWorkerHarness(t, func(cfg *WorkerConfig) {
		cfg.OperationTimeout, cfg.PlaneTimeout, cfg.Renew = operation, plane, false
	})
	sent := make(chan struct{}, 1)
	h.target.script = func(_ int, _ http.ResponseWriter, r *http.Request) bool {
		sent <- struct{}{}
		<-r.Context().Done()
		return true
	}
	h.explorer.enqueue("a1", "charge", chargeInput, workerEpoch.Add(time.Hour))
	h.start()
	select {
	case <-sent:
	case <-time.After(10 * time.Second):
		t.Fatal("no request")
	}
	settle()
	h.explorer.setGate(func(ctx context.Context, op string) error {
		<-ctx.Done()
		if op == "ambiguity" {
			return &DispatchError{Op: op, Kind: DispatchIndeterminate}
		}
		return &DispatchError{Op: op, Kind: DispatchIndeterminate}
	})
	h.clock.Advance(2 * time.Second)
	settle()
	sigterm := h.clock.Now()
	h.cancel()
	bound := GracePeriod(operation, plane) - exitMargin
	for step := 0; h.worker.UnrecordedEntries() == 0 && step < 1000; step++ {
		h.clock.Advance(100 * time.Millisecond)
		time.Sleep(3 * time.Millisecond)
	}
	if err := h.runResult(); err != nil {
		t.Fatalf("Run = %v: the drain did not finish on its own", err)
	}
	entries := h.log.Entries()
	if len(entries) != 1 || entries[0].DispatchError != DispatchIndeterminate ||
		entries[0].Outcome != dispatchapi.AmbiguityOutcomeUnknown {
		t.Fatalf("entries = %+v, want the fallback report's own entry", entries)
	}
	if reports := h.explorer.reportTimes(); len(reports) != FallbackReportAttempts {
		t.Fatalf("%d report attempts, want %d", len(reports), FallbackReportAttempts)
	}
	took := entries[0].LastAt.Sub(sigterm)
	if took > bound {
		t.Fatalf("SIGTERM to the last write took %s, past the drain bound %s", took, bound)
	}
	t.Logf("SIGTERM to the last write: %s of a %s bound", took, bound)
	// The path really is the worst one: it uses nearly all of the bound.
	if took < bound-3*time.Second {
		t.Fatalf("SIGTERM to the last write took %s; the path did not block as intended", took)
	}
}

// settleScenario runs a claim whose renewal at 30s is in flight, blocked
// until the worker gives up on it, when the completion comes back
// indeterminate. The explorer applies the renewal anyway, delay after the
// worker gives up, and never answers it. The claim's own answer takes
// claimDelay to arrive. Re-pulls are refused, so only this claim reports.
func settleScenario(t *testing.T, skew, claimDelay, delay, deadline time.Duration) *workerHarness {
	t.Helper()
	h := newWorkerHarness(t, nil)
	h.explorer.skew = skew
	h.explorer.loseExtendAnswer, h.explorer.extendApplyDelay = true, delay
	var claimed atomic.Bool
	claiming := make(chan struct{}, 1)
	h.explorer.setGate(func(ctx context.Context, op string) error {
		switch op {
		case "pull":
			if claimed.Load() {
				return &DispatchError{Op: "pull", Kind: DispatchUnavailable}
			}
		case "claim":
			claimed.Store(true)
			if claimDelay > 0 {
				claiming <- struct{}{}
				<-h.clock.After(claimDelay)
			}
		case "extend":
			<-ctx.Done()
		case "complete":
			return &DispatchError{Op: "complete", Kind: DispatchIndeterminate}
		}
		return nil
	})
	// The deadline is the explorer's timestamp, on the explorer's clock.
	h.explorer.enqueue("a1", "charge", chargeInput, workerEpoch.Add(-skew).Add(deadline))
	h.target.gate = make(chan struct{})
	t.Cleanup(h.release)
	h.start()
	if claimDelay > 0 {
		<-claiming
		settle()
		h.clock.Advance(claimDelay)
	}
	select {
	case <-h.target.entered:
	case <-time.After(10 * time.Second):
		t.Fatal("the target saw no request")
	}
	settle()
	h.clock.Advance(30*time.Second - h.clock.Now().Sub(workerEpoch))
	settle()
	h.release()
	return h
}

// assertNoReportUntil steps the clock to each instant (from the epoch) and
// checks no report has been made.
func (h *workerHarness) assertNoReportUntil(instants ...time.Duration) {
	h.t.Helper()
	for _, at := range instants {
		if step := at - h.clock.Now().Sub(workerEpoch); step > 0 {
			h.clock.Advance(step)
		}
		settle()
		if reports := h.explorer.reportTimes(); len(reports) != 0 {
			h.t.Fatalf("reported at %s", reports[0].Sub(workerEpoch))
		}
	}
}

// TestTheSettleWaitCoversAnExtensionOfUnknownOutcome: stopping the renewal
// cancels it, and the explorer applies it anyway (lease to 90s); the worker
// never reads that. Nothing on the worker's side bounds when the explorer
// applied it, so the fallback report waits for the latest local instant of
// the action's deadline (120s), not the old anchored end (60s).
func TestTheSettleWaitCoversAnExtensionOfUnknownOutcome(t *testing.T) {
	h := settleScenario(t, 0, 0, 0, 120*time.Second)
	eventually(t, "the cancelled extension to apply", func() bool {
		return h.explorer.record("a1").ClaimLeaseUntil.Equal(workerEpoch.Add(90 * time.Second))
	})
	h.assertNoReportUntil(61*time.Second, 90*time.Second, 119*time.Second)
	h.clock.Advance(time.Second) // 120s
	eventually(t, "the report", func() bool { return len(h.explorer.reportTimes()) == 1 })
	if at := h.explorer.reportTimes()[0].Sub(workerEpoch); at != 120*time.Second {
		t.Fatalf("reported at %s, want the deadline's latest local instant (120s)", at)
	}
	count := 0
	for _, line := range h.logs.lines() {
		if line["event"] == "dispatch_error" && line["fence"] == float64(1) {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("%d dispatch_error events for the claim; only the completion's is expected:\n%s", count, h.logs)
	}
}

// TestTheSettleWaitHoldsUnderSkew: the explorer's clock runs 30s behind the
// worker's, the claim's answer takes 2s to arrive, and the renewal the worker
// gave up on at 35s (its own timeout) is applied 40s after that — as an
// explorer that judged the request's deadline on its own clock, or had the
// write already under way, would. That renewal is clamped to the action's
// deadline, so the lease really ends at the deadline: 130s on the worker's
// clock. DeadlineLocal, the early estimate, says 128s; only the late side,
// DeadlineLocalLatest, is safe. No report may precede 130s; it comes then.
func TestTheSettleWaitHoldsUnderSkew(t *testing.T) {
	h := settleScenario(t, 30*time.Second, 2*time.Second, 40*time.Second, 130*time.Second)
	h.assertNoReportUntil(35*time.Second, 76*time.Second)
	explorerEpoch := workerEpoch.Add(-30 * time.Second)
	if got := h.explorer.record("a1").ClaimLeaseUntil.Sub(explorerEpoch); got != 130*time.Second {
		t.Fatalf("the late renewal granted %s (explorer time), want the deadline", got)
	}
	h.assertNoReportUntil(96*time.Second, 115*time.Second, 128*time.Second, 129*time.Second)
	h.clock.Advance(time.Second) // 130s
	eventually(t, "the report", func() bool { return len(h.explorer.reportTimes()) == 1 })
	if at := h.explorer.reportTimes()[0].Sub(workerEpoch); at != 130*time.Second {
		t.Fatalf("reported at %s, want 130s", at)
	}
}

// TestAnchorAnswer: the late side of the deadline is the answer's arrival
// plus the server's own deadline − updated_at, whatever the server's offset.
func TestAnchorAnswer(t *testing.T) {
	server := workerEpoch.Add(-time.Hour) // any offset
	claim := ClaimTimes{UpdatedAt: server, ClaimLeaseUntil: server.Add(time.Minute),
		Deadline: server.Add(10 * time.Minute)}
	sent, received := workerEpoch, workerEpoch.Add(3*time.Second)
	anchored, err := AnchorAnswer(sent, received, claim)
	if err != nil {
		t.Fatal(err)
	}
	if !anchored.DeadlineLocal.Equal(sent.Add(10*time.Minute)) ||
		!anchored.DeadlineLocalLatest.Equal(received.Add(10*time.Minute)) ||
		!anchored.LeaseLocal.Equal(sent.Add(time.Minute)) {
		t.Fatalf("anchored = %+v", anchored)
	}
	if _, err := AnchorAnswer(received, sent, claim); err == nil {
		t.Fatal("an answer before its request")
	}
}

// TestAHardStopAccountsForEveryRequest: the loop is inside the claim of a
// second action when the hard stop takes its snapshot, and the stop's write
// is held on a slow disk. The claim then succeeds, after the snapshot; the
// worker must not register or work it, so every request that left is one the
// write accounts for — the in-flight run, and nothing else.
func TestAHardStopAccountsForEveryRequest(t *testing.T) {
	h := newWorkerHarness(t, nil)
	gate := make(chan struct{})
	h.target.mu.Lock()
	h.target.gate = gate
	h.target.mu.Unlock()
	t.Cleanup(func() { close(gate) })
	claimHeld, claimRelease := make(chan struct{}, 1), make(chan struct{})
	var claims atomic.Int32
	h.explorer.setGate(func(ctx context.Context, op string) error {
		if op == "claim" && claims.Add(1) == 2 {
			claimHeld <- struct{}{}
			<-claimRelease
		}
		return nil
	})
	h.explorer.enqueue("a1", "charge", chargeInput, workerEpoch.Add(time.Hour))
	h.explorer.enqueue("a2", "charge", chargeInput, workerEpoch.Add(time.Hour))
	h.start()
	for what, ch := range map[string]chan struct{}{"a1's request": h.target.entered, "a2's claim": claimHeld} {
		select {
		case <-ch:
		case <-time.After(10 * time.Second):
			t.Fatalf("never saw %s", what)
		}
	}

	blocked, unblock := make(chan struct{}, 1), make(chan struct{})
	previous := fileSyncer
	fileSyncer = func(file *os.File) error {
		select {
		case blocked <- struct{}{}:
		default:
		}
		<-unblock
		return file.Sync()
	}
	t.Cleanup(func() { fileSyncer = previous })
	stopped := make(chan int, 1)
	go func() { stopped <- h.worker.HardStop() }()
	select {
	case <-blocked:
	case <-time.After(10 * time.Second):
		t.Fatal("the hard stop never wrote")
	}
	// The snapshot is taken and the write is under way: a2's claim answers
	// now, after it.
	close(claimRelease)
	settle()
	close(unblock)
	if abandoned := <-stopped; abandoned != 1 {
		t.Fatalf("abandoned %d", abandoned)
	}
	if err := h.runResult(); err == nil {
		t.Fatal("Run reported a clean stop after a hard stop")
	}
	settle()
	if _, requests, _ := h.target.stats(); requests != 1 {
		t.Fatalf("%d target requests for %d unrecorded entries", requests, h.log.Len())
	}
	entries := h.log.Entries()
	if len(entries) != 1 || string(entries[0].ActionID) != "a1" ||
		entries[0].Outcome != dispatchapi.AmbiguityOutcomeUnknown {
		t.Fatalf("entries = %+v", entries)
	}
	// a2 was claimed and never worked: nothing past its claim is logged.
	a2 := base64.RawURLEncoding.EncodeToString([]byte("a2"))
	for _, line := range h.logs.lines() {
		if line["action_id"] == a2 && line["event"] != string(EventClaimed) {
			t.Fatalf("a2, claimed after the stop's snapshot, was worked: %v", line)
		}
	}
}

// TestAStoppedWorkerSendsNothing: a run the worker stopped before abandon
// saw it — registered, not yet abandoned — sends nothing.
func TestAStoppedWorkerSendsNothing(t *testing.T) {
	h := newWorkerHarness(t, nil)
	t.Cleanup(func() { _ = h.log.Close() })
	route, _ := h.cfg.Routes.Lookup("charge")
	key := testExecutorKey(t, "a1")
	run := &claimRun{action: Action{ID: []byte("a1"), ClaimFence: 1, ExecutorKey: key}, route: route}
	bound, err := h.cfg.Binder.Bind(route, json.RawMessage(chargeInput), key)
	if err != nil {
		t.Fatal(err)
	}
	h.worker.killed.Store(true)
	observation, _, _, _ := h.worker.attempt(run, bound, "Authorization", "Bearer sk_test")
	if _, requests, _ := h.target.stats(); requests != 0 || run.attempted || observation.Written {
		t.Fatalf("a stopped worker sent: %d requests, attempted %v", requests, run.attempted)
	}
}

// TestAnAttemptedRunLeavesRunsOnlyOnceSettled is the invariant on Worker.runs
// directly: a run whose request reached the target and whose goroutine then
// exits unsettled — here because the worker was killed mid-request — stays
// in runs, so a HardStop after it still finds and writes it; and the write is
// what removes it.
func TestAnAttemptedRunLeavesRunsOnlyOnceSettled(t *testing.T) {
	h := newWorkerHarness(t, nil)
	gate := make(chan struct{})
	h.target.mu.Lock()
	h.target.gate = gate
	h.target.mu.Unlock()
	t.Cleanup(func() { close(gate) })
	h.explorer.enqueue("a1", "charge", chargeInput, workerEpoch.Add(time.Hour))
	h.start()
	select {
	case <-h.target.entered:
	case <-time.After(10 * time.Second):
		t.Fatal("the target saw no request")
	}
	// Cancel everything without writing, as a crash would, and let the
	// run's goroutine exit.
	h.worker.Kill()
	_ = h.runResult()
	eventually(t, "the run's goroutine to exit", func() bool {
		done := make(chan struct{})
		go func() { h.worker.work.Wait(); close(done) }()
		select {
		case <-done:
			return true
		case <-time.After(10 * time.Millisecond):
			return false
		}
	})
	if n := h.worker.InFlight(); n != 1 {
		t.Fatalf("an attempted, unsettled run left runs: %d held", n)
	}
	// The write is what removes it. (Run has returned, so HardStop is now a
	// no-op; abandon is the write both stop paths share.)
	if abandoned := h.worker.abandon(); abandoned != 1 {
		t.Fatalf("abandoned %d", abandoned)
	}
	if n := h.worker.InFlight(); n != 0 || h.log.Len() != 1 {
		t.Fatalf("after the write: %d held, %d entries", n, h.log.Len())
	}
}

// TestAHardStopAfterRunHasReturnedDoesNothing: the caller may close the log
// once Run returns, and a second signal can arrive after that. It must not
// touch the log, even with a run still held (here one a Kill left behind).
func TestAHardStopAfterRunHasReturnedDoesNothing(t *testing.T) {
	h := newWorkerHarness(t, nil)
	gate := make(chan struct{})
	h.target.mu.Lock()
	h.target.gate = gate
	h.target.mu.Unlock()
	t.Cleanup(func() { close(gate) })
	h.explorer.enqueue("a1", "charge", chargeInput, workerEpoch.Add(time.Hour))
	h.start()
	select {
	case <-h.target.entered:
	case <-time.After(10 * time.Second):
		t.Fatal("the target saw no request")
	}
	h.worker.Kill()
	_ = h.runResult()
	eventually(t, "the run to be held", func() bool { return h.worker.InFlight() == 1 })
	_ = h.log.Close()
	if abandoned := h.worker.HardStop(); abandoned != 0 {
		t.Fatalf("a HardStop after Run returned abandoned %d, against a closed log", abandoned)
	}
}

// unwritableReport drives one run to a lost-fence report the explorer will
// not record, with the unrecorded log's writes failing until heal is called:
// the report's own append fails, and the run is left attempted and unsettled.
func unwritableReport(t *testing.T) (h *workerHarness, heal func()) {
	t.Helper()
	h = newWorkerHarness(t, nil)
	h.explorer.onExtend = func(int, []byte, ExtendRequest) (Action, error, bool) {
		return Action{}, &DispatchError{Op: "extend", Kind: DispatchFenceLost, Status: 409}, true
	}
	h.explorer.onAmbiguity = func(int, []byte, AmbiguityReport) (Action, error, bool) {
		return Action{}, &DispatchError{Op: "ambiguity", Kind: DispatchAmbiguityUnrecorded, Status: 400}, true
	}
	var broken atomic.Bool
	broken.Store(true)
	previous := fileSyncer
	fileSyncer = func(file *os.File) error {
		if broken.Load() {
			return errors.New("disk")
		}
		return file.Sync()
	}
	t.Cleanup(func() { fileSyncer = previous })
	h.explorer.enqueue("a1", "charge", chargeInput, workerEpoch.Add(time.Hour))
	h.startGated()
	h.clock.Advance(30 * time.Second)
	eventually(t, "the fence to be lost", func() bool { return h.logs.has("fence_lost") })
	h.release()
	eventually(t, "the failed write", func() bool {
		for _, line := range h.logs.lines() {
			if line["event"] == string(EventDispatch) && line["ambiguity"] == string(dispatchapi.AmbiguityEffectObserved) {
				return true
			}
		}
		return false
	})
	if h.log.Len() != 0 || h.worker.InFlight() != 1 {
		t.Fatalf("after the failed write: %d entries, %d held", h.log.Len(), h.worker.InFlight())
	}
	return h, func() { broken.Store(false) }
}

// TestTheDrainRetriesAReportItCouldNotWrite: a report whose append failed
// leaves its run held; the drain writes that report's own entry — the
// outcome and reference it had, not an outcome_unknown in its place — and
// Run returns cleanly only once it is written.
func TestTheDrainRetriesAReportItCouldNotWrite(t *testing.T) {
	h, heal := unwritableReport(t)
	heal()
	if err := h.stop(); err != nil {
		t.Fatalf("Run = %v after the retry succeeded", err)
	}
	entries := h.log.Entries()
	if len(entries) != 1 || string(entries[0].ActionID) != "a1" ||
		entries[0].Outcome != dispatchapi.AmbiguityEffectObserved || entries[0].Reference != "ch_1" {
		t.Fatalf("entries = %+v", entries)
	}
	if n := h.worker.InFlight(); n != 0 {
		t.Fatalf("%d runs still held after the retry", n)
	}
}

// TestADrainThatCannotWriteIsNotClean: if the retry fails too, Run says so;
// it never returns nil with an effect on no record and in no log.
func TestADrainThatCannotWriteIsNotClean(t *testing.T) {
	h, _ := unwritableReport(t)
	if err := h.stop(); !errors.Is(err, ErrUnrecordedUnwritten) {
		t.Fatalf("Run = %v, want ErrUnrecordedUnwritten", err)
	}
}

// TestAHardStopWhoseWriteFailsIsNotAKill: Run reports the failed write ahead
// of the hard stop itself, so the caller never reads it as "every sent
// request accounted for".
func TestAHardStopWhoseWriteFailsIsNotAKill(t *testing.T) {
	h := newWorkerHarness(t, nil)
	gate := make(chan struct{})
	h.target.mu.Lock()
	h.target.gate = gate
	h.target.mu.Unlock()
	t.Cleanup(func() { close(gate) })
	h.explorer.enqueue("a1", "charge", chargeInput, workerEpoch.Add(time.Hour))
	h.start()
	select {
	case <-h.target.entered:
	case <-time.After(10 * time.Second):
		t.Fatal("the target saw no request")
	}
	previous := fileSyncer
	fileSyncer = func(*os.File) error { return errors.New("disk") }
	t.Cleanup(func() { fileSyncer = previous })
	h.cancel()
	h.worker.HardStop()
	if err := h.runResult(); !errors.Is(err, ErrUnrecordedUnwritten) {
		t.Fatalf("Run = %v, want ErrUnrecordedUnwritten", err)
	}
}
