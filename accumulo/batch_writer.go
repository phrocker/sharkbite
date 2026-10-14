package accumulo

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/phrocker/shoal-oss/internal/ingestclient"
	"github.com/phrocker/shoal-oss/internal/thrift/gen/data"
)

const (
	defaultBatchWriterMaxMemoryBytes  int64 = 50 << 20
	defaultBatchWriterMaxBatchBytes   int64 = 128 << 10
	defaultBatchWriterMaxWriteThreads       = 3
	defaultBatchWriterMaxRetries            = 3
	defaultBatchWriterRetryBackoff          = 100 * time.Millisecond
	batchWriterCleanupTimeout               = 5 * time.Second
)

var (
	// ErrBatchWriterClosed indicates that Add or Flush was called after Close.
	ErrBatchWriterClosed = errors.New("accumulo: batch writer is closed")

	// ErrBatchWriterFailed indicates that a submission may have partially
	// committed. The writer cannot safely retry or accept more mutations.
	ErrBatchWriterFailed = errors.New("accumulo: batch writer failed")

	// ErrBatchWriterRetryExhausted indicates that the bounded retries for a
	// provably safe failure were exhausted.
	ErrBatchWriterRetryExhausted = errors.New("accumulo: batch writer retry limit exhausted")

	// ErrBatchWriterAutoFlush indicates that an automatic latency flush
	// failed. The writer becomes terminal and subsequent operations return
	// the same error.
	ErrBatchWriterAutoFlush = errors.New("accumulo: automatic batch writer flush failed")
)

// Durability selects the tablet server's write-ahead-log behavior.
type Durability uint8

const (
	DurabilityDefault Durability = iota
	DurabilitySync
	DurabilityFlush
	DurabilityLog
	DurabilityNone
)

// BatchWriterOptions configures a BatchWriter.
type BatchWriterOptions struct {
	// MaxMemoryBytes bounds buffered encoded mutation bytes. Zero uses 50 MiB.
	// A single larger mutation is accepted and submitted synchronously.
	MaxMemoryBytes int64

	// MaxBatchBytes bounds each applyUpdates mutation batch. Zero uses 128 KiB.
	MaxBatchBytes int64

	// MaxLatency bounds how long the oldest buffered mutation waits before an
	// automatic flush. Later Add calls do not extend its deadline. Zero
	// disables automatic flushing.
	MaxLatency time.Duration

	// MaxWriteThreads bounds concurrent tablet-server submissions. Each
	// tablet server is handled by only one worker per attempt. Zero uses three.
	MaxWriteThreads int

	// MaxRetries bounds additional attempts after the initial submission.
	// Zero uses three retries.
	MaxRetries int

	// RetryBackoff is the fixed delay between safe retries. Zero uses 100 ms.
	RetryBackoff time.Duration

	Durability Durability
}

type normalizedBatchWriterOptions struct {
	maxMemoryBytes  int64
	maxBatchBytes   int64
	maxLatency      time.Duration
	maxWriteThreads int
	maxRetries      int
	retryBackoff    time.Duration
	durability      ingestclient.Durability
}

// FailedExtent reports an Accumulo 4 failed extent and the committed prefix
// returned by closeUpdate.
type FailedExtent struct {
	Extent    TabletExtent
	Submitted int
	Committed int64
}

// ConstraintViolation reports one server-side mutation constraint failure.
type ConstraintViolation struct {
	ConstraintClass            string
	ViolationCode              int16
	Description                string
	NumberOfViolatingMutations int64
}

// AuthorizationFailure reports a server-side authorization rejection.
type AuthorizationFailure struct {
	Extent TabletExtent
	Code   string
}

// MutationRejectionError is the Go-native view of Accumulo closeUpdate
// failures. Generated Thrift types remain internal.
type MutationRejectionError struct {
	Server                string
	FailedExtents         []FailedExtent
	ConstraintViolations  []ConstraintViolation
	AuthorizationFailures []AuthorizationFailure
}

