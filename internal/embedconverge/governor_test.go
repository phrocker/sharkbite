// Licensed to the Apache Software Foundation (ASF) under one
// or more contributor license agreements. See the NOTICE file distributed
// with this work for additional information regarding copyright ownership.
// The ASF licenses this file to you under the Apache License, Version 2.0.
package embedconverge

import (
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/phrocker/shoal-oss/internal/embeddingspace"
)

// fakeClock is a manually advanced clock, so the token bucket's
// behaviour is exact rather than dependent on how long the test took.
type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func newFakeClock() *fakeClock {
	return &fakeClock{now: time.Unix(1700000000, 0)}
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

// admit is the "did it succeed" shorthand the throttle tests want; the
// permit itself is exercised by the budget tests.
func admit(g *Governor) error {
	_, err := g.AdmitFile()
	return err
}

func TestGovernorThrottlesAndRefills(t *testing.T) {
	t.Parallel()

	clock := newFakeClock()
	g := NewGovernor(GovernorOptions{FilesPerSecond: 2, Burst: 2, Now: clock.Now})

	for i := 0; i < 2; i++ {
		if err := admit(g); err != nil {
			t.Fatalf("admission %d must come from the initial burst: %v", i, err)
		}
	}
	err := admit(g)
	if !errors.Is(err, ErrThrottled) {
		t.Fatalf("err = %v, want ErrThrottled", err)
	}
	if !errors.Is(err, embeddingspace.ErrConvergenceUnavailable) {
		t.Fatalf("throttling must read as convergence-unavailable, got %v", err)
	}

	clock.Advance(500 * time.Millisecond)
	if err := admit(g); err != nil {
		t.Fatalf("one token must have refilled after 500ms at 2/s: %v", err)
	}
	if err := admit(g); !errors.Is(err, ErrThrottled) {
		t.Fatalf("err = %v, want ErrThrottled after spending the refill", err)
	}

	clock.Advance(time.Hour)
	if err := admit(g); err != nil {
		t.Fatalf("a long idle period must refill: %v", err)
	}
	// The bucket is capped at Burst, so an hour of idleness does not
	// bank an hour of permits.
	if err := admit(g); err != nil {
		t.Fatalf("the second burst permit must be available: %v", err)
	}
	if err := admit(g); !errors.Is(err, ErrThrottled) {
		t.Fatalf("err = %v, want ErrThrottled: the bucket must cap at Burst", err)
	}
}

// TestGovernorAdmitFilesIsAllOrNothing is finding 5's throttle half. A
// multi-file compaction takes one token per file, and a reservation that
// cannot be met in full takes nothing — a partially admitted compaction
// would start converging files it has no permit for.
func TestGovernorAdmitFilesIsAllOrNothing(t *testing.T) {
	t.Parallel()

	clock := newFakeClock()
	g := NewGovernor(GovernorOptions{FilesPerSecond: 1, Burst: 3, Now: clock.Now})

	first, err := g.AdmitFile()
	if err != nil {
		t.Fatalf("AdmitFile: %v", err)
	}
	first.Settle(1)
	if _, err := g.AdmitFiles(3); !errors.Is(err, ErrThrottled) {
		t.Fatalf("err = %v, want ErrThrottled: only two of three tokens are available", err)
	}
	if got := g.Stats().SpentFiles; got != 1 {
		t.Fatalf("SpentFiles = %d, want the refused reservation to take nothing", got)
	}
	permit, err := g.AdmitFiles(2)
	if err != nil {
		t.Fatalf("AdmitFiles(2): %v", err)
	}
	if permit.Files() != 2 {
		t.Fatalf("permit.Files() = %d, want 2", permit.Files())
	}
	if got := g.Stats().SpentFiles; got != 3 {
		t.Fatalf("SpentFiles = %d, want 3", got)
	}
	if _, err := g.AdmitFiles(0); err == nil {
		t.Fatal("a zero-file reservation is a caller bug and must be refused")
	}
}

func TestGovernorWideAtomicReservationBorrowsAndRepaysRateCapacity(t *testing.T) {
	t.Parallel()

	clock := newFakeClock()
	g := NewGovernor(GovernorOptions{
		FilesPerSecond: 1,
		Burst:          1,
		Budget:         Budget{MaxFiles: 4},
		Now:            clock.Now,
	})

	permit, err := g.AdmitFiles(4)
	if err != nil {
		t.Fatalf("AdmitFiles(4) with Burst 1: %v", err)
	}
	if permit.Files() != 4 {
		t.Fatalf("permit.Files() = %d, want 4", permit.Files())
	}
	if got := g.Stats().SpentFiles; got != 4 {
		t.Fatalf("SpentFiles = %d, want the complete atomic reservation", got)
	}

	// A failed provider attempt refunds the file budget, but it does not
	// refund rate capacity: the next attempt must wait out the four files
	// admitted by the first one.
	permit.Settle(0)
	if got := g.Stats().SpentFiles; got != 0 {
		t.Fatalf("SpentFiles = %d, want the failed reservation refunded", got)
	}
	if _, err := g.AdmitFiles(4); !errors.Is(err, ErrThrottled) {
		t.Fatalf("immediate retry error = %v, want ErrThrottled", err)
	} else if !strings.Contains(err.Error(), "atomic 4-file reservation exceeds burst 1.000") ||
		!strings.Contains(err.Error(), "rate debt outstanding") ||
		!strings.Contains(err.Error(), "retry after at least 3.000s") {
		t.Fatalf("wide-refusal error is not actionable: %v", err)
	}
	g.Pause()
	g.Resume()
	if _, err := g.AdmitFiles(4); !errors.Is(err, ErrThrottled) {
		t.Fatalf("pause/resume retry error = %v, want rate debt to remain", err)
	}

	clock.Advance(2 * time.Second)
	if _, err := g.AdmitFiles(4); !errors.Is(err, ErrThrottled) {
		t.Fatalf("partial-refill retry error = %v, want ErrThrottled", err)
	}
	if got := g.Stats().SpentFiles; got != 0 {
		t.Fatalf("SpentFiles = %d, want refused retries to reserve nothing", got)
	}

	clock.Advance(time.Second)
	if _, err := g.AdmitFiles(4); err != nil {
		t.Fatalf("retry after repaying rate capacity: %v", err)
	}
}

func TestGovernorSmallAdmissionsCannotStarveAWideReservation(t *testing.T) {
	t.Parallel()

	g := NewGovernor(GovernorOptions{
		FilesPerSecond: 1,
		Burst:          3,
		Budget:         Budget{MaxFiles: 8},
		Now:            newFakeClock().Now,
	})
	for i := 0; i < 3; i++ {
		permit, err := g.AdmitFile()
		if err != nil {
			t.Fatalf("small admission %d: %v", i, err)
		}
		permit.Settle(1)
	}

	permit, err := g.AdmitFiles(4)
	if err != nil {
		t.Fatalf("wide admission after the regular burst was consumed: %v", err)
	}
	permit.Settle(4)
	stats := g.Stats()
	if stats.Admitted != 7 || stats.SpentFiles != 7 {
		t.Fatalf("stats = %+v, want all three small files and the four-file merge admitted", stats)
	}
	if _, err := g.AdmitFile(); !errors.Is(err, ErrThrottled) {
		t.Fatalf("admission during wide-request rate debt = %v, want ErrThrottled", err)
	}
}

func TestGovernorZeroRateIsUnlimited(t *testing.T) {
	t.Parallel()

	g := NewGovernor(GovernorOptions{Now: newFakeClock().Now})
	for i := 0; i < 100; i++ {
		if err := admit(g); err != nil {
			t.Fatalf("admission %d: %v", i, err)
		}
	}
}

func TestGovernorFileBudgetStopsAdmitting(t *testing.T) {
	t.Parallel()

	g := NewGovernor(GovernorOptions{Budget: Budget{MaxFiles: 2}, Now: newFakeClock().Now})
	for i := 0; i < 2; i++ {
		permit, err := g.AdmitFile()
		if err != nil {
			t.Fatalf("admission %d: %v", i, err)
		}
		permit.Settle(1)
	}
	err := admit(g)
	if !errors.Is(err, ErrBudgetExhausted) {
		t.Fatalf("err = %v, want ErrBudgetExhausted", err)
	}
	if !errors.Is(err, embeddingspace.ErrConvergenceUnavailable) {
		t.Fatalf("an exhausted budget must read as convergence-unavailable, got %v", err)
	}
}

// TestGovernorFileBudgetCountsReservationsNotCompletions is why the
// budget is charged at admission rather than at settlement: an operator
// who caps a migration at n files must not be able to have 10n files
// in flight because none of them has finished yet.
func TestGovernorFileBudgetCountsReservationsNotCompletions(t *testing.T) {
	t.Parallel()

	g := NewGovernor(GovernorOptions{Budget: Budget{MaxFiles: 3}, Now: newFakeClock().Now})
	first, err := g.AdmitFiles(2)
	if err != nil {
		t.Fatalf("AdmitFiles(2): %v", err)
	}
	if _, err := g.AdmitFiles(2); !errors.Is(err, ErrBudgetExhausted) {
		t.Fatalf("err = %v, want the outstanding reservation to count", err)
	}
	first.Settle(0)
	if _, err := g.AdmitFiles(2); err != nil {
		t.Fatalf("a refunded reservation must free the budget: %v", err)
	}
}

func TestPermitSettleIsIdempotentAndClamped(t *testing.T) {
	t.Parallel()

	g := NewGovernor(GovernorOptions{Budget: Budget{MaxFiles: 8}, Now: newFakeClock().Now})
	permit, err := g.AdmitFiles(4)
	if err != nil {
		t.Fatalf("AdmitFiles: %v", err)
	}
	permit.Settle(1)
	if got := g.Stats().SpentFiles; got != 1 {
		t.Fatalf("SpentFiles = %d, want 3 of 4 refunded", got)
	}
	// A second settlement must not refund again, or one attempt would
	// eat into another's reservation.
	permit.Settle(0)
	if got := g.Stats().SpentFiles; got != 1 {
		t.Fatalf("SpentFiles = %d after a duplicate Settle, want 1", got)
	}

	other, err := g.AdmitFiles(2)
	if err != nil {
		t.Fatalf("AdmitFiles: %v", err)
	}
	// Nonsense inputs are clamped rather than trusted: over-reporting
	// would spend permits nobody reserved and under-reporting is a
	// refund.
	other.Settle(99)
	if got := g.Stats().SpentFiles; got != 3 {
		t.Fatalf("SpentFiles = %d, want the settlement clamped to the reservation", got)
	}
}

func TestGovernorRefundsTheFilePermitWhenConvergenceFailed(t *testing.T) {
	t.Parallel()

	g := NewGovernor(GovernorOptions{Budget: Budget{MaxFiles: 1}, Now: newFakeClock().Now})
	permit, err := g.AdmitFile()
	if err != nil {
		t.Fatalf("first admission: %v", err)
	}
	// A provider outage must not consume the entire file budget.
	if err := g.ChargeCells(3); err != nil {
		t.Fatalf("ChargeCells: %v", err)
	}
	permit.Settle(0)
	permit, err = g.AdmitFile()
	if err != nil {
		t.Fatalf("a failed file must be admissible again: %v", err)
	}
	if err := g.ChargeCells(3); err != nil {
		t.Fatalf("ChargeCells: %v", err)
	}
	permit.Settle(1)
	if err := admit(g); !errors.Is(err, ErrBudgetExhausted) {
		t.Fatalf("err = %v, want ErrBudgetExhausted once the file actually converged", err)
	}
	if got := g.Stats().SpentCells; got != 6 {
		t.Fatalf("SpentCells = %d, want 6: cells stay charged even when the file failed", got)
	}
}

func TestGovernorCellBudgetStopsMidFile(t *testing.T) {
	t.Parallel()

	g := NewGovernor(GovernorOptions{Budget: Budget{MaxCells: 10}, Now: newFakeClock().Now})
	if err := g.CheckRunning(); err != nil {
		t.Fatalf("CheckRunning: %v", err)
	}
	for i := 0; i < 10; i++ {
		if err := g.ChargeCell(); err != nil {
			t.Fatalf("cell %d must be inside a budget of 10: %v", i, err)
		}
	}
	err := g.ChargeCell()
	if !errors.Is(err, ErrBudgetExhausted) {
		t.Fatalf("err = %v, want ErrBudgetExhausted at the cap", err)
	}
	if got := g.Stats().SpentCells; got != 10 {
		t.Fatalf("SpentCells = %d, want a refused cell to charge nothing", got)
	}
	// The kill switch remains independent of the budget: a governor that
	// is out of cells is still "running".
	if err := g.CheckRunning(); err != nil {
		t.Fatalf("CheckRunning = %v, want the budget not to change run state", err)
	}
	if err := g.ChargeCells(20); !errors.Is(err, ErrBudgetExhausted) {
		t.Fatalf("err = %v, want a bulk charge over budget refused", err)
	}
}

func TestGovernorPauseIsReversibleAndStopIsNot(t *testing.T) {
	t.Parallel()

	g := NewGovernor(GovernorOptions{Now: newFakeClock().Now})
	g.Pause()
	if got := g.State(); got != StatePaused {
		t.Fatalf("state = %q, want %q", got, StatePaused)
	}
	if err := admit(g); !errors.Is(err, ErrPaused) {
		t.Fatalf("err = %v, want ErrPaused", err)
	}
	if err := g.CheckRunning(); !errors.Is(err, ErrPaused) {
		t.Fatalf("CheckRunning err = %v, want ErrPaused", err)
	}
	if err := g.ChargeCell(); !errors.Is(err, ErrPaused) {
		t.Fatalf("ChargeCell err = %v, want ErrPaused", err)
	}
	if got := g.Stats().SpentCells; got != 0 {
		t.Fatalf("SpentCells = %d, want a paused charge to cost nothing", got)
	}
	g.Resume()
	if err := admit(g); err != nil {
		t.Fatalf("resume must re-admit: %v", err)
	}

	g.Stop()
	if err := admit(g); !errors.Is(err, ErrStopped) {
		t.Fatalf("err = %v, want ErrStopped", err)
	}
	// The kill switch is terminal: neither resuming nor pausing may
	// take a stopped governor back into a state that admits work.
	g.Resume()
	g.Pause()
	if got := g.State(); got != StateStopped {
		t.Fatalf("state = %q, want %q: stop must be terminal", got, StateStopped)
	}
	if err := g.CheckRunning(); !errors.Is(err, ErrStopped) {
		t.Fatalf("CheckRunning err = %v, want ErrStopped", err)
	}
	if err := g.ChargeCell(); !errors.Is(err, ErrStopped) {
		t.Fatalf("ChargeCell err = %v, want ErrStopped", err)
	}
}

func TestGovernorStatsCountAdmissionsAndRefusals(t *testing.T) {
	t.Parallel()

	clock := newFakeClock()
	g := NewGovernor(GovernorOptions{FilesPerSecond: 1, Burst: 1, Now: clock.Now})
	if err := admit(g); err != nil {
		t.Fatalf("AdmitFile: %v", err)
	}
	_ = admit(g)
	_ = admit(g)
	stats := g.Stats()
	if stats.Admitted != 1 {
		t.Fatalf("Admitted = %d, want 1", stats.Admitted)
	}
	if stats.Refused != 2 {
		t.Fatalf("Refused = %d, want 2", stats.Refused)
	}
	if stats.State != StateRunning {
		t.Fatalf("State = %q, want running", stats.State)
	}
}

func TestGovernorIsSafeUnderConcurrentUse(t *testing.T) {
	t.Parallel()

	g := NewGovernor(GovernorOptions{FilesPerSecond: 1000, Burst: 50, Now: time.Now})
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 200; j++ {
				if permit, err := g.AdmitFiles(int64(j%3 + 1)); err == nil {
					_ = g.CheckRunning()
					_ = g.ChargeCell()
					permit.Settle(int64(j % 2))
					// A duplicate settlement from a racing goroutine
					// must not double-refund.
					permit.Settle(0)
				}
				_ = g.Stats()
				_ = g.State()
			}
		}()
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		for j := 0; j < 50; j++ {
			g.Pause()
			g.Resume()
		}
	}()
	wg.Wait()
	g.Stop()
	if err := admit(g); !errors.Is(err, ErrStopped) {
		t.Fatalf("err = %v, want ErrStopped", err)
	}
}

func TestGovernorConcurrentWideReservationsAdmitOnlyOneBurst(t *testing.T) {
	t.Parallel()

	g := NewGovernor(GovernorOptions{
		FilesPerSecond: 1,
		Burst:          1,
		Budget:         Budget{MaxFiles: 64},
		Now:            newFakeClock().Now,
	})
	const (
		workers = 16
		width   = 4
	)
	var wg sync.WaitGroup
	var mu sync.Mutex
	admitted := 0
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			permit, err := g.AdmitFiles(width)
			if err != nil {
				if !errors.Is(err, ErrThrottled) {
					t.Errorf("AdmitFiles(%d): %v", width, err)
				}
				return
			}
			mu.Lock()
			admitted++
			mu.Unlock()
			permit.Settle(width)
		}()
	}
	wg.Wait()

	mu.Lock()
	gotAdmitted := admitted
	mu.Unlock()
	if gotAdmitted != 1 {
		t.Fatalf("admitted reservations = %d, want exactly one", gotAdmitted)
	}
	stats := g.Stats()
	if stats.Admitted != width || stats.Refused != workers-1 || stats.SpentFiles != width {
		t.Fatalf("stats = %+v, want admitted=%d refused=%d spent=%d",
			stats, width, workers-1, width)
	}
}
