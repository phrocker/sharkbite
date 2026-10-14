package promotion

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"testing"

	"github.com/phrocker/shoal-oss/internal/engine"
	"github.com/phrocker/shoal-oss/internal/rfile"
	"github.com/phrocker/shoal-oss/internal/storage/memory"
)

func validRFileBytes(t *testing.T, value []byte) []byte {
	return validRFileBytesForRow(t, []byte("row"), value)
}

func validRFileBytesForRow(t *testing.T, row, value []byte) []byte {
	t.Helper()
	var data bytes.Buffer
	writer, err := rfile.NewWriter(&data, rfile.WriterOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if err := writer.Append(&rfile.Key{Row: row, Timestamp: 1}, value); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	return data.Bytes()
}

func populateManifestRFiles(t *testing.T, src *memory.Backend, manifest *engine.RFileExportManifest) {
	t.Helper()
	for i := range manifest.RFiles {
		tabletIndex := manifest.RFiles[i].TabletIndex
		row := representativeTabletRow(t, manifest, tabletIndex)
		data, file := testRFileForRow(
			t, tabletIndex, manifest.RFiles[i].DestinationPath, row, []byte{byte('a' + i)},
		)
		src.Put(file.DestinationPath, data)
		manifest.RFiles[i] = file
	}
}

func representativeTabletRow(
	t *testing.T,
	manifest *engine.RFileExportManifest,
	tabletIndex int,
) []byte {
	t.Helper()
	if len(manifest.Tablets) == 0 {
		return []byte("row")
	}
	for _, tablet := range manifest.Tablets {
		if tablet.Index != tabletIndex {
			continue
		}
		if tablet.StartRow != nil {
			return []byte(*tablet.StartRow)
		}
		if tablet.EndRow == nil {
			return []byte("row")
		}
		end := []byte(*tablet.EndRow)
		if len(end) == 0 {
			t.Fatalf("tablet %d has an empty exclusive end row", tabletIndex)
		}
		row := append([]byte(nil), end...)
		if row[len(row)-1] == 0 {
			return row[:len(row)-1]
		}
		row[len(row)-1]--
		return row
	}
	t.Fatalf("manifest has no tablet %d", tabletIndex)
	return nil
}

func testRFile(t *testing.T, tabletIndex int, path string, value []byte) ([]byte, engine.RFileExportFile) {
	return testRFileForRow(t, tabletIndex, path, []byte("row"), value)
}

func testRFileForRow(
	t *testing.T,
	tabletIndex int,
	path string,
	row, value []byte,
) ([]byte, engine.RFileExportFile) {
	t.Helper()
	data := validRFileBytesForRow(t, row, value)
	sum := sha256.Sum256(data)
	return data, engine.RFileExportFile{
		TabletIndex:     tabletIndex,
		DestinationPath: path,
		Size:            int64(len(data)),
		SHA256:          hex.EncodeToString(sum[:]),
		Format:          engine.ExportFormatRFile,
		Role:            engine.ExportRoleAuthoritative,
	}
}
