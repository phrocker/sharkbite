package bcfile

import (
	"errors"
	"fmt"
	"io"
)

// Writer assembles a BCFile by streaming bytes to an io.Writer. Use
// model:
//
//	w := bcfile.NewWriter(out, "gz")
//	for each data block:  w.AppendDataBlock(rawCompressedBytes, rawSize)
//	for each meta block:  w.AppendMetaBlock(name, rawCompressedBytes, rawSize)
//	w.Close()                                      // writes BCFile.index, MetaIndex, trailer
//
// IMPORTANT: this Writer takes ALREADY-COMPRESSED bytes and the original
// raw size — it doesn't compress for you. Compression lives in
// internal/rfile/bcfile/block.Compressor; the rfile.Writer above us
// handles the codec call. Keeping bcfile.Writer codec-agnostic mirrors
// the reader split (bcfile.Reader → block.Decompressor) and lets the
// rfile.Writer choose codec per block.
//
// The Writer tracks byte offsets via a counting wrapper around the
// underlying writer so callers don't have to expose os.File.Seek.
type Writer struct {
	out                *countingWriter
	defaultCompression string

	dataBlocks []BlockRegion
	metaIndex  *MetaIndex

	closed bool
}

// NewWriter constructs a Writer over out. defaultCompression is recorded
// in the DataIndex; it doesn't constrain which codec individual data
// blocks were actually compressed with (the BCFile format trusts the
// writer to be consistent).
//
// Conventional use: callers compress all data blocks with this default
// codec and record per-meta-block codecs separately via AppendMetaBlock.
func NewWriter(out io.Writer, defaultCompression string) *Writer {
	return &Writer{
		out:                &countingWriter{w: out},
		defaultCompression: defaultCompression,
		metaIndex:          &MetaIndex{Entries: map[string]MetaIndexEntry{}},
	}
}

// AppendDataBlock writes one already-compressed data block to the file
// and records its position in the (in-memory) DataIndex. Returns the
// region the block landed at, in case the caller needs it for the
// RFile-level index.
func (w *Writer) AppendDataBlock(compressed []byte, rawSize int64) (BlockRegion, error) {
	if w.closed {
		return BlockRegion{}, errors.New("bcfile: writer closed")
	}
	if rawSize < 0 {
		return BlockRegion{}, fmt.Errorf("bcfile: negative rawSize %d", rawSize)
	}
	region := BlockRegion{
		Offset:         w.out.n,
		CompressedSize: int64(len(compressed)),
		RawSize:        rawSize,
	}
	if _, err := w.out.Write(compressed); err != nil {
		return BlockRegion{}, fmt.Errorf("bcfile: write data block: %w", err)
	}
	w.dataBlocks = append(w.dataBlocks, region)
	return region, nil
}

// AppendMetaBlock writes one already-compressed meta block to the file
// and registers it in the MetaIndex under the given name + codec. If a
// meta block with that name already exists, returns an error rather
// than silently overwriting (Java throws MetaBlockAlreadyExists).
func (w *Writer) AppendMetaBlock(name, codec string, compressed []byte, rawSize int64) (BlockRegion, error) {
	if w.closed {
		return BlockRegion{}, errors.New("bcfile: writer closed")
	}
	if _, exists := w.metaIndex.Entries[name]; exists {
		return BlockRegion{}, fmt.Errorf("bcfile: meta block %q already exists", name)
	}
	if rawSize < 0 {
		return BlockRegion{}, fmt.Errorf("bcfile: negative rawSize %d", rawSize)
	}
	region := BlockRegion{
		Offset:         w.out.n,
		CompressedSize: int64(len(compressed)),
		RawSize:        rawSize,
	}
	if _, err := w.out.Write(compressed); err != nil {
		return BlockRegion{}, fmt.Errorf("bcfile: write meta block: %w", err)
	}
	w.metaIndex.Entries[name] = MetaIndexEntry{
		Name:            name,
		CompressionAlgo: codec,
		Region:          region,
	}
	return region, nil
}

// Close finalizes the file: writes the BCFile.index meta block (the
// DataIndex), then the MetaIndex, then a placeholder crypto-params
// region, then the v3 trailer. Idempotent — second Close is a no-op.
//
// Caller is responsible for closing the underlying io.Writer afterwards
// if it's an *os.File or similar.
func (w *Writer) Close() error {
	if w.closed {
		return nil
	}
	w.closed = true

	// 1. Encode + register the DataIndex meta block. We don't compress
	// it — for V0 simplicity, the DataIndex meta block always uses
	// codec "none". Java compresses it with the default codec; either
	// way our reader handles it via the codec field in the MetaIndex
	// entry.
	di := &DataIndex{
		DefaultCompression: w.defaultCompression,
		Blocks:             w.dataBlocks,
	}
	var diBuf countingBuffer
	if err := WriteDataIndex(&diBuf, di); err != nil {
		return fmt.Errorf("bcfile.Close: encode DataIndex: %w", err)
	}
	diOffset := w.out.n
	if _, err := w.out.Write(diBuf.bytes); err != nil {
		return fmt.Errorf("bcfile.Close: write DataIndex block: %w", err)
	}
	w.metaIndex.Entries[DataIndexBlockName] = MetaIndexEntry{
		Name:            DataIndexBlockName,
		CompressionAlgo: CodecNone,
		Region: BlockRegion{
			Offset:         diOffset,
			CompressedSize: int64(len(diBuf.bytes)),
			RawSize:        int64(len(diBuf.bytes)),
		},
	}

	// 2. Write the MetaIndex itself. Its offset goes into the trailer.
	metaIndexOffset := w.out.n
	if err := WriteMetaIndex(w.out, w.metaIndex); err != nil {
		return fmt.Errorf("bcfile.Close: write MetaIndex: %w", err)
	}

	// 3. Crypto-params placeholder. v3 trailer expects an offset; we
	// write 8 placeholder bytes of zeros. The bcfile.Reader doesn't
	// actually parse this region (Phase 1 left it opaque), so any
	// content that matches the offset works.
	cryptoOffset := w.out.n
	if _, err := w.out.Write(make([]byte, 8)); err != nil {
		return fmt.Errorf("bcfile.Close: write crypto placeholder: %w", err)
	}

	// 4. Trailer.
	if err := WriteFooter(w.out, Footer{
		Version:            APIVersion3,
		OffsetIndexMeta:    metaIndexOffset,
		OffsetCryptoParams: cryptoOffset,
	}); err != nil {
		return fmt.Errorf("bcfile.Close: write trailer: %w", err)
	}
	return nil
}

// CodecNone mirrors block.CodecNone — re-declared here so bcfile.Writer
// doesn't import block (which would be a downward-circular dep).
const CodecNone = "none"

// countingWriter wraps an io.Writer and exposes the running byte count.
// We need this to record block offsets as we write — the alternative
// (Seek-able underlying) would force callers to use *os.File.
type countingWriter struct {
	w io.Writer
	n int64
}

func (c *countingWriter) Write(p []byte) (int, error) {
	n, err := c.w.Write(p)
	c.n += int64(n)
	return n, err
}

// countingBuffer is a tiny bytes.Buffer-equivalent used to capture the
// DataIndex serialization before writing it. Avoids importing bytes for
// just one buffer.
type countingBuffer struct {
	bytes []byte
}

func (b *countingBuffer) Write(p []byte) (int, error) {
	b.bytes = append(b.bytes, p...)
	return len(p), nil
}
