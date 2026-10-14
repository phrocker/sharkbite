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
	"testing"

	"github.com/phrocker/shoal-oss/internal/embedpb"
	"github.com/phrocker/shoal-oss/internal/graphschema"
)

// seedBruteForceVectors writes one full-precision vec: cell per source row into
// the graph table so the brute-force VectorSearch anchor path has data, mirroring
// the rows seeded by seedIvfIndex (evt:0..evt:N-1).
func seedBruteForceVectors(t *testing.T, store EmbedStore, table string, vecs [][]float32) {
	t.Helper()
	if err := store.CreateTable(context.Background(), table, nil); err != nil {
		t.Fatalf("CreateTable %s: %v", table, err)
	}
	var muts []*embedpb.Mutation
	for i, v := range vecs {
		vid := "evt:" + itoa(i)
		muts = append(muts, &embedpb.Mutation{
			Row: []byte(vid),
			Entries: []*embedpb.Entry{
				{ColumnFamily: []byte(graphschema.VectorCF()), ColumnQualifier: []byte("0"), Value: PackVector(v)},
			},
		})
	}
	if err := store.Write(context.Background(), table, muts); err != nil {
		t.Fatalf("write vec cells: %v", err)
	}
}

func itoa(i int) string {
	const digits = "0123456789"
	if i == 0 {
		return "0"
	}
	var b []byte
	for i > 0 {
		b = append([]byte{digits[i%10]}, b...)
		i /= 10
	}
	return string(b)
}

var optinVecs = [][]float32{
	{1, 0, 0, 0},
	{0, 1, 0, 0},
	{0, 0, 1, 0},
	{0, 0, 0, 1},
	{1, 1, 0, 0},
	{0, 0, 1, 1},
}

// Identity-bearing IVF artifacts serve the explicit UseIVF path.
func TestSemanticAnchors_IVFEnabled(t *testing.T) {
	ctx := context.Background()
	store := NewFakeStore()
	const table = "graph"
	_ = seedIvfIndex(t, store, table, optinVecs, 2, 6, 2, 1)
	seedBruteForceVectors(t, store, table, optinVecs)

	c, err := New(Config{
		Store: store, Table: table, Embedder: FakeEmbedder{Dim: 4},
		UseIVF: true,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	for i := range optinVecs {
		got, err := c.semanticAnchors(ctx, analysis{vector: optinVecs[i]})
		if err != nil {
			t.Fatalf("semanticAnchors[%d]: %v", i, err)
		}
		if len(got) == 0 {
			t.Errorf("semanticAnchors[%d] = %v, want IVF result", i, got)
		}
	}
	if c.ivf == nil || c.ivfErr != nil {
		t.Fatalf("UseIVF did not load identity-checked index: index=%v err=%v",
			c.ivf, c.ivfErr)
	}
	if c.cfg.IvfNprobe != 8 {
		t.Fatalf("default IvfNprobe = %d, want 8", c.cfg.IvfNprobe)
	}
}

// With UseIVF enabled but no index trained, semanticAnchors degrades gracefully
// to the brute-force VectorSearch path instead of erroring.
func TestSemanticAnchors_IVFFallsBackWhenUntrained(t *testing.T) {
	ctx := context.Background()
	store := NewFakeStore()
	const table = "graph"
	seedBruteForceVectors(t, store, table, optinVecs)

	c, err := New(Config{
		Store: store, Table: table, Embedder: FakeEmbedder{Dim: 4},
		UseIVF: true,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	got, err := c.semanticAnchors(ctx, analysis{vector: optinVecs[0]})
	if err != nil {
		t.Fatalf("semanticAnchors: %v", err)
	}
	if len(got) == 0 || got[0] != "evt:0" {
		t.Errorf("fallback semanticAnchors = %v, want first=evt:0", got)
	}
}

// The default config (UseIVF=false) uses the brute-force path verbatim even when
// a trained IVF index is present, preserving existing behavior.
func TestSemanticAnchors_DefaultIgnoresIVF(t *testing.T) {
	ctx := context.Background()
	store := NewFakeStore()
	const table = "graph"
	seedIvfIndex(t, store, table, optinVecs, 2, 6, 2, 1)
	seedBruteForceVectors(t, store, table, optinVecs)

	c, err := New(Config{
		Store: store, Table: table, Embedder: FakeEmbedder{Dim: 4},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	got, err := c.semanticAnchors(ctx, analysis{vector: optinVecs[0]})
	if err != nil {
		t.Fatalf("semanticAnchors: %v", err)
	}
	// Brute-force VectorSearch returns rows sorted by uniqueRows; evt:0 must be
	// present as the closest match to its own vector.
	if len(got) == 0 {
		t.Fatalf("default semanticAnchors returned no rows")
	}
	found := false
	for _, r := range got {
		if r == "evt:0" {
			found = true
		}
	}
	if !found {
		t.Errorf("default semanticAnchors = %v, want it to contain evt:0", got)
	}
}
