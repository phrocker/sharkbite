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

package explorer

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/phrocker/shoal-oss/pkg/document"
	"github.com/phrocker/shoal-oss/pkg/ontology"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

func TestPublishedMorphismAndHistoricAssertionSurviveRestart(t *testing.T) {
	root := filepath.Join(t.TempDir(), "corpus")
	schema, _ := ontology.NewOntologySchema("restart", "Restart", "", nil)
	person, _ := ontology.NewConceptDefinition("person", "Person", "", nil, nil)
	org, _ := ontology.NewConceptDefinition("org", "Org", "", nil, nil)
	oldRel, _ := ontology.NewRelationshipDefinition(
		"member-of", "Member Of", "", []shoal.ID{person.ID()},
		[]shoal.ID{org.ID()}, nil, true, nil)
	newRel, _ := ontology.NewRelationshipDefinition(
		"belongs-to", "Belongs To", "", []shoal.ID{person.ID()},
		[]shoal.ID{org.ID()}, nil, true, nil)
	at := time.Date(2026, 9, 5, 12, 0, 0, 0, time.UTC)
	base, _ := ontology.NewOntologyVersion(
		schema, "1", at, []ontology.ConceptDefinition{person, org},
		[]ontology.RelationshipDefinition{oldRel}, nil, nil)
	target, _ := ontology.NewOntologyVersion(
		schema, "2", at.Add(time.Second), []ontology.ConceptDefinition{person, org},
		[]ontology.RelationshipDefinition{newRel}, nil, nil)
	evidence, _ := ontology.NewEvidenceRef(document.Citation{
		DocumentID: "doc", RevisionID: "rev", SectionID: "section", SpanID: "span",
		Range: document.SourceRange{
			Start: document.SourcePosition{Offset: 0, Page: 1},
			End:   document.SourcePosition{Offset: 4, Page: 1},
		},
	}, "rename evidence", nil)
	morphism, err := ontology.NewOntologyMorphism(ontology.MorphismConfig{
		Kind: ontology.MorphismRename, SourceVersion: base, TargetVersion: target,
		Sources: []shoal.ID{oldRel.ID()}, Targets: []shoal.ID{newRel.ID()},
		Evidence: []ontology.EvidenceRef{evidence}, Rationale: "rename relationship",
	})
	if err != nil {
		t.Fatal(err)
	}

	proposal, err := ontology.NewGovernedProposalWithMorphisms(
		schema, base, target, []ontology.OntologyMorphism{morphism},
		"author", "rename", at.Add(2*time.Second), nil)
	if err != nil {
		t.Fatal(err)
	}
	corpus, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := corpus.CreateOntologyProposal(ctx, proposal, base); err != nil {
		t.Fatal(err)
	}
	for index, state := range []ontology.ProposalState{
		ontology.ProposalSubmitted, ontology.ProposalApproved, ontology.ProposalPublished,
	} {
		proposal, err = corpus.TransitionOntologyProposal(
			ctx, proposal.ID(), state, "governor", "approved",
			at.Add(time.Duration(index+3)*time.Second))
		if err != nil {
			t.Fatal(err)
		}
	}

	if err := corpus.Close(); err != nil {
		t.Fatal(err)
	}
	corpus, err = Open(root)
	if err != nil {
		t.Fatal(err)
	}
	defer corpus.Close()
	proposals, err := corpus.OntologyProposals(ctx)
	if err != nil || len(proposals) != 1 || len(proposals[0].Morphisms()) != 1 ||
		proposals[0].State() != ontology.ProposalPublished {
		t.Fatalf("restored proposals = %#v, err = %v", proposals, err)
	}
	provenance, _ := ontology.NewExtractionProvenance(
		"provider", "model", "1", "prompt", "1", "extractor", "1", nil)
	value, _ := ontology.NewReferenceValue("org-1")
	baseIdentity, _ := ontology.NewOntologyIdentity(base)
	assertion, err := ontology.NewAssertion(
		"person-1", oldRel.ID(), value, ontology.AssertionExplicit, 1,
		[]ontology.EvidenceRef{evidence}, provenance, nil,
		ontology.WithAssertionSubjectType(person.ID()),
		ontology.WithAssertionObjectType(org.ID()),
		ontology.WithAssertionOntology(baseIdentity))
	if err != nil {
		t.Fatal(err)
	}
	targetIdentity, _ := ontology.NewOntologyIdentity(target)
	read, err := corpus.InterpretAssertions(
		ctx, []ontology.Assertion{assertion}, targetIdentity)
	if err != nil || len(read) != 1 || !read[0].Resolved() ||
		read[0].Predicate() != newRel.ID() ||
		read[0].Original().Predicate() != oldRel.ID() {
		t.Fatalf("restart interpretation = %#v, err = %v", read, err)
	}
}

