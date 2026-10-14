// Licensed to the Apache Software Foundation (ASF) under one
// or more contributor license agreements. See the NOTICE file distributed
// with this work for additional information regarding copyright ownership.
// The ASF licenses this file to you under the Apache License, Version 2.0.
package embedconverge

import (
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/phrocker/shoal-oss/internal/embeddingspace"
)

// Mode distinguishes the two ways convergence happens.
type Mode string

const (
	// ModeLazy converges files as ordinary compaction happens to touch
	// them. It costs nothing extra and converges the corpus over time.
	ModeLazy Mode = "lazy"

	// ModeForced converges a frozen set of files on demand, the way a
	// forced full compaction drives compaction to completion.
	ModeForced Mode = "forced"
)

// FileStatus is one file's position in the epoch's lifecycle.
type FileStatus string

const (
	// StatusPending means the file has not converged and is eligible for
	// work.
	StatusPending FileStatus = "pending"

	// StatusSkipped means the file was already in the target space when
	// the epoch was taken, so convergence must not touch it. This is
	// what makes repeated migrations free rather than a full re-embed.
	StatusSkipped FileStatus = "skipped"

	// StatusConverged means a rewrite landed the file in the target
	// space.
	StatusConverged FileStatus = "converged"

	// StatusDeferred means an attempt did not converge the file — the
	// provider failed, the budget ran out, an operator paused — and the
	// file is still in its original space. Deferred is a resumable
	// state, not a terminal one.
	StatusDeferred FileStatus = "deferred"
)

// ErrInvalidEpoch reports a snapshot that cannot describe a migration.
var ErrInvalidEpoch = errors.New("embedconverge: invalid epoch")

const epochSchemaVersion = 2

// FileRef identifies one immutable file inside a table.
//
// The metadata file entry is part of the identity rather than just the
// path, because a fenced StoredTabletFile is its path *and* its range:
// two references over one path with different ranges are different
// files, and collapsing them would let a migration report a file
// converged when only one of its two references was.
//
// Extent is bytes, not a string, because a tablet extent is arbitrary
// row bytes and need not be valid UTF-8. Persisting it as a Go string
// would send it through encoding/json, which replaces every invalid byte
// sequence with U+FFFD — so two genuinely distinct extents could decode
// to the same value, collide in Key, and let one file's convergence be
// recorded against another's. A []byte round-trips through JSON as
// base64 and is byte-exact.
type FileRef struct {
	Table  string `json:"table"`
	Extent []byte `json:"extent,omitempty"`
	Entry  string `json:"entry"`
}

// Key is FileRef's comparison key. The parts are length-prefixed rather
// than delimited because a metadata entry is arbitrary text and an
// extent's rows are arbitrary bytes, so no separator is safe.
func (r FileRef) Key() string {
	var b strings.Builder
	writePart := func(n int) {
		b.WriteString(strconv.Itoa(n))
		b.WriteByte(':')
	}
	writePart(len(r.Table))
	b.WriteString(r.Table)
	writePart(len(r.Extent))
	b.Write(r.Extent)
	writePart(len(r.Entry))
	b.WriteString(r.Entry)
	return b.String()
}

// Equal reports whether two references name the same file. FileRef holds
// a byte slice, so it is not comparable with ==.
func (r FileRef) Equal(other FileRef) bool { return r.Key() == other.Key() }

func (r FileRef) valid() bool { return r.Table != "" && r.Entry != "" }

// Observation is one file as the metadata table described it when the
// epoch was taken.
type Observation struct {
	Ref   FileRef
	State embeddingspace.FileState
	// Spans is the metadata entry count used for per-space migration progress.
	Spans int64
}

// EpochFile is one file's entry in a frozen migration set.
type EpochFile struct {
	Ref FileRef `json:"ref"`
	// Observed is the space recorded for this file when the epoch was
	// taken. It is the "before" side of every monotonicity check.
	Observed embeddingspace.FileState `json:"observed"`
	// Current is the space the file is in now: equal to Observed until
	// a rewrite converges it.
	Current  embeddingspace.FileState `json:"current"`
	Status   FileStatus               `json:"status"`
	Attempts int                      `json:"attempts,omitempty"`
	Spans    int64                    `json:"spans,omitempty"`
	// LastError is the most recent failure, retained so an operator can
	// see *why* a migration stalled rather than only that it did.
	LastError string `json:"last_error,omitempty"`
}

