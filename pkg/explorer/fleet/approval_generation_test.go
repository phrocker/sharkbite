// Licensed to the Apache Software Foundation (ASF) under one or more
// contributor license agreements. See the NOTICE file distributed with this
// work for additional information regarding copyright ownership.
package fleet

import (
	"bytes"
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/phrocker/shoal-oss/pkg/explorer/auth"
)

// This test is scoped to one guard and deliberately runs WITHOUT the hosted
// interaction recorder. In the hosted composition a superseded approval is
// also refused by the recorder's interaction pin, which would mask whether the
// approval service itself refuses it. Here the approval recorder records
// nothing, so the only thing that can refuse is advance's own in-force check.

type memoryApprovalStore struct {
	mu      sync.Mutex
	records map[string]ApprovalRecord
}

func (s *memoryApprovalStore) GetApproval(
	_ context.Context, id []byte,
) (ApprovalRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	record, ok := s.records[string(id)]
	if !ok {
		return ApprovalRecord{}, ErrApprovalNotFound
	}
	return CloneApprovalRecord(record), nil
}

func (s *memoryApprovalStore) ApplyApproval(
	_ context.Context, mutation ApprovalMutation,
) (ApprovalRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	current, ok := s.records[string(mutation.Record.ID)]
	if (!ok && mutation.ExpectedVersion != 0) ||
		(ok && current.Version != mutation.ExpectedVersion) {
		return ApprovalRecord{}, ErrApprovalConflict
	}
	s.records[string(mutation.Record.ID)] = CloneApprovalRecord(mutation.Record)
	return CloneApprovalRecord(mutation.Record), nil
}

func (s *memoryApprovalStore) ScanApprovals(
	context.Context, []byte, int,
) (ApprovalPage, error) {
	return ApprovalPage{}, nil
}

type unguardedApprovalRecorder struct{}

func (unguardedApprovalRecorder) RecordApproval(context.Context, ApprovalAudit) error {
	return nil
}

type fixedGenerations struct{ generation int64 }

func (g fixedGenerations) CurrentPolicyGeneration(
	context.Context, []byte,
) (int64, error) {
	return g.generation, nil
}

// TestApprovalRefusesASupersededApprovalBeforeCommitting: an approval given
// under policy generation 3 is re-requested with a token still at 3, after the
// policy in force has moved to 4. advance must refuse before the approved →
// enqueued commit, leave the row approved, and create no work.
func TestApprovalRefusesASupersededApprovalBeforeCommitting(t *testing.T) {
	approval := decided(validApprovalRecord(t), ApprovalApproved, ApprovalVerdictApprove)
	now := approval.DecidedAt.Add(2 * time.Second)
	store := &memoryApprovalStore{records: map[string]ApprovalRecord{
		string(approval.ID): CloneApprovalRecord(approval),
	}}
	dispatchStore := newMemoryDispatchStore()
	service := func(inForce int64) *ApprovalService {
		return &ApprovalService{
			dispatch: &DispatchService{
				store: dispatchStore, outbox: dispatchStore,
				recorder: &dispatchRecorder{}, events: dispatchEvents{},
				clock: func() time.Time { return now },
			},
			store: store, recorder: unguardedApprovalRecorder{},
			narrowed:    func(context.Context) bool { return false },
			generations: fixedGenerations{generation: inForce},
			window:      DefaultApprovalWindow,
		}
	}
	// The token is at the request's generation, as a stale one would be.
	decision := approvalBranchDecision(t, approval.RequestedAt, auth.OperationDispatch)

	_, err := service(approval.PolicyGeneration+1).advance(
		context.Background(), decision, approval, now)
	if !errors.Is(err, ErrApprovalSuperseded) {
		t.Fatalf("advance under a superseded policy = %v", err)
	}
	stored, _ := store.GetApproval(context.Background(), approval.ID)
	if stored.State != ApprovalApproved {
		t.Fatalf("superseded approval moved to %q", stored.State)
	}
	if _, err := dispatchStore.GetAction(
		context.Background(), approval.ID); !errors.Is(err, ErrActionNotFound) {
		t.Fatalf("work was created under a superseded policy: %v", err)
	}

	// Control: the same call with the policy unchanged materializes, so the
	// refusal above is the generation check and nothing else.
	receipt, err := service(approval.PolicyGeneration).advance(
		context.Background(), decision, approval, now)
	if err != nil || receipt.State != ApprovalEnqueued ||
		!bytes.Equal(receipt.Action.ID, approval.ID) {
		t.Fatalf("control advance = %+v, %v", receipt, err)
	}
}

