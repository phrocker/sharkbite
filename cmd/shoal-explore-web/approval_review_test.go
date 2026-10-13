// Licensed to the Apache Software Foundation (ASF) under one or more
// contributor license agreements. See the NOTICE file distributed with this
// work for additional information regarding copyright ownership.
package main

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/phrocker/shoal-oss/pkg/explorer/auth"
	"github.com/phrocker/shoal-oss/pkg/explorer/fleet"
	"github.com/phrocker/shoal-oss/pkg/explorer/webapi"
	"github.com/phrocker/shoal-oss/pkg/explorer/workspace"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

// Regression tests for the review of #489, against the same composition as
// approval_acceptance_test.go.

// TestApprovalRefusesTheAgentsDelegationChain: a delegated agent acts with
// authority its parent granted, so the parent agent and the parent's
// registrant are parties to the work. Before the fix either could approve a
// request against the child alone and the request materialized.
func TestApprovalRefusesTheAgentsDelegationChain(t *testing.T) {
	t.Parallel()
	h := newApprovalHarness(t)
	// The child is registered by the same subject (delegation requires it)
	// under a different actor, so the parent's registrant actor and the
	// child's are different identities and each must be refused on its own.
	childRegistrant := principal{
		subject: "owner", actor: "operator-2",
		operations: []auth.Operation{
			auth.OperationAgentRegister, auth.OperationDelegate,
		},
	}
	if _, err := h.opened.fleetRegistry.Register(
		h.as(childRegistrant), fleet.RegisterRequest{
			Context:         h.context(h.now().Add(time.Minute)),
			RegistrationKey: "child-registration",
			Spec: fleet.Spec{
				ID: "gateway-child", ParentID: "gateway",
				AuthorizationDomain: workspaceAuthorizationDomain,
				Scopes: []fleet.Scope{{
					SourceID: workspaceSourceID, PolicyID: workspaceGrantPolicyID,
				}},
				ExecutorRef:    "local",
				Capabilities:   approvalActions(true),
				LeaseExpiresAt: h.now().Add(19 * time.Hour),
			},
		}); err != nil {
		t.Fatalf("register the child: %v", err)
	}
	request := h.held("chain-1")
	enqueue := h.enqueue(request)
	enqueue.AgentID = "gateway-child"
	receipt, err := h.opened.approvals.Request(h.as(requester), enqueue)
	if err != nil || receipt.State != fleet.ApprovalPending {
		t.Fatalf("request against the child = %+v, %v", receipt, err)
	}
	approveOnly := []auth.Operation{auth.OperationActionApprove}
	for _, attempt := range []struct {
		name string
		who  principal
	}{
		{"the parent agent", principal{
			subject: "gateway", actor: "gateway-console", operations: approveOnly}},
		{"the parent's registrant actor", principal{
			subject: "zed", actor: "operator", operations: approveOnly}},
		{"the child agent", principal{
			subject: "zed", actor: "gateway-child", operations: approveOnly}},
		{"the child's registrant actor", principal{
			subject: "zed", actor: "operator-2", operations: approveOnly}},
		{"the shared registrant subject", principal{
			subject: "owner", actor: "zed", operations: approveOnly}},
	} {
		_, err := h.decide(attempt.who, receipt, fleet.ApprovalVerdictApprove)
		if !shoal.IsErrorCode(err, shoal.ErrorUnauthorized) ||
			!strings.Contains(err.Error(), "not independent") {
			t.Fatalf("%s: decide = %v, want the independence refusal",
				attempt.name, err)
		}
	}
	if status := h.statusAs(approver, request.id); status.State != fleet.ApprovalPending {
		t.Fatalf("after refused chain approvals the request is %q", status.State)
	}
	// Re-requesting still holds it: nothing became work.
	again, err := h.opened.approvals.Request(h.as(requester), enqueue)
	if err != nil || again.State != fleet.ApprovalPending {
		t.Fatalf("re-request after refused chain approvals = %+v, %v", again, err)
	}
	if _, err := h.decide(approver, receipt, fleet.ApprovalVerdictApprove); err != nil {
		t.Fatalf("an independent approver: %v", err)
	}
}

