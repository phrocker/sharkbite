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
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/phrocker/shoal-oss/pkg/explorer"
	"github.com/phrocker/shoal-oss/pkg/explorer/auth"
	"github.com/phrocker/shoal-oss/pkg/explorer/fleet"
	"github.com/phrocker/shoal-oss/pkg/explorer/teamoverview"
	"github.com/phrocker/shoal-oss/pkg/explorer/webapi"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

func TestBuildScenarioIsDeterministicAndComplete(t *testing.T) {
	value := validConfig("https://shoal.example.test")
	if err := value.validate(); err != nil {
		t.Fatal(err)
	}
	first, err := buildScenario(value)
	if err != nil {
		t.Fatal(err)
	}
	second, err := buildScenario(value)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(first, second) {
		t.Fatal("scenario generation is not deterministic")
	}
	if len(first.Plans) != 2 {
		t.Fatalf("plans = %d", len(first.Plans))
	}
	kinds := make(map[string]int)
	workItems := 0
	activities := make(map[string]struct{})
	actors := make(map[string]struct{})
	for _, plan := range first.Plans {
		for _, file := range plan.Files {
			var envelope struct {
				Schema  string            `json:"schema"`
				Kind    string            `json:"kind"`
				Records []json.RawMessage `json:"records"`
			}
			if err := json.Unmarshal(file.Content, &envelope); err != nil {
				t.Fatalf("decode %s: %v", file.Name, err)
			}
			if envelope.Schema != scenarioSchema {
				t.Fatalf("%s schema = %q", file.Name, envelope.Schema)
			}
			kinds[envelope.Kind] += len(envelope.Records)
			switch envelope.Kind {
			case "work_item":
				workItems += len(envelope.Records)
			case "activity":
				for _, raw := range envelope.Records {
					var activity struct {
						OccurredAt string `json:"occurred_at"`
					}
					if err := json.Unmarshal(raw, &activity); err != nil {
						t.Fatal(err)
					}
					activities[activity.OccurredAt[:10]] = struct{}{}
					var actor struct {
						ActorID string `json:"actor_id"`
					}
					if err := json.Unmarshal(raw, &actor); err != nil {
						t.Fatal(err)
					}
					actors[actor.ActorID] = struct{}{}
				}
			}
		}
	}
	if kinds["team"] != 1 || kinds["person"] != 2 ||
		kinds["simulated_agent"] != 2 {
		t.Fatalf("fixture kinds = %#v", kinds)
	}
	if workItems != workItemCount {
		t.Fatalf("work items = %d", workItems)
	}
	if len(activities) != activityDays {
		t.Fatalf("activity dates = %d", len(activities))
	}
	for _, actor := range []string{
		"person-a", "person-b", "agent-planning", "agent-delivery",
	} {
		if _, ok := actors[actor]; !ok {
			t.Fatalf("activity does not include actor %q: %#v", actor, actors)
		}
	}
	graphKinds := make(map[string]int)
	for _, node := range first.Nodes {
		graphKinds[node.Kind]++
	}
	if graphKinds["team"] != 1 || graphKinds["person"] != 2 ||
		graphKinds["agent"] != 2 || graphKinds["work_item"] != workItemCount ||
		graphKinds["activity"] != activityDays {
		t.Fatalf("graph kinds = %#v", graphKinds)
	}
}

