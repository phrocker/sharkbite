package bcfile

import (
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
)

// DataIndexBlockName is the meta-block name under which BCFile stores the
// DataIndex itself (a meta block containing the data-block list). Java:
// BCFile.DataIndex.BLOCK_NAME = "BCFile.index".
const DataIndexBlockName = "BCFile.index"

// maxMetaIndexEntries bounds decoded map growth independently of the encoded
// MetaIndex region size. Production BCFiles use only a small set of metadata
// blocks; this limit leaves ample extension room while bounding hostile input.
const maxMetaIndexEntries int32 = 1 << 16

// maxDataIndexBlocks bounds decoded index memory independently of the
// compressed meta-block size. One million blocks already describes RFiles
// far larger than practical production files while keeping the region slice
// below 24 MiB.
const maxDataIndexBlocks int32 = 1 << 20

// metaNamePrefix is prepended to every MetaIndexEntry's user-facing name
// when serialized. The Java reader strips it on read; we do the same.
// Keeping this internal — exported names are always prefix-free.
const metaNamePrefix = "data:"

// MetaIndexEntry names one meta block: which BlockRegion it lives at,
// what compression algorithm it uses, and its (prefix-stripped) name.
type MetaIndexEntry struct {
	Name            string // user-facing name, prefix already stripped (e.g. "BCFile.index", "RFile.index")
	CompressionAlgo string // codec name as written by the producer (e.g. "gz", "snappy", "none")
	Region          BlockRegion
}

// MetaIndex is the deserialized meta-block index: a map keyed by the
// (stripped) meta-block name. Java uses a TreeMap (sorted order) for
// deterministic write; the Go map is fine for reads since the writer
// re-sorts on serialize.
type MetaIndex struct {
	Entries map[string]MetaIndexEntry
}

// ErrMissingDataPrefix is returned when a meta-block name on disk does
// not begin with "data:" — Java treats this as a corrupt index.
var ErrMissingDataPrefix = errors.New("bcfile: meta entry missing 'data:' prefix")

// ReadMetaIndex deserializes a MetaIndex: vint count + N entries.
// Each entry is `data:` + name string, codec name string, BlockRegion.
func ReadMetaIndex(r ByteAndReader) (*MetaIndex, error) {
	count, _, err := ReadVInt(r)
	if err != nil {
		return nil, fmt.Errorf("MetaIndex count: %w", err)
	}
	if count < 0 {
		return nil, fmt.Errorf("bcfile: negative MetaIndex count %d", count)
	}
	if count > maxMetaIndexEntries {
		return nil, fmt.Errorf("bcfile: MetaIndex entry count %d exceeds %d-entry limit",
			count, maxMetaIndexEntries)
	}
	out := &MetaIndex{Entries: make(map[string]MetaIndexEntry, int(count))}
	for i := int32(0); i < count; i++ {
		entry, err := readMetaIndexEntry(r)
		if err != nil {
			return nil, fmt.Errorf("MetaIndex entry %d: %w", i, err)
		}
		if _, exists := out.Entries[entry.Name]; exists {
			return nil, fmt.Errorf("bcfile: duplicate MetaIndex entry %q", entry.Name)
		}
		out.Entries[entry.Name] = entry
	}
	return out, nil
}

func readMetaIndexEntry(r ByteAndReader) (MetaIndexEntry, error) {
	full, ok, err := ReadString(r)
	if err != nil {
		return MetaIndexEntry{}, fmt.Errorf("entry name: %w", err)
	}
	if !ok {
		return MetaIndexEntry{}, errors.New("bcfile: null meta entry name")
	}
	if !strings.HasPrefix(full, metaNamePrefix) {
		return MetaIndexEntry{}, fmt.Errorf("%w: got %q", ErrMissingDataPrefix, full)
	}
	algo, ok, err := ReadString(r)
	if err != nil {
		return MetaIndexEntry{}, fmt.Errorf("compression name: %w", err)
	}
	if !ok {
		return MetaIndexEntry{}, errors.New("bcfile: null compression algorithm")
	}
	region, err := ReadBlockRegion(r)
	if err != nil {
		return MetaIndexEntry{}, err
	}
	return MetaIndexEntry{
		Name:            strings.TrimPrefix(full, metaNamePrefix),
		CompressionAlgo: algo,
		Region:          region,
	}, nil
}

// WriteMetaIndex serializes a MetaIndex. Entries are sorted by name to
// match Java's TreeMap iteration order — important for byte-exact roundtrips.
func WriteMetaIndex(w io.Writer, mi *MetaIndex) error {
	bw := byteWriterShim{w: w}
	if _, err := WriteVInt(bw, int32(len(mi.Entries))); err != nil {
		return err
	}
	names := make([]string, 0, len(mi.Entries))
	for name := range mi.Entries {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		entry := mi.Entries[name]
		if err := WriteString(w, metaNamePrefix+entry.Name); err != nil {
			return err
		}
		if err := WriteString(w, entry.CompressionAlgo); err != nil {
			return err
		}
		if err := WriteBlockRegion(w, entry.Region); err != nil {
			return err
		}
	}
	return nil
}

// Lookup returns the meta-block entry by name (no prefix). false if absent.
func (mi *MetaIndex) Lookup(name string) (MetaIndexEntry, bool) {
	e, ok := mi.Entries[name]
	return e, ok
}

// DataIndex enumerates every data block in the BCFile, plus the codec
// they share. Java: BCFile.DataIndex. Stored on disk INSIDE a meta block
// named "BCFile.index" (DataIndexBlockName).
type DataIndex struct {
	DefaultCompression string        // e.g. "gz", "snappy", "none"
	Blocks             []BlockRegion // ordered by file offset (writer guarantees)
}

// ReadDataIndex deserializes a DataIndex from r (the *uncompressed*
// contents of the BCFile.index meta block).
func ReadDataIndex(r ByteAndReader) (*DataIndex, error) {
	algo, ok, err := ReadString(r)
	if err != nil {
		return nil, fmt.Errorf("DataIndex compression name: %w", err)
	}
	if !ok {
		return nil, errors.New("bcfile: null DataIndex compression algorithm")
	}
	count, _, err := ReadVInt(r)
	if err != nil {
		return nil, fmt.Errorf("DataIndex block count: %w", err)
	}
	if count < 0 {
		return nil, fmt.Errorf("bcfile: negative DataIndex block count %d", count)
	}
	if count > maxDataIndexBlocks {
		return nil, fmt.Errorf("bcfile: DataIndex block count %d exceeds %d-block limit",
			count, maxDataIndexBlocks)
	}
	blocks := make([]BlockRegion, 0, int(count))
	for i := int32(0); i < count; i++ {
		region, err := ReadBlockRegion(r)
		if err != nil {
			return nil, fmt.Errorf("DataIndex block %d: %w", i, err)
		}
		blocks = append(blocks, region)
	}
	return &DataIndex{DefaultCompression: algo, Blocks: blocks}, nil
}

// WriteDataIndex serializes a DataIndex to w.
func WriteDataIndex(w io.Writer, di *DataIndex) error {
	if err := WriteString(w, di.DefaultCompression); err != nil {
		return err
	}
	bw := byteWriterShim{w: w}
	if _, err := WriteVInt(bw, int32(len(di.Blocks))); err != nil {
		return err
	}
	for i, region := range di.Blocks {
		if err := WriteBlockRegion(w, region); err != nil {
			return fmt.Errorf("DataIndex block %d: %w", i, err)
		}
	}
	return nil
}
