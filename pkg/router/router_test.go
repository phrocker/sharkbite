// Licensed to the Apache Software Foundation (ASF) under one or more
// contributor license agreements. See the NOTICE file distributed with this
// work for additional information regarding copyright ownership.

package router

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/phrocker/shoal-oss/pkg/explorer/fleet"
	"github.com/phrocker/shoal-oss/pkg/graph"
	"github.com/phrocker/shoal-oss/pkg/lexicon"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

// fleetValidator is the test's validator: fleet's own, as internal/routerwire
// supplies it. Only this test file imports fleet; the router does not.
type fleetValidator struct{}

func (fleetValidator) ValidateInput(schema, input json.RawMessage) (json.RawMessage, error) {
	return fleet.ValidateActionInput(fleet.Action{InputSchema: schema}, input)
}

const restartGrammar = `{
  "schema": "shoal.router.grammar/v1",
  "target": {"kind": "action", "capability": "ops", "action": "restart"},
  "cues": ["restart", "Bounce"],
  "patterns": ["[please] restart {service} in {env} [{mode}]", "bounce {service}"],
  "slots": [
    {"name": "service", "node_kinds": ["service"]},
    {"name": "env", "node_kinds": ["environment"]},
    {"name": "mode", "enum": {"graceful": ["gracefully"], "force": ["force", "hard"]}}
  ],
  "input": {"service": "$service", "environment": "$env", "mode": "$mode"}
}`

const riskGrammar = `{
  "schema": "shoal.router.grammar/v1",
  "target": {"kind": "decision", "profile": "risk"},
  "cues": ["risky"],
  "patterns": ["how risky is {op} {service}"],
  "slots": [
    {"name": "service", "node_kinds": ["service"]},
    {"name": "op", "enum": {"restart": ["restarting"]}}
  ],
  "input": {"service": "$service", "operation": "$op"}
}`

const ownerGrammar = `{
  "schema": "shoal.router.grammar/v1",
  "target": {"kind": "lookup", "template": "lookup:owned_by:out"},
  "cues": ["owns"],
  "patterns": ["who owns {subject}"],
  "slots": [{"name": "subject"}]
}`

var restartSchema = json.RawMessage(`{"type":"object","required":["service","environment"],"additionalProperties":false,"properties":{"service":{"type":"string"},"environment":{"type":"string"},"mode":{"type":"string","enum":["graceful","force"]}}}`)

func grammarSet(t testing.TB, docs ...string) *GrammarSet {
	t.Helper()
	raw := make([][]byte, len(docs))
	for i, d := range docs {
		raw[i] = []byte(d)
	}
	set, err := ParseGrammarSet(raw)
	if err != nil {
		t.Fatal(err)
	}
	return set
}

func restartTarget(agent string) Target {
	return Target{
		Ref:    TargetRef{Kind: KindAction, Action: &ActionRef{AgentID: shoal.ID(agent), AgentGeneration: 1, Capability: "ops", Action: "restart", RequiresApproval: true}},
		Action: &ActionSpec{Name: "restart", InputSchema: restartSchema, RequiresApproval: true},
		Name:   "restart",
	}
}

func riskTarget() Target {
	return Target{
		Ref:        TargetRef{Kind: KindDecision, Decision: &DecisionRef{ProfileID: "risk", ProfileRevisionID: "r1", TaskID: "task:risk"}},
		SlotSchema: json.RawMessage(`{"type":"object","required":["service","operation"],"properties":{"service":{"type":"string"},"operation":{"type":"string","enum":["restart"]}}}`),
		Name:       "risk",
	}
}

func ownerTarget() Target {
	return Target{
		Ref:             TargetRef{Kind: KindLookup, Lookup: &LookupRef{TemplateID: "lookup:owned_by:out", RelationKey: "owned_by", Direction: "out"}},
		SubjectConcepts: []shoal.ID{"concept:service"},
		Name:            "owned_by",
	}
}

type world struct {
	bundle   *lexicon.Bundle
	concepts map[shoal.ID]shoal.ID
}

