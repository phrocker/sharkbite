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
	"testing"
	"time"

	"github.com/phrocker/shoal-oss/pkg/shoal"
)

func TestPublishedCatalogExposesGovernedChoicesAndActiveTip(t *testing.T) {
	schema, _ := NewOntologySchema("catalog", "Catalog", "", nil)
	at := time.Date(2026, time.September, 6, 0, 0, 0, 0, time.UTC)
	v1, _ := NewOntologyVersion(schema, "1", at, nil, nil, nil, nil)
	v2, _ := NewOntologyVersion(schema, "2", at.Add(time.Second), nil, nil, nil, nil)
	v3, _ := NewOntologyVersion(schema, "3", at.Add(2*time.Second), nil, nil, nil, nil)
	first := publishedProposal(t, v1, v2, at.Add(3*time.Second))
	second := publishedProposal(t, v2, v3, at.Add(7*time.Second))

	catalog, err := NewPublishedCatalog(v1, []GovernedProposal{second, first})
	if err != nil {
		t.Fatal(err)
	}
	if catalog.Active().ID() != v3.ID() ||
		catalog.ActiveIdentity() != mustIdentity(t, v3) {
		t.Fatal("catalog did not resolve the published active tip")
	}
	identities := catalog.Identities()
	if len(identities) != 3 ||
		identities[0] != mustIdentity(t, v1) ||
		identities[1] != mustIdentity(t, v2) ||
		identities[2] != mustIdentity(t, v3) {
		t.Fatalf("catalog identities = %#v", identities)
	}
	if !catalog.Contains(mustIdentity(t, v2)) ||
		catalog.Contains(UnknownOntology()) {
		t.Fatal("catalog eligibility did not use exact governed identities")
	}
}

func TestPublishedCatalogRejectsForkedActiveHistory(t *testing.T) {
	schema, _ := NewOntologySchema("fork", "Fork", "", nil)
	at := time.Date(2026, time.September, 6, 1, 0, 0, 0, time.UTC)
	base, _ := NewOntologyVersion(schema, "1", at, nil, nil, nil, nil)
	left, _ := NewOntologyVersion(schema, "2-left", at.Add(time.Second), nil, nil, nil, nil)
	right, _ := NewOntologyVersion(schema, "2-right", at.Add(2*time.Second), nil, nil, nil, nil)
	_, err := NewPublishedCatalog(base, []GovernedProposal{
		publishedProposal(t, base, left, at.Add(3*time.Second)),
		publishedProposal(t, base, right, at.Add(7*time.Second)),
	})
	if !shoal.IsErrorCode(err, shoal.ErrorConflict) {
		t.Fatalf("forked catalog error = %v, want conflict", err)
	}
}

func TestPublishedCatalogIgnoresDisconnectedFork(t *testing.T) {
	schema, _ := NewOntologySchema("disconnected", "Disconnected", "", nil)
	at := time.Date(2026, time.September, 6, 2, 0, 0, 0, time.UTC)
	configured, _ := NewOntologyVersion(schema, "1", at, nil, nil, nil, nil)
	active, _ := NewOntologyVersion(
		schema, "2", at.Add(time.Second), nil, nil, nil, nil)
	disconnected, _ := NewOntologyVersion(
		schema, "other", at.Add(2*time.Second), nil, nil, nil, nil)
	left, _ := NewOntologyVersion(
		schema, "other-left", at.Add(3*time.Second), nil, nil, nil, nil)
	right, _ := NewOntologyVersion(
		schema, "other-right", at.Add(4*time.Second), nil, nil, nil, nil)
	catalog, err := NewPublishedCatalog(configured, []GovernedProposal{
		publishedProposal(t, configured, active, at.Add(5*time.Second)),
		publishedProposal(t, disconnected, left, at.Add(9*time.Second)),
		publishedProposal(t, disconnected, right, at.Add(13*time.Second)),
	})
	if err != nil {
		t.Fatal(err)
	}
	if catalog.Active().ID() != active.ID() || len(catalog.Versions()) != 2 {
		t.Fatalf("disconnected fork changed catalog = %#v", catalog.Versions())
	}
}

func publishedProposal(
	t *testing.T,
	base, target OntologyVersion,
	at time.Time,
) GovernedProposal {
	t.Helper()
	proposal, err := NewGovernedProposal(
		base.Schema(), base, target, "governor", "publish", at, nil)
	if err != nil {
		t.Fatal(err)
	}
	for index, state := range []ProposalState{
		ProposalSubmitted, ProposalApproved, ProposalPublished,
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
