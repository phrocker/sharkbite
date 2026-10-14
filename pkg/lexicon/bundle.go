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
	"crypto/sha256"
	"fmt"
	"io"
)

// Bundle is an immutable, loaded lexicon. It is safe for concurrent use.
//
// A Bundle never exposes its bytes. Shipping requires a pinned scope; see
// ForShipping.
//
// Every formatting method prints only the ID and scope. The state is also
// reachable only through a function value, which fmt prints as an address
// under every verb, so even a Bundle or Shippable held in another struct's
// unexported field cannot print the names it holds.
type Bundle struct {
	get func() *bundleState
}

type bundleState struct {
	id            BundleID
	data          []byte
	contents      *contents
	tokenIndex    map[string]uint32
	maxTermTokens int
	maxPostings   int
	automaton     automaton
	// minted is set only by Build from ScopedNodes. It is not serialized: a
	// pinned digest read from bytes is an unauthenticated label.
	minted bool
}

func newBundle(data []byte, c *contents) *Bundle {
	b := &bundleState{
		id:         sha256.Sum256(data),
		data:       data,
		contents:   c,
		tokenIndex: make(map[string]uint32, len(c.tokens)),
	}
	for index, token := range c.tokens {
		b.tokenIndex[token] = uint32(index)
	}
	for _, t := range c.terms {
		b.maxTermTokens = max(b.maxTermTokens, len(t.tokens))
		b.maxPostings = max(b.maxPostings, len(t.postings))
	}
	b.automaton = buildAutomaton(len(c.tokens), c.terms)
	return &Bundle{get: func() *bundleState { return b }}
}

// ID is the SHA-256 of the bundle's canonical bytes.
func (bb *Bundle) ID() BundleID { return bb.get().id }

// Snapshot is the graph snapshot the bundle is pinned to. AsOf is in UTC.
func (bb *Bundle) Snapshot() Snapshot { return bb.get().contents.snapshot }

// Scope is ScopeServerFiltered or ScopePinned.
func (bb *Bundle) Scope() Scope { return bb.get().contents.scope }

// DerivesInitialisms reports whether the bundle was built with
// Input.DeriveInitialisms.
func (bb *Bundle) DerivesInitialisms() bool {
	b := bb.get()
	return b.contents.flags&flagDeriveInitialisms != 0
}

// TermCount is the number of distinct terms.
func (bb *Bundle) TermCount() int { return len(bb.get().contents.terms) }

// Templates returns a copy of the lookup templates, sorted by ID.
func (bb *Bundle) Templates() []Template {
	b := bb.get()
	out := make([]Template, len(b.contents.templates))
	for index, template := range b.contents.templates {
		out[index] = cloneTemplate(template)
	}
	return out
}

// CandidateBound is the most distinct node IDs Candidates can return for any
// text of at most maxTokens tokens: every span of at most the bundle's longest
// term matches at most one term, and every term has at most the bundle's
// largest posting count. It depends only on the bundle, never on the text.
func (bb *Bundle) CandidateBound(maxTokens int) int {
	b := bb.get()
	if maxTokens <= 0 {
		return 0
	}
	spans := 0
	for length := 1; length <= b.maxTermTokens && length <= maxTokens; length++ {
		spans += maxTokens - length + 1
	}
	return spans * b.maxPostings
}

// Shippable is a bundle whose bytes may leave the server. Only a bundle built
// for a pinned scope can become one, so an unscoped (server-filtered) bundle
// cannot be exported through this package.
type Shippable struct {
	bundle Bundle
}

// ForShipping returns the shippable view of a pinned bundle that Build made
// from ScopedNodes in this process, and false otherwise. A server-filtered
// bundle never ships. Neither does a loaded bundle, even one whose bytes say
// ScopePinned: the digest in bundle bytes is an unauthenticated label that
// anyone holding bytes could have written, so shipping requires building the
// bundle afresh from an authorized scope filter.
func (bb *Bundle) ForShipping() (Shippable, bool) {
	state := bb.get()
	if _, ok := state.contents.scope.(ScopePinned); !ok || !state.minted {
		return Shippable{}, false
	}
	return Shippable{bundle: *bb}, true
}

// state is nil for a zero Bundle.
func (bb Bundle) state() *bundleState {
	if bb.get == nil {
		return nil
	}
	return bb.get()
}

// Bytes returns a copy of the canonical bundle bytes.
func (s Shippable) Bytes() []byte {
	state := s.bundle.state()
	if state == nil {
		return nil
	}
	return append([]byte(nil), state.data...)
}

// ID is the shipped bundle's ID.
func (s Shippable) ID() BundleID {
	state := s.bundle.state()
	if state == nil {
		return BundleID{}
	}
	return state.id
}

// Scope is the pinned scope the shipped bundle was built for.
func (s Shippable) Scope() ScopePinned {
	state := s.bundle.state()
	if state == nil {
		return ScopePinned{}
	}
	return state.contents.scope.(ScopePinned)
}

// String shows only the bundle ID and scope. A server-filtered bundle holds
// names and IDs of nodes a reader may not see, so no formatting verb may
// print its contents.
func (bb Bundle) String() string {
	return describe("lexicon.Bundle", bb.state())
}

// GoString is String, for %#v.
func (bb Bundle) GoString() string { return bb.String() }

// Format writes String for every verb, including %v, %+v, %#v, %x and %q.
func (bb Bundle) Format(state fmt.State, _ rune) { _, _ = io.WriteString(state, bb.String()) }

// String shows only the shipped bundle's ID and scope.
func (s Shippable) String() string {
	return describe("lexicon.Shippable", s.bundle.state())
}

// GoString is String, for %#v.
func (s Shippable) GoString() string { return s.String() }

// Format writes String for every verb.
func (s Shippable) Format(state fmt.State, _ rune) { _, _ = io.WriteString(state, s.String()) }

func describe(name string, state *bundleState) string {
	if state == nil || state.contents == nil {
		return name + "(empty)"
	}
	return name + "{" + state.id.String() + " " + scopeString(state.contents.scope) + "}"
}

func scopeString(scope Scope) string {
	switch value := scope.(type) {
	case ScopeServerFiltered:
		return "server-filtered"
	case ScopePinned:
		return fmt.Sprintf("pinned:%x", value.digest)
	default:
		return "invalid"
	}
}
