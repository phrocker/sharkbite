// Licensed to the Apache Software Foundation (ASF) under one
// or more contributor license agreements.  See the NOTICE file
// distributed with this work for additional information
// regarding copyright ownership.  The ASF licenses this file
// to you under the Apache License, Version 2.0 (the
// "License"); you may not use this file except in compliance
// with the License.  You may obtain a copy of the License at
//
//   https://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing,
// software distributed under the License is distributed on an
// "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY
// KIND, either express or implied.  See the License for the
// specific language governing permissions and limitations
// under the License.

package checkpoint_test

import (
	"context"
	"testing"

	"github.com/phrocker/shoal-oss/internal/checkpoint"
	"github.com/phrocker/shoal-oss/internal/embedpb"
	"github.com/phrocker/shoal-oss/internal/embedstore"
	"github.com/phrocker/shoal-oss/internal/engine"
)

func newStore(t *testing.T) (*embedstore.EngineStore, string) {
	t.Helper()
	eng, err := engine.Open(t.TempDir(), engine.Options{})
	if err != nil {
		t.Fatalf("open engine: %v", err)
	}
	t.Cleanup(func() { eng.Close() })
	store := embedstore.New(eng)
	const table = "graph"
	if err := store.CreateTable(context.Background(), table, []string{"ckpt:", "d:"}); err != nil {
		t.Fatalf("create table: %v", err)
	}
	return store, table
}

func writeData(t *testing.T, store *embedstore.EngineStore, table string, ts int64, val string) {
	t.Helper()
	err := store.Write(context.Background(), table, []*embedpb.Mutation{{
		Row:     []byte("d:row"),
		Entries: []*embedpb.Entry{{ColumnFamily: []byte("v:"), ColumnQualifier: []byte("x"), Timestamp: ts, Value: []byte(val)}},
	}})
	if err != nil {
		t.Fatalf("write data ts=%d: %v", ts, err)
	}
}

func TestSaveGetListDelete(t *testing.T) {
	ctx := context.Background()
	store, table := newStore(t)
	m := checkpoint.NewManager(store, table)

	if _, err := m.Save(ctx, "", 100, 1); err == nil {
		t.Error("empty label should error")
	}
	if _, err := m.Save(ctx, "bad", 0, 1); err == nil {
		t.Error("non-positive watermark should error")
	}

	if _, err := m.Save(ctx, "before-deploy", 100, 1700); err != nil {
		t.Fatalf("save: %v", err)
	}
	if _, err := m.Save(ctx, "after-deploy", 200, 1800); err != nil {
		t.Fatalf("save: %v", err)
	}
	if err := store.Flush(ctx, table); err != nil {
		t.Fatalf("flush: %v", err)
	}

	cp, ok, err := m.Get(ctx, "before-deploy")
	if err != nil || !ok {
		t.Fatalf("get: ok=%v err=%v", ok, err)
	}
	if cp.Timestamp != 100 || cp.CreatedAt != 1700 {
		t.Errorf("get: %+v want ts=100 created=1700", cp)
	}
	if _, ok, _ := m.Get(ctx, "missing"); ok {
		t.Error("missing label should not be found")
	}

	list, err := m.List(ctx)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(list) != 2 || list[0].Label != "after-deploy" || list[1].Label != "before-deploy" {
		t.Fatalf("list ordering: %+v", list)
	}

	if err := m.Delete(ctx, "before-deploy"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if err := store.Flush(ctx, table); err != nil {
		t.Fatalf("flush: %v", err)
	}
	if _, ok, _ := m.Get(ctx, "before-deploy"); ok {
		t.Error("deleted checkpoint should be gone")
	}
	if list, _ := m.List(ctx); len(list) != 1 {
		t.Errorf("after delete, list should have 1, got %d", len(list))
	}
}

// TestScanAsOfReconstructsHistory binds checkpoints to data timestamps and
// proves ScanAsOf reads the table as it stood at each checkpoint.
func TestScanAsOfReconstructsHistory(t *testing.T) {
	ctx := context.Background()
	store, table := newStore(t)
	m := checkpoint.NewManager(store, table)

	writeData(t, store, table, 100, "first")
	if _, err := m.Save(ctx, "v1", 150, 1); err != nil {
		t.Fatalf("save v1: %v", err)
	}
	writeData(t, store, table, 200, "second")
	if _, err := m.Save(ctx, "v2", 250, 1); err != nil {
		t.Fatalf("save v2: %v", err)
	}
	if err := store.Flush(ctx, table); err != nil {
		t.Fatalf("flush: %v", err)
	}

	at := func(label string) string {
		cells, err := m.ScanAsOf(ctx, label, &embedpb.ScanRequest{RowPrefix: "d:"})
		if err != nil {
			t.Fatalf("scan as-of %s: %v", label, err)
		}
		if len(cells) != 1 {
			t.Fatalf("as-of %s: expected 1 cell, got %d", label, len(cells))
		}
		return string(cells[0].Value)
	}
	if got := at("v1"); got != "first" {
		t.Errorf("as-of v1: got %q want first", got)
	}
	if got := at("v2"); got != "second" {
		t.Errorf("as-of v2: got %q want second", got)
	}
	if _, err := m.ScanAsOf(ctx, "nope", &embedpb.ScanRequest{RowPrefix: "d:"}); err == nil {
		t.Error("unknown label should error")
	}
}
