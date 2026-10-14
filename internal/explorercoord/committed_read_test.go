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
	"testing"

	"github.com/phrocker/shoal-oss/internal/cclient"
	"github.com/phrocker/shoal-oss/pkg/explorer/coordination"
	"github.com/phrocker/shoal-oss/pkg/explorer/coordination/guard"
	"github.com/phrocker/shoal-oss/pkg/explorer/coordination/transaction"
)

func TestScanCommittedPinsFrontierAndSkipsPoisonedNewerVersion(t *testing.T) {
	config := testRuntimeConfig(t, testDirectory(t))
	armed := false
	config.testStageHook = func(stage recoveryStage) error {
		if armed && stage == recoveryStagePhysical {
			armed = false
			return context.Canceled
		}
		return nil
	}
	runtime, err := Open(config)
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Close()

	first := committedReadIntent(
		t, config.Domain, "event-a-1", []byte("event/a"), []byte("one"),
		guard.ModeAbsentOrIdentical, 0, coordination.Digest{},
	)
	firstResult, err := runtime.Publish(context.Background(), Request{Intent: first})
	if err != nil {
		t.Fatal(err)
	}
	second := committedReadIntent(
		t, config.Domain, "event-a-2", []byte("event/a"), []byte("two"),
		guard.ModeMutate, firstResult.Epoch, firstResult.LogicalDigest,
	)
	secondResult, err := runtime.Publish(context.Background(), Request{Intent: second})
	if err != nil {
		t.Fatal(err)
	}
	atFirst, err := runtime.ScanCommitted(context.Background(), CommittedScanRequest{
		Table: "records", RowPrefix: []byte("event/"), Family: []byte("event"),
		Qualifier: []byte("record"), Frontier: firstResult.Epoch, Limit: 10,
	})
	if err != nil || len(atFirst.Cells) != 1 ||
		!bytes.Equal(atFirst.Cells[0].Cell.Value, []byte("one")) {
		t.Fatalf("first frontier page = %#v, %v", atFirst, err)
	}

	third := committedReadIntent(
		t, config.Domain, "event-a-3", []byte("event/a"), []byte("three"),
		guard.ModeMutate, secondResult.Epoch, secondResult.LogicalDigest,
	)
	armed = true
	if _, err := runtime.Publish(context.Background(), Request{Intent: third}); err == nil ||
		!errors.Is(err, ErrIndeterminatePublication) {
		t.Fatalf("staged third publication = %v", err)
	}
	current, err := runtime.ScanCommitted(context.Background(), CommittedScanRequest{
		Table: "records", RowPrefix: []byte("event/"), Family: []byte("event"),
		Qualifier: []byte("record"), Limit: 10,
	})
	if err != nil || current.Frontier != secondResult.Epoch ||
		len(current.Cells) != 1 ||
		!bytes.Equal(current.Cells[0].Cell.Value, []byte("two")) {
		t.Fatalf("page with nonterminal newer version = %#v, %v", current, err)
	}

	txn, err := DeriveTXN(config.Domain, third.Operation, third.Token)
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := runtime.Inspect(context.Background(), txn)
	if err != nil {
		t.Fatal(err)
	}
	corrupt, err := cclient.NewMutation([]byte("event/a"))
	if err != nil {
		t.Fatal(err)
	}
	corrupt.Put(
		[]byte("event"), []byte("record"), nil,
		int64(snapshot.Root.Epoch), []byte("corrupt"),
	)
	if err := runtime.engine.Write("records", []*cclient.Mutation{corrupt}); err != nil {
		t.Fatal(err)
	}
	if err := runtime.Recover(context.Background()); err != nil {
		t.Fatalf("settle poisoned third publication = %v", err)
	}
	if _, err := runtime.allocator.AdvanceFrontier(context.Background()); err != nil {
		t.Fatalf("advance across poisoned outcome: %v", err)
	}
	afterPoison, err := runtime.ScanCommitted(context.Background(), CommittedScanRequest{
		Table: "records", RowPrefix: []byte("event/"), Family: []byte("event"),
		Qualifier: []byte("record"), Limit: 10,
	})
	if err != nil || afterPoison.Frontier != snapshot.Root.Epoch ||
		len(afterPoison.Cells) != 1 ||
		!bytes.Equal(afterPoison.Cells[0].Cell.Value, []byte("two")) {
		t.Fatalf("page after poisoned outcome = %#v, %v", afterPoison, err)
	}

	committed, ok, err := runtime.Committed(
		context.Background(), secondResult.TXN, secondResult.LogicalDigest,
	)
	if err != nil || !ok || committed.Epoch != secondResult.Epoch {
		t.Fatalf("committed lookup = %#v, %v, %v", committed, ok, err)
	}
	if _, _, err := runtime.Committed(
		context.Background(), secondResult.TXN, coordination.Sum([]byte("wrong")),
	); !errors.Is(err, transaction.ErrConflict) {
		t.Fatalf("wrong digest committed lookup = %v", err)
	}
}

