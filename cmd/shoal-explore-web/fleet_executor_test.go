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
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/phrocker/shoal-oss/pkg/explorer"
	"github.com/phrocker/shoal-oss/pkg/explorer/auth"
	"github.com/phrocker/shoal-oss/pkg/explorer/fleet"
	"github.com/phrocker/shoal-oss/pkg/explorer/webapi"
	"github.com/phrocker/shoal-oss/pkg/inference"
	"github.com/phrocker/shoal-oss/pkg/model"
	"github.com/phrocker/shoal-oss/pkg/retrieval"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

// bindStubProvider satisfies the executor's provider dependency for tests that
// only need a real executor value to bind. It is never invoked.
type bindStubProvider struct{}

func (bindStubProvider) Ask(
	context.Context, webapi.AskRequest,
) (webapi.CitationEnvelope, error) {
	return webapi.CitationEnvelope{}, errors.New("bind stub is never invoked")
}

func askAgentSpec(now time.Time, provider webapi.AskProvider) fleet.Spec {
	return fleet.Spec{
		ID: "ask-agent", AuthorizationDomain: workspaceAuthorizationDomain,
		Scopes: []fleet.Scope{{
			SourceID: workspaceSourceID, PolicyID: workspaceGrantPolicyID,
		}},
		ExecutorRef: "ask",
		Capabilities: []fleet.Capability{{
			Name: webapi.AskCapability,
			Actions: []fleet.Action{{
				Name:         webapi.AskAction,
				InputSchema:  webapi.AskActionInputSchema(),
				OutputSchema: webapi.AskActionOutputSchema(),
				// Taken from the same exported source the executor derives its
				// own bound from, rather than restated here. A descriptor that
				// understates is refused at registration, and what it must
				// declare depends on where the operator pointed the model
				// provider — which is not something a registrant can guess.
				Effects: webapi.AskActionEffects(provider),
			}},
		}},
		LeaseExpiresAt: now.Add(10 * time.Minute),
	}
}

func askDecision(
	t *testing.T, now time.Time, requestID string, operations ...auth.Operation,
) auth.Decision {
	t.Helper()
	decision, err := auth.NewDecision(auth.DecisionConfig{
		Subject: "owner", Actor: "operator",
		AuthorizationDomain:   workspaceAuthorizationDomain,
		AllowedOperations:     operations,
		PermittedSourceIDs:    [][]byte{workspaceSourceID},
		PermittedPolicyIDs:    [][]byte{workspaceGrantPolicyID},
		PolicyGeneration:      workspacePolicyGeneration,
		AuthenticationExpires: now.Add(time.Hour),
		RequestID:             shoal.ID(requestID),
		CorrelationID:         shoal.ID(requestID + "-correlation"),
	})
	if err != nil {
		t.Fatal(err)
	}
	return decision
}
func TestBindRejectsUnallowlistedExecutorReference(t *testing.T) {
	t.Parallel()
	executor, err := webapi.NewAskExecutor(webapi.AskExecutorConfig{
		Provider: bindStubProvider{},
	})
	if err != nil {
		t.Fatal(err)
	}
	registry := configuredFleetExecutors{"allowed": configuredFleetExecutor{
		reference: "allowed",
	}}
	if err := registry.bind("smuggled", executor); err == nil {
		t.Fatal("binding an unallowlisted reference must fail closed")
	}
	if _, resolved := registry.ResolveExecutor("smuggled"); resolved {
		t.Fatal("a refused binding must not enter the registry")
	}
	if err := registry.bind("allowed", executor); err != nil {
		t.Fatalf("binding an allowlisted reference: %v", err)
	}
	bound, resolved := registry.ResolveExecutor("allowed")
	if !resolved {
		t.Fatal("an allowlisted reference must resolve after binding")
	}
	if _, ok := bound.(fleet.ActionExecutor); !ok {
		t.Fatal("a bound reference must resolve to a real action executor")
	}
}

// TestBindRejectsTypedNilExecutor proves a typed nil cannot enter the registry.
// fleet.Executor is an empty interface, so a plain nil check would admit one,
// and it would panic mid-dispatch after the effect-admission record is written.
func TestBindRejectsTypedNilExecutor(t *testing.T) {
	t.Parallel()
	registry := configuredFleetExecutors{"allowed": configuredFleetExecutor{
		reference: "allowed",
	}}
	var typedNil *webapi.AskExecutor
	if err := registry.bind("allowed", typedNil); err == nil {
		t.Fatal("a typed-nil executor must be refused")
	}
	bound, _ := registry.ResolveExecutor("allowed")
	if _, ok := bound.(fleet.ActionExecutor); ok {
		t.Fatal("a refused binding must not replace the placeholder")
	}
}

