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

// Package compactjob translates a CompactionCoordinator assignment
// (tabletserver.TExternalCompactionJob) into the compaction shoal would
// run, or refuses it with a structured, operator-legible reason.
//
// It is the layer between cmd/shoal-compactor's poll loop (which speaks
// Thrift to the manager) and internal/compaction (the pure "read N
// RFiles → apply the iterator stack → write one RFile" composer). It
// performs no I/O: Translate is a pure function of the job plus the
// caller's limits, so every field mapping and every refusal is unit
// testable without a coordinator.
//
// # Why refusal is a first-class result
//
// A compaction that runs with a *slightly* different stack than Java
// would have used silently corrupts a tablet: the output replaces its
// inputs, so a dropped filter or an unhonored fence is not recoverable
// from the surviving files. The safe failure mode is therefore always
// "refuse the job, let the coordinator reschedule it onto a Java
// compactor" — Accumulo treats a failed external compaction as a normal,
// retryable event. Every construct shoal cannot reproduce cell-for-cell
// is refused here, before a single input byte is read, and the reason
// travels back to the manager as the compactionFailed exception-class
// name (see the Class* constants).
//
// # What Translate deliberately cannot see
//
// The job carries only the table properties the compaction *overrides*
// (job.overrides, populated by a CompactionConfigurer). The table's own
// configuration — locality groups, samplers, summarizers, bloom filters,
// crypto — is not in the job; a compactor reads it from ZooKeeper.
// shoal-compactor does not read it yet, so Translate gates only what the
// job itself declares. Wiring the table config in (and refusing tables
// whose output shoal cannot reproduce, e.g. multi-locality-group or
// sampled tables) is a precondition for actually executing a job, and is
// tracked on the umbrella issue rather than assumed here.
package compactjob

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"net/netip"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf16"
	"unicode/utf8"

	"github.com/phrocker/shoal-oss/internal/compaction"
	"github.com/phrocker/shoal-oss/internal/embeddingspace"
	"github.com/phrocker/shoal-oss/internal/iterrt"
	"github.com/phrocker/shoal-oss/internal/metadata"
	"github.com/phrocker/shoal-oss/internal/rfile/bcfile/block"
	"github.com/phrocker/shoal-oss/internal/shadow/itercfg"
	"github.com/phrocker/shoal-oss/internal/thrift/gen/data"
	"github.com/phrocker/shoal-oss/internal/thrift/gen/manager"
	"github.com/phrocker/shoal-oss/internal/thrift/gen/tabletserver"
)

// ECIDPrefix is the prefix Accumulo's ExternalCompactionId carries. It
// is the literal PREFIX constant in
// core/src/main/java/org/apache/accumulo/core/metadata/schema/ExternalCompactionId.java,
// and it is a hyphen, not a colon — an unrelated javadoc in that file
// still says "ECID:", but ExternalCompactionId.of rejects anything that
// does not start with "ECID-", so a compactor that invents the colon
// form has every poll rejected before a job can be assigned.
// Jobs whose id is not in this form did not come from a coordinator this
// compactor can talk to.
const ECIDPrefix = "ECID-"

// Sentinel exception-class names reported to the coordinator through
// compactionFailed(..., exceptionClassName, ...). Java only logs and
// stores this string, so the vocabulary is ours; keeping it stable and
// specific is what lets an operator tell "shoal cannot do this yet" from
// "this job is broken" in the manager log without correlating compactor
// logs by ecid.
const (
	// ClassMalformedJob: the assignment itself is not internally
	// consistent (missing extent, no input files, duplicate inputs,
	// output colliding with an input, ...). Nothing about shoal's
	// capabilities is implied — a Java compactor would reject it too.
	ClassMalformedJob = "org.apache.accumulo.shoal.MalformedCompactionJob"

	// ClassRangedInputFile: an input is a *fenced* StoredTabletFile
	// (non-empty startRow/endRow in the metadata entry). Reading it whole
	// would pull in cells that belong to another tablet, so the job is
	// refused until ranged reads land.
	ClassRangedInputFile = "org.apache.accumulo.shoal.RangedInputFileUnsupported"

	// ClassUnsupportedIterator: the job's iterator stack references a
	// class shoal has not ported, a system iterator shoal composes
	// itself, or options the ported iterator rejects.
	ClassUnsupportedIterator = "org.apache.accumulo.shoal.UnsupportedIterator"

	// ClassUnsupportedProperty: the table is configured in a way shoal
	// cannot honor when writing the output — either an override the job
	// carries, or (via the output file's extension) a table.file.type
	// that is not RFile.
	ClassUnsupportedProperty = "org.apache.accumulo.shoal.UnsupportedTableProperty"

	// ClassUnsupportedCrypto: job.overrides configures on-disk
	// encryption. shoal's RFile writer emits cleartext blocks, so an
	// encrypted table's output must come from Java.
	ClassUnsupportedCrypto = "org.apache.accumulo.shoal.UnsupportedCrypto"

	// ClassResourceLimitExceeded: the job is executable in principle but
	// exceeds this compactor's configured input budget. The composer
	// holds whole RFile images in memory, so an unbounded job would OOM
	// the process (and take every other compaction on it down).
	ClassResourceLimitExceeded = "org.apache.accumulo.shoal.ResourceLimitExceeded"

	// ClassExecutionUnavailable: the job translated cleanly, but the
	// polling worker has not yet been wired to the isolated executor and
	// a configured storage backend. Accumulo's existing
	// compactionCompleted RPC is sufficient for the manager-authoritative
	// commit; this refusal is an integration gate, not a protocol gap.
	ClassExecutionUnavailable = "org.apache.accumulo.shoal.ExecutorNotWired"

	// ClassConfigurationUnavailable reports that the effective table
	// configuration could not be resolved consistently before execution.
	ClassConfigurationUnavailable = "org.apache.accumulo.shoal.TableConfigurationUnavailable"

	// ClassExecutionFailed reports a storage or compaction failure after the
	// complete capability gate succeeded.
	ClassExecutionFailed = "org.apache.accumulo.shoal.CompactionExecutionFailed"

	// ClassCommitUnavailable is retained as a source-compatible alias for
	// callers built against the previous slice.
	ClassCommitUnavailable = ClassExecutionUnavailable

	// ClassShuttingDown: the compactor was asked to stop while it held
	// this assignment. Reported so the coordinator can reschedule
	// immediately instead of waiting for the assignment to age out.
	ClassShuttingDown = "org.apache.accumulo.shoal.CompactorShuttingDown"

	// ClassUnsupportedVolume: the path is a spelling java.net.URI
	// accepts but shoal's storage backend cannot resolve, so the file
	// could be named in metadata yet never opened here. Distinct from
	// ClassMalformedJob because the job is not broken — a Java compactor
	// would run it.
	ClassUnsupportedVolume = "org.apache.accumulo.shoal.UnsupportedVolumeURI"
)

// Refusal is the structured reason a job cannot be executed by shoal.
// It is the error type every Translate failure carries; callers use
// RefusalOf to recover the coordinator-facing Class.
type Refusal struct {
	// Class is the sentinel exception-class name reported to the
	// coordinator (one of the Class* constants).
	Class string
	// Field names the job field that caused the refusal ("files[2]",
	// "iteratorSettings.vers", "overrides[table.file.compress.type]"),
	// or "" when the whole job is at fault.
	Field string
	// Detail is the human-readable explanation.
	Detail string
}

func (r *Refusal) Error() string {
	if r.Field == "" {
		return fmt.Sprintf("compactjob: %s: %s", r.Class, r.Detail)
	}
	return fmt.Sprintf("compactjob: %s: %s: %s", r.Class, r.Field, r.Detail)
}

// RefusalOf returns the structured refusal wrapped in err, or nil when
// err is not a refusal. Every error Translate returns is a refusal.
func RefusalOf(err error) *Refusal {
	var r *Refusal
	if errors.As(err, &r) {
		return r
	}
	return nil
}

func refuse(class, field, format string, args ...any) *Refusal {
	return &Refusal{Class: class, Field: field, Detail: fmt.Sprintf(format, args...)}
}

// Default per-job budget. The composer reads every input RFile fully
// into memory and retains the whole output image, so its footprint is
// roughly (sum of input sizes + output size), plus whatever the iterator
// stack buffers.
//
// The two halves are budgeted separately because neither bounds the
// other. The input half is knowable before the job runs — the
// coordinator declares each file's size — so it is an admission check.
// The output half is not: an output can exceed its inputs when
// compressed blocks are rewritten with codec "none", or when a stack
// emits cells that were in no input. So it is enforced during
// composition instead, by compaction.Compact, which abandons the
// compaction rather than growing past the cap.
//
// These defaults keep one job inside a few GiB; operators raise them for
// compactor pools with more headroom, or lower them to push big merges
// at Java.
const (
	DefaultMaxInputFiles      = 64
	DefaultMaxTotalInputBytes = int64(2) << 30 // 2 GiB
	DefaultMaxOutputBytes     = int64(2) << 30 // 2 GiB
)

// Limits bounds what a single job may consume. A zero field means
// "unlimited" so a caller can opt out explicitly; use DefaultLimits for
// the safe defaults.
type Limits struct {
	// MaxInputFiles caps len(job.files).
	MaxInputFiles int
	// MaxTotalInputBytes caps the summed size of the input files.
	MaxTotalInputBytes int64
	// MaxOutputBytes caps the output image the composer may retain. It
	// is carried on the Plan into compaction.Spec rather than checked
	// here: no translation-time figure predicts an output's size.
	MaxOutputBytes int64
}

// DefaultLimits returns the standard per-job input budget.
func DefaultLimits() Limits {
	return Limits{
		MaxInputFiles:      DefaultMaxInputFiles,
		MaxTotalInputBytes: DefaultMaxTotalInputBytes,
		MaxOutputBytes:     DefaultMaxOutputBytes,
	}
}

// Options tunes translation.
type Options struct {
	// DefaultCodec is the output block codec to use when the job carries
	// no table.file.compress.type override. Empty defers to the
	// compaction composer default. A caller that has read the table's
	// configuration should pass the table's codec here so the output
	// matches what Java would have written.
	//
	// The value is the writer's codec name (block.CodecNone,
	// block.CodecGzip, block.CodecSnappy), not the Accumulo property
	// spelling: the override path translates Accumulo's codec names
	// through accumuloCodecs, and a default skips that translation.
	// Anything the writer does not register is refused rather than
	// carried into a plan it would reject.
	DefaultCodec string

	// DefaultBlockSize is the output data-block threshold used when the
	// job carries no table.file.compress.blocksize override. Zero defers
	// to the RFile writer default. Negative values, and values past the
	// same cap the override honors, are refused.
	DefaultBlockSize int

	// Limits bounds the job's inputs. The zero value is unlimited; pass
	// DefaultLimits() for the standard budget.
	Limits Limits

	// TargetEmbeddingSpace records the table's desired convergence target.
	TargetEmbeddingSpace string

	// EmbeddingEpoch records the migration snapshot the table is
	// currently converging under, when one is configured. A job override
	// takes precedence over it.
	EmbeddingEpoch string
}

