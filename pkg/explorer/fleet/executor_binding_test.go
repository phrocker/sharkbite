// Licensed to the Apache Software Foundation (ASF) under one or more
// contributor license agreements. See the NOTICE file distributed with this
// work for additional information regarding copyright ownership.
package fleet

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/phrocker/shoal-oss/pkg/executorref"
	"github.com/phrocker/shoal-oss/pkg/explorer/auth"
	"github.com/phrocker/shoal-oss/pkg/interaction"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

// executorDecisionFor mints the decision a worker holds since #391: the
// action-execution service role, execute only, bound to one executor ref and
// acting as itself. An execute decision without a binding claims nothing, so
// every test that means "a worker" builds one of these.
func executorDecisionFor(
	t *testing.T, who principal, binding string,
) auth.Decision {
	t.Helper()
	decision, err := auth.NewDecision(auth.DecisionConfig{
		Subject: shoal.ID(who.subject), Actor: shoal.ID(who.actor),
		ClientID:            who.clientID,
		AuthorizationDomain: []byte("domain"),
		AllowedOperations:   []auth.Operation{auth.OperationExecute},
		PermittedSourceIDs:  [][]byte{[]byte("source")},
		PermittedPolicyIDs:  [][]byte{[]byte("policy")}, PolicyGeneration: 1,
		AuthenticationExpires: time.Date(2026, 9, 7, 0, 0, 0, 0, time.UTC),
		RequestID:             shoal.ID(who.request), CorrelationID: "correlation",
		ServiceRole:            auth.ServiceRoleActionExecution,
		ServiceCeilingIdentity: "executor-ceiling",
		ExecutorBinding:        binding,
	})
	if err != nil {
		t.Fatal(err)
	}
	return decision
}

// namedExecutor is namedWorker for a worker bound to the fixture's executor
// ref, which is what a worker is since #391.
func (f *executorClaimFixture) namedExecutor(
	t *testing.T, name string,
) context.Context {
	t.Helper()
	return f.boundExecutor(t, name, "exec")
}

// boundExecutor is a worker of this name bound to an arbitrary ref, so a test
// can hold one principal under two bindings.
func (f *executorClaimFixture) boundExecutor(
	t *testing.T, name, binding string,
) context.Context {
	t.Helper()
	return bindDecision(t, f.authority, executorDecisionFor(t, principal{
		subject: name + "-subject", actor: name + "-actor",
		request: name + "-request",
	}, binding))
}

// executorWorker is worker for a bound executor.
func (f *executorClaimFixture) executorWorker(t *testing.T) context.Context {
	t.Helper()
	return bindDecision(t, f.authority, executorDecisionFor(t, principal{
		subject: "gateway-subject", actor: "gateway-actor",
		request: "gateway-request",
	}, "exec"))
}

// delegationChainOf builds a chain of entries identities of size bytes each.
func delegationChainOf(entries, size int) []shoal.ID {
	chain := make([]shoal.ID, 0, entries)
	for index := 0; index < entries; index++ {
		chain = append(chain, shoal.ID(
			strings.Repeat(string(rune('a'+index)), size)))
	}
	return chain
}

