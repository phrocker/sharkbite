// Licensed to the Apache Software Foundation (ASF) under one
// or more contributor license agreements.
package replication

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/connectivity"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/keepalive"
	"google.golang.org/grpc/status"

	pb "github.com/accumulo/wal-quorum-sidecar/proto/qwalpb"
)

// SegmentInfo holds metadata about a segment on a peer.
type SegmentInfo struct {
	ID            string
	WALPath       string
	OriginatorPod string
	Size          int64
	Sealed        bool
}

const (
	// breakerThreshold is the number of consecutive replication failures to a
	// peer before that peer is shed for a cooldown.
	breakerThreshold = 3

	// breakerCooldown is how long a shed peer is skipped before it is retried.
	breakerCooldown = 5 * time.Second

	// DefaultReplicateTimeout bounds a single send/ack exchange on a peer's
	// persistent replication stream. The stream itself outlives any one entry,
	// so without this bound a peer that accepts the connection and then stops
	// answering leaves the caller blocked in Recv forever: the failure is never
	// recorded, the breaker never opens, and every subsequent write both pays
	// the quorum timeout and leaks another blocked goroutine.
	DefaultReplicateTimeout = 5 * time.Second
)

// ErrPeerCoolingDown is returned instead of attempting replication while a peer
// is shed by the failure breaker. Callers treat it as a normal peer failure but
// need not log it per entry — the breaker logs its own state changes.
var ErrPeerCoolingDown = errors.New("peer is cooling down after repeated replication failures")

// ErrPeerReplicaBehind is returned when the peer's replica is known to be
// missing bytes, so it is not a prefix-consistent target for the entry being
// replicated. Replication to that replica stays suspended until the missing
// range has been replayed; sending the entry anyway would leave a hole in the
// replica that only surfaces as a checksum failure at seal time.
var ErrPeerReplicaBehind = errors.New("peer replica is missing entries and must be caught up before replication resumes")

// PeerClient manages a gRPC connection to one peer sidecar.
// The connection is lazy: it is not established until the first RPC call.
type PeerClient struct {
	address string
	logger  *slog.Logger

	mu     sync.Mutex
	conn   *grpc.ClientConn
	client pb.WalQuorumPeerClient

	// replicateStreams caches open bidirectional ReplicateEntries streams
	// keyed by segment ID, so entries for the same segment reuse the stream.
	streamsMu sync.Mutex
	streams   map[string]*replicateStream

	// Failure breaker. A peer that keeps failing is shed for a cooldown so a
	// single unreachable or wedged peer cannot make every write on this node
	// pay the full quorum timeout, and so we stop hammering it while it is
	// down. State changes are logged at WARN.
	breakerMu       sync.Mutex
	failures        int
	cooldownUntil   time.Time
	suppressedCalls int

	// replicas tracks, per segment, how much of the segment this peer has
	// confirmed it persisted, and whether a range is known to be missing.
	replicaMu sync.Mutex
	replicas  map[string]*replicaState

	// replicateTimeout bounds one send/ack exchange; see DefaultReplicateTimeout.
	replicateTimeout time.Duration
}

// replicaState is what this client knows about one segment replica on the peer.
//
// acked is the peer's own reported persisted offset, so it is authoritative
// rather than an optimistic count of what was sent. known is false until the
// peer has reported an offset for this incarnation of the replica (e.g. right
// after PrepareSegment, when the replica is empty but unconfirmed).
// needsCatchUp records that at least one entry did not reach the replica, so
// the replica is short and must be replayed before normal replication resumes.
type replicaState struct {
	acked        int64
	known        bool
	needsCatchUp bool
}

// replicateStream holds a persistent bidirectional stream for one segment.
type replicateStream struct {
	stream pb.WalQuorumPeer_ReplicateEntriesClient
	cancel context.CancelFunc

	// sendMu serialises the send/ack pair: a gRPC stream must not be used by
	// several goroutines at once, and the ack has to be matched to the entry
	// that produced it.
	sendMu sync.Mutex
}

