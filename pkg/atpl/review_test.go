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
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/phrocker/shoal-oss/internal/atpltest"
	"github.com/phrocker/shoal-oss/pkg/explorer/fleet"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

// The tests in this file pin the defects review found in the first version.

func searcherLike(id, parent string) Agent {
	return Agent{
		ID: id, Parent: parent, AuthorizationDomain: "domain",
		Scopes:      []Scope{{SourceID: "source-a", PolicyID: "policy"}},
		ExecutorRef: "search-exec", LeaseTTL: "6h",
		Capabilities: []Capability{{Name: "search", Actions: []Action{{Name: "query", Inherit: true}}}},
	}
}

func liveFrom(t *testing.T, registry *atpltest.Registry, ids ...shoal.ID) LiveParents {
	t.Helper()
	result := LiveParents{}
	for _, id := range ids {
		stored, err := registry.Store.Get(context.Background(), id)
		if err != nil {
			t.Fatal(err)
		}
		result[id] = stored.Descriptor
	}
	return result
}

// Finding 1: depth and validity through live ancestors.
func TestLiveAncestorDepthMatchesRegister(t *testing.T) {
	document := base(t)
	registry := newRegistry(t, document)
	root := compileOne(t, base(t)).Agents()[0]
	parent := shoal.ID("")
	for index := 0; index < fleet.MaxDelegationDepth-1; index++ {
		spec := root
		spec.ID = shoal.ID(fmt.Sprintf("chain-%02d", index))
		spec.ParentID = parent
		if _, err := registry.Register(t, spec, 0, "key-"+string(spec.ID)); err != nil {
			t.Fatalf("register chain %d: %v", index, err)
		}
		parent = spec.ID
	}
	live := LiveParents(registry.Live(t))
	if len(live) != fleet.MaxDelegationDepth-1 {
		t.Fatalf("live chain = %d", len(live))
	}
	policy := Document{ATPL: Version, Origin: Origin, Executors: document.Executors,
		Agents: []Agent{searcherLike("deep", string(parent))}}.WithName("deep.atpl.json")
	compiled, err := Compile([]Document{policy}, testNow, live)
	if err != nil {
		t.Fatalf("64th registration: %v", err)
	}
	policy.Agents = append(policy.Agents, searcherLike("deeper", "deep"))
	if _, err := Compile([]Document{policy}, testNow, live); err == nil ||
		!strings.Contains(err.Error(), "agents[id=deeper].parent: delegation depth exceeds 64") {
		t.Fatalf("65th registration = %v", err)
	}

	if _, err := registry.Register(t, compiled.Agents()[0], 0, "key-deep"); err != nil {
		t.Fatalf("Register refused the 64th registration: %v", err)
	}
	deep := compiled.Agents()[0]
	spec := rawSpec(searcherLike("deeper", "deep"), testNow, &deep)
	if _, err := registry.Register(t, spec, 0, "key-deeper"); err == nil {
		t.Fatal("Register accepted a 65th registration the compiler refuses")
	}
}

