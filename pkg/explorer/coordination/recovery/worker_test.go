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

package recovery

import (
	"bytes"
	"context"
	"errors"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/phrocker/shoal-oss/pkg/explorer/coordination"
	"github.com/phrocker/shoal-oss/pkg/explorer/coordination/allocator"
	"github.com/phrocker/shoal-oss/pkg/explorer/coordination/transaction"
)

type fixedSource []coordination.TXN

func (s fixedSource) Candidates(_ context.Context, _ coordination.DomainID, after []byte, limit int) ([]coordination.TXN, []byte, error) {
	start := 0
	if len(after) != 0 {
		start = int(after[0])
	}
	end := min(start+limit, len(s))
	var next []byte
	if end < len(s) {
		next = []byte{byte(end)}
	}
	return s[start:end], next, nil
}

type pageScanner []allocator.Cell

func (s pageScanner) ScanPrefixFrom(
	_ context.Context,
	prefix, start, _, _, _ []byte,
	limit int,
) ([]allocator.Cell, error) {
	var result []allocator.Cell
	for _, cell := range s {
		if bytes.HasPrefix(cell.Coordinate.Row, prefix) && bytes.Compare(cell.Coordinate.Row, start) >= 0 {
			result = append(result, cell)
		}
	}
	sort.Slice(result, func(i, j int) bool {
		return bytes.Compare(result[i].Coordinate.Row, result[j].Coordinate.Row) < 0
	})
	if len(result) > limit {
		result = result[:limit]
	}
	return result, nil
}

type fakeCoordinator struct {
	snapshot   transaction.Snapshot
	mu         sync.Mutex
	recovered  int
	active     int
	maxActive  int
	recoverLag time.Duration
}

type failOnceCoordinator struct {
	fakeCoordinator
	failed bool
}

func (f *failOnceCoordinator) Recover(
	ctx context.Context,
	txn coordination.TXN,
	owner coordination.OwnerID,
	lease time.Time,
	authority transaction.Authority,
) (transaction.Result, error) {
	if !f.failed {
		f.failed = true
		return transaction.Result{}, transaction.ErrUnavailable
	}
	return f.fakeCoordinator.Recover(ctx, txn, owner, lease, authority)
}

func (f *fakeCoordinator) Inspect(context.Context, coordination.TXN) (transaction.Snapshot, error) {
	return f.snapshot, nil
}

func (f *fakeCoordinator) Recover(context.Context, coordination.TXN, coordination.OwnerID, time.Time, transaction.Authority) (transaction.Result, error) {
	f.mu.Lock()
	f.recovered++
	f.active++
	if f.active > f.maxActive {
		f.maxActive = f.active
	}
	f.mu.Unlock()
	time.Sleep(f.recoverLag)
	f.mu.Lock()
	f.active--
	f.mu.Unlock()
	return transaction.Result{Epoch: f.snapshot.Root.Epoch}, nil
}

