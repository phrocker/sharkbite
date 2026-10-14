// Licensed to the Apache Software Foundation (ASF) under one
// or more contributor license agreements. See the NOTICE file distributed
// with this work for additional information regarding copyright ownership.
// The ASF licenses this file to you under the Apache License, Version 2.0.
package metadata

import (
	"sort"

	"github.com/phrocker/shoal-oss/internal/embeddingspace"
)

type EmbeddingSpaceCount struct {
	State     embeddingspace.State
	Identity  string
	Files     int64
	SpanCount int64
}

// CountEmbeddingSpaces groups a tablet set's files by embedding-space
// state so an operator can watch a migration converge.
//
// unknown is reported as its own bucket rather than folded into
// no_embeddings. The two are not interchangeable here: no_embeddings is
// a positive assertion that a file holds no vectors, while unknown means
// nothing has been recorded about the file at all. Collapsing the second
// into the first would make a migration read as complete while files
// nobody has ever classified are still outstanding, which is exactly the
// signal this function exists to provide.
func CountEmbeddingSpaces(tablets []TabletInfo) []EmbeddingSpaceCount {
	counts := map[string]*EmbeddingSpaceCount{}
	for _, tablet := range tablets {
		for _, file := range tablet.Files {
			state := file.Embedding
			if state.State == "" {
				state = embeddingspace.Unknown()
			}
			key := string(state.State) + "\x00" + state.Identity
			count := counts[key]
			if count == nil {
				count = &EmbeddingSpaceCount{State: state.State, Identity: state.Identity}
				counts[key] = count
			}
			count.Files++
			if file.NumEntries > 0 {
				count.SpanCount += file.NumEntries
			}
		}
	}
	out := make([]EmbeddingSpaceCount, 0, len(counts))
	for _, count := range counts {
		out = append(out, *count)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].State != out[j].State {
			return out[i].State < out[j].State
		}
		return out[i].Identity < out[j].Identity
	})
	return out
}
