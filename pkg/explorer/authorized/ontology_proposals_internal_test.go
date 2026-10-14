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

package authorized

import (
	"context"
	"testing"
	"time"

	"github.com/phrocker/shoal-oss/pkg/document"
	"github.com/phrocker/shoal-oss/pkg/explorer"
	"github.com/phrocker/shoal-oss/pkg/explorer/auth"
	"github.com/phrocker/shoal-oss/pkg/graph"
	"github.com/phrocker/shoal-oss/pkg/ontology"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

func TestPublishedOntologyRemainsDurableWhenFinalGenerationGuardFails(t *testing.T) {
	corpus, err := explorer.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer corpus.Close()
	schema, _ := ontology.NewOntologySchema("guarded", "Guarded", "", nil)
	at := time.Date(2026, time.September, 6, 0, 0, 0, 0, time.UTC)
	base, _ := ontology.NewOntologyVersion(schema, "1", at, nil, nil, nil, nil)
	target, _ := ontology.NewOntologyVersion(
		schema, "2", at.Add(time.Second), nil, nil, nil, nil)
	proposal, err := ontology.NewGovernedProposal(
		schema, base, target, "author", "publish", at.Add(2*time.Second), nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := corpus.CreateOntologyProposal(ctx, proposal, base); err != nil {
		t.Fatal(err)
	}
	for index, state := range []ontology.ProposalState{
		ontology.ProposalSubmitted, ontology.ProposalApproved,
	} {
		proposal, err = corpus.TransitionOntologyProposal(
			ctx, proposal.ID(), state, "governor", "approved",
			at.Add(time.Duration(index+3)*time.Second))
		if err != nil {
			t.Fatal(err)
		}
	}

	generation := int64(1)
	baseClient := &generationChangingProposalBase{
		Explorer: corpus,
		afterEvidence: func() {
			generation = 2
		},
	}
	selector, err := NewStaticPolicySelector([]byte("source"), []byte("policy"))
	if err != nil {
		t.Fatal(err)
	}
	decision, err := auth.NewDecision(auth.DecisionConfig{
		Subject: "subject", Actor: "actor",
		AuthorizationDomain: []byte("domain"),
		AllowedOperations:   []auth.Operation{auth.OperationIngest},
		PermittedSourceIDs:  [][]byte{[]byte("source")},
		PermittedPolicyIDs:  [][]byte{[]byte("policy")},
		PolicyGeneration:    1, AuthenticationExpires: at.Add(time.Hour),
		RequestID: "request",
	})
	if err != nil {
		t.Fatal(err)
	}
	client, err := NewClient(Config{
		Base:                  baseClient,
		OntologyProposalStore: baseClient,
		Resolver: resolverFunc(func(context.Context) (auth.Decision, error) {
			return decision, nil
		}),
		PolicySelector: selector, PolicyStore: NewMemoryPolicyStore(),
		GenerationReader: generationReaderFunc(
			func(context.Context, []byte) (int64, error) {
				return generation, nil
			}),
		Clock: func() time.Time { return at.Add(5 * time.Second) },
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = client.TransitionOntologyProposal(
		ctx, proposal.ID(), ontology.ProposalPublished,
		"governor", "published", at.Add(5*time.Second))
	if !shoal.IsErrorCode(err, shoal.ErrorUnavailable) {
		t.Fatalf("pre-commit generation change = %v, want unavailable", err)
	}
	stored, err := corpus.OntologyProposals(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(stored) != 1 || stored[0].State() != ontology.ProposalApproved {
		t.Fatalf("proposal changed after pre-commit guard failure = %#v", stored)
	}

	generation = 1
	baseClient.afterEvidence = nil
	baseClient.after = func(next ontology.ProposalState) {
		if next == ontology.ProposalPublished {
			generation = 2
		}
	}
	_, err = client.TransitionOntologyProposal(
		ctx, proposal.ID(), ontology.ProposalPublished,
		"governor", "published", at.Add(5*time.Second))
	if !shoal.IsErrorCode(err, shoal.ErrorUnavailable) {
		t.Fatalf("post-commit generation change = %v, want unavailable", err)
	}
	stored, err = corpus.OntologyProposals(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(stored) != 1 || stored[0].State() != ontology.ProposalPublished {
		t.Fatalf("durable proposal after guard failure = %#v", stored)
	}
}

func TestOntologyProposalEvidenceRequiresObjectAuthorization(t *testing.T) {
	ctx := context.Background()
	at := time.Date(2026, time.September, 6, 0, 0, 0, 0, time.UTC)
	corpus, err := explorer.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer corpus.Close()

	policyA, err := auth.NewPolicy(auth.PolicyConfig{
		AuthorizationDomain: []byte("domain"),
		SourceID:            []byte("source-a"),
		GrantPolicyID:       []byte("policy"),
		Epoch:               1,
	})
	if err != nil {
		t.Fatal(err)
	}
	policyB, err := auth.NewPolicy(auth.PolicyConfig{
		AuthorizationDomain: []byte("domain"),
		SourceID:            []byte("source-b"),
		GrantPolicyID:       []byte("policy"),
		Epoch:               1,
	})
	if err != nil {
		t.Fatal(err)
	}
	ruleA, err := NewAccessRule(policyA)
	if err != nil {
		t.Fatal(err)
	}
	ruleB, err := NewAccessRule(policyB)
	if err != nil {
		t.Fatal(err)
	}
	policyStore := NewMemoryPolicyStore()
	viewA, pathA := registerOntologyEvidenceDocument(
		t, policyStore, "document-a", "revision-a", "section-a", "contains-a",
		ruleA, at)
	viewB, pathB := registerOntologyEvidenceDocument(
		t, policyStore, "document-b", "revision-b", "section-b", "contains-b",
		ruleB, at)
	base := &ontologyEvidenceBase{
		Explorer: corpus,
		views: map[shoal.ID]explorer.DocumentView{
			viewA.Document.ID: viewA,
			viewB.Document.ID: viewB,
		},
	}
	decision, err := auth.NewDecision(auth.DecisionConfig{
		Subject: "subject", Actor: "actor",
		AuthorizationDomain: []byte("domain"),
		AllowedOperations: []auth.Operation{
			auth.OperationRead, auth.OperationIngest,
		},
		PermittedSourceIDs:    [][]byte{[]byte("source-a")},
		PermittedPolicyIDs:    [][]byte{[]byte("policy")},
		PolicyGeneration:      1,
		AuthenticationExpires: at.Add(time.Hour),
		RequestID:             "request",
	})
	if err != nil {
		t.Fatal(err)
	}
	selector, err := NewStaticPolicySelector([]byte("source-a"), []byte("policy"))
	if err != nil {
		t.Fatal(err)
	}
	client, err := NewClient(Config{
		Base: &untrustedOntologyEvidenceBase{
			ontologyEvidenceBase: base,
		},
		OntologyProposalStore: base,
		Resolver: resolverFunc(func(context.Context) (auth.Decision, error) {
			return decision, nil
		}),
		PolicySelector: selector, PolicyStore: policyStore,
		GenerationReader: generationReaderFunc(
			func(context.Context, []byte) (int64, error) { return 1, nil }),
		Clock: func() time.Time { return at.Add(time.Minute) },
	})
	if err != nil {
		t.Fatal(err)
	}

	schema, baseVersion := ontologyEvidenceVersions(t, at)
	allowedEvidence := ontologyEvidenceRef(
		t, viewA, nil)
	deniedCitation := ontologyEvidenceRef(
		t, viewB, nil)
	deniedPath := ontologyEvidenceRef(
		t, viewA, &pathB)
	var deniedProposals []ontology.GovernedProposal
	for index, evidence := range []ontology.EvidenceRef{
		allowedEvidence, deniedCitation, deniedPath,
	} {
		proposal, target := ontologyEvidenceProposal(
			t, schema, baseVersion, index+2, evidence, at)
		if err := corpus.CreateOntologyProposal(ctx, proposal, baseVersion); err != nil {
			t.Fatal(err)
		}
		if target.ID() == "" {
			t.Fatal("target ontology version is empty")
		}
		if index > 0 {
			deniedProposals = append(deniedProposals, proposal)
		}
	}

	visible, err := client.OntologyProposals(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(visible) != 1 ||
		visible[0].Morphisms()[0].Evidence()[0].ID() != allowedEvidence.ID() {
		t.Fatalf("visible proposals = %#v, want only source-a evidence", visible)
	}
	hiddenState, err := client.OntologyProposalMutationState(
		ctx, baseVersion, deniedProposals[0].ID())
	if err != nil || hiddenState.ProposalFound() {
		t.Fatalf("hidden proposal mutation state = %#v, %v", hiddenState, err)
	}
	published := deniedProposals[0]
	for index, state := range []ontology.ProposalState{
		ontology.ProposalSubmitted,
		ontology.ProposalApproved,
		ontology.ProposalPublished,
	} {
		published, err = corpus.TransitionOntologyProposal(
			ctx, published.ID(), state, "governor", "publish",
			at.Add(time.Duration(index+2)*time.Minute))
		if err != nil {
			t.Fatal(err)
		}
	}
	active, err := client.OntologyActiveState(ctx, baseVersion)
	if err != nil {
		t.Fatal(err)
	}
	if active.ID() != published.ProposedVersion().ID() {
		t.Fatalf("active ontology was filtered by hidden evidence: %q", active.ID())
	}
	if _, err := client.TransitionOntologyProposal(
		ctx, deniedProposals[1].ID(), ontology.ProposalSubmitted,
		"governor", "unauthorized evidence", at.Add(time.Minute),
	); !shoal.IsErrorCode(err, shoal.ErrorNotFound) {
		t.Fatalf("unauthorized evidence transition = %v, want non-disclosing not found", err)
	}

	allowed, _ := ontologyEvidenceProposal(
		t, schema, baseVersion, 5, allowedEvidence, at)
	if err := client.CreateOntologyProposal(ctx, allowed, baseVersion); err != nil {
		t.Fatalf("authorized evidence create = %v", err)
	}
	outsideRange, err := ontology.NewEvidenceRef(document.Citation{
		DocumentID: viewA.Document.ID, RevisionID: viewA.Revision.ID,
		SectionID: viewA.Root.Section.ID,
		Range: document.SourceRange{
			Start: document.SourcePosition{Offset: 0},
			End:   document.SourcePosition{Offset: 1},
		},
	}, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	forgedQuote, err := ontology.NewEvidenceRef(
		allowedEvidence.Citation(), "fabricated quote", nil)
	if err != nil {
		t.Fatal(err)
	}
	for index, evidence := range []ontology.EvidenceRef{
		deniedCitation, deniedPath, outsideRange, forgedQuote,
	} {
		rejected, _ := ontologyEvidenceProposal(
			t, schema, baseVersion, index+6, evidence, at)
		if err := client.CreateOntologyProposal(
			ctx, rejected, baseVersion,
		); !shoal.IsErrorCode(err, shoal.ErrorNotFound) {
			t.Fatalf("unauthorized evidence create %d = %v, want non-disclosing not found",
				index, err)
		}
	}
	stored, err := corpus.OntologyProposals(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(stored) != 4 {
		t.Fatalf("unauthorized evidence proposal persisted: %d proposals", len(stored))
	}
	for _, proposal := range stored {
		if proposal.ID() == published.ID() {
			if proposal.State() != ontology.ProposalPublished {
				t.Fatalf("published proposal state = %s", proposal.State())
			}
		} else if proposal.State() != ontology.ProposalDraft {
			t.Fatalf("unauthorized evidence transition changed proposal: %#v", proposal)
		}
	}

	if len(pathA.Nodes) != 2 || len(pathA.Edges) != 1 {
		t.Fatalf("authorized evidence path fixture = %#v", pathA)
	}
}

type generationChangingProposalBase struct {
	*explorer.Explorer
	after         func(ontology.ProposalState)
	afterEvidence func()
}

func (b *generationChangingProposalBase) OntologyProposalEvidence(
	ctx context.Context,
	proposalID shoal.ID,
) ([]ontology.EvidenceRef, bool, error) {
	evidence, found, err := b.Explorer.OntologyProposalEvidence(ctx, proposalID)
	if err == nil && b.afterEvidence != nil {
		b.afterEvidence()
	}
	return evidence, found, err
}

func (b *generationChangingProposalBase) TransitionOntologyProposal(
	ctx context.Context,
	proposalID shoal.ID,
	next ontology.ProposalState,
	actor, note string,
	at time.Time,
) (ontology.GovernedProposal, error) {
	proposal, err := b.Explorer.TransitionOntologyProposal(
		ctx, proposalID, next, actor, note, at)
	if err == nil && b.after != nil {
		b.after(next)
	}
	return proposal, err
}

type ontologyEvidenceBase struct {
	*explorer.Explorer
	views map[shoal.ID]explorer.DocumentView
}

type untrustedOntologyEvidenceBase struct {
	*ontologyEvidenceBase
}

func (*untrustedOntologyEvidenceBase) OntologyProposalEvidence(
	context.Context,
	shoal.ID,
) ([]ontology.EvidenceRef, bool, error) {
	return nil, true, nil
}

func (*untrustedOntologyEvidenceBase) ResolveOntologyEvidenceCitation(
	context.Context,
	document.Citation,
) (string, error) {
	return "forged untrusted quote", nil
}

func (*untrustedOntologyEvidenceBase) OntologyActiveState(
	context.Context,
	ontology.OntologyVersion,
) (ontology.OntologyVersion, error) {
	return ontology.OntologyVersion{}, nil
}

func (b *ontologyEvidenceBase) Documents(
	context.Context,
) ([]explorer.DocumentSummary, error) {
	summaries := make([]explorer.DocumentSummary, 0, len(b.views))
	for _, view := range b.views {
		summaries = append(summaries, explorer.DocumentSummary{
			Document: view.Document, Revision: view.Revision,
			SourceURI: view.SourceURI, SourceMediaType: view.SourceMediaType,
		})
	}
	return summaries, nil
}

func (b *ontologyEvidenceBase) Document(
	_ context.Context,
	documentID, revisionID shoal.ID,
) (explorer.DocumentView, error) {
	view, ok := b.views[documentID]
	if !ok || view.Revision.ID != revisionID {
		return explorer.DocumentView{}, shoal.NewError(
			shoal.ErrorNotFound, "document revision not found")
	}
	return view, nil
}

func (b *ontologyEvidenceBase) ResolveOntologyEvidenceCitation(
	_ context.Context,
	citation document.Citation,
) (string, error) {
	view, ok := b.views[citation.DocumentID]
	if !ok || view.Revision.ID != citation.RevisionID {
		return "", shoal.NewError(
			shoal.ErrorNotFound, "document revision not found")
	}
	if citation.SectionID != view.Root.Section.ID ||
		citation.Range != view.Root.Section.Range {
		return "", shoal.NewError(
			shoal.ErrorInvalidArgument, "citation is outside the section")
	}
	return "", nil
}

func registerOntologyEvidenceDocument(
	t *testing.T,
	store PolicyStore,
	documentID, revisionID, sectionID, edgeID shoal.ID,
	rule AccessRule,
	at time.Time,
) (explorer.DocumentView, graph.Path) {
	t.Helper()
	view := explorer.DocumentView{
		Document: document.Document{
			ID: documentID, RevisionID: revisionID,
			Title: "Evidence", RootSectionID: sectionID,
		},
		Revision: document.Revision{
			ID: revisionID, DocumentID: documentID, CreatedAt: at,
		},
		SourceURI: "memory://" + string(documentID),
		Root: explorer.SectionView{Section: document.Section{
			ID: sectionID, DocumentID: documentID, RevisionID: revisionID,
			Heading: "Evidence",
		}},
	}
	edge := graph.Edge{
		ID: edgeID, From: documentID, To: sectionID,
		Type: "contains", Weight: 1,
	}
	digest, err := documentViewDigest(view)
	if err != nil {
		t.Fatal(err)
	}
	registration := RevisionRegistration{
		DocumentID: documentID, RevisionID: revisionID,
		NodeIDs:        []shoal.ID{documentID, sectionID},
		IntrinsicEdges: []graph.Edge{edge},
		ContentDigest:  digest, Rule: rule, Current: true,
	}
	if err := store.PutRevision(context.Background(), registration); err != nil {
		t.Fatal(err)
	}
	canonical, err := buildCanonicalRetrievalDocument(view, registration)
	if err != nil {
		t.Fatal(err)
	}
	return view, graph.Path{
		Nodes: []graph.Node{
			canonical.nodes[documentID],
			canonical.nodes[sectionID],
		},
		Edges: []graph.Edge{edge},
	}
}

func ontologyEvidenceRef(
	t *testing.T,
	view explorer.DocumentView,
	path *graph.Path,
) ontology.EvidenceRef {
	t.Helper()
	options := []ontology.EvidenceOption{}
	if path != nil {
		options = append(options, ontology.WithEvidencePath(*path))
	}
	evidence, err := ontology.NewEvidenceRef(document.Citation{
		DocumentID: view.Document.ID,
		RevisionID: view.Revision.ID,
		SectionID:  view.Root.Section.ID,
		Range:      view.Root.Section.Range,
	}, "", nil, options...)
	if err != nil {
		t.Fatal(err)
	}
	return evidence
}

func ontologyEvidenceVersions(
	t *testing.T,
	at time.Time,
) (ontology.OntologySchema, ontology.OntologyVersion) {
	t.Helper()
	schema, err := ontology.NewOntologySchema(
		"evidence", "Evidence", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	person, err := ontology.NewConceptDefinition(
		"person", "Person", "", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	organization, err := ontology.NewConceptDefinition(
		"organization", "Organization", "", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	member, err := ontology.NewRelationshipDefinition(
		"member_of", "Member of", "",
		[]shoal.ID{person.ID()}, []shoal.ID{organization.ID()},
		nil, true, nil)
	if err != nil {
		t.Fatal(err)
	}
	base, err := ontology.NewOntologyVersion(
		schema, "1", at,
		[]ontology.ConceptDefinition{person, organization},
		[]ontology.RelationshipDefinition{member}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	return schema, base
}

func ontologyEvidenceProposal(
	t *testing.T,
	schema ontology.OntologySchema,
	base ontology.OntologyVersion,
	version int,
	evidence ontology.EvidenceRef,
	at time.Time,
) (ontology.GovernedProposal, ontology.OntologyVersion) {
	t.Helper()
	concepts := base.Concepts()
	contractor, err := ontology.NewConceptDefinition(
		"contractor", "Contractor", "", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	concepts = append(concepts, contractor)
	baseRelationship := base.Relationships()[0]
	targetRelationship, err := ontology.NewRelationshipDefinition(
		baseRelationship.Key(), baseRelationship.Name(), baseRelationship.Description(),
		baseRelationship.FromConcepts(),
		append(baseRelationship.ToConcepts(), contractor.ID()),
		baseRelationship.Properties(), baseRelationship.Directed(),
		baseRelationship.Metadata())
	if err != nil {
		t.Fatal(err)
	}
	target, err := ontology.NewOntologyVersion(
		schema, string(rune('0'+version)), at.Add(time.Duration(version)*time.Second),
		concepts, []ontology.RelationshipDefinition{targetRelationship}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	morphism, err := ontology.NewOntologyMorphism(ontology.MorphismConfig{
		Kind: ontology.MorphismWiden, SourceVersion: base, TargetVersion: target,
		Sources:   []shoal.ID{baseRelationship.ID()},
		Targets:   []shoal.ID{targetRelationship.ID()},
		Evidence:  []ontology.EvidenceRef{evidence},
		Rationale: "evidence-backed widening",
	})
	if err != nil {
		t.Fatal(err)
	}
	proposal, err := ontology.NewGovernedProposalWithMorphisms(
		schema, base, target, []ontology.OntologyMorphism{morphism},
		"author", "proposal", at.Add(time.Duration(version+10)*time.Second), nil)
	if err != nil {
		t.Fatal(err)
	}
	return proposal, target
}
