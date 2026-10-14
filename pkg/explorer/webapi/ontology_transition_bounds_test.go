// Licensed to the Apache Software Foundation (ASF) under one or more
// contributor license agreements. See the NOTICE file distributed with this
// work for additional information regarding copyright ownership.
package webapi

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/phrocker/shoal-oss/pkg/explorer"
	"github.com/phrocker/shoal-oss/pkg/ontology"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

func TestOntologyHTTPTransitionBoundsPrecedeCommit(t *testing.T) {
	for _, fixture := range []struct {
		name  string
		limit uint32
		build func(*testing.T, uint32) (ontology.GovernedProposal, ontology.OntologyVersion)
	}{
		{"evidence", MaxEvidencePerResult, ontologyEvidenceBoundProposal},
		{"discriminator", MaxOntologyConcepts, ontologyDiscriminatorBoundProposal},
		{"concepts", MaxOntologyConcepts, ontologyConceptBoundProposal},
	} {
		for _, oversized := range []bool{false, true} {
			for _, next := range []ontology.ProposalState{
				ontology.ProposalSubmitted, ontology.ProposalApproved,
				ontology.ProposalWithdrawn, ontology.ProposalPublished,
			} {
				t.Run(fmt.Sprintf("%s/oversized=%t/%s", fixture.name, oversized, next), func(t *testing.T) {
					ctx := context.Background()
					count := fixture.limit
					if oversized {
						count++
					}
					proposal, base := fixture.build(t, count)
					directory := t.TempDir()
					corpus, err := explorer.Open(directory)
					if err != nil {
						t.Fatal(err)
					}
					defer func() {
						if corpus != nil {
							_ = corpus.Close()
						}
					}()
					if err := corpus.CreateOntologyProposal(ctx, proposal, base); err != nil {
						t.Fatal(err)
					}
					var prefix []ontology.ProposalState
					if next == ontology.ProposalApproved || next == ontology.ProposalPublished {
						prefix = append(prefix, ontology.ProposalSubmitted)
					}
					if next == ontology.ProposalPublished {
						prefix = append(prefix, ontology.ProposalApproved)
					}
					for _, state := range prefix {
						proposal, err = corpus.TransitionOntologyProposal(
							ctx, proposal.ID(), state, "author", "prepare", time.Now().UTC())
						if err != nil {
							t.Fatal(err)
						}
					}
					if _, err := proposal.Transition(
						next, "author", "valid domain transition", proposal.UpdatedAt().Add(time.Second),
					); err != nil {
						t.Fatalf("invalid fixture transition: %v", err)
					}
					service := ontologyBoundService(t, corpus, base)
					server := httptest.NewUnstartedServer(nil)
					handler, err := NewHandler(service, server.Listener.Addr().String())
					if err != nil {
						server.Close()
						t.Fatal(err)
					}
					server.Config.Handler = handler
					server.Start()
					defer server.Close()
					response, err := server.Client().Post(
						server.URL+"/api/v1/ontology/proposals/"+encodeID(proposal.ID())+"/transition",
						"application/json", strings.NewReader(fmt.Sprintf(`{"state":%q,"note":"bounded transition"}`, next)),
					)
					if err != nil {
						t.Fatal(err)
					}
					body, readErr := io.ReadAll(response.Body)
					response.Body.Close()
					if readErr != nil {
						t.Fatal(readErr)
					}
					wantState := next
					wantTransitions := len(proposal.Transitions()) + 1
					if oversized {
						wantState = proposal.State()
						wantTransitions--
						if response.StatusCode != http.StatusServiceUnavailable {
							t.Fatalf("oversized response = %d %s", response.StatusCode, body)
						}
					} else if response.StatusCode != http.StatusOK {
						t.Fatalf("exact-bound response = %d %s", response.StatusCode, body)
					}
					server.Close()
					if err := corpus.Close(); err != nil {
						t.Fatal(err)
					}
					corpus, err = explorer.Open(directory)
					if err != nil {
						t.Fatal(err)
					}
					stored, err := corpus.OntologyProposals(ctx)
					if err != nil || len(stored) != 1 ||
						stored[0].State() != wantState ||
						len(stored[0].Transitions()) != wantTransitions {
						t.Fatalf("reopened proposals = %#v, %v; want %s/%d transitions",
							stored, err, wantState, wantTransitions)
					}
					active, configured, err := ontologyBoundService(t, corpus, base).ActiveOntology(ctx)
					wantActive := base.ID()
					if next == ontology.ProposalPublished && !oversized {
						wantActive = proposal.ProposedVersion().ID()
					}
					if err != nil || !configured || active.ID() != wantActive {
						t.Fatalf("active ontology = %q/%t, %v; want %q",
							active.ID(), configured, err, wantActive)
					}
					if oversized {
						domainResult, err := corpus.TransitionOntologyProposal(
							ctx, proposal.ID(), next, "author", "domain API remains valid", time.Now().UTC())
						if err != nil || domainResult.State() != next {
							t.Fatalf("unbounded domain transition = %q, %v", domainResult.State(), err)
						}
					}
				})
			}
		}
	}
}

func ontologyConceptBoundProposal(
	t *testing.T, count uint32,
) (ontology.GovernedProposal, ontology.OntologyVersion) {
	t.Helper()
	base := ontologyBoundVersion(t, 0)
	concepts := make([]ontology.ConceptDefinition, 0, count)
	for index := uint32(0); index < count; index++ {
		concept, err := ontology.NewConceptDefinition(
			fmt.Sprintf("concept-%03d", index), "Concept", "", nil, nil)
		if err != nil {
			t.Fatal(err)
		}
		concepts = append(concepts, concept)
	}
	target, err := ontology.NewOntologyVersion(
		base.Schema(), "next", base.CreatedAt().Add(time.Second), concepts, nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	return ontologyBoundProposal(t, base, target), base
}

func TestOntologyMutationProjectionFailureIsIndeterminate(t *testing.T) {
	proposal, _ := ontologyEvidenceBoundProposal(t, MaxEvidencePerResult+1)
	service := &oversizedOntologyResponseService{proposal: proposal}
	for _, operation := range []string{"create", "transition"} {
		t.Run(operation, func(t *testing.T) {
			var err error
			if operation == "create" {
				_, err = createOntologyProposalFor(context.Background(), service, CreateOntologyProposalRequest{})
			} else {
				_, err = transitionOntologyProposalFor(
					context.Background(), service, proposal.ID(), TransitionOntologyProposalRequest{})
			}
			if !explorer.IsIndeterminateCommit(err) {
				t.Fatalf("postcommit projection error = %v", err)
			}
		})
	}
}

type oversizedOntologyResponseService struct {
	Service
	proposal ontology.GovernedProposal
}

func (s *oversizedOntologyResponseService) OntologyProposals(context.Context) ([]ontology.GovernedProposal, error) {
	return []ontology.GovernedProposal{s.proposal}, nil
}

func (s *oversizedOntologyResponseService) CreateOntologyProposal(
	context.Context, CreateOntologyProposalRequest,
) (ontology.GovernedProposal, error) {
	return s.proposal, nil
}

func (s *oversizedOntologyResponseService) TransitionOntologyProposal(
	context.Context, shoal.ID, TransitionOntologyProposalRequest,
) (ontology.GovernedProposal, error) {
	return s.proposal, nil
}
