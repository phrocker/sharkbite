package accumulo

import (
	"bytes"
	"errors"
	"fmt"
	"math"
	"strings"

	"github.com/phrocker/shoal-oss/internal/thrift/gen/data"
)

// Range describes a row-bounded scan range.
type Range struct {
	startKey       *Key
	endKey         *Key
	startRowOnly   bool
	endRowOnly     bool
	startInclusive bool
	endInclusive   bool
}

// NewRange constructs a range. A nil start or end row is unbounded.
func NewRange(startRow []byte, startInclusive bool, endRow []byte, endInclusive bool) (*Range, error) {
	start := cloneRow(startRow)
	end := cloneRow(endRow)
	if start != nil && end != nil && bytes.Compare(end, start) < 0 {
		return nil, fmt.Errorf("accumulo: range end row %q precedes start row %q", end, start)
	}
	return &Range{
		startKey:       keyForRow(start),
		endKey:         keyForRow(end),
		startRowOnly:   start != nil,
		endRowOnly:     end != nil,
		startInclusive: startInclusive,
		endInclusive:   endInclusive,
	}, nil
}

// NewKeyRange constructs a range with full Accumulo key bounds. A nil start or
// end key is unbounded. A bound may not carry a deletion marker: Key.Compare
// orders deletions before the matching live key, but the scan wire's TKey has
// no field for the marker, so such a bound would exclude a key locally and
// include it on the server.
func NewKeyRange(start *Key, startInclusive bool, end *Key, endInclusive bool) (*Range, error) {
	if (start != nil && start.Deleted) || (end != nil && end.Deleted) {
		return nil, ErrDeletedRangeBound
	}
	startCopy := cloneKey(start)
	endCopy := cloneKey(end)
	if startCopy != nil && endCopy != nil && compareKeys(*endCopy, *startCopy) < 0 {
		return nil, errors.New("accumulo: range end key precedes start key")
	}
	return &Range{
		startKey:       startCopy,
		endKey:         endCopy,
		startInclusive: startInclusive,
		endInclusive:   endInclusive,
	}, nil
}

// NewRangeRow constructs an inclusive range containing one row.
func NewRangeRow(row []byte) (*Range, error) {
	if len(row) == 0 {
		return nil, errors.New("accumulo: range row is empty")
	}
	return NewRange(row, true, row, true)
}

// InfiniteRange constructs the unbounded range (-infinity, +infinity).
func InfiniteRange() *Range {
	return &Range{startInclusive: true}
}

// StartRow returns a defensive copy of the lower row bound.
func (r *Range) StartRow() []byte {
	if r == nil {
		return nil
	}
	if r.startKey == nil {
		return nil
	}
	return cloneRow(r.startKey.Row)
}

// EndRow returns a defensive copy of the upper row bound.
func (r *Range) EndRow() []byte {
	if r == nil {
		return nil
	}
	if r.endKey == nil {
		return nil
	}
	return cloneRow(r.endKey.Row)
}

// StartKey returns a defensive copy of the full lower key bound. It is nil for
// an unbounded start.
func (r *Range) StartKey() *Key {
	if r == nil {
		return nil
	}
	return cloneKey(r.startKey)
}

// EndKey returns a defensive copy of the full upper key bound. It is nil for
// an unbounded end.
func (r *Range) EndKey() *Key {
	if r == nil {
		return nil
	}
	return cloneKey(r.endKey)
}

// StartInclusive reports whether the lower row bound is included.
func (r *Range) StartInclusive() bool {
	return r != nil && r.startInclusive
}

// EndInclusive reports whether the upper row bound is included.
func (r *Range) EndInclusive() bool {
	return r != nil && r.endInclusive
}

