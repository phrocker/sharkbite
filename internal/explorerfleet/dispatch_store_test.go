// Licensed to the Apache Software Foundation (ASF) under one or more
// contributor license agreements. See the NOTICE file distributed with this
// work for additional information regarding copyright ownership.
package explorerfleet

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/phrocker/shoal-oss/internal/explorercoord"
	"github.com/phrocker/shoal-oss/pkg/explorer"
	"github.com/phrocker/shoal-oss/pkg/explorer/auth"
	"github.com/phrocker/shoal-oss/pkg/explorer/coordination"
	"github.com/phrocker/shoal-oss/pkg/explorer/coordination/transaction"
	"github.com/phrocker/shoal-oss/pkg/explorer/fleet"
	"github.com/phrocker/shoal-oss/pkg/interaction"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

func TestDispatchStoreCASReplayFenceAndRestart(t *testing.T) {
	directory := t.TempDir()
	runtime := openDispatchRuntime(t, directory)
	defer func() {
		if runtime != nil {
			_ = runtime.Close()
		}
	}()
	store, err := NewDispatchStore(runtime, nil)
	if err != nil {
		t.Fatal(err)
	}

	record := testActionRecord()
	created, err := store.ApplyAction(context.Background(), fleet.DispatchMutation{
		Token: []byte("enqueue-key"), Record: record,
	})
	if err != nil {
		t.Fatal(err)
	}
	replayed, err := store.ApplyAction(context.Background(), fleet.DispatchMutation{
		Token: []byte("enqueue-key"), Record: record,
	})
	if err != nil || replayed.Version != created.Version {
		t.Fatalf("replay = %#v, %v", replayed, err)
	}
	divergent := record
	divergent.Input = json.RawMessage(`{"value":2}`)
	if _, err := store.ApplyAction(context.Background(), fleet.DispatchMutation{
		Token: []byte("enqueue-key"), Record: divergent,
	}); !errors.Is(err, fleet.ErrActionConflict) {
		t.Fatalf("divergent replay = %v", err)
	}
	secondRecord := record
	secondRecord.ID = []byte{'b', 0, 255}
	secondRecord.IdempotencyKey = []byte("second-key")
	secondRecord.ExecutorKey = []byte("second-executor-key")
	if _, err := store.ApplyAction(context.Background(), fleet.DispatchMutation{
		Token: []byte("second-key"), Record: secondRecord,
	}); err != nil {
		t.Fatal(err)
	}
	firstPage, err := store.ScanActions(context.Background(), nil, 1)
	if err != nil || len(firstPage.Actions) != 1 ||
		string(firstPage.Actions[0].ID) != string(record.ID) ||
		string(firstPage.Next) != string(record.ID) {
		t.Fatalf("first page = %#v, %v", firstPage, err)
	}
	secondPage, err := store.ScanActions(context.Background(), firstPage.Next, 1)
	if err != nil || len(secondPage.Actions) != 1 ||
		string(secondPage.Actions[0].ID) != string(secondRecord.ID) {
		t.Fatalf("second page = %#v, %v", secondPage, err)
	}

	left, right := record, record
	left.Version, right.Version = 2, 2
	left.State, right.State = fleet.DispatchClaimed, fleet.DispatchClaimed
	left.ClaimID, right.ClaimID = []byte("left"), []byte("right")
	left.ClaimFence, right.ClaimFence = 1, 1
	left.ClaimLease, right.ClaimLease = time.Minute, time.Minute
	left.ClaimLeaseUntil = record.UpdatedAt.Add(time.Minute)
	right.ClaimLeaseUntil = record.UpdatedAt.Add(time.Minute)
	left.ExecutionPolicyGeneration, right.ExecutionPolicyGeneration = 1, 1
	left.ExecutionExpiresAt, right.ExecutionExpiresAt = record.Deadline, record.Deadline
	left.TransitionRequestID, right.TransitionRequestID = "left-request", "right-request"
	left.TransitionCorrelationID = "left-correlation"
	right.TransitionCorrelationID = "right-correlation"
	var wait sync.WaitGroup
	results := make(chan error, 2)
	for _, item := range []struct {
		token  []byte
		record fleet.ActionRecord
	}{{[]byte("claim-left"), left}, {[]byte("claim-right"), right}} {
		item := item
		wait.Add(1)
		go func() {
			defer wait.Done()
			_, applyErr := store.ApplyAction(context.Background(), fleet.DispatchMutation{
				Token: item.token, ExpectedVersion: 1, Record: item.record,
			})
			results <- applyErr
		}()
	}
	wait.Wait()
	close(results)
	successes, conflicts := 0, 0
	for result := range results {
		if result == nil {
			successes++
		} else if errors.Is(result, fleet.ErrActionConflict) ||
			errors.Is(result, transaction.ErrConflict) {
			conflicts++
		} else {
			t.Fatalf("claim result = %v", result)
		}
	}
	if successes != 1 || conflicts != 1 {
		t.Fatalf("successes=%d conflicts=%d", successes, conflicts)
	}
	current, err := store.GetAction(context.Background(), record.ID)
	if err != nil || current.Version != 2 || current.ClaimFence != 1 {
		t.Fatalf("current = %#v, %v", current, err)
	}
	stale := current
	stale.Version++
	stale.ClaimFence++
	if _, err := store.ApplyAction(context.Background(), fleet.DispatchMutation{
		Token: []byte("stale-fence"), ExpectedVersion: current.Version,
		ExpectedFence: current.ClaimFence + 1, Record: stale,
	}); !errors.Is(err, fleet.ErrActionConflict) {
		t.Fatalf("stale fence = %v", err)
	}
	if err := runtime.Close(); err != nil {
		t.Fatal(err)
	}
	runtime = nil
	runtime = openDispatchRuntime(t, directory)
	store, _ = NewDispatchStore(runtime, nil)
	restarted, err := store.GetAction(context.Background(), record.ID)
	if err != nil || restarted.Version != 2 ||
		string(restarted.ExecutorKey) != string(record.ExecutorKey) ||
		string(restarted.ClaimID) != string(current.ClaimID) ||
		restarted.EventProvenance().RequestID !=
			current.EventProvenance().RequestID ||
		restarted.EventProvenance().CorrelationID !=
			current.EventProvenance().CorrelationID {
		t.Fatalf("restart = %#v, %v", restarted, err)
	}
}