func TestRevokedOrBrokenLiveAncestorsAreRefusedLikeRegister(t *testing.T) {
	document := base(t)
	registry := newRegistry(t, document)
	root := compileOne(t, base(t)).Agents()[0]
	middle := root
	middle.ID, middle.ParentID = "middle", root.ID
	if _, err := registry.Register(t, root, 0, "key-root"); err != nil {
		t.Fatal(err)
	}
	if _, err := registry.Register(t, middle, 0, "key-middle"); err != nil {
		t.Fatal(err)
	}
	ctx, requestContext := registry.Context(t)
	if _, err := registry.Service.Revoke(ctx, fleet.RevokeRequest{
		Context: requestContext, RegistrationKey: "revoke-root", ID: root.ID, ExpectedGeneration: 1,
	}); err != nil {
		t.Fatal(err)
	}
	live := liveFrom(t, registry, root.ID, middle.ID)
	policy := Document{ATPL: Version, Origin: Origin, Executors: document.Executors,
		Agents: []Agent{searcherLike("leaf", "middle")}}.WithName("leaf.atpl.json")
	if _, err := Compile([]Document{policy}, testNow, live); err == nil ||
		!strings.Contains(err.Error(), "live ancestor agents[id=planner] is revoked or expired") {
		t.Fatalf("child under a revoked ancestor = %v", err)
	}
	spec := rawSpec(searcherLike("leaf", "middle"), testNow, &middle)
	if _, err := registry.Register(t, spec, 0, "key-leaf"); err == nil {
		t.Fatal("Register accepted a child under a revoked ancestor")
	}

	// A link that no longer narrows, and an ancestor that is not visible.
	broken := liveFrom(t, registry, middle.ID)
	rootDescriptor := live[root.ID]
	rootDescriptor.RevokedAt = time.Time{}
	rootDescriptor.Scopes = []fleet.Scope{{SourceID: []byte("source-b"), PolicyID: []byte("policy")}}
	broken[root.ID] = rootDescriptor
	if _, err := Compile([]Document{policy}, testNow, broken); err == nil ||
		!strings.Contains(err.Error(), "live link from agents[id=middle] to agents[id=planner] no longer narrows") {
		t.Fatalf("child under a broken link = %v", err)
	}
	delete(broken, root.ID)
	if _, err := Compile([]Document{policy}, testNow, broken); err == nil ||
		!strings.Contains(err.Error(), "live ancestor agents[id=planner] is not visible") {
		t.Fatalf("child under an invisible ancestor = %v", err)
	}
}

// Finding 2: documents built in code cannot smuggle invalid UTF-8 into the
// digest.
func TestCompileRefusesNonUTF8FromCodeBuiltDocuments(t *testing.T) {
	for _, mutate := range []struct {
		name   string
		apply  func(*Document)
		prefix string
	}{
		{"id", func(d *Document) { agentByID(d, "planner").ID = "planner\xff" }, ".id: is not valid UTF-8"},
		{"domain", func(d *Document) { d.Agents[0].AuthorizationDomain = "domain\xff" }, ".authorization_domain: is not valid UTF-8"},
		{"scope", func(d *Document) { d.Agents[0].Scopes[0].SourceID = "source\xfe" }, ".scopes[0].source_id: is not valid UTF-8"},
		{"executor", func(d *Document) { d.Executors[0].Ref = "exec\xff" }, ".ref: executor reference may contain only ASCII"},
	} {
		t.Run(mutate.name, func(t *testing.T) {
			document := base(t)
			document.Agents = document.Agents[:1]
			mutate.apply(&document)
			if _, err := Compile([]Document{document}, testNow, nil); err == nil ||
				!strings.Contains(err.Error(), mutate.prefix) {
				t.Fatalf("compile = %v", err)
			}
		})
	}
}

// Finding 3: a heartbeat that leaves a parent with less lease than its child
// must still export to a policy that compiles.
func TestExportAfterAHeartbeatCompiles(t *testing.T) {
	document := base(t)
	registry := newRegistry(t, document)
	applyDirect(t, registry, Diff(compileOne(t, document), registry.Live(t), ""))
	later := testNow.Add(time.Hour)
	registry.Clock.Set(later)
	ctx, requestContext := registry.Context(t)
	if _, err := registry.Service.Heartbeat(ctx, fleet.HeartbeatRequest{
		Context: requestContext, RegistrationKey: "heartbeat", ID: "planner",
		ExpectedGeneration: 1, LeaseExpiresAt: later.Add(30 * time.Minute),
	}); err != nil {
		t.Fatal(err)
	}
	exported, err := Export(registry.Live(t), document.Executors, later.Add(400*time.Millisecond))
	if err != nil {
		t.Fatal(err)
	}
	if exported.Agents[0].LeaseTTL != "30m0s" || exported.Agents[1].LeaseTTL != "30m0s" {
		t.Fatalf("exported TTLs = %s, %s", exported.Agents[0].LeaseTTL, exported.Agents[1].LeaseTTL)
	}
	if _, err := Compile([]Document{exported.WithName("exported.atpl.json")}, later, nil); err != nil {
		t.Fatalf("export after a heartbeat does not compile: %v", err)
	}
	if _, err := Export(registry.Live(t), document.Executors, later.Add(30*time.Minute-400*time.Millisecond)); err == nil ||
		!strings.Contains(err.Error(), "lease ends within a second") {
		t.Fatalf("export of an expiring lease = %v", err)
	}
}

