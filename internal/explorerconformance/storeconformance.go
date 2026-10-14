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

package explorerconformance

import (
	"context"
	"encoding/binary"
	"fmt"
	"sync"
	"testing"

	"github.com/phrocker/shoal-oss/pkg/explorer/coordination/allocator"
)

// StoreFactory produces a fresh, empty Store for a single check. Each call must
// return independent state so that concurrency and contract checks do not leak
// into one another.
type StoreFactory func() allocator.Store

func coord(row, family, qualifier, visibility string) allocator.Coordinate {
	return allocator.Coordinate{
		Row: []byte(row), Family: []byte(family),
		Qualifier: []byte(qualifier), Visibility: []byte(visibility),
	}
}

func insertAbsent(ctx context.Context, store allocator.Store, c allocator.Coordinate, value string, ts int64) (allocator.Status, error) {
	return store.CompareAndMutate(ctx, allocator.Mutation{
		Row:        c.Row,
		Conditions: []allocator.Condition{{Coordinate: c, Absent: true}},
		Updates:    []allocator.Update{{Coordinate: c, Value: []byte(value), Timestamp: ts}},
	})
}

// RunStoreContractSuite verifies ReadExact / ScanRowPrefix / CompareAndMutate
// semantics against a freshly produced Store. It is a thin wrapper around
// checkStoreContract that fails the test on the first violation.
func RunStoreContractSuite(t *testing.T, newStore StoreFactory) {
	t.Helper()
	if err := checkStoreContract(newStore); err != nil {
		t.Fatalf("store contract: %v", err)
	}
}

