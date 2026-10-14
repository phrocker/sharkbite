// Licensed to the Apache Software Foundation (ASF) under one or more
// contributor license agreements. See the NOTICE file distributed with this
// work for additional information regarding copyright ownership.
package explorerfleetevents

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/phrocker/shoal-oss/internal/explorercoord"
	"github.com/phrocker/shoal-oss/internal/explorerfleet"
	"github.com/phrocker/shoal-oss/internal/explorerfleetcap"
	"github.com/phrocker/shoal-oss/pkg/explorer"
	"github.com/phrocker/shoal-oss/pkg/explorer/auth"
	"github.com/phrocker/shoal-oss/pkg/explorer/fleet"
	"github.com/phrocker/shoal-oss/pkg/explorer/fleetevents"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

func TestRealDispatchPublishesAllLifecycleEventsIdempotently(t *testing.T) {
	now := time.Date(2026, 9, 6, 9, 0, 0, 0, time.UTC)
	config := runtimeConfig(t.TempDir())
	config = explorerfleet.ConfigureRuntime(config)
	ConfigureRuntime(&config)
	runtime, err := explorercoord.Open(config)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if runtime != nil {
			_ = runtime.Close()
		}
	}()

	authority, err := auth.NewAuthorityWithClock(func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	decision, err := auth.NewDecision(auth.DecisionConfig{
		Subject: "owner", Actor: "actor", ClientID: "client",
		AuthorizationDomain: []byte("domain"),
		AllowedOperations: []auth.Operation{
			auth.OperationDispatch, auth.OperationInvoke,
		},
		PermittedSourceIDs: [][]byte{[]byte("source")},
		PermittedPolicyIDs: [][]byte{[]byte("policy")},
		PolicyGeneration:   1, AuthenticationExpires: now.Add(2 * time.Hour),
		RequestID: "request", CorrelationID: "correlation",
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, err := authority.Binder().Bind(context.Background(), decision)
	if err != nil {
		t.Fatal(err)
	}

	successExecutor := &integrationExecutor{
		result: fleet.ExecutionResult{Output: json.RawMessage(`{"ok":true}`)},
	}
	registry, executors := newIntegrationRegistry(
		t, authority.Resolver(), now, successExecutor)
	eventBackend, err := New(runtime, config.Domain)
	if err != nil {
		t.Fatal(err)
	}
	capability := explorerfleetcap.New()
	eventService, err := fleetevents.NewWithLifecycleCapability(fleetevents.Config{
		Backend: eventBackend, Resolver: authority.Resolver(),
		GenerationReader: integrationGeneration{}, LeaseValidator: integrationLease{},
		Auditor: integrationAuditor{}, CursorKey: bytes.Repeat([]byte{7}, 32),
		Clock: func() time.Time { return now },
	}, capability)
	if err != nil {
		t.Fatal(err)
	}
	publisher, err := NewActionEventPublisher(
		eventService, authority.Resolver(), capability,
		func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	dispatch, err := explorerfleet.ComposeDispatch(
		runtime, registry, authority.Resolver(), integrationActionRecorder{},
		publisher, nil, func() time.Time { return now },
	)
	if err != nil {
		t.Fatal(err)
	}

	completedRequest := integrationEnqueueRequest(now, "completed", "enqueue-completed")
	queued, err := dispatch.Enqueue(ctx, completedRequest)
	if err != nil {
		t.Fatal(err)
	}
	if replayed, replayErr := dispatch.Enqueue(ctx, completedRequest); replayErr != nil ||
		replayed.Version != queued.Version {
		t.Fatalf("enqueue replay = %#v, %v", replayed, replayErr)
	}
	claimed, err := dispatch.Claim(ctx, fleet.ClaimRequest{
		ID: queued.ID, ExpectedVersion: queued.Version,
		ClaimID: []byte("claim-completed"), Lease: time.Minute,
		Context: integrationRequestContext(now),
	})
	if err != nil {
		t.Fatal(err)
	}
	if claimed.ClaimLease != time.Minute ||
		!claimed.ClaimLeaseUntil.Equal(now.Add(time.Minute)) {
		t.Fatalf("claim lease was not persisted: %#v", claimed)
	}
	if replayed, replayErr := dispatch.Claim(ctx, fleet.ClaimRequest{
		ID: queued.ID, ExpectedVersion: queued.Version,
		ClaimID: []byte("claim-completed"), Lease: time.Minute,
		Context: integrationRequestContext(now),
	}); replayErr != nil || replayed.Version != claimed.Version {
		t.Fatalf("claim replay = %#v, %v", replayed, replayErr)
	}
	completed, err := dispatch.ExecuteClaim(ctx, claimed)
	if err != nil || completed.State != fleet.DispatchSucceeded {
		t.Fatalf("complete = %#v, %v", completed, err)
	}
	if replayed, replayErr := dispatch.Enqueue(ctx, completedRequest); replayErr != nil ||
		replayed.Version != completed.Version {
		t.Fatalf("completed event replay = %#v, %v", replayed, replayErr)
	}

	failingExecutor := &integrationExecutor{err: errors.New("executor failed")}
	executors["exec"] = failingExecutor
	failedRequest := integrationEnqueueRequest(now, "failed", "enqueue-failed")
	failedQueued, err := dispatch.Enqueue(ctx, failedRequest)
	if err != nil {
		t.Fatal(err)
	}
	failedClaim, err := dispatch.Claim(ctx, fleet.ClaimRequest{
		ID: failedQueued.ID, ExpectedVersion: failedQueued.Version,
		ClaimID: []byte("claim-failed"), Lease: time.Minute,
		Context: integrationRequestContext(now),
	})
	if err != nil {
		t.Fatal(err)
	}
	failed, executionErr := dispatch.ExecuteClaim(ctx, failedClaim)
	if executionErr == nil || failed.State != fleet.DispatchFailed {
		t.Fatalf("failed completion = %#v, %v", failed, executionErr)
	}
	if replayed, replayErr := dispatch.Enqueue(ctx, failedRequest); replayErr != nil ||
		replayed.Version != failed.Version {
		t.Fatalf("failed event replay = %#v, %v", replayed, replayErr)
	}

	executors["exec"] = successExecutor
	canceledRequest := integrationEnqueueRequest(now, "canceled", "enqueue-canceled")
	cancelQueued, err := dispatch.Enqueue(ctx, canceledRequest)
	if err != nil {
		t.Fatal(err)
	}
	cancelRequest := fleet.CancelRequest{
		ID: cancelQueued.ID, ExpectedVersion: cancelQueued.Version,
		MutationKey: []byte("cancel-key"), Context: integrationRequestContext(now),
	}
	canceled, err := dispatch.Cancel(ctx, cancelRequest)
	if err != nil || !bytes.Equal(canceled.CancelKey, cancelRequest.MutationKey) {
		t.Fatalf("cancel = %#v, %v", canceled, err)
	}
	if replayed, replayErr := dispatch.Cancel(ctx, cancelRequest); replayErr != nil ||
		replayed.Version != canceled.Version {
		t.Fatalf("cancel replay = %#v, %v", replayed, replayErr)
	}
	divergent := cancelRequest
	divergent.MutationKey = []byte("different-cancel-key")
	if _, err := dispatch.Cancel(ctx, divergent); !errors.Is(err, fleet.ErrActionConflict) {
		t.Fatalf("divergent cancel replay = %v", err)
	}

	events, _, err := eventBackend.Scan(ctx, 1, 0, 32)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]map[string]int{
		"completed": {
			"action.enqueued": 1, "action.claimed": 1, "action.completed": 1,
		},
		"failed": {
			"action.enqueued": 1, "action.claimed": 1, "action.failed": 1,
		},
		"canceled": {
			"action.enqueued": 1, "action.canceled": 1,
		},
	}
	if len(events) != 8 {
		t.Fatalf("lifecycle event count = %d, events=%#v", len(events), events)
	}
	for _, event := range events {
		action := string(event.ActionID)
		if want[action] == nil || want[action][event.Kind] != 1 {
			t.Fatalf("unexpected lifecycle event %q/%q", action, event.Kind)
		}
		want[action][event.Kind]--
	}
	for action, kinds := range want {
		for kind, remaining := range kinds {
			if remaining != 0 {
				t.Fatalf("missing lifecycle event %q/%q", action, kind)
			}
		}
	}

	if err := runtime.Close(); err != nil {
		t.Fatal(err)
	}
	runtime = nil
	runtime, err = explorercoord.Open(config)
	if err != nil {
		t.Fatal(err)
	}
	stored, err := explorerfleet.NewDispatchStore(runtime, nil)
	if err != nil {
		t.Fatal(err)
	}
	claimedStored, err := stored.GetAction(ctx, completedRequest.ID)
	if err != nil || claimedStored.ClaimLease != time.Minute {
		t.Fatalf("stored claim lease = %#v, %v", claimedStored, err)
	}
	canceledStored, err := stored.GetAction(ctx, canceledRequest.ID)
	if err != nil || !bytes.Equal(canceledStored.CancelKey, cancelRequest.MutationKey) {
		t.Fatalf("stored cancel key = %#v, %v", canceledStored, err)
	}
	reopenedEvents, err := New(runtime, config.Domain)
	if err != nil {
		t.Fatal(err)
	}
	events, _, err = reopenedEvents.Scan(ctx, 1, 0, 32)
	if err != nil || len(events) != 8 {
		t.Fatalf("reopened lifecycle events = %d, %v", len(events), err)
	}
}

func newIntegrationRegistry(
	t *testing.T, resolver auth.Resolver, now time.Time, executor fleet.ActionExecutor,
) (*fleet.Service, integrationExecutors) {
	t.Helper()
	descriptor := fleet.Descriptor{
		ID: "agent", Generation: 1, Subject: "owner", Actor: "actor",
		AuthorizationDomain: []byte("domain"),
		Scopes:              []fleet.Scope{{SourceID: []byte("source"), PolicyID: []byte("policy")}},
		ExecutorRef:         "exec", LeaseExpiresAt: now.Add(time.Hour), UpdatedAt: now,
		Capabilities: []fleet.Capability{{Name: "search", Actions: []fleet.Action{{
			Name: "query",
			InputSchema: json.RawMessage(
				`{"type":"object","properties":{"value":{"type":"integer"}},"required":["value"],"additionalProperties":false}`),
			OutputSchema: json.RawMessage(
				`{"type":"object","properties":{"ok":{"type":"boolean"}},"required":["ok"],"additionalProperties":false}`),
		}}}},
	}
	executors := integrationExecutors{"exec": executor}
	service, err := fleet.NewService(fleet.Config{
		Store:    integrationRegistryStore{stored: fleet.Stored{Descriptor: descriptor}},
		Resolver: resolver, Recorder: integrationLifecycleRecorder{},
		Snapshots: integrationSnapshot{now: now}, Executors: executors,
		Clock: func() time.Time { return now },
	})
	if err != nil {
		t.Fatal(err)
	}
	return service, executors
}

type integrationRegistryStore struct{ stored fleet.Stored }

func (s integrationRegistryStore) Apply(
	context.Context, fleet.Mutation,
) (fleet.Stored, error) {
	return fleet.Stored{}, errors.New("unexpected registry mutation")
}
func (s integrationRegistryStore) Get(
	_ context.Context, id shoal.ID,
) (fleet.Stored, error) {
	if id != s.stored.Descriptor.ID {
		return fleet.Stored{}, shoal.NewError(shoal.ErrorNotFound, "agent not found")
	}
	return s.stored, nil
}
func (s integrationRegistryStore) List(
	context.Context, []byte, int,
) (fleet.StoredPage, error) {
	return fleet.StoredPage{Entries: []fleet.Stored{s.stored}}, nil
}

type integrationExecutors map[string]fleet.Executor

func (e integrationExecutors) ResolveExecutor(ref string) (fleet.Executor, bool) {
	value, ok := e[ref]
	return value, ok
}

type integrationExecutor struct {
	result fleet.ExecutionResult
	err    error
}

func (e *integrationExecutor) Execute(
	context.Context, fleet.Invocation,
) (fleet.ExecutionResult, error) {
	return e.result, e.err
}

type integrationLifecycleRecorder struct{}

func (integrationLifecycleRecorder) RecordLifecycle(context.Context, fleet.Lifecycle) error {
	return nil
}

type integrationSnapshot struct{ now time.Time }

func (s integrationSnapshot) InteractionSnapshot(context.Context) (explorer.Snapshot, error) {
	return explorer.Snapshot{AsOf: s.now}, nil
}

type integrationActionRecorder struct{}

func (integrationActionRecorder) RecordAction(context.Context, fleet.ActionAudit) error {
	return nil
}

type integrationAuditor struct{}

func (integrationAuditor) RecordFleetAction(context.Context, fleetevents.AuditRecord) error {
	return nil
}

type integrationGeneration struct{}

func (integrationGeneration) CurrentPolicyGeneration(context.Context, []byte) (int64, error) {
	return 1, nil
}

type integrationLease struct{}

func (integrationLease) ValidateDelivery(context.Context, shoal.ID, int64) error {
	return nil
}

func integrationEnqueueRequest(now time.Time, id, key string) fleet.EnqueueRequest {
	return fleet.EnqueueRequest{
		ID: []byte(id), IdempotencyKey: []byte(key), AgentID: "agent",
		AgentGeneration: 1, Capability: "search", Action: "query",
		SourceID: []byte("source"), PolicyID: []byte("policy"),
		ObjectID: "object", Input: json.RawMessage(`{"value":1}`),
		Context: integrationRequestContext(now),
	}
}

func integrationRequestContext(now time.Time) fleet.RequestContext {
	return fleet.RequestContext{
		RequestID: "request", CorrelationID: "correlation",
		ReasonCode: "operator_request", Deadline: now.Add(time.Hour),
	}
}
