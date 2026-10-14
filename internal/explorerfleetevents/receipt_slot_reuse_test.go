// Licensed to the Apache Software Foundation (ASF) under one or more
// contributor license agreements. See the NOTICE file distributed with this
// work for additional information regarding copyright ownership.
package explorerfleetevents

import (
	"bytes"
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/phrocker/shoal-oss/internal/explorercoord"
	"github.com/phrocker/shoal-oss/pkg/explorer/auth"
	"github.com/phrocker/shoal-oss/pkg/explorer/fleetevents"
)

// TestAReceiptSlotIsReusedAcrossSubscriptionSlots: a create receipt slot
// reused after expiry by a subscription in a different subscription slot is
// mutated under a different LPART than it was created with. The guard's
// head-ownership rule must allow that (the owner is the logical policy), or
// every receipt slot becomes unusable once used. Found in review of the
// ownership fix, which first compared the LPART too.
func TestAReceiptSlotIsReusedAcrossSubscriptionSlots(t *testing.T) {
	ctx := context.Background()
	config := runtimeConfig(t.TempDir())
	runtime, err := explorercoord.Open(config)
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Close()
	adapter, err := New(runtime, config.Domain)
	if err != nil {
		t.Fatal(err)
	}
	mut := func(tok []byte) []byte {
		return digest("fleet-subscription-create-mutation-v1", []byte("subscriber"), tok)
	}
	sid := func(tok []byte) []byte { return digest("fleet-subscription-v1", []byte("subscriber"), tok) }
	first := []byte("first")
	receipt := adapter.subscriptionReceiptRow(createReceiptPrefix, mut(first))
	var second []byte
	for i := 0; i < 1_000_000; i++ {
		tok := []byte(fmt.Sprintf("second-%d", i))
		if bytes.Equal(adapter.subscriptionReceiptRow(createReceiptPrefix, mut(tok)), receipt) &&
			!bytes.Equal(adapter.subscriptionRow(sid(tok)), adapter.subscriptionRow(sid(first))) {
			second = tok
			break
		}
	}
	if second == nil {
		t.Fatal("no collision")
	}
	now := time.Date(2026, 9, 6, 6, 0, 0, 0, time.UTC)
	if _, _, err := adapter.Create(ctx, fleetevents.CreateRequest{
		Token: first, SubscriberID: "subscriber", AgentID: "agent", AgentGeneration: 1,
		TTL: time.Minute, RetryUntil: now.Add(time.Minute),
	}, auth.Fingerprint{1}, 1, now); err != nil {
		t.Fatal(err)
	}
	later := now.Add(10 * time.Minute)
	if _, _, err := adapter.Create(ctx, fleetevents.CreateRequest{
		Token: second, SubscriberID: "subscriber", AgentID: "agent", AgentGeneration: 1,
		TTL: time.Minute, RetryUntil: later.Add(time.Minute),
	}, auth.Fingerprint{1}, 1, later); err != nil {
		t.Fatalf("second create reusing an expired receipt slot = %v", err)
	}
}
