// Licensed to the Apache Software Foundation (ASF) under one
// or more contributor license agreements. See the NOTICE file distributed
// with this work for additional information regarding copyright ownership.
// The ASF licenses this file to you under the Apache License, Version 2.0.
package compaction

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/phrocker/shoal-oss/internal/embeddingspace"
	"github.com/phrocker/shoal-oss/internal/iterrt"
)

// ConvergeRequest describes one compaction's convergence request.
//
// It is a struct rather than an argument list because the set of things
// a Converger has to agree about — target, epoch, input states — grows
// with the migration protocol, and a struct lets it grow without
// breaking every implementation.
type ConvergeRequest struct {
	// Target is the table's desired embedding space.
	Target string

	// Epoch identifies the migration snapshot this compaction is acting
	// for. Empty means the caller is not running an epoch-tracked
	// migration.
	//
	// It exists so a target change cannot make the corpus oscillate. A
	// Converger belonging to epoch E refuses a compaction stamped E',
	// and a compaction stamped E' never publishes a file labelled with
	// E's target, so the two migrations cannot take turns rewriting the
	// same files in opposite directions.
	Epoch string

	// Inputs are the per-input states, in Spec.Inputs order.
	Inputs []embeddingspace.FileState
}

// Converger admits convergence attempts. It is the seam between the
// composer, which knows nothing about vectors, and a migration engine,
// which owns the embedding provider, the rate limit, the budget and the
// kill switch.
//
// A Converger is shared across compactions; a ConvergeAttempt is not.
// Everything a compaction consumes — the rate-limit permit, the file
// budget reservation, the cell charges — belongs to the attempt, so two
// concurrent compactions cannot settle each other's accounting.
type Converger interface {
	// Begin admits (or refuses) one compaction's convergence.
	//
	// Returning an error wrapping
	// embeddingspace.ErrConvergenceUnavailable is the normal way to say
	// "not now": the provider is down, the migration is throttled, the
	// budget is spent, the epoch is stale, or an operator hit the kill
	// switch. The compaction then runs unconverged. Any other error
	// aborts the compaction.
	//
	// A refusal must return a nil ConvergeAttempt and must have settled
	// whatever it provisionally took, because the caller has nothing to
	// End. Only a successful Begin creates an obligation to End.
	Begin(ctx context.Context, req ConvergeRequest) (ConvergeAttempt, error)
}

// ConvergeAttempt is one compaction's admitted convergence. Exactly one
// End follows a successful Begin.
type ConvergeAttempt interface {
	// Convert rewrites one cell's value so that any vectors it carries
	// are in the target space. It must return a value that is either
	// fully in the target space or an error; a partial conversion would
	// mislabel the output file.
	//
	// Convert is called for every cell, including cells that carry no
	// vectors at all, which it should return unchanged.
	Convert(ctx context.Context, key *iterrt.Key, value []byte) ([]byte, error)

	// End settles the attempt exactly once. converted reports whether
	// the output was actually written in the target space; cells is how
	// many cells were converted before the attempt ended; err is the
	// failure that ended it, or nil.
	End(ctx context.Context, converted bool, cells int64, err error)
}

// ErrConvergenceAborted reports a convergence attempt that failed for a
// reason the composer must not paper over — a malformed cell, a bug in
// the rewriter. Distinct from embeddingspace.ErrConvergenceUnavailable,
// which is the expected "provider is unhappy, try later" signal.
var ErrConvergenceAborted = errors.New("compaction: convergence aborted")

// ErrConvergenceRequired reports a compaction that cannot run at all
// right now: its inputs carry two different embedding identities, and
// convergence — the only thing that could reconcile them into one file —
// is unavailable.
//
// There is no correct unconverged output for this case. One compaction
// produces one file carrying one label, so the alternatives are to
// re-embed every input into the target (which needs the provider), to
// label the merged file with an identity whose vectors it only partly
// contains (silent mislabelling), or to not run. This error is the third
// option, reported before any work is done.
//
// It is retryable, and callers must treat it that way. The inputs are
// untouched and still individually well labelled, so the tablet stays
// consistent and queryable; the same compaction succeeds once the
// provider is reachable, or once an operator gives the node an embedder
// that can produce the target. It wraps embeddingspace.ErrMismatch, so
// callers that already classify identity conflicts keep working.
var ErrConvergenceRequired = errors.New("compaction: convergence required but unavailable")

