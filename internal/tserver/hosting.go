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

package tserver

import (
	"errors"
	"fmt"
	"slices"
	"sort"
	"sync"
)

// Lifecycle errors. Every one of them is a refusal to change hosting state,
// so callers can treat any non-nil error from a transition as "the tablet is
// exactly where it was".
var (
	// ErrInvalidExtent means the extent cannot name a tablet.
	ErrInvalidExtent = errors.New("tserver: invalid extent")
	// ErrNoLock means this process holds no tablet-server ServiceLock, so it
	// has no authority to host anything.
	ErrNoLock = errors.New("tserver: no tablet server lock held")
	// ErrInvalidLock means a lock identity was offered that names nothing.
	ErrInvalidLock = errors.New("tserver: invalid lock")
	// ErrStaleServerLock means the request was stamped with a tablet-server
	// lock generation other than the one currently held.
	ErrStaleServerLock = errors.New("tserver: stale tablet server lock")
	// ErrStaleManagerLock means the request came from a manager that has been
	// superseded by a later one.
	ErrStaleManagerLock = errors.New("tserver: stale manager lock")
	// ErrAlreadyAssigned means the extent is already assigned here — a
	// duplicate assignment.
	ErrAlreadyAssigned = errors.New("tserver: tablet already assigned")
	// ErrOverlapping means the extent shares rows with an assigned tablet,
	// which is how a stale pre-split or post-merge extent shows up.
	ErrOverlapping = errors.New("tserver: tablet overlaps an assigned tablet")
	// ErrNotAssigned means the extent is not tracked by this host.
	ErrNotAssigned = errors.New("tserver: tablet not assigned")
	// ErrStaleAttempt means the completion belongs to an assignment attempt
	// that has already ended, so the tablet it names is a later attempt at
	// the same extent.
	ErrStaleAttempt = errors.New("tserver: stale assignment attempt")
	// ErrWrongState means the tablet is tracked but not in the state the
	// transition requires.
	ErrWrongState = errors.New("tserver: tablet in wrong hosting state")
	// ErrInvalidUnloadMode means the unload mode is not one this host knows,
	// so it cannot tell whether the manager asked for a drain or a forced
	// release.
	ErrInvalidUnloadMode = errors.New("tserver: invalid unload mode")
	// ErrLockNotNewer means a lock was offered that is not a later generation
	// than the newest one this host has already seen.
	ErrLockNotNewer = errors.New("tserver: lock is not a later generation")
	// ErrTabletsAssigned means a lock change was attempted while tablets are
	// still assigned.
	ErrTabletsAssigned = errors.New("tserver: tablets still assigned")
)

// HostingState is where a tablet sits in this host's lifecycle.
type HostingState int

const (
	// StateUnassigned means the host is not tracking the tablet at all. It is
	// the zero value, so an unknown extent reports as unassigned.
	StateUnassigned HostingState = iota
	// StateLoading means the manager assigned the tablet and the host is
	// bringing it online. It is not yet serving.
	StateLoading
	// StateHosted means the tablet is online and serving here.
	StateHosted
	// StateUnloading means the tablet is draining and will be released. It
	// still counts as assigned, so it cannot be re-assigned until released.
	StateUnloading
)

// String renders the state for logs and status output.
func (s HostingState) String() string {
	switch s {
	case StateUnassigned:
		return "UNASSIGNED"
	case StateLoading:
		return "LOADING"
	case StateHosted:
		return "HOSTED"
	case StateUnloading:
		return "UNLOADING"
	default:
		return fmt.Sprintf("HostingState(%d)", int(s))
	}
}

// UnloadMode selects how a manager-directed unassignment is carried out.
type UnloadMode int

const (
	// UnloadGraceful drains the tablet: it moves to StateUnloading and is
	// released once the host reports the drain finished. Used for migrations
	// and rolling replacement, where the manager waits for a clean handoff.
	UnloadGraceful UnloadMode = iota
	// UnloadImmediate releases the tablet at once, whatever it was doing.
	// Used when the manager needs the tablet back now — a forced unload or a
	// dead-server recovery handoff.
	UnloadImmediate
)

