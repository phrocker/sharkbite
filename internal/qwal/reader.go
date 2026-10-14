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

// reader.go is the Phase W0 QWAL reader: given a segment UUID and the set of
// peer sidecar addresses, it queries ListSegments on each peer, runs the
// quorum-truncate decision (quorum.go), then ReadSegment-streams the agreed
// bytes from a peer at the quorum-agreed length and surfaces the segment as a
// stream of decoded WAL entries (logfile.go).
//
// The gRPC call pattern mirrors QuorumWalRecoveryReader.java: query peers,
// pick the best replica, stream via the WalQuorumPeer service.
package qwal

import (
	"context"
	"errors"
	"fmt"
	"io"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	pb "github.com/accumulo/wal-quorum-sidecar/proto/qwalpb"
)

// DefaultPeerPort is the gRPC port of the peer sidecar's WalQuorumPeer
// service. It mirrors QuorumWalFileSystem.DEFAULT_PEER_PORT
// (server/pvc-fs/.../QuorumWalFileSystem.java) — the fallback port used
// when a metadata log: entry records a peer address without ":port".
const DefaultPeerPort = 9710

// ErrSegmentLost is returned when no peer holds a replica of the segment —
// the NO_PEERS outcome. The caller decides whether to mark the WAL lost.
var ErrSegmentLost = errors.New("qwal: segment not found on any peer sidecar")

// ErrDivergence is returned for the SEALED_DIVERGENCE outcome: committed
// bytes diverge across peers and there is no safe automatic resolution.
// Distinct from a retryable result so callers don't loop on it.
var ErrDivergence = errors.New("qwal: sealed-replica size divergence; operator action required")

// PeerDialer opens a gRPC connection to a peer sidecar address. Injected so
// tests can supply an in-memory bufconn dialer instead of real TCP.
type PeerDialer func(ctx context.Context, addr string) (grpc.ClientConnInterface, io.Closer, error)

// defaultDialer dials a peer over plaintext TCP — the sidecar peer service
// runs without TLS inside the pod network (QuorumWalLogCloser uses
// usePlaintext() too).
func defaultDialer(ctx context.Context, addr string) (grpc.ClientConnInterface, io.Closer, error) {
	cc, err := grpc.NewClient(addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return nil, nil, fmt.Errorf("qwal: dial peer %s: %w", addr, err)
	}
	return cc, cc, nil
}

// Reader reads a single in-flight WAL segment across a quorum of peer
// sidecars. It is stateless beyond its config and safe to reuse.
type Reader struct {
	dialer PeerDialer
}

// NewReader builds a Reader that dials peers over plaintext TCP.
func NewReader() *Reader {
	return &Reader{dialer: defaultDialer}
}

// NewReaderWithDialer builds a Reader with a custom peer dialer (tests).
func NewReaderWithDialer(d PeerDialer) *Reader {
	return &Reader{dialer: d}
}

// SegmentResult is the outcome of resolving a segment across peers: the
// quorum decision plus the per-peer views it was computed from.
type SegmentResult struct {
	Agreement AgreementResult
	Peers     []PeerView
	// SourcePeer is the address the bytes were (or would be) streamed from —
	// a peer holding a replica at the quorum-agreed size. Empty for NoPeers.
	SourcePeer string
}

