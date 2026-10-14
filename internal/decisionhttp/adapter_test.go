// Licensed under the Apache License, Version 2.0. See LICENSE and NOTICE.
package decisionhttp

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/phrocker/shoal-oss/internal/decisionservice"
	"github.com/phrocker/shoal-oss/internal/decisionstore"
	"github.com/phrocker/shoal-oss/pkg/decision"
	"github.com/phrocker/shoal-oss/pkg/decision/api"
)

func TestProjectionPreservesBindingsAndOwnsAnswerValues(t *testing.T) {
	now := time.Date(2026, 10, 8, 12, 0, 0, 123, time.UTC)
	p := 0.25
	r := decisionservice.Response{Receipt: decisionstore.Receipt{ID: "receipt:" + strings.Repeat("a", 64), Version: 2, State: decisionstore.Committed, RequestID: "request", TaskID: "task", PictureID: "picture", PredictorID: "predictor", CreatedAt: now, UpdatedAt: now.Add(time.Second), LeaseUntil: now.Add(time.Minute), PredictionID: "prediction", Result: &decision.ResultConfig{RequestID: "request", PredictorID: "predictor", EffectiveDevice: "cpu", Status: decision.Completed, CompletedAt: now.Add(time.Second), Answers: []decision.Answer{{SubjectID: "subject", QuestionID: "question", Status: decision.Answered, Probability: &p}}}}}
	out, e := project(r, "request", nil)
	if e != nil {
		t.Fatal(e)
	}
	if out.Receipt.RequestID != api.EncodeID("request") || !out.Receipt.CreatedAt.Equal(now) || *out.Receipt.Result.Answers[0].Probability != p {
		t.Fatal("projection changed original")
	}
	*out.Receipt.Result.Answers[0].Probability = 0.9
	if *r.Receipt.Result.Answers[0].Probability != 0.25 {
		t.Fatal("response aliases stored prediction")
	}
	if _, e = project(r, "substituted", nil); e == nil {
		t.Fatal("projection accepted mismatched request")
	}
}
func TestProjectionErrors(t *testing.T) {
	if _, e := New(nil); e == nil {
		t.Fatal("nil service admitted")
	}
	if _, e := project(decisionservice.Response{}, "request", decisionservice.ErrIndeterminate); !errors.Is(e, api.ErrIndeterminate) {
		t.Fatal("write uncertainty lost", e)
	}
	if _, e := project(decisionservice.Response{}, "request", nil); e == nil {
		t.Fatal("malformed receipt projected")
	}
}
