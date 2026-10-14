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

package embedstore_test

import (
	"context"
	"testing"

	"github.com/phrocker/shoal-oss/internal/embedpb"
	"github.com/phrocker/shoal-oss/internal/embedstore"
	"github.com/phrocker/shoal-oss/internal/engine"
)

// TestScanAsOf reconstructs a single coordinate's history through the public
// embedstore Scan path: write v10 (ts=10), overwrite v30 (ts=30), delete
// (ts=40). A scan with ScanRequest.AsOf must return the state at that instant.
func TestScanAsOf(t *testing.T) {
	ctx := context.Background()
	eng, err := engine.Open(t.TempDir(), engine.Options{})
	if err != nil {
		t.Fatalf("open engine: %v", err)
	}
	defer eng.Close()
	store := embedstore.New(eng)

	const table = "tt"
	if err := store.CreateTable(ctx, table, []string{"k"}); err != nil {
		t.Fatalf("create table: %v", err)
	}

	write := func(ts int64, val string, del bool) {
		e := &embedpb.Entry{ColumnFamily: []byte("cf"), ColumnQualifier: []byte("a"), Timestamp: ts, Delete: del}
		if !del {
			e.Value = []byte(val)
		}
		if err := store.Write(ctx, table, []*embedpb.Mutation{{Row: []byte("k:row"), Entries: []*embedpb.Entry{e}}}); err != nil {
			t.Fatalf("write ts=%d: %v", ts, err)
		}
	}
	write(10, "v10", false)
	write(30, "v30", false)
	write(40, "", true)
	if err := store.Flush(ctx, table); err != nil {
		t.Fatalf("flush: %v", err)
	}

	scan := func(asOf int64) []*embedpb.Cell {
		cells, err := store.Scan(ctx, table, &embedpb.ScanRequest{RowPrefix: "k:", AsOf: asOf})
		if err != nil {
			t.Fatalf("scan as-of %d: %v", asOf, err)
		}
		return cells
	}

	// Live scan (no ceiling): the delete suppresses everything.
	if got := scan(0); len(got) != 0 {
		t.Errorf("live scan: expected coordinate deleted, got %d cells", len(got))
	}
	// As-of 15: only the original write existed.
	if got := scan(15); len(got) != 1 || string(got[0].Value) != "v10" {
		t.Errorf("as-of 15: got %v want [v10]", cellValues(got))
	}
	// As-of 35: the overwrite is current, the delete has not happened.
	if got := scan(35); len(got) != 1 || string(got[0].Value) != "v30" {
		t.Errorf("as-of 35: got %v want [v30]", cellValues(got))
	}
	// As-of 50: the delete (ts=40) is in effect.
	if got := scan(50); len(got) != 0 {
		t.Errorf("as-of 50: expected coordinate deleted, got %v", cellValues(got))
	}
}

func cellValues(cells []*embedpb.Cell) []string {
	out := make([]string, len(cells))
	for i, c := range cells {
		out[i] = string(c.Value)
	}
	return out
}