func TestInterpretAssertionsResolvesSameVersionWithoutProposalCatalog(t *testing.T) {
	corpus, err := Open(filepath.Join(t.TempDir(), "corpus"))
	if err != nil {
		t.Fatal(err)
	}
	defer corpus.Close()
	schema, _ := ontology.NewOntologySchema("direct", "Direct", "", nil)
	property, _ := ontology.NewPropertyDefinition(
		"name", "Name", "", ontology.ValueString, nil, nil)
	concept, _ := ontology.NewConceptDefinition(
		"person", "Person", "", []shoal.ID{property.ID()}, nil)
	version, _ := ontology.NewOntologyVersion(
		schema, "1", time.Date(2026, 9, 5, 12, 0, 0, 0, time.UTC),
		[]ontology.ConceptDefinition{concept}, nil,
		[]ontology.PropertyDefinition{property}, nil)
	evidence, _ := ontology.NewEvidenceRef(document.Citation{
		DocumentID: "doc", RevisionID: "rev", SectionID: "section", SpanID: "span",
		Range: document.SourceRange{
			Start: document.SourcePosition{Offset: 0, Page: 1},
			End:   document.SourcePosition{Offset: 4, Page: 1},
		},
	}, "name", nil)
	provenance, _ := ontology.NewExtractionProvenance(
		"provider", "model", "1", "prompt", "1", "extractor", "1", nil)
	value, _ := ontology.NewStringValue("Ada")
	identity, _ := ontology.NewOntologyIdentity(version)
	assertion, err := ontology.NewAssertion(
		"person-1", property.ID(), value, ontology.AssertionExplicit, 1,
		[]ontology.EvidenceRef{evidence}, provenance, nil,
		ontology.WithAssertionSubjectType(concept.ID()),
		ontology.WithAssertionOntology(identity))
	if err != nil {
		t.Fatal(err)
	}
	read, err := corpus.InterpretAssertions(
		context.Background(), []ontology.Assertion{assertion}, identity)
	if err != nil || len(read) != 1 || !read[0].Resolved() ||
		read[0].Predicate() != property.ID() {
		t.Fatalf("same-version interpretation = %#v, err = %v", read, err)
	}
}

