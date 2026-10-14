// Package compactexec executes a translated external-compaction plan without
// owning coordinator polling or metadata authority.
package compactexec

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/phrocker/shoal-oss/internal/compaction"
	"github.com/phrocker/shoal-oss/internal/compactjob"
	"github.com/phrocker/shoal-oss/internal/embeddingspace"
	"github.com/phrocker/shoal-oss/internal/storage"
	"github.com/phrocker/shoal-oss/internal/thrift/gen/data"
)

// Store is the durable file boundary used by an external compaction.
// Write must not return until the output is published durably at path.
type Store interface {
	Read(context.Context, string) ([]byte, error)
	Write(context.Context, string, []byte) error
	Remove(context.Context, string) error
}

// Composer is injectable so execution failures and cancellation are testable.
type Composer interface {
	Compact(context.Context, compaction.Spec, func(compaction.Progress)) (*compaction.Result, error)
}

type nativeComposer struct{}

func (nativeComposer) Compact(ctx context.Context, spec compaction.Spec, observe func(compaction.Progress)) (*compaction.Result, error) {
	return compaction.CompactContext(ctx, spec, observe)
}

// Phase identifies a monotonic executor lifecycle transition.
type Phase string

const (
	PhaseRecovering Phase = "recovering"
	PhaseReading    Phase = "reading"
	PhaseCompacting Phase = "compacting"
	PhasePublishing Phase = "publishing"
	PhaseCompleted  Phase = "completed"
	PhaseCleaning   Phase = "cleaning"
)

// Progress is safe to expose through Accumulo's status RPC.
type Progress struct {
	Phase                Phase
	InputFilesRead       int
	InputFilesTotal      int
	EntriesToBeCompacted int64
	EntriesRead          int64
	EntriesWritten       int64
	Age                  time.Duration
}

// Reporter receives best-effort progress. Reporter errors never invalidate a
// correct compaction output.
type Reporter interface {
	Report(context.Context, Progress) error
}

// RetryPolicy controls recoverable storage operations.
type RetryPolicy struct {
	Attempts int
	Backoff  time.Duration
}

// Options configures an Executor.
type Options struct {
	Retry                RetryPolicy
	CleanupTimeout       time.Duration
	ProgressEveryEntries int64
	Reporter             Reporter
	Logger               *slog.Logger
	Now                  func() time.Time
	Sleep                func(context.Context, time.Duration) error

	// Converger, when non-nil, lets a compaction converge its inputs
	// toward the table's target embedding space. Nil — the default —
	// leaves every output carrying the space its inputs agreed on, which
	// is what a compactor without an embedding provider must do.
	//
	// A Converger shared across concurrently executing jobs must be safe
	// for concurrent use; the Executor makes no attempt to serialise it,
	// because the throttle and budget it enforces are global to a
	// migration rather than per job.
	Converger compaction.Converger
}

// Executor is stateless and safe for concurrent use when its Store,
// Composer, and Reporter are safe for concurrent use.
type Executor struct {
	store    Store
	composer Composer
	opts     Options
}

// New creates an executor using the native iterator/compaction stack.
func New(store Store, opts Options) (*Executor, error) {
	return NewWithComposer(store, nativeComposer{}, opts)
}

// NewWithComposer creates an executor with an injected composer.
func NewWithComposer(store Store, composer Composer, opts Options) (*Executor, error) {
	if store == nil {
		return nil, fmt.Errorf("compactexec: store is required")
	}
	if composer == nil {
		return nil, fmt.Errorf("compactexec: composer is required")
	}
	if opts.Retry.Attempts <= 0 {
		opts.Retry.Attempts = 3
	}
	if opts.Retry.Backoff < 0 {
		return nil, fmt.Errorf("compactexec: retry backoff must not be negative")
	}
	if opts.CleanupTimeout < 0 {
		return nil, fmt.Errorf("compactexec: cleanup timeout must not be negative")
	}
	if opts.CleanupTimeout == 0 {
		opts.CleanupTimeout = 15 * time.Second
	}
	if opts.ProgressEveryEntries <= 0 {
		opts.ProgressEveryEntries = 10_000
	}
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.Sleep == nil {
		opts.Sleep = sleepContext
	}
	return &Executor{store: store, composer: composer, opts: opts}, nil
}

