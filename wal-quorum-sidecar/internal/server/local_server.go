// Licensed to the Apache Software Foundation (ASF) under one
// or more contributor license agreements.
package server

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/accumulo/wal-quorum-sidecar/internal/config"
	"github.com/accumulo/wal-quorum-sidecar/internal/gcs"
	"github.com/accumulo/wal-quorum-sidecar/internal/metrics"
	"github.com/accumulo/wal-quorum-sidecar/internal/replication"
	"github.com/accumulo/wal-quorum-sidecar/internal/segment"
	pb "github.com/accumulo/wal-quorum-sidecar/proto/qwalpb"
)

// DefaultPeerReadyTimeout bounds how long the first OpenSegment waits for a
// peer connection to come up before proceeding in degraded mode.
//
// This wait exists for the startup race (the TServer can ask for a WAL before
// the peer sidecars are listening); if it expires the segment is still created
// and the peers are caught up later by the background prepare + replay. It is
// deliberately short: the WAL open blocks a TServer's commits for its whole
// duration, so a long wait here is itself an outage.
const DefaultPeerReadyTimeout = 5 * time.Second

const (
	// prepareAttemptTimeout bounds one background PrepareSegment RPC. Without
	// it a peer that accepts the connection and never answers blocks the retry
	// goroutine forever, which both defeats the retry bound and keeps the
	// (segment, peer) slot claimed so no later attempt can start.
	prepareAttemptTimeout = 5 * time.Second

	// catchUpTimeout bounds a background replica catch-up.
	catchUpTimeout = 60 * time.Second

	// catchUpChunkSize is how much of a segment is replayed per message. It
	// stays well inside the 4 MiB gRPC message limit.
	catchUpChunkSize = 1 << 20

	// catchUpRetryDelay throttles catch-up attempts against a peer that is
	// still down, so a steady stream of failing writes cannot start one per
	// entry.
	catchUpRetryDelay = 2 * time.Second
)

// LocalServer implements the WalQuorumLocal gRPC service.
// It handles communication between the co-located TServer and this sidecar
// over a Unix domain socket.
type LocalServer struct {
	pb.UnimplementedWalQuorumLocalServer

	mgr      *segment.Manager
	cfg      *config.Config
	quorum   *replication.QuorumWriter
	pool     *replication.PeerPool
	uploader *gcs.Uploader
	logger   *slog.Logger

	// peerReadyTimeout bounds the startup peer wait.
	peerReadyTimeout time.Duration

	// peersEverReady is set once any peer has been observed alive; after that,
	// opens no longer wait for peer readiness.
	peersEverReady atomic.Bool

	// peerTasks tracks in-flight background work against a peer (a
	// PrepareSegment retry loop or a replica catch-up), keyed by
	// "<kind>|<segmentID>|<peer address>", so repeated opens or a stream of
	// failing writes cannot pile up duplicate goroutines against a peer.
	prepareMu sync.Mutex
	peerTasks map[string]struct{}
}

// NewLocalServer creates a new LocalServer with quorum replication and GCS
// upload enabled. The uploader may be nil if GCS upload is not configured.
func NewLocalServer(mgr *segment.Manager, cfg *config.Config, pool *replication.PeerPool, quorum *replication.QuorumWriter, uploader *gcs.Uploader, logger *slog.Logger) *LocalServer {
	return &LocalServer{
		mgr:              mgr,
		cfg:              cfg,
		quorum:           quorum,
		pool:             pool,
		uploader:         uploader,
		logger:           logger.With("component", "local-server"),
		peerReadyTimeout: DefaultPeerReadyTimeout,
		peerTasks:        make(map[string]struct{}),
	}
}

// SetPeerReadyTimeout overrides how long the first OpenSegment waits for a
// reachable peer. Zero or negative restores the default.
func (s *LocalServer) SetPeerReadyTimeout(d time.Duration) {
	if d <= 0 {
		d = DefaultPeerReadyTimeout
	}
	s.peerReadyTimeout = d
}

