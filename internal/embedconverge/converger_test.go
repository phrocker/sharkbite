// Licensed to the Apache Software Foundation (ASF) under one
// or more contributor license agreements. See the NOTICE file distributed
// with this work for additional information regarding copyright ownership.
// The ASF licenses this file to you under the Apache License, Version 2.0.
package embedconverge

import (
	"bytes"
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/phrocker/shoal-oss/internal/compaction"
	"github.com/phrocker/shoal-oss/internal/embeddingspace"
	"github.com/phrocker/shoal-oss/internal/iterrt"
	"github.com/phrocker/shoal-oss/internal/rfile"
	"github.com/phrocker/shoal-oss/internal/rfile/wire"
)

func passthrough() Rewriter {
	return RewriterFunc(func(
		_ context.Context, _ string, _ *iterrt.Key, value []byte,
	) ([]byte, error) {
		return value, nil
	})
}

func states(n int) []embeddingspace.FileState {
	out := make([]embeddingspace.FileState, 0, n)
	for i := 0; i < n; i++ {
		out = append(out, embeddingspace.Has("model-b"))
	}
	return out
}

func mustConverger(t *testing.T, opts ConvergerOptions) *Converger {
	t.Helper()
	if opts.Rewriter == nil {
		opts.Rewriter = passthrough()
	}
	c, err := NewConverger(opts)
	if err != nil {
		t.Fatalf("NewConverger: %v", err)
	}
	return c
}

func convergenceInput(
	t *testing.T, name string, state embeddingspace.FileState,
) compaction.Input {
	return convergenceInputWithVisibility(t, name, state, nil, []byte("value"))
}

