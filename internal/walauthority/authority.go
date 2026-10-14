// Licensed to the Apache Software Foundation (ASF) under one
// or more contributor license agreements. See the NOTICE file
// distributed with this work for additional information
// regarding copyright ownership. The ASF licenses this file
// to you under the Apache License, Version 2.0 (the
// "License"); you may not use this file except in compliance
// with the License. You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// Package walauthority implements the durable WAL lifecycle for hosted
// Accumulo tablets. SealForMinorCompaction exposes an atomic WAL/memtable
// boundary to internal/mincauthority; Retire may only be called after that
// coordinator's metadata transaction makes every referenced mutation durable
// in its authoritative RFile.
package walauthority

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/phrocker/shoal-oss/internal/ingestrouter"
)

var (
	ErrClosed              = errors.New("walauthority: tablet is closed")
	ErrStaleOwner          = errors.New("walauthority: stale owner")
	ErrIdempotencyConflict = errors.New("walauthority: operation id reused with different mutations")
	ErrAmbiguous           = errors.New("walauthority: operation outcome is ambiguous")
	ErrCorrupt             = errors.New("walauthority: corrupt WAL")
	ErrInvalidConfig       = errors.New("walauthority: invalid configuration")
	ErrInvalidCommit       = errors.New("walauthority: invalid commit")
)

const (
	formatVersion = 1
	maxFrameSize  = 64 << 20
)

// Reference is the exact metadata identity of one WAL. Accumulo 4 encodes a
// tablet log column qualifier as "-/<path>"; implementations must preserve
// Qualifier byte-for-byte when removing the reference.
type Reference struct {
	ID        string
	Path      string
	Qualifier string
}

// MetadataAuthority is the sole authority for tablet WAL references.
// Implementations should condition every mutation on Owner and return
// ErrStaleOwner when the hosted assignment is no longer current.
type MetadataAuthority interface {
	EnsureReference(context.Context, ingestrouter.Extent, ingestrouter.Fence, Reference) error
	HasReference(context.Context, ingestrouter.Extent, ingestrouter.Fence, Reference) (bool, error)
	RemoveReference(context.Context, ingestrouter.Extent, ingestrouter.Fence, Reference) error
	References(context.Context, ingestrouter.Extent) ([]Reference, error)
}

// FenceVerifier checks the live ServiceLock, manager generation, and
// assignment before an owner performs or acknowledges a write.
type FenceVerifier interface {
	Verify(context.Context, ingestrouter.Fence) error
}

// MutationSink installs a durably logged batch into the hosted tablet's
// memtable. Apply must be idempotent by operationID because recovery and an
// ambiguous RPC retry can present the same batch more than once.
type MutationSink interface {
	Apply(context.Context, string, int64, []ingestrouter.Mutation) error
}

// Store is an append-capable, durable WAL store. Append must not return nil
// until the complete frame is stable according to the configured durability.
// A non-nil error may be ambiguous; Authority reconciles it by rereading.
type Store interface {
	Create(context.Context, string, []byte) error
	Append(context.Context, string, []byte) error
	Read(context.Context, string) ([]byte, error)
	Truncate(context.Context, string, int64) error
	Sync(context.Context, string) error
	Remove(context.Context, string) error
}

type Config struct {
	Root             string
	ServerAddress    string
	Extent           ingestrouter.Extent
	Fence            ingestrouter.Fence
	Metadata         MetadataAuthority
	Store            Store
	Verifier         FenceVerifier
	RecoveryVerifier FenceVerifier
	Sink             MutationSink
	ReconcileTimeout time.Duration
	NewID            func() string
	Now              func() time.Time
}

type operation struct {
	Sequence    int64                   `json:"sequence"`
	OperationID string                  `json:"operation_id"`
	SessionID   string                  `json:"session_id"`
	RequestID   string                  `json:"request_id"`
	Extent      ingestrouter.Extent     `json:"extent"`
	Fence       ingestrouter.Fence      `json:"fence"`
	Mutations   []ingestrouter.Mutation `json:"mutations"`
	Digest      string                  `json:"digest"`
}

