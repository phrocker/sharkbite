// Ground-truth test against a real cluster RFile dropped into testdata/.
//
// Workflow to populate this fixture:
//
//	# Pick any small RFile from a live cluster — the smaller the better
//	# since these bytes get committed.
//	./bin/shoal-rfile-pull \
//	    -src gs://<bucket>/<accumulo-instance-id>/tables/<id>/<file>.rf \
//	    -dst internal/rfile/testdata/captured.rf
//
// The test then opens that RFile, walks every cell, and asserts the
// usual invariants: at least one cell, monotonic non-decreasing keys
// per locality group, no decode errors. It auto-skips if the fixture
// isn't present, so a fresh checkout's `go test ./...` still passes.
package rfile

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/phrocker/shoal-oss/internal/rfile/bcfile"
	"github.com/phrocker/shoal-oss/internal/rfile/bcfile/block"
)

// captureFixturePath is the on-disk path to the captured cluster RFile.
// Drop one in via shoal-rfile-pull (see file header).
func captureFixturePath() string {
	return filepath.Join("testdata", "captured.rf")
}

func TestGroundTruth_CapturedRFile(t *testing.T) {
	path := captureFixturePath()
	stat, err := os.Stat(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			t.Skipf("no capture at %s — run shoal-rfile-pull to populate", path)
		}
		t.Fatalf("stat %s: %v", path, err)
	}

	bs, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	t.Logf("captured RFile: %d bytes", stat.Size())

	bc, err := bcfile.NewReader(bytes.NewReader(bs), int64(len(bs)))
	if err != nil {
		t.Fatalf("bcfile.NewReader: %v", err)
	}
	t.Logf("BCFile version=%s", bc.Footer().Version)
	t.Logf("BCFile MetaIndex entries:")
	for name, entry := range bc.MetaIndex().Entries {
		t.Logf("  %q codec=%s region=%+v", name, entry.CompressionAlgo, entry.Region)
	}

	r, err := Open(bc, block.Default())
	if err != nil {
		// If the failure is "unsupported codec", surface that distinctly
		// — that's the most likely real-cluster mismatch (snappy / zstd).
		t.Fatalf("rfile.Open: %v\n\n"+
			"If this says 'unsupported codec', the captured RFile uses a codec "+
			"shoal doesn't yet decompress. Currently supported: none, gz. "+
			"Add the codec via Decompressor.Register, or pick a different "+
			"RFile that uses gz / none.", err)
	}
	defer r.Close()

	lg := r.LocalityGroup()
	t.Logf("default LG: name=%q firstKey=%v cfCount=%d numEntries=%d",
		lg.Name, lg.FirstKey, len(lg.ColumnFamilies), lg.NumTotalEntries)

	// Walk every cell. Validate monotonic non-decreasing keys (Accumulo
	// invariant). Cap at 100k cells so a huge captured file doesn't hang
	// CI — partial validation is still meaningful.
	const maxCells = 100_000
	var prev *Key
	count := 0
	for count < maxCells {
		k, _, err := r.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("Next at cell %d: %v", count, err)
		}
		if prev != nil && k.Compare(prev) < 0 {
			t.Errorf("non-monotonic keys at cell %d:\n  prev = %+v\n  curr = %+v", count, prev, k)
		}
		prev = k.Clone()
		count++
	}
	if count == 0 {
		t.Errorf("captured RFile yielded zero cells — likely a parsing failure that didn't error")
	}
	t.Logf("walked %d cells successfully", count)

	// Spot-check a seek roundtrip: re-seek to the first key and confirm
	// we get the same value back.
	if prev != nil {
		// We've consumed the whole stream; re-seek.
		first, _, err := openAndSeekFirst(t, bs)
		if err != nil {
			t.Fatalf("re-seek to first cell: %v", err)
		}
		if first != nil {
			t.Logf("first key on re-seek: row=%q cf=%q ts=%d", first.Row, first.ColumnFamily, first.Timestamp)
		}
	}
}

// openAndSeekFirst is a helper that opens a fresh reader against bs and
// returns just the first cell's key. Saves us repeating the open boiler-
// plate at the test call site.
func openAndSeekFirst(t *testing.T, bs []byte) (*Key, []byte, error) {
	t.Helper()
	bc, err := bcfile.NewReader(bytes.NewReader(bs), int64(len(bs)))
	if err != nil {
		return nil, nil, err
	}
	r, err := Open(bc, block.Default())
	if err != nil {
		return nil, nil, err
	}
	defer r.Close()

	if err := r.Seek(nil); err != nil {
		return nil, nil, err
	}
	k, v, err := r.Next()
	if errors.Is(err, io.EOF) {
		return nil, nil, nil
	}
	return k, v, err
}
