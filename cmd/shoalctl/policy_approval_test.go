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
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Approval end to end over HTTP: a policy that adds a requirement plans as a
// narrowing and applies; the live fleet then exports with the requirement and
// recompiles to the applied policy; a policy that omits it again is refused.
func TestPolicyApprovalPlansAppliesAndExports(t *testing.T) {
	fixture := newPolicyFixture(t)
	if _, err := fixture.applyReviewed(t); err != nil {
		t.Fatal(err)
	}

	old := `{"name": "open", "effects": ["external"],`
	if !strings.Contains(policyAgents, old) {
		t.Fatalf("fixture lacks %q", old)
	}
	approved := strings.Replace(policyAgents, old,
		`{"name": "open", "approval": {"required": true}, "effects": ["external"],`, 1)
	writeFile(t, filepath.Join(fixture.directory, "agents.atpl.json"), approved)
	planned, digest := fixture.plan(t)
	for _, want := range []string{
		"~ agents[id=planner] narrow (live generation 1)",
		"    + capabilities[name=tickets].actions[name=open].approval: required",
		"= agents[id=searcher] unchanged",
		"0 refused",
	} {
		if !strings.Contains(planned, want) {
			t.Fatalf("plan output missing %q:\n%s", want, planned)
		}
	}
	policyDigest := policyDigestLine.FindStringSubmatch(planned)[1]
	applied, _, err := fixture.run(t, append([]string{"apply", fixture.directory, "-plan-digest", digest},
		fixture.registryArgs()...)...)
	if err != nil {
		t.Fatalf("apply: %v\n%s", err, applied)
	}
	planner := fixture.registry.Live(t)["planner"]
	var required bool
	for _, capability := range planner.Capabilities {
		for _, action := range capability.Actions {
			if capability.Name == "tickets" && action.Name == "open" {
				required = action.RequiresApproval
			}
		}
	}
	if !required {
		t.Fatal("apply did not register the approval requirement")
	}

	out := filepath.Join(t.TempDir(), "exported")
	exported, stderr, err := fixture.run(t, append([]string{"export", "-out", out, "-executors",
		filepath.Join(fixture.directory, "executors.atpl.json")}, fixture.registryArgs()...)...)
	if err != nil {
		t.Fatalf("export: %v\n%s", err, stderr)
	}
	if !strings.Contains(exported, "policy digest: "+policyDigest) {
		t.Fatalf("export does not recompile to the applied policy:\n%s", exported)
	}
	file, err := os.ReadFile(filepath.Join(out, "agent.planner.atpl.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(file), `"approval": {`) || !strings.Contains(string(file), `"required": true`) {
		t.Fatalf("exported planner lacks its approval requirement:\n%s", file)
	}

	writeFile(t, filepath.Join(fixture.directory, "agents.atpl.json"), policyAgents)
	stdout, _, err := fixture.run(t, append([]string{"plan", fixture.directory}, fixture.registryArgs()...)...)
	if err == nil || !strings.Contains(stdout, "! agents[id=planner] refused-widening") ||
		!strings.Contains(stdout, "    - capabilities[name=tickets].actions[name=open].approval") {
		t.Fatalf("dropping approval = %v:\n%s", err, stdout)
	}
}
