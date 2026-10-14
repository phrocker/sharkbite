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
	"fmt"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/phrocker/shoal-oss/internal/cclient"
	"github.com/phrocker/shoal-oss/pkg/explorer"
	"github.com/phrocker/shoal-oss/pkg/explorer/coordination"
	"github.com/phrocker/shoal-oss/pkg/explorer/coordination/allocator"
	"github.com/phrocker/shoal-oss/pkg/explorer/coordination/guard"
	"github.com/phrocker/shoal-oss/pkg/explorer/coordination/transaction"
)

func TestRuntimeRejectsUnconfiguredPhysicalTablesBeforeIntentPersistence(t *testing.T) {
	config := testRuntimeConfig(t, testDirectory(t))
	runtime, err := Open(config)
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Close()

	intent := testIntent(
		t,
		config.Domain,
		"unconfigured-table",
		"token",
		"value",
		guard.ModeAbsentOrIdentical,
		0,
		coordination.Digest{},
	)
	intent.Cells[0].Table = DefaultCoordinationTable
	if _, err := runtime.Publish(
		context.Background(),
		Request{Intent: intent},
	); !errors.Is(err, transaction.ErrInvalid) ||
		errors.Is(err, ErrIndeterminatePublication) {
		t.Fatalf("publish to coordination table = %v", err)
	}
	if pending, err := runtime.PendingPublications(
		context.Background(),
	); err != nil || pending {
		t.Fatalf("pending publication after table rejection = %v, %v", pending, err)
	}

	record := testRecordPublication("document", "revision", "value", nil)
	record.Table = DefaultCoordinationTable
	if _, err := runtime.PublishRecord(
		context.Background(),
		record,
	); !errors.Is(err, transaction.ErrInvalid) {
		t.Fatalf("record publication to coordination table = %v", err)
	}
	if pending, err := runtime.PendingPublications(
		context.Background(),
	); err != nil || pending {
		t.Fatalf("pending record publication after table rejection = %v, %v", pending, err)
	}
}

