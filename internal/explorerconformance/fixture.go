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
	"sync"
	"time"

	"github.com/phrocker/shoal-oss/pkg/explorer/coordination"
	"github.com/phrocker/shoal-oss/pkg/explorer/coordination/allocator"
	"github.com/phrocker/shoal-oss/pkg/explorer/coordination/guard"
	"github.com/phrocker/shoal-oss/pkg/explorer/coordination/transaction"
)

// skewClock is a mutable, non-monotonic clock. It lets the harness advance,
// rewind, or freeze time to exercise lease and fence evaluation under skew.
type skewClock struct {
	mu  sync.Mutex
	now time.Time
}

func newSkewClock(start time.Time) *skewClock {
	return &skewClock{now: start.UTC()}
}

func (c *skewClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *skewClock) Set(t time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = t.UTC()
}

func (c *skewClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d).UTC()
}

// fixedAuthority / fixedRetirement / statusProxy mirror the minimal guard
// dependencies used by the transaction coordinator's own tests, reconstructed
// here from exported interfaces so the harness can build a coordinator from
// outside the transaction package.
type fixedAuthority struct{ value guard.Authority }

func (a fixedAuthority) Current(context.Context, coordination.DomainID) (guard.Authority, error) {
	return a.value, nil
}

type fixedRetirement struct{}

func (fixedRetirement) Retired(context.Context, coordination.DomainID, guard.Entity) (bool, coordination.Generation, error) {
	return false, 1, nil
}

type statusProxy struct{ coordinator *transaction.Coordinator }

func (p *statusProxy) Status(ctx context.Context, domain coordination.DomainID, txn coordination.TXN) (guard.TxnDisposition, error) {
	return p.coordinator.Status(ctx, domain, txn)
}

type staticMaterializer struct{ plan transaction.Plan }

func (m staticMaterializer) Materialize(context.Context, transaction.MaterializeRequest) (transaction.Plan, error) {
	return m.plan, nil
}

// coordinatorFixture is a fully wired allocator + guard + transaction
// coordinator over a chosen control-plane Store and a physical MemoryStore,
// with an injectable clock. It reproduces the wiring of the transaction
// package's internal test fixture using only exported API.
type coordinatorFixture struct {
	physical    *transaction.MemoryStore
	control     allocator.Store
	clock       *skewClock
	allocator   *allocator.Client
	guards      *guard.Client
	coordinator *transaction.Coordinator
	proxy       *statusProxy
	plan        transaction.Plan
	authority   transaction.Authority
	start       time.Time
}

const (
	fixtureDomain = "domain"
	fixtureOwner  = "worker"
	fixtureTXN    = "txn"
)

// newCoordinatorFixture builds the fixture. control is the Store handed to the
// allocator, guard, and coordinator (it may be a FaultStore wrapping physical);
// physical backs the physical writer/verifier/quarantine. clock drives all
// three clients.
func newCoordinatorFixture(control allocator.Store, physical *transaction.MemoryStore, clock *skewClock) (*coordinatorFixture, error) {
	return newCoordinatorFixtureWithWriter(control, physical, clock, physical)
}

// newCoordinatorFixtureWithWriter is newCoordinatorFixture with an overridable
// physical writer, used to inject data-plane failures that leave a transaction
// mid-flight for the recovery path to converge.
func newCoordinatorFixtureWithWriter(
	control allocator.Store,
	physical *transaction.MemoryStore,
	clock *skewClock,
	writer transaction.PhysicalWriter,
) (*coordinatorFixture, error) {
	f := &coordinatorFixture{
		physical: physical,
		control:  control,
		clock:    clock,
		start:    clock.Now(),
		authority: transaction.Authority{
			Generation: 3, Fence: 4, Holder: coordination.OwnerID("authority"),
			Mode: coordination.WriterModeAccumuloPrimary, RetentionGeneration: 2, HistoryFloor: 1,
		},
	}
	allocatorClient, err := allocator.New(allocator.Config{
		Domain: coordination.DomainID(fixtureDomain), Store: control,
		Clock: clock.Now, MaxRetries: 5, RetryBackoff: time.Nanosecond,
	})
	if err != nil {
		return nil, err
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
		return nil, err
	}
	f.proxy = &statusProxy{}
	guardClient, err := guard.New(guard.Config{
		Domain: coordination.DomainID(fixtureDomain), Store: control,
		Authority: fixedAuthority{guard.Authority{
			Generation: 3, Fence: 4, RetentionGeneration: 2, HistoryFloor: 1,
		}},
		Retirement: fixedRetirement{}, Transactions: f.proxy,
		Clock: clock.Now, MaxRetries: 5, RetryBackoff: time.Nanosecond,
	})
	if err != nil {
		return nil, err
	}
	f.guards = guardClient
	f.plan = conformancePlan()
	coordinator, err := transaction.New(transaction.Config{
		Domain: coordination.DomainID(fixtureDomain), Store: control, Allocator: f.allocator,
		Guards: f.guards, Materializer: staticMaterializer{f.plan},
		Writer: writer, Verifier: physical, Quarantine: physical,
		Clock: clock.Now, MaxRetries: 5, RetryBackoff: time.Nanosecond,
	})
	if err != nil {
		return nil, err
	}
	f.coordinator = coordinator
	f.proxy.coordinator = coordinator
	return f, nil
}

// newMemoryFixture is the common case: a plain MemoryStore backing both the
// control and physical planes.
func newMemoryFixture(clock *skewClock) (*coordinatorFixture, error) {
	store := transaction.NewMemoryStore()
	return newCoordinatorFixture(store, store, clock)
}

func (f *coordinatorFixture) publication(digest coordination.Digest) transaction.Publication {
	return transaction.Publication{
		TXN: coordination.TXN(fixtureTXN), Token: []byte("token"), LogicalDigest: digest,
		Owner: coordination.OwnerID(fixtureOwner), LeaseUntil: f.clock.Now().Add(time.Minute),
		Authority: f.authority,
	}
}

func (f *coordinatorFixture) defaultPublication() transaction.Publication {
	return f.publication(coordination.Sum([]byte("logical")))
}

// conformancePlan reproduces the transaction test plan using exported API. Its
// logical digest is Sum("logical"), matching defaultPublication.
func conformancePlan() transaction.Plan {
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
		panic(err)
	}
	return transaction.Plan{
		Chunks: chunks,
		Cells:  []transaction.PhysicalCell{{Entry: entry, Value: value, Visibility: visibility}},
		Guards: []transaction.GuardPlan{{
			Entity: guard.Entity{Kind: 'D', ID: coordination.EntityID("document")},
			Mode:   guard.ModeAbsentOrIdentical, DesiredState: guard.StateLive,
			DesiredWinnerID: []byte("revision"), DesiredDigest: logical,
			LPART: coordination.LPART("policy"), LogicalPolicyID: []byte("policy-id"),
			RetirementGeneration: 1, PhysicalDigest: physical,
		}},
		Copies: []transaction.CommitCopy{{
			LPART: coordination.LPART("policy"), CopyGeneration: 1,
			VisibilityDigest: coordination.Sum(visibility), LogicalDigest: logical,
			PhysicalCopyDigest: physical, RequiredIndexFamilies: []coordination.Family{coordination.Family("lexical")},
			Visibility: visibility,
		}},
		Results: []coordination.ResultIdentity{{Kind: []byte("document"), ID: []byte("document")}},
	}
}

func fixtureStart() time.Time {
	return time.Date(2026, 8, 27, 16, 0, 0, 0, time.UTC)
}
