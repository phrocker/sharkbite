// Licensed under the Apache License, Version 2.0. See LICENSE and NOTICE.
package decision

import (
	"github.com/phrocker/shoal-oss/pkg/shoal"
	"sort"
	"strings"
	"time"
)

// MaxOutcomeAncestors bounds retained correction ancestry across outcome stores
// and adjudication snapshots.
const MaxOutcomeAncestors = 32

// OutcomeKind keeps reported execution success separate from asserted correctness.
type OutcomeKind string

const (
	OutcomeExecution   OutcomeKind = "execution"
	OutcomeCorrectness OutcomeKind = "correctness"
)

type OutcomeExecutionStatus string

const (
	ExecutionSucceeded OutcomeExecutionStatus = "succeeded"
	ExecutionFailed    OutcomeExecutionStatus = "failed"
	ExecutionUnknown   OutcomeExecutionStatus = "unknown"
)

// OutcomeProvenance contains only caller assertions. These references do not
// authenticate a reporter, attest a model run or grant adjudication authority.
type OutcomeProvenance struct{ ReporterID, ModelID, PromptID, ToolID shoal.ID }

// OutcomeObservationConfig is a proposed observation, never a verified label.
// ObservedAt is asserted by the caller; an admitting service must compare it to
// its own authenticated receipt time and verify every contributing evidence ID.
// Supersedes references an existing outcome receipt, not an unreceived proposal.
type OutcomeObservationConfig struct {
	RequestID          shoal.ID
	PredictionID       shoal.ID
	SubjectID          shoal.ID
	Kind               OutcomeKind
	QuestionID         shoal.ID
	Label              string
	Truth              *bool
	ExecutionStatus    OutcomeExecutionStatus
	ActionID           shoal.ID
	EvidenceIDs        []shoal.ID
	ObservedAt         time.Time
	AssertedProvenance OutcomeProvenance
	Supersedes         shoal.ID
}

// OutcomeObservation binds asserted content to a structurally valid prediction.
// Its identity proves content identity only. Construction grants no reporting,
// adjudication or training permission and does not verify any asserted evidence.
type OutcomeObservation struct {
	id         shoal.ID
	prediction PredictionRecord
	config     OutcomeObservationConfig
}