func TestRuntimeRejectsCoordinationTableAsPhysicalTargetBeforeOpen(t *testing.T) {
	config := testRuntimeConfig(t, testDirectory(t))
	config.PhysicalTables = []string{DefaultCoordinationTable}
	if runtime, err := Open(config); !errors.Is(err, transaction.ErrInvalid) {
		if runtime != nil {
			_ = runtime.Close()
		}
		t.Fatalf("open with coordination physical target = %v", err)
	}

	config.PhysicalTables = []string{"records"}
	runtime, err := Open(config)
	if err != nil {
		t.Fatalf("open after rejected configuration = %v", err)
	}
	if err := runtime.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestAmbiguousIntentWriteAndFailedReadbackIsIndeterminate(t *testing.T) {
	config := testRuntimeConfig(t, testDirectory(t))
	runtime, err := Open(config)
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Close()

	faults := &ambiguousIntentStore{
		EngineStore: runtime.store,
		ambiguous:   true,
	}
	runtime.intents.store = faults
	intent := testIntent(
		t,
		config.Domain,
		"ambiguous-intent",
		"token",
		"value",
		guard.ModeAbsentOrIdentical,
		0,
		coordination.Digest{},
	)
	if _, err := runtime.Publish(
		context.Background(),
		Request{Intent: intent},
	); !errors.Is(err, ErrIndeterminatePublication) ||
		!errors.Is(err, allocator.ErrConditionalUnknown) {
		t.Fatalf("ambiguous intent publication = %v", err)
	}

	runtime.intents.store = runtime.store
	if pending, err := runtime.PendingPublications(
		context.Background(),
	); err != nil || !pending {
		t.Fatalf("pending ambiguous intent = %v, %v", pending, err)
	}
	if err := runtime.Recover(context.Background()); err != nil {
		t.Fatalf("recover ambiguous intent = %v", err)
	}
	if pending, err := runtime.PendingPublications(
		context.Background(),
	); err != nil || pending {
		t.Fatalf("pending publication after recovery = %v, %v", pending, err)
	}
}

func TestIntentLoadRejectsNoncanonicalDefaultFields(t *testing.T) {
	eng, store := openTestEngineStore(t, "coord")
	defer eng.Close()
	domain := coordination.DomainID("domain")
	intents, err := NewIntentStore(domain, nil, store)
	if err != nil {
		t.Fatal(err)
	}
	intent := testIntent(
		t,
		domain,
		"noncanonical",
		"token",
		"value",
		guard.ModeAbsentOrIdentical,
		0,
		coordination.Digest{},
	)
	intent.Cells[0].CopyGeneration = 0
	intent.Guards[0].DesiredState = 0
	intent.Guards[0].RetirementGeneration = 0
	normalized, _, digest, err := canonicalIntent(intent)
	if err != nil {
		t.Fatal(err)
	}
	if normalized.Cells[0].CopyGeneration == 0 ||
		normalized.Guards[0].DesiredState == 0 ||
		normalized.Guards[0].RetirementGeneration == 0 {
		t.Fatal("intent defaults were not normalized")
	}
	txn, err := DeriveTXN(domain, intent.Operation, intent.Token)
	if err != nil {
		t.Fatal(err)
	}
	record := storedIntent{
		Version:       intentVersion,
		Domain:        append(coordination.DomainID(nil), domain...),
		TXN:           append(coordination.TXN(nil), txn...),
		LogicalDigest: digest,
		Intent:        intent,
	}
	encoded, err := encodeStoredIntent(record)
	if err != nil {
		t.Fatal(err)
	}
	coordinate := intents.intentCoordinate(txn)
	status, err := store.CompareAndMutate(
		context.Background(),
		allocator.Mutation{
			Row:        coordinate.Row,
			Conditions: []allocator.Condition{{Coordinate: coordinate, Absent: true}},
			Updates: []allocator.Update{{
				Coordinate: coordinate,
				Value:      encoded,
				Timestamp:  intentVersion,
			}},
		},
	)
	if err != nil || status != allocator.StatusAccepted {
		t.Fatalf("write noncanonical intent = %v, %v", status, err)
	}
	if _, err := intents.Load(
		context.Background(),
		txn,
	); !errors.Is(err, transaction.ErrInternal) {
		t.Fatalf("load noncanonical intent = %v", err)
	}
}

func TestRecordBindingFailuresDoNotLeaveRecoverableOrphans(t *testing.T) {
	t.Run("invalid key", func(t *testing.T) {
		config := testRuntimeConfig(t, testDirectory(t))
		runtime, err := Open(config)
		if err != nil {
			t.Fatal(err)
		}
		defer runtime.Close()

		request := testRecordPublication("document", "revision", "value", nil)
		request.RecordKey = make([]byte, coordination.MaxOpaqueIDBytes+1)
		if _, err := runtime.PublishRecord(
			context.Background(),
			request,
		); !errors.Is(err, transaction.ErrInvalid) {
			t.Fatalf("oversized record key = %v", err)
		}
		if pending, err := runtime.PendingPublications(
			context.Background(),
		); err != nil || pending {
			t.Fatalf("pending publication after invalid key = %v, %v", pending, err)
		}
	})

	t.Run("definite conflict", func(t *testing.T) {
		config := testRuntimeConfig(t, testDirectory(t))
		runtime, err := Open(config)
		if err != nil {
			t.Fatal(err)
		}
		first := testRecordPublication("document-one", "revision-one", "one", nil)
		if _, err := runtime.PublishRecord(context.Background(), first); err != nil {
			t.Fatal(err)
		}
		second := testRecordPublication("document-two", "revision-two", "two", nil)
		second.RecordKey = append([]byte(nil), first.RecordKey...)
		if _, err := runtime.PublishRecord(
			context.Background(),
			second,
		); !errors.Is(err, transaction.ErrConflict) ||
			errors.Is(err, ErrIndeterminatePublication) {
			t.Fatalf("conflicting record binding = %v", err)
		}
		if pending, err := runtime.PendingPublications(
			context.Background(),
		); err != nil || pending {
			t.Fatalf("pending publication after binding conflict = %v, %v", pending, err)
		}
		if err := runtime.Close(); err != nil {
			t.Fatal(err)
		}

		reopened, err := Open(config)
		if err != nil {
			t.Fatal(err)
		}
		defer reopened.Close()
		head, err := reopened.RecordHead(
			context.Background(),
			second.EntityKind,
			second.EntityID,
		)
		if err != nil || head != nil {
			t.Fatalf("orphaned binding conflict was recovered = %#v, %v", head, err)
		}
	})
}

func TestPendingCandidateIndexIgnoresTombstonedHistory(t *testing.T) {
	eng, store := openTestEngineStore(t, "coord")
	defer eng.Close()
	domain := coordination.DomainID("domain")
	intents, err := NewIntentStore(domain, nil, store)
	if err != nil {
		t.Fatal(err)
	}
	records := make([]storedIntent, 0, 2)
	for _, token := range []string{"first", "second"} {
		record, _, err := intents.Put(
			context.Background(),
			testIntent(
				t,
				domain,
				"pending-page",
				token,
				token,
				guard.ModeAbsentOrIdentical,
				0,
				coordination.Digest{},
			),
		)
		if err != nil {
			t.Fatal(err)
		}
		records = append(records, record)
	}
	sort.Slice(records, func(i, j int) bool {
		return bytes.Compare(
			intents.intentRow(records[i].TXN),
			intents.intentRow(records[j].TXN),
		) < 0
	})
	if err := intents.Settle(
		context.Background(),
		records[0].TXN,
		records[0].LogicalDigest,
	); err != nil {
		t.Fatal(err)
	}
	intents.store = &scanFailIntentStore{EngineStore: store}
	candidates, next, err := intents.Candidates(context.Background(), nil, 1)
	if err != nil || len(candidates) != 1 ||
		!bytes.Equal(candidates[0], records[1].TXN) ||
		len(next) != 0 {
		t.Fatalf("live pending index = %#v, %x, %v", candidates, next, err)
	}
}

func TestPendingPublicationsUsesLoadedLiveIndex(t *testing.T) {
	config := testRuntimeConfig(t, testDirectory(t))
	runtime, err := Open(config)
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Close()
	intent := testIntent(
		t,
		config.Domain,
		"completed-history",
		"token",
		"value",
		guard.ModeAbsentOrIdentical,
		0,
		coordination.Digest{},
	)
	if _, err := runtime.Publish(
		context.Background(),
		Request{Intent: intent},
	); err != nil {
		t.Fatal(err)
	}
	runtime.intents.store = &scanFailIntentStore{EngineStore: runtime.store}
	if pending, err := runtime.PendingPublications(
		context.Background(),
	); err != nil || pending {
		t.Fatalf("indexed pending check = %v, %v", pending, err)
	}
}

func TestRecoveryAndPublicationUseSameLockOrder(t *testing.T) {
	config := testRuntimeConfig(t, testDirectory(t))
	runtime, err := Open(config)
	if err != nil {
		t.Fatal(err)
	}

	runtime.recoveryMu.Lock()
	recoverDone := make(chan error, 1)
	go func() {
		_, err := runtime.RecoverPage(context.Background())
		recoverDone <- err
	}()
	time.Sleep(20 * time.Millisecond)

	publishDone := make(chan error, 1)
	go func() {
		_, err := runtime.PublishRecord(
			context.Background(),
			testRecordPublication("document", "revision", "value", nil),
		)
		publishDone <- err
	}()
	deadline := time.Now().Add(2 * time.Second)
	for runtime.mu.TryLock() {
		runtime.mu.Unlock()
		if time.Now().After(deadline) {
			runtime.recoveryMu.Unlock()
			t.Fatal("publication did not acquire the runtime read lock")
		}
		time.Sleep(time.Millisecond)
	}

	closeDone := make(chan error, 1)
	go func() {
		closeDone <- runtime.Close()
	}()
	time.Sleep(20 * time.Millisecond)
	runtime.recoveryMu.Unlock()

	for name, done := range map[string]<-chan error{
		"recovery": recoverDone,
		"publish":  publishDone,
		"close":    closeDone,
	} {
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			t.Fatalf("%s deadlocked", name)
		}
	}
}

