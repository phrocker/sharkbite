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
	"math/rand"
	"sync"
	"testing"
)

// The UUIDs below are fixed so failures name a stable holder. They have to be
// real UUIDs: a LockID is only trusted as fencing authority when it could
// name an actual "zlock#<uuid>#<sequence>" node.
const (
	serverUUID  = "6f1b2c8e-6a4d-4c2e-9f3b-1d5e7a9c0b21"
	managerUUID = "b91d4a70-3c85-4f11-8ad6-2e6c4b8f9d03"
	otherUUID   = "0c7e5a13-9b62-4d8f-a41c-77e2f5b6c8a9"
)

func serverLock(sequence int64) LockID {
	return LockID{UUID: serverUUID, Sequence: sequence}
}

func managerLock(sequence int64) LockID {
	return LockID{UUID: managerUUID, Sequence: sequence}
}

func mustObserveManager(t *testing.T, host *Host, lock LockID) {
	t.Helper()
	if err := host.ObserveManagerLock(lock); err != nil {
		t.Fatalf("ObserveManagerLock(%s): %v", lock, err)
	}
}

// newTestHost returns a host holding a lock, plus that lock and a fence
// stamped with it and a current manager.
func newTestHost(t *testing.T) (*Host, LockID, Fence) {
	t.Helper()
	host := NewHost()
	lock := serverLock(3)
	if err := host.AdoptLock(lock); err != nil {
		t.Fatalf("AdoptLock: %v", err)
	}
	manager := managerLock(7)
	mustObserveManager(t, host, manager)
	return host, lock, Fence{Server: lock, Manager: manager}
}

// hostTablet drives a tablet all the way to StateHosted and returns the
// attempt it was assigned under.
func hostTablet(t *testing.T, host *Host, fence Fence, e Extent) Attempt {
	t.Helper()
	attempt, err := host.Assign(fence, e)
	if err != nil {
		t.Fatalf("Assign(%s): %v", e, err)
	}
	if err := host.LoadComplete(attempt); err != nil {
		t.Fatalf("LoadComplete(%s): %v", e, err)
	}
	return attempt
}

// mustAssign assigns a tablet and returns its attempt.
func mustAssign(t *testing.T, host *Host, fence Fence, e Extent) Attempt {
	t.Helper()
	attempt, err := host.Assign(fence, e)
	if err != nil {
		t.Fatalf("Assign(%s): %v", e, err)
	}
	return attempt
}

func wantState(t *testing.T, host *Host, e Extent, want HostingState) {
	t.Helper()
	if got := host.State(e); got != want {
		t.Fatalf("state of %s = %s, want %s", e, got, want)
	}
}

func wantHosted(t *testing.T, host *Host, want ...string) {
	t.Helper()
	hosted := host.Hosted()
	if len(hosted) != len(want) {
		t.Fatalf("hosted = %v, want %v", hosted, want)
	}
	for i := range want {
		if hosted[i].String() != want[i] {
			t.Fatalf("hosted = %v, want %v", hosted, want)
		}
	}
}

// TestHostWithoutLockRefusesEverything covers a process that has not yet won
// the ServiceLock, or has lost it: it has no authority to host, so every
// manager-directed call and every local completion fails closed.
func TestHostWithoutLockRefusesEverything(t *testing.T) {
	host := NewHost()
	lock := serverLock(3)
	fence := Fence{Server: lock, Manager: managerLock(7)}
	e := extent("2", "", "m")

	if _, err := host.Assign(fence, e); !errors.Is(err, ErrNoLock) {
		t.Fatalf("Assign: want ErrNoLock, got %v", err)
	}
	if _, err := host.Unassign(fence, e, UnloadGraceful); !errors.Is(err, ErrNoLock) {
		t.Fatalf("Unassign: want ErrNoLock, got %v", err)
	}
	// A completion from a previous lock generation, replayed against a host
	// that holds nothing.
	orphan := Attempt{extent: e, lock: lock, id: 1}
	if err := host.LoadComplete(orphan); !errors.Is(err, ErrNoLock) {
		t.Fatalf("LoadComplete: want ErrNoLock, got %v", err)
	}
	if err := host.UnloadComplete(orphan); !errors.Is(err, ErrNoLock) {
		t.Fatalf("UnloadComplete: want ErrNoLock, got %v", err)
	}
	if _, held := host.Lock(); held {
		t.Fatal("a fresh host must not report a held lock")
	}
	wantState(t, host, e, StateUnassigned)
}

// TestAssignThroughHostedLifecycle is the happy path: the manager assigns,
// the load finishes, and the tablet becomes visible as hosted.
func TestAssignThroughHostedLifecycle(t *testing.T) {
	host, _, fence := newTestHost(t)
	e := extent("2", "", "m")

	attempt := mustAssign(t, host, fence, e)
	wantState(t, host, e, StateLoading)
	wantHosted(t, host) // still loading: not routable yet

	if err := host.LoadComplete(attempt); err != nil {
		t.Fatalf("LoadComplete: %v", err)
	}
	wantState(t, host, e, StateHosted)
	wantHosted(t, host, "2;m;<")

	metrics := host.Metrics()
	if metrics.Assignments != 1 || metrics.Loads != 1 || metrics.Hosted != 1 || metrics.Loading != 0 {
		t.Fatalf("metrics = %+v", metrics)
	}

	statuses := host.Tablets()
	if len(statuses) != 1 || statuses[0].State != StateHosted || !statuses[0].Extent.Equal(e) {
		t.Fatalf("tablets = %+v", statuses)
	}
}

