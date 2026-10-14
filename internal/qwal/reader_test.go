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

// reader_test.go drives the QWAL reader against in-process fake peer
// sidecars (bufconn — no real TCP) so the gRPC call pattern, the
// quorum-truncate decision wiring, and the SegmentChunk -> entry-stream
// adapter are all exercised end to end.
package qwal

import (
	"context"
	"errors"
	"io"
	"net"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/test/bufconn"

	pb "github.com/accumulo/wal-quorum-sidecar/proto/qwalpb"
)

// fakePeer is an in-process WalQuorumPeer sidecar: it holds one segment
// replica (uuid + bytes + sealed flag) and serves ListSegments / ReadSegment.
type fakePeer struct {
	pb.UnimplementedWalQuorumPeerServer
	uuid   string
	bytes  []byte
	sealed bool
	// missing makes the peer report it holds no segments at all.
	missing bool
}

func (f *fakePeer) ListSegments(ctx context.Context, _ *pb.ListSegmentsRequest) (*pb.ListSegmentsResponse, error) {
	if f.missing {
		return &pb.ListSegmentsResponse{}, nil
	}
	return &pb.ListSegmentsResponse{
		Segments: []*pb.SegmentInfo{{
			SegmentId: &pb.SegmentId{Uuid: f.uuid},
			Size:      int64(len(f.bytes)),
			Sealed:    f.sealed,
		}},
	}, nil
}

func (f *fakePeer) ReadSegment(req *pb.ReadSegmentRequest, stream grpc.ServerStreamingServer[pb.SegmentChunk]) error {
	data := f.bytes
	if req.GetOffset() > 0 {
		data = data[req.GetOffset():]
	}
	// Deliberately stream the FULL replica even when MaxBytes is smaller, so
	// the test proves chunkReader enforces the quorum ceiling defensively.
	const chunkSz = 7
	for off := 0; off < len(data); off += chunkSz {
		end := off + chunkSz
		if end > len(data) {
			end = len(data)
		}
		if err := stream.Send(&pb.SegmentChunk{
			Data:   data[off:end],
			Offset: int64(off),
			Last:   end == len(data),
		}); err != nil {
			return err
		}
	}
	return nil
}

// peerHarness wires a set of named fake peers onto bufconn listeners and
// hands back a PeerDialer that routes by address.
type peerHarness struct {
	conns map[string]*bufconn.Listener
	srvs  []*grpc.Server
}

func newPeerHarness(t *testing.T, peers map[string]*fakePeer) *peerHarness {
	t.Helper()
	h := &peerHarness{conns: make(map[string]*bufconn.Listener)}
	for addr, fp := range peers {
		lis := bufconn.Listen(1 << 20)
		srv := grpc.NewServer()
		pb.RegisterWalQuorumPeerServer(srv, fp)
		go func() { _ = srv.Serve(lis) }()
		h.conns[addr] = lis
		h.srvs = append(h.srvs, srv)
	}
	t.Cleanup(func() {
		for _, s := range h.srvs {
			s.Stop()
		}
	})
	return h
}

func (h *peerHarness) dialer() PeerDialer {
	return func(ctx context.Context, addr string) (grpc.ClientConnInterface, io.Closer, error) {
		lis, ok := h.conns[addr]
		if !ok {
			return nil, nil, errors.New("no such fake peer: " + addr)
		}
		cc, err := grpc.NewClient("passthrough:///bufnet",
			grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) {
				return lis.DialContext(ctx)
			}),
			grpc.WithTransportCredentials(insecure.NewCredentials()),
		)
		if err != nil {
			return nil, nil, err
		}
		return cc, cc, nil
	}
}

func TestReaderResolveQuorumAgreed(t *testing.T) {
	fixture := buildFixture(t)
	peers := map[string]*fakePeer{
		"ts0:9710": {uuid: "seg-1", bytes: fixture, sealed: false},
		"ts1:9710": {uuid: "seg-1", bytes: fixture, sealed: false},
		"ts2:9710": {uuid: "seg-1", bytes: fixture, sealed: false},
	}
	h := newPeerHarness(t, peers)
	r := NewReaderWithDialer(h.dialer())

	res, err := r.Resolve(context.Background(),
		[]string{"ts0:9710", "ts1:9710", "ts2:9710"}, "seg-1")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if res.Agreement.Outcome != ReadyToSeal {
		t.Fatalf("want ReadyToSeal, got %v", res.Agreement.Outcome)
	}
	if res.Agreement.AgreedSize != int64(len(fixture)) {
		t.Fatalf("agreed size: want %d, got %d", len(fixture), res.Agreement.AgreedSize)
	}
	if res.SourcePeer == "" {
		t.Fatal("expected a source peer to be chosen")
	}
}

