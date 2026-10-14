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
	"sort"
	"testing"
)

// bound maps a test string to a row bound, with "" meaning the absent
// bound (nil). Tests that care how an absent bound was spelled build their
// extents literally instead.
func bound(s string) []byte {
	if s == "" {
		return nil
	}
	return []byte(s)
}

func extent(table, prev, end string) Extent {
	return Extent{TableID: table, PrevEndRow: bound(prev), EndRow: bound(end)}
}

func TestExtentValidate(t *testing.T) {
	tests := []struct {
		name    string
		in      Extent
		wantErr bool
	}{
		{"whole table", extent("2", "", ""), false},
		{"bounded", extent("2", "d", "m"), false},
		{"open start", extent("2", "", "m"), false},
		{"open end", extent("2", "m", ""), false},
		{"empty prev row means absent", Extent{TableID: "2", PrevEndRow: []byte{}, EndRow: []byte("m")}, false},
		{"empty end row means absent", Extent{TableID: "2", PrevEndRow: []byte("m"), EndRow: []byte{}}, false},
		{"no table", extent("", "d", "m"), true},
		{"inverted", extent("2", "m", "d"), true},
		{"empty range", extent("2", "m", "m"), true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.in.Validate()
			if tt.wantErr {
				if !errors.Is(err, ErrInvalidExtent) {
					t.Fatalf("want ErrInvalidExtent, got %v", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
		})
	}
}

// TestExtentTreatsEmptyBoundAsAbsent pins the wire semantics this package
// shares with cclient.KeyExtent and Accumulo's Java KeyExtent: nil and empty
// are the same absent bound. If they were distinct, one tablet could be
// assigned under one spelling and unassigned under the other, and the
// idempotent missing-tablet path in Unassign would report success without
// releasing it.
func TestExtentTreatsEmptyBoundAsAbsent(t *testing.T) {
	nilBound := Extent{TableID: "2", PrevEndRow: nil, EndRow: []byte("m")}
	emptyBound := Extent{TableID: "2", PrevEndRow: []byte{}, EndRow: []byte("m")}

	if !nilBound.Equal(emptyBound) {
		t.Fatal("a nil prev end row must equal an empty one")
	}
	if nilBound.key() != emptyBound.key() {
		t.Fatalf("keys differ: %q vs %q", nilBound.key(), emptyBound.key())
	}
	if nilBound.String() != emptyBound.String() {
		t.Fatalf("rendering differs: %q vs %q", nilBound, emptyBound)
	}
	if compareExtents(nilBound, emptyBound) != 0 {
		t.Fatal("an absent bound must order identically however it is spelled")
	}

	// The same holds for the upper bound, and for an extent that is absent on
	// both sides.
	whole := Extent{TableID: "2", PrevEndRow: []byte{}, EndRow: []byte{}}
	if !whole.Equal(extent("2", "", "")) || whole.key() != extent("2", "", "").key() {
		t.Fatalf("an all-empty extent must be the whole table: %s / %q", whole, whole.key())
	}

	if !nilBound.Equal(extent("2", "", "m")) {
		t.Fatal("identical extents must compare equal")
	}
	if nilBound.Equal(extent("3", "", "m")) {
		t.Fatal("extents of different tables must not compare equal")
	}
}

// TestExtentCloneCanonicalizesBounds checks that what the host stores is
// already normalized, so a caller's empty slice cannot survive as a distinct
// identity.
func TestExtentCloneCanonicalizesBounds(t *testing.T) {
	got := Extent{TableID: "2", PrevEndRow: []byte{}, EndRow: []byte("m")}.clone()
	if got.PrevEndRow != nil {
		t.Fatalf("clone kept an empty prev end row: %#v", got.PrevEndRow)
	}
	if string(got.EndRow) != "m" {
		t.Fatalf("clone lost the end row: %#v", got.EndRow)
	}
}

// TestExtentKeyIsUnambiguous covers extents whose table id and bounds could
// run together under a naive concatenation.
func TestExtentKeyIsUnambiguous(t *testing.T) {
	keys := make(map[string]string)
	for _, e := range []Extent{
		extent("2", "", ""),
		extent("2", "", "m"),
		extent("2", "m", ""),
		extent("2;m", "", ""),
		extent("22", ";m", ""),
	} {
		key := e.key()
		if prior, ok := keys[key]; ok {
			t.Fatalf("key %q collides between %s and %s", key, prior, e)
		}
		keys[key] = e.String()
	}
}

func TestExtentOverlaps(t *testing.T) {
	tests := []struct {
		name string
		a, b Extent
		want bool
	}{
		{"same extent", extent("2", "d", "m"), extent("2", "d", "m"), true},
		{"different tables", extent("2", "", ""), extent("3", "", ""), false},
		{"adjacent at the split point", extent("2", "", "m"), extent("2", "m", ""), false},
		{"disjoint", extent("2", "a", "d"), extent("2", "m", "q"), false},
		{"parent covers child", extent("2", "", ""), extent("2", "d", "m"), true},
		{"child inside parent", extent("2", "d", "m"), extent("2", "", ""), true},
		{"straddles the split point", extent("2", "d", "q"), extent("2", "m", "z"), true},
		{"open end reaches everything above", extent("2", "m", ""), extent("2", "q", "z"), true},
		{"open start reaches everything below", extent("2", "", "m"), extent("2", "a", "d"), true},
		{"touching at one row", extent("2", "a", "m"), extent("2", "l", "z"), true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.a.Overlaps(tt.b); got != tt.want {
				t.Fatalf("%s overlaps %s = %v, want %v", tt.a, tt.b, got, tt.want)
			}
			if got := tt.b.Overlaps(tt.a); got != tt.want {
				t.Fatalf("overlap is not symmetric: %s vs %s", tt.b, tt.a)
			}
		})
	}
}

// TestExtentOverlapsCoversASplitExactly checks the invariant the assignment
// fence depends on: the children of a split tile their parent without
// overlapping each other.
func TestExtentOverlapsCoversASplitExactly(t *testing.T) {
	parent := extent("2", "", "")
	left := extent("2", "", "m")
	right := extent("2", "m", "")

	if left.Overlaps(right) {
		t.Fatal("split children must not overlap each other")
	}
	if !parent.Overlaps(left) || !parent.Overlaps(right) {
		t.Fatal("the parent must overlap both children")
	}
}

func TestExtentString(t *testing.T) {
	if got, want := extent("2", "d", "m").String(), "2;m;d"; got != want {
		t.Fatalf("String() = %q, want %q", got, want)
	}
	if got, want := extent("2", "", "").String(), "2;<;<"; got != want {
		t.Fatalf("String() = %q, want %q", got, want)
	}
}

func TestCompareExtentsSortsInfinitiesToTheOutside(t *testing.T) {
	got := []Extent{
		extent("3", "", ""),
		extent("2", "m", ""),
		extent("2", "d", "m"),
		extent("2", "", "d"),
		extent("2", "", ""),
	}
	sort.Slice(got, func(i, j int) bool { return compareExtents(got[i], got[j]) < 0 })

	want := []string{"2;d;<", "2;<;<", "2;m;d", "2;<;m", "3;<;<"}
	if len(got) != len(want) {
		t.Fatalf("length changed: %d", len(got))
	}
	for i := range want {
		if got[i].String() != want[i] {
			t.Fatalf("position %d = %s, want %s (full order %v)", i, got[i], want[i], got)
		}
	}
}
