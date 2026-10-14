package rfile

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"testing"

	"github.com/phrocker/shoal-oss/internal/rfile/bcfile"
	"github.com/phrocker/shoal-oss/internal/rfile/bcfile/block"
)

// This file isolates the cost of block compression on the RFile write and
// read paths, and reports the on-disk size each codec produces for the
// same entity-shaped dataset (3 cells/row, 128-byte values) the engine
// benchmarks use. It answers two questions:
//
//   - How much does snappy cost on flush (compress) and scan (decompress)
//     versus storing blocks uncompressed?
//   - How much smaller does each codec make the RFile?
//
// These run without cgo: `go test -bench=BenchmarkRFileCodec ./internal/rfile/`
// and `go test -run=TestRFileSize_Codecs -v ./internal/rfile/`.

// codecCases is the set of codecs we compare. "none" is the baseline that
// isolates the compression cost; snappy is the engine default; gz shows
// the higher-ratio / higher-CPU end.
var codecCases = []string{block.CodecNone, block.CodecSnappy, block.CodecGzip}

// vocab is a small word pool used to synthesize realistic, text-like
// property values. Natural-language labels/descriptions in a knowledge
// graph have moderate entropy — repeated words and characters — so they
// compress meaningfully (unlike random bytes) but far less than a single
// repeated value. This yields representative codec ratios.
var vocab = []string{
	"entity", "person", "organization", "location", "event", "document",
	"knowledge", "graph", "node", "edge", "relation", "concept", "topic",
	"the", "of", "and", "in", "with", "associated", "related", "primary",
	"secondary", "active", "inactive", "verified", "pending", "source",
	"target", "weight", "score", "label", "summary", "description", "title",
}

// realisticValue fills ~n bytes with space-joined words drawn from vocab,
// producing text-like content with natural redundancy.
func realisticValue(rng *rand.Rand, n int) []byte {
	out := make([]byte, 0, n+16)
	for len(out) < n {
		if len(out) > 0 {
			out = append(out, ' ')
		}
		out = append(out, vocab[rng.Intn(len(vocab))]...)
	}
	return out[:n]
}

// buildCodecDataset returns rows*3 cells mirroring engine makeMutation:
// row "entity:%08d", CF "props", CQ in {label,type,salience}. The 128-byte
// "label" value is realistic text (space-joined words from a vocabulary),
// so the compression numbers reflect representative graph payloads —
// natural-language labels and descriptions — with moderate entropy rather
// than either a single repeated value or incompressible random bytes.
func buildCodecDataset(rows int) ([]*Key, [][]byte) {
	rng := rand.New(rand.NewSource(1))
	keys := make([]*Key, 0, rows*3)
	vals := make([][]byte, 0, rows*3)
	for i := 0; i < rows; i++ {
		row := []byte(fmt.Sprintf("entity:%08d", i))
		val := realisticValue(rng, 128)
		add := func(cq string, v []byte) {
			keys = append(keys, &Key{
				Row:             row,
				ColumnFamily:    []byte("props"),
				ColumnQualifier: []byte(cq),
				Timestamp:       1,
			})
			vals = append(vals, v)
		}
		add("label", val)
		add("salience", []byte("0.85"))
		add("type", []byte("entity"))
	}
	return keys, vals
}

// writeCodecRFile encodes keys/vals into an RFile with the given codec and
// returns the serialized bytes.
func writeCodecRFile(tb testing.TB, codec string, keys []*Key, vals [][]byte) []byte {
	tb.Helper()
	var buf bytes.Buffer
	w, err := NewWriter(&buf, WriterOptions{Codec: codec, Compressor: block.DefaultCompressor()})
	if err != nil {
		tb.Fatal(err)
	}
	for i := range keys {
		if err := w.Append(keys[i], vals[i]); err != nil {
			tb.Fatal(err)
		}
	}
	if err := w.Close(); err != nil {
		tb.Fatal(err)
	}
	return buf.Bytes()
}

// BenchmarkRFileCodec_Write measures the cost of building (and thus
// compressing) the whole RFile per codec — the flush-path compression
// cost. Reports the resulting file size as bytes/op metadata via
// ReportMetric so the size/CPU tradeoff is visible in one run.
func BenchmarkRFileCodec_Write(b *testing.B) {
	keys, vals := buildCodecDataset(10_000)
	for _, codec := range codecCases {
		b.Run(codec, func(b *testing.B) {
			var size int
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				bs := writeCodecRFile(b, codec, keys, vals)
				size = len(bs)
			}
			b.StopTimer()
			b.ReportMetric(float64(size), "filebytes")
			b.ReportMetric(float64(size)/float64(len(keys)), "bytes/cell")
		})
	}
}

// BenchmarkRFileCodec_Scan measures a full sequential read of every cell
// per codec — the decompress cost on the scan hot path. The file is built
// once outside the timed loop; only Open + walk is timed.
func BenchmarkRFileCodec_Scan(b *testing.B) {
	keys, vals := buildCodecDataset(10_000)
	// Reference logical size: the uncompressed RFile bytes. Used as the
	// SetBytes basis for EVERY codec so the reported MB/s is comparable
	// decode throughput over the same logical data — not skewed by each
	// codec's on-disk size.
	logicalSize := int64(len(writeCodecRFile(b, block.CodecNone, keys, vals)))
	for _, codec := range codecCases {
		bs := writeCodecRFile(b, codec, keys, vals)
		b.Run(codec, func(b *testing.B) {
			b.SetBytes(logicalSize)
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				bc, err := bcfile.NewReader(bytes.NewReader(bs), int64(len(bs)))
				if err != nil {
					b.Fatal(err)
				}
				r, err := Open(bc, block.Default())
				if err != nil {
					b.Fatal(err)
				}
				if err := r.Seek(nil); err != nil {
					b.Fatal(err)
				}
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
				if count != len(keys) {
					b.Fatalf("scanned %d cells, want %d", count, len(keys))
				}
			}
		})
	}
}

// TestRFileSize_Codecs reports the on-disk RFile size each codec produces
// for the entity dataset, plus the compression ratio versus uncompressed.
// It always runs (no cgo) so the size comparison is available alongside
// the SQLite file-size test. Pure reporting — it asserts only that
// compressed output is no larger than uncompressed.
func TestRFileSize_Codecs(t *testing.T) {
	const rows = 10_000
	keys, vals := buildCodecDataset(rows)

	var noneSize int
	t.Logf("RFile size for %d rows (%d cells, 128-byte label values):", rows, len(keys))
	for _, codec := range codecCases {
		bs := writeCodecRFile(t, codec, keys, vals)
		if codec == block.CodecNone {
			noneSize = len(bs)
		}
		ratio := 1.0
		if noneSize > 0 {
			ratio = float64(len(bs)) / float64(noneSize)
		}
		t.Logf("  %-7s %9d bytes  %6.2f bytes/cell  %5.2fx vs none",
			codec, len(bs), float64(len(bs))/float64(len(keys)), ratio)
		if len(bs) > noneSize && codec != block.CodecNone {
			t.Errorf("codec %s produced %d bytes, larger than uncompressed %d", codec, len(bs), noneSize)
		}
	}
}
