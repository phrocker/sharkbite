// Licensed to the Apache Software Foundation (ASF) under one
// or more contributor license agreements. See the NOTICE file
// distributed with this work for additional information
// regarding copyright ownership. The ASF licenses this file
// to you under the Apache License, Version 2.0 (the
// "License"); you may not use this file except in compliance
// with the License. You may obtain a copy of the License at
//
//     https://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing,
// software distributed under the License is distributed on an
// "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY
// KIND, either express or implied. See the License for the
// specific language governing permissions and limitations
// under the License.

package atpl

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/phrocker/shoal-oss/internal/atpltest"
	"github.com/phrocker/shoal-oss/pkg/explorer/fleet"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

var testNow = time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)

// basePolicy is the fixture every test varies: a root agent that may open
// tickets, and a delegated searcher inheriting its query action.
const basePolicy = `{
  "atpl": "shoal.atpl/v1",
  "origin": "Derived from the Agent Trust Policy Language, github.com/SentriusLLC/atpl (Apache-2.0)",
  "executors": [
    {"ref": "search-exec", "max_effects": ["reads-corpus"], "min_effects": ["reads-corpus"]},
    {"ref": "remote-exec", "max_effects": ["external", "reads-corpus"], "min_effects": []}
  ],
  "agents": [
    {
      "id": "planner",
      "authorization_domain": "domain",
      "scopes": [
        {"source_id": "source-a", "policy_id": "policy"},
        {"source_id": "source-b", "policy_id": "policy"}
      ],
      "executor_ref": "remote-exec",
      "lease_ttl": "12h",
      "capabilities": [
        {"name": "search", "actions": [
          {"name": "query", "effects": ["reads-corpus"],
           "input_schema": {"type": "object"}, "output_schema": {"type": "object"}}
        ]},
        {"name": "tickets", "actions": [
          {"name": "open", "effects": ["external"],
           "input_schema": {"type": "object", "properties": {"title": {"type": "string"}}},
           "output_schema": {"type": "object"}}
        ]}
      ]
    },
    {
      "id": "searcher",
      "parent": "planner",
      "authorization_domain": "domain",
      "scopes": [{"source_id": "source-a", "policy_id": "policy"}],
      "executor_ref": "search-exec",
      "lease_ttl": "6h",
      "capabilities": [
        {"name": "search", "actions": [{"name": "query", "inherit": true}]}
      ]
    }
  ]
}`

func decodeString(t *testing.T, name, text string) Document {
	t.Helper()
	document, err := Decode(name, strings.NewReader(text))
	if err != nil {
		t.Fatalf("decode %s: %v", name, err)
	}
	return document
}

func base(t *testing.T) Document {
	t.Helper()
	return decodeString(t, "base.atpl.json", basePolicy)
}

func agentByID(document *Document, id string) *Agent {
	for i := range document.Agents {
		if document.Agents[i].ID == id {
			return &document.Agents[i]
		}
	}
	return nil
}

func compileOne(t *testing.T, document Document) *Policy {
	t.Helper()
	policy, err := Compile([]Document{document}, testNow, nil)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	return policy
}

// hostExecutors binds the policy's executor assertions as the host would, so
// the registry enforces the same ceilings and floors the file declares.
func hostExecutors(documents ...Document) atpltest.Executors {
	result := atpltest.Executors{}
	for _, document := range documents {
		for _, executor := range document.Executors {
			bound := atpltest.Executor{}
			for _, effect := range executor.MaxEffects {
				bound.Max = append(bound.Max, fleet.Effect(effect))
			}
			for _, effect := range executor.MinEffects {
				bound.Min = append(bound.Min, fleet.Effect(effect))
			}
			result[executor.Ref] = bound
		}
	}
	return result
}

func newRegistry(t *testing.T, documents ...Document) *atpltest.Registry {
	t.Helper()
	return atpltest.NewRegistry(t, atpltest.NewClock(testNow), hostExecutors(documents...),
		[]string{"source-a", "source-b", "source-c"}, []string{"policy"})
}

// applyDirect performs a plan's writes through Service.Register, parent first,
// as shoalctl does over HTTP.
func applyDirect(t *testing.T, registry *atpltest.Registry, plan Plan) {
	t.Helper()
	for _, entry := range plan.Writes() {
		key := fmt.Sprintf("key-%s-%d", entry.ID, entry.LiveGeneration)
		if _, err := registry.Register(t, entry.Spec, entry.LiveGeneration, key); err != nil {
			t.Fatalf("register %s: %v", entry.ID, err)
		}
	}
}

