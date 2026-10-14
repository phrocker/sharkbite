// Licensed to the Apache Software Foundation (ASF) under one
// or more contributor license agreements.
package integration_test

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"os"
	"testing"
	"time"

	"github.com/accumulo/wal-quorum-sidecar/internal/replication"
	"github.com/accumulo/wal-quorum-sidecar/internal/segment"
	"github.com/accumulo/wal-quorum-sidecar/internal/server"
	pb "github.com/accumulo/wal-quorum-sidecar/proto/qwalpb"
	"google.golang.org/grpc"
)

// startPeerServer starts a peer gRPC server on a random port and returns
// the port and a cleanup function.
func startPeerServer(t *testing.T, mgr *segment.Manager, logger *slog.Logger) (int, func()) {
	t.Helper()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	port := lis.Addr().(*net.TCPAddr).Port

	srv := grpc.NewServer()
	peerSrv := server.NewPeerServer(mgr, logger)
	pb.RegisterWalQuorumPeerServer(srv, peerSrv)

	go func() {
		if err := srv.Serve(lis); err != nil {
			// Ignore errors after stop
		}
	}()

	return port, func() {
		srv.GracefulStop()
	}
}

// TestQuorumReplication_3Nodes tests the full quorum flow:
// 1. Start 3 peer gRPC servers
// 2. Originator opens a segment
// 3. Originator writes entries — peers should replicate
// 4. Verify peers have the data
// 5. Seal — verify all 3 have matching checksums
func TestQuorumReplication_3Nodes(t *testing.T) {
	logger := slog.Default()

	// Create 3 data dirs (simulating 3 pods)
	dirs := make([]string, 3)
	mgrs := make([]*segment.Manager, 3)
	for i := 0; i < 3; i++ {
		dirs[i] = t.TempDir()
		mgrs[i] = segment.NewManager(dirs[i], logger)
	}

	// Start peer gRPC servers for nodes 1 and 2
	port1, stop1 := startPeerServer(t, mgrs[1], logger)
	defer stop1()
	port2, stop2 := startPeerServer(t, mgrs[2], logger)
	defer stop2()

	t.Logf("peer servers started on ports %d, %d", port1, port2)

	// Create peer clients from node 0 to nodes 1 and 2
	peer1 := replication.NewPeerClient(fmt.Sprintf("127.0.0.1:%d", port1), logger)
	defer peer1.Close()
	peer2 := replication.NewPeerClient(fmt.Sprintf("127.0.0.1:%d", port2), logger)
	defer peer2.Close()

	// Wait for peers to be healthy
	ctx := context.Background()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if peer1.IsHealthy() && peer2.IsHealthy() {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}

	// Create segment on originator (node 0)
	segID := "test-quorum-segment-001"
	walPath := "/wal/10.0.0.1+9800/test-quorum-segment-001"
	originator := "tserver-0"

	seg, err := mgrs[0].Create(segID, walPath, originator)
	if err != nil {
		t.Fatalf("create segment: %v", err)
	}

	// PrepareSegment on both peers
	err = peer1.PrepareSegment(ctx, segID, walPath, originator)
	if err != nil {
		t.Fatalf("prepare peer 1: %v", err)
	}
	err = peer2.PrepareSegment(ctx, segID, walPath, originator)
	if err != nil {
		t.Fatalf("prepare peer 2: %v", err)
	}
	t.Log("both peers prepared")

	// Write 20 entries on originator + replicate to peers
	for i := 1; i <= 20; i++ {
		data := []byte(fmt.Sprintf("entry-%03d-payload-data-for-testing-quorum-replication", i))
		seqNum := uint64(i)

		// Write locally
		_, _, err := seg.Write(data, seqNum)
		if err != nil {
			t.Fatalf("local write seq %d: %v", i, err)
		}

		// Replicate to both peers
		err = peer1.ReplicateEntry(ctx, segID, walPath, originator, data, seg.Offset()-int64(len(data)), seqNum)
		if err != nil {
			t.Errorf("replicate to peer 1 seq %d: %v", i, err)
		}
		err = peer2.ReplicateEntry(ctx, segID, walPath, originator, data, seg.Offset()-int64(len(data)), seqNum)
		if err != nil {
			t.Errorf("replicate to peer 2 seq %d: %v", i, err)
		}
	}

	// Sync originator
	if err := seg.Fdatasync(); err != nil {
		t.Fatalf("fdatasync: %v", err)
	}

	t.Logf("originator: %d bytes, %d entries", seg.Offset(), seg.HighSequence())

	// Verify peer segments have data by checking file sizes
	for i, dir := range dirs[1:] {
		peerFile := fmt.Sprintf("%s/%s.wal", dir, segID)
		info, err := os.Stat(peerFile)
		if err != nil {
			t.Errorf("peer %d segment file not found: %v", i+1, err)
			continue
		}
		if info.Size() == 0 {
			t.Errorf("peer %d segment file is empty", i+1)
			continue
		}
		t.Logf("peer %d: %d bytes", i+1, info.Size())

		if info.Size() != seg.Offset() {
			t.Errorf("peer %d size %d != originator size %d", i+1, info.Size(), seg.Offset())
		}
	}

	// Seal originator
	checksum, size, err := seg.Seal()
	if err != nil {
		t.Fatalf("seal: %v", err)
	}
	t.Logf("originator sealed: size=%d, checksum=%x", size, checksum)

	// Seal peers
	for i, peer := range []*replication.PeerClient{peer1, peer2} {
		match, err := peer.SealSegment(ctx, segID, size, checksum)
		if err != nil {
			t.Errorf("seal peer %d: %v", i+1, err)
			continue
		}
		if !match {
			t.Errorf("peer %d checksum mismatch", i+1)
		} else {
			t.Logf("peer %d sealed with matching checksum", i+1)
		}
	}
}

