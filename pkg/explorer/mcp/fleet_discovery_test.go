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

package mcp

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"reflect"
	"testing"
	"time"

	"github.com/phrocker/shoal-oss/pkg/explorer/auth"
	"github.com/phrocker/shoal-oss/pkg/explorer/fleet"
	"github.com/phrocker/shoal-oss/pkg/explorer/webapi"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

type recordingFleetLister struct {
	request fleet.ListRequest
	page    fleet.ListPage
	calls   int
}

func (l *recordingFleetLister) List(_ context.Context, request fleet.ListRequest) (fleet.ListPage, error) {
	l.calls++
	l.request = request
	return l.page, nil
}

func TestFleetDiscoveryUsesTrustedIdentityAndPublicProjection(t *testing.T) {
	now := time.Now().UTC()
	decision, err := auth.NewDecision(auth.DecisionConfig{
		Subject: "subject", Actor: "actor", AuthorizationDomain: []byte("domain"),
		AllowedOperations: []auth.Operation{auth.OperationAgentResolve},
		PolicyGeneration:  1, AuthenticationExpires: now.Add(time.Hour),
		RequestID: "trusted-request", CorrelationID: "trusted-correlation",
	})
	if err != nil {
		t.Fatal(err)
	}
	descriptor := fleet.Descriptor{
		ID: "agent", Generation: 3, Subject: "subject", Actor: "actor",
		AuthorizationDomain: []byte("domain"), ExecutorRef: "host-executor",
		Capabilities: []fleet.Capability{{Name: "operations", Actions: []fleet.Action{{
			Name: "deploy", InputSchema: json.RawMessage(`{"type":"object"}`),
			OutputSchema:     json.RawMessage(`{"type":"object"}`),
			Effects:          fleet.Effects{fleet.EffectMutatesExternal},
			RequiresApproval: true, RequiresAttestation: true,
		}}}},
		LeaseExpiresAt: now.Add(time.Hour), UpdatedAt: now,
	}
	lister := &recordingFleetLister{page: fleet.ListPage{
		Descriptors: []fleet.Descriptor{descriptor}, Next: []byte("opaque-next"),
	}}
	tool, err := NewFleetDiscoveryTool(lister, fleetToolResolver{decision: decision})
	if err != nil {
		t.Fatal(err)
	}
	if tool.(interface{ ToolAuthorizationOperation() auth.Operation }).ToolAuthorizationOperation() != auth.OperationAgentResolve {
		t.Fatal("discovery did not declare the registry resolve operation")
	}
	if !*tool.Tool().Annotations.ReadOnlyHint || toolMutates(tool.Tool()) {
		t.Fatal("discovery was not classified read-only")
	}
	if server, _ := newTestServer(t, &stubService{}, []OptionalToolProvider{tool}); server == nil {
		t.Fatal("discovery tool was not accepted by MCP server")
	}
	cursor := base64.RawURLEncoding.EncodeToString([]byte("opaque-cursor"))
	value, err := tool.Call(context.Background(), json.RawMessage(`{"limit":2,"cursor":"`+cursor+`"}`))
	if err != nil {
		t.Fatal(err)
	}
	request := lister.request
	if lister.calls != 1 || request.Context.RequestID != decision.RequestID() ||
		request.Context.CorrelationID != decision.CorrelationID() ||
		request.Context.ReasonCode != "mcp_discovery" ||
		request.Context.Deadline.Before(now) || request.Context.Deadline.After(now.Add(31*time.Second)) ||
		request.Limit != 2 || string(request.Cursor) != "opaque-cursor" {
		t.Fatalf("untrusted or malformed registry request: %#v", request)
	}
	result := value.(FleetDiscoveryResult)
	if result.Next != base64.RawURLEncoding.EncodeToString([]byte("opaque-next")) ||
		len(result.Agents) != 1 || !reflect.DeepEqual(result.Agents[0], webapi.EncodeFleetDescriptor(descriptor)) {
		t.Fatalf("result does not match HTTP projection: %#v", result)
	}
	encoded, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	if string(encoded) == "" || result.Agents[0].ID != base64.RawURLEncoding.EncodeToString([]byte("agent")) {
		t.Fatalf("agent ID not projected for public wire: %s", encoded)
	}
}

