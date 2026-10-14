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
	"bytes"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/phrocker/shoal-oss/internal/atpltest"
	"github.com/phrocker/shoal-oss/pkg/atpl"
	"github.com/phrocker/shoal-oss/pkg/explorer/auth"
	"github.com/phrocker/shoal-oss/pkg/explorer/fleet"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

const policyExecutors = `{
  "atpl": "shoal.atpl/v1",
  "origin": "Derived from the Agent Trust Policy Language, github.com/SentriusLLC/atpl (Apache-2.0)",
  "executors": [
    {"ref": "search-exec", "max_effects": ["reads-corpus"], "min_effects": ["reads-corpus"]},
    {"ref": "remote-exec", "max_effects": ["external", "reads-corpus"], "min_effects": []}
  ]
}
`

const policyAgents = `{
  "atpl": "shoal.atpl/v1",
  "origin": "Derived from the Agent Trust Policy Language, github.com/SentriusLLC/atpl (Apache-2.0)",
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
           "input_schema": {"type": "object"}, "output_schema": {"type": "object"}}
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
}
`

type policyFixture struct {
	registry  *atpltest.Registry
	server    *httptest.Server
	tokenFile string
	directory string
	// interfere, when set, runs once before the next register request
	// reaches the registry.
	mu        sync.Mutex
	interfere func()
	// observe, when set, runs before every register request.
	observe func()
}

func newPolicyFixture(t *testing.T) *policyFixture {
	t.Helper()
	fixture := &policyFixture{}
	fixture.registry = atpltest.NewRegistry(t, atpltest.NewClock(time.Now()), atpltest.Executors{
		"search-exec": atpltest.Executor{Max: fleet.Effects{fleet.EffectReadsCorpus}, Min: fleet.Effects{fleet.EffectReadsCorpus}},
		"remote-exec": atpltest.Executor{Max: fleet.Effects{fleet.EffectMutatesExternal, fleet.EffectReadsCorpus}},
	}, []string{"source-a", "source-b"}, []string{"policy"})
	handler := fixture.registry.Handler(t, "secret-token")
	fixture.server = httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path == "/api/v1/fleet/agents" {
			fixture.mu.Lock()
			interfere := fixture.interfere
			fixture.interfere = nil
			observe := fixture.observe
			fixture.mu.Unlock()
			if observe != nil {
				observe()
			}
			if interfere != nil {
				interfere()
			}
		}
		handler.ServeHTTP(writer, request)
	}))
	t.Cleanup(fixture.server.Close)
	root := t.TempDir()
	fixture.tokenFile = filepath.Join(root, "token")
	writeFile(t, fixture.tokenFile, "secret-token\n")
	fixture.directory = filepath.Join(root, "policy")
	if err := os.Mkdir(fixture.directory, 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(fixture.directory, "executors.atpl.json"), policyExecutors)
	writeFile(t, filepath.Join(fixture.directory, "agents.atpl.json"), policyAgents)
	return fixture
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func (f *policyFixture) run(t *testing.T, args ...string) (string, string, error) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	err := run(append([]string{"policy"}, args...), &stdout, &stderr)
	return stdout.String(), stderr.String(), err
}

func (f *policyFixture) registryArgs() []string {
	return []string{"-endpoint", f.server.URL, "-token-file", f.tokenFile}
}

var planDigestLine = regexp.MustCompile(`(?m)^plan digest: (atpl:plan:v2:[0-9a-f]{64})$`)
var policyDigestLine = regexp.MustCompile(`(?m)^policy digest: (atpl:policy:v1:[0-9a-f]{64})$`)

func (f *policyFixture) plan(t *testing.T) (string, string) {
	t.Helper()
	stdout, stderr, err := f.run(t, append([]string{"plan", f.directory}, f.registryArgs()...)...)
	if err != nil {
		t.Fatalf("plan: %v\n%s%s", err, stdout, stderr)
	}
	match := planDigestLine.FindStringSubmatch(stdout)
	if match == nil {
		t.Fatalf("plan printed no digest:\n%s", stdout)
	}
	return stdout, match[1]
}

