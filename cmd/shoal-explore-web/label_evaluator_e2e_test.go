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
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/phrocker/shoal-oss/pkg/explorer"
	"github.com/phrocker/shoal-oss/pkg/explorer/auth"
	"github.com/phrocker/shoal-oss/pkg/explorer/authorized"
	"github.com/phrocker/shoal-oss/pkg/explorer/fleet"
	"github.com/phrocker/shoal-oss/pkg/explorer/fleetevents"
	"github.com/phrocker/shoal-oss/pkg/explorer/webapi"
	"github.com/phrocker/shoal-oss/pkg/inference"
	"github.com/phrocker/shoal-oss/pkg/interaction"
	"github.com/phrocker/shoal-oss/pkg/model"
	"github.com/phrocker/shoal-oss/pkg/retrieval"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

// labelHolderDecision is askDecision, holding the workspace source's
// "secret" label policy when holds (#570).
func labelHolderDecision(
	t *testing.T, now time.Time, requestID string, holds bool,
	operations ...auth.Operation,
) auth.Decision {
	t.Helper()
	policies := [][]byte{workspaceGrantPolicyID}
	if holds {
		id, err := authorized.LabelPolicyID(workspaceSourceID, "secret")
		if err != nil {
			t.Fatal(err)
		}
		policies = append(policies, id)
	}
	decision, err := auth.NewDecision(auth.DecisionConfig{
		Subject: "owner", Actor: "operator",
		AuthorizationDomain:   workspaceAuthorizationDomain,
		AllowedOperations:     operations,
		PermittedSourceIDs:    [][]byte{workspaceSourceID},
		PermittedPolicyIDs:    policies,
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

// TestTheHostedServiceRecordsAndDecidesLabelledEvidence is #564 through the
// composition shoal-explore-web serves (openService), with nothing a test
// double but the model: a document labelled "secret" is ingested, a holder
// invokes an ask action grounded in it, and
//
//   - the action's evidence and the chat session's output restriction are
//     recorded with the structured terms of the (source, secret) label
//     policy, not the free-form label;
//   - the holder reads the evidence on Status and on fleet event delivery
//     exactly as stored, and the session exactly as stored;
//   - a reader of the same action without the label sees none of it.
//
// It fails if openService leaves the evaluator or the translator off the
// dispatch plane, or the evaluator off the event plane.
func TestTheHostedServiceRecordsAndDecidesLabelledEvidence(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	now := time.Now().UTC().Add(time.Minute)
	authority := auth.NewAuthority()
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
		t.Fatal("embedded service exposes no authorized client")
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
	bind := func(decision auth.Decision) context.Context {
		t.Helper()
		ctx, err := authority.Binder().Bind(context.Background(), decision)
		if err != nil {
			t.Fatal(err)
		}
		return ctx
	}

	if _, err := opened.client.Ingest(bind(labelHolderDecision(
		t, now, "label-ingest", true,
		auth.OperationIngest, auth.OperationRead, auth.OperationRetrieve,
	)), explorer.Source{
		URI: "shoal://test/closed.md", Title: "Closed",
		MediaType: "text/markdown",
		Content:   "# Promotion\n\nLocal tables promote under a fenced handoff.\n",
		Metadata:  shoal.Metadata{interaction.PropertyVisibility: "secret"},
	}); err != nil {
		t.Fatalf("ingest: %v", err)
	}
	descriptor, err := opened.fleetRegistry.Register(
		bind(labelHolderDecision(t, now, "label-register", false,
			auth.OperationAgentRegister)),
		fleet.RegisterRequest{
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
	readOperations := []auth.Operation{
		auth.OperationInvoke, auth.OperationRetrieve, auth.OperationRead,
		auth.OperationList, auth.OperationNeighborhood,
		auth.OperationWorkspaceSettingsRead, auth.OperationValidate,
		auth.OperationDispatch,
	}
	holder := bind(labelHolderDecision(t, now, "label-invoke", true, readOperations...))
	record, err := opened.fleetDispatch.Invoke(holder, fleet.InvokeRequest{
		Enqueue: fleet.EnqueueRequest{
			ID: []byte("label-evidence"), IdempotencyKey: []byte("label-evidence-key"),
			AgentID: descriptor.ID, AgentGeneration: descriptor.Generation,
			Capability: webapi.AskCapability, Action: webapi.AskAction,
			SourceID: workspaceSourceID, PolicyID: workspaceGrantPolicyID,
			ObjectID: "corpus",
			Input:    json.RawMessage(`{"question":"fenced handoff promote"}`),
			Context: fleet.RequestContext{
				RequestID: "invoke", CorrelationID: "label-correlation",
				ReasonCode: "test", Deadline: now.Add(time.Minute),
			},
		},
		ClaimID: []byte("label-evidence-claim"), Lease: time.Minute,
	})
	if err != nil {
		t.Fatalf("invoke: %v", err)
	}
	if record.State != fleet.DispatchSucceeded || len(record.Evidence) == 0 {
		t.Fatalf("action = %q / %q with %d references", record.State,
			record.ErrorCode, len(record.Evidence))
	}

	// Record time: structured terms only.
	policyID, err := authorized.LabelPolicyID(workspaceSourceID, "secret")
	if err != nil {
		t.Fatal(err)
	}
	policy, err := auth.NewPolicy(auth.PolicyConfig{
		AuthorizationDomain: workspaceAuthorizationDomain,
		SourceID:            workspaceSourceID, GrantPolicyID: policyID,
		Epoch: workspacePolicyGeneration,
	})
	if err != nil {
		t.Fatal(err)
	}
	want, err := policy.VisibilityTerms()
	if err != nil {
		t.Fatal(err)
	}
	want, err = interaction.Conjoin(want)
	if err != nil {
		t.Fatal(err)
	}
	for _, reference := range record.Evidence {
		if !reflect.DeepEqual(reference.Visibility, want) {
			t.Fatalf("evidence recorded with %v, want the label policy's %v",
				reference.Visibility, want)
		}
	}
	var output webapi.AskExecutionOutput
	if err := json.Unmarshal(record.Output, &output); err != nil {
		t.Fatal(err)
	}
	decodedSession, err := base64.RawURLEncoding.DecodeString(output.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	sessionID := shoal.ID(decodedSession)
	session, err := opened.client.Interaction(holder, sessionID)
	if err != nil {
		t.Fatalf("the holder cannot read the chat session: %v", err)
	}
	for _, term := range session.RequiredVisibility {
		if !auth.IsStructuredVisibilityTerm(term) {
			t.Fatalf("the chat session was recorded with a free-form term: %v",
				session.RequiredVisibility)
		}
	}
	if len(session.RequiredVisibility) == 0 {
		t.Fatal("the chat session carries no output restriction; the probe is vacuous")
	}

	// Reads: the holder sees what was stored, the outsider nothing.
	status := func(ctx context.Context) fleet.ActionRecord {
		t.Helper()
		got, err := opened.fleetDispatch.Status(ctx, fleet.StatusRequest{
			ID: record.ID, Context: fleet.RequestContext{
				RequestID: "status", ReasonCode: "test", Deadline: now.Add(time.Minute),
			},
		})
		if err != nil {
			t.Fatal(err)
		}
		return got
	}
	if got := status(holder); !reflect.DeepEqual(got.Evidence, record.Evidence) {
		t.Fatalf("the holder's Status returned %#v, want %#v", got.Evidence, record.Evidence)
	}
	outsider := bind(labelHolderDecision(t, now, "label-outsider", false, readOperations...))
	if got := status(outsider); len(got.Evidence) != 0 {
		t.Fatalf("an outsider's Status returned %#v", got.Evidence)
	}
	if _, err := opened.client.Interaction(outsider, sessionID); err == nil {
		t.Fatal("an outsider read the labelled chat session")
	}

	delivered := func(name string, holds bool) []byte {
		t.Helper()
		ctx := bind(labelHolderDecision(t, now, "subscribe-"+name, holds,
			auth.OperationSubscriptionCreate, auth.OperationAgentResolve))
		subscription, err := opened.fleetEvents.Create(ctx, fleetevents.CreateRequest{
			Token: []byte("subscribe-" + name), RetryUntil: now.Add(time.Hour),
			AgentID: descriptor.ID, AgentGeneration: descriptor.Generation,
			TTL: time.Hour,
		})
		if err != nil {
			t.Fatal(err)
		}
		page, err := opened.fleetEvents.Pull(ctx, fleetevents.PullRequest{
			SubscriptionID: subscription.ID, Limit: fleetevents.MaxPageSize,
		})
		if err != nil {
			t.Fatal(err)
		}
		for _, event := range page.Events {
			if event.Kind == "action.completed" {
				encoded, err := json.Marshal(event)
				if err != nil {
					t.Fatal(err)
				}
				return encoded
			}
		}
		t.Fatal("the completion was not delivered")
		return nil
	}
	anchor := []byte(record.Evidence[0].AnchorID)
	if held := delivered("holder", true); !bytes.Contains(held, anchor) {
		t.Fatalf("a holding subscriber did not receive the evidence: %s", held)
	}
	if outside := delivered("outsider", false); bytes.Contains(outside, anchor) ||
		bytes.Contains(outside, []byte(want[0])) {
		t.Fatalf("an outsider subscriber received the evidence: %s", outside)
	}
}
