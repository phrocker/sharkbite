// Licensed to the Apache Software Foundation (ASF) under one or more
// contributor license agreements. See the NOTICE file distributed with this
// work for additional information regarding copyright ownership.

// Package eval measures the router on curated fixtures: a synthetic graph,
// fleet registry, decision profiles and grammars, and labelled cases split
// into train, dev and test. It reports exact counts (N of D), never rates
// alone. The pre-registered protocol is docs/router-evaluation.md.
package eval

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io/fs"
	"path"
	"sort"
	"strconv"
	"time"

	"github.com/phrocker/shoal-oss/pkg/graph"
	"github.com/phrocker/shoal-oss/pkg/lexicon"
	"github.com/phrocker/shoal-oss/pkg/ontology"
	"github.com/phrocker/shoal-oss/pkg/router"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

// ConceptProperty is the node property holding a node's ontology concept ID.
const ConceptProperty = "shoal.ontology.concept_id"

type worldNode struct {
	Key     string   `json:"key"`
	Kind    string   `json:"kind"`
	Name    string   `json:"name"`
	Aliases []string `json:"aliases"`
	Source  string   `json:"source"`
}

type worldAction struct {
	Name             string          `json:"name"`
	RequiresApproval bool            `json:"requires_approval"`
	InputSchema      json.RawMessage `json:"input_schema"`
}

type worldCapability struct {
	Name    string        `json:"name"`
	Actions []worldAction `json:"actions"`
}

type worldDescriptor struct {
	ID           string            `json:"id"`
	Source       string            `json:"source"`
	Capabilities []worldCapability `json:"capabilities"`
}

type worldProfile struct {
	ID         string          `json:"id"`
	Revision   string          `json:"revision"`
	Name       string          `json:"name"`
	Source     string          `json:"source"`
	Comment    string          `json:"comment"`
	SlotSchema json.RawMessage `json:"slot_schema"`
}

type worldRelationship struct {
	Key      string   `json:"key"`
	From     []string `json:"from"`
	To       []string `json:"to"`
	Directed bool     `json:"directed"`
}

type worldFile struct {
	Comment        string              `json:"comment"`
	Callers        map[string][]string `json:"callers"`
	Concepts       []string            `json:"concepts"`
	Relationships  []worldRelationship `json:"relationships"`
	OntologySource string              `json:"ontology_source"`
	Nodes          []worldNode         `json:"nodes"`
	Descriptors    []worldDescriptor   `json:"descriptors"`
	Profiles       []worldProfile      `json:"profiles"`
}

// World is the loaded fixture world.
type World struct {
	file      worldFile
	Bundle    *lexicon.Bundle
	Grammars  *router.GrammarSet
	concepts  map[shoal.ID]shoal.ID
	nodeKey   map[shoal.ID]string
	nodeSrc   map[shoal.ID]string
	templates []lexicon.Template
}

// NodeID is the fixture node ID for a node key.
func NodeID(key string) shoal.ID { return shoal.ID("node:" + key) }

