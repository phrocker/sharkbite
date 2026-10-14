// Licensed to the Apache Software Foundation (ASF) under one or more
// contributor license agreements. See the NOTICE file distributed with this
// work for additional information regarding copyright ownership.

package narrate

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/text/feature/plural"
	"golang.org/x/text/language"
)

//go:embed catalog/en.json
var englishCatalog []byte

// CatalogFile is the on-disk shape of a catalog. A translation is a new file
// of this shape; no Go code changes.
type CatalogFile struct {
	// Locale is a BCP 47 tag. It selects the CLDR plural rules.
	Locale string `json:"locale"`
	// Formats holds the locale's number and time conventions.
	Formats CatalogFormats `json:"formats"`
	// Messages maps a catalog key to an ICU-style pattern.
	Messages map[string]string `json:"messages"`
}

// CatalogFormats are the non-message conventions of a locale.
type CatalogFormats struct {
	// Time is a Go reference-time layout. Times are converted to UTC before
	// formatting, so the layout must say so (for example "… UTC").
	Time string `json:"time"`
	// Group separates thousands; Decimal separates the fraction.
	Group   string `json:"group"`
	Decimal string `json:"decimal"`
}

// Catalog is a parsed, complete message catalog.
type Catalog struct {
	tag      language.Tag
	formats  CatalogFormats
	messages map[string][]node
	// observe, when set by a test, sees every key formatted.
	observe func(key string)
}

// English returns the built-in English catalog.
func English() *Catalog {
	c, err := ParseCatalog(englishCatalog)
	if err != nil {
		panic("narrate: built-in English catalog is invalid: " + err.Error())
	}
	return c
}

// ParseCatalog parses and checks a catalog.
//
// A catalog is refused unless it has a message for every key in
// RequiredKeys and nothing else, every pattern parses, and every pattern reads
// only the arguments its English counterpart reads, in a compatible way. This is the coverage
// rule applied at load: a translation that lacks a template cannot be loaded,
// so it can never fall back to an unreviewed sentence at run time.
func ParseCatalog(data []byte) (*Catalog, error) {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var file CatalogFile
	if err := decoder.Decode(&file); err != nil {
		return nil, fmt.Errorf("narrate: catalog: %w", err)
	}
	tag, err := language.Parse(file.Locale)
	if err != nil {
		return nil, fmt.Errorf("narrate: catalog locale: %w", err)
	}
	if file.Formats.Time == "" || file.Formats.Decimal == "" {
		return nil, fmt.Errorf("narrate: catalog formats are incomplete")
	}
	if !strings.Contains(time.Date(2001, 2, 3, 4, 5, 6, 0, time.UTC).
		Format(file.Formats.Time), "2001") {
		return nil, fmt.Errorf("narrate: catalog time layout omits the year")
	}
	c := &Catalog{tag: tag, formats: file.Formats, messages: map[string][]node{}}
	required := map[string]bool{}
	for _, key := range RequiredKeys() {
		required[key] = true
	}
	var problems []string
	for key, pattern := range file.Messages {
		if !required[key] {
			problems = append(problems, "unexpected key "+key)
			continue
		}
		nodes, err := parsePattern(pattern)
		if err != nil {
			problems = append(problems, key+": "+err.Error())
			continue
		}
		c.messages[key] = nodes
	}
	for key := range required {
		if _, ok := file.Messages[key]; !ok {
			problems = append(problems, "missing key "+key)
		}
	}
	reference, err := englishUses()
	if err != nil {
		return nil, err
	}
	for key, nodes := range c.messages {
		uses := map[string][]argUse{}
		argUses(nodes, uses)
		for name, list := range uses {
			for _, use := range list {
				if !compatibleUse(use, reference[key][name]) {
					problems = append(problems, fmt.Sprintf(
						"%s: argument %q: %s", key, name, incompatibility(use, reference[key][name])))
				}
			}
		}
	}
	if len(problems) > 0 {
		sort.Strings(problems)
		return nil, fmt.Errorf("narrate: catalog: %s", strings.Join(problems, "; "))
	}
	return c, nil
}

// englishUses is the argument contract: for each key, the arguments the
// English message reads and how. A translation may read only these.
var englishUses = sync.OnceValues(func() (map[string]map[string][]argUse, error) {
	var file CatalogFile
	if err := json.Unmarshal(englishCatalog, &file); err != nil {
		return nil, fmt.Errorf("narrate: built-in catalog: %w", err)
	}
	out := map[string]map[string][]argUse{}
	for key, pattern := range file.Messages {
		nodes, err := parsePattern(pattern)
		if err != nil {
			return nil, fmt.Errorf("narrate: built-in catalog %s: %w", key, err)
		}
		uses := map[string][]argUse{}
		argUses(nodes, uses)
		out[key] = uses
	}
	return out, nil
})

