// Licensed to the Apache Software Foundation (ASF) under one or more
// contributor license agreements. See the NOTICE file distributed with this
// work for additional information regarding copyright ownership.

package router

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/phrocker/shoal-oss/pkg/lexicon"
)

// GrammarSchema is the only grammar document version this router reads.
const GrammarSchema = "shoal.router.grammar/v1"

// Grammar bounds. A document outside them is refused, never truncated.
const (
	MaxGrammars          = 256
	MaxGrammarBytes      = 64 << 10
	MaxCues              = 32
	MaxPatterns          = 32
	MaxPatternElements   = 16
	MaxOptionalGroups    = 4
	MaxSlots             = 8
	MaxEnumValues        = 16
	MaxEnumPhrases       = 8
	MaxPhraseTokens      = 4
	MaxNodeKinds         = 8
	MaxGrammarNameBytes  = 128
	MaxInputTemplateSize = 4096
)

// GrammarTarget is what a grammar binds to. A grammar fires only for a target
// the caller can see; for an action, every visible descriptor that offers the
// named capability and action is an executor of it.
type GrammarTarget struct {
	Kind       Kind   `json:"kind"`
	Capability string `json:"capability,omitempty"`
	Action     string `json:"action,omitempty"`
	Profile    string `json:"profile,omitempty"`
	Template   string `json:"template,omitempty"`
}

// Key is the binding key, as TargetRef.Key.
func (t GrammarTarget) Key() string {
	switch t.Kind {
	case KindAction:
		return actionKey(t.Capability, t.Action)
	case KindDecision:
		return "decision:" + t.Profile
	case KindLookup:
		return "lookup:" + t.Template
	}
	return ""
}

type elementKind uint8

const (
	elemLiteral elementKind = iota + 1
	elemSlot
	elemOptional
)

type element struct {
	kind  elementKind
	token string
	slot  int
	group []element
}

type pattern struct {
	elems []element
}

type enumValue struct {
	value   string
	phrases [][]string
}

type slotSpec struct {
	name      string
	nodeKinds []string
	enum      []enumValue
}

func (s slotSpec) isEnum() bool { return len(s.enum) > 0 }

// Grammar is one compiled, immutable grammar.
type Grammar struct {
	target   GrammarTarget
	cues     []string
	literals map[string]bool
	patterns []pattern
	slots    []slotSpec
	input    json.RawMessage
	digest   string
}

// Target is what the grammar binds to.
func (g *Grammar) Target() GrammarTarget { return g.target }

// Digest is the SHA-256 of the grammar's canonical form.
func (g *Grammar) Digest() string { return g.digest }

// GrammarSet is a versioned set of grammars with at most one per target key.
type GrammarSet struct {
	byKey  map[string]*Grammar
	digest string
}

// Digest is the SHA-256 over the sorted (key, grammar digest) pairs.
func (s *GrammarSet) Digest() string {
	if s == nil {
		return emptySetDigest
	}
	return s.digest
}

// Len is the number of grammars.
func (s *GrammarSet) Len() int {
	if s == nil {
		return 0
	}
	return len(s.byKey)
}

func (s *GrammarSet) lookup(key string) *Grammar {
	if s == nil {
		return nil
	}
	return s.byKey[key]
}

var emptySetDigest = digestPairs(nil)

// ParseGrammarSet compiles grammar documents. It refuses the whole set if any
// document is malformed, out of bounds, or binds a key another already binds.
func ParseGrammarSet(documents [][]byte) (*GrammarSet, error) {
	if len(documents) > MaxGrammars {
		return nil, invalid("router grammar set exceeds its bound")
	}
	set := &GrammarSet{byKey: make(map[string]*Grammar, len(documents))}
	for index, document := range documents {
		g, err := ParseGrammar(document)
		if err != nil {
			return nil, fmt.Errorf("router grammar %d: %w", index, err)
		}
		key := g.target.Key()
		if _, duplicate := set.byKey[key]; duplicate {
			return nil, invalid("router grammar set binds a target twice")
		}
		set.byKey[key] = g
	}
	pairs := make([][2]string, 0, len(set.byKey))
	for key, g := range set.byKey {
		pairs = append(pairs, [2]string{key, g.digest})
	}
	set.digest = digestPairs(pairs)
	return set, nil
}

