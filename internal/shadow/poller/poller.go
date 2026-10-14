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

// Package poller watches an Accumulo table's metadata and emits a
// CompactionEvent every time a tablet's file: set changes.
//
// Detection model: every tick, walk the table's tablets via the shared
// metadata.Walker and snapshot each tablet's `file:` qualifier set.
// Diff the snapshot against the previous tick — any tablet whose file
// set shrank (one or more files removed and zero-or-more added) is
// treated as having completed a compaction. The removed files were
// inputs; the added files were the output.
//
// The "removed and added" shape matches Accumulo's commit semantics:
// the manager-side commit deletes input file: entries and inserts the
// output file: entry in one batch (see Ample's TabletMetadata commit).
// Pure additions (no removals) are flushes (minc) and are reported
// separately so the oracle can shadow those too.
//
// Races:
//   - Input files get GC'd after commit. Veculo's gc interval default
//     is 60s; with a 5s poll cycle we have ample margin to fetch.
//     If fetch fails with NotFound we record InputsMissing and move
//     on — the operator's signal that the GC window is too tight.
//   - Tablet split / merge changes the keyspace: a tablet disappears
//     from one extent and reappears under another. The diff sees this
//     as "files removed, but no replacement output" — we skip those
//     events explicitly because they're not compactions.
//   - First-tick bootstrap: we DON'T emit events for the initial
//     snapshot — only post-bootstrap changes are events.
package poller

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/phrocker/shoal-oss/internal/metadata"
	"github.com/phrocker/shoal-oss/internal/shadow/metrics"
)

// Event is one observed file-set change for a single tablet. EventKind
// distinguishes compactions (inputs removed AND output added) from
// flushes (additions only).
type Event struct {
	// Kind names the event class.
	Kind EventKind

	// Tablet is a snapshot of the tablet's metadata at observation
	// time (post-change). Use Tablet.Files for the NEW file set;
	// OldFiles/NewFiles below give the delta.
	Tablet metadata.TabletInfo

	// OldFiles are the input file: entries that disappeared on this
	// tick. Paths are absolute (StoredTabletFile URI form).
	OldFiles []metadata.FileEntry

	// NewFiles are the output file: entries that appeared on this
	// tick. For Compaction this is typically one entry; for Flush
	// also typically one.
	NewFiles []metadata.FileEntry

	// ObservedAt is the wall-clock time at which the diff was taken.
	// Used for "is this still inside the GC window" decisions.
	ObservedAt time.Time
}

// EventKind discriminates the observed-change taxonomy.
type EventKind int

const (
	// KindCompaction: at least one file removed AND at least one
	// file added. This is the dominant case — the shadow oracle
	// operates on this kind almost exclusively.
	KindCompaction EventKind = iota
	// KindFlush: zero files removed AND at least one added. Output
	// is the result of a minor compaction (memtable flush).
	KindFlush
)

// String renders an EventKind for log lines.
func (k EventKind) String() string {
	switch k {
	case KindCompaction:
		return "compaction"
	case KindFlush:
		return "flush"
	default:
		return "unknown"
	}
}

// Config is the per-table poller config.
type Config struct {
	// TableID is the Accumulo table ID to watch (e.g. "2k").
	TableID string

	// PollInterval is the cadence at which the metadata walk runs.
	// Default 5s. Below 1s is rejected to avoid metadata-table load.
	PollInterval time.Duration

	// Logger is the slog logger used for per-event diagnostics. Nil
	// uses slog.Default().
	Logger *slog.Logger

	// Metrics is the shared registry. Nil disables metric updates
	// (useful for tests).
	Metrics *metrics.Registry
}

// Poller is one goroutine watching one table.
type Poller struct {
	cfg    Config
	walker *metadata.Walker
	out    chan<- Event

	// prev is the most recent per-tablet file: snapshot. Tablet
	// identity = (TableID, EndRow, PrevRow) — we don't rely on the
	// tserver's session because a hosted-tablet move doesn't change
	// the file set.
	prev map[tabletKey]map[string]metadata.FileEntry // path → entry

	mu       sync.Mutex
	stopOnce sync.Once
	stopped  chan struct{}
}

