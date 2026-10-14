// Licensed to the Apache Software Foundation (ASF) under one or more
// contributor license agreements. See the NOTICE file distributed with this
// work for additional information regarding copyright ownership.
package fleet

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
	"time"

	"github.com/phrocker/shoal-oss/pkg/explorer/auth"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

// These two tests pin branches the hosted composition cannot reach on demand:
// a registry read that fails only on the approval service's own chain walk,
// and a store that reports a conflict for a write that is already readable.
// They use package-local store doubles, deliberately and only for that: each
// double changes exactly one store answer, and everything the branch decides
// with is the production code. The behavioural acceptance tests stay in
// cmd/shoal-explore-web against the real composition.

// chainFailingStore fails the Nth read of one agent and answers every other
// read from the registry's in-package memory store.
type chainFailingStore struct {
	*memoryStore
	mu     sync.Mutex
	id     shoal.ID
	failOn int
	reads  int
}

func (s *chainFailingStore) Get(ctx context.Context, id shoal.ID) (Stored, error) {
	if id == s.id {
		s.mu.Lock()
		s.reads++
		fail := s.reads == s.failOn
		s.mu.Unlock()
		if fail {
			return Stored{}, shoal.NewError(
				shoal.ErrorUnavailable, "injected chain read failure")
		}
	}
	return s.memoryStore.Get(ctx, id)
}

func approvalBranchDescriptor(now time.Time) Descriptor {
	schema := json.RawMessage(`{"type":"object"}`)
	return Descriptor{
		ID: "agent", Generation: 1, Subject: "owner", Actor: "operator",
		AuthorizationDomain: []byte("domain"),
		Scopes:              []Scope{{SourceID: []byte("s"), PolicyID: []byte("p")}},
		ExecutorRef:         "exec",
		Capabilities: []Capability{{Name: "ops", Actions: []Action{{
			Name: "deploy", InputSchema: schema, OutputSchema: schema,
			RequiresApproval: true,
		}}}},
		LeaseExpiresAt: now.Add(time.Hour), UpdatedAt: now,
	}
}

func approvalBranchDecision(
	t *testing.T, now time.Time, operations ...auth.Operation,
) auth.Decision {
	t.Helper()
	decision, err := auth.NewDecision(auth.DecisionConfig{
		Subject: "bob", Actor: "bob-console",
		AuthorizationDomain:   []byte("domain"),
		AllowedOperations:     operations,
		PermittedSourceIDs:    [][]byte{[]byte("s")},
		PermittedPolicyIDs:    [][]byte{[]byte("p")},
		PolicyGeneration:      3,
		AuthenticationExpires: now.Add(time.Hour),
		RequestID:             "branch-request",
	})
	if err != nil {
		t.Fatal(err)
	}
	return decision
}

// TestApprovalRefusesWhenTheChainCannotBeRead kills replacing the
// "chain unreadable → refuse" branch with a no-op. The binding resolves (the
// first read of the agent succeeds) and the approval service's own chain walk
// then fails; the approver must be refused, not waved through with an
// unchecked chain.
func TestApprovalRefusesWhenTheChainCannotBeRead(t *testing.T) {
	now := time.Unix(1_800_000_000, 0).UTC()
	memory := newMemoryStore()
	if _, err := memory.Apply(context.Background(), Mutation{
		RegistrationKey: "key", Descriptor: approvalBranchDescriptor(now),
	}); err != nil {
		t.Fatal(err)
	}
	record := ApprovalRecord{
		ID: []byte("held"),
		Request: ActionRecord{
			ID: []byte("held"), AgentID: "agent", AgentGeneration: 1,
			Capability: "ops", Action: "deploy",
			SourceID: []byte("s"), PolicyID: []byte("p"), ObjectID: "object",
			Subject: "alice", Actor: "alice-agent",
		},
	}
	decision := approvalBranchDecision(t, now, auth.OperationActionApprove)
	service := func(store Store) *ApprovalService {
		return &ApprovalService{
			dispatch: &DispatchService{registry: &Service{
				store:     store,
				executors: executorMap{"exec": struct{}{}},
				clock:     func() time.Time { return now },
			}},
			narrowed: func(context.Context) bool { return false },
		}
	}
	// Control: with every read answered, the approver is eligible, so a
	// refusal below can only come from the failed chain read.
	if _, err := service(&chainFailingStore{memoryStore: memory}).eligibleApprover(
		context.Background(), decision, record, now); err != nil {
		t.Fatalf("control: %v", err)
	}
	failing := &chainFailingStore{memoryStore: memory, id: "agent", failOn: 2}
	_, err := service(failing).eligibleApprover(
		context.Background(), decision, record, now)
	if failing.reads < 2 {
		t.Fatalf("the chain was read %d times; the injected failure never ran",
			failing.reads)
	}
	if !shoal.IsErrorCode(err, shoal.ErrorNotFound) {
		t.Fatalf("approver with an unreadable chain = %v, want refusal", err)
	}
}

// conflictAfterWriteStore writes the action and then reports the write as a
// shoal conflict, which is how the durable store answers a first write that
// lost to a concurrent identical one: the work exists, the error says
// conflict, and it is not the ErrActionConflict sentinel.
type conflictAfterWriteStore struct {
	*memoryDispatchStore
}

func (s conflictAfterWriteStore) ApplyAction(
	ctx context.Context, mutation DispatchMutation,
) (ActionRecord, error) {
	if _, err := s.memoryDispatchStore.ApplyAction(ctx, mutation); err != nil {
		return ActionRecord{}, err
	}
	return ActionRecord{}, shoal.NewError(
		shoal.ErrorConflict, "fleet registry conflict")
}

// TestApprovalMaterializeRereadsOnAnyConflict kills restricting the
// post-failure re-read to the ErrActionConflict sentinel. materialize is
// called directly, so no Request retry can mask the difference: with the
// re-read the call returns the work that exists; without it, a conflict.
func TestApprovalMaterializeRereadsOnAnyConflict(t *testing.T) {
	now := time.Unix(1_800_000_000, 0).UTC()
	store := conflictAfterWriteStore{memoryDispatchStore: newMemoryDispatchStore()}
	approval := decided(validApprovalRecord(t), ApprovalApproved, ApprovalVerdictApprove)
	approval.Version++
	approval.State = ApprovalEnqueued
	approval.MaterializedAt = approval.DecidedAt.Add(time.Second)
	approval.UpdatedAt = approval.MaterializedAt
	if err := approval.Validate(); err != nil {
		t.Fatal(err)
	}
	service := &ApprovalService{dispatch: &DispatchService{
		store: store, outbox: store, recorder: &dispatchRecorder{},
		events: dispatchEvents{}, clock: func() time.Time { return now },
	}}
	action, err := service.materialize(
		context.Background(),
		approvalBranchDecision(t, approval.RequestedAt, auth.OperationDispatch),
		approval)
	if err != nil {
		t.Fatalf("materialize over a conflict for work that exists = %v", err)
	}
	if !isMaterializationOf(action, approval) || action.Version != 1 {
		t.Fatalf("materialized = %+v", action)
	}
}
