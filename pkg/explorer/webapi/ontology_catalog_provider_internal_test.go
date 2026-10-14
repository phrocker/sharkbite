// Licensed to the Apache Software Foundation (ASF) under one or more
// contributor license agreements. See the NOTICE file distributed with this
// work for additional information regarding copyright ownership.

package webapi

import (
	"context"
	"testing"
	"time"

	"github.com/phrocker/shoal-oss/pkg/explorer"
	"github.com/phrocker/shoal-oss/pkg/ontology"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

func TestEmbeddedServicePrefersNarrowPublishedOntologyCatalogProvider(
	t *testing.T,
) {
	at := time.Date(2026, time.September, 6, 0, 0, 0, 0, time.UTC)
	schema, _ := ontology.NewOntologySchema("catalog", "Catalog", "", nil)
	base, _ := ontology.NewOntologyVersion(schema, "1", at, nil, nil, nil, nil)
	target, _ := ontology.NewOntologyVersion(
		schema, "2", at.Add(time.Second), nil, nil, nil, nil)
	proposal, _ := ontology.NewGovernedProposal(
		schema, base, target, "author", "publish", at.Add(2*time.Second), nil)
	var err error
	for index, state := range []ontology.ProposalState{
		ontology.ProposalSubmitted,
		ontology.ProposalApproved,
		ontology.ProposalPublished,
	} {
		proposal, err = proposal.Transition(
			state, "governor", "publish",
			at.Add(time.Duration(index+3)*time.Second))
		if err != nil {
			t.Fatal(err)
		}
	}
	catalog, err := explorer.NewPublishedOntologyCatalog(
		base, []ontology.GovernedProposal{proposal})
	if err != nil {
		t.Fatal(err)
	}
	client := &narrowOntologyCatalogClient{catalog: catalog}
	service, err := NewEmbeddedService(client)
	if err != nil {
		t.Fatal(err)
	}
	if err := service.SetOntologyVersion(base); err != nil {
		t.Fatal(err)
	}
	got, configured, err := service.OntologyCatalog(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !configured || got.ActiveIdentity() != catalog.ActiveIdentity() ||
		client.catalogCalls != 1 || client.rawCalls != 0 {
		t.Fatalf(
			"catalog = %#v, configured=%v, provider calls=%d, raw calls=%d",
			got.Identities(), configured, client.catalogCalls, client.rawCalls,
		)
	}
}

func TestEmbeddedServiceDoesNotFallbackToRawOntologyProposals(t *testing.T) {
	at := time.Date(2026, time.September, 6, 1, 0, 0, 0, time.UTC)
	schema, _ := ontology.NewOntologySchema("catalog", "Catalog", "", nil)
	base, _ := ontology.NewOntologyVersion(schema, "1", at, nil, nil, nil, nil)
	client := &rawOnlyOntologyCatalogClient{}
	service, err := NewEmbeddedService(client)
	if err != nil {
		t.Fatal(err)
	}
	if err := service.SetOntologyVersion(base); err != nil {
		t.Fatal(err)
	}
	if _, _, err := service.OntologyCatalog(
		context.Background(),
	); !shoal.IsErrorCode(err, shoal.ErrorUnavailable) {
		t.Fatalf("raw-only ontology catalog = %v", err)
	}
	if client.rawCalls != 0 {
		t.Fatal("embedded service read raw ontology proposals")
	}
}

type narrowOntologyCatalogClient struct {
	explorer.BoundedClient
	catalog      ontology.PublishedCatalog
	catalogCalls int
	rawCalls     int
}

func (c *narrowOntologyCatalogClient) PublishedOntologyCatalog(
	context.Context,
	ontology.OntologyVersion,
) (ontology.PublishedCatalog, error) {
	c.catalogCalls++
	return c.catalog, nil
}

func (c *narrowOntologyCatalogClient) OntologyProposals(
	context.Context,
) ([]ontology.GovernedProposal, error) {
	c.rawCalls++
	return nil, shoal.NewError(
		shoal.ErrorUnauthorized, "raw proposal access is denied")
}

type rawOnlyOntologyCatalogClient struct {
	explorer.BoundedClient
	rawCalls int
}

func (c *rawOnlyOntologyCatalogClient) OntologyProposals(
	context.Context,
) ([]ontology.GovernedProposal, error) {
	c.rawCalls++
	return []ontology.GovernedProposal{}, nil
}