// TestDuplicateAssignmentFailsClosed covers the acceptance criterion
// directly: a second assignment of a tablet already assigned here is refused
// and changes nothing.
func TestDuplicateAssignmentFailsClosed(t *testing.T) {
	host, _, fence := newTestHost(t)
	e := extent("2", "", "m")
	var attempt Attempt

	for _, state := range []HostingState{StateLoading, StateHosted} {
		if state == StateHosted {
			if err := host.LoadComplete(attempt); err != nil {
				t.Fatalf("LoadComplete: %v", err)
			}
		} else {
			attempt = mustAssign(t, host, fence, e)
		}

		_, err := host.Assign(fence, e)
		if !errors.Is(err, ErrAlreadyAssigned) {
			t.Fatalf("duplicate assign while %s: want ErrAlreadyAssigned, got %v", state, err)
		}
		wantState(t, host, e, state)
	}

	metrics := host.Metrics()
	if metrics.Assignments != 1 {
		t.Fatalf("a refused duplicate must not count as an assignment: %+v", metrics)
	}
	if metrics.RejectedDuplicate != 2 {
		t.Fatalf("RejectedDuplicate = %d, want 2", metrics.RejectedDuplicate)
	}
}

// TestOverlappingAssignmentFailsClosed covers stale split metadata: a parent
// extent arriving after its children are assigned would host the same rows
// twice, and the reverse direction is just as unsafe.
func TestOverlappingAssignmentFailsClosed(t *testing.T) {
	t.Run("parent after children", func(t *testing.T) {
		host, _, fence := newTestHost(t)
		left, right, parent := extent("2", "", "m"), extent("2", "m", ""), extent("2", "", "")

		mustAssign(t, host, fence, left)
		if _, err := host.Assign(fence, right); err != nil {
			t.Fatalf("split children must both be assignable: %v", err)
		}
		if _, err := host.Assign(fence, parent); !errors.Is(err, ErrOverlapping) {
			t.Fatalf("want ErrOverlapping, got %v", err)
		}
		wantState(t, host, parent, StateUnassigned)
		wantState(t, host, left, StateLoading)
		wantState(t, host, right, StateLoading)
	})

	t.Run("child after parent", func(t *testing.T) {
		host, _, fence := newTestHost(t)
		parent, child := extent("2", "", ""), extent("2", "d", "m")

		mustAssign(t, host, fence, parent)
		if _, err := host.Assign(fence, child); !errors.Is(err, ErrOverlapping) {
			t.Fatalf("want ErrOverlapping, got %v", err)
		}
		wantState(t, host, child, StateUnassigned)
	})

	t.Run("other tables are unaffected", func(t *testing.T) {
		host, _, fence := newTestHost(t)
		mustAssign(t, host, fence, extent("2", "", ""))
		if _, err := host.Assign(fence, extent("3", "", "")); err != nil {
			t.Fatalf("the same range in another table must be assignable: %v", err)
		}
	})
}

func TestAssignRejectsMalformedExtent(t *testing.T) {
	host, _, fence := newTestHost(t)
	if _, err := host.Assign(fence, extent("2", "m", "d")); !errors.Is(err, ErrInvalidExtent) {
		t.Fatalf("want ErrInvalidExtent, got %v", err)
	}
	if len(host.Tablets()) != 0 {
		t.Fatalf("a malformed extent must not be tracked: %+v", host.Tablets())
	}
}

// TestStaleServerLockFailsClosed covers an assignment the manager minted
// against a lock generation this process no longer holds.
func TestStaleServerLockFailsClosed(t *testing.T) {
	host, lock, fence := newTestHost(t)
	e := extent("2", "", "m")

	stale := Fence{Server: serverLock(lock.Sequence - 1), Manager: fence.Manager}
	if _, err := host.Assign(stale, e); !errors.Is(err, ErrStaleServerLock) {
		t.Fatalf("older lock: want ErrStaleServerLock, got %v", err)
	}
	// A generation we do not hold yet is equally unusable.
	ahead := Fence{Server: serverLock(lock.Sequence + 1), Manager: fence.Manager}
	if _, err := host.Assign(ahead, e); !errors.Is(err, ErrStaleServerLock) {
		t.Fatalf("unheld lock: want ErrStaleServerLock, got %v", err)
	}
	// Same sequence, different holder: ambiguous, so refuse.
	other := Fence{Server: LockID{UUID: otherUUID, Sequence: lock.Sequence}, Manager: fence.Manager}
	if _, err := host.Assign(other, e); !errors.Is(err, ErrStaleServerLock) {
		t.Fatalf("other holder: want ErrStaleServerLock, got %v", err)
	}

	wantState(t, host, e, StateUnassigned)
	if metrics := host.Metrics(); metrics.RejectedStale != 3 || metrics.Assignments != 0 {
		t.Fatalf("metrics = %+v", metrics)
	}
}

// TestSupersededManagerCannotCountermandTheLiveOne covers a manager failover:
// once a newer manager lock is seen it becomes the authority, and the old
// manager's requests are refused.
func TestSupersededManagerCannotCountermandTheLiveOne(t *testing.T) {
	host, lock, fence := newTestHost(t)
	old, current := extent("2", "", "m"), extent("2", "m", "")

	mustAssign(t, host, fence, old)

	newer := Fence{Server: lock, Manager: managerLock(fence.Manager.Sequence + 1)}
	mustObserveManager(t, host, newer.Manager)
	if _, err := host.Assign(newer, current); err != nil {
		t.Fatalf("a newer manager is the authority: %v", err)
	}
	if got, ok := host.ManagerLock(); !ok || !got.Equal(newer.Manager) {
		t.Fatalf("manager lock = %s (held=%v), want %s", got, ok, newer.Manager)
	}

	if _, err := host.Unassign(fence, current, UnloadImmediate); !errors.Is(err, ErrStaleManagerLock) {
		t.Fatalf("superseded manager: want ErrStaleManagerLock, got %v", err)
	}
	wantState(t, host, current, StateLoading)

	unstamped := Fence{Server: lock}
	if _, err := host.Assign(unstamped, extent("2", "q", "z")); !errors.Is(err, ErrStaleManagerLock) {
		t.Fatalf("missing manager lock: want ErrStaleManagerLock, got %v", err)
	}
}

