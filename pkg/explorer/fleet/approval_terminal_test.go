// Licensed to the Apache Software Foundation (ASF) under one or more
// contributor license agreements. See the NOTICE file distributed with this
// work for additional information regarding copyright ownership.
package fleet

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/phrocker/shoal-oss/pkg/explorer/auth"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

// The terminal-states switch (#655) and the sweep's scan, at the unit level.
// The paths end to end, against the real composition, are in
// cmd/shoal-explore-web/approval_terminal_test.go.

// TestNoTransitionWritesATerminalStateWhileDisabled: with TerminalStates
// off, commit — the one place every transition passes — refuses a withdrawn
// or a stranded record, whatever built it, and the lazy settlement and the
// sweep write nothing. An expiry, which a build before #655 already writes,
// is still allowed.
func TestNoTransitionWritesATerminalStateWhileDisabled(t *testing.T) {
	pending := validApprovalRecord(t)
	store := &memoryApprovalStore{records: map[string]ApprovalRecord{
		string(pending.ID): CloneApprovalRecord(pending),
	}}
	now := pending.RequestedAt.Add(2 * time.Minute)
	// Disabled, the lazy settlement does not even look: reading the
	// registry is the first thing it would do.
	untouched := &readTrapStore{memoryStore: newMemoryStore(), t: t}
	service := &ApprovalService{
		dispatch: &DispatchService{
			clock: func() time.Time { return now }, registry: &Service{store: untouched},
		},
		store: store, recorder: unguardedApprovalRecorder{},
		generations: fixedGenerations{generation: 4},
		sweeper: func(context.Context, ApprovalSweepScope) (context.Context, error) {
			t.Fatal("the sweep bound its observer while terminal states are off")
			return nil, nil
		},
	}
	decision := approvalBranchDecision(t, pending.RequestedAt, auth.OperationDispatch)
	for _, record := range []ApprovalRecord{
		withdrawnRecord(t),
		strandedRecord(pending, ApprovalConditionTargetMoved, now),
	} {
		if _, err := service.commit(
			context.Background(), "approval_strand", auth.OperationDispatch,
			decision, record, pending.Version,
		); !errors.Is(err, ErrApprovalTerminalStatesDisabled) {
			t.Fatalf("commit of a %s record = %v", record.State, err)
		}
	}
	// The policy in force (4) is past the request's (3): with the switch on
	// this would strand.
	if _, settled, err := service.settleObserved(
		context.Background(), auth.OperationDispatch, decision, pending, now,
	); settled || err != nil {
		t.Fatalf("settleObserved while disabled = %v, %v", settled, err)
	}
	if _, err := service.Sweep(context.Background(), ApprovalSweepRequest{
		Limit: MaxApprovalListResults,
	}); !errors.Is(err, ErrApprovalTerminalStatesDisabled) {
		t.Fatalf("sweep while disabled = %v", err)
	}
	stored, _ := store.GetApproval(context.Background(), pending.ID)
	if stored.State != ApprovalPending || stored.Version != pending.Version {
		t.Fatalf("a disabled host wrote %q v%d", stored.State, stored.Version)
	}
	// An expiry is a state every build reads, and is still written.
	expired := CloneApprovalRecord(pending)
	expired.Version++
	expired.State = ApprovalExpired
	expired.UpdatedAt = pending.ExpiresAt
	if _, err := service.commit(
		context.Background(), "approval_expiry", auth.OperationDispatch,
		decision, expired, pending.Version,
	); err != nil {
		t.Fatalf("an expiry while disabled = %v", err)
	}
}

// tolerantStore answers the sweep's tolerant scan with a page that skipped
// some undecodable records, and counts action reads.
type tolerantStore struct {
	*scanningApprovalStore
	undecodable int
}

func (s tolerantStore) ScanApprovalsTolerant(
	ctx context.Context, after []byte, limit int,
) (ApprovalPage, error) {
	page, err := s.ScanApprovals(ctx, after, limit)
	page.Undecodable = s.undecodable
	return page, err
}

type countingActionStore struct {
	*memoryDispatchStore
	reads atomic.Int64
}

func (s *countingActionStore) GetAction(ctx context.Context, id []byte) (ActionRecord, error) {
	s.reads.Add(1)
	return s.memoryDispatchStore.GetAction(ctx, id)
}

