// Licensed to the Apache Software Foundation (ASF) under one
// or more contributor license agreements. See the NOTICE file distributed
// with this work for additional information regarding copyright ownership.
// The ASF licenses this file to you under the Apache License, Version 2.0.

// Package embedbackfill resolves tablet files whose embedding-space
// state was never recorded.
//
// # Why this exists
//
// Issue #274 stopped the metadata layers fabricating no_embeddings for a
// file that carries no file.embedding column. Absence of the column now
// reads as unknown, which is the honest answer and the fail-closed one.
// It is also, for an existing cluster, a large number of files that used
// to carry a (false) definite label and now carry none.
//
// The backfill is the migration path. It walks the files, establishes
// each one's real state, and writes the explicit column so unknown
// resolves to something definite.
//
// # What counts as truth
//
// The RFile (or Parquet) footer meta block written by whoever produced
// the file — embeddingspace.RFileMetaBlockName. It travels with the
// file, it was written by the producer, and it cannot be desynchronised
// from the bytes it describes. Nothing else is consulted, and in
// particular the backfill never inspects cell values to guess whether
// something looks like a vector: a guess written into durable metadata
// is exactly the failure this whole change set exists to remove.
//
// A file whose footer does not establish a known state — the meta block
// is absent, or present and explicitly unknown — therefore cannot be
// resolved from metadata alone. Those files are left unknown and
// reported individually, with the reason, so an operator knows precisely
// which files need attention and why. Marking them no_embeddings to make
// the report look clean would reintroduce the bug.
//
// # Safety
//
// Every write is conditional on the file entry still being present and
// unchanged, and on the embedding column still holding exactly what it
// held when the file was examined — absent, or an explicit unknown. A
// file that was compacted away mid-run is reported as raced, not
// written. A file that already carries a definite column is left alone.
// Re-running the backfill over an already-backfilled table therefore
// writes nothing and reports every file as already labelled, which is
// what makes it safe to run repeatedly and safe to interrupt.
package embedbackfill

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/phrocker/shoal-oss/internal/embeddingspace"
)

// ReasonUnestablishedFooter is reported for a file whose own footer does
// not establish a known state — the embedding-space meta block is
// absent, or it is present and explicitly encodes unknown. It is the one
// case the backfill deliberately refuses to resolve, because there is
// nothing to copy and inventing a value is the bug this whole change set
// removes.
const ReasonUnestablishedFooter = "the file's footer does not establish a known embedding state, " +
	"so it cannot be resolved from metadata"

// ReasonUnverifiableNoEmbeddings is reported for a file whose footer says
// no_embeddings while the run has not been told to trust that claim.
//
// The minor-compaction path fixed by issue #274 fabricated no_embeddings
// and stamped it into the footer, so such a footer may be that
// fabrication rather than an observation, and copying it into the column
// would make the bug's own output durable while reporting the file
// migrated. See Config.TrustNoEmbeddings.
const ReasonUnverifiableNoEmbeddings = "the footer says no_embeddings, but a footer written before " +
	"issue #274 may have fabricated it; re-run with --trust-no-embeddings to accept it"

// ReasonUnverifiableNoEmbeddingsColumn is reported for a file that
// already carries a no_embeddings file.embedding column.
//
// The same pre-#274 commit path wrote that column, so it is no more
// established than the footer is, and the backfill cannot repair it —
// the footer would only supply the same claim, and the CAS refuses to
// replace an established column regardless. It is reported so an
// operator knows the file exists and can decide, rather than being
// counted as migrated. See Config.TrustNoEmbeddings.
const ReasonUnverifiableNoEmbeddingsColumn = "the file.embedding column says no_embeddings, but a " +
	"column written before issue #274 may have fabricated it; re-run with --trust-no-embeddings to accept it"

// File is one metadata file entry the backfill may have to label.
type File struct {
	// TableID and the tablet bounds identify the metadata row the
	// column is written into.
	TableID    string
	PrevEndRow []byte
	EndRow     []byte

	// Entry is the metadata file: column qualifier as a string — the
	// name an operator sees and the name a compaction job uses.
	Entry string

	// Path is the storage path the footer is read from.
	Path string

	// Qualifier is the raw file: column qualifier.
	Qualifier []byte

	// Value is the raw DataFileValue currently in metadata. It is the
	// CAS precondition: if the entry changed, the file this backfill
	// examined is not the file it would be labelling.
	Value []byte

	// Metadata is the file's current embedding-space state as the
	// metadata layer reports it. Anything Known() is left alone.
	Metadata embeddingspace.FileState

	// ExistingColumn is the raw file.embedding column bytes, nil when
	// the column is absent. Metadata cannot express that difference —
	// an absent column and one explicitly encoding unknown both decode
	// to the same state — and a conditional write has to.
	ExistingColumn []byte
}

