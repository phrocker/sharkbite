package rfile

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/phrocker/shoal-oss/internal/cache"
	"github.com/phrocker/shoal-oss/internal/embeddingspace"
	"github.com/phrocker/shoal-oss/internal/rfile/adjacency"
	"github.com/phrocker/shoal-oss/internal/rfile/bcfile"
	"github.com/phrocker/shoal-oss/internal/rfile/bcfile/block"
	"github.com/phrocker/shoal-oss/internal/rfile/blockmeta"
	"github.com/phrocker/shoal-oss/internal/rfile/index"
	"github.com/phrocker/shoal-oss/internal/rfile/relkey"
)

// IndexMetaBlockName is the BCFile meta-block name under which RFile
// stashes its directory (locality groups + per-group MultiLevelIndex
// roots). Mirrors RFile.java's getMetaBlock("RFile.index") at line 1410.
const IndexMetaBlockName = "RFile.index"

// SkipPredicate decides whether to skip a leaf data block before fetch.
// Inputs: 0-based leaf index in tree-order, the IndexEntry that points
// at the data block, and the parsed BlockMeta (or nil if absent).
// Return true to skip (no I/O, no decode); false to fetch + iterate.
//
// Predicates are pure / stateless from shoal's perspective; the caller
// owns any state (e.g., a query's ts-floor for time-travel).
type SkipPredicate func(leafIdx int, entry *index.IndexEntry, bm *blockmeta.BlockMeta) bool

// Reader is the top-level RFile reader: bcfile + decompressor + index
// walker + relkey, wired together to produce (Key, Value) pairs from
// a row range.
//
// V0 scope: iterates the DEFAULT locality group only. Multi-LG merge
// (heap-merge across all LGs at read time, like Java's RFile.Reader)
// lands when a real cluster RFile that splits CFs across LGs forces
// the issue. For shoal V0's read-fleet purpose, the default LG is
// nearly always the only one in practice.
//
// Concurrency: a Reader is single-consumer. Two goroutines must not
// share the same Reader through Seek/Next; instantiate two readers
// from the same bcfile.Reader+Decompressor instead.
type Reader struct {
	bc           *bcfile.Reader
	dec          *block.Decompressor
	idx          *index.Reader
	walker       *index.Walker
	lg           *index.LocalityGroup
	dataCodec    string
	dataBlocks   []bcfile.BlockRegion
	dataBlockSet map[bcfile.BlockRegion]struct{}

	// Optional block cache. nil disables caching. Keyed by (cacheKey,
	// region.Offset). When set, every block fetch (data, BCFile.index,
	// RFile.index, RFile.blockmeta, multi-level child index) goes
	// through the cache.
	cache    *cache.BlockCache
	cacheKey string

	// Optional per-block summary metadata (RFile.blockmeta meta-block).
	// nil for stock RFiles produced without shoal augmentation. When
	// present, callers can install a SkipPredicate that consults
	// per-block overlays (zone maps, future tier-2 overlays) to
	// short-circuit fetch + decompress before openLeaf.
	bm *blockmeta.BlockMeta

	// Optional out-edge index (shoal.adjacency meta-block). nil for
	// files written without an AdjacencyEdgeCF. When present, Neighbors
	// answers "out-edges of row" via binary search + a contiguous slice
	// read, bypassing the merge/versioning/relkey decode a Scan pays.
	adj            *adjacency.Index
	embeddingSpace embeddingspace.FileState

	// Per-cell filter. Threaded into every relkey.Reader we open so it
	// applies inside the inner decode loop — rejected cells skip value
	// materialization without crossing the rfile.Reader API boundary.
	// nil = accept everything.
	filter relkey.Filter

	// SkipPredicate is called once per leaf during Seek + advanceBlock
	// BEFORE the block is fetched and decompressed. Returning true
	// causes the block to be skipped entirely (no I/O, no decode work).
	// Receives leafIdx (0-based) + the IndexEntry + the parsed
	// BlockMeta if available (nil otherwise — predicates should treat
	// nil as "no metadata, must fetch").
	skipPred SkipPredicate

	// sharedLeaves marks that leaves were supplied by a SharedFile and
	// are immutable + already collected — Seek must not re-walk the index
	// to rebuild them. When false, the first Seek collects leaves once
	// and reuses them on subsequent re-seeks (leaves are file-immutable).
	sharedLeaves bool

	// Iteration state. Set by Seek; advanced by Next.
	leaves     []*index.IndexEntry // pre-collected leaf list (default LG, in order)
	leafIdx    int                 // index of the CURRENT data block (one being decoded)
	currentBlk *relkey.Reader      // open relkey reader over the current block (or nil)

	// One-cell lookahead so Seek can compare-without-consuming. After
	// Seek, this holds the cell that Next will return; once Next consumes
	// it, the slot empties and Next pulls fresh from currentBlk. relkey
	// has no peek primitive — this fills that role at the rfile layer.
	pendingKey *Key
	pendingVal []byte
	hasPending bool

	// Optional async prefetcher — populated lazily on first Next.
	prefetcher *block.Prefetcher
	ctx        context.Context
	cancel     context.CancelFunc

	// Iteration end signals.
	exhausted bool
	lastErr   error
}

