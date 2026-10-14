package memory

import (
	"context"
	"errors"
	"testing"

	"github.com/phrocker/shoal-oss/internal/storage"
)

func TestMemory_CreateThenOpen(t *testing.T) {
	b := New()
	w, err := b.Create(context.Background(), "k")
	if err != nil {
		t.Fatal(err)
	}

	// Before Close, Open returns ErrNotFound — bytes haven't been published.
	_, err = b.Open(context.Background(), "k")
	if !errors.Is(err, storage.ErrNotFound) {
		t.Errorf("pre-Close Open: err = %v, want ErrNotFound", err)
	}

	if _, err := w.Write([]byte("hello")); err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write([]byte(", world")); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}

	f, err := b.Open(context.Background(), "k")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	got := make([]byte, f.Size())
	_, _ = f.ReadAt(got, 0)
	if string(got) != "hello, world" {
		t.Errorf("got %q", got)
	}
}

func TestMemory_WriteAfterCloseErrors(t *testing.T) {
	b := New()
	w, _ := b.Create(context.Background(), "k")
	_ = w.Close()
	if _, err := w.Write([]byte("late")); err == nil {
		t.Errorf("Write after Close should fail")
	}
}

func TestMemory_DoubleCloseOk(t *testing.T) {
	b := New()
	w, _ := b.Create(context.Background(), "k")
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Errorf("second Close: %v", err)
	}
}

func TestMemory_CreateReplacesExisting(t *testing.T) {
	b := New()
	b.Put("k", []byte("old contents"))
	w, _ := b.Create(context.Background(), "k")
	_, _ = w.Write([]byte("new"))
	_ = w.Close()

	f, _ := b.Open(context.Background(), "k")
	defer f.Close()
	if f.Size() != 3 {
		t.Errorf("size = %d, want 3", f.Size())
	}
}

func TestMemory_AbortDiscardsBufferedWrite(t *testing.T) {
	b := New()
	b.Put("k", []byte("old"))

	w, err := b.Create(context.Background(), "k")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write([]byte("new")); err != nil {
		t.Fatal(err)
	}
	aborter, ok := w.(storage.Aborter)
	if !ok {
		t.Fatal("writer does not implement storage.Aborter")
	}
	if err := aborter.Abort(); err != nil {
		t.Fatal(err)
	}

	f, err := b.Open(context.Background(), "k")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	got := make([]byte, f.Size())
	if _, err := f.ReadAt(got, 0); err != nil {
		t.Fatal(err)
	}
	if string(got) != "old" {
		t.Fatalf("got %q, want old contents preserved", got)
	}
}
