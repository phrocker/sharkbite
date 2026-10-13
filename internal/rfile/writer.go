package rfile

import (
	"bytes"
	"errors"
	"fmt"
	"io"

	"github.com/phrocker/shoal-oss/internal/embeddingspace"
	"github.com/phrocker/shoal-oss/internal/rfile/adjacency"
	"github.com/phrocker/shoal-oss/internal/rfile/bcfile"
	"github.com/phrocker/shoal-oss/internal/rfile/bcfile/block"
	"github.com/phrocker/shoal-oss/internal/rfile/blockmeta"
	"github.com/phrocker/shoal-oss/internal/rfile/index"
	"github.com/phrocker/shoal-oss/internal/rfile/relkey"
	"github.com/phrocker/shoal-oss/internal/rfile/wire"
)

// DefaultBlockSize is the per-data-block target byte size for newly
// produced RFiles. Mirrors Java's default of 100KB
// (Property.TABLE_FILE_COMPRESSED_BLOCK_SIZE). The writer flushes a
// block once its accumulated raw bytes reach or exceed this threshold.
const DefaultBlockSize = 100 * 1024

// DefaultIndexBlockSize is the per-index-block target byte size for
// the multi-level index. Mirrors Java's default of 128KB
// (Property.TABLE_FILE_COMPRESSED_BLOCK_SIZE_INDEX). Once a level's
// accumulated entries (their serialized bytes plus the int32 offset
// table) exceed this threshold, the level is finalized as a BCFile
// data block and a parent-level entry pointing at it is created.
const DefaultIndexBlockSize = 128 * 1024

// MaxRunBlockSize is the raw-byte size at which a data block is cut even
// inside a run of identical keys. Below it, the writer never cuts between
// equal keys (see Writer). It sits well under bcfile.MaxRawBlockSize so a
// block that reaches it, plus the cell that crosses it, stays readable.
// A BlockSize above it raises the cap to BlockSize.
const MaxRunBlockSize = 64 * 1024 * 1024

// Writer produces an RFile by streaming bytes to an io.Writer. Use:
//
//	w := rfile.NewWriter(out, rfile.WriterOptions{Codec: "gz"})
//	for each cell: w.Append(key, value)
//	w.Close()
//
// Limitations:
//   - No sample groups or vector index.
//   - Cell ordering: caller MUST Append cells in non-decreasing Key
//     order within a locality group. The writer doesn't sort; Append
//     rejects a key that sorts before the previous one with
//     ErrKeyOutOfOrder. Equal keys are allowed (the embedded memtable
//     keeps every write to a coordinate, and PutLatest writes all share
//     timestamp math.MaxInt64).
//
// Block boundaries: every index entry's key is the last key of its data
// block. Once a block reaches BlockSize it is cut just before the next key
// that differs from its last key, so a run of identical keys normally
// stays in one block and index keys are strictly increasing. The one
// exception is a run so long that its block reaches MaxRunBlockSize: the
// block is then cut inside the run, and consecutive index entries carry
// the same key. Without that cap a perpetually overwritten cell would grow
// one block past bcfile.MaxRawBlockSize and every later flush or
// compaction would fail. Equal adjacent index keys are readable by this
// reader (it accepts them, never decreasing ones) but not by readers from
// before #690 was fixed, which reject them; files written before the fix
// may carry them too, from shorter runs.
//
// Multi-level index: the writer mirrors Java's
// MultiLevelIndex.Writer.flush(int level, Key lastKey, boolean last).
// Each level accumulates IndexEntries; when a level overflows
// IndexBlockSize, it's serialized + compressed + written as a BCFile
// data block, and a parent-level entry pointing at that block is
// created. The topmost level always stays in memory and is written
// inline as the root in the RFile.index meta block. This means files
// of any practical cluster size produce a compact root regardless of
// data-block count.
type Writer struct {
	out  *bcfile.Writer
	comp *block.Compressor
	opts WriterOptions

	// Currently-accumulating data block.
	blockBuf     bytes.Buffer
	blockPrev    *Key // for relkey prefix compression
	blockLast    *Key // becomes IndexEntry.Key on flush
	blockEntries int32

	// lastKey is the most recently appended key in the current locality
	// group. Unlike blockLast it survives block flushes, so Append can
	// enforce key order across block boundaries.
	lastKey *Key

	// runBlockCap is the block size at which a run of identical keys is
	// cut anyway: max(MaxRunBlockSize, BlockSize). Tests lower it.
	runBlockCap int

	// Multi-level index state.
	levels     []*indexLevel
	totalAdded int32 // count of data blocks (leaf IndexEntries) added

	// pendingLeaf defers the most-recently-flushed data block's
	// IndexEntry by one position. We need this because Java's
	// MultiLevelIndex distinguishes add() (= non-last) from addLast()
	// (= force-cascade), and we don't know mid-stream which flushBlock
	// is the final one. By deferring, the next flushBlock promotes the
	// previous deferral as non-last, and Close promotes the deferral
	// as last.
	pendingLeaf *index.IndexEntry

	// Whole-file metadata.
	firstKey       *Key
	cfCounts       map[string]int64
	localityGroups []*index.LocalityGroup
	currentLGName  string
	currentDefault bool
	localityNames  map[string]struct{}

	// blockmeta accumulators. Empty when no builders configured. Per-
	// block snapshots are appended to blockOverlays at every data-block
	// flush; the assembled BlockMeta is written as the RFile.blockmeta
	// meta-block at Close.
	bmBuilders    []blockmeta.OverlayBuilder
	blockOverlays [][]blockmeta.Overlay

	// adjBuilder, when non-nil, accumulates out-edge cells (cells whose
	// column family equals opts.AdjacencyEdgeCF) across the whole file
	// and emits the shoal.adjacency CSR meta-block at Close. nil when
	// no edge CF was configured — stock/legacy behavior.
	adjBuilder *adjacency.Builder

	closed bool
}