// KeyBounds returns the range as absolute key bounds, which is what a
// key-ordered reader needs.
//
// NewRange builds a row-bounded range, and a row bound is not the key that
// spells the row. Every cell of row R sorts at or after the *first possible
// key* of R, which is Key{Row: R} with the maximum timestamp, because Accumulo
// sorts timestamps descending: a cell at (R, empty family, timestamp 100)
// sorts before Key{Row: R, Timestamp: 0}. Resolving the two forms therefore
// differs:
//
//   - an inclusive row start becomes the first possible key of the row;
//   - an exclusive row start becomes the first possible key of the following
//     row boundary (R+NUL), so the whole row is skipped;
//   - an inclusive row end becomes that same following-row boundary, exclusive,
//     so the whole row is kept;
//   - an exclusive row end becomes the first possible key of the row,
//     exclusive, so the whole row is dropped;
//   - a range built by NewKeyRange already carries absolute keys and is
//     returned with its own inclusivity.
//
// The boundary key carries no delete flag, exactly like Accumulo's own
// Key(Text row) row boundary, because accumulo.Key cannot express one.
//
// The returned keys are copies, and a nil key means that side is unbounded.
func (r *Range) KeyBounds() (start *Key, startInclusive bool, end *Key, endInclusive bool) {
	if r == nil {
		return nil, true, nil, false
	}
	start, startInclusive = r.effectiveStart()
	end, endInclusive = r.effectiveEnd()
	return cloneKey(start), startInclusive, cloneKey(end), endInclusive
}

// effectiveStart resolves the lower bound without copying. A row-only bound
// allocates its boundary key; a key bound is the stored key, which callers here
// only read. KeyBounds copies before handing either to a caller.
func (r *Range) effectiveStart() (*Key, bool) {
	if r.startKey == nil {
		return nil, r.startInclusive
	}
	if r.startRowOnly {
		return firstKeyOfRow(r.startKey.Row, !r.startInclusive), true
	}
	return r.startKey, r.startInclusive
}

// effectiveEnd resolves the upper bound without copying, as effectiveStart does.
func (r *Range) effectiveEnd() (*Key, bool) {
	if r.endKey == nil {
		return nil, r.endInclusive
	}
	if r.endRowOnly {
		return firstKeyOfRow(r.endKey.Row, r.endInclusive), false
	}
	return r.endKey, r.endInclusive
}

// firstKeyOfRow returns the smallest key Accumulo can hold in row, or in the
// row that immediately follows it when following is true.
func firstKeyOfRow(row []byte, following bool) *Key {
	bound := cloneRow(row)
	if following {
		bound = append(bound, 0)
	}
	return &Key{Row: bound, Timestamp: math.MaxInt64}
}

// AfterEndKey reports whether key sorts after this range's upper bound, which
// is the Go form of Sharkbite's after_end_key.
//
// The comparison uses the range's effective bounds (the same ones
// [Range.KeyBounds] returns), so a row bound covers every cell of the row: a key
// in the end row is inside an inclusive row-bounded range and outside an
// exclusive one, whatever its column family or timestamp.
func (r *Range) AfterEndKey(key Key) bool {
	if r == nil {
		return false
	}
	end, endInclusive := r.effectiveEnd()
	if end == nil {
		return false
	}
	if endInclusive {
		return compareKeys(*end, key) < 0
	}
	return compareKeys(*end, key) <= 0
}

// BeforeStartKey reports whether key sorts before this range's lower bound,
// which is the Go form of Sharkbite's before_start_key. It resolves row bounds
// exactly as AfterEndKey does.
func (r *Range) BeforeStartKey(key Key) bool {
	if r == nil {
		return false
	}
	start, startInclusive := r.effectiveStart()
	if start == nil {
		return false
	}
	if startInclusive {
		return compareKeys(key, *start) < 0
	}
	return compareKeys(key, *start) <= 0
}

// Contains reports whether key falls inside the range: not before its start and
// not after its end. Sharkbite has no such predicate — a caller composes the
// two — but composing them is the only correct way to ask the question, so the
// composition is published rather than left to every caller.
func (r *Range) Contains(key Key) bool {
	return !r.BeforeStartKey(key) && !r.AfterEndKey(key)
}

