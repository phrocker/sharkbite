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
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/phrocker/shoal-oss/pkg/shoal"
)

// ATPL's "attestation": {"required": true} (#446), held to exactly approval's
// strictness and compared against Register throughout.

const attestationRequired = `"attestation": {"required": true}`

func replaceOnce(t *testing.T, text, old, replacement string) string {
	t.Helper()
	if !strings.Contains(text, old) {
		t.Fatalf("fixture lacks %q", old)
	}
	return strings.Replace(text, old, replacement, 1)
}

// withAttestation is basePolicy with planner's tickets.open requiring
// attestation.
func withAttestation(t *testing.T) string {
	return replaceOnce(t, basePolicy, `{"name": "open", "effects": ["external"],`,
		`{"name": "open", `+attestationRequired+`, "effects": ["external"],`)
}

// withOpener adds a delegate of planner, on planner's own executor, that
// holds tickets.open — the shape a delegation of an attested action takes.
func withOpener(t *testing.T, text, openAction string) string {
	return replaceOnce(t, text, "\n  ]\n}", `,
    {
      "id": "opener",
      "parent": "planner",
      "authorization_domain": "domain",
      "scopes": [{"source_id": "source-a", "policy_id": "policy"}],
      "executor_ref": "remote-exec",
      "lease_ttl": "6h",
      "capabilities": [{"name": "tickets", "actions": [`+openAction+`]}]
    }
  ]
}`)
}

const explicitOpen = `{"name": "open", "effects": ["external"],
 "input_schema": {"type": "object", "properties": {"title": {"type": "string"}}},
 "output_schema": {"type": "object"}`

func TestAttestationDecodesWithApprovalsStrictness(t *testing.T) {
	document := decodeString(t, "base.atpl.json", withAttestation(t))
	open := agentByID(&document, "planner").Capabilities[1].Actions[0]
	if open.Attestation == nil || !open.Attestation.Required {
		t.Fatalf("attestation not decoded: %+v", open)
	}
	action := func(value string) string {
		return replaceOnce(t, basePolicy, `{"name": "open", "effects"`, `{"name": "open", "attestation": `+value+`, "effects"`)
	}
	const openPath = "agents[id=planner].capabilities[name=tickets].actions[name=open]"
	cases := []struct{ name, text, want string }{
		{"top level", replaceOnce(t, basePolicy, `"executors": [`, attestationRequired+`, "executors": [`),
			"base.atpl.json: attestation: is declared per action only"},
		{"executor", replaceOnce(t, basePolicy, `{"ref": "search-exec",`, `{"ref": "search-exec", `+attestationRequired+`,`),
			"executors[ref=search-exec].attestation: is declared per action only"},
		{"agent", replaceOnce(t, basePolicy, `"id": "planner",`, `"id": "planner", `+attestationRequired+`,`),
			"agents[id=planner].attestation: is declared per action only"},
		{"capability", replaceOnce(t, basePolicy, `{"name": "tickets", "actions"`, `{"name": "tickets", `+attestationRequired+`, "actions"`),
			"agents[id=planner].capabilities[name=tickets].attestation: is declared per action only"},
		{"explicit false", action(`{"required": false}`), openPath + ".attestation.required: must be true; omit attestation"},
		{"empty object", action(`{}`), openPath + ".attestation.required: is required"},
		{"null", action(`null`), openPath + ".attestation: must be a JSON object"},
		{"bare boolean", action(`true`), openPath + ".attestation: must be a JSON object"},
		{"string boolean", action(`{"required": "true"}`), openPath + ".attestation.required: must be a boolean"},
		{"null required", action(`{"required": null}`), openPath + ".attestation.required: must be a boolean"},
		{"extra key", action(`{"required": true, "verifier": "k"}`), openPath + ".attestation.verifier: unknown field"},
		{"case-folded key", action(`{"Required": true}`), openPath + ".attestation.Required: unknown field"},
		{"case-folded field", replaceOnce(t, basePolicy, `{"name": "open", "effects"`, `{"name": "open", "Attestation": {"required": true}, "effects"`),
			openPath + ".Attestation: unknown field"},
		{"duplicate required", action(`{"required": true, "required": true}`), `duplicate key "required"`},
		{"duplicate attestation", replaceOnce(t, basePolicy, `{"name": "open", "effects"`,
			`{"name": "open", `+attestationRequired+`, `+attestationRequired+`, "effects"`), `duplicate key "attestation"`},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			_, err := Decode("base.atpl.json", strings.NewReader(test.text))
			if !shoal.IsErrorCode(err, shoal.ErrorInvalidArgument) || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("decode = %v, want %q", err, test.want)
			}
		})
	}
}