// TestDelayedRPCFromSupersededManagerFailsBeforeSuccessorRequest covers the
// first RPC that arrives after manager failover: once the live manager lock is
// updated externally, the predecessor's delayed request is stale immediately
// even if no request from the successor has reached this host yet.
func TestDelayedRPCFromSupersededManagerFailsBeforeSuccessorRequest(t *testing.T) {
	host, lock, fence := newTestHost(t)
	e := extent("2", "", "m")

	hostTablet(t, host, fence, e)

	newer := Fence{Server: lock, Manager: managerLock(fence.Manager.Sequence + 1)}
	mustObserveManager(t, host, newer.Manager)

	if _, err := host.Unassign(fence, e, UnloadImmediate); !errors.Is(err, ErrStaleManagerLock) {
		t.Fatalf("delayed predecessor RPC: want ErrStaleManagerLock, got %v", err)
	}
	wantState(t, host, e, StateHosted)
	wantHosted(t, host, "2;m;<")

	if _, err := host.Unassign(newer, e, UnloadImmediate); err != nil {
		t.Fatalf("successor manager: %v", err)
	}
	wantState(t, host, e, StateUnassigned)
}

// TestObserveManagerLockClearKeepsMonotonicHistory covers a transient loss of
// live-manager knowledge. Clearing the current observation must leave requests
// fail-closed, and it must not let an older manager epoch become current if it
// is observed before the real live manager is rediscovered.
func TestObserveManagerLockClearKeepsMonotonicHistory(t *testing.T) {
	host, lock, fence := newTestHost(t)
	e := extent("2", "", "m")
	current := Fence{Server: lock, Manager: managerLock(fence.Manager.Sequence + 1)}

	mustObserveManager(t, host, current.Manager)
	if err := host.ObserveManagerLock(LockID{}); err != nil {
		t.Fatalf("clearing live manager: %v", err)
	}
	if got, ok := host.ManagerLock(); ok {
		t.Fatalf("manager lock = %s (held=%v), want unknown after clear", got, ok)
	}
	if _, err := host.Assign(current, e); !errors.Is(err, ErrStaleManagerLock) {
		t.Fatalf("request while live manager unknown: want ErrStaleManagerLock, got %v", err)
	}

	if err := host.ObserveManagerLock(fence.Manager); !errors.Is(err, ErrLockNotNewer) {
		t.Fatalf("older re-observation: want ErrLockNotNewer, got %v", err)
	}
	if got, ok := host.ManagerLock(); ok {
		t.Fatalf("manager lock = %s (held=%v), want still unknown after rollback attempt", got, ok)
	}
	if _, err := host.Assign(current, e); !errors.Is(err, ErrStaleManagerLock) {
		t.Fatalf("request after rollback attempt while unknown: want ErrStaleManagerLock, got %v", err)
	}

	mustObserveManager(t, host, current.Manager)
	if _, err := host.Assign(current, e); err != nil {
		t.Fatalf("re-observed current manager: %v", err)
	}
	wantState(t, host, e, StateLoading)
}

// TestGracefulUnloadDrainsThenReleases is the migration path: the tablet
// stops being routable as soon as the manager asks for it back, but is only
// released once the drain finishes.
func TestGracefulUnloadDrainsThenReleases(t *testing.T) {
	host, _, fence := newTestHost(t)
	e := extent("2", "", "m")
	hostTablet(t, host, fence, e)

	drain, err := host.Unassign(fence, e, UnloadGraceful)
	if err != nil {
		t.Fatalf("Unassign: %v", err)
	}
	if !drain.Valid() {
		t.Fatal("a graceful unassignment must hand back the attempt to finish")
	}
	wantState(t, host, e, StateUnloading)
	wantHosted(t, host) // no longer routable

	// Still assigned here, so it cannot be handed back to this host yet.
	if _, err := host.Assign(fence, e); !errors.Is(err, ErrAlreadyAssigned) {
		t.Fatalf("re-assign while draining: want ErrAlreadyAssigned, got %v", err)
	}

	if err := host.UnloadComplete(drain); err != nil {
		t.Fatalf("UnloadComplete: %v", err)
	}
	wantState(t, host, e, StateUnassigned)

	// Released: the manager can migrate it straight back.
	if _, err := host.Assign(fence, e); err != nil {
		t.Fatalf("re-assign after release: %v", err)
	}

	metrics := host.Metrics()
	if metrics.Unloads != 1 || metrics.ForcedUnloads != 0 {
		t.Fatalf("metrics = %+v", metrics)
	}
}

// TestImmediateUnloadReleasesAtOnce covers forced unload and dead-server
// recovery handoff, where the manager needs the tablet back now.
func TestImmediateUnloadReleasesAtOnce(t *testing.T) {
	host, _, fence := newTestHost(t)
	hosted, loading := extent("2", "", "m"), extent("2", "m", "")
	hostTablet(t, host, fence, hosted)
	mustAssign(t, host, fence, loading)

	for _, e := range []Extent{hosted, loading} {
		drain, err := host.Unassign(fence, e, UnloadImmediate)
		if err != nil {
			t.Fatalf("Unassign(%s): %v", e, err)
		}
		if drain.Valid() {
			t.Fatalf("an immediate unload leaves nothing to finish, got %s", drain)
		}
		wantState(t, host, e, StateUnassigned)
	}

	metrics := host.Metrics()
	if metrics.Unloads != 2 || metrics.ForcedUnloads != 2 {
		t.Fatalf("metrics = %+v", metrics)
	}
}

