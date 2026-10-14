// Licensed to the Apache Software Foundation (ASF) under one
// or more contributor license agreements. See the NOTICE file
// distributed with this work for additional information
// regarding copyright ownership. The ASF licenses this file
// to you under the Apache License, Version 2.0 (the
// "License"); you may not use this file except in compliance
// with the License. You may obtain a copy of the License at
//
//   http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// Package mincauthority coordinates the Accumulo-authoritative metadata
// commit for a hosted tablet minor compaction.
package mincauthority

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/phrocker/shoal-oss/internal/embeddingspace"
	"github.com/phrocker/shoal-oss/internal/ingestrouter"
	"github.com/phrocker/shoal-oss/internal/rfile"
	"github.com/phrocker/shoal-oss/internal/rfile/bcfile"
	"github.com/phrocker/shoal-oss/internal/rfile/bcfile/block"
	"github.com/phrocker/shoal-oss/internal/walauthority"
)

var (
	ErrInvalidConfig            = errors.New("mincauthority: invalid configuration")
	ErrInvalidSnapshot          = errors.New("mincauthority: invalid snapshot")
	ErrStaleOwner               = errors.New("mincauthority: stale owner")
	ErrCorruptOutput            = errors.New("mincauthority: corrupt RFile output")
	ErrConcurrentMetadataChange = errors.New("mincauthority: concurrent metadata change")
	ErrMetadataInconsistent     = errors.New("mincauthority: inconsistent metadata commit state")
	ErrAmbiguous                = errors.New("mincauthority: outcome is ambiguous")
)

// Cell is one immutable memtable cell included in a minor-compaction snapshot.
type Cell struct {
	Key   rfile.Key
	Value []byte
}

// Snapshot is an immutable, resumable memtable generation. Prepare must swap
// the active memtable and roll the WAL atomically at Boundary, so later writes
// use another memtable/WAL and cannot enter Cells or CoveredWALs.
type Snapshot struct {
	ID          string
	Extent      ingestrouter.Extent
	Fence       ingestrouter.Fence
	Boundary    int64
	TabletTime  string
	Embedding   embeddingspace.FileState
	Cells       []Cell
	CoveredWALs []walauthority.Reference
}

// Snapshotter is the future hosted-tablet/TabletIngest integration seam.
// Prepare is idempotent by operationID and must return the same retained
// snapshot after a process restart. Complete is idempotent and may release the
// immutable memtable and delete WAL bytes only after metadata is committed.
type Snapshotter interface {
	Prepare(context.Context, string, ingestrouter.Extent, ingestrouter.Fence) (Snapshot, error)
	Complete(context.Context, string, DataFile) error
}

// FenceVerifier verifies the live ServiceLock, manager generation, hosted
// assignment attempt, and non-unloading state.
type FenceVerifier interface {
	Verify(context.Context, ingestrouter.Fence) error
}

// DataFile is the exact authoritative RFile metadata value.
type DataFile struct {
	Path       string
	Format     string
	Size       int64
	Entries    int64
	Checksum   string
	Embedding  embeddingspace.FileState
	StartRow   []byte
	EndRow     []byte
	SnapshotID string
	Boundary   int64
}

// MetadataCommit is one atomic conditional mutation. Implementations must
// condition on Fence, require every RemoveWAL to still be present byte-for-
// byte, add File, and remove only RemoveWAL. Unrelated files and WALs must be
// preserved.
type MetadataCommit struct {
	OperationID string
	Extent      ingestrouter.Extent
	Fence       ingestrouter.Fence
	File        DataFile
	RemoveWALs  []walauthority.Reference
	TabletTime  string
}

type CommitOutcome uint8

const (
	CommitApplied CommitOutcome = iota + 1
	CommitRejected
	CommitUnknown
)

// MetadataState is the authoritative state used to reconcile a lost response.
type MetadataState struct {
	Files []DataFile
	WALs  []walauthority.Reference
	Time  string
}

type MetadataAuthority interface {
	Commit(context.Context, MetadataCommit) (CommitOutcome, error)
	Read(context.Context, ingestrouter.Extent) (MetadataState, error)
}

// OutputStore durably publishes immutable objects. Publish may return an error
// after the object became visible; the coordinator always reads it back.
type OutputStore interface {
	Publish(context.Context, string, []byte) error
	Read(context.Context, string) ([]byte, error)
	Remove(context.Context, string) error
}