// Resolve queries every peer's view of the segment and runs the
// quorum-truncate decision. It does not stream bytes — Open does that — so a
// caller can inspect the outcome (retryable vs hard error) first.
//
// peerAddrs are "host:port" sidecar peer addresses. segmentUUID is the WAL
// segment UUID (SegmentId.uuid). Peers that fail to answer are simply omitted
// from the view set, exactly as QuorumWalLogCloser does — a missing peer is
// not the same as a peer reporting a divergent size.
func (r *Reader) Resolve(ctx context.Context, peerAddrs []string, segmentUUID string) (SegmentResult, error) {
	var views []PeerView
	for _, addr := range peerAddrs {
		view, ok, err := r.queryPeer(ctx, addr, segmentUUID)
		if err != nil {
			// A peer we couldn't reach is dropped from the quorum set, not
			// treated as divergence. Surfacing the error would turn a single
			// flaky peer into a hard failure.
			continue
		}
		if ok {
			views = append(views, view)
		}
	}

	agreement := EvaluatePeerAgreement(views)
	res := SegmentResult{Agreement: agreement, Peers: views}

	switch agreement.Outcome {
	case NoPeers:
		return res, ErrSegmentLost
	case SealedDivergence:
		return res, fmt.Errorf("%w: peers=%v", ErrDivergence, views)
	case UnsealedDivergence:
		// Retryable — surface a sentinel-free error the caller can re-attempt.
		return res, fmt.Errorf("qwal: no quorum on segment %s (retryable): peers=%v",
			segmentUUID, views)
	case SealedAgreed, ReadyToSeal, QuorumTruncate:
		// Pick a peer holding a replica AT the agreed size. For QuorumTruncate
		// this is mandatory (a peer past the agreed size would stream surplus
		// uncommitted bytes); for the others every peer is at the agreed size
		// anyway, so the same filter is harmless.
		for _, v := range views {
			if v.Size == agreement.AgreedSize {
				res.SourcePeer = v.Addr
				break
			}
		}
		if res.SourcePeer == "" {
			// Quorum says a size exists but no single peer is at it — should
			// be unreachable given the agreement math, but fail loud rather
			// than stream a wrong-length replica.
			return res, fmt.Errorf("qwal: no peer at agreed size %d for segment %s",
				agreement.AgreedSize, segmentUUID)
		}
		return res, nil
	default:
		return res, fmt.Errorf("qwal: unexpected agreement outcome %v", agreement.Outcome)
	}
}

// queryPeer calls ListSegments on one peer and returns that peer's view of
// the named segment. ok is false when the peer simply doesn't hold the
// segment (not an error).
func (r *Reader) queryPeer(ctx context.Context, addr, segmentUUID string) (PeerView, bool, error) {
	cc, closer, err := r.dialer(ctx, addr)
	if err != nil {
		return PeerView{}, false, err
	}
	defer closer.Close()

	client := pb.NewWalQuorumPeerClient(cc)
	resp, err := client.ListSegments(ctx, &pb.ListSegmentsRequest{})
	if err != nil {
		return PeerView{}, false, fmt.Errorf("qwal: ListSegments on %s: %w", addr, err)
	}
	for _, info := range resp.GetSegments() {
		if info.GetSegmentId().GetUuid() == segmentUUID {
			return PeerView{
				Addr:   addr,
				Size:   info.GetSize(),
				Sealed: info.GetSealed(),
			}, true, nil
		}
	}
	return PeerView{}, false, nil
}

// Open resolves the segment, then streams the quorum-agreed bytes from the
// chosen source peer and returns an EntryStream over the decoded WAL entries.
// For QUORUM_TRUNCATE the stream is bounded at the quorum-agreed size — any
// surplus bytes on the source replica past that point are uncommitted and
// dropped, exactly matching the manager's recovery view.
//
// segmentWALPath is the WAL path within the Accumulo namespace
// (SegmentId.wal_path) — the sidecar needs both the UUID and path to locate
// the replica file.
func (r *Reader) Open(ctx context.Context, peerAddrs []string, segmentUUID, segmentWALPath string) (*EntryStream, error) {
	res, err := r.Resolve(ctx, peerAddrs, segmentUUID)
	if err != nil {
		return nil, err
	}

	cc, closer, err := r.dialer(ctx, res.SourcePeer)
	if err != nil {
		return nil, err
	}

	client := pb.NewWalQuorumPeerClient(cc)
	stream, err := client.ReadSegment(ctx, &pb.ReadSegmentRequest{
		SegmentId: &pb.SegmentId{Uuid: segmentUUID, WalPath: segmentWALPath},
		Offset:    0,
		// MaxBytes bounds the stream at the quorum-agreed size. For
		// QuorumTruncate this discards the minority's uncommitted surplus;
		// for the agreed cases it is just the full segment length.
		MaxBytes: res.Agreement.AgreedSize,
	})
	if err != nil {
		closer.Close()
		return nil, fmt.Errorf("qwal: ReadSegment on %s: %w", res.SourcePeer, err)
	}

	// chunkReader adapts the gRPC server-stream of SegmentChunk into an
	// io.Reader, enforcing the quorum-agreed byte ceiling defensively even if
	// the peer streams past MaxBytes.
	cr := &chunkReader{
		stream:   stream,
		limit:    res.Agreement.AgreedSize,
		closer:   closer,
		fromAddr: res.SourcePeer,
	}
	di := newDataInput(cr)
	if err := di.readHeader(); err != nil {
		cr.Close()
		return nil, err
	}
	return &EntryStream{di: di, raw: cr, result: res}, nil
}

