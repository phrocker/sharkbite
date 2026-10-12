// Licensed to the Apache Software Foundation (ASF) under one or more
// contributor license agreements. See the NOTICE file distributed with this
// work for additional information regarding copyright ownership.
package explorerfleetevents

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/phrocker/shoal-oss/internal/explorercoord"
	"github.com/phrocker/shoal-oss/internal/explorerfleet"
	"github.com/phrocker/shoal-oss/internal/explorerfleetcap"
	"github.com/phrocker/shoal-oss/pkg/explorer/auth"
	"github.com/phrocker/shoal-oss/pkg/explorer/coordination"
	"github.com/phrocker/shoal-oss/pkg/explorer/coordination/guard"
	"github.com/phrocker/shoal-oss/pkg/explorer/fleet"
	"github.com/phrocker/shoal-oss/pkg/explorer/fleetevents"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

// legacyDeleteReceiptEntity is the guard entity a build before #669 gave the
// delete receipt at a legacy slot row. Anyone can compute all 4096 of them.
func (a *Adapter) legacyDeleteReceiptEntity(row []byte) guard.Entity {
	return a.subscriptionReceiptEntity('D', row)
}

// squattedSlots is how many legacy receipt slots the squatting test occupies
// beyond the victims' own (see the test).
const squattedSlots = 512

func legacyDeleteReceiptRows() [][]byte {
	rows := make([][]byte, 0, DefaultSubscriptionSlots)
	for slot := uint64(0); slot < DefaultSubscriptionSlots; slot++ {
		row := make([]byte, len(legacyDeleteReceiptPrefix)+8)
		copy(row, legacyDeleteReceiptPrefix)
		binary.BigEndian.PutUint64(row[len(legacyDeleteReceiptPrefix):], slot)
		rows = append(rows, row)
	}
	return rows
}

func deletionTokenFor(subscriberID shoal.ID, id []byte, expected uint64) []byte {
	return digest("fleet-subscription-delete-token-v2",
		[]byte(subscriberID), id, encodeUint64(expected))
}

