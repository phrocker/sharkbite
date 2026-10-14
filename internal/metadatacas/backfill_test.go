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

const backfillEntry = `{"path":"hdfs://nn/tables/5/A.rf","startRow":"","endRow":""}`

func backfillTarget() BackfillTarget {
	return BackfillTarget{
		TableID:       "5",
		FileQualifier: []byte(backfillEntry),
		FileValue:     []byte("100,10"),
	}
}

func newBackfillFixture(t *testing.T) (*fakeCluster, *BackfillWriter) {
	t.Helper()
	cluster := newFakeCluster()
	cluster.cells[cell(metadata.CFFile, backfillEntry)] = []byte("100,10")
	writer, err := NewBackfillWriter(cluster, cluster, cluster)
	if err != nil {
		t.Fatal(err)
	}
	return cluster, writer
}

// TestBackfillWriterWritesTheColumnOnce also pins idempotence at the CAS
// layer: the second write is rejected because the column is no longer
// absent, so a re-run cannot overwrite an established label.
func TestBackfillWriterWritesTheColumnOnce(t *testing.T) {
	cluster, writer := newBackfillFixture(t)
	applied, err := writer.WriteFileEmbedding(
		context.Background(), backfillTarget(), embeddingspace.Has("space-a"))
	if err != nil {
		t.Fatal(err)
	}
	if !applied {
		t.Fatal("first write was not applied")
	}
	stored := cluster.cells[cell(metadata.CFFileEmbedding, backfillEntry)]
	state, err := embeddingspace.Decode(stored)
	if err != nil {
		t.Fatal(err)
	}
	if state != embeddingspace.Has("space-a") {
		t.Fatalf("stored column = %+v", state)
	}

	applied, err = writer.WriteFileEmbedding(
		context.Background(), backfillTarget(), embeddingspace.Has("space-b"))
	if err != nil {
		t.Fatal(err)
	}
	if applied {
		t.Fatal("second write must be rejected; the column is already present")
	}
	if string(cluster.cells[cell(metadata.CFFileEmbedding, backfillEntry)]) != string(stored) {
		t.Fatal("an established column was overwritten")
	}
}

// TestBackfillWriterRejectsAChangedFileEntry: a file replaced by a
// compaction mid-run is not the file whose footer was read, so the
// column must not land on it.
func TestBackfillWriterRejectsAChangedFileEntry(t *testing.T) {
	cluster, writer := newBackfillFixture(t)
	cluster.cells[cell(metadata.CFFile, backfillEntry)] = []byte("900,90")
	applied, err := writer.WriteFileEmbedding(
		context.Background(), backfillTarget(), embeddingspace.Has("space-a"))
	if err != nil {
		t.Fatal(err)
	}
	if applied {
		t.Fatal("write applied against a changed DataFileValue")
	}
	if _, ok := cluster.cells[cell(metadata.CFFileEmbedding, backfillEntry)]; ok {
		t.Fatal("a column was written for a file entry that had changed")
	}
}

// TestBackfillWriterRejectsAMissingFileEntry covers the orphan hazard:
// writing file.embedding for a file: entry that no longer exists would
// make metadata aggregation fail for the whole tablet.
func TestBackfillWriterRejectsAMissingFileEntry(t *testing.T) {
	cluster, writer := newBackfillFixture(t)
	delete(cluster.cells, cell(metadata.CFFile, backfillEntry))
	applied, err := writer.WriteFileEmbedding(
		context.Background(), backfillTarget(), embeddingspace.Has("space-a"))
	if err != nil {
		t.Fatal(err)
	}
	if applied {
		t.Fatal("write applied against a deleted file entry")
	}
	if _, ok := cluster.cells[cell(metadata.CFFileEmbedding, backfillEntry)]; ok {
		t.Fatal("an orphan file.embedding column was written")
	}
}

// TestBackfillWriterRefusesToRecordUnknown: the backfill exists to
// replace absence with a positive claim taken from a footer. Recording
// unknown would write a non-claim no footer produced.
func TestBackfillWriterRefusesToRecordUnknown(t *testing.T) {
	cluster, writer := newBackfillFixture(t)
	for _, state := range []embeddingspace.FileState{
		embeddingspace.Unknown(),
		{},
	} {
		if _, err := writer.WriteFileEmbedding(context.Background(), backfillTarget(), state); err == nil {
			t.Fatalf("WriteFileEmbedding(%+v) was accepted", state)
		}
	}
	if _, ok := cluster.cells[cell(metadata.CFFileEmbedding, backfillEntry)]; ok {
		t.Fatal("a column was written for a state with no basis")
	}
}

