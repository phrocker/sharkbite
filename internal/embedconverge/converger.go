// Licensed to the Apache Software Foundation (ASF) under one
// or more contributor license agreements. See the NOTICE file distributed
// with this work for additional information regarding copyright ownership.
// The ASF licenses this file to you under the Apache License, Version 2.0.
package embedconverge

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"

	"github.com/phrocker/shoal-oss/internal/compaction"
	"github.com/phrocker/shoal-oss/internal/embeddingspace"
	"github.com/phrocker/shoal-oss/internal/iterrt"
)

// Rewriter re-embeds one cell's value into an embedding space.
//
// It is supplied by the layer that knows the value schema; this package
// deliberately knows nothing about it, so the throttle, budget, kill
// switch and epoch accounting are the same whatever the cells contain.
//
// The contract is strict on purpose. Rewrite must return a value whose
// vectors are entirely in target, or an error. There is no partial
// success, because the output file carries one label and a file labelled
// with a space it only partly contains is exactly the silent
// mislabelling the whole design exists to prevent. A cell carrying no
// vectors is returned unchanged and is not a failure.
//
// An error that wraps compaction.ErrConvergenceAborted fails the
// compaction outright; that is for genuine corruption. Every other error
// is read as a provider failure, which leaves the compaction to complete
// with the input's original space preserved.
type Rewriter interface {
	Rewrite(ctx context.Context, target string, key *iterrt.Key, value []byte) ([]byte, error)
}

// RewriterFunc adapts a function to Rewriter.
type RewriterFunc func(ctx context.Context, target string, key *iterrt.Key, value []byte) ([]byte, error)

// Rewrite implements Rewriter.
func (f RewriterFunc) Rewrite(
	ctx context.Context, target string, key *iterrt.Key, value []byte,
) ([]byte, error) {
	return f(ctx, target, key, value)
}

// EgressGuard decides whether a cell's exact visibility may be sent to the
// provider that produces target. It receives no cell value or source text.
//
// A hosted-provider deployment uses this hook to deny classifications that
// may only be embedded in place. A local provider can allow every visibility.
// Denial is best effort: the compaction completes unconverged with the input
// identity and bytes preserved.
type EgressGuard interface {
	CheckEmbeddingEgress(ctx context.Context, target string, visibility []byte) error
}

// EgressGuardFunc adapts a function to EgressGuard.
type EgressGuardFunc func(ctx context.Context, target string, visibility []byte) error

// CheckEmbeddingEgress implements EgressGuard.
func (f EgressGuardFunc) CheckEmbeddingEgress(
	ctx context.Context, target string, visibility []byte,
) error {
	return f(ctx, target, visibility)
}

// ErrEgressDenied reports a classification policy that prohibited sending a
// cell to the target provider.
var ErrEgressDenied = errors.New("embedconverge: embedding egress denied")

// Converger implements compaction.Converger on top of a Governor and a
// Rewriter. One Converger is meant to be shared by every compaction on a
// node: the Governor it holds is what makes the rate limit and budget
// apply to the node rather than to each job.
//
// It holds no per-attempt state. Everything one compaction consumes —
// the rate-limit permit, the file reservation, the cells it charges —
// lives on the attempt Begin returns, so two concurrent compactions can
// neither mis-attribute nor settle each other's work.
type Converger struct {
	target   string
	epoch    string
	governor *Governor
	rewriter Rewriter
	egress   EgressGuard
	observer func(Outcome)
}

// Outcome reports one compaction's convergence result to an observer.
type Outcome struct {
	Target    string
	Epoch     string
	Files     int64
	Converged bool
	Cells     int64
	Err       error
}

// ConvergerOptions configures a Converger.
type ConvergerOptions struct {
	// Target is the identity this Converger's embedder actually
	// produces — it must come from model.EmbeddingSpaceIdentity, never
	// from CacheIdentity. CacheIdentity folds in the provider base URL,
	// the HTTP client, credential identity and the maximum text size,
	// none of which change a single vector: keying convergence on it
	// would make moving an Ollama server to a different port invalidate
	// every embedding in the corpus despite the vectors being
	// bit-identical.
	Target string

	// Epoch binds this Converger to one migration snapshot. Empty means
	// unbound, which accepts only compactions that are themselves
	// unstamped.
	//
	// Requiring an exact match is what stops a target change from making
	// the corpus oscillate: after the target moves from A to B a new
	// epoch is taken, and compactions still carrying the old epoch are
	// refused rather than publishing A-labelled files into a table that
	// has moved on. Refusing is safe because a refused compaction still
	// runs, unconverged, preserving its inputs.
	Epoch string

	// Governor supplies the rate limit, budget and kill switch.
	Governor *Governor

	// Rewriter re-embeds cell values.
	Rewriter Rewriter

	// EgressGuard enforces classification-specific provider selection before
	// source text can reach a hosted provider.
	EgressGuard EgressGuard

	// Observer, when non-nil, is called once per settled attempt. It
	// must be safe for concurrent use.
	Observer func(Outcome)
}

