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

package lexicon_test

import (
	"fmt"
	"testing"

	"github.com/phrocker/shoal-oss/pkg/graph"
	"github.com/phrocker/shoal-oss/pkg/lexicon"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

// syntheticNodes gives terms/2 nodes, each with a unique one-token name and a
// unique two-token alias, so the bundle holds exactly terms terms.
func syntheticNodes(terms int) []graph.Node {
	const vocabulary = 1024
	nodes := make([]graph.Node, terms/2)
	for index := range nodes {
		nodes[index] = graph.Node{
			ID:   shoal.ID(fmt.Sprintf("node-%07d", index)),
			Kind: "service",
			Properties: shoal.Metadata{
				"name": fmt.Sprintf("svc%07d", index),
				"shoal.lexicon.alias.0": fmt.Sprintf("word%04d term%04d",
					index%vocabulary, index/vocabulary),
			},
		}
	}
	return nodes
}

// benchmarkText is a 24-token question mixing known names, known two-token
// aliases, overlapping prefixes and unknown words.
const benchmarkText = "does svc0000007 depend on word0003 term0000 or the " +
	"Word0010-term0001 billing gateway, and who owns svc0004095 and " +
	"word0005 term0002 word0006 today?"

func BenchmarkCandidates(b *testing.B) {
	for _, terms := range []int{10_000, 100_000, 1_000_000} {
		bundle, err := lexicon.Build(lexicon.Input{
			Snapshot: fixedSnapshot,
			Nodes:    syntheticNodes(terms),
		}, lexicon.Limits{})
		if err != nil {
			b.Fatal(err)
		}
		if bundle.TermCount() != terms {
			b.Fatalf("term count = %d, want %d", bundle.TermCount(), terms)
		}
		if len(bundle.Candidates(benchmarkText)) == 0 {
			b.Fatal("benchmark text matched nothing")
		}
		b.Run(fmt.Sprintf("terms=%d", terms), func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				_ = bundle.Candidates(benchmarkText)
			}
		})
	}
}