// Phase is a durable state-machine checkpoint.
type Phase uint8

const (
	PhaseSnapshotted Phase = iota + 1
	PhasePublished
	PhaseValidated
	PhaseCommitted
	PhaseComplete
)

type State struct {
	OperationID         string
	Extent              ingestrouter.Extent
	Fence               ingestrouter.Fence
	SnapshotID          string
	Boundary            int64
	TabletTime          string
	SnapshotCells       []Cell
	SnapshotFingerprint string
	CoveredWALs         []walauthority.Reference
	File                DataFile
	Phase               Phase
}

// StateStore must make Save durable before returning. Keeping PhaseComplete is
// intentional: a duplicate RPC must not create a second snapshot/output.
type StateStore interface {
	Load(context.Context, string) (*State, error)
	Save(context.Context, State) error
}

type Config struct {
	Root             string
	Extent           ingestrouter.Extent
	Fence            ingestrouter.Fence
	Snapshots        Snapshotter
	Verifier         FenceVerifier
	Metadata         MetadataAuthority
	Outputs          OutputStore
	States           StateStore
	ReconcileTimeout time.Duration
	WriterOptions    rfile.WriterOptions

	// DefaultEmbedding is the embedding-space state to record for an
	// output whose Snapshotter declared none.
	//
	// This is a write path: whatever lands here is stamped into the
	// RFile footer meta block and into the file's metadata column, so it
	// becomes durable evidence. Defaulting it to no_embeddings would
	// manufacture the positive assertion "this file holds no vectors"
	// out of a provider that simply said nothing, which is the claim
	// that merges freely with every embedding space.
	//
	// The zero value means "no default configured", and an undeclared
	// snapshot is then recorded as embeddingspace.Unknown() — absent
	// information, failing closed. An operator who knows their ingest
	// pipeline emits no vectors may set this to
	// embeddingspace.NoEmbeddings() and have that assertion recorded
	// truthfully, because they are the ones making it.
	//
	// It must be a valid embeddingspace.FileState when set; New rejects
	// anything else rather than writing an unreadable column.
	DefaultEmbedding embeddingspace.FileState
}

// Coordinator serializes minor compactions for one hosted tablet.
type Coordinator struct {
	mu  sync.Mutex
	cfg Config
}

func New(cfg Config) (*Coordinator, error) {
	if cfg.Root == "" || cfg.Snapshots == nil || cfg.Verifier == nil ||
		cfg.Metadata == nil || cfg.Outputs == nil || cfg.States == nil ||
		cfg.Extent.Validate() != nil || !cfg.Fence.Valid() {
		return nil, ErrInvalidConfig
	}
	if cfg.ReconcileTimeout <= 0 {
		cfg.ReconcileTimeout = 5 * time.Second
	}
	if cfg.DefaultEmbedding != (embeddingspace.FileState{}) {
		// Compared against the whole zero value, not just State: a
		// partial FileState{Identity: "x"} is malformed configuration,
		// not an absent one, and must be rejected here rather than
		// silently ignored as if nothing had been configured.
		if err := cfg.DefaultEmbedding.Validate(); err != nil {
			return nil, fmt.Errorf("%w: default embedding: %w", ErrInvalidConfig, err)
		}
	}
	if cfg.WriterOptions.EmbeddingSpace != (embeddingspace.FileState{}) {
		// defaultEmbedding falls back to this, so it is configuration
		// with the same authority and must fail the same way. Without
		// this a malformed value is accepted here, copied into the
		// snapshot, and only surfaces at the first flush as
		// ErrInvalidSnapshot — a configuration error reported as
		// corrupt durable state.
		if err := cfg.WriterOptions.EmbeddingSpace.Validate(); err != nil {
			return nil, fmt.Errorf("%w: writer embedding space: %w", ErrInvalidConfig, err)
		}
	}
	return &Coordinator{cfg: cfg}, nil
}