func TestPublishedTransitionWithoutMorphismsRemainsReadable(t *testing.T) {
	corpus, err := Open(filepath.Join(t.TempDir(), "corpus"))
	if err != nil {
		t.Fatal(err)
	}

	defer corpus.Close()
	schema, _ := ontology.NewOntologySchema("additive", "Additive", "", nil)
	property, _ := ontology.NewPropertyDefinition(
		"name", "Name", "", ontology.ValueString, nil, nil)
	person, _ := ontology.NewConceptDefinition(
		"person", "Person", "", []shoal.ID{property.ID()}, nil)
	organization, _ := ontology.NewConceptDefinition(
		"organization", "Organization", "", nil, nil)
	at := time.Date(2026, 9, 6, 0, 0, 0, 0, time.UTC)
	base, _ := ontology.NewOntologyVersion(
		schema, "1", at, []ontology.ConceptDefinition{person}, nil,
		[]ontology.PropertyDefinition{property}, nil)
	target, _ := ontology.NewOntologyVersion(
		schema, "2", at.Add(time.Second),
		[]ontology.ConceptDefinition{person, organization}, nil,
		[]ontology.PropertyDefinition{property}, nil)
	proposal, err := ontology.NewGovernedProposal(
		schema, base, target, "author", "add organization",
		at.Add(2*time.Second), nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := corpus.CreateOntologyProposal(ctx, proposal, base); err != nil {
		t.Fatal(err)
	}
	for index, state := range []ontology.ProposalState{
		ontology.ProposalSubmitted, ontology.ProposalApproved, ontology.ProposalPublished,
	} {
		proposal, err = corpus.TransitionOntologyProposal(
			ctx, proposal.ID(), state, "governor", "approved",
			at.Add(time.Duration(index+3)*time.Second))
		if err != nil {
			t.Fatal(err)
		}
	}

	evidence, _ := ontology.NewEvidenceRef(document.Citation{
		DocumentID: "doc", RevisionID: "rev", SectionID: "section", SpanID: "span",
		Range: document.SourceRange{
			Start: document.SourcePosition{Offset: 0, Page: 1},
			End:   document.SourcePosition{Offset: 4, Page: 1},
		},
	}, "name", nil)
	provenance, _ := ontology.NewExtractionProvenance(
		"provider", "model", "1", "prompt", "1", "extractor", "1", nil)
	value, _ := ontology.NewStringValue("Ada")
	baseIdentity, _ := ontology.NewOntologyIdentity(base)
	assertion, err := ontology.NewAssertion(
		"person-1", property.ID(), value, ontology.AssertionExplicit, 1,
		[]ontology.EvidenceRef{evidence}, provenance, nil,
		ontology.WithAssertionSubjectType(person.ID()),
		ontology.WithAssertionOntology(baseIdentity))
	if err != nil {
		t.Fatal(err)
	}
	targetIdentity, _ := ontology.NewOntologyIdentity(target)
	read, err := corpus.InterpretAssertions(
		ctx, []ontology.Assertion{assertion}, targetIdentity)
	if err != nil || len(read) != 1 || !read[0].Resolved() ||
		len(read[0].AppliedMorphisms()) != 0 {
		t.Fatalf("additive transition read = %#v, err = %v", read, err)
	}
}

func TestIndeterminateOntologyMutationBlocksFurtherWritesUntilReopen(t *testing.T) {
	schema, _ := ontology.NewOntologySchema("indeterminate", "Indeterminate", "", nil)
	at := time.Date(2026, 9, 6, 0, 0, 0, 0, time.UTC)
	base, _ := ontology.NewOntologyVersion(schema, "1", at, nil, nil, nil, nil)
	target, _ := ontology.NewOntologyVersion(
		schema, "2", at.Add(time.Second), nil, nil, nil, nil)
	proposal, _ := ontology.NewGovernedProposal(
		schema, base, target, "author", "proposal", at.Add(2*time.Second), nil)

	t.Run("create", func(t *testing.T) {
		corpus, err := Open(filepath.Join(t.TempDir(), "corpus"))
		if err != nil {
			t.Fatal(err)
		}
		defer corpus.Close()
		corpus.mu.Lock()
		corpus.ontologyMutationIndeterminate = true
		corpus.mu.Unlock()
		if err := corpus.CreateOntologyProposal(
			context.Background(), proposal, base,
		); !shoal.IsErrorCode(err, shoal.ErrorUnavailable) {
			t.Fatalf("create after indeterminate mutation = %v", err)
		}
		if _, err := corpus.OntologyProposals(
			context.Background(),
		); !shoal.IsErrorCode(err, shoal.ErrorUnavailable) {
			t.Fatalf("read after indeterminate mutation = %v", err)
		}
		if _, err := corpus.OntologyProposalMutationState(
			context.Background(), base, "",
		); !shoal.IsErrorCode(err, shoal.ErrorUnavailable) {
			t.Fatalf("mutation state after indeterminate mutation = %v", err)
		}
		if _, err := corpus.PublishedOntologyCatalog(
			context.Background(), base,
		); !shoal.IsErrorCode(err, shoal.ErrorUnavailable) {
			t.Fatalf("catalog after indeterminate mutation = %v", err)
		}
		selected, _ := ontology.NewOntologyIdentity(target)
		if _, err := corpus.InterpretAssertions(
			context.Background(), nil, selected,
		); !shoal.IsErrorCode(err, shoal.ErrorUnavailable) {
			t.Fatalf("lens read after indeterminate mutation = %v", err)
		}
	})
	t.Run("transition", func(t *testing.T) {
		data := filepath.Join(t.TempDir(), "corpus")
		corpus, err := Open(data)
		if err != nil {
			t.Fatal(err)
		}
		if err := corpus.CreateOntologyProposal(
			context.Background(), proposal, base,
		); err != nil {
			t.Fatal(err)
		}
		corpus.mu.Lock()
		corpus.ontologyMutationIndeterminate = true
		corpus.mu.Unlock()
		if _, err := corpus.TransitionOntologyProposal(
			context.Background(), proposal.ID(), ontology.ProposalSubmitted,
			"governor", "submit", at.Add(3*time.Second),
		); !shoal.IsErrorCode(err, shoal.ErrorUnavailable) {
			t.Fatalf("transition after indeterminate mutation = %v", err)
		}
		if _, err := corpus.OntologyProposals(
			context.Background(),
		); !shoal.IsErrorCode(err, shoal.ErrorUnavailable) {
			t.Fatalf("read after indeterminate transition = %v", err)
		}
		if _, err := corpus.OntologyProposalMutationState(
			context.Background(), base, proposal.ID(),
		); !shoal.IsErrorCode(err, shoal.ErrorUnavailable) {
			t.Fatalf("mutation state after indeterminate transition = %v", err)
		}
		if _, err := corpus.PublishedOntologyCatalog(
			context.Background(), base,
		); !shoal.IsErrorCode(err, shoal.ErrorUnavailable) {
			t.Fatalf("catalog after indeterminate transition = %v", err)
		}
		selected, _ := ontology.NewOntologyIdentity(target)
		if _, err := corpus.InterpretAssertions(
			context.Background(), nil, selected,
		); !shoal.IsErrorCode(err, shoal.ErrorUnavailable) {
			t.Fatalf("lens read after indeterminate transition = %v", err)
		}
		if err := corpus.Close(); err != nil {
			t.Fatal(err)
		}
		corpus, err = Open(data)
		if err != nil {
			t.Fatal(err)
		}
		defer corpus.Close()
		stored, err := corpus.OntologyProposals(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if len(stored) != 1 || stored[0].State() != ontology.ProposalDraft {
			t.Fatalf("blocked transition mutated proposal = %#v", stored)
		}
		if _, err := corpus.InterpretAssertions(
			context.Background(), nil, selected,
		); err != nil {
			t.Fatalf("lens read after reopen = %v", err)
		}
		state, err := corpus.OntologyProposalMutationState(
			context.Background(), base, proposal.ID())
		if err != nil || !state.ProposalFound() {
			t.Fatalf("mutation state after reopen = %#v, %v", state, err)
		}
		if _, err := corpus.PublishedOntologyCatalog(
			context.Background(), base,
		); err != nil {
			t.Fatalf("catalog after reopen = %v", err)
		}
	})
}

func TestPublishedSemanticChangeWithoutMorphismRemainsUnresolved(t *testing.T) {
	corpus, err := Open(filepath.Join(t.TempDir(), "corpus"))
	if err != nil {
		t.Fatal(err)
	}
	defer corpus.Close()
	schema, _ := ontology.NewOntologySchema("semantic", "Semantic", "", nil)
	property, _ := ontology.NewPropertyDefinition(
		"name", "Name", "", ontology.ValueString, nil, nil)
	basePerson, _ := ontology.NewConceptDefinition(
		"person", "Person", "", []shoal.ID{property.ID()}, nil)
	targetPerson, _ := ontology.NewConceptDefinition(
		"person", "Person", "", nil, nil)
	at := time.Date(2026, 9, 6, 0, 0, 0, 0, time.UTC)
	base, _ := ontology.NewOntologyVersion(
		schema, "1", at, []ontology.ConceptDefinition{basePerson}, nil,
		[]ontology.PropertyDefinition{property}, nil)
	target, _ := ontology.NewOntologyVersion(
		schema, "2", at.Add(time.Second),
		[]ontology.ConceptDefinition{targetPerson}, nil,
		[]ontology.PropertyDefinition{property}, nil)
	proposal, err := ontology.NewGovernedProposal(
		schema, base, target, "author", "remove ownership",
		at.Add(2*time.Second), nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := corpus.CreateOntologyProposal(ctx, proposal, base); err != nil {
		t.Fatal(err)
	}
	for index, state := range []ontology.ProposalState{
		ontology.ProposalSubmitted, ontology.ProposalApproved, ontology.ProposalPublished,
	} {
		proposal, err = corpus.TransitionOntologyProposal(
			ctx, proposal.ID(), state, "governor", "approved",
			at.Add(time.Duration(index+3)*time.Second))
		if err != nil {
			t.Fatal(err)
		}
	}

	evidence, _ := ontology.NewEvidenceRef(document.Citation{
		DocumentID: "doc", RevisionID: "rev", SectionID: "section", SpanID: "span",
		Range: document.SourceRange{
			Start: document.SourcePosition{Offset: 0, Page: 1},
			End:   document.SourcePosition{Offset: 4, Page: 1},
		},
	}, "name", nil)
	provenance, _ := ontology.NewExtractionProvenance(
		"provider", "model", "1", "prompt", "1", "extractor", "1", nil)
	value, _ := ontology.NewStringValue("Ada")
	baseIdentity, _ := ontology.NewOntologyIdentity(base)
	assertion, err := ontology.NewAssertion(
		"person-1", property.ID(), value, ontology.AssertionExplicit, 1,
		[]ontology.EvidenceRef{evidence}, provenance, nil,
		ontology.WithAssertionSubjectType(basePerson.ID()),
		ontology.WithAssertionOntology(baseIdentity))
	if err != nil {
		t.Fatal(err)
	}
	targetIdentity, _ := ontology.NewOntologyIdentity(target)
	read, err := corpus.InterpretAssertions(
		ctx, []ontology.Assertion{assertion}, targetIdentity)
	if err != nil || len(read) != 1 || read[0].Resolved() ||
		read[0].Reason() != "no unique published morphism path" {
		t.Fatalf("unsafe implicit transition read = %#v, err = %v", read, err)
	}
}

func TestGenesisPublicationsForDifferentSchemasDoNotConflict(t *testing.T) {
	corpus, err := Open(filepath.Join(t.TempDir(), "corpus"))
	if err != nil {
		t.Fatal(err)
	}
	defer corpus.Close()
	at := time.Date(2026, 9, 6, 3, 0, 0, 0, time.UTC)
	for index, key := range []string{"schema-a", "schema-b"} {
		schema, _ := ontology.NewOntologySchema(key, key, "", nil)
		version, _ := ontology.NewOntologyVersion(
			schema, "1", at.Add(time.Duration(index)*time.Second),
			nil, nil, nil, nil)
		proposal, err := ontology.NewGovernedProposal(
			schema, ontology.OntologyVersion{}, version,
			"author", "genesis", at.Add(time.Duration(index+2)*time.Second), nil)
		if err != nil {
			t.Fatal(err)
		}
		if err := corpus.CreateOntologyProposal(
			context.Background(), proposal, ontology.OntologyVersion{}); err != nil {
			t.Fatal(err)
		}
		for step, state := range []ontology.ProposalState{
			ontology.ProposalSubmitted,
			ontology.ProposalApproved,
			ontology.ProposalPublished,
		} {
			proposal, err = corpus.TransitionOntologyProposal(
				context.Background(), proposal.ID(), state,
				"governor", "approved",
				at.Add(time.Duration(index*10+step+4)*time.Second))
			if err != nil {
				t.Fatalf("publish %s genesis: %v", key, err)
			}
		}
	}
}

func TestUnrelatedSchemaForkDoesNotPoisonSelectedOntology(t *testing.T) {
	corpus, err := Open(filepath.Join(t.TempDir(), "corpus"))
	if err != nil {
		t.Fatal(err)
	}
	defer corpus.Close()
	at := time.Date(2026, 9, 6, 4, 0, 0, 0, time.UTC)

	schemaA, _ := ontology.NewOntologySchema("selected", "Selected", "", nil)
	property, _ := ontology.NewPropertyDefinition(
		"name", "Name", "", ontology.ValueString, nil, nil)
	concept, _ := ontology.NewConceptDefinition(
		"person", "Person", "", []shoal.ID{property.ID()}, nil)
	versionA, _ := ontology.NewOntologyVersion(
		schemaA, "1", at, []ontology.ConceptDefinition{concept},
		nil, []ontology.PropertyDefinition{property}, nil)
	proposalA := mustPublishedProposal(
		t, schemaA, ontology.OntologyVersion{}, versionA, at.Add(time.Second))

	schemaB, _ := ontology.NewOntologySchema("forked", "Forked", "", nil)
	baseB, _ := ontology.NewOntologyVersion(
		schemaB, "1", at.Add(10*time.Second), nil, nil, nil, nil)
	leftB, _ := ontology.NewOntologyVersion(
		schemaB, "2-left", at.Add(11*time.Second), nil, nil, nil, nil)
	rightB, _ := ontology.NewOntologyVersion(
		schemaB, "2-right", at.Add(12*time.Second), nil, nil, nil, nil)
	left := mustPublishedProposal(t, schemaB, baseB, leftB, at.Add(13*time.Second))
	right := mustPublishedProposal(t, schemaB, baseB, rightB, at.Add(17*time.Second))

	records := []struct {
		proposal ontology.GovernedProposal
		base     ontology.OntologyVersion
	}{
		{proposalA, ontology.OntologyVersion{}},
		{left, baseB},
		{right, baseB},
	}
	corpus.mu.Lock()
	for _, item := range records {
		record := mustPersistedPublishedProposal(t, item.proposal, item.base)
		copy := record
		corpus.ontologyProposals[item.proposal.ID()] = &copy
	}
	corpus.mu.Unlock()

	evidence, _ := ontology.NewEvidenceRef(document.Citation{
		DocumentID: "doc", RevisionID: "rev", SectionID: "section", SpanID: "span",
		Range: document.SourceRange{
			Start: document.SourcePosition{Offset: 0, Page: 1},
			End:   document.SourcePosition{Offset: 4, Page: 1},
		},
	}, "name", nil)
	provenance, _ := ontology.NewExtractionProvenance(
		"provider", "model", "1", "prompt", "1", "extractor", "1", nil)
	value, _ := ontology.NewStringValue("Ada")
	identity, _ := ontology.NewOntologyIdentity(versionA)
	assertion, err := ontology.NewAssertion(
		"person-1", property.ID(), value, ontology.AssertionExplicit, 1,
		[]ontology.EvidenceRef{evidence}, provenance, nil,
		ontology.WithAssertionSubjectType(concept.ID()),
		ontology.WithAssertionOntology(identity))
	if err != nil {
		t.Fatal(err)
	}
	read, err := corpus.InterpretAssertions(
		context.Background(), []ontology.Assertion{assertion}, identity)
	if err != nil || len(read) != 1 || !read[0].Resolved() {
		t.Fatalf("unrelated fork poisoned selected schema: %#v, err=%v", read, err)
	}
}

func TestDisconnectedSameSchemaForkDoesNotPoisonExactRead(t *testing.T) {
	corpus, err := Open(filepath.Join(t.TempDir(), "corpus"))
	if err != nil {
		t.Fatal(err)
	}
	defer corpus.Close()
	at := time.Date(2026, 9, 6, 4, 30, 0, 0, time.UTC)
	schema, _ := ontology.NewOntologySchema("selected", "Selected", "", nil)
	property, _ := ontology.NewPropertyDefinition(
		"name", "Name", "", ontology.ValueString, nil, nil)
	concept, _ := ontology.NewConceptDefinition(
		"person", "Person", "", []shoal.ID{property.ID()}, nil)
	selected, _ := ontology.NewOntologyVersion(
		schema, "1", at, []ontology.ConceptDefinition{concept},
		nil, []ontology.PropertyDefinition{property}, nil)
	disconnected, _ := ontology.NewOntologyVersion(
		schema, "other", at.Add(time.Second), nil, nil, nil, nil)
	left, _ := ontology.NewOntologyVersion(
		schema, "other-left", at.Add(2*time.Second), nil, nil, nil, nil)
	right, _ := ontology.NewOntologyVersion(
		schema, "other-right", at.Add(3*time.Second), nil, nil, nil, nil)
	corpus.mu.Lock()
	for _, proposal := range []ontology.GovernedProposal{
		mustPublishedProposal(t, schema, disconnected, left, at.Add(4*time.Second)),
		mustPublishedProposal(t, schema, disconnected, right, at.Add(8*time.Second)),
	} {
		record := mustPersistedPublishedProposal(t, proposal, disconnected)
		copy := record
		corpus.ontologyProposals[proposal.ID()] = &copy
	}
	corpus.mu.Unlock()
	evidence, _ := ontology.NewEvidenceRef(document.Citation{
		DocumentID: "doc", RevisionID: "rev", SectionID: "section",
		Range: document.SourceRange{},
	}, "", nil)
	provenance, _ := ontology.NewExtractionProvenance(
		"provider", "model", "1", "prompt", "1", "extractor", "1", nil)
	value, _ := ontology.NewStringValue("Ada")
	identity, _ := ontology.NewOntologyIdentity(selected)
	assertion, err := ontology.NewAssertion(
		"person-1", property.ID(), value, ontology.AssertionExplicit, 1,
		[]ontology.EvidenceRef{evidence}, provenance, nil,
		ontology.WithAssertionSubjectType(concept.ID()),
		ontology.WithAssertionOntology(identity))
	if err != nil {
		t.Fatal(err)
	}
	read, err := corpus.InterpretAssertions(
		context.Background(), []ontology.Assertion{assertion}, identity)
	if err != nil || len(read) != 1 || !read[0].Resolved() {
		t.Fatalf("disconnected same-schema fork poisoned exact read: %#v, %v", read, err)
	}
}

func TestOntologyPublicationRejectsCycleToPublishedAncestor(t *testing.T) {
	corpus, err := Open(filepath.Join(t.TempDir(), "corpus"))
	if err != nil {
		t.Fatal(err)
	}
	defer corpus.Close()
	schema, _ := ontology.NewOntologySchema("cycle", "Cycle", "", nil)
	at := time.Date(2026, 9, 6, 5, 0, 0, 0, time.UTC)
	v1, _ := ontology.NewOntologyVersion(schema, "1", at, nil, nil, nil, nil)
	v2, _ := ontology.NewOntologyVersion(
		schema, "2", at.Add(time.Second), nil, nil, nil, nil)
	first, _ := ontology.NewGovernedProposal(
		schema, v1, v2, "author", "forward", at.Add(2*time.Second), nil)
	ctx := context.Background()
	if err := corpus.CreateOntologyProposal(ctx, first, v1); err != nil {
		t.Fatal(err)
	}
	for index, state := range []ontology.ProposalState{
		ontology.ProposalSubmitted,
		ontology.ProposalApproved,
		ontology.ProposalPublished,
	} {
		first, err = corpus.TransitionOntologyProposal(
			ctx, first.ID(), state, "governor", "approved",
			at.Add(time.Duration(index+3)*time.Second))
		if err != nil {
			t.Fatal(err)
		}
	}
	cycle, _ := ontology.NewGovernedProposal(
		schema, v2, v1, "author", "cycle", at.Add(10*time.Second), nil)
	if err := corpus.CreateOntologyProposal(ctx, cycle, v2); err != nil {
		t.Fatal(err)
	}
	for index, state := range []ontology.ProposalState{
		ontology.ProposalSubmitted,
		ontology.ProposalApproved,
	} {
		cycle, err = corpus.TransitionOntologyProposal(
			ctx, cycle.ID(), state, "governor", "approved",
			at.Add(time.Duration(index+11)*time.Second))
		if err != nil {
			t.Fatal(err)
		}
	}
	if _, err := corpus.TransitionOntologyProposal(
		ctx, cycle.ID(), ontology.ProposalPublished,
		"governor", "reject cycle", at.Add(13*time.Second),
	); !shoal.IsErrorCode(err, shoal.ErrorConflict) {
		t.Fatalf("cycle publication error = %v, want conflict", err)
	}
}

func mustPublishedProposal(
	t *testing.T,
	schema ontology.OntologySchema,
	base, target ontology.OntologyVersion,
	at time.Time,
) ontology.GovernedProposal {
	t.Helper()
	proposal, err := ontology.NewGovernedProposal(
		schema, base, target, "author", "publish", at, nil)
	if err != nil {
		t.Fatal(err)
	}
	for index, state := range []ontology.ProposalState{
		ontology.ProposalSubmitted,
		ontology.ProposalApproved,
		ontology.ProposalPublished,
	} {
		proposal, err = proposal.Transition(
			state, "governor", "approved",
			at.Add(time.Duration(index+1)*time.Second))
		if err != nil {
			t.Fatal(err)
		}
	}
	return proposal
}

func mustPersistedPublishedProposal(
	t *testing.T,
	proposal ontology.GovernedProposal,
	base ontology.OntologyVersion,
) persistedOntologyProposal {
	t.Helper()
	record, err := persistOntologyProposal(proposal, base)
	if err != nil {
		t.Fatal(err)
	}
	for index, transition := range proposal.Transitions() {
		record.transitions = append(record.transitions, persistProposalTransition(
			proposal.ID(), uint32(index+1), transition))
	}
	return record
}
