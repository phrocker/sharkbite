// Licensed to the Apache Software Foundation (ASF) under one
// or more contributor license agreements. See the NOTICE file
// distributed with this work for additional information.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.

package ingestrouter

import (
	"context"
	"errors"
	"math/rand"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type fakeDirectory struct {
	mu      sync.Mutex
	tablets map[string]HostedTablet
	errs    map[string][]error
}

func (d *fakeDirectory) Lookup(_ context.Context, extent Extent) (HostedTablet, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	key := extent.Key()
	if queued := d.errs[key]; len(queued) > 0 {
		err := queued[0]
		d.errs[key] = queued[1:]
		if err != nil {
			return nil, err
		}
	}
	tablet := d.tablets[key]
	if tablet == nil {
		return nil, ErrNotHosted
	}
	return tablet, nil
}

type fakeTablet struct {
	extent    Extent
	fence     Fence
	authority CommitAuthority
	commit    func(context.Context, CommitRequest) error
	active    []Cell
	calls     atomic.Int64
}

func (t *fakeTablet) Extent() Extent             { return t.extent.clone() }
func (t *fakeTablet) Fence() Fence               { return t.fence }
func (t *fakeTablet) Authority() CommitAuthority { return t.authority }
func (t *fakeTablet) Commit(ctx context.Context, request CommitRequest) error {
	t.calls.Add(1)
	if t.commit != nil {
		return t.commit(ctx, request)
	}
	return nil
}

func (t *fakeTablet) ConditionalCommit(
	ctx context.Context,
	request CommitRequest,
	evaluate ConditionalEvaluator,
) (bool, error) {
	accepted, err := evaluate(ctx, t.active)
	if err != nil || !accepted {
		return accepted, err
	}
	return true, t.Commit(ctx, request)
}

func testFence() Fence {
	return Fence{ServerGeneration: "server-4", ManagerGeneration: "manager-9", Assignment: 3}
}

func testBatch(extent Extent, row string) Batch {
	return Batch{
		Extent: extent,
		Mutations: []Mutation{{
			Row: []byte(row),
			Updates: []Update{{
				ColumnFamily:     []byte("cf"),
				ColumnQualifier:  []byte("cq"),
				ColumnVisibility: []byte(`A&("team one"|B)`),
				Timestamp:        Timestamp{Set: true, Value: 17},
				Value:            []byte("value"),
			}},
		}},
	}
}

func newTestSession(t *testing.T, directory Directory) *Session {
	t.Helper()
	router, err := New(directory, DefaultLimits())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	session, err := router.Open("session-1", "5")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	return session
}

func TestApplyPartialFailureAndRetryMap(t *testing.T) {
	first := Extent{TableID: "5", EndRow: []byte("m")}
	second := Extent{TableID: "5", PrevEndRow: []byte("m")}
	replacementA := Extent{TableID: "5", PrevEndRow: []byte("m"), EndRow: []byte("t")}
	replacementB := Extent{TableID: "5", PrevEndRow: []byte("t")}
	firstTablet := &fakeTablet{extent: first, fence: testFence(), authority: AuthorityAccumuloWAL}
	secondTablet := &fakeTablet{extent: second, fence: testFence(), authority: AuthorityAccumuloWAL}
	directory := &fakeDirectory{
		tablets: map[string]HostedTablet{first.Key(): firstTablet, second.Key(): secondTablet},
		errs: map[string][]error{
			second.Key(): {
				&RouteError{Cause: ErrStaleExtent, RetryExtents: []Extent{replacementA, replacementB}},
				nil,
			},
		},
	}
	session := newTestSession(t, directory)
	request := Request{ID: "request-1", Batches: []Batch{
		testBatch(first, "a"),
		testBatch(second, "z"),
	}}

	result, err := session.Apply(context.Background(), request)
	if err != nil {
		t.Fatalf("first Apply: %v", err)
	}
	if result.Outcomes[first.Key()].Status != OutcomeApplied {
		t.Fatalf("first outcome = %#v", result.Outcomes[first.Key()])
	}
	stale := result.Outcomes[second.Key()]
	if stale.Status != OutcomeRetry || !errors.Is(stale.Cause, ErrStaleExtent) ||
		len(stale.RetryExtents) != 2 {
		t.Fatalf("stale outcome = %#v", stale)
	}
	if retries := result.RetryMap(); len(retries[second.Key()]) != 2 {
		t.Fatalf("retry map = %#v", retries)
	}

	result, err = session.Apply(context.Background(), request)
	if err != nil {
		t.Fatalf("retry Apply: %v", err)
	}
	if !result.Applied() {
		t.Fatalf("retry result = %#v", result)
	}
	if firstTablet.calls.Load() != 1 || secondTablet.calls.Load() != 1 {
		t.Fatalf("commit calls = first %d, second %d; applied extent was replayed",
			firstTablet.calls.Load(), secondTablet.calls.Load())
	}
}

func TestApplyRejectsInvalidRequestBeforeAnyCommit(t *testing.T) {
	first := Extent{TableID: "5", EndRow: []byte("m")}
	second := Extent{TableID: "5", PrevEndRow: []byte("m")}
	firstTablet := &fakeTablet{extent: first, fence: testFence(), authority: AuthorityAccumuloWAL}
	directory := &fakeDirectory{tablets: map[string]HostedTablet{
		first.Key():  firstTablet,
		second.Key(): &fakeTablet{extent: second, fence: testFence(), authority: AuthorityAccumuloWAL},
	}}
	session := newTestSession(t, directory)
	invalid := testBatch(second, "z")
	invalid.Mutations[0].Updates[0].ColumnVisibility = []byte("A&B|C")

	_, err := session.Apply(context.Background(), Request{
		ID: "invalid", Batches: []Batch{testBatch(first, "a"), invalid},
	})
	if !errors.Is(err, ErrInvalidBatch) {
		t.Fatalf("Apply error = %v, want ErrInvalidBatch", err)
	}
	if firstTablet.calls.Load() != 0 {
		t.Fatalf("first tablet committed %d times before later validation failed", firstTablet.calls.Load())
	}
}

func TestUnsupportedAuthorityIsExplicit(t *testing.T) {
	extent := Extent{TableID: "5"}
	tablet := &fakeTablet{extent: extent, fence: testFence(), authority: AuthorityUnsupported}
	session := newTestSession(t, &fakeDirectory{
		tablets: map[string]HostedTablet{extent.Key(): tablet},
	})

	result, err := session.Apply(context.Background(), Request{
		ID: "unsupported", Batches: []Batch{testBatch(extent, "row")},
	})
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	outcome := result.Outcomes[extent.Key()]
	if outcome.Status != OutcomeRejected || !errors.Is(outcome.Cause, ErrWALAuthorityUnsupported) {
		t.Fatalf("outcome = %#v", outcome)
	}
	if tablet.calls.Load() != 0 {
		t.Fatalf("unsupported tablet Commit called %d times", tablet.calls.Load())
	}
}

func TestConditionalCommitEvaluatesBeforeCommit(t *testing.T) {
	extent := Extent{TableID: "5"}
	tablet := &fakeTablet{
		extent: extent, fence: testFence(), authority: AuthorityAccumuloWAL,
		active: []Cell{{Row: []byte("r"), Value: []byte("current")}},
	}
	router, err := New(&fakeDirectory{
		tablets: map[string]HostedTablet{extent.Key(): tablet},
	}, DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}
	accepted, err := router.ConditionalCommit(
		context.Background(), "session", "request",
		testBatch(extent, "r"),
		func(_ context.Context, active []Cell) (bool, error) {
			return string(active[0].Value) == "current", nil
		},
	)
	if err != nil || !accepted || tablet.calls.Load() != 1 {
		t.Fatalf("accepted=%t calls=%d err=%v", accepted, tablet.calls.Load(), err)
	}
}

func TestConcurrentDuplicateIsCommittedOnce(t *testing.T) {
	extent := Extent{TableID: "5"}
	entered := make(chan struct{})
	release := make(chan struct{})
	var got CommitRequest
	tablet := &fakeTablet{
		extent: extent, fence: testFence(), authority: AuthorityAccumuloWAL,
		commit: func(_ context.Context, request CommitRequest) error {
			got = request
			close(entered)
			<-release
			return nil
		},
	}
	session := newTestSession(t, &fakeDirectory{
		tablets: map[string]HostedTablet{extent.Key(): tablet},
	})
	request := Request{ID: "same", Batches: []Batch{testBatch(extent, "row")}}

	var wg sync.WaitGroup
	results := make(chan Result, 2)
	errs := make(chan error, 2)
	for range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			result, err := session.Apply(context.Background(), request)
			results <- result
			errs <- err
		}()
	}
	<-entered
	close(release)
	wg.Wait()
	close(results)
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("Apply: %v", err)
		}
	}
	for result := range results {
		if !result.Applied() {
			t.Fatalf("result = %#v", result)
		}
	}
	if tablet.calls.Load() != 1 {
		t.Fatalf("Commit calls = %d, want 1", tablet.calls.Load())
	}
	if got.SessionID != "session-1" || got.RequestID != "same" {
		t.Fatalf("commit identity = session %q request %q", got.SessionID, got.RequestID)
	}
}

