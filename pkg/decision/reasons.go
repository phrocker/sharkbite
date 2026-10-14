// Licensed to the Apache Software Foundation (ASF) under one or more
// contributor license agreements. See the NOTICE file distributed with this
// work for additional information regarding copyright ownership.
package decision

// Whole-request reasons the decision service establishes itself.
//
// These name facts about the *service's* own adjudication: that it found the
// evidence ineligible and ran nothing, that the predictor it was configured
// with was unavailable or was not the predictor the request pinned, that its
// own deadline computation fired, that the call returned an error, or that the
// response it got back was not a valid prediction.
//
// They are reserved (ReservedServiceReason), so a predictor may not return
// one. Without that, a predictor returning "evidence_ineligible" produced a
// stored result reading "the service found the evidence ineligible and ran no
// predictor" — which was false, and nothing in the record distinguished the
// two (#509).
//
// Reserving rather than recording an origin is deliberate. An origin field
// makes the misattribution legible; reserving makes it unrepresentable, needs
// no new persisted field on every stored result, and lets a reader take the
// reason at face value: if it is one of these, the service established it. A
// predictor with its own view of the same situation says so in its own words,
// which is more honest anyway — its judgement that evidence was unusable is
// not the service's eligibility gate.
const (
	// ReasonEvidenceIneligible is abstention because the service's own
	// eligibility check refused the evidence, before any predictor ran.
	ReasonEvidenceIneligible = "evidence_ineligible"
	// ReasonPredictorUnavailable is failure because the configured predictor
	// could not be reached at all.
	ReasonPredictorUnavailable = "predictor_unavailable"
	// ReasonPredictorIdentityMismatch is failure because the available
	// predictor is not the one the request pinned.
	ReasonPredictorIdentityMismatch = "predictor_identity_mismatch"
	// ReasonDeadlineExceeded is failure because the service's own deadline —
	// derived from the lease, the settlement margin and the principal's
	// authentication expiry — left no time, or elapsed during the call.
	ReasonDeadlineExceeded = "deadline_exceeded"
	// ReasonPredictorFailed is failure because the call returned an error.
	ReasonPredictorFailed = "predictor_failed"
	// ReasonInvalidPredictorResponse is failure because what came back was
	// not a valid prediction — including a response claiming one of these
	// reserved reasons as its own.
	ReasonInvalidPredictorResponse = "invalid_predictor_response"
)

// ServiceReasons lists the reserved reasons by the status they accompany.
//
// Exported so a renderer states a service-established fact only where the
// service established it, instead of mirroring this list and drifting from it.
func ServiceReasons() map[ResultStatus][]string {
	return map[ResultStatus][]string{
		Abstained: {ReasonEvidenceIneligible},
		Failed: {
			ReasonPredictorUnavailable,
			ReasonPredictorIdentityMismatch,
			ReasonDeadlineExceeded,
			ReasonPredictorFailed,
			ReasonInvalidPredictorResponse,
		},
	}
}

// ReservedServiceReason reports whether a reason is one only the service may
// set.
//
// Checked across every status rather than per status on purpose. A predictor
// returning "evidence_ineligible" on a Failed result is making the same false
// claim as one returning it on an Abstained result, and a per-status check
// would admit exactly that.
func ReservedServiceReason(reason string) bool {
	for _, reasons := range ServiceReasons() {
		for _, candidate := range reasons {
			if reason == candidate {
				return true
			}
		}
	}
	return false
}