// LoadWorld reads world.json and grammars/*.json from fsys (os.DirFS or an
// embed.FS); the package itself opens no file.
func LoadWorld(fsys fs.FS) (*World, error) {
	raw, err := fs.ReadFile(fsys, "world.json")
	if err != nil {
		return nil, err
	}
	w := &World{concepts: map[shoal.ID]shoal.ID{}, nodeKey: map[shoal.ID]string{}, nodeSrc: map[shoal.ID]string{}}
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if err := d.Decode(&w.file); err != nil {
		return nil, fmt.Errorf("world.json: %w", err)
	}
	conceptIDs := map[string]shoal.ID{}
	for _, key := range w.file.Concepts {
		c, err := ontology.NewConceptDefinition(key, key, "", nil, nil)
		if err != nil {
			return nil, err
		}
		conceptIDs[key] = c.ID()
	}
	var relationships []ontology.RelationshipDefinition
	for _, r := range w.file.Relationships {
		ids := func(keys []string) []shoal.ID {
			out := make([]shoal.ID, len(keys))
			for i, k := range keys {
				out[i] = conceptIDs[k]
			}
			return out
		}
		def, err := ontology.NewRelationshipDefinition(r.Key, r.Key, "", ids(r.From), ids(r.To), nil, r.Directed, nil)
		if err != nil {
			return nil, err
		}
		relationships = append(relationships, def)
	}
	var nodes []graph.Node
	for _, n := range w.file.Nodes {
		id := NodeID(n.Key)
		props := shoal.Metadata{"name": n.Name, ConceptProperty: string(conceptIDs[n.Kind])}
		for i, alias := range n.Aliases {
			props[lexicon.PropertyAliasPrefix+strconv.Itoa(i)] = alias
		}
		nodes = append(nodes, graph.Node{ID: id, Kind: n.Kind, Properties: props})
		w.concepts[id] = conceptIDs[n.Kind]
		w.nodeKey[id] = n.Key
		w.nodeSrc[id] = n.Source
	}
	w.Bundle, err = lexicon.Build(lexicon.Input{
		Snapshot:      lexicon.Snapshot{ID: "router-eval-v1", AsOf: time.Date(2026, 10, 8, 0, 0, 0, 0, time.UTC), Frontier: 1},
		Nodes:         nodes,
		Relationships: relationships,
	}, lexicon.Limits{})
	if err != nil {
		return nil, err
	}
	w.templates = w.Bundle.Templates()
	entries, err := fs.Glob(fsys, path.Join("grammars", "*.json"))
	if err != nil {
		return nil, err
	}
	sort.Strings(entries)
	var docs [][]byte
	for _, path := range entries {
		b, err := fs.ReadFile(fsys, path)
		if err != nil {
			return nil, err
		}
		docs = append(docs, b)
	}
	w.Grammars, err = router.ParseGrammarSet(docs)
	if err != nil {
		return nil, err
	}
	return w, nil
}

func (w *World) sees(caller, source string) bool {
	for _, s := range w.file.Callers[caller] {
		if s == source {
			return true
		}
	}
	return false
}

// Callers lists the fixture callers.
func (w *World) Callers() []string {
	var out []string
	for c := range w.file.Callers {
		out = append(out, c)
	}
	sort.Strings(out)
	return out
}

// Targets is what caller can see, as an authorized enumeration would give.
func (w *World) Targets(caller string) []router.Target {
	var out []router.Target
	for _, d := range w.file.Descriptors {
		if !w.sees(caller, d.Source) {
			continue
		}
		for _, c := range d.Capabilities {
			for _, a := range c.Actions {
				var compact bytes.Buffer
				_ = json.Compact(&compact, a.InputSchema)
				out = append(out, router.Target{
					Ref: router.TargetRef{Kind: router.KindAction, Action: &router.ActionRef{
						AgentID: shoal.ID(d.ID), AgentGeneration: 1, Capability: c.Name, Action: a.Name,
						RequiresApproval: a.RequiresApproval,
					}},
					Action: &router.ActionSpec{Name: a.Name, InputSchema: compact.Bytes(), RequiresApproval: a.RequiresApproval},
					Name:   a.Name,
				})
			}
		}
	}
	for _, p := range w.file.Profiles {
		if !w.sees(caller, p.Source) {
			continue
		}
		var compact bytes.Buffer
		_ = json.Compact(&compact, p.SlotSchema)
		out = append(out, router.Target{
			Ref: router.TargetRef{Kind: router.KindDecision, Decision: &router.DecisionRef{
				ProfileID: shoal.ID(p.ID), ProfileRevisionID: shoal.ID(p.Revision), TaskID: shoal.ID("task:" + p.ID),
			}},
			SlotSchema: compact.Bytes(), Name: p.Name,
		})
	}
	if w.sees(caller, w.file.OntologySource) {
		for _, t := range w.templates {
			out = append(out, router.Target{
				Ref: router.TargetRef{Kind: router.KindLookup, Lookup: &router.LookupRef{
					TemplateID: t.ID, RelationKey: t.RelationKey, Direction: t.Direction.String(),
				}},
				SubjectConcepts: t.SubjectConcepts, Name: t.RelationKey,
			})
		}
	}
	return out
}

