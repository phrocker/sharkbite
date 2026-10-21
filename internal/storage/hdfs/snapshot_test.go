package hdfs

import (
	"context"
	"errors"
	"testing"

	"github.com/phrocker/shoal-oss/internal/storage"
)

func TestSnapshotRefusesCustomClientWithoutPinnedBlocks(t *testing.T) {
	client := newFakeClient()
	client.files["/tmp/ordinary.rf"] = []byte("ordinary")
	backend, err := New("nn:8020", WithClient(client))
	if err != nil {
		t.Fatal(err)
	}
	defer backend.Close()
	if _, err := backend.OpenSnapshot(context.Background(), "/tmp/ordinary.rf"); !errors.Is(err, storage.ErrSnapshotUnsupported) {
		t.Fatalf("custom client claimed pinned snapshot: %v", err)
	}
	file, err := backend.Open(context.Background(), "/tmp/ordinary.rf")
	if err != nil {
		t.Fatalf("ordinary Open regressed: %v", err)
	}
	_ = file.Close()
}
