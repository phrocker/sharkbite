package rfile

import (
	"fmt"

	"github.com/phrocker/shoal-oss/accumulo"
)

// StreamRelocation is the argument every seekable stream accepts: where to
// position, and which column families to admit once positioned. It is the Go
// equivalent of Sharkbite's opaque StreamRelocation base type, and Seekable is
// the concrete implementation callers build.
type StreamRelocation interface {
	// Range is the key range to position within. Never nil.
	Range() *accumulo.Range

	// ColumnFamilies is the family restriction, or nil for none. The returned
	// slice is a copy: mutating it cannot change the relocation.
	ColumnFamilies() [][]byte

	// Inclusive reports how ColumnFamilies is applied: true admits only the
	// listed families, false admits everything except them.
	Inclusive() bool
}

// Seekable positions a stream over a range, optionally restricted to a set of
// column families. It is the Go equivalent of Sharkbite's Seekable.
type Seekable struct {
	keyRange  *accumulo.Range
	families  [][]byte
	inclusive bool
}

// NewSeekable positions over keyRange with no column-family restriction,
// mirroring Sharkbite's Seekable(Range).
func NewSeekable(keyRange *accumulo.Range) (Seekable, error) {
	if keyRange == nil {
		return Seekable{}, fmt.Errorf("%w: range is required", ErrInvalidSeekable)
	}
	return Seekable{keyRange: keyRange}, nil
}

// NewSeekableColumns positions over keyRange and restricts the stream to the
// given column families, mirroring Sharkbite's Seekable(Range, list[str],
// bool). When inclusive is true only the listed families are yielded; when it
// is false the listed families are excluded and everything else is yielded.
//
// Families are copied, so the caller's slices cannot change the relocation
// afterwards. An empty family list with inclusive true admits nothing, which
// is the same degenerate case Accumulo's SortedKeyValueIterator defines.
func NewSeekableColumns(keyRange *accumulo.Range, families [][]byte, inclusive bool) (Seekable, error) {
	seekable, err := NewSeekable(keyRange)
	if err != nil {
		return Seekable{}, err
	}
	copied := make([][]byte, 0, len(families))
	for _, family := range families {
		if family == nil {
			return Seekable{}, fmt.Errorf("%w: column family must not be nil", ErrInvalidSeekable)
		}
		copied = append(copied, append([]byte(nil), family...))
	}
	seekable.families = copied
	seekable.inclusive = inclusive
	return seekable, nil
}

// EntireFile is the relocation that reads a whole RFile from the first entry
// to the last, which is what Sharkbite's sequentialRead positions on.
func EntireFile() Seekable {
	keyRange, err := accumulo.NewRange(nil, true, nil, true)
	if err != nil {
		// NewRange only fails when end sorts before start; both are nil here.
		panic("rfile: unbounded range must be constructible: " + err.Error())
	}
	return Seekable{keyRange: keyRange}
}

// Range implements StreamRelocation.
func (s Seekable) Range() *accumulo.Range { return s.keyRange }

// ColumnFamilies implements StreamRelocation and returns a copy.
func (s Seekable) ColumnFamilies() [][]byte {
	if len(s.families) == 0 {
		return nil
	}
	copied := make([][]byte, len(s.families))
	for i, family := range s.families {
		copied[i] = append([]byte(nil), family...)
	}
	return copied
}

// Inclusive implements StreamRelocation.
func (s Seekable) Inclusive() bool { return s.inclusive }

// String implements fmt.Stringer.
func (s Seekable) String() string {
	if s.keyRange == nil {
		return "rfile.Seekable{}"
	}
	if len(s.families) == 0 {
		return fmt.Sprintf("rfile.Seekable{range: [%q, %q]}", s.keyRange.StartRow(), s.keyRange.EndRow())
	}
	return fmt.Sprintf(
		"rfile.Seekable{range: [%q, %q], families: %d, inclusive: %t}",
		s.keyRange.StartRow(), s.keyRange.EndRow(), len(s.families), s.inclusive,
	)
}