// checkStoreContract returns a non-nil error describing the first contract
// violation it observes, or nil when the store honours the seam. Returning an
// error (rather than calling t.Fatal directly) lets the mutation tests assert
// that a broken store is actually caught.
func checkStoreContract(newStore StoreFactory) error {
	ctx := context.Background()

	// ReadExact coordinate exactness: a written cell is returned only for its
	// exact coordinate, and never for a coordinate that differs in any field.
	store := newStore()
	target := coord("row", "f", "q", "v")
	if status, err := insertAbsent(ctx, store, target, "value", 10); err != nil || status != allocator.StatusAccepted {
		return fmt.Errorf("absent insert = %v, %v (want accepted)", status, err)
	}
	cells, err := store.ReadExact(ctx, []allocator.Coordinate{target})
	if err != nil || len(cells) != 1 || string(cells[0].Value) != "value" || cells[0].Timestamp != 10 {
		return fmt.Errorf("readback of written cell = %#v, %v", cells, err)
	}
	for _, miss := range []allocator.Coordinate{
		coord("row", "f", "q", "other"),
		coord("row", "f", "other", "v"),
		coord("row", "other", "q", "v"),
		coord("other", "f", "q", "v"),
	} {
		got, err := store.ReadExact(ctx, []allocator.Coordinate{miss})
		if err != nil {
			return fmt.Errorf("readexact miss %v error: %v", miss, err)
		}
		if len(got) != 0 {
			return fmt.Errorf("readexact returned a cell for non-exact coordinate %v: %#v", miss, got)
		}
	}

	// CompareAndMutate condition semantics.
	store = newStore()
	c := coord("r", "f", "q", "v")
	if status, err := insertAbsent(ctx, store, c, "v1", 1); err != nil || status != allocator.StatusAccepted {
		return fmt.Errorf("first absent insert = %v, %v", status, err)
	}
	if status, err := insertAbsent(ctx, store, c, "v2", 2); err != nil || status != allocator.StatusRejected {
		return fmt.Errorf("absent insert over existing cell = %v, %v (want rejected)", status, err)
	}
	// Value+timestamp condition that matches must be accepted.
	update := func(condValue string, condTS int64, newValue string, newTS int64) (allocator.Status, error) {
		return store.CompareAndMutate(ctx, allocator.Mutation{
			Row: c.Row,
			Conditions: []allocator.Condition{{
				Coordinate: c, Value: []byte(condValue), Timestamp: condTS, TimestampSet: true,
			}},
			Updates: []allocator.Update{{Coordinate: c, Value: []byte(newValue), Timestamp: newTS}},
		})
	}
	if status, err := update("wrong", 1, "v3", 3); err != nil || status != allocator.StatusRejected {
		return fmt.Errorf("CAS with wrong value = %v, %v (want rejected)", status, err)
	}
	if status, err := update("v1", 99, "v3", 3); err != nil || status != allocator.StatusRejected {
		return fmt.Errorf("CAS with wrong timestamp = %v, %v (want rejected)", status, err)
	}
	if status, err := update("v1", 1, "v3", 3); err != nil || status != allocator.StatusAccepted {
		return fmt.Errorf("CAS with matching value+timestamp = %v, %v (want accepted)", status, err)
	}
	cells, err = store.ReadExact(ctx, []allocator.Coordinate{c})
	if err != nil || len(cells) != 1 || string(cells[0].Value) != "v3" || cells[0].Timestamp != 3 {
		return fmt.Errorf("readback after accepted CAS = %#v, %v", cells, err)
	}

	// Bounded scan semantics: qualifier lower bound, ordering, and the limit.
	store = newStore()
	for i, q := range []string{"a", "b", "c", "d", "e"} {
		if status, err := insertAbsent(ctx, store, coord("scan", "f", q, "v"), "V"+q, int64(i+1)); err != nil || status != allocator.StatusAccepted {
			return fmt.Errorf("scan seed %q = %v, %v", q, status, err)
		}
	}
	// A cell in a different family / visibility must be excluded.
	if status, err := insertAbsent(ctx, store, coord("scan", "g", "a", "v"), "wrong-family", 1); err != nil || status != allocator.StatusAccepted {
		return fmt.Errorf("scan family seed = %v, %v", status, err)
	}
	if status, err := insertAbsent(ctx, store, coord("scan", "f", "a", "w"), "wrong-vis", 1); err != nil || status != allocator.StatusAccepted {
		return fmt.Errorf("scan visibility seed = %v, %v", status, err)
	}
	got, err := store.ScanRowPrefix(ctx, []byte("scan"), []byte("f"), []byte("b"), []byte("v"), 10)
	if err != nil {
		return fmt.Errorf("scan from qualifier b: %v", err)
	}
	wantQualifiers := []string{"b", "c", "d", "e"}
	if len(got) != len(wantQualifiers) {
		return fmt.Errorf("scan lower bound/filtering returned %d cells, want %d: %#v", len(got), len(wantQualifiers), got)
	}
	for i, cell := range got {
		if string(cell.Coordinate.Qualifier) != wantQualifiers[i] {
			return fmt.Errorf("scan ordering: position %d = %q, want %q", i, cell.Coordinate.Qualifier, wantQualifiers[i])
		}
		if string(cell.Coordinate.Family) != "f" || string(cell.Coordinate.Visibility) != "v" {
			return fmt.Errorf("scan returned a cell outside the requested family/visibility: %#v", cell.Coordinate)
		}
	}
	// The limit must be respected exactly.
	limited, err := store.ScanRowPrefix(ctx, []byte("scan"), []byte("f"), []byte("a"), []byte("v"), 2)
	if err != nil {
		return fmt.Errorf("bounded scan: %v", err)
	}
	if len(limited) != 2 {
		return fmt.Errorf("bounded scan returned %d cells, want limit 2: %#v", len(limited), limited)
	}
	if string(limited[0].Coordinate.Qualifier) != "a" || string(limited[1].Coordinate.Qualifier) != "b" {
		return fmt.Errorf("bounded scan did not return the lowest-qualifier prefix: %#v", limited)
	}
	return nil
}

// RunStoreConcurrencySuite drives many concurrent writers against a single
// contended row and fails the test if the store admits a lost update, a torn
// read, or more than one winner per contended generation.
func RunStoreConcurrencySuite(t *testing.T, newStore StoreFactory) {
	t.Helper()
	if err := checkStoreConcurrency(newStore, 32); err != nil {
		t.Fatalf("store concurrency: %v", err)
	}
}

