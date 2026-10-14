package local

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/phrocker/shoal-oss/internal/storage"
)

func writeFixture(t *testing.T, dir string, name string, body []byte) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, body, 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestLocal_OpenReadAtClose(t *testing.T) {
	dir := t.TempDir()
	body := []byte("hello, local backend")
	path := writeFixture(t, dir, "fixture.bin", body)

	f, err := New().Open(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	if got := f.Size(); got != int64(len(body)) {
		t.Errorf("Size = %d, want %d", got, len(body))
	}

	buf := make([]byte, 5)
	n, err := f.ReadAt(buf, 7)
	if err != nil {
		t.Fatal(err)
	}
	if n != 5 || string(buf) != "local" {
		t.Errorf("ReadAt(5,7) = %q (n=%d), want \"local\"", buf, n)
	}
}

func TestLocal_NotFoundIsSentinel(t *testing.T) {
	_, err := New().Open(context.Background(), "/definitely/does/not/exist/anywhere")
	if !errors.Is(err, storage.ErrNotFound) {
		t.Errorf("err = %v, want chain to storage.ErrNotFound", err)
	}
}

func TestLocal_ReadAtPastEnd(t *testing.T) {
	path := writeFixture(t, t.TempDir(), "x.bin", []byte("abc"))
	f, err := New().Open(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	// ReadAt past the end of the file: io.ReaderAt contract says we get
	// io.EOF (possibly with a partial read).
	buf := make([]byte, 10)
	n, err := f.ReadAt(buf, 0)
	if !errors.Is(err, io.EOF) {
		t.Errorf("err = %v, want io.EOF", err)
	}
	if n != 3 {
		t.Errorf("n = %d, want 3", n)
	}
}

func TestLocal_ReadAt_FullRoundtrip(t *testing.T) {
	body := bytes.Repeat([]byte{0xab, 0xcd, 0xef, 0x01}, 1024) // 4KB
	path := writeFixture(t, t.TempDir(), "blob.bin", body)
	f, err := New().Open(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	got := make([]byte, len(body))
	n, err := f.ReadAt(got, 0)
	if err != nil {
		t.Fatal(err)
	}
	if n != len(body) || !bytes.Equal(got, body) {
		t.Errorf("roundtrip mismatch: n=%d", n)
	}
}

func TestLocal_ConcurrentReadAt(t *testing.T) {
	body := bytes.Repeat([]byte{0x42}, 4096)
	path := writeFixture(t, t.TempDir(), "concurrent.bin", body)
	f, err := New().Open(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	const N = 32
	done := make(chan error, N)
	for i := 0; i < N; i++ {
		go func() {
			buf := make([]byte, 64)
			_, err := f.ReadAt(buf, 100)
			done <- err
		}()
	}
	for i := 0; i < N; i++ {
		if err := <-done; err != nil {
			t.Errorf("goroutine %d: %v", i, err)
		}
	}
}
