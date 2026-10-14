/*
 * Licensed to the Apache Software Foundation (ASF) under one
 * or more contributor license agreements. See the NOTICE file
 * distributed with this work for additional information
 * regarding copyright ownership. The ASF licenses this file
 * to you under the Apache License, Version 2.0 (the
 * "License"); you may not use this file except in compliance
 * with the License. You may obtain a copy of the License at
 *
 *     https://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing,
 * software distributed under the License is distributed on an
 * "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY
 * KIND, either express or implied. See the License for the
 * specific language governing permissions and limitations
 * under the License.
 */

package explorer_test

import (
	"context"
	"strconv"
	"testing"

	"github.com/phrocker/shoal-oss/pkg/explorer"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

// benchReadCorpus builds a corpus with several sessions and one fold recorded
// over shared sources, so that each read path re-derives visibility for real
// work under contention.
func benchReadCorpus(b *testing.B, sessions int) (
	*explorer.Explorer, shoal.ID, shoal.ID,
) {
	b.Helper()
	corpus, err := explorer.Open(b.TempDir())
	if err != nil {
		b.Fatal(err)
	}
	open := ingestVisible(b, corpus, "file:///runbook.md", publicMarkdown, "ops")
	restricted := ingestVisible(
		b, corpus, "file:///incident.md", restrictedMarkdown, "secret&incident")
	for i := 0; i < sessions; i++ {
		id := shoal.ID("interaction.session_" + strconv.Itoa(i))
		recordedSession(
			b, corpus, id,
			[]shoal.ID{open[0], restricted[0]}, []shoal.ID{open[0]})
	}
	return corpus, open[0], restricted[0]
}

// BenchmarkInteractionsParallelReaders measures concurrent readers of the
// interaction listing, which re-derives every live record's visibility. It is
// the contention benchmark for issue #273's read-time resolution: the graph is
// warmed first, so the measured section is pure read work that should scale
// across goroutines rather than serialize.
func BenchmarkInteractionsParallelReaders(b *testing.B) {
	corpus, _, _ := benchReadCorpus(b, 24)
	defer corpus.Close()
	ctx := context.Background()
	if _, err := corpus.Interactions(ctx); err != nil {
		b.Fatal(err)
	}
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			if _, err := corpus.Interactions(ctx); err != nil {
				b.Fatal(err)
			}
		}
	})
}

// BenchmarkTraversalParallelReaders measures concurrent provenance traversal,
// which re-derives each candidate record's visibility before including it.
func BenchmarkTraversalParallelReaders(b *testing.B) {
	corpus, openID, _ := benchReadCorpus(b, 24)
	defer corpus.Close()
	ctx := context.Background()
	if _, err := corpus.InteractionsTouching(ctx, openID); err != nil {
		b.Fatal(err)
	}
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			if _, err := corpus.InteractionsTouching(ctx, openID); err != nil {
				b.Fatal(err)
			}
		}
	})
}
