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
 */

package ontology

import (
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/phrocker/shoal-oss/pkg/document"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

func TestOntologyLensSafetySemanticsAndImmutableOriginals(t *testing.T) {
	f := newMorphismFixture(t)
	widen := mustMorphism(t, MorphismConfig{
		Kind: MorphismWiden, SourceVersion: f.v1, TargetVersion: f.v2,
		Sources: []shoal.ID{f.v1rel.ID()}, Targets: []shoal.ID{f.v2rel.ID()},
		Evidence: []EvidenceRef{f.evidence}, Rationale: "support contractors",
	})
	if widen.Safety() != MorphismSafeWidening {
		t.Fatalf("widen safety = %q", widen.Safety())
	}
	lens, err := NewOntologyLens(f.v2, []OntologyMorphism{widen})
	if err != nil {
		t.Fatal(err)
	}
	read := lens.Read(f.personAssertion)
	if !read.Resolved() || read.Predicate() != f.v2rel.ID() {
		t.Fatalf("widening read = %#v", read)
	}

	narrow := mustMorphism(t, MorphismConfig{
		Kind: MorphismNarrow, SourceVersion: f.v2, TargetVersion: f.v1,
		Sources: []shoal.ID{f.v2rel.ID()}, Targets: []shoal.ID{f.v1rel.ID()},
		Evidence: []EvidenceRef{f.evidence}, Rationale: "remove contractors",
	})
	narrowLens, err := NewOntologyLens(f.v1, []OntologyMorphism{narrow})
	if err != nil {
		t.Fatal(err)
	}
	read = narrowLens.Read(f.contractorAssertion)
	if read.Resolved() || read.Status() != InterpretationUnresolved {
		t.Fatal("unsafe narrowing silently retained an incompatible assertion")
	}
	if originalType, _ := read.Original().SubjectType(); originalType != f.contractor.ID() {
		t.Fatal("narrowing rewrote the historic assertion")
	}
}

func TestOntologyLensRenameSplitMergeAndUnknown(t *testing.T) {
	f := newMorphismFixture(t)
	rename := mustMorphism(t, MorphismConfig{
		Kind: MorphismRename, SourceVersion: f.renameFrom, TargetVersion: f.renameTo,
		Sources: []shoal.ID{f.oldRel.ID()}, Targets: []shoal.ID{f.newRel.ID()},
		Evidence: []EvidenceRef{f.evidence}, Rationale: "terminology change",
	})
	lens, _ := NewOntologyLens(f.renameTo, []OntologyMorphism{rename})
	read := lens.Read(f.renameAssertion)
	if !read.Resolved() || read.Predicate() != f.newRel.ID() {
		t.Fatal("explicit rename mapping was not applied")
	}

	if read.Original().Predicate() != f.oldRel.ID() {
		t.Fatal("rename mutated the original assertion")
	}

	discriminator, err := NewMorphismDiscriminator(
		"kind", map[string]shoal.ID{"person": f.person.ID(), "org": f.org.ID()})
	if err != nil {
		t.Fatal(err)
	}
	split := mustMorphism(t, MorphismConfig{
		Kind: MorphismSplit, SourceVersion: f.splitFrom, TargetVersion: f.splitTo,
		Sources: []shoal.ID{f.party.ID()}, Targets: []shoal.ID{f.person.ID(), f.org.ID()},
		Discriminator: discriminator, Evidence: []EvidenceRef{f.evidence},
		Rationale: "separate parties",
	})
	splitLens, _ := NewOntologyLens(f.splitTo, []OntologyMorphism{split})
	if got := splitLens.Read(f.splitAssertion); !got.Resolved() {
		t.Fatalf("discriminated split unresolved: %s", got.Reason())
	}
	ambiguous := mustPropertyAssertion(
		t, f.party.ID(), f.name.ID(), f.splitFrom, nil, f.evidence, f.provenance)
	if got := splitLens.Read(ambiguous); got.Resolved() {
		t.Fatal("split without a discriminator guessed a target")
	}

	merge := mustMorphism(t, MorphismConfig{
		Kind: MorphismMerge, SourceVersion: f.splitTo, TargetVersion: f.splitFrom,
		Sources: []shoal.ID{f.person.ID(), f.org.ID()}, Targets: []shoal.ID{f.party.ID()},
		Evidence: []EvidenceRef{f.evidence}, Rationale: "unified party view",
	})
	mergeLens, _ := NewOntologyLens(f.splitFrom, []OntologyMorphism{merge})
	merged := mergeLens.Read(mustPropertyAssertion(
		t, f.person.ID(), f.name.ID(), f.splitTo, nil, f.evidence, f.provenance))
	if !merged.Resolved() {
		t.Fatalf("merge unresolved: %s", merged.Reason())
	}
	if original, _ := merged.Original().SubjectType(); original != f.person.ID() {
		t.Fatal("lossy merge did not retain the original type")
	}
	if effective, _ := merged.SubjectType(); effective != f.party.ID() {
		t.Fatal("merge did not expose the selected target type")
	}

	unknown := mustPropertyAssertion(
		t, f.party.ID(), f.name.ID(), OntologyVersion{}, nil, f.evidence, f.provenance)
	if got := mergeLens.Read(unknown); got.Resolved() || got.Reading() != OntologyUnresolved {
		t.Fatal("unknown ontology was silently reinterpreted")
	}
}

func TestOntologyMorphismDefensivelyOwnsSafetyInputs(t *testing.T) {
	f := newMorphismFixture(t)
	sources := []shoal.ID{f.oldRel.ID()}
	targets := []shoal.ID{f.newRel.ID()}
	metadata := shoal.Metadata{"review": "approved"}
	morphism := mustMorphism(t, MorphismConfig{
		Kind: MorphismRename, SourceVersion: f.renameFrom, TargetVersion: f.renameTo,
		Sources: sources, Targets: targets, Evidence: []EvidenceRef{f.evidence},
		Rationale: "rename safely tracked", Metadata: metadata,
	})
	sources[0] = f.person.ID()
	targets[0] = f.org.ID()
	metadata["review"] = "mutated"
	returnedSources := morphism.Sources()
	returnedSources[0] = f.person.ID()
	returnedMetadata := morphism.Metadata()
	returnedMetadata["review"] = "mutated-again"
	if err := morphism.Validate(); err != nil {
		t.Fatalf("caller mutation changed morphism: %v", err)
	}

	if morphism.Sources()[0] != f.oldRel.ID() ||
		morphism.Metadata()["review"] != "approved" {
		t.Fatal("morphism retained caller-owned mutable data")
	}
}

func TestReadAssertionUnderResolvesOnlyExactRecordedVersion(t *testing.T) {
	f := newMorphismFixture(t)
	first := ReadAssertionUnder(f.personAssertion, mustIdentity(t, f.v1))
	if !first.Resolved() || first.Reading() != OntologySameVersion ||
		first.Predicate() != f.v1rel.ID() {
		t.Fatalf("same-version identity-only read = %#v", first)
	}
	other := ReadAssertionUnder(f.personAssertion, mustIdentity(t, f.v2))
	if other.Resolved() || other.Reading() != OntologyOtherVersion {
		t.Fatal("identity-only read silently reinterpreted another version")
	}
	unknown := ReadAssertionUnder(f.personAssertion, UnknownOntology())
	if unknown.Resolved() || unknown.Reading() != OntologyUnresolved {
		t.Fatal("identity-only read upgraded an unknown reader")
	}
}

func TestOntologyLensRejectsAbsentDefinitionInExactVersion(t *testing.T) {
	f := newMorphismFixture(t)
	future, err := NewPropertyDefinition(
		"future", "Future", "", ValueString, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	assertion := mustPropertyAssertion(
		t, f.person.ID(), future.ID(), f.v1, nil, f.evidence, f.provenance)
	lens, err := NewOntologyLensWithTransitions(f.v1, []OntologyTransition{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := lens.Read(assertion); got.Resolved() {
		t.Fatalf("exact-version read resolved absent predicate: %#v", got)
	}
}

func TestOntologyLensRejectsFutureDefinitionStampedWithSourceVersion(t *testing.T) {
	f := newMorphismFixture(t)
	future, err := NewPropertyDefinition(
		"future", "Future", "", ValueString, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	basePerson, err := NewConceptDefinition(
		"person", "Person", "", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	targetPerson, err := NewConceptDefinition(
		"person", "Person", "", []shoal.ID{future.ID()}, nil)
	if err != nil {
		t.Fatal(err)
	}
	schema, err := NewOntologySchema("future", "Future", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	at := time.Date(2026, time.September, 6, 0, 0, 0, 0, time.UTC)
	base, err := NewOntologyVersion(
		schema, "1", at, []ConceptDefinition{basePerson}, nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	target, err := NewOntologyVersion(
		schema, "2", at.Add(time.Second), []ConceptDefinition{targetPerson},
		nil, []PropertyDefinition{future}, nil)
	if err != nil {
		t.Fatal(err)
	}
	transition, err := NewOntologyTransition(base, target, nil)
	if err != nil {
		t.Fatal(err)
	}
	lens, err := NewOntologyLensWithTransitions(
		target, []OntologyTransition{transition}, nil)
	if err != nil {
		t.Fatal(err)
	}
	assertion := mustPropertyAssertion(
		t, basePerson.ID(), future.ID(), base, nil, f.evidence, f.provenance)
	if got := lens.Read(assertion); got.Resolved() {
		t.Fatalf("future definition retroactively resolved: %#v", got)
	}
}

func mustIdentity(t *testing.T, version OntologyVersion) OntologyIdentity {
	t.Helper()
	identity, err := NewOntologyIdentity(version)
	if err != nil {
		t.Fatal(err)
	}

	return identity
}

func TestMorphismBoundsAndProposalPayloadAccounting(t *testing.T) {
	f := newMorphismFixture(t)
	evidence := make([]EvidenceRef, MaxMorphismEvidence+1)
	for i := range evidence {
		evidence[i] = f.evidence
	}

	_, err := NewOntologyMorphism(MorphismConfig{
		Kind: MorphismRename, SourceVersion: f.renameFrom, TargetVersion: f.renameTo,
		Sources: []shoal.ID{f.oldRel.ID()}, Targets: []shoal.ID{f.newRel.ID()},
		Evidence: evidence, Rationale: "bounded evidence",
	})
	if err == nil || !strings.Contains(err.Error(), "evidence exceeds") {
		t.Fatalf("oversized evidence error = %v", err)
	}
	choices := make(map[string]shoal.ID, MaxMorphismDiscriminatorChoices+1)
	for i := 0; i <= MaxMorphismDiscriminatorChoices; i++ {
		choices[strconv.Itoa(i)] = f.person.ID()
	}
	if _, err := NewMorphismDiscriminator("kind", choices); err == nil ||
		!strings.Contains(err.Error(), "choices exceed") {
		t.Fatalf("oversized discriminator error = %v", err)
	}

	morphism := mustMorphism(t, MorphismConfig{
		Kind: MorphismRename, SourceVersion: f.renameFrom, TargetVersion: f.renameTo,
		Sources: []shoal.ID{f.oldRel.ID()}, Targets: []shoal.ID{f.newRel.ID()},
		Evidence: []EvidenceRef{f.evidence}, Rationale: strings.Repeat("r", 1024),
	})
	at := time.Date(2026, 9, 5, 12, 30, 0, 0, time.UTC)
	without, err := NewGovernedProposal(
		f.renameFrom.Schema(), f.renameFrom, f.renameTo,
		"author", "proposal", at, nil)
	if err != nil {
		t.Fatal(err)
	}
	with, err := NewGovernedProposalWithMorphisms(
		f.renameFrom.Schema(), f.renameFrom, f.renameTo,
		[]OntologyMorphism{morphism}, "author", "proposal", at, nil)
	if err != nil {
		t.Fatal(err)
	}
	if proposalPayloadBytes(with) <= proposalPayloadBytes(without) {
		t.Fatal("morphism payload was omitted from proposal accounting")
	}
}

func TestOntologyLensTraversesPublishedTransitionsWithoutMorphisms(t *testing.T) {
	f := newMorphismFixture(t)
	v3, err := NewOntologyVersion(
		f.v2.Schema(), "3", f.v2.CreatedAt().Add(time.Second),
		f.v2.Concepts(), f.v2.Relationships(), f.v2.Properties(), nil)
	if err != nil {
		t.Fatal(err)
	}
	first, err := NewOntologyTransition(
		f.v1, f.v2, []OntologyMorphism{mustMorphism(t, MorphismConfig{
			Kind: MorphismWiden, SourceVersion: f.v1, TargetVersion: f.v2,
			Sources: []shoal.ID{f.v1rel.ID()}, Targets: []shoal.ID{f.v2rel.ID()},
			Evidence: []EvidenceRef{f.evidence}, Rationale: "widen endpoints",
		})})
	if err != nil {
		t.Fatal(err)
	}
	second, err := NewOntologyTransition(
		f.v2, v3, nil)
	if err != nil {
		t.Fatal(err)
	}
	assertion := mustPropertyAssertion(
		t, f.person.ID(), f.name.ID(), f.v1, nil, f.evidence, f.provenance)
	lens, err := NewOntologyLensWithTransitions(
		v3, []OntologyTransition{first, second}, nil)
	if err != nil {
		t.Fatal(err)
	}
	read := lens.Read(assertion)
	if !read.Resolved() || read.Predicate() != f.name.ID() ||
		len(read.AppliedMorphisms()) != 0 {
		t.Fatalf("empty-transition read = %#v", read)
	}
}

func TestOntologyMorphismRejectsInvalidShapeBeforeSemanticLookup(t *testing.T) {
	f := newMorphismFixture(t)
	if _, err := NewOntologyMorphism(MorphismConfig{
		Kind: MorphismWiden, SourceVersion: f.v1, TargetVersion: f.v2,
		Evidence: []EvidenceRef{f.evidence}, Rationale: "missing relationship",
	}); err == nil {
		t.Fatal("empty widening did not return an error")
	}
	discriminator, err := NewMorphismDiscriminator(
		"kind", map[string]shoal.ID{"person": f.newRel.ID()})
	if err != nil {
		t.Fatal(err)
	}
	tests := []MorphismConfig{
		{
			Kind: MorphismWiden, SourceVersion: f.v1, TargetVersion: f.v2,
			Sources: []shoal.ID{f.v1rel.ID()}, Targets: []shoal.ID{f.v2rel.ID()},
		},
		{
			Kind: MorphismNarrow, SourceVersion: f.v2, TargetVersion: f.v1,
			Sources: []shoal.ID{f.v2rel.ID()}, Targets: []shoal.ID{f.v1rel.ID()},
		},
		{
			Kind: MorphismRename, SourceVersion: f.renameFrom, TargetVersion: f.renameTo,
			Sources: []shoal.ID{f.oldRel.ID()}, Targets: []shoal.ID{f.newRel.ID()},
		},
		{
			Kind: MorphismMerge, SourceVersion: f.splitTo, TargetVersion: f.splitFrom,
			Sources: []shoal.ID{f.person.ID(), f.org.ID()},
			Targets: []shoal.ID{f.party.ID()},
		},
	}
	for _, config := range tests {
		config.Discriminator = discriminator
		config.Evidence = []EvidenceRef{f.evidence}
		config.Rationale = "invalid discriminator"
		if _, err := NewOntologyMorphism(config); err == nil ||
			!strings.Contains(err.Error(), "only split") {
			t.Fatalf("%s discriminator error = %v", config.Kind, err)
		}
	}
}

func TestOntologyLensRejectsRemovedPropertyOwnership(t *testing.T) {
	f := newMorphismFixture(t)
	targetPerson, _ := NewConceptDefinition("person", "Person", "", nil, nil)
	source, err := NewOntologyVersion(
		f.v1.Schema(), "ownership-1", f.v1.CreatedAt().Add(10*time.Second),
		[]ConceptDefinition{f.person, f.org},
		[]RelationshipDefinition{f.oldRel},
		[]PropertyDefinition{f.name}, nil)
	if err != nil {
		t.Fatal(err)
	}
	target, err := NewOntologyVersion(
		f.v1.Schema(), "ownership-2", f.v1.CreatedAt().Add(11*time.Second),
		[]ConceptDefinition{targetPerson, f.org},
		[]RelationshipDefinition{f.newRel},
		[]PropertyDefinition{f.name}, nil)
	if err != nil {
		t.Fatal(err)
	}
	unrelated := mustMorphism(t, MorphismConfig{
		Kind: MorphismRename, SourceVersion: source, TargetVersion: target,
		Sources: []shoal.ID{f.oldRel.ID()}, Targets: []shoal.ID{f.newRel.ID()},
		Evidence: []EvidenceRef{f.evidence}, Rationale: "unrelated rename",
	})
	if _, err := NewOntologyTransition(
		source, target, []OntologyMorphism{unrelated},
	); err == nil {
		t.Fatal("ownership-removing identity-only transition was accepted")
	}
	lens, err := NewOntologyLensWithTransitions(
		target, []OntologyTransition{}, []OntologyMorphism{unrelated})
	if err != nil {
		t.Fatal(err)
	}
	assertion := mustPropertyAssertion(
		t, f.person.ID(), f.name.ID(), source, nil, f.evidence, f.provenance)
	if read := lens.Read(assertion); read.Resolved() ||
		!strings.Contains(read.Reason(), "no unique published morphism path") {
		t.Fatalf("removed property ownership read = %#v", read)
	}
}

func TestOntologyMorphismRejectsCrossKindMerge(t *testing.T) {
	f := newMorphismFixture(t)
	if _, err := NewOntologyMorphism(MorphismConfig{
		Kind: MorphismMerge, SourceVersion: f.splitTo, TargetVersion: f.splitFrom,
		Sources:  []shoal.ID{f.person.ID(), f.org.ID()},
		Targets:  []shoal.ID{f.name.ID()},
		Evidence: []EvidenceRef{f.evidence}, Rationale: "invalid cross-kind merge",
	}); err == nil || !strings.Contains(err.Error(), "same definition kind") {
		t.Fatalf("cross-kind merge error = %v", err)
	}
}

func TestProposalRequiresMorphismForRetainedRelationshipMeaningChange(t *testing.T) {
	f := newMorphismFixture(t)
	at := time.Date(2026, 9, 6, 2, 0, 0, 0, time.UTC)
	if _, err := NewGovernedProposal(
		f.v1.Schema(), f.v1, f.v2, "author",
		"silent endpoint widening", at, nil,
	); err != nil {
		t.Fatalf("governance draft should remain reviewable: %v", err)
	}
	if _, err := NewOntologyTransition(f.v1, f.v2, nil); err == nil ||
		!strings.Contains(err.Error(), "explicit morphism") {
		t.Fatalf("silent relationship transition error = %v", err)
	}
	widen := mustMorphism(t, MorphismConfig{
		Kind: MorphismWiden, SourceVersion: f.v1, TargetVersion: f.v2,
		Sources: []shoal.ID{f.v1rel.ID()}, Targets: []shoal.ID{f.v2rel.ID()},
		Evidence: []EvidenceRef{f.evidence}, Rationale: "widen endpoints",
	})
	if _, err := NewGovernedProposalWithMorphisms(
		f.v1.Schema(), f.v1, f.v2, []OntologyMorphism{widen},
		"author", "governed endpoint widening", at, nil,
	); err != nil {
		t.Fatalf("governed relationship change rejected: %v", err)
	}
	if _, err := NewOntologyTransition(
		f.v1, f.v2, []OntologyMorphism{widen},
	); err != nil {
		t.Fatalf("governed relationship transition rejected: %v", err)
	}
}

func TestProposalAllowsOnlyOptionalAdditiveConceptProperties(t *testing.T) {
	f := newMorphismFixture(t)
	nickname, _ := NewPropertyDefinition(
		"nickname", "Nickname", "", ValueString, nil, nil)
	person, _ := NewConceptDefinition(
		"person", "Person", "", []shoal.ID{f.name.ID(), nickname.ID()}, nil)
	target, err := NewOntologyVersion(
		f.splitFrom.Schema(), "optional-addition",
		f.splitFrom.CreatedAt().Add(time.Second),
		[]ConceptDefinition{person}, nil,
		[]PropertyDefinition{f.name, nickname}, nil)
	if err != nil {
		t.Fatal(err)
	}
	sourcePerson, _ := NewConceptDefinition(
		"person", "Person", "", []shoal.ID{f.name.ID()}, nil)
	source, err := NewOntologyVersion(
		f.splitFrom.Schema(), "optional-base",
		f.splitFrom.CreatedAt(),
		[]ConceptDefinition{sourcePerson}, nil,
		[]PropertyDefinition{f.name}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := NewOntologyTransition(source, target, nil); err != nil {
		t.Fatalf("optional additive property transition rejected: %v", err)
	}
	required, _ := NewFlagConstraint(ConstraintRequired)
	requiredNickname, _ := NewPropertyDefinition(
		"nickname", "Nickname", "", ValueString, []Constraint{required}, nil)
	requiredPerson, _ := NewConceptDefinition(
		"person", "Person", "",
		[]shoal.ID{f.name.ID(), requiredNickname.ID()}, nil)
	requiredTarget, err := NewOntologyVersion(
		source.Schema(), "required-addition", target.CreatedAt().Add(time.Second),
		[]ConceptDefinition{requiredPerson}, nil,
		[]PropertyDefinition{f.name, requiredNickname}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := NewOntologyTransition(source, requiredTarget, nil); err == nil ||
		!strings.Contains(err.Error(), "retained concept") {
		t.Fatalf("required additive property error = %v", err)
	}
}

func TestOntologyLensReportsOnlyMorphismsAppliedToAssertion(t *testing.T) {
	f := newMorphismFixture(t)
	otherOld, _ := NewRelationshipDefinition(
		"assigned-to", "Assigned To", "", []shoal.ID{f.person.ID()},
		[]shoal.ID{f.org.ID()}, nil, true, nil)
	otherNew, _ := NewRelationshipDefinition(
		"allocated-to", "Allocated To", "", []shoal.ID{f.person.ID()},
		[]shoal.ID{f.org.ID()}, nil, true, nil)
	source, err := NewOntologyVersion(
		f.renameFrom.Schema(), "multi-rename-1",
		f.renameFrom.CreatedAt().Add(20*time.Second),
		[]ConceptDefinition{f.person, f.org},
		[]RelationshipDefinition{f.oldRel, otherOld},
		[]PropertyDefinition{f.name}, nil)
	if err != nil {
		t.Fatal(err)
	}
	target, err := NewOntologyVersion(
		f.renameFrom.Schema(), "multi-rename-2",
		f.renameFrom.CreatedAt().Add(21*time.Second),
		[]ConceptDefinition{f.person, f.org},
		[]RelationshipDefinition{f.newRel, otherNew},
		[]PropertyDefinition{f.name}, nil)
	if err != nil {
		t.Fatal(err)
	}
	used := mustMorphism(t, MorphismConfig{
		Kind: MorphismRename, SourceVersion: source, TargetVersion: target,
		Sources: []shoal.ID{f.oldRel.ID()}, Targets: []shoal.ID{f.newRel.ID()},
		Evidence: []EvidenceRef{f.evidence}, Rationale: "rename used relationship",
	})
	unrelated := mustMorphism(t, MorphismConfig{
		Kind: MorphismRename, SourceVersion: source, TargetVersion: target,
		Sources: []shoal.ID{otherOld.ID()}, Targets: []shoal.ID{otherNew.ID()},
		Evidence: []EvidenceRef{f.evidence}, Rationale: "rename unrelated relationship",
	})
	transition, err := NewOntologyTransition(
		source, target, []OntologyMorphism{used, unrelated})
	if err != nil {
		t.Fatal(err)
	}
	lens, err := NewOntologyLensWithTransitions(
		target, []OntologyTransition{transition},
		[]OntologyMorphism{used, unrelated})
	if err != nil {
		t.Fatal(err)
	}
	assertion := mustRelationshipAssertion(
		t, f.person.ID(), f.oldRel.ID(), f.org.ID(),
		source, f.evidence, f.provenance)
	read := lens.Read(assertion)
	applied := read.AppliedMorphisms()
	if !read.Resolved() || len(applied) != 1 || applied[0] != used.ID() {
		t.Fatalf("applied morphisms = %v, want only %s", applied, used.ID())
	}
}

func TestProposalRejectsRestrictingPreviouslyUnownedProperty(t *testing.T) {
	schema, _ := NewOntologySchema("owners", "Owners", "", nil)
	property, _ := NewPropertyDefinition(
		"name", "Name", "", ValueString, nil, nil)
	first, _ := NewConceptDefinition("first", "First", "", nil, nil)
	second, _ := NewConceptDefinition("second", "Second", "", nil, nil)
	restricted, _ := NewConceptDefinition(
		"first", "First", "", []shoal.ID{property.ID()}, nil)
	at := time.Date(2026, time.September, 6, 1, 0, 0, 0, time.UTC)
	base, _ := NewOntologyVersion(
		schema, "1", at, []ConceptDefinition{first, second}, nil,
		[]PropertyDefinition{property}, nil)
	target, _ := NewOntologyVersion(
		schema, "2", at.Add(time.Second),
		[]ConceptDefinition{restricted, second}, nil,
		[]PropertyDefinition{property}, nil)
	if _, err := NewOntologyTransition(base, target, nil); err == nil {
		t.Fatal("unowned property became owner-restricted without an explicit transformation")
	}
}

func TestInferredLensValidatesCompleteEvolution(t *testing.T) {
	f := newMorphismFixture(t)
	concepts := f.v2.Concepts()
	for index, concept := range concepts {
		if concept.ID() != f.person.ID() {
			continue
		}
		changed, err := NewConceptDefinition(
			concept.Key(), concept.Name(), "changed without a morphism",
			concept.Properties(), concept.Metadata())
		if err != nil {
			t.Fatal(err)
		}
		concepts[index] = changed
	}
	target, err := NewOntologyVersion(
		f.v2.Schema(), "inferred-invalid",
		f.v2.CreatedAt().Add(time.Second), concepts,
		f.v2.Relationships(), f.v2.Properties(), f.v2.Metadata())
	if err != nil {
		t.Fatal(err)
	}
	widen := mustMorphism(t, MorphismConfig{
		Kind: MorphismWiden, SourceVersion: f.v1, TargetVersion: target,
		Sources: []shoal.ID{f.v1rel.ID()}, Targets: []shoal.ID{f.v2rel.ID()},
		Evidence: []EvidenceRef{f.evidence}, Rationale: "valid endpoint widening",
	})
	if _, err := NewOntologyLens(target, []OntologyMorphism{widen}); err == nil {
		t.Fatal("inferred lens accepted unrelated semantic changes")
	}
}

func TestExplicitTransitionUsesItsGovernedMorphisms(t *testing.T) {
	f := newMorphismFixture(t)
	schema, _ := NewOntologySchema("bound", "Bound", "", nil)
	sourceProperty, _ := NewPropertyDefinition(
		"old", "Old", "", ValueString, nil, nil)
	governedProperty, _ := NewPropertyDefinition(
		"governed", "Governed", "", ValueString, nil, nil)
	otherProperty, _ := NewPropertyDefinition(
		"other", "Other", "", ValueString, nil, nil)
	sourceConcept, _ := NewConceptDefinition(
		"person", "Person", "", []shoal.ID{sourceProperty.ID()}, nil)
	targetConcept, _ := NewConceptDefinition(
		"person", "Person", "",
		[]shoal.ID{governedProperty.ID(), otherProperty.ID()}, nil)
	at := time.Date(2026, time.September, 6, 2, 0, 0, 0, time.UTC)
	source, _ := NewOntologyVersion(
		schema, "1", at, []ConceptDefinition{sourceConcept}, nil,
		[]PropertyDefinition{sourceProperty}, nil)
	target, _ := NewOntologyVersion(
		schema, "2", at.Add(time.Second), []ConceptDefinition{targetConcept}, nil,
		[]PropertyDefinition{governedProperty, otherProperty}, nil)
	governed := mustMorphism(t, MorphismConfig{
		Kind: MorphismRename, SourceVersion: source, TargetVersion: target,
		Sources:  []shoal.ID{sourceProperty.ID()},
		Targets:  []shoal.ID{governedProperty.ID()},
		Evidence: []EvidenceRef{f.evidence}, Rationale: "governed rename",
	})
	other := mustMorphism(t, MorphismConfig{
		Kind: MorphismRename, SourceVersion: source, TargetVersion: target,
		Sources:  []shoal.ID{sourceProperty.ID()},
		Targets:  []shoal.ID{otherProperty.ID()},
		Evidence: []EvidenceRef{f.evidence}, Rationale: "ungoverned rename",
	})
	transition, err := NewOntologyTransition(
		source, target, []OntologyMorphism{governed})
	if err != nil {
		t.Fatal(err)
	}
	lens, err := NewOntologyLensWithTransitions(
		target, []OntologyTransition{transition}, []OntologyMorphism{other})
	if err != nil {
		t.Fatal(err)
	}
	assertion := mustPropertyAssertion(
		t, sourceConcept.ID(), sourceProperty.ID(), source, nil,
		f.evidence, f.provenance)
	read := lens.Read(assertion)
	if !read.Resolved() || read.Predicate() != governedProperty.ID() {
		t.Fatalf("explicit transition used ungoverned morphism: %#v", read)
	}
}

func TestOntologyLensTraversesBeyondLegacyDepth(t *testing.T) {
	f := newMorphismFixture(t)
	schema, _ := NewOntologySchema("long", "Long", "", nil)
	property, _ := NewPropertyDefinition(
		"name", "Name", "", ValueString, nil, nil)
	person, _ := NewConceptDefinition(
		"person", "Person", "", []shoal.ID{property.ID()}, nil)
	at := time.Date(2026, time.September, 6, 3, 0, 0, 0, time.UTC)
	versions := make([]OntologyVersion, 41)
	for index := range versions {
		version, err := NewOntologyVersion(
			schema, strconv.Itoa(index), at.Add(time.Duration(index)*time.Second),
			[]ConceptDefinition{person}, nil,
			[]PropertyDefinition{property}, nil)
		if err != nil {
			t.Fatal(err)
		}
		versions[index] = version
	}
	transitions := make([]OntologyTransition, 0, len(versions)-1)
	for index := 1; index < len(versions); index++ {
		transition, err := NewOntologyTransition(
			versions[index-1], versions[index], nil)
		if err != nil {
			t.Fatal(err)
		}
		transitions = append(transitions, transition)
	}
	lens, err := NewOntologyLensWithTransitions(
		versions[len(versions)-1], transitions, nil)
	if err != nil {
		t.Fatal(err)
	}
	assertion := mustPropertyAssertion(
		t, person.ID(), property.ID(), versions[0], nil,
		f.evidence, f.provenance)
	if read := lens.Read(assertion); !read.Resolved() {
		t.Fatalf("governed 40-hop lens read = %#v", read)
	}
}

func TestOntologyTransitionRejectsMorphismFromDifferentVersions(t *testing.T) {
	f := newMorphismFixture(t)
	widen := mustMorphism(t, MorphismConfig{
		Kind: MorphismWiden, SourceVersion: f.v1, TargetVersion: f.v2,
		Sources: []shoal.ID{f.v1rel.ID()}, Targets: []shoal.ID{f.v2rel.ID()},
		Evidence: []EvidenceRef{f.evidence}, Rationale: "widen original schema",
	})
	foreignSchema, _ := NewOntologySchema("foreign", "Foreign", "", nil)
	source, err := NewOntologyVersion(
		foreignSchema, "1", f.v1.CreatedAt(),
		f.v1.Concepts(), f.v1.Relationships(), f.v1.Properties(), nil)
	if err != nil {
		t.Fatal(err)
	}
	target, err := NewOntologyVersion(
		foreignSchema, "2", f.v2.CreatedAt(),
		f.v2.Concepts(), f.v2.Relationships(), f.v2.Properties(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := NewOntologyTransition(
		source, target, []OntologyMorphism{widen},
	); err == nil || !strings.Contains(err.Error(), "does not connect") {
		t.Fatalf("foreign-version morphism error = %v", err)
	}
}

func TestOntologyTransitionMapsRenamedOwnedProperty(t *testing.T) {
	f := newMorphismFixture(t)
	oldProperty, _ := NewPropertyDefinition(
		"display-name", "Display Name", "", ValueString, nil, nil)
	newProperty, _ := NewPropertyDefinition(
		"preferred-name", "Preferred Name", "", ValueString, nil, nil)
	sourcePerson, _ := NewConceptDefinition(
		"person", "Person", "", []shoal.ID{oldProperty.ID()}, nil)
	targetPerson, _ := NewConceptDefinition(
		"person", "Person", "", []shoal.ID{newProperty.ID()}, nil)
	source, _ := NewOntologyVersion(
		f.v1.Schema(), "property-rename-1", f.v1.CreatedAt().Add(30*time.Second),
		[]ConceptDefinition{sourcePerson}, nil,
		[]PropertyDefinition{oldProperty}, nil)
	target, _ := NewOntologyVersion(
		f.v1.Schema(), "property-rename-2", f.v1.CreatedAt().Add(31*time.Second),
		[]ConceptDefinition{targetPerson}, nil,
		[]PropertyDefinition{newProperty}, nil)
	rename := mustMorphism(t, MorphismConfig{
		Kind: MorphismRename, SourceVersion: source, TargetVersion: target,
		Sources: []shoal.ID{oldProperty.ID()}, Targets: []shoal.ID{newProperty.ID()},
		Evidence: []EvidenceRef{f.evidence}, Rationale: "rename owned property",
	})
	transition, err := NewOntologyTransition(
		source, target, []OntologyMorphism{rename})
	if err != nil {
		t.Fatalf("property rename transition rejected: %v", err)
	}
	lens, err := NewOntologyLensWithTransitions(
		target, []OntologyTransition{transition}, []OntologyMorphism{rename})
	if err != nil {
		t.Fatal(err)
	}
	assertion := mustPropertyAssertion(
		t, sourcePerson.ID(), oldProperty.ID(), source,
		nil, f.evidence, f.provenance)
	read := lens.Read(assertion)
	if !read.Resolved() || read.Predicate() != newProperty.ID() {
		t.Fatalf("property rename interpretation = %#v", read)
	}
}

func TestOntologyTransitionRejectsUnmappedRemovalBeforeReintroduction(t *testing.T) {
	schema, _ := NewOntologySchema("revival", "Revival", "", nil)
	person, _ := NewConceptDefinition("person", "Person", "", nil, nil)
	at := time.Date(2026, 9, 6, 3, 0, 0, 0, time.UTC)
	v1, _ := NewOntologyVersion(
		schema, "1", at, []ConceptDefinition{person}, nil, nil, nil)
	v2, _ := NewOntologyVersion(
		schema, "2", at.Add(time.Second), nil, nil, nil, nil)
	v3, _ := NewOntologyVersion(
		schema, "3", at.Add(2*time.Second),
		[]ConceptDefinition{person}, nil, nil, nil)
	if _, err := NewOntologyTransition(v1, v2, nil); err == nil ||
		!strings.Contains(err.Error(), "removed concept") {
		t.Fatalf("unmapped removal error = %v", err)
	}
	if _, err := NewOntologyTransition(v2, v3, nil); err != nil {
		t.Fatalf("additive reintroduction transition rejected: %v", err)
	}
}

type morphismFixture struct {
	v1, v2, renameFrom, renameTo, splitFrom, splitTo                      OntologyVersion
	v1rel, v2rel, oldRel, newRel                                          RelationshipDefinition
	person, contractor, org, party                                        ConceptDefinition
	name                                                                  PropertyDefinition
	evidence                                                              EvidenceRef
	provenance                                                            ExtractionProvenance
	personAssertion, contractorAssertion, renameAssertion, splitAssertion Assertion
}

func newMorphismFixture(t *testing.T) morphismFixture {
	t.Helper()
	schema, _ := NewOntologySchema("fleet", "Fleet", "", nil)
	name, _ := NewPropertyDefinition("name", "Name", "", ValueString, nil, nil)
	person, _ := NewConceptDefinition("person", "Person", "", []shoal.ID{name.ID()}, nil)
	contractor, _ := NewConceptDefinition("contractor", "Contractor", "", nil, nil)
	org, _ := NewConceptDefinition("org", "Organization", "", []shoal.ID{name.ID()}, nil)
	party, _ := NewConceptDefinition("party", "Party", "", []shoal.ID{name.ID()}, nil)
	v1rel, _ := NewRelationshipDefinition(
		"works-for", "Works For", "", []shoal.ID{person.ID()},
		[]shoal.ID{org.ID()}, nil, true, nil)
	v2rel, _ := NewRelationshipDefinition(
		"works-for", "Works For", "", []shoal.ID{person.ID(), contractor.ID()},
		[]shoal.ID{org.ID()}, nil, true, nil)
	oldRel, _ := NewRelationshipDefinition(
		"employed-by", "Employed By", "", []shoal.ID{person.ID()},
		[]shoal.ID{org.ID()}, nil, true, nil)
	newRel, _ := NewRelationshipDefinition(
		"works-at", "Works At", "", []shoal.ID{person.ID()},
		[]shoal.ID{org.ID()}, nil, true, nil)
	at := time.Date(2026, 9, 5, 12, 0, 0, 0, time.UTC)
	version := func(label string, concepts []ConceptDefinition, rels []RelationshipDefinition) OntologyVersion {
		v, err := NewOntologyVersion(schema, label, at, concepts, rels, []PropertyDefinition{name}, nil)
		if err != nil {
			t.Fatal(err)
		}
		at = at.Add(time.Second)
		return v
	}
	f := morphismFixture{
		v1:         version("1", []ConceptDefinition{person, contractor, org}, []RelationshipDefinition{v1rel}),
		v2:         version("2", []ConceptDefinition{person, contractor, org}, []RelationshipDefinition{v2rel}),
		renameFrom: version("rename-1", []ConceptDefinition{person, org}, []RelationshipDefinition{oldRel}),
		renameTo:   version("rename-2", []ConceptDefinition{person, org}, []RelationshipDefinition{newRel}),
		splitFrom:  version("split-1", []ConceptDefinition{party}, nil),
		splitTo:    version("split-2", []ConceptDefinition{person, org}, nil),
		v1rel:      v1rel, v2rel: v2rel, oldRel: oldRel, newRel: newRel,
		person: person, contractor: contractor, org: org, party: party, name: name,
	}
	citation := document.Citation{
		DocumentID: "doc", RevisionID: "rev", SectionID: "section", SpanID: "span",
		Range: document.SourceRange{
			Start: document.SourcePosition{Offset: 0, Page: 1},
			End:   document.SourcePosition{Offset: 4, Page: 1},
		},
	}
	f.evidence, _ = NewEvidenceRef(citation, "fact", nil)
	f.provenance, _ = NewExtractionProvenance(
		"provider", "model", "1", "prompt", "1", "extractor", "1", nil)
	f.personAssertion = mustRelationshipAssertion(
		t, person.ID(), v1rel.ID(), org.ID(), f.v1, f.evidence, f.provenance)
	f.contractorAssertion = mustRelationshipAssertion(
		t, contractor.ID(), v2rel.ID(), org.ID(), f.v2, f.evidence, f.provenance)
	f.renameAssertion = mustRelationshipAssertion(
		t, person.ID(), oldRel.ID(), org.ID(), f.renameFrom, f.evidence, f.provenance)
	f.splitAssertion = mustPropertyAssertion(
		t, party.ID(), name.ID(), f.splitFrom,
		shoal.Metadata{"kind": "person"}, f.evidence, f.provenance)
	return f
}

func mustMorphism(t *testing.T, config MorphismConfig) OntologyMorphism {
	t.Helper()
	m, err := NewOntologyMorphism(config)
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func mustRelationshipAssertion(
	t *testing.T, subjectType, predicate, objectType shoal.ID,
	version OntologyVersion, evidence EvidenceRef, provenance ExtractionProvenance,
) Assertion {
	t.Helper()
	value, _ := NewReferenceValue("object")
	identity, _ := NewOntologyIdentity(version)
	assertion, err := NewAssertion(
		"subject", predicate, value, AssertionExplicit, 1,
		[]EvidenceRef{evidence}, provenance, nil,
		WithAssertionSubjectType(subjectType), WithAssertionObjectType(objectType),
		WithAssertionOntology(identity))
	if err != nil {
		t.Fatal(err)
	}
	return assertion
}

func mustPropertyAssertion(
	t *testing.T, subjectType, predicate shoal.ID, version OntologyVersion,
	metadata shoal.Metadata, evidence EvidenceRef, provenance ExtractionProvenance,
) Assertion {
	t.Helper()
	value, _ := NewStringValue("Ada")
	options := []AssertionOption{WithAssertionSubjectType(subjectType)}
	if version.ID() != "" {
		identity, _ := NewOntologyIdentity(version)
		options = append(options, WithAssertionOntology(identity))
	}
	assertion, err := NewAssertion(
		"subject", predicate, value, AssertionExplicit, 1,
		[]EvidenceRef{evidence}, provenance, metadata, options...)
	if err != nil {
		t.Fatal(err)
	}
	return assertion
}
