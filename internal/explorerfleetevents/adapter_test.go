/*
 * Licensed to the Apache Software Foundation (ASF) under one
 * or more contributor license agreements. See the NOTICE file
 * distributed with this work for additional information
 * regarding copyright ownership. The ASF licenses this file
 * to you under the Apache License, Version 2.0 (the
 * "License"); you may not use this file except in compliance
 * with the License. You may obtain a copy of the License at
 *
 *     https://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing,
 * software distributed under the License is distributed on an
 * "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY
 * KIND, either express or implied. See the License for the
 * specific language governing permissions and limitations
 * under the License.
 */

package explorerfleetevents

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/phrocker/shoal-oss/internal/explorercoord"
	"github.com/phrocker/shoal-oss/pkg/document"
	"github.com/phrocker/shoal-oss/pkg/explorer/auth"
	"github.com/phrocker/shoal-oss/pkg/explorer/coordination"
	"github.com/phrocker/shoal-oss/pkg/explorer/coordination/guard"
	"github.com/phrocker/shoal-oss/pkg/explorer/coordination/transaction"
	"github.com/phrocker/shoal-oss/pkg/explorer/fleetevents"
	"github.com/phrocker/shoal-oss/pkg/interaction"
	"github.com/phrocker/shoal-oss/pkg/ontology"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