// TestAskExecutorCommitsEvidenceAgainstRealCorpus is the acceptance test for
// #362. Nothing in this path is a test double except the model itself, which
// is the deterministic offline FakeGenerator: a real corpus, a real authorized
// client, the real ChatService and inference harness, real retrieval and
// verification, and the real DispatchService commit. It is the only test that
// exercises the evidence-carrying branch of ActionRecorder, which pins
// EvidenceSnapshotID and ExecutionFingerprint and authorizes every evidence
// node.
func TestAskExecutorCommitsEvidenceAgainstRealCorpus(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	now := time.Now().UTC().Add(time.Minute)
	authority := auth.NewAuthority()
	// The reasoning service needs the authorized client, which openService
	// creates, but the executor must be bound before openService composes the
	// fleet. The registry entry is therefore filled in once both exist.
	executors := configuredFleetExecutors{"ask": configuredFleetExecutor{
		reference: "ask",
	}}
	opened, err := openService(context.Background(), serviceConfig{
		backend: "embedded", data: filepath.Join(root, "corpus"),
		policyDir: filepath.Join(root, "policy"),
		resolver:  authority.Resolver(), clock: func() time.Time { return now },
		executors: executors,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer opened.close()
	if opened.client == nil {
		t.Skip("embedded service exposes no authorized client")
	}

	provenance, err := inference.NewModelProvenance("fake", "fake", "", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	chat, err := webapi.NewChatService(context.Background(), webapi.ChatConfig{
		Client: opened.client, Resolver: authority.Resolver(),
		Generator: model.FakeGenerator{Model: "fake"}, Model: provenance,
		RetrievalModes: []retrieval.Mode{retrieval.ModeLexical},
		Clock:          func() time.Time { return now },
	})
	if err != nil {
		t.Fatal(err)
	}
	executor, err := webapi.NewAskExecutor(webapi.AskExecutorConfig{
		Provider: chat, Clock: func() time.Time { return now },
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := executors.bind("ask", executor); err != nil {
		t.Fatal(err)
	}

	ingestCtx, err := authority.Binder().Bind(context.Background(), askDecision(
		t, now, "ask-ingest",
		auth.OperationIngest, auth.OperationRead, auth.OperationRetrieve))
	if err != nil {
		t.Fatal(err)
	}
	const content = "# Promotion\n\nLocal tables promote under a fenced handoff.\n"
	if _, err := opened.client.Ingest(ingestCtx, explorer.Source{
		URI: "shoal://test/promotion.md", Title: "Promotion",
		MediaType: "text/markdown", Content: content,
	}); err != nil {
		t.Fatalf("ingest: %v", err)
	}

	registerCtx, err := authority.Binder().Bind(context.Background(),
		askDecision(t, now, "ask-register2", auth.OperationAgentRegister))
	if err != nil {
		t.Fatal(err)
	}
	descriptor, err := opened.fleetRegistry.Register(
		registerCtx, fleet.RegisterRequest{
			Context: fleet.RequestContext{
				RequestID: "register", ReasonCode: "test",
				Deadline: now.Add(time.Minute),
			},
			RegistrationKey: "ask-registration",
			Spec:            askAgentSpec(now, chat),
		})
	if err != nil {
		t.Fatalf("register ask agent: %v", err)
	}

	invokeCtx, err := authority.Binder().Bind(context.Background(), askDecision(
		t, now, "ask-invoke2",
		auth.OperationInvoke, auth.OperationRetrieve, auth.OperationRead,
		auth.OperationList, auth.OperationNeighborhood,
		auth.OperationWorkspaceSettingsRead, auth.OperationValidate))
	if err != nil {
		t.Fatal(err)
	}
	record, err := opened.fleetDispatch.Invoke(invokeCtx, fleet.InvokeRequest{
		Enqueue: fleet.EnqueueRequest{
			ID: []byte("ask-evidence"), IdempotencyKey: []byte("ask-evidence-key"),
			AgentID: descriptor.ID, AgentGeneration: descriptor.Generation,
			Capability: webapi.AskCapability, Action: webapi.AskAction,
			SourceID: workspaceSourceID, PolicyID: workspaceGrantPolicyID,
			ObjectID: "corpus",
			// The question shares terms with the ingested source so the
			// deterministic lexical retriever actually grounds it.
			Input: json.RawMessage(`{"question":"fenced handoff promote"}`),
			Context: fleet.RequestContext{
				RequestID: "invoke", CorrelationID: "ask-correlation",
				ReasonCode: "test", Deadline: now.Add(time.Minute),
			},
		},
		ClaimID: []byte("ask-evidence-claim"), Lease: time.Minute,
	})
	if err != nil {
		t.Fatalf("invoke against the real reasoning path: %v", err)
	}
	if record.State != fleet.DispatchSucceeded {
		t.Fatalf("action state = %q, error code = %q", record.State, record.ErrorCode)
	}
	var output webapi.AskExecutionOutput
	if err := json.Unmarshal(record.Output, &output); err != nil {
		t.Fatalf("decode action receipt: %v", err)
	}
	if output.Verification != "verified" || output.SessionID == "" {
		t.Fatalf("receipt = %#v", output)
	}
	if len(record.Evidence) == 0 {
		t.Fatal("a grounded answer must record evidence")
	}
	if record.EvidenceSnapshotID == "" {
		t.Fatal("evidence-carrying commit must pin its snapshot")
	}
	if record.ExecutionFingerprint.String() == "" {
		t.Fatal("evidence-carrying commit must pin its execution fingerprint")
	}
	for _, evidence := range record.Evidence {
		if evidence.AnchorID == "" {
			t.Fatalf("recorded evidence = %#v", record.Evidence)
		}
	}
}
