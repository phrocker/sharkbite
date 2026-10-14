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

package cclient

import (
	"bytes"
	"testing"
)

func TestCells_EmptyMutation(t *testing.T) {
	m, err := NewMutation([]byte("r1"))
	if err != nil {
		t.Fatal(err)
	}
	if got := m.Cells(); got != nil {
		t.Errorf("empty mutation should yield nil cells, got %v", got)
	}
}

func TestCells_PutAndDeleteProjection(t *testing.T) {
	m, err := NewMutation([]byte("row-1"))
	if err != nil {
		t.Fatal(err)
	}
	m.Put([]byte("cf"), []byte("cq"), []byte("cv"), 100, []byte("val"))
	m.Delete([]byte("cf2"), []byte("cq2"), nil, 200)

	cells := m.Cells()
	if len(cells) != 2 {
		t.Fatalf("expected 2 cells, got %d", len(cells))
	}

	put := cells[0]
	if !bytes.Equal(put.Key.Row, []byte("row-1")) {
		t.Errorf("put row = %q, want row-1", put.Key.Row)
	}
	if !bytes.Equal(put.Key.ColumnFamily, []byte("cf")) ||
		!bytes.Equal(put.Key.ColumnQualifier, []byte("cq")) ||
		!bytes.Equal(put.Key.ColumnVisibility, []byte("cv")) {
		t.Errorf("put column coords mismatch: %+v", put.Key)
	}
	if put.Key.Timestamp != 100 {
		t.Errorf("put ts = %d, want 100", put.Key.Timestamp)
	}
	if put.Key.Deleted {
		t.Error("put cell should not be marked deleted")
	}
	if !bytes.Equal(put.Value, []byte("val")) {
		t.Errorf("put value = %q, want val", put.Value)
	}

	del := cells[1]
	if !del.Key.Deleted {
		t.Error("delete cell should be marked deleted")
	}
	if del.Key.Timestamp != 200 {
		t.Errorf("delete ts = %d, want 200", del.Key.Timestamp)
	}
	if len(del.Value) != 0 {
		t.Errorf("delete cell value should be empty, got %q", del.Value)
	}
}

// TestCells_RowAliasing documents that Cell.Key.Row aliases the Mutation's
// row buffer — the allocation-conscious contract. All cells of one mutation
// share the same backing row array.
func TestCells_RowAliasing(t *testing.T) {
	m, err := NewMutation([]byte("shared-row"))
	if err != nil {
		t.Fatal(err)
	}
	m.Put([]byte("a"), nil, nil, 1, []byte("v1"))
	m.Put([]byte("b"), nil, nil, 2, []byte("v2"))

	cells := m.Cells()
	if &cells[0].Key.Row[0] != &cells[1].Key.Row[0] {
		t.Error("expected both cells to alias the same row backing array")
	}
	if &cells[0].Key.Row[0] != &m.Row()[0] {
		t.Error("expected cell row to alias the Mutation row")
	}
}

// TestCells_OrderingViaWireKey is a smoke check that the projected keys feed
// wire.Key.Compare correctly: a tombstone sorts before a live cell at the
// same coordinate, and a newer timestamp sorts before an older one.
func TestCells_OrderingViaWireKey(t *testing.T) {
	m, err := NewMutation([]byte("r"))
	if err != nil {
		t.Fatal(err)
	}
	m.Put([]byte("cf"), []byte("cq"), nil, 50, []byte("old")) // cells[0]
	m.Put([]byte("cf"), []byte("cq"), nil, 99, []byte("new")) // cells[1]
	m.Delete([]byte("cf"), []byte("cq"), nil, 99)             // cells[2]
	cells := m.Cells()

	// newer ts (99) sorts before older ts (50)
	if cells[1].Key.Compare(&cells[0].Key) >= 0 {
		t.Error("newer timestamp should sort before older")
	}
	// tombstone at ts 99 sorts before the live cell at ts 99
	if cells[2].Key.Compare(&cells[1].Key) >= 0 {
		t.Error("tombstone should sort before live cell at same coordinate")
	}
}
