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

package server_test

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"net"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	"github.com/accumulo/wal-quorum-sidecar/internal/config"
	"github.com/accumulo/wal-quorum-sidecar/internal/replication"
	"github.com/accumulo/wal-quorum-sidecar/internal/segment"
	"github.com/accumulo/wal-quorum-sidecar/internal/server"
	pb "github.com/accumulo/wal-quorum-sidecar/proto/qwalpb"
)

// startPeer starts a peer (sidecar-to-sidecar) gRPC server backed by its own
// segment manager, and returns its dial address.
func startPeer(t *testing.T, logger *slog.Logger) (string, *segment.Manager) {
	t.Helper()

	mgr := segment.NewManager(t.TempDir(), logger)
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}

	srv := grpc.NewServer()
	server.NewPeerServer(mgr, logger).Register(srv)
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(srv.Stop)

	return lis.Addr().String(), mgr
}

// startLocal starts a LocalServer (the service the TServer talks to) over
// loopback TCP with the given peers, and returns a connected client.
func startLocal(t *testing.T, peerAddrs []string) (pb.WalQuorumLocalClient, *segment.Manager) {
	return startLocalWithPeerWait(t, peerAddrs, 0)
}

// startLocalWithPeerWait is startLocal with an explicit first-segment peer
// readiness timeout (0 = the production default).
func startLocalWithPeerWait(t *testing.T, peerAddrs []string, peerWait time.Duration) (pb.WalQuorumLocalClient, *segment.Manager) {
	t.Helper()

	logger := slog.Default()
	mgr := segment.NewManager(t.TempDir(), logger)
	pool := replication.NewPeerPoolFromAddresses(peerAddrs, logger)
	quorum := replication.NewQuorumWriter(pool, 0, logger)

	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	srv := grpc.NewServer()
	local := server.NewLocalServer(mgr, &config.Config{PodName: "tserver-2"}, pool, quorum, nil, logger)
	if peerWait > 0 {
		local.SetPeerReadyTimeout(peerWait)
	}
	local.Register(srv)
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(srv.Stop)
	t.Cleanup(pool.Close)

	conn, err := grpc.NewClient(lis.Addr().String(),
		grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("dial local server: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })

	return pb.NewWalQuorumLocalClient(conn), mgr
}

// startPeerAt starts a peer server on a specific address, for tests that need
// a peer to appear after the originator has already opened its segment.
func startPeerAt(t *testing.T, addr string, logger *slog.Logger) *segment.Manager {
	t.Helper()

	mgr := segment.NewManager(t.TempDir(), logger)
	lis, err := net.Listen("tcp", addr)
	if err != nil {
		t.Fatalf("listen on %s: %v", addr, err)
	}

	srv := grpc.NewServer()
	server.NewPeerServer(mgr, logger).Register(srv)
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(srv.Stop)

	return mgr
}

// freeAddress returns a loopback address that nothing is listening on.
func freeAddress(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := l.Addr().String()
	_ = l.Close()
	return addr
}

// silentListener accepts connections and then says nothing, standing in for a
// peer that is reachable but wedged.
func silentListener(t *testing.T) string {
	t.Helper()

	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}

	var (
		mu    sync.Mutex
		conns []net.Conn
	)
	t.Cleanup(func() {
		_ = l.Close()
		mu.Lock()
		defer mu.Unlock()
		for _, c := range conns {
			_ = c.Close()
		}
		conns = nil
	})

	go func() {
		for {
			conn, err := l.Accept()
			if err != nil {
				return
			}
			// Hold the connection open without speaking HTTP/2.
			mu.Lock()
			conns = append(conns, conn)
			mu.Unlock()
		}
	}()

	return l.Addr().String()
}

func openReq(segID, walPath, originator string) *pb.OpenSegmentRequest {
	return &pb.OpenSegmentRequest{
		SegmentId:         &pb.SegmentId{Uuid: segID, WalPath: walPath},
		OriginatorPod:     originator,
		ReplicationFactor: 3,
	}
}

// TestOpenSegmentDoubleOpenIsIdempotent reproduces the write outage this fix
// was written for.
//
// The TServer-side WAL open is retried for the SAME segment UUID
// (VolumeManagerImpl.createSyncable catches any exception from its first
// fs.create and immediately re-calls fs.create on the same path). The first
// open had already created the segment inside the sidecar and prepared it on
// both peers, so the retry hit "segment <id> already exists" and every WAL
// write on that TServer wedged.
//
// A repeat open of a live segment by the same owner must be a no-op that hands
// back the existing segment, not an error.
func TestOpenSegmentDoubleOpenIsIdempotent(t *testing.T) {
	peer1, peerMgr1 := startPeer(t, slog.Default())
	peer2, peerMgr2 := startPeer(t, slog.Default())
	client, mgr := startLocal(t, []string{peer1, peer2})

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	const (
		segID      = "2648432f-0000-4000-8000-000000000001"
		walPath    = "/accumulo/wal/tserver-2+9997/" + segID
		originator = "tserver-2"
	)

	first, err := client.OpenSegment(ctx, openReq(segID, walPath, originator))
	if err != nil {
		t.Fatalf("first OpenSegment RPC failed: %v", err)
	}
	if !first.GetSuccess() {
		t.Fatalf("first OpenSegment failed: %s", first.GetError())
	}
	t.Logf("first open ok, prepared_peers=%v", first.GetPreparedPeers())

	// The retry: same segment id, same WAL path, same originator pod.
	second, err := client.OpenSegment(ctx, openReq(segID, walPath, originator))
	if err != nil {
		t.Fatalf("second OpenSegment RPC failed: %v", err)
	}
	if !second.GetSuccess() {
		t.Fatalf("second OpenSegment for the SAME live segment must be a no-op, got error: %q",
			second.GetError())
	}

	if len(second.GetPreparedPeers()) != len(first.GetPreparedPeers()) {
		t.Errorf("retry must report the same peer set: first=%v second=%v",
			first.GetPreparedPeers(), second.GetPreparedPeers())
	}

	// The re-open must not have disturbed the segment or the replicas.
	seg := mgr.Get(segID)
	if seg == nil {
		t.Fatal("segment disappeared from the manager after the repeat open")
	}
	if seg.IsSealed() {
		t.Error("segment must not be sealed by a repeat open")
	}
	if seg.OriginatorPod() != originator {
		t.Errorf("originator changed: got %q want %q", seg.OriginatorPod(), originator)
	}
	for i, pm := range []*segment.Manager{peerMgr1, peerMgr2} {
		if pm.Get(segID) == nil {
			t.Errorf("peer %d lost its replica after the repeat open", i+1)
		}
	}

	// And writes must still complete — this is what hung for 90 minutes.
	stream, err := client.WriteEntries(ctx)
	if err != nil {
		t.Fatalf("open WriteEntries stream: %v", err)
	}
	if err := stream.Send(&pb.WriteEntryRequest{
		SegmentId:   &pb.SegmentId{Uuid: segID, WalPath: walPath},
		Data:        []byte("post-reopen-entry"),
		SequenceNum: 1,
	}); err != nil {
		t.Fatalf("send entry after repeat open: %v", err)
	}
	ack, err := stream.Recv()
	if err != nil {
		t.Fatalf("no ack after repeat open (writes are wedged): %v", err)
	}
	if ack.GetAckedSequenceNum() != 1 {
		t.Errorf("acked seq = %d, want 1", ack.GetAckedSequenceNum())
	}
	_ = stream.CloseSend()
}

// TestOpenSegmentRejectsDifferentOriginator locks in the other half of the
// contract: idempotency is only for the same owner. A segment id claimed by a
// different originator pod must be refused, never silently adopted.
func TestOpenSegmentRejectsDifferentOriginator(t *testing.T) {
	peer1, _ := startPeer(t, slog.Default())
	client, _ := startLocal(t, []string{peer1})

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	const (
		segID   = "2648432f-0000-4000-8000-000000000002"
		walPath = "/accumulo/wal/tserver-2+9997/" + segID
	)

	first, err := client.OpenSegment(ctx, openReq(segID, walPath, "tserver-2"))
	if err != nil {
		t.Fatalf("first OpenSegment RPC failed: %v", err)
	}
	if !first.GetSuccess() {
		t.Fatalf("first OpenSegment failed: %s", first.GetError())
	}

	stolen, err := client.OpenSegment(ctx, openReq(segID, walPath, "tserver-9"))
	if err != nil {
		t.Fatalf("second OpenSegment RPC failed: %v", err)
	}
	if stolen.GetSuccess() {
		t.Fatal("a segment owned by another originator must NOT be adopted")
	}
	t.Logf("correctly refused foreign claim: %s", stolen.GetError())
}

// TestOpenSegmentDoesNotStallOnHealthyPeers covers the second half of the
// incident: the first OpenSegment on a sidecar blocks in a "wait for a
// reachable peer" loop that polls PeerClient.IsHealthy(). Peer connections are
// lazy, so nothing has dialed them yet and IsHealthy() is false for every peer
// — the loop burns its full timeout and logs "no peers reachable ...,
// proceeding in degraded mode" even when every peer is up and answering.
//
// Opening the first WAL segment must not take tens of seconds when the peers
// are reachable.
func TestOpenSegmentDoesNotStallOnHealthyPeers(t *testing.T) {
	peer1, _ := startPeer(t, slog.Default())
	peer2, _ := startPeer(t, slog.Default())
	client, _ := startLocal(t, []string{peer1, peer2})

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	segID := "2648432f-0000-4000-8000-000000000003"
	walPath := fmt.Sprintf("/accumulo/wal/tserver-2+9997/%s", segID)

	start := time.Now()
	resp, err := client.OpenSegment(ctx, openReq(segID, walPath, "tserver-2"))
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("OpenSegment RPC failed: %v", err)
	}
	if !resp.GetSuccess() {
		t.Fatalf("OpenSegment failed: %s", resp.GetError())
	}

	t.Logf("first OpenSegment took %s", elapsed)
	if elapsed > 5*time.Second {
		t.Errorf("first OpenSegment took %s with both peers reachable; "+
			"the peer-readiness wait is not actually dialing the peers", elapsed)
	}
}

