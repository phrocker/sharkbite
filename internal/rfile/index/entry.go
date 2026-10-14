package index

import (
	"fmt"
	"io"

	"github.com/phrocker/shoal-oss/internal/rfile/bcfile"
	"github.com/phrocker/shoal-oss/internal/rfile/wire"
)

// IndexEntry points to one data block (or, at non-leaf levels of a
// multi-level index, to one child IndexBlock). The Key is the LAST key
// in the referenced block, used as a binary-search anchor.
//
// Mirrors core/.../MultiLevelIndex.java:50-136 (IndexEntry).
type IndexEntry struct {
	Key            *wire.Key
	NumEntries     int32
	Offset         int64 // BCFile offset of the referenced block
	CompressedSize int64
	RawSize        int64
}

// ReadIndexEntry deserializes an IndexEntry from r. The Key uses Hadoop
// vint lengths (wire.ReadKey); NumEntries is a 4-byte big-endian int32;
// Offset/CompressedSize/RawSize are BCFile vlongs (Utils.readVLong from
// core/.../bcfile/Utils.java — NOT Hadoop varint).
//
// "newFormat" branch only: every RFile we'd encounter on a 4.0 cluster
// uses newFormat (set by IndexBlock.readFields when version ≥ 4). The
// pre-newFormat branch (v3) sets offset/sizes to -1, which we don't
// support yet — see TODO below.
func ReadIndexEntry(r wire.ByteAndReader) (*IndexEntry, error) {
	key, err := wire.ReadKey(r)
	if err != nil {
		return nil, fmt.Errorf("IndexEntry key: %w", err)
	}
	entries, err := wire.ReadInt32(r)
	if err != nil {
		return nil, fmt.Errorf("IndexEntry entries: %w", err)
	}
	off, _, err := bcfile.ReadVLong(r)
	if err != nil {
		return nil, fmt.Errorf("IndexEntry offset: %w", err)
	}
	csz, _, err := bcfile.ReadVLong(r)
	if err != nil {
		return nil, fmt.Errorf("IndexEntry compressedSize: %w", err)
	}
	rsz, _, err := bcfile.ReadVLong(r)
	if err != nil {
		return nil, fmt.Errorf("IndexEntry rawSize: %w", err)
	}
	return &IndexEntry{
		Key:            key,
		NumEntries:     entries,
		Offset:         off,
		CompressedSize: csz,
		RawSize:        rsz,
	}, nil
}

// WriteIndexEntry serializes an IndexEntry. Inverse of ReadIndexEntry.
// Used by tests that build synthetic RFile.index meta blocks.
func WriteIndexEntry(w io.Writer, e *IndexEntry) error {
	if err := wire.WriteKey(w, e.Key); err != nil {
		return fmt.Errorf("IndexEntry key: %w", err)
	}
	if err := wire.WriteInt32(w, e.NumEntries); err != nil {
		return err
	}
	bw := byteWriterAdapter{w: w}
	if _, err := bcfile.WriteVLong(bw, e.Offset); err != nil {
		return err
	}
	if _, err := bcfile.WriteVLong(bw, e.CompressedSize); err != nil {
		return err
	}
	if _, err := bcfile.WriteVLong(bw, e.RawSize); err != nil {
		return err
	}
	return nil
}

// byteWriterAdapter promotes io.Writer → io.ByteWriter for the BCFile
// varint writer.
type byteWriterAdapter struct{ w io.Writer }

func (s byteWriterAdapter) WriteByte(b byte) error {
	_, err := s.w.Write([]byte{b})
	return err
}
