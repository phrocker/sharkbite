// Licensed to the Apache Software Foundation (ASF) under one or more
// contributor license agreements. See the NOTICE file distributed with this
// work for additional information regarding copyright ownership.

package router

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"sort"

	"github.com/phrocker/shoal-oss/pkg/lexicon"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

// MaxTargets bounds the visible targets one routing considers. A caller who
// can see more is refused (fail closed), never routed over a truncated set.
const MaxTargets = 256

// ActionSpec is the router's own copy of a registered action's data: its
// name, input schema and whether it requires approval. The router holds no
// registry type, so nothing it holds has a method that could act.
type ActionSpec struct {
	Name             string
	InputSchema      json.RawMessage
	RequiresApproval bool
}

// InputValidator validates and canonicalizes a filled input against a schema
// in the fleet descriptor subset. The host supplies it (internal/routerwire
// wraps fleet.ValidateActionInput), so an action's proposed input is exactly
// what an enqueue would store. It must be pure.
type InputValidator interface {
	ValidateInput(schema, input json.RawMessage) (json.RawMessage, error)
}

// Target is one target the caller can currently see. Callers build it only
// from authorized reads; the router never sees a target the caller cannot.
type Target struct {
	Ref TargetRef
	// Action is the registered action's data, for an action target.
	Action *ActionSpec
	// SlotSchema is the router-side input schema of a decision target, in
	// the same declarative subset fleet descriptors use.
	SlotSchema json.RawMessage
	// SubjectConcepts are the ontology concept IDs a lookup subject may have.
	SubjectConcepts []shoal.ID
	// Name gives the cue words of a target that has no grammar: the action
	// name, the profile name, or the relation key.
	Name string
}

// candidate is one grammar binding key: one decision, one lookup, or one
// (capability, action) pair with every visible descriptor that offers it.
type candidate struct {
	key       string
	kind      Kind
	executors []Target
	grammar   *Grammar
	vocab     map[string]bool
}

// Catalog is the caller's visible targets with their bound grammars.
type Catalog struct {
	candidates []candidate
	digest     string
	grammars   *GrammarSet
	validator  InputValidator
}

// ErrTooManyTargets is returned when the visible targets exceed MaxTargets.
var ErrTooManyTargets = shoal.NewError(shoal.ErrorUnavailable, "router visible targets exceed their bound")

// NewCatalog groups visible targets by binding key and binds each to its
// grammar from set, or to a default grammar of cue words from its name. A
// grammar whose key no visible target has is never bound, so it can never
// fire. The digest covers only what is bound here. validator checks every
// action and decision input.
func NewCatalog(targets []Target, set *GrammarSet, validator InputValidator) (*Catalog, error) {
	if validator == nil {
		return nil, invalid("router catalog requires an input validator")
	}
	if len(targets) > MaxTargets {
		return nil, ErrTooManyTargets
	}
	byKey := map[string]*candidate{}
	for _, t := range targets {
		if err := t.Ref.validate(); err != nil {
			return nil, err
		}
		switch t.Ref.Kind {
		case KindAction:
			if t.Action == nil || t.Action.Name != t.Ref.Action.Action {
				return nil, invalid("router action target requires its registered action")
			}
		case KindDecision:
			if len(t.SlotSchema) == 0 {
				return nil, invalid("router decision target requires a slot schema")
			}
		case KindLookup:
			if len(t.SubjectConcepts) == 0 {
				return nil, invalid("router lookup target requires subject concepts")
			}
		default:
			return nil, invalid("router target kind is unknown")
		}
		key := t.Ref.Key()
		c, ok := byKey[key]
		if !ok {
			c = &candidate{key: key, kind: t.Ref.Kind}
			byKey[key] = c
		} else if t.Ref.Kind != KindAction {
			return nil, invalid("router catalog has a decision or lookup target twice")
		}
		c.executors = append(c.executors, cloneTarget(t))
	}
	catalog := &Catalog{grammars: set, validator: validator}
	for _, c := range byKey {
		sort.Slice(c.executors, func(i, j int) bool {
			return c.executors[i].Ref.Action.AgentID < c.executors[j].Ref.Action.AgentID
		})
		if g := set.lookup(c.key); g != nil {
			c.grammar = g
		} else {
			c.grammar = defaultGrammar(c)
		}
		c.vocab = vocabulary(c)
		catalog.candidates = append(catalog.candidates, *c)
	}
	sort.Slice(catalog.candidates, func(i, j int) bool {
		return catalog.candidates[i].key < catalog.candidates[j].key
	})
	digest, err := catalogDigest(catalog.candidates)
	if err != nil {
		return nil, err
	}
	catalog.digest = digest
	return catalog, nil
}

