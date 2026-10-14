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
	"sync"
	"testing"
	"time"

	gozk "github.com/go-zookeeper/zk"
)

// fakeManagerReader stands in for the ZooKeeper reader that watches the
// manager lock directory. Readings are scripted so a test can walk a host
// through a manager failover, a transient read failure, and a manager going
// away entirely.
type fakeManagerReader struct {
	mu       sync.Mutex
	children []string
	err      error
	paths    []string
	reads    int
	// script replaces the reading after each call, so successive polls can
	// see different things.
	script []func(*fakeManagerReader)
}

func (r *fakeManagerReader) InstancePath() string { return testInstancePath }

func (r *fakeManagerReader) Children(ctx context.Context, path string) ([]string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	r.paths = append(r.paths, path)
	r.reads++
	children, err := append([]string(nil), r.children...), r.err
	if len(r.script) > 0 {
		next := r.script[0]
		r.script = r.script[1:]
		next(r)
	}
	return children, err
}

func (r *fakeManagerReader) set(children []string, err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.children, r.err = children, err
}

func (r *fakeManagerReader) readCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.reads
}

func (r *fakeManagerReader) readPaths() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.paths...)
}

func managerNode(holder string, sequence int) string {
	return fmt.Sprintf("zlock#%s#%010d", holder, sequence)
}

// refusalLog collects what WatchManagerLock reports, so a test can tell a
// refusal that was reported from one that was swallowed.
type refusalLog struct {
	mu   sync.Mutex
	errs []error
}

func (l *refusalLog) record(err error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.errs = append(l.errs, err)
}

func (l *refusalLog) count() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.errs)
}

func (l *refusalLog) first() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.errs) == 0 {
		return nil
	}
	return l.errs[0]
}

// TestReadManagerLockTakesTheLowestNode is the authority rule. Standby
// managers queue in the same directory as the live one, so reading anything
// but the lowest sequence would treat a manager that has not taken over as if
// it had.
func TestReadManagerLockTakesTheLowestNode(t *testing.T) {
	reader := &fakeManagerReader{children: []string{
		managerNode(otherUUID, 7),
		managerNode(managerUUID, 3),
		managerNode(serverUUID, 11),
	}}
	id, err := ReadManagerLock(context.Background(), reader)
	if err != nil {
		t.Fatalf("ReadManagerLock: %v", err)
	}
	want := LockID{UUID: managerUUID, Sequence: 3}
	if id != want {
		t.Fatalf("live manager = %s, want %s", id, want)
	}
	paths := reader.readPaths()
	if len(paths) != 1 || paths[0] != testInstancePath+"/managers/lock" {
		t.Fatalf("read %v, want the Accumulo manager lock directory", paths)
	}
}

func TestReadManagerLockIgnoresStrangers(t *testing.T) {
	reader := &fakeManagerReader{children: []string{
		"zlock#not-a-uuid#0000000000",
		"README",
		managerNode(managerUUID, 4),
	}}
	id, err := ReadManagerLock(context.Background(), reader)
	if err != nil {
		t.Fatalf("ReadManagerLock: %v", err)
	}
	if want := (LockID{UUID: managerUUID, Sequence: 4}); id != want {
		t.Fatalf("live manager = %s, want %s", id, want)
	}
}

// TestReadManagerLockReportsAnAbsentManager keeps "there is no manager" apart
// from "I could not tell", because only the first is grounds for withdrawing
// a manager's authority.
func TestReadManagerLockReportsAnAbsentManager(t *testing.T) {
	for _, tt := range []struct {
		name     string
		children []string
		err      error
	}{
		{"no lock directory", nil, gozk.ErrNoNode},
		{"empty directory", nil, nil},
		{"nothing that parses", []string{"README", "zlock#bad#0000000000"}, nil},
	} {
		t.Run(tt.name, func(t *testing.T) {
			reader := &fakeManagerReader{children: tt.children, err: tt.err}
			if _, err := ReadManagerLock(context.Background(), reader); !errors.Is(err, ErrNoManagerLock) {
				t.Fatalf("ReadManagerLock: want ErrNoManagerLock, got %v", err)
			}
		})
	}
}

