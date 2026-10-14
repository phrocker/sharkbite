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
	"context"
	"errors"
	"fmt"
	"path"
	"time"

	gozk "github.com/go-zookeeper/zk"
)

// zManagerLock is the manager's lock directory under the instance root. It
// mirrors Constants.ZMANAGER_LOCK, and is the same path internal/zk reads to
// resolve the manager's Thrift address.
const zManagerLock = "managers/lock"

// ErrNoManagerLock means the manager lock directory holds no lock: there is
// currently no manager. It is a fact about the cluster, not a read failure —
// the two are kept apart because only the first is grounds for withdrawing a
// manager's authority.
var ErrNoManagerLock = errors.New("tserver: no manager lock held")

// ManagerLockReader is the ZooKeeper read surface needed to see which manager
// holds the manager lock. *internal/zk.Locator satisfies it, so the component
// that resolves tablet locations is the one that observes manager authority.
//
// An implementation is read once per poll interval for as long as the process
// runs, so it has to serve those reads from a session it keeps. One that
// connects per read turns a steady observation into steady session churn:
// every tablet server in the fleet would open, authenticate and close a
// ZooKeeper session on every interval, which is a connect-and-auth round trip
// per server per interval and counts against the ensemble's per-client
// connection limit.
//
// internal/zk.Locator does that today when the context can be cancelled — it
// takes a scoped connection per read and closes it — and reuses its own
// session only when the context cannot be. Neither half is usable here as it
// stands: a poll that cannot be cancelled cannot be shut down. Wiring this
// into a process therefore needs a reader that serves cancellable reads from
// one session; see the note in docs/tserver-hosting-lifecycle.md.
//
// Reads are not assumed to be monotonic across calls. ZooKeeper promises a
// client its own reads never go backwards within a session, but that is a
// promise about a session, and an implementation is free to use more than one:
// internal/zk.Locator opens a scoped connection per read when the context can
// be cancelled, so consecutive readings here can come from different sessions,
// and a newer one may land on a server that has not caught up. What that costs
// is described on WatchManagerLock, which is where a reading that appears to
// move backwards is handled.
type ManagerLockReader interface {
	InstancePath() string
	Children(ctx context.Context, path string) ([]string, error)
}

// ReadManagerLock returns the identity of the manager that currently holds the
// manager ServiceLock.
//
// The holder is the lowest-sequence node in the lock directory, the rule
// Accumulo applies in ServiceLock.validateAndSort. Queued candidates — the
// standby managers waiting to take over — are not authority and are ignored.
//
// A node counts here only when its UUID is the canonical 36-character form,
// which is what Java's UUID.fromString takes and therefore what Accumulo ever
// writes. internal/zk, which resolves the manager's Thrift address from this
// same directory, is looser: it counts any spelling uuid.Parse accepts, the
// undashed and URN forms among them. On a directory Accumulo wrote the two
// always agree. On one holding a node in a spelling only internal/zk counts,
// numbered below the real holder, they would not — this side would observe the
// manager Accumulo calls the holder while internal/zk resolved an address that
// is not one. Following Accumulo is the right side of that divergence, but the
// two readers should share one parser, and that is a change to internal/zk
// rather than a second opinion here.
//
// Returns ErrNoManagerLock when the directory is missing or holds no valid
// lock node.
func ReadManagerLock(ctx context.Context, reader ManagerLockReader) (LockID, error) {
	if reader == nil {
		return LockID{}, errors.New("tserver: nil manager lock reader")
	}
	if err := ctx.Err(); err != nil {
		return LockID{}, err
	}
	lockPath := path.Join(reader.InstancePath(), zManagerLock)
	children, err := reader.Children(ctx, lockPath)
	if err != nil {
		if errors.Is(err, gozk.ErrNoNode) {
			return LockID{}, ErrNoManagerLock
		}
		return LockID{}, fmt.Errorf("list manager locks %s: %w", lockPath, err)
	}
	sorted := sortLockNodes(children)
	if len(sorted) == 0 {
		return LockID{}, ErrNoManagerLock
	}
	id, ok := ParseLockNode(sorted[0])
	if !ok {
		// sortLockNodes only returns parseable names.
		return LockID{}, fmt.Errorf("%w: %q in %s", ErrInvalidLock, sorted[0], lockPath)
	}
	return id, nil
}

