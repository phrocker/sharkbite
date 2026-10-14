package rfile

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"runtime"
	"testing"

	"github.com/phrocker/shoal-oss/internal/cache"
	"github.com/phrocker/shoal-oss/internal/rfile/bcfile"
	"github.com/phrocker/shoal-oss/internal/rfile/bcfile/block"
	"github.com/phrocker/shoal-oss/internal/rfile/blockmeta"
	"github.com/phrocker/shoal-oss/internal/rfile/index"
)

// BenchmarkWalkBigUserData walks a real ~50MB user-data RFile
// at /tmp/captured-big-user.rf if present (skips otherwise). Used to
// track perf as we add visibility-pushdown / zero-copy / cursor-based
// decoding. Reports cells/sec via b.ReportMetric.
//
// Not committed to testdata because of size — operator pulls it via
// shoal-rfile-pull. Skipping cleanly keeps `go test ./...` green for
// fresh checkouts.
func BenchmarkWalkBigUserData(b *testing.B) {
	const path = "/tmp/captured-big-user.rf"
	bs, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			b.Skipf("benchmark fixture not present at %s", path)
		}
		b.Fatal(err)
	}
	b.SetBytes(int64(len(bs)))
	b.ResetTimer()

	totalCells := int64(0)
	for i := 0; i < b.N; i++ {
		bc, err := bcfile.NewReader(bytes.NewReader(bs), int64(len(bs)))
		if err != nil {
			b.Fatal(err)
		}
		r, err := Open(bc, block.Default())
		if err != nil {
			b.Fatal(err)
		}
		count := int64(0)
		for {
			_, _, err := r.Next()
			if errors.Is(err, io.EOF) {
				break
			}
			if err != nil {
				b.Fatal(err)
			}
			count++
		}
		_ = r.Close()
		totalCells += count
	}
	if totalCells > 0 {
		b.ReportMetric(float64(totalCells)/b.Elapsed().Seconds(), "cells/sec")
		b.ReportMetric(float64(totalCells)/float64(b.N), "cells/op")
	}
}

// BenchmarkWalkCaptured walks the small committed fixture (if present).
// Microbenchmark — useful for catching low-level decode regressions, but
// dominated by Open() overhead at this size.
func BenchmarkWalkCaptured(b *testing.B) {
	bs, err := os.ReadFile("testdata/captured.rf")
	if err != nil {
		b.Skip("no captured.rf fixture")
	}
	b.SetBytes(int64(len(bs)))
	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		bc, _ := bcfile.NewReader(bytes.NewReader(bs), int64(len(bs)))
		r, err := Open(bc, block.Default())
		if err != nil {
			b.Fatal(err)
		}
		for {
			_, _, err := r.Next()
			if errors.Is(err, io.EOF) {
				break
			}
			if err != nil {
				b.Fatal(err)
			}
		}
		_ = r.Close()
	}
}

// BenchmarkWalkBigUserDataAcceptAll measures the cost of installing a
// trivially-accept filter — should be ~free (function-call overhead per
// cell) so we can compare against the filter-pushdown variants.
func BenchmarkWalkBigUserDataAcceptAll(b *testing.B) {
	const path = "/tmp/captured-big-user.rf"
	bs, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			b.Skipf("benchmark fixture not present at %s", path)
		}
		b.Fatal(err)
	}
	b.SetBytes(int64(len(bs)))
	b.ResetTimer()

	totalCells := int64(0)
	for i := 0; i < b.N; i++ {
		bc, _ := bcfile.NewReader(bytes.NewReader(bs), int64(len(bs)))
		r, _ := Open(bc, block.Default())
		r.SetFilter(func(_ *Key) bool { return true })
		count := int64(0)
		for {
			_, _, err := r.Next()
			if errors.Is(err, io.EOF) {
				break
			}
			if err != nil {
				b.Fatal(err)
			}
			count++
		}
		_ = r.Close()
		totalCells += count
	}
	if totalCells > 0 {
		b.ReportMetric(float64(totalCells)/b.Elapsed().Seconds(), "cells/sec")
	}
}

