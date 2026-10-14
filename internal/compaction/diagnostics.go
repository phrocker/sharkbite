// Licensed to the Apache Software Foundation (ASF) under one
// or more contributor license agreements. See the NOTICE file distributed
// with this work for additional information regarding copyright ownership.
// The ASF licenses this file to you under the Apache License, Version 2.0.
package compaction

import (
	"errors"
	"fmt"
	"strings"

	"github.com/phrocker/shoal-oss/internal/embeddingspace"
)

// BackfillCommand is the operator instruction embedded in every refusal
// caused by an input whose embedding space was never established.
//
// It is a constant rather than prose repeated at each call site because
// an operator reading a compactor log has to be able to copy it, and a
// message that drifts between two error paths is a message they cannot
// trust.
const BackfillCommand = "shoalctl embedding-backfill"

// ErrEmbeddingBackfillRequired reports a compaction that was refused
// because at least one input's embedding space has never been
// established — the file carries no file.embedding column, or the column
// says unknown.
//
// It exists so an operator can tell two very different situations apart
// without reading source:
//
//   - "these files genuinely hold vectors from different embedding
//     spaces", which is a real data condition and is resolved by
//     convergence (or by not compacting them together); and
//   - "nobody ever recorded what this file holds", which is a metadata
//     gap and is resolved by running the backfill, after which the same
//     compaction either succeeds or reports the first case honestly.
//
// Before issue #274 the second case could not be reported at all,
// because the aggregation layer fabricated no_embeddings for any file
// missing the column and the compactor was handed a confident claim
// instead of a gap.
//
// Errors carrying it always wrap the underlying cause too, so callers
// that already classify embeddingspace.ErrMismatch or
// embeddingspace.ErrIntegrity keep working unchanged.
var ErrEmbeddingBackfillRequired = errors.New(
	"compaction: an input's embedding space has never been established; a backfill is required")

// unresolvedInput reports whether a state is one nothing has ever
// established: an explicit unknown, or the zero value meaning no claim
// was made at all.
func unresolvedInput(state embeddingspace.FileState) bool {
	return state.State == "" || state.State == embeddingspace.StateUnknown
}

// describeInputs renders every input as "name=state", in Spec.Inputs
// order, so a refusal names the actual files rather than only the states
// that collided.
func describeInputs(names []string, states []embeddingspace.FileState) string {
	described := make([]string, 0, len(states))
	for i, state := range states {
		name := "input " + fmt.Sprint(i)
		if i < len(names) && strings.TrimSpace(names[i]) != "" {
			name = names[i]
		}
		rendered := state.String()
		if state.State == "" {
			rendered = "<no metadata column>"
		}
		described = append(described, name+"="+rendered)
	}
	return strings.Join(described, ", ")
}

// unresolvedNames lists the inputs a backfill would have to visit.
func unresolvedNames(names []string, states []embeddingspace.FileState) []string {
	out := make([]string, 0, len(states))
	for i, state := range states {
		if !unresolvedInput(state) {
			continue
		}
		name := "input " + fmt.Sprint(i)
		if i < len(names) && strings.TrimSpace(names[i]) != "" {
			name = names[i]
		}
		out = append(out, name)
	}
	return out
}

// annotateEmbeddingRefusal turns a bare embedding-space refusal into one
// an operator can act on.
//
// Every refusal is told which file was in which state, because the
// underlying errors describe states only ("has_embeddings:a vs
// has_embeddings:b") and an operator cannot map that back to a file. A
// refusal that involves an unresolved input additionally wraps
// ErrEmbeddingBackfillRequired and names the command to run, so
// "run the backfill" is distinguishable from "these files really do hold
// different spaces".
func annotateEmbeddingRefusal(
	err error, names []string, states []embeddingspace.FileState,
) error {
	if err == nil {
		return nil
	}
	detail := describeInputs(names, states)
	unresolved := unresolvedNames(names, states)
	if len(unresolved) == 0 {
		return fmt.Errorf("%w [inputs: %s]", err, detail)
	}
	return fmt.Errorf("%w: %w; %s has no established embedding space, run %q over this table [inputs: %s]",
		ErrEmbeddingBackfillRequired, err, strings.Join(unresolved, ", "), BackfillCommand, detail)
}

// annotateIntegrityRefusal reports a metadata column that disagrees with
// the file it describes, naming the file.
//
// When the metadata side is the unresolved one the disagreement is a
// stale or missing column rather than a corrupted file, and the backfill
// is exactly the repair: it rewrites the column from the footer, which
// is the authority. That case is called out explicitly so an operator
// does not go looking for file corruption.
func annotateIntegrityRefusal(err error, name string, metadataState embeddingspace.FileState) error {
	if err == nil {
		return nil
	}
	if strings.TrimSpace(name) == "" {
		name = "<unnamed input>"
	}
	if unresolvedInput(metadataState) {
		return fmt.Errorf("%w: %w: input %q has an unresolved metadata column, run %q over this table",
			ErrEmbeddingBackfillRequired, err, name, BackfillCommand)
	}
	return fmt.Errorf("%w [input: %s]", err, name)
}