func TestScanCommittedPagesBoundsAndDetectsPhysicalMutation(t *testing.T) {
	config := testRuntimeConfig(t, testDirectory(t))
	runtime, err := Open(config)
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Close()
	for _, row := range [][]byte{[]byte("event/a"), []byte("event/b")} {
		intent := committedReadIntent(
			t, config.Domain, string(row), row, append([]byte("value-"), row...),
			guard.ModeAbsentOrIdentical, 0, coordination.Digest{},
		)
		if _, err := runtime.Publish(context.Background(), Request{Intent: intent}); err != nil {
			t.Fatal(err)
		}
	}

	first, err := runtime.ScanCommitted(context.Background(), CommittedScanRequest{
		Table: "records", RowPrefix: []byte("event/"), Family: []byte("event"),
		Qualifier: []byte("record"), Limit: 1,
	})
	if err != nil || len(first.Cells) != 1 || len(first.NextRow) == 0 ||
		!bytes.Equal(first.Cells[0].Cell.Coordinate.Row, []byte("event/a")) {
		t.Fatalf("first committed page = %#v, %v", first, err)
	}
	second, err := runtime.ScanCommitted(context.Background(), CommittedScanRequest{
		Table: "records", RowPrefix: []byte("event/"), StartAfterRow: first.NextRow,
		Family: []byte("event"), Qualifier: []byte("record"),
		Frontier: first.Frontier, Limit: 1,
	})
	if err != nil || len(second.Cells) != 1 || len(second.NextRow) != 0 ||
		second.HistoryFloor != 1 ||
		!bytes.Equal(second.Cells[0].Cell.Coordinate.Row, []byte("event/b")) {
		t.Fatalf("second committed page = %#v, %v", second, err)
	}
	exact, found, err := runtime.ReadCommittedCell(
		context.Background(), "records", []byte("event/b"),
		[]byte("event"), []byte("record"), nil, second.Cells[0].Epoch,
	)
	if err != nil || !found ||
		!bytes.Equal(exact.Cell.Value, second.Cells[0].Cell.Value) {
		t.Fatalf("exact committed cell = %#v, %v, %v", exact, found, err)
	}
	if _, found, err := runtime.ReadCommittedCell(
		context.Background(), "records", []byte("event/b"),
		[]byte("event"), []byte("record"), nil, second.Cells[0].Epoch+1,
	); err == nil || found {
		t.Fatalf("unavailable future epoch = found %v, err %v", found, err)
	}
	if _, err := runtime.ScanCommitted(context.Background(), CommittedScanRequest{
		Table: "unconfigured", RowPrefix: []byte("event/"),
		Family: []byte("event"), Qualifier: []byte("record"), Limit: 1,
	}); !errors.Is(err, transaction.ErrInvalid) {
		t.Fatalf("unconfigured table scan = %v", err)
	}
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := runtime.ScanCommitted(canceled, CommittedScanRequest{
		Table: "records", RowPrefix: []byte("event/"),
		Family: []byte("event"), Qualifier: []byte("record"), Limit: 1,
	}); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled committed scan = %v", err)
	}

	for index := 0; index < 3; index++ {
		mutation, err := cclient.NewMutation([]byte{'x', byte('a' + index)})
		if err != nil {
			t.Fatal(err)
		}
		mutation.Put([]byte("event"), []byte("other"), nil, 1, []byte("noise"))
		if err := runtime.engine.Write("records", []*cclient.Mutation{mutation}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := runtime.ScanCommitted(context.Background(), CommittedScanRequest{
		Table: "records", RowPrefix: []byte("x"), Family: []byte("event"),
		Qualifier: []byte("record"), Limit: 1, MaxScanned: 1,
	}); !errors.Is(err, transaction.ErrUnavailable) {
		t.Fatalf("scan work exhaustion = %v", err)
	}

	epoch := first.Cells[0].Epoch
	divergent, err := cclient.NewMutation(first.Cells[0].Cell.Coordinate.Row)
	if err != nil {
		t.Fatal(err)
	}
	divergent.Put(
		first.Cells[0].Cell.Coordinate.Family,
		first.Cells[0].Cell.Coordinate.Qualifier,
		first.Cells[0].Cell.Coordinate.Visibility,
		int64(epoch),
		[]byte("divergent"),
	)
	if err := runtime.engine.Write("records", []*cclient.Mutation{divergent}); err != nil {
		t.Fatal(err)
	}
	if _, err := runtime.ScanCommitted(context.Background(), CommittedScanRequest{
		Table: "records", RowPrefix: []byte("event/a"), Family: []byte("event"),
		Qualifier: []byte("record"), Frontier: first.Frontier, Limit: 1,
	}); !errors.Is(err, transaction.ErrInternal) {
		t.Fatalf("divergent physical mutation scan = %v", err)
	}
}

func TestScanCommittedFinalizesMatchingRowBeforeNonmatchingWork(t *testing.T) {
	config := testRuntimeConfig(t, testDirectory(t))
	runtime, err := Open(config)
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Close()
	intent := committedReadIntent(
		t, config.Domain, "matching", []byte("event/a"), []byte("value"),
		guard.ModeAbsentOrIdentical, 0, coordination.Digest{},
	)
	if _, err := runtime.Publish(
		context.Background(), Request{Intent: intent},
	); err != nil {
		t.Fatal(err)
	}
	noise, err := cclient.NewMutation([]byte("event/b"))
	if err != nil {
		t.Fatal(err)
	}
	noise.Put([]byte("event"), []byte("other"), nil, 1, []byte("noise"))
	if err := runtime.engine.Write("records", []*cclient.Mutation{noise}); err != nil {
		t.Fatal(err)
	}
	page, err := runtime.ScanCommitted(context.Background(), CommittedScanRequest{
		Table: "records", RowPrefix: []byte("event/"),
		Family: []byte("event"), Qualifier: []byte("record"),
		Limit: 1, MaxScanned: 1,
	})
	if err != nil || len(page.Cells) != 1 ||
		!bytes.Equal(page.Cells[0].Cell.Coordinate.Row, []byte("event/a")) {
		t.Fatalf("page before nonmatching work = %#v, %v", page, err)
	}
}

func committedReadIntent(
	t *testing.T,
	domain coordination.DomainID,
	token string,
	row, value []byte,
	mode guard.Mode,
	expectedEpoch coordination.Epoch,
	expectedDigest coordination.Digest,
) Intent {
	t.Helper()
	lpart, err := Partition(domain, row)
	if err != nil {
		t.Fatal(err)
	}
	return Intent{
		Operation: []byte("event-record-v1"), Token: []byte(token),
		Cells: []Cell{{
			Table: "records", Row: append([]byte(nil), row...),
			Family: []byte("event"), Qualifier: []byte("record"),
			Value: append([]byte(nil), value...), EpochTimestamp: true,
			LPART: lpart, CopyGeneration: 1,
		}},
		Guards: []GuardIntent{{
			Entity: guard.Entity{
				Kind: 'E', ID: append(coordination.EntityID(nil), row...),
			},
			Mode: mode, ExpectedEpoch: expectedEpoch, ExpectedDigest: expectedDigest,
			DesiredState: guard.StateLive, DesiredWinnerID: append([]byte(nil), value...),
			LPART: lpart, LogicalPolicyID: []byte("event/default"),
			RetirementGeneration: 1,
		}},
		Results: []ResultIdentity{{
			Kind: []byte("event"), ID: append([]byte(nil), row...),
		}},
	}
}
