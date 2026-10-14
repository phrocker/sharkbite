// Licensed under the Apache License, Version 2.0. See LICENSE and NOTICE.
package decision_test

import (
	"github.com/phrocker/shoal-oss/pkg/decision"
	"github.com/phrocker/shoal-oss/pkg/shoal"
	"strings"
	"testing"
)

func labelPolicyFixture(t *testing.T) decision.LabelPolicy {
	t.Helper()
	p, err := decision.NewLabelPolicy(decision.LabelPolicyConfig{OwnerID: "shared-policy-authority", Version: "v1", AdjudicatorRoleID: "adjudicators", DisputeResolverRoleID: "resolvers", TrainingPurposeID: "training", MinIndependentWitnesses: 2})
	if err != nil {
		t.Fatal(err)
	}
	return p
}
func adjudicationFixture(t *testing.T) (decision.LabelPolicy, decision.PredictionRecord, decision.AdjudicationProposalConfig) {
	t.Helper()
	policy := labelPolicyFixture(t)
	old, result := requestFixture(t)
	taskCfg := old.Task().Config()
	taskCfg.LabelPolicyID = policy.ID()
	task, err := decision.NewTaskSpec(taskCfg)
	if err != nil {
		t.Fatal(err)
	}
	pack, pictureCfg := fixture(t)
	pictureCfg.TaskID = task.ID()
	picture, err := decision.NewPictureManifest(pack, pictureCfg)
	if err != nil {
		t.Fatal(err)
	}
	request, err := decision.NewDecisionRequest(task, picture, old.Predictor(), old.Config())
	if err != nil {
		t.Fatal(err)
	}
	result.RequestID = request.ID()
	prediction, err := decision.NewPredictionRecord(request, result)
	if err != nil {
		t.Fatal(err)
	}
	return policy, prediction, decision.AdjudicationProposalConfig{RequestID: request.ID(), PredictionID: prediction.ID(), SubjectID: "subject1", QuestionID: "priority", ObservationReceiptIDs: []shoal.ID{"outcome-receipt:" + shoal.ID(strings.Repeat("b", 64)), "outcome-receipt:" + shoal.ID(strings.Repeat("a", 64))}, WitnessIDs: []shoal.ID{"witness:z", "witness:a"}, Disposition: decision.AdjudicationVerified, Label: "low"}
}
func TestLabelPolicyBoundsAndIdentity(t *testing.T) {
	p := labelPolicyFixture(t)
	if err := p.Validate(); err != nil {
		t.Fatal(err)
	}
	for _, n := range []int{0, 65, -1} {
		c := p.Config()
		c.MinIndependentWitnesses = n
		if _, err := decision.NewLabelPolicy(c); err == nil {
			t.Fatal("invalid witness count")
		}
	}
	c := p.Config()
	c.Version = " "
	if _, err := decision.NewLabelPolicy(c); err == nil {
		t.Fatal("empty version")
	}
	c = p.Config()
	c.OwnerID = shoal.ID(string([]byte{255}))
	if _, err := decision.NewLabelPolicy(c); err == nil {
		t.Fatal("invalid policy ID")
	}
	c = p.Config()
	c.DisputeResolverRoleID = "different"
	other, err := decision.NewLabelPolicy(c)
	if err != nil || other.ID() == p.ID() {
		t.Fatal("policy role absent from identity")
	}
	if err := (decision.LabelPolicy{}).Validate(); err == nil {
		t.Fatal("accepted zero policy")
	}
}

