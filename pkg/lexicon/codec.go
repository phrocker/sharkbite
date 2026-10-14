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
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"math"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/phrocker/shoal-oss/pkg/ontology"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

// Wire format, version 1. Every integer is fixed-width big-endian and every
// string is a uint32 byte length followed by the bytes.
//
//	magic            16 bytes "shoal-lexicon-v1"
//	normalization    uint32 NormalizationVersion
//	unicode tables   string
//	build flags      uint8 (bit 0: initialisms derived)
//	snapshot ID      string
//	snapshot frontier uint64
//	snapshot as-of   int64 Unix nanoseconds, UTC
//	scope kind       uint8 (1 server-filtered, 2 pinned)
//	scope digest     32 bytes, pinned only
//	tokens           uint32 count, strings strictly increasing
//	nodes            uint32 count, {string ID, string kind} strictly increasing by ID
//	terms            uint32 count, strictly increasing by token sequence:
//	                   uint8 token count, uint32 token IDs,
//	                   uint32 posting count, {uint32 node index, uint8 origin}
//	                   strictly increasing by node index
//	templates        uint32 count, strictly increasing by ID:
//	                   string ID, string relation key, uint8 direction,
//	                   uint32 count + strings subject concepts,
//	                   uint32 count + strings answer concepts, string phrase key
//
// Load accepts exactly the bytes encode produces for the decoded contents and
// nothing else, so a bundle's ID (SHA-256 of its bytes) is a function of its
// contents.
const magic = "shoal-lexicon-v1"

type nodeEntry struct {
	id   shoal.ID
	kind string
}

type posting struct {
	node   uint32
	origin Origin
}

type term struct {
	tokens   []uint32
	postings []posting
}

// Build flags recorded in the header. Unknown bits are refused.
const (
	flagDeriveInitialisms uint8 = 1 << 0
	knownFlags                  = flagDeriveInitialisms
)

type contents struct {
	flags     uint8
	snapshot  Snapshot
	scope     Scope
	tokens    []string
	nodes     []nodeEntry
	terms     []term
	templates []Template
}

type encoder struct {
	buf []byte
	err error
}

func (e *encoder) u8(v uint8)   { e.buf = append(e.buf, v) }
func (e *encoder) u32(v uint32) { e.buf = binary.BigEndian.AppendUint32(e.buf, v) }
func (e *encoder) u64(v uint64) { e.buf = binary.BigEndian.AppendUint64(e.buf, v) }

func (e *encoder) count(n int) {
	if n < 0 || uint64(n) > math.MaxUint32 {
		e.err = invalid("lexicon bundle section is too large to encode")
		return
	}
	e.u32(uint32(n))
}

func (e *encoder) str(s string) {
	e.count(len(s))
	e.buf = append(e.buf, s...)
}

func encode(c *contents) ([]byte, error) {
	e := &encoder{buf: make([]byte, 0, 1<<12)}
	e.buf = append(e.buf, magic...)
	e.u32(NormalizationVersion)
	e.str(unicodeTables)
	e.u8(c.flags)
	e.str(c.snapshot.ID)
	e.u64(c.snapshot.Frontier)
	e.u64(uint64(c.snapshot.AsOf.UTC().UnixNano()))
	e.u8(c.scope.scopeKind())
	if pinned, ok := c.scope.(ScopePinned); ok {
		e.buf = append(e.buf, pinned.digest[:]...)
	}
	e.count(len(c.tokens))
	for _, token := range c.tokens {
		e.str(token)
	}
	e.count(len(c.nodes))
	for _, node := range c.nodes {
		e.str(string(node.id))
		e.str(node.kind)
	}
	e.count(len(c.terms))
	for _, t := range c.terms {
		if len(t.tokens) > math.MaxUint8 {
			return nil, invalid("lexicon term is too long to encode")
		}
		e.u8(uint8(len(t.tokens)))
		for _, id := range t.tokens {
			e.u32(id)
		}
		e.count(len(t.postings))
		for _, p := range t.postings {
			e.u32(p.node)
			e.u8(uint8(p.origin))
		}
	}
	e.count(len(c.templates))
	for _, template := range c.templates {
		e.str(template.ID)
		e.str(template.RelationKey)
		e.u8(uint8(template.Direction))
		e.count(len(template.SubjectConcepts))
		for _, id := range template.SubjectConcepts {
			e.str(string(id))
		}
		e.count(len(template.AnswerConcepts))
		for _, id := range template.AnswerConcepts {
			e.str(string(id))
		}
		e.str(template.PhraseKey)
	}
	if e.err != nil {
		return nil, e.err
	}
	return e.buf, nil
}