// Register adds this service to a gRPC server.
func (s *LocalServer) Register(srv *grpc.Server) {
	pb.RegisterWalQuorumLocalServer(srv, s)
}

// OpenSegment creates a new WAL segment via the segment manager.
// In a full implementation this would also fan out PrepareSegment RPCs
// to peer sidecars; for now it creates the local segment.
func (s *LocalServer) OpenSegment(ctx context.Context, req *pb.OpenSegmentRequest) (*pb.OpenSegmentResponse, error) {
	if req.GetSegmentId() == nil {
		return nil, status.Error(codes.InvalidArgument, "segment_id is required")
	}

	segID := req.GetSegmentId().GetUuid()
	walPath := req.GetSegmentId().GetWalPath()
	originator := req.GetOriginatorPod()

	if segID == "" {
		return nil, status.Error(codes.InvalidArgument, "segment_id.uuid is required")
	}

	s.logger.Info("opening segment",
		"segment_id", segID,
		"wal_path", walPath,
		"originator", originator,
		"replication_factor", req.GetReplicationFactor(),
	)

	// Create the segment, or take back the one we already created if this open
	// is a retry. WAL opens are retried by the client for the same segment id
	// (VolumeManagerImpl.createSyncable re-calls fs.create on any failure from
	// its first attempt), and the first attempt may well have succeeded here
	// before the client gave up. A repeat open of a live segment owned by the
	// same writer is therefore a no-op, not an error — see
	// segment.Manager.CreateOrAdopt for the ownership rules that still reject
	// a claim from a different pod, WAL path, role, or incarnation.
	seg, adopted, err := s.mgr.CreateOrAdopt(segID, walPath, originator, segment.RoleOriginator)
	if err != nil {
		if segment.IsOwnershipConflict(err) {
			metrics.SegmentOpenConflicts.Inc()
		}
		s.logger.Error("failed to create segment", "segment_id", segID, "error", err)
		return &pb.OpenSegmentResponse{
			Success: false,
			Error:   err.Error(),
		}, nil
	}

	if adopted {
		// Repeat open of a segment we already prepared. Report the same peer
		// set as the original open and do not re-run the fan-out: the peers
		// already hold (or are being caught up to) this segment.
		peers := seg.PreparedPeers()
		if len(peers) == 0 && s.pool != nil {
			// The original open has not recorded its fan-out yet. Report every
			// configured peer, which is what the first open reports too — the
			// TServer stores this list in the metadata table for recovery.
			for _, peer := range s.pool.GetPeers() {
				peers = append(peers, peer.Address())
			}
		}
		metrics.SegmentReopens.Inc()
		s.logger.Warn("repeat open of live segment — returning the existing segment (idempotent)",
			"segment_id", segID,
			"originator", originator,
			"offset", seg.Offset(),
			"prepared_peers", peers,
		)
		return &pb.OpenSegmentResponse{
			Success:       true,
			PreparedPeers: peers,
		}, nil
	}

	metrics.SegmentsOpen.Inc()

	// Wait briefly for at least one peer connection before fanning out, to
	// smooth over the startup race where the TServer asks for a WAL before the
	// peer sidecars are listening. Peer connections are lazy, so this actively
	// dials them; polling IsHealthy() without dialing can never succeed.
	if s.pool != nil && len(s.pool.GetPeers()) > 0 {
		s.awaitPeers(ctx)
	}

	// Fan out PrepareSegment to peers so they create replica segment files.
	// If a peer isn't ready yet (startup race), retry in background.
	// Always include ALL configured peers in the response so the Java side
	// stores them in the metadata table for recovery (even if PrepareSegment
	// hasn't succeeded yet — the peer will be auto-prepared during writes).
	var preparedPeers []string
	if s.pool != nil {
		for _, peer := range s.pool.GetPeers() {
			preparedPeers = append(preparedPeers, peer.Address())
			if err := peer.PrepareSegment(ctx, segID, walPath, originator); err != nil {
				s.logger.Warn("failed to prepare segment on peer, will retry in background",
					"segment_id", segID,
					"peer", peer.Address(),
					"error", err,
				)
				s.retryPrepareInBackground(peer, segID, walPath, originator)
				continue
			}
			s.logger.Info("peer prepared segment",
				"segment_id", segID,
				"peer", peer.Address(),
			)
		}
	}
	seg.SetPreparedPeers(preparedPeers)

	return &pb.OpenSegmentResponse{
		Success:       true,
		PreparedPeers: preparedPeers,
	}, nil
}

