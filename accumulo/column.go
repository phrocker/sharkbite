package accumulo

import "bytes"

// NewColumnWithVisibility selects one family, qualifier and visibility. Every
// slice is copied, so the caller may reuse its buffers.
func NewColumnWithVisibility(family, qualifier, visibility []byte) Column {
	return Column{
		family:     cloneRow(family),
		qualifier:  cloneRow(qualifier),
		visibility: cloneRow(visibility),
	}
}

// Visibility returns a defensive copy of the selected visibility. A nil
// visibility means every visibility in the column.
func (c Column) Visibility() []byte { return cloneRow(c.visibility) }

// SetFamily replaces the column family with a copy of family.
func (c *Column) SetFamily(family []byte) { c.family = cloneRow(family) }

// SetQualifier replaces the qualifier with a copy of qualifier.
func (c *Column) SetQualifier(qualifier []byte) { c.qualifier = cloneRow(qualifier) }

// SetVisibility replaces the visibility with a copy of visibility.
func (c *Column) SetVisibility(visibility []byte) { c.visibility = cloneRow(visibility) }

// Compare orders two columns by family, then qualifier, then visibility. It
// returns a negative number, zero, or a positive number.
func (c Column) Compare(other Column) int {
	if order := bytes.Compare(c.family, other.family); order != 0 {
		return order
	}
	if order := bytes.Compare(c.qualifier, other.qualifier); order != 0 {
		return order
	}
	return bytes.Compare(c.visibility, other.visibility)
}

// Less reports whether the column sorts before other under Compare.
func (c Column) Less(other Column) bool { return c.Compare(other) < 0 }

// Equal reports whether two columns select the same family, qualifier and
// visibility. It is Compare(other) == 0, so ordering and equality never
// disagree, and one comparison answers for every caller.
func (c Column) Equal(other Column) bool { return c.Compare(other) == 0 }