// OpenOption tunes Reader construction.
type OpenOption func(*Reader)

// WithBlockCache attaches a per-replica block cache. cacheKey uniquely
// identifies this RFile within the cache (typically the GCS path). All
// block fetches consult the cache; misses populate it. nil cache or
// empty key disables caching for this Reader.
func WithBlockCache(c *cache.BlockCache, cacheKey string) OpenOption {
	return func(r *Reader) {
		r.cache = c
		r.cacheKey = cacheKey
	}
}

// Open constructs a Reader from a parsed BCFile + a decompressor. It
// loads the "RFile.index" meta block (decompressing as needed), parses
// it, and prepares a walker over the default locality group.
//
// The decompressor must support every codec used in the BCFile — at
// minimum the meta block's codec and the data-block default codec.
// `block.Default()` covers "none", "gz", and "snappy"; register zstd/lz4
// as needed.
func Open(bc *bcfile.Reader, dec *block.Decompressor, opts ...OpenOption) (*Reader, error) {
	if bc == nil {
		return nil, errors.New("rfile: nil bcfile.Reader")
	}
	if dec == nil {
		return nil, errors.New("rfile: nil Decompressor")
	}

	r := &Reader{bc: bc, dec: dec}
	for _, o := range opts {
		o(r)
	}

	metaEntry, err := bc.MetaBlockEntry(IndexMetaBlockName)
	if err != nil {
		return nil, fmt.Errorf("rfile: locate %q: %w", IndexMetaBlockName, err)
	}
	rawIndex, err := r.fetchBlock(metaEntry.Region, metaEntry.CompressionAlgo)
	if err != nil {
		return nil, fmt.Errorf("rfile: decompress %q: %w", IndexMetaBlockName, err)
	}
	idx, err := index.Parse(rawIndex)
	if err != nil {
		return nil, fmt.Errorf("rfile: parse %q: %w", IndexMetaBlockName, err)
	}
	if err := validateReadableIndex(idx); err != nil {
		return nil, err
	}

	defaultLG := pickDefaultLG(idx.Groups)
	if defaultLG == nil {
		return nil, errors.New("rfile: no default locality group found")
	}

	// Data-block compression: read from the BCFile's DataIndex (which
	// itself is stored as the BCFile.index meta block). For V0 we don't
	// need DataIndex.Blocks (the data-block list); we need its
	// DefaultCompression so we know how to decode IndexEntries' targets.
	dataCodec, err := r.loadDataCodec()
	if err != nil {
		return nil, fmt.Errorf("rfile: load data codec: %w", err)
	}

	walker := r.makeWalker(idx, defaultLG.RootIndex, dataCodec)

	// Optional: parse RFile.blockmeta if present. Stock Apache RFiles
	// don't emit it; shoal-augmented + Java-coordinated RFiles will.
	// Absence is non-fatal — walker just runs without skip predicates.
	bm, err := r.loadBlockMeta()
	if err != nil {
		return nil, fmt.Errorf("rfile: load %q: %w", blockmeta.MetaBlockName, err)
	}

	// Optional: parse shoal.adjacency if present. Same non-fatal
	// contract as blockmeta — absence just means no edge accelerator.
	adj, err := r.loadAdjacency()
	if err != nil {
		return nil, fmt.Errorf("rfile: load %q: %w", adjacency.MetaBlockName, err)
	}
	embedding, err := r.loadEmbeddingSpace()
	if err != nil {
		return nil, fmt.Errorf("rfile: load %q: %w", embeddingspace.RFileMetaBlockName, err)
	}

	r.idx = idx
	r.walker = walker
	r.lg = defaultLG
	r.dataCodec = dataCodec
	r.bm = bm
	r.adj = adj
	r.embeddingSpace = embedding
	return r, nil
}

func ReadEmbeddingSpaceMetadata(bc *bcfile.Reader, dec *block.Decompressor) (embeddingspace.FileState, error) {
	if bc == nil {
		return embeddingspace.FileState{}, errors.New("rfile: nil bcfile.Reader")
	}
	if dec == nil {
		return embeddingspace.FileState{}, errors.New("rfile: nil Decompressor")
	}
	r := &Reader{bc: bc, dec: dec}
	return r.loadEmbeddingSpace()
}

