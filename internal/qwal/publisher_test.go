// Licensed to the Apache Software Foundation (ASF) under one
// or more contributor license agreements. See the NOTICE file
// distributed with this work for additional information
// regarding copyright ownership. The ASF licenses this file
// to you under the Apache License, Version 2.0 (the
// "License"); you may not use this file except in compliance
// with the License. You may obtain a copy of the License at
//
//   https://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing,
// software distributed under the License is distributed on an
// "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY
// KIND, either express or implied. See the License for the
// specific language governing permissions and limitations
// under the License.

package qwal

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"io"
	"testing"
	"time"

	"github.com/phrocker/shoal-oss/internal/storage"
	"github.com/phrocker/shoal-oss/internal/storage/memory"
)

const testSegmentID = "11111111-1111-4111-8111-111111111111"
const testSegmentPath = "hdfs://namenode:8020/accumulo/wal/server.example+9997/" + testSegmentID

func validSegmentRequest() NativeSegmentRequest {
	extent := KeyExtent{TableID: "5", PrevEndRow: []byte("a"), EndRow: []byte("z")}
	return NativeSegmentRequest{
		Volume: "hdfs://namenode:8020/accumulo", ServerAddress: "server.example:9997",
		ID: testSegmentID, Extent: extent,
		Entries: []Entry{
			{Key: LogFileKey{Event: EventOpen, TServerSession: "session"}},
			{Key: LogFileKey{Event: EventDefineTablet, TabletID: 7, Seq: 7, Tablet: &extent}},
			{Key: LogFileKey{Event: EventMutation, TabletID: 7, Seq: 8}, Value: LogFileValue{Mutations: []ServerMutation{{
				Row: []byte("m"), Updates: []ColumnUpdate{{ColumnFamily: []byte("cf"), Value: []byte("v")}},
			}}}},
		},
	}
}

type segmentFaultStore struct {
	*memory.Backend
	createErr, openErr, readErr, closeErr              error
	shortWrite, corruptOnClose, skipCommit, blockWrite bool
	writeEntered                                       chan struct{}
	creates, aborts                                    int
}

func newSegmentFaultStore() *segmentFaultStore {
	return &segmentFaultStore{Backend: memory.New()}
}

func (s *segmentFaultStore) CreateNativeSegmentExclusive(ctx context.Context, path string) (storage.AbortableWriter, error) {
	s.creates++
	if s.createErr != nil {
		return nil, s.createErr
	}
	w, err := s.Backend.CreateExclusive(ctx, path)
	if err != nil {
		return nil, err
	}
	return &segmentFaultWriter{Writer: w, store: s, path: path}, nil
}

func (s *segmentFaultStore) Open(ctx context.Context, path string) (storage.File, error) {
	if s.openErr != nil {
		return nil, s.openErr
	}
	f, err := s.Backend.Open(ctx, path)
	if err != nil {
		return nil, err
	}
	if s.readErr != nil {
		return &segmentFaultFile{File: f, err: s.readErr}, nil
	}
	return f, nil
}

type segmentFaultFile struct {
	storage.File
	err error
}

func (f *segmentFaultFile) ReadAt([]byte, int64) (int, error) { return 0, f.err }

type segmentFaultWriter struct {
	storage.Writer
	store          *segmentFaultStore
	path           string
	closeAttempted bool
}

func (w *segmentFaultWriter) Write(p []byte) (int, error) {
	if w.store.shortWrite {
		return w.Writer.Write(p[:len(p)/2])
	}
	return w.Writer.Write(p)
}

func (w *segmentFaultWriter) WriteContext(ctx context.Context, p []byte) (int, error) {
	if w.store.blockWrite {
		close(w.store.writeEntered)
		<-ctx.Done()
		return 0, ctx.Err()
	}
	return w.Write(p)
}

func (w *segmentFaultWriter) Close() error {
	w.closeAttempted = true
	if w.store.skipCommit {
		return w.store.closeErr
	}
	err := w.Writer.Close()
	if err != nil {
		return err
	}
	if w.store.corruptOnClose {
		f, err := w.store.Backend.Open(context.Background(), w.path)
		if err != nil {
			return err
		}
		data := make([]byte, f.Size())
		if _, err := f.ReadAt(data, 0); err != nil {
			return err
		}
		_ = f.Close()
		data[len(data)-1] ^= 1
		w.store.Put(w.path, data)
	}
	return w.store.closeErr
}

