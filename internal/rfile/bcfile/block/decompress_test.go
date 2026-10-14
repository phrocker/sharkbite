package block

import (
	"bytes"
	"compress/zlib"
	"encoding/binary"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"

	"github.com/phrocker/shoal-oss/internal/rfile/bcfile"
)

// gzipBytes returns the BCFile-"gz"-compressed form of data, used to
// set up fixtures without ever depending on a real BCFile. NB: the
// codec is named "gz" but produces zlib (RFC 1950) — see compress.go's
// encodeGzip note. The function name is kept for back-compat with the
// existing test call sites.
func gzipBytes(t *testing.T, data []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zlib.NewWriter(&buf)
	if _, err := zw.Write(data); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// fileLike packs a slice into an io.ReaderAt with a known offset, so
// tests can mimic a BCFile's "block N starts at offset O" layout without
// writing real BCFiles.
func fileLike(layout map[int64][]byte) io.ReaderAt {
	// Find total size, write a flat buffer, return a bytes.Reader.
	var max int64
	for off, data := range layout {
		if end := off + int64(len(data)); end > max {
			max = end
		}
	}
	buf := make([]byte, max)
	for off, data := range layout {
		copy(buf[off:], data)
	}
	return bytes.NewReader(buf)
}

func TestDecompressor_None(t *testing.T) {
	d := Default()
	payload := []byte("hello, world — passthrough")
	src := fileLike(map[int64][]byte{100: payload})
	got, err := d.Block(src, bcfile.BlockRegion{
		Offset:         100,
		CompressedSize: int64(len(payload)),
		RawSize:        int64(len(payload)),
	}, CodecNone)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, payload) {
		t.Errorf("got %q, want %q", got, payload)
	}
}

func TestDecompressor_NoneRawSizeMismatch(t *testing.T) {
	d := Default()
	payload := []byte("five-")
	src := fileLike(map[int64][]byte{0: payload})
	_, err := d.Block(src, bcfile.BlockRegion{
		Offset: 0, CompressedSize: int64(len(payload)), RawSize: int64(len(payload) + 1),
	}, CodecNone)
	if !errors.Is(err, ErrSizeMismatch) {
		t.Errorf("err = %v, want ErrSizeMismatch", err)
	}
}

func TestDecompressor_Gzip(t *testing.T) {
	d := Default()
	payload := []byte(strings.Repeat("the quick brown fox ", 50))
	gz := gzipBytes(t, payload)
	src := fileLike(map[int64][]byte{0: gz})

	got, err := d.Block(src, bcfile.BlockRegion{
		Offset:         0,
		CompressedSize: int64(len(gz)),
		RawSize:        int64(len(payload)),
	}, CodecGzip)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, payload) {
		t.Errorf("decompression mismatch: got %d bytes, want %d", len(got), len(payload))
	}
}

func TestDecompressor_GzipBadHeader(t *testing.T) {
	d := Default()
	garbage := []byte("not a gzip stream")
	src := fileLike(map[int64][]byte{0: garbage})
	_, err := d.Block(src, bcfile.BlockRegion{
		Offset: 0, CompressedSize: int64(len(garbage)), RawSize: 99,
	}, CodecGzip)
	if err == nil {
		t.Errorf("expected gzip header error")
	}
}

func TestDecompressor_GzipRawSizeMismatch(t *testing.T) {
	d := Default()
	payload := []byte("twelve bytes")
	gz := gzipBytes(t, payload)
	src := fileLike(map[int64][]byte{0: gz})
	_, err := d.Block(src, bcfile.BlockRegion{
		Offset: 0, CompressedSize: int64(len(gz)), RawSize: int64(len(payload) + 100),
	}, CodecGzip)
	if !errors.Is(err, ErrSizeMismatch) {
		t.Errorf("err = %v, want ErrSizeMismatch", err)
	}
}

func TestDecompressor_GzipStopsAtDeclaredRawSize(t *testing.T) {
	payload := bytes.Repeat([]byte("x"), 1<<20)
	gz := gzipBytes(t, payload)
	_, err := Default().Block(bytes.NewReader(gz), bcfile.BlockRegion{
		CompressedSize: int64(len(gz)),
		RawSize:        1,
	}, CodecGzip)
	if !errors.Is(err, ErrSizeMismatch) {
		t.Fatalf("err = %v, want ErrSizeMismatch", err)
	}
}

func TestDecompressor_SnappyRejectsChunkBeyondDeclaredOutput(t *testing.T) {
	compressed, err := encodeSnappy([]byte("oversized"))
	if err != nil {
		t.Fatal(err)
	}
	binary.BigEndian.PutUint32(compressed[:4], 1)
	_, err = Default().Block(bytes.NewReader(compressed), bcfile.BlockRegion{
		CompressedSize: int64(len(compressed)),
		RawSize:        1,
	}, CodecSnappy)
	if !errors.Is(err, ErrSizeMismatch) {
		t.Fatalf("err = %v, want ErrSizeMismatch", err)
	}
}

