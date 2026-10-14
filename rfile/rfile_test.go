package rfile_test

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/phrocker/shoal-oss/accumulo"
	"github.com/phrocker/shoal-oss/rfile"
)

func entry(row, family, qualifier string, timestamp int64, value string) rfile.Entry {
	return rfile.Entry{
		Key: accumulo.Key{
			Row:             []byte(row),
			ColumnFamily:    []byte(family),
			ColumnQualifier: []byte(qualifier),
			Timestamp:       timestamp,
		},
		Value: []byte(value),
	}
}

func tombstone(row, family, qualifier string, timestamp int64) rfile.Entry {
	e := entry(row, family, qualifier, timestamp, "")
	e.Deleted = true
	return e
}

// writeFile writes entries into a fresh RFile under its own temp dir.
func writeFile(t *testing.T, name string, entries ...rfile.Entry) string {
	t.Helper()
	return writeInDir(t, t.TempDir(), name, entries...)
}

func writeInDir(t *testing.T, dir, name string, entries ...rfile.Entry) string {
	t.Helper()
	path := filepath.Join(dir, name)
	writer, err := rfile.Create(context.Background(), path, rfile.WriterOptions{})
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if err := writer.Append(context.Background(), e); err != nil {
			t.Fatal(err)
		}
	}
	if got := writer.Entries(); got != int64(len(entries)) {
		t.Fatalf("Entries = %d, want %d", got, len(entries))
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	return path
}

