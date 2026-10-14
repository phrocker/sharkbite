package explorer

import (
	"context"
	"testing"
	"time"

	"github.com/phrocker/shoal-oss/pkg/ontology"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

// TestOntologyProposalTransitionsSurviveCoarseClockGranularity pins that a
// lifecycle driven faster than the wall clock ticks still advances. Callers
// read the clock once per transition, and on platforms whose granularity is
// coarser than the work between two transitions every read returns the same
// instant.
func TestOntologyProposalTransitionsSurviveCoarseClockGranularity(t *testing.T) {
	corpus, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer corpus.Close()

	base := proposalClockOntologyVersion(t, "v1")
	proposed := proposalClockOntologyVersion(t, "v2")
	frozen := time.Date(2026, 9, 4, 0, 0, 0, 0, time.UTC)

	proposal, err := ontology.NewGovernedProposal(
		base.Schema(), base, proposed, "proposer", "coarse clock", frozen, nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := corpus.CreateOntologyProposal(ctx, proposal, base); err != nil {
		t.Fatal(err)
	}

	states := []ontology.ProposalState{
		ontology.ProposalSubmitted,
		ontology.ProposalApproved,
		ontology.ProposalPublished,
	}
	var updated ontology.GovernedProposal
	for _, state := range states {
		updated, err = corpus.TransitionOntologyProposal(
			ctx, proposal.ID(), state, "approver", "advancing lifecycle", frozen)
		if err != nil {
			t.Fatalf("transition to %s with an unchanged clock reading: %v", state, err)
		}
	}

	transitions := updated.Transitions()
	if len(transitions) != len(states) {
		t.Fatalf("transition count = %d, want %d", len(transitions), len(states))
	}
	previous := updated.CreatedAt()
	for i, transition := range transitions {
		if !transition.At().After(previous) {
			t.Fatalf("transition %d at %s does not follow %s",
				i, transition.At(), previous)
		}
		previous = transition.At()
	}
	if err := updated.Validate(); err != nil {
		t.Fatalf("proposal advanced past a coarse clock is invalid: %v", err)
	}
}

func proposalClockOntologyVersion(t *testing.T, version string) ontology.OntologyVersion {
	t.Helper()
	name, err := ontology.NewPropertyDefinition(
		"name", "Name", "Display name", ontology.ValueString, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	person, err := ontology.NewConceptDefinition(
		"person", "Person", "A person", []shoal.ID{name.ID()}, nil)
	if err != nil {
		t.Fatal(err)
	}
	schema, err := ontology.NewOntologySchema(
		"workspace", "Workspace", "Workspace ontology", nil)
	if err != nil {
		t.Fatal(err)
	}
	built, err := ontology.NewOntologyVersion(
		schema, version, time.Date(2026, 9, 4, 0, 0, 0, 0, time.UTC),
		[]ontology.ConceptDefinition{person}, nil,
		[]ontology.PropertyDefinition{name}, nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	return built
}
