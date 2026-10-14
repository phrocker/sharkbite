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
	"math/big"
	"sort"
	"strconv"
	"time"

	"github.com/phrocker/shoal-oss/pkg/shoal"
)

// MeasurementRequirement binds the meaning as well as the count. A known zero
// denominator or an unknown denominator does not satisfy a coverage requirement.
type MeasurementRequirement struct {
	ID              shoal.ID
	Unit            string
	MethodID        shoal.ID
	MinimumFraction float64
}

// EvidencePolicyConfig is resolved by the trusted task registry. Source role,
// control, authority and attestation references are claims until the service
// verifies their provenance; structural eligibility never establishes trust.
// MaxObservationAge is measured at request time, not snapshot AsOf or receipt
// time, so rereceiving old evidence cannot refresh it.
type EvidencePolicyConfig struct {
	MaxObservationAge  time.Duration
	AllowedRoles       []EvidenceRole
	AllowedControls    []Control
	AuthorityPolicyIDs []shoal.ID
	RequireAttestation bool
	Measurements       []MeasurementRequirement
}
type EvidencePolicy struct {
	id     shoal.ID
	config EvidencePolicyConfig
}

func NewEvidencePolicy(c EvidencePolicyConfig) (EvidencePolicy, error) {
	if c.MaxObservationAge <= 0 || len(c.AllowedRoles) == 0 || len(c.AllowedRoles) > 4 || len(c.AllowedControls) == 0 || len(c.AllowedControls) > 4 || len(c.AuthorityPolicyIDs) == 0 || len(c.AuthorityPolicyIDs) > MaxSources || len(c.Measurements) > MaxMeasurements {
		return EvidencePolicy{}, invalid("invalid evidence policy bounds")
	}
	var b byteBudget
	for _, id := range c.AuthorityPolicyIDs {
		if err := requiredID(id); err != nil {
			return EvidencePolicy{}, err
		}
		if err := b.ids(id); err != nil {
			return EvidencePolicy{}, err
		}
	}
	for _, m := range c.Measurements {
		if err := b.charge(256); err != nil {
			return EvidencePolicy{}, err
		}
		if err := b.ids(m.ID, m.MethodID); err != nil {
			return EvidencePolicy{}, err
		}
		if err := b.text(m.Unit); err != nil {
			return EvidencePolicy{}, err
		}
	}
	c = cloneEvidencePolicy(c)
	sort.Slice(c.AllowedRoles, func(i, j int) bool { return c.AllowedRoles[i] < c.AllowedRoles[j] })
	for i, r := range c.AllowedRoles {
		switch r {
		case Normative, Observation, ReportedOutcome, Prediction:
		default:
			return EvidencePolicy{}, invalid("unknown allowed role")
		}
		if i > 0 && c.AllowedRoles[i-1] == r {
			return EvidencePolicy{}, invalid("duplicate allowed role")
		}
	}
	sort.Slice(c.AllowedControls, func(i, j int) bool { return c.AllowedControls[i] < c.AllowedControls[j] })
	for i, r := range c.AllowedControls {
		switch r {
		case CandidateControlled, ExternalControlled, RegistryControlled, UnknownControl:
		default:
			return EvidencePolicy{}, invalid("unknown allowed control")
		}
		if i > 0 && c.AllowedControls[i-1] == r {
			return EvidencePolicy{}, invalid("duplicate allowed control")
		}
	}
	sort.Slice(c.AuthorityPolicyIDs, func(i, j int) bool { return c.AuthorityPolicyIDs[i] < c.AuthorityPolicyIDs[j] })
	for i := 1; i < len(c.AuthorityPolicyIDs); i++ {
		if c.AuthorityPolicyIDs[i-1] == c.AuthorityPolicyIDs[i] {
			return EvidencePolicy{}, invalid("duplicate authority policy")
		}
	}
	sort.Slice(c.Measurements, func(i, j int) bool { return c.Measurements[i].ID < c.Measurements[j].ID })
	for i := range c.Measurements {
		m := &c.Measurements[i]
		if err := requiredID(m.ID); err != nil {
			return EvidencePolicy{}, err
		}
		if err := requiredID(m.MethodID); err != nil {
			return EvidencePolicy{}, err
		}
		if err := requiredText(m.Unit); err != nil {
			return EvidencePolicy{}, err
		}
		if !finiteRange(m.MinimumFraction, 0, 1) {
			return EvidencePolicy{}, invalid("invalid coverage fraction")
		}
		m.MinimumFraction = positiveZero(m.MinimumFraction)
		if i > 0 && c.Measurements[i-1].ID == m.ID {
			return EvidencePolicy{}, invalid("duplicate coverage requirement")
		}
	}
	id, err := identity("evidence-policy", c)
	if err != nil {
		return EvidencePolicy{}, err
	}
	return EvidencePolicy{id, c}, nil
}
func (p EvidencePolicy) ID() shoal.ID                 { return p.id }
func (p EvidencePolicy) Config() EvidencePolicyConfig { return cloneEvidencePolicy(p.config) }
func (p EvidencePolicy) Validate() error {
	q, err := NewEvidencePolicy(p.config)
	if err != nil {
		return err
	}
	if q.id != p.id {
		return invalid("evidence policy identity mismatch")
	}
	return nil
}
func cloneEvidencePolicy(c EvidencePolicyConfig) EvidencePolicyConfig {
	c.AllowedRoles = append([]EvidenceRole(nil), c.AllowedRoles...)
	c.AllowedControls = append([]Control(nil), c.AllowedControls...)
	c.AuthorityPolicyIDs = append([]shoal.ID(nil), c.AuthorityPolicyIDs...)
	c.Measurements = append([]MeasurementRequirement(nil), c.Measurements...)
	return c
}

