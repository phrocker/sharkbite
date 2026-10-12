// Licensed to the Apache Software Foundation (ASF) under one or more
// contributor license agreements. See the NOTICE file distributed with this
// work for additional information regarding copyright ownership.

package narrate

import (
	"sort"
	"sync"

	"github.com/phrocker/shoal-oss/pkg/decision"
	"github.com/phrocker/shoal-oss/pkg/explorer/fleet"
	"github.com/phrocker/shoal-oss/pkg/router"
)

// RequiredKeys returns every catalog key the renderers use, sorted. It is
// derived from the vocabularies in vocab.go, so a value added there needs a
// template in every catalog before any catalog loads.
func RequiredKeys() []string {
	set := requiredKeySet()
	keys := make([]string, 0, len(set))
	for key := range set {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

var requiredKeySet = sync.OnceValue(func() map[string]bool {
	keys := map[string]bool{}
	add := func(values ...string) {
		for _, v := range values {
			keys[v] = true
		}
	}
	// Formatting.
	add("time.unrecorded", "principal.unrecorded",
		"duration.days", "duration.hours", "duration.minutes", "duration.seconds",
		"duration.subsecond", "duration.pair",
		"list.empty", "list.more")
	for _, style := range []string{"and", "or"} {
		add("list."+style+".pair", "list."+style+".end", "list."+style+".join")
	}

	// Dispatch.
	for _, state := range DispatchStates {
		add("dispatch.outcome." + string(state))
	}
	add("dispatch.outcome.unrecognized")
	codes := append(append([]string{}, GatewayErrorCodes...), FleetErrorCodes...)
	codes = append(codes, "target_rejected", "unrecognized")
	for _, code := range codes {
		for _, origin := range ErrorOrigins {
			add("dispatch.error." + code + "." + string(origin))
		}
		add("dispatch.error." + code + ".next")
	}
	add("dispatch.reason.canceled_after_lapse", "dispatch.evidence",
		"dispatch.effect.declared_none", "dispatch.effect.declared_none_unsure",
		"dispatch.gap.effect_may", "dispatch.gap.effect_contradicted",
		"dispatch.gap.effect_possible", "dispatch.gap.no_evidence",
		"dispatch.history.requested", "dispatch.history.approved",
		"dispatch.history.claims", "dispatch.history.claimant",
		"dispatch.blocked.lease_live", "dispatch.blocked.lease_lapsed",
		"dispatch.blocked.deadline_passed", "dispatch.blocked.unclaimed",
		"dispatch.next.succeeded", "dispatch.next.canceled",
		"dispatch.next.none", "dispatch.next.options",
		"dispatch.detail.input", "dispatch.detail.output",
		"dispatch.transition.claim_run", "dispatch.transition.unrecognized")
	for _, edge := range DispatchEdges {
		add("dispatch.transition." + edge.Name)
		if edge.From != "" {
			add("dispatch.next.edge." + edge.Name)
		}
	}

	// Approval.
	for _, row := range EffectiveApprovals {
		key := "approval.outcome." + string(row.State)
		if row.Condition != fleet.ApprovalConditionNone {
			key += "." + string(row.Condition)
		}
		add(key)
	}
	for _, condition := range ApprovalConditions {
		if condition != fleet.ApprovalConditionNone {
			add("approval.reason."+string(condition), "approval.next."+string(condition))
		}
	}
	add("approval.outcome.unrecognized", "approval.reason.stored_differs",
		"approval.blocked.pending", "approval.blocked.approved",
		"approval.blocked.enqueued_without_action",
		"approval.next.enqueued", "approval.next.final", "approval.next.options")
	for _, edge := range ApprovalEdges {
		add("approval.transition." + edge.Name)
		if approvalEdgeOffered(edge) {
			add("approval.next.edge." + edge.Name)
		}
	}

	// Decision.
	for _, status := range ResultStatuses {
		add("decision.outcome." + string(status))
		if status == decision.Completed {
			add("decision.next.completed")
			continue
		}
		for _, reason := range append(append([]string{}, DecisionServiceReasons[status]...), "unrecognized") {
			add("decision.reason."+string(status)+"."+reason,
				"decision.next."+string(status)+"."+reason)
		}
	}
	for _, kind := range AnswerKinds {
		add("decision.answer." + string(kind))
	}
	for _, reason := range AnswerAbstentionReasons {
		add("decision.answer.abstained." + reason)
	}
	add("decision.answer.abstained.unrecognized", "decision.answers.more")
	for _, disposition := range Dispositions {
		if disposition != decision.Supported {
			add("decision.gap.disposition." + string(disposition))
		}
	}
	add("decision.gap.truncated",
		"decision.gap.subject_reason", "decision.gap.more_reasons",
		"decision.gap.measurement.unknown", "decision.gap.measurement.not_applicable",
		"decision.gap.measurement.partial", "decision.gap.inventory",
		"decision.history.requested")
	for _, reason := range InspectionReasons {
		add("decision.inspection." + string(reason))
	}
	add("decision.eligibility.outcome", "decision.eligibility.next",
		"decision.ranking.outcome", "decision.ranking.next")

	// Interaction.
	add("interaction.asserted_reason")

	// Router proposals.
	for _, kind := range router.Kinds {
		if kind == router.KindAbstain {
			continue
		}
		add("router.proposal."+string(kind), "router.target."+string(kind), "router.next."+string(kind))
	}
	for _, reason := range router.Reasons {
		add("router.abstain." + string(reason))
	}
	add("router.slot", "router.next.abstain")
	return keys
})