// awaitPeers dials the peer sidecars and waits (briefly) for one of them to
// report a live connection. It returns as soon as a peer is reachable, when the
// caller's context is done, or when peerReadyTimeout expires — whichever comes
// first. Proceeding without a peer is a degraded open, so it is logged loudly.
//
// The wait only covers the startup race (peers not yet listening). Once any
// peer has been seen alive, later opens never wait: a peer that dies later is
// handled by the background prepare/replay and by the quorum writer, and making
// every WAL open pay a timeout for it is exactly how one bad node takes the
// write path down.
func (s *LocalServer) awaitPeers(ctx context.Context) {
	// Kick the lazy connections; without this, IsHealthy() is false for every
	// peer simply because nothing has dialed yet.
	s.pool.Warm()

	if s.peersEverReady.Load() {
		return
	}

	deadline := time.Now().Add(s.peerReadyTimeout)
	start := time.Now()
	for {
		for _, peer := range s.pool.GetPeers() {
			if peer.IsHealthy() {
				s.peersEverReady.Store(true)
				s.logger.Info("peer is reachable, proceeding with segment creation",
					"peer", peer.Address(), "waited", time.Since(start).String())
				return
			}
		}
		if !time.Now().Before(deadline) {
			s.logger.Warn("no peers reachable, opening segment in degraded mode "+
				"(peers will be prepared and replayed in the background)",
				"waited", time.Since(start).String(),
				"peer_count", len(s.pool.GetPeers()))
			return
		}
		select {
		case <-ctx.Done():
			s.logger.Warn("caller went away while waiting for peers, "+
				"opening segment in degraded mode",
				"waited", time.Since(start).String(), "error", ctx.Err())
			return
		case <-time.After(100 * time.Millisecond):
		}
	}
}

// claimPeerTask reserves the slot for background work against a peer,
// reporting whether the caller now owns it.
func (s *LocalServer) claimPeerTask(key string) bool {
	s.prepareMu.Lock()
	defer s.prepareMu.Unlock()
	if _, running := s.peerTasks[key]; running {
		return false
	}
	s.peerTasks[key] = struct{}{}
	return true
}

// releasePeerTask frees a slot claimed by claimPeerTask.
func (s *LocalServer) releasePeerTask(key string) {
	s.prepareMu.Lock()
	delete(s.peerTasks, key)
	s.prepareMu.Unlock()
}

// retryPrepareInBackground keeps trying to prepare a segment on a peer that was
// not ready, then replays whatever the segment has accumulated. At most one
// retry loop runs per (segment, peer): a wedged or repeatedly reopened segment
// must not spawn a new loop on every attempt.
func (s *LocalServer) retryPrepareInBackground(peer *replication.PeerClient, segID, walPath, originator string) {
	key := "prepare|" + segID + "|" + peer.Address()

	if !s.claimPeerTask(key) {
		s.logger.Warn("prepare retry already running for this segment and peer",
			"segment_id", segID, "peer", peer.Address())
		return
	}

	go func() {
		defer s.releasePeerTask(key)

		for i := 0; i < 30; i++ { // retry for up to 60 seconds
			time.Sleep(2 * time.Second)
			seg := s.mgr.Get(segID)
			if seg == nil {
				return // segment deleted, stop retrying
			}
			if seg.IsSealed() {
				// The segment finished without this peer; SealQuorum already
				// covered it. Nothing left to prepare.
				return
			}
			// Each attempt gets its own deadline: a peer that accepts the
			// connection but never answers must not park this goroutine (and
			// this segment's retry slot) forever.
			if err := s.prepareWithTimeout(peer, segID, walPath, originator); err == nil {
				s.logger.Info("peer prepared segment (background retry)",
					"segment_id", segID,
					"peer", peer.Address(),
					"attempt", i+1,
				)
				// Replay whatever this segment has accumulated so far.
				ctx, cancel := context.WithTimeout(context.Background(), catchUpTimeout)
				err := s.catchUpPeer(ctx, seg, peer)
				cancel()
				if err != nil {
					s.logger.Warn("failed to catch up peer after prepare",
						"segment_id", segID, "peer", peer.Address(), "error", err)
				}
				return
			}
		}
		s.logger.Warn("gave up preparing segment on peer after retries — "+
			"this segment stays under-replicated",
			"segment_id", segID,
			"peer", peer.Address(),
		)
	}()
}

