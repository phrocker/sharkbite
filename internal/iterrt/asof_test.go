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

import "testing"

func initAsOf(t *testing.T, src SortedKeyValueIterator, opts map[string]string) *AsOfIterator {
	t.Helper()
	a := NewAsOfIterator()
	if err := a.Init(src, opts, IteratorEnvironment{Scope: ScopeScan}); err != nil {
		t.Fatalf("Init: %v", err)
	}
	if err := a.Seek(InfiniteRange(), nil, false); err != nil {
		t.Fatalf("Seek: %v", err)
	}
	return a
}

// asOfStream: coord r1/cf/a has three versions (ts 30,20,10), r1/cf/b one
// (ts=25), r2/cf/a one (ts=15). Versions arrive timestamp-descending.
func asOfStream() []kv {
	return []kv{
		{mk("r1", "cf", "a", "", 30), []byte("a-v30")},
		{mk("r1", "cf", "a", "", 20), []byte("a-v20")},
		{mk("r1", "cf", "a", "", 10), []byte("a-v10")},
		{mk("r1", "cf", "b", "", 25), []byte("b-v25")},
		{mk("r2", "cf", "a", "", 15), []byte("r2a-v15")},
	}
}

// TestAsOf_DropsNewerVersions: with ceiling 20, the ts=30 version of r1/cf/a
// and the ts=25 r1/cf/b cell are dropped; older versions survive.
func TestAsOf_DropsNewerVersions(t *testing.T) {
	a := initAsOf(t, newSliceSource(asOfStream()...), map[string]string{AsOfOption: "20"})
	got, err := drain(a)
	if err != nil {
		t.Fatalf("drain: %v", err)
	}
	want := []string{"a-v20", "a-v10", "r2a-v15"}
	if len(got) != len(want) {
		t.Fatalf("got %d cells, want %d: %v", len(got), len(want), valStrings(got))
	}
	for i, w := range want {
		if string(got[i].v) != w {
			t.Errorf("cell %d: got %q want %q", i, got[i].v, w)
		}
		if got[i].k.Timestamp > 20 {
			t.Errorf("cell %d ts %d exceeds ceiling 20", i, got[i].k.Timestamp)
		}
	}
}

// TestAsOf_Disabled: a non-positive / absent ceiling passes everything.
func TestAsOf_Disabled(t *testing.T) {
	for _, opts := range []map[string]string{nil, {AsOfOption: "0"}, {AsOfOption: "-5"}} {
		a := initAsOf(t, newSliceSource(asOfStream()...), opts)
		got, err := drain(a)
		if err != nil {
			t.Fatalf("drain: %v", err)
		}
		if len(got) != 5 {
			t.Fatalf("disabled ceiling should pass all 5, got %d (%v)", len(got), valStrings(got))
		}
	}
}

func TestAsOf_BadInteger(t *testing.T) {
	a := NewAsOfIterator()
	if err := a.Init(newSliceSource(asOfStream()...), map[string]string{AsOfOption: "notanint"}, IteratorEnvironment{}); err == nil {
		t.Fatal("expected error for malformed ceiling")
	}
}

// TestAsOf_StackWithDeletingVersioning is the real composition: a coordinate
// is written (ts=10), overwritten (ts=30), then deleted (ts=40). Scanning the
// [asOf, deleting, versioning] stack at three ceilings must reconstruct the
// coordinate's state at each instant.
func TestAsOf_StackWithDeletingVersioning(t *testing.T) {
	// Source order is timestamp-descending within the coordinate, tombstone
	// included as just another version (Deleting interprets it).
	stream := []kv{
		{mkDel("r1", "cf", "a", "", 40), []byte{}},
		{mk("r1", "cf", "a", "", 30), []byte("v30")},
		{mk("r1", "cf", "a", "", 10), []byte("v10")},
	}
	stack := func(ceiling string) SortedKeyValueIterator {
		specs := []IterSpec{
			{Name: IterAsOf, Options: map[string]string{AsOfOption: ceiling}},
			{Name: IterDeleting, Options: map[string]string{DeletingOptionPropagate: "false"}},
			{Name: IterVersioning, Options: map[string]string{VersioningOption: "1"}},
		}
		it, err := BuildStack(newSliceSource(stream...), specs, IteratorEnvironment{Scope: ScopeScan})
		if err != nil {
			t.Fatalf("BuildStack: %v", err)
		}
		if err := it.Seek(InfiniteRange(), nil, false); err != nil {
			t.Fatalf("Seek: %v", err)
		}
		return it
	}

	cases := []struct {
		ceiling string
		want    string // "" means coordinate absent (deleted or not yet written)
	}{
		{"15", "v10"}, // only the original write is visible
		{"35", "v30"}, // the overwrite is visible, delete not yet in effect
		{"50", ""},    // the delete (ts=40) has taken effect -> suppressed
	}
	for _, c := range cases {
		got, err := drain(stack(c.ceiling))
		if err != nil {
			t.Fatalf("ceiling %s drain: %v", c.ceiling, err)
		}
		if c.want == "" {
			if len(got) != 0 {
				t.Errorf("ceiling %s: expected coordinate suppressed, got %v", c.ceiling, valStrings(got))
			}
			continue
		}
		if len(got) != 1 || string(got[0].v) != c.want {
			t.Errorf("ceiling %s: got %v want [%s]", c.ceiling, valStrings(got), c.want)
		}
	}
}
