package bcfile

import (
	"errors"
	"fmt"
	"io"
	"math/bits"
)

// BCFile uses a custom variable-length integer encoding (NOT Hadoop
// WritableUtils-style — see rfile/varint.go for that). Encoding rules,
// transcribed from Utils.writeVLong (BCFile.java):
//
//	1 byte  : -32 ≤ n < 128            firstByte = n
//	2 bytes : -20*2^8 ≤ n < 20*2^8     firstByte = (n>>8) - 52, then n&0xff
//	3 bytes : -16*2^16 ≤ n < 16*2^16   firstByte = (n>>16) - 88, then 2-byte BE
//	4 bytes : -8*2^24 ≤ n < 8*2^24     firstByte = (n>>24) - 112, then 3-byte BE
//	5 bytes : firstByte = -125 (= 5-129),  then 4-byte BE int
//	6 bytes : firstByte = -124,            then 5-byte BE
//	7 bytes : firstByte = -123,            then 6-byte BE
//	8 bytes : firstByte = -122,            then 7-byte BE
//	9 bytes : firstByte = -121,            then 8-byte BE long
//
// Decode: peek firstByte, dispatch on signed range.
//
// Magnitude bytes are big-endian. Sign-extension happens implicitly because
// the writer subtracts a positive bias (52/88/112) from a signed firstByte
// — the reader undoes the same subtraction, treating firstByte as signed.

// ErrCorruptVInt is returned when a varint header byte is invalid. (The
// only firstByte values that aren't valid headers are those for which
// firstByte+128 is negative — impossible since firstByte is a byte cast
// to signed int8.)
var ErrCorruptVInt = errors.New("bcfile: corrupted vint encoding")

// ReadVLong decodes a BCFile-style variable-length signed long from r.
// Returns (value, bytesConsumed, error).
func ReadVLong(r io.ByteReader) (int64, int, error) {
	first, err := r.ReadByte()
	if err != nil {
		return 0, 0, err
	}
	fb := int8(first)
	if fb >= -32 {
		return int64(fb), 1, nil
	}
	// Java's switch ((firstByte+128)/8) — fb in [-128..-33] maps to bucket 0..11.
	bucket := (int(fb) + 128) / 8
	switch bucket {
	case 11, 10, 9, 8, 7:
		// 2 bytes: ((fb+52)<<8) | next
		b, err := r.ReadByte()
		if err != nil {
			return 0, 1, fmt.Errorf("bcfile vint byte 1: %w", err)
		}
		v := (int64(fb) + 52) << 8
		v |= int64(b)
		return v, 2, nil
	case 6, 5, 4, 3:
		// 3 bytes: ((fb+88)<<16) | next2
		var buf [2]byte
		if err := readFull(r, buf[:]); err != nil {
			return 0, 1, fmt.Errorf("bcfile vint 3-byte tail: %w", err)
		}
		v := (int64(fb) + 88) << 16
		v |= int64(buf[0])<<8 | int64(buf[1])
		return v, 3, nil
	case 2, 1:
		// 4 bytes: ((fb+112)<<24) | (next2<<8) | next1
		var buf [3]byte
		if err := readFull(r, buf[:]); err != nil {
			return 0, 1, fmt.Errorf("bcfile vint 4-byte tail: %w", err)
		}
		v := (int64(fb) + 112) << 24
		v |= int64(buf[0])<<16 | int64(buf[1])<<8 | int64(buf[2])
		return v, 4, nil
	case 0:
		// 5..9 bytes: len = fb+129 ∈ [4..8], read len bytes BE as signed
		dataLen := int(fb) + 129
		if dataLen < 4 || dataLen > 8 {
			return 0, 1, fmt.Errorf("%w: len byte %d out of [4..8]", ErrCorruptVInt, dataLen)
		}
		buf := make([]byte, dataLen)
		if err := readFull(r, buf); err != nil {
			return 0, 1, fmt.Errorf("bcfile vint %d-byte tail: %w", dataLen+1, err)
		}
		// Big-endian, signed: top bit of buf[0] sets sign via shift in int64.
		var v int64
		// Sign-extend the high byte first so negative values come out right.
		v = int64(int8(buf[0]))
		for i := 1; i < dataLen; i++ {
			v = (v << 8) | int64(buf[i])
		}
		return v, 1 + dataLen, nil
	default:
		return 0, 1, fmt.Errorf("%w: firstByte %d", ErrCorruptVInt, fb)
	}
}