// rawSpec converts a file agent to a registration without validating it, so
// the parity test can hand Register exactly what the file says.
func rawSpec(agent Agent, now time.Time, parent *fleet.Spec) fleet.Spec {
	ttl, _ := time.ParseDuration(agent.LeaseTTL)
	spec := fleet.Spec{
		ID: shoal.ID(agent.ID), ParentID: shoal.ID(agent.Parent),
		AuthorizationDomain: []byte(agent.AuthorizationDomain),
		ExecutorRef:         agent.ExecutorRef, LeaseExpiresAt: now.Add(ttl),
	}
	for _, scope := range agent.Scopes {
		spec.Scopes = append(spec.Scopes, fleet.Scope{
			SourceID: []byte(scope.SourceID), PolicyID: []byte(scope.PolicyID),
		})
	}
	for _, capability := range agent.Capabilities {
		compiled := fleet.Capability{Name: capability.Name}
		for _, action := range capability.Actions {
			converted := fleet.Action{
				Name: action.Name, InputSchema: action.InputSchema, OutputSchema: action.OutputSchema,
				RequiresApproval:    action.Approval != nil && action.Approval.Required,
				RequiresAttestation: action.Attestation != nil && action.Attestation.Required,
			}
			for _, effect := range action.Effects {
				converted.Effects = append(converted.Effects, fleet.Effect(effect))
			}
			if action.Inherit && parent != nil {
				if inherited := findAction(findCapability(parent.Capabilities, capability.Name), action.Name); inherited != nil {
					adds, addsAttestation := converted.RequiresApproval, converted.RequiresAttestation
					converted = *inherited
					converted.RequiresApproval = converted.RequiresApproval || adds
					converted.RequiresAttestation = converted.RequiresAttestation || addsAttestation
				}
			}
			compiled.Actions = append(compiled.Actions, converted)
		}
		spec.Capabilities = append(spec.Capabilities, compiled)
	}
	return spec
}

