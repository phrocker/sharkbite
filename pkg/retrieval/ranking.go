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

package retrieval

import (
	"strings"
	"unicode"

	"github.com/phrocker/shoal-oss/pkg/shoal"
)

const (
	// UnicodeTermAnalyzerVersion identifies the exact Unicode letter/number,
	// lowercase, unique-term analysis behavior and the exact standard-library
	// Unicode tables used by unicode.IsLetter, IsNumber, and strings.ToLower.
	UnicodeTermAnalyzerVersion = "unicode-letter-number-lower-unique-v1+unicode-" +
		unicode.Version

	// CoverageFusionScorerVersion identifies the exact term coverage and
	// lexical/tree/graph/vector fusion formulas implemented below.
	CoverageFusionScorerVersion = "exact-coverage-lexical-tree-graph-vector-fusion-v2"
)

// TermSet is the unique analyzed terms for one value. Analyzer and Scorer
// implementations must not retain or mutate caller-owned sets.
type TermSet map[string]struct{}

// Analyzer converts text into terms for retrieval. Implementations must return
// the same terms for the same input and Version, independent of map iteration
// order, process, storage adapter, or transport.
type Analyzer interface {
	Version() string
	Analyze(string) TermSet
}

// ComponentScores contains the storage-neutral ranking inputs for one
// candidate.
type ComponentScores struct {
	Lexical shoal.Score
	Tree    shoal.Score
	Graph   shoal.Score
	Vector  shoal.Score
}

// Scorer computes exact coverage and mode fusion. Implementations must return
// bit-identical values for the same inputs and Version, independent of
// process, storage adapter, or transport. Coverage-derived component inputs
// must produce finite scores.
type Scorer interface {
	Version() string
	Coverage(query, candidate TermSet) shoal.Score
	ModeScore(Mode, ComponentScores) shoal.Score
	CombinedScore([]Mode, ComponentScores) shoal.Score
}

// UnicodeTermAnalyzer is the shared analyzer used by the embedded Explorer.
// It lowercases text, splits on non-letter/non-number runes, and retains each
// nonempty term once.
type UnicodeTermAnalyzer struct{}

// Version returns the stable analysis behavior identifier.
func (UnicodeTermAnalyzer) Version() string {
	return UnicodeTermAnalyzerVersion
}

// Analyze returns Unicode letter/number terms using the embedded Explorer's
// original analysis behavior.
func (UnicodeTermAnalyzer) Analyze(value string) TermSet {
	terms := make(TermSet)
	var current strings.Builder
	flush := func() {
		if current.Len() > 0 {
			terms[current.String()] = struct{}{}
			current.Reset()
		}
	}
	for _, r := range strings.ToLower(value) {
		if unicode.IsLetter(r) || unicode.IsNumber(r) {
			current.WriteRune(r)
		} else {
			flush()
		}
	}
	flush()
	return terms
}

// CoverageFusionScorer is the shared exact coverage and lexical/tree/graph
// fusion scorer used by the embedded Explorer.
type CoverageFusionScorer struct{}

// Version returns the stable scoring behavior identifier.
func (CoverageFusionScorer) Version() string {
	return CoverageFusionScorerVersion
}

// Coverage returns the fraction of unique query terms present in candidate.
func (CoverageFusionScorer) Coverage(query, candidate TermSet) shoal.Score {
	if len(query) == 0 {
		return 0
	}
	matched := 0
	for term := range query {
		if _, ok := candidate[term]; ok {
			matched++
		}
	}
	return shoal.Score(matched) / shoal.Score(len(query))
}

// ModeScore returns the original embedded Explorer contribution for one mode.
func (CoverageFusionScorer) ModeScore(
	mode Mode, scores ComponentScores,
) shoal.Score {
	switch mode {
	case ModeLexical:
		return scores.Lexical
	case ModeTree:
		return scores.Tree*0.35 + scores.Lexical*0.65
	case ModeGraph:
		return scores.Graph*0.25 + scores.Lexical*0.75
	case ModeVector:
		return scores.Vector
	default:
		return 0
	}
}

// CombinedScore averages mode contributions in caller-supplied order.
// An empty mode list has no contribution and returns zero.
func (s CoverageFusionScorer) CombinedScore(
	modes []Mode, scores ComponentScores,
) shoal.Score {
	if len(modes) == 0 {
		return 0
	}
	var score shoal.Score
	for _, mode := range modes {
		score += s.ModeScore(mode, scores)
	}
	return score / shoal.Score(len(modes))
}

var (
	_ Analyzer = UnicodeTermAnalyzer{}
	_ Scorer   = CoverageFusionScorer{}
)
