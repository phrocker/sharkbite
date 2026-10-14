// Licensed under the Apache License, Version 2.0. See LICENSE and NOTICE.
package main

import (
	"strings"
	"testing"
)

func TestProjectionAndIdentityFacts(t *testing.T) {
	r := extract(`func F(req Request, d Decision) Key { req.RequestID=d.RequestID(); visibility:=d.Label(); return Key{ColumnVisibility: visibility} }`)
	if r.Error != "" {
		t.Fatal(r.Error)
	}
	s := strings.Join(r.Facts, "\n")
	for _, want := range []string{"ASSIGN = req.RequestID FROM d.RequestID()", "CONSTRUCT Key KEY ColumnVisibility FROM visibility"} {
		if !strings.Contains(s, want) {
			t.Fatalf("missing %s", want)
		}
	}
}
func TestInvalidEvidenceNotEmptySuccess(t *testing.T) {
	if extract("func broken(").Error == "" {
		t.Fatal("parse failure lost")
	}
}

func TestParallelAssignmentDoesNotInventFlows(t *testing.T) {
	r := extract(`func F() { a,b=x,y; a+=z; c,d:=pair(); var e,f=g,h }`)
	text := strings.Join(r.Facts, "\n")
	for _, bad := range []string{"ASSIGN = a FROM y", "ASSIGN = b FROM x", "DECLARE e FROM h", "DECLARE f FROM g"} {
		if strings.Contains(text, bad) {
			t.Fatal(bad)
		}
	}
	for _, want := range []string{"ASSIGN = a FROM x", "ASSIGN += a FROM z", "TUPLE_ASSIGN_UNRESOLVED c, d := pair()", "DECLARE e FROM g"} {
		if !strings.Contains(text, want) {
			t.Fatal(want)
		}
	}
}

func TestCallbackAndLimitsAreVisible(t *testing.T) {
	r := extract(`func F(){ f(func(){deny()}) }`)
	text := strings.Join(r.Facts, "\n")
	if !strings.Contains(text, "ARG TO f FROM func()") || !strings.Contains(text, "CALL deny") {
		t.Fatal(text)
	}
	r = extract("func F(){" + strings.Repeat("f();", 600) + "}")
	if r.Omitted == 0 {
		t.Fatal("fact limit silently dropped evidence")
	}
}