// Input tokenizes text and resolves its mentions to the nodes caller may
// see, as the authorized resolver would.
func (w *World) Input(caller, text string, catalog *router.Catalog) router.Input {
	tokens := lexicon.Tokenize(text)
	in := router.Input{Tokens: tokens, Catalog: catalog, NodeConcepts: map[shoal.ID]shoal.ID{}}
	if len(text) > 4096 || len(tokens) > router.MaxTokens {
		in.OutOfBounds = true
		return in
	}
	in.Mentions = lexicon.Select(w.Bundle.CandidatesForTokens(tokens), func(id shoal.ID) bool {
		return w.sees(caller, w.nodeSrc[id])
	})
	for _, m := range in.Mentions {
		for _, id := range m.NodeIDs {
			in.NodeConcepts[id] = w.concepts[id]
		}
	}
	return in
}

// NodeKey maps a fixture node ID back to its key.
func (w *World) NodeKey(id shoal.ID) string { return w.nodeKey[id] }

// Case is one labelled fixture.
type Case struct {
	ID       string   `json:"id"`
	Text     string   `json:"text"`
	Caller   string   `json:"caller"`
	Split    string   `json:"split"`
	Tags     []string `json:"tags"`
	Expected Expected `json:"expected"`
	line     []byte
}

// Expected is a case's label.
type Expected struct {
	Kind   router.Kind       `json:"kind"`
	Target string            `json:"target,omitempty"`
	Slots  map[string]string `json:"slots,omitempty"`
	Reason router.Reason     `json:"reason,omitempty"`
}

// Answerable reports whether the case expects a proposal.
func (c Case) Answerable() bool { return c.Expected.Kind != router.KindAbstain }

// Cases is the loaded fixture set. Its splits are reachable only through
// Split, which refuses the held-out test split, and PreRegistered, which
// releases it only under the pre-registered digest.
type Cases struct {
	all []Case
}

// LoadCases reads cases.jsonl from fsys.
func LoadCases(fsys fs.FS) (*Cases, error) {
	f, err := fsys.Open("cases.jsonl")
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var out []Case
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 64<<10), 1<<20)
	for scanner.Scan() {
		line := append([]byte(nil), scanner.Bytes()...)
		var c Case
		d := json.NewDecoder(bytes.NewReader(line))
		d.DisallowUnknownFields()
		if err := d.Decode(&c); err != nil {
			return nil, fmt.Errorf("case %d: %w", len(out)+1, err)
		}
		switch c.Split {
		case "train", "dev", "test":
		default:
			return nil, fmt.Errorf("case %s: unknown split %q", c.ID, c.Split)
		}
		c.line = line
		out = append(out, c)
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	return &Cases{all: out}, nil
}

// Digest is the SHA-256 of a split's case lines, in file order, each
// followed by a newline. It pins exactly which cases a split holds.
func (cs *Cases) Digest(split string) string {
	h := sha256.New()
	for _, c := range cs.all {
		if c.Split == split {
			h.Write(c.line)
			h.Write([]byte{'\n'})
		}
	}
	return hex.EncodeToString(h.Sum(nil))
}

// ErrHeldOut is returned when the test split is asked for outside the
// pre-registration gate.
var ErrHeldOut = fmt.Errorf("the test split is held out: use PreRegistered")

// Split returns the cases of the train or dev split, in file order. It
// refuses the test split.
func (cs *Cases) Split(split string) ([]Case, error) {
	if split != "train" && split != "dev" {
		return nil, ErrHeldOut
	}
	return cs.selectSplit(split), nil
}

func (cs *Cases) selectSplit(split string) []Case {
	var out []Case
	for _, c := range cs.all {
		if c.Split == split {
			out = append(out, c)
		}
	}
	return out
}
