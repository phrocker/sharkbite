// Licensed to the Apache Software Foundation (ASF) under one
// or more contributor license agreements.
package segment

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"hash"
	"io"
	"os"
	"sync"
	"syscall"
)

// State represents the lifecycle state of a WAL segment.
type State int

const (
	StateOpen   State = iota // Accepting writes.
	StateSealed              // No more writes; checksum finalized.
)

// Role describes why this sidecar holds a segment.
type Role int

const (
	// RoleUnknown is used for segments recovered from disk, whose creator is
	// not known to this process.
	RoleUnknown Role = iota
	// RoleOriginator means the co-located TServer writes this segment through
	// the local (Unix socket) service.
	RoleOriginator
	// RoleReplica means a peer sidecar owns the segment and replicates entries
	// into it via PrepareSegment/ReplicateEntries.
	RoleReplica
)

// String renders the role for log messages.
func (r Role) String() string {
	switch r {
	case RoleOriginator:
		return "originator"
	case RoleReplica:
		return "replica"
	default:
		return "unknown"
	}
}

// Owner identifies which generation of which writer a segment belongs to.
//
// Pod/WALPath come from the open request. Epoch is the incarnation of the
// segment Manager (i.e. of this sidecar process) that created the segment;
// segments recovered off disk carry epoch 0, meaning "created by some earlier
// generation we cannot vouch for". Epoch is what separates a retried open from
// a stale claim on a reused id: only a segment created by the running
// incarnation may be handed back to a repeat open.
type Owner struct {
	Pod     string
	WALPath string
	Role    Role
	Epoch   uint64
}

// Segment represents a single WAL segment file on disk.
// It tracks the write offset, running checksum, and sealed state.
// All methods are safe for concurrent use.
type Segment struct {
	mu sync.Mutex

	id            string
	walPath       string
	originatorPod string
	owner         Owner
	preparedPeers []string
	file          *os.File
	offset        int64
	state         State
	hasher        hash.Hash
	finalChecksum []byte
	sequenceHigh  uint64 // highest sequence_num written
}

// NewSegment opens (or creates) a segment file at the given path.
// The file is opened with O_CREATE|O_WRONLY|O_APPEND to ensure
// sequential writes.
func NewSegment(id, walPath, originatorPod, filePath string) (*Segment, error) {
	f, err := os.OpenFile(filePath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o640)
	if err != nil {
		return nil, fmt.Errorf("open segment file %s: %w", filePath, err)
	}

	// If reopening an existing file, seek to the end to get the current offset.
	info, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("stat segment file %s: %w", filePath, err)
	}

	// Reopening a file that already has bytes means the running checksum has
	// to cover them too, otherwise a later Seal would report the digest of the
	// appended tail only and every checksum comparison against it would fail.
	hasher := sha256.New()
	if info.Size() > 0 {
		if err := hashFileContents(filePath, hasher); err != nil {
			_ = f.Close()
			return nil, err
		}
	}

	return &Segment{
		id:            id,
		walPath:       walPath,
		originatorPod: originatorPod,
		file:          f,
		offset:        info.Size(),
		state:         StateOpen,
		hasher:        hasher,
	}, nil
}

// hashFileContents feeds the current contents of filePath into h. It is used
// when a segment file is reopened so the running digest reflects the bytes
// already on disk.
func hashFileContents(filePath string, h hash.Hash) error {
	rf, err := os.Open(filePath)
	if err != nil {
		return fmt.Errorf("open segment file %s for checksum: %w", filePath, err)
	}
	defer rf.Close()

	if _, err := io.Copy(h, rf); err != nil {
		return fmt.Errorf("checksum existing contents of %s: %w", filePath, err)
	}
	return nil
}

// ID returns the segment's unique identifier.
func (s *Segment) ID() string {
	return s.id
}

// WALPath returns the Accumulo WAL namespace path.
func (s *Segment) WALPath() string {
	return s.walPath
}

// OriginatorPod returns the pod that created this segment.
func (s *Segment) OriginatorPod() string {
	return s.originatorPod
}

// Owner returns the ownership stamp (pod, WAL path, role, incarnation epoch)
// recorded when this segment was created.
func (s *Segment) Owner() Owner {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.owner
}