// ReadVInt is ReadVLong with a 32-bit overflow check. Mirrors
// Utils.readVInt (which throws if the decoded long doesn't fit in int32).
func ReadVInt(r io.ByteReader) (int32, int, error) {
	v, n, err := ReadVLong(r)
	if err != nil {
		return 0, n, err
	}
	if v > int64(int32(^uint32(0)>>1)) || v < int64(int32(-1<<31)) {
		return 0, n, fmt.Errorf("bcfile: vint %d does not fit in int32", v)
	}
	return int32(v), n, nil
}

// WriteVLong encodes n in BCFile-style and writes it to w. Returns bytes
// written. Mirrors Utils.writeVLong.
func WriteVLong(w io.ByteWriter, n int64) (int, error) {
	if n < 128 && n >= -32 {
		return 1, w.WriteByte(byte(int8(n)))
	}
	// length: how many bytes does signed n need? + 1 sign byte.
	var un uint64
	if n < 0 {
		un = uint64(^n)
	} else {
		un = uint64(n)
	}
	// bits.Len64(0) == 0; n==0 already handled above.
	bitLen := bits.Len64(un)
	dataLen := bitLen/8 + 1 // matches Java: (Long.SIZE - leadingZeros) / 8 + 1
	first := int(n >> ((dataLen - 1) * 8))

	// Java uses fall-through to widen len when firstByte hits a range that
	// the smaller encoding can't cover.
	switch dataLen {
	case 1:
		// fall through to len=2 case with firstByte == -1 (sign-extended).
		first >>= 8
		fallthrough
	case 2:
		if first < 20 && first >= -20 {
			if err := w.WriteByte(byte(int8(first - 52))); err != nil {
				return 0, err
			}
			if err := w.WriteByte(byte(n)); err != nil {
				return 1, err
			}
			return 2, nil
		}
		first >>= 8
		fallthrough
	case 3:
		if first < 16 && first >= -16 {
			if err := w.WriteByte(byte(int8(first - 88))); err != nil {
				return 0, err
			}
			if err := w.WriteByte(byte(n >> 8)); err != nil {
				return 1, err
			}
			if err := w.WriteByte(byte(n)); err != nil {
				return 2, err
			}
			return 3, nil
		}
		first >>= 8
		fallthrough
	case 4:
		if first < 8 && first >= -8 {
			if err := w.WriteByte(byte(int8(first - 112))); err != nil {
				return 0, err
			}
			if err := w.WriteByte(byte(n >> 16)); err != nil {
				return 1, err
			}
			if err := w.WriteByte(byte(n >> 8)); err != nil {
				return 2, err
			}
			if err := w.WriteByte(byte(n)); err != nil {
				return 3, err
			}
			return 4, nil
		}
		// firstByte hits the "no fit" floor at len=4 ⇒ fall through to 5.
		dataLen = 4
		// header byte = len-129 = -125; write 4 magnitude bytes BE
		if err := w.WriteByte(byte(int8(dataLen - 129))); err != nil {
			return 0, err
		}
		for shift := 24; shift >= 0; shift -= 8 {
			if err := w.WriteByte(byte(n >> uint(shift))); err != nil {
				return 0, err
			}
		}
		return 5, nil
	default:
		// dataLen ∈ [5..8]
		if dataLen < 5 || dataLen > 8 {
			return 0, fmt.Errorf("bcfile: WriteVLong impossible len %d", dataLen)
		}
		if err := w.WriteByte(byte(int8(dataLen - 129))); err != nil {
			return 0, err
		}
		for shift := (dataLen - 1) * 8; shift >= 0; shift -= 8 {
			if err := w.WriteByte(byte(n >> uint(shift))); err != nil {
				return 0, err
			}
		}
		return dataLen + 1, nil
	}
}

// WriteVInt writes a 32-bit integer using the same encoding as WriteVLong.
func WriteVInt(w io.ByteWriter, n int32) (int, error) {
	return WriteVLong(w, int64(n))
}

// readFull reads exactly len(buf) bytes via repeated ReadByte calls. We
// can't use io.ReadFull because our parameter is an io.ByteReader, not
// an io.Reader (callers may pass *bytes.Reader or bufio.Reader, both of
// which satisfy both, but our API contract is the narrower ByteReader so
// the helpers compose freely with the rest of the package).
func readFull(r io.ByteReader, buf []byte) error {
	for i := range buf {
		b, err := r.ReadByte()
		if err != nil {
			return err
		}
		buf[i] = b
	}
	return nil
}
