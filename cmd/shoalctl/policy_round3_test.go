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
	"net/http"
	"path/filepath"
	"strings"
	"testing"

	"github.com/phrocker/shoal-oss/pkg/explorer/fleet"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

func TestEndpointKeepsAnIPv6Zone(t *testing.T) {
	tokenFile := filepath.Join(t.TempDir(), "token")
	writeFile(t, tokenFile, "token")
	client, err := newFleetClient("https://[fe80::1%25eth0]:8443/shoal/", tokenFile)
	if err != nil {
		t.Fatal(err)
	}
	request, err := http.NewRequest(http.MethodPost, client.base+"/api/v1/fleet/agents", nil)
	if err != nil {
		t.Fatalf("request URL built on %q does not parse: %v", client.base, err)
	}
	if request.URL.Hostname() != "fe80::1%eth0" || request.URL.Port() != "8443" ||
		request.URL.Path != "/shoal/api/v1/fleet/agents" {
		t.Fatalf("request URL = %s (host %q)", request.URL, request.URL.Hostname())
	}
}

// twoStepFixture applies the wide policy and stages the narrowed one whose
// apply writes the searcher in two steps around the planner.
func twoStepFixture(t *testing.T) (*policyFixture, string) {
	t.Helper()
	fixture := newPolicyFixture(t)
	wide := strings.Replace(policyAgents, `"scopes": [{"source_id": "source-a", "policy_id": "policy"}],
      "executor_ref": "search-exec"`, searcherTwoScopes, 1)
	wide = strings.Replace(wide, `"lease_ttl": "6h"`, `"lease_ttl": "12h"`, 1)
	writeFile(t, filepath.Join(fixture.directory, "agents.atpl.json"), wide)
	if _, err := fixture.applyReviewed(t); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(fixture.directory, "agents.atpl.json"),
		strings.Replace(narrowedAgents(), `"lease_ttl": "6h"`, `"lease_ttl": "12h"`, 1))
	_, digest := fixture.plan(t)
	return fixture, digest
}

// beforeRegister runs change before the nth register request apply makes.
func (f *policyFixture) beforeRegister(n int, change func()) {
	f.mu.Lock()
	defer f.mu.Unlock()
	count := 0
	f.observe = func() {
		if count++; count == n {
			change()
		}
	}
}

// otherWriter re-registers an agent with changed content, a change apply
// must not paper over.
func (f *policyFixture) otherWriter(t *testing.T, id shoal.ID, change func(*fleet.Spec)) {
	t.Helper()
	current := f.registry.Live(t)[id]
	spec := fleet.Spec{
		ID: current.ID, ParentID: current.ParentID, AuthorizationDomain: current.AuthorizationDomain,
		Scopes: current.Scopes, ExecutorRef: current.ExecutorRef, LeaseExpiresAt: current.LeaseExpiresAt,
		Capabilities: current.Capabilities,
	}
	change(&spec)
	if _, err := f.registry.Register(t, spec, current.Generation, "other-writer-"+string(id)); err != nil {
		t.Error(err)
	}
}

func TestApplyStoppedBetweenStepsNamesTheClampedChild(t *testing.T) {
	fixture, digest := twoStepFixture(t)
	// The planner is the second write; another writer changes it first.
	fixture.beforeRegister(2, func() {
		fixture.otherWriter(t, "planner", func(spec *fleet.Spec) {
			spec.Capabilities = spec.Capabilities[:1]
		})
	})
	stdout, _, err := fixture.run(t, append([]string{"apply", fixture.directory, "-plan-digest", digest},
		fixture.registryArgs()...)...)
	// The registry refuses the planner's write as widening what the other
	// writer narrowed; any refusal there stops apply between the steps.
	if err == nil || !strings.Contains(err.Error(), "agents[id=planner]: registry refused") ||
		!strings.Contains(err.Error(), "stopped after 1 of 3 writes; agents[id=searcher] holds its clamped lease until ") ||
		!strings.Contains(err.Error(), "a re-plan will show it unchanged") {
		t.Fatalf("apply stopped between steps = %v\n%s", err, stdout)
	}
}

func TestApplyConflictOnASecondStepCarriesTheStepLabel(t *testing.T) {
	fixture, digest := twoStepFixture(t)
	fixture.beforeRegister(3, func() {
		fixture.otherWriter(t, "searcher", func(spec *fleet.Spec) {
			spec.ExecutorRef = "remote-exec"
		})
	})
	stdout, _, err := fixture.run(t, append([]string{"apply", fixture.directory, "-plan-digest", digest},
		fixture.registryArgs()...)...)
	if err == nil ||
		!strings.Contains(err.Error(), "agents[id=searcher] (step 2 of 2): registry refused (409 conflict)") ||
		!strings.Contains(err.Error(), "its live content changed since the plan") ||
		!strings.Contains(err.Error(), "stopped after 2 of 3 writes; agents[id=searcher] holds its clamped lease") {
		t.Fatalf("conflict on a second step = %v\n%s", err, stdout)
	}
}