// BenchmarkWalkBigUserDataReject90 measures filter-pushdown speedup
// when 90% of cells are rejected at the relkey level (no Key clone +
// no value alloc + no value memcpy for rejected cells).
func BenchmarkWalkBigUserDataReject90(b *testing.B) {
	const path = "/tmp/captured-big-user.rf"
	bs, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			b.Skipf("benchmark fixture not present at %s", path)
		}
		b.Fatal(err)
	}
	b.SetBytes(int64(len(bs)))
	b.ResetTimer()

	totalCells := int64(0)
	for i := 0; i < b.N; i++ {
		bc, _ := bcfile.NewReader(bytes.NewReader(bs), int64(len(bs)))
		r, _ := Open(bc, block.Default())
		seen := int64(0)
		r.SetFilter(func(_ *Key) bool {
			seen++
			return seen%10 == 0 // accept 10%, reject 90%
		})
		accepted := int64(0)
		for {
			_, _, err := r.Next()
			if errors.Is(err, io.EOF) {
				break
			}
			if err != nil {
				b.Fatal(err)
			}
			accepted++
		}
		_ = r.Close()
		totalCells += accepted
	}
	if totalCells > 0 {
		b.ReportMetric(float64(totalCells)/b.Elapsed().Seconds(), "accepted-cells/sec")
	}
}

// BenchmarkWalkBigUserDataReject99 mirrors a typical low-clearance scan
// where most cells are filtered out. The pushdown advantage is most
// dramatic here — every reject saves a Key clone + value alloc.
func BenchmarkWalkBigUserDataReject99(b *testing.B) {
	const path = "/tmp/captured-big-user.rf"
	bs, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			b.Skipf("benchmark fixture not present at %s", path)
		}
		b.Fatal(err)
	}
	b.SetBytes(int64(len(bs)))
	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		bc, _ := bcfile.NewReader(bytes.NewReader(bs), int64(len(bs)))
		r, _ := Open(bc, block.Default())
		seen := int64(0)
		r.SetFilter(func(_ *Key) bool {
			seen++
			return seen%100 == 0
		})
		for {
			_, _, err := r.Next()
			if errors.Is(err, io.EOF) {
				break
			}
			if err != nil {
				b.Fatal(err)
			}
		}
		_ = r.Close()
	}
}

// BenchmarkBlockSkip_NoSkip walks a synthetic 50K-cell augmented file
// with no skip predicate. Establishes the baseline for the block-skip
// comparison below.
func BenchmarkBlockSkip_NoSkip(b *testing.B) {
	bs := buildAugmentedFixture(b, 50000, 4096) // 4KB blocks → ~250 blocks
	b.SetBytes(int64(len(bs)))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		bc, _ := bcfile.NewReader(bytesReaderAt(bs), int64(len(bs)))
		r, _ := Open(bc, block.Default())
		count := 0
		for {
			_, _, err := r.Next()
			if errors.Is(err, io.EOF) {
				break
			}
			if err != nil {
				b.Fatal(err)
			}
			count++
		}
		_ = r.Close()
		_ = count
	}
}

// BenchmarkBlockSkip_Skip90 walks the same file but with a skip
// predicate that bypasses the 90% of blocks whose tsMax exceeds a
// threshold — typical "give me old cells only" pattern. The skipped
// blocks pay zero cost (no fetch, no decompress, no decode).
func BenchmarkBlockSkip_Skip90(b *testing.B) {
	bs := buildAugmentedFixture(b, 50000, 4096)
	// Threshold = 5000 → 10% of cells live in blocks where tsMin <= 5000.
	const threshold int64 = 5000
	b.SetBytes(int64(len(bs)))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		bc, _ := bcfile.NewReader(bytesReaderAt(bs), int64(len(bs)))
		r, _ := Open(bc, block.Default())
		r.SetSkipPredicate(func(leafIdx int, _ *index.IndexEntry, bm *blockmeta.BlockMeta) bool {
			ov := bm.FindOverlay(leafIdx, blockmeta.OverlayZoneMap)
			if ov == nil {
				return false
			}
			zm, err := blockmeta.DecodeZoneMap(ov.Payload)
			if err != nil {
				return false
			}
			return zm.TsMin > threshold
		})
		count := 0
		for {
			_, _, err := r.Next()
			if errors.Is(err, io.EOF) {
				break
			}
			if err != nil {
				b.Fatal(err)
			}
			count++
		}
		_ = r.Close()
	}
}

// BenchmarkBlockSkip_Skip99 same as above but with threshold tightening
// to skip 99% of blocks — the metadata-driven block-skip win is most
// dramatic here.
func BenchmarkBlockSkip_Skip99(b *testing.B) {
	bs := buildAugmentedFixture(b, 50000, 4096)
	const threshold int64 = 500
	b.SetBytes(int64(len(bs)))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		bc, _ := bcfile.NewReader(bytesReaderAt(bs), int64(len(bs)))
		r, _ := Open(bc, block.Default())
		r.SetSkipPredicate(func(leafIdx int, _ *index.IndexEntry, bm *blockmeta.BlockMeta) bool {
			ov := bm.FindOverlay(leafIdx, blockmeta.OverlayZoneMap)
			if ov == nil {
				return false
			}
			zm, err := blockmeta.DecodeZoneMap(ov.Payload)
			if err != nil {
				return false
			}
			return zm.TsMin > threshold
		})
		for {
			_, _, err := r.Next()
			if errors.Is(err, io.EOF) {
				break
			}
			if err != nil {
				b.Fatal(err)
			}
		}
		_ = r.Close()
	}
}

