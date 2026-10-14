// Licensed to the Apache Software Foundation (ASF) under one or more
// contributor license agreements. See the NOTICE file distributed with this
// work for additional information regarding copyright ownership.
package fleet

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/phrocker/shoal-oss/pkg/explorer/auth"
	"github.com/phrocker/shoal-oss/pkg/explorer/evidencelabels"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

// executorClaimFixture enqueues as one principal so a second can try to claim.
//
// The two decisions differ in Subject, Actor and RequestID, which is what
// sameActionPrincipal compares — so the worker here is a genuinely different
// principal rather than the same one with a different grant.
type executorClaimFixture struct {
	service *DispatchService
	queued  ActionRecord
	// enqueuer holds invoke and dispatch, as an agent asking for work does.
	enqueuer      context.Context
	authority     *auth.Authority
	registryStore *memoryStore
	dispatchStore *memoryDispatchStore
	recorder      *dispatchRecorder
	executors     executorMap
	now           time.Time
	// clock is what the service and the authority both read, so advancing it
	// moves them together. The fixture's own now is kept in step because every
	// test builds its RequestContext from it.
	clock *time.Time
}

// advance moves the fixture's clock forward. Tests that need a lease to lapse
// need this rather than a short sleep: the service clock is frozen, so a
// one-nanosecond lease is still live however long the test waits.
func (f *executorClaimFixture) advance(t *testing.T, by time.Duration) {
	t.Helper()
	*f.clock = f.clock.Add(by)
	f.now = *f.clock
}

func newExecutorClaimFixture(t *testing.T) *executorClaimFixture {
	t.Helper()
	now := time.Date(2026, 9, 6, 12, 0, 0, 0, time.UTC)
	clock := &now
	authority, err := auth.NewAuthorityWithClock(
		func() time.Time { return *clock })
	if err != nil {
		t.Fatal(err)
	}
	registryStore := newMemoryStore()
	executors := executorMap{"exec": &remoteBoundExecutor{}}
	dispatchStore := newMemoryDispatchStore()
	recorder := &dispatchRecorder{}
	registry, err := NewService(Config{
		Store: registryStore, Resolver: authority.Resolver(), Recorder: &memoryRecorder{},
		Snapshots: fixedSnapshot{now},
		Executors: executors,
		Clock:     func() time.Time { return *clock },
	})
	if err != nil {
		t.Fatal(err)
	}
	descriptor := dispatchDescriptor(now)
	descriptor.LeaseExpiresAt = now.Add(24 * time.Hour)
	registryStore.records["agent"] = Stored{Descriptor: descriptor}
	service, err := NewDispatchService(DispatchConfig{
		Store: dispatchStore, Registry: registry,
		Resolver: authority.Resolver(), Recorder: recorder,
		Events: dispatchEvents{}, Clock: func() time.Time { return *clock },
	})
	if err != nil {
		t.Fatal(err)
	}
	enqueuer := bindDecision(t, authority, dispatchDecision(t,
		"owner", "actor", "request",
		auth.OperationDispatch, auth.OperationInvoke))
	queued, err := service.Enqueue(enqueuer, dispatchEnqueue(now, "request"))
	if err != nil {
		t.Fatal(err)
	}
	return &executorClaimFixture{
		service: service, queued: queued, enqueuer: enqueuer,
		authority: authority, registryStore: registryStore, now: now,
		executors: executors, dispatchStore: dispatchStore,
		recorder: recorder, clock: clock,
	}
}

// bindExecutor replaces the descriptor's bound executor with one that runs in
// process, so ExecuteClaim reaches it instead of refusing for want of an
// ActionExecutor implementation.
func (f *executorClaimFixture) bindExecutor(t *testing.T, executor ActionExecutor) {
	t.Helper()
	f.executors["exec"] = executor
}

// breakExecutor makes the record's descriptor unresolvable for a reason that is
// neither unauthorized nor absent, which is what produces the bare
// ErrorUnavailable that used to abort a page.
func (f *executorClaimFixture) breakExecutor(t *testing.T) {
	t.Helper()
	stored := f.registryStore.records["agent"]
	stored.Descriptor.ExecutorRef = "no-such-executor"
	f.registryStore.records["agent"] = stored
}

// worker binds a decision for a principal that is not the enqueuer.
func (f *executorClaimFixture) worker(t *testing.T, operations ...auth.Operation) context.Context {
	t.Helper()
	return bindDecision(t, f.authority, dispatchDecision(t,
		"gateway-subject", "gateway-actor", "gateway-request", operations...))
}