func newWorld(t testing.TB) world {
	t.Helper()
	nodes := []graph.Node{
		{ID: "n:payments", Kind: "service", Properties: shoal.Metadata{"name": "Payments"}},
		{ID: "n:payments-api", Kind: "service", Properties: shoal.Metadata{"name": "Payments API"}},
		{ID: "n:prod", Kind: "environment", Properties: shoal.Metadata{"name": "Production", "shoal.lexicon.alias.0": "prod"}},
		{ID: "n:core-svc", Kind: "service", Properties: shoal.Metadata{"name": "Core"}},
		{ID: "n:core-team", Kind: "team", Properties: shoal.Metadata{"name": "Core"}},
	}
	b, err := lexicon.Build(lexicon.Input{Snapshot: lexicon.Snapshot{ID: "s", AsOf: time.Unix(0, 0), Frontier: 1}, Nodes: nodes}, lexicon.Limits{})
	if err != nil {
		t.Fatal(err)
	}
	return world{bundle: b, concepts: map[shoal.ID]shoal.ID{
		"n:payments": "concept:service", "n:payments-api": "concept:service",
		"n:prod": "concept:environment", "n:core-svc": "concept:service", "n:core-team": "concept:team",
	}}
}

func (w world) input(text string, catalog *Catalog) Input {
	tokens := lexicon.Tokenize(text)
	return Input{
		Tokens:       tokens,
		Mentions:     lexicon.Select(w.bundle.CandidatesForTokens(tokens), func(shoal.ID) bool { return true }),
		Catalog:      catalog,
		NodeConcepts: w.concepts,
	}
}

