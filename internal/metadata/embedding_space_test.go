// Licensed to the Apache Software Foundation (ASF) under one
// or more contributor license agreements. See the NOTICE file distributed
// with this work for additional information regarding copyright ownership.
// The ASF licenses this file to you under the Apache License, Version 2.0.
package metadata

import (
	"errors"
	"testing"

	"github.com/phrocker/shoal-oss/internal/embeddingspace"
	"github.com/phrocker/shoal-oss/internal/thrift/gen/data"
)

func TestAggregateRowsFileEmbeddingStates(t *testing.T) {
	row := "2k<"
	fileA := `{"path":"hdfs://nn/tables/2k/A.rf","startRow":"","endRow":""}`
	fileB := `{"path":"hdfs://nn/tables/2k/B.rf","startRow":"","endRow":""}`
	fileC := `{"path":"hdfs://nn/tables/2k/C.rf","startRow":"","endRow":""}`
	spaceValue, err := embeddingspace.Encode(embeddingspace.Has("space-a"))
	if err != nil {
		t.Fatal(err)
	}
	unknownValue, err := embeddingspace.Encode(embeddingspace.Unknown())
	if err != nil {
		t.Fatal(err)
	}
	out, err := AggregateRows([]*data.TKeyValue{
		kv(row, CFFile, fileA, "100,10"),
		kv(row, CFFileEmbedding, fileA, string(spaceValue)),
		kv(row, CFFile, fileB, "200,20"),
		kv(row, CFFileEmbedding, fileC, string(unknownValue)),
		kv(row, CFFile, fileC, "300,30"),
		kv(row, CFTabletSection, CQPrevRow, "\x00"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := out[0].Files[0].Embedding; got != embeddingspace.Has("space-a") {
		t.Fatalf("file A embedding = %+v", got)
	}
	if got := out[0].Files[1].Embedding; got != embeddingspace.Unknown() {
		// File B has a file: entry and no file.embedding column. Before
		// issue #274 this asserted no_embeddings, which is a positive
		// claim the aggregation layer had no basis for. Absence of the
		// column is absence of information.
		t.Fatalf("file B missing embedding column = %+v, want unknown", got)
	}
	if got := out[0].Files[2].Embedding; got != embeddingspace.Unknown() {
		t.Fatalf("file C embedding = %+v", got)
	}
}

func TestMetadataFooterEmbeddingDisagreementIsTyped(t *testing.T) {
	err := embeddingspace.VerifyMetadataMatchesFooter(
		embeddingspace.Has("space-a"),
		embeddingspace.Has("space-b"),
	)
	if !errors.Is(err, embeddingspace.ErrIntegrity) {
		t.Fatalf("error = %v, want ErrIntegrity", err)
	}
}

func TestCountEmbeddingSpaces(t *testing.T) {
	counts := CountEmbeddingSpaces([]TabletInfo{{
		Files: []FileEntry{
			{NumEntries: 2, Embedding: embeddingspace.Has("space-a")},
			{NumEntries: 3, Embedding: embeddingspace.Has("space-a")},
			{NumEntries: 5, Embedding: embeddingspace.NoEmbeddings()},
		},
	}})
	if len(counts) != 2 {
		t.Fatalf("counts = %+v", counts)
	}
	if counts[0].State != embeddingspace.StateHasEmbeddings ||
		counts[0].Identity != "space-a" || counts[0].Files != 2 ||
		counts[0].SpanCount != 5 {
		t.Fatalf("has count = %+v", counts[0])
	}
	if counts[1].State != embeddingspace.StateNoEmbeddings ||
		counts[1].Files != 1 || counts[1].SpanCount != 5 {
		t.Fatalf("none count = %+v", counts[1])
	}
}