func TestCallerCancellationReturnsPartialAndCanResume(t *testing.T) {
	first := Extent{TableID: "5", EndRow: []byte("m")}
	second := Extent{TableID: "5", PrevEndRow: []byte("m")}
	firstTablet := &fakeTablet{extent: first, fence: testFence(), authority: AuthorityAccumuloWAL}
	entered := make(chan struct{}, 1)
	secondTablet := &fakeTablet{extent: second, fence: testFence(), authority: AuthorityAccumuloWAL}
	secondTablet.commit = func(ctx context.Context, _ CommitRequest) error {
		if secondTablet.calls.Load() == 1 {
			entered <- struct{}{}
			<-ctx.Done()
			return ctx.Err()
		}
		return nil
	}
	session := newTestSession(t, &fakeDirectory{tablets: map[string]HostedTablet{
		first.Key(): firstTablet, second.Key(): secondTablet,
	}})
	request := Request{ID: "cancel", Batches: []Batch{
		testBatch(first, "a"), testBatch(second, "z"),
	}}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	var result Result
	var applyErr error
	go func() {
		result, applyErr = session.Apply(ctx, request)
		close(done)
	}()
	<-entered
	cancel()
	<-done
	if !errors.Is(applyErr, context.Canceled) {
		t.Fatalf("Apply error = %v", applyErr)
	}
	if result.Outcomes[first.Key()].Status != OutcomeApplied ||
		result.Outcomes[second.Key()].Status != OutcomeRetry {
		t.Fatalf("partial result = %#v", result)
	}

	result, err := session.Apply(context.Background(), request)
	if err != nil || !result.Applied() {
		t.Fatalf("resumed Apply = %#v, %v", result, err)
	}
	if firstTablet.calls.Load() != 1 || secondTablet.calls.Load() != 2 {
		t.Fatalf("calls = first %d second %d", firstTablet.calls.Load(), secondTablet.calls.Load())
	}
}

