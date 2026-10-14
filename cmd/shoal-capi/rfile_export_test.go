package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	publicrfile "github.com/phrocker/shoal-oss/rfile"
)

func TestStatusForRFileErrors(t *testing.T) {
	tests := []struct {
		err  error
		want int32
	}{
		{publicrfile.ErrClosed, 6},
		{publicrfile.ErrNoTop, 9},
		{publicrfile.ErrInvalidSeekable, 1},
		{publicrfile.ErrInvalidPath, 1},
		{publicrfile.ErrOutOfOrder, 1},
		{publicrfile.ErrUnsupportedCodec, 4},
		{publicrfile.ErrInvalidLocalityGroup, 1},
		{os.ErrNotExist, 9},
		{os.ErrPermission, 10},
	}
	for _, test := range tests {
		t.Run(test.err.Error(), func(t *testing.T) {
			if got := int32(statusForError(fmt.Errorf("wrapped: %w", test.err))); got != test.want {
				t.Fatalf("status = %d, want %d", got, test.want)
			}
		})
	}
}

func TestRFileLifecycleCloseCancelsAndWaits(t *testing.T) {
	var lifecycle rfileLifecycle
	ctx, done, status, err := lifecycle.begin(0)
	if err != nil || status != 0 {
		t.Fatalf("begin = (%d, %v), want success", status, err)
	}

	operationDone := make(chan struct{})
	go func() {
		defer close(operationDone)
		<-ctx.Done()
		if !errors.Is(ctx.Err(), context.Canceled) {
			t.Errorf("operation context error = %v, want canceled", ctx.Err())
		}
		done()
	}()

	var closes atomic.Int32
	if err := lifecycle.close(func() error {
		closes.Add(1)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	<-operationDone
	if closes.Load() != 1 {
		t.Fatalf("close calls = %d, want 1", closes.Load())
	}
	if _, _, _, err := lifecycle.begin(0); err == nil {
		t.Fatal("begin after close succeeded")
	}
	if _, err := lifecycle.retain(); err == nil {
		t.Fatal("retain after close succeeded")
	}
	if err := lifecycle.close(func() error {
		closes.Add(1)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if closes.Load() != 1 {
		t.Fatalf("idempotent close calls = %d, want 1", closes.Load())
	}
}

func TestRFileLifecycleConcurrentRetainAndClose(t *testing.T) {
	var lifecycle rfileLifecycle
	const getters = 32
	release := make(chan struct{})
	started := make(chan struct{}, getters)
	var wait sync.WaitGroup
	wait.Add(getters)
	for range getters {
		go func() {
			defer wait.Done()
			done, err := lifecycle.retain()
			if err != nil {
				t.Errorf("retain: %v", err)
				return
			}
			started <- struct{}{}
			<-release
			done()
		}()
	}
	for range getters {
		<-started
	}

	closed := make(chan struct{})
	go func() {
		_ = lifecycle.close(func() error { return nil })
		close(closed)
	}()
	select {
	case <-closed:
		t.Fatal("close returned while getters were active")
	case <-time.After(10 * time.Millisecond):
	}
	close(release)
	wait.Wait()
	select {
	case <-closed:
	case <-time.After(time.Second):
		t.Fatal("close did not return after getters completed")
	}
}

func TestRFileLifecycleDeadline(t *testing.T) {
	var lifecycle rfileLifecycle
	ctx, done, status, err := lifecycle.begin(1)
	if err != nil || status != 0 {
		t.Fatalf("begin = (%d, %v), want success", status, err)
	}
	defer done()
	select {
	case <-ctx.Done():
		if !errors.Is(ctx.Err(), context.DeadlineExceeded) {
			t.Fatalf("context error = %v, want deadline exceeded", ctx.Err())
		}
	case <-time.After(time.Second):
		t.Fatal("deadline did not fire")
	}
}