func TestReadManagerLockPropagatesReadFailures(t *testing.T) {
	reader := &fakeManagerReader{err: gozk.ErrConnectionClosed}
	_, err := ReadManagerLock(context.Background(), reader)
	if !errors.Is(err, gozk.ErrConnectionClosed) {
		t.Fatalf("ReadManagerLock: want the ZooKeeper error, got %v", err)
	}
	if errors.Is(err, ErrNoManagerLock) {
		t.Fatal("an unreadable directory must not be reported as an absent manager")
	}
}

func TestReadManagerLockRefusesBadInput(t *testing.T) {
	if _, err := ReadManagerLock(context.Background(), nil); err == nil {
		t.Fatal("ReadManagerLock accepted a nil reader")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	reader := &fakeManagerReader{children: []string{managerNode(managerUUID, 1)}}
	if _, err := ReadManagerLock(ctx, reader); !errors.Is(err, context.Canceled) {
		t.Fatalf("ReadManagerLock: want context.Canceled, got %v", err)
	}
	if reader.readCount() != 0 {
		t.Fatal("a cancelled read still went to ZooKeeper")
	}
}

// TestManagerAuthorityIsObservedNotAsserted is the whole reason this observer
// exists. A manager cannot talk its way into authority: until the lock has
// been read from ZooKeeper, every manager-directed request fails closed, and
// what makes them start working is the observation, not the request.
func TestManagerAuthorityIsObservedNotAsserted(t *testing.T) {
	host := NewHost()
	if err := host.AdoptLock(serverLock(1)); err != nil {
		t.Fatalf("AdoptLock: %v", err)
	}
	fence := Fence{Server: serverLock(1), Manager: managerLock(4)}
	extent := Extent{TableID: "2", EndRow: []byte("m")}

	if _, err := host.Assign(fence, extent); !errors.Is(err, ErrStaleManagerLock) {
		t.Fatalf("Assign before observing a manager: want ErrStaleManagerLock, got %v", err)
	}

	reader := &fakeManagerReader{children: []string{managerNode(managerUUID, 4)}}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	watch := make(chan error, 1)
	go func() { watch <- WatchManagerLock(ctx, reader, host, time.Millisecond, nil) }()

	waitFor(t, "the manager lock to be observed", func() bool {
		observed, ok := host.ManagerLock()
		return ok && observed == managerLock(4)
	})
	if _, err := host.Assign(fence, extent); err != nil {
		t.Fatalf("Assign after observing the live manager: %v", err)
	}

	cancel()
	if err := <-watch; !errors.Is(err, context.Canceled) {
		t.Fatalf("WatchManagerLock: want context.Canceled, got %v", err)
	}
}

// TestWatchManagerLockFollowsAFailover checks the observer moves authority
// forward when the live manager changes.
func TestWatchManagerLockFollowsAFailover(t *testing.T) {
	host := NewHost()
	reader := &fakeManagerReader{children: []string{managerNode(managerUUID, 4)}}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = WatchManagerLock(ctx, reader, host, time.Millisecond, nil) }()

	waitFor(t, "the first manager", func() bool {
		observed, ok := host.ManagerLock()
		return ok && observed == managerLock(4)
	})

	// The live manager died and the standby took over with a later node.
	reader.set([]string{managerNode(otherUUID, 9)}, nil)
	waitFor(t, "the failover", func() bool {
		observed, ok := host.ManagerLock()
		return ok && observed == LockID{UUID: otherUUID, Sequence: 9}
	})
}

