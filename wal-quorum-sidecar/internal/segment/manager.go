// Licensed to the Apache Software Foundation (ASF) under one
// or more contributor license agreements.
package segment

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// OwnershipConflictError reports that a segment id is already held by a
// different owner or a different generation, so it must not be adopted.
//
// This is deliberately distinct from the idempotent case: a repeat open by the
// same owner of the same live segment is a no-op, but a claim that disagrees
// about the pod, the WAL path, the role, the incarnation, or the segment's
// lifecycle state is a genuine conflict and stays an error.
type OwnershipConflictError struct {
	ID        string
	Reason    string
	Existing  Owner
	Requested Owner
}

func (e *OwnershipConflictError) Error() string {
	return fmt.Sprintf("segment %s already exists and cannot be adopted: %s "+
		"(existing owner: pod=%q wal_path=%q role=%s epoch=%d; requested: pod=%q wal_path=%q role=%s epoch=%d)",
		e.ID, e.Reason,
		e.Existing.Pod, e.Existing.WALPath, e.Existing.Role, e.Existing.Epoch,
		e.Requested.Pod, e.Requested.WALPath, e.Requested.Role, e.Requested.Epoch)
}

// IsOwnershipConflict reports whether err is an OwnershipConflictError.
func IsOwnershipConflict(err error) bool {
	var oc *OwnershipConflictError
	return errors.As(err, &oc)
}

// Manager creates, tracks, and manages WAL segments.
// It is the central registry that maps segment IDs to Segment instances.
type Manager struct {
	mu       sync.RWMutex
	walDir   string
	segments map[string]*Segment
	epoch    uint64
	logger   *slog.Logger
}

// NewManager creates a Manager that stores segments under walDir.
//
// Each Manager instance stamps the segments it creates with a unique epoch, so
// segments left on disk by a previous sidecar incarnation are recognisable as
// foreign and are never silently appended to.
func NewManager(walDir string, logger *slog.Logger) *Manager {
	return &Manager{
		walDir:   walDir,
		segments: make(map[string]*Segment),
		epoch:    uint64(time.Now().UnixNano()),
		logger:   logger.With("component", "segment-manager"),
	}
}

// Epoch returns this Manager's incarnation stamp.
func (m *Manager) Epoch() uint64 {
	return m.epoch
}

// Create opens a segment with the given ID and WAL path in the originator
// role. See CreateOrAdopt for the full contract; a repeat create by the same
// owner returns the existing segment rather than an error.
func (m *Manager) Create(id, walPath, originatorPod string) (*Segment, error) {
	seg, _, err := m.CreateOrAdopt(id, walPath, originatorPod, RoleOriginator)
	return seg, err
}

