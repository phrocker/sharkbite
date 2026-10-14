// Licensed to the Apache Software Foundation (ASF) under one
// or more contributor license agreements.
package gcs

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"sync"
	"time"

	"cloud.google.com/go/storage"

	"github.com/accumulo/wal-quorum-sidecar/internal/metrics"
	"github.com/accumulo/wal-quorum-sidecar/internal/segment"
)

const (
	// uploadChunkSize is the buffer size for streaming segment data to GCS.
	uploadChunkSize = 1 << 20 // 1 MB

	// maxRetries is the maximum number of upload attempts before giving up.
	maxRetries = 3
)

// uploadRequest holds the data needed to upload a single segment to GCS.
type uploadRequest struct {
	seg           *segment.Segment
	originatorPod string
}

// UploadResult is sent on the result channel after each upload completes.
type UploadResult struct {
	SegmentID string
	GCSPath   string
	Err       error
}

// Uploader handles asynchronous upload of sealed WAL segments to GCS.
// It maintains a pool of background worker goroutines that drain a shared
// upload queue. Use NewUploader to create and start workers.
type Uploader struct {
	bucket  string
	prefix  string
	workers int
	client  *storage.Client
	logger  *slog.Logger

	queue   chan uploadRequest
	results chan UploadResult
	wg      sync.WaitGroup
	cancel  context.CancelFunc
}

// NewUploader creates an Uploader and starts N background worker goroutines.
// The GCS client uses Application Default Credentials, which on GKE are
// provided by workload identity.
func NewUploader(ctx context.Context, bucket, prefix string, workers int, logger *slog.Logger) (*Uploader, error) {
	client, err := storage.NewClient(ctx)
	if err != nil {
		return nil, fmt.Errorf("create GCS client: %w", err)
	}

	workerCtx, cancel := context.WithCancel(ctx)

	u := &Uploader{
		bucket:  bucket,
		prefix:  prefix,
		workers: workers,
		client:  client,
		logger:  logger.With("component", "gcs-uploader"),
		queue:   make(chan uploadRequest, 256),
		results: make(chan UploadResult, 256),
		cancel:  cancel,
	}

	for i := 0; i < workers; i++ {
		u.wg.Add(1)
		go u.run(workerCtx, i)
	}

	u.logger.Info("GCS uploader started", "bucket", bucket, "prefix", prefix, "workers", workers)
	return u, nil
}

// QueueUpload enqueues a sealed segment for asynchronous upload to GCS.
// This method is non-blocking; if the queue is full, it logs a warning and
// drops the request (the segment remains on local disk for retry later).
func (u *Uploader) QueueUpload(seg *segment.Segment, originatorPod string) {
	req := uploadRequest{
		seg:           seg,
		originatorPod: originatorPod,
	}

	select {
	case u.queue <- req:
		metrics.GCSUploadsPending.Inc()
		u.logger.Info("segment queued for GCS upload",
			"segment_id", seg.ID(),
			"originator", originatorPod,
		)
	default:
		u.logger.Warn("GCS upload queue full, dropping upload request",
			"segment_id", seg.ID(),
			"originator", originatorPod,
		)
		metrics.GCSUploadErrors.Inc()
	}
}

// Results returns the channel on which upload results are published.
// Consumers (e.g. the purger) can read from this channel to learn when
// uploads complete and trigger post-upload cleanup.
func (u *Uploader) Results() <-chan UploadResult {
	return u.results
}

// run is the background loop for a single worker goroutine. It processes
// upload requests from the queue until the context is cancelled.
func (u *Uploader) run(ctx context.Context, workerID int) {
	defer u.wg.Done()

	u.logger.Debug("upload worker started", "worker_id", workerID)
	for {
		select {
		case <-ctx.Done():
			u.logger.Debug("upload worker stopping", "worker_id", workerID)
			return
		case req, ok := <-u.queue:
			if !ok {
				return
			}
			metrics.GCSUploadsPending.Dec()

			gcsPath := u.objectPath(req.originatorPod, req.seg.ID())
			err := u.uploadWithRetry(ctx, req.seg, gcsPath)

			result := UploadResult{
				SegmentID: req.seg.ID(),
				GCSPath:   fmt.Sprintf("gs://%s/%s", u.bucket, gcsPath),
			}
			if err != nil {
				result.Err = err
				metrics.GCSUploadErrors.Inc()
				u.logger.Error("GCS upload failed after retries",
					"segment_id", req.seg.ID(),
					"gcs_path", gcsPath,
					"error", err,
				)
			} else {
				u.logger.Info("GCS upload complete",
					"segment_id", req.seg.ID(),
					"gcs_path", gcsPath,
				)
			}

			// Publish result; drop if no consumer is reading.
			select {
			case u.results <- result:
			default:
				u.logger.Warn("upload result channel full, dropping result",
					"segment_id", req.seg.ID(),
				)
			}
		}
	}
}