// TestWatchManagerLockKeepsAuthorityThroughAReadFailure is the difference
// between not knowing and knowing there is nobody. Dropping the live manager
// because ZooKeeper was briefly unreachable would refuse that manager's
// assignments for no reason.
func TestWatchManagerLockKeepsAuthorityThroughAReadFailure(t *testing.T) {
	host := NewHost()
	reader := &fakeManagerReader{children: []string{managerNode(managerUUID, 4)}}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = WatchManagerLock(ctx, reader, host, time.Millisecond, nil) }()

	waitFor(t, "the manager lock to be observed", func() bool {
		_, ok := host.ManagerLock()
		return ok
	})

	reader.set(nil, gozk.ErrConnectionClosed)
	before := reader.readCount()
	waitFor(t, "several failed reads", func() bool { return reader.readCount() > before+3 })

	observed, ok := host.ManagerLock()
	if !ok || observed != managerLock(4) {
		t.Fatalf("manager authority = %s, %v after a read failure; want it retained", observed, ok)
	}
}

// TestWatchManagerLockClearsAuthorityWhenTheManagerIsGone is the other half:
// a directory that is readable and holds no lock is evidence there is no
// manager, and manager-directed work stops until one appears.
func TestWatchManagerLockClearsAuthorityWhenTheManagerIsGone(t *testing.T) {
	host := NewHost()
	if err := host.AdoptLock(serverLock(1)); err != nil {
		t.Fatalf("AdoptLock: %v", err)
	}
	reader := &fakeManagerReader{children: []string{managerNode(managerUUID, 4)}}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = WatchManagerLock(ctx, reader, host, time.Millisecond, nil) }()

	waitFor(t, "the manager lock to be observed", func() bool {
		_, ok := host.ManagerLock()
		return ok
	})

	reader.set(nil, nil)
	waitFor(t, "the manager to be forgotten", func() bool {
		_, ok := host.ManagerLock()
		return !ok
	})

	fence := Fence{Server: serverLock(1), Manager: managerLock(4)}
	if _, err := host.Assign(fence, Extent{TableID: "2", EndRow: []byte("m")}); !errors.Is(err, ErrStaleManagerLock) {
		t.Fatalf("Assign with no live manager: want ErrStaleManagerLock, got %v", err)
	}
}

// TestWatchManagerLockSurfacesARefusedEpoch covers a reading no later poll
// resolves. ZooKeeper hands out sequence numbers from a counter on the parent,
// so a manager lock directory that was deleted and remade starts again at
// zero: the manager holding it is live, and its epoch is older than the one
// this host has already seen. Refusing it is right — authority does not move
// backwards — and swallowing it is not, because the host answers to a manager
// that is gone in the meantime and nothing else would say so.
func TestWatchManagerLockSurfacesARefusedEpoch(t *testing.T) {
	host := NewHost()
	reader := &fakeManagerReader{children: []string{managerNode(otherUUID, 9)}}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	watch := make(chan error, 1)
	reports := &refusalLog{}
	go func() { watch <- WatchManagerLock(ctx, reader, host, time.Millisecond, reports.record) }()

	waitFor(t, "the manager lock to be observed", func() bool {
		_, ok := host.ManagerLock()
		return ok
	})

	reader.set([]string{managerNode(managerUUID, 2)}, nil)
	waitFor(t, "the refusal to be reported", func() bool { return reports.count() > 0 })
	if err := reports.first(); !errors.Is(err, ErrLockNotNewer) {
		t.Fatalf("reported %v, want ErrLockNotNewer", err)
	}

	// The watch is still running: the report is a report, not a verdict.
	select {
	case err := <-watch:
		t.Fatalf("WatchManagerLock ended on a refusal it cannot explain: %v", err)
	default:
	}

	// And nothing moved backwards.
	observed, ok := host.ManagerLock()
	if !ok || observed != (LockID{UUID: otherUUID, Sequence: 9}) {
		t.Fatalf("manager authority = %s, %v; want the newer epoch retained", observed, ok)
	}

	// The recovery a recreated directory needs still works, and is the
	// supervisor's to start: a host with no epoch history takes the live
	// manager on its first reading.
	fresh := NewHost()
	go func() { _ = WatchManagerLock(ctx, reader, fresh, time.Millisecond, nil) }()
	waitFor(t, "a fresh host to take the live manager", func() bool {
		seen, ok := fresh.ManagerLock()
		return ok && seen == managerLock(2)
	})
}

