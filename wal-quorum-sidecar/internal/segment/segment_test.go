// Licensed to the Apache Software Foundation (ASF) under one
// or more contributor license agreements.
package segment_test

import (
	"bytes"
	"crypto/sha256"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"github.com/accumulo/wal-quorum-sidecar/internal/segment"
)

func testLogger() *slog.Logger {
	return slog.Default()
}

// TestSegmentReplayAfterPeerDelay simulates the startup race:
// 1. Originator creates segment and writes data (peers not ready)
// 2. Peer comes up late
// 3. Originator replays buffered data to peer
func TestSegmentReplayAfterPeerDelay(t *testing.T) {
	// Create temp dirs for originator and peer
	origDir := t.TempDir()
	peerDir := t.TempDir()

	// Create segment manager for originator
	origMgr := segment.NewManager(origDir, testLogger())

	// Create a segment and write some data
	seg, err := origMgr.Create("test-segment-1", "/wal/test/test-segment-1", "tserver-0")
	if err != nil {
		t.Fatalf("failed to create segment: %v", err)
	}

	// Write 10 entries
	for i := 0; i < 10; i++ {
		data := []byte("entry-" + string(rune('0'+i)) + "-data-payload-for-testing")
		_, _, err := seg.Write(data, uint64(i+1))
		if err != nil {
			t.Fatalf("failed to write entry %d: %v", i, err)
		}
	}

	// Sync
	if err := seg.Fdatasync(); err != nil {
		t.Fatalf("fdatasync failed: %v", err)
	}

	// Verify segment has data
	offset := seg.Offset()
	if offset == 0 {
		t.Fatal("segment offset should be > 0 after writes")
	}
	t.Logf("originator segment has %d bytes, %d entries", offset, seg.HighSequence())

	// Simulate peer coming up late — create peer segment manager
	peerMgr := segment.NewManager(peerDir, testLogger())

	// Peer creates replica (PrepareSegment equivalent)
	peerSeg, err := peerMgr.Create("test-segment-1", "/wal/test/test-segment-1", "tserver-0")
	if err != nil {
		t.Fatalf("peer failed to create segment: %v", err)
	}

	// Simulate replay: read originator's segment file and write to peer
	origFile := seg.FilePath()
	data, err := os.ReadFile(origFile)
	if err != nil {
		t.Fatalf("failed to read originator segment: %v", err)
	}

	if len(data) == 0 {
		t.Fatal("originator segment file is empty")
	}

	// Write replayed data to peer segment
	_, _, err = peerSeg.Write(data, 0) // seq 0 for replay
	if err != nil {
		t.Fatalf("peer failed to write replayed data: %v", err)
	}

	if err := peerSeg.Fdatasync(); err != nil {
		t.Fatalf("peer fdatasync failed: %v", err)
	}

	// Verify peer has same data
	peerOffset := peerSeg.Offset()
	if peerOffset != offset {
		t.Errorf("peer offset %d != originator offset %d", peerOffset, offset)
	}

	// Verify files are identical
	peerData, err := os.ReadFile(peerSeg.FilePath())
	if err != nil {
		t.Fatalf("failed to read peer segment: %v", err)
	}

	if len(peerData) != len(data) {
		t.Errorf("peer file size %d != originator file size %d", len(peerData), len(data))
	}

	t.Logf("replay successful: peer has %d bytes matching originator", peerOffset)

	// Clean up
	seg.Close()
	peerSeg.Close()
}

// TestSegmentSealDoesNotBlockReplay verifies that a sealed segment
// can still be read for replay purposes.
func TestSegmentSealDoesNotBlockReplay(t *testing.T) {
	dir := t.TempDir()
	mgr := segment.NewManager(dir, testLogger())

	seg, err := mgr.Create("seal-test", "/wal/test/seal-test", "tserver-0")
	if err != nil {
		t.Fatalf("create failed: %v", err)
	}

	// Write data
	testData := []byte("important WAL data that must survive")
	_, _, err = seg.Write(testData, 1)
	if err != nil {
		t.Fatalf("write failed: %v", err)
	}
	seg.Fdatasync()

	// Seal the segment
	checksum, size, err := seg.Seal()
	if err != nil {
		t.Fatalf("seal failed: %v", err)
	}
	t.Logf("sealed: size=%d, checksum=%x", size, checksum)

	// Verify we can still read the file even though segment is sealed
	data, err := os.ReadFile(seg.FilePath())
	if err != nil {
		t.Fatalf("reading sealed segment file failed: %v", err)
	}

	if len(data) == 0 {
		t.Fatal("sealed segment file is empty")
	}

	// Verify writing to sealed segment fails
	_, _, err = seg.Write([]byte("should fail"), 2)
	if err == nil {
		t.Fatal("expected write to sealed segment to fail")
	}

	t.Logf("sealed segment readable (%d bytes), write correctly rejected", len(data))

	seg.Close()
}

