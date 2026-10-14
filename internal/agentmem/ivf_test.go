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
	"strconv"
	"testing"

	"github.com/phrocker/shoal-oss/internal/embeddingspace"
	"github.com/phrocker/shoal-oss/internal/embedpb"
	"github.com/phrocker/shoal-oss/internal/ivfpq"
)

// seedIvfIndex trains an IVF-PQ index over vecs and writes the coded cells +
// codebook blobs into store exactly as cmd/shoal-ivf-train does, returning the
// source row id assigned to each input vector (index-aligned with vecs).
func seedIvfIndex(t *testing.T, store EmbedStore, table string, vecs [][]float32, m, ks, nlist int, version int32) []string {
	t.Helper()
	ctx := context.Background()

	cent, err := ivfpq.TrainCentroids(vecs, nlist, 25, 42, version)
	if err != nil {
		t.Fatalf("TrainCentroids: %v", err)
	}
	pq, err := ivfpq.TrainPQ(vecs, m, ks, 25, 42, version)
	if err != nil {
		t.Fatalf("TrainPQ: %v", err)
	}

	ivfTable := ivfpq.IvfTableName(table)
	cfgTable := ivfpq.ConfigTableName(table)
	if err := store.CreateTable(ctx, ivfTable, nil); err != nil {
		t.Fatalf("CreateTable ivf: %v", err)
	}
	if err := store.CreateTable(ctx, cfgTable, nil); err != nil {
		t.Fatalf("CreateTable cfg: %v", err)
	}

	rows := make([]string, len(vecs))
	var muts []*embedpb.Mutation
	for i, v := range vecs {
		vid := "evt:" + strconv.Itoa(i)
		rows[i] = vid
		norm := append([]float32(nil), v...)
		normalize(norm)
		cid := cent.Assign(norm)
		code, err := pq.Encode(v)
		if err != nil {
			t.Fatalf("Encode: %v", err)
		}
		muts = append(muts, &embedpb.Mutation{
			Row: []byte(ivfpq.RowKey(cid, vid)),
			Entries: []*embedpb.Entry{
				{ColumnFamily: []byte(ivfpq.ColFam), ColumnQualifier: []byte(ivfpq.QualPQCode), Value: code},
				{ColumnFamily: []byte(ivfpq.ColFam), ColumnQualifier: []byte(ivfpq.QualCodebookVersion), Value: []byte(strconv.Itoa(int(version)))},
			},
		})
	}
	if err := store.Write(ctx, ivfTable, muts); err != nil {
		t.Fatalf("write coded vectors: %v", err)
	}

	pqBlob, err := pq.Bytes()
	if err != nil {
		t.Fatalf("pq.Bytes: %v", err)
	}
	centBlob, err := cent.Bytes()
	if err != nil {
		t.Fatalf("cent.Bytes: %v", err)
	}
	cfgMut := func(row string, value []byte) *embedpb.Mutation {
		return &embedpb.Mutation{Row: []byte(row), Entries: []*embedpb.Entry{
			{ColumnFamily: []byte(ivfpq.ConfigColFam), ColumnQualifier: []byte(ivfpq.ConfigQual), Value: value},
		}}
	}
	cfgMuts := []*embedpb.Mutation{
		cfgMut(ivfpq.PQRow(version), pqBlob),
		cfgMut(ivfpq.CentroidsRow(version), centBlob),
		cfgMut(ivfpq.EmbeddingSpaceRow(version), []byte(
			testIvfEmbeddingSpace(t, len(vecs[0])))),
		cfgMut(ivfpq.ConfigRowActiveVersion, []byte(strconv.Itoa(int(version)))),
		cfgMut(ivfpq.ConfigRowLastTrainedRows, []byte(strconv.Itoa(len(vecs)))),
	}
	if err := store.Write(ctx, cfgTable, cfgMuts); err != nil {
		t.Fatalf("write config: %v", err)
	}
	return rows
}

func testIvfEmbeddingSpace(t *testing.T, dimensions int) string {
	t.Helper()
	identity, err := (FakeEmbedder{Dim: dimensions}).EmbeddingSpaceIdentity()
	if err != nil {
		t.Fatal(err)
	}
	return identity
}

