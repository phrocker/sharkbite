package embeddingsidecar

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"testing"

	"github.com/phrocker/shoal-oss/internal/embeddingspace"
	"github.com/phrocker/shoal-oss/internal/rfile"
	"github.com/phrocker/shoal-oss/internal/storage"
	"github.com/phrocker/shoal-oss/internal/storage/memory"
)

func evidenceRFile(t *testing.T, state embeddingspace.FileState, value string) []byte {
	t.Helper()
	var output bytes.Buffer
	w, err := rfile.NewWriter(&output, rfile.WriterOptions{EmbeddingSpace: state})
	if err != nil {
		t.Fatal(err)
	}
	if err := w.Append(&rfile.Key{Row: []byte("r"), ColumnFamily: []byte("cf"), Timestamp: 1}, []byte(value)); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return output.Bytes()
}

func TestRFileEvidenceReaderBindsFooterAndWholeFileSeparately(t *testing.T) {
	ctx := context.Background()
	backend := memory.New()
	first := evidenceRFile(t, embeddingspace.Has("space-a"), "first")
	second := evidenceRFile(t, embeddingspace.Has("space-a"), "other")
	backend.Put("file.rf", first)
	reader := RFileEvidenceReader{Backend: backend}
	footerA, state, err := reader.ReadFooter(ctx, "file.rf")
	if err != nil || state != embeddingspace.Has("space-a") {
		t.Fatalf("footer A = %s, %v, %v", footerA, state, err)
	}
	fileA, sizeA, err := reader.ReadFileDigest(ctx, "file.rf")
	if err != nil || sizeA != int64(len(first)) {
		t.Fatalf("file A = %s, %d, %v", fileA, sizeA, err)
	}
	want := sha256.Sum256(first)
	if fileA != hex.EncodeToString(want[:]) {
		t.Fatalf("full-file digest = %s", fileA)
	}
	backend.Put("file.rf", second)
	footerB, state, err := reader.ReadFooter(ctx, "file.rf")
	if err != nil || state != embeddingspace.Has("space-a") || footerA != footerB {
		t.Fatalf("same footer bytes = %s, %v, %v", footerB, state, err)
	}
	fileB, _, err := reader.ReadFileDigest(ctx, "file.rf")
	if err != nil || fileA == fileB {
		t.Fatalf("replaced RFile inherited whole-file identity: %s, %v", fileB, err)
	}
	backend.Put("file.rf", evidenceRFile(t, embeddingspace.Unknown(), "first"))
	absentDigest, absentState, err := reader.ReadFooter(ctx, "file.rf")
	if err != nil || absentState != embeddingspace.Unknown() || absentDigest == footerA {
		t.Fatalf("missing footer = %s, %v, %v", absentDigest, absentState, err)
	}
	backend.Put("file.rf", []byte("not an RFile"))
	if _, _, err := reader.ReadFooter(ctx, "file.rf"); err == nil {
		t.Fatal("corrupt RFile footer accepted")
	}
}

type interruptingBackend struct {
	storage.Backend
	cancel context.CancelFunc
	short  bool
}
type interruptingFile struct {
	storage.File
	cancel context.CancelFunc
	short  bool
}

func (b interruptingBackend) Open(ctx context.Context, path string) (storage.File, error) {
	f, err := b.Backend.Open(ctx, path)
	if err != nil {
		return nil, err
	}
	return interruptingFile{File: f, cancel: b.cancel, short: b.short}, nil
}
func (f interruptingFile) ReadAt(buf []byte, offset int64) (int, error) {
	if f.short {
		return 0, nil
	}
	n, err := f.File.ReadAt(buf, offset)
	f.cancel()
	return n, err
}

func TestRFileEvidenceReaderRefusesCancelledOrShortWholeFileHash(t *testing.T) {
	backend := memory.New()
	backend.Put("file.rf", bytes.Repeat([]byte("x"), 2*fileDigestChunkBytes))
	ctx, cancel := context.WithCancel(context.Background())
	reader := RFileEvidenceReader{Backend: interruptingBackend{Backend: backend, cancel: cancel}}
	if _, _, err := reader.ReadFileDigest(ctx, "file.rf"); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled full-file hash = %v", err)
	}
	reader.Backend = interruptingBackend{Backend: backend, cancel: func() {}, short: true}
	if _, _, err := reader.ReadFileDigest(context.Background(), "file.rf"); !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("short full-file hash = %v", err)
	}
}

func TestRFileEvidenceReaderRefusesShortFooterReadWithNilError(t *testing.T) {
	backend := memory.New()
	backend.Put("file.rf", evidenceRFile(t, embeddingspace.Has("space-a"), "v"))
	reader := RFileEvidenceReader{Backend: interruptingBackend{
		Backend: backend, cancel: func() {}, short: true}}
	if _, _, err := reader.ReadFooter(context.Background(), "file.rf"); !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("short nil-error BCFile read = %v", err)
	}
}
