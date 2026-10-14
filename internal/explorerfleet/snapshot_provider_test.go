// Licensed to the Apache Software Foundation (ASF) under one or more
// contributor license agreements. See the NOTICE file distributed with this
// work for additional information regarding copyright ownership.
package explorerfleet

import (
	"context"
	"testing"

	"github.com/phrocker/shoal-oss/pkg/explorer"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

func TestInteractionSnapshotProviderUsesConcreteCorpus(t *testing.T) {
	corpus, err := explorer.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer corpus.Close()

	provider, err := NewInteractionSnapshotProvider(corpus)
	if err != nil {
		t.Fatal(err)
	}
	want, err := corpus.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	got, err := provider.InteractionSnapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != want.ID || !got.AsOf.Equal(want.AsOf) {
		t.Fatalf("snapshot = %#v, want %#v", got, want)
	}
}

func TestInteractionSnapshotProviderRejectsNil(t *testing.T) {
	if _, err := NewInteractionSnapshotProvider(nil); !shoal.IsErrorCode(
		err, shoal.ErrorInvalidArgument,
	) {
		t.Fatalf("nil corpus error = %v", err)
	}
	var provider *InteractionSnapshotProvider
	if _, err := provider.InteractionSnapshot(
		context.Background(),
	); !shoal.IsErrorCode(err, shoal.ErrorInvalidArgument) {
		t.Fatalf("nil provider error = %v", err)
	}
}
