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
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/phrocker/shoal-oss/pkg/explorer/coordination"
	"github.com/phrocker/shoal-oss/pkg/explorer/coordination/allocator"
	"github.com/phrocker/shoal-oss/pkg/explorer/coordination/transaction"
)

// casProbeDepth bounds how many CompareAndMutate ordinals a single Publish is
// probed at. A full publish issues roughly a dozen CAS calls across the
// claim/plan/guard/commit/frontier stages; probing beyond that simply exercises
// the no-fault path.
const casProbeDepth = 24

// failOnceWriter fails the first physical write (after applying it) and then
// succeeds, leaving a transaction durably mid-flight for recovery to resume.
type failOnceWriter struct {
	next   transaction.PhysicalWriter
	mu     sync.Mutex
	failed bool
}

func (w *failOnceWriter) Write(ctx context.Context, epoch coordination.Epoch, cells []transaction.PhysicalCell) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if !w.failed {
		w.failed = true
		return transaction.ErrUnavailable
	}
	return w.next.Write(ctx, epoch, cells)
}

// committedInvariant asserts the allocator advanced exactly one epoch and that
// it is durably committed. This is the convergence oracle for every
// fault-driven publish.
func committedInvariant(f *coordinatorFixture, result transaction.Result) error {
	if result.Epoch != 1 {
		return fmt.Errorf("published epoch = %d, want 1", result.Epoch)
	}
	if len(result.Identities) != 1 {
		return fmt.Errorf("published identities = %d, want 1", len(result.Identities))
	}
	head, err := f.allocator.CurrentHead(context.Background())
	if err != nil {
		return fmt.Errorf("current head: %w", err)
	}
	if head.NextEpoch != 2 || head.Frontier != 1 {
		return fmt.Errorf("allocator duplicated or skipped an epoch: NextEpoch=%d Frontier=%d", head.NextEpoch, head.Frontier)
	}
	outcome, err := f.allocator.Outcome(context.Background(), result.Epoch)
	if err != nil {
		return fmt.Errorf("outcome: %w", err)
	}
	if outcome.State != coordination.StateCommitted {
		return fmt.Errorf("outcome state = %v, want committed", outcome.State)
	}
	return nil
}

// publishThroughFault injects a single CAS fault at the given ordinal, publishes
// once, and if that publish fails, clears the fault and republishes. It returns
// the converged result and whether the fault actually fired.
func publishThroughFault(fault CASFault, position int) (transaction.Result, bool, error) {
	mem := transaction.NewMemoryStore()
	fs := NewFaultStore(mem)
	clock := newSkewClock(fixtureStart())
	f, err := newCoordinatorFixture(fs, mem, clock)
	if err != nil {
		return transaction.Result{}, false, fmt.Errorf("fixture: %w", err)
	}
	request := f.defaultPublication()

	schedule := make([]CASFault, position+1)
	schedule[position] = fault
	fs.InjectCAS(schedule...)

	result, pubErr := f.coordinator.Publish(context.Background(), request)
	fired := fs.TotalUnknown() > 0
	if pubErr != nil {
		fs.Clear()
		result, pubErr = f.coordinator.Publish(context.Background(), request)
	}
	if pubErr != nil {
		return transaction.Result{}, fired, fmt.Errorf("publish did not converge: %w", pubErr)
	}
	if err := committedInvariant(f, result); err != nil {
		return result, fired, err
	}
	// Idempotent replay must return the same committed epoch.
	replay, err := f.coordinator.Publish(context.Background(), request)
	if err != nil {
		return result, fired, fmt.Errorf("idempotent replay: %w", err)
	}
	if replay.Epoch != result.Epoch {
		return result, fired, fmt.Errorf("replay epoch %d != %d", replay.Epoch, result.Epoch)
	}
	return result, fired, nil
}

