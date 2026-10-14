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
 *
 * Unless required by applicable law or agreed to in writing,
 * software distributed under the License is distributed on an
 * "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY
 * KIND, either express or implied. See the License for the
 * specific language governing permissions and limitations
 * under the License.
 */

package workspace

import (
	"bytes"
	"context"
	"sync"
	"testing"
	"time"

	"github.com/phrocker/shoal-oss/accumulo"
	"github.com/phrocker/shoal-oss/pkg/explorer/auth"
	"github.com/phrocker/shoal-oss/pkg/ontology"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

var testNow = time.Date(2026, 9, 5, 18, 0, 0, 0, time.UTC)

type mutableResolver struct {
	mu       sync.RWMutex
	decision auth.Decision
}

type testGenerationReader struct {
	generation int64
}

func (r testGenerationReader) CurrentPolicyGeneration(
	ctx context.Context,
	_ []byte,
) (int64, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	return r.generation, nil
}

func testProviderOptions(resolver auth.Resolver) ProviderOptions {
	return ProviderOptions{
		Resolver:         resolver,
		GenerationReader: testGenerationReader{generation: 7},
		Clock:            func() time.Time { return testNow },
	}
}

func (r *mutableResolver) Resolve(ctx context.Context) (auth.Decision, error) {
	if err := ctx.Err(); err != nil {
		return auth.Decision{}, err
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.decision, nil
}

func (r *mutableResolver) set(decision auth.Decision) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.decision = decision
}

type staticCeilingResolver struct {
	ceiling auth.ServiceCeiling
}

func (r staticCeilingResolver) ResolveServiceCeiling(
	context.Context,
	auth.Decision,
) (auth.ServiceCeiling, error) {
	return r.ceiling, nil
}

type callerOntologyChoices struct {
	bySubject      map[shoal.ID][]OntologyChoice
	listCalls      *int
	authorizeCalls *int
}

func (c callerOntologyChoices) ListOntologyChoices(
	ctx context.Context,
	decision auth.Decision,
) ([]OntologyChoice, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if c.listCalls != nil {
		(*c.listCalls)++
	}
	return append([]OntologyChoice(nil), c.bySubject[decision.Subject()]...), nil
}

func (c callerOntologyChoices) AuthorizeOntology(
	ctx context.Context,
	decision auth.Decision,
	identity ontology.OntologyIdentity,
) error {
	if c.authorizeCalls != nil {
		(*c.authorizeCalls)++
	}
	choices, err := c.ListOntologyChoices(ctx, decision)
	if err != nil {
		return err
	}
	for _, choice := range choices {
		if choice.Identity == identity {
			return nil
		}
	}
	return authDenied()
}

func TestProviderAppliesOnlyNarrowingAndPreservesDecision(t *testing.T) {
	ctx := context.Background()
	store, err := OpenDurableStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	firstOntology, secondOntology := testOntologies(t)
	choices, err := NewStaticOntologyChoices(firstOntology, secondOntology)
	if err != nil {
		t.Fatal(err)
	}
	base := testDecision(t, decisionOptions{})
	resolver := &mutableResolver{decision: base}
	options := testProviderOptions(resolver)
	options.OntologyChoices = choices
	provider, err := NewProvider(store, options)
	if err != nil {
		t.Fatal(err)
	}
	topK, depth, outputBytes := uint32(7), uint32(2), uint64(4096)
	created, err := provider.Update(ctx, "workspace-one", UpdateRequest{
		ExpectedRevision: 0,
		MutationID:       "mutation-one",
		Narrowing: UpdateNarrowing{
			AllowedOperations: OperationSelection{
				Present: true,
				Values: []auth.Operation{
					auth.OperationRead,
					auth.OperationRetrieve,
				},
			},
			PermittedSourceIDs: IDSelection{
				Present: true, Values: [][]byte{[]byte("source-b")},
			},
			PermittedPolicyIDs: IDSelection{
				Present: true, Values: [][]byte{[]byte("policy-b")},
			},
			Budgets: Budgets{
				RetrievalTopK: &topK, GraphDepth: &depth,
				OutputBytes: &outputBytes,
			},
			OutputPolicies: []OutputPolicySpec{{
				SourceID:      []byte("source-b"),
				GrantPolicyID: []byte("policy-b"),
				Epoch:         4,
			}},
			SelectedOntology: OntologySelection{
				Present: true, Identity: secondOntology,
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if created.Revision != 1 || created.SettingsID == "" {
		t.Fatalf("created settings = %#v", created)
	}
	baseOutput := testPolicy(t, base, "source-a", "policy-a", 3)
	effective, err := provider.ApplyForOperation(
		ctx, "workspace-one", auth.OperationRead, Limits{
			RetrievalTopK: 20, GraphDepth: 4, GraphFanout: 30,
			GraphNodes: 200, OutputBytes: 1 << 20,
		}, []auth.Policy{baseOutput})
	if err != nil {
		t.Fatal(err)
	}
	got := effective.Decision()
	if got.Subject() != base.Subject() ||
		got.Actor() != base.Actor() ||
		got.ClientID() != base.ClientID() ||
		!equalIDs(got.OnBehalfOf(), base.OnBehalfOf()) ||
		!bytes.Equal(got.AuthorizationDomain(), base.AuthorizationDomain()) ||
		got.PolicyGeneration() != base.PolicyGeneration() ||
		!got.AuthenticationExpires().Equal(base.AuthenticationExpires()) ||
		got.RequestID() != base.RequestID() ||
		got.CorrelationID() != base.CorrelationID() ||
		got.AuditPurpose() != base.AuditPurpose() ||
		got.ServiceRole() != base.ServiceRole() ||
		got.ServiceCeilingIdentity() != base.ServiceCeilingIdentity() {
		t.Fatal("effective decision did not preserve trusted identity fields")
	}
	if !equalOperations(got.AllowedOperations(), []auth.Operation{
		auth.OperationRead, auth.OperationRetrieve,
	}) {
		t.Fatalf("operations = %v", got.AllowedOperations())
	}
	if !equalBytes(got.PermittedSourceIDs(), [][]byte{[]byte("source-b")}) ||
		!equalBytes(got.PermittedPolicyIDs(), [][]byte{[]byte("policy-b")}) {
		t.Fatal("effective decision did not apply explicit scopes")
	}
	if selected, ok := got.SelectedOntology(); !ok || selected != secondOntology {
		t.Fatal("effective decision did not use the governed selected ontology")
	}
	if effective.Limits().RetrievalTopK != 7 ||
		effective.Limits().GraphDepth != 2 ||
		effective.Limits().GraphFanout != 30 ||
		effective.Limits().GraphNodes != 200 ||
		effective.Limits().OutputBytes != 4096 {
		t.Fatalf("effective limits = %#v", effective.Limits())
	}
	visibility, err := effective.OutputVisibility()
	if err != nil {
		t.Fatal(err)
	}
	baseVisibility, _ := baseOutput.Encode()
	settingsVisibility, _ := created.Narrowing.OutputPolicies[0].Encode()
	for _, term := range append(
		bytes.Split(baseVisibility, []byte("&")),
		bytes.Split(settingsVisibility, []byte("&"))...,
	) {
		if !bytes.Contains(visibility, term) {
			t.Fatalf("output visibility %q dropped term %q", visibility, term)
		}
	}
	if effective.CacheDimensions()["workspace_settings_revision"] != 1 ||
		effective.CacheDimensions()["workspace_settings_identity_0"] == 0 {
		t.Fatalf("cache dimensions = %#v", effective.CacheDimensions())
	}
}

func TestEffectiveDecisionCannotReplaceIssuerSelectedOntology(t *testing.T) {
	firstOntology, secondOntology := testOntologies(t)
	base := testDecision(t, decisionOptions{ontology: firstOntology})
	choices, err := NewStaticOntologyChoices(firstOntology, secondOntology)
	if err != nil {
		t.Fatal(err)
	}
	store, err := OpenDurableStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	options := testProviderOptions(&mutableResolver{decision: base})
	options.OntologyChoices = choices
	provider, err := NewProvider(store, options)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := provider.Update(
		context.Background(), "workspace", UpdateRequest{
			MutationID: "replace",
			Narrowing: UpdateNarrowing{
				SelectedOntology: OntologySelection{
					Present: true, Identity: secondOntology,
				},
			},
		},
	); !shoal.IsErrorCode(err, shoal.ErrorUnauthorized) {
		t.Fatalf("replacement update error = %v, want unauthorized", err)
	}
	settings, err := provider.Update(
		context.Background(), "workspace", UpdateRequest{
			MutationID: "preserve",
			Narrowing: UpdateNarrowing{
				SelectedOntology: OntologySelection{
					Present: true, Identity: firstOntology,
				},
			},
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	effective, err := provider.ApplyForOperation(
		context.Background(), "workspace", auth.OperationRead,
		testLimits(), nil)
	if err != nil {
		t.Fatal(err)
	}
	selected, ok := effective.Decision().SelectedOntology()
	if !ok || selected != firstOntology {
		t.Fatalf("selected ontology = %#v, %v", selected, ok)
	}

	settings.Narrowing.SelectedOntology.Identity = secondOntology
	if _, err := DeriveEffectiveDecision(
		context.Background(), base, settings, ApplyOptions{
			Now: testNow, BaseLimits: testLimits(), OntologyChoices: choices,
		},
	); !shoal.IsErrorCode(err, shoal.ErrorUnauthorized) {
		t.Fatalf("replacement derive error = %v, want unauthorized", err)
	}
}

func TestSelectedOntologyCannotChangeAcrossSettingsRevisions(t *testing.T) {
	firstOntology, secondOntology := testOntologies(t)
	store, err := OpenDurableStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	created, err := store.CompareAndSwap(
		context.Background(), "workspace", "owner", []byte("domain"),
		0, "first", Narrowing{
			SelectedOntology: OntologySelection{
				Present: true, Identity: firstOntology,
			},
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.CompareAndSwap(
		context.Background(), "workspace", "owner", []byte("domain"),
		created.Revision, "replace", Narrowing{
			SelectedOntology: OntologySelection{
				Present: true, Identity: secondOntology,
			},
		},
	); !shoal.IsErrorCode(err, shoal.ErrorUnauthorized) {
		t.Fatalf("ontology replacement error = %v, want unauthorized", err)
	}
	current, err := store.Load(context.Background(), "workspace")
	if err != nil {
		t.Fatal(err)
	}
	if !current.Narrowing.SelectedOntology.Present ||
		current.Narrowing.SelectedOntology.Identity != firstOntology ||
		current.Revision != created.Revision {
		t.Fatalf("ontology replacement changed settings: %#v", current)
	}
}

func TestExplicitEmptyScopeDiffersFromOmission(t *testing.T) {
	store, err := OpenDurableStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	base := testDecision(t, decisionOptions{})
	provider, err := NewProvider(
		store, testProviderOptions(&mutableResolver{decision: base}))
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		id        shoal.ID
		selection IDSelection
		want      [][]byte
	}{
		{id: "omitted", want: base.PermittedSourceIDs()},
		{
			id:        "empty",
			selection: IDSelection{Present: true, Values: [][]byte{}},
			want:      [][]byte{},
		},
	} {
		_, err := provider.Update(context.Background(), test.id, UpdateRequest{
			MutationID: "mutation-" + test.id,
			Narrowing: UpdateNarrowing{
				PermittedSourceIDs: test.selection,
			},
		})
		if err != nil {
			t.Fatal(err)
		}
		effective, err := provider.ApplyForOperation(
			context.Background(), test.id, auth.OperationRead,
			testLimits(), nil)
		if err != nil {
			t.Fatal(err)
		}
		got := effective.Decision().PermittedSourceIDs()
		if !equalBytes(got, test.want) {
			t.Fatalf("%s sources = %q, want %q", test.id, got, test.want)
		}
		stored, err := provider.Get(context.Background(), test.id)
		if err != nil {
			t.Fatal(err)
		}
		if test.id == "empty" &&
			!stored.Narrowing.PermittedSourceIDs.Present {
			t.Fatal("explicit empty source scope was collapsed to omission")
		}
	}
}

func TestProviderRejectsEscalationAndWideningMutations(t *testing.T) {
	store, err := OpenDurableStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	base := testDecision(t, decisionOptions{})
	provider, err := NewProvider(
		store, testProviderOptions(&mutableResolver{decision: base}))
	if err != nil {
		t.Fatal(err)
	}
	topK := uint32(5)
	first, err := provider.Update(context.Background(), "guarded", UpdateRequest{
		MutationID: "mutation-one",
		Narrowing: UpdateNarrowing{
			AllowedOperations: OperationSelection{
				Present: true,
				Values:  []auth.Operation{auth.OperationRead},
			},
			PermittedSourceIDs: IDSelection{
				Present: true, Values: [][]byte{[]byte("source-a")},
			},
			PermittedPolicyIDs: IDSelection{
				Present: true, Values: [][]byte{[]byte("policy-a")},
			},
			Budgets: Budgets{RetrievalTopK: &topK},
			OutputPolicies: []OutputPolicySpec{{
				SourceID:      []byte("source-a"),
				GrantPolicyID: []byte("policy-a"),
				Epoch:         1,
			}},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	higher := uint32(6)
	mutations := []struct {
		name string
		next UpdateNarrowing
	}{
		{
			name: "add operation",
			next: UpdateNarrowing{
				AllowedOperations: OperationSelection{
					Present: true,
					Values: []auth.Operation{
						auth.OperationRead, auth.OperationRetrieve,
					},
				},
				PermittedSourceIDs: first.Narrowing.PermittedSourceIDs,
				PermittedPolicyIDs: first.Narrowing.PermittedPolicyIDs,
				Budgets:            first.Narrowing.Budgets,
				OutputPolicies: []OutputPolicySpec{{
					SourceID:      []byte("source-a"),
					GrantPolicyID: []byte("policy-a"), Epoch: 1,
				}},
			},
		},
		{
			name: "widen source scope",
			next: UpdateNarrowing{
				AllowedOperations: first.Narrowing.AllowedOperations,
				PermittedSourceIDs: IDSelection{
					Present: true,
					Values:  [][]byte{[]byte("source-a"), []byte("source-b")},
				},
				PermittedPolicyIDs: first.Narrowing.PermittedPolicyIDs,
				Budgets:            first.Narrowing.Budgets,
				OutputPolicies: []OutputPolicySpec{{
					SourceID:      []byte("source-a"),
					GrantPolicyID: []byte("policy-a"), Epoch: 1,
				}},
			},
		},
		{
			name: "raise budget",
			next: UpdateNarrowing{
				AllowedOperations:  first.Narrowing.AllowedOperations,
				PermittedSourceIDs: first.Narrowing.PermittedSourceIDs,
				PermittedPolicyIDs: first.Narrowing.PermittedPolicyIDs,
				Budgets:            Budgets{RetrievalTopK: &higher},
				OutputPolicies: []OutputPolicySpec{{
					SourceID:      []byte("source-a"),
					GrantPolicyID: []byte("policy-a"), Epoch: 1,
				}},
			},
		},
		{
			name: "drop output label",
			next: UpdateNarrowing{
				AllowedOperations:  first.Narrowing.AllowedOperations,
				PermittedSourceIDs: first.Narrowing.PermittedSourceIDs,
				PermittedPolicyIDs: first.Narrowing.PermittedPolicyIDs,
				Budgets:            first.Narrowing.Budgets,
			},
		},
	}
	for index, test := range mutations {
		t.Run(test.name, func(t *testing.T) {
			_, err := provider.Update(context.Background(), "guarded", UpdateRequest{
				ExpectedRevision: first.Revision,
				MutationID:       shoal.ID("mutation-widen-" + string(rune('a'+index))),
				Narrowing:        test.next,
			})
			if !shoal.IsErrorCode(err, shoal.ErrorUnauthorized) {
				t.Fatalf("widening error = %v", err)
			}
		})
	}
	_, err = provider.Update(context.Background(), "new", UpdateRequest{
		MutationID: "bad-source",
		Narrowing: UpdateNarrowing{
			PermittedSourceIDs: IDSelection{
				Present: true, Values: [][]byte{[]byte("not-granted")},
			},
		},
	})
	if !shoal.IsErrorCode(err, shoal.ErrorUnauthorized) {
		t.Fatalf("out-of-grant source error = %v", err)
	}
	tooHigh := uint32(MaxRetrievalTopK + 1)
	_, err = provider.Update(context.Background(), "new", UpdateRequest{
		MutationID: "bad-budget",
		Narrowing: UpdateNarrowing{
			Budgets: Budgets{RetrievalTopK: &tooHigh},
		},
	})
	if !shoal.IsErrorCode(err, shoal.ErrorInvalidArgument) {
		t.Fatalf("out-of-bound budget error = %v", err)
	}
}

func TestProviderRevalidatesRevokedDecisionAndOwnership(t *testing.T) {
	store, err := OpenDurableStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	resolver := &mutableResolver{decision: testDecision(t, decisionOptions{})}
	provider, err := NewProvider(store, testProviderOptions(resolver))
	if err != nil {
		t.Fatal(err)
	}
	_, err = provider.Update(context.Background(), "owned", UpdateRequest{
		MutationID: "create",
		Narrowing: UpdateNarrowing{
			PermittedSourceIDs: IDSelection{
				Present: true, Values: [][]byte{[]byte("source-b")},
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	resolver.set(testDecision(t, decisionOptions{
		sources: [][]byte{[]byte("source-a")},
	}))
	_, err = provider.ApplyForOperation(
		context.Background(), "owned", auth.OperationRead,
		testLimits(), nil)
	if !shoal.IsErrorCode(err, shoal.ErrorUnauthorized) {
		t.Fatalf("revoked source application error = %v", err)
	}
	resolver.set(testDecision(t, decisionOptions{subject: "other-owner"}))
	if _, err := provider.Get(context.Background(), "owned"); !shoal.IsErrorCode(
		err, shoal.ErrorNotFound,
	) {
		t.Fatalf("cross-owner read error = %v", err)
	}
	if _, err := provider.Update(context.Background(), "owned", UpdateRequest{
		ExpectedRevision: 1, MutationID: "cross-owner",
	}); !shoal.IsErrorCode(err, shoal.ErrorNotFound) {
		t.Fatalf("cross-owner write error = %v", err)
	}
	resolver.set(testDecision(t, decisionOptions{
		subject: "owner", domain: []byte("other-domain"),
	}))
	if _, err := provider.Get(context.Background(), "owned"); !shoal.IsErrorCode(
		err, shoal.ErrorNotFound,
	) {
		t.Fatalf("cross-domain read error = %v", err)
	}

	resolver.set(testDecision(t, decisionOptions{
		operations: []auth.Operation{auth.OperationWorkspaceSettingsRead},
	}))
	if _, err := provider.Update(context.Background(), "new", UpdateRequest{
		MutationID: "missing-write",
	}); !shoal.IsErrorCode(err, shoal.ErrorUnauthorized) {
		t.Fatalf("missing write operation error = %v", err)
	}
	resolver.set(testDecision(t, decisionOptions{
		operations: []auth.Operation{auth.OperationWorkspaceSettingsWrite},
	}))
	if _, err := provider.Get(context.Background(), "owned"); !shoal.IsErrorCode(
		err, shoal.ErrorUnauthorized,
	) {
		t.Fatalf("missing read operation error = %v", err)
	}
}

func TestProviderRequiresGovernedOntologyChoice(t *testing.T) {
	store, err := OpenDurableStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	first, second := testOntologies(t)
	choices, err := NewStaticOntologyChoices(first)
	if err != nil {
		t.Fatal(err)
	}
	options := testProviderOptions(
		&mutableResolver{decision: testDecision(t, decisionOptions{})})
	options.OntologyChoices = choices
	provider, err := NewProvider(store, options)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := provider.Update(context.Background(), "ontology", UpdateRequest{
		MutationID: "ungoverned",
		Narrowing: UpdateNarrowing{
			SelectedOntology: OntologySelection{
				Present: true, Identity: second,
			},
		},
	}); !shoal.IsErrorCode(err, shoal.ErrorUnauthorized) {
		t.Fatalf("ungoverned ontology error = %v", err)
	}
}

func TestSelectableLensPreservesSettingsAndDoesNotLeakAcrossCallers(t *testing.T) {
	store, err := OpenDurableStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	first, second := testOntologies(t)
	resolver := &mutableResolver{decision: testDecision(t, decisionOptions{})}
	listCalls, authorizeCalls := 0, 0
	callerChoices := callerOntologyChoices{
		bySubject: map[shoal.ID][]OntologyChoice{
			"owner": {
				{Identity: first, Active: true},
				{Identity: second},
			},
			"other-owner": {
				{Identity: second, Active: true},
			},
		},
		listCalls:      &listCalls,
		authorizeCalls: &authorizeCalls,
	}
	options := testProviderOptions(resolver)
	options.OntologyChoices = callerChoices
	provider, err := NewProvider(store, options)
	if err != nil {
		t.Fatal(err)
	}
	topK := uint32(4)
	created, err := provider.Update(context.Background(), "lens-workspace", UpdateRequest{
		MutationID: "lens-create",
		Narrowing: UpdateNarrowing{
			PermittedSourceIDs: IDSelection{
				Present: true, Values: [][]byte{[]byte("source-a")},
			},
			Budgets: Budgets{RetrievalTopK: &topK},
			OutputPolicies: []OutputPolicySpec{{
				SourceID:      []byte("source-a"),
				GrantPolicyID: []byte("policy-a"), Epoch: 1,
			}},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	choices, err := provider.ListOntologyChoices(
		context.Background(), "lens-workspace")
	if err != nil {
		t.Fatal(err)
	}
	if len(choices.Choices) != 2 || !choices.Choices[0].Active ||
		choices.SettingsRevision != created.Revision {
		t.Fatalf("owner choices = %#v", choices)
	}
	if listCalls != 1 || authorizeCalls != 0 {
		t.Fatalf("choice snapshot calls: list=%d authorize=%d",
			listCalls, authorizeCalls)
	}
	selected, err := provider.SelectOntology(
		context.Background(), "lens-workspace", created.Revision,
		"lens-select", second)
	if err != nil {
		t.Fatal(err)
	}
	if selected.Revision != 2 ||
		!selected.Narrowing.PermittedSourceIDs.Present ||
		!equalBytes(
			selected.Narrowing.PermittedSourceIDs.Values,
			created.Narrowing.PermittedSourceIDs.Values,
		) ||
		selected.Narrowing.Budgets.RetrievalTopK == nil ||
		*selected.Narrowing.Budgets.RetrievalTopK != topK ||
		len(selected.Narrowing.OutputPolicies) != 1 {
		t.Fatalf("lens selection did not preserve settings: %#v", selected)
	}
	effective, err := provider.ApplyForOperation(
		context.Background(), "lens-workspace", auth.OperationRead,
		testLimits(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if selectedIdentity, ok := effective.Decision().SelectedOntology(); !ok || selectedIdentity != second {
		t.Fatal("selected lens was not bound into the issuer decision")
	}
	if effective.Decision().Actor() != "actor" ||
		effective.Decision().RequestID() != "request" ||
		effective.Decision().AuditPurpose() != "workspace test" {
		t.Fatal("lens selection replaced issuer decision identity")
	}

	resolver.set(testDecision(t, decisionOptions{subject: "other-owner"}))
	if _, err := provider.ListOntologyChoices(
		context.Background(), "lens-workspace",
	); !shoal.IsErrorCode(err, shoal.ErrorNotFound) {
		t.Fatalf("cross-caller lens listing error = %v", err)
	}
	otherChoices, err := provider.ListOntologyChoices(
		context.Background(), "unowned-workspace")
	if err != nil {
		t.Fatal(err)
	}
	if len(otherChoices.Choices) != 1 ||
		otherChoices.Choices[0].Identity != second {
		t.Fatalf("other caller choices leaked owner choices: %#v", otherChoices)
	}
	resolver.set(testDecision(t, decisionOptions{}))
	callerChoices.bySubject["owner"] = []OntologyChoice{
		{Identity: first, Active: true},
	}
	if _, err := provider.ApplyForOperation(
		context.Background(), "lens-workspace", auth.OperationRead,
		testLimits(), nil,
	); !shoal.IsErrorCode(err, shoal.ErrorUnauthorized) {
		t.Fatalf("revoked selected lens apply error = %v", err)
	}
	if _, err := provider.ListOntologyChoices(
		context.Background(), "lens-workspace",
	); !shoal.IsErrorCode(err, shoal.ErrorUnauthorized) {
		t.Fatalf("revoked selected lens listing error = %v", err)
	}
}

func TestUpdateMayAddButNotRemoveOutputPolicies(t *testing.T) {
	store, err := OpenDurableStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	provider, err := NewProvider(store, testProviderOptions(
		&mutableResolver{decision: testDecision(t, decisionOptions{})}))
	if err != nil {
		t.Fatal(err)
	}
	firstPolicy := OutputPolicySpec{
		SourceID:      []byte("source-a"),
		GrantPolicyID: []byte("policy-a"), Epoch: 1,
	}
	secondPolicy := OutputPolicySpec{
		SourceID:      []byte("source-b"),
		GrantPolicyID: []byte("policy-b"), Epoch: 1,
	}
	first, err := provider.Update(context.Background(), "labels", UpdateRequest{
		MutationID: "labels-one",
		Narrowing: UpdateNarrowing{
			OutputPolicies: []OutputPolicySpec{firstPolicy},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	second, err := provider.Update(context.Background(), "labels", UpdateRequest{
		ExpectedRevision: first.Revision, MutationID: "labels-two",
		Narrowing: UpdateNarrowing{
			OutputPolicies: []OutputPolicySpec{firstPolicy, secondPolicy},
		},
	})
	if err != nil {
		t.Fatalf("add output policy: %v", err)
	}
	if len(second.Narrowing.OutputPolicies) != 2 {
		t.Fatalf("output policies = %d, want 2", len(second.Narrowing.OutputPolicies))
	}
	if _, err := provider.Update(context.Background(), "labels", UpdateRequest{
		ExpectedRevision: second.Revision, MutationID: "labels-three",
		Narrowing: UpdateNarrowing{
			OutputPolicies: []OutputPolicySpec{secondPolicy},
		},
	}); !shoal.IsErrorCode(err, shoal.ErrorUnauthorized) {
		t.Fatalf("drop output policy error = %v", err)
	}
}

func TestDurableStoreCASReplayRaceAndRestart(t *testing.T) {
	dir := t.TempDir()
	store, err := OpenDurableStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	owner := shoal.ID("owner")
	initial, err := store.CompareAndSwap(
		context.Background(), "workspace", owner, []byte("domain"),
		0, "mutation-create", Narrowing{})
	if err != nil {
		t.Fatal(err)
	}
	replay, err := store.CompareAndSwap(
		context.Background(), "workspace", owner, []byte("domain"),
		0, "mutation-create", Narrowing{})
	if err != nil || replay.Revision != initial.Revision {
		t.Fatalf("idempotent replay = %#v, %v", replay, err)
	}
	changed := uint32(3)
	if _, err := store.CompareAndSwap(
		context.Background(), "workspace", owner, []byte("domain"),
		0, "mutation-create",
		Narrowing{Budgets: Budgets{GraphDepth: &changed}},
	); !shoal.IsErrorCode(err, shoal.ErrorConflict) {
		t.Fatalf("mutation ID content reuse error = %v", err)
	}

	start := make(chan struct{})
	results := make(chan error, 2)
	for _, mutationID := range []shoal.ID{"race-a", "race-b"} {
		go func(id shoal.ID) {
			<-start
			_, err := store.CompareAndSwap(
				context.Background(), "workspace", owner, []byte("domain"),
				1, id, Narrowing{})
			results <- err
		}(mutationID)
	}
	close(start)
	successes, conflicts := 0, 0
	for range 2 {
		err := <-results
		switch {
		case err == nil:
			successes++
		case shoal.IsErrorCode(err, shoal.ErrorConflict):
			conflicts++
		default:
			t.Fatalf("race error = %v", err)
		}
	}
	if successes != 1 || conflicts != 1 {
		t.Fatalf("race results successes=%d conflicts=%d", successes, conflicts)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := OpenDurableStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	loaded, err := reopened.Load(context.Background(), "workspace")
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Revision != 2 || loaded.Owner != owner ||
		loaded.SettingsID != initial.SettingsID {
		t.Fatalf("restarted settings = %#v", loaded)
	}
}

func TestServiceCeilingConstrainsOutputPolicies(t *testing.T) {
	decision := testDecision(t, decisionOptions{
		serviceRole: auth.ServiceRoleWorkspaceSettingsWrite,
		ceilingID:   "settings-writer-ceiling",
		operations: []auth.Operation{
			auth.OperationWorkspaceSettingsWrite,
		},
	})
	allowedPolicy := testPolicy(t, decision, "source-a", "policy-a", 1)
	labels, _ := allowedPolicy.Encode()
	authorizations := accumulo.NewAuthorizations(
		bytes.Split(labels, []byte("&"))...)
	ceiling, err := auth.NewServiceCeiling(auth.ServiceCeilingConfig{
		Identity:       "settings-writer-ceiling",
		Role:           auth.ServiceRoleWorkspaceSettingsWrite,
		Authorizations: authorizations,
	})
	if err != nil {
		t.Fatal(err)
	}
	store, err := OpenDurableStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	options := testProviderOptions(&mutableResolver{decision: decision})
	options.CeilingResolver = staticCeilingResolver{ceiling: ceiling}
	provider, err := NewProvider(store, options)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := provider.Update(context.Background(), "service-owned", UpdateRequest{
		MutationID: "allowed",
		Narrowing: UpdateNarrowing{
			OutputPolicies: []OutputPolicySpec{{
				SourceID:      []byte("source-a"),
				GrantPolicyID: []byte("policy-a"), Epoch: 1,
			}},
		},
	}); err != nil {
		t.Fatalf("within-ceiling policy: %v", err)
	}
	if _, err := provider.Update(context.Background(), "outside", UpdateRequest{
		MutationID: "outside",
		Narrowing: UpdateNarrowing{
			OutputPolicies: []OutputPolicySpec{{
				SourceID:      []byte("source-b"),
				GrantPolicyID: []byte("policy-b"), Epoch: 1,
			}},
		},
	}); !shoal.IsErrorCode(err, shoal.ErrorUnauthorized) {
		t.Fatalf("outside-ceiling policy error = %v", err)
	}
}

func TestSettingsRevisionAndOntologyPartitionCaches(t *testing.T) {
	firstOntology, secondOntology := testOntologies(t)
	base := testDecision(t, decisionOptions{})
	choices, _ := NewStaticOntologyChoices(firstOntology, secondOntology)
	first := Settings{
		WorkspaceID: "workspace", Owner: base.Subject(),
		AuthorizationDomain: base.AuthorizationDomain(),
		SettingsID: settingsIdentity(
			"workspace", base.Subject(), base.AuthorizationDomain()),
		Revision: 1, LastMutationID: "one",
		Narrowing: Narrowing{
			SelectedOntology: OntologySelection{
				Present: true, Identity: firstOntology,
			},
		},
	}
	second := first
	second.Revision = 2
	second.LastMutationID = "two"
	second.Narrowing.SelectedOntology.Identity = secondOntology
	firstEffective, err := DeriveEffectiveDecision(
		context.Background(), base, first,
		ApplyOptions{
			Now: testNow, BaseLimits: testLimits(),
			OntologyChoices: choices,
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	secondEffective, err := DeriveEffectiveDecision(
		context.Background(), base, second,
		ApplyOptions{
			Now: testNow, BaseLimits: testLimits(),
			OntologyChoices: choices,
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if firstEffective.CacheDimensions()["workspace_settings_revision"] ==
		secondEffective.CacheDimensions()["workspace_settings_revision"] {
		t.Fatal("settings revisions shared a cache partition")
	}
	firstFingerprint, _ := auth.AuthorizationFingerprint(firstEffective.Decision())
	secondFingerprint, _ := auth.AuthorizationFingerprint(secondEffective.Decision())
	if firstFingerprint == secondFingerprint {
		t.Fatal("different selected ontologies shared an authorization fingerprint")
	}
}

type decisionOptions struct {
	subject     shoal.ID
	domain      []byte
	sources     [][]byte
	operations  []auth.Operation
	ontology    ontology.OntologyIdentity
	serviceRole auth.ServiceRole
	ceilingID   shoal.ID
}

func testDecision(t *testing.T, options decisionOptions) auth.Decision {
	t.Helper()
	if options.subject == "" {
		options.subject = "owner"
	}
	if options.domain == nil {
		options.domain = []byte("domain")
	}
	if options.sources == nil {
		options.sources = [][]byte{[]byte("source-a"), []byte("source-b")}
	}
	if options.operations == nil {
		options.operations = []auth.Operation{
			auth.OperationRead,
			auth.OperationRetrieve,
			auth.OperationWorkspaceSettingsRead,
			auth.OperationWorkspaceSettingsWrite,
		}
	}
	decision, err := auth.NewDecision(auth.DecisionConfig{
		Subject:                options.subject,
		Actor:                  "actor",
		ClientID:               "client",
		OnBehalfOf:             []shoal.ID{"delegator"},
		AuthorizationDomain:    options.domain,
		AllowedOperations:      options.operations,
		PermittedSourceIDs:     options.sources,
		PermittedPolicyIDs:     [][]byte{[]byte("policy-a"), []byte("policy-b")},
		PolicyGeneration:       7,
		AuthenticationExpires:  testNow.Add(time.Hour),
		RequestID:              "request",
		CorrelationID:          "correlation",
		AuditPurpose:           "workspace test",
		ServiceRole:            options.serviceRole,
		ServiceCeilingIdentity: options.ceilingID,
		SelectedOntology:       options.ontology,
	})
	if err != nil {
		t.Fatal(err)
	}
	return decision
}

func testPolicy(
	t *testing.T,
	decision auth.Decision,
	source, policy string,
	epoch int64,
) auth.Policy {
	t.Helper()
	config := auth.PolicyConfig{
		AuthorizationDomain: decision.AuthorizationDomain(),
		SourceID:            []byte(source), GrantPolicyID: []byte(policy),
		Epoch: epoch,
	}
	var value auth.Policy
	var err error
	if decision.TrustedService() {
		value, err = auth.NewServicePolicy(config, decision)
	} else {
		value, err = auth.NewPolicy(config)
	}
	if err != nil {
		t.Fatal(err)
	}
	return value
}

func testOntologies(
	t *testing.T,
) (ontology.OntologyIdentity, ontology.OntologyIdentity) {
	t.Helper()
	schema, err := ontology.NewOntologySchema("workspace", "Workspace", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	first, err := ontology.NewOntologyVersion(
		schema, "1", testNow, nil, nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	second, err := ontology.NewOntologyVersion(
		schema, "2", testNow.Add(time.Second), nil, nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	firstIdentity, err := ontology.NewOntologyIdentity(first)
	if err != nil {
		t.Fatal(err)
	}
	secondIdentity, err := ontology.NewOntologyIdentity(second)
	if err != nil {
		t.Fatal(err)
	}
	return firstIdentity, secondIdentity
}

func testLimits() Limits {
	return Limits{
		RetrievalTopK: 20, GraphDepth: 4, GraphFanout: 50,
		GraphNodes: 250, OutputBytes: 1 << 20,
	}
}

func equalOperations(left, right []auth.Operation) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}

func equalBytes(left, right [][]byte) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if !bytes.Equal(left[index], right[index]) {
			return false
		}
	}
	return true
}

func equalIDs(left, right []shoal.ID) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}
