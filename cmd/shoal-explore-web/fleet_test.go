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

package main

import (
	"context"
	"encoding/json"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/phrocker/shoal-oss/pkg/explorer"
	"github.com/phrocker/shoal-oss/pkg/explorer/auth"
	"github.com/phrocker/shoal-oss/pkg/explorer/fleet"
	"github.com/phrocker/shoal-oss/pkg/explorer/fleetevents"
	"github.com/phrocker/shoal-oss/pkg/interaction"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

type mutableFleetGeneration struct {
	value atomic.Int64
}

func (r *mutableFleetGeneration) CurrentPolicyGeneration(
	ctx context.Context, domain []byte,
) (int64, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	if string(domain) != string(workspaceAuthorizationDomain) {
		return 0, nil
	}
	return r.value.Load(), nil
}

type nilBoundFleetResolver struct{}

func (*nilBoundFleetResolver) Resolve(
	context.Context,
) (auth.Decision, error) {
	panic("typed-nil resolver must be rejected")
}

func TestNewBoundFleetRegistryRejectsTypedNilResolver(t *testing.T) {
	var resolver *nilBoundFleetResolver
	if _, err := newBoundFleetRegistry(
		&fleet.Service{}, resolver,
	); !shoal.IsErrorCode(err, shoal.ErrorInvalidArgument) {
		t.Fatalf("typed-nil resolver error = %v", err)
	}
}

func TestOpenServiceComposesFleetWithActionOnlyRecording(t *testing.T) {
	root := t.TempDir()
	now := time.Now().UTC().Add(time.Minute)
	authority := auth.NewAuthority()
	opened, err := openService(context.Background(), serviceConfig{
		backend: "embedded", data: filepath.Join(root, "corpus"),
		policyDir: filepath.Join(root, "policy"),
		resolver:  authority.Resolver(), clock: func() time.Time { return now },
		executors: configuredFleetExecutors{
			"local": configuredFleetExecutor{reference: "local"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	if opened.fleetRegistry == nil {
		opened.close()
		t.Fatal("embedded service did not compose the fleet registry")
	}
	decision, err := auth.NewDecision(auth.DecisionConfig{
		Subject: "owner", Actor: "operator",
		AuthorizationDomain:   workspaceAuthorizationDomain,
		AllowedOperations:     []auth.Operation{auth.OperationAgentRegister},
		PermittedSourceIDs:    [][]byte{workspaceSourceID},
		PermittedPolicyIDs:    [][]byte{workspaceGrantPolicyID},
		PolicyGeneration:      workspacePolicyGeneration,
		AuthenticationExpires: now.Add(time.Hour),
		RequestID:             "fleet-register-request",
	})
	if err != nil {
		opened.close()
		t.Fatal(err)
	}
	ctx, err := authority.Binder().Bind(context.Background(), decision)
	if err != nil {
		opened.close()
		t.Fatal(err)
	}
	descriptor, err := opened.fleetRegistry.Register(ctx, fleet.RegisterRequest{
		Context: fleet.RequestContext{
			RequestID: "caller-supplied", CorrelationID: "caller-correlation",
			ReasonCode: "test",
			Deadline:   now.Add(time.Minute),
		},
		RegistrationKey: "registration-key",
		Spec: fleet.Spec{
			ID: "agent", AuthorizationDomain: workspaceAuthorizationDomain,
			Scopes: []fleet.Scope{{
				SourceID: workspaceSourceID, PolicyID: workspaceGrantPolicyID,
			}},
			ExecutorRef: "local",
			Capabilities: []fleet.Capability{{
				Name: "documents",
				Actions: []fleet.Action{{
					Name: "list", InputSchema: json.RawMessage(`{"type":"object"}`),
					OutputSchema: json.RawMessage(`{"type":"object"}`),
				}},
			}},
			LeaseExpiresAt: now.Add(10 * time.Minute),
		},
	})
	if err != nil {
		opened.close()
		t.Fatal(err)
	}
	if descriptor.ID != "agent" || descriptor.Generation != 1 {
		opened.close()
		t.Fatalf("registered descriptor = %#v", descriptor)
	}
	opened.close()

	reopened, err := openService(context.Background(), serviceConfig{
		backend: "embedded", data: filepath.Join(root, "corpus"),
		policyDir: filepath.Join(root, "policy"),
		resolver:  authority.Resolver(), clock: func() time.Time { return now },
		executors: configuredFleetExecutors{
			"local": configuredFleetExecutor{reference: "local"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	resolveDecision, err := auth.NewDecision(auth.DecisionConfig{
		Subject: "owner", Actor: "operator",
		AuthorizationDomain:   workspaceAuthorizationDomain,
		AllowedOperations:     []auth.Operation{auth.OperationAgentResolve},
		PermittedSourceIDs:    [][]byte{workspaceSourceID},
		PermittedPolicyIDs:    [][]byte{workspaceGrantPolicyID},
		PolicyGeneration:      workspacePolicyGeneration,
		AuthenticationExpires: now.Add(time.Hour),
		RequestID:             "fleet-resolve-request",
	})
	if err != nil {
		reopened.close()
		t.Fatal(err)
	}
	resolveContext, err := authority.Binder().Bind(
		context.Background(), resolveDecision)
	if err != nil {
		reopened.close()
		t.Fatal(err)
	}
	resolved, err := reopened.fleetRegistry.Resolve(
		resolveContext,
		fleet.ResolveRequest{
			Context: fleet.RequestContext{
				RequestID: "spoofed-resolve-request", ReasonCode: "test",
				Deadline: now.Add(time.Minute),
			},
			ID: "agent",
		},
	)
	if err != nil {
		reopened.close()
		t.Fatal(err)
	}
	if resolved.Descriptor.ID != descriptor.ID ||
		resolved.Descriptor.Generation != descriptor.Generation {
		reopened.close()
		t.Fatalf("restarted fleet resolve = %#v", resolved.Descriptor)
	}
	reopened.close()

	corpus, err := explorer.Open(filepath.Join(root, "corpus"))
	if err != nil {
		t.Fatal(err)
	}
	defer corpus.Close()
	summaries, err := corpus.Interactions(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(summaries) != 2 {
		t.Fatalf("fleet lifecycle summaries = %#v", summaries)
	}
	var recorded interaction.Session
	for _, summary := range summaries {
		if summary.AuthorizationOperation !=
			string(auth.OperationAgentRegister) {
			continue
		}
		recorded, err = corpus.Interaction(
			context.Background(), summary.SessionID)
		if err != nil {
			t.Fatal(err)
		}
	}
	if recorded.ID == "" ||
		recorded.Operation != interaction.OperationToolCall ||
		recorded.AuthorizationOperation != string(auth.OperationAgentRegister) ||
		recorded.AuthorizationFingerprint !=
			shoal.ID(mustAuthorizationFingerprint(t, decision).String()) ||
		!recorded.AuthorizationExpiresAt.Equal(
			decision.AuthenticationExpires()) ||
		recorded.SnapshotID == "" ||
		recorded.SnapshotAsOf.IsZero() ||
		recorded.RequestID != decision.RequestID() ||
		recorded.ResultID != descriptor.ID ||
		recorded.Actor.SubjectID != decision.Subject() ||
		recorded.Actor.ActorID != decision.Actor() ||
		recorded.Reason != (interaction.Reason{}) ||
		len(recorded.Turns) != 1 ||
		recorded.Turns[0].ToolCall == nil ||
		recorded.Turns[0].ToolCall.Kind !=
			"fleet.registry."+string(auth.OperationAgentRegister) {
		t.Fatalf("fleet lifecycle interaction = %#v", recorded)
	}
}

func mustAuthorizationFingerprint(
	t *testing.T,
	decision auth.Decision,
) auth.Fingerprint {
	t.Helper()
	fingerprint, err := auth.AuthorizationFingerprint(decision)
	if err != nil {
		t.Fatal(err)
	}
	return fingerprint
}

func TestOpenServiceLongPollObservesPolicyOnlyRevocation(t *testing.T) {
	root := t.TempDir()
	now := time.Now().UTC()
	authority := auth.NewAuthority()
	generations := &mutableFleetGeneration{}
	generations.value.Store(1)
	opened, err := openService(context.Background(), serviceConfig{
		backend: "embedded", data: filepath.Join(root, "corpus"),
		policyDir: filepath.Join(root, "policy"),
		resolver:  authority.Resolver(), clock: time.Now,
		generationReader: generations,
		executors: configuredFleetExecutors{
			"local": configuredFleetExecutor{reference: "local"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer opened.close()
	if opened.fleetRegistry == nil || opened.fleetEvents == nil {
		t.Fatal("embedded service did not compose Fleet delivery")
	}

	registerDecision, err := auth.NewDecision(auth.DecisionConfig{
		Subject: "subscriber", Actor: "operator",
		AuthorizationDomain: workspaceAuthorizationDomain,
		AllowedOperations: []auth.Operation{
			auth.OperationAgentRegister, auth.OperationAgentResolve,
		},
		PermittedSourceIDs:    [][]byte{workspaceSourceID},
		PermittedPolicyIDs:    [][]byte{workspaceGrantPolicyID},
		PolicyGeneration:      1,
		AuthenticationExpires: now.Add(time.Hour),
		RequestID:             "register-request",
	})
	if err != nil {
		t.Fatal(err)
	}
	registerContext, err := authority.Binder().Bind(
		context.Background(), registerDecision)
	if err != nil {
		t.Fatal(err)
	}
	descriptor, err := opened.fleetRegistry.Register(
		registerContext, fleet.RegisterRequest{
			Context: fleet.RequestContext{
				ReasonCode: "test", Deadline: now.Add(time.Minute),
			},
			RegistrationKey: "registration-key",
			Spec: fleet.Spec{
				ID: "agent", AuthorizationDomain: workspaceAuthorizationDomain,
				Scopes: []fleet.Scope{{
					SourceID: workspaceSourceID,
					PolicyID: workspaceGrantPolicyID,
				}},
				ExecutorRef: "local",
				Capabilities: []fleet.Capability{{
					Name: "documents",
					Actions: []fleet.Action{{
						Name:         "list",
						InputSchema:  json.RawMessage(`{"type":"object"}`),
						OutputSchema: json.RawMessage(`{"type":"object"}`),
					}},
				}},
				LeaseExpiresAt: now.Add(10 * time.Minute),
			},
		})
	if err != nil {
		t.Fatal(err)
	}

	createDecision, err := auth.NewDecision(auth.DecisionConfig{
		Subject: "subscriber", Actor: "operator",
		AuthorizationDomain: workspaceAuthorizationDomain,
		AllowedOperations: []auth.Operation{
			auth.OperationAgentResolve,
			auth.OperationSubscriptionCreate,
		},
		PermittedSourceIDs:    [][]byte{workspaceSourceID},
		PermittedPolicyIDs:    [][]byte{workspaceGrantPolicyID},
		PolicyGeneration:      1,
		AuthenticationExpires: now.Add(time.Hour),
		RequestID:             "create-request",
	})
	if err != nil {
		t.Fatal(err)
	}
	createContext, err := authority.Binder().Bind(
		context.Background(), createDecision)
	if err != nil {
		t.Fatal(err)
	}
	subscription, err := opened.fleetEvents.Create(
		createContext, fleetevents.CreateRequest{
			Token: []byte("create-token"), AgentID: descriptor.ID,
			AgentGeneration: descriptor.Generation, TTL: time.Minute,
			RetryUntil: now.Add(time.Hour),
		})
	if err != nil {
		t.Fatal(err)
	}

	pullDecision, err := auth.NewDecision(auth.DecisionConfig{
		Subject: "subscriber", Actor: "operator",
		AuthorizationDomain: workspaceAuthorizationDomain,
		AllowedOperations: []auth.Operation{
			auth.OperationAgentResolve,
			auth.OperationSubscriptionDeliver,
		},
		PermittedSourceIDs:    [][]byte{workspaceSourceID},
		PermittedPolicyIDs:    [][]byte{workspaceGrantPolicyID},
		PolicyGeneration:      1,
		AuthenticationExpires: now.Add(time.Hour),
		RequestID:             "pull-request",
	})
	if err != nil {
		t.Fatal(err)
	}
	pullContext, err := authority.Binder().Bind(
		context.Background(), pullDecision)
	if err != nil {
		t.Fatal(err)
	}
	result := make(chan error, 1)
	go func() {
		_, pullErr := opened.fleetEvents.Pull(
			pullContext, fleetevents.PullRequest{
				SubscriptionID: subscription.ID,
				Limit:          1,
				Wait:           2 * time.Second,
			})
		result <- pullErr
	}()
	time.Sleep(100 * time.Millisecond)
	generations.value.Store(2)
	select {
	case err := <-result:
		if !shoal.IsErrorCode(err, shoal.ErrorUnavailable) &&
			!shoal.IsErrorCode(err, shoal.ErrorUnauthorized) {
			t.Fatalf("policy-only revocation error = %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("long poll did not observe policy-only revocation")
	}
}

func TestConfiguredFleetExecutorsFailClosed(t *testing.T) {
	registry, err := newConfiguredFleetExecutors(nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := registry.ResolveExecutor("local"); ok {
		t.Fatal("unconfigured executor was resolved")
	}
	if _, err := newConfiguredFleetExecutors([]string{" bad "}); err == nil {
		t.Fatal("invalid executor reference was accepted")
	}
	registry, err = newConfiguredFleetExecutors([]string{"local", "local"})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := registry.ResolveExecutor("local"); !ok {
		t.Fatal("configured executor was not resolved")
	}
}