// NewPeerClient creates a PeerClient for the given peer address.
// The address should include the port (e.g. "tserver-1.tserver.default.svc.cluster.local:9710").
// No connection is made until the first RPC call.
func NewPeerClient(address string, logger *slog.Logger) *PeerClient {
	return &PeerClient{
		address:          address,
		logger:           logger.With("component", "peer-client", "peer", address),
		streams:          make(map[string]*replicateStream),
		replicas:         make(map[string]*replicaState),
		replicateTimeout: DefaultReplicateTimeout,
	}
}

// SetReplicateTimeout overrides the bound on a single send/ack exchange.
// Zero or negative restores the default.
func (pc *PeerClient) SetReplicateTimeout(d time.Duration) {
	if d <= 0 {
		d = DefaultReplicateTimeout
	}
	pc.mu.Lock()
	defer pc.mu.Unlock()
	pc.replicateTimeout = d
}

func (pc *PeerClient) callTimeout() time.Duration {
	pc.mu.Lock()
	defer pc.mu.Unlock()
	if pc.replicateTimeout <= 0 {
		return DefaultReplicateTimeout
	}
	return pc.replicateTimeout
}

// Address returns the peer's address.
func (pc *PeerClient) Address() string {
	return pc.address
}

// ensureConnected lazily establishes the gRPC connection if not already connected.
func (pc *PeerClient) ensureConnected() (pb.WalQuorumPeerClient, error) {
	pc.mu.Lock()
	defer pc.mu.Unlock()

	if pc.client != nil {
		return pc.client, nil
	}

	opts := []grpc.DialOption{
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithKeepaliveParams(keepalive.ClientParameters{
			Time:                5 * time.Minute,
			Timeout:             20 * time.Second,
			PermitWithoutStream: false,
		}),
		grpc.WithDefaultCallOptions(
			grpc.MaxCallRecvMsgSize(4*1024*1024),
			grpc.MaxCallSendMsgSize(4*1024*1024),
		),
	}

	conn, err := grpc.NewClient(pc.address, opts...)
	if err != nil {
		return nil, fmt.Errorf("dial peer %s: %w", pc.address, err)
	}

	pc.conn = conn
	pc.client = pb.NewWalQuorumPeerClient(conn)
	pc.logger.Info("peer connection created")
	return pc.client, nil
}

// Connect establishes the gRPC connection (if needed) and asks it to start
// connecting, without blocking on the handshake. Used to warm up the lazily
// dialed peers so health checks reflect reality.
func (pc *PeerClient) Connect() {
	client, err := pc.ensureConnected()
	if err != nil || client == nil {
		pc.logger.Warn("failed to create peer connection", "error", err)
		return
	}
	pc.mu.Lock()
	conn := pc.conn
	pc.mu.Unlock()
	if conn != nil {
		conn.Connect()
	}
}

// isCoolingDown reports whether the failure breaker is currently shedding this
// peer, counting the suppressed call.
func (pc *PeerClient) isCoolingDown() bool {
	pc.breakerMu.Lock()
	defer pc.breakerMu.Unlock()

	if time.Now().Before(pc.cooldownUntil) {
		pc.suppressedCalls++
		// Re-state the situation periodically so a long outage stays visible.
		if pc.suppressedCalls%1000 == 0 {
			pc.logger.Warn("peer still shed by the failure breaker",
				"suppressed_calls", pc.suppressedCalls,
				"cooldown_remaining", time.Until(pc.cooldownUntil).String())
		}
		return true
	}
	return false
}

// recordFailure advances the failure breaker, shedding the peer once it has
// failed breakerThreshold times in a row.
func (pc *PeerClient) recordFailure(err error) {
	pc.breakerMu.Lock()
	defer pc.breakerMu.Unlock()

	pc.failures++
	if pc.failures >= breakerThreshold && time.Now().After(pc.cooldownUntil) {
		pc.cooldownUntil = time.Now().Add(breakerCooldown)
		pc.logger.Warn("shedding peer after consecutive replication failures — "+
			"writes continue with the remaining replicas",
			"consecutive_failures", pc.failures,
			"cooldown", breakerCooldown.String(),
			"error", err)
	}
}

