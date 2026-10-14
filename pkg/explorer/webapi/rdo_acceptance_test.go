// Licensed to the Apache Software Foundation (ASF) under one or more
// contributor license agreements. See the NOTICE file distributed with this
// work for additional information regarding copyright ownership.

package webapi_test

import (
	"context"
	"encoding/json"
	"net/http"
	"reflect"
	"strconv"
	"testing"
	"time"

	"github.com/phrocker/shoal-oss/pkg/explorer"
	"github.com/phrocker/shoal-oss/pkg/explorer/auth"
	"github.com/phrocker/shoal-oss/pkg/explorer/authorized"
	"github.com/phrocker/shoal-oss/pkg/explorer/webapi"
	"github.com/phrocker/shoal-oss/pkg/explorer/workspace"
	"github.com/phrocker/shoal-oss/pkg/ontology"
	"github.com/phrocker/shoal-oss/pkg/retrieval"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

// Exercise the seams together: actual extraction and governance persistence,
// live published choices, caller-owned HTTP settings, and authorized graph reads.
// The existing HTTP lens fixture observes decisions but does not interpret data.
func TestRDOCallerLensesOverStoredGraph(t *testing.T) {
	now := time.Now().UTC()
	authority, err := auth.NewAuthorityWithClock(func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	decision := func(subject string) auth.Decision {
		t.Helper()
		config := auth.DecisionConfig{
			Subject: shoal.ID(subject), Actor: shoal.ID(subject + "-actor"),
			AuthorizationDomain: authnDomain,
			AllowedOperations: append(append([]auth.Operation(nil), authnAllOperations...),
				auth.OperationWorkspaceSettingsRead, auth.OperationWorkspaceSettingsWrite),
			PermittedSourceIDs:    [][]byte{authnSourceGranted},
			PermittedPolicyIDs:    [][]byte{authnPolicyGranted},
			PolicyGeneration:      1,
			AuthenticationExpires: now.Add(time.Hour),
			RequestID:             "rdo-request",
		}
		if subject == "outsider" {
			config.PermittedSourceIDs = [][]byte{authnSourceOther}
			config.PermittedPolicyIDs = [][]byte{authnPolicyOther}
		}
		result, err := auth.NewDecision(config)
		if err != nil {
			t.Fatal(err)
		}
		return result
	}
	bound := func(subject string) context.Context {
		t.Helper()
		ctx, err := authority.Binder().Bind(context.Background(), decision(subject))
		if err != nil {
			t.Fatal(err)
		}
		return ctx
	}
	selector, err := authorized.NewStaticPolicySelector(authnSourceGranted, authnPolicyGranted)
	if err != nil {
		t.Fatal(err)
	}
	policies := authorized.NewMemoryPolicyStore()
	base := webapiSkillsOntologyVersion(t)
	baseIdentity := mustOntologyIdentity(t, base)
	corpusDir, settingsDir := t.TempDir(), t.TempDir()
	var corpus *explorer.Explorer
	var client *authorized.Client
	var store *workspace.DurableStore
	var provider *workspace.Provider
	var handler *webapi.Handler
	open := func() {
		t.Helper()
		corpus, err = explorer.Open(corpusDir)
		if err != nil {
			t.Fatal(err)
		}
		client, err = authorized.NewClient(authorized.Config{
			Base: corpus, OntologyInterpreter: corpus, OntologyProposalStore: corpus,
			Resolver: authority.Resolver(), PolicySelector: selector, PolicyStore: policies,
			GenerationReader: authnGenerationReader{}, Clock: func() time.Time { return now },
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
		choices, err := webapi.NewGovernedOntologyChoices(service)
		if err != nil {
			t.Fatal(err)
		}
		store, err = workspace.OpenDurableStore(settingsDir)
		if err != nil {
			t.Fatal(err)
		}
		provider, err = workspace.NewProvider(store, workspace.ProviderOptions{
			Resolver: authority.Resolver(), GenerationReader: authnGenerationReader{},
			Clock: func() time.Time { return now }, OntologyChoices: choices,
		})
		if err != nil {
			t.Fatal(err)
		}
		handler, err = webapi.NewAuthenticatedHandler(service,
			webapi.AuthenticatorFunc(func(request *http.Request) (auth.Decision, error) {
				return decision(request.Header.Get("X-Test-Subject")), nil
			}), authority.Binder(), "example.test")
		if err != nil {
			t.Fatal(err)
		}
		if err := handler.SetWorkspaceSettingsProvider(provider); err != nil {
			t.Fatal(err)
		}
	}
	closeStores := func() {
		t.Helper()
		if err := store.Close(); err != nil {
			t.Fatal(err)
		}
		if err := corpus.Close(); err != nil {
			t.Fatal(err)
		}
	}
	open()
	defer func() { closeStores() }()
	ctx := bound("alice")
	document, err := client.Ingest(ctx, explorer.Source{
		URI: "file:///rdo/SKILL.md", MediaType: explorer.MediaTypeMarkdown,
		Content: "# RDO Skill\n\nTools:\n- graph-cli\n\nCapabilities:\n- Graph interpretation\n",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.ExtractDocument(ctx, explorer.ExtractionRequest{
		DocumentID: document.Document.ID, RevisionID: document.Revision.ID, Version: base,
	}); err != nil {
		t.Fatal(err)
	}
	rawRequest := explorer.NeighborhoodRequest{NodeIDs: []shoal.ID{document.Document.ID}, Depth: 2}
	original, err := corpus.Neighborhood(context.Background(), rawRequest)
	if err != nil {
		t.Fatal(err)
	}
	var oldRelationship ontology.RelationshipDefinition
	for _, relationship := range base.Relationships() {
		if relationship.Key() == "provides_tool" {
			oldRelationship = relationship
		}
	}
	var pathRequest webapi.PathRequest
	for _, edge := range original.Edges {
		if edge.Type == oldRelationship.Key() {
			pathRequest = webapi.PathRequest{
				From: edge.From, To: edge.To, MaxDepth: 1, Fanout: 32,
				EdgeTypes: []string{oldRelationship.Key()},
			}
		}
	}
	if pathRequest.From == "" {
		t.Fatal("extraction did not persist the tool relationship edge")
	}
	var assertion ontology.Assertion
	for _, candidate := range original.Assertions {
		if candidate.Predicate() == oldRelationship.ID() {
			assertion = candidate
		}
	}
	if identity, known := assertion.Ontology(); assertion.ID() == "" || !known || identity != baseIdentity {
		t.Fatal("extraction did not persist the version-qualified tool assertion")
	}
	renamed, err := ontology.NewRelationshipDefinition(
		"offers_tool", "Offers tool", oldRelationship.Description(),
		oldRelationship.FromConcepts(), oldRelationship.ToConcepts(),
		oldRelationship.Properties(), oldRelationship.Directed(), oldRelationship.Metadata())
	if err != nil {
		t.Fatal(err)
	}
	relationships := base.Relationships()
	for i := range relationships {
		if relationships[i].ID() == oldRelationship.ID() {
			relationships[i] = renamed
		}
	}
	target, err := ontology.NewOntologyVersion(base.Schema(), "v2", now,
		base.Concepts(), relationships, base.Properties(), nil)
	if err != nil {
		t.Fatal(err)
	}
	targetIdentity := mustOntologyIdentity(t, target)
	morphism, err := ontology.NewOntologyMorphism(ontology.MorphismConfig{
		Kind: ontology.MorphismRename, SourceVersion: base, TargetVersion: target,
		Sources: []shoal.ID{oldRelationship.ID()}, Targets: []shoal.ID{renamed.ID()},
		Evidence: assertion.Evidence(), Rationale: "Rename without rewriting observations",
	})
	if err != nil {
		t.Fatal(err)
	}
	proposal, err := ontology.NewGovernedProposalWithMorphisms(
		base.Schema(), base, target, []ontology.OntologyMorphism{morphism},
		"alice", "Govern the read-time rename", now, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := client.CreateOntologyProposal(ctx, proposal, base); err != nil {
		t.Fatal(err)
	}
	lensPath := func(workspaceID string) string {
		return "/api/v1/workspaces/" + encodeTestID(shoal.ID(workspaceID)) + "/settings/lens"
	}
	selectLens := func(subject, workspaceID string, revision uint64, identity ontology.OntologyIdentity, status int) {
		t.Helper()
		response := settingsRequest(t, handler, http.MethodPut, lensPath(workspaceID), map[string]any{
			"expected_revision": revision,
			"mutation_id":       encodeTestID(shoal.ID(subject + "-" + strconv.FormatUint(revision, 10))),
			"selected_ontology": workspaceOntologyProjection(identity),
		}, subject, "http://example.test")
		if response.Code != status {
			t.Fatalf("select %s lens: status=%d want=%d body=%s",
				subject, response.Code, status, response.Body.String())
		}
	}
	for i, state := range []ontology.ProposalState{
		ontology.ProposalSubmitted, ontology.ProposalApproved, ontology.ProposalPublished,
	} {
		// Draft and approval alone confer neither selection nor reinterpretation.
		selectLens("alice", "alice-rdo", 0, targetIdentity, http.StatusUnauthorized)
		unpublished, err := corpus.InterpretAssertions(context.Background(),
			[]ontology.Assertion{assertion}, targetIdentity)
		if err != nil || len(unpublished) != 1 || unpublished[0].Resolved() ||
			unpublished[0].Original().ID() != assertion.ID() || unpublished[0].Reason() == "" {
			t.Fatalf("unpublished interpretation = %#v, err=%v", unpublished, err)
		}
		proposal, err = client.TransitionOntologyProposal(ctx, proposal.ID(), state,
			"alice", "Reviewed rename", now.Add(time.Duration(i+1)*time.Second))
		if err != nil {
			t.Fatal(err)
		}
	}
	choicesResponse := settingsRequest(t, handler, http.MethodGet, lensPath("alice-rdo"), nil, "alice", "")
	var choices webapi.WorkspaceOntologyChoicesResponse
	if choicesResponse.Code != http.StatusOK ||
		json.Unmarshal(choicesResponse.Body.Bytes(), &choices) != nil ||
		len(choices.Choices) != 2 || choices.Active.VersionID != encodeTestID(target.ID()) {
		t.Fatalf("published choices = %s", choicesResponse.Body.String())
	}
	selectLens("outsider", "outsider-rdo", 0, targetIdentity, http.StatusUnauthorized)
	selectLens("alice", "alice-rdo", 0, baseIdentity, http.StatusCreated)
	selectLens("bob", "bob-rdo", 0, targetIdentity, http.StatusCreated)
	read := func(subject, workspaceID string, identity ontology.OntologyIdentity, predicate shoal.ID) webapi.NeighborhoodResponse {
		t.Helper()
		response := settingsWorkspaceRequest(t, handler, http.MethodPost, "/api/v1/neighborhood",
			webapi.NeighborhoodRequest{NodeIDs: rawRequest.NodeIDs, Depth: 2, Fanout: 32, MaxNodes: 64},
			subject, encodeTestID(shoal.ID(workspaceID)))
		var result webapi.NeighborhoodResponse
		if response.Code != http.StatusOK || json.Unmarshal(response.Body.Bytes(), &result) != nil {
			t.Fatalf("%s graph status=%d body=%s", subject, response.Code, response.Body.String())
		}
		if len(result.Neighborhood.Assertions) == 0 ||
			len(result.OntologyInterpretations) != len(result.Neighborhood.Assertions) {
			t.Fatalf("%s lost assertions or interpretations", subject)
		}
		var matched *webapi.OntologyInterpretationReport
		for _, report := range result.OntologyInterpretations {
			if report.AssertionID != encodeTestID(assertion.ID()) {
				continue
			}
			matched = &report
			if report.Status != ontology.InterpretationResolved ||
				report.VersionID != encodeTestID(identity.VersionID()) ||
				report.OriginalPredicate != encodeTestID(oldRelationship.ID()) ||
				report.Predicate != encodeTestID(predicate) {
				t.Fatalf("%s interpretation = %#v", subject, report)
			}
			if identity == targetIdentity {
				if !reflect.DeepEqual(report.AppliedMorphisms, []string{encodeTestID(morphism.ID())}) {
					t.Fatalf("rename lost governed path: %#v", report)
				}
			} else if len(report.AppliedMorphisms) != 0 {
				t.Fatalf("original lens unexpectedly applied morphisms: %#v", report)
			}
		}
		if matched == nil {
			t.Fatalf("%s silently dropped the historic assertion", subject)
		}
		pathResponse := settingsWorkspaceRequest(t, handler, http.MethodPost, "/api/v1/path",
			pathRequest, subject, encodeTestID(shoal.ID(workspaceID)))
		var path webapi.PathResponse
		if pathResponse.Code != http.StatusOK ||
			json.Unmarshal(pathResponse.Body.Bytes(), &path) != nil ||
			len(path.Path.Edges) != 1 || path.Path.Edges[0].Type != oldRelationship.Key() ||
			len(path.OntologyInterpretations) != 1 ||
			!reflect.DeepEqual(path.OntologyInterpretations[0], *matched) {
			t.Fatalf("%s path did not preserve observations and selected interpretation: status=%d body=%s",
				subject, pathResponse.Code, pathResponse.Body.String())
		}
		return result
	}
	alice := read("alice", "alice-rdo", baseIdentity, oldRelationship.ID())
	bob := read("bob", "bob-rdo", targetIdentity, renamed.ID())
	if !reflect.DeepEqual(alice.Neighborhood, bob.Neighborhood) {
		t.Fatal("caller lenses mutated the shared observations rather than separate reports")
	}
	unselected := settingsRequest(t, handler, http.MethodPost, "/api/v1/path", pathRequest, "alice", "")
	var noLens webapi.PathResponse
	if unselected.Code != http.StatusOK || json.Unmarshal(unselected.Body.Bytes(), &noLens) != nil ||
		len(noLens.Assertions) != 1 || noLens.Assertions[0].ID() != assertion.ID() ||
		len(noLens.OntologyInterpretations) != 0 {
		t.Fatalf("unselected path changed no-lens behavior: %s", unselected.Body.String())
	}
	denied := settingsRequest(t, handler, http.MethodPost, "/api/v1/path", pathRequest, "outsider", "")
	if denied.Code != http.StatusNotFound {
		t.Fatalf("unauthorized graph status=%d, body=%s", denied.Code, denied.Body.String())
	}
	effective := func(workspaceID shoal.ID) auth.Decision {
		t.Helper()
		value, err := provider.ApplyDecisionForOperation(ctx, workspaceID, auth.OperationNeighborhood)
		if err != nil {
			t.Fatal(err)
		}
		return value
	}
	// A workspace pin is monotonic, not an editable preference. The same issuer
	// may use a separate owned workspace with a different eligible pin.
	before := effective("alice-rdo")
	selectLens("alice", "alice-rdo", 1, targetIdentity, http.StatusUnauthorized)
	selectLens("alice", "alice-target-rdo", 0, targetIdentity, http.StatusCreated)
	after := effective("alice-target-rdo")
	firstFingerprint, err := auth.AuthorizationFingerprint(before)
	if err != nil {
		t.Fatal(err)
	}
	secondFingerprint, err := auth.AuthorizationFingerprint(after)
	if err != nil || firstFingerprint == secondFingerprint {
		t.Fatalf("same caller changing only its lens reused authorization fingerprint: %v", err)
	}
	cacheKey := func(decision auth.Decision) auth.CacheKey {
		t.Helper()
		key, err := auth.NewCacheKey(auth.CacheKeyConfig{
			Decision: decision, AuthorizationDomain: authnDomain, PolicyCopyPin: []byte("pin"),
			SnapshotFrontier: 1, RetentionGeneration: 1,
			Request: retrieval.Request{Text: "graph", TopK: 1},
		})
		if err != nil {
			t.Fatal(err)
		}
		return key
	}
	if cacheKey(before).Digest() == cacheKey(after).Digest() {
		t.Fatal("same caller changing only its lens reused a cache partition")
	}
	read("alice", "alice-target-rdo", targetIdentity, renamed.ID())
	read("alice", "alice-rdo", baseIdentity, oldRelationship.ID())
	read("bob", "bob-rdo", targetIdentity, renamed.ID())

	// Reopen the persisted corpus and settings; retain the fixture's policy
	// catalog, whose durability is tested separately by authorized store tests.
	closeStores()
	open()
	read("alice", "alice-rdo", baseIdentity, oldRelationship.ID())
	read("alice", "alice-target-rdo", targetIdentity, renamed.ID())
	read("bob", "bob-rdo", targetIdentity, renamed.ID())
	persisted, err := corpus.Neighborhood(context.Background(), rawRequest)
	if err != nil || !reflect.DeepEqual(original, persisted) {
		t.Fatalf("lens selection or restart changed stored observations: %v", err)
	}
}