// String renders the mode for logs.
func (m UnloadMode) String() string {
	switch m {
	case UnloadGraceful:
		return "GRACEFUL"
	case UnloadImmediate:
		return "IMMEDIATE"
	default:
		return fmt.Sprintf("UnloadMode(%d)", int(m))
	}
}

// valid reports whether the mode is one of the two this host implements.
// Anything else is refused rather than guessed at.
func (m UnloadMode) valid() bool {
	return m == UnloadGraceful || m == UnloadImmediate
}

// TabletStatus is one tracked tablet as the manager and operators see it.
type TabletStatus struct {
	Extent Extent
	State  HostingState
}

// Attempt is an opaque handle to one assignment of one tablet, minted by
// Assign and required by every local completion.
//
// It exists because the ServiceLock generation is not fine-grained enough to
// fence a completion. The manager may unassign a tablet and assign it here
// again without the lock ever changing — a migration that is rolled back, or
// a reassignment after a forced unload. Loads run asynchronously, so a
// completion from the first assignment can arrive after the second has begun,
// and it would otherwise match on extent, lock and state alike: LoadFailed
// would release the new assignment, and LoadComplete would publish it as
// serving before its data was loaded. Carrying the attempt makes each
// completion name the assignment it actually belongs to.
//
// The zero Attempt is invalid and names nothing. A caller cannot construct a
// valid one; the only source is Assign.
type Attempt struct {
	extent Extent
	lock   LockID
	id     uint64
}

// Valid reports whether the attempt names an assignment. The zero value does
// not.
func (a Attempt) Valid() bool { return a.id != 0 }

// Extent returns the tablet this attempt was minted for.
func (a Attempt) Extent() Extent { return a.extent.clone() }

// Assignment returns the process-local assignment generation carried by the
// attempt. It is used when stamping data-plane requests with the same
// assignment identity as the hosting lifecycle.
func (a Attempt) Assignment() uint64 { return a.id }

// Equal reports whether both handles name the same assignment. An Attempt
// carries row bounds, so it is not comparable with ==; use this instead.
func (a Attempt) Equal(other Attempt) bool {
	return a.id == other.id && a.lock.Equal(other.lock) && a.extent.Equal(other.extent)
}

// String renders the attempt for logs.
func (a Attempt) String() string {
	if !a.Valid() {
		return "attempt#<none>"
	}
	return fmt.Sprintf("attempt#%d(%s under %s)", a.id, a.extent, a.lock)
}

// Metrics is a snapshot of the host's operational counters.
//
// Loading, Hosted and Unloading are gauges derived from the tracked tablets
// at snapshot time. The rest are monotonic counters over the life of the
// process.
type Metrics struct {
	Loading   int
	Hosted    int
	Unloading int

	// Assignments counts accepted manager assignments.
	Assignments uint64
	// Loads counts tablets that finished loading and became hosted.
	Loads uint64
	// LoadFailures counts assignments abandoned because the load failed.
	LoadFailures uint64
	// Unloads counts tablets released back to the manager, gracefully or by
	// force.
	Unloads uint64
	// ForcedUnloads counts the subset of Unloads that skipped draining.
	ForcedUnloads uint64
	// RejectedStale counts transitions refused for naming a superseded
	// caller: a fence that did not match the current lock generation, or a
	// completion for an assignment attempt that has already ended.
	RejectedStale uint64
	// RejectedDuplicate counts assignments refused because the tablet, or a
	// tablet overlapping it, was already assigned here.
	RejectedDuplicate uint64
	// LockLosses counts ServiceLock losses.
	LockLosses uint64
	// DroppedOnLockLoss counts tablets dropped because the lock was lost.
	DroppedOnLockLoss uint64
}