func (w *segmentFaultWriter) Abort() error {
	w.store.aborts++
	if w.closeAttempted && !w.store.skipCommit {
		return nil
	}
	return w.Writer.(storage.Aborter).Abort()
}

func TestPublishNativeSegmentVerifiesClosedObject(t *testing.T) {
	store := newSegmentFaultStore()
	segment, err := PublishNativeSegment(context.Background(), store, validSegmentRequest())
	if err != nil {
		t.Fatal(err)
	}
	if segment.ID != testSegmentID || segment.Path != testSegmentPath {
		t.Fatalf("unexpected identity: %+v", segment)
	}
	f, err := store.Open(context.Background(), segment.Path)
	if err != nil {
		t.Fatal(err)
	}
	data := make([]byte, f.Size())
	if _, err := f.ReadAt(data, 0); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	if segment.Size != int64(len(data)) || segment.SHA256 != sha256.Sum256(data) {
		t.Fatal("segment result does not describe published bytes")
	}
	r := newDataInput(bytes.NewReader(data))
	if err := r.readHeader(); err != nil {
		t.Fatal(err)
	}
	for _, want := range validSegmentRequest().Entries {
		key, err := r.readKey()
		if err != nil || key.Event != want.Key.Event {
			t.Fatalf("native WAL key: %+v, %v", key, err)
		}
		if _, err := r.readValue(); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := r.readKey(); err != io.EOF {
		t.Fatalf("trailing native WAL data: %v", err)
	}
}

func TestPublishNativeSegmentRejectsInvalidInputBeforeCreate(t *testing.T) {
	cases := map[string]func(*NativeSegmentRequest){
		"local volume":         func(r *NativeSegmentRequest) { r.Volume = "/accumulo" },
		"other scheme":         func(r *NativeSegmentRequest) { r.Volume = "s3://bucket/accumulo" },
		"dot path":             func(r *NativeSegmentRequest) { r.Volume = "hdfs://nn/a/../accumulo" },
		"empty fragment":       func(r *NativeSegmentRequest) { r.Volume += "#" },
		"server traversal":     func(r *NativeSegmentRequest) { r.ServerAddress = "../host:9997" },
		"server URI injection": func(r *NativeSegmentRequest) { r.ServerAddress = "host?x:9997" },
		"noncanonical id":      func(r *NativeSegmentRequest) { r.ID = "11111111111141118111111111111111" },
		"empty table":          func(r *NativeSegmentRequest) { r.Extent.TableID = "" },
		"reversed extent":      func(r *NativeSegmentRequest) { r.Extent.PrevEndRow = []byte("z") },
		"different extent":     func(r *NativeSegmentRequest) { r.Extent.TableID = "6" },
		"wrong tablet":         func(r *NativeSegmentRequest) { r.Entries[2].Key.TabletID = 8 },
		"old sequence":         func(r *NativeSegmentRequest) { r.Entries[2].Key.Seq = 7 },
		"decreasing sequence": func(r *NativeSegmentRequest) {
			entry := r.Entries[2]
			entry.Key.Seq = 7
			r.Entries = append(r.Entries, entry)
		},
		"negative define seq": func(r *NativeSegmentRequest) { r.Entries[1].Key.Seq = -1 },
		"compaction event":    func(r *NativeSegmentRequest) { r.Entries[2].Key.Event = EventCompactionFinish },
		"row below extent":    func(r *NativeSegmentRequest) { r.Entries[2].Value.Mutations[0].Row = []byte("a") },
		"row above extent":    func(r *NativeSegmentRequest) { r.Entries[2].Value.Mutations[0].Row = []byte("zz") },
		"second row outside": func(r *NativeSegmentRequest) {
			r.Entries[2].Key.Event = EventManyMutations
			r.Entries[2].Value.Mutations = append(r.Entries[2].Value.Mutations, ServerMutation{Row: []byte("zz")})
		},
		"empty row":                func(r *NativeSegmentRequest) { r.Entries[2].Value.Mutations[0].Row = nil },
		"missing open":             func(r *NativeSegmentRequest) { r.Entries = r.Entries[1:] },
		"negative verify timeout":  func(r *NativeSegmentRequest) { r.VerifyTimeout = -time.Second },
		"excessive verify timeout": func(r *NativeSegmentRequest) { r.VerifyTimeout = 6 * time.Minute },
	}
	for name, change := range cases {
		t.Run(name, func(t *testing.T) {
			store := newSegmentFaultStore()
			req := validSegmentRequest()
			change(&req)
			if _, err := PublishNativeSegment(context.Background(), store, req); err == nil {
				t.Fatal("accepted invalid segment")
			}
			if store.creates != 0 {
				t.Fatal("created storage before validating segment")
			}
		})
	}
}

func TestPublishNativeSegmentFailureAndAmbiguity(t *testing.T) {
	createFailure := errors.New("create failed")
	closeFailure := errors.New("close outcome unknown")
	readFailure := errors.New("read unavailable")
	cases := []struct {
		name      string
		setup     func(*segmentFaultStore)
		wantErr   error
		wantAbort int
	}{
		{"create error", func(s *segmentFaultStore) { s.createErr = createFailure }, createFailure, 0},
		{"short write", func(s *segmentFaultStore) { s.shortWrite = true }, io.ErrShortWrite, 1},
		{"close committed then error", func(s *segmentFaultStore) { s.closeErr = closeFailure }, ErrSegmentUnverified, 1},
		{"close committed but corrupt", func(s *segmentFaultStore) { s.closeErr = closeFailure; s.corruptOnClose = true }, ErrSegmentUnverified, 1},
		{"close failed before commit", func(s *segmentFaultStore) { s.closeErr = closeFailure; s.skipCommit = true }, ErrSegmentUnverified, 1},
		{"close committed but unreadable", func(s *segmentFaultStore) { s.closeErr = closeFailure; s.openErr = readFailure }, ErrSegmentUnverified, 1},
		{"close success but unreadable", func(s *segmentFaultStore) { s.openErr = readFailure }, ErrSegmentUnverified, 0},
		{"close success but read fails", func(s *segmentFaultStore) { s.readErr = readFailure }, ErrSegmentUnverified, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := newSegmentFaultStore()
			tc.setup(s)
			segment, err := PublishNativeSegment(context.Background(), s, validSegmentRequest())
			if tc.wantErr == nil && err != nil || tc.wantErr != nil && !errors.Is(err, tc.wantErr) {
				t.Fatalf("publish error = %v, want %v", err, tc.wantErr)
			}
			if tc.name == "close committed then error" && !errors.Is(err, ErrSegmentVisible) {
				t.Fatalf("exact visible bytes were not reported: %v", err)
			}
			if tc.wantErr != nil && segment != (NativeSegment{}) {
				t.Fatalf("unverified publication returned segment %+v", segment)
			}
			if tc.wantErr == nil && segment.Path != testSegmentPath {
				t.Fatalf("reconciled segment = %+v", segment)
			}
			if s.aborts != tc.wantAbort {
				t.Fatalf("abort count %d, want %d", s.aborts, tc.wantAbort)
			}
		})
	}
}

