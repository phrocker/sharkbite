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
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/phrocker/shoal-oss/internal/cclient"
	"github.com/phrocker/shoal-oss/internal/engine"
	"github.com/phrocker/shoal-oss/internal/localwal"
	"github.com/phrocker/shoal-oss/pkg/explorer"
	"github.com/phrocker/shoal-oss/pkg/explorer/coordination"
	"github.com/phrocker/shoal-oss/pkg/explorer/coordination/allocator"
	"github.com/phrocker/shoal-oss/pkg/explorer/coordination/guard"
	"github.com/phrocker/shoal-oss/pkg/explorer/coordination/transaction"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

func TestRuntimePublishesRetriesAndRejectsConcurrentDivergence(t *testing.T) {
	config := testRuntimeConfig(t, testDirectory(t))
	runtime, err := Open(config)
	if err != nil {
		t.Fatal(err)
	}

	base := testIntent(t, config.Domain, "create", "base", "v1", guard.ModeAbsentOrIdentical, 0, coordination.Digest{})
	baseResult, err := runtime.Publish(context.Background(), Request{Intent: base})
	if err != nil {
		t.Fatal(err)
	}
	baseDigest, err := LogicalDigest(base)
	if err != nil || baseResult.LogicalDigest != baseDigest || len(baseResult.TXN) == 0 {
		t.Fatalf("published identity = %#v, digest err %v", baseResult, err)
	}
	retry, err := runtime.Publish(context.Background(), Request{Intent: base})
	if err != nil || !retry.Unchanged || retry.Epoch != baseResult.Epoch {
		t.Fatalf("identical retry = %#v, %v", retry, err)
	}
	left := testIntent(t, config.Domain, "update", "left", "v2-left", guard.ModeMutate, baseResult.Epoch, baseDigest)
	right := testIntent(t, config.Domain, "update", "right", "v2-right", guard.ModeMutate, baseResult.Epoch, baseDigest)

	type outcome struct {
		name   string
		result Result
		err    error
	}
	outcomes := make(chan outcome, 2)
	var wait sync.WaitGroup
	for name, intent := range map[string]Intent{"left": left, "right": right} {
		name, intent := name, intent
		wait.Add(1)
		go func() {
			defer wait.Done()
			result, err := runtime.Publish(context.Background(), Request{Intent: intent})
			outcomes <- outcome{name: name, result: result, err: err}
		}()
	}
	wait.Wait()
	close(outcomes)
	successes := 0
	var loser Intent
	var loserErr error
	for outcome := range outcomes {
		if outcome.err == nil {
			successes++
			continue
		}
		if outcome.name == "left" {
			loser = left
		} else {
			loser = right
		}
		loserErr = outcome.err
	}
	if successes != 1 {
		t.Fatalf("concurrent successful updates = %d, want 1", successes)
	}
	if !errors.Is(loserErr, transaction.ErrConflict) ||
		errors.Is(loserErr, ErrIndeterminatePublication) {
		t.Fatalf("concurrent losing update = %v", loserErr)
	}
	if _, err := runtime.Publish(context.Background(), Request{Intent: loser}); !errors.Is(err, transaction.ErrConflict) {
		t.Fatalf("losing expected-value update = %v", err)
	}
	head, pending, err := runtime.ReadEntity(context.Background(), guard.Entity{
		Kind: 'R', ID: coordination.EntityID("record/one"),
	})
	if err != nil || head == nil || head.Epoch <= baseResult.Epoch ||
		(pending != nil && pending.Active) {
		t.Fatalf("winning entity state = head %#v pending %#v err %v", head, pending, err)
	}
	if err := runtime.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(config)
	if err != nil {
		t.Fatalf("reopen after guard conflict = %v", err)
	}
	defer reopened.Close()
}

func TestIntentStoreCanonicalReplayPagingAndDigestMutation(t *testing.T) {
	eng, store := openTestEngineStore(t, "coord")
	defer eng.Close()
	domain := coordination.DomainID("domain")
	intents, err := NewIntentStore(domain, nil, store)
	if err != nil {
		t.Fatal(err)
	}
	var txns []coordination.TXN
	for index := 0; index < 24; index++ {
		intent := testIntent(
			t, domain, "create", fmt.Sprintf("token-%d", index),
			fmt.Sprintf("value-%d", index), guard.ModeAbsentOrIdentical, 0, coordination.Digest{},
		)
		record, unchanged, err := intents.Put(context.Background(), intent)
		if err != nil || unchanged {
			t.Fatalf("put %d = %#v, %v", index, record, err)
		}
		replay, unchanged, err := intents.Put(context.Background(), intent)
		if err != nil || !unchanged || !bytes.Equal(replay.TXN, record.TXN) {
			t.Fatalf("replay %d = %#v, %v, unchanged=%v", index, replay, err, unchanged)
		}
		txns = append(txns, record.TXN)
	}
	divergent := testIntent(t, domain, "create", "token-0", "different", guard.ModeAbsentOrIdentical, 0, coordination.Digest{})
	if _, _, err := intents.Put(context.Background(), divergent); !errors.Is(err, transaction.ErrConflict) {
		t.Fatalf("divergent intent replay = %v", err)
	}

	var got []coordination.TXN
	var cursor []byte
	for {
		page, next, err := intents.Candidates(context.Background(), cursor, 2)
		if err != nil {
			t.Fatal(err)
		}
		got = append(got, page...)
		if len(next) == 0 {
			break
		}
		cursor = next
	}
	if len(got) != len(txns) {
		t.Fatalf("candidate count = %d, want %d", len(got), len(txns))
	}
	for index := 1; index < len(got); index++ {
		if bytes.Compare(got[index-1], got[index]) >= 0 {
			t.Fatalf("candidates are not ordered: %x then %x", got[index-1], got[index])
		}
	}
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, _, err := intents.Candidates(canceled, nil, 2); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled candidate scan = %v", err)
	}

	record, err := intents.Load(context.Background(), txns[0])
	if err != nil {
		t.Fatal(err)
	}
	coordinate := intents.intentCoordinate(record.TXN)
	cells, err := store.ReadExact(context.Background(), []allocator.Coordinate{coordinate})
	if err != nil || len(cells) != 1 {
		t.Fatalf("read durable intent = %#v, %v", cells, err)
	}
	corrupt := append([]byte(nil), cells[0].Value...)
	corrupt[len(corrupt)-1] ^= 0xff
	status, err := store.CompareAndMutate(context.Background(), allocator.Mutation{
		Row: coordinate.Row,
		Conditions: []allocator.Condition{{
			Coordinate: coordinate, Value: cells[0].Value,
			Timestamp: cells[0].Timestamp, TimestampSet: true,
		}},
		Updates: []allocator.Update{{
			Coordinate: coordinate, Value: corrupt, Timestamp: cells[0].Timestamp + 1,
		}},
	})
	if err != nil || status != allocator.StatusAccepted {
		t.Fatalf("corrupt intent = %v, %v", status, err)
	}
	if _, err := intents.Materialize(context.Background(), transaction.MaterializeRequest{
		TXN: record.TXN, LogicalDigest: record.LogicalDigest,
	}); !errors.Is(err, transaction.ErrInternal) {
		t.Fatalf("materialize corrupt intent = %v", err)
	}
}