// LabelPriority supplies an explicit priority for a nominal choice label.
// Ordinal questions instead follow their ordered task rubric (first=0,last=1).
// Probability questions use their native proposition probability, without a
// calibration claim. All term directions and weights are versioned in the plan.
type LabelPriority struct {
	Label string
	Value float64
}
type RankingTerm struct {
	QuestionID shoal.ID
	Weight     float64
	Reverse    bool
	Labels     []LabelPriority
}
type RankingConfig struct{ Terms []RankingTerm }
type RankingPlan struct {
	id     shoal.ID
	config RankingConfig
}

func NewRankingPlan(c RankingConfig) (RankingPlan, error) {
	if len(c.Terms) == 0 || len(c.Terms) > MaxQuestions {
		return RankingPlan{}, invalid("invalid ranking term count")
	}
	var b byteBudget
	for _, t := range c.Terms {
		if len(t.Labels) > MaxLabels {
			return RankingPlan{}, invalid("too many label priorities")
		}
		if err := b.charge(512); err != nil {
			return RankingPlan{}, err
		}
		if err := b.ids(t.QuestionID); err != nil {
			return RankingPlan{}, err
		}
		for _, l := range t.Labels {
			if err := b.charge(128); err != nil {
				return RankingPlan{}, err
			}
			if err := b.text(l.Label); err != nil {
				return RankingPlan{}, err
			}
		}
	}
	c = cloneRanking(c)
	sort.Slice(c.Terms, func(i, j int) bool { return c.Terms[i].QuestionID < c.Terms[j].QuestionID })
	for i := range c.Terms {
		t := &c.Terms[i]
		if err := requiredID(t.QuestionID); err != nil {
			return RankingPlan{}, err
		}
		if !finiteRange(t.Weight, 0, 1) || t.Weight == 0 {
			return RankingPlan{}, invalid("ranking weight must be positive and at most one")
		}
		if i > 0 && c.Terms[i-1].QuestionID == t.QuestionID {
			return RankingPlan{}, invalid("duplicate ranking term")
		}
		sort.Slice(t.Labels, func(i, j int) bool { return t.Labels[i].Label < t.Labels[j].Label })
		for j := range t.Labels {
			l := &t.Labels[j]
			if err := requiredText(l.Label); err != nil {
				return RankingPlan{}, err
			}
			if !finiteRange(l.Value, 0, 1) {
				return RankingPlan{}, invalid("invalid label priority")
			}
			l.Value = positiveZero(l.Value)
			if j > 0 && t.Labels[j-1].Label == l.Label {
				return RankingPlan{}, invalid("duplicate label priority")
			}
		}
	}
	id, err := identity("ranking-plan", c)
	if err != nil {
		return RankingPlan{}, err
	}
	return RankingPlan{id, c}, nil
}
func (p RankingPlan) ID() shoal.ID          { return p.id }
func (p RankingPlan) Config() RankingConfig { return cloneRanking(p.config) }
func (p RankingPlan) Validate() error {
	q, err := NewRankingPlan(p.config)
	if err != nil {
		return err
	}
	if q.id != p.id {
		return invalid("ranking plan identity mismatch")
	}
	return nil
}
func cloneRanking(c RankingConfig) RankingConfig {
	c.Terms = append([]RankingTerm(nil), c.Terms...)
	for i := range c.Terms {
		c.Terms[i].Labels = append([]LabelPriority(nil), c.Terms[i].Labels...)
	}
	return c
}

