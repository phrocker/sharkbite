// Licensed to the Apache Software Foundation (ASF) under one
// or more contributor license agreements. See the NOTICE file distributed
// with this work for additional information regarding copyright ownership.
// The ASF licenses this file to you under the Apache License, Version 2.0.
package metadatacas

import (
	"context"
	"errors"
	"testing"

	"github.com/phrocker/shoal-oss/internal/embeddingspace"
	"github.com/phrocker/shoal-oss/internal/metadata"
)

func TestBackfillWriterRequiresCompatibleSidecar(t *testing.T) {
	cluster := newFakeCluster()
	cluster.cells[cell(metadata.CFFile, "existing-file")] = []byte("100,10")
	writer, err := NewBackfillWriter(cluster, cluster, cluster)
	if writer != nil || !errors.Is(err, ErrEmbeddingSidecarRequired) {
		t.Fatalf("NewBackfillWriter = %v, %v, want sidecar rejection", writer, err)
	}
	applied, err := new(BackfillWriter).WriteFileEmbedding(context.Background(), BackfillTarget{
		TableID: "5", FileQualifier: []byte("existing-file"), FileValue: []byte("100,10"),
	}, embeddingspace.Has("space-a"))
	if applied || !errors.Is(err, ErrEmbeddingSidecarRequired) {
		t.Fatalf("WriteFileEmbedding = %t, %v, want sidecar rejection", applied, err)
	}
	cluster.mu.Lock()
	defer cluster.mu.Unlock()
	if _, ok := cluster.cells[cell(metadata.CFFileEmbedding, "existing-file")]; ok {
		t.Fatal("backfill wrote an Accumulo-incompatible family")
	}
}

func TestBackfillWriterRefusesRootTablet(t *testing.T) {
	applied, err := new(BackfillWriter).WriteFileEmbedding(context.Background(), BackfillTarget{
		TableID: metadata.RootTableID,
	}, embeddingspace.Has("space-a"))
	if applied || !errors.Is(err, ErrRootBackfillUnsupported) {
		t.Fatalf("root backfill = %t, %v, want root refusal", applied, err)
	}
}

func TestBackfillWriterNilReceiverIsInvalid(t *testing.T) {
	var writer *BackfillWriter
	applied, err := writer.WriteFileEmbedding(context.Background(), BackfillTarget{
		TableID: "5",
	}, embeddingspace.Has("space-a"))
	if applied || !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("nil backfill writer = %t, %v, want invalid config", applied, err)
	}
}

func TestLocateMetadataTargetPreservesEmptyBounds(t *testing.T) {
	reader := emptyBoundReader{}
	_, _, extent, err := locateMetadataTarget(
		context.Background(), reader, nil, "5", []byte("5;row"))
	if err != nil {
		t.Fatal(err)
	}
	if extent.PrevEndRow == nil || len(extent.PrevEndRow) != 0 {
		t.Fatalf("PrevEndRow = %#v, want an empty non-nil bound", extent.PrevEndRow)
	}
	if extent.EndRow == nil || len(extent.EndRow) != 0 {
		t.Fatalf("EndRow = %#v, want an empty non-nil bound", extent.EndRow)
	}
}

type emptyBoundReader struct{}

func (emptyBoundReader) LocateTable(context.Context, string) ([]metadata.TabletInfo, error) {
	return []metadata.TabletInfo{{
		TableID:    metadata.MetadataTableID,
		PrevRowSet: true,
		PrevRow:    []byte{},
		EndRow:     []byte{},
		Location:   &metadata.Location{HostPort: "md:9997"},
	}}, nil
}

func TestNormalizedEmbeddingPreservesOnlyUndeclaredAsUnknown(t *testing.T) {
	if got := normalizedEmbedding(embeddingspace.FileState{}); got != embeddingspace.Unknown() {
		t.Fatalf("normalized zero = %+v, want unknown", got)
	}
	partial := embeddingspace.FileState{Identity: "space-a"}
	if got := normalizedEmbedding(partial); got != partial {
		t.Fatalf("normalized partial = %+v, want unchanged", got)
	}
	if err := normalizedEmbedding(partial).Validate(); err == nil {
		t.Fatal("partial embedding state passed validation")
	}
}
