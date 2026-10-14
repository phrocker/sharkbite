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

// These tests are the evidence that the harness has teeth: each one feeds a
// deliberately broken Store to a harness check and asserts the check REJECTS
// it. A harness that only passes proves nothing; a harness that fails when the
// implementation is broken is the actual guarantee.

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/phrocker/shoal-oss/pkg/explorer/coordination/allocator"
	"github.com/phrocker/shoal-oss/pkg/explorer/coordination/transaction"
)

// ignoreConditionsStore is a Store that silently drops every CAS condition and
// applies the updates unconditionally. It models the most damaging class of
// broken CAS: one that admits lost updates and multiple winners.
type ignoreConditionsStore struct {
	inner allocator.Store
	mu    sync.Mutex
	seq   int64
}

func (s *ignoreConditionsStore) ReadExact(ctx context.Context, c []allocator.Coordinate) ([]allocator.Cell, error) {
	return s.inner.ReadExact(ctx, c)
}

func (s *ignoreConditionsStore) ScanRowPrefix(ctx context.Context, row, family, q, vis []byte, limit int) ([]allocator.Cell, error) {
	return s.inner.ScanRowPrefix(ctx, row, family, q, vis, limit)
}

func (s *ignoreConditionsStore) CompareAndMutate(ctx context.Context, m allocator.Mutation) (allocator.Status, error) {
	s.mu.Lock()
	s.seq++
	base := int64(1_000_000) + s.seq*int64(len(m.Updates)+1)
	s.mu.Unlock()
	stripped := allocator.Mutation{Row: m.Row, Updates: make([]allocator.Update, len(m.Updates))}
	copy(stripped.Updates, m.Updates)
	for i := range stripped.Updates {
		stripped.Updates[i].Timestamp = base + int64(i)
	}
	status, err := s.inner.CompareAndMutate(ctx, stripped)
	if err != nil {
		return status, err
	}
	return allocator.StatusAccepted, nil
}

// unboundedScanStore ignores the caller's scan limit, modelling a store that
// violates the bounded-scan contract.
type unboundedScanStore struct{ inner allocator.Store }

func (s *unboundedScanStore) ReadExact(ctx context.Context, c []allocator.Coordinate) ([]allocator.Cell, error) {
	return s.inner.ReadExact(ctx, c)
}

func (s *unboundedScanStore) CompareAndMutate(ctx context.Context, m allocator.Mutation) (allocator.Status, error) {
	return s.inner.CompareAndMutate(ctx, m)
}

func (s *unboundedScanStore) ScanRowPrefix(ctx context.Context, row, family, q, vis []byte, _ int) ([]allocator.Cell, error) {
	return s.inner.ScanRowPrefix(ctx, row, family, q, vis, 1<<20)
}

func TestHarnessCatchesLostUpdates(t *testing.T) {
	broken := func() allocator.Store {
		return &ignoreConditionsStore{inner: transaction.NewMemoryStore()}
	}
	if err := checkStoreContract(broken); err == nil {
		t.Fatal("contract check accepted a store that ignores CAS conditions")
	} else {
		t.Logf("caught (contract): %v", err)
	}
	if err := checkStoreConcurrency(broken, 16); err == nil {
		t.Fatal("concurrency check accepted a store that admits lost updates")
	} else {
		t.Logf("caught (concurrency): %v", err)
	}
}

func TestHarnessCatchesUnboundedScan(t *testing.T) {
	broken := func() allocator.Store {
		return &unboundedScanStore{inner: transaction.NewMemoryStore()}
	}
	if err := checkStoreContract(broken); err == nil {
		t.Fatal("contract check accepted a store that ignores the scan limit")
	} else {
		t.Logf("caught: %v", err)
	}
}

// TestHarnessCatchesClockInsensitiveTakeover shows the clock-skew gate has
// teeth: a takeover that would be REFUSED while the lease is live is ADMITTED
// once the injected clock is skewed past the lease boundary. If the coordinator
// ignored the clock, both evaluations would agree and this contrast would
// vanish.
func TestHarnessCatchesClockInsensitiveTakeover(t *testing.T) {
	start := fixtureStart()
	admittedLive, _, _, err := leaseTakeoverGated(start.Add(30 * time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if admittedLive {
		t.Fatal("takeover admitted while lease live (clock gate absent)")
	}
	admittedExpired, before, after, err := leaseTakeoverGated(start.Add(time.Minute).Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if !admittedExpired {
		t.Fatal("forward clock skew past the lease did not admit takeover (clock gate insensitive)")
	}
	if after != before+1 {
		t.Fatalf("fence did not advance on skewed takeover: %d->%d", before, after)
	}
	t.Logf("clock gate is sensitive: refused while live, admitted after skew (fence %d->%d)", before, after)
}
