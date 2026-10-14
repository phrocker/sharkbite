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

package transaction

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/phrocker/shoal-oss/pkg/explorer/coordination"
	"github.com/phrocker/shoal-oss/pkg/explorer/coordination/allocator"
	"github.com/phrocker/shoal-oss/pkg/explorer/coordination/guard"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

type staticMaterializer struct{ plan Plan }

func (m staticMaterializer) Materialize(context.Context, MaterializeRequest) (Plan, error) {
	return clonePlan(m.plan), nil
}

type fixedAuthority struct{ value guard.Authority }

func (a fixedAuthority) Current(context.Context, coordination.DomainID) (guard.Authority, error) {
	return a.value, nil
}

type fixedRetirement struct{}

func (fixedRetirement) Retired(context.Context, coordination.DomainID, guard.Entity) (bool, coordination.Generation, error) {
	return false, 1, nil
}

type statusProxy struct{ coordinator *Coordinator }

func (p *statusProxy) Status(ctx context.Context, domain coordination.DomainID, txn coordination.TXN) (guard.TxnDisposition, error) {
	return p.coordinator.Status(ctx, domain, txn)
}

type failOnceWriter struct {
	next   PhysicalWriter
	mu     sync.Mutex
	failed bool
}

func (w *failOnceWriter) Write(ctx context.Context, epoch coordination.Epoch, cells []PhysicalCell) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if !w.failed {
		w.failed = true
		if err := w.next.Write(ctx, epoch, cells); err != nil {
			return err
		}
		return ErrUnavailable
	}
	return w.next.Write(ctx, epoch, cells)
}

type fixture struct {
	store       *MemoryStore
	allocator   *allocator.Client
	guards      *guard.Client
	coordinator *Coordinator
	proxy       *statusProxy
	plan        Plan
	now         time.Time
	authority   Authority
}

