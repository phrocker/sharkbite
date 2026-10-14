// Licensed under the Apache License, Version 2.0. See LICENSE and NOTICE.
package decision_test

import (
	"github.com/phrocker/shoal-oss/pkg/decision"
	"github.com/phrocker/shoal-oss/pkg/shoal"
	"strings"
	"testing"
	"time"
)

func outcomeFixture(t *testing.T) (decision.PredictionRecord, decision.OutcomeObservationConfig) {
	t.Helper()
	request, result := requestFixture(t)
	prediction, err := decision.NewPredictionRecord(request, result)
	if err != nil {
		t.Fatal(err)
	}
	return prediction, decision.OutcomeObservationConfig{RequestID: request.ID(), PredictionID: prediction.ID(), SubjectID: "subject1", Kind: decision.OutcomeCorrectness, QuestionID: "priority", Label: "low", EvidenceIDs: []shoal.ID{"evidence:z", "evidence:a"}, ObservedAt: now, AssertedProvenance: decision.OutcomeProvenance{ReporterID: "asserted-reporter"}}
}
func TestOutcomeBindingAndCopyIsolation(t *testing.T) {
	prediction, c := outcomeFixture(t)
	a, err := decision.NewOutcomeObservation(prediction, c)
	if err != nil {
		t.Fatal(err)
	}
	if err = a.Validate(); err != nil {
		t.Fatal(err)
	}
	if a.TaskID() != prediction.Request().TaskID() || a.PictureID() != prediction.Request().PictureID() || a.RequestID() != c.RequestID || a.PredictionID() != c.PredictionID {
		t.Fatal("derived binding mismatch")
	}
	c.EvidenceIDs[0] = "changed"
	owned := a.Config()
	owned.EvidenceIDs[0] = "also changed"
	if a.Config().EvidenceIDs[0] != "evidence:a" {
		t.Fatal("evidence slice escaped")
	}
	normalized := a.Config()
	normalized.EvidenceIDs = []shoal.ID{"evidence:z", "evidence:a"}
	normalized.ObservedAt = now.In(time.FixedZone("other", 7200))
	b, err := decision.NewOutcomeObservation(prediction, normalized)
	if err != nil || b.ID() != a.ID() {
		t.Fatalf("noncanonical ordering/time: %v", err)
	}
	normalized.AssertedProvenance.ReporterID = "another asserted reporter"
	b, err = decision.NewOutcomeObservation(prediction, normalized)
	if err != nil || b.ID() == a.ID() {
		t.Fatal("asserted provenance omitted from identity")
	}
}

