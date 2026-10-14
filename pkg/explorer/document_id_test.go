// Licensed to the Apache Software Foundation (ASF) under one or more
// contributor license agreements. See the NOTICE file distributed with this
// work for additional information regarding copyright ownership.
package explorer

import (
	"context"
	"strings"
	"testing"
)

// TestEveryDocumentIDHasTheReservedShape: fleet dispatch refuses action IDs
// for which IsDocumentID holds, so that no action can occupy a document's
// guard entity {'D', document ID} (#669). That works only while every ID a
// document is published under has the shape. Whatever changes how a document
// ID is derived must keep the shape, or move the reservation with it.
func TestEveryDocumentIDHasTheReservedShape(t *testing.T) {
	for _, uri := range []string{
		"a.md", "https://example.com/release-notes", "upload://durable.md",
		strings.Repeat("x", 4096), "", "\x00",
	} {
		if id := stableID(documentIDKind, uri); !IsDocumentID([]byte(id)) {
			t.Errorf("document ID %q for %q is outside the reserved shape", id, uri)
		}
	}
	e, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()
	result, err := e.Ingest(context.Background(), Source{
		URI: "notes/ingested.md", Title: "Ingested", MediaType: MediaTypeMarkdown,
		Content: "# Ingested\n\nBody.\n",
	})
	if err != nil {
		t.Fatal(err)
	}
	if id := result.Document.ID; !IsDocumentID([]byte(id)) {
		t.Fatalf("an ingested document's ID %q is outside the reserved shape", id)
	}
}

func TestIsDocumentIDIsExact(t *testing.T) {
	valid := "doc_" + strings.Repeat("0123456789abcdef", 2)
	if !IsDocumentID([]byte(valid)) {
		t.Fatalf("%q is a document ID", valid)
	}
	for _, id := range []string{
		"", "doc_", valid[:len(valid)-1], valid + "0",
		"doc_" + strings.Repeat("0123456789ABCDEF", 2),
		"rev_" + valid[4:], "Doc_" + valid[4:], "doc-" + valid[4:],
		"doc_" + strings.Repeat("g", 32),
		"xdoc_" + valid[5:],
	} {
		if IsDocumentID([]byte(id)) {
			t.Errorf("%q is not a document ID", id)
		}
	}
}
