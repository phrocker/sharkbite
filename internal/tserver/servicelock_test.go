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
	"reflect"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	gozk "github.com/go-zookeeper/zk"
)

// testLockData is the payload a tablet server publishes in these tests,
// announced by the lock that will publish it. Acquire refuses a payload naming
// anyone else, which is the rule that keeps an advertisement tied to the
// generation it was made under.
func testLockData(t *testing.T, lock *ServiceLock) ServiceLockData {
	t.Helper()
	return testLockDataFor(t, lock.UUID())
}

// testLockDataFor is the same payload announced by a named server, for the
// tests that need to name one the lock does not.
func testLockDataFor(t *testing.T, holder string) ServiceLockData {
	t.Helper()
	data, err := TabletServerLockData(holder, testAddress, testGroup, TabletServerServices()...)
	if err != nil {
		t.Fatalf("TabletServerLockData: %v", err)
	}
	return data
}

// newTestLock builds a lock over conn. Its UUID is minted by NewServiceLock,
// as it is in production, so a test that needs the name of a node has to ask
// the lock rather than assume one.
func newTestLock(t *testing.T, conn LockConn) *ServiceLock {
	t.Helper()
	lock, err := NewServiceLock(conn, ServiceLockOptions{Path: testLockPath()})
	if err != nil {
		t.Fatalf("NewServiceLock: %v", err)
	}
	return lock
}

// nodesUnder returns the lock nodes carrying prefix, which is how a test reads
// the place in line of a candidate that has not acquired yet — Node() reports
// only a node that is held.
func nodesUnder(f *fakeZK, prefix string) []string {
	var found []string
	for _, node := range f.lockNodes(testLockPath()) {
		if strings.HasPrefix(node, prefix) {
			found = append(found, node)
		}
	}
	return found
}

// nextArmed returns the very next watch the code under test establishes,
// which is how a test asserts *which* node is being watched rather than only
// that something is.
func nextArmed(t *testing.T, f *fakeZK) string {
	t.Helper()
	select {
	case armed := <-f.armed:
		return armed
	case <-time.After(5 * time.Second):
		t.Fatal("no watch was established")
		return ""
	}
}

// TestAcquireCreatesAnAccumuloCompatibleLockNode is the registration contract.
// An unmodified manager finds live tablet servers by listing this directory,
// picking the lowest zlock node and reading its payload, so the node name, the
// ephemeral flag, the ACL and the bytes all have to match what a Java tablet
// server would have written.
func TestAcquireCreatesAnAccumuloCompatibleLockNode(t *testing.T) {
	f := newFakeZK()
	lock := newTestLock(t, f)
	data := testLockData(t, lock)

	id, err := lock.Acquire(context.Background(), data)
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	want := LockID{UUID: lock.UUID(), Sequence: 0}
	if id != want {
		t.Fatalf("acquired %s, want %s", id, want)
	}
	node := lock.Node()
	if node != fmt.Sprintf("zlock#%s#%010d", lock.UUID(), 0) {
		t.Fatalf("lock node %q is not Accumulo's zlock#<uuid>#<sequence>", node)
	}
	held, ok := lock.LockID()
	if !ok || held != want {
		t.Fatalf("LockID() = %s, %v; want %s, true", held, ok, want)
	}
	if lock.LossReason() != LossNone {
		t.Fatalf("a held lock reports loss %s", lock.LossReason())
	}

	stored, ok := f.node(path.Join(testLockPath(), node))
	if !ok {
		t.Fatal("the lock node was not created")
	}
	if !stored.ephemeral {
		t.Fatal("the lock node must be ephemeral, or a crashed server keeps its lock forever")
	}
	payload, err := data.Encode()
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	if string(stored.data) != string(payload) {
		t.Fatalf("lock node payload = %s, want %s", stored.data, payload)
	}
	if !reflect.DeepEqual(stored.acl, PublicACL()) {
		t.Fatalf("lock node ACL = %+v, want ZooUtil.PUBLIC %+v", stored.acl, PublicACL())
	}
}

// TestAcquireCreatesTheResourceGroupPath covers a tablet server starting in a
// resource group no server has used yet, which Accumulo handles in
// ServiceLockSupport.createNonHaServiceLockPath.
func TestAcquireCreatesTheResourceGroupPath(t *testing.T) {
	f := newFakeZK()
	lock := newTestLock(t, f)
	if _, err := lock.Acquire(context.Background(), testLockData(t, lock)); err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	for _, required := range []string{
		path.Join(testInstancePath, "tservers"),
		path.Join(testInstancePath, "tservers", testGroup),
		testLockPath(),
	} {
		if !f.exists(required) {
			t.Fatalf("%s was not created", required)
		}
	}
}

// TestAcquireWaitsForTheHolderToLeave is the queueing contract: a second
// process with the same address does not take over a live server's lock, it
// waits behind it. This is what stops two processes believing they host the
// same tablets.
func TestAcquireWaitsForTheHolderToLeave(t *testing.T) {
	f := newFakeZK()
	holder := f.seedForeignLock(testLockPath(), otherUUID, 0)
	lock := newTestLock(t, f)

	type result struct {
		id  LockID
		err error
	}
	done := make(chan result, 1)
	go func() {
		id, err := lock.Acquire(context.Background(), testLockData(t, lock))
		done <- result{id, err}
	}()

	waitArmed(t, f, path.Join(testLockPath(), holder))
	select {
	case got := <-done:
		t.Fatalf("Acquire returned %s/%v while another process held the lock", got.id, got.err)
	default:
	}

	if err := f.Delete(path.Join(testLockPath(), holder), -1); err != nil {
		t.Fatalf("delete the holder's node: %v", err)
	}
	got := <-done
	if got.err != nil {
		t.Fatalf("Acquire after the holder left: %v", got.err)
	}
	if got.id.Sequence != 1 {
		t.Fatalf("acquired sequence %d, want 1", got.id.Sequence)
	}
	if !got.id.Supersedes(LockID{UUID: otherUUID, Sequence: 0}) {
		t.Fatalf("%s must supersede the generation it replaced", got.id)
	}
}

// TestAcquireWatchesTheHoldersLowestNode pins which node a queued candidate
// watches. A holder that retried a create owns several nodes; watching the
// immediate predecessor would wake this process when that holder tidies up a
// duplicate rather than when it actually leaves, and it would then find the
// lock still taken.
//
// It also pins the order: a queued candidate watches its own node first, so a
// node that disappears from under it is noticed without waiting on the holder.
func TestAcquireWatchesTheHoldersLowestNode(t *testing.T) {
	f := newFakeZK()
	lowest := f.seedForeignLock(testLockPath(), otherUUID, 0)
	predecessor := f.seedForeignLock(testLockPath(), otherUUID, 1)
	lock := newTestLock(t, f)

	go func() {
		_, _ = lock.Acquire(context.Background(), testLockData(t, lock))
	}()

	own := nextArmed(t, f)
	if !strings.HasPrefix(path.Base(own), lock.nodePrefix()) {
		t.Fatalf("first watch is on %s, want a node of this process (prefix %s)",
			own, lock.nodePrefix())
	}
	armed := nextArmed(t, f)
	if armed != path.Join(testLockPath(), lowest) {
		t.Fatalf("watching %s, want the holder's lowest node %s (its highest is %s)",
			armed, path.Join(testLockPath(), lowest), predecessor)
	}
}

// TestAcquireCollapsesDuplicateNodes covers a create whose answer was lost and
// retried. Every attempt left a node, and all of them carry this process's
// prefix; keeping more than one would hold several places in the queue and
// leave a stale node behind after the lock is released.
func TestAcquireCollapsesDuplicateNodes(t *testing.T) {
	f := newFakeZK()
	f.duplicates = 2
	lock := newTestLock(t, f)

	id, err := lock.Acquire(context.Background(), testLockData(t, lock))
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	if id.Sequence != 0 {
		t.Fatalf("kept sequence %d, want the first node this process created (0)", id.Sequence)
	}
	remaining := f.lockNodes(testLockPath())
	if len(remaining) != 1 {
		t.Fatalf("lock directory holds %v, want exactly one node", remaining)
	}
	if remaining[0] != lock.Node() {
		t.Fatalf("the surviving node is %s, but the lock believes it holds %s",
			remaining[0], lock.Node())
	}
}

// TestAcquireIsCancellableWhileQueued matters because a shutting-down process
// that leaves a node in the queue blocks everyone behind it until its session
// ends, which can be tens of seconds of an unhosted tablet range.
func TestAcquireIsCancellableWhileQueued(t *testing.T) {
	f := newFakeZK()
	holder := f.seedForeignLock(testLockPath(), otherUUID, 0)
	lock := newTestLock(t, f)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := lock.Acquire(ctx, testLockData(t, lock))
		done <- err
	}()

	waitArmed(t, f, path.Join(testLockPath(), holder))
	cancel()

	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("Acquire: want context.Canceled, got %v", err)
	}
	if _, ok := lock.LockID(); ok {
		t.Fatal("a cancelled acquisition must not report a held lock")
	}
	for _, node := range f.lockNodes(testLockPath()) {
		if strings.HasPrefix(node, lock.nodePrefix()) {
			t.Fatalf("cancelled acquisition left %s queued", node)
		}
	}
}

// TestAcquireRefusesToPublishUnusableLockData checks the refusal happens
// before anything is written. A znode is a claim to be a live server, so a
// payload the manager cannot act on must never reach ZooKeeper.
func TestAcquireRefusesToPublishUnusableLockData(t *testing.T) {
	f := newFakeZK()
	lock := newTestLock(t, f)

	_, err := lock.Acquire(context.Background(), ServiceLockData{})
	if !errors.Is(err, ErrInvalidLockData) {
		t.Fatalf("Acquire: want ErrInvalidLockData, got %v", err)
	}
	if created := f.createdPaths(); len(created) != 0 {
		t.Fatalf("a refused acquisition wrote %v", created)
	}
}

// TestAcquireRefusesASecondGeneration keeps one ServiceLock meaning one
// generation. Re-acquiring through the same object would give two different
// generations the same identity, and the fence is built on that identity.
func TestAcquireRefusesASecondGeneration(t *testing.T) {
	f := newFakeZK()
	lock := newTestLock(t, f)
	if _, err := lock.Acquire(context.Background(), testLockData(t, lock)); err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	if _, err := lock.Acquire(context.Background(), testLockData(t, lock)); !errors.Is(err, ErrLockInUse) {
		t.Fatalf("second Acquire: want ErrLockInUse, got %v", err)
	}
}

func TestAcquireReportsACreateFailure(t *testing.T) {
	f := newFakeZK()
	lock := newTestLock(t, f)
	f.failCreate(testLockPath()+"/zlock#"+lock.UUID()+"#", gozk.ErrNoAuth)

	_, err := lock.Acquire(context.Background(), testLockData(t, lock))
	if !errors.Is(err, gozk.ErrNoAuth) {
		t.Fatalf("Acquire: want the ZooKeeper error, got %v", err)
	}
	if _, ok := lock.LockID(); ok {
		t.Fatal("a failed acquisition must not report a held lock")
	}
}

// TestAcquireExplainsAMissingInstanceSecret turns the least obvious ZooKeeper
// failure into the thing an operator actually has to fix. A session that never
// authenticated cannot write under the instance root, and "not authenticated"
// on its own does not say why.
func TestAcquireExplainsAMissingInstanceSecret(t *testing.T) {
	f := newFakeZK()
	f.seed(path.Join(testInstancePath, "tservers"), nil, false)
	lock := newTestLock(t, f)
	f.failCreate(path.Join(testInstancePath, "tservers", testGroup), gozk.ErrNoAuth)

	_, err := lock.Acquire(context.Background(), testLockData(t, lock))
	if !errors.Is(err, gozk.ErrNoAuth) {
		t.Fatalf("Acquire: want ErrNoAuth, got %v", err)
	}
	if !strings.Contains(err.Error(), "instance secret") {
		t.Fatalf("error %q does not point at the instance secret", err)
	}
}

func TestAcquireReportsADirectoryFailure(t *testing.T) {
	f := newFakeZK()
	lock := newTestLock(t, f)
	f.failCreate(path.Join(testInstancePath, "tservers"), gozk.ErrConnectionClosed)

	if _, err := lock.Acquire(context.Background(), testLockData(t, lock)); !errors.Is(err, gozk.ErrConnectionClosed) {
		t.Fatalf("Acquire: want the ZooKeeper error, got %v", err)
	}
}

func TestAcquireHonoursAnAlreadyCancelledContext(t *testing.T) {
	f := newFakeZK()
	lock := newTestLock(t, f)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if _, err := lock.Acquire(ctx, testLockData(t, lock)); !errors.Is(err, context.Canceled) {
		t.Fatalf("Acquire: want context.Canceled, got %v", err)
	}
	if created := f.createdPaths(); len(created) != 0 {
		t.Fatalf("a cancelled acquisition wrote %v", created)
	}
}

// TestAcquireRereadsWhenThePredecessorVanishesBeforeTheWatch closes the window
// between listing the directory and watching what the listing found. The node
// ahead can leave in between, and an event for a node that is already gone
// never arrives — so the queue is re-read rather than waited on.
func TestAcquireRereadsWhenThePredecessorVanishesBeforeTheWatch(t *testing.T) {
	f := newFakeZK()
	holder := f.seedForeignLock(testLockPath(), otherUUID, 0)
	holderPath := path.Join(testLockPath(), holder)

	var once sync.Once
	f.beforeGet = func(watched string) {
		if watched != holderPath {
			return
		}
		once.Do(func() { _ = f.Delete(holderPath, -1) })
	}
	lock := newTestLock(t, f)

	id, err := lock.Acquire(context.Background(), testLockData(t, lock))
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	if id.Sequence != 1 {
		t.Fatalf("acquired sequence %d, want 1", id.Sequence)
	}
	// And nothing was left watching it. A node that vanished this way is a
	// sequence ZooKeeper will not hand out again, so a watch registered on it
	// could only be released by the session ending.
	if watches := f.watchCount(holderPath); watches != 0 {
		t.Fatalf("%d watches left on a node that is gone for good", watches)
	}
}

// TestQueueLeavesNoWatchOnItsOwnNodeOnceItIsGone is the same requirement on
// the other watch a queued candidate arms. Both are sequential names, and the
// one this process created is not coming back either.
func TestQueueLeavesNoWatchOnItsOwnNodeOnceItIsGone(t *testing.T) {
	f := newFakeZK()
	f.seedForeignLock(testLockPath(), otherUUID, 0)
	lock := newTestLock(t, f)

	var once sync.Once
	f.beforeGet = func(watched string) {
		if !strings.HasPrefix(path.Base(watched), lock.nodePrefix()) {
			return
		}
		once.Do(func() { _ = f.Delete(watched, -1) })
	}

	_, err := lock.Acquire(context.Background(), testLockData(t, lock))
	if !errors.Is(err, ErrLockNodeMissing) {
		t.Fatalf("Acquire = %v, want ErrLockNodeMissing", err)
	}
	own := path.Join(testLockPath(), fmt.Sprintf("%s%010d", lock.nodePrefix(), 1))
	if watches := f.watchCount(own); watches != 0 {
		t.Fatalf("%d watches left on %s, which cannot be created again", watches, own)
	}
}

