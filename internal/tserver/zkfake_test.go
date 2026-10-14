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
	"fmt"
	"path"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	gozk "github.com/go-zookeeper/zk"
)

const (
	testInstancePath = "/accumulo/1a2b3c4d-5e6f-4071-8293-a4b5c6d7e8f9"
	testAddress      = "shoal-1.example:9997"
	testGroup        = "default"
)

// testLockPath is the tablet-server lock directory the fakes register under.
// testGroup is a constant this package controls, so the path always resolves.
func testLockPath() string {
	dir, err := TabletServerLockPath(testInstancePath, testGroup, testAddress)
	if err != nil {
		panic(err)
	}
	return dir
}

type fakeNode struct {
	data      []byte
	acl       []gozk.ACL
	ephemeral bool
}

// fakeZK is an in-memory stand-in for a ZooKeeper session: a flat znode map
// with per-parent sequence counters, existence watches, ephemeral nodes, and
// injectable failures. It exists to drive the ServiceLock protocol through
// races and failures a real quorum would only produce by luck.
type fakeZK struct {
	mu       sync.Mutex
	nodes    map[string]*fakeNode
	sequence map[string]int32
	watches  map[string][]chan gozk.Event

	// armed reports the path of every established watch, so a test can act at
	// the moment the code under test is listening.
	armed chan string

	createErr map[string]error
	childErr  map[string]error
	getErr    map[string]error
	deleteErr map[string]error

	// duplicates makes the next sequential create leave that many extra nodes
	// behind under the same prefix and return the last one, reproducing a
	// client that retried a create whose answer it never saw.
	duplicates int

	creates []string
	deletes []string

	// beforeDelete runs before a delete is applied, so a test can observe the
	// order of operations against other state.
	beforeDelete func(path string)

	// beforeCreate runs before a create is applied, so a test can act in the
	// window between deciding to register and having registered.
	beforeCreate func(path string)

	// beforeGet runs before a node is read and watched, so a test can make it
	// vanish in the window between listing a directory and watching what it
	// found.
	beforeGet func(path string)

	// beforeChildren runs before a directory is listed, so a test can change
	// the tree in the window between creating a node and reading the queue.
	beforeChildren func(path string)
}

func newFakeZK(seeded ...string) *fakeZK {
	f := &fakeZK{
		nodes:     make(map[string]*fakeNode),
		sequence:  make(map[string]int32),
		watches:   make(map[string][]chan gozk.Event),
		armed:     make(chan string, 64),
		createErr: make(map[string]error),
		childErr:  make(map[string]error),
		getErr:    make(map[string]error),
		deleteErr: make(map[string]error),
	}
	for _, znode := range seeded {
		f.seed(znode, nil, false)
	}
	return f
}

// seed creates a znode and every missing ancestor without going through the
// Create path, so a test can describe a pre-existing tree.
func (f *fakeZK) seed(znodePath string, data []byte, ephemeral bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	segments := strings.Split(strings.Trim(znodePath, "/"), "/")
	current := ""
	for i, segment := range segments {
		current += "/" + segment
		if _, ok := f.nodes[current]; ok {
			continue
		}
		node := &fakeNode{}
		if i == len(segments)-1 {
			node.data = data
			node.ephemeral = ephemeral
		}
		f.nodes[current] = node
	}
}

// seedForeignLock adds a lock node owned by somebody else.
func (f *fakeZK) seedForeignLock(dir, holder string, sequence int32) string {
	name := fmt.Sprintf("%s%s#%010d", zLockPrefix, holder, sequence)
	f.seed(path.Join(dir, name), []byte("{}"), true)
	f.mu.Lock()
	if next := sequence + 1; f.sequence[dir] < next {
		f.sequence[dir] = next
	}
	f.mu.Unlock()
	return name
}