func newFixture(t *testing.T, writer PhysicalWriter) *fixture {
	t.Helper()
	f := &fixture{
		store: NewMemoryStore(),
		now:   time.Date(2026, 8, 27, 16, 0, 0, 0, time.UTC),
		authority: Authority{
			Generation: 3, Fence: 4, Holder: coordination.OwnerID("authority"),
			Mode: coordination.WriterModeAccumuloPrimary, RetentionGeneration: 2, HistoryFloor: 1,
		},
	}
	allocatorClient, err := allocator.New(allocator.Config{
		Domain: coordination.DomainID("domain"), Store: f.store, Clock: func() time.Time { return f.now },
		MaxRetries: 5, RetryBackoff: time.Nanosecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	f.allocator = allocatorClient
	if _, err := f.allocator.EnsureInitialized(context.Background(), allocator.InitializeOptions{
		HistoryFloor: 1, RetentionGeneration: 2,
		Authority: allocator.Authority{
			Generation: 3, Fence: 4, Holder: coordination.OwnerID("authority"),
			Mode: coordination.WriterModeAccumuloPrimary,
		},
		MaxActiveReservations: 64,
	}); err != nil {
		t.Fatal(err)
	}
	f.proxy = &statusProxy{}
	guardClient, err := guard.New(guard.Config{
		Domain: coordination.DomainID("domain"), Store: f.store,
		Authority: fixedAuthority{guard.Authority{
			Generation: 3, Fence: 4, RetentionGeneration: 2, HistoryFloor: 1,
		}},
		Retirement: fixedRetirement{}, Transactions: f.proxy,
		Clock: func() time.Time { return f.now }, MaxRetries: 5, RetryBackoff: time.Nanosecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	f.guards = guardClient
	f.plan = testPlan(t)
	if writer == nil {
		writer = f.store
	}
	coordinator, err := New(Config{
		Domain: coordination.DomainID("domain"), Store: f.store, Allocator: f.allocator,
		Guards: f.guards, Materializer: staticMaterializer{f.plan},
		Writer: writer, Verifier: f.store, Quarantine: f.store,
		Clock: func() time.Time { return f.now }, MaxRetries: 5, RetryBackoff: time.Nanosecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	f.coordinator = coordinator
	f.proxy.coordinator = coordinator
	return f
}

func testPlan(t *testing.T) Plan {
	t.Helper()
	value := []byte("document")
	visibility := []byte("A")
	logical := coordination.Sum([]byte("logical"))
	physical := coordination.Sum([]byte("physical-copy"))
	entry := coordination.ManifestEntry{
		Table: []byte("documents"), Row: []byte("row"), ColumnFamily: []byte("o"),
		ColumnQualifier: []byte("document"), EpochSlot: coordination.EpochSlotContent,
		ValueLength: uint32(len(value)), ValueDigest: coordination.Sum(value),
		LPART: coordination.LPART("policy"), CopyGeneration: 1,
		VisibilityDigest: coordination.Sum(visibility), LogicalDigest: logical,
		PhysicalCopyDigest: physical,
	}
	chunks, err := coordination.ChunkManifest([]coordination.ManifestEntry{entry})
	if err != nil {
		t.Fatal(err)
	}
	return Plan{
		Chunks: chunks,
		Cells:  []PhysicalCell{{Entry: entry, Value: value, Visibility: visibility}},
		Guards: []GuardPlan{{
			Entity: guard.Entity{Kind: 'D', ID: coordination.EntityID("document")},
			Mode:   guard.ModeAbsentOrIdentical, DesiredState: guard.StateLive,
			DesiredWinnerID: []byte("revision"), DesiredDigest: logical,
			LPART: coordination.LPART("policy"), LogicalPolicyID: []byte("policy-id"),
			RetirementGeneration: 1, PhysicalDigest: physical,
		}},
		Copies: []CommitCopy{{
			LPART: coordination.LPART("policy"), CopyGeneration: 1,
			VisibilityDigest: coordination.Sum(visibility), LogicalDigest: logical,
			PhysicalCopyDigest: physical, RequiredIndexFamilies: []coordination.Family{coordination.Family("lexical")},
			Visibility: visibility,
		}},
		Results: []coordination.ResultIdentity{{Kind: []byte("document"), ID: []byte("document")}},
	}
}

func publication(f *fixture, digest coordination.Digest) Publication {
	return Publication{
		TXN: coordination.TXN("txn"), Token: []byte("token"), LogicalDigest: digest,
		Owner: coordination.OwnerID("worker"), LeaseUntil: f.now.Add(time.Minute), Authority: f.authority,
	}
}

func TestPublishAtomicAndIdempotent(t *testing.T) {
	f := newFixture(t, nil)
	request := publication(f, coordination.Sum([]byte("logical")))
	result, err := f.coordinator.Publish(context.Background(), request)
	if err != nil || result.Epoch != 1 || len(result.Identities) != 1 {
		t.Fatalf("Publish = %#v, %v", result, err)
	}
	head, err := f.allocator.CurrentHead(context.Background())
	if err != nil || head.Frontier != result.Epoch {
		t.Fatalf("frontier = %#v, %v", head, err)
	}
	outcome, err := f.allocator.Outcome(context.Background(), result.Epoch)
	if err != nil || outcome.State != coordination.StateCommitted {
		t.Fatalf("outcome = %#v, %v", outcome, err)
	}
	retry, err := f.coordinator.Publish(context.Background(), request)
	if err != nil || retry.Epoch != result.Epoch || !retry.Unchanged {
		t.Fatalf("retry = %#v, %v", retry, err)
	}
	conflict := request
	conflict.LogicalDigest = coordination.Sum([]byte("different"))
	if _, err := f.coordinator.Publish(context.Background(), conflict); !errors.Is(err, ErrConflict) {
		t.Fatalf("different digest reuse = %v", err)
	}
}

func TestNoVisibilityBeforeOutcomeAndCheckpoint(t *testing.T) {
	f := newFixture(t, nil)
	writer := &failOnceWriter{next: f.store}
	f.coordinator.writer = writer
	request := publication(f, coordination.Sum([]byte("logical")))
	if _, err := f.coordinator.Publish(context.Background(), request); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("first publish = %v", err)
	}
	snapshot, err := f.coordinator.Inspect(context.Background(), request.TXN)
	if err != nil || snapshot.Root.State != coordination.StateEpochReserved {
		t.Fatalf("snapshot before publication = %#v, %v", snapshot, err)
	}
	if _, err := f.allocator.Outcome(context.Background(), snapshot.Root.Epoch); !errors.Is(err, allocator.ErrNotFound) {
		t.Fatalf("outcome exists before publication: %v", err)
	}
	head, _ := f.allocator.CurrentHead(context.Background())
	if head.Frontier != 0 {
		t.Fatalf("frontier advanced early: %d", head.Frontier)
	}
	result, err := f.coordinator.Publish(context.Background(), request)
	if err != nil || result.Epoch != snapshot.Root.Epoch {
		t.Fatalf("retry publish = %#v, %v", result, err)
	}
}

func TestFaultUnknownConverges(t *testing.T) {
	for position := 0; position < 18; position++ {
		t.Run(fmt.Sprint(position), func(t *testing.T) {
			f := newFixture(t, nil)
			faults := make([]FaultMode, position+1)
			faults[position] = FaultUnknownAfter
			f.store.Inject(faults...)
			request := publication(f, coordination.Sum([]byte("logical")))
			result, err := f.coordinator.Publish(context.Background(), request)
			if err != nil {
				f.store.ClearFaults()
				result, err = f.coordinator.Publish(context.Background(), request)
			}
			if err != nil || result.Epoch != 1 {
				t.Fatalf("fault position %d = %#v, %v", position, result, err)
			}
			head, _ := f.allocator.CurrentHead(context.Background())
			if head.NextEpoch != 2 || head.Frontier != 1 {
				t.Fatalf("allocator duplicated or skipped epoch: %#v", head)
			}
		})
	}
}

func TestConcurrentIdenticalPublicationConverges(t *testing.T) {
	f := newFixture(t, nil)
	request := publication(f, coordination.Sum([]byte("logical")))
	const workers = 8
	results := make(chan Result, workers)
	errs := make(chan error, workers)
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			result, err := f.coordinator.Publish(context.Background(), request)
			results <- result
			errs <- err
		}()
	}
	wg.Wait()
	close(results)
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent identical publication = %v", err)
		}
	}
	for result := range results {
		if result.Epoch != 1 {
			t.Fatalf("concurrent epoch = %d", result.Epoch)
		}
	}
	head, _ := f.allocator.CurrentHead(context.Background())
	if head.NextEpoch != 2 || head.Frontier != 1 {
		t.Fatalf("concurrent allocator state = %#v", head)
	}
}

func TestRecoveryTakeoverFencesReservationAndGuard(t *testing.T) {
	f := newFixture(t, nil)
	f.coordinator.writer = &failOnceWriter{next: f.store}
	request := publication(f, coordination.Sum([]byte("logical")))
	if _, err := f.coordinator.Publish(context.Background(), request); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("initial publish = %v", err)
	}
	before, _ := f.coordinator.Inspect(context.Background(), request.TXN)
	f.now = request.LeaseUntil.Add(time.Second)
	result, err := f.coordinator.Recover(
		context.Background(), request.TXN, coordination.OwnerID("recovery"),
		f.now.Add(time.Minute), f.authority,
	)
	if err != nil {
		t.Fatal(err)
	}
	after, _ := f.coordinator.Inspect(context.Background(), request.TXN)
	if result.Epoch != before.Root.Epoch || after.Root.Fence != before.Root.Fence+1 {
		t.Fatalf("takeover = before %#v after %#v result %#v", before, after, result)
	}
	reservation, err := f.allocator.Reservation(context.Background(), result.Epoch)
	if err != nil || reservation.State != coordination.StateCommitted ||
		reservation.Fence != after.Root.Fence {
		t.Fatalf("reservation takeover = %#v, %v", reservation, err)
	}
}

func TestCommittedMissingPhysicalCellQuarantines(t *testing.T) {
	f := newFixture(t, nil)
	request := publication(f, coordination.Sum([]byte("logical")))
	result, err := f.coordinator.Publish(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	f.store.DeletePhysical(result.Epoch, f.plan.Cells[0])
	if _, err := f.coordinator.Publish(context.Background(), request); !errors.Is(err, ErrQuarantined) {
		t.Fatalf("missing committed cell = %v", err)
	}
	if !f.store.Quarantined(coordination.DomainID("domain"), request.TXN) {
		t.Fatal("missing committed cell was not quarantined")
	}
}

func TestPublicErrorRedactsCoordinationDetails(t *testing.T) {
	err := PublicError(fmt.Errorf("%w: row=%q visibility=%q", ErrConflict, "secret-row", "secret-label"))
	if !shoal.IsErrorCode(err, shoal.ErrorConflict) ||
		err.Error() != "conflict: transaction conflicts with existing state" {
		t.Fatalf("public error = %v", err)
	}
}