// Digest identifies the visible targets and the grammars bound to them.
func (c *Catalog) Digest() string { return c.digest }

// Len is the number of candidates (binding keys).
func (c *Catalog) Len() int { return len(c.candidates) }

// Keys lists the candidates' binding keys, sorted.
func (c *Catalog) Keys() []string {
	keys := make([]string, len(c.candidates))
	for i, cand := range c.candidates {
		keys[i] = cand.key
	}
	return keys
}

// GrammarSetDigest is the digest of the host's grammar set.
func (c *Catalog) GrammarSetDigest() string { return c.grammars.Digest() }

func cloneTarget(t Target) Target {
	t.Ref = t.Ref.clone()
	if t.Action != nil {
		a := *t.Action
		a.InputSchema = append(json.RawMessage(nil), a.InputSchema...)
		t.Action = &a
	}
	t.SlotSchema = append(json.RawMessage(nil), t.SlotSchema...)
	t.SubjectConcepts = append([]shoal.ID(nil), t.SubjectConcepts...)
	sort.Slice(t.SubjectConcepts, func(i, j int) bool { return t.SubjectConcepts[i] < t.SubjectConcepts[j] })
	return t
}

// defaultGrammar gives a target with no grammar its name's tokens as cues,
// no patterns, and, for a lookup, the subject slot; slots are then filled
// from the lexicon alone.
func defaultGrammar(c *candidate) *Grammar {
	g := &Grammar{literals: map[string]bool{}, digest: "default"}
	seen := map[string]bool{}
	for _, t := range c.executors {
		names := []string{t.Name}
		if t.Ref.Action != nil {
			names = append(names, t.Ref.Action.Action)
		}
		for _, name := range names {
			for _, token := range lexicon.Tokenize(name) {
				if !seen[token.Text] {
					seen[token.Text] = true
					g.cues = append(g.cues, token.Text)
				}
			}
		}
	}
	sort.Strings(g.cues)
	switch c.kind {
	case KindLookup:
		g.target = GrammarTarget{Kind: KindLookup}
		g.slots = []slotSpec{{name: LookupSubjectSlot}}
	default:
		g.target = GrammarTarget{Kind: c.kind}
		g.input = json.RawMessage(`{}`)
	}
	return g
}

func vocabulary(c *candidate) map[string]bool {
	v := map[string]bool{}
	for _, cue := range c.grammar.cues {
		v[cue] = true
	}
	for literal := range c.grammar.literals {
		v[literal] = true
	}
	for _, s := range c.grammar.slots {
		for _, ev := range s.enum {
			for _, phrase := range ev.phrases {
				for _, token := range phrase {
					v[token] = true
				}
			}
		}
	}
	return v
}

func catalogDigest(candidates []candidate) (string, error) {
	type executor struct {
		Ref             TargetRef
		InputSchema     json.RawMessage `json:",omitempty"`
		SlotSchema      json.RawMessage `json:",omitempty"`
		SubjectConcepts []shoal.ID      `json:",omitempty"`
		Name            string
	}
	type entry struct {
		Key       string
		Grammar   string
		Executors []executor
	}
	entries := make([]entry, len(candidates))
	for i, c := range candidates {
		entries[i] = entry{Key: c.key, Grammar: c.grammar.digest}
		for _, t := range c.executors {
			e := executor{Ref: t.Ref, SlotSchema: t.SlotSchema, SubjectConcepts: t.SubjectConcepts, Name: t.Name}
			if t.Action != nil {
				e.InputSchema = t.Action.InputSchema
			}
			entries[i].Executors = append(entries[i].Executors, e)
		}
	}
	b, err := json.Marshal(struct {
		Version string
		Entries []entry
	}{Version, entries})
	if err != nil {
		return "", invalid("router catalog cannot be encoded")
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:]), nil
}
