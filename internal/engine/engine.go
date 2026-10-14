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

// Package engine is the top-level API for the embedded Accumulo-model
// storage engine. It manages tables, routes mutations to the correct
// tablet via a SplitPolicy, and serves scans that merge across tablets
// in parallel.
//
// This is the package external consumers talk to — it replaces
// better-sqlite3 as the storage layer.
//
// Core API:
//
//	eng, _ := engine.Open("~/.shoal/data", engine.Options{})
//	eng.CreateTable("graph", engine.TableOptions{
//	    Splits: engine.PrefixSplit("entity:", "event:", "knowledge:"),
//	})
//	eng.Write("graph", mutations)
//	scanner := eng.Scan("graph", iterrt.InfiniteRange(), engine.ScanOptions{})
//	for scanner.Next() { ... }
//	scanner.Close()
//	eng.Close()
package engine

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/phrocker/shoal-oss/internal/cclient"
	"github.com/phrocker/shoal-oss/internal/embeddingspace"
	"github.com/phrocker/shoal-oss/internal/iterrt"
	"github.com/phrocker/shoal-oss/internal/localwal"
	"github.com/phrocker/shoal-oss/internal/rfile/wire"
	"github.com/phrocker/shoal-oss/internal/storage"
	"github.com/phrocker/shoal-oss/internal/storage/local"
	"github.com/phrocker/shoal-oss/internal/tablet"
)

// Engine is the embedded Accumulo-model storage engine.
type Engine struct {
	mu     sync.RWMutex
	dir    string
	tables map[string]*table
	logger *slog.Logger
	cache  *tablet.Cache

	walSyncMode     localwal.SyncMode
	walSyncInterval time.Duration
	backend         storage.Backend

	metrics engineCounters

	// RFile event bus: subscribers receive an RFileEvent each time a tablet
	// flushes or compacts a new immutable RFile (including auto-flushes). Used
	// by the in-process event-driven syncer so RFiles ship as soon as they
	// land instead of on a fixed interval.
	subsMu  sync.Mutex
	subs    map[uint64]chan RFileEvent
	nextSub uint64
}

// engineCounters holds the atomic operation counters surfaced by Metrics().
// Kept as a separate struct so the atomics live on one cache-friendly block
// and are trivially snapshotted.
type engineCounters struct {
	writes       atomic.Uint64
	mutations    atomic.Uint64
	cellsWritten atomic.Uint64
	scans        atomic.Uint64
	flushes      atomic.Uint64
	compactions  atomic.Uint64
}

// Options configures the engine.
type Options struct {
	// Logger for engine operations. Nil uses slog.Default().
	Logger *slog.Logger

	// CacheBytes is the budget for the shared RFile byte + block cache
	// reused across every tablet scan. Zero uses tablet.DefaultCacheBytes;
	// negative disables caching (every scan re-reads from disk).
	CacheBytes int64

	// WALSyncMode selects the write-ahead-log durability tier applied to
	// every tablet (created or reopened). The zero value is
	// localwal.SyncFull (fsync every write — safest, but slowest). Use
	// SyncNormal to take the fsync off the per-write hot path (durable
	// across process crash, like SQLite's synchronous=NORMAL), optionally
	// paired with WALSyncInterval to bound the loss window.
	WALSyncMode localwal.SyncMode

	// WALSyncInterval, when > 0, enables group-commit: a background
	// goroutine fsyncs each tablet WAL at most once per interval whenever
	// writes are pending. Only meaningful under SyncNormal/SyncOff.
	WALSyncInterval time.Duration

	// Backend is the object store flushed RFiles are written to and read
	// from (the durable, immutable layer). Nil defaults to the local
	// filesystem, preserving on-disk behavior. A memory or cloud backend
	// keeps each tablet's WAL local while flushing RFiles elsewhere — the
	// basis for a locally-resident, cloud-durable subgraph.
	Backend storage.Backend
}

