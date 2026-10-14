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
	"context"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/phrocker/shoal-oss/internal/atpltest"
	"github.com/phrocker/shoal-oss/pkg/explorer/fleet"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

// wideChain is the base policy with the searcher holding both scopes and the
// given TTL, plus optional further children of the searcher.
func wideChain(t *testing.T, searcherTTL string, extra ...Agent) Document {
	t.Helper()
	document := base(t)
	searcher := agentByID(&document, "searcher")
	searcher.Scopes = append(searcher.Scopes, Scope{SourceID: "source-b", PolicyID: "policy"})
	searcher.LeaseTTL = searcherTTL
	document.Agents = append(document.Agents, extra...)
	return document
}

// dropScopeB narrows every agent in the document to source-a.
func dropScopeB(document Document) Document {
	for i := range document.Agents {
		var kept []Scope
		for _, scope := range document.Agents[i].Scopes {
			if scope.SourceID != "source-b" {
				kept = append(kept, scope)
			}
		}
		document.Agents[i].Scopes = kept
	}
	return document
}

func writeOrder(plan Plan) []string {
	var order []string
	for _, write := range plan.Writes() {
		label := string(write.ID)
		if write.Steps > 1 {
			label = fmt.Sprintf("%s/%d", write.ID, write.Step)
		}
		order = append(order, label)
	}
	return order
}

// applySteps performs a plan's writes through Register, checking after every
// one that each watched agent still resolves.
func applySteps(t *testing.T, registry *atpltest.Registry, plan Plan, watch ...shoal.ID) {
	t.Helper()
	written := map[shoal.ID]int64{}
	before := registry.Live(t)
	for _, write := range plan.Writes() {
		expected := write.LiveGeneration
		if write.Step > 1 {
			expected = written[write.ID]
		}
		key := fmt.Sprintf("apply-%s-%d-%d", write.ID, expected, write.Step)
		descriptor, err := registry.Register(t, write.Spec, expected, key)
		if err != nil {
			t.Fatalf("register %s step %d: %v", write.ID, write.Step, err)
		}
		written[write.ID] = descriptor.Generation
		for _, id := range watch {
			if _, existed := before[id]; !existed {
				if _, created := written[id]; !created {
					continue
				}
			}
			ctx, requestContext := registry.Context(t)
			if _, err := registry.Service.Resolve(ctx, fleet.ResolveRequest{
				Context: requestContext, ID: id,
			}); err != nil {
				t.Fatalf("%s stopped resolving after %s step %d: %v", id, write.ID, write.Step, err)
			}
		}
	}
}

func planAt(t *testing.T, registry *atpltest.Registry, document Document) Plan {
	t.Helper()
	policy, live := compileLive(t, document, registry)
	plan := Diff(policy, live, "")
	if refusals := plan.Refusals(); len(refusals) > 0 {
		t.Fatalf("plan refused: %+v", refusals)
	}
	return plan
}

// The reviewer's repro: equal TTLs, a minute after the last apply, the planner
// drops a capability and the searcher a scope. The searcher's new lease
// outlasts the planner's live one, so the parent must be written first.
func TestOrderParentFirstWhenTheChildsNewLeaseOutlastsTheLiveParent(t *testing.T) {
	document := wideChain(t, "12h")
	registry := newRegistry(t, document)
	applySteps(t, registry, planAt(t, registry, document), "searcher")
	registry.Clock.Set(testNow.Add(time.Minute))

	narrowed := wideChain(t, "12h")
	agentByID(&narrowed, "planner").Capabilities = agentByID(&narrowed, "planner").Capabilities[:1]
	agentByID(&narrowed, "searcher").Scopes = agentByID(&narrowed, "searcher").Scopes[:1]
	plan := planAt(t, registry, narrowed)
	if got := writeOrder(plan); !reflect.DeepEqual(got, []string{"planner", "searcher"}) {
		t.Fatalf("order = %v, want parent first", got)
	}
	applySteps(t, registry, plan, "searcher")
}

func TestOrderChildFirstWhenItFitsTheLiveParent(t *testing.T) {
	document := wideChain(t, "6h")
	registry := newRegistry(t, document)
	applySteps(t, registry, planAt(t, registry, document), "searcher")

	plan := planAt(t, registry, dropScopeB(wideChain(t, "6h")))
	if got := writeOrder(plan); !reflect.DeepEqual(got, []string{"searcher", "planner"}) {
		t.Fatalf("order = %v, want child first", got)
	}
	applySteps(t, registry, plan, "searcher")
}

func TestOrderTwoStepWhenOnlyTheLeaseBlocksChildFirst(t *testing.T) {
	document := wideChain(t, "12h")
	registry := newRegistry(t, document)
	applySteps(t, registry, planAt(t, registry, document), "searcher")
	registry.Clock.Set(testNow.Add(time.Minute))

	plan := planAt(t, registry, dropScopeB(wideChain(t, "12h")))
	if got := writeOrder(plan); !reflect.DeepEqual(got, []string{"searcher/1", "planner", "searcher/2"}) {
		t.Fatalf("order = %v, want a two-step child", got)
	}
	writes := plan.Writes()
	if !writes[0].Spec.LeaseExpiresAt.Equal(testNow.Add(12*time.Hour)) ||
		!writes[2].Spec.LeaseExpiresAt.Equal(testNow.Add(time.Minute+12*time.Hour)) {
		t.Fatalf("two-step leases = %s, %s", writes[0].Spec.LeaseExpiresAt, writes[2].Spec.LeaseExpiresAt)
	}
	var searcher Entry
	for _, entry := range plan.Entries {
		if entry.ID == "searcher" {
			searcher = entry
		}
	}
	if !searcher.TwoStep || searcher.Changes[len(searcher.Changes)-1].Path != "lease" {
		t.Fatalf("plan does not show the two-step write: %+v", searcher)
	}
	oneStep := planAt(t, registry, dropScopeB(wideChain(t, "11h")))
	if oneStep.Digest == plan.Digest {
		t.Fatal("the plan digest does not bind the write order")
	}
	applySteps(t, registry, plan, "searcher")
	if live := registry.Live(t)["searcher"]; !live.LeaseExpiresAt.Equal(writes[2].Spec.LeaseExpiresAt) {
		t.Fatalf("searcher lease after apply = %s", live.LeaseExpiresAt)
	}
}

