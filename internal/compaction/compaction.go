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

// Package compaction is shoal's compaction-stack composer: the "read N
// RFiles → apply the iterator stack → write one RFile" core of Bet 1
// (design doc, "What the compactor actually has to do", component 4).
//
// It is deliberately decoupled from the coordinator. A compaction is a
// pure function of (input RFiles, iterator stack, scope) → output RFile,
// so this package is fully testable offline with synthetic RFiles — no
// coordinator, no ZK, no metadata. cmd/shoal-compactor wires it to the
// CompactionCoordinator job-poll loop; the metadata commit of the output
// file is a separate, manager-side step (see cmd/shoal-compactor).
//
// The stack order mirrors the JVM tserver's SortedKeyValueIterator
// pipeline: a MergingIterator over the per-file RFileSource leaves, then
// the user/system iterator stack built by iterrt.BuildStack on top
// (VersioningIterator → user iterators). The top of the stack is drained
// into an rfile.Writer.
package compaction

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/phrocker/shoal-oss/internal/embeddingspace"
	"github.com/phrocker/shoal-oss/internal/iterrt"
	"github.com/phrocker/shoal-oss/internal/parquetfile"
	"github.com/phrocker/shoal-oss/internal/rfile"
	"github.com/phrocker/shoal-oss/internal/rfile/bcfile"
	"github.com/phrocker/shoal-oss/internal/rfile/bcfile/block"
	"github.com/phrocker/shoal-oss/internal/rfile/wire"
)

// Input is one immutable file feeding a compaction. Bytes is the whole file
// image; Name is a human label used only in error messages (typically
// the metadata file entry, e.g. the tablet-relative path).
//
// The whole-image-in-memory shape matches how shoal already pulls RFiles
// for reads (internal/storage fetches the object, the reader works over
// a bytes.Reader). A streaming variant is a later optimisation; it does
// not change the composer's contract.
type Input struct {
	Name string
	// MetadataEmbedding is the metadata-table state for this file when
	// the caller has one. If present it must agree with the file footer
	// before the input is used, and it then becomes the input's state.
	//
	// The zero FileState means the caller has no metadata column for
	// this file — "nothing recorded", not "recorded as unknown". Absent
	// leaves the footer as the sole authority; an explicit
	// embeddingspace.Unknown() is a recorded claim and is cross-checked
	// like any other. Callers translating a wire payload must preserve
	// that difference, because collapsing absent into unknown makes
	// every file written before the column existed fail its integrity
	// check the first time something compacts it.
	MetadataEmbedding embeddingspace.FileState
	Bytes             []byte
	Format            string
}

