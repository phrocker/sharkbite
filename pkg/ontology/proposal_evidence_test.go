// Licensed to the Apache Software Foundation (ASF) under one or more
// contributor license agreements. See the NOTICE file distributed with this
// work for additional information regarding copyright ownership.

package ontology

import (
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/phrocker/shoal-oss/pkg/shoal"
)

func TestProposalEvidencePreservesLegacyIdentity(t *testing.T) {
	f := newMorphismFixture(t)
	at := f.v2.CreatedAt().Add(time.Hour)
	morphism := mustMorphism(t, MorphismConfig{
		Kind: MorphismWiden, SourceVersion: f.v1, TargetVersion: f.v2,
		Sources: []shoal.ID{f.v1rel.ID()}, Targets: []shoal.ID{f.v2rel.ID()},
		Evidence: []EvidenceRef{f.evidence}, Rationale: "widen",
	})
	for _, morphisms := range [][]OntologyMorphism{nil, {morphism}} {
		proposal, err := NewGovernedProposalWithMorphisms(
			f.v1.Schema(), f.v1, f.v2, morphisms, "author", "reason", at, nil)
		if err != nil {
			t.Fatal(err)
		}
		parts := []string{
			string(f.v1.Schema().ID()),
			canonicalOptional(true, canonicalParts(string(f.v1.Schema().ID()), string(f.v1.ID()))),
			string(f.v2.ID()), "author", "reason", canonicalTime(at),
		}
		if len(morphisms) > 0 {
			parts = append(parts, canonicalParts(string(morphism.ID())))
		}
		legacyID, _ := deriveID("proposal", parts...)
		if proposal.ID() != legacyID || len(proposal.ExplicitEvidence()) != 0 {
			t.Fatal("legacy proposal identity or evidence changed")
		}
		if len(proposal.Evidence()) != len(morphisms) {
			t.Fatal("legacy evidence union omitted morphism evidence")
		}
	}
}

func TestProposalEvidenceCanonicalIdentityAndDefensiveCopies(t *testing.T) {
	f := newMorphismFixture(t)
	at := f.v2.CreatedAt().Add(time.Hour)
	extra, err := NewEvidenceRef(f.evidence.Citation(), "another source quote",
		shoal.Metadata{"source": "exposed"})
	if err != nil {
		t.Fatal(err)
	}
	morphism := mustMorphism(t, MorphismConfig{
		Kind: MorphismWiden, SourceVersion: f.v1, TargetVersion: f.v2,
		Sources: []shoal.ID{f.v1rel.ID()}, Targets: []shoal.ID{f.v2rel.ID()},
		Evidence: []EvidenceRef{f.evidence}, Rationale: "widen",
	})
	makeProposal := func(evidence []EvidenceRef) GovernedProposal {
		t.Helper()
		proposal, err := NewGovernedProposalWithEvidence(
			f.v1.Schema(), f.v1, f.v2, []OntologyMorphism{morphism}, evidence,
			"author", "reason", at, nil)
		if err != nil {
			t.Fatal(err)
		}
		return proposal
	}
	input := []EvidenceRef{extra, f.evidence, extra}
	proposal := makeProposal(input)
	reordered := makeProposal([]EvidenceRef{f.evidence, extra})
	if proposal.ID() != reordered.ID() || len(proposal.Evidence()) != 2 ||
		len(proposal.ExplicitEvidence()) != 2 {
		t.Fatal("proposal evidence was not normalized")
	}
	input[0].metadata["source"] = "mutated"
	input[1] = EvidenceRef{}
	returned := proposal.Evidence()
	returned[0].metadata = shoal.Metadata{"mutated": "true"}
	returned[1] = EvidenceRef{}
	explicit := proposal.ExplicitEvidence()
	explicit[0] = EvidenceRef{}
	if !reflect.DeepEqual(proposal.Evidence(), reordered.Evidence()) {
		t.Fatal("proposal retained mutable evidence")
	}
	changed, _ := NewEvidenceRef(extra.Citation(), extra.Quote(), shoal.Metadata{"source": "changed"})
	if proposal.ID() == makeProposal([]EvidenceRef{changed, f.evidence}).ID() ||
		proposal.ID() == makeProposal([]EvidenceRef{f.evidence}).ID() {
		t.Fatal("explicit evidence identity or metadata is not bound")
	}
	transitioned, err := proposal.Transition(ProposalSubmitted, "reviewer", "review", at.Add(time.Second))
	if err != nil || transitioned.ID() != proposal.ID() ||
		!reflect.DeepEqual(transitioned.Evidence(), proposal.Evidence()) {
		t.Fatalf("transition lost evidence: %v", err)
	}
}

func TestProposalEvidenceRejectsInvalidExposure(t *testing.T) {
	f := newMorphismFixture(t)
	at := f.v2.CreatedAt().Add(time.Hour)
	conflict, _ := NewEvidenceRef(f.evidence.Citation(), f.evidence.Quote(), shoal.Metadata{"conflict": "true"})
	derived, err := NewAssertionDerivation("embedding", "1", "cosine", 0.8,
		"cell:1", 0.9, "entity:a", "entity:b", "iterator", nil)
	if err != nil {
		t.Fatal(err)
	}
	derivation, err := NewDerivationEvidenceRef(derived, nil)
	if err != nil {
		t.Fatal(err)
	}
	tooMany := make([]EvidenceRef, MaxProposalEvidence+1)
	for i := range tooMany {
		tooMany[i] = f.evidence
	}
	for _, evidence := range [][]EvidenceRef{
		nil, {EvidenceRef{}}, {derivation}, tooMany, {f.evidence, conflict},
	} {
		if _, err := NewGovernedProposalWithEvidence(
			f.v1.Schema(), f.v1, f.v2, nil, evidence,
			"author", "reason", at, nil); err == nil {
			t.Fatal("invalid proposal evidence accepted")
		}
	}
	// A syntactically valid source locator must still be authorizable.
	citation := f.evidence.Citation()
	citation.SectionID, citation.SpanID = "", ""
	if ref, err := NewEvidenceRef(citation, "", nil); err == nil {
		if _, err := NewGovernedProposalWithEvidence(
			f.v1.Schema(), f.v1, f.v2, nil, []EvidenceRef{ref},
			"author", "reason", at, nil); err == nil {
			t.Fatal("non-authorizable citation accepted")
		}
	}
}

