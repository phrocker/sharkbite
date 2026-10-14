// Licensed to the Apache Software Foundation (ASF) under one or more
// contributor license agreements. See the NOTICE file distributed with this
// work for additional information regarding copyright ownership.

package router

// Baseline thresholds. They are part of BaselineVersion: changing one is a
// new baseline.
const (
	BaselineMinScore = 0.5
	BaselineMinGap   = 0.2
)

// Baseline is the lexical comparator the router is measured against. It uses
// no model: a candidate one of whose patterns covers the whole text is the
// target (two such candidates are ambiguous_target); otherwise the candidate
// whose vocabulary explains the largest share of the text's non-mention
// tokens is the target, if that share is at least BaselineMinScore and leads
// the next candidate by at least BaselineMinGap; otherwise it abstains. Slots
// are filled and validated exactly as for the router.
func (a *Analysis) Baseline() (Proposal, error) {
	receipt := Receipt{
		Router:           BaselineVersion,
		GrammarSetDigest: a.catalog.GrammarSetDigest(),
		CatalogDigest:    a.catalog.Digest(),
		Candidates:       len(a.catalog.candidates),
	}
	if reason, early := a.Early(); early {
		return abstain(reason, receipt), nil
	}
	full := -1
	for i, c := range a.cands {
		if c.features[FeaturePatternFull] == 1 {
			if full >= 0 {
				return abstain(ReasonAmbiguousTarget, receipt), nil
			}
			full = i
		}
	}
	if full >= 0 {
		return a.fill(full, receipt)
	}
	best, bestScore, second := -1, 0.0, 0.0
	for i, c := range a.cands {
		score := c.features[FeatureVocabRecall]
		switch {
		case best < 0 || score > bestScore:
			if best >= 0 {
				second = bestScore
			}
			best, bestScore = i, score
		case score > second:
			second = score
		}
	}
	if best < 0 || bestScore < BaselineMinScore {
		return abstain(ReasonNoTarget, receipt), nil
	}
	if bestScore-second < BaselineMinGap {
		return abstain(ReasonAmbiguousTarget, receipt), nil
	}
	return a.fill(best, receipt)
}
