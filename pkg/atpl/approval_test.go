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
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/phrocker/shoal-oss/pkg/explorer/fleet"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

// Digests of basePolicy as main computed them before approval could be
// declared. A policy without approval must keep them exactly: they are what
// apply records as the policy source, and what reviewed plans bind.
const (
	goldenBasePolicyDigest = "atpl:policy:v1:cac7dcc79b5d31eb50de3c2152bed9f4cdee2ab43fce3262104e357f7eeffa09"
	goldenPlannerContent   = "atpl:live:v1:6a8230199ac31e4e9bc6052dcf74233233e3a7f42db5f9c3588f6ba19da94168"
	goldenSearcherContent  = "atpl:live:v1:2f150252bcb16eebe1d0e4fb1df54bcdfbe8bb67b53e4885d867a0a807a04257"
	// The unchanged plan of basePolicy against its own applied fleet, at
	// registry "https://registry.example".
	goldenUnchangedPlan = "atpl:plan:v2:bdcdb6b8d4120a4e858917193ab24f9edc06845676edfefe70761f9e412cd689"
)

const approvalRequired = `"approval": {"required": true}`

// withApproval is basePolicy with planner's tickets.open requiring approval.
func withApproval(t *testing.T) string {
	t.Helper()
	old := `{"name": "open", "effects": ["external"],`
	if !strings.Contains(basePolicy, old) {
		t.Fatalf("fixture lacks %q", old)
	}
	return strings.Replace(basePolicy, old, `{"name": "open", `+approvalRequired+`, "effects": ["external"],`, 1)
}

// queryWithApproval is basePolicy with planner's search.query requiring
// approval, which searcher inherits.
func queryWithApproval(t *testing.T) string {
	t.Helper()
	old := `{"name": "query", "effects": ["reads-corpus"],`
	if !strings.Contains(basePolicy, old) {
		t.Fatalf("fixture lacks %q", old)
	}
	return strings.Replace(basePolicy, old, `{"name": "query", `+approvalRequired+`, "effects": ["reads-corpus"],`, 1)
}

func specAction(t *testing.T, policy *Policy, id shoal.ID, capability, action string) fleet.Action {
	t.Helper()
	for _, spec := range policy.Agents() {
		if spec.ID != id {
			continue
		}
		if found := findAction(findCapability(spec.Capabilities, capability), action); found != nil {
			return *found
		}
	}
	t.Fatalf("%s has no %s.%s", id, capability, action)
	return fleet.Action{}
}