// TestApprovalRefusesANarrowedApproverOnEveryPath: a principal holding
// approve and dispatch narrows its own decision, through a workspace it owns,
// to approve alone. Every approver path — decide, pending, and status asked
// as an approver — must refuse it, because separation is judged on the
// authority the principal holds, not on what it chose to present.
func TestApprovalRefusesANarrowedApproverOnEveryPath(t *testing.T) {
	t.Parallel()
	h := newApprovalHarness(t)
	request := h.held("narrowed-1")
	receipt := h.mustRequest(requester, request, fleet.ApprovalPending)
	mallory := principal{
		subject: "mallory", actor: "mallory-console",
		operations: []auth.Operation{
			auth.OperationActionApprove, auth.OperationDispatch,
			auth.OperationWorkspaceSettingsWrite,
		},
	}
	// Without narrowing the separation rule refuses her: she may dispatch.
	if _, err := h.decide(mallory, receipt, fleet.ApprovalVerdictApprove); !shoal.IsErrorCode(
		err, shoal.ErrorUnauthorized) ||
		!strings.Contains(err.Error(), "create or perform") {
		t.Fatalf("unnarrowed dispatch-holder = %v", err)
	}
	created, err := h.opened.settings.Update(
		h.as(mallory), "mallory-workspace", workspace.UpdateRequest{
			MutationID: "narrow-to-approve",
			Narrowing: workspace.UpdateNarrowing{
				AllowedOperations: workspace.OperationSelection{
					Present: true,
					Values:  []auth.Operation{auth.OperationActionApprove},
				},
			},
		})
	if err != nil {
		t.Fatalf("create the narrowing workspace: %v", err)
	}
	applier, ok := h.opened.settings.(webapi.WorkspaceSettingsApplier)
	if !ok {
		t.Fatal("the composed settings provider cannot apply settings")
	}
	narrowed, err := webapi.ApplyWorkspaceSettingsForOperation(
		h.as(mallory), applier, h.authority.Binder(), created.WorkspaceID,
		auth.OperationActionApprove, workspace.MaximumLimits(), nil)
	if err != nil {
		t.Fatalf("apply the narrowing: %v", err)
	}
	// The narrowing really did shed dispatch, so without the refusal she
	// would pass the separation rule. Otherwise this test proves nothing.
	decision, err := h.authority.Resolver().Resolve(narrowed)
	if err != nil {
		t.Fatal(err)
	}
	if decision.AuthorizeObject(auth.OperationDispatch, auth.ResourceRequest{
		AuthorizationDomain: workspaceAuthorizationDomain,
		SourceID:            workspaceSourceID, PolicyID: workspaceGrantPolicyID,
		ObjectID: "release-7",
	}, h.now()) == nil {
		t.Fatal("the narrowed decision still holds dispatch")
	}
	contextFor := func() fleet.RequestContext {
		return fleet.RequestContext{
			RequestID: decision.RequestID(), CorrelationID: decision.CorrelationID(),
			ReasonCode: "test", Deadline: h.now().Add(time.Minute),
		}
	}
	isNarrowingRefusal := func(err error) bool {
		return shoal.IsErrorCode(err, shoal.ErrorUnauthorized) &&
			strings.Contains(err.Error(), "workspace settings")
	}
	if _, err := h.opened.approvals.Decide(narrowed, fleet.ApprovalDecisionRequest{
		ID: receipt.ID, RequestDigest: receipt.RequestDigest,
		PolicyGeneration: receipt.PolicyGeneration,
		Verdict:          fleet.ApprovalVerdictApprove, Context: contextFor(),
	}); !isNarrowingRefusal(err) {
		t.Fatalf("decide under narrowing = %v", err)
	}
	if _, err := h.opened.approvals.Pending(narrowed, fleet.PendingApprovalsRequest{
		Limit: fleet.MaxApprovalListResults, Context: contextFor(),
	}); !isNarrowingRefusal(err) {
		t.Fatalf("pending under narrowing = %v", err)
	}
	if _, err := h.opened.approvals.Status(narrowed, fleet.ApprovalStatusRequest{
		ID: receipt.ID, Context: contextFor(),
	}); !isNarrowingRefusal(err) {
		t.Fatalf("status as an approver under narrowing = %v", err)
	}
	if status := h.status(request.id); status.State != fleet.ApprovalPending {
		t.Fatalf("after narrowed attempts the request is %q", status.State)
	}
}