// Open opens or creates an engine rooted at dir.
func Open(dir string, opts Options) (*Engine, error) {
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("engine: mkdir %s: %w", dir, err)
	}
	backend := opts.Backend
	if backend == nil {
		backend = local.New()
	}
	var rfCache *tablet.Cache
	if opts.CacheBytes >= 0 {
		rfCache = tablet.NewCacheWithBackend(opts.CacheBytes, backend)
	}
	eng := &Engine{
		dir:             dir,
		tables:          make(map[string]*table),
		logger:          opts.Logger,
		cache:           rfCache,
		walSyncMode:     opts.WALSyncMode,
		walSyncInterval: opts.WALSyncInterval,
		backend:         backend,
	}

	// Discover existing tables (each subdirectory is a table)
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("engine: readdir %s: %w", dir, err)
	}
	for _, e := range entries {
		if e.IsDir() {
			tbl, err := openTable(filepath.Join(dir, e.Name()), e.Name(), opts.Logger, eng.cache, eng.walSyncMode, eng.walSyncInterval, eng.backend, eng.publishRFile)
			if err != nil {
				eng.Close()
				return nil, fmt.Errorf("engine: open table %s: %w", e.Name(), err)
			}
			eng.tables[e.Name()] = tbl
		}
	}
	return eng, nil
}

// CreateTable creates a new table with the given split policy. If splits
// is nil, the table has a single tablet covering the entire key space.
func (e *Engine) CreateTable(name string, opts TableOptions) error {
	e.mu.Lock()
	defer e.mu.Unlock()

	if _, exists := e.tables[name]; exists {
		return fmt.Errorf("engine: table %q already exists", name)
	}

	tableDir := filepath.Join(e.dir, name)
	// Apply the engine-wide WAL durability policy unless the caller set a
	// per-table override (non-zero value).
	if opts.TabletOptions.WALSyncMode == localwal.SyncFull {
		opts.TabletOptions.WALSyncMode = e.walSyncMode
	}
	if opts.TabletOptions.WALSyncInterval == 0 {
		opts.TabletOptions.WALSyncInterval = e.walSyncInterval
	}
	if opts.TabletOptions.Backend == nil {
		opts.TabletOptions.Backend = e.backend
	}
	tbl, err := createTable(tableDir, name, opts, e.logger, e.cache, e.publishRFile)
	if err != nil {
		return err
	}
	e.tables[name] = tbl
	return nil
}

// Write applies mutations to the named table. Mutations are routed to
// the correct tablet based on the table's SplitPolicy.
func (e *Engine) Write(table string, mutations []*cclient.Mutation) error {
	e.mu.RLock()
	tbl, ok := e.tables[table]
	e.mu.RUnlock()
	if !ok {
		return fmt.Errorf("engine: table %q not found", table)
	}
	if err := tbl.write(mutations); err != nil {
		return err
	}
	e.metrics.writes.Add(1)
	e.metrics.mutations.Add(uint64(len(mutations)))
	var cells uint64
	for _, m := range mutations {
		cells += uint64(m.Size())
	}
	e.metrics.cellsWritten.Add(cells)
	return nil
}

// Condition and ConditionalMutation expose the tablet CAS model at the engine
// boundary without duplicating its locking or persistence implementation.
type Condition = tablet.Condition
type ConditionKind = tablet.ConditionKind
type ConditionalMutation = tablet.ConditionalMutation

const (
	ConditionAbsent                        = tablet.ConditionAbsent
	ConditionValueEquals                   = tablet.ConditionValueEquals
	ConditionLatestValueAndTimestampEquals = tablet.ConditionLatestValueAndTimestampEquals
)

// ConditionalWrite evaluates and applies each mutation atomically at its
// owning tablet. Results align with mutations and are true only when accepted.
func (e *Engine) ConditionalWrite(table string, mutations []ConditionalMutation) ([]bool, error) {
	for mutationIndex, mutation := range mutations {
		if mutation.Mutation == nil {
			return nil, fmt.Errorf("engine: conditional mutation %d is nil", mutationIndex)
		}
		for conditionIndex, condition := range mutation.Conditions {
			if condition.Kind != ConditionAbsent &&
				condition.Kind != ConditionValueEquals &&
				condition.Kind != ConditionLatestValueAndTimestampEquals {
				return nil, fmt.Errorf("engine: conditional mutation %d condition %d has unsupported kind %d", mutationIndex, conditionIndex, condition.Kind)
			}
		}
	}
	e.mu.RLock()
	tbl, ok := e.tables[table]
	e.mu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("engine: table %q not found", table)
	}
	results, err := tbl.conditionalWrite(mutations)
	if err != nil {
		return nil, err
	}
	e.metrics.writes.Add(1)
	var accepted, cells uint64
	for i, mutation := range mutations {
		if results[i] {
			accepted++
			cells += uint64(mutation.Mutation.Size())
		}
	}
	e.metrics.mutations.Add(accepted)
	e.metrics.cellsWritten.Add(cells)
	return results, nil
}

