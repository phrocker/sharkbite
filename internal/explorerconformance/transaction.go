/*
 * Licensed to the Apache Software Foundation (ASF) under one
 * or more contributor license agreements. See the NOTICE file
 * distributed with this work for additional information
 * regarding copyright ownership. The ASF licenses this file
 * to you under the Apache License, Version 2.0 (the
 * "License"); you may not use this file except in compliance
 * with the License. You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing,
 * software distributed under the License is distributed on an
 * "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY
 * KIND, either express or implied. See the License for the
 * specific language governing permissions and limitations
 * under the License.
 */

// Package explorerconformance contains reusable coordination adapter checks.
package explorerconformance

import (
	"context"
	"errors"
	"testing"

	"github.com/phrocker/shoal-oss/pkg/explorer/coordination"
	"github.com/phrocker/shoal-oss/pkg/explorer/coordination/allocator"
	"github.com/phrocker/shoal-oss/pkg/explorer/coordination/transaction"
)

func coordinationEntry(id, value string) coordination.ManifestEntry {
	return coordination.ManifestEntry{
		Table: []byte("table"), Row: []byte("row-" + id), ColumnFamily: []byte("f"),
		ColumnQualifier: []byte("q-" + id), EpochSlot: coordination.EpochSlotContent,
		ValueLength: uint32(len(value)), ValueDigest: coordination.Sum([]byte(value)),
		LPART: coordination.LPART("policy"), CopyGeneration: 1,
		VisibilityDigest:   coordination.Sum([]byte("A")),
		LogicalDigest:      coordination.Sum([]byte("logical")),
		PhysicalCopyDigest: coordination.Sum([]byte("physical")),
	}
}

func RunAtomicStoreSuite(t *testing.T, store *transaction.MemoryStore) {
	t.Helper()
	cell := allocator.Coordinate{Row: []byte("row"), Family: []byte("f"), Qualifier: []byte("q")}
	mutation := allocator.Mutation{
		Row: []byte("row"), Conditions: []allocator.Condition{{Coordinate: cell, Absent: true}},
		Updates: []allocator.Update{{Coordinate: cell, Value: []byte("value"), Timestamp: 1}},
	}
	store.Inject(transaction.FaultUnknownAfter)
	status, err := store.CompareAndMutate(context.Background(), mutation)
	if status != allocator.StatusUnknown || !errors.Is(err, allocator.ErrConditionalUnknown) {
		t.Fatalf("unknown-after status = %v, %v", status, err)
	}
	cells, err := store.ReadExact(context.Background(), []allocator.Coordinate{cell})
	if err != nil || len(cells) != 1 || string(cells[0].Value) != "value" {
		t.Fatalf("durable unknown-after readback = %#v, %v", cells, err)
	}
	status, err = store.CompareAndMutate(context.Background(), mutation)
	if err != nil || status != allocator.StatusRejected {
		t.Fatalf("absent CAS replay = %v, %v", status, err)
	}
}

func RunPhysicalMappingSuite(
	t *testing.T,
	adapter *transaction.AccumuloPhysicalAdapter,
	captured func() []transaction.TrustedCell,
) {
	t.Helper()
	cells := []transaction.PhysicalCell{
		{Entry: coordinationEntry("z", "value-z"), Value: []byte("value-z"), Visibility: []byte("A")},
		{Entry: coordinationEntry("a", "value-a"), Value: []byte("value-a"), Visibility: []byte("A")},
		{Entry: coordinationEntry("m", "value-m"), Value: []byte("value-m"), Visibility: []byte("A")},
	}
	if err := adapter.Write(context.Background(), 9, cells); err != nil {
		t.Fatal(err)
	}
	values := captured()
	if len(values) != len(cells) || values[0].Timestamp != 9 ||
		string(values[0].Table) != "table" || string(values[0].Visibility) != "A" {
		t.Fatalf("trusted mapping = %#v", values)
	}
	if err := adapter.Verify(context.Background(), 9, cells); err != nil {
		t.Fatal(err)
	}
}