func buildAugmentedFixture(b *testing.B, cells, blockSize int) []byte {
	b.Helper()
	zm := blockmeta.NewZoneMapBuilder()
	var buf bytes.Buffer
	w, err := NewWriter(&buf, WriterOptions{
		Codec:             block.CodecSnappy,
		BlockSize:         blockSize,
		BlockMetaBuilders: []blockmeta.OverlayBuilder{zm},
	})
	if err != nil {
		b.Fatal(err)
	}
	for i := 0; i < cells; i++ {
		k := &Key{
			Row:             []byte(fmt.Sprintf("row%07d", i)),
			ColumnFamily:    []byte("cf"),
			ColumnQualifier: []byte("cq"),
			Timestamp:       int64(i + 1),
		}
		v := []byte(fmt.Sprintf("value-%d-padding-xxxxxxxxxxxxxxxxxxxx", i))
		if err := w.Append(k, v); err != nil {
			b.Fatal(err)
		}
	}
	if err := w.Close(); err != nil {
		b.Fatal(err)
	}
	return buf.Bytes()
}

type bytesReaderAtT []byte

func (b bytesReaderAtT) ReadAt(p []byte, off int64) (int, error) {
	if off >= int64(len(b)) {
		return 0, io.EOF
	}
	n := copy(p, b[off:])
	if n < len(p) {
		return n, io.EOF
	}
	return n, nil
}

func bytesReaderAt(b []byte) bytesReaderAtT { return bytesReaderAtT(b) }

// BenchmarkWarmCache_BigUserData walks the same ~50MB user-data file
// many times against a single shared cache. First iteration populates;
// subsequent iterations hit cache and skip GCS-equivalent reads + snappy
// decompression. Reports the speedup over cold reads.
func BenchmarkWarmCache_BigUserData(b *testing.B) {
	const path = "/tmp/captured-big-user.rf"
	bs, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			b.Skipf("benchmark fixture not present at %s", path)
		}
		b.Fatal(err)
	}
	bcache := cache.NewBlockCache(512 << 20) // 512 MB cache — comfortably holds the ~250MB of decompressed blocks for this 51MB snappy file
	b.SetBytes(int64(len(bs)))
	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		bc, _ := bcfile.NewReader(bytes.NewReader(bs), int64(len(bs)))
		r, _ := Open(bc, block.Default(), WithBlockCache(bcache, "captured-big-user"))
		count := 0
		for {
			_, _, err := r.Next()
			if errors.Is(err, io.EOF) {
				break
			}
			if err != nil {
				b.Fatal(err)
			}
			count++
		}
		_ = r.Close()
	}
	b.StopTimer()
	st := bcache.Stats()
	b.ReportMetric(float64(st.Hits), "cache-hits")
	b.ReportMetric(float64(st.Misses), "cache-misses")
}

// BenchmarkAllocsBigUserData reports per-op allocation count separately —
// useful so we can SEE the alloc reduction as we push visibility filtering
// + zero-copy slicing into relkey.
func BenchmarkAllocsBigUserData(b *testing.B) {
	const path = "/tmp/captured-big-user.rf"
	bs, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			b.Skipf("benchmark fixture not present at %s", path)
		}
		b.Fatal(err)
	}
	b.ResetTimer()
	b.ReportAllocs()

	var memBefore, memAfter runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&memBefore)
	totalCells := int64(0)
	for i := 0; i < b.N; i++ {
		bc, _ := bcfile.NewReader(bytes.NewReader(bs), int64(len(bs)))
		r, _ := Open(bc, block.Default())
		for {
			_, _, err := r.Next()
			if errors.Is(err, io.EOF) {
				break
			}
			if err != nil {
				b.Fatal(err)
			}
			totalCells++
		}
		_ = r.Close()
	}
	runtime.ReadMemStats(&memAfter)
	allocsPerCell := float64(memAfter.Mallocs-memBefore.Mallocs) / float64(totalCells)
	b.ReportMetric(allocsPerCell, "allocs/cell")
}
