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
	"context"
	"errors"
	"log/slog"
	"os"
	"testing"
	"time"

	"github.com/accumulo/wal-quorum-sidecar/internal/replication"
)

// TestReplicaRefusesGapAndHealsByReplay is the containment the failure breaker
// depends on.
//
// Entries dropped while a peer is failing (or while the breaker has it shed)
// leave the replica short. If replication simply resumed at the next entry the
// peer would either grow a hole or silently append at the wrong offset, and the
// damage would surface only as a checksum failure when the segment is sealed.
// So: the peer refuses an entry that starts past the end of its replica, the
// client refuses to send one at all, and a replay of the missing range is what
// puts the replica back in service.
func TestReplicaRefusesGapAndHealsByReplay(t *testing.T) {
	addr, peerMgr := startPeer(t, slog.Default())

	pc := replication.NewPeerClient(addr, slog.Default())
	defer pc.Close()
	pc.SetReplicateTimeout(5 * time.Second)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	const segID = "2648432f-0000-4000-8000-00000000000a"
	const walPath = "/accumulo/wal/tserver-2+9997/" + segID

	if err := pc.PrepareSegment(ctx, segID, walPath, "tserver-2"); err != nil {
		t.Fatalf("PrepareSegment: %v", err)
	}

	first := []byte("entry-one;")
	if err := pc.ReplicateEntry(ctx, segID, walPath, "tserver-2", first, 0, 1); err != nil {
		t.Fatalf("first entry: %v", err)
	}

	// "entry-two;" never reaches the peer (breaker shed it, say), so the next
	// entry starts past the end of the replica.
	missing := []byte("entry-two;")
	third := []byte("entry-three;")
	gapOffset := int64(len(first) + len(missing))

	if err := pc.ReplicateEntry(ctx, segID, walPath, "tserver-2", third, gapOffset, 3); err == nil {
		t.Fatal("an entry that starts past the end of the replica must be refused")
	}

	// And the client must not quietly resume once the peer looks healthy.
	if err := pc.ReplicateEntry(ctx, segID, walPath, "tserver-2", third, gapOffset, 3); !errors.Is(err, replication.ErrPeerReplicaBehind) {
		t.Fatalf("replication resumed into a short replica, got: %v", err)
	}
	if !pc.NeedsCatchUp(segID) {
		t.Fatal("the replica is short but no catch-up is flagged")
	}

	peerSeg := peerMgr.Get(segID)
	if peerSeg == nil {
		t.Fatal("replica segment missing on the peer")
	}
	if peerSeg.Offset() != int64(len(first)) {
		t.Fatalf("replica holds %d bytes, expected the %d it acked",
			peerSeg.Offset(), len(first))
	}

	// Replaying the missing range (overlapping the byte the peer already has,
	// as a replay from a known-good offset does) heals it exactly once.
	replay := append(append([]byte{}, first...), missing...)
	replay = append(replay, third...)
	if err := pc.CatchUpReplica(ctx, segID, replay, 0); err != nil {
		t.Fatalf("catch-up replay: %v", err)
	}
	if pc.NeedsCatchUp(segID) {
		t.Error("replica still flagged as short after a full replay")
	}

	want := string(replay)
	got, err := os.ReadFile(peerSeg.FilePath())
	if err != nil {
		t.Fatalf("read replica file: %v", err)
	}
	if string(got) != want {
		t.Errorf("replica contents %q, want %q", got, want)
	}

	// Normal replication resumes from the healed offset.
	fourth := []byte("entry-four;")
	if err := pc.ReplicateEntry(ctx, segID, walPath, "tserver-2", fourth,
		int64(len(replay)), 4); err != nil {
		t.Fatalf("replication did not resume after the catch-up: %v", err)
	}
	if peerSeg.Offset() != int64(len(replay)+len(fourth)) {
		t.Errorf("replica is at %d bytes, want %d", peerSeg.Offset(), len(replay)+len(fourth))
	}
}

// TestReplicateEntryBoundsAWedgedPeer covers the peer that completes the TCP
// handshake and then says nothing. The replication stream outlives any one
// entry, so without a bound on the exchange the caller blocks in Recv forever:
// the failure is never recorded, the breaker never opens, and every subsequent
// write both pays the quorum timeout and leaks another blocked goroutine.
func TestReplicateEntryBoundsAWedgedPeer(t *testing.T) {
	addr := silentListener(t)

	pc := replication.NewPeerClient(addr, slog.Default())
	defer pc.Close()
	pc.SetReplicateTimeout(300 * time.Millisecond)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	done := make(chan error, 1)
	go func() {
		done <- pc.ReplicateEntry(ctx, "seg-wedged", "/wal/a/seg-wedged",
			"tserver-2", []byte("entry"), 0, 1)
	}()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("a peer that never answers must not report success")
		}
		t.Logf("wedged peer reported: %v", err)
	case <-time.After(10 * time.Second):
		t.Fatal("replication to a silent peer never returned; the failure can " +
			"never reach the breaker")
	}
}
