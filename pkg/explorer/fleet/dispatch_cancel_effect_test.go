// Licensed under the Apache License, Version 2.0. See LICENSE and NOTICE.
package fleet

import (
	"errors"
	"testing"
	"time"
)

// A gateway may disappear after sending to its target but before completing
// its claim. Claim-time EffectPossible is the durable fact that survives that
// gap; cancellation must not replace it with a statement that work was refused.
func TestCancelPreservesPossibleExternalEffectAfterLeaseExpiry(t *testing.T) {
	fixture := newExecutorClaimFixture(t)
	stored := fixture.registryStore.records["agent"]
	descriptor := cloneDescriptor(stored.Descriptor)
	descriptor.Capabilities[0].Actions[0].Effects = Effects{EffectMutatesExternal}
	stored.Descriptor = descriptor
	fixture.registryStore.records["agent"] = stored
	fixture.executors["exec"] = ceilingExecutor{ceiling: everyEffect()}

	enqueue := dispatchEnqueue(fixture.now, "request")
	enqueue.ID = []byte("external-action")
	enqueue.IdempotencyKey = []byte("external-key")
	queued, err := fixture.service.Enqueue(fixture.enqueuer, enqueue)
	if err != nil {
		t.Fatal(err)
	}
	first, err := fixture.service.Claim(fixture.enqueuer, ClaimRequest{
		ID: queued.ID, ExpectedVersion: queued.Version, ClaimID: []byte("first"),
		Lease: time.Minute, Context: dispatchContext(fixture.now, "request"),
	})
	if err != nil || !first.EffectPossible || first.ClaimFence != 1 {
		t.Fatalf("first claim = %+v, %v", first, err)
	}

	fixture.advance(t, 2*time.Minute)
	if _, err := fixture.service.Cancel(fixture.enqueuer, CancelRequest{
		ID: first.ID, ExpectedVersion: first.Version,
		MutationKey: []byte("cancel-external"),
		Context:     dispatchContext(fixture.now, "request"),
	}); !errors.Is(err, ErrCancelClaimUncertain) {
		t.Fatalf("cancel after possible external effect = %v, want refusal", err)
	}
	current, err := fixture.dispatchStore.GetAction(fixture.enqueuer, first.ID)
	if err != nil || current.State != DispatchClaimed ||
		current.Version != first.Version || !current.EffectPossible {
		t.Fatalf("cancel rewrote the possible effect: %+v, %v", current, err)
	}
	second, err := fixture.service.Claim(fixture.enqueuer, ClaimRequest{
		ID: first.ID, ExpectedVersion: first.Version, ClaimID: []byte("second"),
		Lease: time.Minute, Context: dispatchContext(fixture.now, "request"),
	})
	if err != nil || second.ClaimFence != 2 || !second.EffectPossible {
		t.Fatalf("reclaim after refused cancellation = %+v, %v", second, err)
	}
	reported, err := fixture.service.ReportAmbiguity(fixture.enqueuer, AmbiguityRequest{
		ID: first.ID, ClaimFence: first.ClaimFence,
		Outcome: AmbiguityOutcomeUnknown, Target: "external-target",
		Context: dispatchContext(fixture.now, "request"),
	})
	if err != nil || reported.ClaimFence != second.ClaimFence ||
		len(reported.AmbiguityReports) != 1 ||
		reported.AmbiguityReports[0].ClaimFence != first.ClaimFence {
		t.Fatalf("old holder's reconciliation report = %+v, %v", reported, err)
	}
}

func TestCancelStillAllowsLapsedClaimWithoutPossibleExternalEffect(t *testing.T) {
	fixture := newExecutorClaimFixture(t)
	first, err := fixture.service.Claim(fixture.enqueuer, ClaimRequest{
		ID: fixture.queued.ID, ExpectedVersion: fixture.queued.Version,
		ClaimID: []byte("internal"), Lease: time.Minute,
		Context: dispatchContext(fixture.now, "request"),
	})
	if err != nil || first.EffectPossible {
		t.Fatalf("internal claim = %+v, %v", first, err)
	}
	fixture.advance(t, 2*time.Minute)
	canceled, err := fixture.service.Cancel(fixture.enqueuer, CancelRequest{
		ID: first.ID, ExpectedVersion: first.Version,
		MutationKey: []byte("cancel-internal"),
		Context:     dispatchContext(fixture.now, "request"),
	})
	if err != nil || canceled.State != DispatchCanceled {
		t.Fatalf("cancellation of claim with no possible external effect = %+v, %v", canceled, err)
	}
}
