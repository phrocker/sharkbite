// Licensed to the Apache Software Foundation (ASF) under one or more
// contributor license agreements. See the NOTICE file distributed with this
// work for additional information regarding copyright ownership.
package fleet

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/phrocker/shoal-oss/pkg/shoal"
)

// displacedHolder sets up the situation ExtendRequest.ClaimFence exists for:
// one worker whose claim lapsed and is retained in ClaimHistory, and the
// worker that took the claim over and holds it live.
func displacedHolder(t *testing.T, fixture *executorClaimFixture) (
	former context.Context, formerFence uint64,
	holder context.Context, held ActionRecord,
) {
	t.Helper()
	former, formerFence, claimed := fixture.lapsedClaimant(t, "former")
	holder = fixture.namedExecutor(t, "holder")
	held, err := fixture.service.Claim(holder, ClaimRequest{
		ID: fixture.queued.ID, ExpectedVersion: claimed.Version,
		ClaimID: []byte("holder-claim"), Lease: time.Minute,
		Context: dispatchContext(fixture.now, "holder-request"),
	})
	if err != nil {
		t.Fatalf("the second worker could not take the lapsed claim: %v", err)
	}
	if held.ClaimFence == formerFence {
		t.Fatalf("both claims are at fence %d, so nothing here distinguishes "+
			"a displaced holder from the current one", formerFence)
	}
	return former, formerFence, holder, held
}

// TestAFenceBoundRenewalSurvivesAnUnrelatedWrite is the #391 PR4 case, stated
// as the thing that was impossible before the fence binding.
//
// A former holder reporting an ambiguity is a legitimate write that moves the
// record's version and deliberately leaves the fence alone (#438). The current
// holder cannot learn the new version by any route it is authorized for, so a
// version-bound renewal conflicts and the claim runs out at its current lease
// end — while the holder still holds the fence and could still complete. That
// stranded a long-running effect mid-flight for no reason.
func TestAFenceBoundRenewalSurvivesAnUnrelatedWrite(t *testing.T) {
	fixture := newExecutorClaimFixture(t)
	former, formerFence, holder, held := displacedHolder(t, fixture)

	reported, err := fixture.service.ReportAmbiguity(former, AmbiguityRequest{
		ID: fixture.queued.ID, ClaimFence: formerFence,
		Outcome: AmbiguityOutcomeUnknown,
		Context: dispatchContext(fixture.now, "former-request"),
	})
	if err != nil {
		t.Fatalf("the displaced holder could not report: %v", err)
	}
	// Both halves of the premise, checked rather than assumed: without the
	// version moving there is no conflict to fix, and if the fence moved the
	// holder would have lost the claim for a real reason.
	if reported.Version == held.Version {
		t.Fatal("the report did not move the version, so the conflict this " +
			"test is about cannot arise and it would pass either way")
	}
	if reported.ClaimFence != held.ClaimFence {
		t.Fatalf("the report moved the fence from %d to %d, so the holder's "+
			"claim really did end and this is not an unrelated write",
			held.ClaimFence, reported.ClaimFence)
	}

	fixture.advance(t, 30*time.Second)

	// The symptom. The holder pins the version it was handed at claim time,
	// which is the only version it ever saw.
	if _, err := fixture.service.ExtendClaim(holder, ExtendRequest{
		ID: fixture.queued.ID, ExpectedVersion: held.Version,
		ClaimID: []byte("holder-claim"), Lease: time.Minute,
		Context: dispatchContext(fixture.now, "holder-request"),
	}); !errors.Is(err, ErrActionConflict) {
		t.Fatalf("a version-bound renewal after an unrelated write = %v, "+
			"want ErrActionConflict; this is the behaviour the fence binding "+
			"exists to work around, so it is pinned rather than changed", err)
	}

	// The fix. Same claim, same holder, no version.
	extended, err := fixture.service.ExtendClaim(holder, ExtendRequest{
		ID: fixture.queued.ID, ClaimFence: held.ClaimFence,
		ClaimID: []byte("holder-claim"), Lease: time.Minute,
		Context: dispatchContext(fixture.now, "holder-request"),
	})
	if err != nil {
		t.Fatalf("a fence-bound renewal by the live holder = %v, want it to "+
			"succeed: the write that moved the version left this claim "+
			"untouched", err)
	}
	if extended.ClaimFence != held.ClaimFence {
		t.Fatalf("the renewal moved the fence from %d to %d",
			held.ClaimFence, extended.ClaimFence)
	}
	if !extended.ClaimLeaseUntil.After(reported.ClaimLeaseUntil) {
		t.Fatalf("the lease did not move forward: %s then %s",
			reported.ClaimLeaseUntil, extended.ClaimLeaseUntil)
	}

	// And the point of renewing: the holder can still finish. Deliberately
	// with the *stale* version it was handed at claim time, which is what it
	// actually holds — CompleteClaim requires a non-zero version but ignores
	// it in the fence branch, which is why completion never hit this and
	// renewal did.
	if _, err := fixture.service.CompleteClaim(holder, CompletionRequest{
		ID: fixture.queued.ID, ExpectedVersion: held.Version,
		ClaimFence: extended.ClaimFence,
		ClaimID:    []byte("holder-claim"),
		Result:     ExecutionResult{Output: json.RawMessage(`{"ok":true}`)},
		Context:    dispatchContext(fixture.now, "holder-request"),
	}); err != nil {
		t.Fatalf("the holder cannot complete after a fence-bound renewal: %v",
			err)
	}
}