// prepareWithTimeout runs one PrepareSegment RPC under its own deadline.
func (s *LocalServer) prepareWithTimeout(peer *replication.PeerClient, segID, walPath, originator string) error {
	ctx, cancel := context.WithTimeout(context.Background(), prepareAttemptTimeout)
	defer cancel()
	return peer.PrepareSegment(ctx, segID, walPath, originator)
}

// healReplicas starts a catch-up for every peer whose replica is known to be
// missing entries. Replication to such a replica is suspended until it has
// been replayed, so this is what lets a peer rejoin the quorum after a failure
// or after the failure breaker shed it.
func (s *LocalServer) healReplicas(seg *segment.Segment) {
	if s.pool == nil {
		return
	}
	for _, peer := range s.pool.GetPeers() {
		if !peer.NeedsCatchUp(seg.ID()) {
			continue
		}
		s.catchUpInBackground(seg, peer)
	}
}

// catchUpInBackground replays a peer's missing range without blocking the
// write path. At most one catch-up runs per (segment, peer).
func (s *LocalServer) catchUpInBackground(seg *segment.Segment, peer *replication.PeerClient) {
	key := "catchup|" + seg.ID() + "|" + peer.Address()
	if !s.claimPeerTask(key) {
		return
	}

	go func() {
		defer s.releasePeerTask(key)

		ctx, cancel := context.WithTimeout(context.Background(), catchUpTimeout)
		defer cancel()

		if err := s.catchUpPeer(ctx, seg, peer); err != nil {
			s.logger.Warn("failed to catch up peer replica — it stays out of the quorum "+
				"until the next attempt",
				"segment_id", seg.ID(), "peer", peer.Address(), "error", err)
			// Hold the slot a little longer so the next entry does not
			// immediately start another attempt against a peer still down.
			time.Sleep(catchUpRetryDelay)
		}
	}()
}

// catchUpPeer replays the byte range a peer's replica is missing, so that
// replica is once again a complete prefix of the local segment and normal
// replication can resume.
//
// Entries dropped while a peer was failing (or while the failure breaker had
// shed it) are exactly this missing range. Resuming replication without
// replaying it would leave a hole in the replica that is invisible until the
// seal-time size and checksum comparison rejects it.
func (s *LocalServer) catchUpPeer(ctx context.Context, seg *segment.Segment, peer *replication.PeerClient) error {
	from, known := peer.ReplicaAcked(seg.ID())
	if !known {
		from = 0
	}

	err := s.sendCatchUpRange(ctx, seg, peer, from)
	if err != nil && replication.IsSegmentMissing(err) {
		// The peer lost the replica (restart, or it was never prepared), so
		// prepare it again and replay the whole segment.
		if prepErr := peer.PrepareSegment(ctx, seg.ID(), seg.WALPath(), seg.OriginatorPod()); prepErr != nil {
			return prepErr
		}
		err = s.sendCatchUpRange(ctx, seg, peer, 0)
	}
	if err != nil {
		return err
	}

	metrics.ReplicaCatchups.Inc()
	s.logger.Info("peer replica caught up",
		"segment_id", seg.ID(), "peer", peer.Address(),
		"from_offset", from, "to_offset", seg.Offset())
	return nil
}