// checkStoreConcurrency runs two contention experiments and returns an error if
// the store's linearizable CAS guarantee is violated.
func checkStoreConcurrency(newStore StoreFactory, workers int) error {
	ctx := context.Background()

	// Experiment 1: absent-insert race. Exactly one writer may create the cell;
	// every other writer must be rejected, and the surviving value must belong
	// to the single winner (no torn/blended state).
	store := newStore()
	target := coord("race", "f", "q", "v")
	var accepted int64
	var winner string
	var mu sync.Mutex
	var wg sync.WaitGroup
	errCh := make(chan error, workers)
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			value := fmt.Sprintf("writer-%d", id)
			status, err := insertAbsent(ctx, store, target, value, 1)
			if err != nil {
				errCh <- fmt.Errorf("absent insert by %d: %w", id, err)
				return
			}
			if status == allocator.StatusAccepted {
				mu.Lock()
				accepted++
				winner = value
				mu.Unlock()
			}
		}(i)
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		return err
	}
	if accepted != 1 {
		return fmt.Errorf("absent-insert race admitted %d winners, want exactly 1", accepted)
	}
	cells, err := store.ReadExact(ctx, []allocator.Coordinate{target})
	if err != nil || len(cells) != 1 {
		return fmt.Errorf("post-race readback = %#v, %v", cells, err)
	}
	if string(cells[0].Value) != winner {
		return fmt.Errorf("post-race value %q does not belong to the winner %q (lost update)", cells[0].Value, winner)
	}

	// Experiment 2: contended monotonic counter. Every worker performs one
	// successful increment via read/modify/CAS. The store must serialise them:
	// each generation (timestamp) has exactly one winner, no increment is lost,
	// and the final value equals the number of workers.
	store = newStore()
	counter := coord("counter", "f", "n", "v")
	if status, err := insertAbsent(ctx, store, counter, string(encodeUint(0)), 1); err != nil || status != allocator.StatusAccepted {
		return fmt.Errorf("counter seed = %v, %v", status, err)
	}
	var genMu sync.Mutex
	genWinners := make(map[int64]int)
	wg = sync.WaitGroup{}
	errCh = make(chan error, workers)
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				read, err := store.ReadExact(ctx, []allocator.Coordinate{counter})
				if err != nil || len(read) != 1 {
					errCh <- fmt.Errorf("counter read = %#v, %v", read, err)
					return
				}
				current := read[0]
				next := decodeUint(current.Value) + 1
				status, err := store.CompareAndMutate(ctx, allocator.Mutation{
					Row: counter.Row,
					Conditions: []allocator.Condition{{
						Coordinate: counter, Value: current.Value,
						Timestamp: current.Timestamp, TimestampSet: true,
					}},
					Updates: []allocator.Update{{
						Coordinate: counter, Value: encodeUint(next), Timestamp: current.Timestamp + 1,
					}},
				})
				if err != nil {
					errCh <- fmt.Errorf("counter CAS: %w", err)
					return
				}
				if status == allocator.StatusAccepted {
					genMu.Lock()
					genWinners[current.Timestamp+1]++
					genMu.Unlock()
					return
				}
				// Rejected: another writer won this generation; retry.
			}
		}()
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		return err
	}
	final, err := store.ReadExact(ctx, []allocator.Coordinate{counter})
	if err != nil || len(final) != 1 {
		return fmt.Errorf("final counter read = %#v, %v", final, err)
	}
	if got := decodeUint(final[0].Value); got != uint64(workers) {
		return fmt.Errorf("counter final value = %d, want %d (lost update)", got, workers)
	}
	if len(genWinners) != workers {
		return fmt.Errorf("expected %d distinct winning generations, got %d", workers, len(genWinners))
	}
	for gen, count := range genWinners {
		if count != 1 {
			return fmt.Errorf("generation %d had %d winners, want exactly 1", gen, count)
		}
	}
	return nil
}

func encodeUint(v uint64) []byte {
	buf := make([]byte, 8)
	binary.BigEndian.PutUint64(buf, v)
	return buf
}

func decodeUint(b []byte) uint64 {
	if len(b) != 8 {
		return 0
	}
	return binary.BigEndian.Uint64(b)
}