// Files enumerates the candidate file entries.
type Files interface {
	List(ctx context.Context) ([]File, error)
}

// Footers resolves a file's authoritative embedding-space state from the
// file itself. It must return embeddingspace.Unknown() — not an error —
// when the file simply carries no embedding-space meta block, because
// that is a normal state for a file written before the block existed.
type Footers interface {
	FooterState(ctx context.Context, path string) (embeddingspace.FileState, error)
}

// Columns writes one explicit file.embedding column.
//
// It reports applied=false, with a nil error, when the conditional write
// was rejected because the file entry changed underneath the run. That
// is not a failure: the file is simply no longer the one that was
// examined, and a later run will pick up whatever replaced it.
type Columns interface {
	Write(ctx context.Context, file File, state embeddingspace.FileState) (applied bool, err error)
}

// Config is one backfill run.
type Config struct {
	Files   Files
	Footers Footers

	// Columns may be nil only when DryRun is set.
	Columns Columns

	// DryRun resolves and reports without writing anything. It is the
	// safe way to size a migration before committing to it.
	DryRun bool

	// TrustNoEmbeddings is an explicit operator assertion that a
	// no_embeddings claim about this table means what it says, whether
	// it appears in a footer or in an existing file.embedding column.
	//
	// It exists because the paths fixed by issue #274 fabricated
	// no_embeddings and wrote it into both places, so for files produced
	// by that code neither is independent evidence — both are the same
	// unfounded claim. Trusting them by default would let the backfill
	// copy the bug's output into durable metadata, and report a table as
	// migrated while convergence keeps trusting a false claim.
	//
	// An operator who knows the table's ingest pipeline never emitted
	// vectors can assert that here, exactly as they can with
	// mincauthority.Config.DefaultEmbedding. Without it those files are
	// reported unresolvable, by entry and path, so the decision is made
	// deliberately and on a named set of files.
	//
	// has_embeddings is trusted unconditionally and is unaffected: no
	// version of the writer ever invented an identity.
	TrustNoEmbeddings bool
}

// Unresolved is one file the backfill could not establish a state for.
type Unresolved struct {
	Entry  string
	Path   string
	Reason string
}

// Summary is what one run did. Every file in Files.List is accounted for
// in exactly one of the counters or in Unresolved.
type Summary struct {
	// DryRun repeats the run's mode so a caller printing a Summary
	// cannot claim writes that did not happen.
	DryRun bool

	// Scanned is every file the run considered.
	Scanned int

	// AlreadyLabelled carried a definite column before the run. On a
	// second run over the same table this is every file that the first
	// run resolved, which is what idempotence looks like from here.
	AlreadyLabelled int

	// Resolved had their state established from the footer and the
	// column written (or, under DryRun, would have been).
	Resolved int

	// Raced were resolvable but their metadata entry changed before the
	// conditional write landed. Re-running picks them up. They are
	// listed, not just counted: the run is reported as incomplete
	// because of them, and an operator told a run is incomplete needs
	// to be told which files.
	Raced []Unresolved

	// Unresolved could not be established from metadata alone, sorted by
	// entry. These are the files that need an operator.
	Unresolved []Unresolved
}

// Complete reports whether the run left nothing outstanding.
//
// A dry run that would have written columns is not complete: nothing was
// made durable, so the table still refuses compaction. Reporting
// otherwise would let a script take the CLI's default dry-run mode as
// evidence that a table needs no migration.
func (s Summary) Complete() bool {
	if len(s.Unresolved) != 0 || len(s.Raced) != 0 {
		return false
	}
	return !s.DryRun || s.Resolved == 0
}