// String renders the range the way Sharkbite's operator<< does, which is what
// its __str__ and __repr__ return: "Range " then the start bound, a comma, and
// the end bound, with a trailing space.
//
// An unbounded side is "(-inf" or "+inf) "; a bounded side is the key in
// [Key.String] form, bracketed by "[" or "(" for the start and "]" or ")" for
// the end according to inclusivity. Row bounds render as the first key of the
// row, which is how Sharkbite's Range(std::string) constructor stores them
// (Key(row) carries the maximum timestamp).
func (r *Range) String() string {
	if r == nil {
		return "Range (-inf,+inf) "
	}
	var builder strings.Builder
	builder.WriteString("Range ")
	if start := r.boundKey(true); start == nil {
		builder.WriteString("(-inf")
	} else {
		if r.startInclusive {
			builder.WriteString("[")
		} else {
			builder.WriteString("(")
		}
		builder.WriteString(start.String())
	}
	builder.WriteString(",")
	if end := r.boundKey(false); end == nil {
		builder.WriteString("+inf) ")
	} else {
		builder.WriteString(end.String())
		if r.endInclusive {
			builder.WriteString("] ")
		} else {
			builder.WriteString(") ")
		}
	}
	return builder.String()
}

// boundKey returns the declared bound as the key Sharkbite would hold: a
// row-only bound becomes the first key of that row, and a key bound is
// returned as it was given.
func (r *Range) boundKey(start bool) *Key {
	key, rowOnly := r.endKey, r.endRowOnly
	if start {
		key, rowOnly = r.startKey, r.startRowOnly
	}
	if key == nil {
		return nil
	}
	if rowOnly {
		return firstKeyOfRow(key.Row, false)
	}
	return cloneKey(key)
}

func (r *Range) routingRow() []byte {
	if r.startKey == nil {
		return nil
	}
	if r.startInclusive || !r.startRowOnly {
		return cloneRow(r.startKey.Row)
	}
	row := make([]byte, len(r.startKey.Row)+1)
	copy(row, r.startKey.Row)
	return row
}

func (r *Range) fitsTablet(tablet Tablet) bool {
	if r.endKey == nil {
		return tablet.Extent.EndRow == nil
	}
	if tablet.Extent.EndRow == nil {
		return true
	}
	rangeStop := r.endKey.Row
	if r.endInclusive {
		rangeStop = append(cloneRow(rangeStop), 0)
	}
	tabletStop := append(cloneRow(tablet.Extent.EndRow), 0)
	return bytes.Compare(rangeStop, tabletStop) <= 0
}

func (r *Range) toThrift() *data.TRange {
	out := &data.TRange{
		StartKeyInclusive: r.startInclusive,
		StopKeyInclusive:  r.endInclusive,
		InfiniteStartKey:  r.startKey == nil,
		InfiniteStopKey:   r.endKey == nil,
	}
	if r.startKey != nil {
		out.Start = keyToThrift(r.startKey)
	}
	if r.endKey != nil {
		out.Stop = keyToThrift(r.endKey)
		if r.endRowOnly && r.endInclusive {
			out.Stop.Row = append(out.Stop.Row, 0)
			out.StopKeyInclusive = false
		}
	}
	return out
}

func keyForRow(row []byte) *Key {
	if row == nil {
		return nil
	}
	return &Key{Row: cloneRow(row)}
}

func keyToThrift(key *Key) *data.TKey {
	if key == nil {
		return nil
	}
	return &data.TKey{
		Row:           cloneRow(key.Row),
		ColFamily:     cloneRow(key.ColumnFamily),
		ColQualifier:  cloneRow(key.ColumnQualifier),
		ColVisibility: cloneRow(key.ColumnVisibility),
		Timestamp:     key.Timestamp,
	}
}

func cloneKey(key *Key) *Key {
	if key == nil {
		return nil
	}
	clone := key.Clone()
	return &clone
}

// compareKeys orders range bounds. It is Key.Compare, so a range predicate can
// never disagree with the ordering the key type publishes.
func compareKeys(left, right Key) int {
	return left.Compare(right)
}
