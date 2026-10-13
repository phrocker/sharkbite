// Licensed to the Apache Software Foundation (ASF) under one or more
// contributor license agreements. See the NOTICE file distributed with this
// work for additional information regarding copyright ownership.
package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/phrocker/shoal-oss/pkg/explorer"
	"github.com/phrocker/shoal-oss/pkg/explorer/auth"
	"github.com/phrocker/shoal-oss/pkg/explorer/fleet"
	"github.com/phrocker/shoal-oss/pkg/explorer/mcp"
	"github.com/phrocker/shoal-oss/pkg/explorer/teamoverview"
	"github.com/phrocker/shoal-oss/pkg/explorer/webapi"
	"github.com/phrocker/shoal-oss/pkg/graph"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

func TestTwoUserHTTPMCPDemoAcceptance(t *testing.T) {
	t.Parallel()
	root, err := os.MkdirTemp(".", ".demo-acceptance-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.RemoveAll(root); err != nil {
			t.Errorf("remove acceptance state: %v", err)
		}
	})
	harness := newDemoAcceptanceHarness(t, root)
	defer func() { harness.close() }()

	aliceWorkspace := harness.createWorkspace(t, "alice", "alice-workspace")
	bobWorkspace := harness.createWorkspace(t, "bob", "bob-workspace")
	if aliceWorkspace == bobWorkspace {
		t.Fatal("distinct workspace IDs collapsed")
	}
	if status := harness.getWorkspaceStatus(
		t, "bob", aliceWorkspace,
	); status != http.StatusNotFound {
		t.Fatalf("Bob read Alice workspace status = %d, want 404", status)
	}

	aliceSession := harness.initializeMCP(t, "alice", aliceWorkspace, 1)
	bobSession := harness.initializeMCP(t, "bob", bobWorkspace, 2)
	harness.initializeNotification(t, "alice", aliceWorkspace, aliceSession)
	harness.initializeNotification(t, "bob", bobWorkspace, bobSession)
	if aliceSession == bobSession {
		t.Fatal("two users received the same MCP session")
	}
	crossUserResponse := harness.rawMCP(
		t, "bob", bobWorkspace, aliceSession,
		`{"jsonrpc":"2.0","id":3,"method":"tools/list"}`,
	)
	if status := crossUserResponse.StatusCode; status != http.StatusNotFound {
		crossUserResponse.Body.Close()
		t.Fatalf("cross-user MCP session status = %d, want 404", status)
	}
	crossUserResponse.Body.Close()

	harness.callTool(t, "alice", aliceWorkspace, aliceSession, 4,
		mcp.ToolIngest, map[string]any{"files": []webapi.UploadFile{{
			Name: "alice-demo.md",
			Content: []byte(
				"# Shared marker\n\nAlice recorded sharedmarker planning evidence."),
		}}})
	harness.callTool(t, "bob", bobWorkspace, bobSession, 5,
		mcp.ToolIngest, map[string]any{"files": []webapi.UploadFile{{
			Name: "bob-demo.md",
			Content: []byte(
				"# Shared marker\n\nBob recorded sharedmarker delivery evidence."),
		}}})

	for _, user := range []struct {
		token, workspace, session string
		id                        int
	}{
		{"alice", aliceWorkspace, aliceSession, 6},
		{"bob", bobWorkspace, bobSession, 7},
	} {
		result := harness.callTool(
			t, user.token, user.workspace, user.session, user.id,
			mcp.ToolDocuments, map[string]any{})
		var documents webapi.DocumentsResponse
		if err := json.Unmarshal(result.StructuredContent, &documents); err != nil {
			t.Fatal(err)
		}
		if len(documents.Documents) < 2 {
			t.Fatalf(
				"%s retrieved %d shared documents, want at least 2",
				user.token, len(documents.Documents))
		}
	}

	harness.registerAgent(t, "alice")
	before := harness.teamOverview(t, "alice", aliceWorkspace)

	aliceDispatch := harness.callFleetTool(
		t, "alice", aliceWorkspace, aliceSession, 8,
		mcp.FleetDispatchToolName, "alice-queued", false)
	aliceInvoke := harness.callFleetTool(
		t, "alice", aliceWorkspace, aliceSession, 9,
		mcp.FleetInvokeToolName, "alice-completed", true)
	bobDispatch := harness.callFleetTool(
		t, "bob", bobWorkspace, bobSession, 10,
		mcp.FleetDispatchToolName, "bob-queued", false)

	if aliceDispatch.Action.RequestID == aliceInvoke.Action.RequestID {
		t.Fatalf(
			"Alice MCP calls reused request ID %q",
			aliceDispatch.Action.RequestID)
	}
	if aliceDispatch.Action.CorrelationID == "" ||
		aliceDispatch.Action.CorrelationID !=
			aliceInvoke.Action.CorrelationID {
		t.Fatalf(
			"Alice session correlations = %q, %q",
			aliceDispatch.Action.CorrelationID,
			aliceInvoke.Action.CorrelationID)
	}
	if aliceDispatch.Action.CorrelationID ==
		bobDispatch.Action.CorrelationID {
		t.Fatalf(
			"cross-user session correlation was shared: %q",
			aliceDispatch.Action.CorrelationID)
	}
	for _, action := range []fleet.ActionRecord{
		aliceDispatch.Action, aliceInvoke.Action, bobDispatch.Action,
	} {
		if strings.HasPrefix(
			string(action.CorrelationID), "caller-correlation-") {
			t.Fatalf("caller-authored correlation was trusted: %q",
				action.CorrelationID)
		}
	}
	if aliceInvoke.Action.State != fleet.DispatchSucceeded {
		t.Fatalf("simulated invoke state = %q", aliceInvoke.Action.State)
	}

	after := harness.teamOverview(t, "bob", bobWorkspace)
	if after.Metrics.ActionStates.Succeeded !=
		before.Metrics.ActionStates.Succeeded+1 ||
		after.Metrics.ActionStates.Queued !=
			before.Metrics.ActionStates.Queued+2 {
		t.Fatalf(
			"team metrics did not reflect Fleet lifecycle: before=%#v after=%#v",
			before.Metrics.ActionStates, after.Metrics.ActionStates)
	}
	if !overviewHasFleetEvidence(after, "alice-completed") {
		t.Fatalf("team overview omitted completed action evidence: %#v",
			after.Activities)
	}

	provenance := harness.provenance(t, "alice", aliceWorkspace)
	if !hasVisibleInteraction(provenance, "bob", true) {
		t.Fatalf(
			"Alice did not see Bob's authorized evidence-backed interaction: %#v",
			provenance.Interactions)
	}
	beforeOutside := provenanceIDs(provenance)
	outside := filepath.Join(root, "outside-mcp")
	if err := os.Mkdir(outside, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(
		filepath.Join(outside, "editor-save.txt"),
		[]byte("outside MCP editor fixture"), 0o600,
	); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(
		filepath.Join(outside, "terminal-command.txt"),
		[]byte("go test ./..."), 0o600,
	); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(
		filepath.Join(outside, "git-status.txt"),
		[]byte("working tree clean"), 0o600,
	); err != nil {
		t.Fatal(err)
	}
	afterOutside := provenanceIDs(
		harness.provenance(t, "alice", aliceWorkspace))
	if !reflect.DeepEqual(beforeOutside, afterOutside) {
		t.Fatalf(
			"outside-MCP fixture actions changed durable interactions:\nbefore=%v\nafter=%v",
			beforeOutside, afterOutside)
	}

	harness.close()
	harness = newDemoAcceptanceHarness(t, root)
	if status := harness.getWorkspaceStatus(
		t, "alice", aliceWorkspace,
	); status != http.StatusOK {
		t.Fatalf("Alice workspace after restart status = %d", status)
	}
	if status := harness.getWorkspaceStatus(
		t, "bob", bobWorkspace,
	); status != http.StatusOK {
		t.Fatalf("Bob workspace after restart status = %d", status)
	}
	restartedSession := harness.initializeMCP(
		t, "alice", aliceWorkspace, 11)
	harness.initializeNotification(
		t, "alice", aliceWorkspace, restartedSession)
	restartedRetrieve := harness.callTool(
		t, "alice", aliceWorkspace, restartedSession, 12,
		mcp.ToolDocuments, map[string]any{})
	var documents webapi.DocumentsResponse
	if err := json.Unmarshal(
		restartedRetrieve.StructuredContent, &documents,
	); err != nil {
		t.Fatal(err)
	}
	if len(documents.Documents) < 2 {
		t.Fatalf(
			"restart retrieval returned %d results",
			len(documents.Documents))
	}
	restartedOverview := harness.teamOverview(
		t, "alice", aliceWorkspace)
	if restartedOverview.Metrics.ActionStates !=
		after.Metrics.ActionStates {
		t.Fatalf(
			"Fleet metrics changed across restart: before=%#v after=%#v",
			after.Metrics.ActionStates,
			restartedOverview.Metrics.ActionStates)
	}
	restartedProvenance := harness.provenance(
		t, "alice", aliceWorkspace)
	if len(restartedProvenance.Interactions) <=
		len(provenance.Interactions) ||
		!hasVisibleInteraction(restartedProvenance, "bob", true) {
		t.Fatalf(
			"durable provenance after restart = %#v",
			restartedProvenance.Interactions)
	}
}