// TestDispatchCannotSquatSubscriptionRevocation is #669. Before it, delete
// receipts were guarded at {'D', digest(slot row)}, and fleet dispatch
// guards an action at {'D', raw action ID}. So a principal holding dispatch
// could enqueue actions at every one of the 4096 receipt entity IDs, and
// every later subscription Delete conflicted on a head that belonged to
// dispatch. No subscriber could revoke.
//
// The attacker here occupies legacy receipt entities under dispatch's
// policy: the first victim's slot through the real dispatch service, and the
// second victim's slot plus squattedSlots others directly under
// fleet/dispatch/v1, as an enqueue would. Delete and DeleteWithAudit still revoke, and the squatted heads are
// untouched, still dispatch's.
func TestDispatchCannotSquatSubscriptionRevocation(t *testing.T) {
	now := time.Date(2026, 9, 6, 9, 0, 0, 0, time.UTC)
	config := runtimeConfig(t.TempDir())
	config = explorerfleet.ConfigureRuntime(config)
	ConfigureRuntime(&config)
	runtime, err := explorercoord.Open(config)
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Close()
	adapter, err := New(runtime, config.Domain)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	create := func(token string) fleetevents.Subscription {
		t.Helper()
		subscription, _, err := adapter.Create(ctx, fleetevents.CreateRequest{
			Token: []byte(token), SubscriberID: "victim", AgentID: "agent",
			AgentGeneration: 1, TTL: time.Hour, RetryUntil: now.Add(time.Hour),
		}, auth.Fingerprint{1}, 1, now)
		if err != nil {
			t.Fatal(err)
		}
		return subscription
	}
	first, second := create("first"), create("second")

	// The victim's own slot, through the real dispatch route.
	dispatch := squattingDispatch(t, runtime, adapter, now)
	victimRow := adapter.subscriptionReceiptRow(legacyDeleteReceiptPrefix,
		deletionTokenFor("victim", first.ID, first.Generation))
	victimEntity := adapter.legacyDeleteReceiptEntity(victimRow)
	if _, err := dispatch.Enqueue(dispatch.ctx, integrationEnqueueRequest(
		now, string(victimEntity.ID), "squat")); err != nil {
		t.Fatalf("the attacker's enqueue at the receipt entity ID: %v", err)
	}
	// The second deletion's slot, and a block of other legacy receipt
	// entities, under dispatch's policy. Not all 4096: every guard read
	// here snapshots the whole memtable, so occupying them all takes
	// minutes. Delete never builds a legacy entity at all, which is what
	// the mutation check on deleteReceiptKind shows, so the victims' own
	// slots are what matter.
	secondEntity := adapter.legacyDeleteReceiptEntity(adapter.subscriptionReceiptRow(
		legacyDeleteReceiptPrefix,
		deletionTokenFor("victim", second.ID, second.Generation)))
	occupied := []guard.Entity{secondEntity}
	for _, row := range legacyDeleteReceiptRows()[:squattedSlots] {
		entity := adapter.legacyDeleteReceiptEntity(row)
		if !bytes.Equal(entity.ID, victimEntity.ID) &&
			!bytes.Equal(entity.ID, secondEntity.ID) {
			occupied = append(occupied, entity)
		}
	}
	occupyUnder(t, runtime, config.Domain, occupied, "fleet/dispatch/v1")

	deleted, repeated, err := adapter.Delete(ctx, first.ID, "victim",
		first.Generation, now.Add(time.Hour), now)
	if err != nil || repeated || deleted.RevokedAt.IsZero() {
		t.Fatalf("revocation under a squatted receipt space = %+v, %v, %v",
			deleted, repeated, err)
	}
	if replayed, repeated, err := adapter.Delete(ctx, first.ID, "victim",
		first.Generation, now.Add(time.Hour), now.Add(time.Minute)); err != nil ||
		!repeated || replayed.Generation != deleted.Generation {
		t.Fatalf("the revocation's retry = %+v, %v, %v", replayed, repeated, err)
	}
	audit := fleetevents.LifecycleReceipt{RequestID: "revoke-second"}
	revoked, persisted, repeated, err := adapter.DeleteWithAudit(ctx, second.ID,
		"victim", second.Generation, now.Add(time.Hour), audit, now)
	if err != nil || repeated || revoked.RevokedAt.IsZero() ||
		persisted.RequestID != audit.RequestID {
		t.Fatalf("audited revocation under a squatted receipt space = %+v, %+v, %v, %v",
			revoked, persisted, repeated, err)
	}

	// The squatted heads are dispatch's still: nothing adopted them.
	head, _, err := runtime.ReadEntity(ctx, victimEntity)
	if err != nil || head == nil ||
		string(head.LogicalPolicyID) != "fleet/dispatch/v1" {
		t.Fatalf("the attacker's head after revocation = %+v, %v", head, err)
	}
	if action, err := dispatch.Status(dispatch.ctx, fleet.StatusRequest{
		ID: victimEntity.ID, Context: integrationRequestContext(now),
	}); err != nil || action.State != fleet.DispatchQueued {
		t.Fatalf("the attacker's action after revocation = %+v, %v", action, err)
	}
	// And the receipt the revocation wrote is under its own kind.
	newRow := adapter.subscriptionReceiptRow(deleteReceiptPrefix,
		deletionTokenFor("victim", first.ID, first.Generation))
	head, _, err = runtime.ReadEntity(ctx,
		adapter.subscriptionReceiptEntity(deleteReceiptKind, newRow))
	if err != nil || head == nil ||
		!bytes.Equal(head.LogicalPolicyID, logicalPolicy) {
		t.Fatalf("the revocation's receipt head = %+v, %v", head, err)
	}
}

// occupyUnder creates a head at each entity under another store's logical
// policy, as writeUnder does, in batches so 4096 of them stay quick.
func occupyUnder(
	t *testing.T, runtime *explorercoord.Runtime, domain coordination.DomainID,
	entities []guard.Entity, policy string,
) {
	t.Helper()
	lpart, err := explorercoord.Partition(domain, coordination.EntityID("occupier"))
	if err != nil {
		t.Fatal(err)
	}
	const batch = 512
	for start := 0; start < len(entities); start += batch {
		end := min(start+batch, len(entities))
		token := []byte(fmt.Sprintf("occupy-%s-%d", policy, start))
		intent := explorercoord.Intent{
			Operation: []byte("foreign-store-write"), Token: token,
			Results: []explorercoord.ResultIdentity{{
				Kind: []byte("foreign-store-write"), ID: token,
			}},
		}
		for _, entity := range entities[start:end] {
			row := append([]byte("zz-"+policy+"-"), entity.ID...)
			intent.Cells = append(intent.Cells, explorercoord.Cell{
				Table: Table, Row: row, Family: recordFamily,
				Qualifier: recordQualifier, Value: []byte("foreign"),
				EpochTimestamp: true, LPART: lpart, CopyGeneration: 1,
			})
			intent.Guards = append(intent.Guards, explorercoord.GuardIntent{
				Entity: entity, Mode: guard.ModeAbsentOrIdentical,
				DesiredState: guard.StateLive, DesiredWinnerID: []byte("foreign"),
				LPART: lpart, LogicalPolicyID: []byte(policy),
				RetirementGeneration: 1,
			})
		}
		if _, err := runtime.Publish(context.Background(),
			explorercoord.Request{Intent: intent}); err != nil {
			t.Fatalf("occupying entities %d to %d: %v", start, end, err)
		}
	}
	for _, entity := range entities {
		head, _, err := runtime.ReadEntity(context.Background(), entity)
		if err != nil || head == nil || string(head.LogicalPolicyID) != policy {
			t.Fatalf("occupied head %x = %+v, %v", entity.ID, head, err)
		}
	}
}