func TestProposalValidateEvolutionUsesExactBaseAndSharedRules(t *testing.T) {
	f := newMorphismFixture(t)
	at := f.v2.CreatedAt().Add(time.Hour)
	proposal, err := NewGovernedProposalWithEvidence(f.v1.Schema(), f.v1, f.v2, nil,
		[]EvidenceRef{f.evidence}, "author", "reason", at, nil)
	if err != nil {
		t.Fatal(err)
	}
	if proposal.ValidateEvolution(f.v1) == nil {
		t.Fatal("unmapped semantic change accepted")
	}
	morphism := mustMorphism(t, MorphismConfig{
		Kind: MorphismWiden, SourceVersion: f.v1, TargetVersion: f.v2,
		Sources: []shoal.ID{f.v1rel.ID()}, Targets: []shoal.ID{f.v2rel.ID()},
		Evidence: []EvidenceRef{f.evidence}, Rationale: "widen",
	})
	proposal, err = NewGovernedProposalWithEvidence(f.v1.Schema(), f.v1, f.v2,
		[]OntologyMorphism{morphism}, []EvidenceRef{f.evidence}, "author", "reason", at, nil)
	if err != nil || proposal.ValidateEvolution(f.v1) != nil {
		t.Fatalf("mapped evolution rejected: %v", err)
	}
	if proposal.ValidateEvolution(f.v2) == nil || proposal.ValidateEvolution(OntologyVersion{}) == nil {
		t.Fatal("wrong base accepted")
	}
	tampered := proposal
	tampered.id = ""
	if tampered.ValidateEvolution(f.v1) == nil {
		t.Fatal("invalid proposal accepted")
	}
	added, _ := NewConceptDefinition("addition", "Addition", "", nil, nil)
	additive, err := NewOntologyVersion(f.v1.Schema(), "additive", at,
		append(f.v1.Concepts(), added), f.v1.Relationships(), f.v1.Properties(), nil)
	if err != nil {
		t.Fatal(err)
	}
	proposal, err = NewGovernedProposalWithEvidence(f.v1.Schema(), f.v1, additive,
		nil, []EvidenceRef{f.evidence}, "author", "additive", at.Add(time.Second), nil)
	if err != nil || proposal.ValidateEvolution(f.v1) != nil {
		t.Fatalf("additive evolution rejected: %v", err)
	}
}

func TestProposalEvidenceExtractionBoundsAndAccounting(t *testing.T) {
	f := newMorphismFixture(t)
	extra, err := NewEvidenceRef(f.evidence.Citation(), strings.Repeat("extra", 256),
		shoal.Metadata{"exposure": "uncited"})
	if err != nil {
		t.Fatal(err)
	}
	at := f.v2.CreatedAt().Add(time.Hour)
	legacy, _ := NewGovernedProposal(f.v1.Schema(), f.v1, f.v2, "author", "reason", at, nil)
	proposal, err := NewGovernedProposalWithEvidence(f.v1.Schema(), f.v1, f.v2, nil,
		[]EvidenceRef{extra}, "author", "reason", at, nil)
	if err != nil {
		t.Fatal(err)
	}
	if proposalPayloadBytes(proposal) != proposalPayloadBytes(legacy)+evidencePayloadBytes(extra) {
		t.Fatal("explicit evidence omitted from payload accounting")
	}
	oldCounter, newCounter := payloadCounter{limit: ^uint64(0)}, payloadCounter{limit: ^uint64(0)}
	oldCounter.addProposal(legacy)
	newCounter.addProposal(proposal)
	if newCounter.size <= oldCounter.size {
		t.Fatal("explicit evidence omitted from preflight accounting")
	}
	limits := DefaultExtractionLimits()
	request, err := NewExtractionRequest(f.v1, []EvidenceRef{f.evidence},
		"instructions", f.provenance, limits, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := NewExtractionResult(request, nil, []GovernedProposal{proposal}, at, nil); err == nil || !strings.Contains(err.Error(), "outside the extraction request") {
		t.Fatalf("unexposed proposal evidence accepted: %v", err)
	}
	limits.MaxEvidence = 1
	request, err = NewExtractionRequest(f.v1, []EvidenceRef{f.evidence},
		"instructions", f.provenance, limits, nil)
	if err != nil {
		t.Fatal(err)
	}
	proposal, err = NewGovernedProposalWithEvidence(f.v1.Schema(), f.v1, f.v2, nil,
		[]EvidenceRef{extra, f.evidence}, "author", "reason", at, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := NewExtractionResult(request, nil, []GovernedProposal{proposal}, at, nil); err == nil || !strings.Contains(err.Error(), "proposal evidence exceeds") {
		t.Fatalf("proposal evidence bypassed extraction count bound: %v", err)
	}
}