func TestRecordAttemptAliasGenerationSurvivesSeparateRFiles(t *testing.T) {
	directory := testDirectory(t)
	eng, err := engine.Open(directory, engine.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if err := eng.CreateTable("coord", engine.TableOptions{}); err != nil {
		t.Fatal(err)
	}
	store, _ := NewEngineStore(eng, "coord")
	intents, _ := NewIntentStore(coordination.DomainID("domain"), nil, store)
	key := []byte("document/revision")
	oldTxn := coordination.TXN("old")
	newTxn := coordination.TXN("new")
	if err := intents.SetAttempt(context.Background(), key, nil, oldTxn); err != nil {
		t.Fatal(err)
	}
	stale, found, err := intents.readAttempt(context.Background(), key)
	if err != nil || !found {
		t.Fatalf("read initial alias = %#v, %v, %v", stale, found, err)
	}
	if err := eng.Flush("coord"); err != nil {
		t.Fatal(err)
	}
	if err := intents.SetAttempt(context.Background(), key, oldTxn, newTxn); err != nil {
		t.Fatal(err)
	}
	if err := intents.setAttempt(
		context.Background(), key, stale, coordination.TXN("stale"),
	); !errors.Is(err, transaction.ErrConflict) {
		t.Fatalf("stale alias generation = %v", err)
	}
	if err := eng.Flush("coord"); err != nil {
		t.Fatal(err)
	}
	if err := eng.Compact("coord", nil); err != nil {
		t.Fatal(err)
	}
	if err := eng.Close(); err != nil {
		t.Fatal(err)
	}
	eng, err = engine.Open(directory, engine.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer eng.Close()
	store, _ = NewEngineStore(eng, "coord")
	intents, _ = NewIntentStore(coordination.DomainID("domain"), nil, store)
	coordinate, _ := intents.attemptCoordinate(key)
	cells, readErr := store.ReadExact(context.Background(), []allocator.Coordinate{coordinate})
	if readErr != nil {
		t.Fatal(readErr)
	}
	got, found, err := intents.Attempt(context.Background(), key)
	if err != nil || !found || !bytes.Equal(got, newTxn) {
		t.Fatalf("reopened attempt alias = %q, %v, %v; cells=%#v", got, found, err, cells)
	}
}

func TestRecordAttemptAliasConcurrentRebindHasOneWinner(t *testing.T) {
	eng, store := openTestEngineStore(t, "coord")
	defer eng.Close()
	intents, err := NewIntentStore(coordination.DomainID("domain"), nil, store)
	if err != nil {
		t.Fatal(err)
	}
	key := []byte("document/revision")
	oldTxn := coordination.TXN("old")
	if err := intents.SetAttempt(context.Background(), key, nil, oldTxn); err != nil {
		t.Fatal(err)
	}
	candidates := []coordination.TXN{
		coordination.TXN("new-a"),
		coordination.TXN("new-b"),
	}
	errs := make(chan error, len(candidates))
	var wait sync.WaitGroup
	for _, candidate := range candidates {
		candidate := candidate
		wait.Add(1)
		go func() {
			defer wait.Done()
			errs <- intents.SetAttempt(
				context.Background(), key, oldTxn, candidate,
			)
		}()
	}
	wait.Wait()
	close(errs)
	successes, conflicts := 0, 0
	for err := range errs {
		switch {
		case err == nil:
			successes++
		case errors.Is(err, transaction.ErrConflict):
			conflicts++
		default:
			t.Fatalf("concurrent alias rebind = %v", err)
		}
	}
	if successes != 1 || conflicts != 1 {
		t.Fatalf("alias rebind successes=%d conflicts=%d", successes, conflicts)
	}
	got, found, err := intents.Attempt(context.Background(), key)
	if err != nil || !found ||
		(!bytes.Equal(got, candidates[0]) && !bytes.Equal(got, candidates[1])) {
		t.Fatalf("alias rebind winner = %q, %v, %v", got, found, err)
	}
}

func TestRuntimeCrashRecoveryAtEveryDurableStage(t *testing.T) {
	stages := []recoveryStage{
		recoveryStageIntent,
		recoveryStagePhysical,
		recoveryStagePrepared,
		recoveryStageCommitted,
		recoveryStageCheckpoint,
		recoveryStageComplete,
	}
	for _, stage := range stages {
		t.Run(fmt.Sprintf("stage-%d", stage), func(t *testing.T) {
			directory := testDirectory(t)
			config := testRuntimeConfig(t, directory)
			var mu sync.Mutex
			fired := false
			config.testStageHook = func(got recoveryStage) error {
				mu.Lock()
				defer mu.Unlock()
				if got == stage && !fired {
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
				t, config.Domain, "create", "recover", "durable",
				guard.ModeAbsentOrIdentical, 0, coordination.Digest{},
			)
			if _, err := runtime.Publish(context.Background(), Request{Intent: intent}); err == nil ||
				!errors.Is(err, ErrIndeterminatePublication) {
				t.Fatalf("publish at stage %d = %v", stage, err)
			}
			mu.Lock()
			wasFired := fired
			mu.Unlock()
			if !wasFired {
				t.Fatalf("stage %d was not reached", stage)
			}
			if err := runtime.Close(); err != nil {
				t.Fatal(err)
			}

			config.testStageHook = nil
			reopened, err := Open(config)
			if err != nil {
				t.Fatalf("reopen recovery: %v", err)
			}
			defer reopened.Close()
			txn, _ := DeriveTXN(config.Domain, intent.Operation, intent.Token)
			record, err := reopened.intents.Load(context.Background(), txn)
			if err != nil {
				t.Fatal(err)
			}
			epoch, complete, err := reopened.intents.Completed(context.Background(), txn, record.LogicalDigest)
			if err != nil || !complete {
				t.Fatalf("completion = epoch %d complete %v err %v", epoch, complete, err)
			}
			snapshot, err := reopened.coordinator.Inspect(context.Background(), txn)
			if err != nil || snapshot.Root.State != coordination.StateCommitted ||
				snapshot.Root.Epoch != epoch {
				t.Fatalf("recovered snapshot = %#v, %v", snapshot, err)
			}
			plan, err := reopened.intents.Materialize(context.Background(), transaction.MaterializeRequest{
				TXN: txn, LogicalDigest: record.LogicalDigest,
			})
			if err != nil {
				t.Fatal(err)
			}
			if err := reopened.physical.Verify(context.Background(), epoch, plan.Cells); err != nil {
				t.Fatalf("recovered physical cells: %v", err)
			}
			head, err := reopened.allocator.CurrentHead(context.Background())
			if err != nil || head.Frontier < epoch {
				t.Fatalf("recovered frontier = %#v, %v", head, err)
			}
		})
	}
}

func TestOpenSettlesConflictingPendingIntentsInOnePass(t *testing.T) {
	config := testRuntimeConfig(t, testDirectory(t))
	config.RecoveryLimit = 2
	config.RecoveryConcurrency = 2
	runtime, err := Open(config)
	if err != nil {
		t.Fatal(err)
	}
	base := testIntent(
		t, config.Domain, "create", "base", "v1",
		guard.ModeAbsentOrIdentical, 0, coordination.Digest{},
	)
	baseResult, err := runtime.Publish(context.Background(), Request{Intent: base})
	if err != nil {
		t.Fatal(err)
	}
	left := testIntent(
		t, config.Domain, "update", "pending-left", "left",
		guard.ModeMutate, baseResult.Epoch, baseResult.LogicalDigest,
	)
	right := testIntent(
		t, config.Domain, "update", "pending-right", "right",
		guard.ModeMutate, baseResult.Epoch, baseResult.LogicalDigest,
	)
	leftRecord, _, err := runtime.intents.Put(context.Background(), left)
	if err != nil {
		t.Fatal(err)
	}
	rightRecord, _, err := runtime.intents.Put(context.Background(), right)
	if err != nil {
		t.Fatal(err)
	}
	if err := runtime.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := Open(config)
	if err != nil {
		t.Fatalf("single-pass conflict recovery open = %v", err)
	}
	defer reopened.Close()
	candidates, _, err := reopened.intents.Candidates(context.Background(), nil, 2)
	if err != nil || len(candidates) != 0 {
		t.Fatalf("conflicted pending candidates = %#v, %v", candidates, err)
	}
	committed, conflicted := 0, 0
	for _, txn := range []coordination.TXN{leftRecord.TXN, rightRecord.TXN} {
		snapshot, err := reopened.Inspect(context.Background(), txn)
		if err != nil {
			t.Fatal(err)
		}
		switch snapshot.Root.State {
		case coordination.StateCommitted:
			committed++
		case coordination.StateConflicted:
			conflicted++
		}
	}
	if committed != 1 || conflicted != 1 {
		t.Fatalf("recovery outcomes committed=%d conflicted=%d", committed, conflicted)
	}
}

func TestOpenSettlesCompetingFirstCreateIntentsInOnePass(t *testing.T) {
	config := testRuntimeConfig(t, testDirectory(t))
	config.RecoveryLimit = 2
	config.RecoveryConcurrency = 2
	runtime, err := Open(config)
	if err != nil {
		t.Fatal(err)
	}
	left := testIntent(
		t, config.Domain, "create", "first-left", "left",
		guard.ModeAbsentOrIdentical, 0, coordination.Digest{},
	)
	right := testIntent(
		t, config.Domain, "create", "first-right", "right",
		guard.ModeAbsentOrIdentical, 0, coordination.Digest{},
	)
	leftRecord, _, err := runtime.intents.Put(context.Background(), left)
	if err != nil {
		t.Fatal(err)
	}
	rightRecord, _, err := runtime.intents.Put(context.Background(), right)
	if err != nil {
		t.Fatal(err)
	}
	if err := runtime.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := Open(config)
	if err != nil {
		t.Fatalf("single-pass first-create recovery open = %v", err)
	}
	defer reopened.Close()
	candidates, _, err := reopened.intents.Candidates(context.Background(), nil, 2)
	if err != nil || len(candidates) != 0 {
		t.Fatalf("first-create pending candidates = %#v, %v", candidates, err)
	}
	committed, conflicted := 0, 0
	for _, txn := range []coordination.TXN{leftRecord.TXN, rightRecord.TXN} {
		snapshot, err := reopened.Inspect(context.Background(), txn)
		if err != nil {
			t.Fatal(err)
		}
		switch snapshot.Root.State {
		case coordination.StateCommitted:
			committed++
		case coordination.StateConflicted:
			conflicted++
		}
	}
	if committed != 1 || conflicted != 1 {
		t.Fatalf("first-create outcomes committed=%d conflicted=%d", committed, conflicted)
	}
}

func TestOpenSettlesNewlyPoisonedIntentInOnePass(t *testing.T) {
	directory := testDirectory(t)
	config := testRuntimeConfig(t, directory)
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
	intent := testIntent(
		t, config.Domain, "poison-on-open", "poison", "expected",
		guard.ModeAbsentOrIdentical, 0, coordination.Digest{},
	)
	if _, err := runtime.Publish(
		context.Background(), Request{Intent: intent},
	); err == nil {
		t.Fatal("physical-stage failure did not occur")
	}
	txn, _ := DeriveTXN(config.Domain, intent.Operation, intent.Token)
	snapshot, err := runtime.Inspect(context.Background(), txn)
	if err != nil {
		t.Fatal(err)
	}
	corrupt, _ := cclient.NewMutation([]byte("record/one"))
	corrupt.Put(
		[]byte("record"), []byte("v1"), nil,
		int64(snapshot.Root.Epoch), []byte("corrupt"),
	)
	if err := runtime.engine.Write("records", []*cclient.Mutation{corrupt}); err != nil {
		t.Fatal(err)
	}
	if err := runtime.Close(); err != nil {
		t.Fatal(err)
	}
	config.testStageHook = nil
	reopened, err := Open(config)
	if err != nil {
		t.Fatalf("single-pass poison recovery open = %v", err)
	}
	defer reopened.Close()
	snapshot, err = reopened.Inspect(context.Background(), txn)
	if err != nil || snapshot.Root.State != coordination.StatePoisoned {
		t.Fatalf("poisoned recovery root = %#v, %v", snapshot, err)
	}
	candidates, _, err := reopened.intents.Candidates(context.Background(), nil, 1)
	if err != nil || len(candidates) != 0 {
		t.Fatalf("poisoned pending candidates = %#v, %v", candidates, err)
	}
}

func TestExplorerRetryReusesDurableAttemptAcrossPublicationFaults(t *testing.T) {
	for _, stage := range []recoveryStage{
		recoveryStageIntent,
		recoveryStageCommitted,
		recoveryStageComplete,
	} {
		t.Run(fmt.Sprintf("stage-%d", stage), func(t *testing.T) {
			config := testRuntimeConfig(t, testDirectory(t))
			fired := false
			config.testStageHook = func(got recoveryStage) error {
				if got == stage && !fired {
					fired = true
					return context.Canceled
				}
				return nil
			}
			embedded, err := OpenExplorer(config, explorer.Options{})
			if err != nil {
				t.Fatal(err)
			}
			defer embedded.Close()
			source := explorer.Source{
				URI: "file:///retry.md", Title: "Retry",
				MediaType: explorer.MediaTypeMarkdown,
				Content:   "# Retry\n\nExactly once.\n",
			}
			analyzed, err := explorer.AnalyzeSource(source)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := embedded.Explorer.Ingest(
				context.Background(), source,
			); err == nil || !explorer.IsIndeterminateCommit(err) {
				t.Fatalf("first ingest at stage %d = %v", stage, err)
			}
			row := []byte(
				"document/" + string(analyzed.Document.ID) + "/" +
					string(analyzed.Revision.ID),
			)
			attempt, err := embedded.Runtime.RecordAttempt(
				context.Background(),
				explorer.RecordPublication{
					Operation: []byte("explorer-document-record-v1"),
					Token:     documentTestRecordKey(row),
					Table:     explorer.EmbeddedTableName, Row: row,
					Family: []byte("record"), Qualifier: []byte("v2"),
				},
			)
			if err != nil || attempt == nil || len(attempt.Value) == 0 {
				t.Fatalf("durable attempt at stage %d = %#v, %v", stage, attempt, err)
			}
			retry, err := embedded.Explorer.Ingest(context.Background(), source)
			if err != nil || retry.Revision.ID != analyzed.Revision.ID {
				t.Fatalf("retry at stage %d = %#v, %v", stage, retry, err)
			}
			after, err := embedded.Runtime.RecordAttempt(
				context.Background(),
				explorer.RecordPublication{
					Operation: []byte("explorer-document-record-v1"),
					Token:     documentTestRecordKey(row),
					Table:     explorer.EmbeddedTableName, Row: row,
					Family: []byte("record"), Qualifier: []byte("v2"),
				},
			)
			if err != nil || after == nil || !bytes.Equal(after.Value, attempt.Value) ||
				after.ExpectedEpoch != attempt.ExpectedEpoch ||
				after.ExpectedDigest != attempt.ExpectedDigest {
				t.Fatalf("attempt changed at stage %d: before %#v after %#v err %v", stage, attempt, after, err)
			}
			documents, err := embedded.Explorer.Documents(context.Background())
			if err != nil || len(documents) != 1 ||
				documents[0].Revision.ID != analyzed.Revision.ID {
				t.Fatalf("retry documents at stage %d = %#v, %v", stage, documents, err)
			}
		})
	}
}

func TestPendingPublicationBlocksLaterChangeFeedAdvance(t *testing.T) {
	config := testRuntimeConfig(t, testDirectory(t))
	fired := false
	config.testStageHook = func(stage recoveryStage) error {
		if stage == recoveryStageIntent && !fired {
			fired = true
			return context.Canceled
		}
		return nil
	}
	embedded, err := OpenExplorer(config, explorer.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer embedded.Close()
	first := explorer.Source{
		URI: "file:///first.md", Title: "First",
		MediaType: explorer.MediaTypeMarkdown, Content: "# First\n",
	}
	second := explorer.Source{
		URI: "file:///second.md", Title: "Second",
		MediaType: explorer.MediaTypeMarkdown, Content: "# Second\n",
	}
	if _, err := embedded.Explorer.Ingest(
		context.Background(), first,
	); err == nil || !explorer.IsIndeterminateCommit(err) {
		t.Fatalf("first indeterminate ingest = %v", err)
	}
	pending, err := embedded.Runtime.PendingPublications(context.Background())
	if err != nil || !pending {
		t.Fatalf("pending publication after failure = %v, %v", pending, err)
	}
	if _, err := embedded.Explorer.Ingest(
		context.Background(), second,
	); !shoal.IsErrorCode(err, shoal.ErrorUnavailable) {
		t.Fatalf("later ingest while pending = %v", err)
	}
	if _, err := embedded.Explorer.Ingest(
		context.Background(), first,
	); err != nil {
		t.Fatalf("retry first ingest = %v", err)
	}
	if _, err := embedded.Explorer.Ingest(
		context.Background(), second,
	); err != nil {
		t.Fatalf("second ingest after recovery = %v", err)
	}
	feed, err := embedded.Explorer.Changes(
		context.Background(), explorer.ChangeRequest{Limit: 10},
	)
	if err != nil || len(feed.Changes) != 2 ||
		feed.Changes[0].Sequence != 1 ||
		feed.Changes[1].Sequence != 2 ||
		feed.Head != 2 {
		t.Fatalf("change feed after retry = %#v, %v", feed, err)
	}
}

func TestTransactionalRuntimeRejectsLegacyExplorerOpen(t *testing.T) {
	config := testRuntimeConfig(t, testDirectory(t))
	embedded, err := OpenExplorer(config, explorer.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer embedded.Close()
	legacy, err := explorer.Open(config.Directory)
	if err == nil {
		_ = legacy.Close()
		t.Fatal("legacy Explorer opened a transaction-owned directory")
	}
	if !shoal.IsErrorCode(err, shoal.ErrorUnavailable) {
		t.Fatalf("legacy open error = %v", err)
	}
}

func TestRuntimeRequiresFullPerWriteWALSync(t *testing.T) {
	tests := []struct {
		name    string
		options engine.Options
	}{
		{
			name: "normal",
			options: engine.Options{
				WALSyncMode: localwal.SyncNormal,
			},
		},
		{
			name: "off",
			options: engine.Options{
				WALSyncMode: localwal.SyncOff,
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			config := testRuntimeConfig(t, testDirectory(t))
			config.EngineOptions = test.options
			if runtime, err := Open(config); err == nil ||
				!errors.Is(err, transaction.ErrInvalid) {
				if runtime != nil {
					_ = runtime.Close()
				}
				t.Fatalf("unsafe WAL configuration = %v", err)
			}
		})
	}
	config := testRuntimeConfig(t, testDirectory(t))
	config.EngineOptions = engine.Options{
		WALSyncMode: localwal.SyncFull, WALSyncInterval: time.Millisecond,
	}
	runtime, err := Open(config)
	if err != nil {
		t.Fatalf("full sync with no-op interval = %v", err)
	}
	_ = runtime.Close()
}

func TestPublishDoesNotHealCallerCancellation(t *testing.T) {
	config := testRuntimeConfig(t, testDirectory(t))
	runtime, err := Open(config)
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Close()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	intent := testIntent(
		t, config.Domain, "create", "canceled", "value",
		guard.ModeAbsentOrIdentical, 0, coordination.Digest{},
	)
	if _, err := runtime.Publish(ctx, Request{Intent: intent}); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled publication = %v", err)
	}
	txn, _ := DeriveTXN(config.Domain, intent.Operation, intent.Token)
	if _, err := runtime.Inspect(context.Background(), txn); !errors.Is(err, transaction.ErrNotFound) {
		t.Fatalf("canceled publication persisted transaction = %v", err)
	}
}

func TestRecoveryPageBoundIgnoresCompletedHistory(t *testing.T) {
	directory := testDirectory(t)
	config := testRuntimeConfig(t, directory)
	config.RecoveryLimit = 1
	config.RecoveryMaxPages = 1
	runtime, err := Open(config)
	if err != nil {
		t.Fatal(err)
	}
	for index := 0; index < 24; index++ {
		row := []byte(fmt.Sprintf("history/%d", index))
		intent := committedReadIntent(
			t, config.Domain, fmt.Sprintf("history-%d", index),
			row, []byte("committed"), guard.ModeAbsentOrIdentical,
			0, coordination.Digest{},
		)
		if _, err := runtime.Publish(
			context.Background(), Request{Intent: intent},
		); err != nil {
			t.Fatal(err)
		}
	}
	if candidates, _, err := runtime.intents.Candidates(
		context.Background(), nil, 1,
	); err != nil || len(candidates) != 0 {
		t.Fatalf("completed pending candidates = %#v, %v", candidates, err)
	}
	if err := runtime.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(config)
	if err != nil {
		t.Fatalf("tiny-page reopen after completed history: %v", err)
	}
	defer reopened.Close()
	head, err := reopened.CurrentHead(context.Background())
	if err != nil || head.Frontier != 24 {
		t.Fatalf("reopened completed frontier = %#v, %v", head, err)
	}
}

func TestRuntimeRejectsSecondOpenAndStaleFence(t *testing.T) {
	config := testRuntimeConfig(t, testDirectory(t))
	runtime, err := Open(config)
	if err != nil {
		t.Fatal(err)
	}
	if second, err := Open(config); err == nil {
		_ = second.Close()
		t.Fatal("second process-style open succeeded")
	}
	runtime.authority.Fence++
	intent := testIntent(
		t, config.Domain, "create", "stale", "value",
		guard.ModeAbsentOrIdentical, 0, coordination.Digest{},
	)
	if _, err := runtime.Publish(context.Background(), Request{Intent: intent}); !errors.Is(err, transaction.ErrConflict) {
		t.Fatalf("stale fence publish = %v", err)
	}
	runtime.authority.Fence--
	if _, err := runtime.Publish(context.Background(), Request{
		Intent: intent, LeaseUntil: time.Now().UTC().Add(-time.Second),
	}); !errors.Is(err, transaction.ErrInvalid) {
		t.Fatalf("expired lease publish = %v", err)
	}
	if err := runtime.Close(); err != nil {
		t.Fatal(err)
	}
	stale := config
	stale.Authority.Generation++
	stale.Authority.Fence++
	if reopened, err := Open(stale); err == nil || !errors.Is(err, transaction.ErrConflict) {
		if reopened != nil {
			_ = reopened.Close()
		}
		t.Fatalf("stale durable authority open = %v", err)
	}
}

func TestRuntimeEmbeddedEngineLifetime(t *testing.T) {
	var nilRuntime *Runtime
	if got := nilRuntime.EmbeddedEngine(); got != nil {
		t.Fatalf("nil runtime engine = %p", got)
	}

	runtime, err := Open(testRuntimeConfig(t, testDirectory(t)))
	if err != nil {
		t.Fatal(err)
	}
	eng := runtime.EmbeddedEngine()
	if eng == nil || eng != runtime.engine {
		t.Fatalf("embedded engine = %p, runtime engine = %p", eng, runtime.engine)
	}
	if err := runtime.Close(); err != nil {
		t.Fatal(err)
	}
	if got := runtime.EmbeddedEngine(); got != nil {
		t.Fatalf("closed runtime engine = %p", got)
	}
}

func TestRuntimeReconcilesAmbiguousCommitCAS(t *testing.T) {
	config := testRuntimeConfig(t, testDirectory(t))
	var mu sync.Mutex
	fired := false
	config.testStageHook = func(stage recoveryStage) error {
		mu.Lock()
		defer mu.Unlock()
		if stage == recoveryStageCommitted && !fired {
			fired = true
			return allocator.ErrConditionalUnknown
		}
		return nil
	}
	runtime, err := Open(config)
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Close()
	intent := testIntent(
		t, config.Domain, "create", "ambiguous", "value",
		guard.ModeAbsentOrIdentical, 0, coordination.Digest{},
	)
	result, err := runtime.Publish(context.Background(), Request{Intent: intent})
	if err != nil || result.Epoch != 1 {
		t.Fatalf("ambiguous commit result = %#v, %v", result, err)
	}
	mu.Lock()
	wasFired := fired
	mu.Unlock()
	if !wasFired {
		t.Fatal("ambiguous commit hook was not reached")
	}
	retry, err := runtime.Publish(context.Background(), Request{Intent: intent})
	if err != nil || !retry.Unchanged || retry.Epoch != result.Epoch {
		t.Fatalf("retry after ambiguous commit = %#v, %v", retry, err)
	}
}

func TestRecordPublicationUsesStableDocumentExpectedHead(t *testing.T) {
	config := testRuntimeConfig(t, testDirectory(t))
	runtime, err := Open(config)
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Close()
	first := testRecordPublication("document", "revision-1", "one", nil)
	if _, err := runtime.PublishRecord(context.Background(), first); err != nil {
		t.Fatal(err)
	}
	head, err := runtime.RecordHead(context.Background(), 'D', []byte("document"))
	if err != nil || head == nil || !bytes.Equal(head.WinnerID, []byte("revision-1")) {
		t.Fatalf("first document head = %#v, %v", head, err)
	}
	left := testRecordPublication("document", "revision-2a", "left", head)
	right := testRecordPublication("document", "revision-2b", "right", head)
	type outcome struct {
		request explorer.RecordPublication
		result  explorer.RecordPublicationResult
		err     error
	}
	outcomes := make(chan outcome, 2)
	var wait sync.WaitGroup
	for _, request := range []explorer.RecordPublication{left, right} {
		request := request
		wait.Add(1)
		go func() {
			defer wait.Done()
			result, err := runtime.PublishRecord(context.Background(), request)
			outcomes <- outcome{request: request, result: result, err: err}
		}()
	}
	wait.Wait()
	close(outcomes)
	successes, failures := 0, 0
	var loser explorer.RecordPublication
	for outcome := range outcomes {
		if outcome.err == nil {
			successes++
		} else {
			failures++
			loser = outcome.request
		}
	}
	if successes != 1 || failures != 1 {
		t.Fatalf("divergent publications = successes %d failures %d", successes, failures)
	}
	if _, err := runtime.PublishRecord(context.Background(), loser); err == nil ||
		!errors.Is(err, transaction.ErrConflict) {
		t.Fatalf("stale expected-head retry = %v", err)
	}
	head, err = runtime.RecordHead(context.Background(), 'D', []byte("document"))
	if err != nil || head == nil ||
		(!bytes.Equal(head.WinnerID, []byte("revision-2a")) &&
			!bytes.Equal(head.WinnerID, []byte("revision-2b"))) {
		t.Fatalf("winning document head = %#v, %v", head, err)
	}
	refreshed := testRecordPublication(
		"document", string(loser.WinnerID), string(loser.Value), head,
	)
	if _, err := runtime.PublishRecord(context.Background(), refreshed); err != nil {
		t.Fatalf("refreshed expected-head retry = %v", err)
	}
}

func TestConcurrentFirstDocumentPublicationsResolveWithoutIndeterminate(t *testing.T) {
	config := testRuntimeConfig(t, testDirectory(t))
	runtime, err := Open(config)
	if err != nil {
		t.Fatal(err)
	}
	left := testRecordPublication("new-document", "revision-a", "left", nil)
	right := testRecordPublication("new-document", "revision-b", "right", nil)
	errs := make(chan error, 2)
	var wait sync.WaitGroup
	for _, request := range []explorer.RecordPublication{left, right} {
		request := request
		wait.Add(1)
		go func() {
			defer wait.Done()
			_, err := runtime.PublishRecord(context.Background(), request)
			errs <- err
		}()
	}
	wait.Wait()
	close(errs)
	successes, conflicts := 0, 0
	for err := range errs {
		switch {
		case err == nil:
			successes++
		case errors.Is(err, transaction.ErrConflict) &&
			!explorer.IsIndeterminateCommit(err):
			conflicts++
		default:
			t.Fatalf("unexpected first-publication result: %v", err)
		}
	}
	if successes != 1 || conflicts != 1 {
		t.Fatalf("first publications successes=%d conflicts=%d", successes, conflicts)
	}
	if err := runtime.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(config)
	if err != nil {
		t.Fatalf("reopen after first-publication conflict = %v", err)
	}
	defer reopened.Close()
	candidates, _, err := reopened.intents.Candidates(context.Background(), nil, 1)
	if err != nil || len(candidates) != 0 {
		t.Fatalf("pending first-publication candidates = %#v, %v", candidates, err)
	}
}

func TestConcurrentIdenticalFirstPublicationIsIdempotent(t *testing.T) {
	config := testRuntimeConfig(t, testDirectory(t))
	runtime, err := Open(config)
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Close()
	intent := testIntent(
		t, config.Domain, "create", "same-token", "same-value",
		guard.ModeAbsentOrIdentical, 0, coordination.Digest{},
	)
	results := make(chan Result, 2)
	errs := make(chan error, 2)
	var wait sync.WaitGroup
	for range 2 {
		wait.Add(1)
		go func() {
			defer wait.Done()
			result, err := runtime.Publish(
				context.Background(), Request{Intent: intent},
			)
			results <- result
			errs <- err
		}()
	}
	wait.Wait()
	close(results)
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("identical concurrent publication = %v", err)
		}
	}
	var epoch coordination.Epoch
	unchanged := 0
	for result := range results {
		if epoch == 0 {
			epoch = result.Epoch
		}
		if result.Epoch != epoch {
			t.Fatalf("identical publications used epochs %d and %d", epoch, result.Epoch)
		}
		if result.Unchanged {
			unchanged++
		}
	}
	if unchanged != 1 {
		t.Fatalf("unchanged identical publications = %d, want 1", unchanged)
	}
}

func TestAbsentPublicationRetriesAfterForeignPendingRelease(t *testing.T) {
	config := testRuntimeConfig(t, testDirectory(t))
	runtime, err := Open(config)
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Close()
	intent := testIntent(
		t, config.Domain, "create", "after-release", "value",
		guard.ModeAbsentOrIdentical, 0, coordination.Digest{},
	)
	digest, err := LogicalDigest(intent)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := buildPlan(intent, digest)
	if err != nil {
		t.Fatal(err)
	}
	item := plan.Guards[0]
	foreign, err := runtime.guards.Acquire(context.Background(), guard.Intent{
		Entity: item.Entity, TXN: coordination.TXN("foreign"),
		Owner: coordination.OwnerID("foreign"), LeaseUntil: time.Now().UTC().Add(time.Minute),
		Fence: 1, AuthorityGeneration: config.Authority.Generation,
		AuthorityFence:       config.Authority.Fence,
		RetentionGeneration:  config.Authority.RetentionGeneration,
		RetirementGeneration: item.RetirementGeneration,
		HistoryFloor:         config.Authority.HistoryFloor,
		Mode:                 guard.ModeAbsentOrIdentical, DesiredState: item.DesiredState,
		DesiredWinnerID: item.DesiredWinnerID, DesiredDigest: item.DesiredDigest,
		LPART: item.LPART, LogicalPolicyID: item.LogicalPolicyID,
		ManifestChunk: item.ManifestChunk, ManifestEntry: item.ManifestEntry,
		Ordinal: item.Ordinal, PhysicalDigest: item.PhysicalDigest,
	})
	if err != nil {
		t.Fatal(err)
	}
	released := make(chan error, 1)
	go func() {
		time.Sleep(20 * time.Millisecond)
		released <- runtime.guards.Abort(context.Background(), foreign.Pending, false)
	}()
	result, err := runtime.Publish(context.Background(), Request{Intent: intent})
	if err != nil || result.Epoch == 0 {
		t.Fatalf("publication after pending release = %#v, %v", result, err)
	}
	if err := <-released; err != nil {
		t.Fatalf("release foreign guard = %v", err)
	}
}

func TestExplorerLoadHidesUncommittedAndPoisonedPhysicalRevision(t *testing.T) {
	directory := testDirectory(t)
	config := testRuntimeConfig(t, directory)
	fired := false
	config.testStageHook = func(stage recoveryStage) error {
		if stage == recoveryStagePhysical && !fired {
			fired = true
			return context.Canceled
		}
		return nil
	}
	embedded, err := OpenExplorer(config, explorer.Options{})
	if err != nil {
		t.Fatal(err)
	}
	source := explorer.Source{
		URI: "file:///staged.md", Title: "Staged",
		MediaType: explorer.MediaTypeMarkdown, Content: "# Staged\n\nNot committed.\n",
	}
	analyzed, err := explorer.AnalyzeSource(source)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := embedded.Explorer.Ingest(context.Background(), source); err == nil ||
		!explorer.IsIndeterminateCommit(err) {
		t.Fatalf("physical-stage ingest = %v", err)
	}
	if err := embedded.Close(); err != nil {
		t.Fatal(err)
	}

	config.testStageHook = nil
	config.DisableRecoveryOnOpen = true
	config.PhysicalTables = append(config.PhysicalTables, explorer.EmbeddedTableName)
	runtime, err := Open(config)
	if err != nil {
		t.Fatal(err)
	}
	corpus, err := explorer.OpenWithEmbeddedEngine(runtime.engine, explorer.Options{}, runtime)
	if err != nil {
		t.Fatal(err)
	}
	documents, err := corpus.Documents(context.Background())
	if err != nil || len(documents) != 0 {
		t.Fatalf("uncommitted documents = %#v, %v", documents, err)
	}
	if err := corpus.Close(); err != nil {
		t.Fatal(err)
	}

	row := []byte("document/" + string(analyzed.Document.ID) + "/" + string(analyzed.Revision.ID))
	key := sha256.Sum256(append([]byte("explorer-document-record-v1\x00"), row...))
	txn, found, err := runtime.intents.Attempt(context.Background(), key[:])
	if err != nil || !found {
		t.Fatalf("document attempt binding = %x, %v, %v", txn, found, err)
	}
	snapshot, err := runtime.Inspect(context.Background(), txn)
	if err != nil || snapshot.Root.Epoch == 0 {
		t.Fatalf("staged transaction = %#v, %v", snapshot, err)
	}
	corrupt, err := cclient.NewMutation(row)
	if err != nil {
		t.Fatal(err)
	}
	corrupt.Put(
		[]byte("record"), []byte("v2"), nil,
		int64(snapshot.Root.Epoch), []byte("corrupt-uncommitted-record"),
	)
	if err := runtime.engine.Write(explorer.EmbeddedTableName, []*cclient.Mutation{corrupt}); err != nil {
		t.Fatal(err)
	}
	if err := runtime.Recover(context.Background()); err != nil {
		t.Fatalf("settle poisoned staged transaction = %v", err)
	}
	snapshot, err = runtime.Inspect(context.Background(), txn)
	if err != nil || snapshot.Root.State != coordination.StatePoisoned {
		t.Fatalf("poisoned transaction = %#v, %v", snapshot, err)
	}
	if candidates, _, err := runtime.intents.Candidates(
		context.Background(), nil, 1,
	); err != nil || len(candidates) != 0 {
		t.Fatalf("poisoned pending candidates = %#v, %v", candidates, err)
	}
	if err := runtime.Close(); err != nil {
		t.Fatal(err)
	}

	config.DisableRecoveryOnOpen = false
	reopened, err := OpenExplorer(config, explorer.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	documents, err = reopened.Explorer.Documents(context.Background())
	if err != nil || len(documents) != 0 {
		t.Fatalf("poisoned physical documents = %#v, %v", documents, err)
	}
}

func TestRecoveryRejectsCorruptCheckpoint(t *testing.T) {
	config := testRuntimeConfig(t, testDirectory(t))
	runtime, err := Open(config)
	if err != nil {
		t.Fatal(err)
	}
	intent := testIntent(
		t, config.Domain, "create", "checkpoint", "value",
		guard.ModeAbsentOrIdentical, 0, coordination.Digest{},
	)
	if _, err := runtime.Publish(context.Background(), Request{Intent: intent}); err != nil {
		t.Fatal(err)
	}
	row, _ := coordination.AllocatorRow(config.Domain)
	coordinate := allocator.Coordinate{Row: row, Family: []byte("q"), Qualifier: []byte("head")}
	cells, err := runtime.store.ReadExact(context.Background(), []allocator.Coordinate{coordinate})
	if err != nil || len(cells) != 1 {
		t.Fatalf("read allocator head = %#v, %v", cells, err)
	}
	corrupt := append([]byte(nil), cells[0].Value...)
	corrupt[len(corrupt)-1] ^= 0xff
	status, err := runtime.store.CompareAndMutate(context.Background(), allocator.Mutation{
		Row: row,
		Conditions: []allocator.Condition{{
			Coordinate: coordinate, Value: cells[0].Value,
			Timestamp: cells[0].Timestamp, TimestampSet: true,
		}},
		Updates: []allocator.Update{{
			Coordinate: coordinate, Value: corrupt, Timestamp: cells[0].Timestamp + 1,
		}},
	})
	if err != nil || status != allocator.StatusAccepted {
		t.Fatalf("corrupt checkpoint = %v, %v", status, err)
	}
	if err := runtime.Close(); err != nil {
		t.Fatal(err)
	}
	if reopened, err := Open(config); err == nil {
		_ = reopened.Close()
		t.Fatal("runtime reopened with a corrupt allocator checkpoint")
	}
}

func TestExplorerDocumentUsesTransactionalPublication(t *testing.T) {
	config := testRuntimeConfig(t, testDirectory(t))
	embedded, err := OpenExplorer(config, explorer.Options{})
	if err != nil {
		t.Fatal(err)
	}
	source := explorer.Source{
		URI: "file:///document.md", Title: "Document",
		MediaType: explorer.MediaTypeMarkdown, Content: "# Heading\n\nBody.\n",
	}
	first, err := embedded.Explorer.IngestWithOptions(
		context.Background(), source, explorer.IngestOptions{
			CreatedAt: time.Date(2026, time.September, 5, 18, 0, 0, 0, time.UTC),
		},
	)
	if err != nil || first.Disposition != explorer.IngestApplied {
		t.Fatalf("transactional ingest = %#v, %v", first, err)
	}
	head, err := embedded.Runtime.CurrentHead(context.Background())
	if err != nil || head.Frontier == 0 {
		t.Fatalf("transactional frontier = %#v, %v", head, err)
	}
	if err := embedded.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := OpenExplorer(config, explorer.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	second, err := reopened.Explorer.IngestWithOptions(
		context.Background(), source, explorer.IngestOptions{
			CreatedAt: time.Date(2026, time.September, 5, 18, 0, 0, 0, time.UTC),
		},
	)
	if err != nil || second.Disposition != explorer.IngestUnchanged ||
		second.Revision.ID != first.Revision.ID {
		t.Fatalf("reopened ingest = %#v, %v", second, err)
	}
}

func testRuntimeConfig(t *testing.T, directory string) Config {
	t.Helper()
	return Config{
		Directory: directory, Domain: coordination.DomainID("domain"),
		Owner: coordination.OwnerID("recovery-worker"),
		Authority: transaction.Authority{
			Generation: 1, Fence: 1, Holder: coordination.OwnerID("embedded-process"),
			Mode:                coordination.WriterModeEmbeddedPrimary,
			RetentionGeneration: 1, HistoryFloor: 1,
		},
		PhysicalTables:   []string{"records"},
		Lease:            time.Minute,
		RecoveryLimit:    2,
		RecoveryMaxPages: 64,
		RetryBackoff:     time.Nanosecond,
		RecoveryBackoff:  time.Nanosecond,
	}
}

func testIntent(
	t *testing.T,
	domain coordination.DomainID,
	operation, token, value string,
	mode guard.Mode,
	expectedEpoch coordination.Epoch,
	expectedDigest coordination.Digest,
) Intent {
	t.Helper()
	lpart, err := Partition(domain, []byte("record/one"))
	if err != nil {
		t.Fatal(err)
	}
	return Intent{
		Operation: []byte(operation), Token: []byte(token),
		Cells: []Cell{{
			Table: "records", Row: []byte("record/one"), Family: []byte("record"),
			Qualifier: []byte("v1"), Value: []byte(value), EpochTimestamp: true,
			LPART: lpart, CopyGeneration: 1,
		}},
		Guards: []GuardIntent{{
			Entity: guard.Entity{Kind: 'R', ID: coordination.EntityID("record/one")},
			Mode:   mode, ExpectedEpoch: expectedEpoch, ExpectedDigest: expectedDigest,
			DesiredState: guard.StateLive, DesiredWinnerID: []byte(value),
			LPART: lpart, LogicalPolicyID: []byte("embedded/default"),
			RetirementGeneration: 1,
		}},
		Results: []ResultIdentity{{Kind: []byte("record"), ID: []byte("record/one")}},
	}
}

func testRecordPublication(
	document, revision, value string,
	head *explorer.RecordPublicationHead,
) explorer.RecordPublication {
	request := explorer.RecordPublication{
		Operation:       []byte("document-test-v1"),
		Table:           "records",
		Row:             []byte("document/" + document + "/" + revision),
		Family:          []byte("record"),
		Qualifier:       []byte("v1"),
		Value:           []byte(value),
		EntityKind:      'D',
		EntityID:        []byte(document),
		WinnerID:        []byte(revision),
		Partition:       []byte(document),
		LogicalPolicyID: []byte("embedded/default"),
		ResultKind:      []byte("document-revision"),
		ResultID:        []byte(revision),
	}
	stable := sha256.Sum256(request.Row)
	request.RecordKey = stable[:]
	hash := sha256.New()
	_, _ = hash.Write(stable[:])
	if head != nil {
		request.ExpectedEpoch = head.Epoch
		request.ExpectedDigest = head.LogicalDigest
		var epoch [8]byte
		binary.BigEndian.PutUint64(epoch[:], uint64(head.Epoch))
		_, _ = hash.Write(epoch[:])
		_, _ = hash.Write(head.LogicalDigest[:])
	}
	request.Token = hash.Sum(nil)
	return request
}

func documentTestRecordKey(row []byte) []byte {
	key := sha256.Sum256(append([]byte("explorer-document-record-v1\x00"), row...))
	return key[:]
}