func TestFleetDiscoveryRejectsMalformedPaginationBeforeRegistryRead(t *testing.T) {
	decision, err := auth.NewDecision(auth.DecisionConfig{
		Subject: "subject", Actor: "actor", AuthorizationDomain: []byte("domain"),
		AllowedOperations: []auth.Operation{auth.OperationAgentResolve},
		PolicyGeneration:  1, AuthenticationExpires: time.Now().Add(time.Hour), RequestID: "request",
	})
	if err != nil {
		t.Fatal(err)
	}
	lister := &recordingFleetLister{}
	tool, err := NewFleetDiscoveryTool(lister, fleetToolResolver{decision: decision})
	if err != nil {
		t.Fatal(err)
	}
	for _, raw := range []string{
		`{"limit":17}`, `{"limit":-1}`, `{"cursor":"%%%"}`,
		`{"cursor":"YQ=="}`, `{"request_id":"forged"}`,
	} {
		if _, err := tool.Call(context.Background(), json.RawMessage(raw)); !shoal.IsErrorCode(err, shoal.ErrorInvalidArgument) {
			t.Fatalf("%s: expected invalid arguments, got %v", raw, err)
		}
	}
	if lister.calls != 0 {
		t.Fatalf("invalid arguments reached registry %d times", lister.calls)
	}
}

func TestFleetDiscoveryRequiresDependencies(t *testing.T) {
	var lister *recordingFleetLister
	if _, err := NewFleetDiscoveryTool(lister, fleetToolResolver{}); !shoal.IsErrorCode(err, shoal.ErrorInvalidArgument) {
		t.Fatalf("typed nil lister: %v", err)
	}
	if _, err := NewFleetDiscoveryTool(&recordingFleetLister{}, nil); !shoal.IsErrorCode(err, shoal.ErrorInvalidArgument) {
		t.Fatalf("nil resolver: %v", err)
	}
}

func TestFleetDiscoveryMCPCallRequiresResolveGrant(t *testing.T) {
	for _, allowed := range []bool{false, true} {
		operations := []auth.Operation{auth.OperationRetrieve}
		if allowed {
			operations = append(operations, auth.OperationAgentResolve)
		}
		decision, err := auth.NewDecision(auth.DecisionConfig{
			Subject: "subject", Actor: "actor", AuthorizationDomain: []byte("domain"),
			AllowedOperations: operations, PolicyGeneration: 1,
			AuthenticationExpires: time.Now().Add(time.Hour), RequestID: "template-request",
		})
		if err != nil {
			t.Fatal(err)
		}
		authority := auth.NewAuthority()
		lister := &recordingFleetLister{}
		tool, err := NewFleetDiscoveryTool(lister, authority.Resolver())
		if err != nil {
			t.Fatal(err)
		}
		server, err := newRecordedTestServer(t, Config{
			Service: &stubService{}, Authority: authority,
			Decisions: DecisionProviderFunc(func(context.Context) (auth.Decision, error) {
				return decision, nil
			}),
			OptionalTools:    []OptionalToolProvider{tool},
			requestIDFactory: sequentialRequestIDs(),
		})
		if err != nil {
			t.Fatal(err)
		}
		makeReady(t, server)
		response := callToolRequest(t, server, FleetDiscoverToolName, `{}`)
		if response.Error != nil {
			t.Fatalf("MCP protocol error: %+v", response.Error)
		}
		result := decodeToolResult(t, response)
		if result.IsError == allowed {
			t.Fatalf("allowed=%v, tool error=%v", allowed, result.IsError)
		}
		if !allowed && lister.calls != 0 {
			t.Fatal("unauthorized MCP caller reached registry")
		}
		if allowed && (lister.calls != 1 || lister.request.Context.RequestID == "template-request") {
			t.Fatalf("trusted request identity was not minted: %#v", lister.request)
		}
	}
}
