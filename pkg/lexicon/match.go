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

package lexicon

import (
	"sort"

	"github.com/phrocker/shoal-oss/pkg/shoal"
)

// automaton is an Aho-Corasick automaton over token IDs. Trie children are
// stored in compressed sparse rows sorted by token ID; the root has a dense
// table. It is built deterministically from the canonical term list.
type automaton struct {
	rootNext   []int32
	childStart []int32
	childToken []uint32
	childState []int32
	fail       []int32
	out        []int32
	term       []int32
	depth      []int32
}

func buildAutomaton(tokenCount int, terms []term) automaton {
	type edge struct {
		parent int32
		token  uint32
		child  int32
	}
	a := automaton{
		rootNext: make([]int32, tokenCount),
		term:     []int32{-1},
		depth:    []int32{0},
	}
	for index := range a.rootNext {
		a.rootNext[index] = -1
	}
	// Terms are strictly sorted by token sequence, so each term shares a
	// prefix with its predecessor and every new child of a parent has a
	// larger token than that parent's earlier children.
	var edges []edge
	path := []int32{0}
	var previous []uint32
	for termIndex, t := range terms {
		common := 0
		for common < len(previous) && common < len(t.tokens) &&
			previous[common] == t.tokens[common] {
			common++
		}
		path = path[:common+1]
		for position := common; position < len(t.tokens); position++ {
			state := int32(len(a.term))
			a.term = append(a.term, -1)
			a.depth = append(a.depth, int32(position+1))
			parent := path[len(path)-1]
			edges = append(edges, edge{parent: parent, token: t.tokens[position], child: state})
			if parent == 0 {
				a.rootNext[t.tokens[position]] = state
			}
			path = append(path, state)
		}
		a.term[path[len(path)-1]] = int32(termIndex)
		previous = t.tokens
	}
	states := len(a.term)
	a.childStart = make([]int32, states+1)
	for _, e := range edges {
		a.childStart[e.parent+1]++
	}
	for index := 1; index <= states; index++ {
		a.childStart[index] += a.childStart[index-1]
	}
	a.childToken = make([]uint32, len(edges))
	a.childState = make([]int32, len(edges))
	fill := append([]int32(nil), a.childStart[:states]...)
	for _, e := range edges {
		slot := fill[e.parent]
		fill[e.parent]++
		a.childToken[slot] = e.token
		a.childState[slot] = e.child
	}

	a.fail = make([]int32, states)
	a.out = make([]int32, states)
	a.out[0] = -1
	queue := make([]int32, 0, states)
	for slot := a.childStart[0]; slot < a.childStart[1]; slot++ {
		child := a.childState[slot]
		a.fail[child] = 0
		a.out[child] = -1
		queue = append(queue, child)
	}
	for head := 0; head < len(queue); head++ {
		state := queue[head]
		for slot := a.childStart[state]; slot < a.childStart[state+1]; slot++ {
			token, child := a.childToken[slot], a.childState[slot]
			f := a.fail[state]
			for {
				if next := a.next(f, token); next >= 0 {
					a.fail[child] = next
					break
				}
				if f == 0 {
					a.fail[child] = 0
					break
				}
				f = a.fail[f]
			}
			target := a.fail[child]
			if a.term[target] >= 0 {
				a.out[child] = target
			} else {
				a.out[child] = a.out[target]
			}
			queue = append(queue, child)
		}
	}
	return a
}

func (a *automaton) next(state int32, token uint32) int32 {
	if state == 0 {
		if int(token) < len(a.rootNext) {
			return a.rootNext[token]
		}
		return -1
	}
	low, high := int(a.childStart[state]), int(a.childStart[state+1])
	for low < high {
		mid := int(uint(low+high) >> 1)
		if a.childToken[mid] < token {
			low = mid + 1
		} else {
			high = mid
		}
	}
	if low < int(a.childStart[state+1]) && a.childToken[low] == token {
		return a.childState[low]
	}
	return -1
}