// Host tracks the tablets this process hosts on the manager's behalf.
//
// It is the fence, not the decision maker. Assign and Unassign apply what the
// manager asked for; nothing here initiates a hosting change. What Host adds
// is the guarantee that a change is only applied when it can be proven
// current: every manager-directed transition carries the ServiceLock
// generation it was minted under, and anything stale, duplicated, or
// overlapping an already-assigned tablet is refused rather than applied.
//
// Host is safe for concurrent use.
type Host struct {
	mu sync.Mutex

	// held is the ServiceLock currently held; invalid when none is.
	held LockID
	// newest is the newest server lock ever adopted. It outlives a lock loss
	// so a stale lock cannot be re-adopted after the session drops.
	newest LockID
	// manager is the authoritative live manager lock currently known from
	// external observation. It is invalid when live manager authority is
	// temporarily unknown.
	manager LockID
	// newestManager is the highest manager lock ever observed. It outlives a
	// temporary loss of live-manager knowledge so clearing manager cannot make
	// an older epoch look current again.
	newestManager LockID

	tablets map[string]*tabletEntry
	// byTable indexes the tracked tablets of each table in range order, so an
	// overlap check reads two neighbours instead of scanning everything.
	byTable map[string][]*tabletEntry
	// nextAttempt mints attempt ids. It starts at 1 so the zero Attempt is
	// never a real one, and never restarts, so an id identifies an assignment
	// for the life of the process.
	nextAttempt uint64
	metrics     Metrics
}

type tabletEntry struct {
	extent Extent
	state  HostingState
	// attempt is the assignment this entry belongs to. A completion carrying
	// any other id is reporting on an assignment that has already ended.
	attempt uint64
}

// NewHost returns a host that holds no lock and knows no live manager.
// Until AdoptLock records an acquired ServiceLock, every transition fails
// closed with ErrNoLock. Manager-directed transitions additionally require
// ObserveManagerLock to seed the live manager lock; until then they fail
// closed with ErrStaleManagerLock.
func NewHost() *Host {
	return &Host{
		tablets:     make(map[string]*tabletEntry),
		byTable:     make(map[string][]*tabletEntry),
		nextAttempt: 1,
	}
}

// AdoptLock records a newly acquired tablet-server ServiceLock.
//
// The lock must be a later generation than any this host has already used,
// including ones released by LoseLock — a ZooKeeper session that comes back
// always brings a higher sequence, so a lower one is proof the caller is
// replaying a lock it no longer holds. Tablets must all be released first: a
// host that still tracks tablets from an older generation cannot safely
// re-stamp them under a new one.
func (h *Host) AdoptLock(lock LockID) error {
	if !lock.Valid() {
		return fmt.Errorf("%w: %s", ErrInvalidLock, lock)
	}
	h.mu.Lock()
	defer h.mu.Unlock()

	if len(h.tablets) > 0 {
		return fmt.Errorf("%w: %d assigned, release them before adopting %s",
			ErrTabletsAssigned, len(h.tablets), lock)
	}
	if h.newest.Valid() && !lock.Supersedes(h.newest) {
		return fmt.Errorf("%w: offered %s, already saw %s", ErrLockNotNewer, lock, h.newest)
	}
	h.held = lock
	h.newest = lock
	return nil
}

// LoseLock records the loss of the tablet-server ServiceLock named by lost,
// dropping every tablet the host was tracking and returning them so the
// caller can close them.
//
// This is what keeps a tablet from being multiply hosted across a session
// loss. Once the lock is gone the manager is free to give these tablets to
// somebody else, so the host stops claiming them immediately rather than
// waiting to find out; any in-flight transition stamped with the lost lock
// then fails closed.
//
// The notification is fenced to its own generation, exactly like the local
// completions: a loss is applied only when lost is the lock still held. A
// duplicate or delayed notification for a generation that has already been
// replaced is ignored, because acting on it would tear down the generation
// this host does hold and drop tablets the manager believes are served here.
// Nothing is returned and no loss is counted in that case.
func (h *Host) LoseLock(lost LockID) []Extent {
	h.mu.Lock()
	defer h.mu.Unlock()

	if !h.held.Valid() || !lost.Equal(h.held) {
		// Already released, or a stale notification for a generation we no
		// longer hold: nothing to hand back, and no new loss to record.
		return nil
	}
	h.held = LockID{}
	h.metrics.LockLosses++
	if len(h.tablets) == 0 {
		return nil
	}
	dropped := make([]Extent, 0, len(h.tablets))
	for _, entry := range h.tablets {
		dropped = append(dropped, entry.extent.clone())
	}
	sort.Slice(dropped, func(i, j int) bool { return compareExtents(dropped[i], dropped[j]) < 0 })
	h.tablets = make(map[string]*tabletEntry)
	h.byTable = make(map[string][]*tabletEntry)
	h.metrics.DroppedOnLockLoss += uint64(len(dropped))
	return dropped
}