// CreateOrAdopt opens a segment with the given ID and WAL path, or hands back
// the existing one if this is a repeat open by the same owner.
//
// The segment file is placed at <walDir>/<segmentID>.wal.
//
// Segment creation is idempotent for the owner that created it: an open that
// repeats an open this incarnation already served (same id, same pod, same WAL
// path, same role, segment still unsealed) returns the existing segment with
// adopted=true and does not truncate, rewind, or re-stat the file. WAL opens
// are retried by the client (VolumeManagerImpl.createSyncable retries
// fs.create on any failure), and a retry that lands after the segment was
// already created must not wedge the WAL.
//
// Everything else is a conflict and returns *OwnershipConflictError:
//   - a different originator pod, WAL path, or role — another writer's segment;
//   - a segment created by an earlier incarnation (different epoch, including
//     segments recovered off disk with epoch 0) — appending this generation's
//     entries onto the previous generation's bytes would corrupt the WAL;
//   - a sealed segment — its checksum is final, so its generation is over.
func (m *Manager) CreateOrAdopt(id, walPath, originatorPod string, role Role) (*Segment, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	requested := Owner{Pod: originatorPod, WALPath: walPath, Role: role, Epoch: m.epoch}

	if existing, exists := m.segments[id]; exists {
		if err := m.checkAdoptable(id, existing, requested); err != nil {
			return nil, false, err
		}
		// Idempotent re-open: same owner, same generation, still live.
		if offset := existing.Offset(); offset > 0 {
			m.logger.Warn("repeat open of a segment that already has data — "+
				"returning the existing handle without truncating",
				"id", id, "offset", offset, "originator", originatorPod, "role", role.String())
		} else {
			m.logger.Info("repeat open of live segment — returning existing handle (idempotent)",
				"id", id, "originator", originatorPod, "role", role.String())
		}
		return existing, true, nil
	}

	filePath := filepath.Join(m.walDir, id+".wal")

	// A file on disk with no in-memory entry belongs to an earlier incarnation
	// of this sidecar (the map does not survive a restart). Opening it in the
	// originator role would append the new generation's entries onto the old
	// generation's bytes, so refuse. A replica's bytes cannot be vouched for
	// either — they are unverifiable against the originator's checksum, and
	// appending to them would leave this replica something other than a strict
	// prefix of the originator's segment, so no replay could ever repair it.
	// Discard it instead: the originator replays the whole segment after a
	// successful prepare.
	if info, statErr := os.Stat(filePath); statErr == nil && info.Size() > 0 {
		if role == RoleOriginator {
			return nil, false, &OwnershipConflictError{
				ID:     id,
				Reason: fmt.Sprintf("a %d-byte segment file from a previous incarnation is already on disk at %s", info.Size(), filePath),
				Existing: Owner{
					Pod:     "",
					WALPath: "",
					Role:    RoleUnknown,
					Epoch:   0,
				},
				Requested: requested,
			}
		}
		m.logger.Warn("discarding a stale replica segment file from a previous incarnation — "+
			"the originator replays this segment from the start",
			"id", id, "path", filePath, "size", info.Size(), "role", role.String())
		if err := os.Remove(filePath); err != nil {
			return nil, false, fmt.Errorf("discard stale replica segment file %s: %w", filePath, err)
		}
	}

	seg, err := NewSegment(id, walPath, originatorPod, filePath)
	if err != nil {
		return nil, false, err
	}
	seg.setOwner(requested)

	m.segments[id] = seg
	m.logger.Info("segment created", "id", id, "path", filePath,
		"originator", originatorPod, "role", role.String(), "epoch", m.epoch)
	return seg, false, nil
}

// checkAdoptable decides whether an existing segment may be handed back to a
// repeat open. Called with m.mu held.
func (m *Manager) checkAdoptable(id string, existing *Segment, requested Owner) error {
	current := existing.Owner()

	conflict := func(reason string) error {
		return &OwnershipConflictError{
			ID:        id,
			Reason:    reason,
			Existing:  current,
			Requested: requested,
		}
	}

	// A sealed segment has a final checksum; reopening it would append past it.
	if existing.IsSealed() {
		return conflict("segment is sealed")
	}
	// Segments this incarnation did not create (including epoch 0 segments
	// recovered from disk) belong to another generation.
	if current.Epoch != requested.Epoch {
		return conflict("segment belongs to a different sidecar incarnation")
	}
	if current.Pod != requested.Pod {
		return conflict("segment is owned by a different originator pod")
	}
	if current.Role != requested.Role {
		return conflict(fmt.Sprintf("segment is held in the %s role", current.Role))
	}
	// An empty WAL path means "unknown" (recovery load), never a match.
	if current.WALPath != requested.WALPath {
		return conflict("same segment id maps to a different WAL path")
	}
	return nil
}

// Get returns the segment with the given ID, or nil if not found.
func (m *Manager) Get(id string) *Segment {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.segments[id]
}

// GetOrLoad returns the segment with the given ID. If not in the in-memory
// map, it checks for the segment file on disk and loads it. This is needed
// for recovery: after a pod restart, replicated segment files exist on disk
// but aren't registered in memory. Returns nil if no file exists either.
func (m *Manager) GetOrLoad(id string) *Segment {
	// Fast path: already in memory.
	m.mu.RLock()
	if seg, ok := m.segments[id]; ok {
		m.mu.RUnlock()
		return seg
	}
	m.mu.RUnlock()

	// Check if the file exists on disk.
	filePath := filepath.Join(m.walDir, id+".wal")
	if _, err := os.Stat(filePath); err != nil {
		return nil
	}

	// Load the segment from the existing file.
	m.mu.Lock()
	defer m.mu.Unlock()

	// Double-check under write lock.
	if seg, ok := m.segments[id]; ok {
		return seg
	}

	seg, err := NewSegment(id, "", "", filePath)
	if err != nil {
		m.logger.Error("failed to load segment from disk", "id", id, "error", err)
		return nil
	}
	// Epoch 0 / RoleUnknown: this segment predates the running incarnation, so
	// CreateOrAdopt will refuse to hand it to a new open.
	seg.setOwner(Owner{Role: RoleUnknown, Epoch: 0})

	m.segments[id] = seg
	m.logger.Info("loaded segment from disk", "id", id, "path", filePath, "size", seg.Offset())
	return seg
}