// TestAMaterializationRecorderFailureIsResumable is the counterexample to a
// claim I made in fleetDispatchError and should not have.
//
// That comment said every ErrRecordingUnavailable is raised from a RecordAction
// failure sitting immediately before the matching store write, "so the
// transition did not commit", and that this licenses answering it as a plain
// 503 with no indeterminate marker. It is false here. advance's
// ApprovalApproved arm commits the approved → enqueued transition and then
// falls through to materialize, whose recorder failure is raised with that
// approval write already durable.
//
// The 503 is still correct, and for a reason worth stating rather than
// assuming: nothing external happened, and the partial state is resumable. The
// approval row sits at ApprovalEnqueued, a re-request re-enters at that case,
// and materialize completes. The marker means "an effect may have happened";
// marking a resumable internal partial would tell a caller to reconcile
// against a target that was never contacted.
//
// Both halves are asserted, because the first without the second would license
// leaving a durable partial unreported, and the second without the first would
// be the false claim again.
func TestAMaterializationRecorderFailureIsResumable(t *testing.T) {
	approval := decided(
		validApprovalRecord(t), ApprovalApproved, ApprovalVerdictApprove)
	now := approval.DecidedAt.Add(2 * time.Second)
	store := &memoryApprovalStore{records: map[string]ApprovalRecord{
		string(approval.ID): CloneApprovalRecord(approval),
	}}
	dispatchStore := newMemoryDispatchStore()
	recorder := &dispatchRecorder{failPhase: "approval_enqueue"}
	service := &ApprovalService{
		dispatch: &DispatchService{
			store: dispatchStore, outbox: dispatchStore,
			recorder: recorder, events: dispatchEvents{},
			clock: func() time.Time { return now },
		},
		store: store, recorder: unguardedApprovalRecorder{},
		narrowed:    func(context.Context) bool { return false },
		generations: fixedGenerations{generation: approval.PolicyGeneration},
		window:      DefaultApprovalWindow,
	}
	decision := approvalBranchDecision(
		t, approval.RequestedAt, auth.OperationDispatch)

	_, err := service.advance(context.Background(), decision, approval, now)
	if !errors.Is(err, ErrRecordingUnavailable) {
		t.Fatalf("the materialization audit failure was not reported as a "+
			"recording failure: %v", err)
	}
	// The half that falsifies the comment: a durable transition landed.
	stored, _ := store.GetApproval(context.Background(), approval.ID)
	if stored.State != ApprovalEnqueued {
		t.Fatalf("approval state = %q, want %q: this test exists because "+
			"this write commits before the recorder failure, and if it no "+
			"longer does, fleetDispatchError's comment should be corrected "+
			"back", stored.State, ApprovalEnqueued)
	}
	if stored.Version != approval.Version+1 {
		t.Fatalf("approval version = %d, want %d",
			stored.Version, approval.Version+1)
	}
	// And no work exists, which is why the caller must retry rather than
	// reconcile: nothing was dispatched and nothing external was contacted.
	if _, err := dispatchStore.GetAction(
		context.Background(), approval.ID); !errors.Is(err, ErrActionNotFound) {
		t.Fatalf("an action exists despite the refused audit: %v", err)
	}

	// The half that licenses the unmarked 503: the retry completes it.
	recorder.failPhase = ""
	if _, err := service.advance(
		context.Background(), decision, stored, now,
	); err != nil {
		t.Fatalf("the retry did not resume the materialization, so the "+
			"partial is not resumable and the 503 must carry the "+
			"indeterminate marker after all: %v", err)
	}
	action, err := dispatchStore.GetAction(context.Background(), approval.ID)
	if err != nil {
		t.Fatalf("the resumed materialization created no work: %v", err)
	}
	if action.State != DispatchQueued {
		t.Fatalf("resumed action state = %q, want %q",
			action.State, DispatchQueued)
	}
}
