package cclient

import (
	"errors"
	"fmt"
	"sort"
	"strings"
)

// Authorizations is the user's set of authorization labels for visibility
// filtering. Order on the wire is sorted/canonical (matches Java's
// `getAuthorizationsBB()` which iterates a sorted list — Authorizations.java:262-269).
//
// Duplicates are dropped silently (sharkbite Authorizations.h:95 keeps a
// std::set) but empty labels are an error (Authorizations.java:100-102).
//
// References:
//   - Java:      core/.../security/Authorizations.java:213-229
//   - sharkbite: include/data/constructs/security/Authorizations.h:60-93
type Authorizations struct {
	auths []string // canonical sorted, deduped, never contains empty string
}

// NewAuthorizations constructs a deduplicated, sorted authorization set
// from the given labels. Each label is trimmed (Authorizations.java:224)
// and validated against the canonical char-class:
//
//	[a-zA-Z0-9_\-:./]
//
// Mirrors Authorizations.java:73-90.
func NewAuthorizations(labels ...string) (*Authorizations, error) {
	seen := make(map[string]struct{}, len(labels))
	out := make([]string, 0, len(labels))
	for _, raw := range labels {
		l := strings.TrimSpace(raw)
		if l == "" {
			return nil, errors.New("cclient: empty authorization label")
		}
		if err := validateAuth(l); err != nil {
			return nil, err
		}
		if _, dup := seen[l]; dup {
			continue
		}
		seen[l] = struct{}{}
		out = append(out, l)
	}
	sort.Strings(out)
	return &Authorizations{auths: out}, nil
}

// EmptyAuthorizations is the canonical no-auths object — equivalent to
// Authorizations.EMPTY (Authorizations.java:57).
func EmptyAuthorizations() *Authorizations {
	return &Authorizations{auths: nil}
}

// ParseAuthorizations builds Authorizations from a comma-separated string
// (e.g. `"PUBLIC,RESTRICTED,SECRET"`). An empty string yields the empty
// set rather than an error.
func ParseAuthorizations(s string) (*Authorizations, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return EmptyAuthorizations(), nil
	}
	return NewAuthorizations(strings.Split(s, ",")...)
}

// validateAuth enforces the same character class as
// Authorizations.java:73-90 / sharkbite Authorizations.h:81-93. We
// accept the named ASCII ranges plus `_-:./`. A more permissive
// variant (quoted/base64-encoded auths) would belong in a follow-up.
func validateAuth(s string) error {
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'a' && c <= 'z':
		case c >= 'A' && c <= 'Z':
		case c >= '0' && c <= '9':
		case c == '_' || c == '-' || c == ':' || c == '.' || c == '/':
		default:
			return fmt.Errorf("cclient: invalid authorization character %q in %q", c, s)
		}
	}
	return nil
}

// Strings returns the canonical (sorted, deduped) string form. The
// returned slice is a copy.
func (a *Authorizations) Strings() []string {
	out := make([]string, len(a.auths))
	copy(out, a.auths)
	return out
}

// String renders comma-separated, e.g. `PUBLIC,SECRET`. Matches Java's
// Authorizations.toString (Authorizations.java:272-280).
func (a *Authorizations) String() string {
	return strings.Join(a.auths, ",")
}

// Contains reports whether the given label is in the set.
func (a *Authorizations) Contains(label string) bool {
	for _, x := range a.auths {
		if x == label {
			return true
		}
	}
	return false
}

// Empty reports whether there are zero authorizations.
func (a *Authorizations) Empty() bool { return len(a.auths) == 0 }

// ToThrift returns the wire form expected by TabletScanClientService.startScan
// — a list-of-bytes (`[][]byte`). This matches Java's
// `Authorizations.getAuthorizationsBB()` (Authorizations.java:262-269) which
// returns a List<ByteBuffer> in sorted order.
//
// nil and an empty slice are equivalent on the wire; we return nil so the
// Thrift layer omits the optional field.
func (a *Authorizations) ToThrift() [][]byte {
	if len(a.auths) == 0 {
		return nil
	}
	out := make([][]byte, len(a.auths))
	for i, s := range a.auths {
		out[i] = []byte(s)
	}
	return out
}

// AuthorizationsFromThrift inverts ToThrift. Bytes are interpreted as
// UTF-8 strings — the Java side does the same (it constructs ByteSequence
// from `str.getBytes(UTF_8)` in Authorizations.java:225).
func AuthorizationsFromThrift(t [][]byte) (*Authorizations, error) {
	if len(t) == 0 {
		return EmptyAuthorizations(), nil
	}
	labels := make([]string, len(t))
	for i, b := range t {
		labels[i] = string(b)
	}
	return NewAuthorizations(labels...)
}