func TestAttestationCompilesAndIsRefusedWithoutExternal(t *testing.T) {
	policy := compileOne(t, decodeString(t, "base.atpl.json", withAttestation(t)))
	if !specAction(t, policy, "planner", "tickets", "open").RequiresAttestation ||
		specAction(t, policy, "planner", "search", "query").RequiresAttestation {
		t.Fatal("attestation compiled onto the wrong actions")
	}
	// On an action without the external effect: refused at the field, and
	// Register refuses the same spec.
	text := replaceOnce(t, basePolicy, `{"name": "query", "effects": ["reads-corpus"],`,
		`{"name": "query", `+attestationRequired+`, "effects": ["reads-corpus"],`)
	document := decodeString(t, "base.atpl.json", text)
	_, err := Compile([]Document{document}, testNow, nil)
	if err == nil || !strings.Contains(err.Error(),
		"agents[id=planner].capabilities[name=search].actions[name=query].attestation: requires the action to declare the external effect") {
		t.Fatalf("compile = %v", err)
	}
	registry := newRegistry(t, document)
	if _, err := registry.Register(t, rawSpec(*agentByID(&document, "planner"), testNow, nil), 0, "key"); err == nil {
		t.Fatal("Register accepted attestation without the external effect")
	}
	// A code-built required:false is refused too.
	built := base(t)
	agentByID(&built, "planner").Capabilities[1].Actions[0].Attestation = &Attestation{Required: false}
	if _, err := Compile([]Document{built}, testNow, nil); err == nil ||
		!strings.Contains(err.Error(), ".attestation.required: must be true") {
		t.Fatalf("code-built required:false = %v", err)
	}
}

func TestDelegationCannotDropAttestationAndRegisterAgrees(t *testing.T) {
	cases := []struct {
		name    string
		parent  bool
		child   string
		refusal string
	}{
		{"inherit carries it", true, `{"name": "open", "inherit": true}`, ""},
		{"inherit may add it", false, `{"name": "open", "inherit": true, ` + attestationRequired + `}`, ""},
		{"an explicit child drops it", true, explicitOpen + `}`,
			"agents[id=opener].capabilities[name=tickets].actions[name=open].attestation: is required by parent agents[id=planner]"},
		{"an explicit child keeps it", true, explicitOpen + `, ` + attestationRequired + `}`, ""},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			text := basePolicy
			if test.parent {
				text = withAttestation(t)
			}
			document := decodeString(t, "base.atpl.json", withOpener(t, text, test.child))
			policy, err := Compile([]Document{document}, testNow, nil)
			switch {
			case test.refusal == "" && err != nil:
				t.Fatalf("compile = %v", err)
			case test.refusal != "" && (err == nil || !strings.Contains(err.Error(), test.refusal)):
				t.Fatalf("compile = %v, want %q", err, test.refusal)
			}
			if err == nil && !specAction(t, policy, "opener", "tickets", "open").RequiresAttestation {
				t.Fatal("the delegate does not require attestation")
			}
			// Parity with Register.
			registry := newRegistry(t, document)
			parentDocument := document
			parentDocument.Agents = []Agent{*agentByID(&document, "planner")}
			planner := compileOne(t, parentDocument).Agents()[0]
			if _, err := registry.Register(t, planner, 0, "key-planner"); err != nil {
				t.Fatal(err)
			}
			spec := rawSpec(*agentByID(&document, "opener"), testNow, &planner)
			if _, err := registry.Register(t, spec, 0, "key-opener"); (err == nil) != (test.refusal == "") {
				t.Fatalf("Register = %v, compile refusal %q", err, test.refusal)
			}
		})
	}
}

func TestDigestsChangeOnlyForAttestationRequiredActions(t *testing.T) {
	policy := compileOne(t, base(t))
	if policy.Digest() != goldenBasePolicyDigest || strings.Contains(string(policy.CanonicalJSON()), "attestation") {
		t.Fatalf("a policy without attestation changed: %s", policy.Digest())
	}
	attested := compileOne(t, decodeString(t, "base.atpl.json", withAttestation(t)))
	canonical := string(attested.CanonicalJSON())
	if strings.Count(canonical, `"attestation":{"required":true}`) != 1 {
		t.Fatalf("canonical JSON does not carry exactly one requirement:\n%s", canonical)
	}
	if strings.Replace(canonical, `,"attestation":{"required":true}`, ``, 1) != string(policy.CanonicalJSON()) {
		t.Fatal("attestation changed more than its own action")
	}
	registry := newRegistry(t, base(t))
	applyDirect(t, registry, Diff(policy, registry.Live(t), ""))
	live := registry.Live(t)
	if ContentDigest(live["planner"]) != goldenPlannerContent {
		t.Fatal("content digest moved without attestation")
	}
	flagged := live["planner"]
	flagged.Capabilities = cloneCapabilities(flagged.Capabilities)
	flagged.Capabilities[1].Actions[0].RequiresAttestation = true
	if ContentDigest(flagged) == goldenPlannerContent {
		t.Fatal("content digest ignores attestation")
	}
}