// Stats is the complete executor result used by the coordinator completion
// adapter and local metrics.
type Stats struct {
	EntriesRead    int64
	EntriesWritten int64
	FileSize       int64
	InputFiles     int
	StartedAt      time.Time
	FinishedAt     time.Time
	Duration       time.Duration
}

// Result describes a durable, uncommitted output. The manager remains solely
// responsible for renaming it and replacing the tablet's input references.
type Result struct {
	ECID       string
	Extent     *data.TKeyExtent
	OutputFile string
	Stats      Stats

	// EmbeddingSpace is the space the published output is labelled with.
	// It is what the caller must persist into the file.embedding column
	// family for the new file.
	EmbeddingSpace embeddingspace.FileState

	// Converged reports whether this compaction actually re-embedded its
	// inputs, as opposed to preserving whatever space they already had.
	Converged bool

	// EmbeddingEpoch is the migration snapshot this output belongs to,
	// and is set only when Converged. It is what lets a coordinator
	// attribute a converged file to the migration that produced it and
	// discard a completion that belongs to a superseded epoch.
	EmbeddingEpoch string
}

// Execute recovers the job's exact temporary output path, reads all inputs,
// executes the translated iterator stack, and durably publishes the output.
func (e *Executor) Execute(ctx context.Context, plan *compactjob.Plan) (*Result, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := validatePlan(plan); err != nil {
		return nil, err
	}
	started := e.opts.Now()
	progress := Progress{
		InputFilesTotal:      len(plan.Inputs),
		EntriesToBeCompacted: plan.TotalInputEntries,
	}
	e.report(ctx, started, &progress, PhaseRecovering)
	if err := e.Recover(ctx, plan); err != nil {
		return nil, err
	}

	inputs := make([]compaction.Input, 0, len(plan.Inputs))
	for _, input := range plan.Inputs {
		var image []byte
		err := e.retry(ctx, func() error {
			var err error
			image, err = e.store.Read(ctx, input.Path)
			return err
		})
		if err != nil {
			return nil, fmt.Errorf("compactexec: read input %s: %w", input.Path, err)
		}
		if int64(len(image)) != input.Size {
			return nil, fmt.Errorf("compactexec: input %s size %d does not match job size %d", input.Path, len(image), input.Size)
		}
		inputs = append(inputs, compaction.Input{
			Name:              input.Entry,
			Bytes:             image,
			MetadataEmbedding: input.Embedding,
		})
		progress.InputFilesRead++
		progress.EntriesRead += input.Entries
		e.report(ctx, started, &progress, PhaseReading)
	}

	var lastReported int64
	e.report(ctx, started, &progress, PhaseCompacting)
	compacted, err := e.composer.Compact(ctx, plan.SpecWithConverger(inputs, e.opts.Converger), func(p compaction.Progress) {
		progress.EntriesWritten = p.EntriesWritten
		if p.EntriesWritten-lastReported >= e.opts.ProgressEveryEntries {
			lastReported = p.EntriesWritten
			e.report(ctx, started, &progress, PhaseCompacting)
		}
	})
	if err != nil {
		return nil, fmt.Errorf("compactexec: compact %s: %w", plan.ECID, err)
	}
	if compacted == nil {
		return nil, fmt.Errorf("compactexec: compact %s returned no result", plan.ECID)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	progress.EntriesWritten = compacted.EntriesWritten
	e.report(ctx, started, &progress, PhasePublishing)
	err = e.retry(ctx, func() error {
		if err := e.store.Write(ctx, plan.OutputFile, compacted.Output); err != nil {
			_ = e.cleanupOutput(ctx, plan.OutputFile)
			return err
		}
		return nil
	})
	if err != nil {
		_ = e.cleanupOutput(ctx, plan.OutputFile)
		return nil, fmt.Errorf("compactexec: publish output %s: %w", plan.OutputFile, err)
	}
	if err := ctx.Err(); err != nil {
		_ = e.cleanupOutput(ctx, plan.OutputFile)
		return nil, err
	}

	finished := e.opts.Now()
	e.report(ctx, started, &progress, PhaseCompleted)
	return &Result{
		ECID:       plan.ECID,
		Extent:     cloneExtent(plan.Extent),
		OutputFile: plan.OutputFile,
		Stats: Stats{
			EntriesRead:    progress.EntriesRead,
			EntriesWritten: compacted.EntriesWritten,
			FileSize:       int64(len(compacted.Output)),
			InputFiles:     len(inputs),
			StartedAt:      started,
			FinishedAt:     finished,
			Duration:       finished.Sub(started),
		},
		EmbeddingSpace: compacted.EmbeddingSpace,
		Converged:      compacted.Converged,
		EmbeddingEpoch: compacted.EmbeddingEpoch,
	}, nil
}

// Recover removes an uncommitted output left at this job's unique temporary
// path before a fresh attempt.
func (e *Executor) Recover(ctx context.Context, plan *compactjob.Plan) error {
	if plan == nil || plan.OutputFile == "" {
		return fmt.Errorf("compactexec: recovery requires an output path")
	}
	if err := e.remove(ctx, plan.OutputFile); err != nil {
		return fmt.Errorf("compactexec: recover output %s: %w", plan.OutputFile, err)
	}
	return nil
}

// Cleanup removes a durable output when completion was definitively rejected
// or the job was cancelled. It must not be used for an ambiguous completion
// RPC timeout: the same completion request should be retried instead.
func (e *Executor) Cleanup(ctx context.Context, result *Result) error {
	if result == nil || result.OutputFile == "" {
		return fmt.Errorf("compactexec: cleanup requires a result")
	}
	e.report(ctx, result.Stats.StartedAt, &Progress{}, PhaseCleaning)
	if err := e.remove(ctx, result.OutputFile); err != nil {
		return fmt.Errorf("compactexec: cleanup output %s: %w", result.OutputFile, err)
	}
	return nil
}

func (e *Executor) remove(ctx context.Context, path string) error {
	err := e.retry(ctx, func() error { return e.store.Remove(ctx, path) })
	if errors.Is(err, storage.ErrNotFound) {
		return nil
	}
	return err
}

func (e *Executor) cleanupOutput(parent context.Context, path string) error {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(parent), e.opts.CleanupTimeout)
	defer cancel()
	return e.remove(ctx, path)
}

