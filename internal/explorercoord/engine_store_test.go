/*
 * Licensed to the Apache Software Foundation (ASF) under one
 * or more contributor license agreements. See the NOTICE file
 * distributed with this work for additional information
 * regarding copyright ownership. The ASF licenses this file
 * to you under the Apache License, Version 2.0 (the
 * "License"); you may not use this file except in compliance
 * with the License. You may obtain a copy of the License at
 *
 *     https://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing,
 * software distributed under the License is distributed on an
 * "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY
 * KIND, either express or implied. See the License for the
 * specific language governing permissions and limitations
 * under the License.
 */

package explorercoord

import (
	"bytes"
	"context"
	"errors"
	"os"
	"sync"
	"testing"

	"github.com/phrocker/shoal-oss/internal/cclient"
	"github.com/phrocker/shoal-oss/internal/engine"
	"github.com/phrocker/shoal-oss/pkg/explorer/coordination"
	"github.com/phrocker/shoal-oss/pkg/explorer/coordination/allocator"
	"github.com/phrocker/shoal-oss/pkg/explorer/coordination/transaction"
)

func TestEngineStoreCASExactReadsAndBounds(t *testing.T) {
	eng, store := openTestEngineStore(t, "coord")
	defer eng.Close()
	ctx := context.Background()
	coordinate := allocator.Coordinate{
		Row: []byte("row"), Family: []byte("f"), Qualifier: []byte("q"),
	}
	status, err := store.CompareAndMutate(ctx, allocator.Mutation{
		Row:        coordinate.Row,
		Conditions: []allocator.Condition{{Coordinate: coordinate, Absent: true}},
		Updates:    []allocator.Update{{Coordinate: coordinate, Value: []byte{}, Timestamp: 1}},
	})
	if err != nil || status != allocator.StatusAccepted {
		t.Fatalf("initial CAS = %v, %v", status, err)
	}
	cells, err := store.ReadExact(ctx, []allocator.Coordinate{{
		Row: []byte("row"), Family: []byte("f"), Qualifier: []byte("q"), Visibility: []byte{},
	}})
	if err != nil || len(cells) != 1 || cells[0].Timestamp != 1 || len(cells[0].Value) != 0 {
		t.Fatalf("empty point read = %#v, %v", cells, err)
	}

	var wg sync.WaitGroup
	results := make(chan allocator.Status, 2)
	for _, value := range [][]byte{[]byte("left"), []byte("right")} {
		value := value
		wg.Add(1)
		go func() {
			defer wg.Done()
			status, _ := store.CompareAndMutate(ctx, allocator.Mutation{
				Row: coordinate.Row,
				Conditions: []allocator.Condition{{
					Coordinate: coordinate, Value: nil, Timestamp: 1, TimestampSet: true,
				}},
				Updates: []allocator.Update{{
					Coordinate: coordinate, Value: value, Timestamp: 2,
				}},
			})
			results <- status
		}()
	}
	wg.Wait()
	close(results)
	accepted := 0
	for status := range results {
		if status == allocator.StatusAccepted {
			accepted++
		}
	}
	if accepted != 1 {
		t.Fatalf("accepted divergent updates = %d, want 1", accepted)
	}
	cells, err = store.ReadExact(ctx, []allocator.Coordinate{coordinate})
	if err != nil || len(cells) != 1 || cells[0].Timestamp != 2 ||
		(!bytes.Equal(cells[0].Value, []byte("left")) && !bytes.Equal(cells[0].Value, []byte("right"))) {
		t.Fatalf("winner = %#v, %v", cells, err)
	}

	if _, err := store.ScanRowPrefix(ctx, coordinate.Row, coordinate.Family, nil, nil, 0); err == nil {
		t.Fatal("zero scan limit succeeded")
	}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := store.ReadExact(canceled, []allocator.Coordinate{coordinate}); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled point read = %v", err)
	}
}