func TestWorkerBoundsAndAuthoritativeRecheck(t *testing.T) {
	now := time.Date(2026, 8, 27, 16, 0, 0, 0, time.UTC)
	coordinator := &fakeCoordinator{snapshot: transaction.Snapshot{
		Root: coordination.TxnRootV3{State: coordination.StateClaimed},
		Lease: coordination.TxnLeaseV1{
			Generation: 1, Owner: coordination.OwnerID("old"), Fence: 1, LeaseUntil: now.Add(-time.Minute),
		},
	}}
	worker, err := New(Config{
		Domain: coordination.DomainID("domain"), Owner: coordination.OwnerID("recovery"),
		Source:      fixedSource{coordination.TXN("b"), coordination.TXN("a")},
		Coordinator: coordinator, Clock: func() time.Time { return now },
		Authority: transaction.Authority{}, Limit: 2, Concurrency: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := worker.RunOnce(context.Background()); err != nil || coordinator.recovered != 2 {
		t.Fatalf("RunOnce = recovered %d, %v", coordinator.recovered, err)
	}
	worker.config.Source = fixedSource{coordination.TXN("a"), coordination.TXN("b"), coordination.TXN("c")}
	if err := worker.RunOnce(context.Background()); err != nil {
		t.Fatalf("first bounded page = %v", err)
	}
	if err := worker.RunOnce(context.Background()); err != nil || coordinator.recovered != 5 {
		t.Fatalf("second bounded page = recovered %d, %v", coordinator.recovered, err)
	}
}

func TestWorkerPoolHonorsConcurrencyCap(t *testing.T) {
	now := time.Date(2026, 8, 27, 16, 0, 0, 0, time.UTC)
	coordinator := &fakeCoordinator{
		snapshot: transaction.Snapshot{
			Root: coordination.TxnRootV3{State: coordination.StateClaimed},
			Lease: coordination.TxnLeaseV1{
				Generation: 1, Owner: coordination.OwnerID("old"), Fence: 1,
				LeaseUntil: now.Add(-time.Minute),
			},
		},
		recoverLag: time.Millisecond,
	}

	candidates := make(fixedSource, 24)
	for i := range candidates {
		candidates[i] = coordination.TXN{byte(i + 1)}
	}
	worker, err := New(Config{
		Domain: coordination.DomainID("domain"), Owner: coordination.OwnerID("recovery"),
		Source: candidates, Coordinator: coordinator, Clock: func() time.Time { return now },
		Authority: transaction.Authority{}, Limit: len(candidates), Concurrency: 3,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := worker.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	coordinator.mu.Lock()
	defer coordinator.mu.Unlock()
	if coordinator.recovered != len(candidates) || coordinator.maxActive > 3 ||
		coordinator.maxActive < 2 {
		t.Fatalf("pool recovered=%d maxActive=%d", coordinator.recovered, coordinator.maxActive)
	}
}

func TestWorkerRunPageRetainsCursorOnFailure(t *testing.T) {
	now := time.Date(2026, 9, 5, 18, 0, 0, 0, time.UTC)
	coordinator := &failOnceCoordinator{fakeCoordinator: fakeCoordinator{
		snapshot: transaction.Snapshot{
			Root: coordination.TxnRootV3{State: coordination.StateClaimed},
			Lease: coordination.TxnLeaseV1{
				Generation: 1, Owner: coordination.OwnerID("old"), Fence: 1,
				LeaseUntil: now.Add(-time.Minute),
			},
		},
	}}
	worker, err := New(Config{
		Domain: coordination.DomainID("domain"), Owner: coordination.OwnerID("recovery"),
		Source: fixedSource{
			coordination.TXN("a"), coordination.TXN("b"), coordination.TXN("c"),
		},
		Coordinator: coordinator, Clock: func() time.Time { return now },
		Authority: transaction.Authority{}, Limit: 2, Concurrency: 1,
		MaxRounds: 1, Backoff: time.Nanosecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	if more, err := worker.RunPage(context.Background()); !more ||
		!errors.Is(err, transaction.ErrUnavailable) {
		t.Fatalf("failed page = more %v, %v", more, err)
	}
	if more, err := worker.RunPage(context.Background()); !more || err != nil {
		t.Fatalf("retried page = more %v, %v", more, err)
	}
	coordinator.mu.Lock()
	recovered := coordinator.recovered
	coordinator.mu.Unlock()
	if recovered != 3 {
		t.Fatalf("successful recoveries = %d, want 3 including replayed page", recovered)
	}
}

func TestBandedSourcePaginatesPastCompletedHistory(t *testing.T) {
	domain := coordination.DomainID("domain")
	txns := []coordination.TXN{coordination.TXN("a"), coordination.TXN("b"), coordination.TXN("c")}
	scanner := make(pageScanner, 0, len(txns))
	for _, txn := range txns {
		row, err := coordination.TxnRow(domain, txn)
		if err != nil {
			t.Fatal(err)
		}
		scanner = append(scanner, allocator.Cell{Coordinate: allocator.Coordinate{Row: row}})
	}
	source := BandedSource{Scanner: scanner}
	first, cursor, err := source.Candidates(context.Background(), domain, nil, 2)
	if err != nil || len(first) != 2 || len(cursor) == 0 {
		t.Fatalf("first page = %#v, %x, %v", first, cursor, err)
	}
	second, next, err := source.Candidates(context.Background(), domain, cursor, 2)
	if err != nil || len(second) != 1 || len(next) != 0 {
		t.Fatalf("second page = %#v, %x, %v", second, next, err)
	}
}