func TestRoundTripThroughTheRegistryYieldsIdenticalRegistrations(t *testing.T) {
	document := base(t)
	policy := compileOne(t, document)
	registry := newRegistry(t, document)
	applyDirect(t, registry, Diff(policy, registry.Live(t), ""))

	live := registry.Live(t)
	if len(live) != 2 {
		t.Fatalf("live agents = %d, want 2", len(live))
	}
	exported, err := Export(live, document.Executors, testNow)
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.MarshalIndent(exported, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	reread := decodeString(t, "exported.atpl.json", string(encoded))
	recompiled, err := Compile([]Document{reread}, testNow, nil)
	if err != nil {
		t.Fatalf("recompile export: %v\n%s", err, encoded)
	}
	if !reflect.DeepEqual(recompiled.Agents(), policy.Agents()) {
		t.Fatalf("registrations differ after round trip:\n%#v\n%#v", recompiled.Agents(), policy.Agents())
	}
	if recompiled.Digest() != policy.Digest() {
		t.Fatalf("digest changed across round trip: %s != %s", recompiled.Digest(), policy.Digest())
	}
	for _, spec := range policy.Agents() {
		descriptor := live[spec.ID]
		if !reflect.DeepEqual(descriptor.Capabilities, spec.Capabilities) ||
			!reflect.DeepEqual(descriptor.Scopes, spec.Scopes) ||
			!descriptor.LeaseExpiresAt.Equal(spec.LeaseExpiresAt) {
			t.Fatalf("registry stored something other than the compiled spec for %s", spec.ID)
		}
	}
	if plan := Diff(recompiled, live, ""); len(plan.Writes()) != 0 || len(plan.Refusals()) != 0 {
		t.Fatalf("recompiled export does not plan as unchanged: %+v", plan.Entries)
	}
}

// The registry records an "atpl-apply" reason detail only if it has exactly
// the shape Digest produces; a drift between the two would refuse every apply.
func TestDigestIsAcceptedAsRegistryAssertedPolicySource(t *testing.T) {
	if fleet.ATPLPolicyDigestPrefix != DigestPrefix {
		t.Fatalf("registry prefix %q != %q", fleet.ATPLPolicyDigestPrefix, DigestPrefix)
	}
	digest := compileOne(t, base(t)).Digest()
	reason, err := fleet.CallerAssertedRegistryReason(fleet.ReasonCodeATPLApply, digest)
	if err != nil || reason.Source != digest {
		t.Fatalf("compiled digest %q as asserted source = %#v, %v", digest, reason, err)
	}
}

func TestDigestIsStableAcrossRecompilesOrderAndTime(t *testing.T) {
	first := compileOne(t, base(t))
	again := compileOne(t, base(t))
	if first.Digest() != again.Digest() || !strings.HasPrefix(first.Digest(), DigestPrefix) {
		t.Fatalf("recompile digest %s != %s", again.Digest(), first.Digest())
	}
	later, err := Compile([]Document{base(t)}, testNow.Add(3*time.Hour), nil)
	if err != nil {
		t.Fatal(err)
	}
	if later.Digest() != first.Digest() {
		t.Fatal("digest depends on compile time")
	}

	// The same policy split across files, every list reversed.
	shuffled := base(t)
	reverse := func(n int, swap func(i, j int)) {
		for i, j := 0, n-1; i < j; i, j = i+1, j-1 {
			swap(i, j)
		}
	}
	reverse(len(shuffled.Executors), func(i, j int) {
		shuffled.Executors[i], shuffled.Executors[j] = shuffled.Executors[j], shuffled.Executors[i]
	})
	for i := range shuffled.Executors {
		effects := shuffled.Executors[i].MaxEffects
		reverse(len(effects), func(a, b int) { effects[a], effects[b] = effects[b], effects[a] })
	}
	planner := agentByID(&shuffled, "planner")
	reverse(len(planner.Scopes), func(i, j int) { planner.Scopes[i], planner.Scopes[j] = planner.Scopes[j], planner.Scopes[i] })
	reverse(len(planner.Capabilities), func(i, j int) {
		planner.Capabilities[i], planner.Capabilities[j] = planner.Capabilities[j], planner.Capabilities[i]
	})
	planner.Capabilities[0].Actions[0].InputSchema = json.RawMessage(
		`{"properties": {"title": {"type": "string"}}, "type": "object"}`)
	agents := Document{ATPL: Version, Origin: Origin, Agents: []Agent{shuffled.Agents[1]}}.WithName("a.atpl.json")
	roots := Document{ATPL: Version, Origin: Origin, Agents: []Agent{shuffled.Agents[0]}}.WithName("b.atpl.json")
	executors := Document{ATPL: Version, Origin: Origin, Executors: shuffled.Executors}.WithName("c.atpl.json")
	for _, order := range [][]Document{{agents, roots, executors}, {executors, roots, agents}} {
		policy, err := Compile(order, testNow, nil)
		if err != nil {
			t.Fatal(err)
		}
		if policy.Digest() != first.Digest() {
			t.Fatalf("reordered policy digest %s != %s", policy.Digest(), first.Digest())
		}
		if !reflect.DeepEqual(policy.Agents(), first.Agents()) {
			t.Fatal("reordered policy compiled to different registrations")
		}
	}

	changed := base(t)
	agentByID(&changed, "searcher").LeaseTTL = "5h"
	if compileOne(t, changed).Digest() == first.Digest() {
		t.Fatal("a TTL change did not change the digest")
	}
}

func TestCompiledOrderIsParentsFirstThenByID(t *testing.T) {
	document := base(t)
	child := *agentByID(&document, "searcher")
	child.ID = "a-grandchild"
	child.Parent = "searcher"
	child.LeaseTTL = "1h"
	child.Capabilities = []Capability{{Name: "search", Actions: []Action{{Name: "query", Inherit: true}}}}
	document.Agents = append([]Agent{child}, document.Agents...)
	var ids []shoal.ID
	for _, spec := range compileOne(t, document).Agents() {
		ids = append(ids, spec.ID)
	}
	if want := []shoal.ID{"planner", "searcher", "a-grandchild"}; !reflect.DeepEqual(ids, want) {
		t.Fatalf("order = %v, want %v", ids, want)
	}
}

// TestRefusalsNameThePathAndMatchRegister is the parity test: every fixture
// the compiler refuses, Register refuses too.
func TestRefusalsNameThePathAndMatchRegister(t *testing.T) {
	explicitQuery := func(effects []string, input string) Action {
		return Action{Name: "query", Effects: effects,
			InputSchema: json.RawMessage(input), OutputSchema: json.RawMessage(`{"type":"object"}`)}
	}
	cases := []struct {
		name   string
		agent  string
		mutate func(*Document)
		path   string
	}{
		{"child capability outside parent", "searcher", func(d *Document) {
			agent := agentByID(d, "searcher")
			agent.Capabilities = append(agent.Capabilities, Capability{Name: "admin", Actions: []Action{
				{Name: "wipe", Effects: []string{"reads-corpus"}, InputSchema: json.RawMessage(`{}`), OutputSchema: json.RawMessage(`{}`)},
			}})
		}, "agents[id=searcher].capabilities[name=admin]: parent agents[id=planner] has no capability admin"},
		{"child action outside parent", "searcher", func(d *Document) {
			agent := agentByID(d, "searcher")
			agent.Capabilities[0].Actions = append(agent.Capabilities[0].Actions, Action{
				Name: "delete", Effects: []string{"reads-corpus"},
				InputSchema: json.RawMessage(`{}`), OutputSchema: json.RawMessage(`{}`)})
		}, "agents[id=searcher].capabilities[name=search].actions[name=delete]: parent agents[id=planner] has no action delete"},
		{"child effect outside parent", "searcher", func(d *Document) {
			agent := agentByID(d, "searcher")
			agent.ExecutorRef = "remote-exec"
			agent.Capabilities[0].Actions[0] = explicitQuery([]string{"reads-corpus", "external"}, `{"type":"object"}`)
		}, "agents[id=searcher].capabilities[name=search].actions[name=query].effects: external is not among parent"},
		{"child schema differs from parent", "searcher", func(d *Document) {
			agent := agentByID(d, "searcher")
			agent.Capabilities[0].Actions[0] = explicitQuery([]string{"reads-corpus"}, `{"type":"object","required":["q"]}`)
		}, "agents[id=searcher].capabilities[name=search].actions[name=query].input_schema: differs from parent"},
		{"scope outside parent", "searcher", func(d *Document) {
			agentByID(d, "searcher").Scopes = []Scope{{SourceID: "source-c", PolicyID: "policy"}}
		}, "agents[id=searcher].scopes[0]: source source-c policy policy is not among parent"},
		{"domain differs from parent", "searcher", func(d *Document) {
			agentByID(d, "searcher").AuthorizationDomain = "elsewhere"
		}, "agents[id=searcher].authorization_domain: differs from parent"},
		{"child TTL outlasts parent", "searcher", func(d *Document) {
			agentByID(d, "searcher").LeaseTTL = "13h"
		}, "agents[id=searcher].lease_ttl: 13h0m0s outlasts parent agents[id=planner]'s lease_ttl 12h0m0s"},
		{"effects above executor max", "planner", func(d *Document) {
			agentByID(d, "planner").Capabilities[0].Actions[0].Effects = []string{"egresses-content"}
		}, "agents[id=planner].capabilities[name=search].actions[name=query].effects: action declares effects its executor is not bound to perform"},
		// tickets.open rather than search.query, because searcher inherits
		// query and would be refused for its own reason first. remote-exec
		// has an external ceiling and no floor, so the action may not stay
		// silent about what it does.
		{"dispatch-only action declares nothing", "planner", func(d *Document) {
			agentByID(d, "planner").Capabilities[1].Actions[0].Effects = nil
		}, "agents[id=planner].capabilities[name=tickets].actions[name=open].effects: action declares no effects at all and is bound to an executor that may reach outside Shoal (external)"},
		{"effects below executor min", "searcher", func(d *Document) {
			agentByID(d, "searcher").Capabilities[0].Actions[0] = explicitQuery(nil, `{"type":"object"}`)
		}, "agents[id=searcher].capabilities[name=search].actions[name=query].effects: action omits effects its executor causes on every invocation (reads-corpus)"},
		{"unknown executor_ref", "searcher", func(d *Document) {
			agentByID(d, "searcher").ExecutorRef = "ghost"
		}, "agents[id=searcher].executor_ref: ghost is not declared in the policy's executors"},
		{"bad inherit", "searcher", func(d *Document) {
			agentByID(d, "searcher").Capabilities[0].Actions[0].Name = "missing"
		}, "agents[id=searcher].capabilities[name=search].actions[name=missing].inherit: parent agents[id=planner] has no action missing"},
		{"unknown effect", "planner", func(d *Document) {
			agentByID(d, "planner").Capabilities[0].Actions[0].Effects = []string{"teleport"}
		}, `agents[id=planner].capabilities[name=search].actions[name=query].effects: "teleport" is not a known effect class`},
		{"lease beyond the registry bound", "planner", func(d *Document) {
			agentByID(d, "planner").LeaseTTL = "25h"
		}, "agents[id=planner].lease_ttl: must be positive and at most 23h55m0s"},
		{"capability name the registry refuses", "planner", func(d *Document) {
			agentByID(d, "planner").Capabilities[0].Name = "bad name"
		}, `agents[id=planner].capabilities[name="bad name"].actions[name=query]: capability name contains an unsupported character`},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			document := base(t)
			test.mutate(&document)
			_, err := Compile([]Document{document}, testNow, nil)
			if !shoal.IsErrorCode(err, shoal.ErrorInvalidArgument) {
				t.Fatalf("compile = %v, want invalid argument", err)
			}
			if want := "base.atpl.json: " + test.path; !strings.Contains(err.Error(), want) {
				t.Fatalf("refusal %q does not contain %q", err, want)
			}

			registry := newRegistry(t, base(t))
			var parent *fleet.Spec
			if test.agent == "searcher" {
				planner := compileOne(t, base(t)).Agents()[0]
				if _, err := registry.Register(t, planner, 0, "key-planner"); err != nil {
					t.Fatal(err)
				}
				parent = &planner
			}
			spec := rawSpec(*agentByID(&document, test.agent), testNow, parent)
			if _, err := registry.Register(t, spec, 0, "key-"+test.agent); err == nil {
				t.Fatal("Register accepted what the compiler refused")
			}
		})
	}
}