func (e *Executor) retry(ctx context.Context, fn func() error) error {
	var err error
	for attempt := 1; attempt <= e.opts.Retry.Attempts; attempt++ {
		if err = ctx.Err(); err != nil {
			return err
		}
		if err = fn(); err == nil {
			return nil
		}
		if attempt < e.opts.Retry.Attempts {
			if sleepErr := e.opts.Sleep(ctx, e.opts.Retry.Backoff*time.Duration(attempt)); sleepErr != nil {
				return errors.Join(err, sleepErr)
			}
		}
	}
	return err
}

func (e *Executor) report(ctx context.Context, started time.Time, p *Progress, phase Phase) {
	if e.opts.Reporter == nil {
		return
	}
	snapshot := *p
	snapshot.Phase = phase
	snapshot.Age = e.opts.Now().Sub(started)
	if err := e.opts.Reporter.Report(ctx, snapshot); err != nil {
		e.opts.Logger.Warn("compaction progress report failed",
			slog.String("phase", string(phase)), slog.String("err", err.Error()))
	}
}

func validatePlan(plan *compactjob.Plan) error {
	switch {
	case plan == nil:
		return fmt.Errorf("compactexec: plan is required")
	case plan.ECID == "":
		return fmt.Errorf("compactexec: plan ECID is required")
	case plan.Extent == nil:
		return fmt.Errorf("compactexec: plan extent is required")
	case len(plan.Inputs) == 0:
		return fmt.Errorf("compactexec: plan has no inputs")
	case plan.OutputFile == "":
		return fmt.Errorf("compactexec: plan output is required")
	}
	return nil
}

func cloneExtent(ex *data.TKeyExtent) *data.TKeyExtent {
	if ex == nil {
		return nil
	}
	return &data.TKeyExtent{
		Table:      append([]byte(nil), ex.GetTable()...),
		EndRow:     append([]byte(nil), ex.GetEndRow()...),
		PrevEndRow: append([]byte(nil), ex.GetPrevEndRow()...),
	}
}

func sleepContext(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return nil
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