// tabletKey uniquely identifies a tablet within a table independent of
// its current hosting tserver. Two TabletInfos with the same key map to
// the same physical tablet.
type tabletKey struct {
	endRow  string
	prevRow string
}

func keyOf(t metadata.TabletInfo) tabletKey {
	return tabletKey{endRow: string(t.EndRow), prevRow: string(t.PrevRow)}
}

// New constructs a Poller for one table. Caller must Run it in a
// goroutine (or via Manager.Start). out receives one Event per detected
// file-set change; buffer it appropriately for the consumer's latency
// budget.
func New(cfg Config, walker *metadata.Walker, out chan<- Event) (*Poller, error) {
	if cfg.TableID == "" {
		return nil, fmt.Errorf("poller: TableID required")
	}
	if cfg.PollInterval == 0 {
		cfg.PollInterval = 5 * time.Second
	}
	if cfg.PollInterval < time.Second {
		return nil, fmt.Errorf("poller: PollInterval %s below 1s floor", cfg.PollInterval)
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	return &Poller{
		cfg:     cfg,
		walker:  walker,
		out:     out,
		prev:    map[tabletKey]map[string]metadata.FileEntry{},
		stopped: make(chan struct{}),
	}, nil
}

// Run drives the poller loop until ctx is cancelled or Stop is called.
// Blocks; meant for one goroutine per Poller. Returns nil on graceful
// shutdown; returns ctx.Err() if cancelled.
//
// First tick captures the initial snapshot without emitting events;
// subsequent ticks diff against the prior snapshot.
func (p *Poller) Run(ctx context.Context) error {
	logger := p.cfg.Logger.With(slog.String("table", p.cfg.TableID))
	logger.Info("poller starting", slog.Duration("interval", p.cfg.PollInterval))

	// Bootstrap the initial snapshot — no events emitted. Bounded
	// timeout so a stuck Thrift dial / ZK round-trip doesn't pin the
	// goroutine before the ticker can ever fire.
	if err := p.snapshotWithTimeout(ctx, true); err != nil {
		logger.Warn("poller: initial snapshot failed; will retry on next tick",
			slog.String("err", err.Error()))
		if p.cfg.Metrics != nil {
			p.cfg.Metrics.PollerError()
		}
	}

	t := time.NewTicker(p.cfg.PollInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			close(p.stopped)
			logger.Info("poller stopped", slog.String("reason", ctx.Err().Error()))
			return ctx.Err()
		case <-t.C:
			if err := p.snapshotWithTimeout(ctx, false); err != nil {
				logger.Warn("poller: tick failed",
					slog.String("err", err.Error()))
				if p.cfg.Metrics != nil {
					p.cfg.Metrics.PollerError()
				}
				continue
			}
			if p.cfg.Metrics != nil {
				p.cfg.Metrics.PollerLoopOK()
			}
		}
	}
}

// snapshotWithTimeout wraps snapshot with a deadline derived from the
// poller's PollInterval (2× — long enough for the metadata walk under
// normal load, short enough that a hung tserver doesn't silence the
// poller indefinitely). On timeout, returns a wrapped context.DeadlineExceeded.
func (p *Poller) snapshotWithTimeout(parent context.Context, bootstrap bool) error {
	deadline := 2 * p.cfg.PollInterval
	if deadline < 10*time.Second {
		deadline = 10 * time.Second
	}
	ctx, cancel := context.WithTimeout(parent, deadline)
	defer cancel()
	return p.snapshot(ctx, bootstrap)
}

// Stop signals the poller to exit. Returns after the run loop has
// fully exited (next select observes ctx.Done()). Idempotent.
func (p *Poller) Stop() {
	p.stopOnce.Do(func() {
		// The owning caller cancels ctx; Stop just blocks until the
		// goroutine has visibly exited. If the goroutine never started
		// (e.g. Run was never called), stopped will never close —
		// callers must arrange ctx cancellation as well.
		<-p.stopped
	})
}