// uploadWithRetry attempts to upload the segment with exponential backoff.
// It retries up to maxRetries times with delays of 1s, 2s, 4s.
func (u *Uploader) uploadWithRetry(ctx context.Context, seg *segment.Segment, gcsPath string) error {
	var lastErr error
	for attempt := 0; attempt < maxRetries; attempt++ {
		if attempt > 0 {
			backoff := time.Duration(1<<uint(attempt-1)) * time.Second
			u.logger.Info("retrying GCS upload",
				"segment_id", seg.ID(),
				"attempt", attempt+1,
				"backoff", backoff,
			)
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(backoff):
			}
		}

		lastErr = u.upload(ctx, seg, gcsPath)
		if lastErr == nil {
			return nil
		}
		u.logger.Warn("GCS upload attempt failed",
			"segment_id", seg.ID(),
			"attempt", attempt+1,
			"error", lastErr,
		)
	}
	return fmt.Errorf("upload failed after %d attempts: %w", maxRetries, lastErr)
}

// upload streams a single segment file to GCS in 1 MB chunks.
func (u *Uploader) upload(ctx context.Context, seg *segment.Segment, gcsPath string) error {
	start := time.Now()

	filePath := seg.FilePath()
	if filePath == "" {
		return fmt.Errorf("segment %s has no file path", seg.ID())
	}

	f, err := os.Open(filePath)
	if err != nil {
		return fmt.Errorf("open segment file %s: %w", filePath, err)
	}
	defer f.Close()

	obj := u.client.Bucket(u.bucket).Object(gcsPath)
	writer := obj.NewWriter(ctx)
	writer.ChunkSize = uploadChunkSize
	writer.ContentType = "application/octet-stream"

	buf := make([]byte, uploadChunkSize)
	for {
		n, readErr := f.Read(buf)
		if n > 0 {
			if _, writeErr := writer.Write(buf[:n]); writeErr != nil {
				// Abort the upload on write failure.
				_ = writer.Close()
				return fmt.Errorf("write to GCS object %s: %w", gcsPath, writeErr)
			}
		}
		if readErr == io.EOF {
			break
		}
		if readErr != nil {
			_ = writer.Close()
			return fmt.Errorf("read segment file %s: %w", filePath, readErr)
		}
	}

	if err := writer.Close(); err != nil {
		return fmt.Errorf("close GCS writer for %s: %w", gcsPath, err)
	}

	// Verify the upload succeeded by checking object attributes.
	attrs, err := obj.Attrs(ctx)
	if err != nil {
		return fmt.Errorf("verify GCS upload attrs for %s: %w", gcsPath, err)
	}

	expectedSize := seg.Offset()
	if attrs.Size != expectedSize {
		return fmt.Errorf("GCS upload size mismatch for %s: uploaded %d bytes, expected %d",
			gcsPath, attrs.Size, expectedSize)
	}

	elapsed := time.Since(start)
	metrics.GCSUploadLatency.Observe(elapsed.Seconds())

	u.logger.Debug("segment uploaded to GCS",
		"segment_id", seg.ID(),
		"gcs_path", gcsPath,
		"size", attrs.Size,
		"duration", elapsed,
	)

	return nil
}

// objectPath constructs the GCS object path for a segment.
// Format: <prefix>/<originatorPod>/<segmentID>.wal
func (u *Uploader) objectPath(originatorPod, segmentID string) string {
	if u.prefix != "" {
		return fmt.Sprintf("%s/%s/%s.wal", u.prefix, originatorPod, segmentID)
	}
	return fmt.Sprintf("%s/%s.wal", originatorPod, segmentID)
}

// ObjectExists checks whether a GCS object exists at the given path.
// Used by the purger to verify upload before deleting local copies.
func (u *Uploader) ObjectExists(ctx context.Context, gcsObjectPath string) (bool, error) {
	obj := u.client.Bucket(u.bucket).Object(gcsObjectPath)
	_, err := obj.Attrs(ctx)
	if err == storage.ErrObjectNotExist {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("check GCS object %s: %w", gcsObjectPath, err)
	}
	return true, nil
}

// Close drains the upload queue and shuts down all worker goroutines.
// It blocks until all in-flight uploads complete or the context used
// to create the uploader is cancelled.
func (u *Uploader) Close() error {
	u.cancel()
	u.wg.Wait()
	close(u.results)

	if err := u.client.Close(); err != nil {
		return fmt.Errorf("close GCS client: %w", err)
	}

	u.logger.Info("GCS uploader stopped")
	return nil
}