// RunIndeterminateCASSuite is the centrepiece: it proves the 2PC protocol above
// the Store seam converges to a single committed epoch whether an
// ErrConditionalUnknown was reported for a mutation that DID apply or for one
// that did NOT, at every CAS ordinal of a publish.
func RunIndeterminateCASSuite(t *testing.T) {
	t.Helper()
	appliedFired, err := checkIndeterminateCAS(CASUnknownAfterApply)
	if err != nil {
		t.Fatalf("indeterminate CAS (unknown-after-apply): %v", err)
	}
	unappliedFired, err := checkIndeterminateCAS(CASUnknownWithoutApply)
	if err != nil {
		t.Fatalf("indeterminate CAS (unknown-without-apply): %v", err)
	}
	if appliedFired == 0 || unappliedFired == 0 {
		t.Fatalf("indeterminate CAS never fired a fault (applied=%d, unapplied=%d)", appliedFired, unappliedFired)
	}
	t.Logf("indeterminate CAS converged; faults fired: applied=%d unapplied=%d", appliedFired, unappliedFired)
}

func checkIndeterminateCAS(fault CASFault) (int, error) {
	fired := 0
	for position := 0; position < casProbeDepth; position++ {
		_, didFire, err := publishThroughFault(fault, position)
		if err != nil {
			return fired, fmt.Errorf("position %d: %w", position, err)
		}
		if didFire {
			fired++
		}
	}
	return fired, nil
}

// RunPartitionSuite injects definite unavailability (an error without the
// ErrConditionalUnknown sentinel) at every CAS ordinal of a publish and proves
// the SAFETY property of task item 4: whatever the outcome, the allocator
// frontier is never advanced to an epoch that is not durably committed, no
// second epoch ever becomes visible, and a converged publish is idempotent. It
// also proves that a data-plane partition is recovered by a fresh owner.
//
// It additionally regression-locks the LIVENESS property that this harness was
// built to discover: a bare unavailability at any CAS ordinal - including the
// epoch-reservation and frontier-advance CAS that previously wedged - must
// resume via retry or recovery to a single committed epoch. The set of wedged
// ordinals and the subset that leaks an epoch are pinned (asserted, not merely
// logged), so a regression that reintroduces the gap fails this suite loudly
// and forces the harness to be updated deliberately.
func RunPartitionSuite(t *testing.T) {
	t.Helper()
	wedged, leaked, err := checkControlPlaneUnavailability()
	if err != nil {
		t.Fatalf("partition safety: %v", err)
	}
	if err := checkRecoveryConverges(); err != nil {
		t.Fatalf("recovery convergence: %v", err)
	}
	if !equalInts(wedged, expectedWedgedOrdinals) {
		t.Fatalf("wedged CAS ordinals = %v, want %v; the discovered liveness gap changed - "+
			"if the resumability gap was fixed this is expected, update the harness deliberately", wedged, expectedWedgedOrdinals)
	}
	if !equalInts(leaked, expectedEpochLeakOrdinals) {
		t.Fatalf("epoch-leak ordinals = %v, want %v; the head-damage profile of the gap changed - "+
			"update the harness deliberately", leaked, expectedEpochLeakOrdinals)
	}
	if len(wedged) == 0 {
		t.Logf("RESUMABILITY HOLDS (regression-locked): definite unavailability at every probed CAS "+
			"ordinal converged to a single committed epoch via retry or recovery, with no epoch leak "+
			"(NextEpoch=%d) and no premature visibility. The previously discovered liveness gap at the "+
			"epoch-reservation and frontier-advance CAS is closed.", cleanNextEpoch)
	} else {
		t.Logf("LIVENESS GAP (regression-locked): bare unavailability wedged the transaction at "+
			"CAS ordinals %v with no convergence via retry or recovery. Frontier stayed 0 at every wedged "+
			"ordinal (no visibility corruption). Head damage: ordinal(s) %v leaked one epoch "+
			"(NextEpoch=%d, a reserved-but-unused epoch); the remaining wedged ordinal(s) left NextEpoch=%d "+
			"(no leak, only the frontier lags). Reported for the owner to triage.",
			wedged, leaked, wedgedNextEpochMax, cleanNextEpoch)
	}
}