// TestBackfillWriterReplacesAnExplicitUnknownColumn: an explicit unknown
// column is as uninformative as an absent one, and the compaction
// refusal tells operators the backfill repairs it — so the write is
// conditioned on the stored bytes rather than on absence.
func TestBackfillWriterReplacesAnExplicitUnknownColumn(t *testing.T) {
	cluster, writer := newBackfillFixture(t)
	stored, err := embeddingspace.Encode(embeddingspace.Unknown())
	if err != nil {
		t.Fatal(err)
	}
	cluster.cells[cell(metadata.CFFileEmbedding, backfillEntry)] = stored
	target := backfillTarget()
	target.ExistingEmbedding = stored

	applied, err := writer.WriteFileEmbedding(
		context.Background(), target, embeddingspace.Has("space-a"))
	if err != nil {
		t.Fatal(err)
	}
	if !applied {
		t.Fatal("an explicit unknown column was not repaired")
	}
	state, err := embeddingspace.Decode(cluster.cells[cell(metadata.CFFileEmbedding, backfillEntry)])
	if err != nil {
		t.Fatal(err)
	}
	if state != embeddingspace.Has("space-a") {
		t.Fatalf("column = %+v", state)
	}
}

// TestBackfillWriterRefusesToReplaceAnEstablishedColumn: a definite
// column was written by something with better evidence than a migration
// tool. A disagreement with the footer is an integrity condition for an
// operator, not something to silently overwrite.
func TestBackfillWriterRefusesToReplaceAnEstablishedColumn(t *testing.T) {
	cluster, writer := newBackfillFixture(t)
	stored, err := embeddingspace.Encode(embeddingspace.NoEmbeddings())
	if err != nil {
		t.Fatal(err)
	}
	cluster.cells[cell(metadata.CFFileEmbedding, backfillEntry)] = stored
	target := backfillTarget()
	target.ExistingEmbedding = stored

	applied, err := writer.WriteFileEmbedding(
		context.Background(), target, embeddingspace.Has("space-a"))
	if applied || err == nil {
		t.Fatalf("applied = %t, err = %v; an established column must be refused", applied, err)
	}
	if string(cluster.cells[cell(metadata.CFFileEmbedding, backfillEntry)]) != string(stored) {
		t.Fatal("an established column was overwritten")
	}
}

// TestBackfillWriterRefusesAnEmptyExistingColumn: nil means the column
// is absent; a present-but-empty value is a malformed column. Testing
// length rather than nilness would conflate the two and let the write
// replace a value it never decoded, when only a column decoding to
// unknown may be replaced.
func TestBackfillWriterRefusesAnEmptyExistingColumn(t *testing.T) {
	cluster, writer := newBackfillFixture(t)
	cluster.cells[cell(metadata.CFFileEmbedding, backfillEntry)] = []byte{}
	target := backfillTarget()
	target.ExistingEmbedding = []byte{}

	applied, err := writer.WriteFileEmbedding(
		context.Background(), target, embeddingspace.Has("space-a"))
	if applied || !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("applied = %t, err = %v; a malformed column must be refused", applied, err)
	}
	if len(cluster.cells[cell(metadata.CFFileEmbedding, backfillEntry)]) != 0 {
		t.Fatal("a malformed column was overwritten")
	}
}

// TestLocateMetadataTargetPreservesEmptyBounds: a nil prevEndRow means
// the tablet has no lower bound; an empty one means the bound is the
// empty row, which is what a split on the empty row produces. The
// resolver must not collapse the second into the first — append-to-nil
// does exactly that — because the extent then addresses a different
// tablet than the row was found in and every conditional write against
// it is rejected on every retry, forever.
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

// TestBackfillWriterRefusesTheRootTablet: the root tablet's metadata
// lives in ZooKeeper and is owned by whichever server hosts it.
func TestBackfillWriterRefusesTheRootTablet(t *testing.T) {
	_, writer := newBackfillFixture(t)
	target := backfillTarget()
	target.TableID = metadata.RootTableID
	_, err := writer.WriteFileEmbedding(
		context.Background(), target, embeddingspace.NoEmbeddings())
	if !errors.Is(err, ErrRootBackfillUnsupported) {
		t.Fatalf("error = %v, want ErrRootBackfillUnsupported", err)
	}
}

// TestBackfillWriterReportsAnAmbiguousOutcome: an unknown conditional
// outcome may or may not have landed, so it must surface rather than be
// reported as a clean skip.
func TestBackfillWriterReportsAnAmbiguousOutcome(t *testing.T) {
	cluster, writer := newBackfillFixture(t)
	cluster.ambiguous = true
	applied, err := writer.WriteFileEmbedding(
		context.Background(), backfillTarget(), embeddingspace.Has("space-a"))
	if applied {
		t.Fatal("an ambiguous outcome must not be reported as applied")
	}
	if err == nil {
		t.Fatal("an ambiguous outcome must surface as an error")
	}
}

