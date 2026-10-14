package wire

import (
	"bytes"
	"errors"
	"io"
	"math"
	"testing"
)

// TestVIntRoundtrip exercises the boundary cases that make Hadoop's varint
// unusual: the -112 / -113 / -120 / -121 transitions and the largest values
// that fit in 1, 2, 3, 4, 5, 8 magnitude bytes.
func TestVIntRoundtrip(t *testing.T) {
	cases := []struct {
		name    string
		v       int64
		wantLen int
	}{
		// Single-byte direct values (-112..127).
		{"zero", 0, 1},
		{"one", 1, 1},
		{"neg_one", -1, 1},
		{"upper_single", 127, 1},
		{"lower_single", -112, 1},
		// Just past the single-byte window: these need the 1-byte magnitude
		// header plus 1 magnitude byte.
		{"pos_just_past_127", 128, 2},
		{"neg_just_past_minus_112", -113, 2},
		{"neg_minus_120", -120, 2},
		{"neg_minus_121", -121, 2},
		// 2-byte magnitude
		{"pos_max_short", math.MaxInt16, 3},
		{"neg_min_short", math.MinInt16, 3},
		// 3-byte magnitude
		{"pos_24bit", 1 << 23, 4},
		{"neg_24bit", -(1 << 23), 4},
		// 4-byte magnitude
		{"pos_max_int32", math.MaxInt32, 5},
		{"neg_min_int32", math.MinInt32, 5},
		// Large: 1<<40 needs 6 magnitude bytes -> 1 header + 6 = 7 total.
		{"pos_big", 1 << 40, 7},
		{"max_int64", math.MaxInt64, 9},
		{"min_int64", math.MinInt64, 9},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var buf bytes.Buffer
			n, err := WriteVLong(&buf, c.v)
			if err != nil {
				t.Fatalf("WriteVLong(%d): %v", c.v, err)
			}
			if n != c.wantLen {
				t.Errorf("WriteVLong(%d) wrote %d bytes, want %d", c.v, n, c.wantLen)
			}
			if buf.Len() != c.wantLen {
				t.Errorf("buffer has %d bytes, want %d", buf.Len(), c.wantLen)
			}
			// Verify DecodeVIntSize agrees with the encoder.
			if got := DecodeVIntSize(buf.Bytes()[0]); got != c.wantLen {
				t.Errorf("DecodeVIntSize(%#x)=%d, want %d", buf.Bytes()[0], got, c.wantLen)
			}
			got, n2, err := ReadVLong(bytes.NewReader(buf.Bytes()))
			if err != nil {
				t.Fatalf("ReadVLong: %v", err)
			}
			if n2 != c.wantLen {
				t.Errorf("ReadVLong consumed %d bytes, want %d", n2, c.wantLen)
			}
			if got != c.v {
				t.Errorf("roundtrip: got %d, want %d", got, c.v)
			}
		})
	}
}

// TestVIntKnownEncodings pins exact byte encodings for a few critical
// values. These were cross-checked against Hadoop's WritableUtils by hand:
//
//	v=0     -> 0x00 (single-byte direct)
//	v=127   -> 0x7f (single-byte direct, max)
//	v=-112  -> 0x90 (single-byte direct, min — int8(-112) = 0x90)
//	v=128   -> 0x8f 0x80 (header -113 = 0x8f, magnitude 1 byte = 0x80)
//	v=-113  -> 0x87 0x70 (header -121 = 0x87, magnitude = ^(-113) = 112 = 0x70)
//	v=-1    -> 0xff (single-byte direct: int8(-1) = 0xff)
func TestVIntKnownEncodings(t *testing.T) {
	cases := []struct {
		v    int64
		want []byte
	}{
		{0, []byte{0x00}},
		{1, []byte{0x01}},
		{127, []byte{0x7f}},
		{-1, []byte{0xff}},
		{-112, []byte{0x90}},
		{128, []byte{0x8f, 0x80}},
		{-113, []byte{0x87, 0x70}},
		{255, []byte{0x8f, 0xff}},
		{256, []byte{0x8e, 0x01, 0x00}},
		{-256, []byte{0x87, 0xff}}, // ^(-256) = 255 = 0xff
	}
	for _, c := range cases {
		var buf bytes.Buffer
		if _, err := WriteVLong(&buf, c.v); err != nil {
			t.Fatalf("WriteVLong(%d): %v", c.v, err)
		}
		if !bytes.Equal(buf.Bytes(), c.want) {
			t.Errorf("WriteVLong(%d) = %#x, want %#x", c.v, buf.Bytes(), c.want)
		}
		got, _, err := ReadVLong(bytes.NewReader(c.want))
		if err != nil {
			t.Fatalf("ReadVLong: %v", err)
		}
		if got != c.v {
			t.Errorf("ReadVLong(%#x) = %d, want %d", c.want, got, c.v)
		}
	}
}

// TestReadVIntOverflowGuard ensures ReadVInt rejects encoded values that
// don't fit in int32, even though the wire format would happily accept
// them.
func TestReadVIntOverflowGuard(t *testing.T) {
	var buf bytes.Buffer
	if _, err := WriteVLong(&buf, int64(math.MaxInt32)+1); err != nil {
		t.Fatalf("WriteVLong: %v", err)
	}
	_, _, err := ReadVInt(bytes.NewReader(buf.Bytes()))
	if err == nil {
		t.Fatal("expected overflow error")
	}
}

// TestReadVLongTruncated ensures we surface short reads instead of
// silently returning a partial value.
func TestReadVLongTruncated(t *testing.T) {
	// 0x8f signals "1 magnitude byte follows" but we only give the header.
	_, _, err := ReadVLong(bytes.NewReader([]byte{0x8f}))
	if err == nil {
		t.Fatal("expected error on truncated vint")
	}
	if !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrUnexpectedEOF) {
		// fmt.Errorf wraps the underlying io error.
		if !bytes.Contains([]byte(err.Error()), []byte("EOF")) {
			t.Fatalf("expected EOF-flavored error, got: %v", err)
		}
	}
}

func TestIsNegativeVInt(t *testing.T) {
	cases := []struct {
		first byte
		want  bool
	}{
		{0x00, false}, // 0
		{0x7f, false}, // 127, single-byte direct, non-negative
		{0xff, true},  // -1, single-byte direct, predicate says negative
		{0x90, true},  // -112, single-byte direct (decode returns -112 directly)
		{0x8f, false}, // -113, leads positive multi-byte (header for >127)
		{0x88, false}, // -120, leads positive multi-byte (header for >127, largest)
		{0x87, true},  // -121, leads negative multi-byte
		{0x80, true},  // -128, leads negative multi-byte
	}
	for _, c := range cases {
		if got := IsNegativeVInt(c.first); got != c.want {
			t.Errorf("IsNegativeVInt(%#x) = %v, want %v", c.first, got, c.want)
		}
	}
}