// Spec describes a single compaction unit fully: the inputs, the
// iterator stack to apply, the scope, and the output RFile's encoding.
type Spec struct {
	// Inputs are the immutable files to merge, NEWEST FIRST. The
	// MergingIterator sorts cells across them by key; for keys identical
	// down to the timestamp and delete flag (every overwrite written with
	// PutLatest), an earlier input's cells come first, and the output file
	// keeps them in that order so a later read still finds the newest
	// first. Callers without recency information (e.g. files listed from
	// Accumulo metadata, whose servers assign distinct timestamps) may pass
	// any order; ties then follow that order. An empty Inputs list produces
	// an empty (but valid) output RFile.
	Inputs []Input

	// Stack is the iterator stack applied above the merge, bottom-first
	// (Stack[0] sits directly on the MergingIterator). Empty Stack is an
	// identity compaction: every cell passes through untouched, which is
	// the C0/C1 "identity compaction" behaviour.
	Stack []iterrt.IterSpec

	// Scope is the compaction context handed to every iterator's Init.
	// For a real compaction this is ScopeMinc or ScopeMajc; the composer
	// itself does not care which.
	Scope iterrt.IteratorScope

	// FullMajorCompaction is threaded into IteratorEnvironment. True only
	// when the output is the tablet's sole remaining file, which is the
	// only time a delete-aware iterator may drop a tombstone. Maps to the
	// coordinator job's PropagateDeletes flag (inverted): a job that says
	// "do not propagate deletes" is a full major compaction.
	FullMajorCompaction bool

	// Codec is the output RFile's block compression codec ("none", "gz",
	// "snappy"). Empty defaults to "snappy" for good throughput/size balance.
	Codec string

	// BlockSize overrides the output writer's data-block size threshold.
	// Zero uses rfile.DefaultBlockSize.
	BlockSize int

	// AdjacencyEdgeCF, when non-empty, is forwarded to the output
	// writer so the compacted RFile carries a shoal.adjacency out-edge
	// index for cells in this column family. Empty disables it.
	AdjacencyEdgeCF string

	// MaxOutputBytes caps the output RFile image Compact will retain,
	// in bytes. Zero means unlimited.
	//
	// This exists because an input-side budget cannot bound the
	// composer's footprint. Compact holds the whole output in memory,
	// and the output is not bounded by the inputs' on-disk size:
	//
	//   - inputs arrive with their blocks compressed, and Codec "none"
	//     rewrites them uncompressed, so the image can grow by whatever
	//     ratio the input codec achieved;
	//   - a stack may emit more cells than it consumes.
	//     LatentEdgeDiscoveryIterator is the in-tree example: it buffers
	//     vertices and emits link cells that were in no input.
	//
	// Enforcement is at the write boundary: a write that would take the
	// image over the cap is refused before the buffer grows, so the
	// image itself never exceeds it. Checking after the fact would not
	// be equivalent — one cell can be arbitrarily larger than BlockSize,
	// so a single flush can carry an unbounded amount of data.
	//
	// What this does not cover is memory the writer holds outside the
	// image: the pending, uncompressed data block (BlockSize, or one
	// oversized cell) and the in-memory index levels. Size the budget
	// with that headroom in mind.
	MaxOutputBytes int64

	// OutputFormat selects "rfile" (default) or "parquet".
	OutputFormat string

	// TargetEmbeddingSpace is the table's convergence target, taken from
	// the embeddingspace.TableTargetProperty table property. Empty means
	// the table declares no target, so nothing converges.
	TargetEmbeddingSpace string

	// EmbeddingEpoch identifies the migration snapshot this compaction
	// acts for, taken from embeddingspace.JobEpochProperty. Empty means
	// no epoch-tracked migration is driving this compaction.
	//
	// It travels with the target so the two cannot drift apart. A
	// Converger that belongs to a different epoch refuses the attempt,
	// which is what stops two migrations with different targets from
	// rewriting the same files in opposite directions.
	EmbeddingEpoch string

	// Converger, when non-nil, is allowed to re-embed this compaction's
	// cells into TargetEmbeddingSpace. Nil disables convergence
	// entirely, which is what every caller that does not own an
	// embedding provider should pass: the output then carries whatever
	// space the inputs agreed on.
	Converger Converger
}

// Result reports what a Compact call produced.
type Result struct {
	// Output is the written immutable file image.
	Output []byte
	// EntriesWritten is the cell count drained into the output — the
	// number of cells the iterator stack surfaced, which may be less
	// than the sum of input entries (versioning/filtering drops cells).
	EntriesWritten int64
	// EmbeddingSpace is the space the output file is labelled with, and
	// is exactly what was written into the file's footer. A caller
	// publishing this file must record this same value in the metadata
	// entry; recording anything else recreates the metadata/footer
	// disagreement VerifyMetadataMatchesFooter exists to catch.
	EmbeddingSpace embeddingspace.FileState
	// Converged reports whether a Converger actually re-embedded this
	// output into Spec.TargetEmbeddingSpace. False means the output
	// preserves whatever space the inputs agreed on.
	Converged bool
	// EmbeddingEpoch is the migration epoch this output was converged
	// for. It is set only when Converged is true, so a caller can tell
	// "this file was moved by epoch E" from "this file merely happens to
	// be in E's target space", and can discard a result produced for an
	// epoch that has since been superseded.
	EmbeddingEpoch string
}

// Progress is a monotonic snapshot emitted while CompactContext drains the
// iterator stack.
type Progress struct {
	EntriesWritten int64
}

// ErrOutputTooLarge reports that a compaction produced more output than
// Spec.MaxOutputBytes allows. The compaction is abandoned: callers get
// no partial Result, because a truncated RFile is not a compaction.
var ErrOutputTooLarge = errors.New("compaction: output exceeds the configured budget")

