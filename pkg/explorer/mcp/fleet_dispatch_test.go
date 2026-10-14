// Licensed to the Apache Software Foundation (ASF) under one or more
// contributor license agreements. See the NOTICE file distributed with this
// work for additional information regarding copyright ownership.
package mcp

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"testing"
	"time"

	"github.com/phrocker/shoal-oss/pkg/explorer/auth"
	"github.com/phrocker/shoal-oss/pkg/explorer/fleet"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

func TestFleetDispatchToolsUseTrustedDecisionAndStableKeys(t *testing.T) {
	now := time.Date(2026, 9, 6, 4, 0, 0, 0, time.UTC)
	decision, err := auth.NewDecision(auth.DecisionConfig{
		Subject: "subject", Actor: "actor", AuthorizationDomain: []byte("domain"),
		AllowedOperations: []auth.Operation{
			auth.OperationDispatch, auth.OperationInvoke, auth.OperationDelegate,
		},
		PermittedSourceIDs: [][]byte{[]byte("source")},
		PermittedPolicyIDs: [][]byte{[]byte("policy")},
		PolicyGeneration:   1, AuthenticationExpires: now.AddDate(1, 0, 0),
		RequestID: "trusted-request", CorrelationID: "trusted-correlation",
	})
	if err != nil {
		t.Fatal(err)
	}
	provider := &fleetToolService{}
	tools, err := NewFleetDispatchTools(
		provider, fleetToolResolver{decision: decision})
	if err != nil {
		t.Fatal(err)
	}
	if len(tools) != 2 {
		t.Fatalf("tools = %d", len(tools))
	}
	if server, _ := newTestServer(t, &stubService{}, tools); server == nil {
		t.Fatal("fleet tools were not accepted by MCP server")
	}
	dispatch := tools[0].(*fleetDispatchTool)
	if dispatch.ToolAuthorizationOperation() != auth.OperationDispatch {
		t.Fatalf("dispatch operation = %q", dispatch.ToolAuthorizationOperation())
	}
	raw := fleetToolArguments(now, false)
	_, err = dispatch.Call(context.Background(), raw)
	if err != nil {
		t.Fatal(err)
	}
	if provider.enqueued.Context.RequestID != decision.RequestID() ||
		provider.enqueued.Context.CorrelationID != decision.CorrelationID() ||
		string(provider.enqueued.IdempotencyKey) != "stable-key" {
		t.Fatalf("enqueue request = %#v", provider.enqueued)
	}
	invoke := tools[1].(*fleetDispatchTool)
	if invoke.ToolAuthorizationOperation() != auth.OperationInvoke {
		t.Fatalf("invoke operation = %q", invoke.ToolAuthorizationOperation())
	}
	if _, err := invoke.Call(context.Background(), fleetToolArguments(now, true)); err != nil {
		t.Fatal(err)
	}
	if string(provider.invoked.ClaimID) != "claim" ||
		provider.invoked.Lease != time.Second {
		t.Fatalf("invoke request = %#v", provider.invoked)
	}
}

func TestFleetDispatchToolRejectsUntrustedIdentityAndClaimFields(t *testing.T) {
	decision, err := auth.NewDecision(auth.DecisionConfig{
		Subject: "subject", Actor: "actor", AuthorizationDomain: []byte("domain"),
		AllowedOperations: []auth.Operation{auth.OperationDispatch},
		PolicyGeneration:  1, AuthenticationExpires: time.Now().Add(time.Hour),
		RequestID: "request",
	})
	if err != nil {
		t.Fatal(err)
	}
	tools, err := NewFleetDispatchTools(
		&fleetToolService{}, fleetToolResolver{decision: decision})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tools[0].Call(
		context.Background(), fleetToolArguments(time.Now(), false),
	); !shoal.IsErrorCode(err, shoal.ErrorUnauthorized) {
		t.Fatalf("missing trusted correlation = %v", err)
	}
}