// TestApprovalConcurrentReRequestsAgree: after approval, many re-requests
// race to materialize. Exactly one ActionRecord is created and every caller is
// told so. Before the fix the durable store's first-write conflict was a shoal
// conflict rather than the fleet sentinel, the replay fallback never ran, and
// a third of the callers got "fleet registry conflict" for work that existed.
func TestApprovalConcurrentReRequestsAgree(t *testing.T) {
	// Not parallel (#710): pinned to one core this fails even run alone,
	// which is a defect of its own, and with the package's parallel tests
	// competing for a small runner's cores it would fail as often.
	h := newApprovalHarness(t)
	request := h.held("concurrent-1")
	receipt := h.mustRequest(requester, request, fleet.ApprovalPending)
	if _, err := h.decide(approver, receipt, fleet.ApprovalVerdictApprove); err != nil {
		t.Fatal(err)
	}
	const callers = 16
	contexts := make([]context.Context, callers)
	for i := range contexts {
		contexts[i] = h.as(requester)
	}
	receipts := make([]fleet.ApprovalReceipt, callers)
	errs := make([]error, callers)
	var start, done sync.WaitGroup
	start.Add(1)
	for i := 0; i < callers; i++ {
		done.Add(1)
		go func(i int) {
			defer done.Done()
			start.Wait()
			receipts[i], errs[i] = h.opened.approvals.Request(
				contexts[i], h.enqueue(request))
		}(i)
	}
	start.Done()
	done.Wait()
	for i := 0; i < callers; i++ {
		if errs[i] != nil {
			t.Fatalf("caller %d: %v", i, errs[i])
		}
		if receipts[i].State != fleet.ApprovalEnqueued ||
			receipts[i].Action.Version != 1 ||
			!receipts[i].Action.UpdatedAt.Equal(receipts[0].Action.UpdatedAt) ||
			receipts[i].Action.ApproverSubject != approver.subject {
			t.Fatalf("caller %d = %+v, caller 0 = %+v",
				i, receipts[i], receipts[0])
		}
	}
	page, err := h.opened.fleetDispatch.Pull(h.as(requester), fleet.PullActionsRequest{
		Limit: fleet.MaxDispatchListResults, Context: h.context(h.now().Add(time.Minute)),
	})
	if err != nil {
		t.Fatal(err)
	}
	found := 0
	for _, action := range page.Actions {
		if string(action.ID) == string(request.id) {
			found++
		}
	}
	if found != 1 {
		t.Fatalf("pull found the materialized action %d times", found)
	}
}

// TestApprovalStatusReportsTheEffectiveState: Status reports what a request
// amounts to now, not merely the stored row, and writes nothing doing it.
func TestApprovalStatusReportsTheEffectiveState(t *testing.T) {
	t.Parallel()
	h := newApprovalHarness(t)

	// Window closed: Status writes the expiry it observes (#655), as the
	// party asking, so the stored state is the effective one afterwards.
	lapsed := h.held("status-lapsed")
	h.mustRequest(requester, lapsed, fleet.ApprovalPending)
	approvedLapse := h.held("status-approved-lapse")
	approvedReceipt := h.mustRequest(requester, approvedLapse, fleet.ApprovalPending)
	if _, err := h.decide(approver, approvedReceipt, fleet.ApprovalVerdictApprove); err != nil {
		t.Fatal(err)
	}
	h.advance(fleet.DefaultApprovalWindow + time.Second)
	for _, id := range [][]byte{lapsed.id, approvedLapse.id} {
		status := h.status(id)
		if status.State != fleet.ApprovalExpired ||
			status.Condition != fleet.ApprovalConditionNone ||
			status.Approval.State != fleet.ApprovalExpired {
			t.Fatalf("%s status = %q/%q stored %q", id, status.State,
				status.Condition, status.Approval.State)
		}
	}

	// A policy generation that has moved: the request can never progress,
	// and the requester's Status on the new generation records it as
	// stranded (#655).
	current := h.held("status-policy")
	h.mustRequest(requester, current, fleet.ApprovalPending)
	movedRequester := requester
	movedRequester.generation = workspacePolicyGeneration + 1
	h.reader.value.Store(workspacePolicyGeneration + 1)
	status := h.statusAs(movedRequester, current.id)
	if status.State != fleet.ApprovalStranded ||
		status.Condition != fleet.ApprovalConditionPolicyMoved ||
		status.Approval.State != fleet.ApprovalStranded {
		t.Fatalf("policy-moved status = %q/%q stored %q",
			status.State, status.Condition, status.Approval.State)
	}
	h.reader.value.Store(workspacePolicyGeneration)

	// The target's generation moves: an approved request strands, and a
	// re-request cannot reach it. Status says so instead of "approved", and
	// records it (#655).
	stranded := h.held("status-target")
	strandedReceipt := h.mustRequest(requester, stranded, fleet.ApprovalPending)
	if _, err := h.decide(approver, strandedReceipt, fleet.ApprovalVerdictApprove); err != nil {
		t.Fatal(err)
	}
	if _, err := h.register("gateway", "regenerate", 1, true, ""); err != nil {
		t.Fatalf("move the agent's generation: %v", err)
	}
	status = h.status(stranded.id)
	if status.State != fleet.ApprovalStranded ||
		status.Condition != fleet.ApprovalConditionTargetMoved ||
		status.Approval.State != fleet.ApprovalStranded {
		t.Fatalf("target-moved status = %q/%q stored %q",
			status.State, status.Condition, status.Approval.State)
	}
	if _, err := h.request(requester, stranded); !shoal.IsErrorCode(
		err, shoal.ErrorNotFound) {
		t.Fatalf("re-request after the target moved = %v (documented as "+
			"not found: the request names a generation that no longer resolves)", err)
	}
	if status := h.status(stranded.id); status.Approval.State != fleet.ApprovalStranded ||
		status.Approval.StrandedCause != fleet.ApprovalConditionTargetMoved {
		t.Fatalf("the stranding did not stay recorded: %q/%q",
			status.Approval.State, status.Approval.StrandedCause)
	}
}

