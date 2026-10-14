// Licensed to the Apache Software Foundation (ASF) under one
// or more contributor license agreements. See the NOTICE file distributed
// with this work for additional information regarding copyright ownership.
// The ASF licenses this file to you under the Apache License, Version 2.0.

// Package embedconverge orchestrates the migration of a table's corpus
// from one embedding space to another.
//
// It provides the three things a migration needs beyond the per-file
// space identity Phase 1 records:
//
//   - a Governor, which rate limits convergence, spends a declared
//     budget, and gives an operator a kill switch that takes effect on
//     the next cell rather than at the end of the job;
//   - an Epoch, which freezes the set of files a migration is
//     responsible for so progress is measured against a fixed
//     denominator instead of against a corpus that keeps growing under
//     it;
//   - a Migration, which tracks each file in the epoch through a
//     monotonic, idempotent, interruptible, resumable and abandonable
//     lifecycle.
//
// Two convergence modes share all of it. Lazy convergence attaches a
// Converger to ordinary compaction and converges files as compaction
// naturally touches them; forced convergence drives an Epoch to
// completion on demand. Both consult one Governor, so an operator's rate
// limit, budget and kill switch bound the whole migration rather than
// one mode of it.
package embedconverge

import (
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/phrocker/shoal-oss/internal/embeddingspace"
)

// RunState is the Governor's operator-visible state.
type RunState string

const (
	// StateRunning admits convergence subject to the rate limit and
	// budget.
	StateRunning RunState = "running"

	// StatePaused admits nothing but is reversible. Work already in
	// flight finishes unconverged rather than being torn up.
	StatePaused RunState = "paused"

	// StateStopped is the kill switch: terminal, irreversible, and
	// checked per cell so a migration stops without waiting for the
	// compaction it is inside to finish.
	StateStopped RunState = "stopped"
)

var (
	// ErrThrottled reports that the rate limit has no permit available
	// right now.
	ErrThrottled = errors.New("embedconverge: rate limited")

	// ErrBudgetExhausted reports that the migration has spent its
	// declared budget.
	ErrBudgetExhausted = errors.New("embedconverge: budget exhausted")

	// ErrPaused reports that an operator paused the migration.
	ErrPaused = errors.New("embedconverge: paused")

	// ErrStopped reports that an operator stopped the migration.
	ErrStopped = errors.New("embedconverge: stopped")
)

// Budget bounds what one migration may consume before it stops admitting
// work. A zero field means unlimited for that dimension.
//
// Files and cells are budgeted separately because neither bounds the
// other: a migration of a few enormous files and a migration of many
// tiny ones cost very different amounts at the provider, and only the
// cell count tracks what is actually being embedded.
type Budget struct {
	// MaxFiles caps how many files convergence may be admitted for.
	MaxFiles int64
	// MaxCells caps how many cells may be converted in total.
	MaxCells int64
}

// GovernorOptions configures a Governor.
type GovernorOptions struct {
	// FilesPerSecond is the sustained admission rate. Zero or negative
	// means unlimited, which is only appropriate for a local embedder; a
	// hosted provider must be given a real rate.
	FilesPerSecond float64

	// Burst is the reusable token capacity after an idle period. Values
	// below one are raised to one, because a bucket that cannot hold a
	// single permit never admits anything.
	//
	// One atomic AdmitFiles request may be wider than Burst. Such a
	// request may overdraw a non-negative bucket and borrows the excess
	// permits from future refill, so a compaction merge width larger than
	// Burst is delayed by the rate limit rather than refused forever.
	// Only one overdraft can be outstanding: a negative balance refuses
	// every later request until refill repays it. This prevents the
	// exception from increasing the sustained rate.
	Burst float64

	// Budget bounds total consumption. The zero value is unlimited.
	Budget Budget

	// Now injects the clock. Nil uses time.Now.
	//
	// The rate limiter is a token bucket over this clock rather than a
	// timer, so it holds no goroutine, admits without blocking, and is
	// exactly reproducible in tests.
	Now func() time.Time
}

// Governor rate limits and budgets convergence, and carries the kill
// switch. It is safe for concurrent use; one Governor is meant to be
// shared by every compaction and every migration worker on a node, so
// that the limits bound the node rather than each job independently.
type Governor struct {
	mu     sync.Mutex
	state  RunState
	rate   float64
	burst  float64
	tokens float64
	last   time.Time
	now    func() time.Time
	budget Budget

	spentFiles int64
	spentCells int64
	admitted   int64
	refused    int64
}

