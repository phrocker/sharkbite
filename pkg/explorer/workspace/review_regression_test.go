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
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/phrocker/shoal-oss/accumulo"
	"github.com/phrocker/shoal-oss/internal/dirlock"
	"github.com/phrocker/shoal-oss/internal/engine"
	"github.com/phrocker/shoal-oss/internal/iterrt"
	"github.com/phrocker/shoal-oss/pkg/explorer"
	"github.com/phrocker/shoal-oss/pkg/explorer/auth"
	"github.com/phrocker/shoal-oss/pkg/ontology"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

func TestReviewSettingsRejectOutputPolicyConjunctionOverflow(t *testing.T) {
	policies := make([]auth.Policy, 0, 32)
	for index := 0; index < 32; index++ {
		policy, err := auth.NewPolicy(auth.PolicyConfig{
			AuthorizationDomain: []byte("domain"),
			SourceID:            []byte(fmt.Sprintf("source-%02d", index)),
			GrantPolicyID:       []byte(fmt.Sprintf("policy-%02d", index)),
			Epoch:               1,
		})
		if err != nil {
			t.Fatal(err)
		}
		policies = append(policies, policy)
	}
	if _, err := normalizePolicies(policies); !shoal.IsErrorCode(
		err, shoal.ErrorInvalidArgument,
	) {
		t.Fatalf("output policy conjunction error = %v", err)
	}
}

type reviewBlockingStore struct {
	Store
	afterLoad func()
	afterCAS  func()
}

func (s *reviewBlockingStore) Load(
	ctx context.Context,
	id shoal.ID,
) (Settings, error) {
	value, err := s.Store.Load(ctx, id)
	if s.afterLoad != nil {
		s.afterLoad()
	}
	return value, err
}

func (s *reviewBlockingStore) CompareAndSwap(
	ctx context.Context,
	workspaceID, owner shoal.ID,
	authorizationDomain []byte,
	expectedRevision uint64,
	mutationID shoal.ID,
	narrowing Narrowing,
) (Settings, error) {
	value, err := s.Store.CompareAndSwap(
		ctx, workspaceID, owner, authorizationDomain,
		expectedRevision, mutationID, narrowing)
	if s.afterCAS != nil {
		s.afterCAS()
	}
	return value, err
}

type mutableGenerationReader struct {
	generation int64
	afterRead  func()
}

type reviewOntologyChoices struct {
	identity       ontology.OntologyIdentity
	afterAuthorize func()
	afterList      func()
}

func (c reviewOntologyChoices) ListOntologyChoices(
	context.Context,
	auth.Decision,
) ([]OntologyChoice, error) {
	if c.afterList != nil {
		c.afterList()
	}
	return []OntologyChoice{{Identity: c.identity, Active: true}}, nil
}

func (c reviewOntologyChoices) AuthorizeOntology(
	context.Context,
	auth.Decision,
	ontology.OntologyIdentity,
) error {
	if c.afterAuthorize != nil {
		c.afterAuthorize()
	}
	return nil
}

type reviewCeilingResolver struct {
	ceiling auth.ServiceCeiling
	after   func()
}

type roleCeilingResolver map[auth.ServiceRole]auth.ServiceCeiling

func (r roleCeilingResolver) ResolveServiceCeiling(
	_ context.Context,
	decision auth.Decision,
) (auth.ServiceCeiling, error) {
	ceiling, ok := r[decision.ServiceRole()]
	if !ok {
		return auth.ServiceCeiling{}, authDenied()
	}
	return ceiling, nil
}

func (r reviewCeilingResolver) ResolveServiceCeiling(
	context.Context,
	auth.Decision,
) (auth.ServiceCeiling, error) {
	if r.after != nil {
		r.after()
	}
	return r.ceiling, nil
}

func (r *mutableGenerationReader) CurrentPolicyGeneration(
	ctx context.Context,
	_ []byte,
) (int64, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	generation := r.generation
	if r.afterRead != nil {
		r.afterRead()
	}
	return generation, nil
}

func TestReviewSettingsRequiresGenerationReader(t *testing.T) {
	store, err := OpenDurableStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	_, err = NewProvider(store, ProviderOptions{
		Resolver: &mutableResolver{
			decision: testDecision(t, decisionOptions{}),
		},
		Clock: func() time.Time { return testNow },
	})
	if !shoal.IsErrorCode(err, shoal.ErrorInvalidArgument) {
		t.Fatalf("missing generation reader error = %v", err)
	}
}

