// Licensed to the Apache Software Foundation (ASF) under one
// or more contributor license agreements. See the NOTICE file distributed
// with this work for additional information regarding copyright ownership.
// The ASF licenses this file to you under the Apache License, Version 2.0.
package embeddingspace

import (
	"errors"
	"fmt"
	"strings"
)

// Decision is what convergence must do with one file relative to a
// table's target space.
type Decision string

const (
	// DecisionNone means the table declares no convergence target, so
	// there is nothing to converge toward.
	DecisionNone Decision = "none"

	// DecisionSkip means the file is already in the target space.
	// Re-embedding it would burn provider budget and churn vectors
	// without changing a single score, so convergence must not run.
	DecisionSkip Decision = "skip"

	// DecisionRewrite means the file must be re-embedded into the target
	// space. It covers three genuinely different inputs that all resolve
	// the same way: embeddings in another space (a re-embed), no
	// embeddings at all (a backfill), and an unknown space (a file
	// written before the space was recorded, which we have not
	// established anything about and therefore cannot trust).
	DecisionRewrite Decision = "rewrite"
)

var (
	// ErrNoTarget reports that an operation needing a convergence target
	// was asked to run without one.
	ErrNoTarget = errors.New("embedding space: no convergence target configured")

	// ErrNotMonotonic reports a rewrite that moved a file away from the
	// table's target space. Convergence is only ever allowed to move
	// toward the target, so this is a bug in the caller, not a
	// recoverable condition.
	ErrNotMonotonic = errors.New("embedding space: convergence moved a file away from the target")

	// ErrConvergenceUnavailable reports that convergence could not be
	// attempted or could not be completed: the embedding provider is
	// down or rate limiting, the migration budget is exhausted, or an
	// operator stopped the migration.
	//
	// It is an expected condition, not a fault. A caller that sees it
	// must complete its work with the input's original space preserved,
	// so that a later attempt can pick the file up again.
	ErrConvergenceUnavailable = errors.New("embedding space: convergence unavailable")
)

// ParseTarget normalizes and validates a table's configured target
// space. An empty (or whitespace-only) property means "no target", which
// is not an error: a table that never opted into a space is simply not
// migrating.
func ParseTarget(raw string) (string, error) {
	target := strings.TrimSpace(raw)
	if target == "" {
		return "", nil
	}
	if err := Has(target).Validate(); err != nil {
		return "", err
	}
	return target, nil
}

// PlanConvergence decides what convergence should do with a file
// currently in state current, given the table's target space.
//
// The rule is deliberately blunt, because the alternative is guessing:
// only a file that positively declares the target space is left alone.
// A file whose space is unknown is rewritten rather than assumed usable,
// which is what "fail closed" means here — an unknown file is not
// treated as if it already carried target-space vectors.
func PlanConvergence(target string, current FileState) (Decision, error) {
	normalized, err := ParseTarget(target)
	if err != nil {
		return "", err
	}
	if err := current.Validate(); err != nil {
		return "", err
	}
	if normalized == "" {
		return DecisionNone, nil
	}
	if current.HasEmbeddings() && current.Identity == normalized {
		return DecisionSkip, nil
	}
	return DecisionRewrite, nil
}

// EnsureMonotonic verifies that a completed convergence step did not
// move a file away from target, and did not make a positive claim the
// file never earned.
//
// Exactly three outcomes are legal:
//
//   - the state is unchanged, which is the provider-failure outcome: a
//     compaction that could not reach the embedding provider preserves
//     the input's space rather than mislabelling the output;
//   - the state is exactly the target, which is convergence;
//   - the state degraded to unknown. That is the only legal degradation,
//     and it is what merging an embedded file with an unembedded one
//     already produces (see Compatible). unknown claims nothing, so it
//     cannot be wrong.
//
// Everything else is a violation, including a degrade to no_embeddings.
// no_embeddings is not a weaker claim than has_embeddings — it is a
// different positive assertion, "this file holds no vectors at all".
// Applying it to a file that still holds vectors mislabels the file just
// as badly as the wrong identity would, and it hides those vectors from
// planning without anything ever reporting an error. unknown is the
// fail-closed degradation; no_embeddings is a claim, and a claim has to
// be earned.
func EnsureMonotonic(target string, before, after FileState) error {
	normalized, err := ParseTarget(target)
	if err != nil {
		return err
	}
	if err := before.Validate(); err != nil {
		return err
	}
	if err := after.Validate(); err != nil {
		return err
	}
	if before == after {
		return nil
	}
	if normalized != "" && after == Has(normalized) {
		return nil
	}
	if after.State == StateUnknown {
		return nil
	}
	if normalized != "" && before == Has(normalized) {
		return fmt.Errorf("%w: %s -> %s left the target %s",
			ErrNotMonotonic, before.String(), after.String(), Has(normalized).String())
	}
	if after.State == StateNoEmbeddings {
		return fmt.Errorf(
			"%w: %s -> %s asserts the file holds no vectors; degrade to %s instead",
			ErrNotMonotonic, before.String(), after.String(), Unknown().String())
	}
	return fmt.Errorf("%w: %s -> %s is neither the input space nor the target",
		ErrNotMonotonic, before.String(), after.String())
}

// Converged reports whether state has reached target. A file whose space
// is not Known cannot have converged, because convergence is a positive
// claim about which model produced the vectors.
func Converged(target string, state FileState) bool {
	normalized, err := ParseTarget(target)
	if err != nil || normalized == "" {
		return false
	}
	if !state.Known() {
		return false
	}
	return state.HasEmbeddings() && state.Identity == normalized
}
