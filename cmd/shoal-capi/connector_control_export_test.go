package main

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

func TestOwnedCancellationCancelsCurrentAndFutureScans(t *testing.T) {
	cancellation := newOwnedCancellation()
	scanner := newOwnedScanner(nil, nil, nil)

	ctx, done, err := scanner.beginCancelable(0, cancellation)
	if err != nil {
		t.Fatal(err)
	}
	cancellation.cancel()
	select {
	case <-ctx.Done():
		if !errors.Is(ctx.Err(), context.Canceled) {
			t.Fatalf("context error = %v, want canceled", ctx.Err())
		}
	case <-time.After(time.Second):
		t.Fatal("cancel did not interrupt active scan")
	}
	done()

	ctx, done, err = scanner.beginCancelable(0, cancellation)
	if err != nil {
		t.Fatal(err)
	}
	defer done()
	if !errors.Is(ctx.Err(), context.Canceled) {
		t.Fatalf("future context error = %v, want canceled", ctx.Err())
	}
}

func TestOwnedCancellationCloseCancelsAndJoinsActiveScan(t *testing.T) {
	cancellation := newOwnedCancellation()
	scanner := newOwnedScanner(nil, nil, nil)
	ctx, done, err := scanner.beginCancelable(0, cancellation)
	if err != nil {
		t.Fatal(err)
	}

	closeReturned := make(chan struct{})
	go func() {
		cancellation.close()
		close(closeReturned)
	}()

	select {
	case <-ctx.Done():
	case <-time.After(time.Second):
		t.Fatal("close did not cancel active scan")
	}
	select {
	case <-closeReturned:
		t.Fatal("close returned before active scan completed")
	default:
	}
	done()
	select {
	case <-closeReturned:
	case <-time.After(time.Second):
		t.Fatal("close did not join active scan")
	}
}

func TestOwnedCancellationConcurrentCancelIsIdempotent(t *testing.T) {
	cancellation := newOwnedCancellation()
	const callers = 32
	var wait sync.WaitGroup
	wait.Add(callers)
	for range callers {
		go func() {
			defer wait.Done()
			cancellation.cancel()
		}()
	}
	wait.Wait()
	if !cancellation.isCancelled() {
		t.Fatal("cancellation is not marked canceled")
	}
}