// NewConverger builds a Converger.
func NewConverger(opts ConvergerOptions) (*Converger, error) {
	normalized, err := embeddingspace.ParseTarget(opts.Target)
	if err != nil {
		return nil, err
	}
	if normalized == "" {
		return nil, fmt.Errorf("%w: converger needs a target embedding space",
			embeddingspace.ErrNoTarget)
	}
	if opts.Governor == nil {
		return nil, fmt.Errorf("embedconverge: converger needs a governor")
	}
	if opts.Rewriter == nil {
		return nil, fmt.Errorf("embedconverge: converger needs a rewriter")
	}
	return &Converger{
		target:   normalized,
		epoch:    strings.TrimSpace(opts.Epoch),
		governor: opts.Governor,
		rewriter: opts.Rewriter,
		egress:   opts.EgressGuard,
		observer: opts.Observer,
	}, nil
}

// Target reports the space this Converger converges to.
func (c *Converger) Target() string { return c.target }

// Epoch reports the migration snapshot this Converger is bound to, or
// empty when it is unbound.
func (c *Converger) Epoch() string { return c.epoch }

// Begin admits one compaction's convergence attempt.
//
// Every refusal wraps embeddingspace.ErrConvergenceUnavailable and
// returns a nil attempt, having taken nothing. That is what makes the
// accounting sound: there is no path on which a caller settles an
// attempt that was never admitted.
func (c *Converger) Begin(
	ctx context.Context, req compaction.ConvergeRequest,
) (compaction.ConvergeAttempt, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if strings.TrimSpace(req.Target) != c.target {
		// The table wants a space this embedder cannot produce. Refusing
		// as "unavailable" rather than as an error is deliberate: the
		// compaction still runs and preserves its inputs, and a node
		// that *can* produce the target converges the file later. Failing
		// the compaction would let one misconfigured node block every
		// compaction on the table.
		return nil, fmt.Errorf("%w: converger produces %q but the table targets %q",
			embeddingspace.ErrConvergenceUnavailable, c.target, strings.TrimSpace(req.Target))
	}
	if epoch := strings.TrimSpace(req.Epoch); epoch != c.epoch {
		return nil, fmt.Errorf("%w: converger serves epoch %q but the compaction carries %q",
			embeddingspace.ErrConvergenceUnavailable, c.epoch, epoch)
	}
	if len(req.Inputs) == 0 {
		return nil, fmt.Errorf("%w: nothing to converge", embeddingspace.ErrConvergenceUnavailable)
	}
	permit, err := c.governor.AdmitFiles(int64(len(req.Inputs)))
	if err != nil {
		return nil, err
	}
	return &convergeAttempt{converger: c, permit: permit}, nil
}

// convergeAttempt is one compaction's admitted convergence. It owns the
// Governor reservation for that compaction and nothing else's.
type convergeAttempt struct {
	converger *Converger
	permit    *Permit

	mu    sync.Mutex
	ended bool
}

// Convert rewrites one cell.
//
// The kill switch and the cell budget are enforced here, per cell,
// rather than only at Begin. A migration over a large corpus can sit
// inside a single compaction for a long time: an operator who hits stop
// should not have to wait for it to finish, and a cell budget that only
// settled between files could not bound one enormous file at all.
func (a *convergeAttempt) Convert(
	ctx context.Context, key *iterrt.Key, value []byte,
) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if a.converger.egress != nil {
		var visibility []byte
		if key != nil {
			visibility = append([]byte(nil), key.ColumnVisibility...)
		}
		if err := a.converger.egress.CheckEmbeddingEgress(
			ctx, a.converger.target, visibility); err != nil {
			if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				return nil, err
			}
			return nil, fmt.Errorf(
				"%w: %w: %v",
				embeddingspace.ErrConvergenceUnavailable, ErrEgressDenied, err)
		}
	}
	// Charged before the provider is called, so a refusal costs nothing
	// and a charge is never recorded for work that did not happen.
	if err := a.converger.governor.ChargeCell(); err != nil {
		return nil, err
	}
	var rewriteKey *iterrt.Key
	if key != nil {
		rewriteKey = key.Clone()
	}
	rewriteValue := append([]byte(nil), value...)
	return a.converger.rewriter.Rewrite(
		ctx, a.converger.target, rewriteKey, rewriteValue)
}

// End settles the attempt against the Governor's reservation and reports
// it. It is idempotent.
//
// cells is reported to the observer only; conversions were already
// charged as they happened, so settling them again here would count
// every cell twice.
func (a *convergeAttempt) End(_ context.Context, converted bool, cells int64, err error) {
	a.mu.Lock()
	if a.ended {
		a.mu.Unlock()
		return
	}
	a.ended = true
	a.mu.Unlock()

	files := a.permit.Files()
	if converted {
		a.permit.Settle(files)
	} else {
		a.permit.Settle(0)
	}
	if a.converger.observer != nil {
		a.converger.observer(Outcome{
			Target:    a.converger.target,
			Epoch:     a.converger.epoch,
			Files:     files,
			Converged: converted,
			Cells:     cells,
			Err:       err,
		})
	}
}

var (
	_ compaction.Converger       = (*Converger)(nil)
	_ compaction.ConvergeAttempt = (*convergeAttempt)(nil)
)