// TestUnassignRejectsUnknownOverlappingExtent covers stale split/merge
// metadata: when the exact extent is unknown but overlapping rows are still
// assigned under another extent, treating the request as idempotent would let
// those rows be hosted elsewhere too.
func TestUnassignRejectsUnknownOverlappingExtent(t *testing.T) {
	t.Run("parent after child", func(t *testing.T) {
		host, _, fence := newTestHost(t)
		child, parent := extent("2", "", "m"), extent("2", "", "")

		hostTablet(t, host, fence, child)
		if _, err := host.Unassign(fence, parent, UnloadImmediate); !errors.Is(err, ErrOverlapping) {
			t.Fatalf("want ErrOverlapping, got %v", err)
		}
		wantState(t, host, child, StateHosted)
		wantHosted(t, host, "2;m;<")
	})

	t.Run("child after parent", func(t *testing.T) {
		host, _, fence := newTestHost(t)
		parent, child := extent("2", "", ""), extent("2", "d", "m")

		hostTablet(t, host, fence, parent)
		if _, err := host.Unassign(fence, child, UnloadImmediate); !errors.Is(err, ErrOverlapping) {
			t.Fatalf("want ErrOverlapping, got %v", err)
		}
		wantState(t, host, parent, StateHosted)
		wantHosted(t, host, "2;<;<")
	})
}

// TestUnassignIsIdempotent covers the manager retrying an unassignment, or
// unassigning a tablet this host never had once no tracked tablet still covers
// those rows: the requested end state already holds, so there is nothing to
// refuse.
func TestUnassignIsIdempotent(t *testing.T) {
	host, _, fence := newTestHost(t)
	e := extent("2", "", "m")

	unknown, err := host.Unassign(fence, e, UnloadGraceful)
	if err != nil {
		t.Fatalf("unknown extent: %v", err)
	}
	if unknown.Valid() {
		t.Fatalf("a tablet we never had leaves nothing to finish, got %s", unknown)
	}
	hostTablet(t, host, fence, e)
	first, err := host.Unassign(fence, e, UnloadGraceful)
	if err != nil {
		t.Fatalf("first unassign: %v", err)
	}
	repeat, err := host.Unassign(fence, e, UnloadGraceful)
	if err != nil {
		t.Fatalf("repeated unassign: %v", err)
	}
	if !repeat.Equal(first) {
		t.Fatalf("a repeated unassign must name the same drain: %s then %s", first, repeat)
	}
	wantState(t, host, e, StateUnloading)

	if err := host.UnloadComplete(first); err != nil {
		t.Fatalf("UnloadComplete: %v", err)
	}
	if _, err := host.Unassign(fence, e, UnloadImmediate); err != nil {
		t.Fatalf("unassign after release: %v", err)
	}
	if metrics := host.Metrics(); metrics.Unloads != 1 {
		t.Fatalf("idempotent unassigns must not double-count: %+v", metrics)
	}
}

func TestLoadFailedReleasesTheAssignment(t *testing.T) {
	host, _, fence := newTestHost(t)
	e := extent("2", "", "m")
	attempt := mustAssign(t, host, fence, e)

	if err := host.LoadFailed(attempt); err != nil {
		t.Fatalf("LoadFailed: %v", err)
	}
	wantState(t, host, e, StateUnassigned)

	if err := host.LoadFailed(attempt); !errors.Is(err, ErrNotAssigned) {
		t.Fatalf("want ErrNotAssigned, got %v", err)
	}
	// Only Assign mints attempts, so a handle that finds nothing tracked is
	// reporting on an assignment that ended — the same superseded caller
	// RejectedStale counts everywhere else.
	if metrics := host.Metrics(); metrics.LoadFailures != 1 || metrics.RejectedStale != 1 {
		t.Fatalf("metrics = %+v", metrics)
	}
}

// TestLoadCompleteAfterUnassignDoesNotRepublish covers a load that finishes
// after the manager already took the tablet back: publishing it would host a
// tablet the manager has reassigned, so the load must release it instead.
func TestLoadCompleteAfterUnassignDoesNotRepublish(t *testing.T) {
	host, _, fence := newTestHost(t)
	e := extent("2", "", "m")
	attempt := mustAssign(t, host, fence, e)
	drain, err := host.Unassign(fence, e, UnloadGraceful)
	if err != nil {
		t.Fatalf("Unassign: %v", err)
	}
	// The drain is the same attempt, so the loader can finish with the handle
	// it already holds.
	if !drain.Equal(attempt) {
		t.Fatalf("drain %s must be the loading attempt %s", drain, attempt)
	}

	if err := host.LoadComplete(attempt); !errors.Is(err, ErrWrongState) {
		t.Fatalf("want ErrWrongState, got %v", err)
	}
	wantState(t, host, e, StateUnloading)
	wantHosted(t, host)

	if err := host.UnloadComplete(attempt); err != nil {
		t.Fatalf("UnloadComplete: %v", err)
	}
	wantState(t, host, e, StateUnassigned)
}

func TestCompletionsRejectWrongState(t *testing.T) {
	host, _, fence := newTestHost(t)
	e := extent("2", "", "m")
	attempt := hostTablet(t, host, fence, e)

	if err := host.LoadComplete(attempt); !errors.Is(err, ErrWrongState) {
		t.Fatalf("LoadComplete on a hosted tablet: want ErrWrongState, got %v", err)
	}
	if err := host.UnloadComplete(attempt); !errors.Is(err, ErrWrongState) {
		t.Fatalf("UnloadComplete on a hosted tablet: want ErrWrongState, got %v", err)
	}
	wantState(t, host, e, StateHosted)
}