// recordSuccess clears the failure breaker.
func (pc *PeerClient) recordSuccess() {
	pc.breakerMu.Lock()
	defer pc.breakerMu.Unlock()

	if pc.failures >= breakerThreshold || pc.suppressedCalls > 0 {
		pc.logger.Warn("peer recovered, resuming replication",
			"consecutive_failures", pc.failures,
			"suppressed_calls", pc.suppressedCalls)
	}
	pc.failures = 0
	pc.suppressedCalls = 0
	pc.cooldownUntil = time.Time{}
}

// PrepareSegment tells the peer to create a replica segment file.
func (pc *PeerClient) PrepareSegment(ctx context.Context, segmentID, walPath, originatorPod string) error {
	client, err := pc.ensureConnected()
	if err != nil {
		return err
	}

	resp, err := client.PrepareSegment(ctx, &pb.PrepareSegmentRequest{
		SegmentId: &pb.SegmentId{
			Uuid:    segmentID,
			WalPath: walPath,
		},
		OriginatorPod: originatorPod,
	})
	if err != nil {
		return fmt.Errorf("PrepareSegment RPC to %s: %w", pc.address, err)
	}
	if !resp.GetSuccess() {
		return fmt.Errorf("PrepareSegment on %s failed: %s", pc.address, resp.GetError())
	}

	// A prepared replica starts empty (a stale file from a previous
	// incarnation is discarded by the peer), and how much of it survived a
	// peer restart is not something this client can assume, so drop whatever
	// was recorded and let the first ack re-establish the offset.
	pc.resetReplica(segmentID)
	return nil
}

// replicaFor returns the tracked state for a segment, creating it if needed.
// Called with replicaMu held.
func (pc *PeerClient) replicaFor(segmentID string) *replicaState {
	st, ok := pc.replicas[segmentID]
	if !ok {
		st = &replicaState{}
		pc.replicas[segmentID] = st
	}
	return st
}

// resetReplica forgets what was known about a replica, without demanding a
// catch-up: the caller has just (re)prepared it.
func (pc *PeerClient) resetReplica(segmentID string) {
	pc.replicaMu.Lock()
	defer pc.replicaMu.Unlock()
	pc.replicas[segmentID] = &replicaState{}
}

// markCatchUpNeeded records that an entry did not reach the replica, so the
// replica is now short of the originator and must be replayed.
func (pc *PeerClient) markCatchUpNeeded(segmentID string) {
	pc.replicaMu.Lock()
	defer pc.replicaMu.Unlock()
	pc.replicaFor(segmentID).needsCatchUp = true
}

// recordAck stores the peer's reported persisted offset. Offsets only ever
// move forward: a catch-up replay of an already-persisted range reports the
// replica's current end, which must not pull the tracked offset back.
func (pc *PeerClient) recordAck(segmentID string, persisted int64, caughtUp bool) {
	pc.replicaMu.Lock()
	defer pc.replicaMu.Unlock()

	st := pc.replicaFor(segmentID)
	if !st.known || persisted > st.acked {
		st.acked = persisted
	}
	st.known = true
	if caughtUp {
		st.needsCatchUp = false
	}
}

// checkReplicaPosition refuses an entry that would not land at the end of the
// replica. Sending it anyway would either be silently dropped by the peer or
// leave a hole; either way the divergence would only be discovered at seal.
func (pc *PeerClient) checkReplicaPosition(segmentID string, offset int64) error {
	pc.replicaMu.Lock()
	defer pc.replicaMu.Unlock()

	st, ok := pc.replicas[segmentID]
	if !ok || !st.known {
		// Nothing confirmed yet; the peer validates the offset itself.
		return nil
	}
	if st.needsCatchUp {
		return ErrPeerReplicaBehind
	}
	if st.acked != offset {
		st.needsCatchUp = true
		return fmt.Errorf("%w: replica of segment %s is at offset %d, entry starts at %d",
			ErrPeerReplicaBehind, segmentID, st.acked, offset)
	}
	return nil
}

