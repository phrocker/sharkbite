// Licensed to the Apache Software Foundation (ASF) under one or more
// contributor license agreements. See the NOTICE file distributed with this
// work for additional information regarding copyright ownership.
package main

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/phrocker/shoal-oss/pkg/explorer/auth"
	"github.com/phrocker/shoal-oss/pkg/explorer/fleet"
	"github.com/phrocker/shoal-oss/pkg/explorer/webapi"
	"github.com/phrocker/shoal-oss/pkg/explorer/workspace"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

// Regression tests for the second review round of #489, against the same
// composition as approval_acceptance_test.go. The only seam is the approval
// store wrapper, which runs a hook before a write reaches the real store.

type hookedApprovalStore struct {
	fleet.ApprovalStore
	before func(fleet.ApprovalMutation) error
}

func (s hookedApprovalStore) ApplyApproval(
	ctx context.Context, mutation fleet.ApprovalMutation,
) (fleet.ApprovalRecord, error) {
	if err := s.before(mutation); err != nil {
		return fleet.ApprovalRecord{}, err
	}
	return s.ApprovalStore.ApplyApproval(ctx, mutation)
}

// newHookedHarness is newApprovalHarness with a hook before every approval
// write. The hook receives the harness so it can act through the real
// services.
func newHookedHarness(
	t *testing.T,
	hook func(*approvalHarness, fleet.ApprovalMutation) error,
) *approvalHarness {
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
		return hookedApprovalStore{ApprovalStore: store, before: func(
			mutation fleet.ApprovalMutation,
		) error {
			return hook(h, mutation)
		}}
	}
	h.open()
	t.Cleanup(h.close)
	h.register("gateway", "approval-registration", 0, true, "")
	return h
}

// TestApprovalStatusNamesATakenIdentity: an ordinary action enqueued at the
// identity between the pre-commit check and the approved → enqueued commit.
// The approval commits enqueued, materialization refuses someone else's work,
// and Status must not call that action this approval's: it reports
// unresolvable with condition identity_taken.
func TestApprovalStatusNamesATakenIdentity(t *testing.T) {
	t.Parallel()
	armed := &atomic.Bool{}
	request := heldRequest{}
	h := newHookedHarness(t, func(
		h *approvalHarness, mutation fleet.ApprovalMutation,
	) error {
		if mutation.Record.State != fleet.ApprovalEnqueued ||
			!armed.CompareAndSwap(true, false) {
			return nil
		}
		squat := h.enqueue(request)
		squat.Action = "status"
		if _, err := h.opened.fleetDispatch.Enqueue(h.as(principal{
			subject: "mallory", actor: "mallory",
			operations: []auth.Operation{auth.OperationDispatch},
		}), squat); err != nil {
			return err
		}
		return nil
	})
	request = h.held("taken-1")
	receipt := h.mustRequest(requester, request, fleet.ApprovalPending)
	if _, err := h.decide(approver, receipt, fleet.ApprovalVerdictApprove); err != nil {
		t.Fatal(err)
	}
	armed.Store(true)
	if _, err := h.request(requester, request); !errors.Is(err, fleet.ErrActionConflict) {
		t.Fatalf("materialize onto a taken identity = %v", err)
	}
	if armed.Load() {
		t.Fatal("the squat hook did not run")
	}
	// The identity can never be this approval's, so the requester's Status
	// records it as stranded (#655).
	status := h.status(request.id)
	if status.Approval.State != fleet.ApprovalStranded ||
		status.State != fleet.ApprovalStranded ||
		status.Condition != fleet.ApprovalConditionIdentityTaken {
		t.Fatalf("status of a taken identity = %q/%q stored %q",
			status.State, status.Condition, status.Approval.State)
	}
}