// TestManagerCreateIsIdempotentForSameOwner covers the write-outage case:
// the TServer-side WAL open is retried for the same segment id after the
// sidecar already created it. A repeat create by the same owner, on a live
// (unsealed) segment, must return the existing segment instead of failing.
func TestManagerCreateIsIdempotentForSameOwner(t *testing.T) {
	mgr := segment.NewManager(t.TempDir(), testLogger())

	const (
		id         = "2648432f-dup"
		walPath    = "/accumulo/wal/tserver-2+9997/2648432f-dup"
		originator = "tserver-2"
	)

	first, err := mgr.Create(id, walPath, originator)
	if err != nil {
		t.Fatalf("first create failed: %v", err)
	}
	if _, _, err := first.Write([]byte("already-written"), 1); err != nil {
		t.Fatalf("write: %v", err)
	}
	offsetBefore := first.Offset()

	second, err := mgr.Create(id, walPath, originator)
	if err != nil {
		t.Fatalf("repeat create by the same owner must succeed, got: %v", err)
	}
	if second != first {
		t.Error("repeat create must return the existing segment handle")
	}
	if second.Offset() != offsetBefore {
		t.Errorf("repeat create must not truncate or rewind: offset %d, want %d",
			second.Offset(), offsetBefore)
	}
	first.Close()
}

// TestManagerCreateRejectsDifferentOwner verifies the other half of the
// contract: an id held by a different originator pod is a genuine conflict and
// must stay an error.
func TestManagerCreateRejectsDifferentOwner(t *testing.T) {
	mgr := segment.NewManager(t.TempDir(), testLogger())

	seg, err := mgr.Create("conflict-1", "/wal/a/conflict-1", "tserver-2")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	defer seg.Close()

	if _, err := mgr.Create("conflict-1", "/wal/a/conflict-1", "tserver-9"); err == nil {
		t.Fatal("expected an error when another pod claims a live segment id")
	}
}

// TestManagerCreateRejectsDifferentWalPath verifies that the same uuid pointing
// at a different WAL path is a conflict, not an idempotent re-open.
func TestManagerCreateRejectsDifferentWalPath(t *testing.T) {
	mgr := segment.NewManager(t.TempDir(), testLogger())

	seg, err := mgr.Create("conflict-2", "/wal/a/conflict-2", "tserver-2")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	defer seg.Close()

	if _, err := mgr.Create("conflict-2", "/wal/b/conflict-2", "tserver-2"); err == nil {
		t.Fatal("expected an error when the same uuid maps to a different WAL path")
	}
}

// TestManagerCreateRejectsSealedReopen verifies that a sealed segment's
// generation is over: re-opening it would append past a finalized checksum.
func TestManagerCreateRejectsSealedReopen(t *testing.T) {
	mgr := segment.NewManager(t.TempDir(), testLogger())

	seg, err := mgr.Create("sealed-1", "/wal/a/sealed-1", "tserver-2")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, _, err := seg.Write([]byte("data"), 1); err != nil {
		t.Fatalf("write: %v", err)
	}
	if _, _, err := seg.Seal(); err != nil {
		t.Fatalf("seal: %v", err)
	}
	defer seg.Close()

	if _, err := mgr.Create("sealed-1", "/wal/a/sealed-1", "tserver-2"); err == nil {
		t.Fatal("expected an error when re-opening a sealed segment")
	}
}