// defaultEmbedding is the state recorded for a snapshot whose provider
// declared none. An operator may configure a positive assertion; with no
// configuration the output is labelled unknown rather than having a
// claim invented for it.
//
// WriterOptions.EmbeddingSpace is honoured as a second source because it
// is operator configuration too, and because before issue #274 it was
// the only way to say anything here. Reading it as the default keeps
// that intent working, and keeps the footer and the metadata column
// saying the same thing — previously a set WriterOptions.EmbeddingSpace
// reached the footer while the metadata column recorded the fabricated
// no_embeddings, which is a pair the next integrity check rejects.
func (c *Coordinator) defaultEmbedding() embeddingspace.FileState {
	if c.cfg.DefaultEmbedding != (embeddingspace.FileState{}) {
		return c.cfg.DefaultEmbedding
	}
	if c.cfg.WriterOptions.EmbeddingSpace != (embeddingspace.FileState{}) {
		return c.cfg.WriterOptions.EmbeddingSpace
	}
	return embeddingspace.Unknown()
}

// Run advances operationID until its RFile is committed and the retained
// snapshot is released. Every external step is retryable after a crash.
func (c *Coordinator) Run(ctx context.Context, operationID string) (DataFile, error) {
	if operationID == "" {
		return DataFile{}, fmt.Errorf("%w: empty operation id", ErrInvalidConfig)
	}
	if err := ctx.Err(); err != nil {
		return DataFile{}, err
	}
	c.mu.Lock()
	defer c.mu.Unlock()

	state, err := c.cfg.States.Load(ctx, operationID)
	if err != nil {
		return DataFile{}, err
	}
	if state != nil {
		if state.OperationID != operationID || !state.Extent.Equal(c.cfg.Extent) || state.Fence != c.cfg.Fence {
			return DataFile{}, fmt.Errorf("%w: checkpoint identity mismatch", ErrInvalidSnapshot)
		}
		if state.Phase == PhaseComplete {
			return cloneFile(state.File), nil
		}
	}

	if err := c.verify(ctx); err != nil {
		return DataFile{}, err
	}

	var snapshot Snapshot
	var encoded []byte
	if state == nil || state.Phase < PhaseCommitted {
		snapshot, err = c.cfg.Snapshots.Prepare(ctx, operationID, c.cfg.Extent, c.cfg.Fence)
		if err != nil {
			return DataFile{}, err
		}
		if snapshot.Embedding == (embeddingspace.FileState{}) {
			// Only the exact zero value means "the provider declared
			// nothing". A partial state such as {Identity: "x"} is
			// malformed provider output and must reach validateSnapshot
			// and be rejected, not be quietly replaced by the default.
			if state != nil && state.File.Embedding != (embeddingspace.FileState{}) {
				// Resuming replays a decision that was already made and
				// checkpointed, so it must not be re-derived from
				// configuration that may have changed since — including
				// across the upgrade that introduced DefaultEmbedding,
				// where every in-flight checkpoint carries the old
				// implicit label. Re-deriving would make equalFile below
				// report "resumed snapshot changed" and leave the tablet
				// unable to open.
				snapshot.Embedding = state.File.Embedding
			} else {
				snapshot.Embedding = c.defaultEmbedding()
			}
		}
		if err := validateSnapshot(snapshot, c.cfg.Extent, c.cfg.Fence); err != nil {
			return DataFile{}, err
		}
		writerOptions := c.cfg.WriterOptions
		// The snapshot's resolved state is the single label for this
		// output: it is stamped into the RFile footer here and recorded
		// in the metadata column by describeFile below, and the next
		// compaction cross-checks the two. Letting WriterOptions carry a
		// different value would publish a pair guaranteed to fail that
		// check, so the declaration wins outright rather than only when
		// the writer option happens to be empty.
		writerOptions.EmbeddingSpace = snapshot.Embedding
		encoded, err = encodeSnapshot(snapshot, writerOptions)
		if err != nil {
			return DataFile{}, err
		}
		file := describeFile(c.outputPath(operationID), snapshot, encoded)
		fingerprint := snapshotFingerprint(snapshot)
		if state == nil {
			state = &State{
				OperationID: operationID, Extent: cloneExtent(c.cfg.Extent), Fence: c.cfg.Fence,
				SnapshotID: snapshot.ID, Boundary: snapshot.Boundary,
				TabletTime:          snapshot.TabletTime,
				SnapshotCells:       cloneCells(snapshot.Cells),
				SnapshotFingerprint: fingerprint, CoveredWALs: cloneRefs(snapshot.CoveredWALs),
				File: file, Phase: PhaseSnapshotted,
			}
			if err := c.cfg.States.Save(ctx, *state); err != nil {
				return DataFile{}, err
			}
		} else if state.SnapshotID != snapshot.ID || state.Boundary != snapshot.Boundary ||
			state.TabletTime != snapshot.TabletTime ||
			state.SnapshotFingerprint != fingerprint || !equalFile(state.File, file) ||
			!equalRefs(state.CoveredWALs, snapshot.CoveredWALs) {
			return DataFile{}, fmt.Errorf("%w: resumed snapshot changed", ErrInvalidSnapshot)
		}
	}

	if state.Phase < PhasePublished {
		publishErr := c.cfg.Outputs.Publish(ctx, state.File.Path, encoded)
		if err := c.validatePublished(ctx, state.File); err != nil {
			if publishErr != nil {
				return DataFile{}, errors.Join(publishErr, err)
			}
			return DataFile{}, err
		}
		state.Phase = PhasePublished
		if err := c.cfg.States.Save(ctx, *state); err != nil {
			return DataFile{}, err
		}
	}

	if state.Phase < PhaseValidated {
		if err := c.validatePublished(ctx, state.File); err != nil {
			return DataFile{}, err
		}
		state.Phase = PhaseValidated
		if err := c.cfg.States.Save(ctx, *state); err != nil {
			return DataFile{}, err
		}
	}

	if state.Phase < PhaseCommitted {
		if err := c.verify(ctx); err != nil {
			return DataFile{}, err
		}
		outcome, commitErr := c.cfg.Metadata.Commit(ctx, MetadataCommit{
			OperationID: operationID, Extent: cloneExtent(c.cfg.Extent), Fence: c.cfg.Fence,
			File: cloneFile(state.File), RemoveWALs: cloneRefs(state.CoveredWALs),
			TabletTime: state.TabletTime,
		})
		switch outcome {
		case CommitApplied:
			// The CAS itself fences ownership; no post-CAS owner check can
			// safely roll back an already-authoritative commit.
		case CommitRejected:
			applied, reconcileErr := c.reconcileMetadata(state)
			if reconcileErr != nil {
				return DataFile{}, errors.Join(commitErr, reconcileErr)
			}
			if !applied {
				return DataFile{}, errors.Join(ErrConcurrentMetadataChange, commitErr)
			}
		case CommitUnknown:
			applied, reconcileErr := c.reconcileMetadata(state)
			if reconcileErr != nil {
				return DataFile{}, errors.Join(ErrAmbiguous, commitErr, reconcileErr)
			}
			if !applied {
				return DataFile{}, errors.Join(ErrAmbiguous, commitErr)
			}
		default:
			return DataFile{}, fmt.Errorf("%w: invalid metadata outcome %d", ErrMetadataInconsistent, outcome)
		}
		state.Phase = PhaseCommitted
		if err := c.cfg.States.Save(ctx, *state); err != nil {
			return DataFile{}, err
		}
	}

	if state.Phase < PhaseComplete {
		if err := c.cfg.Snapshots.Complete(ctx, state.SnapshotID, cloneFile(state.File)); err != nil {
			return DataFile{}, err
		}
		state.Phase = PhaseComplete
		if err := c.cfg.States.Save(ctx, *state); err != nil {
			return DataFile{}, err
		}
	}
	return cloneFile(state.File), nil
}

