// Licensed to the Apache Software Foundation (ASF) under one or more
// contributor license agreements. See the NOTICE file distributed with this
// work for additional information regarding copyright ownership.
package fleet

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/phrocker/shoal-oss/pkg/shoal"
)

// rebindToEvidenceOnly declares an egressing action, claims it, and then
// rebinds the executor reference to an evidence-only ceiling — the host
// narrowing a binding while work taken under the old one is still in flight.
//
// Returns the claimed record so each phase can be driven against it.
func rebindToEvidenceOnly(
	t *testing.T, fixture *executorClaimFixture,
) ActionRecord {
	t.Helper()
	stored := fixture.registryStore.records["agent"]
	descriptor := cloneDescriptor(stored.Descriptor)
	descriptor.Capabilities[0].Actions[0].Effects =
		Effects{EffectEgressesContent}
	stored.Descriptor = descriptor
	fixture.registryStore.records["agent"] = stored
	// Wide enough to admit the declaration while the claim is taken.
	fixture.executors["exec"] = ceilingExecutor{ceiling: everyEffect()}

	claimed, err := fixture.service.Claim(fixture.enqueuer, ClaimRequest{
		ID: fixture.queued.ID, ExpectedVersion: fixture.queued.Version,
		ClaimID: []byte("claim"), Lease: time.Minute,
		Context: dispatchContext(fixture.now, "request"),
	})
	if err != nil {
		t.Fatalf("the claim was refused before the rebind, so this fixture "+
			"never reaches the case it is for: %v", err)
	}

	// The rebind: the same reference now points at an evidence-only
	// executor, so the action's declaration exceeds its ceiling.
	fixture.executors["exec"] = ceilingExecutor{ceiling: nil}
	if !descriptor.Capabilities[0].Actions[0].Effects.exceeds(
		executorCeiling(ceilingExecutor{ceiling: nil})) {
		t.Fatal("the rebound executor still admits the declaration, so this " +
			"fixture cannot produce the ceiling mismatch it is for")
	}
	return claimed
}

