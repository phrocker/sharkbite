package engine

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/phrocker/shoal-oss/internal/cclient"
	"github.com/phrocker/shoal-oss/internal/iterrt"
	"github.com/phrocker/shoal-oss/internal/rfile"
	"github.com/phrocker/shoal-oss/internal/rfile/bcfile"
	"github.com/phrocker/shoal-oss/internal/rfile/wire"
	"github.com/phrocker/shoal-oss/internal/storage"
	"github.com/phrocker/shoal-oss/internal/storage/memory"
	"github.com/phrocker/shoal-oss/internal/tablet"
)

type exportSourceBackend struct{}

func (exportSourceBackend) Open(context.Context, string) (storage.File, error) {
	return exportFailingFile{}, nil
}

type committedImportBackend struct {
	*memory.Backend
	failManifest bool
}

func (b *committedImportBackend) Create(ctx context.Context, path string) (storage.Writer, error) {
	writer, err := b.Backend.Create(ctx, path)
	if err != nil {
		return nil, err
	}
	if filepath.Base(path) != "files.json" || !b.failManifest {
		return writer, nil
	}
	b.failManifest = false
	return &committedImportWriter{Writer: writer}, nil
}

type committedImportWriter struct {
	storage.Writer
}

func (w *committedImportWriter) Close() error {
	if err := w.Writer.Close(); err != nil {
		return err
	}
	return storage.MarkCommittedWrite(errors.New("injected post-commit cleanup failure"))
}

type exportFailingFile struct{}

func (exportFailingFile) ReadAt([]byte, int64) (int, error) { return 0, io.ErrUnexpectedEOF }
func (exportFailingFile) Close() error                      { return nil }
func (exportFailingFile) Size() int64                       { return 1 }

type exportDestinationBackend struct {
	writer *exportAbortWriter
}

func (*exportDestinationBackend) Open(context.Context, string) (storage.File, error) {
	return nil, storage.ErrNotFound
}

func (b *exportDestinationBackend) Create(context.Context, string) (storage.Writer, error) {
	return b.writer, nil
}

type exportAbortWriter struct {
	aborted bool
	closed  bool
}

func (*exportAbortWriter) Write(p []byte) (int, error) { return len(p), nil }
func (w *exportAbortWriter) Close() error {
	w.closed = true
	return nil
}
func (w *exportAbortWriter) Abort() error {
	w.aborted = true
	return nil
}

func TestCopyWithSHA256AbortsDestinationOnReadFailure(t *testing.T) {
	writer := &exportAbortWriter{}
	_, _, _, err := copyWithSHA256(
		context.Background(),
		exportSourceBackend{},
		"source.rf",
		&exportDestinationBackend{writer: writer},
		"destination.rf",
	)
	if !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("copyWithSHA256 error = %v, want read failure", err)
	}
	if !writer.aborted {
		t.Fatal("failed export did not abort destination")
	}
	if writer.closed {
		t.Fatal("failed export closed and could have committed destination")
	}
}

func TestImportRFileManifestRejectsUnsafeSourceTableBeforeSideEffects(t *testing.T) {
	for _, name := range []string{"", ".", "..", "../outside", `..\outside`, "/absolute", `C:\absolute`} {
		t.Run(fmt.Sprintf("%q", name), func(t *testing.T) {
			root := t.TempDir()
			dst, err := Open(filepath.Join(root, "engine"), Options{Backend: memory.New()})
			if err != nil {
				t.Fatal(err)
			}
			defer dst.Close()

			err = dst.ImportRFileManifest(context.Background(), &RFileExportManifest{
				Version:     RFileExportManifestVersion,
				SourceTable: name,
			})
			if err == nil || !strings.Contains(err.Error(), "invalid imported table name") {
				t.Fatalf("ImportRFileManifest(%q) error = %v, want table-name rejection", name, err)
			}
			entries, readErr := os.ReadDir(root)
			if readErr != nil {
				t.Fatal(readErr)
			}
			if len(entries) != 1 || entries[0].Name() != "engine" {
				t.Fatalf("root entries = %v, want only engine directory", entries)
			}
		})
	}
}

func TestRFileExportImportMemoryRoundTrip(t *testing.T) {
	ctx := context.Background()
	srcDir := filepath.Join(t.TempDir(), "src")
	dstDir := filepath.Join(t.TempDir(), "dst")

	src, err := Open(srcDir, Options{})
	if err != nil {
		t.Fatalf("Open source: %v", err)
	}
	if err := src.CreateTable("graph", TableOptions{Splits: PrefixSplit("entity:", "event:")}); err != nil {
		t.Fatalf("CreateTable: %v", err)
	}
	for i, row := range []string{"entity:a", "entity:b", "event:1", "z:last"} {
		m, err := cclient.NewMutation([]byte(row))
		if err != nil {
			t.Fatalf("NewMutation: %v", err)
		}
		m.Put([]byte("cf"), []byte(fmt.Sprintf("cq-%d", i)), []byte("tenantA"), int64(100+i), []byte("value-"+row))
		if err := src.Write("graph", []*cclient.Mutation{m}); err != nil {
			t.Fatalf("Write: %v", err)
		}
	}

	dstBackend := memory.New()
	manifest, err := src.ExportRFiles(ctx, "graph", dstBackend, RFileExportOptions{
		DestinationRoot:     dstDir,
		CFSchema:            "graphschema/v1",
		VisibilityStamp:     "tenantA",
		AuthorizationsStamp: "tenantA",
		EngineVersion:       "test",
	})
	if err != nil {
		t.Fatalf("ExportRFiles: %v", err)
	}
	defer src.Close()
	if got, want := manifest.Version, RFileExportManifestLegacyVersion; got != want {
		t.Fatalf("manifest version = %d, want %d", got, want)
	}
	if got, want := manifest.CFSchema, "graphschema/v1"; got != want {
		t.Fatalf("cf schema = %q, want %q", got, want)
	}
	if len(manifest.RFiles) == 0 {
		t.Fatalf("manifest has no RFiles")
	}
	if len(manifest.Tablets) != 3 {
		t.Fatalf("manifest tablets = %d, want 3", len(manifest.Tablets))
	}
	for _, rf := range manifest.RFiles {
		if rf.Size <= 0 {
			t.Fatalf("RFile %s has non-positive size %d", rf.DestinationPath, rf.Size)
		}
		if len(rf.SHA256) != 64 {
			t.Fatalf("RFile %s sha256 = %q", rf.DestinationPath, rf.SHA256)
		}
		if rf.BCFileVersion == "" {
			t.Fatalf("RFile %s missing BCFile version", rf.DestinationPath)
		}
		if !strings.HasPrefix(rf.DestinationPath, dstDir) {
			t.Fatalf("RFile destination %q not under %q", rf.DestinationPath, dstDir)
		}
	}
	if err := VerifyRFileExport(ctx, dstBackend, manifest); err != nil {
		t.Fatalf("VerifyRFileExport: %v", err)
	}

	wantCells := scanAll(t, src, "graph")
	dst, err := Open(dstDir, Options{Backend: dstBackend})
	if err != nil {
		t.Fatalf("Open destination: %v", err)
	}
	defer dst.Close()
	if err := dst.ImportRFileManifest(ctx, manifest); err != nil {
		t.Fatalf("ImportRFileManifest: %v", err)
	}
	gotCells := scanAll(t, dst, "graph")
	if fmt.Sprint(gotCells) != fmt.Sprint(wantCells) {
		t.Fatalf("imported scan mismatch\ngot  %v\nwant %v", gotCells, wantCells)
	}
}