// sendCatchUpRange streams the segment file from the given offset to the peer.
// The peer skips any part of the range it already holds, so an overlapping
// replay is harmless.
func (s *LocalServer) sendCatchUpRange(ctx context.Context, seg *segment.Segment, peer *replication.PeerClient, from int64) error {
	filePath := seg.FilePath()
	if filePath == "" {
		return fmt.Errorf("segment %s has no file to replay", seg.ID())
	}

	f, err := os.Open(filePath)
	if err != nil {
		return fmt.Errorf("open segment file %s for replay: %w", filePath, err)
	}
	defer f.Close()

	if from > 0 {
		if _, err := f.Seek(from, io.SeekStart); err != nil {
			return fmt.Errorf("seek segment %s to offset %d: %w", seg.ID(), from, err)
		}
	}

	offset := from
	buf := make([]byte, catchUpChunkSize)
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		n, readErr := f.Read(buf)
		if n > 0 {
			if err := peer.CatchUpReplica(ctx, seg.ID(), buf[:n], offset); err != nil {
				return err
			}
			offset += int64(n)
		}
		if readErr == io.EOF {
			break
		}
		if readErr != nil {
			return fmt.Errorf("read segment %s for replay: %w", seg.ID(), readErr)
		}
	}

	if offset == from {
		// Nothing outstanding, but the peer's position still has to be
		// confirmed before normal replication may resume.
		return peer.CatchUpReplica(ctx, seg.ID(), nil, from)
	}
	return nil
}

// WriteEntries is a bidirectional streaming RPC. The TServer streams WAL entry
// bytes to the sidecar, and the sidecar streams acks after quorum persistence.
//
// Each received entry is written locally AND replicated to peers via the
// QuorumWriter. The ack is sent back only after 2-of-3 quorum is achieved
// (local + at least one peer), or after the quorum timeout with a degraded
// local-only write.
func (s *LocalServer) WriteEntries(stream pb.WalQuorumLocal_WriteEntriesServer) error {
	for {
		req, err := stream.Recv()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return status.Errorf(codes.Internal, "recv error: %v", err)
		}

		segID := req.GetSegmentId().GetUuid()
		seg := s.mgr.Get(segID)
		if seg == nil {
			return status.Errorf(codes.NotFound, "segment %s not found", segID)
		}

		data := req.GetData()
		seqNum := req.GetSequenceNum()
		offset := seg.Offset()

		// Write locally and replicate to peers with quorum semantics.
		quorumCount, err := s.quorum.WriteAndReplicate(stream.Context(), seg, data, offset, seqNum)
		if err != nil {
			s.logger.Error("quorum write failed",
				"segment_id", segID,
				"seq", seqNum,
				"error", err,
			)
			return status.Errorf(codes.Internal, "quorum write failed: %v", err)
		}

		// A peer that missed this entry (it failed, or the failure breaker
		// shed it) now holds a short replica, and replication to it stays
		// suspended until the missing range is replayed. Start that replay in
		// the background so the peer can rejoin the quorum.
		s.healReplicas(seg)

		// Ack with actual quorum count (1 = local only / degraded,
		// 2 = local + 1 peer, 3 = local + 2 peers).
		resp := &pb.WriteEntryResponse{
			SegmentId:        req.GetSegmentId(),
			AckedSequenceNum: seqNum,
			QuorumCount:      quorumCount,
			CommittedOffset:  seg.Offset(),
		}
		if err := stream.Send(resp); err != nil {
			return status.Errorf(codes.Internal, "send ack error: %v", err)
		}
	}
}