func (c *Coordinator) verify(ctx context.Context) error {
	if err := c.cfg.Verifier.Verify(ctx, c.cfg.Fence); err != nil {
		return errors.Join(ErrStaleOwner, err)
	}
	return nil
}

func (c *Coordinator) reconcileMetadata(state *State) (bool, error) {
	ctx, cancel := context.WithTimeout(context.Background(), c.cfg.ReconcileTimeout)
	defer cancel()
	current, err := c.cfg.Metadata.Read(ctx, c.cfg.Extent)
	if err != nil {
		return false, err
	}
	filePresent := false
	for _, file := range current.Files {
		if file.Path == state.File.Path {
			if !equalAuthoritativeFile(file, state.File) {
				return false, fmt.Errorf("%w: output path has different metadata", ErrMetadataInconsistent)
			}
			filePresent = true
		}

	}
	remaining := 0
	for _, covered := range state.CoveredWALs {
		for _, ref := range current.WALs {
			if ref == covered {
				remaining++
				break
			}
		}
	}
	switch {
	case filePresent && remaining == 0:
		if state.TabletTime != "" && current.Time != state.TabletTime {
			return false, fmt.Errorf("%w: tablet time is %q, want %q",
				ErrMetadataInconsistent, current.Time, state.TabletTime)
		}
		return true, nil
	case !filePresent && remaining == len(state.CoveredWALs):
		return false, nil
	default:
		return false, fmt.Errorf("%w: filePresent=%t coveredWALsRemaining=%d/%d",
			ErrMetadataInconsistent, filePresent, remaining, len(state.CoveredWALs))
	}
}