func TestImportRFileManifestPreservesFilesAfterCommittedManifestError(t *testing.T) {
	ctx := context.Background()
	src, err := Open(filepath.Join(t.TempDir(), "src"), Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer src.Close()
	if err := src.CreateTable("graph", TableOptions{}); err != nil {
		t.Fatal(err)
	}
	writeRow(t, src, "graph", "row", 0)

	backend := &committedImportBackend{Backend: memory.New()}
	dstDir := filepath.Join(t.TempDir(), "dst")
	dst, err := Open(dstDir, Options{Backend: backend})
	if err != nil {
		t.Fatal(err)
	}
	if err := dst.CreateTable("graph", TableOptions{}); err != nil {
		t.Fatal(err)
	}
	manifest, err := src.ExportRFiles(ctx, "graph", backend, RFileExportOptions{
		DestinationRoot: dstDir,
	})
	if err != nil {
		t.Fatal(err)
	}
	backend.failManifest = true
	err = dst.ImportRFileManifest(ctx, manifest)
	if !storage.IsCommittedWriteError(err) {
		t.Fatalf("ImportRFileManifest error = %v, want committed-write error", err)
	}
	if err := dst.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := Open(dstDir, Options{Backend: backend})
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if got := scanAll(t, reopened, "graph"); fmt.Sprint(got) != "[row|cf|cq-0|tenantA|100|value-row]" {
		t.Fatalf("reopened imported cells = %v", got)
	}
	foundStaged := false
	for _, key := range backend.Keys() {
		if strings.Contains(filepath.Base(key), ".shoal-import-") {
			foundStaged = true
			break
		}
	}
	if !foundStaged {
		t.Fatal("committed import object was removed by rollback")
	}
}

// TestRFileExportTabletJSONRoundTripsNonUTF8Rows proves the fix for a
// real, previously-latent bug: RFileExportTablet.StartRow/EndRow hold
// arbitrary Accumulo row bytes, not necessarily valid UTF-8, but
// encoding/json's default string handling silently replaces any invalid
// UTF-8 byte sequence with U+FFFD. That was harmless while nothing read
// a non-nil StartRow/EndRow's value (the original single-tablet-only
// promotion code rejected any manifest that declared one), but
// multi-tablet promotion's resolveManifestTablets now depends on these
// bytes being exact. MarshalJSON/UnmarshalJSON must round-trip
// non-UTF-8-safe rows byte-for-byte via base64, not silently corrupt
// them.
func TestRFileExportTabletJSONRoundTripsNonUTF8Rows(t *testing.T) {
	nonUTF8 := string([]byte{0x80, 0xff, 0xc0, 0x00, 0x01})
	ascii := "valid-ascii-row"
	original := RFileExportTablet{Index: 1, StartRow: &nonUTF8, EndRow: &ascii}

	data, err := json.Marshal(original)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	// The wire form must carry the row under start_row_b64 as base64,
	// not as a JSON string literal holding the raw bytes (which would
	// corrupt them), and must not fall back to writing the legacy
	// start_row key (which is now read-only, for old manifests).
	var wire struct {
		StartRow    *string `json:"start_row"`
		StartRowB64 *string `json:"start_row_b64"`
	}
	if err := json.Unmarshal(data, &wire); err != nil {
		t.Fatalf("Unmarshal wire probe: %v", err)
	}
	if wire.StartRow != nil {
		t.Fatalf("legacy start_row = %q, want absent (new manifests must only carry start_row_b64)", *wire.StartRow)
	}
	if wire.StartRowB64 == nil {
		t.Fatal("start_row_b64 missing from wire form")
	}
	if _, err := base64.URLEncoding.DecodeString(*wire.StartRowB64); err != nil {
		t.Fatalf("start_row_b64 %q is not valid base64: %v", *wire.StartRowB64, err)
	}

	var round RFileExportTablet
	if err := json.Unmarshal(data, &round); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if round.Index != original.Index {
		t.Fatalf("Index = %d, want %d", round.Index, original.Index)
	}
	if round.StartRow == nil || *round.StartRow != nonUTF8 {
		t.Fatalf("StartRow round-trip mismatch: got %v, want exact byte match with the original non-UTF-8 row", round.StartRow)
	}
	if round.EndRow == nil || *round.EndRow != ascii {
		t.Fatalf("EndRow = %v, want %q", round.EndRow, ascii)
	}
}

// TestRFileExportTabletJSONOmitsNilBoundaries confirms the encoding
// change is fully backward compatible with every manifest that could
// ever round-trip successfully before it: a single-tablet manifest's
// StartRow/EndRow are always nil, and must stay entirely absent from
// the wire form (as before), not merely encoded as an empty value.
func TestRFileExportTabletJSONOmitsNilBoundaries(t *testing.T) {
	original := RFileExportTablet{Index: 0}
	data, err := json.Marshal(original)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if strings.Contains(string(data), "start_row") || strings.Contains(string(data), "end_row") {
		t.Fatalf("nil boundaries must be omitted from the wire form, got %s", data)
	}
	var round RFileExportTablet
	if err := json.Unmarshal(data, &round); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if round.StartRow != nil || round.EndRow != nil {
		t.Fatalf("round-tripped nil boundaries = (%v, %v), want (nil, nil)", round.StartRow, round.EndRow)
	}
}

// TestRFileExportTabletJSONRejectsMalformedBase64 confirms a corrupted
// or hand-edited manifest's start_row_b64 field fails closed at decode
// time with a clear error, rather than silently misinterpreting
// garbage as row bytes. (The legacy start_row key is intentionally NOT
// base64-validated -- see TestRFileExportTabletJSONDecodesLegacyPlainStringRows
// -- so this test exercises start_row_b64, the only field this
// validation now applies to.)
func TestRFileExportTabletJSONRejectsMalformedBase64(t *testing.T) {
	data := []byte(`{"index":0,"start_row_b64":"not valid base64!!"}`)
	var tablet RFileExportTablet
	if err := json.Unmarshal(data, &tablet); err == nil {
		t.Fatal("Unmarshal with malformed base64 start_row = nil error, want error")
	}
}

// TestRFileExportTabletJSONDecodesLegacyPlainStringRows proves the
// backward-compatibility guarantee in RFileExportTablet's doc comment:
// a manifest already persisted by a build that predates the
// start_row_b64/end_row_b64 encoding -- using the plain start_row/
// end_row string keys exportTablets has always been able to populate,
// for example one sitting on a storage backend waiting for
// `shoal-embed import`, and not yet touched by this fix -- must still
// decode successfully and losslessly for the UTF-8-safe rows that
// format could ever correctly express, exactly as it always did before
// this change.
func TestRFileExportTabletJSONDecodesLegacyPlainStringRows(t *testing.T) {
	data := []byte(`{"index":2,"start_row":"g","end_row":"m"}`)
	var tablet RFileExportTablet
	if err := json.Unmarshal(data, &tablet); err != nil {
		t.Fatalf("Unmarshal legacy plain-string manifest: %v", err)
	}
	if tablet.Index != 2 {
		t.Fatalf("Index = %d, want 2", tablet.Index)
	}
	if tablet.StartRow == nil || *tablet.StartRow != "g" {
		t.Fatalf("StartRow = %v, want \"g\"", tablet.StartRow)
	}
	if tablet.EndRow == nil || *tablet.EndRow != "m" {
		t.Fatalf("EndRow = %v, want \"m\"", tablet.EndRow)
	}
}

// TestRFileExportTabletJSONPrefersB64OverLegacyWhenBothPresent documents
// the (never produced by this package, but still deterministic) case of
// a tablet object carrying both wire forms at once: the current,
// byte-safe start_row_b64 representation must win.
func TestRFileExportTabletJSONPrefersB64OverLegacyWhenBothPresent(t *testing.T) {
	b64 := base64.URLEncoding.EncodeToString([]byte("b64-row"))
	data := []byte(`{"index":0,"start_row_b64":"` + b64 + `","start_row":"legacy-row"}`)
	var tablet RFileExportTablet
	if err := json.Unmarshal(data, &tablet); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if tablet.StartRow == nil || *tablet.StartRow != "b64-row" {
		t.Fatalf("StartRow = %v, want \"b64-row\" (start_row_b64 must take priority over legacy start_row)", tablet.StartRow)
	}
}

// TestRFileExportManifestJSONRoundTripsMultiTabletNonUTF8Splits
// exercises the exact path production tooling uses (cmd/shoal-embed's
// `export`/`import` subcommands: json.Marshal/json.Unmarshal on a whole
// *RFileExportManifest, not just one tablet in isolation) with a
// realistic multi-tablet chain whose split rows are not valid UTF-8, to
// confirm the manifest-level round trip -- not just the tablet type in
// isolation -- preserves every boundary byte-for-byte.
func TestRFileExportManifestJSONRoundTripsMultiTabletNonUTF8Splits(t *testing.T) {
	rowA := string([]byte{0x00, 0x80, 0xff})
	rowB := string([]byte{0xff, 0xff})
	manifest := &RFileExportManifest{
		Version:     RFileExportManifestVersion,
		SourceTable: "events",
		Tablets: []RFileExportTablet{
			{Index: 0, EndRow: &rowA},
			{Index: 1, StartRow: &rowA, EndRow: &rowB},
			{Index: 2, StartRow: &rowB},
		},
	}

	data, err := json.Marshal(manifest)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	var round RFileExportManifest
	if err := json.Unmarshal(data, &round); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if len(round.Tablets) != 3 {
		t.Fatalf("tablets = %d, want 3", len(round.Tablets))
	}
	if round.Tablets[0].StartRow != nil {
		t.Fatalf("tablet 0 StartRow = %v, want nil", round.Tablets[0].StartRow)
	}
	if round.Tablets[0].EndRow == nil || *round.Tablets[0].EndRow != rowA {
		t.Fatalf("tablet 0 EndRow round-trip mismatch: got %v", round.Tablets[0].EndRow)
	}
	if round.Tablets[1].StartRow == nil || *round.Tablets[1].StartRow != rowA {
		t.Fatalf("tablet 1 StartRow round-trip mismatch: got %v", round.Tablets[1].StartRow)
	}
	if round.Tablets[1].EndRow == nil || *round.Tablets[1].EndRow != rowB {
		t.Fatalf("tablet 1 EndRow round-trip mismatch: got %v", round.Tablets[1].EndRow)
	}
	if round.Tablets[2].StartRow == nil || *round.Tablets[2].StartRow != rowB {
		t.Fatalf("tablet 2 StartRow round-trip mismatch: got %v", round.Tablets[2].StartRow)
	}
	if round.Tablets[2].EndRow != nil {
		t.Fatalf("tablet 2 EndRow = %v, want nil", round.Tablets[2].EndRow)
	}
}

// TestRFileExportManifestJSONDecodesLegacyMultiTabletManifest proves a
// whole multi-tablet manifest already persisted in the pre-fix wire
// shape -- every start_row/end_row a plain JSON string, exactly what
// exportTablets combined with the original json.Marshal-based encoding
// produced for any multi-tablet table before start_row_b64/end_row_b64
// existed -- still decodes successfully via cmd/shoal-embed import's
// exact code path (json.Unmarshal into *RFileExportManifest), for the
// UTF-8-safe split rows that pre-fix format could ever correctly
// express. This is the manifest-level counterpart of
// TestRFileExportTabletJSONDecodesLegacyPlainStringRows.
func TestRFileExportManifestJSONDecodesLegacyMultiTabletManifest(t *testing.T) {
	legacy := []byte(`{
		"version": 1,
		"source_table": "events",
		"tablets": [
			{"index": 0, "end_row": "g"},
			{"index": 1, "start_row": "g", "end_row": "m"},
			{"index": 2, "start_row": "m"}
		],
		"rfiles": []
	}`)
	var manifest RFileExportManifest
	if err := json.Unmarshal(legacy, &manifest); err != nil {
		t.Fatalf("Unmarshal legacy manifest: %v", err)
	}
	if len(manifest.Tablets) != 3 {
		t.Fatalf("tablets = %d, want 3", len(manifest.Tablets))
	}
	if manifest.Tablets[0].StartRow != nil {
		t.Fatalf("tablet 0 StartRow = %v, want nil", manifest.Tablets[0].StartRow)
	}
	if manifest.Tablets[0].EndRow == nil || *manifest.Tablets[0].EndRow != "g" {
		t.Fatalf("tablet 0 EndRow = %v, want \"g\"", manifest.Tablets[0].EndRow)
	}
	if manifest.Tablets[1].StartRow == nil || *manifest.Tablets[1].StartRow != "g" {
		t.Fatalf("tablet 1 StartRow = %v, want \"g\"", manifest.Tablets[1].StartRow)
	}
	if manifest.Tablets[1].EndRow == nil || *manifest.Tablets[1].EndRow != "m" {
		t.Fatalf("tablet 1 EndRow = %v, want \"m\"", manifest.Tablets[1].EndRow)
	}
	if manifest.Tablets[2].StartRow == nil || *manifest.Tablets[2].StartRow != "m" {
		t.Fatalf("tablet 2 StartRow = %v, want \"m\"", manifest.Tablets[2].StartRow)
	}
	if manifest.Tablets[2].EndRow != nil {
		t.Fatalf("tablet 2 EndRow = %v, want nil", manifest.Tablets[2].EndRow)
	}
}

func TestCopyWithSHA256PreservesDestinationOnReadFailure(t *testing.T) {
	readErr := errors.New("read failed")
	src := exportFileBackend{file: &exportReadFuncFile{
		size: 2,
		read: func(p []byte, off int64) (int, error) {
			switch off {
			case 0:
				p[0] = 'x'
				return 1, nil
			default:
				return 0, readErr
			}
		},
	}}
	dst := newExportTrackingBackend()
	dst.files["/dst.rf"] = []byte("old")

	written, sum, bcVersion, err := copyWithSHA256(context.Background(), src, "/src.rf", dst, "/dst.rf")
	if !errors.Is(err, readErr) {
		t.Fatalf("copyWithSHA256 error = %v, want %v", err, readErr)
	}
	if written != 1 {
		t.Fatalf("written = %d, want 1", written)
	}
	if sum != "" {
		t.Fatalf("sum = %q, want empty on failure", sum)
	}
	if bcVersion != "" {
		t.Fatalf("bcVersion = %q, want empty on failure", bcVersion)
	}
	if got := string(dst.files["/dst.rf"]); got != "old" {
		t.Fatalf("destination contents = %q, want old", got)
	}
	if dst.lastWriter == nil || dst.lastWriter.abortCalls != 1 {
		t.Fatalf("Abort calls = %d, want 1", dst.lastWriter.abortCalls)
	}
	if dst.lastWriter.stagePresent {
		t.Fatal("failed export copy left staged bytes behind")
	}
}

func TestCopyWithSHA256AbortsDestinationOnShortWrite(t *testing.T) {
	src := exportFileBackend{file: &exportReadFuncFile{
		size: 3,
		read: func(p []byte, off int64) (int, error) {
			switch off {
			case 0:
				copy(p, []byte("xyz"))
				return 3, io.EOF
			default:
				return 0, io.EOF
			}
		},
	}}
	dst := newExportTrackingBackend()
	dst.files["/dst.rf"] = []byte("old")
	dst.shortWrite = 1

	written, sum, bcVersion, err := copyWithSHA256(context.Background(), src, "/src.rf", dst, "/dst.rf")
	if !errors.Is(err, io.ErrShortWrite) {
		t.Fatalf("copyWithSHA256 error = %v, want %v", err, io.ErrShortWrite)
	}
	if written != 1 {
		t.Fatalf("written = %d, want 1", written)
	}
	if sum != "" {
		t.Fatalf("sum = %q, want empty on failure", sum)
	}
	if bcVersion != "" {
		t.Fatalf("bcVersion = %q, want empty on failure", bcVersion)
	}
	if got := string(dst.files["/dst.rf"]); got != "old" {
		t.Fatalf("destination contents = %q, want old", got)
	}
	if dst.lastWriter == nil || dst.lastWriter.abortCalls != 1 {
		t.Fatalf("Abort calls = %d, want 1", dst.lastWriter.abortCalls)
	}
	if dst.lastWriter.closed {
		t.Fatal("short export write closed and could have committed destination")
	}
	if dst.lastWriter.stagePresent {
		t.Fatal("short export write left staged bytes behind")
	}
}

func TestCopyWithSHA256AbortsDestinationOnPrematureEOF(t *testing.T) {
	src := exportFileBackend{file: &exportReadFuncFile{
		size: 2,
		read: func(p []byte, off int64) (int, error) {
			switch off {
			case 0:
				p[0] = 'x'
				return 1, io.EOF
			default:
				return 0, io.EOF
			}
		},
	}}
	dst := newExportTrackingBackend()
	dst.files["/dst.rf"] = []byte("old")

	written, sum, bcVersion, err := copyWithSHA256(context.Background(), src, "/src.rf", dst, "/dst.rf")
	if !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("copyWithSHA256 error = %v, want %v", err, io.ErrUnexpectedEOF)
	}
	if written != 1 {
		t.Fatalf("written = %d, want 1", written)
	}
	if sum != "" {
		t.Fatalf("sum = %q, want empty on failure", sum)
	}
	if bcVersion != "" {
		t.Fatalf("bcVersion = %q, want empty on failure", bcVersion)
	}
	if got := string(dst.files["/dst.rf"]); got != "old" {
		t.Fatalf("destination contents = %q, want old", got)
	}
	if dst.lastWriter == nil || dst.lastWriter.abortCalls != 1 {
		t.Fatalf("Abort calls = %d, want 1", dst.lastWriter.abortCalls)
	}
	if dst.lastWriter.closed {
		t.Fatal("premature EOF closed and could have committed destination")
	}
	if dst.lastWriter.stagePresent {
		t.Fatal("premature EOF left staged bytes behind")
	}
}