// OptionsFromTableProperties resolves the effective table configuration into
// the writer defaults used by Translate. Properties that require output
// features Shoal does not implement are refused before any storage I/O.
func OptionsFromTableProperties(properties map[string]string, limits Limits) (Options, error) {
	if properties == nil {
		return Options{}, refuse(ClassUnsupportedProperty, "tableConfiguration", "missing effective table configuration")
	}
	if fileType := properties["table.file.type"]; fileType != "" && fileType != "rf" {
		return Options{}, refuse(ClassUnsupportedProperty, "table.file.type",
			"unsupported table file type %q; shoal writes RFiles only", fileType)
	}
	for key, value := range properties {
		if value == "" {
			continue
		}
		if isCryptoOverride(key) &&
			!strings.Contains(value, "NoCryptoService") {
			return Options{}, refuse(ClassUnsupportedCrypto, key,
				"shoal writes cleartext RFiles; encrypted output must come from a Java compactor")
		}
		if strings.HasPrefix(key, "table.summarizer.") ||
			strings.HasPrefix(key, "table.sampler.opt.") {
			return Options{}, refuse(ClassUnsupportedProperty, key,
				"shoal cannot reproduce this table output feature")
		}
	}
	if sampler := properties["table.sampler"]; sampler != "" {
		return Options{}, refuse(ClassUnsupportedProperty, "table.sampler",
			"shoal cannot reproduce sampled RFile output")
	}
	if groups := properties["table.groups.enabled"]; groups != "" {
		return Options{}, refuse(ClassUnsupportedProperty, "table.groups.enabled",
			"shoal cannot reproduce locality-group RFile output")
	}
	if strings.EqualFold(properties["table.bloom.enabled"], "true") {
		return Options{}, refuse(ClassUnsupportedProperty, "table.bloom.enabled",
			"shoal cannot reproduce Bloom-filtered RFile output")
	}

	opts := Options{Limits: limits}
	opts.TargetEmbeddingSpace = strings.TrimSpace(properties[embeddingspace.TableTargetProperty])
	opts.EmbeddingEpoch = strings.TrimSpace(properties[embeddingspace.JobEpochProperty])
	if raw := properties[propCompressType]; raw != "" {
		codec, ok := accumuloCodecs[raw]
		if !ok {
			return Options{}, refuse(ClassUnsupportedProperty, propCompressType,
				"unsupported compression codec %q (shoal writes %s)", raw, supportedCodecList())
		}
		opts.DefaultCodec = codec
	}
	if raw := properties[propCompressBlockSize]; raw != "" {
		size, err := parseMemoryBytes(raw)
		if err != nil || size <= 0 || size > maxBlockSize {
			return Options{}, refuse(ClassUnsupportedProperty, propCompressBlockSize,
				"unsupported compression block size %q", raw)
		}
		opts.DefaultBlockSize = int(size)
	}
	return opts, nil
}

// InputFile is one translated entry of job.files.
type InputFile struct {
	// Entry is the raw metadata file entry exactly as the coordinator
	// sent it (StoredTabletFile JSON in Accumulo 3+, a bare path in the
	// legacy form). Preserved byte-for-byte because the eventual commit
	// must dereference this exact string.
	Entry string
	// Path is the file's URI, decoded out of Entry.
	Path string
	// Size is the file's byte size from its DataFileValue.
	Size int64
	// Entries is the file's cell count from its DataFileValue.
	Entries int64
	// Timestamp is the DataFileValue time field (-1 when absent).
	Timestamp int64
	// Embedding is the file's persisted embedding-space column, carried
	// through the job's override map (see
	// embeddingspace.JobFileStatesProperty).
	//
	// It is the zero FileState — State "" — when the job says nothing
	// about this file. That is *absence*, not an explicit unknown: the
	// coordinator made no claim, so there is nothing to cross-check the
	// file's footer against and the footer alone decides the input's
	// space. An explicit embeddingspace.Unknown() is a claim, and one
	// that must agree with the footer.
	Embedding embeddingspace.FileState
}

// Iterator is one translated entry of job.iteratorSettings.
type Iterator struct {
	// Name is the operator's nickname for the stack entry.
	Name string
	// Class is the fully-qualified Java class the coordinator sent.
	Class string
	// Priority is the entry's stack priority (low runs closer to the
	// source).
	Priority int32
	// Spec is the shoal iterator the class maps to, with the job's
	// options attached.
	Spec iterrt.IterSpec
}

// Plan is a fully translated, executable description of one coordinator
// job: everything internal/compaction needs except the input bytes.
type Plan struct {
	// ECID is the external compaction id the coordinator assigned.
	ECID string
	// TableID is the extent's table id.
	TableID string
	// Extent is the tablet being compacted, as sent.
	Extent *data.TKeyExtent
	// Kind is SYSTEM (the coordinator's own planning) or USER (a
	// client-requested compaction driven by a FATE transaction).
	Kind tabletserver.TCompactionKind
	// FateID renders job.fateId in FateId's canonical form,
	// "FATE:<INSTANCE_TYPE>:<uuid>", for USER compactions; empty for
	// SYSTEM jobs that carry none. The canonical spelling is what
	// FateId.from(String) round-trips and what Accumulo's own logs and
	// fate admin commands show, so an operator can grep one string
	// across both systems.
	FateID string
	// Inputs are the files to merge, in the coordinator's order.
	Inputs []InputFile
	// OutputFile is the path the compactor must write, verbatim. It is
	// the coordinator's per-job temporary name
	// (<dir>/<A|C><name>.rf_tmp_<ECID>, from
	// TabletNameGenerator.getNextDataFilenameForMajc), not the file the
	// tablet ends up referencing: the *manager* renames it at commit
	// (manager/.../coordinator/commit/RenameCompactionFile). A compactor
	// that wrote the final name directly, or renamed this one itself,
	// would be publishing a file the manager has not accepted, so shoal
	// writes this path and nothing else.
	OutputFile string
	// PropagateDeletes is the job flag as sent: false means this output
	// becomes the tablet's only file, so tombstones may be dropped.
	PropagateDeletes bool
	// TotalInputBytes / TotalInputEntries are the summed DataFileValue
	// figures across Inputs.
	TotalInputBytes   int64
	TotalInputEntries int64
	// Iterators is the translated stack with its Java provenance intact
	// (for logging and for the eventual shadow comparison).
	Iterators []Iterator
	// Stack is the iterator chain handed to iterrt.BuildStack.
	Stack []iterrt.IterSpec
	// Scope is always iterrt.ScopeMajc — an external compaction is a
	// major compaction by construction.
	Scope iterrt.IteratorScope
	// FullMajorCompaction is !PropagateDeletes, mirroring Java's
	// FileCompactor.CompactionIteratorEnvironment.isFullMajorCompaction.
	FullMajorCompaction bool
	// Codec is the resolved output block codec.
	Codec string
	// BlockSize is the resolved output data-block threshold (0 = writer
	// default).
	BlockSize int
	// MaxOutputBytes is the caller's Limits.MaxOutputBytes, carried here
	// so the executor enforces it without having to be handed the
	// Limits separately. Zero means unlimited.
	MaxOutputBytes       int64
	TargetEmbeddingSpace string
	// EmbeddingEpoch is the migration snapshot this compaction belongs
	// to, taken from the job override or the table property. It is
	// stamped onto the output so a converged file can be attributed to
	// the migration that produced it, and so a converger bound to a
	// different epoch refuses this compaction rather than publishing a
	// file labelled with a superseded target.
	EmbeddingEpoch string
}

// Spec assembles the compaction.Spec for this plan. inputs must be the
// RFile images for Plan.Inputs; the caller fetches them (translation is
// I/O-free by design).
//
// converger may be nil, which disables convergence: the output then
// carries whatever space the inputs agreed on. A caller that owns an
// embedding provider passes one so this compaction can converge the
// tablet toward TargetEmbeddingSpace.
func (p *Plan) Spec(inputs []compaction.Input) compaction.Spec {
	return p.SpecWithConverger(inputs, nil)
}

// SpecWithConverger is Spec with a convergence engine attached.
func (p *Plan) SpecWithConverger(
	inputs []compaction.Input, converger compaction.Converger,
) compaction.Spec {
	return compaction.Spec{
		Inputs:               inputs,
		Stack:                p.Stack,
		Scope:                p.Scope,
		FullMajorCompaction:  p.FullMajorCompaction,
		Codec:                p.Codec,
		BlockSize:            p.BlockSize,
		MaxOutputBytes:       p.MaxOutputBytes,
		TargetEmbeddingSpace: p.TargetEmbeddingSpace,
		EmbeddingEpoch:       p.EmbeddingEpoch,
		Converger:            converger,
	}
}

// LogValue renders the plan for slog without dumping every file path at
// info level.
func (p *Plan) LogValue() slog.Value {
	names := make([]string, 0, len(p.Iterators))
	for _, it := range p.Iterators {
		names = append(names, it.Name+"="+it.Spec.Name)
	}
	return slog.GroupValue(
		slog.String("ecid", p.ECID),
		slog.String("table", p.TableID),
		slog.String("extent", ExtentString(p.Extent)),
		slog.String("kind", p.Kind.String()),
		slog.String("fate_id", p.FateID),
		slog.Int("inputs", len(p.Inputs)),
		slog.Int64("input_bytes", p.TotalInputBytes),
		slog.Int64("input_entries", p.TotalInputEntries),
		slog.String("output", p.OutputFile),
		slog.Bool("propagate_deletes", p.PropagateDeletes),
		slog.Bool("full_major", p.FullMajorCompaction),
		slog.String("codec", p.Codec),
		slog.Int("block_size", p.BlockSize),
		slog.Int64("max_output_bytes", p.MaxOutputBytes),
		slog.String("target_embedding_space", p.TargetEmbeddingSpace),
		slog.String("embedding_epoch", p.EmbeddingEpoch),
		slog.Any("stack", names),
	)
}

// ExtentString renders a tablet extent for logs. Defensive against a
// nil/partial extent so logging can never panic on a malformed job.
func ExtentString(ex *data.TKeyExtent) string {
	if ex == nil {
		return "<no-extent>"
	}
	end := "+inf"
	if r := ex.GetEndRow(); r != nil {
		end = fmt.Sprintf("%q", r)
	}
	prev := "-inf"
	if r := ex.GetPrevEndRow(); r != nil {
		prev = fmt.Sprintf("%q", r)
	}
	return fmt.Sprintf("table=%s prev=%s end=%s", ex.GetTable(), prev, end)
}

