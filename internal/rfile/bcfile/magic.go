package bcfile

import (
	"bytes"
	"errors"
	"fmt"
	"io"
)

// MagicBytes are the 16-byte sentinel that BCFile writes at the very end of
// every file. Mirrors BCFile.Magic.AB_MAGIC_BCFILE.
var MagicBytes = [...]byte{
	0xd1, 0x11, 0xd3, 0x68, 0x91, 0xb5, 0xd7, 0xb6,
	0x39, 0xdf, 0x41, 0x40, 0x92, 0xba, 0xe1, 0x50,
}

// MagicSize is the magic length in bytes (16).
const MagicSize = len(MagicBytes)

// ErrBadMagic indicates the file does not start (when read backwards from
// the trailer) with the BCFile magic — i.e. it's not a BCFile.
var ErrBadMagic = errors.New("bcfile: not a valid BCFile (magic mismatch)")

// VerifyMagic reads MagicSize bytes from r and returns ErrBadMagic if
// they don't match the BCFile sentinel.
func VerifyMagic(r io.Reader) error {
	var buf [MagicSize]byte
	if _, err := io.ReadFull(r, buf[:]); err != nil {
		return fmt.Errorf("bcfile: read magic: %w", err)
	}
	if !bytes.Equal(buf[:], MagicBytes[:]) {
		return ErrBadMagic
	}
	return nil
}

// WriteMagic writes the BCFile magic to w. Used by tests / round-trippers.
func WriteMagic(w io.Writer) error {
	_, err := w.Write(MagicBytes[:])
	return err
}
