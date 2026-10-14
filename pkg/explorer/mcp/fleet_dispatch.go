// Licensed to the Apache Software Foundation (ASF) under one or more
// contributor license agreements. See the NOTICE file distributed with this
// work for additional information regarding copyright ownership.
package mcp

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"time"

	"github.com/phrocker/shoal-oss/pkg/explorer/auth"
	"github.com/phrocker/shoal-oss/pkg/explorer/fleet"
	"github.com/phrocker/shoal-oss/pkg/explorer/webapi"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

const (
	FleetDispatchToolName = "shoal.agent_dispatch"
	FleetInvokeToolName   = "shoal.agent_invoke"
)

type fleetDispatchTool struct {
	service   webapi.FleetDispatchProvider
	resolver  auth.Resolver
	operation auth.Operation
	invoke    bool
}

// FleetActionToolResult is the structured MCP result. This provider
// intentionally does not implement OptionalToolObservationProvider until the
// finalized complete evidence-reference schema is composed; bare node IDs
// would understate edge, anchor, revision, range, and visibility provenance.
type FleetActionToolResult struct {
	Action fleet.ActionRecord `json:"action"`
}

// NewFleetDispatchTools returns enqueue and synchronous invoke providers.
// The MCP dispatcher enforces each provider's primary operation; fleet.Service
// performs target-bound and conditional delegation authorization again.
func NewFleetDispatchTools(
	service webapi.FleetDispatchProvider,
	resolver auth.Resolver,
) ([]OptionalToolProvider, error) {
	if service == nil || resolver == nil {
		return nil, shoal.NewError(
			shoal.ErrorInvalidArgument, "fleet MCP dependencies are required")
	}
	return []OptionalToolProvider{
		&fleetDispatchTool{
			service: service, resolver: resolver,
			operation: auth.OperationDispatch,
		},
		&fleetDispatchTool{
			service: service, resolver: resolver,
			operation: auth.OperationInvoke, invoke: true,
		},
	}, nil
}

func (p *fleetDispatchTool) Tool() Tool {
	name, title, description := FleetDispatchToolName, "Dispatch agent action",
		"Durably enqueue one governed agent action."
	if p.invoke {
		name, title = FleetInvokeToolName, "Invoke agent action"
		description = "Durably enqueue, claim, and invoke one governed agent action."
	}
	idempotent, destructive, openWorld := true, true, false
	return Tool{
		Name: name, Title: title, Description: description,
		InputSchema: json.RawMessage(`{
			"type":"object",
			"properties":{
				"action_id":{"type":"string","minLength":1,"maxLength":344},
				"idempotency_key":{"type":"string","minLength":1,"maxLength":344},
				"agent_id":{"type":"string","minLength":1,"maxLength":1366},
				"agent_generation":{"type":"integer","minimum":1},
				"capability":{"type":"string","minLength":1,"maxLength":128},
				"action":{"type":"string","minLength":1,"maxLength":128},
				"source_id":{"type":"string","minLength":1,"maxLength":344},
				"policy_id":{"type":"string","minLength":1,"maxLength":344},
				"object_id":{"type":"string","minLength":1,"maxLength":1366},
				"input":{},
				"reason_code":{"type":"string","minLength":1,"maxLength":128},
				"reason_detail":{"type":"string","maxLength":2048},
				"deadline":{"type":"string","minLength":20,"maxLength":64},
				"claim_id":{"type":"string","maxLength":344},
				"lease_milliseconds":{"type":"integer","minimum":1,"maximum":300000}
			},
			"required":["action_id","idempotency_key","agent_id","agent_generation",
				"capability","action","source_id","policy_id","object_id","input",
				"reason_code","deadline"],
			"additionalProperties":false
		}`),
		Annotations: &ToolAnnotations{
			DestructiveHint: &destructive, IdempotentHint: &idempotent,
			OpenWorldHint: &openWorld,
		},
		Execution: &ToolExecution{TaskSupport: "forbidden"},
	}
}

// ToolAuthorizationOperation is consumed by the finalized MCP dispatcher.
func (p *fleetDispatchTool) ToolAuthorizationOperation() auth.Operation {
	return p.operation
}