func TestGeneratedGraphIsTeamOverviewCompatible(t *testing.T) {
	value := validConfig("https://shoal.example.test")
	if err := value.validate(); err != nil {
		t.Fatal(err)
	}
	generated, err := buildScenario(value)
	if err != nil {
		t.Fatal(err)
	}
	corpus, err := explorer.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer corpus.Close()
	snapshot, _ := corpus.Snapshot(context.Background())
	nodes := make([]explorer.GraphNodeSpec, len(generated.Nodes))
	for index, node := range generated.Nodes {
		key, _ := base64.RawURLEncoding.DecodeString(node.Key)
		nodes[index] = explorer.GraphNodeSpec{
			Key: key, Kind: node.Kind, Properties: node.Properties,
		}
	}
	relations := make([]explorer.GraphRelationSpec, len(generated.Relations))
	for index, relation := range generated.Relations {
		key, _ := base64.RawURLEncoding.DecodeString(relation.Key)
		from, _ := base64.RawURLEncoding.DecodeString(relation.From)
		to, _ := base64.RawURLEncoding.DecodeString(relation.To)
		relations[index] = explorer.GraphRelationSpec{
			Key: key, From: from, To: to, Type: relation.Type,
			Properties: relation.Properties,
		}
	}
	result, err := corpus.MaterializeGraph(context.Background(),
		explorer.GraphMaterializationRequest{
			Namespace:         []byte(value.Graph.Namespace),
			IdentityNamespace: []byte("test-policy"),
			MutationID:        "demo-mutation", ExpectedSnapshot: snapshot,
			Nodes: nodes, Relations: relations,
		})
	if err != nil {
		t.Fatal(err)
	}
	var teamID shoal.ID
	for _, node := range result.Nodes {
		if string(node.Key) == value.Team.ID {
			teamID = node.ID
		}
	}
	now := time.Date(2026, 9, 7, 12, 0, 0, 0, time.UTC)
	authority, err := auth.NewAuthorityWithClock(func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	decision, err := auth.NewDecision(auth.DecisionConfig{
		Subject: "tester", Actor: "tester", AuthorizationDomain: []byte(value.Graph.AuthorizationDomain),
		AllowedOperations:  []auth.Operation{auth.OperationTeamOverviewRead},
		PermittedSourceIDs: [][]byte{[]byte(value.Graph.SourceID)},
		PermittedPolicyIDs: [][]byte{[]byte(value.Graph.PolicyID)},
		PolicyGeneration:   1, AuthenticationExpires: now.Add(time.Hour),
		RequestID: "overview-request",
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, err := authority.Binder().Bind(context.Background(), decision)
	if err != nil {
		t.Fatal(err)
	}
	dependencies := overviewDependencies{agents: []fleet.Descriptor{
		{ID: shoal.ID(value.Users[0].Agent.ID), LeaseExpiresAt: now.Add(time.Hour)},
		{ID: shoal.ID(value.Users[1].Agent.ID), LeaseExpiresAt: now.Add(time.Hour)},
	}}
	service, err := teamoverview.NewService(teamoverview.Config{
		Graph: corpus, Agents: dependencies, Actions: dependencies,
		Interactions: dependencies, Resolver: authority.Resolver(),
		Clock: func() time.Time { return now },
	})
	if err != nil {
		t.Fatal(err)
	}
	overview, err := service.Overview(ctx, teamoverview.Request{
		TeamID: teamID, SourceID: []byte(value.Graph.SourceID),
		PolicyID: []byte(value.Graph.PolicyID), HistoryDays: activityDays,
		Limit: 100,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(overview.People) != 2 || len(overview.Agents) != 2 ||
		len(overview.WorkItems) != workItemCount ||
		len(overview.Activities) != activityDays {
		t.Fatalf("overview = people:%d agents:%d items:%d activities:%d %#v",
			len(overview.People), len(overview.Agents), len(overview.WorkItems),
			len(overview.Activities), overview.Activities)
	}
}

type overviewDependencies struct {
	agents []fleet.Descriptor
}

func (o overviewDependencies) List(context.Context, fleet.ListRequest) (fleet.ListPage, error) {
	return fleet.ListPage{Descriptors: o.agents}, nil
}
func (overviewDependencies) TeamActions(context.Context, fleet.TeamActionListRequest) (fleet.ActionPage, error) {
	return fleet.ActionPage{}, nil
}
func (overviewDependencies) InteractionRecordsPage(context.Context, shoal.ID, uint32) (explorer.InteractionRecordPage, error) {
	return explorer.InteractionRecordPage{}, nil
}

func TestProvisionIsIdempotentAndUsesDistinctOwners(t *testing.T) {
	state := newProvisionServerState()
	server := httptest.NewServer(http.HandlerFunc(state.serveHTTP))
	defer server.Close()
	value := validConfig(server.URL)
	if err := value.validate(); err != nil {
		t.Fatal(err)
	}
	generated, err := buildScenario(value)
	if err != nil {
		t.Fatal(err)
	}
	tokens := map[string]string{
		"SHOAL_DEMO_TOKEN_A":     "token-a",
		"SHOAL_DEMO_TOKEN_B":     "token-b",
		"SHOAL_DEMO_AGENT_KEY_A": "agent-key-a",
		"SHOAL_DEMO_AGENT_KEY_B": "agent-key-b",
	}
	provision := provisioner{
		client: server.Client(),
		getenv: func(name string) string { return tokens[name] },
	}
	first, err := provision.provision(context.Background(), value, generated)
	if err != nil {
		t.Fatal(err)
	}
	second, err := provision.provision(context.Background(), value, generated)
	if err != nil {
		t.Fatal(err)
	}
	if len(first.UserSummaries) != 2 || len(second.UserSummaries) != 2 {
		t.Fatalf("summaries = %#v %#v", first, second)
	}
	if first.GraphDisposition != "applied" ||
		second.GraphDisposition != "unchanged" ||
		first.OverviewPeople != 2 || first.OverviewAgents != 2 ||
		first.OverviewWorkItems != workItemCount {
		t.Fatalf("graph summaries = %#v %#v", first, second)
	}
	for _, user := range first.UserSummaries {
		for name, disposition := range user.Files {
			if disposition != "applied" {
				t.Fatalf("first %s disposition = %q", name, disposition)
			}
		}
	}
	for _, user := range second.UserSummaries {
		for name, disposition := range user.Files {
			if disposition != "unchanged" {
				t.Fatalf("second %s disposition = %q", name, disposition)
			}
		}
	}
	state.mu.Lock()
	defer state.mu.Unlock()
	if len(state.settings) != 2 {
		t.Fatalf("workspace settings = %d", len(state.settings))
	}
	if len(state.files) != 9 {
		t.Fatalf("fixture files = %d", len(state.files))
	}
	if state.identityCalls["token-a"] != 2 ||
		state.identityCalls["token-b"] != 2 {
		t.Fatalf("identity calls = %#v", state.identityCalls)
	}
	for token, workspaces := range state.tokenWorkspaces {
		if len(workspaces) != 1 {
			t.Fatalf("%s used workspaces %#v", token, workspaces)
		}
	}
	if reflect.DeepEqual(
		state.tokenWorkspaces["token-a"], state.tokenWorkspaces["token-b"]) {
		t.Fatal("two users used the same workspace header")
	}
}

// TestProvisionRefusesAHistoricalFixture pins that a fixture the server
// reports as matching an earlier, non-current revision (#674) fails the seed
// instead of being recorded as seeded.
func TestProvisionRefusesAHistoricalFixture(t *testing.T) {
	state := newProvisionServerState()
	state.fileDisposition = "historical"
	server := httptest.NewServer(http.HandlerFunc(state.serveHTTP))
	defer server.Close()
	value := validConfig(server.URL)
	if err := value.validate(); err != nil {
		t.Fatal(err)
	}
	generated, err := buildScenario(value)
	if err != nil {
		t.Fatal(err)
	}
	tokens := map[string]string{
		"SHOAL_DEMO_TOKEN_A":     "token-a",
		"SHOAL_DEMO_TOKEN_B":     "token-b",
		"SHOAL_DEMO_AGENT_KEY_A": "agent-key-a",
		"SHOAL_DEMO_AGENT_KEY_B": "agent-key-b",
	}
	provision := provisioner{
		client: server.Client(),
		getenv: func(name string) string { return tokens[name] },
	}
	_, err = provision.provision(context.Background(), value, generated)
	if err == nil || !strings.Contains(err.Error(), "earlier, non-current revision") {
		t.Fatalf("provision with a historical fixture = %v", err)
	}
}

func TestConfigValidation(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*config)
		want   string
	}{
		{
			name: "public HTTP",
			mutate: func(value *config) {
				value.BaseURL = "http://shoal.example.test"
			},
			want: "plain HTTP",
		},
		{
			name: "one user",
			mutate: func(value *config) {
				value.Users = value.Users[:1]
			},
			want: "exactly two",
		},
		{
			name: "duplicate subjects",
			mutate: func(value *config) {
				value.Users[1].OIDCSubject = value.Users[0].OIDCSubject
			},
			want: "distinct oidc_subject_id",
		},
		{
			// #546: shoal-explore-web refuses a sub containing '#'.
			name: "subject containing a hash",
			mutate: func(value *config) {
				value.Users[1].OIDCSubject = "0123456789abcdef#subject-a"
			},
			want: "must not contain '#'",
		},
		{
			name: "issuer with an empty fragment",
			mutate: func(value *config) {
				value.OIDCIssuer = "https://identity.example.test/#"
			},
			want: "HTTPS issuer",
		},
		{
			name: "duplicate workspaces",
			mutate: func(value *config) {
				value.Users[1].WorkspaceID = value.Users[0].WorkspaceID
			},
			want: "distinct workspace_id",
		},
		{
			name: "bad token environment",
			mutate: func(value *config) {
				value.Users[0].TokenEnv = "token-a"
			},
			want: "uppercase environment variable",
		},
		{
			name: "non UTC start",
			mutate: func(value *config) {
				value.ScenarioStart = "2026-09-01T09:00:00-04:00"
			},
			want: "UTC timestamp",
		},
		{
			name: "budget exceeds bound",
			mutate: func(value *config) {
				value.Workspace.GraphDepth = 5
			},
			want: "graph_depth",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			value := validConfig("https://shoal.example.test")
			test.mutate(&value)
			err := value.validate()
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("validation error = %v, want %q", err, test.want)
			}
		})
	}
}

func TestLoadConfigRejectsUnknownAndTrailingValues(t *testing.T) {
	value := validConfig("https://shoal.example.test")
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	for name, content := range map[string][]byte{
		"unknown": bytes.Replace(
			encoded, []byte(`"base_url"`),
			[]byte(`"unknown":true,"base_url"`), 1),
		"trailing": append(append([]byte(nil), encoded...), []byte(` {}`)...),
	} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.json")
			if err := os.WriteFile(path, content, 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := loadConfig(path); err == nil {
				t.Fatal("invalid config was accepted")
			}
		})
	}
}

