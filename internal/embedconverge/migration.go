// Licensed to the Apache Software Foundation (ASF) under one
// or more contributor license agreements. See the NOTICE file distributed
// with this work for additional information regarding copyright ownership.
// The ASF licenses this file to you under the Apache License, Version 2.0.
package embedconverge

import (
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/phrocker/shoal-oss/internal/embeddingspace"
)

// ErrUnknownFile reports an outcome for a file that is not in the epoch.
// Files created after the snapshot are deliberately not this migration's
// responsibility, so reporting one is a caller bug rather than something
// to absorb.
var ErrUnknownFile = errors.New("embedconverge: file is not in this epoch")

// ErrAbandoned reports work attempted on an abandoned migration.
var ErrAbandoned = errors.New("embedconverge: migration abandoned")

// Migration drives one Epoch toward its target space.
//
// It is safe for concurrent use: a forced migration runs several
// compactions at once, and every one of them leases and settles files
// through this type.
//
// The lifecycle it enforces is the whole point of the epoch:
//
//   - monotonic — a file only ever moves to the target space, never
//     between two non-target spaces and never out of the target;
//   - idempotent — a file already in the target space is never leased,
//     so re-running a completed migration does no provider work at all;
//   - interruptible — Abandon stops leasing immediately, and files
//     already converged stay converged;
//   - resumable — the epoch encodes to durable state and Resume rebuilds
//     the migration without redoing converged work;
//   - abandonable — an abandoned migration leaves a legitimately mixed
//     corpus, not a half-labelled one, because every file is in exactly
//     one recorded space at every instant.
type Migration struct {
	mu         sync.Mutex
	epoch      Epoch
	index      map[string]int
	leased     map[string]*Permit
	governor   *Governor
	now        func() time.Time
	lastChange time.Time
}

// NewMigration starts a migration over epoch. governor may be nil, in
// which case leasing is unthrottled — appropriate for a local embedder
// and for tests, never for a hosted provider.
func NewMigration(epoch Epoch, governor *Governor, now func() time.Time) (*Migration, error) {
	if err := epoch.Validate(); err != nil {
		return nil, err
	}
	if now == nil {
		now = time.Now
	}
	m := &Migration{
		epoch:      epoch,
		index:      make(map[string]int, len(epoch.Files)),
		leased:     make(map[string]*Permit),
		governor:   governor,
		now:        now,
		lastChange: now(),
	}
	for i, file := range epoch.Files {
		m.index[file.Ref.Key()] = i
	}
	return m, nil
}

// EpochID reports the snapshot this migration is driving. It is what a
// compaction must be stamped with for a Converger bound to this epoch to
// admit it.
func (m *Migration) EpochID() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.epoch.ID
}

// Target reports the embedding space this migration converges toward.
func (m *Migration) Target() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.epoch.Target
}

// Resume rebuilds a migration from a persisted epoch.
//
// Every deferred file returns to pending and every lease is dropped,
// because a lease is in-memory state about a worker that no longer
// exists. Converged and skipped files keep their status, which is what
// makes resuming cost nothing for the part of the corpus that already
// moved.
func Resume(epoch Epoch, governor *Governor, now func() time.Time) (*Migration, error) {
	if err := epoch.Validate(); err != nil {
		return nil, err
	}
	for i := range epoch.Files {
		if epoch.Files[i].Status == StatusDeferred {
			epoch.Files[i].Status = StatusPending
		}
	}
	return NewMigration(epoch, governor, now)
}

