// Licensed under the Apache License, Version 2.0. See LICENSE and NOTICE.
package decision

import (
	"github.com/phrocker/shoal-oss/pkg/shoal"
	"sort"
	"strings"
)

const (
	MaxAdjudicationObservations = 256
	MaxAdjudicationWitnesses    = 1024
)

// LabelPolicyConfig references roles and a purpose in a trusted registry. IDs
// alone neither establish registration nor prove membership or independence.
// Reporter/source-controller separation is mandatory in the admitting service;
// this contract deliberately provides no switch to disable it.
type LabelPolicyConfig struct {
	OwnerID                 shoal.ID
	Version                 string
	AdjudicatorRoleID       shoal.ID
	DisputeResolverRoleID   shoal.ID
	TrainingPurposeID       shoal.ID
	MinIndependentWitnesses int
}
type LabelPolicy struct {
	id     shoal.ID
	config LabelPolicyConfig
}

func NewLabelPolicy(c LabelPolicyConfig) (LabelPolicy, error) {
	for _, id := range []shoal.ID{c.OwnerID, c.AdjudicatorRoleID, c.DisputeResolverRoleID, c.TrainingPurposeID} {
		if err := requiredID(id); err != nil {
			return LabelPolicy{}, err
		}
	}
	if err := requiredText(c.Version); err != nil {
		return LabelPolicy{}, err
	}
	if c.MinIndependentWitnesses < 1 || c.MinIndependentWitnesses > 64 {
		return LabelPolicy{}, invalid("invalid independent witness requirement")
	}
	id, err := identity("label-policy", c)
	if err != nil {
		return LabelPolicy{}, err
	}
	return LabelPolicy{id, c}, nil
}
func (p LabelPolicy) ID() shoal.ID              { return p.id }
func (p LabelPolicy) Config() LabelPolicyConfig { return p.config }
func (p LabelPolicy) Validate() error {
	rebuilt, err := NewLabelPolicy(p.config)
	if err != nil {
		return err
	}
	if rebuilt.id != p.id {
		return invalid("label policy identity mismatch")
	}
	return nil
}

type AdjudicationDisposition string

const (
	AdjudicationVerified   AdjudicationDisposition = "verified"
	AdjudicationDisputed   AdjudicationDisposition = "disputed"
	AdjudicationUnresolved AdjudicationDisposition = "unresolved"
)

// AdjudicationProposalConfig describes a requested disposition, not its approval.
// In particular, "verified" is only a proposal until an authorized service
// independently verifies evidence, roles, conflicts, lineage and receipt times.
// Witness IDs are references, never proof that evidence origins are independent.
type AdjudicationProposalConfig struct {
	RequestID             shoal.ID
	PredictionID          shoal.ID
	SubjectID             shoal.ID
	QuestionID            shoal.ID
	ObservationReceiptIDs []shoal.ID
	WitnessIDs            []shoal.ID
	Disposition           AdjudicationDisposition
	Label                 string
	Truth                 *bool
	Reason                string
	ExpectedHeadID        shoal.ID
	ExpectedVersion       int64
}

// AdjudicationProposal is immutable asserted content. It contains no authenticated
// adjudicator, receipt timestamp, approved label or training-eligibility grant.
// ExpectedHeadID/ExpectedVersion must be checked atomically by a future journal;
// structural construction cannot establish that a predecessor exists or is current.
type AdjudicationProposal struct {
	id         shoal.ID
	targetID   shoal.ID
	policy     LabelPolicy
	prediction PredictionRecord
	config     AdjudicationProposalConfig
}