// ReplicaAcked returns the offset the peer last confirmed it had persisted for
// the segment, and whether that offset has been confirmed at all.
func (pc *PeerClient) ReplicaAcked(segmentID string) (int64, bool) {
	pc.replicaMu.Lock()
	defer pc.replicaMu.Unlock()

	st, ok := pc.replicas[segmentID]
	if !ok {
		return 0, false
	}
	return st.acked, st.known
}

// NeedsCatchUp reports whether entries are known to be missing from this
// peer's replica of the segment, i.e. replication to it is suspended until the
// missing range is replayed.
func (pc *PeerClient) NeedsCatchUp(segmentID string) bool {
	pc.replicaMu.Lock()
	defer pc.replicaMu.Unlock()

	st, ok := pc.replicas[segmentID]
	return ok && st.needsCatchUp
}

// ForgetSegment drops the tracked replica state for a segment. Called once the
// segment is sealed or deleted so the map does not grow without bound.
func (pc *PeerClient) ForgetSegment(segmentID string) {
	pc.replicaMu.Lock()
	delete(pc.replicas, segmentID)
	pc.replicaMu.Unlock()
}

// ReplicateEntry sends a single WAL entry to the peer for replication.
// It lazily opens a persistent bidirectional stream per segment ID and reuses
// it for subsequent entries. Returns nil on success (peer ack received).
func (pc *PeerClient) ReplicateEntry(ctx context.Context, segmentID string, walPath string, originatorPod string, data []byte, offset int64, seqNum uint64) error {
	// Shed the peer while the failure breaker is open: a peer that is down or
	// wedged must not cost every write the full quorum timeout, and must not be
	// re-dialed once per entry. The entry is dropped, so the replica is now
	// short — record that, or replication would silently resume mid-file and
	// leave a hole that only surfaces as a checksum failure at seal.
	if pc.isCoolingDown() {
		pc.markCatchUpNeeded(segmentID)
		return ErrPeerCoolingDown
	}

	// Likewise, do not append to a replica that is already known to be short:
	// it has to be replayed first (see CatchUpReplica).
	if err := pc.checkReplicaPosition(segmentID, offset); err != nil {
		return err
	}

	persisted, err := pc.replicateEntryInner(ctx, segmentID, data, offset, seqNum)
	if err != nil {
		pc.markCatchUpNeeded(segmentID)
		pc.recordFailure(err)
		return err
	}
	if end := offset + int64(len(data)); persisted < end {
		err := fmt.Errorf("peer %s persisted segment %s to offset %d, expected %d",
			pc.address, segmentID, persisted, end)
		pc.markCatchUpNeeded(segmentID)
		pc.recordFailure(err)
		return err
	}
	pc.recordAck(segmentID, persisted, false)
	pc.recordSuccess()
	return nil
}

// CatchUpReplica replays a byte range into the peer's replica, starting at
// offset. Unlike ReplicateEntry it is not gated on the replica being up to
// date — it is what makes it up to date. The peer skips any part of the range
// it already holds, so replaying an overlapping range is safe.
func (pc *PeerClient) CatchUpReplica(ctx context.Context, segmentID string, data []byte, offset int64) error {
	persisted, err := pc.replicateEntryInner(ctx, segmentID, data, offset, 0)
	if err != nil {
		pc.markCatchUpNeeded(segmentID)
		pc.recordFailure(err)
		return err
	}
	if end := offset + int64(len(data)); persisted < end {
		err := fmt.Errorf("peer %s caught up segment %s only to offset %d, expected %d",
			pc.address, segmentID, persisted, end)
		pc.markCatchUpNeeded(segmentID)
		pc.recordFailure(err)
		return err
	}
	pc.recordAck(segmentID, persisted, true)
	pc.recordSuccess()
	return nil
}