func NewOutcomeObservation(prediction PredictionRecord, c OutcomeObservationConfig) (OutcomeObservation, error) {
	if err := prediction.Validate(); err != nil {
		return OutcomeObservation{}, err
	}
	if c.RequestID != prediction.request.ID() || c.PredictionID != prediction.ID() {
		return OutcomeObservation{}, invalid("outcome prediction/request mismatch")
	}
	if len(c.EvidenceIDs) == 0 || len(c.EvidenceIDs) > MaxSources {
		return OutcomeObservation{}, invalid("invalid outcome evidence count")
	}
	var budget byteBudget
	if err := budget.charge(2048); err != nil {
		return OutcomeObservation{}, err
	}
	if err := budget.ids(c.RequestID, c.PredictionID, c.SubjectID, c.QuestionID, c.ActionID, c.Supersedes, c.AssertedProvenance.ReporterID, c.AssertedProvenance.ModelID, c.AssertedProvenance.PromptID, c.AssertedProvenance.ToolID); err != nil {
		return OutcomeObservation{}, err
	}
	if err := budget.text(c.Label); err != nil {
		return OutcomeObservation{}, err
	}
	if err := budget.ids(c.EvidenceIDs...); err != nil {
		return OutcomeObservation{}, err
	}
	if err := requiredID(c.SubjectID); err != nil {
		return OutcomeObservation{}, err
	}
	subject := false
	for _, id := range prediction.request.config.SubjectIDs {
		subject = subject || id == c.SubjectID
	}
	if !subject {
		return OutcomeObservation{}, invalid("outcome subject outside request")
	}
	for _, id := range []shoal.ID{c.AssertedProvenance.ReporterID, c.AssertedProvenance.ModelID, c.AssertedProvenance.PromptID, c.AssertedProvenance.ToolID} {
		if id != "" {
			if err := requiredID(id); err != nil {
				return OutcomeObservation{}, err
			}
		}
	}
	if c.Supersedes != "" && !validOutcomeReceiptID(c.Supersedes) {
		return OutcomeObservation{}, invalid("invalid superseded outcome receipt")
	}
	c.ObservedAt = utc(c.ObservedAt)
	if !validTime(c.ObservedAt) {
		return OutcomeObservation{}, invalid("invalid asserted observation time")
	}
	switch c.Kind {
	case OutcomeExecution:
		if c.QuestionID != "" || c.Label != "" || c.Truth != nil {
			return OutcomeObservation{}, invalid("execution observation carries correctness")
		}
		if err := requiredID(c.ActionID); err != nil {
			return OutcomeObservation{}, err
		}
		if c.ExecutionStatus != ExecutionSucceeded && c.ExecutionStatus != ExecutionFailed && c.ExecutionStatus != ExecutionUnknown {
			return OutcomeObservation{}, invalid("invalid execution status")
		}
	case OutcomeCorrectness:
		if c.ActionID != "" || c.ExecutionStatus != "" {
			return OutcomeObservation{}, invalid("correctness observation carries execution")
		}
		var question *Question
		for _, q := range prediction.request.task.config.Questions {
			if q.ID == c.QuestionID {
				copy := q
				question = &copy
				break
			}
		}
		if question == nil {
			return OutcomeObservation{}, invalid("outcome question outside task")
		}
		if question.Kind == Probability {
			if c.Label != "" || c.Truth == nil {
				return OutcomeObservation{}, invalid("proposition observation requires truth")
			}
		} else {
			if c.Truth != nil {
				return OutcomeObservation{}, invalid("label observation carries truth")
			}
			label := false
			for _, l := range question.Labels {
				label = label || l == c.Label
			}
			if !label {
				return OutcomeObservation{}, invalid("outcome label outside task")
			}
		}
	default:
		return OutcomeObservation{}, invalid("invalid outcome kind")
	}
	c = cloneOutcome(c)
	sort.Slice(c.EvidenceIDs, func(i, j int) bool { return c.EvidenceIDs[i] < c.EvidenceIDs[j] })
	for i, id := range c.EvidenceIDs {
		if err := requiredID(id); err != nil {
			return OutcomeObservation{}, err
		}
		if i > 0 && c.EvidenceIDs[i-1] == id {
			return OutcomeObservation{}, invalid("duplicate outcome evidence")
		}
	}
	id, err := identity("outcome", c)
	if err != nil {
		return OutcomeObservation{}, err
	}
	return OutcomeObservation{id: id, prediction: prediction, config: c}, nil
}
func validOutcomeReceiptID(id shoal.ID) bool {
	return strings.HasPrefix(string(id), "outcome-receipt:") && digestValid(strings.TrimPrefix(string(id), "outcome-receipt:"))
}
func (o OutcomeObservation) ID() shoal.ID                     { return o.id }
func (o OutcomeObservation) RequestID() shoal.ID              { return o.config.RequestID }
func (o OutcomeObservation) PredictionID() shoal.ID           { return o.config.PredictionID }
func (o OutcomeObservation) TaskID() shoal.ID                 { return o.prediction.request.TaskID() }
func (o OutcomeObservation) PictureID() shoal.ID              { return o.prediction.request.PictureID() }
func (o OutcomeObservation) Config() OutcomeObservationConfig { return cloneOutcome(o.config) }
func (o OutcomeObservation) Validate() error {
	rebuilt, err := NewOutcomeObservation(o.prediction, o.config)
	if err != nil {
		return err
	}
	if rebuilt.id != o.id {
		return invalid("outcome identity mismatch")
	}
	return nil
}
func cloneOutcome(c OutcomeObservationConfig) OutcomeObservationConfig {
	c.EvidenceIDs = append([]shoal.ID(nil), c.EvidenceIDs...)
	if c.Truth != nil {
		value := *c.Truth
		c.Truth = &value
	}
	return c
}
