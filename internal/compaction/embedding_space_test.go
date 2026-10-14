// Licensed to the Apache Software Foundation (ASF) under one
// or more contributor license agreements. See the NOTICE file distributed
// with this work for additional information regarding copyright ownership.
// The ASF licenses this file to you under the Apache License, Version 2.0.
package compaction

import (
	"bytes"
	"errors"
	"testing"

	"github.com/phrocker/shoal-oss/internal/embeddingspace"
	"github.com/phrocker/shoal-oss/internal/iterrt"
	"github.com/phrocker/shoal-oss/internal/rfile"
	"github.com/phrocker/shoal-oss/internal/rfile/bcfile"
	"github.com/phrocker/shoal-oss/internal/rfile/bcfile/block"
	"github.com/phrocker/shoal-oss/internal/rfile/wire"
)

func TestCompactRejectsDifferingEmbeddingSpaces(t *testing.T) {
	a := buildRFileInSpace(t, "a", embeddingspace.Has("space-a"))
	b := buildRFileInSpace(t, "b", embeddingspace.Has("space-b"))
	_, err := Compact(Spec{
		Inputs: []Input{
			{Name: "a.rf", Bytes: a},
			{Name: "b.rf", Bytes: b},
		},
		Scope: iterrt.ScopeMajc,
	})
	if !errors.Is(err, embeddingspace.ErrMismatch) {
		t.Fatalf("Compact error = %v, want ErrMismatch", err)
	}
}

func TestCompactPropagatesCompatibleEmbeddingSpace(t *testing.T) {
	input := buildRFileInSpace(t, "a", embeddingspace.Has("space-a"))
	result, err := Compact(Spec{
		Inputs: []Input{{Name: "a.rf", Bytes: input}},
		Scope:  iterrt.ScopeMajc,
	})
	if err != nil {
		t.Fatal(err)
	}
	bc, err := bcfile.NewReader(bytes.NewReader(result.Output), int64(len(result.Output)))
	if err != nil {
		t.Fatal(err)
	}
	got, err := rfile.ReadEmbeddingSpaceMetadata(bc, block.Default())
	if err != nil {
		t.Fatal(err)
	}
	if got != embeddingspace.Has("space-a") {
		t.Fatalf("output embedding = %+v", got)
	}
}

func TestCompactRejectsMetadataFooterEmbeddingDisagreement(t *testing.T) {
	input := buildRFileInSpace(t, "a", embeddingspace.Has("space-a"))
	_, err := Compact(Spec{
		Inputs: []Input{{
			Name:              "a.rf",
			Bytes:             input,
			MetadataEmbedding: embeddingspace.Has("space-b"),
		}},
		Scope: iterrt.ScopeMajc,
	})
	if !errors.Is(err, embeddingspace.ErrIntegrity) {
		t.Fatalf("Compact error = %v, want ErrIntegrity", err)
	}
}

func buildRFileInSpace(t *testing.T, row string, state embeddingspace.FileState) []byte {
	t.Helper()
	var buf bytes.Buffer
	w, err := rfile.NewWriter(&buf, rfile.WriterOptions{EmbeddingSpace: state})
	if err != nil {
		t.Fatal(err)
	}
	if err := w.Append(&wire.Key{Row: []byte(row), ColumnFamily: []byte("cf"), Timestamp: 1}, []byte("v")); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}