// setOwner records the ownership stamp. Called by the Manager at creation.
func (s *Segment) setOwner(o Owner) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.owner = o
}

// PreparedPeers returns the peer addresses reported to the writer when the
// segment was opened. A repeat open replays this same list so the TServer
// stores a stable peer set in the metadata table.
func (s *Segment) PreparedPeers() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.preparedPeers == nil {
		return nil
	}
	out := make([]string, len(s.preparedPeers))
	copy(out, s.preparedPeers)
	return out
}

// SetPreparedPeers records the peer set reported for this segment.
func (s *Segment) SetPreparedPeers(peers []string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.preparedPeers = append([]string(nil), peers...)
}

// Offset returns the current write offset (total bytes written).
func (s *Segment) Offset() int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.offset
}

// IsSealed returns true if the segment has been sealed.
func (s *Segment) IsSealed() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.state == StateSealed
}

// HighSequence returns the highest sequence number written to this segment.
func (s *Segment) HighSequence() uint64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.sequenceHigh
}

// Write appends data to the segment file and updates the running checksum.
// Returns the byte offset at which the data was written and the new total offset.
// Returns an error if the segment is sealed.
func (s *Segment) Write(data []byte, seqNum uint64) (writeOffset int64, newOffset int64, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.state == StateSealed {
		return 0, 0, errors.New("segment is sealed")
	}

	writeOffset = s.offset
	n, err := s.file.Write(data)
	if err != nil {
		return 0, 0, fmt.Errorf("write to segment %s: %w", s.id, err)
	}
	if n != len(data) {
		return 0, 0, fmt.Errorf("short write to segment %s: wrote %d of %d bytes", s.id, n, len(data))
	}

	// Update running SHA-256.
	s.hasher.Write(data)
	s.offset += int64(n)

	if seqNum > s.sequenceHigh {
		s.sequenceHigh = seqNum
	}

	return writeOffset, s.offset, nil
}

// Fdatasync calls fdatasync(2) on the underlying file descriptor.
// This flushes data to disk without updating file metadata, which is faster
// than a full fsync and sufficient for WAL durability.
func (s *Segment) Fdatasync() error {
	s.mu.Lock()
	fd := int(s.file.Fd())
	s.mu.Unlock()

	if err := syscall.Fdatasync(fd); err != nil {
		return fmt.Errorf("fdatasync segment %s: %w", s.id, err)
	}
	return nil
}

// Seal finalizes the segment: calls fdatasync, computes the final checksum,
// and marks the segment as sealed. No further writes are allowed after sealing.
func (s *Segment) Seal() (checksum []byte, size int64, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.state == StateSealed {
		return s.finalChecksum, s.offset, nil
	}

	// Flush to disk before sealing.
	if err := syscall.Fdatasync(int(s.file.Fd())); err != nil {
		return nil, 0, fmt.Errorf("fdatasync during seal of segment %s: %w", s.id, err)
	}

	s.finalChecksum = s.hasher.Sum(nil)
	s.state = StateSealed

	return s.finalChecksum, s.offset, nil
}

// Checksum returns the current checksum. If the segment is sealed,
// this is the final SHA-256 digest. If open, it is the running digest
// (a snapshot, not finalized).
func (s *Segment) Checksum() []byte {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.finalChecksum != nil {
		return s.finalChecksum
	}
	// Return a snapshot of the running hash (non-destructive copy).
	h := sha256.New()
	// We cannot clone the internal state without the hash.Hash interface,
	// so for an open segment we return nil.
	_ = h
	return nil
}

// FinalChecksum returns the SHA-256 digest of the sealed segment.
// Returns nil if the segment has not been sealed.
func (s *Segment) FinalChecksum() []byte {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.finalChecksum
}

// Close closes the underlying file. Should be called after sealing.
func (s *Segment) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.file == nil {
		return nil
	}
	err := s.file.Close()
	s.file = nil
	return err
}

// FilePath returns the path of the underlying file on disk.
func (s *Segment) FilePath() string {
	if s.file == nil {
		return ""
	}
	return s.file.Name()
}
