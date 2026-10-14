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

package offlinecompact

import (
	"context"
	"fmt"
	"io"

	"github.com/phrocker/shoal-oss/internal/storage"
)

// syncer is implemented by writers whose underlying object supports an
// explicit durability barrier (e.g. *os.File.Sync on the local backend).
// Cloud backends commit on Close instead and do not implement it.
type syncer interface{ Sync() error }

// BackendStore adapts a storage.Backend into the RFileStore the
// orchestrator needs: reads pull a whole RFile image, writes publish the
// compacted output durably.
//
// The backend must implement storage.WritableBackend for writes; if it
// does not, Write returns storage.ErrReadOnly. On backends whose Writer
// supports Sync (local filesystem), Write fsyncs before returning so the
// output RFile is durable before oc-commit ever references it — the
// design's write-then-commit ordering depends on the write being on
// stable storage first.
type BackendStore struct {
	Backend storage.Backend
}

// NewBackendStore wraps a storage.Backend.
func NewBackendStore(b storage.Backend) *BackendStore {
	return &BackendStore{Backend: b}
}

// Read returns the whole image at path.
func (s *BackendStore) Read(ctx context.Context, path string) ([]byte, error) {
	return storage.ReadAll(ctx, s.Backend, path)
}

// Write publishes data at path durably. It creates + writes + fsyncs (if
// the backend's Writer supports it) + closes. Any error leaves no
// committed metadata ref pointing at path, so a failed write is a safe
// no-op (the partial file, if any, is unreferenced and GC-reclaimable).
func (s *BackendStore) Write(ctx context.Context, path string, data []byte) (err error) {
	wb, ok := s.Backend.(storage.WritableBackend)
	if !ok {
		return storage.ErrReadOnly
	}
	w, err := wb.Create(ctx, path)
	if err != nil {
		return fmt.Errorf("offlinecompact: create %s: %w", path, err)
	}
	var cleanupState storage.WriteCleanupState
	defer func() { storage.AbortOnError(&err, w, &cleanupState) }()
	if err = writeAll(w, data); err != nil {
		return fmt.Errorf("offlinecompact: write %s: %w", path, err)
	}
	if sy, ok := w.(syncer); ok {
		if err = sy.Sync(); err != nil {
			return fmt.Errorf("offlinecompact: fsync %s: %w", path, err)
		}
	}
	cleanupState.MarkCloseAttempted()
	if err = w.Close(); err != nil {
		return fmt.Errorf("offlinecompact: close %s: %w", path, err)
	}
	return nil
}

// writeAll writes every byte of data, tolerating a Writer that returns
// short counts. A zero-progress write with no error is treated as a
// failure rather than looping forever, so a misbehaving backend can never
// silently leave a truncated RFile that verification (which uses the
// in-memory image) would still pass and oc-commit would later reference.
func writeAll(w io.Writer, data []byte) error {
	for len(data) > 0 {
		n, err := w.Write(data)
		if n < 0 || n > len(data) {
			return fmt.Errorf("writer returned invalid count %d (remaining %d)", n, len(data))
		}
		data = data[n:]
		if err != nil {
			return err
		}
		if n == 0 && len(data) > 0 {
			return fmt.Errorf("writer made no progress with %d bytes remaining", len(data))
		}
	}
	return nil
}
