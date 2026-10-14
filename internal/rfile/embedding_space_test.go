// Licensed to the Apache Software Foundation (ASF) under one
// or more contributor license agreements. See the NOTICE file distributed
// with this work for additional information regarding copyright ownership.
// The ASF licenses this file to you under the Apache License, Version 2.0.
package rfile

import (
	"bytes"
	"testing"

	"github.com/phrocker/shoal-oss/internal/embeddingspace"
	"github.com/phrocker/shoal-oss/internal/rfile/bcfile"
	"github.com/phrocker/shoal-oss/internal/rfile/bcfile/block"
)

func TestEmbeddingSpaceMetadataStates(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   embeddingspace.FileState
		want embeddingspace.FileState
	}{
		{"has embeddings", embeddingspace.Has("space-a"), embeddingspace.Has("space-a")},
		{"no embeddings", embeddingspace.NoEmbeddings(), embeddingspace.NoEmbeddings()},
		{"unknown", embeddingspace.Unknown(), embeddingspace.Unknown()},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			w, err := NewWriter(&buf, WriterOptions{EmbeddingSpace: tc.in})
			if err != nil {
				t.Fatal(err)
			}
			if err := w.Append(&Key{Row: []byte("r"), ColumnFamily: []byte("cf"), Timestamp: 1}, []byte("v")); err != nil {
				t.Fatal(err)
			}
			if err := w.Close(); err != nil {
				t.Fatal(err)
			}
			bc, err := bcfile.NewReader(bytes.NewReader(buf.Bytes()), int64(buf.Len()))
			if err != nil {
				t.Fatal(err)
			}
			got, err := ReadEmbeddingSpaceMetadata(bc, block.Default())
			if err != nil {
				t.Fatal(err)
			}
			if got != tc.want {
				t.Fatalf("embedding space = %+v, want %+v", got, tc.want)
			}
		})
	}
}

func TestMissingEmbeddingSpaceMetadataIsUnknown(t *testing.T) {
	var buf bytes.Buffer
	w, err := NewWriter(&buf, WriterOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if err := w.Append(&Key{Row: []byte("r"), ColumnFamily: []byte("cf"), Timestamp: 1}, []byte("v")); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	bc, err := bcfile.NewReader(bytes.NewReader(buf.Bytes()), int64(buf.Len()))
	if err != nil {
		t.Fatal(err)
	}
	got, err := ReadEmbeddingSpaceMetadata(bc, block.Default())
	if err != nil {
		t.Fatal(err)
	}
	if got != embeddingspace.Unknown() {
		t.Fatalf("missing metadata = %+v, want unknown", got)
	}
}