// TestAnExecutorClaimsWorkItDidNotEnqueue is the capability #437 adds, and the
// reason the gateway in docs/gateway-proxy-design.md could not be built.
//
// Claiming was authorized under OperationInvoke and additionally required the
// claimant to be the principal that enqueued. So an agent enqueued, a worker
// pulled with its own token, every record failed sameActionPrincipal, and the
// worker received an empty page — forever, with no error and nothing in any
// log. A ready worker, a growing queue, and nothing happening.
//
// OperationExecute is permission to take an agent's queued work and finish it,
// as distinct from permission to ask that agent to do something. Granted on
// the action's descriptor, it lets the enqueuer and the executor be different
// principals.
func TestAnExecutorClaimsWorkItDidNotEnqueue(t *testing.T) {
	fixture := newExecutorClaimFixture(t)
	worker := fixture.executorWorker(t)

	// It is visible to the worker at all, which is the half that used to fail
	// silently. A test that only exercised Claim would pass against a Pull
	// that still filtered the record out, and the worker would never find the
	// action to claim.
	page, err := fixture.service.Pull(worker, PullActionsRequest{
		Limit: 10, Context: dispatchContext(fixture.now, "gateway-request"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Actions) != 1 {
		t.Fatalf("pulled %d actions, want 1: a worker that cannot see the work "+
			"never claims it, and an empty page is not an error", len(page.Actions))
	}

	claimed, err := fixture.service.Claim(worker, ClaimRequest{
		ID: fixture.queued.ID, ExpectedVersion: fixture.queued.Version,
		ClaimID: []byte("worker-claim"), Lease: time.Minute,
		Context: dispatchContext(fixture.now, "gateway-request"),
	})
	if err != nil {
		t.Fatalf("an executor granted execute could not claim: %v", err)
	}
	if claimed.State != DispatchClaimed {
		t.Fatalf("state = %v, want claimed", claimed.State)
	}

	// And it can finish what it took. A claim it cannot complete is the #384
	// gap again, reached from a different direction.
	completed, err := fixture.service.CompleteClaim(worker, CompletionRequest{
		ID: claimed.ID, ExpectedVersion: claimed.Version,
		ClaimID: []byte("worker-claim"),
		Result:  ExecutionResult{Output: []byte(`{"ok":true}`)},
		Context: dispatchContext(fixture.now, "gateway-request"),
	})
	if err != nil {
		t.Fatalf("an executor could claim and not complete: %v", err)
	}
	if completed.State != DispatchSucceeded {
		t.Fatalf("state = %v, want succeeded", completed.State)
	}
}

// TestInvokeAloneStillCannotClaimAnotherPrincipalsWork bounds the widening.
//
// The separation is the whole point: an agent that may ask a gateway to post a
// message must not thereby be able to claim another agent's queued work and
// read its input. If invoke were enough, #437 would have replaced one
// authorization hole with a larger one.
func TestInvokeAloneStillCannotClaimAnotherPrincipalsWork(t *testing.T) {
	fixture := newExecutorClaimFixture(t)
	// Every operation a non-executor could plausibly hold, and no execute.
	other := fixture.worker(t, auth.OperationInvoke, auth.OperationDispatch)

	page, err := fixture.service.Pull(other, PullActionsRequest{
		Limit: 10, Context: dispatchContext(fixture.now, "gateway-request"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Actions) != 0 {
		t.Fatalf("a principal without execute pulled %d actions, want 0",
			len(page.Actions))
	}

	_, err = fixture.service.Claim(other, ClaimRequest{
		ID: fixture.queued.ID, ExpectedVersion: fixture.queued.Version,
		ClaimID: []byte("other-claim"), Lease: time.Minute,
		Context: dispatchContext(fixture.now, "gateway-request"),
	})
	if err == nil {
		t.Fatal("a principal holding only invoke claimed another principal's work")
	}
	// And it is told exactly what an absent action is told. Two routes into
	// authorizedCurrent must not become two distinguishable answers, or the
	// absent-versus-foreign normalisation that keeps this surface from being an
	// existence oracle is gone (#398).
	if !shoal.IsErrorCode(err, shoal.ErrorNotFound) {
		t.Fatalf("foreign claim error = %v, want the not-found shape", err)
	}
	_, absentErr := fixture.service.Claim(other, ClaimRequest{
		ID: []byte("no-such-action"), ExpectedVersion: 1,
		ClaimID: []byte("other-claim"), Lease: time.Minute,
		Context: dispatchContext(fixture.now, "gateway-request"),
	})
	if absentErr == nil {
		t.Fatal("an absent action was claimable")
	}
	if absentErr.Error() != err.Error() {
		t.Fatalf("absent and foreign differ:\n  absent: %v\n  foreign: %v",
			absentErr, err)
	}
}

// TestTheEnqueuingPrincipalStillClaimsUnderInvoke is the backward-compatibility
// half, and it is why the two routes are tried in the order they are.
//
// A deployment that enqueues and claims under one identity holds invoke and
// not execute. Requiring execute would have broken it on upgrade, so the
// principal-bound route remains and is tried second — the broader route first,
// the narrower one only if it fails, so nothing that worked stops working.
func TestTheEnqueuingPrincipalStillClaimsUnderInvoke(t *testing.T) {
	fixture := newExecutorClaimFixture(t)

	page, err := fixture.service.Pull(fixture.enqueuer, PullActionsRequest{
		Limit: 10, Context: dispatchContext(fixture.now, "request"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Actions) != 1 {
		t.Fatalf("the enqueuer pulled %d actions, want 1", len(page.Actions))
	}
	if _, err = fixture.service.Claim(fixture.enqueuer, ClaimRequest{
		ID: fixture.queued.ID, ExpectedVersion: fixture.queued.Version,
		ClaimID: []byte("own-claim"), Lease: time.Minute,
		Context: dispatchContext(fixture.now, "request"),
	}); err != nil {
		t.Fatalf("the enqueuing principal lost the ability to claim: %v", err)
	}
}

// TestAForeignClaimDoesNotDetachTheEnqueuer is the destructive case my first
// version of this change shipped, and the case TestCancelAndStatusStayEnqueuerOnly
// cannot see.
//
// applyClaim assigned record.Actor = decision.Actor(). That was a no-op while
// claiming required the principal to match, because the claimant was by
// construction the enqueuer. Once a third party could claim, the same line
// overwrote the record's own principal — permanently, since the completion path
// clones the record forward and never restores it. sameActionPrincipal compares
// Actor, so the enqueuer was then refused Status and Cancel on its own
// in-flight action, with no fallback: TeamActions needs an operation fleet
// principals do not hold.
//
// Claiming with a one-nanosecond lease is the sharp form. The claim commits,
// the lease lapses immediately so the action is reclaimable, and the enqueuer
// is detached from a record that is still live.
//
// The other test checks Status and Cancel on a record that was never claimed,
// which is why it passed throughout. The bodies were unchanged; their
// reachability was not, because the field they key on had become
// third-party-writable.
func TestAForeignClaimDoesNotDetachTheEnqueuer(t *testing.T) {
	fixture := newExecutorClaimFixture(t)
	worker := fixture.executorWorker(t)

	if _, err := fixture.service.Claim(worker, ClaimRequest{
		ID: fixture.queued.ID, ExpectedVersion: fixture.queued.Version,
		ClaimID: []byte("worker-claim"), Lease: time.Nanosecond,
		Context: dispatchContext(fixture.now, "gateway-request"),
	}); err != nil {
		t.Fatal(err)
	}

	// The enqueuer can still read its own action.
	if _, err := fixture.service.Status(fixture.enqueuer, StatusRequest{
		ID: fixture.queued.ID, Context: dispatchContext(fixture.now, "request"),
	}); err != nil {
		t.Fatalf("the enqueuer lost Status on its own action after a foreign "+
			"claim: %v", err)
	}
	// And is still *recognised* by Cancel, which is the precise property. The
	// clock is frozen, so a one-nanosecond lease is still live and Cancel
	// correctly refuses with a conflict — that is the pre-existing semantics
	// and not the bug.
	//
	// The bug looked like ObjectNotFound: the predicate no longer recognising
	// the enqueuer at all. So the assertion is on which refusal arrives, not on
	// whether the call succeeds. An earlier version of this test required
	// success and failed for the right reason in the wrong way, which would
	// have been read as the fix not working.
	_, err := fixture.service.Cancel(fixture.enqueuer, CancelRequest{
		ID: fixture.queued.ID, ExpectedVersion: fixture.queued.Version + 1,
		MutationKey: []byte("owner-cancel"),
		Context:     dispatchContext(fixture.now, "request"),
	})
	if shoal.IsErrorCode(err, shoal.ErrorNotFound) {
		t.Fatalf("Cancel no longer recognises the enqueuer after a foreign "+
			"claim: %v", err)
	}
	if !errors.Is(err, ErrActionConflict) {
		t.Fatalf("Cancel = %v, want the live-claim conflict", err)
	}
}

// TestAForeignResolveFailureDoesNotAbortThePage covers the shared-queue outage.
//
// resolveActionBinding returns a bare ErrorUnavailable when an executor is
// unregistered or an effect ceiling has narrowed. Before #437 the principal
// check ran first, so such a record never reached that call for anyone but its
// owner. With two routes it would abort the page of every execute-holder in the
// scope, on every pull, until the record expired — and any principal able to
// enqueue could plant one.
//
// The probe revokes the descriptor the record names, which is the cheapest way
// to make resolution fail for a reason that is neither unauthorized nor absent.
func TestAForeignResolveFailureDoesNotAbortThePage(t *testing.T) {
	fixture := newExecutorClaimFixture(t)
	worker := fixture.executorWorker(t)

	// Sanity: the record is visible before anything is broken, or this test
	// would pass against a page that was empty for an unrelated reason.
	page, err := fixture.service.Pull(worker, PullActionsRequest{
		Limit: 10, Context: dispatchContext(fixture.now, "gateway-request"),
	})
	if err != nil || len(page.Actions) != 1 {
		t.Fatalf("precondition: pulled %d actions, err %v", len(page.Actions), err)
	}

	fixture.breakExecutor(t)

	// The page is now empty rather than an error. A skip is the pre-#437
	// behaviour for a record the caller does not own.
	page, err = fixture.service.Pull(worker, PullActionsRequest{
		Limit: 10, Context: dispatchContext(fixture.now, "gateway-request"),
	})
	if err != nil {
		t.Fatalf("one unresolvable foreign record aborted the whole page: %v", err)
	}
	if len(page.Actions) != 0 {
		t.Fatalf("pulled %d actions, want 0", len(page.Actions))
	}
}

// TestCancelAndStatusStayEnqueuerOnly pins the scope of this change.
//
// #437 separated execute from invoke for *claiming*. Whether an executor
// should be able to cancel or inspect is a different question behind a
// different operation, and widening dispatch here would answer it by accident.
// So an executor holding execute and dispatch still cannot reach either, and
// that is deliberate rather than overlooked.
func TestCancelAndStatusStayEnqueuerOnly(t *testing.T) {
	fixture := newExecutorClaimFixture(t)
	worker := fixture.worker(t, auth.OperationExecute, auth.OperationDispatch)

	if _, err := fixture.service.Status(worker, StatusRequest{
		ID: fixture.queued.ID, Context: dispatchContext(fixture.now, "gateway-request"),
	}); !shoal.IsErrorCode(err, shoal.ErrorNotFound) {
		t.Fatalf("status for a non-enqueuer = %v, want the not-found shape", err)
	}
	if _, err := fixture.service.Cancel(worker, CancelRequest{
		ID: fixture.queued.ID, ExpectedVersion: fixture.queued.Version,
		MutationKey: []byte("gateway-cancel"),
		Context:     dispatchContext(fixture.now, "gateway-request"),
	}); !shoal.IsErrorCode(err, shoal.ErrorNotFound) {
		t.Fatalf("cancel for a non-enqueuer = %v, want the not-found shape", err)
	}
}

// TestAnExecuteHolderCannotProbeForExistence closes the #398 oracle on the
// route that reopened it.
//
// authorizedCurrent normalised only Unauthorized and NotFound from
// resolveActionBinding. A record whose descriptor fails executor availability
// or the effect ceiling returns a bare ErrorUnavailable, which surfaces as a
// different answer than absent. For the enqueuing principal that distinction is
// information it already has; for a caller reaching a record on the strength of
// an execute grant it is an existence oracle.
//
// It matters most on the admission namespace, whose IDs are computable from a
// principal tuple and whose isAdmission refusal runs *after* this — so the
// distinguishable error would arrive first.
//
// The existing foreign-claim test cannot catch this: a principal holding only
// invoke never produces the bare error, because the route that would resolve
// the binding without a principal check is the execute one, and it is not
// authorized to take it. It is refused at decision.AuthorizeObject on the
// execute route and then at the principal check on the invoke route — the
// order matters only to the comment, not the conclusion, and an earlier
// version of this one named the second mechanism alone. This test holds
// execute, which is the whole difference.
func TestAnExecuteHolderCannotProbeForExistence(t *testing.T) {
	fixture := newExecutorClaimFixture(t)
	worker := fixture.executorWorker(t)
	// Unresolvable for a reason that is neither unauthorized nor absent, which
	// is what produces the bare ErrorUnavailable.
	fixture.breakExecutor(t)

	_, existing := fixture.service.Claim(worker, ClaimRequest{
		ID: fixture.queued.ID, ExpectedVersion: fixture.queued.Version,
		ClaimID: []byte("probe"), Lease: time.Minute,
		Context: dispatchContext(fixture.now, "gateway-request"),
	})
	_, absent := fixture.service.Claim(worker, ClaimRequest{
		ID: []byte("no-such-action"), ExpectedVersion: 1,
		ClaimID: []byte("probe"), Lease: time.Minute,
		Context: dispatchContext(fixture.now, "gateway-request"),
	})
	if existing == nil || absent == nil {
		t.Fatalf("expected both to be refused: existing=%v absent=%v", existing, absent)
	}
	if existing.Error() != absent.Error() {
		t.Fatalf("an execute-holder can tell an existing action from an absent "+
			"one:\n  existing: %v\n  absent:   %v", existing, absent)
	}
	if !shoal.IsErrorCode(existing, shoal.ErrorNotFound) {
		t.Fatalf("probe error = %v, want the not-found shape", existing)
	}
}

// principal is the whole of what sameClaimantPrincipal compares, so a test can
// vary one component at a time. dispatchDecision takes only subject, actor and
// request, which is why three of the four components had no coverage.
type principal struct {
	subject    string
	actor      string
	request    string
	clientID   shoal.ID
	onBehalfOf []shoal.ID
}

func dispatchDecisionFor(
	t *testing.T, who principal, operations ...auth.Operation,
) auth.Decision {
	t.Helper()
	decision, err := auth.NewDecision(auth.DecisionConfig{
		Subject: shoal.ID(who.subject), Actor: shoal.ID(who.actor),
		ClientID: who.clientID, OnBehalfOf: who.onBehalfOf,
		AuthorizationDomain: []byte("domain"),
		AllowedOperations:   operations,
		PermittedSourceIDs:  [][]byte{[]byte("source")},
		PermittedPolicyIDs:  [][]byte{[]byte("policy")}, PolicyGeneration: 1,
		AuthenticationExpires: time.Date(2026, 9, 7, 0, 0, 0, 0, time.UTC),
		RequestID:             shoal.ID(who.request), CorrelationID: "correlation",
	})
	if err != nil {
		t.Fatal(err)
	}
	return decision
}

// namedWorker binds a decision for a specific principal, so two workers can be
// told apart. The shared worker helper mints one identity, which is enough for
// "not the enqueuer" and not enough for "not the other worker" — and the
// difference between those two is where this PR's worst defect lived.
func (f *executorClaimFixture) namedWorker(
	t *testing.T, name string, operations ...auth.Operation,
) context.Context {
	t.Helper()
	return bindDecision(t, f.authority, dispatchDecision(t,
		name+"-subject", name+"-actor", name+"-request", operations...))
}

// TestOneWorkerCannotCompleteAnotherWorkersClaim is the gate #437 removed
// without replacing, and the reason this PR was held in draft a second time.
//
// Claiming and completing were both authorized under OperationInvoke and both
// additionally required sameActionPrincipal, so the claimant was by
// construction the enqueuer and the enqueuer's chain was already on the record.
// Relaxing claiming to OperationExecute left the completion predicate as
// version, state and ClaimID — and none of those is an identity.
//
// ClaimID cannot stand in for one. validateOpaque accepts any one to
// MaxActionIDBytes bytes with no entropy requirement, dispatch_model.go calls
// it "the durable claimant identity" — which invites a worker to reuse a
// stable value — and it is published twice over: Status returns it to every
// co-principal, and Pull returns a claimed record whose lease has lapsed, its
// ClaimID intact, to every principal authorized to execute the descriptor.
//
// The result was worse than a fabricated outcome. The hijacker's write lands,
// and then the replay branch recognises the real worker's report as its own
// committed write and returns 200 carrying the hijacker's output. A worker that
// performed an irreversible external effect is told the work was recorded and
// handed someone else's result, which is the one failure mode it cannot detect.
func TestOneWorkerCannotCompleteAnotherWorkersClaim(t *testing.T) {
	fixture := newExecutorClaimFixture(t)
	holder := fixture.namedExecutor(t, "holder")
	stranger := fixture.namedExecutor(t, "stranger")

	claimed, err := fixture.service.Claim(holder, ClaimRequest{
		ID: fixture.queued.ID, ExpectedVersion: fixture.queued.Version,
		ClaimID: []byte("holder-claim"), Lease: time.Minute,
		Context: dispatchContext(fixture.now, "holder-request"),
	})
	if err != nil {
		t.Fatalf("the holder could not claim: %v", err)
	}

	// The stranger holds execute on the same descriptor in the same scope and
	// has claimed nothing. It presents the holder's ClaimID and version.
	_, err = fixture.service.CompleteClaim(stranger, CompletionRequest{
		ID: fixture.queued.ID, ExpectedVersion: claimed.Version,
		ClaimID: []byte("holder-claim"),
		Result:  ExecutionResult{Output: json.RawMessage(`{"ok":false}`)},
		Context: dispatchContext(fixture.now, "stranger-request"),
	})
	if err == nil {
		t.Fatal("a principal that never claimed this action completed it, " +
			"which lets any execute-holder commit a fabricated outcome for " +
			"work another worker is performing")
	}
	// Concealed, not refused: ErrClaimLost would confirm the action exists to
	// a caller with no standing, which is the #398 oracle.
	if !shoal.IsErrorCode(err, shoal.ErrorNotFound) {
		t.Fatalf("the refusal discloses the action to a caller with no "+
			"standing: %v", err)
	}

	// The record is untouched, which is the part that matters. A refusal that
	// still wrote the output would be the same defect with an error attached.
	after, err := fixture.service.Status(fixture.enqueuer, StatusRequest{
		ID: fixture.queued.ID, Context: dispatchContext(fixture.now, "request"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if after.State != DispatchClaimed || len(after.Output) != 0 {
		t.Fatalf("the refused completion still changed the record: state=%s "+
			"output=%s", after.State, after.Output)
	}

	// And the real claimant still completes, with its own outcome rather than
	// a receipt for someone else's. This is the half that makes the test about
	// identity instead of about refusing everything.
	done, err := fixture.service.CompleteClaim(holder, CompletionRequest{
		ID: fixture.queued.ID, ExpectedVersion: claimed.Version,
		ClaimID: []byte("holder-claim"),
		Result:  ExecutionResult{Output: json.RawMessage(`{"ok":true}`)},
		Context: dispatchContext(fixture.now, "holder-request"),
	})
	if err != nil {
		t.Fatalf("the claim's actual holder was refused: %v", err)
	}
	if string(done.Output) != `{"ok":true}` {
		t.Fatalf("the claimant's own outcome was not recorded: %s", done.Output)
	}
}

// TestAReclaimMovesWhoMayReport covers the case the stored claimant has to get
// right to be usable at all: a lease lapses, a second worker takes the record,
// and the first worker must no longer be able to report on it.
//
// A claimant field that were only ever written once would pass the test above
// and still leave the first worker able to complete after losing the fence.
func TestAReclaimMovesWhoMayReport(t *testing.T) {
	fixture := newExecutorClaimFixture(t)
	first := fixture.namedExecutor(t, "first")
	second := fixture.namedExecutor(t, "second")

	claimed, err := fixture.service.Claim(first, ClaimRequest{
		ID: fixture.queued.ID, ExpectedVersion: fixture.queued.Version,
		ClaimID: []byte("first-claim"), Lease: time.Nanosecond,
		Context: dispatchContext(fixture.now, "first-request"),
	})
	if err != nil {
		t.Fatalf("the first worker could not claim: %v", err)
	}

	// The clock is frozen, so advance it past the lease rather than sleeping.
	fixture.advance(t, time.Second)

	reclaimed, err := fixture.service.Claim(second, ClaimRequest{
		ID: fixture.queued.ID, ExpectedVersion: claimed.Version,
		ClaimID: []byte("second-claim"), Lease: time.Minute,
		Context: dispatchContext(fixture.now, "second-request"),
	})
	if err != nil {
		t.Fatalf("the second worker could not take the lapsed claim: %v", err)
	}
	if reclaimed.ClaimFence <= claimed.ClaimFence {
		t.Fatalf("the fence did not advance across claimants: %d then %d",
			claimed.ClaimFence, reclaimed.ClaimFence)
	}

	// The first worker reports under its own, now-stale claim.
	if _, err := fixture.service.CompleteClaim(first, CompletionRequest{
		ID: fixture.queued.ID, ExpectedVersion: claimed.Version,
		ClaimID: []byte("first-claim"),
		Result:  ExecutionResult{Output: json.RawMessage(`{"ok":false}`)},
		Context: dispatchContext(fixture.now, "first-request"),
	}); err == nil {
		t.Fatal("a worker whose claim was taken over still completed the " +
			"action, so the stored claimant is not updated on re-claim")
	}

	// And the new holder can.
	if _, err := fixture.service.CompleteClaim(second, CompletionRequest{
		ID: fixture.queued.ID, ExpectedVersion: reclaimed.Version,
		ClaimID: []byte("second-claim"),
		Result:  ExecutionResult{Output: json.RawMessage(`{"ok":true}`)},
		Context: dispatchContext(fixture.now, "second-request"),
	}); err != nil {
		t.Fatalf("the current holder was refused: %v", err)
	}
}

// TestAStrangerCannotReplayAnotherWorkersClaim covers Claim's replay branch,
// which is a second way into a live record.
//
// The branch exists so a worker whose response was lost receives the claim it
// already holds instead of a conflict against its own write, and it keys on
// ClaimID, version and lease — all three of which a second execute-holder
// knows, because Pull hands it a lapsed-claim record carrying them. Without an
// identity condition it returned the full record of a live claim, including the
// action's input, which Pull withholds precisely by excluding live-claimed
// records from the page.
func TestAStrangerCannotReplayAnotherWorkersClaim(t *testing.T) {
	fixture := newExecutorClaimFixture(t)
	holder := fixture.namedExecutor(t, "holder")
	stranger := fixture.namedExecutor(t, "stranger")

	if _, err := fixture.service.Claim(holder, ClaimRequest{
		ID: fixture.queued.ID, ExpectedVersion: fixture.queued.Version,
		ClaimID: []byte("holder-claim"), Lease: time.Minute,
		Context: dispatchContext(fixture.now, "holder-request"),
	}); err != nil {
		t.Fatalf("the holder could not claim: %v", err)
	}

	// Exactly the request the holder would replay, from the wrong principal.
	replayed, err := fixture.service.Claim(stranger, ClaimRequest{
		ID: fixture.queued.ID, ExpectedVersion: fixture.queued.Version,
		ClaimID: []byte("holder-claim"), Lease: time.Minute,
		Context: dispatchContext(fixture.now, "stranger-request"),
	})
	if err == nil {
		t.Fatalf("the replay branch handed a live claim to a principal that "+
			"does not hold it, input included: state=%s input=%s",
			replayed.State, replayed.Input)
	}

	// The holder's own replay still works, so this is an identity condition
	// rather than the branch being disabled.
	again, err := fixture.service.Claim(holder, ClaimRequest{
		ID: fixture.queued.ID, ExpectedVersion: fixture.queued.Version,
		ClaimID: []byte("holder-claim"), Lease: time.Minute,
		Context: dispatchContext(fixture.now, "holder-request"),
	})
	if err != nil {
		t.Fatalf("the holder's own replay was refused, so a lost response is "+
			"now an unrecoverable conflict: %v", err)
	}
	if !bytes.Equal(again.ClaimID, []byte("holder-claim")) {
		t.Fatalf("the replay returned a different claim: %q", again.ClaimID)
	}
}

// breakingExecutor performs the work and, as a side effect of performing it,
// makes the action's descriptor stop resolving.
//
// That side effect is the only way to reach the post-effect re-authorization
// from a test. Both authorization checks in a completion live inside one call —
// the pre-effect resolve in authorizedClaimant and the post-effect one in
// applyExecutionResult — so nothing outside can act between them. Breaking the
// registry from inside Execute puts the change exactly where a revocation would
// land in production: after the effect, before the record.
type breakingExecutor struct {
	store *memoryStore
}

func (e *breakingExecutor) Execute(
	_ context.Context, invocation Invocation,
) (ExecutionResult, error) {
	stored := e.store.records["agent"]
	stored.Descriptor.ExecutorRef = "no-such-executor"
	e.store.records["agent"] = stored
	return ExecutionResult{Output: json.RawMessage(`{"ok":true}`)}, nil
}

// TestARevokedExecutorCannotCommitItsOutcome covers the post-effect
// re-authorization, which was the one line in this change that no test could
// see. Deleting it left the whole repository green.
//
// It is also the most expensive check here to get wrong, because it runs
// *after* the effect. An executor refused at this point has already performed
// irreversible work and is then told the outcome is ambiguous — so it has to
// refuse exactly the caller whose authorization genuinely went away and nobody
// else. Before this PR it was sameActionPrincipal paired with a
// resolveActionBinding hardcoded to OperationInvoke, which would have failed
// every execute-authorized completion at the last step, indistinguishably from
// the lease loss the ambiguity is reserved for.
func TestARevokedExecutorCannotCommitItsOutcome(t *testing.T) {
	fixture := newExecutorClaimFixture(t)
	fixture.bindExecutor(t, &breakingExecutor{store: fixture.registryStore})

	// Driven by the enqueuer, because ExecuteClaim is the in-process path and
	// requires sameActionPrincipal at its entry — in-process execution runs on
	// the enqueuer's behalf. That is also why this reaches the post-effect
	// check at all: the remote completion path resolves before the effect and
	// refuses a broken descriptor there, so a revocation can only be observed
	// after the work when the work happens in process.
	claimed, err := fixture.service.Claim(fixture.enqueuer, ClaimRequest{
		ID: fixture.queued.ID, ExpectedVersion: fixture.queued.Version,
		ClaimID: []byte("in-process-claim"), Lease: time.Minute,
		Context: dispatchContext(fixture.now, "request"),
	})
	if err != nil {
		t.Fatalf("the enqueuer could not claim: %v", err)
	}

	_, err = fixture.service.ExecuteClaim(fixture.enqueuer, claimed)
	if err == nil {
		t.Fatal("a worker whose authorization to execute this action was " +
			"withdrawn mid-effect still committed its outcome")
	}
	// Ambiguous rather than a plain refusal, and that distinction is the
	// point. The work happened, so "this did not happen" would be a lie;
	// ErrExecutionAmbiguous is what tells a caller to treat the outcome as
	// unknown and reconcile it.
	if !errors.Is(err, ErrExecutionAmbiguous) {
		t.Fatalf("the refusal does not tell the caller its effect is "+
			"unreconciled, which is the only thing it can act on: %v", err)
	}
	// Specifically the fault arm, not the authorization verdict below it. Both
	// return ErrExecutionAmbiguous, so asserting only that let the fault arm be
	// deleted with the suite green — the verdict arm caught the same case and
	// produced its own message. That is the defect this test's sibling was
	// split off to fix, left standing in the other half.
	if strings.Contains(err.Error(), "terminal execution identity changed") {
		t.Fatalf("refused by the authorization verdict rather than by the "+
			"fault arm, so the fault arm is still uncovered: %v", err)
	}

	// Put the descriptor back before reading the record. The executor broke it
	// permanently, and Status resolves the binding too — so without this the
	// read fails for the reason the test created rather than telling us
	// anything about the record.
	stored := fixture.registryStore.records["agent"]
	stored.Descriptor.ExecutorRef = "exec"
	fixture.registryStore.records["agent"] = stored

	// The record did not quietly become terminal on the way out, and recorded
	// no outcome. A refusal that still wrote the output would be the same
	// defect with an error attached.
	//
	// EffectPossible is deliberately not asserted here: applyClaim sets it
	// only for an action declaring external mutation or egress, and this
	// fixture's action declares neither, so it is false for a reason that has
	// nothing to do with this check. Asserting it would be asserting the
	// fixture.
	after, err := fixture.service.Status(fixture.enqueuer, StatusRequest{
		ID: fixture.queued.ID, Context: dispatchContext(fixture.now, "request"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if after.State != DispatchClaimed {
		t.Fatalf("the refused completion still moved the record to %s", after.State)
	}
	if len(after.Output) != 0 {
		t.Fatalf("the refused completion recorded an outcome anyway: %s",
			after.Output)
	}
}

// TestATerminalActionIsNotVisibleToAStranger covers the #398 normalisation on
// the routes this PR added, which is where it was reopened.
//
// Every route used to resolve through authorizedCurrent with
// sameActionPrincipal required unconditionally, so a caller with no standing
// was refused with ObjectNotFound before reaching any handler. The execute
// routes removed that gate deliberately. What they then exposed is that the
// handlers answer with ErrActionConflict, ErrActionTerminal and ErrClaimLost,
// and an absent action answers with ObjectNotFound — so an execute-holder could
// probe arbitrary action IDs, which are caller-chosen opaque bytes, and tell
// the two apart.
//
// Three branches reach an answer before any of them compares the caller to the
// record, and each is covered separately below. A first version of this test
// covered only the version mismatch, because it probed with a stale version —
// and removing the normalisation from either of the other two left the suite
// green.
//
// Terminal and past-deadline are two of the three states Pull does not return;
// the third is live-claimed, which an earlier version of this comment wrongly
// listed as visible through Pull and which has its own test now
// (TestALiveClaimIsNotVisibleToAStranger). A queued record is in the caller's
// page, so a refusal about one conceals nothing and is left informative.
func TestATerminalActionIsNotVisibleToAStranger(t *testing.T) {
	t.Run("a stale version", func(t *testing.T) {
		fixture := newExecutorClaimFixture(t)
		stranger := fixture.namedExecutor(t, "stranger")
		fixture.cancel(t)
		fixture.assertIndistinguishable(t, stranger, 1)
	})

	t.Run("a terminal record at its current version", func(t *testing.T) {
		fixture := newExecutorClaimFixture(t)
		stranger := fixture.namedExecutor(t, "stranger")
		cancelled := fixture.cancel(t)
		// Pull does not offer it, so this is a record the stranger has no
		// other way to observe.
		page, err := fixture.service.Pull(stranger, PullActionsRequest{
			Limit: 10, Context: dispatchContext(fixture.now, "stranger-request"),
		})
		if err != nil {
			t.Fatal(err)
		}
		if len(page.Actions) != 0 {
			t.Fatalf("a terminal action is still pullable, so this case is "+
				"not about a hidden record: %d actions", len(page.Actions))
		}
		fixture.assertIndistinguishable(t, stranger, cancelled.Version)
	})

	t.Run("a record past its deadline", func(t *testing.T) {
		fixture := newExecutorClaimFixture(t)
		// Still queued rather than terminal, so this reaches the deadline
		// check rather than the terminal one above it. The clock moves before
		// the caller is minted, because a decision bound beforehand would
		// expire with it and be refused at begin — both probes identically,
		// which would make this case pass without reaching the branch at all.
		// Two hours, not more: the action's deadline is an hour out and the
		// agent descriptor's lease is twenty-four, so a larger jump expires
		// the descriptor and the probe is refused for that instead — which
		// refuses both probes identically and so passes without reaching the
		// branch.
		fixture.advance(t, 2*time.Hour)
		stranger := fixture.namedExecutor(t, "stranger")
		fixture.assertIndistinguishable(t, stranger, fixture.queued.Version)
	})
}

func (f *executorClaimFixture) cancel(t *testing.T) ActionRecord {
	t.Helper()
	cancelled, err := f.service.Cancel(f.enqueuer, CancelRequest{
		ID: f.queued.ID, ExpectedVersion: f.queued.Version,
		MutationKey: []byte("cancel-once"),
		Context:     dispatchContext(f.now, "request"),
	})
	if err != nil {
		t.Fatalf("the enqueuer could not cancel: %v", err)
	}
	return cancelled
}

// assertIndistinguishable requires that claiming and completing the fixture's
// real action produce byte-identical refusals to claiming and completing an
// action ID that was never used.
func (f *executorClaimFixture) assertIndistinguishable(
	t *testing.T, caller context.Context, version uint64,
) {
	t.Helper()
	absentID := []byte("never-existed")
	for _, probe := range []struct {
		name string
		call func(id []byte) error
	}{
		{"claim", func(id []byte) error {
			_, err := f.service.Claim(caller, ClaimRequest{
				ID: id, ExpectedVersion: version, ClaimID: []byte("probe"),
				Lease:   time.Minute,
				Context: dispatchContext(f.now, "stranger-request"),
			})
			return err
		}},
		{"complete", func(id []byte) error {
			_, err := f.service.CompleteClaim(caller, CompletionRequest{
				ID: id, ExpectedVersion: version, ClaimID: []byte("probe"),
				Result:  ExecutionResult{Output: json.RawMessage(`{"ok":true}`)},
				Context: dispatchContext(f.now, "stranger-request"),
			})
			return err
		}},
	} {
		present, absent := probe.call(f.queued.ID), probe.call(absentID)
		if present == nil {
			t.Fatalf("%s succeeded against a record this caller has no "+
				"standing on", probe.name)
		}
		if absent == nil {
			t.Fatalf("%s succeeded against an action ID that was never used",
				probe.name)
		}
		if present.Error() != absent.Error() {
			t.Errorf("%s distinguishes an existing action from an absent one "+
				"at version %d:\n existing: %v\n absent:   %v",
				probe.name, version, present, absent)
		}
		if !shoal.IsErrorCode(present, shoal.ErrorNotFound) {
			t.Errorf("%s answers an existing action with %v, which is not the "+
				"answer an absent one gets", probe.name, present)
		}
	}
}

// revokingExecutor performs the work and then deletes the agent's descriptor
// outright, so resolution afterwards is a clean not-found rather than a fault.
//
// That distinction is the whole reason this type exists alongside
// breakingExecutor. Pointing the descriptor at a missing executor reference
// produces a bare Unavailable, which claimableBy returns as an *error* — so the
// completion is refused by the `authorizeErr != nil` arm and the authorization
// verdict below it is never consulted. A review found that deleting either arm
// alone left the suite green. Deleting the descriptor exercises the verdict.
type revokingExecutor struct {
	store *memoryStore
}

func (e *revokingExecutor) Execute(
	_ context.Context, _ Invocation,
) (ExecutionResult, error) {
	delete(e.store.records, "agent")
	return ExecutionResult{Output: json.RawMessage(`{"ok":true}`)}, nil
}

// TestAnUnauthorizedExecutorCannotCommitItsOutcome covers the authorization
// verdict of the post-effect re-authorization, as distinct from a fault in
// reaching it.
//
// Both arms refuse, and both must: the work has happened either way, so the
// caller has to be told the outcome is unreconciled rather than that it did not
// occur. They are separate tests because they are separate lines, and a single
// fixture that trips the fault arm leaves the verdict arm uncovered.
func TestAnUnauthorizedExecutorCannotCommitItsOutcome(t *testing.T) {
	fixture := newExecutorClaimFixture(t)
	fixture.bindExecutor(t, &revokingExecutor{store: fixture.registryStore})

	claimed, err := fixture.service.Claim(fixture.enqueuer, ClaimRequest{
		ID: fixture.queued.ID, ExpectedVersion: fixture.queued.Version,
		ClaimID: []byte("in-process-claim"), Lease: time.Minute,
		Context: dispatchContext(fixture.now, "request"),
	})
	if err != nil {
		t.Fatalf("the enqueuer could not claim: %v", err)
	}

	_, err = fixture.service.ExecuteClaim(fixture.enqueuer, claimed)
	if err == nil {
		t.Fatal("an executor no longer authorized for this action committed " +
			"its outcome anyway")
	}
	if !errors.Is(err, ErrExecutionAmbiguous) {
		t.Fatalf("the refusal does not tell the caller its effect is "+
			"unreconciled: %v", err)
	}
	// Specifically the verdict, not the fault arm above it. If the descriptor
	// had merely become unresolvable this would read "unavailable".
	if !strings.Contains(err.Error(), "terminal execution identity changed") {
		t.Fatalf("refused by the fault arm rather than by the authorization "+
			"verdict, so the verdict is still uncovered: %v", err)
	}
}

// TestTheClaimantIsComparedOnItsWholeChain covers the three components of
// sameClaimantPrincipal that no test could see.
//
// The shared worker helper varies subject, actor and request together, so a
// comparison checking only the subject passed every existing test.
//
// Each variant below differs from the claim's holder in **exactly one**
// component. A first version of this test varied three at once — it left the
// client ID and the delegation chain empty while changing the actor — so
// dropping any single comparison still refused, and dropping the Actor, the
// ClientID or the whole chain comparison from the production code left this
// test green. One component at a time is the whole point: a fixture that
// differs in three ways cannot tell you which of the three is checked.
//
// Since #391 a worker is executor-bound and acts as itself, so the execute
// route cannot carry a delegation chain: the Actor and ClientID variants run
// through the service with bound workers, and the two chain variants are
// asserted on sameClaimantPrincipal directly, where the chain is still
// compared for a delegated invoke-route claimant. Through the service those
// two would be refused earlier — by the binding on the execute route, and by
// sameActionPrincipal on the invoke route — so a service-level probe could no
// longer tell whether the chain comparison is there.
func TestTheClaimantIsComparedOnItsWholeChain(t *testing.T) {
	holder := principal{
		subject: "holder-subject", actor: "holder-actor",
		request: "holder-request", clientID: "holder-client",
	}
	// Derived from the holder so every field matches unless a case changes it.
	differing := func(mutate func(*principal)) principal {
		other := holder
		other.request = "other-request"
		mutate(&other)
		return other
	}

	delegated := holder
	delegated.onBehalfOf = []shoal.ID{"delegator"}
	record := ActionRecord{
		ClaimantSubject: shoal.ID(delegated.subject),
		ClaimantActor:   shoal.ID(delegated.actor), ClaimantClientID: delegated.clientID,
		ClaimantOnBehalfOf: delegated.onBehalfOf,
	}
	if !sameClaimantPrincipal(dispatchDecisionFor(
		t, delegated, auth.OperationInvoke, auth.OperationDelegate), record) {
		t.Fatal("the delegated holder is not its own claimant, so the chain " +
			"cases below prove nothing")
	}
	for component, chain := range map[string][]shoal.ID{
		"OnBehalfOf (chain emptied)":       nil,
		"OnBehalfOf (different delegator)": {"other-delegator"},
	} {
		other := delegated
		other.onBehalfOf = chain
		if sameClaimantPrincipal(dispatchDecisionFor(
			t, other, auth.OperationInvoke, auth.OperationDelegate), record) {
			t.Fatalf("a principal differing from the claimant only in %s is "+
				"its claimant, so that component is not compared", component)
		}
	}

	for _, variant := range []struct {
		component string
		who       principal
	}{
		{"Actor", differing(func(p *principal) { p.actor = "other-actor" })},
		{"ClientID", differing(func(p *principal) { p.clientID = "other-client" })},
	} {
		t.Run(variant.component, func(t *testing.T) {
			fixture := newExecutorClaimFixture(t)
			claimant := bindDecision(t, fixture.authority,
				executorDecisionFor(t, holder, "exec"))

			claimed, err := fixture.service.Claim(claimant, ClaimRequest{
				ID: fixture.queued.ID, ExpectedVersion: fixture.queued.Version,
				ClaimID: []byte("holder-claim"), Lease: time.Minute,
				Context: dispatchContext(fixture.now, holder.request),
			})
			if err != nil {
				t.Fatalf("the holder could not claim: %v", err)
			}

			other := bindDecision(t, fixture.authority,
				executorDecisionFor(t, variant.who, "exec"))
			_, err = fixture.service.CompleteClaim(other, CompletionRequest{
				ID: fixture.queued.ID, ExpectedVersion: claimed.Version,
				ClaimID: []byte("holder-claim"),
				Result:  ExecutionResult{Output: json.RawMessage(`{"ok":false}`)},
				Context: dispatchContext(fixture.now, variant.who.request),
			})
			if err == nil {
				t.Fatalf("a principal differing from the claimant only in %s "+
					"completed its claim, so that component is not compared",
					variant.component)
			}

			// And the holder itself still completes, so the refusal above is
			// about the differing component and not about the fixture having
			// made the claim unreportable by anyone.
			if _, err := fixture.service.CompleteClaim(
				claimant, CompletionRequest{
					ID: fixture.queued.ID, ExpectedVersion: claimed.Version,
					ClaimID: []byte("holder-claim"),
					Result: ExecutionResult{
						Output: json.RawMessage(`{"ok":true}`),
					},
					Context: dispatchContext(fixture.now, holder.request),
				}); err != nil {
				t.Fatalf("the holder was refused too, so this case proves "+
					"nothing about %s: %v", variant.component, err)
			}
		})
	}
}

// TestTheEnqueuerCannotReportOnAWorkersClaim pins the narrowing of who may
// report.
//
// The claimant check first shipped accepting the record's own principal
// alongside the claimant, on two stated grounds that are both false: that the
// enqueuer "can already cancel it at any moment", when Cancel refuses while a
// claim is live; and that fabricating its own action's outcome "harms only
// itself", when the replay branch then hands the worker that performed the
// effect a success receipt carrying the fabrication. That is the same failure
// the gate exists to prevent, with a narrower attacker.
//
// The enqueuer is admitted only where the record carries no claimant at all,
// which is the pre-#437 encoding and nothing else.
func TestTheEnqueuerCannotReportOnAWorkersClaim(t *testing.T) {
	fixture := newExecutorClaimFixture(t)
	worker := fixture.namedExecutor(t, "worker")

	claimed, err := fixture.service.Claim(worker, ClaimRequest{
		ID: fixture.queued.ID, ExpectedVersion: fixture.queued.Version,
		ClaimID: []byte("worker-claim"), Lease: time.Minute,
		Context: dispatchContext(fixture.now, "worker-request"),
	})
	if err != nil {
		t.Fatalf("the worker could not claim: %v", err)
	}

	// The enqueuer can read the ClaimID off Status, so presenting it is not the
	// hard part.
	status, err := fixture.service.Status(fixture.enqueuer, StatusRequest{
		ID: fixture.queued.ID, Context: dispatchContext(fixture.now, "request"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(status.ClaimID, []byte("worker-claim")) {
		t.Fatalf("Status does not disclose the claim ID (%q), so this test is "+
			"not exercising the path it describes", status.ClaimID)
	}

	if _, err := fixture.service.CompleteClaim(
		fixture.enqueuer, CompletionRequest{
			ID: fixture.queued.ID, ExpectedVersion: claimed.Version,
			ClaimID: status.ClaimID,
			Result:  ExecutionResult{Output: json.RawMessage(`{"ok":false}`)},
			Context: dispatchContext(fixture.now, "request"),
		}); err == nil {
		t.Fatal("the enqueuer committed an outcome for a claim a worker holds, " +
			"and the worker would then receive a success receipt for it")
	}

	// And the worker still gets its own outcome recorded, which is what makes
	// this a narrowing rather than a refusal of everything.
	done, err := fixture.service.CompleteClaim(worker, CompletionRequest{
		ID: fixture.queued.ID, ExpectedVersion: claimed.Version,
		ClaimID: []byte("worker-claim"),
		Result:  ExecutionResult{Output: json.RawMessage(`{"ok":true}`)},
		Context: dispatchContext(fixture.now, "worker-request"),
	})
	if err != nil {
		t.Fatalf("the claim's holder was refused: %v", err)
	}
	if string(done.Output) != `{"ok":true}` {
		t.Fatalf("the holder's own outcome was not recorded: %s", done.Output)
	}
}

// TestALiveClaimIsNotVisibleToAStranger is the fourth existence oracle, found
// by a review after the first three were closed.
//
// Claiming a live-claimed record answered ErrActionConflict with no standing
// check, on the stated grounds that such a record is already visible through
// Pull. It is not: Pull returns a claimed record only once its lease has
// lapsed. So an execute-holder could guess action IDs and tell a live-claimed
// record from an absent one — cheaply, because a freshly claimed record is at
// version 2 and every wrong guess answers not-found.
func TestALiveClaimIsNotVisibleToAStranger(t *testing.T) {
	fixture := newExecutorClaimFixture(t)
	holder := fixture.namedExecutor(t, "holder")
	stranger := fixture.namedExecutor(t, "stranger")

	claimed, err := fixture.service.Claim(holder, ClaimRequest{
		ID: fixture.queued.ID, ExpectedVersion: fixture.queued.Version,
		ClaimID: []byte("holder-claim"), Lease: time.Minute,
		Context: dispatchContext(fixture.now, "holder-request"),
	})
	if err != nil {
		t.Fatalf("the holder could not claim: %v", err)
	}

	// Pull withholds it, which is what makes the error the only channel.
	page, err := fixture.service.Pull(stranger, PullActionsRequest{
		Limit: 10, Context: dispatchContext(fixture.now, "stranger-request"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Actions) != 0 {
		t.Fatalf("Pull returned a live-claimed record, so this test is not "+
			"about a record the stranger cannot otherwise see: %d actions",
			len(page.Actions))
	}

	fixture.assertIndistinguishable(t, stranger, claimed.Version)
}

// TestTheLoserOfAClaimRaceIsToldNotFound pins a deliberate degradation, which
// is worth a test precisely because it is the losing side of a trade.
//
// Two workers pull the same page and both claim. The winner's claim makes the
// record live-claimed, and Pull withholds live-claimed records — so by the time
// the loser asks, the error is the only channel through which that record's
// existence could be learned, and it has to be the answer an absent action
// gets. The loser therefore receives not-found for work it saw in its own page
// moments earlier.
//
// The alternative was considered and rejected. Answering the loser with
// ErrActionConflict would mean an execute-holder could guess action IDs and
// distinguish a live-claimed record from a nonexistent one, which is cheap:
// every wrong guess answers not-found and a freshly claimed record sits at
// version 2. A misleading error for the loser of a race it can simply retry is
// a smaller cost than a working existence oracle.
//
// **A worker must therefore treat not-found from Claim as "re-pull", not as
// "this action is gone."** Operationally that is what it would do with a
// conflict anyway, which is what makes the trade affordable.
//
// An earlier comment in dispatch_service.go claimed this case could not arise,
// on the grounds that a caller holding the right version never reaches the
// version-mismatch branch. That ignored the race.
func TestTheLoserOfAClaimRaceIsToldNotFound(t *testing.T) {
	fixture := newExecutorClaimFixture(t)
	first := fixture.namedExecutor(t, "first")
	second := fixture.namedExecutor(t, "second")

	page, err := fixture.service.Pull(second, PullActionsRequest{
		Limit: 10, Context: dispatchContext(fixture.now, "second-request"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Actions) != 1 {
		t.Fatalf("the loser must have seen the record in its own page for this "+
			"test to describe a race at all: %d actions", len(page.Actions))
	}
	offered := page.Actions[0]

	if _, err := fixture.service.Claim(first, ClaimRequest{
		ID: offered.ID, ExpectedVersion: offered.Version,
		ClaimID: []byte("first-claim"), Lease: time.Minute,
		Context: dispatchContext(fixture.now, "first-request"),
	}); err != nil {
		t.Fatalf("the winner could not claim: %v", err)
	}

	_, err = fixture.service.Claim(second, ClaimRequest{
		ID: offered.ID, ExpectedVersion: offered.Version,
		ClaimID: []byte("second-claim"), Lease: time.Minute,
		Context: dispatchContext(fixture.now, "second-request"),
	})
	if err == nil {
		t.Fatal("both workers claimed the same action")
	}
	if !shoal.IsErrorCode(err, shoal.ErrorNotFound) {
		t.Fatalf("the loser of a claim race no longer receives not-found. If "+
			"that is deliberate, confirm an execute-holder still cannot "+
			"distinguish a live-claimed record from an absent one — see "+
			"TestALiveClaimIsNotVisibleToAStranger: %v", err)
	}

	// The record itself is untouched by the losing attempt: the winner still
	// holds it under its own claim.
	status, err := fixture.service.Status(fixture.enqueuer, StatusRequest{
		ID: fixture.queued.ID, Context: dispatchContext(fixture.now, "request"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(status.ClaimID, []byte("first-claim")) {
		t.Fatalf("the losing claim changed the holder to %q", status.ClaimID)
	}

	// And a still-queued record does produce a conflict for a stale version,
	// which is why the concealment keys on what Pull shows rather than on
	// standing alone. Nothing is concealed that the caller can already pull.
	fresh := newExecutorClaimFixture(t)
	stale := fresh.namedExecutor(t, "stale")
	_, err = fresh.service.Claim(stale, ClaimRequest{
		ID: fresh.queued.ID, ExpectedVersion: fresh.queued.Version + 7,
		ClaimID: []byte("stale-claim"), Lease: time.Minute,
		Context: dispatchContext(fresh.now, "stale-request"),
	})
	if err == nil {
		t.Fatal("a claim at a version the record never had succeeded")
	}
	if shoal.IsErrorCode(err, shoal.ErrorNotFound) {
		t.Fatalf("a queued record that Pull would hand this caller is "+
			"concealed anyway, which costs an informative error for nothing: %v",
			err)
	}
}

// TestCloningARecordDoesNotShareTheClaimantChain covers the one deep copy the
// claimant fields needed and nothing exercised.
//
// Three of the four are scalars that the struct assignment in
// cloneActionRecord copies correctly on its own. ClaimantOnBehalfOf is a slice,
// so without an explicit copy the clone shares its backing array — and clones
// are what leave this package: Status, Pull and every completion return one. A
// caller mutating the chain it was handed would be editing the record the
// service still holds, and that chain is half of who may report on the claim.
//
// Asserted by mutating the clone, because a test that only compared the two
// would pass against an aliased copy.
func TestCloningARecordDoesNotShareTheClaimantChain(t *testing.T) {
	original := ActionRecord{
		ID:                 []byte("action"),
		ClaimantSubject:    "holder",
		ClaimantActor:      "holder-actor",
		ClaimantOnBehalfOf: []shoal.ID{"delegator", "second"},
	}

	clone := cloneActionRecord(original)
	if len(clone.ClaimantOnBehalfOf) != 2 {
		t.Fatalf("the clone lost the claimant chain: %v", clone.ClaimantOnBehalfOf)
	}
	clone.ClaimantOnBehalfOf[0] = "attacker"
	if original.ClaimantOnBehalfOf[0] != "delegator" {
		t.Fatal("the clone shares the claimant delegation chain with the " +
			"record it was built from, so a caller holding a clone can edit " +
			"half of who may report on the claim")
	}

	// And the scalars survive, so this is not passing because the fields are
	// simply absent from the clone.
	if clone.ClaimantSubject != "holder" || clone.ClaimantActor != "holder-actor" {
		t.Fatalf("the clone did not carry the claimant identity: %+v", clone)
	}
}

// inProcessExecutor runs and returns a fixed result, changing nothing else.
//
// It is not what makes the branch below reachable, and an earlier version of
// this comment claimed it was. The fixture's default remoteBoundExecutor does
// implement ActionExecutor — it only errors when invoked — so resolveAction
// resolves it fine, and the test still catches the both-gates-deleted mutation
// without this binding. What it does is let the primary assertion fire directly
// rather than through a post-condition, which is worth having and is a smaller
// claim.
//
// The two failed reproductions that produced that wrong explanation are worth
// keeping, because both refused for reasons unrelated to the defect: the
// default executor errors on invocation, and breakingExecutor breaks its own
// descriptor so the refusal comes from the post-effect check. A fixture that
// refuses for the wrong reason reads exactly like a fixed bug.
type inProcessExecutor struct{}

func (*inProcessExecutor) Execute(
	_ context.Context, _ Invocation,
) (ExecutionResult, error) {
	return ExecutionResult{Output: json.RawMessage(`{"ok":false}`)}, nil
}

// TestInvokeCannotExecuteOverAnotherPrincipalsClaim closes the second door into
// a terminal write, which narrowing completeClaim alone left open.
//
// Invoke does not go through completeClaim. It calls Claim, and for a record
// already claimed at the ClaimID it presents it calls straight through to
// ExecuteClaim — whose only identity gate was sameActionPrincipal. So the
// enqueuer could still commit over a live foreign claim, with the same
// prerequisite as before (Status discloses the claim ID to every co-principal)
// and the same consequence: the worker's own report is recognised by the replay
// branch and answered with a 200 carrying the enqueuer's output.
//
// Only reachable where an in-process executor is bound. A gateway reference is
// remote-bound, so ExecuteClaim refuses it earlier for want of an
// ActionExecutor — which makes this an ordinary-dispatch hazard rather than a
// gateway one, and is why it survived two rounds of review of the gateway path.
func TestInvokeCannotExecuteOverAnotherPrincipalsClaim(t *testing.T) {
	fixture := newExecutorClaimFixture(t)
	fixture.bindExecutor(t, &inProcessExecutor{})
	worker := fixture.namedExecutor(t, "worker")

	claimed, err := fixture.service.Claim(worker, ClaimRequest{
		ID: fixture.queued.ID, ExpectedVersion: fixture.queued.Version,
		ClaimID: []byte("worker-claim"), Lease: time.Minute,
		Context: dispatchContext(fixture.now, "worker-request"),
	})
	if err != nil {
		t.Fatalf("the worker could not claim: %v", err)
	}

	// The enqueuer reads the claim ID off its own Status call, which is the
	// disclosure this attack needs and which is by design.
	status, err := fixture.service.Status(fixture.enqueuer, StatusRequest{
		ID: fixture.queued.ID, Context: dispatchContext(fixture.now, "request"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(status.ClaimID, []byte("worker-claim")) {
		t.Fatalf("Status did not disclose the claim ID (%q), so this test is "+
			"not exercising the path it describes", status.ClaimID)
	}

	if _, err := fixture.service.Invoke(fixture.enqueuer, InvokeRequest{
		Enqueue: dispatchEnqueue(fixture.now, "request"),
		ClaimID: status.ClaimID, Lease: time.Minute,
	}); err == nil {
		t.Fatal("the enqueuer executed over a live foreign claim through " +
			"Invoke, and the worker would then receive a success receipt for " +
			"an outcome it did not produce")
	}

	// The record is untouched and the worker still records its own outcome.
	after, err := fixture.service.Status(fixture.enqueuer, StatusRequest{
		ID: fixture.queued.ID, Context: dispatchContext(fixture.now, "request"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if after.State != DispatchClaimed {
		t.Fatalf("the refused invoke still moved the record to %s", after.State)
	}
	done, err := fixture.service.CompleteClaim(worker, CompletionRequest{
		ID: fixture.queued.ID, ExpectedVersion: claimed.Version,
		ClaimID: []byte("worker-claim"),
		Result:  ExecutionResult{Output: json.RawMessage(`{"ok":true}`)},
		Context: dispatchContext(fixture.now, "worker-request"),
	})
	if err != nil {
		t.Fatalf("the claim's holder was refused: %v", err)
	}
	if string(done.Output) != `{"ok":true}` {
		t.Fatalf("the holder's own outcome was not recorded: %s", done.Output)
	}
}

// TestTheEnqueuerStillInvokesItsOwnWork is the other side of the gate above,
// and the reason it is holdsClaimOn rather than a refusal of the enqueuer.
//
// The synchronous path is the enqueuer claiming and executing its own action in
// one call. That must keep working: it is what every in-process action uses,
// and a gate that refused it would break the common case to close an edge one.
func TestTheEnqueuerStillInvokesItsOwnWork(t *testing.T) {
	fixture := newExecutorClaimFixture(t)
	fixture.bindExecutor(t, &inProcessExecutor{})

	result, err := fixture.service.Invoke(fixture.enqueuer, InvokeRequest{
		Enqueue: dispatchEnqueue(fixture.now, "request"),
		ClaimID: []byte("own-claim"), Lease: time.Minute,
	})
	if err != nil {
		t.Fatalf("the enqueuer could not invoke its own work: %v", err)
	}
	if result.State != DispatchSucceeded {
		t.Fatalf("the synchronous path did not complete: state=%s", result.State)
	}
	if string(result.Output) != `{"ok":false}` {
		t.Fatalf("the executor's result was not recorded: %s", result.Output)
	}
}

// TestAnEnqueuerCannotReplayAnotherPrincipalsClaim covers the replay-branch half
// of the narrowing, which had no test of its own.
//
// Claim's replay branch asks holdsClaimOn rather than standingOn, and the two
// differ only for the enqueuer on a claim a worker holds. Swapping the
// predicate back left the suite green, because the existing replay test uses a
// stranger and a stranger is refused by either.
func TestAnEnqueuerCannotReplayAnotherPrincipalsClaim(t *testing.T) {
	fixture := newExecutorClaimFixture(t)
	worker := fixture.namedExecutor(t, "worker")

	if _, err := fixture.service.Claim(worker, ClaimRequest{
		ID: fixture.queued.ID, ExpectedVersion: fixture.queued.Version,
		ClaimID: []byte("worker-claim"), Lease: time.Minute,
		Context: dispatchContext(fixture.now, "worker-request"),
	}); err != nil {
		t.Fatalf("the worker could not claim: %v", err)
	}

	// Exactly the request the worker would replay, from the enqueuer.
	replayed, err := fixture.service.Claim(fixture.enqueuer, ClaimRequest{
		ID: fixture.queued.ID, ExpectedVersion: fixture.queued.Version,
		ClaimID: []byte("worker-claim"), Lease: time.Minute,
		Context: dispatchContext(fixture.now, "request"),
	})
	if err == nil {
		t.Fatalf("the replay branch handed the enqueuer a live claim it does "+
			"not hold: state=%s claim=%q", replayed.State, replayed.ClaimID)
	}
}

// TestAForgedRecordCannotRetrieveAnothersAction closes the door that opens when
// a method takes a record instead of an ID.
//
// ExecuteClaim is handed an ActionRecord, and its two entry checks compare the
// caller against *that argument*. A caller that writes its own principal into
// both the action slots and the claimant slots satisfies both trivially. The
// replay branch below then loads the record by ID and returns it — the victim's
// input, executor key, claim ID and claimant chain — for any (ID, ClaimID,
// version, fence) tuple the caller can produce, and Pull hands every one of
// those to any execute-holder once a lease lapses.
//
// So the checks that bind are the ones below the load, and they have to sit
// above the branch that returns the record. The entry checks stay as a cheap
// early refusal on the normal path, where the argument is a record this service
// just produced.
//
// The refusal is not-found rather than unauthorized, because the three answers
// a forged argument would otherwise separate — absent, present under another
// claim, present under this one — are the same oracle the refusals in Claim
// were normalised to close.
func TestAForgedRecordCannotRetrieveAnothersAction(t *testing.T) {
	fixture := newExecutorClaimFixture(t)
	fixture.bindExecutor(t, &inProcessExecutor{})
	worker := fixture.namedExecutor(t, "worker")

	claimed, err := fixture.service.Claim(worker, ClaimRequest{
		ID: fixture.queued.ID, ExpectedVersion: fixture.queued.Version,
		ClaimID: []byte("worker-claim"), Lease: time.Minute,
		Context: dispatchContext(fixture.now, "worker-request"),
	})
	if err != nil {
		t.Fatalf("the worker could not claim: %v", err)
	}
	// Completed, so the record is terminal at claimed.Version+1 — the replay
	// branch's own condition, and the state in which it returns the record.
	if _, err := fixture.service.CompleteClaim(worker, CompletionRequest{
		ID: fixture.queued.ID, ExpectedVersion: claimed.Version,
		ClaimID: []byte("worker-claim"),
		Result:  ExecutionResult{Output: json.RawMessage(`{"ok":true}`)},
		Context: dispatchContext(fixture.now, "worker-request"),
	}); err != nil {
		t.Fatalf("the worker could not complete: %v", err)
	}

	stranger := bindDecision(t, fixture.authority, dispatchDecisionFor(t,
		principal{
			subject: "stranger-subject", actor: "stranger-actor",
			request: "stranger-request",
		}, auth.OperationExecute, auth.OperationInvoke))

	// Every field here is either knowable from a pull page or chosen by the
	// attacker. resolveAction is hardcoded to OperationInvoke, so the caller
	// needs invoke as well as execute — which makes the reachable set wider
	// than execute-holders, not narrower.
	forged := ActionRecord{
		ID: fixture.queued.ID, Version: claimed.Version,
		ClaimFence: claimed.ClaimFence, ClaimID: []byte("worker-claim"),
		State: DispatchClaimed, ClaimLeaseUntil: fixture.now.Add(time.Hour),
		Deadline: fixture.now.Add(time.Hour), AgentID: claimed.AgentID,
		AgentGeneration: claimed.AgentGeneration, Capability: claimed.Capability,
		Action: claimed.Action, SourceID: claimed.SourceID,
		PolicyID: claimed.PolicyID, ObjectID: claimed.ObjectID,
		Subject: "stranger-subject", Actor: "stranger-actor",
		ClaimantSubject: "stranger-subject", ClaimantActor: "stranger-actor",
	}

	got, err := fixture.service.ExecuteClaim(stranger, forged)
	if err == nil {
		t.Fatalf("a forged record argument returned another principal's "+
			"action: input=%s output=%s claim=%q claimant=%q",
			got.Input, got.Output, got.ClaimID, got.ClaimantSubject)
	}
	if !shoal.IsErrorCode(err, shoal.ErrorNotFound) {
		t.Fatalf("the refusal separates an existing action from an absent one, "+
			"which is the oracle a forged argument would otherwise probe: %v", err)
	}
}

// TestARecordNamesTheOperationThatAuthorizedItsTransition joins the two halves
// of the fix that made #437's capability work end to end.
//
// The event publisher authorizes the publishing decision against an operation
// it selects per event kind. It used to hardcode invoke for a claim, which a
// worker holding only execute does not have — so the publication failed, and a
// failed publication is returned as ErrActionCommitted, meaning the transition
// was durably written and the worker was told to reconcile an outcome that had
// in fact been recorded. The publisher reads record.TransitionOperation now,
// and this is the test that the record carries the truth for it to read.
//
// AuthorizedOperations cannot serve that purpose and this asserts why:
// decisionOperations returns the operation it is handed without consulting the
// decision, so the list asserts a capability rather than observing one. It
// happened to be right while every claim was authorized under invoke.
//
// Without this, applyClaim's write could be replaced by a hardcoded invoke and
// only the publisher's own tests would notice — and they set the field by hand,
// so they would not. The claim half of this test is what catches that;
// applyExecutionResult's write is not distinguishable by any reachable path,
// for the reason recorded beside it.
func TestARecordNamesTheOperationThatAuthorizedItsTransition(t *testing.T) {
	t.Run("a claim taken under execute", func(t *testing.T) {
		fixture := newExecutorClaimFixture(t)
		worker := fixture.namedExecutor(t, "worker")

		claimed, err := fixture.service.Claim(worker, ClaimRequest{
			ID: fixture.queued.ID, ExpectedVersion: fixture.queued.Version,
			ClaimID: []byte("worker-claim"), Lease: time.Minute,
			Context: dispatchContext(fixture.now, "worker-request"),
		})
		if err != nil {
			t.Fatalf("the worker could not claim: %v", err)
		}
		if claimed.TransitionOperation != auth.OperationExecute {
			t.Fatalf("the claim records %q, so the publisher authorizes the "+
				"worker against an operation it does not hold and the claim "+
				"returns 503", claimed.TransitionOperation)
		}
		// And the record's operation list carries execute rather than
		// asserting invoke, because the publisher cross-checks the two.
		if !slices.Contains(claimed.AuthorizedOperations, auth.OperationExecute) {
			t.Fatalf("AuthorizedOperations is %v and does not carry execute, "+
				"so the publisher's provenance check refuses the operation the "+
				"record names", claimed.AuthorizedOperations)
		}
		if slices.Contains(claimed.AuthorizedOperations, auth.OperationInvoke) {
			t.Fatalf("AuthorizedOperations is %v and asserts invoke, which no "+
				"principal in this record's history held",
				claimed.AuthorizedOperations)
		}

		// The completion carries it too. This is a consistency check and not
		// coverage of applyExecutionResult's write: cloneActionRecord carries
		// the claim's operation forward, and no reachable path completes under
		// an operation different from the one that claimed — ExecuteClaim
		// requires its caller to be both the record's principal and its
		// claimant, and completeClaim resolves the same routes in the same
		// order. So deleting that assignment is not observable, which the
		// production comment beside it now says. Asserting it here anyway is
		// worth the line: if a future route completes under a different
		// operation, this is what notices the field went stale.
		done, err := fixture.service.CompleteClaim(worker, CompletionRequest{
			ID: fixture.queued.ID, ExpectedVersion: claimed.Version,
			ClaimID: []byte("worker-claim"),
			Result:  ExecutionResult{Output: json.RawMessage(`{"ok":true}`)},
			Context: dispatchContext(fixture.now, "worker-request"),
		})
		if err != nil {
			t.Fatalf("the worker could not complete: %v", err)
		}
		if done.TransitionOperation != auth.OperationExecute {
			t.Fatalf("the completion records %q", done.TransitionOperation)
		}
	})

	t.Run("a claim taken by the enqueuer under invoke", func(t *testing.T) {
		fixture := newExecutorClaimFixture(t)
		claimed, err := fixture.service.Claim(fixture.enqueuer, ClaimRequest{
			ID: fixture.queued.ID, ExpectedVersion: fixture.queued.Version,
			ClaimID: []byte("own-claim"), Lease: time.Minute,
			Context: dispatchContext(fixture.now, "request"),
		})
		if err != nil {
			t.Fatalf("the enqueuer could not claim: %v", err)
		}
		// The enqueuer holds dispatch and invoke and not execute, so it takes
		// the invoke route and the record must say so — asserting execute for
		// everything would be as wrong as asserting invoke for everything.
		if claimed.TransitionOperation != auth.OperationInvoke {
			t.Fatalf("the enqueuer's own claim records %q, want invoke",
				claimed.TransitionOperation)
		}
	})
}

// TestAnUpgradeFindsARecordWithNoTransitionOperation covers the one path where
// the new field is absent on a live record, which is the case that reaches
// production and never reaches a test written after the field exists.
//
// A record claimed by a build without TransitionOperation decodes with it
// empty. ActionRecorder.RecordAction validates the operation before anything
// else, so passing the empty value straight through fails the audit and
// surfaces as 503 — and the route that gets there is Invoke's live-claim
// shortcut, which calls ExecuteClaim directly for a record already claimed at
// the ClaimID presented. The publisher has the same fallback for the same
// reason; this was the one consumer missing it.
//
// Simulated by clearing the field in the store rather than by constructing a
// record by hand, so the rest of the record is exactly what this build writes
// and only the one field differs — which is what an upgrade actually looks
// like.
func TestAnUpgradeFindsARecordWithNoTransitionOperation(t *testing.T) {
	fixture := newExecutorClaimFixture(t)
	fixture.bindExecutor(t, &inProcessExecutor{})

	claimed, err := fixture.service.Claim(fixture.enqueuer, ClaimRequest{
		ID: fixture.queued.ID, ExpectedVersion: fixture.queued.Version,
		ClaimID: []byte("own-claim"), Lease: time.Minute,
		Context: dispatchContext(fixture.now, "request"),
	})
	if err != nil {
		t.Fatalf("the enqueuer could not claim: %v", err)
	}
	if claimed.TransitionOperation == "" {
		t.Fatal("the claim did not record an operation, so clearing it below " +
			"changes nothing and this test would pass vacuously")
	}

	// What the previous build left behind: everything else as written, this
	// one field absent.
	stored := fixture.dispatchStore.records[string(fixture.queued.ID)]
	stored.TransitionOperation = ""
	fixture.dispatchStore.records[string(fixture.queued.ID)] = stored

	// Invoke's live-claim shortcut: same ClaimID, lease still live, so it goes
	// straight to ExecuteClaim without re-claiming.
	result, err := fixture.service.Invoke(fixture.enqueuer, InvokeRequest{
		Enqueue: dispatchEnqueue(fixture.now, "request"),
		ClaimID: []byte("own-claim"), Lease: time.Minute,
	})
	if err != nil {
		t.Fatalf("a record claimed before TransitionOperation existed cannot "+
			"be executed after an upgrade: %v", err)
	}
	if result.State != DispatchSucceeded {
		t.Fatalf("the upgraded record did not complete: state=%s", result.State)
	}
}

// lapsedClaimant claims an action with a one-nanosecond lease and advances the
// clock past it, returning the worker, the fence it held, and the record as it
// stood after the claim.
//
// It leaves the worker as the record's *current* claimant with a lapsed lease.
// It does not reclaim the record, so a test needing the history path — where
// the claimant fields name someone else — must take the record with a second
// worker itself. An earlier version of this comment claimed the helper did
// that, and named a function that does not exist; five tests that read it were
// exercising a still-current claimant rather than the path they described.
func (f *executorClaimFixture) lapsedClaimant(
	t *testing.T, name string,
) (context.Context, uint64, ActionRecord) {
	t.Helper()
	worker := f.namedExecutor(t, name)
	claimed, err := f.service.Claim(worker, ClaimRequest{
		ID: f.queued.ID, ExpectedVersion: f.queued.Version,
		ClaimID: []byte(name + "-claim"), Lease: time.Nanosecond,
		Context: dispatchContext(f.now, name+"-request"),
	})
	if err != nil {
		t.Fatalf("%s could not claim: %v", name, err)
	}
	f.advance(t, time.Second)
	return worker, claimed.ClaimFence, claimed
}

// TestALapsedClaimantRecordsWhatItAttempted is #438's first acceptance
// criterion: a worker whose renewal is refused mid-effect records what it
// attempted, and an operator finds it on the action.
//
// The worker cannot use CompleteClaim — that route is gated on the fence it
// just lost — and EffectPossible already says durably that an effect may have
// happened. What was missing is the detail: what was attempted, against which
// target, and the handle the target returned, which is the thing an operator
// takes to the other system.
func TestALapsedClaimantRecordsWhatItAttempted(t *testing.T) {
	fixture := newExecutorClaimFixture(t)
	worker, fence, claimed := fixture.lapsedClaimant(t, "worker")

	// A second worker takes the record, so the first is no longer its
	// claimant. This is the situation the route exists for, and the one the
	// issue's own "require a claim_id the record has seen" rule could not
	// express: the record keeps only the current claim ID.
	second := fixture.namedExecutor(t, "second")
	reclaimed, err := fixture.service.Claim(second, ClaimRequest{
		ID: fixture.queued.ID, ExpectedVersion: claimed.Version,
		ClaimID: []byte("second-claim"), Lease: time.Minute,
		Context: dispatchContext(fixture.now, "second-request"),
	})
	if err != nil {
		t.Fatalf("the second worker could not take the lapsed claim: %v", err)
	}
	if reclaimed.ClaimantSubject == "worker-subject" {
		t.Fatal("the record still names the first worker as claimant, so this " +
			"test is not exercising a lapsed claimant")
	}

	reported, err := fixture.service.ReportAmbiguity(worker, AmbiguityRequest{
		ID: fixture.queued.ID, ExpectedVersion: reclaimed.Version,
		ClaimFence: fence, Outcome: AmbiguityOutcomeUnknown,
		Target: "payments.example.test", Reference: "ch_1A2b3C",
		Context: dispatchContext(fixture.now, "worker-request"),
	})
	if err != nil {
		t.Fatalf("a worker that held the claim at fence %d cannot report: %v",
			fence, err)
	}

	if len(reported.AmbiguityReports) != 1 {
		t.Fatalf("report count = %d, want 1", len(reported.AmbiguityReports))
	}
	report := reported.AmbiguityReports[0]
	if report.ClaimFence != fence ||
		report.Outcome != AmbiguityOutcomeUnknown ||
		report.Target != "payments.example.test" ||
		report.Reference != "ch_1A2b3C" {
		t.Fatalf("the report lost what the worker said: %+v", report)
	}
	// Attributed from the decision, never from the request, so a report says
	// who made it rather than who claims to have made it.
	if report.Subject != "worker-subject" || report.Actor != "worker-actor" {
		t.Fatalf("the report is not attributed to the reporter: %+v", report)
	}

	// And an operator finds it. Status is the surface an operator reads, and
	// the report has to be there rather than only in the service's return.
	seen, err := fixture.service.Status(fixture.enqueuer, StatusRequest{
		ID: fixture.queued.ID, Context: dispatchContext(fixture.now, "request"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(seen.AmbiguityReports) != 1 ||
		seen.AmbiguityReports[0].Reference != "ch_1A2b3C" {
		t.Fatalf("an operator reading Status does not find the report: %+v",
			seen.AmbiguityReports)
	}
}

// TestAnAmbiguityReportDoesNotTransitionTheAction is the second acceptance
// criterion, and the reason this route is safe to expose to a caller that has
// lost its claim.
//
// A worker without the fence has no standing to assert an outcome, only to say
// what it tried. So the state, the claim and EffectPossible are all unchanged,
// and in particular the fence does not advance — a report must not invalidate
// whoever currently holds the claim.
func TestAnAmbiguityReportDoesNotTransitionTheAction(t *testing.T) {
	fixture := newExecutorClaimFixture(t)
	worker, fence, claimed := fixture.lapsedClaimant(t, "worker")

	before, err := fixture.service.Status(fixture.enqueuer, StatusRequest{
		ID: fixture.queued.ID, Context: dispatchContext(fixture.now, "request"),
	})
	if err != nil {
		t.Fatal(err)
	}

	reported, err := fixture.service.ReportAmbiguity(worker, AmbiguityRequest{
		ID: fixture.queued.ID, ExpectedVersion: claimed.Version,
		ClaimFence: fence, Outcome: AmbiguityEffectObserved,
		Context: dispatchContext(fixture.now, "worker-request"),
	})
	if err != nil {
		t.Fatalf("the lapsed claimant could not report: %v", err)
	}

	if reported.State != before.State {
		t.Fatalf("the report moved the action from %s to %s",
			before.State, reported.State)
	}
	if reported.ClaimFence != before.ClaimFence {
		t.Fatalf("the report advanced the fence from %d to %d, which would "+
			"invalidate a live claimant's right to complete",
			before.ClaimFence, reported.ClaimFence)
	}
	if !bytes.Equal(reported.ClaimID, before.ClaimID) {
		t.Fatalf("the report changed the claim from %q to %q",
			before.ClaimID, reported.ClaimID)
	}
	if reported.EffectPossible != before.EffectPossible {
		t.Fatalf("the report changed EffectPossible from %v to %v",
			before.EffectPossible, reported.EffectPossible)
	}
	// An AmbiguityEffectObserved report says the work succeeded. The record
	// deliberately does not: this route cannot reach a terminal state, because
	// a worker without the fence cannot be allowed to assert one.
	if reported.State.terminal() {
		t.Fatal("a report reached a terminal state, so a worker that lost its " +
			"fence can assert an outcome after all")
	}

	// The version does advance, because this writes to the record and must
	// serialise against concurrent writers.
	if reported.Version != before.Version+1 {
		t.Fatalf("version = %d, want %d", reported.Version, before.Version+1)
	}
}

// TestOnlyAPrincipalTheRecordHasSeenMayReport is the third acceptance
// criterion: a caller presenting a fence the record never held is refused.
//
// Refused as not-found rather than with a distinguishable error, because this
// route is reachable by every principal authorized to execute the descriptor.
// Telling one of them "that fence is not yours" confirms both that the action
// exists and which fences it has been through.
func TestOnlyAPrincipalTheRecordHasSeenMayReport(t *testing.T) {
	fixture := newExecutorClaimFixture(t)
	worker, fence, claimed := fixture.lapsedClaimant(t, "worker")

	for _, probe := range []struct {
		name    string
		caller  func() context.Context
		request func() AmbiguityRequest
	}{
		{
			name: "a principal that never claimed",
			caller: func() context.Context {
				return fixture.namedExecutor(t, "stranger")
			},
			request: func() AmbiguityRequest {
				return AmbiguityRequest{
					ID: fixture.queued.ID, ExpectedVersion: claimed.Version,
					ClaimFence: fence, Outcome: AmbiguityOutcomeUnknown,
					Context: dispatchContext(fixture.now, "stranger-request"),
				}
			},
		},
		{
			name:   "a fence the record never reached",
			caller: func() context.Context { return worker },
			request: func() AmbiguityRequest {
				return AmbiguityRequest{
					ID: fixture.queued.ID, ExpectedVersion: claimed.Version,
					ClaimFence: fence + 41, Outcome: AmbiguityOutcomeUnknown,
					Context: dispatchContext(fixture.now, "worker-request"),
				}
			},
		},
		{
			name:   "an action that does not exist",
			caller: func() context.Context { return worker },
			request: func() AmbiguityRequest {
				return AmbiguityRequest{
					ID: []byte("never-existed"), ExpectedVersion: 1,
					ClaimFence: fence, Outcome: AmbiguityOutcomeUnknown,
					Context: dispatchContext(fixture.now, "worker-request"),
				}
			},
		},
	} {
		t.Run(probe.name, func(t *testing.T) {
			_, err := fixture.service.ReportAmbiguity(
				probe.caller(), probe.request())
			if err == nil {
				t.Fatal("the report was accepted")
			}
			if !shoal.IsErrorCode(err, shoal.ErrorNotFound) {
				t.Fatalf("refusal is distinguishable from an absent action, "+
					"which tells an execute-holder the action exists: %v", err)
			}
		})
	}
}

// TestASecondReportDoesNotOverwriteTheFirst is the issue's last listed test,
// and it is a requirement rather than an implementation detail.
//
// Two reports under one fence mean the worker retried. An operator wants both:
// collapsing them would hide that the worker made two attempts at the same
// target, which is exactly the shape of a double effect.
func TestASecondReportDoesNotOverwriteTheFirst(t *testing.T) {
	fixture := newExecutorClaimFixture(t)
	worker, fence, claimed := fixture.lapsedClaimant(t, "worker")

	first, err := fixture.service.ReportAmbiguity(worker, AmbiguityRequest{
		ID: fixture.queued.ID, ExpectedVersion: claimed.Version,
		ClaimFence: fence, Outcome: AmbiguityOutcomeUnknown,
		Reference: "first-attempt",
		Context:   dispatchContext(fixture.now, "worker-request"),
	})
	if err != nil {
		t.Fatalf("the first report was refused: %v", err)
	}

	// A different outcome under the same fence, which is what a retry that got
	// further looks like.
	second, err := fixture.service.ReportAmbiguity(worker, AmbiguityRequest{
		ID: fixture.queued.ID, ExpectedVersion: first.Version,
		ClaimFence: fence, Outcome: AmbiguityEffectObserved,
		Reference: "second-attempt",
		Context:   dispatchContext(fixture.now, "worker-request"),
	})
	if err != nil {
		t.Fatalf("the second report was refused: %v", err)
	}
	if len(second.AmbiguityReports) != 2 {
		t.Fatalf("report count = %d, want 2 — the second overwrote the first, "+
			"which hides that the worker attempted the target twice",
			len(second.AmbiguityReports))
	}
	if second.AmbiguityReports[0].Reference != "first-attempt" ||
		second.AmbiguityReports[1].Reference != "second-attempt" {
		t.Fatalf("the reports are not in the order received: %+v",
			second.AmbiguityReports)
	}
}

// TestAnAmbiguityReportRefusesTargetControlledBytes pins the closed shape.
//
// Target and Reference describe a system Shoal does not control, so both values
// are target-controlled and both reach the durable record and the team
// overview. The design doc reaches the same conclusion for Output: a closed,
// bounded shape gives a hostile or merely verbose target nothing.
func TestAnAmbiguityReportRefusesTargetControlledBytes(t *testing.T) {
	for _, probe := range []struct {
		name   string
		mutate func(*AmbiguityRequest)
	}{
		{"an outcome outside the vocabulary", func(r *AmbiguityRequest) {
			r.Outcome = AmbiguityOutcome("probably_fine")
		}},
		{"an empty outcome", func(r *AmbiguityRequest) { r.Outcome = "" }},
		{"a target past its bound", func(r *AmbiguityRequest) {
			r.Target = strings.Repeat("t", MaxAmbiguityTargetBytes+1)
		}},
		{"a reference past its bound", func(r *AmbiguityRequest) {
			r.Reference = strings.Repeat("r", MaxAmbiguityReferenceBytes+1)
		}},
		{"a control character in the target", func(r *AmbiguityRequest) {
			r.Target = "host\x1b[31m"
		}},
		{"a newline in the reference", func(r *AmbiguityRequest) {
			r.Reference = "ref\nsecond line"
		}},
		{"no fence", func(r *AmbiguityRequest) { r.ClaimFence = 0 }},
	} {
		t.Run(probe.name, func(t *testing.T) {
			fixture := newExecutorClaimFixture(t)
			worker, fence, claimed := fixture.lapsedClaimant(t, "worker")
			request := AmbiguityRequest{
				ID: fixture.queued.ID, ExpectedVersion: claimed.Version,
				ClaimFence: fence, Outcome: AmbiguityOutcomeUnknown,
				Context: dispatchContext(fixture.now, "worker-request"),
			}
			probe.mutate(&request)
			if _, err := fixture.service.ReportAmbiguity(
				worker, request); err == nil {
				t.Fatal("accepted")
			}
		})
	}

	// And the shape it does accept, so the refusals above are not passing
	// because nothing is acceptable.
	fixture := newExecutorClaimFixture(t)
	worker, fence, claimed := fixture.lapsedClaimant(t, "worker")
	if _, err := fixture.service.ReportAmbiguity(worker, AmbiguityRequest{
		ID: fixture.queued.ID, ExpectedVersion: claimed.Version,
		ClaimFence: fence, Outcome: AmbiguityRequestNotSent,
		Target: "queue.example.test", Reference: "msg-0001",
		Context: dispatchContext(fixture.now, "worker-request"),
	}); err != nil {
		t.Fatalf("a well-formed report was refused: %v", err)
	}
}

// TestTheClaimHistoryIsBoundedAndDropsTheOldest covers the cap, which exists
// for a sharper reason than tidiness.
//
// encodeAction refuses a record past 3*MaxActionPayloadBytes, so an unbounded
// history would eventually make a repeatedly re-claimed action *unwritable* —
// bricking the record rather than merely growing it. The oldest entry is
// dropped because the most recent holders are the ones whose effects may still
// be unreconciled.
func TestTheClaimHistoryIsBoundedAndDropsTheOldest(t *testing.T) {
	fixture := newExecutorClaimFixture(t)
	version := fixture.queued.Version
	var firstFence uint64
	// The most recent holder that is *not* the current claimant — one behind
	// the final iteration — which is what the history lookup exists for.
	var priorWorker context.Context
	var priorFence uint64
	var priorName string
	var lastWorker context.Context
	var lastFence uint64
	var lastName string

	// Two more claims than the bound. The first claim appends no prior holder
	// — there is none — so N+1 claims produce only N history entries and would
	// not overflow at all. An earlier version of this loop ran to N+1 and
	// concluded the cap dropped the wrong end, when nothing had been dropped.
	for index := 0; index <= MaxActionClaimHistory+1; index++ {
		name := fmt.Sprintf("worker-%d", index)
		worker := fixture.namedExecutor(t, name)
		claimed, err := fixture.service.Claim(worker, ClaimRequest{
			ID: fixture.queued.ID, ExpectedVersion: version,
			ClaimID: []byte(name), Lease: time.Nanosecond,
			Context: dispatchContext(fixture.now, name+"-request"),
		})
		if err != nil {
			t.Fatalf("%s could not claim: %v", name, err)
		}
		version = claimed.Version
		if index == 0 {
			firstFence = claimed.ClaimFence
		}
		priorWorker, priorFence, priorName = lastWorker, lastFence, lastName
		lastWorker, lastFence, lastName = worker, claimed.ClaimFence, name
		fixture.advance(t, time.Second)
	}

	current, err := fixture.service.Status(fixture.enqueuer, StatusRequest{
		ID: fixture.queued.ID, Context: dispatchContext(fixture.now, "request"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(current.ClaimHistory) != MaxActionClaimHistory {
		t.Fatalf("history length = %d, want %d",
			len(current.ClaimHistory), MaxActionClaimHistory)
	}
	// The oldest went, not the newest.
	for _, holder := range current.ClaimHistory {
		if holder.ClaimFence == firstFence {
			t.Fatalf("the oldest holder (fence %d) is still retained while the "+
				"history is full, so the cap drops the wrong end", firstFence)
		}
	}
	// The most recent *prior* holder can still report, which is the whole
	// point of retaining any of them.
	//
	// Prior, not current. An earlier version of this used the final loop
	// iteration's worker, which is the record's current claimant at the
	// current fence — so it passed through sameClaimantPrincipal and never
	// consulted the history at all. Disabling the entire history lookup in
	// heldClaimAt left it green.
	if priorWorker == nil {
		t.Fatal("the loop did not retain a prior holder, so this case checks " +
			"nothing")
	}
	if priorFence == current.ClaimFence {
		t.Fatalf("the prior holder's fence (%d) is the current one, so this "+
			"reaches the claimant branch rather than the history",
			priorFence)
	}
	if _, err := fixture.service.ReportAmbiguity(
		priorWorker, AmbiguityRequest{
			ID: fixture.queued.ID, ClaimFence: priorFence,
			Outcome: AmbiguityOutcomeUnknown,
			// The request context's ID must match the reporting decision's,
			// which is minted per worker name.
			Context: dispatchContext(fixture.now, priorName+"-request"),
		}); err != nil {
		t.Fatalf("the most recent prior holder cannot report, so retaining "+
			"the history buys nothing: %v", err)
	}

	// The dropped holder cannot, and is refused the same way a stranger is.
	dropped := fixture.namedExecutor(t, "worker-0")
	_, err = fixture.service.ReportAmbiguity(dropped, AmbiguityRequest{
		ID: fixture.queued.ID, ExpectedVersion: current.Version,
		ClaimFence: firstFence, Outcome: AmbiguityOutcomeUnknown,
		Context: dispatchContext(fixture.now, "worker-0-request"),
	})
	if err == nil {
		t.Fatal("a holder dropped from the history can still report")
	}
	if !shoal.IsErrorCode(err, shoal.ErrorNotFound) {
		t.Fatalf("a dropped holder is refused distinguishably: %v", err)
	}
}

// TestReportsAreBoundedPerAction covers the other cap. The report list refuses
// rather than dropping, because a report is evidence an operator is going to
// read and silently discarding the first is worse than refusing the ninth.
func TestReportsAreBoundedPerAction(t *testing.T) {
	fixture := newExecutorClaimFixture(t)
	worker, fence, claimed := fixture.lapsedClaimant(t, "worker")
	version := claimed.Version

	for index := 0; index < MaxActionAmbiguityReports; index++ {
		reported, err := fixture.service.ReportAmbiguity(
			worker, AmbiguityRequest{
				ID: fixture.queued.ID, ExpectedVersion: version,
				ClaimFence: fence, Outcome: AmbiguityOutcomeUnknown,
				Reference: fmt.Sprintf("attempt-%d", index),
				Context:   dispatchContext(fixture.now, "worker-request"),
			})
		if err != nil {
			t.Fatalf("report %d was refused: %v", index, err)
		}
		version = reported.Version
	}

	_, err := fixture.service.ReportAmbiguity(worker, AmbiguityRequest{
		ID: fixture.queued.ID, ExpectedVersion: version,
		ClaimFence: fence, Outcome: AmbiguityOutcomeUnknown,
		Reference: "one-too-many",
		Context:   dispatchContext(fixture.now, "worker-request"),
	})
	if err == nil {
		t.Fatalf("a %dth report was accepted, so the list is unbounded and an "+
			"action can be made unwritable", MaxActionAmbiguityReports+1)
	}
	if !shoal.IsErrorCode(err, shoal.ErrorInvalidArgument) {
		t.Fatalf("the refusal is not a bound violation: %v", err)
	}

	// The reports already recorded are intact — refusing the next one must not
	// discard the ones an operator needs.
	current, err := fixture.service.Status(fixture.enqueuer, StatusRequest{
		ID: fixture.queued.ID, Context: dispatchContext(fixture.now, "request"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(current.AmbiguityReports) != MaxActionAmbiguityReports {
		t.Fatalf("report count = %d after a refusal, want %d",
			len(current.AmbiguityReports), MaxActionAmbiguityReports)
	}
}

// TestAnAdmissionCannotBeReportedAsAmbiguous keeps the admission namespace out.
//
// An admission grant is reported through AdmissionService.Report, which has its
// own terminal semantics. Refused as not-found for the same reason every other
// dispatch route refuses one: the IDs are computable from a principal tuple, so
// a distinguishable answer is a probe.
func TestAnAdmissionCannotBeReportedAsAmbiguous(t *testing.T) {
	fixture := newExecutorClaimFixture(t)
	worker, fence, claimed := fixture.lapsedClaimant(t, "worker")

	// Marked as an admission by the durable field isAdmission actually reads —
	// a non-empty AdmittedEffects — rather than by its identity, which is the
	// distinction the dispatch service already makes deliberately: the
	// reserved span held ordinary actions before it was reserved.
	stored := fixture.dispatchStore.records[string(fixture.queued.ID)]
	stored.AdmittedEffects = Effects{EffectReadsCorpus}
	fixture.dispatchStore.records[string(fixture.queued.ID)] = stored

	_, err := fixture.service.ReportAmbiguity(worker, AmbiguityRequest{
		ID: fixture.queued.ID, ExpectedVersion: claimed.Version,
		ClaimFence: fence, Outcome: AmbiguityOutcomeUnknown,
		Context: dispatchContext(fixture.now, "worker-request"),
	})
	if err == nil {
		t.Fatal("an admission accepted a dispatch ambiguity report")
	}
	if !shoal.IsErrorCode(err, shoal.ErrorNotFound) {
		t.Fatalf("an admission is refused distinguishably: %v", err)
	}
}

// TestALapsedClaimantReportsWithoutKnowingTheVersion is the test the first
// version of this route did not have, and its absence made the route unusable
// by the only caller it exists for.
//
// A lapsed claimant cannot learn the record's current version. Status needs
// OperationDispatch and the record's own principal; Pull withholds
// live-claimed records, so a reclaimed action is absent from its page; and
// ErrActionConflict carries no version. The other tests here all pass a
// version obtained by being the party that reclaimed, which a real worker
// never is — a fixture that can express something its subject cannot.
//
// So the version is optional. What must not change under a report is the
// claim, and the store asserts that through ExpectedFence.
func TestALapsedClaimantReportsWithoutKnowingTheVersion(t *testing.T) {
	fixture := newExecutorClaimFixture(t)
	worker, fence, claimed := fixture.lapsedClaimant(t, "worker")

	// Someone else takes the record, moving the version past anything the
	// first worker saw.
	second := fixture.namedExecutor(t, "second")
	if _, err := fixture.service.Claim(second, ClaimRequest{
		ID: fixture.queued.ID, ExpectedVersion: claimed.Version,
		ClaimID: []byte("second-claim"), Lease: time.Minute,
		Context: dispatchContext(fixture.now, "second-request"),
	}); err != nil {
		t.Fatalf("the second worker could not reclaim: %v", err)
	}

	// Everything the lapsed worker actually knows: the action ID and the fence
	// it held. No version.
	reported, err := fixture.service.ReportAmbiguity(worker, AmbiguityRequest{
		ID: fixture.queued.ID, ClaimFence: fence,
		Outcome: AmbiguityOutcomeUnknown, Reference: "ch_1A2b3C",
		Context: dispatchContext(fixture.now, "worker-request"),
	})
	if err != nil {
		t.Fatalf("a lapsed claimant cannot report without a version it has no "+
			"way to obtain: %v", err)
	}
	if len(reported.AmbiguityReports) != 1 ||
		reported.AmbiguityReports[0].Reference != "ch_1A2b3C" {
		t.Fatalf("the report was not recorded: %+v", reported.AmbiguityReports)
	}

	// The stale version it does know is still refused when pinned, so the
	// optional pin is a real check rather than ignored.
	if _, err := fixture.service.ReportAmbiguity(worker, AmbiguityRequest{
		ID: fixture.queued.ID, ExpectedVersion: claimed.Version,
		ClaimFence: fence, Outcome: AmbiguityOutcomeUnknown,
		Context: dispatchContext(fixture.now, "worker-request"),
	}); !errors.Is(err, ErrActionConflict) {
		t.Fatalf("a pinned stale version was not refused as a conflict: %v", err)
	}

	// And the second worker's claim is untouched by the report.
	current, err := fixture.service.Status(fixture.enqueuer, StatusRequest{
		ID: fixture.queued.ID, Context: dispatchContext(fixture.now, "request"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(current.ClaimID, []byte("second-claim")) {
		t.Fatalf("the report disturbed the live claim: %q", current.ClaimID)
	}
}

// TestAReportDoesNotStrandALiveClaimant is this route's central safety
// property, and the first version of it asserted the wrong thing.
//
// A report advances the record version while leaving the state, the claim, the
// fence and the lease alone. The completion path keyed on the version, so a
// lapsed holder filing a report moved it out from under a live claimant, whose
// completion then failed with ErrClaimLost while it held the claim at the right
// fence and claim ID.
//
// That worker had no recourse: Status requires OperationDispatch and the
// record's own principal, Pull withholds live-claimed records, and this route
// has no replay branch. So an effect it had actually performed became
// permanently unreportable — the precise harm #438 exists to prevent, caused by
// #438.
//
// The original test checked the fence, the claim ID and EffectPossible and
// never that a live claimant could still complete, which is the only assertion
// that would have caught it. Asserted here from the live claimant's side.
func TestAReportDoesNotStrandALiveClaimant(t *testing.T) {
	fixture := newExecutorClaimFixture(t)
	lapsed, fence, claimedByLapsed := fixture.lapsedClaimant(t, "lapsed")

	// A second worker takes over and holds a live claim.
	live := fixture.namedExecutor(t, "live")
	claimedByLive, err := fixture.service.Claim(live, ClaimRequest{
		ID: fixture.queued.ID, ExpectedVersion: claimedByLapsed.Version,
		ClaimID: []byte("live-claim"), Lease: time.Minute,
		Context: dispatchContext(fixture.now, "live-request"),
	})
	if err != nil {
		t.Fatalf("the live worker could not take the lapsed claim: %v", err)
	}

	// The lapsed holder reports, which is this route working as intended.
	if _, err := fixture.service.ReportAmbiguity(lapsed, AmbiguityRequest{
		ID: fixture.queued.ID, ClaimFence: fence,
		Outcome: AmbiguityOutcomeUnknown,
		Context: dispatchContext(fixture.now, "lapsed-request"),
	}); err != nil {
		t.Fatalf("the lapsed holder could not report: %v", err)
	}

	// The live claimant completes with the version it was handed at claim
	// time, which is the only version it has — Status is closed to it and Pull
	// withholds its own live-claimed record.
	done, err := fixture.service.CompleteClaim(live, CompletionRequest{
		ID: fixture.queued.ID, ExpectedVersion: claimedByLive.Version,
		ClaimID: []byte("live-claim"),
		// The fence it was handed at claim time, which is what binds a
		// completion to its claim generation. A worker always has it.
		ClaimFence: claimedByLive.ClaimFence,
		Result:     ExecutionResult{Output: json.RawMessage(`{"ok":true}`)},
		Context:    dispatchContext(fixture.now, "live-request"),
	})
	if err != nil {
		t.Fatalf("another principal's report stranded the live claimant, so an "+
			"effect it performed is permanently unreportable: %v", err)
	}
	if done.State != DispatchSucceeded ||
		string(done.Output) != `{"ok":true}` {
		t.Fatalf("the completion recorded the wrong outcome: state=%s output=%s",
			done.State, done.Output)
	}
	// And the report survived the completion, so tolerating the drift did not
	// cost the evidence.
	if len(done.AmbiguityReports) != 1 {
		t.Fatalf("the completion discarded the report: %+v", done.AmbiguityReports)
	}
}

// TestAReportDoesNotStrandASynchronousInvoke is the same hazard on the
// in-process path. ExecuteClaim re-reads the record twice and both gates keyed
// on the version, so a report landing between Claim and execution stranded a
// synchronous invoke exactly as it stranded a completion.
func TestAReportDoesNotStrandASynchronousInvoke(t *testing.T) {
	fixture := newExecutorClaimFixture(t)
	fixture.bindExecutor(t, &inProcessExecutor{})
	lapsed, fence, claimedByLapsed := fixture.lapsedClaimant(t, "lapsed")

	// The enqueuer claims it next, which is the identity the in-process path
	// requires: ExecuteClaim needs its caller to be both the record's
	// principal and its claimant.
	claimed, err := fixture.service.Claim(fixture.enqueuer, ClaimRequest{
		ID: fixture.queued.ID, ExpectedVersion: claimedByLapsed.Version,
		ClaimID: []byte("own-claim"), Lease: time.Minute,
		Context: dispatchContext(fixture.now, "request"),
	})
	if err != nil {
		t.Fatalf("the enqueuer could not claim: %v", err)
	}

	if _, err := fixture.service.ReportAmbiguity(lapsed, AmbiguityRequest{
		ID: fixture.queued.ID, ClaimFence: fence,
		Outcome: AmbiguityRequestNotSent,
		Context: dispatchContext(fixture.now, "lapsed-request"),
	}); err != nil {
		t.Fatalf("the lapsed holder could not report: %v", err)
	}

	// ExecuteClaim is handed the record as it was at claim time.
	result, err := fixture.service.ExecuteClaim(fixture.enqueuer, claimed)
	if err != nil {
		t.Fatalf("a report stranded the in-process execution path: %v", err)
	}
	if result.State != DispatchSucceeded {
		t.Fatalf("the execution did not complete: state=%s", result.State)
	}
}

// TestAFencelessCompletionStillComparesTheVersionExactly pins the fallback a
// caller gets when it supplies no fence, which is the only branch of
// completeClaim that still looks at the version.
//
// This test was named TestADriftLargerThanTheReportsIsStillRefused and its
// docstring described a "tolerance"/"allowance" budget for version drift that
// reports could account for. That mechanism was deleted rather than refined —
// onlyAmbiguityReportsAdvanced is gone, because it was papering over a missing
// fence check and widening the version comparison is what made the ABA break
// possible. So the name and the whole rationale described code that no longer
// exists, and the test was in fact exercising the plain exact-version
// comparison in the fence-less else branch.
//
// That branch is still worth pinning: a caller written against the older
// contract keeps the behaviour it was written against, strandable and all, and
// nothing should quietly loosen it. The ambiguity report below is what makes
// the pin stale by one more version than the claim alone would, which is the
// shape a pre-fence caller actually hits.
//
// The honest statement of the property: with no fence, any version other than
// exactly the one pinned is ErrClaimLost, whatever moved it.
func TestAFencelessCompletionStillComparesTheVersionExactly(t *testing.T) {
	fixture := newExecutorClaimFixture(t)
	lapsed, fence, claimedByLapsed := fixture.lapsedClaimant(t, "lapsed")
	live := fixture.namedExecutor(t, "live")
	claimedByLive, err := fixture.service.Claim(live, ClaimRequest{
		ID: fixture.queued.ID, ExpectedVersion: claimedByLapsed.Version,
		ClaimID: []byte("live-claim"), Lease: time.Minute,
		Context: dispatchContext(fixture.now, "live-request"),
	})
	if err != nil {
		t.Fatalf("the live worker could not claim: %v", err)
	}
	// One report, so exactly one version of drift is explainable.
	if _, err := fixture.service.ReportAmbiguity(lapsed, AmbiguityRequest{
		ID: fixture.queued.ID, ClaimFence: fence,
		Outcome: AmbiguityOutcomeUnknown,
		Context: dispatchContext(fixture.now, "lapsed-request"),
	}); err != nil {
		t.Fatalf("the lapsed holder could not report: %v", err)
	}

	// A pin one version behind the record. With no fence supplied, the
	// comparison is exact, so this is refused regardless of what moved the
	// version — here a report, which under the deleted tolerance would have
	// been "explainable" and accepted.
	stale := claimedByLive.Version - 1
	if stale == 0 {
		t.Fatalf("the fixture cannot express a non-zero stale pin: version %d",
			claimedByLive.Version)
	}
	if _, err := fixture.service.CompleteClaim(live, CompletionRequest{
		ID: fixture.queued.ID, ExpectedVersion: stale,
		ClaimID: []byte("live-claim"),
		Result:  ExecutionResult{Output: json.RawMessage(`{"ok":true}`)},
		Context: dispatchContext(fixture.now, "live-request"),
	}); !errors.Is(err, ErrClaimLost) {
		t.Fatalf("a fence-less completion pinning a stale version was "+
			"accepted, so the legacy branch has been loosened: %v", err)
	}

	// And the fence path is the supported one: the same caller, same stale
	// version, presenting the fence it holds, completes. Without this the test
	// above would also pass against a service that refused every completion.
	if _, err := fixture.service.CompleteClaim(live, CompletionRequest{
		ID: fixture.queued.ID, ExpectedVersion: stale,
		ClaimFence: claimedByLive.ClaimFence,
		ClaimID:    []byte("live-claim"),
		Result:     ExecutionResult{Output: json.RawMessage(`{"ok":true}`)},
		Context:    dispatchContext(fixture.now, "live-request"),
	}); err != nil {
		t.Fatalf("the fence path refused a live claimant holding the right "+
			"fence, which is the stranding this route was fixed to stop: %v",
			err)
	}
}

// TestAnAmbiguityReportWritesNoOutboxRow pins the PR's claim that this route
// publishes no event, which nothing asserted.
//
// It is not only about log volume. An outbox row for a report would be
// publishable by whoever filed it and by nobody else, which is the
// mixed-identity outbox #480 records as leavable undrainable by any principal.
// A lapsed claimant is exactly the principal least able to drain one.
func TestAnAmbiguityReportWritesNoOutboxRow(t *testing.T) {
	fixture := newExecutorClaimFixture(t)
	worker, fence, _ := fixture.lapsedClaimant(t, "worker")
	before := len(fixture.dispatchStore.transitions)

	if _, err := fixture.service.ReportAmbiguity(worker, AmbiguityRequest{
		ID: fixture.queued.ID, ClaimFence: fence,
		Outcome: AmbiguityOutcomeUnknown,
		Context: dispatchContext(fixture.now, "worker-request"),
	}); err != nil {
		t.Fatalf("the report failed: %v", err)
	}

	if after := len(fixture.dispatchStore.transitions); after != before {
		t.Fatalf("the report enqueued %d outbox rows; a row only its filer "+
			"can publish is the mixed-identity outbox #480 describes",
			after-before)
	}
}

// TestAnAmbiguityReportAuditsItsOwnPhaseAndOperation covers the two audit
// fields nothing pinned.
//
// The phase distinguishes this write in the audit trail from the five that
// transition the action. The operation must be the one that authorized *this*
// call rather than the record's last transition — reading the record's field
// would attribute the report to whatever claimed the action, and would pass an
// empty operation for a record claimed by a build without the field, which
// RecordAction validates first and refuses as a 503.
func TestAnAmbiguityReportAuditsItsOwnPhaseAndOperation(t *testing.T) {
	fixture := newExecutorClaimFixture(t)
	worker, fence, _ := fixture.lapsedClaimant(t, "worker")

	// The record was claimed by this worker under execute, so a correct audit
	// names execute. Clearing the record's own field proves the audit does not
	// read it — a record from a build without the field must still report.
	stored := fixture.dispatchStore.records[string(fixture.queued.ID)]
	stored.TransitionOperation = ""
	fixture.dispatchStore.records[string(fixture.queued.ID)] = stored

	if _, err := fixture.service.ReportAmbiguity(worker, AmbiguityRequest{
		ID: fixture.queued.ID, ClaimFence: fence,
		Outcome: AmbiguityOutcomeUnknown,
		Context: dispatchContext(fixture.now, "worker-request"),
	}); err != nil {
		t.Fatalf("a record with no recorded transition operation cannot be "+
			"reported on, which is a 503 across an upgrade: %v", err)
	}

	var found bool
	for _, phase := range fixture.recorder.phases {
		if phase == "ambiguity_report" {
			found = true
		}
	}
	if !found {
		t.Fatalf("no ambiguity_report audit phase was recorded: %v",
			fixture.recorder.phases)
	}
	// And the operation, which this test claimed to cover and did not. The
	// recorder double kept only the phase, so changing the audited operation
	// to one that had not authorized the call left the whole package green —
	// the assertion above is satisfied by any operation that passes
	// Operation.Validate, which every operation does.
	if got := fixture.recorder.recordedOperation(
		"ambiguity_report"); got != auth.OperationExecute {
		t.Fatalf("the report was audited under %q, not the operation that "+
			"authorized it: an auditor then attributes it to whatever "+
			"claimed the action", got)
	}
}

// TestARenewalAuditsItsOwnOperationAcrossAnUpgrade is the defence ExtendClaim
// was missing and both its siblings have.
//
// ReportAmbiguity audits the operation that authorized the call. ExecuteClaim
// keeps the record's and falls back to invoke when it is empty. ExtendClaim
// read the record's field with no fallback, so a record claimed by a build
// before TransitionOperation existed audited an empty operation — which
// RecordAction validates first and refuses, joined with
// ErrRecordingUnavailable and answered as a 503.
//
// The consequence is the one the route exists to prevent: a worker mid-long-
// operation across an upgrade cannot renew, loses its claim, and ends up in
// the ambiguity case holding an effect it can no longer report through the
// completion path.
func TestARenewalAuditsItsOwnOperationAcrossAnUpgrade(t *testing.T) {
	fixture := newExecutorClaimFixture(t)
	worker := fixture.namedExecutor(t, "worker")
	claimed, err := fixture.service.Claim(worker, ClaimRequest{
		ID: fixture.queued.ID, ExpectedVersion: fixture.queued.Version,
		ClaimID: []byte("worker-claim"), Lease: time.Minute,
		Context: dispatchContext(fixture.now, "worker-request"),
	})
	if err != nil {
		t.Fatal(err)
	}

	stored := fixture.dispatchStore.records[string(fixture.queued.ID)]
	stored.TransitionOperation = ""
	fixture.dispatchStore.records[string(fixture.queued.ID)] = stored

	if _, err := fixture.service.ExtendClaim(worker, ExtendRequest{
		ID: fixture.queued.ID, ExpectedVersion: claimed.Version,
		ClaimID: []byte("worker-claim"), Lease: 2 * time.Minute,
		Context: dispatchContext(fixture.now, "worker-request"),
	}); err != nil {
		t.Fatalf("a record with no recorded transition operation cannot be "+
			"renewed, so every live claim across an upgrade is lost: %v", err)
	}
	if got := fixture.recorder.recordedOperation(
		"claim_extension"); got != auth.OperationExecute {
		t.Fatalf("the renewal was audited under %q rather than the operation "+
			"that authorized it", got)
	}
}

// TestAStaleGenerationCannotCommitOntoALiveOne is the ABA break a second
// review found in the first attempt at the stranding fix, and the reason this
// route binds on the fence rather than on a widened version comparison.
//
// The first attempt tolerated version drift that ambiguity reports could
// account for. The budget was the record's *lifetime* report count while the
// drift was measured from the caller's read, so reports filed before the read
// bought slack without adding drift. A worker reusing one ClaimID across two
// claim generations could then commit attempt one's outcome onto attempt two's
// generation — terminating a claim that was still executing.
//
// ClaimID cannot prevent it: it is caller-chosen, nothing requires it to be
// unique across generations, and a worker deriving it from the action ID is
// doing something the model's own wording invites. The fence can, because
// applyClaim increments it on every claim.
func TestAStaleGenerationCannotCommitOntoALiveOne(t *testing.T) {
	fixture := newExecutorClaimFixture(t)

	// A lapsed holder files a report first, which under the old tolerance
	// bought a version of slack without moving the caller's read.
	other, otherFence, otherClaim := fixture.lapsedClaimant(t, "other")
	if _, err := fixture.service.ReportAmbiguity(other, AmbiguityRequest{
		ID: fixture.queued.ID, ClaimFence: otherFence,
		Outcome: AmbiguityOutcomeUnknown,
		Context: dispatchContext(fixture.now, "other-request"),
	}); err != nil {
		t.Fatalf("the lapsed holder could not report: %v", err)
	}
	reported, err := fixture.service.Status(fixture.enqueuer, StatusRequest{
		ID: fixture.queued.ID, Context: dispatchContext(fixture.now, "request"),
	})
	if err != nil {
		t.Fatal(err)
	}
	_ = otherClaim

	// One worker, one reused ClaimID, two generations.
	worker := fixture.namedExecutor(t, "worker")
	first, err := fixture.service.Claim(worker, ClaimRequest{
		ID: fixture.queued.ID, ExpectedVersion: reported.Version,
		ClaimID: []byte("reused"), Lease: time.Nanosecond,
		Context: dispatchContext(fixture.now, "worker-request"),
	})
	if err != nil {
		t.Fatalf("the first attempt could not claim: %v", err)
	}
	fixture.advance(t, time.Second)
	second, err := fixture.service.Claim(worker, ClaimRequest{
		ID: fixture.queued.ID, ExpectedVersion: first.Version,
		ClaimID: []byte("reused"), Lease: time.Minute,
		Context: dispatchContext(fixture.now, "worker-request"),
	})
	if err != nil {
		t.Fatalf("the second attempt could not claim: %v", err)
	}
	if second.ClaimFence == first.ClaimFence {
		t.Fatalf("both attempts share fence %d, so this test cannot "+
			"distinguish them", first.ClaimFence)
	}

	// The first attempt completes late, under its own generation's fence.
	// The second attempt is live and still executing.
	_, err = fixture.service.CompleteClaim(worker, CompletionRequest{
		ID: fixture.queued.ID, ExpectedVersion: first.Version,
		ClaimID: []byte("reused"), ClaimFence: first.ClaimFence,
		Result:  ExecutionResult{Output: json.RawMessage(`{"ok":false}`)},
		Context: dispatchContext(fixture.now, "worker-request"),
	})
	if err == nil {
		t.Fatal("a stale claim generation committed its outcome onto a live " +
			"one, terminating a claim that was still executing")
	}
	if !errors.Is(err, ErrClaimLost) {
		t.Fatalf("the refusal does not tell the stale attempt its claim is "+
			"gone: %v", err)
	}

	// And the live generation still completes with its own outcome.
	done, err := fixture.service.CompleteClaim(worker, CompletionRequest{
		ID: fixture.queued.ID, ExpectedVersion: second.Version,
		ClaimID: []byte("reused"), ClaimFence: second.ClaimFence,
		Result:  ExecutionResult{Output: json.RawMessage(`{"ok":true}`)},
		Context: dispatchContext(fixture.now, "worker-request"),
	})
	if err != nil {
		t.Fatalf("the live generation was refused: %v", err)
	}
	if string(done.Output) != `{"ok":true}` {
		t.Fatalf("the stale attempt's outcome was recorded: %s", done.Output)
	}
}

// TestAReplayIsNotAnotherGenerationsOutcome is the second defect of that
// attempt: dropping the version from the replay branch without putting the
// fence in its place.
//
// Terminal plus a matching ClaimID is true of a *later* generation's
// completion, so a worker reusing one ClaimID was handed attempt two's
// committed outcome as though it were attempt one's — a success receipt for
// someone else's result, which is the failure a worker cannot detect. It
// needed no ambiguity report at all, and nothing in the repository covered it.
func TestAReplayIsNotAnotherGenerationsOutcome(t *testing.T) {
	fixture := newExecutorClaimFixture(t)
	worker := fixture.namedExecutor(t, "worker")

	first, err := fixture.service.Claim(worker, ClaimRequest{
		ID: fixture.queued.ID, ExpectedVersion: fixture.queued.Version,
		ClaimID: []byte("reused"), Lease: time.Nanosecond,
		Context: dispatchContext(fixture.now, "worker-request"),
	})
	if err != nil {
		t.Fatalf("the first attempt could not claim: %v", err)
	}
	fixture.advance(t, time.Second)

	// The second attempt claims under the same ClaimID and completes.
	second, err := fixture.service.Claim(worker, ClaimRequest{
		ID: fixture.queued.ID, ExpectedVersion: first.Version,
		ClaimID: []byte("reused"), Lease: time.Minute,
		Context: dispatchContext(fixture.now, "worker-request"),
	})
	if err != nil {
		t.Fatalf("the second attempt could not claim: %v", err)
	}
	if _, err := fixture.service.CompleteClaim(worker, CompletionRequest{
		ID: fixture.queued.ID, ExpectedVersion: second.Version,
		ClaimID: []byte("reused"), ClaimFence: second.ClaimFence,
		Result:  ExecutionResult{Output: json.RawMessage(`{"ok":false}`)},
		Context: dispatchContext(fixture.now, "worker-request"),
	}); err != nil {
		t.Fatalf("the second attempt could not complete: %v", err)
	}

	// The first attempt retries, as it would after a lost response.
	replayed, err := fixture.service.CompleteClaim(worker, CompletionRequest{
		ID: fixture.queued.ID, ExpectedVersion: first.Version,
		ClaimID: []byte("reused"), ClaimFence: first.ClaimFence,
		Result:  ExecutionResult{Output: json.RawMessage(`{"ok":true}`)},
		Context: dispatchContext(fixture.now, "worker-request"),
	})
	if err == nil {
		t.Fatalf("the first attempt was handed another generation's outcome "+
			"as its own: %s", replayed.Output)
	}
	if !errors.Is(err, ErrClaimLost) {
		t.Fatalf("the refusal is not a lost claim: %v", err)
	}
}

// TestAmbiguityTextRefusesNonPrintableCharacters covers the fix that
// unicode.IsControl was the wrong test, which nothing asserted.
//
// IsControl is false for U+202E RIGHT-TO-LEFT OVERRIDE, the U+2066..U+2069
// isolates, U+200E/U+200F, the zero-width characters and U+FEFF — all of which
// reached the durable record while the comment claimed "printable and
// single-line". U+202E is exactly the case the bound's own rationale names: a
// value that is at worst a terminal escape in whatever renders it.
//
// The existing refusal table probed only ASCII controls, so the entire content
// of the fix was uncovered.
func TestAmbiguityTextRefusesNonPrintableCharacters(t *testing.T) {
	for _, probe := range []struct {
		name  string
		value string
	}{
		// Built from rune values rather than written literally: a source file
		// may not contain a byte order mark except at its start, and the
		// literals also make the test unreadable in exactly the way these
		// characters make a target unreadable.
		{"right-to-left override", "host" + string(rune(0x202E)) + "evil"},
		{"left-to-right isolate", "host" + string(rune(0x2066)) + "evil"},
		{"pop directional isolate", "host" + string(rune(0x2069)) + "evil"},
		{"left-to-right mark", "host" + string(rune(0x200E)) + "evil"},
		{"zero-width space", "host" + string(rune(0x200B)) + "evil"},
		{"soft hyphen", "host" + string(rune(0x00AD)) + "evil"},
		{"byte order mark", "host" + string(rune(0xFEFF)) + "evil"},
	} {
		t.Run(probe.name, func(t *testing.T) {
			fixture := newExecutorClaimFixture(t)
			worker, fence, _ := fixture.lapsedClaimant(t, "worker")
			if _, err := fixture.service.ReportAmbiguity(
				worker, AmbiguityRequest{
					ID: fixture.queued.ID, ClaimFence: fence,
					Outcome: AmbiguityOutcomeUnknown, Target: probe.value,
					Context: dispatchContext(fixture.now, "worker-request"),
				}); err == nil {
				t.Fatalf("%q was accepted into the durable record", probe.value)
			}
		})
	}

	// A non-ASCII value that *is* printable must still be accepted, or the
	// stricter test would be refusing ordinary targets.
	fixture := newExecutorClaimFixture(t)
	worker, fence, _ := fixture.lapsedClaimant(t, "worker")
	if _, err := fixture.service.ReportAmbiguity(worker, AmbiguityRequest{
		ID: fixture.queued.ID, ClaimFence: fence,
		Outcome:   AmbiguityOutcomeUnknown,
		Target:    "zahlungen.beispiel.test",
		Reference: "r" + string(rune(0x00E9)) + "f-001",
		Context:   dispatchContext(fixture.now, "worker-request"),
	}); err != nil {
		t.Fatalf("a printable non-ASCII target was refused: %v", err)
	}
}

// TestTheRetainedHolderCarriesItsOwnClaimID covers the ordering fix in
// applyClaim, which nothing asserted.
//
// The history retains the *outgoing* holder, and the block read
// record.ClaimID after the incoming claim had already overwritten it — so
// every retained holder carried its successor's claim ID. No authorization
// consequence, since heldClaimAt keys on the fence, but the history is
// evidence an operator reads and it was wrong.
func TestTheRetainedHolderCarriesItsOwnClaimID(t *testing.T) {
	fixture := newExecutorClaimFixture(t)
	first, _, claimedFirst := fixture.lapsedClaimant(t, "first")
	_ = first

	second := fixture.namedExecutor(t, "second")
	if _, err := fixture.service.Claim(second, ClaimRequest{
		ID: fixture.queued.ID, ExpectedVersion: claimedFirst.Version,
		ClaimID: []byte("second-claim"), Lease: time.Minute,
		Context: dispatchContext(fixture.now, "second-request"),
	}); err != nil {
		t.Fatalf("the second worker could not claim: %v", err)
	}

	current, err := fixture.service.Status(fixture.enqueuer, StatusRequest{
		ID: fixture.queued.ID, Context: dispatchContext(fixture.now, "request"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(current.ClaimHistory) != 1 {
		t.Fatalf("history length = %d, want 1", len(current.ClaimHistory))
	}
	holder := current.ClaimHistory[0]
	if !bytes.Equal(holder.ClaimID, []byte("first-claim")) {
		t.Fatalf("the retained holder carries %q, want its own claim ID "+
			"\"first-claim\" — reading record.ClaimID after the incoming claim "+
			"overwrote it gives every holder its successor's", holder.ClaimID)
	}
	if holder.Subject != "first-subject" {
		t.Fatalf("the retained holder is %q, not the outgoing one",
			holder.Subject)
	}
}

// TestAClaimHolderChainIsBoundedInBytes covers the aggregate bound, which
// nothing asserted.
//
// MaxOnBehalfOfEntries bounds the chain by entry count and not by size. A
// measurement found the chains were 89% of the record's worst-case growth:
// eight holders with maximal chains reach 18% of the encoding ceiling alone,
// and combined with a maximal input, output and evidence set they pushed a
// record past what encodeAction accepts — the brick the cap exists to prevent.
func TestAClaimHolderChainIsBoundedInBytes(t *testing.T) {
	long := make([]shoal.ID, 0, 8)
	for len(long) < 8 {
		long = append(long, shoal.ID(strings.Repeat("d", 1024)))
	}
	holder := ClaimHolder{
		Subject: "holder", Actor: "holder-actor",
		ClaimID: []byte("claim"), ClaimFence: 1,
		HeldAt:     time.Date(2026, 9, 6, 12, 0, 0, 0, time.UTC),
		OnBehalfOf: long,
	}
	if err := holder.validate(); err == nil {
		t.Fatalf("a delegation chain of %d bytes across %d entries was "+
			"accepted, so the record is still brickable by a repeatedly "+
			"re-claimed action", 8*1024, len(long))
	}

	// And a chain inside the bound is still accepted, so the byte cap is not
	// simply refusing delegation.
	holder.OnBehalfOf = []shoal.ID{shoal.ID(strings.Repeat("d", 512))}
	if err := holder.validate(); err != nil {
		t.Fatalf("a chain well inside the byte bound was refused: %v", err)
	}
}

// TestARenewalExtendsTheLeaseWithoutMovingTheFence is #430's central
// invariant, and the one the gateway design depends on.
//
// ClaimFence identifies *which* claim. A renewal changes only its deadline, so
// incrementing the fence would invalidate the fence the claimant is holding and
// break the report-under-the-same-fence contract — the worker would renew its
// lease and lose the ability to complete under it.
func TestARenewalExtendsTheLeaseWithoutMovingTheFence(t *testing.T) {
	fixture := newExecutorClaimFixture(t)
	worker := fixture.namedExecutor(t, "worker")

	claimed, err := fixture.service.Claim(worker, ClaimRequest{
		ID: fixture.queued.ID, ExpectedVersion: fixture.queued.Version,
		ClaimID: []byte("worker-claim"), Lease: time.Minute,
		Context: dispatchContext(fixture.now, "worker-request"),
	})
	if err != nil {
		t.Fatalf("the worker could not claim: %v", err)
	}

	// Part way through the lease, as a worker renewing at L/2 would be.
	fixture.advance(t, 30*time.Second)
	extended, err := fixture.service.ExtendClaim(worker, ExtendRequest{
		ID: fixture.queued.ID, ExpectedVersion: claimed.Version,
		ClaimID: []byte("worker-claim"), Lease: time.Minute,
		Context: dispatchContext(fixture.now, "worker-request"),
	})
	if err != nil {
		t.Fatalf("the claim holder could not renew: %v", err)
	}

	if extended.ClaimFence != claimed.ClaimFence {
		t.Fatalf("the renewal moved the fence from %d to %d, which invalidates "+
			"the fence the claimant reports under",
			claimed.ClaimFence, extended.ClaimFence)
	}
	if !bytes.Equal(extended.ClaimID, claimed.ClaimID) {
		t.Fatalf("the renewal changed the claim ID from %q to %q",
			claimed.ClaimID, extended.ClaimID)
	}
	if !extended.ClaimLeaseUntil.After(claimed.ClaimLeaseUntil) {
		t.Fatalf("the lease did not move forward: %s then %s",
			claimed.ClaimLeaseUntil, extended.ClaimLeaseUntil)
	}
	if extended.Version != claimed.Version+1 {
		t.Fatalf("version = %d, want %d", extended.Version, claimed.Version+1)
	}
	if extended.State != DispatchClaimed {
		t.Fatalf("the renewal moved the state to %s", extended.State)
	}

	// And the claimant can still complete under the same fence and claim,
	// which is the whole point of not advancing either.
	if _, err := fixture.service.CompleteClaim(worker, CompletionRequest{
		ID: fixture.queued.ID, ExpectedVersion: extended.Version,
		ClaimID: []byte("worker-claim"),
		Result:  ExecutionResult{Output: json.RawMessage(`{"ok":true}`)},
		Context: dispatchContext(fixture.now, "worker-request"),
	}); err != nil {
		t.Fatalf("the claimant cannot complete after renewing: %v", err)
	}
}

// TestARenewalCoversAnOperationLongerThanTheLeaseCeiling is the capability
// #430 adds, stated as the thing that was impossible before it.
//
// MaxActionClaimTTL is five minutes and the service refuses a larger lease
// rather than clamping it, so the fenced window used to be capped at five
// minutes total. Renewal makes that a heartbeat interval instead: the total
// budget is the action's Deadline, which already existed and already clamps.
func TestARenewalCoversAnOperationLongerThanTheLeaseCeiling(t *testing.T) {
	fixture := newExecutorClaimFixture(t)
	worker := fixture.namedExecutor(t, "worker")

	// A lease at the ceiling. A larger one is refused, which is what makes
	// renewal the only way to outlast it.
	if _, err := fixture.service.Claim(worker, ClaimRequest{
		ID: fixture.queued.ID, ExpectedVersion: fixture.queued.Version,
		ClaimID: []byte("too-long"), Lease: MaxActionClaimTTL + time.Second,
		Context: dispatchContext(fixture.now, "worker-request"),
	}); err == nil {
		t.Fatal("a lease past MaxActionClaimTTL was accepted, so this test " +
			"is not describing the constraint renewal exists for")
	}

	claimed, err := fixture.service.Claim(worker, ClaimRequest{
		ID: fixture.queued.ID, ExpectedVersion: fixture.queued.Version,
		ClaimID: []byte("worker-claim"), Lease: MaxActionClaimTTL,
		Context: dispatchContext(fixture.now, "worker-request"),
	})
	if err != nil {
		t.Fatalf("the worker could not claim at the ceiling: %v", err)
	}

	// Renew at half the lease, repeatedly, past the point where the original
	// claim would have expired.
	version := claimed.Version
	fence := claimed.ClaimFence
	for round := 0; round < 4; round++ {
		fixture.advance(t, MaxActionClaimTTL/2)
		extended, err := fixture.service.ExtendClaim(worker, ExtendRequest{
			ID: fixture.queued.ID, ExpectedVersion: version,
			ClaimID: []byte("worker-claim"), Lease: MaxActionClaimTTL,
			Context: dispatchContext(fixture.now, "worker-request"),
		})
		if err != nil {
			t.Fatalf("renewal %d failed: %v", round, err)
		}
		if extended.ClaimFence != fence {
			t.Fatalf("renewal %d moved the fence to %d",
				round, extended.ClaimFence)
		}
		version = extended.Version
	}

	// Past the original lease's expiry, still holding the claim.
	if !fixture.now.After(claimed.ClaimLeaseUntil) {
		t.Fatalf("the clock (%s) has not passed the original lease (%s), so "+
			"this test does not outlast it", fixture.now, claimed.ClaimLeaseUntil)
	}
	if _, err := fixture.service.CompleteClaim(worker, CompletionRequest{
		ID: fixture.queued.ID, ExpectedVersion: version,
		ClaimID: []byte("worker-claim"),
		Result:  ExecutionResult{Output: json.RawMessage(`{"ok":true}`)},
		Context: dispatchContext(fixture.now, "worker-request"),
	}); err != nil {
		t.Fatalf("after outlasting its original lease by renewal, the worker "+
			"cannot complete: %v", err)
	}
}

// TestALeaseIsNotRenewableOnceItHasLapsed is the semantics the issue calls the
// point rather than a limitation.
//
// Once ClaimLeaseUntil has passed, the action may already have been reclaimed
// under a new ClaimID and fence, so renewing would hand two workers a live
// claim. The refusal is ErrClaimLost and not a transport-shaped error, because
// a worker must be able to tell "my claim is gone, treat this as ambiguous"
// from "retry the renewal" — the first means it is in the #438 case.
func TestALeaseIsNotRenewableOnceItHasLapsed(t *testing.T) {
	fixture := newExecutorClaimFixture(t)
	worker := fixture.namedExecutor(t, "worker")

	claimed, err := fixture.service.Claim(worker, ClaimRequest{
		ID: fixture.queued.ID, ExpectedVersion: fixture.queued.Version,
		ClaimID: []byte("worker-claim"), Lease: time.Nanosecond,
		Context: dispatchContext(fixture.now, "worker-request"),
	})
	if err != nil {
		t.Fatalf("the worker could not claim: %v", err)
	}
	fixture.advance(t, time.Second)

	_, err = fixture.service.ExtendClaim(worker, ExtendRequest{
		ID: fixture.queued.ID, ExpectedVersion: claimed.Version,
		ClaimID: []byte("worker-claim"), Lease: time.Minute,
		Context: dispatchContext(fixture.now, "worker-request"),
	})
	if err == nil {
		t.Fatal("a lapsed lease was renewed, so two workers can hold one claim")
	}
	if !errors.Is(err, ErrClaimLost) {
		t.Fatalf("the refusal does not tell the worker its claim is gone, so "+
			"it cannot distinguish that from a renewal worth retrying: %v", err)
	}

	// And the worker's recourse is the #438 route, under the fence it held.
	if _, err := fixture.service.ReportAmbiguity(worker, AmbiguityRequest{
		ID: fixture.queued.ID, ExpectedVersion: claimed.Version,
		ClaimFence: claimed.ClaimFence, Outcome: AmbiguityOutcomeUnknown,
		Context: dispatchContext(fixture.now, "worker-request"),
	}); err != nil {
		t.Fatalf("a worker whose renewal was refused cannot report the "+
			"ambiguity that refusal creates: %v", err)
	}
}

// TestOnlyTheClaimHolderMayRenew pins that a claim ID is necessary and not
// sufficient.
//
// Status publishes ClaimID to every co-principal and Pull returns it on a
// lapsed claim to every execute-holder in the scope, so it identifies a claim
// rather than who holds it — the same reasoning that made the completion path
// require the claimant's principal chain.
func TestOnlyTheClaimHolderMayRenew(t *testing.T) {
	fixture := newExecutorClaimFixture(t)
	holder := fixture.namedExecutor(t, "holder")
	stranger := fixture.namedExecutor(t, "stranger")

	claimed, err := fixture.service.Claim(holder, ClaimRequest{
		ID: fixture.queued.ID, ExpectedVersion: fixture.queued.Version,
		ClaimID: []byte("holder-claim"), Lease: time.Minute,
		Context: dispatchContext(fixture.now, "holder-request"),
	})
	if err != nil {
		t.Fatalf("the holder could not claim: %v", err)
	}

	for _, probe := range []struct {
		name     string
		caller   context.Context
		claimID  []byte
		notFound bool
	}{
		{
			name:   "a stranger presenting the right claim ID",
			caller: stranger, claimID: []byte("holder-claim"), notFound: true,
		},
		{
			name:   "the enqueuer presenting the right claim ID",
			caller: fixture.enqueuer, claimID: []byte("holder-claim"),
			notFound: true,
		},
		{
			name:   "the holder presenting the wrong claim ID",
			caller: holder, claimID: []byte("not-the-claim"), notFound: true,
		},
	} {
		t.Run(probe.name, func(t *testing.T) {
			request := dispatchContext(fixture.now, "holder-request")
			if probe.caller == stranger {
				request = dispatchContext(fixture.now, "stranger-request")
			} else if probe.caller == fixture.enqueuer {
				request = dispatchContext(fixture.now, "request")
			}
			_, err := fixture.service.ExtendClaim(probe.caller, ExtendRequest{
				ID: fixture.queued.ID, ExpectedVersion: claimed.Version,
				ClaimID: probe.claimID, Lease: time.Minute,
				Context: request,
			})
			if err == nil {
				t.Fatal("the renewal was accepted")
			}
			if probe.notFound && !shoal.IsErrorCode(err, shoal.ErrorNotFound) {
				t.Fatalf("refusal is distinguishable from an absent action, "+
					"which tells an execute-holder the action exists: %v", err)
			}
		})
	}

	// The holder with the right claim ID still renews, so the refusals above
	// are about standing rather than the route being broken.
	if _, err := fixture.service.ExtendClaim(holder, ExtendRequest{
		ID: fixture.queued.ID, ExpectedVersion: claimed.Version,
		ClaimID: []byte("holder-claim"), Lease: 2 * time.Minute,
		Context: dispatchContext(fixture.now, "holder-request"),
	}); err != nil {
		t.Fatalf("the holder was refused: %v", err)
	}
}

// TestARenewalIsRefusedWhenItWouldNotMoveTheLeaseForward covers the shortening
// decision, which the issue asks to be made explicitly either way.
//
// A lease moving ClaimLeaseUntil backwards is a worker bug, and honouring it
// costs the claim the worker was trying to keep. Refused, so the worker finds
// out while it still holds it.
//
// The same check covers the case where the Deadline clamp makes an extension a
// no-op: once ClaimLeaseUntil has reached Deadline there is nothing left to
// extend, and reporting success would tell a worker it had bought time it did
// not get.
func TestARenewalIsRefusedWhenItWouldNotMoveTheLeaseForward(t *testing.T) {
	t.Run("a shorter lease", func(t *testing.T) {
		fixture := newExecutorClaimFixture(t)
		worker := fixture.namedExecutor(t, "worker")
		claimed, err := fixture.service.Claim(worker, ClaimRequest{
			ID: fixture.queued.ID, ExpectedVersion: fixture.queued.Version,
			ClaimID: []byte("worker-claim"), Lease: 2 * time.Minute,
			Context: dispatchContext(fixture.now, "worker-request"),
		})
		if err != nil {
			t.Fatalf("the worker could not claim: %v", err)
		}
		_, err = fixture.service.ExtendClaim(worker, ExtendRequest{
			ID: fixture.queued.ID, ExpectedVersion: claimed.Version,
			ClaimID: []byte("worker-claim"), Lease: time.Second,
			Context: dispatchContext(fixture.now, "worker-request"),
		})
		if err == nil {
			t.Fatal("a renewal that shortens the lease was accepted")
		}
		if !shoal.IsErrorCode(err, shoal.ErrorInvalidArgument) {
			t.Fatalf("the refusal is not an argument error: %v", err)
		}
	})

	t.Run("a lease already clamped to the deadline", func(t *testing.T) {
		fixture := newExecutorClaimFixture(t)
		worker := fixture.namedExecutor(t, "worker")
		// The action's deadline is an hour out and the ceiling is five
		// minutes, so claim at the ceiling and advance until the clamp binds.
		claimed, err := fixture.service.Claim(worker, ClaimRequest{
			ID: fixture.queued.ID, ExpectedVersion: fixture.queued.Version,
			ClaimID: []byte("worker-claim"), Lease: MaxActionClaimTTL,
			Context: dispatchContext(fixture.now, "worker-request"),
		})
		if err != nil {
			t.Fatalf("the worker could not claim: %v", err)
		}
		version := claimed.Version
		// Renew until ClaimLeaseUntil reaches Deadline. The action's deadline
		// is an hour out and each round advances MaxActionClaimTTL/2, so the
		// clamp binds at round 23 — computed rather than guessed, because an
		// earlier version of this loop stopped at 20 and concluded the clamp
		// was never reached.
		for round := 0; round < 30; round++ {
			fixture.advance(t, MaxActionClaimTTL/2)
			extended, err := fixture.service.ExtendClaim(worker, ExtendRequest{
				ID: fixture.queued.ID, ExpectedVersion: version,
				ClaimID: []byte("worker-claim"), Lease: MaxActionClaimTTL,
				Context: dispatchContext(fixture.now, "worker-request"),
			})
			if err != nil {
				// Refused once the clamp leaves nothing to extend, which is
				// the behaviour under test.
				if shoal.IsErrorCode(err, shoal.ErrorInvalidArgument) {
					return
				}
				t.Fatalf("renewal %d failed for an unexpected reason: %v",
					round, err)
			}
			if extended.ClaimLeaseUntil.After(extended.Deadline) {
				t.Fatalf("the renewal pushed the lease past the deadline: "+
					"%s > %s", extended.ClaimLeaseUntil, extended.Deadline)
			}
			version = extended.Version
		}
		t.Fatal("the lease never reached the deadline clamp, so this case " +
			"does not exercise it")
	})
}

// TestARenewalPublishesNoLifecycleEvent pins the decision not to emit one.
//
// A heartbeat on a long operation would publish an event every few minutes per
// action and drown the event log in liveness. ClaimLeaseUntil and Version
// already carry the renewal and Status already exposes them. It also keeps a
// renewal out of the outbox, which matters beyond volume: an outbox row that
// only one principal can publish is the mixed-identity wedge #480 records.
func TestARenewalPublishesNoLifecycleEvent(t *testing.T) {
	fixture := newExecutorClaimFixture(t)
	worker := fixture.namedExecutor(t, "worker")

	claimed, err := fixture.service.Claim(worker, ClaimRequest{
		ID: fixture.queued.ID, ExpectedVersion: fixture.queued.Version,
		ClaimID: []byte("worker-claim"), Lease: time.Minute,
		Context: dispatchContext(fixture.now, "worker-request"),
	})
	if err != nil {
		t.Fatalf("the worker could not claim: %v", err)
	}
	before := len(fixture.dispatchStore.transitions)

	fixture.advance(t, 30*time.Second)
	if _, err := fixture.service.ExtendClaim(worker, ExtendRequest{
		ID: fixture.queued.ID, ExpectedVersion: claimed.Version,
		ClaimID: []byte("worker-claim"), Lease: time.Minute,
		Context: dispatchContext(fixture.now, "worker-request"),
	}); err != nil {
		t.Fatalf("the renewal failed: %v", err)
	}

	if after := len(fixture.dispatchStore.transitions); after != before {
		t.Fatalf("the renewal enqueued %d outbox rows, so a long operation "+
			"publishes a liveness event every heartbeat and a row only its "+
			"claimant can drain", after-before)
	}
}

// TestAnAdmissionClaimCannotBeRenewed keeps the admission namespace out of the
// renewal route.
//
// An admission's grant is not a claim a worker renews — it is reported through
// AdmissionService.Report, and an expired one is abandoned rather than
// reclaimable. Refused as not-found for the same reason every other dispatch
// route refuses an admission: the IDs are computable from a principal tuple, so
// a distinguishable answer is a probe.
//
// This exists because a mutation removing the check survived: no other test
// puts an admission in front of this route.
func TestAnAdmissionClaimCannotBeRenewed(t *testing.T) {
	fixture := newExecutorClaimFixture(t)
	worker := fixture.namedExecutor(t, "worker")

	claimed, err := fixture.service.Claim(worker, ClaimRequest{
		ID: fixture.queued.ID, ExpectedVersion: fixture.queued.Version,
		ClaimID: []byte("worker-claim"), Lease: time.Minute,
		Context: dispatchContext(fixture.now, "worker-request"),
	})
	if err != nil {
		t.Fatalf("the worker could not claim: %v", err)
	}
	// Renewal works first, so the refusal below is about the marker and not
	// about the fixture being in a state no renewal would accept.
	fixture.advance(t, 30*time.Second)
	extended, err := fixture.service.ExtendClaim(worker, ExtendRequest{
		ID: fixture.queued.ID, ExpectedVersion: claimed.Version,
		ClaimID: []byte("worker-claim"), Lease: time.Minute,
		Context: dispatchContext(fixture.now, "worker-request"),
	})
	if err != nil {
		t.Fatalf("the renewal failed before the marker was set: %v", err)
	}

	// Marked by the durable field isAdmission reads, rather than by identity:
	// the reserved span held ordinary actions before it was reserved, which is
	// a distinction the dispatch service already makes deliberately.
	stored := fixture.dispatchStore.records[string(fixture.queued.ID)]
	stored.AdmittedEffects = Effects{EffectReadsCorpus}
	fixture.dispatchStore.records[string(fixture.queued.ID)] = stored

	_, err = fixture.service.ExtendClaim(worker, ExtendRequest{
		ID: fixture.queued.ID, ExpectedVersion: extended.Version,
		ClaimID: []byte("worker-claim"), Lease: time.Minute,
		Context: dispatchContext(fixture.now, "worker-request"),
	})
	if err == nil {
		t.Fatal("an admission grant was renewed as if it were a dispatch claim")
	}
	if !shoal.IsErrorCode(err, shoal.ErrorNotFound) {
		t.Fatalf("an admission is refused distinguishably: %v", err)
	}
}

// TestAClaimantChainTooLongToRetainIsRefusedAtClaimTime closes the brick that
// MaxClaimHolderChainBytes itself opened.
//
// ClaimantOnBehalfOf on the live record was bounded only by entry count — 64
// entries of up to shoal.MaxIDBytes each, so 64 KB in the worst case — while a
// retained ClaimHolder's chain was additionally bounded at
// MaxClaimHolderChainBytes. A claimant whose chain fell between the two claimed
// successfully, and then the *next* claim built a record that
// ActionRecord.Validate refuses, so encodeAction refused the write and every
// later claim by anyone was refused identically. The action sat in
// DispatchClaimed with a dead lease and no principal could ever take it again.
//
// It was reproduced before it was fixed: a five-entry 5120-byte chain claimed
// at version 2, its lease lapsed, and the next worker was refused "claim holder
// delegation chain exceeds its byte bound" with the record still at version 2.
//
// Refused at claim time rather than truncated on retention, because
// heldClaimAt compares a retained holder's chain element for element — so
// dropping it would silently deny the ambiguity route to exactly the delegated
// worker whose claim was taken over, the one caller that route exists for.
//
// The second half is what makes the first half mean anything: a chain just
// inside the bound must still claim, lapse and be re-claimed. Without it this
// test would pass against a service that refused every delegated claim.
//
// Since #391 a delegated claimant can only be the delegated enqueuer on the
// invoke route — a worker on the execute route is bound and acts as itself —
// so both halves claim as that enqueuer, and the re-claim is by a bound
// worker. The bound now also counts the executor ref the holder claimed
// under.
func TestAClaimantChainTooLongToRetainIsRefusedAtClaimTime(t *testing.T) {
	// Legal for a decision — six entries, well under MaxOnBehalfOfEntries,
	// each well under shoal.MaxIDBytes — and 6144 bytes in aggregate, which no
	// retained holder may carry.
	fixture := newExecutorClaimFixture(t)
	overLong, overLongQueued := fixture.delegatedQueued(
		t, "long", delegationChainOf(6, 1024))
	_, err := fixture.service.Claim(overLong, ClaimRequest{
		ID: overLongQueued.ID, ExpectedVersion: overLongQueued.Version,
		ClaimID: []byte("long-claim"), Lease: time.Nanosecond,
		Context: dispatchContext(fixture.now, "long-request"),
	})
	if err == nil {
		t.Fatal("a claimant whose chain cannot be retained was accepted: the " +
			"next claim will produce a record encodeAction refuses, and the " +
			"action is then unclaimable by anyone, permanently")
	}
	if !shoal.IsErrorCode(err, shoal.ErrorInvalidArgument) {
		t.Fatalf("refusal is not an invalid argument, so a caller cannot tell "+
			"it is the chain rather than the record: %v", err)
	}

	// And a chain inside the bound survives the whole sequence the brick
	// broke: claim, lapse, re-claim by someone else, and the retained holder
	// is recognisable to the route that needs it.
	fixture = newExecutorClaimFixture(t)
	withinBound := delegationChainOf(3, 1024)
	delegated, queued := fixture.delegatedQueued(t, "short", withinBound)
	claimed, err := fixture.service.Claim(delegated, ClaimRequest{
		ID: queued.ID, ExpectedVersion: queued.Version,
		ClaimID: []byte("short-claim"), Lease: time.Nanosecond,
		Context: dispatchContext(fixture.now, "short-request"),
	})
	if err != nil {
		t.Fatalf("a delegated chain inside the bound was refused, so the "+
			"bound above is refusing ordinary callers: %v", err)
	}
	fixture.advance(t, time.Second)
	second := fixture.namedExecutor(t, "second")
	if _, err := fixture.service.Claim(second, ClaimRequest{
		ID: queued.ID, ExpectedVersion: claimed.Version,
		ClaimID: []byte("second-claim"), Lease: time.Minute,
		Context: dispatchContext(fixture.now, "second-request"),
	}); err != nil {
		t.Fatalf("the re-claim that bricked is still refused: %v", err)
	}
	if _, err := fixture.service.ReportAmbiguity(
		delegated, AmbiguityRequest{
			ID: queued.ID, ClaimFence: claimed.ClaimFence,
			Outcome: AmbiguityOutcomeUnknown,
			Context: dispatchContext(fixture.now, "short-request"),
		}); err != nil {
		t.Fatalf("the displaced delegated holder cannot report what it "+
			"attempted, so its retained chain was not recognised: %v", err)
	}
}

// TestARecorderFailureLeavesTheRecordUnchanged is what licenses
// fleetDispatchError to answer ErrRecordingUnavailable as a plain 503, with no
// indeterminate marker, while marking ErrExecutionAmbiguous and
// ErrActionCommitted.
//
// All three were one arm, so a caller got a bare 503 and could not tell "retry,
// nothing happened" from "stop, something may have happened" — the sharpest
// distinction this surface has.
//
// Splitting them rested on a claim about the raise sites, and reading them was
// not enough: the first version of this said "all nine", there are ten, one is
// a RecordApproval rather than a RecordAction failure, and one of them commits
// a durable approval transition before it raises. That one is covered by
// TestAMaterializationRecorderFailureIsResumable, which asserts both that it
// commits and that a retry resolves it.
//
// This table covers the five dispatch phases reachable from this fixture:
// claim, cancel, extension, report and completion. The remaining sites —
// enqueue_admission, effect_admission, the admission grant and denial, and the
// two approval recorders — are not covered here, and the honest reading of
// that is that the property is asserted where a fixture can reach it rather
// than everywhere it holds.
//
// Each phase's recorder failure must leave the stored record byte-identical.
// The one exception is deliberate and checked: the post-effect recorder failure
// in applyExecutionResult joins ErrExecutionAmbiguous as well, because there the
// effect may genuinely have happened — and fleetDispatchError's marked arm is
// ordered first precisely so that error is marked.
func TestARecorderFailureLeavesTheRecordUnchanged(t *testing.T) {
	// setup performs whatever legitimate transitions the phase needs and
	// returns the state act will work from. The snapshot is taken between the
	// two: taking it before setup made two sub-cases fail on their own
	// successful claim, which is a fixture error and not a product one.
	for _, probe := range []struct {
		phase string
		// ambiguous is true where the failure is genuinely post-effect, so the
		// error must also carry ErrExecutionAmbiguous and the record may move.
		ambiguous bool
		setup     func(t *testing.T, f *executorClaimFixture) (context.Context, ActionRecord)
		act       func(t *testing.T, f *executorClaimFixture, who context.Context, from ActionRecord) error
	}{
		{
			phase: "claim_admission",
			setup: func(t *testing.T, f *executorClaimFixture) (context.Context, ActionRecord) {
				return f.namedExecutor(t, "worker"), f.queued
			},
			act: func(_ *testing.T, f *executorClaimFixture, who context.Context, from ActionRecord) error {
				_, err := f.service.Claim(who, ClaimRequest{
					ID: f.queued.ID, ExpectedVersion: from.Version,
					ClaimID: []byte("worker-claim"), Lease: time.Minute,
					Context: dispatchContext(f.now, "worker-request"),
				})
				return err
			},
		},
		{
			phase: "cancel_admission",
			setup: func(_ *testing.T, f *executorClaimFixture) (context.Context, ActionRecord) {
				return f.enqueuer, f.queued
			},
			act: func(_ *testing.T, f *executorClaimFixture, who context.Context, from ActionRecord) error {
				_, err := f.service.Cancel(who, CancelRequest{
					ID: f.queued.ID, ExpectedVersion: from.Version,
					MutationKey: []byte("cancel-key"),
					Context:     dispatchContext(f.now, "request"),
				})
				return err
			},
		},
		{
			phase: "claim_extension",
			setup: func(t *testing.T, f *executorClaimFixture) (context.Context, ActionRecord) {
				worker := f.namedExecutor(t, "worker")
				claimed, err := f.service.Claim(worker, ClaimRequest{
					ID: f.queued.ID, ExpectedVersion: f.queued.Version,
					ClaimID: []byte("worker-claim"), Lease: time.Minute,
					Context: dispatchContext(f.now, "worker-request"),
				})
				if err != nil {
					t.Fatalf("the claim this phase needs was refused: %v", err)
				}
				return worker, claimed
			},
			act: func(_ *testing.T, f *executorClaimFixture, who context.Context, from ActionRecord) error {
				_, err := f.service.ExtendClaim(who, ExtendRequest{
					ID: f.queued.ID, ExpectedVersion: from.Version,
					ClaimID: []byte("worker-claim"), Lease: 2 * time.Minute,
					Context: dispatchContext(f.now, "worker-request"),
				})
				return err
			},
		},
		{
			phase: "ambiguity_report",
			setup: func(t *testing.T, f *executorClaimFixture) (context.Context, ActionRecord) {
				worker, _, claimed := f.lapsedClaimant(t, "worker")
				return worker, claimed
			},
			act: func(_ *testing.T, f *executorClaimFixture, who context.Context, from ActionRecord) error {
				_, err := f.service.ReportAmbiguity(who, AmbiguityRequest{
					ID: f.queued.ID, ClaimFence: from.ClaimFence,
					Outcome: AmbiguityOutcomeUnknown,
					Context: dispatchContext(f.now, "worker-request"),
				})
				return err
			},
		},
		{
			phase:     "effect_outcome",
			ambiguous: true,
			setup: func(t *testing.T, f *executorClaimFixture) (context.Context, ActionRecord) {
				worker := f.namedExecutor(t, "worker")
				claimed, err := f.service.Claim(worker, ClaimRequest{
					ID: f.queued.ID, ExpectedVersion: f.queued.Version,
					ClaimID: []byte("worker-claim"), Lease: time.Minute,
					Context: dispatchContext(f.now, "worker-request"),
				})
				if err != nil {
					t.Fatalf("the claim this phase needs was refused: %v", err)
				}
				return worker, claimed
			},
			act: func(_ *testing.T, f *executorClaimFixture, who context.Context, from ActionRecord) error {
				_, err := f.service.CompleteClaim(who, CompletionRequest{
					ID: f.queued.ID, ExpectedVersion: from.Version,
					ClaimFence: from.ClaimFence, ClaimID: []byte("worker-claim"),
					Result:  ExecutionResult{Output: json.RawMessage(`{"ok":true}`)},
					Context: dispatchContext(f.now, "worker-request"),
				})
				return err
			},
		},
	} {
		t.Run(probe.phase, func(t *testing.T) {
			fixture := newExecutorClaimFixture(t)
			who, from := probe.setup(t, fixture)
			// After setup, so the snapshot is the state act works from, and
			// before failPhase, so setup's own audits are not the thing
			// refused.
			before := cloneActionRecord(
				fixture.dispatchStore.records[string(fixture.queued.ID)])
			fixture.recorder.failPhase = probe.phase

			err := probe.act(t, fixture, who, from)
			if err == nil {
				t.Fatal("the recorder failure was not reported at all, so a " +
					"transition committed with no privileged audit entry")
			}
			if !errors.Is(err, ErrRecordingUnavailable) {
				t.Fatalf("the failure does not carry ErrRecordingUnavailable, "+
					"so the transport cannot classify it: %v", err)
			}
			if got := errors.Is(err, ErrExecutionAmbiguous); got != probe.ambiguous {
				t.Fatalf("ErrExecutionAmbiguous = %v, want %v: the transport "+
					"marks a 503 indeterminate on exactly this sentinel, so "+
					"getting it wrong either hides a possible effect or "+
					"invents one", got, probe.ambiguous)
			}

			after := fixture.dispatchStore.records[string(fixture.queued.ID)]
			if probe.ambiguous {
				// Nothing is asserted about the record here beyond its
				// continued existence: the point of the ambiguous case is that
				// the service cannot promise what happened.
				if len(after.ID) == 0 {
					t.Fatal("the record vanished")
				}
				return
			}
			if after.Version != before.Version ||
				after.State != before.State ||
				after.ClaimFence != before.ClaimFence ||
				!bytes.Equal(after.ClaimID, before.ClaimID) ||
				len(after.AmbiguityReports) != len(before.AmbiguityReports) ||
				!after.ClaimLeaseUntil.Equal(before.ClaimLeaseUntil) {
				t.Fatalf("the recorder failed and the record moved anyway "+
					"(version %d→%d, state %s→%s, fence %d→%d): this 503 is "+
					"answered unmarked, so a caller is told to retry "+
					"something that already committed",
					before.Version, after.Version, before.State, after.State,
					before.ClaimFence, after.ClaimFence)
			}
		})
	}
}

// TestAnExecutorCannotClaimTheServicesAdjudication is #508: the error code was
// an unauthenticated string that the service and the executor both wrote,
// through the same assignment, with nothing recording which.
//
// Both halves are asserted, because either alone is insufficient. Reserving
// the service's codes stops a worker claiming an adjudication that never
// happened; recording the origin tells a reader which of the two wrote any
// code at all, including the next code someone adds without remembering to
// reserve it.
//
// The sharpest reserved code is invalid_executor_error, which the service
// assigns *because* it rejected the worker's own code — so a worker reporting
// it produces a record saying Shoal refused a code Shoal never saw.
func TestAnExecutorCannotClaimTheServicesAdjudication(t *testing.T) {
	claimed := func(t *testing.T, f *executorClaimFixture) (
		context.Context, ActionRecord,
	) {
		t.Helper()
		worker := f.namedExecutor(t, "worker")
		record, err := f.service.Claim(worker, ClaimRequest{
			ID: f.queued.ID, ExpectedVersion: f.queued.Version,
			ClaimID: []byte("worker-claim"), Lease: time.Minute,
			Context: dispatchContext(f.now, "worker-request"),
		})
		if err != nil {
			t.Fatalf("the claim this test needs was refused: %v", err)
		}
		return worker, record
	}

	for _, reserved := range []string{
		"invalid_executor_output",
		"invalid_executor_evidence",
		"invalid_executor_error",
		"executor_error",
	} {
		t.Run("refuses "+reserved, func(t *testing.T) {
			fixture := newExecutorClaimFixture(t)
			worker, record := claimed(t, fixture)
			_, err := fixture.service.CompleteClaim(worker, CompletionRequest{
				ID: fixture.queued.ID, ExpectedVersion: record.Version,
				ClaimFence: record.ClaimFence, ClaimID: []byte("worker-claim"),
				Failed:  true,
				Result:  ExecutionResult{ErrorCode: reserved},
				Context: dispatchContext(fixture.now, "worker-request"),
			})
			if err == nil {
				t.Fatalf("a worker reported %q, which the service assigns, "+
					"so the record reads as an adjudication that never "+
					"happened", reserved)
			}
			if !shoal.IsErrorCode(err, shoal.ErrorInvalidArgument) {
				t.Fatalf("refused as %v, want an invalid argument", err)
			}
			// And nothing was written: this is a boundary refusal, so the
			// claim is still the worker's to complete properly.
			stored := fixture.dispatchStore.records[string(fixture.queued.ID)]
			if stored.State != DispatchClaimed {
				t.Fatalf("the refused completion moved the record to %q",
					stored.State)
			}
		})
	}

	t.Run("records the executor as the origin", func(t *testing.T) {
		fixture := newExecutorClaimFixture(t)
		worker, record := claimed(t, fixture)
		// #492 fixed: the report was recorded, so the operation succeeded and
		// the committed record comes back. Both are read — the returned
		// record and the stored one — because returning a record that does
		// not match what landed would be the same defect in a new place.
		returned, err := fixture.service.CompleteClaim(
			worker, CompletionRequest{
				ID: fixture.queued.ID, ExpectedVersion: record.Version,
				ClaimFence: record.ClaimFence, ClaimID: []byte("worker-claim"),
				Failed:  true,
				Result:  ExecutionResult{ErrorCode: "payment_declined"},
				Context: dispatchContext(fixture.now, "worker-request"),
			})
		if err != nil {
			t.Fatalf("a durably recorded failure was answered as a "+
				"refusal: %v", err)
		}
		completed := fixture.dispatchStore.records[string(fixture.queued.ID)]
		if returned.State != completed.State ||
			returned.ErrorCode != completed.ErrorCode ||
			returned.ErrorCodeOrigin != completed.ErrorCodeOrigin {
			t.Fatalf("the returned record disagrees with the stored one: "+
				"%q/%q/%q vs %q/%q/%q", returned.State, returned.ErrorCode,
				returned.ErrorCodeOrigin, completed.State, completed.ErrorCode,
				completed.ErrorCodeOrigin)
		}
		if completed.State != DispatchFailed {
			t.Fatalf("the failure was not recorded: state %q", completed.State)
		}
		if completed.ErrorCode != "payment_declined" {
			t.Fatalf("error code = %q", completed.ErrorCode)
		}
		if completed.ErrorCodeOrigin != ErrorCodeOriginExecutor {
			t.Fatalf("origin = %q, want %q: a reader cannot otherwise tell "+
				"this from the service's own adjudication",
				completed.ErrorCodeOrigin, ErrorCodeOriginExecutor)
		}
	})

	t.Run("records the service as the origin", func(t *testing.T) {
		// A successful report whose output fails the action's schema: the
		// service adjudicates and assigns invalid_executor_output itself.
		fixture := newExecutorClaimFixture(t)
		worker, record := claimed(t, fixture)
		// Committed as failed with the service's own code, and answered as a
		// success, for the same reason as above (#492).
		returned, err := fixture.service.CompleteClaim(
			worker, CompletionRequest{
				ID: fixture.queued.ID, ExpectedVersion: record.Version,
				ClaimFence: record.ClaimFence, ClaimID: []byte("worker-claim"),
				Result:  ExecutionResult{Output: json.RawMessage(`"not an object"`)},
				Context: dispatchContext(fixture.now, "worker-request"),
			})
		if err != nil {
			t.Fatalf("a committed invalid_executor_output was answered as a "+
				"refusal: %v", err)
		}
		completed := fixture.dispatchStore.records[string(fixture.queued.ID)]
		if returned.ErrorCode != completed.ErrorCode ||
			returned.ErrorCodeOrigin != completed.ErrorCodeOrigin {
			t.Fatalf("the returned record disagrees with the stored one: "+
				"%q/%q vs %q/%q", returned.ErrorCode, returned.ErrorCodeOrigin,
				completed.ErrorCode, completed.ErrorCodeOrigin)
		}
		if completed.State != DispatchFailed {
			t.Fatalf("the adjudication was not recorded: state %q",
				completed.State)
		}
		if completed.ErrorCode != "invalid_executor_output" {
			t.Fatalf("error code = %q, want the service's adjudication",
				completed.ErrorCode)
		}
		if completed.ErrorCodeOrigin != ErrorCodeOriginService {
			t.Fatalf("origin = %q, want %q: the service assigned this code "+
				"and a reader must be able to tell",
				completed.ErrorCodeOrigin, ErrorCodeOriginService)
		}
	})
}

// TestAGateRefusalNeverPrecedesTheStandingCheck is the invariant that makes
// the approval and attestation gates' distinguishable refusals safe.
//
// Both gates return a conflict naming the requirement rather than
// auth.ObjectNotFound, which departs from this surface's rule that every
// standing refusal is indistinguishable (#398). That is sound only while
// #398's rule is doing its own job first: the rule is about callers *without*
// standing, and such a caller must still be refused before any gate runs.
//
// So a caller who fails authorizedClaimant on an approval-required action has
// to be told exactly what a caller probing a nonexistent action ID is told,
// byte for byte. If the two ever differ, the gate has become an existence
// oracle for the scope — and the gate's refusal, which is the useful half for
// a legitimate worker, would have to go back to being not-found.
//
// The legitimate worker's side is the other half of the same decision: a
// not-found there would be actively misleading, because it would retry or
// reconcile rather than await an approval that is the actual answer.
func TestAGateRefusalNeverPrecedesTheStandingCheck(t *testing.T) {
	fixture := newExecutorClaimFixture(t)

	// Turn the requirement on for the registered action, so the gate would
	// fire for anyone who got past standing.
	stored := fixture.registryStore.records[fixture.queued.AgentID]
	descriptor := cloneDescriptor(stored.Descriptor)
	for capability := range descriptor.Capabilities {
		for action := range descriptor.Capabilities[capability].Actions {
			descriptor.Capabilities[capability].Actions[action].
				RequiresApproval = true
		}
	}
	stored.Descriptor = descriptor
	fixture.registryStore.records[fixture.queued.AgentID] = stored

	// A caller with standing is told the requirement, which is the point of
	// the distinguishable refusal.
	worker := fixture.namedExecutor(t, "worker")
	withStanding := fixture.service.claimOnce(t, worker, "worker-request")
	if !errors.Is(withStanding, ErrApprovalRequired) {
		t.Fatalf("a caller with standing was not told the requirement: %v",
			withStanding)
	}

	// A caller with no standing gets not-found, and must get exactly what a
	// probe for an action that does not exist gets.
	//
	// "No standing" has to mean no standing on the *descriptor*, not merely
	// "not the enqueuer". An execute-holder in the same scope does have
	// standing — that is the whole of #437 — and the first version of this
	// probe used one, got the conflict, and read as an oracle when it was
	// the fixture that was wrong. This one is outside the scope entirely, so
	// resolveActionBinding refuses it before any gate.
	outsider, err := auth.NewDecision(auth.DecisionConfig{
		Subject: "outsider", Actor: "outsider-actor",
		AuthorizationDomain: []byte("domain"),
		AllowedOperations:   []auth.Operation{auth.OperationExecute},
		PermittedSourceIDs:  [][]byte{[]byte("other-source")},
		PermittedPolicyIDs:  [][]byte{[]byte("other-policy")},
		PolicyGeneration:    1,
		AuthenticationExpires: time.Date(
			2026, 9, 7, 0, 0, 0, 0, time.UTC),
		RequestID: "outsider-request", CorrelationID: "correlation",
	})
	if err != nil {
		t.Fatal(err)
	}
	stranger := bindDecision(t, fixture.authority, outsider)
	refused := fixture.service.claimOnce(t, stranger, "outsider-request")
	absent := fixture.service.claimAbsent(t, stranger, "outsider-request")
	if refused == nil || absent == nil {
		t.Fatalf("a caller without standing was not refused: %v / %v",
			refused, absent)
	}
	if refused.Error() != absent.Error() {
		t.Fatalf("an approval-required action refuses a caller without "+
			"standing as %q while an absent action refuses it as %q: the "+
			"gate is an existence oracle for this scope",
			refused, absent)
	}
	if !shoal.IsErrorCode(refused, shoal.ErrorNotFound) {
		t.Fatalf("the standing refusal is not a not-found: %v", refused)
	}
}

// claimOnce attempts a claim on the fixture's queued action and returns only
// the error, which is what these probes compare.
func (s *DispatchService) claimOnce(
	t *testing.T, who context.Context, request string,
) error {
	t.Helper()
	_, err := s.Claim(who, ClaimRequest{
		ID: []byte("action"), ExpectedVersion: 1,
		ClaimID: []byte("probe-claim"), Lease: time.Minute,
		Context: dispatchContext(
			time.Date(2026, 9, 6, 12, 0, 0, 0, time.UTC), request),
	})
	return err
}

// claimAbsent attempts a claim on an action ID that was never enqueued.
func (s *DispatchService) claimAbsent(
	t *testing.T, who context.Context, request string,
) error {
	t.Helper()
	_, err := s.Claim(who, ClaimRequest{
		ID: []byte("no-such-action"), ExpectedVersion: 1,
		ClaimID: []byte("probe-claim"), Lease: time.Minute,
		Context: dispatchContext(
			time.Date(2026, 9, 6, 12, 0, 0, 0, time.UTC), request),
	})
	return err
}

// TestEffectPossibleAnswersTheQuestionItNames is #510: the flag was
// unconditionally true on every terminal record, so it carried no information
// where an operator actually reads it.
//
// applyClaim sets it only for an action declaring EffectMutatesExternal or
// EffectEgressesContent, with a comment explaining that an action which
// "neither mutates externally nor transmits leaves its whole outcome in
// Shoal's own record, so nothing has to be assumed about it".
// applyExecutionResult then set it true for everything, and
// ActionRecord.Validate *required* that — so the thoughtful write ran first
// and the careless one ran last and won, and the model forbade the honest
// answer.
//
// Both halves are asserted here, because asserting only the false case would
// pass against a service that never set the flag at all.
func TestEffectPossibleAnswersTheQuestionItNames(t *testing.T) {
	declare := func(t *testing.T, f *executorClaimFixture, effects Effects) {
		t.Helper()
		stored := f.registryStore.records[f.queued.AgentID]
		descriptor := cloneDescriptor(stored.Descriptor)
		for capability := range descriptor.Capabilities {
			for action := range descriptor.Capabilities[capability].Actions {
				descriptor.Capabilities[capability].Actions[action].
					Effects = effects
			}
		}
		stored.Descriptor = descriptor
		f.registryStore.records[f.queued.AgentID] = stored
	}
	complete := func(t *testing.T, f *executorClaimFixture) ActionRecord {
		t.Helper()
		worker := f.namedExecutor(t, "worker")
		claimed, err := f.service.Claim(worker, ClaimRequest{
			ID: f.queued.ID, ExpectedVersion: f.queued.Version,
			ClaimID: []byte("worker-claim"), Lease: time.Minute,
			Context: dispatchContext(f.now, "worker-request"),
		})
		if err != nil {
			t.Fatalf("the claim this test needs was refused: %v", err)
		}
		if _, err := f.service.CompleteClaim(worker, CompletionRequest{
			ID: f.queued.ID, ExpectedVersion: claimed.Version,
			ClaimFence: claimed.ClaimFence, ClaimID: []byte("worker-claim"),
			Result:  ExecutionResult{Output: json.RawMessage(`{"ok":true}`)},
			Context: dispatchContext(f.now, "worker-request"),
		}); err != nil {
			t.Fatalf("the completion was refused: %v", err)
		}
		return f.dispatchStore.records[string(f.queued.ID)]
	}

	t.Run("an action declaring no external effect", func(t *testing.T) {
		fixture := newExecutorClaimFixture(t)
		declare(t, fixture, nil)
		completed := complete(t, fixture)
		if completed.State != DispatchSucceeded {
			t.Fatalf("state = %q", completed.State)
		}
		if completed.EffectPossible {
			t.Fatal("a terminal record for an action that mutates nothing " +
				"externally and transmits nothing still asserts an effect " +
				"may have happened, so the flag tells an operator nothing " +
				"and reconciliation cannot filter on it")
		}
	})

	t.Run("an action declaring external mutation", func(t *testing.T) {
		fixture := newExecutorClaimFixture(t)
		declare(t, fixture, Effects{EffectMutatesExternal})
		completed := complete(t, fixture)
		if !completed.EffectPossible {
			t.Fatal("a terminal record for an externally-mutating action " +
				"does not assert that an effect may have happened, which is " +
				"the one thing this flag has to get right")
		}
	})

	t.Run("an action declaring egress", func(t *testing.T) {
		fixture := newExecutorClaimFixture(t)
		declare(t, fixture, Effects{EffectEgressesContent})
		completed := complete(t, fixture)
		if !completed.EffectPossible {
			t.Fatal("content that left the host cannot be recalled, so an " +
				"egressing action must assert a possible effect")
		}
	})
}

// TestInvokeRefusesADispatchOnlyBindingWithoutClaiming is #455: a synchronous
// invoke against a gateway-bound descriptor marked EffectPossible before
// reaching any executor.
//
// ExecuteClaim asserts the bound reference implements ActionExecutor, and an
// ExternalEffectBinding deliberately does not — the work reaches a gateway
// over the dispatch queue and the completion report is what Shoal records, so
// nothing runs in process. Reached through Invoke, that assertion failed
// *after* Claim had committed, and Claim sets EffectPossible for an action
// declaring external mutation or egress.
//
// So the record said an effect may have happened, for an action that provably
// did nothing: the executor resolution failed in this process, before anything
// was serialized and before anything left the host. The flag is monotonic
// (#461), so it never washed out — every accidental invoke permanently added a
// record to the set an operator reconciles by hand.
//
// The assertion that matters is not that the call fails. It failed before too.
// It is that the record is left unclaimed and unmarked.
func TestInvokeRefusesADispatchOnlyBindingWithoutClaiming(t *testing.T) {
	fixture := newExecutorClaimFixture(t)

	// An action that declares an external effect, so Claim would mark it.
	stored := fixture.registryStore.records[fixture.queued.AgentID]
	descriptor := cloneDescriptor(stored.Descriptor)
	for capability := range descriptor.Capabilities {
		for action := range descriptor.Capabilities[capability].Actions {
			descriptor.Capabilities[capability].Actions[action].Effects =
				Effects{EffectMutatesExternal}
		}
	}
	stored.Descriptor = descriptor
	fixture.registryStore.records[fixture.queued.AgentID] = stored

	// A dispatch-only binding: it declares a ceiling and runs nothing.
	binding, err := NewExternalEffectBinding(Effects{EffectMutatesExternal})
	if err != nil {
		t.Fatal(err)
	}
	fixture.executors["exec"] = binding

	before := fixture.dispatchStore.records[string(fixture.queued.ID)]
	if before.EffectPossible {
		t.Fatal("the queued record already claims a possible effect, so this " +
			"test cannot tell whether the invoke added one")
	}

	if _, err := fixture.service.Invoke(fixture.enqueuer, InvokeRequest{
		Enqueue: dispatchEnqueue(fixture.now, "request"),
		ClaimID: []byte("invoke-claim"), Lease: time.Minute,
	}); err == nil {
		t.Fatal("a synchronous invoke against a dispatch-only binding was " +
			"accepted, so something ran that cannot run in process")
	}

	after := fixture.dispatchStore.records[string(fixture.queued.ID)]
	if after.EffectPossible {
		t.Fatal("the refused invoke left a record claiming an effect may " +
			"have happened, for an action whose executor could not run here: " +
			"the flag is monotonic, so an operator reconciles this by hand " +
			"forever")
	}
	if after.State != DispatchQueued {
		t.Fatalf("the refused invoke moved the record to %q; it should be "+
			"left queued and claimable by a worker that can run it",
			after.State)
	}
	if after.ClaimFence != before.ClaimFence {
		t.Fatalf("the refused invoke advanced the claim fence from %d to %d, "+
			"so it counts toward what an operator reads as re-claims",
			before.ClaimFence, after.ClaimFence)
	}
}

// TestAnIdenticalAmbiguityReportIsAReplay fixes an honest case and an
// adversarial one with the same change.
//
// The honest case: a worker whose response was lost retries, and the retry
// consumed one of eight report slots for a report the record already had — on
// the one route whose purpose is recourse for a worker that cannot otherwise
// get its outcome recorded.
//
// The adversarial case is #514's report-budget exhaustion. The budget is per
// record and shared across every principal the record has seen, and the
// reproduction filed eight *identical* reports from a lapsed co-tenant until
// the worker that performed the effect was refused. That reproduction no
// longer works.
//
// It does not close #514, and the test says so: a co-tenant willing to vary
// its reports still exhausts the budget. What the mitigation claims is that
// eight distinct and plausible reports from one principal are a much less
// comfortable thing to write off as organic.
//
// The append rule it preserves is the other half: a retry carrying *different*
// information is still appended, because an operator wants both rather than
// the later silently overwriting the earlier.
func TestAnIdenticalAmbiguityReportIsAReplay(t *testing.T) {
	fixture := newExecutorClaimFixture(t)
	worker, fence, _ := fixture.lapsedClaimant(t, "worker")
	report := func(outcome AmbiguityOutcome, reference string) (ActionRecord, error) {
		return fixture.service.ReportAmbiguity(worker, AmbiguityRequest{
			ID: fixture.queued.ID, ClaimFence: fence, Outcome: outcome,
			Target: "payments.example.test", Reference: reference,
			Context: dispatchContext(fixture.now, "worker-request"),
		})
	}

	first, err := report(AmbiguityOutcomeUnknown, "ch_1")
	if err != nil {
		t.Fatalf("the first report was refused: %v", err)
	}
	if len(first.AmbiguityReports) != 1 {
		t.Fatalf("reports after the first = %d", len(first.AmbiguityReports))
	}

	// Identical: a replay. Success, the record back, and nothing appended.
	replayed, err := report(AmbiguityOutcomeUnknown, "ch_1")
	if err != nil {
		t.Fatalf("an identical retry was refused, so a worker whose response "+
			"was lost cannot safely retry the one route that exists for it: %v",
			err)
	}
	if len(replayed.AmbiguityReports) != 1 {
		t.Fatalf("an identical retry appended a second report (%d total), so "+
			"it still consumes the shared budget",
			len(replayed.AmbiguityReports))
	}
	if replayed.Version != first.Version {
		t.Fatalf("an identical retry advanced the version from %d to %d, so "+
			"it still writes", first.Version, replayed.Version)
	}

	// Different in any respect: appended, because a retry that carries new
	// information is exactly what the append rule is for.
	for _, probe := range []struct {
		name      string
		outcome   AmbiguityOutcome
		reference string
	}{
		{"a changed outcome", AmbiguityEffectObserved, "ch_1"},
		{"a reference obtained since", AmbiguityOutcomeUnknown, "ch_2"},
	} {
		t.Run(probe.name, func(t *testing.T) {
			before := fixture.dispatchStore.records[string(fixture.queued.ID)]
			after, err := report(probe.outcome, probe.reference)
			if err != nil {
				t.Fatalf("a report differing in %s was refused: %v",
					probe.name, err)
			}
			if len(after.AmbiguityReports) != len(before.AmbiguityReports)+1 {
				t.Fatalf("a report differing in %s was not appended: %d then "+
					"%d", probe.name, len(before.AmbiguityReports),
					len(after.AmbiguityReports))
			}
		})
	}
}

// TestAClaimDoesNotRecordTheClaimantsDelegationAsTheEnqueuesIs the secondary
// half of #460.
//
// decisionOperations appends OperationDelegate whenever the decision carries a
// non-empty OnBehalfOf, and the claim path widened AuthorizedOperations with
// its whole result. So a delegated worker claiming an action it did not
// enqueue permanently added delegate to that record — asserting the enqueue
// was delegated on the strength of who later picked the work up.
//
// AuthorizedOperations is the one authorization field still written per
// transition rather than at creation, which is what makes this reachable;
// #443 removed the other such write (record.Actor) for closely related
// reasons. The audit trail is what an operator reads when an external effect
// may or may not have happened, so a record misdescribing the authority an
// irreversible action was taken under is the defect, not untidiness.
func TestAClaimDoesNotRecordTheClaimantsDelegationAsTheEnqueues(t *testing.T) {
	fixture := newExecutorClaimFixture(t)

	// The enqueuer is not delegated: the fixture's queued record must not
	// already carry delegate, or this test cannot tell what the claim added.
	if containsOperationForTest(
		fixture.queued.AuthorizedOperations, auth.OperationDelegate) {
		t.Fatalf("the queued record already records delegation (%v), so this "+
			"test cannot attribute it to the claim",
			fixture.queued.AuthorizedOperations)
	}

	// A worker holding execute and acting on someone's behalf. It holds
	// delegate as well, because a delegated caller without that standing
	// cannot learn the action exists at all (#539) — so this was the only
	// shape the defect occurred in.
	//
	// Since #391 that shape cannot claim at all: a worker acts as itself, so
	// the execute route refuses a delegated caller, as not-found. That closes
	// the defect at its source, and the bound worker below checks what a
	// claim does record.
	delegatedWorker, err := fixture.authority.Binder().Bind(
		context.Background(),
		dispatchDecisionFor(t, principal{
			subject: "worker-subject", actor: "worker-actor",
			request: "worker-request", clientID: "worker-client",
			onBehalfOf: []shoal.ID{"worker-delegator"},
		}, auth.OperationExecute, auth.OperationDelegate))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.service.Claim(delegatedWorker, ClaimRequest{
		ID: fixture.queued.ID, ExpectedVersion: fixture.queued.Version,
		ClaimID: []byte("worker-claim"), Lease: time.Minute,
		Context: dispatchContext(fixture.now, "worker-request"),
	}); !shoal.IsErrorCode(err, shoal.ErrorNotFound) {
		t.Fatalf("a delegated worker claimed on the execute route: %v", err)
	}

	worker := fixture.namedExecutor(t, "worker")
	claimed, err := fixture.service.Claim(worker, ClaimRequest{
		ID: fixture.queued.ID, ExpectedVersion: fixture.queued.Version,
		ClaimID: []byte("worker-claim"), Lease: time.Minute,
		Context: dispatchContext(fixture.now, "worker-request"),
	})
	if err != nil {
		t.Fatalf("a bound worker could not claim: %v", err)
	}

	if containsOperationForTest(
		claimed.AuthorizedOperations, auth.OperationDelegate) {
		t.Fatalf("the claim recorded the claimant's delegation on the "+
			"record's own authority (%v): the enqueue was not delegated, and "+
			"this is the field an operator reads to learn under what "+
			"authority the action was authorized",
			claimed.AuthorizedOperations)
	}

	// The operation that did authorize the claim is there, and has to be: the
	// publisher authorizes a claim event against TransitionOperation and
	// refuses a publication whose operation is absent from this set, which
	// comes back as ErrActionCommitted — committed work whose worker is told
	// to reconcile.
	if !containsOperationForTest(
		claimed.AuthorizedOperations, auth.OperationExecute) {
		t.Fatalf("the claim did not record the operation that authorized it "+
			"(%v), so its lifecycle events cannot publish",
			claimed.AuthorizedOperations)
	}
	if claimed.TransitionOperation != auth.OperationExecute {
		t.Fatalf("transition operation = %q, want %q",
			claimed.TransitionOperation, auth.OperationExecute)
	}

	// Cancel is not affected and deliberately not changed. It resolves
	// through authorizedCurrent with requirePrincipal true, so only the
	// record's own principal reaches it: a delegated canceller is the
	// delegated enqueuer, whose delegation is already on the record from
	// creation. A probe here was written, could not be made to reach the
	// path — a canceller differing from the enqueuer in client ID or chain
	// gets object_not_found — and was removed rather than weakened into one
	// that passes without proving anything.

	// And the enqueuer's own delegation is still recorded where it belongs.
	// Without this the change would read as "delegation is never recorded",
	// which is a different and wrong fix.
	delegated := newExecutorClaimFixture(t)
	enqueuer, err := delegated.authority.Binder().Bind(
		context.Background(),
		dispatchDecisionFor(t, principal{
			subject: "owner", actor: "owner-actor", request: "owner-request",
			onBehalfOf: []shoal.ID{"owner-delegator"},
		}, auth.OperationInvoke, auth.OperationDispatch, auth.OperationDelegate))
	if err != nil {
		t.Fatal(err)
	}
	queued, err := delegated.service.Enqueue(enqueuer, EnqueueRequest{
		ID: []byte("delegated-action"), IdempotencyKey: []byte("delegated-key"),
		AgentID:         delegated.queued.AgentID,
		AgentGeneration: delegated.queued.AgentGeneration,
		Capability:      delegated.queued.Capability,
		Action:          delegated.queued.Action,
		SourceID:        []byte("source"), PolicyID: []byte("policy"),
		ObjectID: delegated.queued.ObjectID,
		Input:    json.RawMessage(`{"value":1}`),
		Context:  dispatchContext(delegated.now, "owner-request"),
	})
	if err != nil {
		t.Fatalf("a delegated enqueue was refused: %v", err)
	}
	if !containsOperationForTest(
		queued.AuthorizedOperations, auth.OperationDelegate) {
		t.Fatalf("a delegated enqueue did not record its delegation (%v): "+
			"creation is where that belongs, because there the delegating "+
			"decision is the enqueuer's own",
			queued.AuthorizedOperations)
	}
}

func containsOperationForTest(
	values []auth.Operation, wanted auth.Operation,
) bool {
	for _, value := range values {
		if value == wanted {
			return true
		}
	}
	return false
}

// confiningExecutor declares that it confines retrieval to the invocation's
// scope, which is the promise #370 requires before an in-process execution
// may run wider than the action's scope.
type confiningExecutor struct{ confines bool }

func (confiningExecutor) Execute(
	context.Context, Invocation,
) (ExecutionResult, error) {
	return ExecutionResult{Output: json.RawMessage(`{"ok":true}`)}, nil
}

func (e confiningExecutor) ConfinesRetrievalToScope() bool { return e.confines }

// TestAnUnconfinedExecutorMayNotRunWiderThanItsScope is #370.
//
// A Descriptor declares Scopes as (SourceID, PolicyID) pairs and
// resolveAction gates every invocation on them, so the scope reads as a bound
// on what the agent reaches. For an in-process executor that retrieves it was
// not one: AskExecutor passes only the question and a top-K to its provider,
// and ChatService authorizes OperationRetrieve domain-wide. Nothing was
// escalated — the principal reads nothing it could not read directly — but
// the record became a cross-scope index of document, section and span
// identifiers, and the scope an operator read on the descriptor described
// none of it.
func TestAnUnconfinedExecutorMayNotRunWiderThanItsScope(t *testing.T) {
	now := time.Date(2026, 9, 6, 12, 0, 0, 0, time.UTC)
	record := ActionRecord{
		SourceID: []byte("source-a"), PolicyID: []byte("policy-a"),
	}
	decisionWith := func(
		t *testing.T, sources, policies [][]byte, ops ...auth.Operation,
	) auth.Decision {
		t.Helper()
		decision, err := auth.NewDecision(auth.DecisionConfig{
			Subject: "owner", Actor: "actor",
			AuthorizationDomain: []byte("domain"),
			AllowedOperations:   ops,
			PermittedSourceIDs:  sources, PermittedPolicyIDs: policies,
			PolicyGeneration:      1,
			AuthenticationExpires: now.Add(time.Hour),
			RequestID:             "request", CorrelationID: "correlation",
		})
		if err != nil {
			t.Fatal(err)
		}
		return decision
	}
	onlyA := [][]byte{[]byte("source-a")}
	aAndB := [][]byte{[]byte("source-a"), []byte("source-b")}
	policyA := [][]byte{[]byte("policy-a")}

	for _, probe := range []struct {
		name     string
		decision func(*testing.T) auth.Decision
		executor Executor
		refused  bool
	}{
		{
			// The case that was silently widening: more sources than the
			// action's scope, and an executor that makes no promise.
			name: "a wider principal and an unconfined executor",
			decision: func(t *testing.T) auth.Decision {
				return decisionWith(t, aAndB, policyA, auth.OperationRetrieve)
			},
			executor: confiningExecutor{},
			refused:  true,
		},
		{
			// An executor that does not implement the interface at all is
			// treated the same way, which is the fail-closed half: a promise
			// nobody made is not a promise.
			name:     "a wider principal and an executor that cannot answer",
			executor: &remoteBoundExecutor{},
			decision: func(t *testing.T) auth.Decision {
				return decisionWith(t, aAndB, policyA, auth.OperationRetrieve)
			},
			refused: true,
		},
		{
			// The promise is what lifts the refusal.
			name:     "a wider principal and a confining executor",
			executor: confiningExecutor{confines: true},
			decision: func(t *testing.T) auth.Decision {
				return decisionWith(t, aAndB, policyA, auth.OperationRetrieve)
			},
		},
		{
			// The shipped configuration: the principal is minted with the
			// same source and policy as the descriptor, so the confinement
			// constrains nothing and nothing changes. If this refused, every
			// reasoning action in the demo would stop working.
			name:     "a principal no wider than the scope",
			executor: confiningExecutor{},
			decision: func(t *testing.T) auth.Decision {
				return decisionWith(t, onlyA, policyA, auth.OperationRetrieve)
			},
		},
		{
			// A principal that may not retrieve cannot widen retrieval, so
			// there is nothing to confine. Refusing here would stop actions
			// that reach no corpus at all.
			name:     "a principal that cannot retrieve",
			executor: confiningExecutor{},
			decision: func(t *testing.T) auth.Decision {
				return decisionWith(t, aAndB, policyA, auth.OperationInvoke)
			},
		},
		{
			// A label policy on the action's own source only narrows what
			// the principal retrieves (#570), so a holder of one is no wider
			// than the scope (#564).
			name:     "a label holder no wider than the scope",
			executor: confiningExecutor{},
			decision: func(t *testing.T) auth.Decision {
				return decisionWith(t, onlyA, [][]byte{
					[]byte("policy-a"), labelPolicyOn(t, "source-a", "secret"),
				}, auth.OperationRetrieve)
			},
		},
		{
			// Only a canonical label policy is left out. An ID that merely
			// shares the namespace prefix is compared like any policy.
			name:     "a malformed label-namespace grant",
			executor: confiningExecutor{},
			decision: func(t *testing.T) auth.Decision {
				return decisionWith(t, onlyA, [][]byte{
					[]byte("policy-a"), []byte(auth.LabelPolicyIDPrefix + "9/source-a/x"),
				}, auth.OperationRetrieve)
			},
			refused: true,
		},
		{
			// A label policy on another source comes with that source, and
			// the source is what widens.
			name:     "a label holder on another source",
			executor: confiningExecutor{},
			decision: func(t *testing.T) auth.Decision {
				return decisionWith(t, aAndB, [][]byte{
					[]byte("policy-a"), labelPolicyOn(t, "source-b", "secret"),
				}, auth.OperationRetrieve)
			},
			refused: true,
		},
		{
			// A wider *policy* is the same defect by the other component, and
			// a check that read only sources would miss it.
			name:     "a wider policy and an unconfined executor",
			executor: confiningExecutor{},
			decision: func(t *testing.T) auth.Decision {
				return decisionWith(t, onlyA, [][]byte{
					[]byte("policy-a"), []byte("policy-b"),
				}, auth.OperationRetrieve)
			},
			refused: true,
		},
	} {
		t.Run(probe.name, func(t *testing.T) {
			err := refuseUnconfinedRetrieval(
				probe.decision(t), probe.executor, record, now)
			if probe.refused {
				if err == nil {
					t.Fatal("an in-process execution ran wider than its " +
						"action's scope, so the record will name evidence " +
						"the descriptor's scope does not cover")
				}
				if !shoal.IsErrorCode(err, shoal.ErrorUnauthorized) {
					t.Fatalf("refusal = %v, want unauthorized", err)
				}
				// Names no source the caller does not already hold: which
				// other sources exist is host configuration.
				if strings.Contains(err.Error(), "source-b") ||
					strings.Contains(err.Error(), "policy-b") {
					t.Fatalf("the refusal names another scope: %v", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("a confined or already-narrow execution was "+
					"refused: %v", err)
			}
		})
	}
}

// TestExecuteClaimAppliesTheRetrievalConfinementCheck pins the call site, not
// the predicate.
//
// Without this, deleting the call from ExecuteClaim leaves every case in
// TestAnUnconfinedExecutorMayNotRunWiderThanItsScope green — the predicate
// would be correct and unreached. That is the defect shape this package has
// produced four times in a row: a mechanism correct in the service and absent
// from the path that needs it.
func TestExecuteClaimAppliesTheRetrievalConfinementCheck(t *testing.T) {
	// A principal that may retrieve from two sources, invoking an action
	// scoped to one of them.
	wider := func(t *testing.T, f *executorClaimFixture) context.Context {
		t.Helper()
		decision, err := auth.NewDecision(auth.DecisionConfig{
			Subject: "owner", Actor: "actor",
			AuthorizationDomain: []byte("domain"),
			AllowedOperations: []auth.Operation{
				auth.OperationDispatch, auth.OperationInvoke,
				auth.OperationRetrieve,
			},
			PermittedSourceIDs: [][]byte{
				[]byte("source"), []byte("other-source"),
			},
			PermittedPolicyIDs: [][]byte{[]byte("policy")},
			PolicyGeneration:   1,
			AuthenticationExpires: time.Date(
				2026, 9, 7, 0, 0, 0, 0, time.UTC),
			RequestID: "request", CorrelationID: "correlation",
		})
		if err != nil {
			t.Fatal(err)
		}
		return bindDecision(t, f.authority, decision)
	}

	t.Run("refused for an unconfined executor", func(t *testing.T) {
		fixture := newExecutorClaimFixture(t)
		fixture.bindExecutor(t, confiningExecutor{})
		caller := wider(t, fixture)
		enqueue := dispatchEnqueue(fixture.now, "request")
		enqueue.ID = []byte("unconfined-action")
		enqueue.IdempotencyKey = []byte("unconfined-idempotency")
		if _, err := fixture.service.Invoke(caller, InvokeRequest{
			Enqueue: enqueue,
			ClaimID: []byte("invoke-claim"), Lease: time.Minute,
		}); err == nil {
			t.Fatal("an in-process execution ran for a principal that may " +
				"retrieve beyond the action's scope, against an executor " +
				"making no confinement promise")
		}
		// Refused before the executor ran, so the record is not terminal and
		// a confining deployment can still run it.
		stored := fixture.dispatchStore.records["unconfined-action"]
		if stored.State == DispatchSucceeded || stored.State == DispatchFailed {
			t.Fatalf("the refused invoke left the record %q", stored.State)
		}
	})

	t.Run("allowed for a confining executor", func(t *testing.T) {
		fixture := newExecutorClaimFixture(t)
		fixture.bindExecutor(t, confiningExecutor{confines: true})
		caller := wider(t, fixture)
		// A distinct action: the fixture already enqueued one in its
		// constructor, and reusing it conflicts on the idempotency key rather
		// than reaching the check.
		enqueue := dispatchEnqueue(fixture.now, "request")
		enqueue.ID = []byte("confined-action")
		enqueue.IdempotencyKey = []byte("confined-idempotency")
		invoked, err := fixture.service.Invoke(caller, InvokeRequest{
			Enqueue: enqueue,
			ClaimID: []byte("invoke-claim"), Lease: time.Minute,
		})
		if err != nil {
			t.Fatalf("an executor that confines retrieval was refused: %v", err)
		}
		if invoked.State != DispatchSucceeded {
			t.Fatalf("state = %q, want succeeded: the executor must have "+
				"actually run, or this passes without reaching the check",
				invoked.State)
		}
	})
}

// stubEvidenceVisibility answers a fixed verdict, or fails.
type stubEvidenceVisibility struct {
	visible bool
	err     error
}

func (s *stubEvidenceVisibility) VisibleToReader(
	_ context.Context, _ []string,
) (bool, error) {
	if s.err != nil {
		return false, s.err
	}
	return s.visible, nil
}

// TestADispatchReaderSeesOnlyEvidenceItsLabelsCover is #369.
//
// Status, Pull and TeamActions authorize a reader on
// (domain, source, policy, object) and applied no visibility-label check
// before returning an ActionRecord, and webapi serializes Evidence in full:
// anchor ID, citation document, revision, section and span identifiers,
// character offsets, node and edge IDs, and the label expression itself.
//
// Every EvidenceRef records its own Visibility, so the record states which
// labels apply, and nothing on the read path consulted it. An action enqueued
// on source A, answered from a document in source B labelled
// secret&project-x, handed its identifiers and that label expression to any
// principal with dispatch or team-overview authority on A — holding neither
// of B's labels. Content was not exposed; identity, structure and the label
// expression were.
//
// These labels are enforced nowhere else, which is not what this test
// originally said — it claimed the scan filtered them and that an EvidenceRef
// merely escaped that. Free-form labels never become cell visibility
// (#570), so the scan filters nothing. An EvidenceRef is
// different — it was recorded by the action's principal and is stored as a
// field of the record — so it needed a check of its own.
//
// Holding and lacking the label are decided by the real evaluator over
// structured labels in TestADispatchHolderSeesTheStoredEvidence
// (dispatch_label_evaluator_test.go); this test keeps the two directions no
// decision can answer: no evaluator, and the question failing.
func TestADispatchReaderSeesOnlyEvidenceItsLabelsCover(t *testing.T) {
	labelled, open := structuredEvidence(t)

	for _, probe := range []struct {
		name    string
		nodes   func(*executorClaimFixture) evidencelabels.NodeGate
		want    []shoal.ID
		wantErr bool
	}{
		{
			// No host gate at all. Nothing can evaluate the nodes' rules, so
			// nothing naming a node may be shown — the direction
			// Attestations takes when nil.
			name: "no node gate is wired", want: []shoal.ID{},
			nodes: func(*executorClaimFixture) evidencelabels.NodeGate { return nil },
		},
		{
			// A failing question is not a false answer. Returning the error
			// keeps a transient fault from reading as a redaction, which
			// would be indistinguishable from a permanent one.
			name: "the question fails",
			nodes: func(f *executorClaimFixture) evidencelabels.NodeGate {
				gate := fixtureCatalog(t, f.authority.Resolver(), f.now)
				gate.err = shoal.NewError(shoal.ErrorUnavailable, "label store down")
				return gate
			},
			wantErr: true,
		},
	} {
		t.Run(probe.name, func(t *testing.T) {
			fixture := newExecutorClaimFixture(t)
			fixture.service.evidenceNodes = probe.nodes(fixture)
			stored := fixture.dispatchStore.records[string(fixture.queued.ID)]
			stored.Evidence = []EvidenceRef{labelled, open}
			fixture.dispatchStore.records[string(fixture.queued.ID)] = stored

			record, err := fixture.service.Status(
				fixture.enqueuer, StatusRequest{
					ID:      fixture.queued.ID,
					Context: dispatchContext(fixture.now, "request"),
				})
			if probe.wantErr {
				if err == nil {
					t.Fatal("a failing visibility question was answered as " +
						"a redaction, so a transient fault is " +
						"indistinguishable from a permanent refusal")
				}
				return
			}
			if err != nil {
				t.Fatalf("Status = %v", err)
			}
			assertAnchors(t, "Status", record.Evidence, probe.want)

			// TeamActions has its own operation, so the enqueuer's token
			// cannot read it — the overview is a different authority, which
			// is also why it is the widest audience for this evidence.
			overseer := bindDecision(t, fixture.authority, dispatchDecision(
				t, "owner", "actor", "request",
				auth.OperationTeamOverviewRead))
			page, err := fixture.service.TeamActions(
				overseer, TeamActionListRequest{
					Limit:     10,
					SourceIDs: [][]byte{[]byte("source")},
					PolicyIDs: [][]byte{[]byte("policy")},
					Context:   dispatchContext(fixture.now, "request"),
				})
			if err != nil {
				t.Fatalf("TeamActions = %v", err)
			}
			if len(page.Actions) != 1 {
				t.Fatalf("TeamActions returned %d actions, want 1: this "+
					"probe cannot check what it does not receive",
					len(page.Actions))
			}
			assertAnchors(t, "TeamActions", page.Actions[0].Evidence, probe.want)

			pulled, err := fixture.service.Pull(
				fixture.enqueuer, PullActionsRequest{
					Limit: 10, Context: dispatchContext(fixture.now, "request"),
				})
			if err != nil {
				t.Fatalf("Pull = %v", err)
			}
			if len(pulled.Actions) != 1 {
				t.Fatalf("Pull returned %d actions, want 1", len(pulled.Actions))
			}
			assertAnchors(t, "Pull", pulled.Actions[0].Evidence, probe.want)
		})
	}

	// The stored record keeps everything. The read path redacts; the record
	// stays complete, which is #369's own non-goal about not changing what an
	// executor records.
	t.Run("the durable record is unchanged", func(t *testing.T) {
		fixture := newExecutorClaimFixture(t)
		fixture.service.evidenceVisibility = &stubEvidenceVisibility{}
		stored := fixture.dispatchStore.records[string(fixture.queued.ID)]
		stored.Evidence = []EvidenceRef{labelled, open}
		fixture.dispatchStore.records[string(fixture.queued.ID)] = stored
		if _, err := fixture.service.Status(
			fixture.enqueuer, StatusRequest{
				ID:      fixture.queued.ID,
				Context: dispatchContext(fixture.now, "request"),
			}); err != nil {
			t.Fatal(err)
		}
		after := fixture.dispatchStore.records[string(fixture.queued.ID)]
		if len(after.Evidence) != 2 {
			t.Fatalf("the read redacted the stored record: %d references "+
				"remain", len(after.Evidence))
		}
	})
}

// assertAnchors compares the evidence a path returned, by anchor and in
// order, and reports the whole list on a mismatch so a redaction that drops
// the wrong reference is legible.
func assertAnchors(
	t *testing.T, path string, got []EvidenceRef, want []shoal.ID,
) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("%s returned %d evidence references, want %d: %#v",
			path, len(got), len(want), got)
	}
	for index := range want {
		if got[index].AnchorID != want[index] {
			t.Fatalf("%s evidence[%d] = %q, want %q",
				path, index, got[index].AnchorID, want[index])
		}
	}
}
