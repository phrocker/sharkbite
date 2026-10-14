// Licensed to the Apache Software Foundation (ASF) under one
// or more contributor license agreements. See the NOTICE file distributed
// with this work for additional information regarding copyright ownership.
// The ASF licenses this file to you under the Apache License, Version 2.0.

// Package hostedingest joins fenced WAL, memtable, and minor-compaction
// authorities for one manager-assigned tablet.
package hostedingest

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"math"
	"path/filepath"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	"github.com/phrocker/shoal-oss/internal/embeddingspace"
	"github.com/phrocker/shoal-oss/internal/ingestrouter"
	"github.com/phrocker/shoal-oss/internal/metadata"
	"github.com/phrocker/shoal-oss/internal/mincauthority"
	"github.com/phrocker/shoal-oss/internal/rfile"
	"github.com/phrocker/shoal-oss/internal/storage"
	"github.com/phrocker/shoal-oss/internal/tabletloader"
	"github.com/phrocker/shoal-oss/internal/tserver"
	"github.com/phrocker/shoal-oss/internal/walauthority"
)

type MetadataAuthority interface {
	walauthority.MetadataAuthority
	mincauthority.MetadataAuthority
}

type MetadataFactory interface {
	Open(context.Context, tabletloader.Specification, ingestrouter.Fence) (MetadataAuthority, error)
}

type systemTabletMetadataFactory interface {
	SupportsSystemTablets() bool
}

func (t *Tablet) formattedTabletTimeLocked() string {
	return string([]byte{t.timeType}) + strconv.FormatInt(t.tabletTime, 10)
}

func initialTabletTime(value string, now time.Time) (byte, int64, int64, bool, error) {
	if value == "" {
		current := now.UnixMilli() - 1
		return 'M', current, current + 1, false, nil
	}
	if len(value) < 2 || (value[0] != 'M' && value[0] != 'L') {
		return 0, 0, 0, false, fmt.Errorf("hostedingest: invalid tablet time %q", value)
	}
	current, err := strconv.ParseInt(value[1:], 10, 64)
	if err != nil || current < 0 {
		return 0, 0, 0, false, fmt.Errorf("hostedingest: invalid tablet time %q", value)
	}
	if current == math.MaxInt64 {
		return value[0], current, current, true, nil
	}
	next := current + 1
	if value[0] == 'M' && now.UnixMilli() > next {
		next = now.UnixMilli()
	}
	return value[0], current, next, false, nil
}

var (
	ErrTimestampExhausted                 = errors.New("hostedingest: tablet timestamp exhausted")
	ErrSystemTabletConditionalUnsupported = errors.New(
		"hostedingest: system tablets require conditional-update hosting")
)

const maxDedupOperations = 1 << 16

type Config struct {
	Host           *tserver.Host
	ServerAddress  string
	WALRoot        string
	MincRoot       string
	StateRoot      string
	WALStore       walauthority.Store
	Outputs        storage.Backend
	Metadata       MetadataFactory
	FlushID        func(context.Context, string) (int64, error)
	FlushCells     int
	Now            func() time.Time
	NewOperationID func() string

	// DefaultEmbedding is the embedding-space state recorded for a minor
	// compaction whose snapshot declared none. It is forwarded verbatim
	// to mincauthority.Config.DefaultEmbedding: with no value configured,
	// an undeclared output is labelled unknown rather than having a claim
	// invented for it. See issue #274.
	DefaultEmbedding embeddingspace.FileState
}

type Metrics struct {
	HostedTablets int64
	WALCommits    uint64
	WALFailures   uint64
	WALRecoveries uint64
	MincStarted   uint64
	MincCompleted uint64
	MincFailures  uint64
	MincResumed   uint64
}

type factoryMetrics struct {
	hostedTablets atomic.Int64
	walCommits    atomic.Uint64
	walFailures   atomic.Uint64
	walRecoveries atomic.Uint64
	mincStarted   atomic.Uint64
	mincCompleted atomic.Uint64
	mincFailures  atomic.Uint64
	mincResumed   atomic.Uint64
}

type Factory struct {
	cfg     Config
	metrics factoryMetrics
}