// TestARebindDoesNotStrandWorkAlreadyClaimed is the #391 follow-up.
//
// The effect ceiling and floor are re-checked at resolution, not only at
// registration, so a host that rebinds a reference to a narrower executor
// stops descriptors registered under the old one from resolving. That is
// right for *taking* work and wrong for *finishing* it: a completion and an
// ambiguity report describe an effect that already happened, and refusing
// them does not un-happen it — it loses the record, which is the failure
// EffectPossible and the ambiguity route exist to prevent.
//
// All four phases are pinned, not just the two ends. A fix that lifted the
// re-check everywhere would pass the completion case while silently letting
// new work start against a binding that no longer permits it.
func TestARebindDoesNotStrandWorkAlreadyClaimed(t *testing.T) {
	t.Run("complete is accepted", func(t *testing.T) {
		fixture := newExecutorClaimFixture(t)
		claimed := rebindToEvidenceOnly(t, fixture)
		if _, err := fixture.service.CompleteClaim(
			fixture.enqueuer, CompletionRequest{
				ID: claimed.ID, ExpectedVersion: claimed.Version,
				ClaimFence: claimed.ClaimFence, ClaimID: []byte("claim"),
				Result:  ExecutionResult{Output: json.RawMessage(`{"ok":true}`)},
				Context: dispatchContext(fixture.now, "request"),
			}); err != nil {
			t.Fatalf("a rebind stranded the completion of work already "+
				"claimed, so the effect happened and the record does not "+
				"say so: %v", err)
		}
	})

	t.Run("report ambiguity is accepted", func(t *testing.T) {
		fixture := newExecutorClaimFixture(t)
		claimed := rebindToEvidenceOnly(t, fixture)
		if _, err := fixture.service.ReportAmbiguity(
			fixture.enqueuer, AmbiguityRequest{
				ID: claimed.ID, ClaimFence: claimed.ClaimFence,
				Outcome: AmbiguityOutcomeUnknown,
				Target:  "payments.example.test", Reference: "ch_1",
				Context: dispatchContext(fixture.now, "request"),
			}); err != nil {
			t.Fatalf("a rebind stranded an ambiguity report, which is the "+
				"one route left for a worker that cannot say what "+
				"happened: %v", err)
		}
	})

	// The other half, and the reason this is four cases rather than two.
	t.Run("a new claim is refused", func(t *testing.T) {
		fixture := newExecutorClaimFixture(t)
		// Enqueued *before* the rebind, deliberately. Enqueue resolves the
		// binding too, so enqueuing afterwards is refused there and the
		// claim is never reached — which would make this case pass without
		// testing Claim at all.
		stored := fixture.registryStore.records["agent"]
		descriptor := cloneDescriptor(stored.Descriptor)
		descriptor.Capabilities[0].Actions[0].Effects =
			Effects{EffectEgressesContent}
		stored.Descriptor = descriptor
		fixture.registryStore.records["agent"] = stored
		fixture.executors["exec"] = ceilingExecutor{ceiling: everyEffect()}

		enqueue := dispatchEnqueue(fixture.now, "request")
		enqueue.ID = []byte("second-action")
		enqueue.IdempotencyKey = []byte("second-idempotency")
		queued, err := fixture.service.Enqueue(fixture.enqueuer, enqueue)
		if err != nil {
			t.Fatalf("the second enqueue was refused before the rebind: %v",
				err)
		}
		fixture.executors["exec"] = ceilingExecutor{ceiling: nil}
		if _, err := fixture.service.Claim(fixture.enqueuer, ClaimRequest{
			ID: queued.ID, ExpectedVersion: queued.Version,
			ClaimID: []byte("second-claim"), Lease: time.Minute,
			Context: dispatchContext(fixture.now, "request"),
		}); err == nil {
			t.Fatal("new work was claimed against a binding that no longer " +
				"permits the action's declared effects, so lifting the " +
				"re-check went too far")
		}
	})

	t.Run("an extension is refused", func(t *testing.T) {
		fixture := newExecutorClaimFixture(t)
		claimed := rebindToEvidenceOnly(t, fixture)
		if _, err := fixture.service.ExtendClaim(
			fixture.enqueuer, ExtendRequest{
				ID: claimed.ID, ExpectedVersion: claimed.Version,
				ClaimID: []byte("claim"), Lease: 2 * time.Minute,
				Context: dispatchContext(fixture.now, "request"),
			}); err == nil {
			t.Fatal("a claim was renewed against a binding that no longer " +
				"permits the action's declared effects; an extension takes " +
				"more time rather than reporting on time already spent, so " +
				"it belongs with Claim")
		}
	})
}

// lapseDescriptorLease puts the descriptor's lease in the past without
// touching the clock.
//
// Advancing the clock would also expire the claim lease, the admission token
// and the action deadline, so a refusal could not be attributed to the lapse
// — the check would pass while testing something else.
func lapseDescriptorLease(
	t *testing.T, store *memoryStore, agentID shoal.ID,
) {
	t.Helper()
	stored := store.records[agentID]
	if stored.Descriptor.LeaseExpiresAt.IsZero() {
		t.Fatal("the descriptor has no lease, so lapsing it is not the " +
			"condition this helper is for")
	}
	descriptor := cloneDescriptor(stored.Descriptor)
	descriptor.LeaseExpiresAt = descriptor.UpdatedAt.Add(-time.Second)
	if !descriptor.RevokedAt.IsZero() {
		t.Fatal("the descriptor is revoked, which is refused for a different " +
			"reason and would mask what this tests")
	}
	stored.Descriptor = descriptor
	store.records[agentID] = stored
}