func TestCheckedInExamplesAreValidAndContainNoCredential(t *testing.T) {
	configPath := filepath.Join(
		"..", "..", "deploy", "shoal-explore-web",
		"demo-scenario.json.example",
	)
	if _, err := loadConfig(configPath); err != nil {
		t.Fatalf("example scenario config: %v", err)
	}
	mcpPath := filepath.Join("..", "..", ".vscode", "mcp.json.example")
	content, err := os.ReadFile(mcpPath)
	if err != nil {
		t.Fatal(err)
	}
	var document map[string]any
	if err := json.Unmarshal(content, &document); err != nil {
		t.Fatalf("example MCP config is not JSON: %v", err)
	}
	text := string(content)
	for _, want := range []string{
		"${env:SHOAL_MCP_URL}",
		"Bearer ${env:SHOAL_MCP_BEARER_TOKEN}",
		"${env:SHOAL_MCP_WORKSPACE_ID}",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("example MCP config missing %q", want)
		}
	}
}

func validConfig(baseURL string) config {
	return config{
		BaseURL:       baseURL,
		OIDCIssuer:    "https://identity.example.test",
		ScenarioStart: "2026-08-25T00:00:00Z",
		Team: teamConfig{
			ID: "demo-team", Name: "Example Delivery Team",
		},
		Graph: graphConfig{
			Namespace: "demo-team-graph", AuthorizationDomain: "demo-domain",
			SourceID: "demo-source", PolicyID: "demo-policy",
		},
		Workspace: workspaceConfig{
			RetrievalTopK: 20, GraphDepth: 3, GraphFanout: 20,
			GraphNodes: 100, OutputBytes: 1048576,
		},
		Users: []userConfig{
			{
				PersonID: "person-a", DisplayName: "Alex Morgan",
				TeamRole: "maintainer", OIDCSubject: "subject-a",
				WorkspaceID: "demo-workspace-a", TokenEnv: "SHOAL_DEMO_TOKEN_A",
				Agent: agentConfig{
					ID: "agent-planning", Name: "Planning Agent",
					Capability:         "planning",
					Skills:             []string{"triage", "dependency-analysis"},
					ExecutorRef:        "simulated-planning",
					RegistrationKeyEnv: "SHOAL_DEMO_AGENT_KEY_A",
				},
			},
			{
				PersonID: "person-b", DisplayName: "Sam Rivera",
				TeamRole: "developer", OIDCSubject: "subject-b",
				WorkspaceID: "demo-workspace-b", TokenEnv: "SHOAL_DEMO_TOKEN_B",
				Agent: agentConfig{
					ID: "agent-delivery", Name: "Delivery Agent",
					Capability:         "delivery",
					Skills:             []string{"validation", "release-readiness"},
					ExecutorRef:        "simulated-delivery",
					RegistrationKeyEnv: "SHOAL_DEMO_AGENT_KEY_B",
				},
			},
		},
	}
}

