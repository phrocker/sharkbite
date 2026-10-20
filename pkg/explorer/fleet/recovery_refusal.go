// Licensed under the Apache License, Version 2.0. See LICENSE and NOTICE.
package fleet

// RecoveryRefusal is a closed, non-secret reason for refusing a prepared
// gateway recovery. It never includes target bytes, credentials, or digests.
type RecoveryRefusal string

const (
	RecoveryLegacyClaim       RecoveryRefusal = "legacy_claim"
	RecoveryMissingCommitment RecoveryRefusal = "missing_commitment"
	RecoveryUnprotectedReplay RecoveryRefusal = "unprotected_replay"
	RecoveryModeDrift         RecoveryRefusal = "mode_drift"
	RecoveryRequestDrift      RecoveryRefusal = "request_drift"
	RecoveryScopeDrift        RecoveryRefusal = "scope_drift"
)

// Valid prevents an arbitrary error wrapper from publishing free text through
// the operator-visible recovery header.
func (r RecoveryRefusal) Valid() bool {
	switch r {
	case RecoveryLegacyClaim, RecoveryMissingCommitment, RecoveryUnprotectedReplay,
		RecoveryModeDrift, RecoveryRequestDrift, RecoveryScopeDrift:
		return true
	default:
		return false
	}
}

// RecoveryRefusalError preserves the normal conflict/claim-lost classification
// while exposing only a closed reason code to authorized operators.
type RecoveryRefusalError struct {
	Code  RecoveryRefusal
	Cause error
}

func (e *RecoveryRefusalError) Error() string { return "gateway recovery refused" }
func (e *RecoveryRefusalError) Unwrap() error { return e.Cause }
