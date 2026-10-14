// Licensed under the Apache License, Version 2.0. See LICENSE and NOTICE.
package decision_test

import (
	"bytes"
	"fmt"
	"github.com/phrocker/shoal-oss/pkg/decision"
	"github.com/phrocker/shoal-oss/pkg/shoal"
	"strings"
	"testing"
	"time"
)

func basisFixture(t *testing.T) (decision.AdjudicationProposal, decision.AdjudicationBasisConfig) {
	t.Helper()
	policy, prediction, pc := adjudicationFixture(t)
	proposal, err := decision.NewAdjudicationProposal(policy, prediction, pc)
	if err != nil {
		t.Fatal(err)
	}
	person := decision.BasisIdentity{SubjectID: []byte{0xff, 0}, ActorID: []byte("actor"), ClientID: []byte{0xfe}, OnBehalfOf: [][]byte{[]byte("delegate")}}
	c := decision.AdjudicationBasisConfig{AuthorityID: "authority", AuthorityRevisionID: "authority:revision", EnumerationID: "enumeration", CapturedAt: now.Add(time.Hour), Cutoff: now.Add(time.Minute), InventoryComplete: true, ControllersComplete: true, SourceControllers: []decision.BasisIdentity{person}, RoleEvidenceIDs: []shoal.ID{"role:z", "role:a"}}
	for _, id := range pc.ObservationReceiptIDs {
		c.Outcomes = append(c.Outcomes, decision.BasisOutcome{ReceiptID: id, ObservationID: "observation:" + id, RequestID: pc.RequestID, PredictionID: pc.PredictionID, TaskID: proposal.TaskID(), PictureID: proposal.PictureID(), SubjectID: pc.SubjectID, QuestionID: pc.QuestionID, Kind: decision.OutcomeCorrectness, Reporter: person, ReceivedAt: now})
	}
	for _, id := range pc.WitnessIDs {
		c.Witnesses = append(c.Witnesses, decision.BasisWitness{ID: id, Digest: strings.Repeat("a", 64), VerificationReceiptID: "verification:" + id, OriginGroupID: "shared-origin", Origin: person, ControllerIdentities: []decision.BasisIdentity{person}, ReceivedAt: now, VerifiedAt: now.Add(time.Second)})
	}
	return proposal, c
}
func TestBasisCanonicalDeepCopiesAndOpaqueIdentity(t *testing.T) {
	proposal, c := basisFixture(t)
	basis, err := decision.NewAdjudicationBasis(proposal, c)
	if err != nil {
		t.Fatal(err)
	}
	if err = basis.Validate(); err != nil {
		t.Fatal(err)
	}
	c.Outcomes[0].Reporter.SubjectID[0] = 1
	c.Witnesses[0].Origin.OnBehalfOf[0][0] = 1
	copy := basis.Config()
	copy.SourceControllers[0].ClientID[0] = 1
	copy.Witnesses[0].ControllerIdentities[0].ActorID[0] = 1
	copy.Outcomes[0].Reporter.OnBehalfOf[0][0] = 1
	stable := basis.Config()
	if !bytes.Equal(stable.Outcomes[0].Reporter.SubjectID, []byte{255, 0}) || !bytes.Equal(stable.SourceControllers[0].ClientID, []byte{254}) || string(stable.Outcomes[0].Reporter.OnBehalfOf[0]) != "delegate" {
		t.Fatal("opaque bytes lost or copy escaped")
	}
	stable.Outcomes[0], stable.Outcomes[1] = stable.Outcomes[1], stable.Outcomes[0]
	stable.Witnesses[0], stable.Witnesses[1] = stable.Witnesses[1], stable.Witnesses[0]
	stable.RoleEvidenceIDs[0], stable.RoleEvidenceIDs[1] = stable.RoleEvidenceIDs[1], stable.RoleEvidenceIDs[0]
	stable.Cutoff = stable.Cutoff.In(time.FixedZone("local", 3600))
	stable.CapturedAt = stable.CapturedAt.In(time.FixedZone("local", 3600))
	for i := range stable.Outcomes {
		stable.Outcomes[i].ReceivedAt = stable.Outcomes[i].ReceivedAt.In(time.FixedZone("local", 3600))
	}
	b, err := decision.NewAdjudicationBasis(proposal, stable)
	if err != nil || b.ID() != basis.ID() {
		t.Fatalf("noncanonical snapshot: %v", err)
	}
	if b.Proposal().ID() != proposal.ID() {
		t.Fatal("proposal binding lost")
	}
	// Duplicate origin groups remain visible; this constructor cannot infer that
	// two distinct witness references constitute independent corroboration.
	if b.Config().Witnesses[0].OriginGroupID != b.Config().Witnesses[1].OriginGroupID {
		t.Fatal("shared provenance hidden")
	}
}
func TestBasisRejectsIncompleteSubstitutedAndFutureEvidence(t *testing.T) {
	cases := map[string]func(*decision.AdjudicationBasisConfig){
		"incomplete inventory": func(c *decision.AdjudicationBasisConfig) { c.InventoryComplete = false }, "unknown controllers": func(c *decision.AdjudicationBasisConfig) { c.ControllersComplete = false },
		"missing outcome": func(c *decision.AdjudicationBasisConfig) { c.Outcomes = c.Outcomes[:1] }, "duplicate outcome": func(c *decision.AdjudicationBasisConfig) { c.Outcomes[1] = c.Outcomes[0] }, "wrong target": func(c *decision.AdjudicationBasisConfig) { c.Outcomes[0].PictureID = "other" }, "execution report": func(c *decision.AdjudicationBasisConfig) { c.Outcomes[0].Kind = decision.OutcomeExecution },
		"missing witness": func(c *decision.AdjudicationBasisConfig) { c.Witnesses = c.Witnesses[:1] }, "duplicate witness": func(c *decision.AdjudicationBasisConfig) { c.Witnesses[1] = c.Witnesses[0] }, "wrong witness": func(c *decision.AdjudicationBasisConfig) { c.Witnesses[0].ID = "other" }, "invalid digest": func(c *decision.AdjudicationBasisConfig) { c.Witnesses[0].Digest = strings.Repeat("A", 64) },
		"future outcome": func(c *decision.AdjudicationBasisConfig) { c.Outcomes[0].ReceivedAt = c.Cutoff.Add(time.Nanosecond) }, "future verification": func(c *decision.AdjudicationBasisConfig) { c.Witnesses[0].VerifiedAt = c.Cutoff.Add(time.Nanosecond) }, "verification before receipt": func(c *decision.AdjudicationBasisConfig) { c.Witnesses[0].VerifiedAt = now.Add(-time.Second) }, "capture before cutoff": func(c *decision.AdjudicationBasisConfig) { c.CapturedAt = c.Cutoff.Add(-time.Nanosecond) },
		"missing authority": func(c *decision.AdjudicationBasisConfig) { c.AuthorityID = "" }, "missing role evidence": func(c *decision.AdjudicationBasisConfig) { c.RoleEvidenceIDs = nil }, "duplicate role evidence": func(c *decision.AdjudicationBasisConfig) { c.RoleEvidenceIDs[1] = c.RoleEvidenceIDs[0] },
		"missing reporter": func(c *decision.AdjudicationBasisConfig) { c.Outcomes[0].Reporter.SubjectID = nil }, "long identity": func(c *decision.AdjudicationBasisConfig) {
			c.SourceControllers[0].SubjectID = make([]byte, shoal.MaxIDBytes+1)
		}, "too many delegates": func(c *decision.AdjudicationBasisConfig) { c.Outcomes[0].Reporter.OnBehalfOf = make([][]byte, 65) }, "empty delegate": func(c *decision.AdjudicationBasisConfig) { c.Outcomes[0].Reporter.OnBehalfOf = [][]byte{nil} },
		"duplicate controllers": func(c *decision.AdjudicationBasisConfig) {
			c.SourceControllers = append(c.SourceControllers, c.SourceControllers[0])
		}, "duplicate witness controllers": func(c *decision.AdjudicationBasisConfig) {
			c.Witnesses[0].ControllerIdentities = append(c.Witnesses[0].ControllerIdentities, c.Witnesses[0].ControllerIdentities[0])
		},
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			p, c := basisFixture(t)
			mutate(&c)
			if _, err := decision.NewAdjudicationBasis(p, c); err == nil {
				t.Fatal("accepted malformed basis")
			}
		})
	}
	if err := (decision.AdjudicationBasis{}).Validate(); err == nil {
		t.Fatal("accepted zero basis")
	}
}
func TestBasisCorrectionInventoryAndBranches(t *testing.T) {
	p, original := basisFixture(t)
	original.Outcomes[1].Supersedes = original.Outcomes[0].ReceiptID
	valid, err := decision.NewAdjudicationBasis(p, original)
	if err != nil {
		t.Fatal(err)
	}
	for name, mutate := range map[string]func(*decision.AdjudicationBasisConfig){
		"missing ancestor": func(c *decision.AdjudicationBasisConfig) {
			c.Outcomes[0].Supersedes = "outcome-receipt:" + shoal.ID(strings.Repeat("c", 64))
		},
		"cycle": func(c *decision.AdjudicationBasisConfig) {
			c.Outcomes[1].Supersedes = c.Outcomes[0].ReceiptID
			c.Outcomes[0].Supersedes = c.Outcomes[1].ReceiptID
		},
		"changed reporter":   func(c *decision.AdjudicationBasisConfig) { c.Outcomes[0].Reporter.ActorID = []byte("different") },
		"changed request":    func(c *decision.AdjudicationBasisConfig) { c.Outcomes[0].RequestID = "different" },
		"changed prediction": func(c *decision.AdjudicationBasisConfig) { c.Outcomes[0].PredictionID = "different" },
	} {
		t.Run(name, func(t *testing.T) {
			c := valid.Config()
			mutate(&c)
			if _, err := decision.NewAdjudicationBasis(p, c); err == nil {
				t.Fatal("accepted invalid ancestry")
			}
		})
	}
}
func TestBasisAggregateBoundAndUnresolvedControllers(t *testing.T) {
	p, c := basisFixture(t)
	id := decision.BasisIdentity{SubjectID: bytes.Repeat([]byte{'s'}, 1024), ActorID: bytes.Repeat([]byte{'a'}, 1024)}
	c.Witnesses[0].ControllerIdentities = make([]decision.BasisIdentity, 1024)
	for i := range c.Witnesses[0].ControllerIdentities {
		c.Witnesses[0].ControllerIdentities[i] = id
	}
	if _, err := decision.NewAdjudicationBasis(p, c); err == nil {
		t.Fatal("accepted aggregate over bound")
	}
	policy, prediction, pc := adjudicationFixture(t)
	pc.Disposition = decision.AdjudicationUnresolved
	pc.Label = ""
	pc.Reason = "authority incomplete"
	pc.WitnessIDs = nil
	unresolved, err := decision.NewAdjudicationProposal(policy, prediction, pc)
	if err != nil {
		t.Fatal(err)
	}
	_, c = basisFixture(t)
	c.Witnesses = nil
	c.ControllersComplete = false
	c.SourceControllers = nil
	if _, err := decision.NewAdjudicationBasis(unresolved, c); err != nil {
		t.Fatal(err)
	}
}