func drain(t *testing.T, reader *rfile.Reader) []rfile.Entry {
	t.Helper()
	var out []rfile.Entry
	for reader.HasTop() {
		top, err := reader.Top()
		if err != nil {
			t.Fatal(err)
		}
		key, err := reader.TopKey()
		if err != nil {
			t.Fatal(err)
		}
		value, err := reader.TopValue()
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(key.Row, top.Key.Row) || !bytes.Equal(value, top.Value) {
			t.Fatalf("Top, TopKey and TopValue disagree: %+v / %+v / %q", top, key, value)
		}
		out = append(out, top)
		if err := reader.Next(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	return out
}

func rows(entries []rfile.Entry) []string {
	out := make([]string, 0, len(entries))
	for _, e := range entries {
		out = append(out, string(e.Key.Row))
	}
	return out
}

func requireRows(t *testing.T, got []rfile.Entry, want ...string) {
	t.Helper()
	gotRows := rows(got)
	if len(gotRows) != len(want) {
		t.Fatalf("rows = %v, want %v", gotRows, want)
	}
	for i := range want {
		if gotRows[i] != want[i] {
			t.Fatalf("rows = %v, want %v", gotRows, want)
		}
	}
}

func TestWriteThenReadSequentially(t *testing.T) {
	path := writeFile(t, "seq.rf",
		entry("row1", "cf", "cq", 10, "v1"),
		entry("row2", "cf", "cq", 10, "v2"),
		entry("row3", "cf", "cq", 10, "v3"),
	)

	reader, err := rfile.OpenSequential(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()

	requireRows(t, drain(t, reader), "row1", "row2", "row3")
	if reader.HasTop() {
		t.Fatal("HasTop is still true after the file was exhausted")
	}
	if _, err := reader.Top(); !errors.Is(err, rfile.ErrNoTop) {
		t.Fatalf("Top after exhaustion = %v, want ErrNoTop", err)
	}
	if err := reader.Next(context.Background()); !errors.Is(err, rfile.ErrNoTop) {
		t.Fatalf("Next after exhaustion = %v, want ErrNoTop", err)
	}
}

func TestOpenRequiresSeekBeforeReading(t *testing.T) {
	path := writeFile(t, "random.rf",
		entry("row1", "cf", "cq", 10, "v1"),
		entry("row2", "cf", "cq", 10, "v2"),
	)

	reader, err := rfile.Open(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()

	if reader.HasTop() {
		t.Fatal("HasTop is true before the first Seek")
	}
	if _, err := reader.TopKey(); !errors.Is(err, rfile.ErrNoTop) {
		t.Fatalf("TopKey before Seek = %v, want ErrNoTop", err)
	}
	if _, err := reader.TopValue(); !errors.Is(err, rfile.ErrNoTop) {
		t.Fatalf("TopValue before Seek = %v, want ErrNoTop", err)
	}

	seekable, err := rfile.NewSeekable(mustRange(t, "row2", true, nil, true))
	if err != nil {
		t.Fatal(err)
	}
	if err := reader.Seek(context.Background(), seekable); err != nil {
		t.Fatal(err)
	}
	requireRows(t, drain(t, reader), "row2")
}

func TestSeekRejectsAnInvalidRelocation(t *testing.T) {
	path := writeFile(t, "badseek.rf", entry("row1", "cf", "cq", 10, "v1"))
	reader, err := rfile.Open(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()

	if err := reader.Seek(context.Background(), nil); !errors.Is(err, rfile.ErrInvalidSeekable) {
		t.Fatalf("Seek(nil) = %v, want ErrInvalidSeekable", err)
	}
	if err := reader.Seek(context.Background(), rfile.Seekable{}); !errors.Is(err, rfile.ErrInvalidSeekable) {
		t.Fatalf("Seek(zero Seekable) = %v, want ErrInvalidSeekable", err)
	}
}

func TestSeekRepositionsAndHonorsColumnFamilies(t *testing.T) {
	path := writeFile(t, "families.rf",
		entry("row1", "alpha", "cq", 10, "a1"),
		entry("row1", "beta", "cq", 10, "b1"),
		entry("row2", "alpha", "cq", 10, "a2"),
		entry("row2", "beta", "cq", 10, "b2"),
	)

	reader, err := rfile.Open(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()

	inclusive, err := rfile.NewSeekableColumns(mustRange(t, nil, true, nil, true), [][]byte{[]byte("beta")}, true)
	if err != nil {
		t.Fatal(err)
	}
	if err := reader.Seek(context.Background(), inclusive); err != nil {
		t.Fatal(err)
	}
	got := drain(t, reader)
	requireRows(t, got, "row1", "row2")
	for _, e := range got {
		if string(e.Key.ColumnFamily) != "beta" {
			t.Fatalf("inclusive seek yielded family %q", e.Key.ColumnFamily)
		}
	}

	exclusive, err := rfile.NewSeekableColumns(mustRange(t, nil, true, nil, true), [][]byte{[]byte("beta")}, false)
	if err != nil {
		t.Fatal(err)
	}
	if err := reader.Seek(context.Background(), exclusive); err != nil {
		t.Fatal(err)
	}
	got = drain(t, reader)
	requireRows(t, got, "row1", "row2")
	for _, e := range got {
		if string(e.Key.ColumnFamily) != "alpha" {
			t.Fatalf("exclusive seek yielded family %q", e.Key.ColumnFamily)
		}
	}
}

func TestSeekableCopiesAndReportsItsRestriction(t *testing.T) {
	families := [][]byte{[]byte("alpha")}
	keyRange := mustRange(t, "a", true, "z", false)
	seekable, err := rfile.NewSeekableColumns(keyRange, families, true)
	if err != nil {
		t.Fatal(err)
	}
	if seekable.Range() != keyRange {
		t.Fatal("Range did not report the range it was built with")
	}
	if !seekable.Inclusive() {
		t.Fatal("Inclusive = false, want true")
	}
	if seekable.String() == "" {
		t.Fatal("String returned nothing")
	}

	families[0][0] = 'A'
	got := seekable.ColumnFamilies()
	if len(got) != 1 || string(got[0]) != "alpha" {
		t.Fatalf("ColumnFamilies = %q; the caller's slice mutated the relocation", got)
	}
	got[0][0] = 'A'
	if again := seekable.ColumnFamilies(); string(again[0]) != "alpha" {
		t.Fatalf("ColumnFamilies returned an aliased slice: %q", again)
	}

	if _, err := rfile.NewSeekable(nil); !errors.Is(err, rfile.ErrInvalidSeekable) {
		t.Fatalf("NewSeekable(nil) = %v, want ErrInvalidSeekable", err)
	}
	if _, err := rfile.NewSeekableColumns(keyRange, [][]byte{nil}, true); !errors.Is(err, rfile.ErrInvalidSeekable) {
		t.Fatalf("NewSeekableColumns with a nil family = %v, want ErrInvalidSeekable", err)
	}
	if entire := rfile.EntireFile(); entire.Range() == nil || entire.ColumnFamilies() != nil {
		t.Fatalf("EntireFile = %v, want an unrestricted relocation", entire)
	}
}

// TestReaderSatisfiesTheIteratorContract pins that the reader is usable
// through the exported interface, which is what Sharkbite's KeyValueIterator
// base class gives Python callers.
func TestReaderSatisfiesTheIteratorContract(t *testing.T) {
	path := writeFile(t, "iface.rf", entry("row1", "cf", "cq", 10, "v1"))
	reader, err := rfile.OpenSequential(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()

	var iter rfile.KeyValueIterator = reader
	if !iter.HasTop() {
		t.Fatal("HasTop = false on a positioned iterator")
	}
	top, err := iter.Top()
	if err != nil {
		t.Fatal(err)
	}
	if kv := top.KeyValue(); string(kv.Key.Row) != "row1" || string(kv.Value) != "v1" {
		t.Fatalf("KeyValue = %+v, want row1/v1", kv)
	}
	if err := iter.Next(context.Background()); err != nil {
		t.Fatal(err)
	}
	if iter.HasTop() {
		t.Fatal("HasTop = true after the only entry was consumed")
	}
}

func TestOpenManyMergesFilesInKeyOrder(t *testing.T) {
	dir := t.TempDir()
	first := writeInDir(t, dir, "a.rf",
		entry("row1", "cf", "cq", 10, "a"),
		entry("row3", "cf", "cq", 10, "c"),
	)
	second := writeInDir(t, dir, "b.rf",
		entry("row2", "cf", "cq", 10, "b"),
		entry("row4", "cf", "cq", 10, "d"),
	)

	reader, err := rfile.OpenMany(context.Background(), []string{first, second}, rfile.MergeOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	requireRows(t, drain(t, reader), "row1", "row2", "row3", "row4")
}

func TestOpenManyLimitsVersions(t *testing.T) {
	dir := t.TempDir()
	first := writeInDir(t, dir, "v1.rf", entry("row1", "cf", "cq", 20, "new"))
	second := writeInDir(t, dir, "v2.rf", entry("row1", "cf", "cq", 10, "old"))

	all, err := rfile.OpenMany(context.Background(), []string{first, second}, rfile.MergeOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if got := drain(t, all); len(got) != 2 {
		t.Fatalf("unlimited versions yielded %d entries, want 2", len(got))
	}
	_ = all.Close()

	latest, err := rfile.OpenMany(context.Background(), []string{first, second}, rfile.MergeOptions{Versions: 1})
	if err != nil {
		t.Fatal(err)
	}
	defer latest.Close()
	got := drain(t, latest)
	if len(got) != 1 || string(got[0].Value) != "new" {
		t.Fatalf("Versions=1 yielded %d entries (%v), want the newest only", len(got), rows(got))
	}
}

// TestOpenManyAppliesDeletes covers the three delete modes of
// openManySequential: no delete handling, suppression with the tombstone kept,
// and suppression with the tombstone dropped.
func TestOpenManyAppliesDeletes(t *testing.T) {
	dir := t.TempDir()
	deletes := writeInDir(t, dir, "deletes.rf", tombstone("row1", "cf", "cq", 20))
	data := writeInDir(t, dir, "data.rf",
		entry("row1", "cf", "cq", 10, "gone"),
		entry("row2", "cf", "cq", 10, "kept"),
	)
	paths := []string{deletes, data}

	raw, err := rfile.OpenMany(context.Background(), paths, rfile.MergeOptions{})
	if err != nil {
		t.Fatal(err)
	}
	got := drain(t, raw)
	_ = raw.Close()
	requireRows(t, got, "row1", "row1", "row2")
	if !got[0].Deleted {
		t.Fatal("the tombstone lost its deleted flag on the way through the merge")
	}
	if got[1].Deleted || string(got[1].Value) != "gone" {
		t.Fatalf("entry under the tombstone = %+v, want the live row1 entry", got[1])
	}

	propagated, err := rfile.OpenMany(context.Background(), paths, rfile.MergeOptions{
		ApplyDeletes: true,
		Propagate:    true,
	})
	if err != nil {
		t.Fatal(err)
	}
	got = drain(t, propagated)
	_ = propagated.Close()
	requireRows(t, got, "row1", "row2")
	if !got[0].Deleted {
		t.Fatalf("Propagate=true dropped the tombstone: %+v", got[0])
	}

	applied, err := rfile.OpenMany(context.Background(), paths, rfile.MergeOptions{ApplyDeletes: true})
	if err != nil {
		t.Fatal(err)
	}
	defer applied.Close()
	got = drain(t, applied)
	requireRows(t, got, "row2")
	if string(got[0].Value) != "kept" {
		t.Fatalf("surviving entry = %+v, want row2/kept", got[0])
	}
}

// TestOpenManyAgesOffOldEntries pins the fifth openManySequential argument:
// entries older than the cutoff are dropped, which is what Sharkbite's
// AgeOffCondition.earliest_allowed_timestamp means.
func TestOpenManyAgesOffOldEntries(t *testing.T) {
	path := writeFile(t, "stamps.rf",
		entry("row1", "cf", "cq", 30, "new"),
		entry("row2", "cf", "cq", 10, "old"),
	)

	reader, err := rfile.OpenMany(context.Background(), []string{path}, rfile.MergeOptions{MinTimestamp: 20})
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	got := drain(t, reader)
	if len(got) != 1 || string(got[0].Key.Row) != "row1" {
		t.Fatalf("MinTimestamp=20 yielded %v, want only row1", rows(got))
	}
}

func TestOpenManyRejectsEmptyAndMissingPaths(t *testing.T) {
	if _, err := rfile.OpenMany(context.Background(), nil, rfile.MergeOptions{}); !errors.Is(err, rfile.ErrInvalidPath) {
		t.Fatalf("OpenMany(nil) = %v, want ErrInvalidPath", err)
	}
	missing := filepath.Join(t.TempDir(), "absent.rf")
	if _, err := rfile.OpenMany(context.Background(), []string{missing}, rfile.MergeOptions{}); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("OpenMany(missing) = %v, want a not-exist error", err)
	}
	if _, err := rfile.Open(context.Background(), ""); !errors.Is(err, rfile.ErrInvalidPath) {
		t.Fatalf("Open(\"\") = %v, want ErrInvalidPath", err)
	}
	if _, err := rfile.OpenSequential(context.Background(), missing); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("OpenSequential(missing) = %v, want a not-exist error", err)
	}
	if _, err := rfile.Create(context.Background(), "", rfile.WriterOptions{}); !errors.Is(err, rfile.ErrInvalidPath) {
		t.Fatalf("Create(\"\") = %v, want ErrInvalidPath", err)
	}
}

// TestOpenManyClosesEveryFileItOpened pins that a failure part way through
// OpenMany does not leak the handles it already took.
func TestOpenManyClosesEveryFileItOpened(t *testing.T) {
	dir := t.TempDir()
	good := writeInDir(t, dir, "good.rf", entry("row1", "cf", "cq", 10, "v"))
	missing := filepath.Join(dir, "missing.rf")
	if _, err := rfile.OpenMany(context.Background(), []string{good, missing}, rfile.MergeOptions{}); err == nil {
		t.Fatal("OpenMany with a missing second path succeeded")
	}
	// On Windows an unclosed handle blocks removal, so this is the portable
	// assertion that the first file was released.
	if err := os.Remove(good); err != nil {
		t.Fatalf("the first file was still open: %v", err)
	}
}

func TestWriterRejectsOutOfOrderAppendAndUseAfterClose(t *testing.T) {
	path := filepath.Join(t.TempDir(), "order.rf")
	writer, err := rfile.Create(context.Background(), path, rfile.WriterOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if err := writer.Append(context.Background(), entry("row2", "cf", "cq", 10, "v")); err != nil {
		t.Fatal(err)
	}
	if err := writer.Append(context.Background(), entry("row1", "cf", "cq", 10, "v")); !errors.Is(err, rfile.ErrOutOfOrder) {
		t.Fatalf("descending append = %v, want ErrOutOfOrder", err)
	}
	if err := writer.Append(context.Background(), entry("row2", "cf", "cq", 10, "v")); !errors.Is(err, rfile.ErrOutOfOrder) {
		t.Fatalf("duplicate append = %v, want ErrOutOfOrder", err)
	}
	if err := writer.AddLocalityGroup("families"); err != nil {
		t.Fatalf("AddLocalityGroup: %v", err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("second Close = %v, want nil", err)
	}
	if err := writer.Append(context.Background(), entry("row3", "cf", "cq", 10, "v")); !errors.Is(err, rfile.ErrClosed) {
		t.Fatalf("Append after Close = %v, want ErrClosed", err)
	}
	if err := writer.AddLocalityGroup("g"); !errors.Is(err, rfile.ErrClosed) {
		t.Fatalf("AddLocalityGroup after Close = %v, want ErrClosed", err)
	}
}

func TestWriterCreatesNamedLocalityGroups(t *testing.T) {
	path := filepath.Join(t.TempDir(), "groups.rf")
	writer, err := rfile.Create(context.Background(), path, rfile.WriterOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if err := writer.Append(context.Background(), entry("z", "default", "", 1, "d")); err != nil {
		t.Fatal(err)
	}
	if err := writer.AddLocalityGroup("named"); err != nil {
		t.Fatal(err)
	}
	if err := writer.Append(context.Background(), entry("a", "named", "", 1, "n")); err != nil {
		t.Fatalf("append after ordering restart: %v", err)
	}
	if err := writer.AddLocalityGroup("named"); !errors.Is(err, rfile.ErrInvalidLocalityGroup) {
		t.Fatalf("duplicate AddLocalityGroup = %v, want ErrInvalidLocalityGroup", err)
	}
	if err := writer.Append(context.Background(), entry("b", "named", "", 1, "n2")); err != nil {
		t.Fatalf("append after rejected duplicate: %v", err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	reader, err := rfile.OpenSequential(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	requireRows(t, drain(t, reader), "a", "b", "z")
}

func TestReaderRejectsUseAfterClose(t *testing.T) {
	path := writeFile(t, "closed.rf", entry("row1", "cf", "cq", 10, "v"))
	reader, err := rfile.OpenSequential(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	if err := reader.Close(); err != nil {
		t.Fatal(err)
	}
	if err := reader.Close(); err != nil {
		t.Fatalf("second Close = %v, want nil", err)
	}
	if reader.HasTop() {
		t.Fatal("HasTop is true after Close")
	}
	if _, err := reader.Top(); !errors.Is(err, rfile.ErrClosed) {
		t.Fatalf("Top after Close = %v, want ErrClosed", err)
	}
	if _, err := reader.TopKey(); !errors.Is(err, rfile.ErrClosed) {
		t.Fatalf("TopKey after Close = %v, want ErrClosed", err)
	}
	if _, err := reader.TopValue(); !errors.Is(err, rfile.ErrClosed) {
		t.Fatalf("TopValue after Close = %v, want ErrClosed", err)
	}
	if err := reader.Next(context.Background()); !errors.Is(err, rfile.ErrClosed) {
		t.Fatalf("Next after Close = %v, want ErrClosed", err)
	}
	if err := reader.Seek(context.Background(), rfile.EntireFile()); !errors.Is(err, rfile.ErrClosed) {
		t.Fatalf("Seek after Close = %v, want ErrClosed", err)
	}
}

// TestCreateRejectsAnUnsupportedCodecWithoutTouchingTheFile pins that option
// validation happens before the file is created or truncated, so a rejected
// codec cannot destroy an existing RFile.
func TestCreateRejectsAnUnsupportedCodecWithoutTouchingTheFile(t *testing.T) {
	path := writeFile(t, "codec.rf", entry("row1", "cf", "cq", 10, "v1"))
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	if _, err := rfile.Create(context.Background(), path, rfile.WriterOptions{Codec: "brotli"}); !errors.Is(err, rfile.ErrUnsupportedCodec) {
		t.Fatalf("Create with an unregistered codec = %v, want ErrUnsupportedCodec", err)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatalf("the existing file was modified: %d bytes before, %d after", len(before), len(after))
	}

	reader, err := rfile.OpenSequential(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	requireRows(t, drain(t, reader), "row1")
}

func TestTombstoneAndLiveEntryAtOneTimestampAppendInOrder(t *testing.T) {
	path := filepath.Join(t.TempDir(), "deleteorder.rf")
	writer, err := rfile.Create(context.Background(), path, rfile.WriterOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if err := writer.Append(context.Background(), tombstone("row1", "cf", "cq", 10)); err != nil {
		t.Fatal(err)
	}
	// Accumulo sorts a tombstone before the live entry at the same
	// coordinate and timestamp, so this pair is ordered, not duplicated.
	if err := writer.Append(context.Background(), entry("row1", "cf", "cq", 10, "v1")); err != nil {
		t.Fatalf("live entry after its tombstone = %v, want it accepted", err)
	}
	if err := writer.Append(context.Background(), tombstone("row1", "cf", "cq", 10)); !errors.Is(err, rfile.ErrOutOfOrder) {
		t.Fatalf("re-appending the tombstone = %v, want ErrOutOfOrder", err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}

	reader, err := rfile.OpenSequential(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	got := drain(t, reader)
	if len(got) != 2 || !got[0].Deleted || got[1].Deleted {
		t.Fatalf("entries = %+v, want the tombstone then the live entry", got)
	}
}

// TestSeekHonorsRowBoundsForKeysWithFamilies pins that a row bound covers the
// whole row: every key in the row has a non-empty family and still sorts after
// the key that spells the row alone.
func TestSeekHonorsRowBoundsForKeysWithFamilies(t *testing.T) {
	path := writeFile(t, "rowbounds.rf",
		entry("row1", "cf", "cq", 10, "v1"),
		entry("row2", "cf", "cq", 10, "v2"),
		entry("row3", "cf", "cq", 10, "v3"),
	)
	reader, err := rfile.Open(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()

	seek := func(t *testing.T, start any, startInclusive bool, end any, endInclusive bool) []rfile.Entry {
		t.Helper()
		seekable, err := rfile.NewSeekable(mustRange(t, start, startInclusive, end, endInclusive))
		if err != nil {
			t.Fatal(err)
		}
		if err := reader.Seek(context.Background(), seekable); err != nil {
			t.Fatal(err)
		}
		return drain(t, reader)
	}

	requireRows(t, seek(t, "row2", false, nil, true), "row3")
	requireRows(t, seek(t, "row2", true, nil, true), "row2", "row3")
	requireRows(t, seek(t, nil, true, "row2", true), "row1", "row2")
	requireRows(t, seek(t, nil, true, "row2", false), "row1")
	requireRows(t, seek(t, "row1", false, "row2", true), "row2")
}

// TestSeekHonorsRowBoundsForEmptyFamiliesAndTimestamps pins the harder half of
// the same contract: a cell with an empty column family and a positive
// timestamp sorts before Key{Row: R, Timestamp: 0}, because timestamps sort
// descending, so only a true first-key-of-row boundary keeps it inside an
// inclusive row start and outside an exclusive one.
func TestSeekHonorsRowBoundsForEmptyFamiliesAndTimestamps(t *testing.T) {
	bare := func(row string, timestamp int64, value string) rfile.Entry {
		return rfile.Entry{
			Key:   accumulo.Key{Row: []byte(row), Timestamp: timestamp},
			Value: []byte(value),
		}
	}
	path := writeFile(t, "emptyfamily.rf",
		bare("row1", 100, "a"),
		bare("row2", 100, "b"),
		entry("row2", "cf", "cq", 10, "c"),
		bare("row3", 100, "d"),
	)
	reader, err := rfile.Open(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()

	seek := func(t *testing.T, start any, startInclusive bool, end any, endInclusive bool) string {
		t.Helper()
		seekable, err := rfile.NewSeekable(mustRange(t, start, startInclusive, end, endInclusive))
		if err != nil {
			t.Fatal(err)
		}
		if err := reader.Seek(context.Background(), seekable); err != nil {
			t.Fatal(err)
		}
		out := ""
		for _, e := range drain(t, reader) {
			out += string(e.Value)
		}
		return out
	}

	if got := seek(t, "row2", true, nil, true); got != "bcd" {
		t.Fatalf("inclusive start at row2 = %q, want bcd: the empty-family cell of row2 must be included", got)
	}
	if got := seek(t, "row2", false, nil, true); got != "d" {
		t.Fatalf("exclusive start at row2 = %q, want d: every cell of row2 must be skipped", got)
	}
	if got := seek(t, nil, true, "row2", true); got != "abc" {
		t.Fatalf("inclusive end at row2 = %q, want abc", got)
	}
	if got := seek(t, nil, true, "row2", false); got != "a" {
		t.Fatalf("exclusive end at row2 = %q, want a: no cell of row2 may survive", got)
	}
}

func TestOperationsHonorCancelledContext(t *testing.T) {
	path := writeFile(t, "cancel.rf", entry("row1", "cf", "cq", 10, "v"))
	reader, err := rfile.OpenSequential(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := reader.Seek(ctx, rfile.EntireFile()); !errors.Is(err, context.Canceled) {
		t.Fatalf("Seek = %v, want context.Canceled", err)
	}
	if err := reader.Next(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("Next = %v, want context.Canceled", err)
	}
	if _, err := rfile.Open(ctx, path); !errors.Is(err, context.Canceled) {
		t.Fatalf("Open = %v, want context.Canceled", err)
	}
	if _, err := rfile.OpenSequential(ctx, path); !errors.Is(err, context.Canceled) {
		t.Fatalf("OpenSequential = %v, want context.Canceled", err)
	}
	if _, err := rfile.OpenMany(ctx, []string{path}, rfile.MergeOptions{}); !errors.Is(err, context.Canceled) {
		t.Fatalf("OpenMany = %v, want context.Canceled", err)
	}
	if _, err := rfile.Create(ctx, filepath.Join(t.TempDir(), "x.rf"), rfile.WriterOptions{}); !errors.Is(err, context.Canceled) {
		t.Fatalf("Create = %v, want context.Canceled", err)
	}

	writer, err := rfile.Create(context.Background(), filepath.Join(t.TempDir(), "y.rf"), rfile.WriterOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Close()
	if err := writer.Append(ctx, entry("row1", "cf", "cq", 10, "v")); !errors.Is(err, context.Canceled) {
		t.Fatalf("Append = %v, want context.Canceled", err)
	}
}

func TestIndependentReadersOverOneFileAreConcurrent(t *testing.T) {
	path := writeFile(t, "shared.rf",
		entry("row1", "cf", "cq", 10, "v1"),
		entry("row2", "cf", "cq", 10, "v2"),
		entry("row3", "cf", "cq", 10, "v3"),
	)

	var wg sync.WaitGroup
	for worker := 0; worker < 8; worker++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 10; i++ {
				reader, err := rfile.OpenSequential(context.Background(), path)
				if err != nil {
					t.Error(err)
					return
				}
				count := 0
				for reader.HasTop() {
					if _, err := reader.Top(); err != nil {
						t.Error(err)
						_ = reader.Close()
						return
					}
					if err := reader.Next(context.Background()); err != nil {
						t.Error(err)
						_ = reader.Close()
						return
					}
					count++
				}
				if count != 3 {
					t.Errorf("read %d entries, want 3", count)
				}
				if err := reader.Close(); err != nil {
					t.Error(err)
					return
				}
			}
		}()
	}
	wg.Wait()
}

// TestCloseDuringConcurrentReadsIsSafe pins that the cursor lock covers Close,
// so a reader shared by several goroutines cannot free a handle mid-read.
func TestCloseDuringConcurrentReadsIsSafe(t *testing.T) {
	path := writeFile(t, "raced.rf",
		entry("row1", "cf", "cq", 10, "v1"),
		entry("row2", "cf", "cq", 10, "v2"),
		entry("row3", "cf", "cq", 10, "v3"),
	)
	reader, err := rfile.OpenSequential(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}

	var wg sync.WaitGroup
	for worker := 0; worker < 4; worker++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 50; i++ {
				if !reader.HasTop() {
					continue
				}
				if _, err := reader.Top(); err != nil && !errors.Is(err, rfile.ErrClosed) && !errors.Is(err, rfile.ErrNoTop) {
					t.Error(err)
					return
				}
				if err := reader.Next(context.Background()); err != nil &&
					!errors.Is(err, rfile.ErrClosed) && !errors.Is(err, rfile.ErrNoTop) {
					t.Error(err)
					return
				}
			}
		}()
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		if err := reader.Close(); err != nil {
			t.Error(err)
		}
	}()
	wg.Wait()
}

func TestValuesAndKeysAreCopies(t *testing.T) {
	path := writeFile(t, "copies.rf",
		entry("row1", "cf", "cq", 10, "v1"),
		entry("row2", "cf", "cq", 10, "v2"),
	)
	reader, err := rfile.OpenSequential(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()

	first, err := reader.Top()
	if err != nil {
		t.Fatal(err)
	}
	value, err := reader.TopValue()
	if err != nil {
		t.Fatal(err)
	}
	if err := reader.Next(context.Background()); err != nil {
		t.Fatal(err)
	}
	if string(first.Key.Row) != "row1" || string(first.Value) != "v1" {
		t.Fatalf("entry mutated after Next: %+v", first)
	}
	if string(value) != "v1" {
		t.Fatalf("value mutated after Next: %q", value)
	}
	first.Value[0] = 'X'
	first.Key.Row[0] = 'X'
	value[0] = 'X'
	second, err := reader.Top()
	if err != nil {
		t.Fatal(err)
	}
	if string(second.Key.Row) != "row2" || string(second.Value) != "v2" {
		t.Fatalf("mutating a returned entry corrupted the cursor: %+v", second)
	}
}

// TestWriterCopiesTheCallersBuffers pins that a caller can reuse the slices it
// appended, which Sharkbite's shared_ptr keys cannot promise.
func TestWriterCopiesTheCallersBuffers(t *testing.T) {
	path := filepath.Join(t.TempDir(), "reuse.rf")
	writer, err := rfile.Create(context.Background(), path, rfile.WriterOptions{})
	if err != nil {
		t.Fatal(err)
	}
	row := []byte("row1")
	value := []byte("v1")
	if err := writer.Append(context.Background(), rfile.Entry{
		Key:   accumulo.Key{Row: row, ColumnFamily: []byte("cf"), ColumnQualifier: []byte("cq"), Timestamp: 10},
		Value: value,
	}); err != nil {
		t.Fatal(err)
	}
	copy(row, "ROW9")
	copy(value, "V9")
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}

	reader, err := rfile.OpenSequential(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	got := drain(t, reader)
	if len(got) != 1 || string(got[0].Key.Row) != "row1" || string(got[0].Value) != "v1" {
		t.Fatalf("entry = %+v, want the values as they were at Append", got)
	}
}

// TestBinaryDataRoundTrips pins that every field is bytes, not text: NUL bytes
// and invalid UTF-8 survive a write/read cycle unchanged.
func TestBinaryDataRoundTrips(t *testing.T) {
	binary := []byte{0x00, 0xff, 0xfe, 0x01, 0x00}
	written := rfile.Entry{
		Key: accumulo.Key{
			Row:              binary,
			ColumnFamily:     []byte{0x00, 'c'},
			ColumnQualifier:  []byte{0xc3, 0x28},
			ColumnVisibility: []byte{0x00, 'V'},
			Timestamp:        7,
		},
		Value: []byte{0x00, 0x80, 0x00},
	}
	path := writeFile(t, "binary.rf", written)

	reader, err := rfile.OpenSequential(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	got := drain(t, reader)
	if len(got) != 1 {
		t.Fatalf("read %d entries, want 1", len(got))
	}
	if !bytes.Equal(got[0].Key.Row, written.Key.Row) ||
		!bytes.Equal(got[0].Key.ColumnFamily, written.Key.ColumnFamily) ||
		!bytes.Equal(got[0].Key.ColumnQualifier, written.Key.ColumnQualifier) ||
		!bytes.Equal(got[0].Key.ColumnVisibility, written.Key.ColumnVisibility) ||
		!bytes.Equal(got[0].Value, written.Value) ||
		got[0].Key.Timestamp != written.Key.Timestamp {
		t.Fatalf("round trip changed the entry: %+v, want %+v", got[0], written)
	}
}

// TestEntryHelpers pins the conversions between the RFile entry type and the
// scan-shaped accumulo.KeyValue.
func TestEntryHelpers(t *testing.T) {
	kv := accumulo.KeyValue{Key: accumulo.Key{Row: []byte("row1")}, Value: []byte("v")}
	live := rfile.NewEntry(kv)
	if live.Deleted {
		t.Fatal("NewEntry produced a tombstone")
	}
	if got := live.KeyValue(); string(got.Key.Row) != "row1" || string(got.Value) != "v" {
		t.Fatalf("KeyValue = %+v, want the pair it was built from", got)
	}
	dead := rfile.NewTombstone(kv.Key)
	if !dead.Deleted || len(dead.Value) != 0 {
		t.Fatalf("NewTombstone = %+v, want an empty deleted entry", dead)
	}
	if !dead.Key.Deleted {
		t.Fatal("NewTombstone left the key's own deletion marker clear")
	}
	if got := dead.KeyValue(); !got.Key.Deleted {
		t.Fatal("KeyValue dropped the tombstone marker")
	}
	// A key that already carries the marker produces a tombstone, so the two
	// flags never disagree.
	marked := kv
	marked.Key.Deleted = true
	if entry := rfile.NewEntry(marked); !entry.Deleted || !entry.Key.Deleted {
		t.Fatalf("NewEntry on a marked key = %+v, want a tombstone", entry)
	}
}

// TestKeyDeletionMarkerSurvivesTheFile pins that accumulo.Key.Deleted is the
// same state as Entry.Deleted on both the write and the read side.
func TestKeyDeletionMarkerSurvivesTheFile(t *testing.T) {
	key := accumulo.Key{Row: []byte("row1"), ColumnFamily: []byte("cf"),
		ColumnQualifier: []byte("cq"), Timestamp: 20, Deleted: true}
	path := writeFile(t, "key-marker.rf",
		rfile.Entry{Key: key},
		entry("row2", "cf", "cq", 10, "live"),
	)
	reader, err := rfile.OpenSequential(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	got := drain(t, reader)
	requireRows(t, got, "row1", "row2")
	if !got[0].Deleted || !got[0].Key.Deleted {
		t.Fatalf("the marker did not survive: entry=%v key=%v", got[0].Deleted, got[0].Key.Deleted)
	}
	if got[1].Deleted || got[1].Key.Deleted {
		t.Fatalf("a live entry came back marked: %+v", got[1])
	}
}

// TestTombstonesRoundTrip pins that the deleted flag survives the file, which
// is what makes MergeOptions.ApplyDeletes meaningful.
func TestTombstonesRoundTrip(t *testing.T) {
	path := writeFile(t, "tombstone.rf",
		tombstone("row1", "cf", "cq", 20),
		entry("row2", "cf", "cq", 10, "live"),
	)
	reader, err := rfile.OpenSequential(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	got := drain(t, reader)
	requireRows(t, got, "row1", "row2")
	if !got[0].Deleted {
		t.Fatalf("first entry = %+v, want a tombstone", got[0])
	}
	if got[1].Deleted {
		t.Fatalf("second entry = %+v, want a live entry", got[1])
	}
}

func mustRange(t *testing.T, start any, startInclusive bool, end any, endInclusive bool) *accumulo.Range {
	t.Helper()
	toRow := func(v any) []byte {
		switch typed := v.(type) {
		case nil:
			return nil
		case string:
			return []byte(typed)
		case []byte:
			return typed
		default:
			t.Fatalf("unsupported row type %T", v)
			return nil
		}
	}
	keyRange, err := accumulo.NewRange(toRow(start), startInclusive, toRow(end), endInclusive)
	if err != nil {
		t.Fatal(err)
	}
	return keyRange
}