// replicateEntryInner sends one entry on the segment's persistent stream and
// returns the offset the peer reports it has persisted.
//
// The stream outlives the call, so the exchange is bounded here: the caller's
// context and a timeout both close the stream, which unblocks a Send or Recv
// that a wedged peer would otherwise hold forever and lets the failure reach
// the breaker.
func (pc *PeerClient) replicateEntryInner(ctx context.Context, segmentID string, data []byte, offset int64, seqNum uint64) (int64, error) {
	rs, err := pc.getOrCreateStream(segmentID)
	if err != nil {
		// Don't auto-prepare here — the background goroutine in OpenSegment
		// handles late peers via PrepareSegment + a catch-up replay.
		// Auto-preparing here races with that replay and corrupts the peer's
		// WAL file (entries get prepended before the header).
		return 0, err
	}

	req := &pb.ReplicateEntryRequest{
		SegmentId: &pb.SegmentId{
			Uuid: segmentID,
		},
		Data:        data,
		Offset:      offset,
		SequenceNum: seqNum,
	}

	type ack struct {
		persisted int64
		err       error
	}
	done := make(chan ack, 1)
	go func() {
		rs.sendMu.Lock()
		defer rs.sendMu.Unlock()

		if err := rs.stream.Send(req); err != nil {
			done <- ack{err: fmt.Errorf("send replicate entry to %s: %w", pc.address, err)}
			return
		}
		resp, err := rs.stream.Recv()
		if err != nil {
			done <- ack{err: fmt.Errorf("recv replicate ack from %s: %w", pc.address, err)}
			return
		}
		if resp.GetAckedSequenceNum() != seqNum {
			done <- ack{err: fmt.Errorf("sequence mismatch from %s: expected %d, got %d",
				pc.address, seqNum, resp.GetAckedSequenceNum())}
			return
		}
		done <- ack{persisted: resp.GetPersistedOffset()}
	}()

	timeout := pc.callTimeout()
	timer := time.NewTimer(timeout)
	defer timer.Stop()

	select {
	case a := <-done:
		if a.err != nil {
			pc.discardStream(segmentID, rs)
			return 0, a.err
		}
		return a.persisted, nil
	case <-ctx.Done():
		pc.discardStream(segmentID, rs)
		return 0, fmt.Errorf("replicating to %s for segment %s: %w", pc.address, segmentID, ctx.Err())
	case <-timer.C:
		pc.discardStream(segmentID, rs)
		return 0, fmt.Errorf("replicating to %s for segment %s: no ack within %s",
			pc.address, segmentID, timeout)
	}
}

// IsSegmentMissing reports whether err says the peer does not know the segment
// (it was never prepared, or the peer restarted and lost it), in which case the
// segment has to be prepared again before it can be replayed.
func IsSegmentMissing(err error) bool {
	if err == nil {
		return false
	}
	if status.Code(err) == codes.NotFound {
		return true
	}
	// The error may have been wrapped past the point where the gRPC status is
	// recoverable, so fall back to the message.
	s := err.Error()
	return strings.Contains(s, "NotFound") || strings.Contains(s, "not found")
}

// getOrCreateStream returns an existing ReplicateEntries stream for the segment,
// or creates a new one if none exists.
//
// Opening the stream is bounded and happens without streamsMu held: a peer that
// completes the TCP handshake and then goes silent leaves the connection in
// CONNECTING, where gRPC's fail-fast does not apply, so an unbounded attempt
// would block every other segment's replication behind this one.
func (pc *PeerClient) getOrCreateStream(segmentID string) (*replicateStream, error) {
	pc.streamsMu.Lock()
	rs, ok := pc.streams[segmentID]
	pc.streamsMu.Unlock()
	if ok {
		return rs, nil
	}

	client, err := pc.ensureConnected()
	if err != nil {
		return nil, err
	}

	// The stream context is long-lived — it persists across entries — but it
	// is cancelled whenever the stream is discarded, which is what unblocks a
	// call left waiting on a wedged peer.
	streamCtx, cancel := context.WithCancel(context.Background())

	type opened struct {
		stream pb.WalQuorumPeer_ReplicateEntriesClient
		err    error
	}
	ch := make(chan opened, 1)
	go func() {
		stream, err := client.ReplicateEntries(streamCtx)
		ch <- opened{stream: stream, err: err}
	}()

	timeout := pc.callTimeout()
	timer := time.NewTimer(timeout)
	defer timer.Stop()

	select {
	case o := <-ch:
		if o.err != nil {
			cancel()
			return nil, fmt.Errorf("open ReplicateEntries stream to %s for segment %s: %w",
				pc.address, segmentID, o.err)
		}
		newStream := &replicateStream{stream: o.stream, cancel: cancel}

		pc.streamsMu.Lock()
		if existing, raced := pc.streams[segmentID]; raced {
			pc.streamsMu.Unlock()
			_ = o.stream.CloseSend()
			cancel()
			return existing, nil
		}
		pc.streams[segmentID] = newStream
		pc.streamsMu.Unlock()

		pc.logger.Debug("opened replication stream", "segment_id", segmentID)
		return newStream, nil
	case <-timer.C:
		// Abandon the attempt; cancelling releases the goroutine and the
		// half-open stream once gRPC gives up on it.
		cancel()
		return nil, fmt.Errorf("open ReplicateEntries stream to %s for segment %s: no stream within %s",
			pc.address, segmentID, timeout)
	}
}