// Remove removes a segment from the manager's tracking (does NOT delete the file).
// Returns the removed segment or nil if not found.
func (m *Manager) Remove(id string) *Segment {
	m.mu.Lock()
	defer m.mu.Unlock()

	seg, ok := m.segments[id]
	if !ok {
		return nil
	}
	delete(m.segments, id)
	return seg
}

// Delete removes a segment from tracking AND deletes the file on disk.
func (m *Manager) Delete(id string) error {
	seg := m.Remove(id)
	if seg == nil {
		return fmt.Errorf("segment %s not found", id)
	}

	_ = seg.Close()
	filePath := seg.FilePath()
	if filePath == "" {
		return nil
	}
	if err := os.Remove(filePath); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove segment file %s: %w", filePath, err)
	}
	m.logger.Info("segment deleted", "id", id, "path", filePath)
	return nil
}

// OpenCount returns the number of segments currently in the Open state.
func (m *Manager) OpenCount() int {
	m.mu.RLock()
	defer m.mu.RUnlock()
	count := 0
	for _, seg := range m.segments {
		if !seg.IsSealed() {
			count++
		}
	}
	return count
}

// SealedCount returns the number of segments currently in the Sealed state.
func (m *Manager) SealedCount() int {
	m.mu.RLock()
	defer m.mu.RUnlock()
	count := 0
	for _, seg := range m.segments {
		if seg.IsSealed() {
			count++
		}
	}
	return count
}

// AllSegments returns a snapshot of all tracked segments.
func (m *Manager) AllSegments() []*Segment {
	m.mu.RLock()
	defer m.mu.RUnlock()
	result := make([]*Segment, 0, len(m.segments))
	for _, seg := range m.segments {
		result = append(result, seg)
	}
	return result
}

// SealAll seals every open segment. Used during graceful shutdown.
// Errors are accumulated and returned as a combined error.
func (m *Manager) SealAll(ctx context.Context) error {
	m.mu.RLock()
	openSegs := make([]*Segment, 0)
	for _, seg := range m.segments {
		if !seg.IsSealed() {
			openSegs = append(openSegs, seg)
		}
	}
	m.mu.RUnlock()

	var errs []error
	for _, seg := range openSegs {
		select {
		case <-ctx.Done():
			errs = append(errs, ctx.Err())
			return errors.Join(errs...)
		default:
		}

		_, _, err := seg.Seal()
		if err != nil {
			m.logger.Error("failed to seal segment during shutdown", "id", seg.ID(), "error", err)
			errs = append(errs, fmt.Errorf("seal %s: %w", seg.ID(), err))
		} else {
			m.logger.Info("sealed segment during shutdown", "id", seg.ID())
		}
	}
	return errors.Join(errs...)
}

// ListDir scans the WAL directory and returns info about each .wal file found.
// This is used by the peer ListSegments RPC for segments that may not be in the
// in-memory map (e.g. after a restart).
func (m *Manager) ListDir() ([]SegmentFileInfo, error) {
	entries, err := os.ReadDir(m.walDir)
	if err != nil {
		return nil, fmt.Errorf("read WAL directory %s: %w", m.walDir, err)
	}

	var result []SegmentFileInfo
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		name := entry.Name()
		if len(name) < 5 || name[len(name)-4:] != ".wal" {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			continue
		}
		segID := name[:len(name)-4]
		seg := m.Get(segID)
		sealed := false
		if seg != nil {
			sealed = seg.IsSealed()
		}
		result = append(result, SegmentFileInfo{
			ID:     segID,
			Size:   info.Size(),
			Sealed: sealed,
		})
	}
	return result, nil
}

// SegmentFilePath returns the full path for a segment file given its ID.
func (m *Manager) SegmentFilePath(id string) string {
	return filepath.Join(m.walDir, id+".wal")
}

// SegmentFileInfo holds metadata about a segment file on disk.
type SegmentFileInfo struct {
	ID     string
	Size   int64
	Sealed bool
}
