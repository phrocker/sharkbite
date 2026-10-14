// Licensed to the Apache Software Foundation (ASF) under one or more
// contributor license agreements. See the NOTICE file distributed with this
// work for additional information regarding copyright ownership.

package webapi_test

import (
	"context"
	"testing"
	"time"

	"github.com/phrocker/shoal-oss/pkg/explorer"
	"github.com/phrocker/shoal-oss/pkg/explorer/auth"
	"github.com/phrocker/shoal-oss/pkg/explorer/authorized"
	"github.com/phrocker/shoal-oss/pkg/explorer/webapi"
	"github.com/phrocker/shoal-oss/pkg/ontology"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

func TestEmbeddedOntologyCatalogAllowsSettingsRoleWithoutRawProposalRead(
	t *testing.T,
) {
	ctx := context.Background()
	at := time.Now().UTC()
	corpus, err := explorer.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer corpus.Close()
	schema, _ := ontology.NewOntologySchema("settings", "Settings", "", nil)
	base, _ := ontology.NewOntologyVersion(schema, "1", at, nil, nil, nil, nil)
	target, _ := ontology.NewOntologyVersion(
		schema, "2", at.Add(time.Second), nil, nil, nil, nil)
	proposal, _ := ontology.NewGovernedProposal(
		schema, base, target, "author", "publish", at.Add(2*time.Second), nil)
	if err := corpus.CreateOntologyProposal(ctx, proposal, base); err != nil {
		t.Fatal(err)
	}
	for index, state := range []ontology.ProposalState{
		ontology.ProposalSubmitted,
		ontology.ProposalApproved,
		ontology.ProposalPublished,
	} {
		proposal, err = corpus.TransitionOntologyProposal(
			ctx, proposal.ID(), state, "governor", "publish",
			at.Add(time.Duration(index+3)*time.Second))
		if err != nil {
			t.Fatal(err)
		}
	}

	for _, fixture := range []struct {
		operation auth.Operation
		role      auth.ServiceRole
	}{
		{auth.OperationWorkspaceSettingsRead, auth.ServiceRoleWorkspaceSettingsRead},
		{auth.OperationWorkspaceSettingsWrite, auth.ServiceRoleWorkspaceSettingsWrite},
	} {
		t.Run(string(fixture.operation), func(t *testing.T) {
			authority := auth.NewAuthority()
			decision, err := auth.NewDecision(auth.DecisionConfig{
				Subject: "settings-service", Actor: "settings-service",
				AuthorizationDomain:    authnDomain,
				AllowedOperations:      []auth.Operation{fixture.operation},
				PermittedSourceIDs:     [][]byte{authnSourceGranted},
				PermittedPolicyIDs:     [][]byte{authnPolicyGranted},
				PolicyGeneration:       1,
				AuthenticationExpires:  at.Add(time.Hour),
				RequestID:              "settings-request",
				ServiceRole:            fixture.role,
				ServiceCeilingIdentity: "settings-ceiling",
			})
			if err != nil {
				t.Fatal(err)
			}
			bound, err := authority.Binder().Bind(ctx, decision)
			if err != nil {
				t.Fatal(err)
			}
			selector, err := authorized.NewStaticPolicySelector(
				authnSourceGranted, authnPolicyGranted)
			if err != nil {
				t.Fatal(err)
			}
			client, err := authorized.NewClient(authorized.Config{
				Base:                  corpus,
				OntologyProposalStore: corpus,
				Resolver:              authority.Resolver(),
				PolicySelector:        selector,
				PolicyStore:           authorized.NewMemoryPolicyStore(),
				GenerationReader:      authnGenerationReader{},
				Clock:                 func() time.Time { return at.Add(30 * time.Minute) },
			})
			if err != nil {
				t.Fatal(err)
			}
			service, err := webapi.NewEmbeddedService(client)
			if err != nil {
				t.Fatal(err)
			}
			if err := service.SetOntologyVersion(base); err != nil {
				t.Fatal(err)
			}
			catalog, configured, err := service.OntologyCatalog(bound)
			if err != nil {
				t.Fatal(err)
			}
			targetIdentity, _ := ontology.NewOntologyIdentity(target)
			if !configured || catalog.ActiveIdentity() != targetIdentity {
				t.Fatalf("catalog = %#v, configured=%v", catalog.Identities(), configured)
			}
			if _, err := client.OntologyProposals(bound); !shoal.IsErrorCode(
				err, shoal.ErrorUnauthorized,
			) {
				t.Fatalf("settings role raw proposal read = %v", err)
			}
		})
	}
}
