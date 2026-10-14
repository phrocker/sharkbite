// Licensed under the Apache License, Version 2.0. See LICENSE and NOTICE.
// Package decisionoutcomehttp exposes an authenticated outcome store through the
// public protocol. It does not register sources, confer labels, or bypass the
// host's admission and evidence authorities.
package decisionoutcomehttp

import (
	"context"
	"errors"

	outcomes "github.com/phrocker/shoal-oss/internal/decisionoutcomes"
	"github.com/phrocker/shoal-oss/pkg/decision"
	"github.com/phrocker/shoal-oss/pkg/decision/api"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

type Adapter struct{ store *outcomes.Store }

func New(store *outcomes.Store) (*Adapter, error) {
	if store == nil {
		return nil, shoal.NewError(shoal.ErrorInvalidArgument, "outcome store required")
	}
	return &Adapter{store}, nil
}
func truthCopy(v *bool) *bool {
	if v == nil {
		return nil
	}
	copy := *v
	return &copy
}
func toCore(o api.OutcomeObservation) decision.OutcomeObservationConfig {
	return decision.OutcomeObservationConfig{
		RequestID: o.RequestID, PredictionID: o.PredictionID, SubjectID: o.SubjectID,
		Kind: decision.OutcomeKind(o.Kind), QuestionID: o.QuestionID, Label: o.Label, Truth: truthCopy(o.Truth),
		ExecutionStatus: decision.OutcomeExecutionStatus(o.ExecutionStatus), ActionID: o.ActionID,
		EvidenceIDs: append([]shoal.ID(nil), o.EvidenceIDs...), ObservedAt: o.ObservedAt,
		AssertedProvenance: decision.OutcomeProvenance{ReporterID: o.AssertedProvenance.ReporterID, ModelID: o.AssertedProvenance.ModelID, PromptID: o.AssertedProvenance.PromptID, ToolID: o.AssertedProvenance.ToolID}, Supersedes: o.Supersedes,
	}
}
func observation(c decision.OutcomeObservationConfig) api.OutcomeObservation {
	return api.OutcomeObservation{RequestID: c.RequestID, PredictionID: c.PredictionID, SubjectID: c.SubjectID,
		Kind: string(c.Kind), QuestionID: c.QuestionID, Label: c.Label, Truth: truthCopy(c.Truth),
		ExecutionStatus: string(c.ExecutionStatus), ActionID: c.ActionID, EvidenceIDs: append([]shoal.ID(nil), c.EvidenceIDs...), ObservedAt: c.ObservedAt,
		AssertedProvenance: api.OutcomeProvenance{ReporterID: c.AssertedProvenance.ReporterID, ModelID: c.AssertedProvenance.ModelID, PromptID: c.AssertedProvenance.PromptID, ToolID: c.AssertedProvenance.ToolID}, Supersedes: c.Supersedes,
	}
}
func project(r outcomes.Receipt) (api.OutcomeReceipt, error) {
	out := api.OutcomeReceipt{ID: r.ID, ObservationID: r.ObservationID, Observation: observation(r.ObservationConfig), SubmitterID: r.SubmitterID, ActorID: r.ActorID, ClientID: r.ClientID, OnBehalfOf: append([]shoal.ID(nil), r.OnBehalfOf...), AuthorizationFingerprint: r.AuthorizationFingerprint, ReceivedAt: r.ReceivedAt, State: r.State}
	if e := api.ValidateOutcomeReceipt(out); e != nil {
		return api.OutcomeReceipt{}, shoal.NewError(shoal.ErrorUnavailable, "outcome receipt unavailable")
	}
	return out, nil
}
func (a *Adapter) AppendOutcome(ctx context.Context, o api.OutcomeObservation, key []byte) (api.OutcomeReceipt, error) {
	if a == nil || a.store == nil {
		return api.OutcomeReceipt{}, shoal.NewError(shoal.ErrorUnavailable, "outcome store unavailable")
	}
	if len(key) == 0 || len(key) > shoal.MaxIDBytes {
		return api.OutcomeReceipt{}, shoal.NewError(shoal.ErrorInvalidArgument, "invalid outcome key")
	}

	normalized, e := api.NormalizeOutcomeObservation(o)
	if e != nil {
		return api.OutcomeReceipt{}, e
	}
	r, e := a.store.Append(ctx, normalized.RequestID, normalized.PredictionID, append([]byte(nil), key...), toCore(normalized))
	if e != nil {
		if errors.Is(e, outcomes.ErrIndeterminate) {
			return api.OutcomeReceipt{}, api.ErrIndeterminate
		}
		return api.OutcomeReceipt{}, e
	}
	out, e := project(r)
	if e != nil || ctx.Err() != nil {
		return api.OutcomeReceipt{}, api.ErrIndeterminate
	}
	return out, nil
}
func (a *Adapter) ReadOutcome(ctx context.Context, request, prediction shoal.ID, key []byte) (api.OutcomeReceipt, error) {
	if a == nil || a.store == nil {
		return api.OutcomeReceipt{}, shoal.NewError(shoal.ErrorUnavailable, "outcome store unavailable")
	}
	if len(key) == 0 || len(key) > shoal.MaxIDBytes {
		return api.OutcomeReceipt{}, shoal.NewError(shoal.ErrorInvalidArgument, "invalid outcome key")
	}

	r, e := a.store.Read(ctx, request, prediction, append([]byte(nil), key...))
	if e != nil {
		return api.OutcomeReceipt{}, e
	}
	if ctx.Err() != nil {
		return api.OutcomeReceipt{}, ctx.Err()
	}
	return project(r)
}

var _ api.OutcomeProvider = (*Adapter)(nil)