// NewGovernor creates a Governor in the running state with a full token
// bucket, so the first admission is immediate rather than waiting out
// one interval.
func NewGovernor(opts GovernorOptions) *Governor {
	now := opts.Now
	if now == nil {
		now = time.Now
	}
	burst := opts.Burst
	if burst < 1 {
		burst = 1
	}
	rate := opts.FilesPerSecond
	if rate < 0 {
		rate = 0
	}
	return &Governor{
		state:  StateRunning,
		rate:   rate,
		burst:  burst,
		tokens: burst,
		last:   now(),
		now:    now,
		budget: opts.Budget,
	}
}

// State reports the current operator state.
func (g *Governor) State() RunState {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.state
}

// Pause suspends admission reversibly. It is a no-op once stopped: the
// kill switch is terminal, and letting a pause "downgrade" a stop would
// make the stop reversible by accident.
func (g *Governor) Pause() {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.state == StateRunning {
		g.state = StatePaused
	}
}

// Resume returns a paused Governor to running. It is a no-op once
// stopped.
func (g *Governor) Resume() {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.state == StatePaused {
		g.state = StateRunning
	}
}

// Stop is the kill switch. It is terminal: nothing re-enables a stopped
// Governor, so an operator who has stopped a runaway migration does not
// have to win a race against whatever might try to resume it.
func (g *Governor) Stop() {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.state = StateStopped
}