func (e *MutationRejectionError) Error() string {
	return fmt.Sprintf(
		"accumulo: tablet server %s rejected mutations: %d failed extents, %d constraint violations, %d authorization failures",
		e.Server,
		len(e.FailedExtents),
		len(e.ConstraintViolations),
		len(e.AuthorizationFailures),
	)
}

// BatchWriterCleanupError reports that a failed update session could not be
// conclusively cancelled.
type BatchWriterCleanupError struct {
	Server string
	Err    error
}

func (e *BatchWriterCleanupError) Error() string {
	return fmt.Sprintf("accumulo: cancel update session on %s: %v", e.Server, e.Err)
}

// Unwrap returns the update-session cleanup failure.
func (e *BatchWriterCleanupError) Unwrap() error { return e.Err }

type batchWriterSession interface {
	Apply(context.Context, *data.TKeyExtent, []*data.TMutation) error
	Close(context.Context) (*data.UpdateErrors, error)
	Cancel(context.Context) (bool, error)
}

type bufferedMutation struct {
	row  []byte
	wire *data.TMutation
	size int64
}

type extentPlan struct {
	extent    *data.TKeyExtent
	mutations []bufferedMutation
}

type serverPlan struct {
	address string
	extents []extentPlan
	indexes map[tabletExtentMapKey]int
}

type batchWriterRoutingError struct {
	row []byte
	err error
}

func (e *batchWriterRoutingError) Error() string { return e.err.Error() }
func (e *batchWriterRoutingError) Unwrap() error { return e.err }

type serverSendResult struct {
	retry          []bufferedMutation
	invalidateRows [][]byte
	evidence       error
	submitted      bool
}

type serverSendOutcome struct {
	result serverSendResult
	err    error
}

type mutationUpdateResult struct {
	retry          []bufferedMutation
	invalidateRows [][]byte
	rejection      *MutationRejectionError
	terminal       bool
}

// BatchWriter buffers mutations for one table and submits them through
// Accumulo 4 update sessions. Operations are serialized; concurrent callers
// waiting for the writer honor their contexts. Independent tablet servers are
// submitted concurrently up to MaxWriteThreads, while each server plan remains
// ordered and single-threaded. When MaxLatency is enabled, one writer-owned
// goroutine and timer are stopped and joined by Close.
type BatchWriter struct {
	connector *Connector
	table     Table
	options   normalizedBatchWriterOptions

	gate chan struct{}

	pending      []bufferedMutation
	pendingBytes int64
	pendingSince time.Time
	failure      error
	closed       bool
	closeErr     error

	autoFlushWake   chan struct{}
	autoFlushCancel context.CancelFunc
	autoFlushDone   chan struct{}

	startSession func(context.Context, string, ingestclient.Durability) (batchWriterSession, error)
}

// NewBatchWriter constructs a bounded writer for table.
func (c *Connector) NewBatchWriter(
	table Table,
	options BatchWriterOptions,
) (*BatchWriter, error) {
	if c == nil {
		return nil, errors.New("accumulo: connector is nil")
	}
	if table.ID == "" && table.Name == "" {
		return nil, fmt.Errorf("%w: batch writer table identity is empty", ErrTableNotFound)
	}
	normalized, err := normalizeBatchWriterOptions(options)
	if err != nil {
		return nil, err
	}
	if _, err := c.discoveryState(); err != nil {
		return nil, err
	}
	writer := &BatchWriter{
		connector: c,
		table: Table{
			Name: table.Name,
			ID:   table.ID,
		},
		options: normalized,
		gate:    make(chan struct{}, 1),
	}
	writer.gate <- struct{}{}
	writer.startSession = func(
		ctx context.Context,
		address string,
		durability ingestclient.Durability,
	) (batchWriterSession, error) {
		return c.ingest.Start(ctx, address, durability)
	}
	writer.startAutomaticFlush()
	return writer, nil
}

