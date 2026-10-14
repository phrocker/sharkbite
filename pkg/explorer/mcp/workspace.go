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
	"net/http"
	"strings"

	"github.com/phrocker/shoal-oss/pkg/explorer/auth"
	"github.com/phrocker/shoal-oss/pkg/explorer/webapi"
	"github.com/phrocker/shoal-oss/pkg/explorer/workspace"
	"github.com/phrocker/shoal-oss/pkg/retrieval"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

type workspaceBinding struct {
	workspaceID     shoal.ID
	settingsID      shoal.ID
	revision        uint64
	cacheDimensions map[string]uint64
	limits          workspace.Limits
}

func effectiveWorkspaceBinding(
	ctx context.Context,
) (workspaceBinding, bool, error) {
	effective, ok := webapi.EffectiveWorkspaceSettings(ctx)
	if !ok {
		workspaceID, selected := webapi.EffectiveWorkspaceID(ctx)
		if !selected {
			return workspaceBinding{}, false, nil
		}
		if err := shoal.ValidateRequiredID(
			"effective workspace ID", workspaceID,
		); err != nil {
			return workspaceBinding{}, false, err
		}
		return workspaceBinding{workspaceID: workspaceID}, true, nil
	}
	workspaceID, ok := webapi.EffectiveWorkspaceID(ctx)
	if !ok {
		return workspaceBinding{}, false, shoal.NewError(
			shoal.ErrorUnauthorized,
			"effective workspace identity is unavailable",
		)
	}
	if err := shoal.ValidateRequiredID(
		"effective workspace ID", workspaceID,
	); err != nil {
		return workspaceBinding{}, false, err
	}
	if err := shoal.ValidateRequiredID(
		"effective workspace settings ID", effective.SettingsID(),
	); err != nil {
		return workspaceBinding{}, false, err
	}
	limits := effective.Limits()
	if limits.OutputBytes == 0 {
		return workspaceBinding{}, false, shoal.NewError(
			shoal.ErrorUnauthorized,
			"effective workspace settings disable MCP output",
		)
	}
	return workspaceBinding{
		workspaceID:     workspaceID,
		settingsID:      effective.SettingsID(),
		revision:        effective.Revision(),
		cacheDimensions: effective.CacheDimensions(),
		limits:          limits,
	}, true, nil
}

func workspaceBindingForRequest(
	request *http.Request,
	required bool,
) (workspaceBinding, bool, error) {
	if request == nil {
		return workspaceBinding{}, false, shoal.NewError(
			shoal.ErrorInvalidArgument, "HTTP request is required")
	}
	values := request.Header.Values(webapi.WorkspaceIDHeader)
	binding, ok, err := effectiveWorkspaceBinding(request.Context())
	if err != nil {
		return workspaceBinding{}, false, err
	}
	if !ok {
		if required || len(values) != 0 {
			return workspaceBinding{}, false, shoal.NewError(
				shoal.ErrorInvalidArgument,
				"one applied workspace settings header is required",
			)
		}
		return workspaceBinding{}, false, nil
	}
	if len(values) != 1 ||
		strings.TrimSpace(values[0]) != binding.headerValue() {
		return workspaceBinding{}, false, shoal.NewError(
			shoal.ErrorInvalidArgument,
			"workspace settings header does not match the effective workspace",
		)
	}
	return binding, true, nil
}

func (b workspaceBinding) equal(other workspaceBinding) bool {
	if b.workspaceID != other.workspaceID {
		return false
	}
	if b.settingsID == "" || other.settingsID == "" {
		return b.settingsID == "" && other.settingsID == ""
	}
	if b.settingsID != other.settingsID ||
		b.revision != other.revision ||
		b.limits != other.limits ||
		len(b.cacheDimensions) != len(other.cacheDimensions) {
		return false
	}
	for key, value := range b.cacheDimensions {
		if other.cacheDimensions[key] != value {
			return false
		}
	}
	return true
}

func (b workspaceBinding) headerValue() string {
	if b.workspaceID == "" {
		return ""
	}
	return base64.RawURLEncoding.EncodeToString([]byte(b.workspaceID))
}

func workspaceInitializeMeta(ctx context.Context) map[string]any {
	binding, ok, err := effectiveWorkspaceBinding(ctx)
	if err != nil || !ok {
		return nil
	}
	if binding.settingsID == "" {
		return map[string]any{
			"shoal.workspace": map[string]any{
				"id": binding.headerValue(),
			},
		}
	}
	return map[string]any{
		"shoal.workspace": map[string]any{
			"id": binding.headerValue(),
			"settings_id": base64.RawURLEncoding.EncodeToString(
				[]byte(binding.settingsID)),
			"revision":         binding.revision,
			"cache_dimensions": cloneCacheDimensions(binding.cacheDimensions),
			"limits": map[string]any{
				"retrieval_top_k": binding.limits.RetrievalTopK,
				"graph_depth":     binding.limits.GraphDepth,
				"graph_fanout":    binding.limits.GraphFanout,
				"graph_nodes":     binding.limits.GraphNodes,
				"output_bytes":    binding.limits.OutputBytes,
			},
		},
	}
}

