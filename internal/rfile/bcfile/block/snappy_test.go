package block

import (
	"bytes"
	"errors"
	"strings"
	"testing"
)

func TestSnappy_Roundtrip(t *testing.T) {
	cases := []struct {
		name string
		raw  []byte
	}{
		{"empty", []byte{}},
		{"short", []byte("hello world")},
		{"compressible", bytes.Repeat([]byte("ab"), 200)},
		{"high-entropy", []byte(strings.Repeat("the quick brown fox jumps over the lazy dog ", 30))},
	}
	c := DefaultCompressor()
	d := Default()
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			enc, err := c.Encode(tc.raw, CodecSnappy)
			if err != nil {
				t.Fatalf("encode: %v", err)
			}
			got, err := decompressSnappy(enc, int64(len(tc.raw)))
			if err != nil {
				t.Fatalf("decompress: %v", err)
			}
			if !bytes.Equal(got, tc.raw) {
				t.Errorf("roundtrip mismatch: got %d bytes, want %d", len(got), len(tc.raw))
			}
			if !d.Has(CodecSnappy) {
				t.Errorf("snappy not registered in Default decompressor")
			}
		})
	}
}

// TestSnappy_FrameSizeMismatch covers the failure mode where the outer
// frame's originalSize disagrees with the BlockRegion's RawSize. The
// codec MUST surface this loud — corrupt files often appear here first.
func TestSnappy_FrameSizeMismatch(t *testing.T) {
	c := DefaultCompressor()
	enc, err := c.Encode([]byte("abc"), CodecSnappy)
	if err != nil {
		t.Fatal(err)
	}
	_, err = decompressSnappy(enc, 99)
	if !errors.Is(err, ErrSizeMismatch) {
		t.Errorf("got %v; want ErrSizeMismatch", err)
	}
}

// TestSnappy_TruncatedFrame: cut off the inner snappy bytes; the codec
// should error rather than panic or return short output.
func TestSnappy_TruncatedFrame(t *testing.T) {
	c := DefaultCompressor()
	enc, err := c.Encode([]byte("the quick brown fox"), CodecSnappy)
	if err != nil {
		t.Fatal(err)
	}
	if len(enc) < 9 {
		t.Skip("encoded form too short to truncate")
	}
	_, err = decompressSnappy(enc[:len(enc)-2], int64(len("the quick brown fox")))
	if err == nil {
		t.Errorf("expected error on truncated frame")
	}
}