func TestOfflineCompileRequiresDeclaredParents(t *testing.T) {
	document := base(t)
	document.Agents = document.Agents[1:]
	if _, err := Compile([]Document{document}, testNow, nil); err == nil ||
		!strings.Contains(err.Error(), "agents[id=searcher].parent: agents[id=planner] is not declared in the policy") {
		t.Fatalf("undeclared parent offline = %v", err)
	}
	live := LiveParents{"planner": {
		ID: "planner", Generation: 1, AuthorizationDomain: []byte("domain"),
		Scopes:         []fleet.Scope{{SourceID: []byte("source-a"), PolicyID: []byte("policy")}},
		Capabilities:   compileOne(t, base(t)).Agents()[0].Capabilities,
		LeaseExpiresAt: testNow.Add(2 * time.Hour),
	}}
	if _, err := Compile([]Document{document}, testNow, live); err == nil ||
		!strings.Contains(err.Error(), "agents[id=searcher].lease_ttl: 6h0m0s outlasts parent agents[id=planner] (live)") {
		t.Fatalf("child outlasting a live parent = %v", err)
	}
	agentByID(&document, "searcher").LeaseTTL = "2h"
	if _, err := Compile([]Document{document}, testNow, live); err != nil {
		t.Fatalf("child of a live parent: %v", err)
	}
}