// delegatedQueued enqueues a second action as a principal acting on behalf of
// chain, with invoke, dispatch and delegate, and returns that principal's
// context and the record. It claims on the invoke route, which is the only
// route a delegated claimant has since #391.
func (f *executorClaimFixture) delegatedQueued(
	t *testing.T, name string, chain []shoal.ID,
) (context.Context, ActionRecord) {
	t.Helper()
	ctx := bindDecision(t, f.authority, dispatchDecisionFor(t, principal{
		subject: name + "-subject", actor: name + "-actor",
		request: name + "-request", onBehalfOf: chain,
	}, auth.OperationDispatch, auth.OperationInvoke, auth.OperationDelegate))
	request := dispatchEnqueue(f.now, name+"-request")
	request.ID = []byte(name + "-action")
	request.IdempotencyKey = []byte(name + "-idempotency")
	queued, err := f.service.Enqueue(ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	return ctx, queued
}

// TestAMaximalHolderHistoryWithMaximalRefsStillReclaims pins the raise of
// MaxClaimHolderChainBytes by executorref.MaxExecutorRefBytes (#391).
//
// ClaimHolder gained the executor ref its claim was taken under, and the
// holder's byte bound counts it. A claimant at the old chain bound, under a
// maximum-length ref, would otherwise claim and then make its successor's
// record one ActionRecord.Validate refuses — the brick the bound was added
// to prevent, reopened by upgrading. So here every holder in a full history
// carries a chain at the old maximum and a maximum-length ref, and the
// action is still re-claimed past the history bound, every record still
// valid. Reverting the raise refuses the very first claim.
func TestAMaximalHolderHistoryWithMaximalRefsStillReclaims(t *testing.T) {
	fixture := newExecutorClaimFixture(t)
	maximalRef := strings.Repeat("r", executorref.MaxExecutorRefBytes)
	fixture.executors[maximalRef] = &remoteBoundExecutor{}
	stored := fixture.registryStore.records["agent"]
	stored.Descriptor.ExecutorRef = maximalRef
	fixture.registryStore.records["agent"] = stored

	// The old bound, exactly: 4096 bytes of chain.
	chain := delegationChainOf(4, 1024)
	delegated, queued := fixture.delegatedQueued(t, "maximal", chain)
	version := queued.Version
	for index := 0; index <= MaxActionClaimHistory+1; index++ {
		claimed, err := fixture.service.Claim(delegated, ClaimRequest{
			ID: queued.ID, ExpectedVersion: version,
			ClaimID: []byte(fmt.Sprintf("claim-%d", index)),
			Lease:   time.Nanosecond,
			Context: dispatchContext(fixture.now, "maximal-request"),
		})
		if err != nil {
			t.Fatalf("claim %d of a maximal holder under a maximal ref was "+
				"refused, so the holder bound does not cover the ref it now "+
				"carries: %v", index, err)
		}
		// The memory store does not encode, so the check encodeAction makes
		// is made here.
		if err := claimed.Validate(); err != nil {
			t.Fatalf("claim %d produced a record the durable store refuses: %v",
				index, err)
		}
		if claimed.ClaimExecutorRef != maximalRef {
			t.Fatalf("claim %d recorded ref of %d bytes", index,
				len(claimed.ClaimExecutorRef))
		}
		version = claimed.Version
		fixture.advance(t, time.Second)
	}
	current := fixture.dispatchStore.records[string(queued.ID)]
	if len(current.ClaimHistory) != MaxActionClaimHistory {
		t.Fatalf("history = %d, want a full %d", len(current.ClaimHistory),
			MaxActionClaimHistory)
	}
	for _, holder := range current.ClaimHistory {
		if holder.ExecutorRef != maximalRef ||
			len(holder.OnBehalfOf) != len(chain) {
			t.Fatalf("a retained holder lost its ref or chain: %d bytes, %d "+
				"entries", len(holder.ExecutorRef), len(holder.OnBehalfOf))
		}
	}
}

// rebind points the fixture's descriptor at ref, as a registrar re-registering
// it under another executor would, and makes ref resolvable by the host.
func (f *executorClaimFixture) rebind(t *testing.T, ref string) {
	t.Helper()
	if _, ok := f.executors[ref]; !ok {
		f.executors[ref] = &remoteBoundExecutor{}
	}
	stored := f.registryStore.records["agent"]
	stored.Descriptor.ExecutorRef = ref
	f.registryStore.records["agent"] = stored
}

// The calls below take the caller's name, which is also its request ID's
// prefix, so each one runs under the request its decision was minted for.

func (f *executorClaimFixture) pullAs(
	t *testing.T, who context.Context, name string,
) []ActionRecord {
	t.Helper()
	page, err := f.service.Pull(who, PullActionsRequest{
		Limit: 10, Context: dispatchContext(f.now, name+"-request"),
	})
	if err != nil {
		t.Fatalf("%s could not pull: %v", name, err)
	}
	return page.Actions
}

func (f *executorClaimFixture) claimAs(
	who context.Context, name string, id []byte, version uint64,
	lease time.Duration,
) (ActionRecord, error) {
	return f.service.Claim(who, ClaimRequest{
		ID: id, ExpectedVersion: version, ClaimID: []byte(name + "-claim"),
		Lease: lease, Context: dispatchContext(f.now, name+"-request"),
	})
}

func (f *executorClaimFixture) extendAs(
	who context.Context, name string, claimed ActionRecord,
) (ActionRecord, error) {
	current := f.dispatchStore.records[string(claimed.ID)]
	return f.service.ExtendClaim(who, ExtendRequest{
		ID: claimed.ID, ExpectedVersion: current.Version,
		ClaimID: claimed.ClaimID, Lease: 2 * time.Minute,
		Context: dispatchContext(f.now, name+"-request"),
	})
}

func (f *executorClaimFixture) completeAs(
	who context.Context, name string, claimed ActionRecord,
) (ActionRecord, error) {
	return f.service.CompleteClaim(who, CompletionRequest{
		ID: claimed.ID, ExpectedVersion: claimed.Version,
		ClaimFence: claimed.ClaimFence, ClaimID: claimed.ClaimID,
		Result:  ExecutionResult{Output: []byte(`{"ok":true}`)},
		Context: dispatchContext(f.now, name+"-request"),
	})
}

func (f *executorClaimFixture) reportAs(
	who context.Context, name string, id []byte, fence uint64,
) (ActionRecord, error) {
	return f.service.ReportAmbiguity(who, AmbiguityRequest{
		ID: id, ClaimFence: fence, Outcome: AmbiguityOutcomeUnknown,
		Target:  name + ".example.test",
		Context: dispatchContext(f.now, name+"-request"),
	})
}

func requireNotFound(t *testing.T, what string, err error) {
	t.Helper()
	if !shoal.IsErrorCode(err, shoal.ErrorNotFound) {
		t.Fatalf("%s = %v, want the not-found an absent action gets", what, err)
	}
}

// TestExecutorBindingPermitsEachPhase is the rule, row by row, so each row
// can be mutated on its own (#391).
func TestExecutorBindingPermitsEachPhase(t *testing.T) {
	for _, row := range []struct {
		name                      string
		phase                     executorPhase
		binding, current, claimed string
		want                      bool
	}{
		{"pull: the current ref", executorPhasePull, "x", "x", "", true},
		{"pull: another current ref", executorPhasePull, "x", "y", "x", false},
		{"claim: the current ref", executorPhaseClaim, "x", "x", "y", true},
		{"claim: another current ref", executorPhaseClaim, "x", "y", "x", false},
		{"extend: current and claimed", executorPhaseExtend, "x", "x", "x", true},
		{"extend: rebound since the claim", executorPhaseExtend, "x", "y", "x", false},
		{"extend: claimed under another", executorPhaseExtend, "x", "x", "y", false},
		{"extend: a legacy claim", executorPhaseExtend, "x", "x", "", false},
		{"complete: claimed, then rebound", executorPhaseComplete, "x", "y", "x", true},
		{"complete: claimed under another", executorPhaseComplete, "x", "x", "y", false},
		{"complete: a legacy claim", executorPhaseComplete, "x", "y", "", true},
		{"ambiguity: claimed, then rebound", executorPhaseAmbiguity, "x", "y", "x", true},
		{"ambiguity: claimed under another", executorPhaseAmbiguity, "x", "x", "y", false},
		{"ambiguity: a legacy claim", executorPhaseAmbiguity, "x", "y", "", true},
		{"no phase", executorPhaseNone, "x", "x", "x", false},
	} {
		if got := executorBindingPermits(
			row.phase, row.binding, row.current, row.claimed); got != row.want {
			t.Errorf("%s: permits = %v, want %v", row.name, got, row.want)
		}
	}
	// An empty binding claims nothing, in every phase and against every ref,
	// including the empty one a legacy claim carries.
	for _, phase := range []executorPhase{
		executorPhasePull, executorPhaseClaim, executorPhaseExtend,
		executorPhaseComplete, executorPhaseAmbiguity,
	} {
		for _, refs := range [][2]string{{"x", "x"}, {"", ""}, {"x", ""}} {
			if executorBindingPermits(phase, "", refs[0], refs[1]) {
				t.Errorf("phase %d: an empty binding was permitted against "+
					"%q/%q, so it means any rather than none", phase,
					refs[0], refs[1])
			}
		}
	}
}

// TestAnUnboundExecuteHolderClaimsNothing is the service half of the empty
// binding row: a decision holding execute and no binding — what every worker
// was before #391 — sees no work and claims none.
func TestAnUnboundExecuteHolderClaimsNothing(t *testing.T) {
	fixture := newExecutorClaimFixture(t)
	unbound := fixture.namedWorker(t, "unbound", auth.OperationExecute)
	if got := fixture.pullAs(t, unbound, "unbound"); len(got) != 0 {
		t.Fatalf("an unbound execute-holder pulled %d actions", len(got))
	}
	_, err := fixture.claimAs(unbound, "unbound",
		fixture.queued.ID, fixture.queued.Version, time.Minute)
	requireNotFound(t, "an unbound claim", err)
	// The control: the same principal bound to the descriptor's ref takes it.
	bound := fixture.namedExecutor(t, "unbound")
	if got := fixture.pullAs(t, bound, "unbound"); len(got) != 1 {
		t.Fatalf("a bound worker pulled %d actions, want 1", len(got))
	}
}

// TestADelegatedExecuteHolderIsRefusedAtPullAndClaim: a worker acts as itself
// (#391). A delegated execute decision cannot carry a binding, so the empty
// binding refuses it too; the fleet also refuses the chain itself, so the
// rule does not rest on auth's validation alone.
func TestADelegatedExecuteHolderIsRefusedAtPullAndClaim(t *testing.T) {
	fixture := newExecutorClaimFixture(t)
	delegated := bindDecision(t, fixture.authority, dispatchDecisionFor(t,
		principal{
			subject: "delegated-subject", actor: "delegated-actor",
			request:    "delegated-request",
			onBehalfOf: []shoal.ID{"delegator"},
		}, auth.OperationExecute, auth.OperationDelegate))
	if got := fixture.pullAs(t, delegated, "delegated"); len(got) != 0 {
		t.Fatalf("a delegated execute-holder pulled %d actions", len(got))
	}
	_, err := fixture.claimAs(delegated, "delegated",
		fixture.queued.ID, fixture.queued.Version, time.Minute)
	requireNotFound(t, "a delegated claim", err)
}

// TestARebindAfterAClaimLetsTheHolderReportButNotRenew: a worker claims
// under X and the descriptor is then rebound to Y. Completion and ambiguity
// are judged against X, so the effect that happened is reportable; the
// extension needs X now as well, so the claim runs to its lease end.
func TestARebindAfterAClaimLetsTheHolderReportButNotRenew(t *testing.T) {
	fixture := newExecutorClaimFixture(t)
	x := fixture.namedExecutor(t, "x")
	claimed, err := fixture.claimAs(x, "x",
		fixture.queued.ID, fixture.queued.Version, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if claimed.ClaimExecutorRef != "exec" {
		t.Fatalf("ClaimExecutorRef = %q, want the descriptor's ref",
			claimed.ClaimExecutorRef)
	}
	fixture.rebind(t, "exec-y")
	// The same principal holding a credential bound to the new ref: it
	// matches the claimant on every identity field, so only the claimed-ref
	// half of the rule can refuse it.
	xAsY := fixture.boundExecutor(t, "x", "exec-y")

	_, err = fixture.extendAs(x, "x", claimed)
	requireNotFound(t, "extending a rebound claim", err)
	_, err = fixture.extendAs(xAsY, "x", claimed)
	requireNotFound(t, "extending a claim taken under another ref", err)
	_, err = fixture.reportAs(xAsY, "x", claimed.ID, claimed.ClaimFence)
	requireNotFound(t, "a report on a claim taken under another ref", err)
	_, err = fixture.completeAs(xAsY, "x", claimed)
	requireNotFound(t, "completing a claim taken under another ref", err)

	reported, err := fixture.reportAs(x, "x", claimed.ID, claimed.ClaimFence)
	if err != nil {
		t.Fatalf("the holder could not report an ambiguity after a rebind: %v",
			err)
	}
	if len(reported.AmbiguityReports) != 1 {
		t.Fatalf("reports = %d, want 1", len(reported.AmbiguityReports))
	}
	done, err := fixture.completeAs(x, "x", claimed)
	if err != nil {
		t.Fatalf("the holder could not complete after a rebind: %v", err)
	}
	if done.State != DispatchSucceeded {
		t.Fatalf("state = %v, want succeeded", done.State)
	}
}

// TestARebindMovesLapsedWorkToTheNewRef: after a rebind the lapsed claim is
// work only the new ref's worker may take, and the displaced holder can still
// report what it attempted, judged against the ref its retained holder entry
// carries.
func TestARebindMovesLapsedWorkToTheNewRef(t *testing.T) {
	fixture := newExecutorClaimFixture(t)
	x := fixture.namedExecutor(t, "x")
	first, err := fixture.claimAs(x, "x",
		fixture.queued.ID, fixture.queued.Version, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	fixture.advance(t, 2*time.Second)
	fixture.rebind(t, "exec-y")

	if got := fixture.pullAs(t, x, "x"); len(got) != 0 {
		t.Fatalf("the old ref's worker pulled %d actions after a rebind",
			len(got))
	}
	_, err = fixture.claimAs(x, "x", first.ID, first.Version, time.Minute)
	requireNotFound(t, "a re-claim by the old ref's worker", err)

	y := fixture.boundExecutor(t, "y", "exec-y")
	if got := fixture.pullAs(t, y, "y"); len(got) != 1 {
		t.Fatalf("the new ref's worker pulled %d actions, want 1", len(got))
	}
	second, err := fixture.claimAs(y, "y", first.ID, first.Version, time.Minute)
	if err != nil {
		t.Fatalf("the new ref's worker could not claim: %v", err)
	}
	if second.ClaimExecutorRef != "exec-y" {
		t.Fatalf("ClaimExecutorRef = %q, want exec-y", second.ClaimExecutorRef)
	}
	if len(second.ClaimHistory) != 1 ||
		second.ClaimHistory[0].ExecutorRef != "exec" {
		t.Fatalf("the displaced holder's ref was not retained: %+v",
			second.ClaimHistory)
	}

	if _, err := fixture.reportAs(x, "x", first.ID, first.ClaimFence); err != nil {
		t.Fatalf("the displaced holder could not report: %v", err)
	}
	// The displaced holder's principal under a credential for the new ref
	// matches the retained holder on every identity field. Only the ref the
	// retained entry carries refuses it.
	xAsY := fixture.boundExecutor(t, "x", "exec-y")
	_, err = fixture.reportAs(xAsY, "x", first.ID, first.ClaimFence)
	requireNotFound(t, "a report on a displaced claim taken under another ref",
		err)
}

// TestARebindBeforeAClaimHidesTheWork: rebound before anyone claimed, the old
// ref's worker sees nothing and is told what an absent action gets.
func TestARebindBeforeAClaimHidesTheWork(t *testing.T) {
	fixture := newExecutorClaimFixture(t)
	fixture.rebind(t, "exec-y")
	x := fixture.namedExecutor(t, "x")
	if got := fixture.pullAs(t, x, "x"); len(got) != 0 {
		t.Fatalf("pulled %d actions for a ref the descriptor left", len(got))
	}
	_, err := fixture.claimAs(x, "x",
		fixture.queued.ID, fixture.queued.Version, time.Minute)
	requireNotFound(t, "a claim after a rebind", err)
	y := fixture.boundExecutor(t, "y", "exec-y")
	if got := fixture.pullAs(t, y, "y"); len(got) != 1 {
		t.Fatalf("the new ref's worker pulled %d actions, want 1", len(got))
	}
}

// TestARebindAndBackLeavesTheClaimWhole: X, Y, X while claimed is the claim's
// own ref again, so both renewing and completing are allowed.
func TestARebindAndBackLeavesTheClaimWhole(t *testing.T) {
	fixture := newExecutorClaimFixture(t)
	x := fixture.namedExecutor(t, "x")
	claimed, err := fixture.claimAs(x, "x",
		fixture.queued.ID, fixture.queued.Version, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	fixture.rebind(t, "exec-y")
	fixture.rebind(t, "exec")
	extended, err := fixture.extendAs(x, "x", claimed)
	if err != nil {
		t.Fatalf("extending after X->Y->X: %v", err)
	}
	if _, err := fixture.completeAs(x, "x", extended); err != nil {
		t.Fatalf("completing after X->Y->X: %v", err)
	}
}

// TestALapsedDescriptorLeaseDoesNotStrandAClaim: the worker never heartbeats
// and cannot keep its descriptor live, so a claim taken while the descriptor
// was live is still reported after its lease lapses. New work is refused.
func TestALapsedDescriptorLeaseDoesNotStrandAClaim(t *testing.T) {
	fixture := newExecutorClaimFixture(t)
	stored := fixture.registryStore.records["agent"]
	stored.Descriptor.LeaseExpiresAt = fixture.now.Add(30 * time.Second)
	fixture.registryStore.records["agent"] = stored
	// A second action, queued while the descriptor is live, for the "new
	// claims are refused" half.
	request := dispatchEnqueue(fixture.now, "request")
	request.ID, request.IdempotencyKey = []byte("second"), []byte("second-key")
	queued, err := fixture.service.Enqueue(fixture.enqueuer, request)
	if err != nil {
		t.Fatal(err)
	}

	x := fixture.namedExecutor(t, "x")
	claimed, err := fixture.claimAs(x, "x",
		fixture.queued.ID, fixture.queued.Version, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	fixture.advance(t, 45*time.Second)

	if got := fixture.pullAs(t, x, "x"); len(got) != 0 {
		t.Fatalf("pulled %d actions from a lapsed descriptor", len(got))
	}
	_, err = fixture.claimAs(x, "x", queued.ID, queued.Version, time.Minute)
	requireNotFound(t, "a new claim on a lapsed descriptor", err)
	_, err = fixture.extendAs(x, "x", claimed)
	requireNotFound(t, "extending on a lapsed descriptor", err)

	if _, err := fixture.reportAs(x, "x", claimed.ID, claimed.ClaimFence); err != nil {
		t.Fatalf("a lapsed descriptor stranded an ambiguity report: %v", err)
	}
	current := fixture.dispatchStore.records[string(claimed.ID)]
	done, err := fixture.completeAs(x, "x", current)
	if err != nil {
		t.Fatalf("a lapsed descriptor stranded a completion: %v", err)
	}
	if done.State != DispatchSucceeded {
		t.Fatalf("state = %v, want succeeded", done.State)
	}
}

// TestARevokedDescriptorStillRefusesCompletion bounds the lapse tolerance: it
// is for a lease nobody renewed, not for a descriptor an operator revoked.
func TestARevokedDescriptorStillRefusesCompletion(t *testing.T) {
	fixture := newExecutorClaimFixture(t)
	x := fixture.namedExecutor(t, "x")
	claimed, err := fixture.claimAs(x, "x",
		fixture.queued.ID, fixture.queued.Version, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	stored := fixture.registryStore.records["agent"]
	stored.Descriptor.RevokedAt = fixture.now
	fixture.registryStore.records["agent"] = stored
	_, err = fixture.completeAs(x, "x", claimed)
	requireNotFound(t, "completing on a revoked descriptor", err)
}

// TestALegacyClaimCompletesAndReportsButDoesNotRenew: a claim taken before
// ClaimExecutorRef existed carries none. Completion and ambiguity accept it,
// so an upgrade never strands a claim in flight; extension refuses it.
func TestALegacyClaimCompletesAndReportsButDoesNotRenew(t *testing.T) {
	fixture := newExecutorClaimFixture(t)
	x := fixture.namedExecutor(t, "x")
	claimed, err := fixture.claimAs(x, "x",
		fixture.queued.ID, fixture.queued.Version, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	// As a pre-upgrade build wrote it, and rebound since, so the current ref
	// does not rescue it either.
	legacy := fixture.dispatchStore.records[string(claimed.ID)]
	legacy.ClaimExecutorRef = ""
	fixture.dispatchStore.records[string(claimed.ID)] = legacy
	fixture.rebind(t, "exec-y")

	_, err = fixture.extendAs(x, "x", legacy)
	requireNotFound(t, "extending a legacy claim", err)
	if _, err := fixture.reportAs(x, "x", legacy.ID, legacy.ClaimFence); err != nil {
		t.Fatalf("a legacy claim could not report an ambiguity: %v", err)
	}
	current := fixture.dispatchStore.records[string(claimed.ID)]
	if _, err := fixture.completeAs(x, "x", current); err != nil {
		t.Fatalf("a legacy claim could not complete: %v", err)
	}
}

// TestAGatewayScopedNarrowerThanItsWorkspaceStillWorks is #561's probe for
// the executor credential: a remote worker's descriptor scoped narrower than
// what its credential may retrieve still pulls, claims and completes. #561's
// confinement refuses only in-process execution; the worker is bound to a
// ref, not to sources.
func TestAGatewayScopedNarrowerThanItsWorkspaceStillWorks(t *testing.T) {
	fixture := newExecutorClaimFixture(t)
	wide, err := auth.NewDecision(auth.DecisionConfig{
		Subject: "wide-subject", Actor: "wide-actor",
		AuthorizationDomain: []byte("domain"),
		AllowedOperations:   []auth.Operation{auth.OperationExecute},
		PermittedSourceIDs: [][]byte{
			[]byte("source"), []byte("other-source"),
		},
		PermittedPolicyIDs: [][]byte{
			[]byte("policy"), []byte("other-policy"),
		},
		PolicyGeneration:      1,
		AuthenticationExpires: time.Date(2026, 9, 7, 0, 0, 0, 0, time.UTC),
		RequestID:             "wide-request", CorrelationID: "correlation",
		ServiceRole:            auth.ServiceRoleActionExecution,
		ServiceCeilingIdentity: "executor-ceiling",
		ExecutorBinding:        "exec",
	})
	if err != nil {
		t.Fatal(err)
	}
	worker := bindDecision(t, fixture.authority, wide)
	if got := fixture.pullAs(t, worker, "wide"); len(got) != 1 {
		t.Fatalf("pulled %d actions, want 1", len(got))
	}
	claimed, err := fixture.claimAs(worker, "wide",
		fixture.queued.ID, fixture.queued.Version, time.Minute)
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if _, err := fixture.completeAs(worker, "wide", claimed); err != nil {
		t.Fatalf("complete: %v", err)
	}
}

// TestAWorkerBoundElsewhereIsToldWhatAnAbsentActionIsTold: the binding is a
// standing check, so a caller bound to another ref is refused byte for byte
// as a probe for a nonexistent ID is, on every claimant route, and never
// reaches the attestation store.
func TestAWorkerBoundElsewhereIsToldWhatAnAbsentActionIsTold(t *testing.T) {
	f := newAttestationFixture(t, true)
	elsewhere := func(name string) (context.Context, RequestContext) {
		request := name + "-elsewhere"
		return bindDecision(t, f.authority, executorDecisionFor(t, principal{
			subject: name, actor: name + "-actor", request: request,
			clientID: shoal.ID(name + "-client"),
		}, "other-exec")), dispatchContext(f.now(), request)
	}
	compare := func(route string, probe func(id []byte) error) {
		t.Helper()
		calls := f.attestations.callCount()
		refused, absent := probe(f.queued.ID), probe([]byte("no-such-action"))
		if refused == nil || absent == nil {
			t.Fatalf("%s: not refused: %v / %v", route, refused, absent)
		}
		if refused.Error() != absent.Error() {
			t.Fatalf("%s: refused %q but absent %q: the binding is an "+
				"existence oracle", route, refused, absent)
		}
		requireNotFound(t, route, refused)
		if f.attestations.callCount() != calls {
			t.Fatalf("%s: the attestation store was read for a caller bound "+
				"to another ref", route)
		}
	}

	// Unclaimed: the claim gate is where the attestation store would be read.
	beta, betaRequest := elsewhere("beta")
	compare("claim", func(id []byte) error {
		_, err := f.service.Claim(beta, ClaimRequest{
			ID: id, ExpectedVersion: f.queued.Version,
			ClaimID: []byte("beta-claim"), Lease: time.Minute,
			Context: betaRequest,
		})
		return err
	})

	// Claimed by alpha under the right ref; alpha's own principal under a
	// credential for another ref is refused on every other route.
	f.attest("alpha", f.now().Add(time.Hour))
	claimed, err := f.claim(t, "alpha", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	alpha, alphaRequest := elsewhere("alpha")
	compare("extend", func(id []byte) error {
		_, err := f.service.ExtendClaim(alpha, ExtendRequest{
			ID: id, ExpectedVersion: claimed.Version,
			ClaimID: claimed.ClaimID, Lease: 2 * time.Minute,
			Context: alphaRequest,
		})
		return err
	})
	compare("complete", func(id []byte) error {
		_, err := f.service.CompleteClaim(alpha, CompletionRequest{
			ID: id, ExpectedVersion: claimed.Version,
			ClaimFence: claimed.ClaimFence, ClaimID: claimed.ClaimID,
			Result:  ExecutionResult{Output: []byte(`{"ok":true}`)},
			Context: alphaRequest,
		})
		return err
	})
	compare("ambiguity", func(id []byte) error {
		_, err := f.service.ReportAmbiguity(alpha, AmbiguityRequest{
			ID: id, ClaimFence: claimed.ClaimFence,
			Outcome: AmbiguityOutcomeUnknown, Context: alphaRequest,
		})
		return err
	})
}

// TestAWorkerResolvesOnlyItsOwnDescriptor: the action-execution role may
// resolve, and only the descriptor its binding names (#391). It holds no
// registrar credential: the role cannot heartbeat, which auth refuses at mint.
func TestAWorkerResolvesOnlyItsOwnDescriptor(t *testing.T) {
	fixture := newExecutorClaimFixture(t)
	other := dispatchDescriptor(fixture.now)
	other.ID, other.ExecutorRef = "other-agent", "other-exec"
	other.LeaseExpiresAt = fixture.now.Add(time.Hour)
	fixture.registryStore.records["other-agent"] = Stored{Descriptor: other}
	fixture.executors["other-exec"] = &remoteBoundExecutor{}

	config := auth.DecisionConfig{
		Subject: "resolver-subject", Actor: "resolver-actor",
		AuthorizationDomain: []byte("domain"),
		AllowedOperations: []auth.Operation{
			auth.OperationExecute, auth.OperationAgentResolve,
		},
		PermittedSourceIDs: [][]byte{[]byte("source")},
		PermittedPolicyIDs: [][]byte{[]byte("policy")}, PolicyGeneration: 1,
		AuthenticationExpires: time.Date(2026, 9, 7, 0, 0, 0, 0, time.UTC),
		RequestID:             "resolver-request", CorrelationID: "correlation",
		ServiceRole:            auth.ServiceRoleActionExecution,
		ServiceCeilingIdentity: "executor-ceiling",
		ExecutorBinding:        "exec",
	}
	decision, err := auth.NewDecision(config)
	if err != nil {
		t.Fatal(err)
	}
	worker := bindDecision(t, fixture.authority, decision)
	registry := fixture.service.registry
	resolve := func(id shoal.ID) error {
		_, err := registry.Resolve(worker, ResolveRequest{
			ID: id, Context: dispatchContext(fixture.now, "resolver-request"),
		})
		return err
	}
	if err := resolve("agent"); err != nil {
		t.Fatalf("a worker could not resolve its own descriptor: %v", err)
	}
	refused, absent := resolve("other-agent"), resolve("no-such-agent")
	if refused == nil || absent == nil || refused.Error() != absent.Error() {
		t.Fatalf("another ref's descriptor = %v, absent = %v; want the "+
			"same not-found", refused, absent)
	}
	page, err := registry.List(worker, ListRequest{
		Context: dispatchContext(fixture.now, "resolver-request"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Descriptors) != 1 || page.Descriptors[0].ID != "agent" {
		t.Fatalf("listed %+v, want only the bound descriptor", page.Descriptors)
	}

	config.AllowedOperations = append(
		config.AllowedOperations, auth.OperationAgentHeartbeat)
	if _, err := auth.NewDecision(config); err == nil {
		t.Fatal("an action-execution decision was minted with heartbeat")
	}
}

// TestABoundWorkersPullStillRedactsEvidence: the execute route's Pull reads
// the store through the one page path, scanDispatchActions, so a bound
// worker gets evidence decided exactly as the enqueuer does (#369). #391 adds
// no second read path. A worker is a trusted service whose role requires its
// own svc:<role> term, which no document rule carries, so it sees no
// reference that names a node (#564).
func TestABoundWorkersPullStillRedactsEvidence(t *testing.T) {
	fixture := newExecutorClaimFixture(t)
	fixture.service.evidenceVisibility = &stubEvidenceVisibility{}
	fixture.service.evidenceNodes = fixtureCatalog(
		t, fixture.authority.Resolver(), fixture.now)
	stored := fixture.dispatchStore.records[string(fixture.queued.ID)]
	stored.Evidence = []EvidenceRef{
		{
			AnchorID: "anchor-secret", Kind: interaction.EvidenceDocument,
			NodeIDs: []shoal.ID{"node-secret"}, Visibility: []string{"secret"},
		},
		{
			AnchorID: "anchor-open", Kind: interaction.EvidenceDocument,
			NodeIDs: []shoal.ID{"node-open"},
		},
	}
	fixture.dispatchStore.records[string(fixture.queued.ID)] = stored
	worker := fixture.namedExecutor(t, "worker")
	pulled := fixture.pullAs(t, worker, "worker")
	if len(pulled) != 1 {
		t.Fatalf("pulled %d actions, want 1", len(pulled))
	}
	assertAnchors(t, "Pull (execute route)", pulled[0].Evidence, []shoal.ID{})
}
