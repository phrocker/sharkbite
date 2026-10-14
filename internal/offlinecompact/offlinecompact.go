// Licensed to the Apache Software Foundation (ASF) under one
// or more contributor license agreements.  See the NOTICE file
// distributed with this work for additional information
// regarding copyright ownership.  The ASF licenses this file
// to you under the Apache License, Version 2.0 (the
// "License"); you may not use this file except in compliance
// with the License.  You may obtain a copy of the License at
//
//   https://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing,
// software distributed under the License is distributed on an
// "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY
// KIND, either express or implied.  See the License for the
// specific language governing permissions and limitations
// under the License.

// Package offlinecompact orchestrates a full major compaction of the
// tablets of an OFFLINE Accumulo table, from a standalone process — no
// tserver, manager, or compaction coordinator in the loop. See
// docs/offline-compaction-design.md for the safety model.
//
// This package owns exactly one phase of that design: the *orchestrator*
// (todo oc-orchestrator). Given a table, it enumerates the table's
// tablets, resolves each tablet's major-compaction iterator stack,
// fetches the input RFiles, runs the pure compaction composer
// (internal/compaction) as a full major compaction, and writes the
// output RFile into the tablet's directory under an Accumulo-style
// `A<base>.rf` name. It returns a Plan describing, per tablet, the input
// files it consumed and the output file it produced.
//
// It deliberately does NOT fence the OFFLINE state or commit the result
// to accumulo.metadata — those are the separate oc-commit phase, which
// consumes this Plan. Nor does it verify the output (oc-verify). Keeping
// the orchestrator free of ZK and metadata writes makes it a pure
// function of (tablet enumeration, stack resolution, RFile bytes) →
// (output RFile bytes + Plan), fully unit-testable with in-memory fakes.
//
// Safety note: writing an output RFile is always safe and reversible on
// its own — the file is unreferenced until oc-commit inserts its metadata
// ref, so a crash (or a dry-run that never commits) leaves an orphan that
// Accumulo's GC reclaims. The dangerous step is the metadata mutation,
// which lives in oc-commit and is gated behind the OFFLINE fence.
package offlinecompact

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"strings"

	"github.com/google/uuid"

	"github.com/phrocker/shoal-oss/internal/compaction"
	"github.com/phrocker/shoal-oss/internal/iterrt"
	"github.com/phrocker/shoal-oss/internal/metadata"
	"github.com/phrocker/shoal-oss/internal/shadow/itercfg"
)

// TabletEnumerator yields the tablets of a table. *metadata.Walker
// satisfies this directly via LocateTable.
type TabletEnumerator interface {
	LocateTable(ctx context.Context, tableID string) ([]metadata.TabletInfo, error)
}

// MajcStackResolver resolves a table's iterator stack at a scope.
// *itercfg.Resolver satisfies this directly.
type MajcStackResolver interface {
	Resolve(ctx context.Context, tableID string, scope iterrt.IteratorScope) (*itercfg.ResolvedStack, error)
}

// RFileStore reads and writes RFile images by path. Read fetches an input
// file's whole image; Write publishes the compacted output durably (the
// implementation is responsible for fsync/flush before returning). See
// BackendStore for the storage.Backend-backed implementation.
type RFileStore interface {
	Read(ctx context.Context, path string) ([]byte, error)
	Write(ctx context.Context, path string, data []byte) error
}

// Deps bundles the injected collaborators. All three are required.
type Deps struct {
	Tablets TabletEnumerator
	Stacks  MajcStackResolver
	Files   RFileStore
}

// Options tunes the compaction output and naming.
type Options struct {
	// Codec is the output RFile block codec ("none", "gz", "snappy").
	// Empty defers to the compaction composer default (snappy).
	Codec string
	// BlockSize overrides the output data-block threshold. Zero uses the
	// rfile default.
	BlockSize int
	// AdjacencyEdgeCF, when set, forwards to the output writer's
	// adjacency out-edge index (see compaction.Spec.AdjacencyEdgeCF).
	AdjacencyEdgeCF string
	// NewFileName generates the leaf name for an output RFile (e.g.
	// "A0f3c1a2b4d6.rf"). nil uses a UUID-based default. Injectable so
	// tests are deterministic.
	NewFileName func() string
	// Verify, when true, runs the §5 verification on every compacted
	// tablet (self-consistency + shadow shoal-only tiers) and aborts the
	// run if any tablet's output fails to verify.
	Verify bool
	// Logger for per-tablet progress. nil uses slog.Default().
	Logger *slog.Logger
}