// budgetedWriter passes writes through to w until they would take the
// total past max, then refuses. Refusing before the write lands is the
// point: it is what keeps the output image inside the budget no matter
// how large a single block or metadata section is.
//
// A max of zero (or less) disables the budget entirely.
type budgetedWriter struct {
	w       io.Writer
	max     int64
	written int64
}

func (b *budgetedWriter) Write(p []byte) (int, error) {
	if b.max > 0 && b.written+int64(len(p)) > b.max {
		return 0, fmt.Errorf(
			"compaction: writing %d bytes would take the output to %d bytes, over the %d-byte budget: %w",
			len(p), b.written+int64(len(p)), b.max, ErrOutputTooLarge)
	}
	n, err := b.w.Write(p)
	b.written += int64(n)
	return n, err
}

// Compact runs one compaction described by spec and returns the output
// RFile bytes. It is the offline-testable core; it performs no I/O
// beyond the in-memory buffers it is handed.
//
// Pipeline:
//  1. open each Input as an rfile.Reader → wrap as an iterrt.RFileSource
//  2. merge the leaves with iterrt.MergingIterator
//  3. stack iterrt.BuildStack(merge, spec.Stack, env) on top
//  4. Seek the stack to the full range and drain every cell into an
//     rfile.Writer
//
// Cells reach the writer already in non-decreasing Key order because the
// MergingIterator emits them sorted and the stack above it is
// order-preserving (versioning/visibility only drop cells, never reorder).
//
// Compact fails with ErrOutputTooLarge if the output grows past
// spec.MaxOutputBytes.
func Compact(spec Spec) (*Result, error) {
	return CompactContext(context.Background(), spec, nil)
}

// CompactContext is Compact with cooperative cancellation and progress
// observation. Cancellation is checked before source construction, before
// each cell is written, and before the final RFile close.
//
// When the spec carries a convergence target and a Converger,
// CompactContext re-embeds the merged stream into that target space and
// labels the output with it. If the provider refuses, or fails
// mid-flight, the compaction is recomposed once with convergence off, so
// the output preserves the inputs' original space instead of failing the
// compaction or, worse, claiming a space it does not contain.
//
// The one case with no unconverged answer is inputs carrying two
// different identities: no single label describes their merge. That is
// reported as ErrConvergenceRequired, before any work is done, and is
// retryable — see that error's documentation.
func CompactContext(ctx context.Context, spec Spec, observe func(Progress)) (*Result, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	states, err := inputEmbeddingSpaces(spec.Inputs)
	if err != nil {
		return nil, err
	}
	attempt, err := planConvergence(ctx, spec, states)
	if err != nil {
		return nil, err
	}
	if !attempt.active() {
		if err := attempt.verify(); err != nil {
			return nil, err
		}
		return compactOnce(ctx, spec, observe, attempt)
	}
	if err := attempt.verify(); err != nil {
		attempt.attempt.End(ctx, false, 0, err)
		return nil, err
	}

	result, cells, err := compactConverged(ctx, spec, observe, attempt)
	if err == nil {
		attempt.attempt.End(ctx, true, cells, nil)
		return result, nil
	}
	attempt.attempt.End(ctx, false, cells, err)
	if ctxErr := ctx.Err(); ctxErr != nil {
		return nil, ctxErr
	}
	if errors.Is(err, ErrConvergenceAborted) || errors.Is(err, ErrOutputTooLarge) {
		return nil, err
	}
	// Provider failure mid-stream. Recompose from the same in-memory
	// inputs with convergence off; composition is deterministic, so the
	// unconverged output is exactly what a converger-less compaction
	// would have produced, and the file keeps the space it arrived with.
	fallback, fallbackErr := attempt.preserved(err)
	if fallbackErr != nil {
		return nil, fallbackErr
	}
	if err := fallback.verify(); err != nil {
		return nil, err
	}
	return compactOnce(ctx, spec, observe, fallback)
}

