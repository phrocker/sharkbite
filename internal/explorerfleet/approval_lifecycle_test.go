// Licensed to the Apache Software Foundation (ASF) under one or more
// contributor license agreements. See the NOTICE file distributed with this
// work for additional information regarding copyright ownership.
package explorerfleet

import (
	"context"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/phrocker/shoal-oss/pkg/explorer/auth"
	"github.com/phrocker/shoal-oss/pkg/explorer/fleet"
	"github.com/phrocker/shoal-oss/pkg/interaction"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

// TestApprovalStoreRefusesAWriteOutsideTheLifecycle (#655): the durable store
// holds the lifecycle table, so a writer that reads version N of a final
// record and writes N+1 cannot re-enable it, and a pending request cannot
// skip its approval to become enqueued. Each probe is valid by Validate and
// changes no set-once field, so the refusal is the table's. A withdrawn
// record's own fields are set once as well.
func TestApprovalStoreRefusesAWriteOutsideTheLifecycle(t *testing.T) {
	ctx := context.Background()
	open := func(t *testing.T) (*ApprovalStore, fleet.ApprovalRecord, func()) {
		t.Helper()
		runtime := openDispatchRuntime(t, t.TempDir())
		store, err := NewApprovalStore(runtime, nil)
		if err != nil {
			_ = runtime.Close()
			t.Fatal(err)
		}
		created, err := store.ApplyApproval(ctx, fleet.ApprovalMutation{
			Token: []byte("approval-request"), Record: pendingApprovalRecord(),
		})
		if err != nil {
			_ = runtime.Close()
			t.Fatal(err)
		}
		return store, created, func() { _ = runtime.Close() }
	}
	next := func(current fleet.ApprovalRecord, state fleet.ApprovalState) fleet.ApprovalRecord {
		record := fleet.CloneApprovalRecord(current)
		record.Version++
		record.State = state
		record.UpdatedAt = current.UpdatedAt.Add(time.Second)
		return record
	}
	apply := func(
		store *ApprovalStore, record fleet.ApprovalRecord, expected uint64,
	) (fleet.ApprovalRecord, error) {
		return store.ApplyApproval(ctx, fleet.ApprovalMutation{
			Token: []byte("approval-" + string(record.State)), Record: record,
			ExpectedVersion: expected,
		})
	}
	decide := func(record *fleet.ApprovalRecord) {
		record.Verdict = fleet.ApprovalVerdictApprove
		record.ApproverSubject = "oidc:https://issuer.example#bob"
		record.ApproverActor = record.ApproverSubject
		record.ApproverFingerprint = auth.Fingerprint{7}
		record.ApproverPolicyGeneration = record.PolicyGeneration
		record.DecidedAt = record.UpdatedAt
		record.DecisionRequestID = "decision"
	}
	refused := func(t *testing.T, store *ApprovalStore, current, rewritten fleet.ApprovalRecord, want string) {
		t.Helper()
		if err := rewritten.Validate(); err != nil {
			t.Fatalf("the probe cannot express its write validly: %v", err)
		}
		_, err := apply(store, rewritten, current.Version)
		if !shoal.IsErrorCode(err, shoal.ErrorInternal) || !strings.Contains(err.Error(), want) {
			t.Fatalf("write %s -> %s = %v, want %q", current.State, rewritten.State, err, want)
		}
		stored, readErr := store.GetApproval(ctx, current.ID)
		if readErr != nil || !reflect.DeepEqual(stored, current) {
			t.Fatalf("the refused write changed the record: %v %#v", readErr, stored)
		}
	}

	t.Run("expired back to pending", func(t *testing.T) {
		store, created, done := open(t)
		defer done()
		expired, err := apply(store, next(created, fleet.ApprovalExpired), created.Version)
		if err != nil {
			t.Fatal(err)
		}
		refused(t, store, expired, next(expired, fleet.ApprovalPending), "is not allowed")
	})
	t.Run("expired to approved", func(t *testing.T) {
		store, created, done := open(t)
		defer done()
		expired, err := apply(store, next(created, fleet.ApprovalExpired), created.Version)
		if err != nil {
			t.Fatal(err)
		}
		approved := next(expired, fleet.ApprovalApproved)
		decide(&approved)
		refused(t, store, expired, approved, "is not allowed")
	})
	t.Run("pending straight to enqueued", func(t *testing.T) {
		store, created, done := open(t)
		defer done()
		enqueued := next(created, fleet.ApprovalEnqueued)
		decide(&enqueued)
		enqueued.MaterializedAt = enqueued.UpdatedAt
		refused(t, store, created, enqueued, "is not allowed")
	})
	t.Run("withdrawn is set once", func(t *testing.T) {
		store, created, done := open(t)
		defer done()
		withdrawn := next(created, fleet.ApprovalWithdrawn)
		withdrawn.WithdrawnAt = withdrawn.UpdatedAt
		withdrawn.WithdrawalReason = interaction.Reason{Code: "changed"}
		stored, err := apply(store, withdrawn, created.Version)
		if err != nil {
			t.Fatalf("a withdrawal was refused: %v", err)
		}
		again := next(stored, fleet.ApprovalWithdrawn)
		again.WithdrawnAt = again.UpdatedAt
		refused(t, store, stored, again, "withdrawal time is immutable")
		again = next(stored, fleet.ApprovalWithdrawn)
		again.UpdatedAt = stored.UpdatedAt
		again.WithdrawalReason = interaction.Reason{Code: "other"}
		refused(t, store, stored, again, "withdrawal reason is immutable")
	})
	t.Run("stranded is set once", func(t *testing.T) {
		store, created, done := open(t)
		defer done()
		stranded := next(created, fleet.ApprovalStranded)
		stranded.StrandedCause = fleet.ApprovalConditionTargetMoved
		stored, err := apply(store, stranded, created.Version)
		if err != nil {
			t.Fatalf("a stranding was refused: %v", err)
		}
		again := next(stored, fleet.ApprovalStranded)
		again.StrandedCause = fleet.ApprovalConditionPolicyMoved
		refused(t, store, stored, again, "stranding cause is immutable")
	})
}
