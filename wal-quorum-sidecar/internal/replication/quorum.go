// Licensed to the Apache Software Foundation (ASF) under one
// or more contributor license agreements.
package replication

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/accumulo/wal-quorum-sidecar/internal/metrics"
	"github.com/accumulo/wal-quorum-sidecar/internal/segment"
)

const (
	// DefaultQuorumTimeout is the maximum time to wait for quorum acks
	// before falling back to degraded mode.
	DefaultQuorumTimeout = 500 * time.Millisecond
)

// quorumResult captures the outcome of a single replication attempt to a peer.
type quorumResult struct {
	peer string
	err  error
}

// QuorumWriter coordinates writes across the local segment and peer replicas.
// It implements 2-of-3 quorum semantics: a write is considered durable when
// the local write succeeds AND at least one peer acknowledges.
//
// If all peers are unreachable, the write still succeeds locally (degraded
// mode) with a warning logged. This ensures the TServer is never blocked
// by peer failures.
type QuorumWriter struct {
	pool          *PeerPool
	quorumTimeout time.Duration
	logger        *slog.Logger
}

// NewQuorumWriter creates a QuorumWriter with the given peer pool and timeout.
// If quorumTimeout is 0, DefaultQuorumTimeout (500ms) is used.
func NewQuorumWriter(pool *PeerPool, quorumTimeout time.Duration, logger *slog.Logger) *QuorumWriter {
	if quorumTimeout <= 0 {
		quorumTimeout = DefaultQuorumTimeout
	}
	return &QuorumWriter{
		pool:          pool,
		quorumTimeout: quorumTimeout,
		logger:        logger.With("component", "quorum-writer"),
	}
}

