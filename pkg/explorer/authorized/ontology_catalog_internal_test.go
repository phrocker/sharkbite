// Licensed to the Apache Software Foundation (ASF) under one or more
// contributor license agreements. See the NOTICE file distributed with this
// work for additional information regarding copyright ownership.

package authorized

import (
	"context"
	"testing"
	"time"

	"github.com/phrocker/shoal-oss/pkg/explorer"
	"github.com/phrocker/shoal-oss/pkg/explorer/auth"
	"github.com/phrocker/shoal-oss/pkg/ontology"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

func TestPublishedOntologyCatalogUsesSettingsAuthorityWithoutProposalRead(
	t *testing.T,
) {
	ctx := context.Background()
	at := time.Date(2026, time.September, 6, 0, 0, 0, 0, time.UTC)
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
	publishOntologyCatalogProposal(t, corpus, proposal, base, at)

	for _, operation := range []auth.Operation{
		auth.OperationWorkspaceSettingsRead,
		auth.OperationWorkspaceSettingsWrite,
	} {
		t.Run(string(operation), func(t *testing.T) {
			client := newOntologyCatalogClient(
				t, corpus, corpus,
				ontologyCatalogDecision(t, operation, []byte("source"), at),
				func(context.Context, []byte) (int64, error) { return 1, nil },
				nil, at,
			)
			catalog, err := client.PublishedOntologyCatalog(ctx, base)
			if err != nil {
				t.Fatal(err)
			}
			if catalog.ActiveIdentity() != mustOntologyCatalogIdentity(t, target) ||
				len(catalog.Versions()) != 2 {
				t.Fatalf("catalog = %#v", catalog.Identities())
			}
			if _, err := client.OntologyProposals(ctx); !shoal.IsErrorCode(
				err, shoal.ErrorUnauthorized,
			) {
				t.Fatalf("settings-only raw proposal read = %v", err)
			}
		})
	}

	targetIdentity := mustOntologyCatalogIdentity(t, target)
	for _, operation := range []auth.Operation{
		auth.OperationRetrieve,
		auth.OperationAnalyticsRead,
		auth.OperationInvoke,
	} {
		t.Run(string(operation)+"_membership", func(t *testing.T) {
			client := newOntologyCatalogClient(
				t, corpus, corpus,
				ontologyCatalogDecision(t, operation, []byte("source"), at),
				func(context.Context, []byte) (int64, error) { return 1, nil },
				nil, at,
			)
			if _, err := client.PublishedOntologyCatalog(
				ctx, base,
			); !shoal.IsErrorCode(err, shoal.ErrorUnauthorized) {
				t.Fatalf("non-settings catalog read = %v", err)
			}
			if err := client.AuthorizePublishedOntology(
				ctx, base, targetIdentity, operation,
			); err != nil {
				t.Fatalf("published identity membership = %v", err)
			}
		})
	}

	client := newOntologyCatalogClient(
		t, corpus, nil,
		ontologyCatalogDecision(
			t, auth.OperationWorkspaceSettingsRead, []byte("source"), at),
		func(context.Context, []byte) (int64, error) { return 1, nil },
		nil, at,
	)
	if _, err := client.PublishedOntologyCatalog(
		ctx, base,
	); !shoal.IsErrorCode(err, shoal.ErrorUnavailable) {
		t.Fatalf("untrusted Base catalog fallback = %v", err)
	}
}

func TestPublishedOntologyCatalogFiltersHiddenEvidenceAndOtherWorkspaces(
	t *testing.T,
) {
	ctx := context.Background()
	at := time.Date(2026, time.September, 6, 1, 0, 0, 0, time.UTC)
	corpus, err := explorer.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer corpus.Close()
	policyStore := &revokingOntologyCatalogPolicyStore{
		MemoryPolicyStore: NewMemoryPolicyStore(),
	}
	ruleA := ontologyCatalogRule(t, []byte("source-a"))
	ruleB := ontologyCatalogRule(t, []byte("source-b"))
	viewA, _ := registerOntologyEvidenceDocument(
		t, policyStore, "document-a", "revision-a", "section-a", "contains-a",
		ruleA, at)
	viewB, _ := registerOntologyEvidenceDocument(
		t, policyStore, "document-b", "revision-b", "section-b", "contains-b",
		ruleB, at)
	trusted := &ontologyEvidenceBase{
		Explorer: corpus,
		views: map[shoal.ID]explorer.DocumentView{
			viewA.Document.ID: viewA,
			viewB.Document.ID: viewB,
		},
	}
	schema, base := ontologyEvidenceVersions(t, at)
	hiddenEvidence := ontologyEvidenceRef(t, viewB, nil)
	proposal, target := ontologyEvidenceProposal(
		t, schema, base, 2, hiddenEvidence, at)
	publishOntologyCatalogProposal(t, corpus, proposal, base, at)

	sourceA := newOntologyCatalogClient(
		t, trusted, trusted,
		ontologyCatalogDecision(
			t, auth.OperationWorkspaceSettingsRead, []byte("source-a"), at),
		func(context.Context, []byte) (int64, error) { return 1, nil },
		policyStore, at,
	)
	if _, err := sourceA.PublishedOntologyCatalog(
		ctx, base,
	); !shoal.IsErrorCode(err, shoal.ErrorNotFound) {
		t.Fatalf("source-a hidden catalog = %v", err)
	}
	if _, err := sourceA.OntologyProposals(ctx); !shoal.IsErrorCode(
		err, shoal.ErrorUnauthorized,
	) {
		t.Fatalf("source-a raw proposal read = %v", err)
	}

	sourceB := newOntologyCatalogClient(
		t, trusted, trusted,
		ontologyCatalogDecision(
			t, auth.OperationWorkspaceSettingsRead, []byte("source-b"), at),
		func(context.Context, []byte) (int64, error) { return 1, nil },
		policyStore, at,
	)
	catalog, err := sourceB.PublishedOntologyCatalog(ctx, base)
	if err != nil {
		t.Fatal(err)
	}
	if len(catalog.Versions()) != 2 ||
		catalog.ActiveIdentity() != mustOntologyCatalogIdentity(t, target) {
		t.Fatalf("source-b catalog = %#v", catalog.Identities())
	}
	policyStore.revoked = true
	if _, err := sourceB.PublishedOntologyCatalog(
		ctx, base,
	); !shoal.IsErrorCode(err, shoal.ErrorNotFound) {
		t.Fatalf("revoked evidence catalog = %v", err)
	}

	otherSchema, _ := ontology.NewOntologySchema(
		"other-workspace", "Other Workspace", "", nil)
	otherBase, _ := ontology.NewOntologyVersion(
		otherSchema, "1", at, nil, nil, nil, nil)
	catalog, err = sourceB.PublishedOntologyCatalog(ctx, otherBase)
	if err != nil {
		t.Fatal(err)
	}
	if len(catalog.Versions()) != 1 ||
		catalog.ActiveIdentity() != mustOntologyCatalogIdentity(t, otherBase) ||
		catalog.Contains(mustOntologyCatalogIdentity(t, target)) {
		t.Fatalf("other workspace catalog = %#v", catalog.Identities())
	}
}

type revokingOntologyCatalogPolicyStore struct {
	*MemoryPolicyStore
	revoked bool
}

func (s *revokingOntologyCatalogPolicyStore) Revision(
	ctx context.Context,
	documentID, revisionID shoal.ID,
) (RevisionRegistration, bool, error) {
	if s.revoked {
		return RevisionRegistration{}, false, nil
	}
	return s.MemoryPolicyStore.Revision(ctx, documentID, revisionID)
}

func TestPublishedOntologyCatalogRechecksRevokedGeneration(t *testing.T) {
	ctx := context.Background()
	at := time.Date(2026, time.September, 6, 2, 0, 0, 0, time.UTC)
	corpus, err := explorer.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer corpus.Close()
	schema, _ := ontology.NewOntologySchema("generation", "Generation", "", nil)
	base, _ := ontology.NewOntologyVersion(schema, "1", at, nil, nil, nil, nil)
	target, _ := ontology.NewOntologyVersion(
		schema, "2", at.Add(time.Second), nil, nil, nil, nil)
	proposal, _ := ontology.NewGovernedProposal(
		schema, base, target, "author", "publish", at.Add(2*time.Second), nil)
	publishOntologyCatalogProposal(t, corpus, proposal, base, at)

	generation := int64(1)
	store := &generationChangingOntologyCatalogStore{
		Explorer: corpus,
		after:    func() { generation = 2 },
	}
	client := newOntologyCatalogClient(
		t, corpus, store,
		ontologyCatalogDecision(
			t, auth.OperationWorkspaceSettingsRead, []byte("source"), at),
		func(context.Context, []byte) (int64, error) { return generation, nil },
		nil, at,
	)
	if _, err := client.PublishedOntologyCatalog(
		ctx, base,
	); !shoal.IsErrorCode(err, shoal.ErrorUnavailable) {
		t.Fatalf("revoked catalog generation = %v", err)
	}
}

func TestPublishedOntologyCatalogRechecksCredentialExpiry(t *testing.T) {
	ctx := context.Background()
	at := time.Date(2026, time.September, 7, 1, 0, 0, 0, time.UTC)
	corpus, err := explorer.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer corpus.Close()
	schema, _ := ontology.NewOntologySchema("expiry", "Expiry", "", nil)
	base, _ := ontology.NewOntologyVersion(schema, "1", at, nil, nil, nil, nil)
	target, _ := ontology.NewOntologyVersion(
		schema, "2", at.Add(time.Second), nil, nil, nil, nil)
	proposal, _ := ontology.NewGovernedProposal(
		schema, base, target, "author", "publish", at.Add(2*time.Second), nil)
	publishOntologyCatalogProposal(t, corpus, proposal, base, at)
	targetIdentity := mustOntologyCatalogIdentity(t, target)

	for _, test := range []struct {
		name      string
		operation auth.Operation
		call      func(*Client) error
	}{
		{
			name:      "catalog",
			operation: auth.OperationWorkspaceSettingsRead,
			call: func(client *Client) error {
				_, err := client.PublishedOntologyCatalog(ctx, base)
				return err
			},
		},
		{
			name:      "membership",
			operation: auth.OperationRetrieve,
			call: func(client *Client) error {
				return client.AuthorizePublishedOntology(
					ctx, base, targetIdentity, auth.OperationRetrieve)
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			decision := ontologyCatalogDecision(
				t, test.operation, []byte("source"), at)
			now := at.Add(30 * time.Minute)
			store := &generationChangingOntologyCatalogStore{
				Explorer: corpus,
				after:    func() { now = decision.AuthenticationExpires() },
			}
			client := newOntologyCatalogClientWithClock(
				t, corpus, store, decision,
				func(context.Context, []byte) (int64, error) { return 1, nil },
				nil, func() time.Time { return now },
			)
			if err := test.call(client); !shoal.IsErrorCode(
				err, shoal.ErrorUnauthorized,
			) {
				t.Fatalf("expired catalog authorization = %v", err)
			}
		})
	}
}

type generationChangingOntologyCatalogStore struct {
	*explorer.Explorer
	after func()
}

func (s *generationChangingOntologyCatalogStore) OntologyProposals(
	ctx context.Context,
) ([]ontology.GovernedProposal, error) {
	proposals, err := s.Explorer.OntologyProposals(ctx)
	if err == nil && s.after != nil {
		s.after()
	}
	return proposals, err
}

func newOntologyCatalogClient(
	t *testing.T,
	base explorer.Client,
	store explorer.OntologyProposalStore,
	decision auth.Decision,
	generation func(context.Context, []byte) (int64, error),
	policyStore PolicyStore,
	at time.Time,
) *Client {
	return newOntologyCatalogClientWithClock(
		t, base, store, decision, generation, policyStore,
		func() time.Time { return at.Add(30 * time.Minute) },
	)
}

func newOntologyCatalogClientWithClock(
	t *testing.T,
	base explorer.Client,
	store explorer.OntologyProposalStore,
	decision auth.Decision,
	generation func(context.Context, []byte) (int64, error),
	policyStore PolicyStore,
	clock func() time.Time,
) *Client {
	t.Helper()
	selector, err := NewStaticPolicySelector([]byte("source"), []byte("policy"))
	if err != nil {
		t.Fatal(err)
	}
	if policyStore == nil {
		policyStore = NewMemoryPolicyStore()
	}
	client, err := NewClient(Config{
		Base:                  base,
		OntologyProposalStore: store,
		Resolver: resolverFunc(func(context.Context) (auth.Decision, error) {
			return decision, nil
		}),
		PolicySelector:   selector,
		PolicyStore:      policyStore,
		GenerationReader: generationReaderFunc(generation),
		Clock:            clock,
	})
	if err != nil {
		t.Fatal(err)
	}
	return client
}

func ontologyCatalogDecision(
	t *testing.T,
	operation auth.Operation,
	source []byte,
	at time.Time,
) auth.Decision {
	t.Helper()
	decision, err := auth.NewDecision(auth.DecisionConfig{
		Subject: "subject", Actor: "actor",
		AuthorizationDomain:   []byte("domain"),
		AllowedOperations:     []auth.Operation{operation},
		PermittedSourceIDs:    [][]byte{source},
		PermittedPolicyIDs:    [][]byte{[]byte("policy")},
		PolicyGeneration:      1,
		AuthenticationExpires: at.Add(time.Hour),
		RequestID:             "request",
		ServiceRole:           ontologyCatalogServiceRole(operation),
		ServiceCeilingIdentity: func() shoal.ID {
			if ontologyCatalogServiceRole(operation) == "" {
				return ""
			}
			return "settings-ceiling"
		}(),
	})
	if err != nil {
		t.Fatal(err)
	}
	return decision
}

func ontologyCatalogServiceRole(operation auth.Operation) auth.ServiceRole {
	switch operation {
	case auth.OperationWorkspaceSettingsRead:
		return auth.ServiceRoleWorkspaceSettingsRead
	case auth.OperationWorkspaceSettingsWrite:
		return auth.ServiceRoleWorkspaceSettingsWrite
	default:
		return ""
	}
}

func ontologyCatalogRule(t *testing.T, source []byte) AccessRule {
	t.Helper()
	policy, err := auth.NewPolicy(auth.PolicyConfig{
		AuthorizationDomain: []byte("domain"),
		SourceID:            source,
		GrantPolicyID:       []byte("policy"),
		Epoch:               1,
	})
	if err != nil {
		t.Fatal(err)
	}
	rule, err := NewAccessRule(policy)
	if err != nil {
		t.Fatal(err)
	}
	return rule
}

func publishOntologyCatalogProposal(
	t *testing.T,
	corpus *explorer.Explorer,
	proposal ontology.GovernedProposal,
	base ontology.OntologyVersion,
	at time.Time,
) {
	t.Helper()
	ctx := context.Background()
	if err := corpus.CreateOntologyProposal(ctx, proposal, base); err != nil {
		t.Fatal(err)
	}
	var err error
	for index, state := range []ontology.ProposalState{
		ontology.ProposalSubmitted,
		ontology.ProposalApproved,
		ontology.ProposalPublished,
	} {
		proposal, err = corpus.TransitionOntologyProposal(
			ctx, proposal.ID(), state, "governor", "publish",
			at.Add(time.Duration(index+3)*time.Minute))
		if err != nil {
			t.Fatal(err)
		}
	}
}

func mustOntologyCatalogIdentity(
	t *testing.T,
	version ontology.OntologyVersion,
) ontology.OntologyIdentity {
	t.Helper()
	identity, err := ontology.NewOntologyIdentity(version)
	if err != nil {
		t.Fatal(err)
	}
	return identity
}
