// Licensed to the Apache Software Foundation (ASF) under one
// or more contributor license agreements. See the NOTICE file distributed
// with this work for additional information regarding copyright ownership.
package fleet

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/phrocker/shoal-oss/pkg/explorer/auth"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

type transientPublishEvents struct {
	failures  int
	attempts  int
	published int
}

func (*transientPublishEvents) MayPublishActionEvent(
	context.Context, string, ActionRecord,
) (bool, error) {
	return true, nil
}

func (e *transientPublishEvents) PublishActionEvent(
	context.Context, string, ActionRecord,
) error {
	e.attempts++
	if e.failures > 0 {
		e.failures--
		return shoal.NewError(shoal.ErrorUnavailable, "transient publication store lag")
	}
	e.published++
	return nil
}

func TestCommittedEnqueueRetriesTransientPublicationWithoutLosingAmbiguity(t *testing.T) {
	for _, test := range []struct {
		name        string
		failures    int
		wantFailure bool
	}{
		{name: "transient", failures: 2},
		{name: "persistent", failures: 5, wantFailure: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			now := time.Date(2026, 9, 6, 12, 0, 0, 0, time.UTC)
			authority, err := auth.NewAuthorityWithClock(func() time.Time { return now })
			if err != nil {
				t.Fatal(err)
			}
			registryStore := newMemoryStore()
			registry, err := NewService(Config{
				Store: registryStore, Resolver: authority.Resolver(),
				Recorder: &memoryRecorder{}, Snapshots: fixedSnapshot{now},
				Executors: executorMap{"exec": &dispatchExecutor{}},
				Clock:     func() time.Time { return now },
			})
			if err != nil {
				t.Fatal(err)
			}
			registryStore.records["agent"] = Stored{Descriptor: dispatchDescriptor(now)}
			store := newMemoryDispatchStore()
			events := &transientPublishEvents{failures: test.failures}
			service, err := NewDispatchService(DispatchConfig{
				Store: store, Registry: registry, Resolver: authority.Resolver(),
				Recorder: &dispatchRecorder{}, Events: events,
				Clock: func() time.Time { return now },
			})
			if err != nil {
				t.Fatal(err)
			}
			ctx := bindDecision(t, authority, dispatchDecision(
				t, "owner", "actor", "request", auth.OperationDispatch))
			request := dispatchEnqueue(now, "request")
			_, err = service.Enqueue(ctx, request)
			if test.wantFailure {
				if !errors.Is(err, ErrActionCommitted) || events.published != 0 {
					t.Fatalf("persistent publication = %v, published=%d", err, events.published)
				}
				if _, readErr := store.GetAction(ctx, request.ID); readErr != nil {
					t.Fatalf("failed publication lost committed action: %v", readErr)
				}
				events.failures = 0
				if _, err := service.Enqueue(ctx, request); err != nil {
					t.Fatalf("idempotent retry did not drain event: %v", err)
				}
			} else if err != nil {
				t.Fatalf("transient publication was not retried: %v", err)
			}
			if events.published != 1 {
				t.Fatalf("published %d times, want one", events.published)
			}
			page, err := store.PendingActionTransitions(ctx, request.ID, nil, MaxDispatchListResults)
			if err != nil || len(page.Transitions) != 0 {
				t.Fatalf("pending transition after success = %#v, %v", page, err)
			}
		})
	}
}