// WriteAndReplicate writes data to the local segment and fans out to peers
// in parallel. It waits for the local write AND at least one peer ack
// (2-of-3 quorum). If no peers are available or all peers time out, the
// local write alone is sufficient (degraded mode).
//
// Returns the number of nodes that durably persisted the entry (1 = local only,
// 2 = local + 1 peer, 3 = local + 2 peers). The caller uses this to detect
// degraded replication and trigger early WAL close.
//
// The data is copied into a pooled buffer for the fan-out so the caller's
// slice is safe to reuse after this call returns.
func (qw *QuorumWriter) WriteAndReplicate(
	ctx context.Context,
	seg *segment.Segment,
	data []byte,
	offset int64,
	seqNum uint64,
) (quorumCount uint32, err error) {
	start := time.Now()
	defer func() {
		metrics.QuorumLatency.Observe(time.Since(start).Seconds())
	}()

	peers := qw.pool.GetPeers()

	// Copy data into a pooled buffer that all goroutines can safely read.
	buf := segment.GetBuffer()
	*buf = append(*buf, data...)
	dataCopy := *buf

	// Channel collects results: local + each peer.
	type writeResult struct {
		source string
		err    error
	}
	resultCh := make(chan writeResult, 1+len(peers))

	// Local write.
	go func() {
		localStart := time.Now()
		_, _, err := seg.Write(dataCopy, seqNum)
		metrics.WriteLatency.Observe(time.Since(localStart).Seconds())
		if err == nil {
			metrics.EntriesWrittenTotal.Inc()
			metrics.BytesWrittenTotal.Add(float64(len(dataCopy)))
		}
		resultCh <- writeResult{source: "local", err: err}
	}()

	// Fan out to peers.
	for _, peer := range peers {
		peer := peer // capture loop variable
		go func() {
			err := peer.ReplicateEntry(ctx, seg.ID(), seg.WALPath(), seg.OriginatorPod(), dataCopy, offset, seqNum)
			resultCh <- writeResult{source: peer.Address(), err: err}
		}()
	}

	// Wait for quorum: local + >= 1 peer (2-of-3).
	// If there are no peers, we only need the local write.
	requiredPeerAcks := 1
	if len(peers) == 0 {
		requiredPeerAcks = 0
	}

	var (
		localDone     bool
		localErr      error
		peerAcks      int
		peerErrors    []error
		totalDone     int
		totalExpected = 1 + len(peers)
	)

	timer := time.NewTimer(qw.quorumTimeout)
	defer timer.Stop()

	for {
		select {
		case r := <-resultCh:
			totalDone++
			if r.source == "local" {
				localDone = true
				localErr = r.err
			} else {
				if r.err == nil {
					peerAcks++
				} else {
					peerErrors = append(peerErrors, fmt.Errorf("peer %s: %w", r.source, r.err))
					// A peer shed by the failure breaker already logged itself
					// at WARN when it was shed and keeps re-stating it while it
					// is down, and a peer awaiting a catch-up replay is logged
					// by the catch-up itself; logging every suppressed entry
					// here would bury the originator's own errors under
					// thousands of lines.
					if !errors.Is(r.err, ErrPeerCoolingDown) && !errors.Is(r.err, ErrPeerReplicaBehind) {
						qw.logger.Warn("peer replication failed",
							"peer", r.source,
							"segment_id", seg.ID(),
							"seq", seqNum,
							"error", r.err,
						)
					}
				}
			}

			// Check if quorum is achieved.
			if localDone && localErr == nil && peerAcks >= requiredPeerAcks {
				// Quorum achieved. Return the buffer after all outstanding
				// goroutines finish (in the background) to avoid data races.
				qc := uint32(1 + peerAcks)
				go func() {
					for totalDone < totalExpected {
						<-resultCh
						totalDone++
					}
					segment.PutBuffer(buf)
				}()
				return qc, nil
			}

			// If local write failed, fail immediately.
			if localDone && localErr != nil {
				// Drain remaining in background.
				go func() {
					for totalDone < totalExpected {
						<-resultCh
						totalDone++
					}
					segment.PutBuffer(buf)
				}()
				return 0, fmt.Errorf("local write failed: %w", localErr)
			}

			// If all results are in, evaluate.
			if totalDone >= totalExpected {
				segment.PutBuffer(buf)
				if localErr != nil {
					return 0, fmt.Errorf("local write failed: %w", localErr)
				}
				if peerAcks < requiredPeerAcks {
					// Degraded mode: local write succeeded but no peer acks.
					qw.logger.Warn("write succeeded in degraded mode (no peer acks)",
						"segment_id", seg.ID(),
						"seq", seqNum,
						"peer_errors", len(peerErrors),
					)
				}
				return uint32(1 + peerAcks), nil
			}

		case <-timer.C:
			// Timeout waiting for quorum.
			if localDone && localErr == nil {
				// Local succeeded, but peers didn't respond in time.
				// Proceed in degraded mode.
				if peerAcks < requiredPeerAcks {
					qw.logger.Warn("quorum timeout — proceeding with local write only",
						"segment_id", seg.ID(),
						"seq", seqNum,
						"peer_acks", peerAcks,
						"timeout", qw.quorumTimeout,
					)
				}
				qc := uint32(1 + peerAcks)
				// Drain remaining in background.
				go func() {
					for totalDone < totalExpected {
						<-resultCh
						totalDone++
					}
					segment.PutBuffer(buf)
				}()
				return qc, nil
			}

			// Local hasn't completed yet either — wait a bit more for local only.
			// This shouldn't happen in practice (local disk < 500ms) but handle it.
			qw.logger.Error("quorum timeout and local write not yet complete",
				"segment_id", seg.ID(),
				"seq", seqNum,
			)
			// Wait for at least the local result.
			for !localDone {
				r := <-resultCh
				totalDone++
				if r.source == "local" {
					localDone = true
					localErr = r.err
				} else if r.err == nil {
					peerAcks++
				}
			}
			// Drain remaining in background.
			go func() {
				for totalDone < totalExpected {
					<-resultCh
					totalDone++
				}
				segment.PutBuffer(buf)
			}()
			if localErr != nil {
				return 0, fmt.Errorf("local write failed after timeout: %w", localErr)
			}
			return uint32(1 + peerAcks), nil

		case <-ctx.Done():
			// Drain remaining in background.
			go func() {
				for totalDone < totalExpected {
					<-resultCh
					totalDone++
				}
				segment.PutBuffer(buf)
			}()
			return uint32(1 + peerAcks), ctx.Err()
		}
	}
}