// TestBackfillWriterCachesMetadataRouting: Run calls this once per file,
// and locateMetadataTarget re-derives routing from a LocateTable call,
// which is a root-tablet scan. Without a cache a table with a million
// files costs a million routing scans, which is enough for an operator
// to reasonably decline to run the migration at all.
//
// A rejection drops the cache, because a rejection is also what a stale
// route produces and continuing to send every remaining file to a tablet
// that moved would be worse than paying for one extra scan.
func TestBackfillWriterCachesMetadataRouting(t *testing.T) {
	cluster, writer := newBackfillFixture(t)
	for i := 0; i < 5; i++ {
		target := backfillTarget()
		// A distinct entry each time so the column-absent condition
		// holds and every write is a real accepted write.
		entry := backfillEntry[:len(backfillEntry)-1] + `,"n":` + string(rune('0'+i)) + `}`
		target.FileQualifier = []byte(entry)
		cluster.mu.Lock()
		cluster.cells[cell(metadata.CFFile, entry)] = []byte("100,10")
		cluster.mu.Unlock()
		applied, err := writer.WriteFileEmbedding(
			context.Background(), target, embeddingspace.Has("space-a"))
		if err != nil {
			t.Fatal(err)
		}
		if !applied {
			t.Fatalf("write %d was not applied", i)
		}
	}
	cluster.mu.Lock()
	calls := cluster.locateCalls
	cluster.mu.Unlock()
	if calls != 1 {
		t.Fatalf("LocateTable called %d times for 5 files, want 1: routing must be cached", calls)
	}

	// A rejected write invalidates, so the next file re-resolves.
	rejected := backfillTarget()
	rejected.FileValue = []byte("nope")
	cluster.mu.Lock()
	cluster.cells[cell(metadata.CFFile, backfillEntry)] = []byte("100,10")
	cluster.mu.Unlock()
	if applied, err := writer.WriteFileEmbedding(
		context.Background(), rejected, embeddingspace.Has("space-a")); err != nil || applied {
		t.Fatalf("applied = %v, err = %v; want a rejection", applied, err)
	}
	next := backfillTarget()
	next.FileQualifier = []byte(backfillEntry[:len(backfillEntry)-1] + `,"n":9}`)
	cluster.mu.Lock()
	cluster.cells[cell(metadata.CFFile, string(next.FileQualifier))] = []byte("100,10")
	cluster.mu.Unlock()
	if applied, err := writer.WriteFileEmbedding(
		context.Background(), next, embeddingspace.Has("space-a")); err != nil || !applied {
		t.Fatalf("applied = %v, err = %v; want the follow-up write to land", applied, err)
	}
	cluster.mu.Lock()
	afterReject := cluster.locateCalls
	cluster.mu.Unlock()
	if afterReject != 2 {
		t.Fatalf("LocateTable calls = %d after a rejection, want the cached route dropped", afterReject)
	}
}

// TestUndeclaredCommitEncodesUnknown covers the other write path this
// package owns. encodeFileEmbedding turns a commit into the durable
// file.embedding column, so a commit that declared nothing must be
// persisted as unknown rather than as the no_embeddings claim that
// merges with every space.
func TestUndeclaredCommitEncodesUnknown(t *testing.T) {
	encoded, err := encodeFileEmbedding(embeddingspace.FileState{})
	if err != nil {
		t.Fatal(err)
	}
	state, err := embeddingspace.Decode(encoded)
	if err != nil {
		t.Fatal(err)
	}
	if state != embeddingspace.Unknown() {
		t.Fatalf("undeclared commit encoded as %+v, want unknown", state)
	}

	for _, declared := range []embeddingspace.FileState{
		embeddingspace.NoEmbeddings(),
		embeddingspace.Has("space-a"),
		embeddingspace.Unknown(),
	} {
		encoded, err := encodeFileEmbedding(declared)
		if err != nil {
			t.Fatal(err)
		}
		state, err := embeddingspace.Decode(encoded)
		if err != nil {
			t.Fatal(err)
		}
		if state != declared {
			t.Fatalf("declared %+v encoded as %+v", declared, state)
		}
	}
}

// TestPartialCommitEmbeddingIsRefused: an identity with no state is
// malformed, not undeclared. Normalizing it to unknown would persist
// something other than what the caller supplied by hiding it from
// Encode's validation, and would let a malformed reconciliation request
// compare equal to a stored unknown and be accepted as consistent.
func TestPartialCommitEmbeddingIsRefused(t *testing.T) {
	partial := embeddingspace.FileState{Identity: "space-a"}
	if _, err := encodeFileEmbedding(partial); err == nil {
		t.Fatal("a partial embedding state was encoded instead of refused")
	}
	if got := normalizedEmbedding(partial); got != partial {
		t.Fatalf("normalizedEmbedding(%+v) = %+v, want it returned untouched", partial, got)
	}
	if got := normalizedEmbedding(embeddingspace.FileState{}); got != embeddingspace.Unknown() {
		t.Fatalf("normalizedEmbedding(zero) = %+v, want unknown", got)
	}
}