// InspectionReason records a conservative fallback, never an authorization
// verdict or proof that a subject is safe. Reasons are finite machine codes.
type InspectionReason string

const (
	EvidenceTruncated        InspectionReason = "evidence_truncated"
	SourceStale              InspectionReason = "source_stale"
	SourceRoleRejected       InspectionReason = "source_role_rejected"
	SourceControlRejected    InspectionReason = "source_control_rejected"
	SourceAuthorityRejected  InspectionReason = "source_authority_rejected"
	SourceAttestationMissing InspectionReason = "source_attestation_missing"
	CoverageUnsatisfied      InspectionReason = "coverage_unsatisfied"
	SubjectUnsupported       InspectionReason = "subject_unsupported"
	SubjectNotEvaluated      InspectionReason = "subject_not_evaluated"
	PredictionFailed         InspectionReason = "prediction_failed"
	PredictionAbstained      InspectionReason = "prediction_abstained"
)

// InspectionEntry has either an ordering score or reasons for ordinary
// inspection. Every picture subject appears, including unrequested subjects.
// A score is a weighted priority, never a probability of safety.
type InspectionEntry struct {
	SubjectID shoal.ID
	Score     *float64
	Reasons   []InspectionReason
}

// InspectionRanking is computed from validated records, never accepted as a
// caller-supplied ordering. Unscored subjects come first, sorted by ID; scored
// subjects follow in descending priority, tied by exact subject ID. No threshold
// or position authorizes exclusion. Evidence comes from the pinned picture;
// the ranking does not duplicate anchors into an unbounded result.
type InspectionRanking struct {
	id         shoal.ID
	prediction PredictionRecord
	policy     EvidencePolicy
	plan       RankingPlan
	entries    []InspectionEntry
}