func TestPolicyCompileIsOfflineAndRefusesWithThePath(t *testing.T) {
	fixture := newPolicyFixture(t)
	first, _, err := fixture.run(t, "compile", fixture.directory)
	if err != nil {
		t.Fatal(err)
	}
	second, _, err := fixture.run(t, "compile", fixture.directory)
	if err != nil {
		t.Fatal(err)
	}
	if first != second || policyDigestLine.FindString(first) == "" ||
		!strings.Contains(first, `"lease_ttl_ns": 21600000000000`) {
		t.Fatalf("compile output is not stable canonical JSON:\n%s", first)
	}

	writeFile(t, filepath.Join(fixture.directory, "agents.atpl.json"),
		strings.Replace(policyAgents, `"lease_ttl": "6h"`, `"lease_ttl": "13h"`, 1))
	_, _, err = fixture.run(t, "compile", fixture.directory)
	if err == nil || !strings.Contains(err.Error(), "agents.atpl.json: agents[id=searcher].lease_ttl: 13h0m0s outlasts parent") {
		t.Fatalf("compile refusal = %v", err)
	}
	writeFile(t, filepath.Join(fixture.directory, "agents.atpl.json"),
		strings.Replace(policyAgents, `"id": "planner",`, `"id": "planner", "trust_score": 0.8,`, 1))
	_, _, err = fixture.run(t, "compile", fixture.directory)
	if err == nil || !strings.Contains(err.Error(), "agents[id=planner].trust_score: is not part of ATPL in Shoal") {
		t.Fatalf("reserved field = %v", err)
	}
	if _, _, err := fixture.run(t, "compile", t.TempDir()); err == nil ||
		!strings.Contains(err.Error(), "holds no *.atpl.json files") {
		t.Fatalf("empty directory = %v", err)
	}
}