// discardStream tears down a stream that failed or stopped answering, unless
// it has already been replaced. Cancelling the stream context is what unblocks
// a Send or Recv that is waiting on a wedged peer.
func (pc *PeerClient) discardStream(segmentID string, rs *replicateStream) {
	pc.streamsMu.Lock()
	current, ok := pc.streams[segmentID]
	if ok && current == rs {
		delete(pc.streams, segmentID)
	}
	pc.streamsMu.Unlock()

	if ok && current == rs {
		_ = rs.stream.CloseSend()
		rs.cancel()
	}
}

// closeStream closes and removes the replication stream for a segment.
func (pc *PeerClient) closeStream(segmentID string) {
	pc.streamsMu.Lock()
	defer pc.streamsMu.Unlock()

	if rs, ok := pc.streams[segmentID]; ok {
		_ = rs.stream.CloseSend()
		rs.cancel()
		delete(pc.streams, segmentID)
	}
}

// CloseSegmentStream closes the replication stream for a segment after sealing.
// This should be called after SealSegment completes.
func (pc *PeerClient) CloseSegmentStream(segmentID string) {
	pc.closeStream(segmentID)
}

// SealSegment tells the peer to seal its replica and verify the checksum.
// Returns (success, error). success is true if the peer sealed and checksums match.
func (pc *PeerClient) SealSegment(ctx context.Context, segmentID string, finalOffset int64, checksum []byte) (bool, error) {
	// Close the replication stream for this segment first.
	pc.closeStream(segmentID)

	client, err := pc.ensureConnected()
	if err != nil {
		return false, err
	}

	resp, err := client.SealSegment(ctx, &pb.SealSegmentRequest{
		SegmentId: &pb.SegmentId{
			Uuid: segmentID,
		},
		ExpectedChecksum: checksum,
		ExpectedSize:     finalOffset,
	})
	if err != nil {
		return false, fmt.Errorf("SealSegment RPC to %s: %w", pc.address, err)
	}

	return resp.GetSuccess(), nil
}

// SyncSegment is a no-op on the peer side for now.
// The peer's data is durable after the ReplicateEntries ack (which writes and fsyncs).
// A dedicated peer SyncSegment RPC can be added later for explicit fsync coordination.
// IMPORTANT: Do NOT call SealSegment here — that permanently seals the segment!
func (pc *PeerClient) SyncSegment(ctx context.Context, segmentID string) error {
	// No-op: peer data is already durable after replicate acks.
	// IMPORTANT: Do NOT call SealSegment here — that permanently seals the segment!
	return nil
}

// ReadSegment streams a segment's data from the peer starting at the given offset.
// The caller must close the returned ReadCloser when done.
func (pc *PeerClient) ReadSegment(ctx context.Context, segmentID string, offset int64) (io.ReadCloser, error) {
	client, err := pc.ensureConnected()
	if err != nil {
		return nil, err
	}

	stream, err := client.ReadSegment(ctx, &pb.ReadSegmentRequest{
		SegmentId: &pb.SegmentId{
			Uuid: segmentID,
		},
		Offset: offset,
	})
	if err != nil {
		return nil, fmt.Errorf("ReadSegment RPC to %s: %w", pc.address, err)
	}

	return &segmentReader{stream: stream}, nil
}

