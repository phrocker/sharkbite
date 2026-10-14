package memory

import (
	"bytes"
	"context"
	"errors"
	"io"
	"sync"
	"testing"

	"github.com/phrocker/shoal-oss/internal/storage"
)

func TestMemory_PutOpenReadAt(t *testing.T) {
	b := New()
	body := []byte("the quick brown fox")
	b.Put("path/to/obj", body)

	f, err := b.Open(context.Background(), "path/to/obj")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	if got := f.Size(); got != int64(len(body)) {
		t.Errorf("Size = %d, want %d", got, len(body))
	}
	buf := make([]byte, 5)
	if _, err := f.ReadAt(buf, 4); err != nil {
		t.Fatal(err)
	}
	if string(buf) != "quick" {
		t.Errorf("got %q, want \"quick\"", buf)
	}
}

func TestMemory_NotFound(t *testing.T) {
	_, err := New().Open(context.Background(), "no-such-path")
	if !errors.Is(err, storage.ErrNotFound) {
		t.Errorf("err = %v, want chain to storage.ErrNotFound", err)
	}
}

func TestMemory_Delete(t *testing.T) {
	b := New()
	b.Put("a", []byte("alpha"))
	b.Delete("a")
	_, err := b.Open(context.Background(), "a")
	if !errors.Is(err, storage.ErrNotFound) {
		t.Errorf("after Delete, err = %v, want ErrNotFound", err)
	}
}

func TestMemory_PutCopiesInput(t *testing.T) {
	b := New()
	src := []byte("original")
	b.Put("k", src)
	// Mutate src after Put — must not affect the stored fixture.
	src[0] = 'X'

	f, _ := b.Open(context.Background(), "k")
	defer f.Close()
	got := make([]byte, 8)
	_, _ = f.ReadAt(got, 0)
	if !bytes.Equal(got, []byte("original")) {
		t.Errorf("Put didn't copy: got %q after src mutation", got)
	}
}

func TestMemory_ReadAtEOF(t *testing.T) {
	b := New()
	b.Put("k", []byte("abc"))
	f, _ := b.Open(context.Background(), "k")
	defer f.Close()
	buf := make([]byte, 10)
	n, err := f.ReadAt(buf, 0)
	if !errors.Is(err, io.EOF) {
		t.Errorf("err = %v, want io.EOF", err)
	}
	if n != 3 {
		t.Errorf("n = %d, want 3", n)
	}
}

func TestMemory_ConcurrentPutOpen(t *testing.T) {
	b := New()
	var wg sync.WaitGroup
	for i := 0; i < 100; i++ {
		wg.Add(2)
		go func(i int) {
			defer wg.Done()
			b.Put("k", []byte{byte(i)})
		}(i)
		go func() {
			defer wg.Done()
			_, _ = b.Open(context.Background(), "k")
		}()
	}
	wg.Wait()
}
