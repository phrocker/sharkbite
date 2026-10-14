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

package agentmem

import (
	"context"
	"errors"
	"testing"

	"github.com/phrocker/shoal-oss/internal/embeddingspace"
	"github.com/phrocker/shoal-oss/internal/embedpb"
	"github.com/phrocker/shoal-oss/internal/ivfpq"
)

// TestIvfIndex_Add verifies an incrementally added vector becomes searchable
// through the IVF path without any retrain.
func TestIvfIndex_Add(t *testing.T) {
	ctx := context.Background()
	store := NewFakeStore()
	const table = "graph"

	vecs := [][]float32{
		{1, 0, 0, 0},
		{0, 1, 0, 0},
		{0, 0, 1, 0},
		{0, 0, 0, 1},
		{1, 1, 0, 0},
		{0, 0, 1, 1},
	}
	seedIvfIndex(t, store, table, vecs, 2, 6, 2, 1)

	space := testIvfEmbeddingSpace(t, len(vecs[0]))
	ix, err := LoadIvfIndexInSpace(ctx, store, table, space)
	if err != nil {
		t.Fatalf("LoadIvfIndex: %v", err)
	}

	// Not present before Add.
	const newID = "evt:fresh"
	if found := searchHasRow(t, ctx, ix, []float32{1, 0, 0, 0}, newID); found {
		t.Fatalf("row %q present before Add", newID)
	}

	// Add a vector identical to vecs[0]; PQ encodes losslessly here (ks==n) so
	// it must rank alongside the original.
	if err := ix.AddInSpace(
		ctx, newID, []float32{1, 0, 0, 0}, space); err != nil {
		t.Fatalf("Add: %v", err)
	}
	if err := ix.Add(
		ctx, "evt:legacy", []float32{1, 0, 0, 0},
	); !errors.Is(err, embeddingspace.ErrQueryIdentityRequired) {
		t.Fatalf("legacy Add error = %v", err)
	}
	if err := ix.AddInSpace(
		ctx, "evt:foreign", []float32{1, 0, 0, 0}, "foreign-space",
	); !errors.Is(err, embeddingspace.ErrMismatch) {
		t.Fatalf("foreign AddInSpace error = %v", err)
	}
	if found := searchHasRow(t, ctx, ix, []float32{1, 0, 0, 0}, newID); !found {
		t.Fatalf("row %q not searchable after Add", newID)
	}

	// Dim mismatch is rejected.
	if err := ix.AddInSpace(
		ctx, "evt:bad", []float32{1, 0, 0}, space); err == nil {
		t.Error("Add with wrong dim: want error, got nil")
	}
	if err := ix.AddInSpace(
		ctx, "", []float32{1, 0, 0, 0}, space); err == nil {
		t.Error("Add with empty vertexID: want error, got nil")
	}
}

func searchHasRow(t *testing.T, ctx context.Context, ix *IvfIndex, q []float32, row string) bool {
	t.Helper()
	hits, err := ix.SearchInSpace(ctx, q, ix.EmbeddingSpace(), 10, 2)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	for _, h := range hits {
		if h.Row == row {
			return true
		}
	}
	return false
}

// TestIngest_IvfFreshness verifies that, with IvfFreshness enabled and a trained
// index present, Ingest writes an IVF posting for the new memory so it is
// indexed immediately; with the flag off, no posting is written.
func TestIngest_IvfFreshness(t *testing.T) {
	ctx := context.Background()
	const table = "graph"
	emb := FakeEmbedder{Dim: 16}

	seedTexts := []string{"alpha event", "beta event", "gamma event", "delta event", "epsilon event", "zeta event"}
	vecs := make([][]float32, len(seedTexts))
	for i, txt := range seedTexts {
		v, err := emb.Embed(ctx, txt)
		if err != nil {
			t.Fatalf("embed: %v", err)
		}
		vecs[i] = v
	}

	run := func(freshness bool) (string, bool) {
		store := NewFakeStore()
		seedIvfIndex(t, store, table, vecs, 8, 4, 2, 1)
		c, err := New(Config{Store: store, Table: table, Embedder: emb, IvfFreshness: freshness})
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		if err := c.EnsureTable(ctx); err != nil {
			t.Fatalf("EnsureTable: %v", err)
		}
		res, err := c.Ingest(ctx, IngestRequest{Text: "a brand new memory"})
		if err != nil {
			t.Fatalf("Ingest: %v", err)
		}
		return res.Row, ivfPostingExists(t, ctx, store, table, res.Row)
	}

	if row, present := run(true); !present {
		t.Errorf("freshness on: expected IVF posting for %q, found none", row)
	}
	if row, present := run(false); present {
		t.Errorf("freshness off: expected no IVF posting for %q, found one", row)
	}
}

func ivfPostingExists(t *testing.T, ctx context.Context, store EmbedStore, table, vertexID string) bool {
	t.Helper()
	cells, err := store.Scan(ctx, ivfpq.IvfTableName(table), &embedpb.ScanRequest{})
	if err != nil {
		t.Fatalf("scan ivf table: %v", err)
	}
	for _, cell := range cells {
		if string(cell.ColumnFamily) != ivfpq.ColFam || string(cell.ColumnQualifier) != ivfpq.QualPQCode {
			continue
		}
		if ivfpq.ExtractVertexID(string(cell.Row)) == vertexID {
			return true
		}
	}
	return false
}
