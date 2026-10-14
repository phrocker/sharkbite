package mincauthority

import (
	"context"
	"errors"
	"testing"

	"github.com/phrocker/shoal-oss/internal/storage/memory"
)

func TestBackendOutputStorePublishesIdempotentlyAndRejectsCollision(t *testing.T) {
	backend := memory.New()
	store := BackendOutputStore{Backend: backend}
	if err := store.Publish(context.Background(), "5/a.rf", []byte("one")); err != nil {
		t.Fatal(err)
	}
	if err := store.Publish(context.Background(), "5/a.rf", []byte("one")); err != nil {
		t.Fatal(err)
	}
	if err := store.Publish(context.Background(), "5/a.rf", []byte("two")); !errors.Is(err, ErrCorruptOutput) {
		t.Fatalf("got %v, want corrupt output", err)
	}
	data, err := store.Read(context.Background(), "5/a.rf")
	if err != nil || string(data) != "one" {
		t.Fatalf("data=%q err=%v", data, err)
	}
}