func TestReviewSettingsRejectExpiryDuringStoreRead(t *testing.T) {
	for _, operation := range []string{"get", "update", "apply"} {
		t.Run(operation, func(t *testing.T) {
			durable, err := OpenDurableStore(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			defer durable.Close()
			store := &reviewBlockingStore{Store: durable}
			base := testDecision(t, decisionOptions{})
			now := testNow
			provider, err := NewProvider(store, ProviderOptions{
				Resolver:         &mutableResolver{decision: base},
				GenerationReader: testGenerationReader{generation: 7},
				Clock:            func() time.Time { return now },
			})
			if err != nil {
				t.Fatal(err)
			}
			_, err = provider.Update(context.Background(), "review-workspace", UpdateRequest{
				MutationID: "create",
			})
			if err != nil {
				t.Fatal(err)
			}
			store.afterLoad = func() { now = base.AuthenticationExpires() }
			switch operation {
			case "get":
				_, err = provider.Get(context.Background(), "review-workspace")
			case "update":
				_, err = provider.Update(
					context.Background(), "review-workspace", UpdateRequest{
						ExpectedRevision: 1, MutationID: "update",
					})
			case "apply":
				_, err = provider.ApplyForOperation(
					context.Background(), "review-workspace",
					auth.OperationRead,
					MaximumLimits(), nil)
			}
			if !shoal.IsErrorCode(err, shoal.ErrorUnauthorized) {
				t.Fatalf("%s expiry error = %v", operation, err)
			}
			stored, loadErr := durable.Load(
				context.Background(), "review-workspace")
			if loadErr != nil {
				t.Fatal(loadErr)
			}
			if stored.Revision != 1 {
				t.Fatalf("%s wrote revision %d after expiry", operation, stored.Revision)
			}
		})
	}
}

func TestReviewSettingsRejectGenerationChangeDuringStoreRead(t *testing.T) {
	for _, operation := range []string{"get", "update", "apply"} {
		t.Run(operation, func(t *testing.T) {
			durable, err := OpenDurableStore(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			defer durable.Close()
			store := &reviewBlockingStore{Store: durable}
			reader := &mutableGenerationReader{generation: 7}
			provider, err := NewProvider(store, ProviderOptions{
				Resolver: &mutableResolver{
					decision: testDecision(t, decisionOptions{}),
				},
				GenerationReader: reader,
				Clock:            func() time.Time { return testNow },
			})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := provider.Update(
				context.Background(), "generation-workspace",
				UpdateRequest{MutationID: "create"},
			); err != nil {
				t.Fatal(err)
			}
			store.afterLoad = func() { reader.generation = 8 }
			switch operation {
			case "get":
				_, err = provider.Get(
					context.Background(), "generation-workspace")
			case "update":
				_, err = provider.Update(
					context.Background(), "generation-workspace",
					UpdateRequest{
						ExpectedRevision: 1, MutationID: "revoked",
					})
			case "apply":
				_, err = provider.ApplyForOperation(
					context.Background(), "generation-workspace",
					auth.OperationRead,
					MaximumLimits(), nil)
			}
			if !shoal.IsErrorCode(err, shoal.ErrorUnavailable) {
				t.Fatalf("%s generation change error = %v", operation, err)
			}
			stored, err := durable.Load(
				context.Background(), "generation-workspace")
			if err != nil {
				t.Fatal(err)
			}
			if stored.Revision != 1 {
				t.Fatalf(
					"%s generation-revoked call wrote revision %d",
					operation, stored.Revision)
			}
		})
	}
}

func TestReviewSettingsRecheckAfterOntologyAndCeilingCalls(t *testing.T) {
	t.Run("ontology generation revoke", func(t *testing.T) {
		store, err := OpenDurableStore(t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		defer store.Close()
		identity, _ := testOntologies(t)
		reader := &mutableGenerationReader{generation: 7}
		choices := reviewOntologyChoices{
			identity: identity,
			afterAuthorize: func() {
				reader.generation = 8
			},
		}
		provider, err := NewProvider(store, ProviderOptions{
			Resolver: &mutableResolver{
				decision: testDecision(t, decisionOptions{}),
			},
			GenerationReader: reader,
			OntologyChoices:  choices,
			Clock:            func() time.Time { return testNow },
		})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := provider.SelectOntology(
			context.Background(), "ontology-revoke", 0,
			"select", identity,
		); !shoal.IsErrorCode(err, shoal.ErrorUnavailable) {
			t.Fatalf("ontology generation revoke error = %v", err)
		}
		if _, err := store.Load(
			context.Background(), "ontology-revoke",
		); !shoal.IsErrorCode(err, shoal.ErrorNotFound) {
			t.Fatalf("ontology-revoked selection persisted: %v", err)
		}
	})

	t.Run("ontology list expiry", func(t *testing.T) {
		store, err := OpenDurableStore(t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		defer store.Close()
		identity, _ := testOntologies(t)
		base := testDecision(t, decisionOptions{})
		now := testNow
		provider, err := NewProvider(store, ProviderOptions{
			Resolver:         &mutableResolver{decision: base},
			GenerationReader: testGenerationReader{generation: 7},
			OntologyChoices: reviewOntologyChoices{
				identity: identity,
				afterList: func() {
					now = base.AuthenticationExpires()
				},
			},
			Clock: func() time.Time { return now },
		})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := provider.ListOntologyChoices(
			context.Background(), "ontology-list-expiry",
		); !shoal.IsErrorCode(err, shoal.ErrorUnauthorized) {
			t.Fatalf("ontology list expiry error = %v", err)
		}
	})

	t.Run("ceiling expiry", func(t *testing.T) {
		store, err := OpenDurableStore(t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		defer store.Close()
		base := testDecision(t, decisionOptions{
			serviceRole: auth.ServiceRoleWorkspaceSettingsWrite,
			ceilingID:   "settings-writer-ceiling",
			operations: []auth.Operation{
				auth.OperationWorkspaceSettingsWrite,
			},
		})
		policy := testPolicy(t, base, "source-a", "policy-a", 1)
		visibility, err := policy.Encode()
		if err != nil {
			t.Fatal(err)
		}
		ceiling, err := auth.NewServiceCeiling(auth.ServiceCeilingConfig{
			Identity: "settings-writer-ceiling",
			Role:     auth.ServiceRoleWorkspaceSettingsWrite,
			Authorizations: accumulo.NewAuthorizations(
				bytes.Split(visibility, []byte("&"))...),
		})
		if err != nil {
			t.Fatal(err)
		}
		now := testNow
		provider, err := NewProvider(store, ProviderOptions{
			Resolver:         &mutableResolver{decision: base},
			GenerationReader: testGenerationReader{generation: 7},
			CeilingResolver: reviewCeilingResolver{
				ceiling: ceiling,
				after: func() {
					now = base.AuthenticationExpires()
				},
			},
			Clock: func() time.Time { return now },
		})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := provider.Update(
			context.Background(), "ceiling-expiry",
			UpdateRequest{
				MutationID: "ceiling",
				Narrowing: UpdateNarrowing{
					OutputPolicies: []OutputPolicySpec{{
						SourceID:      []byte("source-a"),
						GrantPolicyID: []byte("policy-a"),
						Epoch:         1,
					}},
				},
			},
		); !shoal.IsErrorCode(err, shoal.ErrorUnauthorized) {
			t.Fatalf("ceiling expiry error = %v", err)
		}
		if _, err := store.Load(
			context.Background(), "ceiling-expiry",
		); !shoal.IsErrorCode(err, shoal.ErrorNotFound) {
			t.Fatalf("ceiling-expired update persisted: %v", err)
		}
	})
}

func TestReviewServiceWriterOutputPolicyIsConsumerNeutral(t *testing.T) {
	writer := testDecision(t, decisionOptions{
		serviceRole: auth.ServiceRoleWorkspaceSettingsWrite,
		ceilingID:   "writer-ceiling",
		operations: []auth.Operation{
			auth.OperationWorkspaceSettingsWrite,
		},
	})
	reader := testDecision(t, decisionOptions{
		serviceRole: auth.ServiceRoleDataRead,
		ceilingID:   "data-reader-ceiling",
		operations: []auth.Operation{
			auth.OperationRead,
			auth.OperationRetrieve,
		},
	})
	writerCeiling := serviceCeilingForDecision(
		t, writer, "source-a", "policy-a", 1)
	readerCeiling := serviceCeilingForDecision(
		t, reader, "source-a", "policy-a", 1)
	store, err := OpenDurableStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	resolver := &mutableResolver{decision: writer}
	provider, err := NewProvider(store, ProviderOptions{
		Resolver:         resolver,
		GenerationReader: testGenerationReader{generation: 7},
		CeilingResolver: roleCeilingResolver{
			auth.ServiceRoleWorkspaceSettingsWrite: writerCeiling,
			auth.ServiceRoleDataRead:               readerCeiling,
		},
		Clock: func() time.Time { return testNow },
	})
	if err != nil {
		t.Fatal(err)
	}
	created, err := provider.Update(
		context.Background(), "service-output",
		UpdateRequest{
			MutationID: "writer",
			Narrowing: UpdateNarrowing{
				OutputPolicies: []OutputPolicySpec{{
					SourceID:      []byte("source-a"),
					GrantPolicyID: []byte("policy-a"),
					Epoch:         1,
				}},
			},
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(created.Narrowing.OutputPolicies) != 1 ||
		created.Narrowing.OutputPolicies[0].ServiceRole() != "" {
		t.Fatalf("persisted output policy role = %q",
			created.Narrowing.OutputPolicies[0].ServiceRole())
	}
	resolver.set(reader)
	effective, err := provider.ApplyForOperation(
		context.Background(), "service-output", auth.OperationRetrieve,
		MaximumLimits(), nil)
	if err != nil {
		t.Fatalf("reader apply: %v", err)
	}
	if len(effective.OutputPolicies()) != 1 ||
		effective.OutputPolicies()[0].ServiceRole() != "" {
		t.Fatalf("effective output policies = %#v", effective.OutputPolicies())
	}
	if err := effective.Decision().Authorize(
		auth.OperationRetrieve,
		auth.ResourceRequest{
			AuthorizationDomain: effective.Decision().AuthorizationDomain(),
		},
		testNow,
	); err != nil {
		t.Fatalf("effective data-read decision cannot retrieve: %v", err)
	}
}

func TestReviewNarrowServiceRolesApplyOwnedWorkspaceRestrictions(t *testing.T) {
	for _, test := range []struct {
		name      string
		role      auth.ServiceRole
		operation auth.Operation
	}{
		{"invocation", auth.ServiceRoleActionInvocation, auth.OperationInvoke},
		{"analytics", auth.ServiceRoleAnalytics, auth.OperationAnalyticsRead},
		{"retrieval", auth.ServiceRoleDataRead, auth.OperationRetrieve},
	} {
		t.Run(test.name, func(t *testing.T) {
			store, err := OpenDurableStore(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			defer store.Close()
			base := testDecision(t, decisionOptions{
				serviceRole: test.role,
				ceilingID:   "narrow-role-ceiling",
				operations:  []auth.Operation{test.operation},
			})
			restriction, err := auth.NewPolicy(auth.PolicyConfig{
				AuthorizationDomain: base.AuthorizationDomain(),
				SourceID:            []byte("source-a"),
				GrantPolicyID:       []byte("policy-a"),
				Epoch:               1,
			})
			if err != nil {
				t.Fatal(err)
			}
			outputBytes := uint64(1024)
			settings, err := store.CompareAndSwap(
				context.Background(), "owned-workspace", base.Subject(),
				base.AuthorizationDomain(), 0, "create",
				Narrowing{
					Budgets:        Budgets{OutputBytes: &outputBytes},
					OutputPolicies: []auth.Policy{restriction},
				},
			)
			if err != nil {
				t.Fatal(err)
			}
			ceiling := serviceCeilingForDecision(
				t, base, "source-a", "policy-a", 1)
			provider, err := NewProvider(store, ProviderOptions{
				Resolver:         &mutableResolver{decision: base},
				GenerationReader: testGenerationReader{generation: 7},
				CeilingResolver:  roleCeilingResolver{test.role: ceiling},
				Clock:            func() time.Time { return testNow },
			})
			if err != nil {
				t.Fatal(err)
			}
			effective, err := provider.ApplyForOperation(
				context.Background(), settings.WorkspaceID,
				test.operation,
				MaximumLimits(), nil)
			if err != nil {
				t.Fatalf(
					"owned narrowing disables otherwise-authorized %s: %v",
					test.operation, err)
			}
			if effective.Limits().OutputBytes != outputBytes ||
				effective.Revision() != settings.Revision ||
				len(effective.OutputPolicies()) != 1 ||
				effective.OutputPolicies()[0].ServiceRole() != "" {
				t.Fatalf("workspace restriction was not applied: %#v", effective)
			}
			resource := auth.ResourceRequest{
				AuthorizationDomain: base.AuthorizationDomain(),
				SourceID:            []byte("source-a"),
				PolicyID:            []byte("policy-a"),
			}
			if err := effective.Decision().Authorize(
				test.operation, resource, testNow,
			); err != nil {
				t.Fatalf("narrowing lost the service operation: %v", err)
			}
			if _, err := provider.Get(
				context.Background(), settings.WorkspaceID,
			); !shoal.IsErrorCode(err, shoal.ErrorUnauthorized) {
				t.Fatalf(
					"application granted settings-management read access: %v",
					err)
			}
		})
	}
}

func serviceCeilingForDecision(
	t *testing.T,
	decision auth.Decision,
	source, policy string,
	epoch int64,
) auth.ServiceCeiling {
	t.Helper()
	servicePolicy := testPolicy(t, decision, source, policy, epoch)
	visibility, err := servicePolicy.Encode()
	if err != nil {
		t.Fatal(err)
	}
	ceiling, err := auth.NewServiceCeiling(auth.ServiceCeilingConfig{
		Identity: decision.ServiceCeilingIdentity(),
		Role:     decision.ServiceRole(),
		Authorizations: accumulo.NewAuthorizations(
			bytes.Split(visibility, []byte("&"))...),
	})
	if err != nil {
		t.Fatal(err)
	}
	return ceiling
}

func TestReviewSettingsPostCommitExpiryReturnsResultAndIndeterminate(t *testing.T) {
	durable, err := OpenDurableStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer durable.Close()
	store := &reviewBlockingStore{Store: durable}
	base := testDecision(t, decisionOptions{})
	now := testNow
	provider, err := NewProvider(store, ProviderOptions{
		Resolver:         &mutableResolver{decision: base},
		GenerationReader: testGenerationReader{generation: 7},
		Clock:            func() time.Time { return now },
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := provider.Update(
		context.Background(), "postcommit-workspace",
		UpdateRequest{MutationID: "create"},
	); err != nil {
		t.Fatal(err)
	}
	store.afterCAS = func() { now = base.AuthenticationExpires() }
	result, err := provider.Update(
		context.Background(), "postcommit-workspace",
		UpdateRequest{ExpectedRevision: 1, MutationID: "postcommit"},
	)
	if result.Revision != 2 || !explorer.IsIndeterminateCommit(err) ||
		!shoal.IsErrorCode(err, shoal.ErrorUnauthorized) {
		t.Fatalf("postcommit result = %#v, error = %v", result, err)
	}
	stored, loadErr := durable.Load(
		context.Background(), "postcommit-workspace")
	if loadErr != nil {
		t.Fatal(loadErr)
	}
	if stored.Revision != 2 ||
		stored.LastMutationID != "postcommit" {
		t.Fatalf("postcommit durable state = %#v", stored)
	}
}

func TestReviewSettingsConditionalWriteErrorReadback(t *testing.T) {
	t.Run("exact committed mutation", func(t *testing.T) {
		store, err := OpenDurableStore(t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		defer store.Close()
		write := store.conditionalWrite
		store.conditionalWrite = func(
			table string,
			mutations []engine.ConditionalMutation,
		) ([]bool, error) {
			if _, err := write(table, mutations); err != nil {
				return nil, err
			}
			return nil, errors.New("response lost after WAL append")
		}
		result, err := store.CompareAndSwap(
			context.Background(), "readback", "owner", []byte("domain"),
			0, "mutation", Narrowing{})
		if err != nil || result.Revision != 1 {
			t.Fatalf("readback result = %#v, error = %v", result, err)
		}
	})

	t.Run("unknown outcome", func(t *testing.T) {
		store, err := OpenDurableStore(t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		defer store.Close()
		store.conditionalWrite = func(
			string,
			[]engine.ConditionalMutation,
		) ([]bool, error) {
			return nil, errors.New("write response unavailable")
		}
		result, err := store.CompareAndSwap(
			context.Background(), "unknown", "owner", []byte("domain"),
			0, "mutation", Narrowing{})
		if result.WorkspaceID != "" || result.Revision != 0 ||
			!explorer.IsIndeterminateCommit(err) ||
			!shoal.IsErrorCode(err, shoal.ErrorUnavailable) {
			t.Fatalf("unknown result = %#v, error = %v", result, err)
		}
	})

	t.Run("malformed committed result", func(t *testing.T) {
		store, err := OpenDurableStore(t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		defer store.Close()
		write := store.conditionalWrite
		store.conditionalWrite = func(
			table string,
			mutations []engine.ConditionalMutation,
		) ([]bool, error) {
			if _, err := write(table, mutations); err != nil {
				return nil, err
			}
			return nil, nil
		}
		result, err := store.CompareAndSwap(
			context.Background(), "malformed-committed",
			"owner", []byte("domain"), 0, "mutation", Narrowing{})
		if err != nil || result.Revision != 1 {
			t.Fatalf("malformed committed result = %#v, error = %v",
				result, err)
		}
	})

	t.Run("malformed unknown result", func(t *testing.T) {
		store, err := OpenDurableStore(t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		defer store.Close()
		store.conditionalWrite = func(
			string,
			[]engine.ConditionalMutation,
		) ([]bool, error) {
			return nil, nil
		}
		result, err := store.CompareAndSwap(
			context.Background(), "malformed-unknown",
			"owner", []byte("domain"), 0, "mutation", Narrowing{})
		if result.WorkspaceID != "" || result.Revision != 0 ||
			!explorer.IsIndeterminateCommit(err) ||
			!shoal.IsErrorCode(err, shoal.ErrorInternal) {
			t.Fatalf("malformed unknown result = %#v, error = %v",
				result, err)
		}
	})
}

func TestReviewSettingsReadbackConcealsForeignWinner(t *testing.T) {
	record := persistedSettings{
		Owner:               "other-owner",
		AuthorizationDomain: []byte("domain"),
		LastMutationID:      "other-mutation",
	}
	replayed, _, err := replayResult(
		record, true, "owner", []byte("domain"),
		0, "mutation", [sha256.Size]byte{},
	)
	if !replayed || !shoal.IsErrorCode(err, shoal.ErrorNotFound) {
		t.Fatalf("foreign readback replayed=%v, error=%v", replayed, err)
	}
}

func TestReviewSettingsRejectConcurrentDirectoryOpen(t *testing.T) {
	directory := t.TempDir()
	first, err := OpenDurableStore(directory)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(directory, settingsLockFile)); err != nil {
		t.Fatalf("settings lock file: %v", err)
	}
	defer first.Close()
	second, err := OpenDurableStore(directory)
	if err == nil {
		defer second.Close()
		t.Fatal("two independent settings engines accepted the same WAL directory")
	}
}

func TestReviewSettingsRejectExplorerRuntimeDirectory(t *testing.T) {
	directory := t.TempDir()
	runtime, err := explorer.Open(directory)
	if err != nil {
		t.Fatal(err)
	}

	if _, err := OpenDurableStore(directory); !shoal.IsErrorCode(err, shoal.ErrorUnavailable) {
		t.Fatalf("expected unavailable while explorer runtime owns directory, got %v", err)
	}
	if err := runtime.Close(); err != nil {
		t.Fatal(err)
	}

	store, err := OpenDurableStore(directory)
	if err != nil {
		t.Fatalf("open settings store after explorer runtime close: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestReviewSettingsStoreRejectsPathAliasAndReopensAfterClose(t *testing.T) {
	root := t.TempDir()
	directory := filepath.Join(root, "settings")
	aliasParent := filepath.Join(root, "alias")
	if err := os.MkdirAll(directory, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(aliasParent, 0o755); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(aliasParent, "..", "settings")
	first, err := OpenDurableStore(directory)
	if err != nil {
		t.Fatal(err)
	}
	if second, err := OpenDurableStore(alias); !errors.Is(err, dirlock.ErrLocked) ||
		!shoal.IsErrorCode(err, shoal.ErrorUnavailable) {
		if second != nil {
			_ = second.Close()
		}
		t.Fatalf("path-alias open error = %v", err)
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := OpenDurableStore(directory)
	if err != nil {
		t.Fatalf("reopen after close: %v", err)
	}
	if err := reopened.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestReviewSettingsStoreRetainsDirectoryLockWhenEngineCloseFails(
	t *testing.T,
) {
	directory := t.TempDir()
	store, err := OpenDurableStore(directory)
	if err != nil {
		t.Fatal(err)
	}
	closeEngine := store.closeEngine
	attempts := 0
	store.closeEngine = func() error {
		attempts++
		if attempts == 1 {
			return errors.New("simulated engine close failure")
		}
		return closeEngine()
	}
	if err := store.Close(); !shoal.IsErrorCode(err, shoal.ErrorInternal) {
		t.Fatalf("first close error = %v", err)
	}
	if store.closed || store.engineClosed {
		t.Fatalf("failed close marked store closed: %#v", store)
	}
	if second, err := OpenDurableStore(directory); !shoal.IsErrorCode(
		err, shoal.ErrorUnavailable,
	) {
		if second != nil {
			_ = second.Close()
		}
		t.Fatalf("failed engine close released directory lock: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("retry close: %v", err)
	}
	reopened, err := OpenDurableStore(directory)
	if err != nil {
		t.Fatalf("reopen after successful close retry: %v", err)
	}
	if err := reopened.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestReviewSettingsStoreRechecksCancellationAfterMutexWait(t *testing.T) {
	store, err := OpenDurableStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	store.mu.Lock()
	ctx, cancel := context.WithCancel(context.Background())
	started := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		close(started)
		_, err := store.CompareAndSwap(
			ctx, "cancelled-workspace", "owner", []byte("domain"),
			0, "cancelled-mutation", Narrowing{})
		done <- err
	}()
	<-started
	time.Sleep(10 * time.Millisecond)
	cancel()
	store.mu.Unlock()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("queued canceled mutation error = %v", err)
	}
	if _, err := store.Load(
		context.Background(), "cancelled-workspace",
	); !shoal.IsErrorCode(err, shoal.ErrorNotFound) {
		t.Fatalf("queued canceled mutation committed: %v", err)
	}
}

func TestReviewSettingsStoreCompactsSupersededRevisions(t *testing.T) {
	directory := t.TempDir()
	store, err := OpenDurableStore(directory)
	if err != nil {
		t.Fatal(err)
	}
	for revision := uint64(0); revision < 12; revision++ {
		topK := uint32(20 - revision)
		if _, err := store.CompareAndSwap(
			context.Background(), "retained-workspace", "owner", []byte("domain"),
			revision, shoal.ID(fmt.Sprintf("mutation-%d", revision)),
			Narrowing{Budgets: Budgets{RetrievalTopK: &topK}},
		); err != nil {
			t.Fatalf("revision %d: %v", revision+1, err)
		}
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := OpenDurableStore(directory)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	rawVersions := 0
	if err := reopened.engine.LookupRows(
		settingsTable,
		[][]byte{settingsRow("retained-workspace")},
		engine.ScanOptions{
			ColumnFamilies:          [][]byte{[]byte(settingsCF)},
			ColumnFamiliesInclusive: true,
		},
		func(_ int, key *iterrt.Key, _ []byte) {
			if bytes.Equal(key.ColumnFamily, []byte(settingsCF)) &&
				bytes.Equal(key.ColumnQualifier, []byte(settingsCQ)) {
				rawVersions++
			}
		},
	); err != nil {
		t.Fatal(err)
	}
	if rawVersions != 1 {
		t.Fatalf("raw retained versions = %d, want 1", rawVersions)
	}
	loaded, err := reopened.Load(context.Background(), "retained-workspace")
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Revision != 12 ||
		loaded.Narrowing.Budgets.RetrievalTopK == nil ||
		*loaded.Narrowing.Budgets.RetrievalTopK != 9 {
		t.Fatalf("retained settings = %#v", loaded)
	}
}

func TestReviewSettingsStoreSchedulesCompactionOutsideStoreMutex(t *testing.T) {
	store, err := OpenDurableStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if _, err := store.CompareAndSwap(
		context.Background(), "background-compact", "owner", []byte("domain"),
		0, "create", Narrowing{},
	); err != nil {
		t.Fatal(err)
	}
	store.maintenanceMu.Lock()
	store.uncompacted = settingsCompactInterval - 1
	store.maintenanceMu.Unlock()
	started := make(chan struct{})
	release := make(chan struct{})
	defer close(release)
	compact := store.compact
	store.compact = func(table string, stack []iterrt.IterSpec) error {
		close(started)
		<-release
		return compact(table, stack)
	}
	topK := uint32(5)
	if _, err := store.CompareAndSwap(
		context.Background(), "background-compact", "owner", []byte("domain"),
		1, "narrow", Narrowing{
			Budgets: Budgets{RetrievalTopK: &topK},
		},
	); err != nil {
		t.Fatal(err)
	}
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("background compaction was not scheduled")
	}
	if _, err := store.Load(
		context.Background(), "background-compact"); err != nil {
		t.Fatalf("store mutex remained held during compaction: %v", err)
	}
}

func TestReviewSettingsReplayDoesNotScheduleCompaction(t *testing.T) {
	store, err := OpenDurableStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	created, err := store.CompareAndSwap(
		context.Background(), "replay-compact", "owner", []byte("domain"),
		0, "create", Narrowing{})
	if err != nil {
		t.Fatal(err)
	}
	store.maintenanceMu.Lock()
	store.uncompacted = settingsCompactInterval - 1
	store.maintenanceMu.Unlock()
	compactions := 0
	compact := store.compact
	store.compact = func(table string, stack []iterrt.IterSpec) error {
		compactions++
		return compact(table, stack)
	}
	replayed, err := store.CompareAndSwap(
		context.Background(), "replay-compact", "owner", []byte("domain"),
		0, "create", Narrowing{})
	if err != nil {
		t.Fatal(err)
	}
	if replayed.Revision != created.Revision {
		t.Fatalf("replayed revision = %d, want %d",
			replayed.Revision, created.Revision)
	}
	time.Sleep(20 * time.Millisecond)
	if compactions != 0 {
		t.Fatalf("exact replay scheduled %d compactions", compactions)
	}
}

func TestReviewSettingsStoreCanUseNonOwningSharedEngine(t *testing.T) {
	eng, err := engine.Open(t.TempDir(), engine.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer eng.Close()

	store, err := NewDurableStoreWithEngine(eng)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := NewDurableStoreWithEngine(eng); !shoal.IsErrorCode(
		err, shoal.ErrorUnavailable,
	) {
		t.Fatalf("duplicate shared-engine store error = %v", err)
	}
	created, err := store.CompareAndSwap(
		context.Background(), "shared-workspace", "owner", []byte("domain"),
		0, "create", Narrowing{})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := NewDurableStoreWithEngine(eng)
	if err != nil {
		t.Fatalf("reattach after store close: %v", err)
	}
	loaded, err := reopened.Load(context.Background(), "shared-workspace")
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Revision != created.Revision ||
		loaded.SettingsID != created.SettingsID {
		t.Fatalf("shared-engine reload = %#v, want %#v", loaded, created)
	}
	if err := reopened.Close(); err != nil {
		t.Fatal(err)
	}
	if err := eng.CreateTable(
		"after-settings-close", engine.TableOptions{},
	); err != nil {
		t.Fatalf("settings store closed its caller-owned engine: %v", err)
	}
}

func TestReviewStoreCASRechecksMonotonicityAtAcceptedRevision(t *testing.T) {
	store, err := OpenDurableStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if _, err := store.CompareAndSwap(
		context.Background(), "race-monotonic", "owner", []byte("domain"),
		0, "create", Narrowing{},
	); err != nil {
		t.Fatal(err)
	}
	topK := uint32(5)
	narrowed, err := store.CompareAndSwap(
		context.Background(), "race-monotonic", "owner", []byte("domain"),
		1, "narrow", Narrowing{
			Budgets: Budgets{RetrievalTopK: &topK},
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if narrowed.Revision != 2 {
		t.Fatalf("narrowed revision = %d", narrowed.Revision)
	}
	if _, err := store.CompareAndSwap(
		context.Background(), "race-monotonic", "owner", []byte("domain"),
		2, "stale-future", Narrowing{},
	); !shoal.IsErrorCode(err, shoal.ErrorUnauthorized) {
		t.Fatalf("store widening error = %v", err)
	}
	current, err := store.Load(context.Background(), "race-monotonic")
	if err != nil {
		t.Fatal(err)
	}
	if current.Revision != 2 ||
		current.Narrowing.Budgets.RetrievalTopK == nil ||
		*current.Narrowing.Budgets.RetrievalTopK != topK {
		t.Fatalf("current settings = %#v", current)
	}
}
