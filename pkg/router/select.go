// Licensed to the Apache Software Foundation (ASF) under one or more
// contributor license agreements. See the NOTICE file distributed with this
// work for additional information regarding copyright ownership.

package router

import (
	"github.com/phrocker/shoal-oss/pkg/lexicon"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

// MaxTokens bounds the tokens of one routed text, matching the authorized
// mention resolver's bound.
const MaxTokens = 32

// MaxTextBytes bounds the bytes of one routed text, matching the authorized
// mention resolver's bound.
const MaxTextBytes = 4096

// Input is everything one routing reads. Tokens and Mentions come from the
// caller's text through the authorized mention resolver; Mentions name only
// nodes the caller may see. NodeConcepts maps visible mention nodes to their
// ontology concept ID, for lookup subjects.
type Input struct {
	Tokens       []lexicon.Token
	Mentions     []lexicon.Mention
	Catalog      *Catalog
	NodeConcepts map[shoal.ID]shoal.ID
	// OutOfBounds reports that the text exceeded the resolver's bound; the
	// routing then abstains without looking at anything else.
	OutOfBounds bool
}

type candAnalysis struct {
	best     *match
	features [NumFeatures]float64
}

// Analysis is one text matched against one catalog: every candidate's best
// pattern match and pair features. It is the evidence for the target-choice
// decision and for the baseline.
type Analysis struct {
	catalog  *Catalog
	text     text
	concepts map[shoal.ID]shoal.ID
	early    Reason
	cands    []candAnalysis
}

// Analyze matches the text against every visible candidate. It fails only on
// a malformed input (no catalog, or a mention outside the tokens).
func Analyze(in Input) (*Analysis, error) {
	if in.Catalog == nil {
		return nil, invalid("router analysis requires a catalog")
	}
	a := &Analysis{catalog: in.Catalog, concepts: in.NodeConcepts}
	switch {
	case in.OutOfBounds || len(in.Tokens) > MaxTokens:
		a.early = ReasonTextOutOfBounds
		return a, nil
	case len(in.Tokens) == 0:
		a.early = ReasonEmptyText
		return a, nil
	}
	end := 0
	for _, m := range in.Mentions {
		if m.TokenSpan.Start < end || m.TokenSpan.End <= m.TokenSpan.Start || m.TokenSpan.End > len(in.Tokens) ||
			len(m.NodeIDs) == 0 || len(m.NodeIDs) != len(m.Kinds) || m.Ambiguous != (len(m.NodeIDs) > 1) {
			return nil, invalid("router mentions must be ordered, disjoint and within the tokens")
		}
		end = m.TokenSpan.End
	}
	a.text = newText(in.Tokens, in.Mentions)
	a.cands = make([]candAnalysis, len(in.Catalog.candidates))
	for i, c := range in.Catalog.candidates {
		best := c.grammar.bestMatch(a.text)
		a.cands[i] = candAnalysis{best: best, features: c.features(a.text, best)}
	}
	return a, nil
}

// Early reports an abstention decided before any target choice (empty or
// out-of-bounds text). No decision is requested for it.
func (a *Analysis) Early() (Reason, bool) { return a.early, a.early != "" }

// Candidates is the number of visible candidates, the decision's subjects.
func (a *Analysis) Candidates() int { return len(a.cands) }

// TokenCount is the number of tokens analyzed.
func (a *Analysis) TokenCount() int { return len(a.text.tokens) }

// Receipt is the receipt fields the analysis alone determines.
func (a *Analysis) Receipt() Receipt {
	return Receipt{
		Router:           Version,
		GrammarSetDigest: a.catalog.GrammarSetDigest(),
		CatalogDigest:    a.catalog.Digest(),
		Candidates:       len(a.catalog.candidates),
	}
}

// Propose aggregates the target-choice answers into a proposal: exactly one
// candidate labelled match is the target; none is no_target; two or more is
// ambiguous_target. matched is in catalog order, one per candidate. The
// selected target's slots are then filled and validated (see fill).
func (a *Analysis) Propose(matched []bool, receipt Receipt) (Proposal, error) {
	if reason, early := a.Early(); early {
		return abstain(reason, receipt), nil
	}
	if len(matched) != len(a.cands) {
		return Proposal{}, invalid("router answers do not cover the candidates")
	}
	selected := -1
	for i, m := range matched {
		if !m {
			continue
		}
		if selected >= 0 {
			return abstain(ReasonAmbiguousTarget, receipt), nil
		}
		selected = i
	}
	if selected < 0 {
		return abstain(ReasonNoTarget, receipt), nil
	}
	return a.fill(selected, receipt)
}
