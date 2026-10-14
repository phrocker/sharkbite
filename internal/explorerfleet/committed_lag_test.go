// Licensed to the Apache Software Foundation (ASF) under one or more
// contributor license agreements. See the NOTICE file distributed with this
// work for additional information regarding copyright ownership.
package explorerfleet

import (
	"context"
	"errors"
	"testing"

	"github.com/phrocker/shoal-oss/internal/explorercoord"
	"github.com/phrocker/shoal-oss/pkg/explorer"
	"github.com/phrocker/shoal-oss/pkg/explorer/coordination"
	"github.com/phrocker/shoal-oss/pkg/explorer/coordination/allocator"
	"github.com/phrocker/shoal-oss/pkg/explorer/coordination/guard"
	"github.com/phrocker/shoal-oss/pkg/explorer/fleet"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

// laggingRuntime answers every entity read with a live head at one epoch and
// every committed-cell read with "not visible". That is the #633 window: a
// claim or extend has advanced the epoch and the committed value for it has
// not landed for this reader yet.
type laggingRuntime struct{ epoch coordination.Epoch }

func (r laggingRuntime) Publish(
	context.Context, explorercoord.Request,
) (explorercoord.Result, error) {
	return explorercoord.Result{}, nil
}

func (r laggingRuntime) ReadEntity(
	context.Context, guard.Entity,
) (*guard.Head, *guard.Pending, error) {
	head := guard.Head{
		Generation: 1, State: guard.StateLive, Epoch: r.epoch,
	}
	return &head, nil, nil
}

func (r laggingRuntime) ScanCommitted(
	context.Context, explorercoord.CommittedScanRequest,
) (explorercoord.CommittedPage, error) {
	return explorercoord.CommittedPage{}, nil
}

func (r laggingRuntime) CurrentHead(
	context.Context,
) (coordination.AllocatorHeadV1, error) {
	return coordination.AllocatorHeadV1{}, nil
}

func (r laggingRuntime) ReadCommittedCell(
	_ context.Context,
	_ string,
	_, _, _, _ []byte,
	_ coordination.Epoch,
) (explorercoord.CommittedCell, bool, error) {
	return explorercoord.CommittedCell{}, false, nil
}

// TestACommittedValueLagIsNotAbsence is #633's correctness half.
//
// A live object read as absent is wrong on its own, and the gateway treated
// the 404 on its first Complete attempt as definite: it reported
// effect_observed and the action was re-claimed. Once the guard head is
// non-nil the object exists, so not-found is no longer a possible answer for
// these two reads — their entity key and their row key are the same, so a
// missing committed cell can only be a lag.
func TestACommittedValueLagIsNotAbsence(t *testing.T) {
	runtime := laggingRuntime{epoch: 7}
	ctx := context.Background()

	dispatch, err := NewDispatchStore(runtime, nil)
	if err != nil {
		t.Fatal(err)
	}
	approvals, err := NewApprovalStore(runtime, nil)
	if err != nil {
		t.Fatal(err)
	}

	for _, probe := range []struct {
		name    string
		subject string
		call    func() error
	}{
		{
			name: "an action read", subject: "action",
			call: func() error {
				_, err := dispatch.GetAction(ctx, []byte("action-one"))
				return err
			},
		},
		{
			name: "an approval read", subject: "approval",
			call: func() error {
				_, err := approvals.GetApproval(ctx, []byte("approval-one"))
				return err
			},
		},
	} {
		t.Run(probe.name, func(t *testing.T) {
			err := probe.call()
			if err == nil {
				t.Fatal("the lagging read succeeded, so this fixture is not " +
					"reaching the committed-value branch it is named for")
			}
			if shoal.IsErrorCode(err, shoal.ErrorNotFound) {
				t.Fatalf("err = %v, want unavailable: a non-nil head proves "+
					"the object exists, so reporting it absent is the #633 "+
					"bug and a gateway treats it as definite", err)
			}
			if !shoal.IsErrorCode(err, shoal.ErrorUnavailable) {
				t.Fatalf("err = %v, want unavailable", err)
			}
			// Named, so the message cannot drift back to another store's noun.
			if got := err.Error(); !contains(got, probe.subject) {
				t.Fatalf("err = %q, want it to name %q", got, probe.subject)
			}
			// And it must not be marked as a possibly-committed write.
			// writeError sets Shoal-Commit-Outcome: indeterminate from this
			// predicate, and a read-side lag committed nothing.
			if explorer.IsIndeterminateCommit(err) {
				t.Fatalf("err = %v is an indeterminate commit, so writeError "+
					"would tell a client a write may have landed; this is a "+
					"read lag", err)
			}
			// The check above is only meaningful if the predicate can see a
			// marked error at all. Marking this same error must be detected,
			// otherwise "unmarked" is indistinguishable from a predicate that
			// never fires and the assertion guards nothing.
			if !explorer.IsIndeterminateCommit(
				explorer.MarkIndeterminateCommit(err)) {
				t.Fatal("IsIndeterminateCommit does not detect an error it " +
					"was just asked to mark, so the assertion above cannot " +
					"fail and proves nothing")
			}
		})
	}
}

// TestATransitionReadStillAnswersNotFound pins the site that deliberately did
// not change, so nobody "fixes the inconsistency" and creates a retry trap.
//
// transitionEntity is keyed on id alone while transitionRow composes
// (actionID, version, id). A real transition id with a mismatched actionID or
// version reaches the same branch with a non-nil head, and that is genuine
// absence. Answering Unavailable there would make such a request retry
// forever.
func TestATransitionReadStillAnswersNotFound(t *testing.T) {
	dispatch, err := NewDispatchStore(laggingRuntime{epoch: 7}, nil)
	if err != nil {
		t.Fatal(err)
	}
	_, _, err = dispatch.readTransition(
		context.Background(), []byte("action-one"), 1, []byte("transition-one"))
	if !errors.Is(err, fleet.ErrActionNotFound) {
		t.Fatalf("transition read = %v, want fleet.ErrActionNotFound: its row "+
			"key is wider than its entity key, so this branch can be genuine "+
			"absence and a retryable answer would be a trap", err)
	}
	// And specifically not the retryable answer the other two now give.
	if shoal.IsErrorCode(err, shoal.ErrorUnavailable) {
		t.Fatalf("transition read = %v is retryable; a mismatched "+
			"actionID or version can never succeed, so a client would retry "+
			"forever", err)
	}
}

func contains(haystack, needle string) bool {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return true
		}
	}
	return false
}

