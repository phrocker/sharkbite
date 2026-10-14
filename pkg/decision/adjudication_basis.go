// Licensed under the Apache License, Version 2.0. See LICENSE and NOTICE.
package decision

import (
	"bytes"
	"encoding/json"
	"sort"
	"time"

	"github.com/phrocker/shoal-oss/pkg/explorer/auth"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

const MaxBasisIdentities = 1024

// BasisIdentity preserves opaque authentication bytes. It describes a claimed
// identity; only trusted authority resolution can authenticate it. Delegation
// order is preserved, because OnBehalfOf is a chain rather than an unordered set.
type BasisIdentity struct {
	SubjectID  []byte
	ActorID    []byte
	ClientID   []byte
	OnBehalfOf [][]byte
}
type BasisOutcome struct {
	ReceiptID     shoal.ID
	ObservationID shoal.ID
	RequestID     shoal.ID
	PredictionID  shoal.ID
	TaskID        shoal.ID
	PictureID     shoal.ID
	SubjectID     shoal.ID
	QuestionID    shoal.ID
	Kind          OutcomeKind
	Reporter      BasisIdentity
	ReceivedAt    time.Time
	Supersedes    shoal.ID
}

// BasisWitness preserves common provenance even when several references share
// an origin group. Distinct IDs do not establish independent verification.
type BasisWitness struct {
	ID                    shoal.ID
	Digest                string
	VerificationReceiptID shoal.ID
	OriginGroupID         shoal.ID
	Origin                BasisIdentity
	ControllerIdentities  []BasisIdentity
	ReceivedAt            time.Time
	VerifiedAt            time.Time
}

// AdjudicationBasisConfig is a content snapshot, not an authority grant.
// InventoryComplete and ControllersComplete are assertions whose truth must be
// established by a trusted collector. Matching proposal references alone cannot
// establish that no other reports, controllers or conflicts exist.
type AdjudicationBasisConfig struct {
	AuthorityID         shoal.ID
	AuthorityRevisionID shoal.ID
	EnumerationID       shoal.ID
	CapturedAt          time.Time
	Cutoff              time.Time
	InventoryComplete   bool
	ControllersComplete bool
	Outcomes            []BasisOutcome
	Witnesses           []BasisWitness
	SourceControllers   []BasisIdentity
	RoleEvidenceIDs     []shoal.ID
}

// AdjudicationBasis binds claimed provenance and availability to a proposal.
// Construction validates structure only: no receipt is authenticated, no role
// membership or independence is established, and no training use is authorized.
type AdjudicationBasis struct {
	id       shoal.ID
	proposal AdjudicationProposal
	config   AdjudicationBasisConfig
}

func NewAdjudicationBasis(proposal AdjudicationProposal, c AdjudicationBasisConfig) (AdjudicationBasis, error) {
	if err := proposal.Validate(); err != nil {
		return AdjudicationBasis{}, err
	}
	if err := basisPreflight(c); err != nil {
		return AdjudicationBasis{}, err
	}
	for _, id := range []shoal.ID{c.AuthorityID, c.AuthorityRevisionID, c.EnumerationID} {
		if err := requiredID(id); err != nil {
			return AdjudicationBasis{}, err
		}
	}
	c.CapturedAt = utc(c.CapturedAt)
	c.Cutoff = utc(c.Cutoff)
	if !validTime(c.CapturedAt) || !validTime(c.Cutoff) || c.CapturedAt.Before(c.Cutoff) {
		return AdjudicationBasis{}, invalid("invalid basis capture/cutoff")
	}
	if !c.InventoryComplete {
		return AdjudicationBasis{}, invalid("basis inventory completeness claim required")
	}
	if proposal.config.Disposition == AdjudicationVerified && !c.ControllersComplete {
		return AdjudicationBasis{}, invalid("verification requires controller completeness claim")
	}
	c = cloneBasis(c)
	sort.Slice(c.Outcomes, func(i, j int) bool { return c.Outcomes[i].ReceiptID < c.Outcomes[j].ReceiptID })
	if len(c.Outcomes) != len(proposal.config.ObservationReceiptIDs) {
		return AdjudicationBasis{}, invalid("basis outcome set mismatch")
	}
	for i := range c.Outcomes {
		o := &c.Outcomes[i]
		if o.ReceiptID != proposal.config.ObservationReceiptIDs[i] || !adjudicationReceiptReference(o.ReceiptID, "outcome-receipt:") {
			return AdjudicationBasis{}, invalid("basis outcome set mismatch")
		}
		for _, id := range []shoal.ID{o.ObservationID, o.RequestID, o.PredictionID, o.TaskID, o.PictureID, o.SubjectID, o.QuestionID} {
			if err := requiredID(id); err != nil {
				return AdjudicationBasis{}, err
			}
		}
		if o.Kind != OutcomeCorrectness || o.TaskID != proposal.TaskID() || o.PictureID != proposal.PictureID() || o.SubjectID != proposal.config.SubjectID || o.QuestionID != proposal.config.QuestionID {
			return AdjudicationBasis{}, invalid("basis outcome outside correctness target")
		}
		if o.Supersedes != "" && (!adjudicationReceiptReference(o.Supersedes, "outcome-receipt:") || o.Supersedes == o.ReceiptID) {
			return AdjudicationBasis{}, invalid("invalid basis supersession reference")
		}
		o.ReceivedAt = utc(o.ReceivedAt)
		if !validTime(o.ReceivedAt) || o.ReceivedAt.After(c.Cutoff) {
			return AdjudicationBasis{}, invalid("outcome unavailable at basis cutoff")
		}
	}
	if err := validateBasisAncestry(c.Outcomes); err != nil {
		return AdjudicationBasis{}, err
	}
	sort.Slice(c.Witnesses, func(i, j int) bool { return c.Witnesses[i].ID < c.Witnesses[j].ID })
	if len(c.Witnesses) != len(proposal.config.WitnessIDs) {
		return AdjudicationBasis{}, invalid("basis witness set mismatch")
	}
	for i := range c.Witnesses {
		w := &c.Witnesses[i]
		if w.ID != proposal.config.WitnessIDs[i] {
			return AdjudicationBasis{}, invalid("basis witness set mismatch")
		}
		for _, id := range []shoal.ID{w.ID, w.VerificationReceiptID, w.OriginGroupID} {
			if err := requiredID(id); err != nil {
				return AdjudicationBasis{}, err
			}
		}
		if !digestValid(w.Digest) {
			return AdjudicationBasis{}, invalid("invalid witness digest")
		}
		w.ReceivedAt = utc(w.ReceivedAt)
		w.VerifiedAt = utc(w.VerifiedAt)
		if !validTime(w.ReceivedAt) || !validTime(w.VerifiedAt) || w.VerifiedAt.Before(w.ReceivedAt) || w.VerifiedAt.After(c.Cutoff) {
			return AdjudicationBasis{}, invalid("witness unavailable at basis cutoff")
		}
		if err := sortBasisIdentities(w.ControllerIdentities); err != nil {
			return AdjudicationBasis{}, err
		}
	}
	if err := sortBasisIdentities(c.SourceControllers); err != nil {
		return AdjudicationBasis{}, err
	}
	sort.Slice(c.RoleEvidenceIDs, func(i, j int) bool { return c.RoleEvidenceIDs[i] < c.RoleEvidenceIDs[j] })
	for i, id := range c.RoleEvidenceIDs {
		if err := requiredID(id); err != nil {
			return AdjudicationBasis{}, err
		}
		if i > 0 && id == c.RoleEvidenceIDs[i-1] {
			return AdjudicationBasis{}, invalid("duplicate role evidence")
		}
	}
	id, err := identity("adjudication-basis", struct {
		ProposalID shoal.ID
		Config     AdjudicationBasisConfig
	}{proposal.ID(), c})
	if err != nil {
		return AdjudicationBasis{}, err
	}
	return AdjudicationBasis{id, proposal, c}, nil
}

// Preflight validates bounds and opaque identity shapes before copying any
// attacker-sized collections or allocating their canonical JSON representation.
func basisPreflight(c AdjudicationBasisConfig) error {
	if len(c.Outcomes) == 0 || len(c.Outcomes) > MaxAdjudicationObservations || len(c.Witnesses) > MaxAdjudicationWitnesses || len(c.SourceControllers) > MaxBasisIdentities || len(c.RoleEvidenceIDs) == 0 || len(c.RoleEvidenceIDs) > MaxBasisIdentities {
		return invalid("invalid basis collection counts")
	}
	var b byteBudget
	if err := b.charge(4096); err != nil {
		return err
	}
	if err := b.ids(c.AuthorityID, c.AuthorityRevisionID, c.EnumerationID); err != nil {
		return err
	}
	if err := b.ids(c.RoleEvidenceIDs...); err != nil {
		return err
	}
	for _, o := range c.Outcomes {
		if err := b.charge(1024); err != nil {
			return err
		}
		if err := b.ids(o.ReceiptID, o.ObservationID, o.RequestID, o.PredictionID, o.TaskID, o.PictureID, o.SubjectID, o.QuestionID, o.Supersedes); err != nil {
			return err
		}
		if err := b.text(string(o.Kind)); err != nil {
			return err
		}
		if err := basisIdentityBudget(&b, o.Reporter); err != nil {
			return err
		}
	}
	for _, w := range c.Witnesses {
		if len(w.ControllerIdentities) > MaxBasisIdentities {
			return invalid("too many witness controllers")
		}
		if err := b.charge(1024); err != nil {
			return err
		}
		if err := b.ids(w.ID, w.VerificationReceiptID, w.OriginGroupID); err != nil {
			return err
		}
		if err := b.text(w.Digest); err != nil {
			return err
		}
		if err := basisIdentityBudget(&b, w.Origin); err != nil {
			return err
		}
		for _, id := range w.ControllerIdentities {
			if err := basisIdentityBudget(&b, id); err != nil {
				return err
			}
		}
	}
	for _, id := range c.SourceControllers {
		if err := basisIdentityBudget(&b, id); err != nil {
			return err
		}
	}
	return nil
}
func basisIdentityBudget(b *byteBudget, id BasisIdentity) error {
	if len(id.SubjectID) == 0 || len(id.ActorID) == 0 || len(id.OnBehalfOf) > auth.MaxOnBehalfOfEntries {
		return invalid("invalid basis identity")
	}
	for _, component := range [][]byte{id.SubjectID, id.ActorID, id.ClientID} {
		if len(component) > shoal.MaxIDBytes {
			return invalid("basis identity exceeds limit")
		}
		if err := b.charge(2*len(component) + 64); err != nil {
			return err
		}
	}
	for _, delegate := range id.OnBehalfOf {
		if len(delegate) == 0 || len(delegate) > shoal.MaxIDBytes {
			return invalid("invalid basis delegate")
		}
		if err := b.charge(2*len(delegate) + 32); err != nil {
			return err
		}
	}
	return nil
}
func sortBasisIdentities(ids []BasisIdentity) error {
	// Identity arrays have already been bounded, copied and nil-normalized.
	keys := make(map[string]bool, len(ids))
	for _, id := range ids {
		raw, _ := json.Marshal(id)
		key := string(raw)
		if keys[key] {
			return invalid("duplicate basis identity")
		}
		keys[key] = true
	}
	sort.Slice(ids, func(i, j int) bool {
		a, _ := json.Marshal(ids[i])
		b, _ := json.Marshal(ids[j])
		return bytes.Compare(a, b) < 0
	})
	return nil
}
func cloneBasisIdentity(id BasisIdentity) BasisIdentity {
	id.SubjectID = append([]byte(nil), id.SubjectID...)
	id.ActorID = append([]byte(nil), id.ActorID...)
	id.ClientID = append([]byte(nil), id.ClientID...)
	delegates := id.OnBehalfOf
	id.OnBehalfOf = nil
	for _, delegate := range delegates {
		id.OnBehalfOf = append(id.OnBehalfOf, append([]byte(nil), delegate...))
	}
	return id
}
func cloneBasis(c AdjudicationBasisConfig) AdjudicationBasisConfig {
	c.Outcomes = append([]BasisOutcome(nil), c.Outcomes...)
	for i := range c.Outcomes {
		c.Outcomes[i].Reporter = cloneBasisIdentity(c.Outcomes[i].Reporter)
	}
	c.Witnesses = append([]BasisWitness(nil), c.Witnesses...)
	for i := range c.Witnesses {
		w := &c.Witnesses[i]
		w.Origin = cloneBasisIdentity(w.Origin)
		w.ControllerIdentities = append([]BasisIdentity(nil), w.ControllerIdentities...)
		for j := range w.ControllerIdentities {
			w.ControllerIdentities[j] = cloneBasisIdentity(w.ControllerIdentities[j])
		}
	}
	c.SourceControllers = append([]BasisIdentity(nil), c.SourceControllers...)
	for i := range c.SourceControllers {
		c.SourceControllers[i] = cloneBasisIdentity(c.SourceControllers[i])
	}
	c.RoleEvidenceIDs = append([]shoal.ID(nil), c.RoleEvidenceIDs...)
	return c
}
func (b AdjudicationBasis) ID() shoal.ID                    { return b.id }
func (b AdjudicationBasis) Config() AdjudicationBasisConfig { return cloneBasis(b.config) }
func (b AdjudicationBasis) Proposal() AdjudicationProposal  { return b.proposal }
func (b AdjudicationBasis) Validate() error {
	rebuilt, err := NewAdjudicationBasis(b.proposal, b.config)
	if err != nil {
		return err
	}
	if rebuilt.id != b.id {
		return invalid("adjudication basis identity mismatch")
	}
	return nil
}

func validateBasisAncestry(outcomes []BasisOutcome) error {
	byID := make(map[shoal.ID]int, len(outcomes))
	reporters := make([]string, len(outcomes))
	for i, o := range outcomes {
		byID[o.ReceiptID] = i
		raw, _ := json.Marshal(o.Reporter)
		reporters[i] = string(raw)
	}
	parents := make([]int, len(outcomes))
	for i, child := range outcomes {
		parents[i] = -1
		if child.Supersedes == "" {
			continue
		}
		j, exists := byID[child.Supersedes]
		if !exists {
			return invalid("missing basis correction ancestry")
		}
		parent := outcomes[j]
		if parent.RequestID != child.RequestID || parent.PredictionID != child.PredictionID || reporters[i] != reporters[j] || parent.ReceivedAt.After(child.ReceivedAt) {
			return invalid("basis correction ancestry mismatch")
		}
		parents[i] = j
	}
	// Each node and edge is visited once. Memoized depths preserve branching
	// histories without repeatedly allocating reporter encodings per ancestor.
	state := make([]uint8, len(outcomes))
	depth := make([]int, len(outcomes))
	var visit func(int) error
	visit = func(i int) error {
		if state[i] == 1 {
			return invalid("cyclic basis correction ancestry")
		}
		if state[i] == 2 {
			return nil
		}
		state[i] = 1
		if p := parents[i]; p >= 0 {
			if err := visit(p); err != nil {
				return err
			}
			depth[i] = depth[p] + 1
			if depth[i] > MaxOutcomeAncestors {
				return invalid("basis correction ancestry exceeds limit")
			}
		}
		state[i] = 2
		return nil
	}
	for i := range outcomes {
		if err := visit(i); err != nil {
			return err
		}
	}
	return nil
}
