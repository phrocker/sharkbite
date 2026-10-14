package index

import (
	"fmt"
	"io"

	"github.com/phrocker/shoal-oss/internal/rfile/wire"
)

// IndexBlock is one node of a (possibly multi-level) RFile key index. We
// preserve the raw on-disk form — Offsets[] + Data[] — without walking
// the IndexEntry tree. Phase 3b's tree walker will lazily decode entries
// from Data using the offsets.
//
// Why opaque preservation? Sharkbite, Java, and the Accumulo cache all
// keep the serialized form so they can index-by-position into Data
// without re-decoding the whole block on every seek. We mirror that.
type IndexBlock struct {
	// Multi-level fields (v6/v7/v8). Level=0 is leaf (Entries point at
	// data blocks); higher levels' Entries point at sub-IndexBlocks.
	Level   int32
	Offset  int32 // count of entries before this block in the level (cumulative)
	HasNext bool

	// Offsets[i] = byte position within Data of the i'th entry's encoded
	// form. Lets random-access seek without sequentially decoding earlier
	// entries.
	Offsets []int32
	// Data is the concatenated serialized IndexEntries. Decode entry i
	// by seeking to Offsets[i] in this slice and calling ReadIndexEntry.
	Data []byte
}

// NumEntries returns the number of IndexEntries this block holds.
func (b *IndexBlock) NumEntries() int { return len(b.Offsets) }

// ReadIndexBlock deserializes an IndexBlock from r given the RFile
// version that produced the surrounding meta block. v3/v4 use a flat
// "list of IndexEntries" layout; v6/v7/v8 use the multi-level layout
// with explicit offsets. We support both.
func ReadIndexBlock(r wire.ByteAndReader, version int32) (*IndexBlock, error) {
	switch {
	case hasMultiLevelIndex(version):
		return readMultiLevelBlock(r)
	case version == V3 || version == V4:
		return readFlatBlock(r, version)
	default:
		return nil, fmt.Errorf("%w: %d", ErrUnsupportedVersion, version)
	}
}

func readMultiLevelBlock(r wire.ByteAndReader) (*IndexBlock, error) {
	level, err := wire.ReadInt32(r)
	if err != nil {
		return nil, fmt.Errorf("IndexBlock level: %w", err)
	}
	offset, err := wire.ReadInt32(r)
	if err != nil {
		return nil, fmt.Errorf("IndexBlock offset: %w", err)
	}
	hasNext, err := wire.ReadBool(r)
	if err != nil {
		return nil, fmt.Errorf("IndexBlock hasNext: %w", err)
	}
	numOffsets, err := wire.ReadInt32(r)
	if err != nil {
		return nil, fmt.Errorf("IndexBlock numOffsets: %w", err)
	}
	if numOffsets < 0 {
		return nil, fmt.Errorf("IndexBlock: negative numOffsets %d", numOffsets)
	}
	if err := validateDecodedCount(
		"IndexBlock offsets", numOffsets, r, maxIndexEntries, 4, 4,
	); err != nil {
		return nil, err
	}
	offsets := make([]int32, numOffsets)
	for i := int32(0); i < numOffsets; i++ {
		o, err := wire.ReadInt32(r)
		if err != nil {
			return nil, fmt.Errorf("IndexBlock offsets[%d]: %w", i, err)
		}
		offsets[i] = o
	}
	indexSize, err := wire.ReadInt32(r)
	if err != nil {
		return nil, fmt.Errorf("IndexBlock indexSize: %w", err)
	}
	if indexSize < 0 {
		return nil, fmt.Errorf("IndexBlock: negative indexSize %d", indexSize)
	}
	if err := validateIndexOffsets(offsets, indexSize); err != nil {
		return nil, err
	}
	if err := validateDecodedLength(
		"IndexBlock data", indexSize, r, maxIndexDataBytes, 0,
	); err != nil {
		return nil, err
	}
	data := make([]byte, indexSize)
	if _, err := io.ReadFull(r, data); err != nil {
		return nil, fmt.Errorf("IndexBlock data (%d bytes): %w", indexSize, err)
	}
	return &IndexBlock{
		Level:   level,
		Offset:  offset,
		HasNext: hasNext,
		Offsets: offsets,
		Data:    data,
	}, nil
}