func convergenceInputWithVisibility(
	t *testing.T,
	name string,
	state embeddingspace.FileState,
	visibility []byte,
	value []byte,
) compaction.Input {
	t.Helper()
	var buf bytes.Buffer
	w, err := rfile.NewWriter(&buf, rfile.WriterOptions{EmbeddingSpace: state})
	if err != nil {
		t.Fatalf("NewWriter(%s): %v", name, err)
	}
	if err := w.Append(&wire.Key{
		Row: []byte(name), ColumnFamily: []byte("cf"),
		ColumnVisibility: append([]byte(nil), visibility...), Timestamp: 1,
	}, append([]byte(nil), value...)); err != nil {
		t.Fatalf("Append(%s): %v", name, err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("Close(%s): %v", name, err)
	}
	return compaction.Input{Name: name, Bytes: buf.Bytes()}
}

func readSingleCompactionCell(
	t *testing.T, image []byte,
) (*wire.Key, []byte) {
	t.Helper()
	var gotKey *wire.Key
	var gotValue []byte
	err := compaction.StreamCells(compaction.Spec{
		Inputs: []compaction.Input{{Name: "output.rf", Bytes: image}},
	}, func(key *wire.Key, value []byte) error {
		if gotKey != nil {
			t.Fatal("output contained more than one cell")
		}
		gotKey = key.Clone()
		gotValue = append([]byte(nil), value...)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if gotKey == nil {
		t.Fatal("output contained no cells")
	}
	return gotKey, gotValue
}

func TestNewConvergerRequiresATargetAndItsCollaborators(t *testing.T) {
	t.Parallel()

	g := NewGovernor(GovernorOptions{Now: time.Now})
	_, err := NewConverger(ConvergerOptions{Target: "  ", Governor: g, Rewriter: passthrough()})
	if !errors.Is(err, embeddingspace.ErrNoTarget) {
		t.Fatalf("err = %v, want ErrNoTarget", err)
	}
	if _, err := NewConverger(ConvergerOptions{Target: "model-a", Rewriter: passthrough()}); err == nil {
		t.Fatal("a converger without a governor is unthrottled and must be refused")
	}
	if _, err := NewConverger(ConvergerOptions{Target: "model-a", Governor: g}); err == nil {
		t.Fatal("a converger without a rewriter must be refused")
	}
	c := mustConverger(t, ConvergerOptions{Target: "  model-a  ", Governor: g, Epoch: "  e1  "})
	if c.Target() != "model-a" {
		t.Fatalf("Target = %q, want %q", c.Target(), "model-a")
	}
	if c.Epoch() != "e1" {
		t.Fatalf("Epoch = %q, want %q", c.Epoch(), "e1")
	}
}

func TestConvergerRefusesATargetItCannotProduce(t *testing.T) {
	t.Parallel()

	c := mustConverger(t, ConvergerOptions{
		Target:   "model-a",
		Governor: NewGovernor(GovernorOptions{Now: time.Now}),
	})
	ctx := context.Background()

	_, err := c.Begin(ctx, compaction.ConvergeRequest{Target: "model-z", Inputs: states(1)})
	if !errors.Is(err, embeddingspace.ErrConvergenceUnavailable) {
		t.Fatalf("err = %v, want ErrConvergenceUnavailable: a misconfigured node must not fail the compaction", err)
	}
	_, err = c.Begin(ctx, compaction.ConvergeRequest{Target: "model-a"})
	if !errors.Is(err, embeddingspace.ErrConvergenceUnavailable) {
		t.Fatalf("err = %v, want ErrConvergenceUnavailable with no inputs", err)
	}
	attempt, err := c.Begin(ctx, compaction.ConvergeRequest{Target: " model-a ", Inputs: states(1)})
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	if attempt == nil {
		t.Fatal("an admitted Begin must return an attempt")
	}
}

func TestEgressDenialPreservesIdentityVisibilityAndSourceBytes(t *testing.T) {
	input := convergenceInputWithVisibility(
		t, "classified", embeddingspace.Has("model-old"),
		[]byte("secret&legal"), []byte("source-bytes"),
	)
	var rewrites int
	var outcomes []Outcome
	converger := mustConverger(t, ConvergerOptions{
		Target:   "model-hosted",
		Governor: NewGovernor(GovernorOptions{Now: time.Now}),
		EgressGuard: EgressGuardFunc(func(
			_ context.Context, target string, visibility []byte,
		) error {
			if target != "model-hosted" ||
				string(visibility) != "secret&legal" {
				t.Fatalf("egress check = target %q visibility %q", target, visibility)
			}
			return errors.New("classification requires local embedding")
		}),
		Rewriter: RewriterFunc(func(
			_ context.Context, _ string, _ *iterrt.Key, _ []byte,
		) ([]byte, error) {
			rewrites++
			return []byte("rewritten"), nil
		}),
		Observer: func(outcome Outcome) {
			outcomes = append(outcomes, outcome)
		},
	})
	result, err := compaction.Compact(compaction.Spec{
		Inputs: []compaction.Input{input}, Scope: iterrt.ScopeMajc,
		TargetEmbeddingSpace: "model-hosted", Converger: converger,
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Converged || result.EmbeddingSpace != embeddingspace.Has("model-old") {
		t.Fatalf("result = converged %t space %s", result.Converged, result.EmbeddingSpace)
	}
	if rewrites != 0 {
		t.Fatalf("rewriter calls = %d, want 0", rewrites)
	}
	stats := converger.governor.Stats()
	if stats.SpentFiles != 0 || stats.SpentCells != 0 {
		t.Fatalf("governor spent denied work: %+v", stats)
	}
	if len(outcomes) != 1 || !errors.Is(outcomes[0].Err, ErrEgressDenied) {
		t.Fatalf("outcomes = %+v, want typed egress denial", outcomes)
	}
	key, value := readSingleCompactionCell(t, result.Output)
	if string(key.ColumnVisibility) != "secret&legal" ||
		string(value) != "source-bytes" {
		t.Fatalf("fallback changed visibility/value: %q %q", key.ColumnVisibility, value)
	}
}

func TestRewriterCannotWidenVisibility(t *testing.T) {
	input := convergenceInputWithVisibility(
		t, "classified", embeddingspace.Has("model-old"),
		[]byte("secret"), []byte("source"),
	)
	converger := mustConverger(t, ConvergerOptions{
		Target:   "model-new",
		Governor: NewGovernor(GovernorOptions{Now: time.Now}),
		Rewriter: RewriterFunc(func(
			_ context.Context, _ string, key *iterrt.Key, _ []byte,
		) ([]byte, error) {
			key.ColumnVisibility = []byte("public")
			return []byte("rewritten"), nil
		}),
	})
	result, err := compaction.Compact(compaction.Spec{
		Inputs: []compaction.Input{input}, Scope: iterrt.ScopeMajc,
		TargetEmbeddingSpace: "model-new", Converger: converger,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !result.Converged ||
		result.EmbeddingSpace != embeddingspace.Has("model-new") {
		t.Fatalf("result = converged %t space %s", result.Converged, result.EmbeddingSpace)
	}
	key, value := readSingleCompactionCell(t, result.Output)
	if string(key.ColumnVisibility) != "secret" ||
		string(value) != "rewritten" {
		t.Fatalf("rewrite changed key visibility or lost value: %q %q", key.ColumnVisibility, value)
	}
}

// TestConvergerRefusesAMismatchedEpoch is the anti-oscillation guard: a
// converger serving one migration snapshot must not converge a
// compaction that belongs to another.
func TestConvergerRefusesAMismatchedEpoch(t *testing.T) {
	t.Parallel()

	g := NewGovernor(GovernorOptions{Now: time.Now})
	c := mustConverger(t, ConvergerOptions{Target: "model-a", Epoch: "epoch-2", Governor: g})
	ctx := context.Background()

	for _, epoch := range []string{"", "epoch-1"} {
		_, err := c.Begin(ctx, compaction.ConvergeRequest{
			Target: "model-a", Epoch: epoch, Inputs: states(1),
		})
		if !errors.Is(err, embeddingspace.ErrConvergenceUnavailable) {
			t.Fatalf("epoch %q: err = %v, want ErrConvergenceUnavailable", epoch, err)
		}
	}
	if got := g.Stats().SpentFiles; got != 0 {
		t.Fatalf("SpentFiles = %d, want a refused epoch to reserve nothing", got)
	}
	if _, err := c.Begin(ctx, compaction.ConvergeRequest{
		Target: "model-a", Epoch: " epoch-2 ", Inputs: states(1),
	}); err != nil {
		t.Fatalf("the matching epoch must be admitted: %v", err)
	}

	// An unbound converger accepts only unstamped compactions.
	unbound := mustConverger(t, ConvergerOptions{
		Target: "model-a", Governor: NewGovernor(GovernorOptions{Now: time.Now}),
	})
	if _, err := unbound.Begin(ctx, compaction.ConvergeRequest{
		Target: "model-a", Epoch: "epoch-2", Inputs: states(1),
	}); !errors.Is(err, embeddingspace.ErrConvergenceUnavailable) {
		t.Fatalf("err = %v, want an unbound converger to refuse a stamped compaction", err)
	}
}

// TestConvergerReservesOnePermitPerInputFile covers finding 5: a
// compaction merging n files consumes n units of the file budget, not
// one. Charging one would let a single compaction converge an entire
// tablet against a budget of one file.
func TestConvergerReservesOnePermitPerInputFile(t *testing.T) {
	t.Parallel()

	g := NewGovernor(GovernorOptions{Budget: Budget{MaxFiles: 5}, Now: time.Now})
	c := mustConverger(t, ConvergerOptions{Target: "model-a", Governor: g})
	ctx := context.Background()

	attempt, err := c.Begin(ctx, compaction.ConvergeRequest{Target: "model-a", Inputs: states(4)})
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	if got := g.Stats().SpentFiles; got != 4 {
		t.Fatalf("SpentFiles = %d, want 4 — one per input file", got)
	}
	// Only one permit is left, so a second four-file compaction cannot be
	// admitted at all. All-or-nothing matters: a partial reservation
	// would start work the budget cannot finish.
	if _, err := c.Begin(ctx, compaction.ConvergeRequest{
		Target: "model-a", Inputs: states(4),
	}); !errors.Is(err, ErrBudgetExhausted) {
		t.Fatalf("err = %v, want ErrBudgetExhausted", err)
	}
	if got := g.Stats().SpentFiles; got != 4 {
		t.Fatalf("SpentFiles = %d, want a refused Begin to reserve nothing", got)
	}
	attempt.End(ctx, true, 0, nil)
	if got := g.Stats().SpentFiles; got != 4 {
		t.Fatalf("SpentFiles = %d, want a converged attempt to keep all four", got)
	}
}

// TestConvergerRefundsEveryPermitOfAFailedAttempt is the other half of
// finding 5: a provider outage must give back the whole reservation, not
// one file of it, or a few failures would exhaust the budget.
func TestConvergerRefundsEveryPermitOfAFailedAttempt(t *testing.T) {
	t.Parallel()

	g := NewGovernor(GovernorOptions{Budget: Budget{MaxFiles: 4}, Now: time.Now})
	c := mustConverger(t, ConvergerOptions{Target: "model-a", Governor: g})
	ctx := context.Background()

	attempt, err := c.Begin(ctx, compaction.ConvergeRequest{Target: "model-a", Inputs: states(4)})
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	attempt.End(ctx, false, 3, errors.New("provider down"))
	if got := g.Stats().SpentFiles; got != 0 {
		t.Fatalf("SpentFiles = %d, want the whole reservation refunded", got)
	}
	if _, err := c.Begin(ctx, compaction.ConvergeRequest{
		Target: "model-a", Inputs: states(4),
	}); err != nil {
		t.Fatalf("the budget must be usable again: %v", err)
	}
}

// TestConvergerEndIsAttemptScoped covers finding 2: two concurrent
// attempts hold separate reservations, so settling one cannot refund the
// other's, and a double End cannot refund twice.
func TestConvergerEndIsAttemptScoped(t *testing.T) {
	t.Parallel()

	var mu sync.Mutex
	outcomes := 0
	g := NewGovernor(GovernorOptions{Budget: Budget{MaxFiles: 10}, Now: time.Now})
	c := mustConverger(t, ConvergerOptions{Target: "model-a", Governor: g, Observer: func(Outcome) {
		mu.Lock()
		outcomes++
		mu.Unlock()
	}})
	ctx := context.Background()

	first, err := c.Begin(ctx, compaction.ConvergeRequest{Target: "model-a", Inputs: states(3)})
	if err != nil {
		t.Fatalf("first Begin: %v", err)
	}
	second, err := c.Begin(ctx, compaction.ConvergeRequest{Target: "model-a", Inputs: states(2)})
	if err != nil {
		t.Fatalf("second Begin: %v", err)
	}
	if got := g.Stats().SpentFiles; got != 5 {
		t.Fatalf("SpentFiles = %d, want 5", got)
	}
	first.End(ctx, false, 0, errors.New("provider down"))
	if got := g.Stats().SpentFiles; got != 2 {
		t.Fatalf("SpentFiles = %d, want only the first attempt's 3 refunded", got)
	}
	// Idempotence: a second End must not refund the other attempt's
	// permits by accident.
	first.End(ctx, false, 0, nil)
	if got := g.Stats().SpentFiles; got != 2 {
		t.Fatalf("SpentFiles = %d after a duplicate End, want 2", got)
	}
	second.End(ctx, true, 0, nil)
	if got := g.Stats().SpentFiles; got != 2 {
		t.Fatalf("SpentFiles = %d, want the converged attempt to keep its 2", got)
	}
	// Three End calls, two attempts: the duplicate must not be reported
	// either, or migration accounting double-counts every retry.
	mu.Lock()
	defer mu.Unlock()
	if outcomes != 2 {
		t.Fatalf("observer saw %d outcomes, want 2", outcomes)
	}
}

// TestConvergerChargesEveryCellAsItConverts covers finding 6: the cell
// budget must be able to stop a conversion mid-file, which it cannot do
// if cells are only counted when the file finishes.
func TestConvergerChargesEveryCellAsItConverts(t *testing.T) {
	t.Parallel()

	g := NewGovernor(GovernorOptions{Budget: Budget{MaxCells: 3}, Now: time.Now})
	c := mustConverger(t, ConvergerOptions{Target: "model-a", Governor: g})
	ctx := context.Background()

	attempt, err := c.Begin(ctx, compaction.ConvergeRequest{Target: "model-a", Inputs: states(1)})
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	for i := 0; i < 3; i++ {
		if _, err := attempt.Convert(ctx, nil, []byte("cell")); err != nil {
			t.Fatalf("cell %d: %v", i, err)
		}
		if got := g.Stats().SpentCells; got != int64(i+1) {
			t.Fatalf("after cell %d SpentCells = %d, want %d", i, got, i+1)
		}
	}
	// The budget stops the file mid-stream rather than after it.
	if _, err := attempt.Convert(ctx, nil, []byte("cell")); !errors.Is(err, ErrBudgetExhausted) {
		t.Fatalf("err = %v, want ErrBudgetExhausted mid-file", err)
	}
	if got := g.Stats().SpentCells; got != 3 {
		t.Fatalf("SpentCells = %d, want a refused cell to charge nothing", got)
	}
	// End reports cells to the observer but must not charge them again.
	attempt.End(ctx, false, 3, errors.New("budget"))
	if got := g.Stats().SpentCells; got != 3 {
		t.Fatalf("SpentCells = %d after End, want cells charged exactly once", got)
	}
}

func TestConvergerConvertHonoursTheKillSwitchPerCell(t *testing.T) {
	t.Parallel()

	g := NewGovernor(GovernorOptions{Now: time.Now})
	c := mustConverger(t, ConvergerOptions{Target: "model-a", Governor: g})
	ctx := context.Background()

	attempt, err := c.Begin(ctx, compaction.ConvergeRequest{Target: "model-a", Inputs: states(1)})
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	value, err := attempt.Convert(ctx, nil, []byte("cell"))
	if err != nil {
		t.Fatalf("Convert: %v", err)
	}
	if string(value) != "cell" {
		t.Fatalf("value = %q, want %q", value, "cell")
	}

	g.Stop()
	if _, err := attempt.Convert(ctx, nil, []byte("cell")); !errors.Is(err, ErrStopped) {
		t.Fatalf("err = %v, want ErrStopped mid-stream", err)
	}
}

func TestConvergerBeginRespectsTheRateLimitAndContext(t *testing.T) {
	t.Parallel()

	clock := newFakeClock()
	g := NewGovernor(GovernorOptions{FilesPerSecond: 1, Burst: 1, Now: clock.Now})
	c := mustConverger(t, ConvergerOptions{Target: "model-a", Governor: g})
	ctx := context.Background()

	if _, err := c.Begin(ctx, compaction.ConvergeRequest{
		Target: "model-a", Inputs: states(1),
	}); err != nil {
		t.Fatalf("first Begin: %v", err)
	}
	if _, err := c.Begin(ctx, compaction.ConvergeRequest{
		Target: "model-a", Inputs: states(1),
	}); !errors.Is(err, ErrThrottled) {
		t.Fatalf("err = %v, want ErrThrottled", err)
	}

	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := c.Begin(cancelled, compaction.ConvergeRequest{
		Target: "model-a", Inputs: states(1),
	}); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
}

func TestLazyCompactionConvergesWhenBurstIsSmallerThanMergeWidth(t *testing.T) {
	t.Parallel()

	clock := newFakeClock()
	g := NewGovernor(GovernorOptions{
		FilesPerSecond: 1,
		Burst:          1,
		Budget:         Budget{MaxFiles: 4},
		Now:            clock.Now,
	})
	c := mustConverger(t, ConvergerOptions{
		Target:   "model-new",
		Governor: g,
	})
	inputs := []compaction.Input{
		convergenceInput(t, "a.rf", embeddingspace.Has("model-old")),
		convergenceInput(t, "b.rf", embeddingspace.Has("model-old")),
		convergenceInput(t, "c.rf", embeddingspace.Has("model-old")),
		convergenceInput(t, "d.rf", embeddingspace.Has("model-old")),
	}

	result, err := compaction.Compact(compaction.Spec{
		Inputs:               inputs,
		Scope:                iterrt.ScopeMajc,
		TargetEmbeddingSpace: "model-new",
		Converger:            c,
	})
	if err != nil {
		t.Fatalf("Compact: %v", err)
	}
	if !result.Converged || result.EmbeddingSpace != embeddingspace.Has("model-new") {
		t.Fatalf("result = (converged=%v, space=%s), want model-new",
			result.Converged, result.EmbeddingSpace)
	}
	stats := g.Stats()
	if stats.Admitted != 4 || stats.SpentFiles != 4 || stats.SpentCells != 4 {
		t.Fatalf("stats = %+v, want four files and four cells charged", stats)
	}
}

func TestWideLazyCompactionCancellationTakesNothing(t *testing.T) {
	t.Parallel()

	g := NewGovernor(GovernorOptions{
		FilesPerSecond: 1,
		Burst:          1,
		Budget:         Budget{MaxFiles: 4},
		Now:            newFakeClock().Now,
	})
	c := mustConverger(t, ConvergerOptions{
		Target:   "model-new",
		Governor: g,
	})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	result, err := compaction.CompactContext(ctx, compaction.Spec{
		Inputs: []compaction.Input{
			convergenceInput(t, "a.rf", embeddingspace.Has("model-old")),
			convergenceInput(t, "b.rf", embeddingspace.Has("model-old")),
			convergenceInput(t, "c.rf", embeddingspace.Has("model-old")),
			convergenceInput(t, "d.rf", embeddingspace.Has("model-old")),
		},
		Scope:                iterrt.ScopeMajc,
		TargetEmbeddingSpace: "model-new",
		Converger:            c,
	}, nil)
	if result != nil {
		t.Fatal("a cancelled compaction must not publish an output")
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("CompactContext error = %v, want context.Canceled", err)
	}
	stats := g.Stats()
	if stats.Admitted != 0 || stats.Refused != 0 || stats.SpentFiles != 0 || stats.SpentCells != 0 {
		t.Fatalf("cancelled compaction changed governor accounting: %+v", stats)
	}
}

func TestWideLazyCompactionOutagePreservesIdentityAndCanRetry(t *testing.T) {
	t.Parallel()

	clock := newFakeClock()
	g := NewGovernor(GovernorOptions{
		FilesPerSecond: 1,
		Burst:          1,
		Budget:         Budget{MaxFiles: 2},
		Now:            clock.Now,
	})
	outage := errors.New("provider unavailable")
	c := mustConverger(t, ConvergerOptions{
		Target:   "model-new",
		Governor: g,
		Rewriter: RewriterFunc(func(
			_ context.Context, _ string, _ *iterrt.Key, value []byte,
		) ([]byte, error) {
			value[0] = 'X'
			return nil, outage
		}),
	})
	inputs := []compaction.Input{
		convergenceInput(t, "a.rf", embeddingspace.Has("model-old")),
		convergenceInput(t, "b.rf", embeddingspace.Has("model-old")),
	}

	result, err := compaction.Compact(compaction.Spec{
		Inputs:               inputs,
		Scope:                iterrt.ScopeMajc,
		TargetEmbeddingSpace: "model-new",
		Converger:            c,
	})
	if err != nil {
		t.Fatalf("provider outage must not fail a same-space compaction: %v", err)
	}
	if result.Converged || result.EmbeddingSpace != embeddingspace.Has("model-old") {
		t.Fatalf("result = (converged=%v, space=%s), want preserved model-old",
			result.Converged, result.EmbeddingSpace)
	}
	values := 0
	if err := compaction.StreamCells(compaction.Spec{
		Inputs: []compaction.Input{{Name: "output.rf", Bytes: result.Output}},
	}, func(_ *wire.Key, value []byte) error {
		values++
		if string(value) != "value" {
			t.Fatalf("provider outage mutated fallback bytes: %q", value)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if values != len(inputs) {
		t.Fatalf("fallback cells = %d, want %d", values, len(inputs))
	}
	if got := g.Stats().SpentFiles; got != 0 {
		t.Fatalf("SpentFiles = %d, want the failed reservation refunded", got)
	}

	if _, err := c.Begin(context.Background(), compaction.ConvergeRequest{
		Target: "model-new", Inputs: states(2),
	}); !errors.Is(err, ErrThrottled) {
		t.Fatalf("immediate retry error = %v, want ErrThrottled", err)
	}
	clock.Advance(time.Second)
	if _, err := c.Begin(context.Background(), compaction.ConvergeRequest{
		Target: "model-new", Inputs: states(2),
	}); err != nil {
		t.Fatalf("retry after refill: %v", err)
	}
}

func TestWideLazyCompactionNeverMergesDifferentIdentitiesOnOutage(t *testing.T) {
	t.Parallel()

	g := NewGovernor(GovernorOptions{
		FilesPerSecond: 1,
		Burst:          1,
		Budget:         Budget{MaxFiles: 2},
		Now:            newFakeClock().Now,
	})
	c := mustConverger(t, ConvergerOptions{
		Target:   "model-new",
		Governor: g,
		Rewriter: RewriterFunc(func(
			_ context.Context, _ string, _ *iterrt.Key, _ []byte,
		) ([]byte, error) {
			return nil, errors.New("provider unavailable")
		}),
	})

	result, err := compaction.Compact(compaction.Spec{
		Inputs: []compaction.Input{
			convergenceInput(t, "a.rf", embeddingspace.Has("model-a")),
			convergenceInput(t, "b.rf", embeddingspace.Has("model-b")),
		},
		Scope:                iterrt.ScopeMajc,
		TargetEmbeddingSpace: "model-new",
		Converger:            c,
	})
	if result != nil {
		t.Fatal("a failed mixed-space convergence must not publish an output")
	}
	if !errors.Is(err, compaction.ErrConvergenceRequired) ||
		!errors.Is(err, embeddingspace.ErrMismatch) {
		t.Fatalf("Compact error = %v, want ErrConvergenceRequired and ErrMismatch", err)
	}
	if got := g.Stats().SpentFiles; got != 0 {
		t.Fatalf("SpentFiles = %d, want the failed reservation refunded", got)
	}
}

func TestConvergerConvertReportsRewriterFailure(t *testing.T) {
	t.Parallel()

	boom := errors.New("provider 503")
	rewriter := RewriterFunc(func(
		_ context.Context, target string, _ *iterrt.Key, _ []byte,
	) ([]byte, error) {
		if target != "model-a" {
			return nil, errors.New("rewriter was given the wrong target: " + target)
		}
		return nil, boom
	})
	c := mustConverger(t, ConvergerOptions{
		Target:   "model-a",
		Governor: NewGovernor(GovernorOptions{Now: time.Now}),
		Rewriter: rewriter,
	})
	ctx := context.Background()
	attempt, err := c.Begin(ctx, compaction.ConvergeRequest{Target: "model-a", Inputs: states(1)})
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	if _, err := attempt.Convert(ctx, nil, []byte("cell")); !errors.Is(err, boom) {
		t.Fatalf("err = %v, want the rewriter's error", err)
	}

	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := attempt.Convert(cancelled, nil, []byte("cell")); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
}

func TestConvergerEndReportsTheOutcome(t *testing.T) {
	t.Parallel()

	var mu sync.Mutex
	var seen []Outcome
	g := NewGovernor(GovernorOptions{Budget: Budget{MaxFiles: 2}, Now: time.Now})
	c := mustConverger(t, ConvergerOptions{
		Target:   "model-a",
		Epoch:    "epoch-9",
		Governor: g,
		Observer: func(o Outcome) {
			mu.Lock()
			defer mu.Unlock()
			seen = append(seen, o)
		},
	})
	ctx := context.Background()

	failed, err := c.Begin(ctx, compaction.ConvergeRequest{
		Target: "model-a", Epoch: "epoch-9", Inputs: states(1),
	})
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	failed.End(ctx, false, 7, errors.New("provider down"))

	ok, err := c.Begin(ctx, compaction.ConvergeRequest{
		Target: "model-a", Epoch: "epoch-9", Inputs: states(1),
	})
	if err != nil {
		t.Fatalf("a failed attempt must leave the file admissible: %v", err)
	}
	ok.End(ctx, true, 5, nil)

	mu.Lock()
	defer mu.Unlock()
	if len(seen) != 2 {
		t.Fatalf("observer saw %d outcomes, want 2", len(seen))
	}
	if seen[0].Converged || seen[0].Cells != 7 || seen[0].Err == nil || seen[0].Files != 1 {
		t.Fatalf("first outcome = %+v", seen[0])
	}
	if !seen[1].Converged || seen[1].Cells != 5 ||
		seen[1].Target != "model-a" || seen[1].Epoch != "epoch-9" {
		t.Fatalf("second outcome = %+v", seen[1])
	}
}

// TestConvergerIsSafeForConcurrentCompactions exercises the shared-state
// paths the race detector checks in CI: one Converger, many attempts.
func TestConvergerIsSafeForConcurrentCompactions(t *testing.T) {
	t.Parallel()

	g := NewGovernor(GovernorOptions{Now: time.Now})
	var count int64
	var mu sync.Mutex
	c := mustConverger(t, ConvergerOptions{
		Target:   "model-a",
		Governor: g,
		Observer: func(Outcome) {
			mu.Lock()
			count++
			mu.Unlock()
		},
	})
	ctx := context.Background()

	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			attempt, err := c.Begin(ctx, compaction.ConvergeRequest{
				Target: "model-a", Inputs: states(2),
			})
			if err != nil {
				return
			}
			for j := 0; j < 8; j++ {
				if _, err := attempt.Convert(ctx, nil, []byte("cell")); err != nil {
					break
				}
			}
			attempt.End(ctx, true, 8, nil)
		}()
	}
	wg.Wait()

	mu.Lock()
	defer mu.Unlock()
	if count != 16 {
		t.Fatalf("observer saw %d outcomes, want 16", count)
	}
	if got := g.Stats().SpentFiles; got != 32 {
		t.Fatalf("SpentFiles = %d, want 32", got)
	}
	if got := g.Stats().SpentCells; got != 128 {
		t.Fatalf("SpentCells = %d, want 128", got)
	}
}