type provisionServerState struct {
	mu              sync.Mutex
	settings        map[string][]byte
	files           map[string][]byte
	identityCalls   map[string]int
	tokenWorkspaces map[string]map[string]struct{}
	graph           []byte
	agents          map[string]int64
	// fileDisposition, when set, replaces every upload's disposition.
	fileDisposition string
}

func newProvisionServerState() *provisionServerState {
	return &provisionServerState{
		settings: make(map[string][]byte), files: make(map[string][]byte),
		identityCalls: make(map[string]int),
		tokenWorkspaces: map[string]map[string]struct{}{
			"token-a": {}, "token-b": {},
		},
		agents: make(map[string]int64),
	}
}

func (s *provisionServerState) serveHTTP(
	writer http.ResponseWriter,
	request *http.Request,
) {
	token := strings.TrimPrefix(request.Header.Get("Authorization"), "Bearer ")
	switch {
	case request.Method == http.MethodGet &&
		request.URL.Path == "/api/v1/identity":
		s.identity(writer, token)
	case request.Method == http.MethodPut &&
		strings.HasPrefix(request.URL.Path, "/api/v1/workspaces/"):
		s.workspace(writer, request, token)
	case request.Method == http.MethodPost &&
		request.URL.Path == "/api/v1/ingest":
		s.ingest(writer, request, token)
	case request.Method == http.MethodPost &&
		request.URL.Path == "/api/v1/documents":
		writeTestJSON(writer, http.StatusOK, map[string]any{
			"snapshot": map[string]any{
				"id": "snapshot-1", "as_of": "2026-09-07T00:00:00Z",
				"frontier": "1",
			}, "documents": []any{},
		})
	case request.Method == http.MethodPost &&
		request.URL.Path == "/api/v1/graph/materialize":
		s.graphMaterialize(writer, request)
	case request.Method == http.MethodPost &&
		request.URL.Path == "/api/v1/fleet/agents":
		s.registerAgent(writer, request)
	case request.Method == http.MethodPost &&
		strings.HasSuffix(request.URL.Path, "/resolve"):
		s.resolveAgent(writer, request)
	case request.Method == http.MethodPost &&
		strings.HasSuffix(request.URL.Path, "/heartbeat"):
		s.heartbeatAgent(writer, request)
	case request.Method == http.MethodPost &&
		request.URL.Path == "/api/v1/team/overview":
		writeTestJSON(writer, http.StatusOK, map[string]any{
			"people": make([]any, 2), "agents": make([]any, 2),
			"work_items": make([]any, workItemCount),
			"activities": make([]any, activityDays),
		})
	default:
		http.NotFound(writer, request)
	}
}