func TestAdjudicationStorageIdentityHelpersAreBoundedAndNotValidation(t *testing.T) {
	policy, prediction, config := adjudicationFixture(t)
	proposal, err := decision.NewAdjudicationProposal(policy, prediction, config)
	if err != nil {
		t.Fatal(err)
	}
	target, err := decision.AdjudicationTargetID(proposal.TaskID(), proposal.PictureID(), config.SubjectID, config.QuestionID)
	if err != nil || target != proposal.TargetID() {
		t.Fatal("storage target identity drift", err)
	}
	firstReference := config.ObservationReceiptIDs[0]
	id, err := decision.AdjudicationProposalID(policy.ID(), target, config)
	if err != nil || id != proposal.ID() || config.ObservationReceiptIDs[0] != firstReference {
		t.Fatal("storage proposal identity drift or mutation", err)
	}
	// A metadata hash is deliberately not a validated, authorized proposal.
	config.Label = "not-a-task-label"
	if _, err := decision.AdjudicationProposalID(policy.ID(), target, config); err != nil {
		t.Fatal(err)
	}
	if _, err := decision.NewAdjudicationProposal(policy, prediction, config); err == nil {
		t.Fatal("metadata identity bypassed task label validation")
	}
	config.Label = string([]byte{255})
	if _, err := decision.AdjudicationProposalID(policy.ID(), target, config); err == nil {
		t.Fatal("lossy UTF-8 metadata identity accepted")
	}
	config.Label = "low"
	config.WitnessIDs = make([]shoal.ID, decision.MaxAdjudicationWitnesses+1)
	if _, err := decision.AdjudicationProposalID(policy.ID(), target, config); err == nil {
		t.Fatal("unbounded metadata identity accepted")
	}
	if _, err := decision.AdjudicationTargetID("", proposal.PictureID(), config.SubjectID, config.QuestionID); err == nil {
		t.Fatal("empty target binding accepted")
	}
}
func TestAdjudicationCanonicalReferencesAndCopyIsolation(t *testing.T) {
	policy, prediction, c := adjudicationFixture(t)
	if policy.Config().OwnerID == prediction.Request().Task().Config().OwnerID {
		t.Fatal("fixture must use shared policy owner")
	}
	a, err := decision.NewAdjudicationProposal(policy, prediction, c)
	if err != nil {
		t.Fatal(err)
	}
	if err = a.Validate(); err != nil {
		t.Fatal(err)
	}
	if a.PolicyID() != policy.ID() || a.TaskID() != prediction.Request().TaskID() || a.PictureID() != prediction.Request().PictureID() {
		t.Fatal("derived IDs mismatch")
	}
	c.WitnessIDs[0] = "mutated"
	c.ObservationReceiptIDs[0] = "mutated"
	copy := a.Config()
	copy.WitnessIDs[0] = "mutated"
	copy.ObservationReceiptIDs[0] = "mutated"
	normalized := a.Config()
	normalized.WitnessIDs = []shoal.ID{"witness:z", "witness:a"}
	normalized.ObservationReceiptIDs = []shoal.ID{normalized.ObservationReceiptIDs[1], normalized.ObservationReceiptIDs[0]}
	b, err := decision.NewAdjudicationProposal(policy, prediction, normalized)
	if err != nil || a.ID() != b.ID() {
		t.Fatalf("copy or canonicality failed: %v", err)
	}
	otherCfg := policy.Config()
	otherCfg.Version = "v2"
	other, _ := decision.NewLabelPolicy(otherCfg)
	if _, err := decision.NewAdjudicationProposal(other, prediction, a.Config()); err == nil {
		t.Fatal("accepted unpinned policy")
	}
}
func TestAdjudicationTargetIgnoresRequestPredictionAndReportSubset(t *testing.T) {
	policy, prediction, c := adjudicationFixture(t)
	a, err := decision.NewAdjudicationProposal(policy, prediction, c)
	if err != nil {
		t.Fatal(err)
	}
	request := prediction.Request()
	rc := request.Config()
	rc.PrincipalID = "another principal"
	rc.CorrelationID = "another call"
	next, err := decision.NewDecisionRequest(request.Task(), request.Picture(), request.Predictor(), rc)
	if err != nil {
		t.Fatal(err)
	}
	result := prediction.Config()
	result.RequestID = next.ID()
	p2, err := decision.NewPredictionRecord(next, result)
	if err != nil {
		t.Fatal(err)
	}
	c.RequestID = next.ID()
	c.PredictionID = p2.ID()
	c.ObservationReceiptIDs = c.ObservationReceiptIDs[:1]
	c.WitnessIDs = []shoal.ID{"other1", "other2"}
	c.Label = "high"
	b, err := decision.NewAdjudicationProposal(policy, p2, c)
	if err != nil {
		t.Fatal(err)
	}
	if a.TargetID() != b.TargetID() || a.ID() == b.ID() {
		t.Fatal("label journal target fragmented or proposal identity collided")
	}
}
func TestAdjudicationRejectsMalformedClaims(t *testing.T) {
	changes := map[string]func(*decision.AdjudicationProposalConfig){
		"request": func(c *decision.AdjudicationProposalConfig) { c.RequestID = "other" }, "prediction": func(c *decision.AdjudicationProposalConfig) { c.PredictionID = "other" }, "subject": func(c *decision.AdjudicationProposalConfig) { c.SubjectID = "other" }, "question": func(c *decision.AdjudicationProposalConfig) { c.QuestionID = "other" },
		"no observations": func(c *decision.AdjudicationProposalConfig) { c.ObservationReceiptIDs = nil }, "too many observations": func(c *decision.AdjudicationProposalConfig) { c.ObservationReceiptIDs = make([]shoal.ID, 257) }, "duplicate observations": func(c *decision.AdjudicationProposalConfig) { c.ObservationReceiptIDs[1] = c.ObservationReceiptIDs[0] }, "assertion instead receipt": func(c *decision.AdjudicationProposalConfig) { c.ObservationReceiptIDs[0] = "outcome:unreceived" },
		"duplicate witnesses": func(c *decision.AdjudicationProposalConfig) { c.WitnessIDs[1] = c.WitnessIDs[0] }, "insufficient witnesses": func(c *decision.AdjudicationProposalConfig) { c.WitnessIDs = c.WitnessIDs[:1] }, "too many witnesses": func(c *decision.AdjudicationProposalConfig) { c.WitnessIDs = make([]shoal.ID, 1025) }, "invalid witness": func(c *decision.AdjudicationProposalConfig) { c.WitnessIDs[0] = "" },
		"unknown label": func(c *decision.AdjudicationProposalConfig) { c.Label = "other" }, "verification reason": func(c *decision.AdjudicationProposalConfig) { c.Reason = "asserted authority" }, "label truth": func(c *decision.AdjudicationProposalConfig) { v := false; c.Truth = &v },
		"head without version": func(c *decision.AdjudicationProposalConfig) {
			c.ExpectedHeadID = "adjudication-receipt:" + shoal.ID(strings.Repeat("a", 64))
		}, "version without head": func(c *decision.AdjudicationProposalConfig) { c.ExpectedVersion = 1 }, "wrong head family": func(c *decision.AdjudicationProposalConfig) {
			c.ExpectedHeadID = c.ObservationReceiptIDs[0]
			c.ExpectedVersion = 1
		}, "negative version": func(c *decision.AdjudicationProposalConfig) { c.ExpectedVersion = -1 },
	}
	for name, change := range changes {
		t.Run(name, func(t *testing.T) {
			policy, prediction, c := adjudicationFixture(t)
			change(&c)
			if _, err := decision.NewAdjudicationProposal(policy, prediction, c); err == nil {
				t.Fatal("accepted invalid proposal")
			}
		})
	}
	if err := (decision.AdjudicationProposal{}).Validate(); err == nil {
		t.Fatal("accepted zero proposal")
	}
}
func TestAdjudicationDisputeAndUnknownCarryNoLabel(t *testing.T) {
	policy, prediction, c := adjudicationFixture(t)
	c.Label = ""
	c.Reason = "conflicting evidence"
	c.Disposition = decision.AdjudicationDisputed
	c.WitnessIDs = c.WitnessIDs[:1]
	c.ExpectedHeadID = "adjudication-receipt:" + shoal.ID(strings.Repeat("a", 64))
	c.ExpectedVersion = 1
	if _, err := decision.NewAdjudicationProposal(policy, prediction, c); err != nil {
		t.Fatal(err)
	}
	c.WitnessIDs = nil
	if _, err := decision.NewAdjudicationProposal(policy, prediction, c); err == nil {
		t.Fatal("dispute without witness")
	}
	c.Disposition = decision.AdjudicationUnresolved
	if _, err := decision.NewAdjudicationProposal(policy, prediction, c); err != nil {
		t.Fatal(err)
	}
	c.Label = "low"
	if _, err := decision.NewAdjudicationProposal(policy, prediction, c); err == nil {
		t.Fatal("unknown became negative label")
	}
}