// TestAcquireStopsWhenItsOwnNodeIsRemovedWhileQueued covers an operator or a
// session hiccup taking this process out of the queue. Waiting on a place in
// line that no longer exists would hang forever. Only this process's own node
// is deleted: the holder stays where it is, so nothing but the watch on the
// node that went can end the wait.
func TestAcquireStopsWhenItsOwnNodeIsRemovedWhileQueued(t *testing.T) {
	f := newFakeZK()
	holder := f.seedForeignLock(testLockPath(), otherUUID, 0)
	holderPath := path.Join(testLockPath(), holder)
	lock := newTestLock(t, f)

	done := make(chan error, 1)
	go func() {
		_, err := lock.Acquire(context.Background(), testLockData(t, lock))
		done <- err
	}()

	// The predecessor watch is armed last, so the queue is fully parked: the
	// own-node watch is already outstanding and no listing is in flight.
	waitArmed(t, f, holderPath)
	if err := f.Delete(path.Join(testLockPath(), "zlock#"+lock.UUID()+"#0000000001"), -1); err != nil {
		t.Fatalf("delete our queued node: %v", err)
	}

	if err := <-done; !errors.Is(err, ErrLockNodeMissing) {
		t.Fatalf("Acquire: want ErrLockNodeMissing, got %v", err)
	}
	// The holder never moved, so the wait ended on the own-node watch.
	if !f.exists(holderPath) {
		t.Fatalf("%s went away; the wait may have ended on the predecessor instead", holderPath)
	}
}

// TestAcquireStopsWhenItsNodeNeverReachesTheQueue covers the node being
// removed between the create and the first reading of the directory — an
// operator clearing the lock path, say. A create that leaves nothing behind
// has to be reported, not waited on.
func TestAcquireStopsWhenItsNodeNeverReachesTheQueue(t *testing.T) {
	f := newFakeZK()
	lock := newTestLock(t, f)
	var once sync.Once
	f.beforeChildren = func(listed string) {
		if listed != testLockPath() {
			return
		}
		once.Do(func() {
			_ = f.Delete(path.Join(testLockPath(), "zlock#"+lock.UUID()+"#0000000000"), -1)
		})
	}

	_, err := lock.Acquire(context.Background(), testLockData(t, lock))
	if !errors.Is(err, ErrLockNodeMissing) {
		t.Fatalf("Acquire: want ErrLockNodeMissing, got %v", err)
	}
	if _, ok := lock.LockID(); ok {
		t.Fatal("a failed acquisition must not report a held lock")
	}
}

// TestAcquireReportsAnUnreadableLockDirectory documents the one cleanup this
// package cannot do: if the directory cannot be listed, the node that was
// created cannot be found to delete it, and it is left to the session ending.
func TestAcquireReportsAnUnreadableLockDirectory(t *testing.T) {
	f := newFakeZK()
	lock := newTestLock(t, f)
	f.failChildren(testLockPath(), gozk.ErrConnectionClosed)

	_, err := lock.Acquire(context.Background(), testLockData(t, lock))
	if !errors.Is(err, gozk.ErrConnectionClosed) {
		t.Fatalf("Acquire: want the ZooKeeper error, got %v", err)
	}
	if _, ok := lock.LockID(); ok {
		t.Fatal("a failed acquisition must not report a held lock")
	}
}

// TestMaintainReportsTheLockNodeBeingDeleted is the ordinary loss: the session
// ended, so ZooKeeper removed the ephemeral node and the manager is already
// free to place these tablets elsewhere.
func TestMaintainReportsTheLockNodeBeingDeleted(t *testing.T) {
	f := newFakeZK()
	lock := newTestLock(t, f)
	id, err := lock.Acquire(context.Background(), testLockData(t, lock))
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	nodePath := path.Join(testLockPath(), lock.Node())

	done := make(chan error, 1)
	go func() { done <- lock.Maintain(context.Background()) }()
	waitArmed(t, f, nodePath)
	if err := f.Delete(nodePath, -1); err != nil {
		t.Fatalf("delete the lock node: %v", err)
	}

	err = <-done
	if !errors.Is(err, ErrLockLost) {
		t.Fatalf("Maintain: want ErrLockLost, got %v", err)
	}
	if lock.LossReason() != LossNodeDeleted {
		t.Fatalf("loss reason %s, want NODE_DELETED", lock.LossReason())
	}
	lost, ok := lock.LockID()
	if ok {
		t.Fatal("the lock is still reported as held after it was lost")
	}
	if lost != id {
		t.Fatalf("LockID() = %s after the loss, want the generation that ended (%s)", lost, id)
	}
	if !strings.Contains(err.Error(), id.String()) {
		t.Fatalf("the loss %q does not name the generation that ended", err)
	}
}

// TestMaintainReportsSessionExpiry covers the case the fence exists for: the
// ZooKeeper session ended, every ephemeral node went with it, and this process
// may not touch a tablet under that generation again.
func TestMaintainReportsSessionExpiry(t *testing.T) {
	f := newFakeZK()
	lock := newTestLock(t, f)
	if _, err := lock.Acquire(context.Background(), testLockData(t, lock)); err != nil {
		t.Fatalf("Acquire: %v", err)
	}

	done := make(chan error, 1)
	go func() { done <- lock.Maintain(context.Background()) }()
	waitArmed(t, f, path.Join(testLockPath(), lock.Node()))
	f.expire()

	if err := <-done; !errors.Is(err, ErrLockLost) {
		t.Fatalf("Maintain: want ErrLockLost, got %v", err)
	}
	if lock.LossReason() != LossUnmonitorable {
		t.Fatalf("loss reason %s, want UNMONITORABLE", lock.LossReason())
	}
}

// TestMaintainLeavesNoWatchOnANodeThatIsAlreadyGone covers the same
// registration question at the holder's end. A generation whose node vanished
// before the watch was armed ends, and it must end without leaving a watch on
// a sequential name ZooKeeper will not issue twice.
func TestMaintainLeavesNoWatchOnANodeThatIsAlreadyGone(t *testing.T) {
	f := newFakeZK()
	lock := newTestLock(t, f)
	if _, err := lock.Acquire(context.Background(), testLockData(t, lock)); err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	nodePath := path.Join(testLockPath(), lock.Node())

	var once sync.Once
	f.beforeGet = func(watched string) {
		if watched != nodePath {
			return
		}
		once.Do(func() { _ = f.Delete(nodePath, -1) })
	}

	err := lock.Maintain(context.Background())
	if !errors.Is(err, ErrLockLost) {
		t.Fatalf("Maintain: want ErrLockLost, got %v", err)
	}
	if lock.LossReason() != LossNodeDeleted {
		t.Fatalf("loss reason %s, want NODE_DELETED", lock.LossReason())
	}
	if watches := f.watchCount(nodePath); watches != 0 {
		t.Fatalf("%d watches left on %s, which cannot be created again", watches, nodePath)
	}
}

// TestAcquireRefusesDescriptorsThatDoNotMatchTheDirectory covers the other
// half of the identity a lock publishes. The directory is how a server is
// enumerated and the descriptor is what a client dials, and Accumulo builds
// both from the same address and resource group. They arrive here separately,
// so a lock could register under one address and advertise another — a server
// the manager can see and nothing can reach.
func TestAcquireRefusesDescriptorsThatDoNotMatchTheDirectory(t *testing.T) {
	for _, tc := range []struct {
		what string
		data func(t *testing.T, lock *ServiceLock) ServiceLockData
	}{
		{
			"another address",
			func(t *testing.T, lock *ServiceLock) ServiceLockData {
				data, err := TabletServerLockData(lock.UUID(), "shoal-2.example:9997", testGroup,
					TabletServerServices()...)
				if err != nil {
					t.Fatalf("TabletServerLockData: %v", err)
				}
				return data
			},
		},
		{
			"another resource group",
			func(t *testing.T, lock *ServiceLock) ServiceLockData {
				data, err := TabletServerLockData(lock.UUID(), testAddress, "analytics",
					TabletServerServices()...)
				if err != nil {
					t.Fatalf("TabletServerLockData: %v", err)
				}
				return data
			},
		},
	} {
		t.Run(tc.what, func(t *testing.T) {
			f := newFakeZK()
			lock := newTestLock(t, f)
			_, err := lock.Acquire(context.Background(), tc.data(t, lock))
			if !errors.Is(err, ErrInvalidLockData) {
				t.Fatalf("Acquire = %v, want ErrInvalidLockData", err)
			}
			if nodes := f.lockNodes(testLockPath()); len(nodes) != 0 {
				t.Fatalf("a refused advertisement still left %v in %s", nodes, testLockPath())
			}
		})
	}
}

// TestAcquireRefusesAnotherRolesServiceOnATabletServerPath applies the same
// rule to what a descriptor claims to speak. TabletServerLockData refuses the
// services other roles own, but ServiceLockData is an ordinary struct and can
// be built without it, and a MANAGER endpoint advertised from a tablet
// server's lock points every client that looks it up at a process which does
// not implement it.
func TestAcquireRefusesAnotherRolesServiceOnATabletServerPath(t *testing.T) {
	f := newFakeZK()
	lock := newTestLock(t, f)
	data := ServiceLockData{Descriptors: []ServiceDescriptor{{
		UUID:    lock.UUID(),
		Service: ThriftService("MANAGER"),
		Address: testAddress,
		Group:   testGroup,
	}}}
	_, err := lock.Acquire(context.Background(), data)
	if !errors.Is(err, ErrInvalidLockData) {
		t.Fatalf("Acquire = %v, want ErrInvalidLockData", err)
	}
	if nodes := f.lockNodes(testLockPath()); len(nodes) != 0 {
		t.Fatalf("a refused advertisement still left %v in %s", nodes, testLockPath())
	}
}

// TestAcquireRefusesATabletServerLockWithoutTSERV is the service every lock in
// the tablet-server tree has to carry. LiveTServerSet.checkServer reads
// getAddress(TSERV) off each held lock it finds there and passes the result
// straight to a TServerInstance, which dereferences it: no TSERV descriptor
// means a null address and a NullPointerException that ends the scan for every
// server in that pass, not just this one. A subset that leaves it out would
// look like a modest advertisement and stop the manager seeing the cluster.
func TestAcquireRefusesATabletServerLockWithoutTSERV(t *testing.T) {
	for _, tc := range []struct {
		what     string
		services []ThriftService
	}{
		{"a subset without it", []ThriftService{ServiceClient, ServiceTabletScan}},
		{"every other tablet-server service", []ThriftService{
			ServiceClient, ServiceTabletIngest, ServiceTabletManagement, ServiceTabletScan,
		}},
		{"no services at all", nil},
	} {
		t.Run(tc.what, func(t *testing.T) {
			f := newFakeZK()
			lock := newTestLock(t, f)
			data := ServiceLockData{}
			for _, service := range tc.services {
				data.Descriptors = append(data.Descriptors, ServiceDescriptor{
					UUID:    lock.UUID(),
					Service: service,
					Address: testAddress,
					Group:   testGroup,
				})
			}
			_, err := lock.Acquire(context.Background(), data)
			if !errors.Is(err, ErrInvalidLockData) {
				t.Fatalf("Acquire = %v, want ErrInvalidLockData", err)
			}
			// An advertisement with no descriptors at all never reaches this
			// check: encoding refuses an empty one first, for its own reasons.
			if len(tc.services) > 0 && !strings.Contains(err.Error(), string(ServiceTabletServer)) {
				t.Fatalf("Acquire = %v, want the missing service named", err)
			}
			if nodes := f.lockNodes(testLockPath()); len(nodes) != 0 {
				t.Fatalf("a refused advertisement still left %v in %s", nodes, testLockPath())
			}
		})
	}
}

// TestAcquireTakesATabletServerLockThatAdvertisesTSERV is the other side: the
// check is about the one service the manager reads, not about publishing all
// five. A process that serves TSERV and nothing else is visible and dialable.
func TestAcquireTakesATabletServerLockThatAdvertisesTSERV(t *testing.T) {
	f := newFakeZK()
	lock := newTestLock(t, f)
	data := ServiceLockData{Descriptors: []ServiceDescriptor{{
		UUID:    lock.UUID(),
		Service: ServiceTabletServer,
		Address: testAddress,
		Group:   testGroup,
	}}}
	if _, err := lock.Acquire(context.Background(), data); err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	if nodes := f.lockNodes(testLockPath()); len(nodes) != 1 {
		t.Fatalf("lock nodes in %s = %v, want the one just created", testLockPath(), nodes)
	}
}

// TestAcquireLeavesALockThatNamesNoServerAlone keeps both checks where they
// belong. The manager's lock lives at <instance>/managers/lock, where nothing
// in the path identifies a process, so there is no address to hold a
// descriptor to — and inventing one from the last two segments would refuse
// every lock that is not a tablet server's, along with the services that role
// owns.
func TestAcquireLeavesALockThatNamesNoServerAlone(t *testing.T) {
	f := newFakeZK()
	managerPath := testInstancePath + "/managers/lock"
	lock, err := NewServiceLock(f, ServiceLockOptions{Path: managerPath})
	if err != nil {
		t.Fatalf("NewServiceLock: %v", err)
	}
	// The package names constants for the services a tablet server publishes;
	// MANAGER is one Accumulo knows and this process does not announce, which
	// is the point of the case.
	data := ServiceLockData{Descriptors: []ServiceDescriptor{{
		UUID:    lock.UUID(),
		Service: ThriftService("MANAGER"),
		Address: "shoal-manager.example:9999",
		Group:   testGroup,
	}}}
	if _, err := lock.Acquire(context.Background(), data); err != nil {
		t.Fatalf("Acquire on a lock that names no server: %v", err)
	}
	if nodes := f.lockNodes(managerPath); len(nodes) != 1 {
		t.Fatalf("lock nodes in %s = %v, want the one just created", managerPath, nodes)
	}
}

// TestMaintainFailsClosedWhenTheLockCannotBeWatched is the equivalent of
// Accumulo's LockWatcher.unableToMonitorLockNode, where the Java tablet server
// halts. A lock this process cannot watch is one it cannot prove it still
// holds, and hosting on an unprovable lock is what the fence exists to stop.
func TestMaintainFailsClosedWhenTheLockCannotBeWatched(t *testing.T) {
	f := newFakeZK()
	lock := newTestLock(t, f)
	if _, err := lock.Acquire(context.Background(), testLockData(t, lock)); err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	f.failGet(path.Join(testLockPath(), lock.Node()), gozk.ErrConnectionClosed)

	err := lock.Maintain(context.Background())
	if !errors.Is(err, ErrLockLost) {
		t.Fatalf("Maintain: want ErrLockLost, got %v", err)
	}
	if lock.LossReason() != LossUnmonitorable {
		t.Fatalf("loss reason %s, want UNMONITORABLE", lock.LossReason())
	}
}

// TestMaintainKeepsWatchingThroughBenignEvents stops the opposite failure: a
// tablet server that drops everything because ZooKeeper mentioned its node.
func TestMaintainKeepsWatchingThroughBenignEvents(t *testing.T) {
	f := newFakeZK()
	lock := newTestLock(t, f)
	if _, err := lock.Acquire(context.Background(), testLockData(t, lock)); err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	nodePath := path.Join(testLockPath(), lock.Node())

	done := make(chan error, 1)
	go func() { done <- lock.Maintain(context.Background()) }()

	waitArmed(t, f, nodePath)
	f.fire(nodePath, gozk.Event{
		Type:  gozk.EventNodeDataChanged,
		State: gozk.StateHasSession,
		Path:  nodePath,
	})

	// The watch is re-armed rather than the lock given up.
	waitArmed(t, f, nodePath)
	select {
	case err := <-done:
		t.Fatalf("Maintain gave up on a data-changed event: %v", err)
	default:
	}
	if _, ok := lock.LockID(); !ok {
		t.Fatal("the lock was released on a benign event")
	}

	if err := f.Delete(nodePath, -1); err != nil {
		t.Fatalf("delete the lock node: %v", err)
	}
	if err := <-done; !errors.Is(err, ErrLockLost) {
		t.Fatalf("Maintain: want ErrLockLost, got %v", err)
	}
	if lock.LossReason() != LossNodeDeleted {
		t.Fatalf("loss reason %s, want NODE_DELETED", lock.LossReason())
	}
}

