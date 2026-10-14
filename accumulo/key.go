package accumulo

import "bytes"

// DefaultKeyTimestamp is the timestamp a key carries when the caller does not
// supply one. It matches the pinned Sharkbite constructor default.
const DefaultKeyTimestamp int64 = 9223372036854775807

// NewKey builds a key that names a row and nothing else. The row is copied, so
// the caller may reuse its slice.
func NewKey(row []byte) Key {
	return Key{Row: cloneRow(row), Timestamp: DefaultKeyTimestamp}
}

// NewKeyWithColumns builds a fully specified key. Every slice is copied.
func NewKeyWithColumns(row, family, qualifier, visibility []byte, timestamp int64) Key {
	return Key{
		Row:              cloneRow(row),
		ColumnFamily:     cloneRow(family),
		ColumnQualifier:  cloneRow(qualifier),
		ColumnVisibility: cloneRow(visibility),
		Timestamp:        timestamp,
	}
}

// Clone returns a deep copy that shares no storage with the receiver.
func (k Key) Clone() Key {
	return Key{
		Row:              cloneRow(k.Row),
		ColumnFamily:     cloneRow(k.ColumnFamily),
		ColumnQualifier:  cloneRow(k.ColumnQualifier),
		ColumnVisibility: cloneRow(k.ColumnVisibility),
		Timestamp:        k.Timestamp,
		Deleted:          k.Deleted,
	}
}

// Empty reports whether the key carries no bytes in any component, which is
// how the pinned type defines empty.
func (k Key) Empty() bool {
	return len(k.Row) == 0 && len(k.ColumnFamily) == 0 &&
		len(k.ColumnQualifier) == 0 && len(k.ColumnVisibility) == 0
}

// SetRow replaces the row with a copy of row.
func (k *Key) SetRow(row []byte) { k.Row = cloneRow(row) }

// SetColumnFamily replaces the column family with a copy of family.
func (k *Key) SetColumnFamily(family []byte) { k.ColumnFamily = cloneRow(family) }

// SetColumnQualifier replaces the column qualifier with a copy of qualifier.
func (k *Key) SetColumnQualifier(qualifier []byte) { k.ColumnQualifier = cloneRow(qualifier) }

// SetColumnVisibility replaces the column visibility with a copy of visibility.
func (k *Key) SetColumnVisibility(visibility []byte) { k.ColumnVisibility = cloneRow(visibility) }

// SetTimestamp replaces the timestamp.
func (k *Key) SetTimestamp(timestamp int64) { k.Timestamp = timestamp }

// SetDeleted marks the key as a deletion marker or clears the mark.
func (k *Key) SetDeleted(deleted bool) { k.Deleted = deleted }

// RowSize returns the row length in bytes.
func (k Key) RowSize() int { return len(k.Row) }

// ColumnFamilySize returns the column family length in bytes.
func (k Key) ColumnFamilySize() int { return len(k.ColumnFamily) }

// ColumnQualifierSize returns the column qualifier length in bytes.
func (k Key) ColumnQualifierSize() int { return len(k.ColumnQualifier) }

// ColumnVisibilitySize returns the column visibility length in bytes.
func (k Key) ColumnVisibilitySize() int { return len(k.ColumnVisibility) }

// Length returns the number of bytes the key's components occupy, counting the
// eight bytes of the timestamp. Size is the same number: a Go key holds no
// spare capacity to report separately.
func (k Key) Length() int {
	return k.RowSize() + k.ColumnFamilySize() + k.ColumnQualifierSize() +
		k.ColumnVisibilitySize() + 8
}

// Size returns the number of bytes the key's components occupy.
func (k Key) Size() int { return k.Length() }

// Compare orders two keys the way Accumulo does: by row, column family,
// column qualifier and column visibility ascending, then by timestamp
// descending so the newest version sorts first, then with deletion markers
// before live entries. It is the ROW_COLFAM_COLQUAL_COLVIS_TIME_DEL ordering
// `internal/rfile/wire` already uses. It returns a negative number, zero, or a
// positive number.
func (k Key) Compare(other Key) int {
	if order := bytes.Compare(k.Row, other.Row); order != 0 {
		return order
	}
	if order := bytes.Compare(k.ColumnFamily, other.ColumnFamily); order != 0 {
		return order
	}
	if order := bytes.Compare(k.ColumnQualifier, other.ColumnQualifier); order != 0 {
		return order
	}
	if order := bytes.Compare(k.ColumnVisibility, other.ColumnVisibility); order != 0 {
		return order
	}
	switch {
	case other.Timestamp < k.Timestamp:
		return -1
	case other.Timestamp > k.Timestamp:
		return 1
	}
	switch {
	case k.Deleted == other.Deleted:
		return 0
	case k.Deleted:
		return -1
	default:
		return 1
	}
}

// CompareToVisibility orders two keys by row, column family, column qualifier
// and then column visibility, ignoring the timestamp and the deletion marker.
func (k Key) CompareToVisibility(other Key) int {
	if order := bytes.Compare(k.Row, other.Row); order != 0 {
		return order
	}
	if order := bytes.Compare(k.ColumnFamily, other.ColumnFamily); order != 0 {
		return order
	}
	if order := bytes.Compare(k.ColumnQualifier, other.ColumnQualifier); order != 0 {
		return order
	}
	return bytes.Compare(k.ColumnVisibility, other.ColumnVisibility)
}

// Less reports whether the key sorts before other under Compare. It agrees
// with Equal: two keys are ordered unless Compare finds them identical.
func (k Key) Less(other Key) bool { return k.Compare(other) < 0 }

// LessOrEqual reports whether the key sorts before other or matches it.
func (k Key) LessOrEqual(other Key) bool { return k.Compare(other) <= 0 }

// Equal reports whether two keys name the same row, column and version. It is
// exactly Compare(other) == 0, so ordering and equality never disagree.
func (k Key) Equal(other Key) bool { return k.Compare(other) == 0 }
