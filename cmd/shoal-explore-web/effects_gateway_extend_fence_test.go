// Licensed to the Apache Software Foundation (ASF) under one or more
// contributor license agreements. See the NOTICE file distributed with this
// work for additional information regarding copyright ownership.
package main

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/phrocker/shoal-oss/internal/effectsgateway"
	dispatchapi "github.com/phrocker/shoal-oss/pkg/dispatch/api"
)

// TestTheGatewayRenewsOnTheFenceAfterAFormerHoldersReport (#629): a claim
// lapses and is re-claimed; the displaced former holder reports its
// ambiguity, which moves the record's version and leaves the new claim
// alone; the current holder, still holding the version its claim returned,
// renews on its fence and the renewal lands. A renewal that pinned the
// version it holds would be refused here, stranding the live claim. The
// former holder's own renewal is refused as fence_lost.
func TestTheGatewayRenewsOnTheFenceAfterAFormerHoldersReport(t *testing.T) {
	g := newGatewayOps(t)
	ctx := context.Background()
	g.enqueueUntil("stripe-fence", "alice-trace-fence", g.h.now().Add(time.Hour))
	client := g.gateway(testExecutorRef)

	_, former, formerClaimID := g.pullAndClaim(client, "stripe-fence", "alice-trace-fence", time.Minute)
	g.h.advance(time.Minute + time.Second)
	client = g.gateway(testExecutorRef)
	_, current, currentClaimID := g.pullAndClaim(client, "stripe-fence", "alice-trace-fence", time.Minute)
	if current.ClaimFence == former.ClaimFence {
		t.Fatalf("the re-claim kept fence %d", current.ClaimFence)
	}

	// Between the claim and the extend: the displaced holder's report.
	reported, err := client.ReportAmbiguity(ctx, former.ID, effectsgateway.AmbiguityReport{
		Context:    former.Correlate(g.context("gateway_ambiguity")),
		ClaimFence: former.ClaimFence, Outcome: dispatchapi.AmbiguityOutcomeUnknown,
		Target: "api.stripe.test",
	})
	if err != nil {
		t.Fatalf("the former holder's report: %v", err)
	}
	if reported.Version == current.Version {
		t.Fatalf("this test is about a conflict that cannot arise: the record is still at "+
			"version %d, the version the current holder holds", current.Version)
	}
	if reported.ClaimFence != current.ClaimFence {
		t.Fatalf("the report moved the claim to fence %d", reported.ClaimFence)
	}

	extended, err := client.Extend(ctx, current.ID, effectsgateway.ExtendRequest{
		Context: current.Correlate(g.context("gateway_extend")),
		ClaimID: currentClaimID, ClaimFence: current.ClaimFence, Lease: 2 * time.Minute,
	})
	if err != nil {
		t.Fatalf("the current holder's fence-bound renewal after the report: %v", err)
	}
	if extended.ClaimFence != current.ClaimFence || extended.Version <= reported.Version ||
		!extended.ClaimLeaseUntil.Equal(g.h.now().Add(2*time.Minute)) {
		t.Fatalf("renewed = fence %d version %d lease %v", extended.ClaimFence,
			extended.Version, extended.ClaimLeaseUntil)
	}

	// The displaced holder cannot renew the claim another attempt holds.
	_, err = client.Extend(ctx, former.ID, effectsgateway.ExtendRequest{
		Context: former.Correlate(g.context("gateway_extend")),
		ClaimID: formerClaimID, ClaimFence: former.ClaimFence, Lease: time.Minute,
	})
	var dispatchErr *effectsgateway.DispatchError
	if effectsgateway.DispatchKind(err) != effectsgateway.DispatchFenceLost ||
		!errors.As(err, &dispatchErr) ||
		(dispatchErr.Status != http.StatusConflict && dispatchErr.Status != http.StatusNotFound) {
		t.Fatalf("the former holder's renewal = %v, want fence_lost", err)
	}

	completed, err := g.complete(client, current, currentClaimID, current.Version)
	if err != nil || completed.State != dispatchapi.DispatchSucceeded {
		t.Fatalf("complete = %+v, %v", completed.State, err)
	}
}
