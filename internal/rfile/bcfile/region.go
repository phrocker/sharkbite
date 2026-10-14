package bcfile

import (
	"fmt"
	"io"
)

const (
	// MaxCompressedBlockSize bounds a single on-disk block allocation.
	MaxCompressedBlockSize int64 = 256 * 1024 * 1024
	// MaxRawBlockSize bounds a single decompressed block allocation.
	MaxRawBlockSize int64 = 512 * 1024 * 1024
)

// BlockRegion identifies one compressed region inside a BCFile: where it
// starts (Offset), how many bytes it occupies on disk (CompressedSize),
// and how many bytes it expands to (RawSize). Mirrors BCFile.BlockRegion.
//
// Used for both data blocks (listed in DataIndex) and meta blocks
// (referenced by MetaIndexEntry).
type BlockRegion struct {
	Offset         int64
	CompressedSize int64
	RawSize        int64
}

// ValidateBlockRegion rejects regions that cannot be safely read and
// decompressed from a file of fileLength bytes.
func ValidateBlockRegion(region BlockRegion, fileLength int64) error {
	if fileLength < 0 {
		return fmt.Errorf("bcfile: negative file length %d", fileLength)
	}
	if region.Offset < 0 {
		return fmt.Errorf("bcfile: negative block offset %d", region.Offset)
	}
	if region.CompressedSize < 0 {
		return fmt.Errorf("bcfile: negative CompressedSize %d", region.CompressedSize)
	}
	if region.RawSize < 0 {
		return fmt.Errorf("bcfile: negative RawSize %d", region.RawSize)
	}
	if region.CompressedSize > MaxCompressedBlockSize {
		return fmt.Errorf("bcfile: compressed block size %d exceeds %d-byte limit",
			region.CompressedSize, MaxCompressedBlockSize)
	}
	if region.RawSize > MaxRawBlockSize {
		return fmt.Errorf("bcfile: raw block size %d exceeds %d-byte limit",
			region.RawSize, MaxRawBlockSize)
	}
	if region.Offset > fileLength || region.CompressedSize > fileLength-region.Offset {
		return fmt.Errorf("bcfile: block region out of bounds: offset=%d size=%d fileLen=%d",
			region.Offset, region.CompressedSize, fileLength)
	}
	return nil
}

// ReadBlockRegion deserializes a BlockRegion: three back-to-back BCFile
// vlongs (offset, compressedSize, rawSize).
func ReadBlockRegion(r ByteAndReader) (BlockRegion, error) {
	off, _, err := ReadVLong(r)
	if err != nil {
		return BlockRegion{}, fmt.Errorf("BlockRegion offset: %w", err)
	}
	csz, _, err := ReadVLong(r)
	if err != nil {
		return BlockRegion{}, fmt.Errorf("BlockRegion compressedSize: %w", err)
	}
	rsz, _, err := ReadVLong(r)
	if err != nil {
		return BlockRegion{}, fmt.Errorf("BlockRegion rawSize: %w", err)
	}
	return BlockRegion{Offset: off, CompressedSize: csz, RawSize: rsz}, nil
}

// WriteBlockRegion serializes a BlockRegion in BCFile wire form.
func WriteBlockRegion(w io.Writer, br BlockRegion) error {
	bw := byteWriterShim{w: w}
	if _, err := WriteVLong(bw, br.Offset); err != nil {
		return err
	}
	if _, err := WriteVLong(bw, br.CompressedSize); err != nil {
		return err
	}
	if _, err := WriteVLong(bw, br.RawSize); err != nil {
		return err
	}
	return nil
}
