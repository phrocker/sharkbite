// Licensed to the Apache Software Foundation (ASF) under one or more
// contributor license agreements. See the NOTICE file distributed with this
// work for additional information regarding copyright ownership.

package narrate

import (
	"strconv"
	"strings"

	"github.com/phrocker/shoal-oss/pkg/decision"
	"github.com/phrocker/shoal-oss/pkg/explorer/fleet"
)

// The closed vocabularies this package narrates. Where the owner exports
// typed constants they are referenced directly; where the owner is internal/
// or writes string literals, the set is mirrored here and a parity test reads
// the owner's source, so a value added there without a template here fails.

// Effects gateway error codes: the closed set in
// internal/effectsgateway/classify.go. Mirrored rather than imported so this
// public package does not depend on an internal one.
const (
	GatewayRequestNotSent       = "request_not_sent"
	GatewayOutcomeUnknown       = "outcome_unknown"
	GatewayRetryExhausted       = "retry_exhausted"
	GatewayInputInvalid         = "input_invalid"
	GatewayTargetRejectedPrefix = "target_rejected_"
)

// GatewayErrorCodes lists the gateway's fixed codes; target_rejected_NNN is
// the parameterized one.
var GatewayErrorCodes = []string{
	GatewayRequestNotSent, GatewayOutcomeUnknown,
	GatewayRetryExhausted, GatewayInputInvalid,
}

// TargetRejectedStatus parses target_rejected_NNN exactly as the gateway's
// ValidErrorCode accepts it: three digits, 400 to 599.
func TargetRejectedStatus(code string) (int, bool) {
	digits, ok := strings.CutPrefix(code, GatewayTargetRejectedPrefix)
	if !ok || len(digits) != 3 || digits[0] == '0' {
		return 0, false
	}
	for i := 0; i < len(digits); i++ {
		if digits[i] < '0' || digits[i] > '9' {
			return 0, false
		}
	}
	status, err := strconv.Atoi(digits)
	if err != nil || status < 400 || status > 599 {
		return 0, false
	}
	return status, true
}

// FleetErrorCodes are the codes the dispatch service itself writes on a
// failed record (pkg/explorer/fleet/dispatch_service.go).
var FleetErrorCodes = []string{
	"invalid_executor_output",
	"invalid_executor_evidence",
	"invalid_executor_error",
	// The service's adjudication when a reported effected volume is not
	// usable — out of bounds, or attached to an action that declares no
	// egress (#427). Like the others it is the service's own code, so a
	// reader can tell it from anything the executor said.
	"invalid_executor_effected",
	"executor_error",
}

// isGatewayCode reports whether code belongs to the effects gateway's closed
// set: a fixed code or target_rejected_NNN, exactly as errorCodeKey places
// it. Each is assigned only once a request to a target was being bound or
// attempted.
func isGatewayCode(code string) bool {
	if _, ok := TargetRejectedStatus(code); ok {
		return true
	}
	for _, known := range GatewayErrorCodes {
		if code == known {
			return true
		}
	}
	return false
}

// Who assigned an error code, as the renderer narrates it (#508, #529).
//
// The three are the only template decisions there are: a code Shoal itself
// assigned is narrated as Shoal's determination; a code the executor reported
// is narrated as the executor's hedged account; and a code whose origin the
// record does not establish is attributed to neither. They are chosen from
// ActionRecord.ErrorCodeOrigin alone, never from the code: the reserved-code
// list is closed, the origin field is not.
const (
	OriginService  Selector = "service"
	OriginExecutor Selector = "executor"
	OriginEither   Selector = "either"
)

// ErrorOrigins lists the origin renderings, each of which has its own
// reason template for every error code.
var ErrorOrigins = []Selector{OriginService, OriginExecutor, OriginEither}