func TestCopyWithSHA256CountsPartialWriteProgressBeforeError(t *testing.T) {
	src := exportFileBackend{file: &exportReadFuncFile{
		size: 3,
		read: func(p []byte, off int64) (int, error) {
			switch off {
			case 0:
				copy(p, []byte("xyz"))
				return 3, io.EOF
			default:
				return 0, io.EOF
			}
		},
	}}
	dst := newExportTrackingBackend()
	dst.files["/dst.rf"] = []byte("old")
	dst.partialErrBytes = 2
	dst.writeErr = errors.New("write failed")

	written, sum, bcVersion, err := copyWithSHA256(context.Background(), src, "/src.rf", dst, "/dst.rf")
	if !errors.Is(err, dst.writeErr) {
		t.Fatalf("copyWithSHA256 error = %v, want %v", err, dst.writeErr)
	}
	if written != 2 {
		t.Fatalf("written = %d, want 2", written)
	}
	if sum != "" {
		t.Fatalf("sum = %q, want empty on failure", sum)
	}
	if bcVersion != "" {
		t.Fatalf("bcVersion = %q, want empty on failure", bcVersion)
	}
	if got := string(dst.files["/dst.rf"]); got != "old" {
		t.Fatalf("destination contents = %q, want old", got)
	}
	if dst.lastWriter == nil || dst.lastWriter.abortCalls != 1 {
		t.Fatalf("Abort calls = %d, want 1", dst.lastWriter.abortCalls)
	}
	if dst.lastWriter.closed {
		t.Fatal("partial error write closed and could have committed destination")
	}
	if dst.lastWriter.stagePresent {
		t.Fatal("partial error write left staged bytes behind")
	}
}

