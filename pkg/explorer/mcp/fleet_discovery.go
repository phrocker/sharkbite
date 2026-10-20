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
	"time"

	"github.com/phrocker/shoal-oss/pkg/explorer/auth"
	"github.com/phrocker/shoal-oss/pkg/explorer/fleet"
	"github.com/phrocker/shoal-oss/pkg/explorer/webapi"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

const FleetDiscoverToolName = "shoal.agent_discover"

type fleetLister interface {
	List(context.Context, fleet.ListRequest) (fleet.ListPage, error)
}

type fleetDiscoveryTool struct {
	service  fleetLister
	resolver auth.Resolver
}

// NewFleetDiscoveryTool exposes the durable Fleet registry's authorized list
// through MCP. The registry, not this transport, decides descriptor visibility.
func NewFleetDiscoveryTool(service fleetLister, resolver auth.Resolver) (OptionalToolProvider, error) {
	if isAbsent(service) || isAbsent(resolver) {
		return nil, shoal.NewError(shoal.ErrorInvalidArgument, "fleet MCP discovery dependencies are required")
	}
	return &fleetDiscoveryTool{service: service, resolver: resolver}, nil
}

func (*fleetDiscoveryTool) Tool() Tool {
	readOnly, harmless, idempotent := true, false, true
	return Tool{
		Name: FleetDiscoverToolName, Title: "Discover agent actions",
		Description: "List authorized active registry declarations, including action schemas, effects, approval and attestation requirements. A listed executor may still be unavailable at dispatch time.",
		InputSchema: json.RawMessage(`{
			"type":"object",
			"properties":{
				"limit":{"type":"integer","minimum":0,"maximum":16},
				"cursor":{"type":"string","maxLength":4096}
			},
			"additionalProperties":false
		}`),
		Annotations: &ToolAnnotations{
			ReadOnlyHint: &readOnly, DestructiveHint: &harmless,
			IdempotentHint: &idempotent, OpenWorldHint: &harmless,
		},
		Execution: &ToolExecution{TaskSupport: "forbidden"},
	}
}

func (*fleetDiscoveryTool) ToolAuthorizationOperation() auth.Operation {
	return auth.OperationAgentResolve
}

type fleetDiscoveryInput struct {
	Limit  int    `json:"limit"`
	Cursor string `json:"cursor,omitempty"`
}

type FleetDiscoveryResult struct {
	Agents []webapi.FleetDescriptor `json:"agents"`
	Next   string                   `json:"next_cursor,omitempty"`
}

func (p *fleetDiscoveryTool) Call(ctx context.Context, raw json.RawMessage) (any, error) {
	var input fleetDiscoveryInput
	if err := decodeFleetToolInput(raw, &input); err != nil || input.Limit < 0 || input.Limit > fleet.MaxListResults || len(input.Cursor) > 4096 {
		return nil, invalidToolArguments(FleetDiscoverToolName)
	}
	var cursor []byte
	if input.Cursor != "" {
		var err error
		cursor, err = base64.RawURLEncoding.DecodeString(input.Cursor)
		if err != nil || len(cursor) == 0 || len(cursor) > 3072 || base64.RawURLEncoding.EncodeToString(cursor) != input.Cursor {
			return nil, invalidToolArguments(FleetDiscoverToolName)
		}
	}
	decision, err := p.resolver.Resolve(ctx)
	if err != nil {
		return nil, err
	}
	if decision.RequestID() == "" {
		return nil, shoal.NewError(shoal.ErrorUnauthorized, "fleet MCP decision lacks request identity")
	}
	page, err := p.service.List(ctx, fleet.ListRequest{
		Context: fleet.RequestContext{
			RequestID: decision.RequestID(), CorrelationID: decision.CorrelationID(),
			ReasonCode: "mcp_discovery", Deadline: time.Now().UTC().Add(30 * time.Second),
		},
		Cursor: cursor, Limit: input.Limit,
	})
	if err != nil {
		return nil, err
	}
	result := FleetDiscoveryResult{Agents: make([]webapi.FleetDescriptor, len(page.Descriptors))}
	for i, descriptor := range page.Descriptors {
		result.Agents[i] = webapi.EncodeFleetDescriptor(descriptor)
	}
	if len(page.Next) > 0 {
		result.Next = base64.RawURLEncoding.EncodeToString(page.Next)
	}
	return result, nil
}

var _ OptionalToolProvider = (*fleetDiscoveryTool)(nil)