func TestCycleAndDuplicateRefusals(t *testing.T) {
	document := base(t)
	agentByID(&document, "planner").Parent = "searcher"
	if _, err := Compile([]Document{document}, testNow, nil); err == nil ||
		!strings.Contains(err.Error(), "delegation forms a cycle") {
		t.Fatalf("cycle = %v", err)
	}
	self := base(t)
	agentByID(&self, "planner").Parent = "planner"
	if _, err := Compile([]Document{self}, testNow, nil); err == nil ||
		!strings.Contains(err.Error(), "agents[id=planner].parent: an agent cannot delegate from itself") {
		t.Fatalf("self parent = %v", err)
	}
	first := base(t).WithName("a.atpl.json")
	second := base(t).WithName("b.atpl.json")
	second.Executors = nil
	if _, err := Compile([]Document{first, second}, testNow, nil); err == nil ||
		!strings.Contains(err.Error(), "b.atpl.json: agents[id=planner]: agent is also declared in a.atpl.json") {
		t.Fatalf("duplicate agent = %v", err)
	}
	second = base(t).WithName("b.atpl.json")
	second.Agents = nil
	if _, err := Compile([]Document{first, second}, testNow, nil); err == nil ||
		!strings.Contains(err.Error(), "b.atpl.json: executors[ref=search-exec]: executor is also declared in a.atpl.json") {
		t.Fatalf("duplicate executor = %v", err)
	}
	floor := base(t)
	floor.Executors[0].MaxEffects = nil
	if _, err := Compile([]Document{floor}, testNow, nil); err == nil ||
		!strings.Contains(err.Error(), "executors[ref=search-exec].min_effects: reads-corpus is not within max_effects") {
		t.Fatalf("floor above ceiling = %v", err)
	}
}