// Lock returns the ServiceLock currently held, and whether one is held.
func (h *Host) Lock() (LockID, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.held, h.held.Valid()
}

// ObserveManagerLock records the authoritative live manager ServiceLock seen
// elsewhere, or clears it when offered the zero LockID.
//
// The host never elects a manager from RPC traffic. Manager authority stays
// external, and manager-directed requests are only accepted when their fence
// matches the lock observed here. Clearing the current live-manager
// observation does not discard monotonic history, so a stale observation
// cannot move authority backwards after a transient unknown period.
func (h *Host) ObserveManagerLock(lock LockID) error {
	h.mu.Lock()
	defer h.mu.Unlock()

	if !lock.Valid() {
		if lock == (LockID{}) {
			h.manager = LockID{}
			return nil
		}
		return fmt.Errorf("%w: %s", ErrInvalidLock, lock)
	}
	if h.newestManager.Valid() {
		switch {
		case lock.Equal(h.newestManager):
			h.manager = lock
			return nil
		case !lock.Supersedes(h.newestManager):
			return fmt.Errorf("%w: observed %s, already saw %s", ErrLockNotNewer, lock, h.newestManager)
		}
	}
	h.manager = lock
	h.newestManager = lock
	return nil
}

// ManagerLock returns the authoritative live manager ServiceLock this host
// currently knows, and whether one is currently known.
func (h *Host) ManagerLock() (LockID, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.manager, h.manager.Valid()
}

// Assign records a manager-directed assignment and moves the tablet to
// StateLoading, returning the attempt handle for it. The caller then brings
// the tablet online and reports the outcome with LoadComplete or LoadFailed,
// passing that handle back so the report can only ever apply to this
// assignment.
//
// It fails closed — leaving hosting state untouched — when the extent is
// malformed, the fence is stale, the tablet is already assigned here, or the
// extent overlaps one that is. Overlap covers the stale-split case: a parent
// extent arriving after its children are assigned (or the reverse) would host
// the same rows twice, so it is refused.
func (h *Host) Assign(fence Fence, extent Extent) (Attempt, error) {
	if err := extent.Validate(); err != nil {
		return Attempt{}, err
	}
	h.mu.Lock()
	defer h.mu.Unlock()

	if err := h.checkFence(fence); err != nil {
		return Attempt{}, err
	}
	if existing, ok := h.tablets[extent.key()]; ok {
		h.metrics.RejectedDuplicate++
		return Attempt{}, fmt.Errorf("%w: %s is %s", ErrAlreadyAssigned, existing.extent, existing.state)
	}
	if conflict := h.overlapping(extent); conflict != nil {
		h.metrics.RejectedDuplicate++
		return Attempt{}, fmt.Errorf("%w: %s overlaps %s (%s)",
			ErrOverlapping, extent, conflict.extent, conflict.state)
	}
	entry := &tabletEntry{extent: extent.clone(), state: StateLoading, attempt: h.nextAttempt}
	h.nextAttempt++
	h.track(entry)
	h.metrics.Assignments++
	return Attempt{extent: entry.extent, lock: h.held, id: entry.attempt}, nil
}

// LoadComplete reports that a loading tablet is online, moving it to
// StateHosted.
//
// attempt is the handle Assign returned. If its lock no longer matches, the
// load raced a lock loss and the tablet is not published — the manager has
// already been free to place it elsewhere. If the assignment itself has ended
// and the extent was assigned again, this returns ErrStaleAttempt rather than
// publishing somebody else's tablet. If the manager unassigned the tablet
// while it was loading it is in StateUnloading, and this returns
// ErrWrongState; the caller releases it with UnloadComplete instead.
func (h *Host) LoadComplete(attempt Attempt) error {
	return h.transition(attempt, StateLoading, func(entry *tabletEntry) {
		entry.state = StateHosted
		h.metrics.Loads++
	})
}