// Lease reserves up to n pending files for convergence.
//
// Each file costs one Governor admission, so the rate limit, the budget
// and the kill switch all apply here rather than only inside the
// compaction. A refused admission simply returns fewer files: a
// throttled migration makes slower progress, it does not fail.
func (m *Migration) Lease(n int) []FileRef {
	if n <= 0 {
		return nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.epoch.Abandoned {
		return nil
	}
	out := make([]FileRef, 0, n)
	for i := range m.epoch.Files {
		if len(out) == n {
			break
		}
		file := &m.epoch.Files[i]
		if file.Status != StatusPending {
			continue
		}
		key := file.Ref.Key()
		if _, held := m.leased[key]; held {
			continue
		}
		var permit *Permit
		if m.governor != nil {
			admitted, err := m.governor.AdmitFile()
			if err != nil {
				break
			}
			permit = admitted
		}
		m.leased[key] = permit
		out = append(out, file.Ref)
	}
	return out
}

// Complete records the space a file ended up in after a convergence
// attempt.
//
// The recorded state is checked for monotonicity before it is stored, so
// a rewriter that produced a third space — or that dropped a file out of
// the target — is reported as an error rather than persisted. A file
// that did not reach the target is deferred, not failed: it is still in
// a valid, queryable space and a later pass can pick it up.
func (m *Migration) Complete(ref FileRef, after embeddingspace.FileState) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	file, err := m.fileLocked(ref)
	if err != nil {
		return err
	}
	key := ref.Key()
	if err := after.Validate(); err != nil {
		// The lease is refunded: nothing was recorded, so nothing was
		// spent. Holding it would leak a permit out of the budget for
		// the life of the migration.
		m.releaseLocked(key, false)
		return err
	}
	converged := embeddingspace.Converged(m.epoch.Target, after)
	m.releaseLocked(key, converged)
	if file.Status == StatusConverged || file.Status == StatusSkipped {
		// Idempotence: a duplicate completion for a file that already
		// reached the target is not an error, and must not re-open it.
		if converged {
			return nil
		}
		return fmt.Errorf("%w: %s regressed from %s to %s",
			embeddingspace.ErrNotMonotonic, ref.Entry, file.Current.String(), after.String())
	}
	if err := embeddingspace.EnsureMonotonic(m.epoch.Target, file.Current, after); err != nil {
		return err
	}
	file.Attempts++
	file.Current = after
	if converged {
		file.Status = StatusConverged
		file.LastError = ""
	} else {
		file.Status = StatusDeferred
		file.LastError = "convergence did not reach the target space"
	}
	m.lastChange = m.now()
	return nil
}

// Fail records an attempt that did not converge a file. The file keeps
// the space it already had — which is what a provider failure must leave
// behind — and becomes eligible again on the next Resume.
func (m *Migration) Fail(ref FileRef, cause error) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	file, err := m.fileLocked(ref)
	if err != nil {
		return err
	}
	m.releaseLocked(ref.Key(), false)
	if file.Status == StatusConverged || file.Status == StatusSkipped {
		return nil
	}
	file.Attempts++
	file.Status = StatusDeferred
	if cause != nil {
		file.LastError = cause.Error()
	}
	m.lastChange = m.now()
	return nil
}

// Abandon stops the migration, leaving the corpus in whatever mixed
// state it reached.
//
// This is safe precisely because every file is in exactly one recorded
// space at every instant: convergence publishes a whole new file with a
// new label, so there is no moment at which a file is half-labelled.
// Abandoning is therefore an operator decision, not a recovery
// procedure.
//
// Abandoning refuses *new* leases but deliberately keeps the ones
// already outstanding. Dropping them would make Progress report no
// in-flight work while workers were still running, and a worker that
// then published a converged replacement would have nowhere to record
// it — the file's real space and the migration's record would diverge,
// which is exactly the silent mislabelling this package exists to
// prevent. Complete and Fail therefore still settle a file that was
// leased before the abandonment; every other file is refused with
// ErrAbandoned. Drained reports when the last of them has settled.
//
// Abandon is not a hard stop. Stopping work already in flight is the
// Governor's kill switch, which is checked per cell; Abandon is the
// bookkeeping half of the same operation, and an operator who wants both
// calls Governor.Stop and then Abandon.
func (m *Migration) Abandon() {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.epoch.Abandoned {
		return
	}
	m.epoch.Abandoned = true
	m.lastChange = m.now()
}

// Drained reports whether abandonment has finished: the migration is
// abandoned and no lease is still outstanding. A migration that was
// never abandoned is not drained.
func (m *Migration) Drained() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.epoch.Abandoned && len(m.leased) == 0
}

// SpaceCount is the number of epoch files currently in one space.
type SpaceCount struct {
	State    embeddingspace.State `json:"state"`
	Identity string               `json:"identity,omitempty"`
	Files    int                  `json:"files"`
	Spans    int64                `json:"spans"`
}

// Progress is what an operator watches.
//
// The signal that matters is not "is the corpus mixed" — during a
// migration it legitimately is, possibly for a long time — but "is
// convergence progressing". Converged against Total, and LastChange
// against now, answer that.
type Progress struct {
	Epoch      string       `json:"epoch"`
	Table      string       `json:"table"`
	Target     string       `json:"target"`
	Mode       Mode         `json:"mode"`
	State      RunState     `json:"state"`
	Abandoned  bool         `json:"abandoned"`
	Draining   bool         `json:"draining"`
	Total      int          `json:"total"`
	Converged  int          `json:"converged"`
	Skipped    int          `json:"skipped"`
	Pending    int          `json:"pending"`
	Deferred   int          `json:"deferred"`
	InFlight   int          `json:"in_flight"`
	Spaces     []SpaceCount `json:"spaces"`
	LastChange time.Time    `json:"last_change"`
}

