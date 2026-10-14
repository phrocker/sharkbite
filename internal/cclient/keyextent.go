//go:build !embed

// Package cclient holds the user-facing ("cooked") data types for talking
// to Accumulo: KeyExtent, Range, Authorizations, Column, IterInfo, Mutation.
// They wrap the generated Thrift structs in `internal/thrift/gen/data` with
// validation and convert to/from the wire form via ToThrift/FromThrift.
//
// The naming + layering mirrors sharkbite's `cclient::data` namespace
// (include/data/constructs/) and the canonical Apache Accumulo Java types
// in `core/src/main/java/org/apache/accumulo/core/data/`.
package cclient

import (
	"bytes"
	"errors"
	"fmt"

	"github.com/phrocker/shoal-oss/internal/thrift/gen/data"
)

// KeyExtent identifies a single tablet: (tableID, endRow, prevEndRow).
//
// endRow == nil  => last tablet in the table (no upper bound)
// prevEndRow == nil => first tablet in the table (no lower bound)
//
// References:
//   - Java:      core/.../dataImpl/KeyExtent.java:67-102
//   - sharkbite: include/data/constructs/KeyExtent.h:71-84
type KeyExtent struct {
	tableID    string
	endRow     []byte
	prevEndRow []byte
}

// NewKeyExtent constructs a KeyExtent. tableID must be non-empty
// (KeyExtent.java:95 / KeyExtent.h:72-74). When both endRow and
// prevEndRow are non-nil, prevEndRow must be < endRow
// (KeyExtent.java:96-99 / KeyExtent.h:75-79).
//
// Nil and empty []byte are treated identically — both mean "absent" — to
// match the Java semantics where Text(null) and the absent boundary cases
// collapse on the wire (see KeyExtent.java:130-132).
func NewKeyExtent(tableID string, endRow, prevEndRow []byte) (*KeyExtent, error) {
	if tableID == "" {
		return nil, errors.New("cclient: KeyExtent tableID must be non-empty")
	}
	er := normalizeRow(endRow)
	per := normalizeRow(prevEndRow)
	if er != nil && per != nil && bytes.Compare(per, er) >= 0 {
		return nil, fmt.Errorf("cclient: KeyExtent prevEndRow (%q) >= endRow (%q)", per, er)
	}
	return &KeyExtent{tableID: tableID, endRow: er, prevEndRow: per}, nil
}

// normalizeRow collapses a zero-length slice to nil so callers don't have
// to distinguish between the two on the wire. Mirrors Java's null-vs-empty
// Text handling.
func normalizeRow(b []byte) []byte {
	if len(b) == 0 {
		return nil
	}
	return b
}

// TableID returns the table id this extent belongs to.
func (k *KeyExtent) TableID() string { return k.tableID }

// EndRow returns the inclusive upper boundary row, or nil for "no boundary".
func (k *KeyExtent) EndRow() []byte { return k.endRow }

// PrevEndRow returns the exclusive lower boundary row, or nil for "no boundary".
func (k *KeyExtent) PrevEndRow() []byte { return k.prevEndRow }

// ToThrift converts to the wire form. Absent (nil) rows stay nil — never
// zero-length []byte — to match the nil-for-absent semantics already in
// place in metadata/walker.go.
//
// Reference: KeyExtent.java:129-133.
func (k *KeyExtent) ToThrift() *data.TKeyExtent {
	return &data.TKeyExtent{
		Table:      []byte(k.tableID),
		EndRow:     k.endRow,
		PrevEndRow: k.prevEndRow,
	}
}

// FromThrift constructs a KeyExtent from its wire form. Re-runs constructor
// validation, so a malformed wire extent (empty table, prev>=end) errors.
//
// Reference: KeyExtent.java:114-124.
func KeyExtentFromThrift(t *data.TKeyExtent) (*KeyExtent, error) {
	if t == nil {
		return nil, errors.New("cclient: nil TKeyExtent")
	}
	return NewKeyExtent(string(t.Table), t.EndRow, t.PrevEndRow)
}

// Equal reports byte-for-byte equality of all three fields.
func (k *KeyExtent) Equal(o *KeyExtent) bool {
	if k == o {
		return true
	}
	if k == nil || o == nil {
		return false
	}
	return k.tableID == o.tableID &&
		bytes.Equal(k.endRow, o.endRow) &&
		bytes.Equal(k.prevEndRow, o.prevEndRow)
}

// String renders the extent in sharkbite's `tableId:X end:Y prev:Z` shape
// (KeyExtent.h:165-170), with `<` standing in for an absent boundary.
func (k *KeyExtent) String() string {
	end := "<"
	if k.endRow != nil {
		end = string(k.endRow)
	}
	prev := "<"
	if k.prevEndRow != nil {
		prev = string(k.prevEndRow)
	}
	return fmt.Sprintf("tableId:%s end:%s prev:%s", k.tableID, end, prev)
}