func (f *Factory) Metrics() Metrics {
	return Metrics{
		HostedTablets: f.metrics.hostedTablets.Load(),
		WALCommits:    f.metrics.walCommits.Load(), WALFailures: f.metrics.walFailures.Load(),
		WALRecoveries: f.metrics.walRecoveries.Load(), MincStarted: f.metrics.mincStarted.Load(),
		MincCompleted: f.metrics.mincCompleted.Load(), MincFailures: f.metrics.mincFailures.Load(),
		MincResumed: f.metrics.mincResumed.Load(),
	}
}

func NewFactory(cfg Config) (*Factory, error) {
	if cfg.Host == nil || cfg.ServerAddress == "" || cfg.WALRoot == "" ||
		cfg.MincRoot == "" || cfg.StateRoot == "" || cfg.WALStore == nil ||
		cfg.Outputs == nil || cfg.Metadata == nil {
		return nil, errors.New("hostedingest: incomplete factory configuration")
	}
	if cfg.FlushCells <= 0 {
		// Hosted scans currently consume immutable files, not a live memtable.
		// Flush each acknowledged batch by default so new scans observe it.
		cfg.FlushCells = 1
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.NewOperationID == nil {
		cfg.NewOperationID = uuid.NewString
	}
	if cfg.DefaultEmbedding != (embeddingspace.FileState{}) {
		// Whole-struct comparison: a partial FileState{Identity: "x"}
		// is malformed startup configuration and must fail fast here,
		// not be treated as if nothing had been configured.
		if err := cfg.DefaultEmbedding.Validate(); err != nil {
			return nil, fmt.Errorf("hostedingest: default embedding: %w", err)
		}
	}
	return &Factory{cfg: cfg}, nil
}

func (f *Factory) Open(
	ctx context.Context,
	spec tabletloader.Specification,
	attempt tserver.Attempt,
) (ingestrouter.HostedTablet, error) {
	serverLock, serverOK := f.cfg.Host.Lock()
	managerLock, managerOK := f.cfg.Host.ManagerLock()
	if !serverOK || !managerOK || !attempt.Valid() {
		return nil, ingestrouter.ErrStaleFence
	}
	serverGeneration := string(spec.Generation)
	if serverGeneration == "" {
		serverGeneration = serverLock.String()
	}
	fence := ingestrouter.Fence{
		ServerGeneration: serverGeneration, ManagerGeneration: managerLock.String(),
		Assignment: attempt.Assignment(),
	}
	extent := ingestrouter.Extent{
		TableID: spec.Extent.TableID, PrevEndRow: spec.Extent.PrevEndRow, EndRow: spec.Extent.EndRow,
	}
	if extent.TableID == metadata.RootTableID || extent.TableID == metadata.MetadataTableID {
		conditional, ok := f.cfg.Metadata.(systemTabletMetadataFactory)
		if !ok || !conditional.SupportsSystemTablets() {
			return nil, ErrSystemTabletConditionalUnsupported
		}
	}
	timeType, tabletTime, nextTimestamp, exhausted, err := initialTabletTime(spec.Time, f.cfg.Now())
	if err != nil {
		return nil, err
	}
	verifier := hostVerifier{
		host: f.cfg.Host, attempt: attempt,
		server: serverLock, manager: managerLock, fence: fence,
	}
	recoveryVerifier := verifier
	recoveryVerifier.allowLoading = true
	metadata, err := f.cfg.Metadata.Open(ctx, spec, fence)
	if err != nil {
		return nil, err
	}
	opened := false
	defer func() {
		if !opened {
			if releaser, ok := metadata.(interface {
				Release(context.Context, ingestrouter.Extent, ingestrouter.Fence) error
			}); ok {
				_ = releaser.Release(context.Background(), extent, fence)
			}
		}
	}()
	tablet := &Tablet{
		extent: extent, fence: fence, verifier: verifier, flushCells: f.cfg.FlushCells,
		metadata: metadata, flushID: f.cfg.FlushID,
		snapshots: make(map[string]mincauthority.Snapshot),
		applied:   make(map[string]struct{}), assigned: make(map[string][]ingestrouter.Mutation),
		timeType: timeType, tabletTime: tabletTime, nextTimestamp: nextTimestamp,
		timestampExhausted: exhausted, newOperationID: f.cfg.NewOperationID,
		metrics: &f.metrics,
	}
	wal, report, err := walauthority.Open(ctx, walauthority.Config{
		Root: f.cfg.WALRoot, ServerAddress: f.cfg.ServerAddress,
		Extent: extent, Fence: fence, Metadata: metadata, Store: f.cfg.WALStore,
		Verifier: verifier, RecoveryVerifier: recoveryVerifier, Sink: tablet,
	})
	if err != nil {
		return nil, err
	}
	tablet.wal = wal
	tablet.recovery = report
	if report.Applied > 0 {
		f.metrics.walRecoveries.Add(1)
	}
	stateStore := &mincauthority.FileStateStore{Dir: filepath.Join(f.cfg.StateRoot, extentDigest(extent))}
	coordinator, err := mincauthority.New(mincauthority.Config{
		Root: f.cfg.MincRoot, Extent: extent, Fence: fence,
		Snapshots: tablet, Verifier: recoveryVerifier, Metadata: metadata,
		Outputs: mincauthority.BackendOutputStore{Backend: f.cfg.Outputs},
		States:  stateStore,

		DefaultEmbedding: f.cfg.DefaultEmbedding,
	})
	if err != nil {
		_ = wal.Close(context.Background())
		return nil, err
	}
	tablet.minc = coordinator
	pending, err := stateStore.Pending(ctx, extent, fence)
	if err != nil {
		_ = wal.Close(context.Background())
		return nil, err
	}
	for _, state := range pending {
		f.metrics.mincResumed.Add(1)
		tablet.resume = append(tablet.resume, state.OperationID)
		tablet.snapshots[state.OperationID] = mincauthority.Snapshot{
			ID: state.SnapshotID, Extent: extent, Fence: fence,
			Boundary: state.Boundary, TabletTime: state.TabletTime,
			Cells:       cloneMincCells(state.SnapshotCells),
			CoveredWALs: append([]walauthority.Reference(nil), state.CoveredWALs...),
		}
		if state.Phase < mincauthority.PhaseCommitted {
			if err := tablet.removeRecoveredSnapshotCells(state.SnapshotCells); err != nil {
				_ = wal.Close(context.Background())
				return nil, err
			}
		}
	}
	if err := tablet.resumePending(ctx); err != nil {
		_ = wal.Close(context.Background())
		return nil, err
	}
	tablet.mu.Lock()
	recovered := len(tablet.active) > 0
	tablet.mu.Unlock()
	if recovered {
		if err := tablet.flush(ctx); err != nil {
			_ = wal.Close(context.Background())
			return nil, err
		}
	}
	opened = true
	f.metrics.hostedTablets.Add(1)
	return tablet, nil
}

type hostVerifier struct {
	host            *tserver.Host
	attempt         tserver.Attempt
	server, manager tserver.LockID
	fence           ingestrouter.Fence
	allowLoading    bool
}

func (v hostVerifier) Verify(_ context.Context, fence ingestrouter.Fence) error {
	if fence != v.fence {
		return ingestrouter.ErrStaleFence
	}
	fenceValue := tserver.Fence{Server: v.server, Manager: v.manager}
	if v.allowLoading {
		return v.host.VerifyAssigned(fenceValue, v.attempt)
	}
	return v.host.VerifyHosted(fenceValue, v.attempt)
}

type Tablet struct {
	opMu sync.Mutex
	mu   sync.Mutex

	extent   ingestrouter.Extent
	fence    ingestrouter.Fence
	verifier hostVerifier
	metadata MetadataAuthority
	wal      *walauthority.Tablet
	minc     *mincauthority.Coordinator
	recovery walauthority.RecoveryReport
	metrics  *factoryMetrics

	active             []mincauthority.Cell
	activeSize         int
	snapshots          map[string]mincauthority.Snapshot
	applied            map[string]struct{}
	assigned           map[string][]ingestrouter.Mutation
	operationOrder     []string
	nextTimestamp      int64
	timeType           byte
	tabletTime         int64
	timestampExhausted bool
	flushCells         int
	flushID            func(context.Context, string) (int64, error)
	newOperationID     func() string
	pendingFlush       string
	resume             []string
	files              []mincauthority.DataFile
	closed             bool
}

func (t *Tablet) Extent() ingestrouter.Extent { return cloneExtent(t.extent) }
func (t *Tablet) Fence() ingestrouter.Fence   { return t.fence }
func (t *Tablet) Authority() ingestrouter.CommitAuthority {
	return ingestrouter.AuthorityAccumuloWAL
}

func (t *Tablet) Commit(ctx context.Context, request ingestrouter.CommitRequest) error {
	t.opMu.Lock()
	defer t.opMu.Unlock()
	return t.commitLocked(ctx, request)
}

func (t *Tablet) ConditionalCommit(
	ctx context.Context,
	request ingestrouter.CommitRequest,
	evaluate ingestrouter.ConditionalEvaluator,
) (bool, error) {
	t.opMu.Lock()
	defer t.opMu.Unlock()
	if err := t.resumePending(ctx); err != nil {
		return false, err
	}
	t.mu.Lock()
	active := make([]ingestrouter.Cell, len(t.active))
	for i, cell := range t.active {
		active[i] = ingestrouter.Cell{
			Row:              append([]byte(nil), cell.Key.Row...),
			ColumnFamily:     append([]byte(nil), cell.Key.ColumnFamily...),
			ColumnQualifier:  append([]byte(nil), cell.Key.ColumnQualifier...),
			ColumnVisibility: append([]byte(nil), cell.Key.ColumnVisibility...),
			Timestamp:        cell.Key.Timestamp,
			Value:            append([]byte(nil), cell.Value...),
			Deleted:          cell.Key.Deleted,
		}
	}
	t.mu.Unlock()
	accepted, err := evaluate(ctx, active)
	if err != nil || !accepted {
		return accepted, err
	}
	if err := t.commitLocked(ctx, request); err != nil {
		return false, err
	}
	return true, nil
}

func (t *Tablet) commitLocked(ctx context.Context, request ingestrouter.CommitRequest) error {
	if err := t.resumePending(ctx); err != nil {
		return err
	}
	t.mu.Lock()
	closed := t.closed
	t.mu.Unlock()
	if closed {
		return walauthority.ErrClosed
	}
	assigned, err := t.assignTimestamps(request.OperationID, request.Mutations)
	if err != nil {
		return err
	}
	request.Mutations = assigned
	if err := t.wal.Commit(ctx, request); err != nil {
		t.metrics.walFailures.Add(1)
		return routeError(err)
	}
	t.metrics.walCommits.Add(1)
	t.mu.Lock()
	flush := !t.closed && len(t.active) >= t.flushCells
	t.mu.Unlock()
	if flush {
		if err := t.flush(ctx); err != nil {
			// The WAL commit may already be durable, but scans cannot observe
			// the batch until its immutable file is installed. Force the
			// caller to retry the same idempotency key while retaining the
			// exact compaction operation for reconciliation.
			return errors.Join(ingestrouter.ErrUnknownCommit, err)
		}
	}
	return nil
}

func (t *Tablet) Apply(
	ctx context.Context,
	operationID string,
	_ int64,
	mutations []ingestrouter.Mutation,
) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.closed {
		return walauthority.ErrClosed
	}
	if _, ok := t.applied[operationID]; ok {
		return nil
	}
	t.rememberOperationLocked(operationID, mutations)
	for _, mutation := range mutations {
		for _, update := range mutation.Updates {
			if update.Timestamp.Set && update.Timestamp.Value >= t.nextTimestamp {
				if update.Timestamp.Value == math.MaxInt64 {
					t.timestampExhausted = true
				} else {
					t.nextTimestamp = update.Timestamp.Value + 1
				}
			}
			if update.Timestamp.Set && update.Timestamp.Value > t.tabletTime {
				t.tabletTime = update.Timestamp.Value
			}
			t.active = append(t.active, mincauthority.Cell{
				Key: rfile.Key{
					Row:              append([]byte(nil), mutation.Row...),
					ColumnFamily:     append([]byte(nil), update.ColumnFamily...),
					ColumnQualifier:  append([]byte(nil), update.ColumnQualifier...),
					ColumnVisibility: append([]byte(nil), update.ColumnVisibility...),
					Timestamp:        update.Timestamp.Value, Deleted: update.Delete,
				},
				Value: append([]byte(nil), update.Value...),
			})
			t.activeSize += len(mutation.Row) + len(update.ColumnFamily) +
				len(update.ColumnQualifier) + len(update.ColumnVisibility) + len(update.Value) + 24
		}
	}
	t.applied[operationID] = struct{}{}
	return nil
}

