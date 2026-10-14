// Licensed to the Apache Software Foundation (ASF) under one
// or more contributor license agreements. See the NOTICE file
// distributed with this work for additional information
// regarding copyright ownership. The ASF licenses this file
// to you under the Apache License, Version 2.0 (the
// "License"); you may not use this file except in compliance
// with the License. You may obtain a copy of the License at
//
//     https://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing,
// software distributed under the License is distributed on an
// "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY
// KIND, either express or implied. See the License for the
// specific language governing permissions and limitations
// under the License.

package fleet

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/phrocker/shoal-oss/pkg/explorer"
	"github.com/phrocker/shoal-oss/pkg/explorer/auth"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

func TestCanonicalSchemaRejectsTrailingJSON(t *testing.T) {
	if _, err := canonicalSchema(
		json.RawMessage(`{"type":"object"} {"type":"string"}`),
	); !shoal.IsErrorCode(err, shoal.ErrorInvalidArgument) {
		t.Fatalf("trailing schema JSON = %v", err)
	}
}

func TestGenerationValidationRejectsIncrementOverflow(t *testing.T) {
	if err := validateGeneration(math.MaxInt64); !shoal.IsErrorCode(
		err, shoal.ErrorInvalidArgument,
	) {
		t.Fatalf("register maximum generation error = %v", err)
	}
	if err := validateMutationIdentity(
		"agent", "registration", math.MaxInt64,
	); !shoal.IsErrorCode(err, shoal.ErrorInvalidArgument) {
		t.Fatalf("mutation maximum generation error = %v", err)
	}
}