func NewInspectionRanking(prediction PredictionRecord, policy EvidencePolicy, plan RankingPlan) (InspectionRanking, error) {
	if err := prediction.Validate(); err != nil {
		return InspectionRanking{}, err
	}
	if err := policy.Validate(); err != nil {
		return InspectionRanking{}, err
	}
	if err := plan.Validate(); err != nil {
		return InspectionRanking{}, err
	}
	r := prediction.request
	if r.task.config.EvidencePolicyID != policy.id || r.task.config.AggregationID != plan.id {
		return InspectionRanking{}, invalid("ranking policies do not match task")
	}
	if err := plan.ValidateTask(r.task); err != nil {
		return InspectionRanking{}, err
	}
	questions := map[shoal.ID]Question{}
	for _, q := range r.task.config.Questions {
		questions[q.ID] = q
	}
	eligibility, err := NewEvidenceEligibility(r, policy)
	if err != nil {
		return InspectionRanking{}, err
	}
	eligibilityBySubject := map[shoal.ID][]InspectionReason{}
	for _, e := range eligibility.entries {
		eligibilityBySubject[e.SubjectID] = e.Reasons
	}
	maxWeight := 0.0
	for _, term := range plan.config.Terms {
		if term.Weight > maxWeight {
			maxWeight = term.Weight
		}
	}
	requested := map[shoal.ID]bool{}
	for _, id := range r.config.SubjectIDs {
		requested[id] = true
	}
	answers := map[shoal.ID]map[shoal.ID]Answer{}
	for _, a := range prediction.config.Answers {
		if answers[a.SubjectID] == nil {
			answers[a.SubjectID] = map[shoal.ID]Answer{}
		}
		answers[a.SubjectID][a.QuestionID] = a
	}
	entries := make([]InspectionEntry, 0, len(r.picture.config.Subjects))
	for _, subject := range r.picture.config.Subjects {
		e := InspectionEntry{SubjectID: subject.ID, Reasons: append([]InspectionReason(nil), eligibilityBySubject[subject.ID]...)}
		if !requested[subject.ID] {
			e.Reasons = append(e.Reasons, SubjectNotEvaluated)
		} else {
			switch prediction.config.Status {
			case Failed:
				e.Reasons = append(e.Reasons, PredictionFailed)
			case Abstained:
				e.Reasons = append(e.Reasons, PredictionAbstained)
			}
			// Any abstention, even on a non-ranking question, preserves ordinary review.
			for _, a := range answers[subject.ID] {
				if a.Status == AnswerAbstained {
					e.Reasons = append(e.Reasons, PredictionAbstained)
					break
				}
			}
		}
		if len(e.Reasons) == 0 {
			weighted, total := 0.0, 0.0
			for _, term := range plan.config.Terms {
				a := answers[subject.ID][term.QuestionID]
				q := questions[term.QuestionID]
				value := 0.0
				switch q.Kind {
				case Probability:
					value = *a.Probability
				case Ordinal:
					for i, l := range q.Labels {
						if l == a.Label {
							value = float64(i) / float64(len(q.Labels)-1)
							break
						}
					}
				case Choice:
					for _, l := range term.Labels {
						if l.Label == a.Label {
							value = l.Value
							break
						}
					}
				}
				if term.Reverse {
					value = 1 - value
				}
				// Scaling by the maximum preserves relative weights and avoids
				// underflow when all registered weights are subnormal.
				weight := term.Weight / maxWeight
				weighted += weight * value
				total += weight
			}
			score := positiveZero(weighted / total)
			e.Score = &score
		} else {
			sort.Slice(e.Reasons, func(i, j int) bool { return e.Reasons[i] < e.Reasons[j] })
			e.Reasons = uniqueReasons(e.Reasons)
		}
		entries = append(entries, e)
	}
	sort.Slice(entries, func(i, j int) bool {
		a, b := entries[i], entries[j]
		if (a.Score == nil) != (b.Score == nil) {
			return a.Score == nil
		}
		if a.Score != nil && *a.Score != *b.Score {
			return *a.Score > *b.Score
		}
		return a.SubjectID < b.SubjectID
	})
	payload := struct {
		PredictionID, PolicyID, PlanID shoal.ID
		Entries                        []InspectionEntry
	}{prediction.id, policy.id, plan.id, entries}
	id, err := identity("inspection-ranking", payload)
	if err != nil {
		return InspectionRanking{}, err
	}
	return InspectionRanking{id, prediction, policy, plan, entries}, nil
}
func (x InspectionRanking) ID() shoal.ID           { return x.id }
func (x InspectionRanking) PredictionID() shoal.ID { return x.prediction.id }
func (x InspectionRanking) PictureID() shoal.ID    { return x.prediction.request.picture.id }
func (x InspectionRanking) Entries() []InspectionEntry {
	entries := append([]InspectionEntry(nil), x.entries...)
	for i := range entries {
		entries[i].Reasons = append([]InspectionReason(nil), entries[i].Reasons...)
		if entries[i].Score != nil {
			s := *entries[i].Score
			entries[i].Score = &s
		}
	}
	return entries
}
func (x InspectionRanking) Validate() error {
	y, err := NewInspectionRanking(x.prediction, x.policy, x.plan)
	if err != nil {
		return err
	}
	if x.id != y.id {
		return invalid("ranking identity mismatch")
	}
	return nil
}
func uniqueReasons(reasons []InspectionReason) []InspectionReason {
	out := reasons[:0]
	for _, r := range reasons {
		if len(out) == 0 || out[len(out)-1] != r {
			out = append(out, r)
		}
	}
	return out
}
func evidenceReasons(r DecisionRequest, p EvidencePolicy) []InspectionReason {
	reasons := []InspectionReason{}
	if r.picture.config.Truncated {
		reasons = append(reasons, EvidenceTruncated)
	}
	roles := map[EvidenceRole]bool{}
	for _, v := range p.config.AllowedRoles {
		roles[v] = true
	}
	controls := map[Control]bool{}
	for _, v := range p.config.AllowedControls {
		controls[v] = true
	}
	authorities := map[shoal.ID]bool{}
	for _, v := range p.config.AuthorityPolicyIDs {
		authorities[v] = true
	}
	// All sources can affect the shared predictor input. A disallowed/stale source
	// anywhere in that input invalidates scoring for the whole picture, not just
	// the subject that declares it. Dispositions still remain per subject.
	for _, s := range r.picture.config.Sources {
		if r.config.RequestedAt.After(s.ObservedAt.Add(p.config.MaxObservationAge)) {
			reasons = append(reasons, SourceStale)
		}
		if !roles[s.Role] {
			reasons = append(reasons, SourceRoleRejected)
		}
		if !controls[s.Control] {
			reasons = append(reasons, SourceControlRejected)
		}
		if !authorities[s.AuthorityPolicyID] {
			reasons = append(reasons, SourceAuthorityRejected)
		}
		if p.config.RequireAttestation && s.AttestationID == "" {
			reasons = append(reasons, SourceAttestationMissing)
		}
	}
	measurements := map[shoal.ID]Measurement{}
	for _, m := range r.picture.config.Measurements {
		measurements[m.ID] = m
	}
	for _, requirement := range p.config.Measurements {
		m, ok := measurements[requirement.ID]
		if !ok || m.Unit != requirement.Unit || m.MethodID != requirement.MethodID || m.Denominator == nil || *m.Denominator == 0 || !coverageAtLeast(m.Numerator, *m.Denominator, requirement.MinimumFraction) {
			reasons = append(reasons, CoverageUnsatisfied)
		}
	}
	sort.Slice(reasons, func(i, j int) bool { return reasons[i] < reasons[j] })
	return uniqueReasons(reasons)
}