// Keys returns the catalog's keys, sorted.
func (c *Catalog) Keys() []string {
	keys := make([]string, 0, len(c.messages))
	for key := range c.messages {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

// ArgumentNames returns the argument names a message reads, sorted.
func (c *Catalog) ArgumentNames(key string) []string {
	names := map[string][]argUse{}
	argUses(c.messages[key], names)
	out := make([]string, 0, len(names))
	for name := range names {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// format renders one message to a fragment.
func (c *Catalog) format(key string, args Args) (Fragment, error) {
	nodes, ok := c.messages[key]
	if !ok {
		return Fragment{}, fmt.Errorf("narrate: no message %q", key)
	}
	if c.observe != nil {
		c.observe(key)
	}
	f := formatter{catalog: c}
	if err := f.run(nodes, args, nil, 0); err != nil {
		return Fragment{}, fmt.Errorf("narrate: message %q: %w", key, err)
	}
	return Fragment{text: f.out.String(), quotes: f.quotes, spans: f.spans}, nil
}

func (c *Catalog) pluralCategory(n int64) string {
	// Clamp before negating: -math.MinInt64 overflows back to itself.
	if n > math.MaxInt32 || n < -math.MaxInt32 {
		n = math.MaxInt32
	} else if n < 0 {
		n = -n
	}
	switch plural.Cardinal.MatchPlural(c.tag, int(n), 0, 0, 0, 0) {
	case plural.Zero:
		return "zero"
	case plural.One:
		return "one"
	case plural.Two:
		return "two"
	case plural.Few:
		return "few"
	case plural.Many:
		return "many"
	}
	return "other"
}

func (c *Catalog) integer(n int64) string {
	if n < 0 {
		return "-" + c.group(strconv.FormatUint(uint64(-(n+1))+1, 10))
	}
	return c.group(strconv.FormatInt(n, 10))
}

func (c *Catalog) unsigned(n uint64) string { return c.group(strconv.FormatUint(n, 10)) }

// group inserts the locale's thousands separator into a run of digits.
// Four-digit numbers are left ungrouped, as CLDR's minimum grouping does for
// English.
func (c *Catalog) group(digits string) string {
	if c.formats.Group == "" || len(digits) <= 4 {
		return digits
	}
	var b strings.Builder
	lead := len(digits) % 3
	if lead > 0 {
		b.WriteString(digits[:lead])
	}
	for i := lead; i < len(digits); i += 3 {
		if b.Len() > 0 {
			b.WriteString(c.formats.Group)
		}
		b.WriteString(digits[i : i+3])
	}
	return b.String()
}

// decimal renders a probability or other fraction. It rounds to three places
// unless rounding would land on 0 or 1 when the value is neither, in which
// case it prints the value exactly: a reported 0.9996 must not read as
// certainty.
func (c *Catalog) decimal(v float64) string {
	if math.IsNaN(v) || math.IsInf(v, 0) {
		return strconv.FormatFloat(v, 'f', -1, 64)
	}
	s := strconv.FormatFloat(v, 'f', 3, 64)
	rounded, _ := strconv.ParseFloat(s, 64)
	if (rounded == 0 || rounded == 1 || rounded == -1) && rounded != v {
		s = strconv.FormatFloat(v, 'f', -1, 64)
	}
	if strings.Contains(s, ".") {
		s = strings.TrimRight(strings.TrimRight(s, "0"), ".")
	}
	return strings.Replace(s, ".", c.formats.Decimal, 1)
}

func (c *Catalog) time(t time.Time) string {
	if t.IsZero() {
		fr, err := c.format("time.unrecorded", Args{})
		if err != nil {
			return ""
		}
		return fr.text
	}
	return t.UTC().Format(c.formats.Time)
}

// duration renders the two most significant units, truncating toward zero so
// a lease is never described as longer than it is.
//
// A negative duration is refused rather than printed: every duration the
// renderer states (a lease, time left before a lease or window ends) is
// positive on a valid record, and printing its magnitude would state a time
// the record does not hold.
func (c *Catalog) duration(d time.Duration) (Fragment, error) {
	if d < 0 {
		return Fragment{}, fmt.Errorf("negative duration %v", d)
	}
	if d < time.Second {
		if d == 0 {
			return c.format("duration.seconds", Args{"n": 0})
		}
		return c.format("duration.subsecond", Args{})
	}
	units := []struct {
		key  string
		size time.Duration
	}{
		{"duration.days", 24 * time.Hour},
		{"duration.hours", time.Hour},
		{"duration.minutes", time.Minute},
		{"duration.seconds", time.Second},
	}
	var parts []Fragment
	for i, unit := range units {
		n := int64(d / unit.size)
		if n == 0 {
			continue
		}
		first, err := c.format(unit.key, Args{"n": n})
		if err != nil {
			return Fragment{}, err
		}
		parts = append(parts, first)
		if i+1 < len(units) {
			rest := int64((d % unit.size) / units[i+1].size)
			if rest > 0 {
				second, err := c.format(units[i+1].key, Args{"n": rest})
				if err != nil {
					return Fragment{}, err
				}
				parts = append(parts, second)
			}
		}
		break
	}
	if len(parts) == 2 {
		return c.format("duration.pair", Args{"first": parts[0], "second": parts[1]})
	}
	return parts[0], nil
}

// list joins items with the locale's list patterns. Items are fragments the
// caller already rendered, so joining adds only catalog text.
func (c *Catalog) list(items []Fragment, style string) (Fragment, error) {
	if style == "" {
		style = "and"
	}
	switch len(items) {
	case 0:
		return c.format("list.empty", Args{})
	case 1:
		return items[0], nil
	case 2:
		return c.format("list."+style+".pair", Args{"a": items[0], "b": items[1]})
	}
	acc, err := c.format("list."+style+".end",
		Args{"a": items[len(items)-2], "b": items[len(items)-1]})
	if err != nil {
		return Fragment{}, err
	}
	for i := len(items) - 3; i >= 0; i-- {
		acc, err = c.format("list."+style+".join", Args{"a": items[i], "b": acc})
		if err != nil {
			return Fragment{}, err
		}
	}
	return acc, nil
}