func TestAdapterConcurrentAppendRestartResume(t *testing.T) {
	directory := t.TempDir()
	config := runtimeConfig(directory)
	runtime, err := explorercoord.Open(config)
	if err != nil {
		t.Fatal(err)
	}

	closed := false
	defer func() {
		if !closed {
			_ = runtime.Close()
		}
	}()
	adapter, err := New(runtime, config.Domain)
	if err != nil {
		t.Fatal(err)
	}
	const count = 24
	appendNow := time.Now().UTC()
	retryUntil := appendNow.Add(time.Hour)
	var wg sync.WaitGroup
	errs := make(chan error, count)
	for i := 0; i < count; i++ {
		wg.Add(1)
		go func(index int) {
			defer wg.Done()
			_, appendErr := adapter.Append(context.Background(), fleetevents.PublishRequest{
				Token:      []byte(fmt.Sprintf("token-%02d", index)),
				RetryUntil: retryUntil, Event: testEvent(index),
			}, appendNow)
			errs <- appendErr
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	stream, _, err := runtime.ReadEntity(context.Background(), streamEntity)
	if err != nil || stream == nil || binary.BigEndian.Uint64(stream.WinnerID) != count {
		t.Fatalf("stream head = %#v, %v", stream, err)
	}
	repeated, err := adapter.Append(context.Background(), fleetevents.PublishRequest{
		Token: []byte("token-00"), RetryUntil: retryUntil, Event: testEvent(0),
	}, appendNow)
	if err != nil || !repeated.Repeated {
		t.Fatalf("repeated append = %#v, %v", repeated, err)
	}
	divergent := testEvent(0)
	divergent.Kind = "different"
	if _, err := adapter.Append(context.Background(), fleetevents.PublishRequest{
		Token: []byte("token-00"), RetryUntil: retryUntil, Event: divergent,
	}, appendNow); !errors.Is(err, transaction.ErrConflict) ||
		!shoal.IsErrorCode(err, shoal.ErrorConflict) {
		t.Fatalf("divergent retry error = %v, want conflict", err)
	}
	for index := 0; index < count; index++ {
		head, _, readErr := runtime.ReadEntity(
			context.Background(), adapter.eventSlotEntity(uint64(index+1)))
		if readErr != nil || head == nil {
			t.Fatalf("event slot %d = %#v, %v", index, head, readErr)
		}
	}
	events, frontier, err := adapter.Scan(context.Background(), 1, 0, count)
	if err != nil || len(events) != count || frontier == 0 {
		t.Fatalf("committed scan = %d events, frontier %d, %v", len(events), frontier, err)
	}
	for index := range events {
		if events[index].Sequence != uint64(index+1) {
			t.Fatalf("event sequence %d = %d", index, events[index].Sequence)
		}
	}
	if err := runtime.Close(); err != nil {
		t.Fatal(err)
	}
	closed = true
	runtime, err = explorercoord.Open(config)
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Close()
	stream, _, err = runtime.ReadEntity(context.Background(), streamEntity)
	if err != nil || stream == nil || binary.BigEndian.Uint64(stream.WinnerID) != count {
		t.Fatalf("reopened stream head = %#v, %v", stream, err)
	}
	adapter, err = New(runtime, config.Domain)
	if err != nil {
		t.Fatal(err)
	}
	resumed, resumedFrontier, err := adapter.Scan(context.Background(), 13, 0, count)
	if err != nil || len(resumed) != 12 || resumed[0].Sequence != 13 || resumedFrontier == 0 {
		t.Fatalf("resumed scan = %#v, frontier %d, %v", resumed, resumedFrontier, err)
	}
}

func TestAdapterOpaqueIDsRemainByteSafeAcrossRestart(t *testing.T) {
	ctx := context.Background()
	config := runtimeConfig(t.TempDir())
	runtime, err := explorercoord.Open(config)
	if err != nil {
		t.Fatal(err)
	}
	adapter, err := New(runtime, config.Domain)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 9, 6, 22, 0, 0, 0, time.UTC)
	request := fleetevents.CreateRequest{
		Token: []byte("opaque-create"), SubscriberID: shoal.ID(string([]byte{0xff})),
		AgentID: shoal.ID(string([]byte{'a', 0xfe})), AgentGeneration: 1,
		TTL: time.Hour, RetryUntil: now.Add(time.Hour),
	}
	fingerprint := auth.Fingerprint{1}
	created, repeated, err := adapter.Create(ctx, request, fingerprint, 1, now)
	if err != nil || repeated {
		t.Fatalf("create = %#v, repeated %v, %v", created, repeated, err)
	}
	divergent := request
	divergent.SubscriberID = shoal.ID(string([]byte{0xfe}))
	if _, _, err := adapter.Create(
		ctx, divergent, fingerprint, 1, now,
	); !errors.Is(err, transaction.ErrConflict) ||
		!shoal.IsErrorCode(err, shoal.ErrorConflict) {
		t.Fatalf("byte-distinct create retry error = %v, want conflict", err)
	}

	event := testEvent(0)
	event.ProducerID = []byte{0xff, 0x00}
	event.Evidence[0].ObjectID = shoal.ID(string([]byte{0xff}))
	event.ConsumedEvidence = []interaction.EvidenceReference{{
		AnchorID: shoal.ID(string([]byte{0xfc})),
		Kind:     interaction.EvidenceGraph,
		NodeIDs: []shoal.ID{
			shoal.ID(string([]byte{0xfe})),
			shoal.ID(string([]byte{0xfb})),
		},
		EdgeIDs: []shoal.ID{shoal.ID(string([]byte{0xfd}))},
		Assertions: []interaction.AssertionReference{{
			AssertionID: shoal.ID(string([]byte{0xfa})),
			EdgeID:      shoal.ID(string([]byte{0xfd})),
			Origin:      ontology.AssertionDerived,
		}},
	}}
	event.CitedEvidence = []interaction.EvidenceReference{{
		AnchorID: shoal.ID(string([]byte{0xf9})),
		Kind:     interaction.EvidenceDocument,
		Citation: document.Citation{
			DocumentID: shoal.ID(string([]byte{0xf8})),
			RevisionID: shoal.ID(string([]byte{0xf7})),
			SectionID:  shoal.ID(string([]byte{0xf6})),
			SpanID:     shoal.ID(string([]byte{0xf5})),
			Range: document.SourceRange{
				Start: document.SourcePosition{Offset: 3, Page: 1},
				End:   document.SourcePosition{Offset: 9, Page: 2},
			},
		},
		NodeIDs: []shoal.ID{
			shoal.ID(string([]byte{0xf8})),
			shoal.ID(string([]byte{0xf6})),
			shoal.ID(string([]byte{0xf5})),
		},
	}}
	reference := event.ConsumedEvidence[0]
	event.Evidence[0].Reference = &reference
	audit := fleetevents.LifecycleReceipt{
		RequestID:                shoal.ID(string([]byte{0xff, 0x00, 'r'})),
		CorrelationID:            []byte{0xfe, 0x00, 'c'},
		AuthorizationFingerprint: auth.Fingerprint{9},
		AuthorizationExpiresAt:   now.Add(time.Hour),
	}
	if _, err := adapter.Append(ctx, fleetevents.PublishRequest{
		Token: []byte("opaque-event"), RetryUntil: now.Add(time.Hour),
		Event: event, Audit: audit,
	}, now); err != nil {
		t.Fatal(err)
	}
	if err := runtime.Close(); err != nil {
		t.Fatal(err)
	}
	runtime, err = explorercoord.Open(config)
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Close()
	adapter, err = New(runtime, config.Domain)
	if err != nil {
		t.Fatal(err)
	}
	reloaded, err := adapter.Subscription(ctx, created.ID)
	if err != nil || reloaded.SubscriberID != request.SubscriberID ||
		reloaded.AgentID != request.AgentID {
		t.Fatalf("reloaded subscription = %#v, %v", reloaded, err)
	}
	events, _, err := adapter.Scan(ctx, 1, 0, 1)
	if err != nil || len(events) != 1 ||
		!reflect.DeepEqual(events[0], fleetevents.Event{
			Sequence: 1, EventID: events[0].EventID, Kind: event.Kind,
			ProducerID: event.ProducerID, ProducerGeneration: event.ProducerGeneration,
			ActionID: event.ActionID, TransitionID: event.TransitionID,
			CorrelationID: event.CorrelationID, Reason: event.Reason,
			Evidence: event.Evidence, ConsumedEvidence: event.ConsumedEvidence,
			CitedEvidence: event.CitedEvidence,
			OccurredAt:    event.OccurredAt,
		}) {
		t.Fatalf("reloaded event = %#v, %v", events, err)
	}
	repeatedEvent, err := adapter.Append(ctx, fleetevents.PublishRequest{
		Token: []byte("opaque-event"), RetryUntil: now.Add(time.Hour),
		Event: event, Audit: fleetevents.LifecycleReceipt{
			RequestID: "fresh-attempt",
		},
	}, now)
	if err != nil || !repeatedEvent.Repeated ||
		!reflect.DeepEqual(repeatedEvent.Audit, audit) {
		t.Fatalf("reloaded audit receipt = %#v, %v", repeatedEvent, err)
	}
}

func TestDigestLengthFramesOpaqueParts(t *testing.T) {
	left := digest("opaque", []byte{'a', 0, 'b'}, []byte("c"))
	right := digest("opaque", []byte("a"), []byte{'b', 0, 'c'})
	if bytes.Equal(left, right) {
		t.Fatal("length-distinct opaque identity parts collided")
	}
}

func TestAdapterRetentionFloorAndIdempotencyExpirySurviveRestart(t *testing.T) {
	ctx := context.Background()
	config := runtimeConfig(t.TempDir())
	runtime, err := explorercoord.Open(config)
	if err != nil {
		t.Fatal(err)
	}
	adapter, err := NewWithRetention(runtime, config.Domain, 3)
	if err != nil {
		t.Fatal(err)
	}
	start := time.Date(2026, 9, 6, 6, 0, 0, 0, time.UTC)
	var firstEventID []byte
	var historicalFrontier coordination.Epoch
	var historyFloor coordination.Epoch
	for index := 0; index < 5; index++ {
		event := testEvent(index)
		event.OccurredAt = start.Add(time.Duration(index) * time.Minute)
		if index == 0 {
			event.OccurredAt = start.Add(365 * 24 * time.Hour)
		}
		result, err := adapter.Append(ctx, fleetevents.PublishRequest{
			Token:      []byte(fmt.Sprintf("retained-%d", index)),
			RetryUntil: start.Add(time.Duration(index+2) * time.Minute), Event: event,
		}, start.Add(time.Duration(index)*time.Minute))
		if err != nil {
			t.Fatal(err)
		}
		if index == 0 {
			firstEventID = append([]byte(nil), result.EventID...)
		}
		if index == 2 {
			page, scanErr := runtime.ScanCommitted(
				ctx, explorercoord.CommittedScanRequest{
					Table: Table, RowPrefix: eventPrefix,
					Family: recordFamily, Qualifier: recordQualifier, Limit: 10,
				})
			if scanErr != nil || len(page.Cells) != 3 {
				t.Fatalf("pre-prune event rows = %d, %v", len(page.Cells), scanErr)
			}
			historicalFrontier, historyFloor = page.Frontier, page.HistoryFloor
		}
	}
	if _, _, err := adapter.Scan(ctx, 1, 0, 3); !errors.Is(
		err, fleetevents.ErrResyncRequired,
	) {
		t.Fatalf("below-floor scan = %v", err)
	}
	events, _, err := adapter.Scan(ctx, 3, 0, 3)
	if err != nil || len(events) != 3 ||
		events[0].Sequence != 3 || events[2].Sequence != 5 {
		t.Fatalf("retained events = %#v, %v", events, err)
	}
	page, err := runtime.ScanCommitted(ctx, explorercoord.CommittedScanRequest{
		Table: Table, RowPrefix: eventPrefix, Family: recordFamily,
		Qualifier: recordQualifier, Limit: 10,
	})
	if err != nil || len(page.Cells) != 3 {
		t.Fatalf("bounded event rows = %d, %v", len(page.Cells), err)
	}
	if page.HistoryFloor != historyFloor {
		t.Fatalf(
			"prune changed shared history floor from %d to %d",
			historyFloor, page.HistoryFloor,
		)
	}
	historical, err := runtime.ScanCommitted(ctx, explorercoord.CommittedScanRequest{
		Table: Table, RowPrefix: eventPrefix, Family: recordFamily,
		Qualifier: recordQualifier, Frontier: historicalFrontier, Limit: 10,
	})
	if err != nil {
		t.Fatal(err)
	}
	historicalEvents, err := decodeSortedEvents(historical.Cells, 1, 10)
	if err != nil || len(historicalEvents) != 3 ||
		historicalEvents[0].Sequence != 1 ||
		historicalEvents[2].Sequence != 3 {
		t.Fatalf("historical event rows = %#v, %v", historicalEvents, err)
	}
	retired, _, err := runtime.ReadEntity(ctx, adapter.eventSlotEntity(1))
	if err != nil || retired == nil || retired.State != guard.StateTombstone {
		t.Fatalf("retired event guard = %#v, %v", retired, err)
	}
	replacement, _, err := runtime.ReadEntity(ctx, adapter.eventSlotEntity(4))
	if err != nil || replacement == nil || replacement.State != guard.StateLive {
		t.Fatalf("replacement event guard = %#v, %v", replacement, err)
	}
	if _, _, found, err := adapter.readPublicationEvent(
		ctx, firstEventID); err != nil || found {
		t.Fatalf("retired publication index found = %v, %v", found, err)
	}
	if err := runtime.Close(); err != nil {
		t.Fatal(err)
	}
	runtime, err = explorercoord.Open(config)
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Close()
	adapter, err = NewWithRetention(runtime, config.Domain, 3)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := adapter.Scan(ctx, 1, 0, 3); !errors.Is(
		err, fleetevents.ErrResyncRequired,
	) {
		t.Fatalf("restart below-floor scan = %v", err)
	}
	old := testEvent(0)
	old.OccurredAt = start.Add(365 * 24 * time.Hour)
	if _, err := adapter.Append(ctx, fleetevents.PublishRequest{
		Token: []byte("retained-0"), RetryUntil: start.Add(2 * time.Minute), Event: old,
	}, start.Add(10*time.Minute)); !errors.Is(
		err, fleetevents.ErrPublicationExpired,
	) {
		t.Fatalf("expired idempotency retry = %v", err)
	}
	changedExpired := old
	changedExpired.OccurredAt = start.Add(366 * 24 * time.Hour)
	changedExpired.Kind = "agent.failed"
	if _, err := adapter.Append(ctx, fleetevents.PublishRequest{
		Token: []byte("retained-0"), RetryUntil: start.Add(2 * time.Minute),
		Event: changedExpired,
	}, start.Add(10*time.Minute)); !errors.Is(err, fleetevents.ErrPublicationExpired) {
		t.Fatalf("changed expired publication retry = %v", err)
	}
	reusedWindow := fleetevents.PublishRequest{
		Token: []byte("retained-0"), RetryUntil: start.Add(20 * time.Minute),
		Event: changedExpired,
	}
	reused, err := adapter.Append(ctx, reusedWindow, start.Add(10*time.Minute))
	if err != nil {
		t.Fatalf("new publication window = %v", err)
	}
	if bytes.Equal(reused.EventID, firstEventID) {
		t.Fatal("expired publication identity resurrected the retired event ID")
	}
	replayed, err := adapter.Append(ctx, reusedWindow, start.Add(11*time.Minute))
	if err != nil || !replayed.Repeated || !bytes.Equal(replayed.EventID, reused.EventID) {
		t.Fatalf("new-window replay = %#v, %v", replayed, err)
	}
	firstLate := testEvent(99)
	firstLate.OccurredAt = start
	if _, err := adapter.Append(ctx, fleetevents.PublishRequest{
		Token:      []byte("first-late-publication"),
		RetryUntil: start.Add(20 * time.Minute), Event: firstLate,
	}, start.Add(10*time.Minute)); err != nil {
		t.Fatalf("first late publication = %v", err)
	}
}

func TestAdapterActivePublicationIdentitySurvivesSlotRotation(t *testing.T) {
	ctx := context.Background()
	config := runtimeConfig(t.TempDir())
	runtime, err := explorercoord.Open(config)
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Close()
	adapter, err := NewWithRetention(runtime, config.Domain, 2)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 9, 7, 12, 0, 0, 0, time.UTC)
	first := fleetevents.PublishRequest{
		Token: []byte("stable-publication"), RetryUntil: now.Add(time.Hour),
		Event: testEvent(1),
	}
	first.Event.OccurredAt = now
	published, err := adapter.Append(ctx, first, now)
	if err != nil {
		t.Fatal(err)
	}
	for index := 0; index < 20; index++ {
		event := testEvent(index + 10)
		event.OccurredAt = now.Add(time.Duration(index+1) * time.Second)
		if _, err := adapter.Append(ctx, fleetevents.PublishRequest{
			Token:      []byte(fmt.Sprintf("rotation-%d", index)),
			RetryUntil: now.Add(time.Hour), Event: event,
		}, now.Add(time.Duration(index+1)*time.Second)); err != nil {
			t.Fatalf("rotation %d: %v", index, err)
		}
	}
	replayed, err := adapter.Append(ctx, first, now.Add(30*time.Second))
	if err != nil || !replayed.Repeated ||
		!bytes.Equal(replayed.EventID, published.EventID) {
		t.Fatalf("rotated exact retry = %#v, %v", replayed, err)
	}
	changedDeadline := first
	changedDeadline.RetryUntil = first.RetryUntil.Add(time.Minute)
	if _, err := adapter.Append(
		ctx, changedDeadline, now.Add(30*time.Second),
	); !errors.Is(err, transaction.ErrConflict) {
		t.Fatalf("changed active deadline = %v", err)
	}
	changedContent := first
	changedContent.Event.Kind = "agent.failed"
	if _, err := adapter.Append(
		ctx, changedContent, now.Add(30*time.Second),
	); !errors.Is(err, transaction.ErrConflict) {
		t.Fatalf("changed active content = %v", err)
	}
	later := now.Add(2 * time.Hour)
	expirySweep := testEvent(100)
	expirySweep.OccurredAt = later
	if _, err := adapter.Append(ctx, fleetevents.PublishRequest{
		Token: []byte("after-expiry-sweep"), RetryUntil: later.Add(time.Hour),
		Event: expirySweep,
	}, later); err != nil {
		t.Fatalf("post-expiry publication = %v", err)
	}
	receipts, err := runtime.ScanCommitted(
		ctx, explorercoord.CommittedScanRequest{
			Table: Table, RowPrefix: publicationPrefix,
			Family: recordFamily, Qualifier: recordQualifier, Limit: 100,
		})
	if err != nil || len(receipts.Cells) != 1 {
		t.Fatalf("retained publication receipts = %d, %v", len(receipts.Cells), err)
	}
}