// WatchManagerLock keeps a Host's view of live manager authority in step with
// ZooKeeper until ctx ends, polling every interval.
//
// It reads exactly once per interval, so the load a fleet puts on ZooKeeper is
// one read per server per interval and nothing hidden behind it. What that
// read costs is the reader's: see ManagerLockReader on why it has to be one
// that keeps a session.
//
// This is what makes manager-directed transitions possible at all: Host
// refuses every one of them until it has been told which manager is live, and
// it decides authority from this observation rather than from the requests it
// receives. A manager cannot talk its way into authority it does not hold.
//
// A read that fails leaves the previous observation in place. Failing to read
// ZooKeeper is not evidence that the manager changed, and withdrawing
// authority on a transient failure would refuse the live manager's assignments
// for no reason. A directory that is readable and holds no lock is evidence,
// and clears the observation.
//
// An observation the host refuses — a live holder whose epoch is older than
// one already seen — is handed to refused, which may be nil, and the watch
// goes on polling. The error wraps ErrLockNotNewer.
//
// It is reported rather than acted on because nothing here can tell what
// caused it. Readings go backwards for two very different reasons: the lock
// directory was deleted and recreated, which restarts the sequence counter and
// does not heal, or the reading came from a ZooKeeper server that has not
// caught up. Readings are not monotonic across calls — a reader that opens a
// session per read can land on a lagging replica repeatedly — so no number of
// refusals separates the two. A count makes the mistake less frequent, never
// impossible, and a count that acts on the mistake is worse than one that does
// not.
//
// Safety does not depend on separating them. The host keeps the newer epoch
// whatever a reading says, so a stale one never gains authority; that holds on
// the first refusal and on the thousandth. Ending the watch would add only a
// guess with a cost: a lagging ensemble is read by every tablet server at
// once, so ending on it restarts all of them, discarding the high-water marks
// that were doing the rejecting. A recreated directory does need the fresh
// Host that AdoptLock's refusal already calls for, but that call belongs to
// the supervisor, which can establish the cause. This package reports what it
// saw.
//
// The report is made once per run of refusals rather than once per poll, since
// a condition that persists is one condition. A single refusal is not reported
// at all: one stale reading is ordinary, and the host has already refused it.
//
// Only a reading the host accepts ends a run. A poll that could not be read
// leaves it where it was, because it is not evidence of anything: treating it
// as a clean reading would let refusals interleaved with unreadable polls go
// unreported.
func WatchManagerLock(
	ctx context.Context,
	reader ManagerLockReader,
	host *Host,
	interval time.Duration,
	refused func(error),
) error {
	if reader == nil {
		return errors.New("tserver: nil manager lock reader")
	}
	if host == nil {
		return errors.New("tserver: nil host")
	}
	if interval <= 0 {
		return fmt.Errorf("tserver: manager lock poll interval %s must be positive", interval)
	}
	timer := time.NewTimer(interval)
	defer timer.Stop()
	// Consecutive refusals, counted across polls that read nothing and ended
	// by any reading the host accepts, so this measures one condition rather
	// than refusals in total. reported keeps the run to a single report.
	run := 0
	reported := false
	for {
		observed, err := observeManagerLockOnce(ctx, reader, host)
		switch {
		case err != nil:
			run++
			if run > 1 && !reported {
				reported = true
				if refused != nil {
					refused(err)
				}
			}
		case observed:
			run = 0
			reported = false
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		timer.Reset(interval)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-timer.C:
		}
	}
}

// observeManagerLockOnce applies one reading of the manager lock to the host.
//
// It reports whether the host was given a reading at all — false when the
// directory could not be read, which leaves the previous observation standing
// — and returns an error only when the host refuses one, the single outcome
// another poll cannot change.
func observeManagerLockOnce(ctx context.Context, reader ManagerLockReader, host *Host) (bool, error) {
	id, err := ReadManagerLock(ctx, reader)
	switch {
	case err == nil:
		if err := host.ObserveManagerLock(id); err != nil {
			return false, fmt.Errorf("observe manager lock %s: %w",
				path.Join(reader.InstancePath(), zManagerLock), err)
		}
	case errors.Is(err, ErrNoManagerLock):
		// The zero LockID clears the observation and is never refused.
		_ = host.ObserveManagerLock(LockID{})
	default:
		// Unreadable is not the same as absent: hold the last observation.
		return false, nil
	}
	return true, nil
}
