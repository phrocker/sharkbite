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

package iterrt

import (
	"errors"
	"testing"
)

// streamWithDelete builds a stream where coord r1/cf/a has a tombstone at
// ts=20 and two live versions at ts=30 (newer than the delete) + ts=10
// (older). Per wire.Key ordering the source surfaces them in this order:
//
//	(r1,cf,a,ts=30, live)
//	(r1,cf,a,ts=20, DEL)
//	(r1,cf,a,ts=10, live)
//	(r1,cf,b,ts=5,  live)
//	(r2,cf,a,ts=9,  live)
func streamWithDelete() []kv {
	return []kv{
		{mk("r1", "cf", "a", "", 30), []byte("r1a-v30")},
		{mkDel("r1", "cf", "a", "", 20), []byte{}},
		{mk("r1", "cf", "a", "", 10), []byte("r1a-v10")},
		{mk("r1", "cf", "b", "", 5), []byte("r1b-v5")},
		{mk("r2", "cf", "a", "", 9), []byte("r2a-v9")},
	}
}

func initDel(t *testing.T, src SortedKeyValueIterator, opts map[string]string, env IteratorEnvironment) *DeletingIterator {
	t.Helper()
	d := NewDeletingIterator()
	if err := d.Init(src, opts, env); err != nil {
		t.Fatalf("Init: %v", err)
	}
	if err := d.Seek(InfiniteRange(), nil, false); err != nil {
		t.Fatalf("Seek: %v", err)
	}
	return d
}

// TestDeleting_ScanKeepsTombstone: at ScopeScan propagateDeletes is true,
// so the tombstone passes through. The live ts=10 cell still gets
// swallowed because the tombstone advance jumps past every same-coord
// cell that follows it.
func TestDeleting_ScanKeepsTombstone(t *testing.T) {
	d := initDel(t, newSliceSource(streamWithDelete()...), nil, IteratorEnvironment{Scope: ScopeScan})
	got, err := drain(d)
	if err != nil {
		t.Fatalf("drain: %v", err)
	}
	// ts=30 live, ts=20 DEL (emitted; swallows ts=10), r1b ts=5, r2a ts=9.
	want := []string{"r1a-v30", "", "r1b-v5", "r2a-v9"}
	if vs := valStrings(got); !eqStr(vs, want) {
		t.Fatalf("got %v want %v", vs, want)
	}
	if !got[1].k.Deleted {
		t.Fatalf("expected ts=20 cell to be the delete marker, got %+v", got[1].k)
	}
}

// TestDeleting_FullMajcDropsTombstone: ScopeMajc+FullMajorCompaction sets
// propagateDeletes=false. Tombstone is dropped AND every same-coord cell
// after it is dropped.
func TestDeleting_FullMajcDropsTombstone(t *testing.T) {
	env := IteratorEnvironment{Scope: ScopeMajc, FullMajorCompaction: true}
	d := initDel(t, newSliceSource(streamWithDelete()...), nil, env)
	got, err := drain(d)
	if err != nil {
		t.Fatalf("drain: %v", err)
	}
	// ts=30 live (came BEFORE the tombstone in stream order, so not swallowed),
	// r1b ts=5, r2a ts=9. ts=20 DEL + ts=10 live both gone.
	want := []string{"r1a-v30", "r1b-v5", "r2a-v9"}
	if vs := valStrings(got); !eqStr(vs, want) {
		t.Fatalf("got %v want %v", vs, want)
	}
}

// TestDeleting_PartialMajcKeepsTombstone: a non-full majc still propagates
// deletes (some older RFile not in this stack might hold the live cells).
func TestDeleting_PartialMajcKeepsTombstone(t *testing.T) {
	env := IteratorEnvironment{Scope: ScopeMajc, FullMajorCompaction: false}
	d := initDel(t, newSliceSource(streamWithDelete()...), nil, env)
	got, _ := drain(d)
	want := []string{"r1a-v30", "", "r1b-v5", "r2a-v9"}
	if vs := valStrings(got); !eqStr(vs, want) {
		t.Fatalf("got %v want %v", vs, want)
	}
}

// TestDeleting_PropagateOptionOverridesEnv: the per-iterator option wins
// over the env-derived default.
func TestDeleting_PropagateOptionOverridesEnv(t *testing.T) {
	env := IteratorEnvironment{Scope: ScopeMajc, FullMajorCompaction: true}
	d := initDel(t, newSliceSource(streamWithDelete()...),
		map[string]string{DeletingOptionPropagate: "true"}, env)
	got, _ := drain(d)
	// Override forces propagate=true at full-majc => behaves like scan.
	want := []string{"r1a-v30", "", "r1b-v5", "r2a-v9"}
	if vs := valStrings(got); !eqStr(vs, want) {
		t.Fatalf("got %v want %v", vs, want)
	}
}

