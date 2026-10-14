package main

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"
)

func TestWaitForShutdownStopsServerOnContextCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	stopper := &recordingStopper{}
	done := make(chan struct{})

	go func() {
		waitForShutdown(ctx, nil, slog.New(slog.NewTextHandler(io.Discard, nil)), stopper)
		close(done)
	}()

	cancel()

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("waitForShutdown did not return after cancellation")
	}
	if stopper.calls != 1 {
		t.Fatalf("Stop calls = %d, want 1", stopper.calls)
	}
}

func TestWaitForShutdownReturnsAfterStopError(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	stopper := &recordingStopper{err: errors.New("stop failed")}
	done := make(chan struct{})

	go func() {
		waitForShutdown(ctx, nil, slog.New(slog.NewTextHandler(io.Discard, nil)), stopper)
		close(done)
	}()

	cancel()

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("waitForShutdown did not return after Stop error")
	}
	if stopper.calls != 1 {
		t.Fatalf("Stop calls = %d, want 1", stopper.calls)
	}
}

type recordingStopper struct {
	calls int
	err   error
}

func (s *recordingStopper) Stop() error {
	s.calls++
	return s.err
}