func digestPairs(pairs [][2]string) string {
	sort.Slice(pairs, func(i, j int) bool { return pairs[i][0] < pairs[j][0] })
	h := sha256.New()
	h.Write([]byte(GrammarSchema + "\x00set\x00"))
	for _, pair := range pairs {
		writeField(h, pair[0])
		writeField(h, pair[1])
	}
	return hex.EncodeToString(h.Sum(nil))
}

func writeField(w io.Writer, value string) {
	var length [8]byte
	n := uint64(len(value))
	for i := 7; i >= 0; i-- {
		length[i] = byte(n)
		n >>= 8
	}
	_, _ = w.Write(length[:])
	_, _ = io.WriteString(w, value)
}

// ParseGrammar compiles one shoal.router.grammar/v1 document. Decoding is
// strict: keys must be exactly the documented ones (case-sensitive), each
// appears once, unknown keys and trailing data are refused, strings must be
// valid UTF-8, and every list is bounded. The input template alone may repeat
// a key: it is passed to the target's own validator, which canonicalizes it
// exactly as an enqueue would.
func ParseGrammar(document []byte) (*Grammar, error) {
	if len(document) == 0 || len(document) > MaxGrammarBytes || !utf8.Valid(document) {
		return nil, invalid("router grammar is empty, too large or not UTF-8")
	}
	top, err := strictObject(document, []string{"schema", "target", "cues", "patterns", "slots"}, []string{"input"})
	if err != nil {
		return nil, err
	}
	var schema string
	if err := strictString(top["schema"], &schema); err != nil || schema != GrammarSchema {
		return nil, invalid("router grammar schema is not " + GrammarSchema)
	}
	g := &Grammar{literals: map[string]bool{}}
	if g.target, err = parseGrammarTarget(top["target"]); err != nil {
		return nil, err
	}
	var cues []string
	if err := strictStrings(top["cues"], MaxCues, &cues); err != nil {
		return nil, err
	}
	seenCue := map[string]bool{}
	for _, cue := range cues {
		token, err := singleToken(cue)
		if err != nil {
			return nil, err
		}
		if !seenCue[token] {
			seenCue[token] = true
			g.cues = append(g.cues, token)
		}
	}
	sort.Strings(g.cues)
	var rawSlots []json.RawMessage
	if err := strictArray(top["slots"], MaxSlots, &rawSlots); err != nil {
		return nil, err
	}
	slotIndex := map[string]int{}
	for _, raw := range rawSlots {
		s, err := parseSlot(raw)
		if err != nil {
			return nil, err
		}
		if _, duplicate := slotIndex[s.name]; duplicate {
			return nil, invalid("router grammar declares a slot twice")
		}
		slotIndex[s.name] = len(g.slots)
		g.slots = append(g.slots, s)
	}
	if err := checkEnumPhrases(g.slots); err != nil {
		return nil, err
	}
	var patterns []string
	if err := strictStrings(top["patterns"], MaxPatterns, &patterns); err != nil {
		return nil, err
	}
	for _, text := range patterns {
		p, err := parsePattern(text, slotIndex)
		if err != nil {
			return nil, err
		}
		collectLiterals(p.elems, g.literals)
		g.patterns = append(g.patterns, p)
	}
	rawInput, hasInput := top["input"]
	switch g.target.Kind {
	case KindLookup:
		if hasInput || len(g.slots) != 1 || g.slots[0].name != LookupSubjectSlot || g.slots[0].isEnum() {
			return nil, invalid("router lookup grammar has exactly one node slot named subject and no input")
		}
	default:
		if !hasInput {
			return nil, invalid("router grammar for an action or decision requires an input template")
		}
		if len(rawInput) > MaxInputTemplateSize {
			return nil, invalid("router input template exceeds its bound")
		}
		used := map[string]bool{}
		if _, err := renderTemplate(rawInput, func(name string) (string, bool, error) {
			if _, ok := slotIndex[name]; !ok {
				return "", false, invalid("router input template names an undeclared slot")
			}
			used[name] = true
			return "", true, nil
		}); err != nil {
			return nil, err
		}
		if len(used) != len(g.slots) {
			return nil, invalid("router input template must use every declared slot")
		}
		g.input = append(json.RawMessage(nil), rawInput...)
	}
	canonical, err := json.Marshal(struct {
		Schema   string
		Target   GrammarTarget
		Cues     []string
		Patterns []string
		Slots    []json.RawMessage
		Input    string
	}{GrammarSchema, g.target, g.cues, patterns, rawSlots, string(g.input)})
	if err != nil {
		return nil, errInternal
	}
	sum := sha256.Sum256(canonical)
	g.digest = hex.EncodeToString(sum[:])
	return g, nil
}