func TestApprovalDecodesOnlyOnAnAction(t *testing.T) {
	document := decodeString(t, "base.atpl.json", withApproval(t))
	open := agentByID(&document, "planner").Capabilities[1].Actions[0]
	if open.Approval == nil || !open.Approval.Required {
		t.Fatalf("approval not decoded: %+v", open)
	}

	replaceIn := func(text, old, replacement string) string {
		if !strings.Contains(text, old) {
			t.Fatalf("fixture lacks %q", old)
		}
		return strings.Replace(text, old, replacement, 1)
	}
	action := func(value string) string {
		return replaceIn(basePolicy, `{"name": "open", "effects"`, `{"name": "open", "approval": `+value+`, "effects"`)
	}
	const openPath = "agents[id=planner].capabilities[name=tickets].actions[name=open]"
	cases := []struct {
		name string
		text string
		want string
	}{
		{"top level", replaceIn(basePolicy, `"executors": [`, approvalRequired+`, "executors": [`),
			"base.atpl.json: approval: is declared per action only"},
		{"executor", replaceIn(basePolicy, `{"ref": "search-exec",`, `{"ref": "search-exec", `+approvalRequired+`,`),
			"executors[ref=search-exec].approval: is declared per action only"},
		{"agent", replaceIn(basePolicy, `"id": "planner",`, `"id": "planner", `+approvalRequired+`,`),
			"agents[id=planner].approval: is declared per action only"},
		{"scope", replaceIn(basePolicy, `{"source_id": "source-a", "policy_id": "policy"},`,
			`{"source_id": "source-a", "policy_id": "policy", `+approvalRequired+`},`),
			"agents[id=planner].scopes[0].approval: is declared per action only"},
		{"capability", replaceIn(basePolicy, `{"name": "tickets", "actions"`, `{"name": "tickets", `+approvalRequired+`, "actions"`),
			"agents[id=planner].capabilities[name=tickets].approval: is declared per action only"},
		{"explicit false", action(`{"required": false}`), openPath + ".approval.required: must be true; omit approval"},
		{"empty object", action(`{}`), openPath + ".approval.required: is required"},
		{"null", action(`null`), openPath + ".approval: must be a JSON object"},
		{"bare boolean", action(`true`), openPath + ".approval: must be a JSON object"},
		{"string boolean", action(`{"required": "true"}`), openPath + ".approval.required: must be a boolean"},
		{"null required", action(`{"required": null}`), openPath + ".approval.required: must be a boolean"},
		{"extra key", action(`{"required": true, "quorum": 2}`), openPath + ".approval.quorum: unknown field"},
		{"reserved key inside", action(`{"required": true, "runtime": {}}`),
			openPath + ".approval.runtime: requires a later ATPL version"},
		{"attestation inside approval", action(`{"required": true, "attestation": {}}`),
			openPath + ".approval.attestation: is declared per action only"},
		{"case-folded key", action(`{"Required": true}`), openPath + ".approval.Required: unknown field"},
		{"case-folded field", replaceIn(basePolicy, `{"name": "open", "effects"`, `{"name": "open", "Approval": {"required": true}, "effects"`),
			openPath + ".Approval: unknown field"},
		{"duplicate required", action(`{"required": true, "required": true}`), `duplicate key "required"`},
		{"duplicate approval", replaceIn(basePolicy, `{"name": "open", "effects"`,
			`{"name": "open", `+approvalRequired+`, `+approvalRequired+`, "effects"`), `duplicate key "approval"`},
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

	// Inherit may carry approval, to add the requirement; nothing else.
	inherited := replaceIn(basePolicy, `{"name": "query", "inherit": true}`,
		`{"name": "query", "inherit": true, `+approvalRequired+`}`)
	document = decodeString(t, "base.atpl.json", inherited)
	if query := agentByID(&document, "searcher").Capabilities[0].Actions[0]; !query.Inherit ||
		query.Approval == nil {
		t.Fatalf("inherit with approval: %+v", query)
	}
}

func TestApprovalCompilesIntoTheRegistration(t *testing.T) {
	policy := compileOne(t, decodeString(t, "base.atpl.json", withApproval(t)))
	if !specAction(t, policy, "planner", "tickets", "open").RequiresApproval {
		t.Fatal("tickets.open compiled without its approval requirement")
	}
	if specAction(t, policy, "planner", "search", "query").RequiresApproval ||
		specAction(t, policy, "searcher", "search", "query").RequiresApproval {
		t.Fatal("approval leaked onto an action that does not declare it")
	}
	// Writes hand apply the same flag Agents does.
	registry := newRegistry(t, base(t))
	for _, write := range Diff(policy, registry.Live(t), "").Writes() {
		if write.ID == "planner" &&
			!findAction(findCapability(write.Spec.Capabilities, "tickets"), "open").RequiresApproval {
			t.Fatal("plan writes drop the approval requirement")
		}
	}

	// A document built in code is held to Decode's rule.
	document := base(t)
	agentByID(&document, "planner").Capabilities[1].Actions[0].Approval = &Approval{Required: false}
	if _, err := Compile([]Document{document}, testNow, nil); err == nil || !strings.Contains(err.Error(),
		"agents[id=planner].capabilities[name=tickets].actions[name=open].approval.required: must be true") {
		t.Fatalf("code-built required:false = %v", err)
	}
}

func TestInheritCarriesApprovalAndDelegationCannotDropIt(t *testing.T) {
	// inherit copies the parent's requirement.
	policy := compileOne(t, decodeString(t, "base.atpl.json", queryWithApproval(t)))
	if !specAction(t, policy, "searcher", "search", "query").RequiresApproval {
		t.Fatal("inherit dropped the parent's approval requirement")
	}

	// inherit may add a requirement the parent lacks.
	adding := strings.Replace(basePolicy, `{"name": "query", "inherit": true}`,
		`{"name": "query", "inherit": true, `+approvalRequired+`}`, 1)
	policy = compileOne(t, decodeString(t, "base.atpl.json", adding))
	if !specAction(t, policy, "searcher", "search", "query").RequiresApproval ||
		specAction(t, policy, "planner", "search", "query").RequiresApproval {
		t.Fatal("inherit with approval did not add the requirement to the child alone")
	}

	explicitQuery := func(approval bool) Action {
		action := Action{Name: "query", Effects: []string{"reads-corpus"},
			InputSchema: json.RawMessage(`{"type":"object"}`), OutputSchema: json.RawMessage(`{"type":"object"}`)}
		if approval {
			action.Approval = &Approval{Required: true}
		}
		return action
	}
	cases := []struct {
		name          string
		parent, child bool
		refusal       string
	}{
		{"child drops the parent's requirement", true, false,
			"agents[id=searcher].capabilities[name=search].actions[name=query].approval: is required by parent agents[id=planner]"},
		{"child keeps the parent's requirement", true, true, ""},
		{"child adds a requirement", false, true, ""},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			document := base(t)
			if test.parent {
				agentByID(&document, "planner").Capabilities[0].Actions[0].Approval = &Approval{Required: true}
			}
			agentByID(&document, "searcher").Capabilities[0].Actions[0] = explicitQuery(test.child)
			_, err := Compile([]Document{document}, testNow, nil)
			switch {
			case test.refusal == "" && err != nil:
				t.Fatalf("compile = %v", err)
			case test.refusal != "" && (err == nil || !strings.Contains(err.Error(), test.refusal)):
				t.Fatalf("compile = %v, want %q", err, test.refusal)
			}

			// Parity: Register decides the same way.
			registry := newRegistry(t, document)
			parentDocument := document
			parentDocument.Agents = []Agent{*agentByID(&document, "planner")}
			planner := compileOne(t, parentDocument).Agents()[0]
			if _, err := registry.Register(t, planner, 0, "key-planner"); err != nil {
				t.Fatal(err)
			}
			spec := rawSpec(*agentByID(&document, "searcher"), testNow, &planner)
			_, registerErr := registry.Register(t, spec, 0, "key-searcher")
			if (registerErr == nil) != (test.refusal == "") {
				t.Fatalf("Register = %v, compile refusal %q", registerErr, test.refusal)
			}
		})
	}

	// A live parent's requirement is inherited too.
	registry := newRegistry(t, base(t))
	parentDocument := decodeString(t, "base.atpl.json", queryWithApproval(t))
	planner := compileOne(t, parentDocument).Agents()[0]
	if _, err := registry.Register(t, planner, 0, "key-planner"); err != nil {
		t.Fatal(err)
	}
	childOnly := base(t)
	childOnly.Agents = []Agent{*agentByID(&childOnly, "searcher")}
	policy, _ = compileLive(t, childOnly, registry)
	if !specAction(t, policy, "searcher", "search", "query").RequiresApproval {
		t.Fatal("inherit from a live parent dropped its approval requirement")
	}
	childOnly.Agents[0].Capabilities[0].Actions[0] = explicitQuery(false)
	if _, err := Compile([]Document{childOnly}, testNow, LiveParents(registry.Live(t))); err == nil ||
		!strings.Contains(err.Error(), ".approval: is required by parent agents[id=planner] (live)") {
		t.Fatalf("dropping a live parent's requirement = %v", err)
	}
}