func NewAdjudicationProposal(policy LabelPolicy, prediction PredictionRecord, c AdjudicationProposalConfig) (AdjudicationProposal, error) {
	if err := policy.Validate(); err != nil {
		return AdjudicationProposal{}, err
	}
	if err := prediction.Validate(); err != nil {
		return AdjudicationProposal{}, err
	}
	request := prediction.request
	if request.task.config.LabelPolicyID != policy.ID() {
		return AdjudicationProposal{}, invalid("task label policy mismatch")
	}
	if c.RequestID != request.ID() || c.PredictionID != prediction.ID() {
		return AdjudicationProposal{}, invalid("adjudication prediction/request mismatch")
	}
	if len(c.ObservationReceiptIDs) == 0 || len(c.ObservationReceiptIDs) > MaxAdjudicationObservations || len(c.WitnessIDs) > MaxAdjudicationWitnesses {
		return AdjudicationProposal{}, invalid("invalid adjudication reference counts")
	}
	var budget byteBudget
	if err := budget.charge(4096); err != nil {
		return AdjudicationProposal{}, err
	}
	if err := budget.ids(c.RequestID, c.PredictionID, c.SubjectID, c.QuestionID, c.ExpectedHeadID); err != nil {
		return AdjudicationProposal{}, err
	}
	if err := budget.ids(c.ObservationReceiptIDs...); err != nil {
		return AdjudicationProposal{}, err
	}
	if err := budget.ids(c.WitnessIDs...); err != nil {
		return AdjudicationProposal{}, err
	}
	for _, text := range []string{c.Label, c.Reason, string(c.Disposition)} {
		if text != "" {
			if err := requiredText(text); err != nil {
				return AdjudicationProposal{}, err
			}
		}
		if err := budget.text(text); err != nil {
			return AdjudicationProposal{}, err
		}
	}
	subject := false
	for _, id := range request.config.SubjectIDs {
		subject = subject || id == c.SubjectID
	}
	if !subject {
		return AdjudicationProposal{}, invalid("adjudication subject outside request")
	}
	var question *Question
	for _, q := range request.task.config.Questions {
		if q.ID == c.QuestionID {
			copy := q
			question = &copy
			break
		}
	}
	if question == nil {
		return AdjudicationProposal{}, invalid("adjudication question outside task")
	}
	if c.ExpectedHeadID == "" {
		if c.ExpectedVersion != 0 {
			return AdjudicationProposal{}, invalid("head version without predecessor")
		}
	} else if c.ExpectedVersion <= 0 || !adjudicationReceiptReference(c.ExpectedHeadID, "adjudication-receipt:") {
		return AdjudicationProposal{}, invalid("invalid adjudication predecessor")
	}
	switch c.Disposition {
	case AdjudicationVerified:
		if c.Reason != "" || len(c.WitnessIDs) < policy.config.MinIndependentWitnesses {
			return AdjudicationProposal{}, invalid("invalid proposed verification")
		}
		if question.Kind == Probability {
			if c.Label != "" || c.Truth == nil {
				return AdjudicationProposal{}, invalid("proposition adjudication requires truth")
			}
		} else {
			if c.Truth != nil {
				return AdjudicationProposal{}, invalid("label adjudication carries truth")
			}
			valid := false
			for _, label := range question.Labels {
				valid = valid || label == c.Label
			}
			if !valid {
				return AdjudicationProposal{}, invalid("adjudication label outside task")
			}
		}
	case AdjudicationDisputed, AdjudicationUnresolved:
		if c.Label != "" || c.Truth != nil {
			return AdjudicationProposal{}, invalid("unverified proposal carries label")
		}
		if err := requiredText(c.Reason); err != nil {
			return AdjudicationProposal{}, err
		}
		if c.Disposition == AdjudicationDisputed && len(c.WitnessIDs) == 0 {
			return AdjudicationProposal{}, invalid("dispute requires witness reference")
		}
	default:
		return AdjudicationProposal{}, invalid("invalid adjudication disposition")
	}
	c = cloneAdjudication(c)
	for _, references := range []struct {
		ids    []shoal.ID
		prefix string
	}{{c.ObservationReceiptIDs, "outcome-receipt:"}, {c.WitnessIDs, ""}} {
		sort.Slice(references.ids, func(i, j int) bool { return references.ids[i] < references.ids[j] })
		for i, id := range references.ids {
			if err := requiredID(id); err != nil {
				return AdjudicationProposal{}, err
			}
			if references.prefix != "" && !adjudicationReceiptReference(id, references.prefix) {
				return AdjudicationProposal{}, invalid("invalid outcome receipt reference")
			}
			if i > 0 && references.ids[i-1] == id {
				return AdjudicationProposal{}, invalid("duplicate adjudication reference")
			}
		}
	}
	targetID, err := AdjudicationTargetID(request.TaskID(), request.PictureID(), c.SubjectID, c.QuestionID)
	if err != nil {
		return AdjudicationProposal{}, err
	}
	id, err := AdjudicationProposalID(policy.ID(), targetID, c)
	if err != nil {
		return AdjudicationProposal{}, err
	}
	return AdjudicationProposal{id, targetID, policy, prediction, c}, nil
}
func adjudicationReceiptReference(id shoal.ID, prefix string) bool {
	return strings.HasPrefix(string(id), prefix) && digestValid(strings.TrimPrefix(string(id), prefix))
}
func (a AdjudicationProposal) ID() shoal.ID                       { return a.id }
func (a AdjudicationProposal) TargetID() shoal.ID                 { return a.targetID }
func (a AdjudicationProposal) TaskID() shoal.ID                   { return a.prediction.request.TaskID() }
func (a AdjudicationProposal) PictureID() shoal.ID                { return a.prediction.request.PictureID() }
func (a AdjudicationProposal) PolicyID() shoal.ID                 { return a.policy.ID() }
func (a AdjudicationProposal) Config() AdjudicationProposalConfig { return cloneAdjudication(a.config) }
func (a AdjudicationProposal) Validate() error {
	rebuilt, err := NewAdjudicationProposal(a.policy, a.prediction, a.config)
	if err != nil {
		return err
	}
	if rebuilt.id != a.id || rebuilt.targetID != a.targetID {
		return invalid("adjudication proposal identity mismatch")
	}
	return nil
}
func cloneAdjudication(c AdjudicationProposalConfig) AdjudicationProposalConfig {
	c.ObservationReceiptIDs = append([]shoal.ID(nil), c.ObservationReceiptIDs...)
	c.WitnessIDs = append([]shoal.ID(nil), c.WitnessIDs...)
	if c.Truth != nil {
		value := *c.Truth
		c.Truth = &value
	}
	return c
}

