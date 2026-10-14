package accumulo

import (
	"bytes"
	"fmt"
	"sort"
	"strings"
)

// Authorizations is the set of visibility labels a scan or a user carries.
//
// It is the Go form of Sharkbite's Authorizations, and it keeps that type's
// observable contract: the labels are a *set*, so duplicates collapse and
// iteration is in sorted order (Sharkbite stores them in a std::set), an empty
// set is legal for scanning and writing, and a label is arbitrary bytes rather
// than text, so a non-UTF-8 label round-trips unchanged.
//
// Sharkbite declares a character rule for labels but never applies it: its
// validateAuths() is unreachable because no constructor and no addAuthorization
// call it, so every pinned program can hold any bytes. Shoal therefore accepts
// any label as well, and exposes the rule as Validate for callers that want it.
// See the compatibility matrix, SB-UNSAFE-034.
//
// The zero value is an empty, usable set, and every method is safe on a nil
// receiver so a caller that never built one behaves like a caller that built an
// empty one.
type Authorizations struct {
	labels [][]byte
}

// NewAuthorizations builds the set holding labels. Duplicates collapse, order
// does not matter, and every label is copied, so later mutation of the caller's
// slices cannot change the set.
func NewAuthorizations(labels ...[]byte) *Authorizations {
	auths := &Authorizations{}
	for _, label := range labels {
		auths.Add(label)
	}
	return auths
}

// NewAuthorizationStrings is the convenience form for labels that are text,
// which is how Sharkbite's Python constructor is always called.
func NewAuthorizationStrings(labels ...string) *Authorizations {
	auths := &Authorizations{}
	for _, label := range labels {
		auths.Add([]byte(label))
	}
	return auths
}

// Add inserts a label and reports whether the set changed. Adding a label the
// set already holds is a no-op, matching set insertion in Sharkbite.
func (a *Authorizations) Add(label []byte) bool {
	if a == nil {
		return false
	}
	index, found := a.search(label)
	if found {
		return false
	}
	a.labels = append(a.labels, nil)
	copy(a.labels[index+1:], a.labels[index:])
	a.labels[index] = cloneRow(label)
	if a.labels[index] == nil {
		a.labels[index] = []byte{}
	}
	return true
}

// Remove drops a label and reports whether the set changed. Sharkbite has no
// removal, but a set that can only grow cannot express dropping a label a user
// lost, and the operation cannot break a Sharkbite program that never calls it.
func (a *Authorizations) Remove(label []byte) bool {
	if a == nil {
		return false
	}
	index, found := a.search(label)
	if !found {
		return false
	}
	a.labels = append(a.labels[:index], a.labels[index+1:]...)
	return true
}

// Contains reports whether the set holds the label, comparing bytes exactly.
func (a *Authorizations) Contains(label []byte) bool {
	if a == nil {
		return false
	}
	_, found := a.search(label)
	return found
}

// List returns the labels in sorted order, as copies. It is the Go form of
// Sharkbite's get_authorizations, whose std::set iteration is sorted too.
func (a *Authorizations) List() [][]byte {
	if a == nil || len(a.labels) == 0 {
		return nil
	}
	out := make([][]byte, len(a.labels))
	for i, label := range a.labels {
		out[i] = append([]byte(nil), label...)
	}
	return out
}

// Strings returns the labels in sorted order as strings, for callers that know
// their labels are text.
func (a *Authorizations) Strings() []string {
	if a == nil || len(a.labels) == 0 {
		return nil
	}
	out := make([]string, len(a.labels))
	for i, label := range a.labels {
		out[i] = string(label)
	}
	return out
}

// Len reports how many labels the set holds.
func (a *Authorizations) Len() int {
	if a == nil {
		return 0
	}
	return len(a.labels)
}

// Empty reports whether the set holds no labels. An empty set is legal
// everywhere an authorization set is accepted.
func (a *Authorizations) Empty() bool { return a.Len() == 0 }

// Equal reports whether two sets hold the same labels, which is what
// Sharkbite's operator== compares.
func (a *Authorizations) Equal(other *Authorizations) bool {
	if a.Len() != other.Len() {
		return false
	}
	for i := 0; i < a.Len(); i++ {
		if !bytes.Equal(a.labels[i], other.labels[i]) {
			return false
		}
	}
	return true
}

// Clone returns an independent copy.
func (a *Authorizations) Clone() *Authorizations {
	if a == nil {
		return nil
	}
	return &Authorizations{labels: a.List()}
}

// String renders the set the way Sharkbite's __str__ and __repr__ do:
// "[ a, b ]" for a populated set.
//
// **Safe shim for unsafe C++ behavior:** the pinned bindings build that text by
// dereferencing vec.end()-1 and vec.back() on the label vector, which is
// undefined behavior for the empty set that every pinned test constructs. Shoal
// renders "[ ]" instead of crashing; see the compatibility matrix,
// SB-UNSAFE-001.
func (a *Authorizations) String() string {
	if a.Empty() {
		return "[ ]"
	}
	var builder strings.Builder
	builder.WriteString("[ ")
	for i, label := range a.labels {
		if i > 0 {
			builder.WriteString(", ")
		}
		builder.Write(label)
	}
	builder.WriteString(" ]")
	return builder.String()
}

// Validate reports the first label that holds a character outside the set
// Sharkbite declares valid: ASCII letters, digits, '_', '-' and ':'.
//
// It applies exactly the rule the pinned validateAuths() walks, and nothing
// more: that loop only inspects the characters a label holds, so an empty label
// passes there and passes here. Nothing in Shoal calls it, exactly as nothing in
// Sharkbite calls its equivalent. It exists so a caller that wants the declared
// rule can apply it before a label reaches a cluster, without Shoal silently
// rejecting labels the pinned release accepts.
func (a *Authorizations) Validate() error {
	if a == nil {
		return nil
	}
	for _, label := range a.labels {
		for _, character := range label {
			if !ValidAuthorizationCharacter(character) {
				return fmt.Errorf(
					"%w: authorization %q holds invalid character %q",
					ErrInvalidAuthorizations, label, rune(character),
				)
			}
		}
	}
	return nil
}

// ValidAuthorizationCharacter reports whether a byte is one Sharkbite's
// AuthsInit::buildDefaultAuths marks valid: 'a'-'z', 'A'-'Z', '0'-'9', '_',
// '-' and ':'.
func ValidAuthorizationCharacter(character byte) bool {
	switch {
	case character >= 'a' && character <= 'z':
		return true
	case character >= 'A' && character <= 'Z':
		return true
	case character >= '0' && character <= '9':
		return true
	case character == '_' || character == '-' || character == ':':
		return true
	default:
		return false
	}
}

// search returns the position the label occupies, or would occupy, and whether
// it is already present.
func (a *Authorizations) search(label []byte) (int, bool) {
	index := sort.Search(len(a.labels), func(i int) bool {
		return bytes.Compare(a.labels[i], label) >= 0
	})
	found := index < len(a.labels) && bytes.Equal(a.labels[index], label)
	return index, found
}