// ScanOptions configures a scan.
type ScanOptions struct {
	// Stack is the iterator stack applied above the merge. Empty means
	// no filtering — raw cells are returned.
	Stack []iterrt.IterSpec

	// Authorizations is the caller's visibility auth set. Nil means
	// system context — all cells visible.
	Authorizations [][]byte

	// ColumnFamilies restricts the scan to these CFs. Nil = all CFs.
	ColumnFamilies [][]byte

	// ColumnFamiliesInclusive: true = only these CFs; false = exclude these CFs.
	// Follows the SKVI Seek contract.
	ColumnFamiliesInclusive bool
}

// Scan returns a Scanner over the named table for the given range.
// When the table has multiple tablets, the scan merges results from
// all tablets whose range overlaps the requested range — each tablet
// is scanned in its own goroutine for parallelism.
func (e *Engine) Scan(tableName string, r iterrt.Range, opts ScanOptions) (*Scanner, error) {
	e.mu.RLock()
	tbl, ok := e.tables[tableName]
	e.mu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("engine: table %q not found", tableName)
	}
	e.metrics.scans.Add(1)
	return tbl.scan(r, opts)
}

// SetTableFileFormat changes the immutable format used by future flushes and
// compactions. Existing RFile and Parquet files remain readable together.
func (e *Engine) SetTableFileFormat(tableName string, format tablet.FileFormat) error {
	return e.SetTableStorageFormat(tableName, StorageFormat(format))
}