// TestOpenSegmentProceedsWhenPeersAreDown checks the containment direction:
// unreachable peers must not block the local WAL open beyond the readiness
// timeout. The open proceeds in degraded mode and the peers are caught up in
// the background.
func TestOpenSegmentProceedsWhenPeersAreDown(t *testing.T) {
	// Bind then immediately release two ports so nothing is listening there.
	dead := make([]string, 0, 2)
	for i := 0; i < 2; i++ {
		l, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatalf("listen: %v", err)
		}
		dead = append(dead, l.Addr().String())
		_ = l.Close()
	}

	client, mgr := startLocalWithPeerWait(t, dead, 250*time.Millisecond)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	segID := "2648432f-0000-4000-8000-000000000004"
	walPath := "/accumulo/wal/tserver-2+9997/" + segID

	start := time.Now()
	resp, err := client.OpenSegment(ctx, openReq(segID, walPath, "tserver-2"))
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("OpenSegment RPC failed: %v", err)
	}
	if !resp.GetSuccess() {
		t.Fatalf("open must succeed in degraded mode, got: %s", resp.GetError())
	}
	if mgr.Get(segID) == nil {
		t.Error("segment should exist locally after a degraded open")
	}
	if elapsed > 10*time.Second {
		t.Errorf("degraded open took %s; unreachable peers are blocking the WAL open", elapsed)
	}
	t.Logf("degraded open completed in %s with %d peers reported",
		elapsed, len(resp.GetPreparedPeers()))
}

