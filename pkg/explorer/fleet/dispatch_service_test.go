// Licensed to the Apache Software Foundation (ASF) under one or more
// contributor license agreements. See the NOTICE file distributed with this
// work for additional information regarding copyright ownership.
package fleet

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/phrocker/shoal-oss/pkg/explorer/auth"
	"github.com/phrocker/shoal-oss/pkg/interaction"
	"github.com/phrocker/shoal-oss/pkg/ontology"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

func TestValidateExecutionEvidenceRequiresCompletePinnedAnchors(t *testing.T) {
	now := time.Date(2026, 9, 6, 12, 0, 0, 0, time.UTC)
	result := ExecutionResult{
		EvidenceSnapshotID: "snapshot", EvidenceSnapshotAsOf: now.Add(-time.Minute),
		Evidence: []EvidenceRef{{
			AnchorID: "anchor", Kind: interaction.EvidenceGraph,
			NodeIDs: []shoal.ID{"left", "right"}, EdgeIDs: []shoal.ID{"edge"},
			Assertions: []interaction.AssertionReference{{
				AssertionID: "assertion", EdgeID: "edge",
				Origin: ontology.AssertionExplicit,
			}},
			Visibility: []string{"restricted"},
		}},
	}
	if err := validateExecutionEvidence(result, now, now.Add(time.Hour)); err != nil {
		t.Fatalf("complete pinned evidence = %v", err)
	}
	missingPin := result
	missingPin.EvidenceSnapshotID = ""
	if err := validateExecutionEvidence(
		missingPin, now, now.Add(time.Hour),
	); err == nil {
		t.Fatal("accepted evidence without a snapshot ID")
	}
	future := result
	future.EvidenceSnapshotAsOf = now.Add(time.Second)
	if err := validateExecutionEvidence(
		future, now, now.Add(time.Hour),
	); err == nil {
		t.Fatal("accepted a future evidence snapshot")
	}
}

func TestDispatchInvokeFreshAuthorizationAndAmbiguousRecording(t *testing.T) {
	now := time.Date(2026, 9, 6, 12, 0, 0, 0, time.UTC)
	authority, _ := auth.NewAuthorityWithClock(func() time.Time { return now })
	registryStore := newMemoryStore()
	executor := &dispatchExecutor{result: ExecutionResult{Output: json.RawMessage(`{"ok":true}`)}}
	registry, err := NewService(Config{
		Store: registryStore, Resolver: authority.Resolver(), Recorder: &memoryRecorder{},
		Snapshots: fixedSnapshot{now},
		Executors: executorMap{"exec": executor}, Clock: func() time.Time { return now },
	})
	if err != nil {
		t.Fatal(err)
	}

	registryStore.records["agent"] = Stored{Descriptor: dispatchDescriptor(now)}
	store := newMemoryDispatchStore()
	recorder := &dispatchRecorder{failPhase: "effect_outcome"}
	service, err := NewDispatchService(DispatchConfig{
		Store: store, Registry: registry, Resolver: authority.Resolver(),
		Recorder: recorder, Events: dispatchEvents{}, Clock: func() time.Time { return now },
	})
	if err != nil {
		t.Fatal(err)
	}
	decision := dispatchDecision(t, "owner", "actor", "request", auth.OperationDispatch, auth.OperationInvoke)
	ctx := bindDecision(t, authority, decision)
	_, err = service.Invoke(ctx, InvokeRequest{
		Enqueue: dispatchEnqueue(now, "request"), ClaimID: []byte("claim"), Lease: time.Minute,
	})
	if !errors.Is(err, ErrExecutionAmbiguous) || !errors.Is(err, ErrRecordingUnavailable) {
		t.Fatalf("invoke error = %v", err)
	}
	if executor.calls != 1 {
		t.Fatalf("executor calls = %d", executor.calls)
	}
	if len(executor.invocations) != 1 ||
		!bytes.Equal(executor.invocations[0].SourceID, []byte("source")) ||
		!bytes.Equal(executor.invocations[0].PolicyID, []byte("policy")) ||
		executor.invocations[0].ObjectID != "object" {
		t.Fatalf("executor target = %#v", executor.invocations)
	}
	stored, err := store.GetAction(ctx, []byte("action"))
	if err != nil || stored.State != DispatchClaimed || stored.EffectPossible {
		t.Fatalf("stored = %#v, %v", stored, err)
	}
	recorder.failPhase = ""
	retryDecision := dispatchDecision(t, "owner", "actor", "retry", auth.OperationDispatch, auth.OperationInvoke)
	retryCtx := bindDecision(t, authority, retryDecision)
	completed, err := service.Invoke(retryCtx, InvokeRequest{
		Enqueue: dispatchEnqueue(now, "retry"), ClaimID: []byte("claim"), Lease: time.Minute,
	})
	if err != nil || completed.State != DispatchSucceeded {
		t.Fatalf("retry = %#v, %v", completed, err)
	}
	if len(executor.keys) != 2 ||
		string(executor.keys[0]) != string(executor.keys[1]) ||
		string(executor.keys[0]) != string(stored.ExecutorKey) {
		t.Fatalf("executor key was not stable: %#v", executor.keys)
	}
}

func TestDispatchInvokeOnlyAuthorizationEnqueuesAndExecutes(t *testing.T) {
	now := time.Date(2026, 9, 6, 12, 0, 0, 0, time.UTC)
	authority, _ := auth.NewAuthorityWithClock(func() time.Time { return now })
	registryStore := newMemoryStore()
	registry, _ := NewService(Config{
		Store: registryStore, Resolver: authority.Resolver(),
		Recorder: &memoryRecorder{}, Snapshots: fixedSnapshot{now},
		Executors: executorMap{"exec": &dispatchExecutor{
			result: ExecutionResult{Output: json.RawMessage(`{"ok":true}`)},
		}},
		Clock: func() time.Time { return now },
	})
	registryStore.records["agent"] = Stored{Descriptor: dispatchDescriptor(now)}
	service, _ := NewDispatchService(DispatchConfig{
		Store: newMemoryDispatchStore(), Registry: registry,
		Resolver: authority.Resolver(), Recorder: &dispatchRecorder{},
		Events: dispatchEvents{}, Clock: func() time.Time { return now },
	})
	decision := dispatchDecision(t, "owner", "actor", "request", auth.OperationInvoke)
	ctx := bindDecision(t, authority, decision)
	result, err := service.Invoke(ctx, InvokeRequest{
		Enqueue: dispatchEnqueue(now, "request"), ClaimID: []byte("claim"),
		Lease: time.Minute,
	})
	if err != nil || result.State != DispatchSucceeded {
		t.Fatalf("invoke-only dispatch = %#v, %v", result, err)
	}
}