// snapshot does one metadata walk + diff. If bootstrap=true, the
// observed state replaces p.prev without emitting events.
func (p *Poller) snapshot(ctx context.Context, bootstrap bool) error {
	tabletMap, err := p.walker.BootstrapAll(ctx)
	if err != nil {
		return fmt.Errorf("walker.BootstrapAll: %w", err)
	}
	tablets := tabletMap[p.cfg.TableID]
	if tablets == nil {
		// Table not visible (yet): treat as zero-tablet state.
		tablets = []metadata.TabletInfo{}
	}

	now := time.Now()
	newState := make(map[tabletKey]map[string]metadata.FileEntry, len(tablets))
	for _, t := range tablets {
		fileSet := make(map[string]metadata.FileEntry, len(t.Files))
		for _, f := range t.Files {
			fileSet[f.Path] = f
		}
		newState[keyOf(t)] = fileSet
	}

	p.mu.Lock()
	prev := p.prev
	p.mu.Unlock()

	if bootstrap {
		p.mu.Lock()
		p.prev = newState
		p.mu.Unlock()
		return nil
	}

	// Build a parallel by-key view of the current tablets so events
	// can carry full TabletInfos (Files + Logs) when emitted.
	tabletByKey := make(map[tabletKey]metadata.TabletInfo, len(tablets))
	for _, t := range tablets {
		tabletByKey[keyOf(t)] = t
	}

	for k, newFiles := range newState {
		oldFiles, hadPrev := prev[k]
		if !hadPrev {
			// New tablet — could be a split-spawned half. Skip; the
			// next tick will see its file set steady.
			continue
		}
		removed, added := diffFileSets(oldFiles, newFiles)
		if len(removed) == 0 && len(added) == 0 {
			continue
		}
		kind := classify(removed, added)
		if kind == -1 {
			// Files removed but nothing added — likely a split/merge
			// shedding files to a sibling. Not a compaction.
			continue
		}
		ev := Event{
			Kind:       kind,
			Tablet:     tabletByKey[k],
			OldFiles:   removed,
			NewFiles:   added,
			ObservedAt: now,
		}
		if p.cfg.Metrics != nil {
			tm := p.cfg.Metrics.For(p.cfg.TableID)
			tm.CompactionsObserved.Add(1)
			tm.LastObservedUnix.Store(now.Unix())
		}
		p.cfg.Logger.Info("compaction observed",
			slog.String("table", p.cfg.TableID),
			slog.String("kind", kind.String()),
			slog.String("end_row", metadata.PrintableBytes(ev.Tablet.EndRow)),
			slog.Int("inputs", len(removed)),
			slog.Int("outputs", len(added)),
		)
		// Non-blocking send: if the consumer can't keep up, dropping
		// events is preferable to blocking the metadata-walk loop
		// (which would amplify metadata-table load on overload).
		select {
		case p.out <- ev:
		case <-ctx.Done():
			return ctx.Err()
		default:
			p.cfg.Logger.Warn("event channel full; dropping event",
				slog.String("table", p.cfg.TableID),
				slog.String("kind", kind.String()),
			)
		}
	}

	p.mu.Lock()
	p.prev = newState
	p.mu.Unlock()
	return nil
}

// diffFileSets returns (removed, added). Pure.
func diffFileSets(old, new map[string]metadata.FileEntry) (removed, added []metadata.FileEntry) {
	for p, e := range old {
		if _, ok := new[p]; !ok {
			removed = append(removed, e)
		}
	}
	for p, e := range new {
		if _, ok := old[p]; !ok {
			added = append(added, e)
		}
	}
	return removed, added
}

// classify maps (removed, added) → EventKind. Returns -1 for shapes
// that aren't compactions or flushes (a tablet shedding files to a
// split sibling, etc.).
func classify(removed, added []metadata.FileEntry) EventKind {
	switch {
	case len(removed) > 0 && len(added) > 0:
		return KindCompaction
	case len(removed) == 0 && len(added) > 0:
		return KindFlush
	default:
		return -1
	}
}
