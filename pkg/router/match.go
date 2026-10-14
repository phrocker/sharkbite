// Licensed to the Apache Software Foundation (ASF) under one or more
// contributor license agreements. See the NOTICE file distributed with this
// work for additional information regarding copyright ownership.

package router

import (
	"github.com/phrocker/shoal-oss/pkg/lexicon"
)

// fill is one slot a pattern match filled.
type fill struct {
	slot    int
	span    lexicon.Span
	mention int // index into the mentions, or -1 for an enum value
	enum    int // index into the slot's enum values, or -1
}

// match is one pattern occurrence over a contiguous token window.
type match struct {
	pattern int
	start   int
	end     int
	fills   []fill
}

// text is the tokenized input with its mentions indexed by start token.
type text struct {
	tokens    []string
	mentions  []lexicon.Mention
	mentionAt map[int]int
	inMention []bool
}

func newText(tokens []lexicon.Token, mentions []lexicon.Mention) text {
	t := text{
		tokens:    make([]string, len(tokens)),
		mentions:  mentions,
		mentionAt: map[int]int{},
		inMention: make([]bool, len(tokens)),
	}
	for i, token := range tokens {
		t.tokens[i] = token.Text
	}
	for i, m := range mentions {
		if m.TokenSpan.Start < 0 || m.TokenSpan.End > len(tokens) || m.TokenSpan.Start >= m.TokenSpan.End {
			continue
		}
		t.mentionAt[m.TokenSpan.Start] = i
		for p := m.TokenSpan.Start; p < m.TokenSpan.End; p++ {
			t.inMention[p] = true
		}
	}
	return t
}

// bestMatch returns the grammar's preferred match in t, or nil. Preference,
// in order: covering the whole text, covering more tokens, filling more
// slots, an earlier pattern, an earlier start. A slot placeholder matches
// only a whole mention that starts where it stands (node slots) or one of
// its enum phrases (enum slots); it never matches arbitrary words.
func (g *Grammar) bestMatch(t text) *match {
	var best *match
	n := len(t.tokens)
	better := func(m match) bool {
		if best == nil {
			return true
		}
		full := m.start == 0 && m.end == n
		bestFull := best.start == 0 && best.end == n
		if full != bestFull {
			return full
		}
		if m.end-m.start != best.end-best.start {
			return m.end-m.start > best.end-best.start
		}
		if len(m.fills) != len(best.fills) {
			return len(m.fills) > len(best.fills)
		}
		if m.pattern != best.pattern {
			return m.pattern < best.pattern
		}
		return m.start < best.start
	}
	for pi, p := range g.patterns {
		for start := 0; start < n; start++ {
			g.walk(t, p.elems, 0, start, nil, func(end int, fills []fill) {
				if end == start {
					return
				}
				m := match{pattern: pi, start: start, end: end, fills: append([]fill(nil), fills...)}
				if better(m) {
					copied := m
					best = &copied
				}
			})
		}
	}
	return best
}

func (g *Grammar) walk(t text, elems []element, index, pos int, fills []fill, done func(int, []fill)) {
	if index == len(elems) {
		done(pos, fills)
		return
	}
	e := elems[index]
	switch e.kind {
	case elemLiteral:
		if pos < len(t.tokens) && t.tokens[pos] == e.token {
			g.walk(t, elems, index+1, pos+1, fills, done)
		}
	case elemSlot:
		s := g.slots[e.slot]
		if s.isEnum() {
			for vi, v := range s.enum {
				for _, phrase := range v.phrases {
					if pos+len(phrase) > len(t.tokens) {
						continue
					}
					ok := true
					for k, token := range phrase {
						if t.tokens[pos+k] != token {
							ok = false
							break
						}
					}
					if ok {
						f := fill{slot: e.slot, span: lexicon.Span{Start: pos, End: pos + len(phrase)}, mention: -1, enum: vi}
						g.walk(t, elems, index+1, pos+len(phrase), append(fills, f), done)
					}
				}
			}
			return
		}
		if mi, ok := t.mentionAt[pos]; ok {
			m := t.mentions[mi]
			f := fill{slot: e.slot, span: m.TokenSpan, mention: mi, enum: -1}
			g.walk(t, elems, index+1, m.TokenSpan.End, append(fills, f), done)
		}
	case elemOptional:
		g.walk(t, e.group, 0, pos, fills, func(next int, withGroup []fill) {
			g.walk(t, elems, index+1, next, withGroup, done)
		})
		g.walk(t, elems, index+1, pos, fills, done)
	}
}

// kindFits reports whether any of a mention's node kinds is one the slot
// accepts. A slot with no kinds accepts any node.
func (s slotSpec) kindFits(m lexicon.Mention) bool {
	if len(s.nodeKinds) == 0 {
		return true
	}
	for _, kind := range m.Kinds {
		for _, want := range s.nodeKinds {
			if kind == want {
				return true
			}
		}
	}
	return false
}
