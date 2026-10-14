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

// versioned builds a cell stream with several coordinates, each carrying
// descending-timestamp versions — exactly the shape an RFile / merge
// hands a VersioningIterator.
func versioned() []kv {
	return []kv{
		{mk("r1", "cf", "a", "", 30), []byte("r1a-v30")},
		{mk("r1", "cf", "a", "", 20), []byte("r1a-v20")},
		{mk("r1", "cf", "a", "", 10), []byte("r1a-v10")},
		{mk("r1", "cf", "b", "", 5), []byte("r1b-v5")},
		{mk("r2", "cf", "a", "", 9), []byte("r2a-v9")},
		{mk("r2", "cf", "a", "", 8), []byte("r2a-v8")},
	}
}

func initVers(t *testing.T, src SortedKeyValueIterator, maxV string, scope IteratorScope) *VersioningIterator {
	t.Helper()
	v := NewVersioningIterator()
	opts := map[string]string{}
	if maxV != "" {
		opts[VersioningOption] = maxV
	}
	if err := v.Init(src, opts, IteratorEnvironment{Scope: scope}); err != nil {
		t.Fatalf("Init: %v", err)
	}
	if err := v.Seek(InfiniteRange(), nil, false); err != nil {
		t.Fatalf("Seek: %v", err)
	}
	return v
}

// TestVersioning_DefaultKeepsNewestOnly: default maxVersions=1 keeps only
// the newest version of every coordinate.
func TestVersioning_DefaultKeepsNewestOnly(t *testing.T) {
	v := initVers(t, newSliceSource(versioned()...), "", ScopeScan)
	got, err := drain(v)
	if err != nil {
		t.Fatalf("drain: %v", err)
	}
	want := []string{"r1a-v30", "r1b-v5", "r2a-v9"}
	if len(got) != len(want) {
		t.Fatalf("got %d cells, want %d: %v", len(got), len(want), valStrings(got))
	}
	for i, w := range want {
		if string(got[i].v) != w {
			t.Errorf("cell %d: got %q want %q", i, got[i].v, w)
		}
	}
}

// TestVersioning_KeepsNewestN: maxVersions=2 keeps the two newest
// versions per coordinate, in descending-timestamp order, and drops the
// rest.
func TestVersioning_KeepsNewestN(t *testing.T) {
	v := initVers(t, newSliceSource(versioned()...), "2", ScopeScan)
	got, err := drain(v)
	if err != nil {
		t.Fatalf("drain: %v", err)
	}
	want := []string{"r1a-v30", "r1a-v20", "r1b-v5", "r2a-v9", "r2a-v8"}
	if got2 := valStrings(got); !eqStr(got2, want) {
		t.Fatalf("got %v want %v", got2, want)
	}
}

// TestVersioning_MaxGreaterThanAvailable: maxVersions larger than any
// coordinate's version count surfaces every cell unchanged.
func TestVersioning_MaxGreaterThanAvailable(t *testing.T) {
	v := initVers(t, newSliceSource(versioned()...), "100", ScopeScan)
	got, err := drain(v)
	if err != nil {
		t.Fatalf("drain: %v", err)
	}
	if len(got) != 6 {
		t.Fatalf("got %d cells, want 6 (passthrough): %v", len(got), valStrings(got))
	}
}

// TestVersioning_DeletesCountAsVersions confirms the Java semantic: a
// tombstone is just another version. With maxVersions=1 and a tombstone
// as the newest version, the tombstone is what survives — VersioningIterator
// does NOT skip deletes (delete suppression is a separate iterator).
func TestVersioning_DeletesCountAsVersions(t *testing.T) {
	cells := []kv{
		{mkDel("r1", "cf", "a", "", 40), []byte("")},    // newest = tombstone
		{mk("r1", "cf", "a", "", 30), []byte("live30")}, // older live
		{mk("r1", "cf", "b", "", 5), []byte("r1b")},
	}
	v := initVers(t, newSliceSource(cells...), "1", ScopeMajc)
	got, err := drain(v)
	if err != nil {
		t.Fatalf("drain: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d cells, want 2: %v", len(got), valStrings(got))
	}
	if !got[0].k.Deleted {
		t.Errorf("cell 0 should be the tombstone (newest version), got %+v", got[0].k)
	}
	if string(got[1].v) != "r1b" {
		t.Errorf("cell 1: got %q want r1b", got[1].v)
	}
}

// TestVersioning_ScopeInvariant: the newest-N result is identical at
// scan, minc, and majc — Java's VersioningIterator does no scope
// branching, and neither do we.
func TestVersioning_ScopeInvariant(t *testing.T) {
	var prev []string
	for _, sc := range []IteratorScope{ScopeScan, ScopeMinc, ScopeMajc} {
		v := initVers(t, newSliceSource(versioned()...), "1", sc)
		got, err := drain(v)
		if err != nil {
			t.Fatalf("scope %v drain: %v", sc, err)
		}
		gs := valStrings(got)
		if prev != nil && !eqStr(prev, gs) {
			t.Fatalf("scope %v diverged: %v vs %v", sc, gs, prev)
		}
		prev = gs
	}
}

// TestVersioning_BadOptions: maxVersions < 1 or non-integer is rejected
// at Init, matching Java's IllegalArgumentException.
func TestVersioning_BadOptions(t *testing.T) {
	for _, bad := range []string{"0", "-3", "xyz"} {
		v := NewVersioningIterator()
		err := v.Init(newSliceSource(versioned()...), map[string]string{VersioningOption: bad}, IteratorEnvironment{})
		if err == nil {
			t.Errorf("maxVersions=%q: expected Init error, got nil", bad)
		}
	}
}

// TestVersioning_DeepCopyIndependent: a DeepCopy seeks independently and
// carries maxVersions forward.
func TestVersioning_DeepCopyIndependent(t *testing.T) {
	v := initVers(t, newSliceSource(versioned()...), "2", ScopeScan)
	if err := v.Next(); err != nil { // consume one cell on the original
		t.Fatalf("Next: %v", err)
	}
	cp := v.DeepCopy(IteratorEnvironment{Scope: ScopeScan}).(*VersioningIterator)
	if err := cp.Seek(InfiniteRange(), nil, false); err != nil {
		t.Fatalf("copy Seek: %v", err)
	}
	got, err := drain(cp)
	if err != nil {
		t.Fatalf("copy drain: %v", err)
	}
	want := []string{"r1a-v30", "r1a-v20", "r1b-v5", "r2a-v9", "r2a-v8"}
	if gs := valStrings(got); !eqStr(gs, want) {
		t.Fatalf("copy got %v want %v", gs, want)
	}
}

func valStrings(cells []kv) []string {
	out := make([]string, len(cells))
	for i, c := range cells {
		out[i] = string(c.v)
	}
	return out
}

func eqStr(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
