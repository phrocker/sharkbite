package accumulo

import (
	"bytes"
	"errors"
	"fmt"
	"strings"
)

// MetadataTableID is the table that holds tablet metadata rows.
const MetadataTableID = "!0"

// ErrInvalidTabletExtent reports an extent that cannot describe a tablet.
var ErrInvalidTabletExtent = errors.New("accumulo: invalid tablet extent")

// rootTabletExtent is the value RootTabletExtent hands out copies of. It is
// unexported so no caller can rewrite the extent every other caller reads.
var rootTabletExtent = TabletExtent{
	TableID: MetadataTableID,
	EndRow:  []byte(MetadataEntry(MetadataTableID, nil)),
}

// RootTabletExtent returns the extent of the tablet that holds the metadata
// table's own entries, which is where tablet lookups start. Each call returns
// a fresh value whose slices the caller owns.
func RootTabletExtent() TabletExtent { return rootTabletExtent.Clone() }

// MetadataEntry renders the metadata row that names a tablet: "<table>;<row>"
// for a bounded tablet and "<table><" for the last tablet of the table, whose
// end row is nil.
func MetadataEntry(tableID string, endRow []byte) string {
	if endRow == nil {
		return tableID + "<"
	}
	return tableID + ";" + string(endRow)
}

// NewTabletExtent builds an extent. A nil endRow means the tablet has no upper
// bound and a nil prevRow means it has no lower bound, which is how the
// discovery layer already reads the two fields; a non-nil empty slice is the
// empty row, a bound like any other. Both slices are copied.
func NewTabletExtent(tableID string, endRow, prevRow []byte) (TabletExtent, error) {
	if tableID == "" {
		return TabletExtent{}, fmt.Errorf("%w: table id is empty", ErrInvalidTabletExtent)
	}
	if endRow != nil && prevRow != nil && bytes.Compare(prevRow, endRow) >= 0 {
		return TabletExtent{}, fmt.Errorf(
			"%w: previous end row %q is not before end row %q",
			ErrInvalidTabletExtent,
			prevRow,
			endRow,
		)
	}
	return TabletExtent{
		TableID: tableID,
		EndRow:  cloneRow(endRow),
		PrevRow: cloneRow(prevRow),
	}, nil
}

// ParseTabletExtent decodes a metadata row - "<table>;<row>" or "<table><" -
// into an extent, taking the previous end row from the caller. The row is
// searched byte by byte, so a table id or end row of any length decodes.
func ParseTabletExtent(metadataRow string, prevRow []byte) (TabletExtent, error) {
	if metadataRow == "" {
		return TabletExtent{}, fmt.Errorf("%w: metadata row is empty", ErrInvalidTabletExtent)
	}
	// The bounded form wins: an end row may itself end in '<', and only a row
	// with no separator at all names the tablet with no upper bound.
	if separator := strings.IndexByte(metadataRow, ';'); separator >= 0 {
		return NewTabletExtent(
			metadataRow[:separator],
			[]byte(metadataRow[separator+1:]),
			prevRow,
		)
	}
	if strings.HasSuffix(metadataRow, "<") {
		return NewTabletExtent(metadataRow[:len(metadataRow)-1], nil, prevRow)
	}
	return TabletExtent{}, fmt.Errorf(
		"%w: metadata row %q contains neither ';' nor a trailing '<'",
		ErrInvalidTabletExtent,
		metadataRow,
	)
}

// Clone returns a copy that shares no storage with the receiver.
func (e TabletExtent) Clone() TabletExtent {
	return TabletExtent{
		TableID: e.TableID,
		EndRow:  cloneRow(e.EndRow),
		PrevRow: cloneRow(e.PrevRow),
	}
}

// SetTableID replaces the table id.
func (e *TabletExtent) SetTableID(tableID string) { e.TableID = tableID }

// SetPrevRow replaces the previous end row with a copy of prevRow.
func (e *TabletExtent) SetPrevRow(prevRow []byte) { e.PrevRow = cloneRow(prevRow) }

// MetadataEntry renders the metadata row that names this tablet.
func (e TabletExtent) MetadataEntry() string { return MetadataEntry(e.TableID, e.EndRow) }

// Range returns the rows the tablet holds: everything after the previous end
// row up to and including the end row. An absent bound is unbounded.
func (e TabletExtent) Range() (*Range, error) {
	return NewRange(e.PrevRow, false, e.EndRow, true)
}

// Contains reports whether row falls inside the tablet.
func (e TabletExtent) Contains(row []byte) bool {
	if e.PrevRow != nil && bytes.Compare(row, e.PrevRow) <= 0 {
		return false
	}
	return e.EndRow == nil || bytes.Compare(row, e.EndRow) <= 0
}

// Compare orders two extents by table id, then by end row, then by previous
// end row. A nil end row is the tablet with no upper bound, so it sorts last;
// a nil previous end row is the tablet with no lower bound, so it sorts first.
// A non-nil empty slice is the empty row and orders like any other bound. It
// returns a negative number, zero, or a positive number.
func (e TabletExtent) Compare(other TabletExtent) int {
	if order := strings.Compare(e.TableID, other.TableID); order != 0 {
		return order
	}
	if order := compareUpperBound(e.EndRow, other.EndRow); order != 0 {
		return order
	}
	return compareLowerBound(e.PrevRow, other.PrevRow)
}

// compareUpperBound orders end rows, treating an absent bound as the largest.
func compareUpperBound(left, right []byte) int {
	switch {
	case left == nil && right == nil:
		return 0
	case left == nil:
		return 1
	case right == nil:
		return -1
	default:
		return bytes.Compare(left, right)
	}
}

// compareLowerBound orders previous end rows, treating an absent bound as the
// smallest.
func compareLowerBound(left, right []byte) int {
	switch {
	case left == nil && right == nil:
		return 0
	case left == nil:
		return -1
	case right == nil:
		return 1
	default:
		return bytes.Compare(left, right)
	}
}

// Less reports whether the extent sorts before other under Compare.
func (e TabletExtent) Less(other TabletExtent) bool { return e.Compare(other) < 0 }

// Equal reports whether two extents describe the same tablet. It is
// Compare(other) == 0, so ordering and equality never disagree.
func (e TabletExtent) Equal(other TabletExtent) bool { return e.Compare(other) == 0 }

// String renders the extent in the pinned Sharkbite form, where an absent
// bound is written as "<".
func (e TabletExtent) String() string {
	return fmt.Sprintf(
		"tableId:%s end:%s prev:%s",
		e.TableID,
		boundText(e.EndRow),
		boundText(e.PrevRow),
	)
}

func boundText(bound []byte) string {
	if bound == nil {
		return "<"
	}
	return string(bound)
}