// TestDeleting_BehaviorFail: with behavior=fail, encountering any delete
// errors out via ErrUnexpectedDelete.
func TestDeleting_BehaviorFail(t *testing.T) {
	env := IteratorEnvironment{Scope: ScopeScan}
	d := initDel(t, newSliceSource(streamWithDelete()...),
		map[string]string{DeletingOptionBehavior: "fail"}, env)
	var emitted []kv
	for d.HasTop() {
		emitted = append(emitted, kv{k: d.GetTopKey().Clone(), v: append([]byte(nil), d.GetTopValue()...)})
		if err := d.Next(); err != nil {
			t.Fatalf("Next: %v", err)
		}
	}
	if !errors.Is(d.Err(), ErrUnexpectedDelete) {
		t.Fatalf("expected ErrUnexpectedDelete latched on iterator, got err=%v after %d cells", d.Err(), len(emitted))
	}
	// The pre-delete cell should still have been emitted.
	if len(emitted) == 0 || string(emitted[0].v) != "r1a-v30" {
		t.Fatalf("expected r1a-v30 before the fail, got %v", valStrings(emitted))
	}
}

// TestDeleting_TombstoneAtNewestSwallowsAllOlder: tombstone is the newest
// version at the coord — every older live cell is suppressed. Tests the
// canonical "delete the row" use case.
func TestDeleting_TombstoneAtNewestSwallowsAllOlder(t *testing.T) {
	cells := []kv{
		{mkDel("r1", "cf", "a", "", 100), []byte{}},
		{mk("r1", "cf", "a", "", 50), []byte("old1")},
		{mk("r1", "cf", "a", "", 10), []byte("old2")},
		{mk("r1", "cf", "b", "", 5), []byte("r1b")},
	}
	d := initDel(t, newSliceSource(cells...), nil, IteratorEnvironment{Scope: ScopeMajc, FullMajorCompaction: true})
	got, _ := drain(d)
	want := []string{"r1b"}
	if vs := valStrings(got); !eqStr(vs, want) {
		t.Fatalf("got %v want %v", vs, want)
	}
}

// TestDeleting_NoDeletes: a stream with no tombstones is a pure
// passthrough at any scope.
func TestDeleting_NoDeletes(t *testing.T) {
	cells := []kv{
		{mk("r1", "cf", "a", "", 30), []byte("r1a-v30")},
		{mk("r2", "cf", "a", "", 9), []byte("r2a-v9")},
	}
	for _, env := range []IteratorEnvironment{
		{Scope: ScopeScan},
		{Scope: ScopeMinc},
		{Scope: ScopeMajc, FullMajorCompaction: true},
	} {
		d := initDel(t, newSliceSource(cells...), nil, env)
		got, _ := drain(d)
		want := []string{"r1a-v30", "r2a-v9"}
		if vs := valStrings(got); !eqStr(vs, want) {
			t.Errorf("scope=%s: got %v want %v", env.Scope, vs, want)
		}
	}
}

// TestDeleting_BuildStackRegistered: BuildStack accepts "deleting" by name
// and honors propagateDeletes option through the stack composer.
func TestDeleting_BuildStackRegistered(t *testing.T) {
	leaf := newSliceSource(streamWithDelete()...)
	stack, err := BuildStack(leaf, []IterSpec{
		{Name: IterDeleting, Options: map[string]string{DeletingOptionPropagate: "false"}},
	}, IteratorEnvironment{Scope: ScopeScan})
	if err != nil {
		t.Fatalf("BuildStack: %v", err)
	}
	if err := stack.Seek(InfiniteRange(), nil, false); err != nil {
		t.Fatalf("Seek: %v", err)
	}
	got, _ := drain(stack)
	// propagate=false override => tombstone + same-coord live cells dropped.
	want := []string{"r1a-v30", "r1b-v5", "r2a-v9"}
	if vs := valStrings(got); !eqStr(vs, want) {
		t.Fatalf("got %v want %v", vs, want)
	}
}

// TestDeleting_SeekMaximizeStart: when the user's seek start key falls
// mid-version-chain at a coord with a tombstone, the iterator must still
// process the tombstone (it sorts earlier in the chain). Mirrors the Java
// IteratorUtil.maximizeStartKeyTimeStamp contract.
func TestDeleting_SeekMaximizeStart(t *testing.T) {
	// Stream: r1/cf/a has tombstone at ts=50 + live ts=10 + live ts=5.
	// Wire order: tombstone (ts=50, del), live ts=10, live ts=5.
	cells := []kv{
		{mkDel("r1", "cf", "a", "", 50), []byte{}},
		{mk("r1", "cf", "a", "", 10), []byte("v10")},
		{mk("r1", "cf", "a", "", 5), []byte("v5")},
		{mk("r1", "cf", "b", "", 1), []byte("r1b")},
	}
	d := NewDeletingIterator()
	if err := d.Init(newSliceSource(cells...), nil,
		IteratorEnvironment{Scope: ScopeMajc, FullMajorCompaction: true}); err != nil {
		t.Fatalf("Init: %v", err)
	}
	// Seek to (r1,cf,a, ts=10) inclusive — without the maximize-start
	// rewrite the source would skip the tombstone at ts=50 and we'd
	// surface the live ts=10 cell instead of suppressing it.
	if err := d.Seek(Range{
		Start:          mk("r1", "cf", "a", "", 10),
		StartInclusive: true,
		InfiniteEnd:    true,
	}, nil, false); err != nil {
		t.Fatalf("Seek: %v", err)
	}
	got, _ := drain(d)
	want := []string{"r1b"}
	if vs := valStrings(got); !eqStr(vs, want) {
		t.Fatalf("got %v want %v — tombstone failed to swallow same-coord cells after a mid-chain seek", vs, want)
	}
}