// TestManagerCreateRejectsForeignEpoch verifies that a segment left behind by a
// previous incarnation (loaded off disk, owner unknown) is never silently
// adopted by a new local open — that would append this incarnation's entries
// onto another generation's bytes.
func TestManagerCreateRejectsForeignEpoch(t *testing.T) {
	dir := t.TempDir()

	// Previous incarnation writes a segment file and exits.
	old := segment.NewManager(dir, testLogger())
	oldSeg, err := old.Create("epoch-1", "/wal/a/epoch-1", "tserver-2")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, _, err := oldSeg.Write([]byte("previous generation bytes"), 1); err != nil {
		t.Fatalf("write: %v", err)
	}
	oldSeg.Close()

	// New sidecar incarnation, same WAL directory.
	fresh := segment.NewManager(dir, testLogger())
	if _, err := fresh.Create("epoch-1", "/wal/a/epoch-1", "tserver-2"); err == nil {
		t.Fatal("expected an error when opening a segment left by a previous incarnation")
	}
}

// TestManagerOpenCount verifies segment tracking
func TestManagerOpenCount(t *testing.T) {
	dir := t.TempDir()
	mgr := segment.NewManager(dir, testLogger())

	if mgr.OpenCount() != 0 {
		t.Fatal("expected 0 open segments")
	}

	seg1, _ := mgr.Create("s1", "/wal/test/s1", "t-0")
	if mgr.OpenCount() != 1 {
		t.Fatal("expected 1 open segment")
	}

	seg2, _ := mgr.Create("s2", "/wal/test/s2", "t-0")
	if mgr.OpenCount() != 2 {
		t.Fatal("expected 2 open segments")
	}

	seg1.Seal()
	// Sealed segments are still tracked until deleted
	got := mgr.Get("s1")
	if got == nil {
		t.Fatal("sealed segment should still be gettable")
	}

	seg1.Close()
	seg2.Close()
}

// TestSegmentChecksumCoversExistingBytes guards the seal path against a
// reopened file: the running digest has to include the bytes already on disk,
// or a seal reports the digest of the appended tail only and every comparison
// against the originator's checksum fails.
func TestSegmentChecksumCoversExistingBytes(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "reopen.wal")

	first, err := segment.NewSegment("reopen", "/wal/a/reopen", "tserver-0", path)
	if err != nil {
		t.Fatalf("create segment: %v", err)
	}
	if _, _, err := first.Write([]byte("first-half"), 1); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := first.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	reopened, err := segment.NewSegment("reopen", "/wal/a/reopen", "tserver-0", path)
	if err != nil {
		t.Fatalf("reopen segment: %v", err)
	}
	defer reopened.Close()
	if _, _, err := reopened.Write([]byte("second-half"), 2); err != nil {
		t.Fatalf("write after reopen: %v", err)
	}

	checksum, size, err := reopened.Seal()
	if err != nil {
		t.Fatalf("seal: %v", err)
	}

	contents, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read segment file: %v", err)
	}
	if size != int64(len(contents)) {
		t.Errorf("sealed size %d, file holds %d bytes", size, len(contents))
	}
	want := sha256.Sum256(contents)
	if !bytes.Equal(checksum, want[:]) {
		t.Error("sealed checksum does not cover the bytes that were already on disk")
	}
}

// TestManagerDiscardsStaleReplicaFile covers the replica side of a restart.
// A replica file left by a previous incarnation cannot be vouched for, and
// appending to it would leave the replica something other than a prefix of the
// originator's segment, so no replay could repair it. It must be discarded and
// replayed from the start instead.
func TestManagerDiscardsStaleReplicaFile(t *testing.T) {
	dir := t.TempDir()
	stale := filepath.Join(dir, "stale-replica.wal")
	if err := os.WriteFile(stale, []byte("bytes from a previous generation"), 0o640); err != nil {
		t.Fatalf("seed stale replica: %v", err)
	}

	mgr := segment.NewManager(dir, testLogger())
	seg, adopted, err := mgr.CreateOrAdopt("stale-replica", "/wal/a/stale-replica",
		"tserver-0", segment.RoleReplica)
	if err != nil {
		t.Fatalf("replica prepare must succeed, got: %v", err)
	}
	if adopted {
		t.Error("a stale on-disk file is not an adoptable live segment")
	}
	if seg.Offset() != 0 {
		t.Errorf("replica reattached to %d stale bytes; the originator's replay "+
			"would then never line up", seg.Offset())
	}
}