func (t *Tablet) Prepare(
	ctx context.Context,
	operationID string,
	extent ingestrouter.Extent,
	fence ingestrouter.Fence,
) (mincauthority.Snapshot, error) {
	t.mu.Lock()
	if existing, ok := t.snapshots[operationID]; ok {
		t.mu.Unlock()
		return cloneSnapshot(existing), nil
	}
	t.mu.Unlock()

	var snapshot mincauthority.Snapshot
	boundary, err := t.wal.SealForMinorCompaction(ctx, func(boundary walauthority.MinorCompactionBoundary) error {
		t.mu.Lock()
		defer t.mu.Unlock()
		if t.closed || len(t.active) == 0 {
			return mincauthority.ErrInvalidSnapshot
		}
		snapshot = mincauthority.Snapshot{
			ID:     operationID + ":" + fmt.Sprint(boundary.Sequence),
			Extent: cloneExtent(extent), Fence: fence, Boundary: boundary.Sequence,
			TabletTime:  t.formattedTabletTimeLocked(),
			Cells:       append([]mincauthority.Cell(nil), t.active...),
			CoveredWALs: append([]walauthority.Reference(nil), boundary.References...),
		}
		t.active = nil
		t.activeSize = 0
		t.snapshots[operationID] = cloneSnapshot(snapshot)
		return nil
	})
	if err != nil {
		return mincauthority.Snapshot{}, err
	}
	if snapshot.Boundary != boundary.Sequence {
		return mincauthority.Snapshot{}, mincauthority.ErrInvalidSnapshot
	}
	return cloneSnapshot(snapshot), nil
}