// AdmitFiles reserves permission to converge n files as one atomic
// reservation, returning the Permit that must settle it.
//
// The reservation is all-or-nothing. A compaction that merges eight
// files re-embeds eight files' worth of cells, so charging it one unit
// of MaxFiles would let a budget of "100 files" convert thousands.
// Taking the whole reservation up front also means a compaction never
// starts converging on a budget that cannot cover it.
//
// The returned error always wraps
// embeddingspace.ErrConvergenceUnavailable, so a caller that only needs
// to know "not now" can test for that, while an operator interface can
// still tell throttling from an exhausted budget. On error the Permit is
// nil and nothing was taken, so there is nothing to settle.
//
// Admission never blocks. A compaction that is refused runs unconverged
// and preserves its inputs' space, which is strictly better than holding
// a compaction slot open waiting for a token.
//
// A reservation wider than Burst is not permanently impossible. When
// there is no existing rate debt it is admitted once, atomically, and
// the bucket goes negative until refill repays the borrowed capacity.
func (g *Governor) AdmitFiles(n int64) (*Permit, error) {
	if n <= 0 {
		return nil, fmt.Errorf("%w: nothing to admit", embeddingspace.ErrConvergenceUnavailable)
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if err := g.stateErrorLocked(); err != nil {
		g.refused++
		return nil, err
	}
	if err := g.fileBudgetErrorLocked(n); err != nil {
		g.refused++
		return nil, err
	}
	if err := g.cellBudgetErrorLocked(0); err != nil {
		g.refused++
		return nil, err
	}
	if !g.takeTokensLocked(n) {
		g.refused++
		return nil, g.throttledErrorLocked(n)
	}
	g.spentFiles += n
	g.admitted += n
	return &Permit{governor: g, files: n}, nil
}

// AdmitFile reserves one file. It is AdmitFiles(1).
func (g *Governor) AdmitFile() (*Permit, error) { return g.AdmitFiles(1) }

// Permit is one admitted reservation. It belongs to exactly one
// convergence attempt, which is what keeps concurrent compactions from
// settling each other's accounting: without it, any attempt reaching a
// shared Governor could refund a permit it never took, and the file
// budget would drift upward under concurrency.
//
// Settle must be called exactly once; further calls are no-ops so a
// deferred Settle alongside an explicit one cannot double-refund.
type Permit struct {
	mu       sync.Mutex
	governor *Governor
	files    int64
	settled  bool
}

// Files reports how many file permits this reservation holds.
func (p *Permit) Files() int64 {
	if p == nil {
		return 0
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.files
}

// Settle closes the reservation, keeping converged of its file permits
// charged and refunding the rest.
//
// Refunding what did not converge is what keeps a provider outage from
// silently consuming the file budget: the files were not moved, so they
// must remain admissible. Cells are not settled here — they are charged
// as they are converted, by ChargeCell, so that MaxCells can stop a
// migration part way through a file rather than only between files.
func (p *Permit) Settle(converged int64) {
	if p == nil {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.settled {
		return
	}
	p.settled = true
	if converged < 0 {
		converged = 0
	}
	if converged > p.files {
		converged = p.files
	}
	refund := p.files - converged
	p.governor.refundFiles(refund)
}

func (g *Governor) refundFiles(n int64) {
	if n <= 0 {
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	g.spentFiles -= n
	if g.spentFiles < 0 {
		g.spentFiles = 0
	}
}

// CheckRunning is the per-cell kill-switch probe. It is what makes a
// stop take effect immediately instead of at the end of whatever file is
// being rewritten.
func (g *Governor) CheckRunning() error {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.stateErrorLocked()
}

// ChargeCell charges one converted cell against the budget, atomically
// with the state and budget check.
//
// Charging per cell rather than per file is what gives MaxCells any
// force. A budget only debited when a file finished could not stop a
// single enormous file from converting far past the cap, and the
// operator asking for a bounded migration would get an unbounded one.
func (g *Governor) ChargeCell() error { return g.ChargeCells(1) }

// ChargeCells charges n converted cells.
func (g *Governor) ChargeCells(n int64) error {
	if n <= 0 {
		return nil
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if err := g.stateErrorLocked(); err != nil {
		return err
	}
	if err := g.cellBudgetErrorLocked(n); err != nil {
		return err
	}
	g.spentCells += n
	return nil
}

// Stats is a consistent snapshot of the Governor's counters.
type Stats struct {
	State      RunState
	SpentFiles int64
	SpentCells int64
	Admitted   int64
	Refused    int64
	Budget     Budget
}

// Stats returns a snapshot of admission accounting.
func (g *Governor) Stats() Stats {
	g.mu.Lock()
	defer g.mu.Unlock()
	return Stats{
		State:      g.state,
		SpentFiles: g.spentFiles,
		SpentCells: g.spentCells,
		Admitted:   g.admitted,
		Refused:    g.refused,
		Budget:     g.budget,
	}
}

func (g *Governor) stateErrorLocked() error {
	switch g.state {
	case StateStopped:
		return fmt.Errorf("%w: %w", embeddingspace.ErrConvergenceUnavailable, ErrStopped)
	case StatePaused:
		return fmt.Errorf("%w: %w", embeddingspace.ErrConvergenceUnavailable, ErrPaused)
	default:
		return nil
	}
}

func (g *Governor) fileBudgetErrorLocked(pendingFiles int64) error {
	if g.budget.MaxFiles > 0 && g.spentFiles+pendingFiles > g.budget.MaxFiles {
		return fmt.Errorf("%w: %w: %d of %d files reserved",
			embeddingspace.ErrConvergenceUnavailable, ErrBudgetExhausted,
			g.spentFiles+pendingFiles, g.budget.MaxFiles)
	}
	return nil
}

func (g *Governor) cellBudgetErrorLocked(pendingCells int64) error {
	if g.budget.MaxCells > 0 && g.spentCells+pendingCells > g.budget.MaxCells {
		return fmt.Errorf("%w: %w: %d of %d cells converted",
			embeddingspace.ErrConvergenceUnavailable, ErrBudgetExhausted,
			g.spentCells+pendingCells, g.budget.MaxCells)
	}
	return nil
}

// takeTokensLocked spends n permits, or none at all. Partial admission
// would let a large compaction start converging with a reservation too
// small to finish it.
//
// A request wider than the bucket is the important exception to the
// ordinary token-bucket rule. Refusing it forever would starve every
// lazy compaction whose merge width exceeds Burst. Instead, a bucket
// with no existing debt may admit it atomically and go negative by the
// excess. That negative balance is rate debt: refill pays it back before
// another oversized request can proceed, preserving the sustained rate
// without a waiter queue, retry allocation, or partially admitted
// compaction. A stream of smaller admissions cannot permanently starve
// a wide request because any non-negative balance is sufficient for the
// one bounded overdraft.
func (g *Governor) takeTokensLocked(n int64) bool {
	if g.rate <= 0 {
		return true
	}
	now := g.now()
	if elapsed := now.Sub(g.last); elapsed > 0 {
		g.tokens += elapsed.Seconds() * g.rate
		if g.tokens > g.burst {
			g.tokens = g.burst
		}
		g.last = now
	}
	required := float64(n)
	if required > g.burst {
		if g.tokens < 0 {
			return false
		}
		g.tokens -= required
		return true
	}
	if g.tokens < required {
		return false
	}
	g.tokens -= required
	return true
}

func (g *Governor) throttledErrorLocked(n int64) error {
	if required := float64(n); required > g.burst {
		waitSeconds := -g.tokens / g.rate
		if waitSeconds < 0 {
			waitSeconds = 0
		}
		return fmt.Errorf(
			"%w: %w: atomic %d-file reservation exceeds burst %.3f with rate debt outstanding; retry after at least %.3fs without another admission (rate balance %.3f at %.3f/s)",
			embeddingspace.ErrConvergenceUnavailable, ErrThrottled,
			n, g.burst, waitSeconds, g.tokens, g.rate)
	}
	return fmt.Errorf("%w: %w: %d permits requested, %.3f available at %.3f/s",
		embeddingspace.ErrConvergenceUnavailable, ErrThrottled, n, g.tokens, g.rate)
}
