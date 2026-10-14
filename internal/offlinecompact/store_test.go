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
	"bytes"
	"context"
	"errors"
	"io"
	"testing"

	"github.com/phrocker/shoal-oss/internal/storage"
	"github.com/phrocker/shoal-oss/internal/storage/memory"
)

func TestBackendStore_WriteThenRead(t *testing.T) {
	s := NewBackendStore(memory.New())
	want := []byte("rfile-image-bytes")
	if err := s.Write(context.Background(), "tables/2k/t-abc/Aout.rf", want); err != nil {
		t.Fatalf("Write: %v", err)
	}
	got, err := s.Read(context.Background(), "tables/2k/t-abc/Aout.rf")
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if string(got) != string(want) {
		t.Fatalf("roundtrip mismatch: got %q", got)
	}
}

// readOnlyBackend implements storage.Backend but not WritableBackend.
type readOnlyBackend struct{}

func (readOnlyBackend) Open(_ context.Context, _ string) (storage.File, error) {
	return nil, storage.ErrNotFound
}

func TestBackendStore_WriteReadOnlyBackend(t *testing.T) {
	s := NewBackendStore(readOnlyBackend{})
	err := s.Write(context.Background(), "x.rf", []byte("data"))
	if !errors.Is(err, storage.ErrReadOnly) {
		t.Fatalf("want ErrReadOnly, got %v", err)
	}
}

func TestBackendStore_ReadMissing(t *testing.T) {
	s := NewBackendStore(memory.New())
	if _, err := s.Read(context.Background(), "nope.rf"); err == nil {
		t.Fatal("expected not-found error")
	}
}

func TestBackendStore_WriteAbortsOnWriteFailure(t *testing.T) {
	backend := newTrackingWritableBackend()
	backend.files["x.rf"] = []byte("old")
	backend.writeLimit = 2
	backend.writeErr = errors.New("write failed")

	s := NewBackendStore(backend)
	err := s.Write(context.Background(), "x.rf", []byte("data"))
	if !errors.Is(err, backend.writeErr) {
		t.Fatalf("Write error = %v, want %v", err, backend.writeErr)
	}
	if got := string(backend.files["x.rf"]); got != "old" {
		t.Fatalf("destination contents = %q, want old", got)
	}
	if backend.lastWriter == nil || backend.lastWriter.abortCalls != 1 {
		t.Fatalf("Abort calls = %d, want 1", backend.lastWriter.abortCalls)
	}
	if backend.lastWriter.stagePresent {
		t.Fatal("failed write left staged bytes behind")
	}
	if backend.lastWriter.syncCalls != 0 {
		t.Fatalf("Sync calls = %d, want 0 after write failure", backend.lastWriter.syncCalls)
	}
}

func TestBackendStore_WriteAbortsOnSyncFailureAndPreservesTarget(t *testing.T) {
	backend := newTrackingWritableBackend()
	backend.files["x.rf"] = []byte("old")
	backend.syncErr = errors.New("sync failed")

	s := NewBackendStore(backend)
	err := s.Write(context.Background(), "x.rf", []byte("data"))
	if !errors.Is(err, backend.syncErr) {
		t.Fatalf("Write error = %v, want %v", err, backend.syncErr)
	}
	if got := string(backend.files["x.rf"]); got != "old" {
		t.Fatalf("destination contents = %q, want old", got)
	}
	if backend.lastWriter == nil || backend.lastWriter.abortCalls != 1 {
		t.Fatalf("Abort calls = %d, want 1", backend.lastWriter.abortCalls)
	}
	if backend.lastWriter.syncCalls != 1 {
		t.Fatalf("Sync calls = %d, want 1", backend.lastWriter.syncCalls)
	}
	if backend.lastWriter.stagePresent {
		t.Fatal("failed sync left staged bytes behind")
	}
}

type trackingWritableBackend struct {
	files      map[string][]byte
	lastWriter *trackingWriter
	writeLimit int
	writeErr   error
	syncErr    error
}

func newTrackingWritableBackend() *trackingWritableBackend {
	return &trackingWritableBackend{
		files:      make(map[string][]byte),
		writeLimit: -1,
	}
}

func (b *trackingWritableBackend) Open(context.Context, string) (storage.File, error) {
	return nil, storage.ErrNotFound
}

func (b *trackingWritableBackend) Create(_ context.Context, path string) (storage.Writer, error) {
	writer := &trackingWriter{
		backend:    b,
		path:       path,
		writeLimit: b.writeLimit,
		writeErr:   b.writeErr,
		syncErr:    b.syncErr,
	}
	b.lastWriter = writer
	return writer, nil
}

type trackingWriter struct {
	backend      *trackingWritableBackend
	path         string
	stage        bytes.Buffer
	stagePresent bool
	writeLimit   int
	writeErr     error
	syncErr      error
	closeCalls   int
	abortCalls   int
	syncCalls    int
	closed       bool
	aborted      bool
}

func (w *trackingWriter) Write(p []byte) (int, error) {
	w.stagePresent = true
	if w.writeErr != nil && w.writeLimit >= 0 {
		remaining := w.writeLimit - w.stage.Len()
		if remaining <= 0 {
			return 0, w.writeErr
		}
		if remaining < len(p) {
			_, _ = w.stage.Write(p[:remaining])
			return remaining, w.writeErr
		}
	}
	return w.stage.Write(p)
}

func (w *trackingWriter) Sync() error {
	w.syncCalls++
	return w.syncErr
}

func (w *trackingWriter) Close() error {
	w.closeCalls++
	if w.closed {
		return nil
	}
	w.closed = true
	w.stagePresent = false
	w.backend.files[w.path] = append([]byte(nil), w.stage.Bytes()...)
	return nil
}

func (w *trackingWriter) Abort() error {
	w.abortCalls++
	w.aborted = true
	w.stagePresent = false
	w.stage.Reset()
	return nil
}

var _ storage.Aborter = (*trackingWriter)(nil)
var _ io.Writer = (*trackingWriter)(nil)
var _ io.Writer = (*trackingWriter)(nil)