// WriterOptions tunes Writer construction.
type WriterOptions struct {
	// Codec is the compression algorithm used for every data block.
	// Empty defaults to "none". Must be registered in the Writer's
	// compressor (see Compressor option). The reader on the other side
	// will need the matching decoder registered.
	Codec string

	// BlockSize is the raw-byte size threshold at which the writer
	// flushes the current data block. Zero defaults to DefaultBlockSize.
	BlockSize int

	// IndexBlockSize is the raw-byte threshold at which a level of the
	// multi-level index is finalized as its own BCFile data block. Zero
	// defaults to DefaultIndexBlockSize. Set this small (e.g. 200) in
	// tests to force multi-level structure.
	IndexBlockSize int

	// Compressor overrides the default block.DefaultCompressor (which
	// supports "none" + "gz"). Use this to register snappy/zstd/lz4
	// encoders without forking the default registry.
	Compressor *block.Compressor

	// BlockMetaBuilders, when non-empty, enables RFile.blockmeta
	// emission. Each builder aggregates per-block state during Append
	// and produces one Overlay per block at flush time. Empty list
	// produces no meta-block (legacy / stock behavior).
	//
	// Builders are called in registration order per cell; per-block
	// overlays in the meta-block appear in the same order.
	BlockMetaBuilders []blockmeta.OverlayBuilder

	// AdjacencyEdgeCF, when non-empty, enables shoal.adjacency emission.
	// Cells whose column family equals this string are mirrored into a
	// CSR (compressed sparse row) out-edge index — a binary-searchable
	// node directory + contiguous per-node edge slices — written as the
	// shoal.adjacency meta-block at Close. The edge cells are ALSO
	// written into data blocks as usual; the index is a read accelerator,
	// not a replacement, so Scan/compaction/parity are unaffected. Empty
	// produces no adjacency block (stock behavior).
	AdjacencyEdgeCF string

	// EmbeddingSpace records this file's vector-space state in a small
	// file-level metadata block. Zero leaves the state unknown for legacy
	// callers that cannot assert whether the file carries embeddings.
	EmbeddingSpace embeddingspace.FileState
}