// SyncSegment calls fdatasync on the local segment file and fans out sync
// requests to peers, waiting for quorum (local + 1 peer) before returning.
func (s *LocalServer) SyncSegment(ctx context.Context, req *pb.SyncSegmentRequest) (*pb.SyncSegmentResponse, error) {
	if req.GetSegmentId() == nil {
		return nil, status.Error(codes.InvalidArgument, "segment_id is required")
	}

	segID := req.GetSegmentId().GetUuid()
	seg := s.mgr.Get(segID)
	if seg == nil {
		return nil, status.Errorf(codes.NotFound, "segment %s not found", segID)
	}

	if err := s.quorum.SyncQuorum(ctx, seg); err != nil {
		s.logger.Error("quorum sync failed", "segment_id", segID, "error", err)
		return &pb.SyncSegmentResponse{
			Success: false,
			Error:   err.Error(),
		}, nil
	}

	return &pb.SyncSegmentResponse{
		Success:      true,
		SyncedOffset: seg.Offset(),
	}, nil
}

// CloseSegment seals the local segment and all peer replicas via quorum seal,
// then triggers an async upload to GCS if configured.
func (s *LocalServer) CloseSegment(ctx context.Context, req *pb.CloseSegmentRequest) (*pb.CloseSegmentResponse, error) {
	if req.GetSegmentId() == nil {
		return nil, status.Error(codes.InvalidArgument, "segment_id is required")
	}

	segID := req.GetSegmentId().GetUuid()
	seg := s.mgr.Get(segID)
	if seg == nil {
		return nil, status.Errorf(codes.NotFound, "segment %s not found", segID)
	}

	// Before sealing, replay whatever each peer is missing: a peer prepared
	// late, restarted, or shed by the failure breaker holds a short replica,
	// and the seal compares sizes and checksums. Writes have stopped by now,
	// so this is the point at which a replica can be brought fully level.
	if s.pool != nil {
		for _, peer := range s.pool.GetPeers() {
			acked, known := peer.ReplicaAcked(segID)
			if known && !peer.NeedsCatchUp(segID) && acked == seg.Offset() {
				continue
			}
			if err := s.catchUpPeer(ctx, seg, peer); err != nil {
				s.logger.Warn("failed to catch up peer replica before seal — "+
					"its seal will be rejected on the size/checksum check",
					"segment_id", segID, "peer", peer.Address(), "error", err)
			}
		}
	}

	// Seal locally and fan out to all peers (waits for all, not just quorum).
	checksum, size, err := s.quorum.SealQuorum(ctx, seg)
	if err != nil {
		s.logger.Error("quorum seal failed", "segment_id", segID, "error", err)
		return &pb.CloseSegmentResponse{
			Success: false,
			Error:   err.Error(),
		}, nil
	}

	if s.pool != nil {
		for _, peer := range s.pool.GetPeers() {
			peer.ForgetSegment(segID)
		}
	}

	metrics.SegmentsOpen.Dec()
	metrics.SegmentsSealed.Inc()

	gcsPath := ""
	if s.cfg.GCSBucket != "" && s.uploader != nil {
		gcsPath = fmt.Sprintf("gs://%s/wal/%s/%s.wal", s.cfg.GCSBucket, seg.OriginatorPod(), segID)
		s.uploader.QueueUpload(seg, seg.OriginatorPod())
		s.logger.Info("GCS upload queued", "segment_id", segID, "gcs_path", gcsPath)
	}

	s.logger.Info("segment sealed (quorum)",
		"segment_id", segID,
		"size", size,
		"checksum_len", len(checksum),
		"gcs_path", gcsPath,
	)

	return &pb.CloseSegmentResponse{
		Success:       true,
		SegmentSize:   size,
		GcsObjectPath: gcsPath,
	}, nil
}

// HealthCheck returns the current health status of the sidecar.
func (s *LocalServer) HealthCheck(ctx context.Context, req *pb.HealthCheckRequest) (*pb.HealthCheckResponse, error) {
	openCount := s.mgr.OpenCount()

	var peerConns uint32
	if s.pool != nil {
		for _, p := range s.pool.GetPeers() {
			if p.IsHealthy() {
				peerConns++
			}
		}
	}

	return &pb.HealthCheckResponse{
		Status:          pb.HealthCheckResponse_SERVING,
		OpenSegments:    uint32(openCount),
		PeerConnections: peerConns,
	}, nil
}