func TestFleetDispatchToolAcceptsScalarInput(t *testing.T) {
	tool := (&fleetDispatchTool{}).Tool()
	schema, err := parseOptionalToolInputSchema(tool.InputSchema)
	if err != nil {
		t.Fatal(err)
	}
	var arguments map[string]any
	if err := json.Unmarshal(fleetToolArguments(time.Now(), false), &arguments); err != nil {
		t.Fatal(err)
	}
	arguments["input"] = "scalar input"
	raw, err := json.Marshal(arguments)
	if err != nil {
		t.Fatal(err)
	}
	if err := validateOptionalToolArguments(raw, schema); err != nil {
		t.Fatalf("scalar input rejected: %v", err)
	}
}

func fleetToolArguments(now time.Time, invoke bool) json.RawMessage {
	value := map[string]any{
		"action_id":        encodeFleetToolBytes([]byte("action")),
		"idempotency_key":  encodeFleetToolBytes([]byte("stable-key")),
		"agent_id":         encodeFleetToolBytes([]byte("agent")),
		"agent_generation": 1,
		"capability":       "search", "action": "query",
		"source_id":   encodeFleetToolBytes([]byte("source")),
		"policy_id":   encodeFleetToolBytes([]byte("policy")),
		"object_id":   encodeFleetToolBytes([]byte("object")),
		"input":       map[string]any{"value": 1},
		"reason_code": "operator_request",
		"deadline":    now.Add(time.Minute).Format(time.RFC3339Nano),
	}
	if invoke {
		value["claim_id"] = encodeFleetToolBytes([]byte("claim"))
		value["lease_milliseconds"] = 1000
	}
	raw, _ := json.Marshal(value)
	return raw
}

func encodeFleetToolBytes(value []byte) string {
	return base64.RawURLEncoding.EncodeToString(value)
}

type fleetToolResolver struct{ decision auth.Decision }

func (r fleetToolResolver) Resolve(context.Context) (auth.Decision, error) {
	return r.decision, nil
}

type fleetToolService struct {
	enqueued fleet.EnqueueRequest
	invoked  fleet.InvokeRequest
}

func (s *fleetToolService) Enqueue(
	_ context.Context, request fleet.EnqueueRequest,
) (fleet.ActionRecord, error) {
	s.enqueued = request
	return fleet.ActionRecord{ID: request.ID, Evidence: []fleet.EvidenceRef{}}, nil
}
func (s *fleetToolService) Invoke(
	_ context.Context, request fleet.InvokeRequest,
) (fleet.ActionRecord, error) {
	s.invoked = request
	return fleet.ActionRecord{ID: request.Enqueue.ID}, nil
}
func (*fleetToolService) Claim(context.Context, fleet.ClaimRequest) (fleet.ActionRecord, error) {
	return fleet.ActionRecord{}, nil
}
func (*fleetToolService) CompleteClaim(context.Context, fleet.CompletionRequest) (fleet.ActionRecord, error) {
	return fleet.ActionRecord{}, nil
}
func (*fleetToolService) Cancel(context.Context, fleet.CancelRequest) (fleet.ActionRecord, error) {
	return fleet.ActionRecord{}, nil
}
func (*fleetToolService) Status(context.Context, fleet.StatusRequest) (fleet.ActionRecord, error) {
	return fleet.ActionRecord{}, nil
}
func (*fleetToolService) Pull(context.Context, fleet.PullActionsRequest) (fleet.ActionPage, error) {
	return fleet.ActionPage{}, nil
}

func (*fleetToolService) ExtendClaim(
	context.Context, fleet.ExtendRequest,
) (fleet.ActionRecord, error) {
	return fleet.ActionRecord{}, nil
}

func (*fleetToolService) ReportAmbiguity(
	context.Context, fleet.AmbiguityRequest,
) (fleet.ActionRecord, error) {
	return fleet.ActionRecord{}, nil
}
