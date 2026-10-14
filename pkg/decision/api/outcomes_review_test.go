// Licensed under the Apache License, Version 2.0. See LICENSE and NOTICE.
package api

import (
	"context"
	"errors"
	"github.com/phrocker/shoal-oss/pkg/shoal"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestReviewOutcomeEverySemanticFieldBound(t *testing.T) {
	for name, mutate := range map[string]func(*OutcomeObservation){
		"subject":  func(o *OutcomeObservation) { o.SubjectID = "other" },
		"question": func(o *OutcomeObservation) { o.QuestionID = "other" },
		"truth":    func(o *OutcomeObservation) { v := true; o.Truth = &v },
		"label":    func(o *OutcomeObservation) { o.Truth = nil; o.Label = "positive" },
		"kind": func(o *OutcomeObservation) {
			o.Kind = "execution"
			o.QuestionID = ""
			o.Truth = nil
			o.ActionID = "action"
			o.ExecutionStatus = "failed"
		},
		"evidence":   func(o *OutcomeObservation) { o.EvidenceIDs = []shoal.ID{"different"} },
		"time":       func(o *OutcomeObservation) { o.ObservedAt = o.ObservedAt.Add(-time.Nanosecond) },
		"reporter":   func(o *OutcomeObservation) { o.AssertedProvenance.ReporterID = "other" },
		"model":      func(o *OutcomeObservation) { o.AssertedProvenance.ModelID = "other" },
		"prompt":     func(o *OutcomeObservation) { o.AssertedProvenance.PromptID = "other" },
		"tool":       func(o *OutcomeObservation) { o.AssertedProvenance.ToolID = "other" },
		"supersedes": func(o *OutcomeObservation) { o.Supersedes = shoal.ID("outcome-receipt:" + strings.Repeat("d", 64)) },
	} {
		t.Run(name, func(t *testing.T) {
			original := outcomeFixture()
			replacement := outcomeFixture()
			mutate(&replacement.Observation)
			raw, err := EncodeOutcomeReceipt(replacement)
			if err != nil {
				t.Fatal(err)
			}
			calls := 0
			c := outcomeClient(t, func(*http.Request) (*http.Response, error) { calls++; return outcomeHTTPResponse(200, raw), nil })
			got, err := c.AppendOutcome(context.Background(), original.Observation, []byte("key"))
			if got.ID != "" || !errors.Is(err, ErrIndeterminate) || calls != 1 {
				t.Fatalf("substitution accepted: %v %v %d", got, err, calls)
			}
		})
	}
}