// SharedFile holds the immutable, parse-once state of one RFile's default
// locality group: the parsed RFile.index, the default LG, the data-block
// codec, the optional RFile.blockmeta, and — crucially — the fully
// collected leaf IndexEntry list. All of this is derived solely from the
// file's bytes and never changes (RFiles are immutable by path), so it can
// be computed once and shared by many lightweight cursor Readers.
//
// Point lookups previously re-ran the whole index parse (index.Parse +
// loadDataCodec + makeWalker + loadBlockMeta) AND re-walked the index to
// re-collect every leaf on every single Seek — pure per-file work redone
// per lookup. Caching a SharedFile per path collapses that to once.
//
// The embedded bcfile.Reader is safe for concurrent read use (it issues
// stateless ReadAt against an immutable io.ReaderAt), so one SharedFile
// backs concurrent scans; each scan gets its own cursor Reader (own
// decompressor + cursor state) via NewReaderFromShared.
type SharedFile struct {
	bc             *bcfile.Reader
	idx            *index.Reader
	lg             *index.LocalityGroup
	dataCodec      string
	dataBlocks     []bcfile.BlockRegion
	dataBlockSet   map[bcfile.BlockRegion]struct{}
	bm             *blockmeta.BlockMeta
	adj            *adjacency.Index
	embeddingSpace embeddingspace.FileState
	leaves         []*index.IndexEntry
}

// OpenShared parses an RFile once and collects its default-LG leaves,
// returning a SharedFile that many cursor Readers can reuse. The block
// cache option (if supplied) is consulted for the index/meta blocks read
// during the one-time parse; cursor Readers attach their own cache for
// data-block fetches.
func OpenShared(bc *bcfile.Reader, dec *block.Decompressor, opts ...OpenOption) (*SharedFile, error) {
	r, err := Open(bc, dec, opts...)
	if err != nil {
		return nil, err
	}
	leaves, err := r.collectLeaves()
	if err != nil {
		return nil, err
	}
	return &SharedFile{
		bc:             bc,
		idx:            r.idx,
		lg:             r.lg,
		dataCodec:      r.dataCodec,
		dataBlocks:     r.dataBlocks,
		dataBlockSet:   r.dataBlockSet,
		bm:             r.bm,
		adj:            r.adj,
		embeddingSpace: r.embeddingSpace,
		leaves:         leaves,
	}, nil
}

// Adjacency returns the parsed shoal.adjacency out-edge index for this
// shared file, or nil if it carries none.
func (sf *SharedFile) Adjacency() *adjacency.Index { return sf.adj }

func (sf *SharedFile) EmbeddingSpace() embeddingspace.FileState {
	if sf.embeddingSpace.State == "" {
		return embeddingspace.Unknown()
	}
	return sf.embeddingSpace
}

// Neighbors returns this file's out-edges for row, plus ok=false when the
// file has no adjacency index (caller must fall back to a Scan). ok=true
// with a nil/empty slice means the index is present and row simply has no
// edges in this file. The returned slice aliases index storage; do not
// mutate.
func (sf *SharedFile) Neighbors(row []byte) ([]adjacency.Edge, bool) {
	if sf.adj == nil {
		return nil, false
	}
	return sf.adj.Neighbors(row), true
}

// NewReaderFromShared builds a lightweight cursor Reader over a SharedFile.
// It reuses the shared bcfile.Reader, parsed index, data codec, blockmeta,
// and pre-collected leaves — skipping the per-lookup index parse and leaf
// re-collection entirely. The Reader gets its own decompressor and cursor
// state, so concurrent scans over one SharedFile are independent. Pass
// WithBlockCache to route data-block fetches through a shared cache.
func NewReaderFromShared(sf *SharedFile, dec *block.Decompressor, opts ...OpenOption) *Reader {
	r := &Reader{
		bc:             sf.bc,
		dec:            dec,
		idx:            sf.idx,
		lg:             sf.lg,
		dataCodec:      sf.dataCodec,
		dataBlocks:     sf.dataBlocks,
		dataBlockSet:   sf.dataBlockSet,
		bm:             sf.bm,
		adj:            sf.adj,
		embeddingSpace: sf.embeddingSpace,
		leaves:         sf.leaves,
		sharedLeaves:   true,
	}
	for _, o := range opts {
		o(r)
	}
	return r
}

// makeWalker builds the multi-level index walker for one LG's root.
// LevelReader fetches child IndexBlocks via the cached fetchBlock so
// nested index nodes go through the same cache as data blocks.
func (r *Reader) makeWalker(idx *index.Reader, root *index.IndexBlock, dataCodec string) *index.Walker {
	lr := index.LevelReaderFunc(func(region bcfile.BlockRegion) (*index.IndexBlock, error) {
		raw, err := r.fetchDataBlock(region, dataCodec)
		if err != nil {
			return nil, fmt.Errorf("level-reader fetch: %w", err)
		}
		return index.ReadIndexBlock(bytes.NewReader(raw), idx.Version)
	})
	return index.NewWalker(root, lr)
}