// Finding 4: export of an agent near the registry's bound fits a file, and a
// policy may span one file per agent.
func TestLargeDescriptorsExportWithinTheFileBound(t *testing.T) {
	var properties strings.Builder
	for i := 0; properties.Len() < 50<<10; i++ {
		if i > 0 {
			properties.WriteString(",")
		}
		fmt.Fprintf(&properties, `"p%05d":{"type":"object","properties":{"x":{"type":"array","items":{"type":"string"}}}}`, i)
	}
	schema := json.RawMessage(`{"type":"object","properties":{` + properties.String() + `}}`)
	var actions []fleet.Action
	for i := 0; i < 8; i++ {
		actions = append(actions, fleet.Action{
			Name: fmt.Sprintf("action-%d", i), InputSchema: schema, OutputSchema: json.RawMessage(`{}`),
		})
	}
	spec, err := fleet.Spec{
		ID: "large", AuthorizationDomain: []byte("domain"),
		Scopes:      []fleet.Scope{{SourceID: []byte("source-a"), PolicyID: []byte("policy")}},
		ExecutorRef: "exec", LeaseExpiresAt: testNow.Add(time.Hour),
		Capabilities: []fleet.Capability{{Name: "big", Actions: actions}},
	}.Canonical(testNow)
	if err != nil {
		t.Fatal(err)
	}
	wire, _ := json.Marshal(spec)
	descriptor := fleet.Descriptor{
		ID: spec.ID, Generation: 1, AuthorizationDomain: spec.AuthorizationDomain,
		Scopes: spec.Scopes, ExecutorRef: spec.ExecutorRef, Capabilities: spec.Capabilities,
		LeaseExpiresAt: spec.LeaseExpiresAt, UpdatedAt: testNow,
	}
	exported, err := Export(map[shoal.ID]fleet.Descriptor{"large": descriptor}, nil, testNow)
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := Encode(exported)
	if err != nil {
		t.Fatal(err)
	}
	if len(encoded) > len(wire)+len(wire)/10 {
		t.Fatalf("encoded file is %d bytes for a %d-byte descriptor", len(encoded), len(wire))
	}
	if bytes.Contains(encoded, []byte("atpl-schema-")) {
		t.Fatal("a schema placeholder survived encoding")
	}
	reread, err := Decode("large.atpl.json", bytes.NewReader(encoded))
	if err != nil {
		t.Fatal(err)
	}
	policy, err := Compile([]Document{reread}, testNow, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := policy.Agents()[0]; !bytes.Equal(got.Capabilities[0].Actions[0].InputSchema, spec.Capabilities[0].Actions[0].InputSchema) {
		t.Fatal("schema changed across export")
	}
	if MaxFiles < MaxAgents+1 {
		t.Fatalf("MaxFiles %d cannot hold one file per agent and an executor file", MaxFiles)
	}
}

// Finding 5: unpaired surrogate escapes would decode to U+FFFD.
func TestUnpairedSurrogateEscapesAreRefused(t *testing.T) {
	for _, text := range []string{
		strings.Replace(basePolicy, `"id": "planner"`, `"id": "planner\ud800"`, 1),
		strings.Replace(basePolicy, `"id": "planner"`, `"id": "planner\udc00"`, 1),
		strings.Replace(basePolicy, `"id": "planner"`, `"id": "planner\ud800A"`, 1),
		strings.Replace(basePolicy, `"id": "planner",`, `"id": "planner", "\ud800": 1, "\ud801": 2,`, 1),
	} {
		if _, err := Decode("surrogate.atpl.json", strings.NewReader(text)); err == nil ||
			!strings.Contains(err.Error(), "unpaired surrogate escape") {
			t.Fatalf("decode = %v", err)
		}
	}
	paired := strings.Replace(basePolicy, `{"type": "object"}, "output_schema"`,
		`{"type": "object", "description": "😀 \\ud800"}, "output_schema"`, 1)
	if _, err := Decode("paired.atpl.json", strings.NewReader(paired)); err != nil {
		t.Fatalf("a paired surrogate and an escaped backslash: %v", err)
	}
}

// Findings 7, 8, 9 and 12: the plan digest survives heartbeats and binds the
// registry, updates land deepest first, and an executor change is its own kind.
func TestPlanDigestWriteOrderAndExecutorChange(t *testing.T) {
	document := base(t)
	agentByID(&document, "searcher").Scopes = append(agentByID(&document, "searcher").Scopes,
		Scope{SourceID: "source-b", PolicyID: "policy"})
	registry := newRegistry(t, document)
	applyDirect(t, registry, Diff(compileOne(t, document), registry.Live(t), ""))

	narrowed := base(t)
	agentByID(&narrowed, "planner").Scopes = agentByID(&narrowed, "planner").Scopes[:1]
	policy, live := compileLive(t, narrowed, registry)
	plan := Diff(policy, live, "https://registry.example")
	writes := plan.Writes()
	if len(writes) != 2 || writes[0].ID != "searcher" || writes[1].ID != "planner" {
		t.Fatalf("update order = %+v", writes)
	}
	if other := Diff(policy, live, "https://staging.example"); other.Digest == plan.Digest {
		t.Fatal("plan digest does not bind the registry")
	}

	ctx, requestContext := registry.Context(t)
	if _, err := registry.Service.Heartbeat(ctx, fleet.HeartbeatRequest{
		Context: requestContext, RegistrationKey: "heartbeat", ID: "planner",
		ExpectedGeneration: 1, LeaseExpiresAt: testNow.Add(20 * time.Hour),
	}); err != nil {
		t.Fatal(err)
	}
	afterHeartbeat := Diff(policy, registry.Live(t), "https://registry.example")
	if afterHeartbeat.Digest != plan.Digest {
		t.Fatal("a heartbeat changed the plan digest")
	}
	for _, entry := range afterHeartbeat.Entries {
		if entry.ID == "planner" && entry.LiveGeneration != 2 {
			t.Fatalf("plan did not see the heartbeat's generation: %d", entry.LiveGeneration)
		}
	}
	// Each write is valid against what is live when it lands.
	for _, entry := range writes {
		current := registry.Live(t)[entry.ID]
		if _, err := registry.Register(t, entry.Spec, current.Generation, "narrow-"+string(entry.ID)); err != nil {
			t.Fatalf("register %s: %v", entry.ID, err)
		}
		ctx, requestContext := registry.Context(t)
		if _, err := registry.Service.Resolve(ctx, fleet.ResolveRequest{
			Context: requestContext, ID: "searcher",
		}); err != nil {
			t.Fatalf("searcher stopped resolving after writing %s: %v", entry.ID, err)
		}
	}

	moved := base(t)
	agentByID(&moved, "planner").Scopes = agentByID(&moved, "planner").Scopes[:1]
	agentByID(&moved, "searcher").ExecutorRef = "remote-exec"
	policy, live = compileLive(t, moved, registry)
	plan = Diff(policy, live, "")
	if got := kinds(plan); got["searcher"] != KindExecutorChange || KindExecutorChange.Symbol() != "*" {
		t.Fatalf("executor change kinds = %v", got)
	}
	if len(plan.Writes()) != 1 || plan.Writes()[0].Changes[0].Path != "executor_ref" {
		t.Fatalf("executor change plan = %+v", plan.Writes())
	}
}

// Finding 10: a TTL of exactly the registry bound is refused with room for
// clock skew.
func TestLeaseTTLLeavesASkewMargin(t *testing.T) {
	document := base(t)
	agentByID(&document, "planner").LeaseTTL = "24h"
	if _, err := Compile([]Document{document}, testNow, nil); err == nil ||
		!strings.Contains(err.Error(), "at most 23h55m0s") {
		t.Fatalf("24h TTL = %v", err)
	}
	agentByID(&document, "planner").LeaseTTL = MaxLeaseTTL.String()
	if _, err := Compile([]Document{document}, testNow, nil); err != nil {
		t.Fatalf("TTL at the margin: %v", err)
	}
}
