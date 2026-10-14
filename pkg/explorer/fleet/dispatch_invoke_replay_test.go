// Licensed to the Apache Software Foundation (ASF) under one or more
// contributor license agreements. See the NOTICE file distributed with this
// work for additional information regarding copyright ownership.
package fleet

import (
	"testing"
	"time"

	"github.com/phrocker/shoal-oss/pkg/explorer/auth"
)

// invokeOnce runs an action to completion through Invoke and returns the
// request, so a replay can re-send the identical one.
func invokeOnce(
	t *testing.T, fixture *executorClaimFixture, id string,
) EnqueueRequest {
	t.Helper()
	fixture.bindExecutor(t, confiningExecutor{confines: true})
	enqueue := dispatchEnqueue(fixture.now, "request")
	enqueue.ID = []byte(id)
	enqueue.IdempotencyKey = []byte(id + "-idempotency")
	invoked, err := fixture.service.Invoke(fixture.enqueuer, InvokeRequest{
		Enqueue: enqueue,
		ClaimID: []byte("invoke-claim"), Lease: time.Minute,
	})
	if err != nil {
		t.Fatalf("the first invoke failed, so there is no terminal record "+
			"to replay: %v", err)
	}
	if invoked.State != DispatchSucceeded {
		t.Fatalf("the action is %q, so the replay branch is not reached",
			invoked.State)
	}
	return enqueue
}