type demoAcceptanceHarness struct {
	root      string
	authority *auth.Authority
	opened    openedService
	server    *httptest.Server
	requests  atomic.Uint64
}

func newDemoAcceptanceHarness(
	t *testing.T,
	root string,
) *demoAcceptanceHarness {
	t.Helper()
	authority := auth.NewAuthority()
	opened, err := openService(context.Background(), serviceConfig{
		backend: "embedded", data: filepath.Join(root, "corpus"),
		policyDir: filepath.Join(root, "policy"),
		resolver:  authority.Resolver(), clock: time.Now,
		executors: configuredFleetExecutors{
			"demo": demoAcceptanceExecutor{},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	harness := &demoAcceptanceHarness{
		root: root, authority: authority, opened: opened,
	}
	authenticator := webapi.AuthenticatorFunc(func(
		request *http.Request,
	) (auth.Decision, error) {
		token := strings.TrimPrefix(
			request.Header.Get("Authorization"), "Bearer ")
		if token != "alice" && token != "bob" {
			return auth.Decision{}, shoal.NewError(
				shoal.ErrorUnauthorized, "unknown test token")
		}
		return demoAcceptanceDecision(
			t, shoal.ID(token),
			shoal.ID(token+"-request-"+
				strconv.FormatUint(harness.requests.Add(1), 10)),
			shoal.ID("caller-correlation-"+token),
		), nil
	})

	httpServer := httptest.NewUnstartedServer(nil)
	handler, err := webapi.NewAuthenticatedHandler(
		opened.service, authenticator, authority.Binder(),
		httpServer.Listener.Addr().String())
	if err != nil {
		opened.close()
		t.Fatal(err)
	}
	if err := handler.SetWorkspaceSettingsProvider(
		opened.settings); err != nil {
		opened.close()
		t.Fatal(err)
	}
	provenance, err := webapi.NewInteractionService(opened.client)
	if err != nil {
		opened.close()
		t.Fatal(err)
	}
	if err := handler.SetInteractionProvider(provenance); err != nil {
		opened.close()
		t.Fatal(err)
	}
	interactionTools, err := mcp.NewInteractionTools(provenance)
	if err != nil {
		opened.close()
		t.Fatal(err)
	}
	fleetTools, err := mcp.NewFleetDispatchTools(
		opened.fleetDispatch, authority.Resolver())
	if err != nil {
		opened.close()
		t.Fatal(err)
	}
	mcpServer, err := mcp.NewServer(mcp.Config{
		Service: opened.service, Authority: authority,
		Decisions:       mcp.DecisionProviderFunc(authority.Resolver().Resolve),
		InteractionSink: opened.client, Snapshots: opened.client,
		WorkspaceSettings: opened.settings,
		OptionalTools:     append(interactionTools, fleetTools...),
		ServerInfo: mcp.Implementation{
			Name: "demo-acceptance", Version: "1",
		},
	})
	if err != nil {
		opened.close()
		t.Fatal(err)
	}
	mcpHandler, err := mcp.NewHTTPHandler(mcp.HTTPConfig{
		Server: mcpServer,
		AllowedOrigins: []string{
			"http://" + httpServer.Listener.Addr().String(),
		},
		RequireWorkspaceSettings: true,
	})
	if err != nil {
		opened.close()
		t.Fatal(err)
	}
	if err := handler.MountAuthenticated("/mcp", mcpHandler); err != nil {
		opened.close()
		t.Fatal(err)
	}
	fleetHandler, err := webapi.NewFleetHandler(
		opened.fleetRegistry, opened.fleetDispatch)
	if err != nil {
		opened.close()
		t.Fatal(err)
	}
	if err := handler.MountAuthenticated(
		webapi.FleetRoutePrefix, fleetHandler,
	); err != nil {
		opened.close()
		t.Fatal(err)
	}
	boundDispatch, ok := opened.fleetDispatch.(*boundFleetDispatch)
	if !ok {
		opened.close()
		t.Fatal("embedded Fleet dispatch binding is unavailable")
	}
	overview, err := teamoverview.NewService(teamoverview.Config{
		Graph: demoTeamGraph{}, Agents: opened.fleetRegistry,
		Actions: boundDispatch.service, Interactions: opened.client,
		Resolver: authority.Resolver(), Clock: time.Now,
	})
	if err != nil {
		opened.close()
		t.Fatal(err)
	}
	teamHandler, err := webapi.NewTeamOverviewHandler(overview)
	if err != nil {
		opened.close()
		t.Fatal(err)
	}
	if err := handler.MountAuthenticated(
		webapi.TeamOverviewRoute, teamHandler,
	); err != nil {
		opened.close()
		t.Fatal(err)
	}
	httpServer.Config.Handler = handler
	httpServer.Start()
	harness.server = httpServer
	return harness
}

func (h *demoAcceptanceHarness) close() {
	if h.server != nil {
		h.server.Close()
		h.server = nil
	}
	if h.opened.close != nil {
		h.opened.close()
		h.opened.close = nil
	}
}

func (h *demoAcceptanceHarness) createWorkspace(
	t *testing.T,
	token string,
	workspace string,
) string {
	t.Helper()
	encoded := encodeDemoID(workspace)
	body := map[string]any{
		"expected_revision": 0,
		"mutation_id":       encodeDemoID(workspace + "-create"),
		"settings":          map[string]any{},
	}
	response := h.jsonRequest(
		t, http.MethodPut,
		"/api/v1/workspaces/"+encoded+"/settings",
		token, "", body)
	if response.StatusCode != http.StatusCreated {
		t.Fatalf(
			"create %s workspace status = %d: %s",
			token, response.StatusCode, response.body)
	}
	return encoded
}

func (h *demoAcceptanceHarness) getWorkspaceStatus(
	t *testing.T,
	token string,
	workspace string,
) int {
	t.Helper()
	return h.jsonRequest(
		t, http.MethodGet,
		"/api/v1/workspaces/"+workspace+"/settings",
		token, "", nil).StatusCode
}

func (h *demoAcceptanceHarness) initializeMCP(
	t *testing.T,
	token string,
	workspace string,
	id int,
) string {
	t.Helper()
	response := h.rawMCP(
		t, token, workspace, "",
		`{"jsonrpc":"2.0","id":`+strconv.Itoa(id)+
			`,"method":"initialize","params":{"protocolVersion":"`+
			mcp.ProtocolVersion+`","capabilities":{},`+
			`"clientInfo":{"name":"demo-acceptance","version":"1"}}}`)
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(response.Body)
		t.Fatalf("MCP initialize status = %d: %s",
			response.StatusCode, body)
	}
	session := response.Header.Get(mcp.SessionHeader)
	if session == "" {
		t.Fatal("MCP initialize omitted session ID")
	}
	return session
}

func (h *demoAcceptanceHarness) initializeNotification(
	t *testing.T,
	token string,
	workspace string,
	session string,
) {
	t.Helper()
	response := h.rawMCP(
		t, token, workspace, session,
		`{"jsonrpc":"2.0","method":"notifications/initialized"}`)
	defer response.Body.Close()
	if response.StatusCode != http.StatusAccepted {
		body, _ := io.ReadAll(response.Body)
		t.Fatalf("MCP initialized status = %d: %s",
			response.StatusCode, body)
	}
}

func (h *demoAcceptanceHarness) callTool(
	t *testing.T,
	token string,
	workspace string,
	session string,
	id int,
	name string,
	arguments any,
) mcp.ToolResult {
	t.Helper()
	rawArguments, err := json.Marshal(arguments)
	if err != nil {
		t.Fatal(err)
	}
	params, err := json.Marshal(mcp.CallToolParams{
		Name: name, Arguments: rawArguments,
	})
	if err != nil {
		t.Fatal(err)
	}
	response := h.rawMCP(
		t, token, workspace, session,
		`{"jsonrpc":"2.0","id":`+strconv.Itoa(id)+
			`,"method":"tools/call","params":`+string(params)+`}`)
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusOK {
		t.Fatalf("%s HTTP status = %d: %s",
			name, response.StatusCode, body)
	}
	var envelope struct {
		Result mcp.ToolResult `json:"result"`
		Error  *mcp.Error     `json:"error"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil {
		t.Fatal(err)
	}
	if envelope.Error != nil || envelope.Result.IsError {
		t.Fatalf("%s failed: %s", name, body)
	}
	return envelope.Result
}

func (h *demoAcceptanceHarness) callFleetTool(
	t *testing.T,
	token string,
	workspace string,
	session string,
	id int,
	name string,
	actionID string,
	invoke bool,
) mcp.FleetActionToolResult {
	t.Helper()
	arguments := map[string]any{
		"action_id":        encodeDemoID(actionID),
		"idempotency_key":  encodeDemoID(actionID + "-key"),
		"agent_id":         encodeDemoID("agent-alice"),
		"agent_generation": 1,
		"capability":       "work",
		"action":           "complete",
		"source_id":        encodeDemoID(string(workspaceSourceID)),
		"policy_id":        encodeDemoID(string(workspaceGrantPolicyID)),
		"object_id":        encodeDemoID("work-1"),
		"input":            map[string]any{"action_id": actionID},
		"reason_code":      "demo_acceptance",
		"deadline": time.Now().UTC().Add(time.Minute).
			Format(time.RFC3339Nano),
	}
	if invoke {
		arguments["claim_id"] = encodeDemoID(actionID + "-claim")
		arguments["lease_milliseconds"] = 30000
	}
	result := h.callTool(
		t, token, workspace, session, id, name, arguments)
	var decoded mcp.FleetActionToolResult
	if err := json.Unmarshal(result.StructuredContent, &decoded); err != nil {
		t.Fatal(err)
	}
	return decoded
}

func (h *demoAcceptanceHarness) registerAgent(
	t *testing.T,
	token string,
) {
	t.Helper()
	decision := demoAcceptanceDecision(
		t, shoal.ID(token), "agent-registration",
		"agent-registration-correlation")
	ctx, err := h.authority.Binder().Bind(
		context.Background(), decision)
	if err != nil {
		t.Fatal(err)
	}
	descriptor, err := h.opened.fleetRegistry.Register(
		ctx, fleet.RegisterRequest{
			Context: fleet.RequestContext{
				ReasonCode: "demo_fixture",
				Deadline:   time.Now().UTC().Add(time.Minute),
			},
			RegistrationKey: "agent-registration-key",
			Spec: fleet.Spec{
				ID:                  "agent-alice",
				AuthorizationDomain: workspaceAuthorizationDomain,
				Scopes: []fleet.Scope{{
					SourceID: workspaceSourceID,
					PolicyID: workspaceGrantPolicyID,
				}},
				ExecutorRef: "demo",
				Capabilities: []fleet.Capability{{
					Name: "work",
					Actions: []fleet.Action{{
						Name:         "complete",
						InputSchema:  json.RawMessage(`{"type":"object"}`),
						OutputSchema: json.RawMessage(`{"type":"object"}`),
					}},
				}},
				LeaseExpiresAt: time.Now().UTC().Add(time.Hour),
			},
		})
	if err != nil {
		t.Fatal(err)
	}
	if descriptor.Generation != 1 {
		t.Fatalf("agent generation = %d", descriptor.Generation)
	}
}

func (h *demoAcceptanceHarness) teamOverview(
	t *testing.T,
	token string,
	workspace string,
) teamoverview.Response {
	t.Helper()
	response := h.jsonRequest(
		t, http.MethodPost, webapi.TeamOverviewRoute,
		token, workspace, teamoverview.Request{
			TeamID: "team-1", SourceID: workspaceSourceID,
			PolicyID: workspaceGrantPolicyID, HistoryDays: 14, Limit: 100,
		})
	if response.StatusCode != http.StatusOK {
		t.Fatalf("team overview status = %d: %s",
			response.StatusCode, response.body)
	}
	var decoded teamoverview.Response
	if err := json.Unmarshal(response.body, &decoded); err != nil {
		t.Fatal(err)
	}
	return decoded
}

func (h *demoAcceptanceHarness) provenance(
	t *testing.T,
	token string,
	_ string,
) webapi.ProvenanceListResponse {
	t.Helper()
	response := h.jsonRequest(
		t, http.MethodGet, "/api/v1/provenance?limit=1000",
		token, "", nil)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("provenance status = %d: %s",
			response.StatusCode, response.body)
	}
	var decoded webapi.ProvenanceListResponse
	if err := json.Unmarshal(response.body, &decoded); err != nil {
		t.Fatal(err)
	}
	return decoded
}

type demoHTTPResponse struct {
	StatusCode int
	body       []byte
}

func (h *demoAcceptanceHarness) jsonRequest(
	t *testing.T,
	method string,
	path string,
	token string,
	workspace string,
	body any,
) demoHTTPResponse {
	t.Helper()
	var reader io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		reader = bytes.NewReader(encoded)
	}
	request, err := http.NewRequest(
		method, h.server.URL+path, reader)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer "+token)
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	if workspace != "" {
		request.Header.Set(webapi.WorkspaceIDHeader, workspace)
	}
	response, err := h.server.Client().Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	encoded, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	return demoHTTPResponse{
		StatusCode: response.StatusCode, body: encoded,
	}
}

func (h *demoAcceptanceHarness) rawMCP(
	t *testing.T,
	token string,
	workspace string,
	session string,
	body string,
) *http.Response {
	t.Helper()
	request, err := http.NewRequest(
		http.MethodPost, h.server.URL+"/mcp", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer "+token)
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "application/json, text/event-stream")
	request.Header.Set(webapi.WorkspaceIDHeader, workspace)
	if session != "" {
		request.Header.Set(mcp.SessionHeader, session)
		request.Header.Set(
			mcp.ProtocolVersionHeader, mcp.ProtocolVersion)
	}
	response, err := h.server.Client().Do(request)
	if err != nil {
		t.Fatal(err)
	}
	return response
}

func demoAcceptanceDecision(
	t *testing.T,
	subject shoal.ID,
	requestID shoal.ID,
	correlationID shoal.ID,
) auth.Decision {
	t.Helper()
	decision, err := auth.NewDecision(auth.DecisionConfig{
		Subject: subject, Actor: subject,
		ClientID:            "vscode-" + subject,
		AuthorizationDomain: workspaceAuthorizationDomain,
		AllowedOperations: []auth.Operation{
			auth.OperationIngest, auth.OperationList, auth.OperationRead,
			auth.OperationRetrieve, auth.OperationNeighborhood,
			auth.OperationConnect, auth.OperationValidate,
			auth.OperationWorkspaceSettingsRead,
			auth.OperationWorkspaceSettingsWrite,
			auth.OperationAgentRegister, auth.OperationAgentResolve,
			auth.OperationDispatch, auth.OperationInvoke,
			auth.OperationTeamOverviewRead,
		},
		PermittedSourceIDs:    [][]byte{workspaceSourceID},
		PermittedPolicyIDs:    [][]byte{workspaceGrantPolicyID},
		PolicyGeneration:      workspacePolicyGeneration,
		AuthenticationExpires: time.Now().UTC().Add(time.Hour),
		RequestID:             requestID,
		CorrelationID:         correlationID,
		AuditPurpose:          "issue 348 demo acceptance",
	})
	if err != nil {
		t.Fatal(err)
	}
	return decision
}

type demoAcceptanceExecutor struct{}

func (demoAcceptanceExecutor) Execute(
	ctx context.Context,
	invocation fleet.Invocation,
) (fleet.ExecutionResult, error) {
	if err := ctx.Err(); err != nil {
		return fleet.ExecutionResult{}, err
	}
	return fleet.ExecutionResult{
		Output: append(json.RawMessage(nil), invocation.Input...),
	}, nil
}

type demoTeamGraph struct{}

func (demoTeamGraph) BoundedNeighborhood(
	context.Context,
	explorer.BoundedNeighborhoodRequest,
) (explorer.BoundedNeighborhood, error) {
	return explorer.BoundedNeighborhood{
		Neighborhood: explorer.Neighborhood{
			Nodes: []graph.Node{
				{
					ID: "team-1", Kind: teamoverview.KindTeam,
					Properties: shoal.Metadata{"name": "Demo Team"},
				},
				{
					ID: "person-alice", Kind: teamoverview.KindPerson,
					Properties: shoal.Metadata{
						"name": "Alice", "subject_id": "alice",
					},
				},
				{
					ID: "person-bob", Kind: teamoverview.KindPerson,
					Properties: shoal.Metadata{
						"name": "Bob", "subject_id": "bob",
					},
				},
				{
					ID: "agent-node", Kind: teamoverview.KindAgent,
					Properties: shoal.Metadata{
						"name":     "Simulated Agent",
						"agent_id": "agent-alice",
					},
				},
				{
					ID: "work-1", Kind: teamoverview.KindWorkItem,
					Properties: shoal.Metadata{
						"title": "Complete demo", "status": "active",
					},
				},
			},
			Edges: []graph.Edge{
				{ID: "member-alice", From: "person-alice", To: "team-1", Type: teamoverview.RelationMemberOf},
				{ID: "member-bob", From: "person-bob", To: "team-1", Type: teamoverview.RelationMemberOf},
				{ID: "member-agent", From: "agent-node", To: "team-1", Type: teamoverview.RelationMemberOf},
				{ID: "member-work", From: "work-1", To: "team-1", Type: teamoverview.RelationMemberOf},
				{ID: "assigned-work", From: "work-1", To: "person-alice", Type: teamoverview.RelationAssignedTo},
			},
		},
	}, nil
}

func overviewHasFleetEvidence(
	response teamoverview.Response,
	actionID string,
) bool {
	encoded := encodeDemoID(actionID)
	for _, activity := range response.Activities {
		if activity.Kind != "fleet_action" {
			continue
		}
		for _, evidence := range activity.Evidence {
			if evidence.Kind == "fleet_action" &&
				evidence.ID == encoded {
				return true
			}
		}
	}
	return false
}

func hasVisibleInteraction(
	response webapi.ProvenanceListResponse,
	subject shoal.ID,
	requireEvidence bool,
) bool {
	encodedSubject := encodeDemoID(string(subject))
	for _, item := range response.Interactions {
		if item.Actor.SubjectID != encodedSubject {
			continue
		}
		if !requireEvidence || item.NodeCount > 0 ||
			item.EdgeCount > 0 {
			return true
		}
	}
	return false
}

func provenanceIDs(
	response webapi.ProvenanceListResponse,
) []string {
	result := make([]string, len(response.Interactions))
	for index, item := range response.Interactions {
		result[index] = item.SessionID
	}
	return result
}

func encodeDemoID(value string) string {
	return base64.RawURLEncoding.EncodeToString([]byte(value))
}