// NewWriter constructs a Writer over out.
func NewWriter(out io.Writer, opts WriterOptions) (*Writer, error) {
	if opts.Codec == "" {
		opts.Codec = block.CodecNone
	}
	if opts.BlockSize <= 0 {
		opts.BlockSize = DefaultBlockSize
	}
	if opts.IndexBlockSize <= 0 {
		opts.IndexBlockSize = DefaultIndexBlockSize
	}
	if opts.Compressor == nil {
		opts.Compressor = block.DefaultCompressor()
	}
	if !opts.Compressor.Has(opts.Codec) {
		return nil, fmt.Errorf("rfile: codec %q not registered in compressor", opts.Codec)
	}
	runBlockCap := MaxRunBlockSize
	if opts.BlockSize > runBlockCap {
		runBlockCap = opts.BlockSize
	}
	return &Writer{
		runBlockCap:    runBlockCap,
		out:            bcfile.NewWriter(out, opts.Codec),
		comp:           opts.Compressor,
		opts:           opts,
		cfCounts:       map[string]int64{},
		currentDefault: true,
		localityNames:  map[string]struct{}{},
		bmBuilders:     opts.BlockMetaBuilders,
		adjBuilder:     newAdjBuilder(opts.AdjacencyEdgeCF),
	}, nil
}

// newAdjBuilder returns an adjacency.Builder for edgeCF, or nil when no
// edge CF is configured.
func newAdjBuilder(edgeCF string) *adjacency.Builder {
	if edgeCF == "" {
		return nil
	}
	return adjacency.NewBuilder([]byte(edgeCF))
}

// ErrKeyOutOfOrder is returned by Append when a key sorts before the
// previously appended key of the same locality group.
var ErrKeyOutOfOrder = errors.New("rfile: key appended out of order")

// Append writes one cell. key and value must be in non-decreasing Key
// order across calls (the writer does NOT sort, and rejects a decreasing
// key with ErrKeyOutOfOrder). value may be nil (writes a zero-length
// value).
//
// When the current block has reached the block-size threshold and key
// differs from the block's last key, the block is flushed first:
// compressed, written via bcfile.Writer, and recorded in the in-memory
// RFile-level index. A key equal to the block's last key joins the
// current block, so no two blocks end on the same key, unless the block
// has reached MaxRunBlockSize, in which case it is flushed regardless.
func (w *Writer) Append(key *Key, value []byte) error {
	if w.closed {
		return errors.New("rfile: writer closed")
	}
	if key == nil {
		return errors.New("rfile: nil key")
	}
	if w.lastKey != nil {
		c := key.Compare(w.lastKey)
		if c < 0 {
			return fmt.Errorf("%w: %v sorts before %v", ErrKeyOutOfOrder, key, w.lastKey)
		}
		// Cut between distinct keys. Cutting inside a run of equal keys
		// gives two blocks the same last key, i.e. two index entries with
		// the same key (#690), which readers before the fix reject; do it
		// only when the run would otherwise grow the block without bound.
		size := w.blockBuf.Len()
		if (c > 0 && size >= w.opts.BlockSize) || size >= w.runBlockCap {
			if err := w.flushDataBlock(); err != nil {
				return err
			}
		}
	}
	// On a fresh block, blockPrev=nil so relkey writes the cell uncompressed.
	if err := relkey.EncodeKey(&w.blockBuf, w.blockPrev, key, value); err != nil {
		return fmt.Errorf("rfile.Append: relkey encode: %w", err)
	}
	keyClone := key.Clone()
	if w.firstKey == nil {
		w.firstKey = keyClone
	}
	w.blockPrev = keyClone
	w.blockLast = keyClone
	w.lastKey = keyClone
	w.blockEntries++
	w.cfCounts[string(key.ColumnFamily)]++

	// Feed every blockmeta builder for this cell. Builders see the
	// caller-supplied slices directly — they MUST NOT retain them
	// (documented on the OverlayBuilder interface).
	for _, b := range w.bmBuilders {
		b.AppendCell(key.Row, key.ColumnFamily, key.ColumnQualifier, key.ColumnVisibility, key.Timestamp, value)
	}

	// Mirror edge cells into the adjacency CSR builder. Add copies the
	// slices, so it's safe even though key aliases caller buffers.
	if w.adjBuilder != nil && bytes.Equal(key.ColumnFamily, w.adjBuilder.EdgeCF()) {
		w.adjBuilder.Add(key.Row, key.ColumnQualifier, value, key.ColumnVisibility, key.Timestamp, key.Deleted)
	}
	// A full block is flushed by the next Append with a distinct key, or
	// by Close / AddLocalityGroup.
	return nil
}