// OpenAll returns one Reader per locality group in the BCFile — default
// LG plus every named LG. Each Reader is independent: it walks its own
// LG's RootIndex, holds its own per-LG state (leaves, current block,
// pending cell), and Next() returns cells in Key order WITHIN that LG.
//
// Real-world graph tables routinely use a named LG (e.g. "vertex") to
// store one CF separately from the rest of the row's cells in the default
// LG. A scan that needs both must heap-merge across multiple Readers —
// the fileIter heap in scanserver/scan.go does this naturally; it gets
// one fileIter per (file, LG) tuple.
//
// Single-LG files (no named LGs) return a 1-element slice that behaves
// identically to a single Open() call. Callers that don't care about
// multi-LG can use Open() unchanged.
//
// All Readers share the same bcfile.Reader, decompressor, and block
// cache — closing one doesn't affect the others.
func OpenAll(bc *bcfile.Reader, dec *block.Decompressor, opts ...OpenOption) ([]*Reader, error) {
	if bc == nil {
		return nil, errors.New("rfile: nil bcfile.Reader")
	}
	if dec == nil {
		return nil, errors.New("rfile: nil Decompressor")
	}

	// Bootstrap: parse RFile.index, BCFile.index, RFile.blockmeta
	// once. These are file-scoped, not per-LG. Reuse them across all
	// the Readers we return.
	root := &Reader{bc: bc, dec: dec}
	for _, o := range opts {
		o(root)
	}
	metaEntry, err := bc.MetaBlockEntry(IndexMetaBlockName)
	if err != nil {
		return nil, fmt.Errorf("rfile: locate %q: %w", IndexMetaBlockName, err)
	}
	rawIndex, err := root.fetchBlock(metaEntry.Region, metaEntry.CompressionAlgo)
	if err != nil {
		return nil, fmt.Errorf("rfile: decompress %q: %w", IndexMetaBlockName, err)
	}
	idx, err := index.Parse(rawIndex)
	if err != nil {
		return nil, fmt.Errorf("rfile: parse %q: %w", IndexMetaBlockName, err)
	}
	if err := validateReadableIndex(idx); err != nil {
		return nil, err
	}
	dataCodec, err := root.loadDataCodec()
	if err != nil {
		return nil, fmt.Errorf("rfile: load data codec: %w", err)
	}
	bm, err := root.loadBlockMeta()
	if err != nil {
		return nil, fmt.Errorf("rfile: load %q: %w", blockmeta.MetaBlockName, err)
	}
	adj, err := root.loadAdjacency()
	if err != nil {
		return nil, fmt.Errorf("rfile: load %q: %w", adjacency.MetaBlockName, err)
	}
	embedding, err := root.loadEmbeddingSpace()
	if err != nil {
		return nil, fmt.Errorf("rfile: load %q: %w", embeddingspace.RFileMetaBlockName, err)
	}

	if len(idx.Groups) == 0 {
		return nil, errors.New("rfile: file has no locality groups")
	}

	out := make([]*Reader, 0, len(idx.Groups))
	for _, lg := range idx.Groups {
		// Empty roots carry no cells; skip to avoid constructing a walker
		// over an empty index.
		if lg.RootIndex == nil || lg.RootIndex.NumEntries() == 0 {
			continue
		}
		r := &Reader{
			bc:             bc,
			dec:            dec,
			cache:          root.cache,
			cacheKey:       root.cacheKey,
			idx:            idx,
			lg:             lg,
			dataCodec:      dataCodec,
			dataBlocks:     root.dataBlocks,
			dataBlockSet:   root.dataBlockSet,
			bm:             bm,
			adj:            adj,
			embeddingSpace: embedding,
		}
		r.walker = r.makeWalker(idx, lg.RootIndex, dataCodec)
		out = append(out, r)
	}
	return out, nil
}