func validateIndexOffsets(offsets []int32, indexSize int32) error {
	if len(offsets) == 0 {
		if indexSize != 0 {
			return fmt.Errorf("IndexBlock: %d data bytes have no offsets", indexSize)
		}
		return nil
	}
	if offsets[0] != 0 {
		return fmt.Errorf("IndexBlock: first offset %d is not zero", offsets[0])
	}
	for i, offset := range offsets {
		if offset < 0 || offset >= indexSize {
			return fmt.Errorf(
				"IndexBlock: offset[%d]=%d is outside [0,%d)", i, offset, indexSize)
		}
		if i > 0 && offset <= offsets[i-1] {
			return fmt.Errorf(
				"IndexBlock: offset[%d]=%d is not greater than offset[%d]=%d",
				i, offset, i-1, offsets[i-1])
		}
	}
	return nil
}

// readFlatBlock handles the v3/v4 layout: int32 size + size × IndexEntry.
// We materialize each entry's bytes back into an Offsets[]+Data[] form so
// the post-decode shape matches the multi-level case — that way the
// Phase 3b tree walker has only one structural variant to handle.
func readFlatBlock(r wire.ByteAndReader, version int32) (*IndexBlock, error) {
	size, err := wire.ReadInt32(r)
	if err != nil {
		return nil, fmt.Errorf("IndexBlock flat size: %w", err)
	}
	if size < 0 {
		return nil, fmt.Errorf("IndexBlock: negative flat size %d", size)
	}
	if err := validateDecodedCount(
		"IndexBlock flat entries", size, r, maxIndexEntries, 1, 0,
	); err != nil {
		return nil, err
	}
	// Decode each IndexEntry from the wire, then re-encode it into Data
	// at a known offset. Avoids dual-format handling downstream.
	out := &IndexBlock{
		Level:   0,
		Offset:  0,
		HasNext: false,
		Offsets: make([]int32, 0, size),
	}
	var data []byte
	for i := int32(0); i < size; i++ {
		entry, err := ReadIndexEntry(r)
		if err != nil {
			return nil, fmt.Errorf("flat IndexEntry %d: %w", i, err)
		}
		out.Offsets = append(out.Offsets, int32(len(data)))
		buf := &fixedBuffer{}
		if err := WriteIndexEntry(buf, entry); err != nil {
			return nil, fmt.Errorf("re-encode flat IndexEntry %d: %w", i, err)
		}
		data = append(data, buf.Bytes...)
	}
	out.Data = data
	_ = version // kept for future per-version branches
	return out, nil
}

// fixedBuffer is a minimal io.Writer that appends to an in-memory slice.
// Avoids pulling bytes.Buffer into the package's exported surface.
type fixedBuffer struct{ Bytes []byte }

func (b *fixedBuffer) Write(p []byte) (int, error) {
	b.Bytes = append(b.Bytes, p...)
	return len(p), nil
}

// WriteIndexBlock serializes an IndexBlock in the multi-level wire form.
// Used by tests; we don't write v3/v4 (no need — only readers care about
// older formats).
func WriteIndexBlock(w io.Writer, b *IndexBlock) error {
	if err := wire.WriteInt32(w, b.Level); err != nil {
		return err
	}
	if err := wire.WriteInt32(w, b.Offset); err != nil {
		return err
	}
	if err := wire.WriteBool(w, b.HasNext); err != nil {
		return err
	}
	if err := wire.WriteInt32(w, int32(len(b.Offsets))); err != nil {
		return err
	}
	for _, o := range b.Offsets {
		if err := wire.WriteInt32(w, o); err != nil {
			return err
		}
	}
	if err := wire.WriteInt32(w, int32(len(b.Data))); err != nil {
		return err
	}
	if len(b.Data) > 0 {
		if _, err := w.Write(b.Data); err != nil {
			return err
		}
	}
	return nil
}