// AddLocalityGroup finishes the current locality group and starts a named
// group. Keys must remain sorted within each group, but groups are independent
// streams and may restart at any key. Names are unique and non-empty.
func (w *Writer) AddLocalityGroup(name string) error {
	if w.closed {
		return errors.New("rfile: writer closed")
	}
	if name == "" {
		return errors.New("rfile: locality group name is empty")
	}
	if _, exists := w.localityNames[name]; exists {
		return fmt.Errorf("rfile: duplicate locality group %q", name)
	}
	if err := w.finalizeCurrentLocalityGroup(); err != nil {
		return err
	}
	w.currentDefault = false
	w.currentLGName = name
	w.localityNames[name] = struct{}{}
	return nil
}

// flushDataBlock compresses the current data block, appends it to the
// BCFile, and produces an IndexEntry for it. The new IndexEntry becomes
// the deferred pendingLeaf; any previously-deferred leaf is now
// confirmed non-last and pushed into the multi-level index.
//
// No-op if the current block is empty.
func (w *Writer) flushDataBlock() error {
	if w.blockBuf.Len() == 0 {
		return nil
	}
	raw := w.blockBuf.Bytes()
	rawSize := int64(len(raw))
	compressed, err := w.comp.Encode(raw, w.opts.Codec)
	if err != nil {
		return fmt.Errorf("rfile.flushDataBlock: compress (codec=%s): %w", w.opts.Codec, err)
	}
	region, err := w.out.AppendDataBlock(compressed, rawSize)
	if err != nil {
		return fmt.Errorf("rfile.flushDataBlock: BCFile append: %w", err)
	}
	leaf := &index.IndexEntry{
		Key:            w.blockLast,
		NumEntries:     w.blockEntries,
		Offset:         region.Offset,
		CompressedSize: region.CompressedSize,
		RawSize:        region.RawSize,
	}

	// Snapshot every blockmeta builder for the just-flushed block.
	// nil-payload Snapshot results suppress emission for that builder
	// on this block (matches the Empty case for zone maps). Reset for
	// the next block.
	if len(w.bmBuilders) > 0 {
		var overlays []blockmeta.Overlay
		for _, b := range w.bmBuilders {
			payload := b.Snapshot()
			if payload != nil {
				overlays = append(overlays, blockmeta.Overlay{
					Type:    b.OverlayType(),
					Payload: payload,
				})
			}
			b.Reset()
		}
		w.blockOverlays = append(w.blockOverlays, overlays)
	}

	// Promote the previously-deferred leaf as non-last (a newer leaf has
	// arrived, so the previous one is definitely not the final block).
	if w.pendingLeaf != nil {
		if err := w.indexAdd(w.pendingLeaf, false); err != nil {
			return err
		}
	}
	w.pendingLeaf = leaf

	// Reset block state for the next block.
	w.blockBuf.Reset()
	w.blockPrev = nil
	w.blockLast = nil
	w.blockEntries = 0
	return nil
}

