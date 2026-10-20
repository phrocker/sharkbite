// Licensed to the Apache Software Foundation (ASF) under one
// or more contributor license agreements. See the NOTICE file
// distributed with this work for additional information
// regarding copyright ownership. The ASF licenses this file
// to you under the Apache License, Version 2.0 (the
// "License"); you may obtain a copy of the License at
//
//   https://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing,
// software distributed under the License is distributed on an
// "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY
// KIND, either express or implied. See the License for the
// specific language governing permissions and limitations
// under the License.

package hdfs

import (
	"bytes"
	"context"
	"errors"
	"io"
	"testing"

	"github.com/phrocker/shoal-oss/internal/qwal"
	"github.com/phrocker/shoal-oss/internal/storage"
)

const nativeTestPath = "hdfs://nn:8020/accumulo/wal/server+9997/11111111-1111-4111-8111-111111111111"
const nativeResolvedPath = "/accumulo/wal/server+9997/11111111-1111-4111-8111-111111111111"

type nativeTestClient struct{ *fakeClient }

func (c *nativeTestClient) CreateNativeSegmentExclusive(name string) (storage.Writer, error) {
	return c.fakeClient.Create(name)
}

func TestNativePublisherUsesDirectHDFSAdapter(t *testing.T) {
	client := &nativeTestClient{fakeClient: newFakeClient()}
	backend, err := New("nn:8020", WithClient(client))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = backend.Close() })
	extent := qwal.KeyExtent{TableID: "5", PrevEndRow: []byte("a"), EndRow: []byte("z")}
	segment, err := qwal.PublishNativeSegment(context.Background(), backend, qwal.NativeSegmentRequest{
		Volume: "hdfs://nn:8020/accumulo", ServerAddress: "server:9997",
		ID: "11111111-1111-4111-8111-111111111111", Extent: extent,
		Entries: []qwal.Entry{
			{Key: qwal.LogFileKey{Event: qwal.EventOpen, TServerSession: "session"}},
			{Key: qwal.LogFileKey{Event: qwal.EventDefineTablet, TabletID: 7, Seq: 1, Tablet: &extent}},
			{Key: qwal.LogFileKey{Event: qwal.EventMutation, TabletID: 7, Seq: 2}, Value: qwal.LogFileValue{Mutations: []qwal.ServerMutation{{
				Row: []byte("m"), Updates: []qwal.ColumnUpdate{{ColumnFamily: []byte("cf"), Value: []byte("v")}},
			}}}},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if segment.Path != nativeTestPath || segment.Size != int64(len(client.files[nativeResolvedPath])) {
		t.Fatalf("segment %+v does not describe HDFS object", segment)
	}
	if len(client.renameCalls) != 0 || len(client.removeCalls) != 0 {
		t.Fatal("native publication used rename or remove")
	}
}

func TestNativeSegmentDirectExclusiveAndReadback(t *testing.T) {
	client := &nativeTestClient{fakeClient: newFakeClient()}
	backend, err := New("nn:8020", WithClient(client))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = backend.Close() })
	w, err := backend.CreateNativeSegmentExclusive(context.Background(), nativeTestPath)
	if err != nil {
		t.Fatal(err)
	}
	if client.lastCreatePath != nativeResolvedPath {
		t.Fatalf("created %q instead of final path", client.lastCreatePath)
	}
	want := []byte("complete native WAL bytes")
	if _, err := w.Write(want); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	f, err := backend.Open(context.Background(), nativeTestPath)
	if err != nil {
		t.Fatal(err)
	}
	got := make([]byte, f.Size())
	if _, err := f.ReadAt(got, 0); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("readback %q, want %q", got, want)
	}
	if _, err := backend.CreateNativeSegmentExclusive(context.Background(), nativeTestPath); !errors.Is(err, storage.ErrExists) {
		t.Fatalf("second create = %v, want ErrExists", err)
	}
	if !bytes.Equal(client.files[nativeResolvedPath], want) {
		t.Fatal("exclusive collision changed the destination")
	}
	if len(client.removeCalls) != 0 || len(client.renameCalls) != 0 {
		t.Fatalf("direct publication removed or renamed paths: removes=%v renames=%v", client.removeCalls, client.renameCalls)
	}
}

func TestNativeSegmentRejectsClientWithoutDirectExclusiveCreate(t *testing.T) {
	client := newFakeClient()
	backend, err := New("nn:8020", WithClient(client))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = backend.Close() })
	if _, err := backend.CreateNativeSegmentExclusive(context.Background(), nativeTestPath); !errors.Is(err, errors.ErrUnsupported) {
		t.Fatalf("unmarked client = %v, want unsupported", err)
	}
	if len(client.files) != 0 {
		t.Fatal("unsupported client created a destination")
	}
}

func TestNativeSegmentCloseFailureLeavesUnreferencedDestination(t *testing.T) {
	client := &nativeTestClient{fakeClient: newFakeClient()}
	client.failWriterClose = true
	backend, err := New("nn:8020", WithClient(client))
	if err != nil {
		t.Fatal(err)
	}
	w, err := backend.CreateNativeSegmentExclusive(context.Background(), nativeTestPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write([]byte("partial")); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err == nil {
		t.Fatal("failed close reported success")
	}
	if err := w.(storage.Aborter).Abort(); err != nil {
		t.Fatalf("abort after failed close: %v", err)
	}
	if err := backend.Close(); err != nil {
		t.Fatalf("backend retained failed writer: %v", err)
	}
	if _, exists := client.files[nativeResolvedPath]; !exists {
		t.Fatal("ambiguous destination was deleted")
	}
	if len(client.removeCalls) != 0 {
		t.Fatalf("removed ambiguous destination: %v", client.removeCalls)
	}
}

func TestNativeSegmentAbortLeavesOnlyAttemptOwnedOrphan(t *testing.T) {
	client := &nativeTestClient{fakeClient: newFakeClient()}
	backend, err := New("nn:8020", WithClient(client))
	if err != nil {
		t.Fatal(err)
	}
	w, err := backend.CreateNativeSegmentExclusive(context.Background(), nativeTestPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.WriteString(w, "incomplete"); err != nil {
		t.Fatal(err)
	}
	if err := w.(storage.Aborter).Abort(); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); !errors.Is(err, ErrClosed) {
		t.Fatalf("Close after Abort = %v, want ErrClosed", err)
	}
	if err := backend.Close(); err != nil {
		t.Fatal(err)
	}
	if len(client.removeCalls) != 0 {
		t.Fatalf("abort removed a final path: %v", client.removeCalls)
	}
	if !bytes.Equal(client.files[nativeResolvedPath], []byte("incomplete")) {
		t.Fatalf("orphan = %q", client.files[nativeResolvedPath])
	}
}
