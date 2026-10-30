// Licensed to the Apache Software Foundation (ASF) under one or more
// contributor license agreements. See the NOTICE file distributed with this
// work for additional information regarding copyright ownership.

package explorer

import (
	"context"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/phrocker/shoal-oss/pkg/document"
	"github.com/phrocker/shoal-oss/pkg/ontology"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

func additiveEvidenceFixture(t *testing.T, version string) (ontology.GovernedProposal, ontology.OntologyVersion) {
	t.Helper()
	at := time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC)
	schema, _ := ontology.NewOntologySchema("proposal-evidence", "Proposal Evidence", "", nil)
	person, _ := ontology.NewConceptDefinition("person", "Person", "", nil, nil)
	added, _ := ontology.NewConceptDefinition("organization", "Organization", "", nil, nil)
	base, err := ontology.NewOntologyVersion(schema, "1", at,
		[]ontology.ConceptDefinition{person}, nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	target, err := ontology.NewOntologyVersion(schema, version, at.Add(time.Second),
		[]ontology.ConceptDefinition{person, added}, nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	var evidence []ontology.EvidenceRef
	for _, doc := range []shoal.ID{"doc-a", "doc-b"} {
		ref, err := ontology.NewEvidenceRef(document.Citation{
			DocumentID: doc, RevisionID: "revision", SectionID: "section",
			Range: document.SourceRange{
				Start: document.SourcePosition{Offset: 0},
				End:   document.SourcePosition{Offset: 1},
			},
		}, "source", shoal.Metadata{"exposure": string(doc)})
		if err != nil {
			t.Fatal(err)
		}
		evidence = append(evidence, ref)
	}
	proposal, err := ontology.NewGovernedProposalWithEvidence(schema, base, target, nil,
		evidence, "author", "add organization", at.Add(2*time.Second), nil)
	if err != nil {
		t.Fatal(err)
	}
	return proposal, base
}

func TestProposalEvidencePersistenceRestartAndBoundedTransitions(t *testing.T) {
	root := t.TempDir()
	corpus, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	proposal, base := additiveEvidenceFixture(t, "2")
	for range 2 {
		if err := corpus.CreateOntologyProposalAtBase(ctx, proposal, base); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := corpus.TransitionOntologyProposalWithLimits(ctx, proposal.ID(),
		ontology.ProposalSubmitted, "governor", "review", proposal.CreatedAt().Add(time.Second),
		OntologyProjectionLimits{MaxMorphismEvidence: 1},
	); !shoal.IsErrorCode(err, shoal.ErrorUnavailable) {
		t.Fatalf("explicit proposal evidence bypassed projection bound: %v", err)
	}
	for index, state := range []ontology.ProposalState{
		ontology.ProposalSubmitted, ontology.ProposalApproved, ontology.ProposalPublished,
	} {
		updated, err := corpus.TransitionOntologyProposal(ctx, proposal.ID(), state,
			"governor", "approved", proposal.CreatedAt().Add(time.Duration(index+1)*time.Second))
		if err != nil || !reflect.DeepEqual(updated.Evidence(), proposal.Evidence()) {
			t.Fatalf("transition evidence lost: %v", err)
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
	stored, err := corpus.OntologyProposals(ctx)
	if err != nil || len(stored) != 1 || stored[0].ID() != proposal.ID() ||
		stored[0].State() != ontology.ProposalPublished ||
		!reflect.DeepEqual(stored[0].ExplicitEvidence(), proposal.ExplicitEvidence()) {
		t.Fatalf("restart proposal evidence = %#v, %v", stored, err)
	}
	evidence, found, err := corpus.OntologyProposalEvidence(ctx, proposal.ID())
	if err != nil || !found || !reflect.DeepEqual(evidence, proposal.Evidence()) {
		t.Fatalf("restart evidence lookup = %#v, %v, %v", evidence, found, err)
	}
	if err := corpus.CreateOntologyProposal(ctx, proposal, base); err != nil {
		t.Fatalf("legacy idempotent replay after publication = %v", err)
	}
}

func TestCreateOntologyProposalAtBaseRejectsPublishedSuccessor(t *testing.T) {
	corpus, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer corpus.Close()
	ctx := context.Background()
	published, base := additiveEvidenceFixture(t, "2")
	sibling, _ := additiveEvidenceFixture(t, "3")
	if err := corpus.CreateOntologyProposalAtBase(ctx, published, base); err != nil {
		t.Fatal(err)
	}
	for index, state := range []ontology.ProposalState{
		ontology.ProposalSubmitted, ontology.ProposalApproved, ontology.ProposalPublished,
	} {
		if _, err := corpus.TransitionOntologyProposal(ctx, published.ID(), state,
			"governor", "publish", published.CreatedAt().Add(time.Duration(index+1)*time.Second)); err != nil {
			t.Fatal(err)
		}
	}
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := corpus.CreateOntologyProposalAtBase(ctx, sibling, base); !shoal.IsErrorCode(err, shoal.ErrorConflict) {
				t.Errorf("stale atomic creation = %v, want conflict", err)
			}
		}()
	}
	wg.Wait()
	stored, err := corpus.OntologyProposals(ctx)
	if err != nil || len(stored) != 1 {
		t.Fatalf("stale proposal persisted: %d, %v", len(stored), err)
	}
	if err := corpus.CreateOntologyProposal(ctx, sibling, base); err != nil {
		t.Fatalf("legacy sibling draft creation changed: %v", err)
	}
	active := published.ProposedVersion()
	target, err := ontology.NewOntologyVersion(active.Schema(), "4",
		active.CreatedAt().Add(time.Hour), active.Concepts(), active.Relationships(),
		active.Properties(), nil)
	if err != nil {
		t.Fatal(err)
	}
	next, err := ontology.NewGovernedProposalWithEvidence(active.Schema(), active, target,
		nil, published.Evidence(), "author", "build on published target",
		published.CreatedAt().Add(time.Hour), nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := corpus.CreateOntologyProposalAtBase(ctx, next, active); err != nil {
		t.Fatalf("published target as active base rejected: %v", err)
	}
	if err := corpus.CreateOntologyProposalAtBase(ctx, next, base); !shoal.IsErrorCode(err, shoal.ErrorInvalidArgument) {
		t.Fatalf("mismatched active base = %v", err)
	}
}

func TestCreateOntologyProposalAtBaseSerializesWithPublication(t *testing.T) {
	corpus, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}

	defer corpus.Close()
	ctx := context.Background()
	published, base := additiveEvidenceFixture(t, "2")
	sibling, _ := additiveEvidenceFixture(t, "3")
	if err := corpus.CreateOntologyProposal(ctx, published, base); err != nil {
		t.Fatal(err)
	}
	for index, state := range []ontology.ProposalState{ontology.ProposalSubmitted, ontology.ProposalApproved} {
		if _, err := corpus.TransitionOntologyProposal(ctx, published.ID(), state,
			"governor", "approve", published.CreatedAt().Add(time.Duration(index+1)*time.Second)); err != nil {
			t.Fatal(err)
		}
	}
	start := make(chan struct{})
	created := make(chan error, 1)
	transitioned := make(chan error, 1)
	go func() {
		<-start
		created <- corpus.CreateOntologyProposalAtBase(ctx, sibling, base)
	}()
	go func() {
		<-start
		_, err := corpus.TransitionOntologyProposal(ctx, published.ID(),
			ontology.ProposalPublished, "governor", "publish", published.CreatedAt().Add(3*time.Second))
		transitioned <- err
	}()
	close(start)
	createErr, publishErr := <-created, <-transitioned
	if publishErr != nil || createErr != nil && !shoal.IsErrorCode(createErr, shoal.ErrorConflict) {
		t.Fatalf("concurrent create/publish = %v / %v", createErr, publishErr)
	}
	// Once publication returns, even simultaneous retries must reject the old base.
	if err := corpus.CreateOntologyProposalAtBase(ctx, sibling, base); !shoal.IsErrorCode(err, shoal.ErrorConflict) {
		t.Fatalf("post-publication create = %v, want conflict", err)
	}
}

func TestCreateOntologyProposalAtBaseDeduplicatesConcurrentTargets(t *testing.T) {
	corpus, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer corpus.Close()
	proposal, base := additiveEvidenceFixture(t, "2")
	other, err := ontology.NewGovernedProposalWithEvidence(base.Schema(), base,
		proposal.ProposedVersion(), nil, proposal.Evidence(), "other-author",
		"Independent rationale for the same change", proposal.CreatedAt(), nil)
	if err != nil {
		t.Fatal(err)
	}
	start := make(chan struct{})
	results := make(chan error, 2)
	for _, candidate := range []ontology.GovernedProposal{proposal, other} {
		go func() {
			<-start
			results <- corpus.CreateOntologyProposalAtBase(context.Background(), candidate, base)
		}()
	}
	close(start)
	first, second := <-results, <-results
	if !(first == nil && shoal.IsErrorCode(second, shoal.ErrorConflict) ||
		second == nil && shoal.IsErrorCode(first, shoal.ErrorConflict)) {
		t.Fatalf("concurrent equivalent candidates = %v / %v", first, second)
	}
	stored, err := corpus.OntologyProposals(context.Background())
	if err != nil || len(stored) != 1 {
		t.Fatalf("duplicate candidates persisted: %d, %v", len(stored), err)
	}
}
