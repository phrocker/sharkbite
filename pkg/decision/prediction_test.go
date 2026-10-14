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
	"fmt"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/phrocker/shoal-oss/pkg/decision"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

func predictorConfig() decision.PredictorConfig {
	return decision.PredictorConfig{Provider: "local", RuntimeID: "runtime:1", WeightsDigest: strings.Repeat("a", 64), TokenizerDigest: strings.Repeat("b", 64), FormattingID: "format:1", PreprocessingID: "preprocessing:1", CalibrationID: "uncalibrated:1", EnvironmentDigest: strings.Repeat("c", 64), Device: "cpu", Precision: "float64", BatchPolicyID: "batch:1", DistributionTolerance: 0.001, ReplayTolerance: 0.000001}
}
func requestFixture(t *testing.T) (decision.DecisionRequest, decision.ResultConfig) {
	t.Helper()
	pack, p := fixture(t)
	task, err := decision.NewTaskSpec(taskConfig())
	if err != nil {
		t.Fatal(err)
	}
	picture, err := decision.NewPictureManifest(pack, p)
	if err != nil {
		t.Fatal(err)
	}
	predictor, err := decision.NewPredictorIdentity(predictorConfig())
	if err != nil {
		t.Fatal(err)
	}
	r, err := decision.NewDecisionRequest(task, picture, predictor, decision.RequestConfig{PrincipalID: "principal", ReleaseID: "release:1", CorrelationID: "call:1", RequestedAt: now, Deadline: now.Add(time.Minute), SubjectIDs: []shoal.ID{"subject1"}})
	if err != nil {
		t.Fatal(err)
	}
	return r, decision.ResultConfig{RequestID: r.ID(), PredictorID: r.PredictorID(), EffectiveDevice: "cpu", Status: decision.Completed, CompletedAt: now.Add(time.Second), Answers: []decision.Answer{{SubjectID: "subject1", QuestionID: "priority", Status: decision.Answered, Label: "high", Distribution: []decision.LabelProbability{{Label: "low", Probability: 0.25}, {Label: "high", Probability: 0.75}}}, {SubjectID: "subject1", QuestionID: "relevance", Status: decision.Answered, Label: "yes"}}}
}
func TestPredictorIdentity(t *testing.T) {
	c := predictorConfig()
	a, err := decision.NewPredictorIdentity(c)
	if err != nil {
		t.Fatal(err)
	}
	for name, change := range map[string]func(*decision.PredictorConfig){
		"weights":       func(c *decision.PredictorConfig) { c.WeightsDigest = strings.Repeat("d", 64) },
		"preprocessing": func(c *decision.PredictorConfig) { c.PreprocessingID = "preprocessing:2" },
		"runtime":       func(c *decision.PredictorConfig) { c.RuntimeID = "runtime:2" },
		"device":        func(c *decision.PredictorConfig) { c.Device = "gpu:0" },
		"batch":         func(c *decision.PredictorConfig) { c.BatchPolicyID = "batch:2" },
		"calibration":   func(c *decision.PredictorConfig) { c.CalibrationID = "calibrated:1" },
		"rounding":      func(c *decision.PredictorConfig) { c.DistributionTolerance = 0 },
	} {
		t.Run(name, func(t *testing.T) {
			c := a.Config()
			change(&c)
			b, err := decision.NewPredictorIdentity(c)
			if err != nil {
				t.Fatal(err)
			}
			if a.ID() == b.ID() {
				t.Fatal("changed runtime omitted from identity")
			}
		})
	}
	for _, v := range []float64{math.NaN(), math.Inf(1), -1, 0.02} {
		c := a.Config()
		c.DistributionTolerance = v
		if _, err := decision.NewPredictorIdentity(c); err == nil {
			t.Fatal("invalid tolerance accepted")
		}
	}
	c = a.Config()
	c.WeightsDigest = "latest"
	if _, err := decision.NewPredictorIdentity(c); err == nil {
		t.Fatal("alias accepted as weights")
	}
	if (decision.PredictorIdentity{}).Validate() == nil {
		t.Fatal("zero identity accepted")
	}
}
func TestRequestBindingsAndIsolation(t *testing.T) {
	pack, c := fixture(t)
	task, err := decision.NewTaskSpec(taskConfig())
	if err != nil {
		t.Fatal(err)
	}
	p, err := decision.NewPictureManifest(pack, c)
	if err != nil {
		t.Fatal(err)
	}
	model, err := decision.NewPredictorIdentity(predictorConfig())
	if err != nil {
		t.Fatal(err)
	}
	original, _ := requestFixture(t)
	cfg := original.Config()
	copy, err := decision.NewDecisionRequest(task, p, model, cfg)
	if err != nil {
		t.Fatal(err)
	}
	cfg.SubjectIDs[0] = "changed"
	out := copy.Config()
	out.SubjectIDs[0] = "also-changed"
	if err := copy.Validate(); err != nil {
		t.Fatal(err)
	}
	if copy.ID() != original.ID() {
		t.Fatal("same request changed identity")
	}
	for name, change := range map[string]func(*decision.RequestConfig){
		"unknown subject":   func(c *decision.RequestConfig) { c.SubjectIDs = []shoal.ID{"absent"} },
		"duplicate subject": func(c *decision.RequestConfig) { c.SubjectIDs = append(c.SubjectIDs, c.SubjectIDs[0]) },
		"empty subject":     func(c *decision.RequestConfig) { c.SubjectIDs = nil },
		"old request":       func(c *decision.RequestConfig) { c.RequestedAt = now.Add(-time.Second) },
		"expired deadline":  func(c *decision.RequestConfig) { c.Deadline = now },
		"past auth":         func(c *decision.RequestConfig) { c.Deadline = now.Add(2 * time.Hour) },
		"missing principal": func(c *decision.RequestConfig) { c.PrincipalID = "" },
	} {
		t.Run(name, func(t *testing.T) {
			c := original.Config()
			change(&c)
			if _, err := decision.NewDecisionRequest(task, p, model, c); err == nil {
				t.Fatal("invalid request accepted")
			}
		})
	}
	tc := taskConfig()
	tc.Version = "v2"
	other, err := decision.NewTaskSpec(tc)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := decision.NewDecisionRequest(other, p, model, original.Config()); err == nil {
		t.Fatal("substituted task accepted")
	}
	if (decision.DecisionRequest{}).Validate() == nil {
		t.Fatal("zero request accepted")
	}
}
func TestPredictionCanonicalityAndIsolation(t *testing.T) {
	r, c := requestFixture(t)
	a, err := decision.NewPredictionRecord(r, c)
	if err != nil {
		t.Fatal(err)
	}
	c.Answers[0], c.Answers[1] = c.Answers[1], c.Answers[0]
	c.Answers[1].Distribution[0], c.Answers[1].Distribution[1] = c.Answers[1].Distribution[1], c.Answers[1].Distribution[0]
	b, err := decision.NewPredictionRecord(r, c)
	if err != nil {
		t.Fatal(err)
	}
	if a.ID() != b.ID() {
		t.Fatal("response order changed identity")
	}
	c.Answers[1].Distribution[0].Probability = 0
	out := a.Config()
	out.Answers[0].Distribution[0].Probability = 0
	if err := a.Validate(); err != nil {
		t.Fatal(err)
	}
	if a.Config().Answers[0].Distribution[0].Probability != 0.75 {
		t.Fatal("distribution aliases caller")
	}
	if (decision.PredictionRecord{}).Validate() == nil {
		t.Fatal("zero record accepted")
	}
}
func TestPredictionRejectsMalformedResponses(t *testing.T) {
	tests := map[string]func(*decision.ResultConfig){
		"substituted request":          func(c *decision.ResultConfig) { c.RequestID = "another-request" },
		"substituted model":            func(c *decision.ResultConfig) { c.PredictorID = "another-model" },
		"silent GPU fallback":          func(c *decision.ResultConfig) { c.EffectiveDevice = "gpu" },
		"missing answer":               func(c *decision.ResultConfig) { c.Answers = c.Answers[:1] },
		"duplicate pair":               func(c *decision.ResultConfig) { c.Answers[1] = c.Answers[0] },
		"unknown question":             func(c *decision.ResultConfig) { c.Answers[0].QuestionID = "unknown" },
		"unknown subject":              func(c *decision.ResultConfig) { c.Answers[0].SubjectID = "unknown" },
		"unknown label":                func(c *decision.ResultConfig) { c.Answers[0].Label = "safe" },
		"unknown distribution label":   func(c *decision.ResultConfig) { c.Answers[0].Distribution[0].Label = "safe" },
		"duplicate distribution label": func(c *decision.ResultConfig) { c.Answers[0].Distribution[0].Label = "high" },
		"partial distribution":         func(c *decision.ResultConfig) { c.Answers[0].Distribution = c.Answers[0].Distribution[:1] },
		"unnormalized":                 func(c *decision.ResultConfig) { c.Answers[0].Distribution[0].Probability = 0.5 },
		"NaN":                          func(c *decision.ResultConfig) { c.Answers[0].Distribution[0].Probability = math.NaN() },
		"infinity":                     func(c *decision.ResultConfig) { c.Answers[0].Distribution[0].Probability = math.Inf(1) },
		"negative":                     func(c *decision.ResultConfig) { c.Answers[0].Distribution[0].Probability = -0.1 },
		"abstention with score": func(c *decision.ResultConfig) {
			c.Answers[0].Status = decision.AnswerAbstained
			c.Answers[0].Reason = "missing"
		},
		"unknown answer status": func(c *decision.ResultConfig) { c.Answers[0].Status = "safe" },
		"failure with answers":  func(c *decision.ResultConfig) { c.Status = decision.Failed; c.Reason = "timeout" },
		"late success":          func(c *decision.ResultConfig) { c.CompletedAt = now.Add(2 * time.Minute) },
		"before request":        func(c *decision.ResultConfig) { c.CompletedAt = now.Add(-time.Second) },
		"unknown result status": func(c *decision.ResultConfig) { c.Status = "safe" },
	}
	for name, change := range tests {
		t.Run(name, func(t *testing.T) {
			r, c := requestFixture(t)
			change(&c)
			if _, err := decision.NewPredictionRecord(r, c); err == nil {
				t.Fatal("malformed response accepted")
			}
		})
	}
}
func TestFailureAbstentionAndNativeRounding(t *testing.T) {
	r, c := requestFixture(t)
	c.Answers[0].Distribution[0].Probability = 0.2495
	p, err := decision.NewPredictionRecord(r, c)
	if err != nil {
		t.Fatal(err)
	}
	if p.Config().Answers[0].Distribution[1].Probability != 0.2495 {
		t.Fatal("native probability normalized")
	}
	c.Status = decision.Failed
	c.Reason = "timeout"
	c.Answers = nil
	c.CompletedAt = now.Add(2 * time.Hour)
	c.EffectiveDevice = ""
	if _, err := decision.NewPredictionRecord(r, c); err != nil {
		t.Fatal("late failure cannot be recorded:", err)
	}
	c.Status = decision.Abstained
	c.CompletedAt = now.Add(time.Second)
	c.EffectiveDevice = "cpu"
	c.Reason = "unfamiliar input"
	if _, err := decision.NewPredictionRecord(r, c); err != nil {
		t.Fatal(err)
	}
}
func TestProbabilityAndEvidenceAbstention(t *testing.T) {
	pack, pc := fixture(t)
	tc := taskConfig()
	tc.Questions = []decision.Question{{ID: "p", Kind: decision.Probability, RubricID: "p:1"}}
	task, err := decision.NewTaskSpec(tc)
	if err != nil {
		t.Fatal(err)
	}
	pc.TaskID = task.ID()
	model, err := decision.NewPredictorIdentity(predictorConfig())
	if err != nil {
		t.Fatal(err)
	}
	base, _ := requestFixture(t)
	for _, truncated := range []bool{false, true} {
		pc.Truncated = truncated
		p, err := decision.NewPictureManifest(pack, pc)
		if err != nil {
			t.Fatal(err)
		}
		r, err := decision.NewDecisionRequest(task, p, model, base.Config())
		if err != nil {
			t.Fatal(err)
		}
		v := 0.3
		c := decision.ResultConfig{RequestID: r.ID(), PredictorID: model.ID(), EffectiveDevice: "cpu", Status: decision.Completed, CompletedAt: now.Add(time.Second), Answers: []decision.Answer{{SubjectID: "subject1", QuestionID: "p", Status: decision.Answered, Probability: &v}}}
		record, err := decision.NewPredictionRecord(r, c)
		if truncated {
			if err == nil {
				t.Fatal("truncated evidence scored")
			}
			c.Answers[0] = decision.Answer{SubjectID: "subject1", QuestionID: "p", Status: decision.AnswerAbstained, Reason: "truncated input"}
			if _, err := decision.NewPredictionRecord(r, c); err != nil {
				t.Fatal(err)
			}
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
		v = 0.9
		out := record.Config()
		*out.Answers[0].Probability = 0.8
		if *record.Config().Answers[0].Probability != 0.3 {
			t.Fatal("probability aliases caller")
		}
		for _, bad := range []float64{math.NaN(), math.Inf(1), -0.1, 1.1} {
			c := record.Config()
			*c.Answers[0].Probability = bad
			if _, err := decision.NewPredictionRecord(r, c); err == nil {
				t.Fatal("invalid proposition accepted")
			}
		}
	}
}

func TestUnsupportedSubjectAndResponseBounds(t *testing.T) {
	pack, pc := fixture(t)
	pc.Subjects = append(pc.Subjects, decision.Subject{ID: "unsupported", SourceID: "source", Disposition: decision.Unsupported, Reason: "unresolved reference"})
	task, err := decision.NewTaskSpec(taskConfig())
	if err != nil {
		t.Fatal(err)
	}
	picture, err := decision.NewPictureManifest(pack, pc)
	if err != nil {
		t.Fatal(err)
	}
	model, err := decision.NewPredictorIdentity(predictorConfig())
	if err != nil {
		t.Fatal(err)
	}
	original, c := requestFixture(t)
	cfg := original.Config()
	cfg.SubjectIDs = []shoal.ID{"unsupported"}
	r, err := decision.NewDecisionRequest(task, picture, model, cfg)
	if err != nil {
		t.Fatal(err)
	}
	c.RequestID = r.ID()
	for i := range c.Answers {
		c.Answers[i].SubjectID = "unsupported"
	}
	if _, err := decision.NewPredictionRecord(r, c); err == nil {
		t.Fatal("unsupported subject scored")
	}
	for i := range c.Answers {
		c.Answers[i] = decision.Answer{SubjectID: "unsupported", QuestionID: c.Answers[i].QuestionID, Status: decision.AnswerAbstained, Reason: "unresolved reference"}
	}
	if _, err := decision.NewPredictionRecord(r, c); err != nil {
		t.Fatal(err)
	}
	c.Answers = make([]decision.Answer, decision.MaxAnswers+1)
	if _, err := decision.NewPredictionRecord(r, c); err == nil {
		t.Fatal("unbounded response accepted")
	}
}

func TestRequestScopeAndReleaseChangeIdentity(t *testing.T) {
	pack, pc := fixture(t)
	task, err := decision.NewTaskSpec(taskConfig())
	if err != nil {
		t.Fatal(err)
	}
	picture, err := decision.NewPictureManifest(pack, pc)
	if err != nil {
		t.Fatal(err)
	}
	model, err := decision.NewPredictorIdentity(predictorConfig())
	if err != nil {
		t.Fatal(err)
	}
	original, _ := requestFixture(t)
	for name, change := range map[string]func(*decision.RequestConfig){
		"principal":   func(c *decision.RequestConfig) { c.PrincipalID = "another-principal" },
		"release":     func(c *decision.RequestConfig) { c.ReleaseID = "release:2" },
		"correlation": func(c *decision.RequestConfig) { c.CorrelationID = "another-call" },
		"deadline":    func(c *decision.RequestConfig) { c.Deadline = c.Deadline.Add(time.Second) },
	} {
		t.Run(name, func(t *testing.T) {
			c := original.Config()
			change(&c)
			other, err := decision.NewDecisionRequest(task, picture, model, c)
			if err != nil {
				t.Fatal(err)
			}
			if original.ID() == other.ID() {
				t.Fatal("request identity omitted binding")
			}
		})
	}
}

func TestPreprocessingRequiredAndResponseSubstitutionRejected(t *testing.T) {
	cfg := predictorConfig()
	cfg.PreprocessingID = ""
	if _, err := decision.NewPredictorIdentity(cfg); err == nil {
		t.Fatal("missing preprocessing accepted")
	}
	cfg = predictorConfig()
	cfg.PreprocessingID = "different-preprocessing"
	other, err := decision.NewPredictorIdentity(cfg)
	if err != nil {
		t.Fatal(err)
	}
	request, response := requestFixture(t)
	response.PredictorID = other.ID()
	if _, err := decision.NewPredictionRecord(request, response); err == nil {
		t.Fatal("substituted preprocessing accepted")
	}
}

func TestRequestResponseCapacityBoundary(t *testing.T) {
	for _, mode := range []string{"short IDs", "long IDs", "long labels"} {
		t.Run(mode, func(t *testing.T) {
			pack, pc := fixture(t)
			tc := taskConfig()
			tc.Questions = nil
			ids := make([]shoal.ID, 64)
			anchor := pc.Subjects[0].EvidenceIDs[0]
			pc.Subjects = nil
			label := "high"
			if mode == "long labels" {
				label = strings.Repeat("x", shoal.MaxSemanticStringBytes)
			}
			for i := 0; i < 64; i++ {
				subject := fmt.Sprintf("subject-%02d", i)
				question := fmt.Sprintf("question-%02d", i)
				if mode == "long IDs" {
					subject += strings.Repeat("s", shoal.MaxIDBytes-len(subject))
					question += strings.Repeat("q", shoal.MaxIDBytes-len(question))
				}
				ids[i] = shoal.ID(subject)
				pc.Subjects = append(pc.Subjects, decision.Subject{ID: ids[i], SourceID: "source", Disposition: decision.Supported, EvidenceIDs: []shoal.ID{anchor}})
				tc.Questions = append(tc.Questions, decision.Question{ID: shoal.ID(question), Kind: decision.Choice, RubricID: "rubric:1", Labels: []string{"low", label}})
			}
			task, err := decision.NewTaskSpec(tc)
			if err != nil {
				t.Fatal(err)
			}
			pc.TaskID = task.ID()
			picture, err := decision.NewPictureManifest(pack, pc)
			if err != nil {
				t.Fatal(err)
			}
			model, err := decision.NewPredictorIdentity(predictorConfig())
			if err != nil {
				t.Fatal(err)
			}
			baseline, _ := requestFixture(t)
			request := func(n int) (decision.DecisionRequest, error) {
				c := baseline.Config()
				c.SubjectIDs = ids[:n]
				return decision.NewDecisionRequest(task, picture, model, c)
			}
			if _, err := request(64); err == nil {
				t.Fatal("4096-pair unfulfillable request admitted")
			}
			low, high := 0, 64
			for low+1 < high {
				mid := (low + high) / 2
				if _, err := request(mid); err != nil {
					high = mid
				} else {
					low = mid
				}
			}
			if low == 0 {
				t.Fatal("no representable request admitted")
			}
			r, err := request(low)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := request(low + 1); err == nil {
				t.Fatal("accepted request beyond byte capacity")
			}
			result := decision.ResultConfig{RequestID: r.ID(), PredictorID: model.ID(), EffectiveDevice: "cpu", Status: decision.Completed, CompletedAt: now.Add(time.Second)}
			for _, subject := range r.Config().SubjectIDs {
				for _, q := range tc.Questions {
					result.Answers = append(result.Answers, decision.Answer{SubjectID: subject, QuestionID: q.ID, Status: decision.Answered, Label: label})
				}
			}
			if _, err := decision.NewPredictionRecord(r, result); err != nil {
				t.Fatal("admitted boundary cannot complete:", err)
			}
			for i := range result.Answers {
				result.Answers[i].Label = ""
				result.Answers[i].Status = decision.AnswerAbstained
				result.Answers[i].Reason = "unavailable"
			}
			if _, err := decision.NewPredictionRecord(r, result); err != nil {
				t.Fatal("admitted boundary cannot abstain completely:", err)
			}
		})
	}
}
