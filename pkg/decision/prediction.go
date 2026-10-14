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

package decision

import (
	"math"
	"sort"
	"time"

	"github.com/phrocker/shoal-oss/pkg/shoal"
)

const MaxAnswers = 4096

// PredictorConfig pins an effective runtime, not a friendly model alias.
// Referenced artifacts must be resolved and verified by the serving registry.
// CalibrationID may identify a versioned uncalibrated configuration; a valid
// identity makes no claim that returned probabilities are calibrated.
type PredictorConfig struct {
	Provider        string
	RuntimeID       shoal.ID
	WeightsDigest   string
	TokenizerDigest string
	FormattingID    shoal.ID
	// PreprocessingID pins the complete input transformation contract, including
	// normalization, feature extraction, chunking and truncation rules.
	PreprocessingID   shoal.ID
	CalibrationID     shoal.ID
	EnvironmentDigest string
	Device            string
	Precision         string
	BatchPolicyID     shoal.ID
	// DistributionTolerance is the provider's explicit absolute rounding bound.
	// It is capped at 0.01; a task may require a stricter policy. No normalization
	// is performed. ReplayTolerance is separate and never relaxes validation.
	DistributionTolerance float64
	ReplayTolerance       float64
}
type PredictorIdentity struct {
	id     shoal.ID
	config PredictorConfig
}

func NewPredictorIdentity(c PredictorConfig) (PredictorIdentity, error) {
	for _, v := range []string{c.Provider, c.Device, c.Precision} {
		if err := requiredText(v); err != nil {
			return PredictorIdentity{}, err
		}
	}
	for _, id := range []shoal.ID{c.RuntimeID, c.FormattingID, c.PreprocessingID, c.CalibrationID, c.BatchPolicyID} {
		if err := requiredID(id); err != nil {
			return PredictorIdentity{}, err
		}
	}
	for _, d := range []string{c.WeightsDigest, c.TokenizerDigest, c.EnvironmentDigest} {
		if !digestValid(d) {
			return PredictorIdentity{}, invalid("invalid predictor artifact digest")
		}
	}
	if !finiteRange(c.DistributionTolerance, 0, 0.01) || !finiteRange(c.ReplayTolerance, 0, 0.01) {
		return PredictorIdentity{}, invalid("invalid numeric tolerance")
	}
	c.DistributionTolerance = positiveZero(c.DistributionTolerance)
	c.ReplayTolerance = positiveZero(c.ReplayTolerance)
	id, err := identity("predictor", c)
	if err != nil {
		return PredictorIdentity{}, err
	}
	return PredictorIdentity{id, c}, nil
}
func (p PredictorIdentity) ID() shoal.ID            { return p.id }
func (p PredictorIdentity) Config() PredictorConfig { return p.config }
func (p PredictorIdentity) Validate() error {
	q, err := NewPredictorIdentity(p.config)
	if err != nil {
		return err
	}
	if p.id != q.id {
		return invalid("predictor identity mismatch")
	}
	return nil
}

// RequestConfig describes one bounded evaluation. The service authenticates
// PrincipalID and resolves ReleaseID once; caller assertions alone are not
// authority. Idempotency reservation remains a service concern and must bind
// this request identity before inference, never a nondeterministic result.
type RequestConfig struct {
	PrincipalID   shoal.ID
	ReleaseID     shoal.ID
	CorrelationID shoal.ID
	RequestedAt   time.Time
	Deadline      time.Time
	SubjectIDs    []shoal.ID
}
type DecisionRequest struct {
	id        shoal.ID
	task      TaskSpec
	picture   PictureManifest
	predictor PredictorIdentity
	config    RequestConfig
}