func TestParquetExportImportWithVisibilityStamp(t *testing.T) {
	ctx := context.Background()
	srcDir := filepath.Join(t.TempDir(), "src")
	dstDir := filepath.Join(t.TempDir(), "dst")
	src, err := Open(srcDir, Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer src.Close()
	if err := src.CreateTable("graph", TableOptions{
		TabletOptions: tablet.Options{FileFormat: tablet.FormatParquet},
	}); err != nil {
		t.Fatal(err)
	}
	m, _ := cclient.NewMutation([]byte("row"))
	m.Put([]byte("cf"), []byte("cq"), nil, 7, []byte("value"))
	if err := src.Write("graph", []*cclient.Mutation{m}); err != nil {
		t.Fatal(err)
	}

	backend := memory.New()
	manifest, err := src.ExportRFiles(ctx, "graph", backend, RFileExportOptions{
		DestinationRoot:      dstDir,
		StampVisibilityLabel: "tenantA",
	})
	if err != nil {
		t.Fatal(err)
	}
	if manifest.FileFormat != tablet.FormatParquet || len(manifest.RFiles) != 1 {
		t.Fatalf("unexpected manifest: %+v", manifest)
	}
	if manifest.Version != RFileExportManifestVersion {
		t.Fatalf("manifest version = %d, want %d", manifest.Version, RFileExportManifestVersion)
	}
	manifest.Version = RFileExportManifestLegacyVersion
	if err := VerifyRFileExport(ctx, backend, manifest); err == nil || !strings.Contains(err.Error(), "authoritative RFile") {
		t.Fatalf("VerifyRFileExport legacy parquet error = %v", err)
	}
	manifest.Version = RFileExportManifestVersion
	if manifest.RFiles[0].Format != string(tablet.FormatParquet) || manifest.RFiles[0].BCFileVersion != "" {
		t.Fatalf("unexpected parquet file metadata: %+v", manifest.RFiles[0])
	}

	dst, err := Open(dstDir, Options{Backend: backend})
	if err != nil {
		t.Fatal(err)
	}
	defer dst.Close()
	if err := dst.ImportRFileManifest(ctx, manifest); err != nil {
		t.Fatal(err)
	}
	got := scanAll(t, dst, "graph")
	if fmt.Sprint(got) != "[row|cf|cq|tenantA|7|value]" {
		t.Fatalf("imported parquet cells = %v", got)
	}
}

func TestMixedFormatExportManifestUsesActualFileFormats(t *testing.T) {
	ctx := context.Background()
	src, err := Open(t.TempDir(), Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer src.Close()
	if err := src.CreateTable("graph", TableOptions{}); err != nil {
		t.Fatal(err)
	}
	write := func(row string) {
		m, _ := cclient.NewMutation([]byte(row))
		m.Put([]byte("cf"), []byte("cq"), nil, 1, []byte(row))
		if err := src.Write("graph", []*cclient.Mutation{m}); err != nil {
			t.Fatal(err)
		}
		if err := src.Flush("graph"); err != nil {
			t.Fatal(err)
		}
	}
	write("rfile")
	if err := src.SetTableFileFormat("graph", tablet.FormatParquet); err != nil {
		t.Fatal(err)
	}
	write("parquet")
	policy, err := src.TableStoragePolicy("graph")
	if err != nil {
		t.Fatal(err)
	}
	if policy.WriteFormat != StorageFormatParquet || !policy.Mixed ||
		fmt.Sprint(policy.ReadFormats) != "[parquet rfile]" || policy.Role != "authoritative" {
		t.Fatalf("mixed storage policy = %+v", policy)
	}

	dst := memory.New()
	manifest, err := src.ExportRFiles(ctx, "graph", dst, RFileExportOptions{
		DestinationRoot: t.TempDir(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if manifest.FileFormat != tablet.FormatParquet ||
		manifest.RFileCompatibility != "mixed-rfile-parquet/shoal" ||
		manifest.Version != RFileExportManifestVersion {
		t.Fatalf("mixed manifest metadata = %+v", manifest)
	}
	formats := map[string]bool{}
	for _, file := range manifest.RFiles {
		formats[file.Format] = true
	}
	if !formats[string(tablet.FormatRFile)] || !formats[string(tablet.FormatParquet)] {
		t.Fatalf("mixed manifest file formats = %v", formats)
	}
}

func TestImportRFileManifestRejectsDerivedFiles(t *testing.T) {
	ctx := context.Background()
	src, err := Open(filepath.Join(t.TempDir(), "src"), Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer src.Close()
	if err := src.CreateTable("graph", TableOptions{}); err != nil {
		t.Fatal(err)
	}
	writeRow(t, src, "graph", "row", 0)

	backend := memory.New()
	manifest, err := src.ExportRFiles(ctx, "graph", backend, RFileExportOptions{DestinationRoot: "export"})
	if err != nil {
		t.Fatal(err)
	}
	manifest.Version = RFileExportManifestVersion
	manifest.RFiles[0].Role = ExportRoleDerived

	dst, err := Open(filepath.Join(t.TempDir(), "dst"), Options{Backend: backend})
	if err != nil {
		t.Fatal(err)
	}
	defer dst.Close()
	if err := dst.ImportRFileManifest(ctx, manifest); err == nil || !strings.Contains(err.Error(), "only authoritative") {
		t.Fatalf("ImportRFileManifest error = %v, want derived-role rejection", err)
	}
}

func TestImportRFileManifestRejectsSameCountDifferentSplits(t *testing.T) {
	ctx := context.Background()
	src, err := Open(filepath.Join(t.TempDir(), "src"), Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer src.Close()
	if err := src.CreateTable("graph", TableOptions{Splits: PrefixSplit("m")}); err != nil {
		t.Fatal(err)
	}
	writeRow(t, src, "graph", "row", 0)

	backend := memory.New()
	manifest, err := src.ExportRFiles(ctx, "graph", backend, RFileExportOptions{DestinationRoot: "export"})
	if err != nil {
		t.Fatal(err)
	}
	dst, err := Open(filepath.Join(t.TempDir(), "dst"), Options{Backend: backend})
	if err != nil {
		t.Fatal(err)
	}
	defer dst.Close()
	if err := dst.CreateTable("graph", TableOptions{Splits: PrefixSplit("n")}); err != nil {
		t.Fatal(err)
	}
	if err := dst.ImportRFileManifest(ctx, manifest); err == nil || !strings.Contains(err.Error(), "split 0") {
		t.Fatalf("ImportRFileManifest error = %v, want split mismatch", err)
	}
	for _, key := range backend.Keys() {
		if strings.Contains(filepath.Base(key), ".shoal-import-") {
			t.Fatalf("rejected import leaked staged object %q", key)
		}
	}
}

func TestVerifyImmutableExportRejectsInvalidParquet(t *testing.T) {
	data := []byte("not a parquet file")
	path := filepath.Join(t.TempDir(), "F0001.parquet")
	backend := memory.New()
	backend.Put(path, data)
	manifest := exportManifestForBytes(path, data)
	manifest.FileFormat = tablet.FormatParquet
	manifest.RFileCompatibility = "parquet/shoal"
	manifest.RFiles[0].Format = ExportFormatParquet

	err := verifyImmutableExport(
		context.Background(), backend, manifest, newVerificationSnapshot,
	)
	if !errors.Is(err, storage.ErrImmutablePolicy) ||
		!strings.Contains(err.Error(), "structurally valid Parquet") {
		t.Fatalf("verifyImmutableExport(invalid Parquet) = %v, want structural rejection", err)
	}
}

func TestStageVerifiedImportFilesRequiresRemovalSupport(t *testing.T) {
	data := validRFileBytes(t)
	path := filepath.Join(t.TempDir(), "F0001.rf")
	backend := memory.New()
	backend.Put(path, data)
	withoutRemove := struct{ storage.WritableBackend }{WritableBackend: backend}

	_, err := stageVerifiedImportFiles(
		context.Background(), withoutRemove, exportManifestForBytes(path, data).RFiles,
	)
	if !errors.Is(err, storage.ErrRemoverUnsupported) {
		t.Fatalf("stageVerifiedImportFiles() = %v, want ErrRemoverUnsupported", err)
	}
	for _, key := range backend.Keys() {
		if strings.Contains(filepath.Base(key), ".shoal-import-") {
			t.Fatalf("unsupported backend created staged object %q", key)
		}
	}
}

func TestVerifyRFileExportRejectsParquet(t *testing.T) {
	ctx := context.Background()
	backend := memory.New()
	manifest := exportManifestForBytes(
		filepath.Join(t.TempDir(), "F0001.parquet"),
		[]byte("parquet bytes are not accepted by RFile import"),
	)

	err := VerifyRFileExport(ctx, backend, manifest)
	if !errors.Is(err, storage.ErrImmutablePolicy) {
		t.Fatalf("VerifyRFileExport(Parquet) = %v, want ErrImmutablePolicy", err)
	}
	if errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("VerifyRFileExport(Parquet) reached backend open: %v", err)
	}
}

func TestVerifyRFileExportRejectsForgedValidFooter(t *testing.T) {
	data := forgedBCFileWithoutRFileIndex(t)
	path := filepath.Join(t.TempDir(), "F0001.rf")
	backend := memory.New()
	backend.Put(path, data)

	err := VerifyRFileExport(context.Background(), backend, exportManifestForBytes(path, data))
	if !errors.Is(err, storage.ErrImmutablePolicy) {
		t.Fatalf("VerifyRFileExport(forged BCFile) = %v, want ErrImmutablePolicy", err)
	}
	if !errors.Is(err, bcfile.ErrNoSuchMetaBlock) {
		t.Fatalf("VerifyRFileExport(forged BCFile) = %v, want ErrNoSuchMetaBlock", err)
	}
}

func TestVerifyRFileExportScansEveryLocalityGroup(t *testing.T) {
	data := validMultiGroupRFileBytes(t)
	valueOffset := bytes.Index(data, []byte("named-value"))
	if valueOffset < 4 {
		t.Fatal("named locality-group value not found in uncompressed fixture")
	}
	data = append([]byte(nil), data...)
	for i := valueOffset - 4; i < valueOffset; i++ {
		data[i] = 0xff
	}
	path := filepath.Join(t.TempDir(), "F0001.rf")
	backend := memory.New()
	backend.Put(path, data)

	if err := VerifyRFileExport(
		context.Background(), backend, exportManifestForBytes(path, data),
	); err == nil || !strings.Contains(err.Error(), "locality group 1") {
		t.Fatalf("VerifyRFileExport(corrupt named locality group) error = %v", err)
	}
}

func TestImportRFileManifestRejectsNamedLocalityGroupData(t *testing.T) {
	data := validMultiGroupRFileBytes(t)
	path := filepath.Join(t.TempDir(), "F0001.rf")
	backend := memory.New()
	backend.Put(path, data)
	manifest := exportManifestForBytes(path, data)

	if err := VerifyRFileExport(context.Background(), backend, manifest); err != nil {
		t.Fatalf("VerifyRFileExport(valid named locality group): %v", err)
	}
	dst, err := Open(filepath.Join(t.TempDir(), "dst"), Options{Backend: backend})
	if err != nil {
		t.Fatal(err)
	}
	defer dst.Close()
	err = dst.ImportRFileManifest(context.Background(), manifest)
	if !errors.Is(err, storage.ErrImmutablePolicy) ||
		!errors.Is(err, errNamedLocalityGroupUnsupported) {
		t.Fatalf("ImportRFileManifest error = %v, want named locality-group policy rejection", err)
	}
	for _, key := range backend.Keys() {
		if strings.Contains(filepath.Base(key), ".shoal-import-") {
			t.Fatalf("rejected locality-group import staged object %q", key)
		}
	}
}

func TestVerifyRFileExportRejectsRowsOutsideManifestTablet(t *testing.T) {
	data := validRFileBytes(t)
	tests := []struct {
		name        string
		split       string
		tabletIndex int
	}{
		{name: "at or above end row", split: "m", tabletIndex: 0},
		{name: "below start row", split: "s", tabletIndex: 1},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "F0001.rf")
			backend := memory.New()
			backend.Put(path, data)
			manifest := exportManifestForBytes(path, data)
			manifest.Tablets = []RFileExportTablet{
				{Index: 0, EndRow: &test.split},
				{Index: 1, StartRow: &test.split},
			}
			manifest.RFiles[0].TabletIndex = test.tabletIndex

			err := VerifyRFileExport(context.Background(), backend, manifest)
			if !errors.Is(err, storage.ErrImmutablePolicy) ||
				!strings.Contains(err.Error(), "outside manifest tablet") {
				t.Fatalf("VerifyRFileExport(out-of-tablet row) = %v, want structural policy error", err)
			}
		})
	}
}

func TestVerifyRFileExportAcceptsRowWithinManifestTablet(t *testing.T) {
	data := validRFileBytes(t)
	path := filepath.Join(t.TempDir(), "F0001.rf")
	backend := memory.New()
	backend.Put(path, data)
	manifest := exportManifestForBytes(path, data)
	start, end := "q", "s"
	manifest.Tablets = []RFileExportTablet{
		{Index: 0, EndRow: &start},
		{Index: 1, StartRow: &start, EndRow: &end},
		{Index: 2, StartRow: &end},
	}
	manifest.RFiles[0].TabletIndex = 1

	if err := VerifyRFileExport(context.Background(), backend, manifest); err != nil {
		t.Fatalf("VerifyRFileExport(in-tablet row) = %v, want success", err)
	}
}

func TestVerifyRFileExportAcceptsEmptyRFile(t *testing.T) {
	var data bytes.Buffer
	writer, err := rfile.NewWriter(&data, rfile.WriterOptions{})
	if err != nil {
		t.Fatalf("NewWriter: %v", err)
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("Writer.Close: %v", err)
	}

	path := filepath.Join(t.TempDir(), "empty.rf")
	backend := memory.New()
	backend.Put(path, data.Bytes())
	if err := VerifyRFileExport(
		context.Background(), backend, exportManifestForBytes(path, data.Bytes()),
	); err != nil {
		t.Fatalf("VerifyRFileExport(empty RFile): %v", err)
	}
}

func TestVerifyRFileExportPinsOneObjectSnapshot(t *testing.T) {
	forged := forgedBCFileWithoutRFileIndex(t)
	valid := validRFileBytes(t)
	path := filepath.Join(t.TempDir(), "F0001.rf")
	backend := &replacementBackend{generations: [][]byte{forged, valid}}

	err := VerifyRFileExport(context.Background(), backend, exportManifestForBytes(path, forged))
	if !errors.Is(err, storage.ErrImmutablePolicy) ||
		!errors.Is(err, bcfile.ErrNoSuchMetaBlock) {
		t.Fatalf("VerifyRFileExport(replaced object) = %v, want structural policy error", err)
	}
	if got := backend.OpenCount(); got != 1 {
		t.Fatalf("backend opens = %d, want exactly 1 pinned snapshot", got)
	}
}

func TestStageVerifiedImportFilesRejectsPostVerificationReplacement(t *testing.T) {
	valid := validRFileBytes(t)
	replacement := forgedBCFileWithoutRFileIndex(t)
	path := filepath.Join(t.TempDir(), "F0001.rf")
	manifest := exportManifestForBytes(path, valid)
	backend := memory.New()
	backend.Put(path, valid)

	if err := verifyImmutableExport(
		context.Background(), backend, manifest, newVerificationSnapshot,
	); err != nil {
		t.Fatalf("verifyImmutableExport: %v", err)
	}
	backend.Put(path, replacement)

	if _, err := stageVerifiedImportFiles(
		context.Background(), backend, manifest.RFiles,
	); err == nil || !strings.Contains(err.Error(), "changed after verification") {
		t.Fatalf("stageVerifiedImportFiles replacement error = %v", err)
	}
}

func TestStageVerifiedImportFilesRegistersExactStagedObject(t *testing.T) {
	valid := validRFileBytes(t)
	path := filepath.Join(t.TempDir(), "F0001.rf")
	manifest := exportManifestForBytes(path, valid)
	backend := memory.New()
	backend.Put(path, valid)

	staged, err := stageVerifiedImportFiles(
		context.Background(), backend, manifest.RFiles,
	)
	if err != nil {
		t.Fatalf("stageVerifiedImportFiles: %v", err)
	}
	if len(staged) != 1 || staged[0].DestinationPath == path {
		t.Fatalf("staged files = %+v, want one distinct staged path", staged)
	}

	backend.Put(path, forgedBCFileWithoutRFileIndex(t))
	size, sum, err := hashObject(context.Background(), backend, staged[0].DestinationPath)
	if err != nil {
		t.Fatalf("hash staged object: %v", err)
	}
	if size != manifest.RFiles[0].Size || sum != manifest.RFiles[0].SHA256 {
		t.Fatalf("staged object changed with source: size=%d sha256=%s", size, sum)
	}
}

func TestStageVerifiedImportFilesDoesNotReplaceRegisteredObjectOnMismatch(t *testing.T) {
	valid := validRFileBytes(t)
	path := filepath.Join(t.TempDir(), "F0001.rf")
	manifest := exportManifestForBytes(path, valid)
	backend := memory.New()
	backend.Put(path, valid)

	first, err := stageVerifiedImportFiles(
		context.Background(), backend, manifest.RFiles,
	)
	if err != nil {
		t.Fatalf("first stageVerifiedImportFiles: %v", err)
	}
	registeredPath := first[0].DestinationPath

	backend.Put(path, forgedBCFileWithoutRFileIndex(t))
	if _, err := stageVerifiedImportFiles(
		context.Background(), backend, manifest.RFiles,
	); err == nil || !strings.Contains(err.Error(), "changed after verification") {
		t.Fatalf("second stageVerifiedImportFiles error = %v, want mismatch", err)
	}

	size, sum, err := hashObject(context.Background(), backend, registeredPath)
	if err != nil {
		t.Fatalf("hash registered object: %v", err)
	}
	if size != manifest.RFiles[0].Size || sum != manifest.RFiles[0].SHA256 {
		t.Fatalf("registered object was replaced: size=%d sha256=%s", size, sum)
	}
}

func TestVerifyRFileExportRejectsChangingReadGeneration(t *testing.T) {
	valueSize := 3 * verificationSnapshotChunkSize
	first := validRFileBytesWithValue(t, bytes.Repeat([]byte{0x35}, valueSize))
	second := validRFileBytesWithValue(t, bytes.Repeat([]byte{0xa7}, valueSize))
	if len(first) <= verificationSnapshotChunkSize {
		t.Fatalf("first generation size = %d, want more than one snapshot chunk", len(first))
	}
	if len(first) != len(second) {
		t.Fatalf("generation sizes differ: first=%d second=%d", len(first), len(second))
	}

	path := filepath.Join(t.TempDir(), "F0001.rf")
	file := &changingGenerationFile{first: first, second: second}
	backend := &singleFileBackend{file: file}

	err := VerifyRFileExport(context.Background(), backend, exportManifestForBytes(path, first))
	if err == nil || !strings.Contains(err.Error(), "sha256") {
		t.Fatalf("VerifyRFileExport(changing generation) = %v, want hash rejection", err)
	}
	if errors.Is(err, storage.ErrImmutablePolicy) {
		t.Fatalf("VerifyRFileExport(changing generation) misclassified hash rejection: %v", err)
	}
	if reads, switched := file.state(); reads < 2 || !switched {
		t.Fatalf("backend range reads = %d, switched=%t; want a mid-copy generation change",
			reads, switched)
	}
}

func TestVerifyRFileExportParsesOnlyLocalSnapshot(t *testing.T) {
	valid := validRFileBytes(t)
	path := filepath.Join(t.TempDir(), "F0001.rf")
	file := &snapshotFile{
		data:      valid,
		failAfter: 1,
		failErr:   errors.New("backend accessed after snapshot copy"),
	}
	backend := &singleFileBackend{file: file}

	if err := VerifyRFileExport(context.Background(), backend, exportManifestForBytes(path, valid)); err != nil {
		t.Fatalf("VerifyRFileExport() = %v, want local-snapshot validation", err)
	}
	if got := file.ReadCount(); got != 1 {
		t.Fatalf("backend ReadAt calls = %d, want one materialization read", got)
	}
}

func TestVerifyRFileExportPreservesOperationalErrors(t *testing.T) {
	valid := validRFileBytes(t)
	path := filepath.Join(t.TempDir(), "F0001.rf")
	manifest := exportManifestForBytes(path, valid)

	t.Run("backend read error during materialization", func(t *testing.T) {
		readErr := errors.New("injected backend read failure")
		large := validRFileBytesWithValue(t,
			bytes.Repeat([]byte{0x5c}, 3*verificationSnapshotChunkSize))
		backend := &singleFileBackend{file: &snapshotFile{
			data:      large,
			failAfter: 1,
			failErr:   readErr,
		}}
		err := VerifyRFileExport(context.Background(), backend,
			exportManifestForBytes(path, large))
		if !errors.Is(err, readErr) {
			t.Fatalf("VerifyRFileExport() = %v, want injected read error", err)
		}
		if errors.Is(err, storage.ErrImmutablePolicy) {
			t.Fatalf("VerifyRFileExport() misclassified I/O as policy error: %v", err)
		}
	})

	t.Run("cancellation during materialization", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		backend := &singleFileBackend{file: &snapshotFile{
			data:        valid,
			afterReadAt: cancel,
		}}
		err := VerifyRFileExport(ctx, backend, manifest)
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("VerifyRFileExport() = %v, want context.Canceled", err)
		}
		if errors.Is(err, storage.ErrImmutablePolicy) {
			t.Fatalf("VerifyRFileExport() misclassified cancellation as policy error: %v", err)
		}
	})

	t.Run("backend close", func(t *testing.T) {
		closeErr := errors.New("injected backend close failure")
		backend := &singleFileBackend{file: &snapshotFile{
			data:     valid,
			closeErr: closeErr,
		}}
		err := VerifyRFileExport(context.Background(), backend, manifest)
		if !errors.Is(err, closeErr) {
			t.Fatalf("VerifyRFileExport() = %v, want injected close error", err)
		}
		if errors.Is(err, storage.ErrImmutablePolicy) {
			t.Fatalf("VerifyRFileExport() misclassified close failure as policy error: %v", err)
		}
	})
}

func TestVerifyRFileExportPreservesSnapshotErrors(t *testing.T) {
	valid := validRFileBytes(t)
	path := filepath.Join(t.TempDir(), "F0001.rf")
	manifest := exportManifestForBytes(path, valid)
	backend := &singleFileBackend{file: &snapshotFile{data: valid}}

	tests := []struct {
		name        string
		configure   func(*injectedVerificationSnapshot)
		createErr   error
		cleanupErr  error
		wantCleanup bool
	}{
		{
			name:      "create",
			createErr: errors.New("injected temp create failure"),
		},
		{
			name: "write",
			configure: func(s *injectedVerificationSnapshot) {
				s.writeErr = errors.New("injected disk write failure")
			},
			wantCleanup: true,
		},
		{
			name: "sync",
			configure: func(s *injectedVerificationSnapshot) {
				s.syncErr = errors.New("injected disk sync failure")
			},
			wantCleanup: true,
		},
		{
			name: "read",
			configure: func(s *injectedVerificationSnapshot) {
				s.readErr = errors.New("injected disk read failure")
			},
			wantCleanup: true,
		},
		{
			name:        "cleanup",
			cleanupErr:  errors.New("injected snapshot cleanup failure"),
			wantCleanup: true,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			snapshot := &injectedVerificationSnapshot{}
			if tc.configure != nil {
				tc.configure(snapshot)
			}
			var preserved error
			if tc.createErr != nil {
				preserved = tc.createErr
			} else if snapshot.writeErr != nil {
				preserved = snapshot.writeErr
			} else if snapshot.syncErr != nil {
				preserved = snapshot.syncErr
			} else if snapshot.readErr != nil {
				preserved = snapshot.readErr
			} else {
				preserved = tc.cleanupErr
			}
			cleanupCalled := false
			factory := func() (verificationSnapshot, func() error, error) {
				if tc.createErr != nil {
					return nil, nil, tc.createErr
				}
				return snapshot, func() error {
					cleanupCalled = true
					return tc.cleanupErr
				}, nil
			}

			err := verifyRFileExport(context.Background(), backend, manifest, factory)
			if !errors.Is(err, preserved) {
				t.Fatalf("verifyRFileExport() = %v, want %v", err, preserved)
			}
			if errors.Is(err, storage.ErrImmutablePolicy) {
				t.Fatalf("verifyRFileExport() misclassified snapshot error as policy error: %v", err)
			}
			if cleanupCalled != tc.wantCleanup {
				t.Fatalf("cleanup called = %t, want %t", cleanupCalled, tc.wantCleanup)
			}
		})
	}
}