func validateReadableIndex(idx *index.Reader) error {
	if idx == nil {
		return errors.New("rfile: nil index")
	}
	if idx.VectorExtension != nil {
		return errors.New("rfile: vector-index extensions are not supported")
	}
	if len(idx.SampleGroups) > 0 || idx.SamplerConfiguration != nil {
		return errors.New("rfile: sampled files are not supported")
	}
	defaultGroups := 0
	namedGroups := make(map[string]int)
	columnFamilies := make(map[string]int)
	for group, lg := range idx.Groups {
		if lg == nil {
			return fmt.Errorf("rfile: locality group %d is nil", group)
		}
		if lg.IsDefault {
			defaultGroups++
			if lg.Name != "" {
				return fmt.Errorf("rfile: default locality group %d has name %q", group, lg.Name)
			}
		} else {
			if lg.Name == "" {
				return fmt.Errorf("rfile: named locality group %d has an empty name", group)
			}
			if previous, exists := namedGroups[lg.Name]; exists {
				return fmt.Errorf(
					"rfile: locality groups %d and %d have duplicate name %q",
					previous, group, lg.Name,
				)
			}
			namedGroups[lg.Name] = group
		}
		for cf := range lg.ColumnFamilies {
			if previous, exists := columnFamilies[cf]; exists {
				return fmt.Errorf(
					"rfile: column family %q belongs to locality groups %d and %d",
					cf, previous, group,
				)
			}
			columnFamilies[cf] = group
		}
		if lg.RootIndex == nil {
			return fmt.Errorf("rfile: locality group %d has no root index", group)
		}
		if err := index.ValidateBlock(lg.RootIndex); err != nil {
			return fmt.Errorf("rfile: locality group %d root index: %w", group, err)
		}
		rootEmpty := lg.RootIndex.NumEntries() == 0
		countEmpty := lg.NumTotalEntries == 0
		if rootEmpty != countEmpty {
			return fmt.Errorf(
				"rfile: locality group %d total-entry count is inconsistent with its root index",
				group)
		}
	}
	if defaultGroups != 1 {
		return fmt.Errorf(
			"rfile: expected exactly one default locality group, found %d",
			defaultGroups,
		)
	}
	return nil
}

// fetchBlock fetches + decompresses one BCFile region, consulting the
// optional block cache. On miss the decompressed bytes are inserted
// into the cache so the next Get returns instantly.
//
// The cache stores RAW (decompressed) bytes — same shape as
// dec.Block returns. Memory is owned by the cache; caller treats the
// returned slice as read-only for the duration of its lifetime in the
// cache (typically until LRU eviction).
func (r *Reader) fetchBlock(region bcfile.BlockRegion, codec string) ([]byte, error) {
	if err := bcfile.ValidateBlockRegion(region, r.bc.FileLength()); err != nil {
		return nil, err
	}
	if r.cache != nil && r.cacheKey != "" {
		if v, ok := r.cache.Get(r.cacheKey, region.Offset); ok {
			return v, nil
		}
	}
	raw, err := r.dec.Block(r.bc.Source(), region, codec)
	if err != nil {
		return nil, err
	}
	if r.cache != nil && r.cacheKey != "" {
		r.cache.Put(r.cacheKey, region.Offset, raw)
	}
	return raw, nil
}

func (r *Reader) fetchDataBlock(
	region bcfile.BlockRegion,
	codec string,
) ([]byte, error) {
	if err := bcfile.ValidateBlockRegion(region, r.bc.DataRegionEnd()); err != nil {
		return nil, err
	}
	// Java-produced RFiles can omit the optional DataIndex block list.
	// When present, require an exact tuple match; when absent, the
	// data/meta boundary above remains authoritative.
	if r.dataBlockSet != nil {
		if _, declared := r.dataBlockSet[region]; !declared {
			return nil, fmt.Errorf(
				"rfile: block region is not declared by BCFile.index: offset=%d compressed=%d raw=%d",
				region.Offset, region.CompressedSize, region.RawSize,
			)
		}
	}
	return r.fetchBlock(region, codec)
}

// loadBlockMeta returns the parsed RFile.blockmeta meta-block if the
// BCFile MetaIndex contains one. Missing entry → (nil, nil). Present
// but malformed → (nil, error). Cache-aware via r.fetchBlock.
func (r *Reader) loadBlockMeta() (*blockmeta.BlockMeta, error) {
	entry, err := r.bc.MetaBlockEntry(blockmeta.MetaBlockName)
	if err != nil {
		// Not present — stock RFile, perfectly fine.
		return nil, nil
	}
	raw, err := r.fetchBlock(entry.Region, entry.CompressionAlgo)
	if err != nil {
		return nil, fmt.Errorf("decompress: %w", err)
	}
	bm, err := blockmeta.Parse(bytes.NewReader(raw))
	if err != nil {
		return nil, fmt.Errorf("parse: %w", err)
	}
	return bm, nil
}

// loadAdjacency returns the parsed shoal.adjacency meta-block if the
// BCFile MetaIndex contains one. Missing entry → (nil, nil). Present
// but malformed → (nil, error). Cache-aware via r.fetchBlock.
func (r *Reader) loadAdjacency() (*adjacency.Index, error) {
	entry, err := r.bc.MetaBlockEntry(adjacency.MetaBlockName)
	if err != nil {
		// Not present — file written without an edge CF. Fine.
		return nil, nil
	}
	raw, err := r.fetchBlock(entry.Region, entry.CompressionAlgo)
	if err != nil {
		return nil, fmt.Errorf("decompress: %w", err)
	}
	adj, err := adjacency.Parse(bytes.NewReader(raw))
	if err != nil {
		return nil, fmt.Errorf("parse: %w", err)
	}
	return adj, nil
}

