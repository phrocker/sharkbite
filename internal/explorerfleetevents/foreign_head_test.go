// Licensed to the Apache Software Foundation (ASF) under one or more
// contributor license agreements. See the NOTICE file distributed with this
// work for additional information regarding copyright ownership.
package explorerfleetevents

import (
	"bytes"
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/phrocker/shoal-oss/internal/explorercoord"
	"github.com/phrocker/shoal-oss/pkg/explorer/coordination"
	"github.com/phrocker/shoal-oss/pkg/explorer/coordination/guard"
	"github.com/phrocker/shoal-oss/pkg/explorer/coordination/transaction"
	"github.com/phrocker/shoal-oss/pkg/explorer/fleetevents"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

const publishOperation = "fleet-event-publish-v1"

// countingRuntime counts the adapter's event publications and can run a hook
// just before the first one reaches the runtime, after the adapter has read
// every head it plans against.
type countingRuntime struct {
	Runtime
	mu       sync.Mutex
	attempts int
	before   func()
}

func (r *countingRuntime) Publish(
	ctx context.Context, request explorercoord.Request,
) (explorercoord.Result, error) {
	if string(request.Intent.Operation) == publishOperation {
		r.mu.Lock()
		r.attempts++
		hook := r.before
		r.before = nil
		r.mu.Unlock()
		if hook != nil {
			hook()
		}
	}
	return r.Runtime.Publish(ctx, request)
}

func (r *countingRuntime) publishAttempts() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.attempts
}

// publicationEventID is the event ID Append derives for a request, the hex of
// which is its 'P' guard entity. A publisher can compute it in advance.
func publicationEventID(request fleetevents.PublishRequest) []byte {
	publicationID := digest("fleet-event-publication-id-v2", request.Token)
	return digest("fleet-event-id-v2", publicationID, encodeTime(request.RetryUntil))
}

// squat writes a head at entity under another store's logical policy, as a
// fleet approval at ID hex(eventID) does: approvals also use kind 'P'.
func squat(
	t *testing.T, runtime *explorercoord.Runtime, domain coordination.DomainID,
	entity guard.Entity,
) {
	t.Helper()
	if err := writeUnder(
		t, runtime, domain, entity, "fleet/approval/v1"); err != nil {
		t.Fatalf("the foreign store's write: %v", err)
	}
}

// writeUnder publishes one cell guarded by entity, created absent or
// identical under policy.
func writeUnder(
	t *testing.T, runtime *explorercoord.Runtime, domain coordination.DomainID,
	entity guard.Entity, policy string,
) error {
	t.Helper()
	lpart, err := explorercoord.Partition(domain, entity.ID)
	if err != nil {
		t.Fatal(err)
	}
	row := append([]byte("zz-"+policy+"-"), entity.ID...)
	_, err = runtime.Publish(context.Background(), explorercoord.Request{
		Intent: explorercoord.Intent{
			Operation: []byte("foreign-store-write"), Token: row,
			Cells: []explorercoord.Cell{{
				Table: Table, Row: row, Family: recordFamily,
				Qualifier: recordQualifier, Value: []byte("foreign"),
				EpochTimestamp: true, LPART: lpart, CopyGeneration: 1,
			}},
			Guards: []explorercoord.GuardIntent{{
				Entity: entity, Mode: guard.ModeAbsentOrIdentical,
				DesiredState: guard.StateLive, DesiredWinnerID: []byte("foreign"),
				LPART: lpart, LogicalPolicyID: []byte(policy),
				RetirementGeneration: 1,
			}},
			Results: []explorercoord.ResultIdentity{{
				Kind: []byte("foreign-store-write"), ID: row,
			}},
		},
	})
	return err
}

// TestTheRuntimeKeepsAnOwnershipRefusalTyped: the guard's refusal reaches a
// runtime caller as guard.ErrForeignHead, still a transaction conflict. The
// coordinator retries a conflicted publication, and a retry reads back only
// the conflicted root's bare conflict, so it must return this refusal as it
// is instead.
func TestTheRuntimeKeepsAnOwnershipRefusalTyped(t *testing.T) {
	config := runtimeConfig(t.TempDir())
	runtime, err := explorercoord.Open(config)
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Close()
	entity := guard.Entity{Kind: 'P', ID: coordination.EntityID("shared-id")}
	squat(t, runtime, config.Domain, entity)
	err = writeUnder(t, runtime, config.Domain, entity, string(logicalPolicy))
	if !errors.Is(err, guard.ErrForeignHead) ||
		!errors.Is(err, transaction.ErrConflict) ||
		errors.Is(err, explorercoord.ErrIndeterminatePublication) {
		t.Fatalf("a write over another store's head = %v, want a typed, "+
			"definite ownership refusal", err)
	}
}

func assertRefused(t *testing.T, what string, err error) {
	t.Helper()
	if err == nil {
		t.Fatalf("%s: a publication adopted another store's head", what)
	}
	if errors.Is(err, fleetevents.ErrPublicationUnknown) {
		t.Fatalf("%s = %v; an ownership refusal is definite, not unknown", what, err)
	}
	if !errors.Is(err, fleetevents.ErrPublicationRefused) ||
		!errors.Is(err, guard.ErrForeignHead) ||
		!shoal.IsErrorCode(err, shoal.ErrorConflict) {
		t.Fatalf("%s = %v, want a definite refusal (conflict)", what, err)
	}
}