func TestDecompressor_UnsupportedCodec(t *testing.T) {
	d := Default()
	src := fileLike(map[int64][]byte{0: {0, 0, 0}})
	_, err := d.Block(src, bcfile.BlockRegion{Offset: 0, CompressedSize: 3, RawSize: 3}, "brotli")
	if !errors.Is(err, ErrUnsupportedCodec) {
		t.Errorf("err = %v, want ErrUnsupportedCodec", err)
	}
}

func TestDecompressor_Register(t *testing.T) {
	d := NewDecompressor()
	d.Register("rev", func(compressed []byte, rawSize int64) ([]byte, error) {
		out := make([]byte, len(compressed))
		for i, b := range compressed {
			out[len(compressed)-1-i] = b
		}
		if int64(len(out)) != rawSize {
			return nil, ErrSizeMismatch
		}
		return out, nil
	})
	src := fileLike(map[int64][]byte{0: []byte("abc")})
	got, err := d.Block(src, bcfile.BlockRegion{Offset: 0, CompressedSize: 3, RawSize: 3}, "rev")
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "cba" {
		t.Errorf("got %q, want %q", got, "cba")
	}
}

func TestDecompressor_Has(t *testing.T) {
	d := Default()
	for _, codec := range []string{CodecNone, CodecGzip, CodecSnappy, CodecZstd, CodecLZ4} {
		if !d.Has(codec) {
			t.Errorf("Default() missing codec %q", codec)
		}
	}
}

func TestDecompressor_BlockSizeLimits(t *testing.T) {
	d := Default()
	src := fileLike(nil)

	_, err := d.Block(src, bcfile.BlockRegion{
		CompressedSize: bcfile.MaxCompressedBlockSize + 1,
	}, CodecNone)
	if err == nil {
		t.Fatal("expected oversized compressed block to fail")
	}

	_, err = d.Block(src, bcfile.BlockRegion{
		RawSize: bcfile.MaxRawBlockSize + 1,
	}, CodecNone)
	if err == nil {
		t.Fatal("expected oversized raw block to fail")
	}
}

func TestDecompressor_NegativeSizes(t *testing.T) {
	d := Default()
	src := fileLike(nil)
	_, err := d.Block(src, bcfile.BlockRegion{Offset: 0, CompressedSize: -1, RawSize: 0}, CodecNone)
	if err == nil {
		t.Errorf("expected error on negative CompressedSize")
	}
	_, err = d.Block(src, bcfile.BlockRegion{Offset: 0, CompressedSize: 0, RawSize: -1}, CodecNone)
	if err == nil {
		t.Errorf("expected error on negative RawSize")
	}
}

func TestDecompressor_ZeroSizeBlock(t *testing.T) {
	d := Default()
	src := fileLike(nil)
	got, err := d.Block(src, bcfile.BlockRegion{Offset: 0, CompressedSize: 0, RawSize: 0}, CodecNone)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Errorf("got %d bytes, want 0", len(got))
	}
}

// TestDecompressor_ConcurrentBlock ensures Block is safe to call from
// multiple goroutines against the same Decompressor + ReaderAt — that's
// the substrate the prefetcher relies on.
func TestDecompressor_ConcurrentBlock(t *testing.T) {
	d := Default()
	payload := []byte(strings.Repeat("xy", 1024))
	gz := gzipBytes(t, payload)

	// Lay out 16 identical blocks at distinct offsets.
	const N = 16
	const blockSize = int64(2048) // each block reserves 2KB but only fills `len(gz)`
	layout := map[int64][]byte{}
	for i := int64(0); i < N; i++ {
		layout[i*blockSize] = gz
	}
	src := fileLike(layout)

	var wg sync.WaitGroup
	wg.Add(N)
	for i := int64(0); i < N; i++ {
		go func(blockIdx int64) {
			defer wg.Done()
			offset := blockIdx * blockSize
			got, err := d.Block(src, bcfile.BlockRegion{
				Offset:         offset,
				CompressedSize: int64(len(gz)),
				RawSize:        int64(len(payload)),
			}, CodecGzip)
			if err != nil {
				t.Errorf("block %d (offset %d): %v", blockIdx, offset, err)
				return
			}
			if !bytes.Equal(got, payload) {
				t.Errorf("block %d (offset %d): payload mismatch", blockIdx, offset)
			}
		}(i)
	}
	wg.Wait()
}