func TestRecoveryRejectsPendingIntentOutsideCurrentTableAllowlist(t *testing.T) {
	config := testRuntimeConfig(t, testDirectory(t))
	config.PhysicalTables = []string{"legacy"}
	fired := false
	config.testStageHook = func(stage recoveryStage) error {
		if stage == recoveryStageIntent && !fired {
			fired = true
			return context.Canceled
		}
		return nil
	}
	runtime, err := Open(config)
	if err != nil {
		t.Fatal(err)
	}
	intent := testIntent(
		t,
		config.Domain,
		"legacy-table",
		"token",
		"value",
		guard.ModeAbsentOrIdentical,
		0,
		coordination.Digest{},
	)
	intent.Cells[0].Table = "legacy"
	if _, err := runtime.Publish(
		context.Background(),
		Request{Intent: intent},
	); !errors.Is(err, ErrIndeterminatePublication) {
		t.Fatalf("stage pending legacy intent = %v", err)
	}
	if err := runtime.Close(); err != nil {
		t.Fatal(err)
	}

	config.PhysicalTables = []string{"records"}
	config.testStageHook = nil
	if reopened, err := Open(config); !errors.Is(err, transaction.ErrInvalid) {
		if reopened != nil {
			_ = reopened.Close()
		}
		t.Fatalf("recover intent outside current allowlist = %v", err)
	}
}

func TestCommittedProofPreservesOperationalReadErrors(t *testing.T) {
	config := testRuntimeConfig(t, testDirectory(t))
	runtime, err := Open(config)
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Close()
	intent := testIntent(
		t,
		config.Domain,
		"committed-proof",
		"token",
		"value",
		guard.ModeAbsentOrIdentical,
		0,
		coordination.Digest{},
	)
	result, err := runtime.Publish(
		context.Background(),
		Request{Intent: intent},
	)
	if err != nil {
		t.Fatal(err)
	}

	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := runtime.proofForEpoch(
		canceled,
		result.Epoch,
		make(map[coordination.Epoch]publicationProof),
	); !errors.Is(err, context.Canceled) ||
		errors.Is(err, transaction.ErrInternal) {
		t.Fatalf("canceled outcome read = %v", err)
	}

	runtime.intents.store = &readFailIntentStore{
		EngineStore: runtime.store,
		err:         context.Canceled,
	}
	if _, err := runtime.proofForEpoch(
		context.Background(),
		result.Epoch,
		make(map[coordination.Epoch]publicationProof),
	); !errors.Is(err, context.Canceled) ||
		errors.Is(err, transaction.ErrInternal) {
		t.Fatalf("canceled committed intent read = %v", err)
	}
}

func TestCommittedRequiresImmutableEpochOutcome(t *testing.T) {
	config := testRuntimeConfig(t, testDirectory(t))
	runtime, err := Open(config)
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Close()
	intent := testIntent(
		t,
		config.Domain,
		"committed-outcome",
		"token",
		"value",
		guard.ModeAbsentOrIdentical,
		0,
		coordination.Digest{},
	)
	result, err := runtime.Publish(
		context.Background(),
		Request{Intent: intent},
	)
	if err != nil {
		t.Fatal(err)
	}
	if _, committed, err := runtime.Committed(
		context.Background(),
		result.TXN,
		result.LogicalDigest,
	); err != nil || !committed {
		t.Fatalf("initial committed proof = %v, %v", committed, err)
	}

	row, err := coordination.OutcomeRow(config.Domain, result.Epoch)
	if err != nil {
		t.Fatal(err)
	}
	coordinate := allocator.Coordinate{
		Row:       row,
		Family:    []byte("o"),
		Qualifier: []byte("terminal"),
	}
	cells, err := runtime.store.ReadExact(
		context.Background(),
		[]allocator.Coordinate{coordinate},
	)
	if err != nil || len(cells) != 1 {
		t.Fatalf("read outcome = %#v, %v", cells, err)
	}
	status, err := runtime.store.CompareAndMutate(
		context.Background(),
		allocator.Mutation{
			Row: row,
			Conditions: []allocator.Condition{{
				Coordinate:   coordinate,
				Value:        cells[0].Value,
				Timestamp:    cells[0].Timestamp,
				TimestampSet: true,
			}},
			Updates: []allocator.Update{{
				Coordinate: coordinate,
				Delete:     true,
				Timestamp:  cells[0].Timestamp + 1,
			}},
		},
	)
	if err != nil || status != allocator.StatusAccepted {
		t.Fatalf("delete immutable outcome = %v, %v", status, err)
	}
	if _, committed, err := runtime.Committed(
		context.Background(),
		result.TXN,
		result.LogicalDigest,
	); committed || !errors.Is(err, transaction.ErrInternal) {
		t.Fatalf("committed proof without outcome = %v, %v", committed, err)
	}
}