func NewDecisionRequest(task TaskSpec, picture PictureManifest, predictor PredictorIdentity, c RequestConfig) (DecisionRequest, error) {
	if err := task.Validate(); err != nil {
		return DecisionRequest{}, err
	}
	if err := picture.Validate(); err != nil {
		return DecisionRequest{}, err
	}
	if err := predictor.Validate(); err != nil {
		return DecisionRequest{}, err
	}
	if task.id != picture.config.TaskID {
		return DecisionRequest{}, invalid("picture task mismatch")
	}
	if len(c.SubjectIDs) == 0 || len(c.SubjectIDs) > MaxSubjects || len(c.SubjectIDs) > MaxAnswers/len(task.config.Questions) {
		return DecisionRequest{}, invalid("request answer count exceeds bound")
	}
	for _, id := range []shoal.ID{c.PrincipalID, c.ReleaseID, c.CorrelationID} {
		if err := requiredID(id); err != nil {
			return DecisionRequest{}, err
		}
	}
	if !validTime(c.RequestedAt) || !validTime(c.Deadline) || c.RequestedAt.Before(picture.config.Cutoff) || !c.Deadline.After(c.RequestedAt) || c.Deadline.After(picture.Authorization().ExpiresAt()) {
		return DecisionRequest{}, invalid("invalid request interval")
	}
	c.RequestedAt = utc(c.RequestedAt)
	c.Deadline = utc(c.Deadline)
	subjects := map[shoal.ID]bool{}
	for _, s := range picture.config.Subjects {
		subjects[s.ID] = true
	}
	// Validate and bound all IDs before copying or hashing.
	var budget byteBudget
	for _, id := range c.SubjectIDs {
		if err := requiredID(id); err != nil {
			return DecisionRequest{}, err
		}
		if err := budget.ids(id); err != nil {
			return DecisionRequest{}, err
		}
		if !subjects[id] {
			return DecisionRequest{}, invalid("request subject outside picture")
		}
	}
	c.SubjectIDs = append([]shoal.ID(nil), c.SubjectIDs...)
	sort.Slice(c.SubjectIDs, func(i, j int) bool { return c.SubjectIDs[i] < c.SubjectIDs[j] })
	for i := 1; i < len(c.SubjectIDs); i++ {
		if c.SubjectIDs[i-1] == c.SubjectIDs[i] {
			return DecisionRequest{}, invalid("duplicate requested subject")
		}
	}
	payload := struct {
		TaskID, PictureID, PredictorID shoal.ID
		Config                         RequestConfig
	}{task.id, picture.id, predictor.id, c}
	id, err := identity("request", payload)
	if err != nil {
		return DecisionRequest{}, err
	}
	request := DecisionRequest{id, task, picture, predictor, c}
	if err := requestResponseBudget(request); err != nil {
		return DecisionRequest{}, err
	}
	return request, nil
}

// Predictor returns the immutable runtime identity bound to this request.
func (r DecisionRequest) Predictor() PredictorIdentity { return r.predictor }

// Task returns the immutable registered task bound to this request.
func (r DecisionRequest) Task() TaskSpec { return r.task }

// Picture returns the immutable evidence picture bound to this request.
func (r DecisionRequest) Picture() PictureManifest { return r.picture }
func (r DecisionRequest) ID() shoal.ID             { return r.id }
func (r DecisionRequest) TaskID() shoal.ID         { return r.task.id }
func (r DecisionRequest) PictureID() shoal.ID      { return r.picture.id }
func (r DecisionRequest) PredictorID() shoal.ID    { return r.predictor.id }
func (r DecisionRequest) Config() RequestConfig {
	c := r.config
	c.SubjectIDs = append([]shoal.ID(nil), c.SubjectIDs...)
	return c
}
func (r DecisionRequest) Validate() error {
	q, err := NewDecisionRequest(r.task, r.picture, r.predictor, r.config)
	if err != nil {
		return err
	}
	if q.id != r.id {
		return invalid("request identity mismatch")
	}
	return nil
}

type ResultStatus string

const (
	Completed ResultStatus = "completed"
	Abstained ResultStatus = "abstained"
	Failed    ResultStatus = "failed"
)

type AnswerStatus string

const (
	Answered        AnswerStatus = "answered"
	AnswerAbstained AnswerStatus = "abstained"
)

type LabelProbability struct {
	Label       string
	Probability float64
}

// Answer must cover exactly one requested subject/question pair. Choice and
// ordinal answers supply a selected task label and optionally all label probabilities;
// proposition answers supply only Probability. Abstentions carry only Reason.
// Selected labels are native provider decisions, not recomputed argmaxes.
type Answer struct {
	SubjectID    shoal.ID
	QuestionID   shoal.ID
	Status       AnswerStatus
	Label        string
	Distribution []LabelProbability
	Probability  *float64
	Reason       string
}

// ResultConfig is a provider response claim until a trusted executor attests it.
// Successful responses must name the exact effective predictor and device.
// Whole-request abstention/failure has no partial answers. Individual abstention
// is supported in Completed results, preserving exact membership for every pair.
type ResultConfig struct {
	RequestID       shoal.ID
	PredictorID     shoal.ID
	EffectiveDevice string
	Status          ResultStatus
	Reason          string
	CompletedAt     time.Time
	Answers         []Answer
}
type PredictionRecord struct {
	id      shoal.ID
	request DecisionRequest
	config  ResultConfig
}