// Regression-locked behaviour of the control-plane unavailability sweep. The
// resumability gap this harness discovered (PR #286) has since been fixed
// (PR fix/cas-resumability): a definite unavailability at every probed CAS
// ordinal now resumes to a single committed epoch, so no ordinal wedges and none
// leaks an epoch. These sets are pinned empty so a regression that reintroduces
// a wedge or leak breaks this suite loudly. (Historically ordinal 6 was the
// epoch-reservation CAS - wedged in GUARDS_ACQUIRED and leaked a reserved epoch;
// ordinal 13 was the frontier-advance CAS - wedged in COMMITTED, no leak.)
var (
	expectedWedgedOrdinals    = []int{}
	expectedEpochLeakOrdinals = []int{}
)

// cleanNextEpoch is the allocator NextEpoch after a single committed epoch (one
// epoch reserved). wedgedNextEpochMax bounds how far NextEpoch may run ahead at
// a wedged ordinal: at most one additional epoch may leak (NextEpoch=3, observed
// only at the epoch-reservation CAS). Anything beyond is unaccounted leakage.
const (
	cleanNextEpoch     = 2
	wedgedNextEpochMax = 3
)

func equalInts(a, b []int) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// convergenceOutcome captures how a publish under an injected fault ultimately
// resolved, for safety classification.
type convergenceOutcome struct {
	converged bool
	frontier  coordination.Epoch
	nextEpoch coordination.Epoch
}

// checkControlPlaneUnavailability sweeps a bare unavailability across CAS
// ordinals. For each ordinal it enforces SAFETY (a converged publish is fully
// consistent; a wedged publish never advanced the frontier) and a head-state
// BOUND (a wedged publish leaves NextEpoch within [cleanNextEpoch,
// wedgedNextEpochMax], i.e. at most one leaked epoch). It returns the ordinals
// that failed to converge (the liveness gap) and the subset of those that leaked
// an epoch. A non-nil error means a genuine corruption was observed and the
// suite must fail.
func checkControlPlaneUnavailability() (wedged, leaked []int, err error) {
	for position := 0; position < casProbeDepth; position++ {
		outcome, fired, perr := publishUnderBareUnavailability(position)
		if perr != nil {
			return nil, nil, fmt.Errorf("position %d: %w", position, perr)
		}
		if !fired {
			continue
		}
		if outcome.converged {
			// A converged publish must be fully consistent.
			if outcome.frontier != 1 || outcome.nextEpoch != cleanNextEpoch {
				return nil, nil, fmt.Errorf("position %d converged with inconsistent allocator head: frontier=%d next=%d", position, outcome.frontier, outcome.nextEpoch)
			}
			continue
		}
		// Wedged. SAFETY: nothing may have become visible.
		if outcome.frontier != 0 {
			return nil, nil, fmt.Errorf("position %d wedged but advanced the frontier to %d (visibility corruption)", position, outcome.frontier)
		}
		// BOUND: at most one epoch may have leaked at a wedged ordinal.
		if outcome.nextEpoch < cleanNextEpoch || outcome.nextEpoch > wedgedNextEpochMax {
			return nil, nil, fmt.Errorf("position %d wedged with NextEpoch=%d outside [%d,%d] (unaccounted epoch leakage)", position, outcome.nextEpoch, cleanNextEpoch, wedgedNextEpochMax)
		}
		wedged = append(wedged, position)
		if outcome.nextEpoch == wedgedNextEpochMax {
			leaked = append(leaked, position)
		}
	}
	return wedged, leaked, nil
}