// Close flushes the final block (if any), promotes the deferred final
// leaf as last (cascading through the multi-level index), serializes
// the RFile.index meta block, then finalizes the BCFile. Idempotent.
//
// After Close, callers must close the underlying io.Writer themselves
// (bcfile.Writer doesn't own it).
func (w *Writer) Close() error {
	if w.closed {
		return nil
	}
	w.closed = true
	if err := w.finalizeCurrentLocalityGroup(); err != nil {
		return err
	}

	// Serialize the RFile.index meta block.
	var rfIdx bytes.Buffer
	if err := writeRFileIndexMeta(&rfIdx, w.localityGroups); err != nil {
		return fmt.Errorf("rfile.Close: serialize RFile.index: %w", err)
	}
	rawSize := int64(rfIdx.Len())
	// V0: store the RFile.index meta block uncompressed. Java compresses
	// it with the default codec; the reader honors the per-meta-block
	// codec name in the MetaIndex either way.
	if _, err := w.out.AppendMetaBlock(IndexMetaBlockName, block.CodecNone, rfIdx.Bytes(), rawSize); err != nil {
		return fmt.Errorf("rfile.Close: register %q meta block: %w", IndexMetaBlockName, err)
	}

	// Optionally emit RFile.blockmeta. Only when the writer was
	// configured with at least one OverlayBuilder.
	if len(w.bmBuilders) > 0 {
		bm := &blockmeta.BlockMeta{
			Version:       blockmeta.V1,
			BlockOverlays: w.blockOverlays,
		}
		var bmBuf bytes.Buffer
		if err := blockmeta.Write(&bmBuf, bm); err != nil {
			return fmt.Errorf("rfile.Close: serialize %q: %w", blockmeta.MetaBlockName, err)
		}
		if _, err := w.out.AppendMetaBlock(blockmeta.MetaBlockName, block.CodecNone, bmBuf.Bytes(), int64(bmBuf.Len())); err != nil {
			return fmt.Errorf("rfile.Close: register %q meta block: %w", blockmeta.MetaBlockName, err)
		}
	}

	if w.opts.EmbeddingSpace.State != "" {
		encoded, err := embeddingspace.Encode(w.opts.EmbeddingSpace)
		if err != nil {
			return fmt.Errorf("rfile.Close: encode %q: %w", embeddingspace.RFileMetaBlockName, err)
		}
		if _, err := w.out.AppendMetaBlock(embeddingspace.RFileMetaBlockName, block.CodecNone, encoded, int64(len(encoded))); err != nil {
			return fmt.Errorf("rfile.Close: register %q meta block: %w", embeddingspace.RFileMetaBlockName, err)
		}
	}

	// Optionally emit shoal.adjacency. Only when an edge CF was
	// configured AND at least one edge cell was seen (an empty graph
	// produces no block, matching the "no index" default).
	if w.adjBuilder != nil {
		if ix := w.adjBuilder.Build(); ix != nil {
			var adjBuf bytes.Buffer
			if err := adjacency.Write(&adjBuf, ix); err != nil {
				return fmt.Errorf("rfile.Close: serialize %q: %w", adjacency.MetaBlockName, err)
			}
			compressed, err := w.comp.Encode(adjBuf.Bytes(), w.opts.Codec)
			if err != nil {
				return fmt.Errorf("rfile.Close: compress %q: %w", adjacency.MetaBlockName, err)
			}
			if _, err := w.out.AppendMetaBlock(adjacency.MetaBlockName, w.opts.Codec, compressed, int64(adjBuf.Len())); err != nil {
				return fmt.Errorf("rfile.Close: register %q meta block: %w", adjacency.MetaBlockName, err)
			}
		}
	}

	if err := w.out.Close(); err != nil {
		return fmt.Errorf("rfile.Close: BCFile close: %w", err)
	}
	return nil
}

func (w *Writer) finalizeCurrentLocalityGroup() error {
	if err := w.flushDataBlock(); err != nil {
		return err
	}
	if w.pendingLeaf != nil {
		if err := w.indexAdd(w.pendingLeaf, true); err != nil {
			return err
		}
		w.pendingLeaf = nil
	}
	root, err := w.buildRoot()
	if err != nil {
		return fmt.Errorf("rfile: build locality group %q root index: %w", w.currentLGName, err)
	}
	w.localityGroups = append(w.localityGroups, &index.LocalityGroup{
		IsDefault:       w.currentDefault,
		Name:            w.currentLGName,
		ColumnFamilies:  w.cfCounts,
		FirstKey:        w.firstKey,
		NumTotalEntries: w.totalAdded,
		RootIndex:       root,
	})
	w.blockBuf.Reset()
	w.blockPrev = nil
	w.blockLast = nil
	w.lastKey = nil
	w.blockEntries = 0
	w.levels = nil
	w.totalAdded = 0
	w.pendingLeaf = nil
	w.firstKey = nil
	w.cfCounts = map[string]int64{}
	return nil
}