// TestMaintainCancellationKeepsTheLockHeld separates two decisions that look
// alike: stopping the watch is not giving up the tablets. The caller decides
// whether a shutdown releases, and until it does the lock is still this
// process's.
func TestMaintainCancellationKeepsTheLockHeld(t *testing.T) {
	f := newFakeZK()
	lock := newTestLock(t, f)
	id, err := lock.Acquire(context.Background(), testLockData(t, lock))
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	nodePath := path.Join(testLockPath(), lock.Node())

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- lock.Maintain(ctx) }()
	waitArmed(t, f, nodePath)
	cancel()

	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("Maintain: want context.Canceled, got %v", err)
	}
	held, ok := lock.LockID()
	if !ok || held != id {
		t.Fatalf("LockID() = %s, %v after cancellation; want %s, true", held, ok, id)
	}
	if lock.LossReason() != LossNone {
		t.Fatalf("cancellation recorded loss %s", lock.LossReason())
	}
	if !f.exists(nodePath) {
		t.Fatal("cancelling the watch deleted the lock node")
	}
}

func TestMaintainRequiresAHeldLock(t *testing.T) {
	lock := newTestLock(t, newFakeZK())
	if err := lock.Maintain(context.Background()); !errors.Is(err, ErrNotHeld) {
		t.Fatalf("Maintain: want ErrNotHeld, got %v", err)
	}
	if node := lock.Node(); node != "" {
		t.Fatalf("Node() = %q with no lock held", node)
	}
}

// TestMaintainTreatsAnExpiredSessionAsALoss covers the state change arriving
// on its own, without the watch being torn down first. An expired session has
// already taken the ephemeral node with it, whatever the event says.
func TestMaintainTreatsAnExpiredSessionAsALoss(t *testing.T) {
	f := newFakeZK()
	lock := newTestLock(t, f)
	if _, err := lock.Acquire(context.Background(), testLockData(t, lock)); err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	nodePath := path.Join(testLockPath(), lock.Node())

	done := make(chan error, 1)
	go func() { done <- lock.Maintain(context.Background()) }()
	waitArmed(t, f, nodePath)
	f.fire(nodePath, gozk.Event{Type: gozk.EventSession, State: gozk.StateExpired, Path: nodePath})

	if err := <-done; !errors.Is(err, ErrLockLost) {
		t.Fatalf("Maintain: want ErrLockLost, got %v", err)
	}
	if lock.LossReason() != LossUnmonitorable {
		t.Fatalf("loss reason %s, want UNMONITORABLE", lock.LossReason())
	}
}

// TestReleaseReportsADeleteFailure keeps a lock node that could not be removed
// visible. The session ending is the backstop, but until then the directory
// still says this process is a live server.
func TestReleaseReportsADeleteFailure(t *testing.T) {
	f := newFakeZK()
	lock := newTestLock(t, f)
	if _, err := lock.Acquire(context.Background(), testLockData(t, lock)); err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	nodePath := path.Join(testLockPath(), lock.Node())
	f.failDelete(nodePath, gozk.ErrConnectionClosed)

	err := lock.Release()
	if !errors.Is(err, gozk.ErrConnectionClosed) {
		t.Fatalf("Release: want the ZooKeeper error, got %v", err)
	}
	// The generation is over locally regardless: this process stops claiming
	// to hold a lock it has given up on.
	if _, ok := lock.LockID(); ok {
		t.Fatal("the lock is still reported held after a failed release")
	}
	if lock.LossReason() != LossReleased {
		t.Fatalf("loss reason %s, want RELEASED", lock.LossReason())
	}
}

// TestVerifyDetectsASupersedingHolder covers the case a watch cannot see. If
// the lock directory is deleted and recreated, ZooKeeper's sequence counter
// restarts, so a server arriving afterwards can take a lower number than the
// one held here — the node is still there, but it is no longer the holder.
func TestVerifyDetectsASupersedingHolder(t *testing.T) {
	f := newFakeZK()
	stale := f.seedForeignLock(testLockPath(), otherUUID, 5)
	if err := f.Delete(path.Join(testLockPath(), stale), -1); err != nil {
		t.Fatalf("delete the seeded node: %v", err)
	}
	lock := newTestLock(t, f)
	id, err := lock.Acquire(context.Background(), testLockData(t, lock))
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	if id.Sequence != 6 {
		t.Fatalf("acquired sequence %d, want 6", id.Sequence)
	}
	if err := lock.Verify(); err != nil {
		t.Fatalf("Verify while holding: %v", err)
	}

	// The counter restarted underneath us and somebody took a lower number.
	f.seedForeignLock(testLockPath(), managerUUID, 2)

	err = lock.Verify()
	if !errors.Is(err, ErrLockLost) {
		t.Fatalf("Verify: want ErrLockLost, got %v", err)
	}
	if lock.LossReason() != LossSuperseded {
		t.Fatalf("loss reason %s, want SUPERSEDED", lock.LossReason())
	}
}

// TestMaintainKeepsExactlyOneWatchOutstanding covers the ordinary case: a lock
// held across many verify intervals with nothing going wrong.
//
// A ZooKeeper existence watch is one-shot, but it stays registered on the
// client and on the server until it fires. Arming a fresh one on every pass
// through the maintain loop would leave the previous one behind, so a healthy
// tablet server would accumulate a registration per verify interval for the
// life of its lock — and every one of them would fire at once the moment the
// node finally changed.
func TestMaintainKeepsExactlyOneWatchOutstanding(t *testing.T) {
	f := newFakeZK()
	lock, err := NewServiceLock(f, ServiceLockOptions{
		Path:           testLockPath(),
		VerifyInterval: time.Millisecond,
	})
	if err != nil {
		t.Fatalf("NewServiceLock: %v", err)
	}
	if _, err := lock.Acquire(context.Background(), testLockData(t, lock)); err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	nodePath := path.Join(testLockPath(), lock.Node())

	// Verify is the only thing that lists the directory once maintaining has
	// started, so counting listings counts the intervals that have elapsed.
	// Set before the goroutine starts, and never touched again.
	var verifies atomic.Int64
	f.beforeChildren = func(listed string) {
		if listed == testLockPath() {
			verifies.Add(1)
		}
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- lock.Maintain(ctx) }()

	const intervals = 20
	deadline := time.After(5 * time.Second)
	for verifies.Load() < intervals {
		select {
		case err := <-done:
			t.Fatalf("Maintain returned before the intervals elapsed: %v", err)
		case <-deadline:
			t.Fatalf("only %d verify intervals elapsed, wanted %d", verifies.Load(), intervals)
		case <-time.After(time.Millisecond):
		}
	}
	if got := f.watchCount(nodePath); got != 1 {
		t.Fatalf("%d watches outstanding on %s after %d verify intervals, want exactly 1",
			got, nodePath, verifies.Load())
	}

	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Maintain: want context.Canceled, got %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Maintain did not return after cancellation")
	}
	if _, held := lock.LockID(); !held {
		t.Fatal("a cancelled maintain must leave the lock held")
	}
}

// TestMaintainRearmsTheWatchAfterItFires is the other half of the accounting:
// one watch is the ceiling, not the total. A watch that has fired is spent, so
// a lock that survives an unrelated event has to arm another or it would stop
// hearing about its own node.
func TestMaintainRearmsTheWatchAfterItFires(t *testing.T) {
	f := newFakeZK()
	lock := newTestLock(t, f)
	if _, err := lock.Acquire(context.Background(), testLockData(t, lock)); err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	nodePath := path.Join(testLockPath(), lock.Node())

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- lock.Maintain(ctx) }()

	waitArmed(t, f, nodePath)
	// A change that is not a deletion: the holder keeps the lock and has to
	// keep watching it.
	f.fire(nodePath, gozk.Event{
		Type:  gozk.EventNodeDataChanged,
		State: gozk.StateHasSession,
		Path:  nodePath,
	})
	waitArmed(t, f, nodePath)
	if got := f.watchCount(nodePath); got != 1 {
		t.Fatalf("%d watches outstanding on %s, want exactly 1", got, nodePath)
	}

	// The re-armed watch is the one that reports the loss.
	if err := f.Delete(nodePath, -1); err != nil {
		t.Fatalf("delete the lock node: %v", err)
	}
	select {
	case err := <-done:
		if !errors.Is(err, ErrLockLost) {
			t.Fatalf("Maintain: want ErrLockLost, got %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the re-armed watch never reported the deleted node")
	}
	if lock.LossReason() != LossNodeDeleted {
		t.Fatalf("loss reason %s, want NODE_DELETED", lock.LossReason())
	}
}

// TestReleaseWaitsOutACreateAlreadyInFlight covers a release that arrives
// while this process is in the middle of registering.
//
// Release reports success by sweeping every node this process made. A create
// that started before the release and landed after its sweep is exactly the
// node that sweep existed to remove, so a release that does not wait for it
// reports success over a node still holding a place in the queue — one no host
// adopted, that nothing is waiting on, and that nothing will clean up until
// the session ends.
//
// The abandoned acquisition does sweep its own node on the way out, so the
// directory ends empty either way. What differs is what a successful Release
// means at the moment it returns, which is what this asserts: no node of this
// process's may be created behind a release that has already reported success.
func TestReleaseWaitsOutACreateAlreadyInFlight(t *testing.T) {
	f := newFakeZK()
	lock := newTestLock(t, f)

	var (
		wg           sync.WaitGroup
		releaseErr   error
		atReleaseEnd []string
		released     = make(chan struct{})
	)
	f.beforeCreate = func(creating string) {
		if !strings.Contains(creating, zLockPrefix) {
			// An ancestor directory, not the lock node.
			return
		}
		f.beforeCreate = nil
		wg.Add(1)
		go func() {
			defer wg.Done()
			releaseErr = lock.Release()
			// Everything this process had created by the time the release
			// reported success. Anything outside it that turns up later is a
			// node the release promised was gone and was not.
			atReleaseEnd = f.createdPaths()
			close(released)
		}()
		// Give the release every chance to run to completion before the node
		// it has to sweep exists. Proving it did not is what needs a bound:
		// coordinated, it is parked here until this create is done; without
		// that, it lists an empty directory, reports success, and this create
		// lands behind it.
		select {
		case <-released:
		case <-time.After(100 * time.Millisecond):
		}
	}

	_, err := lock.Acquire(context.Background(), testLockData(t, lock))
	if !errors.Is(err, ErrLockReleased) {
		t.Fatalf("Acquire: want ErrLockReleased, got %v", err)
	}
	wg.Wait()
	if releaseErr != nil {
		t.Fatalf("Release: %v", releaseErr)
	}
	seen := make(map[string]struct{}, len(atReleaseEnd))
	for _, created := range atReleaseEnd {
		seen[created] = struct{}{}
	}
	for _, created := range f.createdPaths() {
		if !strings.Contains(created, zLockPrefix) {
			continue
		}
		if _, known := seen[created]; !known {
			t.Fatalf("Release reported success and %s was registered behind it", created)
		}
	}
	if nodes := f.lockNodes(testLockPath()); len(nodes) != 0 {
		t.Fatalf("Release reported success and left %v in the queue", nodes)
	}
	if lock.LossReason() != LossReleased {
		t.Fatalf("loss reason %s, want RELEASED", lock.LossReason())
	}
}

func TestVerifyDetectsAMissingNode(t *testing.T) {
	f := newFakeZK()
	lock := newTestLock(t, f)
	if _, err := lock.Acquire(context.Background(), testLockData(t, lock)); err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	if err := f.Delete(path.Join(testLockPath(), lock.Node()), -1); err != nil {
		t.Fatalf("delete the lock node: %v", err)
	}
	if err := lock.Verify(); !errors.Is(err, ErrLockLost) {
		t.Fatalf("Verify: want ErrLockLost, got %v", err)
	}
	if lock.LossReason() != LossNodeDeleted {
		t.Fatalf("loss reason %s, want NODE_DELETED", lock.LossReason())
	}
}

func TestVerifyFailsClosedWhenTheDirectoryCannotBeRead(t *testing.T) {
	f := newFakeZK()
	lock := newTestLock(t, f)
	if _, err := lock.Acquire(context.Background(), testLockData(t, lock)); err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	f.failChildren(testLockPath(), gozk.ErrConnectionClosed)

	if err := lock.Verify(); !errors.Is(err, ErrLockLost) {
		t.Fatalf("Verify: want ErrLockLost, got %v", err)
	}
	if lock.LossReason() != LossUnmonitorable {
		t.Fatalf("loss reason %s, want UNMONITORABLE", lock.LossReason())
	}
}

func TestVerifyRequiresAHeldLock(t *testing.T) {
	lock := newTestLock(t, newFakeZK())
	if err := lock.Verify(); !errors.Is(err, ErrNotHeld) {
		t.Fatalf("Verify: want ErrNotHeld, got %v", err)
	}
}

// TestVerifyReportsAGenerationThatEndedWhileItWasReading is the honesty
// requirement on a verification's timing. The directory can be read before a
// release lands and the answer given after it, and a release whose delete
// fails leaves the node exactly where the checks look for it — so a reading
// alone would confirm a generation that is over, to a caller asking whether it
// may still act on the lock.
func TestVerifyReportsAGenerationThatEndedWhileItWasReading(t *testing.T) {
	f := newFakeZK()
	lock := newTestLock(t, f)
	if _, err := lock.Acquire(context.Background(), testLockData(t, lock)); err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	nodePath := path.Join(testLockPath(), lock.Node())
	// The release cannot take the node away, so the directory still reads as
	// this process holding the lock.
	f.failDelete(nodePath, gozk.ErrConnectionClosed)

	var once sync.Once
	f.beforeChildren = func(listed string) {
		if listed != testLockPath() {
			return
		}
		once.Do(func() {
			f.beforeChildren = nil
			_ = lock.Release()
		})
	}

	err := lock.Verify()
	if !errors.Is(err, ErrLockLost) {
		t.Fatalf("Verify = %v, want ErrLockLost for a generation that ended mid-read", err)
	}
	if lock.LossReason() != LossReleased {
		t.Fatalf("loss reason %s, want RELEASED", lock.LossReason())
	}
	if !f.exists(nodePath) {
		t.Fatal("the node must survive for this test to mean anything")
	}
}