// TabletResult is the outcome of compacting one tablet.
type TabletResult struct {
	// Tablet is the tablet that was compacted (identity = TableID +
	// PrevRow + EndRow).
	Tablet metadata.TabletInfo
	// Stack is the resolved majc iterator stack that was applied.
	Stack []iterrt.IterSpec
	// Inputs are the file: entries consumed. oc-commit dereferences
	// exactly these (RawQualifier preserved for byte-exact deletion).
	Inputs []metadata.FileEntry
	// OutputPath is the absolute path of the written output RFile.
	OutputPath string
	// OutputSize is the output image byte length.
	OutputSize int64
	// EntriesWritten is the cell count in the output.
	EntriesWritten int64
	// Verify is the §5 verification report for this tablet's output.
	// Populated only when Options.Verify is set; nil otherwise.
	Verify *VerifyReport
}

// Plan is the full result of an orchestration pass over one table. It is
// the hand-off to oc-commit: Results carries, per tablet, the exact
// (inputs to dereference, output to reference) delta for the metadata
// mutation. NoOp lists tablets that were skipped because there was
// nothing to compact (see shouldCompact).
type Plan struct {
	TableID string
	Results []TabletResult
	NoOp    []metadata.TabletInfo
}

// Run orchestrates a full-major offline compaction over every tablet of
// tableID (optionally pre-filter the enumerator's output with
// SelectTablets before calling, or filter the returned Plan). It is
// all-or-nothing: any unported iterator, fetch failure, or compaction
// error aborts the whole run so the operator can fix the cause before
// committing anything.
//
// Run performs no metadata writes and no OFFLINE fencing — it only reads
// inputs and writes output RFiles. Callers (oc-cli) establish the fence
// and pass the returned Plan to oc-commit.
func Run(ctx context.Context, tableID string, deps Deps, opts Options) (*Plan, error) {
	if err := validateDeps(deps); err != nil {
		return nil, err
	}
	if tableID == "" {
		return nil, fmt.Errorf("offlinecompact: tableID is required")
	}
	logger := opts.Logger
	if logger == nil {
		logger = slog.Default()
	}
	newName := opts.NewFileName
	if newName == nil {
		newName = defaultFileName
	}

	tablets, err := deps.Tablets.LocateTable(ctx, tableID)
	if err != nil {
		return nil, fmt.Errorf("offlinecompact: enumerate tablets of %s: %w", tableID, err)
	}

	// Resolve the majc iterator stack once per run: Resolve is scoped to
	// (tableID, scope) and identical for every tablet, so resolving per
	// tablet would add repeated ZK/config reads on large tables. Fail
	// early if any class is unported — never silently drop an iterator,
	// which would corrupt compaction semantics (closing that gap is the
	// iterator-forge track, docs/iterator-forge-design.md).
	resolved, err := deps.Stacks.Resolve(ctx, tableID, iterrt.ScopeMajc)
	if err != nil {
		return nil, fmt.Errorf("offlinecompact: resolve majc stack for %s: %w", tableID, err)
	}
	if len(resolved.Skipped) > 0 {
		return nil, unportedError(tableID, resolved.Skipped)
	}

	plan := &Plan{TableID: tableID}
	for _, tablet := range tablets {
		res, skipped, err := compactTablet(ctx, tableID, tablet, resolved.Stack, deps, opts, newName, logger)
		if err != nil {
			return nil, err
		}
		if skipped {
			plan.NoOp = append(plan.NoOp, tablet)
			continue
		}
		plan.Results = append(plan.Results, *res)
	}
	logger.Info("offline compaction plan complete",
		slog.String("table", tableID),
		slog.Int("compacted_tablets", len(plan.Results)),
		slog.Int("noop_tablets", len(plan.NoOp)),
	)
	return plan, nil
}