// LookupSubjectSlot is the one slot a lookup grammar declares.
const LookupSubjectSlot = "subject"

func parseGrammarTarget(raw json.RawMessage) (GrammarTarget, error) {
	fields, err := strictObject(raw, []string{"kind"}, []string{"capability", "action", "profile", "template"})
	if err != nil {
		return GrammarTarget{}, err
	}
	var t GrammarTarget
	var kind string
	if err := strictString(fields["kind"], &kind); err != nil {
		return GrammarTarget{}, err
	}
	t.Kind = Kind(kind)
	want := map[Kind][]string{
		KindAction:   {"capability", "action"},
		KindDecision: {"profile"},
		KindLookup:   {"template"},
	}[t.Kind]
	if want == nil || len(fields) != len(want)+1 {
		return GrammarTarget{}, invalid("router grammar target names an unknown kind or the wrong fields")
	}
	dst := map[string]*string{"capability": &t.Capability, "action": &t.Action, "profile": &t.Profile, "template": &t.Template}
	for _, key := range want {
		raw, ok := fields[key]
		if !ok {
			return GrammarTarget{}, invalid("router grammar target is missing " + key)
		}
		if err := strictString(raw, dst[key]); err != nil {
			return GrammarTarget{}, err
		}
		if err := boundedName(*dst[key]); err != nil {
			return GrammarTarget{}, err
		}
	}
	return t, nil
}

func parseSlot(raw json.RawMessage) (slotSpec, error) {
	fields, err := strictObject(raw, []string{"name"}, []string{"node_kinds", "enum"})
	if err != nil {
		return slotSpec{}, err
	}
	var s slotSpec
	if err := strictString(fields["name"], &s.name); err != nil {
		return slotSpec{}, err
	}
	if !plainName(s.name) {
		return slotSpec{}, invalid("router slot name must be [a-z0-9_]")
	}
	rawKinds, hasKinds := fields["node_kinds"]
	rawEnum, hasEnum := fields["enum"]
	if hasKinds && hasEnum {
		return slotSpec{}, invalid("router slot is a node slot or an enum slot, not both")
	}
	if hasKinds {
		if err := strictStrings(rawKinds, MaxNodeKinds, &s.nodeKinds); err != nil {
			return slotSpec{}, err
		}
		for _, kind := range s.nodeKinds {
			if err := boundedName(kind); err != nil {
				return slotSpec{}, err
			}
		}
		sort.Strings(s.nodeKinds)
	}
	if hasEnum {
		values, err := strictObject(rawEnum, nil, nil)
		if err != nil {
			return slotSpec{}, err
		}
		if len(values) == 0 || len(values) > MaxEnumValues {
			return slotSpec{}, invalid("router enum slot value count is outside its bound")
		}
		for value, rawPhrases := range values {
			if !plainName(value) {
				return slotSpec{}, invalid("router enum value must be [a-z0-9_]")
			}
			var phrases []string
			if err := strictStrings(rawPhrases, MaxEnumPhrases, &phrases); err != nil {
				return slotSpec{}, err
			}
			if len(phrases) == 0 {
				return slotSpec{}, invalid("router enum value needs a phrase")
			}
			ev := enumValue{value: value}
			for _, phrase := range phrases {
				tokens := lexicon.Tokenize(phrase)
				if len(tokens) == 0 || len(tokens) > MaxPhraseTokens {
					return slotSpec{}, invalid("router enum phrase token count is outside its bound")
				}
				texts := make([]string, len(tokens))
				for i, token := range tokens {
					texts[i] = token.Text
				}
				ev.phrases = append(ev.phrases, texts)
			}
			s.enum = append(s.enum, ev)
		}
		sort.Slice(s.enum, func(i, j int) bool { return s.enum[i].value < s.enum[j].value })
	}
	return s, nil
}

