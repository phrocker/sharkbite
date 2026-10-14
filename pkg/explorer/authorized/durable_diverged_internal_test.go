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

package authorized

import (
	"context"
	"testing"
	"time"

	"github.com/phrocker/shoal-oss/pkg/explorer/auth"
	"github.com/phrocker/shoal-oss/pkg/graph"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

// TestDurablePolicyStoreFailsClosedWhenAWriteThroughFails pins that a failed
// write-through, which leaves the memory store ahead of the engine, makes the
// durable store refuse every read and write until it is reopened. Otherwise
// a registration that never persisted could authorize as if it had (#665):
// a relabel whose registration failed to persist would read as already
// registered, and the next ingest would need only the new rule.
func TestDurablePolicyStoreFailsClosedWhenAWriteThroughFails(t *testing.T) {
	dir := t.TempDir()
	store, err := OpenDurablePolicyStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	seedDurableStore(t, store)
	ctx := context.Background()
	before, ok, err := store.CurrentRevision(ctx, "document")
	if err != nil || !ok {
		t.Fatalf("seeded registration: %v %v", ok, err)
	}
	// Make every engine write fail.
	if err := store.engine.Close(); err != nil {
		t.Fatal(err)
	}
	rule := batchTestRule(t, "source", "policy")
	err = store.PutRevision(ctx, RevisionRegistration{
		DocumentID:    "document",
		RevisionID:    "revision-2",
		NodeIDs:       []shoal.ID{"document"},
		ContentDigest: auth.DigestBytes("test-content", []byte("revision-2")),
		Rule:          rule,
		Current:       true,
	})
	if !shoal.IsErrorCode(err, shoal.ErrorUnavailable) {
		t.Fatalf("PutRevision with a failing engine = %v, want unavailable", err)
	}
	if _, _, err := store.CurrentRevision(ctx, "document"); !shoal.IsErrorCode(err, shoal.ErrorUnavailable) {
		t.Fatalf("read after a failed write-through = %v, want unavailable", err)
	}
	if _, _, err := store.SourceClaim(ctx, "file:///anything"); !shoal.IsErrorCode(err, shoal.ErrorUnavailable) {
		t.Fatalf("claim read after a failed write-through = %v, want unavailable", err)
	}
	if _, err := store.AcquireMutation(ctx); !shoal.IsErrorCode(err, shoal.ErrorUnavailable) {
		t.Fatalf("mutation after a failed write-through = %v, want unavailable", err)
	}
	if store.HasRegistrations() {
		t.Fatal("a diverged store still reports registrations")
	}
	_ = store.lock.Close()
	store.closed = true

	reopened, err := OpenDurablePolicyStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
	after, ok, err := reopened.CurrentRevision(ctx, "document")
	if err != nil || !ok || after.RevisionID != before.RevisionID {
		t.Fatalf("reopened registration = %+v %v %v, want the persisted %s",
			after, ok, err, before.RevisionID)
	}
}

// divergenceCase prepares a store, then returns the write that is made to
// fail by closing the engine underneath it.
type divergenceCase struct {
	name string
	// prepare runs with a working engine and returns the write under test.
	prepare func(t *testing.T, store *DurablePolicyStore) func() error
	// diverges is false for a write that persists before it applies, whose
	// failure leaves memory and disk in agreement.
	diverges bool
}

func divergenceCases(t *testing.T) []divergenceCase {
	ctx := context.Background()
	rule := batchTestRule(t, "source", "policy")
	labelled, err := LabelRule(rule.components()[0], []string{"secret"})
	if err != nil {
		t.Fatal(err)
	}
	return []divergenceCase{
		{"source claim CAS", func(t *testing.T, store *DurablePolicyStore) func() error {
			return func() error {
				_, err := store.CompareAndSwapSourceClaim(ctx, "file:///new", nil, rule)
				return err
			}
		}, true},
		{"commit source claim", func(t *testing.T, store *DurablePolicyStore) func() error {
			claim, err := store.CompareAndSwapSourceClaim(ctx, "file:///commit", nil, rule)
			if err != nil {
				t.Fatal(err)
			}
			return func() error { return store.CommitSourceClaim(ctx, claim) }
		}, true},
		{"pend source claim", func(t *testing.T, store *DurablePolicyStore) func() error {
			claim, err := store.CompareAndSwapSourceClaim(ctx, "file:///pend", nil, rule)
			if err != nil {
				t.Fatal(err)
			}
			return func() error { return store.PendSourceClaim(ctx, claim) }
		}, true},
		{"rollback source claim", func(t *testing.T, store *DurablePolicyStore) func() error {
			claim, err := store.CompareAndSwapSourceClaim(ctx, "file:///rollback", nil, rule)
			if err != nil {
				t.Fatal(err)
			}
			return func() error { return store.RollbackSourceClaim(ctx, claim) }
		}, true},
		{"tighten rule", func(t *testing.T, store *DurablePolicyStore) func() error {
			return func() error {
				_, err := store.TightenRule(ctx, nil, RuleTightening{
					DocumentID: "document", RevisionID: "revision",
					SourceURI: "file:///unclaimed", From: rule, To: labelled,
				})
				return err
			}
		}, true},
		{"label migration marker", func(t *testing.T, store *DurablePolicyStore) func() error {
			return func() error {
				return store.PutLabelMigration(ctx, LabelMigrationRecord{Version: 1})
			}
		}, true},
		{"co-occurrence", func(t *testing.T, store *DurablePolicyStore) func() error {
			return func() error {
				return store.StoreCoOccurrence(ctx, "identity", CoOccurrenceRecord{
					WindowStart: time.Date(2026, time.August, 26, 18, 0, 0, 0, time.UTC),
					Domains:     []string{"a"},
				})
			}
		}, true},
		{"reserve edge", func(t *testing.T, store *DurablePolicyStore) func() error {
			return func() error {
				return store.ReserveEdge(ctx, EdgeRegistration{
					Edge: graph.Edge{ID: "reserved", From: "document", To: "node-a",
						Type: "links", Weight: 1},
					Rule: rule,
				})
			}
		}, true},
		{"put edge", func(t *testing.T, store *DurablePolicyStore) func() error {
			return func() error {
				return store.PutEdge(ctx, EdgeRegistration{
					Edge: graph.Edge{ID: "new-edge", From: "document", To: "node-a",
						Type: "links", Weight: 1},
					Rule: rule,
				})
			}
		}, true},
		{"rollback edge reservation", func(t *testing.T, store *DurablePolicyStore) func() error {
			reservation := EdgeRegistration{
				Edge: graph.Edge{ID: "to-roll-back", From: "document", To: "node-a",
					Type: "links", Weight: 1},
				Rule: rule,
			}
			if err := store.ReserveEdge(ctx, reservation); err != nil {
				t.Fatal(err)
			}
			return func() error { return store.RollbackEdgeReservation(ctx, reservation) }
		}, true},
		{"declassification record (persisted before applied)", func(t *testing.T, store *DurablePolicyStore) func() error {
			return func() error {
				return store.RecordDeclassification(ctx, DeclassificationRecord{
					AttemptID: "a", Phase: DeclassificationIntent,
					OccurredAt: time.Date(2026, time.August, 26, 18, 0, 0, 0, time.UTC),
					SourceURI:  "file:///x",
					PriorRules: []AccessRule{labelled}, Rule: rule,
				})
			}
		}, false},
	}
}

// TestDurablePolicyStoreDivergesOnEveryWriteThrough pins the divergence rule
// for every durable write, not just PutRevision. A write applied to memory
// before it persists makes the store fail closed when its persist fails. A
// write persisted before it applies does not, because its failure leaves
// memory and disk in agreement. The OnDiverged hook fires exactly once.
func TestDurablePolicyStoreDivergesOnEveryWriteThrough(t *testing.T) {
	for _, test := range divergenceCases(t) {
		t.Run(test.name, func(t *testing.T) {
			store, err := OpenDurablePolicyStore(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			seedDurableStore(t, store)
			fired := 0
			store.OnDiverged(func() { fired++ })
			write := test.prepare(t, store)
			if err := store.engine.Close(); err != nil {
				t.Fatal(err)
			}
			if err := write(); err == nil {
				t.Fatal("a write with a failed persist succeeded")
			}
			_, _, readErr := store.CurrentRevision(context.Background(), "document")
			if test.diverges {
				if !store.Diverged() || fired != 1 ||
					!shoal.IsErrorCode(readErr, shoal.ErrorUnavailable) {
					t.Fatalf("diverged=%v fired=%d read=%v, want a fail-closed store signalled once",
						store.Diverged(), fired, readErr)
				}
				// A second failure does not signal again.
				_ = write()
				if fired != 1 {
					t.Fatalf("hook fired %d times", fired)
				}
			} else if store.Diverged() || fired != 0 || readErr != nil {
				t.Fatalf("diverged=%v fired=%d read=%v, want the store still serving",
					store.Diverged(), fired, readErr)
			}
			_ = store.lock.Close()
			store.closed = true
		})
	}
}

// TestDurablePolicyStoreOnDivergedAfterTheFact pins that a hook registered
// after the store diverged fires immediately.
func TestDurablePolicyStoreOnDivergedAfterTheFact(t *testing.T) {
	store, err := OpenDurablePolicyStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	seedDurableStore(t, store)
	if err := store.engine.Close(); err != nil {
		t.Fatal(err)
	}
	_ = store.PutLabelMigration(context.Background(), LabelMigrationRecord{Version: 1})
	fired := 0
	store.OnDiverged(func() { fired++ })
	if fired != 1 {
		t.Fatalf("late hook fired %d times, want 1", fired)
	}
	_ = store.lock.Close()
	store.closed = true
}

// TestDivergedStoreRefusesTighteningBeforeChangingMemory pins that a store
// that has already diverged refuses a tightening before it touches memory,
// even while its engine still accepts writes, so nothing it could not have
// persisted becomes visible.
func TestDivergedStoreRefusesTighteningBeforeChangingMemory(t *testing.T) {
	store, err := OpenDurablePolicyStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	seedDurableStore(t, store)
	rule := batchTestRule(t, "source", "policy")
	labelled, err := LabelRule(rule.components()[0], []string{"secret"})
	if err != nil {
		t.Fatal(err)
	}
	store.diverged.Store(true)
	if _, err := store.TightenRule(context.Background(), nil, RuleTightening{
		DocumentID: "document", RevisionID: "revision",
		SourceURI: "file:///unclaimed", From: rule, To: labelled,
	}); !shoal.IsErrorCode(err, shoal.ErrorUnavailable) {
		t.Fatalf("tightening a diverged store = %v, want unavailable", err)
	}
	stored, ok := store.memory.snapshotRevision(revisionKey{
		documentID: "document", revisionID: "revision"})
	if !ok || !stored.Rule.equal(rule) {
		t.Fatalf("a refused tightening changed memory: %+v", stored.Rule)
	}
	if err := store.writeRows(nil); !shoal.IsErrorCode(err, shoal.ErrorUnavailable) {
		t.Fatalf("writeRows on a diverged store = %v, want unavailable", err)
	}
}

// TestDurablePolicyStoreWriteAfterCloseIsNotADivergence pins that a write
// racing Close is refused without diverging or signalling: the process is
// shutting down, and the next open reloads from disk.
func TestDurablePolicyStoreWriteAfterCloseIsNotADivergence(t *testing.T) {
	store, err := OpenDurablePolicyStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	seedDurableStore(t, store)
	fired := 0
	store.OnDiverged(func() { fired++ })
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	rule := batchTestRule(t, "source", "policy")
	if err := store.PutEdge(context.Background(), EdgeRegistration{
		Edge: graph.Edge{ID: "after-close", From: "document", To: "node-a",
			Type: "links", Weight: 1},
		Rule: rule,
	}); !shoal.IsErrorCode(err, shoal.ErrorUnavailable) {
		t.Fatalf("write after close = %v, want unavailable", err)
	}
	if store.Diverged() || fired != 0 {
		t.Fatalf("a write after close diverged=%v fired=%d", store.Diverged(), fired)
	}
	// The refused write is not observable: a closed store serves no reads.
	if _, _, err := store.Edge(context.Background(), "after-close"); !shoal.IsErrorCode(err, shoal.ErrorUnavailable) {
		t.Fatalf("read after close = %v, want unavailable", err)
	}
}

// TestDivergedStoreRefusesPersistFirstWrites pins that a write which
// persists before it applies is refused too once the store has diverged, so
// nothing reaches disk from a store that no longer serves reads.
func TestDivergedStoreRefusesPersistFirstWrites(t *testing.T) {
	store, err := OpenDurablePolicyStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	rule := batchTestRule(t, "source", "policy")
	labelled, err := LabelRule(rule.components()[0], []string{"secret"})
	if err != nil {
		t.Fatal(err)
	}
	store.diverged.Store(true)
	if err := store.RecordDeclassification(context.Background(), DeclassificationRecord{
		AttemptID: "a", Phase: DeclassificationIntent,
		OccurredAt: time.Date(2026, time.August, 26, 18, 0, 0, 0, time.UTC),
		SourceURI:  "file:///x",
		PriorRules: []AccessRule{labelled}, Rule: rule,
	}); !shoal.IsErrorCode(err, shoal.ErrorUnavailable) {
		t.Fatalf("persist-first write on a diverged store = %v, want unavailable", err)
	}
	store.diverged.Store(false)
	if records, err := store.Declassifications(context.Background()); err != nil || len(records) != 0 {
		t.Fatalf("a refused write was recorded: %+v %v", records, err)
	}
}