func (t *Tablet) Complete(ctx context.Context, snapshotID string, _ mincauthority.DataFile) error {
	t.mu.Lock()
	var snapshot mincauthority.Snapshot
	var operationID string
	for id, candidate := range t.snapshots {
		if candidate.ID == snapshotID {
			operationID, snapshot = id, candidate
			break
		}
	}
	t.mu.Unlock()
	for _, ref := range snapshot.CoveredWALs {
		if err := t.wal.Retire(ctx, ref); err != nil {
			return err
		}
	}
	if operationID != "" {
		t.mu.Lock()
		delete(t.snapshots, operationID)
		t.mu.Unlock()
	}
	return nil
}

func (t *Tablet) Flush(ctx context.Context) error {
	var flushID int64
	if t.flushID != nil {
		var err error
		flushID, err = t.flushID(ctx, t.extent.TableID)
		if err != nil {
			return err
		}
	}
	t.opMu.Lock()
	defer t.opMu.Unlock()
	if err := t.resumePending(ctx); err != nil {
		return err
	}
	if err := t.flush(ctx); err != nil {
		return err
	}
	if updater, ok := t.metadata.(interface {
		UpdateFlushID(context.Context, int64) error
	}); ok && t.flushID != nil {
		return updater.UpdateFlushID(ctx, flushID)
	}
	return nil
}