// Compare exact counts against the decimal fraction recorded by JSON identity.
// Float64 division would round (MaxUint64-1)/MaxUint64 to one and could accept
// incomplete coverage as complete.
func coverageAtLeast(n, d uint64, minimum float64) bool {
	ratio := new(big.Rat).SetFrac(new(big.Int).SetUint64(n), new(big.Int).SetUint64(d))
	threshold, _ := new(big.Rat).SetString(strconv.FormatFloat(minimum, 'g', -1, 64))
	return ratio.Cmp(threshold) >= 0
}

// EligibilityEntry reports structural evidence-policy checks. An empty Reasons
// list does not authenticate provenance, grant access or establish safe behavior.
type EligibilityEntry struct {
	SubjectID shoal.ID
	Reasons   []InspectionReason
}

// EvidenceEligibility is available before inference so a service can avoid
// spending on ineligible evidence. It is also recomputed for ranking. It binds
// request-time freshness and the exact task policy rather than wall-clock replay.
type EvidenceEligibility struct {
	id      shoal.ID
	request DecisionRequest
	policy  EvidencePolicy
	entries []EligibilityEntry
}

func NewEvidenceEligibility(r DecisionRequest, p EvidencePolicy) (EvidenceEligibility, error) {
	if err := r.Validate(); err != nil {
		return EvidenceEligibility{}, err
	}
	if err := p.Validate(); err != nil {
		return EvidenceEligibility{}, err
	}
	if r.task.config.EvidencePolicyID != p.id {
		return EvidenceEligibility{}, invalid("evidence policy does not match task")
	}
	global := evidenceReasons(r, p)
	entries := make([]EligibilityEntry, 0, len(r.picture.config.Subjects))
	for _, s := range r.picture.config.Subjects {
		reasons := append([]InspectionReason(nil), global...)
		if s.Disposition != Supported {
			reasons = append(reasons, SubjectUnsupported)
		}
		sort.Slice(reasons, func(i, j int) bool { return reasons[i] < reasons[j] })
		entries = append(entries, EligibilityEntry{SubjectID: s.ID, Reasons: uniqueReasons(reasons)})
	}
	payload := struct {
		RequestID, PolicyID shoal.ID
		Entries             []EligibilityEntry
	}{r.id, p.id, entries}
	id, err := identity("evidence-eligibility", payload)
	if err != nil {
		return EvidenceEligibility{}, err
	}
	return EvidenceEligibility{id, r, p, entries}, nil
}
func (e EvidenceEligibility) ID() shoal.ID        { return e.id }
func (e EvidenceEligibility) RequestID() shoal.ID { return e.request.id }
func (e EvidenceEligibility) Entries() []EligibilityEntry {
	entries := append([]EligibilityEntry(nil), e.entries...)
	for i := range entries {
		entries[i].Reasons = append([]InspectionReason(nil), entries[i].Reasons...)
	}
	return entries
}
func (e EvidenceEligibility) Validate() error {
	other, err := NewEvidenceEligibility(e.request, e.policy)
	if err != nil {
		return err
	}
	if other.id != e.id {
		return invalid("eligibility identity mismatch")
	}
	return nil
}

// ValidateTask checks the registered aggregation binding and label meanings.
func (p RankingPlan) ValidateTask(task TaskSpec) error {
	if err := p.Validate(); err != nil {
		return err
	}
	if err := task.Validate(); err != nil {
		return err
	}
	if task.config.AggregationID != p.id {
		return invalid("ranking plan does not match task")
	}
	questions := map[shoal.ID]Question{}
	for _, q := range task.config.Questions {
		questions[q.ID] = q
	}
	for _, term := range p.config.Terms {
		q, ok := questions[term.QuestionID]
		if !ok {
			return invalid("ranking question outside task")
		}
		if q.Kind != Choice {
			if len(term.Labels) != 0 {
				return invalid("only choice questions accept label priorities")
			}
			continue
		}
		if len(term.Labels) != len(q.Labels) {
			return invalid("choice ranking requires all task labels")
		}
		for i, l := range term.Labels {
			if l.Label != q.Labels[i] {
				return invalid("choice ranking label mismatch")
			}
		}
	}

	return nil
}