func TestInheritedApprovalSurvivesEncode(t *testing.T) {
	text := strings.Replace(basePolicy, `{"name": "query", "inherit": true}`,
		`{"name": "query", "inherit": true, `+approvalRequired+`}`, 1)
	document := decodeString(t, "base.atpl.json", text)
	policy := compileOne(t, document)
	encoded, err := Encode(document)
	if err != nil {
		t.Fatal(err)
	}
	reread := decodeString(t, "base.atpl.json", string(encoded))
	query := agentByID(&reread, "searcher").Capabilities[0].Actions[0]
	if !query.Inherit || query.Approval == nil || !query.Approval.Required {
		t.Fatalf("encode lost the inherited action's approval: %+v\n%s", query, encoded)
	}
	recompiled := compileOne(t, reread)
	if recompiled.Digest() != policy.Digest() {
		t.Fatalf("digest changed across encode: %s != %s", recompiled.Digest(), policy.Digest())
	}
	if !specAction(t, recompiled, "searcher", "search", "query").RequiresApproval {
		t.Fatal("child no longer requires approval after encode")
	}
}

func TestCreatePlanShowsApproval(t *testing.T) {
	document := decodeString(t, "base.atpl.json", withApproval(t))
	policy := compileOne(t, document)
	registry := newRegistry(t, document)
	plan := Diff(policy, registry.Live(t), "")
	for _, entry := range plan.Entries {
		if entry.Kind != KindCreate {
			t.Fatalf("%s = %s, want create", entry.ID, entry.Kind)
		}
		var approvals []Change
		for _, change := range entry.Changes {
			if strings.HasSuffix(change.Path, ".approval") {
				approvals = append(approvals, change)
			}
		}
		var want []Change
		if entry.ID == "planner" {
			want = []Change{{Op: "+", Path: "capabilities[name=tickets].actions[name=open].approval", Detail: "required"}}
		}
		if !reflect.DeepEqual(approvals, want) {
			t.Fatalf("%s approval changes = %+v, want %+v (all: %+v)", entry.ID, approvals, want, entry.Changes)
		}
	}
}