func (t *Tablet) resumePending(ctx context.Context) error {
	for len(t.resume) > 0 {
		operationID := t.resume[0]
		file, err := t.minc.Run(ctx, operationID)
		if err != nil {
			return err
		}
		t.mu.Lock()
		found := false
		for _, existing := range t.files {
			if existing.Path == file.Path {
				found = true
				break
			}
		}
		if !found {
			t.files = append(t.files, file)
		}
		t.resume = t.resume[1:]
		t.mu.Unlock()
	}
	return nil
}

func (t *Tablet) flush(ctx context.Context) error {
	t.mu.Lock()
	empty := len(t.active) == 0 && t.pendingFlush == ""
	closed := t.closed
	t.mu.Unlock()
	if closed {
		return walauthority.ErrClosed
	}
	if empty {
		return nil
	}
	t.mu.Lock()
	operationID := t.pendingFlush
	if operationID == "" {
		operationID = "minc-" + t.newOperationID()
		t.pendingFlush = operationID
	}
	t.mu.Unlock()
	t.metrics.mincStarted.Add(1)
	file, err := t.minc.Run(ctx, operationID)
	if err == nil {
		t.metrics.mincCompleted.Add(1)
		t.mu.Lock()
		if t.pendingFlush == operationID {
			t.pendingFlush = ""
		}
		found := false
		for _, existing := range t.files {
			if existing.Path == file.Path {
				found = true
				break
			}
		}
		if !found {
			t.files = append(t.files, file)
		}
		t.mu.Unlock()
	} else {
		t.metrics.mincFailures.Add(1)
	}
	return err
}