func TestRecordCommittedUsesFullPublicationProof(t *testing.T) {
	config := testRuntimeConfig(t, testDirectory(t))
	runtime, err := Open(config)
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Close()
	request := testRecordPublication("document", "revision", "value", nil)
	result, err := runtime.PublishRecord(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	if committed, err := runtime.RecordCommitted(
		context.Background(),
		request,
	); err != nil || !committed {
		t.Fatalf("initial record committed proof = %v, %v", committed, err)
	}
	if err := deleteEpochOutcome(
		context.Background(),
		runtime,
		config.Domain,
		result.Epoch,
	); err != nil {
		t.Fatal(err)
	}
	if committed, err := runtime.RecordCommitted(
		context.Background(),
		request,
	); committed || !errors.Is(err, transaction.ErrInternal) {
		t.Fatalf("record proof without outcome = %v, %v", committed, err)
	}
}

func TestReadCommittedCellSupportsMaximumOpaqueRow(t *testing.T) {
	config := testRuntimeConfig(t, testDirectory(t))
	runtime, err := Open(config)
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Close()
	row := bytes.Repeat([]byte{0xff}, coordination.MaxCoordinateBytes)
	intent := testIntent(
		t,
		config.Domain,
		"maximum-row",
		"token",
		"value",
		guard.ModeAbsentOrIdentical,
		0,
		coordination.Digest{},
	)
	intent.Cells[0].Row = row
	intent.Cells[0].Family = []byte("event")
	intent.Cells[0].Qualifier = []byte("record")
	result, err := runtime.Publish(
		context.Background(),
		Request{Intent: intent},
	)
	if err != nil {
		t.Fatal(err)
	}
	cell, found, err := runtime.ReadCommittedCell(
		context.Background(),
		"records",
		row,
		[]byte("event"),
		[]byte("record"),
		nil,
		result.Epoch,
	)
	if err != nil || !found ||
		!bytes.Equal(cell.Cell.Coordinate.Row, row) ||
		!bytes.Equal(cell.Cell.Value, []byte("value")) {
		t.Fatalf("maximum-row committed cell = %#v, %v, %v", cell, found, err)
	}
}

func TestCommittedScanRejectsUnauthenticatedCommittedEpochTombstone(t *testing.T) {
	config := testRuntimeConfig(t, testDirectory(t))
	runtime, err := Open(config)
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Close()
	targetRow := []byte("event/target")
	first := committedReadIntent(
		t,
		config.Domain,
		"target",
		targetRow,
		[]byte("target"),
		guard.ModeAbsentOrIdentical,
		0,
		coordination.Digest{},
	)
	if _, err := runtime.Publish(
		context.Background(),
		Request{Intent: first},
	); err != nil {
		t.Fatal(err)
	}
	second := committedReadIntent(
		t,
		config.Domain,
		"other",
		[]byte("event/other"),
		[]byte("other"),
		guard.ModeAbsentOrIdentical,
		0,
		coordination.Digest{},
	)
	secondResult, err := runtime.Publish(
		context.Background(),
		Request{Intent: second},
	)
	if err != nil {
		t.Fatal(err)
	}
	tombstone, err := cclient.NewMutation(targetRow)
	if err != nil {
		t.Fatal(err)
	}
	tombstone.Delete(
		[]byte("event"),
		[]byte("record"),
		nil,
		int64(secondResult.Epoch),
	)
	if err := runtime.engine.Write(
		"records",
		[]*cclient.Mutation{tombstone},
	); err != nil {
		t.Fatal(err)
	}
	if _, err := runtime.ScanCommitted(
		context.Background(),
		CommittedScanRequest{
			Table:      "records",
			RowPrefix:  []byte("event/target"),
			Family:     []byte("event"),
			Qualifier:  []byte("record"),
			Frontier:   secondResult.Epoch,
			Limit:      1,
			MaxScanned: 16,
		},
	); !errors.Is(err, transaction.ErrInternal) {
		t.Fatalf("unauthenticated committed-epoch tombstone = %v", err)
	}
}

func TestRecordPreflightFailureAfterPersistenceIsIndeterminate(t *testing.T) {
	config := testRuntimeConfig(t, testDirectory(t))
	runtime, err := Open(config)
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Close()
	ctx, cancel := context.WithCancel(context.Background())
	faults := &cancelAfterAttemptStore{
		EngineStore: runtime.store,
		cancel:      cancel,
	}
	runtime.intents.store = faults
	request := testRecordPublication("document", "revision", "value", nil)
	if _, err := runtime.PublishRecord(
		ctx,
		request,
	); !errors.Is(err, context.Canceled) ||
		!explorer.IsIndeterminateCommit(err) {
		t.Fatalf("post-persistence canceled preflight = %v", err)
	}

	runtime.intents.store = runtime.store
	if pending, err := runtime.PendingPublications(
		context.Background(),
	); err != nil || !pending {
		t.Fatalf("pending after canceled preflight = %v, %v", pending, err)
	}
	if err := runtime.Recover(context.Background()); err != nil {
		t.Fatal(err)
	}
	if committed, err := runtime.RecordCommitted(
		context.Background(),
		request,
	); err != nil || !committed {
		t.Fatalf("recovered canceled preflight = %v, %v", committed, err)
	}
}

func TestExplorerLegacyMigrationDoesNotGrandfatherTransactionalResidue(t *testing.T) {
	directory := testDirectory(t)
	legacy, err := explorer.Open(directory)
	if err != nil {
		t.Fatal(err)
	}
	legacySource := explorer.Source{
		URI:       "file:///legacy.md",
		Title:     "Legacy",
		MediaType: explorer.MediaTypeMarkdown,
		Content:   "# Legacy\n\nGrandfathered.\n",
	}
	legacyResult, err := legacy.Ingest(context.Background(), legacySource)
	if err != nil {
		_ = legacy.Close()
		t.Fatal(err)
	}
	if err := legacy.Close(); err != nil {
		t.Fatal(err)
	}

	config := testRuntimeConfig(t, directory)
	embedded, err := OpenExplorer(config, explorer.Options{})
	if err != nil {
		t.Fatal(err)
	}
	documents, err := embedded.Explorer.Documents(context.Background())
	if err != nil || len(documents) != 1 ||
		documents[0].Document.ID != legacyResult.Document.ID {
		_ = embedded.Close()
		t.Fatalf("migrated legacy documents = %#v, %v", documents, err)
	}
	transactionalSource := explorer.Source{
		URI:       "file:///transactional.md",
		Title:     "Transactional",
		MediaType: explorer.MediaTypeMarkdown,
		Content:   "# Transactional\n\nMust remain fenced.\n",
	}
	transactional, err := embedded.Explorer.Ingest(
		context.Background(),
		transactionalSource,
	)
	if err != nil {
		_ = embedded.Close()
		t.Fatal(err)
	}
	row := []byte(
		"document/" +
			string(transactional.Document.ID) +
			"/" +
			string(transactional.Revision.ID),
	)
	recordKey := documentTestRecordKey(row)
	coordinate, err := embedded.Runtime.intents.attemptCoordinate(recordKey)
	if err != nil {
		_ = embedded.Close()
		t.Fatal(err)
	}
	cells, err := embedded.Runtime.store.ReadExact(
		context.Background(),
		[]allocator.Coordinate{coordinate},
	)
	if err != nil || len(cells) != 1 {
		_ = embedded.Close()
		t.Fatalf("read transactional binding = %#v, %v", cells, err)
	}
	status, err := embedded.Runtime.store.CompareAndMutate(
		context.Background(),
		allocator.Mutation{
			Row: coordinate.Row,
			Conditions: []allocator.Condition{{
				Coordinate:   coordinate,
				Value:        cells[0].Value,
				Timestamp:    cells[0].Timestamp,
				TimestampSet: true,
			}},
			Updates: []allocator.Update{{
				Coordinate: coordinate,
				Delete:     true,
				Timestamp:  cells[0].Timestamp + 1,
			}},
		},
	)
	if err != nil || status != allocator.StatusAccepted {
		_ = embedded.Close()
		t.Fatalf("delete transactional binding = %v, %v", status, err)
	}
	if err := embedded.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := OpenExplorer(config, explorer.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	documents, err = reopened.Explorer.Documents(context.Background())
	if err != nil || len(documents) != 1 ||
		documents[0].Document.ID != legacyResult.Document.ID {
		t.Fatalf("documents after binding loss = %#v, %v", documents, err)
	}
}

func TestExplorerLegacyMigrationMarksV1RecordsExactly(t *testing.T) {
	config := testRuntimeConfig(t, testDirectory(t))
	config.PhysicalTables = append(config.PhysicalTables, explorer.EmbeddedTableName)
	runtime, err := Open(config)
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Close()
	legacyRow := []byte("document/legacy/revision")
	legacyValue := []byte(`{"legacy":true}`)
	legacyMutation, err := cclient.NewMutation(legacyRow)
	if err != nil {
		t.Fatal(err)
	}
	legacyMutation.PutLatest(
		[]byte("record"),
		[]byte("v1"),
		nil,
		legacyValue,
	)
	if err := runtime.engine.Write(
		explorer.EmbeddedTableName,
		[]*cclient.Mutation{legacyMutation},
	); err != nil {
		t.Fatal(err)
	}
	if err := runtime.enableExplorerLegacyCompatibility(
		context.Background(),
	); err != nil {
		t.Fatal(err)
	}
	allowed, err := runtime.RecordCommitted(
		context.Background(),
		explorer.RecordPublication{
			Operation: []byte("explorer-document-record-v1"),
			RecordKey: documentTestRecordKey(legacyRow),
			Table:     explorer.EmbeddedTableName,
			Row:       legacyRow,
			Family:    []byte("record"),
			Qualifier: []byte("v1"),
			Value:     legacyValue,
		},
	)
	if err != nil || !allowed {
		t.Fatalf("grandfathered v1 record = %v, %v", allowed, err)
	}

	residueRow := []byte("document/residue/revision")
	residueValue := []byte(`{"residue":true}`)
	residueMutation, err := cclient.NewMutation(residueRow)
	if err != nil {
		t.Fatal(err)
	}
	residueMutation.PutLatest(
		[]byte("record"),
		[]byte("v1"),
		nil,
		residueValue,
	)
	if err := runtime.engine.Write(
		explorer.EmbeddedTableName,
		[]*cclient.Mutation{residueMutation},
	); err != nil {
		t.Fatal(err)
	}
	allowed, err = runtime.RecordCommitted(
		context.Background(),
		explorer.RecordPublication{
			Operation: []byte("explorer-document-record-v1"),
			RecordKey: documentTestRecordKey(residueRow),
			Table:     explorer.EmbeddedTableName,
			Row:       residueRow,
			Family:    []byte("record"),
			Qualifier: []byte("v1"),
			Value:     residueValue,
		},
	)
	if err != nil || allowed {
		t.Fatalf("post-migration v1 residue = %v, %v", allowed, err)
	}
}

func TestGenericPublishSerializesPendingRegistrationWithRecovery(t *testing.T) {
	config := testRuntimeConfig(t, testDirectory(t))
	fired := false
	config.testStageHook = func(stage recoveryStage) error {
		if stage == recoveryStageIntent && !fired {
			fired = true
			return context.Canceled
		}
		return nil
	}
	runtime, err := Open(config)
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Close()
	blocking := &blockingIntentStore{
		EngineStore: runtime.store,
		entered:     make(chan struct{}),
		release:     make(chan struct{}),
	}
	runtime.intents.store = blocking
	intent := testIntent(
		t,
		config.Domain,
		"generic-recovery-race",
		"token",
		"value",
		guard.ModeAbsentOrIdentical,
		0,
		coordination.Digest{},
	)
	publishDone := make(chan error, 1)
	go func() {
		_, err := runtime.Publish(
			context.Background(),
			Request{Intent: intent},
		)
		publishDone <- err
	}()
	select {
	case <-blocking.entered:
	case <-time.After(3 * time.Second):
		t.Fatal("generic publication did not reach intent persistence")
	}

	recoverDone := make(chan error, 1)
	go func() {
		recoverDone <- runtime.Recover(context.Background())
	}()
	var earlyRecovery error
	recoveredEarly := false
	select {
	case earlyRecovery = <-recoverDone:
		recoveredEarly = true
	case <-time.After(100 * time.Millisecond):
	}
	close(blocking.release)
	if err := <-publishDone; !errors.Is(err, ErrIndeterminatePublication) {
		t.Fatalf("staged generic publication = %v", err)
	}
	if recoveredEarly {
		t.Fatalf("recovery passed index-before-intent window: %v", earlyRecovery)
	}
	if err := <-recoverDone; err != nil {
		t.Fatalf("recover serialized generic publication = %v", err)
	}
	digest, err := LogicalDigest(intent)
	if err != nil {
		t.Fatal(err)
	}
	txn, err := DeriveTXN(config.Domain, intent.Operation, intent.Token)
	if err != nil {
		t.Fatal(err)
	}
	if _, committed, err := runtime.Committed(
		context.Background(),
		txn,
		digest,
	); err != nil || !committed {
		t.Fatalf("serialized recovered publication = %v, %v", committed, err)
	}
}

func TestSuccessfulRecoveryRefreshesPendingCache(t *testing.T) {
	config := testRuntimeConfig(t, testDirectory(t))
	runtime, err := Open(config)
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Close()
	txn, err := DeriveTXN(config.Domain, []byte("stale-cache"), []byte("token"))
	if err != nil {
		t.Fatal(err)
	}
	runtime.intents.markPending(txn)
	if pending, err := runtime.PendingPublications(
		context.Background(),
	); err != nil || !pending {
		t.Fatalf("seed stale pending cache = %v, %v", pending, err)
	}
	if err := runtime.Recover(context.Background()); err != nil {
		t.Fatal(err)
	}
	if pending, err := runtime.PendingPublications(
		context.Background(),
	); err != nil || pending {
		t.Fatalf("refreshed pending cache = %v, %v", pending, err)
	}
}

func TestCompletedIntentConflictDoesNotReopenPendingIndex(t *testing.T) {
	config := testRuntimeConfig(t, testDirectory(t))
	runtime, err := Open(config)
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Close()
	intent := testIntent(
		t,
		config.Domain,
		"completed-conflict",
		"token",
		"original",
		guard.ModeAbsentOrIdentical,
		0,
		coordination.Digest{},
	)
	if _, err := runtime.Publish(
		context.Background(),
		Request{Intent: intent},
	); err != nil {
		t.Fatal(err)
	}
	divergent := cloneIntent(intent)
	divergent.Cells[0].Value = []byte("divergent")
	if _, err := runtime.Publish(
		context.Background(),
		Request{Intent: divergent},
	); !errors.Is(err, transaction.ErrConflict) ||
		errors.Is(err, ErrIndeterminatePublication) {
		t.Fatalf("divergent completed replay = %v", err)
	}
	if pending, err := runtime.PendingPublications(
		context.Background(),
	); err != nil || pending {
		t.Fatalf("pending after completed replay conflict = %v, %v", pending, err)
	}
}

func TestCommittedScanDefaultWorkBoundSupportsMaximumLimit(t *testing.T) {
	config := testRuntimeConfig(t, testDirectory(t))
	runtime, err := Open(config)
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Close()
	page, err := runtime.ScanCommitted(
		context.Background(),
		CommittedScanRequest{
			Table:     "records",
			RowPrefix: []byte("missing/"),
			Family:    []byte("record"),
			Qualifier: []byte("v1"),
			Limit:     MaxCommittedScanLimit,
		},
	)
	if err != nil || len(page.Cells) != 0 {
		t.Fatalf("maximum default committed scan = %#v, %v", page, err)
	}
}

func TestCommittedScanRejectsOversizedCursors(t *testing.T) {
	config := testRuntimeConfig(t, testDirectory(t))
	runtime, err := Open(config)
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Close()
	oversized := bytes.Repeat([]byte{'x'}, coordination.MaxCoordinateBytes+1)
	for name, applyCursor := range map[string]func(*CommittedScanRequest){
		"start row": func(request *CommittedScanRequest) {
			request.StartRow = oversized
		},
		"exclusive start row": func(request *CommittedScanRequest) {
			request.StartAfterRow = oversized
		},
	} {
		t.Run(name, func(t *testing.T) {
			request := CommittedScanRequest{
				Table:     "records",
				RowPrefix: []byte("x"),
				Family:    []byte("record"),
				Qualifier: []byte("v1"),
				Limit:     1,
			}
			applyCursor(&request)
			if _, err := runtime.ScanCommitted(
				context.Background(),
				request,
			); !errors.Is(err, transaction.ErrInvalid) {
				t.Fatalf("oversized committed scan cursor = %v", err)
			}
		})
	}
}

func TestCommittedScanRejectsNegativeFrontier(t *testing.T) {
	config := testRuntimeConfig(t, testDirectory(t))
	runtime, err := Open(config)
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Close()
	if _, err := runtime.ScanCommitted(
		context.Background(),
		CommittedScanRequest{
			Table:     "records",
			RowPrefix: []byte("row/"),
			Family:    []byte("record"),
			Qualifier: []byte("v1"),
			Frontier:  coordination.Epoch(-1),
			Limit:     1,
		},
	); !errors.Is(err, transaction.ErrInvalid) {
		t.Fatalf("negative committed frontier = %v", err)
	}
}

func TestExactCommittedReadIgnoresSiblingCoordinateWork(t *testing.T) {
	config := testRuntimeConfig(t, testDirectory(t))
	runtime, err := Open(config)
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Close()
	row := []byte("shared-row")
	intent := committedReadIntent(
		t,
		config.Domain,
		"exact-coordinate",
		row,
		[]byte("target"),
		guard.ModeAbsentOrIdentical,
		0,
		coordination.Digest{},
	)
	result, err := runtime.Publish(
		context.Background(),
		Request{Intent: intent},
	)
	if err != nil {
		t.Fatal(err)
	}
	siblings, err := cclient.NewMutation(row)
	if err != nil {
		t.Fatal(err)
	}
	siblings.Put(
		[]byte("event"),
		[]byte("a"),
		nil,
		int64(result.Epoch),
		[]byte("a"),
	)
	siblings.Put(
		[]byte("event"),
		[]byte("b"),
		nil,
		int64(result.Epoch),
		[]byte("b"),
	)
	if err := runtime.engine.Write(
		"records",
		[]*cclient.Mutation{siblings},
	); err != nil {
		t.Fatal(err)
	}
	cell, found, err := runtime.readCommittedCell(
		context.Background(),
		"records",
		row,
		[]byte("event"),
		[]byte("record"),
		nil,
		result.Epoch,
		1,
	)
	if err != nil || !found || !bytes.Equal(cell.Cell.Value, []byte("target")) {
		t.Fatalf("exact read with sibling cells = %#v, %v, %v", cell, found, err)
	}
}

func TestRecoveryRestoresAmbiguousRecordAttemptBinding(t *testing.T) {
	for _, applied := range []bool{false, true} {
		t.Run(map[bool]string{false: "not applied", true: "applied"}[applied], func(t *testing.T) {
			config := testRuntimeConfig(t, testDirectory(t))
			runtime, err := Open(config)
			if err != nil {
				t.Fatal(err)
			}
			defer runtime.Close()
			faults := &ambiguousAttemptStore{
				EngineStore: runtime.store,
				apply:       applied,
			}
			runtime.intents.store = faults
			request := testRecordPublication(
				"document",
				"revision",
				"value",
				nil,
			)
			if _, err := runtime.PublishRecord(
				context.Background(),
				request,
			); !errors.Is(err, ErrIndeterminatePublication) ||
				!errors.Is(err, allocator.ErrConditionalUnknown) {
				t.Fatalf("ambiguous record binding = %v", err)
			}

			runtime.intents.store = runtime.store
			if err := runtime.Recover(context.Background()); err != nil {
				t.Fatalf("recover ambiguous record binding = %v", err)
			}
			attempt, err := runtime.RecordAttempt(context.Background(), request)
			if err != nil || attempt == nil ||
				!bytes.Equal(attempt.Value, request.Value) ||
				attempt.ExpectedEpoch != request.ExpectedEpoch ||
				attempt.ExpectedDigest != request.ExpectedDigest {
				t.Fatalf("recovered record attempt = %#v, %v", attempt, err)
			}
			head, err := runtime.RecordHead(
				context.Background(),
				request.EntityKind,
				request.EntityID,
			)
			if err != nil || head == nil {
				t.Fatalf("recovered record head = %#v, %v", head, err)
			}
			retry, err := runtime.PublishRecord(context.Background(), request)
			if err != nil || !retry.Unchanged || retry.Epoch != head.Epoch {
				t.Fatalf("exact retry after binding recovery = %#v, %v", retry, err)
			}
			after, err := runtime.CurrentHead(context.Background())
			if err != nil || after.Frontier != head.Epoch {
				t.Fatalf("retry duplicated recovered publication = %#v, %v", after, err)
			}
		})
	}
}

func TestRecordAttemptPreservesEmptyValue(t *testing.T) {
	config := testRuntimeConfig(t, testDirectory(t))
	runtime, err := Open(config)
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Close()
	request := testRecordPublication("document", "revision", "value", nil)
	request.Value = nil
	if _, err := runtime.PublishRecord(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	attempt, err := runtime.RecordAttempt(context.Background(), request)
	if err != nil || attempt == nil || len(attempt.Value) != 0 {
		t.Fatalf("empty record attempt = %#v, %v", attempt, err)
	}
}

func TestRecoverySerializesHighConcurrencyPendingIndexUpdates(t *testing.T) {
	config := testRuntimeConfig(t, testDirectory(t))
	config.RecoveryLimit = 64
	config.RecoveryConcurrency = 32
	config.testStageHook = func(stage recoveryStage) error {
		if stage == recoveryStageIntent {
			return context.Canceled
		}
		return nil
	}
	runtime, err := Open(config)
	if err != nil {
		t.Fatal(err)
	}
	const publications = 32
	for index := 0; index < publications; index++ {
		row := []byte(fmt.Sprintf("recovery-index/%02d", index))
		intent := committedReadIntent(
			t,
			config.Domain,
			fmt.Sprintf("recovery-index-%02d", index),
			row,
			[]byte("value"),
			guard.ModeAbsentOrIdentical,
			0,
			coordination.Digest{},
		)
		if _, err := runtime.Publish(
			context.Background(),
			Request{Intent: intent},
		); !errors.Is(err, ErrIndeterminatePublication) {
			t.Fatalf("stage pending publication %d = %v", index, err)
		}
	}
	if err := runtime.Close(); err != nil {
		t.Fatal(err)
	}

	config.testStageHook = nil
	reopened, err := Open(config)
	if err != nil {
		t.Fatalf("high-concurrency recovery open = %v", err)
	}
	defer reopened.Close()
	if pending, err := reopened.PendingPublications(
		context.Background(),
	); err != nil || pending {
		t.Fatalf("pending after high-concurrency recovery = %v, %v", pending, err)
	}
}

func TestPendingIndexSupportsMaximumRecoveryConcurrency(t *testing.T) {
	config := testRuntimeConfig(t, testDirectory(t))
	runtime, err := Open(config)
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Close()
	const workers = 64
	txns := make([]coordination.TXN, 0, workers)
	for index := 0; index < workers; index++ {
		txn, err := DeriveTXN(
			config.Domain,
			[]byte("index-concurrency"),
			[]byte(fmt.Sprintf("token-%03d", index)),
		)
		if err != nil {
			t.Fatal(err)
		}
		if err := runtime.intents.addPending(
			context.Background(),
			txn,
		); err != nil {
			t.Fatal(err)
		}
		txns = append(txns, txn)
	}
	start := make(chan struct{})
	errs := make(chan error, workers)
	var wait sync.WaitGroup
	for _, txn := range txns {
		txn := append(coordination.TXN(nil), txn...)
		wait.Add(1)
		go func() {
			defer wait.Done()
			<-start
			errs <- runtime.intents.removePending(context.Background(), txn)
		}()
	}
	close(start)
	wait.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent pending index removal = %v", err)
		}
	}
	if err := runtime.intents.IndexPending(context.Background(), 1, 1); err != nil {
		t.Fatal(err)
	}
	if runtime.intents.HasPending() {
		t.Fatal("pending index retained entries after concurrent removal")
	}
}

func TestTerminalRecordAttemptCanAdvanceToNewRetryGeneration(t *testing.T) {
	config := testRuntimeConfig(t, testDirectory(t))
	fired := false
	config.testStageHook = func(stage recoveryStage) error {
		if stage == recoveryStagePhysical && !fired {
			fired = true
			return context.Canceled
		}
		return nil
	}
	runtime, err := Open(config)
	if err != nil {
		t.Fatal(err)
	}
	first := testRecordPublication("document", "revision", "first-envelope", nil)
	if _, err := runtime.PublishRecord(
		context.Background(),
		first,
	); !errors.Is(err, ErrIndeterminatePublication) {
		t.Fatalf("stage first record attempt = %v", err)
	}
	firstTXN, found, err := runtime.intents.Attempt(
		context.Background(),
		first.RecordKey,
	)
	if err != nil || !found {
		t.Fatalf("first record attempt binding = %x, %v, %v", firstTXN, found, err)
	}
	snapshot, err := runtime.Inspect(context.Background(), firstTXN)
	if err != nil || snapshot.Root.Epoch == 0 {
		t.Fatalf("first staged transaction = %#v, %v", snapshot, err)
	}
	corrupt, err := cclient.NewMutation(first.Row)
	if err != nil {
		t.Fatal(err)
	}
	corrupt.Put(
		first.Family,
		first.Qualifier,
		first.Visibility,
		int64(snapshot.Root.Epoch),
		[]byte("corrupt"),
	)
	if err := runtime.engine.Write(
		first.Table,
		[]*cclient.Mutation{corrupt},
	); err != nil {
		t.Fatal(err)
	}
	if err := runtime.Recover(context.Background()); err != nil {
		t.Fatalf("poison first record attempt = %v", err)
	}
	snapshot, err = runtime.Inspect(context.Background(), firstTXN)
	if err != nil || snapshot.Root.State != coordination.StatePoisoned {
		t.Fatalf("terminal first record attempt = %#v, %v", snapshot, err)
	}
	if attempt, err := runtime.RecordAttempt(
		context.Background(),
		first,
	); err != nil || attempt != nil {
		t.Fatalf("terminal record attempt remained reusable = %#v, %v", attempt, err)
	}
	if err := runtime.Close(); err != nil {
		t.Fatal(err)
	}

	config.testStageHook = nil
	reopened, err := Open(config)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	retry := first
	retry.Value = []byte("second-envelope")
	result, err := reopened.PublishRecord(context.Background(), retry)
	if err != nil || result.Epoch <= snapshot.Root.Epoch {
		t.Fatalf("retry after terminal attempt = %#v, %v", result, err)
	}
	retryTXN, found, err := reopened.intents.Attempt(
		context.Background(),
		retry.RecordKey,
	)
	if err != nil || !found || bytes.Equal(retryTXN, firstTXN) {
		t.Fatalf("retry attempt generation = %x, %v, %v", retryTXN, found, err)
	}
	replayed, err := reopened.PublishRecord(context.Background(), retry)
	if err != nil || !replayed.Unchanged || replayed.Epoch != result.Epoch {
		t.Fatalf("terminal retry replay = %#v, %v", replayed, err)
	}
}

type ambiguousIntentStore struct {
	*EngineStore
	ambiguous    bool
	failReadback bool
}

func (s *ambiguousIntentStore) CompareAndMutate(
	ctx context.Context,
	mutation allocator.Mutation,
) (allocator.Status, error) {
	status, err := s.EngineStore.CompareAndMutate(ctx, mutation)
	if !s.ambiguous || !bytes.HasPrefix(mutation.Row, intentRowMagic) {
		return status, err
	}
	s.ambiguous = false
	if err != nil || status != allocator.StatusAccepted {
		return status, err
	}
	s.failReadback = true
	return allocator.StatusUnknown, allocator.ErrConditionalUnknown
}

func (s *ambiguousIntentStore) ReadExact(
	ctx context.Context,
	coordinates []allocator.Coordinate,
) ([]allocator.Cell, error) {
	if s.failReadback {
		s.failReadback = false
		return nil, errors.New("injected intent readback failure")
	}
	return s.EngineStore.ReadExact(ctx, coordinates)
}

type scanFailIntentStore struct {
	*EngineStore
}

func (s *scanFailIntentStore) ScanPrefixFrom(
	context.Context,
	[]byte,
	[]byte,
	[]byte,
	[]byte,
	[]byte,
	int,
) ([]allocator.Cell, error) {
	return nil, errors.New("unexpected historical pending scan")
}

type readFailIntentStore struct {
	*EngineStore
	err error
}

func (s *readFailIntentStore) ReadExact(
	context.Context,
	[]allocator.Coordinate,
) ([]allocator.Cell, error) {
	return nil, s.err
}

type blockingIntentStore struct {
	*EngineStore
	entered chan struct{}
	release chan struct{}
	blocked bool
}

func (s *blockingIntentStore) CompareAndMutate(
	ctx context.Context,
	mutation allocator.Mutation,
) (allocator.Status, error) {
	if !s.blocked && bytes.HasPrefix(mutation.Row, intentRowMagic) {
		s.blocked = true
		close(s.entered)
		select {
		case <-s.release:
		case <-ctx.Done():
			return allocator.StatusUnknown, ctx.Err()
		}
	}
	return s.EngineStore.CompareAndMutate(ctx, mutation)
}

type ambiguousAttemptStore struct {
	*EngineStore
	apply        bool
	ambiguous    bool
	failReadback bool
}

func (s *ambiguousAttemptStore) CompareAndMutate(
	ctx context.Context,
	mutation allocator.Mutation,
) (allocator.Status, error) {
	if s.ambiguous || !bytes.HasPrefix(mutation.Row, attemptRowMagic) {
		return s.EngineStore.CompareAndMutate(ctx, mutation)
	}
	s.ambiguous = true
	s.failReadback = true
	if s.apply {
		status, err := s.EngineStore.CompareAndMutate(ctx, mutation)
		if err != nil || status != allocator.StatusAccepted {
			return status, err
		}
	}
	return allocator.StatusUnknown, allocator.ErrConditionalUnknown
}

func (s *ambiguousAttemptStore) ReadExact(
	ctx context.Context,
	coordinates []allocator.Coordinate,
) ([]allocator.Cell, error) {
	if s.failReadback && len(coordinates) == 1 &&
		bytes.HasPrefix(coordinates[0].Row, attemptRowMagic) {
		s.failReadback = false
		return nil, errors.New("injected attempt binding readback failure")
	}
	return s.EngineStore.ReadExact(ctx, coordinates)
}

type cancelAfterAttemptStore struct {
	*EngineStore
	cancel   context.CancelFunc
	canceled bool
}

func (s *cancelAfterAttemptStore) CompareAndMutate(
	ctx context.Context,
	mutation allocator.Mutation,
) (allocator.Status, error) {
	status, err := s.EngineStore.CompareAndMutate(ctx, mutation)
	if !s.canceled &&
		err == nil &&
		status == allocator.StatusAccepted &&
		bytes.HasPrefix(mutation.Row, attemptRowMagic) {
		s.canceled = true
		s.cancel()
	}
	return status, err
}

func deleteEpochOutcome(
	ctx context.Context,
	runtime *Runtime,
	domain coordination.DomainID,
	epoch coordination.Epoch,
) error {
	row, err := coordination.OutcomeRow(domain, epoch)
	if err != nil {
		return err
	}
	coordinate := allocator.Coordinate{
		Row:       row,
		Family:    []byte("o"),
		Qualifier: []byte("terminal"),
	}
	cells, err := runtime.store.ReadExact(
		ctx,
		[]allocator.Coordinate{coordinate},
	)
	if err != nil {
		return err
	}
	if len(cells) != 1 {
		return errors.New("epoch outcome is missing")
	}
	status, err := runtime.store.CompareAndMutate(ctx, allocator.Mutation{
		Row: row,
		Conditions: []allocator.Condition{{
			Coordinate:   coordinate,
			Value:        cells[0].Value,
			Timestamp:    cells[0].Timestamp,
			TimestampSet: true,
		}},
		Updates: []allocator.Update{{
			Coordinate: coordinate,
			Delete:     true,
			Timestamp:  cells[0].Timestamp + 1,
		}},
	})
	if err != nil {
		return err
	}
	if status != allocator.StatusAccepted {
		return fmt.Errorf("delete epoch outcome status %d", status)
	}
	return nil
}
