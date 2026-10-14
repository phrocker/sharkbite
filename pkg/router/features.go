// Licensed to the Apache Software Foundation (ASF) under one or more
// contributor license agreements. See the NOTICE file distributed with this
// work for additional information regarding copyright ownership.

package router

import (
	"bytes"
	"fmt"
	"math"
	"strconv"

	"github.com/phrocker/shoal-oss/pkg/lexicon"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

// FeatureSchemaID names the pair features: one vector per (text, visible
// candidate). Every feature is a scope-free number in [0, 1] computed from
// counts, so trained weights cannot memorize an entity, target or caller.
const FeatureSchemaID = "router.pair/v1"

// Feature indices of router.pair/v1.
const (
	// FeatureCueAny is 1 when a cue word of the candidate is in the text.
	FeatureCueAny = iota
	// FeatureCueDensity is the share of non-mention tokens that are cues.
	FeatureCueDensity
	// FeaturePatternFull is 1 when a pattern covers the whole text.
	FeaturePatternFull
	// FeaturePatternCover is the share of tokens the best pattern covers.
	FeaturePatternCover
	// FeatureMatchSlotFit is the share of the best match's slots whose
	// mention kind fits; 0 without a match, 1 for a match with no slots.
	FeatureMatchSlotFit
	// FeatureKindCompat is the share of the candidate's node slots that some
	// mention's kind fits; for a candidate with none, 1 if the text has no
	// mentions and 0 otherwise.
	FeatureKindCompat
	// FeatureVocabRecall is the share of non-mention tokens in the
	// candidate's vocabulary (cues, pattern words, enum phrases).
	FeatureVocabRecall
	// FeatureUnexplained is the share of all tokens that are neither in a
	// mention, in the candidate's vocabulary, nor a function word.
	FeatureUnexplained
	// FeatureFormAgree is 1 when a question-form text meets a lookup or
	// decision candidate, or a non-question meets an action candidate.
	FeatureFormAgree
	// NumFeatures is the vector width.
	NumFeatures
)

// FeatureNames lists the features in index order.
var FeatureNames = [NumFeatures]string{
	"cue_any", "cue_density", "pattern_full", "pattern_cover",
	"match_slot_fit", "kind_compat", "vocab_recall", "unexplained", "form_agree",
}

// functionWords are fixed English words that explain nothing about a target.
var functionWords = map[string]bool{
	"a": true, "an": true, "the": true, "to": true, "of": true, "in": true,
	"on": true, "for": true, "at": true, "by": true, "from": true, "with": true,
	"and": true, "or": true, "please": true, "can": true, "could": true,
	"would": true, "you": true, "me": true, "i": true, "we": true, "us": true,
	"my": true, "our": true, "it": true, "this": true, "that": true,
	"now": true, "right": true, "away": true, "just": true, "kindly": true,
	"be": true, "is": true, "are": true, "was": true, "do": true, "does": true,
	"what": true, "which": true, "who": true, "how": true, "will": true,
	"go": true, "ahead": true, "let": true, "s": true, "into": true,
	"tell": true, "show": true, "list": true, "give": true,
}

// questionWords open a question-form text.
var questionWords = map[string]bool{
	"what": true, "which": true, "who": true, "whom": true, "whose": true,
	"where": true, "how": true, "is": true, "are": true, "does": true,
	"do": true, "should": true, "would": true, "list": true, "show": true,
	"tell": true, "give": true,
}

func quantize(x float64) float64 {
	if x <= 0 {
		return 0
	}
	if x >= 1 {
		return 1
	}
	return math.Round(x*1e6) / 1e6
}

func ratio(n, d int) float64 {
	if d <= 0 {
		return 0
	}
	return quantize(float64(n) / float64(d))
}

func isQuestion(t text) bool {
	if len(t.tokens) == 0 {
		return false
	}
	return questionWords[t.tokens[0]]
}

func (c candidate) features(t text, best *match) [NumFeatures]float64 {
	var f [NumFeatures]float64
	g := c.grammar
	cues := map[string]bool{}
	for _, cue := range g.cues {
		cues[cue] = true
	}
	nonMention, cueTokens, inVocab, unexplained := 0, 0, 0, 0
	for i, token := range t.tokens {
		if t.inMention[i] {
			continue
		}
		nonMention++
		if cues[token] {
			cueTokens++
		}
		if c.vocab[token] {
			inVocab++
		} else if !functionWords[token] {
			unexplained++
		}
	}
	if cueTokens > 0 {
		f[FeatureCueAny] = 1
	}
	f[FeatureCueDensity] = ratio(cueTokens, nonMention)
	if best != nil {
		if best.start == 0 && best.end == len(t.tokens) {
			f[FeaturePatternFull] = 1
		}
		f[FeaturePatternCover] = ratio(best.end-best.start, len(t.tokens))
		fit := 0
		for _, fl := range best.fills {
			s := g.slots[fl.slot]
			if fl.mention < 0 || s.kindFits(t.mentions[fl.mention]) {
				fit++
			}
		}
		if len(best.fills) == 0 {
			f[FeatureMatchSlotFit] = 1
		} else {
			f[FeatureMatchSlotFit] = ratio(fit, len(best.fills))
		}
	}
	nodeSlots, compatible := 0, 0
	for _, s := range g.slots {
		if s.isEnum() {
			continue
		}
		nodeSlots++
		for _, m := range t.mentions {
			if s.kindFits(m) {
				compatible++
				break
			}
		}
	}
	if nodeSlots == 0 {
		if len(t.mentions) == 0 {
			f[FeatureKindCompat] = 1
		}
	} else {
		f[FeatureKindCompat] = ratio(compatible, nodeSlots)
	}
	f[FeatureVocabRecall] = ratio(inVocab, nonMention)
	f[FeatureUnexplained] = ratio(unexplained, len(t.tokens))
	question := isQuestion(t)
	if (question && c.kind != KindAction) || (!question && c.kind == KindAction) {
		f[FeatureFormAgree] = 1
	}
	return f
}

// SubjectID is the opaque decision subject of the candidate at index i. It
// names a position in the visible catalog, never a target.
func SubjectID(i int) shoal.ID { return shoal.ID(fmt.Sprintf("router.candidate:%04d", i)) }

// FeatureArtifact is the numeric evidence of one target-choice decision: the
// decisionlinear input document, and the byte range of each subject's entry
// within it. It contains no text and no target identity.
type FeatureArtifact struct {
	Bytes    []byte
	Subjects []shoal.ID
	Ranges   []lexicon.Span
}

// FeatureArtifact encodes the analysis's features as the strict numeric JSON
// internal/decisionlinear reads. Encoding is deterministic.
func (a *Analysis) FeatureArtifact() FeatureArtifact {
	var b bytes.Buffer
	b.WriteString(`{"schema":1,"feature_schema_id":"` + FeatureSchemaID + `","subjects":[`)
	out := FeatureArtifact{}
	for i, c := range a.cands {
		if i > 0 {
			b.WriteByte(',')
		}
		start := b.Len()
		id := SubjectID(i)
		b.WriteString(`{"id":"` + string(id) + `","features":[`)
		for k, x := range c.features {
			if k > 0 {
				b.WriteByte(',')
			}
			b.WriteString(strconv.FormatFloat(x, 'g', -1, 64))
		}
		b.WriteString(`]}`)
		out.Subjects = append(out.Subjects, id)
		out.Ranges = append(out.Ranges, lexicon.Span{Start: start, End: b.Len()})
	}
	b.WriteString(`]}`)
	out.Bytes = b.Bytes()
	return out
}

// Features returns each candidate's feature vector, in catalog order.
func (a *Analysis) Features() [][NumFeatures]float64 {
	out := make([][NumFeatures]float64, len(a.cands))
	for i, c := range a.cands {
		out[i] = c.features
	}
	return out
}