// chunkReader turns the ReadSegment server stream into an io.Reader. It caps
// total bytes delivered at limit (0 means "no cap"), so QUORUM_TRUNCATE is
// honored even if the source sidecar over-streams.
type chunkReader struct {
	stream   grpc.ServerStreamingClient[pb.SegmentChunk]
	closer   io.Closer
	fromAddr string
	limit    int64
	buf      []byte
	served   int64
	done     bool
}

func (c *chunkReader) Read(p []byte) (int, error) {
	for len(c.buf) == 0 {
		if c.done {
			return 0, io.EOF
		}
		if c.limit > 0 && c.served >= c.limit {
			c.done = true
			return 0, io.EOF
		}
		chunk, err := c.stream.Recv()
		if err == io.EOF {
			c.done = true
			return 0, io.EOF
		}
		if err != nil {
			return 0, fmt.Errorf("qwal: ReadSegment recv from %s: %w", c.fromAddr, err)
		}
		data := chunk.GetData()
		if c.limit > 0 {
			remaining := c.limit - c.served
			if int64(len(data)) > remaining {
				data = data[:remaining]
			}
		}
		c.buf = data
		c.served += int64(len(data))
		if chunk.GetLast() {
			c.done = true
		}
	}
	n := copy(p, c.buf)
	c.buf = c.buf[n:]
	return n, nil
}

// Close releases the underlying gRPC connection.
func (c *chunkReader) Close() error {
	if c.closer != nil {
		return c.closer.Close()
	}
	return nil
}

// EntryStream surfaces a resolved QWAL segment as a forward-only stream of
// decoded WAL entries. The W1 in-memory merger consumes this directly.
type EntryStream struct {
	di     *dataInput
	raw    io.Closer
	result SegmentResult
}

// Result returns the quorum decision and per-peer views this stream was
// opened from — useful for logging which peer served and at what length.
func (s *EntryStream) Result() SegmentResult { return s.result }

// Next decodes and returns the next WAL entry. It returns io.EOF at the clean
// end of the (quorum-bounded) segment. A decode error past the header is
// surfaced as-is so the caller can quarantine a corrupt segment.
func (s *EntryStream) Next() (*Entry, error) {
	key, err := s.di.readKey()
	if err != nil {
		if err == io.EOF {
			return nil, io.EOF
		}
		// An unexpected EOF mid-key on a quorum-truncated stream means the
		// agreed size fell inside an entry — the writer-side framing bug class
		// from the 2026-05-04 incident. Surface it distinctly.
		if errors.Is(err, io.ErrUnexpectedEOF) {
			return nil, fmt.Errorf("qwal: truncated WAL entry key (segment ends mid-entry): %w", err)
		}
		return nil, err
	}
	value, err := s.di.readValue()
	if err != nil {
		return nil, fmt.Errorf("qwal: decode value for %s entry: %w", key.Event, err)
	}
	return &Entry{Key: key, Value: value}, nil
}

// Close releases the segment stream's gRPC connection.
func (s *EntryStream) Close() error {
	if s.raw != nil {
		return s.raw.Close()
	}
	return nil
}