func (r *Reader) loadEmbeddingSpace() (embeddingspace.FileState, error) {
	entry, err := r.bc.MetaBlockEntry(embeddingspace.RFileMetaBlockName)
	if err != nil {
		return embeddingspace.Unknown(), nil
	}
	raw, err := r.fetchBlock(entry.Region, entry.CompressionAlgo)
	if err != nil {
		return embeddingspace.FileState{}, fmt.Errorf("decompress: %w", err)
	}
	state, err := embeddingspace.Decode(raw)
	if err != nil {
		return embeddingspace.FileState{}, fmt.Errorf("parse: %w", err)
	}
	return state, nil
}

func (r *Reader) EmbeddingSpace() embeddingspace.FileState {
	if r.embeddingSpace.State == "" {
		return embeddingspace.Unknown()
	}
	return r.embeddingSpace
}

// Adjacency returns the parsed shoal.adjacency out-edge index, or nil if
// the file doesn't carry one. Callers use Neighbors for the resolved
// lookup; this accessor is for diagnostics + cross-file union logic.
func (r *Reader) Adjacency() *adjacency.Index { return r.adj }

// Neighbors returns this file's out-edges for row from the adjacency
// index, or (nil, false) when the file has no adjacency index. A present
// index with no edges for row returns (nil, true) — distinguishing
// "no accelerator" from "accelerator says zero edges". Returned slice
// aliases the index storage; do not mutate.
func (r *Reader) Neighbors(row []byte) ([]adjacency.Edge, bool) {
	if r.adj == nil {
		return nil, false
	}
	return r.adj.Neighbors(row), true
}

// pickDefaultLG returns the unique default LG in groups, or nil if none.
// Java RFile guarantees exactly one default LG; we don't enforce that
// invariant strictly — return the first one.
func pickDefaultLG(groups []*index.LocalityGroup) *index.LocalityGroup {
	for _, g := range groups {
		if g.IsDefault {
			return g
		}
	}
	return nil
}

// loadDataCodec reads the BCFile's BCFile.index meta block (which holds
// the DataIndex with the cluster's chosen default compression) and
// returns the codec name. Cache-aware via r.fetchBlock. Single-shot at
// Open time.
func (r *Reader) loadDataCodec() (string, error) {
	entry, err := r.bc.MetaBlockEntry(bcfile.DataIndexBlockName)
	if err != nil {
		return "", fmt.Errorf("locate %q: %w", bcfile.DataIndexBlockName, err)
	}
	raw, err := r.fetchBlock(entry.Region, entry.CompressionAlgo)
	if err != nil {
		return "", fmt.Errorf("decompress %q: %w", bcfile.DataIndexBlockName, err)
	}
	di, err := bcfile.ReadDataIndex(bytes.NewReader(raw))
	if err != nil {
		return "", fmt.Errorf("parse DataIndex: %w", err)
	}
	if err := validateDataRegions(di.Blocks, r.bc.DataRegionEnd()); err != nil {
		return "", err
	}
	r.dataBlocks = append([]bcfile.BlockRegion(nil), di.Blocks...)
	if len(di.Blocks) > 0 {
		r.dataBlockSet = make(map[bcfile.BlockRegion]struct{}, len(di.Blocks))
		for _, region := range di.Blocks {
			r.dataBlockSet[region] = struct{}{}
		}
	}
	return di.DefaultCompression, nil
}

func validateDataRegions(regions []bcfile.BlockRegion, dataRegionEnd int64) error {
	for i, region := range regions {
		if err := bcfile.ValidateBlockRegion(region, dataRegionEnd); err != nil {
			return fmt.Errorf("validate DataIndex block %d: %w", i, err)
		}
		if i > 0 {
			previous := regions[i-1]
			if region.Offset < previous.Offset+previous.CompressedSize {
				return fmt.Errorf(
					"validate DataIndex block %d: overlaps or precedes block %d",
					i, i-1,
				)
			}
		}
	}
	return nil
}

// LocalityGroup returns the default LG metadata that the reader is
// iterating. Useful for diagnostics + tests.
func (r *Reader) LocalityGroup() *index.LocalityGroup { return r.lg }

// BlockMeta returns the parsed RFile.blockmeta meta-block, or nil if
// the file doesn't have one. Useful for diagnostics + custom skip
// predicates that consume overlay payloads directly.
func (r *Reader) BlockMeta() *blockmeta.BlockMeta { return r.bm }

// SetSkipPredicate installs a per-leaf skip predicate. Consulted in
// Seek and advanceBlock before the block is fetched + decompressed —
// returning true causes the block to be bypassed entirely. nil disables.
//
// Predicate inputs include the parsed BlockMeta (or nil if the file
// doesn't have one); predicates that rely on metadata typically
// short-circuit when it's absent — "no metadata → can't skip safely".
func (r *Reader) SetSkipPredicate(p SkipPredicate) { r.skipPred = p }

