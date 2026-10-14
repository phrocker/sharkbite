package bcfile

import (
	"bytes"
	"errors"
	"io"
	"math"
	"testing"
)

// TestVLongRoundtrip walks every encoding-length boundary listed in the
// BCFile Utils.writeVLong Javadoc, encoding each then decoding back.
func TestVLongRoundtrip(t *testing.T) {
	cases := []struct {
		name    string
		v       int64
		wantLen int
	}{
		// 1-byte direct: -32..127
		{"zero", 0, 1},
		{"one", 1, 1},
		{"max_single", 127, 1},
		{"min_single", -32, 1},
		// 2 bytes: -20*256 ≤ n < 20*256, excluding the single-byte window
		{"pos_just_past_127", 128, 2},
		{"neg_just_past_minus_32", -33, 2},
		{"max_2byte", 20*256 - 1, 2},
		{"min_2byte", -20 * 256, 2},
		// 3 bytes
		{"max_3byte", 16*65536 - 1, 3},
		{"min_3byte", -16 * 65536, 3},
		// 4 bytes
		{"max_4byte", 8*(1<<24) - 1, 4},
		{"min_4byte", -8 * (1 << 24), 4},
		// 5 bytes — int32 range [-2^31, 2^31). Spec inclusivity:
		// the writer uses 5 bytes when value fits in 4 magnitude bytes,
		// and we step into 6 bytes once we cross 2^31.
		{"max_int32", math.MaxInt32, 5},
		{"min_int32", math.MinInt32, 5},
		// 6 bytes covers [-2^39, 2^39). 2^39 itself spills to 7.
		{"max_6byte", (1 << 39) - 1, 6},
		{"min_6byte", -(1 << 39), 6},
		{"pos_just_over_6byte", 1 << 39, 7},
		// 7 bytes covers [-2^47, 2^47).
		{"max_7byte", (1 << 47) - 1, 7},
		{"min_7byte", -(1 << 47), 7},
		{"pos_just_over_7byte", 1 << 47, 8},
		// 8 bytes covers [-2^55, 2^55).
		{"max_8byte", (1 << 55) - 1, 8},
		{"min_8byte", -(1 << 55), 8},
		{"pos_just_over_8byte", 1 << 55, 9},
		// 9 bytes — int64 max/min
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
				t.Errorf("WriteVLong(%d) wrote %d bytes, want %d (bytes=%x)",
					c.v, n, c.wantLen, buf.Bytes())
			}
			if buf.Len() != c.wantLen {
				t.Errorf("buffer has %d bytes, want %d", buf.Len(), c.wantLen)
			}
			got, m, err := ReadVLong(&buf)
			if err != nil {
				t.Fatalf("ReadVLong: %v", err)
			}
			if got != c.v {
				t.Errorf("roundtrip: got %d, want %d", got, c.v)
			}
			if m != c.wantLen {
				t.Errorf("ReadVLong consumed %d bytes, want %d", m, c.wantLen)
			}
		})
	}
}

// TestVIntOverflow ensures a vlong > int32 range surfaces an error from
// ReadVInt — matches Utils.readVInt's IllegalStateException.
func TestVIntOverflow(t *testing.T) {
	var buf bytes.Buffer
	if _, err := WriteVLong(&buf, int64(math.MaxInt32)+1); err != nil {
		t.Fatal(err)
	}
	if _, _, err := ReadVInt(&buf); err == nil {
		t.Errorf("ReadVInt accepted overflowing value")
	}
}

// TestVLongTruncatedTail catches the case where the header advertises N
// magnitude bytes but the stream is short — we should surface io.EOF /
// io.ErrUnexpectedEOF rather than silently returning a truncated value.
func TestVLongTruncatedTail(t *testing.T) {
	cases := []struct {
		name string
		in   []byte
	}{
		// firstByte=0x80 (-128 signed) → bucket 0 → dataLen = -128+129 = 1,
		// which is below the [4..8] valid range — should be ErrCorruptVInt
		// rather than a truncation, but still an error.
		{"corrupt_len_byte", []byte{0x80}},
		// firstByte=0xa0 (-96) → bucket 4 → 3-byte total, need 2 tail bytes.
		{"3byte_no_tail", []byte{0xa0}},
		// 3-byte total with 1 of 2 tail bytes present.
		{"3byte_partial_tail", []byte{0xa0, 0x00}},
		// firstByte=-120 → bucket 1 → 4-byte total, 2 of 3 tail bytes.
		{"4byte_partial_tail", []byte{0x88, 0x00, 0x00}},
		// firstByte=-125 → bucket 0 → dataLen=4, need 4 magnitude bytes,
		// got 3.
		{"5byte_partial_tail", []byte{0x83, 0x01, 0x02, 0x03}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, _, err := ReadVLong(bytes.NewReader(c.in))
			if err == nil {
				t.Fatalf("expected error for %x", c.in)
			}
			// Accept either an EOF chain (truncation) or ErrCorruptVInt
			// (invalid header for the corrupt_len_byte case).
			if !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrUnexpectedEOF) && !errors.Is(err, ErrCorruptVInt) {
				t.Errorf("err = %v, want io.EOF / io.ErrUnexpectedEOF / ErrCorruptVInt chain", err)
			}
		})
	}
}

// TestKnownEncodings nails specific bit patterns against the Java spec —
// these are the values the spec text in Utils.writeVLong shows literally.
func TestKnownEncodings(t *testing.T) {
	cases := []struct {
		v    int64
		want []byte
	}{
		// n=128: case 1 falls through to case 2 with firstByte=0; emits
		// 0-52=-52 (=0xcc) then n=0x80.
		{128, []byte{0xcc, 0x80}},
		// n=-33: case 1 (smallest len) falls through. firstByte init=-33,
		// then >>=8 (sign-extending) = -1. case 2 fit: -1 ∈ [-20, 20).
		// Header = -1-52 = -53 = 0xcb; tail byte = -33 & 0xff = 0xdf.
		{-33, []byte{0xcb, 0xdf}},
		// n=20*256 - 1 = 5119. firstByte init=19. case 2 fit: 19 ∈ [-20,20).
		// Header = 19-52 = -33 = 0xdf; tail = 0xff.
		{5119, []byte{0xdf, 0xff}},
		// n=-20*256 = -5120. firstByte init=-20. case 2 fit: -20 ∈ [-20,20).
		// Header = -20-52 = -72 = 0xb8; tail = 0x00.
		{-5120, []byte{0xb8, 0x00}},
	}
	for _, c := range cases {
		var buf bytes.Buffer
		if _, err := WriteVLong(&buf, c.v); err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(buf.Bytes(), c.want) {
			t.Errorf("WriteVLong(%d) = %x, want %x", c.v, buf.Bytes(), c.want)
		}
		// And decode back
		got, _, err := ReadVLong(bytes.NewReader(c.want))
		if err != nil {
			t.Fatal(err)
		}
		if got != c.v {
			t.Errorf("ReadVLong(%x) = %d, want %d", c.want, got, c.v)
		}
	}
}