// TestVerifyIntervalCatchesASilentlyDroppedWatch is why the timer exists: a
// watch that never fires is indistinguishable from a quiet cluster, so the
// holder re-reads the directory on its own schedule.
func TestVerifyIntervalCatchesASilentlyDroppedWatch(t *testing.T) {
	f := newFakeZK()
	stale := f.seedForeignLock(testLockPath(), otherUUID, 5)
	if err := f.Delete(path.Join(testLockPath(), stale), -1); err != nil {
		t.Fatalf("delete the seeded node: %v", err)
	}
	lock, err := NewServiceLock(f, ServiceLockOptions{
		Path:           testLockPath(),
		VerifyInterval: 5 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("NewServiceLock: %v", err)
	}
	if _, err := lock.Acquire(context.Background(), testLockData(t, lock)); err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	// No watch fires for this: our node is untouched, but it is no longer
	// the lowest.
	f.seedForeignLock(testLockPath(), managerUUID, 2)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	err = lock.Maintain(ctx)
	if !errors.Is(err, ErrLockLost) {
		t.Fatalf("Maintain: want ErrLockLost, got %v", err)
	}
	if lock.LossReason() != LossSuperseded {
		t.Fatalf("loss reason %s, want SUPERSEDED", lock.LossReason())
	}
}

func TestReleaseDeletesTheNodeAndIsIdempotent(t *testing.T) {
	f := newFakeZK()
	lock := newTestLock(t, f)
	if _, err := lock.Acquire(context.Background(), testLockData(t, lock)); err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	nodePath := path.Join(testLockPath(), lock.Node())

	if err := lock.Release(); err != nil {
		t.Fatalf("Release: %v", err)
	}
	if f.exists(nodePath) {
		t.Fatal("Release left the lock node in place")
	}
	if _, ok := lock.LockID(); ok {
		t.Fatal("a released lock still reports itself held")
	}
	if lock.LossReason() != LossReleased {
		t.Fatalf("loss reason %s, want RELEASED", lock.LossReason())
	}
	if err := lock.Release(); err != nil {
		t.Fatalf("second Release: %v", err)
	}
	if lock.LossReason() != LossReleased {
		t.Fatalf("loss reason %s after a second Release, want RELEASED", lock.LossReason())
	}
}

// TestReleaseAfterALossKeepsTheOriginalReason matters for diagnosis: the
// interesting fact is that the session died, not that the shutdown path
// afterwards also asked to let go.
func TestReleaseAfterALossKeepsTheOriginalReason(t *testing.T) {
	f := newFakeZK()
	lock := newTestLock(t, f)
	if _, err := lock.Acquire(context.Background(), testLockData(t, lock)); err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	nodePath := path.Join(testLockPath(), lock.Node())

	done := make(chan error, 1)
	go func() { done <- lock.Maintain(context.Background()) }()
	waitArmed(t, f, nodePath)
	if err := f.Delete(nodePath, -1); err != nil {
		t.Fatalf("delete the lock node: %v", err)
	}
	if err := <-done; !errors.Is(err, ErrLockLost) {
		t.Fatalf("Maintain: want ErrLockLost, got %v", err)
	}

	if err := lock.Release(); err != nil {
		t.Fatalf("Release after a loss: %v", err)
	}
	if lock.LossReason() != LossNodeDeleted {
		t.Fatalf("loss reason %s, want the original NODE_DELETED", lock.LossReason())
	}
}

func TestReleaseRequiresAnAcquisition(t *testing.T) {
	lock := newTestLock(t, newFakeZK())
	if err := lock.Release(); !errors.Is(err, ErrNotHeld) {
		t.Fatalf("Release: want ErrNotHeld, got %v", err)
	}
}

// TestQueuedCandidatesAcquireInSequenceOrder is the whole point of the
// protocol under contention: whatever order processes arrive in, they take the
// lock one at a time and in the order ZooKeeper numbered them.
func TestQueuedCandidatesAcquireInSequenceOrder(t *testing.T) {
	f := newFakeZK()
	f.seed(testLockPath(), nil, false)

	locks := make([]*ServiceLock, 3)
	for i := range locks {
		locks[i] = newTestLock(t, f)
	}

	type acquisition struct {
		index int
		id    LockID
		err   error
	}
	acquired := make(chan acquisition, len(locks))
	for i, lock := range locks {
		go func(i int, lock *ServiceLock) {
			id, err := lock.Acquire(context.Background(), testLockData(t, lock))
			acquired <- acquisition{i, id, err}
		}(i, lock)
	}

	previous := int64(-1)
	for range locks {
		select {
		case got := <-acquired:
			if got.err != nil {
				t.Fatalf("Acquire: %v", got.err)
			}
			if got.id.Sequence <= previous {
				t.Fatalf("acquired sequence %d after %d: the queue is out of order",
					got.id.Sequence, previous)
			}
			previous = got.id.Sequence
			if err := locks[got.index].Release(); err != nil {
				t.Fatalf("Release: %v", err)
			}
		case <-time.After(10 * time.Second):
			t.Fatal("a queued candidate never acquired the lock")
		}
	}
	if remaining := f.lockNodes(testLockPath()); len(remaining) != 0 {
		t.Fatalf("lock nodes left behind: %v", remaining)
	}
}

// TestConcurrentMaintainVerifyAndRelease is a race-detector test: the loss can
// be recorded by whichever path notices first, and all of them must agree on
// one outcome.
func TestConcurrentMaintainVerifyAndRelease(t *testing.T) {
	f := newFakeZK()
	lock := newTestLock(t, f)
	if _, err := lock.Acquire(context.Background(), testLockData(t, lock)); err != nil {
		t.Fatalf("Acquire: %v", err)
	}

	var wg sync.WaitGroup
	start := make(chan struct{})
	wg.Add(4)
	go func() {
		defer wg.Done()
		<-start
		_ = lock.Maintain(context.Background())
	}()
	for i := 0; i < 2; i++ {
		go func() {
			defer wg.Done()
			<-start
			for j := 0; j < 20; j++ {
				_ = lock.Verify()
				_, _ = lock.LockID()
				_ = lock.LossReason()
			}
		}()
	}
	go func() {
		defer wg.Done()
		<-start
		_ = lock.Release()
	}()
	close(start)
	wg.Wait()

	if _, ok := lock.LockID(); ok {
		t.Fatal("the lock is still held after Release")
	}
	if lock.LossReason() == LossNone {
		t.Fatal("the lock ended without recording a reason")
	}
}

func TestParseLockNode(t *testing.T) {
	tests := []struct {
		name string
		node string
		want LockID
		ok   bool
	}{
		{"holder", "zlock#" + serverUUID + "#0000000000", LockID{UUID: serverUUID, Sequence: 0}, true},
		{"later generation", "zlock#" + serverUUID + "#0000000042", LockID{UUID: serverUUID, Sequence: 42}, true},
		{"no prefix", serverUUID + "#0000000000", LockID{}, false},
		{"wrong prefix", "lock#" + serverUUID + "#0000000000", LockID{}, false},
		{"no separator", "zlock#" + serverUUID + "0000000000", LockID{}, false},
		{"not a uuid", "zlock#shoal#0000000000", LockID{}, false},
		{"short sequence", "zlock#" + serverUUID + "#000000000", LockID{}, false},
		{"long sequence", "zlock#" + serverUUID + "#00000000000", LockID{}, false},
		{"non-numeric sequence", "zlock#" + serverUUID + "#00000000zz", LockID{}, false},
		{"negative sequence", "zlock#" + serverUUID + "#-000000001", LockID{}, false},
		{"past the 32-bit counter", "zlock#" + serverUUID + "#9999999999", LockID{}, false},
		{"empty", "", LockID{}, false},
		{"directory marker", "zlock#", LockID{}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := ParseLockNode(tt.node)
			if ok != tt.ok || got != tt.want {
				t.Fatalf("ParseLockNode(%q) = %s, %v; want %s, %v", tt.node, got, ok, tt.want, tt.ok)
			}
		})
	}
}

// TestSortLockNodesIgnoresStrangers keeps a stray child from displacing a real
// holder. Accumulo's validateAndSort drops anything that is not a lock node,
// and a directory with a leftover file in it must still resolve to the same
// holder in both implementations.
func TestSortLockNodesIgnoresStrangers(t *testing.T) {
	holder := "zlock#" + serverUUID + "#0000000001"
	queued := "zlock#" + otherUUID + "#0000000009"
	got := sortLockNodes([]string{
		queued,
		"notes.txt",
		"zlock#not-a-uuid#0000000000",
		holder,
		"zlock#" + managerUUID + "#bad",
	})
	want := []string{holder, queued}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("sortLockNodes = %v, want %v", got, want)
	}
	if len(sortLockNodes(nil)) != 0 {
		t.Fatal("an empty directory must produce no candidates")
	}
}

func TestFindLowestPrevPrefix(t *testing.T) {
	first := "zlock#" + otherUUID + "#0000000000"
	second := "zlock#" + otherUUID + "#0000000001"
	third := "zlock#" + managerUUID + "#0000000002"
	ours := "zlock#" + serverUUID + "#0000000003"

	tests := []struct {
		name   string
		sorted []string
		index  int
		want   string
	}{
		{"single predecessor", []string{first, ours}, 1, first},
		{"predecessor with duplicates", []string{first, second, ours}, 2, first},
		{"nearest holder only", []string{first, second, third, ours}, 3, third},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := findLowestPrevPrefix(tt.sorted, tt.index); got != tt.want {
				t.Fatalf("findLowestPrevPrefix = %q, want %q", got, tt.want)
			}
			// Whatever it picks, it picks something below us. That is the
			// property the wait depends on: a node below us has to go before
			// we can be lowest, so watching any of them cannot leave us
			// asleep past our turn.
			if !slices.Contains(tt.sorted[:tt.index], findLowestPrevPrefix(tt.sorted, tt.index)) {
				t.Fatalf("findLowestPrevPrefix returned %q, which is not one of the nodes ahead of us %v",
					findLowestPrevPrefix(tt.sorted, tt.index), tt.sorted[:tt.index])
			}
		})
	}
}

// TestFindLowestPrevPrefixStopsAtTheFirstOtherHolder is about the one shape
// where "lowest node of the holder ahead of us" and "what this returns" come
// apart: a holder whose nodes are not contiguous.
//
// A create whose response was lost leaves a node behind, and the retry lands
// wherever the sequence counter has got to by then — so a directory can read
// A, B, A, C. Scanning back from C, this stops at A's *second* node rather
// than reaching past B to A's first, because the scan ends at the first
// different prefix. ServiceLock.findLowestPrevPrefix does exactly the same,
// and matching it is deliberate: the node a candidate parks on is a private
// choice that ZooKeeper does not arbitrate, so there is nothing to gain by
// answering differently from the implementation this one is read against.
//
// What it costs is one spurious wakeup, in the window before A collapses its
// own duplicate: C wakes when A#2 goes, re-reads the directory, finds B still
// ahead of it and parks again. What it cannot cost is a missed wakeup — A#2
// is below C, so C cannot reach the front without it going first.
func TestFindLowestPrevPrefixStopsAtTheFirstOtherHolder(t *testing.T) {
	aFirst := "zlock#" + otherUUID + "#0000000000"
	b := "zlock#" + managerUUID + "#0000000001"
	aSecond := "zlock#" + otherUUID + "#0000000002"
	ours := "zlock#" + serverUUID + "#0000000003"
	interleaved := []string{aFirst, b, aSecond, ours}

	got := findLowestPrevPrefix(interleaved, 3)
	if got != aSecond {
		t.Fatalf("findLowestPrevPrefix = %q, want %q — the scan stops where the prefix changes", got, aSecond)
	}
	if got == aFirst {
		t.Fatal("reaching past an intervening holder would diverge from ServiceLock.findLowestPrevPrefix")
	}
	if !slices.Contains(interleaved[:3], got) {
		t.Fatalf("findLowestPrevPrefix returned %q, which is not ahead of us", got)
	}

	// The wakeup that this arrangement produces is an extra pass, not a
	// missed one: once the duplicate is gone the directory is contiguous
	// again and the next pass parks on the holder actually ahead of us.
	collapsed := []string{aFirst, b, ours}
	if got := findLowestPrevPrefix(collapsed, 2); got != b {
		t.Fatalf("after the duplicate is collapsed: findLowestPrevPrefix = %q, want %q", got, b)
	}
}

func TestTabletServerLockPath(t *testing.T) {
	got, err := TabletServerLockPath(testInstancePath, "ingest", testAddress)
	if err != nil {
		t.Fatalf("TabletServerLockPath: %v", err)
	}
	if want := testInstancePath + "/tservers/ingest/" + testAddress; got != want {
		t.Fatalf("TabletServerLockPath = %q, want %q", got, want)
	}
	got, err = TabletServerLockPath(testInstancePath, "", testAddress)
	if err != nil {
		t.Fatalf("TabletServerLockPath with an unset group: %v", err)
	}
	if want := testInstancePath + "/tservers/default/" + testAddress; got != want {
		t.Fatalf("an unset group must mean %q: got %q, want %q", DefaultResourceGroup, got, want)
	}
}

func TestTabletServerLockIDPath(t *testing.T) {
	got, err := TabletServerLockIDPath("ingest", testAddress)
	if err != nil {
		t.Fatalf("TabletServerLockIDPath: %v", err)
	}
	if want := "/tservers/ingest/" + testAddress; got != want {
		t.Fatalf("TabletServerLockIDPath = %q, want %q", got, want)
	}
}

func TestCompactorLockPath(t *testing.T) {
	got, err := CompactorLockPath(testInstancePath, "shoal_default", testAddress)
	if err != nil {
		t.Fatalf("CompactorLockPath: %v", err)
	}
	if want := testInstancePath + "/compactors/shoal_default/" + testAddress; got != want {
		t.Fatalf("CompactorLockPath = %q, want %q", got, want)
	}
}

// TestTabletServerLockPathRefusesAGroupAccumuloWouldReject covers the group
// arriving from configuration. The path segments are cleaned when they are
// joined, so a name with a traversal in it does not produce a rejected path —
// it produces a valid path in somebody else's subtree, which is worse: this
// server would register where nothing looks for a tablet server.
func TestTabletServerLockPathRefusesAGroupAccumuloWouldReject(t *testing.T) {
	for _, group := range invalidResourceGroups() {
		if group == "" {
			// An unset group means DefaultResourceGroup, covered above.
			continue
		}
		t.Run(group, func(t *testing.T) {
			got, err := TabletServerLockPath(testInstancePath, group, testAddress)
			if err == nil {
				t.Fatalf("TabletServerLockPath accepted group %q and returned %q", group, got)
			}
			if !errors.Is(err, ErrInvalidLockData) {
				t.Fatalf("error for group %q = %v, want ErrInvalidLockData", group, err)
			}
			if got != "" {
				t.Fatalf("a refused group must yield no path, got %q", got)
			}
		})
	}
}

// TestTabletServerLockPathTraversalWouldHaveEscaped pins why the check above
// matters, by showing what the unchecked join produced.
func TestTabletServerLockPathTraversalWouldHaveEscaped(t *testing.T) {
	escaped := path.Join(testInstancePath, zTabletServers, "../managers", testAddress)
	if strings.Contains(escaped, "/"+zTabletServers+"/") {
		t.Fatalf("expected the traversal to leave the tservers subtree, got %q", escaped)
	}
	if want := testInstancePath + "/managers/" + testAddress; escaped != want {
		t.Fatalf("traversal landed at %q, want %q", escaped, want)
	}
}

// TestTabletServerLockPathRefusesAnAddressAccumuloCouldNotDial covers the
// other variable segment. The address is the directory's name as well as the
// endpoint written into the node, so a value that fails in either sense is
// refused before it is registered: a name nothing can dial draws work to a
// process that cannot answer, and a name carrying a separator registers this
// server somewhere else entirely.
func TestTabletServerLockPathRefusesAnAddressAccumuloCouldNotDial(t *testing.T) {
	for _, address := range []string{
		"",
		"shoal-1.example",
		"shoal-1.example:thrift",
		"../..:9997",
		"0.0.0.0:9997",
		"[::]:9997",
		placeholderAddress,
	} {
		t.Run(address, func(t *testing.T) {
			got, err := TabletServerLockPath(testInstancePath, testGroup, address)
			if err == nil {
				t.Fatalf("TabletServerLockPath accepted address %q and returned %q", address, got)
			}
			if !errors.Is(err, ErrInvalidLockData) {
				t.Fatalf("error for address %q = %v, want ErrInvalidLockData", address, err)
			}
			if got != "" {
				t.Fatalf("a refused address must yield no path, got %q", got)
			}
		})
	}
}

// TestTabletServerLockPathAddressTraversalWouldHaveEscaped is the same
// demonstration for the address: it is a path segment too, and one carrying a
// traversal lands in the manager's subtree rather than being rejected.
func TestTabletServerLockPathAddressTraversalWouldHaveEscaped(t *testing.T) {
	const traversal = "../../managers/lock:9997"
	escaped := path.Join(testInstancePath, zTabletServers, testGroup, traversal)
	if strings.Contains(escaped, "/"+zTabletServers+"/") {
		t.Fatalf("expected the traversal to leave the tservers subtree, got %q", escaped)
	}
	if want := testInstancePath + "/managers/lock:9997"; escaped != want {
		t.Fatalf("traversal landed at %q, want %q", escaped, want)
	}
	if got, err := TabletServerLockPath(testInstancePath, testGroup, traversal); err == nil {
		t.Fatalf("TabletServerLockPath accepted %q and returned %q", traversal, got)
	}
}