// SetFilter installs a per-cell filter predicate. The predicate is
// applied inside the relkey decoder for every cell — rejected cells
// don't allocate a Key clone or materialize the value, so high-rejection
// scans get a substantial throughput boost.
//
// Lifetime of the *Key passed to the filter: TRANSIENT. Slices alias
// the decompressed block buffer + are invalidated on the next decode
// step. Filters that need to retain the Key must Clone() — but for
// the visibility-evaluator use case we only inspect bytes during the
// call and return immediately, so no clone is needed.
//
// Calling SetFilter mid-iteration is safe; the new predicate takes
// effect on the next cell decoded.
func (r *Reader) SetFilter(f relkey.Filter) {
	r.filter = f
	if r.currentBlk != nil {
		r.currentBlk.SetFilter(f)
	}
}

// IndexReader returns the parsed RFile.index for callers that need
// access to other LGs / sample groups.
func (r *Reader) IndexReader() *index.Reader { return r.idx }

// Seek positions the iterator at the smallest key >= target. After Seek,
// Next returns the first matching cell (which might have key > target if
// target's exact row isn't present).
//
// SeekRow(row) is the common case — wrap a Key with just the Row field.
//
// Use Seek(nil) to position at the start of the locality group.
func (r *Reader) Seek(target *Key) error {
	r.exhausted = false
	r.lastErr = nil
	r.closeCurrentBlock()
	r.closePrefetcher()

	// Build the leaf list. Leaves are immutable per file, so we collect
	// them only once: a SharedFile supplies them pre-collected
	// (sharedLeaves), and an ordinary Reader caches them on its first
	// Seek and reuses them on every re-seek. This skips a full index
	// walk (walker.IterateLeaves allocates every IndexEntry) on the hot
	// point-lookup path. For huge files V1 may stream lazily; V0's index
	// is small (hundreds to low-thousands of leaves).
	if !r.sharedLeaves && r.leaves == nil {
		leaves, err := r.collectLeaves()
		if err != nil {
			return fmt.Errorf("rfile.Seek: collect leaves: %w", err)
		}
		r.leaves = leaves
	}
	leaves := r.leaves

	if len(leaves) == 0 {
		r.exhausted = true
		return nil
	}

	// Find the first leaf whose IndexEntry.Key >= target. That's the
	// data block our seek lands in.
	startIdx := 0
	if target != nil {
		startIdx = -1
		for i, leaf := range leaves {
			if leaf.Key.Compare(target) >= 0 {
				startIdx = i
				break
			}
		}
		if startIdx < 0 {
			r.exhausted = true
			return nil
		}
	}
	// Slide forward over any leaves the skip predicate rejects.
	startIdx = r.firstNonSkippedFrom(startIdx)
	if startIdx >= len(r.leaves) {
		r.exhausted = true
		return nil
	}
	r.leafIdx = startIdx

	// Open the first block, then advance until we find key >= target.
	// The cell we land on is buffered in pendingKey/Val; Next will
	// return it on the first call, then continue normally.
	//
	// The skip-scan uses NextView (zero-copy): cells that sort before
	// target are decoded only to update the relkey delta state, never
	// cloned. Only the landing cell is cloned (to honor Next's
	// owned-output contract once it's handed out). This is the hot path
	// for point lookups, where the target sits partway into a block and
	// every preceding cell would otherwise be cloned and discarded.
	if err := r.openLeaf(r.leafIdx); err != nil {
		r.exhausted = true
		return err
	}
	for {
		k, v, err := r.currentBlk.NextView()
		if errors.Is(err, io.EOF) {
			// Block ended before finding key >= target. Advance to next
			// block; its first key is by index-construction > target.
			r.closeCurrentBlock()
			r.leafIdx = r.firstNonSkippedFrom(r.leafIdx + 1)
			if r.leafIdx >= len(r.leaves) {
				r.exhausted = true
				return nil
			}
			if err := r.openLeaf(r.leafIdx); err != nil {
				r.exhausted = true
				return err
			}
			// Buffer the first cell of the new block as the seek result.
			k, v, err := r.currentBlk.NextView()
			if err != nil {
				if errors.Is(err, io.EOF) {
					// Empty block — defensive; shouldn't happen in
					// practice (IndexEntry.NumEntries should be >0).
					r.exhausted = true
					return nil
				}
				return err
			}
			r.bufferPending(k.Clone(), cloneValue(v))
			return nil
		}
		if err != nil {
			return err
		}
		if target == nil || k.Compare(target) >= 0 {
			r.bufferPending(k.Clone(), cloneValue(v))
			return nil
		}
		// k < target: drop and keep scanning (no clone).
	}
}

// cloneValue returns an owned copy of a transient value slice. nil maps
// to nil so callers can distinguish "no value" from "empty value".
func cloneValue(v []byte) []byte {
	if v == nil {
		return nil
	}
	return append([]byte(nil), v...)
}