func compactConverged(
	ctx context.Context, spec Spec, observe func(Progress), attempt *convergeAttempt,
) (*Result, int64, error) {
	top, closer, err := buildSource(spec)
	if err != nil {
		return nil, 0, err
	}
	defer closer.Close()
	converting := newConvergingIterator(ctx, top, attempt.attempt)
	if err := converting.prime(); err != nil {
		return nil, converting.converted, err
	}
	result, err := drain(ctx, spec, observe, converting, attempt)
	// The conversion error is reported first: a failed conversion stops
	// the stream, which the writer cannot tell apart from a clean end of
	// input, so a nil drain error here would publish a truncated file.
	if convErr := converting.Err(); convErr != nil {
		return nil, converting.converted, convErr
	}
	if err != nil {
		return nil, converting.converted, err
	}
	return result, converting.converted, nil
}

func compactOnce(
	ctx context.Context, spec Spec, observe func(Progress), attempt *convergeAttempt,
) (*Result, error) {
	top, closer, err := buildSource(spec)
	if err != nil {
		return nil, err
	}
	defer closer.Close()
	return drain(ctx, spec, observe, top, attempt)
}

func drain(
	ctx context.Context,
	spec Spec,
	observe func(Progress),
	top iterrt.SortedKeyValueIterator,
	attempt *convergeAttempt,
) (*Result, error) {
	outputEmbedding := attempt.label
	finish := func(output []byte, written int64) *Result {
		result := &Result{
			Output:         output,
			EntriesWritten: written,
			EmbeddingSpace: outputEmbedding,
			Converged:      attempt.active(),
		}
		if result.Converged {
			result.EmbeddingEpoch = attempt.epoch
		}
		return result
	}
	if spec.OutputFormat == "parquet" {
		var buf bytes.Buffer
		written, err := parquetfile.EncodeToWithOptions(
			&budgetedWriter{w: &buf, max: spec.MaxOutputBytes},
			top,
			parquetfile.EncodeOptions{
				Check: ctx.Err,
				Observe: func(written int64) {
					if observe != nil {
						observe(Progress{EntriesWritten: written})
					}
				},
				EmbeddingSpace: outputEmbedding,
			},
		)
		if err != nil {
			return nil, fmt.Errorf("compaction: %w", err)
		}
		return finish(buf.Bytes(), written), nil
	}
	if spec.OutputFormat != "" && spec.OutputFormat != "rfile" {
		return nil, fmt.Errorf("compaction: unknown output format %q", spec.OutputFormat)
	}

	codec := spec.Codec
	if codec == "" {
		codec = block.CodecSnappy
	}
	var buf bytes.Buffer
	w, err := rfile.NewWriter(&budgetedWriter{w: &buf, max: spec.MaxOutputBytes}, rfile.WriterOptions{
		Codec:           codec,
		BlockSize:       spec.BlockSize,
		AdjacencyEdgeCF: spec.AdjacencyEdgeCF,
		EmbeddingSpace:  outputEmbedding,
	})
	if err != nil {
		return nil, fmt.Errorf("compaction: new writer: %w", err)
	}
	var written int64
	for top.HasTop() {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if err := w.Append(top.GetTopKey(), top.GetTopValue()); err != nil {
			return nil, fmt.Errorf("compaction: append cell %d: %w", written, err)
		}
		written++
		if observe != nil {
			observe(Progress{EntriesWritten: written})
		}
		if err := top.Next(); err != nil {
			return nil, fmt.Errorf("compaction: advance after cell %d: %w", written-1, err)
		}
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := w.Close(); err != nil {
		return nil, fmt.Errorf("compaction: close writer: %w", err)
	}
	return finish(buf.Bytes(), written), nil
}

// inputEmbeddingSpaces resolves the authoritative space of every input.
//
// The file footer is the primary source: it is written by whoever
// produced the file and travels with it. The metadata-table entry, when
// the caller supplied one, is a cross-check, and a disagreement is an
// integrity error rather than something to reconcile.
//
// An input whose MetadataEmbedding is the zero FileState carries no
// metadata column at all — the caller had nothing to say, not "the
// caller says unknown". There is then nothing to cross-check against and
// the footer stands alone. That distinction matters: treating an absent
// column as an explicit unknown would make every file written before the
// column existed fail its own integrity check the first time it was
// compacted.
func inputEmbeddingSpaces(inputs []Input) ([]embeddingspace.FileState, error) {
	states := make([]embeddingspace.FileState, 0, len(inputs))
	for _, in := range inputs {
		state, err := inputEmbeddingSpace(in)
		if err != nil {
			return nil, err
		}
		if in.MetadataEmbedding.State != "" {
			if err := embeddingspace.VerifyMetadataMatchesFooter(in.MetadataEmbedding, state); err != nil {
				return nil, annotateIntegrityRefusal(err, in.Name, in.MetadataEmbedding)
			}
			state = in.MetadataEmbedding
		}
		states = append(states, state)
	}
	return states, nil
}

// inputEmbeddingSpace resolves one input's declared space from the file
// itself.
func inputEmbeddingSpace(in Input) (embeddingspace.FileState, error) {
	if len(in.Bytes) == 0 {
		return embeddingspace.FileState{}, fmt.Errorf("compaction: input %q is empty", in.Name)
	}
	format := in.Format
	if format == "" && len(in.Name) >= len(".parquet") && in.Name[len(in.Name)-len(".parquet"):] == ".parquet" {
		format = "parquet"
	}
	if format == "parquet" {
		return parquetfile.ReadEmbeddingSpaceMetadata(bytes.NewReader(in.Bytes), int64(len(in.Bytes)))
	}
	if format != "" && format != "rfile" {
		return embeddingspace.FileState{}, fmt.Errorf("compaction: unknown input format %q for %q", format, in.Name)
	}
	bc, err := bcfile.NewReader(bytes.NewReader(in.Bytes), int64(len(in.Bytes)))
	if err != nil {
		return embeddingspace.FileState{}, fmt.Errorf("compaction: bcfile open %q: %w", in.Name, err)
	}
	r, err := rfile.Open(bc, block.Default())
	if err != nil {
		return embeddingspace.FileState{}, fmt.Errorf("compaction: rfile open %q: %w", in.Name, err)
	}
	defer r.Close()
	return r.EmbeddingSpace(), nil
}

// StreamCells drains the compaction source for spec — the merged,
// delete-aware, fully-iterated cell stream that Compact would feed to the
// writer — invoking emit for each (key, value) in non-decreasing key
// order. It performs no RFile writing, so it is the "second pipeline"
// oc-verify uses to independently re-derive the expected output cells
// from the inputs and compare them against the written RFile.
//
// The key/value passed to emit are owned by the underlying iterators and
// are only valid until the next call; copy anything that must outlive it.
func StreamCells(spec Spec, emit func(k *wire.Key, v []byte) error) error {
	top, closer, err := buildSource(spec)
	if err != nil {
		return err
	}
	defer closer.Close()

	for top.HasTop() {
		if err := emit(top.GetTopKey(), top.GetTopValue()); err != nil {
			return err
		}
		if err := top.Next(); err != nil {
			return fmt.Errorf("compaction: advance: %w", err)
		}
	}
	return nil
}

// buildSource opens spec.Inputs, merges them, wraps the merge in the
// delete-aware source, and stacks spec.Stack on top. The returned
// iterator is seeked to the full range and ready to drain. The returned
// io.Closer releases every input RFile reader; the caller must Close it.
func buildSource(spec Spec) (iterrt.SortedKeyValueIterator, io.Closer, error) {
	leaves := make([]iterrt.SortedKeyValueIterator, 0, len(spec.Inputs))
	closers := make([]io.Closer, 0, len(spec.Inputs))
	closeAll := func() {
		for _, c := range closers {
			_ = c.Close()
		}
	}

	env := iterrt.IteratorEnvironment{
		Scope:               spec.Scope,
		FullMajorCompaction: spec.FullMajorCompaction,
	}

	for _, in := range spec.Inputs {
		src, rdr, err := openInputSource(in, env)
		if err != nil {
			closeAll()
			return nil, nil, err
		}
		closers = append(closers, rdr)
		leaves = append(leaves, src)
	}

	merge := iterrt.NewMergingIterator(leaves...)
	if err := merge.Init(nil, nil, env); err != nil {
		closeAll()
		return nil, nil, fmt.Errorf("compaction: merge init: %w", err)
	}

	// Wrap source in DeletingIterator BEFORE applying user iterators —
	// matches Java's FileCompactor.compactLocalityGroup, which builds
	// the stack as: source → DeletingIterator → user iterators. Without
	// this, any tombstone in the input passes through to user iterators
	// (e.g. LatentEdgeDiscoveryIterator) as a live cell. Symptom on
	// graph_vidx: latentEdge buffers vertices whose Java-side equivalent
	// was already skipped, yielding a ~14% delta in emitted link cells
	// on tablets where vertices have been deleted.
	//
	// The propagateDeletes flag is computed from env.Scope +
	// FullMajorCompaction (see DeletingIterator's Init contract):
	// dropping tombstones is safe only when the output is the tablet's
	// sole file (FullMajorCompaction at ScopeMajc). Everything else
	// preserves them so a later compaction can apply suppression
	// against RFiles this stack didn't see.
	delIter := iterrt.NewDeletingIterator()
	if err := delIter.Init(merge, nil, env); err != nil {
		closeAll()
		return nil, nil, fmt.Errorf("compaction: deleting iter init: %w", err)
	}

	top, err := iterrt.BuildStack(delIter, spec.Stack, env)
	if err != nil {
		closeAll()
		return nil, nil, fmt.Errorf("compaction: build stack: %w", err)
	}
	if err := top.Seek(iterrt.InfiniteRange(), nil, false); err != nil {
		closeAll()
		return nil, nil, fmt.Errorf("compaction: seek: %w", err)
	}

	return top, closerFunc(closeAll), nil
}

// closerFunc adapts a func() to io.Closer.
type closerFunc func()

func (f closerFunc) Close() error { f(); return nil }

// openInputSource opens one Input as an iterrt leaf. The returned closer
// is the rfile.Reader; the caller closes it once the compaction drains.
//
// The RFileSource gets an opener so DeepCopy works — the MergingIterator
// does not DeepCopy its sources today, but a future stack (a parent that
// re-seeks its source) might, and an opener that re-derives a reader
// from the same in-memory image is free to provide.
func openInputSource(in Input, env iterrt.IteratorEnvironment) (iterrt.SortedKeyValueIterator, io.Closer, error) {
	if len(in.Bytes) == 0 {
		return nil, nil, fmt.Errorf("compaction: input %q is empty", in.Name)
	}
	format := in.Format
	if format == "" && len(in.Name) >= len(".parquet") && in.Name[len(in.Name)-len(".parquet"):] == ".parquet" {
		format = "parquet"
	}
	if format == "parquet" {
		cells, err := parquetfile.Decode(in.Bytes)
		if err != nil {
			return nil, nil, fmt.Errorf("compaction: open parquet %q: %w", in.Name, err)
		}
		src := iterrt.NewSliceSource(cells)
		if err := src.Init(nil, nil, env); err != nil {
			return nil, nil, fmt.Errorf("compaction: source init %q: %w", in.Name, err)
		}
		return src, closerFunc(func() {}), nil
	}
	if format != "" && format != "rfile" {
		return nil, nil, fmt.Errorf("compaction: unknown input format %q for %q", format, in.Name)
	}

	open := func() (*rfile.Reader, error) {
		bc, err := bcfile.NewReader(bytes.NewReader(in.Bytes), int64(len(in.Bytes)))
		if err != nil {
			return nil, fmt.Errorf("compaction: bcfile open %q: %w", in.Name, err)
		}
		r, err := rfile.Open(bc, block.Default())
		if err != nil {
			return nil, fmt.Errorf("compaction: rfile open %q: %w", in.Name, err)
		}
		return r, nil
	}

	rdr, err := open()
	if err != nil {
		return nil, nil, err
	}
	src := iterrt.NewRFileSource(rdr, open)
	if err := src.Init(nil, nil, env); err != nil {
		_ = rdr.Close()
		return nil, nil, fmt.Errorf("compaction: source init %q: %w", in.Name, err)
	}
	return src, rdr, nil
}

// ErrNoInputs is returned by callers that treat a zero-input spec as a
// programming error. Compact itself accepts it (and produces an empty
// RFile); cmd/shoal-compactor rejects it before calling Compact because
// the coordinator should never assign a job with no files.
var ErrNoInputs = errors.New("compaction: spec has no input files")
