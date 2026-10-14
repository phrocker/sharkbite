// Ground-truth tests against bytes captured from a real Accumulo cluster
// via `shoal-bootstrap -dump-root-scan -dump-out testdata/<name>.json`.
//
// These tests verify our decoders + AggregateRows survive whatever the
// server actually emits — they're the antidote to "synthetic-only" tests
// drifting from wire reality. They auto-skip when no capture is present,
// so the unit-test suite still runs in a fresh checkout.
//
// To regenerate after a wire-format change:
//
//	cd platform/shoal
//	go run ./cmd/shoal-bootstrap -zk ... -password ... \
//	    -dump-root-scan -dump-out internal/metadata/testdata/root_scan_capture.json
//
// Capture files are committed: they're non-secret (no auth bytes, just
// public table/file metadata) and they lock the wire format in CI.
package metadata

import (
	"encoding/hex"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"testing"

	"github.com/phrocker/shoal-oss/internal/thrift/gen/data"
)

// captureKV mirrors the JSON shape written by shoal-bootstrap (see
// cmd/shoal-bootstrap/main.go rootScanCaptureKV). Kept private to the
// test package — the dump tool is the only writer; tests are the only
// readers.
type captureKV struct {
	RowHex   string `json:"rowHex"`
	CFHex    string `json:"cfHex"`
	CQHex    string `json:"cqHex"`
	CVHex    string `json:"cvHex"`
	TS       int64  `json:"ts"`
	ValueHex string `json:"valueHex"`
}

type capture struct {
	Host       string      `json:"host"`
	More       bool        `json:"more"`
	CapturedAt string      `json:"capturedAt"`
	KVs        []captureKV `json:"kvs"`
}

func loadCapture(t *testing.T, path string) (*capture, []*data.TKeyValue, bool) {
	t.Helper()
	buf, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			t.Skipf("no capture at %s — run shoal-bootstrap -dump-root-scan -dump-out %s", path, path)
		}
		t.Fatalf("read %s: %v", path, err)
	}
	var c capture
	if err := json.Unmarshal(buf, &c); err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	if len(c.KVs) == 0 {
		t.Fatalf("%s contains zero KVs — capture is empty", path)
	}
	kvs := make([]*data.TKeyValue, 0, len(c.KVs))
	for i, raw := range c.KVs {
		row, err := hex.DecodeString(raw.RowHex)
		if err != nil {
			t.Fatalf("KV[%d] rowHex: %v", i, err)
		}
		cf, err := hex.DecodeString(raw.CFHex)
		if err != nil {
			t.Fatalf("KV[%d] cfHex: %v", i, err)
		}
		cq, err := hex.DecodeString(raw.CQHex)
		if err != nil {
			t.Fatalf("KV[%d] cqHex: %v", i, err)
		}
		cv, err := hex.DecodeString(raw.CVHex)
		if err != nil {
			t.Fatalf("KV[%d] cvHex: %v", i, err)
		}
		val, err := hex.DecodeString(raw.ValueHex)
		if err != nil {
			t.Fatalf("KV[%d] valueHex: %v", i, err)
		}
		kvs = append(kvs, &data.TKeyValue{
			Key: &data.TKey{
				Row:           row,
				ColFamily:     cf,
				ColQualifier:  cq,
				ColVisibility: cv,
				Timestamp:     raw.TS,
			},
			Value: val,
		})
	}
	return &c, kvs, true
}

// TestGroundTruth_RootScan_Aggregate locks AggregateRows + decoders against
// real wire bytes. Every tablet row, file qualifier, file value, prev-row
// value, and lock id must parse cleanly. Skip if no capture present.
func TestGroundTruth_RootScan_Aggregate(t *testing.T) {
	path := filepath.Join("testdata", "root_scan_capture.json")
	cap, kvs, _ := loadCapture(t, path)

	tablets, err := AggregateRows(kvs)
	if err != nil {
		t.Fatalf("AggregateRows: %v", err)
	}
	if len(tablets) == 0 {
		t.Fatalf("AggregateRows yielded zero tablets from %d KVs (capture host=%s)", len(kvs), cap.Host)
	}

	// Root tablet contains METADATA-table tablets. Sanity: every emitted
	// tablet must claim TableID == "!0" — anything else means our row
	// decoder split the bytes wrong.
	for i, tb := range tablets {
		if tb.TableID != MetadataTableID {
			t.Errorf("tablet %d: TableID=%q, want %q (DecodeTabletRow regression?)",
				i, tb.TableID, MetadataTableID)
		}
	}

	// Every file: qualifier was already parsed inside applyColumn — but
	// re-run parsers across raw KVs as well, to catch the case where a
	// drift only shows up before/after relative-key materialization.
	t.Run("RawDecoders", func(t *testing.T) {
		var prevCF, prevCQ []byte
		for i, kv := range kvs {
			cf := kv.Key.ColFamily
			cq := kv.Key.ColQualifier
			if len(cf) == 0 && i > 0 {
				cf = prevCF
			}
			if len(cq) == 0 && i > 0 {
				cq = prevCQ
			}
			switch string(cf) {
			case CFFile:
				if _, err := DecodeStoredTabletFile(cq); err != nil {
					t.Errorf("KV[%d] file: qualifier %q: %v", i, cq, err)
				}
				if _, _, _, err := DecodeDataFileValue(kv.Value); err != nil {
					t.Errorf("KV[%d] file: value %q: %v", i, kv.Value, err)
				}
			case CFCurrentLocation, CFFutureLocation:
				if _, err := DecodeLockID(cq); err != nil {
					t.Errorf("KV[%d] %s: lockID %q: %v", i, cf, cq, err)
				}
			case CFTabletSection:
				if string(cq) == CQPrevRow {
					// decodePrevRow doesn't return error; just ensure no panic
					_ = decodePrevRow(kv.Value)
				}
			}
			prevCF, prevCQ = cf, cq
		}
	})

	// Tablet ordering invariant: when sorted by row, prev[i+1] == end[i].
	t.Run("ContiguousRange", func(t *testing.T) {
		// Sort by EndRow ascending (nil last == +inf), to match scan order.
		ts := append([]TabletInfo(nil), tablets...)
		sort.SliceStable(ts, func(i, j int) bool {
			ei, ej := ts[i].EndRow, ts[j].EndRow
			if ei == nil {
				return false
			}
			if ej == nil {
				return true
			}
			return string(ei) < string(ej)
		})
		// First tablet must have PrevRow nil (start at -inf).
		if ts[0].PrevRow != nil {
			t.Errorf("first tablet PrevRow = %q, want nil", ts[0].PrevRow)
		}
		// Subsequent: PrevRow must equal the previous tablet's EndRow.
		for i := 1; i < len(ts); i++ {
			if string(ts[i].PrevRow) != string(ts[i-1].EndRow) {
				t.Errorf("tablet %d: PrevRow=%q, prev EndRow=%q (gap or split mismatch)",
					i, ts[i].PrevRow, ts[i-1].EndRow)
			}
		}
		// Last tablet must have EndRow nil (cover up to +inf).
		if ts[len(ts)-1].EndRow != nil {
			t.Errorf("last tablet EndRow = %q, want nil", ts[len(ts)-1].EndRow)
		}
	})
}
