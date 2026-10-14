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
	"encoding/json"
	"fmt"

	"github.com/phrocker/shoal-oss/pkg/explorer"
	exploreranalytics "github.com/phrocker/shoal-oss/pkg/explorer/analytics"
	"github.com/phrocker/shoal-oss/pkg/explorer/auth"
	"github.com/phrocker/shoal-oss/pkg/explorer/webapi"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

// ToolAnalytics is the optional exact authorized-subgraph analytics tool.
const ToolAnalytics = "shoal.analytics"

type analyticsToolProvider struct {
	provider webapi.AnalyticsProvider
	limits   exploreranalytics.Limits
	tool     Tool
}

// NewAnalyticsTool constructs an optional MCP adapter only when the service
// implements analytics and advertises matching valid limits.
func NewAnalyticsTool(
	provider webapi.AnalyticsProvider,
	limits exploreranalytics.Limits,
) (OptionalToolProvider, error) {
	if isAbsent(provider) {
		return nil, shoal.NewError(
			shoal.ErrorInvalidArgument, "analytics provider is required")
	}
	availability, ok := provider.(webapi.AnalyticsLimitsProvider)
	if !ok || isAbsent(availability) {
		return nil, shoal.NewError(
			shoal.ErrorInvalidArgument, "analytics provider limits are required")
	}
	actual, available := availability.AnalyticsLimits()
	if !available || actual != limits || limits.Validate() != nil {
		return nil, shoal.NewError(
			shoal.ErrorInvalidArgument, "analytics provider limits are unavailable")
	}
	recording, ok := provider.(webapi.AnalyticsRecordingProvider)
	if !ok || isAbsent(recording) || !recording.AnalyticsRecordingRequired() {
		return nil, shoal.NewError(
			shoal.ErrorInvalidArgument,
			"analytics provider does not require durable recording",
		)
	}
	schema := json.RawMessage(fmt.Sprintf(`{
		"type":"object",
		"properties":{
			"snapshot":{
				"type":"object",
				"properties":{"id":{"type":"string","maxLength":256}},
				"additionalProperties":false
			},
			"scope":{
				"type":"object",
				"properties":{
					"node_ids":{"type":"array","items":{"type":"string","minLength":1,"maxLength":1366},"minItems":1,"maxItems":%d},
					"depth":{"type":"integer","minimum":1,"maximum":%d},
					"direction":{"type":"string","enum":["both","outgoing","incoming"]},
					"fanout":{"type":"integer","minimum":1,"maximum":%d},
					"max_nodes":{"type":"integer","minimum":1,"maximum":%d},
					"max_edges":{"type":"integer","minimum":1,"maximum":%d},
					"max_scanned_edges_per_node":{"type":"integer","minimum":1,"maximum":%d},
					"edge_types":{"type":"array","items":{"type":"string"},"maxItems":%d}
				},
				"required":["node_ids","depth","fanout","max_nodes","max_edges","max_scanned_edges_per_node"],
				"additionalProperties":false
			},
			"page_rank":{
				"type":"object",
				"properties":{
					"damping_factor":{"type":"number","minimum":0,"exclusiveMaximum":1},
					"convergence_tolerance":{"type":"number","minimum":0,"exclusiveMaximum":1},
					"max_iterations":{"type":"integer","minimum":0,"maximum":%d}
				},
				"additionalProperties":false
			}
		},
		"required":["scope"],
		"additionalProperties":false
	}`,
		limits.MaxSeeds,
		limits.MaxDepth,
		limits.MaxFanout,
		limits.MaxNodes,
		limits.MaxEdges,
		limits.MaxScannedEdgesPerNode,
		limits.MaxEdgeTypes,
		limits.MaxPageRankIterations,
	))
	tool := Tool{
		Name: ToolAnalytics, Title: "Analyze an authorized Shoal subgraph",
		Description: "Compute converged PageRank, directed degree, and weakly " +
			"connected component summaries only within a complete, explicitly " +
			"bounded, authorization-filtered, ontology-lensed subgraph. " +
			"Incomplete materialization, nonconvergence, or durable interaction " +
			"recording failure fails the call.",
		InputSchema: schema, Annotations: &ToolAnnotations{
			ReadOnlyHint: boolHint(false), DestructiveHint: boolHint(false),
			IdempotentHint: boolHint(false), OpenWorldHint: boolHint(false),
		},
		Execution: &ToolExecution{TaskSupport: "forbidden"},
	}
	if _, err := validateTool(tool); err != nil {
		return nil, err
	}
	return &analyticsToolProvider{
		provider: provider, limits: limits, tool: tool,
	}, nil
}

func (p *analyticsToolProvider) Tool() Tool {
	if p == nil {
		return Tool{}
	}
	return cloneTool(p.tool)
}

func (*analyticsToolProvider) ToolAuthorizationOperation() auth.Operation {
	return auth.OperationAnalyticsRead
}

func (p *analyticsToolProvider) Call(
	ctx context.Context,
	raw json.RawMessage,
) (any, error) {
	if p == nil || isAbsent(p.provider) {
		return nil, shoal.NewError(
			shoal.ErrorUnavailable, "analytics provider is unavailable")
	}
	var request webapi.AnalyticsRequest
	if err := decodeToolArguments(raw, &request, ToolAnalytics); err != nil {
		return nil, err
	}
	if err := exploreranalytics.ValidateRequest(
		exploreranalytics.Request{
			SnapshotID: request.Snapshot.ID,
			Scope:      request.Scope, PageRank: request.PageRank,
		},
		p.limits,
	); err != nil {
		return nil, invalidToolArguments(ToolAnalytics)
	}
	response, err := p.provider.Analytics(ctx, request)
	if err != nil {
		return nil, err
	}
	if err := webapi.ValidateAnalyticsResponse(
		request, response, p.limits); err != nil {
		return nil, explorer.MarkIndeterminateCommit(err)
	}
	return response, nil
}

var _ OptionalToolProvider = (*analyticsToolProvider)(nil)