// Accumulo persists the path in the StoredTabletFile qualifier and size/entry
// count in DataFileValue. A newly flushed file is a whole-file reference, so
// its first/last data rows are local validation facts, not metadata fences.
func equalAuthoritativeFile(a, b DataFile) bool {
	return a.Path == b.Path && a.Size == b.Size && a.Entries == b.Entries
}

func (c *Coordinator) validatePublished(ctx context.Context, expected DataFile) error {
	data, err := c.cfg.Outputs.Read(ctx, expected.Path)
	if err != nil {
		return err
	}
	actual := sha256.Sum256(data)
	if int64(len(data)) != expected.Size || hex.EncodeToString(actual[:]) != expected.Checksum {
		return fmt.Errorf("%w: size/checksum mismatch for %s", ErrCorruptOutput, expected.Path)
	}
	bc, err := bcfile.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return fmt.Errorf("%w: open BCFile: %v", ErrCorruptOutput, err)
	}
	reader, err := rfile.Open(bc, block.Default())
	if err != nil {
		return fmt.Errorf("%w: open RFile: %v", ErrCorruptOutput, err)
	}
	defer reader.Close()
	var first, last []byte
	var count int64
	var previous *rfile.Key
	for {
		key, _, err := reader.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return fmt.Errorf("%w: read RFile: %v", ErrCorruptOutput, err)
		}
		if !c.cfg.Extent.Contains(key.Row) {
			return fmt.Errorf("%w: row %x is outside extent", ErrCorruptOutput, key.Row)
		}
		if previous != nil && previous.Compare(key) > 0 {
			return fmt.Errorf("%w: keys are out of order", ErrCorruptOutput)
		}
		if count == 0 {
			first = append([]byte(nil), key.Row...)
		}
		last = append(last[:0], key.Row...)
		previous = key.Clone()
		count++
	}
	if count != expected.Entries || !bytes.Equal(first, expected.StartRow) || !bytes.Equal(last, expected.EndRow) {
		return fmt.Errorf("%w: key range/count mismatch", ErrCorruptOutput)
	}
	return nil
}

func validateSnapshot(s Snapshot, extent ingestrouter.Extent, fence ingestrouter.Fence) error {
	if s.ID == "" || s.Boundary < 1 || !s.Extent.Equal(extent) || s.Fence != fence || len(s.Cells) == 0 || len(s.CoveredWALs) == 0 {
		return ErrInvalidSnapshot
	}
	seen := make(map[walauthority.Reference]struct{}, len(s.CoveredWALs))
	for _, ref := range s.CoveredWALs {
		if ref.ID == "" || ref.Path == "" || ref.Qualifier == "" {
			return fmt.Errorf("%w: incomplete WAL reference", ErrInvalidSnapshot)
		}
		if _, ok := seen[ref]; ok {
			return fmt.Errorf("%w: duplicate WAL reference", ErrInvalidSnapshot)
		}
		seen[ref] = struct{}{}
	}
	for _, cell := range s.Cells {
		if !extent.Contains(cell.Key.Row) {
			return fmt.Errorf("%w: row %x outside extent", ErrInvalidSnapshot, cell.Key.Row)
		}
	}
	if err := s.Embedding.Validate(); err != nil {
		return fmt.Errorf("%w: embedding state: %v", ErrInvalidSnapshot, err)
	}
	return nil
}

func encodeSnapshot(s Snapshot, options rfile.WriterOptions) ([]byte, error) {
	cells := cloneCells(s.Cells)
	sort.SliceStable(cells, func(i, j int) bool {
		return cells[i].Key.Compare(&cells[j].Key) < 0
	})
	var out bytes.Buffer
	writer, err := rfile.NewWriter(&out, options)
	if err != nil {
		return nil, err
	}
	for i := range cells {
		if err := writer.Append(&cells[i].Key, cells[i].Value); err != nil {
			return nil, err
		}
	}
	if err := writer.Close(); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}