// TestAcquireRefusesToCommitAnOwnershipTheCallerCancelled covers a
// cancellation that lands after the node is created and before the queue is
// read. Whether this process is first by then is a race, and the outcome must
// not turn on it: a caller that stopped waiting gets the same refusal either
// way, and the node it created goes back out of the directory instead of
// staying there as a lock nobody is maintaining.
func TestAcquireRefusesToCommitAnOwnershipTheCallerCancelled(t *testing.T) {
	f := newFakeZK()
	lock := newTestLock(t, f)
	dir := testLockPath()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	f.beforeChildren = func(listing string) {
		if listing != dir {
			return
		}
		f.beforeChildren = nil
		cancel()
	}

	if _, err := lock.Acquire(ctx, testLockData(t, lock)); !errors.Is(err, context.Canceled) {
		t.Fatalf("Acquire: want context.Canceled, got %v", err)
	}
	// The node existed before the cancellation was noticed, so this is the
	// commit point being refused rather than the check made before the create.
	node := lockNodePath(lock.UUID(), 0)
	swept := false
	for _, deleted := range f.deletedPaths() {
		if deleted == node {
			swept = true
		}
	}
	if !swept {
		t.Fatalf("%s was never created and swept: created %v, deleted %v",
			node, f.createdPaths(), f.deletedPaths())
	}
	if nodes := f.lockNodes(dir); len(nodes) != 0 {
		t.Fatalf("a cancelled acquisition left %v queued in %s", nodes, dir)
	}
	if err := lock.Verify(); !errors.Is(err, ErrNotHeld) {
		t.Fatalf("Verify after a cancelled acquisition: want ErrNotHeld, got %v", err)
	}
}

// TestMaintainStopsWhenAReleaseCannotDeleteTheNode covers the release that
// produces no watch event. Maintain normally learns of one the same way it
// learns of anything else — the node disappears and the watch fires — but a
// delete that fails leaves the node in place, and with no verify interval
// configured there is nothing else in the select to wake it. The generation
// has still ended: the caller gave it up, and a watch left running would be
// watching a lock nobody holds until the process stopped.
func TestMaintainStopsWhenAReleaseCannotDeleteTheNode(t *testing.T) {
	f := newFakeZK()
	lock := newTestLock(t, f)
	id, err := lock.Acquire(context.Background(), testLockData(t, lock))
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	node := path.Join(testLockPath(), id.String())
	injected := errors.New("zookeeper is unavailable")
	f.failDelete(node, injected)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	maintained := make(chan error, 1)
	go func() { maintained <- lock.Maintain(ctx) }()
	waitFor(t, "the held node to be watched", func() bool { return f.watchCount(node) == 1 })

	if err := lock.Release(); !errors.Is(err, injected) {
		t.Fatalf("Release: want the injected delete failure, got %v", err)
	}
	select {
	case err := <-maintained:
		if !errors.Is(err, ErrLockLost) {
			t.Fatalf("Maintain: want ErrLockLost, got %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Maintain kept watching a lock that had already been released")
	}
	if !f.exists(node) {
		t.Fatal("the delete was meant to fail: with the node gone, a watch event could have woken Maintain")
	}
}

func TestNewServiceLockValidatesItsOptions(t *testing.T) {
	f := newFakeZK()
	if _, err := NewServiceLock(nil, ServiceLockOptions{Path: testLockPath()}); err == nil {
		t.Fatal("NewServiceLock accepted a nil connection")
	}
	for _, tt := range []struct {
		name string
		opts ServiceLockOptions
		want error
	}{
		{"empty path", ServiceLockOptions{}, ErrInvalidLockPath},
		{"relative path", ServiceLockOptions{Path: "tservers/default"}, ErrInvalidLockPath},
		{"zookeeper root", ServiceLockOptions{Path: "/"}, ErrInvalidLockPath},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := NewServiceLock(f, tt.opts); !errors.Is(err, tt.want) {
				t.Fatalf("NewServiceLock: want %v, got %v", tt.want, err)
			}
		})
	}
	if _, err := NewServiceLock(f, ServiceLockOptions{
		Path:           testLockPath(),
		VerifyInterval: -time.Second,
	}); err == nil {
		t.Fatal("NewServiceLock accepted a negative verify interval")
	}
	lock, err := NewServiceLock(f, ServiceLockOptions{Path: testLockPath()})
	if err != nil {
		t.Fatalf("NewServiceLock: %v", err)
	}
	if lock.Path() != testLockPath() {
		t.Fatalf("Path() = %q, want %q", lock.Path(), testLockPath())
	}
}

func TestLossReasonString(t *testing.T) {
	for reason, want := range map[LossReason]string{
		LossNone:          "NONE",
		LossNodeDeleted:   "NODE_DELETED",
		LossUnmonitorable: "UNMONITORABLE",
		LossSuperseded:    "SUPERSEDED",
		LossReleased:      "RELEASED",
		LossReason(42):    "LossReason(42)",
	} {
		if got := reason.String(); got != want {
			t.Fatalf("LossReason(%d).String() = %q, want %q", int(reason), got, want)
		}
	}
}

// TestParseLockNodeRefusesNonCanonicalUUID keeps the reader in step with
// Accumulo. Go's uuid.Parse accepts bare-hex, URN, and braced forms that
// ServiceLock.validateAndSort throws on, so accepting them here would make
// this package disagree with Accumulo about who is in the queue and who holds
// the lock.
func TestParseLockNodeRefusesNonCanonicalUUID(t *testing.T) {
	for name, value := range nonCanonicalUUIDs() {
		t.Run(name, func(t *testing.T) {
			node := fmt.Sprintf("%s%s#%010d", zLockPrefix, value, 3)
			if id, ok := ParseLockNode(node); ok {
				t.Fatalf("ParseLockNode(%q) = %s, true; Accumulo refuses that node", node, id)
			}
		})
	}
	node := fmt.Sprintf("%s%s#%010d", zLockPrefix, canonicalTestUUID, 3)
	id, ok := ParseLockNode(node)
	if !ok || id.UUID != canonicalTestUUID || id.Sequence != 3 {
		t.Fatalf("ParseLockNode(%q) = %s, %t; want the canonical form to parse", node, id, ok)
	}
}

// TestSortLockNodesDropsNonCanonicalUUIDs is the consequence that matters: a
// stray child whose UUID Accumulo cannot parse must not be able to sit at the
// head of the queue, because Accumulo would not see it there either.
func TestSortLockNodesDropsNonCanonicalUUIDs(t *testing.T) {
	bare := strings.ReplaceAll(canonicalTestUUID, "-", "")
	real := fmt.Sprintf("%s%s#%010d", zLockPrefix, serverUUID, 1)
	sorted := sortLockNodes([]string{
		fmt.Sprintf("%s%s#%010d", zLockPrefix, bare, 0),
		real,
	})
	if len(sorted) != 1 || sorted[0] != real {
		t.Fatalf("sortLockNodes = %v, want only %s", sorted, real)
	}
}

// TestEveryServiceLockMintsAnIdentityOfItsOwn is the property the node prefix
// rests on. The prefix carries the lock UUID, and both the acquisition and the
// cleanup treat it as a statement of ownership: the acquisition adopts what it
// finds under it, and the release deletes what it finds under it. Neither is
// safe if two locks can carry the same prefix, so NewServiceLock mints the
// UUID itself and offers no way to supply one — the same thing Accumulo's
// announceExistence does with UUID.randomUUID() per ServiceLock.
//
// The UUID also has to be one Accumulo can read back. A node whose UUID
// validateAndSort throws on is a node Accumulo drops, so this process would
// queue invisibly: waiting its turn while Accumulo handed the lock to somebody
// else.
func TestEveryServiceLockMintsAnIdentityOfItsOwn(t *testing.T) {
	f := newFakeZK()
	seen := make(map[string]bool)
	for i := 0; i < 64; i++ {
		lock := newTestLock(t, f)
		if seen[lock.UUID()] {
			t.Fatalf("two locks were built with uuid %s: the node prefix names a lock, "+
				"and two locks under one prefix means each can take the other's node", lock.UUID())
		}
		seen[lock.UUID()] = true
		if _, ok := ParseLockNode(zLockPrefix + lock.UUID() + "#0000000000"); !ok {
			t.Fatalf("minted uuid %q cannot name a lock node Accumulo would read", lock.UUID())
		}
	}

	// The identity is not the caller's to choose: ServiceLockOptions has no
	// field for it, so there is no supported way to reintroduce the collision.
	if field, ok := reflect.TypeOf(ServiceLockOptions{}).FieldByName("UUID"); ok {
		t.Fatalf("ServiceLockOptions.UUID is back as %s: a caller-supplied lock uuid lets two "+
			"ServiceLocks share a node prefix", field.Type)
	}
}