// publishUnderBareUnavailability injects CASUnavailable at position, then makes
// a best-effort attempt to converge via outer retry and then lease-expiry
// recovery. It returns the final allocator state and whether the fault fired.
func publishUnderBareUnavailability(position int) (convergenceOutcome, bool, error) {
	ctx := context.Background()
	mem := transaction.NewMemoryStore()
	fs := NewFaultStore(mem)
	clock := newSkewClock(fixtureStart())
	f, err := newCoordinatorFixture(fs, mem, clock)
	if err != nil {
		return convergenceOutcome{}, false, fmt.Errorf("fixture: %w", err)
	}
	request := f.defaultPublication()
	schedule := make([]CASFault, position+1)
	schedule[position] = CASUnavailable
	fs.InjectCAS(schedule...)

	result, pubErr := f.coordinator.Publish(ctx, request)
	_, _, unavailable, _, _ := fs.EmittedFaults()
	fired := unavailable > 0
	fs.Clear()

	converged := pubErr == nil
	if converged {
		if err := committedInvariant(f, result); err != nil {
			return convergenceOutcome{}, fired, fmt.Errorf("clean publish inconsistent: %w", err)
		}
	}

	// Best-effort outer retry by the same owner.
	if !converged {
		for i := 0; i < 3 && !converged; i++ {
			if _, err := f.coordinator.Publish(ctx, request); err == nil {
				converged = true
			}
		}
	}
	// Best-effort recovery by a fresh owner after lease expiry.
	if !converged {
		clock.Set(request.LeaseUntil.Add(time.Second))
		for i := 0; i < 3 && !converged; i++ {
			if _, err := f.coordinator.Recover(ctx, request.TXN, coordination.OwnerID("recovery"), clock.Now().Add(time.Minute), f.authority); err == nil {
				converged = true
			}
			clock.Advance(time.Minute)
		}
	}

	// Idempotency: once converged, further publishes must not change the epoch.
	if converged {
		if replay, err := f.coordinator.Publish(ctx, request); err != nil || replay.Epoch != 1 {
			return convergenceOutcome{}, fired, fmt.Errorf("post-convergence replay = epoch %d, %v", replay.Epoch, err)
		}
	}

	head, err := f.allocator.CurrentHead(ctx)
	if err != nil {
		return convergenceOutcome{}, fired, fmt.Errorf("current head: %w", err)
	}
	return convergenceOutcome{converged: converged, frontier: head.Frontier, nextEpoch: head.NextEpoch}, fired, nil
}

// checkRecoveryConverges leaves a transaction mid-flight via a data-plane
// failure, expires its lease under the injected clock, and verifies that a
// fresh owner recovers it to a committed epoch with a bumped fence.
func checkRecoveryConverges() error {
	ctx := context.Background()
	mem := transaction.NewMemoryStore()
	clock := newSkewClock(fixtureStart())
	writer := &failOnceWriter{next: mem}
	f, err := newCoordinatorFixtureWithWriter(mem, mem, clock, writer)
	if err != nil {
		return fmt.Errorf("fixture: %w", err)
	}
	request := f.defaultPublication()
	if _, err := f.coordinator.Publish(ctx, request); !errors.Is(err, transaction.ErrUnavailable) {
		return fmt.Errorf("mid-flight publish = %v, want ErrUnavailable", err)
	}
	before, err := f.coordinator.Inspect(ctx, request.TXN)
	if err != nil {
		return fmt.Errorf("inspect mid-flight: %w", err)
	}
	if before.Root.State.Terminal() {
		return fmt.Errorf("transaction terminalised before recovery: %v", before.Root.State)
	}
	// Expire the lease and let a new owner take over.
	clock.Set(request.LeaseUntil.Add(time.Second))
	result, err := f.coordinator.Recover(
		ctx, request.TXN, coordination.OwnerID("recovery"),
		clock.Now().Add(time.Minute), f.authority,
	)
	if err != nil {
		return fmt.Errorf("recover: %w", err)
	}
	after, err := f.coordinator.Inspect(ctx, request.TXN)
	if err != nil {
		return fmt.Errorf("inspect after recovery: %w", err)
	}
	if after.Root.Fence != before.Root.Fence+1 {
		return fmt.Errorf("recovery did not bump the fence: before=%d after=%d", before.Root.Fence, after.Root.Fence)
	}
	if err := committedInvariant(f, result); err != nil {
		return fmt.Errorf("post-recovery: %w", err)
	}
	return nil
}