// TestAppendReportsAForeignHeadAsADefiniteRefusal: a publication whose 'P'
// entity, hex(eventID), already belongs to another store (an approval) is
// refused by the guard's ownership rule (#670) for good. Append used to take
// that for a race, retry 32 times and report fleetevents.ErrPublicationUnknown,
// so a caller following the indeterminate contract retried until RetryUntil
// and never succeeded (#671). It is now reported as refused, at once and on
// every retry, and the stream is untouched.
func TestAppendReportsAForeignHeadAsADefiniteRefusal(t *testing.T) {
	cases := map[string]struct {
		// late puts the foreign head in place after the adapter has read
		// the heads it plans against, so that only the guard can refuse.
		late         bool
		wantAttempts int
	}{
		"foreign head already in place":  {late: false, wantAttempts: 0},
		"foreign head lands mid-attempt": {late: true, wantAttempts: 1},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			config := runtimeConfig(t.TempDir())
			runtime, err := explorercoord.Open(config)
			if err != nil {
				t.Fatal(err)
			}
			defer runtime.Close()
			counted := &countingRuntime{Runtime: runtime}
			adapter, err := New(counted, config.Domain)
			if err != nil {
				t.Fatal(err)
			}
			now := time.Now().UTC()
			request := fleetevents.PublishRequest{
				Token: []byte("collide"), RetryUntil: now.Add(time.Hour),
				Event: testEvent(1),
			}
			entity := publicationEntity(publicationEventID(request))
			if tc.late {
				counted.before = func() { squat(t, runtime, config.Domain, entity) }
			} else {
				squat(t, runtime, config.Domain, entity)
			}

			_, err = adapter.Append(ctx, request, now)
			assertRefused(t, "the publication", err)
			if got := counted.publishAttempts(); got != tc.wantAttempts {
				t.Fatalf("publish attempts = %d, want %d: a refusal is not retried",
					got, tc.wantAttempts)
			}
			// The caller's retry of the same publication is refused the same
			// way, without a further attempt.
			_, err = adapter.Append(ctx, request, now)
			assertRefused(t, "the caller's retry", err)
			if got := counted.publishAttempts(); got != tc.wantAttempts {
				t.Fatalf("publish attempts after the retry = %d, want %d",
					got, tc.wantAttempts)
			}

			head, _, err := runtime.ReadEntity(ctx, entity)
			if err != nil || head == nil ||
				!bytes.Equal(head.LogicalPolicyID, []byte("fleet/approval/v1")) {
				t.Fatalf("the foreign head after the refusal = %+v, %v", head, err)
			}
			if stream, _, err := runtime.ReadEntity(ctx, streamEntity); err == nil &&
				stream != nil {
				t.Fatalf("the refused publication moved the stream: %+v", stream)
			}
			// The stream is not wedged: another publication goes through.
			other, err := adapter.Append(ctx, fleetevents.PublishRequest{
				Token: []byte("unrelated"), RetryUntil: now.Add(time.Hour),
				Event: testEvent(2),
			}, now)
			if err != nil || other.Sequence != 1 {
				t.Fatalf("a later, unrelated publication = %+v, %v", other, err)
			}
		})
	}
}

// TestAppendStillRetriesAGenuineRace: a conflict that is a race, here another
// writer moving the stream between Append's read of its head and its
// publication, is not an ownership refusal. Append still treats it as a race
// and retries, and never reports it as refused.
func TestAppendStillRetriesAGenuineRace(t *testing.T) {
	ctx := context.Background()
	config := runtimeConfig(t.TempDir())
	runtime, err := explorercoord.Open(config)
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Close()
	counted := &countingRuntime{Runtime: runtime}
	loser, err := New(counted, config.Domain)
	if err != nil {
		t.Fatal(err)
	}
	winner, err := New(runtime, config.Domain)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	counted.before = func() {
		if _, err := winner.Append(ctx, fleetevents.PublishRequest{
			Token: []byte("winner"), RetryUntil: now.Add(time.Hour),
			Event: testEvent(1),
		}, now); err != nil {
			t.Errorf("the racing publication: %v", err)
		}
	}
	_, err = loser.Append(ctx, fleetevents.PublishRequest{
		Token: []byte("loser"), RetryUntil: now.Add(time.Hour),
		Event: testEvent(2),
	}, now)
	if attempts := counted.publishAttempts(); attempts < 2 {
		t.Fatalf("publish attempts = %d (%v); a race must be retried", attempts, err)
	}
	if errors.Is(err, fleetevents.ErrPublicationRefused) ||
		errors.Is(err, guard.ErrForeignHead) {
		t.Fatalf("a race was reported as an ownership refusal: %v", err)
	}
	// A race that retries cannot win is still an unknown outcome, as before.
	if err != nil && !errors.Is(err, fleetevents.ErrPublicationUnknown) {
		t.Fatalf("the race's outcome = %v, want success or unknown", err)
	}
}