// indexLevel accumulates IndexEntries for one level of the multi-level
// index. Mirrors Java's MultiLevelIndex.IndexBlock during construction:
// a running entryData byte buffer + parallel entryOffsets[] for the
// per-entry start positions.
type indexLevel struct {
	level       int32
	levelOffset int32 // stamped into IndexBlock.Offset on serialize

	entryData    []byte
	entryOffsets []int32
}

// addEntry appends one IndexEntry to the level's accumulators.
func (l *indexLevel) addEntry(e *index.IndexEntry) error {
	var buf bytes.Buffer
	if err := index.WriteIndexEntry(&buf, e); err != nil {
		return err
	}
	l.entryOffsets = append(l.entryOffsets, int32(len(l.entryData)))
	l.entryData = append(l.entryData, buf.Bytes()...)
	return nil
}

// size mirrors Java IndexBlock.getSize: serialized entry bytes plus the
// per-entry int32 offset table.
func (l *indexLevel) size() int {
	return len(l.entryData) + 4*len(l.entryOffsets)
}

// numEntries returns the count of accumulated entries.
func (l *indexLevel) numEntries() int { return len(l.entryOffsets) }

// toBlock snapshots the current accumulators into a fresh IndexBlock.
// Caller copies because the level may be reset and reused.
func (l *indexLevel) toBlock(hasNext bool) *index.IndexBlock {
	offsets := make([]int32, len(l.entryOffsets))
	copy(offsets, l.entryOffsets)
	data := make([]byte, len(l.entryData))
	copy(data, l.entryData)
	return &index.IndexBlock{
		Level:   l.level,
		Offset:  l.levelOffset,
		HasNext: hasNext,
		Offsets: offsets,
		Data:    data,
	}
}

// indexAdd is the external entry point for the multi-level builder. It
// mirrors Java's MultiLevelIndex.Writer.add (last=false) and addLast
// (last=true): bump totalAdded, append the leaf at level 0, then
// recursively cascade-flush starting at level 0.
func (w *Writer) indexAdd(leaf *index.IndexEntry, last bool) error {
	w.totalAdded++
	if err := w.indexAddAtLevel(0, leaf); err != nil {
		return err
	}
	return w.indexFlushLevel(0, leaf.Key, last)
}

// indexAddAtLevel materializes level on demand and appends the entry.
// New levels are stamped with levelOffset = w.totalAdded so the IndexBlock
// records the global leaf-position of the first entry.
func (w *Writer) indexAddAtLevel(level int32, e *index.IndexEntry) error {
	if int(level) == len(w.levels) {
		w.levels = append(w.levels, &indexLevel{
			level:       level,
			levelOffset: w.totalAdded - 1,
		})
	}
	return w.levels[level].addEntry(e)
}