// Add defensively snapshots mutation. When the configured memory bound is
// reached, Add synchronously submits buffered mutations and applies
// backpressure to the caller. An error before submission means mutation was
// not accepted; ErrBatchWriterFailed means it may have committed. A prior
// automatic flush error is terminal and returned unchanged.
func (w *BatchWriter) Add(ctx context.Context, mutation *Mutation) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if mutation == nil {
		return errors.New("accumulo: mutation is nil")
	}
	if err := w.lock(ctx); err != nil {
		return err
	}
	defer w.unlock()

	if w.closed {
		return ErrBatchWriterClosed
	}
	if w.failure != nil {
		return w.failure
	}
	if mutation.Size() == 0 {
		return errors.New("accumulo: mutation must contain at least one update")
	}
	wire, err := mutation.toThrift()
	if err != nil {
		return err
	}
	buffered := bufferedMutation{
		row:  cloneRow(wire.Row),
		wire: wire,
		size: mutationWireSize(wire),
	}

	if w.pendingBytes > 0 &&
		w.pendingBytes+buffered.size > w.options.maxMemoryBytes {
		if err := w.flushLocked(ctx); err != nil {
			return err
		}
	}
	previousLen := len(w.pending)
	previousBytes := w.pendingBytes
	previousSince := w.pendingSince
	startedBuffer := previousLen == 0
	if startedBuffer {
		w.pendingSince = time.Now()
	}
	w.pending = append(w.pending, buffered)
	w.pendingBytes += buffered.size
	if w.pendingBytes >= w.options.maxMemoryBytes {
		err := w.flushLocked(ctx)
		if err != nil && w.failure == nil {
			w.pending = w.pending[:previousLen]
			w.pendingBytes = previousBytes
			w.pendingSince = previousSince
		}
		w.wakeAutomaticFlush()
		return err
	}
	if startedBuffer {
		w.wakeAutomaticFlush()
	}
	return nil
}

// Size returns the exact number of mutations currently buffered for submission.
// The value may change immediately after the call when other goroutines use the
// writer. It returns ErrBatchWriterClosed after Close begins.
func (w *BatchWriter) Size(ctx context.Context) (int, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	if err := w.lock(ctx); err != nil {
		return 0, err
	}
	defer w.unlock()
	if w.closed {
		return 0, ErrBatchWriterClosed
	}
	return len(w.pending), nil
}

// Flush commits every mutation accepted before Flush acquired the writer.
// Tablet routing, session-start, and explicit uncommitted-suffix failures are
// retried within the configured bound. Ambiguous apply or close failures are
// sticky because Accumulo may have committed an unknown prefix. A retry limit
// error without ErrBatchWriterFailed leaves the original buffer available for
// a later Flush. Automatic flush errors are terminal, include
// ErrBatchWriterAutoFlush, and are returned unchanged.
func (w *BatchWriter) Flush(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := w.lock(ctx); err != nil {
		return err
	}
	defer w.unlock()
	if w.closed {
		return ErrBatchWriterClosed
	}
	err := w.flushLocked(ctx)
	w.wakeAutomaticFlush()
	return err
}

// Close stops accepting mutations, commits the remaining buffer, and joins the
// optional automatic flush goroutine. Repeated calls return the first Close
// result without submitting again.
func (w *BatchWriter) Close(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := w.lock(ctx); err != nil {
		return err
	}
	if w.closed {
		err := w.closeErr
		w.unlock()
		w.stopAutomaticFlush()
		return err
	}
	w.closed = true
	w.closeErr = w.flushLocked(ctx)
	if w.closeErr != nil && w.failure == nil {
		w.clearPendingLocked()
	}
	err := w.closeErr
	w.unlock()
	w.stopAutomaticFlush()
	return err
}

func (w *BatchWriter) lock(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-w.gate:
		return nil
	}
}

func (w *BatchWriter) unlock() {
	w.gate <- struct{}{}
}

