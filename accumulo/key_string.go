package accumulo

import (
	"strconv"
	"strings"
)

// String renders the key the way Sharkbite's Key::toString does, which is what
// its __str__ and __repr__ return:
//
//	row family:qualifier [visibility] timestamp
//
// The row, the family and the qualifier are escaped; the visibility is not,
// exactly as in the pinned implementation. Escaping replaces a NUL byte with
// "\u0000" and the C escapes ' " ? \ \a \b \f \n \r \t \v with their two-
// character forms; every other byte, including a byte that is not valid UTF-8,
// is written through unchanged.
func (k Key) String() string {
	var builder strings.Builder
	builder.WriteString(escapeKeyComponent(k.Row))
	builder.WriteString(" ")
	builder.WriteString(escapeKeyComponent(k.ColumnFamily))
	builder.WriteString(":")
	builder.WriteString(escapeKeyComponent(k.ColumnQualifier))
	builder.WriteString(" [")
	builder.Write(k.ColumnVisibility)
	builder.WriteString("] ")
	builder.WriteString(strconv.FormatInt(k.Timestamp, 10))
	return builder.String()
}

// escapeKeyComponent applies Sharkbite's StringUtils::getEscapedString rules.
func escapeKeyComponent(component []byte) string {
	var builder strings.Builder
	builder.Grow(len(component))
	for _, character := range component {
		switch character {
		case 0:
			builder.WriteString(`\u0000`)
		case '\'':
			builder.WriteString(`\'`)
		case '"':
			builder.WriteString(`\"`)
		case '?':
			builder.WriteString(`\?`)
		case '\\':
			builder.WriteString(`\\`)
		case '\a':
			builder.WriteString(`\a`)
		case '\b':
			builder.WriteString(`\b`)
		case '\f':
			builder.WriteString(`\f`)
		case '\n':
			builder.WriteString(`\n`)
		case '\r':
			builder.WriteString(`\r`)
		case '\t':
			builder.WriteString(`\t`)
		case '\v':
			builder.WriteString(`\v`)
		default:
			builder.WriteByte(character)
		}
	}
	return builder.String()
}
