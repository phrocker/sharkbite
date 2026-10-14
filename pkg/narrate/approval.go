// Licensed to the Apache Software Foundation (ASF) under one or more
// contributor license agreements. See the NOTICE file distributed with this
// work for additional information regarding copyright ownership.

package narrate

import (
	"encoding/hex"

	"github.com/phrocker/shoal-oss/pkg/explorer/fleet"
)

func approvalKnown(state fleet.ApprovalState, condition fleet.ApprovalCondition) bool {
	for _, row := range EffectiveApprovals {
		if row.State == state && row.Condition == condition {
			return true
		}
	}
	return false
}

// Approval narrates an approval status: the effective state Status computed,
// the stored state when it differs and why, the record's history, what the
// request is waiting on, and what can happen next.
//
// To narrate a stored record without computing its effective state, pass
// ApprovalStatus{Approval: record, State: record.State}.
func (r *Renderer) Approval(status fleet.ApprovalStatus, opts Options) ([]Sentence, error) {
	record := status.Approval
	request := record.Request
	b := r.begin("approval:"+opaqueID(record.ID), opts)
	refs := []Ref{
		{Kind: "approval_digest", ID: hex.EncodeToString(record.RequestDigest)},
		{Kind: "agent", ID: string(request.AgentID)},
	}
	args := Args{
		"action":       b.ident(request.Action),
		"capability":   b.ident(request.Capability),
		"agent":        b.ident(string(request.AgentID)),
		"generation":   request.AgentGeneration,
		"requester":    b.principal(string(request.Actor), string(request.Subject)),
		"approver":     b.principal(string(record.ApproverActor), string(record.ApproverSubject)),
		"requested":    record.RequestedAt,
		"expires":      record.ExpiresAt,
		"deadline":     request.Deadline,
		"decided":      record.DecidedAt,
		"materialized": record.MaterializedAt,
		"withdrawn":    record.WithdrawnAt,
		"updated":      record.UpdatedAt,
		"policy":       record.PolicyGeneration,
		"stored":       Selector(record.State),
		"verdict":      Selector(record.Verdict),
		"timed":        Selector("no"),
	}
	// Time remaining is stated only when it is positive: a caller passing a
	// Now past the window has a status older than its clock, and "closes 5
	// minutes from now" would then be false.
	if !opts.Now.IsZero() && opts.Now.Before(record.ExpiresAt) {
		args["timed"] = Selector("yes")
		args["left"] = record.ExpiresAt.Sub(opts.Now)
	}
	state, condition := status.State, status.Condition
	if !approvalKnown(state, condition) {
		b.add(RoleOutcome, "approval.outcome.unrecognized", withArgs(args, Args{
			"state":     b.quote(AttributedToRecord, "", string(state)),
			"condition": b.quote(AttributedToRecord, "", string(condition)),
		}), refs...)
		return b.finish()
	}
	key := "approval.outcome." + string(state)
	if condition != fleet.ApprovalConditionNone {
		key += "." + string(condition)
	}
	b.add(RoleOutcome, key, args, refs...)

	// Why the effective state is what it is.
	if condition != fleet.ApprovalConditionNone {
		b.add(RoleReason, "approval.reason."+string(condition), args)
	}
	if record.State != state {
		b.add(RoleReason, "approval.reason.stored_differs", args)
	}

	// History, one sentence per transition the record attests.
	requestRefs := requesterRefs(request)
	decisionRefs := principalRef(string(record.ApproverActor), string(record.ApproverSubject))
	b.add(RoleHistory, "approval.transition.requested", args, requestRefs...)
	if record.DecisionRequestID != "" {
		decisionRefs = append(decisionRefs, Ref{Kind: "request", ID: string(record.DecisionRequestID)})
	}
	switch record.Verdict {
	case fleet.ApprovalVerdictApprove:
		b.add(RoleHistory, "approval.transition.approved", args, decisionRefs...)
	case fleet.ApprovalVerdictRefuse:
		b.add(RoleHistory, "approval.transition.refused", args, decisionRefs...)
	}
	if record.State == fleet.ApprovalExpired {
		if record.Verdict == fleet.ApprovalVerdictApprove {
			b.add(RoleHistory, "approval.transition.expired_unused", args)
		} else {
			b.add(RoleHistory, "approval.transition.expired_undecided", args)
		}
	}
	if !record.MaterializedAt.IsZero() {
		b.add(RoleHistory, "approval.transition.materialized", args)
	}
	switch record.State {
	case fleet.ApprovalWithdrawn:
		b.add(RoleHistory, "approval.transition.withdrawn", args, requestRefs...)
	case fleet.ApprovalStranded:
		switch {
		case !record.MaterializedAt.IsZero():
			b.add(RoleHistory, "approval.transition.stranded_unwritten", args)
		case record.Verdict == fleet.ApprovalVerdictApprove:
			b.add(RoleHistory, "approval.transition.stranded_unused", args)
		default:
			b.add(RoleHistory, "approval.transition.stranded_undecided", args)
		}
	}

	// What it waits on, and what can happen next.
	if condition != fleet.ApprovalConditionNone {
		if condition == fleet.ApprovalConditionAwaitingAction {
			b.add(RoleBlocked, "approval.blocked.enqueued_without_action", args)
		}
		b.add(RoleNext, "approval.next."+string(condition), args)
		return b.finish()
	}
	switch state {
	case fleet.ApprovalPending, fleet.ApprovalApproved:
		b.add(RoleBlocked, "approval.blocked."+string(state), args)
	case fleet.ApprovalEnqueued:
		b.add(RoleNext, "approval.next.enqueued", args,
			Ref{Kind: "action", ID: actionRecordID(record.ID)})
		return b.finish()
	}
	var options []Fragment
	for _, edge := range ApprovalEdges {
		if edge.From == string(state) && approvalEdgeOffered(edge) {
			options = append(options, b.frag("approval.next.edge."+edge.Name, args))
		}
	}
	if len(options) == 0 {
		b.add(RoleNext, "approval.next.final", args)
		return b.finish()
	}
	b.add(RoleNext, "approval.next.options", Args{"options": options})
	return b.finish()
}