func (w *BatchWriter) startAutomaticFlush() {
	if w.options.maxLatency == 0 {
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	w.autoFlushWake = make(chan struct{}, 1)
	w.autoFlushCancel = cancel
	w.autoFlushDone = make(chan struct{})
	go w.automaticFlushLoop(ctx)
}

func (w *BatchWriter) wakeAutomaticFlush() {
	if w.autoFlushWake == nil {
		return
	}
	select {
	case w.autoFlushWake <- struct{}{}:
	default:
	}
}

func (w *BatchWriter) stopAutomaticFlush() {
	if w.autoFlushCancel == nil {
		return
	}
	w.autoFlushCancel()
	<-w.autoFlushDone
}

func (w *BatchWriter) automaticFlushLoop(ctx context.Context) {
	defer close(w.autoFlushDone)
	var timer *time.Timer
	var timerC <-chan time.Time
	defer func() {
		if timer != nil {
			timer.Stop()
		}
	}()

	for {
		select {
		case <-ctx.Done():
			return
		case <-w.autoFlushWake:
		case <-timerC:
		}

		delay, active := w.automaticFlushStep(ctx, time.Now())
		if !active {
			if timer != nil {
				timer.Stop()
			}
			timerC = nil
			continue
		}
		if timer == nil {
			timer = time.NewTimer(delay)
		} else {
			// go.mod requires Go 1.25; channel timers have guaranteed that
			// Reset cannot expose a stale pre-reset tick since Go 1.23.
			timer.Reset(delay)
		}
		timerC = timer.C
	}
}

func (w *BatchWriter) automaticFlushStep(
	ctx context.Context,
	now time.Time,
) (time.Duration, bool) {
	if err := w.lock(ctx); err != nil {
		return 0, false
	}
	defer w.unlock()
	if w.closed || w.failure != nil || len(w.pending) == 0 {
		return 0, false
	}
	if w.pendingSince.IsZero() {
		w.pendingSince = now
	}
	if delay := w.pendingSince.Add(w.options.maxLatency).Sub(now); delay > 0 {
		return delay, true
	}
	if err := w.flushLocked(ctx); err != nil {
		w.failure = errors.Join(ErrBatchWriterAutoFlush, err)
		w.clearPendingLocked()
	}
	return 0, false
}

func (w *BatchWriter) clearPendingLocked() {
	w.pending = nil
	w.pendingBytes = 0
	w.pendingSince = time.Time{}
}

func (w *BatchWriter) flushLocked(ctx context.Context) error {
	if w.failure != nil {
		return w.failure
	}
	if len(w.pending) == 0 {
		return nil
	}
	table, err := w.resolveTable(ctx)
	if err != nil {
		return err
	}
	w.table = table
	remaining := w.pending
	submitted := false

	for attempt := 0; ; attempt++ {
		plans, err := w.plan(ctx, table, remaining)
		if err != nil {
			if !isRetryableWriterRoutingError(err) {
				return w.finishSubmissionFailure(submitted, err)
			}
			if invalidateErr := w.invalidateRoutingError(table, err); invalidateErr != nil {
				return w.finishSubmissionFailure(
					submitted,
					errors.Join(err, invalidateErr),
				)
			}
			if attempt >= w.options.maxRetries {
				return w.finishRetryExhausted(submitted, err)
			}
			if waitErr := waitForWriterRetry(ctx, w.options.retryBackoff); waitErr != nil {
				return w.finishSubmissionFailure(submitted, errors.Join(err, waitErr))
			}
			continue
		}

		var retry []bufferedMutation
		var invalidateRows [][]byte
		var retryEvidence error
		sendFailed := false
		for _, outcome := range w.sendServerPlans(ctx, plans) {
			result := outcome.result
			submitted = submitted || result.submitted
			if outcome.err != nil {
				sendFailed = true
				retryEvidence = errors.Join(retryEvidence, outcome.err)
				continue
			}
			if len(result.retry) == 0 {
				continue
			}
			retry = append(retry, result.retry...)
			invalidateRows = append(invalidateRows, result.invalidateRows...)
			retryEvidence = errors.Join(retryEvidence, result.evidence)
		}
		if sendFailed {
			return w.finishSubmissionFailure(submitted, retryEvidence)
		}
		if len(retry) == 0 {
			w.clearPendingLocked()
			return nil
		}
		if invalidateErr := w.invalidateRetryRows(table, invalidateRows); invalidateErr != nil {
			return w.finishSubmissionFailure(
				submitted,
				errors.Join(retryEvidence, invalidateErr),
			)
		}
		if attempt >= w.options.maxRetries {
			return w.finishRetryExhausted(submitted, retryEvidence)
		}
		if waitErr := waitForWriterRetry(ctx, w.options.retryBackoff); waitErr != nil {
			return w.finishSubmissionFailure(
				submitted,
				errors.Join(retryEvidence, waitErr),
			)
		}
		remaining = retry
	}
}

func (w *BatchWriter) sendServerPlans(
	ctx context.Context,
	plans []serverPlan,
) []serverSendOutcome {
	outcomes := make([]serverSendOutcome, len(plans))
	if len(plans) == 0 {
		return outcomes
	}
	workerCount := min(w.options.maxWriteThreads, len(plans))
	if workerCount == 1 {
		for index := range plans {
			outcomes[index].result, outcomes[index].err = w.sendServer(ctx, plans[index])
		}
		return outcomes
	}

	jobs := make(chan int)
	var workers sync.WaitGroup
	workers.Add(workerCount)
	for range workerCount {
		go func() {
			defer workers.Done()
			for index := range jobs {
				outcomes[index].result, outcomes[index].err =
					w.sendServer(ctx, plans[index])
			}
		}()
	}
	for index := range plans {
		jobs <- index
	}
	close(jobs)
	workers.Wait()
	return outcomes
}

func (w *BatchWriter) plan(
	ctx context.Context,
	table Table,
	mutations []bufferedMutation,
) ([]serverPlan, error) {
	var plans []serverPlan
	serverIndexes := make(map[string]int)
	for _, mutation := range mutations {
		tablet, err := w.connector.LocateTablet(ctx, table, mutation.row)
		if err != nil {
			return nil, &batchWriterRoutingError{
				row: cloneRow(mutation.row),
				err: err,
			}
		}
		address := tablet.Server.HostPort
		serverIndex, ok := serverIndexes[address]
		if !ok {
			serverIndex = len(plans)
			serverIndexes[address] = serverIndex
			plans = append(plans, serverPlan{
				address: address,
				indexes: make(map[tabletExtentMapKey]int),
			})
		}
		plan := &plans[serverIndex]
		extentKey := tabletExtentKey(tablet)
		extentIndex, ok := plan.indexes[extentKey]
		if !ok {
			extentIndex = len(plan.extents)
			plan.indexes[extentKey] = extentIndex
			plan.extents = append(plan.extents, extentPlan{
				extent: tabletExtentToThrift(tablet),
			})
		}
		plan.extents[extentIndex].mutations = append(
			plan.extents[extentIndex].mutations,
			mutation,
		)
	}
	return plans, nil
}

func (w *BatchWriter) resolveTable(ctx context.Context) (Table, error) {
	switch {
	case w.table.ID == "":
		return w.connector.TableByName(ctx, w.table.Name)
	case w.table.Name == "":
		return w.connector.TableByID(ctx, w.table.ID)
	default:
		return w.table, nil
	}
}

func (w *BatchWriter) sendServer(
	ctx context.Context,
	plan serverPlan,
) (serverSendResult, error) {
	session, err := w.startSession(ctx, plan.address, w.options.durability)
	if err != nil {
		return serverSendResult{
			retry:          serverPlanMutations(plan),
			invalidateRows: serverPlanRows(plan),
			evidence:       fmt.Errorf("accumulo: start update session on %s: %w", plan.address, err),
		}, nil
	}
	result := serverSendResult{}
	for _, extent := range plan.extents {
		for _, mutations := range mutationBatches(extent.mutations, w.options.maxBatchBytes) {
			result.submitted = true
			if err := session.Apply(ctx, extent.extent, mutations); err != nil {
				return result, errors.Join(
					fmt.Errorf("accumulo: apply mutations on %s: %w", plan.address, err),
					cancelWriterSession(ctx, plan.address, session),
				)
			}
		}
	}
	updateErrors, err := session.Close(ctx)
	if err != nil {
		return result, errors.Join(
			fmt.Errorf("accumulo: close update session on %s: %w", plan.address, err),
			cancelWriterSession(ctx, plan.address, session),
		)
	}
	updateResult, err := decodeMutationUpdateErrors(plan, updateErrors)
	if err != nil {
		return result, err
	}
	if updateResult.terminal {
		return result, updateResult.rejection
	}
	result.retry = updateResult.retry
	result.invalidateRows = updateResult.invalidateRows
	result.evidence = updateResult.rejection
	return result, nil
}

func (w *BatchWriter) finishSubmissionFailure(submitted bool, err error) error {
	if !submitted {
		return err
	}
	w.failure = errors.Join(ErrBatchWriterFailed, err)
	w.clearPendingLocked()
	return w.failure
}

func (w *BatchWriter) finishRetryExhausted(submitted bool, evidence error) error {
	err := errors.Join(ErrBatchWriterRetryExhausted, evidence)
	return w.finishSubmissionFailure(submitted, err)
}

func (w *BatchWriter) invalidateRoutingError(table Table, err error) error {
	var routing *batchWriterRoutingError
	if !errors.As(err, &routing) {
		return err
	}
	if errors.Is(err, ErrNoTabletCoversRow) {
		return w.connector.InvalidateTable(table)
	}
	return w.connector.InvalidateTablet(table, routing.row)
}

func (w *BatchWriter) invalidateRetryRows(table Table, rows [][]byte) error {
	var invalidateErr error
	for _, row := range rows {
		invalidateErr = errors.Join(
			invalidateErr,
			w.connector.InvalidateTablet(table, row),
		)
	}
	return invalidateErr
}

func isRetryableWriterRoutingError(err error) bool {
	return errors.Is(err, ErrTabletNotLocated) ||
		errors.Is(err, ErrNoTabletCoversRow)
}

func waitForWriterRetry(ctx context.Context, backoff time.Duration) error {
	timer := time.NewTimer(backoff)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func serverPlanMutations(plan serverPlan) []bufferedMutation {
	var mutations []bufferedMutation
	for _, extent := range plan.extents {
		mutations = append(mutations, extent.mutations...)
	}
	return mutations
}

func serverPlanRows(plan serverPlan) [][]byte {
	rows := make([][]byte, 0, len(plan.extents))
	for _, extent := range plan.extents {
		if len(extent.mutations) != 0 {
			rows = append(rows, extent.mutations[0].row)
		}
	}
	return rows
}

func cancelWriterSession(
	ctx context.Context,
	server string,
	session batchWriterSession,
) error {
	cleanupCtx, cancel := context.WithTimeout(
		context.WithoutCancel(ctx),
		batchWriterCleanupTimeout,
	)
	defer cancel()
	cancelled, err := session.Cancel(cleanupCtx)
	if errors.Is(err, ingestclient.ErrSessionClosed) {
		return nil
	}
	if err == nil && !cancelled {
		err = errors.New("server did not confirm cancellation")
	}
	if err == nil {
		return nil
	}
	return &BatchWriterCleanupError{Server: server, Err: err}
}

func mutationBatches(
	mutations []bufferedMutation,
	maxBytes int64,
) [][]*data.TMutation {
	var batches [][]*data.TMutation
	for len(mutations) > 0 {
		var batch []*data.TMutation
		var size int64
		for len(mutations) > 0 {
			next := mutations[0]
			if len(batch) > 0 && size+next.size > maxBytes {
				break
			}
			batch = append(batch, next.wire)
			size += next.size
			mutations = mutations[1:]
		}
		batches = append(batches, batch)
	}
	return batches
}

func decodeMutationUpdateErrors(
	plan serverPlan,
	updateErrors *data.UpdateErrors,
) (mutationUpdateResult, error) {
	if updateErrors == nil {
		return mutationUpdateResult{}, nil
	}
	rejection := &MutationRejectionError{Server: plan.address}
	submitted := make(map[tabletExtentMapKey]int, len(plan.extents))
	for index, extent := range plan.extents {
		submitted[thriftTabletExtentKey(extent.extent)] = index
	}
	failed := make(map[tabletExtentMapKey]int64, len(updateErrors.FailedExtents))
	for extent, committed := range updateErrors.FailedExtents {
		if extent == nil {
			return mutationUpdateResult{}, malformedUpdateError(
				plan.address,
				"a nil failed extent",
			)
		}
		key := thriftTabletExtentKey(extent)
		extentIndex, ok := submitted[key]
		if !ok {
			return mutationUpdateResult{}, malformedUpdateError(
				plan.address,
				"an unknown failed extent",
			)
		}
		count := len(plan.extents[extentIndex].mutations)
		if committed < 0 || committed > int64(count) {
			return mutationUpdateResult{}, malformedUpdateError(
				plan.address,
				"invalid committed count %d for %d mutations",
				committed,
				count,
			)
		}
		if _, duplicate := failed[key]; duplicate {
			return mutationUpdateResult{}, malformedUpdateError(
				plan.address,
				"a duplicate failed extent",
			)
		}
		failed[key] = committed
	}

	result := mutationUpdateResult{}
	for _, extent := range plan.extents {
		committed, ok := failed[thriftTabletExtentKey(extent.extent)]
		if !ok {
			continue
		}
		count := len(extent.mutations)
		rejection.FailedExtents = append(rejection.FailedExtents, FailedExtent{
			Extent:    publicTabletExtent(extent.extent),
			Submitted: count,
			Committed: committed,
		})
		if committed == int64(count) {
			continue
		}
		suffix := extent.mutations[int(committed):]
		result.retry = append(result.retry, suffix...)
		result.invalidateRows = append(result.invalidateRows, suffix[0].row)
	}
	for _, violation := range updateErrors.ViolationSummaries {
		if violation == nil {
			continue
		}
		rejection.ConstraintViolations = append(
			rejection.ConstraintViolations,
			ConstraintViolation{
				ConstraintClass:            violation.ConstrainClass,
				ViolationCode:              violation.ViolationCode,
				Description:                violation.ViolationDescription,
				NumberOfViolatingMutations: violation.NumberOfViolatingMutations,
			},
		)
	}
	for extent, code := range updateErrors.AuthorizationFailures {
		if extent == nil {
			return mutationUpdateResult{}, malformedUpdateError(
				plan.address,
				"a nil authorization extent",
			)
		}
		rejection.AuthorizationFailures = append(
			rejection.AuthorizationFailures,
			AuthorizationFailure{
				Extent: publicTabletExtent(extent),
				Code:   code.String(),
			},
		)
	}
	sort.Slice(rejection.FailedExtents, func(i, j int) bool {
		return tabletExtentLess(
			rejection.FailedExtents[i].Extent,
			rejection.FailedExtents[j].Extent,
		)
	})
	sort.Slice(rejection.AuthorizationFailures, func(i, j int) bool {
		return tabletExtentLess(
			rejection.AuthorizationFailures[i].Extent,
			rejection.AuthorizationFailures[j].Extent,
		)
	})
	result.terminal = len(rejection.ConstraintViolations) != 0 ||
		len(rejection.AuthorizationFailures) != 0
	if result.terminal || len(result.retry) != 0 {
		result.rejection = rejection
	}
	return result, nil
}

func malformedUpdateError(server, format string, args ...any) error {
	detail := fmt.Sprintf(format, args...)
	return fmt.Errorf(
		"accumulo: malformed closeUpdate response from tablet server %s: %s",
		server,
		detail,
	)
}

func normalizeBatchWriterOptions(
	options BatchWriterOptions,
) (normalizedBatchWriterOptions, error) {
	if options.MaxMemoryBytes < 0 {
		return normalizedBatchWriterOptions{}, errors.New(
			"accumulo: batch writer MaxMemoryBytes must not be negative",
		)
	}
	if options.MaxMemoryBytes == 0 {
		options.MaxMemoryBytes = defaultBatchWriterMaxMemoryBytes
	}
	if options.MaxBatchBytes < 0 {
		return normalizedBatchWriterOptions{}, errors.New(
			"accumulo: batch writer MaxBatchBytes must not be negative",
		)
	}
	if options.MaxBatchBytes == 0 {
		options.MaxBatchBytes = defaultBatchWriterMaxBatchBytes
	}
	if options.MaxLatency < 0 {
		return normalizedBatchWriterOptions{}, errors.New(
			"accumulo: batch writer MaxLatency must not be negative",
		)
	}
	if options.MaxWriteThreads < 0 {
		return normalizedBatchWriterOptions{}, errors.New(
			"accumulo: batch writer MaxWriteThreads must not be negative",
		)
	}
	if options.MaxWriteThreads == 0 {
		options.MaxWriteThreads = defaultBatchWriterMaxWriteThreads
	}
	if options.MaxRetries < 0 {
		return normalizedBatchWriterOptions{}, errors.New(
			"accumulo: batch writer MaxRetries must not be negative",
		)
	}
	if options.MaxRetries == 0 {
		options.MaxRetries = defaultBatchWriterMaxRetries
	}
	if options.RetryBackoff < 0 {
		return normalizedBatchWriterOptions{}, errors.New(
			"accumulo: batch writer RetryBackoff must not be negative",
		)
	}
	if options.RetryBackoff == 0 {
		options.RetryBackoff = defaultBatchWriterRetryBackoff
	}
	if options.Durability > DurabilityNone {
		return normalizedBatchWriterOptions{}, errors.New(
			"accumulo: batch writer durability is invalid",
		)
	}
	return normalizedBatchWriterOptions{
		maxMemoryBytes:  options.MaxMemoryBytes,
		maxBatchBytes:   options.MaxBatchBytes,
		maxLatency:      options.MaxLatency,
		maxWriteThreads: options.MaxWriteThreads,
		maxRetries:      options.MaxRetries,
		retryBackoff:    options.RetryBackoff,
		durability:      ingestclient.Durability(options.Durability),
	}, nil
}

func mutationWireSize(mutation *data.TMutation) int64 {
	size := int64(len(mutation.Row) + len(mutation.Data))
	for _, value := range mutation.Values {
		size += int64(len(value))
	}
	return size
}

func thriftTabletExtentKey(extent *data.TKeyExtent) tabletExtentMapKey {
	return tabletExtentMapKey{
		tableID:    string(extent.Table),
		prevRow:    string(extent.PrevEndRow),
		prevRowSet: extent.PrevEndRow != nil,
		endRow:     string(extent.EndRow),
		endRowSet:  extent.EndRow != nil,
	}
}

func publicTabletExtent(extent *data.TKeyExtent) TabletExtent {
	return TabletExtent{
		TableID: string(extent.Table),
		PrevRow: cloneRow(extent.PrevEndRow),
		EndRow:  cloneRow(extent.EndRow),
	}
}

func tabletExtentLess(left, right TabletExtent) bool {
	if left.TableID != right.TableID {
		return left.TableID < right.TableID
	}
	if comparison := optionalBytesCompare(left.PrevRow, right.PrevRow); comparison != 0 {
		return comparison < 0
	}
	return optionalBytesCompare(left.EndRow, right.EndRow) < 0
}

func optionalBytesCompare(left, right []byte) int {
	if left == nil && right != nil {
		return -1
	}
	if left != nil && right == nil {
		return 1
	}
	return bytes.Compare(left, right)
}