// checkEnumPhrases refuses a phrase that two enum values share: it could
// never be resolved to one.
func checkEnumPhrases(slots []slotSpec) error {
	for _, s := range slots {
		owner := map[string]string{}
		for _, v := range s.enum {
			for _, phrase := range v.phrases {
				key := strings.Join(phrase, "\x00")
				if other, ok := owner[key]; ok && other != v.value {
					return invalid("router enum phrase belongs to two values")
				}
				owner[key] = v.value
			}
		}
	}
	return nil
}

func parsePattern(text string, slots map[string]int) (pattern, error) {
	var top []element
	var group []element
	inGroup := false
	groups, count, literalsOutside := 0, 0, 0
	usedSlots := map[int]bool{}
	appendElem := func(e element) error {
		count++
		if count > MaxPatternElements {
			return invalid("router pattern exceeds its element bound")
		}
		if inGroup {
			group = append(group, e)
		} else {
			top = append(top, e)
			if e.kind == elemLiteral {
				literalsOutside++
			}
		}
		return nil
	}
	for i := 0; i < len(text); {
		c := text[i]
		switch {
		case c == ' ' || c == '\t':
			i++
		case c == '[':
			if inGroup {
				return pattern{}, invalid("router pattern optional groups do not nest")
			}
			inGroup, group = true, nil
			i++
		case c == ']':
			if !inGroup || len(group) == 0 {
				return pattern{}, invalid("router pattern has an empty or unopened optional group")
			}
			inGroup = false
			groups++
			if groups > MaxOptionalGroups {
				return pattern{}, invalid("router pattern exceeds its optional group bound")
			}
			top = append(top, element{kind: elemOptional, group: group})
			i++
		case c == '{':
			end := strings.IndexByte(text[i:], '}')
			if end < 0 {
				return pattern{}, invalid("router pattern has an unterminated slot")
			}
			name := text[i+1 : i+end]
			index, ok := slots[name]
			if !ok {
				return pattern{}, invalid("router pattern names an undeclared slot")
			}
			if usedSlots[index] {
				return pattern{}, invalid("router pattern names a slot twice")
			}
			usedSlots[index] = true
			if err := appendElem(element{kind: elemSlot, slot: index}); err != nil {
				return pattern{}, err
			}
			i += end + 1
		case c == '}':
			return pattern{}, invalid("router pattern has an unopened slot")
		default:
			j := i
			for j < len(text) && !strings.ContainsRune(" \t[]{}", rune(text[j])) {
				j++
			}
			token, err := singleToken(text[i:j])
			if err != nil {
				return pattern{}, err
			}
			if err := appendElem(element{kind: elemLiteral, token: token}); err != nil {
				return pattern{}, err
			}
			i = j
		}
	}
	if inGroup {
		return pattern{}, invalid("router pattern has an unterminated optional group")
	}
	if literalsOutside == 0 {
		return pattern{}, invalid("router pattern needs a literal outside optional groups")
	}
	return pattern{elems: top}, nil
}