// TestSyncDoesNotSealPeer verifies that calling SyncSegment on a peer
// does NOT seal the peer's segment. This was the root cause bug —
// SyncSegment was mapped to SealSegment, permanently sealing the peer
// after the first sync. Subsequent writes would fail with "segment is sealed".
func TestSyncDoesNotSealPeer(t *testing.T) {
	logger := slog.Default()

	origDir := t.TempDir()
	peerDir := t.TempDir()

	origMgr := segment.NewManager(origDir, logger)
	peerMgr := segment.NewManager(peerDir, logger)

	port, stop := startPeerServer(t, peerMgr, logger)
	defer stop()

	peer := replication.NewPeerClient(fmt.Sprintf("127.0.0.1:%d", port), logger)
	defer peer.Close()

	ctx := context.Background()
	segID := "test-sync-no-seal"
	walPath := "/wal/test/sync-no-seal"

	seg, _ := origMgr.Create(segID, walPath, "tserver-0")
	peer.PrepareSegment(ctx, segID, walPath, "tserver-0")

	// Write 5 entries
	for i := 1; i <= 5; i++ {
		data := []byte(fmt.Sprintf("entry-%d", i))
		seg.Write(data, uint64(i))
		err := peer.ReplicateEntry(ctx, segID, walPath, "tserver-0", data, seg.Offset()-int64(len(data)), uint64(i))
		if err != nil {
			t.Fatalf("replicate %d: %v", i, err)
		}
	}

	// Sync — this MUST NOT seal the peer's segment
	err := peer.SyncSegment(ctx, segID)
	if err != nil {
		t.Fatalf("sync: %v", err)
	}
	t.Log("SyncSegment completed")

	// Write 5 MORE entries — these MUST succeed if sync didn't seal
	for i := 6; i <= 10; i++ {
		data := []byte(fmt.Sprintf("entry-%d", i))
		seg.Write(data, uint64(i))
		err := peer.ReplicateEntry(ctx, segID, walPath, "tserver-0", data, seg.Offset()-int64(len(data)), uint64(i))
		if err != nil {
			t.Fatalf("replicate %d after sync FAILED (sync likely sealed the segment): %v", i, err)
		}
	}

	t.Logf("all 10 entries replicated after sync — peer segment NOT sealed (%d bytes)", seg.Offset())

	// Verify peer file has all data
	peerFile := fmt.Sprintf("%s/%s.wal", peerDir, segID)
	info, _ := os.Stat(peerFile)
	if info.Size() != seg.Offset() {
		t.Errorf("peer size %d != originator %d", info.Size(), seg.Offset())
	}

	seg.Close()
}