func TestDigestsChangeOnlyForApprovalRequiredActions(t *testing.T) {
	document := base(t)
	policy := compileOne(t, document)
	if policy.Digest() != goldenBasePolicyDigest {
		t.Fatalf("a policy without approval changed digest: %s, want %s", policy.Digest(), goldenBasePolicyDigest)
	}
	if strings.Contains(string(policy.CanonicalJSON()), "approval") {
		t.Fatalf("canonical JSON mentions approval:\n%s", policy.CanonicalJSON())
	}
	registry := newRegistry(t, document)
	applyDirect(t, registry, Diff(policy, registry.Live(t), ""))
	live := registry.Live(t)
	if got := ContentDigest(live["planner"]); got != goldenPlannerContent {
		t.Fatalf("planner content digest %s, want %s", got, goldenPlannerContent)
	}
	if got := ContentDigest(live["searcher"]); got != goldenSearcherContent {
		t.Fatalf("searcher content digest %s, want %s", got, goldenSearcherContent)
	}
	if got := Diff(policy, live, "https://registry.example").Digest; got != goldenUnchangedPlan {
		t.Fatalf("plan digest %s, want %s", got, goldenUnchangedPlan)
	}

	approved := compileOne(t, decodeString(t, "base.atpl.json", withApproval(t)))
	if approved.Digest() == policy.Digest() {
		t.Fatal("an approval requirement did not change the policy digest")
	}
	canonical := string(approved.CanonicalJSON())
	if strings.Count(canonical, `"approval":{"required":true}`) != 1 ||
		!strings.Contains(canonical, `"name":"open","effects":["external"],"input_schema":`) {
		t.Fatalf("canonical JSON does not carry exactly one requirement:\n%s", canonical)
	}
	// Removing the requirement restores the original bytes exactly.
	without := strings.Replace(canonical, `,"approval":{"required":true}`, ``, 1)
	if without != string(policy.CanonicalJSON()) {
		t.Fatalf("approval changed more than its own action:\n%s\n%s", without, policy.CanonicalJSON())
	}

	flagged := live["planner"]
	flagged.Capabilities = cloneCapabilities(flagged.Capabilities)
	flagged.Capabilities[1].Actions[0].RequiresApproval = true
	if ContentDigest(flagged) == goldenPlannerContent {
		t.Fatal("content digest ignores approval")
	}
}

func TestPlanAddingApprovalNarrowsAndDroppingItIsRefused(t *testing.T) {
	document := base(t)
	registry := newRegistry(t, document)
	policy, live := compileLive(t, document, registry)
	applyDirect(t, registry, Diff(policy, live, ""))
	registry.Clock.Set(testNow.Add(time.Minute))

	// Adding a requirement on the parent's query, which searcher inherits:
	// both narrow, and apply lands both through Register.
	approved := decodeString(t, "base.atpl.json", queryWithApproval(t))
	policy, live = compileLive(t, approved, registry)
	plan := Diff(policy, live, "")
	if got := kinds(plan); got["planner"] != KindNarrow || got["searcher"] != KindNarrow {
		t.Fatalf("adding approval planned as %v", got)
	}
	for _, entry := range plan.Entries {
		want := Change{Op: "+", Path: "capabilities[name=search].actions[name=query].approval", Detail: "required"}
		if !reflect.DeepEqual(entry.Changes, []Change{want}) {
			t.Fatalf("%s changes = %+v", entry.ID, entry.Changes)
		}
	}
	applyDirect(t, registry, plan)
	live = registry.Live(t)
	for _, id := range []shoal.ID{"planner", "searcher"} {
		query := findAction(findCapability(live[id].Capabilities, "search"), "query")
		if !query.RequiresApproval {
			t.Fatalf("%s live query lacks approval after apply", id)
		}
	}
	registry.Clock.Set(testNow.Add(2 * time.Minute))
	policy, live = compileLive(t, approved, registry)
	if got := kinds(Diff(policy, live, "")); got["planner"] != KindUnchanged || got["searcher"] != KindUnchanged {
		t.Fatalf("re-plan after apply = %v", got)
	}

	// The original policy, without approval, would drop it: refused as a
	// widening, naming the action, and nothing is written.
	policy, live = compileLive(t, document, registry)
	plan = Diff(policy, live, "")
	for _, entry := range plan.Entries {
		if entry.Kind != KindRefusedWidening {
			t.Fatalf("%s = %s, want refused-widening", entry.ID, entry.Kind)
		}
		if len(entry.Changes) != 1 || entry.Changes[0].Op != "-" ||
			entry.Changes[0].Path != "capabilities[name=search].actions[name=query].approval" {
			t.Fatalf("%s changes = %+v", entry.ID, entry.Changes)
		}
	}
	if len(plan.Writes()) != 0 {
		t.Fatalf("a refused plan writes: %+v", plan.Writes())
	}
	// Register agrees: the dropping write is refused.
	for _, spec := range policy.Agents() {
		if spec.ID == "planner" {
			if _, err := registry.Register(t, spec, live["planner"].Generation, "key-drop"); err == nil {
				t.Fatal("Register accepted a write that drops approval")
			}
		}
	}
}

