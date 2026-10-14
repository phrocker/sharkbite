package accumulo

import (
	"sort"
	"strconv"
	"strings"
	"sync"
)

// noCopy marks a type as non-copyable after first use. `go vet -copylocks`
// recognizes the Lock method on an embedded field and reports accidental
// copies.
type noCopy struct{}

func (*noCopy) Lock()   {}
func (*noCopy) Unlock() {}

// Configuration is a mutable, string-keyed client configuration map. It is
// the Go equivalent of Sharkbite's cclient::impl::Configuration, which client
// code builds before constructing an instance and reads back through
// Instance.Configuration.
//
// A Configuration is safe for concurrent use, and its zero value is ready to
// use: reads report nothing set, and the first Set allocates the map under the
// lock. NewConfiguration exists so that a Configuration can be built and
// populated in one expression.
//
// A Configuration must not be copied after first use: it holds a mutex, and
// its map is reference-backed, so a copy would guard the same entries with a
// different lock. Pass it by pointer and use Clone when an independent copy is
// wanted. `go vet`'s copylocks check enforces this.
type Configuration struct {
	_      noCopy
	mu     sync.RWMutex
	values map[string]string
}

// NewConfiguration returns an empty Configuration.
func NewConfiguration() *Configuration {
	return &Configuration{values: make(map[string]string)}
}

// Set stores value under name, replacing any previous value.
func (c *Configuration) Set(name, value string) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.values == nil {
		c.values = make(map[string]string)
	}
	c.values[name] = value
}

// Get returns the value stored under name, or the empty string when name is
// unset. This mirrors Sharkbite's Configuration::get(name), which returns
// get(name, "").
func (c *Configuration) Get(name string) string {
	value, _ := c.Lookup(name)
	return value
}

// GetOr returns the value stored under name, or def when name is unset. This
// mirrors Sharkbite's Configuration::get(name, def).
func (c *Configuration) GetOr(name, def string) string {
	value, ok := c.Lookup(name)
	if !ok {
		return def
	}
	return value
}

// Lookup returns the value stored under name and whether it was set. It is
// the Go-idiomatic form of Get; Sharkbite has no equivalent, because its
// get(name) cannot distinguish "unset" from "set to the empty string".
func (c *Configuration) Lookup(name string) (string, bool) {
	if c == nil {
		return "", false
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	value, ok := c.values[name]
	return value, ok
}

// GetUint32 returns the value stored under name parsed as an unsigned 32-bit
// integer, or 0 when name is unset or its value is not a number.
//
// Sharkbite's Configuration::getLong(name) evaluates atol(get(name, 0)) and
// truncates the long to uint32_t: a missing key or an unparsable value both
// yield 0, a negative value wraps, and an out-of-range value is undefined
// behavior in C. Shoal keeps the "unset or unparsable is 0" contract, which
// is what callers depend on, and replaces the undefined cases with saturation
// (see GetUint32Or).
func (c *Configuration) GetUint32(name string) uint32 {
	return c.GetUint32Or(name, 0)
}

// GetUint32Or returns the value stored under name parsed as an unsigned
// 32-bit integer. It returns def when name is unset, and 0 when the stored
// value is not a number, mirroring Sharkbite's
// Configuration::getLong(name, def).
//
// Parsing accepts an optional sign and leading and trailing spaces, matching
// the leading-numeric-prefix behavior of C's atol. Values above the uint32
// range saturate to math.MaxUint32 and negative values saturate to 0, instead
// of wrapping or invoking undefined behavior as the C implementation does.
func (c *Configuration) GetUint32Or(name string, def uint32) uint32 {
	value, ok := c.Lookup(name)
	if !ok {
		return def
	}
	return parseUint32Prefix(value)
}

// Keys returns the configured names in sorted order.
func (c *Configuration) Keys() []string {
	if c == nil {
		return nil
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	if len(c.values) == 0 {
		return nil
	}
	keys := make([]string, 0, len(c.values))
	for key := range c.values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

// Len returns the number of configured names.
func (c *Configuration) Len() int {
	if c == nil {
		return 0
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	return len(c.values)
}

// Clone returns an independent copy of the configuration. Sharkbite copies its
// configuration map through the ZookeeperInstance copy constructor; Shoal
// makes the copy explicit so that a Configuration handed to an Instance cannot
// be mutated through an alias or by copying the mutex-bearing Configuration
// value itself.
func (c *Configuration) Clone() *Configuration {
	clone := NewConfiguration()
	if c == nil {
		return clone
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	for key, value := range c.values {
		clone.values[key] = value
	}
	return clone
}

// parseUint32Prefix parses the leading integer of value the way C's atol
// does, then saturates into the uint32 range. Unparsable input yields 0.
func parseUint32Prefix(value string) uint32 {
	trimmed := strings.TrimSpace(value)
	end := 0
	if end < len(trimmed) && (trimmed[end] == '+' || trimmed[end] == '-') {
		end++
	}
	digits := end
	for digits < len(trimmed) && trimmed[digits] >= '0' && trimmed[digits] <= '9' {
		digits++
	}
	if digits == end {
		return 0
	}
	parsed, err := strconv.ParseInt(trimmed[:digits], 10, 64)
	if err != nil {
		// Out of int64 range: saturate on the sign of the literal.
		if strings.HasPrefix(trimmed, "-") {
			return 0
		}
		return ^uint32(0)
	}
	if parsed <= 0 {
		return 0
	}
	if parsed > int64(^uint32(0)) {
		return ^uint32(0)
	}
	return uint32(parsed)
}
