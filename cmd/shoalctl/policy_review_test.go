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

package main

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/phrocker/shoal-oss/pkg/atpl"
	"github.com/phrocker/shoal-oss/pkg/explorer/fleet"
)

func TestEndpointRefusesQueriesFragmentsAndCredentials(t *testing.T) {
	tokenFile := filepath.Join(t.TempDir(), "token")
	writeFile(t, tokenFile, "token")
	for _, endpoint := range []string{
		"https://registry.example/x?", "https://registry.example?", "https://registry.example/#",
		"https://registry.example/?a=b", "https://user:secret@registry.example",
	} {
		if _, err := newFleetClient(endpoint, tokenFile); err == nil ||
			!strings.Contains(err.Error(), "without credentials, query or fragment") {
			t.Fatalf("newFleetClient(%q) = %v", endpoint, err)
		}
	}
	client, err := newFleetClient("HTTPS://Registry.Example:443/base/", tokenFile)
	if err != nil {
		t.Fatal(err)
	}
	if client.base != "https://registry.example/base" {
		t.Fatalf("base = %q", client.base)
	}
}

func TestExportRefusesADirectoryOverThePolicyBound(t *testing.T) {
	var agents atpl.Document
	if err := json.Unmarshal([]byte(policyAgents), &agents); err != nil {
		t.Fatal(err)
	}
	document := atpl.Document{ATPL: atpl.Version, Origin: atpl.Origin, Agents: agents.Agents}
	files, err := exportFiles(document, atpl.MaxPolicyBytes)
	if err != nil {
		t.Fatal(err)
	}
	var total int64
	for _, file := range files {
		total += int64(len(file.data))
	}
	if _, err := exportFiles(document, total); err != nil {
		t.Fatalf("export at the bound: %v", err)
	}
	if _, err := exportFiles(document, total-1); err == nil ||
		!strings.Contains(err.Error(), "more than one policy directory may hold") {
		t.Fatalf("export over the bound = %v", err)
	}
}

func TestPolicyApplyWritesAChildInTwoSteps(t *testing.T) {
	fixture := newPolicyFixture(t)
	wide := strings.Replace(policyAgents, `"scopes": [{"source_id": "source-a", "policy_id": "policy"}],
      "executor_ref": "search-exec"`, searcherTwoScopes, 1)
	wide = strings.Replace(wide, `"lease_ttl": "6h"`, `"lease_ttl": "12h"`, 1)
	writeFile(t, filepath.Join(fixture.directory, "agents.atpl.json"), wide)
	if _, err := fixture.applyReviewed(t); err != nil {
		t.Fatal(err)
	}
	resolvable := func(when string) {
		ctx, requestContext := fixture.registry.Context(t)
		if _, err := fixture.registry.Service.Resolve(ctx, fleet.ResolveRequest{
			Context: requestContext, ID: "searcher",
		}); err != nil {
			t.Errorf("searcher does not resolve %s: %v", when, err)
		}
	}
	fixture.mu.Lock()
	fixture.observe = func() { resolvable("between writes") }
	fixture.mu.Unlock()
	// Equal TTLs computed later than the live leases: the searcher's new lease
	// outlasts the planner's live one, and its live scopes exceed the
	// planner's new ones, so neither single order works.
	writeFile(t, filepath.Join(fixture.directory, "agents.atpl.json"),
		strings.Replace(narrowedAgents(), `"lease_ttl": "6h"`, `"lease_ttl": "12h"`, 1))
	planned, digest := fixture.plan(t)
	for _, want := range []string{
		"  1. ~ agents[id=searcher] narrow (step 1 of 2, lease clamped to ",
		"  2. ~ agents[id=planner] narrow",
		"  3. ~ agents[id=searcher] narrow (step 2 of 2)",
		"    ~ lease: written twice: first clamped to parent agents[id=planner]'s live lease",
	} {
		if !strings.Contains(planned, want) {
			t.Fatalf("plan output missing %q:\n%s", want, planned)
		}
	}
	stdout, _, err := fixture.run(t, append([]string{"apply", fixture.directory, "-plan-digest", digest},
		fixture.registryArgs()...)...)
	if err != nil {
		t.Fatalf("apply: %v\n%s", err, stdout)
	}
	for _, want := range []string{
		"~ agents[id=searcher] narrow (step 1 of 2): generation 2",
		"~ agents[id=planner] narrow: generation 2",
		"~ agents[id=searcher] narrow (step 2 of 2): generation 3",
		"applied 3 of 3 writes",
	} {
		if !strings.Contains(stdout, want) {
			t.Fatalf("apply output missing %q:\n%s", want, stdout)
		}
	}
	resolvable("after apply")
	if after, _ := fixture.plan(t); !strings.Contains(after, "summary: 0 create, 0 narrow, 0 executor-change, 2 unchanged") {
		t.Fatalf("plan after apply:\n%s", after)
	}
}
