// Licensed under the Apache License, Version 2.0. See LICENSE and NOTICE.
package decisionoutcomes

import (
	"context"
	"time"

	"github.com/phrocker/shoal-oss/pkg/decision"
	"github.com/phrocker/shoal-oss/pkg/explorer/auth"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

// Admission coordinates this store with a trusted, durable target inventory.
// Begin MUST durably retain a pending intent before returning success. Publish
// MUST reconcile the exact original immutable receipt. Neither operation may
// infer rollback from absence, expire pending intents, or grant source access.
// All outcome writers inside an asserted inventory coverage boundary must use
// the same admission coordinator. A nil coordinator retains legacy behavior
// and supplies no inventory completeness guarantee.
//
// The store invokes these methods only after current outcome authorization and
// repeats authorization after their IO. Caller identity is the actual resolved
// decision used to derive the receipt, not asserted outcome provenance.
// Implementations must be concurrency safe and preserve exact retry semantics.
type Admission interface {
	Begin(context.Context, auth.Decision, decision.PredictionRecord, decision.OutcomeObservation, shoal.ID) error
	Publish(context.Context, auth.Decision, decision.PredictionRecord, decision.OutcomeObservation, Receipt) error
}

func (s *Store) beginAdmission(ctx context.Context, d auth.Decision, p decision.PredictionRecord, o decision.OutcomeObservation, id shoal.ID, received time.Time) error {
	if e := s.config.Admission.Begin(ctx, d, p, o, id); e != nil {
		return sanitized(e)
	}
	return s.check(ctx, d, o, id, received, auth.OperationIngest)
}
func (s *Store) publishAdmission(ctx context.Context, d auth.Decision, p decision.PredictionRecord, o decision.OutcomeObservation, r Receipt) (Receipt, error) {
	detached := r
	detached.OnBehalfOf = append([]shoal.ID(nil), r.OnBehalfOf...)
	detached.ObservationConfig = o.Config()
	if e := s.config.Admission.Publish(ctx, d, p, o, detached); e != nil {
		return Receipt{}, sanitized(e)
	}
	if e := s.check(ctx, d, o, r.ID, r.ReceivedAt, auth.OperationIngest); e != nil {
		return Receipt{}, e
	}
	return r, nil
}