func TestPublishNativeSegmentNeverClaimsExistingObject(t *testing.T) {
	s := newSegmentFaultStore()
	if _, err := PublishNativeSegment(context.Background(), s, validSegmentRequest()); err != nil {
		t.Fatal(err)
	}
	// The second attempt has byte-identical content but did not publish it.
	if _, err := PublishNativeSegment(context.Background(), s, validSegmentRequest()); !errors.Is(err, storage.ErrExists) {
		t.Fatalf("identical existing object was claimed: %v", err)
	}
}

func TestPublishNativeSegmentCancelsBlockedWriteAndAborts(t *testing.T) {
	s := newSegmentFaultStore()
	s.blockWrite = true
	s.writeEntered = make(chan struct{})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	type result struct {
		segment NativeSegment
		err     error
	}
	done := make(chan result, 1)
	go func() {
		segment, err := PublishNativeSegment(ctx, s, validSegmentRequest())
		done <- result{segment, err}
	}()
	select {
	case <-s.writeEntered:
	case <-time.After(time.Second):
		t.Fatal("publisher did not call WriteContext")
	}
	cancel()
	select {
	case got := <-done:
		if !errors.Is(got.err, context.Canceled) || got.segment != (NativeSegment{}) {
			t.Fatalf("canceled write returned %+v, %v", got.segment, got.err)
		}
	case <-time.After(time.Second):
		t.Fatal("publisher did not stop after cancellation")
	}
	if s.aborts != 1 {
		t.Fatalf("abort count %d, want 1", s.aborts)
	}
	if _, err := s.Open(context.Background(), testSegmentPath); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("canceled write published a segment: %v", err)
	}
}