// TestWatchManagerLockOutlivesALaggingReplica is why a refusal is reported
// rather than acted on. A reader that opens a session per read can land on a
// ZooKeeper server that has not caught up, and can do it on consecutive polls,
// so a run of refusals is not evidence the directory was recreated. Ending the
// watch on one would restart a healthy host — and, since every tablet server
// reads the same lagging ensemble, restart the fleet — throwing away the
// high-water marks that were rejecting the stale readings. The observer has to
// still be there when the replica catches up.
func TestWatchManagerLockOutlivesALaggingReplica(t *testing.T) {
	host := NewHost()
	reader := &fakeManagerReader{children: []string{managerNode(otherUUID, 9)}}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	watch := make(chan error, 1)
	reports := &refusalLog{}
	go func() { watch <- WatchManagerLock(ctx, reader, host, time.Millisecond, reports.record) }()
	waitFor(t, "the manager lock to be observed", func() bool {
		_, ok := host.ManagerLock()
		return ok
	})

	// A replica that is behind, for as many polls as it takes.
	reader.set([]string{managerNode(managerUUID, 2)}, nil)
	waitFor(t, "the refusal to be reported", func() bool { return reports.count() > 0 })
	before := reader.readCount()
	waitFor(t, "the lag to persist across many polls", func() bool { return reader.readCount() > before+20 })

	// It catches up, and the manager has failed over in the meantime.
	reader.set([]string{managerNode(managerUUID, 12)}, nil)
	waitFor(t, "authority to follow the caught-up reading", func() bool {
		seen, ok := host.ManagerLock()
		return ok && seen == managerLock(12)
	})
	select {
	case err := <-watch:
		t.Fatalf("WatchManagerLock ended before the replica caught up: %v", err)
	default:
	}
	if got := reports.count(); got != 1 {
		t.Fatalf("reported %d refusals, want exactly 1 for one run of them", got)
	}
}

// TestWatchManagerLockReportsEachRunOfRefusalsOnce pins the reporting rule. A
// condition that persists is one condition, so it is reported once however
// many polls it spans; a reading the host accepts ends the run, and a refusal
// after that is a new one.
func TestWatchManagerLockReportsEachRunOfRefusalsOnce(t *testing.T) {
	host := NewHost()
	reader := &fakeManagerReader{children: []string{managerNode(otherUUID, 9)}}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	reports := &refusalLog{}
	go func() { _ = WatchManagerLock(ctx, reader, host, time.Millisecond, reports.record) }()
	waitFor(t, "the manager lock to be observed", func() bool {
		_, ok := host.ManagerLock()
		return ok
	})

	reader.set([]string{managerNode(managerUUID, 2)}, nil)
	waitFor(t, "the first run to be reported", func() bool { return reports.count() == 1 })
	before := reader.readCount()
	waitFor(t, "the run to span several polls", func() bool { return reader.readCount() > before+10 })
	if got := reports.count(); got != 1 {
		t.Fatalf("reported %d times during one run, want 1", got)
	}

	// An accepted reading ends the run. Authority cannot show that — it never
	// moved — so wait for polls to have consumed the reading instead: one may
	// have been in flight when it was set, so two of them.
	accepted := reader.readCount()
	reader.set([]string{managerNode(otherUUID, 9)}, nil)
	waitFor(t, "the accepted reading to be polled", func() bool { return reader.readCount() >= accepted+2 })

	// So the next refusal is a new condition and reported again.
	reader.set([]string{managerNode(managerUUID, 2)}, nil)
	waitFor(t, "the second run to be reported", func() bool { return reports.count() == 2 })
}