// TestLockLossDropsEverything covers the acceptance criterion that a
// ServiceLock loss cannot leave a tablet multiply hosted: the host stops
// claiming every tablet at once, and any work still in flight under the lost
// lock fails closed instead of publishing.
func TestLockLossDropsEverything(t *testing.T) {
	host, lock, fence := newTestHost(t)
	loading, hosted, draining := extent("2", "", "d"), extent("2", "d", "m"), extent("2", "m", "")

	loadingAttempt := mustAssign(t, host, fence, loading)
	hostTablet(t, host, fence, hosted)
	hostTablet(t, host, fence, draining)
	if _, err := host.Unassign(fence, draining, UnloadGraceful); err != nil {
		t.Fatalf("Unassign: %v", err)
	}

	dropped := host.LoseLock(lock)
	if len(dropped) != 3 {
		t.Fatalf("dropped = %v, want all three tablets", dropped)
	}
	for i, want := range []string{"2;d;<", "2;m;d", "2;<;m"} {
		if dropped[i].String() != want {
			t.Fatalf("dropped = %v, want %v in extent order", dropped, []string{"2;d;<", "2;m;d", "2;<;m"})
		}
	}
	wantHosted(t, host)
	if len(host.Tablets()) != 0 {
		t.Fatalf("no tablet may survive a lock loss: %+v", host.Tablets())
	}

	// In-flight work stamped with the lost lock must not publish anything.
	if err := host.LoadComplete(loadingAttempt); !errors.Is(err, ErrNoLock) {
		t.Fatalf("want ErrNoLock, got %v", err)
	}
	if _, err := host.Assign(fence, hosted); !errors.Is(err, ErrNoLock) {
		t.Fatalf("want ErrNoLock, got %v", err)
	}

	metrics := host.Metrics()
	if metrics.LockLosses != 1 || metrics.DroppedOnLockLoss != 3 {
		t.Fatalf("metrics = %+v", metrics)
	}

	// A second call for the same loss is a no-op.
	if dropped := host.LoseLock(lock); dropped != nil {
		t.Fatalf("repeat LoseLock returned %v", dropped)
	}
	if metrics := host.Metrics(); metrics.LockLosses != 1 {
		t.Fatalf("repeat LoseLock double-counted: %+v", metrics)
	}
}

// TestReacquiredLockStartsEmpty covers the restart path: after the lock comes
// back the host holds nothing until the manager assigns again, and requests
// stamped with the old generation stay refused.
func TestReacquiredLockStartsEmpty(t *testing.T) {
	host, lock, fence := newTestHost(t)
	e := extent("2", "", "m")
	hostTablet(t, host, fence, e)
	host.LoseLock(lock)

	reacquired := serverLock(lock.Sequence + 5)
	if err := host.AdoptLock(reacquired); err != nil {
		t.Fatalf("AdoptLock: %v", err)
	}
	if got, ok := host.Lock(); !ok || !got.Equal(reacquired) {
		t.Fatalf("lock = %s (held=%v), want %s", got, ok, reacquired)
	}
	wantState(t, host, e, StateUnassigned)

	if _, err := host.Assign(fence, e); !errors.Is(err, ErrStaleServerLock) {
		t.Fatalf("old-generation assign: want ErrStaleServerLock, got %v", err)
	}
	if _, err := host.Assign(Fence{Server: reacquired, Manager: fence.Manager}, e); err != nil {
		t.Fatalf("assign under the new generation: %v", err)
	}
	wantState(t, host, e, StateLoading)
}

func TestAdoptLockGuards(t *testing.T) {
	host, lock, fence := newTestHost(t)

	if err := host.AdoptLock(LockID{}); !errors.Is(err, ErrInvalidLock) {
		t.Fatalf("invalid lock: want ErrInvalidLock, got %v", err)
	}
	if err := host.AdoptLock(lock); !errors.Is(err, ErrLockNotNewer) {
		t.Fatalf("same generation: want ErrLockNotNewer, got %v", err)
	}
	if err := host.AdoptLock(serverLock(lock.Sequence - 1)); !errors.Is(err, ErrLockNotNewer) {
		t.Fatalf("older generation: want ErrLockNotNewer, got %v", err)
	}

	mustAssign(t, host, fence, extent("2", "", "m"))
	if err := host.AdoptLock(serverLock(lock.Sequence + 1)); !errors.Is(err, ErrTabletsAssigned) {
		t.Fatalf("adopting while hosting: want ErrTabletsAssigned, got %v", err)
	}

	// A generation already burned by a lock loss cannot come back.
	host.LoseLock(lock)
	if err := host.AdoptLock(lock); !errors.Is(err, ErrLockNotNewer) {
		t.Fatalf("replayed lock after loss: want ErrLockNotNewer, got %v", err)
	}
}

// TestAssignCopiesExtentBounds makes sure a caller reusing its row buffers
// cannot rewrite what the host thinks it hosts.
func TestAssignCopiesExtentBounds(t *testing.T) {
	host, _, fence := newTestHost(t)
	end := []byte("m")
	e := Extent{TableID: "2", EndRow: end}

	attempt := mustAssign(t, host, fence, e)
	if err := host.LoadComplete(attempt); err != nil {
		t.Fatalf("LoadComplete: %v", err)
	}
	end[0] = 'z'

	wantHosted(t, host, "2;m;<")
	hosted := host.Hosted()
	hosted[0].EndRow[0] = 'q'
	wantHosted(t, host, "2;m;<")

	// The attempt carries its own copy too, so a caller cannot retarget the
	// completion it is holding.
	if got := attempt.Extent(); !got.Equal(Extent{TableID: "2", EndRow: []byte("m")}) {
		t.Fatalf("attempt extent = %s, want 2;m;<", got)
	}
}

// TestConcurrentAssignHostsOnce is the race the fence exists for: several
// manager retries landing at once must produce exactly one assignment.
func TestConcurrentAssignHostsOnce(t *testing.T) {
	host, _, fence := newTestHost(t)
	e := extent("2", "", "m")

	const attempts = 16
	var (
		wg        sync.WaitGroup
		mu        sync.Mutex
		accepted  int
		duplicate int
	)
	wg.Add(attempts)
	for i := 0; i < attempts; i++ {
		go func() {
			defer wg.Done()
			_, err := host.Assign(fence, e)
			mu.Lock()
			defer mu.Unlock()
			switch {
			case err == nil:
				accepted++
			case errors.Is(err, ErrAlreadyAssigned):
				duplicate++
			default:
				t.Errorf("unexpected error: %v", err)
			}
		}()
	}
	wg.Wait()

	if accepted != 1 || duplicate != attempts-1 {
		t.Fatalf("accepted=%d duplicate=%d, want 1 and %d", accepted, duplicate, attempts-1)
	}
	wantState(t, host, e, StateLoading)
	if metrics := host.Metrics(); metrics.Assignments != 1 || metrics.Loading != 1 {
		t.Fatalf("metrics = %+v", metrics)
	}
}