// errorCodeOrigins is the decision made for every fleet.ErrorCodeOrigin. A
// parity test reads fleet's source, so a constant added there fails until it
// is given a row here. A value not in this table — a record from a newer
// build — renders as OriginEither: the fail-safe is to claim nothing about
// who assigned the code, never to name the executor or Shoal.
var errorCodeOrigins = map[fleet.ErrorCodeOrigin]Selector{
	// Written before the field existed: the code may be either's.
	fleet.ErrorCodeOriginUnknown:  OriginEither,
	fleet.ErrorCodeOriginService:  OriginService,
	fleet.ErrorCodeOriginExecutor: OriginExecutor,
}

// errorOrigin is how a record's error code origin is narrated.
func errorOrigin(origin fleet.ErrorCodeOrigin) Selector {
	if s, ok := errorCodeOrigins[origin]; ok {
		return s
	}
	return OriginEither
}

// errorCodeKey maps a recorded error code to its catalog stem; the reason
// template is the stem plus the origin rendering (errorOrigin), so who the
// sentence says assigned the code depends on the record's origin field and
// never on the code. ok is false for a code outside both closed sets: an
// executor may record any bounded string, and such a code is shown only as a
// quotation.
func errorCodeKey(code string) (stem string, status int, ok bool) {
	if s, isRejected := TargetRejectedStatus(code); isRejected {
		return "dispatch.error.target_rejected", s, true
	}
	for _, known := range GatewayErrorCodes {
		if code == known {
			return "dispatch.error." + code, 0, true
		}
	}
	for _, known := range FleetErrorCodes {
		if code == known {
			return "dispatch.error." + code, 0, true
		}
	}
	return "dispatch.error.unrecognized", 0, false
}

// DispatchStates is every fleet.DispatchState.
var DispatchStates = []fleet.DispatchState{
	fleet.DispatchQueued, fleet.DispatchClaimed, fleet.DispatchSucceeded,
	fleet.DispatchFailed, fleet.DispatchCanceled,
}

// ActionTransitionKinds is every kind fleet.NewActionTransition accepts.
var ActionTransitionKinds = []string{
	"action.enqueued", "action.claimed", "action.completed",
	"action.failed", "action.canceled",
}

// Edge is one transition of a narrated state machine. From is empty for a
// record's creation. Name keys its templates.
type Edge struct {
	Name string
	From string
	To   string
	// Kind is the durable transition kind, where the owner has one.
	Kind string
	// Applies limits the edge to admission records ("admission") or to
	// dispatched ones ("dispatch"); empty applies to both.
	Applies string
}

// DispatchEdges is the dispatch state machine (docs/admission-seam.md,
// pkg/explorer/fleet/dispatch_service.go). An admission is born claimed, or
// born canceled when refused, and cannot be re-claimed or canceled.
var DispatchEdges = []Edge{
	{Name: "enqueue", To: "queued", Kind: "action.enqueued", Applies: "dispatch"},
	{Name: "admit", To: "claimed", Kind: "action.claimed", Applies: "admission"},
	{Name: "deny", To: "canceled", Kind: "action.canceled", Applies: "admission"},
	{Name: "claim", From: "queued", To: "claimed", Kind: "action.claimed", Applies: "dispatch"},
	{Name: "cancel", From: "queued", To: "canceled", Kind: "action.canceled", Applies: "dispatch"},
	{Name: "complete", From: "claimed", To: "succeeded", Kind: "action.completed"},
	{Name: "fail", From: "claimed", To: "failed", Kind: "action.failed"},
	{Name: "reclaim", From: "claimed", To: "claimed", Kind: "action.claimed", Applies: "dispatch"},
	{Name: "cancel_lapsed", From: "claimed", To: "canceled", Kind: "action.canceled", Applies: "dispatch"},
}

// ApprovalStates is every stored fleet.ApprovalState plus the effective-only
// ApprovalUnresolvable.
var ApprovalStates = []fleet.ApprovalState{
	fleet.ApprovalPending, fleet.ApprovalApproved, fleet.ApprovalRefused,
	fleet.ApprovalExpired, fleet.ApprovalEnqueued, fleet.ApprovalUnresolvable,
	fleet.ApprovalWithdrawn, fleet.ApprovalStranded,
}

