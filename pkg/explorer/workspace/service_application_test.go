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
	"context"
	"testing"
	"time"

	"github.com/phrocker/shoal-oss/pkg/explorer/auth"
	"github.com/phrocker/shoal-oss/pkg/ontology"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

type operationRecordingOntologyChoices struct {
	identity  ontology.OntologyIdentity
	operation auth.Operation
}

func (c *operationRecordingOntologyChoices) ListOntologyChoices(
	context.Context,
	auth.Decision,
) ([]OntologyChoice, error) {
	return []OntologyChoice{{Identity: c.identity, Active: true}}, nil
}

func (c *operationRecordingOntologyChoices) AuthorizeOntology(
	context.Context,
	auth.Decision,
	ontology.OntologyIdentity,
) error {
	return shoal.NewError(
		shoal.ErrorUnauthorized, "operation-specific authorization required")
}

func (c *operationRecordingOntologyChoices) AuthorizeOntologyForOperation(
	_ context.Context,
	_ auth.Decision,
	identity ontology.OntologyIdentity,
	operation auth.Operation,
) error {
	c.operation = operation
	if identity != c.identity || operation != auth.OperationRetrieve {
		return shoal.NewError(shoal.ErrorUnauthorized, "authorization denied")
	}
	return nil
}

func TestNarrowServiceRolesCanApplyOwnedWorkspaceRestrictions(t *testing.T) {
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
			resource := auth.ResourceRequest{
				AuthorizationDomain: base.AuthorizationDomain(),
				SourceID:            []byte("source-a"),
				PolicyID:            []byte("policy-a"),
			}
			if err := base.Authorize(test.operation, resource, testNow); err != nil {
				t.Fatalf("ordinary service operation is not authorized: %v", err)
			}
			policy, err := neutralOutputPolicy(
				testPolicy(t, base, "source-a", "policy-a", 1))
			if err != nil {
				t.Fatal(err)
			}
			outputBytes := uint64(1024)
			settings, err := store.CompareAndSwap(
				context.Background(), "owned-workspace", base.Subject(),
				base.AuthorizationDomain(), 0, "create",
				Narrowing{
					Budgets:        Budgets{OutputBytes: &outputBytes},
					OutputPolicies: []auth.Policy{policy},
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
				test.operation, MaximumLimits(), nil)
			if err != nil {
				t.Fatalf("owned narrowing disables otherwise-authorized %s: %v", test.operation, err)
			}
			if effective.Limits().OutputBytes != outputBytes ||
				effective.Revision() != settings.Revision {
				t.Fatalf("workspace restriction was not applied: %#v", effective)
			}
			outputPolicies := effective.OutputPolicies()
			if len(outputPolicies) != 1 ||
				!policySubset([]auth.Policy{policy}, outputPolicies) {
				t.Fatalf("workspace output restriction was not retained: %#v", outputPolicies)
			}
			if err := effective.Decision().Authorize(
				test.operation, resource, testNow,
			); err != nil {
				t.Fatalf("narrowing lost the service's operation: %v", err)
			}
			if _, err := provider.Get(
				context.Background(), settings.WorkspaceID,
			); !shoal.IsErrorCode(err, shoal.ErrorUnauthorized) {
				t.Fatalf("application granted settings-management read access: %v", err)
			}
		})
	}
}

func TestApplyForOperationUsesConsumingOperationForSelectedLens(t *testing.T) {
	store, err := OpenDurableStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	schema, err := ontology.NewOntologySchema("workspace", "Workspace", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	version, err := ontology.NewOntologyVersion(
		schema, "1", testNow, nil, nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	identity, err := ontology.NewOntologyIdentity(version)
	if err != nil {
		t.Fatal(err)
	}
	base := testDecision(t, decisionOptions{
		operations: []auth.Operation{
			auth.OperationAnalyticsRead,
			auth.OperationRetrieve,
		},
	})
	settings, err := store.CompareAndSwap(
		context.Background(), "operation-workspace", base.Subject(),
		base.AuthorizationDomain(), 0, "create",
		Narrowing{SelectedOntology: OntologySelection{
			Present: true, Identity: identity,
		}},
	)
	if err != nil {
		t.Fatal(err)
	}
	choices := &operationRecordingOntologyChoices{identity: identity}
	provider, err := NewProvider(store, ProviderOptions{
		Resolver:         &mutableResolver{decision: base},
		GenerationReader: testGenerationReader{generation: 7},
		OntologyChoices:  choices,
		Clock:            func() time.Time { return testNow },
	})
	if err != nil {
		t.Fatal(err)
	}
	effective, err := provider.ApplyForOperation(
		context.Background(), settings.WorkspaceID,
		auth.OperationRetrieve, MaximumLimits(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if choices.operation != auth.OperationRetrieve {
		t.Fatalf("ontology authorization operation = %q", choices.operation)
	}
	selected, ok := effective.Decision().SelectedOntology()
	if !ok || selected != identity {
		t.Fatalf("effective selected ontology = %#v, %v", selected, ok)
	}

	removed, err := store.CompareAndSwap(
		context.Background(), "removed-operation-workspace", base.Subject(),
		base.AuthorizationDomain(), 0, "remove-retrieve",
		Narrowing{
			AllowedOperations: OperationSelection{
				Present: true,
				Values:  []auth.Operation{auth.OperationAnalyticsRead},
			},
			SelectedOntology: OntologySelection{
				Present: true, Identity: identity,
			},
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := provider.ApplyForOperation(
		context.Background(), removed.WorkspaceID,
		auth.OperationRetrieve, MaximumLimits(), nil,
	); !shoal.IsErrorCode(err, shoal.ErrorUnauthorized) {
		t.Fatalf("removed consuming operation error = %v", err)
	}
}