// TestConcurrentAssignAndLockLoss checks that assignments racing a lock loss
// either land before it (and get dropped) or fail closed after it — never
// leaving a tablet claimed by a host that no longer holds the lock.
func TestConcurrentAssignAndLockLoss(t *testing.T) {
	host, lock, fence := newTestHost(t)

	const attempts = 16
	var wg sync.WaitGroup
	wg.Add(attempts + 1)
	for i := 0; i < attempts; i++ {
		go func(i int) {
			defer wg.Done()
			e := Extent{TableID: "2", PrevEndRow: []byte{byte('a' + i)}, EndRow: []byte{byte('a' + i), 0}}
			if _, err := host.Assign(fence, e); err != nil && !errors.Is(err, ErrNoLock) {
				t.Errorf("assign %s: unexpected error: %v", e, err)
			}
		}(i)
	}
	go func() {
		defer wg.Done()
		host.LoseLock(lock)
	}()
	wg.Wait()

	if tablets := host.Tablets(); len(tablets) != 0 {
		t.Fatalf("tablets survived the lock loss: %+v", tablets)
	}
	if _, held := host.Lock(); held {
		t.Fatal("lock still reported as held")
	}
}

// TestLoseLockIgnoresSupersededGeneration covers a delayed or duplicated
// watcher callback. The loss notification is fenced to its own generation, so
// a callback for a lock that has already been replaced must not tear down the
// generation now held and hand back tablets the manager believes are served.
func TestLoseLockIgnoresSupersededGeneration(t *testing.T) {
	host, lock, fence := newTestHost(t)
	e := extent("2", "", "m")
	hostTablet(t, host, fence, e)

	if dropped := host.LoseLock(lock); len(dropped) != 1 {
		t.Fatalf("dropped = %v, want the hosted tablet", dropped)
	}

	// Reconnect under a newer generation and host the tablet again.
	next := serverLock(lock.Sequence + 1)
	if err := host.AdoptLock(next); err != nil {
		t.Fatalf("AdoptLock: %v", err)
	}
	hostTablet(t, host, Fence{Server: next, Manager: fence.Manager}, e)

	if dropped := host.LoseLock(lock); dropped != nil {
		t.Fatalf("a superseded lock loss dropped %v", dropped)
	}
	if got, held := host.Lock(); !held || !got.Equal(next) {
		t.Fatalf("lock = %s (held=%v), want %s still held", got, held, next)
	}
	wantHosted(t, host, "2;m;<")
	if metrics := host.Metrics(); metrics.LockLosses != 1 || metrics.DroppedOnLockLoss != 1 {
		t.Fatalf("a superseded loss was counted: %+v", metrics)
	}

	// The generation actually held can still be lost.
	if dropped := host.LoseLock(next); len(dropped) != 1 {
		t.Fatalf("dropped = %v, want the tablet hosted under %s", dropped, next)
	}
	if metrics := host.Metrics(); metrics.LockLosses != 2 || metrics.DroppedOnLockLoss != 2 {
		t.Fatalf("metrics = %+v", metrics)
	}
}

// TestUnassignRejectsUnknownMode checks that an unload mode this host does
// not implement is refused rather than treated as a drain, which would leave
// the tablet in UNLOADING forever when a forced release was meant.
func TestUnassignRejectsUnknownMode(t *testing.T) {
	host, _, fence := newTestHost(t)
	e := extent("2", "", "m")
	hostTablet(t, host, fence, e)

	if _, err := host.Unassign(fence, e, UnloadMode(42)); !errors.Is(err, ErrInvalidUnloadMode) {
		t.Fatalf("want ErrInvalidUnloadMode, got %v", err)
	}
	wantState(t, host, e, StateHosted)
	wantHosted(t, host, "2;m;<")
	if metrics := host.Metrics(); metrics.Unloads != 0 {
		t.Fatalf("an uninterpretable mode released the tablet: %+v", metrics)
	}
}

// TestUnassignReleasesTabletSpelledWithEmptyBound covers the decode mismatch
// the shared nil-or-empty bound semantics prevent. A tablet assigned with a
// nil bound and unassigned with an empty one is the same tablet, so the
// unassignment must release it — if the two keyed differently, the idempotent
// missing-tablet path would report success while the tablet stayed hosted
// here.
func TestUnassignReleasesTabletSpelledWithEmptyBound(t *testing.T) {
	host, _, fence := newTestHost(t)
	assigned := Extent{TableID: "2", PrevEndRow: nil, EndRow: []byte("m")}
	spelledEmpty := Extent{TableID: "2", PrevEndRow: []byte{}, EndRow: []byte("m")}

	hostTablet(t, host, fence, assigned)
	if _, err := host.Unassign(fence, spelledEmpty, UnloadImmediate); err != nil {
		t.Fatalf("Unassign: %v", err)
	}
	wantState(t, host, assigned, StateUnassigned)
	wantHosted(t, host)
	if metrics := host.Metrics(); metrics.Unloads != 1 {
		t.Fatalf("the tablet was not released: %+v", metrics)
	}

	// The reverse spelling is also the same tablet, so re-assigning it is a
	// duplicate rather than a second tablet covering the same rows.
	mustAssign(t, host, fence, spelledEmpty)
	if _, err := host.Assign(fence, assigned); !errors.Is(err, ErrAlreadyAssigned) {
		t.Fatalf("want ErrAlreadyAssigned, got %v", err)
	}
}