// TestApprovalEnqueuedWithoutActionStrandsHonestly: a crash between the two
// writes is recoverable only while the target and the deadline hold. When
// either moves, Status reports the approval as unresolvable rather than as
// live work, and the docs no longer promise recovery "at any later time".
func TestApprovalEnqueuedWithoutActionStrandsHonestly(t *testing.T) {
	t.Parallel()
	armed := &atomic.Bool{}
	h := newCrashHarness(t, armed)

	// Deadline: a request whose action deadline is inside the window.
	short := h.held("strand-deadline")
	short.deadline = h.now().Add(30 * time.Minute)
	shortReceipt := h.mustRequest(requester, short, fleet.ApprovalPending)
	if !shortReceipt.ExpiresAt.Equal(short.deadline) {
		t.Fatalf("window not clamped to the deadline: %v", shortReceipt.ExpiresAt)
	}
	// Target: an ordinary request.
	moved := h.held("strand-target")
	movedReceipt := h.mustRequest(requester, moved, fleet.ApprovalPending)
	for _, receipt := range []fleet.ApprovalReceipt{shortReceipt, movedReceipt} {
		if _, err := h.decide(approver, receipt, fleet.ApprovalVerdictApprove); err != nil {
			t.Fatal(err)
		}
	}
	armed.Store(true)
	for _, request := range []heldRequest{short, moved} {
		if _, err := h.request(requester, request); err == nil {
			t.Fatal("the injected crash did not surface")
		}
		status := h.status(request.id)
		if status.State != fleet.ApprovalEnqueued ||
			status.Condition != fleet.ApprovalConditionAwaitingAction {
			t.Fatalf("%s after crash = %q/%q", request.id, status.State, status.Condition)
		}
	}
	armed.Store(false)

	h.advance(31 * time.Minute)
	status := h.status(short.id)
	if status.State != fleet.ApprovalUnresolvable ||
		status.Condition != fleet.ApprovalConditionDeadlinePassed {
		t.Fatalf("crash then deadline = %q/%q", status.State, status.Condition)
	}
	if _, err := h.request(requester, short); err == nil {
		t.Fatal("a re-request after the deadline succeeded")
	}

	if _, err := h.register("gateway", "regenerate", 1, true, ""); err != nil {
		t.Fatal(err)
	}
	status = h.status(moved.id)
	if status.State != fleet.ApprovalUnresolvable ||
		status.Condition != fleet.ApprovalConditionTargetMoved {
		t.Fatalf("crash then generation move = %q/%q", status.State, status.Condition)
	}
	// Neither stranded approval became work.
	for _, request := range []heldRequest{short, moved} {
		if _, err := h.opened.fleetDispatch.Status(h.as(requester), fleet.StatusRequest{
			ID: request.id, Context: h.context(h.now().Add(time.Minute)),
		}); !shoal.IsErrorCode(err, shoal.ErrorNotFound) {
			t.Fatalf("%s became an action: %v", request.id, err)
		}
	}
}