// Epoch is the frozen set of files one migration is responsible for.
//
// Freezing matters because a corpus under ingest never stops growing. A
// migration measured against "every file in the table" can run forever
// at 90% complete while being perfectly healthy, and an operator cannot
// tell that from a migration that is genuinely stuck. Files written
// after the epoch was taken are simply not this migration's problem:
// they are written by the current embedder, in the target space,
// already converged by construction.
type Epoch struct {
	Version   int         `json:"version"`
	ID        string      `json:"id"`
	Table     string      `json:"table"`
	Target    string      `json:"target"`
	Mode      Mode        `json:"mode"`
	CreatedAt int64       `json:"created_at_unix_nano"`
	Abandoned bool        `json:"abandoned,omitempty"`
	Files     []EpochFile `json:"files"`
}

// Snapshot freezes observed into an epoch for target.
//
// Files already in the target space are recorded as skipped rather than
// omitted: the denominator an operator watches has to include them, or
// a migration of a mostly-converged table would report a suspiciously
// small total and every progress percentage would be against a
// different base.
func Snapshot(
	id, table, target string, mode Mode, createdAtUnixNano int64, observed []Observation,
) (Epoch, error) {
	if strings.TrimSpace(id) == "" {
		return Epoch{}, fmt.Errorf("%w: epoch id is required", ErrInvalidEpoch)
	}
	if strings.TrimSpace(table) == "" {
		return Epoch{}, fmt.Errorf("%w: table is required", ErrInvalidEpoch)
	}
	normalized, err := embeddingspace.ParseTarget(target)
	if err != nil {
		return Epoch{}, fmt.Errorf("%w: %v", ErrInvalidEpoch, err)
	}
	if normalized == "" {
		return Epoch{}, fmt.Errorf("%w: a migration needs a target embedding space", ErrInvalidEpoch)
	}
	switch mode {
	case ModeLazy, ModeForced:
	default:
		return Epoch{}, fmt.Errorf("%w: unknown mode %q", ErrInvalidEpoch, mode)
	}

	epoch := Epoch{
		Version:   epochSchemaVersion,
		ID:        id,
		Table:     table,
		Target:    normalized,
		Mode:      mode,
		CreatedAt: createdAtUnixNano,
		Files:     make([]EpochFile, 0, len(observed)),
	}
	seen := make(map[string]struct{}, len(observed))
	for _, item := range observed {
		if !item.Ref.valid() {
			return Epoch{}, fmt.Errorf("%w: file reference needs a table and a metadata entry", ErrInvalidEpoch)
		}
		if item.Ref.Table != table {
			return Epoch{}, fmt.Errorf("%w: file %q belongs to table %q, not %q",
				ErrInvalidEpoch, item.Ref.Entry, item.Ref.Table, table)
		}
		if err := item.State.Validate(); err != nil {
			return Epoch{}, fmt.Errorf("%w: %v", ErrInvalidEpoch, err)
		}
		if item.Spans < 0 {
			return Epoch{}, fmt.Errorf(
				"%w: file %q has negative span count %d",
				ErrInvalidEpoch, item.Ref.Entry, item.Spans)
		}
		key := item.Ref.Key()
		if _, dup := seen[key]; dup {
			return Epoch{}, fmt.Errorf("%w: duplicate file reference %q", ErrInvalidEpoch, item.Ref.Entry)
		}
		seen[key] = struct{}{}

		status := StatusPending
		if embeddingspace.Converged(normalized, item.State) {
			status = StatusSkipped
		}
		epoch.Files = append(epoch.Files, EpochFile{
			Ref:      item.Ref,
			Observed: item.State,
			Current:  item.State,
			Status:   status,
			Spans:    item.Spans,
		})
	}
	sort.Slice(epoch.Files, func(i, j int) bool {
		return epoch.Files[i].Ref.Key() < epoch.Files[j].Ref.Key()
	})
	return epoch, nil
}

