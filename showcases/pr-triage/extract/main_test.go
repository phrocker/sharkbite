// Licensed under the Apache License, Version 2.0. See LICENSE and NOTICE.
package main

import (
	"strings"
	"testing"
)

func TestExactDeclarationsAndUTF8Offsets(t *testing.T) {
	source := "// header\npackage p\n\nconst Tenant = \"é\"\n\n// Check denies.\nfunc Check() bool { return allowed() }\n"
	result := extract(request{Path: "p.go", Content: source})
	if result.Error != "" || len(result.Declarations) != 2 {
		t.Fatalf("%+v", result)
	}
	for _, d := range result.Declarations {
		if source[d.Start:d.End] != d.Text {
			t.Fatal("source span mismatch")
		}
	}
	fn := result.Declarations[1]
	if fn.StartLine != 6 || fn.EndLine != 7 || fn.Kind != "function" || len(fn.Calls) != 1 || fn.Calls[0] != "allowed" {
		t.Fatalf("%+v", fn)
	}
	if !strings.HasPrefix(fn.Text, "// Check denies.") {
		t.Fatal("lost attached comment")
	}
}
func TestMethodsInitAndBodylessFunctions(t *testing.T) {
	result := extract(request{Path: "p.go", Content: "package p\ntype T int\nfunc (t *T) Check() {}\nfunc init() {}\nfunc init() {}\nfunc assembly()\n"})
	if result.Error != "" || len(result.Declarations) != 5 {
		t.Fatalf("%+v", result)
	}
	seen := map[string]bool{}
	for _, d := range result.Declarations {
		if seen[d.Key] {
			t.Fatalf("duplicate %s", d.Key)
		}
		seen[d.Key] = true
	}
	if !seen["function:*T.Check"] || !seen["function:init#2"] {
		t.Fatalf("%v", seen)
	}
}
func TestParseFailureIsExplicit(t *testing.T) {
	result := extract(request{Path: "broken.go", Content: "package p\nfunc Broken( {"})
	if result.Error == "" {
		t.Fatal("parse failure hidden")
	}
}
