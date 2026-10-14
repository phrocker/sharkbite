// Licensed to the Apache Software Foundation (ASF) under one or more
// contributor license agreements. See the NOTICE file distributed with this
// work for additional information regarding copyright ownership.

package narrate

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/phrocker/shoal-oss/pkg/explorer/fleet"
)

func testCatalog(t *testing.T, locale string, messages map[string]string) *Catalog {
	t.Helper()
	var file CatalogFile
	if err := json.Unmarshal(englishCatalog, &file); err != nil {
		t.Fatal(err)
	}
	file.Locale = locale
	for k, v := range messages {
		file.Messages[k] = v
	}
	data, err := json.Marshal(file)
	if err != nil {
		t.Fatal(err)
	}
	c, err := ParseCatalog(data)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func format(t *testing.T, pattern string, args Args) string {
	t.Helper()
	c := English()
	nodes, err := parsePattern(pattern)
	if err != nil {
		t.Fatalf("parse %q: %v", pattern, err)
	}
	c.messages["probe"] = nodes
	fr, err := c.format("probe", args)
	if err != nil {
		t.Fatalf("format %q: %v", pattern, err)
	}
	return fr.text
}

func TestMessageFormat(t *testing.T) {
	when := time.Date(2026, 1, 2, 3, 4, 5, 0, time.FixedZone("X", 3600))
	cases := []struct {
		pattern string
		args    Args
		want    string
	}{
		{"plain text", Args{}, "plain text"},
		{"{n, plural, =0 {none} one {# item} other {# items}}", Args{"n": 0}, "none"},
		{"{n, plural, =0 {none} one {# item} other {# items}}", Args{"n": 1}, "1 item"},
		{"{n, plural, =0 {none} one {# item} other {# items}}", Args{"n": 12345}, "12,345 items"},
		{"{n, plural, one {#} other {#}}", Args{"n": uint64(18446744073709551615)}, "18,446,744,073,709,551,615"},
		{"{n, number}", Args{"n": uint64(18446744073709551615)}, "18,446,744,073,709,551,615"},
		{"{n, number}", Args{"n": int64(-1234567)}, "-1,234,567"},
		{"{n, number}", Args{"n": 2048}, "2048"},
		{"{s, select, a {A} other {O}}", Args{"s": Selector("a")}, "A"},
		{"{s, select, a {A} other {O}}", Args{"s": Selector("zzz")}, "O"},
		{"{t, time}", Args{"t": when}, "2026-01-02 02:04:05 UTC"},
		{"{t, time}", Args{"t": time.Time{}}, "an unrecorded time"},
		{"{d, duration}", Args{"d": 90 * time.Minute}, "1 hour 30 minutes"},
		{"{d, duration}", Args{"d": 49*time.Hour + 59*time.Minute}, "2 days 1 hour"},
		{"{d, duration}", Args{"d": time.Second}, "1 second"},
		{"{d, duration}", Args{"d": 500 * time.Millisecond}, "less than a second"},
		{"{d, duration}", Args{"d": time.Duration(0)}, "0 seconds"},
		{"{p, number}", Args{"p": 0.75}, "0.75"},
		{"{p, number}", Args{"p": 0.33333}, "0.333"},
		{"{p, number}", Args{"p": 0.99996}, "0.99996"},
		{"{p, number}", Args{"p": 0.00001}, "0.00001"},
		{"{p, number}", Args{"p": 1.0}, "1"},
		{"{p, number}", Args{"p": 0.0}, "0"},
		{"{l, list}", Args{"l": []Fragment{{text: "a"}}}, "a"},
		{"{l, list}", Args{"l": []Fragment{{text: "a"}, {text: "b"}}}, "a and b"},
		{"{l, list}", Args{"l": []Fragment{{text: "a"}, {text: "b"}, {text: "c"}, {text: "d"}}}, "a, b, c, and d"},
		{"{l, list, or}", Args{"l": []Fragment{{text: "a"}, {text: "b"}, {text: "c"}}}, "a; b; or c"},
		{"it's '{'literal'}' and ''quoted''", Args{}, "it's {literal} and 'quoted'"},
		{"{n, plural, other {'#' is #}}", Args{"n": 3}, "# is 3"},
		{"{a}{b}", Args{"a": Fragment{text: "{b}"}, "b": Fragment{text: "#"}}, "{b}#"},
		{"{s, select, x {{n, plural, one {one {n}} other {many}}} other {}}", Args{"s": Selector("x"), "n": 1}, "one 1"},
	}
	for _, tc := range cases {
		if got := format(t, tc.pattern, tc.args); got != tc.want {
			t.Errorf("%q with %v = %q, want %q", tc.pattern, tc.args, got, tc.want)
		}
	}
}

func TestMessageFormatRefusals(t *testing.T) {
	for _, bad := range []string{
		"{", "}", "{n", "{n, plural, one {x}}", "{n, select, =1 {x} other {y}}",
		"{n, plural, few {x} lots {y} other {z}}", "{n, unknown}", "{n, list, xor}",
		"{n, time, short}", "'{unterminated", "{, number}",
		"{n, plural, one {a} one {b} other {c}}",
		strings.Repeat("{n, select, other {", 20) + strings.Repeat("}}", 20),
	} {
		if _, err := parsePattern(bad); err == nil {
			t.Errorf("pattern %q parsed", bad)
		}
	}
	c := English()
	nodes, _ := parsePattern("{x}")
	c.messages["probe"] = nodes
	for _, arg := range []any{"a raw string", []string{"a"}, struct{}{}, nil} {
		if _, err := c.format("probe", Args{"x": arg}); err == nil {
			t.Errorf("argument %#v was accepted", arg)
		}
	}
	if _, err := c.format("probe", Args{}); err == nil {
		t.Error("missing argument was accepted")
	}
}

func TestCatalogLoadRules(t *testing.T) {
	var file CatalogFile
	if err := json.Unmarshal(englishCatalog, &file); err != nil {
		t.Fatal(err)
	}
	load := func(edit func(*CatalogFile)) error {
		var copyFile CatalogFile
		_ = json.Unmarshal(englishCatalog, &copyFile)
		edit(&copyFile)
		data, _ := json.Marshal(copyFile)
		_, err := ParseCatalog(data)
		return err
	}
	if err := load(func(*CatalogFile) {}); err != nil {
		t.Fatalf("English does not load: %v", err)
	}
	for name, edit := range map[string]func(*CatalogFile){
		"missing key": func(f *CatalogFile) { delete(f.Messages, "dispatch.error.outcome_unknown.either") },
		"extra key":   func(f *CatalogFile) { f.Messages["dispatch.error.made_up"] = "x" },
		"bad pattern": func(f *CatalogFile) { f.Messages["dispatch.next.none"] = "{oops" },
		"bad locale":  func(f *CatalogFile) { f.Locale = "not a locale!" },
		"no year":     func(f *CatalogFile) { f.Formats.Time = "15:04" },
		"no decimal":  func(f *CatalogFile) { f.Formats.Decimal = "" },
	} {
		if err := load(edit); err == nil {
			t.Errorf("%s: catalog loaded", name)
		}
	}
	if _, err := ParseCatalog([]byte(`{"locale":"en","formats":{},"messages":{},"extra":1}`)); err == nil {
		t.Error("unknown catalog field accepted")
	}
	// Every English message reads only arguments, never raw text, and each
	// one parses: the load above proves it. Keys are exactly RequiredKeys.
	if strings.Join(English().Keys(), ",") != strings.Join(RequiredKeys(), ",") {
		t.Error("English keys differ from RequiredKeys")
	}
}

// TestTranslation shows a catalog is data: a different locale with its own
// plural rules and separators renders through the same code.
func TestTranslation(t *testing.T) {
	c := testCatalog(t, "pl", map[string]string{
		"dispatch.history.claims": "{claims, plural, one {Zajęte raz.} few {Zajęte # razy (few).} many {Zajęte # razy (many).} other {Zajęte # razy.}}",
	})
	c.formats.Group, c.formats.Decimal = " ", ","
	r := New(c)
	for fence, want := range map[uint64]string{
		1: "Zajęte raz.", 3: "Zajęte 3 razy (few).", 5: "Zajęte 5 razy (many).", 22: "Zajęte 22 razy (few).",
	} {
		record := action(fleet.DispatchClaimed)
		record.ClaimFence = fence
		sentences, err := r.Action(record, Options{})
		if err != nil {
			t.Fatal(err)
		}
		found := false
		for _, s := range sentences {
			if s.Key == "dispatch.history.claims" {
				found = true
				if s.Text != want {
					t.Errorf("fence %d: %q, want %q", fence, s.Text, want)
				}
			}
		}
		if !found {
			t.Errorf("fence %d: no claims sentence", fence)
		}
	}
	if got := c.decimal(0.25); got != "0,25" {
		t.Errorf("decimal separator: %q", got)
	}
}
