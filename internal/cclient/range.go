//go:build !embed

package cclient

import (
	"bytes"
	"errors"
	"fmt"

	"github.com/phrocker/shoal-oss/internal/thrift/gen/data"
)

// Range represents an inclusive/exclusive scan range over rows.
//
// Internally we keep the *raw* startRow/endRow and the user-supplied
// inclusive flags. Conversion to wire form happens in ToThrift, where the
// "inclusive endRow" trick (append 0x00) is applied — matching sharkbite
// Range.cpp:51-59. Keeping the raw form avoids mutating user data on
// construction and lets ToThrift be deterministic.
//
// References:
//   - Java:      core/.../data/Range.java:120-167
//   - sharkbite: src/data/constructs/Range.cpp:33-65
type Range struct {
	startRow         []byte
	endRow           []byte
	startInclusive   bool
	endInclusive     bool
	infiniteStartKey bool
	infiniteStopKey  bool
}

// NewRange builds a row-bounded range. nil startRow => infinite start;
// nil endRow => infinite end. Inclusive flags are honored on the wire by
// the Java semantics (endRow inclusive → append 0x00 → exclusive).
//
// When both bounds are non-nil, the resulting effective stop must not be
// strictly less than the effective start. See Range.cpp:60-64 /
// Range.java:163-166.
func NewRange(startRow []byte, startInclusive bool, endRow []byte, endInclusive bool) (*Range, error) {
	sr := normalizeRow(startRow)
	er := normalizeRow(endRow)
	r := &Range{
		startRow:         sr,
		endRow:           er,
		startInclusive:   startInclusive,
		endInclusive:     endInclusive,
		infiniteStartKey: sr == nil,
		infiniteStopKey:  er == nil,
	}
	if !r.infiniteStartKey && !r.infiniteStopKey {
		// After expansion (if endInclusive), endRow gets a 0x00 byte,
		// so it's strictly greater than itself — only an actual ordering
		// inversion (endRow < startRow) is invalid.
		if bytes.Compare(er, sr) < 0 {
			return nil, fmt.Errorf("cclient: Range endRow (%q) < startRow (%q)", er, sr)
		}
	}
	return r, nil
}

// NewRangeRow returns a single-row inclusive range — equivalent to
// `NewRange(row, true, row, true)`. Mirrors Range.cpp:31 and the
// Java `Range(Text row)` overload (Range.java:85).
func NewRangeRow(row []byte) (*Range, error) {
	if len(row) == 0 {
		return nil, errors.New("cclient: NewRangeRow: empty row (use InfiniteRange)")
	}
	return NewRange(row, true, row, true)
}

// InfiniteRange returns the unbounded range (-inf, +inf). Matches the
// default-constructed sharkbite Range (Range.cpp:22-29).
func InfiniteRange() *Range {
	return &Range{
		startInclusive:   true,
		endInclusive:     false,
		infiniteStartKey: true,
		infiniteStopKey:  true,
	}
}

// StartRow returns the raw start row (nil if infinite).
func (r *Range) StartRow() []byte { return r.startRow }

// EndRow returns the raw end row (nil if infinite).
func (r *Range) EndRow() []byte { return r.endRow }

// StartInclusive reports whether the start row is included.
func (r *Range) StartInclusive() bool { return r.startInclusive }

// EndInclusive reports whether the end row is included.
func (r *Range) EndInclusive() bool { return r.endInclusive }

// InfiniteStartKey reports whether the start is unbounded.
func (r *Range) InfiniteStartKey() bool { return r.infiniteStartKey }

// InfiniteStopKey reports whether the stop is unbounded.
func (r *Range) InfiniteStopKey() bool { return r.infiniteStopKey }

// ToThrift converts to wire form. Critical correctness rules:
//
//  1. infiniteStartKey == true → Start = nil (NOT a zero TKey).
//     Same for stop. Required by the wire-protocol nil-for-absent
//     contract — see metadata/walker.go fullRange() for prior art.
//  2. endInclusive == true && finite endRow → append a 0x00 byte to
//     the row and write StopKeyInclusive = false. This is sharkbite's
//     trick (Range.cpp:51-59) and the canonical Java behavior in
//     Range.java:120-128 where an inclusive endRow is converted to
//     `new Key(endRow).followingKey(PartialKey.ROW)` — appending 0x00
//     to the row produces the lexicographically smallest key that
//     starts a "later" row, so an exclusive comparison against it
//     covers the original endRow inclusively.
//  3. startInclusive flag passes through unchanged — the Java and
//     sharkbite paths both encode start inclusivity via the flag, not
//     by mutating the row.
func (r *Range) ToThrift() *data.TRange {
	tr := &data.TRange{
		StartKeyInclusive: r.startInclusive,
		StopKeyInclusive:  r.endInclusive,
		InfiniteStartKey:  r.infiniteStartKey,
		InfiniteStopKey:   r.infiniteStopKey,
	}
	if !r.infiniteStartKey {
		// Start = TKey with just the Row populated. cf/cq/cv/timestamp
		// stay zero — the server expands them out for "anything after
		// this row".
		tr.Start = &data.TKey{Row: r.startRow}
	}
	if !r.infiniteStopKey {
		row := r.endRow
		if r.endInclusive {
			// Append 0x00, flip flag to exclusive — Range.cpp:51-59.
			row = append(append(make([]byte, 0, len(r.endRow)+1), r.endRow...), 0x00)
			tr.StopKeyInclusive = false
		}
		tr.Stop = &data.TKey{Row: row}
	}
	return tr
}

// RangeFromThrift inverts ToThrift. Note: the inclusive-row-with-0x00
// transformation is irreversible without an out-of-band marker, so the
// returned Range carries the Thrift-side (post-transform) bytes verbatim
// and faithfully replays its inclusivity flags. Callers using FromThrift
// purely for echo/validation should compare against ToThrift output, not
// against the original constructor arguments.
func RangeFromThrift(t *data.TRange) (*Range, error) {
	if t == nil {
		return nil, errors.New("cclient: nil TRange")
	}
	r := &Range{
		startInclusive:   t.StartKeyInclusive,
		endInclusive:     t.StopKeyInclusive,
		infiniteStartKey: t.InfiniteStartKey,
		infiniteStopKey:  t.InfiniteStopKey,
	}
	if t.Start != nil && !t.InfiniteStartKey {
		r.startRow = normalizeRow(t.Start.Row)
		if r.startRow == nil {
			r.infiniteStartKey = true
		}
	}
	if t.Stop != nil && !t.InfiniteStopKey {
		r.endRow = normalizeRow(t.Stop.Row)
		if r.endRow == nil {
			r.infiniteStopKey = true
		}
	}
	return r, nil
}

// String renders for human eyes. Mimics sharkbite Range.h:142-156.
func (r *Range) String() string {
	var sb bytes.Buffer
	sb.WriteString("Range ")
	if r.infiniteStartKey {
		sb.WriteString("(-inf")
	} else if r.startInclusive {
		fmt.Fprintf(&sb, "[%q", r.startRow)
	} else {
		fmt.Fprintf(&sb, "(%q", r.startRow)
	}
	sb.WriteString(",")
	if r.infiniteStopKey {
		sb.WriteString("+inf)")
	} else if r.endInclusive {
		fmt.Fprintf(&sb, "%q]", r.endRow)
	} else {
		fmt.Fprintf(&sb, "%q)", r.endRow)
	}
	return sb.String()
}