func (t *Tablet) Close(ctx context.Context) error {
	t.opMu.Lock()
	defer t.opMu.Unlock()
	t.mu.Lock()
	if t.closed {
		t.mu.Unlock()
		return nil
	}
	t.mu.Unlock()
	if err := t.resumePending(ctx); err != nil {
		return err
	}
	if err := t.flush(ctx); err != nil {
		return err
	}
	if err := t.wal.Close(ctx); err != nil {
		return err
	}
	if releaser, ok := t.metadata.(interface {
		Release(context.Context, ingestrouter.Extent, ingestrouter.Fence) error
	}); ok {
		if err := releaser.Release(ctx, t.extent, t.fence); err != nil {
			return err
		}
	}
	t.mu.Lock()
	t.closed = true
	t.mu.Unlock()
	t.metrics.hostedTablets.Add(-1)
	return nil
}

func (t *Tablet) Recovery() walauthority.RecoveryReport { return t.recovery }

func (t *Tablet) DataFiles() []mincauthority.DataFile {
	t.mu.Lock()
	defer t.mu.Unlock()
	out := make([]mincauthority.DataFile, len(t.files))
	copy(out, t.files)
	for i := range out {
		out[i].StartRow = append([]byte(nil), t.files[i].StartRow...)
		out[i].EndRow = append([]byte(nil), t.files[i].EndRow...)
	}
	return out
}

func (t *Tablet) assignTimestamps(
	operationID string,
	mutations []ingestrouter.Mutation,
) ([]ingestrouter.Mutation, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if prior, ok := t.assigned[operationID]; ok {
		return cloneMutations(prior), nil
	}
	out := make([]ingestrouter.Mutation, len(mutations))
	for i, mutation := range mutations {
		out[i].Row = append([]byte(nil), mutation.Row...)
		out[i].Updates = make([]ingestrouter.Update, len(mutation.Updates))
		for j, update := range mutation.Updates {
			out[i].Updates[j] = update
			out[i].Updates[j].ColumnFamily = append([]byte(nil), update.ColumnFamily...)
			out[i].Updates[j].ColumnQualifier = append([]byte(nil), update.ColumnQualifier...)
			out[i].Updates[j].ColumnVisibility = append([]byte(nil), update.ColumnVisibility...)
			out[i].Updates[j].Value = append([]byte(nil), update.Value...)
			if !update.Timestamp.Set {
				if t.timestampExhausted {
					return nil, ErrTimestampExhausted
				}
				out[i].Updates[j].Timestamp = ingestrouter.Timestamp{
					Set: true, Value: t.nextTimestamp,
				}
				if t.nextTimestamp == math.MaxInt64 {
					t.timestampExhausted = true
				} else {
					t.nextTimestamp++
				}
				if out[i].Updates[j].Timestamp.Value > t.tabletTime {
					t.tabletTime = out[i].Updates[j].Timestamp.Value
				}
			} else if update.Timestamp.Value >= t.nextTimestamp {
				if update.Timestamp.Value == math.MaxInt64 {
					t.timestampExhausted = true
				} else {
					t.nextTimestamp = update.Timestamp.Value + 1
				}
			}
			if update.Timestamp.Set && update.Timestamp.Value > t.tabletTime {
				t.tabletTime = update.Timestamp.Value
			}
		}
	}
	t.rememberOperationLocked(operationID, out)
	return out, nil
}

