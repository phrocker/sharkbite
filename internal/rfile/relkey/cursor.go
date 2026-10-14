package relkey

import "encoding/binary"

// rcursor is the hot-path byte cursor used during in-block decode. The
// existing Reader interface (io.Reader + io.ByteReader) costs an itab
// lookup per ReadByte call — measurable at >2M cells/sec — so the
// per-cell decode goes through this concrete struct instead. All methods
// are intentionally tiny + inlinable.
//
// The cursor's `buf` is the entire decompressed block. Slice helpers
// return 3-arg-clamped slices (`buf[pos:end:end]`) so callers can't
// `append` into the shared buffer.
type rcursor struct {
	buf []byte
	pos int
}

// byteAt returns the next byte and advances pos. ok=false on EOF.
func (c *rcursor) byteAt() (byte, bool) {
	if c.pos >= len(c.buf) {
		return 0, false
	}
	b := c.buf[c.pos]
	c.pos++
	return b, true
}

// sliceN returns the next n bytes as a slice into buf and advances pos.
// Cap-clamped so callers can't append into shared memory. n=0 returns
// an empty (non-nil) slice. ok=false on EOF or negative n.
func (c *rcursor) sliceN(n int) ([]byte, bool) {
	if n < 0 {
		return nil, false
	}
	end := c.pos + n
	if end > len(c.buf) {
		return nil, false
	}
	s := c.buf[c.pos:end:end]
	c.pos = end
	return s, true
}

// skip advances pos by n without returning a slice. ok=false on EOF or
// negative n. Used for filter-rejected cells: skip the value bytes
// without materializing them.
func (c *rcursor) skip(n int) bool {
	if n < 0 {
		return false
	}
	end := c.pos + n
	if end > len(c.buf) {
		return false
	}
	c.pos = end
	return true
}

// int32be reads a 4-byte big-endian int32. Compiles to a single bswap
// on amd64. RFile uses this for value lengths (RelativeKey.java
// readValue uses readInt, NOT a vint).
func (c *rcursor) int32be() (int32, bool) {
	if c.pos+4 > len(c.buf) {
		return 0, false
	}
	v := binary.BigEndian.Uint32(c.buf[c.pos:])
	c.pos += 4
	return int32(v), true
}

// vlong decodes a Hadoop-style varint at the current position. Mirrors
// wire.ReadVLong but operates directly off buf — no io.ByteReader
// dispatch. The 1-byte common case (most field lengths in compressed
// RFiles fall in [-112, 127]) is a single load + branch + add.
func (c *rcursor) vlong() (int64, bool) {
	if c.pos >= len(c.buf) {
		return 0, false
	}
	first := int8(c.buf[c.pos])
	c.pos++
	if first >= -112 {
		// Single-byte direct value — overwhelmingly the common case for
		// RFile field length prefixes (most are <128 bytes).
		return int64(first), true
	}
	var size int
	if first < -120 {
		size = -119 - int(first)
	} else {
		size = -111 - int(first)
	}
	mag := size - 1
	if c.pos+mag > len(c.buf) {
		return 0, false
	}
	var i int64
	for k := 0; k < mag; k++ {
		i = (i << 8) | int64(c.buf[c.pos+k])
	}
	c.pos += mag
	if first < -120 || (first >= -112 && first < 0) {
		i = ^i
	}
	return i, true
}

// vint is vlong with a 32-bit overflow guard. Used for length prefixes,
// which RFile encodes as int32-bounded vlongs.
func (c *rcursor) vint() (int32, bool) {
	v, ok := c.vlong()
	if !ok {
		return 0, false
	}
	if v > (1<<31-1) || v < -(1<<31) {
		return 0, false
	}
	return int32(v), true
}