// convergeAttempt is one compaction's decision about convergence.
type convergeAttempt struct {
	// attempt is non-nil only when a Converger admitted the attempt, so
	// the output may be labelled with target.
	attempt ConvergeAttempt
	target  string
	epoch   string
	// label is what the output file must claim.
	label embeddingspace.FileState
	// inputs are the per-input states, in Spec.Inputs order.
	inputs []embeddingspace.FileState
	// names are the input names, in Spec.Inputs order, so a refusal can
	// say which file it is about rather than only which states clashed.
	names []string
}

func (a *convergeAttempt) active() bool { return a != nil && a.attempt != nil }

// planConvergence resolves what this compaction should do about the
// table's target space, calling Begin when a rewrite is both wanted and
// possible.
//
// Three properties are load bearing here:
//
//   - Idempotence. When every input already declares the target space,
//     no Converger is consulted at all, so repeatedly compacting a
//     converged tablet costs nothing and does not churn vectors.
//   - Never merging spaces under one label. Without an admitted
//     Converger the merged label comes from embeddingspace.Compatible,
//     which refuses to fold two different identities into one file. With
//     an admitted Converger every cell is rewritten into the target, so
//     mixed input is legitimate: the output really does contain only
//     target-space vectors.
//   - Nothing is spent on a compaction that cannot be published. The
//     conflict check runs before Begin, so a compaction that would end
//     in ErrConvergenceRequired never takes a rate-limit permit or a
//     budget reservation it would only have to give back.
func planConvergence(
	ctx context.Context, spec Spec, states []embeddingspace.FileState,
) (*convergeAttempt, error) {
	attempt := &convergeAttempt{
		inputs: states,
		names:  inputNames(spec.Inputs),
		epoch:  strings.TrimSpace(spec.EmbeddingEpoch),
	}
	target, err := embeddingspace.ParseTarget(spec.TargetEmbeddingSpace)
	if err != nil {
		return nil, fmt.Errorf("compaction: %w", err)
	}
	attempt.target = target
	if attempt.epoch != "" && target == "" {
		// An epoch names the migration an output belongs to, and a
		// migration is defined by the target it converges toward. An
		// epoch without a target would stamp outputs with a migration
		// that cannot exist, which is worse than not stamping them at
		// all: it would let a coordinator attribute unconverged files to
		// a migration.
		return nil, fmt.Errorf(
			"compaction: embedding epoch %q set without a target embedding space", attempt.epoch)
	}

	wanted := false
	for _, state := range states {
		decision, err := embeddingspace.PlanConvergence(target, state)
		if err != nil {
			return nil, fmt.Errorf("compaction: %w", err)
		}
		if decision == embeddingspace.DecisionRewrite {
			wanted = true
		}
	}

	// Resolve the unconverged label up front. It is both the answer when
	// nothing needs converging and the fallback when convergence is
	// refused, and computing it before Begin is what keeps a doomed
	// compaction from spending a permit.
	label, mergeErr := embeddingspace.Compatible("merge embedding spaces", states...)
	if mergeErr != nil {
		mergeErr = annotateEmbeddingRefusal(mergeErr, attempt.names, states)
	}
	if !wanted || spec.Converger == nil {
		if mergeErr != nil {
			return nil, mergeErr
		}
		attempt.label = label
		return attempt, nil
	}

	admitted, beginErr := spec.Converger.Begin(ctx, ConvergeRequest{
		Target: target,
		Epoch:  spec.EmbeddingEpoch,
		Inputs: states,
	})
	if beginErr != nil {
		if !errors.Is(beginErr, embeddingspace.ErrConvergenceUnavailable) {
			return nil, fmt.Errorf("compaction: begin convergence: %w", beginErr)
		}
		if mergeErr != nil {
			return nil, convergenceRequired(mergeErr, beginErr)
		}
		attempt.label = label
		return attempt, nil
	}
	if admitted == nil {
		return nil, fmt.Errorf("compaction: converger admitted the attempt but returned no handle")
	}
	attempt.attempt = admitted
	attempt.label = embeddingspace.Has(target)
	return attempt, nil
}

// inputNames extracts the per-input labels a refusal reports, in
// Spec.Inputs order.
func inputNames(inputs []Input) []string {
	names := make([]string, 0, len(inputs))
	for _, in := range inputs {
		names = append(names, in.Name)
	}
	return names
}

