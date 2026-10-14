package index

import (
	"errors"
	"fmt"
	"sort"

	"github.com/phrocker/shoal-oss/internal/rfile/bcfile"
	"github.com/phrocker/shoal-oss/internal/rfile/wire"
)

// LevelReader fetches a child IndexBlock by its BCFile region. The
// non-leaf entries inside an IndexBlock describe child blocks via
// (Offset, CompressedSize, RawSize); the walker hands those tuples to
// the LevelReader to materialize the next level down.
//
// Implementations are responsible for fetching the bytes from the
// BCFile, decompressing them with the right codec, and parsing as an
// IndexBlock. The walker is codec-agnostic — it doesn't know whether
// the bytes were gzipped, snappied, or stored raw.
type LevelReader interface {
	ReadIndexBlock(region bcfile.BlockRegion) (*IndexBlock, error)
}

// LevelReaderFunc is a closure-style adapter so callers can pass a
// function literal where a LevelReader is expected.
type LevelReaderFunc func(region bcfile.BlockRegion) (*IndexBlock, error)

// ReadIndexBlock satisfies LevelReader.
func (f LevelReaderFunc) ReadIndexBlock(region bcfile.BlockRegion) (*IndexBlock, error) {
	return f(region)
}

// ErrPastEnd is returned by Seek when the target key is beyond the last
// entry in the index — i.e. there's no data block that could contain it.
// Distinct from "key not found": for an RFile, Seek returns the entry
// pointing at the block where key WOULD be if present, even if the actual
// row is missing. Past-end means there's no such block at all.
var ErrPastEnd = errors.New("rfile/index: target key is past the last index entry")

const maxTraversalDepth int32 = 64

type blockRegionKey struct {
	offset         int64
	compressedSize int64
}

// Walker walks a multi-level index tree from a preserved root IndexBlock,
// fetching child blocks on demand via the LevelReader. Stateless across
// Seek calls — each Seek descends from root.
type Walker struct {
	root *IndexBlock
	lr   LevelReader
}

// NewWalker constructs a Walker over root. lr may be nil iff root is
// the only level (Level == 0); for multi-level trees the walker will
// call lr to fetch each child block.
func NewWalker(root *IndexBlock, lr LevelReader) *Walker {
	return &Walker{root: root, lr: lr}
}

// Seek descends the tree to find the leaf IndexEntry whose data block
// covers target. Returns the entry on success; returns ErrPastEnd if
// target sorts after every key in the index.
//
// Java equivalent: Reader.Node.lookup (MultiLevelIndex.java:616-641).
// Mirrors the exact comparator behaviour: an IndexEntry's key is the
// LAST key in its referenced block, so we want the smallest entry whose
// key >= target.
func (w *Walker) Seek(target *wire.Key) (*IndexEntry, error) {
	cur := w.root
	if err := validateRootLevel(cur); err != nil {
		return nil, err
	}
	visited := make(map[blockRegionKey]struct{})
	for {
		if _, _, err := validateBlockEntries(cur); err != nil {
			return nil, err
		}
		entries := EntriesOf(cur)
		idx, err := binarySearchKey(entries, target)
		if err != nil {
			return nil, err
		}
		if idx == entries.Len() {
			// Past end at this level. At root this means target > everything.
			// At a non-root level it would violate the invariant that the
			// parent's chosen entry's key bounds this child — Java throws
			// IllegalStateException here, and we mirror that.
			if cur == w.root {
				return nil, ErrPastEnd
			}
			return nil, fmt.Errorf("rfile/index: walker invariant violation: target past end at non-root level %d", cur.Level)
		}
		entry, err := entries.At(idx)
		if err != nil {
			return nil, err
		}
		if cur.Level == 0 {
			return entry, nil
		}
		// Non-leaf: descend.
		if w.lr == nil {
			return nil, fmt.Errorf("rfile/index: multi-level index requires LevelReader (level=%d)", cur.Level)
		}
		var lowerBound *wire.Key
		if idx > 0 {
			lowerBound, err = entries.KeyAt(idx - 1)
			if err != nil {
				return nil, err
			}
		}
		child, err := readChildBlock(w.lr, cur.Level, bcfile.BlockRegion{
			Offset:         entry.Offset,
			CompressedSize: entry.CompressedSize,
			RawSize:        entry.RawSize,
		}, lowerBound, entry.Key, visited)
		if err != nil {
			return nil, fmt.Errorf("rfile/index: descend to level %d: %w", cur.Level-1, err)
		}
		cur = child
	}
}