// No order exists when clamping the child would leave its own live child
// exceeding it, and apply would otherwise hide that grandchild mid-way.
func TestOrderRefusesWhenNoOrderKeepsEveryLinkValid(t *testing.T) {
	document := wideChain(t, "12h")
	registry := newRegistry(t, document)
	applySteps(t, registry, planAt(t, registry, document), "searcher")
	leaf := compileOne(t, document).Agents()[1]
	leaf.ID, leaf.ParentID = "leaf", "searcher"
	leaf.Scopes = leaf.Scopes[:1]
	if _, err := registry.Register(t, leaf, 0, "key-leaf"); err != nil {
		t.Fatal(err)
	}
	registry.Clock.Set(testNow.Add(time.Minute))
	ctx, requestContext := registry.Context(t)
	if _, err := registry.Service.Heartbeat(ctx, fleet.HeartbeatRequest{
		Context: requestContext, RegistrationKey: "shorten", ID: "planner",
		ExpectedGeneration: 1, LeaseExpiresAt: testNow.Add(2 * time.Hour),
	}); err != nil {
		t.Fatal(err)
	}
	live := registry.Live(t)
	stored, err := registry.Store.Get(context.Background(), "leaf")
	if err != nil {
		t.Fatal(err)
	}
	// The registry's list hides the leaf once its parent exceeds the
	// planner; a caller that can still see it must not be led into hiding it
	// for good.
	live["leaf"] = stored.Descriptor
	policy, err := Compile([]Document{dropScopeB(wideChain(t, "12h"))}, registry.Clock.Now(), nil)
	if err != nil {
		t.Fatal(err)
	}
	plan := Diff(policy, live, "")
	if got := kinds(plan); got["searcher"] != KindRefusedDelegation {
		t.Fatalf("kinds = %v", got)
	}
	if reason := plan.Refusals()[0].Reason; !strings.Contains(reason, "no write order keeps every link valid") ||
		!strings.Contains(reason, "child agents[id=leaf]") {
		t.Fatalf("reason = %q", reason)
	}
	if len(plan.Writes()) != 0 {
		t.Fatal("a refused plan still lists writes")
	}
}

func TestOrderAcrossThreeLevels(t *testing.T) {
	reader := Agent{
		ID: "reader", Parent: "searcher", AuthorizationDomain: "domain",
		Scopes:      []Scope{{SourceID: "source-a", PolicyID: "policy"}, {SourceID: "source-b", PolicyID: "policy"}},
		ExecutorRef: "search-exec", LeaseTTL: "12h",
		Capabilities: []Capability{{Name: "search", Actions: []Action{{Name: "query", Inherit: true}}}},
	}
	document := wideChain(t, "12h", reader)
	registry := newRegistry(t, document)
	applySteps(t, registry, planAt(t, registry, document), "reader")
	registry.Clock.Set(testNow.Add(time.Minute))

	plan := planAt(t, registry, dropScopeB(wideChain(t, "12h", reader)))
	want := []string{"reader/1", "searcher/1", "planner", "searcher/2", "reader/2"}
	if got := writeOrder(plan); !reflect.DeepEqual(got, want) {
		t.Fatalf("order = %v, want %v", got, want)
	}
	applySteps(t, registry, plan, "reader", "searcher")
	if after := Diff(compileLiveOnly(t, dropScopeB(wideChain(t, "12h", reader)), registry), registry.Live(t), ""); len(after.Writes()) != 0 {
		t.Fatalf("plan after apply still writes: %v", writeOrder(after))
	}
}

func compileLiveOnly(t *testing.T, document Document, registry *atpltest.Registry) *Policy {
	t.Helper()
	policy, _ := compileLive(t, document, registry)
	return policy
}

// Rounding happens after the check, so 0.6s remaining is not exported as 1s.
func TestExportRefusesASubSecondLeaseBeforeRounding(t *testing.T) {
	descriptor := fleet.Descriptor{
		ID: "agent", Generation: 1, AuthorizationDomain: []byte("domain"),
		Scopes:      []fleet.Scope{{SourceID: []byte("source"), PolicyID: []byte("policy")}},
		ExecutorRef: "exec", Capabilities: compileOne(t, base(t)).Agents()[0].Capabilities,
		LeaseExpiresAt: testNow.Add(600 * time.Millisecond), UpdatedAt: testNow,
	}
	if _, err := Export(map[shoal.ID]fleet.Descriptor{"agent": descriptor}, nil, testNow); err == nil ||
		!strings.Contains(err.Error(), "lease ends within a second") {
		t.Fatalf("export of 0.6s remaining = %v", err)
	}
}