// convergenceRequired reports inputs that only convergence could merge,
// at a moment when convergence is not available. Both causes are
// wrapped: the mismatch so existing identity-conflict handling still
// matches, and ErrConvergenceRequired so a caller can tell "retry this
// later" apart from "these files must never be compacted together".
func convergenceRequired(mergeErr, cause error) error {
	return fmt.Errorf("%w: %w (provider: %v)", ErrConvergenceRequired, mergeErr, cause)
}

// preserved rebuilds the attempt as an unconverged one after a mid-flight
// provider failure. The output then claims exactly what the inputs
// claimed, which is the self-healing outcome: a later compaction, or a
// forced migration pass, picks the file up again.
//
// When the inputs carry conflicting identities there is no such output,
// so this reports ErrConvergenceRequired rather than inventing one.
func (a *convergeAttempt) preserved(cause error) (*convergeAttempt, error) {
	label, err := embeddingspace.Compatible("merge embedding spaces", a.inputs...)
	if err != nil {
		return nil, convergenceRequired(annotateEmbeddingRefusal(err, a.names, a.inputs), cause)
	}
	return &convergeAttempt{
		target: a.target, epoch: a.epoch, label: label, inputs: a.inputs, names: a.names,
	}, nil
}

// verify checks the output label against every input state before the
// label is written into the file and the metadata entry.
func (a *convergeAttempt) verify() error {
	for _, state := range a.inputs {
		if err := embeddingspace.EnsureMonotonic(a.target, state, a.label); err != nil {
			return annotateEmbeddingRefusal(fmt.Errorf("compaction: %w", err), a.names, a.inputs)
		}
	}
	return a.label.Validate()
}

// convergingIterator rewrites each cell's value on its way to the
// writer. Conversion happens on positioning (Seek/Next) rather than in
// GetTopValue so that a provider failure has somewhere to go: the
// interface's readers cannot return an error, so an iterator that
// converted lazily would have to panic or silently emit the unconverted
// value, and the second of those is a mislabelled file.
type convergingIterator struct {
	source    iterrt.SortedKeyValueIterator
	attempt   ConvergeAttempt
	ctx       context.Context
	value     []byte
	err       error
	converted int64
}

func newConvergingIterator(
	ctx context.Context, source iterrt.SortedKeyValueIterator, attempt ConvergeAttempt,
) *convergingIterator {
	return &convergingIterator{source: source, attempt: attempt, ctx: ctx}
}

func (c *convergingIterator) Init(
	source iterrt.SortedKeyValueIterator, options map[string]string, env iterrt.IteratorEnvironment,
) error {
	return c.source.Init(source, options, env)
}

func (c *convergingIterator) Seek(r iterrt.Range, columnFamilies [][]byte, inclusive bool) error {
	if err := c.source.Seek(r, columnFamilies, inclusive); err != nil {
		return err
	}
	c.convert()
	return c.err
}

func (c *convergingIterator) Next() error {
	if err := c.source.Next(); err != nil {
		return err
	}
	c.convert()
	return c.err
}

// prime converts the cell the wrapped source is already positioned on.
// buildSource seeks the stack before returning, so wrapping it afterwards
// would otherwise skip conversion of the very first cell.
func (c *convergingIterator) prime() error {
	c.convert()
	return c.err
}

func (c *convergingIterator) convert() {
	c.value = nil
	if c.err != nil || !c.source.HasTop() {
		return
	}
	converted, err := c.attempt.Convert(c.ctx, c.source.GetTopKey(), c.source.GetTopValue())
	if err != nil {
		c.err = err
		return
	}
	c.value = converted
	c.converted++
}

// HasTop reports no cell once conversion has failed, which stops the
// drain. Callers must consult Err afterwards: a truncated stream that
// looked like a clean end-of-input would publish a partial file.
func (c *convergingIterator) HasTop() bool { return c.err == nil && c.source.HasTop() }

func (c *convergingIterator) GetTopKey() *iterrt.Key { return c.source.GetTopKey() }

func (c *convergingIterator) GetTopValue() []byte { return c.value }

func (c *convergingIterator) DeepCopy(env iterrt.IteratorEnvironment) iterrt.SortedKeyValueIterator {
	return newConvergingIterator(c.ctx, c.source.DeepCopy(env), c.attempt)
}

func (c *convergingIterator) Err() error { return c.err }
