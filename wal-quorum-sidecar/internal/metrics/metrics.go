// Licensed to the Apache Software Foundation (ASF) under one
// or more contributor license agreements.
package metrics

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

const (
	namespace = "wal_quorum"
)

var (
	// WriteLatency measures the time to write an entry to the local segment file.
	WriteLatency = promauto.NewHistogram(prometheus.HistogramOpts{
		Namespace: namespace,
		Name:      "write_latency_seconds",
		Help:      "Latency of local WAL segment writes in seconds.",
		Buckets:   prometheus.ExponentialBuckets(0.00001, 2, 20), // 10us to ~10s
	})

	// QuorumLatency measures the end-to-end time from receiving a write entry
	// to having quorum acks from all required replicas.
	QuorumLatency = promauto.NewHistogram(prometheus.HistogramOpts{
		Namespace: namespace,
		Name:      "quorum_latency_seconds",
		Help:      "End-to-end latency for quorum-confirmed writes in seconds.",
		Buckets:   prometheus.ExponentialBuckets(0.0001, 2, 18), // 100us to ~26s
	})

	// SegmentsOpen is a gauge of currently open (writable) segments.
	SegmentsOpen = promauto.NewGauge(prometheus.GaugeOpts{
		Namespace: namespace,
		Name:      "segments_open",
		Help:      "Number of WAL segments currently open for writing.",
	})

	// SegmentReopens counts repeat OpenSegment calls that were served
	// idempotently from an existing live segment (i.e. a retried WAL open).
	SegmentReopens = promauto.NewCounter(prometheus.CounterOpts{
		Namespace: namespace,
		Name:      "segment_reopens_total",
		Help:      "Total number of repeat segment opens served from an existing live segment.",
	})

	// SegmentOpenConflicts counts opens refused because the segment id is held
	// by a different owner, role, or incarnation.
	SegmentOpenConflicts = promauto.NewCounter(prometheus.CounterOpts{
		Namespace: namespace,
		Name:      "segment_open_conflicts_total",
		Help:      "Total number of segment opens refused due to an ownership conflict.",
	})

	// ReplicaCatchups counts replica ranges replayed to a peer after entries
	// failed to reach it (a failed write, or one dropped while the failure
	// breaker had the peer shed).
	ReplicaCatchups = promauto.NewCounter(prometheus.CounterOpts{
		Namespace: namespace,
		Name:      "replica_catchups_total",
		Help:      "Total number of peer replica catch-up replays performed.",
	})

	// SegmentsSealed is a counter of segments that have been sealed.
	SegmentsSealed = promauto.NewCounter(prometheus.CounterOpts{
		Namespace: namespace,
		Name:      "segments_sealed_total",
		Help:      "Total number of WAL segments sealed.",
	})

	// PeerConnections is a gauge of active gRPC connections to peer sidecars.
	PeerConnections = promauto.NewGauge(prometheus.GaugeOpts{
		Namespace: namespace,
		Name:      "peer_connections",
		Help:      "Number of active peer-to-peer gRPC connections.",
	})

	// ReplicationLagBytes tracks the byte difference between the local segment
	// write offset and the last acked offset from each peer.
	ReplicationLagBytes = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: namespace,
		Name:      "replication_lag_bytes",
		Help:      "Replication lag in bytes per peer.",
	}, []string{"peer"})

	// EntriesWrittenTotal counts the total number of WAL entries written locally.
	EntriesWrittenTotal = promauto.NewCounter(prometheus.CounterOpts{
		Namespace: namespace,
		Name:      "entries_written_total",
		Help:      "Total number of WAL entries written to local segments.",
	})

	// BytesWrittenTotal counts the total bytes written to local segments.
	BytesWrittenTotal = promauto.NewCounter(prometheus.CounterOpts{
		Namespace: namespace,
		Name:      "bytes_written_total",
		Help:      "Total bytes written to local WAL segments.",
	})

	// SyncLatency measures the time for fdatasync calls.
	SyncLatency = promauto.NewHistogram(prometheus.HistogramOpts{
		Namespace: namespace,
		Name:      "sync_latency_seconds",
		Help:      "Latency of fdatasync calls on WAL segments.",
		Buckets:   prometheus.ExponentialBuckets(0.0001, 2, 16), // 100us to ~3s
	})

	// GCSUploadLatency measures the time to upload a sealed segment to GCS.
	GCSUploadLatency = promauto.NewHistogram(prometheus.HistogramOpts{
		Namespace: namespace,
		Name:      "gcs_upload_latency_seconds",
		Help:      "Latency of sealed segment uploads to GCS.",
		Buckets:   prometheus.ExponentialBuckets(0.01, 2, 14), // 10ms to ~160s
	})

	// GCSUploadErrors counts failed GCS uploads.
	GCSUploadErrors = promauto.NewCounter(prometheus.CounterOpts{
		Namespace: namespace,
		Name:      "gcs_upload_errors_total",
		Help:      "Total number of failed GCS segment uploads.",
	})

	// GCSUploadsPending is a gauge of segment uploads currently queued or in-flight.
	GCSUploadsPending = promauto.NewGauge(prometheus.GaugeOpts{
		Namespace: namespace,
		Name:      "gcs_uploads_pending",
		Help:      "Number of segment uploads currently queued or in-flight.",
	})
)

// Init is a no-op that forces the init() registration of all metrics
// via promauto. Call this early in main() to ensure metrics are registered
// before any Prometheus scrape.
func Init() {}