func TestBasisCorrectionDepthBoundaryAndBranches(t *testing.T) {
	for _, ancestors := range []int{decision.MaxOutcomeAncestors, decision.MaxOutcomeAncestors + 1} {
		t.Run(fmt.Sprint(ancestors), func(t *testing.T) {
			policy, prediction, pc := adjudicationFixture(t)
			_, c := basisFixture(t)
			seed := c.Outcomes[0]
			c.Outcomes = nil
			pc.ObservationReceiptIDs = nil
			for i := 0; i <= ancestors; i++ {
				o := seed
				o.ReceiptID = shoal.ID(fmt.Sprintf("outcome-receipt:%064x", i+1))
				if i > 0 {
					o.Supersedes = c.Outcomes[i-1].ReceiptID
				}
				c.Outcomes = append(c.Outcomes, o)
				pc.ObservationReceiptIDs = append(pc.ObservationReceiptIDs, o.ReceiptID)
			}
			// A sibling branch must neither increase the longest path nor evade it.
			o := seed
			o.ReceiptID = shoal.ID(fmt.Sprintf("outcome-receipt:%064x", 1000))
			o.Supersedes = c.Outcomes[0].ReceiptID
			c.Outcomes = append(c.Outcomes, o)
			pc.ObservationReceiptIDs = append(pc.ObservationReceiptIDs, o.ReceiptID)
			p, err := decision.NewAdjudicationProposal(policy, prediction, pc)
			if err != nil {
				t.Fatal(err)
			}
			_, err = decision.NewAdjudicationBasis(p, c)
			if (err == nil) != (ancestors <= decision.MaxOutcomeAncestors) {
				t.Fatalf("ancestors=%d: %v", ancestors, err)
			}
		})
	}
}
