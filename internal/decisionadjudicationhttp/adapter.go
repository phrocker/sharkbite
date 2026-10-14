// Licensed under the Apache License, Version 2.0. See LICENSE and NOTICE.
// Package decisionadjudicationhttp projects the authenticated adjudication
// service. It supplies neither evidence authority nor training eligibility.
package decisionadjudicationhttp

import (
	"context"
	"errors"
	adjudication "github.com/phrocker/shoal-oss/internal/decisionadjudication"
	journal "github.com/phrocker/shoal-oss/internal/decisionadjudicationstore"
	"github.com/phrocker/shoal-oss/pkg/decision"
	"github.com/phrocker/shoal-oss/pkg/decision/api"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

type Adapter struct{ service *adjudication.Service }

func New(service *adjudication.Service) (*Adapter, error) {
	if service == nil {
		return nil, shoal.NewError(shoal.ErrorInvalidArgument, "adjudication service required")
	}
	return &Adapter{service}, nil
}
func truthCopy(v *bool) *bool {
	if v == nil {
		return nil
	}
	c := *v
	return &c
}
func toCore(p api.AdjudicationProposal) decision.AdjudicationProposalConfig {
	return decision.AdjudicationProposalConfig{RequestID: p.RequestID, PredictionID: p.PredictionID, SubjectID: p.SubjectID, QuestionID: p.QuestionID, ObservationReceiptIDs: append([]shoal.ID(nil), p.ObservationReceiptIDs...), WitnessIDs: append([]shoal.ID(nil), p.WitnessIDs...), Disposition: decision.AdjudicationDisposition(p.Disposition), Label: p.Label, Truth: truthCopy(p.Truth), Reason: p.Reason, ExpectedHeadID: p.ExpectedHeadID, ExpectedVersion: p.ExpectedVersion}
}
func proposal(p decision.AdjudicationProposalConfig) api.AdjudicationProposal {
	return api.AdjudicationProposal{RequestID: p.RequestID, PredictionID: p.PredictionID, SubjectID: p.SubjectID, QuestionID: p.QuestionID, ObservationReceiptIDs: append([]shoal.ID(nil), p.ObservationReceiptIDs...), WitnessIDs: append([]shoal.ID(nil), p.WitnessIDs...), Disposition: string(p.Disposition), Label: p.Label, Truth: truthCopy(p.Truth), Reason: p.Reason, ExpectedHeadID: p.ExpectedHeadID, ExpectedVersion: p.ExpectedVersion}
}
func project(r journal.Receipt) (api.AdjudicationReceipt, error) {
	out := api.AdjudicationReceipt{BasisID: r.BasisID, ID: r.ID, Version: r.Version, TargetID: r.TargetID, TaskID: r.TaskID, PictureID: r.PictureID, PolicyID: r.PolicyID, ProposalID: r.ProposalID, Proposal: proposal(r.ProposalConfig), Adjudicator: api.AdjudicationAttribution{SubjectID: r.Adjudicator.SubjectID, ActorID: r.Adjudicator.ActorID, ClientID: r.Adjudicator.ClientID, OnBehalfOf: append([]shoal.ID(nil), r.Adjudicator.OnBehalfOf...), AuthorizationFingerprint: r.Adjudicator.AuthorizationFingerprint}, ReceivedAt: r.ReceivedAt}
	if e := api.ValidateAdjudicationReceipt(out); e != nil {
		return api.AdjudicationReceipt{}, shoal.NewError(shoal.ErrorUnavailable, "adjudication receipt unavailable")
	}
	return out, nil
}
func (a *Adapter) Adjudicate(ctx context.Context, p api.AdjudicationProposal, key []byte) (api.AdjudicationReceipt, error) {
	if a == nil || a.service == nil {
		return api.AdjudicationReceipt{}, shoal.NewError(shoal.ErrorUnavailable, "adjudication unavailable")
	}
	if len(key) == 0 || len(key) > shoal.MaxIDBytes {
		return api.AdjudicationReceipt{}, shoal.NewError(shoal.ErrorInvalidArgument, "invalid adjudication key")
	}
	p, e := api.NormalizeAdjudicationProposal(p)
	if e != nil {
		return api.AdjudicationReceipt{}, e
	}
	r, e := a.service.Adjudicate(ctx, append([]byte(nil), key...), toCore(p))
	if e != nil {
		if errors.Is(e, adjudication.ErrIndeterminate) {
			return api.AdjudicationReceipt{}, api.ErrIndeterminate
		}
		return api.AdjudicationReceipt{}, e
	}
	out, e := project(r)
	if e != nil || ctx.Err() != nil {
		return api.AdjudicationReceipt{}, api.ErrIndeterminate
	}
	return out, nil
}
func (a *Adapter) AdjudicationHistory(ctx context.Context, target shoal.ID) (api.AdjudicationHistory, error) {
	if a == nil || a.service == nil {
		return api.AdjudicationHistory{}, shoal.NewError(shoal.ErrorUnavailable, "adjudication unavailable")
	}
	rows, e := a.service.History(ctx, target)
	if e != nil {
		return api.AdjudicationHistory{}, e
	}
	out := api.AdjudicationHistory{TargetID: target, Receipts: make([]api.AdjudicationReceipt, len(rows))}
	for i, r := range rows {
		out.Receipts[i], e = project(r)
		if e != nil {
			return api.AdjudicationHistory{}, e
		}
	}
	if e = api.ValidateAdjudicationHistory(out); e != nil {
		return api.AdjudicationHistory{}, shoal.NewError(shoal.ErrorUnavailable, "adjudication history unavailable")
	}
	if e = ctx.Err(); e != nil {
		return api.AdjudicationHistory{}, e
	}
	return out, nil
}

var _ api.AdjudicationProvider = (*Adapter)(nil)
