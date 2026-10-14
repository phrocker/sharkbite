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

package provenance

import (
	"bytes"
	"testing"
	"time"

	"github.com/phrocker/shoal-oss/internal/embedpb"
)

func entriesToCells(es []*embedpb.Entry) []*embedpb.Cell {
	cells := make([]*embedpb.Cell, len(es))
	for i, e := range es {
		cells[i] = &embedpb.Cell{
			ColumnFamily:    e.ColumnFamily,
			ColumnQualifier: e.ColumnQualifier,
			Timestamp:       e.Timestamp,
			Value:           e.Value,
		}
	}
	return cells
}

func TestStampEntriesParseRoundTrip(t *testing.T) {
	created := time.Date(2026, 6, 24, 9, 18, 0, 0, time.UTC)
	in := Stamp{
		Actor:     "agent:planner",
		Sources:   []string{"evt:01", "evt:02"},
		Rationale: "merged two related events",
		Hop:       3,
		Ops:       []string{"narrow", "mint"},
		CreatedAt: created,
	}
	got := Parse(entriesToCells(in.Entries(1000)))

	if got.Actor != in.Actor {
		t.Errorf("actor: got %q want %q", got.Actor, in.Actor)
	}
	if len(got.Sources) != 2 || got.Sources[0] != "evt:01" || got.Sources[1] != "evt:02" {
		t.Errorf("sources: got %v", got.Sources)
	}
	if got.Rationale != in.Rationale {
		t.Errorf("rationale: got %q", got.Rationale)
	}
	if got.Hop != 3 {
		t.Errorf("hop: got %d", got.Hop)
	}
	// Ops are stored canonically sorted.
	if len(got.Ops) != 2 || got.Ops[0] != "mint" || got.Ops[1] != "narrow" {
		t.Errorf("ops: got %v want [mint narrow]", got.Ops)
	}
	if !got.CreatedAt.Equal(created) {
		t.Errorf("created_at: got %v want %v", got.CreatedAt, created)
	}
}

func TestStampEntriesDeterministic(t *testing.T) {
	s := Stamp{Actor: "a", Sources: []string{"x", "y"}, Ops: []string{"b", "a"}, CreatedAt: time.Unix(0, 0).UTC()}
	a := s.Entries(5)
	b := s.Entries(5)
	if len(a) != len(b) {
		t.Fatalf("len mismatch %d vs %d", len(a), len(b))
	}
	for i := range a {
		if !bytes.Equal(a[i].ColumnQualifier, b[i].ColumnQualifier) || !bytes.Equal(a[i].Value, b[i].Value) {
			t.Fatalf("nondeterministic at %d: %q=%q vs %q=%q", i,
				a[i].ColumnQualifier, a[i].Value, b[i].ColumnQualifier, b[i].Value)
		}
	}
}

func TestZeroStampRendersNothing(t *testing.T) {
	var s Stamp
	if !s.IsZero() {
		t.Fatal("zero stamp should report IsZero")
	}
	if es := s.Entries(1); len(es) != 0 {
		t.Fatalf("zero stamp rendered %d entries", len(es))
	}
}

func TestParseIgnoresOtherCFs(t *testing.T) {
	cells := []*embedpb.Cell{
		{ColumnFamily: []byte("content:"), ColumnQualifier: []byte("text"), Value: []byte("hello")},
		{ColumnFamily: CF(), ColumnQualifier: []byte(cqActor), Value: []byte("agent:x")},
	}
	got := Parse(cells)
	if got.Actor != "agent:x" {
		t.Errorf("actor: got %q", got.Actor)
	}
	if got.Rationale != "" {
		t.Errorf("unexpected rationale %q", got.Rationale)
	}
}

func TestStampJSON(t *testing.T) {
	s := Stamp{Actor: "a", Hop: 2, Ops: []string{"z", "a"}}
	b, err := s.MarshalJSON()
	if err != nil {
		t.Fatal(err)
	}
	got := string(b)
	want := `{"actor":"a","pca_hop":2,"pca_ops":["a","z"]}`
	if got != want {
		t.Errorf("json: got %s want %s", got, want)
	}
}