// TestWatchManagerLockReportsNowhereWithoutACallback keeps the report
// optional: a caller that does not want it should not have to invent one, and
// must not get a nil call for its trouble.
func TestWatchManagerLockReportsNowhereWithoutACallback(t *testing.T) {
	host := NewHost()
	reader := &fakeManagerReader{children: []string{managerNode(otherUUID, 9)}}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	watch := make(chan error, 1)
	go func() { watch <- WatchManagerLock(ctx, reader, host, time.Millisecond, nil) }()
	waitFor(t, "the manager lock to be observed", func() bool {
		_, ok := host.ManagerLock()
		return ok
	})

	reader.set([]string{managerNode(managerUUID, 2)}, nil)
	before := reader.readCount()
	waitFor(t, "the refusals to keep coming", func() bool { return reader.readCount() > before+10 })
	select {
	case err := <-watch:
		t.Fatalf("WatchManagerLock ended without a callback to report to: %v", err)
	default:
	}
}

// TestWatchManagerLockReadsOncePerInterval bounds what a fleet costs the
// ensemble. Every tablet server runs one of these, and a reader may pay for a
// connection and an authentication per read, so the interval has to be the
// whole story: a second read per pass, or a wait that spins, multiplies that
// cost by the size of the fleet.
func TestWatchManagerLockReadsOncePerInterval(t *testing.T) {
	const interval = 25 * time.Millisecond
	host := NewHost()
	reader := &fakeManagerReader{children: []string{managerNode(managerUUID, 2)}}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = WatchManagerLock(ctx, reader, host, interval, nil) }()
	waitFor(t, "the watch to start polling", func() bool { return reader.readCount() >= 2 })

	start, before := time.Now(), reader.readCount()
	time.Sleep(20 * interval)
	elapsed, reads := time.Since(start), reader.readCount()-before
	// Measured against the time that actually passed, so a machine too busy to
	// keep the interval reads fewer rather than failing. Two are allowed on
	// top: one in flight at each end of the window.
	if most := int(elapsed/interval) + 2; reads > most {
		t.Fatalf("%d reads in %s at a %s interval, want at most %d", reads, elapsed, interval, most)
	}
}