// Span is a half-open [Start, End) range.
type Span struct {
	Start int
	End   int
}

// Candidate is one term occurrence: every node the term names, whatever the
// caller may see. NodeIDs is sorted and Kinds is parallel to it.
type Candidate struct {
	TokenSpan Span
	ByteSpan  Span
	NodeIDs   []shoal.ID
	Kinds     []string
}

// Candidates tokenizes text and returns every term occurrence, overlapping
// ones included, sorted by token start then end. It applies no visibility; a
// caller must filter it before using or echoing anything. Callers that bound
// text by token count should tokenize once and use CandidatesForTokens.
func (bb *Bundle) Candidates(text string) []Candidate {
	return bb.CandidatesForTokens(Tokenize(text))
}

// CandidatesForTokens is Candidates over already-tokenized text.
func (bb *Bundle) CandidatesForTokens(tokens []Token) []Candidate {
	b := bb.get()
	var out []Candidate
	a := &b.automaton
	state := int32(0)
	for position, token := range tokens {
		id, known := b.tokenIndex[token.Text]
		if !known {
			state = 0
			continue
		}
		for {
			if next := a.next(state, id); next >= 0 {
				state = next
				break
			}
			if state == 0 {
				break
			}
			state = a.fail[state]
		}
		match := state
		if a.term[match] < 0 {
			match = a.out[match]
		}
		for ; match >= 0; match = a.out[match] {
			t := &b.contents.terms[a.term[match]]
			start := position + 1 - int(a.depth[match])
			candidate := Candidate{
				TokenSpan: Span{Start: start, End: position + 1},
				ByteSpan:  Span{Start: tokens[start].Start, End: token.End},
				NodeIDs:   make([]shoal.ID, len(t.postings)),
				Kinds:     make([]string, len(t.postings)),
			}
			for index, p := range t.postings {
				node := b.contents.nodes[p.node]
				candidate.NodeIDs[index] = node.id
				candidate.Kinds[index] = node.kind
			}
			out = append(out, candidate)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].TokenSpan.Start != out[j].TokenSpan.Start {
			return out[i].TokenSpan.Start < out[j].TokenSpan.Start
		}
		return out[i].TokenSpan.End < out[j].TokenSpan.End
	})
	return out
}

// Mention is a selected span. When two or more visible nodes share the span it
// is Ambiguous and lists them all; Select never picks one.
type Mention struct {
	TokenSpan Span
	ByteSpan  Span
	NodeIDs   []shoal.ID
	Kinds     []string
	Ambiguous bool
}

// Select keeps only the visible node IDs of each candidate, drops candidates
// left with none, and only then chooses leftmost-longest non-overlapping
// spans. Filtering first means a hidden node can neither hide a visible
// shorter match behind a longer span nor make a visible match ambiguous.
func Select(candidates []Candidate, visible func(shoal.ID) bool) []Mention {
	filtered := make([]Mention, 0, len(candidates))
	for _, candidate := range candidates {
		mention := Mention{TokenSpan: candidate.TokenSpan, ByteSpan: candidate.ByteSpan}
		for index, id := range candidate.NodeIDs {
			if visible(id) {
				mention.NodeIDs = append(mention.NodeIDs, id)
				mention.Kinds = append(mention.Kinds, candidate.Kinds[index])
			}
		}
		if len(mention.NodeIDs) == 0 {
			continue
		}
		mention.Ambiguous = len(mention.NodeIDs) > 1
		filtered = append(filtered, mention)
	}
	sort.SliceStable(filtered, func(i, j int) bool {
		if filtered[i].TokenSpan.Start != filtered[j].TokenSpan.Start {
			return filtered[i].TokenSpan.Start < filtered[j].TokenSpan.Start
		}
		return filtered[i].TokenSpan.End > filtered[j].TokenSpan.End
	})
	var selected []Mention
	end := 0
	for _, mention := range filtered {
		if mention.TokenSpan.Start < end {
			continue
		}
		selected = append(selected, mention)
		end = mention.TokenSpan.End
	}
	return selected
}
