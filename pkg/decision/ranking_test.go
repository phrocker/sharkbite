/*
 * Licensed to the Apache Software Foundation (ASF) under one
 * or more contributor license agreements. See the NOTICE file
 * distributed with this work for additional information
 * regarding copyright ownership. The ASF licenses this file
 * to you under the Apache License, Version 2.0 (the
 * "License"); you may not use this file except in compliance
 * with the License. You may obtain a copy of the License at
 *
 *     https://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing,
 * software distributed under the License is distributed on an
 * "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY
 * KIND, either express or implied. See the License for the
 * specific language governing permissions and limitations
 * under the License.
 */

package decision_test

import (
	"math"
	"reflect"
	"testing"
	"time"

	"github.com/phrocker/shoal-oss/pkg/decision"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

func evidenceConfig() decision.EvidencePolicyConfig {
	return decision.EvidencePolicyConfig{MaxObservationAge: time.Hour, AllowedRoles: []decision.EvidenceRole{decision.Observation}, AllowedControls: []decision.Control{decision.CandidateControlled}, AuthorityPolicyIDs: []shoal.ID{"authority:1"}, Measurements: []decision.MeasurementRequirement{{ID: "coverage", Unit: "subject", MethodID: "count:1", MinimumFraction: 1}}}
}
func rankingConfig() decision.RankingConfig {
	return decision.RankingConfig{Terms: []decision.RankingTerm{{QuestionID: "priority", Weight: 0.75}, {QuestionID: "relevance", Weight: 0.25, Labels: []decision.LabelPriority{{Label: "yes", Value: 1}, {Label: "no", Value: 0}}}}}
}
func rankingFixture(t *testing.T, change func(*decision.PictureConfig), modify func(*decision.ResultConfig), options ...func(*decision.TaskConfig, *decision.EvidencePolicyConfig, *decision.RankingConfig)) (decision.PredictionRecord, decision.EvidencePolicy, decision.RankingPlan) {
	t.Helper()
	pack, c := fixture(t)
	tc := taskConfig()
	ep := evidenceConfig()
	rp := rankingConfig()
	for _, option := range options {
		option(&tc, &ep, &rp)
	}
	policy, err := decision.NewEvidencePolicy(ep)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := decision.NewRankingPlan(rp)
	if err != nil {
		t.Fatal(err)
	}
	tc.EvidencePolicyID = policy.ID()
	tc.AggregationID = plan.ID()
	task, err := decision.NewTaskSpec(tc)
	if err != nil {
		t.Fatal(err)
	}
	c.TaskID = task.ID()
	anchor := c.Subjects[0].EvidenceIDs[0]
	c.Subjects = append(c.Subjects, decision.Subject{ID: "subject2", SourceID: "source", Disposition: decision.Supported, EvidenceIDs: []shoal.ID{anchor}}, decision.Subject{ID: "unrequested", SourceID: "source", Disposition: decision.Supported, EvidenceIDs: []shoal.ID{anchor}})
	if change != nil {
		change(&c)
	}
	picture, err := decision.NewPictureManifest(pack, c)
	if err != nil {
		t.Fatal(err)
	}
	model, err := decision.NewPredictorIdentity(predictorConfig())
	if err != nil {
		t.Fatal(err)
	}
	baseline, out := requestFixture(t)
	rc := baseline.Config()
	rc.SubjectIDs = []shoal.ID{"subject2", "subject1"}
	request, err := decision.NewDecisionRequest(task, picture, model, rc)
	if err != nil {
		t.Fatal(err)
	}
	out.RequestID = request.ID()
	out.PredictorID = model.ID()
	out.Answers[0].Distribution = nil
	out.Answers = append(out.Answers, decision.Answer{SubjectID: "subject2", QuestionID: "priority", Status: decision.Answered, Label: "low"}, decision.Answer{SubjectID: "subject2", QuestionID: "relevance", Status: decision.Answered, Label: "yes"})
	if modify != nil {
		modify(&out)
	}
	prediction, err := decision.NewPredictionRecord(request, out)
	if err != nil {
		t.Fatal(err)
	}
	return prediction, policy, plan
}
func TestRankingPreservesInventoryAndDeterministicPriority(t *testing.T) {
	p, e, r := rankingFixture(t, nil, nil)
	ranking, err := decision.NewInspectionRanking(p, e, r)
	if err != nil {
		t.Fatal(err)
	}
	entries := ranking.Entries()
	if len(entries) != 3 || entries[0].SubjectID != "unrequested" || entries[0].Score != nil || entries[1].SubjectID != "subject1" || *entries[1].Score != 1 || entries[2].SubjectID != "subject2" || *entries[2].Score != 0.25 {
		t.Fatalf("unexpected ranking: %#v", entries)
	}
	if !reflect.DeepEqual(entries[0].Reasons, []decision.InspectionReason{decision.SubjectNotEvaluated}) {
		t.Fatal(entries[0].Reasons)
	}
	entries[0].Reasons[0] = "mutated"
	*entries[1].Score = 0
	if err := ranking.Validate(); err != nil {
		t.Fatal(err)
	}
	if *ranking.Entries()[1].Score != 1 {
		t.Fatal("ranking aliases output")
	}
	if ranking.PredictionID() != p.ID() || ranking.PictureID() == "" {
		t.Fatal("missing provenance")
	}
	if (decision.InspectionRanking{}).Validate() == nil {
		t.Fatal("zero ranking accepted")
	}
}
func TestRankingTiesUseSubjectIdentity(t *testing.T) {
	p, e, r := rankingFixture(t, nil, func(c *decision.ResultConfig) {
		c.Answers[2].Label = "high"
		c.Answers[0], c.Answers[2] = c.Answers[2], c.Answers[0]
	})
	ranking, err := decision.NewInspectionRanking(p, e, r)
	if err != nil {
		t.Fatal(err)
	}
	entries := ranking.Entries()
	if entries[1].SubjectID != "subject1" || entries[2].SubjectID != "subject2" || *entries[1].Score != *entries[2].Score {
		t.Fatal("unstable tied ordering")
	}
}
func TestEvidenceFailuresPreventWholePictureScoring(t *testing.T) {
	cases := map[string]struct {
		change func(*decision.PictureConfig)
		reason decision.InspectionReason
	}{
		"stale observation despite fresh receipt": {func(c *decision.PictureConfig) { c.Sources[0].ObservedAt = now.Add(-2 * time.Hour) }, decision.SourceStale},
		"retyped prediction":                      {func(c *decision.PictureConfig) { c.Sources[0].Role = decision.Prediction }, decision.SourceRoleRejected},
		"unapproved control":                      {func(c *decision.PictureConfig) { c.Sources[0].Control = decision.UnknownControl }, decision.SourceControlRejected},
		"unapproved authority":                    {func(c *decision.PictureConfig) { c.Sources[0].AuthorityPolicyID = "authority:unapproved" }, decision.SourceAuthorityRejected},
		"unknown coverage":                        {func(c *decision.PictureConfig) { c.Measurements[0].Denominator = nil }, decision.CoverageUnsatisfied},
		"zero denominator":                        {func(c *decision.PictureConfig) { c.Measurements[0].Numerator = 0; *c.Measurements[0].Denominator = 0 }, decision.CoverageUnsatisfied},
		"missing measurement":                     {func(c *decision.PictureConfig) { c.Measurements = nil }, decision.CoverageUnsatisfied},
		"substituted method":                      {func(c *decision.PictureConfig) { c.Measurements[0].MethodID = "other-method" }, decision.CoverageUnsatisfied},
		"substituted unit":                        {func(c *decision.PictureConfig) { c.Measurements[0].Unit = "file" }, decision.CoverageUnsatisfied},
		"near complete large count": {func(c *decision.PictureConfig) {
			c.Measurements[0].Numerator = math.MaxUint64 - 1
			*c.Measurements[0].Denominator = math.MaxUint64
		}, decision.CoverageUnsatisfied},
		"unused source contaminates shared input": {func(c *decision.PictureConfig) {
			s := c.Sources[0]
			s.ID = "context-source"
			s.Role = decision.Prediction
			c.Sources = append(c.Sources, s)
		}, decision.SourceRoleRejected},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			p, e, r := rankingFixture(t, tc.change, nil)
			ranking, err := decision.NewInspectionRanking(p, e, r)
			if err != nil {
				t.Fatal(err)
			}
			for _, entry := range ranking.Entries() {
				if entry.Score != nil {
					t.Fatal("ineligible evidence scored")
				}
				found := false
				for _, reason := range entry.Reasons {
					if reason == tc.reason {
						found = true
					}
				}
				if !found {
					t.Fatalf("missing reason %s: %v", tc.reason, entry.Reasons)
				}
			}
		})
	}
}
func TestEligibilityFreshnessBoundary(t *testing.T) {
	p, e, r := rankingFixture(t, func(c *decision.PictureConfig) { c.Sources[0].ObservedAt = now.Add(-time.Hour) }, nil)
	ranking, err := decision.NewInspectionRanking(p, e, r)
	if err != nil {
		t.Fatal(err)
	}
	if ranking.Entries()[1].Score == nil {
		t.Fatal("inclusive freshness boundary rejected")
	}
}
func TestAbstentionAndFailureRemainVisible(t *testing.T) {
	for _, status := range []decision.ResultStatus{decision.Failed, decision.Abstained, decision.Completed} {
		t.Run(string(status), func(t *testing.T) {
			p, e, r := rankingFixture(t, nil, func(c *decision.ResultConfig) {
				if status == decision.Completed {
					c.Answers[0] = decision.Answer{SubjectID: "subject1", QuestionID: "priority", Status: decision.AnswerAbstained, Reason: "uncertain"}
				} else {
					c.Status = status
					c.Reason = "unavailable"
					c.Answers = nil
				}
			})
			ranking, err := decision.NewInspectionRanking(p, e, r)
			if err != nil {
				t.Fatal(err)
			}
			for _, entry := range ranking.Entries() {
				if entry.SubjectID == "subject1" && entry.Score != nil {
					t.Fatal("abstention/failure became a score")
				}
			}
		})
	}
}
func TestRankingPolicySubstitutionAndInvalidDefinitions(t *testing.T) {
	p, e, r := rankingFixture(t, nil, nil)
	c := e.Config()
	c.MaxObservationAge = time.Minute
	other, err := decision.NewEvidencePolicy(c)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := decision.NewInspectionRanking(p, other, r); err == nil {
		t.Fatal("substituted evidence policy accepted")
	}
	rc := r.Config()
	rc.Terms[0].Reverse = true
	reverse, err := decision.NewRankingPlan(rc)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := decision.NewInspectionRanking(p, e, reverse); err == nil {
		t.Fatal("substituted ranking plan accepted")
	}
	for _, weight := range []float64{0, -1, math.NaN(), math.Inf(1), 2} {
		c := rankingConfig()
		c.Terms[0].Weight = weight
		if _, err := decision.NewRankingPlan(c); err == nil {
			t.Fatal("invalid weight accepted")
		}
	}
	c = evidenceConfig()
	c.AllowedRoles = append(c.AllowedRoles, c.AllowedRoles[0])
	if _, err := decision.NewEvidencePolicy(c); err == nil {
		t.Fatal("duplicate role accepted")
	}
	c = evidenceConfig()
	c.Measurements[0].MinimumFraction = math.NaN()
	if _, err := decision.NewEvidencePolicy(c); err == nil {
		t.Fatal("NaN requirement accepted")
	}
	if (decision.EvidencePolicy{}).Validate() == nil || (decision.RankingPlan{}).Validate() == nil {
		t.Fatal("zero policy accepted")
	}
}
func TestPolicyCanonicalityAndCopyIsolation(t *testing.T) {
	c := evidenceConfig()
	c.AllowedRoles = append(c.AllowedRoles, decision.Normative)
	c.AllowedControls = append(c.AllowedControls, decision.ExternalControlled)
	c.AuthorityPolicyIDs = append(c.AuthorityPolicyIDs, "authority:2")
	a, err := decision.NewEvidencePolicy(c)
	if err != nil {
		t.Fatal(err)
	}
	c.AllowedRoles[0], c.AllowedRoles[1] = c.AllowedRoles[1], c.AllowedRoles[0]
	c.AllowedControls[0], c.AllowedControls[1] = c.AllowedControls[1], c.AllowedControls[0]
	c.AuthorityPolicyIDs[0], c.AuthorityPolicyIDs[1] = c.AuthorityPolicyIDs[1], c.AuthorityPolicyIDs[0]
	b, err := decision.NewEvidencePolicy(c)
	if err != nil {
		t.Fatal(err)
	}
	if a.ID() != b.ID() {
		t.Fatal("unordered policy changed identity")
	}
	c.AllowedRoles[0] = "invalid"
	out := a.Config()
	out.Measurements[0].Unit = "mutated"
	if err := a.Validate(); err != nil {
		t.Fatal(err)
	}
	rc := rankingConfig()
	r, err := decision.NewRankingPlan(rc)
	if err != nil {
		t.Fatal(err)
	}
	rc.Terms[0], rc.Terms[1] = rc.Terms[1], rc.Terms[0]
	rc.Terms[0].Labels[0], rc.Terms[0].Labels[1] = rc.Terms[0].Labels[1], rc.Terms[0].Labels[0]
	other, err := decision.NewRankingPlan(rc)
	if err != nil {
		t.Fatal(err)
	}
	if r.ID() != other.ID() {
		t.Fatal("unordered ranking plan changed identity")
	}
	rc.Terms[0].Labels[0].Value = 0.8
	outR := r.Config()
	outR.Terms[1].Labels[0].Value = 0.7
	if err := r.Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestEligibilityAvailableBeforeInference(t *testing.T) {
	p, policy, _ := rankingFixture(t, func(c *decision.PictureConfig) { c.Sources[0].ObservedAt = now.Add(-2 * time.Hour) }, nil)
	e, err := decision.NewEvidenceEligibility(p.Request(), policy)
	if err != nil {
		t.Fatal(err)
	}
	if e.RequestID() != p.Request().ID() || len(e.Entries()) != 3 {
		t.Fatal("eligibility lost request or subjects")
	}
	if !reflect.DeepEqual(e.Entries()[0].Reasons, []decision.InspectionReason{decision.SourceStale}) {
		t.Fatal(e.Entries())
	}
	entries := e.Entries()
	entries[0].Reasons[0] = "changed"
	if err := e.Validate(); err != nil {
		t.Fatal(err)
	}
	if (decision.EvidenceEligibility{}).Validate() == nil {
		t.Fatal("zero eligibility accepted")
	}
}
func TestMissingAttestationAndUnsupportedSubjects(t *testing.T) {
	p, e, r := rankingFixture(t, func(c *decision.PictureConfig) {
		c.Subjects[2].Disposition = decision.Unsupported
		c.Subjects[2].Reason = "unresolved dispatch"
	}, nil, func(_ *decision.TaskConfig, e *decision.EvidencePolicyConfig, _ *decision.RankingConfig) {
		e.RequireAttestation = true
	})
	ranking, err := decision.NewInspectionRanking(p, e, r)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range ranking.Entries() {
		if entry.Score != nil {
			t.Fatal("missing attestation allowed scoring")
		}
		found := false
		for _, reason := range entry.Reasons {
			if reason == decision.SourceAttestationMissing {
				found = true
			}
		}
		if !found {
			t.Fatal(entry.Reasons)
		}
	}
	check, err := decision.NewEvidenceEligibility(p.Request(), e)
	if err != nil {
		t.Fatal(err)
	}
	last := check.Entries()[2]
	found := false
	for _, reason := range last.Reasons {
		if reason == decision.SubjectUnsupported {
			found = true
		}
	}
	if !found {
		t.Fatal("unsupported subject disappeared")
	}
}
func TestRankingRejectsUnboundQuestionAndLabelMeanings(t *testing.T) {
	for _, mode := range []string{"unknown question", "missing choice label", "extra ordinal label"} {
		t.Run(mode, func(t *testing.T) {
			p, e, r := rankingFixture(t, nil, nil, func(_ *decision.TaskConfig, _ *decision.EvidencePolicyConfig, r *decision.RankingConfig) {
				switch mode {
				case "unknown question":
					r.Terms[0].QuestionID = "unknown"
				case "missing choice label":
					r.Terms[1].Labels = r.Terms[1].Labels[:1]
				case "extra ordinal label":
					r.Terms[0].Labels = []decision.LabelPriority{{Label: "high", Value: 1}}
				}
			})
			if _, err := decision.NewInspectionRanking(p, e, r); err == nil {
				t.Fatal("invalid task/plan combination accepted")
			}
		})
	}
}
func TestReversedProbabilityAndSubnormalWeights(t *testing.T) {
	p, e, r := rankingFixture(t, nil, func(c *decision.ResultConfig) {
		for i := range c.Answers {
			if c.Answers[i].QuestionID == "priority" {
				v := 0.2
				c.Answers[i].Label = ""
				c.Answers[i].Distribution = nil
				c.Answers[i].Probability = &v
			}
		}
	}, func(tc *decision.TaskConfig, _ *decision.EvidencePolicyConfig, r *decision.RankingConfig) {
		tc.Questions[0].Kind = decision.Probability
		tc.Questions[0].Labels = nil
		r.Terms[0].Reverse = true
		r.Terms[0].Weight = math.SmallestNonzeroFloat64
		r.Terms[1].Weight = math.SmallestNonzeroFloat64
	})
	ranking, err := decision.NewInspectionRanking(p, e, r)
	if err != nil {
		t.Fatal(err)
	}
	if math.Abs(*ranking.Entries()[1].Score-0.9) > 1e-15 {
		t.Fatalf("unexpected probability aggregation %v", *ranking.Entries()[1].Score)
	}
}
func TestFractionThresholdUsesExactDecimalCounts(t *testing.T) {
	p, e, r := rankingFixture(t, func(c *decision.PictureConfig) { c.Measurements[0].Numerator = 9; *c.Measurements[0].Denominator = 10 }, nil, func(_ *decision.TaskConfig, e *decision.EvidencePolicyConfig, _ *decision.RankingConfig) {
		e.Measurements[0].MinimumFraction = 0.9
	})
	ranking, err := decision.NewInspectionRanking(p, e, r)
	if err != nil {
		t.Fatal(err)
	}
	if ranking.Entries()[1].Score == nil {
		t.Fatal("exact 9/10 rejected at 0.9 threshold")
	}
}
