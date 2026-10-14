//go:build !embed

package cclient

import (
	"bytes"

	"github.com/phrocker/shoal-oss/internal/thrift/gen/data"
)

// Column is the (cf, cq, cv) tuple used to scope a scan to particular
// columns. All three fields can be empty:
//
//   - empty cf       => "all column families"
//   - empty cq       => "any qualifier within the chosen cf"
//   - empty cv       => "any visibility"
//
// This wildcard semantics matches sharkbite's ThriftWrapper conversion
// (ThriftWrapper.h:321-337) and the Java Column (core/.../data/Column.java).
type Column struct {
	cf []byte
	cq []byte
	cv []byte
}

// NewColumn builds a Column. nil and []byte{} are stored verbatim — the
// distinction is not preserved on the wire (Thrift binary fields are
// length-prefixed). Callers that need wildcard semantics should pass nil.
func NewColumn(cf, cq, cv []byte) *Column {
	return &Column{cf: cf, cq: cq, cv: cv}
}

// NewColumnCF is a convenience constructor when only cf is meaningful.
func NewColumnCF(cf []byte) *Column { return &Column{cf: cf} }

// NewColumnCFCQ is a convenience constructor when cv is the wildcard.
func NewColumnCFCQ(cf, cq []byte) *Column { return &Column{cf: cf, cq: cq} }

// CF returns the column family.
func (c *Column) CF() []byte { return c.cf }

// CQ returns the column qualifier.
func (c *Column) CQ() []byte { return c.cq }

// CV returns the column visibility.
func (c *Column) CV() []byte { return c.cv }

// ToThrift converts to wire form. Empty/nil byte slices are preserved as
// empty slices on the wire — they must not be promoted to nil because
// the server distinguishes "field unset" from "field present but empty"
// only via Thrift's protocol-level set/unset flags, which the generated
// Go writers don't emit for a nil []byte. Mirroring the sharkbite
// behavior of always populating the field (ThriftWrapper.h:328-330).
func (c *Column) ToThrift() *data.TColumn {
	return &data.TColumn{
		ColumnFamily:     c.cf,
		ColumnQualifier:  c.cq,
		ColumnVisibility: c.cv,
	}
}

// ColumnFromThrift builds a Column from its wire form.
// Reference: ThriftWrapper.h:44-49.
func ColumnFromThrift(t *data.TColumn) *Column {
	if t == nil {
		return nil
	}
	return &Column{cf: t.ColumnFamily, cq: t.ColumnQualifier, cv: t.ColumnVisibility}
}

// Equal reports byte-for-byte equality of all three fields.
func (c *Column) Equal(o *Column) bool {
	if c == o {
		return true
	}
	if c == nil || o == nil {
		return false
	}
	return bytes.Equal(c.cf, o.cf) &&
		bytes.Equal(c.cq, o.cq) &&
		bytes.Equal(c.cv, o.cv)
}

// ColumnsToThrift is a convenience for converting a slice. nil/empty
// input returns nil (so Thrift omits the optional list field — matches
// the server's expectation of "no column filter").
func ColumnsToThrift(cs []*Column) []*data.TColumn {
	if len(cs) == 0 {
		return nil
	}
	out := make([]*data.TColumn, len(cs))
	for i, c := range cs {
		out[i] = c.ToThrift()
	}
	return out
}
