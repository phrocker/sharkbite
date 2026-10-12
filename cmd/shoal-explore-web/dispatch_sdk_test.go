// Licensed to the Apache Software Foundation (ASF) under one or more
// contributor license agreements. See the NOTICE file distributed with this
// work for additional information regarding copyright ownership.
package main

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"testing"
	"time"

	dispatchapi "github.com/phrocker/shoal-oss/pkg/dispatch/api"
	"github.com/phrocker/shoal-oss/pkg/sdk"
)

// sdkDispatch is the public SDK's dispatch client pointed at the executor
// world: the composition openService builds, behind the real authenticated
// handler and the real OIDC executor authenticator, holding the stripe
// executor's projected token. Nothing on the server side is a double.
func sdkDispatch(t *testing.T, g *gatewayOps) *dispatchapi.Client {
	t.Helper()
	g.record()
	client, err := sdk.New(sdk.Config{
		BaseURL: g.server.URL, HTTPClient: g.server.Client(),
		Token: func(context.Context) (string, error) {
			return g.executorToken(testExecutorSubject), nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	return client.Dispatch()
}

func (g *gatewayOps) sdkContext(reason string) dispatchapi.Context {
	g.n++
	return dispatchapi.Context{
		RequestID:  []byte("sdk-" + strconv.Itoa(g.n)),
		ReasonCode: reason, Deadline: g.h.now().Add(30 * time.Second),
	}
}

// TestSDKDispatchWorksAnActionThroughTheRealComposition drives
// sdk.Dispatch() through pull → claim → extend → ambiguity → complete
// against the real handler, and through the answers the client must not
// mistake: an extension or a report after the work is done, a report at a
// fence never held, and an identical completion resent.
func TestSDKDispatchWorksAnActionThroughTheRealComposition(t *testing.T) {
	g := newGatewayOps(t)
	ctx := context.Background()
	const trace = "alice-trace-sdk"
	g.enqueueUntil("stripe-sdk", trace, g.h.now().Add(time.Hour))
	client := sdkDispatch(t, g)

	poll := g.sdkContext("sdk_pull")
	poll.CorrelationID = []byte("sdk-poll-1")
	page, err := client.Pull(ctx, poll, "", dispatchapi.MaxDispatchListResults)
	if err != nil {
		t.Fatalf("pull: %v", err)
	}
	var offered dispatchapi.Record
	for _, record := range page.Records {
		if string(record.ID) == "stripe-sdk" {
			offered = record
		}
	}
	if offered.ID == nil || offered.State != dispatchapi.DispatchQueued ||
		string(offered.CorrelationID) != trace || offered.ExecutorKey != nil || offered.Input != nil {
		t.Fatalf("offered = %+v", offered)
	}
	if page.Date == "" || page.ReceivedAt.IsZero() {
		t.Fatalf("the page lacks what a worker estimates the explorer's clock from: %+v", page)
	}

	claimID := []byte("sdk|pod-0|claim-1")
	claimed, err := client.Claim(ctx, offered.ID, dispatchapi.Claim{
		Context: offered.Correlate(g.sdkContext("sdk_claim")), ExpectedVersion: offered.Version,
		ClaimID: claimID, Lease: time.Minute,
	})
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if claimed.State != dispatchapi.DispatchClaimed || claimed.ClaimFence == 0 ||
		len(claimed.ExecutorKey) != dispatchapi.ExecutorKeyBytes || string(claimed.Input) != `{}` {
		t.Fatalf("claimed = %+v", claimed)
	}

	// The renewal binds on the fence; the end granted is the explorer's.
	renewed, err := client.Extend(ctx, claimed.ID, dispatchapi.Extension{
		Context: claimed.Correlate(g.sdkContext("sdk_extend")), ClaimID: claimID,
		ClaimFence: claimed.ClaimFence, Lease: 2 * time.Minute,
	})
	if err != nil {
		t.Fatalf("extend: %v", err)
	}
	if renewed.ClaimFence != claimed.ClaimFence || renewed.Version <= claimed.Version ||
		!renewed.ClaimLeaseUntil.After(claimed.ClaimLeaseUntil) {
		t.Fatalf("renewed = %+v after claimed %+v", renewed, claimed)
	}

	// An ambiguity report under the held fence is recorded, and an identical
	// replay reads the same.
	report := dispatchapi.Ambiguity{
		Context: claimed.Correlate(g.sdkContext("sdk_ambiguity")), ClaimFence: claimed.ClaimFence,
		Outcome: dispatchapi.AmbiguityOutcomeUnknown, Target: "api.stripe.test", Reference: "ch_sdk",
	}
	reported, err := client.ReportAmbiguity(ctx, claimed.ID, report)
	if err != nil || len(reported.AmbiguityReports) != 1 ||
		reported.AmbiguityReports[0].Reference != "ch_sdk" {
		t.Fatalf("report = %+v, %v", reported.AmbiguityReports, err)
	}
	replayed, err := client.ReportAmbiguity(ctx, claimed.ID, report)
	if err != nil || replayed.Version != reported.Version || len(replayed.AmbiguityReports) != 1 {
		t.Fatalf("identical replay = %+v, %v", replayed, err)
	}

	// A fence never held: not found, so the report is unrecorded — never a
	// success, and never "nothing to report".
	never := report
	never.ClaimFence += 7
	_, err = client.ReportAmbiguity(ctx, claimed.ID, never)
	var dispatchErr *dispatchapi.Error
	if !errors.Is(err, dispatchapi.ErrAmbiguityUnrecorded) || !errors.As(err, &dispatchErr) ||
		dispatchErr.Status != http.StatusNotFound {
		t.Fatalf("a report at a fence never held = %v, want unrecorded (404)", err)
	}

	// Two version moves since the claim; the completion still binds on the
	// fence, sent with the version the claim answered.
	completion := dispatchapi.Completion{
		Context: claimed.Correlate(g.sdkContext("sdk_complete")), ExpectedVersion: claimed.Version,
		ClaimID: claimID, ClaimFence: claimed.ClaimFence, Output: []byte(`{"ok":true}`),
	}
	completed, err := client.Complete(ctx, claimed.ID, completion)
	if err != nil || completed.State != dispatchapi.DispatchSucceeded ||
		completed.ClaimFence != claimed.ClaimFence {
		t.Fatalf("complete = %+v, %v", completed, err)
	}
	// The identical report resent is answered from the record.
	again, err := client.Complete(ctx, claimed.ID, completion)
	if err != nil || again.Version != completed.Version {
		t.Fatalf("an identical resend = %+v, %v; want the committed record", again, err)
	}
	// A different report under the same claim is recorded otherwise or
	// refused, never taken as this one.
	different := completion
	different.Output = []byte(`{"ok":false}`)
	if _, err := client.Complete(ctx, claimed.ID, different); err == nil {
		t.Fatal("a different report under a completed claim was accepted")
	}

	// The work is done: the fence is lost.
	_, err = client.Extend(ctx, claimed.ID, dispatchapi.Extension{
		Context: claimed.Correlate(g.sdkContext("sdk_extend")), ClaimID: claimID,
		ClaimFence: claimed.ClaimFence, Lease: time.Minute,
	})
	if dispatchapi.Kind(err) != dispatchapi.KindFenceLost {
		t.Fatalf("extend after completion = %v, want fence_lost", err)
	}
}

// TestSDKDispatchResolvesTheExecutorsDescriptor: the executor's own
// descriptor resolves through sdk.Dispatch(), naming its ref.
func TestSDKDispatchResolvesTheExecutorsDescriptor(t *testing.T) {
	g := newGatewayOps(t)
	client := sdkDispatch(t, g)
	descriptor, err := client.Resolve(context.Background(), []byte("stripe-agent"), g.sdkContext("sdk_resolve"))
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if descriptor.ExecutorRef != testExecutorRef || len(descriptor.Capabilities) == 0 {
		t.Fatalf("descriptor = %+v", descriptor)
	}
}