func TestPolicyPlanApplyRoundTrip(t *testing.T) {
	fixture := newPolicyFixture(t)
	planned, digest := fixture.plan(t)
	for _, want := range []string{
		"+ agents[id=planner] create",
		"+ agents[id=searcher] create",
		"    + capabilities[name=tickets].actions[name=open]: effects [external]",
		"summary: 2 create, 0 narrow, 0 executor-change, 0 unchanged, 0 refused, 0 unmanaged",
	} {
		if !strings.Contains(planned, want) {
			t.Fatalf("plan output missing %q:\n%s", want, planned)
		}
	}
	policyDigest := policyDigestLine.FindStringSubmatch(planned)[1]

	if _, _, err := fixture.run(t, append([]string{"apply", fixture.directory, "-plan-digest",
		"atpl:plan:v2:" + strings.Repeat("0", 64)}, fixture.registryArgs()...)...); err == nil ||
		!strings.Contains(err.Error(), "plan digest mismatch") {
		t.Fatalf("apply with a stale digest = %v", err)
	}
	if live := fixture.registry.Live(t); len(live) != 0 {
		t.Fatalf("a refused apply wrote %d agents", len(live))
	}

	applied, _, err := fixture.run(t, append([]string{"apply", fixture.directory, "-plan-digest", digest},
		fixture.registryArgs()...)...)
	if err != nil {
		t.Fatalf("apply: %v\n%s", err, applied)
	}
	if !strings.Contains(applied, "+ agents[id=planner] create: generation 1") ||
		!strings.Contains(applied, "applied 2 of 2 writes") {
		t.Fatalf("apply output:\n%s", applied)
	}
	if strings.Index(applied, "agents[id=planner]") > strings.Index(applied, "agents[id=searcher]") {
		t.Fatal("apply did not write the parent first")
	}
	var registrations int
	for _, record := range fixture.registry.Recorder.Records() {
		if record.Operation != auth.OperationAgentRegister {
			continue
		}
		registrations++
		if record.ReasonCode != "atpl-apply" || record.ReasonDetail != policyDigest {
			t.Fatalf("lifecycle reason = %q %q, want atpl-apply %s",
				record.ReasonCode, record.ReasonDetail, policyDigest)
		}
	}
	if registrations != 2 {
		t.Fatalf("registrations recorded = %d", registrations)
	}

	unchanged, _ := fixture.plan(t)
	if !strings.Contains(unchanged, "= agents[id=planner] unchanged (live generation 1)") ||
		!strings.Contains(unchanged, "summary: 0 create, 0 narrow, 0 executor-change, 2 unchanged") {
		t.Fatalf("plan after apply:\n%s", unchanged)
	}

	// Export with the executor manifest recompiles to the applied policy.
	out := filepath.Join(t.TempDir(), "exported")
	exported, stderr, err := fixture.run(t, append([]string{"export", "-out", out, "-executors",
		filepath.Join(fixture.directory, "executors.atpl.json")}, fixture.registryArgs()...)...)
	if err != nil {
		t.Fatalf("export: %v\n%s", err, stderr)
	}
	if !strings.Contains(exported, "policy digest: "+policyDigest) || stderr != "" {
		t.Fatalf("export does not recompile to the applied policy:\n%s\n%s", exported, stderr)
	}
	for _, name := range []string{"executors.atpl.json", "agent.planner.atpl.json", "agent.searcher.atpl.json"} {
		if _, err := os.Stat(filepath.Join(out, name)); err != nil {
			t.Fatalf("export file %s: %v", name, err)
		}
	}
	if _, _, err := fixture.run(t, append([]string{"export", "-out", out}, fixture.registryArgs()...)...); err == nil ||
		!strings.Contains(err.Error(), "already holds policy files") {
		t.Fatalf("export over existing files = %v", err)
	}
	_, stderr, err = fixture.run(t, append([]string{"export", "-out", filepath.Join(t.TempDir(), "derived")},
		fixture.registryArgs()...)...)
	if err != nil || !strings.Contains(stderr, "warning: no -executors manifest") {
		t.Fatalf("export without a manifest = %v, stderr %q", err, stderr)
	}
}

// narrowBoth narrows planner and searcher to source-a after both were
// registered holding source-a and source-b.
const searcherTwoScopes = `"scopes": [{"source_id": "source-a", "policy_id": "policy"}, {"source_id": "source-b", "policy_id": "policy"}],
      "executor_ref": "search-exec"`

func narrowedAgents() string {
	narrowed := strings.Replace(policyAgents, `{"source_id": "source-b", "policy_id": "policy"}`, ``, 1)
	return strings.Replace(narrowed, `{"source_id": "source-a", "policy_id": "policy"},`,
		`{"source_id": "source-a", "policy_id": "policy"}`, 1)
}

func (f *policyFixture) applyReviewed(t *testing.T) (string, error) {
	t.Helper()
	_, digest := f.plan(t)
	stdout, _, err := f.run(t, append([]string{"apply", f.directory, "-plan-digest", digest},
		f.registryArgs()...)...)
	return stdout, err
}

func (f *policyFixture) heartbeat(t *testing.T, id shoal.ID) {
	t.Helper()
	ctx, requestContext := f.registry.Context(t)
	current := f.registry.Live(t)[id]
	if _, err := f.registry.Service.Heartbeat(ctx, fleet.HeartbeatRequest{
		Context: requestContext, RegistrationKey: shoal.ID(fmt.Sprintf("heartbeat-%d", current.Generation)),
		ID: id, ExpectedGeneration: current.Generation, LeaseExpiresAt: current.LeaseExpiresAt,
	}); err != nil {
		t.Error(err)
	}
}

