//go:build integration

package hdfs_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path"
	"testing"

	"github.com/google/uuid"
	"github.com/phrocker/shoal-oss/internal/rfile"
	"github.com/phrocker/shoal-oss/internal/rfile/bcfile"
	"github.com/phrocker/shoal-oss/internal/rfile/bcfile/block"
	"github.com/phrocker/shoal-oss/internal/storage"
	hdfsstorage "github.com/phrocker/shoal-oss/internal/storage/hdfs"
)

func TestLiveBackendAndRFileRoundTrip(t *testing.T) {
	if os.Getenv("SHOAL_HDFS_INTEGRATION") == "" {
		t.Skip("set SHOAL_HDFS_INTEGRATION=1 to run live HDFS tests")
	}

	backend, err := hdfsstorage.New("127.0.0.1:8020")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() {
		if err := backend.Close(); err != nil {
			t.Errorf("Close backend: %v", err)
		}
	})

	root := "/tmp/shoal-integration/" + uuid.NewString()
	rawPath := path.Join(root, "raw.bin")
	rfilePath := path.Join(root, "roundtrip.rf")

	testRawBackend(t, backend, rawPath)
	testRFileRoundTrip(t, backend, rfilePath)
}

type liveBackend interface {
	storage.WritableBackend
	storage.Lister
	storage.Remover
}

func testRawBackend(t *testing.T, backend liveBackend, filePath string) {
	t.Helper()

	want := []byte("shoal live hdfs roundtrip")
	writer, err := backend.Create(context.Background(), filePath)
	if err != nil {
		t.Fatalf("Create raw file: %v", err)
	}
	if _, err := writer.Write(want); err != nil {
		t.Fatalf("Write raw file: %v", err)
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("Close raw file: %v", err)
	}

	reader, err := backend.Open(context.Background(), filePath)
	if err != nil {
		t.Fatalf("Open raw file: %v", err)
	}
	got := make([]byte, len(want))
	if _, err := reader.ReadAt(got, 0); err != nil {
		t.Fatalf("ReadAt raw file: %v", err)
	}
	if err := reader.Close(); err != nil {
		t.Fatalf("Close raw reader: %v", err)
	}
	if err := reader.Close(); err != nil {
		t.Fatalf("Close raw reader again: %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("raw file = %q, want %q", got, want)
	}

	entries, err := backend.List(context.Background(), path.Dir(filePath))
	if err != nil {
		t.Fatalf("List raw directory: %v", err)
	}
	if !contains(entries, filePath) {
		t.Fatalf("List(%q) = %v, missing %q", path.Dir(filePath), entries, filePath)
	}

	if err := backend.Remove(context.Background(), filePath); err != nil {
		t.Fatalf("Remove raw file: %v", err)
	}
	if err := backend.Remove(context.Background(), filePath); err != nil {
		t.Fatalf("Remove raw file again: %v", err)
	}
}

func testRFileRoundTrip(t *testing.T, backend liveBackend, filePath string) {
	t.Helper()

	writer, err := backend.Create(context.Background(), filePath)
	if err != nil {
		t.Fatalf("Create RFile: %v", err)
	}
	rfileWriter, err := rfile.NewWriter(writer, rfile.WriterOptions{})
	if err != nil {
		t.Fatalf("NewWriter: %v", err)
	}
	want := []struct {
		key   *rfile.Key
		value []byte
	}{
		{
			key: &rfile.Key{
				Row:             []byte("row-a"),
				ColumnFamily:    []byte("cf"),
				ColumnQualifier: []byte("cq"),
				Timestamp:       2,
			},
			value: []byte("value-a"),
		},
		{
			key: &rfile.Key{
				Row:             []byte("row-b"),
				ColumnFamily:    []byte("cf"),
				ColumnQualifier: []byte("cq"),
				Timestamp:       1,
			},
			value: []byte("value-b"),
		},
	}
	for _, cell := range want {
		if err := rfileWriter.Append(cell.key, cell.value); err != nil {
			t.Fatalf("Append: %v", err)
		}
	}
	if err := rfileWriter.Close(); err != nil {
		t.Fatalf("Close RFile writer: %v", err)
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("Close HDFS writer: %v", err)
	}

	file, err := backend.Open(context.Background(), filePath)
	if err != nil {
		t.Fatalf("Open RFile: %v", err)
	}
	container, err := bcfile.NewReader(file, file.Size())
	if err != nil {
		t.Fatalf("NewReader: %v", err)
	}
	reader, err := rfile.Open(container, block.Default())
	if err != nil {
		t.Fatalf("Open RFile reader: %v", err)
	}
	if err := reader.Seek(nil); err != nil {
		t.Fatalf("Seek start: %v", err)
	}
	for i, cell := range want {
		key, value, err := reader.Next()
		if err != nil {
			t.Fatalf("Next cell %d: %v", i, err)
		}
		if !key.Equal(cell.key) || !bytes.Equal(value, cell.value) {
			t.Fatalf("cell %d = (%+v, %q), want (%+v, %q)", i, key, value, cell.key, cell.value)
		}
	}
	if _, _, err := reader.Next(); !errors.Is(err, io.EOF) {
		t.Fatalf("Next after final cell = %v, want io.EOF", err)
	}

	if err := file.Close(); err != nil {
		t.Fatalf("Close RFile: %v", err)
	}
	if err := backend.Remove(context.Background(), filePath); err != nil {
		t.Fatalf("Remove RFile: %v", err)
	}
}

func contains(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}