func TestAdjudicationPropositionTruthIsNotProbability(t *testing.T) {
	policy, original, c := adjudicationFixture(t)
	taskCfg := original.Request().Task().Config()
	taskCfg.Questions = []decision.Question{{ID: "truth", Kind: decision.Probability, RubricID: "proposition-rubric"}}
	task, err := decision.NewTaskSpec(taskCfg)
	if err != nil {
		t.Fatal(err)
	}
	pack, pictureCfg := fixture(t)
	pictureCfg.TaskID = task.ID()
	picture, err := decision.NewPictureManifest(pack, pictureCfg)
	if err != nil {
		t.Fatal(err)
	}
	request, err := decision.NewDecisionRequest(task, picture, original.Request().Predictor(), original.Request().Config())
	if err != nil {
		t.Fatal(err)
	}
	probability := 0.9
	result := original.Config()
	result.RequestID = request.ID()
	result.Answers = []decision.Answer{{SubjectID: "subject1", QuestionID: "truth", Status: decision.Answered, Probability: &probability}}
	prediction, err := decision.NewPredictionRecord(request, result)
	if err != nil {
		t.Fatal(err)
	}
	truth := false
	c.RequestID = request.ID()
	c.PredictionID = prediction.ID()
	c.QuestionID = "truth"
	c.Label = ""
	c.Truth = &truth
	a, err := decision.NewAdjudicationProposal(policy, prediction, c)
	if err != nil {
		t.Fatal(err)
	}
	truth = true
	if *a.Config().Truth {
		t.Fatal("input truth escaped")
	}
	copy := a.Config()
	*copy.Truth = true
	if *a.Config().Truth {
		t.Fatal("output truth escaped")
	}
	c.Truth = nil
	if _, err := decision.NewAdjudicationProposal(policy, prediction, c); err == nil {
		t.Fatal("absent truth treated as false")
	}
}