func collectLiterals(elems []element, into map[string]bool) {
	for _, e := range elems {
		switch e.kind {
		case elemLiteral:
			into[e.token] = true
		case elemOptional:
			collectLiterals(e.group, into)
		}
	}
}

// singleToken requires a word that normalizes to exactly one token, and
// returns that token, so a grammar written with capitals or fullwidth forms
// matches the normalized text.
func singleToken(word string) (string, error) {
	tokens := lexicon.Tokenize(word)
	if len(tokens) != 1 || tokens[0].Start != 0 || tokens[0].End != len(word) {
		return "", invalid("router grammar word must be exactly one token")
	}
	return tokens[0].Text, nil
}

func plainName(s string) bool {
	if s == "" || len(s) > MaxGrammarNameBytes {
		return false
	}
	for _, r := range s {
		if !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '_') {
			return false
		}
	}
	return true
}

func boundedName(s string) error {
	if strings.TrimSpace(s) != s || s == "" || len(s) > MaxGrammarNameBytes || strings.ContainsRune(s, utf8.RuneError) {
		return invalid("router grammar name is empty, padded or too long")
	}
	return nil
}

// strictObject reads one JSON object level, refusing duplicate keys, trailing
// data, and, when required or optional is non-nil, any key outside them or a
// missing required key. Keys compare exactly, unlike encoding/json's
// case-insensitive field matching.
func strictObject(raw []byte, required, optional []string) (map[string]json.RawMessage, error) {
	d := json.NewDecoder(bytes.NewReader(raw))
	open, err := d.Token()
	if err != nil || open != json.Delim('{') {
		return nil, invalid("router grammar expects an object")
	}
	fields := map[string]json.RawMessage{}
	for d.More() {
		k, err := d.Token()
		if err != nil {
			return nil, invalid("router grammar object is malformed")
		}
		key, ok := k.(string)
		if !ok || !utf8.ValidString(key) || strings.ContainsRune(key, utf8.RuneError) {
			return nil, invalid("router grammar key is malformed")
		}
		if _, duplicate := fields[key]; duplicate {
			return nil, invalid("router grammar repeats a key")
		}
		var value json.RawMessage
		if err := d.Decode(&value); err != nil {
			return nil, invalid("router grammar value is malformed")
		}
		fields[key] = value
	}
	if end, err := d.Token(); err != nil || end != json.Delim('}') {
		return nil, invalid("router grammar object is malformed")
	}
	if _, err := d.Token(); !errors.Is(err, io.EOF) {
		return nil, invalid("router grammar has trailing data")
	}
	if required == nil && optional == nil {
		return fields, nil
	}
	allowed := map[string]bool{}
	for _, key := range append(append([]string(nil), required...), optional...) {
		allowed[key] = true
	}
	for key := range fields {
		if !allowed[key] {
			return nil, invalid("router grammar has an unknown key")
		}
	}
	for _, key := range required {
		if _, ok := fields[key]; !ok {
			return nil, invalid("router grammar is missing a required key")
		}
	}
	return fields, nil
}

func strictString(raw json.RawMessage, dst *string) error {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || trimmed[0] != '"' {
		return invalid("router grammar expects a string")
	}
	if err := json.Unmarshal(trimmed, dst); err != nil || strings.ContainsRune(*dst, utf8.RuneError) {
		return invalid("router grammar string is malformed")
	}
	return nil
}

func strictArray(raw json.RawMessage, limit int, dst *[]json.RawMessage) error {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || trimmed[0] != '[' {
		return invalid("router grammar expects an array")
	}
	if err := json.Unmarshal(trimmed, dst); err != nil {
		return invalid("router grammar array is malformed")
	}
	if len(*dst) > limit {
		return invalid("router grammar array exceeds its bound")
	}
	return nil
}

