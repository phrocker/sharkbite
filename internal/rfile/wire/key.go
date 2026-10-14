// Minimal Accumulo Key for the in-process RFile reader.
//
// Field model and ordering mirror core/.../data/Key.java exactly:
//
//	Row, ColumnFamily, ColumnQualifier, ColumnVisibility,
//	Timestamp (int64), Deleted (bool).
//
// Comparator semantics, from Key.java:1084-1128 (compareTo):
//
//	row, cf, cq, cv: byte-lexicographic ASC
//	timestamp:       Long.compare(other.ts, this.ts) — DESCENDING (newer first)
//	deleted:         deleted=true sorts BEFORE deleted=false (so a tombstone
//	                 is read before the matching live value at the same coord)
//
// We deliberately keep this struct dependency-free: the byte slices are not
// copied at construction, and there is no allocator pool. Callers that need
// to hold onto a Key past the lifetime of its source buffer must clone the
// slices themselves (RelativeKey.Read does that for fields it materialises
// from the wire).
package wire

import "bytes"

// Key is an Accumulo cell coordinate. All byte slices are immutable from
// the package's point of view; callers must not mutate them after handing
// them in.
type Key struct {
	Row              []byte
	ColumnFamily     []byte
	ColumnQualifier  []byte
	ColumnVisibility []byte
	Timestamp        int64
	Deleted          bool
}

// Clone returns a Key that owns its own copies of every byte slice. Used
// after a RelativeKey decode when the caller wants to keep the key past
// the next Read call.
func (k *Key) Clone() *Key {
	return &Key{
		Row:              cloneBytes(k.Row),
		ColumnFamily:     cloneBytes(k.ColumnFamily),
		ColumnQualifier:  cloneBytes(k.ColumnQualifier),
		ColumnVisibility: cloneBytes(k.ColumnVisibility),
		Timestamp:        k.Timestamp,
		Deleted:          k.Deleted,
	}
}

func cloneBytes(b []byte) []byte {
	if b == nil {
		return nil
	}
	out := make([]byte, len(b))
	copy(out, b)
	return out
}

// Equal reports whether two keys have identical row, cf, cq, cv,
// timestamp, and deleted flag. Treats nil and empty slices as equal —
// matches Accumulo's behaviour where an unset Text and an empty Text are
// indistinguishable on the wire.
func (k *Key) Equal(o *Key) bool {
	return bytes.Equal(k.Row, o.Row) &&
		bytes.Equal(k.ColumnFamily, o.ColumnFamily) &&
		bytes.Equal(k.ColumnQualifier, o.ColumnQualifier) &&
		bytes.Equal(k.ColumnVisibility, o.ColumnVisibility) &&
		k.Timestamp == o.Timestamp &&
		k.Deleted == o.Deleted
}

// Compare returns -1, 0, or 1 following Accumulo's PartialKey
// ROW_COLFAM_COLQUAL_COLVIS_TIME_DEL ordering (Key.java:1084-1128).
func (k *Key) Compare(o *Key) int {
	if c := bytes.Compare(k.Row, o.Row); c != 0 {
		return c
	}
	if c := bytes.Compare(k.ColumnFamily, o.ColumnFamily); c != 0 {
		return c
	}
	if c := bytes.Compare(k.ColumnQualifier, o.ColumnQualifier); c != 0 {
		return c
	}
	if c := bytes.Compare(k.ColumnVisibility, o.ColumnVisibility); c != 0 {
		return c
	}
	// Timestamp: DESCENDING. Java does Long.compare(other.ts, this.ts).
	if k.Timestamp != o.Timestamp {
		if o.Timestamp < k.Timestamp {
			return -1
		}
		return 1
	}
	// Deleted: true sorts BEFORE false. Mirrors Key.java:1121-1126:
	//   if (deleted)  result = other.deleted ?  0 : -1;
	//   else          result = other.deleted ?  1 :  0;
	switch {
	case k.Deleted && !o.Deleted:
		return -1
	case !k.Deleted && o.Deleted:
		return 1
	default:
		return 0
	}
}

// CompareRowCFCQ is the prefix comparator (PartialKey.ROW_COLFAM_COLQUAL).
// It compares row, column family, and column qualifier only, ignoring
// visibility, timestamp, and the deleted flag. This is the boundary a
// visibility-transforming pass buffers on: a transform that rewrites only
// ColumnVisibility leaves row/cf/cq fixed, so all cells that may reorder
// relative to one another share this prefix (mirrors Accumulo's
// TransformingIterator, which buffers on the part of the key the transform
// does not change).
func (k *Key) CompareRowCFCQ(o *Key) int {
	if c := bytes.Compare(k.Row, o.Row); c != 0 {
		return c
	}
	if c := bytes.Compare(k.ColumnFamily, o.ColumnFamily); c != 0 {
		return c
	}
	return bytes.Compare(k.ColumnQualifier, o.ColumnQualifier)
}

// CompareRowCFCQCV is the prefix comparator (PartialKey.ROW_COLFAM_COLQUAL_COLVIS).
// Useful when scanning ignores timestamp/deleted (e.g. seek targeting).
func (k *Key) CompareRowCFCQCV(o *Key) int {
	if c := bytes.Compare(k.Row, o.Row); c != 0 {
		return c
	}
	if c := bytes.Compare(k.ColumnFamily, o.ColumnFamily); c != 0 {
		return c
	}
	if c := bytes.Compare(k.ColumnQualifier, o.ColumnQualifier); c != 0 {
		return c
	}
	return bytes.Compare(k.ColumnVisibility, o.ColumnVisibility)
}