// SetTableStorageFormat changes the immutable format through the engine-level,
// format-neutral API. Existing immutable files remain readable during migration.
func (e *Engine) SetTableStorageFormat(tableName string, format StorageFormat) error {
	parsed, err := ParseStorageFormat(string(format))
	if err != nil {
		return err
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	tbl, ok := e.tables[tableName]
	if !ok {
		return fmt.Errorf("engine: table %q not found", tableName)
	}
	return tbl.setFileFormat(tablet.FileFormat(parsed))
}

func (e *Engine) SetTableTargetEmbeddingSpace(tableName, identity string) error {
	e.mu.RLock()
	tbl, ok := e.tables[tableName]
	e.mu.RUnlock()
	if !ok {
		return fmt.Errorf("engine: table %q not found", tableName)
	}
	return tbl.setTargetEmbedding(identity)
}

func (e *Engine) TableTargetEmbeddingSpace(tableName string) (string, error) {
	e.mu.RLock()
	tbl, ok := e.tables[tableName]
	e.mu.RUnlock()
	if !ok {
		return "", fmt.Errorf("engine: table %q not found", tableName)
	}
	return tbl.targetEmbedding(), nil
}

// SetTableDefaultEmbedding changes the actual embedding state stamped on
// future immutable files. It does not relabel existing files.
func (e *Engine) SetTableDefaultEmbedding(
	tableName string, state embeddingspace.FileState,
) error {
	e.mu.RLock()
	tbl, ok := e.tables[tableName]
	e.mu.RUnlock()
	if !ok {
		return fmt.Errorf("engine: table %q not found", tableName)
	}
	return tbl.setDefaultEmbedding(state)
}

// EmbeddingStateSnapshot reports the immutable file states that an exact
// vector scan would span, plus cells still resident in unlabelled memtables.
type EmbeddingStateSnapshot struct {
	Files            []tablet.EmbeddingFileState
	UnflushedCells   int
	DefaultEmbedding embeddingspace.FileState
}

// TableEmbeddingStateSnapshot obtains a deterministic per-file snapshot
// without scanning file bodies.
func (e *Engine) TableEmbeddingStateSnapshot(
	ctx context.Context, tableName string,
) (EmbeddingStateSnapshot, error) {
	e.mu.RLock()
	tbl, ok := e.tables[tableName]
	e.mu.RUnlock()
	if !ok {
		return EmbeddingStateSnapshot{}, fmt.Errorf(
			"engine: table %q not found", tableName)
	}
	files, unflushed, defaultEmbedding, err := tbl.embeddingStateSnapshot(ctx)
	if err != nil {
		return EmbeddingStateSnapshot{}, err
	}
	return EmbeddingStateSnapshot{
		Files: files, UnflushedCells: unflushed,
		DefaultEmbedding: defaultEmbedding,
	}, nil
}

// ValidateExactVectorSpace fails closed unless one raw query vector can be
// compared with every immutable file in the scan snapshot.
func (e *Engine) ValidateExactVectorSpace(
	ctx context.Context, tableName, queryIdentity string,
) error {
	snapshot, err := e.TableEmbeddingStateSnapshot(ctx, tableName)
	if err != nil {
		return err
	}
	if err := embeddingspace.ValidateQueryStates(
		"exact vector query", queryIdentity); err != nil {
		return err
	}
	if snapshot.UnflushedCells > 0 {
		if err := embeddingspace.ValidateQueryStates(
			"exact vector query over unflushed cells",
			queryIdentity,
			snapshot.DefaultEmbedding,
		); err != nil {
			return err
		}
	}
	for _, file := range snapshot.Files {
		if err := embeddingspace.ValidateQueryStates(
			"exact vector query", queryIdentity, file.State); err != nil {
			return fmt.Errorf("%w: file %q", err, file.Path)
		}
	}
	return nil
}

// MigrateTableStorageFormat atomically selects the write format, flushes, and
// compacts a table so concurrent migrations cannot report a format they did
// not persist.
func (e *Engine) MigrateTableStorageFormat(tableName string, format StorageFormat, stack []iterrt.IterSpec) error {
	parsed, err := ParseStorageFormat(string(format))
	if err != nil {
		return err
	}
	e.mu.RLock()
	tbl, ok := e.tables[tableName]
	e.mu.RUnlock()
	if !ok {
		return fmt.Errorf("engine: table %q not found", tableName)
	}
	if err := tbl.migrate(tablet.FileFormat(parsed), stack); err != nil {
		return err
	}
	e.metrics.flushes.Add(1)
	e.metrics.compactions.Add(1)
	return nil
}

// StoragePolicy describes the configured write format and the formats
// currently read from the table's authoritative immutable generation.
type StoragePolicy struct {
	Workload    WorkloadProfile
	WriteFormat StorageFormat
	ReadFormats []StorageFormat
	Mixed       bool
	Role        string
}

// TableStoragePolicy returns a stable snapshot suitable for status and EXPLAIN.
func (e *Engine) TableStoragePolicy(tableName string) (StoragePolicy, error) {
	e.mu.RLock()
	defer e.mu.RUnlock()
	tbl, ok := e.tables[tableName]
	if !ok {
		return StoragePolicy{}, fmt.Errorf("engine: table %q not found", tableName)
	}
	policy := StoragePolicy{
		WriteFormat: StorageFormat(tbl.fileFormat()),
		Role:        "authoritative",
	}
	if policy.WriteFormat == StorageFormatParquet {
		policy.Workload = WorkloadAnalytical
	} else {
		policy.Workload = WorkloadOperational
	}
	seen := make(map[StorageFormat]struct{})
	for _, file := range tbl.rfiles() {
		seen[StorageFormat(fileFormatForPath(file.Path))] = struct{}{}
	}
	for format := range seen {
		policy.ReadFormats = append(policy.ReadFormats, format)
	}
	sort.Slice(policy.ReadFormats, func(i, j int) bool { return policy.ReadFormats[i] < policy.ReadFormats[j] })
	policy.Mixed = len(policy.ReadFormats) > 1
	return policy, nil
}

// ScanHosted runs a scan whose top-of-stack iterators are hosted above a
// re-seekable whole-table merge (rather than per-tablet). Use it for
// iterators that must re-seek across the entire row space — e.g. the
// term-index pushdown (iterrt.IterTermIndex), where a posting row and the
// primary rows it references may live in different tablets. A VersioningIterator
// is always applied beneath topStack; topStack is composed above it.
func (e *Engine) ScanHosted(tableName string, r iterrt.Range, opts ScanOptions, topStack []iterrt.IterSpec) (*Scanner, error) {
	return e.ScanHostedContext(
		context.Background(), tableName, r, opts, topStack)
}

// ScanHostedContext is ScanHosted with cancellation for metadata reads and
// source construction.
func (e *Engine) ScanHostedContext(
	ctx context.Context,
	tableName string,
	r iterrt.Range,
	opts ScanOptions,
	topStack []iterrt.IterSpec,
) (*Scanner, error) {
	e.mu.RLock()
	tbl, ok := e.tables[tableName]
	e.mu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("engine: table %q not found", tableName)
	}
	e.metrics.scans.Add(1)
	return tbl.scanHosted(ctx, r, opts, topStack)
}