// LoadFailed reports that a loading tablet could not be brought online and
// releases it. The caller logs why; the host only records that an assignment
// was abandoned so the manager can place the tablet elsewhere.
//
// Like LoadComplete it applies only to the assignment attempt names, so a
// late failure cannot release a tablet assigned here since.
func (h *Host) LoadFailed(attempt Attempt) error {
	return h.transition(attempt, StateLoading, func(entry *tabletEntry) {
		h.release(entry)
		h.metrics.LoadFailures++
	})
}

// Unassign applies a manager-directed unassignment.
//
// UnloadGraceful moves the tablet to StateUnloading and waits for
// UnloadComplete, which is the migration and rolling-replacement path.
// UnloadImmediate releases it on the spot for a forced unload or a
// dead-server recovery handoff.
//
// It is idempotent only when the named extent is absent and no tracked tablet
// still covers its rows. In that case the manager asked for the tablet not to
// be hosted here, and it is not. A stale pre-split parent or post-merge child
// is not safe to treat as "already gone": if overlapping coverage is still
// tracked under another exact extent, reporting success would let the manager
// place those rows elsewhere while this host still claims them. The fence is
// still checked first, so a superseded manager cannot unassign anything.
//
// An unload mode this host does not implement is refused outright rather than
// guessed at, so a corrupted or newer mode cannot silently become a drain
// that never completes when a forced release was meant.
//
// A graceful unassignment returns the attempt handle of the tablet it began
// draining, which the caller passes to UnloadComplete when the drain is done.
// It is the same handle Assign minted, so a load that was unassigned
// mid-flight can give the tablet back with the handle it already holds. In
// every other case — the tablet was not tracked, or UnloadImmediate released
// it outright — the returned attempt is invalid and there is nothing left to
// finish.
func (h *Host) Unassign(fence Fence, extent Extent, mode UnloadMode) (Attempt, error) {
	if err := extent.Validate(); err != nil {
		return Attempt{}, err
	}
	if !mode.valid() {
		return Attempt{}, fmt.Errorf("%w: %s", ErrInvalidUnloadMode, mode)
	}
	h.mu.Lock()
	defer h.mu.Unlock()

	if err := h.checkFence(fence); err != nil {
		return Attempt{}, err
	}
	entry, ok := h.tablets[extent.key()]
	if !ok {
		if conflict := h.overlapping(extent); conflict != nil {
			return Attempt{}, fmt.Errorf("%w: %s is not tracked exactly; overlapping %s is %s",
				ErrOverlapping, extent, conflict.extent, conflict.state)
		}
		return Attempt{}, nil
	}
	if mode == UnloadImmediate {
		h.release(entry)
		h.metrics.Unloads++
		h.metrics.ForcedUnloads++
		return Attempt{}, nil
	}
	entry.state = StateUnloading
	return Attempt{extent: entry.extent, lock: h.held, id: entry.attempt}, nil
}

// UnloadComplete reports that a draining tablet has finished and releases it.
// It is the last step of a graceful unload, and also how a load that was
// unassigned mid-flight gives the tablet back.
//
// attempt is the handle Assign minted for the tablet, which Unassign returns
// again when it starts a drain. A drain that finishes after its tablet was
// released and the extent assigned here again is refused with
// ErrStaleAttempt, so it cannot release the new assignment.
func (h *Host) UnloadComplete(attempt Attempt) error {
	return h.transition(attempt, StateUnloading, func(entry *tabletEntry) {
		h.release(entry)
		h.metrics.Unloads++
	})
}

// State reports where an extent sits in the lifecycle. An extent this host
// does not track is StateUnassigned.
func (h *Host) State(extent Extent) HostingState {
	h.mu.Lock()
	defer h.mu.Unlock()
	entry, ok := h.tablets[extent.key()]
	if !ok {
		return StateUnassigned
	}
	return entry.state
}

// Hosted returns the tablets currently online here, in extent order. This is
// the set the manager can route reads and writes to.
func (h *Host) Hosted() []Extent {
	h.mu.Lock()
	defer h.mu.Unlock()
	hosted := make([]Extent, 0, len(h.tablets))
	for _, entry := range h.tablets {
		if entry.state == StateHosted {
			hosted = append(hosted, entry.extent.clone())
		}
	}
	sort.Slice(hosted, func(i, j int) bool { return compareExtents(hosted[i], hosted[j]) < 0 })
	return hosted
}