func (p *fleetDispatchTool) Call(
	ctx context.Context,
	arguments json.RawMessage,
) (any, error) {
	var input fleetActionToolInput
	if err := decodeFleetToolInput(arguments, &input); err != nil {
		return nil, err
	}
	decision, err := p.resolver.Resolve(ctx)
	if err != nil {
		return nil, err
	}
	if decision.RequestID() == "" || decision.CorrelationID() == "" {
		return nil, shoal.NewError(
			shoal.ErrorUnauthorized,
			"fleet MCP decision lacks request or correlation identity",
		)
	}
	request, err := input.enqueue(decision)
	if err != nil {
		return nil, err
	}
	if err := decision.AuthorizeObject(p.operation, auth.ResourceRequest{
		AuthorizationDomain: decision.AuthorizationDomain(),
		SourceID:            request.SourceID,
		PolicyID:            request.PolicyID,
		ObjectID:            request.ObjectID,
	}, time.Now().UTC()); err != nil {
		return nil, err
	}
	var action fleet.ActionRecord
	if p.invoke {
		claimID, decodeErr := decodeFleetToolBytes("claim ID", input.ClaimID, false)
		if decodeErr != nil {
			return nil, decodeErr
		}
		if input.LeaseMilliseconds <= 0 {
			return nil, shoal.NewError(
				shoal.ErrorInvalidArgument, "invoke lease is required")
		}
		action, err = p.service.Invoke(ctx, fleet.InvokeRequest{
			Enqueue: request, ClaimID: claimID,
			Lease: time.Duration(input.LeaseMilliseconds) * time.Millisecond,
		})
	} else {
		if input.ClaimID != "" || input.LeaseMilliseconds != 0 {
			return nil, shoal.NewError(
				shoal.ErrorInvalidArgument,
				"claim fields are only valid for agent invoke",
			)
		}
		action, err = p.service.Enqueue(ctx, request)
	}
	if err != nil {
		return nil, err
	}
	return FleetActionToolResult{Action: action}, nil
}

type fleetActionToolInput struct {
	ActionID          string          `json:"action_id"`
	IdempotencyKey    string          `json:"idempotency_key"`
	AgentID           string          `json:"agent_id"`
	AgentGeneration   int64           `json:"agent_generation"`
	Capability        string          `json:"capability"`
	Action            string          `json:"action"`
	SourceID          string          `json:"source_id"`
	PolicyID          string          `json:"policy_id"`
	ObjectID          string          `json:"object_id"`
	Input             json.RawMessage `json:"input"`
	ReasonCode        string          `json:"reason_code"`
	ReasonDetail      string          `json:"reason_detail,omitempty"`
	Deadline          string          `json:"deadline"`
	ClaimID           string          `json:"claim_id,omitempty"`
	LeaseMilliseconds int64           `json:"lease_milliseconds,omitempty"`
}

func (input fleetActionToolInput) enqueue(
	decision auth.Decision,
) (fleet.EnqueueRequest, error) {
	actionID, err := decodeFleetToolBytes("action ID", input.ActionID, false)
	if err != nil {
		return fleet.EnqueueRequest{}, err
	}
	key, err := decodeFleetToolBytes(
		"action idempotency key", input.IdempotencyKey, false)
	if err != nil {
		return fleet.EnqueueRequest{}, err
	}
	agentID, err := decodeFleetToolID("agent ID", input.AgentID, false)
	if err != nil {
		return fleet.EnqueueRequest{}, err
	}
	sourceID, err := decodeFleetToolBytes("source ID", input.SourceID, false)
	if err != nil {
		return fleet.EnqueueRequest{}, err
	}
	policyID, err := decodeFleetToolBytes("policy ID", input.PolicyID, false)
	if err != nil {
		return fleet.EnqueueRequest{}, err
	}
	objectID, err := decodeFleetToolID("object ID", input.ObjectID, false)
	if err != nil {
		return fleet.EnqueueRequest{}, err
	}
	deadline, err := time.Parse(time.RFC3339Nano, input.Deadline)
	if err != nil {
		return fleet.EnqueueRequest{}, shoal.WrapError(
			shoal.ErrorInvalidArgument, "action deadline is invalid", err)
	}
	return fleet.EnqueueRequest{
		ID: actionID, IdempotencyKey: key, AgentID: shoal.ID(agentID),
		AgentGeneration: input.AgentGeneration, Capability: input.Capability,
		Action: input.Action, SourceID: sourceID, PolicyID: policyID,
		ObjectID: shoal.ID(objectID), Input: append(json.RawMessage(nil), input.Input...),
		Context: fleet.RequestContext{
			RequestID: decision.RequestID(), CorrelationID: decision.CorrelationID(),
			ReasonCode: input.ReasonCode, ReasonDetail: input.ReasonDetail,
			Deadline: deadline.UTC(),
		},
	}, nil
}

func decodeFleetToolInput(raw json.RawMessage, target any) error {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return shoal.WrapError(
			shoal.ErrorInvalidArgument, "fleet MCP arguments are invalid", err)
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return shoal.NewError(
			shoal.ErrorInvalidArgument, "fleet MCP arguments contain trailing data")
	}
	return nil
}

func decodeFleetToolBytes(name, encoded string, optional bool) ([]byte, error) {
	return decodeFleetToolBytesBound(name, encoded, optional, fleet.MaxActionIDBytes)
}

func decodeFleetToolID(name, encoded string, optional bool) ([]byte, error) {
	return decodeFleetToolBytesBound(name, encoded, optional, shoal.MaxIDBytes)
}

func decodeFleetToolBytesBound(
	name, encoded string, optional bool, maxBytes int,
) ([]byte, error) {
	if encoded == "" && optional {
		return nil, nil
	}
	value, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil || len(value) == 0 || len(value) > maxBytes {
		return nil, shoal.NewError(
			shoal.ErrorInvalidArgument, name+" is invalid")
	}
	return value, nil
}

var (
	_ OptionalToolProvider = (*fleetDispatchTool)(nil)
)
