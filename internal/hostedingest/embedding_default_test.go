// Licensed to the Apache Software Foundation (ASF) under one
// or more contributor license agreements. See the NOTICE file distributed
// with this work for additional information regarding copyright ownership.
// The ASF licenses this file to you under the Apache License, Version 2.0.
package hostedingest

import (
	"context"
	"testing"

	"github.com/phrocker/shoal-oss/internal/embeddingspace"
	"github.com/phrocker/shoal-oss/internal/storage/memory"
	"github.com/phrocker/shoal-oss/internal/tabletloader"
	"github.com/phrocker/shoal-oss/internal/tserver"
	"github.com/phrocker/shoal-oss/internal/walauthority"
)

func embeddingFactory(
	t *testing.T, host *tserver.Host, md *fakeMetadata, defaultEmbedding embeddingspace.FileState,
) *Factory {
	t.Helper()
	root := t.TempDir()
	factory, err := NewFactory(Config{
		Host: host, ServerAddress: "127.0.0.1:9997",
		WALRoot: root + "\\wal", MincRoot: "rfiles", StateRoot: root + "\\state",
		WALStore: walauthority.NewLocalStore(), Outputs: memory.New(),
		Metadata: fakeMetadataFactory{metadata: md}, FlushCells: 1,
		DefaultEmbedding: defaultEmbedding,
	})
	if err != nil {
		t.Fatal(err)
	}
	return factory
}

func commitOneCell(t *testing.T, factory *Factory, host *tserver.Host, attempt tserver.Attempt) {
	t.Helper()
	opened, err := factory.Open(context.Background(), tabletloader.Specification{
		Extent: tserver.Extent{TableID: "1", EndRow: []byte("z")},
	}, attempt)
	if err != nil {
		t.Fatal(err)
	}
	if err := host.LoadComplete(attempt); err != nil {
		t.Fatal(err)
	}
	tablet := opened.(*Tablet)
	request := testRequest(attempt)
	request.Fence = tablet.Fence()
	if err := tablet.Commit(context.Background(), request); err != nil {
		t.Fatal(err)
	}
}

// TestDefaultEmbeddingReachesTheMinorCompaction is the wiring half of
// issue #274's escape hatch. mincauthority.Config.DefaultEmbedding is
// only useful if an operator can actually set it, and the hosted ingest
// factory is the sole production constructor of that coordinator.
func TestDefaultEmbeddingReachesTheMinorCompaction(t *testing.T) {
	t.Run("unset means unknown", func(t *testing.T) {
		host, attempt := hostAssignment(t)
		md := &fakeMetadata{}
		commitOneCell(t, embeddingFactory(t, host, md, embeddingspace.FileState{}), host, attempt)
		md.mu.Lock()
		defer md.mu.Unlock()
		if len(md.files) != 1 {
			t.Fatalf("files = %#v", md.files)
		}
		if md.files[0].Embedding != embeddingspace.Unknown() {
			t.Fatalf("embedding = %+v, want unknown", md.files[0].Embedding)
		}
	})

	t.Run("configured default is recorded", func(t *testing.T) {
		host, attempt := hostAssignment(t)
		md := &fakeMetadata{}
		commitOneCell(t, embeddingFactory(t, host, md, embeddingspace.NoEmbeddings()), host, attempt)
		md.mu.Lock()
		defer md.mu.Unlock()
		if len(md.files) != 1 {
			t.Fatalf("files = %#v", md.files)
		}
		if md.files[0].Embedding != embeddingspace.NoEmbeddings() {
			t.Fatalf("embedding = %+v, want no embeddings", md.files[0].Embedding)
		}
	})
}

// TestNewFactoryRejectsAnInvalidDefaultEmbedding: the value becomes
// durable evidence in every flushed file, so it must be refused at
// startup rather than at the first flush.
func TestNewFactoryRejectsAnInvalidDefaultEmbedding(t *testing.T) {
	host, _ := hostAssignment(t)
	root := t.TempDir()
	_, err := NewFactory(Config{
		Host: host, ServerAddress: "127.0.0.1:9997",
		WALRoot: root + "\\wal", MincRoot: "rfiles", StateRoot: root + "\\state",
		WALStore: walauthority.NewLocalStore(), Outputs: memory.New(),
		Metadata:         fakeMetadataFactory{metadata: &fakeMetadata{}},
		DefaultEmbedding: embeddingspace.FileState{State: "definitely-not-a-state"},
	})
	if err == nil {
		t.Fatal("an invalid default embedding was accepted")
	}
}

// TestNewFactoryRejectsAPartialDefaultEmbedding: an identity with no
// state fails FileState.Validate, so accepting it at startup would mean
// silently ignoring configuration the operator believes is in effect.
func TestNewFactoryRejectsAPartialDefaultEmbedding(t *testing.T) {
	host, _ := hostAssignment(t)
	root := t.TempDir()
	_, err := NewFactory(Config{
		Host: host, ServerAddress: "127.0.0.1:9997",
		WALRoot: root + "\\wal", MincRoot: "rfiles", StateRoot: root + "\\state",
		WALStore: walauthority.NewLocalStore(), Outputs: memory.New(),
		Metadata:         fakeMetadataFactory{metadata: &fakeMetadata{}},
		DefaultEmbedding: embeddingspace.FileState{Identity: "space-a"},
	})
	if err == nil {
		t.Fatal("a partial default embedding was accepted")
	}
}