// Done reports whether every file in the epoch has reached the target.
func (p Progress) Done() bool {
	return p.Total == p.Converged+p.Skipped
}

// Progress snapshots the migration.
func (m *Migration) Progress() Progress {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := Progress{
		Epoch:      m.epoch.ID,
		Table:      m.epoch.Table,
		Target:     m.epoch.Target,
		Mode:       m.epoch.Mode,
		State:      StateRunning,
		Abandoned:  m.epoch.Abandoned,
		Draining:   m.epoch.Abandoned && len(m.leased) > 0,
		Total:      len(m.epoch.Files),
		InFlight:   len(m.leased),
		LastChange: m.lastChange,
	}
	if m.governor != nil {
		out.State = m.governor.State()
	}
	counts := map[string]*SpaceCount{}
	for _, file := range m.epoch.Files {
		switch file.Status {
		case StatusConverged:
			out.Converged++
		case StatusSkipped:
			out.Skipped++
		case StatusDeferred:
			out.Deferred++
		default:
			out.Pending++
		}
		key := string(file.Current.State) + "\x00" + file.Current.Identity
		count := counts[key]
		if count == nil {
			count = &SpaceCount{State: file.Current.State, Identity: file.Current.Identity}
			counts[key] = count
		}
		count.Files++
		count.Spans += file.Spans
	}
	out.Spaces = make([]SpaceCount, 0, len(counts))
	for _, count := range counts {
		out.Spaces = append(out.Spaces, *count)
	}
	sort.Slice(out.Spaces, func(i, j int) bool {
		if out.Spaces[i].State != out.Spaces[j].State {
			return out.Spaces[i].State < out.Spaces[j].State
		}
		return out.Spaces[i].Identity < out.Spaces[j].Identity
	})
	return out
}

// Stalled reports a migration that still has work to do but has not
// changed status for idle.
//
// This is the alert worth having. Mixed state is normal during a
// migration and alerting on it would train operators to ignore the
// alarm; a migration that has stopped moving is the real fault, and an
// abandoned or finished one is not stalled at all.
func (m *Migration) Stalled(idle time.Duration) bool {
	if idle <= 0 {
		return false
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.epoch.Abandoned {
		return false
	}
	remaining := 0
	for _, file := range m.epoch.Files {
		if file.Status == StatusPending || file.Status == StatusDeferred {
			remaining++
		}
	}
	if remaining == 0 {
		return false
	}
	return m.now().Sub(m.lastChange) >= idle
}

// Snapshot returns a deep copy of the epoch, suitable for Encode. The
// copy is what makes persistence safe under concurrency: the caller
// serialises a value nobody else can mutate underneath it.
func (m *Migration) Snapshot() Epoch {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := m.epoch
	out.Files = append([]EpochFile(nil), m.epoch.Files...)
	return out
}

// fileLocked resolves a reference to its epoch file.
//
// After abandonment only a file whose lease is still outstanding
// resolves; everything else is refused. That is what lets an abandoned
// migration settle work already in flight without accepting new work.
func (m *Migration) fileLocked(ref FileRef) (*EpochFile, error) {
	key := ref.Key()
	index, ok := m.index[key]
	if !ok {
		return nil, fmt.Errorf("%w: %s", ErrUnknownFile, ref.Entry)
	}
	if m.epoch.Abandoned {
		if _, held := m.leased[key]; !held {
			return nil, fmt.Errorf("%w: %s", ErrAbandoned, m.epoch.ID)
		}
	}
	return &m.epoch.Files[index], nil
}

// releaseLocked settles a lease's Governor reservation. converged is
// whether the file actually reached the target, which is what decides
// whether the permit is spent or refunded.
func (m *Migration) releaseLocked(key string, converged bool) {
	permit, held := m.leased[key]
	if !held {
		return
	}
	delete(m.leased, key)
	if permit == nil {
		return
	}
	if converged {
		permit.Settle(permit.Files())
	} else {
		permit.Settle(0)
	}
}
