// Licensed under the Apache License, Version 2.0. See LICENSE and NOTICE.
package collectorregistry

import (
	"slices"
	"time"

	"github.com/phrocker/shoal-oss/pkg/collector"
	"github.com/phrocker/shoal-oss/pkg/decision"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

// Source maps one active observation onto decision.Source by value.
//
// This is an internal adapter, not an agreed contract: how pictures consume
// collector observations belongs to the decision track (#418) and may change.
// It copies only server-owned facts. OriginID is the collector ID;
// AuthorityPolicyID must be both granted by the enrollment the observation
// was recorded under and still provisioned; Control is the provisioned
// control; AttestationID is set only for a verified attestation, never for a
// claim, and only while that attestation covers the observation (see
// AttestationLapsed). Extraction confidence is deliberately not carried: it is not
// authority. Quarantined observations are refused.
func Source(reg collector.Registration, enrollment Enrollment, record ObservationRecord, authorityPolicyID shoal.ID) (decision.Source, error) {
	c := record.Observation.Config()
	if record.Quarantined || record.Observation.Validate() != nil || reg.State != collector.Enrolled || reg.CollectorID != c.CollectorID {
		return decision.Source{}, invalid()
	}
	if record.Generation != reg.Generation || enrollment.Generation != reg.Generation || record.EnrollmentID != enrollment.ID {
		return decision.Source{}, invalid()
	}
	if !slices.Contains(enrollment.Request.RequestedAuthorityPolicyIDs, authorityPolicyID) || !reg.Permits([]shoal.ID{authorityPolicyID}) {
		return decision.Source{}, invalid()
	}
	var control decision.Control
	switch reg.Control {
	case collector.CandidateControlled:
		control = decision.CandidateControlled
	case collector.ExternalControlled:
		control = decision.ExternalControlled
	case collector.RegistryControlled:
		control = decision.RegistryControlled
	case collector.UnknownControl:
		control = decision.UnknownControl
	default:
		return decision.Source{}, invalid()
	}
	if !collector.ValidDigest(record.ArtifactDigest) {
		return decision.Source{}, invalid()
	}
	source := decision.Source{
		ArtifactID:        c.ArtifactID,
		ID:                record.Observation.ID(),
		RevisionID:        shoal.ID("sha256:" + record.ArtifactDigest),
		Digest:            record.ArtifactDigest,
		OriginID:          c.CollectorID,
		AuthorityPolicyID: authorityPolicyID,
		Role:              decision.Observation,
		Control:           control,
		ObservedAt:        c.ObservedAt,
		ReceivedAt:        record.ReceivedAt,
	}
	if !AttestationLapsed(enrollment, record) {
		source.AttestationID = enrollment.Attestation.ID()
	}
	return source, nil
}

// AttestationLapsed reports whether the enrollment's attestation does not
// cover the observation. An enrollment lasts until revocation but a verified
// statement only covers [IssuedAt, ExpiresAt); an observation observed or
// received outside that window carries no AttestationID. A claim, or no
// attestation at all, never covers anything.
func AttestationLapsed(enrollment Enrollment, record ObservationRecord) bool {
	a := enrollment.Attestation
	if a == nil || a.Status != collector.AttestationVerified || a.ID() == "" {
		return true
	}
	within := func(t time.Time) bool { return !t.Before(a.IssuedAt) && t.Before(a.ExpiresAt) }
	return !within(record.Observation.Config().ObservedAt) || !within(record.ReceivedAt)
}