func TestOutcomeRejectsUTCYearOverflow(t *testing.T) {
	prediction, config := outcomeFixture(t)
	for _, stamp := range []time.Time{
		time.Date(1, 1, 1, 0, 0, 0, 0, time.FixedZone("east", 3600)),
		time.Date(9999, 12, 31, 23, 59, 59, 0, time.FixedZone("west", -3600)),
	} {
		config.ObservedAt = stamp
		if _, err := decision.NewOutcomeObservation(prediction, config); err == nil {
			t.Fatalf("accepted UTC timestamp outside supported years: %v", stamp)
		}
	}
}
func TestOutcomeRejectsInvalidVariantsAndSubstitution(t *testing.T) {
	changes := map[string]func(*decision.OutcomeObservationConfig){
		"request": func(c *decision.OutcomeObservationConfig) { c.RequestID = "other" }, "prediction": func(c *decision.OutcomeObservationConfig) { c.PredictionID = "other" },
		"subject": func(c *decision.OutcomeObservationConfig) { c.SubjectID = "other" }, "question": func(c *decision.OutcomeObservationConfig) { c.QuestionID = "other" },
		"label": func(c *decision.OutcomeObservationConfig) { c.Label = "unregistered" }, "kind": func(c *decision.OutcomeObservationConfig) { c.Kind = "verified" },
		"empty evidence": func(c *decision.OutcomeObservationConfig) { c.EvidenceIDs = nil }, "duplicate evidence": func(c *decision.OutcomeObservationConfig) { c.EvidenceIDs = []shoal.ID{"same", "same"} },
		"too many evidence": func(c *decision.OutcomeObservationConfig) { c.EvidenceIDs = make([]shoal.ID, decision.MaxSources+1) },
		"invalid evidence":  func(c *decision.OutcomeObservationConfig) { c.EvidenceIDs = []shoal.ID{shoal.ID(string([]byte{255}))} },
		"invalid provenance": func(c *decision.OutcomeObservationConfig) {
			c.AssertedProvenance.ModelID = shoal.ID(strings.Repeat("m", shoal.MaxIDBytes+1))
		},
		"wrong correction family": func(c *decision.OutcomeObservationConfig) {
			c.Supersedes = "receipt:" + shoal.ID(strings.Repeat("a", 64))
		},
		"wrong correction hash": func(c *decision.OutcomeObservationConfig) {
			c.Supersedes = "outcome-receipt:" + shoal.ID(strings.Repeat("A", 64))
		},
		"time": func(c *decision.OutcomeObservationConfig) { c.ObservedAt = time.Time{} }, "truth on label": func(c *decision.OutcomeObservationConfig) { v := false; c.Truth = &v },
		"action on correctness": func(c *decision.OutcomeObservationConfig) { c.ActionID = "action" }, "execution on correctness": func(c *decision.OutcomeObservationConfig) { c.ExecutionStatus = decision.ExecutionSucceeded },
	}
	for name, change := range changes {
		t.Run(name, func(t *testing.T) {
			prediction, c := outcomeFixture(t)
			change(&c)
			if _, err := decision.NewOutcomeObservation(prediction, c); err == nil {
				t.Fatal("accepted invalid observation")
			}
		})
	}
	if err := (decision.OutcomeObservation{}).Validate(); err == nil {
		t.Fatal("accepted zero observation")
	}
}
func TestExecutionSuccessIsNotCorrectness(t *testing.T) {
	prediction, c := outcomeFixture(t)
	c.Kind = decision.OutcomeExecution
	c.QuestionID = ""
	c.Label = ""
	c.ActionID = "action:1"
	for _, status := range []decision.OutcomeExecutionStatus{decision.ExecutionSucceeded, decision.ExecutionFailed, decision.ExecutionUnknown} {
		c.ExecutionStatus = status
		a, err := decision.NewOutcomeObservation(prediction, c)
		if err != nil {
			t.Fatal(err)
		}
		if a.Config().QuestionID != "" || a.Config().Label != "" || a.Config().Truth != nil {
			t.Fatal("execution became correctness")
		}
	}
	c.Label = "high"
	if _, err := decision.NewOutcomeObservation(prediction, c); err == nil {
		t.Fatal("execution accepted label")
	}
	c.Label = ""
	c.ActionID = ""
	if _, err := decision.NewOutcomeObservation(prediction, c); err == nil {
		t.Fatal("execution accepted no action")
	}
}
func TestPropositionOutcomeUsesObservedTruthAndOwnsPointer(t *testing.T) {
	old, result := requestFixture(t)
	cfg := old.Task().Config()
	cfg.Questions = []decision.Question{{ID: "proposition", Kind: decision.Probability, RubricID: "rubric"}}
	task, err := decision.NewTaskSpec(cfg)
	if err != nil {
		t.Fatal(err)
	}
	pack, pictureConfig := fixture(t)
	pictureConfig.TaskID = task.ID()
	picture, err := decision.NewPictureManifest(pack, pictureConfig)
	if err != nil {
		t.Fatal(err)
	}
	request, err := decision.NewDecisionRequest(task, picture, old.Predictor(), old.Config())
	if err != nil {
		t.Fatal(err)
	}
	p := 0.9
	result.RequestID = request.ID()
	result.Answers = []decision.Answer{{SubjectID: "subject1", QuestionID: "proposition", Status: decision.Answered, Probability: &p}}
	prediction, err := decision.NewPredictionRecord(request, result)
	if err != nil {
		t.Fatal(err)
	}
	truth := false
	c := decision.OutcomeObservationConfig{RequestID: request.ID(), PredictionID: prediction.ID(), SubjectID: "subject1", Kind: decision.OutcomeCorrectness, QuestionID: "proposition", Truth: &truth, EvidenceIDs: []shoal.ID{"evidence"}, ObservedAt: now, Supersedes: "outcome-receipt:" + shoal.ID(strings.Repeat("a", 64))}
	a, err := decision.NewOutcomeObservation(prediction, c)
	if err != nil {
		t.Fatal(err)
	}
	truth = true
	if *a.Config().Truth {
		t.Fatal("input truth pointer escaped")
	}
	copy := a.Config()
	*copy.Truth = true
	if *a.Config().Truth {
		t.Fatal("output truth pointer escaped")
	}
	c.Truth = nil
	if _, err := decision.NewOutcomeObservation(prediction, c); err == nil {
		t.Fatal("unknown treated as false")
	}
}
