// Licensed to the Apache Software Foundation (ASF) under one
// or more contributor license agreements.
package recovery

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	pb "github.com/accumulo/wal-quorum-sidecar/proto/qwalpb"
)

// Reader serves WAL segment data for recovery reads. It provides local file
// access for the ReadSegment RPC (in peer_server.go) and peer discovery to
// find the best replica of a segment across the quorum.
type Reader struct {
	logger *slog.Logger
}

// NewReader creates a Reader for serving recovery data.
func NewReader(logger *slog.Logger) *Reader {
	return &Reader{
		logger: logger.With("component", "recovery-reader"),
	}
}

// ReadSegmentFile opens a local segment file, seeks to the given offset, and
// returns a ReadCloser for streaming the data along with the total file size.
// The caller is responsible for closing the returned reader.
//
// This is used by peer_server.go's ReadSegment RPC to stream segment chunks
// back to a recovering TServer.
func (r *Reader) ReadSegmentFile(segmentPath string, offset int64) (io.ReadCloser, int64, error) {
	f, err := os.Open(segmentPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, 0, fmt.Errorf("segment file not found: %s", segmentPath)
		}
		return nil, 0, fmt.Errorf("open segment file %s: %w", segmentPath, err)
	}

	info, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, 0, fmt.Errorf("stat segment file %s: %w", segmentPath, err)
	}

	fileSize := info.Size()

	if offset > 0 {
		if offset > fileSize {
			_ = f.Close()
			return nil, 0, fmt.Errorf("offset %d exceeds file size %d for %s", offset, fileSize, segmentPath)
		}
		if _, err := f.Seek(offset, io.SeekStart); err != nil {
			_ = f.Close()
			return nil, 0, fmt.Errorf("seek to offset %d in %s: %w", offset, segmentPath, err)
		}
	}

	r.logger.Debug("opened segment for recovery read",
		"path", segmentPath,
		"offset", offset,
		"file_size", fileSize,
	)

	return f, fileSize, nil
}

// PeerReplica holds information about a segment replica found on a peer.
type PeerReplica struct {
	// PeerAddr is the gRPC address of the peer holding this replica.
	PeerAddr string
	// Size is the size in bytes of the replica on the peer.
	Size int64
	// Sealed indicates whether the replica has been sealed on the peer.
	Sealed bool
}

// FindBestReplica queries all peer sidecars via ListSegments and returns the
// peer that holds the most complete replica of the requested segment. This is
// used during recovery to determine which peer to read the WAL data from.
//
// The "best" replica is the one with the largest size (most data received
// before failure), with preference given to sealed replicas over open ones.
//
// Returns an error if no peer holds a replica of the segment.
func (r *Reader) FindBestReplica(ctx context.Context, peers []string, segmentID string) (*PeerReplica, error) {
	var best *PeerReplica

	for _, peerAddr := range peers {
		replica, err := r.queryPeer(ctx, peerAddr, segmentID)
		if err != nil {
			r.logger.Debug("peer query failed during replica search",
				"peer", peerAddr,
				"segment_id", segmentID,
				"error", err,
			)
			continue
		}
		if replica == nil {
			continue
		}

		if best == nil {
			best = replica
			continue
		}

		// Prefer sealed replicas; among same seal state, prefer larger size.
		if replica.Sealed && !best.Sealed {
			best = replica
		} else if replica.Sealed == best.Sealed && replica.Size > best.Size {
			best = replica
		}
	}

	if best == nil {
		return nil, fmt.Errorf("no peer holds a replica of segment %s", segmentID)
	}

	r.logger.Info("found best replica for recovery",
		"segment_id", segmentID,
		"peer", best.PeerAddr,
		"size", best.Size,
		"sealed", best.Sealed,
	)

	return best, nil
}

// queryPeer connects to a single peer and looks up segment metadata via
// the ListSegments RPC. Returns nil if the peer does not hold the segment.
func (r *Reader) queryPeer(ctx context.Context, peerAddr, segmentID string) (*PeerReplica, error) {
	conn, err := grpc.NewClient(peerAddr,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		return nil, fmt.Errorf("dial peer %s: %w", peerAddr, err)
	}
	defer conn.Close()

	client := pb.NewWalQuorumPeerClient(conn)
	resp, err := client.ListSegments(ctx, &pb.ListSegmentsRequest{})
	if err != nil {
		return nil, fmt.Errorf("ListSegments RPC to %s: %w", peerAddr, err)
	}

	for _, info := range resp.GetSegments() {
		if info.GetSegmentId().GetUuid() == segmentID {
			return &PeerReplica{
				PeerAddr: peerAddr,
				Size:     info.GetSize(),
				Sealed:   info.GetSealed(),
			}, nil
		}
	}

	return nil, nil
}
