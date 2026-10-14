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
	"testing"
	"time"

	"github.com/phrocker/shoal-oss/pkg/explorer/coordination"
	"github.com/phrocker/shoal-oss/pkg/explorer/coordination/allocator"
)

// TestUnavailableBeforeResumesToSingleCommittedEpoch pins the control-plane
// resumability invariant surfaced by the store-conformance harness in PR #286:
// a CompareAndMutate that fails *definitely* (StatusUnknown with an error that
// is NOT allocator.ErrConditionalUnknown) at any CAS ordinal within a Publish
// must remain resumable. FaultUnavailableBefore fails before applying, so the
// mutation definitely did not apply; the Store contract withholds
// ErrConditionalUnknown there, which is contract-correct.
//
// For every injected ordinal the test drives convergence through the outer
// Publish retry and a lease-expiry Recover, then asserts BOTH:
//
//   - liveness: the transaction converges to a single committed epoch whose
//     frontier checkpoint has advanced; and
//   - safety: exactly one epoch is ever reserved (NextEpoch == 2, no leaked
//     reservation), the frontier never exceeds that committed epoch, and no
//     second epoch outcome is ever created.
//
// Before the fix this wedges at two ordinals: one where the epoch is reserved
// but never recorded into the txn root (leaking a reservation that then blocks
// the frontier forever), and one where the reservation is terminalized but its
// outcome CAS fails and is never retried (frontier lags forever).
func TestUnavailableBeforeResumesToSingleCommittedEpoch(t *testing.T) {
	const ordinals = 20
	for position := 0; position < ordinals; position++ {
		t.Run(fmt.Sprint(position), func(t *testing.T) {
			f := newFixture(t, nil)
			ctx := context.Background()

			faults := make([]FaultMode, position+1)
			faults[position] = FaultUnavailableBefore
			f.store.Inject(faults...)
			request := publication(f, coordination.Sum([]byte("logical")))

			// First attempt: the injected definite failure fires at `position`.
			// The fault may land before any state machine progress (nil error),
			// so we do not require a specific error here.
			if _, err := f.coordinator.Publish(ctx, request); err != nil {
				assertNoPrematureVisibility(t, f, position)
			}

			// Convergence: only the injected fault is transient, so clear the
			// queue and drive through Publish retry and lease-expiry Recover.
			f.store.ClearFaults()
			var epoch coordination.Epoch
			converged := false
			for round := 0; round < 8 && !converged; round++ {
				assertNoPrematureVisibility(t, f, position)
				if res, err := f.coordinator.Publish(ctx, request); err == nil {
					epoch = res.Epoch
					converged = true
					break
				}
				f.now = request.LeaseUntil.Add(time.Duration(round+1) * time.Second)
				res, err := f.coordinator.Recover(
					ctx, request.TXN, coordination.OwnerID("recovery"),
					f.now.Add(time.Minute), f.authority,
				)
				if err == nil {
					epoch = res.Epoch
					converged = true
					break
				}
				f.now = request.LeaseUntil
			}

			if !converged {
				snap, _ := f.coordinator.Inspect(ctx, request.TXN)
				head, _ := f.allocator.CurrentHead(ctx)
				t.Fatalf("fault ordinal %d wedged the transaction: rootState=%v rootEpoch=%d frontier=%d nextEpoch=%d",
					position, snap.Root.State, snap.Root.Epoch, head.Frontier, head.NextEpoch)
			}

			// Liveness: a single committed, checkpointed epoch.
			if epoch != 1 {
				t.Fatalf("fault ordinal %d converged to epoch %d, want 1", position, epoch)
			}
			head, err := f.allocator.CurrentHead(ctx)
			if err != nil {
				t.Fatalf("fault ordinal %d: CurrentHead: %v", position, err)
			}
			if head.Frontier != epoch {
				t.Fatalf("fault ordinal %d: frontier=%d, want %d (committed epoch not checkpointed)",
					position, head.Frontier, epoch)
			}
			outcome, err := f.allocator.Outcome(ctx, epoch)
			if err != nil || outcome.State != coordination.StateCommitted {
				t.Fatalf("fault ordinal %d: outcome for epoch %d = %#v, %v", position, epoch, outcome, err)
			}

			// Safety: no epoch leak. Exactly one epoch was ever reserved, and no
			// second reservation or outcome exists.
			if head.NextEpoch != epoch+1 {
				t.Fatalf("fault ordinal %d: NextEpoch=%d, want %d (epoch reservation leaked)",
					position, head.NextEpoch, epoch+1)
			}
			if _, err := f.allocator.Reservation(ctx, epoch+1); !errors.Is(err, allocator.ErrNotFound) {
				t.Fatalf("fault ordinal %d: reservation for epoch %d exists (leak): %v", position, epoch+1, err)
			}
			if _, err := f.allocator.Outcome(ctx, epoch+1); !errors.Is(err, allocator.ErrNotFound) {
				t.Fatalf("fault ordinal %d: outcome for epoch %d exists (second epoch visible): %v",
					position, epoch+1, err)
			}

			// Idempotency: a converged publish stays converged and unchanged.
			retry, err := f.coordinator.Publish(ctx, request)
			if err != nil || retry.Epoch != epoch || !retry.Unchanged {
				t.Fatalf("fault ordinal %d: idempotent re-publish = %#v, %v", position, retry, err)
			}
		})
	}
}

// assertNoPrematureVisibility enforces the standing safety invariant that the
// frontier never advances to an epoch that is not durably committed. It reads
// the head and requires that every checkpointed epoch has a committed outcome.
func assertNoPrematureVisibility(t *testing.T, f *fixture, position int) {
	t.Helper()
	head, err := f.allocator.CurrentHead(context.Background())
	if err != nil {
		t.Fatalf("fault ordinal %d: CurrentHead during convergence: %v", position, err)
	}
	for e := coordination.Epoch(1); e <= head.Frontier; e++ {
		outcome, err := f.allocator.Outcome(context.Background(), e)
		if err != nil || outcome.State != coordination.StateCommitted {
			t.Fatalf("fault ordinal %d: frontier=%d but epoch %d has no committed outcome (%#v, %v)",
				position, head.Frontier, e, outcome, err)
		}
	}
}