// compactTablet resolves + applies the majc stack for one tablet. The
// compactTablet applies the (already-resolved) majc stack to one tablet.
// The bool return is true when the tablet was skipped as a no-op.
func compactTablet(ctx context.Context, tableID string, tablet metadata.TabletInfo, stack []iterrt.IterSpec, deps Deps, opts Options, newName func() string, logger *slog.Logger) (*TabletResult, bool, error) {
	if !shouldCompact(tablet.Files, stack) {
		logger.Debug("skipping tablet (nothing to compact)",
			slog.String("table", tableID),
			slog.String("end_row", metadata.PrintableBytes(tablet.EndRow)),
			slog.Int("files", len(tablet.Files)),
			slog.Int("iterators", len(stack)),
		)
		return nil, true, nil
	}

	inputs := make([]compaction.Input, 0, len(tablet.Files))
	for _, f := range tablet.Files {
		b, err := deps.Files.Read(ctx, f.Path)
		if err != nil {
			return nil, false, fmt.Errorf("offlinecompact: read input %s (tablet %s): %w",
				f.Path, metadata.PrintableBytes(tablet.EndRow), err)
		}
		inputs = append(inputs, compaction.Input{Name: f.Path, Bytes: b})
	}

	// One spec drives both the compaction and (if enabled) the
	// verification's independent re-derivation, so they can never drift.
	spec := compaction.Spec{
		Inputs:              inputs,
		Stack:               stack,
		Scope:               iterrt.ScopeMajc,
		FullMajorCompaction: true,
		Codec:               opts.Codec,
		BlockSize:           opts.BlockSize,
		AdjacencyEdgeCF:     opts.AdjacencyEdgeCF,
	}
	result, err := compaction.Compact(spec)
	if err != nil {
		return nil, false, fmt.Errorf("offlinecompact: compact tablet %s of %s: %w",
			metadata.PrintableBytes(tablet.EndRow), tableID, err)
	}

	outPath, err := outputPath(tablet.Files, newName())
	if err != nil {
		return nil, false, fmt.Errorf("offlinecompact: derive output path (tablet %s): %w",
			metadata.PrintableBytes(tablet.EndRow), err)
	}
	if err := deps.Files.Write(ctx, outPath, result.Output); err != nil {
		return nil, false, fmt.Errorf("offlinecompact: write output %s: %w", outPath, err)
	}

	var vreport *VerifyReport
	if opts.Verify {
		vreport, err = VerifyTablet(spec, result.Output, result.EntriesWritten)
		if err != nil {
			return nil, false, fmt.Errorf("offlinecompact: verify tablet %s of %s: %w",
				metadata.PrintableBytes(tablet.EndRow), tableID, err)
		}
		if !vreport.OK() {
			return nil, false, fmt.Errorf("offlinecompact: verification failed for tablet %s of %s: %s",
				metadata.PrintableBytes(tablet.EndRow), tableID, verifyFailureReason(vreport))
		}
		logger.Debug("tablet verified",
			slog.String("table", tableID),
			slog.String("end_row", metadata.PrintableBytes(tablet.EndRow)),
			slog.Int64("cells_compared", vreport.CellsCompared),
		)
	}

	logger.Info("tablet compacted",
		slog.String("table", tableID),
		slog.String("end_row", metadata.PrintableBytes(tablet.EndRow)),
		slog.Int("inputs", len(tablet.Files)),
		slog.String("output", outPath),
		slog.Int64("output_bytes", int64(len(result.Output))),
		slog.Int64("entries", result.EntriesWritten),
	)

	return &TabletResult{
		Tablet:         tablet,
		Stack:          stack,
		Inputs:         tablet.Files,
		OutputPath:     outPath,
		OutputSize:     int64(len(result.Output)),
		EntriesWritten: result.EntriesWritten,
		Verify:         vreport,
	}, false, nil
}

