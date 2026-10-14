/*
 * Licensed to the Apache Software Foundation (ASF) under one
 * or more contributor license agreements. See the NOTICE file
 * distributed with this work for additional information
 * regarding copyright ownership.
 *
 * Licensed under the Apache License, Version 2.0 (the
 * "License"); you may not use this file except in compliance
 * with the License. You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing,
 * software distributed under the License is distributed on an
 * "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY
 * KIND, either express or implied. See the License for the
 * specific language governing permissions and limitations
 * under the License.
 */

package explorerfleetevents

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/phrocker/shoal-oss/internal/explorercoord"
	"github.com/phrocker/shoal-oss/internal/explorerfleetcap"
	"github.com/phrocker/shoal-oss/pkg/explorer/auth"
	"github.com/phrocker/shoal-oss/pkg/explorer/coordination/transaction"
	"github.com/phrocker/shoal-oss/pkg/explorer/fleet"
	"github.com/phrocker/shoal-oss/pkg/explorer/fleetevents"
	"github.com/phrocker/shoal-oss/pkg/interaction"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

func TestActionEventPublisherReplayAndDivergence(t *testing.T) {
	now := time.Date(2026, 9, 6, 18, 0, 0, 0, time.UTC)
	config := runtimeConfig(t.TempDir())
	runtime, err := explorercoord.Open(config)
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Close()
	backend, err := New(runtime, config.Domain)
	if err != nil {
		t.Fatal(err)
	}
	publisher := dispatchEventPublisher(
		t, backend, now, restartGenerationReader{}, restartAuditor{},
		auth.OperationInvoke)
	record := authorizedActionRecord(fleet.ActionRecord{
		ID: []byte{0, 'a', '|', 'b'}, Version: 7,
		State: fleet.DispatchSucceeded, AgentID: "agent-\x00one", AgentGeneration: 3,
		CorrelationID: "correlation", SourceID: []byte("source"),
		PolicyID: []byte("policy"), ObjectID: "object",
		ExecutorKey: []byte{'e', 0, 'x'},
		Reason:      interaction.Reason{Code: "completed"}, UpdatedAt: now.Add(-time.Minute),
	}, now, auth.OperationInvoke)
	if err := publisher.PublishActionEvent(context.Background(), "action.completed", record); err != nil {
		t.Fatal(err)
	}
	if err := publisher.PublishActionEvent(context.Background(), "action.completed", record); err != nil {
		t.Fatalf("identical replay failed: %v", err)
	}
	events, _, err := backend.Scan(context.Background(), 1, 0, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 {
		t.Fatalf("replay committed %d events, want 1", len(events))
	}
	event := events[0]
	if event.Kind != "action.completed" ||
		!bytes.Equal(event.ProducerID, []byte(record.AgentID)) ||
		event.ProducerGeneration != record.AgentGeneration ||
		!bytes.Equal(event.ActionID, record.ID) ||
		len(event.TransitionID) != sha256.Size ||
		bytes.Equal(event.TransitionID, record.ExecutorKey) ||
		!bytes.Equal(event.CorrelationID, []byte(record.CorrelationID)) ||
		event.Reason != record.Reason || !event.OccurredAt.Equal(record.UpdatedAt) {
		t.Fatalf("mapped event = %#v", event)
	}
	if len(event.Evidence) != 1 ||
		!bytes.Equal(event.Evidence[0].SourceID, record.SourceID) ||
		!bytes.Equal(event.Evidence[0].PolicyID, record.PolicyID) ||
		event.Evidence[0].ObjectID != record.ObjectID {
		t.Fatalf("mapped evidence = %#v", event.Evidence)
	}

	divergent := record
	divergent.Reason = interaction.Reason{Code: "changed"}
	if err := publisher.PublishActionEvent(
		context.Background(), "action.completed", divergent,
	); !errors.Is(err, transaction.ErrConflict) {
		t.Fatalf("divergent replay error = %v, want conflict", err)
	}
	divergent = record
	divergent.ExecutorKey = []byte("changed-transition")
	if err := publisher.PublishActionEvent(
		context.Background(), "action.completed", divergent,
	); !errors.Is(err, transaction.ErrConflict) {
		t.Fatalf("transition replay error = %v, want conflict", err)
	}
	divergent = record
	divergent.AgentGeneration++
	if err := publisher.PublishActionEvent(
		context.Background(), "action.completed", divergent,
	); !errors.Is(err, transaction.ErrConflict) {
		t.Fatalf("generation replay error = %v, want conflict", err)
	}
	record.Version++
	if err := publisher.PublishActionEvent(context.Background(), "action.completed", record); err != nil {
		t.Fatalf("new action version failed: %v", err)
	}
	events, _, err = backend.Scan(context.Background(), 1, 0, 10)
	if err != nil || len(events) != 2 {
		t.Fatalf("versioned events = %d, %v", len(events), err)
	}
}

func TestActionEventPublisherUsesTransitionProvenance(t *testing.T) {
	now := time.Date(2026, 9, 7, 10, 0, 0, 0, time.UTC)
	config := runtimeConfig(t.TempDir())
	runtime, err := explorercoord.Open(config)
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Close()
	backend, err := New(runtime, config.Domain)
	if err != nil {
		t.Fatal(err)
	}
	publisher := dispatchEventPublisher(
		t, backend, now, restartGenerationReader{}, restartAuditor{},
		auth.OperationDispatch)
	record := authorizedActionRecord(fleet.ActionRecord{
		ID: []byte("action"), Version: 2, State: fleet.DispatchCanceled,
		AgentID: "agent", AgentGeneration: 3, CancelKey: []byte("cancel"),
		SourceID: []byte("source"), PolicyID: []byte("policy"), ObjectID: "object",
		UpdatedAt: now.Add(-time.Minute),
	}, now, auth.OperationDispatch)
	record.RequestID = "enqueue-request"
	record.CorrelationID = "enqueue-correlation"
	record.TransitionRequestID = "transition-request"
	record.TransitionCorrelationID = "transition-correlation"
	record.CancelAuthorizationFingerprint = auth.Fingerprint{9}
	record.CancelAuthorizationExpiresAt = now.Add(30 * time.Minute)

	if err := publisher.PublishActionEvent(
		context.Background(), "action.canceled", record); err != nil {
		t.Fatal(err)
	}
	events, _, err := backend.Scan(context.Background(), 1, 0, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 ||
		!bytes.Equal(events[0].CorrelationID, []byte("transition-correlation")) {
		t.Fatalf("event transition provenance = %#v", events)
	}
}

func TestActionEventPublisherAllowsInvokeAuthorizedEnqueue(t *testing.T) {
	now := time.Date(2026, 9, 7, 10, 0, 0, 0, time.UTC)
	config := runtimeConfig(t.TempDir())
	runtime, err := explorercoord.Open(config)
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Close()
	backend, err := New(runtime, config.Domain)
	if err != nil {
		t.Fatal(err)
	}
	publisher := dispatchEventPublisher(
		t, backend, now, restartGenerationReader{}, restartAuditor{},
		auth.OperationInvoke)
	record := authorizedActionRecord(fleet.ActionRecord{
		ID: []byte("action"), IdempotencyKey: []byte("enqueue"), Version: 1,
		State: fleet.DispatchQueued, AgentID: "agent", AgentGeneration: 3,
		SourceID: []byte("source"), PolicyID: []byte("policy"), ObjectID: "object",
		UpdatedAt: now.Add(-time.Minute),
	}, now, auth.OperationInvoke)
	record.AuthorizationFingerprint = auth.Fingerprint{8}
	record.AuthorizationExpiresAt = now.Add(time.Hour)

	if err := publisher.PublishActionEvent(
		context.Background(), "action.enqueued", record); err != nil {
		t.Fatal(err)
	}
}

func TestActionEventPublisherPreservesPublicationAmbiguity(t *testing.T) {
	now := time.Date(2026, 9, 6, 18, 0, 0, 0, time.UTC)
	config := runtimeConfig(t.TempDir())
	runtime, err := explorercoord.Open(config)
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Close()
	backend, err := New(runtime, config.Domain)
	if err != nil {
		t.Fatal(err)
	}
	recordingFailure := errors.New("recording failed")
	publisher := dispatchEventPublisher(
		t, backend, now, restartGenerationReader{},
		failingDispatchAuditor{err: recordingFailure}, auth.OperationInvoke)
	err = publisher.PublishActionEvent(context.Background(), "action.failed", authorizedActionRecord(fleet.ActionRecord{
		ID: []byte("action"), Version: 1, State: fleet.DispatchFailed,
		AgentID: "agent", AgentGeneration: 1, ExecutorKey: []byte("executor"),
		CorrelationID: "correlation", SourceID: []byte("source"),
		PolicyID: []byte("policy"), ObjectID: "object",
		Reason: interaction.Reason{Code: "failed"}, UpdatedAt: now,
	}, now, auth.OperationInvoke))
	if !errors.Is(err, fleetevents.ErrAuditOutcomeUnknown) ||
		!errors.Is(err, recordingFailure) {
		t.Fatalf("publication error = %v", err)
	}
	events, _, scanErr := backend.Scan(context.Background(), 1, 0, 10)
	if scanErr != nil || len(events) != 1 {
		t.Fatalf("committed ambiguous event = %d, %v", len(events), scanErr)
	}
}

func TestActionEventPublisherRetriesIndeterminateReceiptWithoutDuplicate(t *testing.T) {
	now := time.Date(2026, 9, 6, 18, 0, 0, 0, time.UTC)
	config := runtimeConfig(t.TempDir())
	runtime, err := explorercoord.Open(config)
	if err != nil {
		t.Fatal(err)
	}

	defer runtime.Close()
	backend, err := New(runtime, config.Domain)
	if err != nil {
		t.Fatal(err)
	}
	auditor := &failingDispatchAuditor{err: errors.New("recording failed")}
	publisher := dispatchEventPublisher(
		t, backend, now, restartGenerationReader{}, auditor, auth.OperationInvoke)
	record := authorizedActionRecord(fleet.ActionRecord{
		ID: []byte("action"), Version: 1, State: fleet.DispatchSucceeded,
		AgentID: "agent", AgentGeneration: 1, ExecutorKey: []byte("executor"),
		CorrelationID: "correlation", SourceID: []byte("source"),
		PolicyID: []byte("policy"), ObjectID: "object",
		Reason: interaction.Reason{Code: "completed"}, UpdatedAt: now,
	}, now, auth.OperationInvoke)
	if err := publisher.PublishActionEvent(
		context.Background(), "action.completed", record,
	); !errors.Is(err, fleetevents.ErrAuditOutcomeUnknown) {
		t.Fatalf("first publication error = %v", err)
	}
	auditor.err = nil
	if err := publisher.PublishActionEvent(
		context.Background(), "action.completed", record,
	); err != nil {
		t.Fatalf("receipt retry = %v", err)
	}
	events, _, err := backend.Scan(context.Background(), 1, 0, 10)
	if err != nil || len(events) != 1 {
		t.Fatalf("events after receipt retry = %d, %v", len(events), err)
	}
}

func TestActionEventPublisherRetrySurvivesPolicyGenerationChange(t *testing.T) {
	now := time.Date(2026, 9, 6, 18, 0, 0, 0, time.UTC)
	config := runtimeConfig(t.TempDir())
	runtime, err := explorercoord.Open(config)
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Close()
	backend, err := New(runtime, config.Domain)
	if err != nil {
		t.Fatal(err)
	}
	resolver := &mutableDispatchResolver{}
	resolver.set(t, now, 1, auth.OperationInvoke)
	generations := &mutableDispatchGeneration{generation: 1}
	var receipts []fleetevents.AuditRecord
	auditor := &failingDispatchAuditor{
		err: errors.New("recording failed"), records: &receipts,
	}
	capability := explorerfleetcap.New()
	service, err := fleetevents.NewWithLifecycleCapability(fleetevents.Config{
		Backend: backend, Resolver: resolver, GenerationReader: generations,
		LeaseValidator: dispatchLeaseValidator{}, Auditor: auditor,
		CursorKey: bytes.Repeat([]byte{7}, 32),
		Clock:     func() time.Time { return now },
	}, capability)
	if err != nil {
		t.Fatal(err)
	}
	publisher, err := NewActionEventPublisher(
		service, resolver, capability, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	record := authorizedActionRecord(fleet.ActionRecord{
		ID: []byte("action"), Version: 9, State: fleet.DispatchSucceeded,
		AgentID: "agent", AgentGeneration: 4, ExecutorKey: []byte("executor"),
		SourceID: []byte("source"), PolicyID: []byte("policy"), ObjectID: "object",
		Reason: interaction.Reason{Code: "completed"}, UpdatedAt: now,
	}, now, auth.OperationInvoke)
	if err := publisher.PublishActionEvent(
		context.Background(), "action.completed", record,
	); !errors.Is(err, fleetevents.ErrAuditOutcomeUnknown) {
		t.Fatalf("first publication = %v", err)
	}
	resolver.set(t, now, 2, auth.OperationInvoke)
	generations.mu.Lock()
	generations.generation = 2
	generations.mu.Unlock()
	auditor.err = nil
	if err := publisher.PublishActionEvent(
		context.Background(), "action.completed", record,
	); err != nil {
		t.Fatalf("policy-changed retry = %v", err)
	}
	events, _, err := backend.Scan(context.Background(), 1, 0, 10)
	if err != nil || len(events) != 1 {
		t.Fatalf("policy retry events = %d, %v", len(events), err)
	}
	if len(receipts) != 2 ||
		receipts[0].AuthorizationFingerprint == (auth.Fingerprint{}) ||
		receipts[1].AuthorizationFingerprint == (auth.Fingerprint{}) {
		t.Fatalf("lifecycle receipt pins = %#v", receipts)
	}
}

func TestActionEventPublisherPreservesExecutorEvidence(t *testing.T) {
	now := time.Date(2026, 9, 6, 18, 0, 0, 0, time.UTC)
	backend := &recordingBackend{}
	publisher := dispatchEventPublisher(
		t, backend, now, restartGenerationReader{}, restartAuditor{},
		auth.OperationInvoke)
	record := authorizedActionRecord(fleet.ActionRecord{
		ID: []byte("action"), Version: 2, State: fleet.DispatchSucceeded,
		AgentID: "agent", AgentGeneration: 1, ExecutorKey: []byte("executor"),
		SourceID: []byte("source"), PolicyID: []byte("policy"), ObjectID: "object",
		Reason: interaction.Reason{Code: "completed"}, UpdatedAt: now,
		Evidence: []fleet.EvidenceRef{{
			Kind:       interaction.EvidenceGraph,
			NodeIDs:    []shoal.ID{"node", "node"},
			EdgeIDs:    []shoal.ID{"edge"},
			AnchorID:   "anchor",
			Visibility: []string{"A", "B"},
		}},
	}, now, auth.OperationInvoke)
	if err := publisher.PublishActionEvent(
		context.Background(), "action.completed", record,
	); err != nil {
		t.Fatal(err)
	}
	if backend.appends != 1 || len(backend.request.Event.Evidence) != 2 ||
		len(backend.request.Event.ConsumedEvidence) != 1 {
		t.Fatalf("preserved evidence = %#v / %#v",
			backend.request.Event.Evidence, backend.request.Event.ConsumedEvidence)
	}
	evidence := backend.request.Event.ConsumedEvidence[0]
	if !reflect.DeepEqual(evidence.NodeIDs, []shoal.ID{"node", "node"}) ||
		!reflect.DeepEqual(evidence.EdgeIDs, []shoal.ID{"edge"}) ||
		evidence.AnchorID != "anchor" ||
		evidence.Kind != interaction.EvidenceGraph {
		t.Fatalf("executor evidence = %#v", evidence)
	}
	referenceCount := 0
	for _, item := range backend.request.Event.Evidence {
		if item.Reference != nil {
			referenceCount++
			if !reflect.DeepEqual(*item.Reference, evidence) {
				t.Fatalf("authorization evidence reference = %#v", item.Reference)
			}
		}
	}
	if referenceCount != 1 {
		t.Fatalf("canonical authorization references = %d, want 1", referenceCount)
	}
	record.Evidence[0].NodeIDs[0] = "changed"
	if evidence.NodeIDs[0] != "node" {
		t.Fatal("event evidence aliases the action record")
	}
}

func TestActionEventPublisherBoundsAuthorizationEvidenceByReferences(t *testing.T) {
	now := time.Date(2026, 9, 7, 3, 40, 0, 0, time.UTC)
	backend := &recordingBackend{}
	publisher := dispatchEventPublisher(
		t, backend, now, restartGenerationReader{}, restartAuditor{},
		auth.OperationInvoke)
	record := authorizedActionRecord(fleet.ActionRecord{
		ID: []byte("action"), Version: 2, State: fleet.DispatchSucceeded,
		AgentID: "agent", AgentGeneration: 1, ExecutorKey: []byte("executor"),
		SourceID: []byte("source"), PolicyID: []byte("policy"), ObjectID: "object",
		Reason: interaction.Reason{Code: "completed"}, UpdatedAt: now,
	}, now, auth.OperationInvoke)
	record.Evidence = make([]fleet.EvidenceRef, 129)
	for index := range record.Evidence {
		record.Evidence[index] = fleet.EvidenceRef{
			AnchorID: shoal.ID(fmt.Sprintf("anchor-%03d", index)),
			Kind:     interaction.EvidenceGraph,
			NodeIDs:  []shoal.ID{shoal.ID(fmt.Sprintf("node-%03d", index))},
		}
	}
	if err := publisher.PublishActionEvent(
		context.Background(), "action.completed", record,
	); err != nil {
		t.Fatal(err)
	}
	evidence := backend.request.Event.Evidence
	if len(evidence) != len(record.Evidence)+1 {
		t.Fatalf("authorization evidence = %d, want %d",
			len(evidence), len(record.Evidence)+1)
	}
}

func TestActionEventPublisherPreservesCommittedAuthorizationFailure(t *testing.T) {
	now := time.Date(2026, 9, 6, 18, 0, 0, 0, time.UTC)
	config := runtimeConfig(t.TempDir())
	runtime, err := explorercoord.Open(config)
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Close()
	backend, err := New(runtime, config.Domain)
	if err != nil {
		t.Fatal(err)
	}
	publisher := dispatchEventPublisher(
		t, backend, now, new(changingDispatchGeneration), restartAuditor{},
		auth.OperationDispatch)
	err = publisher.PublishActionEvent(context.Background(), "action.canceled", authorizedActionRecord(fleet.ActionRecord{
		ID: []byte("action"), Version: 1, State: fleet.DispatchCanceled,
		AgentID: "agent", AgentGeneration: 1, CancelKey: []byte("cancel"),
		CorrelationID: "correlation", SourceID: []byte("source"),
		PolicyID: []byte("policy"), ObjectID: "object",
		Reason: interaction.Reason{Code: "canceled"}, UpdatedAt: now,
	}, now, auth.OperationDispatch))
	if !errors.Is(err, fleetevents.ErrActionCommitted) {
		t.Fatalf("authorization race error = %v", err)
	}
	events, _, scanErr := backend.Scan(context.Background(), 1, 0, 10)
	if scanErr != nil || len(events) != 1 {
		t.Fatalf("committed event after authorization race = %d, %v", len(events), scanErr)
	}
}

func TestActionEventTokenIsLengthFramed(t *testing.T) {
	baseRecord := fleet.ActionRecord{
		ID: []byte("action"), Version: 4, State: fleet.DispatchSucceeded,
		AgentID: "agent", AgentGeneration: 7, ExecutorKey: []byte("transition"),
	}
	base := mustActionEventToken(t, "action.completed", baseRecord)
	changedKind := baseRecord
	changedKind.State = fleet.DispatchFailed
	changedAction := baseRecord
	changedAction.ID = []byte("different")
	changedAgent := baseRecord
	changedAgent.AgentID = "other"
	changedGeneration := baseRecord
	changedGeneration.AgentGeneration++
	changedTransition := baseRecord
	changedTransition.ExecutorKey = []byte("different")
	changedVersion := baseRecord
	changedVersion.Version++
	for name, token := range map[string][]byte{
		"kind":    mustActionEventToken(t, "action.failed", changedKind),
		"action":  mustActionEventToken(t, "action.completed", changedAction),
		"version": mustActionEventToken(t, "action.completed", changedVersion),
	} {
		if bytes.Equal(base, token) {
			t.Fatalf("different %s produced the same token", name)
		}
	}
	for name, token := range map[string][]byte{
		"agent":      mustActionEventToken(t, "action.completed", changedAgent),
		"generation": mustActionEventToken(t, "action.completed", changedGeneration),
		"transition": mustActionEventToken(t, "action.completed", changedTransition),
	} {
		if !bytes.Equal(base, token) {
			t.Fatalf("different %s changed the stable publication token", name)
		}
	}
	left := baseRecord
	left.ID = []byte{'a', 0, 'b'}
	left.ExecutorKey = []byte("c")
	right := baseRecord
	right.ID = []byte("a")
	right.ExecutorKey = []byte{'b', 0, 'c'}
	if bytes.Equal(
		mustActionEventToken(t, "action.completed", left),
		mustActionEventToken(t, "action.completed", right),
	) {
		t.Fatal("ambiguous fields produced the same token")
	}
}

func TestActionEventTokenUsesCanonicalTransitionIdentity(t *testing.T) {
	tests := []struct {
		kind      string
		state     fleet.DispatchState
		configure func(*fleet.ActionRecord)
	}{
		{"action.enqueued", fleet.DispatchQueued, func(record *fleet.ActionRecord) {
			record.IdempotencyKey = []byte("enqueue")
		}},
		{"action.claimed", fleet.DispatchClaimed, func(record *fleet.ActionRecord) {
			record.ClaimID = []byte("claim")
		}},
		{"action.completed", fleet.DispatchSucceeded, func(record *fleet.ActionRecord) {
			record.ExecutorKey = []byte("execute")
		}},
		{"action.failed", fleet.DispatchFailed, func(record *fleet.ActionRecord) {
			record.ExecutorKey = []byte("execute")
		}},
		{"action.canceled", fleet.DispatchCanceled, func(record *fleet.ActionRecord) {
			record.CancelKey = []byte("cancel")
		}},
	}
	for _, test := range tests {
		t.Run(test.kind, func(t *testing.T) {
			record := fleet.ActionRecord{
				ID: []byte("action"), Version: 2, State: test.state,
				AgentID: "agent", AgentGeneration: 3,
			}
			test.configure(&record)
			originalToken, originalTransition, err := actionEventIdentities(test.kind, record)
			if err != nil {
				t.Fatal(err)
			}
			changed := record
			switch test.kind {
			case "action.enqueued":
				changed.IdempotencyKey = []byte("different")
			case "action.claimed":
				changed.ClaimID = []byte("different")
			case "action.completed", "action.failed":
				changed.ExecutorKey = []byte("different")
			case "action.canceled":
				changed.CancelKey = []byte("different")
			}
			changedToken, changedTransition, err := actionEventIdentities(test.kind, changed)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(originalToken, changedToken) ||
				bytes.Equal(originalTransition, changedTransition) ||
				len(originalTransition) != sha256.Size {
				t.Fatal("transition discriminator did not alter only the transition ID")
			}
			record.AgentGeneration++
			generationToken, generationTransition, err := actionEventIdentities(test.kind, record)
			if err != nil {
				t.Fatal(err)
			}
			baseToken, baseTransition, err := actionEventIdentities(test.kind, fleet.ActionRecord{
				ID: []byte("action"), Version: 2, State: test.state,
				AgentID: "agent", AgentGeneration: 3,
				IdempotencyKey: record.IdempotencyKey,
				ClaimID:        record.ClaimID, CancelKey: record.CancelKey,
				ExecutorKey: record.ExecutorKey,
			})
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(generationToken, baseToken) ||
				!bytes.Equal(generationTransition, baseTransition) {
				t.Fatal("agent generation altered token or transition ID")
			}
			record.AgentGeneration = 3
			record.AgentID = "different"
			agentToken, agentTransition, err := actionEventIdentities(test.kind, record)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(agentToken, baseToken) ||
				!bytes.Equal(agentTransition, baseTransition) {
				t.Fatal("agent identity altered token or transition ID")
			}
		})
	}
}

func TestActionTransitionIDUsesOpaqueLengthFraming(t *testing.T) {
	left := fleet.ActionRecord{
		ID: []byte{'a', 0, 'b'}, Version: 1, State: fleet.DispatchSucceeded,
		AgentID:         shoal.ID(string([]byte{'x', 0xff, 0, 'y'})),
		AgentGeneration: 7, ExecutorKey: []byte("c"),
	}
	right := fleet.ActionRecord{
		ID: []byte("a"), Version: 1, State: fleet.DispatchSucceeded,
		AgentID:         shoal.ID(string([]byte{'x', 0xff})),
		AgentGeneration: 7, ExecutorKey: []byte{'b', 0, 'c'},
	}
	leftToken, leftTransition, err := actionEventIdentities("action.completed", left)
	if err != nil {
		t.Fatal(err)
	}
	rightToken, rightTransition, err := actionEventIdentities("action.completed", right)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(leftToken, rightToken) {
		t.Fatal("length-framed action identities collided")
	}
	if bytes.Equal(leftTransition, rightTransition) {
		t.Fatal("length-framed opaque transition identities collided")
	}
	if bytes.Equal(leftTransition, left.ExecutorKey) ||
		bytes.Equal(rightTransition, right.ExecutorKey) {
		t.Fatal("transition ID exposed a raw dispatch discriminator")
	}
}

func TestActionEventTokenRejectsMismatchedOrMissingTransition(t *testing.T) {
	record := fleet.ActionRecord{
		ID: []byte("action"), Version: 1, State: fleet.DispatchSucceeded,
		AgentID: "agent", AgentGeneration: 1, ExecutorKey: []byte("executor"),
	}
	if _, err := actionEventToken("action.failed", record); err == nil {
		t.Fatal("mismatched event kind succeeded")
	}
	record.ExecutorKey = nil
	if _, err := actionEventToken("action.completed", record); err == nil {
		t.Fatal("missing transition identity succeeded")
	}
	if _, err := actionEventToken("action.unknown", record); err == nil {
		t.Fatal("unknown event kind succeeded")
	}
}

func TestNewActionEventPublisherRejectsMissingDependencies(t *testing.T) {
	if _, err := NewActionEventPublisher(
		nil, nil, explorerfleetcap.Capability{}, nil,
	); err == nil {
		t.Fatal("missing dependencies succeeded")
	}
}

func mustActionEventToken(
	t *testing.T, kind string, record fleet.ActionRecord,
) []byte {
	t.Helper()
	token, err := actionEventToken(kind, record)
	if err != nil {
		t.Fatal(err)
	}
	return token
}

func dispatchEventPublisher(
	t *testing.T, backend fleetevents.Backend, now time.Time,
	generations auth.GenerationReader, auditor fleetevents.Auditor,
	operation auth.Operation,
) *ActionEventPublisher {
	t.Helper()
	decision, err := auth.NewDecision(auth.DecisionConfig{
		Subject: "dispatcher", Actor: "dispatcher", RequestID: "request",
		CorrelationID:       "correlation",
		AuthorizationDomain: []byte("domain"),
		AllowedOperations:   []auth.Operation{operation},
		PermittedSourceIDs:  [][]byte{[]byte("source")},
		PermittedPolicyIDs:  [][]byte{[]byte("policy")},
		PolicyGeneration:    1, AuthenticationExpires: now.Add(time.Hour),
	})
	if err != nil {
		t.Fatal(err)
	}
	resolver, err := auth.NewStaticResolverWithClock(decision, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	capability := explorerfleetcap.New()
	service, err := fleetevents.NewWithLifecycleCapability(fleetevents.Config{
		Backend: backend, Resolver: resolver, GenerationReader: generations,
		LeaseValidator: dispatchLeaseValidator{}, Auditor: auditor,
		CursorKey: bytes.Repeat([]byte{7}, 32),
		Clock:     func() time.Time { return now },
	}, capability)
	if err != nil {
		t.Fatal(err)
	}
	publisher, err := NewActionEventPublisher(
		service, resolver, capability, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	return publisher
}

type dispatchLeaseValidator struct{}

func (dispatchLeaseValidator) ValidateDelivery(
	context.Context, shoal.ID, int64,
) error {
	return nil
}

func authorizedActionRecord(
	record fleet.ActionRecord, now time.Time, operation auth.Operation,
) fleet.ActionRecord {
	record.Subject = "dispatcher"
	record.Actor = "dispatcher"
	record.RequestID = "request"
	record.CorrelationID = "correlation"
	record.AuthorizedOperations = []auth.Operation{operation}
	if operation == auth.OperationInvoke {
		record.ExecutionFingerprint = auth.Fingerprint{2}
		record.ExecutionPolicyGeneration = 1
		record.ExecutionExpiresAt = now.Add(time.Hour)
	} else {
		record.AuthorizationFingerprint = auth.Fingerprint{1}
		record.PolicyGeneration = 1
		record.AuthorizationExpiresAt = now.Add(time.Hour)
	}
	return record
}

type failingDispatchAuditor struct {
	err     error
	records *[]fleetevents.AuditRecord
}

func (a failingDispatchAuditor) RecordFleetAction(
	_ context.Context, record fleetevents.AuditRecord,
) error {
	if a.records != nil {
		*a.records = append(*a.records, record)
	}
	return a.err
}

type changingDispatchGeneration struct {
	calls atomic.Int64
}

type mutableDispatchGeneration struct {
	mu         sync.Mutex
	generation int64
}

func (g *mutableDispatchGeneration) CurrentPolicyGeneration(
	context.Context, []byte,
) (int64, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.generation, nil
}

type mutableDispatchResolver struct {
	mu       sync.Mutex
	decision auth.Decision
}

func (r *mutableDispatchResolver) Resolve(context.Context) (auth.Decision, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.decision, nil
}

func (r *mutableDispatchResolver) set(
	t *testing.T, now time.Time, generation int64, operation auth.Operation,
) {
	t.Helper()
	decision, err := auth.NewDecision(auth.DecisionConfig{
		Subject: "dispatcher", Actor: "dispatcher", RequestID: "request",
		CorrelationID: "correlation", AuthorizationDomain: []byte("domain"),
		AllowedOperations:  []auth.Operation{operation},
		PermittedSourceIDs: [][]byte{[]byte("source")},
		PermittedPolicyIDs: [][]byte{[]byte("policy")},
		PolicyGeneration:   generation, AuthenticationExpires: now.Add(time.Hour),
	})
	if err != nil {
		t.Fatal(err)
	}
	r.mu.Lock()
	r.decision = decision
	r.mu.Unlock()
}

type recordingBackend struct {
	appends int
	request fleetevents.PublishRequest
}

func (*recordingBackend) Create(
	context.Context, fleetevents.CreateRequest, auth.Fingerprint, int64, time.Time,
) (fleetevents.Subscription, bool, error) {
	return fleetevents.Subscription{}, false, errors.New("unexpected create")
}
func (*recordingBackend) Subscription(
	context.Context, []byte,
) (fleetevents.Subscription, error) {
	return fleetevents.Subscription{}, errors.New("unexpected subscription")
}
func (*recordingBackend) Delete(
	context.Context, []byte, shoal.ID, uint64, time.Time, time.Time,
) (fleetevents.Subscription, bool, error) {
	return fleetevents.Subscription{}, false, errors.New("unexpected delete")
}
func (b *recordingBackend) Append(
	_ context.Context, request fleetevents.PublishRequest, _ time.Time,
) (fleetevents.PublishResult, error) {
	b.appends++
	b.request = request
	return fleetevents.PublishResult{Audit: request.Audit}, nil
}
func (*recordingBackend) CurrentStart(context.Context) (uint64, error) {
	return 1, nil
}
func (*recordingBackend) Scan(
	context.Context, uint64, uint64, int,
) ([]fleetevents.Event, uint64, error) {
	return nil, 0, errors.New("unexpected scan")
}

func (g *changingDispatchGeneration) CurrentPolicyGeneration(
	context.Context, []byte,
) (int64, error) {
	if g.calls.Add(1) == 1 {
		return 1, nil
	}
	return 2, nil
}

// TestActionEventPublisherAllowsAForeignClaimant is the test whose absence let
// #437's capability ship not working.
//
// The publisher compared the publishing decision against the record's own
// principal for every event kind. applyClaim deliberately leaves that principal
// alone — a record claimed by a worker still names its enqueuer — so a worker's
// claim and completion both failed to publish. A failed publication is returned
// as ErrActionCommitted, which the HTTP layer maps to 503 "fleet action outcome
// requires reconciliation". The transition was durably written and the worker
// was told to reconcile an outcome that had in fact been recorded.
//
// Nothing in pkg/explorer/fleet could see it: those tests bind a no-op event
// sink, so the publisher they exercise is not this one. That is why this test
// lives here, beside the real publisher, rather than there.
func TestActionEventPublisherAllowsAForeignClaimant(t *testing.T) {
	now := time.Date(2026, 9, 7, 10, 0, 0, 0, time.UTC)
	config := runtimeConfig(t.TempDir())
	runtime, err := explorercoord.Open(config)
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Close()
	backend, err := New(runtime, config.Domain)
	if err != nil {
		t.Fatal(err)
	}
	// The publishing identity holds execute and not invoke, as a gateway
	// worker's does.
	publisher := dispatchEventPublisher(
		t, backend, now, restartGenerationReader{}, restartAuditor{},
		auth.OperationExecute)

	for _, kind := range []string{
		"action.claimed", "action.completed", "action.failed",
	} {
		t.Run(kind, func(t *testing.T) {
			record := foreignClaimantRecord(t, now, kind)
			if err := publisher.PublishActionEvent(
				context.Background(), kind, record); err != nil {
				t.Fatalf("a worker that claimed work it did not enqueue cannot "+
					"publish %s, so its claim returns 503 and the write it "+
					"already made is reported as needing reconciliation: %v",
					kind, err)
			}
		})
	}
}

// TestActionEventPublisherRefusesANonClaimant is the other half: widening the
// identity rule must not make it vacuous.
//
// The enqueuer is the right publisher for an enqueue and a cancellation and the
// wrong one for a claim, because it did not perform that transition. Checked
// explicitly, since the obvious way to make the test above pass is to compare
// against both chains and accept either.
func TestActionEventPublisherRefusesANonClaimant(t *testing.T) {
	now := time.Date(2026, 9, 7, 10, 0, 0, 0, time.UTC)
	config := runtimeConfig(t.TempDir())
	runtime, err := explorercoord.Open(config)
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Close()
	backend, err := New(runtime, config.Domain)
	if err != nil {
		t.Fatal(err)
	}
	// "dispatcher" is the publishing identity this helper mints. Below it is
	// the record's enqueuer rather than its claimant.
	publisher := dispatchEventPublisher(
		t, backend, now, restartGenerationReader{}, restartAuditor{},
		auth.OperationInvoke)

	record := foreignClaimantRecord(t, now, "action.claimed")
	record.Subject = "dispatcher"
	record.Actor = "dispatcher"
	record.ClaimantSubject = "worker-subject"
	record.ClaimantActor = "worker-actor"
	// Left as execute, matching AuthorizedOperations, so the refusal below can
	// only come from the identity rule — the publishing decision here holds
	// invoke, so naming invoke would refuse on provenance instead and the test
	// would pass without exercising anything.
	record.TransitionOperation = auth.OperationExecute

	err = publisher.PublishActionEvent(
		context.Background(), "action.claimed", record)
	if err == nil {
		t.Fatal("the record's enqueuer published a claim held by another " +
			"principal, so the event attributes a transition to an identity " +
			"that did not perform it")
	}
	if !strings.Contains(err.Error(), "does not match durable transition") {
		t.Fatalf("refused for an unrelated reason, so this case does not "+
			"exercise the identity rule: %v", err)
	}
}

// TestActionEventPublisherStillRequiresTheEnqueuerForCancellation pins the kinds
// the widening must not reach. A cancellation is the enqueuer's lever and no
// execute-holder can reach Cancel at all.
func TestActionEventPublisherStillRequiresTheEnqueuerForCancellation(t *testing.T) {
	now := time.Date(2026, 9, 7, 10, 0, 0, 0, time.UTC)
	config := runtimeConfig(t.TempDir())
	runtime, err := explorercoord.Open(config)
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Close()
	backend, err := New(runtime, config.Domain)
	if err != nil {
		t.Fatal(err)
	}
	publisher := dispatchEventPublisher(
		t, backend, now, restartGenerationReader{}, restartAuditor{},
		auth.OperationDispatch)

	// A claimant chain on the record must not let a claimant publish a
	// cancellation it did not perform.
	record := authorizedActionRecord(fleet.ActionRecord{
		ID: []byte("action"), IdempotencyKey: []byte("k"), Version: 3,
		State: fleet.DispatchCanceled, AgentID: "agent", AgentGeneration: 3,
		SourceID: []byte("source"), PolicyID: []byte("policy"), ObjectID: "object",
		UpdatedAt: now.Add(-time.Minute), ClaimID: []byte("worker-claim"),
		ClaimFence: 1,
		// action.canceled carries the cancel key as its transition identity.
		// Without one, actionEventTransition refuses before the identity rule
		// is consulted at all — which is how this test came to pass against
		// the very regression it is named for.
		CancelKey: []byte("cancel-key"),
	}, now, auth.OperationDispatch)
	record.Subject = "enqueuer-subject"
	record.Actor = "enqueuer-actor"
	record.ClaimantSubject = "dispatcher"
	record.ClaimantActor = "dispatcher"
	record.TransitionOperation = auth.OperationDispatch

	err = publisher.PublishActionEvent(
		context.Background(), "action.canceled", record)
	if err == nil {
		t.Fatal("a claimant published a cancellation, so the claimant rule " +
			"reaches a kind the claimant never performs")
	}
	// Asserted on the message, like the sibling test, because the refusal has
	// to come from the identity rule. Any earlier refusal — a missing
	// transition identity, an operation the record does not carry — would make
	// this pass while the rule itself was wrong.
	if !strings.Contains(err.Error(), "does not match durable transition") {
		t.Fatalf("refused before the identity rule, so this test does not "+
			"exercise what it is named for: %v", err)
	}
}

// foreignClaimantRecord builds a record whose enqueuer and claimant are
// different principals, with the claimant being the identity
// dispatchEventPublisher mints.
func foreignClaimantRecord(
	t *testing.T, now time.Time, kind string,
) fleet.ActionRecord {
	t.Helper()
	state := fleet.DispatchClaimed
	switch kind {
	case "action.completed":
		state = fleet.DispatchSucceeded
	case "action.failed":
		state = fleet.DispatchFailed
	}
	record := authorizedActionRecord(fleet.ActionRecord{
		ID: []byte("action"), IdempotencyKey: []byte("k"), Version: 2,
		State: state, AgentID: "agent", AgentGeneration: 3,
		SourceID: []byte("source"), PolicyID: []byte("policy"), ObjectID: "object",
		UpdatedAt: now.Add(-time.Minute), ClaimID: []byte("worker-claim"),
		ClaimFence: 1,
	}, now, auth.OperationExecute)
	if state == fleet.DispatchFailed {
		record.ErrorCode = "target_unreachable"
	}
	// The enqueuer is someone else entirely; the claimant is the publisher.
	record.Subject = "enqueuer-subject"
	record.Actor = "enqueuer-actor"
	record.ClaimantSubject = "dispatcher"
	record.ClaimantActor = "dispatcher"
	record.TransitionOperation = auth.OperationExecute
	// action.completed and action.failed carry the executor key as their
	// transition identity, so a record without one is refused before the
	// identity rule is reached.
	record.ExecutorKey = []byte("executor-key")
	record.ExecutionFingerprint = auth.Fingerprint{2}
	record.ExecutionPolicyGeneration = 1
	record.ExecutionExpiresAt = now.Add(time.Hour)
	return record
}

// TestCanceledPublicationNamesTheAuthorizingOperation pins the operation this
// kind publishes under, for every record shape that reaches it — including the
// two written by builds that predate the fix.
//
// The arm hardcoded dispatch, which refused every admission denial's
// publication because a denial's AuthorizedOperations is [invoke]. The obvious
// fix is to read record.TransitionOperation, and it is wrong in both
// directions, which is why this table exists rather than a comment:
//
//   - cloneActionRecord carries that field forward, so a record cancelled
//     while claimed holds the *claim's* operation. Reading it publishes such a
//     record under invoke, which the canceller — who needs only dispatch —
//     cannot authorize, turning a committed cancel into a 503.
//   - a denial written before deny set the field has it empty, so any constant
//     fallback outside its [invoke] set leaves it permanently unpublishable
//     with an undrainable outbox row.
//
// AuthorizedOperations records what actually authorized the transition: Cancel
// widens it with dispatch, and an admission denial carries [invoke] with
// nothing widening it.
func TestCanceledPublicationNamesTheAuthorizingOperation(t *testing.T) {
	for _, probe := range []struct {
		name string
		// field is what TransitionOperation holds, which this arm must ignore.
		field   auth.Operation
		authOps []auth.Operation
		want    auth.Operation
	}{
		{
			name:  "a cancel written by this build",
			field: auth.OperationDispatch,
			authOps: []auth.Operation{
				auth.OperationInvoke, auth.OperationDispatch},
			want: auth.OperationDispatch,
		},
		{
			name:    "an admission denial written by this build",
			field:   auth.OperationInvoke,
			authOps: []auth.Operation{auth.OperationInvoke},
			want:    auth.OperationInvoke,
		},
		{
			// The field says invoke because the claim did. Publishing under
			// invoke here is the 503 this arm was changed to stop producing.
			name:  "a pre-fix cancel of a record claimed under invoke",
			field: auth.OperationInvoke,
			authOps: []auth.Operation{
				auth.OperationInvoke, auth.OperationDispatch},
			want: auth.OperationDispatch,
		},
		{
			name:  "a pre-fix cancel of a record claimed under execute",
			field: auth.OperationExecute,
			authOps: []auth.Operation{
				auth.OperationExecute, auth.OperationDispatch},
			want: auth.OperationDispatch,
		},
		{
			// Unpublishable before this: the field is empty and dispatch is
			// not in the set, so the provenance check refused it outright.
			name:    "a pre-fix admission denial with no recorded operation",
			field:   "",
			authOps: []auth.Operation{auth.OperationInvoke},
			want:    auth.OperationInvoke,
		},
	} {
		t.Run(probe.name, func(t *testing.T) {
			record := fleet.ActionRecord{
				AuthorizationFingerprint: auth.Fingerprint{1},
				AuthorizationExpiresAt: time.Date(
					2030, 1, 1, 0, 0, 0, 0, time.UTC),
				TransitionOperation:  probe.field,
				AuthorizedOperations: probe.authOps,
			}
			operation, _, _, err := actionEventAuthorization(
				"action.canceled", record)
			if err != nil {
				t.Fatalf("publication refused, so this record's lifecycle "+
					"event is lost and its outbox row undrainable: %v", err)
			}
			if operation != probe.want {
				t.Fatalf("operation = %q, want %q: publishing under an "+
					"operation the canceller does not hold fails "+
					"authorization and returns ErrActionCommitted for a "+
					"transition that committed", operation, probe.want)
			}
		})
	}
}