func catalog(t testing.TB, set *GrammarSet, targets ...Target) *Catalog {
	t.Helper()
	c, err := NewCatalog(targets, set, fleetValidator{})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func analyze(t testing.TB, w world, c *Catalog, text string) *Analysis {
	t.Helper()
	a, err := Analyze(w.input(text, c))
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func choose(a *Analysis, key string) []bool {
	matched := make([]bool, len(a.cands))
	for i, k := range a.catalog.Keys() {
		matched[i] = k == key
	}
	return matched
}

func TestParseGrammarRefusals(t *testing.T) {
	valid := restartGrammar
	if _, err := ParseGrammar([]byte(valid)); err != nil {
		t.Fatal(err)
	}
	cases := map[string]string{
		"wrong schema":          strings.Replace(valid, "grammar/v1", "grammar/v2", 1),
		"duplicate key":         strings.Replace(valid, `"cues":`, `"cues": [], "cues":`, 1),
		"case-variant key":      strings.Replace(valid, `"cues":`, `"Cues":`, 1),
		"unknown key":           strings.Replace(valid, `"cues":`, `"regex": "x", "cues":`, 1),
		"trailing data":         valid + `{}`,
		"nested optional":       strings.Replace(valid, `[please]`, `[please [now]]`, 1),
		"undeclared slot":       strings.Replace(valid, `bounce {service}`, `bounce {svc}`, 1),
		"slot twice":            strings.Replace(valid, `bounce {service}`, `bounce {service} {service}`, 1),
		"no literal":            strings.Replace(valid, `bounce {service}`, `[bounce] {service}`, 1),
		"multi-token cue":       strings.Replace(valid, `"Bounce"`, `"bounce it"`, 1),
		"shared enum phrase":    strings.Replace(valid, `["force", "hard"]`, `["force", "gracefully"]`, 1),
		"unused slot":           strings.Replace(valid, `, "mode": "$mode"`, ``, 1),
		"template unknown slot": strings.Replace(valid, `"$mode"`, `"$other"`, 1),
		"missing input": strings.Replace(valid, `,
  "input": {"service": "$service", "environment": "$env", "mode": "$mode"}`, ``, 1),
		"target extra field":  strings.Replace(valid, `"action": "restart"}`, `"action": "restart", "profile": "x"}`, 1),
		"unknown target kind": strings.Replace(valid, `"kind": "action"`, `"kind": "shell"`, 1),
		"slot name casing":    strings.Replace(valid, `"name": "env"`, `"name": "Env"`, 1),
		"lone surrogate":      strings.Replace(valid, `"restart", "Bounce"`, `"restart", "\ud800"`, 1),
		"invalid utf8":        strings.Replace(valid, `"Bounce"`, "\"B\xffounce\"", 1),
		"unterminated group":  strings.Replace(valid, `[please]`, `[please`, 1),
		"node and enum slot":  strings.Replace(valid, `{"name": "service", "node_kinds": ["service"]}`, `{"name": "service", "node_kinds": ["service"], "enum": {"a": ["a"]}}`, 1),
		"lookup with input":   strings.Replace(ownerGrammar, `"slots": [{"name": "subject"}]`, `"slots": [{"name": "subject"}], "input": {"s": "$subject"}`, 1),
		"lookup wrong slot":   strings.Replace(ownerGrammar, `{subject}`, `{who}`, 1),
		"oversized":           strings.Replace(valid, `"restart", "Bounce"`, `"restart", "`+strings.Repeat("a", MaxGrammarBytes)+`"`, 1),
	}
	for name, doc := range cases {
		if _, err := ParseGrammar([]byte(doc)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if _, err := ParseGrammarSet([][]byte{[]byte(valid), []byte(valid)}); err == nil {
		t.Error("a set binding one key twice was accepted")
	}
}

func TestGrammarSetDigestIsOrderIndependent(t *testing.T) {
	a := grammarSet(t, restartGrammar, riskGrammar, ownerGrammar)
	b := grammarSet(t, ownerGrammar, restartGrammar, riskGrammar)
	if a.Digest() != b.Digest() || a.Digest() == grammarSet(t, restartGrammar).Digest() {
		t.Fatal("grammar set digest is order-dependent or ignores content")
	}
	changed := grammarSet(t, strings.Replace(restartGrammar, `"Bounce"`, `"Reboot"`, 1), riskGrammar, ownerGrammar)
	if changed.Digest() == a.Digest() {
		t.Fatal("grammar set digest ignores a cue")
	}
}

func TestSlotsCoverWholeMentionsOnly(t *testing.T) {
	w := newWorld(t)
	c := catalog(t, grammarSet(t, restartGrammar), restartTarget("agent-a"))
	// "payments api" is one mention: the slot takes all of it.
	a := analyze(t, w, c, "restart Payments API in prod")
	p, err := a.Propose(choose(a, "action:ops/restart"), Receipt{})
	if err != nil || p.Kind != KindAction || len(p.Slots) != 2 || p.Slots[1].NodeIDs[0] != "n:payments-api" {
		t.Fatalf("proposal = %+v, %v", p, err)
	}
	// A word that is not a mention never fills a node slot.
	a = analyze(t, w, c, "restart widgets in prod")
	if a.cands[0].best != nil {
		t.Fatalf("pattern matched a non-mention: %+v", a.cands[0].best)
	}
	p, _ = a.Propose(choose(a, "action:ops/restart"), Receipt{})
	if p.Kind != KindAbstain || p.Reasons[0] != ReasonMissingSlot || p.Missing[0] != "service" {
		t.Fatalf("non-mention slot = %+v", p)
	}
	// An ambiguous mention abstains; it is never resolved.
	a = analyze(t, w, c, "restart core in prod")
	p, _ = a.Propose(choose(a, "action:ops/restart"), Receipt{})
	if p.Kind != KindAbstain || p.Reasons[0] != ReasonAmbiguousMention {
		t.Fatalf("ambiguous mention = %+v", p)
	}
	// An enum slot takes the grammar's value, not the text.
	a = analyze(t, w, c, "please restart payments in prod hard")
	p, _ = a.Propose(choose(a, "action:ops/restart"), Receipt{})
	if p.Kind != KindAction || string(p.Input) != `{"environment":"n:prod","mode":"force","service":"n:payments"}` {
		t.Fatalf("enum proposal = %+v %s", p, p.Input)
	}
}

func TestAggregation(t *testing.T) {
	w := newWorld(t)
	c := catalog(t, grammarSet(t, restartGrammar, riskGrammar), restartTarget("agent-a"), riskTarget())
	a := analyze(t, w, c, "restart payments in prod")
	for _, tc := range []struct {
		matched []bool
		want    Reason
	}{{[]bool{false, false}, ReasonNoTarget}, {[]bool{true, true}, ReasonAmbiguousTarget}} {
		p, err := a.Propose(tc.matched, Receipt{})
		if err != nil || p.Kind != KindAbstain || p.Reasons[0] != tc.want || p.Target != nil {
			t.Fatalf("%v: %+v %v", tc.matched, p, err)
		}
	}
	if _, err := a.Propose([]bool{true}, Receipt{}); err == nil {
		t.Fatal("answers not covering the candidates were accepted")
	}
	two := catalog(t, grammarSet(t, restartGrammar), restartTarget("agent-b"), restartTarget("agent-a"))
	a = analyze(t, w, two, "restart payments in prod")
	p, _ := a.Propose([]bool{true}, Receipt{})
	if p.Kind != KindAbstain || p.Reasons[0] != ReasonAmbiguousExecutor || p.Target != nil {
		t.Fatalf("two executors = %+v", p)
	}
	for _, text := range []string{"", "?!"} {
		a = analyze(t, w, c, text)
		p, _ = a.Propose(nil, Receipt{})
		if p.Reasons[0] != ReasonEmptyText {
			t.Fatalf("%q = %+v", text, p)
		}
	}
	long := strings.Repeat("restart ", MaxTokens+1)
	a = analyze(t, w, c, long)
	if p, _ = a.Propose(nil, Receipt{}); p.Reasons[0] != ReasonTextOutOfBounds {
		t.Fatalf("over-bound = %+v", p)
	}
}

func TestDecisionAndLookupValidation(t *testing.T) {
	w := newWorld(t)
	c := catalog(t, grammarSet(t, restartGrammar, riskGrammar, ownerGrammar), restartTarget("a"), riskTarget(), ownerTarget())
	a := analyze(t, w, c, "how risky is restarting payments")
	p, _ := a.Propose(choose(a, "decision:risk"), Receipt{})
	if p.Kind != KindDecision || string(p.Input) != `{"operation":"restart","service":"n:payments"}` {
		t.Fatalf("decision = %+v %s", p, p.Input)
	}
	a = analyze(t, w, c, "who owns payments")
	p, _ = a.Propose(choose(a, "lookup:lookup:owned_by:out"), Receipt{})
	if p.Kind != KindLookup || string(p.Input) != `{"subject":"n:payments"}` {
		t.Fatalf("lookup = %+v", p)
	}
	a = analyze(t, w, c, "who owns prod")
	p, _ = a.Propose(choose(a, "lookup:lookup:owned_by:out"), Receipt{})
	if p.Kind != KindAbstain || p.Reasons[0] != ReasonSlotMismatch {
		t.Fatalf("concept mismatch = %+v", p)
	}
	a = analyze(t, w, c, "restart payments")
	p, _ = a.Propose(choose(a, "action:ops/restart"), Receipt{})
	if p.Kind != KindAbstain || p.Reasons[0] != ReasonMissingSlot || strings.Join(p.Missing, ",") != "env" || p.Target == nil {
		t.Fatalf("missing slot = %+v", p)
	}
}

// TestInputIsWhatEnqueueStores: a template that repeats a key yields the
// canonical bytes fleet.ValidateActionInput returns, not the rendered bytes.
func TestInputIsWhatEnqueueStores(t *testing.T) {
	doc := strings.Replace(restartGrammar, `"input": {"service": "$service",`, `"input": {"mode": "force", "service": "$service",`, 1)
	w := newWorld(t)
	c := catalog(t, grammarSet(t, doc), restartTarget("a"))
	a := analyze(t, w, c, "restart payments in prod gracefully")
	p, _ := a.Propose([]bool{true}, Receipt{})
	g := c.candidates[0].grammar
	rendered, err := renderTemplate(g.input, func(name string) (string, bool, error) {
		return map[string]string{"service": "n:payments", "env": "n:prod", "mode": "graceful"}[name], true, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(rendered, []byte(`"mode":"force"`)) || !bytes.Contains(rendered, []byte(`"mode":"graceful"`)) {
		t.Fatalf("rendered = %s", rendered)
	}
	want, err := fleet.ValidateActionInput(fleet.Action{InputSchema: restartSchema}, rendered)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(p.Input, want) || string(want) != `{"environment":"n:prod","mode":"graceful","service":"n:payments"}` {
		t.Fatalf("proposal input %s, enqueue would store %s", p.Input, want)
	}
}

func TestDeterminism(t *testing.T) {
	w := newWorld(t)
	set := grammarSet(t, restartGrammar, riskGrammar, ownerGrammar)
	one := catalog(t, set, restartTarget("a"), riskTarget(), ownerTarget())
	two := catalog(t, set, ownerTarget(), restartTarget("a"), riskTarget())
	if one.Digest() != two.Digest() {
		t.Fatal("catalog digest depends on target order")
	}
	a1 := analyze(t, w, one, "please restart payments in prod")
	a2 := analyze(t, w, two, "please restart payments in prod")
	if !bytes.Equal(a1.FeatureArtifact().Bytes, a2.FeatureArtifact().Bytes) {
		t.Fatal("feature artifact depends on target order")
	}
	var decoded struct {
		Schema   int    `json:"schema"`
		Features string `json:"feature_schema_id"`
		Subjects []struct {
			ID       string    `json:"id"`
			Features []float64 `json:"features"`
		} `json:"subjects"`
	}
	if err := json.Unmarshal(a1.FeatureArtifact().Bytes, &decoded); err != nil || len(decoded.Subjects) != 3 || len(decoded.Subjects[0].Features) != NumFeatures {
		t.Fatalf("artifact = %s, %v", a1.FeatureArtifact().Bytes, err)
	}
	for _, s := range decoded.Subjects {
		for _, x := range s.Features {
			if x < 0 || x > 1 {
				t.Fatalf("feature %v outside [0, 1]", x)
			}
		}
	}
	if bytes.Contains(a1.FeatureArtifact().Bytes, []byte("payments")) || bytes.Contains(a1.FeatureArtifact().Bytes, []byte("restart")) {
		t.Fatal("feature artifact carries text or a target name")
	}
	p1, _ := a1.Propose(choose(a1, "action:ops/restart"), Receipt{CatalogDigest: one.Digest()})
	p2, _ := a2.Propose(choose(a2, "action:ops/restart"), Receipt{CatalogDigest: two.Digest()})
	if p1.ID != p2.ID || p1.Validate() != nil {
		t.Fatalf("proposal IDs differ or invalid: %s %s", p1.ID, p2.ID)
	}
	tampered := p1
	tampered.Input = json.RawMessage(`{"service":"n:prod","environment":"n:prod"}`)
	if tampered.Validate() == nil {
		t.Fatal("tampered proposal validated")
	}
	examples := []Example{{Features: a1.Features()[0], Match: true}, {Features: a1.Features()[1]}, {Features: a1.Features()[2]}}
	w1, b1 := Train(examples, DefaultTrainConfig())
	w2, b2 := Train(examples, DefaultTrainConfig())
	if w1 != w2 || b1 != b2 {
		t.Fatal("training is not deterministic")
	}
}

func TestHiddenGrammarNeverBinds(t *testing.T) {
	w := newWorld(t)
	set := grammarSet(t, restartGrammar, riskGrammar)
	// The risk grammar exists, but its target is not visible.
	c := catalog(t, set, restartTarget("a"))
	if strings.Join(c.Keys(), ",") != "action:ops/restart" {
		t.Fatalf("keys = %v", c.Keys())
	}
	a := analyze(t, w, c, "how risky is restarting payments")
	if a.Candidates() != 1 {
		t.Fatal("a grammar without a visible target became a candidate")
	}
	none := catalog(t, nil, restartTarget("a"))
	if none.Digest() == c.Digest() || c.GrammarSetDigest() == none.GrammarSetDigest() {
		t.Fatal("bound grammar not reflected in digests")
	}
}

func TestCatalogBound(t *testing.T) {
	targets := make([]Target, MaxTargets+1)
	for i := range targets {
		targets[i] = restartTarget(strings.Repeat("a", i+1))
	}
	if _, err := NewCatalog(targets, nil, fleetValidator{}); err != ErrTooManyTargets {
		t.Fatalf("over-bound catalog = %v", err)
	}
}

func FuzzRoute(f *testing.F) {
	for _, s := range []string{"restart payments in prod", "how risky is restarting payments", "who owns core", "", "\x00\xff", "[please] {service}", strings.Repeat("prod ", 40)} {
		f.Add(s, uint8(1))
	}
	w := newWorld(f)
	c := catalog(f, grammarSet(f, restartGrammar, riskGrammar, ownerGrammar), restartTarget("a"), riskTarget(), ownerTarget())
	f.Fuzz(func(t *testing.T, text string, bits uint8) {
		a, err := Analyze(w.input(text, c))
		if err != nil {
			t.Fatalf("analyze: %v", err)
		}
		matched := make([]bool, a.Candidates())
		for i := range matched {
			matched[i] = bits&(1<<i) != 0
		}
		for _, propose := range []func() (Proposal, error){
			func() (Proposal, error) { return a.Propose(matched, a.Receipt()) },
			a.Baseline,
		} {
			p, err := propose()
			if err != nil {
				t.Fatalf("propose: %v", err)
			}
			if err := p.Validate(); err != nil {
				t.Fatalf("invalid proposal %+v: %v", p, err)
			}
			if p.Kind != KindAbstain && !json.Valid(p.Input) {
				t.Fatal("invalid input")
			}
		}
	})
}
