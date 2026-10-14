package tserverprocess

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/phrocker/shoal-oss/internal/tserver"
)

func TestSupervisorReconnectsAfterLockLossAndStopsOnDrain(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var generations atomic.Int32
	supervisor := Supervisor{
		Host: tserver.NewHost(),
		NewGeneration: func() (*tserver.ServiceLock, tserver.ServiceLockData, error) {
			generations.Add(1)
			return nil, tserver.ServiceLockData{}, nil
		},
		RetryBackoff: time.Millisecond,
		Participate: func(ctx context.Context, _ *tserver.ServiceLock, _ *tserver.Host, _ tserver.ServiceLockData, _ func([]tserver.Extent)) error {
			if generations.Load() == 1 {
				return tserver.ErrLockLost
			}
			<-ctx.Done()
			return ctx.Err()
		},
	}
	done := make(chan error, 1)
	go func() { done <- supervisor.Run(ctx) }()
	deadline := time.Now().Add(time.Second)
	for generations.Load() < 2 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("Run error = %v", err)
	}
	if generations.Load() != 2 {
		t.Fatalf("generations = %d", generations.Load())
	}
}

func TestSupervisorFailsClosedOnNonLockError(t *testing.T) {
	want := errors.New("stale epoch")
	supervisor := Supervisor{
		Host: tserver.NewHost(),
		NewGeneration: func() (*tserver.ServiceLock, tserver.ServiceLockData, error) {
			return nil, tserver.ServiceLockData{}, nil
		},
		Participate: func(context.Context, *tserver.ServiceLock, *tserver.Host, tserver.ServiceLockData, func([]tserver.Extent)) error {
			return want
		},
	}
	if err := supervisor.Run(context.Background()); !errors.Is(err, want) {
		t.Fatalf("Run error = %v", err)
	}
}