// SyncQuorum calls fdatasync on the local segment and requests each peer
// to sync its replica, waiting for the local sync AND at least one peer
// (quorum) before returning.
func (qw *QuorumWriter) SyncQuorum(ctx context.Context, seg *segment.Segment) error {
	peers := qw.pool.GetPeers()

	resultCh := make(chan quorumResult, 1+len(peers))

	// Local fdatasync.
	go func() {
		syncStart := time.Now()
		err := seg.Fdatasync()
		metrics.SyncLatency.Observe(time.Since(syncStart).Seconds())
		resultCh <- quorumResult{peer: "local", err: err}
	}()

	// Peer syncs.
	for _, peer := range peers {
		peer := peer
		go func() {
			err := peer.SyncSegment(ctx, seg.ID())
			resultCh <- quorumResult{peer: peer.Address(), err: err}
		}()
	}

	requiredPeerAcks := 1
	if len(peers) == 0 {
		requiredPeerAcks = 0
	}

	var (
		localDone     bool
		localErr      error
		peerAcks      int
		totalDone     int
		totalExpected = 1 + len(peers)
	)

	timer := time.NewTimer(qw.quorumTimeout)
	defer timer.Stop()

	for {
		select {
		case r := <-resultCh:
			totalDone++
			if r.peer == "local" {
				localDone = true
				localErr = r.err
			} else if r.err == nil {
				peerAcks++
			} else {
				qw.logger.Warn("peer sync failed",
					"peer", r.peer,
					"segment_id", seg.ID(),
					"error", r.err,
				)
			}

			if localDone && localErr == nil && peerAcks >= requiredPeerAcks {
				return nil
			}
			if localDone && localErr != nil {
				return fmt.Errorf("local fdatasync failed: %w", localErr)
			}
			if totalDone >= totalExpected {
				if localErr != nil {
					return fmt.Errorf("local fdatasync failed: %w", localErr)
				}
				if peerAcks < requiredPeerAcks {
					qw.logger.Warn("sync completed in degraded mode (no peer acks)",
						"segment_id", seg.ID(),
					)
				}
				return nil
			}

		case <-timer.C:
			if localDone && localErr == nil {
				if peerAcks < requiredPeerAcks {
					qw.logger.Warn("sync quorum timeout — local sync succeeded, proceeding",
						"segment_id", seg.ID(),
					)
				}
				return nil
			}
			// Wait for local if it hasn't finished.
			for !localDone && totalDone < totalExpected {
				r := <-resultCh
				totalDone++
				if r.peer == "local" {
					localDone = true
					localErr = r.err
				}
			}
			if localErr != nil {
				return fmt.Errorf("local fdatasync failed after timeout: %w", localErr)
			}
			return nil

		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

// SealQuorum seals the local segment and sends SealSegment to ALL peers,
// waiting for every peer to confirm (not just quorum). We want all 3 copies
// to be consistent before GCS upload. If a peer fails to seal, an error is
// logged but the seal is still considered successful as long as the local
// seal succeeded.
func (qw *QuorumWriter) SealQuorum(ctx context.Context, seg *segment.Segment) (checksum []byte, size int64, err error) {
	// Seal the local segment first to get the authoritative checksum.
	checksum, size, err = seg.Seal()
	if err != nil {
		return nil, 0, fmt.Errorf("local seal failed: %w", err)
	}

	peers := qw.pool.GetPeers()
	if len(peers) == 0 {
		return checksum, size, nil
	}

	// Fan out SealSegment to all peers and wait for all of them.
	resultCh := make(chan quorumResult, len(peers))
	for _, peer := range peers {
		peer := peer
		go func() {
			success, sealErr := peer.SealSegment(ctx, seg.ID(), size, checksum)
			if sealErr != nil {
				resultCh <- quorumResult{peer: peer.Address(), err: sealErr}
				return
			}
			if !success {
				resultCh <- quorumResult{
					peer: peer.Address(),
					err:  fmt.Errorf("seal returned success=false"),
				}
				return
			}
			resultCh <- quorumResult{peer: peer.Address(), err: nil}
		}()
	}

	// Wait for all peers with a generous timeout (seal includes fdatasync).
	sealTimeout := qw.quorumTimeout * 4
	timer := time.NewTimer(sealTimeout)
	defer timer.Stop()

	collected := 0
	for collected < len(peers) {
		select {
		case r := <-resultCh:
			collected++
			if r.err != nil {
				qw.logger.Error("peer seal failed",
					"peer", r.peer,
					"segment_id", seg.ID(),
					"error", r.err,
				)
			} else {
				qw.logger.Info("peer seal confirmed",
					"peer", r.peer,
					"segment_id", seg.ID(),
				)
			}
		case <-timer.C:
			qw.logger.Error("timeout waiting for all peer seals",
				"segment_id", seg.ID(),
				"collected", collected,
				"total_peers", len(peers),
			)
			return checksum, size, nil
		case <-ctx.Done():
			return checksum, size, ctx.Err()
		}
	}

	return checksum, size, nil
}