func (s *provisionServerState) identity(
	writer http.ResponseWriter,
	token string,
) {
	subjects := map[string]string{
		"token-a": "oidc:https://identity.example.test#subject-a",
		"token-b": "oidc:https://identity.example.test#subject-b",
	}
	subject, ok := subjects[token]
	if !ok {
		http.Error(writer, "unauthorized", http.StatusUnauthorized)
		return
	}
	s.mu.Lock()
	s.identityCalls[token]++
	s.mu.Unlock()
	writeTestJSON(writer, http.StatusOK, map[string]any{
		"authenticated": true, "subject": subject,
		"operations": []string{
			"ingest", "graph_materialize", "workspace_settings_read",
			"workspace_settings_write", "agent_register", "agent_heartbeat",
			"agent_resolve", "team_overview_read",
		},
	})
}

func (s *provisionServerState) graphMaterialize(
	writer http.ResponseWriter, request *http.Request,
) {
	body, err := io.ReadAll(request.Body)
	if err != nil {
		http.Error(writer, err.Error(), http.StatusBadRequest)
		return
	}
	var input webapi.GraphMaterializeRequest
	if err := json.Unmarshal(body, &input); err != nil {
		http.Error(writer, err.Error(), http.StatusBadRequest)
		return
	}
	s.mu.Lock()
	found := len(s.graph) != 0
	if found && !bytes.Equal(s.graph, body) {
		s.mu.Unlock()
		http.Error(writer, "divergent graph", http.StatusConflict)
		return
	}
	s.graph = append([]byte(nil), body...)
	s.mu.Unlock()
	nodes := make([]webapi.GraphMaterializeIdentity, len(input.Nodes))
	for index, node := range input.Nodes {
		key, _ := base64.RawURLEncoding.DecodeString(node.Key)
		nodes[index] = webapi.GraphMaterializeIdentity{
			Key: node.Key, ID: graphKey("node-" + string(key)),
		}
	}
	disposition := "applied"
	if found {
		disposition = "unchanged"
	}
	writeTestJSON(writer, http.StatusOK, map[string]any{
		"materialization_id": graphKey("materialization"), "mutation_id": input.MutationID,
		"disposition": disposition, "snapshot": input.Snapshot, "nodes": nodes,
	})
}