// binarySearchKey finds the smallest index i in entries where
// entries[i].Key >= target. Returns Len() if no such entry exists.
//
// Uses KeyAt for the comparator — that decodes only the Key portion
// of each candidate entry, skipping the trailing sizes.
func binarySearchKey(entries *Entries, target *wire.Key) (int, error) {
	n := entries.Len()
	var searchErr error
	idx := sort.Search(n, func(i int) bool {
		if searchErr != nil {
			return true // short-circuit further iterations
		}
		k, err := entries.KeyAt(i)
		if err != nil {
			searchErr = err
			return true
		}
		return k.Compare(target) >= 0
	})
	if searchErr != nil {
		return 0, searchErr
	}
	return idx, nil
}

// IterateLeaves walks every leaf-level IndexEntry in tree order (left
// to right), invoking fn for each. fn returning a non-nil error halts
// the walk and surfaces that error from IterateLeaves.
//
// For a single-level tree (Level == 0 root) this just iterates the
// root entries. For a multi-level tree it descends fully into each
// non-leaf entry, recursively iterating leaves.
//
// Note: this implementation is sequential and synchronous — Phase 3c
// will compose it with the prefetcher so DATA-block fetches overlap
// with consumer work, but the index walk itself is single-threaded.
// (The index is small relative to data, so prefetching index blocks
// is rarely the bottleneck.)
func (w *Walker) IterateLeaves(fn func(*IndexEntry) error) error {
	if err := validateRootLevel(w.root); err != nil {
		return err
	}
	state := &iterationState{visited: make(map[blockRegionKey]struct{})}
	return iterateLeaves(w.root, w.lr, fn, state)
}

type iterationState struct {
	visited  map[blockRegionKey]struct{}
	lastLeaf *wire.Key
}

func iterateLeaves(
	block *IndexBlock,
	lr LevelReader,
	fn func(*IndexEntry) error,
	state *iterationState,
) error {
	if _, _, err := validateBlockEntries(block); err != nil {
		return err
	}
	entries := EntriesOf(block)
	n := entries.Len()
	var lowerBound *wire.Key
	for i := 0; i < n; i++ {
		entry, err := entries.At(i)
		if err != nil {
			return err
		}
		if block.Level == 0 {
			region := bcfile.BlockRegion{
				Offset:         entry.Offset,
				CompressedSize: entry.CompressedSize,
				RawSize:        entry.RawSize,
			}
			key := blockRegionKey{offset: region.Offset, compressedSize: region.CompressedSize}
			if _, ok := state.visited[key]; ok {
				return fmt.Errorf("rfile/index: repeated leaf block region %+v", region)
			}
			state.visited[key] = struct{}{}
			// Equal adjacent leaf keys are legal; see validateBlockEntries.
			if state.lastLeaf != nil && entry.Key.Compare(state.lastLeaf) < 0 {
				return fmt.Errorf(
					"rfile/index: leaf key %v sorts before previous leaf key %v",
					entry.Key, state.lastLeaf,
				)
			}
			state.lastLeaf = entry.Key
			if err := fn(entry); err != nil {
				return err
			}
			lowerBound = entry.Key
			continue
		}
		if lr == nil {
			return fmt.Errorf("rfile/index: multi-level iterate requires LevelReader (level=%d)", block.Level)
		}
		child, err := readChildBlock(lr, block.Level, bcfile.BlockRegion{
			Offset:         entry.Offset,
			CompressedSize: entry.CompressedSize,
			RawSize:        entry.RawSize,
		}, lowerBound, entry.Key, state.visited)
		if err != nil {
			return fmt.Errorf("rfile/index: descend to level %d: %w", block.Level-1, err)
		}
		if err := iterateLeaves(child, lr, fn, state); err != nil {
			return err
		}
		lowerBound = entry.Key
	}
	return nil
}

