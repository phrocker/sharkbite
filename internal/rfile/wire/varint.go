// Hadoop WritableUtils-style variable-length integer codec. RFile stores
// every length prefix and most numeric fields with this encoding, so the
// RelativeKey decoder and any future block reader needs a faithful port.
//
// Reference: Hadoop's org.apache.hadoop.io.WritableUtils
//   - readVLong / writeVLong / decodeVIntSize / isNegativeVInt
//
// Encoding (all values stored sign-extended in two's complement):
//
//	-112 <= v <= 127         : 1 byte, the value itself
//	v in [-2^15, -113] u [128, 2^15-1] etc. : 1 first byte + 1..8 magnitude bytes
//
// First-byte ranges:
//
//	[-112, 127]   single-byte direct value
//	[-120, -113]  positive >127, magnitude bytes = -firstByte - 112
//	[-128, -121]  negative <-112, magnitude bytes = -firstByte - 120,
//	              encoded magnitude is ^v (bitwise NOT)
//
// Magnitude bytes are big-endian. We deliberately diverge from sharkbite's
// rkey.cpp readEncodedVLong, which decides sign from the *accumulated* `i`
// rather than the first byte (see InputStream.h:298-318). That works in
// practice but is not the algorithm Hadoop documents; the Java RFile reader
// (WritableUtils.readVLong) gates the `^-1` flip on the first byte, so we do
// the same to stay byte-for-byte identical to RelativeKey.java:189-192.
package wire

import (
	"errors"
	"fmt"
	"io"
)

// ErrVIntOverflow is returned when a varint header advertises more bytes
// than can fit into an int64 (>9 bytes total).
var ErrVIntOverflow = errors.New("rfile: vint overflow")

// DecodeVIntSize returns the total encoded length (1..9) given the leading
// byte. Mirrors WritableUtils.decodeVIntSize.
func DecodeVIntSize(first byte) int {
	v := int8(first)
	if v >= -112 {
		return 1
	}
	if v < -120 {
		return -119 - int(v)
	}
	return -111 - int(v)
}

// IsNegativeVInt reports whether the first byte of an encoded varint
// indicates a negative value. Mirrors WritableUtils.isNegativeVInt.
func IsNegativeVInt(first byte) bool {
	v := int8(first)
	return v < -120 || (v >= -112 && v < 0)
}

// ReadVLong decodes a Hadoop-style variable-length signed long from r.
// Returns the value and the number of bytes consumed.
func ReadVLong(r io.ByteReader) (int64, int, error) {
	first, err := r.ReadByte()
	if err != nil {
		return 0, 0, err
	}
	size := DecodeVIntSize(first)
	if size == 1 {
		return int64(int8(first)), 1, nil
	}
	if size > 9 {
		return 0, 0, ErrVIntOverflow
	}
	var i int64
	for idx := 0; idx < size-1; idx++ {
		b, err := r.ReadByte()
		if err != nil {
			return 0, 0, fmt.Errorf("vint magnitude byte %d: %w", idx, err)
		}
		i = (i << 8) | int64(b)
	}
	if IsNegativeVInt(first) {
		i = ^i
	}
	return i, size, nil
}

// ReadVInt is ReadVLong with a 32-bit overflow check, used for length
// prefixes. Mirrors WritableUtils.readVInt.
func ReadVInt(r io.ByteReader) (int32, int, error) {
	v, n, err := ReadVLong(r)
	if err != nil {
		return 0, n, err
	}
	if v > int64(int32(^uint32(0)>>1)) || v < int64(int32(-1<<31)) {
		return 0, n, fmt.Errorf("rfile: vint %d does not fit in int32", v)
	}
	return int32(v), n, nil
}

// WriteVLong encodes v in Hadoop varint form and writes it to w. Returns
// the number of bytes written. Mirrors WritableUtils.writeVLong.
func WriteVLong(w io.ByteWriter, v int64) (int, error) {
	if v >= -112 && v <= 127 {
		if err := w.WriteByte(byte(int8(v))); err != nil {
			return 0, err
		}
		return 1, nil
	}
	// Choose encoding bias: positive values use -112, negatives use -120 and
	// flip the magnitude.
	first := -112
	mag := v
	if v < 0 {
		mag = ^v
		first = -120
	}
	// Find the smallest number of magnitude bytes needed.
	tmp := mag
	bytesNeeded := 0
	for tmp != 0 {
		tmp >>= 8
		bytesNeeded++
	}
	first -= bytesNeeded
	if err := w.WriteByte(byte(int8(first))); err != nil {
		return 0, err
	}
	written := 1
	// Big-endian magnitude bytes.
	for i := bytesNeeded - 1; i >= 0; i-- {
		if err := w.WriteByte(byte((mag >> uint(8*i)) & 0xff)); err != nil {
			return written, err
		}
		written++
	}
	return written, nil
}

// WriteVInt is WriteVLong specialized for int32. Mirrors
// WritableUtils.writeVInt — the wire format is identical to writeVLong for
// any value that fits in int32.
func WriteVInt(w io.ByteWriter, v int32) (int, error) {
	return WriteVLong(w, int64(v))
}