func describeFile(outputPath string, snapshot Snapshot, data []byte) DataFile {
	sum := sha256.Sum256(data)
	start, end := append([]byte(nil), snapshot.Cells[0].Key.Row...), append([]byte(nil), snapshot.Cells[0].Key.Row...)
	for _, cell := range snapshot.Cells[1:] {
		if bytes.Compare(cell.Key.Row, start) < 0 {
			start = append(start[:0], cell.Key.Row...)
		}
		if bytes.Compare(cell.Key.Row, end) > 0 {
			end = append(end[:0], cell.Key.Row...)
		}
	}
	return DataFile{
		Path: outputPath, Format: "rfile", Size: int64(len(data)), Entries: int64(len(snapshot.Cells)),
		Checksum: hex.EncodeToString(sum[:]), Embedding: snapshot.Embedding,
		StartRow: start, EndRow: end,
		SnapshotID: snapshot.ID, Boundary: snapshot.Boundary,
	}
}

func snapshotFingerprint(snapshot Snapshot) string {
	type digestSnapshot struct {
		ID       string
		Boundary int64
		Time     string
		Cells    []Cell
		WALs     []walauthority.Reference
	}
	value := digestSnapshot{
		ID: snapshot.ID, Boundary: snapshot.Boundary, Time: snapshot.TabletTime,
		Cells: cloneCells(snapshot.Cells), WALs: cloneRefs(snapshot.CoveredWALs),
	}
	sort.SliceStable(value.Cells, func(i, j int) bool { return value.Cells[i].Key.Compare(&value.Cells[j].Key) < 0 })
	sort.Slice(value.WALs, func(i, j int) bool { return value.WALs[i].Qualifier < value.WALs[j].Qualifier })
	data, _ := json.Marshal(value)
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func (c *Coordinator) outputPath(operationID string) string {
	extentDigest := sha256.Sum256([]byte(c.cfg.Extent.Key()))
	opDigest := sha256.Sum256([]byte(operationID))
	return joinPath(c.cfg.Root, c.cfg.Extent.TableID, hex.EncodeToString(extentDigest[:8]),
		hex.EncodeToString(opDigest[:])+".rf")
}

func joinPath(root string, elements ...string) string {
	if strings.Contains(root, "://") {
		return strings.TrimRight(root, "/") + "/" + path.Join(elements...)
	}
	all := append([]string{root}, elements...)
	return filepath.ToSlash(filepath.Join(all...))
}

func cloneCells(in []Cell) []Cell {
	out := make([]Cell, len(in))
	for i := range in {
		out[i] = Cell{Key: *in[i].Key.Clone(), Value: append([]byte(nil), in[i].Value...)}
	}
	return out
}

func cloneRefs(in []walauthority.Reference) []walauthority.Reference {
	return append([]walauthority.Reference(nil), in...)
}

func cloneExtent(e ingestrouter.Extent) ingestrouter.Extent {
	return ingestrouter.Extent{
		TableID: e.TableID, PrevEndRow: append([]byte(nil), e.PrevEndRow...), EndRow: append([]byte(nil), e.EndRow...),
	}
}

func cloneFile(in DataFile) DataFile {
	in.StartRow = append([]byte(nil), in.StartRow...)
	in.EndRow = append([]byte(nil), in.EndRow...)
	return in
}

func equalFile(a, b DataFile) bool {
	return a.Path == b.Path && a.Format == b.Format && a.Size == b.Size &&
		a.Entries == b.Entries && a.Checksum == b.Checksum && a.Embedding == b.Embedding &&
		bytes.Equal(a.StartRow, b.StartRow) && bytes.Equal(a.EndRow, b.EndRow) &&
		a.SnapshotID == b.SnapshotID && a.Boundary == b.Boundary
}

func equalRefs(a, b []walauthority.Reference) bool {
	if len(a) != len(b) {
		return false
	}
	left, right := cloneRefs(a), cloneRefs(b)
	sort.Slice(left, func(i, j int) bool { return left[i].Qualifier < left[j].Qualifier })
	sort.Slice(right, func(i, j int) bool { return right[i].Qualifier < right[j].Qualifier })
	for i := range left {
		if left[i] != right[i] {
			return false
		}
	}
	return true
}
