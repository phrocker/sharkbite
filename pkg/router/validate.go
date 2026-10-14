// Licensed to the Apache Software Foundation (ASF) under one or more
// contributor license agreements. See the NOTICE file distributed with this
// work for additional information regarding copyright ownership.

package router

import (
	"bytes"
	"encoding/json"
	"sort"

	"github.com/phrocker/shoal-oss/pkg/lexicon"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

// fill fills and validates the selected candidate's slots, cheapest tier
// first: the slots the best pattern match placed over whole mentions or enum
// phrases, then, for slots still empty, the lexicon alone (the one unused
// mention whose kind fits, or the one enum value whose phrase occurs). It
// never guesses: a slot two mentions could fill, or one ambiguous mention
// covers, abstains as ambiguous_mention.
func (a *Analysis) fill(index int, receipt Receipt) (Proposal, error) {
	c := a.catalog.candidates[index]
	if len(c.executors) != 1 {
		return abstain(ReasonAmbiguousExecutor, receipt), nil
	}
	target := c.executors[0]
	ref := target.Ref.clone()
	g := c.grammar
	t := a.text
	values := make([]*Slot, len(g.slots))
	var ambiguous, mismatched []string
	settled := make([]bool, len(g.slots))
	usedMention := map[int]bool{}
	usedToken := make([]bool, len(t.tokens))
	mark := func(span lexicon.Span) {
		for p := span.Start; p < span.End && p < len(usedToken); p++ {
			usedToken[p] = true
		}
	}
	if best := a.cands[index].best; best != nil {
		for _, fl := range best.fills {
			s := g.slots[fl.slot]
			settled[fl.slot] = true
			mark(fl.span)
			if fl.mention < 0 {
				values[fl.slot] = &Slot{Name: s.name, Enum: s.enum[fl.enum].value, TokenSpan: fl.span}
				continue
			}
			usedMention[fl.mention] = true
			m := t.mentions[fl.mention]
			switch {
			case m.Ambiguous:
				ambiguous = append(ambiguous, s.name)
			case !s.kindFits(m):
				mismatched = append(mismatched, s.name)
			default:
				values[fl.slot] = &Slot{Name: s.name, NodeIDs: append([]shoal.ID(nil), m.NodeIDs...), TokenSpan: m.TokenSpan}
			}
		}
	}
	for si, s := range g.slots {
		if settled[si] {
			continue
		}
		if s.isEnum() {
			found, span := -1, lexicon.Span{}
			conflict := false
			for vi, v := range s.enum {
				for _, phrase := range v.phrases {
					for p := 0; p+len(phrase) <= len(t.tokens); p++ {
						if !phraseAt(t, phrase, p, usedToken) {
							continue
						}
						if found >= 0 && found != vi {
							conflict = true
						}
						if found < 0 {
							found, span = vi, lexicon.Span{Start: p, End: p + len(phrase)}
						}
					}
				}
			}
			switch {
			case conflict:
				ambiguous = append(ambiguous, s.name)
			case found >= 0:
				values[si] = &Slot{Name: s.name, Enum: s.enum[found].value, TokenSpan: span}
				mark(span)
			}
			continue
		}
		var fits []int
		for mi, m := range t.mentions {
			if !usedMention[mi] && s.kindFits(m) {
				fits = append(fits, mi)
			}
		}
		switch {
		case len(fits) > 1 || (len(fits) == 1 && t.mentions[fits[0]].Ambiguous):
			ambiguous = append(ambiguous, s.name)
		case len(fits) == 1:
			m := t.mentions[fits[0]]
			usedMention[fits[0]] = true
			values[si] = &Slot{Name: s.name, NodeIDs: append([]shoal.ID(nil), m.NodeIDs...), TokenSpan: m.TokenSpan}
			mark(m.TokenSpan)
		}
	}
	slots := make([]Slot, 0, len(values))
	for _, v := range values {
		if v != nil {
			slots = append(slots, *v)
		}
	}
	slotAbstain := func(reason Reason, missing []string) Proposal {
		return seal(Proposal{Kind: KindAbstain, Target: &ref, Reasons: []Reason{reason}, Missing: missing, Receipt: receipt})
	}
	if len(ambiguous) > 0 {
		return slotAbstain(ReasonAmbiguousMention, ambiguous), nil
	}
	if len(mismatched) > 0 {
		return slotAbstain(ReasonSlotMismatch, mismatched), nil
	}

	if c.kind == KindLookup {
		subject := values[0]
		if subject == nil {
			return slotAbstain(ReasonMissingSlot, []string{LookupSubjectSlot}), nil
		}
		concept, known := a.concepts[subject.NodeIDs[0]]
		allowed := false
		for _, want := range target.SubjectConcepts {
			allowed = allowed || (known && concept == want)
		}
		if !allowed {
			return slotAbstain(ReasonSlotMismatch, []string{LookupSubjectSlot}), nil
		}
		input, err := json.Marshal(struct {
			Subject shoal.ID `json:"subject"`
		}{subject.NodeIDs[0]})
		if err != nil {
			return Proposal{}, errInternal
		}
		return seal(Proposal{Kind: KindLookup, Target: &ref, Slots: slots, Input: input, Receipt: receipt}), nil
	}

	byName := map[string]*Slot{}
	for i, s := range g.slots {
		if values[i] != nil {
			byName[s.name] = values[i]
		}
	}
	rendered, err := renderTemplate(g.input, func(name string) (string, bool, error) {
		v, ok := byName[name]
		if !ok {
			return "", false, nil
		}
		if v.Enum != "" {
			return v.Enum, true, nil
		}
		return string(v.NodeIDs[0]), true, nil
	})
	if err != nil {
		return slotAbstain(ReasonInvalidInput, nil), nil
	}
	schema := target.SlotSchema
	if c.kind == KindAction {
		schema = target.Action.InputSchema
	}
	if missing := missingRequired(schema, rendered, g.input); len(missing) > 0 {
		return slotAbstain(ReasonMissingSlot, missing), nil
	}
	canonical, err := a.catalog.validator.ValidateInput(schema, rendered)
	if err != nil {
		return slotAbstain(ReasonInvalidInput, nil), nil
	}
	return seal(Proposal{Kind: c.kind, Target: &ref, Slots: slots, Input: canonical, Receipt: receipt}), nil
}

func phraseAt(t text, phrase []string, p int, used []bool) bool {
	for k, token := range phrase {
		if used[p+k] || t.inMention[p+k] || t.tokens[p+k] != token {
			return false
		}
	}
	return true
}

// missingRequired names, for each top-level field the schema requires that
// the rendered input lacks, the slot whose placeholder the template put
// there, or else the field itself. Only names from the registry or grammar
// are returned, never text.
func missingRequired(schema, rendered, template []byte) []string {
	var root struct {
		Required []string `json:"required"`
	}
	if json.Unmarshal(schema, &root) != nil || len(root.Required) == 0 {
		return nil
	}
	present := topLevel(rendered)
	placed := topLevel(template)
	seen := map[string]bool{}
	var missing []string
	for _, field := range root.Required {
		if _, ok := present[field]; ok {
			continue
		}
		name := field
		if raw, ok := placed[field]; ok {
			var value string
			if json.Unmarshal(raw, &value) == nil && len(value) > 1 && value[0] == '$' {
				name = value[1:]
			}
		}
		if !seen[name] {
			seen[name] = true
			missing = append(missing, name)
		}
	}
	sort.Strings(missing)
	return missing
}

// topLevel reads one object's members, last value winning as in
// canonicalization. A non-object yields none.
func topLevel(raw []byte) map[string]json.RawMessage {
	out := map[string]json.RawMessage{}
	d := json.NewDecoder(bytes.NewReader(raw))
	if open, err := d.Token(); err != nil || open != json.Delim('{') {
		return out
	}
	for d.More() {
		k, err := d.Token()
		if err != nil {
			return out
		}
		key, _ := k.(string)
		var value json.RawMessage
		if err := d.Decode(&value); err != nil {
			return out
		}
		out[key] = value
	}
	return out
}
