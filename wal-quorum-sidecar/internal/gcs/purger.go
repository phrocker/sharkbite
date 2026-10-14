// Licensed to the Apache Software Foundation (ASF) under one
// or more contributor license agreements.
package gcs

import (
	"context"
	"fmt"
	"log/slog"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	"github.com/accumulo/wal-quorum-sidecar/internal/segment"
	pb "github.com/accumulo/wal-quorum-sidecar/proto/qwalpb"
)

// Purger cleans up local and peer segment replicas after a sealed segment
// has been durably uploaded to GCS. It verifies the GCS upload, sends purge
// requests to peer sidecars, and deletes the local copy.
type Purger struct {
	uploader *Uploader
	mgr      *segment.Manager
	logger   *slog.Logger
}

// NewPurger creates a Purger that uses the given uploader to verify GCS
// objects and the segment manager to delete local files.
func NewPurger(uploader *Uploader, mgr *segment.Manager, logger *slog.Logger) *Purger {
	return &Purger{
		uploader: uploader,
		mgr:      mgr,
		logger:   logger.With("component", "gcs-purger"),
	}
}

// PurgeAfterUpload verifies that a segment exists in GCS, then purges the
// local copy and all peer replicas. The peers list contains gRPC addresses
// of peer sidecars (e.g. "tserver-1.tserver.ns.svc.cluster.local:9710").
//
// Steps:
//  1. Verify the segment exists in GCS (HEAD check via ObjectExists).
//  2. Send PurgeSegment RPC to each peer.
//  3. Delete local segment file via the segment manager.
//  4. Log purge completion.
func (p *Purger) PurgeAfterUpload(ctx context.Context, seg *segment.Segment, gcsObjectPath string, peers []string) error {
	segID := seg.ID()

	// Step 1: Verify segment exists in GCS before purging local copies.
	exists, err := p.uploader.ObjectExists(ctx, gcsObjectPath)
	if err != nil {
		return fmt.Errorf("verify GCS object for segment %s: %w", segID, err)
	}
	if !exists {
		return fmt.Errorf("segment %s not found in GCS at %s, aborting purge", segID, gcsObjectPath)
	}

	p.logger.Info("GCS verification passed, purging replicas",
		"segment_id", segID,
		"gcs_path", gcsObjectPath,
		"peer_count", len(peers),
	)

	// Step 2: Send PurgeSegment to all peers.
	var peerErrors []error
	for _, peerAddr := range peers {
		if err := p.purgePeer(ctx, peerAddr, segID); err != nil {
			p.logger.Warn("failed to purge segment from peer",
				"segment_id", segID,
				"peer", peerAddr,
				"error", err,
			)
			peerErrors = append(peerErrors, fmt.Errorf("peer %s: %w", peerAddr, err))
		} else {
			p.logger.Debug("purged segment from peer",
				"segment_id", segID,
				"peer", peerAddr,
			)
		}
	}

	// Step 3: Delete local segment file.
	if err := p.mgr.Delete(segID); err != nil {
		return fmt.Errorf("delete local segment %s: %w", segID, err)
	}

	p.logger.Info("segment purge complete",
		"segment_id", segID,
		"gcs_path", gcsObjectPath,
		"peer_errors", len(peerErrors),
	)

	// Return first peer error if any, but local purge succeeded.
	if len(peerErrors) > 0 {
		return fmt.Errorf("segment %s purged locally but %d peer purge(s) failed: %w",
			segID, len(peerErrors), peerErrors[0])
	}

	return nil
}

// purgePeer sends a PurgeSegment RPC to a single peer sidecar.
func (p *Purger) purgePeer(ctx context.Context, peerAddr, segmentID string) error {
	conn, err := grpc.NewClient(peerAddr,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		return fmt.Errorf("dial peer %s: %w", peerAddr, err)
	}
	defer conn.Close()

	client := pb.NewWalQuorumPeerClient(conn)
	resp, err := client.PurgeSegment(ctx, &pb.PurgeSegmentRequest{
		SegmentId: &pb.SegmentId{Uuid: segmentID},
	})
	if err != nil {
		return fmt.Errorf("PurgeSegment RPC to %s: %w", peerAddr, err)
	}
	if !resp.GetSuccess() {
		return fmt.Errorf("PurgeSegment rejected by %s: %s", peerAddr, resp.GetError())
	}

	return nil
}

// RunPurgeLoop reads from the uploader's results channel and triggers
// purge for each successfully uploaded segment. This should be run as
// a goroutine. It exits when the results channel is closed.
func (p *Purger) RunPurgeLoop(ctx context.Context, peers []string) {
	p.logger.Info("purge loop started", "peer_count", len(peers))
	for result := range p.uploader.Results() {
		if result.Err != nil {
			// Upload failed; nothing to purge.
			continue
		}

		// Extract the GCS object path from the full gs:// URI.
		gcsObjectPath := stripGCSPrefix(result.GCSPath, p.uploader.bucket)

		seg := p.mgr.Get(result.SegmentID)
		if seg == nil {
			p.logger.Warn("segment not found for purge, may have been purged already",
				"segment_id", result.SegmentID,
			)
			continue
		}

		if err := p.PurgeAfterUpload(ctx, seg, gcsObjectPath, peers); err != nil {
			p.logger.Error("purge after upload failed",
				"segment_id", result.SegmentID,
				"error", err,
			)
		}
	}
	p.logger.Info("purge loop stopped")
}

// stripGCSPrefix converts "gs://bucket/path/to/obj" to "path/to/obj".
func stripGCSPrefix(gcsPath, bucket string) string {
	prefix := fmt.Sprintf("gs://%s/", bucket)
	if len(gcsPath) > len(prefix) && gcsPath[:len(prefix)] == prefix {
		return gcsPath[len(prefix):]
	}
	return gcsPath
}