// Translate converts a coordinator assignment into an executable Plan,
// or returns a *Refusal describing why shoal must hand the job back.
//
// Field mapping (Java reference: server/compactor/.../Compactor.java and
// core/.../compaction/thrift/TExternalCompactionJob):
//
//	externalCompactionId → Plan.ECID              (format-checked)
//	extent               → Plan.Extent, TableID   (must be populated)
//	files                → Plan.Inputs            (fences refused)
//	iteratorSettings     → Plan.Stack             (allowlisted classes)
//	outputFile           → Plan.OutputFile        (this job's .rf_tmp_<ECID>)
//	propagateDeletes     → FullMajorCompaction    (inverted, ScopeMajc)
//	kind                 → Plan.Kind              (USER requires fateId)
//	fateId               → Plan.FateID
//	overrides            → Codec, BlockSize       (allowlisted properties)
//
// Checking happens in two complete passes, and the split is load-bearing
// for the class an operator sees:
//
//	pass 1  every structural check, over every field: is this assignment
//	        internally consistent? Failures are ClassMalformedJob and say
//	        nothing about shoal.
//	pass 2  every capability check: could *shoal* reproduce this exact
//	        compaction? Failures name the missing capability.
//
// A job that is both malformed and unsupported is therefore always
// reported as malformed, no matter which field each problem is in —
// a job nobody can run is more actionable than a job only Java can run.
// Within pass 2 the order is inputs, then output encoding, then
// iterators, so a job with several gaps always reports the same one.
func Translate(job *tabletserver.TExternalCompactionJob, opts Options) (*Plan, error) {
	if job == nil {
		return nil, refuse(ClassMalformedJob, "", "nil job")
	}

	// --- pass 1: structure ------------------------------------------

	ecid := job.GetExternalCompactionId()
	if ecid == "" {
		return nil, refuse(ClassMalformedJob, "externalCompactionId", "missing")
	}
	if err := checkECID(ecid); err != nil {
		return nil, err
	}

	extent, tableID, err := translateExtent(job)
	if err != nil {
		return nil, err
	}

	kind, fateID, err := translateKind(job)
	if err != nil {
		return nil, err
	}

	inputs, totalBytes, totalEntries, err := parseInputs(job)
	if err != nil {
		return nil, err
	}

	outputFile, err := parseOutput(job, inputs, ecid)
	if err != nil {
		return nil, err
	}

	if err := checkOutputTable(outputFile, tableID); err != nil {
		return nil, err
	}

	if job.GetOverrides() == nil {
		// Compactor.initialize opens with job.getOverrides().isEmpty();
		// a missing map is a null there, not an empty one, so the
		// reference implementation throws before it reads a single file.
		// Reading it as "no overrides" would have shoal call a job
		// translatable that the compactor it is standing in for cannot
		// start. Thrift decodes a present empty map into a non-nil one,
		// so the two really are distinguishable on the wire.
		return nil, refuse(ClassMalformedJob, "overrides",
			"missing; Compactor.initialize dereferences it before reading any file")
	}

	overrideKeys, blockSizeOverride, err := parseOverrides(job.GetOverrides())
	if err != nil {
		return nil, err
	}

	rawIters, err := parseIterators(job)
	if err != nil {
		return nil, err
	}

	// --- pass 2: capability -----------------------------------------

	if err := checkInputCapability(inputs, totalBytes, opts.Limits); err != nil {
		return nil, err
	}

	if err := checkOutputCapability(outputFile, ecid); err != nil {
		return nil, err
	}

	codec, blockSize, err := resolveOutputEncoding(job.GetOverrides(), overrideKeys, blockSizeOverride, opts)
	if err != nil {
		return nil, err
	}

	// Runs after the input-capability gate so a job that is too large is
	// refused for its size rather than for a column it also carries, and
	// before the plan is built so Inputs already carry their space.
	target, epoch, err := resolveEmbeddingOverrides(job.GetOverrides(), inputs, opts)
	if err != nil {
		return nil, err
	}

	fullMajor := !job.GetPropagateDeletes()
	iters, err := mapIterators(rawIters, fullMajor)
	if err != nil {
		return nil, err
	}

	stack := make([]iterrt.IterSpec, 0, len(iters))
	for _, it := range iters {
		stack = append(stack, it.Spec)
	}

	return &Plan{
		ECID:                 ecid,
		TableID:              tableID,
		Extent:               cloneExtent(extent),
		Kind:                 kind,
		FateID:               fateID,
		Inputs:               inputFiles(inputs),
		OutputFile:           outputFile,
		PropagateDeletes:     job.GetPropagateDeletes(),
		TotalInputBytes:      totalBytes,
		TotalInputEntries:    totalEntries,
		Iterators:            iters,
		Stack:                stack,
		Scope:                iterrt.ScopeMajc,
		FullMajorCompaction:  fullMajor,
		Codec:                codec,
		BlockSize:            blockSize,
		MaxOutputBytes:       opts.Limits.MaxOutputBytes,
		TargetEmbeddingSpace: target,
		EmbeddingEpoch:       epoch,
	}, nil
}

// cloneExtent copies the extent out of the job.
//
// A Plan outlives the RPC that produced it: cmd/shoal-compactor logs it
// and the execution slice will act on it, both after control has gone
// back to the Thrift layer that owns the job struct. Handing out the
// generated struct — and the byte slices inside it — would make the
// plan's identity depend on whether the transport recycles that struct,
// which is exactly why parseIterators copies each iterator's option map
// rather than aliasing it. The extent was the one part still shared.
//
// bytes.Clone keeps nil nil, so the infinite bounds an extent spells as
// a missing endRow or prevEndRow survive the copy as themselves rather
// than becoming empty rows.
func cloneExtent(ex *data.TKeyExtent) *data.TKeyExtent {
	return &data.TKeyExtent{
		Table:      bytes.Clone(ex.GetTable()),
		EndRow:     bytes.Clone(ex.GetEndRow()),
		PrevEndRow: bytes.Clone(ex.GetPrevEndRow()),
	}
}

// checkECID mirrors ExternalCompactionId.of: the prefix, then the
// suffix parsed as a UUID. Java rejects both failures with
// IllegalArgumentException, so an id it would reject can never name a
// compaction the manager is able to track — building a plan for one
// would mean doing work no coordinator will ever accept a result for.
//
// The spelling has to be the canonical hyphenated one. uuid.Parse also
// takes the 32-hex, braced and urn: forms, all of which UUID.fromString
// rejects — and this id is the one every compactionFailed carries, so
// accepting a spelling the coordinator cannot parse would leave the
// slot assigned until it times out rather than released. Going the
// other way costs nothing: UUID.fromString also takes short forms like
// 1-1-1-1-1, but ExternalCompactionId.generate only ever emits the
// canonical one, and refusing a job is still a release.
func checkECID(ecid string) error {
	suffix, ok := strings.CutPrefix(ecid, ECIDPrefix)
	if !ok {
		return refuse(ClassMalformedJob, "externalCompactionId",
			"%q is not in Accumulo's %s<uuid> form", ecid, ECIDPrefix)
	}
	if !isCanonicalUUID(suffix) {
		return refuse(ClassMalformedJob, "externalCompactionId",
			"%q does not carry a canonical UUID after %q", ecid, ECIDPrefix)
	}
	return nil
}

// translateExtent validates the tablet the job names. A compaction whose
// extent is unusable cannot be committed later, so it is refused before
// any work happens.
func translateExtent(job *tabletserver.TExternalCompactionJob) (*data.TKeyExtent, string, error) {
	if !job.IsSetExtent() || job.GetExtent() == nil {
		return nil, "", refuse(ClassMalformedJob, "extent", "missing")
	}
	ex := job.GetExtent()
	if len(ex.GetTable()) == 0 {
		return nil, "", refuse(ClassMalformedJob, "extent.table", "missing table id")
	}
	if ExtentBoundsInverted(ex) {
		prev, end := ex.GetPrevEndRow(), ex.GetEndRow()
		return nil, "", refuse(ClassMalformedJob, "extent",
			"prevEndRow %q is not before endRow %q", prev, end)
	}
	return ex, string(ex.GetTable()), nil
}

// ExtentBoundsInverted reports whether an extent is one KeyExtent's
// constructor throws on: prevEndRow is exclusive and endRow inclusive,
// so prev must sort strictly before end whenever both bounds are finite.
//
// It is exported because the answer decides more than whether a job can
// run. KeyExtent.fromThrift builds the same extent on the manager side,
// and compactionFailed calls it before it clears the assignment, so an
// extent that fails this test cannot be handed back either — see
// unreleasableReason in cmd/shoal-compactor.
//
// A missing bound is infinite rather than inverted, matching
// fromThrift's null mapping, and Text.compareTo compares bytes unsigned,
// which is what bytes.Compare does.
func ExtentBoundsInverted(ex *data.TKeyExtent) bool {
	if ex == nil {
		return false
	}
	prev, end := ex.GetPrevEndRow(), ex.GetEndRow()
	return prev != nil && end != nil && bytes.Compare(prev, end) >= 0
}

// translateKind checks the compaction kind and its FATE binding. A USER
// compaction is driven by a FATE transaction that waits for the commit,
// so a job missing its fateId could never be completed.
func translateKind(job *tabletserver.TExternalCompactionJob) (tabletserver.TCompactionKind, string, error) {
	kind := job.GetKind()
	switch kind {
	case tabletserver.TCompactionKind_SYSTEM, tabletserver.TCompactionKind_USER:
	default:
		return kind, "", refuse(ClassMalformedJob, "kind", "unknown compaction kind %d", int64(kind))
	}

	fateID := ""
	if job.IsSetFateId() && job.GetFateId() != nil {
		fate := job.GetFateId()
		// FateId.fromThrift switches over TFateInstanceType with only
		// USER and META arms and then requires UuidUtil.isUUID, so a
		// value Java would throw on must not become a plan here: the
		// unknown enum would stringify to "<UNSET>" and a malformed
		// uuid would follow the job into every log line as if it named
		// a real transaction.
		switch fate.GetType() {
		case manager.TFateInstanceType_META, manager.TFateInstanceType_USER:
		default:
			return kind, "", refuse(ClassMalformedJob, "fateId",
				"unknown FATE instance type %d", int64(fate.GetType()))
		}
		if txUUID := fate.GetTxUUIDStr(); !isCanonicalUUID(txUUID) {
			return kind, "", refuse(ClassMalformedJob, "fateId",
				"transaction uuid %q is not a canonical uuid", txUUID)
		}
		fateID = fateIDString(fate)
	}
	if kind == tabletserver.TCompactionKind_USER && fateID == "" {
		return kind, "", refuse(ClassMalformedJob, "fateId",
			"USER compaction has no FATE transaction to commit against")
	}
	return kind, fateID, nil
}

// fateIDString renders FateId's canonical form. FateId.from builds it as
// PREFIX + type + ":" + txUUID with PREFIX = "FATE:", and isFateId
// requires that prefix, so dropping it would produce a string that
// names the right transaction but cannot be fed back to Accumulo.
func fateIDString(fate *manager.TFateId) string {
	return fatePrefix + fate.GetType().String() + ":" + fate.GetTxUUIDStr()
}

// fatePrefix is FateId.PREFIX.
const fatePrefix = "FATE:"