// ApprovalConditions is every fleet.ApprovalCondition, including none.
var ApprovalConditions = []fleet.ApprovalCondition{
	fleet.ApprovalConditionNone, fleet.ApprovalConditionWindowClosed,
	fleet.ApprovalConditionTargetMoved, fleet.ApprovalConditionPolicyMoved,
	fleet.ApprovalConditionDeadlinePassed, fleet.ApprovalConditionIdentityTaken,
	fleet.ApprovalConditionAwaitingAction,
	fleet.ApprovalConditionApproverMappingMoved,
	fleet.ApprovalConditionIdentitySchemeMoved,
}

// EffectiveApproval is one row of the status table in docs/approval.md: an
// effective state, its condition, and the stored states it can arise from.
type EffectiveApproval struct {
	State     fleet.ApprovalState
	Condition fleet.ApprovalCondition
	Stored    []fleet.ApprovalState
}

// EffectiveApprovals is the status table of docs/approval.md
// ("Status reports the effective state"), as ApprovalService.effectiveState
// computes it.
var EffectiveApprovals = []EffectiveApproval{
	{fleet.ApprovalPending, fleet.ApprovalConditionNone, []fleet.ApprovalState{fleet.ApprovalPending}},
	{fleet.ApprovalApproved, fleet.ApprovalConditionNone, []fleet.ApprovalState{fleet.ApprovalApproved}},
	{fleet.ApprovalExpired, fleet.ApprovalConditionWindowClosed, []fleet.ApprovalState{fleet.ApprovalPending, fleet.ApprovalApproved}},
	{fleet.ApprovalEnqueued, fleet.ApprovalConditionNone, []fleet.ApprovalState{fleet.ApprovalEnqueued}},
	{fleet.ApprovalEnqueued, fleet.ApprovalConditionAwaitingAction, []fleet.ApprovalState{fleet.ApprovalEnqueued}},
	{fleet.ApprovalUnresolvable, fleet.ApprovalConditionIdentityTaken, []fleet.ApprovalState{fleet.ApprovalEnqueued}},
	{fleet.ApprovalUnresolvable, fleet.ApprovalConditionTargetMoved, []fleet.ApprovalState{fleet.ApprovalPending, fleet.ApprovalApproved, fleet.ApprovalEnqueued}},
	{fleet.ApprovalUnresolvable, fleet.ApprovalConditionPolicyMoved, []fleet.ApprovalState{fleet.ApprovalPending, fleet.ApprovalApproved, fleet.ApprovalEnqueued}},
	{fleet.ApprovalUnresolvable, fleet.ApprovalConditionDeadlinePassed, []fleet.ApprovalState{fleet.ApprovalPending, fleet.ApprovalApproved, fleet.ApprovalEnqueued}},
	{fleet.ApprovalUnresolvable, fleet.ApprovalConditionApproverMappingMoved, []fleet.ApprovalState{fleet.ApprovalApproved}},
	{fleet.ApprovalUnresolvable, fleet.ApprovalConditionIdentitySchemeMoved, []fleet.ApprovalState{fleet.ApprovalPending, fleet.ApprovalApproved}},
	{fleet.ApprovalRefused, fleet.ApprovalConditionNone, []fleet.ApprovalState{fleet.ApprovalRefused}},
	{fleet.ApprovalExpired, fleet.ApprovalConditionNone, []fleet.ApprovalState{fleet.ApprovalExpired}},
	// #655: withdrawn by the requester, and stranded for a permanent cause.
	{fleet.ApprovalWithdrawn, fleet.ApprovalConditionNone, []fleet.ApprovalState{fleet.ApprovalWithdrawn}},
	{fleet.ApprovalStranded, fleet.ApprovalConditionTargetMoved, []fleet.ApprovalState{fleet.ApprovalStranded}},
	{fleet.ApprovalStranded, fleet.ApprovalConditionPolicyMoved, []fleet.ApprovalState{fleet.ApprovalStranded}},
	{fleet.ApprovalStranded, fleet.ApprovalConditionIdentityTaken, []fleet.ApprovalState{fleet.ApprovalStranded}},
	{fleet.ApprovalStranded, fleet.ApprovalConditionDeadlinePassed, []fleet.ApprovalState{fleet.ApprovalStranded}},
}