func TestPolicyApplyRetriesAHeartbeatButNotAContentChange(t *testing.T) {
	fixture := newPolicyFixture(t)
	if _, err := fixture.applyReviewed(t); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(fixture.directory, "agents.atpl.json"), narrowedAgents())
	planned, digest := fixture.plan(t)
	if !strings.Contains(planned, "~ agents[id=planner] narrow (live generation 1)") ||
		!strings.Contains(planned, "    - scopes[source_id=source-b,policy_id=policy]") {
		t.Fatalf("narrow plan:\n%s", planned)
	}

	// A heartbeat between review and apply moves the generation but not the
	// plan.
	fixture.heartbeat(t, "planner")
	// Another between apply's read and its write fails the compare-and-swap;
	// apply re-reads, sees the same content, and retries once.
	fixture.mu.Lock()
	fixture.interfere = func() { fixture.heartbeat(t, "planner") }
	fixture.mu.Unlock()
	stdout, _, err := fixture.run(t, append([]string{"apply", fixture.directory, "-plan-digest", digest},
		fixture.registryArgs()...)...)
	if err != nil {
		t.Fatalf("apply across heartbeats: %v\n%s", err, stdout)
	}
	if !strings.Contains(stdout, "agents[id=planner]: generation moved from 2 to 3 with no change to its content; retrying once") ||
		!strings.Contains(stdout, "~ agents[id=planner] narrow: generation 4") {
		t.Fatalf("apply output:\n%s", stdout)
	}

	// A concurrent write that changes content is not retried.
	writeFile(t, filepath.Join(fixture.directory, "agents.atpl.json"),
		strings.Replace(narrowedAgents(), `"lease_ttl": "6h"`, `"lease_ttl": "5h"`, 1))
	dropped := strings.Replace(narrowedAgents(), `,
        {"name": "tickets", "actions": [
          {"name": "open", "effects": ["external"],
           "input_schema": {"type": "object"}, "output_schema": {"type": "object"}}
        ]}`, ``, 1)
	if dropped == narrowedAgents() {
		t.Fatal("fixture lacks the tickets capability")
	}
	writeFile(t, filepath.Join(fixture.directory, "agents.atpl.json"), dropped)
	_, digest = fixture.plan(t)
	fixture.mu.Lock()
	fixture.interfere = func() {
		current := fixture.registry.Live(t)["planner"]
		spec := fleet.Spec{
			ID: current.ID, AuthorizationDomain: current.AuthorizationDomain, Scopes: current.Scopes,
			ExecutorRef: "search-exec", LeaseExpiresAt: current.LeaseExpiresAt,
			Capabilities: current.Capabilities[:1],
		}
		if _, err := fixture.registry.Register(t, spec, current.Generation, "other-writer"); err != nil {
			t.Error(err)
		}
	}
	fixture.mu.Unlock()
	stdout, _, err = fixture.run(t, append([]string{"apply", fixture.directory, "-plan-digest", digest},
		fixture.registryArgs()...)...)
	if err == nil || !strings.Contains(err.Error(), "agents[id=planner]: registry refused (409 conflict)") ||
		!strings.Contains(err.Error(), "its live content changed since the plan") ||
		!strings.Contains(err.Error(), "stopped after 0 of 1 writes") {
		t.Fatalf("apply across a content change = %v\n%s", err, stdout)
	}
	if _, _, err := fixture.run(t, append([]string{"apply", fixture.directory, "-plan-digest", digest},
		fixture.registryArgs()...)...); err == nil || !strings.Contains(err.Error(), "plan digest mismatch") {
		t.Fatalf("apply of a superseded plan = %v", err)
	}
}

func TestPolicyApplyKeepsChildrenResolvableWhileNarrowing(t *testing.T) {
	fixture := newPolicyFixture(t)
	wide := strings.Replace(policyAgents, `"scopes": [{"source_id": "source-a", "policy_id": "policy"}],
      "executor_ref": "search-exec"`, searcherTwoScopes, 1)
	if wide == policyAgents {
		t.Fatal("fixture lacks the searcher scopes")
	}
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
	writeFile(t, filepath.Join(fixture.directory, "agents.atpl.json"), narrowedAgents())
	stdout, err := fixture.applyReviewed(t)
	if err != nil {
		t.Fatalf("apply: %v\n%s", err, stdout)
	}
	if strings.Index(stdout, "agents[id=searcher] narrow") > strings.Index(stdout, "agents[id=planner] narrow") {
		t.Fatalf("apply narrowed the parent first:\n%s", stdout)
	}
	resolvable("after apply")
}