func (s *Server) applyWorkspaceForOperation(
	ctx context.Context,
	decision auth.Decision,
	operation auth.Operation,
) (
	context.Context,
	auth.Decision,
	int,
	uint64,
	error,
) {
	contextBudget, outputBudget := s.contextBudget, s.outputBudget
	if effective, ok := webapi.EffectiveWorkspaceSettings(ctx); ok {
		contextBudget, outputBudget = lowerWorkspaceBudgets(
			contextBudget, outputBudget, effective.Limits().OutputBytes)
		return ctx, decision, contextBudget, outputBudget, nil
	}
	workspaceID, selected := webapi.EffectiveWorkspaceID(ctx)
	if !selected {
		return ctx, decision, contextBudget, outputBudget, nil
	}
	if isAbsent(s.workspaceSettings) {
		return nil, auth.Decision{}, 0, 0, shoal.NewError(
			shoal.ErrorUnavailable,
			"workspace settings application is unavailable",
		)
	}
	bound, err := webapi.ApplyWorkspaceSettingsForOperation(
		ctx, s.workspaceSettings, s.binder, workspaceID, operation,
		workspace.MaximumLimits(), nil,
	)
	if err != nil {
		return nil, auth.Decision{}, 0, 0, err
	}
	effective, ok := webapi.EffectiveWorkspaceSettings(bound)
	if !ok {
		return nil, auth.Decision{}, 0, 0, shoal.NewError(
			shoal.ErrorInternal,
			"effective workspace settings are unavailable",
		)
	}
	decision = effective.Decision()
	contextBudget, outputBudget = lowerWorkspaceBudgets(
		contextBudget, outputBudget, effective.Limits().OutputBytes)
	return withBoundHTTPDecision(bound, decision), decision,
		contextBudget, outputBudget, nil
}

func lowerWorkspaceBudgets(
	contextBudget int,
	outputBudget uint64,
	workspaceOutput uint64,
) (int, uint64) {
	if workspaceOutput < outputBudget {
		outputBudget = workspaceOutput
	}
	if uint64(contextBudget) > outputBudget {
		contextBudget = int(outputBudget)
	}
	return contextBudget, outputBudget
}

func cloneCacheDimensions(values map[string]uint64) map[string]uint64 {
	if len(values) == 0 {
		return nil
	}
	result := make(map[string]uint64, len(values))
	for key, value := range values {
		result[key] = value
	}
	return result
}

func applyWorkspaceToolLimits(
	ctx context.Context,
	name string,
	raw json.RawMessage,
) (json.RawMessage, error) {
	effective, ok := webapi.EffectiveWorkspaceSettings(ctx)
	if !ok {
		return raw, nil
	}
	limits := effective.Limits()
	switch name {
	case ToolRetrieve:
		if limits.RetrievalTopK == 0 {
			return nil, workspaceDisablesTool(name)
		}
		var request webapi.RetrievalRequest
		if err := decodeToolArguments(raw, &request, name); err != nil {
			return nil, err
		}
		request.Query.TopK = lowerWorkspaceLimit(
			request.Query.TopK, retrieval.DefaultTopK, limits.RetrievalTopK)
		return marshalWorkspaceArguments(name, request)
	case ToolNeighborhood:
		if limits.GraphDepth == 0 || limits.GraphFanout == 0 ||
			limits.GraphNodes == 0 {
			return nil, workspaceDisablesTool(name)
		}
		var request webapi.NeighborhoodRequest
		if err := decodeToolArguments(raw, &request, name); err != nil {
			return nil, err
		}
		request.Depth = lowerWorkspaceLimit(
			request.Depth, webapi.DefaultDepth, limits.GraphDepth)
		request.Fanout = lowerWorkspaceLimit(
			request.Fanout, webapi.DefaultFanout, limits.GraphFanout)
		request.MaxNodes = lowerWorkspaceLimit(
			request.MaxNodes, webapi.DefaultMaxNodes, limits.GraphNodes)
		return marshalWorkspaceArguments(name, request)
	case ToolPath:
		if limits.GraphDepth == 0 || limits.GraphFanout == 0 {
			return nil, workspaceDisablesTool(name)
		}
		var request webapi.PathRequest
		if err := decodeToolArguments(raw, &request, name); err != nil {
			return nil, err
		}
		request.MaxDepth = lowerWorkspaceLimit(
			request.MaxDepth, webapi.DefaultDepth, limits.GraphDepth)
		request.Fanout = lowerWorkspaceLimit(
			request.Fanout, webapi.DefaultFanout, limits.GraphFanout)
		return marshalWorkspaceArguments(name, request)
	default:
		return raw, nil
	}
}

func lowerWorkspaceLimit(value, fallback, maximum uint32) uint32 {
	if value == 0 {
		value = fallback
	}
	if value > maximum {
		return maximum
	}
	return value
}

func marshalWorkspaceArguments(
	toolName string, value any,
) (json.RawMessage, error) {
	encoded, err := json.Marshal(value)
	if err != nil {
		return nil, shoal.WrapError(
			shoal.ErrorInternal,
			"encode effective arguments for "+toolName,
			err,
		)
	}
	return encoded, nil
}

func workspaceDisablesTool(toolName string) error {
	return shoal.NewError(
		shoal.ErrorUnauthorized,
		"effective workspace settings disable "+toolName,
	)
}
