// Java DataInput / DataOutput primitives that the RFile.index format
// uses. Java DataOutput emits everything big-endian; bool is a single
// byte (0/non-0); writeUTF prefixes a 2-byte UNSIGNED big-endian length
// and a modified-UTF-8 body. We don't validate modified-UTF-8 (it's
// only relevant for codepoints ≥ U+10000, which RFile names won't
// contain) — UTF-8 bytes pass through as-is.
//
// Key.write / Key.readFields uses Hadoop WritableUtils varints (this
// package's WriteVLong / ReadVLong) for per-field lengths, then 8B
// timestamp and a bool deleted flag.
//
// References:
//   core/.../data/Key.java   — Key.write / readFields
//   java DataInput / DataOutput interfaces (java.io)
package wire

import (
	"encoding/binary"
	"fmt"
	"io"
)

// ReadInt32 reads a 4-byte big-endian signed int. Mirrors DataInput.readInt.
func ReadInt32(r io.Reader) (int32, error) {
	var buf [4]byte
	if _, err := io.ReadFull(r, buf[:]); err != nil {
		return 0, fmt.Errorf("rfile: readInt32: %w", err)
	}
	return int32(binary.BigEndian.Uint32(buf[:])), nil
}

// ReadInt64 reads an 8-byte big-endian signed long. Mirrors DataInput.readLong.
func ReadInt64(r io.Reader) (int64, error) {
	var buf [8]byte
	if _, err := io.ReadFull(r, buf[:]); err != nil {
		return 0, fmt.Errorf("rfile: readInt64: %w", err)
	}
	return int64(binary.BigEndian.Uint64(buf[:])), nil
}

// ReadBool reads a single byte: 0 → false, anything else → true.
// Mirrors DataInput.readBoolean.
func ReadBool(r io.Reader) (bool, error) {
	var buf [1]byte
	if _, err := io.ReadFull(r, buf[:]); err != nil {
		return false, fmt.Errorf("rfile: readBool: %w", err)
	}
	return buf[0] != 0, nil
}

// ReadUTF deserializes a Java DataOutput.writeUTF string: a 2-byte
// unsigned big-endian length followed by that many bytes of (modified)
// UTF-8. Returns the decoded string.
func ReadUTF(r io.Reader) (string, error) {
	var lenBuf [2]byte
	if _, err := io.ReadFull(r, lenBuf[:]); err != nil {
		return "", fmt.Errorf("rfile: readUTF length: %w", err)
	}
	length := binary.BigEndian.Uint16(lenBuf[:])
	if length == 0 {
		return "", nil
	}
	body := make([]byte, length)
	if _, err := io.ReadFull(r, body); err != nil {
		return "", fmt.Errorf("rfile: readUTF body (%d bytes): %w", length, err)
	}
	return string(body), nil
}

// WriteInt32 writes a 4-byte big-endian signed int.
func WriteInt32(w io.Writer, v int32) error {
	var buf [4]byte
	binary.BigEndian.PutUint32(buf[:], uint32(v))
	_, err := w.Write(buf[:])
	return err
}

// WriteInt64 writes an 8-byte big-endian signed long.
func WriteInt64(w io.Writer, v int64) error {
	var buf [8]byte
	binary.BigEndian.PutUint64(buf[:], uint64(v))
	_, err := w.Write(buf[:])
	return err
}

// WriteBool writes a single byte: false → 0, true → 1.
func WriteBool(w io.Writer, v bool) error {
	var b byte
	if v {
		b = 1
	}
	_, err := w.Write([]byte{b})
	return err
}

// WriteUTF writes a Java DataOutput.writeUTF-style string. Caller must
// ensure len(s) ≤ 65535 (the on-disk length field is uint16); longer
// strings are an error.
func WriteUTF(w io.Writer, s string) error {
	if len(s) > 0xffff {
		return fmt.Errorf("rfile: writeUTF: string too long (%d bytes; max %d)", len(s), 0xffff)
	}
	var lenBuf [2]byte
	binary.BigEndian.PutUint16(lenBuf[:], uint16(len(s)))
	if _, err := w.Write(lenBuf[:]); err != nil {
		return err
	}
	if _, err := w.Write([]byte(s)); err != nil {
		return err
	}
	return nil
}

