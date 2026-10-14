package block

import (
	"bytes"
	"encoding/binary"
	"errors"
	"math/rand"
	"testing"

	"github.com/pierrec/lz4/v4"
)

func TestLZ4_RoundtripAndFraming(t *testing.T) {
	raw := bytes.Repeat([]byte("hadoop-lz4-block-"), hadoopLZ4MaxInputSize/17+2)
	encoded, err := encodeLZ4(raw)
	if err != nil {
		t.Fatal(err)
	}
	if got := int(binary.BigEndian.Uint32(encoded[:4])); got != len(raw) {
		t.Fatalf("original size = %d, want %d", got, len(raw))
	}

	pos := 4
	chunks := 0
	decoded := make([]byte, 0, len(raw))
	for pos < len(encoded) {
		chunkLen := int(binary.BigEndian.Uint32(encoded[pos : pos+4]))
		pos += 4
		chunk := make([]byte, len(raw)-len(decoded))
		n, err := lz4.UncompressBlock(encoded[pos:pos+chunkLen], chunk)
		if err != nil {
			t.Fatalf("decode framed chunk %d: %v", chunks, err)
		}
		decoded = append(decoded, chunk[:n]...)
		pos += chunkLen
		chunks++
	}
	if chunks != 2 {
		t.Fatalf("chunk count = %d, want 2", chunks)
	}
	if !bytes.Equal(decoded, raw) {
		t.Fatal("framed chunks did not decode to the original payload")
	}

	got, err := decompressLZ4(encoded, int64(len(raw)))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, raw) {
		t.Fatal("roundtrip mismatch")
	}
}

func TestLZ4_HadoopLiteralFixture(t *testing.T) {
	fixture := []byte{
		0, 0, 0, 6,
		0, 0, 0, 4, 0x30, 'a', 'b', 'c',
		0, 0, 0, 4, 0x30, 'd', 'e', 'f',
	}
	got, err := decompressLZ4(fixture, 6)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "abcdef" {
		t.Fatalf("got %q, want %q", got, "abcdef")
	}
}

func TestLZ4_IncompressibleChunksUseLiteralBlocks(t *testing.T) {
	raw := make([]byte, hadoopLZ4MaxInputSize+4096)
	if _, err := rand.New(rand.NewSource(42)).Read(raw); err != nil {
		t.Fatal(err)
	}

	for offset := 0; offset < len(raw); {
		end := min(offset+hadoopLZ4MaxInputSize, len(raw))
		n, err := lz4.CompressBlock(raw[offset:end], make([]byte, end-offset), nil)
		if err != nil {
			t.Fatalf("verify incompressible chunk @ %d: %v", offset, err)
		}
		if n != 0 {
			t.Fatalf("chunk @ %d compressed to %d bytes; fixture must exercise literal fallback", offset, n)
		}
		offset = end
	}

	encoded, err := encodeLZ4(raw)
	if err != nil {
		t.Fatal(err)
	}
	if got := int(binary.BigEndian.Uint32(encoded[:4])); got != len(raw) {
		t.Fatalf("original size = %d, want %d", got, len(raw))
	}

	pos := 4
	chunks := 0
	decoded := make([]byte, 0, len(raw))
	for pos < len(encoded) {
		if len(encoded)-pos < 4 {
			t.Fatalf("truncated chunk header at %d", pos)
		}
		chunkLen := int(binary.BigEndian.Uint32(encoded[pos : pos+4]))
		pos += 4
		if chunkLen <= 0 || chunkLen > len(encoded)-pos {
			t.Fatalf("invalid chunk length %d at %d", chunkLen, pos-4)
		}
		dst := make([]byte, len(raw)-len(decoded))
		n, err := lz4.UncompressBlock(encoded[pos:pos+chunkLen], dst)
		if err != nil {
			t.Fatalf("independent chunk decode %d: %v", chunks, err)
		}
		decoded = append(decoded, dst[:n]...)
		pos += chunkLen
		chunks++
	}
	if chunks != 2 {
		t.Fatalf("chunk count = %d, want 2", chunks)
	}
	if !bytes.Equal(decoded, raw) {
		t.Fatal("independently decoded chunks do not match random input")
	}
}

func TestLZ4_Empty(t *testing.T) {
	encoded, err := encodeLZ4(nil)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(encoded, make([]byte, 4)) {
		t.Fatalf("empty frame = %x, want four-byte zero frame", encoded)
	}
	got, err := decompressLZ4(encoded, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("decoded %d bytes, want 0", len(got))
	}
}

func TestLZ4_SizeMismatch(t *testing.T) {
	encoded, err := encodeLZ4([]byte("abc"))
	if err != nil {
		t.Fatal(err)
	}
	for _, rawSize := range []int64{2, 4} {
		_, err := decompressLZ4(encoded, rawSize)
		if !errors.Is(err, ErrSizeMismatch) {
			t.Errorf("rawSize=%d: err=%v, want ErrSizeMismatch", rawSize, err)
		}
	}

	binary.BigEndian.PutUint32(encoded[:4], 2)
	_, err = decompressLZ4(encoded, 3)
	if !errors.Is(err, ErrSizeMismatch) {
		t.Errorf("undersized frame header: err=%v, want ErrSizeMismatch", err)
	}
}

func TestLZ4_RejectsMalformedFrames(t *testing.T) {
	valid, err := encodeLZ4([]byte("abcdef"))
	if err != nil {
		t.Fatal(err)
	}
	cases := map[string]struct {
		encoded []byte
		rawSize int64
	}{
		"empty stream":           {encoded: []byte{}, rawSize: 1},
		"truncated frame header": {encoded: []byte{0, 0, 0}, rawSize: 1},
		"negative frame size":    {encoded: []byte{0xff, 0xff, 0xff, 0xff}, rawSize: 1},
		"truncated chunk header": {encoded: []byte{0, 0, 0, 1, 0, 0, 0}, rawSize: 1},
		"negative chunk size": {
			encoded: []byte{0, 0, 0, 1, 0xff, 0xff, 0xff, 0xff}, rawSize: 1,
		},
		"zero chunk size": {
			encoded: []byte{0, 0, 0, 1, 0, 0, 0, 0}, rawSize: 1,
		},
		"truncated chunk body": {encoded: valid[:len(valid)-1], rawSize: 6},
		"data after zero frame": {
			encoded: []byte{0, 0, 0, 0, 1}, rawSize: 0,
		},
		"corrupt chunk": {
			encoded: []byte{0, 0, 0, 1, 0, 0, 0, 3, 0, 0, 0}, rawSize: 1,
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := decompressLZ4(tc.encoded, tc.rawSize); err == nil {
				t.Fatal("expected malformed frame error")
			}
		})
	}
}