type decoder struct {
	data []byte
	off  int
	bad  bool
}

func (d *decoder) take(n int) []byte {
	if d.bad || n < 0 || n > len(d.data)-d.off {
		d.bad = true
		return nil
	}
	out := d.data[d.off : d.off+n]
	d.off += n
	return out
}

func (d *decoder) u8() uint8 {
	b := d.take(1)
	if b == nil {
		return 0
	}
	return b[0]
}

func (d *decoder) u32() uint32 {
	b := d.take(4)
	if b == nil {
		return 0
	}
	return binary.BigEndian.Uint32(b)
}

func (d *decoder) u64() uint64 {
	b := d.take(8)
	if b == nil {
		return 0
	}
	return binary.BigEndian.Uint64(b)
}

// count reads a uint32 element count and rejects it unless the remaining
// input could hold that many elements of at least minSize bytes each, so a
// forged count cannot force a large allocation.
func (d *decoder) count(minSize int) int {
	n := int(d.u32())
	if d.bad || (minSize > 0 && n > (len(d.data)-d.off)/minSize) {
		d.bad = true
		return 0
	}
	return n
}

func (d *decoder) str() string {
	n := int(d.u32())
	return string(d.take(n))
}

func malformed() error {
	return invalid("lexicon bundle is malformed or not canonical")
}

// Load decodes canonical bundle bytes, verifies every canonical-form
// invariant, and rebuilds the matcher deterministically. Any byte sequence
// other than the exact canonical encoding of its contents is refused,
// including bytes produced under another normalization version or other
// Unicode tables.
func Load(data []byte) (*Bundle, error) {
	c, err := decode(data)
	if err != nil {
		return nil, err
	}
	reencoded, err := encode(c)
	if err != nil || !bytes.Equal(reencoded, data) {
		return nil, malformed()
	}
	return newBundle(append([]byte(nil), data...), c), nil
}

// LoadVerified is Load for bytes received under a known ID: it refuses bytes
// whose SHA-256 is not want before decoding anything.
func LoadVerified(data []byte, want BundleID) (*Bundle, error) {
	if BundleID(sha256.Sum256(data)) != want {
		return nil, invalid("lexicon bundle does not match its ID")
	}
	return Load(data)
}