// TestApprovalNarrowedCallerLearnsNothingAboutExistence: a narrowed caller
// is refused for narrowing only on a record it would otherwise be eligible to
// decide. A missing ID, a request in a scope it does not hold and a request it
// is not independent of must all answer exactly alike, on Status and Decide.
func TestApprovalNarrowedCallerLearnsNothingAboutExistence(t *testing.T) {
	t.Parallel()
	h := newApprovalHarness(t)
	eligible := h.held("narrow-eligible")
	eligibleReceipt := h.mustRequest(requester, eligible, fleet.ApprovalPending)
	otherScope := h.held("narrow-other-scope")
	otherScope.policy = approvalSecondPolicyID
	otherReceipt := h.mustRequest(requester, otherScope, fleet.ApprovalPending)
	own := h.held("narrow-own")
	ownReceipt := h.mustRequest(principal{
		subject: "mallory", actor: "mallory-agent",
		operations: []auth.Operation{auth.OperationDispatch},
	}, own, fleet.ApprovalPending)

	mallory := principal{
		subject: "mallory", actor: "mallory-console",
		operations: []auth.Operation{
			auth.OperationActionApprove, auth.OperationDispatch,
			auth.OperationWorkspaceSettingsWrite,
		},
	}
	created, err := h.opened.settings.Update(
		h.as(mallory), "mallory-narrow", workspace.UpdateRequest{
			MutationID: "narrow",
			Narrowing: workspace.UpdateNarrowing{
				AllowedOperations: workspace.OperationSelection{
					Present: true,
					Values:  []auth.Operation{auth.OperationActionApprove},
				},
				PermittedPolicyIDs: workspace.IDSelection{
					Present: true, Values: [][]byte{workspaceGrantPolicyID},
				},
			},
		})
	if err != nil {
		t.Fatal(err)
	}
	applier := h.opened.settings.(webapi.WorkspaceSettingsApplier)
	narrow := func() (context.Context, fleet.RequestContext) {
		ctx, err := webapi.ApplyWorkspaceSettingsForOperation(
			h.as(mallory), applier, h.authority.Binder(), created.WorkspaceID,
			auth.OperationActionApprove, workspace.MaximumLimits(), nil)
		if err != nil {
			t.Fatal(err)
		}
		decision, err := h.authority.Resolver().Resolve(ctx)
		if err != nil {
			t.Fatal(err)
		}
		return ctx, fleet.RequestContext{
			RequestID: decision.RequestID(), CorrelationID: decision.CorrelationID(),
			ReasonCode: "test", Deadline: h.now().Add(time.Minute),
		}
	}
	missing := fleet.ApprovalReceipt{
		ID: []byte("narrow-missing"), RequestDigest: make([]byte, 32),
		PolicyGeneration: workspacePolicyGeneration,
	}
	answers := func(receipt fleet.ApprovalReceipt) (string, string) {
		ctx, requestContext := narrow()
		_, statusErr := h.opened.approvals.Status(ctx, fleet.ApprovalStatusRequest{
			ID: receipt.ID, Context: requestContext,
		})
		ctx, requestContext = narrow()
		_, decideErr := h.opened.approvals.Decide(ctx, fleet.ApprovalDecisionRequest{
			ID: receipt.ID, RequestDigest: receipt.RequestDigest,
			PolicyGeneration: receipt.PolicyGeneration,
			Verdict:          fleet.ApprovalVerdictApprove, Context: requestContext,
		})
		if statusErr == nil || decideErr == nil {
			t.Fatalf("%s: a narrowed caller was answered: %v / %v",
				receipt.ID, statusErr, decideErr)
		}
		return statusErr.Error(), decideErr.Error()
	}
	missingStatus, missingDecide := answers(missing)
	if !strings.Contains(missingStatus, string(shoal.ErrorNotFound)) {
		t.Fatalf("missing approval status = %s", missingStatus)
	}
	for name, receipt := range map[string]fleet.ApprovalReceipt{
		"another scope":         otherReceipt,
		"not independent of it": ownReceipt,
	} {
		status, decide := answers(receipt)
		if status != missingStatus || decide != missingDecide {
			t.Fatalf("%s answers %q / %q; a missing ID answers %q / %q",
				name, status, decide, missingStatus, missingDecide)
		}
	}
	// And the caller it would otherwise let through is told why it is not.
	status, decide := answers(eligibleReceipt)
	for _, answer := range []string{status, decide} {
		if !strings.Contains(answer, "workspace settings") {
			t.Fatalf("eligible-but-narrowed answer = %s", answer)
		}
	}
}

// TestApprovalMaterializationUsesTheAttemptsOwnTime: a retried
// materialization must judge the window at the time of the retry. Here the
// first approved → enqueued commit loses and, by the time it is retried, the
// window has closed: the request must be expired, not enqueued.
func TestApprovalMaterializationUsesTheAttemptsOwnTime(t *testing.T) {
	t.Parallel()
	armed := &atomic.Bool{}
	h := newHookedHarness(t, func(
		h *approvalHarness, mutation fleet.ApprovalMutation,
	) error {
		if mutation.Record.State == fleet.ApprovalEnqueued &&
			armed.CompareAndSwap(true, false) {
			h.advance(11 * time.Minute)
			return fleet.ErrApprovalConflict
		}
		return nil
	})
	request := h.held("fresh-now")
	receipt := h.mustRequest(requester, request, fleet.ApprovalPending)
	h.advance(50 * time.Minute)
	if _, err := h.decide(approver, receipt, fleet.ApprovalVerdictApprove); err != nil {
		t.Fatal(err)
	}
	armed.Store(true)
	h.mustRequest(requester, request, fleet.ApprovalExpired)
	if _, err := h.opened.fleetDispatch.Status(h.as(requester), fleet.StatusRequest{
		ID: request.id, Context: h.context(h.now().Add(time.Minute)),
	}); !shoal.IsErrorCode(err, shoal.ErrorNotFound) {
		t.Fatalf("work was created after the window closed: %v", err)
	}
}

// TestApprovalJudgesTheCurrentPolicyGeneration: a token minted before the
// policy moved still carries the old generation. Neither a decision nor a
// status read made with one may treat the request as live.
func TestApprovalJudgesTheCurrentPolicyGeneration(t *testing.T) {
	t.Parallel()
	h := newApprovalHarness(t)
	request := h.held("policy-current")
	receipt := h.mustRequest(requester, request, fleet.ApprovalPending)
	h.reader.value.Store(workspacePolicyGeneration + 1)
	// Both tokens below are at the request's (now superseded) generation.
	if _, err := h.decide(
		approver, receipt, fleet.ApprovalVerdictApprove,
	); !errors.Is(err, fleet.ErrApprovalSuperseded) {
		t.Fatalf("decision with a stale token after the policy moved = %v", err)
	}
	status := h.status(request.id)
	if status.State != fleet.ApprovalUnresolvable ||
		status.Condition != fleet.ApprovalConditionPolicyMoved {
		t.Fatalf("status with a stale token = %q/%q", status.State, status.Condition)
	}
	h.reader.value.Store(workspacePolicyGeneration)
	if status := h.status(request.id); status.State != fleet.ApprovalPending {
		t.Fatalf("status once the policy is back = %q", status.State)
	}
}
