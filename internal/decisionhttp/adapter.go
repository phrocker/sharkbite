// Licensed under the Apache License, Version 2.0. See LICENSE and NOTICE.
// Package decisionhttp projects authorized service receipts onto public wire
// contracts. It never loads caller-provided evidence, policies or model paths.
package decisionhttp

import (
	"context"
	"errors"

	"github.com/phrocker/shoal-oss/internal/decisionservice"
	"github.com/phrocker/shoal-oss/pkg/decision/api"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

type Adapter struct{ service *decisionservice.Service }

func New(service *decisionservice.Service) (*Adapter, error) {
	if service == nil {
		return nil, shoal.NewError(shoal.ErrorInvalidArgument, "decision service is required")
	}
	return &Adapter{service}, nil
}
func (a *Adapter) Evaluate(ctx context.Context, id shoal.ID, key []byte) (api.Response, error) {
	if a == nil || a.service == nil {
		return api.Response{}, decisionservice.ErrUnavailable
	}
	response, e := a.service.Evaluate(ctx, id, key)
	out, projectionErr := project(response, id, e)
	if e == nil && projectionErr != nil {
		// The service may have committed even though its wire projection failed.
		return api.Response{}, api.ErrIndeterminate
	}
	return out, projectionErr
}
func (a *Adapter) Read(ctx context.Context, id shoal.ID, key []byte) (api.Response, error) {
	if a == nil || a.service == nil {
		return api.Response{}, decisionservice.ErrUnavailable
	}
	response, e := a.service.Read(ctx, id, key)
	return project(response, id, e)
}
func floatCopy(v *float64) *float64 {
	if v == nil {
		return nil
	}
	copy := *v
	return &copy
}
func project(response decisionservice.Response, id shoal.ID, err error) (api.Response, error) {
	if err != nil {
		if errors.Is(err, decisionservice.ErrIndeterminate) {
			return api.Response{}, api.ErrIndeterminate
		}
		return api.Response{}, err
	}
	r := response.Receipt
	out := api.Response{Schema: 1, Receipt: api.Receipt{ID: r.ID, Version: r.Version, State: string(r.State), RequestID: api.EncodeID(r.RequestID), TaskID: api.EncodeID(r.TaskID), PictureID: api.EncodeID(r.PictureID), PredictorID: api.EncodeID(r.PredictorID), CreatedAt: r.CreatedAt, UpdatedAt: r.UpdatedAt, LeaseUntil: r.LeaseUntil}}
	if r.PredictionID != "" {
		out.Receipt.PredictionID = api.EncodeID(r.PredictionID)
	}
	if r.Result != nil {
		result := r.Result
		out.Receipt.Result = &api.Result{RequestID: api.EncodeID(result.RequestID), PredictorID: api.EncodeID(result.PredictorID), EffectiveDevice: result.EffectiveDevice, Status: string(result.Status), Reason: result.Reason, CompletedAt: result.CompletedAt}
		for _, answer := range result.Answers {
			item := api.Answer{SubjectID: api.EncodeID(answer.SubjectID), QuestionID: api.EncodeID(answer.QuestionID), Status: string(answer.Status), Label: answer.Label, Probability: floatCopy(answer.Probability), Reason: answer.Reason}
			for _, p := range answer.Distribution {
				item.Distribution = append(item.Distribution, api.LabelProbability{Label: p.Label, Probability: p.Probability})
			}
			out.Receipt.Result.Answers = append(out.Receipt.Result.Answers, item)
		}
	}
	if response.Ranking != nil {
		ranking := response.Ranking
		if err := ranking.Validate(); err != nil {
			return api.Response{}, decisionservice.ErrUnavailable
		}
		out.Ranking = &api.Ranking{ID: api.EncodeID(ranking.ID()), PredictionID: api.EncodeID(ranking.PredictionID()), PictureID: api.EncodeID(ranking.PictureID()), Entries: []api.RankingEntry{}}
		for _, entry := range ranking.Entries() {
			item := api.RankingEntry{SubjectID: api.EncodeID(entry.SubjectID), Score: floatCopy(entry.Score)}
			for _, reason := range entry.Reasons {
				item.Reasons = append(item.Reasons, string(reason))
			}
			out.Ranking.Entries = append(out.Ranking.Entries, item)
		}
	}
	if err := api.ValidateResponse(out, id); err != nil {
		return api.Response{}, decisionservice.ErrUnavailable
	}
	return out, nil
}

var _ api.Provider = (*Adapter)(nil)
