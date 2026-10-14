package model

import (
	"context"
	"errors"
	"testing"
)

func testTypedIdentity() TypedIdentity {
	return TypedIdentity{
		Provider: "laya", ModelAlias: "laya-threat-v1", Revision: "r1",
		WeightsDigest: "sha256:weights", TokenizerDigest: "sha256:tokenizer",
		CriteriaID: "criteria-v1", PreprocessingID: "pre-v1", CalibrationID: "uncalibrated",
		RuntimeID: "runtime-v1", Device: "cpu", DistributionTolerance: 0.01,
	}
}

func testTypedRequest() TypedRequest {
	return TypedRequest{
		ModelAlias: "laya-threat-v1", QuestionSetID: "threat-triage-v1", State: []byte("bounded picture"),
		Questions: []TypedQuestion{
			{ID: "risk", Kind: TypedChoice, Instructions: "Classify risk", Options: []string{"low", "high"}, OptionDescriptions: map[string]string{"low": "routine", "high": "requires review"}},
			{ID: "confidence", Kind: TypedPropositionProbability},
		},
	}
}

func validTypedResult() TypedResult {
	id := testTypedIdentity()
	return TypedResult{Identity: id, Status: TypedCompleted, Answers: []TypedAnswer{
		{QuestionID: "risk", Choice: "high", Probability: 0.8, Distribution: []TypedProbability{{Label: "low", Value: 0.2}, {Label: "high", Value: 0.8}}, Confidence: 0.8, AnswerConfidence: 0.8},
		{QuestionID: "confidence", PTrue: 0.7, Confidence: 0.7, AnswerConfidence: 0.7},
	}}
}

func TestValidateTypedResultAcceptsPinnedAlgebra(t *testing.T) {
	if err := ValidateTypedResult(testTypedRequest(), validTypedResult()); err != nil {
		t.Fatal(err)
	}
}

func TestValidateTypedResultDoesNotRenormalize(t *testing.T) {
	r := validTypedResult()
	r.Answers[0].Distribution[1].Value = 0.7
	if err := ValidateTypedResult(testTypedRequest(), r); !errors.Is(err, ErrTypedContract) {
		t.Fatalf("error = %v, want typed contract error", err)
	}
}

func TestValidateTypedResultRejectsIdentityDrift(t *testing.T) {
	r := validTypedResult()
	r.Identity.WeightsDigest = "sha256:other"
	p := FakeTypedPredictor{Pinned: testTypedIdentity(), Result: r}
	if _, err := PredictTyped(context.Background(), p, testTypedRequest()); !errors.Is(err, ErrTypedContract) {
		t.Fatalf("error = %v, want typed contract error", err)
	}
}

func TestValidateTypedResultKeepsAbstentionDistinct(t *testing.T) {
	r := TypedResult{Identity: testTypedIdentity(), Status: TypedAbstained, Reason: "insufficient evidence"}
	if err := ValidateTypedResult(testTypedRequest(), r); err != nil {
		t.Fatal(err)
	}
	bad := r
	bad.Answers = []TypedAnswer{{QuestionID: "risk"}}
	if err := ValidateTypedResult(testTypedRequest(), bad); !errors.Is(err, ErrTypedContract) {
		t.Fatalf("error = %v, want typed contract error", err)
	}
}

func TestPredictTypedValidatesProviderOutput(t *testing.T) {
	p := FakeTypedPredictor{Pinned: testTypedIdentity(), Result: validTypedResult()}
	got, err := PredictTyped(context.Background(), p, testTypedRequest())
	if err != nil {
		t.Fatal(err)
	}
	got.Answers[0].Distribution[0].Value = 0
	if p.Result.Answers[0].Distribution[0].Value != 0.2 {
		t.Fatal("result was not cloned")
	}
}

func TestPredictTypedPreservesOperationalErrors(t *testing.T) {
	want := ErrUnavailable
	p := FakeTypedPredictor{Pinned: testTypedIdentity(), Err: want}
	if _, err := PredictTyped(context.Background(), p, testTypedRequest()); !errors.Is(err, want) {
		t.Fatalf("error = %v, want %v", err, want)
	}
}