func TestPolicyPlanShowsAnExecutorChangeApart(t *testing.T) {
	fixture := newPolicyFixture(t)
	if _, err := fixture.applyReviewed(t); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(fixture.directory, "agents.atpl.json"),
		strings.Replace(policyAgents, `"executor_ref": "search-exec"`, `"executor_ref": "remote-exec"`, 1))
	planned, _ := fixture.plan(t)
	if !strings.Contains(planned, "* agents[id=searcher] executor-change (live generation 1)") ||
		!strings.Contains(planned, "    ~ executor_ref: search-exec -> remote-exec") ||
		!strings.Contains(planned, "summary: 0 create, 0 narrow, 1 executor-change") {
		t.Fatalf("executor change plan:\n%s", planned)
	}
	if !strings.Contains(planned, "registry: "+fixture.server.URL) {
		t.Fatalf("plan does not name the registry:\n%s", planned)
	}
}

func TestExportGuardIgnoresGlobMetacharacters(t *testing.T) {
	fixture := newPolicyFixture(t)
	out := filepath.Join(t.TempDir(), "odd[dir")
	if err := os.Mkdir(out, 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(out, "kept.atpl.json"), policyExecutors)
	if _, _, err := fixture.run(t, append([]string{"export", "-out", out}, fixture.registryArgs()...)...); err == nil ||
		!strings.Contains(err.Error(), "already holds policy files") {
		t.Fatalf("export into a glob-named directory holding policy files = %v", err)
	}
}

func TestNormalizeEndpoint(t *testing.T) {
	for input, want := range map[string]string{
		"HTTPS://Registry.Example:443/":        "https://registry.example",
		"https://registry.example/base/":       "https://registry.example/base",
		"http://127.0.0.1:8080":                "http://127.0.0.1:8080",
		"http://[::1]:80/":                     "http://[::1]",
		"https://registry.example:8443/x/":     "https://registry.example:8443/x",
		"https://[fe80::1%25eth0]:8443/shoal/": "https://[fe80::1%25eth0]:8443/shoal",
	} {
		parsed, err := url.Parse(input)
		if err != nil {
			t.Fatal(err)
		}
		if got := normalizeEndpoint(parsed); got != want {
			t.Fatalf("normalizeEndpoint(%q) = %q, want %q", input, got, want)
		}
	}
}

func TestPolicyPlanRefusalsExitNonZero(t *testing.T) {
	fixture := newPolicyFixture(t)
	_, digest := fixture.plan(t)
	if _, _, err := fixture.run(t, append([]string{"apply", fixture.directory, "-plan-digest", digest},
		fixture.registryArgs()...)...); err != nil {
		t.Fatal(err)
	}
	widened := strings.Replace(policyAgents, `"executor_ref": "search-exec",`, `"executor_ref": "remote-exec",`, 1)
	widened = strings.Replace(widened, `{"name": "query", "inherit": true}`, `{"name": "query", "inherit": true}]},
        {"name": "tickets", "actions": [{"name": "open", "inherit": true}`, 1)
	writeFile(t, filepath.Join(fixture.directory, "agents.atpl.json"), widened)
	stdout, _, err := fixture.run(t, append([]string{"plan", fixture.directory}, fixture.registryArgs()...)...)
	if err == nil || !strings.Contains(err.Error(), "plan has 1 refused agent(s)") {
		t.Fatalf("plan with a widening = %v", err)
	}
	if !strings.Contains(stdout, "! agents[id=searcher] refused-widening (live generation 1)") ||
		!strings.Contains(stdout, "    + capabilities[name=tickets]") {
		t.Fatalf("widening plan:\n%s", stdout)
	}
	digest = planDigestLine.FindStringSubmatch(stdout)[1]
	if _, _, err := fixture.run(t, append([]string{"apply", fixture.directory, "-plan-digest", digest},
		fixture.registryArgs()...)...); err == nil || !strings.Contains(err.Error(), "nothing was applied") {
		t.Fatalf("apply of a refused plan = %v", err)
	}
}