func TestDecodeStrictness(t *testing.T) {
	replaceIn := func(old, replacement string) string {
		if !strings.Contains(basePolicy, old) {
			t.Fatalf("fixture lacks %q", old)
		}
		return strings.Replace(basePolicy, old, replacement, 1)
	}
	plannerField := func(field string) string {
		return replaceIn(`"id": "planner",`, `"id": "planner", "`+field+`": {},`)
	}
	actionField := func(field string) string {
		return replaceIn(`{"name": "query", "effects"`, `{"name": "query", "`+field+`": 1, "effects"`)
	}
	cases := []struct {
		name string
		text string
		want string
	}{
		{"unknown field", plannerField("color"), "agents[id=planner].color: unknown field"},
		{"approval", plannerField("approval"), "agents[id=planner].approval: is declared per action only"},
		{"obligations", plannerField("obligations"), "agents[id=planner].obligations: is not declared in policy: admission obligations are computed per request"},
		{"attestation", plannerField("attestation"), "agents[id=planner].attestation: is declared per action only"},
		{"runtime", plannerField("runtime"), "agents[id=planner].runtime: requires a later ATPL version"},
		{"trust_score", plannerField("trust_score"), "agents[id=planner].trust_score: is not part of ATPL in Shoal"},
		{"behavior", plannerField("behavior"), "agents[id=planner].behavior: is not part of ATPL in Shoal"},
		{"reserved on an action", actionField("runtime"),
			"agents[id=planner].capabilities[name=search].actions[name=query].runtime: requires a later ATPL version (runtime attestation, #446)"},
		{"attestation on an action is an object", actionField("attestation"),
			"agents[id=planner].capabilities[name=search].actions[name=query].attestation: must be a JSON object"},
		{"approval on an action is an object", actionField("approval"),
			"agents[id=planner].capabilities[name=search].actions[name=query].approval: must be a JSON object"},
		{"reserved at the top", replaceIn(`"executors": [`, `"trust_score": 0.9, "executors": [`),
			"base.atpl.json: trust_score: is not part of ATPL in Shoal"},
		{"duplicate key", replaceIn(`"id": "planner",`, `"id": "planner", "id": "planner",`),
			`duplicate key "id" in agents[0]`},
		{"duplicate key in a schema", replaceIn(`{"type": "object", "properties"`, `{"type": "object", "type": "string", "properties"`),
			`duplicate key "type" in agents[0].capabilities[1].actions[0].input_schema`},
		{"trailing data", basePolicy + ` {}`, "trailing data"},
		{"not an object", `[]`, "must be one JSON object"},
		{"truncated", basePolicy[:len(basePolicy)-1], "truncated"},
		{"malformed", `{"atpl": }`, "not valid JSON"},
		{"wrong version", replaceIn(`"shoal.atpl/v1"`, `"shoal.atpl/v2"`), `atpl: unsupported format version "shoal.atpl/v2"`},
		{"version gate precedes fields", replaceIn(`"shoal.atpl/v1",`, `"shoal.atpl/v2", "approval": {},`), "unsupported format version"},
		{"missing origin", replaceIn(`"origin": "Derived from the Agent Trust Policy Language, github.com/SentriusLLC/atpl (Apache-2.0)",`, ``), "origin: must credit"},
		{"altered origin", replaceIn(`(Apache-2.0)`, `(MIT)`), "origin: must credit"},
		{"null value", replaceIn(`"lease_ttl": "12h"`, `"lease_ttl": null`), "agents[id=planner].lease_ttl: must be a string"},
		{"missing field", replaceIn(`"lease_ttl": "12h",`, ``), "agents[id=planner].lease_ttl: is required"},
		{"inherit with a declaration", replaceIn(`{"name": "query", "inherit": true}`,
			`{"name": "query", "inherit": true, "effects": []}`),
			"agents[id=searcher].capabilities[name=search].actions[name=query].effects: must be omitted when inherit is true"},
		{"schema not an object", replaceIn(`"input_schema": {"type": "object"}, "output_schema": {"type": "object"}}`,
			`"input_schema": "object", "output_schema": {"type": "object"}}`),
			"actions[name=query].input_schema: must be a JSON object"},
		{"invalid UTF-8", strings.Replace(basePolicy, "planner", "plan\xffner", 1), "not valid UTF-8"},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			_, err := Decode("base.atpl.json", strings.NewReader(test.text))
			if !shoal.IsErrorCode(err, shoal.ErrorInvalidArgument) {
				t.Fatalf("decode = %v, want invalid argument", err)
			}
			if !strings.Contains(err.Error(), test.want) {
				t.Fatalf("refusal %q does not contain %q", err, test.want)
			}
		})
	}
}

func TestDecodeBounds(t *testing.T) {
	padded := basePolicy + strings.Repeat(" ", MaxFileBytes-len(basePolicy))
	if _, err := Decode("max.atpl.json", strings.NewReader(padded)); err != nil {
		t.Fatalf("file at the bound: %v", err)
	}
	if _, err := Decode("over.atpl.json", strings.NewReader(padded+" ")); err == nil ||
		!strings.Contains(err.Error(), fmt.Sprintf("exceeds %d bytes", MaxFileBytes)) {
		t.Fatalf("file over the bound = %v", err)
	}

	scopes := func(count int) string {
		var builder strings.Builder
		for i := 0; i < count; i++ {
			if i > 0 {
				builder.WriteString(",")
			}
			fmt.Fprintf(&builder, `{"source_id":"source-%d","policy_id":"policy"}`, i)
		}
		return strings.Replace(basePolicy, `"scopes": [{"source_id": "source-a", "policy_id": "policy"}]`,
			`"scopes": [`+builder.String()+`]`, 1)
	}
	if _, err := Decode("scopes.atpl.json", strings.NewReader(scopes(fleet.MaxScopes))); err != nil {
		t.Fatalf("scopes at the bound: %v", err)
	}
	if _, err := Decode("scopes.atpl.json", strings.NewReader(scopes(fleet.MaxScopes+1))); err == nil ||
		!strings.Contains(err.Error(), "agents[id=searcher].scopes: declares more than 64 scopes") {
		t.Fatalf("scopes over the bound = %v", err)
	}

	capabilities := func(count int) Document {
		document := base(t)
		agent := agentByID(&document, "planner")
		agent.Capabilities = nil
		for i := 0; i < count; i++ {
			agent.Capabilities = append(agent.Capabilities, Capability{
				Name: fmt.Sprintf("capability-%02d", i),
				Actions: []Action{{Name: "act", InputSchema: json.RawMessage(`{}`),
					OutputSchema: json.RawMessage(`{}`),
					// planner binds to remote-exec, whose ceiling permits an
					// external class, so an action must declare something.
					// This test is about capability counts; declare the
					// narrowest class that is in bounds.
					Effects: []string{string(fleet.EffectReadsCorpus)}}},
			})
		}
		document.Agents = document.Agents[:1]
		return document
	}
	if _, err := Compile([]Document{capabilities(fleet.MaxCapabilities)}, testNow, nil); err != nil {
		t.Fatalf("capabilities at the bound: %v", err)
	}
	if _, err := Compile([]Document{capabilities(fleet.MaxCapabilities + 1)}, testNow, nil); err == nil ||
		!strings.Contains(err.Error(), "agents[id=planner].capabilities: must declare between 1 and 64 capabilities") {
		t.Fatalf("capabilities over the bound = %v", err)
	}

	files := func(count int) []Document {
		result := []Document{base(t)}
		for i := 1; i < count; i++ {
			result = append(result, Document{ATPL: Version, Origin: Origin}.WithName(fmt.Sprintf("empty-%03d.atpl.json", i)))
		}
		return result
	}
	if _, err := Compile(files(MaxFiles), testNow, nil); err != nil {
		t.Fatalf("files at the bound: %v", err)
	}
	if _, err := Compile(files(MaxFiles+1), testNow, nil); err == nil ||
		!strings.Contains(err.Error(), "policy spans more than 4097 files") {
		t.Fatalf("files over the bound = %v", err)
	}
}