// TestAnInvokeReplaySurvivesALapsedLease is #578.
//
// A caller retrying Invoke is typically the one that lost the response to a
// call whose effect already happened, and the terminal-replay branch is its
// route to learning the outcome. The enqueue's own resolve refused first when
// the descriptor's lease had lapsed, so the record was intact and
// unreachable — the effect stranded at the caller instead of in the record.
//
// A lease lapse means the registrar stopped renewing, not that the work is
// void. Revocation says something else and stays refused.
func TestAnInvokeReplaySurvivesALapsedLease(t *testing.T) {
	for _, probe := range []struct {
		name    string
		revoke  bool
		refused bool
	}{
		{name: "a lapsed lease is tolerated"},
		{name: "a revoked descriptor is refused", revoke: true, refused: true},
	} {
		t.Run(probe.name, func(t *testing.T) {
			fixture := newExecutorClaimFixture(t)
			enqueue := invokeOnce(t, fixture, "replay-action")

			stored := fixture.registryStore.records["agent"]
			descriptor := cloneDescriptor(stored.Descriptor)
			descriptor.LeaseExpiresAt = descriptor.UpdatedAt.Add(-time.Second)
			if probe.revoke {
				descriptor.RevokedAt = descriptor.UpdatedAt
			}
			stored.Descriptor = descriptor
			fixture.registryStore.records["agent"] = stored

			replayed, err := fixture.service.Invoke(
				fixture.enqueuer, InvokeRequest{
					Enqueue: enqueue,
					ClaimID: []byte("invoke-claim"), Lease: time.Minute,
				})
			if probe.refused {
				if err == nil {
					t.Fatal("a replay resolved against a revoked " +
						"descriptor; tolerating a lapse must not tolerate a " +
						"withdrawal")
				}
				return
			}
			if err != nil {
				t.Fatalf("a lapsed lease refused the replay, so a caller "+
					"that lost its response can never learn the outcome of "+
					"an effect that happened: %v", err)
			}
			if replayed.State != DispatchSucceeded {
				t.Fatalf("the replay returned state %q", replayed.State)
			}
		})
	}

	// The hard acceptance criterion, and the only reason this change is
	// delicate — but not the comparison I first wrote.
	//
	// My first version compared a stranger's refusal at an *occupied* ID
	// against an *absent* one and required them identical. That premise was
	// wrong: an enqueue legitimately names an ID nobody holds, so absent
	// succeeds and occupied conflicts, and that difference is the
	// idempotency contract rather than a leak. It is also why the admission
	// span — whose IDs *are* derivable from a knowable principal tuple — is
	// refused wholesale instead.
	//
	// What the pre-read must not do is change what a caller who does not
	// hold the record can see. It is principal-checked, so a stranger's
	// authorizedCurrentBinding answers ObjectNotFound, tolerateLapse stays
	// false, and the enqueue behaves exactly as before. The property with
	// teeth is therefore: a stranger never receives the owner's terminal
	// record. If the pre-read were not principal-checked, it would, and the
	// enqueue's replay branch would hand it over.
	t.Run("occupied and absent answer identically once lapsed", func(t *testing.T) {
		fixture := newExecutorClaimFixture(t)
		owned := invokeOnce(t, fixture, "owned-action")

		stranger, err := auth.NewDecision(auth.DecisionConfig{
			Subject: "stranger", Actor: "stranger-actor",
			AuthorizationDomain: []byte("domain"),
			AllowedOperations: []auth.Operation{
				auth.OperationDispatch, auth.OperationInvoke,
			},
			PermittedSourceIDs: [][]byte{[]byte("source")},
			PermittedPolicyIDs: [][]byte{[]byte("policy")},
			PolicyGeneration:   1,
			AuthenticationExpires: time.Date(
				2026, 9, 7, 0, 0, 0, 0, time.UTC),
			RequestID: "request", CorrelationID: "correlation",
		})
		if err != nil {
			t.Fatal(err)
		}
		strangerCtx := bindDecision(t, fixture.authority, stranger)

		// Lapsed, which is what makes the comparison meaningful. With a live
		// lease an occupied ID conflicts and an absent one succeeds, and that
		// difference is the idempotency contract rather than a leak — an
		// enqueue legitimately names an ID nobody holds. Once lapsed, both
		// must be the same refusal, and the pre-read's principal check is
		// what makes them so: without it a stranger's pre-read succeeds, the
		// enqueue resolves, and the occupied case answers conflict while the
		// absent one answers the lapse refusal. That is an occupancy oracle
		// (#398) and this is the probe that catches it — the version I wrote
		// first asserted "a stranger never receives the record", which passes
		// either way because equivalentEnqueue's Subject comparison refuses
		// the stranger regardless.
		stored := fixture.registryStore.records["agent"]
		descriptor := cloneDescriptor(stored.Descriptor)
		descriptor.LeaseExpiresAt = descriptor.UpdatedAt.Add(-time.Second)
		stored.Descriptor = descriptor
		fixture.registryStore.records["agent"] = stored

		absent := owned
		absent.ID = []byte("no-such-action")

		record, occupiedErr := fixture.service.Invoke(
			strangerCtx, InvokeRequest{
				Enqueue: owned,
				ClaimID: []byte("stranger-claim"), Lease: time.Minute,
			})
		_, absentErr := fixture.service.Invoke(
			strangerCtx, InvokeRequest{
				Enqueue: absent,
				ClaimID: []byte("stranger-claim"), Lease: time.Minute,
			})
		if occupiedErr == nil || absentErr == nil {
			t.Fatalf("a stranger's invoke succeeded once the lease lapsed "+
				"(occupied=%v absent=%v), so there are no refusals to "+
				"compare", occupiedErr, absentErr)
		}
		if occupiedErr.Error() != absentErr.Error() {
			t.Fatalf("an occupied ID and an absent one answer differently "+
				"once the lease has lapsed, so the pre-read discloses "+
				"occupancy (#398):\n  occupied: %v\n  absent:   %v",
				occupiedErr, absentErr)
		}
		// And nothing of the owner's came back alongside the error.
		if record.State.terminal() || len(record.Evidence) > 0 {
			t.Fatalf("a stranger received the owner's record: %#v", record)
		}
	})

	// And a re-send that is not the same work must still conflict rather
	// than being handed somebody's recorded outcome.
	t.Run("a non-equivalent re-send still conflicts", func(t *testing.T) {
		fixture := newExecutorClaimFixture(t)
		enqueue := invokeOnce(t, fixture, "conflict-action")
		different := enqueue
		different.IdempotencyKey = []byte("a-different-key")
		if _, err := fixture.service.Invoke(
			fixture.enqueuer, InvokeRequest{
				Enqueue: different,
				ClaimID: []byte("invoke-claim"), Lease: time.Minute,
			}); err == nil {
			t.Fatal("a re-send that is not the same work replayed the " +
				"recorded outcome, so a caller is told its different " +
				"request was already recorded")
		}
	})

}