func TestIvfIndex_LoadAndSearch(t *testing.T) {
	ctx := context.Background()
	store := NewFakeStore()
	const table = "graph"

	// Six well-separated 4-dim vectors. dim%m==0 (4%2), ks==len so PQ is
	// near-lossless and the query's own vector must rank first.
	vecs := [][]float32{
		{1, 0, 0, 0},
		{0, 1, 0, 0},
		{0, 0, 1, 0},
		{0, 0, 0, 1},
		{1, 1, 0, 0},
		{0, 0, 1, 1},
	}
	rows := seedIvfIndex(t, store, table, vecs, 2, 6, 2, 1)

	space := testIvfEmbeddingSpace(t, len(vecs[0]))
	ix, err := LoadIvfIndexInSpace(ctx, store, table, space)
	if err != nil {
		t.Fatalf("LoadIvfIndex: %v", err)
	}
	if ix.Version() != 1 {
		t.Fatalf("version = %d, want 1", ix.Version())
	}
	if ix.EmbeddingSpace() != space {
		t.Fatalf("embedding space = %q, want %q", ix.EmbeddingSpace(), space)
	}
	if _, err := ix.Search(
		ctx, vecs[0], 3, 2,
	); !errors.Is(err, embeddingspace.ErrQueryIdentityRequired) {
		t.Fatalf("legacy Search error = %v", err)
	}
	if _, err := LoadIvfIndexInSpace(
		ctx, store, table, "foreign-space",
	); !errors.Is(err, embeddingspace.ErrMismatch) {
		t.Fatalf("foreign LoadIvfIndexInSpace error = %v", err)
	}
	if err := store.Write(ctx, ivfpq.IvfTableName(table), []*embedpb.Mutation{{
		Row: []byte(ivfpq.RowKey(0, "evt:stale-only")),
		Entries: []*embedpb.Entry{
			{
				ColumnFamily:    []byte(ivfpq.ColFam),
				ColumnQualifier: []byte(ivfpq.QualPQCode),
				Value:           []byte{0, 0},
			},
			{
				ColumnFamily:    []byte(ivfpq.ColFam),
				ColumnQualifier: []byte(ivfpq.QualCodebookVersion),
				Value:           []byte("0"),
			},
		},
	}}); err != nil {
		t.Fatal(err)
	}
	hits, err := ix.SearchInSpace(ctx, vecs[0], space, 100, 2)
	if err != nil {
		t.Fatal(err)
	}
	for _, hit := range hits {
		if hit.Row == "evt:stale-only" {
			t.Fatal("stale codebook posting was scored")
		}
	}

	// Probe all clusters (full recall) and confirm each vector retrieves
	// itself as the top hit.
	for i, q := range vecs {
		got, err := ix.SearchInSpace(ctx, q, space, 3, 2)
		if err != nil {
			t.Fatalf("Search[%d]: %v", i, err)
		}
		if len(got) == 0 {
			t.Fatalf("Search[%d]: no results", i)
		}
		if got[0].Row != rows[i] {
			t.Errorf("Search[%d] top row = %q, want %q (results=%v)", i, got[0].Row, rows[i], got)
		}
	}
}

func TestLoadIvfIndex_Untrained(t *testing.T) {
	store := NewFakeStore()
	if _, err := LoadIvfIndex(context.Background(), store, "graph"); err == nil {
		t.Fatal("LoadIvfIndex on an untrained table: want error, got nil")
	}
}

func TestLoadIvfIndexRejectsLegacyIdentitylessArtifact(t *testing.T) {
	store := NewFakeStore()
	const table = "graph"
	vecs := [][]float32{
		{1, 0, 0, 0}, {0, 1, 0, 0}, {0, 0, 1, 0},
		{0, 0, 0, 1}, {1, 1, 0, 0}, {0, 0, 1, 1},
	}
	seedIvfIndex(t, store, table, vecs, 2, 6, 2, 1)
	store.mu.Lock()
	delete(
		store.tables[ivfpq.ConfigTableName(table)],
		ivfpq.EmbeddingSpaceRow(1),
	)
	store.mu.Unlock()
	if _, err := LoadIvfIndex(
		context.Background(), store, table,
	); !errors.Is(err, embeddingspace.ErrQueryMetadataMissing) {
		t.Fatalf("legacy load error = %v", err)
	}
}