// TestApprovalRefusesASquattedIdentityBeforeCommitting: an ordinary action
// enqueued at a held request's identity means the approval can never become
// work. Found before the approved → enqueued commit, the approval stays
// approved rather than becoming an enqueued approval naming work that does not
// exist.
func TestApprovalRefusesASquattedIdentityBeforeCommitting(t *testing.T) {
	t.Parallel()
	h := newApprovalHarness(t)
	request := h.held("squatted")
	receipt := h.mustRequest(requester, request, fleet.ApprovalPending)
	if _, err := h.decide(approver, receipt, fleet.ApprovalVerdictApprove); err != nil {
		t.Fatal(err)
	}
	squat := h.enqueue(request)
	squat.Action = "status"
	squatter := principal{subject: "mallory", actor: "mallory",
		operations: []auth.Operation{auth.OperationDispatch}}
	if _, err := h.opened.fleetDispatch.Enqueue(h.as(squatter), squat); err != nil {
		t.Fatalf("squat: %v", err)
	}
	if _, err := h.request(requester, request); !errors.Is(err, fleet.ErrActionConflict) {
		t.Fatalf("re-request at a squatted identity = %v", err)
	}
	if status := h.status(request.id); status.Approval.State != fleet.ApprovalApproved {
		t.Fatalf("squatted approval moved to %q", status.Approval.State)
	}
}

// TestApprovalToleratesClockSkewBetweenReplicas: a decision or a
// materialization on a replica whose clock is behind the one that wrote the
// row clamps its timestamps forward instead of failing validation.
func TestApprovalToleratesClockSkewBetweenReplicas(t *testing.T) {
	t.Parallel()
	h := newApprovalHarness(t)
	request := h.held("skew-1")
	receipt := h.mustRequest(requester, request, fleet.ApprovalPending)
	requestedAt := h.status(request.id).Approval.RequestedAt

	h.advance(-2 * time.Second)
	decided, err := h.decide(approver, receipt, fleet.ApprovalVerdictApprove)
	if err != nil {
		t.Fatalf("decide on a replica 2s behind: %v", err)
	}
	if decided.DecidedAt.Before(requestedAt) {
		t.Fatalf("decided at %v before requested at %v",
			decided.DecidedAt, requestedAt)
	}

	// Materializing on a replica still further behind: the approval commits
	// with ordered timestamps and the action is written, but its UpdatedAt is
	// ahead of this replica's clock, and the lifecycle publisher bounds a
	// publication's retry window from UpdatedAt against the local clock. So
	// the first answer is "committed, reconcile" — the same property every
	// dispatch transition reconciled on a lagging replica already has — and
	// the re-request once the clock has caught up replays the one record.
	h.advance(-time.Second)
	if _, err := h.request(requester, request); err != nil &&
		!errors.Is(err, fleet.ErrActionCommitted) {
		t.Fatalf("materialize on a lagging replica = %v", err)
	}
	h.advance(5 * time.Second)
	materialized := h.mustRequest(requester, request, fleet.ApprovalEnqueued)
	stored := h.status(request.id).Approval
	if stored.MaterializedAt.Before(stored.DecidedAt) ||
		materialized.Action.UpdatedAt.Before(materialized.Action.CreatedAt) {
		t.Fatalf("skewed materialization: approval %+v action %+v",
			stored, materialized.Action)
	}
}

// newCrashHarness is newApprovalHarness with the crash-injecting store of
// TestApprovalCrashBetweenTheTwoWritesRecovers.
func newCrashHarness(t *testing.T, armed *atomic.Bool) *approvalHarness {
	t.Helper()
	h := &approvalHarness{t: t, root: t.TempDir()}
	h.clock.Store(time.Now().UTC().Add(time.Minute).Truncate(time.Second).UnixNano())
	authority, err := auth.NewAuthorityWithClock(h.now)
	if err != nil {
		t.Fatal(err)
	}
	h.authority = authority
	h.reader = &mutableFleetGeneration{}
	h.reader.value.Store(workspacePolicyGeneration)
	h.wrap = func(store fleet.ApprovalStore) fleet.ApprovalStore {
		return crashAfterMaterializeWrite{ApprovalStore: store, armed: armed}
	}
	h.open()
	t.Cleanup(h.close)
	h.register("gateway", "approval-registration", 0, true, "")
	return h
}