func kinds(plan Plan) map[shoal.ID]Kind {
	result := make(map[shoal.ID]Kind, len(plan.Entries))
	for _, entry := range plan.Entries {
		result[entry.ID] = entry.Kind
	}
	return result
}

func compileLive(t *testing.T, document Document, registry *atpltest.Registry) (*Policy, map[shoal.ID]fleet.Descriptor) {
	t.Helper()
	live := registry.Live(t)
	policy, err := Compile([]Document{document}, registry.Clock.Now(), LiveParents(live))
	if err != nil {
		t.Fatalf("compile against live: %v", err)
	}
	return policy, live
}

func TestPlanKinds(t *testing.T) {
	document := base(t)
	registry := newRegistry(t, document)
	policy, live := compileLive(t, document, registry)
	plan := Diff(policy, live, "")
	if got := kinds(plan); got["planner"] != KindCreate || got["searcher"] != KindCreate {
		t.Fatalf("empty registry kinds = %v", got)
	}
	applyDirect(t, registry, plan)
	legacy := compileOne(t, base(t)).Agents()[0]
	legacy.ID = "legacy"
	if _, err := registry.Register(t, legacy, 0, "key-legacy"); err != nil {
		t.Fatal(err)
	}
	registry.Clock.Set(testNow.Add(time.Minute))

	policy, live = compileLive(t, document, registry)
	unchanged := Diff(policy, live, "")
	if got := kinds(unchanged); got["planner"] != KindUnchanged || got["searcher"] != KindUnchanged ||
		got["legacy"] != KindUnmanaged {
		t.Fatalf("unchanged kinds = %v", got)
	}
	if len(unchanged.Writes()) != 0 || unchanged.Entries[2].LiveGeneration != 1 {
		t.Fatalf("unchanged plan = %+v", unchanged.Entries)
	}

	narrowed := base(t)
	planner := agentByID(&narrowed, "planner")
	planner.Capabilities = planner.Capabilities[:1]
	planner.Scopes = planner.Scopes[:1]
	reviewer := *agentByID(&narrowed, "searcher")
	reviewer.ID = "reviewer"
	narrowed.Agents = append(narrowed.Agents, reviewer)
	policy, live = compileLive(t, narrowed, registry)
	plan = Diff(policy, live, "")
	if got := kinds(plan); got["planner"] != KindNarrow || got["searcher"] != KindUnchanged ||
		got["reviewer"] != KindCreate {
		t.Fatalf("narrow kinds = %v", got)
	}
	var plannerEntry Entry
	for _, entry := range plan.Entries {
		if entry.ID == "planner" {
			plannerEntry = entry
		}
	}
	wantChanges := []Change{
		{Op: "-", Path: "scopes[source_id=source-b,policy_id=policy]"},
		{Op: "-", Path: "capabilities[name=tickets]"},
	}
	if !reflect.DeepEqual(plannerEntry.Changes, wantChanges) {
		t.Fatalf("narrow changes = %+v", plannerEntry.Changes)
	}
	if plan.Digest == unchanged.Digest || !strings.HasPrefix(plan.Digest, PlanDigestPrefix) {
		t.Fatal("plan digest does not distinguish plans")
	}
	if again := Diff(policy, registry.Live(t), ""); again.Digest != plan.Digest {
		t.Fatal("plan digest is not reproducible")
	}

	widened := base(t)
	agentByID(&widened, "planner").Scopes = append(agentByID(&widened, "planner").Scopes,
		Scope{SourceID: "source-c", PolicyID: "policy"})
	policy, live = compileLive(t, widened, registry)
	plan = Diff(policy, live, "")
	if got := kinds(plan); got["planner"] != KindRefusedWidening {
		t.Fatalf("widening kinds = %v", got)
	}
	if len(plan.Refusals()) != 1 || plan.Refusals()[0].Changes[0].Path != "scopes[source_id=source-c,policy_id=policy]" {
		t.Fatalf("widening refusal = %+v", plan.Refusals())
	}

	migrated := base(t)
	agentByID(&migrated, "searcher").Parent = ""
	agentByID(&migrated, "searcher").Capabilities[0].Actions[0] = Action{Name: "query",
		Effects: []string{"reads-corpus"}, InputSchema: json.RawMessage(`{"type":"object"}`),
		OutputSchema: json.RawMessage(`{"type":"object"}`)}
	policy, live = compileLive(t, migrated, registry)
	if got := kinds(Diff(policy, live, "")); got["searcher"] != KindRefusedParentMigration {
		t.Fatalf("migration kinds = %v", got)
	}

	// A parent rewritten with a shorter lease would hide a live child the
	// policy does not manage.
	shortened := base(t)
	shortened.Agents = shortened.Agents[:1]
	agentByID(&shortened, "planner").LeaseTTL = "1h"
	agentByID(&shortened, "planner").Capabilities = agentByID(&shortened, "planner").Capabilities[:1]
	policy, live = compileLive(t, shortened, registry)
	plan = Diff(policy, live, "")
	if got := kinds(plan); got["planner"] != KindRefusedDelegation || got["searcher"] != KindUnmanaged {
		t.Fatalf("delegation kinds = %v", got)
	}
	if !strings.Contains(plan.Refusals()[0].Reason, "live child agents[id=searcher]") {
		t.Fatalf("delegation reason = %q", plan.Refusals()[0].Reason)
	}
}

