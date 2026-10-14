// Licensed to the Apache Software Foundation (ASF) under one
// or more contributor license agreements. See the NOTICE file distributed
// with this work for additional information regarding copyright ownership.
// The ASF licenses this file to you under the Apache License, Version 2.0.
package metadata

import (
	"encoding/json"
	"testing"

	"github.com/phrocker/shoal-oss/internal/embeddingspace"
	"github.com/phrocker/shoal-oss/internal/thrift/gen/data"
)

// TestAggregateRowsAbsentColumnIsUnknown pins issue #274: a file entry
// with no file.embedding column must be unknown, never no_embeddings.
// no_embeddings merges freely with every space, so fabricating it turns
// "we were never told" into "we know there is nothing".
func TestAggregateRowsAbsentColumnIsUnknown(t *testing.T) {
	row := "2k<"
	fileA := `{"path":"hdfs://nn/tables/2k/A.rf","startRow":"","endRow":""}`
	out, err := AggregateRows([]*data.TKeyValue{
		kv(row, CFFile, fileA, "100,10"),
		kv(row, CFTabletSection, CQPrevRow, "\x00"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := out[0].Files[0].Embedding; got != embeddingspace.Unknown() {
		t.Fatalf("absent column = %+v, want unknown", got)
	}
}

// TestAggregateRowsExplicitNoEmbeddingsSurvives is the other half of the
// same invariant: an explicitly decoded no_embeddings column is a real
// assertion and must be preserved exactly.
func TestAggregateRowsExplicitNoEmbeddingsSurvives(t *testing.T) {
	row := "2k<"
	fileA := `{"path":"hdfs://nn/tables/2k/A.rf","startRow":"","endRow":""}`
	value, err := embeddingspace.Encode(embeddingspace.NoEmbeddings())
	if err != nil {
		t.Fatal(err)
	}
	out, err := AggregateRows([]*data.TKeyValue{
		kv(row, CFFile, fileA, "100,10"),
		kv(row, CFFileEmbedding, fileA, string(value)),
		kv(row, CFTabletSection, CQPrevRow, "\x00"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := out[0].Files[0].Embedding; got != embeddingspace.NoEmbeddings() {
		t.Fatalf("explicit column = %+v, want no embeddings", got)
	}
}

// TestAggregateRowsColumnBeforeFileEntryIsPreserved exercises the
// finalize pass: the column arrives before the file: entry it describes,
// so the entry is created from the pending map rather than left empty.
func TestAggregateRowsColumnBeforeFileEntryIsPreserved(t *testing.T) {
	row := "2k<"
	fileA := `{"path":"hdfs://nn/tables/2k/A.rf","startRow":"","endRow":""}`
	fileB := `{"path":"hdfs://nn/tables/2k/B.rf","startRow":"","endRow":""}`
	value, err := embeddingspace.Encode(embeddingspace.NoEmbeddings())
	if err != nil {
		t.Fatal(err)
	}
	out, err := AggregateRows([]*data.TKeyValue{
		kv(row, CFFileEmbedding, fileA, string(value)),
		kv(row, CFFile, fileA, "100,10"),
		kv(row, CFFile, fileB, "200,20"),
		kv(row, CFTabletSection, CQPrevRow, "\x00"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := out[0].Files[0].Embedding; got != embeddingspace.NoEmbeddings() {
		t.Fatalf("file A = %+v, want no embeddings", got)
	}
	if got := out[0].Files[1].Embedding; got != embeddingspace.Unknown() {
		t.Fatalf("file B = %+v, want unknown", got)
	}
}

// TestDecodeRootTabletMetadataAbsentColumnIsUnknown covers the root
// tablet, which builds its file entries on a separate code path from
// AggregateRows and had the same fabricated default.
func TestDecodeRootTabletMetadataAbsentColumnIsUnknown(t *testing.T) {
	fileA := `{"path":"hdfs://nn/tables/+r/A.rf","startRow":"","endRow":""}`
	fileB := `{"path":"hdfs://nn/tables/+r/B.rf","startRow":"","endRow":""}`
	declared, err := embeddingspace.Encode(embeddingspace.NoEmbeddings())
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(map[string]any{
		"version": 1,
		"columnValues": map[string]map[string]string{
			CFFile: {
				fileA: "100,10",
				fileB: "200,20",
			},
			CFFileEmbedding: {fileA: string(declared)},
			CFTabletSection: {CQPrevRow: "\x00"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	info, err := DecodeRootTabletMetadata(encoded)
	if err != nil {
		t.Fatal(err)
	}
	if len(info.Files) != 2 {
		t.Fatalf("files = %d, want 2", len(info.Files))
	}
	for _, file := range info.Files {
		want := embeddingspace.Unknown()
		if file.Path == "hdfs://nn/tables/+r/A.rf" {
			want = embeddingspace.NoEmbeddings()
		}
		if file.Embedding != want {
			t.Fatalf("%s embedding = %+v, want %+v", file.Path, file.Embedding, want)
		}
	}
}

// TestFinalizeFileEmbeddingsBackstopIsUnknown pins the second layer
// directly. Every path that builds a FileEntry today already starts it
// at unknown, so this backstop is unreachable through AggregateRows —
// but it is the value a future construction path inherits, and the whole
// point of issue #274 is that the fallback must never be a positive
// claim.
func TestFinalizeFileEmbeddingsBackstopIsUnknown(t *testing.T) {
	info := &TabletInfo{Files: []FileEntry{
		{Path: "hdfs://nn/tables/2k/A.rf"},
		{Path: "hdfs://nn/tables/2k/B.rf", Embedding: embeddingspace.NoEmbeddings()},
		{Path: "hdfs://nn/tables/2k/C.rf", Embedding: embeddingspace.Has("space-a")},
	}}
	if err := finalizeFileEmbeddings(info); err != nil {
		t.Fatal(err)
	}
	want := []embeddingspace.FileState{
		embeddingspace.Unknown(),
		embeddingspace.NoEmbeddings(),
		embeddingspace.Has("space-a"),
	}
	for i, file := range info.Files {
		if file.Embedding != want[i] {
			t.Fatalf("%s = %+v, want %+v", file.Path, file.Embedding, want[i])
		}
	}
}

// TestCountEmbeddingSpacesReportsUnknownDistinctly is the operator-facing
// half of issue #274. Folding unknown into no_embeddings would make a
// migration read as complete while unclassified files remain.
func TestCountEmbeddingSpacesReportsUnknownDistinctly(t *testing.T) {
	counts := CountEmbeddingSpaces([]TabletInfo{{
		Files: []FileEntry{
			{NumEntries: 2, Embedding: embeddingspace.Has("space-a")},
			{NumEntries: 3, Embedding: embeddingspace.NoEmbeddings()},
			{NumEntries: 5, Embedding: embeddingspace.Unknown()},
			// A zero FileState reaches this bucket too: nothing has
			// been recorded, which is exactly unknown.
			{NumEntries: 7},
		},
	}})
	if len(counts) != 3 {
		t.Fatalf("counts = %+v, want three distinct buckets", counts)
	}
	byState := map[embeddingspace.State]EmbeddingSpaceCount{}
	for _, count := range counts {
		byState[count.State] = count
	}
	none, ok := byState[embeddingspace.StateNoEmbeddings]
	if !ok || none.Files != 1 || none.SpanCount != 3 {
		t.Fatalf("no_embeddings bucket = %+v", none)
	}
	unknown, ok := byState[embeddingspace.StateUnknown]
	if !ok || unknown.Files != 2 || unknown.SpanCount != 12 {
		t.Fatalf("unknown bucket = %+v", unknown)
	}
}
