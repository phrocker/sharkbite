package block

import (
	"bytes"
	"errors"
	"strings"
	"testing"
)

func TestZstd_RoundtripAndStreamFormat(t *testing.T) {
	cases := []struct {
		name string
		raw  []byte
	}{
		{name: "empty"},
		{name: "short", raw: []byte("hello zstd")},
		{name: "compressible", raw: []byte(strings.Repeat("accumulo-bcfile-", 1000))},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			encoded, err := encodeZstd(tc.raw)
			if err != nil {
				t.Fatal(err)
			}
			if len(encoded) < 4 || !bytes.Equal(encoded[:4], []byte{0x28, 0xb5, 0x2f, 0xfd}) {
				t.Fatalf("zstd stream magic = %x", encoded)
			}
			got, err := decompressZstd(encoded, int64(len(tc.raw)))
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(got, tc.raw) {
				t.Fatalf("roundtrip got %d bytes, want %d", len(got), len(tc.raw))
			}
		})
	}
}

func TestZstd_SizeMismatch(t *testing.T) {
	encoded, err := encodeZstd([]byte("abc"))
	if err != nil {
		t.Fatal(err)
	}
	for _, rawSize := range []int64{2, 4} {
		_, err := decompressZstd(encoded, rawSize)
		if !errors.Is(err, ErrSizeMismatch) {
			t.Errorf("rawSize=%d: err=%v, want ErrSizeMismatch", rawSize, err)
		}
	}
}

func TestZstd_RejectsTruncatedAndCorruptData(t *testing.T) {
	encoded, err := encodeZstd([]byte(strings.Repeat("zstd-data-", 100)))
	if err != nil {
		t.Fatal(err)
	}
	cases := map[string][]byte{
		"empty":     {},
		"truncated": encoded[:len(encoded)-1],
		"bad magic": append([]byte(nil), encoded...),
	}
	cases["bad magic"][0] ^= 0xff
	for name, data := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := decompressZstd(data, int64(len(strings.Repeat("zstd-data-", 100)))); err == nil {
				t.Fatal("expected decode error")
			}
		})
	}
}