// indexFlushLevel mirrors Java MultiLevelIndex.Writer.flush(level, lastKey, last).
//
// If last && this level is the topmost: return without writing — the
// topmost level always stays in memory, to be serialized inline as the
// RFile.index meta block's root.
//
// Else if the level is overflowing (or last is true), serialize the
// level's IndexBlock as a BCFile data block, add a non-leaf entry
// pointing at that block to level+1, recurse to flush(level+1, ...).
// Reset (or drop) the just-flushed level.
func (w *Writer) indexFlushLevel(level int32, lastKey *Key, last bool) error {
	if last && int(level) == len(w.levels)-1 {
		return nil
	}
	lvl := w.levels[level]
	if lvl == nil {
		return fmt.Errorf("rfile: cascade hit nil level %d", level)
	}
	overflow := lvl.size() > w.opts.IndexBlockSize && lvl.numEntries() > 1
	if !overflow && !last {
		return nil
	}
	if lvl.numEntries() == 0 {
		// Nothing accumulated at this level — happens at addLast time
		// when the cascade reaches a level that was just-reset by an
		// earlier mid-stream cascade. Skip and let the parent level
		// handle finalization.
		return nil
	}

	// Serialize the IndexBlock and write it as a BCFile data block.
	ib := lvl.toBlock(!last)
	var ibBuf bytes.Buffer
	if err := index.WriteIndexBlock(&ibBuf, ib); err != nil {
		return fmt.Errorf("rfile: serialize IndexBlock at level %d: %w", level, err)
	}
	rawSize := int64(ibBuf.Len())
	compressed, err := w.comp.Encode(ibBuf.Bytes(), w.opts.Codec)
	if err != nil {
		return fmt.Errorf("rfile: compress IndexBlock at level %d: %w", level, err)
	}
	region, err := w.out.AppendDataBlock(compressed, rawSize)
	if err != nil {
		return fmt.Errorf("rfile: write IndexBlock at level %d: %w", level, err)
	}

	// Push a non-leaf entry up to the parent level. Java passes 0 for
	// numEntries on non-leaf entries (the field is meaningful only for
	// leaf entries pointing at data blocks); we mirror that.
	parent := &index.IndexEntry{
		Key:            lastKey,
		NumEntries:     0,
		Offset:         region.Offset,
		CompressedSize: region.CompressedSize,
		RawSize:        region.RawSize,
	}
	if err := w.indexAddAtLevel(level+1, parent); err != nil {
		return err
	}
	if err := w.indexFlushLevel(level+1, lastKey, last); err != nil {
		return err
	}

	if last {
		w.levels[level] = nil
	} else {
		w.levels[level] = &indexLevel{
			level:       level,
			levelOffset: w.totalAdded,
		}
	}
	return nil
}

// buildRoot returns the IndexBlock that goes inline in the RFile.index
// meta block. For an empty file (no Append calls), this is a fresh
// empty level-0 block. Otherwise it's the topmost level — the one the
// cascade left in memory.
func (w *Writer) buildRoot() (*index.IndexBlock, error) {
	if len(w.levels) == 0 {
		return &index.IndexBlock{
			Level: 0, Offset: 0, HasNext: false,
		}, nil
	}
	top := w.levels[len(w.levels)-1]
	if top == nil {
		return nil, errors.New("rfile: top level finalized; multi-level cascade bug")
	}
	return top.toBlock(false), nil
}

// writeRFileIndexMeta emits the RFile.index wire form: magic + version
// + group count + N × LocalityGroup + V8 trailing flags.
//
// CRITICAL: Java's V8 reader (RFile.java:1438) calls mb.readBoolean()
// UNCONDITIONALLY for the "hasSamples" flag — without our two trailing
// false bytes, Java EOFs partway through. Confirmed via Java writer at
// RFile.java:662-703 which emits a false-bool for samples and a false-
// bool for vector index whenever those features aren't in use.
func writeRFileIndexMeta(w io.Writer, groups []*index.LocalityGroup) error {
	if err := wire.WriteInt32(w, index.RIndexMagic); err != nil {
		return err
	}
	if err := wire.WriteInt32(w, index.V8); err != nil {
		return err
	}
	if err := wire.WriteInt32(w, int32(len(groups))); err != nil {
		return err
	}
	for _, lg := range groups {
		if err := index.WriteLocalityGroup(w, lg, index.V8); err != nil {
			return err
		}
	}
	// V8 trailer: two trailing booleans for sample-groups + vector-index.
	// Both false (we don't produce either feature in V0).
	if err := wire.WriteBool(w, false); err != nil {
		return fmt.Errorf("write hasSamples=false: %w", err)
	}
	if err := wire.WriteBool(w, false); err != nil {
		return fmt.Errorf("write hasVectorIndex=false: %w", err)
	}
	return nil
}
