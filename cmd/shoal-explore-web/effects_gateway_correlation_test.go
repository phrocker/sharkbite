// Licensed to the Apache Software Foundation (ASF) under one or more
// contributor license agreements. See the NOTICE file distributed with this
// work for additional information regarding copyright ownership.
package main

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"github.com/phrocker/shoal-oss/internal/effectsgateway"
	dispatchapi "github.com/phrocker/shoal-oss/pkg/dispatch/api"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

// TestTheGatewayAcceptsExactlyTheCorrelationsTheExplorerAccepts holds the
// client's correlation check to the explorer's header validator,
// validateSuppliedCorrelationID, by calling both. A stricter client would
// make any action enqueued with a legal correlation unclaimable by every
// gateway, refused locally on every poll until its deadline; a looser one
// would send what the explorer refuses.
func TestTheGatewayAcceptsExactlyTheCorrelationsTheExplorerAccepts(t *testing.T) {
	samples := map[string]string{
		"empty":           "",
		"short":           "alice-trace-1",
		"256 bytes":       strings.Repeat("c", 256),
		"257 bytes":       strings.Repeat("c", 257),
		"1024 bytes":      strings.Repeat("c", shoal.MaxIDBytes),
		"1025 bytes":      strings.Repeat("c", shoal.MaxIDBytes+1),
		"1024 bytes UTF8": strings.Repeat("é", shoal.MaxIDBytes/2),
		"1026 bytes UTF8": strings.Repeat("é", shoal.MaxIDBytes/2+1),
		"non-ASCII":       "trace-ü-391",
		"a space":         "trace 391",
		"padded":          " trace",
		"a tab":           "trace\t391",
		"a newline":       "trace\n391",
		"a control":       "trace\x7f",
		"invalid UTF-8":   "trace\xff",
		"a bidi override": "trace‮",
		"zero-width":      "trace​",
	}
	for name, value := range samples {
		_, explorerErr := validateSuppliedCorrelationID(value)
		clientErr := effectsgateway.CheckCorrelationID([]byte(value))
		if (explorerErr == nil) != (clientErr == nil) {
			t.Errorf("%s (%d bytes): explorer accepts = %v, gateway accepts = %v",
				name, len(value), explorerErr == nil, clientErr == nil)
		}
	}
	// The header the client sends is the one the explorer reads.
	if effectsgateway.CorrelationIDHeader != CorrelationIDHeader {
		t.Errorf("the gateway sends %q, the explorer reads %q",
			effectsgateway.CorrelationIDHeader, CorrelationIDHeader)
	}
	// The boundaries themselves, so a bound that drifts on both sides at
	// once is still caught.
	for length, want := range map[int]bool{256: true, 257: true, 1024: true, 1025: false} {
		if got := effectsgateway.CheckCorrelationID(bytes.Repeat([]byte("c"), length)) == nil; got != want {
			t.Errorf("a %d-byte correlation: gateway accepts = %v, want %v", length, got, want)
		}
	}
}

// TestTheGatewayWorksAnActionWithAMaximalCorrelation: an action enqueued
// with a legal correlation of the explorer's full length is pulled, claimed,
// extended, reported on and completed, each request carrying it.
func TestTheGatewayWorksAnActionWithAMaximalCorrelation(t *testing.T) {
	g := newGatewayOps(t)
	ctx := context.Background()
	trace := "alice-" + strings.Repeat("t", shoal.MaxIDBytes-len("alice-"))
	g.enqueueUntil("stripe-long-trace", trace, g.h.now().Add(time.Hour))
	client := g.gateway(testExecutorRef)
	_, claimed, claimID := g.pullAndClaim(client, "stripe-long-trace", trace, time.Minute)
	extended, err := client.Extend(ctx, claimed.ID, effectsgateway.ExtendRequest{
		Context: claimed.Correlate(g.context("gateway_extend")),
		ClaimID: claimID, ClaimFence: claimed.ClaimFence, Lease: 2 * time.Minute,
	})
	if err != nil {
		t.Fatalf("extend: %v", err)
	}
	if _, err := client.ReportAmbiguity(ctx, claimed.ID, effectsgateway.AmbiguityReport{
		Context:    claimed.Correlate(g.context("gateway_ambiguity")),
		ClaimFence: claimed.ClaimFence, Outcome: dispatchapi.AmbiguityRequestNotSent,
	}); err != nil {
		t.Fatalf("ambiguity: %v", err)
	}
	if _, err := g.complete(client, claimed, claimID, extended.Version); err != nil {
		t.Fatalf("complete: %v", err)
	}
	for _, sent := range g.requests() {
		if !strings.HasSuffix(sent.path, "/actions/pull") && sent.correlation != trace {
			t.Fatalf("%s carried a %d-byte correlation, want the record's %d bytes",
				sent.path, len(sent.correlation), len(trace))
		}
	}
}