// TestStaleCompletionCannotDisturbALaterAssignment is the hazard the attempt
// handle exists for. The manager may unassign a tablet and assign it here
// again without the ServiceLock ever changing, so extent, lock and state all
// match a second time. A completion left over from the first assignment would
// otherwise land on the second one — releasing a tablet that is still loading,
// or publishing one whose data is not loaded yet.
func TestStaleCompletionCannotDisturbALaterAssignment(t *testing.T) {
	// Both completions are checked from the same starting point: a tablet
	// assigned, taken back, and assigned here again under one lock.
	setup := func(t *testing.T) (*Host, Fence, Extent, Attempt, Attempt) {
		t.Helper()
		host, _, fence := newTestHost(t)
		e := extent("2", "", "m")

		first := mustAssign(t, host, fence, e)
		if _, err := host.Unassign(fence, e, UnloadImmediate); err != nil {
			t.Fatalf("Unassign: %v", err)
		}
		second := mustAssign(t, host, fence, e)
		if first.Equal(second) {
			t.Fatal("a second assignment must not reuse the first attempt")
		}
		return host, fence, e, first, second
	}

	t.Run("late failure does not release the new assignment", func(t *testing.T) {
		host, _, e, first, second := setup(t)

		if err := host.LoadFailed(first); !errors.Is(err, ErrStaleAttempt) {
			t.Fatalf("want ErrStaleAttempt, got %v", err)
		}
		wantState(t, host, e, StateLoading)
		if metrics := host.Metrics(); metrics.LoadFailures != 0 || metrics.RejectedStale != 1 {
			t.Fatalf("metrics = %+v", metrics)
		}

		// The assignment that is actually loading still finishes normally.
		if err := host.LoadComplete(second); err != nil {
			t.Fatalf("LoadComplete: %v", err)
		}
		wantHosted(t, host, "2;m;<")
	})

	t.Run("late success does not publish the new assignment", func(t *testing.T) {
		host, _, e, first, second := setup(t)

		if err := host.LoadComplete(first); !errors.Is(err, ErrStaleAttempt) {
			t.Fatalf("want ErrStaleAttempt, got %v", err)
		}
		wantState(t, host, e, StateLoading)
		wantHosted(t, host) // never loaded, so never routable
		if metrics := host.Metrics(); metrics.Loads != 0 {
			t.Fatalf("a stale load was published: %+v", metrics)
		}

		if err := host.LoadFailed(second); err != nil {
			t.Fatalf("LoadFailed: %v", err)
		}
		wantState(t, host, e, StateUnassigned)
	})

	t.Run("late drain does not release the new assignment", func(t *testing.T) {
		host, fence, e, _, second := setup(t)

		if err := host.LoadComplete(second); err != nil {
			t.Fatalf("LoadComplete: %v", err)
		}
		drain, err := host.Unassign(fence, e, UnloadGraceful)
		if err != nil {
			t.Fatalf("Unassign: %v", err)
		}
		if err := host.UnloadComplete(drain); err != nil {
			t.Fatalf("UnloadComplete: %v", err)
		}
		third := mustAssign(t, host, fence, e)

		if err := host.UnloadComplete(drain); !errors.Is(err, ErrStaleAttempt) {
			t.Fatalf("want ErrStaleAttempt, got %v", err)
		}
		wantState(t, host, e, StateLoading)
		if err := host.LoadComplete(third); err != nil {
			t.Fatalf("LoadComplete: %v", err)
		}
		wantHosted(t, host, "2;m;<")
	})
}

// TestReleaseClearsTheIndexTail pins that a released tablet stops being
// reachable through the range index. Deleting by reslicing alone would leave
// the removed entry — and the row buffers it holds — live in the backing
// array for as long as the table keeps any tablet at all.
func TestReleaseClearsTheIndexTail(t *testing.T) {
	host, _, fence := newTestHost(t)
	left, right := extent("2", "", "m"), extent("2", "m", "")
	hostTablet(t, host, fence, left)
	hostTablet(t, host, fence, right)

	entries := host.byTable["2"]
	if len(entries) != 2 {
		t.Fatalf("index = %v, want both tablets", entries)
	}
	backing := entries[:cap(entries)]

	if _, err := host.Unassign(fence, left, UnloadImmediate); err != nil {
		t.Fatalf("Unassign: %v", err)
	}

	remaining := host.byTable["2"]
	if len(remaining) != 1 || !remaining[0].extent.Equal(right) {
		t.Fatalf("index = %v, want only %s", remaining, right)
	}
	for i := len(remaining); i < len(backing); i++ {
		if backing[i] != nil {
			t.Fatalf("released entry %s is still reachable at backing index %d",
				backing[i].extent, i)
		}
	}
}

// TestCompletionsRejectUnmintedAttempts covers a caller that fabricates a
// completion instead of carrying the handle Assign gave it. The zero Attempt
// names no assignment, so it can never satisfy one.
func TestCompletionsRejectUnmintedAttempts(t *testing.T) {
	host, _, fence := newTestHost(t)
	e := extent("2", "", "m")
	hostTablet(t, host, fence, e)

	for name, err := range map[string]error{
		"LoadComplete":   host.LoadComplete(Attempt{}),
		"LoadFailed":     host.LoadFailed(Attempt{}),
		"UnloadComplete": host.UnloadComplete(Attempt{}),
	} {
		if !errors.Is(err, ErrStaleAttempt) {
			t.Fatalf("%s(zero attempt): want ErrStaleAttempt, got %v", name, err)
		}
	}
	wantState(t, host, e, StateHosted)
	wantHosted(t, host, "2;m;<")
}

// TestAttemptString keeps the log rendering readable, including the zero
// value a caller may hold after an unassignment that had nothing to finish.
func TestAttemptString(t *testing.T) {
	if got := (Attempt{}).String(); got != "attempt#<none>" {
		t.Fatalf("zero attempt = %q", got)
	}
	host, lock, fence := newTestHost(t)
	attempt := mustAssign(t, host, fence, extent("2", "", "m"))
	want := fmt.Sprintf("attempt#1(2;m;< under %s)", lock)
	if got := attempt.String(); got != want {
		t.Fatalf("attempt = %q, want %q", got, want)
	}
}