// boundDispatch is a dispatch service with a context bound to a principal
// holding dispatch, the attacker.
type boundDispatch struct {
	*fleet.DispatchService
	ctx context.Context
}

func squattingDispatch(
	t *testing.T, runtime *explorercoord.Runtime, events *Adapter, now time.Time,
) boundDispatch {
	t.Helper()
	authority, err := auth.NewAuthorityWithClock(func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	decision, err := auth.NewDecision(auth.DecisionConfig{
		Subject: "owner", Actor: "actor", ClientID: "client",
		AuthorizationDomain: []byte("domain"),
		AllowedOperations: []auth.Operation{
			auth.OperationDispatch, auth.OperationInvoke,
		},
		PermittedSourceIDs: [][]byte{[]byte("source")},
		PermittedPolicyIDs: [][]byte{[]byte("policy")},
		PolicyGeneration:   1, AuthenticationExpires: now.Add(2 * time.Hour),
		RequestID: "request", CorrelationID: "correlation",
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, err := authority.Binder().Bind(context.Background(), decision)
	if err != nil {
		t.Fatal(err)
	}
	registry, _ := newIntegrationRegistry(t, authority.Resolver(), now,
		&integrationExecutor{result: fleet.ExecutionResult{
			Output: json.RawMessage(`{"ok":true}`)}})
	capability := explorerfleetcap.New()
	eventService, err := fleetevents.NewWithLifecycleCapability(fleetevents.Config{
		Backend: events, Resolver: authority.Resolver(),
		GenerationReader: integrationGeneration{}, LeaseValidator: integrationLease{},
		Auditor: integrationAuditor{}, CursorKey: bytes.Repeat([]byte{7}, 32),
		Clock: func() time.Time { return now },
	}, capability)
	if err != nil {
		t.Fatal(err)
	}
	publisher, err := NewActionEventPublisher(
		eventService, authority.Resolver(), capability,
		func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	dispatch, err := explorerfleet.ComposeDispatch(
		runtime, registry, authority.Resolver(), integrationActionRecorder{},
		publisher, nil, func() time.Time { return now },
	)
	if err != nil {
		t.Fatal(err)
	}
	return boundDispatch{DispatchService: dispatch, ctx: ctx}
}

// legacyDelete commits a deletion exactly as a build before #669 did: the
// receipt at the legacy slot row, guarded at {'D', digest(row)}.
func legacyDelete(
	t *testing.T, a *Adapter, subscription fleetevents.Subscription,
	retryUntil, now time.Time, audit fleetevents.LifecycleReceipt,
) fleetevents.Subscription {
	t.Helper()
	ctx := context.Background()
	token := deletionTokenFor(subscription.SubscriberID, subscription.ID,
		subscription.Generation)
	requestDigest := digest("fleet-subscription-delete-request-v3",
		[]byte(subscription.SubscriberID), subscription.ID,
		encodeUint64(subscription.Generation), encodeTime(retryUntil))
	row := a.subscriptionReceiptRow(legacyDeleteReceiptPrefix, token)
	head, _, err := a.runtime.ReadEntity(ctx, a.subscriptionEntity(subscription.ID))
	if err != nil || head == nil {
		t.Fatalf("subscription head: %+v, %v", head, err)
	}
	revoked := subscription
	revoked.Generation++
	revoked.RevokedAt = now
	value, err := json.Marshal(subscriptionRecord{Subscription: revoked})
	if err != nil {
		t.Fatal(err)
	}
	receipt, err := json.Marshal(subscriptionMutationReceipt{
		MutationID: token, RequestDigest: requestDigest,
		RetryUntil: retryUntil, Subscription: revoked, Audit: audit,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.publishSubscriptionMutation(ctx,
		[]byte("fleet-subscription-delete-v2"),
		digest("fleet-subscription-delete-runtime-v2", token, encodeTime(retryUntil)),
		a.subscriptionRow(subscription.ID), value,
		a.subscriptionEntity(subscription.ID), guard.ModeMutate,
		head.Epoch, head.LogicalDigest, encodeUint64(revoked.Generation),
		row, receipt, a.legacyDeleteReceiptEntity(row),
		guard.ModeAbsentOrIdentical, 0, coordination.Digest{}, token,
	); err != nil {
		t.Fatalf("the legacy deletion: %v", err)
	}
	return revoked
}

// TestALegacyDeleteReceiptIsStillReplayed: a deletion an older build
// committed, with its receipt at the legacy location, is replayed by this
// build within its retry window under exactly the old rules. The same request
// replays, with the receipt's audit. A different request under the same token
// conflicts. Past the window the retry is expired. In none of these cases
// does this build write anything new.
func TestALegacyDeleteReceiptIsStillReplayed(t *testing.T) {
	now := time.Date(2026, 9, 6, 9, 0, 0, 0, time.UTC)
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
	ctx := context.Background()
	subscription, _, err := adapter.Create(ctx, fleetevents.CreateRequest{
		Token: []byte("legacy"), SubscriberID: "subscriber", AgentID: "agent",
		AgentGeneration: 1, TTL: 2 * time.Hour, RetryUntil: now.Add(time.Hour),
	}, auth.Fingerprint{1}, 1, now)
	if err != nil {
		t.Fatal(err)
	}
	retryUntil := now.Add(time.Hour)
	audit := fleetevents.LifecycleReceipt{RequestID: "legacy-revoke"}
	revoked := legacyDelete(t, adapter, subscription, retryUntil, now, audit)

	later := now.Add(time.Minute)
	replayed, persisted, repeated, err := adapter.DeleteWithAudit(ctx,
		subscription.ID, "subscriber", subscription.Generation, retryUntil,
		fleetevents.LifecycleReceipt{RequestID: "retry"}, later)
	if err != nil || !repeated || replayed.Generation != revoked.Generation ||
		persisted.RequestID != audit.RequestID {
		t.Fatalf("the legacy deletion's retry = %+v, %+v, %v, %v",
			replayed, persisted, repeated, err)
	}
	if _, _, err := adapter.Delete(ctx, subscription.ID, "subscriber",
		subscription.Generation, retryUntil.Add(time.Second), later); !shoal.IsErrorCode(
		err, shoal.ErrorConflict) {
		t.Fatalf("another request under the legacy deletion's token = %v", err)
	}
	expired := retryUntil.Add(time.Second)
	if _, _, err := adapter.Delete(ctx, subscription.ID, "subscriber",
		subscription.Generation, expired.Add(time.Hour), expired); !errors.Is(
		err, fleetevents.ErrMutationExpired) {
		t.Fatalf("the legacy deletion's retry past its window = %v", err)
	}
	// Nothing was written at the new location.
	row := adapter.subscriptionReceiptRow(deleteReceiptPrefix, deletionTokenFor(
		"subscriber", subscription.ID, subscription.Generation))
	if receipt, err := adapter.storedSubscriptionReceipt(ctx, row); err != nil ||
		receipt != nil {
		t.Fatalf("a replay wrote a new receipt: %+v, %v", receipt, err)
	}
}

// TestALegacyReceiptOfAnotherMutationBlocksNothing: under the old build, a
// live receipt of another deletion in the same slot refused a Delete with
// ErrRetentionCapacity. The legacy slots are never written again, so an
// unrelated legacy receipt is ignored.
func TestALegacyReceiptOfAnotherMutationBlocksNothing(t *testing.T) {
	now := time.Date(2026, 9, 6, 9, 0, 0, 0, time.UTC)
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
	ctx := context.Background()
	create := func(token string) fleetevents.Subscription {
		t.Helper()
		subscription, _, err := adapter.Create(ctx, fleetevents.CreateRequest{
			Token: []byte(token), SubscriberID: "subscriber", AgentID: "agent",
			AgentGeneration: 1, TTL: 2 * time.Hour, RetryUntil: now.Add(time.Hour),
		}, auth.Fingerprint{1}, 1, now)
		if err != nil {
			t.Fatal(err)
		}
		return subscription
	}
	first := create("first")
	slot := adapter.subscriptionReceiptRow(legacyDeleteReceiptPrefix,
		deletionTokenFor("subscriber", first.ID, first.Generation))
	var second fleetevents.Subscription
	for i := 0; ; i++ {
		if i == 1_000_000 {
			t.Fatal("no subscription shares the legacy receipt slot")
		}
		token := fmt.Sprintf("second-%d", i)
		id := digest("fleet-subscription-v1", []byte("subscriber"), []byte(token))
		if bytes.Equal(adapter.subscriptionRow(id), adapter.subscriptionRow(first.ID)) {
			continue
		}
		if bytes.Equal(slot, adapter.subscriptionReceiptRow(legacyDeleteReceiptPrefix,
			deletionTokenFor("subscriber", id, 1))) {
			second = create(token)
			break
		}
	}
	legacyDelete(t, adapter, first, now.Add(time.Hour), now,
		fleetevents.LifecycleReceipt{})
	if _, repeated, err := adapter.Delete(ctx, second.ID, "subscriber",
		second.Generation, now.Add(time.Hour), now); err != nil || repeated {
		t.Fatalf("a deletion sharing a live legacy receipt slot = %v, %v", repeated, err)
	}
}