func TestRegisterExactReplaySurvivesLeaseExpiry(t *testing.T) {
	now := time.Date(2026, 9, 6, 8, 0, 0, 0, time.UTC)
	authority, err := auth.NewAuthorityWithClock(func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	recorder := &memoryRecorder{}
	service, err := NewService(Config{
		Store: newMemoryStore(), Resolver: authority.Resolver(),
		Recorder: recorder, Snapshots: fixedSnapshot{now},
		Executors: executorMap{"exec": struct{}{}},
		Clock:     func() time.Time { return now },
	})
	if err != nil {
		t.Fatal(err)
	}
	decision := testDecision(
		t, "owner", "owner-actor", "late-retry", [][]byte{[]byte("source-a")})
	ctx := bindDecision(t, authority, decision)
	request := registerRequest(
		now, "late-retry", "late-retry-agent", "", "source-a")
	request.Spec.LeaseExpiresAt = now.Add(time.Second)

	registered, err := service.Register(ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	now = now.Add(2 * time.Second)
	replayed, err := service.Register(ctx, request)
	if err != nil {
		t.Fatalf("exact replay after lease expiry = %v", err)
	}
	if descriptorDigest(replayed) != descriptorDigest(registered) {
		t.Fatal("exact replay did not return the original descriptor")
	}
	// The replay is handed to the recorder (so its asserted reason is checked
	// against the original receipt), with the identical receipt identity and
	// mutation digest; the durable recorder reconciles it to that one receipt.
	recorder.mu.Lock()
	records := append([]Lifecycle(nil), recorder.records...)
	recorder.mu.Unlock()
	if len(records) != 2 {
		t.Fatalf("lifecycle record count = %d, want 2", len(records))
	}
	if records[1].RequestID != records[0].RequestID ||
		records[1].AgentID != records[0].AgentID ||
		records[1].MutationDigest != records[0].MutationDigest ||
		records[1].ReasonCode != records[0].ReasonCode ||
		records[1].ReasonDetail != records[0].ReasonDetail {
		t.Fatalf("replay lifecycle differs from the original: %#v", records)
	}

	divergent := request
	divergent.Spec.ExecutorRef = "changed"
	if _, err := service.Register(ctx, divergent); !shoal.IsErrorCode(
		err, shoal.ErrorConflict,
	) {
		t.Fatalf("divergent late replay = %v", err)
	}
}

func TestActiveChainIsBoundedAndHidesUnauthorizedParents(t *testing.T) {
	now := time.Date(2026, 9, 6, 8, 0, 0, 0, time.UTC)
	store := newMemoryStore()
	service := &Service{store: store}
	for index := 0; index <= MaxDelegationDepth; index++ {
		id := shoal.ID(string(rune('a' + index)))
		parent := shoal.ID("")
		if index < MaxDelegationDepth {
			parent = shoal.ID(string(rune('a' + index + 1)))
		}

		store.records[id] = Stored{Descriptor: Descriptor{
			ID: id, Generation: 1, Subject: "owner", ParentID: parent,
			AuthorizationDomain: []byte("domain"),
			Scopes:              []Scope{{SourceID: []byte("source-a"), PolicyID: []byte("policy")}},
			Capabilities:        []Capability{{Name: "search"}},
			LeaseExpiresAt:      now.Add(time.Hour),
		}}
	}
	if _, err := service.active(
		context.Background(), "a", now,
	); !shoal.IsErrorCode(err, shoal.ErrorNotFound) {
		t.Fatalf("over-depth delegation = %v", err)
	}

	child := store.records["a"]
	child.Descriptor.ParentID = "parent"
	store.records["a"] = child
	store.records["parent"] = Stored{Descriptor: Descriptor{
		ID: "parent", Generation: 1, Subject: "owner",
		AuthorizationDomain: []byte("domain"),
		Scopes: []Scope{
			{SourceID: []byte("source-a"), PolicyID: []byte("policy")},
			{SourceID: []byte("source-b"), PolicyID: []byte("policy")},
		},
		Capabilities:   []Capability{{Name: "search"}},
		LeaseExpiresAt: now.Add(time.Hour),
	}}
	decision := testDecision(
		t, "peer", "peer-actor", "request", [][]byte{[]byte("source-a")})
	if _, err := service.authorizedActive(
		context.Background(), decision, "a", now,
	); !shoal.IsErrorCode(err, shoal.ErrorNotFound) {
		t.Fatalf("hidden parent resolution = %v", err)
	}
}

func TestRegisterRejectsOverDepthAndConcealsForeignParent(t *testing.T) {
	now := time.Date(2026, 9, 6, 8, 0, 0, 0, time.UTC)
	authority, err := auth.NewAuthorityWithClock(func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}

	store := newMemoryStore()
	service, err := NewService(Config{
		Store: store, Resolver: authority.Resolver(), Recorder: &memoryRecorder{},
		Snapshots: fixedSnapshot{now},
		Executors: executorMap{"exec": struct{}{}}, Clock: func() time.Time { return now },
	})
	if err != nil {
		t.Fatal(err)
	}
	decision := testDecision(
		t, "owner", "owner-actor", "depth-request", [][]byte{[]byte("source-a")})
	ctx := bindDecision(t, authority, decision)
	parent := shoal.ID("")
	for index := 0; index < MaxDelegationDepth; index++ {
		id := shoal.ID(fmt.Sprintf("depth-%02d", index))
		request := registerRequest(
			now, "depth-request", string(id), string(parent), "source-a")
		if _, err := service.Register(ctx, request); err != nil {
			t.Fatalf("register depth %d: %v", index+1, err)
		}
		parent = id
	}
	overDepth := registerRequest(
		now, "depth-request", "depth-overflow", string(parent), "source-a")
	if _, err := service.Register(ctx, overDepth); !shoal.IsErrorCode(
		err, shoal.ErrorInvalidArgument,
	) {
		t.Fatalf("over-depth registration = %v", err)
	}

	foreignSpec := registerRequest(
		now, "depth-request", "foreign-parent", "", "source-a").Spec
	foreign, err := foreignSpec.canonical(now)
	if err != nil {
		t.Fatal(err)
	}
	store.records[foreign.ID] = Stored{Descriptor: Descriptor{
		ID: foreign.ID, Generation: 1, Subject: "other", Actor: "other-actor",
		AuthorizationDomain: foreign.AuthorizationDomain, Scopes: foreign.Scopes,
		ExecutorRef: foreign.ExecutorRef, Capabilities: foreign.Capabilities,
		LeaseExpiresAt: foreign.LeaseExpiresAt, UpdatedAt: now,
	}}
	child := registerRequest(
		now, "depth-request", "foreign-child", string(foreign.ID), "source-a")
	if _, err := service.Register(ctx, child); !shoal.IsErrorCode(
		err, shoal.ErrorNotFound,
	) {
		t.Fatalf("foreign parent registration = %v", err)
	}
}

func TestRegisterDoesNotProbeExecutorBeforeScopeAuthorization(t *testing.T) {
	now := time.Date(2026, 9, 6, 8, 0, 0, 0, time.UTC)
	authority, err := auth.NewAuthorityWithClock(func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	executors := &countingExecutorRegistry{known: true}
	service, err := NewService(Config{
		Store: newMemoryStore(), Resolver: authority.Resolver(),
		Recorder: &memoryRecorder{}, Snapshots: fixedSnapshot{now},
		Executors: executors, Clock: func() time.Time { return now },
	})
	if err != nil {
		t.Fatal(err)
	}
	decision := testDecision(
		t, "owner", "owner-actor", "scope-request", [][]byte{[]byte("source-a")})
	request := registerRequest(now, "scope-request", "agent", "", "source-b")
	if _, err := service.Register(
		bindDecision(t, authority, decision), request,
	); err == nil {
		t.Fatalf("unauthorized registration = %v", err)
	}
	if executors.calls != 0 {
		t.Fatalf("unauthorized registration probed executor registry %d times", executors.calls)
	}
}

func TestServiceAuthorizationDelegationLeaseAndRevocation(t *testing.T) {
	now := time.Date(2026, 9, 5, 20, 0, 0, 0, time.UTC)
	authority, err := auth.NewAuthorityWithClock(func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	store := newMemoryStore()
	recorder := &memoryRecorder{}
	service, err := NewService(Config{
		Store: store, Resolver: authority.Resolver(), Recorder: recorder,
		Snapshots: fixedSnapshot{now},
		Executors: executorMap{"exec": struct{}{}}, Clock: func() time.Time { return now },
	})
	if err != nil {
		t.Fatal(err)
	}
	owner := testDecision(t, "owner", "owner-actor", "request-1",
		[][]byte{[]byte("source-a"), []byte("source-b")})
	ctx := bindDecision(t, authority, owner)
	parentRequest := registerRequest(now, "request-1", "parent", "", "source-a")
	parent, err := service.Register(ctx, parentRequest)
	if err != nil {
		t.Fatal(err)
	}
	if parent.Subject != "owner" || parent.Actor != "owner-actor" {
		t.Fatalf("trusted actor projection = %#v", parent)
	}

	delegationDecision, err := auth.NewDecision(auth.DecisionConfig{
		Subject: "owner", Actor: "delegating-actor", AuthorizationDomain: []byte("domain"),
		AllowedOperations:  []auth.Operation{auth.OperationAgentRegister, auth.OperationDelegate},
		PermittedSourceIDs: [][]byte{[]byte("source-a")},
		PermittedPolicyIDs: [][]byte{[]byte("policy")}, PolicyGeneration: 1,
		AuthenticationExpires: time.Date(2026, 9, 7, 0, 0, 0, 0, time.UTC),
		RequestID:             "request-delegate",
	})
	if err != nil {
		t.Fatal(err)
	}
	delegationCtx := bindDecision(t, authority, delegationDecision)
	overlongChild := registerRequest(now, "request-delegate", "overlong", "parent", "source-a")
	overlongChild.Spec.LeaseExpiresAt = parent.LeaseExpiresAt.Add(time.Second)
	if _, err := service.Register(delegationCtx, overlongChild); !shoal.IsErrorCode(err, shoal.ErrorUnauthorized) {
		t.Fatalf("delegated lease widening = %v", err)
	}
	childRequest := registerRequest(now, "request-delegate", "child", "parent", "source-a")
	child, err := service.Register(delegationCtx, childRequest)
	if err != nil {
		t.Fatal(err)
	}
	if child.ParentID != parent.ID {
		t.Fatalf("child parent = %q", child.ParentID)
	}
	childDecision := testDecision(t, "owner", "delegating-actor", "request-2",
		[][]byte{[]byte("source-a"), []byte("source-b")})
	childCtx := bindDecision(t, authority, childDecision)
	updateDecision := testDecision(t, "owner", "owner-actor", "request-update",
		[][]byte{[]byte("source-a"), []byte("source-b")})
	updateCtx := bindDecision(t, authority, updateDecision)
	directWidening := registerRequest(now, "request-update", "parent", "", "source-b")
	directWidening.ExpectedGeneration = 1
	if _, err := service.Register(updateCtx, directWidening); !shoal.IsErrorCode(err, shoal.ErrorUnauthorized) {
		t.Fatalf("direct authorization widening = %v", err)
	}
	widened := registerRequest(now, "request-2", "wide", "parent", "source-b")
	widened.Spec.Capabilities[0].Actions[0].Name = "admin"
	if _, err := service.Register(childCtx, widened); !shoal.IsErrorCode(err, shoal.ErrorUnauthorized) {
		t.Fatalf("widened delegation = %v", err)
	}

	other := testDecision(t, "other", "other-actor", "request-3", [][]byte{[]byte("source-b")})
	otherCtx := bindDecision(t, authority, other)
	if _, err := service.Resolve(otherCtx, ResolveRequest{
		Context: requestContext(now, "request-3"), ID: child.ID,
	}); !shoal.IsErrorCode(err, shoal.ErrorNotFound) {
		t.Fatalf("cross-principal resolution = %v", err)
	}
	peer := testDecision(t, "peer", "peer-actor", "request-peer", [][]byte{[]byte("source-a")})
	peerCtx := bindDecision(t, authority, peer)
	resolved, err := service.Resolve(peerCtx, ResolveRequest{
		Context: requestContext(now, "request-peer"), ID: child.ID,
	})
	if err != nil || resolved.Descriptor.Subject != "owner" || resolved.Executor == nil {
		t.Fatalf("authorized cross-principal resolution = %#v, %v", resolved, err)
	}
	if err := service.ValidateDelivery(peerCtx, child.ID, child.Generation); err != nil {
		t.Fatalf("delivery validation = %v", err)
	}
	if err := service.ValidateCurrentDelivery(peerCtx, child.ID); err != nil {
		t.Fatalf("current delivery validation = %v", err)
	}
	if err := service.ValidateDelivery(peerCtx, child.ID, child.Generation+1); !shoal.IsErrorCode(
		err, shoal.ErrorNotFound,
	) {
		t.Fatalf("stale delivery generation = %v", err)
	}

	heartbeatDecision := testDecision(t, "owner", "owner-actor", "request-4",
		[][]byte{[]byte("source-a"), []byte("source-b")})
	heartbeatCtx := bindDecision(t, authority, heartbeatDecision)
	if _, err := service.Heartbeat(heartbeatCtx, HeartbeatRequest{
		Context: requestContext(now, "request-4"), RegistrationKey: "child-heartbeat-too-long",
		ID: child.ID, ExpectedGeneration: child.Generation,
		LeaseExpiresAt: parent.LeaseExpiresAt.Add(time.Second),
	}); !shoal.IsErrorCode(err, shoal.ErrorUnauthorized) {
		t.Fatalf("delegated heartbeat lease widening = %v", err)
	}
	if _, err := service.Heartbeat(otherCtx, HeartbeatRequest{
		Context: requestContext(now, "request-3"), RegistrationKey: "foreign-heartbeat",
		ID: parent.ID, ExpectedGeneration: parent.Generation,
		LeaseExpiresAt: now.Add(time.Hour),
	}); !shoal.IsErrorCode(err, shoal.ErrorNotFound) {
		t.Fatalf("cross-principal heartbeat = %v", err)
	}
	heartbeat, err := service.Heartbeat(heartbeatCtx, HeartbeatRequest{
		Context: requestContext(now, "request-4"), RegistrationKey: "heartbeat-key",
		ID: parent.ID, ExpectedGeneration: parent.Generation,
		LeaseExpiresAt: now.Add(time.Hour),
	})
	if err != nil {
		t.Fatal(err)
	}
	if heartbeat.Generation != 2 {
		t.Fatalf("heartbeat generation = %d", heartbeat.Generation)
	}
	now = now.Add(31 * time.Minute)
	if _, err := service.Resolve(childCtx, ResolveRequest{
		Context: requestContext(now, "request-2"), ID: child.ID,
	}); !shoal.IsErrorCode(err, shoal.ErrorNotFound) {
		t.Fatalf("expired child resolution = %v", err)
	}
	revokeDecision := testDecision(t, "owner", "owner-actor", "request-5",
		[][]byte{[]byte("source-a"), []byte("source-b")})
	revokeCtx := bindDecision(t, authority, revokeDecision)
	if _, err := service.Revoke(otherCtx, RevokeRequest{
		Context: requestContext(now, "request-3"), RegistrationKey: "foreign-revoke",
		ID: parent.ID, ExpectedGeneration: heartbeat.Generation,
	}); !shoal.IsErrorCode(err, shoal.ErrorNotFound) {
		t.Fatalf("cross-principal revoke = %v", err)
	}
	revoked, err := service.Revoke(revokeCtx, RevokeRequest{
		Context: requestContext(now, "request-5"), RegistrationKey: "revoke-key",
		ID: parent.ID, ExpectedGeneration: heartbeat.Generation,
	})
	if err != nil {
		t.Fatal(err)
	}
	now = now.Add(time.Minute)
	replayed, err := service.Revoke(revokeCtx, RevokeRequest{
		Context: requestContext(now, "request-5"), RegistrationKey: "revoke-key",
		ID: parent.ID, ExpectedGeneration: heartbeat.Generation,
	})
	if err != nil {
		t.Fatal(err)
	}
	if replayed.Generation != revoked.Generation ||
		!replayed.RevokedAt.Equal(revoked.RevokedAt) {
		t.Fatalf("replayed revoke changed descriptor: before=%#v after=%#v", revoked, replayed)
	}
	if _, err := service.Resolve(revokeCtx, ResolveRequest{
		Context: requestContext(now, "request-5"), ID: parent.ID,
	}); !shoal.IsErrorCode(err, shoal.ErrorNotFound) {
		t.Fatalf("revoked resolution = %v", err)
	}
	if _, err := service.Resolve(revokeCtx, ResolveRequest{
		Context: requestContext(now, "request-5"), ID: child.ID,
	}); !shoal.IsErrorCode(err, shoal.ErrorNotFound) {
		t.Fatalf("parent-revoked delegated resolution = %v", err)
	}
	if err := service.ValidateDelivery(revokeCtx, child.ID, child.Generation); !shoal.IsErrorCode(
		err, shoal.ErrorNotFound,
	) {
		t.Fatalf("parent-revoked delivery validation = %v", err)
	}
	if err := service.ValidateCurrentDelivery(revokeCtx, child.ID); !shoal.IsErrorCode(
		err, shoal.ErrorNotFound,
	) {
		t.Fatalf("parent-revoked current delivery validation = %v", err)
	}
	if recorder.count() != 6 {
		t.Fatalf("lifecycle records = %d, want 6", recorder.count())
	}
}

func TestServiceRecorderAndAmbiguousStoreFailClosed(t *testing.T) {
	now := time.Date(2026, 9, 5, 20, 0, 0, 0, time.UTC)
	authority, _ := auth.NewAuthorityWithClock(func() time.Time { return now })
	store := newMemoryStore()
	recorder := &memoryRecorder{err: shoal.NewError(shoal.ErrorUnavailable, "recorder unavailable")}
	service, _ := NewService(Config{
		Store: store, Resolver: authority.Resolver(), Recorder: recorder,
		Snapshots: fixedSnapshot{now},
		Executors: executorMap{"exec": struct{}{}}, Clock: func() time.Time { return now },
	})
	decision := testDecision(t, "owner", "actor", "request-1", [][]byte{[]byte("source-a")})
	ctx := bindDecision(t, authority, decision)
	request := registerRequest(now, "request-1", "agent", "", "source-a")
	if _, err := service.Register(ctx, request); !shoal.IsErrorCode(err, shoal.ErrorUnavailable) {
		t.Fatalf("recorder failure = %v", err)
	}
	if len(store.records) != 0 {
		t.Fatal("mutation occurred after recorder failure")
	}

	recorder.err = nil
	store.applyErr = shoal.NewError(shoal.ErrorUnavailable, "publication outcome indeterminate")
	if _, err := service.Register(ctx, request); !shoal.IsErrorCode(err, shoal.ErrorUnavailable) {
		t.Fatalf("ambiguous write = %v", err)
	}
	if len(store.records) != 0 {
		t.Fatal("ambiguous write fabricated committed state")
	}
	if recorder.count() != 2 {
		t.Fatalf("pre-admission records = %d, want 2", recorder.count())
	}
}

func TestLifecycleCarriesTrustedAuthorizationPins(t *testing.T) {
	now := time.Date(2026, 9, 5, 20, 0, 0, 0, time.UTC)
	authority, err := auth.NewAuthorityWithClock(func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	recorder := &memoryRecorder{}
	service, err := NewService(Config{
		Store: newMemoryStore(), Resolver: authority.Resolver(), Recorder: recorder,
		Snapshots: fixedSnapshot{now},
		Executors: executorMap{"exec": struct{}{}}, Clock: func() time.Time { return now },
	})
	if err != nil {
		t.Fatal(err)
	}
	decision := testDecision(t, "owner", "actor", "request-pins", [][]byte{[]byte("source-a")})
	ctx := bindDecision(t, authority, decision)
	if _, err := service.Register(ctx, registerRequest(
		now, "request-pins", "pinned-agent", "", "source-a",
	)); err != nil {
		t.Fatal(err)
	}
	record := recorder.last(t)
	fingerprint, err := auth.AuthorizationFingerprint(decision)
	if err != nil {
		t.Fatal(err)
	}
	if record.Operation != auth.OperationAgentRegister ||
		record.AuthorizationFingerprint != fingerprint ||
		!record.AuthorizationExpiresAt.Equal(decision.AuthenticationExpires()) ||
		record.AuditPurpose != decision.AuditPurpose() ||
		record.SnapshotID != "snapshot" || !record.SnapshotAsOf.Equal(now) {
		t.Fatalf("lifecycle authorization pins = %#v", record)
	}
}

func TestServiceRejectsTypedNilRecorder(t *testing.T) {
	now := time.Date(2026, 9, 5, 20, 0, 0, 0, time.UTC)
	authority, err := auth.NewAuthorityWithClock(func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	var recorder *memoryRecorder
	if _, err := NewService(Config{
		Store: newMemoryStore(), Resolver: authority.Resolver(), Recorder: recorder,
		Snapshots: fixedSnapshot{now}, Executors: executorMap{"exec": struct{}{}},
		Clock: func() time.Time { return now },
	}); !shoal.IsErrorCode(err, shoal.ErrorInvalidArgument) {
		t.Fatalf("typed-nil recorder = %v", err)
	}
}

type fixedSnapshot struct{ now time.Time }

func (s fixedSnapshot) InteractionSnapshot(context.Context) (explorer.Snapshot, error) {
	return explorer.Snapshot{ID: "snapshot", AsOf: s.now, Frontier: 1}, nil
}

type memoryStore struct {
	mu       sync.Mutex
	records  map[shoal.ID]Stored
	keys     map[shoal.ID][32]byte
	applyErr error
	// gets counts descriptor lookups, so a test can assert that a refusal
	// happened without one. A caller-only authorization failure must not
	// reach the store at all — that is what makes its honest error safe,
	// rather than an existence oracle (#536).
	gets int
}

// getCount reports how many descriptor lookups have been made.
func (s *memoryStore) getCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.gets
}

func newMemoryStore() *memoryStore {
	return &memoryStore{records: make(map[shoal.ID]Stored), keys: make(map[shoal.ID][32]byte)}
}

func (s *memoryStore) Apply(_ context.Context, mutation Mutation) (Stored, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.applyErr != nil {
		return Stored{}, s.applyErr
	}
	current, exists := s.records[mutation.Descriptor.ID]
	keyDigest := sha256.Sum256([]byte(mutation.RegistrationKey))
	if exists &&
		current.Descriptor.Generation == mutation.ExpectedGeneration+1 &&
		current.RegistrationDigest == keyDigest &&
		current.Digest == descriptorDigest(mutation.Descriptor) {
		return current, nil
	}
	if (!exists && mutation.ExpectedGeneration != 0) ||
		(exists && current.Descriptor.Generation != mutation.ExpectedGeneration) {
		return Stored{}, shoal.NewError(shoal.ErrorConflict, "generation conflict")
	}
	stored := Stored{
		Descriptor:         cloneDescriptor(mutation.Descriptor),
		RegistrationDigest: keyDigest,
		Epoch:              mutation.Descriptor.Generation,
	}
	stored.Digest = descriptorDigest(stored.Descriptor)
	s.records[mutation.Descriptor.ID] = stored
	return stored, nil
}

func (s *memoryStore) Get(_ context.Context, id shoal.ID) (Stored, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.gets++
	stored, ok := s.records[id]
	if !ok {
		return Stored{}, shoal.NewError(shoal.ErrorNotFound, "not found")
	}
	stored.Descriptor = cloneDescriptor(stored.Descriptor)
	return stored, nil
}

func (s *memoryStore) List(
	_ context.Context, cursor []byte, limit int,
) (StoredPage, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	result := make([]Stored, 0, len(s.records))
	for _, stored := range s.records {
		stored.Descriptor = cloneDescriptor(stored.Descriptor)
		result = append(result, stored)
	}
	sort.Slice(result, func(i, j int) bool {
		return result[i].Descriptor.ID < result[j].Descriptor.ID
	})
	page := StoredPage{}
	after := shoal.ID(string(cursor))
	for _, stored := range result {
		if stored.Descriptor.ID <= after {
			continue
		}
		if len(page.Entries) == limit {
			page.Next = []byte(page.Entries[len(page.Entries)-1].Descriptor.ID)
			break
		}
		page.Entries = append(page.Entries, stored)
	}
	return page, nil
}

type memoryRecorder struct {
	mu      sync.Mutex
	records []Lifecycle
	err     error
}

func TestRegistryMutationDigestIgnoresServerAcceptanceTime(t *testing.T) {
	mutation := Mutation{
		RegistrationKey:    "retry-key",
		ExpectedGeneration: 4,
		Descriptor: Descriptor{
			ID: "agent", Generation: 5, Subject: "subject", Actor: "actor",
			AuthorizationDomain: []byte("domain"), ExecutorRef: "executor",
			LeaseExpiresAt: time.Unix(100, 0), UpdatedAt: time.Unix(10, 0),
		},
	}
	first := registryMutationDigest(mutation)
	mutation.Descriptor.UpdatedAt = time.Unix(20, 0)
	if second := registryMutationDigest(mutation); second != first {
		t.Fatal("server-derived acceptance time changed logical mutation digest")
	}
	mutation.Descriptor.LeaseExpiresAt = time.Unix(101, 0)
	if changed := registryMutationDigest(mutation); changed == first {
		t.Fatal("logical mutation change did not change digest")
	}
}

func (r *memoryRecorder) RecordLifecycle(_ context.Context, lifecycle Lifecycle) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.records = append(r.records, lifecycle)
	return r.err
}
func (r *memoryRecorder) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.records)
}
func (r *memoryRecorder) last(t *testing.T) Lifecycle {
	t.Helper()
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.records) == 0 {
		t.Fatal("no lifecycle records")
	}
	return r.records[len(r.records)-1]
}

type executorMap map[string]Executor

func (m executorMap) ResolveExecutor(ref string) (Executor, bool) {
	executor, ok := m[ref]
	return executor, ok
}

type countingExecutorRegistry struct {
	calls int
	known bool
}

func (r *countingExecutorRegistry) ResolveExecutor(string) (Executor, bool) {
	r.calls++
	return struct{}{}, r.known
}

func testDecision(t *testing.T, subject, actor, requestID string, sources [][]byte) auth.Decision {
	t.Helper()
	decision, err := auth.NewDecision(auth.DecisionConfig{
		Subject: shoal.ID(subject), Actor: shoal.ID(actor),
		AuthorizationDomain: []byte("domain"),
		AllowedOperations: []auth.Operation{
			auth.OperationAgentRegister, auth.OperationAgentHeartbeat,
			auth.OperationAgentRevoke, auth.OperationAgentResolve, auth.OperationDelegate,
		},
		PermittedSourceIDs: sources, PermittedPolicyIDs: [][]byte{[]byte("policy")},
		PolicyGeneration:      1,
		AuthenticationExpires: time.Date(2026, 9, 7, 0, 0, 0, 0, time.UTC),
		RequestID:             shoal.ID(requestID),
	})
	if err != nil {
		t.Fatal(err)
	}
	return decision
}

func bindDecision(t *testing.T, authority *auth.Authority, decision auth.Decision) context.Context {
	t.Helper()
	ctx, err := authority.Binder().Bind(context.Background(), decision)
	if err != nil {
		t.Fatal(err)
	}
	return ctx
}

func registerRequest(now time.Time, requestID, id, parent, source string) RegisterRequest {
	return RegisterRequest{
		Context: requestContext(now, requestID), RegistrationKey: shoal.ID("key-" + id),
		Spec: Spec{
			ID: shoal.ID(id), ParentID: shoal.ID(parent),
			AuthorizationDomain: []byte("domain"),
			Scopes:              []Scope{{SourceID: []byte(source), PolicyID: []byte("policy")}},
			ExecutorRef:         "exec", LeaseExpiresAt: now.Add(30 * time.Minute),
			Capabilities: []Capability{{
				Name: "search", Actions: []Action{{
					Name: "query", InputSchema: json.RawMessage(`{"type":"object"}`),
					OutputSchema: json.RawMessage(`{"type":"object"}`),
				}},
			}},
		},
	}
}

func requestContext(now time.Time, requestID string) RequestContext {
	return RequestContext{
		RequestID: shoal.ID(requestID), ReasonCode: "operator_request",
		Deadline: now.Add(time.Minute),
	}
}