// TestOverlapDetectionSurvivesRelease pins the failure mode the range index
// could introduce: a released tablet must leave the index, and the ones still
// tracked must stay findable.
func TestOverlapDetectionSurvivesRelease(t *testing.T) {
	host, _, fence := newTestHost(t)
	left, right, whole := extent("2", "", "m"), extent("2", "m", ""), extent("2", "", "")

	hostTablet(t, host, fence, left)
	hostTablet(t, host, fence, right)
	if _, err := host.Assign(fence, whole); !errors.Is(err, ErrOverlapping) {
		t.Fatalf("parent over both children: want ErrOverlapping, got %v", err)
	}

	if _, err := host.Unassign(fence, left, UnloadImmediate); err != nil {
		t.Fatalf("Unassign(left): %v", err)
	}
	if _, err := host.Assign(fence, whole); !errors.Is(err, ErrOverlapping) {
		t.Fatalf("parent over the remaining child: want ErrOverlapping, got %v", err)
	}
	if _, err := host.Assign(fence, left); err != nil {
		t.Fatalf("the released child must be assignable again: %v", err)
	}
	if _, err := host.Unassign(fence, left, UnloadImmediate); err != nil {
		t.Fatalf("Unassign(left): %v", err)
	}

	if _, err := host.Unassign(fence, right, UnloadImmediate); err != nil {
		t.Fatalf("Unassign(right): %v", err)
	}
	if _, err := host.Assign(fence, whole); err != nil {
		t.Fatalf("with both children gone the parent must fit: %v", err)
	}
}

// overlapRows is a small ordered row alphabet, so randomly drawn extents
// collide often enough to exercise both outcomes.
var overlapRows = []string{"b", "d", "f", "h", "j"}

// randomExtent draws a valid extent over overlapRows. Position 0 is the
// absent lower bound and the last position the absent upper bound.
func randomExtent(rng *rand.Rand, table string) Extent {
	n := len(overlapRows) + 2
	lo := rng.Intn(n - 1)
	hi := lo + 1 + rng.Intn(n-lo-1)
	e := Extent{TableID: table}
	if lo > 0 {
		e.PrevEndRow = []byte(overlapRows[lo-1])
	}
	if hi < n-1 {
		e.EndRow = []byte(overlapRows[hi-1])
	}
	return e
}

// anyOverlapByScan is the full scan the per-table range index replaced, kept
// as the reference implementation for the differential test below.
func anyOverlapByScan(host *Host, e Extent) bool {
	for _, entry := range host.tablets {
		if entry.extent.Overlaps(e) {
			return true
		}
	}
	return false
}

// checkIndex asserts the range index still mirrors the tracked tablets: same
// membership, grouped by table, ordered by lower bound, no empty leftovers.
func checkIndex(t *testing.T, host *Host) {
	t.Helper()
	indexed := 0
	for table, entries := range host.byTable {
		if len(entries) == 0 {
			t.Fatalf("table %q left behind an empty index slice", table)
		}
		indexed += len(entries)
		for i, entry := range entries {
			if entry.extent.TableID != table {
				t.Fatalf("%s is indexed under table %q", entry.extent, table)
			}
			if host.tablets[entry.extent.key()] != entry {
				t.Fatalf("index holds %s, which is not tracked", entry.extent)
			}
			if i > 0 && compareLowerBounds(entries[i-1].extent.prev(), entry.extent.prev()) >= 0 {
				t.Fatalf("index out of order at %d: %s then %s",
					i, entries[i-1].extent, entry.extent)
			}
		}
	}
	if indexed != len(host.tablets) {
		t.Fatalf("index holds %d entries for %d tracked tablets", indexed, len(host.tablets))
	}
}

// TestOverlapIndexMatchesFullScan is the differential test for the range
// index: over a long randomized run of assignments and releases across
// several tables, every Assign and the overlap-sensitive edge of Unassign
// must reach exactly the outcome the original full scan would have produced,
// and the index must stay consistent.
func TestOverlapIndexMatchesFullScan(t *testing.T) {
	host, _, fence := newTestHost(t)
	rng := rand.New(rand.NewSource(1))
	tables := []string{"2", "3", "!0"}

	assigned, duplicate, overlapping, released := 0, 0, 0, 0
	for step := 0; step < 3000; step++ {
		e := randomExtent(rng, tables[rng.Intn(len(tables))])

		if rng.Intn(3) == 0 {
			_, tracked := host.tablets[e.key()]
			wantOverlap := anyOverlapByScan(host, e)
			if tracked {
				released++
			}
			_, err := host.Unassign(fence, e, UnloadImmediate)
			switch {
			case tracked:
				if err != nil {
					t.Fatalf("step %d: Unassign(%s): %v", step, e, err)
				}
			case wantOverlap:
				if !errors.Is(err, ErrOverlapping) {
					t.Fatalf("step %d: Unassign(%s): want ErrOverlapping, got %v", step, e, err)
				}
			default:
				if err != nil {
					t.Fatalf("step %d: Unassign(%s): %v", step, e, err)
				}
			}
			checkIndex(t, host)
			continue
		}

		_, wantDuplicate := host.tablets[e.key()]
		wantOverlap := anyOverlapByScan(host, e)
		_, err := host.Assign(fence, e)
		switch {
		case wantDuplicate:
			duplicate++
			if !errors.Is(err, ErrAlreadyAssigned) {
				t.Fatalf("step %d: Assign(%s): want ErrAlreadyAssigned, got %v", step, e, err)
			}
		case wantOverlap:
			overlapping++
			if !errors.Is(err, ErrOverlapping) {
				t.Fatalf("step %d: Assign(%s): want ErrOverlapping, got %v", step, e, err)
			}
		default:
			assigned++
			if err != nil {
				t.Fatalf("step %d: Assign(%s): %v", step, e, err)
			}
		}
		checkIndex(t, host)
	}

	// Guard against the walk degenerating into one uninteresting outcome.
	if assigned == 0 || duplicate == 0 || overlapping == 0 || released == 0 {
		t.Fatalf("walk did not exercise every outcome: assigned=%d duplicate=%d overlapping=%d released=%d",
			assigned, duplicate, overlapping, released)
	}
}