// segmentReader adapts a streaming ReadSegment response into io.ReadCloser.
type segmentReader struct {
	stream pb.WalQuorumPeer_ReadSegmentClient
	buf    []byte
	done   bool
}

func (sr *segmentReader) Read(p []byte) (int, error) {
	// Drain any buffered data from previous chunk first.
	if len(sr.buf) > 0 {
		n := copy(p, sr.buf)
		sr.buf = sr.buf[n:]
		return n, nil
	}

	if sr.done {
		return 0, io.EOF
	}

	chunk, err := sr.stream.Recv()
	if err == io.EOF {
		sr.done = true
		return 0, io.EOF
	}
	if err != nil {
		return 0, err
	}

	if chunk.GetLast() {
		sr.done = true
	}

	data := chunk.GetData()
	n := copy(p, data)
	if n < len(data) {
		sr.buf = data[n:]
	}
	if sr.done && len(sr.buf) == 0 {
		return n, io.EOF
	}
	return n, nil
}

func (sr *segmentReader) Close() error {
	sr.done = true
	sr.buf = nil
	return nil
}

// ListSegments queries the peer for all segment metadata matching the given WAL path prefix.
func (pc *PeerClient) ListSegments(ctx context.Context, walPathPrefix string) ([]SegmentInfo, error) {
	client, err := pc.ensureConnected()
	if err != nil {
		return nil, err
	}

	resp, err := client.ListSegments(ctx, &pb.ListSegmentsRequest{
		OriginatorPod: walPathPrefix,
	})
	if err != nil {
		return nil, fmt.Errorf("ListSegments RPC to %s: %w", pc.address, err)
	}

	var result []SegmentInfo
	for _, info := range resp.GetSegments() {
		result = append(result, SegmentInfo{
			ID:            info.GetSegmentId().GetUuid(),
			WALPath:       info.GetSegmentId().GetWalPath(),
			OriginatorPod: info.GetOriginatorPod(),
			Size:          info.GetSize(),
			Sealed:        info.GetSealed(),
		})
	}
	return result, nil
}

// PurgeSegment tells the peer to delete a segment file.
func (pc *PeerClient) PurgeSegment(ctx context.Context, segmentID string) error {
	client, err := pc.ensureConnected()
	if err != nil {
		return err
	}

	resp, err := client.PurgeSegment(ctx, &pb.PurgeSegmentRequest{
		SegmentId: &pb.SegmentId{
			Uuid: segmentID,
		},
	})
	if err != nil {
		return fmt.Errorf("PurgeSegment RPC to %s: %w", pc.address, err)
	}
	if !resp.GetSuccess() {
		return fmt.Errorf("PurgeSegment on %s failed: %s", pc.address, resp.GetError())
	}
	return nil
}

// Close shuts down all open streams and the gRPC connection.
func (pc *PeerClient) Close() {
	// Close all replication streams.
	pc.streamsMu.Lock()
	for segID, rs := range pc.streams {
		_ = rs.stream.CloseSend()
		rs.cancel()
		delete(pc.streams, segID)
	}
	pc.streamsMu.Unlock()

	// Close the gRPC connection.
	pc.mu.Lock()
	defer pc.mu.Unlock()
	if pc.conn != nil {
		_ = pc.conn.Close()
		pc.conn = nil
		pc.client = nil
		pc.logger.Info("peer connection closed")
	}
}

// IsHealthy returns true if the underlying gRPC connection is alive
// (READY or IDLE state). Returns false if no connection has been established.
func (pc *PeerClient) IsHealthy() bool {
	pc.mu.Lock()
	defer pc.mu.Unlock()

	if pc.conn == nil {
		return false
	}

	state := pc.conn.GetState()
	return state == connectivity.Ready || state == connectivity.Idle
}
