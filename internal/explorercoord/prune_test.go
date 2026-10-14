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
	"encoding/binary"
	"errors"
	"fmt"
	"sync"
	"testing"

	"github.com/phrocker/shoal-oss/internal/cclient"
	"github.com/phrocker/shoal-oss/pkg/explorer/coordination"
	"github.com/phrocker/shoal-oss/pkg/explorer/coordination/guard"
	"github.com/phrocker/shoal-oss/pkg/explorer/coordination/transaction"
)

func TestPruneCommittedRetiresCellsAndAdvancesLocalFloor(t *testing.T) {
	config := testRuntimeConfig(t, testDirectory(t))
	runtime, err := Open(config)
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Close()

	first := publishPruneTarget(t, runtime, config.Domain, "event/0001", "one")
	second := publishPruneTarget(t, runtime, config.Domain, "event/0002", "two")
	before, err := runtime.CurrentHead(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	request := pruneTestRequest(
		t,
		config.Domain,
		"prune-1",
		2,
		nil,
		[]PruneTarget{first},
	)
	result, err := runtime.PruneCommitted(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	if result.Pruned != 1 || result.Epoch <= second.Cell.Epoch {
		t.Fatalf("prune result = %#v", result)
	}
	replay, err := runtime.PruneCommitted(context.Background(), request)
	if err != nil || !replay.Unchanged || replay.Epoch != result.Epoch {
		t.Fatalf("prune replay = %#v, %v", replay, err)
	}

	page, err := runtime.ScanCommitted(context.Background(), CommittedScanRequest{
		Table: "records", RowPrefix: []byte("event/"),
		Family: []byte("event"), Qualifier: []byte("record"), Limit: 10,
	})
	if err != nil || len(page.Cells) != 1 ||
		!bytes.Equal(page.Cells[0].Cell.Coordinate.Row, second.Cell.Cell.Coordinate.Row) {
		t.Fatalf("latest events after prune = %#v, %v", page, err)
	}
	historical, found, err := runtime.ReadCommittedCell(
		context.Background(),
		first.Table,
		first.Cell.Cell.Coordinate.Row,
		first.Cell.Cell.Coordinate.Family,
		first.Cell.Cell.Coordinate.Qualifier,
		first.Cell.Cell.Coordinate.Visibility,
		first.Cell.Epoch,
	)
	if err != nil || !found || !committedCellEqual(historical, first.Cell) {
		t.Fatalf("historical pruned event = %#v, %v, %v", historical, found, err)
	}
	if _, found, err := runtime.ReadCommittedCell(
		context.Background(),
		first.Table,
		first.Cell.Cell.Coordinate.Row,
		first.Cell.Cell.Coordinate.Family,
		first.Cell.Cell.Coordinate.Qualifier,
		first.Cell.Cell.Coordinate.Visibility,
		result.Epoch,
	); err != nil || found {
		t.Fatalf("latest pruned event = %v, %v", found, err)
	}
	floor, found, err := runtime.ReadCommittedCell(
		context.Background(),
		request.Checkpoint.Cell.Table,
		request.Checkpoint.Cell.Row,
		request.Checkpoint.Cell.Family,
		request.Checkpoint.Cell.Qualifier,
		request.Checkpoint.Cell.Visibility,
		result.Epoch,
	)
	if err != nil || !found ||
		!bytes.Equal(floor.Cell.Value, request.Checkpoint.Cell.Value) {
		t.Fatalf("event-local floor = %#v, %v, %v", floor, found, err)
	}
	head, pending, err := runtime.ReadEntity(context.Background(), first.Entity)
	if err != nil || head == nil || head.State != guard.StateTombstone ||
		head.Epoch != result.Epoch || pending != nil && pending.Active {
		t.Fatalf("retired event guard = %#v, %#v, %v", head, pending, err)
	}
	after, err := runtime.CurrentHead(context.Background())
	if err != nil || after.HistoryFloor != before.HistoryFloor {
		t.Fatalf("shared history floor changed: before=%#v after=%#v err=%v", before, after, err)
	}
}

func TestPruneCommittedRejectsForgedAndOversizedTargets(t *testing.T) {
	config := testRuntimeConfig(t, testDirectory(t))
	runtime, err := Open(config)
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Close()
	target := publishPruneTarget(t, runtime, config.Domain, "event/forged", "value")

	forged := target
	forged.Cell.Cell.Value = []byte("different")
	if _, err := runtime.PruneCommitted(
		context.Background(),
		pruneTestRequest(
			t,
			config.Domain,
			"forged",
			2,
			nil,
			[]PruneTarget{forged},
		),
	); !errors.Is(err, transaction.ErrConflict) {
		t.Fatalf("forged prune target = %v", err)
	}
	oversized := make([]PruneTarget, MaxPruneTargets+1)
	if _, err := runtime.PruneCommitted(context.Background(), PruneCommittedRequest{
		Operation: []byte("oversized-prune"),
		Token:     []byte("oversized-prune"),
		Targets:   oversized,
	}); !errors.Is(err, transaction.ErrInvalid) {
		t.Fatalf("oversized prune = %v", err)
	}
}

func TestPruneCommittedCrashRecovery(t *testing.T) {
	for _, crashStage := range []recoveryStage{
		recoveryStageIntent,
		recoveryStagePhysical,
		recoveryStageComplete,
	} {
		t.Run(fmt.Sprint(crashStage), func(t *testing.T) {
			config := testRuntimeConfig(t, testDirectory(t))
			armed := false
			fired := false
			config.testStageHook = func(stage recoveryStage) error {
				if armed && !fired && stage == crashStage {
					fired = true
					return context.Canceled
				}
				return nil
			}
			runtime, err := Open(config)
			if err != nil {
				t.Fatal(err)
			}
			target := publishPruneTarget(
				t,
				runtime,
				config.Domain,
				"event/crash",
				"value",
			)
			request := pruneTestRequest(
				t,
				config.Domain,
				fmt.Sprintf("prune-crash-%d", crashStage),
				2,
				nil,
				[]PruneTarget{target},
			)
			armed = true
			if _, err := runtime.PruneCommitted(
				context.Background(),
				request,
			); !errors.Is(err, ErrIndeterminatePublication) {
				t.Fatalf("prune crash stage %d = %v", crashStage, err)
			}
			if err := runtime.Close(); err != nil {
				t.Fatal(err)
			}
			config.testStageHook = nil
			reopened, err := Open(config)
			if err != nil {
				t.Fatalf("reopen stage %d = %v", crashStage, err)
			}
			defer reopened.Close()
			replay, err := reopened.PruneCommitted(
				context.Background(),
				request,
			)
			if err != nil || !replay.Unchanged {
				t.Fatalf("replay stage %d = %#v, %v", crashStage, replay, err)
			}
			if _, found, err := reopened.ReadCommittedCell(
				context.Background(),
				target.Table,
				target.Cell.Cell.Coordinate.Row,
				target.Cell.Cell.Coordinate.Family,
				target.Cell.Cell.Coordinate.Qualifier,
				target.Cell.Cell.Coordinate.Visibility,
				replay.Epoch,
			); err != nil || found {
				t.Fatalf("pruned event after recovery stage %d = %v, %v", crashStage, found, err)
			}
		})
	}
}

func TestPruneCommittedRejectsStaleCheckpointAndCancellation(t *testing.T) {
	config := testRuntimeConfig(t, testDirectory(t))
	runtime, err := Open(config)
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Close()
	first := publishPruneTarget(t, runtime, config.Domain, "event/stale-1", "one")
	firstRequest := pruneTestRequest(
		t,
		config.Domain,
		"floor-one",
		2,
		nil,
		[]PruneTarget{first},
	)
	if _, err := runtime.PruneCommitted(context.Background(), firstRequest); err != nil {
		t.Fatal(err)
	}
	floorHead, _, err := runtime.ReadEntity(
		context.Background(),
		firstRequest.Checkpoint.Guard.Entity,
	)
	if err != nil || floorHead == nil {
		t.Fatalf("floor head = %#v, %v", floorHead, err)
	}
	second := publishPruneTarget(t, runtime, config.Domain, "event/stale-2", "two")
	stale := pruneTestRequest(
		t,
		config.Domain,
		"floor-stale",
		3,
		&guard.Head{
			Epoch:         floorHead.Epoch - 1,
			LogicalDigest: floorHead.LogicalDigest,
		},
		[]PruneTarget{second},
	)
	if _, err := runtime.PruneCommitted(
		context.Background(),
		stale,
	); !errors.Is(err, transaction.ErrConflict) {
		t.Fatalf("stale floor prune = %v", err)
	}
	if latest, _, err := runtime.ReadEntity(
		context.Background(),
		second.Entity,
	); err != nil || latest == nil || latest.State != guard.StateLive {
		t.Fatalf("stale floor retired target = %#v, %v", latest, err)
	}

	third := publishPruneTarget(t, runtime, config.Domain, "event/canceled", "three")
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := runtime.PruneCommitted(
		canceled,
		pruneTestRequest(
			t,
			config.Domain,
			"floor-canceled",
			4,
			floorHead,
			[]PruneTarget{third},
		),
	); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled prune = %v", err)
	}
	if latest, _, err := runtime.ReadEntity(
		context.Background(),
		third.Entity,
	); err != nil || latest == nil || latest.State != guard.StateLive {
		t.Fatalf("canceled prune retired target = %#v, %v", latest, err)
	}
}

func TestPruneCommittedConcurrentTargetsHaveOneWinner(t *testing.T) {
	config := testRuntimeConfig(t, testDirectory(t))
	runtime, err := Open(config)
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Close()
	target := publishPruneTarget(t, runtime, config.Domain, "event/race", "value")
	requests := []PruneCommittedRequest{
		pruneTestRequest(
			t,
			config.Domain,
			"race-left",
			2,
			nil,
			[]PruneTarget{target},
		),
		pruneTestRequest(
			t,
			config.Domain,
			"race-right",
			2,
			nil,
			[]PruneTarget{target},
		),
	}
	errs := make(chan error, len(requests))
	var wait sync.WaitGroup
	for _, request := range requests {
		request := request
		wait.Add(1)
		go func() {
			defer wait.Done()
			_, err := runtime.PruneCommitted(context.Background(), request)
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
		case errors.Is(err, transaction.ErrConflict):
			conflicts++
		default:
			t.Fatalf("concurrent prune = %v", err)
		}
	}
	if successes != 1 || conflicts != 1 {
		t.Fatalf("concurrent prune successes=%d conflicts=%d", successes, conflicts)
	}
}

func TestPrunePlanAndVerifierBindTombstoneSemantics(t *testing.T) {
	config := testRuntimeConfig(t, testDirectory(t))
	runtime, err := Open(config)
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Close()
	target := publishPruneTarget(t, runtime, config.Domain, "event/mutation", "value")
	request := pruneTestRequest(
		t,
		config.Domain,
		"mutation-prune",
		2,
		nil,
		[]PruneTarget{target},
	)
	runtime.mu.RLock()
	intent, err := runtime.buildPruneIntentLocked(context.Background(), request)
	runtime.mu.RUnlock()
	if err != nil {
		t.Fatal(err)
	}
	digest, err := LogicalDigest(intent)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := buildPlan(intent, digest)
	if err != nil {
		t.Fatal(err)
	}
	deleteIndex := -1
	for index := range plan.Cells {
		if plan.Cells[index].Delete {
			deleteIndex = index
			break
		}
	}
	if deleteIndex < 0 {
		t.Fatal("prune plan has no tombstone")
	}
	broken := plan
	broken.Cells = append([]transaction.PhysicalCell(nil), plan.Cells...)
	broken.Cells[deleteIndex].Delete = false
	if _, err := broken.Validate(); !errors.Is(err, transaction.ErrInvalid) {
		t.Fatalf("delete-bit mutation = %v", err)
	}

	result, err := runtime.PruneCommitted(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	storedPlan, err := runtime.intents.Materialize(
		context.Background(),
		transaction.MaterializeRequest{
			TXN: result.TXN, LogicalDigest: result.LogicalDigest,
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	deleteCell := storedPlan.Cells[deleteIndex]
	mutation, err := cclient.NewMutation(deleteCell.Entry.Row)
	if err != nil {
		t.Fatal(err)
	}
	mutation.Put(
		deleteCell.Entry.ColumnFamily,
		deleteCell.Entry.ColumnQualifier,
		deleteCell.Visibility,
		int64(result.Epoch),
		nil,
	)
	if err := runtime.engine.Write(
		string(deleteCell.Entry.Table),
		[]*cclient.Mutation{mutation},
	); err != nil {
		t.Fatal(err)
	}
	if err := runtime.physical.Verify(
		context.Background(),
		result.Epoch,
		storedPlan.Cells,
	); !errors.Is(err, transaction.ErrInternal) {
		t.Fatalf("tombstone replaced by empty value = %v", err)
	}
}

func TestPruneCommittedRetiresOnlyOwningGuardsAcrossSharedPartition(t *testing.T) {
	config := testRuntimeConfig(t, testDirectory(t))
	runtime, err := Open(config)
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Close()
	stream := guard.Entity{
		Kind: 'E',
		ID:   coordination.EntityID("shared-event-stream"),
	}
	lpart, err := Partition(config.Domain, stream.ID)
	if err != nil {
		t.Fatal(err)
	}
	var streamHead *guard.Head
	targets := make([]PruneTarget, 0, 2)
	for index := 1; index <= 2; index++ {
		row := []byte(fmt.Sprintf("shared/%04d", index))
		eventEntity := guard.Entity{
			Kind: 'e',
			ID:   coordination.EntityID(fmt.Sprintf("event-%04d", index)),
		}
		streamMode := guard.ModeAbsentOrIdentical
		var expectedEpoch coordination.Epoch
		var expectedDigest coordination.Digest
		if streamHead != nil {
			streamMode = guard.ModeMutate
			expectedEpoch = streamHead.Epoch
			expectedDigest = streamHead.LogicalDigest
		}
		intent := Intent{
			Operation: []byte("shared-event-publish"),
			Token:     []byte(fmt.Sprintf("shared-event-%04d", index)),
			Cells: []Cell{{
				Table: "records", Row: row,
				Family: []byte("event"), Qualifier: []byte("record"),
				Value:          []byte(fmt.Sprintf("value-%04d", index)),
				EpochTimestamp: true, LPART: lpart, CopyGeneration: 1,
			}},
			Guards: []GuardIntent{
				{
					Entity: stream, Mode: streamMode,
					ExpectedEpoch: expectedEpoch, ExpectedDigest: expectedDigest,
					DesiredState:    guard.StateLive,
					DesiredWinnerID: []byte(fmt.Sprintf("%04d", index)),
					LPART:           lpart, LogicalPolicyID: []byte("events/shared"),
					RetirementGeneration: 1,
				},
				{
					Entity: eventEntity, Mode: guard.ModeAbsentOrIdentical,
					DesiredState:    guard.StateLive,
					DesiredWinnerID: []byte(fmt.Sprintf("%04d", index)),
					LPART:           lpart, LogicalPolicyID: []byte("events/shared"),
					RetirementGeneration: 1,
				},
			},
		}
		result, err := runtime.Publish(
			context.Background(),
			Request{Intent: intent},
		)
		if err != nil {
			t.Fatal(err)
		}
		streamHead, _, err = runtime.ReadEntity(context.Background(), stream)
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
		if err != nil || !found {
			t.Fatalf("shared event cell = %#v, %v, %v", cell, found, err)
		}
		targets = append(targets, PruneTarget{
			Table: "records", Cell: cell, Entity: eventEntity,
		})
	}
	result, err := runtime.PruneCommitted(
		context.Background(),
		pruneTestRequest(
			t,
			config.Domain,
			"shared-prune",
			3,
			nil,
			targets,
		),
	)
	if err != nil || result.Pruned != 2 {
		t.Fatalf("shared-partition prune = %#v, %v", result, err)
	}
	currentStream, pending, err := runtime.ReadEntity(context.Background(), stream)
	if err != nil || currentStream == nil ||
		currentStream.State != guard.StateLive ||
		currentStream.Epoch != streamHead.Epoch ||
		pending != nil && pending.Active {
		t.Fatalf("shared stream after prune = %#v, %#v, %v", currentStream, pending, err)
	}
	for _, target := range targets {
		head, pending, err := runtime.ReadEntity(context.Background(), target.Entity)
		if err != nil || head == nil ||
			head.State != guard.StateTombstone ||
			head.Epoch != result.Epoch ||
			pending != nil && pending.Active {
			t.Fatalf("retired shared event = %#v, %#v, %v", head, pending, err)
		}
	}
}

func publishPruneTarget(
	t *testing.T,
	runtime *Runtime,
	domain coordination.DomainID,
	row, value string,
) PruneTarget {
	t.Helper()
	intent := committedReadIntent(
		t,
		domain,
		"publish-"+row,
		[]byte(row),
		[]byte(value),
		guard.ModeAbsentOrIdentical,
		0,
		coordination.Digest{},
	)
	result, err := runtime.Publish(context.Background(), Request{Intent: intent})
	if err != nil {
		t.Fatal(err)
	}
	cell, found, err := runtime.ReadCommittedCell(
		context.Background(),
		"records",
		[]byte(row),
		[]byte("event"),
		[]byte("record"),
		nil,
		result.Epoch,
	)
	if err != nil || !found {
		t.Fatalf("published prune target = %#v, %v, %v", cell, found, err)
	}
	return PruneTarget{
		Table: "records",
		Cell:  cell,
		Entity: guard.Entity{
			Kind: 'E',
			ID:   append(coordination.EntityID(nil), []byte(row)...),
		},
	}
}

func pruneTestRequest(
	t *testing.T,
	domain coordination.DomainID,
	token string,
	floor uint64,
	head *guard.Head,
	targets []PruneTarget,
) PruneCommittedRequest {
	t.Helper()
	floorID := coordination.EntityID("event-readable-floor")
	lpart, err := Partition(domain, floorID)
	if err != nil {
		t.Fatal(err)
	}
	value := make([]byte, 8)
	binary.BigEndian.PutUint64(value, floor)
	mode := guard.ModeAbsentOrIdentical
	var expectedEpoch coordination.Epoch
	var expectedDigest coordination.Digest
	if head != nil {
		mode = guard.ModeMutate
		expectedEpoch = head.Epoch
		expectedDigest = head.LogicalDigest
	}
	return PruneCommittedRequest{
		Operation: []byte("fleet-event-prune-v1"),
		Token:     []byte(token),
		Targets:   targets,
		Checkpoint: PruneCheckpoint{
			Cell: Cell{
				Table: "records", Row: []byte("event-floor"),
				Family: []byte("meta"), Qualifier: []byte("floor"),
				Value: value, EpochTimestamp: true, LPART: lpart,
				CopyGeneration: 1,
			},
			Guard: GuardIntent{
				Entity: guard.Entity{Kind: 'F', ID: floorID},
				Mode:   mode, ExpectedEpoch: expectedEpoch,
				ExpectedDigest: expectedDigest, DesiredState: guard.StateLive,
				DesiredWinnerID: value, LPART: lpart,
				LogicalPolicyID:      []byte("fleet-events/retention"),
				RetirementGeneration: 1,
			},
		},
		Results: []ResultIdentity{{
			Kind: []byte("fleet-event-prune-v1"),
			ID:   value,
		}},
	}
}