func TestTeamActionsSharesScopedStateWithoutPrincipalLeak(t *testing.T) {
	now := time.Date(2026, 9, 6, 12, 0, 0, 0, time.UTC)
	authority, _ := auth.NewAuthorityWithClock(func() time.Time { return now })
	registry, _ := NewService(Config{
		Store: newMemoryStore(), Resolver: authority.Resolver(),
		Recorder: &memoryRecorder{}, Snapshots: fixedSnapshot{now},
		Executors: executorMap{}, Clock: func() time.Time { return now },
	})
	store := newMemoryDispatchStore()
	store.records["visible-action"] = ActionRecord{
		ID: []byte("visible-action"), State: DispatchSucceeded,
		AgentID: "agent", SourceID: []byte("source"),
		PolicyID: []byte("policy"), ObjectID: "object",
		Subject: "alice", Actor: "alice",
	}
	store.records["hidden-action"] = ActionRecord{
		ID: []byte("hidden-action"), State: DispatchFailed,
		AgentID: "agent", SourceID: []byte("hidden"),
		PolicyID: []byte("hidden"), ObjectID: "object",
		Subject: "alice", Actor: "alice",
	}
	service, _ := NewDispatchService(DispatchConfig{
		Store: store, Registry: registry, Resolver: authority.Resolver(),
		Recorder: &dispatchRecorder{}, Events: dispatchEvents{},
		Clock: func() time.Time { return now },
	})
	decision := dispatchDecision(
		t, "bob", "bob", "request", auth.OperationTeamOverviewRead)
	ctx := bindDecision(t, authority, decision)
	page, err := service.TeamActions(ctx, TeamActionListRequest{
		Limit: 10, SourceIDs: [][]byte{[]byte("source")},
		PolicyIDs: [][]byte{[]byte("policy")},
		ObjectIDs: []shoal.ID{"object"}, AgentIDs: []shoal.ID{"agent"},
		Context: RequestContext{
			RequestID: "request", CorrelationID: "correlation",
			ReasonCode: "team_overview", Deadline: now.Add(time.Hour),
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Actions) != 1 ||
		string(page.Actions[0].ID) != "visible-action" {
		t.Fatalf("visible actions = %#v", page.Actions)
	}

	denied := dispatchDecision(
		t, "mallory", "mallory", "denied", auth.OperationTeamOverviewRead)
	deniedConfig := auth.DecisionConfig{
		Subject: "mallory", Actor: "mallory",
		AuthorizationDomain: []byte("domain"),
		AllowedOperations:   []auth.Operation{auth.OperationTeamOverviewRead},
		PermittedSourceIDs:  [][]byte{[]byte("other")},
		PermittedPolicyIDs:  [][]byte{[]byte("other")},
		PolicyGeneration:    1, AuthenticationExpires: now.Add(time.Hour),
		RequestID: "denied", CorrelationID: "correlation",
	}
	denied, err = auth.NewDecision(deniedConfig)
	if err != nil {
		t.Fatal(err)
	}
	deniedCtx := bindDecision(t, authority, denied)
	page, err = service.TeamActions(deniedCtx, TeamActionListRequest{
		Limit: 10, SourceIDs: [][]byte{[]byte("source")},
		PolicyIDs: [][]byte{[]byte("policy")},
		Context: RequestContext{
			RequestID: "denied", CorrelationID: "correlation",
			ReasonCode: "team_overview", Deadline: now.Add(time.Hour),
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Actions) != 0 {
		t.Fatalf("unauthorized actions = %#v", page.Actions)
	}
}

func TestDispatchFailsClosedBeforeEffectAndOnRevokedLease(t *testing.T) {
	now := time.Date(2026, 9, 6, 12, 0, 0, 0, time.UTC)
	authority, _ := auth.NewAuthorityWithClock(func() time.Time { return now })
	registryStore := newMemoryStore()
	executor := &dispatchExecutor{result: ExecutionResult{Output: json.RawMessage(`{"ok":true}`)}}
	registry, _ := NewService(Config{
		Store: registryStore, Resolver: authority.Resolver(), Recorder: &memoryRecorder{},
		Snapshots: fixedSnapshot{now},
		Executors: executorMap{"exec": executor}, Clock: func() time.Time { return now },
	})
	registryStore.records["agent"] = Stored{Descriptor: dispatchDescriptor(now)}
	store := newMemoryDispatchStore()
	recorder := &dispatchRecorder{}
	service, _ := NewDispatchService(DispatchConfig{
		Store: store, Registry: registry, Resolver: authority.Resolver(),
		Recorder: recorder, Events: dispatchEvents{}, Clock: func() time.Time { return now },
	})
	decision := dispatchDecision(t, "owner", "actor", "request", auth.OperationDispatch, auth.OperationInvoke)
	ctx := bindDecision(t, authority, decision)
	queued, err := service.Enqueue(ctx, dispatchEnqueue(now, "request"))
	if err != nil {
		t.Fatal(err)
	}

	claimed, err := service.Claim(ctx, ClaimRequest{
		ID: queued.ID, ExpectedVersion: queued.Version, ClaimID: []byte("claim"),
		Lease: time.Minute, Context: dispatchContext(now, "request"),
	})
	if err != nil {
		t.Fatal(err)
	}
	descriptor := registryStore.records["agent"]
	descriptor.Descriptor.RevokedAt = now
	registryStore.records["agent"] = descriptor
	if _, err := service.ExecuteClaim(ctx, claimed); !shoal.IsErrorCode(err, shoal.ErrorNotFound) {
		t.Fatalf("revoked execution = %v", err)
	}
	if executor.calls != 0 {
		t.Fatalf("revoked executor calls = %d", executor.calls)
	}
	descriptor.Descriptor.RevokedAt = time.Time{}
	registryStore.records["agent"] = descriptor
	recorder.failPhase = "effect_admission"
	if _, err := service.ExecuteClaim(ctx, claimed); !errors.Is(err, ErrRecordingUnavailable) {
		t.Fatalf("admission recording = %v", err)
	}
	if executor.calls != 0 {
		t.Fatalf("recording failure executor calls = %d", executor.calls)
	}
}

func TestDispatchFencesExecutorWhenClaimExpiresDuringEffect(t *testing.T) {
	now := time.Date(2026, 9, 6, 12, 0, 0, 0, time.UTC)
	clock := now
	authority, _ := auth.NewAuthorityWithClock(func() time.Time { return clock })
	registryStore := newMemoryStore()
	executor := &dispatchExecutor{
		result:  ExecutionResult{Output: json.RawMessage(`{"ok":true}`)},
		advance: func() { clock = clock.Add(2 * time.Minute) },
	}
	registry, _ := NewService(Config{
		Store: registryStore, Resolver: authority.Resolver(), Recorder: &memoryRecorder{},
		Snapshots: fixedSnapshot{now},
		Executors: executorMap{"exec": executor}, Clock: func() time.Time { return clock },
	})
	registryStore.records["agent"] = Stored{Descriptor: dispatchDescriptor(now)}
	store := newMemoryDispatchStore()
	service, _ := NewDispatchService(DispatchConfig{
		Store: store, Registry: registry, Resolver: authority.Resolver(),
		Recorder: &dispatchRecorder{}, Events: dispatchEvents{}, Clock: func() time.Time { return clock },
	})
	decision := dispatchDecision(t, "owner", "actor", "request", auth.OperationDispatch, auth.OperationInvoke)
	ctx := bindDecision(t, authority, decision)
	queued, err := service.Enqueue(ctx, dispatchEnqueue(now, "request"))
	if err != nil {
		t.Fatal(err)
	}
	claimed, err := service.Claim(ctx, ClaimRequest{
		ID: queued.ID, ExpectedVersion: queued.Version, ClaimID: []byte("claim"),
		Lease: time.Minute, Context: dispatchContext(now, "request"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.ExecuteClaim(ctx, claimed); !errors.Is(err, ErrExecutionAmbiguous) ||
		!errors.Is(err, ErrClaimLost) {
		t.Fatalf("expired execution = %v", err)
	}
	current, err := store.GetAction(ctx, queued.ID)
	if err != nil || current.State != DispatchClaimed || current.EffectPossible {
		t.Fatalf("expired state = %#v, %v", current, err)
	}
}

func TestDispatchAuthorizationReplayConflictAndCancellationFence(t *testing.T) {
	now := time.Date(2026, 9, 6, 12, 0, 0, 0, time.UTC)
	authority, _ := auth.NewAuthorityWithClock(func() time.Time { return now })
	registryStore := newMemoryStore()
	registry, _ := NewService(Config{
		Store: registryStore, Resolver: authority.Resolver(), Recorder: &memoryRecorder{},
		Snapshots: fixedSnapshot{now},
		Executors: executorMap{"exec": &dispatchExecutor{}}, Clock: func() time.Time { return now },
	})
	registryStore.records["agent"] = Stored{Descriptor: dispatchDescriptor(now)}
	store := newMemoryDispatchStore()
	service, _ := NewDispatchService(DispatchConfig{
		Store: store, Registry: registry, Resolver: authority.Resolver(),
		Recorder: &dispatchRecorder{}, Events: dispatchEvents{}, Clock: func() time.Time { return now },
	})
	denied := dispatchDecision(t, "owner", "actor", "request", auth.OperationInvoke)
	deniedCtx := bindDecision(t, authority, denied)
	if _, err := service.Enqueue(deniedCtx, dispatchEnqueue(now, "request")); !shoal.IsErrorCode(err, shoal.ErrorUnauthorized) {
		t.Fatalf("denied enqueue = %v", err)
	}

	delegatedWithoutGrant, err := auth.NewDecision(auth.DecisionConfig{
		Subject: "owner", Actor: "actor", OnBehalfOf: []shoal.ID{"principal"},
		AuthorizationDomain: []byte("domain"),
		AllowedOperations:   []auth.Operation{auth.OperationDispatch},
		PermittedSourceIDs:  [][]byte{[]byte("source")},
		PermittedPolicyIDs:  [][]byte{[]byte("policy")}, PolicyGeneration: 1,
		AuthenticationExpires: now.Add(time.Hour), RequestID: "request",
		CorrelationID: "correlation",
	})
	if err != nil {
		t.Fatal(err)
	}
	delegatedCtx := bindDecision(t, authority, delegatedWithoutGrant)
	// Unauthorized, and honestly so.
	//
	// This was the existence oracle #536 reports — not because the code is
	// unauthorized, but because the check ran *after* the descriptor lookup,
	// so reaching it implied the agent existed while a nonexistent one
	// answered not-found. The fix moved the caller-only checks ahead of the
	// lookup, so this answer is now identical whether or not the agent
	// exists, and it can say what it actually is: this caller holds no
	// delegate grant.
	//
	// TestAnInvokeHolderWithoutStandingCannotReachTheAdmissionGate asserts
	// the identity that licenses it.
	if _, err := service.Enqueue(delegatedCtx, dispatchEnqueue(now, "request")); !shoal.IsErrorCode(err, shoal.ErrorUnauthorized) {
		t.Fatalf("delegation without grant = %v, want unauthorized", err)
	}
	allowed := dispatchDecision(t, "owner", "actor", "request", auth.OperationDispatch, auth.OperationInvoke)
	ctx := bindDecision(t, authority, allowed)
	first, err := service.Enqueue(ctx, dispatchEnqueue(now, "request"))
	if err != nil {
		t.Fatal(err)
	}
	replayed, err := service.Enqueue(ctx, dispatchEnqueue(now, "request"))
	if err != nil || replayed.Version != first.Version {
		t.Fatalf("replay = %#v, %v", replayed, err)
	}
	divergent := dispatchEnqueue(now, "request")
	divergent.Input = json.RawMessage(`{"value":2}`)
	if _, err := service.Enqueue(ctx, divergent); !errors.Is(err, ErrActionConflict) {
		t.Fatalf("divergent replay = %v", err)
	}
	claimed, err := service.Claim(ctx, ClaimRequest{
		ID: first.ID, ExpectedVersion: first.Version, ClaimID: []byte("claim"),
		Lease: time.Minute, Context: dispatchContext(now, "request"),
	})
	if err != nil {
		t.Fatal(err)
	}
	replayedClaim, err := service.Claim(ctx, ClaimRequest{
		ID: first.ID, ExpectedVersion: first.Version, ClaimID: []byte("claim"),
		Lease: time.Minute, Context: dispatchContext(now, "request"),
	})
	if err != nil || replayedClaim.Version != claimed.Version {
		t.Fatalf("claim replay = %#v, %v", replayedClaim, err)
	}
	if _, err := service.Claim(ctx, ClaimRequest{
		ID: first.ID, ExpectedVersion: first.Version, ClaimID: []byte("claim"),
		Lease: 2 * time.Minute, Context: dispatchContext(now, "request"),
	}); !errors.Is(err, ErrActionConflict) {
		t.Fatalf("divergent claim replay = %v", err)
	}
	if _, err := service.Cancel(ctx, CancelRequest{
		ID: first.ID, ExpectedVersion: claimed.Version,
		MutationKey: []byte("cancel-key"), Context: dispatchContext(now, "request"),
	}); !errors.Is(err, ErrActionConflict) {
		t.Fatalf("cancel active claim = %v", err)
	}
	now = now.Add(2 * time.Minute)
	canceled, err := service.Cancel(ctx, CancelRequest{
		ID: first.ID, ExpectedVersion: claimed.Version,
		MutationKey: []byte("cancel-key"), Context: dispatchContext(now, "request"),
	})
	if err != nil || canceled.State != DispatchCanceled {
		t.Fatalf("cancel = %#v, %v", canceled, err)
	}
	replayedCancel, err := service.Cancel(ctx, CancelRequest{
		ID: first.ID, ExpectedVersion: claimed.Version,
		MutationKey: []byte("cancel-key"), Context: dispatchContext(now, "request"),
	})
	if err != nil || replayedCancel.Version != canceled.Version {
		t.Fatalf("cancel replay = %#v, %v", replayedCancel, err)
	}
	if _, err := service.Cancel(ctx, CancelRequest{
		ID: first.ID, ExpectedVersion: claimed.Version,
		MutationKey: []byte("different-key"), Context: dispatchContext(now, "request"),
	}); !errors.Is(err, ErrActionConflict) {
		t.Fatalf("divergent cancel replay = %v", err)
	}
	other := dispatchDecision(t, "other", "other-actor", "request", auth.OperationDispatch)
	otherCtx := bindDecision(t, authority, other)
	if _, err := service.Status(otherCtx, StatusRequest{
		ID: first.ID, Context: dispatchContext(now, "request"),
	}); !shoal.IsErrorCode(err, shoal.ErrorNotFound) {
		t.Fatalf("cross-principal status = %v", err)
	}
}

func TestDispatchCommittedEventAmbiguityReconcilesOnRetry(t *testing.T) {
	now := time.Date(2026, 9, 6, 12, 0, 0, 0, time.UTC)
	authority, _ := auth.NewAuthorityWithClock(func() time.Time { return now })
	registryStore := newMemoryStore()
	registry, _ := NewService(Config{
		Store: registryStore, Resolver: authority.Resolver(), Recorder: &memoryRecorder{},
		Snapshots: fixedSnapshot{now},
		Executors: executorMap{"exec": &dispatchExecutor{}}, Clock: func() time.Time { return now },
	})
	registryStore.records["agent"] = Stored{Descriptor: dispatchDescriptor(now)}
	store := newMemoryDispatchStore()
	events := &controlledDispatchEvents{err: errors.New("event unavailable")}
	service, _ := NewDispatchService(DispatchConfig{
		Store: store, Registry: registry, Resolver: authority.Resolver(),
		Recorder: &dispatchRecorder{}, Events: events, Clock: func() time.Time { return now },
	})
	decision := dispatchDecision(t, "owner", "actor", "request", auth.OperationDispatch)
	ctx := bindDecision(t, authority, decision)
	request := dispatchEnqueue(now, "request")
	if _, err := service.Enqueue(ctx, request); !errors.Is(err, ErrActionCommitted) {
		t.Fatalf("event failure = %v", err)
	}
	if current, err := store.GetAction(ctx, request.ID); err != nil || current.State != DispatchQueued {
		t.Fatalf("committed enqueue = %#v, %v", current, err)
	}
	events.err = nil
	reconciled, err := service.Enqueue(ctx, request)
	if err != nil || reconciled.State != DispatchQueued || events.calls != 2 {
		t.Fatalf("reconciled = %#v calls=%d err=%v", reconciled, events.calls, err)
	}
}

func TestDispatchTransitionProvenanceSurvivesFreshRetryAndPolicyRefresh(
	t *testing.T,
) {
	now := time.Date(2026, 9, 6, 12, 0, 0, 0, time.UTC)
	authority, _ := auth.NewAuthorityWithClock(func() time.Time { return now })
	registryStore := newMemoryStore()
	registry, _ := NewService(Config{
		Store: registryStore, Resolver: authority.Resolver(),
		Recorder: &memoryRecorder{}, Snapshots: fixedSnapshot{now},
		Executors: executorMap{"exec": &dispatchExecutor{}},
		Clock:     func() time.Time { return now },
	})
	registryStore.records["agent"] = Stored{Descriptor: dispatchDescriptor(now)}
	store := newMemoryDispatchStore()
	events := &controlledDispatchEvents{}
	service, _ := NewDispatchService(DispatchConfig{
		Store: store, Registry: registry, Resolver: authority.Resolver(),
		Recorder: &dispatchRecorder{}, Events: events,
		Clock: func() time.Time { return now },
	})

	enqueueDecision := dispatchDecisionWithProvenance(
		t, now, "enqueue-request", "enqueue-correlation", 1, now.Add(time.Hour),
	)
	enqueueCtx := bindDecision(t, authority, enqueueDecision)
	queued, err := service.Enqueue(
		enqueueCtx, dispatchEnqueueWithContext(
			now, "enqueue-request", "enqueue-correlation",
		),
	)
	if err != nil {
		t.Fatal(err)
	}

	events.err = errors.New("event unavailable")
	claimDecision := dispatchDecisionWithProvenance(
		t, now, "claim-request", "claim-correlation", 1, now.Add(time.Hour),
	)
	claimCtx := bindDecision(t, authority, claimDecision)
	claimRequest := ClaimRequest{
		ID: queued.ID, ExpectedVersion: queued.Version, ClaimID: []byte("claim"),
		Lease: time.Minute, Context: RequestContext{
			RequestID: "claim-request", CorrelationID: "claim-correlation",
			ReasonCode: "worker_claim", Deadline: now.Add(time.Hour),
		},
	}
	if _, err := service.Claim(claimCtx, claimRequest); !errors.Is(
		err, ErrActionCommitted,
	) {
		t.Fatalf("claim event ambiguity = %v", err)
	}
	claimed, err := store.GetAction(claimCtx, queued.ID)
	if err != nil {
		t.Fatal(err)
	}
	claimProvenance := claimed.EventProvenance()
	claimFingerprint, _ := auth.AuthorizationFingerprint(claimDecision)
	if claimProvenance.RequestID != "claim-request" ||
		claimProvenance.CorrelationID != "claim-correlation" ||
		claimProvenance.AuthorizationFingerprint != claimFingerprint ||
		!claimProvenance.AuthorizationExpiresAt.Equal(
			claimDecision.AuthenticationExpires(),
		) {
		t.Fatalf("claim provenance = %#v", claimProvenance)
	}

	events.err = nil
	retryDecision := dispatchDecisionWithProvenance(
		t, now, "claim-retry", "claim-retry-correlation", 2, now.Add(2*time.Hour),
	)
	retryCtx := bindDecision(t, authority, retryDecision)
	claimRequest.Context = RequestContext{
		RequestID: "claim-retry", CorrelationID: "claim-retry-correlation",
		ReasonCode: "worker_claim_retry", Deadline: now.Add(time.Hour),
	}
	replayed, err := service.Claim(retryCtx, claimRequest)
	if err != nil || replayed.EventProvenance() != claimProvenance {
		t.Fatalf("claim retry provenance = %#v, %v", replayed.EventProvenance(), err)
	}

	now = now.Add(2 * time.Minute)
	cancelDecision := dispatchDecisionWithProvenance(
		t, now, "cancel-request", "cancel-correlation", 3, now.Add(3*time.Hour),
	)
	cancelCtx := bindDecision(t, authority, cancelDecision)
	events.err = errors.New("event unavailable")
	cancelRequest := CancelRequest{
		ID: queued.ID, ExpectedVersion: claimed.Version,
		MutationKey: []byte("cancel"), Context: RequestContext{
			RequestID: "cancel-request", CorrelationID: "cancel-correlation",
			ReasonCode: "policy_refresh", Deadline: now.Add(time.Hour),
		},
	}
	if _, err := service.Cancel(cancelCtx, cancelRequest); !errors.Is(
		err, ErrActionCommitted,
	) {
		t.Fatalf("cancel event ambiguity = %v", err)
	}
	canceled, err := store.GetAction(cancelCtx, queued.ID)
	if err != nil {
		t.Fatal(err)
	}
	cancelProvenance := canceled.EventProvenance()
	cancelFingerprint, _ := auth.AuthorizationFingerprint(cancelDecision)
	if cancelProvenance.RequestID != "cancel-request" ||
		cancelProvenance.CorrelationID != "cancel-correlation" ||
		cancelProvenance.AuthorizationFingerprint != cancelFingerprint ||
		!cancelProvenance.AuthorizationExpiresAt.Equal(
			cancelDecision.AuthenticationExpires(),
		) {
		t.Fatalf("cancel provenance = %#v", cancelProvenance)
	}

	events.err = nil
	cancelRetryDecision := dispatchDecisionWithProvenance(
		t, now, "cancel-retry", "cancel-retry-correlation", 4, now.Add(4*time.Hour),
	)
	cancelRetryCtx := bindDecision(t, authority, cancelRetryDecision)
	cancelRequest.Context = RequestContext{
		RequestID: "cancel-retry", CorrelationID: "cancel-retry-correlation",
		ReasonCode: "retry", Deadline: now.Add(time.Hour),
	}
	replayedCancel, err := service.Cancel(cancelRetryCtx, cancelRequest)
	if err != nil || replayedCancel.EventProvenance() != cancelProvenance {
		t.Fatalf(
			"cancel retry provenance = %#v, %v",
			replayedCancel.EventProvenance(), err,
		)
	}
}

func TestDispatchCancelAddsDispatchAuthorizationToInvokeAction(t *testing.T) {
	now := time.Date(2026, 9, 6, 12, 0, 0, 0, time.UTC)
	authority, _ := auth.NewAuthorityWithClock(func() time.Time { return now })
	registryStore := newMemoryStore()
	registry, _ := NewService(Config{
		Store: registryStore, Resolver: authority.Resolver(),
		Recorder: &memoryRecorder{}, Snapshots: fixedSnapshot{now},
		Executors: executorMap{"exec": &dispatchExecutor{}},
		Clock:     func() time.Time { return now },
	})
	registryStore.records["agent"] = Stored{Descriptor: dispatchDescriptor(now)}
	store := newMemoryDispatchStore()
	service, _ := NewDispatchService(DispatchConfig{
		Store: store, Registry: registry, Resolver: authority.Resolver(),
		Recorder: &dispatchRecorder{}, Events: dispatchEvents{},
		Clock: func() time.Time { return now },
	})
	invokeDecision := dispatchDecision(
		t, "owner", "actor", "invoke-request", auth.OperationInvoke,
	)
	invokeCtx := bindDecision(t, authority, invokeDecision)
	queued, err := service.enqueue(
		invokeCtx, dispatchEnqueue(now, "invoke-request"), auth.OperationInvoke,
		false,
	)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(
		queued.AuthorizedOperations, []auth.Operation{auth.OperationInvoke},
	) {
		t.Fatalf("invoke operations = %#v", queued.AuthorizedOperations)
	}
	cancelDecision := dispatchDecision(
		t, "owner", "actor", "cancel-request", auth.OperationDispatch,
	)
	cancelCtx := bindDecision(t, authority, cancelDecision)
	canceled, err := service.Cancel(cancelCtx, CancelRequest{
		ID: queued.ID, ExpectedVersion: queued.Version,
		MutationKey: []byte("cancel"), Context: dispatchContext(
			now, "cancel-request",
		),
	})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(canceled.AuthorizedOperations, []auth.Operation{
		auth.OperationDispatch, auth.OperationInvoke,
	}) {
		t.Fatalf("cancel operations = %#v", canceled.AuthorizedOperations)
	}
}

func TestDispatchTerminalEventReconcilesOnInvokeRetry(t *testing.T) {
	now := time.Date(2026, 9, 6, 12, 0, 0, 0, time.UTC)
	authority, _ := auth.NewAuthorityWithClock(func() time.Time { return now })
	registryStore := newMemoryStore()
	registry, _ := NewService(Config{
		Store: registryStore, Resolver: authority.Resolver(),
		Recorder: &memoryRecorder{}, Snapshots: fixedSnapshot{now},
		Executors: executorMap{"exec": &dispatchExecutor{
			result: ExecutionResult{Output: json.RawMessage(`{"ok":true}`)},
		}},
		Clock: func() time.Time { return now },
	})
	registryStore.records["agent"] = Stored{Descriptor: dispatchDescriptor(now)}
	events := &terminalFailEvents{err: errors.New("event unavailable")}
	service, _ := NewDispatchService(DispatchConfig{
		Store: newMemoryDispatchStore(), Registry: registry,
		Resolver: authority.Resolver(), Recorder: &dispatchRecorder{},
		Events: events, Clock: func() time.Time { return now },
	})
	decision := dispatchDecision(t, "owner", "actor", "request", auth.OperationDispatch, auth.OperationInvoke)
	ctx := bindDecision(t, authority, decision)
	request := dispatchEnqueue(now, "request")
	if _, err := service.Invoke(ctx, InvokeRequest{
		Enqueue: request, ClaimID: []byte("claim"), Lease: time.Minute,
	}); !errors.Is(err, ErrActionCommitted) {
		t.Fatalf("terminal event failure = %v", err)
	}
	events.err = nil
	completed, err := service.Invoke(ctx, InvokeRequest{
		Enqueue: request, ClaimID: []byte("claim"), Lease: time.Minute,
	})
	if err != nil || completed.State != DispatchSucceeded || events.calls != 2 {
		t.Fatalf("terminal event retry = %#v, calls=%d, err=%v", completed, events.calls, err)
	}
}

func TestDispatchOutboxPreservesTransitionsAndReconcilesAfterDeadline(t *testing.T) {
	now := time.Date(2026, 9, 6, 12, 0, 0, 0, time.UTC)
	clock := now
	authority, _ := auth.NewAuthorityWithClock(func() time.Time { return clock })
	registryStore := newMemoryStore()
	registry, _ := NewService(Config{
		Store: registryStore, Resolver: authority.Resolver(),
		Recorder: &memoryRecorder{}, Snapshots: fixedSnapshot{now},
		Executors: executorMap{"exec": &dispatchExecutor{
			result: ExecutionResult{Output: json.RawMessage(`{"ok":true}`)},
		}},
		Clock: func() time.Time { return clock },
	})
	registryStore.records["agent"] = Stored{Descriptor: dispatchDescriptor(now)}
	store := newMemoryDispatchStore()
	events := &controlledDispatchEvents{err: errors.New("event unavailable")}
	service, _ := NewDispatchService(DispatchConfig{
		Store: store, Registry: registry, Resolver: authority.Resolver(),
		Recorder: &dispatchRecorder{}, Events: events,
		Clock: func() time.Time { return clock },
	})
	decision := dispatchDecisionWithProvenance(
		t, now, "request", "correlation", 1, now.Add(2*time.Hour))
	ctx := bindDecision(t, authority, decision)
	request := dispatchEnqueue(now, "request")
	request.Context.Deadline = now.Add(time.Minute)
	if _, err := service.Enqueue(ctx, request); !errors.Is(err, ErrActionCommitted) {
		t.Fatalf("enqueue event failure = %v", err)
	}
	events.err = nil
	clock = request.Context.Deadline.Add(time.Minute)
	if err := service.ReconcileActionTransitions(ctx, request.ID); err != nil {
		t.Fatalf("deadline-independent reconciliation = %v", err)
	}
	if !reflect.DeepEqual(events.kinds, []string{
		"action.enqueued", "action.enqueued",
	}) {
		t.Fatalf("published kinds = %v", events.kinds)
	}
	page, err := store.PendingActionTransitions(
		context.Background(), request.ID, nil, MaxDispatchListResults)
	if err != nil || len(page.Transitions) != 0 {
		t.Fatalf("pending transitions = %#v, %v", page, err)
	}
}

func TestDispatchOutboxRetainsEarlierTransitionAfterStateAdvances(t *testing.T) {
	now := time.Date(2026, 9, 6, 13, 0, 0, 0, time.UTC)
	authority, _ := auth.NewAuthorityWithClock(func() time.Time { return now })
	registryStore := newMemoryStore()
	registry, _ := NewService(Config{
		Store: registryStore, Resolver: authority.Resolver(),
		Recorder: &memoryRecorder{}, Snapshots: fixedSnapshot{now},
		Executors: executorMap{"exec": &dispatchExecutor{
			result: ExecutionResult{Output: json.RawMessage(`{"ok":true}`)},
		}},
		Clock: func() time.Time { return now },
	})
	registryStore.records["agent"] = Stored{Descriptor: dispatchDescriptor(now)}
	store := newMemoryDispatchStore()
	events := &controlledDispatchEvents{}
	service, _ := NewDispatchService(DispatchConfig{
		Store: store, Registry: registry, Resolver: authority.Resolver(),
		Recorder: &dispatchRecorder{}, Events: events,
		Clock: func() time.Time { return now },
	})
	decision := dispatchDecision(
		t, "owner", "actor", "request",
		auth.OperationDispatch, auth.OperationInvoke)
	ctx := bindDecision(t, authority, decision)
	request := dispatchEnqueue(now, "request")
	queued, err := service.Enqueue(ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	events.err = errors.New("event unavailable")
	if _, err := service.Claim(ctx, ClaimRequest{
		ID: queued.ID, ExpectedVersion: queued.Version, ClaimID: []byte("claim"),
		Lease: time.Minute, Context: request.Context,
	}); !errors.Is(err, ErrActionCommitted) {
		t.Fatalf("claim publication failure = %v", err)
	}
	claimed, err := store.GetAction(ctx, queued.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.ExecuteClaim(ctx, claimed); !errors.Is(
		err, ErrActionCommitted,
	) {
		t.Fatalf("completion with earlier pending transition = %v", err)
	}
	current, err := store.GetAction(ctx, queued.ID)
	if err != nil || current.State != DispatchSucceeded {
		t.Fatalf("advanced state = %#v, %v", current, err)
	}
	events.err = nil
	if err := service.ReconcileActionTransitions(ctx, queued.ID); err != nil {
		t.Fatal(err)
	}
	got := events.kinds[len(events.kinds)-2:]
	if !reflect.DeepEqual(got, []string{
		"action.claimed", "action.completed",
	}) {
		t.Fatalf("reconciled transition order = %v (all %v)", got, events.kinds)
	}
}

func TestDispatchDoesNotExecuteExpiredClaimAfterAdmission(t *testing.T) {
	now := time.Date(2026, 9, 6, 12, 0, 0, 0, time.UTC)
	clock := now
	authority, _ := auth.NewAuthorityWithClock(func() time.Time { return clock })
	registryStore := newMemoryStore()
	executor := &dispatchExecutor{
		result: ExecutionResult{Output: json.RawMessage(`{"ok":true}`)},
	}
	registry, _ := NewService(Config{
		Store: registryStore, Resolver: authority.Resolver(),
		Recorder: &memoryRecorder{}, Snapshots: fixedSnapshot{now},
		Executors: executorMap{"exec": executor},
		Clock:     func() time.Time { return clock },
	})
	registryStore.records["agent"] = Stored{Descriptor: dispatchDescriptor(now)}
	recorder := &dispatchRecorder{advance: func() {
		clock = clock.Add(2 * time.Minute)
	}}
	service, _ := NewDispatchService(DispatchConfig{
		Store: newMemoryDispatchStore(), Registry: registry,
		Resolver: authority.Resolver(), Recorder: recorder,
		Events: dispatchEvents{}, Clock: func() time.Time { return clock },
	})
	decision := dispatchDecision(
		t, "owner", "actor", "request",
		auth.OperationDispatch, auth.OperationInvoke,
	)
	ctx := bindDecision(t, authority, decision)
	queued, err := service.Enqueue(ctx, dispatchEnqueue(now, "request"))
	if err != nil {
		t.Fatal(err)
	}
	claimed, err := service.Claim(ctx, ClaimRequest{
		ID: queued.ID, ExpectedVersion: queued.Version, ClaimID: []byte("claim"),
		Lease: time.Minute, Context: dispatchContext(now, "request"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.ExecuteClaim(ctx, claimed); !errors.Is(err, ErrClaimLost) {
		t.Fatalf("expired admission execution = %v", err)
	}
	if executor.calls != 0 {
		t.Fatalf("expired admission invoked executor %d times", executor.calls)
	}
}

func TestDispatchInvalidEvidencePersistsFailedOutcome(t *testing.T) {
	now := time.Date(2026, 9, 6, 12, 0, 0, 0, time.UTC)
	authority, _ := auth.NewAuthorityWithClock(func() time.Time { return now })
	registryStore := newMemoryStore()
	executor := &dispatchExecutor{result: ExecutionResult{
		Output: json.RawMessage(`{"ok":true}`),
		Evidence: []EvidenceRef{{
			NodeIDs: []shoal.ID{"node"},
		}},
	}}
	registry, _ := NewService(Config{
		Store: registryStore, Resolver: authority.Resolver(),
		Recorder: &memoryRecorder{}, Snapshots: fixedSnapshot{now},
		Executors: executorMap{"exec": executor},
		Clock:     func() time.Time { return now },
	})
	registryStore.records["agent"] = Stored{Descriptor: dispatchDescriptor(now)}
	store := newMemoryDispatchStore()
	service, _ := NewDispatchService(DispatchConfig{
		Store: store, Registry: registry, Resolver: authority.Resolver(),
		Recorder: &dispatchRecorder{}, Events: dispatchEvents{},
		Clock: func() time.Time { return now },
	})
	decision := dispatchDecision(
		t, "owner", "actor", "request",
		auth.OperationDispatch, auth.OperationInvoke,
	)
	ctx := bindDecision(t, authority, decision)
	result, err := service.Invoke(ctx, InvokeRequest{
		Enqueue: dispatchEnqueue(now, "request"), ClaimID: []byte("claim"),
		Lease: time.Minute,
	})
	if err == nil || result.State != DispatchFailed || len(result.Evidence) != 0 {
		t.Fatalf("invalid evidence result = %#v, %v", result, err)
	}
	stored, err := store.GetAction(ctx, result.ID)
	if err != nil || stored.State != DispatchFailed || len(stored.Evidence) != 0 {
		t.Fatalf("invalid evidence stored = %#v, %v", stored, err)
	}
}

func TestDispatchSchemaRejectsWrongKeywordTypes(t *testing.T) {
	for name, schema := range map[string]json.RawMessage{
		"type":                 json.RawMessage(`{"type":1}`),
		"properties":           json.RawMessage(`{"properties":[]}`),
		"required":             json.RawMessage(`{"required":"value"}`),
		"items":                json.RawMessage(`{"items":[]}`),
		"additionalProperties": json.RawMessage(`{"additionalProperties":"false"}`),
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := validateAgainstSchema(
				schema, json.RawMessage(`{}`), "action input", MaxActionPayloadBytes,
			); err == nil {
				t.Fatal("wrong schema keyword type was accepted")
			}
		})
	}
}

func TestDispatchSchemaInfersObjectFromObjectKeywords(t *testing.T) {
	schema := json.RawMessage(`{"properties":{"value":{"type":"integer"}},"required":["value"]}`)
	if _, err := validateAgainstSchema(
		schema, json.RawMessage(`{"value":"wrong"}`), "action input", MaxActionPayloadBytes,
	); err == nil {
		t.Fatal("implicit object schema accepted an invalid property")
	}
}

type memoryDispatchStore struct {
	mu          sync.Mutex
	records     map[string]ActionRecord
	transitions map[string]ActionTransition
	completed   map[string]bool
	// scans counts ScanActions calls, so a test can assert that stepping over
	// a region costs a bounded number of them rather than one per record.
	scans int
	// failReads makes one identity unreadable, so a test can drive the path a
	// service takes when a check it must make cannot answer.
	failReads map[string]error
	// failScan makes scanning fail, so a test can drive the path a service
	// takes when a verdict it must reach cannot be reached.
	failScan error
}

type uniqueTokenDispatchStore struct {
	*memoryDispatchStore
	tokens map[string]struct{}
}

func (s *uniqueTokenDispatchStore) ApplyAction(
	ctx context.Context,
	mutation DispatchMutation,
) (ActionRecord, error) {
	token := string(mutation.Token)
	if _, exists := s.tokens[token]; exists {
		return ActionRecord{}, ErrActionConflict
	}
	s.tokens[token] = struct{}{}
	return s.memoryDispatchStore.ApplyAction(ctx, mutation)
}

func newMemoryDispatchStore() *memoryDispatchStore {
	return &memoryDispatchStore{
		records:     make(map[string]ActionRecord),
		transitions: make(map[string]ActionTransition),
		completed:   make(map[string]bool),
	}
}

func (s *memoryDispatchStore) GetAction(_ context.Context, id []byte) (ActionRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err, failing := s.failReads[string(id)]; failing {
		return ActionRecord{}, err
	}
	record, ok := s.records[string(id)]
	if !ok {
		return ActionRecord{}, ErrActionNotFound
	}
	return cloneActionRecord(record), nil
}

func (s *memoryDispatchStore) ApplyAction(_ context.Context, mutation DispatchMutation) (ActionRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	current, exists := s.records[string(mutation.Record.ID)]
	if mutation.ExpectedVersion == 0 {
		if exists {
			if equivalentEnqueue(current, mutation.Record) {
				return cloneActionRecord(current), nil
			}
			return ActionRecord{}, ErrActionConflict
		}
	} else if !exists || current.Version != mutation.ExpectedVersion ||
		current.ClaimFence != mutation.ExpectedFence {
		return ActionRecord{}, ErrActionConflict
	}
	s.records[string(mutation.Record.ID)] = cloneActionRecord(mutation.Record)
	if mutation.TransitionKind != "" {
		transition, err := NewActionTransition(
			mutation.Token, mutation.TransitionKind, mutation.Record)
		if err != nil {
			return ActionRecord{}, err
		}
		key := string(transition.ID)
		if current, ok := s.transitions[key]; ok &&
			!reflect.DeepEqual(current, transition) {
			return ActionRecord{}, ErrActionConflict
		}
		s.transitions[key] = transition
	}
	return cloneActionRecord(mutation.Record), nil
}

func (s *memoryDispatchStore) PendingActionTransitions(
	_ context.Context, actionID, after []byte, limit int,
) (ActionTransitionPage, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var keys []string
	for key, transition := range s.transitions {
		if bytes.Equal(transition.Record.ID, actionID) &&
			key > string(after) && !s.completed[key] {
			keys = append(keys, key)
		}
	}
	sort.Slice(keys, func(i, j int) bool {
		left, right := s.transitions[keys[i]], s.transitions[keys[j]]
		if left.Record.Version != right.Record.Version {
			return left.Record.Version < right.Record.Version
		}
		return keys[i] < keys[j]
	})
	result := ActionTransitionPage{}
	for _, key := range keys {
		if len(result.Transitions) == limit {
			result.Next = append([]byte(nil), []byte(key)...)
			break
		}
		transition := s.transitions[key]
		transition.Record = cloneActionRecord(transition.Record)
		result.Transitions = append(result.Transitions, transition)
	}
	return result, nil
}

func (s *memoryDispatchStore) CompleteActionTransition(
	_ context.Context, transition ActionTransition,
) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := string(transition.ID)
	current, ok := s.transitions[key]
	if !ok || !reflect.DeepEqual(current, transition) {
		return ErrActionConflict
	}
	s.completed[key] = true
	return nil
}

// ScanActions walks records in ascending identity order.
//
// It iterated the map directly, which is to say in no order at all, and that
// is why a cursor bug in TeamActions survived the whole suite: the defect
// depended on admission identities sorting ahead of ordinary ones, and an
// unordered double cannot express "ahead". A scan double that does not scan in
// order models the one property every scan caller depends on incorrectly.
func (s *memoryDispatchStore) ScanActions(_ context.Context, after []byte, limit int) (ActionPage, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.scans++
	if s.failScan != nil {
		return ActionPage{}, s.failScan
	}
	keys := make([]string, 0, len(s.records))
	for key := range s.records {
		if key <= string(after) {
			continue
		}
		keys = append(keys, key)
	}
	sort.Strings(keys)
	result := ActionPage{}
	for _, key := range keys {
		record := s.records[key]
		result.Actions = append(result.Actions, cloneActionRecord(record))
		if len(result.Actions) == limit {
			result.Next = append([]byte(nil), record.ID...)
			break
		}
	}
	return result, nil
}

func TestActionEventKindsAreStable(t *testing.T) {
	for state, want := range map[DispatchState]string{
		DispatchQueued:    "action.enqueued",
		DispatchClaimed:   "action.claimed",
		DispatchSucceeded: "action.completed",
		DispatchFailed:    "action.failed",
		DispatchCanceled:  "action.canceled",
	} {
		if got := actionEventKind(ActionRecord{State: state}); got != want {
			t.Fatalf("state %q event kind = %q, want %q", state, got, want)
		}
	}
	if got := actionEventKind(ActionRecord{}); got != "" {
		t.Fatalf("invalid state event kind = %q", got)
	}
}

func TestDispatchTupleKeysAreUnambiguousForOpaqueBytes(t *testing.T) {
	tests := []struct {
		name                string
		firstID, firstKey   []byte
		secondID, secondKey []byte
	}{
		{
			name:    "embedded NUL",
			firstID: []byte("a\x00b"), firstKey: []byte("c"),
			secondID: []byte("a"), secondKey: []byte("b\x00c"),
		},
		{
			name:    "invalid UTF-8 with embedded NUL",
			firstID: []byte{0xff, 0, 0xfe}, firstKey: []byte{0xfd},
			secondID: []byte{0xff}, secondKey: []byte{0xfe, 0, 0xfd},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if !bytes.Equal(
				legacyDispatchTuple(test.firstID, test.firstKey),
				legacyDispatchTuple(test.secondID, test.secondKey),
			) {
				t.Fatal("regression tuple does not reproduce the legacy collision")
			}
			if bytes.Equal(
				executorKey(test.firstID, test.firstKey),
				executorKey(test.secondID, test.secondKey),
			) {
				t.Fatal("distinct executor-key tuples collided")
			}
			if bytes.Equal(
				transitionToken("claim", test.firstID, test.firstKey, 2),
				transitionToken("claim", test.secondID, test.secondKey, 2),
			) {
				t.Fatal("distinct transition-token tuples collided")
			}
		})
	}
}

func legacyDispatchTuple(actionID, discriminator []byte) []byte {
	result := append([]byte(nil), actionID...)
	result = append(result, 0)
	return append(result, discriminator...)
}

func TestDispatchExecutesPreviouslyAmbiguousActionsIndependently(t *testing.T) {
	now := time.Date(2026, 9, 6, 12, 0, 0, 0, time.UTC)
	authority, err := auth.NewAuthorityWithClock(func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	registryStore := newMemoryStore()
	executor := &dispatchExecutor{
		result: ExecutionResult{Output: json.RawMessage(`{"ok":true}`)},
	}
	registry, err := NewService(Config{
		Store: registryStore, Resolver: authority.Resolver(),
		Recorder: &memoryRecorder{}, Snapshots: fixedSnapshot{now},
		Executors: executorMap{"exec": executor},
		Clock:     func() time.Time { return now },
	})
	if err != nil {
		t.Fatal(err)
	}
	registryStore.records["agent"] = Stored{
		Descriptor: dispatchDescriptor(now),
	}
	store := newMemoryDispatchStore()
	service, err := NewDispatchService(DispatchConfig{
		Store: store, Registry: registry, Resolver: authority.Resolver(),
		Recorder: &dispatchRecorder{}, Events: dispatchEvents{},
		Clock: func() time.Time { return now },
	})
	if err != nil {
		t.Fatal(err)
	}
	decision := dispatchDecision(
		t, "owner", "actor", "request",
		auth.OperationDispatch, auth.OperationInvoke,
	)
	ctx := bindDecision(t, authority, decision)
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
		enqueue := dispatchEnqueue(now, "request")
		enqueue.ID = request.id
		enqueue.IdempotencyKey = request.key
		completed, invokeErr := service.Invoke(ctx, InvokeRequest{
			Enqueue: enqueue, ClaimID: request.claim, Lease: time.Minute,
		})
		if invokeErr != nil {
			t.Fatalf("invoke %x = %v", request.id, invokeErr)
		}
		if completed.State != DispatchSucceeded {
			t.Fatalf("invoke %x state = %q", request.id, completed.State)
		}
		stored, readErr := store.GetAction(ctx, request.id)
		if readErr != nil || stored.State != DispatchSucceeded {
			t.Fatalf("stored %x = %#v, %v", request.id, stored, readErr)
		}
	}
	if executor.calls != 2 || len(executor.keys) != 2 {
		t.Fatalf("executor calls/keys = %d/%d", executor.calls, len(executor.keys))
	}
	if bytes.Equal(executor.keys[0], executor.keys[1]) {
		t.Fatalf("distinct actions shared executor key %x", executor.keys[0])
	}
}

func TestDispatchEnqueueTokenCannotAliasClaimTransition(t *testing.T) {
	now := time.Date(2026, 9, 6, 12, 0, 0, 0, time.UTC)
	authority, err := auth.NewAuthorityWithClock(func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	registryStore := newMemoryStore()
	registry, err := NewService(Config{
		Store: registryStore, Resolver: authority.Resolver(),
		Recorder: &memoryRecorder{}, Snapshots: fixedSnapshot{now},
		Executors: executorMap{"exec": &dispatchExecutor{}},
		Clock:     func() time.Time { return now },
	})
	if err != nil {
		t.Fatal(err)
	}
	registryStore.records["agent"] = Stored{
		Descriptor: dispatchDescriptor(now),
	}
	actionID := []byte("action")
	claimID := []byte("claim")
	hostileIdempotency := transitionToken("claim", actionID, claimID, 2)
	store := &uniqueTokenDispatchStore{
		memoryDispatchStore: newMemoryDispatchStore(),
		tokens:              make(map[string]struct{}),
	}
	service, err := NewDispatchService(DispatchConfig{
		Store: store, Registry: registry, Resolver: authority.Resolver(),
		Recorder: &dispatchRecorder{}, Events: dispatchEvents{},
		Clock: func() time.Time { return now },
	})
	if err != nil {
		t.Fatal(err)
	}
	decision := dispatchDecision(
		t, "owner", "actor", "request",
		auth.OperationDispatch, auth.OperationInvoke,
	)
	ctx := bindDecision(t, authority, decision)
	enqueue := dispatchEnqueue(now, "request")
	enqueue.ID = actionID
	enqueue.IdempotencyKey = hostileIdempotency
	queued, err := service.Enqueue(ctx, enqueue)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.Claim(ctx, ClaimRequest{
		ID: queued.ID, ExpectedVersion: queued.Version,
		ClaimID: claimID, Lease: time.Minute,
		Context: dispatchContext(now, "request"),
	}); err != nil {
		t.Fatalf("claim collided with caller enqueue key: %v", err)
	}
}

func TestDispatchRestartPreservesStoredExecutorKeyAcrossAmbiguousRetry(t *testing.T) {
	now := time.Date(2026, 9, 6, 12, 0, 0, 0, time.UTC)
	authority, err := auth.NewAuthorityWithClock(func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	registryStore := newMemoryStore()
	executor := &dispatchExecutor{
		result: ExecutionResult{Output: json.RawMessage(`{"ok":true}`)},
	}
	registry, err := NewService(Config{
		Store: registryStore, Resolver: authority.Resolver(),
		Recorder: &memoryRecorder{}, Snapshots: fixedSnapshot{now},
		Executors: executorMap{"exec": executor},
		Clock:     func() time.Time { return now },
	})
	if err != nil {
		t.Fatal(err)
	}
	registryStore.records["agent"] = Stored{
		Descriptor: dispatchDescriptor(now),
	}
	store := newMemoryDispatchStore()
	newService := func(recorder ActionRecorder) *DispatchService {
		t.Helper()
		service, serviceErr := NewDispatchService(DispatchConfig{
			Store: store, Registry: registry, Resolver: authority.Resolver(),
			Recorder: recorder, Events: dispatchEvents{},
			Clock: func() time.Time { return now },
		})
		if serviceErr != nil {
			t.Fatal(serviceErr)
		}
		return service
	}
	decision := dispatchDecision(
		t, "owner", "actor", "request",
		auth.OperationDispatch, auth.OperationInvoke,
	)
	ctx := bindDecision(t, authority, decision)
	request := dispatchEnqueue(now, "request")
	if _, err := newService(&dispatchRecorder{}).Enqueue(ctx, request); err != nil {
		t.Fatal(err)
	}
	legacyKey := []byte("persisted-v1-executor-key")
	store.mu.Lock()
	stored := store.records[string(request.ID)]
	stored.ExecutorKey = append([]byte(nil), legacyKey...)
	store.records[string(request.ID)] = stored
	store.mu.Unlock()

	failingRecorder := &dispatchRecorder{failPhase: "effect_outcome"}
	_, err = newService(failingRecorder).Invoke(ctx, InvokeRequest{
		Enqueue: request, ClaimID: []byte("claim"), Lease: time.Minute,
	})
	if !errors.Is(err, ErrExecutionAmbiguous) {
		t.Fatalf("first post-restart execution = %v", err)
	}
	completed, err := newService(&dispatchRecorder{}).Invoke(
		ctx,
		InvokeRequest{
			Enqueue: request, ClaimID: []byte("claim"), Lease: time.Minute,
		},
	)
	if err != nil || completed.State != DispatchSucceeded {
		t.Fatalf("ambiguous retry = %#v, %v", completed, err)
	}
	if len(executor.keys) != 2 ||
		!bytes.Equal(executor.keys[0], legacyKey) ||
		!bytes.Equal(executor.keys[1], legacyKey) ||
		!bytes.Equal(completed.ExecutorKey, legacyKey) {
		t.Fatalf(
			"stored executor key rerolled across restart/retry: %#v",
			executor.keys,
		)
	}
}

type dispatchRecorder struct {
	failPhase string
	phases    []string
	// operations is the phase-to-operation pairing, retained because keeping
	// only the phase made every assertion about *which* operation a transition
	// audits vacuous. A review found that mutating an audit's operation to one
	// that did not authorize the call left the whole package green, including
	// in a test whose docstring claimed to cover it.
	operations map[string]auth.Operation
	advance    func()
}

// recordedOperation returns the operation a phase was audited under, so a test
// can assert the pairing rather than only that the phase happened.
func (r *dispatchRecorder) recordedOperation(phase string) auth.Operation {
	return r.operations[phase]
}

func (r *dispatchRecorder) RecordAction(_ context.Context, audit ActionAudit) error {
	r.phases = append(r.phases, audit.Phase)
	if r.operations == nil {
		r.operations = map[string]auth.Operation{}
	}
	r.operations[audit.Phase] = audit.Operation
	// The real ActionRecorder validates the operation before anything else
	// (internal/explorerfleet/action_recorder.go:66), so a double that accepts
	// an invalid one hides every caller that passes one. A fifth review round
	// found that this double's permissiveness is the same shape of gap as the
	// no-op event sink: the dependencies that validate are the ones stubbed
	// with something that does not.
	if err := audit.Operation.Validate(); err != nil {
		return err
	}
	if r.advance != nil && audit.Phase == "effect_admission" {
		r.advance()
	}
	if audit.Phase == r.failPhase {
		return errors.New("recorder unavailable")
	}
	return nil
}

type dispatchEvents struct{}

func (dispatchEvents) PublishActionEvent(context.Context, string, ActionRecord) error { return nil }

func (dispatchEvents) MayPublishActionEvent(
	context.Context, string, ActionRecord,
) (bool, error) {
	return true, nil
}

type controlledDispatchEvents struct {
	calls int
	err   error
	kinds []string
}

type terminalFailEvents struct {
	calls int
	err   error
}

func (e *terminalFailEvents) MayPublishActionEvent(
	context.Context, string, ActionRecord,
) (bool, error) {
	// These doubles exercise publication *failure*, not entitlement. Saying
	// yes keeps every existing test asking its own question.
	return true, nil
}

func (e *terminalFailEvents) PublishActionEvent(_ context.Context, kind string, _ ActionRecord) error {
	if kind == "action.completed" || kind == "action.failed" {
		e.calls++
		return e.err
	}
	return nil
}

func (e *controlledDispatchEvents) MayPublishActionEvent(
	context.Context, string, ActionRecord,
) (bool, error) {
	// These doubles exercise publication *failure*, not entitlement. Saying
	// yes keeps every existing test asking its own question.
	return true, nil
}

func (e *controlledDispatchEvents) PublishActionEvent(
	_ context.Context, kind string, _ ActionRecord,
) error {
	e.calls++
	e.kinds = append(e.kinds, kind)
	return e.err
}

type dispatchExecutor struct {
	calls       int
	keys        [][]byte
	invocations []Invocation
	result      ExecutionResult
	err         error
	advance     func()
}

func (e *dispatchExecutor) Execute(_ context.Context, invocation Invocation) (ExecutionResult, error) {
	e.calls++
	e.keys = append(e.keys, append([]byte(nil), invocation.IdempotencyKey...))
	e.invocations = append(e.invocations, invocation)
	if e.advance != nil {
		e.advance()
	}
	return e.result, e.err
}

func dispatchDescriptor(now time.Time) Descriptor {
	return Descriptor{
		ID: "agent", Generation: 1, Subject: "owner", Actor: "actor",
		AuthorizationDomain: []byte("domain"),
		Scopes:              []Scope{{SourceID: []byte("source"), PolicyID: []byte("policy")}},
		ExecutorRef:         "exec", LeaseExpiresAt: now.Add(time.Hour), UpdatedAt: now,
		Capabilities: []Capability{{Name: "search", Actions: []Action{{
			Name:         "query",
			InputSchema:  json.RawMessage(`{"type":"object","properties":{"value":{"type":"integer"}},"required":["value"],"additionalProperties":false}`),
			OutputSchema: json.RawMessage(`{"type":"object","properties":{"ok":{"type":"boolean"}},"required":["ok"],"additionalProperties":false}`),
		}}}},
	}
}

func dispatchDecision(t *testing.T, subject, actor, request string, operations ...auth.Operation) auth.Decision {
	t.Helper()
	decision, err := auth.NewDecision(auth.DecisionConfig{
		Subject: shoal.ID(subject), Actor: shoal.ID(actor), AuthorizationDomain: []byte("domain"),
		AllowedOperations: operations, PermittedSourceIDs: [][]byte{[]byte("source")},
		PermittedPolicyIDs: [][]byte{[]byte("policy")}, PolicyGeneration: 1,
		AuthenticationExpires: time.Date(2026, 9, 7, 0, 0, 0, 0, time.UTC),
		RequestID:             shoal.ID(request), CorrelationID: "correlation",
	})
	if err != nil {
		t.Fatal(err)
	}
	return decision
}

func dispatchDecisionWithProvenance(
	t *testing.T,
	now time.Time,
	request, correlation string,
	policyGeneration int64,
	expires time.Time,
) auth.Decision {
	t.Helper()
	decision, err := auth.NewDecision(auth.DecisionConfig{
		Subject: "owner", Actor: "actor", AuthorizationDomain: []byte("domain"),
		AllowedOperations: []auth.Operation{
			auth.OperationDispatch, auth.OperationInvoke,
		},
		PermittedSourceIDs:    [][]byte{[]byte("source")},
		PermittedPolicyIDs:    [][]byte{[]byte("policy")},
		PolicyGeneration:      policyGeneration,
		AuthenticationExpires: expires,
		RequestID:             shoal.ID(request),
		CorrelationID:         shoal.ID(correlation),
	})
	if err != nil {
		t.Fatal(err)
	}
	if !now.Before(decision.AuthenticationExpires()) {
		t.Fatal("test decision is already expired")
	}
	return decision
}

func dispatchContext(now time.Time, request string) RequestContext {
	return RequestContext{
		RequestID: shoal.ID(request), CorrelationID: "correlation",
		ReasonCode: "operator_request", Deadline: now.Add(time.Hour),
	}
}

func dispatchEnqueueWithContext(
	now time.Time, request, correlation string,
) EnqueueRequest {
	result := dispatchEnqueue(now, request)
	result.Context.CorrelationID = shoal.ID(correlation)
	return result
}

func dispatchEnqueue(now time.Time, request string) EnqueueRequest {
	return EnqueueRequest{
		ID: []byte("action"), IdempotencyKey: []byte("idempotency"), AgentID: "agent",
		AgentGeneration: 1, Capability: "search", Action: "query",
		SourceID: []byte("source"), PolicyID: []byte("policy"), ObjectID: "object",
		Input: json.RawMessage(`{"value":1}`), Context: dispatchContext(now, request),
	}
}