// TestLatePeerIsCaughtUpBeforeSeal is the end-to-end version of the gap
// problem. The peer is down while the first entries are written, so those
// entries never reach it. Replication must not simply resume when the peer
// comes back: the replica would be missing that range, and the divergence
// would only surface when the seal compares sizes and checksums. The
// originator has to replay what the replica is missing first.
func TestLatePeerIsCaughtUpBeforeSeal(t *testing.T) {
	peerAddr := freeAddress(t)
	client, mgr := startLocalWithPeerWait(t, []string{peerAddr}, 250*time.Millisecond)

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	segID := "2648432f-0000-4000-8000-000000000005"
	walPath := "/accumulo/wal/tserver-2+9997/" + segID

	resp, err := client.OpenSegment(ctx, openReq(segID, walPath, "tserver-2"))
	if err != nil {
		t.Fatalf("OpenSegment RPC failed: %v", err)
	}
	if !resp.GetSuccess() {
		t.Fatalf("OpenSegment failed: %s", resp.GetError())
	}

	stream, err := client.WriteEntries(ctx)
	if err != nil {
		t.Fatalf("WriteEntries: %v", err)
	}
	write := func(seq uint64, payload string) {
		t.Helper()
		if err := stream.Send(&pb.WriteEntryRequest{
			SegmentId:   &pb.SegmentId{Uuid: segID, WalPath: walPath},
			Data:        []byte(payload),
			SequenceNum: seq,
		}); err != nil {
			t.Fatalf("send entry %d: %v", seq, err)
		}
		if _, err := stream.Recv(); err != nil {
			t.Fatalf("ack for entry %d: %v", seq, err)
		}
	}

	// Written while the peer is down — these are the bytes the replica misses.
	for i := uint64(1); i <= 3; i++ {
		write(i, fmt.Sprintf("entry-%d-written-while-the-peer-was-down;", i))
	}

	// The peer comes up; the background prepare retry catches it up.
	peerMgr := startPeerAt(t, peerAddr, slog.Default())

	deadline := time.Now().Add(60 * time.Second)
	for {
		if peerSeg := peerMgr.Get(segID); peerSeg != nil && peerSeg.Offset() == mgr.Get(segID).Offset() {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("peer replica was never caught up after the peer came back")
		}
		time.Sleep(200 * time.Millisecond)
	}

	// Writes after the catch-up must land on the replica too.
	for i := uint64(4); i <= 6; i++ {
		write(i, fmt.Sprintf("entry-%d-written-after-the-peer-returned;", i))
	}
	if err := stream.CloseSend(); err != nil {
		t.Fatalf("close send: %v", err)
	}

	closeResp, err := client.CloseSegment(ctx, &pb.CloseSegmentRequest{
		SegmentId: &pb.SegmentId{Uuid: segID, WalPath: walPath},
	})
	if err != nil {
		t.Fatalf("CloseSegment RPC failed: %v", err)
	}
	if !closeResp.GetSuccess() {
		t.Fatalf("CloseSegment failed: %s", closeResp.GetError())
	}

	local := mgr.Get(segID)
	replica := peerMgr.Get(segID)
	if replica == nil {
		t.Fatal("peer never created the replica")
	}
	if replica.Offset() != local.Offset() {
		t.Fatalf("replica holds %d bytes, originator sealed %d — the entries "+
			"written while the peer was down were never replayed",
			replica.Offset(), local.Offset())
	}
	if !bytes.Equal(replica.FinalChecksum(), local.FinalChecksum()) {
		t.Error("replica checksum diverged from the originator's")
	}
}