// Run executes one backfill pass.
//
// It does not stop at the first file it cannot handle. A single
// unreadable file must not strand a whole table's migration, and the
// operator needs the complete list of what is outstanding, not the first
// entry of it. Only a failure to enumerate the files at all — where
// continuing would silently under-report — aborts the run.
func Run(ctx context.Context, cfg Config) (Summary, error) {
	if cfg.Files == nil || cfg.Footers == nil {
		return Summary{}, fmt.Errorf("embedbackfill: Files and Footers are required")
	}
	if cfg.Columns == nil && !cfg.DryRun {
		return Summary{}, fmt.Errorf("embedbackfill: Columns is required unless DryRun is set")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	files, err := cfg.Files.List(ctx)
	if err != nil {
		return Summary{}, fmt.Errorf("embedbackfill: enumerate files: %w", err)
	}

	summary := Summary{DryRun: cfg.DryRun, Scanned: len(files)}
	for _, file := range files {
		if err := ctx.Err(); err != nil {
			return summary, err
		}
		if file.Metadata.Known() {
			if file.Metadata == embeddingspace.NoEmbeddings() && !cfg.TrustNoEmbeddings {
				// The pre-#274 commit path fabricated no_embeddings into
				// the file.embedding column as well as the footer, so an
				// existing no_embeddings column is not necessarily
				// established evidence either. Counting it as already
				// labelled would let the migration report complete while
				// convergence keeps trusting the false claim.
				//
				// The backfill cannot repair it: the footer came from
				// the same writer, and the CAS refuses to replace an
				// established column in any case. So it is reported, by
				// name, for an operator to decide.
				summary.Unresolved = append(summary.Unresolved, Unresolved{
					Entry: file.Entry, Path: file.Path, Reason: ReasonUnverifiableNoEmbeddingsColumn,
				})
				continue
			}
			summary.AlreadyLabelled++
			continue
		}
		state, err := cfg.Footers.FooterState(ctx, file.Path)
		if err != nil {
			if ctxErr := ctx.Err(); ctxErr != nil {
				// The per-file tolerance exists so one bad file cannot
				// strand a table. A cancelled context is not a bad
				// file: the pass is being torn down and the remaining
				// files were never examined. Recording it as this
				// file's problem would, on the last file, let Run
				// return a nil error and a summary that reads like a
				// completed scan.
				return summary, ctxErr
			}
			summary.Unresolved = append(summary.Unresolved, Unresolved{
				Entry: file.Entry, Path: file.Path,
				Reason: "read the file footer: " + err.Error(),
			})
			continue
		}
		if !state.Known() {
			summary.Unresolved = append(summary.Unresolved, Unresolved{
				Entry: file.Entry, Path: file.Path, Reason: ReasonUnestablishedFooter,
			})
			continue
		}
		if state == embeddingspace.NoEmbeddings() && !cfg.TrustNoEmbeddings {
			// The minor-compaction path this issue fixes fabricated
			// no_embeddings and stamped it into the footer, so on a file
			// written before the fix a no_embeddings footer may be that
			// fabrication rather than an observation. Copying it into
			// the column would launder the bug's own output into durable
			// metadata and report the file as migrated, which is the
			// exact substitution of absence for evidence this whole
			// change exists to stop.
			//
			// has_embeddings was never fabricated, so it is trusted
			// unconditionally: no version of the writer invented an
			// identity.
			summary.Unresolved = append(summary.Unresolved, Unresolved{
				Entry: file.Entry, Path: file.Path, Reason: ReasonUnverifiableNoEmbeddings,
			})
			continue
		}
		if cfg.DryRun {
			summary.Resolved++
			continue
		}
		applied, err := cfg.Columns.Write(ctx, file, state)
		if err != nil {
			if ctxErr := ctx.Err(); ctxErr != nil {
				// Same reasoning as the footer read, and it matters
				// more here: a write that was cut off may or may not
				// have landed, so reporting it as an ordinary
				// recoverable failure understates what the operator
				// needs to know before re-running.
				return summary, ctxErr
			}
			summary.Unresolved = append(summary.Unresolved, Unresolved{
				Entry: file.Entry, Path: file.Path,
				Reason: "write the file.embedding column: " + err.Error(),
			})
			continue
		}
		if !applied {
			summary.Raced = append(summary.Raced, Unresolved{
				Entry: file.Entry, Path: file.Path,
				Reason: "the metadata entry changed before the write landed; re-run to pick it up",
			})
			continue
		}
		summary.Resolved++
	}
	sortUnresolved(summary.Raced)
	sortUnresolved(summary.Unresolved)
	return summary, nil
}

func sortUnresolved(items []Unresolved) {
	sort.Slice(items, func(i, j int) bool {
		if items[i].Entry != items[j].Entry {
			return items[i].Entry < items[j].Entry
		}
		return items[i].Path < items[j].Path
	})
}

// Report renders a summary for an operator, one line per counter and one
// line per outstanding file.
func Report(summary Summary) string {
	var b strings.Builder
	if summary.DryRun {
		b.WriteString("mode: dry-run (no columns written)\n")
	} else {
		b.WriteString("mode: apply\n")
	}
	fmt.Fprintf(&b, "scanned: %d\n", summary.Scanned)
	fmt.Fprintf(&b, "already labelled: %d\n", summary.AlreadyLabelled)
	fmt.Fprintf(&b, "resolved: %d\n", summary.Resolved)
	fmt.Fprintf(&b, "raced (retry): %d\n", len(summary.Raced))
	for _, item := range summary.Raced {
		fmt.Fprintf(&b, "- %s (%s): %s\n", item.Entry, item.Path, item.Reason)
	}
	fmt.Fprintf(&b, "unresolvable: %d\n", len(summary.Unresolved))
	for _, item := range summary.Unresolved {
		fmt.Fprintf(&b, "- %s (%s): %s\n", item.Entry, item.Path, item.Reason)
	}
	return b.String()
}