// AdjudicationTargetID hashes the bounded target metadata used by durable journals.
// This establishes content identity only, not registration or authorization.
func AdjudicationTargetID(taskID, pictureID, subjectID, questionID shoal.ID) (shoal.ID, error) {
	for _, id := range []shoal.ID{taskID, pictureID, subjectID, questionID} {
		if err := requiredID(id); err != nil {
			return "", err
		}
	}
	return identity("adjudication-target", struct{ TaskID, PictureID, SubjectID, QuestionID shoal.ID }{taskID, pictureID, subjectID, questionID})
}

// AdjudicationProposalID hashes bounded, canonical proposal metadata for storage
// integrity checks. It does NOT validate task-specific labels, policy witness
// requirements, or permissions. Reconstruct with NewAdjudicationProposal and
// trusted policy/prediction records before treating stored metadata as a proposal.
func AdjudicationProposalID(policyID, targetID shoal.ID, c AdjudicationProposalConfig) (shoal.ID, error) {
	for _, id := range []shoal.ID{policyID, targetID, c.RequestID, c.PredictionID, c.SubjectID, c.QuestionID} {
		if err := requiredID(id); err != nil {
			return "", err
		}
	}
	if len(c.ObservationReceiptIDs) == 0 || len(c.ObservationReceiptIDs) > MaxAdjudicationObservations || len(c.WitnessIDs) > MaxAdjudicationWitnesses {
		return "", invalid("invalid adjudication reference counts")
	}
	var budget byteBudget
	if err := budget.charge(4096); err != nil {
		return "", err
	}
	if err := budget.ids(policyID, targetID, c.RequestID, c.PredictionID, c.SubjectID, c.QuestionID, c.ExpectedHeadID); err != nil {
		return "", err
	}
	if err := budget.ids(c.ObservationReceiptIDs...); err != nil {
		return "", err
	}
	if err := budget.ids(c.WitnessIDs...); err != nil {
		return "", err
	}
	for _, text := range []string{c.Label, c.Reason, string(c.Disposition)} {
		if text != "" {
			if err := requiredText(text); err != nil {
				return "", err
			}
		}
		if err := budget.text(text); err != nil {
			return "", err
		}
	}
	if c.ExpectedHeadID == "" {
		if c.ExpectedVersion != 0 {
			return "", invalid("head version without predecessor")
		}
	} else if c.ExpectedVersion <= 0 || !adjudicationReceiptReference(c.ExpectedHeadID, "adjudication-receipt:") {
		return "", invalid("invalid adjudication predecessor")
	}
	switch c.Disposition {
	case AdjudicationVerified, AdjudicationDisputed, AdjudicationUnresolved:
	default:
		return "", invalid("invalid adjudication disposition")
	}
	c = cloneAdjudication(c)
	for _, references := range []struct {
		ids    []shoal.ID
		prefix string
	}{{c.ObservationReceiptIDs, "outcome-receipt:"}, {c.WitnessIDs, ""}} {
		sort.Slice(references.ids, func(i, j int) bool { return references.ids[i] < references.ids[j] })
		for i, id := range references.ids {
			if err := requiredID(id); err != nil {
				return "", err
			}
			if references.prefix != "" && !adjudicationReceiptReference(id, references.prefix) {
				return "", invalid("invalid outcome receipt reference")
			}
			if i > 0 && references.ids[i-1] == id {
				return "", invalid("duplicate adjudication reference")
			}
		}
	}
	return identity("adjudication-proposal", struct {
		PolicyID, TargetID shoal.ID
		Config             AdjudicationProposalConfig
	}{policyID, targetID, c})
}