func (s *provisionServerState) registerAgent(
	writer http.ResponseWriter, request *http.Request,
) {
	var input struct {
		Descriptor struct {
			ID string `json:"id"`
		} `json:"descriptor"`
	}
	if err := json.NewDecoder(request.Body).Decode(&input); err != nil {
		http.Error(writer, err.Error(), http.StatusBadRequest)
		return
	}
	s.mu.Lock()
	s.agents[input.Descriptor.ID] = 1
	s.mu.Unlock()
	writeTestJSON(writer, http.StatusCreated, map[string]any{
		"id": input.Descriptor.ID, "generation": 1,
		"lease_expires_at": time.Now().UTC().Add(time.Hour),
	})
}

func (s *provisionServerState) resolveAgent(
	writer http.ResponseWriter, request *http.Request,
) {
	parts := strings.Split(request.URL.Path, "/")
	id := parts[len(parts)-2]
	s.mu.Lock()
	generation, ok := s.agents[id]
	s.mu.Unlock()
	if !ok {
		http.NotFound(writer, request)
		return
	}
	writeTestJSON(writer, http.StatusOK, map[string]any{
		"id": id, "generation": generation,
		"lease_expires_at": time.Now().UTC().Add(time.Hour),
	})
}

func (s *provisionServerState) heartbeatAgent(
	writer http.ResponseWriter, request *http.Request,
) {
	parts := strings.Split(request.URL.Path, "/")
	id := parts[len(parts)-2]
	s.mu.Lock()
	s.agents[id]++
	generation := s.agents[id]
	s.mu.Unlock()
	writeTestJSON(writer, http.StatusOK, map[string]any{
		"id": id, "generation": generation,
		"lease_expires_at": time.Now().UTC().Add(time.Hour),
	})
}