func TestAdapterRejectsOversizedRetryWindows(t *testing.T) {
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
	now := time.Date(2026, 9, 6, 6, 0, 0, 0, time.UTC)
	if _, err := adapter.Append(ctx, fleetevents.PublishRequest{
		Token: []byte("publish"),
		RetryUntil: now.Add(
			fleetevents.MaxMutationRetryWindow + time.Nanosecond),
		Event: testEvent(0),
	}, now); err == nil {
		t.Fatal("adapter accepted oversized publication retry window")
	}
	if _, _, err := adapter.Create(ctx, fleetevents.CreateRequest{
		Token: []byte("create"), SubscriberID: "subscriber",
		AgentID: "agent", AgentGeneration: 1, TTL: time.Hour,
		RetryUntil: now.Add(
			fleetevents.MaxMutationRetryWindow + time.Nanosecond),
	}, auth.Fingerprint{1}, 1, now); err == nil {
		t.Fatal("adapter accepted oversized mutation retry window")
	}
}

func TestAdapterReusesExpiredSubscriptionSlot(t *testing.T) {
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
	firstToken, secondToken := collidingSubscriptionTokens(t, adapter)
	now := time.Date(2026, 9, 6, 6, 0, 0, 0, time.UTC)
	first, _, err := adapter.Create(ctx, fleetevents.CreateRequest{
		Token: firstToken, SubscriberID: "subscriber",
		AgentID: "agent", AgentGeneration: 1, TTL: time.Minute,
		RetryUntil: now.Add(time.Minute),
	}, auth.Fingerprint{1}, 1, now)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := adapter.Create(ctx, fleetevents.CreateRequest{
		Token: secondToken, SubscriberID: "subscriber",
		AgentID: "agent", AgentGeneration: 1, TTL: time.Minute,
		RetryUntil: now.Add(time.Minute),
	}, auth.Fingerprint{1}, 1, now); !errors.Is(err, transaction.ErrConflict) {
		t.Fatalf("active subscription slot collision = %v", err)
	}
	second, _, err := adapter.Create(ctx, fleetevents.CreateRequest{
		Token: secondToken, SubscriberID: "subscriber",
		AgentID: "agent", AgentGeneration: 1, TTL: time.Minute,
		RetryUntil: now.Add(3 * time.Minute),
	}, auth.Fingerprint{1}, 1, now.Add(2*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(first.ID, second.ID) {
		t.Fatal("collision fixture reused the same subscription identity")
	}
	if _, err := adapter.Subscription(ctx, first.ID); !errors.Is(
		err, fleetevents.ErrSubscriptionNotFound,
	) {
		t.Fatalf("retired subscription lookup = %v", err)
	}
	if loaded, err := adapter.Subscription(ctx, second.ID); err != nil ||
		!bytes.Equal(loaded.ID, second.ID) {
		t.Fatalf("replacement subscription = %#v, %v", loaded, err)
	}
	if _, _, err := adapter.Create(ctx, fleetevents.CreateRequest{
		Token: firstToken, SubscriberID: "subscriber",
		AgentID: "agent", AgentGeneration: 1, TTL: time.Minute,
		RetryUntil: now.Add(time.Minute),
	}, auth.Fingerprint{1}, 1, now.Add(2*time.Minute)); !errors.Is(
		err, fleetevents.ErrMutationExpired,
	) {
		t.Fatalf("expired create retry = %v", err)
	}
}

func collidingSubscriptionTokens(t *testing.T, adapter *Adapter) ([]byte, []byte) {
	t.Helper()
	slots := make(map[string][]byte)
	for index := 0; index < 100_000; index++ {
		token := []byte(fmt.Sprintf("collision-%d", index))
		id := digest("fleet-subscription-v1", []byte("subscriber"), token)
		slot := string(adapter.subscriptionRow(id))
		if previous, ok := slots[slot]; ok {
			return previous, token
		}
		slots[slot] = token
	}
	t.Fatal("failed to find bounded subscription slot collision")
	return nil, nil
}

func TestAdapterSubscriptionGenerationCAS(t *testing.T) {
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
	now := time.Date(2026, 9, 5, 20, 0, 0, 0, time.UTC)
	subscription, _, err := adapter.Create(context.Background(), fleetevents.CreateRequest{
		Token: []byte("create-token"), SubscriberID: "subscriber",
		AgentID: shoal.ID("agent"), AgentGeneration: 1, TTL: time.Hour,
		RetryUntil: now.Add(30 * time.Minute),
	}, auth.Fingerprint{1}, 1, now)
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := adapter.Subscription(context.Background(), subscription.ID)
	if err != nil || loaded.Generation != 1 {
		t.Fatalf("loaded subscription = %#v, %v", loaded, err)
	}
	deleteRetryUntil := now.Add(30 * time.Minute)
	deleted, _, err := adapter.Delete(
		context.Background(), subscription.ID, "subscriber", 1,
		deleteRetryUntil, now.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if deleted.Generation != 2 || deleted.RevokedAt.IsZero() {
		t.Fatalf("deleted subscription = %#v", deleted)
	}
	replayed, _, err := adapter.Delete(
		context.Background(), subscription.ID, "subscriber", 1,
		deleteRetryUntil, now.Add(2*time.Minute))
	if err != nil || !replayed.RevokedAt.Equal(deleted.RevokedAt) {
		t.Fatalf("exact deletion replay = %#v, %v", replayed, err)
	}
	if _, _, err := adapter.Delete(
		context.Background(), subscription.ID, "subscriber", 2,
		deleteRetryUntil, now.Add(2*time.Minute),
	); !errors.Is(err, fleetevents.ErrGenerationConflict) {
		t.Fatalf("different deletion replay = %v", err)
	}
}

func TestSubscriptionMutationAuditRetryAcrossRestartAndClockAdvance(t *testing.T) {
	ctx := context.Background()
	createdAt := time.Date(2026, 9, 6, 5, 0, 0, 0, time.UTC)
	current := createdAt
	config := runtimeConfig(t.TempDir())
	runtime, err := explorercoord.Open(config)
	if err != nil {
		t.Fatal(err)
	}
	backend, err := New(runtime, config.Domain)
	if err != nil {
		t.Fatal(err)
	}
	audit := &retryAuditor{err: errors.New("audit unavailable")}
	authorizationExpires := createdAt.Add(24 * time.Hour)
	service := durableRetryService(
		t, backend, func() time.Time { return current }, authorizationExpires, audit)
	request := fleetevents.CreateRequest{
		Token: []byte("durable-create"), AgentID: "agent", AgentGeneration: 1,
		TTL: time.Hour, RetryUntil: createdAt.Add(2 * time.Hour),
	}
	if _, err := service.Create(ctx, request); !errors.Is(
		err, fleetevents.ErrAuditOutcomeUnknown,
	) {
		t.Fatalf("initial create = %v", err)
	}
	if err := runtime.Close(); err != nil {
		t.Fatal(err)
	}
	current = current.Add(10 * time.Minute)
	runtime, err = explorercoord.Open(config)
	if err != nil {
		t.Fatal(err)
	}
	backend, err = New(runtime, config.Domain)
	if err != nil {
		t.Fatal(err)
	}
	audit.err = nil
	service = durableRetryService(
		t, backend, func() time.Time { return current }, authorizationExpires, audit)
	subscription, err := service.Create(ctx, request)
	if err != nil {
		t.Fatalf("create receipt repair = %v", err)
	}
	if !subscription.CreatedAt.Equal(createdAt) ||
		!subscription.ExpiresAt.Equal(createdAt.Add(time.Hour)) {
		t.Fatalf("create replay changed durable envelope: %#v", subscription)
	}
	audit.err = errors.New("audit unavailable")
	if err := service.Delete(ctx, fleetevents.DeleteRequest{
		SubscriptionID: subscription.ID, ExpectedGeneration: 1,
		RetryUntil: createdAt.Add(2 * time.Hour),
	}); !errors.Is(err, fleetevents.ErrAuditOutcomeUnknown) {
		t.Fatalf("initial delete = %v", err)
	}
	deletedAt := current
	audit.err = nil
	recreated, err := service.Create(ctx, request)
	if err != nil {
		t.Fatalf("create receipt after deletion = %v", err)
	}
	if recreated.Generation != 1 ||
		!recreated.CreatedAt.Equal(createdAt) ||
		!recreated.ExpiresAt.Equal(createdAt.Add(time.Hour)) {
		t.Fatalf("create receipt was replaced by mutable slot: %#v", recreated)
	}
	if err := runtime.Close(); err != nil {
		t.Fatal(err)
	}
	current = current.Add(10 * time.Minute)
	runtime, err = explorercoord.Open(config)
	if err != nil {
		t.Fatal(err)
	}
	backend, err = New(runtime, config.Domain)
	if err != nil {
		t.Fatal(err)
	}
	targetRow := backend.subscriptionRow(subscription.ID)
	var replacement fleetevents.Subscription
	for index := 0; index < 1_000_000; index++ {
		token := []byte(fmt.Sprintf("colliding-live-slot-%d", index))
		candidateID := digest(
			"fleet-subscription-v1", []byte("alice"), token)
		if bytes.Equal(candidateID, subscription.ID) ||
			!bytes.Equal(backend.subscriptionRow(candidateID), targetRow) {
			continue
		}
		replacement, _, err = backend.Create(ctx, fleetevents.CreateRequest{
			Token: token, SubscriberID: "alice", AgentID: "agent",
			AgentGeneration: 1, TTL: time.Hour,
			RetryUntil: createdAt.Add(2 * time.Hour),
		}, subscription.AuthorizationFingerprint, subscription.PolicyGeneration, current)
		if errors.Is(err, fleetevents.ErrRetentionCapacity) {
			continue
		}
		if err != nil {
			t.Fatalf("colliding slot create = %v", err)
		}
		break
	}
	if len(replacement.ID) == 0 {
		t.Fatal("could not find a replacement for the revoked live slot")
	}
	if err := runtime.Close(); err != nil {
		t.Fatal(err)
	}
	runtime, err = explorercoord.Open(config)
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Close()
	backend, err = New(runtime, config.Domain)
	if err != nil {
		t.Fatal(err)
	}
	audit.err = nil
	service = durableRetryService(
		t, backend, func() time.Time { return current }, authorizationExpires, audit)
	if err := service.Delete(ctx, fleetevents.DeleteRequest{
		SubscriptionID: subscription.ID, ExpectedGeneration: 1,
		RetryUntil: createdAt.Add(2 * time.Hour),
	}); err != nil {
		t.Fatalf("delete receipt repair = %v", err)
	}
	stored, err := backend.Subscription(ctx, replacement.ID)
	if err != nil || !stored.RevokedAt.IsZero() || stored.Generation != 1 {
		t.Fatalf("delete receipt repair mutated replacement: %#v, %v", stored, err)
	}
	if len(audit.records) == 0 ||
		!audit.records[len(audit.records)-1].OccurredAt.Equal(deletedAt) {
		t.Fatalf("delete audit did not preserve original commit time: %#v", audit.records)
	}
}
func TestServiceCursorResumesAcrossRuntimeRestart(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 9, 6, 4, 0, 0, 0, time.UTC)
	config := runtimeConfig(t.TempDir())
	runtime, err := explorercoord.Open(config)
	if err != nil {
		t.Fatal(err)
	}
	adapter, err := New(runtime, config.Domain)
	if err != nil {
		t.Fatal(err)
	}
	service := restartService(t, adapter, now)
	subscription, err := service.Create(ctx, fleetevents.CreateRequest{
		Token: []byte("create"), AgentID: "agent", AgentGeneration: 1,
		TTL: time.Hour, RetryUntil: now.Add(time.Hour),
	})
	if err != nil {
		t.Fatal(err)
	}
	for index := 0; index < 2; index++ {
		if _, err := service.Publish(ctx, fleetevents.PublishRequest{
			Token:      []byte(fmt.Sprintf("publish-%d", index)),
			RetryUntil: now.Add(time.Hour), Event: testEvent(index),
		}); err != nil {
			t.Fatal(err)
		}
	}
	first, err := service.Pull(ctx, fleetevents.PullRequest{
		SubscriptionID: subscription.ID, Limit: 1,
	})
	if err != nil || len(first.Events) != 1 || first.NextCursor == "" {
		t.Fatalf("first page = %#v, %v", first, err)
	}
	if err := runtime.Close(); err != nil {
		t.Fatal(err)
	}
	runtime, err = explorercoord.Open(config)
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Close()
	adapter, err = New(runtime, config.Domain)
	if err != nil {
		t.Fatal(err)
	}
	service = restartService(t, adapter, now)
	second, err := service.Pull(ctx, fleetevents.PullRequest{
		SubscriptionID: subscription.ID, Cursor: first.NextCursor, Limit: 1,
	})
	if err != nil || len(second.Events) != 1 ||
		bytes.Equal(second.Events[0].ActionID, first.Events[0].ActionID) {
		t.Fatalf("resumed page = %#v, %v", second, err)
	}
}

func restartService(
	t *testing.T, backend fleetevents.Backend, now time.Time,
) *fleetevents.Service {
	t.Helper()
	decision, err := auth.NewDecision(auth.DecisionConfig{
		Subject: "subscriber", Actor: "subscriber", RequestID: "request",
		AuthorizationDomain: []byte("domain"),
		AllowedOperations: []auth.Operation{
			auth.OperationSubscriptionCreate, auth.OperationEventPublish,
		},
		PermittedSourceIDs: [][]byte{[]byte("source")},
		PermittedPolicyIDs: [][]byte{[]byte("policy")},
		PolicyGeneration:   1, AuthenticationExpires: now.Add(time.Hour),
	})
	if err != nil {
		t.Fatal(err)
	}

	resolver, err := auth.NewStaticResolverWithClock(
		decision, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	service, err := fleetevents.New(fleetevents.Config{
		Backend: backend, Resolver: resolver,
		GenerationReader: restartGenerationReader{},
		LeaseValidator:   restartLeaseValidator{},
		Auditor:          restartAuditor{},
		CursorKey:        bytes.Repeat([]byte{0x42}, 32),
		Clock:            func() time.Time { return now },
		PollInterval:     time.Millisecond,
		MaxWait:          time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	return service
}

func durableRetryService(
	t *testing.T, backend fleetevents.Backend, clock func() time.Time,
	authorizationExpires time.Time, audit fleetevents.Auditor,
) *fleetevents.Service {
	t.Helper()
	decision, err := auth.NewDecision(auth.DecisionConfig{
		Subject: "subscriber", Actor: "subscriber", RequestID: "request",
		CorrelationID: "correlation", AuthorizationDomain: []byte("domain"),
		AllowedOperations: []auth.Operation{
			auth.OperationSubscriptionCreate, auth.OperationSubscriptionDelete,
		},
		PermittedSourceIDs: [][]byte{[]byte("source")},
		PermittedPolicyIDs: [][]byte{[]byte("policy")},
		PolicyGeneration:   1, AuthenticationExpires: authorizationExpires,
	})
	if err != nil {
		t.Fatal(err)
	}
	resolver, err := auth.NewStaticResolverWithClock(decision, clock)
	if err != nil {
		t.Fatal(err)
	}
	service, err := fleetevents.New(fleetevents.Config{
		Backend: backend, Resolver: resolver,
		GenerationReader: restartGenerationReader{},
		LeaseValidator:   restartLeaseValidator{}, Auditor: audit,
		CursorKey: bytes.Repeat([]byte{0x42}, 32), Clock: clock,
		PollInterval: time.Millisecond, MaxWait: time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	return service
}

type retryAuditor struct {
	err     error
	records []fleetevents.AuditRecord
}

func (a *retryAuditor) RecordFleetAction(
	_ context.Context, record fleetevents.AuditRecord,
) error {
	a.records = append(a.records, record)
	return a.err
}

type restartGenerationReader struct{}

func (restartGenerationReader) CurrentPolicyGeneration(
	context.Context, []byte,
) (int64, error) {
	return 1, nil
}

type restartLeaseValidator struct{}

func (restartLeaseValidator) ValidateDelivery(
	context.Context, shoal.ID, int64,
) error {
	return nil
}

type restartAuditor struct{}

func (restartAuditor) RecordFleetAction(
	context.Context, fleetevents.AuditRecord,
) error {
	return nil
}

func runtimeConfig(directory string) explorercoord.Config {
	return explorercoord.Config{
		Directory: directory, Domain: coordination.DomainID("fleet-events-domain"),
		Owner: coordination.OwnerID("fleet-events-owner"),
		Authority: transaction.Authority{
			Generation: 1, Fence: 1, Holder: coordination.OwnerID("fleet-events-owner"),
			Mode: coordination.WriterModeEmbeddedPrimary, RetentionGeneration: 1, HistoryFloor: 1,
		},
		PhysicalTables: []string{Table}, Lease: time.Minute,
		RecoveryLimit: 16, RecoveryMaxPages: 64,
		RetryBackoff: time.Nanosecond, RecoveryBackoff: time.Nanosecond,
	}
}

func testEvent(index int) fleetevents.Event {
	return fleetevents.Event{
		Kind: "agent.completed", ProducerID: []byte("producer"),
		ProducerGeneration: 1,
		ActionID:           []byte(fmt.Sprintf("action-%d", index)),
		TransitionID:       []byte(fmt.Sprintf("transition-%d", index)),
		Reason:             interaction.Reason{Code: "completed"},
		Evidence: []fleetevents.Evidence{{
			SourceID: []byte("source"), PolicyID: []byte("policy"),
			ObjectID: shoal.ID(fmt.Sprintf("object-%d", index)),
		}},
		OccurredAt: time.Date(2026, 9, 5, 20, 0, index, 0, time.UTC),
	}
}