func TestVerificationSnapshotCleanup(t *testing.T) {
	snapshot, cleanup, err := newVerificationSnapshot()
	if err != nil {
		t.Fatalf("newVerificationSnapshot: %v", err)
	}
	f, ok := snapshot.(*os.File)
	if !ok {
		t.Fatalf("snapshot type = %T, want *os.File", snapshot)
	}
	path := f.Name()
	if err := cleanup(); err != nil {
		t.Fatalf("cleanup: %v", err)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("snapshot still exists after cleanup: %v", err)
	}
}

func exportManifestForBytes(path string, data []byte) *RFileExportManifest {
	sum := sha256.Sum256(data)
	return &RFileExportManifest{
		Version:     RFileExportManifestVersion,
		SourceTable: "verification-test",
		RFiles: []RFileExportFile{{
			DestinationPath: path,
			Size:            int64(len(data)),
			SHA256:          fmt.Sprintf("%x", sum),
		}},
	}
}

func validRFileBytes(t *testing.T) []byte {
	return validRFileBytesWithValue(t, []byte("value"))
}

func validRFileBytesWithValue(t *testing.T, value []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	writer, err := rfile.NewWriter(&buf, rfile.WriterOptions{})
	if err != nil {
		t.Fatalf("NewWriter: %v", err)
	}
	if err := writer.Append(&wire.Key{
		Row:             []byte("row"),
		ColumnFamily:    []byte("cf"),
		ColumnQualifier: []byte("cq"),
		Timestamp:       1,
	}, value); err != nil {
		t.Fatalf("Append: %v", err)
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	return buf.Bytes()
}

func validMultiGroupRFileBytes(t *testing.T) []byte {
	t.Helper()
	var buf bytes.Buffer
	writer, err := rfile.NewWriter(&buf, rfile.WriterOptions{})
	if err != nil {
		t.Fatalf("NewWriter: %v", err)
	}
	if err := writer.Append(&wire.Key{
		Row:          []byte("row"),
		ColumnFamily: []byte("default"),
		Timestamp:    1,
	}, []byte("default-value")); err != nil {
		t.Fatalf("append default locality group: %v", err)
	}
	if err := writer.AddLocalityGroup("named"); err != nil {
		t.Fatalf("AddLocalityGroup: %v", err)
	}
	if err := writer.Append(&wire.Key{
		Row:          []byte("row"),
		ColumnFamily: []byte("named"),
		Timestamp:    1,
	}, []byte("named-value")); err != nil {
		t.Fatalf("append named locality group: %v", err)
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	return buf.Bytes()
}

func forgedBCFileWithoutRFileIndex(t *testing.T) []byte {
	t.Helper()
	var buf bytes.Buffer
	writer := bcfile.NewWriter(&buf, bcfile.CodecNone)
	if err := writer.Close(); err != nil {
		t.Fatalf("close forged BCFile: %v", err)
	}
	if _, err := bcfile.ReadFooter(bytes.NewReader(buf.Bytes()), int64(buf.Len())); err != nil {
		t.Fatalf("forged fixture footer is invalid: %v", err)
	}
	return buf.Bytes()
}

type replacementBackend struct {
	mu          sync.Mutex
	generations [][]byte
	opens       int
}

func (b *replacementBackend) Open(_ context.Context, _ string) (storage.File, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	index := b.opens
	if index >= len(b.generations) {
		index = len(b.generations) - 1
	}
	b.opens++
	return &snapshotFile{data: bytes.Clone(b.generations[index])}, nil
}

func (b *replacementBackend) OpenCount() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.opens
}

type singleFileBackend struct {
	file storage.File
}

func (b *singleFileBackend) Open(_ context.Context, _ string) (storage.File, error) {
	return b.file, nil
}

type snapshotFile struct {
	data []byte

	mu          sync.Mutex
	reads       int
	failAfter   int
	failErr     error
	closeErr    error
	afterReadAt func()
}

func (f *snapshotFile) ReadAt(p []byte, off int64) (int, error) {
	f.mu.Lock()
	f.reads++
	read := f.reads
	failAfter := f.failAfter
	failErr := f.failErr
	afterReadAt := f.afterReadAt
	f.afterReadAt = nil
	f.mu.Unlock()

	if failAfter > 0 && read > failAfter {
		return 0, failErr
	}
	if off >= int64(len(f.data)) {
		return 0, io.EOF
	}
	n := copy(p, f.data[off:])
	var err error
	if n != len(p) {
		err = io.EOF
	}
	if afterReadAt != nil {
		afterReadAt()
	}
	return n, err
}

func (f *snapshotFile) ReadCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.reads
}

