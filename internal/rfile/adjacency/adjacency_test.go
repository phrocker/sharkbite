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

package adjacency

import (
	"bufio"
	"bytes"
	"reflect"
	"testing"
)

func TestBuilderNeighbors(t *testing.T) {
	b := NewBuilder([]byte("edge"))
	// Add out-of-order to exercise the sort in Build.
	b.Add([]byte("n2"), []byte("n5"), []byte("w5"), nil, 20, false)
	b.Add([]byte("n1"), []byte("n3"), []byte("w3"), []byte("PUBLIC"), 10, false)
	b.Add([]byte("n1"), []byte("n2"), []byte("w2"), nil, 11, true)
	b.Add([]byte("n2"), []byte("n4"), []byte("w4"), nil, 21, false)

	ix := b.Build()
	if ix == nil {
		t.Fatal("Build returned nil")
	}
	if ix.NodeCount() != 2 {
		t.Fatalf("NodeCount = %d, want 2", ix.NodeCount())
	}

	n1 := ix.Neighbors([]byte("n1"))
	want1 := []Edge{
		{CQ: []byte("n2"), Value: []byte("w2"), Timestamp: 11, Deleted: true},
		{CQ: []byte("n3"), Value: []byte("w3"), Timestamp: 10, Vis: []byte("PUBLIC")},
	}
	if !reflect.DeepEqual(n1, want1) {
		t.Fatalf("n1 neighbors = %+v, want %+v", n1, want1)
	}

	if got := ix.Neighbors([]byte("missing")); got != nil {
		t.Fatalf("missing neighbors = %+v, want nil", got)
	}
}

func TestWriteParseRoundTrip(t *testing.T) {
	b := NewBuilder([]byte("edge"))
	for i := 0; i < 50; i++ {
		row := []byte{'r', byte('a' + i%7)}
		b.Add(row, []byte{'c', byte(i)}, []byte{'v', byte(i)}, []byte("VIS"), int64(i*3+1), i%5 == 0)
	}
	orig := b.Build()

	var buf bytes.Buffer
	if err := Write(&buf, orig); err != nil {
		t.Fatalf("Write: %v", err)
	}

	got, err := Parse(bufio.NewReader(bytes.NewReader(buf.Bytes())))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}

	if !bytes.Equal(got.EdgeCF, orig.EdgeCF) {
		t.Fatalf("edgeCF mismatch")
	}
	if got.NodeCount() != orig.NodeCount() {
		t.Fatalf("nodeCount %d != %d", got.NodeCount(), orig.NodeCount())
	}
	for _, n := range orig.nodes {
		a := orig.Neighbors(n.row)
		c := got.Neighbors(n.row)
		if !reflect.DeepEqual(a, c) {
			t.Fatalf("row %q neighbors mismatch:\n got %+v\nwant %+v", n.row, c, a)
		}
	}
}

func TestBuildEmpty(t *testing.T) {
	if ix := NewBuilder([]byte("edge")).Build(); ix != nil {
		t.Fatalf("empty Build = %+v, want nil", ix)
	}
}

func TestParseRejectsBadMagic(t *testing.T) {
	bad := []byte{0, 0, 0, 0, 0, 0, 0, 1}
	if _, err := Parse(bufio.NewReader(bytes.NewReader(bad))); err == nil {
		t.Fatal("expected error for bad magic")
	}
}