func TestPlanRefusesAddingApprovalOverALiveChildItDoesNotRewrite(t *testing.T) {
	document := base(t)
	registry := newRegistry(t, document)
	policy, live := compileLive(t, document, registry)
	applyDirect(t, registry, Diff(policy, live, ""))
	registry.Clock.Set(testNow.Add(time.Minute))

	// The policy manages only planner now; searcher stays live, unmanaged,
	// without approval on the query it holds from planner.
	approved := decodeString(t, "base.atpl.json", queryWithApproval(t))
	approved.Agents = approved.Agents[:1]
	policy, live = compileLive(t, approved, registry)
	plan := Diff(policy, live, "")
	got := kinds(plan)
	if got["planner"] != KindRefusedDelegation || got["searcher"] != KindUnmanaged {
		t.Fatalf("kinds = %v", got)
	}
	for _, entry := range plan.Entries {
		if entry.ID == "planner" && !strings.Contains(entry.Reason, "live child agents[id=searcher]") {
			t.Fatalf("reason = %q", entry.Reason)
		}
	}
}

func TestExportWritesApprovalAndRoundTrips(t *testing.T) {
	// Both shapes: declared on the parent's open, and added on an inherited
	// query by the child.
	text := strings.Replace(withApproval(t), `{"name": "query", "inherit": true}`,
		`{"name": "query", "inherit": true, `+approvalRequired+`}`, 1)
	document := decodeString(t, "base.atpl.json", text)
	policy := compileOne(t, document)
	registry := newRegistry(t, document)
	applyDirect(t, registry, Diff(policy, registry.Live(t), ""))
	live := registry.Live(t)
	if !findAction(findCapability(live["planner"].Capabilities, "tickets"), "open").RequiresApproval ||
		!findAction(findCapability(live["searcher"].Capabilities, "search"), "query").RequiresApproval {
		t.Fatal("the registry did not store the compiled requirements")
	}

	exported, err := Export(live, document.Executors, testNow)
	if err != nil {
		t.Fatalf("export: %v", err)
	}
	encoded, err := Encode(exported)
	if err != nil {
		t.Fatal(err)
	}
	if count := strings.Count(string(encoded), `"approval": {`); count != 2 {
		t.Fatalf("export wrote %d approval requirements, want 2:\n%s", count, encoded)
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
	if plan := Diff(recompiled, live, ""); len(plan.Writes()) != 0 || len(plan.Refusals()) != 0 {
		t.Fatalf("recompiled export does not plan as unchanged: %+v", plan.Entries)
	}

	// Without the flag the same agents export without approval at all.
	plain := compileOne(t, base(t))
	plainRegistry := newRegistry(t, base(t))
	applyDirect(t, plainRegistry, Diff(plain, plainRegistry.Live(t), ""))
	exported, err = Export(plainRegistry.Live(t), nil, testNow)
	if err != nil {
		t.Fatal(err)
	}
	if encoded, _ := Encode(exported); strings.Contains(string(encoded), "approval") {
		t.Fatalf("export wrote approval for agents without it:\n%s", encoded)
	}
}