// clearingRuntime lags for a bounded number of committed reads and then
// answers. It is the case the re-read exists for, and the one the
// always-lagging fixture above cannot express: with that one every attempt
// fails, so the loop is only ever observed through its exhaustion arm and a
// read that never retried at all would pass identically.
type clearingRuntime struct {
	laggingRuntime
	remaining *int
	value     []byte
}

func (r clearingRuntime) ReadCommittedCell(
	_ context.Context,
	_ string,
	_, _, _, _ []byte,
	_ coordination.Epoch,
) (explorercoord.CommittedCell, bool, error) {
	if *r.remaining > 0 {
		*r.remaining--
		return explorercoord.CommittedCell{}, false, nil
	}
	return explorercoord.CommittedCell{
		Cell: allocator.Cell{Value: append([]byte(nil), r.value...)},
	}, true, nil
}

// TestACommittedValueLagClearsOnReRead is the half that matters in production:
// the window closes, so the read must return the record rather than any error.
//
// Before this, the lag escaped the store and the layer above classified it —
// fleet.materializeRead reads "not-found that is not ErrActionNotFound" as
// "look again shortly" and ApprovalService.Request retries it. That worked,
// but it keyed on the error's identity, so relabelling the error silently
// disabled the retry and a concurrent approval re-request failed outright
// (caught by TestApprovalConcurrentReRequestsAgree in CI, not locally).
// Closing the window here means no caller has to recognize it.
func TestACommittedValueLagClearsOnReRead(t *testing.T) {
	record := testActionRecord()
	encoded, err := encodeAction(record)
	if err != nil {
		t.Fatal(err)
	}

	for _, probe := range []struct {
		name      string
		lagReads  int
		wantError bool
	}{
		{name: "clears on the first re-read", lagReads: 1},
		{name: "clears just inside the bound",
			lagReads: maxCommittedReadAttempts - 1},
		{name: "never clears", lagReads: maxCommittedReadAttempts,
			wantError: true},
	} {
		t.Run(probe.name, func(t *testing.T) {
			remaining := probe.lagReads
			store, err := NewDispatchStore(clearingRuntime{
				laggingRuntime: laggingRuntime{epoch: 7},
				remaining:      &remaining, value: encoded,
			}, nil)
			if err != nil {
				t.Fatal(err)
			}
			got, err := store.GetAction(context.Background(), record.ID)
			if probe.wantError {
				if err == nil {
					t.Fatal("a lag that outlasts the bound returned no error")
				}
				if !shoal.IsErrorCode(err, shoal.ErrorUnavailable) {
					t.Fatalf("err = %v, want unavailable", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("err = %v, want the record: the window closed after "+
					"%d lagging reads and the bound is %d, so this read must "+
					"not surface the lag to any caller",
					err, probe.lagReads, maxCommittedReadAttempts)
			}
			if string(got.ID) != string(record.ID) {
				t.Fatalf("record ID = %q, want %q", got.ID, record.ID)
			}
			// The fixture must actually have lagged, or this case passes on a
			// read that never retried.
			if remaining != 0 {
				t.Fatalf("%d lagging reads were never consumed, so the "+
					"re-read was not exercised", remaining)
			}
		})
	}
}