func TestSessionCancellationStopsInflightCommit(t *testing.T) {
	extent := Extent{TableID: "5"}
	entered := make(chan struct{})
	tablet := &fakeTablet{
		extent: extent, fence: testFence(), authority: AuthorityAccumuloWAL,
		commit: func(ctx context.Context, _ CommitRequest) error {
			close(entered)
			<-ctx.Done()
			return ctx.Err()
		},
	}
	session := newTestSession(t, &fakeDirectory{
		tablets: map[string]HostedTablet{extent.Key(): tablet},
	})
	done := make(chan error, 1)
	go func() {
		_, err := session.Apply(context.Background(), Request{
			ID: "cancel-session", Batches: []Batch{testBatch(extent, "row")},
		})
		done <- err
	}()
	<-entered
	if !session.Cancel() || session.Cancel() {
		t.Fatal("Cancel must return true exactly once")
	}
	select {
	case err := <-done:
		if !errors.Is(err, ErrSessionCancelled) {
			t.Fatalf("Apply error = %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("in-flight commit did not observe session cancellation")
	}
}

func TestIdempotencyConflict(t *testing.T) {
	extent := Extent{TableID: "5"}
	tablet := &fakeTablet{extent: extent, fence: testFence(), authority: AuthorityAccumuloWAL}
	session := newTestSession(t, &fakeDirectory{
		tablets: map[string]HostedTablet{extent.Key(): tablet},
	})
	first := Request{ID: "same-key", Batches: []Batch{testBatch(extent, "row")}}
	if _, err := session.Apply(context.Background(), first); err != nil {
		t.Fatalf("first Apply: %v", err)
	}
	changed := first
	changed.Batches = []Batch{testBatch(extent, "other")}
	if _, err := session.Apply(context.Background(), changed); !errors.Is(err, ErrIdempotencyConflict) {
		t.Fatalf("second Apply error = %v", err)
	}
}

func TestSessionRequestLimit(t *testing.T) {
	extent := Extent{TableID: "5"}
	tablet := &fakeTablet{extent: extent, fence: testFence(), authority: AuthorityAccumuloWAL}
	limits := DefaultLimits()
	limits.MaxSessionRequests = 1
	router, err := New(&fakeDirectory{
		tablets: map[string]HostedTablet{extent.Key(): tablet},
	}, limits)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	session, err := router.Open("session", "5")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if _, err := session.Apply(context.Background(), Request{
		ID: "first", Batches: []Batch{testBatch(extent, "row")},
	}); err != nil {
		t.Fatalf("first Apply: %v", err)
	}
	if _, err := session.Apply(context.Background(), Request{
		ID: "second", Batches: []Batch{testBatch(extent, "row")},
	}); !errors.Is(err, ErrSessionLimit) {
		t.Fatalf("second Apply error = %v", err)
	}
}

func TestExtentContainsProperty(t *testing.T) {
	rng := rand.New(rand.NewSource(69))
	for range 10_000 {
		a := byte(rng.Intn(250) + 1)
		b := byte(rng.Intn(250) + 1)
		if a > b {
			a, b = b, a
		}
		if a == b {
			continue
		}
		extent := Extent{TableID: "5", PrevEndRow: []byte{a}, EndRow: []byte{b}}
		for range 8 {
			row := byte(rng.Intn(256))
			got := extent.Contains([]byte{row})
			want := row > a && row <= b
			if got != want {
				t.Fatalf("(%d,%d].Contains(%d) = %v, want %v", a, b, row, got, want)
			}
		}
	}
}