func TestPolicyCommandArguments(t *testing.T) {
	fixture := newPolicyFixture(t)
	invalid := [][]string{
		{},
		{"unknown"},
		{"compile"},
		{"compile", "a", "b"},
		{"plan", fixture.directory},
		{"plan", fixture.directory, "-endpoint", "http://registry.example", "-token-file", fixture.tokenFile},
		{"plan", fixture.directory, "-endpoint", fixture.server.URL, "-token-file", filepath.Join(t.TempDir(), "missing")},
		{"apply", fixture.directory, "-endpoint", fixture.server.URL, "-token-file", fixture.tokenFile},
		{"export", "-endpoint", fixture.server.URL, "-token-file", fixture.tokenFile},
		// A bare "?" or "#" would turn every route into a query or fragment.
		{"plan", fixture.directory, "-endpoint", fixture.server.URL + "/x?", "-token-file", fixture.tokenFile},
		{"plan", fixture.directory, "-endpoint", fixture.server.URL + "?", "-token-file", fixture.tokenFile},
		{"plan", fixture.directory, "-endpoint", fixture.server.URL + "#", "-token-file", fixture.tokenFile},
		{"plan", fixture.directory, "-endpoint", "http://user:secret@127.0.0.1:1", "-token-file", fixture.tokenFile},
	}
	for _, args := range invalid {
		if _, _, err := fixture.run(t, args...); err == nil {
			t.Fatalf("policy %q succeeded", args)
		}
	}
	wrongToken := filepath.Join(t.TempDir(), "token")
	writeFile(t, wrongToken, "wrong")
	if _, _, err := fixture.run(t, "plan", fixture.directory, "-endpoint", fixture.server.URL,
		"-token-file", wrongToken); err == nil || !strings.Contains(err.Error(), "401 unauthorized") {
		t.Fatalf("wrong token = %v", err)
	}
	// Flags before the directory parse as well as after it.
	if _, _, err := fixture.run(t, append(append([]string{"plan"}, fixture.registryArgs()...),
		fixture.directory)...); err != nil {
		t.Fatalf("flags before the directory: %v", err)
	}
}

func TestAgentFileNamesAreStableAndSafe(t *testing.T) {
	if got := agentFileName("planner"); got != "agent.planner"+atpl.FileSuffix {
		t.Fatalf("plain name = %q", got)
	}
	upper, lower := agentFileName("Planner"), agentFileName("planner")
	if upper == lower || !strings.HasPrefix(upper, "agent-sha256.") {
		t.Fatalf("case-distinct IDs share a file name: %q %q", upper, lower)
	}
	if got := agentFileName("../escape"); strings.Contains(got, "/") {
		t.Fatalf("path separator in file name %q", got)
	}
	key := registrationKey("atpl:policy:v1:x", fleet.Spec{ID: "a", LeaseExpiresAt: time.Unix(1, 0)}, 0)
	if again := registrationKey("atpl:policy:v1:x", fleet.Spec{ID: "a", LeaseExpiresAt: time.Unix(1, 0)}, 0); again != key {
		t.Fatal("registration key is not deterministic")
	}
	if other := registrationKey("atpl:policy:v1:x", fleet.Spec{ID: "a", LeaseExpiresAt: time.Unix(1, 0)}, 1); other == key {
		t.Fatal("registration key ignores the expected generation")
	}
	if err := shoal.ValidateRequiredID("registration key", key); err != nil {
		t.Fatal(err)
	}
}
