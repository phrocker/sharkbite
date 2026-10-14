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

package replication_test

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"testing"
	"time"

	"github.com/accumulo/wal-quorum-sidecar/internal/replication"
)

// deadAddress returns a loopback address with nothing listening on it.
func deadAddress(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := l.Addr().String()
	_ = l.Close()
	return addr
}

// TestPeerBreakerShedsFailingPeer verifies the quorum containment: a peer that
// keeps failing is shed for a cooldown, so the originator stops paying a
// per-entry timeout for it (and stops hammering it) instead of dragging every
// write down with the broken peer.
func TestPeerBreakerShedsFailingPeer(t *testing.T) {
	pc := replication.NewPeerClient(deadAddress(t), slog.Default())
	defer pc.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	replicate := func(seq uint64) error {
		callCtx, callCancel := context.WithTimeout(ctx, 2*time.Second)
		defer callCancel()
		return pc.ReplicateEntry(callCtx, "seg-breaker", "/wal/a/seg-breaker",
			"tserver-2", []byte("entry"), 0, seq)
	}

	// Drive it past the breaker threshold.
	for i := uint64(1); i <= 3; i++ {
		if err := replicate(i); err == nil {
			t.Fatalf("replication to a dead peer should fail (attempt %d)", i)
		}
	}

	// The next call must fail fast, without another dial+timeout.
	start := time.Now()
	err := replicate(4)
	elapsed := time.Since(start)

	if !errors.Is(err, replication.ErrPeerCoolingDown) {
		t.Fatalf("expected the peer to be shed by the breaker, got: %v", err)
	}
	if elapsed > 100*time.Millisecond {
		t.Errorf("shed peer still cost %s per write; the breaker is not fast-failing", elapsed)
	}
	t.Logf("shed peer rejected in %s", elapsed)
}

// TestPeerBreakerRecovers verifies the breaker is not sticky: once the cooldown
// elapses the peer is retried.
func TestPeerBreakerRecovers(t *testing.T) {
	pc := replication.NewPeerClient(deadAddress(t), slog.Default())
	defer pc.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	replicate := func(seq uint64) error {
		callCtx, callCancel := context.WithTimeout(ctx, 2*time.Second)
		defer callCancel()
		return pc.ReplicateEntry(callCtx, "seg-recover", "/wal/a/seg-recover",
			"tserver-2", []byte("entry"), 0, seq)
	}

	for i := uint64(1); i <= 3; i++ {
		_ = replicate(i)
	}
	if err := replicate(4); !errors.Is(err, replication.ErrPeerCoolingDown) {
		t.Fatalf("expected peer to be shed, got: %v", err)
	}

	// After the cooldown the peer must be attempted again (it is still dead
	// here, so the error must be a real replication error, not the breaker).
	time.Sleep(5100 * time.Millisecond)
	err := replicate(5)
	if err == nil {
		t.Fatal("dead peer should still fail after the cooldown")
	}
	if errors.Is(err, replication.ErrPeerCoolingDown) {
		t.Error("breaker stayed open past its cooldown; the peer is never retried")
	}
}
