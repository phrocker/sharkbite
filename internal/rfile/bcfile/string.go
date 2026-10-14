package bcfile

import (
	"errors"
	"fmt"
	"io"
)

// ErrCorruptString flags malformed string encodings (e.g. negative length
// other than the -1 nil sentinel).
var ErrCorruptString = errors.New("bcfile: corrupted string encoding")

// ReadString deserializes a Java/Hadoop-Text-style string: a BCFile vint
// length followed by `length` raw UTF-8 bytes. Returns ("", false, nil) for
// the explicit null sentinel (length == -1) — the only way to encode a
// real null reference. Empty strings (length == 0) return ("", true, nil).
//
// Hadoop's Text uses strict UTF-8; we don't validate (Go strings are
// arbitrary bytes), so a malformed-UTF-8 BCFile would parse here but
// blow up in the Java reader. This is by design — we surface bytes
// faithfully and let consumers validate if they care.
func ReadString(r ByteAndReader) (s string, ok bool, err error) {
	length, _, err := ReadVInt(r)
	if err != nil {
		return "", false, fmt.Errorf("bcfile: read string length: %w", err)
	}
	if length == -1 {
		return "", false, nil
	}
	if length < 0 {
		return "", false, fmt.Errorf("%w: length %d", ErrCorruptString, length)
	}
	if remaining, ok := r.(interface{ Len() int }); ok && int64(length) > int64(remaining.Len()) {
		return "", false, fmt.Errorf("%w: length %d exceeds remaining %d bytes",
			ErrCorruptString, length, remaining.Len())
	}
	buf := make([]byte, length)
	if _, err := io.ReadFull(r, buf); err != nil {
		return "", false, fmt.Errorf("bcfile: read string body (%d bytes): %w", length, err)
	}
	return string(buf), true, nil
}

// WriteString writes s in BCFile wire form: vint length + raw bytes. To
// emit the null sentinel, call WriteNullString.
func WriteString(w io.Writer, s string) error {
	bw := byteWriterShim{w: w}
	if _, err := WriteVInt(bw, int32(len(s))); err != nil {
		return err
	}
	if _, err := w.Write([]byte(s)); err != nil {
		return err
	}
	return nil
}

// WriteNullString writes the BCFile null-string sentinel (vint == -1).
func WriteNullString(w io.Writer) error {
	bw := byteWriterShim{w: w}
	_, err := WriteVInt(bw, -1)
	return err
}

// ByteAndReader is the joint interface our parsers need: ReadByte for
// varints, plus io.Reader for fixed-length tails. *bytes.Reader and
// *bufio.Reader both satisfy it; tests usually use bytes.Reader.
type ByteAndReader interface {
	io.Reader
	io.ByteReader
}

// byteWriterShim adapts an io.Writer into an io.ByteWriter so the varint
// writer can target a plain io.Writer without callers having to wrap.
type byteWriterShim struct{ w io.Writer }

func (s byteWriterShim) WriteByte(b byte) error {
	_, err := s.w.Write([]byte{b})
	return err
}