// TestOnlyTheCurrentHolderMayRenewOnAFence pins the distinction that makes the
// fence binding safe, and the trap in implementing it.
//
// heldClaimAt is the obvious helper for "did this caller hold the claim at
// this fence" and is the wrong question here: it walks ClaimHistory and
// deliberately admits a *displaced* former holder, which is what the ambiguity
// route needs. Wired into renewal it would let a holder that lost the claim
// renew one another worker now holds — worse than the conflict being fixed.
// The right test is standing on the record as it is now, plus the fence.
func TestOnlyTheCurrentHolderMayRenewOnAFence(t *testing.T) {
	fixture := newExecutorClaimFixture(t)
	former, formerFence, _, held := displacedHolder(t, fixture)

	for _, probe := range []struct {
		name    string
		request ExtendRequest
	}{
		{
			// The fence the record genuinely held, and still retains.
			// heldClaimAt returns true for exactly this.
			name: "its own retained fence",
			request: ExtendRequest{
				ID: fixture.queued.ID, ClaimFence: formerFence,
				ClaimID: []byte("former-claim"), Lease: time.Minute,
				Context: dispatchContext(fixture.now, "former-request"),
			},
		},
		{
			// And knowing the live fence is not what grants a renewal: a
			// lapsed holder learns it from Pull once its own lease lapses.
			name: "the current holder's fence and claim",
			request: ExtendRequest{
				ID: fixture.queued.ID, ClaimFence: held.ClaimFence,
				ClaimID: []byte("holder-claim"), Lease: time.Minute,
				Context: dispatchContext(fixture.now, "former-request"),
			},
		},
	} {
		t.Run(probe.name, func(t *testing.T) {
			_, err := fixture.service.ExtendClaim(former, probe.request)
			if err == nil {
				t.Fatal("a displaced former holder renewed a claim it no " +
					"longer holds")
			}
			// Not-found, like every other refusal on this route: it is
			// reachable by every principal authorized to execute the
			// descriptor, so a distinguishable answer would confirm the
			// action exists and which fences it has been through.
			if !shoal.IsErrorCode(err, shoal.ErrorNotFound) {
				t.Fatalf("refusal = %v, want not-found", err)
			}
		})
	}
}

// TestAStaleFenceEndsTheRenewalAsALostClaim is the error-code half of the
// change, and the reason it is worth more than the liveness fix alone.
//
// A version-bound renewal answers ErrActionConflict, which a client can only
// read as "something moved" — it cannot tell a lost claim from an unrelated
// write, so it guesses. With a fence presented, a mismatch means exactly one
// thing, and the answer says so.
func TestAStaleFenceEndsTheRenewalAsALostClaim(t *testing.T) {
	fixture := newExecutorClaimFixture(t)
	_, formerFence, holder, held := displacedHolder(t, fixture)
	// Part way through the lease, so a renewal has somewhere to move it;
	// otherwise both calls below are refused for miscomputing the budget and
	// neither says anything about the fence.
	fixture.advance(t, 30*time.Second)

	if _, err := fixture.service.ExtendClaim(holder, ExtendRequest{
		ID: fixture.queued.ID, ClaimFence: formerFence,
		ClaimID: []byte("holder-claim"), Lease: time.Minute,
		Context: dispatchContext(fixture.now, "holder-request"),
	}); !errors.Is(err, ErrClaimLost) {
		t.Fatalf("a renewal on a fence the record has moved past = %v, want "+
			"ErrClaimLost", err)
	}

	// The live fence from the same caller still works, so the case above
	// turns on the fence and not on the caller or the claim ID.
	if _, err := fixture.service.ExtendClaim(holder, ExtendRequest{
		ID: fixture.queued.ID, ClaimFence: held.ClaimFence,
		ClaimID: []byte("holder-claim"), Lease: time.Minute,
		Context: dispatchContext(fixture.now, "holder-request"),
	}); err != nil {
		t.Fatalf("the same caller cannot renew on the live fence: %v", err)
	}
}

// TestARenewalMustPinSomething keeps the relaxed validation from accepting a
// request that pins neither binding. Before the fence existed a version was
// unconditionally required, so "neither" was impossible by construction.
func TestARenewalMustPinSomething(t *testing.T) {
	fixture := newExecutorClaimFixture(t)
	worker := fixture.namedExecutor(t, "worker")
	if _, err := fixture.service.Claim(worker, ClaimRequest{
		ID: fixture.queued.ID, ExpectedVersion: fixture.queued.Version,
		ClaimID: []byte("worker-claim"), Lease: time.Minute,
		Context: dispatchContext(fixture.now, "worker-request"),
	}); err != nil {
		t.Fatalf("the worker could not claim: %v", err)
	}
	_, err := fixture.service.ExtendClaim(worker, ExtendRequest{
		ID: fixture.queued.ID, ClaimID: []byte("worker-claim"),
		Lease:   time.Minute,
		Context: dispatchContext(fixture.now, "worker-request"),
	})
	if !shoal.IsErrorCode(err, shoal.ErrorInvalidArgument) {
		t.Fatalf("a renewal pinning neither a version nor a fence = %v, "+
			"want invalid-argument", err)
	}
}
