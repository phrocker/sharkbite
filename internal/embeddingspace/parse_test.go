// Licensed to the Apache Software Foundation (ASF) under one
// or more contributor license agreements. See the NOTICE file distributed
// with this work for additional information regarding copyright ownership.
// The ASF licenses this file to you under the Apache License, Version 2.0.
package embeddingspace

import "testing"

// TestParseRoundTripsString pins Parse as the inverse of String, which
// is what makes an operator-supplied flag value mean the same thing the
// logs print back at them.
func TestParseRoundTripsString(t *testing.T) {
	for _, want := range []FileState{
		NoEmbeddings(),
		Unknown(),
		Has("space-a"),
		Has("a:b"),
	} {
		got, err := Parse(want.String())
		if err != nil {
			t.Fatalf("Parse(%q): %v", want.String(), err)
		}
		if got != want {
			t.Fatalf("Parse(%q) = %+v, want %+v", want.String(), got, want)
		}
	}
}

// TestParseEmptyIsNotAState: the empty string means "nothing
// configured", which callers must be able to tell apart from an explicit
// "unknown" — the first takes their own default, the second is a
// deliberate declaration.
func TestParseEmptyIsNotAState(t *testing.T) {
	got, err := Parse("")
	if err != nil {
		t.Fatal(err)
	}
	if got != (FileState{}) {
		t.Fatalf("Parse(\"\") = %+v, want the zero value", got)
	}
	if got == Unknown() {
		t.Fatal("the empty string must not parse to an explicit unknown")
	}
}

func TestParseRejectsGarbage(t *testing.T) {
	for _, text := range []string{
		"no_such_state",
		"has_embeddings",
		"has_embeddings:",
		"has_embeddings: space-a",
		"no_embeddings:space-a",
		"unknown:space-a",
		// A trailing colon is a truncated command line, not a
		// shorthand. Splitting leaves an empty identity, which
		// validates for these two states, so without an explicit
		// refusal "--default-embedding=no_embeddings:" would be read
		// as a positive assertion the operator never finished making.
		"no_embeddings:",
		"unknown:",
	} {
		if got, err := Parse(text); err == nil {
			t.Fatalf("Parse(%q) = %+v, want an error", text, got)
		}
	}
}