func NewPredictionRecord(request DecisionRequest, c ResultConfig) (PredictionRecord, error) {
	if err := request.Validate(); err != nil {
		return PredictionRecord{}, err
	}
	if c.RequestID != request.id || c.PredictorID != request.predictor.id {
		return PredictionRecord{}, invalid("response request/predictor mismatch")
	}
	if !validTime(c.CompletedAt) || c.CompletedAt.Before(request.config.RequestedAt) {
		return PredictionRecord{}, invalid("invalid response time")
	}
	if c.Status != Failed && (!c.CompletedAt.Before(request.picture.Authorization().ExpiresAt()) || c.CompletedAt.After(request.config.Deadline)) {
		return PredictionRecord{}, invalid("response outside deadline/authorization interval")
	}
	c.CompletedAt = utc(c.CompletedAt)
	if len(c.Answers) > MaxAnswers {
		return PredictionRecord{}, invalid("too many answers")
	}
	if c.EffectiveDevice != request.predictor.config.Device && !(c.Status == Failed && c.EffectiveDevice == "") {
		return PredictionRecord{}, invalid("unregistered effective device")
	}
	if _, err := responseBudget(c); err != nil {
		return PredictionRecord{}, err
	}

	switch c.Status {
	case Failed, Abstained:
		if len(c.Answers) != 0 {
			return PredictionRecord{}, invalid("non-completed response carries answers")
		}
		if err := requiredText(c.Reason); err != nil {
			return PredictionRecord{}, err
		}
	case Completed:
		if c.Reason != "" {
			return PredictionRecord{}, invalid("completed response carries failure reason")
		}
		if len(c.Answers) != len(request.config.SubjectIDs)*len(request.task.config.Questions) {
			return PredictionRecord{}, invalid("incomplete answer membership")
		}
	default:
		return PredictionRecord{}, invalid("unknown result status")
	}
	c = cloneResult(c)
	if c.Status == Completed {
		questions := map[shoal.ID]Question{}
		for _, q := range request.task.config.Questions {
			questions[q.ID] = q
		}
		subjects := map[shoal.ID]bool{}
		for _, id := range request.config.SubjectIDs {
			subjects[id] = true
		}
		supported := map[shoal.ID]bool{}
		for _, s := range request.picture.config.Subjects {
			supported[s.ID] = s.Disposition == Supported
		}
		sort.Slice(c.Answers, func(i, j int) bool {
			if c.Answers[i].SubjectID == c.Answers[j].SubjectID {
				return c.Answers[i].QuestionID < c.Answers[j].QuestionID
			}
			return c.Answers[i].SubjectID < c.Answers[j].SubjectID
		})
		for i := range c.Answers {
			a := &c.Answers[i]
			q, ok := questions[a.QuestionID]
			if !ok || !subjects[a.SubjectID] {
				return PredictionRecord{}, invalid("answer outside requested membership")
			}
			if i > 0 && c.Answers[i-1].SubjectID == a.SubjectID && c.Answers[i-1].QuestionID == a.QuestionID {
				return PredictionRecord{}, invalid("duplicate answer pair")
			}
			if a.Status == AnswerAbstained {
				if a.Label != "" || len(a.Distribution) != 0 || a.Probability != nil {
					return PredictionRecord{}, invalid("abstention carries prediction")
				}
				if err := requiredText(a.Reason); err != nil {
					return PredictionRecord{}, err
				}
				continue
			}
			if a.Status != Answered || a.Reason != "" {
				return PredictionRecord{}, invalid("invalid answer disposition")
			}
			if !supported[a.SubjectID] || request.picture.config.Truncated {
				return PredictionRecord{}, invalid("ineligible evidence requires abstention")
			}
			if q.Kind == Probability {
				if a.Label != "" || len(a.Distribution) != 0 || a.Probability == nil || !finiteRange(*a.Probability, 0, 1) {
					return PredictionRecord{}, invalid("invalid proposition probability")
				}
				*a.Probability = positiveZero(*a.Probability)
				continue
			}
			if a.Probability != nil || (len(a.Distribution) != 0 && len(a.Distribution) != len(q.Labels)) {
				return PredictionRecord{}, invalid("invalid label distribution")
			}
			labels := map[string]bool{}
			for _, l := range q.Labels {
				labels[l] = true
			}
			if !labels[a.Label] {
				return PredictionRecord{}, invalid("selected label outside task")
			}
			if len(a.Distribution) == 0 {
				continue
			}
			sort.Slice(a.Distribution, func(i, j int) bool { return a.Distribution[i].Label < a.Distribution[j].Label })
			sum := 0.0
			for j := range a.Distribution {
				d := &a.Distribution[j]
				if !labels[d.Label] || (j > 0 && a.Distribution[j-1].Label == d.Label) || !finiteRange(d.Probability, 0, 1) {
					return PredictionRecord{}, invalid("invalid distribution member")
				}
				d.Probability = positiveZero(d.Probability)
				sum += d.Probability
			}
			if math.Abs(sum-1) > request.predictor.config.DistributionTolerance {
				return PredictionRecord{}, invalid("distribution outside rounding tolerance")
			}
		}
	}
	id, err := identity("prediction", c)
	if err != nil {
		return PredictionRecord{}, err
	}
	return PredictionRecord{id, request, c}, nil
}