// recreate removes a directory and its children and puts the directory back
// empty. It is how ZooKeeper's sequential counter restarts: the counter lives
// on the parent, so a directory that is deleted and remade hands out numbers
// from zero again.
func (f *fakeZK) recreate(dir string) {
	f.mu.Lock()
	prefix := strings.TrimSuffix(dir, "/") + "/"
	for candidate := range f.nodes {
		if candidate == dir || strings.HasPrefix(candidate, prefix) {
			delete(f.nodes, candidate)
		}
	}
	delete(f.sequence, dir)
	f.mu.Unlock()
	f.seed(dir, nil, false)
}

func (f *fakeZK) Create(znodePath string, data []byte, flags int32, acl []gozk.ACL) (string, error) {
	if f.beforeCreate != nil {
		f.beforeCreate(znodePath)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.createErr[znodePath]; err != nil {
		return "", err
	}
	parent := path.Dir(znodePath)
	if parent != "/" {
		if _, ok := f.nodes[parent]; !ok {
			return "", gozk.ErrNoNode
		}
	}
	if flags&gozk.FlagSequence == 0 {
		if _, ok := f.nodes[znodePath]; ok {
			return "", gozk.ErrNodeExists
		}
		f.nodes[znodePath] = &fakeNode{
			data:      data,
			acl:       acl,
			ephemeral: flags&gozk.FlagEphemeral != 0,
		}
		f.creates = append(f.creates, znodePath)
		return znodePath, nil
	}
	created := ""
	for i := 0; i <= f.duplicates; i++ {
		created = fmt.Sprintf("%s%010d", znodePath, f.sequence[parent])
		f.sequence[parent]++
		f.nodes[created] = &fakeNode{
			data:      data,
			acl:       acl,
			ephemeral: flags&gozk.FlagEphemeral != 0,
		}
		f.creates = append(f.creates, created)
	}
	f.duplicates = 0
	return created, nil
}

func (f *fakeZK) Children(znodePath string) ([]string, *gozk.Stat, error) {
	if f.beforeChildren != nil {
		f.beforeChildren(znodePath)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.childErr[znodePath]; err != nil {
		return nil, nil, err
	}
	if _, ok := f.nodes[znodePath]; !ok {
		return nil, nil, gozk.ErrNoNode
	}
	prefix := strings.TrimSuffix(znodePath, "/") + "/"
	children := make([]string, 0, 4)
	for candidate := range f.nodes {
		if !strings.HasPrefix(candidate, prefix) {
			continue
		}
		name := candidate[len(prefix):]
		if strings.Contains(name, "/") {
			continue
		}
		children = append(children, name)
	}
	// ZooKeeper returns children in no particular order; shuffling by sorting
	// descending keeps the code under test from depending on arrival order.
	sort.Sort(sort.Reverse(sort.StringSlice(children)))
	return children, &gozk.Stat{}, nil
}

// GetW models go-zookeeper's read-with-watch, whose watch is registered only
// when the read succeeds: a missing node is ErrNoNode and leaves nothing
// behind. That is the difference the protocol depends on — an existence watch
// would be registered either way, and on a sequential node that is never
// created twice it would never fire.
func (f *fakeZK) GetW(znodePath string) ([]byte, *gozk.Stat, <-chan gozk.Event, error) {
	if f.beforeGet != nil {
		f.beforeGet(znodePath)
	}
	f.mu.Lock()
	if err := f.getErr[znodePath]; err != nil {
		f.mu.Unlock()
		return nil, nil, nil, err
	}
	node, exists := f.nodes[znodePath]
	if !exists {
		f.mu.Unlock()
		return nil, nil, nil, gozk.ErrNoNode
	}
	data := append([]byte(nil), node.data...)
	events := make(chan gozk.Event, 1)
	f.watches[znodePath] = append(f.watches[znodePath], events)
	f.mu.Unlock()

	select {
	case f.armed <- znodePath:
	default:
	}
	return data, &gozk.Stat{}, events, nil
}

func (f *fakeZK) Delete(znodePath string, _ int32) error {
	if f.beforeDelete != nil {
		f.beforeDelete(znodePath)
	}
	f.mu.Lock()
	if err := f.deleteErr[znodePath]; err != nil {
		f.mu.Unlock()
		return err
	}
	if _, ok := f.nodes[znodePath]; !ok {
		f.mu.Unlock()
		return gozk.ErrNoNode
	}
	delete(f.nodes, znodePath)
	f.deletes = append(f.deletes, znodePath)
	f.mu.Unlock()

	f.fire(znodePath, gozk.Event{
		Type:  gozk.EventNodeDeleted,
		State: gozk.StateHasSession,
		Path:  znodePath,
	})
	return nil
}

// fire delivers an event to every watch on a path and clears them, as a
// one-shot ZooKeeper watch does.
//
// The channel is closed after the event, which is what go-zookeeper does
// ("ch <- ev; close(ch)" in Conn.notifyWatches). It matters for anything with
// more than one receiver on a watch: production wakes the one that takes the
// event and every other waiter through the close, so a fake that left the
// channel open would leave those waiters blocked on a watch that has already
// fired.
func (f *fakeZK) fire(znodePath string, event gozk.Event) {
	f.mu.Lock()
	waiting := f.watches[znodePath]
	delete(f.watches, znodePath)
	f.mu.Unlock()
	for _, events := range waiting {
		events <- event
		close(events)
	}
}

// expire ends the session: every ephemeral node disappears and every watch is
// invalidated, which is what the client reports as EventNotWatching.
//
// The event matches go-zookeeper's Conn.invalidateWatches exactly — type
// EventNotWatching, state StateDisconnected, and the cause in Err — because
// the client reports the watch it gave up on, not the session state that made
// it give up. StateExpired reaches the caller on the connection's own event
// channel instead, which is not what a watch delivers.
func (f *fakeZK) expire() {
	f.mu.Lock()
	for znodePath, node := range f.nodes {
		if node.ephemeral {
			delete(f.nodes, znodePath)
			f.deletes = append(f.deletes, znodePath)
		}
	}
	waiting := f.watches
	f.watches = make(map[string][]chan gozk.Event)
	f.mu.Unlock()
	for znodePath, channels := range waiting {
		for _, events := range channels {
			events <- gozk.Event{
				Type:  gozk.EventNotWatching,
				State: gozk.StateDisconnected,
				Path:  znodePath,
				Err:   gozk.ErrSessionExpired,
			}
			close(events)
		}
	}
}

func (f *fakeZK) exists(znodePath string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	_, ok := f.nodes[znodePath]
	return ok
}

// watchCount returns how many existence watches are outstanding on a path. A
// one-shot watch stays registered until it fires, so this is what grows when a
// caller arms a watch it never consumes.
func (f *fakeZK) watchCount(znodePath string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.watches[znodePath])
}

func (f *fakeZK) node(znodePath string) (fakeNode, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	found, ok := f.nodes[znodePath]
	if !ok {
		return fakeNode{}, false
	}
	return *found, true
}

// lockNodes returns the sorted lock children of a directory.
func (f *fakeZK) lockNodes(dir string) []string {
	children, _, err := f.Children(dir)
	if err != nil {
		return nil
	}
	return sortLockNodes(children)
}

func (f *fakeZK) deletedPaths() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.deletes...)
}

func (f *fakeZK) createdPaths() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.creates...)
}

func (f *fakeZK) failChildren(znodePath string, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.childErr[znodePath] = err
}

func (f *fakeZK) failGet(znodePath string, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.getErr[znodePath] = err
}

func (f *fakeZK) failCreate(znodePath string, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.createErr[znodePath] = err
}

func (f *fakeZK) failDelete(znodePath string, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.deleteErr[znodePath] = err
}

// waitArmed blocks until an existence watch is established on znodePath, so a
// test can change the tree at the exact moment the code is listening for it.
func waitArmed(t *testing.T, f *fakeZK, znodePath string) {
	t.Helper()
	deadline := time.After(5 * time.Second)
	for {
		select {
		case armed := <-f.armed:
			if armed == znodePath {
				return
			}
		case <-deadline:
			t.Fatalf("no watch was established on %s", znodePath)
		}
	}
}