// ReadKey deserializes a Key in the standalone Key.write wire format
// (core/.../data/Key.java:993-1014). The format uses CUMULATIVE OFFSETS
// — not per-field lengths — and a VLONG timestamp, NOT fixed int64.
// Earlier shoal builds got this wrong; reader/writer were symmetrically
// wrong so self-roundtrip passed but Java's reader caught it on the
// first cross-tool test.
//
//	vint colFamilyOffset       (= rowLen)
//	vint colQualifierOffset    (= rowLen + cfLen)
//	vint colVisibilityOffset   (= rowLen + cfLen + cqLen)
//	vint totalLen              (= rowLen + cfLen + cqLen + cvLen)
//	bytes row                  (length = colFamilyOffset)
//	bytes cf                   (length = colQualifierOffset - colFamilyOffset)
//	bytes cq                   (length = colVisibilityOffset - colQualifierOffset)
//	bytes cv                   (length = totalLen - colVisibilityOffset)
//	vlong timestamp
//	byte  deleted
//
// All byte slices are freshly allocated so the returned Key owns its
// storage independently of the source buffer.
func ReadKey(r ByteAndReader) (*Key, error) {
	cfOff, _, err := ReadVInt(r)
	if err != nil {
		return nil, fmt.Errorf("rfile: ReadKey cfOffset: %w", err)
	}
	cqOff, _, err := ReadVInt(r)
	if err != nil {
		return nil, fmt.Errorf("rfile: ReadKey cqOffset: %w", err)
	}
	cvOff, _, err := ReadVInt(r)
	if err != nil {
		return nil, fmt.Errorf("rfile: ReadKey cvOffset: %w", err)
	}
	total, _, err := ReadVInt(r)
	if err != nil {
		return nil, fmt.Errorf("rfile: ReadKey totalLen: %w", err)
	}
	if cfOff < 0 || cqOff < cfOff || cvOff < cqOff || total < cvOff {
		return nil, fmt.Errorf("rfile: ReadKey corrupt offsets [cf=%d cq=%d cv=%d total=%d]",
			cfOff, cqOff, cvOff, total)
	}

	row := make([]byte, cfOff)
	if _, err := io.ReadFull(r, row); err != nil {
		return nil, fmt.Errorf("rfile: ReadKey row (%d bytes): %w", cfOff, err)
	}
	cf := make([]byte, cqOff-cfOff)
	if _, err := io.ReadFull(r, cf); err != nil {
		return nil, fmt.Errorf("rfile: ReadKey cf (%d bytes): %w", cqOff-cfOff, err)
	}
	cq := make([]byte, cvOff-cqOff)
	if _, err := io.ReadFull(r, cq); err != nil {
		return nil, fmt.Errorf("rfile: ReadKey cq (%d bytes): %w", cvOff-cqOff, err)
	}
	cv := make([]byte, total-cvOff)
	if _, err := io.ReadFull(r, cv); err != nil {
		return nil, fmt.Errorf("rfile: ReadKey cv (%d bytes): %w", total-cvOff, err)
	}

	ts, _, err := ReadVLong(r)
	if err != nil {
		return nil, fmt.Errorf("rfile: ReadKey timestamp: %w", err)
	}
	del, err := ReadBool(r)
	if err != nil {
		return nil, fmt.Errorf("rfile: ReadKey deleted: %w", err)
	}
	return &Key{
		Row:              row,
		ColumnFamily:     cf,
		ColumnQualifier:  cq,
		ColumnVisibility: cv,
		Timestamp:        ts,
		Deleted:          del,
	}, nil
}

// WriteKey serializes a Key in the standalone Key.write wire format.
// See ReadKey for the layout. Inverse of ReadKey.
func WriteKey(w io.Writer, k *Key) error {
	cfOff := int32(len(k.Row))
	cqOff := cfOff + int32(len(k.ColumnFamily))
	cvOff := cqOff + int32(len(k.ColumnQualifier))
	total := cvOff + int32(len(k.ColumnVisibility))

	bw := byteWriterAdapter{w: w}
	if _, err := WriteVInt(bw, cfOff); err != nil {
		return fmt.Errorf("rfile: WriteKey cfOffset: %w", err)
	}
	if _, err := WriteVInt(bw, cqOff); err != nil {
		return fmt.Errorf("rfile: WriteKey cqOffset: %w", err)
	}
	if _, err := WriteVInt(bw, cvOff); err != nil {
		return fmt.Errorf("rfile: WriteKey cvOffset: %w", err)
	}
	if _, err := WriteVInt(bw, total); err != nil {
		return fmt.Errorf("rfile: WriteKey totalLen: %w", err)
	}
	if _, err := w.Write(k.Row); err != nil {
		return fmt.Errorf("rfile: WriteKey row: %w", err)
	}
	if _, err := w.Write(k.ColumnFamily); err != nil {
		return fmt.Errorf("rfile: WriteKey cf: %w", err)
	}
	if _, err := w.Write(k.ColumnQualifier); err != nil {
		return fmt.Errorf("rfile: WriteKey cq: %w", err)
	}
	if _, err := w.Write(k.ColumnVisibility); err != nil {
		return fmt.Errorf("rfile: WriteKey cv: %w", err)
	}
	if _, err := WriteVLong(bw, k.Timestamp); err != nil {
		return fmt.Errorf("rfile: WriteKey timestamp: %w", err)
	}
	return WriteBool(w, k.Deleted)
}

// ByteAndReader is the joint interface ReadKey + ReadV(Long|Int) need.
// Same shape as bcfile.ByteAndReader; we redeclare here to avoid the
// rfile package depending on bcfile.
type ByteAndReader interface {
	io.Reader
	io.ByteReader
}

// byteWriterAdapter promotes io.Writer to io.ByteWriter for the varint
// writers, which need ByteWriter. Defined here to keep the rfile package
// self-contained (a similar shim lives inside bcfile/string.go for
// the same reason).
type byteWriterAdapter struct{ w io.Writer }

func (s byteWriterAdapter) WriteByte(b byte) error {
	_, err := s.w.Write([]byte{b})
	return err
}