func strictStrings(raw json.RawMessage, limit int, dst *[]string) error {
	var items []json.RawMessage
	if err := strictArray(raw, limit, &items); err != nil {
		return err
	}
	out := make([]string, len(items))
	for i, item := range items {
		if err := strictString(item, &out[i]); err != nil {
			return err
		}
	}
	*dst = out
	return nil
}

// renderTemplate re-emits a JSON template token by token, replacing every
// string value that is exactly "$name" with the JSON string resolve returns.
// An object member whose placeholder resolve reports as unfilled is dropped;
// a placeholder anywhere else that is unfilled fails. Duplicate keys are kept
// as written: canonicalization belongs to the target's validator.
func renderTemplate(template []byte, resolve func(name string) (string, bool, error)) ([]byte, error) {
	d := json.NewDecoder(bytes.NewReader(template))
	d.UseNumber()
	var out bytes.Buffer
	if err := renderValue(d, &out, resolve, 0); err != nil {
		return nil, err
	}
	if _, err := d.Token(); !errors.Is(err, io.EOF) {
		return nil, invalid("router input template has trailing data")
	}
	return out.Bytes(), nil
}

func placeholder(token json.Token) (string, bool) {
	s, ok := token.(string)
	if !ok || len(s) < 2 || s[0] != '$' || !plainName(s[1:]) {
		return "", false
	}
	return s[1:], true
}

func renderValue(d *json.Decoder, out *bytes.Buffer, resolve func(string) (string, bool, error), depth int) error {
	if depth > 16 {
		return invalid("router input template is too deep")
	}
	token, err := d.Token()
	if err != nil {
		return invalid("router input template is malformed")
	}
	switch t := token.(type) {
	case json.Delim:
		switch t {
		case '{':
			out.WriteByte('{')
			first := true
			for d.More() {
				k, err := d.Token()
				if err != nil {
					return invalid("router input template is malformed")
				}
				key, _ := k.(string)
				// Peek whether the value is an unfilled placeholder by
				// rendering it into its own buffer.
				var value bytes.Buffer
				dropped := false
				if err := renderMember(d, &value, resolve, depth, &dropped); err != nil {
					return err
				}
				if dropped {
					continue
				}
				if !first {
					out.WriteByte(',')
				}
				first = false
				encoded, _ := json.Marshal(key)
				out.Write(encoded)
				out.WriteByte(':')
				out.Write(value.Bytes())
			}
			if _, err := d.Token(); err != nil {
				return invalid("router input template is malformed")
			}
			out.WriteByte('}')
		case '[':
			out.WriteByte('[')
			first := true
			for d.More() {
				if !first {
					out.WriteByte(',')
				}
				first = false
				if err := renderValue(d, out, resolve, depth+1); err != nil {
					return err
				}
			}
			if _, err := d.Token(); err != nil {
				return invalid("router input template is malformed")
			}
			out.WriteByte(']')
		default:
			return invalid("router input template is malformed")
		}
	default:
		if name, ok := placeholder(t); ok {
			value, filled, err := resolve(name)
			if err != nil {
				return err
			}
			if !filled {
				return invalid("router input template placeholder is unfilled outside an object member")
			}
			encoded, _ := json.Marshal(value)
			out.Write(encoded)
			return nil
		}
		encoded, err := json.Marshal(t)
		if err != nil {
			return invalid("router input template is malformed")
		}
		out.Write(encoded)
	}
	return nil
}

// renderMember renders an object member's value, reporting an unfilled
// placeholder as dropped instead of failing.
func renderMember(d *json.Decoder, out *bytes.Buffer, resolve func(string) (string, bool, error), depth int, dropped *bool) error {
	return renderValue(d, out, func(name string) (string, bool, error) {
		value, filled, err := resolve(name)
		if err != nil {
			return "", false, err
		}
		if !filled {
			*dropped = true
			return "", true, nil
		}
		return value, true, nil
	}, depth+1)
}