func validateRootLevel(root *IndexBlock) error {
	if root == nil {
		return errors.New("rfile/index: nil root index block")
	}
	if root.Level < 0 {
		return fmt.Errorf("rfile/index: negative root level %d", root.Level)
	}
	if root.Level > maxTraversalDepth {
		return fmt.Errorf(
			"rfile/index: root level %d exceeds maximum traversal depth %d",
			root.Level, maxTraversalDepth,
		)
	}
	return nil
}

func readChildBlock(
	lr LevelReader,
	parentLevel int32,
	region bcfile.BlockRegion,
	lowerBound *wire.Key,
	upperBound *wire.Key,
	visited map[blockRegionKey]struct{},
) (*IndexBlock, error) {
	key := blockRegionKey{offset: region.Offset, compressedSize: region.CompressedSize}
	if _, ok := visited[key]; ok {
		return nil, fmt.Errorf("repeated child block region %+v", region)
	}
	visited[key] = struct{}{}
	child, err := lr.ReadIndexBlock(region)
	if err != nil {
		return nil, err
	}
	if child == nil {
		return nil, errors.New("LevelReader returned nil child block")
	}
	expectedLevel := parentLevel - 1
	if child.Level != expectedLevel {
		return nil, fmt.Errorf(
			"child block level %d, want %d from parent level %d",
			child.Level, expectedLevel, parentLevel,
		)
	}
	first, last, err := validateBlockEntries(child)
	if err != nil {
		return nil, err
	}
	if first == nil {
		return nil, errors.New("child index block has no entries")
	}
	// The child's first key may equal the previous sibling's final key
	// when a run of equal leaf keys straddles two index blocks; see
	// validateBlockEntries. It may never sort before it.
	if lowerBound != nil && first.Compare(lowerBound) < 0 {
		return nil, fmt.Errorf(
			"child first key %v sorts before parent lower bound %v",
			first, lowerBound,
		)
	}
	if upperBound == nil || !last.Equal(upperBound) {
		return nil, fmt.Errorf(
			"child final key %v does not match parent upper bound %v",
			last, upperBound,
		)
	}
	return child, nil
}

// ValidateBlock rejects index blocks that are unsafe for binary search.
//
// Entry keys must be non-decreasing. Equal adjacent keys are accepted:
// writers before #690 cut data blocks inside a run of identical cell keys,
// so consecutive blocks could end on the same key. That index is still
// sound, because an entry's key is the last key of its block: every block
// after the first of an equal run holds only that key, a seek lands on the
// first entry whose key is >= the target, and the reader then scans
// forward across block boundaries. The current writer emits equal keys
// only when a single run of identical keys outgrows rfile.MaxRunBlockSize
// and has to be cut. A key that sorts before its predecessor is always
// rejected.
func ValidateBlock(block *IndexBlock) error {
	if err := validateRootLevel(block); err != nil {
		return err
	}
	_, _, err := validateBlockEntries(block)
	return err
}

func validateBlockEntries(block *IndexBlock) (*wire.Key, *wire.Key, error) {
	if block == nil {
		return nil, nil, errors.New("rfile/index: nil index block")
	}
	if block.Level < 0 {
		return nil, nil, fmt.Errorf("rfile/index: negative block level %d", block.Level)
	}
	entries := EntriesOf(block)
	var first, previous *wire.Key
	for i := 0; i < entries.Len(); i++ {
		entry, err := entries.At(i)
		if err != nil {
			return nil, nil, err
		}
		if entry.Key == nil {
			return nil, nil, fmt.Errorf("rfile/index: entry %d has nil key", i)
		}
		if previous != nil && entry.Key.Compare(previous) < 0 {
			return nil, nil, fmt.Errorf(
				"rfile/index: entry %d key %v sorts before entry %d key %v",
				i, entry.Key, i-1, previous,
			)
		}
		if block.Level == 0 && entry.NumEntries <= 0 {
			return nil, nil, fmt.Errorf(
				"rfile/index: leaf entry %d has non-positive cell count %d",
				i, entry.NumEntries,
			)
		}
		if first == nil {
			first = entry.Key
		}
		previous = entry.Key
	}
	return first, previous, nil
}

// Root returns the root IndexBlock the walker was constructed with.
// Convenience for tests + introspection.
func (w *Walker) Root() *IndexBlock { return w.root }