// RunClockSkewSuite verifies that lease takeover is gated strictly by the
// injectable clock: a competing owner is refused while the lease is live and
// admitted only once the clock crosses the lease boundary, with the fence
// advancing monotonically even when the clock is rewound in between.
func RunClockSkewSuite(t *testing.T) {
	t.Helper()
	if err := checkClockSkew(); err != nil {
		t.Fatalf("clock skew: %v", err)
	}
}

// leaseTakeoverGated builds a mid-flight transaction and attempts a competing
// recovery with the clock reported as evalClock. It returns whether the
// takeover was admitted, the resulting fence (when admitted), and the original
// fence. This is the falsifiable core shared by the positive suite and the
// clock-skew mutation test.
func leaseTakeoverGated(evalClock time.Time) (admitted bool, beforeFence, afterFence coordination.Fence, err error) {
	ctx := context.Background()
	mem := transaction.NewMemoryStore()
	clock := newSkewClock(fixtureStart())
	writer := &failOnceWriter{next: mem}
	f, buildErr := newCoordinatorFixtureWithWriter(mem, mem, clock, writer)
	if buildErr != nil {
		return false, 0, 0, fmt.Errorf("fixture: %w", buildErr)
	}
	request := f.defaultPublication()
	if _, pubErr := f.coordinator.Publish(ctx, request); !errors.Is(pubErr, transaction.ErrUnavailable) {
		return false, 0, 0, fmt.Errorf("mid-flight publish = %v, want ErrUnavailable", pubErr)
	}
	before, inspErr := f.coordinator.Inspect(ctx, request.TXN)
	if inspErr != nil {
		return false, 0, 0, fmt.Errorf("inspect: %w", inspErr)
	}
	beforeFence = before.Root.Fence

	clock.Set(evalClock)
	_, recErr := f.coordinator.Recover(
		ctx, request.TXN, coordination.OwnerID("recovery"),
		request.LeaseUntil.Add(2*time.Minute), f.authority,
	)
	if recErr == nil {
		after, inspErr := f.coordinator.Inspect(ctx, request.TXN)
		if inspErr != nil {
			return false, beforeFence, 0, fmt.Errorf("inspect after recover: %w", inspErr)
		}
		return true, beforeFence, after.Root.Fence, nil
	}
	if errors.Is(recErr, transaction.ErrUnavailable) {
		return false, beforeFence, beforeFence, nil
	}
	return false, beforeFence, 0, fmt.Errorf("recover: %w", recErr)
}

func checkClockSkew() error {
	start := fixtureStart()
	leaseUntil := start.Add(time.Minute)

	// While the lease is live (clock before expiry, even rewound before start),
	// a competing owner must be refused.
	for _, live := range []time.Time{start, start.Add(30 * time.Second), start.Add(-time.Hour)} {
		admitted, beforeFence, afterFence, err := leaseTakeoverGated(live)
		if err != nil {
			return err
		}
		if admitted {
			return fmt.Errorf("takeover admitted while lease live at clock=%s (fence %d->%d)", live, beforeFence, afterFence)
		}
	}

	// Once the clock crosses the lease boundary the takeover is admitted and the
	// fence advances by exactly one.
	admitted, beforeFence, afterFence, err := leaseTakeoverGated(leaseUntil.Add(time.Second))
	if err != nil {
		return err
	}
	if !admitted {
		return fmt.Errorf("takeover refused after lease expiry")
	}
	if afterFence != beforeFence+1 {
		return fmt.Errorf("fence did not advance monotonically on expiry takeover: %d->%d", beforeFence, afterFence)
	}
	return nil
}

// RunRootLeaseDisagreementSuite constructs the exact owner/fence mismatch that
// transaction/store.go rejects and confirms it is caught, while a matching
// root/lease pair is accepted.
func RunRootLeaseDisagreementSuite(t *testing.T) {
	t.Helper()
	if err := checkRootLeaseDisagreement(); err != nil {
		t.Fatalf("root/lease disagreement: %v", err)
	}
}