type header struct {
	Version int                 `json:"version"`
	WALID   string              `json:"wal_id"`
	Path    string              `json:"path"`
	Extent  ingestrouter.Extent `json:"extent"`
	Owner   ingestrouter.Fence  `json:"owner"`
	Created int64               `json:"created_unix_nano"`
}

type wal struct {
	ref Reference
}

// RecoveryReport describes exactly what was discovered and replayed.
type RecoveryReport struct {
	References      int
	Records         int
	Applied         int
	Duplicates      int
	Truncated       []Reference
	HighestSequence int64
}

// MinorCompactionBoundary is the WAL side of an atomic memtable snapshot.
// Every listed reference contains only operations at or below Sequence.
type MinorCompactionBoundary struct {
	Sequence   int64
	References []Reference
}

// SnapshotCallback swaps/freezes the corresponding memtable while the WAL
// authority excludes Commit. It must not call back into Tablet.
type SnapshotCallback func(MinorCompactionBoundary) error

// Tablet implements ingestrouter.HostedTablet.
type Tablet struct {
	mu       sync.Mutex
	cfg      Config
	current  *wal
	nextSeq  int64
	ops      map[string]operation
	applied  map[string]struct{}
	closed   bool
	recovery RecoveryReport
}

// Open verifies the owner, replays every authoritative metadata reference,
// repairs cleanly truncated tails, and only then permits new ingest.
func Open(ctx context.Context, cfg Config) (*Tablet, RecoveryReport, error) {
	if err := ctx.Err(); err != nil {
		return nil, RecoveryReport{}, err
	}
	if err := validateConfig(cfg); err != nil {
		return nil, RecoveryReport{}, err
	}
	if cfg.ReconcileTimeout <= 0 {
		cfg.ReconcileTimeout = 5 * time.Second
	}
	if cfg.NewID == nil {
		cfg.NewID = func() string { return uuid.NewString() }
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	t := &Tablet{
		cfg: cfg, nextSeq: 1,
		ops:     make(map[string]operation),
		applied: make(map[string]struct{}),
	}
	recoveryVerifier := cfg.RecoveryVerifier
	if recoveryVerifier == nil {
		recoveryVerifier = cfg.Verifier
	}
	if err := verifyFence(ctx, recoveryVerifier, cfg.Fence); err != nil {
		return nil, RecoveryReport{}, err
	}
	report, err := t.recover(ctx)
	if err != nil {
		return nil, report, err
	}
	if err := verifyFence(ctx, recoveryVerifier, cfg.Fence); err != nil {
		return nil, report, err
	}
	t.recovery = report
	return t, report, nil
}

func validateConfig(cfg Config) error {
	switch {
	case cfg.Root == "":
		return fmt.Errorf("%w: empty WAL root", ErrInvalidConfig)
	case cfg.Metadata == nil:
		return fmt.Errorf("%w: nil metadata authority", ErrInvalidConfig)
	case cfg.Store == nil:
		return fmt.Errorf("%w: nil store", ErrInvalidConfig)
	case cfg.Verifier == nil:
		return fmt.Errorf("%w: nil fence verifier", ErrInvalidConfig)
	case cfg.Sink == nil:
		return fmt.Errorf("%w: nil mutation sink", ErrInvalidConfig)
	case cfg.Extent.Validate() != nil:
		return fmt.Errorf("%w: invalid extent", ErrInvalidConfig)
	case !cfg.Fence.Valid():
		return fmt.Errorf("%w: invalid fence", ErrInvalidConfig)
	}
	host, port, err := net.SplitHostPort(cfg.ServerAddress)
	if err != nil || host == "" || port == "" ||
		host == "." || host == ".." || strings.ContainsAny(host, `/\`+"\x00") {
		return fmt.Errorf("%w: server address must be host:port", ErrInvalidConfig)
	}
	portNumber, err := strconv.ParseUint(port, 10, 16)
	if err != nil || portNumber == 0 {
		return fmt.Errorf("%w: server address has invalid port", ErrInvalidConfig)
	}
	return nil
}

func (t *Tablet) Extent() ingestrouter.Extent { return cloneExtent(t.cfg.Extent) }
func (t *Tablet) Fence() ingestrouter.Fence   { return t.cfg.Fence }
func (t *Tablet) Authority() ingestrouter.CommitAuthority {
	return ingestrouter.AuthorityAccumuloWAL
}

// Commit follows the safety order: verify fence, install the metadata
// reference, durably append, verify the fence again, apply to memory, ack.
func (t *Tablet) Commit(ctx context.Context, req ingestrouter.CommitRequest) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.closed {
		return ErrClosed
	}
	if !req.Extent.Equal(t.cfg.Extent) || req.Fence != t.cfg.Fence {
		return ErrStaleOwner
	}
	if req.OperationID == "" || req.SessionID == "" || req.RequestID == "" ||
		len(req.Mutations) == 0 {
		return ErrInvalidCommit
	}
	if err := t.verify(ctx); err != nil {
		return err
	}

	op := operation{
		OperationID: req.OperationID,
		SessionID:   req.SessionID,
		RequestID:   req.RequestID,
		Extent:      cloneExtent(req.Extent),
		Fence:       req.Fence,
		Mutations:   cloneMutations(req.Mutations),
	}
	op.Digest = operationDigest(op)
	if prior, ok := t.ops[op.OperationID]; ok {
		if prior.Digest != op.Digest {
			return ErrIdempotencyConflict
		}
		if _, ok := t.applied[op.OperationID]; ok {
			return nil
		}
		return t.apply(ctx, prior)
	}
	if t.current == nil {
		if err := t.createWAL(ctx); err != nil {
			return err
		}
	}
	if err := t.ensureReference(ctx, t.current.ref); err != nil {
		return err
	}
	op.Sequence = t.nextSeq
	frame, err := encodeFrame(op)
	if err != nil {
		return err
	}
	if err := t.cfg.Store.Append(ctx, t.current.ref.Path, frame); err != nil {
		reconcileCtx, cancel := t.reconcileContext()
		found, reconcileErr := t.findOperation(reconcileCtx, t.current.ref, op.OperationID)
		cancel()
		if reconcileErr != nil {
			return errors.Join(ErrAmbiguous, err, reconcileErr)
		}
		if found == nil {
			return err
		}
		if found.Digest != op.Digest {
			return ErrIdempotencyConflict
		}
		op = *found
	}
	t.ops[op.OperationID] = op
	if op.Sequence >= t.nextSeq {
		t.nextSeq = op.Sequence + 1
	}
	reconcileCtx, cancel := t.reconcileContext()
	err = t.verify(reconcileCtx)
	cancel()
	if err != nil {
		return err
	}
	return t.apply(ctx, op)
}

func (t *Tablet) apply(ctx context.Context, op operation) error {
	if err := t.cfg.Sink.Apply(ctx, op.OperationID, op.Sequence, cloneMutations(op.Mutations)); err != nil {
		if ctx.Err() != nil {
			return errors.Join(ErrAmbiguous, ctx.Err())
		}
		return err
	}
	t.applied[op.OperationID] = struct{}{}
	return nil
}

func (t *Tablet) createWAL(ctx context.Context) error {
	id := t.cfg.NewID()
	if _, err := uuid.Parse(id); err != nil {
		return fmt.Errorf("%w: WAL id is not a UUID: %q", ErrInvalidConfig, id)
	}
	hostPath := strings.ReplaceAll(t.cfg.ServerAddress, ":", "+")
	walPath := joinPath(t.cfg.Root, hostPath, id)
	ref := Reference{ID: id, Path: walPath, Qualifier: "-/" + walPath}
	h := header{
		Version: formatVersion,
		WALID:   id,
		Path:    walPath,
		Extent:  cloneExtent(t.cfg.Extent),
		Owner:   t.cfg.Fence,
		Created: t.cfg.Now().UnixNano(),
	}
	frame, err := encodeFrame(h)
	if err != nil {
		return err
	}
	if err := t.cfg.Store.Create(ctx, walPath, frame); err != nil {
		return fmt.Errorf("walauthority: create %s: %w", walPath, err)
	}
	t.current = &wal{ref: ref}
	return nil
}

func (t *Tablet) ensureReference(ctx context.Context, ref Reference) error {
	err := t.cfg.Metadata.EnsureReference(ctx, t.cfg.Extent, t.cfg.Fence, ref)
	if err == nil {
		return nil
	}
	reconcileCtx, cancel := t.reconcileContext()
	ok, checkErr := t.cfg.Metadata.HasReference(reconcileCtx, t.cfg.Extent, t.cfg.Fence, ref)
	cancel()
	if checkErr != nil {
		return errors.Join(ErrAmbiguous, err, checkErr)
	}
	if !ok {
		return err
	}
	return nil
}

// Roll closes the current WAL and causes the next commit to create and
// reference a unique successor. The old reference remains authoritative until
// Retire is called after the future minor-compaction metadata commit.
func (t *Tablet) Roll(ctx context.Context) (Reference, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.closed {
		return Reference{}, ErrClosed
	}
	if err := t.verify(ctx); err != nil {
		return Reference{}, err
	}
	if t.current == nil {
		return Reference{}, nil
	}
	ref := t.current.ref
	if err := t.cfg.Store.Sync(ctx, ref.Path); err != nil {
		return Reference{}, err
	}
	t.current = nil
	return ref, nil
}

// SealForMinorCompaction syncs the active WAL, captures every authoritative
// reference and highest applied sequence, invokes snapshot while Commit is
// excluded, then rolls to a new WAL generation. This is the primitive a
// mincauthority.Snapshotter adapter uses to make its memtable/WAL boundary
// atomic. A failed callback leaves the active WAL in place.
func (t *Tablet) SealForMinorCompaction(ctx context.Context, snapshot SnapshotCallback) (MinorCompactionBoundary, error) {
	if snapshot == nil {
		return MinorCompactionBoundary{}, fmt.Errorf("%w: nil snapshot callback", ErrInvalidConfig)
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.closed {
		return MinorCompactionBoundary{}, ErrClosed
	}
	if err := t.verify(ctx); err != nil {
		return MinorCompactionBoundary{}, err
	}
	if t.current != nil {
		if err := t.cfg.Store.Sync(ctx, t.current.ref.Path); err != nil {
			return MinorCompactionBoundary{}, err
		}
	}
	refs, err := t.cfg.Metadata.References(ctx, t.cfg.Extent)
	if err != nil {
		return MinorCompactionBoundary{}, err
	}
	if err := t.verify(ctx); err != nil {
		return MinorCompactionBoundary{}, err
	}
	sort.Slice(refs, func(i, j int) bool { return refs[i].Qualifier < refs[j].Qualifier })
	boundary := MinorCompactionBoundary{
		Sequence:   t.nextSeq - 1,
		References: append([]Reference(nil), refs...),
	}
	if err := snapshot(boundary); err != nil {
		return MinorCompactionBoundary{}, err
	}
	t.current = nil
	return boundary, nil
}

// Retire removes an exact WAL reference and then deletes the WAL. Callers
// must invoke it only after their RFile metadata commit succeeds.
func (t *Tablet) Retire(ctx context.Context, ref Reference) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.closed {
		return ErrClosed
	}
	if t.current != nil && t.current.ref == ref {
		return errors.New("walauthority: cannot retire active WAL")
	}
	if err := t.verify(ctx); err != nil {
		return err
	}
	err := t.cfg.Metadata.RemoveReference(ctx, t.cfg.Extent, t.cfg.Fence, ref)
	if err != nil {
		reconcileCtx, cancel := t.reconcileContext()
		present, checkErr := t.cfg.Metadata.HasReference(reconcileCtx, t.cfg.Extent, t.cfg.Fence, ref)
		cancel()
		if checkErr != nil {
			return errors.Join(ErrAmbiguous, err, checkErr)
		}
		if present {
			return err
		}
	}
	return t.cfg.Store.Remove(ctx, ref.Path)
}

func (t *Tablet) Close(ctx context.Context) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.closed {
		return nil
	}
	if t.current != nil {
		if err := t.cfg.Store.Sync(ctx, t.current.ref.Path); err != nil {
			return err
		}
	}
	t.closed = true
	return nil
}

func (t *Tablet) verify(ctx context.Context) error {
	return verifyFence(ctx, t.cfg.Verifier, t.cfg.Fence)
}

func verifyFence(ctx context.Context, verifier FenceVerifier, fence ingestrouter.Fence) error {
	if err := verifier.Verify(ctx, fence); err != nil {
		return errors.Join(ErrStaleOwner, err)
	}
	return nil
}

func (t *Tablet) reconcileContext() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), t.cfg.ReconcileTimeout)
}

func (t *Tablet) recover(ctx context.Context) (RecoveryReport, error) {
	if err := ctx.Err(); err != nil {
		return RecoveryReport{}, err
	}
	refs, err := t.cfg.Metadata.References(ctx, t.cfg.Extent)
	if err != nil {
		return RecoveryReport{}, err
	}
	sort.Slice(refs, func(i, j int) bool { return refs[i].Path < refs[j].Path })
	report := RecoveryReport{References: len(refs)}
	var all []operation
	for _, ref := range refs {
		ops, valid, truncated, err := t.readWAL(ctx, ref)
		if err != nil {
			return report, err
		}
		if truncated {
			if err := t.cfg.Store.Truncate(ctx, ref.Path, valid); err != nil {
				return report, fmt.Errorf("walauthority: repair truncated %s: %w", ref.Path, err)
			}
			report.Truncated = append(report.Truncated, ref)
		}
		all = append(all, ops...)
	}
	sort.SliceStable(all, func(i, j int) bool {
		if all[i].Sequence == all[j].Sequence {
			return all[i].OperationID < all[j].OperationID
		}
		return all[i].Sequence < all[j].Sequence
	})
	sequences := make(map[int64]string, len(all))
	for _, op := range all {
		report.Records++
		if priorID, ok := sequences[op.Sequence]; ok && priorID != op.OperationID {
			return report, fmt.Errorf("%w: sequence %d belongs to both %s and %s",
				ErrCorrupt, op.Sequence, priorID, op.OperationID)
		}
		sequences[op.Sequence] = op.OperationID
		if prior, ok := t.ops[op.OperationID]; ok {
			if prior.Digest != op.Digest {
				return report, fmt.Errorf("%w: conflicting recovered operation %s", ErrCorrupt, op.OperationID)
			}
			report.Duplicates++
			continue
		}
		if err := t.apply(ctx, op); err != nil {
			return report, fmt.Errorf("walauthority: replay %s: %w", op.OperationID, err)
		}
		t.ops[op.OperationID] = op
		report.Applied++
		if op.Sequence > report.HighestSequence {
			report.HighestSequence = op.Sequence
		}
	}
	t.nextSeq = report.HighestSequence + 1
	if t.nextSeq < 1 {
		t.nextSeq = 1
	}
	return report, nil
}

func (t *Tablet) readWAL(ctx context.Context, ref Reference) ([]operation, int64, bool, error) {
	data, err := t.cfg.Store.Read(ctx, ref.Path)
	if err != nil {
		return nil, 0, false, err
	}
	frames, valid, truncated, err := decodeFrames(data)
	if err != nil {
		return nil, valid, false, fmt.Errorf("%w: %s: %v", ErrCorrupt, ref.Path, err)
	}
	if len(frames) == 0 {
		return nil, valid, truncated, fmt.Errorf("%w: %s has no header", ErrCorrupt, ref.Path)
	}
	var h header
	if err := json.Unmarshal(frames[0], &h); err != nil {
		return nil, valid, truncated, fmt.Errorf("%w: decode header: %v", ErrCorrupt, err)
	}
	if h.Version != formatVersion || h.WALID != ref.ID || h.Path != ref.Path || !h.Extent.Equal(t.cfg.Extent) {
		return nil, valid, truncated, fmt.Errorf("%w: mismatched header for %s", ErrCorrupt, ref.Path)
	}
	ops := make([]operation, 0, len(frames)-1)
	var previousSequence int64
	for i, frame := range frames[1:] {
		var op operation
		if err := json.Unmarshal(frame, &op); err != nil {
			return nil, valid, truncated, fmt.Errorf("%w: record %d: %v", ErrCorrupt, i, err)
		}
		if op.OperationID == "" || op.SessionID == "" || op.RequestID == "" ||
			op.Sequence < 1 || op.Sequence <= previousSequence || !op.Extent.Equal(t.cfg.Extent) ||
			op.Digest != operationDigest(op) {
			return nil, valid, truncated, fmt.Errorf("%w: invalid record %d", ErrCorrupt, i)
		}
		ops = append(ops, op)
		previousSequence = op.Sequence
	}
	return ops, valid, truncated, nil
}

func (t *Tablet) findOperation(ctx context.Context, ref Reference, id string) (*operation, error) {
	ops, valid, truncated, err := t.readWAL(ctx, ref)
	if err != nil {
		return nil, err
	}
	if truncated {
		if err := t.cfg.Store.Truncate(ctx, ref.Path, valid); err != nil {
			return nil, fmt.Errorf("repair partial append: %w", err)
		}
	}
	for i := range ops {
		if ops[i].OperationID == id {
			return &ops[i], nil
		}
	}
	return nil, nil
}

func joinPath(root string, elements ...string) string {
	if strings.Contains(root, "://") {
		return strings.TrimRight(root, "/") + "/" + path.Join(elements...)
	}
	all := append([]string{root}, elements...)
	return filepath.ToSlash(filepath.Join(all...))
}

func operationDigest(op operation) string {
	copy := op
	copy.Sequence = 0
	copy.Digest = ""
	data, _ := json.Marshal(copy)
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func cloneExtent(e ingestrouter.Extent) ingestrouter.Extent {
	return ingestrouter.Extent{
		TableID:    e.TableID,
		PrevEndRow: append([]byte(nil), e.PrevEndRow...),
		EndRow:     append([]byte(nil), e.EndRow...),
	}
}

func cloneMutations(in []ingestrouter.Mutation) []ingestrouter.Mutation {
	data, _ := json.Marshal(in)
	var out []ingestrouter.Mutation
	_ = json.Unmarshal(data, &out)
	return out
}

func encodeFrame(value any) ([]byte, error) {
	payload, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	if len(payload) > maxFrameSize {
		return nil, fmt.Errorf("walauthority: frame exceeds %d bytes", maxFrameSize)
	}
	return frame(payload), nil
}

func frame(payload []byte) []byte {
	out := make([]byte, 8+len(payload))
	putUint32(out[0:4], uint32(len(payload)))
	putUint32(out[4:8], crc32c(payload))
	copy(out[8:], payload)
	return out
}

func decodeFrames(data []byte) (frames [][]byte, valid int64, truncated bool, err error) {
	reader := bytes.NewReader(data)
	for reader.Len() > 0 {
		start := int64(len(data) - reader.Len())
		var prefix [8]byte
		if _, err := io.ReadFull(reader, prefix[:]); err != nil {
			return frames, start, true, nil
		}
		size := int(readUint32(prefix[0:4]))
		if size < 0 || size > maxFrameSize {
			return nil, start, false, errors.New("invalid frame size")
		}
		payload := make([]byte, size)
		if _, err := io.ReadFull(reader, payload); err != nil {
			return frames, start, true, nil
		}
		if crc32c(payload) != readUint32(prefix[4:8]) {
			return nil, start, false, errors.New("checksum mismatch")
		}
		frames = append(frames, payload)
		valid = int64(len(data) - reader.Len())
	}
	return frames, valid, false, nil
}