func (f *snapshotFile) Close() error { return f.closeErr }
func (f *snapshotFile) Size() int64  { return int64(len(f.data)) }

type changingGenerationFile struct {
	first  []byte
	second []byte

	mu       sync.Mutex
	reads    int
	switched bool
}

func (f *changingGenerationFile) ReadAt(p []byte, off int64) (int, error) {
	f.mu.Lock()
	f.reads++
	data := f.first
	if f.reads > 1 {
		data = f.second
		f.switched = true
	}
	f.mu.Unlock()

	if off >= int64(len(data)) {
		return 0, io.EOF
	}
	n := copy(p, data[off:])
	if n != len(p) {
		return n, io.EOF
	}
	return n, nil
}

func (f *changingGenerationFile) state() (int, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.reads, f.switched
}

func (f *changingGenerationFile) Close() error { return nil }
func (f *changingGenerationFile) Size() int64  { return int64(len(f.first)) }

type injectedVerificationSnapshot struct {
	data []byte

	writeErr error
	syncErr  error
	readErr  error
}

func (s *injectedVerificationSnapshot) Write(p []byte) (int, error) {
	if s.writeErr != nil {
		return 0, s.writeErr
	}
	s.data = append(s.data, p...)
	return len(p), nil
}

func (s *injectedVerificationSnapshot) Sync() error {
	return s.syncErr
}