func TestExportRefusesNonUTF8AndDerivesExecutors(t *testing.T) {
	descriptor := fleet.Descriptor{
		ID: "agent", Generation: 1, AuthorizationDomain: []byte("domain"),
		Scopes:      []fleet.Scope{{SourceID: []byte("source\xff"), PolicyID: []byte("policy")}},
		ExecutorRef: "exec",
		Capabilities: []fleet.Capability{{Name: "c", Actions: []fleet.Action{{
			Name: "a", Effects: fleet.Effects{fleet.EffectReadsCorpus, fleet.EffectMutatesExternal},
			InputSchema: json.RawMessage(`{}`), OutputSchema: json.RawMessage(`{}`),
		}}}},
		LeaseExpiresAt: testNow.Add(time.Hour), UpdatedAt: testNow,
	}
	if _, err := Export(map[shoal.ID]fleet.Descriptor{"agent": descriptor}, nil, testNow); err == nil ||
		!strings.Contains(err.Error(), "agents[id=agent].scopes[0].source_id: is not valid UTF-8") {
		t.Fatalf("non-UTF-8 scope = %v", err)
	}
	descriptor.Scopes[0].SourceID = []byte("source")
	descriptor.ID = "agent\xfe"
	if _, err := Export(map[shoal.ID]fleet.Descriptor{descriptor.ID: descriptor}, nil, testNow); err == nil ||
		!strings.Contains(err.Error(), "agents[id(base64url)=YWdlbnT-].id: is not valid UTF-8") {
		t.Fatalf("non-UTF-8 ID = %v", err)
	}
	descriptor.ID = "agent"
	document, err := Export(map[shoal.ID]fleet.Descriptor{"agent": descriptor}, nil, testNow)
	if err != nil {
		t.Fatal(err)
	}
	if want := []Executor{{Ref: "exec", MaxEffects: []string{"external", "reads-corpus"}, MinEffects: []string{}}}; !reflect.DeepEqual(document.Executors, want) {
		t.Fatalf("derived executors = %+v", document.Executors)
	}
	if document.Agents[0].LeaseTTL != "1h0m0s" || document.Origin != Origin {
		t.Fatalf("exported agent = %+v", document)
	}
	if _, err := Export(map[shoal.ID]fleet.Descriptor{"agent": descriptor},
		[]Executor{{Ref: "other"}}, testNow); err == nil || !strings.Contains(err.Error(), "executor exec") {
		t.Fatalf("manifest missing a referenced executor = %v", err)
	}
}

func TestTheFormatCreditsATPL(t *testing.T) {
	if !strings.Contains(Origin, "github.com/SentriusLLC/atpl") || !strings.Contains(Origin, "Apache-2.0") {
		t.Fatalf("origin credit = %q", Origin)
	}
	source, err := os.ReadFile("doc.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"Agent Trust Policy Language", "github.com/SentriusLLC/atpl",
		"Apache-2.0", "trust_score", "behavior", "docs/gateways.md"} {
		if !bytes.Contains(source, []byte(want)) {
			t.Fatalf("package documentation does not mention %q", want)
		}
	}
	document, err := Export(nil, nil, testNow)
	if err != nil {
		t.Fatal(err)
	}
	encoded, _ := json.Marshal(document)
	if !bytes.Contains(encoded, []byte(`"origin":"Derived from the Agent Trust Policy Language`)) {
		t.Fatalf("export omits the credit: %s", encoded)
	}
}