// TestWatchManagerLockRidesOutAReadingThatWentBackwards is the other side of
// the test above. Readings are not monotonic across calls — a reader that
// opens a session per read can land on a ZooKeeper server that has not caught
// up — so a single reading that appears to move the manager backwards is not
// evidence of anything worth reporting. Reporting it would cost an operator a
// recreated-directory investigation for a replica that was a moment behind.
func TestWatchManagerLockRidesOutAReadingThatWentBackwards(t *testing.T) {
	host := NewHost()
	reader := &fakeManagerReader{
		children: []string{managerNode(otherUUID, 9)},
		script: []func(*fakeManagerReader){
			// The second reading is the stale one; every one after it sees the
			// live manager again, as a caught-up replica would.
			func(r *fakeManagerReader) { r.children = []string{managerNode(managerUUID, 2)} },
			func(r *fakeManagerReader) { r.children = []string{managerNode(otherUUID, 9)} },
		},
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	watch := make(chan error, 1)
	reports := &refusalLog{}
	go func() { watch <- WatchManagerLock(ctx, reader, host, time.Millisecond, reports.record) }()

	waitFor(t, "the watch to poll past the stale reading", func() bool { return reader.readCount() >= 6 })
	select {
	case err := <-watch:
		t.Fatalf("WatchManagerLock ended on one stale reading: %v", err)
	default:
	}
	if got := reports.count(); got != 0 {
		t.Fatalf("reported %d refusals for one stale reading, want none", got)
	}
	// The stale reading was refused rather than believed, both times it could
	// have been: authority is still the epoch that was already seen.
	observed, ok := host.ManagerLock()
	if !ok || observed != (LockID{UUID: otherUUID, Sequence: 9}) {
		t.Fatalf("manager authority = %s, %v; want the epoch already observed", observed, ok)
	}

	cancel()
	select {
	case err := <-watch:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("WatchManagerLock: want context.Canceled, got %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("WatchManagerLock did not stop when cancelled")
	}
}

// TestWatchManagerLockKeepsARefusalAcrossAnUnreadablePoll pins what clears the
// run of refusals. A poll that could not be read is not evidence of
// anything: the previous observation stands and the host is told nothing. If
// it ended the run, a recreated lock directory interleaved with transient
// ZooKeeper failures would refuse every reading and never be reported, leaving
// the host fenced to a dead manager with nothing said about it.
func TestWatchManagerLockKeepsARefusalAcrossAnUnreadablePoll(t *testing.T) {
	host := NewHost()
	// Every reading after the first is the recreated directory, and every
	// other poll fails to read it at all, so no two refusals are ever
	// adjacent. A run an unreadable poll ended would never reach two.
	var alternate func(*fakeManagerReader)
	alternate = func(r *fakeManagerReader) {
		if r.err == nil {
			r.err = gozk.ErrConnectionClosed
		} else {
			r.err = nil
		}
		r.script = append(r.script, alternate)
	}
	reader := &fakeManagerReader{
		children: []string{managerNode(otherUUID, 9)},
		script: []func(*fakeManagerReader){
			func(r *fakeManagerReader) {
				r.children = []string{managerNode(managerUUID, 2)}
				r.script = append(r.script, alternate)
			},
		},
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	reports := &refusalLog{}
	go func() { _ = WatchManagerLock(ctx, reader, host, time.Millisecond, reports.record) }()

	waitFor(t, "the refusal to be reported across the unreadable polls", func() bool {
		return reports.count() > 0
	})
	if err := reports.first(); !errors.Is(err, ErrLockNotNewer) {
		t.Fatalf("reported %v, want ErrLockNotNewer", err)
	}
	observed, ok := host.ManagerLock()
	if !ok || observed != (LockID{UUID: otherUUID, Sequence: 9}) {
		t.Fatalf("manager authority = %s, %v; want the epoch already observed", observed, ok)
	}
}

// TestWatchManagerLockObservesBeforeItWaits matters at startup: a tablet
// server that waited a poll interval before its first reading would refuse the
// manager's first assignments after every restart.
func TestWatchManagerLockObservesBeforeItWaits(t *testing.T) {
	host := NewHost()
	reader := &fakeManagerReader{children: []string{managerNode(managerUUID, 4)}}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = WatchManagerLock(ctx, reader, host, time.Hour, nil) }()

	waitFor(t, "the first reading", func() bool {
		observed, ok := host.ManagerLock()
		return ok && observed == managerLock(4)
	})
}

func TestWatchManagerLockStopsWhenCancelled(t *testing.T) {
	host := NewHost()
	reader := &fakeManagerReader{children: []string{managerNode(managerUUID, 4)}}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- WatchManagerLock(ctx, reader, host, time.Millisecond, nil) }()

	waitFor(t, "the manager lock to be observed", func() bool {
		_, ok := host.ManagerLock()
		return ok
	})
	cancel()

	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("WatchManagerLock: want context.Canceled, got %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("WatchManagerLock did not stop when cancelled")
	}

	settled := reader.readCount()
	time.Sleep(20 * time.Millisecond)
	if again := reader.readCount(); again != settled {
		t.Fatalf("the observer kept polling after it returned (%d then %d)", settled, again)
	}
}

func TestWatchManagerLockValidatesArguments(t *testing.T) {
	host := NewHost()
	reader := &fakeManagerReader{}
	if err := WatchManagerLock(context.Background(), nil, host, time.Second, nil); err == nil {
		t.Fatal("WatchManagerLock accepted a nil reader")
	}
	if err := WatchManagerLock(context.Background(), reader, nil, time.Second, nil); err == nil {
		t.Fatal("WatchManagerLock accepted a nil host")
	}
	for _, interval := range []time.Duration{0, -time.Second} {
		if err := WatchManagerLock(context.Background(), reader, host, interval, nil); err == nil {
			t.Fatalf("WatchManagerLock accepted a %s poll interval", interval)
		}
	}
	if reader.readCount() != 0 {
		t.Fatal("a refused watch still read ZooKeeper")
	}
}