func (s *injectedVerificationSnapshot) ReadAt(p []byte, off int64) (int, error) {
	if s.readErr != nil {
		return 0, s.readErr
	}
	if off >= int64(len(s.data)) {
		return 0, io.EOF
	}
	n := copy(p, s.data[off:])
	if n != len(p) {
		return n, io.EOF
	}
	return n, nil
}

func scanAll(t *testing.T, eng *Engine, table string) []string {
	t.Helper()
	sc, err := eng.Scan(table, iterrt.InfiniteRange(), ScanOptions{})
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	defer sc.Close()
	var out []string
	for sc.Next() {
		k := sc.Key()
		out = append(out, fmt.Sprintf("%s|%s|%s|%s|%d|%s",
			k.Row, k.ColumnFamily, k.ColumnQualifier, k.ColumnVisibility, k.Timestamp, sc.Value()))
		if err := sc.Advance(); err != nil {
			t.Fatalf("Advance: %v", err)
		}
	}
	sort.Strings(out)
	return out
}

type exportFileBackend struct {
	file storage.File
}

func (b exportFileBackend) Open(context.Context, string) (storage.File, error) {
	return b.file, nil
}

type exportReadFuncFile struct {
	size int64
	read func([]byte, int64) (int, error)
}

func (f *exportReadFuncFile) ReadAt(p []byte, off int64) (int, error) {
	return f.read(p, off)
}