// VerifyHosted proves that attempt still names a hosted tablet under fence.
// Data-plane implementations call this immediately before durable work and
// again before acknowledging it.
func (h *Host) VerifyHosted(fence Fence, attempt Attempt) error {
	return h.verifyAssignedState(fence, attempt, StateHosted)
}

// VerifyAssigned proves that attempt still names a tablet in either loading
// or hosted state. It is used while recovery constructs a tablet before
// LoadComplete publishes it to data-plane routing.
func (h *Host) VerifyAssigned(fence Fence, attempt Attempt) error {
	return h.verifyAssignedState(fence, attempt, StateLoading, StateHosted)
}

func (h *Host) verifyAssignedState(fence Fence, attempt Attempt, allowed ...HostingState) error {
	if !attempt.Valid() {
		return fmt.Errorf("%w: attempt was never assigned", ErrStaleAttempt)
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if err := h.checkFence(fence); err != nil {
		return err
	}
	entry, ok := h.tablets[attempt.extent.key()]
	if !ok || entry.attempt != attempt.id {
		h.metrics.RejectedStale++
		return fmt.Errorf("%w: %s", ErrStaleAttempt, attempt.extent)
	}
	if !slices.Contains(allowed, entry.state) {
		return fmt.Errorf("%w: %s is %s, want one of %v",
			ErrWrongState, entry.extent, entry.state, allowed)
	}
	return nil
}

// Tablets returns every tracked tablet with its state, in extent order —
// including the ones still loading or draining, which the manager needs to
// see to tell a slow handoff from a stuck one.
func (h *Host) Tablets() []TabletStatus {
	h.mu.Lock()
	defer h.mu.Unlock()
	statuses := make([]TabletStatus, 0, len(h.tablets))
	for _, entry := range h.tablets {
		statuses = append(statuses, TabletStatus{Extent: entry.extent.clone(), State: entry.state})
	}
	sort.Slice(statuses, func(i, j int) bool {
		return compareExtents(statuses[i].Extent, statuses[j].Extent) < 0
	})
	return statuses
}

// Metrics returns a snapshot of the host's counters and gauges.
func (h *Host) Metrics() Metrics {
	h.mu.Lock()
	defer h.mu.Unlock()
	snapshot := h.metrics
	for _, entry := range h.tablets {
		switch entry.state {
		case StateLoading:
			snapshot.Loading++
		case StateHosted:
			snapshot.Hosted++
		case StateUnloading:
			snapshot.Unloading++
		}
	}
	return snapshot
}

// transition runs a local completion: it verifies the tablet is still ours
// under the attempt's lock, is the assignment the attempt was minted for, and
// is in the state the completion expects, then applies apply. Unlike the
// manager-facing calls it is strict about unknown extents — a completion for
// a tablet we never had is a bug in the caller, not a stale view of the
// cluster.
//
// The attempt check is what stops a slow completion from acting on a later
// assignment of the same extent: extent, lock and state can all match again
// after the tablet is released and assigned here once more, but the attempt
// id cannot.
func (h *Host) transition(attempt Attempt, want HostingState, apply func(*tabletEntry)) error {
	if !attempt.Valid() {
		return fmt.Errorf("%w: attempt was never assigned", ErrStaleAttempt)
	}
	extent := attempt.extent
	if err := extent.Validate(); err != nil {
		return err
	}
	h.mu.Lock()
	defer h.mu.Unlock()

	if err := h.checkServerLock(attempt.lock); err != nil {
		return err
	}
	entry, ok := h.tablets[extent.key()]
	if !ok {
		// Only Assign mints attempts, so a handle that reaches here named a
		// tablet this host really had: the assignment ended before the
		// completion arrived. That is the same superseded caller
		// ErrStaleAttempt counts, and the error stays distinct only because
		// there is no longer an assignment to name in its place.
		h.metrics.RejectedStale++
		return fmt.Errorf("%w: %s", ErrNotAssigned, extent)
	}
	if entry.attempt != attempt.id {
		h.metrics.RejectedStale++
		return fmt.Errorf("%w: %s is now %s, not %s",
			ErrStaleAttempt, extent,
			Attempt{extent: entry.extent, lock: attempt.lock, id: entry.attempt}, attempt)
	}
	if entry.state != want {
		return fmt.Errorf("%w: %s is %s, want %s", ErrWrongState, extent, entry.state, want)
	}
	apply(entry)
	return nil
}

// track starts tracking a tablet, in both the key map and the range index.
// Callers must hold h.mu.
func (h *Host) track(entry *tabletEntry) {
	h.tablets[entry.extent.key()] = entry
	entries := h.byTable[entry.extent.TableID]
	i := lowerBoundIndex(entries, entry.extent)
	entries = append(entries, nil)
	copy(entries[i+1:], entries[i:])
	entries[i] = entry
	h.byTable[entry.extent.TableID] = entries
}

// release stops tracking a tablet, removing it from both the key map and the
// range index. Callers must hold h.mu.
func (h *Host) release(entry *tabletEntry) {
	delete(h.tablets, entry.extent.key())

	table := entry.extent.TableID
	entries := h.byTable[table]
	i := lowerBoundIndex(entries, entry.extent)
	if i >= len(entries) || entries[i] != entry {
		return
	}
	// slices.Delete clears the vacated tail, so the released entry and the
	// row buffers it holds do not stay reachable through the backing array
	// of a table that still has tablets.
	entries = slices.Delete(entries, i, i+1)
	if len(entries) == 0 {
		delete(h.byTable, table)
		return
	}
	h.byTable[table] = entries
}

// overlapping returns a tracked tablet that shares rows with extent, or nil
// if none does. Callers must hold h.mu.
//
// Tracked extents within a table are pairwise disjoint — Assign refuses
// anything that is not — so ordering them by lower bound orders them by upper
// bound too, and their lower bounds are unique. That makes two comparisons
// enough: everything below the insertion point's predecessor ends at or
// before the predecessor starts, and everything above its successor starts at
// or after the successor ends, so neither can reach extent without the
// neighbour reaching it first. Assigning N tablets therefore costs
// N log N comparisons rather than N², which matters on the startup and
// recovery paths of a server hosting many thousands of tablets.
func (h *Host) overlapping(extent Extent) *tabletEntry {
	entries := h.byTable[extent.TableID]
	i := lowerBoundIndex(entries, extent)
	if i > 0 && entries[i-1].extent.Overlaps(extent) {
		return entries[i-1]
	}
	if i < len(entries) && entries[i].extent.Overlaps(extent) {
		return entries[i]
	}
	return nil
}

// lowerBoundIndex returns the position of the first entry whose lower bound
// is at or above extent's. entries must be sorted by lower bound.
func lowerBoundIndex(entries []*tabletEntry, extent Extent) int {
	return sort.Search(len(entries), func(i int) bool {
		return compareLowerBounds(entries[i].extent.prev(), extent.prev()) >= 0
	})
}

// checkFence validates a manager-directed request against both locks. The
// manager lock must match the authoritative live manager lock observed through
// ObserveManagerLock; request history alone never establishes authority.
// Callers must hold h.mu.
func (h *Host) checkFence(fence Fence) error {
	if err := h.checkServerLock(fence.Server); err != nil {
		return err
	}
	if !fence.Manager.Valid() {
		h.metrics.RejectedStale++
		return fmt.Errorf("%w: request carries no manager lock", ErrStaleManagerLock)
	}
	if !h.manager.Valid() {
		h.metrics.RejectedStale++
		return fmt.Errorf("%w: live manager lock unknown, request from %s", ErrStaleManagerLock, fence.Manager)
	}
	if !fence.Manager.Equal(h.manager) {
		h.metrics.RejectedStale++
		return fmt.Errorf("%w: request from %s, live manager is %s",
			ErrStaleManagerLock, fence.Manager, h.manager)
	}
	return nil
}

// checkServerLock validates a request against the ServiceLock this process
// holds. Callers must hold h.mu.
func (h *Host) checkServerLock(server LockID) error {
	if !h.held.Valid() {
		h.metrics.RejectedStale++
		return fmt.Errorf("%w: request stamped %s", ErrNoLock, server)
	}
	if !server.Equal(h.held) {
		h.metrics.RejectedStale++
		return fmt.Errorf("%w: request stamped %s, holding %s", ErrStaleServerLock, server, h.held)
	}
	return nil
}
