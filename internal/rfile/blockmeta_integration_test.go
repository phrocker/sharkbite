package rfile

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"sync/atomic"
	"testing"

	"github.com/phrocker/shoal-oss/internal/rfile/bcfile"
	"github.com/phrocker/shoal-oss/internal/rfile/bcfile/block"
	"github.com/phrocker/shoal-oss/internal/rfile/blockmeta"
	"github.com/phrocker/shoal-oss/internal/rfile/index"
)

// writeAugmentedRFile produces a small RFile with zone-map metadata so
// the integration tests can exercise the skip predicate end-to-end.
// Cells are spread across multiple blocks by setting BlockSize small.
//
// Returns the bytes + the per-block tsMin/tsMax we expect to see in
// the parsed BlockMeta — used to assert the writer aggregated correctly.
func writeAugmentedRFile(t *testing.T, n int, blockSize int) (bytes []byte, perBlockTsRanges [][2]int64) {
	t.Helper()
	zm := blockmeta.NewZoneMapBuilder()
	var buf bytesBuf
	w, err := NewWriter(&buf, WriterOptions{
		BlockSize:         blockSize,
		BlockMetaBuilders: []blockmeta.OverlayBuilder{zm},
	})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < n; i++ {
		k := &Key{
			Row:             []byte(fmt.Sprintf("row%05d", i)),
			ColumnFamily:    []byte("cf"),
			ColumnQualifier: []byte("cq"),
			Timestamp:       int64(i + 1),
		}
		v := []byte(fmt.Sprintf("v%05d", i))
		if err := w.Append(k, v); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.bs, nil // tsRanges computed from actual block layout below; not
	// reconstructed here because we'd need to predict BlockSize-driven splits.
}

type bytesBuf struct{ bs []byte }

func (b *bytesBuf) Write(p []byte) (int, error) {
	b.bs = append(b.bs, p...)
	return len(p), nil
}

func TestBlockMeta_WriterEmitsZoneMaps(t *testing.T) {
	bs, _ := writeAugmentedRFile(t, 100, 100) // many small blocks

	bc, err := bcfile.NewReader(byteReaderAt(bs), int64(len(bs)))
	if err != nil {
		t.Fatal(err)
	}
	r, err := Open(bc, block.Default())
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()

	bm := r.BlockMeta()
	if bm == nil {
		t.Fatal("BlockMeta is nil — writer didn't emit RFile.blockmeta")
	}
	if len(bm.BlockOverlays) == 0 {
		t.Fatal("BlockOverlays is empty")
	}

	// Each block should have a zone-map; tsMin/tsMax should be a
	// contiguous range and the union should cover [1, 100].
	var unionMin int64 = 1<<62 - 1
	var unionMax int64 = -1<<62 + 1
	for i, ovs := range bm.BlockOverlays {
		var zm *blockmeta.Overlay
		for j := range ovs {
			if ovs[j].Type == blockmeta.OverlayZoneMap {
				zm = &ovs[j]
				break
			}
		}
		if zm == nil {
			t.Errorf("block %d has no zone-map overlay", i)
			continue
		}
		decoded, err := blockmeta.DecodeZoneMap(zm.Payload)
		if err != nil {
			t.Errorf("block %d zone-map decode: %v", i, err)
			continue
		}
		if decoded.TsMin > decoded.TsMax {
			t.Errorf("block %d ZoneMap{%d, %d}: tsMin > tsMax", i, decoded.TsMin, decoded.TsMax)
		}
		if decoded.TsMin < unionMin {
			unionMin = decoded.TsMin
		}
		if decoded.TsMax > unionMax {
			unionMax = decoded.TsMax
		}
	}
	if unionMin != 1 || unionMax != 100 {
		t.Errorf("union of all zone maps = [%d, %d]; want [1, 100]", unionMin, unionMax)
	}
}

func TestBlockMeta_BackwardsCompatNoMetaBlock(t *testing.T) {
	// Writer with NO BlockMetaBuilders should produce an RFile that
	// parses cleanly + has BlockMeta() == nil.
	var buf bytesBuf
	w, _ := NewWriter(&buf, WriterOptions{})
	for i := 0; i < 10; i++ {
		k := &Key{
			Row:             []byte(fmt.Sprintf("r%02d", i)),
			ColumnFamily:    []byte("cf"),
			ColumnQualifier: []byte("cq"),
			Timestamp:       int64(i + 1),
		}
		_ = w.Append(k, []byte("v"))
	}
	_ = w.Close()

	bc, _ := bcfile.NewReader(byteReaderAt(buf.bs), int64(len(buf.bs)))
	r, err := Open(bc, block.Default())
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()

	if r.BlockMeta() != nil {
		t.Errorf("BlockMeta should be nil for un-augmented RFile")
	}
	// Walk all cells — should produce 10.
	count := 0
	for {
		_, _, err := r.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		count++
	}
	if count != 10 {
		t.Errorf("walked %d cells; want 10", count)
	}
}

func TestBlockMeta_SkipPredicateBypassesBlocks(t *testing.T) {
	// 200 cells with timestamps 1..200, small blocks (≈8-10 cells each).
	// Skip predicate based on a "max ts" threshold: skip blocks where
	// tsMin > threshold. For threshold=80, blocks containing only
	// cells with ts > 80 should be skipped.
	bs, _ := writeAugmentedRFile(t, 200, 100)

	bc, _ := bcfile.NewReader(byteReaderAt(bs), int64(len(bs)))
	r, err := Open(bc, block.Default())
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()

	bm := r.BlockMeta()
	if bm == nil {
		t.Fatal("BlockMeta is nil")
	}

	// Track how many blocks are skipped vs visited. The predicate is
	// the only place we observe block-level skip.
	var visited atomic.Int64
	var skipped atomic.Int64
	r.SetSkipPredicate(func(leafIdx int, _ *index.IndexEntry, bm *blockmeta.BlockMeta) bool {
		ov := bm.FindOverlay(leafIdx, blockmeta.OverlayZoneMap)
		if ov == nil {
			visited.Add(1)
			return false
		}
		zm, err := blockmeta.DecodeZoneMap(ov.Payload)
		if err != nil {
			visited.Add(1)
			return false
		}
		// Skip blocks where the YOUNGEST cell is older than threshold —
		// i.e. tsMin > 80 means everything in the block is > 80.
		// (Reading "give me cells with ts <= 80".)
		if zm.TsMin > 80 {
			skipped.Add(1)
			return true
		}
		visited.Add(1)
		return false
	})

	count := 0
	var maxTs int64
	for {
		k, _, err := r.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if k.Timestamp > maxTs {
			maxTs = k.Timestamp
		}
		count++
	}

	if skipped.Load() == 0 {
		t.Errorf("no blocks skipped; predicate should have skipped some")
	}
	t.Logf("visited=%d skipped=%d cells_returned=%d maxTs=%d",
		visited.Load(), skipped.Load(), count, maxTs)

	// Sanity: every cell with ts <= 80 should still come through; the
	// total count should be at least 80 (it's >= 80 because some blocks
	// span across the threshold and we don't filter at the cell level
	// here — block skip is conservative).
	if count < 80 {
		t.Errorf("returned only %d cells; expected ≥ 80 (every ts<=80 must survive)", count)
	}
}

// byteReaderAt is a small adapter from []byte to io.ReaderAt for tests.
type byteReaderAtType []byte

func (b byteReaderAtType) ReadAt(p []byte, off int64) (int, error) {
	if off >= int64(len(b)) {
		return 0, io.EOF
	}
	n := copy(p, b[off:])
	if n < len(p) {
		return n, io.EOF
	}
	return n, nil
}

func byteReaderAt(b []byte) byteReaderAtType { return byteReaderAtType(b) }

// silence unused-import in case test file omits some helpers
var _ = bytes.NewReader