func (*exportReadFuncFile) Close() error { return nil }
func (f *exportReadFuncFile) Size() int64 {
	return f.size
}

type exportTrackingBackend struct {
	files           map[string][]byte
	lastWriter      *exportTrackingWriter
	shortWrite      int
	partialErrBytes int
	writeErr        error
}

func newExportTrackingBackend() *exportTrackingBackend {
	return &exportTrackingBackend{files: make(map[string][]byte)}
}

func (b *exportTrackingBackend) Open(context.Context, string) (storage.File, error) {
	return nil, storage.ErrNotFound
}

func (b *exportTrackingBackend) Create(_ context.Context, path string) (storage.Writer, error) {
	writer := &exportTrackingWriter{
		backend:         b,
		path:            path,
		shortWrite:      b.shortWrite,
		partialErrBytes: b.partialErrBytes,
		writeErr:        b.writeErr,
	}
	b.lastWriter = writer
	return writer, nil
}

type exportTrackingWriter struct {
	backend         *exportTrackingBackend
	path            string
	stage           strings.Builder
	stagePresent    bool
	abortCalls      int
	closed          bool
	shortWrite      int
	partialErrBytes int
	writeErr        error
}

func (w *exportTrackingWriter) Write(p []byte) (int, error) {
	w.stagePresent = true
	if w.writeErr != nil {
		if w.partialErrBytes > 0 && len(p) > w.partialErrBytes {
			n, _ := w.stage.WriteString(string(p[:w.partialErrBytes]))
			return n, w.writeErr
		}
		return 0, w.writeErr
	}
	if w.shortWrite > 0 && len(p) > w.shortWrite {
		n, _ := w.stage.WriteString(string(p[:w.shortWrite]))
		return n, nil
	}
	return w.stage.WriteString(string(p))
}

func (w *exportTrackingWriter) Close() error {
	if w.closed {
		return nil
	}
	w.closed = true
	w.stagePresent = false
	w.backend.files[w.path] = []byte(w.stage.String())
	return nil
}

func (w *exportTrackingWriter) Abort() error {
	w.abortCalls++
	w.stagePresent = false
	w.stage.Reset()
	return nil
}