// bufferPending stores k/v as the next cell that Next will return.
// Caller must ensure hasPending is currently false (i.e. we haven't
// already buffered without flushing).
func (r *Reader) bufferPending(k *Key, v []byte) {
	r.pendingKey = k
	r.pendingVal = v
	r.hasPending = true
}

// SeekRow is a convenience: Seek(&Key{Row: row}). Common case for
// scan() requests scoped to a row range.
func (r *Reader) SeekRow(row []byte) error {
	return r.Seek(&Key{Row: row})
}

// Next returns the next cell. Returns io.EOF when the iterator is
// exhausted. After io.EOF, subsequent calls keep returning io.EOF.
func (r *Reader) Next() (*Key, []byte, error) {
	if r.lastErr != nil {
		return nil, nil, r.lastErr
	}
	if r.exhausted {
		return nil, nil, io.EOF
	}
	if r.currentBlk == nil && !r.hasPending {
		// Seek wasn't called — implicitly seek to start.
		if err := r.Seek(nil); err != nil {
			r.lastErr = err
			return nil, nil, err
		}
		if r.exhausted {
			return nil, nil, io.EOF
		}
	}
	// Drain the seek-buffered cell first, if any.
	if r.hasPending {
		k, v := r.pendingKey, r.pendingVal
		r.pendingKey, r.pendingVal, r.hasPending = nil, nil, false
		return k, v, nil
	}
	k, v, err := r.currentBlk.Next()
	if errors.Is(err, io.EOF) {
		// Current block exhausted; advance.
		if err := r.advanceBlock(); err != nil {
			r.lastErr = err
			return nil, nil, err
		}
		if r.exhausted {
			return nil, nil, io.EOF
		}
		return r.Next() // tail-recurse into next block
	}
	if err != nil {
		r.lastErr = err
		return nil, nil, err
	}
	return k, v, nil
}

// Close releases resources: the prefetcher (if active) and the current
// block's relkey reader. Safe to call multiple times.
func (r *Reader) Close() error {
	r.closeCurrentBlock()
	r.closePrefetcher()
	return nil
}

func (r *Reader) closeCurrentBlock() {
	r.currentBlk = nil
	r.pendingKey, r.pendingVal, r.hasPending = nil, nil, false
}

func (r *Reader) closePrefetcher() {
	if r.prefetcher != nil {
		_ = r.prefetcher.Close()
		r.prefetcher = nil
	}
	if r.cancel != nil {
		r.cancel()
		r.cancel = nil
	}
	r.ctx = nil
}

// collectLeaves walks every leaf IndexEntry in the default LG's index
// and returns them in tree order. Single-shot; called once per Seek.
func (r *Reader) collectLeaves() ([]*index.IndexEntry, error) {
	var out []*index.IndexEntry
	err := r.walker.IterateLeaves(func(e *index.IndexEntry) error {
		out = append(out, e)
		return nil
	})
	return out, err
}

// openLeaf fetches data block at leaves[i] (via cache when configured),
// decompresses, opens a relkey.Reader over it. Stores in r.currentBlk.
func (r *Reader) openLeaf(i int) error {
	leaf := r.leaves[i]
	raw, err := r.fetchDataBlock(bcfile.BlockRegion{
		Offset:         leaf.Offset,
		CompressedSize: leaf.CompressedSize,
		RawSize:        leaf.RawSize,
	}, r.dataCodec)
	if err != nil {
		return fmt.Errorf("rfile: read data block @ %d: %w", leaf.Offset, err)
	}
	r.currentBlk = relkey.NewReader(raw, int(leaf.NumEntries))
	if r.filter != nil {
		r.currentBlk.SetFilter(r.filter)
	}
	return nil
}

// advanceBlock moves to the next non-skipped leaf, or marks exhausted
// when no eligible leaves remain.
func (r *Reader) advanceBlock() error {
	r.closeCurrentBlock()
	r.leafIdx = r.firstNonSkippedFrom(r.leafIdx + 1)
	if r.leafIdx >= len(r.leaves) {
		r.exhausted = true
		return nil
	}
	return r.openLeaf(r.leafIdx)
}

// firstNonSkippedFrom returns the smallest index i in [start, len(leaves)]
// such that the SkipPredicate (if installed) returns false for leaves[i].
// Returns len(leaves) if every remaining leaf is skipped — caller checks
// for exhaustion.
//
// The predicate is consulted only when installed. With no predicate (V0
// stock behavior) this returns start unchanged in O(1).
func (r *Reader) firstNonSkippedFrom(start int) int {
	if r.skipPred == nil {
		return start
	}
	i := start
	for i < len(r.leaves) {
		if !r.skipPred(i, r.leaves[i], r.bm) {
			return i
		}
		i++
	}
	return i
}
