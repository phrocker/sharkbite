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

package retrieval_test

import (
	"math"
	"reflect"
	"testing"
	"unicode"

	"github.com/phrocker/shoal-oss/pkg/retrieval"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

func TestUnicodeTermAnalyzerPreservesEmbeddedTermBehavior(t *testing.T) {
	analyzer := retrieval.UnicodeTermAnalyzer{}
	got := analyzer.Analyze("ALPHA, alpha; Café １２3 snake_case βETA")
	want := retrieval.TermSet{
		"alpha": {}, "café": {}, "１２3": {}, "snake": {}, "case": {}, "βeta": {},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("terms = %#v, want %#v", got, want)
	}
	if empty := analyzer.Analyze(" \t—_"); len(empty) != 0 {
		t.Fatalf("separator-only terms = %#v", empty)
	}
	second := analyzer.Analyze("ALPHA, alpha; Café １２3 snake_case βETA")
	if !reflect.DeepEqual(second, got) {
		t.Fatalf("analysis is not deterministic: %#v then %#v", got, second)
	}
}

func TestCoverageFusionScorerPreservesExactScoreBits(t *testing.T) {
	analyzer := retrieval.UnicodeTermAnalyzer{}
	scorer := retrieval.CoverageFusionScorer{}
	query := analyzer.Analyze("alpha beta gamma")

	lexical := scorer.Coverage(query, analyzer.Analyze("ALPHA gamma gamma delta"))
	tree := scorer.Coverage(query, analyzer.Analyze("beta heading"))
	graph := scorer.Coverage(query, analyzer.Analyze("alpha beta gamma relationship"))
	scores := retrieval.ComponentScores{
		Lexical: lexical,
		Tree:    tree,
		Graph:   graph,
		Vector:  0.875,
	}
	assertScoreBits(t, "lexical coverage", lexical, 0x3fe5555555555555)
	assertScoreBits(
		t, "lexical mode", scorer.ModeScore(retrieval.ModeLexical, scores),
		0x3fe5555555555555,
	)
	assertScoreBits(
		t, "tree mode", scorer.ModeScore(retrieval.ModeTree, scores),
		0x3fe199999999999a,
	)
	assertScoreBits(
		t, "graph mode", scorer.ModeScore(retrieval.ModeGraph, scores),
		0x3fe8000000000000,
	)
	assertScoreBits(
		t, "vector mode", scorer.ModeScore(retrieval.ModeVector, scores),
		0x3fec000000000000,
	)
	assertScoreBits(
		t, "combined",
		scorer.CombinedScore([]retrieval.Mode{
			retrieval.ModeLexical, retrieval.ModeTree, retrieval.ModeGraph,
		}, scores),
		0x3fe4fa4fa4fa4fa5,
	)
}

func TestCoverageFusionScorerReturnsFiniteBoundaryValues(t *testing.T) {
	scorer := retrieval.CoverageFusionScorer{}
	for name, score := range map[string]shoal.Score{
		"empty coverage": scorer.Coverage(nil, nil),
		"no modes":       scorer.CombinedScore(nil, retrieval.ComponentScores{}),
		"unknown mode": scorer.ModeScore(
			retrieval.Mode("future"), retrieval.ComponentScores{Lexical: 1},
		),
		"maximum coverage": scorer.Coverage(
			retrieval.TermSet{"term": {}}, retrieval.TermSet{"term": {}},
		),
	} {
		if math.IsNaN(float64(score)) || math.IsInf(float64(score), 0) {
			t.Fatalf("%s = %v", name, score)
		}
	}
}

func TestRankingVersionIdentifiersAreStable(t *testing.T) {
	analyzer := retrieval.UnicodeTermAnalyzer{}
	scorer := retrieval.CoverageFusionScorer{}
	for index := 0; index < 3; index++ {
		gotAnalyzer := analyzer.Version()
		wantAnalyzer := "unicode-letter-number-lower-unique-v1+unicode-" +
			unicode.Version
		if gotAnalyzer != wantAnalyzer {
			t.Fatalf("analyzer version literal = %q", gotAnalyzer)
		}
		if gotAnalyzer != retrieval.UnicodeTermAnalyzerVersion {
			t.Fatalf("analyzer version constant = %q", gotAnalyzer)
		}
		gotScorer := scorer.Version()
		if gotScorer != "exact-coverage-lexical-tree-graph-vector-fusion-v2" {
			t.Fatalf("scorer version literal = %q", gotScorer)
		}
		if gotScorer != retrieval.CoverageFusionScorerVersion {
			t.Fatalf("scorer version constant = %q", gotScorer)
		}
	}
}

func assertScoreBits(t *testing.T, name string, score shoal.Score, want uint64) {
	t.Helper()
	if got := math.Float64bits(float64(score)); got != want {
		t.Fatalf("%s bits = %#x (%v), want %#x", name, got, score, want)
	}
}
