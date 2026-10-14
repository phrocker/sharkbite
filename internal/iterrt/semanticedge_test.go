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
	"bytes"
	"fmt"
	"testing"
)

func initSemantic(t *testing.T, src SortedKeyValueIterator, opts map[string]string) SortedKeyValueIterator {
	t.Helper()
	it, err := BuildStack(src, []IterSpec{{Name: IterSemanticEdge, Options: opts}}, IteratorEnvironment{Scope: ScopeMajc, FullMajorCompaction: true})
	if err != nil {
		t.Fatalf("BuildStack: %v", err)
	}
	if err := it.Seek(InfiniteRange(), nil, false); err != nil {
		t.Fatalf("Seek: %v", err)
	}
	return it
}

func TestSemanticEdge_UsesWholeRangeAndSemanticCF(t *testing.T) {
	cells := []kv{
		{mk("v1", "V", "_embedding", "", 7), embed(1, 0, 0)},
		{mk("v2", "V", "_embedding", "", 9), embed(0.99, 0.01, 0)},
	}
	got, err := drain(initSemantic(t, newSliceSource(cells...), map[string]string{LatentEdgeSimilarityThreshold: "0.9"}))
	if err != nil {
		t.Fatalf("drain: %v", err)
	}
	var edges []kv
	for _, c := range got {
		if string(c.k.ColumnFamily) == "edge.sem:" {
			edges = append(edges, c)
		}
	}
	if len(edges) != 2 {
		t.Fatalf("expected bidirectional semantic edges, got %d: %#v", len(edges), edges)
	}
	for _, e := range edges {
		if e.k.Timestamp != 10 {
			t.Fatalf("deterministic ts got %d want 10", e.k.Timestamp)
		}
		if string(e.v) != "0.9999" && string(e.v) != "1.0000" {
			t.Fatalf("unexpected score %q", e.v)
		}
	}
}

func TestSemanticEdge_MaxEdgesPerVertexAndCustomCF(t *testing.T) {
	cells := []kv{
		{mk("v1", "V", "_embedding", "", 1), embed(1, 0)},
		{mk("v2", "V", "_embedding", "", 1), embed(1, 0)},
		{mk("v3", "V", "_embedding", "", 1), embed(0.9, 0.1)},
	}
	got, _ := drain(initSemantic(t, newSliceSource(cells...), map[string]string{LatentEdgeSimilarityThreshold: "0.1", LatentEdgeMaxEdgesPerVertex: "1", LatentEdgeEdgeCF: "sem:"}))
	perVertex := map[string]int{}
	for _, c := range got {
		if string(c.k.ColumnFamily) == "sem:" {
			perVertex[string(c.k.Row)]++
		}
	}
	for row, n := range perVertex {
		if n > 1 {
			t.Fatalf("row %s has %d semantic edges, want <= 1", row, n)
		}
	}
}

func TestSemanticEdgeParity_DeterministicBytes(t *testing.T) {
	cells := []kv{
		{mk("v1", "V", "_embedding", "A", 100), embed(1, 0, 0)},
		{mk("v2", "V", "_embedding", "B", 105), embed(1, 0, 0)},
		{mk("v3", "V", "_embedding", "", 102), embed(0, 1, 0)},
	}
	run := func() []byte {
		got, err := drain(initSemantic(t, newSliceSource(cells...), map[string]string{LatentEdgeSimilarityThreshold: "0.8", LatentEdgeMaxEdgesPerVertex: "5"}))
		if err != nil {
			t.Fatalf("drain: %v", err)
		}
		var b bytes.Buffer
		for _, c := range got {
			fmt.Fprintf(&b, "%s|%s|%s|%s|%d|%t|%x\n", c.k.Row, c.k.ColumnFamily, c.k.ColumnQualifier, c.k.ColumnVisibility, c.k.Timestamp, c.k.Deleted, c.v)
		}
		return b.Bytes()
	}
	first, second := run(), run()
	if !bytes.Equal(first, second) {
		t.Fatalf("semantic edge output differed across runs\nfirst:\n%s\nsecond:\n%s", first, second)
	}
}