// Validate checks a decoded epoch, which may have come off disk written
// by another process or another version.
func (e Epoch) Validate() error {
	if e.Version != epochSchemaVersion {
		return fmt.Errorf(
			"%w: unsupported epoch schema version %d",
			ErrInvalidEpoch, e.Version)
	}
	if strings.TrimSpace(e.ID) == "" {
		return fmt.Errorf("%w: epoch id is required", ErrInvalidEpoch)
	}
	if strings.TrimSpace(e.Table) == "" {
		return fmt.Errorf("%w: table is required", ErrInvalidEpoch)
	}
	target, err := embeddingspace.ParseTarget(e.Target)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrInvalidEpoch, err)
	}
	if target == "" {
		return fmt.Errorf("%w: a migration needs a target embedding space", ErrInvalidEpoch)
	}
	switch e.Mode {
	case ModeLazy, ModeForced:
	default:
		return fmt.Errorf("%w: unknown mode %q", ErrInvalidEpoch, e.Mode)
	}
	seen := make(map[string]struct{}, len(e.Files))
	for _, file := range e.Files {
		if !file.Ref.valid() || file.Ref.Table != e.Table {
			return fmt.Errorf("%w: invalid file reference %q", ErrInvalidEpoch, file.Ref.Entry)
		}
		if _, dup := seen[file.Ref.Key()]; dup {
			return fmt.Errorf("%w: duplicate file reference %q", ErrInvalidEpoch, file.Ref.Entry)
		}
		seen[file.Ref.Key()] = struct{}{}
		if err := file.Observed.Validate(); err != nil {
			return fmt.Errorf("%w: %v", ErrInvalidEpoch, err)
		}
		if err := file.Current.Validate(); err != nil {
			return fmt.Errorf("%w: %v", ErrInvalidEpoch, err)
		}
		if file.Spans < 0 {
			return fmt.Errorf(
				"%w: file %q has negative span count %d",
				ErrInvalidEpoch, file.Ref.Entry, file.Spans)
		}
		switch file.Status {
		case StatusPending, StatusSkipped, StatusConverged, StatusDeferred:
		default:
			return fmt.Errorf("%w: unknown file status %q", ErrInvalidEpoch, file.Status)
		}
		// A persisted epoch is not trusted about convergence: the claim
		// is re-derived from the recorded space, so a corrupted or
		// hand-edited file cannot mark an unconverged file done and have
		// the migration skip it forever.
		if file.Status == StatusConverged && !embeddingspace.Converged(target, file.Current) {
			return fmt.Errorf("%w: file %q is marked converged but records %s",
				ErrInvalidEpoch, file.Ref.Entry, file.Current.String())
		}
		if file.Status == StatusSkipped && !embeddingspace.Converged(target, file.Observed) {
			return fmt.Errorf("%w: file %q is marked skipped but was observed as %s",
				ErrInvalidEpoch, file.Ref.Entry, file.Observed.String())
		}
		if err := embeddingspace.EnsureMonotonic(target, file.Observed, file.Current); err != nil {
			return fmt.Errorf("%w: %v", ErrInvalidEpoch, err)
		}
	}
	return nil
}

// Encode renders an epoch for durable storage. Persisting the epoch is
// what makes a migration survive a restart: without it, resuming means
// re-snapshotting, and a re-snapshot is a different fixed set.
func Encode(epoch Epoch) ([]byte, error) {
	if err := epoch.Validate(); err != nil {
		return nil, err
	}
	return json.Marshal(epoch)
}

// Decode parses a persisted epoch and validates it.
func Decode(raw []byte) (Epoch, error) {
	if len(raw) == 0 {
		return Epoch{}, fmt.Errorf("%w: empty encoding", ErrInvalidEpoch)
	}
	var epoch Epoch
	if err := json.Unmarshal(raw, &epoch); err != nil {
		return Epoch{}, fmt.Errorf("%w: %v", ErrInvalidEpoch, err)
	}
	if err := epoch.Validate(); err != nil {
		return Epoch{}, err
	}
	return epoch, nil
}