// TestTheSweepSkipsWhatCannotChange: an undecodable record no longer ends
// the page (it is counted, in the result and the process counter), and an
// enqueued approval whose action is its materialization is read once and
// then skipped, since nothing can change it again.
func TestTheSweepSkipsWhatCannotChange(t *testing.T) {
	pending := validApprovalRecord(t)
	approved := decided(pending, ApprovalApproved, ApprovalVerdictApprove)
	enqueued := approved
	enqueued.Version++
	enqueued.State = ApprovalEnqueued
	enqueued.MaterializedAt = approved.DecidedAt.Add(time.Minute)
	enqueued.UpdatedAt = enqueued.MaterializedAt
	actions := &countingActionStore{memoryDispatchStore: newMemoryDispatchStore()}
	action := cloneActionRecord(enqueued.Request)
	action.ApprovalRequestDigest = enqueued.RequestDigest
	action.ApproverSubject, action.ApproverActor = enqueued.ApproverSubject, enqueued.ApproverActor
	action.ApprovedAt = enqueued.DecidedAt
	actions.records[string(action.ID)] = action
	store := tolerantStore{
		scanningApprovalStore: &scanningApprovalStore{memoryApprovalStore: &memoryApprovalStore{
			records: map[string]ApprovalRecord{string(enqueued.ID): CloneApprovalRecord(enqueued)},
		}},
		undecodable: 2,
	}
	now := enqueued.UpdatedAt.Add(time.Minute)
	service := &ApprovalService{
		dispatch: &DispatchService{store: actions, clock: func() time.Time { return now }},
		store:    store, recorder: unguardedApprovalRecorder{},
		generations: fixedGenerations{generation: 3}, terminal: true,
		sweeper: func(context.Context, ApprovalSweepScope) (context.Context, error) {
			t.Fatal("the sweep bound its observer for a record that needs nothing")
			return nil, nil
		},
	}
	first, err := service.Sweep(context.Background(), ApprovalSweepRequest{Limit: MaxApprovalListResults})
	if err != nil || first.Examined != 1 || first.Undecodable != 2 || first.Failed != 0 {
		t.Fatalf("first sweep = %+v, %v", first, err)
	}
	if actions.reads.Load() != 1 {
		t.Fatalf("first sweep read the action %d times", actions.reads.Load())
	}
	second, err := service.Sweep(context.Background(), ApprovalSweepRequest{Limit: MaxApprovalListResults})
	if err != nil || second.Settled != 1 || actions.reads.Load() != 1 {
		t.Fatalf("second sweep = %+v, %v, with %d action reads", second, err, actions.reads.Load())
	}
	if undecodable, failed, _ := service.ApprovalSweepCounters(); undecodable != 4 || failed != 0 {
		t.Fatalf("counters = %d undecodable, %d failed", undecodable, failed)
	}
}

// readTrapStore fails the test on any registry read.
type readTrapStore struct {
	*memoryStore
	t *testing.T
}

func (s *readTrapStore) Get(context.Context, shoal.ID) (Stored, error) {
	s.t.Error("a disabled host read the registry to judge a stranding")
	return Stored{}, shoal.NewError(shoal.ErrorNotFound, "trap")
}

// TestASweepStoppedMidPageResumesAfterTheLastRecordItFinished: a pass whose
// budget ends inside a page reports the last record it finished as Next, so
// the next pass resumes after it instead of redoing, or never leaving, a page
// too slow for one budget.
func TestASweepStoppedMidPageResumesAfterTheLastRecordItFinished(t *testing.T) {
	first := validApprovalRecord(t)
	second := CloneApprovalRecord(first)
	second.ID = []byte("held-2")
	second.Request.ID = second.ID
	second.Request.ExecutorKey = executorKey(second.ID, second.Request.IdempotencyKey)
	second.RequestDigest = ApprovalRequestDigest(second.Request)
	if err := second.Validate(); err != nil {
		t.Fatal(err)
	}
	store := &scanningApprovalStore{memoryApprovalStore: &memoryApprovalStore{
		records: map[string]ApprovalRecord{
			string(first.ID): CloneApprovalRecord(first), string(second.ID): CloneApprovalRecord(second),
		},
	}}
	now := first.ExpiresAt.Add(time.Minute)
	authority, err := auth.NewAuthorityWithClock(func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	service := &ApprovalService{
		dispatch: &DispatchService{resolver: authority.Resolver(), clock: func() time.Time { return now }},
		store:    store, recorder: unguardedApprovalRecorder{},
		generations: fixedGenerations{generation: 3}, terminal: true,
		sweeper: func(bindCtx context.Context, scope ApprovalSweepScope) (context.Context, error) {
			decision, err := auth.NewDecision(auth.DecisionConfig{
				Subject: ApprovalSweeperSubject, Actor: ApprovalSweeperSubject,
				ClientID: ApprovalSweeperClientID, AuthorizationDomain: []byte("domain"),
				AllowedOperations:  []auth.Operation{auth.OperationApprovalSweep},
				PermittedSourceIDs: [][]byte{scope.SourceID}, PermittedPolicyIDs: [][]byte{scope.PolicyID},
				PolicyGeneration: 3, AuthenticationExpires: now.Add(time.Minute),
				RequestID: "sweep", CorrelationID: "sweep",
			})
			if err != nil {
				return nil, err
			}
			// The budget runs out after the first record's write.
			defer cancel()
			return authority.Binder().Bind(context.WithoutCancel(bindCtx), decision)
		},
	}
	result, err := service.Sweep(ctx, ApprovalSweepRequest{Limit: MaxApprovalListResults})
	if !errors.Is(err, context.Canceled) || result.Examined != 1 || result.Expired != 1 ||
		string(result.Next) != string(first.ID) {
		t.Fatalf("a sweep stopped mid-page = %+v, %v", result, err)
	}
}
