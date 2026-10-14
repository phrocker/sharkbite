package rfile

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/phrocker/shoal-oss/accumulo"
)

// TestWriterCloseRepeatsItsFailure pins that a failed finalization is never
// reported as success by a later Close: the index and trailer are written by
// Close, so a caller that saw nil from the second call would treat a truncated
// file as complete.
func TestWriterCloseRepeatsItsFailure(t *testing.T) {
	path := filepath.Join(t.TempDir(), "closefail.rf")
	writer, err := Create(context.Background(), path, WriterOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if err := writer.Append(context.Background(), Entry{
		Key:   accumulo.Key{Row: []byte("row1"), ColumnFamily: []byte("cf"), Timestamp: 10},
		Value: []byte("v1"),
	}); err != nil {
		t.Fatal(err)
	}
	// Closing the handle underneath the writer makes the trailer write fail,
	// which is what a revoked handle or a full disk looks like.
	if err := writer.file.Close(); err != nil {
		t.Fatal(err)
	}

	first := writer.Close()
	if first == nil {
		t.Fatal("Close over a released handle returned nil")
	}
	second := writer.Close()
	if second == nil {
		t.Fatal("the second Close reported success after the first failed")
	}
	if first.Error() != second.Error() {
		t.Fatalf("Close reported %q then %q; the failure must be remembered", first, second)
	}
}

// TestFailedAppendIsTerminal pins that an append which fails after the entry
// reached the block stream poisons the writer: further appends report the same
// error, and Close reports it too, so a partially written file is never
// committed as complete.
func TestFailedAppendIsTerminal(t *testing.T) {
	path := filepath.Join(t.TempDir(), "appendfail.rf")
	// BlockSize 1 flushes on every append, so the failure lands inside the
	// inner writer rather than in a buffer the caller could still discard.
	writer, err := Create(context.Background(), path, WriterOptions{BlockSize: 1})
	if err != nil {
		t.Fatal(err)
	}
	cell := func(row, value string) Entry {
		return Entry{
			Key:   accumulo.Key{Row: []byte(row), ColumnFamily: []byte("cf"), Timestamp: 10},
			Value: []byte(value),
		}
	}
	if err := writer.Append(context.Background(), cell("row1", "v1")); err != nil {
		t.Fatal(err)
	}
	if err := writer.file.Close(); err != nil {
		t.Fatal(err)
	}

	failed := writer.Append(context.Background(), cell("row2", "v2"))
	if failed == nil {
		t.Fatal("append over a released handle returned nil")
	}
	if got := writer.Append(context.Background(), cell("row3", "v3")); got == nil || got.Error() != failed.Error() {
		t.Fatalf("later append = %v, want the latched %v", got, failed)
	}
	if got := writer.Entries(); got != 1 {
		t.Fatalf("Entries = %d, want only the append that succeeded", got)
	}
	closeErr := writer.Close()
	if closeErr == nil {
		t.Fatal("Close reported success after an append had failed")
	}
	if !strings.Contains(closeErr.Error(), "append to") {
		t.Fatalf("Close = %v, want it to carry the append failure", closeErr)
	}
	if second := writer.Close(); second == nil || second.Error() != closeErr.Error() {
		t.Fatalf("the second Close = %v, want the remembered %v", second, closeErr)
	}
}

// TestReaderCloseRepeatsItsFailure pins the same contract on the read side.
func TestReaderCloseRepeatsItsFailure(t *testing.T) {
	boom := errors.New("release failed")
	calls := 0
	reader := &Reader{closers: []func() error{func() error {
		calls++
		return boom
	}}}

	first := reader.Close()
	if !errors.Is(first, boom) {
		t.Fatalf("Close = %v, want the closer's error", first)
	}
	second := reader.Close()
	if !errors.Is(second, boom) {
		t.Fatalf("the second Close = %v, want the remembered error", second)
	}
	if calls != 1 {
		t.Fatalf("the closer ran %d times, want exactly 1", calls)
	}
}