func TestDispatchRealRuntimeRegistryAndRestart(t *testing.T) {
	now := time.Date(2026, 9, 6, 12, 0, 0, 0, time.UTC)
	directory := t.TempDir()
	runtime := openFleetDispatchRuntime(t, directory)
	authority, err := auth.NewAuthorityWithClock(func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	executor := &integratedExecutor{}
	registry, dispatch := composeIntegratedServices(t, runtime, authority, executor, now)
	decision := integratedDecision(t, now)
	ctx, err := authority.Binder().Bind(context.Background(), decision)
	if err != nil {
		t.Fatal(err)
	}
	descriptor, err := registry.Register(ctx, fleet.RegisterRequest{
		Context: integratedContext(now), RegistrationKey: "register-agent",
		Spec: fleet.Spec{
			ID: "agent", AuthorizationDomain: []byte("domain"),
			Scopes:      []fleet.Scope{{SourceID: []byte("source"), PolicyID: []byte("policy")}},
			ExecutorRef: "exec", LeaseExpiresAt: now.Add(time.Hour),
			Capabilities: []fleet.Capability{{Name: "search", Actions: []fleet.Action{{
				Name:         "query",
				InputSchema:  json.RawMessage(`{"type":"object","properties":{"value":{"type":"integer"}},"required":["value"],"additionalProperties":false}`),
				OutputSchema: json.RawMessage(`{"type":"object","properties":{"ok":{"type":"boolean"}},"required":["ok"],"additionalProperties":false}`),
			}}}},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	queued, err := dispatch.Enqueue(ctx, fleet.EnqueueRequest{
		ID: []byte("durable-action"), IdempotencyKey: []byte("durable-key"),
		AgentID: descriptor.ID, AgentGeneration: descriptor.Generation,
		Capability: "search", Action: "query", SourceID: []byte("source"),
		PolicyID: []byte("policy"), ObjectID: "object",
		Input: json.RawMessage(`{"value":1}`), Context: integratedContext(now),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := runtime.Close(); err != nil {
		t.Fatal(err)
	}
	runtime = openFleetDispatchRuntime(t, directory)
	defer runtime.Close()
	_, dispatch = composeIntegratedServices(t, runtime, authority, executor, now)
	restarted, err := dispatch.Status(ctx, fleet.StatusRequest{
		ID: queued.ID, Context: integratedContext(now),
	})
	if err != nil || restarted.State != fleet.DispatchQueued ||
		string(restarted.ExecutorKey) != string(queued.ExecutorKey) {
		t.Fatalf("restarted action = %#v, %v", restarted, err)
	}
}

func TestDispatchCollidingOpaqueTuplesExecuteAndSurviveRuntimeRestart(t *testing.T) {
	now := time.Date(2026, 9, 6, 12, 0, 0, 0, time.UTC)
	directory := t.TempDir()
	runtime := openFleetDispatchRuntime(t, directory)
	defer func() {
		if runtime != nil {
			_ = runtime.Close()
		}
	}()
	authority, err := auth.NewAuthorityWithClock(func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	executor := &integratedExecutor{}
	registry, dispatch := composeIntegratedServices(
		t, runtime, authority, executor, now,
	)
	decision := integratedDecision(t, now)
	ctx, err := authority.Binder().Bind(context.Background(), decision)
	if err != nil {
		t.Fatal(err)
	}
	descriptor := registerIntegratedAgent(t, registry, ctx, now)
	requests := []struct {
		id, key, claim []byte
	}{
		{
			id: []byte("a\x00b"), key: []byte("c"),
			claim: []byte("claim-one"),
		},
		{
			id: []byte("a"), key: []byte("b\x00c"),
			claim: []byte("claim-two"),
		},
	}
	for _, request := range requests {
		enqueue := integratedEnqueue(
			now, descriptor, request.id, request.key,
		)
		queued, enqueueErr := dispatch.Enqueue(ctx, enqueue)
		if enqueueErr != nil {
			t.Fatalf("enqueue %x = %v", request.id, enqueueErr)
		}
		claimed, claimErr := dispatch.Claim(ctx, fleet.ClaimRequest{
			ID: queued.ID, ExpectedVersion: queued.Version,
			ClaimID: request.claim, Lease: time.Minute,
			Context: enqueue.Context,
		})
		if claimErr != nil {
			current, readErr := dispatch.Status(ctx, fleet.StatusRequest{
				ID: request.id, Context: enqueue.Context,
			})
			t.Fatalf(
				"claim %x = %v; current = %#v, %v",
				request.id, claimErr, current, readErr,
			)
		}
		completed, executeErr := dispatch.ExecuteClaim(ctx, claimed)
		if executeErr != nil {
			t.Fatalf("execute %x = %v", request.id, executeErr)
		}
		if completed.State != fleet.DispatchSucceeded {
			t.Fatalf("invoke %x state = %q", request.id, completed.State)
		}
	}
	calls, effects, keys := executor.snapshot()
	if calls != 2 || effects != 2 || len(keys) != 2 ||
		bytes.Equal(keys[0], keys[1]) {
		t.Fatalf(
			"pre-restart executor calls/effects/keys = %d/%d/%x",
			calls, effects, keys,
		)
	}
	if err := runtime.Close(); err != nil {
		t.Fatal(err)
	}
	runtime = nil
	runtime = openFleetDispatchRuntime(t, directory)
	_, dispatch = composeIntegratedServices(t, runtime, authority, executor, now)
	for _, request := range requests {
		restarted, statusErr := dispatch.Status(ctx, fleet.StatusRequest{
			ID: request.id, Context: integratedContext(now),
		})
		if statusErr != nil || restarted.State != fleet.DispatchSucceeded {
			t.Fatalf(
				"restarted action %x = %#v, %v",
				request.id, restarted, statusErr,
			)
		}
	}
	calls, effects, _ = executor.snapshot()
	if calls != 2 || effects != 2 {
		t.Fatalf(
			"restart repeated effects: calls=%d effects=%d", calls, effects,
		)
	}
}

func TestDispatchDurableRestartPreservesStoredExecutorKeyAfterAmbiguousEffect(
	t *testing.T,
) {
	now := time.Date(2026, 9, 6, 12, 0, 0, 0, time.UTC)
	directory := t.TempDir()
	runtime := openFleetDispatchRuntime(t, directory)
	defer func() {
		if runtime != nil {
			_ = runtime.Close()
		}
	}()
	authority, err := auth.NewAuthorityWithClock(func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	executor := &integratedExecutor{}
	failingRecorder := &controlledIntegratedActionRecorder{
		failPhase: "effect_outcome",
	}
	registry, dispatch := composeIntegratedServicesWithRecorder(
		t, runtime, authority, executor, failingRecorder, now,
	)
	decision := integratedDecision(t, now)
	ctx, err := authority.Binder().Bind(context.Background(), decision)
	if err != nil {
		t.Fatal(err)
	}
	descriptor := registerIntegratedAgent(t, registry, ctx, now)
	request := integratedEnqueue(
		now, descriptor, []byte("legacy-action"), []byte("legacy-request-key"),
	)
	store, err := NewDispatchStore(runtime, nil)
	if err != nil {
		t.Fatal(err)
	}
	// The legacy record is planted as the action's *first* version rather than
	// by mutating a service-written one.
	//
	// It used to enqueue through the service and then rewrite the executor
	// key, which refuseRewrittenIdentity now refuses — correctly, since the
	// key is immutable and the property this test asserts is that the service
	// preserves it. Planting it as version 1 reaches the store's create path,
	// which has no stored record to compare against, so the precondition is
	// expressible without a rewrite and without exempting the field.
	//
	// A template enqueue under a different identity supplies a
	// service-built record, so the planted one differs from what the service
	// would write in exactly one field. Invoke then replays onto it, because
	// equivalentEnqueue compares the request's own fields and not the
	// executor key.
	template := integratedEnqueue(
		now, descriptor, []byte("template-action"), []byte("template-key"),
	)
	built, err := dispatch.Enqueue(ctx, template)
	if err != nil {
		t.Fatal(err)
	}
	legacyKey := []byte("persisted-pre-v2-executor-key")
	withLegacyKey := built
	withLegacyKey.ID = append([]byte(nil), request.ID...)
	withLegacyKey.IdempotencyKey = append(
		[]byte(nil), request.IdempotencyKey...)
	withLegacyKey.ExecutorKey = append([]byte(nil), legacyKey...)
	withLegacyKey, err = store.ApplyAction(
		ctx,
		fleet.DispatchMutation{
			Token:           []byte("install-legacy-executor-key"),
			ExpectedVersion: 0,
			TransitionKind:  "action.enqueued",
			Record:          withLegacyKey,
		},
	)
	if err != nil {
		current, readErr := store.GetAction(ctx, request.ID)
		t.Fatalf("install legacy key = %v; current = %#v, %v", err, current, readErr)
	}
	if !bytes.Equal(withLegacyKey.ExecutorKey, legacyKey) {
		t.Fatalf("the planted record does not carry the legacy key: %x",
			withLegacyKey.ExecutorKey)
	}
	_, err = dispatch.Invoke(ctx, fleet.InvokeRequest{
		Enqueue: request, ClaimID: []byte("claim"), Lease: time.Minute,
	})
	if !errors.Is(err, fleet.ErrExecutionAmbiguous) ||
		!errors.Is(err, fleet.ErrRecordingUnavailable) {
		t.Fatalf("post-effect receipt failure = %v", err)
	}
	claimed, err := store.GetAction(ctx, request.ID)
	if err != nil || claimed.State != fleet.DispatchClaimed ||
		!bytes.Equal(claimed.ExecutorKey, legacyKey) ||
		claimed.Version != withLegacyKey.Version+1 {
		t.Fatalf("ambiguous durable state = %#v, %v", claimed, err)
	}
	if err := runtime.Close(); err != nil {
		t.Fatal(err)
	}
	runtime = nil
	runtime = openFleetDispatchRuntime(t, directory)
	_, dispatch = composeIntegratedServicesWithRecorder(
		t, runtime, authority, executor, integratedActionRecorder{}, now,
	)
	completed, err := dispatch.Invoke(ctx, fleet.InvokeRequest{
		Enqueue: request, ClaimID: []byte("claim"), Lease: time.Minute,
	})
	if err != nil {
		t.Fatal(err)
	}
	calls, effects, keys := executor.snapshot()
	if completed.State != fleet.DispatchSucceeded ||
		completed.Version != claimed.Version+1 ||
		!bytes.Equal(completed.ExecutorKey, legacyKey) ||
		calls != 2 || effects != 1 || len(keys) != 2 ||
		!bytes.Equal(keys[0], legacyKey) ||
		!bytes.Equal(keys[1], legacyKey) {
		t.Fatalf(
			"recovered action=%#v calls=%d effects=%d keys=%x",
			completed, calls, effects, keys,
		)
	}
}

func TestDispatchMutationAfterFixedTimestampRestartRuntimeGate(t *testing.T) {
	now := time.Date(2026, 9, 6, 12, 0, 0, 0, time.UTC)
	directory := t.TempDir()
	open := func() *explorercoord.Runtime {
		runtime, err := explorercoord.Open(explorercoord.Config{
			Directory: directory, Domain: coordination.DomainID("fleet-dispatch-fixed"),
			Owner: coordination.OwnerID("dispatch-worker"),
			Authority: transaction.Authority{
				Generation: 1, Fence: 1, Holder: coordination.OwnerID("dispatch-process"),
				Mode: coordination.WriterModeEmbeddedPrimary, RetentionGeneration: 1, HistoryFloor: 1,
			},
			PhysicalTables: []string{DispatchPhysicalTable()}, Lease: time.Minute,
			Clock:         func() time.Time { return now },
			RecoveryLimit: 16, RecoveryMaxPages: 64,
			RetryBackoff: time.Nanosecond, RecoveryBackoff: time.Nanosecond,
		})
		if err != nil {
			t.Fatal(err)
		}
		return runtime
	}

	runtime := open()
	store, err := NewDispatchStore(runtime, nil)
	if err != nil {
		t.Fatal(err)
	}
	record := testActionRecord()
	if _, err := store.ApplyAction(context.Background(), fleet.DispatchMutation{
		Token: []byte("fixed-enqueue"), Record: record,
	}); err != nil {
		t.Fatal(err)
	}
	if err := runtime.Close(); err != nil {
		t.Fatal(err)
	}

	runtime = open()
	defer runtime.Close()
	store, err = NewDispatchStore(runtime, nil)
	if err != nil {
		t.Fatal(err)
	}
	current, err := store.GetAction(context.Background(), record.ID)
	if err != nil {
		t.Fatal(err)
	}
	claimed := current
	claimed.Version++
	claimed.State = fleet.DispatchClaimed
	claimed.ClaimID = []byte("fixed-claim")
	claimed.ClaimFence++
	claimed.ClaimLease = time.Minute
	claimed.ClaimLeaseUntil = now.Add(time.Minute)
	claimed.ExecutionPolicyGeneration = 1
	claimed.ExecutionExpiresAt = record.Deadline
	stored, err := store.ApplyAction(context.Background(), fleet.DispatchMutation{
		Token: []byte("fixed-claim-token"), ExpectedVersion: current.Version,
		ExpectedFence: current.ClaimFence, Record: claimed,
	})
	if err != nil {
		t.Fatal(err)
	}
	if stored.Version != claimed.Version || stored.ClaimFence != claimed.ClaimFence ||
		!bytes.Equal(stored.ClaimID, claimed.ClaimID) {
		t.Fatalf("claimed action = %#v", stored)
	}
}

func openDispatchRuntime(t *testing.T, directory string) *explorercoord.Runtime {
	t.Helper()
	runtime, err := explorercoord.Open(explorercoord.Config{
		Directory: directory, Domain: coordination.DomainID("fleet-dispatch"),
		Owner: coordination.OwnerID("dispatch-worker"),
		Authority: transaction.Authority{
			Generation: 1, Fence: 1, Holder: coordination.OwnerID("dispatch-process"),
			Mode: coordination.WriterModeEmbeddedPrimary, RetentionGeneration: 1, HistoryFloor: 1,
		},
		PhysicalTables: []string{DispatchPhysicalTable()}, Lease: time.Minute,
		RecoveryLimit: 16, RecoveryMaxPages: 64,
		RetryBackoff: time.Nanosecond, RecoveryBackoff: time.Nanosecond,
	})
	if err != nil {
		t.Fatal(err)
	}

	return runtime
}

func openFleetDispatchRuntime(t *testing.T, directory string) *explorercoord.Runtime {
	t.Helper()
	config := ConfigureRuntime(explorercoord.Config{
		Directory: directory, Domain: coordination.DomainID("fleet-integrated"),
		Owner: coordination.OwnerID("fleet-worker"),
		Authority: transaction.Authority{
			Generation: 1, Fence: 1, Holder: coordination.OwnerID("fleet-process"),
			Mode: coordination.WriterModeEmbeddedPrimary, RetentionGeneration: 1, HistoryFloor: 1,
		},
		Lease: time.Minute, RecoveryLimit: 16, RecoveryMaxPages: 64,
		RetryBackoff: time.Nanosecond, RecoveryBackoff: time.Nanosecond,
	})
	runtime, err := explorercoord.Open(config)
	if err != nil {
		t.Fatal(err)
	}
	return runtime
}

func composeIntegratedServices(
	t *testing.T,
	runtime *explorercoord.Runtime,
	authority *auth.Authority,
	executor *integratedExecutor,
	now time.Time,
) (*fleet.Service, *fleet.DispatchService) {
	return composeIntegratedServicesWithRecorder(
		t, runtime, authority, executor, integratedActionRecorder{}, now,
	)
}

func composeIntegratedServicesWithRecorder(
	t *testing.T,
	runtime *explorercoord.Runtime,
	authority *auth.Authority,
	executor *integratedExecutor,
	recorder fleet.ActionRecorder,
	now time.Time,
) (*fleet.Service, *fleet.DispatchService) {
	t.Helper()
	registryStore, err := NewStore(runtime, nil)
	if err != nil {
		t.Fatal(err)
	}
	registry, err := fleet.NewService(fleet.Config{
		Store: registryStore, Resolver: authority.Resolver(),
		Recorder:  integratedLifecycleRecorder{},
		Snapshots: integratedSnapshot{now},
		Executors: integratedExecutors{"exec": executor},
		Clock:     func() time.Time { return now },
	})
	if err != nil {
		t.Fatal(err)
	}

	dispatchStore, err := NewDispatchStore(runtime, nil)
	if err != nil {
		t.Fatal(err)
	}
	dispatch, err := fleet.NewDispatchService(fleet.DispatchConfig{
		Store: dispatchStore, Registry: registry, Resolver: authority.Resolver(),
		Recorder: recorder, Events: integratedEvents{},
		Clock: func() time.Time { return now },
	})
	if err != nil {
		t.Fatal(err)
	}
	return registry, dispatch
}

type integratedSnapshot struct{ now time.Time }

func (s integratedSnapshot) InteractionSnapshot(context.Context) (explorer.Snapshot, error) {
	return explorer.Snapshot{ID: "snapshot", AsOf: s.now, Frontier: 1}, nil
}

type integratedLifecycleRecorder struct{}

func (integratedLifecycleRecorder) RecordLifecycle(context.Context, fleet.Lifecycle) error {
	return nil
}

type integratedActionRecorder struct{}

func (integratedActionRecorder) RecordAction(context.Context, fleet.ActionAudit) error {
	return nil
}

type controlledIntegratedActionRecorder struct {
	failPhase string
}

func (r *controlledIntegratedActionRecorder) RecordAction(
	_ context.Context,
	audit fleet.ActionAudit,
) error {
	if audit.Phase == r.failPhase {
		return errors.New("injected action receipt failure")
	}
	return nil
}

type integratedEvents struct{}

func (integratedEvents) MayPublishActionEvent(
	context.Context, string, fleet.ActionRecord,
) (bool, error) {
	return true, nil
}

func (integratedEvents) PublishActionEvent(context.Context, string, fleet.ActionRecord) error {
	return nil
}

func TestDispatchStorePersistsImmutableTransitionOutboxAcrossRestart(t *testing.T) {
	directory := t.TempDir()
	runtime := openDispatchRuntime(t, directory)
	store, err := NewDispatchStore(runtime, nil)
	if err != nil {
		t.Fatal(err)
	}
	record := testActionRecord()
	token := []byte("enqueue-transition")
	if _, err := store.ApplyAction(context.Background(), fleet.DispatchMutation{
		Token: token, TransitionKind: "action.enqueued", Record: record,
	}); err != nil {
		t.Fatal(err)
	}
	if err := runtime.Close(); err != nil {
		t.Fatal(err)
	}
	runtime = openDispatchRuntime(t, directory)
	defer runtime.Close()
	store, err = NewDispatchStore(runtime, nil)
	if err != nil {
		t.Fatal(err)
	}
	page, err := store.PendingActionTransitions(
		context.Background(), record.ID, nil, 10)
	if err != nil || len(page.Transitions) != 1 {
		t.Fatalf("pending after restart = %#v, %v", page, err)
	}
	transition := page.Transitions[0]
	if transition.Kind != "action.enqueued" ||
		!bytes.Equal(transition.ID, token) ||
		!reflect.DeepEqual(transition.Record, record) {
		t.Fatalf("transition = %#v", transition)
	}
	if err := store.CompleteActionTransition(
		context.Background(), transition); err != nil {
		t.Fatal(err)
	}
	page, err = store.PendingActionTransitions(
		context.Background(), record.ID, nil, 10)
	if err != nil || len(page.Transitions) != 0 {
		t.Fatalf("pending after completion = %#v, %v", page, err)
	}
	history, _, err := store.readTransition(
		context.Background(), record.ID, record.Version, token)
	if err != nil || history.CompletedAt.IsZero() ||
		!reflect.DeepEqual(history.Transition, transition) {
		t.Fatalf("durable transition history = %#v, %v", history, err)
	}
}

type integratedExecutors map[string]fleet.Executor

func (e integratedExecutors) ResolveExecutor(ref string) (fleet.Executor, bool) {
	value, ok := e[ref]
	return value, ok
}

type integratedExecutor struct {
	mu      sync.Mutex
	calls   int
	keys    [][]byte
	effects map[string]struct{}
}

func (e *integratedExecutor) Execute(
	_ context.Context,
	invocation fleet.Invocation,
) (fleet.ExecutionResult, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.calls++
	key := append([]byte(nil), invocation.IdempotencyKey...)
	e.keys = append(e.keys, key)
	if e.effects == nil {
		e.effects = make(map[string]struct{})
	}
	e.effects[string(key)] = struct{}{}
	return fleet.ExecutionResult{Output: json.RawMessage(`{"ok":true}`)}, nil
}

func (e *integratedExecutor) snapshot() (int, int, [][]byte) {
	e.mu.Lock()
	defer e.mu.Unlock()
	keys := make([][]byte, len(e.keys))
	for index := range e.keys {
		keys[index] = append([]byte(nil), e.keys[index]...)
	}
	return e.calls, len(e.effects), keys
}

func registerIntegratedAgent(
	t *testing.T,
	registry *fleet.Service,
	ctx context.Context,
	now time.Time,
) fleet.Descriptor {
	t.Helper()
	descriptor, err := registry.Register(ctx, fleet.RegisterRequest{
		Context: integratedContext(now), RegistrationKey: "register-agent",
		Spec: fleet.Spec{
			ID: "agent", AuthorizationDomain: []byte("domain"),
			Scopes: []fleet.Scope{{
				SourceID: []byte("source"), PolicyID: []byte("policy"),
			}},
			ExecutorRef: "exec", LeaseExpiresAt: now.Add(time.Hour),
			Capabilities: []fleet.Capability{{
				Name: "search", Actions: []fleet.Action{{
					Name: "query",
					InputSchema: json.RawMessage(
						`{"type":"object","properties":{"value":{"type":"integer"}},"required":["value"],"additionalProperties":false}`,
					),
					OutputSchema: json.RawMessage(
						`{"type":"object","properties":{"ok":{"type":"boolean"}},"required":["ok"],"additionalProperties":false}`,
					),
				}},
			}},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	return descriptor
}

func integratedEnqueue(
	now time.Time,
	descriptor fleet.Descriptor,
	id, key []byte,
) fleet.EnqueueRequest {
	return fleet.EnqueueRequest{
		ID: id, IdempotencyKey: key,
		AgentID: descriptor.ID, AgentGeneration: descriptor.Generation,
		Capability: "search", Action: "query", SourceID: []byte("source"),
		PolicyID: []byte("policy"), ObjectID: "object",
		Input: json.RawMessage(`{"value":1}`), Context: integratedContext(now),
	}
}

func integratedDecision(t *testing.T, now time.Time) auth.Decision {
	t.Helper()
	decision, err := auth.NewDecision(auth.DecisionConfig{
		Subject: "owner", Actor: "actor", AuthorizationDomain: []byte("domain"),
		AllowedOperations: []auth.Operation{
			auth.OperationAgentRegister, auth.OperationDispatch, auth.OperationInvoke,
		},
		PermittedSourceIDs: [][]byte{[]byte("source")},
		PermittedPolicyIDs: [][]byte{[]byte("policy")}, PolicyGeneration: 1,
		AuthenticationExpires: now.Add(2 * time.Hour), RequestID: "request",
		CorrelationID: "correlation",
	})
	if err != nil {
		t.Fatal(err)
	}
	return decision
}

func integratedContext(now time.Time) fleet.RequestContext {
	return fleet.RequestContext{
		RequestID: "request", CorrelationID: "correlation",
		ReasonCode: "operator_request", Deadline: now.Add(time.Hour),
	}
}

func testActionRecord() fleet.ActionRecord {
	now := time.Date(2026, 9, 6, 12, 0, 0, 0, time.UTC)
	return fleet.ActionRecord{
		ID: []byte{'a', 0, 255}, IdempotencyKey: []byte{'k', 0, 255},
		Version: 1, State: fleet.DispatchQueued, AgentID: "agent",
		AgentGeneration: 1, Capability: "search", Action: "query",
		SourceID: []byte("source"), PolicyID: []byte("policy"), ObjectID: "object",
		Input: json.RawMessage(`{"value":1}`), Subject: "subject", Actor: "actor",
		ClientID: "client", OnBehalfOf: []shoal.ID{"principal"},
		PolicyGeneration: 1, AuthorizedOperations: []auth.Operation{auth.OperationDispatch},
		AuthorizationExpiresAt: now.Add(time.Hour),
		RequestID:              "request", CorrelationID: "correlation",
		Reason: interaction.Reason{Code: "test"}, Deadline: now.Add(time.Hour),
		CreatedAt: now, UpdatedAt: now, ExecutorKey: []byte{'e', 0, 255},
	}
}

// TestDispatchStoreRefusesARewrittenIdentity is the invariant that did not
// exist, and #443 is why it should.
//
// The version and the fence give correct serialisation — one writer wins, a
// stale writer is refused — but they say nothing about what the winner may
// change. #443 shipped `record.Actor = decision.Actor()` in applyClaim. It was
// a no-op while the claimant was by construction the enqueuer, and became a
// third party overwriting the record's own principal, permanently, because the
// completion path clones forward. The enqueuer was then refused Status and
// Cancel on its own in-flight action, and nothing below the service could have
// refused that write.
//
// Every probe below changes one field on an otherwise valid next-version
// record, so a row that stops failing means that field stopped being immutable
// rather than that the test drifted.
func TestDispatchStoreRefusesARewrittenIdentity(t *testing.T) {
	directory := t.TempDir()
	runtime := openDispatchRuntime(t, directory)
	defer func() {
		if runtime != nil {
			_ = runtime.Close()
		}
	}()
	store, err := NewDispatchStore(runtime, nil)
	if err != nil {
		t.Fatal(err)
	}
	record := testActionRecord()
	created, err := store.ApplyAction(
		context.Background(), fleet.DispatchMutation{
			Token: []byte("enqueue-key"), Record: record,
		})
	if err != nil {
		t.Fatal(err)
	}

	// The control. A transition that changes only state advances, so every
	// refusal below is about the field it changed and not about the shape of
	// the mutation.
	advance := func() fleet.ActionRecord {
		next := created
		next.Version++
		next.State = fleet.DispatchCanceled
		next.CancelKey = []byte("cancel-key")
		next.UpdatedAt = created.UpdatedAt.Add(time.Second)
		return next
	}
	if _, err := store.ApplyAction(
		context.Background(), fleet.DispatchMutation{
			Token: []byte("control-advance"), Record: advance(),
			ExpectedVersion: created.Version,
			ExpectedFence:   created.ClaimFence,
		}); err != nil {
		t.Fatalf("a state-only transition was refused, so the probes below "+
			"prove nothing: %v", err)
	}

	// Re-open on a fresh directory so each probe starts from the created
	// record rather than from the control's cancellation.
	// seed shapes the record before it is created, for fields that only a
	// particular record shape may carry at all. Without it, nine probes were
	// refused by ActionRecord.Validate for being malformed rather than by the
	// invariant for being a rewrite — and because both once returned the same
	// error code, the assertion could not tell. The invariant now returns
	// ErrorInternal, and each probe validates its own rewritten record before
	// applying it, so a fixture that cannot express its condition fails as a
	// fixture rather than passing as a test.
	for _, probe := range []struct {
		field   string
		seed    func(*fleet.ActionRecord)
		rewrite func(*fleet.ActionRecord)
	}{
		{"subject", nil, func(r *fleet.ActionRecord) { r.Subject = "someone-else" }},
		// The #443 rewrite, exactly.
		{"actor", nil, func(r *fleet.ActionRecord) { r.Actor = "another-actor" }},
		{"client ID", nil, func(r *fleet.ActionRecord) { r.ClientID = "another-client" }},
		{"delegation chain", nil, func(r *fleet.ActionRecord) {
			r.OnBehalfOf = []shoal.ID{"smuggled"}
		}},
		{"input", nil, func(r *fleet.ActionRecord) {
			r.Input = json.RawMessage(`{"value":99}`)
		}},
		{"object ID", nil, func(r *fleet.ActionRecord) { r.ObjectID = "another-object" }},
		{"source ID", nil, func(r *fleet.ActionRecord) { r.SourceID = []byte("other") }},
		{"policy ID", nil, func(r *fleet.ActionRecord) { r.PolicyID = []byte("other") }},
		{"agent ID", nil, func(r *fleet.ActionRecord) { r.AgentID = "another-agent" }},
		{"agent generation", nil, func(r *fleet.ActionRecord) { r.AgentGeneration = 99 }},
		{"capability", nil, func(r *fleet.ActionRecord) { r.Capability = "other" }},
		{"action", nil, func(r *fleet.ActionRecord) { r.Action = "other" }},
		{"idempotency key", nil, func(r *fleet.ActionRecord) {
			r.IdempotencyKey = []byte("another-key")
		}},
		{"authorization fingerprint", nil, func(r *fleet.ActionRecord) {
			r.AuthorizationFingerprint[0] ^= 1
		}},
		{"policy generation", nil, func(r *fleet.ActionRecord) {
			r.PolicyGeneration = 99
		}},
		{"authorization expiry", nil, func(r *fleet.ActionRecord) {
			r.AuthorizationExpiresAt = r.AuthorizationExpiresAt.Add(time.Hour)
		}},
		// Backwards, not forwards. Forwards pushed CreatedAt past UpdatedAt,
		// which ActionRecord.Validate refuses inside encodeAction before this
		// check runs — so the probe passed while covering nothing, and the
		// assertion could not tell because both refusals shared one error
		// code. The invariant now returns ErrorInternal and the probe moves
		// the time the other way.
		{"creation time", nil, func(r *fleet.ActionRecord) {
			r.CreatedAt = r.CreatedAt.Add(-time.Hour)
		}},
		// The enqueue request's own identifiers, not the transition's. A
		// transition that overwrote these would destroy the link back to the
		// dispatch that created the record and leave the per-transition
		// fields unused.
		{"request ID", nil, func(r *fleet.ActionRecord) { r.RequestID = "other" }},
		{"correlation ID", nil, func(r *fleet.ActionRecord) { r.CorrelationID = "other" }},
		// The most dangerous of them: an extended deadline lets an action
		// outlive the bound its dispatcher accepted.
		{"deadline", nil, func(r *fleet.ActionRecord) {
			r.Deadline = r.Deadline.Add(24 * time.Hour)
		}},
		// A real Reason, not a hand-built one with a bogus digest: Validate
		// requires the digest to be 64 lowercase hex characters, so "x" was
		// refused before this check and the probe covered nothing.
		{"reason", nil, func(r *fleet.ActionRecord) {
			r.Reason = testActionReason(t)
		}},
		{"executor key", nil, func(r *fleet.ActionRecord) {
			r.ExecutorKey = []byte("rederived-key")
		}},
		// The admission grant, which only an admission-shaped record may
		// carry: Validate refuses disclosures that are not a digest, an
		// obligation with no declared references, and an identity scheme with
		// no admitted effect. So these are seeded as a complete grant and
		// rewritten to another *valid* value.
		//
		// AdmittedEffects is the sharpest and the closest structural
		// analogue to #443: isAdmission keys on it being non-empty, and
		// Claim, Cancel, ExtendClaim and completeClaim each refuse an
		// admission on that basis, so a transition that changed it converts a
		// grant into ordinary dispatch work and unlocks all four.
		{"admitted effects", seedAdmittedGrant, func(r *fleet.ActionRecord) {
			r.AdmittedEffects = fleet.Effects{fleet.EffectEgressesContent}
		}},
		{"admitted disclosures", seedAdmittedGrant, func(r *fleet.ActionRecord) {
			other := sha256.Sum256([]byte("another-disclosure-set"))
			r.AdmittedDisclosures = other[:]
		}},
		{"admitted obligation", seedAdmittedGrant, func(r *fleet.ActionRecord) {
			r.AdmittedObligation = []byte{0x02}
		}},
		{"admitted identity scheme", seedAdmittedGrant, func(r *fleet.ActionRecord) {
			r.AdmittedIdentityScheme = 2
		}},
		// The approval it was materialized under. #451's separation-of-duty
		// check is only as good as the record of who decided, and Validate
		// requires the provenance to be complete or entirely absent — so
		// these are seeded complete and rewritten to another complete value.
		{"approval request digest", seedApproval, func(r *fleet.ActionRecord) {
			other := sha256.Sum256([]byte("another-request"))
			r.ApprovalRequestDigest = other[:]
		}},
		{"approver subject", seedApproval, func(r *fleet.ActionRecord) {
			r.ApproverSubject = "another-approver"
		}},
		{"approver actor", seedApproval, func(r *fleet.ActionRecord) {
			r.ApproverActor = "another-approver-actor"
		}},
		{"approver client ID", seedApproval, func(r *fleet.ActionRecord) {
			r.ApproverClientID = "another-approver-client"
		}},
		{"approval time", seedApproval, func(r *fleet.ActionRecord) {
			r.ApprovedAt = r.ApprovedAt.Add(-time.Minute)
		}},
	} {
		t.Run(probe.field, func(t *testing.T) {
			probeDirectory := t.TempDir()
			probeRuntime := openDispatchRuntime(t, probeDirectory)
			defer func() { _ = probeRuntime.Close() }()
			probeStore, err := NewDispatchStore(probeRuntime, nil)
			if err != nil {
				t.Fatal(err)
			}
			seeded := testActionRecord()
			if probe.seed != nil {
				probe.seed(&seeded)
			}
			base, err := probeStore.ApplyAction(
				context.Background(), fleet.DispatchMutation{
					Token: []byte("enqueue-key"), Record: seeded,
				})
			if err != nil {
				t.Fatalf("the seeded record was refused, so this probe "+
					"cannot express its condition: %v", err)
			}
			next := base
			next.Version++
			next.State = fleet.DispatchCanceled
			next.CancelKey = []byte("cancel-key")
			next.UpdatedAt = base.UpdatedAt.Add(time.Second)
			probe.rewrite(&next)
			// The rewritten record must itself be valid, or Validate refuses
			// it inside encodeAction before the invariant is reached and the
			// probe proves nothing. Nine probes were in exactly that state.
			if err := next.Validate(); err != nil {
				t.Fatalf("the rewritten record is invalid for an unrelated "+
					"reason, so this probe cannot reach the invariant: %v",
					err)
			}

			_, err = probeStore.ApplyAction(
				context.Background(), fleet.DispatchMutation{
					Token: []byte("rewrite"), Record: next,
					ExpectedVersion: base.Version,
					ExpectedFence:   base.ClaimFence,
				})
			if err == nil {
				t.Fatalf("a mutation rewrote the record's %s and the store "+
					"wrote it: nothing below the service refuses this, which "+
					"is how #443's Actor overwrite reached a durable record",
					probe.field)
			}
			// Internal, not invalid-argument. ActionRecord.Validate runs
			// first inside encodeAction and returns invalid-argument, so
			// asserting that code cannot tell "the invariant refused this"
			// from "the probe built a malformed record" — which is exactly
			// how two of these probes came to cover nothing.
			if !shoal.IsErrorCode(err, shoal.ErrorInternal) {
				t.Fatalf("%s rewrite refused as %v, which is not this "+
					"invariant refusing it: a record Validate rejects for "+
					"an unrelated reason would look identical",
					probe.field, err)
			}
			if !strings.Contains(err.Error(), "is immutable") {
				t.Fatalf("%s refusal does not name the field: %v",
					probe.field, err)
			}
			stored, readErr := probeStore.GetAction(
				context.Background(), base.ID)
			if readErr != nil || stored.Version != base.Version {
				t.Fatalf("the refused mutation moved the record anyway: "+
					"version %d, %v", stored.Version, readErr)
			}
		})
	}
}

// TestDispatchStoreEnforcesMonotonicFenceAndEffect covers the two fields that
// are monotonic rather than immutable, in the same place and for the same
// reason: a fence that moved backwards would make a stale completion look
// current, and nothing can establish that an effect did not occur after
// something has said it might have.
func TestDispatchStoreEnforcesMonotonicFenceAndEffect(t *testing.T) {
	for _, probe := range []struct {
		name    string
		prepare func(*fleet.ActionRecord)
		rewrite func(*fleet.ActionRecord)
	}{
		{
			// A queued record may not carry claim state at all, so the seed
			// has to be a valid claimed one. Getting this wrong is how the
			// first version of this probe "passed": the seed was refused for
			// an unrelated reason and the rewrite never ran.
			name: "a fence may not move backwards",
			prepare: func(r *fleet.ActionRecord) {
				now := r.CreatedAt
				r.State = fleet.DispatchClaimed
				r.ClaimID = []byte("claim")
				r.ClaimFence = 4
				r.ClaimLease = time.Minute
				r.ClaimLeaseUntil = now.Add(time.Minute)
				r.ClaimantSubject = r.Subject
				r.ClaimantActor = r.Actor
				r.ClaimantClientID = r.ClientID
				r.ClaimantOnBehalfOf = r.OnBehalfOf
				r.TransitionOperation = auth.OperationInvoke
				r.ExecutionPolicyGeneration = 1
				r.ExecutionExpiresAt = now.Add(time.Hour)
			},
			rewrite: func(r *fleet.ActionRecord) { r.ClaimFence = 3 },
		},
		{
			// A queued record may carry EffectPossible. Validate requires
			// it on DispatchSucceeded and DispatchFailed specifically —
			// DispatchCanceled is terminal and does not require it — and
			// permits it anywhere.
			name:    "a possible effect may not be withdrawn",
			prepare: func(r *fleet.ActionRecord) { r.EffectPossible = true },
			rewrite: func(r *fleet.ActionRecord) { r.EffectPossible = false },
		},
		{
			// Write-once, a third class: zero before the terminal
			// transition, written exactly once by it, immutable after
			// (#427). Neither immutable from creation — the terminal
			// transition must set it — nor monotonic, because a larger
			// number is not more valid and a different one is a rewrite of
			// what was reported.
			//
			// The seed must be a *failed* record: Validate refuses a volume
			// on any other state, so seeding a queued one would be refused
			// for an unrelated reason and the rewrite would never run, which
			// is exactly how the fence probe above first "passed".
			name: "an effected volume is written once",
			prepare: func(r *fleet.ActionRecord) {
				now := r.CreatedAt
				r.State = fleet.DispatchFailed
				r.ErrorCode = "response_truncated"
				r.EffectPossible = true
				r.ClaimID = []byte("claim")
				r.ClaimFence = 1
				r.ClaimLease = time.Minute
				r.ClaimLeaseUntil = now.Add(time.Minute)
				r.ClaimantSubject = r.Subject
				r.ClaimantActor = r.Actor
				r.ClaimantClientID = r.ClientID
				r.ClaimantOnBehalfOf = r.OnBehalfOf
				r.TransitionOperation = auth.OperationInvoke
				r.ExecutionPolicyGeneration = 1
				r.ExecutionExpiresAt = now.Add(time.Hour)
				r.Effected = fleet.EffectedVolume{Bytes: 2 << 20, Chunks: 64}
			},
			rewrite: func(r *fleet.ActionRecord) {
				r.Effected = fleet.EffectedVolume{Bytes: 1}
			},
		},
	} {
		t.Run(probe.name, func(t *testing.T) {
			directory := t.TempDir()
			runtime := openDispatchRuntime(t, directory)
			defer func() { _ = runtime.Close() }()
			store, err := NewDispatchStore(runtime, nil)
			if err != nil {
				t.Fatal(err)
			}
			seed := testActionRecord()
			probe.prepare(&seed)
			base, err := store.ApplyAction(
				context.Background(), fleet.DispatchMutation{
					Token: []byte("enqueue-key"), Record: seed,
				})
			if err != nil {
				t.Fatalf("the seeded record was refused, so this probe "+
					"cannot express its condition: %v", err)
			}
			next := base
			next.Version++
			next.UpdatedAt = base.UpdatedAt.Add(time.Second)
			probe.rewrite(&next)
			if err := next.Validate(); err != nil {
				t.Fatalf("the rewritten record is invalid for an unrelated "+
					"reason, so this probe cannot reach the monotonicity "+
					"check: %v", err)
			}
			if _, err := store.ApplyAction(
				context.Background(), fleet.DispatchMutation{
					Token: []byte("rewrite"), Record: next,
					ExpectedVersion: base.Version,
					ExpectedFence:   base.ClaimFence,
				}); err == nil {
				t.Fatal("the store accepted a non-monotonic rewrite")
			}
		})
	}
}

// testActionReason builds a Reason whose digest is real, because
// ActionRecord.Validate requires 64 lowercase hex characters and a hand-built
// digest is refused before any store invariant is reached.
func testActionReason(t *testing.T) interaction.Reason {
	t.Helper()
	reason, err := interaction.NewReason("operator_request", "a detail")
	if err != nil {
		t.Fatal(err)
	}
	return reason
}

// seedAdmittedGrant shapes a record as a complete admission grant, which is
// the only shape that may carry the admitted fields at all.
func seedAdmittedGrant(record *fleet.ActionRecord) {
	disclosures := sha256.Sum256([]byte("a-disclosure-set"))
	record.AdmittedEffects = fleet.Effects{fleet.EffectMutatesExternal}
	record.AdmittedDisclosures = disclosures[:]
	record.AdmittedObligation = []byte{0x01}
	record.AdmittedIdentityScheme = 1
}

// seedApproval shapes a record as one materialized from an approval. The
// provenance must be complete or entirely absent, and the approval's policy
// generation must equal the record's.
func seedApproval(record *fleet.ActionRecord) {
	digest := sha256.Sum256([]byte("an-approval-request"))
	record.ApprovalRequestDigest = digest[:]
	record.ApprovalPolicyGeneration = record.PolicyGeneration
	record.ApproverSubject = "approver"
	record.ApproverActor = "approver-actor"
	record.ApproverClientID = "approver-client"
	record.ApprovedAt = record.CreatedAt
}

// TestEveryMutatingRouteSurvivesTheIdentityInvariant drives each mutating
// route through the real store, because none of them was reaching the
// invariant in any test.
//
// A false refusal here is the dominant risk of refuseRewrittenIdentity — it
// would break a working path — and a green suite is not evidence against it if
// no test exercises the path. ExtendClaim and ReportAmbiguity in particular
// reached the durable store from nowhere in the repository: pkg/explorer/fleet
// binds a fake store, and the webapi and mcp suites bind providers. So the two
// newest routes, the two the invariant's own comment singles out as the ones a
// TransitionKind-based exemption would have missed, were unguarded.
//
// Claim, extend, report and complete in sequence, then a separate cancel, each
// asserting the version advanced — so a refusal shows up as a failure here
// rather than as a 500 in production.
func TestEveryMutatingRouteSurvivesTheIdentityInvariant(t *testing.T) {
	now := time.Date(2026, 9, 6, 12, 0, 0, 0, time.UTC)
	directory := t.TempDir()
	runtime := openFleetDispatchRuntime(t, directory)
	defer func() { _ = runtime.Close() }()
	authority, err := auth.NewAuthorityWithClock(func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	executor := &integratedExecutor{}
	registry, dispatch := composeIntegratedServicesWithRecorder(
		t, runtime, authority, executor, integratedActionRecorder{}, now,
	)
	decision := integratedDecision(t, now)
	ctx, err := authority.Binder().Bind(context.Background(), decision)
	if err != nil {
		t.Fatal(err)
	}
	descriptor := registerIntegratedAgent(t, registry, ctx, now)

	queued, err := dispatch.Enqueue(ctx, integratedEnqueue(
		now, descriptor, []byte("route-action"), []byte("route-key")))
	if err != nil {
		t.Fatal(err)
	}
	claimed, err := dispatch.Claim(ctx, fleet.ClaimRequest{
		ID: queued.ID, ExpectedVersion: queued.Version,
		ClaimID: []byte("claim"), Lease: time.Minute,
		Context: integratedContext(now),
	})
	if err != nil {
		t.Fatalf("Claim was refused through the real store: %v", err)
	}
	if claimed.Version != queued.Version+1 {
		t.Fatalf("claim version = %d", claimed.Version)
	}
	extended, err := dispatch.ExtendClaim(ctx, fleet.ExtendRequest{
		ID: queued.ID, ExpectedVersion: claimed.Version,
		ClaimID: []byte("claim"), Lease: 2 * time.Minute,
		Context: integratedContext(now),
	})
	if err != nil {
		t.Fatalf("ExtendClaim was refused through the real store, which no "+
			"other test would have caught: %v", err)
	}
	if extended.Version != claimed.Version+1 ||
		extended.ClaimFence != claimed.ClaimFence {
		t.Fatalf("extension moved the wrong things: %#v", extended)
	}
	reported, err := dispatch.ReportAmbiguity(ctx, fleet.AmbiguityRequest{
		ID: queued.ID, ClaimFence: extended.ClaimFence,
		Outcome: fleet.AmbiguityOutcomeUnknown,
		Target:  "payments.example.test", Reference: "ch_1",
		Context: integratedContext(now),
	})
	if err != nil {
		t.Fatalf("ReportAmbiguity was refused through the real store, which "+
			"no other test would have caught: %v", err)
	}
	if reported.Version != extended.Version+1 ||
		len(reported.AmbiguityReports) != 1 {
		t.Fatalf("report moved the wrong things: %#v", reported)
	}
	completed, err := dispatch.CompleteClaim(ctx, fleet.CompletionRequest{
		ID: queued.ID, ExpectedVersion: reported.Version,
		ClaimFence: reported.ClaimFence, ClaimID: []byte("claim"),
		Result:  fleet.ExecutionResult{Output: json.RawMessage(`{"ok":true}`)},
		Context: integratedContext(now),
	})
	if err != nil {
		t.Fatalf("CompleteClaim was refused through the real store: %v", err)
	}
	if completed.State != fleet.DispatchSucceeded {
		t.Fatalf("completion state = %q", completed.State)
	}

	// Cancel needs its own action, since the one above is terminal.
	second, err := dispatch.Enqueue(ctx, integratedEnqueue(
		now, descriptor, []byte("cancel-action"), []byte("cancel-key")))
	if err != nil {
		t.Fatal(err)
	}
	canceled, err := dispatch.Cancel(ctx, fleet.CancelRequest{
		ID: second.ID, ExpectedVersion: second.Version,
		MutationKey: []byte("mutation-key"),
		Context:     integratedContext(now),
	})
	if err != nil {
		t.Fatalf("Cancel was refused through the real store: %v", err)
	}
	if canceled.State != fleet.DispatchCanceled ||
		canceled.Version != second.Version+1 {
		t.Fatalf("cancellation = %#v", canceled)
	}
}

// TestAHeartbeatDoesNotStrandInFlightWork is #486, driven through the real
// store because the defect was invisible to every fake.
//
// resolveActionBinding pinned the descriptor to the generation an action was
// enqueued at, and Heartbeat advances that generation on every lease renewal.
// So a single heartbeat made every in-flight action on that agent refuse with
// ObjectNotFound — indistinguishable from an action that never existed. A
// worker that had performed an effect and heartbeated on schedule could not
// report it; a rolling restart stranded a draining replica's effect; and two
// replicas could not share one descriptor identity.
//
// Unpinning alone would have been a bypass: Update refuses changes that *grow*
// authority, but turning a requirement on is a narrowing and therefore
// permitted, so a record enqueued under the laxer rule would have escaped the
// stricter one. The requirement is now checked directly by approvalGate and
// attestationGate against the current registration, which is what makes the
// pin removable — and TestApprovalIsPerActionAndSurvivesRegistration covers
// that half.
//
// This covers the half that was broken: claim, heartbeat, then renew, report
// and complete, each of which resolves the descriptor again.
func TestAHeartbeatDoesNotStrandInFlightWork(t *testing.T) {
	now := time.Date(2026, 9, 6, 12, 0, 0, 0, time.UTC)
	directory := t.TempDir()
	runtime := openFleetDispatchRuntime(t, directory)
	defer func() { _ = runtime.Close() }()
	authority, err := auth.NewAuthorityWithClock(func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	executor := &integratedExecutor{}
	registry, dispatch := composeIntegratedServicesWithRecorder(
		t, runtime, authority, executor, integratedActionRecorder{}, now,
	)
	decision := integratedDecision(t, now)
	ctx, err := authority.Binder().Bind(context.Background(), decision)
	if err != nil {
		t.Fatal(err)
	}
	descriptor := registerIntegratedAgent(t, registry, ctx, now)

	queued, err := dispatch.Enqueue(ctx, integratedEnqueue(
		now, descriptor, []byte("heartbeat-action"), []byte("heartbeat-key")))
	if err != nil {
		t.Fatal(err)
	}
	claimed, err := dispatch.Claim(ctx, fleet.ClaimRequest{
		ID: queued.ID, ExpectedVersion: queued.Version,
		ClaimID: []byte("claim"), Lease: time.Minute,
		Context: integratedContext(now),
	})
	if err != nil {
		t.Fatal(err)
	}

	// The heartbeat needs its own operation, which integratedDecision does
	// not carry — widening that shared fixture would change what every other
	// test in this file is authorized to do.
	beat, err := auth.NewDecision(auth.DecisionConfig{
		Subject: "owner", Actor: "actor",
		AuthorizationDomain: []byte("domain"),
		AllowedOperations: []auth.Operation{
			auth.OperationAgentHeartbeat,
		},
		PermittedSourceIDs: [][]byte{[]byte("source")},
		PermittedPolicyIDs: [][]byte{[]byte("policy")}, PolicyGeneration: 1,
		AuthenticationExpires: now.Add(2 * time.Hour),
		RequestID:             "request", CorrelationID: "correlation",
	})
	if err != nil {
		t.Fatal(err)
	}
	beatCtx, err := authority.Binder().Bind(context.Background(), beat)
	if err != nil {
		t.Fatal(err)
	}

	// This is the whole premise: the heartbeat must move the generation, or
	// the test proves nothing — the descriptor store requires every mutation
	// to advance it by exactly one.
	renewed, err := registry.Heartbeat(beatCtx, fleet.HeartbeatRequest{
		Context: integratedContext(now), RegistrationKey: "beat-once",
		ID: descriptor.ID, ExpectedGeneration: descriptor.Generation,
		LeaseExpiresAt: now.Add(2 * time.Hour),
	})
	if err != nil {
		t.Fatalf("the heartbeat this test needs was refused: %v", err)
	}
	if renewed.Generation == descriptor.Generation {
		t.Fatalf("the heartbeat left the generation at %d, so nothing below "+
			"exercises the pin and this test cannot fail",
			renewed.Generation)
	}

	// Everything the worker does next resolves the descriptor again.
	extended, err := dispatch.ExtendClaim(ctx, fleet.ExtendRequest{
		ID: queued.ID, ExpectedVersion: claimed.Version,
		ClaimID: []byte("claim"), Lease: 2 * time.Minute,
		Context: integratedContext(now),
	})
	if err != nil {
		t.Fatalf("a renewal after a heartbeat was refused, so a long "+
			"operation cannot survive its own liveness signal: %v", err)
	}
	reported, err := dispatch.ReportAmbiguity(ctx, fleet.AmbiguityRequest{
		ID: queued.ID, ClaimFence: extended.ClaimFence,
		Outcome: fleet.AmbiguityOutcomeUnknown,
		Context: integratedContext(now),
	})
	if err != nil {
		t.Fatalf("an ambiguity report after a heartbeat was refused, so a "+
			"worker cannot record an effect it may have caused: %v", err)
	}
	completed, err := dispatch.CompleteClaim(ctx, fleet.CompletionRequest{
		ID: queued.ID, ExpectedVersion: reported.Version,
		ClaimFence: reported.ClaimFence, ClaimID: []byte("claim"),
		Result:  fleet.ExecutionResult{Output: json.RawMessage(`{"ok":true}`)},
		Context: integratedContext(now),
	})
	if err != nil {
		t.Fatalf("a completion after a heartbeat was refused, so a worker "+
			"that performed an effect cannot report it: %v", err)
	}
	if completed.State != fleet.DispatchSucceeded {
		t.Fatalf("completion state = %q", completed.State)
	}

	// And Pull still offers an unclaimed record on the same agent, which is
	// how a second worker finds work after the first heartbeated.
	// The enqueue pin is still exact, and this proves it both ways: the
	// pre-heartbeat generation is refused, and the renewed one is accepted.
	// A dispatcher asserting freshness about the descriptor it read should
	// fail if the descriptor moved underneath, which is the one place the
	// exact comparison is the right question.
	if _, err := dispatch.Enqueue(ctx, integratedEnqueue(
		now, descriptor, []byte("heartbeat-stale"), []byte("stale-key")),
	); err == nil {
		t.Fatal("an enqueue naming the pre-heartbeat generation was " +
			"accepted, so the freshness assertion at enqueue is gone too")
	}
	second, err := dispatch.Enqueue(ctx, integratedEnqueue(
		now, renewed, []byte("heartbeat-second"), []byte("second-key")))
	if err != nil {
		t.Fatalf("an enqueue naming the current generation was refused: %v",
			err)
	}
	page, err := dispatch.Pull(ctx, fleet.PullActionsRequest{
		Limit: fleet.MaxDispatchListResults, Context: integratedContext(now),
	})
	if err != nil {
		t.Fatal(err)
	}
	var offered bool
	for _, action := range page.Actions {
		if string(action.ID) == string(second.ID) {
			offered = true
		}
	}
	if !offered {
		t.Fatal("Pull no longer offers work on an agent that has " +
			"heartbeated, so a worker sees an empty page for staying alive")
	}
}