// RowVisitor receives each cell of each looked-up row. idx is the row's
// position in the rows slice passed to LookupRows. key and value are only
// valid for the duration of the call — copy any bytes retained past it.
type RowVisitor func(idx int, key *iterrt.Key, value []byte)

// LookupRows fetches the cells of many rows efficiently, reusing a single
// re-seekable scan stack per tablet instead of building a fresh Scanner
// for every row. This amortizes the per-lookup memtable+RFile merge and
// stack-build cost across the whole batch — the fast path for graph-style
// point reads such as expanding a BFS frontier.
//
// Cells are delivered to visit in ascending key order (not rows order);
// use the idx argument to map a cell back to its requested row. opts
// ColumnFamilies/Stack/Authorizations apply exactly as in Scan.
func (e *Engine) LookupRows(tableName string, rows [][]byte, opts ScanOptions, visit RowVisitor) error {
	e.mu.RLock()
	tbl, ok := e.tables[tableName]
	e.mu.RUnlock()
	if !ok {
		return fmt.Errorf("engine: table %q not found", tableName)
	}
	return tbl.lookupRows(rows, opts, visit)
}

// Neighbor is one resolved out-edge of a graph node: the target row id
// (the edge cell's column qualifier) plus the edge's value, timestamp,
// and visibility. Deleted edges are already suppressed.
type Neighbor struct {
	Target    []byte
	Value     []byte
	Timestamp int64
	Vis       []byte
}

// Neighbors returns the out-edges of each requested row in the given edge
// column family. results[i] holds the neighbors of rows[i] (sorted by
// target), or nil when rows[i] has none. When every on-disk file carries
// a shoal.adjacency index (see tablet.Options.AdjacencyEdgeCF) the lookup
// is served from that CSR index — a binary search plus a contiguous slice
// read per node — instead of a full merge/versioning scan. Lookups fan
// out across NumCPU, making frontier expansion embarrassingly parallel.
//
// Semantics match a Scan over (row, edgeCF): newest version per
// (target, visibility) wins, deletes suppress, and opts.Authorizations
// gate visibility. Files written without the index fall back to a scan
// transparently.
func (e *Engine) Neighbors(tableName string, rows [][]byte, edgeCF []byte, opts ScanOptions) ([][]Neighbor, error) {
	e.mu.RLock()
	tbl, ok := e.tables[tableName]
	e.mu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("engine: table %q not found", tableName)
	}
	return tbl.neighbors(rows, edgeCF, opts)
}

// Flush forces all memtables in the named table to disk.
func (e *Engine) Flush(table string) error {
	e.mu.RLock()
	tbl, ok := e.tables[table]
	e.mu.RUnlock()
	if !ok {
		return fmt.Errorf("engine: table %q not found", table)
	}
	if err := tbl.flush(); err != nil {
		return err
	}
	e.metrics.flushes.Add(1)
	return nil
}

