// Licensed to the Apache Software Foundation (ASF) under one or more
// contributor license agreements. See the NOTICE file distributed with this
// work for additional information regarding copyright ownership.
package fleet

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/phrocker/shoal-oss/pkg/explorer/auth"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

// completionFixture is a dispatch service with one claimed action, built for
// the remote path: no executor ever runs, because the point of CompleteClaim is
// that the work happened somewhere this process cannot see.
type completionFixture struct {
	service   *DispatchService
	store     *memoryDispatchStore
	authority *auth.Authority
	claimed   ActionRecord
	ctx       context.Context
	// clock drives both the registry and the dispatch service, so advancing it
	// expires a claim lease.
	clock *time.Time
}

func newCompletionFixture(t *testing.T, effects Effects) *completionFixture {
	t.Helper()
	return newCompletionFixtureWithExecutor(t, effects, &remoteBoundExecutor{})
}

func newCompletionFixtureWithExecutor(t *testing.T, effects Effects, executor any) *completionFixture {
	t.Helper()
	now := time.Date(2026, 9, 6, 12, 0, 0, 0, time.UTC)
	clock := now
	authority, err := auth.NewAuthorityWithClock(func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	registryStore := newMemoryStore()
	registry, err := NewService(Config{
		Store: registryStore, Resolver: authority.Resolver(), Recorder: &memoryRecorder{},
		Snapshots: fixedSnapshot{now},
		Executors: executorMap{"exec": executor},
		Clock:     func() time.Time { return clock },
	})
	if err != nil {
		t.Fatal(err)
	}
	descriptor := dispatchDescriptor(now)
	descriptor.Capabilities[0].Actions[0].Effects = effects
	// The shared descriptor's lease and the enqueued action's deadline both sit
	// one hour out, so advancing the clock far enough to expire the action
	// deadline expires the agent lease first and the action is refused as
	// not-found before its deadline is ever consulted. A longer lease keeps the
	// two bounds independent, which is what lets the deadline case below test
	// the deadline.
	descriptor.LeaseExpiresAt = now.Add(24 * time.Hour)
	registryStore.records["agent"] = Stored{Descriptor: descriptor}
	store := newMemoryDispatchStore()
	service, err := NewDispatchService(DispatchConfig{
		Store: store, Registry: registry, Resolver: authority.Resolver(),
		Recorder: &dispatchRecorder{}, Events: dispatchEvents{},
		Clock: func() time.Time { return clock },
	})
	if err != nil {
		t.Fatal(err)
	}
	decision := dispatchDecision(t, "owner", "actor", "request",
		auth.OperationDispatch, auth.OperationInvoke)
	ctx := bindDecision(t, authority, decision)
	queued, err := service.Enqueue(ctx, dispatchEnqueue(now, "request"))
	if err != nil {
		t.Fatal(err)
	}
	claimed, err := service.Claim(ctx, ClaimRequest{
		ID: queued.ID, ExpectedVersion: queued.Version, ClaimID: []byte("claim"),
		Lease: time.Minute, Context: dispatchContext(now, "request"),
	})
	if err != nil {
		t.Fatal(err)
	}
	return &completionFixture{
		service: service, store: store, authority: authority,
		claimed: claimed, ctx: ctx, clock: &clock,
	}
}

// completion is a well-formed report from the worker holding the claim.
func (f *completionFixture) completion() CompletionRequest {
	now := time.Date(2026, 9, 6, 12, 0, 0, 0, time.UTC)
	return CompletionRequest{
		ID: f.claimed.ID, ExpectedVersion: f.claimed.Version, ClaimID: []byte("claim"),
		Result:  ExecutionResult{Output: json.RawMessage(`{"ok":true}`)},
		Context: dispatchContext(now, "request"),
	}
}

// assertStillClaimed proves a refused completion wrote nothing.
func (f *completionFixture) assertStillClaimed(t *testing.T) {
	t.Helper()
	stored, err := f.store.GetAction(f.ctx, f.claimed.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.State != DispatchClaimed || stored.Version != f.claimed.Version {
		t.Fatalf("a refused completion wrote something: %#v", stored)
	}
}

// remoteBoundExecutor is what a host binds for work performed out of process.
//
// The effect boundary (#381) still applies to the remote path: an action
// declaring an external effect cannot resolve to a reference whose ceiling is
// evidence-only, so the host must say that this reference may cause external
// effects. Completion does not bypass that — it is reached only through an
// action that already resolved.
//
// Execute is never called. A remote worker pulls and claims, performs the work
// itself, and reports through CompleteClaim; nothing in this process runs it.
// Calling it is a test failure rather than a no-op, so a test that accidentally
// takes the in-process path says so instead of quietly passing.
type remoteBoundExecutor struct{ executed bool }

func (e *remoteBoundExecutor) Execute(context.Context, Invocation) (ExecutionResult, error) {
	e.executed = true
	return ExecutionResult{}, shoal.NewError(
		shoal.ErrorInternal, "the in-process executor ran for remote work")
}

func (*remoteBoundExecutor) MaxEffects() Effects {
	return Effects{EffectEgressesContent, EffectMutatesExternal, EffectReadsCorpus}
}

// TestCompleteClaimDrivesARemoteActionToSucceeded is the loop #384 is about:
// a worker outside this process claims, performs the work, and reports. Before
// this route existed the report had nowhere to go, the lease expired, and the
// action returned to the queue as though nothing had happened — while the
// effect had already occurred.
func TestCompleteClaimDrivesARemoteActionToSucceeded(t *testing.T) {
	fixture := newCompletionFixture(t, Effects{EffectMutatesExternal})

	completed, err := fixture.service.CompleteClaim(fixture.ctx, fixture.completion())
	if err != nil {
		t.Fatalf("CompleteClaim: %v", err)
	}
	if completed.State != DispatchSucceeded {
		t.Fatalf("state = %q, want %q", completed.State, DispatchSucceeded)
	}
	if string(completed.Output) != `{"ok":true}` {
		t.Fatalf("output = %s", completed.Output)
	}
	if completed.Version != fixture.claimed.Version+1 {
		t.Fatalf("version = %d, want %d", completed.Version, fixture.claimed.Version+1)
	}
	if !completed.EffectPossible {
		t.Fatal("a completed external effect is not marked possible")
	}
	// The durable record, not just the returned one.
	stored, err := fixture.store.GetAction(fixture.ctx, fixture.claimed.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.State != DispatchSucceeded || string(stored.Output) != `{"ok":true}` {
		t.Fatalf("stored = %#v", stored)
	}
	if stored.ExecutionFingerprint == (auth.Fingerprint{}) {
		t.Fatal("the terminal record carries no execution fingerprint, so the " +
			"authority the work was recorded under is not pinned")
	}
}

// TestCompleteClaimRecordsAReportedFailure covers the other terminal state.
func TestCompleteClaimRecordsAReportedFailure(t *testing.T) {
	fixture := newCompletionFixture(t, Effects{EffectMutatesExternal})

	request := fixture.completion()
	request.Failed = true
	request.Result = ExecutionResult{ErrorCode: "gateway_refused"}
	completed, err := fixture.service.CompleteClaim(fixture.ctx, request)
	// Recording the report is what this operation does, and it did it. The
	// failure is the work's outcome, carried in State and ErrorCode below —
	// answering with an error instead told a worker that had just performed an
	// irreversible effect that its report was refused (#492).
	if err != nil {
		t.Fatalf("a durably recorded failure was answered as a refusal: %v", err)
	}
	if completed.State != DispatchFailed || completed.ErrorCode != "gateway_refused" {
		t.Fatalf("failure record = %#v", completed)
	}
	stored, storeErr := fixture.store.GetAction(fixture.ctx, fixture.claimed.ID)
	if storeErr != nil || stored.State != DispatchFailed ||
		stored.ErrorCode != "gateway_refused" {
		t.Fatalf("stored failure = %#v, %v", stored, storeErr)
	}
}

// TestCompleteClaimRefusesAFailureWithNoReason keeps the report honest. A
// failure with no error code would commit a terminal state that says nothing
// about why, which is the one thing the report was for.
func TestCompleteClaimRefusesAFailureWithNoReason(t *testing.T) {
	fixture := newCompletionFixture(t, Effects{EffectMutatesExternal})

	request := fixture.completion()
	request.Failed = true
	request.Result = ExecutionResult{}
	if _, err := fixture.service.CompleteClaim(fixture.ctx, request); !shoal.IsErrorCode(err, shoal.ErrorInvalidArgument) {
		t.Fatalf("reasonless failure = %v", err)
	}
	fixture.assertStillClaimed(t)
}

// TestCompleteClaimRefusesAStaleClaim is the fence. A worker whose lease
// expired while it was working must not be able to write this record: by then
// the action may have been reclaimed and run again by someone else, and the
// stale report would overwrite the newer run's outcome.
func TestCompleteClaimRefusesAStaleClaim(t *testing.T) {
	for _, probe := range []struct {
		name   string
		mutate func(*CompletionRequest)
	}{
		{"a different claim ID", func(r *CompletionRequest) { r.ClaimID = []byte("other") }},
		{"a stale version", func(r *CompletionRequest) { r.ExpectedVersion-- }},
		{"a future version", func(r *CompletionRequest) { r.ExpectedVersion += 2 }},
	} {
		t.Run(probe.name, func(t *testing.T) {
			fixture := newCompletionFixture(t, Effects{EffectMutatesExternal})
			request := fixture.completion()
			probe.mutate(&request)
			if _, err := fixture.service.CompleteClaim(fixture.ctx, request); !errors.Is(err, ErrClaimLost) {
				t.Fatalf("completion under %s = %v, want ErrClaimLost", probe.name, err)
			}
			fixture.assertStillClaimed(t)
		})
	}
}

// TestCompleteClaimValidatesOutputAsTheInProcessPathDoes is the "not weaker"
// requirement. A remote worker must not be able to record output that an
// in-process executor could not.
func TestCompleteClaimValidatesOutputAsTheInProcessPathDoes(t *testing.T) {
	fixture := newCompletionFixture(t, Effects{EffectMutatesExternal})

	request := fixture.completion()
	// The declared OutputSchema requires a boolean "ok" and forbids anything
	// else.
	request.Result.Output = json.RawMessage(`{"ok":"yes","extra":1}`)
	completed, err := fixture.service.CompleteClaim(fixture.ctx, request)
	// Not "accepted": the action is terminally failed, which the record says.
	// The worker reads its own rejection off ErrorCode rather than off a
	// status that also means its request never happened (#492).
	if err != nil {
		t.Fatalf("a committed invalid_executor_output was answered as a "+
			"refusal, so the worker cannot tell it from a rejected report: %v",
			err)
	}
	if completed.State != DispatchFailed || completed.ErrorCode != "invalid_executor_output" {
		t.Fatalf("invalid output record = %#v", completed)
	}
	stored, storeErr := fixture.store.GetAction(fixture.ctx, fixture.claimed.ID)
	if storeErr != nil || stored.State != DispatchFailed {
		t.Fatalf("stored = %#v, %v", stored, storeErr)
	}
	if len(stored.Output) != 0 {
		t.Fatalf("the rejected output was recorded anyway: %s", stored.Output)
	}
}

// TestCompleteClaimValidatesEvidenceAsTheInProcessPathDoes is the same
// requirement for evidence, which is the half that reaches the audit record.
func TestCompleteClaimValidatesEvidenceAsTheInProcessPathDoes(t *testing.T) {
	fixture := newCompletionFixture(t, Effects{EffectMutatesExternal})

	request := fixture.completion()
	// Evidence with no snapshot pin: there is nothing to say what the evidence
	// was read against, so it cannot be verified later.
	request.Result.Evidence = []EvidenceRef{{Kind: "document"}}
	completed, err := fixture.service.CompleteClaim(fixture.ctx, request)
	// As above: committed as failed, not refused (#492).
	if err != nil {
		t.Fatalf("a committed invalid_executor_evidence was answered as a "+
			"refusal: %v", err)
	}
	if completed.State != DispatchFailed || completed.ErrorCode != "invalid_executor_evidence" {
		t.Fatalf("invalid evidence record = %#v", completed)
	}
	stored, storeErr := fixture.store.GetAction(fixture.ctx, fixture.claimed.ID)
	if storeErr != nil || len(stored.Evidence) != 0 {
		t.Fatalf("rejected evidence was recorded anyway: %#v, %v", stored, storeErr)
	}
}

// TestCompleteClaimReplayReturnsTheCommittedRecord covers the worker that
// commits and then loses its response. Retrying must not look like a conflict
// against its own write, or a worker with an unreliable connection can never
// learn that its work landed.
func TestCompleteClaimReplayReturnsTheCommittedRecord(t *testing.T) {
	fixture := newCompletionFixture(t, Effects{EffectMutatesExternal})

	first, err := fixture.service.CompleteClaim(fixture.ctx, fixture.completion())
	if err != nil {
		t.Fatal(err)
	}
	second, err := fixture.service.CompleteClaim(fixture.ctx, fixture.completion())
	if err != nil {
		t.Fatalf("replayed completion = %v, want the committed record", err)
	}
	if second.Version != first.Version || second.State != first.State ||
		string(second.Output) != string(first.Output) {
		t.Fatalf("replay returned a different record:\n first  = %#v\n second = %#v",
			first, second)
	}
}

// TestCompleteClaimDoesNotAcknowledgeACancelledAction is the case a terminal
// state alone cannot distinguish.
//
// Cancel lands on a terminal state at exactly the version a reporter expected
// to produce, and clones the record it cancelled — so it preserves that
// reporter's own ClaimID. A replay check that accepted any terminal state would
// therefore hand a late reporter the cancelled record with no error, telling it
// the work it performed was recorded while the record says the opposite. For an
// external effect that is the worst possible answer: the effect happened, the
// action says cancelled, and the worker was told everything was fine.
func TestCompleteClaimDoesNotAcknowledgeACancelledAction(t *testing.T) {
	fixture := newCompletionFixture(t, Effects{EffectMutatesExternal})
	// Cancel refuses a live claim, so the lease has to lapse first — which is
	// also the only way a worker ends up reporting this late.
	*fixture.clock = fixture.clock.Add(2 * time.Minute)
	cancelled, err := fixture.service.Cancel(fixture.ctx, CancelRequest{
		ID: fixture.claimed.ID, ExpectedVersion: fixture.claimed.Version,
		MutationKey: []byte("cancel"),
		Context:     dispatchContext(*fixture.clock, "request"),
	})
	if err != nil {
		t.Fatalf("Cancel: %v", err)
	}
	if cancelled.State != DispatchCanceled ||
		cancelled.Version != fixture.claimed.Version+1 ||
		string(cancelled.ClaimID) != "claim" {
		t.Fatalf("this test assumes cancel keeps the claim at version+1: %#v", cancelled)
	}

	request := fixture.completion()
	request.Context.Deadline = fixture.clock.Add(time.Hour)
	got, err := fixture.service.CompleteClaim(fixture.ctx, request)
	if !errors.Is(err, ErrClaimLost) {
		t.Fatalf("completion against a cancelled action = %v (record %#v), "+
			"want ErrClaimLost", err, got)
	}
	stored, storeErr := fixture.store.GetAction(fixture.ctx, fixture.claimed.ID)
	if storeErr != nil || stored.State != DispatchCanceled {
		t.Fatalf("stored = %#v, %v", stored, storeErr)
	}
}

// TestRemoteExecutorNeedsNoInProcessExecute is the topology this whole change
// exists for. A gateway proxy performs its work itself and reports through
// CompleteClaim; nothing in this process ever runs it.
//
// Requiring it to supply an Execute method would make every remote deployment
// bind a stub whose only purpose is to be refused, so claim and completion
// resolve through resolveActionBinding. The effect ceiling is still enforced —
// the reference below declares EffectExternal, and an evidence-only one would
// still refuse this action.
func TestRemoteExecutorNeedsNoInProcessExecute(t *testing.T) {
	fixture := newCompletionFixtureWithExecutor(t, Effects{EffectMutatesExternal}, remoteOnlyExecutor{})

	if !fixture.claimed.EffectPossible {
		t.Fatal("claiming an external action through a remote-only reference " +
			"did not mark the effect possible")
	}
	completed, err := fixture.service.CompleteClaim(fixture.ctx, fixture.completion())
	if err != nil {
		t.Fatalf("CompleteClaim through a remote-only executor: %v", err)
	}
	if completed.State != DispatchSucceeded {
		t.Fatalf("state = %q, want %q", completed.State, DispatchSucceeded)
	}
}

// TestExecuteClaimStillRequiresARunnableExecutor is the other half: relaxing
// the requirement for claim and completion must not let in-process execution
// run against a reference that cannot run anything.
func TestExecuteClaimStillRequiresARunnableExecutor(t *testing.T) {
	fixture := newCompletionFixtureWithExecutor(t, Effects{EffectMutatesExternal}, remoteOnlyExecutor{})

	if _, err := fixture.service.ExecuteClaim(fixture.ctx, fixture.claimed); err == nil {
		t.Fatal("ExecuteClaim ran an action whose executor has no Execute method")
	}
}

// remoteOnlyExecutor is a reference bound for work performed out of process. It
// declares an effect ceiling and deliberately implements no Execute.
type remoteOnlyExecutor struct{}

func (remoteOnlyExecutor) MaxEffects() Effects {
	return Effects{EffectMutatesExternal}
}

// TestClaimMarksAnUnrecoverableEffectPossibleBeforeItHappens is the decision
// #384 asked for. A remote worker owns the window between claiming and acting. If it
// then goes silent, the record must not say the effect certainly did not
// happen — otherwise an expired lease after the work was done is
// indistinguishable from one that expired before it started.
func TestClaimMarksAnUnrecoverableEffectPossibleBeforeItHappens(t *testing.T) {
	for _, probe := range []struct {
		name    string
		effects Effects
		want    bool
	}{
		{"external mutation", Effects{EffectMutatesExternal}, true},
		// Egress counts as much as mutation here. A worker can transmit and
		// then go silent, and content that left the host cannot be recalled,
		// so a record asserting no effect was possible would assert the one
		// thing nobody knows.
		{"egresses but does not mutate", Effects{EffectEgressesContent}, true},
		{"reads corpus only", Effects{EffectReadsCorpus}, false},
		{"declares nothing", nil, false},
	} {
		t.Run(probe.name, func(t *testing.T) {
			fixture := newCompletionFixture(t, probe.effects)
			if fixture.claimed.EffectPossible != probe.want {
				t.Fatalf("claimed EffectPossible = %v, want %v",
					fixture.claimed.EffectPossible, probe.want)
			}
			stored, err := fixture.store.GetAction(fixture.ctx, fixture.claimed.ID)
			if err != nil {
				t.Fatal(err)
			}
			if stored.EffectPossible != probe.want {
				t.Fatalf("stored EffectPossible = %v, want %v: the claim record "+
					"does not carry what the returned one claims",
					stored.EffectPossible, probe.want)
			}
		})
	}
}

// TestCompleteClaimRefusesAnExpiredLease is the case the fence exists for. A
// worker whose lease ran out while it was working has lost the right to write
// this record: the action may already have been reclaimed and performed again,
// and accepting the late report would record the wrong run's outcome.
//
// The two probes trip different checks on purpose. Past the claim lease the
// request is still well-formed and the claim check refuses it, so the error is
// specifically ErrClaimLost. Past the action deadline the request context has
// also elapsed and is refused before the claim is even loaded. Both must write
// nothing, and the second asserts only that, because pinning it to ErrClaimLost
// would be asserting which check happens to run first.
func TestCompleteClaimRefusesAnExpiredLease(t *testing.T) {
	// The claim lease is a minute; the action deadline is an hour.
	t.Run("past the claim lease", func(t *testing.T) {
		fixture := newCompletionFixture(t, Effects{EffectMutatesExternal})
		*fixture.clock = fixture.clock.Add(2 * time.Minute)
		_, err := fixture.service.CompleteClaim(fixture.ctx, fixture.completion())
		if !errors.Is(err, ErrClaimLost) {
			t.Fatalf("completion past the claim lease = %v, want ErrClaimLost", err)
		}
		// Not ambiguous. The report was refused before the service did anything
		// with it, so there is nothing for the worker to be uncertain about.
		// Without the up-front lease check this still fails with ErrClaimLost,
		// but joined to ErrExecutionAmbiguous from the post-effect fence check,
		// telling the worker the service cannot say whether its report landed.
		if errors.Is(err, ErrExecutionAmbiguous) {
			t.Fatalf("a plainly-late report was reported as ambiguous: %v", err)
		}
		fixture.assertStillClaimed(t)
	})
	// The action deadline is a separate bound from the claim lease, and a
	// worker can reach it while still sending a well-formed request: the
	// request context carries its own deadline, which this one sets ahead of
	// the clock. Reusing the fixture's original context instead would expire
	// that too, and the report would be refused by request validation before
	// the action deadline was ever consulted — passing without exercising it.
	t.Run("past the action deadline", func(t *testing.T) {
		fixture := newCompletionFixture(t, Effects{EffectMutatesExternal})
		*fixture.clock = fixture.clock.Add(2 * time.Hour)
		request := fixture.completion()
		request.Context.Deadline = fixture.clock.Add(time.Hour)
		_, err := fixture.service.CompleteClaim(fixture.ctx, request)
		if !errors.Is(err, ErrClaimLost) {
			t.Fatalf("completion past the action deadline = %v, want ErrClaimLost", err)
		}
		fixture.assertStillClaimed(t)
	})
}

// TestCompleteClaimDoesNotHandOneWorkersRecordToAnother is the replay path's
// other half. Recognising a terminal record at the expected version as "my
// work already committed" is only safe if it really was this reporter's work.
//
// Without the claim match, a second worker that claimed after the first
// committed would be told its own completion succeeded — while its external
// effect went unrecorded, and the record described a different run.
func TestCompleteClaimDoesNotHandOneWorkersRecordToAnother(t *testing.T) {
	fixture := newCompletionFixture(t, Effects{EffectMutatesExternal})

	first, err := fixture.service.CompleteClaim(fixture.ctx, fixture.completion())
	if err != nil {
		t.Fatal(err)
	}
	stranger := fixture.completion()
	stranger.ClaimID = []byte("other-worker")
	stranger.Result.Output = json.RawMessage(`{"ok":false}`)
	got, err := fixture.service.CompleteClaim(fixture.ctx, stranger)
	if !errors.Is(err, ErrClaimLost) {
		t.Fatalf("a stranger's completion = %v (record %#v), want ErrClaimLost: "+
			"it was handed another worker's committed record and would report "+
			"its own unrecorded effect as a success", err, got)
	}
	if string(got.Output) == string(first.Output) {
		t.Fatal("the stranger received the first worker's record")
	}
}

// TestCompleteClaimHidesAnotherPrincipalsAction pins what replaced the
// redundant identity check: a caller who is not the queued principal is told
// the action does not exist, rather than that it exists and is not theirs.
func TestCompleteClaimHidesAnotherPrincipalsAction(t *testing.T) {
	fixture := newCompletionFixture(t, Effects{EffectMutatesExternal})

	// Bound through the fixture's own authority: a decision the service's
	// resolver cannot resolve would be refused for the wrong reason and the
	// test would pass without exercising the principal check at all.
	stranger := dispatchDecision(t, "stranger", "actor", "request",
		auth.OperationDispatch, auth.OperationInvoke)
	strangerCtx := bindDecision(t, fixture.authority, stranger)

	_, err := fixture.service.CompleteClaim(strangerCtx, fixture.completion())
	if err == nil {
		t.Fatal("a stranger completed another principal's action")
	}
	if !shoal.IsErrorCode(err, shoal.ErrorNotFound) {
		t.Fatalf("stranger completion = %v, want not-found: an unauthorized "+
			"answer confirms the action exists", err)
	}
	fixture.assertStillClaimed(t)
}
