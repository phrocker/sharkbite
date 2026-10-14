package bcfile

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"sort"
)

const maxMetaIndexRegionSize int64 = 64 * 1024 * 1024

// Reader is a partial BCFile reader: it parses the trailer and meta-block
// index but stops short of decompressing data/meta blocks. The block
// decompressor lives in a sibling package (see `internal/rfile/bcfile/block`
// when it lands) and is layered on top: callers grab a BlockRegion from
// here and ask the decompressor to materialize it.
//
// Why split? Decompression pulls in codec libraries (snappy, zstd, gzip)
// that have transitive deps + version concerns. Keeping container parsing
// dependency-free lets callers test the read path against captured trailer
// bytes without ever installing a codec, and lets the codec layer evolve
// independently.
type Reader struct {
	src        io.ReaderAt
	fileLength int64

	footer    Footer
	metaIndex *MetaIndex
}

// ErrNoSuchMetaBlock is returned by MetaBlock when the requested name
// isn't present in the index.
var ErrNoSuchMetaBlock = errors.New("bcfile: no such meta block")

// ErrMetaIndexTooLarge indicates that a footer describes a MetaIndex region
// too large to materialize safely.
var ErrMetaIndexTooLarge = errors.New("bcfile: MetaIndex region too large")

// NewReader parses the BCFile trailer + MetaIndex from src. It does NOT
// touch the DataIndex or any data blocks (those are gated on a codec).
//
// The src must support io.ReaderAt; initialization reads the trailer tail,
// the footer offsets, and the bounded MetaIndex region.
func NewReader(src io.ReaderAt, fileLength int64) (*Reader, error) {
	footer, err := ReadFooter(src, fileLength)
	if err != nil {
		return nil, err
	}
	r := &Reader{src: src, fileLength: fileLength, footer: footer}

	// MetaIndex starts at footer.OffsetIndexMeta and runs up to the start
	// of the trailer (cryptoParams in v3, version+magic in v1). We don't
	// know the exact length on disk because it's variable-length encoded,
	// so we read a generous slice and parse just what we need. Cap the
	// slice at what's actually before the trailer.
	miEnd := r.metaIndexUpperBound()
	miLen := miEnd - footer.OffsetIndexMeta
	if miLen <= 0 {
		return nil, fmt.Errorf("bcfile: MetaIndex region empty (start=%d, end=%d)",
			footer.OffsetIndexMeta, miEnd)
	}
	if miLen > maxMetaIndexRegionSize {
		return nil, fmt.Errorf("%w: %d bytes exceeds %d-byte limit",
			ErrMetaIndexTooLarge, miLen, maxMetaIndexRegionSize)
	}
	miBuf := make([]byte, int(miLen))
	if _, err := src.ReadAt(miBuf, footer.OffsetIndexMeta); err != nil {
		return nil, fmt.Errorf("bcfile: read MetaIndex region: %w", err)
	}
	mi, err := ReadMetaIndex(bytes.NewReader(miBuf))
	if err != nil {
		return nil, err
	}
	for name, entry := range mi.Entries {
		if err := ValidateBlockRegion(entry.Region, footer.OffsetIndexMeta); err != nil {
			return nil, fmt.Errorf("bcfile: MetaIndex entry %q: %w", name, err)
		}
	}
	if err := validateMetaIndexRegions(mi); err != nil {
		return nil, err
	}
	r.metaIndex = mi
	return r, nil
}

func validateMetaIndexRegions(mi *MetaIndex) error {
	entries := make([]MetaIndexEntry, 0, len(mi.Entries))
	for _, entry := range mi.Entries {
		entries = append(entries, entry)
	}
	sort.Slice(entries, func(i, j int) bool {
		if entries[i].Region.Offset != entries[j].Region.Offset {
			return entries[i].Region.Offset < entries[j].Region.Offset
		}
		return entries[i].Name < entries[j].Name
	})
	for i := 1; i < len(entries); i++ {
		previous := entries[i-1]
		current := entries[i]
		if current.Region.Offset < previous.Region.Offset+previous.Region.CompressedSize {
			return fmt.Errorf("bcfile: MetaIndex entries %q and %q overlap",
				previous.Name, current.Name)
		}
	}
	return nil
}

// Footer returns the parsed BCFile trailer.
func (r *Reader) Footer() Footer { return r.footer }

// Source returns the underlying io.ReaderAt the Reader was constructed
// with. Higher-level layers (the RFile reader, the block decompressor)
// need direct access to issue their own ReadAt calls — exposing it here
// avoids forcing them to plumb the original source through alongside
// the *bcfile.Reader.
func (r *Reader) Source() io.ReaderAt { return r.src }

// FileLength returns the byte length of the underlying file.
func (r *Reader) FileLength() int64 { return r.fileLength }

// MetaIndex returns the parsed meta-block index.
func (r *Reader) MetaIndex() *MetaIndex { return r.metaIndex }

// DataRegionEnd returns the first byte reserved for meta blocks. Data
// blocks must end at or before this offset.
func (r *Reader) DataRegionEnd() int64 {
	end := r.footer.OffsetIndexMeta
	for _, entry := range r.metaIndex.Entries {
		if entry.Region.Offset < end {
			end = entry.Region.Offset
		}
	}
	return end
}

// MetaBlockEntry returns the index entry for the named meta block, or
// ErrNoSuchMetaBlock.
func (r *Reader) MetaBlockEntry(name string) (MetaIndexEntry, error) {
	e, ok := r.metaIndex.Lookup(name)
	if !ok {
		return MetaIndexEntry{}, fmt.Errorf("%w: %q", ErrNoSuchMetaBlock, name)
	}
	return e, nil
}

// RawBlock returns the on-disk (still-compressed) bytes of one block.
// Codec dispatch happens in the decompressor layer; callers that just
// want to feed bytes to a snappy/gzip/zstd reader use this. The slice
// is freshly allocated.
func (r *Reader) RawBlock(region BlockRegion) ([]byte, error) {
	if err := ValidateBlockRegion(region, r.fileLength); err != nil {
		return nil, err
	}
	size := int(region.CompressedSize)
	if int64(size) != region.CompressedSize {
		return nil, fmt.Errorf("bcfile: block size %d exceeds platform allocation limit",
			region.CompressedSize)
	}
	buf := make([]byte, size)
	if _, err := r.src.ReadAt(buf, region.Offset); err != nil {
		return nil, fmt.Errorf("bcfile: read block @ %d (%d bytes): %w",
			region.Offset, region.CompressedSize, err)
	}
	return buf, nil
}

// metaIndexUpperBound returns the byte offset where the trailer begins —
// i.e. the upper bound of the MetaIndex region.
func (r *Reader) metaIndexUpperBound() int64 {
	if r.footer.Version.CompatibleWith(APIVersion3) {
		return r.footer.OffsetCryptoParams
	}
	return r.fileLength - int64(FooterMinSizeV1)
}
