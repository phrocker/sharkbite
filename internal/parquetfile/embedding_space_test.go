// Licensed to the Apache Software Foundation (ASF) under one
// or more contributor license agreements. See the NOTICE file distributed
// with this work for additional information regarding copyright ownership.
// The ASF licenses this file to you under the Apache License, Version 2.0.
package parquetfile

import (
	"bytes"
	"testing"

	"github.com/phrocker/shoal-oss/internal/embeddingspace"
	"github.com/phrocker/shoal-oss/internal/iterrt"
	"github.com/phrocker/shoal-oss/internal/rfile/wire"
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
			src := iterrt.NewSliceSource([]iterrt.Cell{{
				Key:   &wire.Key{Row: []byte("r"), ColumnFamily: []byte("cf"), Timestamp: 1},
				Value: []byte("v"),
			}})
			if err := src.Init(nil, nil, iterrt.IteratorEnvironment{}); err != nil {
				t.Fatal(err)
			}
			if err := src.Seek(iterrt.InfiniteRange(), nil, false); err != nil {
				t.Fatal(err)
			}
			data, _, err := EncodeWithOptions(src, EncodeOptions{EmbeddingSpace: tc.in})
			if err != nil {
				t.Fatal(err)
			}
			got, err := ReadEmbeddingSpaceMetadata(bytes.NewReader(data), int64(len(data)))
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
	src := iterrt.NewSliceSource([]iterrt.Cell{{
		Key:   &wire.Key{Row: []byte("r"), ColumnFamily: []byte("cf"), Timestamp: 1},
		Value: []byte("v"),
	}})
	if err := src.Init(nil, nil, iterrt.IteratorEnvironment{}); err != nil {
		t.Fatal(err)
	}
	if err := src.Seek(iterrt.InfiniteRange(), nil, false); err != nil {
		t.Fatal(err)
	}
	data, _, err := EncodeWithOptions(src, EncodeOptions{})
	if err != nil {
		t.Fatal(err)
	}
	got, err := ReadEmbeddingSpaceMetadata(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		t.Fatal(err)
	}
	if got != embeddingspace.Unknown() {
		t.Fatalf("missing metadata = %+v, want unknown", got)
	}
}