// verifyFailureReason renders the most actionable reason a VerifyReport
// failed its gate, for the abort error.
func verifyFailureReason(r *VerifyReport) string {
	if r == nil {
		return "nil report"
	}
	if r.Mismatch != nil {
		return "self-consistency: " + r.Mismatch.String()
	}
	if !r.SelfConsistent {
		return "self-consistency failed"
	}
	if r.Shadow != nil && r.Shadow.T1.Attempted && !r.Shadow.T1.Passed {
		return "java cross-read (T1) rejected output: " + r.Shadow.T1.Error
	}
	return "unknown"
}

// shouldCompact decides whether a tablet is worth compacting. A tablet
// with no files is a no-op. A single-file tablet with no iterator stack
// is also a no-op — rewriting one file byte-for-byte through an identity
// stack gains nothing. Any tablet with ≥2 files, or ≥1 file plus a
// non-empty stack (which may transform/drop cells), is compacted.
func shouldCompact(files []metadata.FileEntry, stack []iterrt.IterSpec) bool {
	switch {
	case len(files) == 0:
		return false
	case len(files) == 1 && len(stack) == 0:
		return false
	default:
		return true
	}
}

// outputPath places the new output leaf name in the same directory as the
// tablet's existing files. All of a tablet's files share the tablet
// directory, so any input's parent dir is the correct destination. URI
// schemes (hdfs://, file://, gs://, plain paths) are preserved because we
// split on the last '/'.
func outputPath(files []metadata.FileEntry, leaf string) (string, error) {
	if len(files) == 0 {
		return "", fmt.Errorf("no input files to derive tablet directory from")
	}
	ref := files[0].Path
	i := strings.LastIndexByte(ref, '/')
	if i < 0 {
		return "", fmt.Errorf("input path %q has no directory component", ref)
	}
	return ref[:i+1] + leaf, nil
}

// defaultFileName generates an Accumulo-style full-major output name:
// 'A' + a collision-free hex base + ".rf". Accumulo assigns semantics to
// the leading letter (A = full major compaction) but not to the base, so
// a UUID-derived base is a valid, unique file name.
func defaultFileName() string {
	return "A" + strings.ReplaceAll(uuid.NewString(), "-", "") + ".rf"
}

// SelectTablets returns the tablets whose (PrevRow, EndRow] range
// intersects the inclusive [start, end] selection. A nil start or end is
// unbounded on that side; nil PrevRow is -inf and nil EndRow is +inf.
// Selection is at tablet granularity — a partially-overlapped tablet is
// included whole (offline compaction never rewrites a sub-tablet slice).
func SelectTablets(tablets []metadata.TabletInfo, start, end []byte) []metadata.TabletInfo {
	if start == nil && end == nil {
		return tablets
	}
	out := make([]metadata.TabletInfo, 0, len(tablets))
	for _, t := range tablets {
		if tabletIntersects(t, start, end) {
			out = append(out, t)
		}
	}
	return out
}

func tabletIntersects(t metadata.TabletInfo, start, end []byte) bool {
	// Entirely below start: even the tablet's largest row (EndRow,
	// inclusive) is < start.
	if start != nil && t.EndRow != nil && bytes.Compare(t.EndRow, start) < 0 {
		return false
	}
	// Entirely above end: every tablet row is > PrevRow, and PrevRow is
	// already >= end (end inclusive), so all rows fall past end.
	if end != nil && t.PrevRow != nil && bytes.Compare(t.PrevRow, end) >= 0 {
		return false
	}
	return true
}

func unportedError(tableID string, skipped []itercfg.SkippedIter) error {
	names := make([]string, 0, len(skipped))
	for _, s := range skipped {
		names = append(names, fmt.Sprintf("%s(%s)", s.Name, s.Class))
	}
	return fmt.Errorf("offlinecompact: table %s has %d unported majc iterator(s): %s — "+
		"port them via the iterator forge before offline-compacting (see docs/iterator-forge-design.md)",
		tableID, len(skipped), strings.Join(names, ", "))
}

func validateDeps(deps Deps) error {
	switch {
	case deps.Tablets == nil:
		return fmt.Errorf("offlinecompact: Deps.Tablets is required")
	case deps.Stacks == nil:
		return fmt.Errorf("offlinecompact: Deps.Stacks is required")
	case deps.Files == nil:
		return fmt.Errorf("offlinecompact: Deps.Files is required")
	}
	return nil
}