// Request returns the immutable request bound to this prediction.
func (p PredictionRecord) Request() DecisionRequest { return p.request }
func (p PredictionRecord) ID() shoal.ID             { return p.id }
func (p PredictionRecord) Config() ResultConfig     { return cloneResult(p.config) }
func (p PredictionRecord) Validate() error {
	q, err := NewPredictionRecord(p.request, p.config)
	if err != nil {
		return err
	}
	if q.id != p.id {
		return invalid("prediction identity mismatch")
	}
	return nil
}
func cloneResult(c ResultConfig) ResultConfig {
	c.Answers = append([]Answer(nil), c.Answers...)
	for i := range c.Answers {
		c.Answers[i].Distribution = append([]LabelProbability(nil), c.Answers[i].Distribution...)
		if c.Answers[i].Probability != nil {
			p := *c.Answers[i].Probability
			c.Answers[i].Probability = &p
		}
	}
	return c
}
func finiteRange(v, low, high float64) bool {
	return !math.IsNaN(v) && !math.IsInf(v, 0) && v >= low && v <= high
}
func positiveZero(v float64) float64 {
	if v == 0 {
		return 0
	}
	return v
}

// Shared accounting keeps request admission and result preflight consistent.
func responseHeaderBudget(c ResultConfig) (byteBudget, error) {
	var b byteBudget
	if err := b.charge(4096); err != nil {
		return 0, err
	}
	if err := b.ids(c.RequestID, c.PredictorID); err != nil {
		return 0, err
	}
	for _, s := range []string{c.EffectiveDevice, string(c.Status), c.Reason} {
		if err := b.text(s); err != nil {
			return 0, err
		}
	}
	return b, nil
}
func answerBudget(a Answer) (byteBudget, error) {
	var b byteBudget
	if len(a.Distribution) > MaxLabels {
		return 0, invalid("too many distribution labels")
	}
	if err := b.charge(1024); err != nil {
		return 0, err
	}
	if err := b.ids(a.SubjectID, a.QuestionID); err != nil {
		return 0, err
	}
	for _, s := range []string{a.Label, a.Reason, string(a.Status)} {
		if err := b.text(s); err != nil {
			return 0, err
		}
	}
	for _, d := range a.Distribution {
		if err := b.charge(128); err != nil {
			return 0, err
		}
		if err := b.text(d.Label); err != nil {
			return 0, err
		}
	}
	return b, nil
}
func responseBudget(c ResultConfig) (byteBudget, error) {
	b, err := responseHeaderBudget(c)
	if err != nil {
		return 0, err
	}
	for _, a := range c.Answers {
		size, err := answerBudget(a)
		if err != nil {
			return 0, err
		}
		if err := b.charge(int(size)); err != nil {
			return 0, err
		}
	}
	return b, nil
}

// Reserve room for every pair's label-only/proposition answer, or an abstention
// with reason "unavailable", whichever costs more. Long task labels and repeated
// subject/question IDs count for each pair. Optional distributions and longer
// reasons remain subject to actual response preflight; admission does not promise
// that every optional representation fits.
func requestResponseBudget(r DecisionRequest) error {
	b, err := responseHeaderBudget(ResultConfig{RequestID: r.id, PredictorID: r.predictor.id, EffectiveDevice: r.predictor.config.Device, Status: Completed})
	if err != nil {
		return err
	}
	for _, subject := range r.config.SubjectIDs {
		for _, q := range r.task.config.Questions {
			label := ""
			for _, candidate := range q.Labels {
				if len(candidate) > len(label) {
					label = candidate
				}
			}
			size, err := answerBudget(Answer{SubjectID: subject, QuestionID: q.ID, Status: Answered, Label: label})
			if err != nil {
				return err
			}
			abstain, err := answerBudget(Answer{SubjectID: subject, QuestionID: q.ID, Status: AnswerAbstained, Reason: "unavailable"})
			if err != nil {
				return err
			}
			if abstain > size {
				size = abstain
			}
			if err := b.charge(int(size)); err != nil {
				return invalid("request cannot fit a complete response")
			}
		}
	}
	return nil
}