// TestAcquireFailsWhenItsOwnQueuedNodeDisappears covers the wait that would
// otherwise never end. A queued candidate that watches only the node ahead of
// it has no way to learn that its own node is gone: the holder stays put, no
// event about it ever fires, and the acquisition sleeps through a queue it is
// no longer in until an unrelated process happens to leave.
func TestAcquireFailsWhenItsOwnQueuedNodeDisappears(t *testing.T) {
	f := newFakeZK()
	holder := f.seedForeignLock(testLockPath(), otherUUID, 0)
	lock := newTestLock(t, f)

	errs := make(chan error, 1)
	go func() {
		_, err := lock.Acquire(context.Background(), testLockData(t, lock))
		errs <- err
	}()

	own := nextArmed(t, f)
	if !strings.HasPrefix(path.Base(own), lock.nodePrefix()) {
		t.Fatalf("first watch is on %s, want this process's own node", own)
	}
	// Wait for the second watch too, so the acquisition is provably asleep on
	// both before its node is taken away.
	waitArmed(t, f, path.Join(testLockPath(), holder))

	if err := f.Delete(own, -1); err != nil {
		t.Fatalf("delete the queued node: %v", err)
	}

	select {
	case err := <-errs:
		if !errors.Is(err, ErrLockNodeMissing) {
			t.Fatalf("Acquire = %v, want ErrLockNodeMissing", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Acquire is still waiting on a queue it is no longer in")
	}
	if _, held := lock.LockID(); held {
		t.Fatal("the lock must not be held after its node disappeared")
	}
	if !f.exists(path.Join(testLockPath(), holder)) {
		t.Fatal("the holder's node must be untouched")
	}
}

// TestReleaseWhileQueuedRefusesTheAcquisition closes the window between a
// release and an acquisition that has not finished. Release used to look only
// at the node this process holds, which is empty while it is still queued, so
// it reported success and left the acquisition running: the caller was told
// the lock was gone and then went on to hold it.
func TestReleaseWhileQueuedRefusesTheAcquisition(t *testing.T) {
	f := newFakeZK()
	holder := f.seedForeignLock(testLockPath(), otherUUID, 0)
	lock := newTestLock(t, f)

	errs := make(chan error, 1)
	go func() {
		_, err := lock.Acquire(context.Background(), testLockData(t, lock))
		errs <- err
	}()
	nextArmed(t, f)
	waitArmed(t, f, path.Join(testLockPath(), holder))

	if err := lock.Release(); err != nil {
		t.Fatalf("Release while queued: %v", err)
	}

	select {
	case err := <-errs:
		if !errors.Is(err, ErrLockReleased) {
			t.Fatalf("Acquire = %v, want ErrLockReleased", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Release did not wake the queued acquisition")
	}
	if _, held := lock.LockID(); held {
		t.Fatal("a lock released before it was acquired must never be held")
	}
	if reason := lock.LossReason(); reason != LossReleased {
		t.Fatalf("LossReason = %s, want RELEASED", reason)
	}
	for _, child := range f.lockNodes(testLockPath()) {
		if strings.HasPrefix(child, lock.nodePrefix()) {
			t.Fatalf("%s still holds a place in the queue after Release", child)
		}
	}
}

// TestReleaseBeforeTheTurnArrivesRefusesToTakeIt is the same race decided the
// other way: the release lands while the holder is leaving, so acquisition
// reaches the front of the queue. It must still refuse, because the caller has
// already been told the lock is gone.
func TestReleaseBeforeTheTurnArrivesRefusesToTakeIt(t *testing.T) {
	f := newFakeZK()
	holder := f.seedForeignLock(testLockPath(), otherUUID, 0)
	lock := newTestLock(t, f)

	// Release the moment the queue is re-read, which is after the holder has
	// gone and before ownership is recorded.
	//
	// Release lists the directory too, so the hook has to disarm itself before
	// it calls back into the fake rather than guard with a sync.Once, which
	// would deadlock on the re-entrant call.
	f.beforeChildren = func(string) {
		f.beforeChildren = nil
		if err := f.Delete(path.Join(testLockPath(), holder), -1); err != nil {
			panic(err)
		}
		_ = lock.Release()
	}

	_, err := lock.Acquire(context.Background(), testLockData(t, lock))
	if !errors.Is(err, ErrLockReleased) {
		t.Fatalf("Acquire = %v, want ErrLockReleased", err)
	}
	if _, held := lock.LockID(); held {
		t.Fatal("acquisition must not take a lock that was already released")
	}
	for _, child := range f.lockNodes(testLockPath()) {
		if strings.HasPrefix(child, lock.nodePrefix()) {
			t.Fatalf("%s survived a released acquisition", child)
		}
	}
}

// TestReleaseSweepsEveryNodeUnderThisLocksPrefix covers the duplicate a lost
// create response left behind. Acquisition collapses the duplicates it can
// see, but a retry whose first attempt was already in flight can land after
// that, so a held lock can still have a second node of this process behind it.
// Releasing only the held node would promote that duplicate to holder of a
// lock nobody is waiting on, blocking every candidate behind it until the
// session ended.
func TestReleaseSweepsEveryNodeUnderThisLocksPrefix(t *testing.T) {
	f := newFakeZK()
	lock := newTestLock(t, f)
	if _, err := lock.Acquire(context.Background(), testLockData(t, lock)); err != nil {
		t.Fatalf("Acquire: %v", err)
	}

	// A node under this process's prefix that it did not adopt. go-zookeeper
	// cannot produce one this late — it fails pending requests on a reconnect
	// rather than replaying them — but the sweep must be complete whatever
	// left it there, because the session keeps it and its place in line.
	duplicate := f.seedForeignLock(testLockPath(), lock.UUID(), 7)
	if !f.exists(path.Join(testLockPath(), duplicate)) {
		t.Fatal("the duplicate must exist for this test to mean anything")
	}

	if err := lock.Release(); err != nil {
		t.Fatalf("Release: %v", err)
	}
	for _, child := range f.lockNodes(testLockPath()) {
		if strings.HasPrefix(child, lock.nodePrefix()) {
			t.Fatalf("%s survived Release and can become a holder nobody is waiting on", child)
		}
	}
}

// TestStaleReleaseLeavesTheGenerationThatReplacedItAlone is the fence across
// ServiceLocks, and what the per-lock UUID buys. A release can arrive at any
// time after its generation ended — a shutdown path that runs late, a
// goroutine that outlived the lock — and by then the process may have
// rejoined. Sweeping by prefix is only safe because the rejoin is under a
// prefix of its own; if the two shared one, the stale shutdown would delete
// the live node of the generation that replaced it, revoking a lock the
// manager had just seen taken and leaving a server that answers as a holder
// ZooKeeper no longer knows.
func TestStaleReleaseLeavesTheGenerationThatReplacedItAlone(t *testing.T) {
	f := newFakeZK()
	old := newTestLock(t, f)
	if _, err := old.Acquire(context.Background(), testLockData(t, old)); err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	oldPath := path.Join(testLockPath(), old.Node())

	// The generation ends the ordinary way: the node goes, and Maintain says
	// so. Only then does the process rejoin.
	lost := make(chan error, 1)
	go func() { lost <- old.Maintain(context.Background()) }()
	waitArmed(t, f, oldPath)
	if err := f.Delete(oldPath, -1); err != nil {
		t.Fatalf("delete the lock node: %v", err)
	}
	if err := <-lost; !errors.Is(err, ErrLockLost) {
		t.Fatalf("Maintain: want ErrLockLost, got %v", err)
	}

	rejoined := newTestLock(t, f)
	id, err := rejoined.Acquire(context.Background(), testLockData(t, rejoined))
	if err != nil {
		t.Fatalf("rejoin: %v", err)
	}
	newPath := path.Join(testLockPath(), rejoined.Node())
	if strings.HasPrefix(path.Base(newPath), old.nodePrefix()) {
		t.Fatalf("the rejoin took a node under the old lock's prefix (%s), so this test proves nothing", newPath)
	}

	// The shutdown of the generation that already ended, arriving late.
	if err := old.Release(); err != nil {
		t.Fatalf("stale Release: %v", err)
	}

	if !f.exists(newPath) {
		t.Fatalf("%s was swept by the release of a generation that had already ended", newPath)
	}
	held, ok := rejoined.LockID()
	if !ok || held != id {
		t.Fatalf("LockID() = %s, %t after a stale release, want the generation that rejoined (%s)", held, ok, id)
	}
	if err := rejoined.Verify(); err != nil {
		t.Fatalf("Verify after a stale release: %v", err)
	}
}

// TestReleaseCalledTwiceLeavesTheGenerationThatReplacedItAlone is the same
// fence on the path that reaches it most easily. Release is documented as safe
// to call twice, so a shutdown that runs from both a signal handler and a
// defer is ordinary; if the rejoin happens in between, the second call must
// still only take back what its own generation created.
func TestReleaseCalledTwiceLeavesTheGenerationThatReplacedItAlone(t *testing.T) {
	f := newFakeZK()
	old := newTestLock(t, f)
	if _, err := old.Acquire(context.Background(), testLockData(t, old)); err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	if err := old.Release(); err != nil {
		t.Fatalf("Release: %v", err)
	}

	rejoined := newTestLock(t, f)
	if _, err := rejoined.Acquire(context.Background(), testLockData(t, rejoined)); err != nil {
		t.Fatalf("rejoin: %v", err)
	}
	newPath := path.Join(testLockPath(), rejoined.Node())

	if err := old.Release(); err != nil {
		t.Fatalf("second Release: %v", err)
	}
	if !f.exists(newPath) {
		t.Fatalf("%s was swept by a second release of the generation before it", newPath)
	}
	if _, ok := rejoined.LockID(); !ok {
		t.Fatal("the rejoined generation stopped being held after a stale release")
	}
}

// TestConcurrentStaleReleasesLeaveASuccessorAlone is the same fence without an
// ordering to lean on. Release is safe to call twice, so a shutdown reached
// from a signal handler and a defer can run both at once, and neither has to
// finish before the process rejoins: the sweep of one can interleave with the
// create of the other. Nothing about the timing may matter, because the prefix
// each sweep reads is its own lock's.
func TestConcurrentStaleReleasesLeaveASuccessorAlone(t *testing.T) {
	f := newFakeZK()
	old := newTestLock(t, f)
	if _, err := old.Acquire(context.Background(), testLockData(t, old)); err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	oldPath := path.Join(testLockPath(), old.Node())

	lost := make(chan error, 1)
	go func() { lost <- old.Maintain(context.Background()) }()
	waitArmed(t, f, oldPath)
	if err := f.Delete(oldPath, -1); err != nil {
		t.Fatalf("delete the lock node: %v", err)
	}
	if err := <-lost; !errors.Is(err, ErrLockLost) {
		t.Fatalf("Maintain: want ErrLockLost, got %v", err)
	}

	// The rejoin and the late shutdown run together.
	rejoined := newTestLock(t, f)
	var wg sync.WaitGroup
	start := make(chan struct{})
	acquired := make(chan error, 1)
	releases := make(chan error, 4)
	wg.Add(5)
	go func() {
		defer wg.Done()
		<-start
		_, err := rejoined.Acquire(context.Background(), testLockData(t, rejoined))
		acquired <- err
	}()
	for i := 0; i < 4; i++ {
		go func() {
			defer wg.Done()
			<-start
			releases <- old.Release()
		}()
	}
	close(start)
	wg.Wait()

	if err := <-acquired; err != nil {
		t.Fatalf("rejoin: %v", err)
	}
	for i := 0; i < 4; i++ {
		if err := <-releases; err != nil {
			t.Fatalf("stale Release: %v", err)
		}
	}
	newPath := path.Join(testLockPath(), rejoined.Node())
	if !f.exists(newPath) {
		t.Fatalf("%s was swept by a release of the generation before it", newPath)
	}
	if err := rejoined.Verify(); err != nil {
		t.Fatalf("Verify after the concurrent stale releases: %v", err)
	}
}

// TestReleasingAQueuedCandidateLeavesTheOthersAlone covers the other sweep,
// the one an acquisition runs on its way out. Release wakes a queued Acquire,
// which cleans up and returns; that cleanup runs after Release has already
// reported success, so it cannot be ordered against anything the caller does
// next. It is safe for the same reason: it sweeps its own lock's prefix, and
// every other candidate has one of its own.
func TestReleasingAQueuedCandidateLeavesTheOthersAlone(t *testing.T) {
	f := newFakeZK()
	holder := f.seedForeignLock(testLockPath(), otherUUID, 0)
	holderPath := path.Join(testLockPath(), holder)

	leaving := newTestLock(t, f)
	leavingDone := make(chan error, 1)
	go func() {
		_, err := leaving.Acquire(context.Background(), testLockData(t, leaving))
		leavingDone <- err
	}()
	waitArmed(t, f, holderPath)

	staying := newTestLock(t, f)
	stayingDone := make(chan error, 1)
	go func() {
		_, err := staying.Acquire(context.Background(), testLockData(t, staying))
		stayingDone <- err
	}()
	waitFor(t, "the second candidate to queue", func() bool {
		return len(nodesUnder(f, staying.nodePrefix())) == 1
	})

	if err := leaving.Release(); err != nil {
		t.Fatalf("Release of a queued candidate: %v", err)
	}
	if err := <-leavingDone; !errors.Is(err, ErrLockReleased) {
		t.Fatalf("the released acquisition returned %v, want ErrLockReleased", err)
	}
	if left := nodesUnder(f, staying.nodePrefix()); len(left) != 1 {
		t.Fatalf("nodes under the other candidate's prefix = %v, want its one place in line: "+
			"the released candidate's cleanup took it", left)
	}

	// The cleanup that acquisition ran on its way out must not have taken the
	// other candidate's place in line.
	if err := f.Delete(holderPath, -1); err != nil {
		t.Fatalf("delete the holder: %v", err)
	}
	if err := <-stayingDone; err != nil {
		t.Fatalf("the candidate that stayed never acquired: %v", err)
	}
	if err := staying.Verify(); err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if err := staying.Release(); err != nil {
		t.Fatalf("Release: %v", err)
	}
	if nodes := f.lockNodes(testLockPath()); len(nodes) != 0 {
		t.Fatalf("lock nodes left behind: %v", nodes)
	}
}

// TestStaleReleaseStillTakesBackItsOwnNode is the other side: scoping the
// sweep must not turn a stale release into a leak. The node of the generation
// that ended is this session's, and until the session goes it keeps its place
// in line, so a release arriving after a rejoin still has to take it back.
func TestStaleReleaseStillTakesBackItsOwnNode(t *testing.T) {
	f := newFakeZK()
	old := newTestLock(t, f)
	if _, err := old.Acquire(context.Background(), testLockData(t, old)); err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	oldPath := path.Join(testLockPath(), old.Node())

	lost := make(chan error, 1)
	go func() { lost <- old.Maintain(context.Background()) }()
	waitArmed(t, f, oldPath)
	// The generation ends without the node going. A session event reaches the
	// watcher before ZooKeeper has removed the ephemeral nodes, which is how a
	// lock is lost while its node is still in the directory.
	f.fire(oldPath, gozk.Event{Type: gozk.EventSession, State: gozk.StateExpired, Path: oldPath})
	if err := <-lost; !errors.Is(err, ErrLockLost) {
		t.Fatalf("Maintain: want ErrLockLost, got %v", err)
	}
	if !f.exists(oldPath) {
		t.Fatal("the node has to survive the loss for this test to mean anything")
	}

	if err := old.Release(); err != nil {
		t.Fatalf("stale Release: %v", err)
	}
	if f.exists(oldPath) {
		t.Fatalf("%s outlived the release of the generation that created it", oldPath)
	}
}

// TestReleaseAfterALossSweepsANodeItNeverListed is the other direction of the
// same property: reaching far enough. A generation can end with the session
// still alive — a node an operator removed, a read that fails closed — and
// anything left under this prefix survives with it. Once the adopted node goes
// that leftover is the lowest in the directory, so the manager reads it as the
// server's lock while no ServiceLock maintains it. The sweep finds it without
// having been told about it, because the prefix is this lock's alone and it is
// the directory that is asked.
func TestReleaseAfterALossSweepsANodeItDidNotCreate(t *testing.T) {
	f := newFakeZK()
	lock := newTestLock(t, f)
	if _, err := lock.Acquire(context.Background(), testLockData(t, lock)); err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	held := path.Join(testLockPath(), lock.Node())

	// The generation ends while the session, and so the stray node, lives on.
	// Nothing lists the directory between the stray appearing and the release,
	// so the sweep has no record of it to work from.
	lost := make(chan error, 1)
	go func() { lost <- lock.Maintain(context.Background()) }()
	waitArmed(t, f, held)
	stray := f.seedForeignLock(testLockPath(), lock.UUID(), 9)
	strayPath := path.Join(testLockPath(), stray)
	if err := f.Delete(held, -1); err != nil {
		t.Fatalf("delete the lock node: %v", err)
	}
	if err := <-lost; !errors.Is(err, ErrLockLost) {
		t.Fatalf("Maintain: want ErrLockLost, got %v", err)
	}
	if !f.exists(strayPath) {
		t.Fatal("the stray node has to survive the loss for this test to mean anything")
	}

	if err := lock.Release(); err != nil {
		t.Fatalf("Release: %v", err)
	}
	if f.exists(strayPath) {
		t.Fatalf("%s outlived the release and is now the holder the manager reads", strayPath)
	}
}

// TestAcquireCannotAdoptANodeLeftByAnEarlierLock is the fence read from the
// acquisition side, and the reason the identity is not the caller's to choose.
//
// Acquisition adopts the lowest node under its prefix, which is how a create
// whose answer was lost is still found. If a rejoin could carry the prefix its
// predecessor used, that same step would adopt the node the predecessor left
// behind — and then the "new" generation would hand back the LockID the dead
// one had, publish the payload already sitting in that node, and accept a
// manager request stamped with the generation that had ended. The fence would
// be defeated by the mechanism meant to make it complete. A UUID minted per
// ServiceLock removes the case: the successor cannot see the predecessor's
// node as its own.
func TestAcquireCannotAdoptANodeLeftByAnEarlierLock(t *testing.T) {
	f := newFakeZK()
	old := newTestLock(t, f)
	deadID, err := old.Acquire(context.Background(), testLockData(t, old))
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	oldNode := old.Node()
	oldPath := path.Join(testLockPath(), oldNode)

	// The generation ends with its node still in the directory: a session
	// event reaches the watcher before ZooKeeper removes the ephemerals.
	lost := make(chan error, 1)
	go func() { lost <- old.Maintain(context.Background()) }()
	waitArmed(t, f, oldPath)
	f.fire(oldPath, gozk.Event{Type: gozk.EventSession, State: gozk.StateExpired, Path: oldPath})
	if err := <-lost; !errors.Is(err, ErrLockLost) {
		t.Fatalf("Maintain: want ErrLockLost, got %v", err)
	}
	if !f.exists(oldPath) {
		t.Fatal("the dead generation's node has to survive for this test to mean anything")
	}

	rejoined := newTestLock(t, f)
	// The rejoin queues behind the node the dead generation left, rather than
	// taking it: the node is not under the rejoin's prefix, so it is somebody
	// else's holder as far as this lock is concerned. Only once it goes can
	// the rejoin take the lock, which is the ordering ZooKeeper enforces and
	// the fence relies on.
	rejoinDone := make(chan LockID, 1)
	rejoinErr := make(chan error, 1)
	go func() {
		id, err := rejoined.Acquire(context.Background(), testLockData(t, rejoined))
		rejoinErr <- err
		rejoinDone <- id
	}()
	waitArmed(t, f, oldPath)
	queued := f.lockNodes(testLockPath())
	if len(queued) != 2 || queued[0] != oldNode {
		t.Fatalf("queue = %v, want the dead generation's %s still at the head", queued, oldNode)
	}
	if err := f.Delete(oldPath, -1); err != nil {
		t.Fatalf("delete the dead generation's node: %v", err)
	}
	if err := <-rejoinErr; err != nil {
		t.Fatalf("rejoin: %v", err)
	}
	liveID := <-rejoinDone

	if rejoined.Node() == oldNode {
		t.Fatalf("the rejoin adopted %s, the node of the generation that already ended", oldNode)
	}
	if liveID.Equal(deadID) {
		t.Fatalf("rejoined as %s, the identity the dead generation had: a request stamped with "+
			"the dead generation would now be accepted", liveID)
	}
	if !strings.HasPrefix(rejoined.Node(), rejoined.nodePrefix()) {
		t.Fatalf("node %q is not under the rejoined lock's own prefix %q", rejoined.Node(), rejoined.nodePrefix())
	}

	// And the payload the manager reads is the rejoin's, not the one the dead
	// generation left in the node it did not adopt.
	stored, ok := f.node(path.Join(testLockPath(), rejoined.Node()))
	if !ok {
		t.Fatal("the rejoined lock node was not created")
	}
	published, err := DecodeServiceLockData(stored.data)
	if err != nil {
		t.Fatalf("DecodeServiceLockData: %v", err)
	}
	for _, descriptor := range published.Descriptors {
		if descriptor.UUID != rejoined.UUID() {
			t.Fatalf("the rejoin advertises %s, the identity of the generation that ended", descriptor.UUID)
		}
	}
}

// TestAcquireCollapsesADuplicateUnderItsOwnPrefix is the recovery for a node
// this lock created and never learned the name of. Acquisition adopts the
// lowest under its prefix and drops the rest, so a duplicate cannot go on
// holding a second place in line.
func TestAcquireCollapsesADuplicateUnderItsOwnPrefix(t *testing.T) {
	f := newFakeZK()
	f.duplicates = 2

	lock := newTestLock(t, f)
	if _, err := lock.Acquire(context.Background(), testLockData(t, lock)); err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	var mine []string
	for _, child := range f.lockNodes(testLockPath()) {
		if strings.HasPrefix(child, lock.nodePrefix()) {
			mine = append(mine, child)
		}
	}
	if len(mine) != 1 || mine[0] != lock.Node() {
		t.Fatalf("nodes under this lock's prefix = %v, want only the adopted %s", mine, lock.Node())
	}
	if err := lock.Verify(); err != nil {
		t.Fatalf("Verify: %v", err)
	}
}

// TestReleaseAfterACancelledAcquisitionLeavesTheGenerationThatReplacedItAlone
// closes the same fence on an acquisition that never reached the directory. A
// cancelled Acquire returns before it creates anything, and a caller that
// treats Release as its shutdown path calls it anyway. The sweep it runs is
// unconditional, so the only thing keeping it off the rejoin's node is that
// the rejoin minted a prefix of its own.
func TestReleaseAfterACancelledAcquisitionLeavesTheGenerationThatReplacedItAlone(t *testing.T) {
	f := newFakeZK()
	old := newTestLock(t, f)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := old.Acquire(ctx, testLockData(t, old)); !errors.Is(err, context.Canceled) {
		t.Fatalf("Acquire: want context.Canceled, got %v", err)
	}
	if nodes := f.lockNodes(testLockPath()); len(nodes) != 0 {
		t.Fatalf("the cancelled acquisition created %v, so it is not the path this test means to cover", nodes)
	}

	rejoined := newTestLock(t, f)
	id, err := rejoined.Acquire(context.Background(), testLockData(t, rejoined))
	if err != nil {
		t.Fatalf("rejoin: %v", err)
	}
	newPath := path.Join(testLockPath(), rejoined.Node())

	if err := old.Release(); err != nil {
		t.Fatalf("Release of the cancelled acquisition: %v", err)
	}

	if !f.exists(newPath) {
		t.Fatalf("%s was swept by the release of an acquisition that never created a node", newPath)
	}
	held, ok := rejoined.LockID()
	if !ok || held != id {
		t.Fatalf("LockID() = %s, %t, want the generation that rejoined (%s)", held, ok, id)
	}
}

// TestReleaseAfterAFailedDirectorySetupLeavesTheGenerationThatReplacedItAlone
// is the same fence on the other pre-node exit. A directory this process
// cannot create is the ordinary shape of a session that has not been given the
// instance secret yet, and retrying it is what a server does; the attempt that
// failed must not be able to sweep the one that succeeded.
func TestReleaseAfterAFailedDirectorySetupLeavesTheGenerationThatReplacedItAlone(t *testing.T) {
	f := newFakeZK()
	old := newTestLock(t, f)
	f.failCreate(path.Join(testInstancePath, "tservers"), gozk.ErrNoAuth)
	if _, err := old.Acquire(context.Background(), testLockData(t, old)); !errors.Is(err, gozk.ErrNoAuth) {
		t.Fatalf("Acquire: want the ZooKeeper error, got %v", err)
	}
	f.failCreate(path.Join(testInstancePath, "tservers"), nil)

	rejoined := newTestLock(t, f)
	id, err := rejoined.Acquire(context.Background(), testLockData(t, rejoined))
	if err != nil {
		t.Fatalf("retry: %v", err)
	}
	newPath := path.Join(testLockPath(), rejoined.Node())

	if err := old.Release(); err != nil {
		t.Fatalf("Release of the failed attempt: %v", err)
	}

	if !f.exists(newPath) {
		t.Fatalf("%s was swept by the release of an attempt that never reached the lock directory", newPath)
	}
	if err := rejoined.Verify(); err != nil {
		t.Fatalf("Verify after the failed attempt was released: %v", err)
	}
	held, ok := rejoined.LockID()
	if !ok || held != id {
		t.Fatalf("LockID() = %s, %t, want the generation that retried (%s)", held, ok, id)
	}
}

// TestAcquireFailsWhenADuplicateCannotBeDropped is the other half of that
// story, at the moment acquisition finds the duplicates. Reporting success
// with one still standing would leave this session holding two places in line
// and maintaining one of them: when the adopted node goes, the survivor
// becomes the lowest in the directory and so the holder the manager sees, a
// server that looks alive and refuses every request because the generation
// this process was fenced to has ended. Failing hands it to the cleanup that
// says only closing the session will clear it.
func TestAcquireFailsWhenADuplicateCannotBeDropped(t *testing.T) {
	f := newFakeZK()
	f.duplicates = 2
	lock := newTestLock(t, f)

	duplicate := path.Join(testLockPath(), fmt.Sprintf("%s%s#%010d", zLockPrefix, lock.UUID(), 1))
	f.failDelete(duplicate, gozk.ErrConnectionClosed)

	_, err := lock.Acquire(context.Background(), testLockData(t, lock))
	if err == nil {
		t.Fatal("Acquire reported success while a duplicate of this session was still queued")
	}
	if !errors.Is(err, ErrLockNodeOrphaned) {
		t.Fatalf("Acquire = %v, want the surviving node reported as ErrLockNodeOrphaned", err)
	}
	if !errors.Is(err, gozk.ErrConnectionClosed) {
		t.Fatalf("Acquire = %v, want the delete failure that caused it", err)
	}
	if _, held := lock.LockID(); held {
		t.Fatal("no generation may be adopted while a duplicate of it is queued")
	}
	if !f.exists(duplicate) {
		t.Fatal("the duplicate must survive for this test to mean anything")
	}
	// The node this process would have held is gone: the cleanup took back
	// everything it could, so the only thing left is the one it reported.
	adopted := path.Join(testLockPath(), fmt.Sprintf("%s%s#%010d", zLockPrefix, lock.UUID(), 0))
	if f.exists(adopted) {
		t.Fatalf("%s outlived a failed acquisition", adopted)
	}
}

// TestAcquireReportsANodeItCouldNotRemove is the honesty requirement on
// cleanup. A cancelled acquisition whose node survives keeps its sequence, and
// so its place in line, for as long as the session lives; only the session's
// owner can close the session, so the owner has to be told rather than left to
// discover a lock held by nobody.
func TestAcquireReportsANodeItCouldNotRemove(t *testing.T) {
	f := newFakeZK()
	holder := f.seedForeignLock(testLockPath(), otherUUID, 0)
	lock := newTestLock(t, f)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	errs := make(chan error, 1)
	go func() {
		_, err := lock.Acquire(ctx, testLockData(t, lock))
		errs <- err
	}()

	own := nextArmed(t, f)
	waitArmed(t, f, path.Join(testLockPath(), holder))
	f.failDelete(own, gozk.ErrConnectionClosed)
	cancel()

	err := <-errs
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Acquire = %v, want it to still report the cancellation it was given", err)
	}
	if !errors.Is(err, ErrLockNodeOrphaned) {
		t.Fatalf("Acquire = %v, want it to also report the node it could not remove", err)
	}
	if !f.exists(own) {
		t.Fatal("the orphaned node must still be there; otherwise the report is wrong")
	}
}

// TestAcquireDoesNotReportAnOrphanItCleanedUp keeps the ordinary cancellation
// exactly comparable to context.Canceled. Joining an error onto every
// cancellation would make callers that compare against it stop working, so the
// orphan report has to be the exception rather than the rule.
func TestAcquireDoesNotReportAnOrphanItCleanedUp(t *testing.T) {
	f := newFakeZK()
	holder := f.seedForeignLock(testLockPath(), otherUUID, 0)
	lock := newTestLock(t, f)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	errs := make(chan error, 1)
	go func() {
		_, err := lock.Acquire(ctx, testLockData(t, lock))
		errs <- err
	}()
	nextArmed(t, f)
	waitArmed(t, f, path.Join(testLockPath(), holder))
	cancel()

	err := <-errs
	if err != context.Canceled { //nolint:errorlint // the identity is the point
		t.Fatalf("Acquire = %v (%T), want exactly context.Canceled", err, err)
	}
	if errors.Is(err, ErrLockNodeOrphaned) {
		t.Fatal("a node that was cleaned up must not be reported as orphaned")
	}
}

// TestReleaseReportsANodeItCouldNotRemove applies the same honesty to the
// shutdown path: a release that cannot prove the node is gone must not report
// success, because the manager reads that node to decide this server is alive.
func TestReleaseReportsANodeItCouldNotRemove(t *testing.T) {
	f := newFakeZK()
	lock := newTestLock(t, f)
	id, err := lock.Acquire(context.Background(), testLockData(t, lock))
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	f.failDelete(path.Join(testLockPath(), lock.Node()), gozk.ErrConnectionClosed)

	if err := lock.Release(); !errors.Is(err, gozk.ErrConnectionClosed) {
		t.Fatalf("Release = %v, want the delete failure", err)
	}
	if _, held := lock.LockID(); held {
		t.Fatalf("%s must not be reported as held after Release", id)
	}
}

// TestReleaseReportsADirectoryItCouldNotList is the same refusal one step
// earlier: a release that cannot even list the directory cannot know whether
// it left a node behind, so it must not claim it did not.
func TestReleaseReportsADirectoryItCouldNotList(t *testing.T) {
	f := newFakeZK()
	lock := newTestLock(t, f)
	if _, err := lock.Acquire(context.Background(), testLockData(t, lock)); err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	f.failChildren(testLockPath(), gozk.ErrConnectionClosed)

	if err := lock.Release(); !errors.Is(err, gozk.ErrConnectionClosed) {
		t.Fatalf("Release = %v, want the listing failure", err)
	}
}

// TestReleaseBeforeTheNodeExistsCreatesNothing covers the earliest release
// there is: the caller gives up between starting the acquisition and the node
// being created. Nothing may be left in the lock directory afterwards, because
// a node created after the caller has been told the lock is gone is a node
// nobody will ever come back for.
func TestReleaseBeforeTheNodeExistsCreatesNothing(t *testing.T) {
	f := newFakeZK()
	lock := newTestLock(t, f)

	f.beforeCreate = func(string) {
		f.beforeCreate = nil
		_ = lock.Release()
	}

	_, err := lock.Acquire(context.Background(), testLockData(t, lock))
	if !errors.Is(err, ErrLockReleased) {
		t.Fatalf("Acquire = %v, want ErrLockReleased", err)
	}
	for _, child := range f.lockNodes(testLockPath()) {
		if strings.HasPrefix(child, lock.nodePrefix()) {
			t.Fatalf("%s was created after the lock was released", child)
		}
	}
	if _, held := lock.LockID(); held {
		t.Fatal("a lock released before it was created must never be held")
	}
}

// TestReleaseWakesAWaitNothingElseWould isolates the wake-up itself. The
// release cannot delete this process's node here, so no watch fires: if the
// wait is not woken by the release directly it is not woken at all.
func TestReleaseWakesAWaitNothingElseWould(t *testing.T) {
	f := newFakeZK()
	holder := f.seedForeignLock(testLockPath(), otherUUID, 0)
	lock := newTestLock(t, f)

	errs := make(chan error, 1)
	go func() {
		_, err := lock.Acquire(context.Background(), testLockData(t, lock))
		errs <- err
	}()
	own := nextArmed(t, f)
	waitArmed(t, f, path.Join(testLockPath(), holder))
	f.failDelete(own, gozk.ErrConnectionClosed)

	if err := lock.Release(); !errors.Is(err, gozk.ErrConnectionClosed) {
		t.Fatalf("Release = %v, want the delete failure", err)
	}
	select {
	case err := <-errs:
		if !errors.Is(err, ErrLockReleased) {
			t.Fatalf("Acquire = %v, want ErrLockReleased", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("nothing woke the wait, so only the release could have")
	}
}

// TestAcquireRefusesToTakeALockAlreadyReleased drives the release into the
// last gap there is: after the queue has been read and this process is at the
// front of it, but before ownership is recorded. Taking the lock there would
// leave a caller that was told the lock was gone holding it.
func TestAcquireRefusesToTakeALockAlreadyReleased(t *testing.T) {
	f := newFakeZK()
	holder := f.seedForeignLock(testLockPath(), otherUUID, 0)
	lock := newTestLock(t, f)

	errs := make(chan error, 1)
	go func() {
		_, err := lock.Acquire(context.Background(), testLockData(t, lock))
		errs <- err
	}()
	own := nextArmed(t, f)
	waitArmed(t, f, path.Join(testLockPath(), holder))

	// The release must not remove the node, or the wait would trip over the
	// missing node instead of reaching the front of the queue.
	f.failDelete(own, gozk.ErrConnectionClosed)
	f.beforeChildren = func(string) {
		f.beforeChildren = nil
		_ = lock.Release()
	}
	if err := f.Delete(path.Join(testLockPath(), holder), -1); err != nil {
		t.Fatalf("delete the holder's node: %v", err)
	}

	select {
	case err := <-errs:
		if !errors.Is(err, ErrLockReleased) {
			t.Fatalf("Acquire = %v, want ErrLockReleased", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Acquire never finished")
	}
	if _, held := lock.LockID(); held {
		t.Fatal("acquisition must not record a lock the caller already released")
	}
}

// TestAcquireFailsWhenItsNodeVanishesBeforeTheWatch covers the window between
// reading the queue and watching this process's own place in it. Establishing
// a watch on a node that is already gone proves nothing, so the acquisition
// has to fail rather than wait on an event that will never come.
func TestAcquireFailsWhenItsNodeVanishesBeforeTheWatch(t *testing.T) {
	f := newFakeZK()
	f.seedForeignLock(testLockPath(), otherUUID, 0)
	lock := newTestLock(t, f)

	f.beforeGet = func(znodePath string) {
		if !strings.HasPrefix(path.Base(znodePath), lock.nodePrefix()) {
			return
		}
		f.beforeGet = nil
		if err := f.Delete(znodePath, -1); err != nil {
			panic(err)
		}
	}

	_, err := lock.Acquire(context.Background(), testLockData(t, lock))
	if !errors.Is(err, ErrLockNodeMissing) {
		t.Fatalf("Acquire = %v, want ErrLockNodeMissing", err)
	}
	if _, held := lock.LockID(); held {
		t.Fatal("a lock whose node vanished must not be held")
	}
}

// TestAcquireRefusesDescriptorsThatNameAnotherServer keeps the two halves of a
// server's identity together. The node name is what a generation is fenced by
// and the descriptor is what a client dials; publishing one server's
// descriptors from another's lock node would make the manager route work to a
// server this process is not fencing on behalf of.
func TestAcquireRefusesDescriptorsThatNameAnotherServer(t *testing.T) {
	f := newFakeZK()
	lock := newTestLock(t, f)

	_, err := lock.Acquire(context.Background(), testLockDataFor(t, otherUUID))
	if !errors.Is(err, ErrInvalidLockData) {
		t.Fatalf("Acquire = %v, want ErrInvalidLockData", err)
	}
	if !strings.Contains(err.Error(), otherUUID) || !strings.Contains(err.Error(), lock.UUID()) {
		t.Fatalf("Acquire = %v, want both server names so the mismatch is diagnosable", err)
	}
	if created := f.createdPaths(); len(created) != 0 {
		t.Fatalf("a refused advertisement created %v", created)
	}
	if _, held := lock.LockID(); held {
		t.Fatal("a refused advertisement must not leave a lock held")
	}
}

// TestAQueuedCandidateKeepsOneWatchOnItsOwnNode is the queued counterpart of
// TestMaintainKeepsExactlyOneWatchOutstanding. A ZooKeeper watch stays
// registered until it fires, and this process's own node does not fire while
// it is climbing the queue — so re-arming it on every pass would leave a
// registration behind at every place it stood, and a server joining a busy
// resource group would pay for the whole queue.
func TestAQueuedCandidateKeepsOneWatchOnItsOwnNode(t *testing.T) {
	const thirdUUID = "3f9c0d21-5b47-4e08-9a12-6c8de4f70b35"

	f := newFakeZK()
	dir := testLockPath()
	f.seed(dir, nil, false)
	predecessors := make([]string, 0, 3)
	for i, holder := range []string{otherUUID, managerUUID, thirdUUID} {
		predecessors = append(predecessors, path.Join(dir, f.seedForeignLock(dir, holder, int32(i))))
	}

	lock := newTestLock(t, f)
	ours := lockNodePath(lock.UUID(), len(predecessors))
	acquired := make(chan error, 1)
	go func() {
		_, err := lock.Acquire(context.Background(), testLockData(t, lock))
		acquired <- err
	}()

	// A candidate watches the node immediately ahead of it, so the queue is
	// climbed nearest-first: each departure moves this one up a place and
	// starts another pass.
	for gone := range predecessors {
		ahead := predecessors[len(predecessors)-1-gone]
		waitFor(t, "the candidate to watch "+path.Base(ahead), func() bool {
			return f.watchCount(ahead) == 1
		})
		// The own-node watch is armed before the one ahead, so by the time
		// that one exists this pass has done all the arming it will do.
		if got := f.watchCount(ours); got != 1 {
			t.Fatalf("own node watched %d times after %d places moved up: want exactly 1", got, gone)
		}
		if err := f.Delete(ahead, -1); err != nil {
			t.Fatalf("Delete(%s): %v", ahead, err)
		}
	}
	if err := <-acquired; err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	if got := f.watchCount(ours); got > 1 {
		t.Fatalf("own node watched %d times after acquiring: want at most 1", got)
	}
}

// TestMaintainInheritsTheWatchTheQueuedAcquisitionArmed continues the one-watch
// invariant across the handover from Acquire to Maintain. A candidate that
// waited in the queue armed a watch on its own node and still holds it when it
// reaches the front, so a Maintain that armed its own would leave that one
// registered on the client and the server for as long as the generation lasts.
//
// Reusing it is also what makes the handover fail closed for free: a node
// deleted between the directory read that decided ownership and the first
// Maintain has already fired the inherited watch, so the ending is delivered
// rather than looked for.
func TestMaintainInheritsTheWatchTheQueuedAcquisitionArmed(t *testing.T) {
	f := newFakeZK()
	dir := testLockPath()
	f.seed(dir, nil, false)
	ahead := path.Join(dir, f.seedForeignLock(dir, otherUUID, 0))

	lock := newTestLock(t, f)
	ours := lockNodePath(lock.UUID(), 1)
	acquired := make(chan error, 1)
	go func() {
		_, err := lock.Acquire(context.Background(), testLockData(t, lock))
		acquired <- err
	}()

	// Queued: the own-node watch is armed before the one ahead, so waiting for
	// the node ahead to be watched proves both exist.
	waitArmed(t, f, ahead)
	if got := f.watchCount(ours); got != 1 {
		t.Fatalf("own node watched %d times while queued, want the one the queue arms", got)
	}
	if err := f.Delete(ahead, -1); err != nil {
		t.Fatalf("Delete(%s): %v", ahead, err)
	}
	if err := <-acquired; err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	if got := f.watchCount(ours); got != 1 {
		t.Fatalf("own node watched %d times after acquiring, want the queue's watch kept", got)
	}

	var rearmed atomic.Bool
	f.beforeGet = func(znodePath string) {
		if znodePath == ours {
			rearmed.Store(true)
		}
	}
	watched := make(chan error, 1)
	go func() { watched <- lock.Maintain(context.Background()) }()
	if err := f.Delete(ours, -1); err != nil {
		t.Fatalf("Delete(%s): %v", ours, err)
	}
	if err := <-watched; !errors.Is(err, ErrLockLost) {
		t.Fatalf("Maintain: want ErrLockLost, got %v", err)
	}
	if lock.LossReason() != LossNodeDeleted {
		t.Fatalf("loss reason %s, want NODE_DELETED", lock.LossReason())
	}
	if rearmed.Load() {
		t.Fatal("Maintain armed a second watch instead of inheriting the one the " +
			"acquisition left armed on the same node")
	}
}

// TestAReleaseAtTheHandoverIsNotReportedAsAnAcquisition closes the last window
// in which Acquire could report a lock it does not have.
//
// Committing ownership and returning it are not one step. acquired records the
// generation under l.mu and lets go of it, and the handover that follows takes
// a different lock. A Release arriving in between finds a held generation,
// ends it as a release, deletes the node and returns — and the acquisition
// then walks out of a directory it no longer occupies with the LockID it was
// briefly given. A caller that fenced a Host on that LockID would host against
// a generation that was over before it started, and nothing downstream would
// say so, because the loss it would wait for has already been recorded.
//
// The window is real rather than theoretical: the handover blocks on watchMu,
// which any Maintain already running holds while it arms. Holding it here is
// how the race is made deterministic rather than how it is created.
//
// A release is reported however far the acquisition got, which is what
// waitForOwnership has always promised for a release landing mid-wait. The
// promise now covers the one moment the wait had already finished.
func TestAReleaseAtTheHandoverIsNotReportedAsAnAcquisition(t *testing.T) {
	f := newFakeZK()
	dir := testLockPath()
	f.seed(dir, nil, false)
	ahead := path.Join(dir, f.seedForeignLock(dir, otherUUID, 0))

	lock := newTestLock(t, f)
	ours := lockNodePath(lock.UUID(), 1)

	// Stop the acquisition between committing ownership and returning it.
	// adoptWatch is the only step in Acquire's path that takes watchMu, so
	// holding it parks the goroutine exactly in the window under test.
	lock.watchMu.Lock()

	type outcome struct {
		id  LockID
		err error
	}
	acquired := make(chan outcome, 1)
	go func() {
		id, err := lock.Acquire(context.Background(), testLockData(t, lock))
		acquired <- outcome{id: id, err: err}
	}()

	// Queued behind the stranger, then first in line when it goes.
	waitArmed(t, f, ahead)
	if err := f.Delete(ahead, -1); err != nil {
		t.Fatalf("Delete(%s): %v", ahead, err)
	}
	// Node() answers only for a generation that is held, so this is the
	// acquisition telling us it has committed and is now in the handover.
	waitFor(t, "the acquisition to commit ownership", func() bool {
		return lock.Node() != ""
	})

	if err := lock.Release(); err != nil {
		t.Fatalf("Release: %v", err)
	}
	lock.watchMu.Unlock()

	got := <-acquired
	if !errors.Is(got.err, ErrLockReleased) {
		t.Fatalf("Acquire returned id %v and error %v, want ErrLockReleased for a "+
			"generation the release had already ended", got.id, got.err)
	}
	if got.id != (LockID{}) {
		t.Fatalf("Acquire returned LockID %v alongside its refusal", got.id)
	}
	if node := lock.Node(); node != "" {
		t.Fatalf("lock still reports holding %q after the release", node)
	}
	if lock.LossReason() != LossReleased {
		t.Fatalf("loss reason %s, want RELEASED", lock.LossReason())
	}
	if f.exists(ours) {
		t.Fatalf("%s survived the release", ours)
	}
}

// TestAnAbandonedAcquisitionArmsNoWatchOnAnotherCandidate covers the one watch
// this package cannot take back. A ZooKeeper watch is released only by firing:
// go-zookeeper has no removeWatches, and it re-registers the outstanding ones
// after a reconnect, so a watch armed on another candidate's node lives until
// that node goes. This lock's own node is not exposed to that — the cleanup
// deletes the node its watch sits on — but a holder that stays put would
// collect one registration from every attempt that gave up beneath it.
//
// So an acquisition that is already over arms nothing. What it cannot avoid is
// the watch it was parked on when it was told to stop, which is why the wait
// is entered only once per predecessor.
//
// The two endings reach that through different exits. Cancellation leaves the
// queue intact, so the guard before the arm is the only thing standing between
// a dead acquisition and a registration on a stranger's node — that case fails
// without it. A release takes this lock's node away first, so the pass ends at
// the missing node and never reaches the arm; it is here because the property
// asserted is the end state, which has to hold however the acquisition ended.
func TestAnAbandonedAcquisitionArmsNoWatchOnAnotherCandidate(t *testing.T) {
	for _, tc := range []struct {
		name string
		stop func(*ServiceLock, context.CancelFunc)
		want error
	}{
		{
			name: "cancelled",
			stop: func(_ *ServiceLock, cancel context.CancelFunc) { cancel() },
			want: context.Canceled,
		},
		{
			name: "released",
			stop: func(lock *ServiceLock, _ context.CancelFunc) { _ = lock.Release() },
			want: ErrLockReleased,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFakeZK()
			dir := testLockPath()
			f.seed(dir, nil, false)
			ahead := path.Join(dir, f.seedForeignLock(dir, otherUUID, 0))

			lock := newTestLock(t, f)
			ours := lockNodePath(lock.UUID(), 1)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()

			// The own-node watch is armed one statement before the one ahead,
			// so ending the acquisition here lands in the window where it has
			// work left to do and no reason left to do it.
			var once sync.Once
			f.beforeGet = func(watched string) {
				if watched != ours {
					return
				}
				once.Do(func() { tc.stop(lock, cancel) })
			}

			_, err := lock.Acquire(ctx, testLockData(t, lock))
			if !errors.Is(err, tc.want) {
				t.Fatalf("Acquire: want %v, got %v", tc.want, err)
			}
			if got := f.watchCount(ahead); got != 0 {
				t.Fatalf("%d watches left on %s, a node this lock never owned and "+
					"cannot take a watch back from", got, ahead)
			}
		})
	}
}

// TestReleaseRecordsItsEndingBeforeTheNodeGoes pins which ending a deliberate
// release is remembered by. The delete is what a watching Maintain sees, so a
// watcher that reached the loss first would record an external NODE_DELETED
// for a node this process took away on purpose — and since the first ending
// recorded is the one kept, the answer would depend on who won the race.
func TestReleaseRecordsItsEndingBeforeTheNodeGoes(t *testing.T) {
	f := newFakeZK()
	lock := newTestLock(t, f)
	if _, err := lock.Acquire(context.Background(), testLockData(t, lock)); err != nil {
		t.Fatalf("Acquire: %v", err)
	}

	var watcher error
	f.beforeDelete = func(znodePath string) {
		if !strings.HasPrefix(path.Base(znodePath), zLockPrefix) {
			return
		}
		f.beforeDelete = nil
		// Stands in for Maintain reacting to the deletion: this is the call
		// its watch-event path makes, at the earliest moment a watcher could
		// make it — the delete has been asked for but has not landed.
		watcher = lock.lose(LossNodeDeleted, nil)
	}

	if err := lock.Release(); err != nil {
		t.Fatalf("Release: %v", err)
	}
	if watcher == nil {
		t.Fatal("the stand-in watcher never ran, so nothing was raced")
	}
	if !strings.Contains(watcher.Error(), LossReleased.String()) {
		t.Fatalf("watcher saw %v, want the deliberate %s ending", watcher, LossReleased)
	}
	if strings.Contains(watcher.Error(), LossNodeDeleted.String()) {
		t.Fatalf("watcher saw %v: a release was remembered as an external loss", watcher)
	}
}

// TestMaintainReusesTheWatchACancelledOneLeftBehind covers the restart the
// contract invites. Cancelling Maintain leaves the lock held, so a caller is
// expected to watch again — and a ZooKeeper watch stays registered until it
// fires, so arming a fresh one per call would leave the cancelled call's
// registration behind every time, which is the accumulation the one-watch
// invariant exists to prevent.
func TestMaintainReusesTheWatchACancelledOneLeftBehind(t *testing.T) {
	f := newFakeZK()
	lock := newTestLock(t, f)
	if _, err := lock.Acquire(context.Background(), testLockData(t, lock)); err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	nodePath := lockNodePath(lock.UUID(), 0)

	ctx, cancel := context.WithCancel(context.Background())
	stopped := make(chan error, 1)
	go func() { stopped <- lock.Maintain(ctx) }()
	waitArmed(t, f, nodePath)
	cancel()
	if err := <-stopped; !errors.Is(err, context.Canceled) {
		t.Fatalf("Maintain: want context.Canceled, got %v", err)
	}
	if got := f.watchCount(nodePath); got != 1 {
		t.Fatalf("%d watches outstanding after cancellation, want the one that was armed", got)
	}
	if _, held := lock.LockID(); !held {
		t.Fatal("cancelling the watch must not give the lock up")
	}

	var rearmed atomic.Bool
	f.beforeGet = func(znodePath string) {
		if znodePath == nodePath {
			rearmed.Store(true)
		}
	}

	resumed := make(chan error, 1)
	go func() { resumed <- lock.Maintain(context.Background()) }()
	if err := f.Delete(nodePath, -1); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if err := <-resumed; !errors.Is(err, ErrLockLost) {
		t.Fatalf("Maintain after the node went: want ErrLockLost, got %v", err)
	}
	if rearmed.Load() {
		t.Fatal("the resumed watcher armed a second watch instead of reusing the one it left behind")
	}
}

// TestConcurrentMaintainersAllReturnWhenTheGenerationEnds is the other half of
// sharing one watch: the event that ends a generation can be received by only
// one watcher — the client hands it to one receiver and closes the channel —
// so the others have to learn of the ending some other way. They are told, and
// they agree on how it ended.
//
// The wait below is for the shared watch to exist, and that is all it can
// prove: the watchers share one registration, so there is nothing per-watcher
// to count, and a watcher that has not reached its select when the node goes
// is indistinguishable from one that has. That case is covered instead by
// asking again after the generation has ended, which is the same question
// asked from the far side of the race — so what a watcher is told does not
// depend on when the scheduler ran it. Sharing itself is pinned exactly by
// TestMaintainReusesTheWatchACancelledOneLeftBehind.
func TestConcurrentMaintainersAllReturnWhenTheGenerationEnds(t *testing.T) {
	f := newFakeZK()
	lock := newTestLock(t, f)
	if _, err := lock.Acquire(context.Background(), testLockData(t, lock)); err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	nodePath := lockNodePath(lock.UUID(), 0)

	const watchers = 4
	ended := make(chan error, 2*watchers)
	for i := 0; i < watchers; i++ {
		go func() { ended <- lock.Maintain(context.Background()) }()
	}
	waitFor(t, "the shared watch to be armed", func() bool { return f.watchCount(nodePath) >= 1 })
	if err := f.Delete(nodePath, -1); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	// Late arrivals: every one of these starts after the ending was recorded,
	// which is the position a watcher the scheduler held back would be in.
	for i := 0; i < watchers; i++ {
		go func() { ended <- lock.Maintain(context.Background()) }()
	}
	for i := 0; i < 2*watchers; i++ {
		select {
		case err := <-ended:
			if !errors.Is(err, ErrLockLost) {
				t.Fatalf("watcher %d: want ErrLockLost, got %v", i, err)
			}
			if !strings.Contains(err.Error(), LossNodeDeleted.String()) {
				t.Fatalf("watcher %d saw %v, want the %s ending every watcher must agree on",
					i, err, LossNodeDeleted)
			}
		case <-time.After(10 * time.Second):
			t.Fatalf("watcher %d never returned after the generation ended", i)
		}
	}
}

// TestWatchingAfterTheGenerationEndedReportsHowItEnded pins the distinction
// that keeps the answer above independent of scheduling: a lock that ended is
// not a lock that was never held, and the two are reported differently.
func TestWatchingAfterTheGenerationEndedReportsHowItEnded(t *testing.T) {
	f := newFakeZK()
	lock := newTestLock(t, f)
	if _, err := lock.Acquire(context.Background(), testLockData(t, lock)); err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	nodePath := lockNodePath(lock.UUID(), 0)
	if err := f.Delete(nodePath, -1); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if err := lock.Verify(); !errors.Is(err, ErrLockLost) {
		t.Fatalf("Verify: want ErrLockLost, got %v", err)
	}

	for _, tc := range []struct {
		what string
		call func() error
	}{
		{"Maintain", func() error { return lock.Maintain(context.Background()) }},
		{"Verify", func() error { return lock.Verify() }},
	} {
		err := tc.call()
		if !errors.Is(err, ErrLockLost) {
			t.Fatalf("%s after the generation ended: want ErrLockLost, got %v", tc.what, err)
		}
		if errors.Is(err, ErrNotHeld) {
			t.Fatalf("%s reported the generation as one that was never held: %v", tc.what, err)
		}
		if !strings.Contains(err.Error(), LossNodeDeleted.String()) {
			t.Fatalf("%s reported %v, want the recorded %s ending", tc.what, err, LossNodeDeleted)
		}
	}

	// A lock that never held a generation is still told exactly that; there is
	// no ending to report, and inventing one would be worse than saying so.
	fresh := newTestLock(t, newFakeZK())
	if err := fresh.Maintain(context.Background()); !errors.Is(err, ErrNotHeld) {
		t.Fatalf("Maintain on a lock that never held one: want ErrNotHeld, got %v", err)
	}
	if err := fresh.Verify(); !errors.Is(err, ErrNotHeld) {
		t.Fatalf("Verify on a lock that never held one: want ErrNotHeld, got %v", err)
	}
}

// TestAnExpiredSessionOnAWatchIsALoss covers the defensive arm of the event
// classifier. go-zookeeper reports a session it gave up on as EventNotWatching
// and sends StateExpired on the connection's own event channel instead, so a
// watch carrying that state is not something the client produces — but a lock
// whose session is gone is one this process cannot prove it holds, and it is
// classified that way rather than treated as an event worth ignoring.
func TestAnExpiredSessionOnAWatchIsALoss(t *testing.T) {
	f := newFakeZK()
	lock := newTestLock(t, f)
	if _, err := lock.Acquire(context.Background(), testLockData(t, lock)); err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	err := lock.classifyEvent(gozk.Event{State: gozk.StateExpired, Path: lockNodePath(lock.UUID(), 0)})
	if !errors.Is(err, ErrLockLost) {
		t.Fatalf("classifyEvent: want ErrLockLost, got %v", err)
	}
	if lock.LossReason() != LossUnmonitorable {
		t.Fatalf("loss reason %s, want UNMONITORABLE", lock.LossReason())
	}
}
