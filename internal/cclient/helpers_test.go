package cclient

import "strings"

// contains is a tiny test helper to avoid an extra import everywhere we
// match on substring of an error message.
func contains(haystack, needle string) bool {
	return strings.Contains(haystack, needle)
}