func (s *provisionServerState) workspace(
	writer http.ResponseWriter,
	request *http.Request,
	token string,
) {
	if token != "token-a" && token != "token-b" {
		http.Error(writer, "unauthorized", http.StatusUnauthorized)
		return
	}
	body, err := io.ReadAll(request.Body)
	if err != nil {
		http.Error(writer, err.Error(), http.StatusBadRequest)
		return
	}
	key := request.URL.Path
	s.mu.Lock()
	existing, found := s.settings[key]
	if found && !bytes.Equal(existing, body) {
		s.mu.Unlock()
		http.Error(writer, "divergent replay", http.StatusConflict)
		return
	}
	s.settings[key] = append([]byte(nil), body...)
	s.mu.Unlock()
	status := http.StatusCreated
	if found {
		status = http.StatusOK
	}
	writeTestJSON(writer, status, map[string]any{"revision": 1})
}

func (s *provisionServerState) ingest(
	writer http.ResponseWriter,
	request *http.Request,
	token string,
) {
	workspaceID := request.Header.Get("Shoal-Workspace-ID")
	if token == "" || workspaceID == "" ||
		request.Header.Get("X-Shoal-Workspace-Request") != "1" {
		http.Error(writer, "missing headers", http.StatusUnauthorized)
		return
	}
	reader, err := request.MultipartReader()
	if err != nil {
		http.Error(writer, err.Error(), http.StatusBadRequest)
		return
	}
	type result struct {
		Name        string `json:"name"`
		Disposition string `json:"disposition"`
	}
	results := []result{}
	for {
		part, err := reader.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			http.Error(writer, err.Error(), http.StatusBadRequest)
			return
		}
		content, err := io.ReadAll(part)
		if err != nil {
			http.Error(writer, err.Error(), http.StatusBadRequest)
			return
		}
		name := part.FileName()
		key := token + "/" + name
		s.mu.Lock()
		existing, found := s.files[key]
		if found && !bytes.Equal(existing, content) {
			s.mu.Unlock()
			http.Error(writer, "divergent upload", http.StatusConflict)
			return
		}
		s.files[key] = append([]byte(nil), content...)
		s.tokenWorkspaces[token][workspaceID] = struct{}{}
		forced := s.fileDisposition
		s.mu.Unlock()
		disposition := "applied"
		if found {
			disposition = "unchanged"
		}
		if forced != "" {
			disposition = forced
		}
		results = append(results, result{
			Name: name, Disposition: disposition,
		})
	}
	writeTestJSON(writer, http.StatusOK, map[string]any{"files": results})
}

func writeTestJSON(writer http.ResponseWriter, status int, value any) {
	writer.Header().Set("Content-Type", "application/json")
	writer.WriteHeader(status)
	_ = json.NewEncoder(writer).Encode(value)
}

func TestWorkspaceHeaderEncoding(t *testing.T) {
	value := validConfig("https://shoal.example.test")
	if err := value.validate(); err != nil {
		t.Fatal(err)
	}
	want := base64.RawURLEncoding.EncodeToString([]byte("demo-workspace-a"))
	if value.Users[0].workspaceKey != want {
		t.Fatalf("workspace key = %q, want %q", value.Users[0].workspaceKey, want)
	}
}

func TestIngestMultipartStaysWithinPublicFileBound(t *testing.T) {
	value := validConfig("https://shoal.example.test")
	if err := value.validate(); err != nil {
		t.Fatal(err)
	}
	generated, err := buildScenario(value)
	if err != nil {
		t.Fatal(err)
	}
	for index, plan := range generated.Plans {
		if len(plan.Files) > 8 {
			t.Fatalf("plan %d contains %d files", index, len(plan.Files))
		}
		var body bytes.Buffer
		writer := multipart.NewWriter(&body)
		for _, file := range plan.Files {
			part, err := writer.CreateFormFile("files", file.Name)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := fmt.Fprint(part, string(file.Content)); err != nil {
				t.Fatal(err)
			}
		}
		if err := writer.Close(); err != nil {
			t.Fatal(err)
		}
	}
}
