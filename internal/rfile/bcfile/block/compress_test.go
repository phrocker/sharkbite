package block

import (
	"bytes"
	"errors"
	"strings"
	"testing"

	"github.com/phrocker/shoal-oss/internal/rfile/bcfile"
)

// TestCompressor_RoundtripNone verifies the identity codec passes bytes
// through and that the decompressor on the other side reads them back.
func TestCompressor_RoundtripNone(t *testing.T) {
	c := DefaultCompressor()
	d := Default()

	payload := []byte("hello none codec")
	compressed, err := c.Encode(payload, CodecNone)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(compressed, payload) {
		t.Errorf("none codec changed bytes: got %q, want %q", compressed, payload)
	}

	src := fileLike(map[int64][]byte{0: compressed})
	got, err := d.Block(src, bcfile.BlockRegion{
		Offset: 0, CompressedSize: int64(len(compressed)), RawSize: int64(len(payload)),
	}, CodecNone)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, payload) {
		t.Errorf("decompress none: got %q, want %q", got, payload)
	}
}

// TestCompressor_RoundtripGzip is the load-bearing roundtrip — write and
// read through the gz codec, confirming our stack is symmetric.
func TestCompressor_RoundtripGzip(t *testing.T) {
	c := DefaultCompressor()
	d := Default()

	payload := []byte(strings.Repeat("the quick brown fox jumps over the lazy dog. ", 200))
	compressed, err := c.Encode(payload, CodecGzip)
	if err != nil {
		t.Fatal(err)
	}
	// Sanity: gzip should actually compress repeated strings.
	if len(compressed) >= len(payload) {
		t.Errorf("gzip didn't shrink: got %d bytes from %d", len(compressed), len(payload))
	}

	src := fileLike(map[int64][]byte{0: compressed})
	got, err := d.Block(src, bcfile.BlockRegion{
		Offset: 0, CompressedSize: int64(len(compressed)), RawSize: int64(len(payload)),
	}, CodecGzip)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, payload) {
		t.Errorf("gzip roundtrip mismatch: got %d bytes, want %d", len(got), len(payload))
	}
}

func TestCompressor_UnsupportedCodec(t *testing.T) {
	c := DefaultCompressor()
	_, err := c.Encode([]byte("x"), "brotli")
	if !errors.Is(err, ErrUnsupportedCodec) {
		t.Errorf("err = %v, want ErrUnsupportedCodec", err)
	}
}

func TestCompressor_Has(t *testing.T) {
	c := DefaultCompressor()
	for _, codec := range []string{CodecNone, CodecGzip, CodecSnappy, CodecZstd, CodecLZ4} {
		if !c.Has(codec) {
			t.Errorf("DefaultCompressor() missing codec %q", codec)
		}
	}
}

func TestCompressor_Register(t *testing.T) {
	c := NewCompressor()
	c.Register("rev", func(raw []byte) ([]byte, error) {
		out := make([]byte, len(raw))
		for i, b := range raw {
			out[len(raw)-1-i] = b
		}
		return out, nil
	})
	got, err := c.Encode([]byte("abc"), "rev")
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "cba" {
		t.Errorf("got %q, want %q", got, "cba")
	}
}