func decode(data []byte) (*contents, error) {
	d := &decoder{data: data}
	if string(d.take(len(magic))) != magic {
		return nil, malformed()
	}
	if d.u32() != NormalizationVersion || d.str() != unicodeTables {
		if d.bad {
			return nil, malformed()
		}
		return nil, invalid(
			"lexicon bundle was built with another normalization version")
	}
	c := &contents{flags: d.u8()}
	if c.flags&^knownFlags != 0 {
		return nil, malformed()
	}
	c.snapshot.ID = d.str()
	c.snapshot.Frontier = d.u64()
	c.snapshot.AsOf = time.Unix(0, int64(d.u64())).UTC()
	switch d.u8() {
	case scopeKindServerFiltered:
		c.scope = ScopeServerFiltered{}
	case scopeKindPinned:
		var pinned ScopePinned
		copy(pinned.digest[:], d.take(len(pinned.digest)))
		c.scope = pinned
	default:
		return nil, malformed()
	}
	if d.bad || validateSnapshot(c.snapshot) != nil || validateScope(c.scope) != nil {
		return nil, malformed()
	}

	c.tokens = make([]string, d.count(5))
	for index := range c.tokens {
		token := d.str()
		if d.bad || !canonicalToken(token) ||
			(index > 0 && token <= c.tokens[index-1]) {
			return nil, malformed()
		}
		c.tokens[index] = token
	}

	c.nodes = make([]nodeEntry, d.count(9))
	for index := range c.nodes {
		id := shoal.ID(d.str())
		kind := d.str()
		if d.bad || id == "" ||
			shoal.ValidateRequiredID("lexicon node ID", id) != nil ||
			shoal.ValidateSemanticString("lexicon node kind", kind) != nil ||
			strings.HasPrefix(string(id), SentinelIDPrefix) ||
			(index > 0 && id <= c.nodes[index-1].id) {
			return nil, malformed()
		}
		c.nodes[index] = nodeEntry{id: id, kind: kind}
	}

	tokenUsed := make([]bool, len(c.tokens))
	nodeUsed := make([]bool, len(c.nodes))
	c.terms = make([]term, d.count(1+4+4+5))
	for index := range c.terms {
		length := int(d.u8())
		if length < 1 || length > HardMaxTermTokens {
			return nil, malformed()
		}
		t := term{tokens: make([]uint32, length)}
		for position := range t.tokens {
			id := d.u32()
			if d.bad || int(id) >= len(c.tokens) {
				return nil, malformed()
			}
			t.tokens[position] = id
			tokenUsed[id] = true
		}
		n := d.count(5)
		if n < 1 || n > HardMaxPostingsPerTerm {
			return nil, malformed()
		}
		t.postings = make([]posting, n)
		for position := range t.postings {
			p := posting{node: d.u32(), origin: Origin(d.u8())}
			if d.bad || int(p.node) >= len(c.nodes) || !p.origin.valid() ||
				(p.origin == OriginDerived && c.flags&flagDeriveInitialisms == 0) ||
				(position > 0 && p.node <= t.postings[position-1].node) {
				return nil, malformed()
			}
			t.postings[position] = p
			nodeUsed[p.node] = true
		}
		if index > 0 && compareTokenIDs(c.terms[index-1].tokens, t.tokens) >= 0 {
			return nil, malformed()
		}
		c.terms[index] = t
	}
	for _, used := range tokenUsed {
		if !used {
			return nil, malformed()
		}
	}
	for _, used := range nodeUsed {
		if !used {
			return nil, malformed()
		}
	}

	c.templates = make([]Template, d.count(4+4+1+4+4+4))
	if _, pinned := c.scope.(ScopePinned); pinned && len(c.templates) > 0 {
		// Build never puts templates in a pinned bundle.
		return nil, malformed()
	}
	for index := range c.templates {
		template := Template{ID: d.str(), RelationKey: d.str(), Direction: Direction(d.u8())}
		template.SubjectConcepts = decodeIDs(d)
		template.AnswerConcepts = decodeIDs(d)
		template.PhraseKey = d.str()
		if d.bad || !canonicalTemplate(template) ||
			(index > 0 && template.ID <= c.templates[index-1].ID) {
			return nil, malformed()
		}
		c.templates[index] = template
	}
	if d.bad || d.off != len(data) {
		return nil, malformed()
	}
	return c, nil
}

func decodeIDs(d *decoder) []shoal.ID {
	ids := make([]shoal.ID, d.count(4))
	for index := range ids {
		ids[index] = shoal.ID(d.str())
	}
	return ids
}

// canonicalToken reports whether token is exactly one normalized token.
func canonicalToken(token string) bool {
	if token == "" || len(token) > HardMaxTokenBytes || !utf8.ValidString(token) {
		return false
	}
	tokens := Tokenize(token)
	return len(tokens) == 1 && tokens[0].Text == token
}

func canonicalTemplate(template Template) bool {
	if !template.Direction.valid() || !wireString(template.RelationKey) ||
		template.ID != templateID(template.RelationKey, template.Direction) ||
		template.PhraseKey != phraseKey(template.RelationKey, template.Direction) {
		return false
	}
	for _, ids := range [][]shoal.ID{template.SubjectConcepts, template.AnswerConcepts} {
		if len(ids) == 0 {
			return false
		}
		for index, id := range ids {
			if shoal.ValidateRequiredID("lexicon template concept ID", id) != nil ||
				!utf8.ValidString(string(id)) ||
				ontology.IDNamespace(id) != "concept" ||
				(index > 0 && id <= ids[index-1]) {
				return false
			}
		}
	}
	if template.Direction == DirectionBoth &&
		!slices.Equal(template.SubjectConcepts, template.AnswerConcepts) {
		return false
	}
	return true
}

// wireString accepts exactly what ontology definition keys may be: non-empty,
// valid UTF-8, without surrounding white space.
func wireString(value string) bool {
	return value != "" && utf8.ValidString(value) && strings.TrimSpace(value) == value
}

// BundleID is the SHA-256 of a bundle's canonical bytes.
type BundleID [sha256.Size]byte

func (id BundleID) String() string {
	const hex = "0123456789abcdef"
	out := make([]byte, 0, len("lexicon:")+2*len(id))
	out = append(out, "lexicon:"...)
	for _, b := range id {
		out = append(out, hex[b>>4], hex[b&0x0f])
	}
	return string(out)
}
