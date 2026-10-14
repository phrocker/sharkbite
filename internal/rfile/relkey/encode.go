package relkey

import (
	"bytes"
	"fmt"
	"io"

	"github.com/phrocker/shoal-oss/internal/rfile/wire"
)

// EncodeKey writes a single RelativeKey + value pair, threading off prev.
// Pass prev == nil for the first cell in a block.
//
// Mirrors RelativeKey.java:530-587 (write). Inverse of Reader.NextView.
func EncodeKey(w io.Writer, prev *Key, k *Key, value []byte) error {
	bw, ok := w.(io.ByteWriter)
	if !ok {
		bw = &byteWriterShim{w: w}
	}

	var fieldsSame, fieldsPrefixed byte
	var rowPrefix, cfPrefix, cqPrefix, cvPrefix int
	var tsDiff int64

	if prev != nil {
		rowPrefix = computeCommon(prev.Row, k.Row, RowSame, RowCommonPrefix, &fieldsSame, &fieldsPrefixed)
		cfPrefix = computeCommon(prev.ColumnFamily, k.ColumnFamily, CFSame, CFCommonPrefix, &fieldsSame, &fieldsPrefixed)
		cqPrefix = computeCommon(prev.ColumnQualifier, k.ColumnQualifier, CQSame, CQCommonPrefix, &fieldsSame, &fieldsPrefixed)
		cvPrefix = computeCommon(prev.ColumnVisibility, k.ColumnVisibility, CVSame, CVCommonPrefix, &fieldsSame, &fieldsPrefixed)
		tsDiff = k.Timestamp - prev.Timestamp
		if tsDiff == 0 {
			fieldsSame |= TSSame
		} else {
			fieldsPrefixed |= TSDiff
		}
		if fieldsPrefixed != 0 {
			fieldsSame |= PrefixCompressionEnabled
		}
	}
	if k.Deleted {
		fieldsSame |= Deleted
	}

	if err := bw.WriteByte(fieldsSame); err != nil {
		return err
	}
	if fieldsSame&PrefixCompressionEnabled == PrefixCompressionEnabled {
		if err := bw.WriteByte(fieldsPrefixed); err != nil {
			return err
		}
	}

	if err := writeField(bw, k.Row, rowPrefix, fieldsSame, fieldsPrefixed, RowSame, RowCommonPrefix); err != nil {
		return fmt.Errorf("row: %w", err)
	}
	if err := writeField(bw, k.ColumnFamily, cfPrefix, fieldsSame, fieldsPrefixed, CFSame, CFCommonPrefix); err != nil {
		return fmt.Errorf("cf: %w", err)
	}
	if err := writeField(bw, k.ColumnQualifier, cqPrefix, fieldsSame, fieldsPrefixed, CQSame, CQCommonPrefix); err != nil {
		return fmt.Errorf("cq: %w", err)
	}
	if err := writeField(bw, k.ColumnVisibility, cvPrefix, fieldsSame, fieldsPrefixed, CVSame, CVCommonPrefix); err != nil {
		return fmt.Errorf("cv: %w", err)
	}

	switch {
	case fieldsSame&TSSame == TSSame:
		// nothing
	case fieldsPrefixed&TSDiff == TSDiff:
		if _, err := wire.WriteVLong(bw, tsDiff); err != nil {
			return err
		}
	default:
		if _, err := wire.WriteVLong(bw, k.Timestamp); err != nil {
			return err
		}
	}

	// Value: int32 length BE + bytes.
	var hdr [4]byte
	n := int32(len(value))
	hdr[0] = byte(n >> 24)
	hdr[1] = byte(n >> 16)
	hdr[2] = byte(n >> 8)
	hdr[3] = byte(n)
	if _, err := w.Write(hdr[:]); err != nil {
		return err
	}
	if _, err := w.Write(value); err != nil {
		return err
	}
	return nil
}

// computeCommon mirrors RelativeKey.java:138-160 (getCommonPrefix) plus
// the SAME/PREFIX bit-setting in getCommonPrefixLen (123-132).
func computeCommon(prev, cur []byte, sameBit, prefixBit byte, fieldsSame, fieldsPrefixed *byte) int {
	if bytes.Equal(prev, cur) {
		*fieldsSame |= sameBit
		return -1
	}
	maxChecks := len(prev)
	if len(cur) < maxChecks {
		maxChecks = len(cur)
	}
	common := 0
	for common < maxChecks && prev[common] == cur[common] {
		common++
	}
	if common > 1 {
		*fieldsPrefixed |= prefixBit
		return common
	}
	return common
}

func writeField(bw io.ByteWriter, field []byte, commonLen int, fieldsSame, fieldsPrefixed, sameBit, prefixBit byte) error {
	if fieldsSame&sameBit == sameBit {
		return nil
	}
	if fieldsPrefixed&prefixBit == prefixBit {
		if _, err := wire.WriteVInt(bw, int32(commonLen)); err != nil {
			return err
		}
		if _, err := wire.WriteVInt(bw, int32(len(field)-commonLen)); err != nil {
			return err
		}
		if w, ok := bw.(io.Writer); ok {
			_, err := w.Write(field[commonLen:])
			return err
		}
		for _, b := range field[commonLen:] {
			if err := bw.WriteByte(b); err != nil {
				return err
			}
		}
		return nil
	}
	if _, err := wire.WriteVInt(bw, int32(len(field))); err != nil {
		return err
	}
	if w, ok := bw.(io.Writer); ok {
		_, err := w.Write(field)
		return err
	}
	for _, b := range field {
		if err := bw.WriteByte(b); err != nil {
			return err
		}
	}
	return nil
}

type byteWriterShim struct{ w io.Writer }

func (s *byteWriterShim) WriteByte(b byte) error {
	_, err := s.w.Write([]byte{b})
	return err
}
