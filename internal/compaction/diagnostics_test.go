// Licensed to the Apache Software Foundation (ASF) under one
// or more contributor license agreements. See the NOTICE file distributed
// with this work for additional information regarding copyright ownership.
// The ASF licenses this file to you under the Apache License, Version 2.0.
package compaction

import (
	"errors"
	"strings"
	"testing"

	"github.com/phrocker/shoal-oss/internal/embeddingspace"
	"github.com/phrocker/shoal-oss/internal/iterrt"
)

// TestUnknownMetadataColumnRefusalNamesTheFileAndTheBackfill covers the
// operator diagnostic issue #274 asks for. After the default flip a file
// whose metadata column was never written reads as unknown; when that
// disagrees with a footer that does name a space the compaction is
// refused, and the operator has to be able to tell that from a genuine
// mixed-space tablet.
func TestUnknownMetadataColumnRefusalNamesTheFileAndTheBackfill(t *testing.T) {
	input := buildRFileInSpace(t, "a", embeddingspace.Has("space-a"))
	_, err := Compact(Spec{
		Inputs: []Input{{
			Name:              "t-0001/A00001.rf",
			Bytes:             input,
			MetadataEmbedding: embeddingspace.Unknown(),
		}},
		Scope: iterrt.ScopeMajc,
	})
	if !errors.Is(err, ErrEmbeddingBackfillRequired) {
		t.Fatalf("error = %v, want ErrEmbeddingBackfillRequired", err)
	}
	if !errors.Is(err, embeddingspace.ErrIntegrity) {
		t.Fatalf("error = %v, want the integrity cause preserved", err)
	}
	if !strings.Contains(err.Error(), "t-0001/A00001.rf") {
		t.Fatalf("error %q does not name the offending file", err)
	}
	if !strings.Contains(err.Error(), BackfillCommand) {
		t.Fatalf("error %q does not name the backfill command", err)
	}
}

// TestGenuineIdentityConflictIsNotReportedAsABackfill is the other side
// of the same diagnostic: two files that really do hold different
// embedding spaces are a data condition, not a metadata gap, and telling
// an operator to run a backfill would send them somewhere useless.
func TestGenuineIdentityConflictIsNotReportedAsABackfill(t *testing.T) {
	a := buildRFileInSpace(t, "a", embeddingspace.Has("space-a"))
	b := buildRFileInSpace(t, "b", embeddingspace.Has("space-b"))
	_, err := Compact(Spec{
		Inputs: []Input{
			{Name: "t-0001/A00001.rf", Bytes: a},
			{Name: "t-0001/A00002.rf", Bytes: b},
		},
		Scope: iterrt.ScopeMajc,
	})
	if !errors.Is(err, embeddingspace.ErrMismatch) {
		t.Fatalf("error = %v, want ErrMismatch", err)
	}
	if errors.Is(err, ErrEmbeddingBackfillRequired) {
		t.Fatalf("error = %v, must not claim a backfill would help", err)
	}
	for _, want := range []string{"t-0001/A00001.rf", "t-0001/A00002.rf", "has_embeddings:space-a"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error %q does not mention %q", err, want)
		}
	}
}

// TestIdentityConflictWithAnUnknownInputAsksForTheBackfillFirst: when a
// tablet holds both a genuine conflict and an unclassified file, the
// backfill is the first thing to run, because it may turn the conflict
// into a merge (or confirm it).
func TestIdentityConflictWithAnUnknownInputAsksForTheBackfillFirst(t *testing.T) {
	a := buildRFileInSpace(t, "a", embeddingspace.Has("space-a"))
	b := buildRFileInSpace(t, "b", embeddingspace.Has("space-b"))
	c := buildRFileInSpace(t, "c", embeddingspace.Unknown())
	_, err := Compact(Spec{
		Inputs: []Input{
			{Name: "t-0001/A00001.rf", Bytes: a},
			{Name: "t-0001/A00002.rf", Bytes: b},
			{Name: "t-0001/A00003.rf", Bytes: c},
		},
		Scope: iterrt.ScopeMajc,
	})
	if !errors.Is(err, ErrEmbeddingBackfillRequired) {
		t.Fatalf("error = %v, want ErrEmbeddingBackfillRequired", err)
	}
	if !errors.Is(err, embeddingspace.ErrMismatch) {
		t.Fatalf("error = %v, want the mismatch cause preserved", err)
	}
	if !strings.Contains(err.Error(), "t-0001/A00003.rf has no established embedding space") {
		t.Fatalf("error %q does not name the unresolved file", err)
	}
}

// TestAbsentMetadataColumnRefusalStillNamesTheFile: a compaction whose
// caller supplied no metadata column at all (the zero FileState) is not
// making a claim, so no integrity check runs — but if the merge itself
// is refused the file still has to be named.
func TestAbsentMetadataColumnRefusalStillNamesTheFile(t *testing.T) {
	a := buildRFileInSpace(t, "a", embeddingspace.Has("space-a"))
	b := buildRFileInSpace(t, "b", embeddingspace.Has("space-b"))
	_, err := Compact(Spec{
		Inputs: []Input{
			{Name: "t-0001/A00001.rf", Bytes: a},
			{Name: "t-0001/A00002.rf", Bytes: b},
		},
		Scope: iterrt.ScopeMajc,
	})
	if err == nil || !strings.Contains(err.Error(), "inputs: t-0001/A00001.rf=has_embeddings:space-a") {
		t.Fatalf("error %v does not render the per-input states", err)
	}
}