func (t *Tablet) rememberOperationLocked(
	operationID string,
	mutations []ingestrouter.Mutation,
) {
	if _, exists := t.assigned[operationID]; !exists {
		t.operationOrder = append(t.operationOrder, operationID)
	}
	t.assigned[operationID] = cloneMutations(mutations)
	for len(t.operationOrder) > maxDedupOperations {
		oldest := t.operationOrder[0]
		t.operationOrder = t.operationOrder[1:]
		delete(t.assigned, oldest)
		delete(t.applied, oldest)
	}
}

func (t *Tablet) removeRecoveredSnapshotCells(snapshot []mincauthority.Cell) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	for _, expected := range snapshot {
		found := -1
		for i, candidate := range t.active {
			if candidate.Key.Compare(&expected.Key) == 0 && bytes.Equal(candidate.Value, expected.Value) {
				found = i
				break
			}
		}
		if found < 0 {
			return mincauthority.ErrInvalidSnapshot
		}
		t.active = append(t.active[:found], t.active[found+1:]...)
	}
	t.activeSize = 0
	for _, cell := range t.active {
		t.activeSize += len(cell.Key.Row) + len(cell.Key.ColumnFamily) +
			len(cell.Key.ColumnQualifier) + len(cell.Key.ColumnVisibility) + len(cell.Value) + 24
	}
	return nil
}

func routeError(err error) error {
	switch {
	case errors.Is(err, walauthority.ErrStaleOwner), errors.Is(err, mincauthority.ErrStaleOwner):
		return errors.Join(ingestrouter.ErrStaleFence, err)
	case errors.Is(err, walauthority.ErrAmbiguous), errors.Is(err, mincauthority.ErrAmbiguous):
		return errors.Join(ingestrouter.ErrUnknownCommit, err)
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return errors.Join(ingestrouter.ErrRetryable, err)
	default:
		return err
	}
}

func cloneExtent(in ingestrouter.Extent) ingestrouter.Extent {
	return ingestrouter.Extent{
		TableID: in.TableID, PrevEndRow: append([]byte(nil), in.PrevEndRow...),
		EndRow: append([]byte(nil), in.EndRow...),
	}
}

func cloneSnapshot(in mincauthority.Snapshot) mincauthority.Snapshot {
	out := in
	out.Extent = cloneExtent(in.Extent)
	out.Cells = cloneMincCells(in.Cells)
	out.CoveredWALs = append([]walauthority.Reference(nil), in.CoveredWALs...)
	return out
}

func cloneMincCells(in []mincauthority.Cell) []mincauthority.Cell {
	out := make([]mincauthority.Cell, len(in))
	for i, cell := range in {
		out[i] = mincauthority.Cell{Key: *cell.Key.Clone(), Value: append([]byte(nil), cell.Value...)}
	}
	return out
}

func cloneMutations(in []ingestrouter.Mutation) []ingestrouter.Mutation {
	out := make([]ingestrouter.Mutation, len(in))
	for i, mutation := range in {
		out[i].Row = append([]byte(nil), mutation.Row...)
		out[i].Updates = make([]ingestrouter.Update, len(mutation.Updates))
		for j, update := range mutation.Updates {
			out[i].Updates[j] = update
			out[i].Updates[j].ColumnFamily = append([]byte(nil), update.ColumnFamily...)
			out[i].Updates[j].ColumnQualifier = append([]byte(nil), update.ColumnQualifier...)
			out[i].Updates[j].ColumnVisibility = append([]byte(nil), update.ColumnVisibility...)
			out[i].Updates[j].Value = append([]byte(nil), update.Value...)
		}
	}
	return out
}

func extentDigest(extent ingestrouter.Extent) string {
	sum := sha256.Sum256([]byte(extent.Key()))
	return fmt.Sprintf("%x", sum[:16])
}
