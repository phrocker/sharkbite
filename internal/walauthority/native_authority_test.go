// Licensed to the Apache Software Foundation (ASF) under one
// or more contributor license agreements. See the NOTICE file
// distributed with this work for additional information
// regarding copyright ownership. The ASF licenses this file
// to you under the Apache License, Version 2.0 (the
// "License"); you may not use this file except in compliance
// with the License. You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package walauthority

import (
	"context"
	"testing"
)

type memoryNativeStore struct{ *memoryStore }

func (s memoryNativeStore) Publish(ctx context.Context, path string, data []byte) error {
	if s.events != nil {
		*s.events = append(*s.events, "publish")
	}
	return s.Create(ctx, path, data)
}

func TestNativeCommitPublishesBeforeReferenceAndRecovers(t *testing.T) {
	var events []string
	store := memoryNativeStore{newMemoryStore(&events)}
	metadata := newMemoryMetadata(&events)
	sink := newSink(&events)
	cfg := testConfig(store.memoryStore, metadata, sink, &verifier{})
	cfg.Root = "hdfs://namenode:8020/accumulo/wal"
	cfg.NativeStore = store
	tablet, _, err := Open(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := tablet.Commit(context.Background(), testRequest("native-op", 7)); err != nil {
		t.Fatal(err)
	}
	if len(events) < 3 || events[0] != "publish" || events[1] != "reference" || events[2] != "apply" {
		t.Fatalf("unsafe native commit order: %v", events)
	}
	refs, err := metadata.References(context.Background(), cfg.Extent)
	if err != nil || len(refs) != 1 {
		t.Fatalf("native references = %v, %v", refs, err)
	}
	if refs[0].Path != "hdfs://namenode:8020/accumulo/wal/host.example+9997/11111111-1111-4111-8111-111111111111" {
		t.Fatalf("native WAL path = %q", refs[0].Path)
	}
	replayed := newSink(nil)
	cfg.Sink = replayed
	cfg.Fence.Assignment++ // a different owner must recover the shared WAL
	_, report, err := Open(context.Background(), cfg)
	if err != nil || report.Applied != 1 || replayed.applied["native-op"] != 1 {
		t.Fatalf("native recovery = %+v, %v, applied=%v", report, err, replayed.applied)
	}
	split := cfg
	split.Extent.EndRow = []byte("m")
	if _, _, err := Open(context.Background(), split); err == nil {
		t.Fatal("split child accepted a parent WAL without an extent-safe replay plan")
	}
	boundary, err := tablet.SealForMinorCompaction(context.Background(), func(MinorCompactionBoundary) error { return nil })
	if err != nil || boundary.Sequence != 1 || len(boundary.References) != 1 {
		t.Fatalf("native boundary = %+v, %v", boundary, err)
	}
	if err := tablet.Retire(context.Background(), refs[0]); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Read(context.Background(), refs[0].Path); err == nil {
		t.Fatal("retired native WAL remains")
	}
}

func TestNativePublicationFailureCannotInstallReference(t *testing.T) {
	store := memoryNativeStore{newMemoryStore(nil)}
	metadata := newMemoryMetadata(nil)
	cfg := testConfig(store.memoryStore, metadata, newSink(nil), &verifier{})
	cfg.Root = "hdfs://namenode:8020/accumulo/wal"
	cfg.NativeStore = store
	path := "hdfs://namenode:8020/accumulo/wal/host.example+9997/11111111-1111-4111-8111-111111111111"
	if err := store.Publish(context.Background(), path, []byte("occupied")); err != nil {
		t.Fatal(err)
	}
	tablet, _, err := Open(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := tablet.Commit(context.Background(), testRequest("native-op", 7)); err == nil {
		t.Fatal("duplicate native path was acknowledged")
	}
	refs, err := metadata.References(context.Background(), cfg.Extent)
	if err != nil || len(refs) != 0 {
		t.Fatalf("failed publish installed references %v, %v", refs, err)
	}
}