// TestALapsedLeaseDoesNotStrandWorkOnTheInvokeRoute is the other half of
// #577, found by asking why the lapse toleration was gated on the execute
// route.
//
// #573 scoped it there to stay contained. The same argument applies to
// invoke: synchronous Invoke and the admission report path both complete
// through completeClaim under executorPhaseComplete, so a report arriving
// after the descriptor's lease lapsed lost the record of an effect that
// happened. The admission path is the one an LLM or effects gateway hits.
//
// A revoked descriptor stays refused — activeChainLapsing checks RevokedAt
// regardless of the flag — which is the line between "the operator stopped
// renewing" and "the operator withdrew this agent".
func TestALapsedLeaseDoesNotStrandWorkOnTheInvokeRoute(t *testing.T) {
	t.Run("complete is accepted", func(t *testing.T) {
		fixture := newExecutorClaimFixture(t)
		claimed, err := fixture.service.Claim(fixture.enqueuer, ClaimRequest{
			ID: fixture.queued.ID, ExpectedVersion: fixture.queued.Version,
			ClaimID: []byte("claim"), Lease: time.Minute,
			Context: dispatchContext(fixture.now, "request"),
		})
		if err != nil {
			t.Fatal(err)
		}
		lapseDescriptorLease(t, fixture.registryStore, "agent")
		if _, err := fixture.service.CompleteClaim(
			fixture.enqueuer, CompletionRequest{
				ID: claimed.ID, ExpectedVersion: claimed.Version,
				ClaimFence: claimed.ClaimFence, ClaimID: []byte("claim"),
				Result:  ExecutionResult{Output: json.RawMessage(`{"ok":true}`)},
				Context: dispatchContext(fixture.now, "request"),
			}); err != nil {
			t.Fatalf("a lapsed lease stranded an invoke-route completion: %v",
				err)
		}
	})

	t.Run("report ambiguity is accepted", func(t *testing.T) {
		fixture := newExecutorClaimFixture(t)
		claimed, err := fixture.service.Claim(fixture.enqueuer, ClaimRequest{
			ID: fixture.queued.ID, ExpectedVersion: fixture.queued.Version,
			ClaimID: []byte("claim"), Lease: time.Minute,
			Context: dispatchContext(fixture.now, "request"),
		})
		if err != nil {
			t.Fatal(err)
		}
		lapseDescriptorLease(t, fixture.registryStore, "agent")
		if _, err := fixture.service.ReportAmbiguity(
			fixture.enqueuer, AmbiguityRequest{
				ID: claimed.ID, ClaimFence: claimed.ClaimFence,
				Outcome: AmbiguityOutcomeUnknown,
				Target:  "payments.example.test", Reference: "ch_1",
				Context: dispatchContext(fixture.now, "request"),
			}); err != nil {
			t.Fatalf("a lapsed lease stranded an invoke-route ambiguity "+
				"report: %v", err)
		}
	})

	// The other half: a lapse must still stop new work.
	t.Run("a new claim is refused", func(t *testing.T) {
		fixture := newExecutorClaimFixture(t)
		lapseDescriptorLease(t, fixture.registryStore, "agent")
		if _, err := fixture.service.Claim(fixture.enqueuer, ClaimRequest{
			ID: fixture.queued.ID, ExpectedVersion: fixture.queued.Version,
			ClaimID: []byte("claim"), Lease: time.Minute,
			Context: dispatchContext(fixture.now, "request"),
		}); err == nil {
			t.Fatal("new work was claimed against a descriptor whose lease " +
				"had lapsed, so tolerating the lapse went too far")
		}
	})

	// And revocation is not a lapse. An operator who withdrew an agent has
	// said something different from one who stopped renewing it.
	t.Run("a revoked descriptor is still refused", func(t *testing.T) {
		fixture := newExecutorClaimFixture(t)
		claimed, err := fixture.service.Claim(fixture.enqueuer, ClaimRequest{
			ID: fixture.queued.ID, ExpectedVersion: fixture.queued.Version,
			ClaimID: []byte("claim"), Lease: time.Minute,
			Context: dispatchContext(fixture.now, "request"),
		})
		if err != nil {
			t.Fatal(err)
		}
		stored := fixture.registryStore.records["agent"]
		descriptor := cloneDescriptor(stored.Descriptor)
		descriptor.RevokedAt = descriptor.UpdatedAt
		stored.Descriptor = descriptor
		fixture.registryStore.records["agent"] = stored
		if _, err := fixture.service.CompleteClaim(
			fixture.enqueuer, CompletionRequest{
				ID: claimed.ID, ExpectedVersion: claimed.Version,
				ClaimFence: claimed.ClaimFence, ClaimID: []byte("claim"),
				Result:  ExecutionResult{Output: json.RawMessage(`{"ok":true}`)},
				Context: dispatchContext(fixture.now, "request"),
			}); err == nil {
			t.Fatal("a completion resolved against a revoked descriptor; " +
				"tolerating a lapse must not tolerate a withdrawal")
		}
	})
}