// isCanonicalUUID mirrors UuidUtil.isUUID, the check FateId.fromThrift
// applies to a transaction id: exactly 36 characters, '-' at offsets 8,
// 13, 18 and 23, and hex everywhere else. It is deliberately stricter
// than uuid.Parse, which also accepts the urn, braced and unhyphenated
// forms Java rejects here.
func isCanonicalUUID(s string) bool {
	if len(s) != 36 {
		return false
	}
	for i := 0; i < 36; i++ {
		c := s[i]
		switch i {
		case 8, 13, 18, 23:
			if c != '-' {
				return false
			}
		default:
			isHex := (c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')
			if !isHex {
				return false
			}
		}
	}
	return true
}

// parsedInput is one structurally valid input entry, carrying the fence
// that pass 2 refuses on.
type parsedInput struct {
	file             InputFile
	field            string
	startRow, endRow string
	// startRaw and endRaw are the rows the entry declares, after base64
	// decoding and after the Hadoop Text length is applied. They are
	// what identifies the reference, because a StoredTabletFile is its
	// path *and* its range, and two spellings of one range have to
	// compare equal.
	startRaw, endRaw []byte
}

func (p parsedInput) fenced() bool { return p.startRow != "" || p.endRow != "" }

// key identifies the tablet-file reference this entry names. Accumulo
// deliberately allows several StoredTabletFiles over one path with
// different ranges — that is how a fenced file is referenced after a
// split or merge — so the range belongs in the identity.
//
// The rows compared are the ones Java would build: base64 decoded, then
// cut to the length the Text declares. Anything shallower would let two
// entries that name one range differ in the key — padded against
// unpadded base64, or bytes past the declared length, which readFully
// never looks at — and a file would be merged twice.
//
// The three parts are length-prefixed rather than delimited. A row is
// arbitrary binary and routinely ends in a zero byte, because the rows
// KeyExtent.toDataRange stores are followingKey(ROW) results, so no
// byte value is available to separate them: ("a", "\x00b") and
// ("a\x00", "b") are different ranges that a delimiter would map to one
// key, and a job naming both would be refused as a duplicate.
func (p parsedInput) key() string {
	var b strings.Builder
	for _, part := range []string{pathIdentity(p.file.Path), string(p.startRaw), string(p.endRaw)} {
		b.WriteString(strconv.Itoa(len(part)))
		b.WriteByte(':')
		b.WriteString(part)
	}
	return b.String()
}

// pathIdentity reduces a path URI to what actually selects a file, so
// that two spellings of one file compare equal.
//
// The measure is shoal's own storage backend, because that is what
// opens these paths, and it is used here rather than modelled:
// hdfs.Backend.resolve runs url.Parse and then keeps only u.Host —
// compared with strings.EqualFold — and u.Path, so this runs the same
// url.Parse and folds the same two fields. Deriving the identity from
// the parse rather than from the spelling is what keeps the two from
// drifting apart; url.Parse decodes both fields, so percent escapes,
// userinfo and host case all fall out of it: hdfs://alice@NN/v%6Fl/x
// and HDFS://nn/vol/x are one file on the namenode.
//
// The host folds by simple-fold class, not by ToLower, because EqualFold
// is what resolve compares with and the two disagree: EqualFold calls
// the Greek "Σ" and final "ς" equal while ToLower maps them apart, and
// it calls ASCII "k" and the Kelvin sign equal likewise. Mapping every
// rune to the lowest member of its fold orbit is a canonical key for
// exactly EqualFold's classes, including its treatment of invalid UTF-8,
// which it decodes to U+FFFD before comparing.
//
// Accumulo's ReferencedTabletFile compares normalized path strings and
// would call all of those distinct, which is exactly why shoal has to
// fold them: two inputs spelled differently would have every cell in
// the file merged twice, and an output colliding with an input would
// overwrite it at commit.
//
// The path itself stays case-sensitive, as it is on every filesystem
// Accumulo runs on, and the port stays because resolve compares it as
// part of the authority.
//
// A URI resolve cannot open at all — one url.Parse rejects, an opaque
// one, or one carrying a query or fragment — is identified by its exact
// spelling. Folding those would report a duplicate for a job whose real
// defect is the URI itself, which checkVolumeCapability names precisely
// in pass 2. A scheme-less path is identified the same way, because
// resolve hands those to the backend verbatim without decoding.
func pathIdentity(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme == "" || u.Opaque != "" || u.RawQuery != "" || u.Fragment != "" {
		return "raw\x00" + raw
	}
	host := foldHost(u.Host)
	// Length-prefixed so that no host and path can trade characters:
	// url.Parse keeps "/" out of a host, but a fold key is not the
	// spelling it came from.
	return "uri\x00" + strconv.Itoa(len(host)) + "\x00" + host + u.Scheme + "\x00" + u.Path
}

// foldHost maps a host to one spelling per strings.EqualFold class, so
// that two hosts resolve calls equal have one identity here. Each rune
// becomes the lowest member of its simple-fold orbit, which is the
// relation EqualFold compares by; ranging over the string decodes
// invalid bytes to U+FFFD, which is what EqualFold compares them as.
func foldHost(host string) string {
	var b strings.Builder
	b.Grow(len(host))
	for _, r := range host {
		lowest := r
		for f := unicode.SimpleFold(r); f != r; f = unicode.SimpleFold(f) {
			if f < lowest {
				lowest = f
			}
		}
		b.WriteRune(lowest)
	}
	return b.String()
}

// decodeEscapes replaces every well-formed percent escape with the byte
// it names, which is what url.Parse hands the namenode and what
// Hadoop's Path.toString renders. A malformed escape is left alone:
// pathIdentity runs before checkTabletFilePath on the output, and a
// truncated escape is that function's refusal to report, not this one's
// to guess at.
func decodeEscapes(raw string) string {
	if !strings.ContainsRune(raw, '%') {
		return raw
	}
	var b strings.Builder
	b.Grow(len(raw))
	for i := 0; i < len(raw); i++ {
		if raw[i] == '%' && i+2 < len(raw) && isHexDigit(raw[i+1]) && isHexDigit(raw[i+2]) {
			value, err := strconv.ParseUint(raw[i+1:i+3], 16, 8)
			if err == nil {
				b.WriteByte(byte(value))
				i += 2
				continue
			}
		}
		b.WriteByte(raw[i])
	}
	return b.String()
}

func inputFiles(inputs []parsedInput) []InputFile {
	out := make([]InputFile, 0, len(inputs))
	for _, in := range inputs {
		out = append(out, in.file)
	}
	return out
}

// resolveEmbeddingOverrides applies the job's embedding-space carriage to
// the parsed inputs and resolves the table's convergence target.
//
// Three refusals live here, and each of them exists because the
// alternative is a file labelled with a space it does not contain:
//
//   - an unparseable or invalid column, rather than treating a corrupt
//     value as "no information";
//   - a target space that is not a legal identity, rather than
//     converging toward something no embedder can produce;
//   - an entry naming a file this job does not compact, which mirrors
//     the orphan-column check the metadata reader already performs and
//     means the coordinator and the compactor disagree about what this
//     job is.
//
// An input the column does not mention is left *absent* — the zero
// FileState — not an explicit unknown. The difference matters: a present
// state is a claim, and the compactor cross-checks every claim against
// the file's own footer. Synthesising "unknown" for silence would turn
// every file written before this column existed into a claim that
// contradicts its footer, and the compaction would fail with an
// integrity error instead of simply compacting. Absence means "the
// coordinator said nothing", so the footer alone decides.
func resolveEmbeddingOverrides(
	overrides map[string]string, inputs []parsedInput, opts Options,
) (string, string, error) {
	target, err := embeddingspace.ParseTarget(opts.TargetEmbeddingSpace)
	if err != nil {
		return "", "", refuse(ClassUnsupportedProperty, embeddingspace.TableTargetProperty,
			"invalid table embedding target: %v", err)
	}
	if raw, ok := overrides[embeddingspace.TableTargetProperty]; ok {
		override, err := embeddingspace.ParseTarget(raw)
		if err != nil {
			return "", "", refuse(ClassUnsupportedProperty,
				"overrides["+embeddingspace.TableTargetProperty+"]",
				"invalid embedding target override: %v", err)
		}
		target = override
	}

	epoch := strings.TrimSpace(opts.EmbeddingEpoch)
	if raw, ok := overrides[embeddingspace.JobEpochProperty]; ok {
		epoch = strings.TrimSpace(raw)
	}
	if len(epoch) > embeddingspace.MaxJobEpochBytes {
		return "", "", refuse(ClassUnsupportedProperty,
			"overrides["+embeddingspace.JobEpochProperty+"]",
			"embedding epoch is %d bytes, limit %d", len(epoch), embeddingspace.MaxJobEpochBytes)
	}

	// Deliberately no loop assigning Unknown() here: absent stays absent.
	raw, ok := overrides[embeddingspace.JobFileStatesProperty]
	if !ok {
		return target, epoch, nil
	}
	states, err := embeddingspace.DecodeFileStates(raw)
	if err != nil {
		return "", "", refuse(ClassMalformedJob,
			"overrides["+embeddingspace.JobFileStatesProperty+"]",
			"invalid per-file embedding column: %v", err)
	}
	byEntry := make(map[string]int, len(inputs))
	for i, in := range inputs {
		byEntry[in.file.Entry] = i
	}
	for entry, state := range states {
		index, found := byEntry[entry]
		if !found {
			return "", "", refuse(ClassMalformedJob,
				"overrides["+embeddingspace.JobFileStatesProperty+"]",
				"names a file this job does not compact")
		}
		inputs[index].file.Embedding = state
	}
	return target, epoch, nil
}

// parseInputs decodes every input entry and checks the job's inputs for
// internal consistency. Structure only: whether shoal can read a given
// file, and whether the job fits this compactor's budget, are pass-2
// questions (checkInputCapability).
func parseInputs(job *tabletserver.TExternalCompactionJob) ([]parsedInput, int64, int64, error) {
	files := job.GetFiles()
	if len(files) == 0 {
		return nil, 0, 0, refuse(ClassMalformedJob, "files", "job has no input files")
	}

	out := make([]parsedInput, 0, len(files))
	// Duplicates are detected by the decoded reference — path plus range
	// — not by the raw entry: the same reference can arrive as JSON with
	// different field order, with padded or unpadded rows, or as a
	// legacy bare path, and merging it twice would double its cells.
	seen := make(map[string]int, len(files))
	var totalBytes, totalEntries int64
	for i, f := range files {
		field := fmt.Sprintf("files[%d]", i)
		if f == nil {
			return nil, 0, 0, refuse(ClassMalformedJob, field, "nil entry")
		}
		entry := f.GetMetadataFileEntry()
		if entry == "" {
			return nil, 0, 0, refuse(ClassMalformedJob, field, "empty metadata file entry")
		}
		if f.GetSize() < 0 || f.GetEntries() < 0 {
			return nil, 0, 0, refuse(ClassMalformedJob, field,
				"negative DataFileValue (size=%d entries=%d)", f.GetSize(), f.GetEntries())
		}

		in, err := decodeFileEntry(entry, field)
		if err != nil {
			return nil, 0, 0, err
		}
		if err := checkTabletFilePath(in.file.Path, field); err != nil {
			return nil, 0, 0, err
		}
		in.file.Size = f.GetSize()
		in.file.Entries = f.GetEntries()
		in.file.Timestamp = f.GetTimestamp()

		if prev, dup := seen[in.key()]; dup {
			return nil, 0, 0, refuse(ClassMalformedJob, field,
				"duplicates files[%d] (%s); compacting a file twice would double its cells",
				prev, in.file.Path)
		}
		seen[in.key()] = i

		// The declared sizes are attacker- or bug-supplied. Summing them
		// blindly can wrap negative, which would slip past the budget
		// check below and report nonsense totals in the plan.
		if totalBytes > math.MaxInt64-f.GetSize() || totalEntries > math.MaxInt64-f.GetEntries() {
			return nil, 0, 0, refuse(ClassMalformedJob, field,
				"DataFileValue totals overflow int64 (size=%d entries=%d after %d files); "+
					"the declared file sizes cannot describe a real tablet",
				f.GetSize(), f.GetEntries(), i)
		}
		totalBytes += f.GetSize()
		totalEntries += f.GetEntries()
		out = append(out, in)
	}
	return out, totalBytes, totalEntries, nil
}

// checkInputCapability refuses inputs shoal cannot read and jobs beyond
// this compactor's configured budget. The two file-shape refusals come
// first: raising a limit could never make a fenced or non-RFile input
// runnable, so they are the more useful answer when a job has both
// problems.
func checkInputCapability(inputs []parsedInput, totalBytes int64, limits Limits) error {
	for _, in := range inputs {
		if in.fenced() {
			// StoredTabletFile.deserialize rebuilds the fence as
			// new Range(startRow, true, endRow, false), so the bound an
			// operator sees here has to read as start-inclusive and
			// end-exclusive. The rows are the decoded ones, not the
			// base64 the entry spells them with.
			return refuse(ClassRangedInputFile, in.field,
				"input %s is fenced to [%q,%q); shoal reads whole RFiles, so the fence cannot be honored",
				in.file.Path, in.startRaw, in.endRaw)
		}
	}
	// compaction.Compact opens every input through bcfile.NewReader and
	// rfile.Open, with no dispatch on the file's type. Accumulo picks a
	// reader by extension (FileOperations), so a tablet holding anything
	// but RFiles would hand shoal a file its composer cannot open — and
	// discovering that after the slot looks runnable is exactly the
	// failure this pass exists to prevent.
	for _, in := range inputs {
		if !strings.HasSuffix(in.file.Path, rfileExtension) {
			return refuse(ClassUnsupportedProperty, in.field,
				"input %s is a %s file; shoal's reader opens RFiles only",
				in.file.Path, outputExtension(in.file.Path))
		}
	}
	for _, in := range inputs {
		if err := checkVolumeCapability(in.file.Path, in.field); err != nil {
			return err
		}
	}
	if limits.MaxInputFiles > 0 && len(inputs) > limits.MaxInputFiles {
		return refuse(ClassResourceLimitExceeded, "files",
			"%d input files exceeds the configured limit of %d", len(inputs), limits.MaxInputFiles)
	}
	if limits.MaxTotalInputBytes > 0 && totalBytes > limits.MaxTotalInputBytes {
		return refuse(ClassResourceLimitExceeded, "files",
			"total input size %d bytes exceeds the configured limit of %d bytes",
			totalBytes, limits.MaxTotalInputBytes)
	}
	return nil
}

// decodeFileEntry extracts the file's URI and row fence from a metadata
// file entry. Accumulo 3+ sends StoredTabletFile JSON ({"path":...,
// "startRow":...,"endRow":...}); the legacy form is a bare path.
func decodeFileEntry(entry, field string) (parsedInput, error) {
	in := parsedInput{field: field, file: InputFile{Entry: entry}}
	if !strings.HasPrefix(strings.TrimSpace(entry), "{") {
		// Legacy bare-path form (Accumulo ≤ 2.0). Whole-file by
		// definition, so there is no fence to carry.
		in.file.Path = entry
		return in, nil
	}
	// The fence must be read before the path: an entry that merely
	// omits startRow or endRow would otherwise decode to empty strings
	// and be indistinguishable from a whole-file entry, which is the
	// one misreading that silently widens a compaction's input beyond
	// the range the manager authorized.
	startRaw, endRaw, err := checkEntryFields(entry, field)
	if err != nil {
		return parsedInput{}, err
	}
	decoded, err := metadata.DecodeStoredTabletFile([]byte(entry))
	if err != nil {
		return parsedInput{}, refuse(ClassMalformedJob, field,
			"undecodable StoredTabletFile entry: %v", err)
	}
	in.file.Path = decoded.Path
	in.startRow, in.endRow = decoded.StartRow, decoded.EndRow
	in.startRaw, in.endRaw = startRaw, endRaw
	return in, nil
}

// hdfsTablesDirName mirrors ReferencedTabletFile.HDFS_TABLES_DIR_NAME,
// which is Constants.HDFS_TABLES_DIR ("/tables") without its leading
// slash. parsePath requires the fourth-from-last path segment to equal
// it exactly.
const hdfsTablesDirName = "tables"

var (
	// validFileNameRE mirrors ValidationUtil's
	// VALID_FILE_NAME_MATCH_PATTERN, which parsePath applies to the last
	// segment with Matcher.matches (whole-string).
	validFileNameRE = regexp.MustCompile(`^[\dA-Za-z._-]+$`)
	// validSchemeRE is the URI scheme grammar; parsePath only asserts
	// that a scheme is present, but a value java.net.URI would reject
	// could never have reached the coordinator as a Path in the first
	// place.
	validSchemeRE = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9+.-]*$`)
)

// checkTabletFilePath refuses a file path Accumulo could not turn back
// into a ReferencedTabletFile.
//
// Every path in a job round-trips through that class: an input becomes a
// StoredTabletFile, whose constructor calls ReferencedTabletFile.of, and
// the output is renamed and stored as one at commit. parsePath demands a
// fully qualified URI shaped
// <volume>/tables/<tableId>/<tablet>/<file> — it requires a scheme, at
// least four path segments, "tables" as the fourth-from-last, non-blank
// volume/tablesPath/tabletDirectory/fileName, and a file name matching
// ValidationUtil's pattern. Anything else throws, so a plan built on it
// names a file the manager could never accept.
//
// The path must already be normalized. Hadoop's Path constructor
// collapses "//" and resolves "."/".." before parsePath ever sees the
// string, and the coordinator sends what that normalization produced
// (getNormalizedPathStr, and the metadata entry serialized from a Path).
// So a non-normalized spelling did not come from Accumulo, and treating
// it as distinct is what would let the same RFile appear twice in one
// job under two names and have every cell counted twice.
func checkTabletFilePath(raw, field string) error {
	if raw == "" {
		return refuse(ClassMalformedJob, field, "empty file path")
	}
	if i := strings.IndexAny(raw, "?#"); i >= 0 {
		return refuse(ClassMalformedJob, field,
			"%q carries a %q; a tablet file path is a plain URI", raw, raw[i:i+1])
	}
	if err := checkURISyntax(raw); err != nil {
		return refuse(ClassMalformedJob, field, "%q is not a valid URI: %v", raw, err)
	}
	colon := strings.Index(raw, ":")
	slash := strings.Index(raw, "/")
	if colon <= 0 || (slash >= 0 && slash < colon) {
		return refuse(ClassMalformedJob, field,
			"%q has no URI scheme; ReferencedTabletFile requires a fully qualified path", raw)
	}
	if scheme := raw[:colon]; !validSchemeRE.MatchString(scheme) {
		return refuse(ClassMalformedJob, field, "%q has an invalid URI scheme %q", raw, scheme)
	}
	rest := raw[colon+1:]
	if authority, found := strings.CutPrefix(rest, "//"); found {
		if i := strings.Index(authority, "/"); i >= 0 {
			rest = authority[i:]
		} else {
			rest = ""
		}
	}
	if !strings.HasPrefix(rest, "/") {
		return refuse(ClassMalformedJob, field,
			"%q has no absolute path after its scheme", raw)
	}
	segments := strings.Split(rest[1:], "/")
	for _, segment := range segments {
		switch segment {
		case "":
			return refuse(ClassMalformedJob, field,
				"%q is not a normalized path (empty segment)", raw)
		case ".", "..":
			return refuse(ClassMalformedJob, field,
				"%q is not a normalized path (%q segment)", raw, segment)
		}
	}
	// The loop above reads the path as written, but Hadoop reads it
	// decoded: new Path(URI) is URI.normalize(), which resolves segments
	// in uri.path — the decoded component. So "%2e%2e" is a ".." that
	// normalizes away and "%2f" is a separator, and
	// .../vol/x/%2e%2e/accumulo/tables/2/t-0001/A0000001.rf is the same
	// Path as .../vol/accumulo/tables/2/t-0001/A0000001.rf. pathIdentity
	// decodes escapes without resolving segments, so it would call those
	// two files distinct: as two inputs every cell would be merged
	// twice, and as output-and-input the output would land on a file
	// still being read.
	if decoded := decodeEscapes(rest[1:]); decoded != rest[1:] {
		for _, segment := range strings.Split(decoded, "/") {
			switch segment {
			case "":
				return refuse(ClassMalformedJob, field,
					"%q decodes to a path with an empty segment; Path.normalize would fold it onto another spelling", raw)
			case ".", "..":
				return refuse(ClassMalformedJob, field,
					"%q decodes to a %q segment; Path.normalize would fold it onto another spelling", raw, segment)
			}
		}
	}
	if len(segments) < 4 {
		return refuse(ClassMalformedJob, field,
			"%q is not shaped <volume>/%s/<tableId>/<tablet>/<file>", raw, hdfsTablesDirName)
	}
	if dir := segments[len(segments)-4]; dir != hdfsTablesDirName {
		return refuse(ClassMalformedJob, field,
			"%q: tables directory name is not %q, is %q", raw, hdfsTablesDirName, dir)
	}
	// parsePath splits uri.getPath(), which is decoded, but rebuilds and
	// compares against uri.toString(), which is not. So a percent-escape
	// anywhere in the four segments it reassembles makes the rebuilt
	// path a string that is not in the raw one, and lastIndexOf returns
	// -1. An escape in the volume is fine: that part is copied out of
	// the raw string and never decoded.
	for _, segment := range segments[len(segments)-4:] {
		if i := strings.IndexByte(segment, '%'); i >= 0 {
			return refuse(ClassMalformedJob, field,
				"%q escapes %q inside the path parsePath rebuilds; that rebuild is decoded and is compared against the raw URI, which would not contain it",
				raw, segment[i:min(i+3, len(segment))])
		}
	}
	if name := segments[len(segments)-1]; !validFileNameRE.MatchString(name) {
		return refuse(ClassMalformedJob, field,
			"%q: file name %q is empty or contains invalid characters", raw, name)
	}
	return nil
}

// uriPunctuation is the non-alphanumeric ASCII java.net.URI's
// single-argument parser accepts unescaped anywhere: RFC 2396's
// unreserved marks and its reserved set, less "?" and "#", which
// checkTabletFilePath has already refused as components a tablet file
// path does not have.
const uriPunctuation = "-_.!~*'();/:@&=+$,"

// checkURISyntax rejects what java.net.URI's single-argument parser
// rejects.
//
// StoredTabletFile.deserialize builds its path with
// new Path(URI.create(metadata.path)), and URI.create is the strict
// parser: an unescaped space, a stray control byte, a non-ASCII byte or
// a truncated percent-escape is a URISyntaxException, not something it
// quotes for the caller. Splitting on "/" without this would let such a
// path through as an executable plan for a file Accumulo cannot even
// name.
func checkURISyntax(raw string) error {
	authStart, authEnd := authoritySpan(raw)
	litStart, litEnd, err := checkAuthorityBrackets(raw, authStart, authEnd)
	if err != nil {
		return err
	}
	for i := 0; i < len(raw); i++ {
		if i >= litStart && i < litEnd {
			// Inside an IPv6 literal, which checkIPv6Literal has already
			// read under its own grammar. A "%" there opens a scope id,
			// not an escape: parseServer reaches the literal only after
			// parseAuthority has scanned the authority with
			// L_SERVER_PERCENT, which admits a bare "%".
			continue
		}
		c := raw[i]
		switch {
		case c == '%':
			if i+2 >= len(raw) || !isHexDigit(raw[i+1]) || !isHexDigit(raw[i+2]) {
				return fmt.Errorf("truncated or non-hex escape at offset %d", i)
			}
			i += 2
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
		case strings.IndexByte(uriPunctuation, c) >= 0:
		case c == '[' || c == ']':
			// The only legal bracket is one bounding the IPv6 literal
			// this loop skips over.
			return fmt.Errorf("character %q at offset %d is legal only around an IPv6 literal host",
				string(rune(c)), i)
		case c >= utf8.RuneSelf:
			// URI$Parser.scanEscape lets an unescaped character above
			// U+0080 through as long as it is visible, so a volume named
			// in a non-Latin script is legal and must not be refused.
			r, size := utf8.DecodeRuneInString(raw[i:])
			if r == utf8.RuneError && size == 1 {
				return fmt.Errorf("byte %#x at offset %d is not valid UTF-8", c, i)
			}
			if unicode.IsSpace(r) || isISOControl(r) {
				return fmt.Errorf("character %q at offset %d must be percent-escaped", string(r), i)
			}
			i += size - 1
		default:
			return fmt.Errorf("character %q at offset %d must be percent-escaped", string(rune(c)), i)
		}
	}
	return nil
}

// checkAuthorityBrackets enforces the one construction java.net.URI
// brackets: an IPv6 literal host.
//
// URI$Parser.parseServer takes "[" as the start of a literal, demands a
// matching "]", and runs the RFC 2373 grammar over what is between them
// (with an optional "%<scope-id>" tail). Nothing else in an authority
// may carry a bracket: REG_NAME has neither character, so parseAuthority
// cannot fall back to a registry-based authority and rethrows the
// literal's parse failure. So "hdfs://[not-ip]/..." and an unmatched
// "[" both throw out of URI.create — which is the call
// StoredTabletFile.deserialize makes — and a plan built on either would
// name a file the manager can never construct.
func checkAuthorityBrackets(raw string, start, end int) (litStart, litEnd int, err error) {
	authority := raw[start:end]
	host := 0
	// parseServer takes userinfo up to the *first* "@" and then parses
	// the host from there.
	if at := strings.IndexByte(authority, '@'); at >= 0 {
		host = at + 1
	}
	if i := strings.IndexAny(authority[:host], "[]"); i >= 0 {
		return 0, 0, fmt.Errorf("bracket at offset %d does not open an IPv6 literal host", start+i)
	}
	literal, found := strings.CutPrefix(authority[host:], "[")
	if !found {
		if i := strings.IndexAny(authority[host:], "[]"); i >= 0 {
			return 0, 0, fmt.Errorf("bracket at offset %d does not open an IPv6 literal host",
				start+host+i)
		}
		return 0, 0, nil
	}
	close := strings.IndexByte(literal, ']')
	if close < 0 {
		return 0, 0, errors.New("IPv6 literal host has no closing bracket")
	}
	if err := checkIPv6Literal(literal[:close]); err != nil {
		return 0, 0, err
	}
	// parseServer accepts only ":<digits>" after the literal, and
	// parseAuthority fails on anything left over.
	switch port := literal[close+1:]; {
	case port == "":
	case port[0] != ':':
		return 0, 0, fmt.Errorf("%q follows an IPv6 literal host; only a port may", port)
	default:
		for i := 1; i < len(port); i++ {
			if port[i] < '0' || port[i] > '9' {
				return 0, 0, fmt.Errorf("port %q after an IPv6 literal host is not numeric", port[1:])
			}
		}
	}
	litStart = start + host
	return litStart, litStart + close + 2, nil
}

// checkIPv6Literal applies URI$Parser.parseIPv6Reference: an IPv6
// address, optionally followed by "%" and a scope id of
// alpha | digit | "_" | ".". A bare IPv4 address is not one — the
// grammar admits IPv4 only as the tail of an IPv6 address, so the
// 4-byte count never reaches the 16 the parser requires.
func checkIPv6Literal(literal string) error {
	if literal == "" {
		return errors.New("IPv6 literal host is empty")
	}
	address := literal
	if pct := strings.IndexByte(literal, '%'); pct >= 0 {
		address = literal[:pct]
		switch scope := literal[pct+1:]; {
		case scope == "":
			return errors.New("IPv6 literal host ends in %, with no scope id")
		default:
			for i := 0; i < len(scope); i++ {
				c := scope[i]
				if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' ||
					c >= '0' && c <= '9' || c == '_' || c == '.') {
					return fmt.Errorf("IPv6 scope id %q contains %q", scope, string(rune(c)))
				}
			}
		}
	}
	parsed, err := netip.ParseAddr(address)
	if err != nil {
		return fmt.Errorf("%q is not an IPv6 address", address)
	}
	if parsed.Is4() {
		return fmt.Errorf("%q is an IPv4 address; URI brackets an IPv6 address only", address)
	}
	return nil
}

// isISOControl matches Character.isISOControl, which URI's parser uses
// to decide whether a non-ASCII character is visible enough to pass
// unescaped.
func isISOControl(r rune) bool {
	return r <= 0x1f || (r >= 0x7f && r <= 0x9f)
}

// authoritySpan locates the authority component, if the URI has one, as
// a half-open byte range. It returns an empty range when there is none,
// so no offset can fall inside it.
func authoritySpan(raw string) (start, end int) {
	colon := strings.Index(raw, ":")
	if colon < 0 || !strings.HasPrefix(raw[colon+1:], "//") {
		return 0, 0
	}
	start = colon + 3
	if i := strings.IndexByte(raw[start:], '/'); i >= 0 {
		return start, start + i
	}
	return start, len(raw)
}

func isHexDigit(c byte) bool {
	return c >= '0' && c <= '9' || c >= 'a' && c <= 'f' || c >= 'A' && c <= 'F'
}

// storedTabletFileMembers are the members TabletFileCqMetadataGson
// declares, spelled the way Gson binds them.
var storedTabletFileMembers = [...]string{"path", "startRow", "endRow"}

// checkEntryFields enforces the StoredTabletFile invariants a Go struct
// decode cannot express.
//
// StoredTabletFile.deserialize calls Objects.requireNonNull on path,
// startRow and endRow, so an entry missing either row field is not a
// whole-file entry — it is an entry Java refuses to parse at all. And
// the rows are byte arrays written through ByteArrayToBase64TypeAdapter
// (Base64.getUrlEncoder), so a value outside that alphabet is equally
// unparseable. Only a present, well-encoded, zero-length row means
// "unbounded on this side" (decodeRow returns null for an empty array).
//
// All three names are matched exactly, because Gson matches them
// exactly. encoding/json would otherwise accept {"Path":...} and
// {"StartRow":...} and hand back a plausible entry for JSON that leaves
// every one of those fields null on the Java side.
//
// An entry carrying both spellings is refused for the same reason from
// the other direction. Gson would bind the exact member and ignore the
// other; encoding/json falls back to a case-insensitive match and
// assigns whichever comes last, so {"startRow":"AWQ=","StartRow":""}
// is a fenced file to Java and a whole one to the struct decode below —
// the misreading that widens a compaction past the range the manager
// authorized. Requiring exactly one member per name makes the two
// decoders agree by construction.
//
// The decoded rows come back so the caller can identify the reference by
// path and range without decoding them a second time.
func checkEntryFields(entry, field string) (startRaw, endRaw []byte, err error) {
	members, err := jsonMembers(entry)
	if err != nil {
		return nil, nil, refuse(ClassMalformedJob, field,
			"undecodable StoredTabletFile entry: %v", err)
	}
	for name := range members {
		for _, bound := range storedTabletFileMembers {
			if name == bound || !strings.EqualFold(name, bound) {
				continue
			}
			// Without the exact member Gson binds nothing and
			// requireNonNull throws, which the presence checks below
			// report more precisely. It is the pair that diverges.
			if _, exact := members[bound]; !exact {
				continue
			}
			return nil, nil, refuse(ClassMalformedJob, field,
				"StoredTabletFile has %q alongside %q; Gson binds only the exact name, so the two decoders would disagree about this entry",
				name, bound)
		}
	}
	if member, ok := members["path"]; !ok || isJSONNull(member) {
		return nil, nil, refuse(ClassMalformedJob, field,
			"StoredTabletFile is missing path")
	}
	decoded := make([][]byte, 2)
	for i, name := range []string{"startRow", "endRow"} {
		member, ok := members[name]
		if !ok || isJSONNull(member) {
			return nil, nil, refuse(ClassMalformedJob, field,
				"StoredTabletFile is missing %s; a fence is absent only when the field is present and empty",
				name)
		}
		var value string
		if err := json.Unmarshal(member, &value); err != nil {
			return nil, nil, refuse(ClassMalformedJob, field,
				"StoredTabletFile %s is not a string: %v", name, err)
		}
		raw, err := checkFenceRow(value)
		if err != nil {
			return nil, nil, refuse(ClassMalformedJob, field,
				"StoredTabletFile %s %q: %v", name, value, err)
		}
		decoded[i] = raw
	}
	// deserialize builds Range(startRow, true, endRow, false), which
	// becomes Range(new Key(startRow), true, new Key(endRow), false) and
	// throws IllegalArgumentException("Start key must be less than end
	// key in range ...") when the end key sorts first. An inverted fence
	// is therefore not an input shoal has yet to port — it is one no Java
	// compactor could deserialize either, so it is malformed rather than
	// unsupported, and saying so is what tells an operator to look at
	// whoever wrote the metadata.
	//
	// Equal rows are left alone: Key(row) is not before Key(row), so Java
	// builds that range happily. It selects nothing, but it deserializes,
	// and pass 2 refuses every fence anyway.
	if decoded[0] != nil && decoded[1] != nil && bytes.Compare(decoded[0], decoded[1]) > 0 {
		return nil, nil, refuse(ClassMalformedJob, field,
			"StoredTabletFile startRow %q sorts after endRow %q; Range rejects a start key past its end",
			decoded[0], decoded[1])
	}
	return decoded[0], decoded[1], nil
}

// jsonMembers reads an entry as its literal members.
//
// Gson binds field names exactly; encoding/json falls back to a
// case-insensitive match, so a decode into a tagged struct accepts
// {"Path":...,"StartRow":...} — an entry whose fields Gson would leave
// null and whose deserialize would then fail on requireNonNull. Reading
// the members and looking them up by exact name is what keeps the two
// sides agreeing on which entries exist.
func jsonMembers(entry string) (map[string]json.RawMessage, error) {
	var members map[string]json.RawMessage
	if err := json.Unmarshal([]byte(entry), &members); err != nil {
		return nil, err
	}
	return members, nil
}

// isJSONNull reports whether a member is present but null, which Gson
// leaves as a null field and requireNonNull then rejects — the same
// outcome as omitting it.
func isJSONNull(member json.RawMessage) bool {
	return string(bytes.TrimSpace(member)) == "null"
}

// checkFenceRow accepts exactly the row encodings
// StoredTabletFile.decodeRow accepts.
//
// Two layers have to hold. The outer one is transport: the field is a
// byte array written through ByteArrayToBase64TypeAdapter
// (Base64.getUrlEncoder), so the string must be URL-safe base64 — the
// decoder tolerates the padding being present or absent.
//
// The inner one is framing, and it is the one a base64 check alone
// misses. decodeRow returns null only for a zero-length array; for
// anything else it hands the bytes to Hadoop Text.readFields, which
// reads a VInt length and then readFully's exactly that many bytes. So a
// row like "AQ==" — a length of one with nothing after it — is not an
// unsupported fence, it is an entry Java throws on, and reporting it as
// a fence would blame the wrong thing and hide a corrupt job.
func checkFenceRow(value string) ([]byte, error) {
	raw, err := decodeURLBase64(value)
	if err != nil {
		return nil, err
	}
	if len(raw) == 0 {
		// decodeRow: an empty array is how "unbounded on this side" is
		// spelled, and it never reaches Text.readFields.
		return nil, nil
	}
	return textPayload(raw)
}

// decodeURLBase64 accepts the encodings Base64.getUrlDecoder accepts:
// the URL-safe alphabet, with or without trailing padding.
func decodeURLBase64(value string) ([]byte, error) {
	if value == "" {
		return nil, nil
	}
	// Go's decoders skip CR and LF; Java's URL decoder has no such
	// leniency and throws on any character outside the alphabet, so
	// accepting them here would call a row Accumulo cannot deserialize
	// a supported-looking fence.
	if strings.ContainsAny(value, "\r\n") {
		return nil, errors.New("not url-safe base64: contains a line break")
	}
	if raw, err := base64.URLEncoding.DecodeString(value); err == nil {
		return raw, nil
	}
	raw, err := base64.RawURLEncoding.DecodeString(value)
	if err != nil {
		return nil, errors.New("not url-safe base64")
	}
	return raw, nil
}

// textPayload reads a Hadoop Text serialization the way Text.readFields
// consumes one — a VInt length followed by that many bytes — and returns
// the row it declares.
//
// Bytes past the declared length are ignored, because readFully never
// reads them: two entries that differ only there name the same row, so
// the row and not the buffer is what callers must compare.
func textPayload(raw []byte) ([]byte, error) {
	length, size, err := readVInt(raw)
	if err != nil {
		return nil, err
	}
	if length < 0 {
		// readWithKnownLength cannot allocate or read a negative count.
		return nil, fmt.Errorf("Text length %d is negative", length)
	}
	if available := int64(len(raw) - size); available < length {
		return nil, fmt.Errorf("Text declares %d byte(s) but only %d follow the length",
			length, available)
	}
	return raw[size : size+int(length)], nil
}

// readVInt mirrors WritableUtils.readVInt: readVLong's first byte
// encodes both the width and the sign, and a value outside int range is
// rejected outright.
func readVInt(raw []byte) (value int64, size int, err error) {
	if len(raw) == 0 {
		return 0, 0, errors.New("Text length is truncated")
	}
	first := int8(raw[0])
	size = decodeVIntSize(first)
	if size == 1 {
		return int64(first), 1, nil
	}
	if len(raw) < size {
		return 0, 0, fmt.Errorf("Text length needs %d bytes but only %d are present",
			size, len(raw))
	}
	var magnitude uint64
	for i := 1; i < size; i++ {
		magnitude = magnitude<<8 | uint64(raw[i])
	}
	value = int64(magnitude)
	if isNegativeVInt(first) {
		value = ^value
	}
	if value > math.MaxInt32 || value < math.MinInt32 {
		return 0, 0, fmt.Errorf("Text length %d does not fit in an int", value)
	}
	return value, size, nil
}

// decodeVIntSize mirrors WritableUtils.decodeVIntSize.
func decodeVIntSize(first int8) int {
	switch {
	case first >= -112:
		return 1
	case first < -120:
		return int(-119 - int32(first))
	default:
		return int(-111 - int32(first))
	}
}

// isNegativeVInt mirrors WritableUtils.isNegativeVInt.
func isNegativeVInt(first int8) bool {
	return first < -120 || (first >= -112 && first < 0)
}

// tmpSuffix renders the "_tmp_<ecid>" tail the coordinator appends to
// every external-compaction output name.
func tmpSuffix(ecid string) string { return "_tmp_" + ecid }

// parseOutput validates the destination file's structure.
//
// The name is not free-form. TabletNameGenerator.getNextDataFilenameForMajc
// builds it as getNextDataFilename(...) + "_tmp_" + ecid.canonical(),
// the coordinator puts exactly that in the job
// (CompactionCoordinator.createThriftJob passes
// ecm.getCompactTmpName().getNormalizedPathStr()), and the manager
// later strips at the first "_tmp" to derive the committed name
// (TabletNameGenerator.computeCompactionFileDest). A name that does not
// carry this job's own ECID therefore did not come from this
// assignment, and writing it would put bytes somewhere the manager will
// never look — or, worse, somewhere another compaction owns.
//
// The alias check runs first because it is the failure that destroys
// data rather than merely wasting it: an output that is also an input
// would truncate that input mid-read.
//
// The alias is checked against both names the file will have. Shoal
// writes the temp name, but the manager renames it to
// computeCompactionFileDest(out) at commit, so an output whose
// committed name collides with an input would have the manager
// overwrite a file this compaction is supposed to be replacing —
// after shoal has already reported success.
func parseOutput(job *tabletserver.TExternalCompactionJob, inputs []parsedInput, ecid string) (string, error) {
	out := job.GetOutputFile()
	if out == "" {
		return "", refuse(ClassMalformedJob, "outputFile", "missing")
	}
	if strings.HasPrefix(strings.TrimSpace(out), "{") {
		return "", refuse(ClassMalformedJob, "outputFile",
			"expected a plain path, got a StoredTabletFile entry: %s", out)
	}
	committed, hasTmp := committedName(out)
	outID := pathIdentity(out)
	committedID := pathIdentity(committed)
	for i, in := range inputs {
		inID := pathIdentity(in.file.Path)
		if inID == outID {
			return "", refuse(ClassMalformedJob, "outputFile",
				"output %s is also files[%d]", out, i)
		}
		if hasTmp && inID == committedID {
			return "", refuse(ClassMalformedJob, "outputFile",
				"output %s commits as %s, which is also files[%d]", out, committed, i)
		}
	}
	marker := tmpSuffix(ecid)
	if !strings.HasSuffix(out, marker) {
		return "", refuse(ClassMalformedJob, "outputFile",
			"%q is not this job's compaction temp name (expected a path ending in %q)",
			out, marker)
	}
	// computeCompactionFileDest truncates the whole normalized path at
	// the first "_tmp", not at the trailing marker, so an earlier one —
	// in a volume or tablet directory, or in the base name — would have
	// the manager rename this file to a destination shorter than the
	// name it asked for. The temp file would look right and the commit
	// would not.
	if idx := strings.Index(out, "_tmp"); idx != len(out)-len(marker) {
		return "", refuse(ClassMalformedJob, "outputFile",
			"%q contains %q before its own temp marker; the manager truncates at the first one and would commit as %q",
			out, "_tmp", out[:idx])
	}
	if err := checkTabletFilePath(out, "outputFile"); err != nil {
		return "", err
	}
	return out, nil
}

// committedName reproduces TabletNameGenerator.computeCompactionFileDest:
// checkOutputTable requires the output to be written under the table
// directory of the extent the job assigns.
//
// The coordinator does not invent this name: getNextDataFilenameForMajc
// builds it under chooseTabletDir, which is
// <volume>/tables/<extent.tableId()>/<dirName>. So an output naming a
// different table is a job whose own fields disagree, and executing it
// would write into another table's directory and have the manager
// commit the rename there.
//
// Only the output is checked. An input legitimately names another
// table: MetadataTableUtil.createCloneMutation copies the source
// tablet's file column qualifiers into the clone verbatim, so every
// tablet of a cloned table references files under the *source* table's
// directory until a compaction rewrites them. Requiring inputs to match
// would leave those tablets permanently uncompactable.
//
// out has already been through checkTabletFilePath, so its path holds at
// least four segments and the whole URI splits into at least five: the
// table id is the third from the end either way.
func checkOutputTable(out, tableID string) error {
	segments := strings.Split(out, "/")
	if named := segments[len(segments)-3]; named != tableID {
		return refuse(ClassMalformedJob, "outputFile",
			"output %s is under table %q, but the extent assigns table %q",
			out, named, tableID)
	}
	return nil
}

// committedName reproduces TabletNameGenerator.computeCompactionFileDest:
// the manager truncates the temp name at the *first* "_tmp", not at the
// trailing "_tmp_<ecid>", so a base that itself contains "_tmp" commits
// somewhere shorter than trimming the suffix would suggest. ok is false
// when there is no "_tmp" at a positive index, the case the manager
// rejects outright.
func committedName(out string) (name string, ok bool) {
	idx := strings.Index(out, "_tmp")
	if idx <= 0 {
		return "", false
	}
	return out[:idx], true
}

// checkOutputCapability gates the file format shoal is being asked to
// produce. The extension under the temp suffix comes from
// FileOperations.getNewFileExtension(table.file.type), which shoal does
// not otherwise get to see: the job carries only overrides, and
// table.file.type is not one. Accumulo ships only RFile today, so a
// different extension means a table configured for a format shoal's
// writer cannot emit, and producing an RFile under that name would hand
// the tablet a file it cannot read.
func checkOutputCapability(out, ecid string) error {
	base := strings.TrimSuffix(out, tmpSuffix(ecid))
	if !strings.HasSuffix(base, rfileExtension) {
		return refuse(ClassUnsupportedProperty, "outputFile",
			"%q names a %s file; shoal's writer emits RFiles only", out, outputExtension(base))
	}
	return checkVolumeCapability(out, "outputFile")
}

// checkVolumeCapability refuses a path java.net.URI accepts but shoal's
// storage backend cannot resolve.
//
// checkTabletFilePath models Java's parser, because that is what decides
// whether Accumulo could name the file at all. Opening it is a separate
// question, and a different parser answers it: internal/storage/hdfs's
// Backend.resolve hands the path to net/url.Parse, whose grammar is not
// java.net.URI's. A raw IPv6 scope id is the spelling that reaches here
// — URI.parseServer takes hdfs://[fe80::1%eth0]/... verbatim, because
// inside the brackets "%" introduces a scope id, while net/url reads
// "%et" as a truncated escape and fails. Without this the job would be
// declared executable and then die on its first read, with the slot
// already spent and no capability reported.
//
// Probing with the parser rather than modelling it keeps the two in
// step, and the scope id shows why that matters: net/url's handling of
// the percent-encoded spelling ("%25") is not even stable across Go
// releases — 1.25 accepts it, 1.26 rejects the empty zone it decodes to
// — so a model written against one toolchain would refuse work the
// running binary can do, or promise work it cannot. url.Parse always
// answers for the binary actually doing the opening.
//
// Parsing only proves the spelling, though, and a well-formed path can
// still name a volume shoal has no reader for. hdfs's Backend.resolve is
// the only shoal path resolver that interprets a URI scheme at all, and
// isHDFSScheme accepts exactly one; the object-store and local backends
// take keys, so handing them "ftp://nn/accumulo/tables/2/t/F.rf" makes
// them look for a file whose name is that whole string rather than the
// file Accumulo wrote. Neither outcome is a compaction, so the scheme is
// checked here too — otherwise the job is declared executable and dies
// on its first read with the slot already spent.
func checkVolumeCapability(raw, field string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return refuse(ClassUnsupportedVolume, field,
			"shoal's storage backend cannot parse this path: %v", err)
	}
	if !strings.EqualFold(u.Scheme, hdfsVolumeScheme) {
		return refuse(ClassUnsupportedVolume, field,
			"volume scheme %q has no shoal backend; only %q volumes can be read",
			u.Scheme, hdfsVolumeScheme)
	}
	return nil
}

// hdfsVolumeScheme is the scheme internal/storage/hdfs's isHDFSScheme
// accepts, and so the only one a tablet file path can carry and still be
// openable by shoal.
const hdfsVolumeScheme = "hdfs"

// rfileExtension is RFile.EXTENSION, the only file type shoal's reader
// and writer implement.
const rfileExtension = ".rf"

// outputExtension renders the extension in a refusal message, so an
// operator sees the format that was asked for rather than the whole path.
func outputExtension(base string) string {
	if i := strings.LastIndex(base, "."); i >= 0 && i+1 < len(base) {
		return base[i+1:]
	}
	return "extensionless"
}

// Table properties the compaction may override that shoal can honor when
// writing the output RFile.
const (
	propCompressType      = "table.file.compress.type"
	propCompressBlockSize = "table.file.compress.blocksize"
)

// ignorableOverrides are properties that change where/how the output file
// is *stored* by the filesystem, not what is inside it. Honoring them is
// the storage layer's job, and ignoring them cannot change a single cell,
// so they do not force the job onto a Java compactor.
var ignorableOverrides = map[string]bool{
	"table.file.blocksize":   true, // HDFS block size
	"table.file.replication": true, // HDFS replication factor
}

// cryptoOverridePrefixes mark on-disk-encryption configuration. shoal's
// RFile writer emits cleartext blocks, so any of these means the output
// must come from Java.
var cryptoOverridePrefixes = []string{
	"table.crypto.",
	"instance.crypto.",
	"general.custom.crypto.",
}

// accumuloCodecs maps Accumulo's table.file.compress.type values to
// shoal's BCFile codec names. Accumulo also ships zstd/lz4/bzip2/lzo;
// shoal's writer cannot produce those, and writing a differently
// compressed file than the table asks for would surprise both the
// operator and any size-based compaction planner, so they are refused.
var accumuloCodecs = map[string]string{
	"none":   block.CodecNone,
	"gz":     block.CodecGzip,
	"snappy": block.CodecSnappy,
}

// maxBlockSize caps table.file.compress.blocksize. The writer buffers a
// whole data block, and the value is operator-controlled, so an absurd
// setting is refused rather than trusted.
const maxBlockSize = 1 << 30 // 1 GiB

// parseOverrides checks the syntax of the job's property overrides and
// returns the keys in sorted order. Sorting is what makes a job with
// several unsupported overrides always report the same one: map
// iteration order would otherwise change the class an operator sees
// between identical jobs.
//
// Whether shoal can *honor* a property is a pass-2 question
// (resolveOutputEncoding); only a value that is not a legal property
// value at all is malformed here.
func parseOverrides(overrides map[string]string) ([]string, int64, error) {
	keys := make([]string, 0, len(overrides))
	for key := range overrides {
		keys = append(keys, key)
	}
	sort.Strings(keys)

	blockSize := int64(0)
	if raw, ok := overrides[propCompressBlockSize]; ok {
		n, err := parseMemoryBytes(raw)
		if err != nil {
			return nil, 0, refuse(ClassMalformedJob, "overrides["+propCompressBlockSize+"]", "%v", err)
		}
		blockSize = n
	}
	return keys, blockSize, nil
}

// resolveOutputEncoding turns the job's overrides into the output RFile's
// codec and block size. Unknown properties are refused rather than
// ignored: an override exists precisely because someone wanted this
// compaction to behave differently, and silently dropping it would
// produce an output that does not match the request.
//
// The resolved values are checked too, not just the overrides. A plan is
// a promise that compaction.Compact can run the job, so a codec or block
// size that only ever came from Options must fail here — as a refusal
// the coordinator can act on — rather than deep inside the writer after
// the slot has been taken.
func resolveOutputEncoding(
	overrides map[string]string,
	keys []string,
	blockSizeOverride int64,
	opts Options,
) (string, int, error) {
	codec := opts.DefaultCodec
	blockSize := opts.DefaultBlockSize

	for _, key := range keys {
		value := overrides[key]
		field := "overrides[" + key + "]"
		if isCryptoOverride(key) {
			return "", 0, refuse(ClassUnsupportedCrypto, field,
				"shoal writes cleartext RFiles; encrypted output must come from a Java compactor")
		}
		switch {
		case key == propCompressType:
			// Verbatim: Compression.getCompressionAlgorithmByName is a
			// plain map lookup keyed by each algorithm's getName(), so
			// "GZ" and " gz " are as unusable to Java as "zstd" is to
			// shoal, and accepting them here would run a compaction the
			// tablet server would have refused.
			mapped, ok := accumuloCodecs[value]
			if !ok {
				return "", 0, refuse(ClassUnsupportedProperty, field,
					"unsupported compression codec %q (shoal writes %s)", value, supportedCodecList())
			}
			codec = mapped
		case key == propCompressBlockSize:
			if blockSizeOverride <= 0 || blockSizeOverride > maxBlockSize {
				return "", 0, refuse(ClassUnsupportedProperty, field,
					"block size %d is outside the supported range (1..%d bytes)",
					blockSizeOverride, maxBlockSize)
			}
			blockSize = int(blockSizeOverride)
		case ignorableOverrides[key]:
			// Filesystem placement only — cannot change output cells.
		case key == embeddingspace.TableTargetProperty ||
			key == embeddingspace.JobFileStatesProperty ||
			key == embeddingspace.JobEpochProperty:
			// Embedding-space carriage, validated by
			// resolveEmbeddingOverrides. Listed here only so the
			// exhaustive default below does not reject a job for
			// carrying a property shoal itself defined; neither key
			// changes the output's codec or block size.
		default:
			return "", 0, refuse(ClassUnsupportedProperty, field,
				"shoal cannot honor this override when writing the output RFile")
		}
	}
	// Only an Options-sourced value can still be bad: every override
	// above was validated before it was assigned.
	if codec != "" && !supportedOutputCodecs[codec] {
		return "", 0, refuse(ClassUnsupportedProperty, "options.defaultCodec",
			"configured default codec %q is not one of %s", codec, supportedCodecList())
	}
	if blockSize < 0 || int64(blockSize) > maxBlockSize {
		return "", 0, refuse(ClassUnsupportedProperty, "options.defaultBlockSize",
			"configured default block size %d is outside the supported range (0..%d bytes, 0 for the writer default)",
			blockSize, maxBlockSize)
	}
	return codec, blockSize, nil
}

// supportedOutputCodecs is the set of codec names a Plan may carry,
// derived from accumuloCodecs so the two cannot drift apart.
var supportedOutputCodecs = func() map[string]bool {
	set := make(map[string]bool, len(accumuloCodecs))
	for _, codec := range accumuloCodecs {
		set[codec] = true
	}
	return set
}()

func supportedCodecList() string {
	names := make([]string, 0, len(supportedOutputCodecs))
	for name := range supportedOutputCodecs {
		names = append(names, name)
	}
	sort.Strings(names)
	return strings.Join(names, ", ")
}

func isCryptoOverride(key string) bool {
	for _, prefix := range cryptoOverridePrefixes {
		if strings.HasPrefix(key, prefix) {
			return true
		}
	}
	return false
}

// parseMemoryBytes mirrors Java's
// ConfigurationTypeHelper.getFixedMemoryAsBytes: an optional B/K/M/G
// suffix (case-insensitive, via Character.toUpperCase) scales a decimal
// value by 2^0/2^10/2^20/2^30; no suffix means bytes.
//
// The value is parsed verbatim. Java reads the last character of the
// raw string and hands the rest to Long.parseLong, which rejects
// surrounding whitespace, so trimming here would make " 128K" a
// property shoal honors and the tablet server does not.
func parseMemoryBytes(raw string) (int64, error) {
	s := raw
	if s == "" {
		return 0, errors.New("empty memory value")
	}
	shift := uint(0)
	switch s[len(s)-1] {
	case 'B', 'b':
		shift = 0
		s = s[:len(s)-1]
	case 'K', 'k':
		shift = 10
		s = s[:len(s)-1]
	case 'M', 'm':
		shift = 20
		s = s[:len(s)-1]
	case 'G', 'g':
		shift = 30
		s = s[:len(s)-1]
	}
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("%q is not a memory size: %w", raw, err)
	}
	if shift > 0 && (n > math.MaxInt64>>shift || n < math.MinInt64>>shift) {
		return 0, fmt.Errorf("%q overflows int64", raw)
	}
	return n << shift, nil
}

// systemIterators are stack entries shoal's composer installs itself
// (internal/compaction wraps the merge in a DeletingIterator, and
// visibility filtering is a scan-scope concern). A job that also lists
// one would double-apply it, so it is refused instead of silently
// deduplicated.
var systemIterators = map[string]bool{
	iterrt.IterDeleting:   true,
	iterrt.IterVisibility: true,
}

// rawIterator is one structurally valid stack entry, before it is mapped
// onto a shoal iterator.
type rawIterator struct {
	name     string
	class    string
	field    string
	priority int32
	options  map[string]string
}

// parseIterators checks job.iteratorSettings for internal consistency,
// over the whole list, before any entry is mapped: an unported class in
// the first entry must not hide a malformed second one.
//
// The stack is then ordered the way Java orders it, which is *not* wire
// order: IteratorConfigUtil.parseIterConf sorts by ITER_INFO_COMPARATOR
// (priority, then iterator name, a null name sorting as "") before
// loadIterators builds the stack bottom-up, so an unsorted or
// equal-priority list is a job Java runs happily rather than a malformed
// one. Sorting here is what makes the plan match; taking wire order
// would build a different stack than Java from the same job.
//
// The sort is stable and by (priority, name), and names are unique by
// the check below, so it is total: two runs over the same job always
// produce the same stack. The name comparison is compareUTF16, not Go's
// byte order, because the names come from user configuration
// (IteratorSetting) rather than from a fixed set.
func parseIterators(job *tabletserver.TExternalCompactionJob) ([]rawIterator, error) {
	// Compactor.initialize runs
	// job.getIteratorSettings().getIterators().forEach(...) with no
	// guard, so both containers are dereferenced before any file is
	// read: a job missing either is one the reference implementation
	// throws on, not one with an empty stack. Only a present, empty list
	// means "no iterators". Thrift decodes a present empty list into a
	// non-nil slice, so the shapes are distinguishable on the wire.
	if job.GetIteratorSettings() == nil {
		return nil, refuse(ClassMalformedJob, "iteratorSettings",
			"missing; Compactor.initialize dereferences it before reading any file")
	}
	settings := job.GetIteratorSettings().GetIterators()
	if settings == nil {
		return nil, refuse(ClassMalformedJob, "iteratorSettings.iterators",
			"missing; Compactor.initialize iterates it before reading any file")
	}
	if len(settings) == 0 {
		return nil, nil
	}

	out := make([]rawIterator, 0, len(settings))
	seen := make(map[string]int, len(settings))
	for i, s := range settings {
		field := fmt.Sprintf("iteratorSettings[%d]", i)
		if s == nil {
			return nil, refuse(ClassMalformedJob, field, "nil iterator setting")
		}
		// The constructor SystemIteratorUtil.toIteratorSetting calls
		// validates in this order, and every one of these checks runs
		// while Compactor.initialize is still assembling the stack, so a
		// job that fails any of them is one the reference compactor
		// throws on before it reads a file.
		if s.GetPriority() <= 0 {
			return nil, refuse(ClassMalformedJob, field,
				"priority %d is not strictly positive, which IteratorSetting.setPriority rejects",
				s.GetPriority())
		}
		name, class := s.GetName(), s.GetIteratorClass()
		if name == "" {
			return nil, refuse(ClassMalformedJob, field, "iterator has no name")
		}
		if strings.Contains(name, ".") {
			// Reported against the index rather than the name: a dotted
			// name is exactly what would make the field path ambiguous.
			return nil, refuse(ClassMalformedJob, field,
				"name %q contains a dot, which IteratorSetting.setName rejects", name)
		}
		field = "iteratorSettings." + name
		if class == "" {
			return nil, refuse(ClassMalformedJob, field, "iterator has no class")
		}
		if prev, dup := seen[name]; dup {
			return nil, refuse(ClassMalformedJob, field,
				"duplicates iteratorSettings[%d]", prev)
		}
		seen[name] = i

		if s.GetProperties() == nil {
			// SystemIteratorUtil.toIteratorSetting passes this map
			// straight to IteratorSetting's four-argument constructor,
			// which calls addOptions, which opens with
			// checkArgument(properties != null). Compactor.initialize
			// builds the whole stack that way before it reads a file, so
			// a job missing this map is one the reference implementation
			// throws on rather than one with an option-less iterator.
			return nil, refuse(ClassMalformedJob, field+".properties",
				"missing; IteratorSetting rejects a null option map")
		}
		// Copied, not aliased: the plan must not change if the transport
		// reuses or recycles the job struct.
		options := make(map[string]string, len(s.GetProperties()))
		for k, v := range s.GetProperties() {
			options[k] = v
		}
		out = append(out, rawIterator{
			name:     name,
			class:    class,
			field:    field,
			priority: s.GetPriority(),
			options:  options,
		})
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].priority != out[j].priority {
			return out[i].priority < out[j].priority
		}
		return compareUTF16(out[i].name, out[j].name) < 0
	})
	return out, nil
}

// compareUTF16 orders two strings the way java.lang.String.compareTo
// does: by UTF-16 code unit. Go's own comparison is by UTF-8 byte, which
// equals code-point order, and the two disagree exactly when one string
// reaches a supplementary character (>= U+10000, encoded in UTF-16 as a
// surrogate pair in D800-DFFF) where the other has a character in
// E000-FFFF. Go sorts the supplementary character higher; Java sorts it
// lower, because its surrogates are numerically below E000.
//
// Iterator names come from user configuration, so that case is reachable
// and reversing two equal-priority iterators reorders the stack — which
// changes cells. Comparing encoded code units removes the divergence
// instead of assuming the names stay ASCII.
func compareUTF16(a, b string) int {
	if a == b {
		return 0
	}
	ua, ub := utf16.Encode([]rune(a)), utf16.Encode([]rune(b))
	for i := 0; i < len(ua) && i < len(ub); i++ {
		if ua[i] != ub[i] {
			return int(ua[i]) - int(ub[i])
		}
	}
	return len(ua) - len(ub)
}

// mapIterators resolves each entry onto shoal's iterator registry, then
// builds the stack once against an empty source: that runs every
// iterator's Init, which is where iterrt validates options, so a bad
// option is refused before any input is read instead of failing
// mid-compaction.
//
// raw arrives in execution order (parseIterators sorted it), so the
// unsupported iterator this reports is the first one the compaction
// would have reached.
func mapIterators(raw []rawIterator, fullMajor bool) ([]Iterator, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	out := make([]Iterator, 0, len(raw))
	for _, r := range raw {
		alias, ok := itercfg.ClassAllowlist[r.class]
		if !ok {
			return nil, refuse(ClassUnsupportedIterator, r.field,
				"%s has no shoal port; the compaction would silently drop its behaviour", r.class)
		}
		if systemIterators[alias] {
			return nil, refuse(ClassUnsupportedIterator, r.field,
				"%s is a system iterator shoal's compaction composer installs itself; "+
					"running it twice would change the output", r.class)
		}
		out = append(out, Iterator{
			Name:     r.name,
			Class:    r.class,
			Priority: r.priority,
			Spec:     iterrt.IterSpec{Name: alias, Options: r.options},
		})
	}

	if err := validateStack(out, fullMajor); err != nil {
		return nil, err
	}
	return out, nil
}

// validateStack builds the translated stack over an empty source so every
// iterator's Init — and therefore its option parsing — runs now.
func validateStack(iters []Iterator, fullMajor bool) error {
	specs := make([]iterrt.IterSpec, 0, len(iters))
	for _, it := range iters {
		specs = append(specs, it.Spec)
	}
	env := iterrt.IteratorEnvironment{
		Scope:               iterrt.ScopeMajc,
		FullMajorCompaction: fullMajor,
	}
	if _, err := iterrt.BuildStack(iterrt.NewSliceSource(nil), specs, env); err != nil {
		return refuse(ClassUnsupportedIterator, "iteratorSettings",
			"shoal cannot build this stack: %v", err)
	}
	return nil
}
