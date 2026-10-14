package index

import (
	"bytes"
	"fmt"

	"github.com/phrocker/shoal-oss/internal/rfile/wire"
)

// Entries is a random-access view of the IndexEntries in one IndexBlock.
// We don't decode every entry up front — most callers (binary search,
// targeted seek) touch only log(N) entries. Decoding happens lazily on
// At(i).
//
// The view is read-only and shares storage with block.Data; callers
// must not mutate the block while iterating.
type Entries struct {
	block *IndexBlock
}

// EntriesOf returns a random-access view over block's entries.
func EntriesOf(block *IndexBlock) *Entries { return &Entries{block: block} }

// Len returns the number of entries in the underlying IndexBlock.
func (e *Entries) Len() int {
	if e.block == nil {
		return 0
	}
	return len(e.block.Offsets)
}

// At returns the i'th IndexEntry (0-indexed). Each call decodes from
// the preserved Data bytes — repeated calls re-decode, so callers
// hot-pathing the same index should hold onto the result.
func (e *Entries) At(i int) (*IndexEntry, error) {
	segment, err := e.segment(i, "At")
	if err != nil {
		return nil, err
	}
	r := bytes.NewReader(segment)
	entry, err := ReadIndexEntry(r)
	if err != nil {
		return nil, err
	}
	if r.Len() != 0 {
		return nil, fmt.Errorf(
			"Entries.At: entry %d left %d bytes in its declared segment", i, r.Len())
	}
	return entry, nil
}

// KeyAt is a perf-oriented helper for binary search: only reads the
// Key portion of entry i, skipping the entries/offsets/sizes tail.
// Equivalent to At(i).Key but ~2× faster for typical key sizes.
func (e *Entries) KeyAt(i int) (*wire.Key, error) {
	segment, err := e.segment(i, "KeyAt")
	if err != nil {
		return nil, err
	}
	return wire.ReadKey(bytes.NewReader(segment))
}

func (e *Entries) segment(i int, operation string) ([]byte, error) {
	if i < 0 || i >= e.Len() {
		return nil, fmt.Errorf(
			"Entries.%s: index %d out of range [0, %d)", operation, i, e.Len())
	}
	start := int(e.block.Offsets[i])
	end := len(e.block.Data)
	if i+1 < e.Len() {
		end = int(e.block.Offsets[i+1])
	}
	if start < 0 || start > end || end > len(e.block.Data) {
		return nil, fmt.Errorf(
			"Entries.%s: segment [%d,%d) out of range [0,%d]",
			operation, start, end, len(e.block.Data))
	}
	return e.block.Data[start:end], nil
}