func TestEngineStorePrefixPagingAndErrorPropagation(t *testing.T) {
	eng, store := openTestEngineStore(t, "coord")
	defer eng.Close()
	ctx := context.Background()
	for _, row := range [][]byte{[]byte("p/a"), []byte("p/b"), []byte("p/c")} {
		coordinate := allocator.Coordinate{
			Row: row, Family: []byte("s"), Qualifier: []byte("root"),
		}
		status, err := store.CompareAndMutate(ctx, allocator.Mutation{
			Row:        row,
			Conditions: []allocator.Condition{{Coordinate: coordinate, Absent: true}},
			Updates:    []allocator.Update{{Coordinate: coordinate, Value: row, Timestamp: 1}},
		})
		if err != nil || status != allocator.StatusAccepted {
			t.Fatalf("write %q = %v, %v", row, status, err)
		}
	}
	first, err := store.ScanPrefixFrom(
		ctx, []byte("p/"), []byte("p/"), []byte("s"), []byte("root"), nil, 2,
	)
	if err != nil || len(first) != 2 ||
		bytes.Compare(first[0].Coordinate.Row, first[1].Coordinate.Row) >= 0 {
		t.Fatalf("first page = %#v, %v", first, err)
	}
	next := append(append([]byte(nil), first[1].Coordinate.Row...), 0)
	second, err := store.ScanPrefixFrom(
		ctx, []byte("p/"), next, []byte("s"), []byte("root"), []byte{}, 2,
	)
	if err != nil || len(second) != 1 || !bytes.Equal(second[0].Coordinate.Row, []byte("p/c")) {
		t.Fatalf("second page = %#v, %v", second, err)
	}

	missing, err := NewEngineStore(eng, "missing")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := missing.ReadExact(ctx, []allocator.Coordinate{{
		Row: []byte("row"), Family: []byte("f"), Qualifier: []byte("q"),
	}}); err == nil {
		t.Fatal("missing-table point read succeeded")
	}
	if _, err := missing.ScanPrefix(
		ctx, []byte("p/"), []byte("f"), []byte("q"), nil, 1,
	); err == nil {
		t.Fatal("missing-table prefix scan succeeded")
	}
	status, err := missing.CompareAndMutate(ctx, allocator.Mutation{
		Row: []byte("row"),
		Conditions: []allocator.Condition{{
			Coordinate: allocator.Coordinate{Row: []byte("row"), Family: []byte("f"), Qualifier: []byte("q")},
			Absent:     true,
		}},
		Updates: []allocator.Update{{
			Coordinate: allocator.Coordinate{Row: []byte("row"), Family: []byte("f"), Qualifier: []byte("q")},
			Value:      []byte("value"), Timestamp: 1,
		}},
	})
	if status != allocator.StatusUnknown || !errors.Is(err, allocator.ErrConditionalUnknown) {
		t.Fatalf("missing-table CAS = %v, %v", status, err)
	}
}

func TestPhysicalPersistsAndDetectsIndependentMutation(t *testing.T) {
	directory := testDirectory(t)
	eng, err := engine.Open(directory, engine.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if err := eng.CreateTable("records", engine.TableOptions{}); err != nil {
		t.Fatal(err)
	}
	physical, err := NewPhysical(eng)
	if err != nil {
		t.Fatal(err)
	}
	cell := testPhysicalCell([]byte("value"))
	if err := physical.Write(context.Background(), 7, []transaction.PhysicalCell{cell}); err != nil {
		t.Fatal(err)
	}
	if err := physical.Verify(context.Background(), 7, []transaction.PhysicalCell{cell}); err != nil {
		t.Fatal(err)
	}
	if err := eng.Close(); err != nil {
		t.Fatal(err)
	}
	eng, err = engine.Open(directory, engine.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer eng.Close()
	physical, _ = NewPhysical(eng)
	if err := physical.Verify(context.Background(), 7, []transaction.PhysicalCell{cell}); err != nil {
		t.Fatalf("verify after reopen: %v", err)
	}

	mutation, _ := cclient.NewMutation(cell.Entry.Row)
	mutation.Put(
		cell.Entry.ColumnFamily, cell.Entry.ColumnQualifier,
		cell.Visibility, 7, []byte("corrupt"),
	)
	if err := eng.Write("records", []*cclient.Mutation{mutation}); err != nil {
		t.Fatal(err)
	}
	if err := physical.Verify(context.Background(), 7, []transaction.PhysicalCell{cell}); !errors.Is(err, transaction.ErrInternal) {
		t.Fatalf("verification after independent mutation = %v", err)
	}
	divergent := testPhysicalCell([]byte("different"))
	if err := physical.Write(context.Background(), 7, []transaction.PhysicalCell{divergent}); !errors.Is(err, transaction.ErrInternal) {
		t.Fatalf("divergent retry = %v", err)
	}
}

func openTestEngineStore(t *testing.T, table string) (*engine.Engine, *EngineStore) {
	t.Helper()
	eng, err := engine.Open(testDirectory(t), engine.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if err := eng.CreateTable(table, engine.TableOptions{}); err != nil {
		_ = eng.Close()
		t.Fatal(err)
	}
	store, err := NewEngineStore(eng, table)
	if err != nil {
		_ = eng.Close()
		t.Fatal(err)
	}
	return eng, store
}

func testDirectory(t *testing.T) string {
	t.Helper()
	directory, err := os.MkdirTemp(".", ".explorercoord-test-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(directory) })
	return directory
}

func testPhysicalCell(value []byte) transaction.PhysicalCell {
	logical := coordination.Sum([]byte("logical"))
	physical := coordination.Sum([]byte("physical"))
	visibility := []byte{}
	return transaction.PhysicalCell{
		Entry: coordination.ManifestEntry{
			Table: []byte("records"), Row: []byte("row"), ColumnFamily: []byte("f"),
			ColumnQualifier: []byte("q"), EpochSlot: coordination.EpochSlotContent,
			ValueLength: uint32(len(value)), ValueDigest: coordination.Sum(value),
			LPART: coordination.LPART("partition"), CopyGeneration: 1,
			VisibilityDigest: coordination.Sum(visibility), LogicalDigest: logical,
			PhysicalCopyDigest: physical,
		},
		Value: append([]byte(nil), value...),
	}
}
