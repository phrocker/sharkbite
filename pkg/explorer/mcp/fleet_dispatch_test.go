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
	if len(tools) != 3 {
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
	dispatchResult, err := dispatch.Call(context.Background(), raw)
	if err != nil {
		t.Fatal(err)
	}
	assertPublicFleetReceipt(t, dispatchResult)
	if provider.enqueued.Context.RequestID != decision.RequestID() ||
		provider.enqueued.Context.CorrelationID != decision.CorrelationID() ||
		string(provider.enqueued.IdempotencyKey) != "stable-key" {
		t.Fatalf("enqueue request = %#v", provider.enqueued)
	}
	invoke := tools[1].(*fleetDispatchTool)
	if invoke.ToolAuthorizationOperation() != auth.OperationInvoke {
		t.Fatalf("invoke operation = %q", invoke.ToolAuthorizationOperation())
	}
	invokeResult, err := invoke.Call(context.Background(), fleetToolArguments(now, true))
	if err != nil {
		t.Fatal(err)
	}
	assertPublicFleetReceipt(t, invokeResult)
	if string(provider.invoked.ClaimID) != "claim" ||
		provider.invoked.Lease != time.Second {
		t.Fatalf("invoke request = %#v", provider.invoked)
	}
	status := tools[2].(*fleetStatusTool)
	if status.ToolAuthorizationOperation() != auth.OperationDispatch {
		t.Fatalf("status operation = %q", status.ToolAuthorizationOperation())
	}
	statusResult, err := status.Call(context.Background(), json.RawMessage(`{"action_id":"YWN0aW9u"}`))
	if err != nil {
		t.Fatal(err)
	}
	assertPublicFleetReceipt(t, statusResult)
	if string(provider.status.ID) != "action" ||
		provider.status.Context.RequestID != decision.RequestID() ||
		provider.status.Context.CorrelationID != decision.CorrelationID() ||
		provider.status.Context.ReasonCode != "receipt_lookup" ||
		!provider.status.Context.Deadline.After(time.Now()) {
		t.Fatalf("status request = %#v", provider.status)
	}
}

func assertPublicFleetReceipt(t *testing.T, result any) {
	t.Helper()
	raw, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	var envelope struct {
		Action map[string]json.RawMessage `json:"action"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil {
		t.Fatal(err)
	}
	if string(envelope.Action["id"]) != `"YWN0aW9u"` {
		t.Fatalf("missing public action identity: %s", raw)
	}
	for _, field := range []string{
		"input", "Input", "Subject", "Actor", "ClientID",
		"IdempotencyKey", "AuthorizationFingerprint", "PolicyGeneration",
		"ClaimantSubject", "ClaimHistory",
	} {
		if _, found := envelope.Action[field]; found {
			t.Fatalf("internal %s leaked in MCP receipt: %s", field, raw)
		}
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
	provider := &fleetToolService{}
	tools, err := NewFleetDispatchTools(
		provider, fleetToolResolver{decision: decision})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tools[0].Call(
		context.Background(), fleetToolArguments(time.Now(), false),
	); !shoal.IsErrorCode(err, shoal.ErrorUnauthorized) {
		t.Fatalf("missing trusted correlation = %v", err)
	}
	if _, err := tools[2].Call(
		context.Background(), json.RawMessage(`{"action_id":"YWN0aW9u"}`),
	); !shoal.IsErrorCode(err, shoal.ErrorUnauthorized) {
		t.Fatalf("status accepted missing trusted correlation: %v", err)
	}
	if len(provider.status.ID) != 0 {
		t.Fatalf("status provider was called on an unbound request: %#v", provider.status)
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
	status   fleet.StatusRequest
}

func (s *fleetToolService) Enqueue(
	_ context.Context, request fleet.EnqueueRequest,
) (fleet.ActionRecord, error) {
	s.enqueued = request
	return fleet.ActionRecord{
		ID: request.ID, Input: request.Input, Subject: "private-subject",
		IdempotencyKey: request.IdempotencyKey,
	}, nil
}
func (s *fleetToolService) Invoke(
	_ context.Context, request fleet.InvokeRequest,
) (fleet.ActionRecord, error) {
	s.invoked = request
	return fleet.ActionRecord{
		ID: request.Enqueue.ID, Input: request.Enqueue.Input,
		Subject: "private-subject", IdempotencyKey: request.Enqueue.IdempotencyKey,
	}, nil
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
func (s *fleetToolService) Status(_ context.Context, request fleet.StatusRequest) (fleet.ActionRecord, error) {
	s.status = request
	return fleet.ActionRecord{
		ID: request.ID, Input: json.RawMessage(`{"secret":"payload"}`),
		Subject: "private-subject", IdempotencyKey: []byte("private-key"),
	}, nil
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