// ApprovalEdges is the approval state machine (docs/approval.md, "The model").
var ApprovalEdges = []Edge{
	{Name: "requested", To: "pending"},
	{Name: "approved", From: "pending", To: "approved"},
	{Name: "refused", From: "pending", To: "refused"},
	{Name: "expired_undecided", From: "pending", To: "expired"},
	{Name: "materialized", From: "approved", To: "enqueued"},
	{Name: "expired_unused", From: "approved", To: "expired"},
	// #655.
	{Name: "withdrawn", From: "pending", To: "withdrawn"},
	{Name: "stranded_undecided", From: "pending", To: "stranded"},
	{Name: "stranded_unused", From: "approved", To: "stranded"},
	// From enqueued, whose next step is its action's own status
	// (approval.next.enqueued), so it is never offered as an option.
	{Name: "stranded_unwritten", From: "enqueued", To: "stranded"},
}

// approvalEdgeOffered reports whether an edge is offered as a next step. An
// enqueued approval's next step is its action's status, not an option.
func approvalEdgeOffered(edge Edge) bool {
	return edge.From != "" && edge.From != string(fleet.ApprovalEnqueued)
}

// Decision vocabularies (pkg/decision).
var (
	ResultStatuses = []decision.ResultStatus{decision.Completed, decision.Abstained, decision.Failed}
	AnswerStatuses = []decision.AnswerStatus{decision.Answered, decision.AnswerAbstained}
	AnswerKinds    = []decision.AnswerKind{decision.Choice, decision.Ordinal, decision.Probability}
	Dispositions   = []decision.Disposition{
		decision.Supported, decision.Unsupported, decision.Missing, decision.OutOfScope,
	}
	InspectionReasons = []decision.InspectionReason{
		decision.EvidenceTruncated, decision.SourceStale, decision.SourceRoleRejected,
		decision.SourceControlRejected, decision.SourceAuthorityRejected,
		decision.SourceAttestationMissing, decision.CoverageUnsatisfied,
		decision.SubjectUnsupported, decision.SubjectNotEvaluated,
		decision.PredictionFailed, decision.PredictionAbstained,
	}
)

// DecisionServiceReasons are the whole-request reasons the decision service
// writes, by status, taken from pkg/decision rather than mirrored: the mirror
// was the drift risk, since a reason added to the service and not to the list
// would have been narrated as a predictor's.
//
// They are still narrated conditionally and attributed to both the predictor
// and the service. Since #556 they are reserved (decision.ReservedServiceReason),
// so a build with #556 never stores one a predictor returned. But a result
// written before #556 could carry one the predictor set — nothing refused it
// then (#509) — and a result records nothing that shows which build wrote it:
// no build version, and the release it names is the predictor's, not the
// service's. Stating the service fact outright would therefore assert, for
// every such historical record, a finding the record cannot support. The hedge
// can go once a result carries a marker that the service wrote its reason, or
// that it was written by a build that refuses these reasons from a predictor.
var DecisionServiceReasons = decision.ServiceReasons()

// AnswerAbstentionReasons are per-answer abstention reasons with a meaning
// documented in docs/decision-contracts.md. Any other reason is the
// predictor's own text and is quoted.
var AnswerAbstentionReasons = []string{"unavailable"}

func knownDecisionReason(status decision.ResultStatus, reason string) bool {
	for _, r := range DecisionServiceReasons[status] {
		if r == reason {
			return true
		}
	}
	return false
}

func knownAnswerReason(reason string) bool {
	for _, r := range AnswerAbstentionReasons {
		if r == reason {
			return true
		}
	}
	return false
}