// TestQuorumReplication_LateJoin tests the startup race scenario:
// 1. Originator writes entries (no peers available)
// 2. Peer comes up late
// 3. PrepareSegment + replay catches the peer up
// 4. New writes replicate normally
func TestQuorumReplication_LateJoin(t *testing.T) {
	logger := slog.Default()

	origDir := t.TempDir()
	peerDir := t.TempDir()

	origMgr := segment.NewManager(origDir, logger)
	peerMgr := segment.NewManager(peerDir, logger)

	ctx := context.Background()
	segID := "test-late-join-segment"
	walPath := "/wal/10.0.0.1+9800/test-late-join-segment"
	originator := "tserver-0"

	// Create segment on originator
	seg, err := origMgr.Create(segID, walPath, originator)
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	// Write 10 entries with NO peers
	for i := 1; i <= 10; i++ {
		data := []byte(fmt.Sprintf("early-entry-%03d", i))
		if _, _, err := seg.Write(data, uint64(i)); err != nil {
			t.Fatalf("write %d: %v", i, err)
		}
	}
	seg.Fdatasync()
	earlyOffset := seg.Offset()
	t.Logf("originator has %d bytes before peer joins", earlyOffset)

	// NOW start the peer (simulating late startup)
	port, stop := startPeerServer(t, peerMgr, logger)
	defer stop()

	peer := replication.NewPeerClient(fmt.Sprintf("127.0.0.1:%d", port), logger)
	defer peer.Close()

	// Wait for peer to be healthy
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if peer.IsHealthy() {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}

	// PrepareSegment on the late peer
	if err := peer.PrepareSegment(ctx, segID, walPath, originator); err != nil {
		t.Fatalf("prepare late peer: %v", err)
	}

	// Replay: read originator's segment file and send to peer
	origData, err := os.ReadFile(seg.FilePath())
	if err != nil {
		t.Fatalf("read originator file: %v", err)
	}
	if err := peer.ReplicateEntry(ctx, segID, walPath, originator, origData, 0, 0); err != nil {
		t.Fatalf("replay to peer: %v", err)
	}
	t.Log("replayed early data to late peer")

	// Write 10 more entries — both originator and peer
	for i := 11; i <= 20; i++ {
		data := []byte(fmt.Sprintf("late-entry-%03d", i))
		if _, _, err := seg.Write(data, uint64(i)); err != nil {
			t.Fatalf("write %d: %v", i, err)
		}
		if err := peer.ReplicateEntry(ctx, segID, walPath, originator, data, seg.Offset()-int64(len(data)), uint64(i)); err != nil {
			t.Errorf("replicate %d to peer: %v", i, err)
		}
	}
	seg.Fdatasync()

	t.Logf("originator has %d bytes after late writes", seg.Offset())

	// Verify peer has all the data
	peerFile := fmt.Sprintf("%s/%s.wal", peerDir, segID)
	info, err := os.Stat(peerFile)
	if err != nil {
		t.Fatalf("peer file not found: %v", err)
	}
	if info.Size() != seg.Offset() {
		t.Errorf("peer size %d != originator size %d", info.Size(), seg.Offset())
	} else {
		t.Logf("peer caught up: %d bytes matching originator", info.Size())
	}

	// Seal and verify checksums match
	checksum, size, err := seg.Seal()
	if err != nil {
		t.Fatalf("seal: %v", err)
	}
	match, err := peer.SealSegment(ctx, segID, size, checksum)
	if err != nil {
		t.Fatalf("seal peer: %v", err)
	}
	if !match {
		t.Error("peer checksum mismatch after late join + replay")
	} else {
		t.Log("checksums match — late join recovery successful")
	}
}

// TestQuorumReplication_PeerDiesAndRecovers tests:
// 1. Write with quorum (all 3)
// 2. Kill one peer
// 3. Continue writing (degraded to 2-of-3)
// 4. Verify surviving peer has all data
func TestQuorumReplication_PeerDies(t *testing.T) {
	logger := slog.Default()

	dirs := make([]string, 3)
	mgrs := make([]*segment.Manager, 3)
	for i := 0; i < 3; i++ {
		dirs[i] = t.TempDir()
		mgrs[i] = segment.NewManager(dirs[i], logger)
	}

	port1, stop1 := startPeerServer(t, mgrs[1], logger)
	defer stop1()
	port2, stop2 := startPeerServer(t, mgrs[2], logger)

	peer1 := replication.NewPeerClient(fmt.Sprintf("127.0.0.1:%d", port1), logger)
	defer peer1.Close()
	peer2 := replication.NewPeerClient(fmt.Sprintf("127.0.0.1:%d", port2), logger)

	ctx := context.Background()
	segID := "test-peer-dies"
	walPath := "/wal/10.0.0.1+9800/test-peer-dies"

	seg, _ := mgrs[0].Create(segID, walPath, "tserver-0")
	peer1.PrepareSegment(ctx, segID, walPath, "tserver-0")
	peer2.PrepareSegment(ctx, segID, walPath, "tserver-0")

	// Write 10 entries to all 3
	for i := 1; i <= 10; i++ {
		data := []byte(fmt.Sprintf("entry-%03d", i))
		seg.Write(data, uint64(i))
		peer1.ReplicateEntry(ctx, segID, walPath, "tserver-0", data, seg.Offset()-int64(len(data)), uint64(i))
		peer2.ReplicateEntry(ctx, segID, walPath, "tserver-0", data, seg.Offset()-int64(len(data)), uint64(i))
	}
	seg.Fdatasync()
	t.Logf("all 3 have %d bytes after first batch", seg.Offset())

	// Kill peer 2 — close client first so GracefulStop doesn't block on open streams
	peer2.Close()
	stop2()
	t.Log("peer 2 killed")

	// Write 10 more entries — peer 1 should still work
	for i := 11; i <= 20; i++ {
		data := []byte(fmt.Sprintf("entry-%03d", i))
		seg.Write(data, uint64(i))
		err := peer1.ReplicateEntry(ctx, segID, walPath, "tserver-0", data, seg.Offset()-int64(len(data)), uint64(i))
		if err != nil {
			t.Errorf("peer 1 should still work: %v", err)
		}
	}
	seg.Fdatasync()
	t.Logf("originator has %d bytes after peer 2 death", seg.Offset())

	// Verify peer 1 (survivor) has all data
	peerFile := fmt.Sprintf("%s/%s.wal", dirs[1], segID)
	info, _ := os.Stat(peerFile)
	if info.Size() != seg.Offset() {
		t.Errorf("surviving peer size %d != originator %d", info.Size(), seg.Offset())
	} else {
		t.Logf("surviving peer has all %d bytes — recovery would work", info.Size())
	}
}