func TestReaderOpenStreamsDecodedEntries(t *testing.T) {
	fixture := buildFixture(t)
	peers := map[string]*fakePeer{
		"ts0:9710": {uuid: "seg-1", bytes: fixture},
		"ts1:9710": {uuid: "seg-1", bytes: fixture},
		"ts2:9710": {uuid: "seg-1", bytes: fixture},
	}
	h := newPeerHarness(t, peers)
	r := NewReaderWithDialer(h.dialer())

	stream, err := r.Open(context.Background(),
		[]string{"ts0:9710", "ts1:9710", "ts2:9710"}, "seg-1", "/accumulo/wal/ts0/seg-1")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer stream.Close()

	var events []LogEvent
	for {
		e, err := stream.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("Next: %v", err)
		}
		events = append(events, e.Key.Event)
	}
	wantEvents := []LogEvent{EventOpen, EventDefineTablet, EventMutation, EventManyMutations}
	if len(events) != len(wantEvents) {
		t.Fatalf("event count: want %d, got %d (%v)", len(wantEvents), len(events), events)
	}
	for i := range wantEvents {
		if events[i] != wantEvents[i] {
			t.Errorf("event %d: want %v, got %v", i, wantEvents[i], events[i])
		}
	}
}

func TestReaderQuorumTruncateBoundsStream(t *testing.T) {
	fixture := buildFixture(t)
	// ts0 holds extra uncommitted bytes past the quorum-agreed length; ts1 and
	// ts2 agree on the true length. The reader must (a) pick QuorumTruncate,
	// (b) bound the stream at the agreed size, (c) still decode every entry
	// since the agreed size lands on an entry boundary.
	surplus := append(append([]byte{}, fixture...), []byte("UNCOMMITTED-TRAILING-GARBAGE")...)
	peers := map[string]*fakePeer{
		"ts0:9710": {uuid: "seg-1", bytes: surplus},
		"ts1:9710": {uuid: "seg-1", bytes: fixture},
		"ts2:9710": {uuid: "seg-1", bytes: fixture},
	}
	h := newPeerHarness(t, peers)
	r := NewReaderWithDialer(h.dialer())

	res, err := r.Resolve(context.Background(),
		[]string{"ts0:9710", "ts1:9710", "ts2:9710"}, "seg-1")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if res.Agreement.Outcome != QuorumTruncate {
		t.Fatalf("want QuorumTruncate, got %v", res.Agreement.Outcome)
	}
	if res.Agreement.AgreedSize != int64(len(fixture)) {
		t.Fatalf("agreed size: want %d, got %d", len(fixture), res.Agreement.AgreedSize)
	}
	// The chosen source peer must be one at the agreed size, never ts0.
	if res.SourcePeer == "ts0:9710" {
		t.Fatalf("source peer must not be the over-long replica ts0")
	}

	stream, err := r.Open(context.Background(),
		[]string{"ts0:9710", "ts1:9710", "ts2:9710"}, "seg-1", "/accumulo/wal/ts0/seg-1")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer stream.Close()
	count := 0
	for {
		_, err := stream.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("Next: %v (truncation should land on an entry boundary)", err)
		}
		count++
	}
	if count != 4 {
		t.Fatalf("want 4 entries after quorum-truncate, got %d", count)
	}
}

func TestReaderNoPeers(t *testing.T) {
	peers := map[string]*fakePeer{
		"ts0:9710": {missing: true},
		"ts1:9710": {missing: true},
	}
	h := newPeerHarness(t, peers)
	r := NewReaderWithDialer(h.dialer())

	_, err := r.Resolve(context.Background(), []string{"ts0:9710", "ts1:9710"}, "seg-1")
	if !errors.Is(err, ErrSegmentLost) {
		t.Fatalf("want ErrSegmentLost, got %v", err)
	}
}

func TestReaderSealedDivergence(t *testing.T) {
	a := []byte("aaaaaaaaaa")          // 10 bytes, sealed
	b := []byte("bbbbbbb")             // 7 bytes, unsealed quorum
	peers := map[string]*fakePeer{
		"ts0:9710": {uuid: "seg-1", bytes: a, sealed: true},
		"ts1:9710": {uuid: "seg-1", bytes: b},
		"ts2:9710": {uuid: "seg-1", bytes: b},
	}
	h := newPeerHarness(t, peers)
	r := NewReaderWithDialer(h.dialer())

	_, err := r.Resolve(context.Background(),
		[]string{"ts0:9710", "ts1:9710", "ts2:9710"}, "seg-1")
	if !errors.Is(err, ErrDivergence) {
		t.Fatalf("want ErrDivergence, got %v", err)
	}
}

func TestReaderUnreachablePeerDroppedNotFatal(t *testing.T) {
	fixture := buildFixture(t)
	// Only two peers are wired; the third address has no listener. Resolve
	// should drop the unreachable peer and still reach a 2-of-2 quorum.
	peers := map[string]*fakePeer{
		"ts0:9710": {uuid: "seg-1", bytes: fixture},
		"ts1:9710": {uuid: "seg-1", bytes: fixture},
	}
	h := newPeerHarness(t, peers)
	r := NewReaderWithDialer(h.dialer())

	res, err := r.Resolve(context.Background(),
		[]string{"ts0:9710", "ts1:9710", "ts2-unreachable:9710"}, "seg-1")
	if err != nil {
		t.Fatalf("Resolve with one unreachable peer should still succeed: %v", err)
	}
	if res.Agreement.Outcome != ReadyToSeal {
		t.Fatalf("want ReadyToSeal from the 2 reachable peers, got %v", res.Agreement.Outcome)
	}
	if len(res.Peers) != 2 {
		t.Fatalf("want 2 peer views, got %d", len(res.Peers))
	}
}