// Compact runs compaction on all tablets in the named table with the
// given iterator stack. This is where application-level iterators
// (decay, prune, dedup) run.
func (e *Engine) Compact(table string, stack []iterrt.IterSpec) error {
	e.mu.RLock()
	tbl, ok := e.tables[table]
	e.mu.RUnlock()
	if !ok {
		return fmt.Errorf("engine: table %q not found", table)
	}
	if err := tbl.compact(stack); err != nil {
		return err
	}
	e.metrics.compactions.Add(1)
	return nil
}

// TableNames returns the names of all tables.
func (e *Engine) TableNames() []string {
	e.mu.RLock()
	defer e.mu.RUnlock()
	names := make([]string, 0, len(e.tables))
	for name := range e.tables {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// TableStat summarizes one table's physical footprint.
type TableStat struct {
	Name    string
	Tablets int
	RFiles  int
}

// Stats returns a per-table snapshot of tablet and RFile counts, ordered by
// table name. It reflects the in-memory file set each tablet currently tracks
// (it does not re-list the backend).
func (e *Engine) Stats() []TableStat {
	e.mu.RLock()
	defer e.mu.RUnlock()
	out := make([]TableStat, 0, len(e.tables))
	for name, tbl := range e.tables {
		rfiles := 0
		for _, tb := range tbl.tablets {
			rfiles += len(tb.RFiles())
		}
		out = append(out, TableStat{Name: name, Tablets: len(tbl.tablets), RFiles: rfiles})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// Metrics is a point-in-time snapshot of the engine's cumulative operation
// counters since Open.
type Metrics struct {
	Writes       uint64 `json:"writes"`
	Mutations    uint64 `json:"mutations"`
	CellsWritten uint64 `json:"cells_written"`
	Scans        uint64 `json:"scans"`
	Flushes      uint64 `json:"flushes"`
	Compactions  uint64 `json:"compactions"`
}

// Metrics returns a snapshot of the engine's operation counters.
func (e *Engine) Metrics() Metrics {
	return Metrics{
		Writes:       e.metrics.writes.Load(),
		Mutations:    e.metrics.mutations.Load(),
		CellsWritten: e.metrics.cellsWritten.Load(),
		Scans:        e.metrics.scans.Load(),
		Flushes:      e.metrics.flushes.Load(),
		Compactions:  e.metrics.compactions.Load(),
	}
}

// Close flushes and closes all tables.
func (e *Engine) Close() error {
	e.mu.Lock()
	defer e.mu.Unlock()
	var firstErr error
	for name, tbl := range e.tables {
		if err := tbl.close(); err != nil && firstErr == nil {
			firstErr = fmt.Errorf("engine: close table %s: %w", name, err)
		}
	}
	return firstErr
}

// Scanner is a pull-based iterator over scan results from one or more
// tablets. When multiple tablets contribute, their results are merged
// in key order.
type Scanner struct {
	scanners []*tablet.Scanner
	merge    iterrt.SortedKeyValueIterator
	closers  []func()
}

// Next reports whether there is a current key/value pair.
func (s *Scanner) Next() bool {
	return s.merge.HasTop()
}

// Key returns the current key.
func (s *Scanner) Key() *wire.Key {
	return s.merge.GetTopKey()
}

// Value returns the current value.
func (s *Scanner) Value() []byte {
	return s.merge.GetTopValue()
}

// Advance moves to the next key/value pair.
func (s *Scanner) Advance() error {
	return s.merge.Next()
}

// Close releases all resources.
func (s *Scanner) Close() {
	for _, sc := range s.scanners {
		sc.Close()
	}
	for _, c := range s.closers {
		c()
	}
}

// --- errors ---

// ErrTableNotFound is returned when the named table doesn't exist.
var ErrTableNotFound = errors.New("engine: table not found")

// ErrTableExists is returned when creating a table that already exists.
var ErrTableExists = errors.New("engine: table already exists")

// Ensure wire.Key and bytes are imported (used transitively).
var (
	_ = wire.Key{}
	_ = bytes.Compare
)

// ErrEmbeddingStateChangeWithUnflushedData prevents relabelling cells that
// were accepted under one producer state but have not yet acquired per-file
// metadata.
var ErrEmbeddingStateChangeWithUnflushedData = errors.New(
	"engine: cannot change default embedding state with unflushed data")