func writeRawTxn(ctx context.Context, store allocator.Store, root coordination.TxnRootV3, lease coordination.TxnLeaseV1) error {
	row, err := coordination.TxnRow(coordination.DomainID(fixtureDomain), coordination.TXN(fixtureTXN))
	if err != nil {
		return err
	}
	rootCoord := allocator.Coordinate{Row: row, Family: []byte("s"), Qualifier: []byte("root")}
	leaseCoord := allocator.Coordinate{Row: row, Family: []byte("s"), Qualifier: []byte("lease")}
	rootBytes, err := coordination.MarshalTxnRootV3(root)
	if err != nil {
		return fmt.Errorf("marshal root: %w", err)
	}
	leaseBytes, err := coordination.MarshalTxnLeaseV1(lease)
	if err != nil {
		return fmt.Errorf("marshal lease: %w", err)
	}
	status, err := store.CompareAndMutate(ctx, allocator.Mutation{
		Row: row,
		Conditions: []allocator.Condition{
			{Coordinate: rootCoord, Absent: true}, {Coordinate: leaseCoord, Absent: true},
		},
		Updates: []allocator.Update{
			{Coordinate: rootCoord, Value: rootBytes, Timestamp: int64(root.StateGeneration)},
			{Coordinate: leaseCoord, Value: leaseBytes, Timestamp: int64(lease.Generation)},
		},
	})
	if err != nil || status != allocator.StatusAccepted {
		return fmt.Errorf("raw txn write = %v, %v", status, err)
	}
	return nil
}

func checkRootLeaseDisagreement() error {
	ctx := context.Background()
	leaseUntil := fixtureStart().Add(time.Minute)
	baseRoot := func(owner string, fence coordination.Fence) coordination.TxnRootV3 {
		return coordination.TxnRootV3{
			State: coordination.StateClaimed, LogicalDigest: coordination.Sum([]byte("logical")),
			TokenHash: coordination.Sum([]byte("token")), Owner: coordination.OwnerID(owner),
			Fence: fence, StateGeneration: 1, WriterAuthorityGeneration: 3, RetentionGeneration: 2,
		}
	}
	baseLease := func(owner string, fence coordination.Fence) coordination.TxnLeaseV1 {
		return coordination.TxnLeaseV1{
			Generation: 1, Owner: coordination.OwnerID(owner), Fence: fence, LeaseUntil: leaseUntil,
		}
	}

	// Positive control: matching root/lease must be readable (the guard is not
	// vacuously rejecting everything).
	matching := transaction.NewMemoryStore()
	if err := writeRawTxn(ctx, matching, baseRoot("owner", 1), baseLease("owner", 1)); err != nil {
		return err
	}
	fMatch, err := newCoordinatorFixture(matching, matching, newSkewClock(fixtureStart()))
	if err != nil {
		return fmt.Errorf("matching fixture: %w", err)
	}
	if _, err := fMatch.coordinator.Inspect(ctx, coordination.TXN(fixtureTXN)); err != nil {
		return fmt.Errorf("matching root/lease was rejected: %w", err)
	}

	// Owner disagreement must be rejected as an internal inconsistency.
	for name, mismatch := range map[string]struct {
		root  coordination.TxnRootV3
		lease coordination.TxnLeaseV1
	}{
		"owner": {baseRoot("owner-a", 1), baseLease("owner-b", 1)},
		"fence": {baseRoot("owner", 1), baseLease("owner", 2)},
	} {
		store := transaction.NewMemoryStore()
		if err := writeRawTxn(ctx, store, mismatch.root, mismatch.lease); err != nil {
			return fmt.Errorf("%s mismatch seed: %w", name, err)
		}
		f, err := newCoordinatorFixture(store, store, newSkewClock(fixtureStart()))
		if err != nil {
			return fmt.Errorf("%s fixture: %w", name, err)
		}
		_, err = f.coordinator.Inspect(ctx, coordination.TXN(fixtureTXN))
		if !errors.Is(err, transaction.ErrInternal) {
			return fmt.Errorf("%s disagreement not caught: Inspect err = %v, want ErrInternal", name, err)
		}
	}
	return nil
}