func TestPlanAttestationNarrowsAndDroppingItIsRefused(t *testing.T) {
	document := base(t)
	registry := newRegistry(t, document)
	policy, live := compileLive(t, document, registry)
	applyDirect(t, registry, Diff(policy, live, ""))
	registry.Clock.Set(testNow.Add(time.Minute))

	attested := decodeString(t, "base.atpl.json", withAttestation(t))
	policy, live = compileLive(t, attested, registry)
	plan := Diff(policy, live, "")
	want := Change{Op: "+", Path: "capabilities[name=tickets].actions[name=open].attestation", Detail: "required"}
	for _, entry := range plan.Entries {
		if entry.ID != "planner" {
			continue
		}
		if entry.Kind != KindNarrow || !reflect.DeepEqual(entry.Changes, []Change{want}) {
			t.Fatalf("planner = %s %+v", entry.Kind, entry.Changes)
		}
	}
	applyDirect(t, registry, plan)
	live = registry.Live(t)
	if !findAction(findCapability(live["planner"].Capabilities, "tickets"), "open").RequiresAttestation {
		t.Fatal("apply did not register the requirement")
	}
	registry.Clock.Set(testNow.Add(2 * time.Minute))
	policy, live = compileLive(t, document, registry)
	plan = Diff(policy, live, "")
	for _, entry := range plan.Entries {
		if entry.ID != "planner" {
			continue
		}
		if entry.Kind != KindRefusedWidening || len(entry.Changes) != 1 ||
			entry.Changes[0].Path != "capabilities[name=tickets].actions[name=open].attestation" ||
			entry.Changes[0].Op != "-" {
			t.Fatalf("dropping = %s %+v", entry.Kind, entry.Changes)
		}
	}
	for _, spec := range policy.Agents() {
		if spec.ID == "planner" {
			if _, err := registry.Register(t, spec, live["planner"].Generation, "key-drop"); err == nil {
				t.Fatal("Register accepted a write that drops attestation")
			}
		}
	}
	// Creation shows it.
	fresh := newRegistry(t, attested)
	for _, entry := range Diff(compileOne(t, attested), fresh.Live(t), "").Entries {
		found := false
		for _, change := range entry.Changes {
			found = found || reflect.DeepEqual(change, want)
		}
		if found != (entry.ID == "planner") {
			t.Fatalf("%s creation changes %+v", entry.ID, entry.Changes)
		}
	}
}

func TestPlanRefusesAddingAttestationOverALiveChildItDoesNotRewrite(t *testing.T) {
	document := decodeString(t, "base.atpl.json", withOpener(t, basePolicy, `{"name": "open", "inherit": true}`))
	registry := newRegistry(t, document)
	policy, live := compileLive(t, document, registry)
	applyDirect(t, registry, Diff(policy, live, ""))
	registry.Clock.Set(testNow.Add(time.Minute))

	attested := decodeString(t, "base.atpl.json", withAttestation(t))
	attested.Agents = attested.Agents[:1]
	policy, live = compileLive(t, attested, registry)
	plan := Diff(policy, live, "")
	got := kinds(plan)
	if got["planner"] != KindRefusedDelegation || got["opener"] != KindUnmanaged {
		t.Fatalf("kinds = %v", got)
	}
}

func TestExportWritesAttestationAndRoundTrips(t *testing.T) {
	document := decodeString(t, "base.atpl.json", withAttestation(t))
	policy := compileOne(t, document)
	registry := newRegistry(t, document)
	applyDirect(t, registry, Diff(policy, registry.Live(t), ""))
	live := registry.Live(t)
	exported, err := Export(live, document.Executors, testNow)
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := Encode(exported)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(string(encoded), `"attestation": {`) != 1 {
		t.Fatalf("export:\n%s", encoded)
	}
	reread := decodeString(t, "exported.atpl.json", string(encoded))
	recompiled, err := Compile([]Document{reread}, testNow, nil)
	if err != nil {
		t.Fatal(err)
	}
	if recompiled.Digest() != policy.Digest() {
		t.Fatalf("digest changed across round trip")
	}
	if plan := Diff(recompiled, live, ""); len(plan.Writes()) != 0 || len(plan.Refusals()) != 0 {
		t.Fatalf("round trip does not plan as unchanged: %+v", plan.Entries)
	}
}
